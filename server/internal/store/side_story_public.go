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
		files[key] = file
	}
	return files, nil
}
