package store

import (
	"context"
	"database/sql"
	"sort"
)

// sideStoryPrimarySource returns the most frequent source; ties prefer human,
// then official, then llm.
func sideStoryPrimarySource(counts SideStorySourceCounts) string {
	best, bestCount := "", 0
	for _, candidate := range []struct {
		source string
		count  int
	}{{SideStorySourceHuman, counts.Human}, {SideStorySourceOfficial, counts.Official}, {SideStorySourceLLM, counts.LLM}} {
		if candidate.count > bestCount {
			best, bestCount = candidate.source, candidate.count
		}
	}
	return best
}

func (c *SideStorySourceCounts) add(source string, count int) {
	switch source {
	case SideStorySourceOfficial:
		c.Official += count
	case SideStorySourceLLM:
		c.LLM += count
	case SideStorySourceHuman:
		c.Human += count
	}
}

// sideStoryStatus judges by translated body and speaker lines, so a story
// with only official episode titles stays untranslated.
func sideStoryStatus(fetchedEpisodes, translatedDialogue, untranslated int) string {
	switch {
	case fetchedEpisodes == 0:
		return "pending"
	case translatedDialogue == 0:
		return "untranslated"
	case untranslated > 0:
		return "partial"
	}
	return "translated"
}

// ListSideStoriesContext summarises every story of kind in locale, newest
// first, with one grouped query per table.
func (s *Store) ListSideStoriesContext(ctx context.Context, kind, locale string) ([]SideStorySummary, error) {
	if !ValidSideStoryKind(kind) {
		return nil, sideStoryInvalid("kind %q", kind)
	}
	if !ValidSideStoryLocale(locale) {
		return nil, sideStoryInvalid("locale %q", locale)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT story_id,title,character_id,area_id,area_category,action_set_id,released_at,updated_at
		FROM side_stories WHERE kind=?`, kind)
	if err != nil {
		return nil, err
	}
	summaries := []SideStorySummary{}
	for rows.Next() {
		summary := SideStorySummary{Kind: kind}
		if err := rows.Scan(&summary.ID, &summary.Title, &summary.CharacterID, &summary.AreaID, &summary.AreaCategory,
			&summary.ActionSetID, &summary.ReleasedAt, &summary.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	index := make(map[string]*SideStorySummary, len(summaries))
	for position := range summaries {
		index[summaries[position].ID] = &summaries[position]
	}

	rows, err = tx.QueryContext(ctx, `SELECT story_id,COUNT(*),SUM(script_sha256<>'') FROM side_story_episodes
		WHERE kind=? GROUP BY story_id`, kind)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var episodes, fetched int
		if err := rows.Scan(&id, &episodes, &fetched); err != nil {
			rows.Close()
			return nil, err
		}
		if summary := index[id]; summary != nil {
			summary.EpisodeCount, summary.FetchedEpisodeCount = episodes, fetched
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT story_id,COUNT(*) FROM side_story_lines WHERE kind=? GROUP BY story_id`, kind)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var lines int
		if err := rows.Scan(&id, &lines); err != nil {
			rows.Close()
			return nil, err
		}
		if summary := index[id]; summary != nil {
			summary.LineCount = lines
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT loc.story_id,loc.source,SUM(loc.text<>''),SUM(loc.text<>'' AND l.role<>'title'),
		MAX(loc.updated_at) FROM side_story_line_localizations loc
		JOIN side_story_lines l ON l.kind=loc.kind AND l.story_id=loc.story_id AND l.episode_key=loc.episode_key AND l.jp_key=loc.jp_key
		WHERE loc.locale=? AND loc.kind=? GROUP BY loc.story_id,loc.source`, locale, kind)
	if err != nil {
		return nil, err
	}
	latestWrite, translatedDialogue := map[string]int64{}, map[string]int{}
	for rows.Next() {
		var id, source string
		var translated, dialogue int
		var updated int64
		if err := rows.Scan(&id, &source, &translated, &dialogue, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		if summary := index[id]; summary != nil {
			summary.SourceCounts.add(source, translated)
			summary.TranslatedCount += translated
			translatedDialogue[id] += dialogue
			latestWrite[id] = max(latestWrite[id], updated)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for position := range summaries {
		summary := &summaries[position]
		summary.UntranslatedCount = summary.LineCount - summary.TranslatedCount
		// Title-only stories read like untranslated ones: no primary source.
		if translatedDialogue[summary.ID] > 0 {
			summary.PrimarySource = sideStoryPrimarySource(summary.SourceCounts)
		}
		summary.Status = sideStoryStatus(summary.FetchedEpisodeCount, translatedDialogue[summary.ID], summary.UntranslatedCount)
		if written, ok := latestWrite[summary.ID]; ok {
			summary.UpdatedAt = written
		}
	}
	sort.SliceStable(summaries, func(i, j int) bool { return sideStoryNewerFirst(summaries[i], summaries[j]) })
	return summaries, nil
}

// sideStoryNewerFirst orders by release time, then by the newer action set or
// card id.
func sideStoryNewerFirst(a, b SideStorySummary) bool {
	if a.ReleasedAt != b.ReleasedAt {
		return a.ReleasedAt > b.ReleasedAt
	}
	if a.ActionSetID != b.ActionSetID {
		return a.ActionSetID > b.ActionSetID
	}
	if len(a.ID) != len(b.ID) {
		return len(a.ID) > len(b.ID)
	}
	return a.ID > b.ID
}

// SideStoryDetailContext returns one story with every line in locale;
// sql.ErrNoRows for an unknown story.
func (s *Store) SideStoryDetailContext(ctx context.Context, kind, storyID, locale string) (SideStoryDetail, error) {
	if err := validSideStoryRequest(kind, storyID, locale); err != nil {
		return SideStoryDetail{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SideStoryDetail{}, err
	}
	defer tx.Rollback()
	detail := SideStoryDetail{Kind: kind, ID: storyID, Locale: locale, Episodes: []SideStoryEpisodeDetail{}}
	if err := tx.QueryRowContext(ctx, `SELECT title,character_id,area_id,area_category,action_set_id FROM side_stories
		WHERE kind=? AND story_id=?`, kind, storyID).Scan(&detail.Title, &detail.CharacterID, &detail.AreaID,
		&detail.AreaCategory, &detail.ActionSetID); err != nil {
		return SideStoryDetail{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT episode_key,scenario_id,title_jp,script_sha256,cn_state,en_state,last_error
		FROM side_story_episodes WHERE kind=? AND story_id=? ORDER BY position,episode_key`, kind, storyID)
	if err != nil {
		return SideStoryDetail{}, err
	}
	for rows.Next() {
		episode := SideStoryEpisodeDetail{Lines: []SideStoryLineState{}}
		if err := rows.Scan(&episode.Key, &episode.ScenarioID, &episode.Title, &episode.ScriptSHA256, &episode.CNState,
			&episode.ENState, &episode.LastError); err != nil {
			rows.Close()
			return SideStoryDetail{}, err
		}
		episode.Fetched = episode.ScriptSHA256 != ""
		detail.Episodes = append(detail.Episodes, episode)
	}
	if err := rows.Err(); err != nil {
		return SideStoryDetail{}, err
	}
	for position := range detail.Episodes {
		episode := &detail.Episodes[position]
		lines, err := loadSideStoryEpisodeLinesTx(ctx, tx, kind, storyID, episode.Key, locale)
		if err != nil {
			return SideStoryDetail{}, err
		}
		for _, line := range lines {
			episode.Lines = append(episode.Lines, line.SideStoryLineState)
			if line.Text != "" {
				episode.TranslatedCount++
			} else {
				episode.UntranslatedCount++
			}
		}
	}
	return detail, nil
}

