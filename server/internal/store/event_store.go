package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
)

// EventStore provides CRUD over event story data backed by SQLite.
type EventStore struct {
	db             *db.DB
	summaryCacheMu sync.RWMutex
	summaryCache   map[string][]model.EventStorySummary
}

func NewEventStore(database *db.DB) *EventStore {
	return &EventStore{
		db:           database,
		summaryCache: make(map[string][]model.EventStorySummary),
	}
}

// InvalidateSummaryCache clears the cached event story summaries.
func (s *EventStore) InvalidateSummaryCache() {
	s.summaryCacheMu.Lock()
	s.summaryCache = make(map[string][]model.EventStorySummary)
	s.summaryCacheMu.Unlock()
}

// OrderedEpisode is an episode with explicit line ordering, used for lossless
// import (map iteration order is not stable, but dialogue flow must survive).
type OrderedEpisode struct {
	EpisodeNo             string
	ScenarioID            string
	ScenarioCanonicalJSON string
	ScenarioSHA256        string
	Title                 string
	TitleSource           string
	SourceTitle           string
	TalkKeys              []string // jp keys in story order
	TalkData              map[string]string
	TalkSources           map[string]string
	SpeakerNames          map[string]string
	Lines                 []OrderedLine
}

type OrderedLine struct {
	JPKey            string
	Text             string
	Source           string
	SpeakerName      string
	ScenarioPosition int
	Field            string
}

// ImportOrdered replaces one event story, preserving episode and line order.
func (s *EventStore) ImportOrdered(eventID int, meta model.EventStoryMeta, episodes []OrderedEpisode) error {
	return s.ImportOrderedContext(context.Background(), eventID, meta, episodes)
}

func (s *EventStore) ImportOrderedContext(ctx context.Context, eventID int, meta model.EventStoryMeta, episodes []OrderedEpisode) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := importOrderedTx(ctx, tx, eventID, meta, episodes, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateSummaryCache()
	return nil
}

// ImportOrderedForSync performs the same replacement as ImportOrdered, but
// rechecks protected local edits after acquiring the write transaction.
func (s *EventStore) ImportOrderedForSync(eventID int, meta model.EventStoryMeta, episodes []OrderedEpisode) (bool, error) {
	return s.ImportOrderedForSyncContext(context.Background(), eventID, meta, episodes)
}

func (s *EventStore) ImportOrderedForSyncContext(ctx context.Context, eventID int, meta model.EventStoryMeta, episodes []OrderedEpisode) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	protected, err := eventHasProtectedTranslationsTx(ctx, tx, eventID)
	if err != nil {
		return false, err
	}
	if protected {
		return false, nil
	}
	if err := importOrderedTx(ctx, tx, eventID, meta, episodes, true); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.InvalidateSummaryCache()
	return true, nil
}

