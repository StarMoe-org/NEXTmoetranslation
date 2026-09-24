package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"moesekai/server/internal/model"
)

type sideStoryStoryRow struct {
	title, areaCategory              string
	characterID, areaID, actionSetID int
	releasedAt                       int64
}

type sideStoryEpisodeRow struct {
	scenarioID, titleJP     string
	position                int
	jpPath, cnPath, enPath  string
	scriptSHA256, lastError string
	cnState, enState        string
	jpRefetch               bool
}

// SyncSideStoryCatalogContext upserts the masterdata catalog of one kind in a
// single transaction. Stories and episodes missing from stories are kept.
func (s *Store) SyncSideStoryCatalogContext(ctx context.Context, kind string, stories []SideStoryCatalogStory, now time.Time) (SideStoryCatalogResult, error) {
	if err := validateSideStoryCatalog(kind, stories); err != nil {
		return SideStoryCatalogResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SideStoryCatalogResult{}, err
	}
	defer tx.Rollback()
	existingStories, err := loadSideStoryStoryRows(ctx, tx, kind)
	if err != nil {
		return SideStoryCatalogResult{}, err
	}
	existingEpisodes, err := loadSideStoryEpisodeRows(ctx, tx, kind)
	if err != nil {
		return SideStoryCatalogResult{}, err
	}
	titleKeys, err := loadSideStoryTitleKeys(ctx, tx, kind)
	if err != nil {
		return SideStoryCatalogResult{}, err
	}
	stamp := now.Unix()
	result := SideStoryCatalogResult{Stories: len(stories)}
	for _, story := range stories {
		if err := ctx.Err(); err != nil {
			return SideStoryCatalogResult{}, err
		}
		wanted := sideStoryStoryRow{
			title: story.Title, areaCategory: story.AreaCategory, characterID: story.CharacterID,
			areaID: story.AreaID, actionSetID: story.ActionSetID, releasedAt: story.ReleasedAt,
		}
		current, exists := existingStories[story.StoryID]
		switch {
		case !exists:
			if _, err := tx.ExecContext(ctx, `INSERT INTO side_stories
				(kind,story_id,title,character_id,area_id,area_category,action_set_id,released_at,updated_at)
				VALUES (?,?,?,?,?,?,?,?,?)`, kind, story.StoryID, wanted.title, wanted.characterID, wanted.areaID,
				wanted.areaCategory, wanted.actionSetID, wanted.releasedAt, stamp); err != nil {
				return SideStoryCatalogResult{}, err
			}
			result.NewStories++
		case current != wanted:
			if _, err := tx.ExecContext(ctx, `UPDATE side_stories SET title=?,character_id=?,area_id=?,area_category=?,
				action_set_id=?,released_at=?,updated_at=? WHERE kind=? AND story_id=?`, wanted.title, wanted.characterID,
				wanted.areaID, wanted.areaCategory, wanted.actionSetID, wanted.releasedAt, stamp, kind, story.StoryID); err != nil {
				return SideStoryCatalogResult{}, err
			}
		}
		for _, episode := range story.Episodes {
			key := story.StoryID + "\x00" + episode.Key
			created, requeued, err := syncSideStoryCatalogEpisodeTx(ctx, tx, kind, story.StoryID, episode, existingEpisodes, key, stamp)
			if err != nil {
				return SideStoryCatalogResult{}, err
			}
			if created {
				result.NewEpisodes++
			}
			result.OfficialRequeued += requeued
			if current := titleKeys[key]; current != "" && current != strings.TrimSpace(episode.TitleJP) {
				result.TitlesReplaced++
			}
			dropped, written, err := syncSideStoryTitleLineTx(ctx, tx, kind, story.StoryID, episode, titleKeys[key], stamp)
			if err != nil {
				return SideStoryCatalogResult{}, err
			}
			result.DroppedHumanTitles += dropped
			result.OfficialTitlesWritten += written
		}
	}
	if err := tx.Commit(); err != nil {
		return SideStoryCatalogResult{}, err
	}
	return result, nil
}

