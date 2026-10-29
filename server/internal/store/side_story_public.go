package store

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
)

// sideStoryPublicFileKey is the file of a story relative to the translation
// root.
func sideStoryPublicFileKey(kind, storyID string, actionSetID int) string {
	if kind == SideStoryKindArea {
		return "areaTalk/group_" + strconv.Itoa(actionSetID/100) + ".json"
	}
	return "cardStory/card_" + storyID + ".json"
}

// sideStoryPublicLabel aggregates translated-line sources: the official label
// when every line is official, else human when any is, else llm.
func sideStoryPublicLabel(locale string, counts SideStorySourceCounts) string {
	switch {
	case counts.Human == 0 && counts.LLM == 0:
		return SideStoryPublicSourceLabel(locale, SideStorySourceOfficial)
	case counts.Human > 0:
		return SideStorySourceHuman
	}
	return SideStorySourceLLM
}

type sideStoryPublicScope struct {
	where string
	args  []any
}

type sideStoryPublicTalkRow struct {
	position int
	talk     SideStoryPublicTalk
}

type sideStoryPublicEpisodeRow struct {
	kind, storyID, key, scenarioID string
	position, actionSetID          int
	title                          string
	talk                           []sideStoryPublicTalkRow
	sources                        SideStorySourceCounts
	lastUpdated                    int64
}