func eventHasProtectedTranslationsTx(ctx context.Context, tx *sql.Tx, eventID int) (bool, error) {
	var protected int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM event_stories WHERE event_id=? AND source IN ('human','pinned')
		UNION ALL
		SELECT 1 FROM event_story_episodes WHERE event_id=? AND title_source IN ('human','pinned')
		UNION ALL
		SELECT 1 FROM event_story_lines WHERE event_id=? AND source IN ('human','pinned')
	)`, eventID, eventID, eventID).Scan(&protected)
	return protected != 0, err
}

func importOrderedTx(ctx context.Context, tx *sql.Tx, eventID int, meta model.EventStoryMeta, episodes []OrderedEpisode, preserveLocales bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scenarioComplete, err := validateOrderedScenarioSet(eventID, episodes)
	if err != nil {
		return err
	}
	activeSegments, err := activeEventSegmentsTx(tx, eventID)
	if err != nil {
		return err
	}
	// Older binaries replace the legacy story rows wholesale. Preserve manual
	// non-Chinese localizations that are still attached to the same source
	// identity before the legacy delete cascades.
	type localization struct {
		locale, text, source, updatedBy string
		updatedAt                       int64
		revision                        int
	}
	type preservedLocalization struct {
		segmentID, episodeNo, kind, sourceHash string
		localizations                          []localization
	}
	type sourceIdentity struct {
		episodeNo, kind, sourceHash string
	}
	var preserved []*preservedLocalization
	oldSourceCounts := map[sourceIdentity]int{}
	if preserveLocales {
		rows, err := tx.QueryContext(ctx, `SELECT loc.segment_id, seg.episode_no, seg.kind, seg.source_hash, loc.locale, loc.text, loc.source,
			loc.updated_at, loc.updated_by, loc.revision
			FROM event_story_segment_localizations loc
			JOIN event_story_segments seg ON seg.segment_id=loc.segment_id
			JOIN event_story_episodes episode
			ON episode.event_id=seg.event_id AND episode.episode_no=seg.episode_no
			AND episode.scenario_id=seg.scenario_id
			WHERE seg.event_id=? AND loc.locale<>?
			ORDER BY loc.segment_id, loc.locale`, eventID, model.LocaleChinese)
		if err != nil {
			return err
		}
		preservedByID := map[string]*preservedLocalization{}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return err
			}
			var segmentID, episodeNo, kind, sourceHash string
			var item localization
			if err := rows.Scan(&segmentID, &episodeNo, &kind, &sourceHash, &item.locale, &item.text, &item.source,
				&item.updatedAt, &item.updatedBy, &item.revision); err != nil {
				rows.Close()
				return err
			}
			if _, active := activeSegments[segmentID]; !active {
				continue
			}
			segment := preservedByID[segmentID]
			if segment == nil {
				segment = &preservedLocalization{segmentID: segmentID, episodeNo: episodeNo, kind: kind, sourceHash: sourceHash}
				preservedByID[segmentID] = segment
				preserved = append(preserved, segment)
			}
			segment.localizations = append(segment.localizations, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, segment := range activeSegments {
			oldSourceCounts[sourceIdentity{episodeNo: segment.EpisodeNo, kind: segment.Kind, sourceHash: segment.SourceHash}]++
		}
	}
	for segmentID := range activeSegments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM event_story_segments WHERE segment_id=?`, segmentID); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM event_stories WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	if scenarioComplete {
		for _, episode := range episodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			record := EventScenarioRecord{
				EventID: eventID, EpisodeNo: episode.EpisodeNo, ScenarioID: episode.ScenarioID,
				CanonicalJSON: episode.ScenarioCanonicalJSON, SHA256: episode.ScenarioSHA256,
			}
			definitions, err := eventScenarioSegmentDefinitions(record, episode.SourceTitle)
			if err != nil {
				return err
			}
			for _, definition := range definitions {
				// A rolling writer may have moved away from this scenario and left
				// its deterministic ID as recovery data. Move that row aside before
				// the scenario identity cycles back and creates a fresh active row.
				if err := archiveEventSegmentTx(tx, definition.SegmentID); err != nil {
					return err
				}
			}
		}
	}
	if meta.Version == "" {
		meta.Version = "1.0"
	}
	if meta.LastUpdated == 0 {
		meta.LastUpdated = time.Now().Unix()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO event_stories (event_id, source, version, last_updated) VALUES (?, ?, ?, ?)`,
		eventID, meta.Source, meta.Version, meta.LastUpdated); err != nil {
		return err
	}

	epStmt, err := tx.PrepareContext(ctx, `INSERT INTO event_story_episodes
		(event_id, episode_no, scenario_id, title, title_source, talk_order_json, position)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer epStmt.Close()
	lineStmt, err := tx.PrepareContext(ctx, `INSERT INTO event_story_lines
		(event_id, episode_no, jp_key, cn_text, source, speaker_name, position)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer lineStmt.Close()
	segmentStmt, err := tx.PrepareContext(ctx, `INSERT INTO event_story_segments
		(segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer segmentStmt.Close()
	localizedStmt, err := tx.PrepareContext(ctx, `INSERT INTO event_story_segment_localizations
		(segment_id, locale, text, source, updated_at, updated_by, revision)
		VALUES (?, ?, ?, ?, ?, ?, 1)`)
	if err != nil {
		return err
	}
	defer localizedStmt.Close()
	newSegmentIDs := map[string]bool{}

	for epPos, ep := range episodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		// talk_order_json is stored only when it adds information beyond the
		// natural line position order (kept empty here; positions drive order).
		if _, err := epStmt.ExecContext(ctx, eventID, ep.EpisodeNo, ep.ScenarioID, ep.Title, ep.TitleSource, "", epPos); err != nil {
			return err
		}
		titleID := eventSegmentID(eventID, ep.ScenarioID, ep.EpisodeNo, "title", -1)
		titleSource := ep.SourceTitle
		if titleSource == "" && (meta.Source == "jp_pending" || ep.TitleSource == "jp_pending") {
			titleSource = ep.Title
		}
		if _, err := segmentStmt.ExecContext(ctx, titleID, eventID, ep.EpisodeNo, ep.ScenarioID, "title", -1, "", titleSource, hashText(titleSource)); err != nil {
			return err
		}
		newSegmentIDs[titleID] = true
		if titleSource == "" {
			if _, err := localizedStmt.ExecContext(ctx, titleID, model.LocaleChinese, ep.Title, ep.TitleSource, meta.LastUpdated, "import"); err != nil {
				return err
			}
		}
		lines := ep.Lines
		if len(lines) == 0 {
			for _, jp := range ep.TalkKeys {
				cn, ok := ep.TalkData[jp]
				if !ok {
					continue
				}
				line := OrderedLine{JPKey: jp, Text: cn, Source: meta.Source}
				if ep.TalkSources != nil && ep.TalkSources[jp] != "" {
					line.Source = ep.TalkSources[jp]
				}
				if ep.SpeakerNames != nil {
					line.SpeakerName = ep.SpeakerNames[jp]
				}
				lines = append(lines, line)
			}
		}
		legacyByKey := map[string]OrderedLine{}
		legacyOrder := append([]string(nil), ep.TalkKeys...)
		legacySeen := map[string]bool{}
		for _, line := range lines {
			if err := ctx.Err(); err != nil {
				return err
			}
			legacyByKey[line.JPKey] = line // legacy maps kept the final repeated value
			if len(ep.TalkKeys) == 0 && line.JPKey != "" && !legacySeen[line.JPKey] {
				legacyOrder = append(legacyOrder, line.JPKey)
				legacySeen[line.JPKey] = true
			}
		}
		for legacyPosition, jpKey := range legacyOrder {
			if err := ctx.Err(); err != nil {
				return err
			}
			line, ok := legacyByKey[jpKey]
			if !ok {
				continue
			}
			if ep.TalkData != nil {
				if text, exists := ep.TalkData[jpKey]; exists {
					line.Text = text
				}
			}
			if ep.TalkSources != nil && ep.TalkSources[jpKey] != "" {
				line.Source = ep.TalkSources[jpKey]
			}
			if ep.SpeakerNames != nil && ep.SpeakerNames[jpKey] != "" {
				line.SpeakerName = ep.SpeakerNames[jpKey]
			}
			if line.Source == "" {
				line.Source = meta.Source
			}
			if _, err := lineStmt.ExecContext(ctx, eventID, ep.EpisodeNo, jpKey, line.Text, line.Source, line.SpeakerName, legacyPosition); err != nil {
				return err
			}
		}
		for linePos, line := range lines {
			if err := ctx.Err(); err != nil {
				return err
			}
			if line.JPKey == "" {
				continue
			}
			src := line.Source
			if src == "" {
				src = meta.Source
			}
			position := linePos
			if line.Field != "" {
				position = line.ScenarioPosition
			}
			segmentID := eventSegmentID(eventID, ep.ScenarioID, ep.EpisodeNo, "talk", position, line.Field)
			if _, err := segmentStmt.ExecContext(ctx, segmentID, eventID, ep.EpisodeNo, ep.ScenarioID, "talk", position, line.JPKey, line.JPKey, hashText(line.JPKey)); err != nil {
				return err
			}
			newSegmentIDs[segmentID] = true
			if _, err := localizedStmt.ExecContext(ctx, segmentID, model.LocaleChinese, line.Text, src, meta.LastUpdated, "import"); err != nil {
				return err
			}
		}
	}
	assignments := map[string]string{}
	claimedDestinations := map[string]bool{}
	// Reserve stable exact matches before considering any positional migration
	// fallback, so a fallback can never steal another segment's destination.
	for _, item := range preserved {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !newSegmentIDs[item.segmentID] {
			continue
		}
		var currentHash string
		err := tx.QueryRowContext(ctx, `SELECT source_hash FROM event_story_segments WHERE segment_id=?`, item.segmentID).Scan(&currentHash)
		if err == nil && currentHash == item.sourceHash {
			assignments[item.segmentID] = item.segmentID
			claimedDestinations[item.segmentID] = true
		} else if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	for _, item := range preserved {
		if err := ctx.Err(); err != nil {
			return err
		}
		if assignments[item.segmentID] != "" {
			continue
		}
		identity := sourceIdentity{episodeNo: item.episodeNo, kind: item.kind, sourceHash: item.sourceHash}
		if oldSourceCounts[identity] != 1 {
			continue
		}
		candidateRows, err := tx.QueryContext(ctx, `SELECT segment_id FROM event_story_segments
			WHERE event_id=? AND episode_no=? AND kind=? AND source_hash=?`,
			eventID, item.episodeNo, item.kind, item.sourceHash)
		if err != nil {
			return err
		}
		var candidates []string
		for candidateRows.Next() {
			if err := ctx.Err(); err != nil {
				candidateRows.Close()
				return err
			}
			var candidate string
			if err := candidateRows.Scan(&candidate); err != nil {
				candidateRows.Close()
				return err
			}
			if newSegmentIDs[candidate] && !claimedDestinations[candidate] {
				candidates = append(candidates, candidate)
			}
		}
		if err := candidateRows.Err(); err != nil {
			candidateRows.Close()
			return err
		}
		if err := candidateRows.Close(); err != nil {
			return err
		}
		if len(candidates) == 1 {
			assignments[item.segmentID] = candidates[0]
			claimedDestinations[candidates[0]] = true
		}
	}
	for _, item := range preserved {
		if err := ctx.Err(); err != nil {
			return err
		}
		targetSegmentID := assignments[item.segmentID]
		if targetSegmentID == "" {
			continue
		}
		for _, localization := range item.localizations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO event_story_segment_localizations
				(segment_id, locale, text, source, updated_at, updated_by, revision)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				targetSegmentID, localization.locale, localization.text, localization.source, localization.updatedAt,
				localization.updatedBy, localization.revision); err != nil {
				return err
			}
		}
	}
	if scenarioComplete {
		for _, episode := range episodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := reconcileEventScenarioSegmentsTx(tx, eventID, episode); err != nil {
				return err
			}
		}
		if err := replaceEventScenariosTx(tx, eventID, episodes); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func activeEventSegmentsTx(tx *sql.Tx, eventID int) (map[string]EventSegmentRecord, error) {
	rows, err := tx.Query(`SELECT segment.segment_id, segment.event_id, segment.episode_no, segment.scenario_id,
		segment.kind, segment.position, segment.jp_key, segment.source_text, segment.source_hash
		FROM event_story_segments segment
		JOIN event_story_episodes episode
		ON episode.event_id=segment.event_id AND episode.episode_no=segment.episode_no
		AND episode.scenario_id=segment.scenario_id
		WHERE segment.event_id=?`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]EventSegmentRecord{}
	for rows.Next() {
		var segment EventSegmentRecord
		if err := rows.Scan(&segment.SegmentID, &segment.EventID, &segment.EpisodeNo, &segment.ScenarioID,
			&segment.Kind, &segment.Position, &segment.JPKey, &segment.SourceText, &segment.SourceHash); err != nil {
			return nil, err
		}
		if isDeterministicEventSegment(segment) {
			result[segment.SegmentID] = segment
		}
	}
	return result, rows.Err()
}

// OrderedDetail is an order-preserving read-back of one event story. Episode
// and talk-line ordering reflect stored positions (i.e. dialogue flow), which
// Go's map-based marshaling would otherwise scramble.
type OrderedDetail struct {
	Meta     model.EventStoryMeta
	Episodes []OrderedEpisode
}

// OrderedDetail loads one event story with episodes and lines in stored order.
func (s *EventStore) OrderedDetail(eventID int) (OrderedDetail, error) {
	var od OrderedDetail
	err := s.db.QueryRow(
		`SELECT source, version, last_updated FROM event_stories WHERE event_id = ?`,
		eventID).Scan(&od.Meta.Source, &od.Meta.Version, &od.Meta.LastUpdated)
	if err == sql.ErrNoRows {
		return od, sql.ErrNoRows
	}
	if err != nil {
		return od, err
	}

	epRows, err := s.db.Query(
		`SELECT episode_no, scenario_id, title, title_source
		 FROM event_story_episodes WHERE event_id = ? ORDER BY position`, eventID)
	if err != nil {
		return od, err
	}
	defer epRows.Close()
	index := map[string]int{}
	for epRows.Next() {
		var ep OrderedEpisode
		if err := epRows.Scan(&ep.EpisodeNo, &ep.ScenarioID, &ep.Title, &ep.TitleSource); err != nil {
			return od, err
		}
		ep.TalkData = map[string]string{}
		ep.TalkSources = map[string]string{}
		ep.SpeakerNames = map[string]string{}
		index[ep.EpisodeNo] = len(od.Episodes)
		od.Episodes = append(od.Episodes, ep)
	}
	if err := epRows.Err(); err != nil {
		return od, err
	}

	lineRows, err := s.db.Query(
		`SELECT episode_no, jp_key, cn_text, source, speaker_name
		 FROM event_story_lines WHERE event_id = ? ORDER BY episode_no, position`, eventID)
	if err != nil {
		return od, err
	}
	defer lineRows.Close()
	for lineRows.Next() {
		var no, jp, cn, src, speaker string
		if err := lineRows.Scan(&no, &jp, &cn, &src, &speaker); err != nil {
			return od, err
		}
		i, ok := index[no]
		if !ok {
			continue
		}
		ep := &od.Episodes[i]
		ep.TalkKeys = append(ep.TalkKeys, jp)
		ep.TalkData[jp] = cn
		ep.TalkSources[jp] = src
		if speaker != "" {
			ep.SpeakerNames[jp] = speaker
		}
	}
	return od, lineRows.Err()
}

// Detail reconstructs the full event story detail for the console API. talkOrder
// is populated from stored line positions so the editor renders dialogue in
// story order (Go map marshaling alone would sort keys alphabetically).
func (s *EventStore) Detail(eventID int) (model.EventStoryDetail, error) {
	od, err := s.OrderedDetail(eventID)
	if err != nil {
		return model.EventStoryDetail{}, err
	}
	detail := model.EventStoryDetail{
		Meta:     od.Meta,
		Episodes: make(map[string]model.EventStoryEpisode, len(od.Episodes)),
	}
	for _, ep := range od.Episodes {
		e := model.EventStoryEpisode{
			ScenarioID:   ep.ScenarioID,
			Title:        ep.Title,
			TitleSource:  ep.TitleSource,
			TalkData:     ep.TalkData,
			TalkSources:  ep.TalkSources,
			TalkOrder:    ep.TalkKeys,
			SpeakerNames: ep.SpeakerNames,
		}
		if len(e.TalkSources) == 0 {
			e.TalkSources = nil
		}
		if len(e.SpeakerNames) == 0 {
			e.SpeakerNames = nil
		}
		detail.Episodes[ep.EpisodeNo] = e
	}
	return detail, nil
}

type eventDisplayName struct {
	Japanese  string
	Localized string
}

func (s *EventStore) eventDisplayNames(locale string) (map[int]eventDisplayName, error) {
	rows, err := s.db.Query(`SELECT e.jp_key, e.cn_text, e.ids_json, COALESCE(l.text, '')
		FROM entries e
		LEFT JOIN entry_localizations l
		  ON l.category=e.category AND l.field=e.field AND l.jp_key=e.jp_key AND l.locale=?
		WHERE e.category='events' AND e.field='name'`, locale)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int]eventDisplayName{}
	for rows.Next() {
		var japanese, chinese, idsJSON, localized string
		if err := rows.Scan(&japanese, &chinese, &idsJSON, &localized); err != nil {
			return nil, err
		}
		var rawIDs []any
		if idsJSON != "" {
			if err := json.Unmarshal([]byte(idsJSON), &rawIDs); err != nil {
				continue
			}
		}
		name := localized
		if locale == model.LocaleJapanese {
			name = japanese
		} else if name == "" {
			name = chinese
		}
		for _, rawID := range rawIDs {
			var id int
			switch value := rawID.(type) {
			case string:
				id, _ = strconv.Atoi(value)
			case float64:
				id = int(value)
			}
			if id > 0 {
				result[id] = eventDisplayName{Japanese: japanese, Localized: name}
			}
		}
	}
	return result, rows.Err()
}

