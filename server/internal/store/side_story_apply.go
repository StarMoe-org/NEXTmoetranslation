package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"moesekai/server/internal/model"
)

const (
	sideStoryBackoffBase = 10 * time.Minute
	sideStoryBackoffCap  = 24 * time.Hour
)

// ParseSideStoryScript canonicalises with CanonicalizeEventScenario (same
// SHA-256 as the event snapshot path) and walks TalkData.
func ParseSideStoryScript(value any, expectedScenarioID string) (SideStoryScript, error) {
	canonical, digest, err := CanonicalizeEventScenario(value, expectedScenarioID)
	if err != nil {
		return SideStoryScript{}, err
	}
	talkData, _ := value.(map[string]any)["TalkData"].([]any)
	talks := make([]SideStoryTalk, len(talkData))
	for index, raw := range talkData {
		entry, ok := raw.(map[string]any)
		if !ok {
			return SideStoryScript{}, fmt.Errorf("%w: TalkData entries must be objects", ErrEventScenarioInvalid)
		}
		body, _ := entry["Body"].(string)
		speaker, _ := entry["WindowDisplayName"].(string)
		talks[index] = SideStoryTalk{Index: index, Body: strings.TrimSpace(body), Speaker: strings.TrimSpace(speaker)}
	}
	return SideStoryScript{ScenarioID: expectedScenarioID, CanonicalJSON: canonical, SHA256: digest, Talks: talks}, nil
}

type sideStoryLineDef struct {
	key, role, speaker string
	position           int
}

// sideStoryScriptLines returns the script's lines deduplicated by Japanese
// key, first occurrence first: a body at TalkData index × 2, a speaker at
// index × 2 + 1. titleKey is left to the title line.
func sideStoryScriptLines(script *SideStoryScript, titleKey string) []sideStoryLineDef {
	seen := map[string]bool{}
	if titleKey != "" {
		seen[titleKey] = true
	}
	var lines []sideStoryLineDef
	for _, talk := range script.Talks {
		if talk.Body != "" && !seen[talk.Body] {
			seen[talk.Body] = true
			lines = append(lines, sideStoryLineDef{key: talk.Body, role: SideStoryRoleTalk, speaker: talk.Speaker, position: talk.Index * 2})
		}
		if talk.Speaker != "" && !seen[talk.Speaker] {
			seen[talk.Speaker] = true
			lines = append(lines, sideStoryLineDef{key: talk.Speaker, role: SideStoryRoleSpeaker, position: talk.Index*2 + 1})
		}
	}
	return lines
}

func sideStoryBackoff(attempts int) time.Duration {
	delay := sideStoryBackoffBase
	for step := 1; step < attempts && delay < sideStoryBackoffCap; step++ {
		delay *= 2
	}
	return min(delay, sideStoryBackoffCap)
}

type sideStoryApplyEpisode struct {
	scriptSHA256     string
	cnPath, enPath   string
	cnState, enState string
	attempts         int
}

// ApplySideStoryFetchesContext records fetched scripts in one transaction.
func (s *Store) ApplySideStoryFetchesContext(ctx context.Context, fetches []SideStoryEpisodeFetch, now time.Time) (SideStoryApplyResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SideStoryApplyResult{}, err
	}
	defer tx.Rollback()
	result := SideStoryApplyResult{Episodes: make([]SideStoryEpisodeApply, 0, len(fetches))}
	for _, fetch := range fetches {
		if err := ctx.Err(); err != nil {
			return SideStoryApplyResult{}, err
		}
		applied, changed, err := applySideStoryFetchTx(ctx, tx, fetch, now.Unix())
		if err != nil {
			return SideStoryApplyResult{}, err
		}
		result.Episodes = append(result.Episodes, applied)
		result.Changed = result.Changed || changed
	}
	if err := tx.Commit(); err != nil {
		return SideStoryApplyResult{}, err
	}
	return result, nil
}