func validateSideStoryCatalog(kind string, stories []SideStoryCatalogStory) error {
	if !ValidSideStoryKind(kind) {
		return sideStoryInvalid("kind %q", kind)
	}
	seen := make(map[string]bool, len(stories))
	for _, story := range stories {
		if story.Kind != kind || !ValidSideStoryID(kind, story.StoryID) || seen[story.StoryID] {
			return sideStoryInvalid("catalog story %s/%q is invalid or repeated", story.Kind, story.StoryID)
		}
		seen[story.StoryID] = true
		if story.CharacterID < 0 || story.AreaID < 0 || story.ReleasedAt < 0 ||
			(kind == SideStoryKindCard) != (story.ActionSetID == 0) || story.ActionSetID < 0 ||
			!validSideStoryText(story.Title) || !validSideStoryText(story.AreaCategory) || len(story.Episodes) == 0 {
			return sideStoryInvalid("catalog story %s/%s has invalid fields", kind, story.StoryID)
		}
		keys := map[string]bool{}
		for _, episode := range story.Episodes {
			if !ValidSideStoryEpisodeKey(kind, episode.Key) || keys[episode.Key] ||
				!sideStoryAreaIDPattern.MatchString(episode.ScenarioID) ||
				(kind == SideStoryKindArea && episode.ScenarioID != story.StoryID) ||
				episode.Position < 0 || episode.JPAssetPath == "" {
				return sideStoryInvalid("catalog episode %s/%s/%q is invalid or repeated", kind, story.StoryID, episode.Key)
			}
			keys[episode.Key] = true
			for _, text := range []string{episode.TitleJP, episode.JPAssetPath, episode.CNAssetPath, episode.ENAssetPath, episode.CNTitle, episode.ENTitle} {
				if !validSideStoryText(text) {
					return sideStoryInvalid("catalog episode %s/%s/%s has invalid text", kind, story.StoryID, episode.Key)
				}
			}
		}
	}
	return nil
}

func loadSideStoryStoryRows(ctx context.Context, tx *sql.Tx, kind string) (map[string]sideStoryStoryRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT story_id,title,character_id,area_id,area_category,action_set_id,released_at
		FROM side_stories WHERE kind=?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]sideStoryStoryRow{}
	for rows.Next() {
		var id string
		var row sideStoryStoryRow
		if err := rows.Scan(&id, &row.title, &row.characterID, &row.areaID, &row.areaCategory, &row.actionSetID, &row.releasedAt); err != nil {
			return nil, err
		}
		out[id] = row
	}
	return out, rows.Err()
}

func loadSideStoryEpisodeRows(ctx context.Context, tx *sql.Tx, kind string) (map[string]sideStoryEpisodeRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT story_id,episode_key,scenario_id,title_jp,position,jp_asset_path,cn_asset_path,
		en_asset_path,script_sha256,last_error,cn_state,en_state,jp_refetch FROM side_story_episodes WHERE kind=?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]sideStoryEpisodeRow{}
	for rows.Next() {
		var id, key string
		var row sideStoryEpisodeRow
		if err := rows.Scan(&id, &key, &row.scenarioID, &row.titleJP, &row.position, &row.jpPath, &row.cnPath, &row.enPath,
			&row.scriptSHA256, &row.lastError, &row.cnState, &row.enState, &row.jpRefetch); err != nil {
			return nil, err
		}
		out[id+"\x00"+key] = row
	}
	return out, rows.Err()
}

