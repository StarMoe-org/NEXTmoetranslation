package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type sideStoryLineRow struct {
	SideStoryLineState
	hasRow bool
}

// loadSideStoryEpisodeLinesTx returns the episode's lines with their row in
// locale, by position; sql.ErrNoRows when the episode is unknown.
func loadSideStoryEpisodeLinesTx(ctx context.Context, tx *sql.Tx, kind, storyID, episodeKey, locale string) ([]sideStoryLineRow, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM side_story_episodes WHERE kind=? AND story_id=? AND episode_key=?)`,
		kind, storyID, episodeKey).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.jp_key,l.role,l.speaker,l.position,
			loc.text IS NOT NULL,COALESCE(loc.text,''),COALESCE(loc.source,''),COALESCE(loc.revision,0),
			COALESCE(loc.updated_by,''),COALESCE(loc.updated_at,0)
		FROM side_story_lines l LEFT JOIN side_story_line_localizations loc
			ON loc.kind=l.kind AND loc.story_id=l.story_id AND loc.episode_key=l.episode_key AND loc.jp_key=l.jp_key AND loc.locale=?
		WHERE l.kind=? AND l.story_id=? AND l.episode_key=? ORDER BY l.position, l.jp_key`, locale, kind, storyID, episodeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []sideStoryLineRow
	for rows.Next() {
		var line sideStoryLineRow
		if err := rows.Scan(&line.JP, &line.Role, &line.Speaker, &line.Position, &line.hasRow, &line.Text, &line.Source,
			&line.Revision, &line.UpdatedBy, &line.UpdatedAt); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

// UpdateSideStoryLinesContext writes editor edits of one episode and locale
// all or nothing. Whitespace-only text is stored as the empty line.
func (s *Store) UpdateSideStoryLinesContext(ctx context.Context, kind, storyID, episodeKey, locale, actor string, edits []SideStoryLineEdit, now time.Time) (SideStoryUpdateResult, error) {
	if err := validSideStoryRequest(kind, storyID, locale); err != nil {
		return SideStoryUpdateResult{}, err
	}
	if !ValidSideStoryEpisodeKey(kind, episodeKey) {
		return SideStoryUpdateResult{}, sideStoryInvalid("episode %q", episodeKey)
	}
	if len(edits) == 0 || len(edits) > sideStoryMaxEdits {
		return SideStoryUpdateResult{}, sideStoryInvalid("%d edits, want 1 through %d", len(edits), sideStoryMaxEdits)
	}
	seen := make(map[string]bool, len(edits))
	for index, edit := range edits {
		if seen[edit.JP] {
			return SideStoryUpdateResult{}, sideStoryInvalid("edit %d repeats line %q", index, edit.JP)
		}
		seen[edit.JP] = true
		if !validSideStoryText(edit.Text) {
			return SideStoryUpdateResult{}, sideStoryInvalid("edit %d text must be valid UTF-8 without NUL of at most %d bytes", index, sideStoryMaxTextBytes)
		}
		if edit.Source != "" && edit.Source != SideStorySourceHuman && edit.Source != SideStorySourceLLM {
			return SideStoryUpdateResult{}, sideStoryInvalid("edit %d source %q", index, edit.Source)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SideStoryUpdateResult{}, err
	}
	defer tx.Rollback()
	lines, err := loadSideStoryEpisodeLinesTx(ctx, tx, kind, storyID, episodeKey, locale)
	if err != nil {
		return SideStoryUpdateResult{}, err
	}
	byKey := make(map[string]*sideStoryLineRow, len(lines))
	for index := range lines {
		byKey[lines[index].JP] = &lines[index]
	}
	var unknown []string
	var conflicts []SideStoryLineConflict
	for _, edit := range edits {
		line, ok := byKey[edit.JP]
		if !ok {
			unknown = append(unknown, edit.JP)
			continue
		}
		if edit.ExpectedRevision != nil && *edit.ExpectedRevision != line.Revision {
			conflicts = append(conflicts, SideStoryLineConflict{
				JP: edit.JP, ExpectedRevision: *edit.ExpectedRevision, CurrentRevision: line.Revision,
				CurrentText: line.Text, CurrentSource: line.Source,
			})
		}
	}
	if len(unknown) > 0 {
		return SideStoryUpdateResult{}, &SideStoryUnknownLinesError{Lines: unknown}
	}
	if len(conflicts) > 0 {
		return SideStoryUpdateResult{}, &SideStoryRevisionConflictError{Conflicts: conflicts}
	}
	stamp := now.Unix()
	result := SideStoryUpdateResult{Lines: make([]SideStoryLineState, 0, len(edits))}
	for _, edit := range edits {
		line := byKey[edit.JP]
		source := edit.Source
		if source == "" {
			source = SideStorySourceHuman
		}
		text := edit.Text
		if strings.TrimSpace(text) == "" {
			text = ""
		}
		if line.hasRow && line.Text == text && line.Source == source {
			result.Unchanged++
			result.Lines = append(result.Lines, line.SideStoryLineState)
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO side_story_line_localizations
			(kind,story_id,episode_key,jp_key,locale,text,source,revision,updated_by,updated_at) VALUES (?,?,?,?,?,?,?,1,?,?)
			ON CONFLICT(kind,story_id,episode_key,jp_key,locale) DO UPDATE SET text=excluded.text,source=excluded.source,
				revision=revision+1,updated_by=excluded.updated_by,updated_at=excluded.updated_at`,
			kind, storyID, episodeKey, edit.JP, locale, text, source, actor, stamp); err != nil {
			return SideStoryUpdateResult{}, err
		}
		line.hasRow, line.Text, line.Source, line.Revision, line.UpdatedBy, line.UpdatedAt = true, text, source, line.Revision+1, actor, stamp
		result.Updated++
		result.Lines = append(result.Lines, line.SideStoryLineState)
	}
	if err := tx.Commit(); err != nil {
		return SideStoryUpdateResult{}, err
	}
	return result, nil
}

// SideStoryAITargetsContext lists the lines of every role with no row in
// locale, or an empty non-human row; episodeKey "" = all episodes. It returns
// sql.ErrNoRows for an unknown story or episode.
func (s *Store) SideStoryAITargetsContext(ctx context.Context, kind, storyID, locale, episodeKey string) ([]SideStoryAITarget, error) {
	if err := validSideStoryRequest(kind, storyID, locale); err != nil {
		return nil, err
	}
	if episodeKey != "" && !ValidSideStoryEpisodeKey(kind, episodeKey) {
		return nil, sideStoryInvalid("episode %q", episodeKey)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM side_story_episodes WHERE kind=? AND story_id=? AND (?='' OR episode_key=?))`,
		kind, storyID, episodeKey, episodeKey).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.episode_key,l.jp_key,COALESCE(loc.revision,0)
		FROM side_story_lines l
		JOIN side_story_episodes e ON e.kind=l.kind AND e.story_id=l.story_id AND e.episode_key=l.episode_key
		LEFT JOIN side_story_line_localizations loc
			ON loc.kind=l.kind AND loc.story_id=l.story_id AND loc.episode_key=l.episode_key AND loc.jp_key=l.jp_key AND loc.locale=?
		WHERE l.kind=? AND l.story_id=? AND (?='' OR l.episode_key=?) AND
			(loc.jp_key IS NULL OR (loc.text='' AND loc.source<>'human'))
		ORDER BY e.position, e.episode_key, l.position, l.jp_key`, locale, kind, storyID, episodeKey, episodeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []SideStoryAITarget{}
	for rows.Next() {
		var target SideStoryAITarget
		if err := rows.Scan(&target.EpisodeKey, &target.JP, &target.Revision); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// ApplySideStoryAITranslationsContext writes texts[i] for targets[i] as llm
// by 'ai' only where the line's row still has the target's revision, is empty
// and is not human. Empty or invalid texts are skipped.
func (s *Store) ApplySideStoryAITranslationsContext(ctx context.Context, kind, storyID, locale string, targets []SideStoryAITarget, texts []string, now time.Time) (int, error) {
	if err := validSideStoryRequest(kind, storyID, locale); err != nil {
		return 0, err
	}
	if len(targets) != len(texts) {
		return 0, sideStoryInvalid("%d targets but %d texts", len(targets), len(texts))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stamp := now.Unix()
	written := 0
	for index, target := range targets {
		text := texts[index]
		if text == "" || !validSideStoryText(text) {
			continue
		}
		var result sql.Result
		if target.Revision == 0 {
			result, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO side_story_line_localizations
				(kind,story_id,episode_key,jp_key,locale,text,source,revision,updated_by,updated_at)
				SELECT kind,story_id,episode_key,jp_key,?,?,'llm',1,'ai',? FROM side_story_lines
				WHERE kind=? AND story_id=? AND episode_key=? AND jp_key=?`,
				locale, text, stamp, kind, storyID, target.EpisodeKey, target.JP)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE side_story_line_localizations
				SET text=?,source='llm',revision=revision+1,updated_by='ai',updated_at=?
				WHERE kind=? AND story_id=? AND episode_key=? AND jp_key=? AND locale=? AND revision=? AND text='' AND source<>'human'`,
				text, stamp, kind, storyID, target.EpisodeKey, target.JP, locale, target.Revision)
		}
		if err != nil {
			return 0, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		written += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}