func applySideStoryFetchTx(ctx context.Context, tx *sql.Tx, fetch SideStoryEpisodeFetch, stamp int64) (SideStoryEpisodeApply, bool, error) {
	out := SideStoryEpisodeApply{Kind: fetch.Kind, StoryID: fetch.StoryID, EpisodeKey: fetch.EpisodeKey}
	var episode sideStoryApplyEpisode
	err := tx.QueryRowContext(ctx, `SELECT script_sha256,cn_asset_path,en_asset_path,cn_state,en_state,attempts
		FROM side_story_episodes WHERE kind=? AND story_id=? AND episode_key=?`, fetch.Kind, fetch.StoryID, fetch.EpisodeKey).
		Scan(&episode.scriptSHA256, &episode.cnPath, &episode.enPath, &episode.cnState, &episode.enState, &episode.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		out.Error = "episode not found"
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	out.CNState, out.ENState = episode.cnState, episode.enState
	if !fetch.JP.Attempted {
		return out, false, nil
	}
	jp := fetch.JP
	// The asset path identifies the script. Its ScenarioId field is not
	// compared: real scripts carry labels such as `016048_rui01 のコピー`.
	if jp.Script == nil {
		attempts := episode.attempts + 1
		delay, message := sideStoryBackoffCap, "no fetch result"
		switch {
		case jp.Missing:
			message = "not found"
		case jp.Err != "":
			message = jp.Err
			if jp.Transient {
				delay = sideStoryBackoff(attempts)
			}
		}
		out.Error = model.LocaleJapanese + ": " + message
		_, err := tx.ExecContext(ctx, `UPDATE side_story_episodes SET attempts=?,next_attempt_at=?,last_error=?,updated_at=?
			WHERE kind=? AND story_id=? AND episode_key=?`, attempts, stamp+int64(delay/time.Second), out.Error, stamp,
			fetch.Kind, fetch.StoryID, fetch.EpisodeKey)
		return out, false, err
	}

	out.Fetched = true
	out.ScriptChanged = episode.scriptSHA256 != jp.Script.SHA256
	lines, linesChanged, dropped, err := replaceSideStoryLinesTx(ctx, tx, fetch, jp.Script)
	if err != nil {
		return out, false, err
	}
	out.DroppedHumanLines = dropped
	requeue := out.ScriptChanged && episode.scriptSHA256 != ""
	var messages []string
	retry := sideStoryNoRetry
	for _, target := range []struct {
		locale  string
		outcome SideStoryFetchOutcome
		path    string
		state   *string
	}{
		{model.LocaleChinese, fetch.CN, episode.cnPath, &out.CNState},
		{model.LocaleEnglish, fetch.EN, episode.enPath, &out.ENState},
	} {
		official := sideStoryOfficialImport{
			fetch: fetch, locale: target.locale, jp: jp.Script, official: target.outcome, lines: lines, stamp: stamp,
		}
		state, written, message, localeRetry, err := official.applyTx(ctx, tx, *target.state, target.path, requeue)
		if err != nil {
			return out, false, err
		}
		*target.state = state
		out.OfficialWritten += written
		if message != "" {
			messages = append(messages, target.locale+": "+message)
		}
		retry = max(retry, localeRetry)
	}
	attempts, next := 0, int64(0)
	switch retry {
	case sideStoryRetryTransient:
		attempts = episode.attempts + 1
		next = stamp + int64(sideStoryBackoff(attempts)/time.Second)
	case sideStoryRetryMissing:
		next = stamp + int64(sideStoryBackoffCap/time.Second)
	}
	out.Error = strings.Join(messages, "; ")
	if _, err := tx.ExecContext(ctx, `UPDATE side_story_episodes SET script_sha256=?,jp_fetched_at=?,jp_refetch=0,attempts=?,
		next_attempt_at=?,last_error=?,cn_state=?,en_state=?,updated_at=? WHERE kind=? AND story_id=? AND episode_key=?`,
		jp.Script.SHA256, stamp, attempts, next, out.Error, out.CNState, out.ENState, stamp,
		fetch.Kind, fetch.StoryID, fetch.EpisodeKey); err != nil {
		return out, false, err
	}
	return out, linesChanged || out.OfficialWritten > 0, nil
}

// replaceSideStoryLinesTx sets the episode's talk and speaker lines to the
// script's line set. Surviving keys keep their translations; vanished keys
// are deleted with theirs and their human rows are counted. The title line
// stays.
func replaceSideStoryLinesTx(ctx context.Context, tx *sql.Tx, fetch SideStoryEpisodeFetch, script *SideStoryScript) ([]sideStoryLineDef, bool, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT jp_key,role,speaker,position FROM side_story_lines
		WHERE kind=? AND story_id=? AND episode_key=?`, fetch.Kind, fetch.StoryID, fetch.EpisodeKey)
	if err != nil {
		return nil, false, 0, err
	}
	existing := map[string]sideStoryLineDef{}
	titleKey := ""
	for rows.Next() {
		var line sideStoryLineDef
		if err := rows.Scan(&line.key, &line.role, &line.speaker, &line.position); err != nil {
			rows.Close()
			return nil, false, 0, err
		}
		existing[line.key] = line
		if line.role == SideStoryRoleTitle {
			titleKey = line.key
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, 0, err
	}
	lines := sideStoryScriptLines(script, titleKey)
	wanted := make(map[string]bool, len(lines))
	for _, line := range lines {
		wanted[line.key] = true
	}
	changed, dropped := false, 0
	for key, line := range existing {
		if line.role == SideStoryRoleTitle || wanted[key] {
			continue
		}
		var humans int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM side_story_line_localizations
			WHERE kind=? AND story_id=? AND episode_key=? AND jp_key=? AND source='human'`,
			fetch.Kind, fetch.StoryID, fetch.EpisodeKey, key).Scan(&humans); err != nil {
			return nil, false, 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM side_story_lines WHERE kind=? AND story_id=? AND episode_key=? AND jp_key=?`,
			fetch.Kind, fetch.StoryID, fetch.EpisodeKey, key); err != nil {
			return nil, false, 0, err
		}
		dropped += humans
		changed = true
	}
	for _, line := range lines {
		current, exists := existing[line.key]
		if exists && current == line {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,speaker,position)
			VALUES (?,?,?,?,?,?,?) ON CONFLICT(kind,story_id,episode_key,jp_key) DO UPDATE SET
			role=excluded.role,speaker=excluded.speaker,position=excluded.position`,
			fetch.Kind, fetch.StoryID, fetch.EpisodeKey, line.key, line.role, line.speaker, line.position); err != nil {
			return nil, false, 0, err
		}
		changed = true
	}
	return lines, changed, dropped, nil
}