func loadSideStoryTitleKeys(ctx context.Context, tx *sql.Tx, kind string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT story_id,episode_key,jp_key FROM side_story_lines WHERE kind=? AND role='title'`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, key, jp string
		if err := rows.Scan(&id, &key, &jp); err != nil {
			return nil, err
		}
		out[id+"\x00"+key] = jp
	}
	return out, rows.Err()
}

// sideStoryOfficialState is the state of a locale whose asset path is path.
func sideStoryOfficialState(path string) string {
	if path == "" {
		return SideStoryStateAbsent
	}
	return SideStoryStatePending
}

func syncSideStoryCatalogEpisodeTx(ctx context.Context, tx *sql.Tx, kind, storyID string, episode SideStoryCatalogEpisode,
	existing map[string]sideStoryEpisodeRow, key string, stamp int64) (bool, int, error) {
	current, exists := existing[key]
	if !exists {
		_, err := tx.ExecContext(ctx, `INSERT INTO side_story_episodes
			(kind,story_id,episode_key,scenario_id,title_jp,position,jp_asset_path,cn_asset_path,en_asset_path,cn_state,en_state,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, kind, storyID, episode.Key, episode.ScenarioID, episode.TitleJP, episode.Position,
			episode.JPAssetPath, episode.CNAssetPath, episode.ENAssetPath, sideStoryOfficialState(episode.CNAssetPath),
			sideStoryOfficialState(episode.ENAssetPath), stamp)
		return err == nil, 0, err
	}
	requeued := 0
	cnState, enState := current.cnState, current.enState
	if episode.CNAssetPath != current.cnPath {
		cnState = sideStoryOfficialState(episode.CNAssetPath)
		if cnState == SideStoryStatePending {
			requeued++
		}
	}
	if episode.ENAssetPath != current.enPath {
		enState = sideStoryOfficialState(episode.ENAssetPath)
		if enState == SideStoryStatePending {
			requeued++
		}
	}
	jpChanged := episode.JPAssetPath != current.jpPath || episode.ScenarioID != current.scenarioID
	if !jpChanged && cnState == current.cnState && enState == current.enState && episode.CNAssetPath == current.cnPath &&
		episode.ENAssetPath == current.enPath && episode.TitleJP == current.titleJP && episode.Position == current.position {
		return false, 0, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE side_story_episodes SET scenario_id=?,title_jp=?,position=?,jp_asset_path=?,
		cn_asset_path=?,en_asset_path=?,cn_state=?,en_state=?,updated_at=? WHERE kind=? AND story_id=? AND episode_key=?`,
		episode.ScenarioID, episode.TitleJP, episode.Position, episode.JPAssetPath, episode.CNAssetPath, episode.ENAssetPath,
		cnState, enState, stamp, kind, storyID, episode.Key); err != nil {
		return false, 0, err
	}
	synced := sideStoryApplyEpisode{scriptSHA256: current.scriptSHA256, cnPath: episode.CNAssetPath, enPath: episode.ENAssetPath,
		cnState: cnState, enState: enState, jpRefetch: current.jpRefetch, lastError: current.lastError}
	// A locale left in mismatch or error is not fetched again, so it keeps
	// its part of last_error.
	var kept []string
	for _, part := range []string{synced.keptError(model.LocaleChinese, cnState), synced.keptError(model.LocaleEnglish, enState)} {
		if part != "" {
			kept = append(kept, part)
		}
	}
	// Without a JP change or a requeue, only a vanished path changes a state.
	vanished := cnState != current.cnState || enState != current.enState
	switch {
	case jpChanged:
		// A new JP path is fetched on the next round, not after the old path's backoff.
		if _, err := tx.ExecContext(ctx, `UPDATE side_story_episodes SET jp_refetch=CASE WHEN script_sha256='' THEN 0 ELSE 1 END,
			attempts=0,next_attempt_at=0,last_error=? WHERE kind=? AND story_id=? AND episode_key=?`,
			strings.Join(kept, "; "), kind, storyID, episode.Key); err != nil {
			return false, 0, err
		}
	case requeued > 0 || (vanished && !synced.queued()):
		// So is a new CN/EN path, even while the old one waits out a 404 retry.
		// A vanished path that leaves nothing to fetch ends the retry, as an
		// apply without a failure does.
		if _, err := tx.ExecContext(ctx, `UPDATE side_story_episodes SET attempts=0,next_attempt_at=0,last_error=?
			WHERE kind=? AND story_id=? AND episode_key=?`, strings.Join(kept, "; "), kind, storyID, episode.Key); err != nil {
			return false, 0, err
		}
	case vanished:
		// The absent locale loses its part; the rest keeps the queued retry.
		var rest []string
		for _, locale := range []struct{ name, state string }{
			{model.LocaleJapanese, ""}, {model.LocaleChinese, cnState}, {model.LocaleEnglish, enState},
		} {
			if part := sideStoryErrorPart(current.lastError, locale.name); part != "" && locale.state != SideStoryStateAbsent {
				rest = append(rest, part)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE side_story_episodes SET last_error=? WHERE kind=? AND story_id=? AND episode_key=?`,
			strings.Join(rest, "; "), kind, storyID, episode.Key); err != nil {
			return false, 0, err
		}
	}
	return false, requeued, nil
}

// syncSideStoryTitleLineTx makes the trimmed JP title the episode's only
// title line and writes the official CN/EN titles under the official-write
// rule. A talk or speaker line with the same key becomes the title line. It
// returns the human rows deleted with a replaced title and the official rows
// written.
func syncSideStoryTitleLineTx(ctx context.Context, tx *sql.Tx, kind, storyID string, episode SideStoryCatalogEpisode, currentKey string, stamp int64) (int, int, error) {
	title := strings.TrimSpace(episode.TitleJP)
	dropped, written := 0, 0
	if currentKey != "" && currentKey != title {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM side_story_line_localizations
			WHERE kind=? AND story_id=? AND episode_key=? AND jp_key=? AND source='human'`,
			kind, storyID, episode.Key, currentKey).Scan(&dropped); err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM side_story_lines WHERE kind=? AND story_id=? AND episode_key=? AND jp_key=?`,
			kind, storyID, episode.Key, currentKey); err != nil {
			return 0, 0, err
		}
	}
	if title == "" {
		return dropped, 0, nil
	}
	if currentKey != title {
		if _, err := tx.ExecContext(ctx, `INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,speaker,position)
			VALUES (?,?,?,?,'title','',-1)
			ON CONFLICT(kind,story_id,episode_key,jp_key) DO UPDATE SET role='title',speaker='',position=-1`,
			kind, storyID, episode.Key, title); err != nil {
			return 0, 0, err
		}
	}
	for locale, official := range map[string]string{"zh-CN": episode.CNTitle, "en-US": episode.ENTitle} {
		text := strings.TrimSpace(official)
		if text == "" || (text == title && sideStoryHasKana(title)) {
			continue
		}
		wrote, err := writeSideStoryOfficialTx(ctx, tx, kind, storyID, episode.Key, title, locale, text, stamp)
		if err != nil {
			return 0, 0, err
		}
		if wrote {
			written++
		}
	}
	return dropped, written, nil
}