func (s *EventStore) eventOfficialTagState(locale string) (map[int]bool, error) {
	// The "cn" source tag means official Simplified Chinese localization. It
	// must not hide the same story from independent English translation or the
	// Japanese read-only source view.
	if locale != model.LocaleChinese {
		return map[int]bool{}, nil
	}

	// 1. Get episode counts per event in a single grouped query
	epRows, err := s.db.Query(`SELECT event_id, COUNT(*) FROM event_story_episodes GROUP BY event_id`)
	if err != nil {
		return nil, err
	}
	defer epRows.Close()
	episodeCounts := make(map[int]int)
	for epRows.Next() {
		var eventID, count int
		if err := epRows.Scan(&eventID, &count); err != nil {
			return nil, err
		}
		episodeCounts[eventID] = count
	}
	if err := epRows.Err(); err != nil {
		return nil, err
	}

	// 2. Query all event_stories
	storyRows, err := s.db.Query(`SELECT event_id FROM event_stories ORDER BY event_id`)
	if err != nil {
		return nil, err
	}
	defer storyRows.Close()
	var eventIDs []int
	for storyRows.Next() {
		var eventID int
		if err := storyRows.Scan(&eventID); err != nil {
			return nil, err
		}
		eventIDs = append(eventIDs, eventID)
	}
	if err := storyRows.Err(); err != nil {
		return nil, err
	}

	// 3. For all events, collect canonical segment IDs
	type eventCanonicalInfo struct {
		segmentIDs map[string]bool
		valid      bool
	}
	canonicalInfo := make(map[int]eventCanonicalInfo, len(eventIDs))
	for _, eventID := range eventIDs {
		epCount := episodeCounts[eventID]
		canonical, err := currentEventCanonicalSegmentIDs(s.db, eventID)
		if err != nil || len(canonical) != epCount {
			canonicalInfo[eventID] = eventCanonicalInfo{valid: false}
			continue
		}
		ids := make(map[string]bool)
		for _, segIDs := range canonical {
			for id := range segIDs {
				ids[id] = true
			}
		}
		if len(ids) == 0 {
			canonicalInfo[eventID] = eventCanonicalInfo{valid: false}
			continue
		}
		canonicalInfo[eventID] = eventCanonicalInfo{segmentIDs: ids, valid: true}
	}

	// 4. In a single bulk query, fetch all segments and their localization source
	segRows, err := s.db.Query(`SELECT seg.event_id, seg.segment_id, seg.kind, seg.source_text, ep.title_source, localization.source
		FROM event_story_segments seg
		JOIN event_story_episodes ep
		  ON ep.event_id=seg.event_id AND ep.episode_no=seg.episode_no AND ep.scenario_id=seg.scenario_id
		LEFT JOIN event_story_segment_localizations localization
		  ON localization.segment_id=seg.segment_id AND localization.locale=?
		ORDER BY seg.event_id`, model.LocaleChinese)
	if err != nil {
		return nil, err
	}
	defer segRows.Close()

	type eventAgg struct {
		matched    int
		considered int
		official   int
	}
	aggs := make(map[int]*eventAgg, len(eventIDs))
	for segRows.Next() {
		var eventID int
		var segmentID, kind, sourceText string
		var titleSource, localizedSource sql.NullString
		if err := segRows.Scan(&eventID, &segmentID, &kind, &sourceText, &titleSource, &localizedSource); err != nil {
			return nil, err
		}
		info := canonicalInfo[eventID]
		if !info.valid || !info.segmentIDs[segmentID] {
			continue
		}
		agg := aggs[eventID]
		if agg == nil {
			agg = &eventAgg{}
			aggs[eventID] = agg
		}
		agg.matched++
		if strings.TrimSpace(sourceText) == "" {
			continue
		}
		agg.considered++
		source := "unknown"
		if kind == "title" && titleSource.Valid {
			source = titleSource.String
		} else if kind != "title" && localizedSource.Valid {
			source = localizedSource.String
		}
		if source == model.SourceCN {
			agg.official++
		}
	}
	if err := segRows.Err(); err != nil {
		return nil, err
	}

	result := make(map[int]bool, len(eventIDs))
	for _, eventID := range eventIDs {
		info := canonicalInfo[eventID]
		if !info.valid {
			result[eventID] = false
			continue
		}
		agg := aggs[eventID]
		if agg == nil {
			result[eventID] = false
			continue
		}
		result[eventID] = agg.matched == len(info.segmentIDs) && agg.considered > 0 && agg.considered == agg.official
	}
	return result, nil
}

