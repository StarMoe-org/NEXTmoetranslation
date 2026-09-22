package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"moesekai/server/internal/model"
)

var (
	ErrEventSourceConflict   = errors.New("event source identity conflict")
	ErrEventRevisionConflict = errors.New("event translation revision conflict")
)

func (s *EventStore) DetailLocale(eventID int, locale string) (model.EventStoryDetail, error) {
	ordered, err := s.OrderedDetail(eventID)
	if err != nil {
		return model.EventStoryDetail{}, err
	}
	detail := model.EventStoryDetail{Meta: ordered.Meta, Episodes: map[string]model.EventStoryEpisode{}}
	baseByEpisode := map[string]OrderedEpisode{}
	for _, episode := range ordered.Episodes {
		baseByEpisode[episode.EpisodeNo] = episode
		detail.Episodes[episode.EpisodeNo] = model.EventStoryEpisode{
			ScenarioID: episode.ScenarioID, TalkData: map[string]string{},
			TalkSources: map[string]string{}, SpeakerNames: episode.SpeakerNames,
		}
	}
	canonicalSegmentIDs, err := currentEventCanonicalSegmentIDs(s.db, eventID)
	if err != nil {
		return model.EventStoryDetail{}, err
	}
	rows, err := s.db.Query(`SELECT seg.segment_id, seg.episode_no, seg.scenario_id, seg.kind, seg.position,
		seg.jp_key, seg.source_text, seg.source_hash, loc.text, loc.source, COALESCE(loc.revision, 0)
		FROM event_story_segments seg
		LEFT JOIN event_story_segment_localizations loc ON loc.segment_id=seg.segment_id AND loc.locale=?
		WHERE seg.event_id=? ORDER BY seg.episode_no, seg.position`, locale, eventID)
	if err != nil {
		return model.EventStoryDetail{}, err
	}
	defer rows.Close()
	seenByEpisode := map[string]int{}
	for rows.Next() {
		var id, episodeNo, scenarioID, kind, jpKey, sourceText, sourceHash string
		var position int
		var revision int
		var localizedText, localizedSource sql.NullString
		if err := rows.Scan(&id, &episodeNo, &scenarioID, &kind, &position, &jpKey, &sourceText, &sourceHash, &localizedText, &localizedSource, &revision); err != nil {
			return model.EventStoryDetail{}, err
		}
		episode, ok := detail.Episodes[episodeNo]
		if !ok {
			continue
		}
		if scenarioID != baseByEpisode[episodeNo].ScenarioID {
			continue
		}
		if canonicalIDs := canonicalSegmentIDs[episodeNo]; canonicalIDs != nil && !canonicalIDs[id] {
			continue
		}
		text, source := "", model.SourceUnknown
		if locale == model.LocaleJapanese {
			text = sourceText
		} else if localizedText.Valid {
			text = localizedText.String
			source = localizedSource.String
		} else if locale == model.LocaleChinese {
			base := baseByEpisode[episodeNo]
			if kind == "title" {
				text, source = base.Title, base.TitleSource
			} else {
				text, source = base.TalkData[jpKey], base.TalkSources[jpKey]
			}
		}
		segment := model.EventStorySegment{
			ID: id, Kind: kind, Position: position, Japanese: sourceText, SourceHash: sourceHash,
			Text: text, Source: source, Revision: revision,
		}
		episode.Segments = append(episode.Segments, segment)
		if kind == "title" {
			episode.Title = text
			episode.TitleSource = source
		} else {
			episode.TalkData[jpKey] = text
			episode.TalkSources[jpKey] = source
			episode.TalkOrder = append(episode.TalkOrder, jpKey)
		}
		detail.Episodes[episodeNo] = episode
		seenByEpisode[episodeNo]++
	}
	if err := rows.Err(); err != nil {
		return model.EventStoryDetail{}, err
	}
	// A previous binary can replace individual episodes while leaving additive
	// segments behind. Fall back episode-by-episode and never expose a segment
	// whose scenario identity no longer matches its current parent.
	for episodeNo, base := range baseByEpisode {
		if seenByEpisode[episodeNo] == 0 {
			episode := detail.Episodes[episodeNo]
			if locale == model.LocaleJapanese {
				for _, key := range base.TalkKeys {
					episode.TalkData[key] = key
					episode.TalkSources[key] = model.SourceUnknown
					episode.TalkOrder = append(episode.TalkOrder, key)
				}
			} else if locale == model.LocaleChinese {
				episode.Title = base.Title
				episode.TitleSource = base.TitleSource
				for _, key := range base.TalkKeys {
					episode.TalkData[key] = base.TalkData[key]
					episode.TalkSources[key] = base.TalkSources[key]
					episode.TalkOrder = append(episode.TalkOrder, key)
				}
			} else {
				for _, key := range base.TalkKeys {
					episode.TalkData[key] = ""
					episode.TalkSources[key] = model.SourceUnknown
					episode.TalkOrder = append(episode.TalkOrder, key)
				}
			}
			detail.Episodes[episodeNo] = episode
		}
	}
	return detail, nil
}