// SideStoryProgressContext returns backfill totals keyed "card" and "area".
func (s *Store) SideStoryProgressContext(ctx context.Context) (map[string]SideStoryKindProgress, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	totals := map[string]*SideStoryKindProgress{SideStoryKindCard: {}, SideStoryKindArea: {}}
	rows, err := tx.QueryContext(ctx, `SELECT kind,COUNT(*) FROM side_stories GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind string
		var stories int
		if err := rows.Scan(&kind, &stories); err != nil {
			rows.Close()
			return nil, err
		}
		if progress := totals[kind]; progress != nil {
			progress.Stories = stories
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT kind,COUNT(*),SUM(script_sha256<>''),SUM(script_sha256='' OR jp_refetch=1),
		SUM(cn_state='imported'),SUM(cn_state='pending'),SUM(cn_state='absent'),SUM(cn_state='mismatch'),SUM(cn_state='error'),
		SUM(en_state='imported'),SUM(en_state='pending'),SUM(en_state='absent'),SUM(en_state='mismatch'),SUM(en_state='error'),
		SUM(last_error LIKE 'ja-JP: %' AND (script_sha256='' OR jp_refetch=1))
		FROM side_story_episodes GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind string
		var p SideStoryKindProgress
		if err := rows.Scan(&kind, &p.Episodes, &p.Fetched, &p.PendingFetch,
			&p.CNImported, &p.CNPending, &p.CNAbsent, &p.CNMismatch, &p.CNError,
			&p.ENImported, &p.ENPending, &p.ENAbsent, &p.ENMismatch, &p.ENError, &p.Errors); err != nil {
			rows.Close()
			return nil, err
		}
		if progress := totals[kind]; progress != nil {
			p.Stories = progress.Stories
			*progress = p
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]SideStoryKindProgress, len(totals))
	for kind, progress := range totals {
		out[kind] = *progress
	}
	return out, nil
}