// List returns summaries of all event stories, ordered by event id. The
// untranslated count mirrors UntranslatedTargets: talk lines with empty cn_text
// plus jp-pending titles (non-empty title text whose source is unset/unknown).
func (s *EventStore) List() ([]model.EventStorySummary, error) {
	return s.ListLocale(model.LocaleChinese)
}

func (s *EventStore) listSummaries(locale string) ([]model.EventStorySummary, error) {
	names, err := s.eventDisplayNames(locale)
	if err != nil {
		return nil, err
	}
	official, err := s.eventOfficialTagState(locale)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT es.event_id, es.source,
		(SELECT COUNT(*) FROM event_story_episodes e WHERE e.event_id = es.event_id),
		CASE WHEN ?='ja-JP' THEN
			(SELECT COUNT(*) FROM event_story_segments seg
				JOIN event_story_episodes episode
				 ON episode.event_id=seg.event_id AND episode.episode_no=seg.episode_no AND episode.scenario_id=seg.scenario_id
				WHERE seg.event_id=es.event_id AND seg.source_text='')
		WHEN ?='zh-CN' THEN
			(SELECT COUNT(*) FROM event_story_lines l
				WHERE l.event_id = es.event_id AND l.cn_text = '')
				+ (SELECT COUNT(*) FROM event_story_episodes e
					WHERE e.event_id = es.event_id AND e.title <> ''
					  AND (e.title_source = '' OR e.title_source = 'unknown' OR e.title_source = 'jp_pending'))
		ELSE
			(SELECT COUNT(*) FROM event_story_segments seg
				JOIN event_story_episodes episode
				 ON episode.event_id=seg.event_id AND episode.episode_no=seg.episode_no AND episode.scenario_id=seg.scenario_id
				LEFT JOIN event_story_segment_localizations loc ON loc.segment_id=seg.segment_id AND loc.locale=?
				WHERE seg.event_id=es.event_id AND (loc.segment_id IS NULL OR loc.text=''))
		END,
		COALESCE((SELECT last_updated FROM event_story_locale_meta lm WHERE lm.event_id=es.event_id AND lm.locale=?), es.last_updated),
		es.last_updated
		FROM event_stories es ORDER BY es.event_id`, locale, locale, locale, locale)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.EventStorySummary
	for rows.Next() {
		var sum model.EventStorySummary
		var legacyLastUpdated int64
		if err := rows.Scan(&sum.EventID, &sum.Source, &sum.EpisodeCount, &sum.UntranslatedCount, &sum.LastUpdated, &legacyLastUpdated); err != nil {
			return nil, err
		}
		if name, ok := names[sum.EventID]; ok {
			sum.EventName = name.Localized
			sum.EventNameJapanese = name.Japanese
		}
		sum.AllOfficialTagged = official[sum.EventID]
		out = append(out, sum)
	}
	return out, rows.Err()
}

// UpdateLine sets the cn text and source of one talk line (entryType "talk")
// or an episode title (entryType "title"). For titles, jpKey is ignored. The
// story's last_updated is bumped. Returns ErrNoRows if the target is missing.
func (s *EventStore) UpdateLine(eventID int, episodeNo, jpKey, cnText, source, entryType string) error {
	if !model.IsValidSource(source) {
		return fmt.Errorf("invalid translation source: %q", source)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var res sql.Result
	if entryType == "title" {
		res, err = tx.Exec(
			`UPDATE event_story_episodes SET title = ?, title_source = ?
			 WHERE event_id = ? AND episode_no = ?`,
			cnText, source, eventID, episodeNo)
	} else {
		res, err = tx.Exec(
			`UPDATE event_story_lines SET cn_text = ?, source = ?
			 WHERE event_id = ? AND episode_no = ? AND jp_key = ?`,
			cnText, source, eventID, episodeNo, jpKey)
	}
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`UPDATE event_stories SET last_updated = ? WHERE event_id = ?`, now, eventID); err != nil {
		return err
	}
	if err := touchEventLocaleMetaTx(tx, eventID, now); err != nil {
		return err
	}
	// Keep the additive zh-CN projection synchronized with the legacy row.
	kind := "talk"
	if entryType == "title" {
		kind = "title"
		jpKey = ""
	}
	if _, err := tx.Exec(`INSERT INTO event_story_segment_localizations
		(segment_id, locale, text, source, updated_at, updated_by, revision)
		SELECT segment.segment_id, ?, ?, ?, ?, 'legacy-api', 1 FROM event_story_segments segment
		JOIN event_story_episodes episode
		ON episode.event_id=segment.event_id AND episode.episode_no=segment.episode_no
		AND episode.scenario_id=segment.scenario_id
		WHERE segment.event_id=? AND segment.episode_no=? AND segment.kind=? AND (?='' OR segment.jp_key=?)
		ON CONFLICT(segment_id, locale) DO UPDATE SET text=excluded.text, source=excluded.source,
		updated_at=excluded.updated_at, updated_by=excluded.updated_by,
		revision=event_story_segment_localizations.revision+1`,
		model.LocaleChinese, cnText, source, now, eventID, episodeNo, kind, jpKey, jpKey); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateSummaryCache()
	return nil
}

// PromoteHuman marks every title and talk line of an event story as human.
func (s *EventStore) PromoteHuman(eventID int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`UPDATE event_story_episodes SET title_source = 'human' WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE event_story_lines SET source = 'human' WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE event_story_segment_localizations
		SET source='human', updated_at=?, updated_by='promote-human', revision=revision+1
		WHERE locale=? AND segment_id IN (
			SELECT segment.segment_id FROM event_story_segments segment
			JOIN event_story_episodes episode
			ON episode.event_id=segment.event_id AND episode.episode_no=segment.episode_no
			AND episode.scenario_id=segment.scenario_id
			WHERE segment.event_id=?)`,
		now, model.LocaleChinese, eventID); err != nil {
		return err
	}
	res, err := tx.Exec(
		`UPDATE event_stories SET source = 'human', last_updated = ? WHERE event_id = ?`,
		now, eventID)
	if err != nil {
		return err
	}
	// The other statements target child rows and legitimately match none; this
	// one is the story itself, so no row means the story does not exist.
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if err := touchEventLocaleMetaTx(tx, eventID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateSummaryCache()
	return nil
}

// touchEventLocaleMetaTx bumps the zh-CN locale metadata timestamp. The summary
// reads it in preference to event_stories.last_updated, so a human edit that
// only bumped the legacy row would otherwise report the last AI run.
func touchEventLocaleMetaTx(tx *sql.Tx, eventID int, now int64) error {
	_, err := tx.Exec(`INSERT INTO event_story_locale_meta(event_id, locale, last_updated) VALUES (?, ?, ?)
		ON CONFLICT(event_id, locale) DO UPDATE SET last_updated=excluded.last_updated`,
		eventID, model.LocaleChinese, now)
	return err
}

// Exists reports whether an event story is present.
func (s *EventStore) Exists(eventID int) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM event_stories WHERE event_id = ?`, eventID).Scan(&n)
	return n > 0, err
}

func cloneEventStorySummaries(src []model.EventStorySummary) []model.EventStorySummary {
	if src == nil {
		return nil
	}
	dst := make([]model.EventStorySummary, len(src))
	copy(dst, src)
	return dst
}

func eventSegmentID(eventID int, scenarioID, episodeNo, kind string, position int, field ...string) string {
	id := fmt.Sprintf("event:%d:%s:%s:%s:%d", eventID, scenarioID, episodeNo, kind, position)
	if len(field) > 0 && field[0] != "" {
		id += ":" + field[0]
	}
	return id
}

func hashText(text string) string {
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