func (s *EventStore) ListLocale(locale string) ([]model.EventStorySummary, error) {
	s.summaryCacheMu.RLock()
	if cached, ok := s.summaryCache[locale]; ok {
		s.summaryCacheMu.RUnlock()
		return cloneEventStorySummaries(cached), nil
	}
	s.summaryCacheMu.RUnlock()

	summaries, err := s.listSummaries(locale)
	if err != nil {
		return nil, err
	}

	s.summaryCacheMu.Lock()
	if s.summaryCache == nil {
		s.summaryCache = make(map[string][]model.EventStorySummary)
	}
	s.summaryCache[locale] = cloneEventStorySummaries(summaries)
	s.summaryCacheMu.Unlock()

	return summaries, nil
}

func (s *EventStore) UpdateLineLocale(eventID int, episodeNo, jpKey, segmentID, sourceHash, text, source, entryType, locale, user string) error {
	return s.UpdateLineLocaleRevision(eventID, episodeNo, jpKey, segmentID, sourceHash, text, source, entryType, locale, user, nil)
}

// UpdateLineLocaleRevision conditionally applies a localized event edit. A nil
// expectedRevision preserves the existing client contract; current clients can
// echo the authenticated detail revision to reject competing translation edits.
func (s *EventStore) UpdateLineLocaleRevision(eventID int, episodeNo, jpKey, segmentID, sourceHash, text, source, entryType, locale, user string, expectedRevision *int) error {
	if !model.IsValidSource(source) {
		return fmt.Errorf("invalid translation source: %q", source)
	}
	if locale == model.LocaleJapanese {
		return ErrReadOnlyLocale
	}
	if entryType != "title" && entryType != "talk" {
		return ErrEventSourceConflict
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kind := "talk"
	if entryType == "title" {
		kind = "title"
	}
	if segmentID == "" {
		return ErrEventSourceConflict
	}
	var storedEpisode, storedScenario, parentScenario, storedKind, storedJPKey, storedSourceHash string
	if err := tx.QueryRow(`SELECT seg.episode_no, seg.scenario_id, episode.scenario_id, seg.kind, seg.jp_key, seg.source_hash
		FROM event_story_segments seg
		JOIN event_story_episodes episode ON episode.event_id=seg.event_id AND episode.episode_no=seg.episode_no
		WHERE seg.segment_id=? AND seg.event_id=?`, segmentID, eventID).
		Scan(&storedEpisode, &storedScenario, &parentScenario, &storedKind, &storedJPKey, &storedSourceHash); err == sql.ErrNoRows {
		return ErrEventSourceConflict
	} else if err != nil {
		return err
	}
	if storedEpisode != episodeNo || storedScenario != parentScenario || storedKind != kind || storedSourceHash != sourceHash ||
		(kind == "talk" && storedJPKey != jpKey) || (kind == "title" && jpKey != "") {
		return ErrEventSourceConflict
	}
	var currentRevision int
	err = tx.QueryRow(`SELECT revision FROM event_story_segment_localizations
		WHERE segment_id=? AND locale=?`, segmentID, locale).Scan(&currentRevision)
	if err == sql.ErrNoRows {
		currentRevision = 0
	} else if err != nil {
		return err
	}
	if expectedRevision != nil && *expectedRevision != currentRevision {
		return ErrEventRevisionConflict
	}
	now := time.Now().Unix()
	if locale == model.LocaleChinese {
		var result sql.Result
		if entryType == "title" {
			result, err = tx.Exec(`UPDATE event_story_episodes SET title=?, title_source=?
				WHERE event_id=? AND episode_no=?`, text, source, eventID, episodeNo)
		} else {
			result, err = tx.Exec(`UPDATE event_story_lines SET cn_text=?, source=?
				WHERE event_id=? AND episode_no=? AND jp_key=?`, text, source, eventID, episodeNo, jpKey)
		}
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected == 0 {
			return sql.ErrNoRows
		}
		if _, err := tx.Exec(`UPDATE event_stories SET last_updated=? WHERE event_id=?`, now, eventID); err != nil {
			return err
		}
	}
	if currentRevision == 0 {
		if _, err := tx.Exec(`INSERT INTO event_story_segment_localizations
			(segment_id, locale, text, source, updated_at, updated_by, revision)
			VALUES (?, ?, ?, ?, ?, ?, 1)`, segmentID, locale, text, source, now, user); err != nil {
			return err
		}
	} else {
		result, err := tx.Exec(`UPDATE event_story_segment_localizations
			SET text=?, source=?, updated_at=?, updated_by=?, revision=revision+1
			WHERE segment_id=? AND locale=? AND revision=?`,
			text, source, now, user, segmentID, locale, currentRevision)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected != 1 {
			return ErrEventRevisionConflict
		}
	}
	// The summary reads this timestamp in preference to event_stories.last_updated
	// for every locale, zh-CN included.
	if _, err := tx.Exec(`INSERT INTO event_story_locale_meta(event_id, locale, last_updated) VALUES (?, ?, ?)
		ON CONFLICT(event_id, locale) DO UPDATE SET last_updated=excluded.last_updated`, eventID, locale, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts, user, action, detail) VALUES (?, ?, 'event.locale.update', ?)`,
		now, user, eventLocaleAuditDetail(locale, eventID, entryType)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateSummaryCache()
	return nil
}

func eventLocaleAuditDetail(locale string, eventID int, entryType string) string {
	return fmt.Sprintf("locale=%s eventId=%d entryType=%s", locale, eventID, entryType)
}