// SideStoryPublicFilesContext projects every public side-story file of
// locale, keyed "cardStory/card_<id>.json" or "areaTalk/group_<n>.json".
func (s *Store) SideStoryPublicFilesContext(ctx context.Context, locale string) (map[string]SideStoryPublicFile, error) {
	if !ValidSideStoryLocale(locale) {
		return nil, sideStoryInvalid("locale %q", locale)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return buildSideStoryPublicFilesTx(ctx, tx, locale, sideStoryPublicScope{})
}

// SideStoryPublicFileForStoryContext projects the one file that holds a
// story. key is returned even when ok is false (no translated talk or
// speaker line), so a caller can remove a stale file; sql.ErrNoRows for an
// unknown story.
func (s *Store) SideStoryPublicFileForStoryContext(ctx context.Context, kind, storyID, locale string) (string, SideStoryPublicFile, bool, error) {
	if err := validSideStoryRequest(kind, storyID, locale); err != nil {
		return "", SideStoryPublicFile{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", SideStoryPublicFile{}, false, err
	}
	defer tx.Rollback()
	var actionSetID int
	if err := tx.QueryRowContext(ctx, `SELECT action_set_id FROM side_stories WHERE kind=? AND story_id=?`, kind, storyID).
		Scan(&actionSetID); err != nil {
		return "", SideStoryPublicFile{}, false, err
	}
	key := sideStoryPublicFileKey(kind, storyID, actionSetID)
	scope := sideStoryPublicScope{where: `s.kind='card' AND s.story_id=?`, args: []any{storyID}}
	if kind == SideStoryKindArea {
		group := actionSetID / 100
		scope = sideStoryPublicScope{where: `s.kind='area' AND s.action_set_id BETWEEN ? AND ?`, args: []any{group * 100, group*100 + 99}}
	}
	files, err := buildSideStoryPublicFilesTx(ctx, tx, locale, scope)
	if err != nil {
		return "", SideStoryPublicFile{}, false, err
	}
	file, ok := files[key]
	return key, file, ok, nil
}

// ForEachSideStoryPublicFileContext streams every public side-story file of
// locale one file at a time so full-projection rebuilds do not retain 230k+
// talk records across 4k+ files simultaneously in memory.
func (s *Store) ForEachSideStoryPublicFileContext(
	ctx context.Context, locale string, emit func(key string, file SideStoryPublicFile) error,
) error {
	if !ValidSideStoryLocale(locale) {
		return sideStoryInvalid("locale %q", locale)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := streamCardSideStoryPublicFilesTx(ctx, tx, locale, emit); err != nil {
		return err
	}
	areaFiles, err := buildSideStoryPublicFilesTx(ctx, tx, locale, sideStoryPublicScope{where: `s.kind='area'`})
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(areaFiles))
	for key := range areaFiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(key, areaFiles[key]); err != nil {
			return err
		}
		delete(areaFiles, key)
	}
	return nil
}

func streamCardSideStoryPublicFilesTx(
	ctx context.Context, tx *sql.Tx, locale string, emit func(key string, file SideStoryPublicFile) error,
) error {
	epRows, err := tx.QueryContext(ctx, `SELECT e.story_id,e.episode_key,e.scenario_id,e.position,s.action_set_id
		FROM side_story_episodes e
		JOIN side_stories s ON s.kind=e.kind AND s.story_id=e.story_id
		WHERE s.kind='card'`)
	if err != nil {
		return err
	}
	type cardEpMeta struct {
		key, scenarioID       string
		position, actionSetID int
	}
	byStory := map[string][]cardEpMeta{}
	for epRows.Next() {
		var storyID string
		var meta cardEpMeta
		if err := epRows.Scan(&storyID, &meta.key, &meta.scenarioID, &meta.position, &meta.actionSetID); err != nil {
			epRows.Close()
			return err
		}
		byStory[storyID] = append(byStory[storyID], meta)
	}
	if err := epRows.Err(); err != nil {
		epRows.Close()
		return err
	}
	if err := epRows.Close(); err != nil {
		return err
	}

	locRows, err := tx.QueryContext(ctx, `SELECT loc.story_id,loc.episode_key,loc.jp_key,l.role,l.position,loc.text,loc.source,loc.updated_at
		FROM side_story_line_localizations loc
		JOIN side_story_lines l ON l.kind=loc.kind AND l.story_id=loc.story_id AND l.episode_key=loc.episode_key AND l.jp_key=loc.jp_key
		WHERE loc.kind='card' AND loc.locale=? AND loc.text<>''
		ORDER BY loc.story_id`, locale)
	if err != nil {
		return err
	}
	defer locRows.Close()

	var currentStoryID string
	var currentEpisodes map[string]*sideStoryPublicEpisodeRow
	flushCurrent := func() error {
		if currentStoryID == "" {
			return nil
		}
		var members []*sideStoryPublicEpisodeRow
		for _, ep := range currentEpisodes {
			if len(ep.talk) > 0 {
				members = append(members, ep)
			}
		}
		currentEpisodes = nil
		storyID := currentStoryID
		currentStoryID = ""
		if len(members) == 0 {
			return nil
		}
		file := assembleSideStoryPublicFile(locale, members)
		return emit("cardStory/card_"+storyID+".json", file)
	}

	for locRows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var storyID, episodeKey, role, source string
		var position int
		var updated int64
		var talk SideStoryPublicTalk
		if err := locRows.Scan(&storyID, &episodeKey, &talk.JP, &role, &position, &talk.Text, &source, &updated); err != nil {
			return err
		}
		if storyID != currentStoryID {
			if err := flushCurrent(); err != nil {
				return err
			}
			currentStoryID = storyID
			metas := byStory[storyID]
			currentEpisodes = make(map[string]*sideStoryPublicEpisodeRow, len(metas))
			for _, m := range metas {
				currentEpisodes[m.key] = &sideStoryPublicEpisodeRow{
					kind: SideStoryKindCard, storyID: storyID, key: m.key,
					scenarioID: m.scenarioID, position: m.position, actionSetID: m.actionSetID,
				}
			}
		}
		ep := currentEpisodes[episodeKey]
		if ep == nil {
			continue
		}
		if role == SideStoryRoleTitle {
			ep.title = talk.Text
		} else {
			ep.talk = append(ep.talk, sideStoryPublicTalkRow{position: position, talk: talk})
		}
		ep.sources.add(source, 1)
		ep.lastUpdated = max(ep.lastUpdated, updated)
	}
	if err := locRows.Err(); err != nil {
		return err
	}
	if err := locRows.Close(); err != nil {
		return err
	}
	return flushCurrent()
}

