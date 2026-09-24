package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Side-story records of translation-content/side-stories.json. Every column
// of the four v39 tables is carried so a restore reproduces the rows exactly.
type SideStoryBackupRecord struct {
	Kind         string `json:"kind"`
	StoryID      string `json:"storyId"`
	Title        string `json:"title"`
	CharacterID  int    `json:"characterId"`
	AreaID       int    `json:"areaId"`
	AreaCategory string `json:"areaCategory"`
	ActionSetID  int    `json:"actionSetId"`
	ReleasedAt   int64  `json:"releasedAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

type SideStoryEpisodeBackupRecord struct {
	Kind          string `json:"kind"`
	StoryID       string `json:"storyId"`
	EpisodeKey    string `json:"episodeKey"`
	ScenarioID    string `json:"scenarioId"`
	TitleJP       string `json:"titleJp"`
	Position      int    `json:"position"`
	JPAssetPath   string `json:"jpAssetPath"`
	CNAssetPath   string `json:"cnAssetPath"`
	ENAssetPath   string `json:"enAssetPath"`
	ScriptSHA256  string `json:"scriptSha256"`
	JPFetchedAt   int64  `json:"jpFetchedAt"`
	JPRefetch     bool   `json:"jpRefetch"`
	CNState       string `json:"cnState"`
	ENState       string `json:"enState"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt int64  `json:"nextAttemptAt"`
	LastError     string `json:"lastError"`
	UpdatedAt     int64  `json:"updatedAt"`
}

type SideStoryLineBackupRecord struct {
	Kind       string `json:"kind"`
	StoryID    string `json:"storyId"`
	EpisodeKey string `json:"episodeKey"`
	JPKey      string `json:"jpKey"`
	Role       string `json:"role"`
	Speaker    string `json:"speaker"`
	Position   int    `json:"position"`
}

type SideStoryLocalizationBackupRecord struct {
	Kind       string `json:"kind"`
	StoryID    string `json:"storyId"`
	EpisodeKey string `json:"episodeKey"`
	JPKey      string `json:"jpKey"`
	Locale     string `json:"locale"`
	Text       string `json:"text"`
	Source     string `json:"source"`
	Revision   int    `json:"revision"`
	UpdatedBy  string `json:"updatedBy"`
	UpdatedAt  int64  `json:"updatedAt"`
}

type SideStoryContentExport struct {
	Stories       []SideStoryBackupRecord             `json:"stories"`
	Episodes      []SideStoryEpisodeBackupRecord      `json:"episodes"`
	Lines         []SideStoryLineBackupRecord         `json:"lines"`
	Localizations []SideStoryLocalizationBackupRecord `json:"localizations"`
}