// writeSideStoryOfficialTx applies the official-write rule: insert when there
// is no row, replace an official row whose text differs and any llm row (an
// identical one becomes official), never touch a human row. It reports whether
// a row was written.
func writeSideStoryOfficialTx(ctx context.Context, tx *sql.Tx, kind, storyID, episodeKey, jpKey, locale, text string, stamp int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO side_story_line_localizations
		(kind,story_id,episode_key,jp_key,locale,text,source,revision,updated_by,updated_at)
		VALUES (?,?,?,?,?,?,'official',1,'sync',?)
		ON CONFLICT(kind,story_id,episode_key,jp_key,locale) DO UPDATE SET text=excluded.text,source='official',
			revision=revision+1,updated_by='sync',updated_at=excluded.updated_at
		WHERE source='llm' OR (source='official' AND text<>excluded.text)`,
		kind, storyID, episodeKey, jpKey, locale, text, stamp)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// SideStoryWorkQueueContext lists episodes due at now that need a JP fetch or
// have a pending CN/EN import; importable ones first, then newer stories.
func (s *Store) SideStoryWorkQueueContext(ctx context.Context, limit int, now time.Time) ([]SideStoryWorkItem, error) {
	if limit < 1 {
		return nil, sideStoryInvalid("queue limit %d", limit)
	}
	if limit > sideStoryMaxQueueLimit {
		limit = sideStoryMaxQueueLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sideStoryWorkItemColumns+`
		FROM side_story_episodes e JOIN side_stories s ON s.kind=e.kind AND s.story_id=e.story_id
		WHERE e.next_attempt_at<=? AND (e.script_sha256='' OR e.jp_refetch=1 OR
			(e.cn_asset_path<>'' AND e.cn_state='pending') OR (e.en_asset_path<>'' AND e.en_state='pending'))
		ORDER BY ((e.cn_asset_path<>'' AND e.cn_state='pending') OR (e.en_asset_path<>'' AND e.en_state='pending')) DESC,
			s.released_at DESC, e.kind, s.action_set_id DESC, length(e.story_id) DESC, e.story_id DESC, e.position, e.episode_key
		LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	return scanSideStoryWorkItems(rows)
}

const sideStoryWorkItemColumns = `e.kind,e.story_id,e.episode_key,e.scenario_id,e.jp_asset_path,e.cn_asset_path,e.en_asset_path,
	e.script_sha256,e.cn_state,e.en_state,e.attempts`

func scanSideStoryWorkItems(rows *sql.Rows) ([]SideStoryWorkItem, error) {
	defer rows.Close()
	items := []SideStoryWorkItem{}
	for rows.Next() {
		var item SideStoryWorkItem
		if err := rows.Scan(&item.Kind, &item.StoryID, &item.EpisodeKey, &item.ScenarioID, &item.JPAssetPath, &item.CNAssetPath,
			&item.ENAssetPath, &item.ScriptSHA256, &item.CNState, &item.ENState, &item.Attempts); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SideStoryEpisodeContext returns one episode; sql.ErrNoRows when unknown.
func (s *Store) SideStoryEpisodeContext(ctx context.Context, kind, storyID, episodeKey string) (SideStoryWorkItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sideStoryWorkItemColumns+` FROM side_story_episodes e
		WHERE e.kind=? AND e.story_id=? AND e.episode_key=?`, kind, storyID, episodeKey)
	if err != nil {
		return SideStoryWorkItem{}, err
	}
	items, err := scanSideStoryWorkItems(rows)
	if err != nil {
		return SideStoryWorkItem{}, err
	}
	if len(items) == 0 {
		return SideStoryWorkItem{}, sql.ErrNoRows
	}
	return items[0], nil
}

// SideStoryEpisodesContext returns a story's episodes by position;
// sql.ErrNoRows for an unknown story.
func (s *Store) SideStoryEpisodesContext(ctx context.Context, kind, storyID string) ([]SideStoryWorkItem, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM side_stories WHERE kind=? AND story_id=?)`, kind, storyID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+sideStoryWorkItemColumns+` FROM side_story_episodes e
		WHERE e.kind=? AND e.story_id=? ORDER BY e.position, e.episode_key`, kind, storyID)
	if err != nil {
		return nil, err
	}
	return scanSideStoryWorkItems(rows)
}