func assembleSideStoryPublicFile(locale string, members []*sideStoryPublicEpisodeRow) SideStoryPublicFile {
	sort.Slice(members, func(i, j int) bool {
		a, b := members[i], members[j]
		if a.actionSetID != b.actionSetID {
			return a.actionSetID < b.actionSetID
		}
		if a.storyID != b.storyID {
			return a.storyID < b.storyID
		}
		if a.position != b.position {
			return a.position < b.position
		}
		return a.key < b.key
	})
	file := SideStoryPublicFile{Episodes: make([]SideStoryPublicEpisode, 0, len(members))}
	var sources SideStorySourceCounts
	for _, member := range members {
		sort.Slice(member.talk, func(i, j int) bool { return member.talk[i].position < member.talk[j].position })
		talk := make([]SideStoryPublicTalk, len(member.talk))
		for index, row := range member.talk {
			talk[index] = row.talk
		}
		publicKey := member.key
		if member.kind == SideStoryKindArea {
			publicKey = member.storyID
		}
		file.Episodes = append(file.Episodes, SideStoryPublicEpisode{
			Key: publicKey, ScenarioID: member.scenarioID, Title: member.title,
			Source: sideStoryPublicLabel(locale, member.sources), Talk: talk,
		})
		sources.Official += member.sources.Official
		sources.LLM += member.sources.LLM
		sources.Human += member.sources.Human
		file.LastUpdated = max(file.LastUpdated, member.lastUpdated)
	}
	file.Source = sideStoryPublicLabel(locale, sources)
	return file
}

func buildSideStoryPublicFilesTx(ctx context.Context, tx *sql.Tx, locale string, scope sideStoryPublicScope) (map[string]SideStoryPublicFile, error) {
	where := "1"
	if scope.where != "" {
		where = scope.where
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.kind,e.story_id,e.episode_key,e.scenario_id,e.position,s.action_set_id
		FROM side_story_episodes e JOIN side_stories s ON s.kind=e.kind AND s.story_id=e.story_id WHERE `+where, scope.args...)
	if err != nil {
		return nil, err
	}
	episodes := map[string]*sideStoryPublicEpisodeRow{}
	for rows.Next() {
		episode := &sideStoryPublicEpisodeRow{}
		if err := rows.Scan(&episode.kind, &episode.storyID, &episode.key, &episode.scenarioID, &episode.position, &episode.actionSetID); err != nil {
			rows.Close()
			return nil, err
		}
		episodes[episode.kind+"\x00"+episode.storyID+"\x00"+episode.key] = episode
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	storyFilter := ""
	if scope.where != "" {
		storyFilter = ` AND (loc.kind,loc.story_id) IN (SELECT s.kind,s.story_id FROM side_stories s WHERE ` + scope.where + `)`
	}
	rows, err = tx.QueryContext(ctx, `SELECT loc.kind,loc.story_id,loc.episode_key,loc.jp_key,l.role,l.position,loc.text,loc.source,loc.updated_at
		FROM side_story_line_localizations loc
		JOIN side_story_lines l ON l.kind=loc.kind AND l.story_id=loc.story_id AND l.episode_key=loc.episode_key AND l.jp_key=loc.jp_key
		WHERE loc.locale=? AND loc.text<>''`+storyFilter, append([]any{locale}, scope.args...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, storyID, episodeKey, role, source string
		var position int
		var updated int64
		var talk SideStoryPublicTalk
		if err := rows.Scan(&kind, &storyID, &episodeKey, &talk.JP, &role, &position, &talk.Text, &source, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		episode := episodes[kind+"\x00"+storyID+"\x00"+episodeKey]
		if episode == nil {
			continue
		}
		if role == SideStoryRoleTitle {
			episode.title = talk.Text
		} else {
			episode.talk = append(episode.talk, sideStoryPublicTalkRow{position: position, talk: talk})
		}
		episode.sources.add(source, 1)
		episode.lastUpdated = max(episode.lastUpdated, updated)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	byFile := map[string][]*sideStoryPublicEpisodeRow{}
	for _, episode := range episodes {
		if len(episode.talk) == 0 {
			continue
		}
		key := sideStoryPublicFileKey(episode.kind, episode.storyID, episode.actionSetID)
		byFile[key] = append(byFile[key], episode)
	}
	files := make(map[string]SideStoryPublicFile, len(byFile))
	for key, members := range byFile {
		files[key] = assembleSideStoryPublicFile(locale, members)
	}
	return files, nil
}