// ExportSideStoryContentContext reads the four side-story tables in one
// read-only transaction, each ordered by its primary key.
func (s *Store) ExportSideStoryContentContext(ctx context.Context) (SideStoryContentExport, error) {
	result := SideStoryContentExport{
		Stories: []SideStoryBackupRecord{}, Episodes: []SideStoryEpisodeBackupRecord{},
		Lines: []SideStoryLineBackupRecord{}, Localizations: []SideStoryLocalizationBackupRecord{},
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	queries := []struct {
		query string
		scan  func(*sql.Rows) error
	}{
		{`SELECT kind,story_id,title,character_id,area_id,area_category,action_set_id,released_at,updated_at
			FROM side_stories ORDER BY kind,story_id`, func(rows *sql.Rows) error {
			var record SideStoryBackupRecord
			if err := rows.Scan(&record.Kind, &record.StoryID, &record.Title, &record.CharacterID, &record.AreaID,
				&record.AreaCategory, &record.ActionSetID, &record.ReleasedAt, &record.UpdatedAt); err != nil {
				return err
			}
			result.Stories = append(result.Stories, record)
			return nil
		}},
		{`SELECT kind,story_id,episode_key,scenario_id,title_jp,position,jp_asset_path,cn_asset_path,en_asset_path,
			script_sha256,jp_fetched_at,jp_refetch,cn_state,en_state,attempts,next_attempt_at,last_error,updated_at
			FROM side_story_episodes ORDER BY kind,story_id,episode_key`, func(rows *sql.Rows) error {
			var record SideStoryEpisodeBackupRecord
			if err := rows.Scan(&record.Kind, &record.StoryID, &record.EpisodeKey, &record.ScenarioID, &record.TitleJP,
				&record.Position, &record.JPAssetPath, &record.CNAssetPath, &record.ENAssetPath, &record.ScriptSHA256,
				&record.JPFetchedAt, &record.JPRefetch, &record.CNState, &record.ENState, &record.Attempts,
				&record.NextAttemptAt, &record.LastError, &record.UpdatedAt); err != nil {
				return err
			}
			result.Episodes = append(result.Episodes, record)
			return nil
		}},
		{`SELECT kind,story_id,episode_key,jp_key,role,speaker,position
			FROM side_story_lines ORDER BY kind,story_id,episode_key,jp_key`, func(rows *sql.Rows) error {
			var record SideStoryLineBackupRecord
			if err := rows.Scan(&record.Kind, &record.StoryID, &record.EpisodeKey, &record.JPKey, &record.Role,
				&record.Speaker, &record.Position); err != nil {
				return err
			}
			result.Lines = append(result.Lines, record)
			return nil
		}},
		{`SELECT kind,story_id,episode_key,jp_key,locale,text,source,revision,updated_by,updated_at
			FROM side_story_line_localizations ORDER BY kind,story_id,episode_key,jp_key,locale`, func(rows *sql.Rows) error {
			var record SideStoryLocalizationBackupRecord
			if err := rows.Scan(&record.Kind, &record.StoryID, &record.EpisodeKey, &record.JPKey, &record.Locale,
				&record.Text, &record.Source, &record.Revision, &record.UpdatedBy, &record.UpdatedAt); err != nil {
				return err
			}
			result.Localizations = append(result.Localizations, record)
			return nil
		}},
	}
	for _, query := range queries {
		if err := scanSideStoryBackupRows(ctx, tx, query.query, query.scan); err != nil {
			return result, err
		}
	}
	return result, nil
}

func scanSideStoryBackupRows(ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validSideStoryState(state string) bool {
	switch state {
	case SideStoryStatePending, SideStoryStateImported, SideStoryStateAbsent, SideStoryStateMismatch, SideStoryStateError:
		return true
	}
	return false
}

// validateRestoredSideStories checks identity, enumerations and parent rows of
// a side-story backup before anything is deleted. Numeric ranges and text
// types are left to the v39 CHECK constraints.
func validateRestoredSideStories(ctx context.Context, content SideStoryContentExport) error {
	stories := make(map[string]bool, len(content.Stories))
	for _, record := range content.Stories {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !ValidSideStoryKind(record.Kind) || !ValidSideStoryID(record.Kind, record.StoryID) {
			return fmt.Errorf("side story backup: story %q/%q has an invalid kind or id", record.Kind, record.StoryID)
		}
		key := record.Kind + "\x00" + record.StoryID
		if stories[key] {
			return fmt.Errorf("side story backup: story %s/%s is repeated", record.Kind, record.StoryID)
		}
		stories[key] = true
	}
	episodes := make(map[string]bool, len(content.Episodes))
	for _, record := range content.Episodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := fmt.Sprintf("%q/%q/%q", record.Kind, record.StoryID, record.EpisodeKey)
		if !stories[record.Kind+"\x00"+record.StoryID] {
			return fmt.Errorf("side story backup: episode %s has no story", name)
		}
		if !ValidSideStoryEpisodeKey(record.Kind, record.EpisodeKey) {
			return fmt.Errorf("side story backup: episode %s has an invalid episode key", name)
		}
		if !sideStoryAreaIDPattern.MatchString(record.ScenarioID) ||
			(record.Kind == SideStoryKindArea && record.ScenarioID != record.StoryID) {
			return fmt.Errorf("side story backup: episode %s has an invalid scenario id %q", name, record.ScenarioID)
		}
		if !validSideStoryState(record.CNState) || !validSideStoryState(record.ENState) {
			return fmt.Errorf("side story backup: episode %s has an invalid state %q/%q", name, record.CNState, record.ENState)
		}
		if record.ScriptSHA256 != "" && !isCanonicalContentBackupSHA256(record.ScriptSHA256) {
			return fmt.Errorf("side story backup: episode %s has an invalid script sha256", name)
		}
		key := record.Kind + "\x00" + record.StoryID + "\x00" + record.EpisodeKey
		if episodes[key] {
			return fmt.Errorf("side story backup: episode %s is repeated", name)
		}
		episodes[key] = true
	}
	lines := make(map[string]bool, len(content.Lines))
	for _, record := range content.Lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		episode := record.Kind + "\x00" + record.StoryID + "\x00" + record.EpisodeKey
		name := fmt.Sprintf("%q/%q/%q line %q", record.Kind, record.StoryID, record.EpisodeKey, record.JPKey)
		if !episodes[episode] {
			return fmt.Errorf("side story backup: %s has no episode", name)
		}
		if record.JPKey == "" || record.JPKey != strings.TrimSpace(record.JPKey) {
			return fmt.Errorf("side story backup: %s has an invalid jp key", name)
		}
		switch {
		case record.Role == SideStoryRoleTitle && record.Position == -1:
		case (record.Role == SideStoryRoleTalk || record.Role == SideStoryRoleSpeaker) && record.Position >= 0:
		default:
			return fmt.Errorf("side story backup: %s has an invalid role %q at position %d", name, record.Role, record.Position)
		}
		key := episode + "\x00" + record.JPKey
		if lines[key] {
			return fmt.Errorf("side story backup: %s is repeated", name)
		}
		lines[key] = true
	}
	localizations := make(map[string]bool, len(content.Localizations))
	for _, record := range content.Localizations {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := record.Kind + "\x00" + record.StoryID + "\x00" + record.EpisodeKey + "\x00" + record.JPKey
		name := fmt.Sprintf("%q/%q/%q line %q locale %q", record.Kind, record.StoryID, record.EpisodeKey, record.JPKey, record.Locale)
		if !lines[line] {
			return fmt.Errorf("side story backup: localization %s has no line", name)
		}
		if !ValidSideStoryLocale(record.Locale) {
			return fmt.Errorf("side story backup: localization %s has an invalid locale", name)
		}
		if record.Source != SideStorySourceOfficial && record.Source != SideStorySourceLLM && record.Source != SideStorySourceHuman {
			return fmt.Errorf("side story backup: localization %s has an invalid source %q", name, record.Source)
		}
		if record.Revision <= 0 {
			return fmt.Errorf("side story backup: localization %s has an invalid revision %d", name, record.Revision)
		}
		key := line + "\x00" + record.Locale
		if localizations[key] {
			return fmt.Errorf("side story backup: localization %s is repeated", name)
		}
		localizations[key] = true
	}
	return nil
}