type sideStoryOfficialImport struct {
	fetch    SideStoryEpisodeFetch
	locale   string
	jp       *SideStoryScript
	official SideStoryFetchOutcome
	lines    []sideStoryLineDef
	stamp    int64
}

// sideStoryRetry is when a locale's failed import is retried; the episode
// takes the highest. A transient failure outranks the others because its
// backoff never exceeds sideStoryBackoffCap, and a requeued locale outranks a
// 404 in the other so it is fetched next round.
type sideStoryRetry int

const (
	sideStoryNoRetry        sideStoryRetry = iota
	sideStoryRetryMissing                  // after sideStoryBackoffCap, without counting an attempt
	sideStoryRetryNow                      // next round: a JP change requeued an imported locale
	sideStoryRetryTransient                // after the episode backoff
)

// applyTx returns the locale's new state, the rows written, a failure
// message and when to retry it. A locale not fetched now keeps its state
// unless the JP script changed under an earlier import.
func (o sideStoryOfficialImport) applyTx(ctx context.Context, tx *sql.Tx, state, path string, requeue bool) (string, int, string, sideStoryRetry, error) {
	outcome := o.official
	switch {
	case !outcome.Attempted:
		if requeue && path != "" && state != SideStoryStateAbsent {
			return SideStoryStatePending, 0, "", sideStoryRetryNow, nil
		}
		return state, 0, "", sideStoryNoRetry, nil
	case outcome.Missing:
		// A listed script the mirror has not synced yet 404s; absent is kept
		// for a locale without an asset path.
		return SideStoryStatePending, 0, "not found", sideStoryRetryMissing, nil
	case outcome.Err != "":
		if outcome.Transient {
			return SideStoryStatePending, 0, outcome.Err, sideStoryRetryTransient, nil
		}
		return SideStoryStateError, 0, outcome.Err, sideStoryNoRetry, nil
	case outcome.Script == nil:
		return SideStoryStateError, 0, "no fetch result", sideStoryNoRetry, nil
	case len(outcome.Script.Talks) != len(o.jp.Talks):
		return SideStoryStateMismatch, 0, fmt.Sprintf("TalkData length mismatch (%d != %d)", len(o.jp.Talks), len(outcome.Script.Talks)), sideStoryNoRetry, nil
	}
	official := outcome.Script.Talks
	kanaBodies, mirrored := 0, 0
	for index, talk := range o.jp.Talks {
		if sideStoryHasKana(talk.Body) {
			kanaBodies++
			if official[index].Body == talk.Body {
				mirrored++
			}
		}
	}
	// A mirror still serving the Japanese placeholder of a newly listed script
	// gets the real one later, so this is retried like a 404.
	if mirrored*2 > kanaBodies {
		return SideStoryStatePending, 0, "official script repeats the Japanese text", sideStoryRetryMissing, nil
	}
	written := 0
	for _, line := range o.lines {
		paired := official[line.position/2]
		text := paired.Body
		if line.role == SideStoryRoleSpeaker {
			text = paired.Speaker
		}
		if text == "" || (text == line.key && sideStoryHasKana(line.key)) {
			continue
		}
		wrote, err := writeSideStoryOfficialTx(ctx, tx, o.fetch.Kind, o.fetch.StoryID, o.fetch.EpisodeKey, line.key, o.locale, text, o.stamp)
		if err != nil {
			return "", 0, "", sideStoryNoRetry, err
		}
		if wrote {
			written++
		}
	}
	return SideStoryStateImported, written, "", sideStoryNoRetry, nil
}