func deleteSideStoryContentTx(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`DELETE FROM side_story_line_localizations`,
		`DELETE FROM side_story_lines`,
		`DELETE FROM side_story_episodes`,
		`DELETE FROM side_stories`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// importSideStoryContentRowsTx inserts validated rows into the cleared tables.
func importSideStoryContentRowsTx(ctx context.Context, tx *sql.Tx, content SideStoryContentExport) error {
	insert := func(table, query string, count int, args func(int) []any) error {
		if count == 0 {
			return nil
		}
		statement, err := tx.PrepareContext(ctx, query)
		if err != nil {
			return err
		}
		defer statement.Close()
		for index := 0; index < count; index++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := statement.ExecContext(ctx, args(index)...); err != nil {
				return fmt.Errorf("side story backup: %s row %d: %w", table, index, err)
			}
		}
		return nil
	}
	if err := insert("side_stories", `INSERT INTO side_stories(kind,story_id,title,character_id,area_id,area_category,action_set_id,
		released_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`, len(content.Stories), func(index int) []any {
		record := content.Stories[index]
		return []any{record.Kind, record.StoryID, record.Title, record.CharacterID, record.AreaID, record.AreaCategory,
			record.ActionSetID, record.ReleasedAt, record.UpdatedAt}
	}); err != nil {
		return err
	}
	if err := insert("side_story_episodes", `INSERT INTO side_story_episodes(kind,story_id,episode_key,scenario_id,title_jp,position,
		jp_asset_path,cn_asset_path,en_asset_path,script_sha256,jp_fetched_at,jp_refetch,cn_state,en_state,attempts,
		next_attempt_at,last_error,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, len(content.Episodes), func(index int) []any {
		record := content.Episodes[index]
		refetch := 0
		if record.JPRefetch {
			refetch = 1
		}
		return []any{record.Kind, record.StoryID, record.EpisodeKey, record.ScenarioID, record.TitleJP, record.Position,
			record.JPAssetPath, record.CNAssetPath, record.ENAssetPath, record.ScriptSHA256, record.JPFetchedAt, refetch,
			record.CNState, record.ENState, record.Attempts, record.NextAttemptAt, record.LastError, record.UpdatedAt}
	}); err != nil {
		return err
	}
	if err := insert("side_story_lines", `INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,speaker,position)
		VALUES (?,?,?,?,?,?,?)`, len(content.Lines), func(index int) []any {
		record := content.Lines[index]
		return []any{record.Kind, record.StoryID, record.EpisodeKey, record.JPKey, record.Role, record.Speaker, record.Position}
	}); err != nil {
		return err
	}
	return insert("side_story_line_localizations", `INSERT INTO side_story_line_localizations(kind,story_id,episode_key,jp_key,locale,text,source,
		revision,updated_by,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, len(content.Localizations), func(index int) []any {
		record := content.Localizations[index]
		return []any{record.Kind, record.StoryID, record.EpisodeKey, record.JPKey, record.Locale, record.Text,
			record.Source, record.Revision, record.UpdatedBy, record.UpdatedAt}
	})
}
