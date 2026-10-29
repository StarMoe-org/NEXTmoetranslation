package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"moesekai/server/internal/model"
)

const (
	EventScenarioParserVersion     = 1
	maxValidatedScenarioCacheItems = 4096
)

type scenarioValidationKey struct {
	sha256     [32]byte
	scenarioID string
}

var (
	sha256HasherPool = sync.Pool{
		New: func() any { return sha256.New() },
	}
	validatedScenarioMu    sync.RWMutex
	validatedScenarioCache = make(map[scenarioValidationKey]struct{})
)

func sha256StringSum(value string) [32]byte {
	hasher := sha256HasherPool.Get().(hash.Hash)
	hasher.Reset()
	_, _ = io.WriteString(hasher, value)
	var sum [32]byte
	_ = hasher.Sum(sum[:0])
	sha256HasherPool.Put(hasher)
	return sum
}

var (
	ErrEventScenarioConflict = errors.New("event scenario identity conflict")
	ErrEventScenarioInvalid  = errors.New("event scenario is invalid")
)

type EventScenarioRecord struct {
	EventID       int    `json:"eventId"`
	EpisodeNo     string `json:"episodeNo"`
	ScenarioID    string `json:"scenarioId"`
	CanonicalJSON string `json:"canonicalJson"`
	SHA256        string `json:"sha256"`
}

type EventEpisodeIdentity struct {
	EventID    int
	EpisodeNo  string
	ScenarioID string
}

type EventSourceTalk struct {
	Speaker       string   `json:"speaker"`
	Text          string   `json:"text"`
	Voices        []string `json:"voices,omitempty"`
	Volume        []int    `json:"volume,omitempty"`
	CharIndex     int      `json:"charIndex"`
	Chara2D       int      `json:"chara2d,omitempty"`
	TalkDataIndex *int     `json:"talkDataIndex,omitempty"`
}

type EventEpisodeScenarioSnapshot struct {
	ScenarioID    string            `json:"scenarioId"`
	FileName      string            `json:"fileName"`
	SHA256        string            `json:"sha256"`
	ParserVersion int               `json:"parserVersion"`
	RawJSON       string            `json:"rawJson"`
	SourceTalks   []EventSourceTalk `json:"sourceTalks"`
}

type EventEpisodeSnapshot struct {
	EventID   int                          `json:"eventId"`
	EpisodeNo string                       `json:"episodeNo"`
	Locale    string                       `json:"locale"`
	Revision  string                       `json:"revision"`
	Segments  []model.EventStorySegment    `json:"segments"`
	Scenario  EventEpisodeScenarioSnapshot `json:"scenario"`
}

type canonicalEventScenario struct {
	ScenarioID        string `json:"ScenarioId"`
	Snippets          any    `json:"Snippets"`
	TalkData          any    `json:"TalkData"`
	SpecialEffectData any    `json:"SpecialEffectData"`
	AppearCharacters  any    `json:"AppearCharacters"`
}

// CanonicalizeEventScenario retains the complete five public Unity scenario
// fields while producing deterministic compact JSON and its SHA-256.
func CanonicalizeEventScenario(value any, expectedScenarioID string) (string, string, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("%w: root must be an object", ErrEventScenarioInvalid)
	}
	scenarioID, ok := object["ScenarioId"].(string)
	if !ok || scenarioID == "" {
		return "", "", fmt.Errorf("%w: ScenarioId required", ErrEventScenarioInvalid)
	}
	if scenarioID != expectedScenarioID {
		return "", "", fmt.Errorf("%w: ScenarioId mismatch", ErrEventScenarioConflict)
	}
	fields := make([]any, 4)
	for index, name := range []string{"Snippets", "TalkData", "SpecialEffectData", "AppearCharacters"} {
		array, ok := object[name].([]any)
		if !ok {
			return "", "", fmt.Errorf("%w: %s must be an array", ErrEventScenarioInvalid, name)
		}
		fields[index] = array
	}
	document := canonicalEventScenario{
		ScenarioID: scenarioID, Snippets: fields[0], TalkData: fields[1],
		SpecialEffectData: fields[2], AppearCharacters: fields[3],
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", "", fmt.Errorf("%w: encode: %v", ErrEventScenarioInvalid, err)
	}
	sum := sha256.Sum256(encoded)
	return string(encoded), hex.EncodeToString(sum[:]), nil
}

func ValidateEventScenarioRecord(record EventScenarioRecord) error {
	if record.EventID <= 0 || strings.TrimSpace(record.EpisodeNo) == "" || strings.TrimSpace(record.ScenarioID) == "" {
		return fmt.Errorf("%w: incomplete identity", ErrEventScenarioInvalid)
	}
	hash := sha256StringSum(record.CanonicalJSON)
	if hex.EncodeToString(hash[:]) != record.SHA256 {
		return fmt.Errorf("%w: sha256 mismatch", ErrEventScenarioInvalid)
	}
	key := scenarioValidationKey{sha256: hash, scenarioID: record.ScenarioID}
	validatedScenarioMu.RLock()
	_, ok := validatedScenarioCache[key]
	validatedScenarioMu.RUnlock()
	if ok {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(record.CanonicalJSON))
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrEventScenarioInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrEventScenarioInvalid)
	}
	canonical, digest, err := CanonicalizeEventScenario(decoded, record.ScenarioID)
	if err != nil {
		return err
	}
	if canonical != record.CanonicalJSON || digest != record.SHA256 {
		return fmt.Errorf("%w: non-canonical JSON", ErrEventScenarioInvalid)
	}
	validatedScenarioMu.Lock()
	if len(validatedScenarioCache) >= maxValidatedScenarioCacheItems {
		validatedScenarioCache = make(map[scenarioValidationKey]struct{})
	}
	validatedScenarioCache[key] = struct{}{}
	validatedScenarioMu.Unlock()
	return nil
}

func validateOrderedScenarioSet(eventID int, episodes []OrderedEpisode) (bool, error) {
	present := 0
	for _, episode := range episodes {
		if episode.ScenarioCanonicalJSON != "" || episode.ScenarioSHA256 != "" {
			present++
		}
	}
	// Legacy imports have no raw scenario payload and retain their exact behavior.
	if present == 0 {
		return false, nil
	}
	seenEpisodes := map[int]bool{}
	seenScenarios := map[string]bool{}
	for _, episode := range episodes {
		episodeNo, err := strconv.Atoi(episode.EpisodeNo)
		if err != nil || episodeNo <= 0 || strings.TrimSpace(episode.ScenarioID) == "" {
			return false, fmt.Errorf("%w: incomplete ordered episode identity", ErrEventScenarioInvalid)
		}
		if seenEpisodes[episodeNo] || seenScenarios[episode.ScenarioID] {
			return false, fmt.Errorf("%w: duplicate ordered episode identity", ErrEventScenarioConflict)
		}
		seenEpisodes[episodeNo] = true
		seenScenarios[episode.ScenarioID] = true
	}
	if present != len(episodes) {
		return false, fmt.Errorf("%w: partial ordered scenario set", ErrEventScenarioInvalid)
	}
	for _, episode := range episodes {
		if err := ValidateEventScenarioRecord(EventScenarioRecord{
			EventID: eventID, EpisodeNo: episode.EpisodeNo, ScenarioID: episode.ScenarioID,
			CanonicalJSON: episode.ScenarioCanonicalJSON, SHA256: episode.ScenarioSHA256,
		}); err != nil {
			return false, fmt.Errorf("event %d episode %s: %w", eventID, episode.EpisodeNo, err)
		}
	}
	return true, nil
}

func replaceEventScenariosTx(tx *sql.Tx, eventID int, episodes []OrderedEpisode) error {
	// Clear old identities first so scenario IDs can legally move or swap
	// between episodes without tripping the per-event unique constraint.
	if _, err := tx.Exec(`DELETE FROM event_story_scenarios WHERE event_id=?`, eventID); err != nil {
		return err
	}
	for _, episode := range episodes {
		if _, err := tx.Exec(`INSERT INTO event_story_scenarios(event_id, episode_no, scenario_id, canonical_json, sha256)
			VALUES (?, ?, ?, ?, ?)`,
			eventID, episode.EpisodeNo, episode.ScenarioID, episode.ScenarioCanonicalJSON, episode.ScenarioSHA256); err != nil {
			return err
		}
	}
	return nil
}

func (s *EventStore) MissingScenarioEpisodes(eventID int) ([]EventEpisodeIdentity, error) {
	rows, err := s.db.Query(`SELECT ep.event_id, ep.episode_no, ep.scenario_id,
		scenario.scenario_id, scenario.canonical_json, scenario.sha256
		FROM event_story_episodes ep
		LEFT JOIN event_story_scenarios scenario
		ON scenario.event_id=ep.event_id AND scenario.episode_no=ep.episode_no
		WHERE ep.event_id=?
		ORDER BY ep.position`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type episodeState struct {
		identity EventEpisodeIdentity
		record   EventScenarioRecord
		valid    bool
	}
	var episodes []episodeState
	for rows.Next() {
		var identity EventEpisodeIdentity
		var sideScenarioID, canonicalJSON, digest sql.NullString
		if err := rows.Scan(&identity.EventID, &identity.EpisodeNo, &identity.ScenarioID,
			&sideScenarioID, &canonicalJSON, &digest); err != nil {
			return nil, err
		}
		record := EventScenarioRecord{
			EventID: identity.EventID, EpisodeNo: identity.EpisodeNo, ScenarioID: sideScenarioID.String,
			CanonicalJSON: canonicalJSON.String, SHA256: digest.String,
		}
		episodes = append(episodes, episodeState{identity: identity, record: record,
			valid: sideScenarioID.Valid && sideScenarioID.String == identity.ScenarioID && ValidateEventScenarioRecord(record) == nil})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var result []EventEpisodeIdentity
	for _, episode := range episodes {
		if episode.valid {
			covered, err := eventEpisodeHasCanonicalSegments(s.db, episode.record)
			if err != nil {
				return nil, err
			}
			episode.valid = covered
		}
		if !episode.valid {
			result = append(result, episode.identity)
		}
	}
	return result, nil
}

// BackfillScenarios updates only side-table rows after validating every parent
// identity. It never modifies event translations or source metadata.
func (s *EventStore) BackfillScenarios(eventID int, episodes []OrderedEpisode) error {
	complete, err := validateOrderedScenarioSet(eventID, episodes)
	if err != nil {
		return err
	}
	if !complete || len(episodes) == 0 {
		return fmt.Errorf("%w: empty scenario backfill", ErrEventScenarioInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, episode := range episodes {
		var parentScenarioID string
		if err := tx.QueryRow(`SELECT scenario_id FROM event_story_episodes WHERE event_id=? AND episode_no=?`,
			eventID, episode.EpisodeNo).Scan(&parentScenarioID); err == sql.ErrNoRows {
			return ErrEventScenarioConflict
		} else if err != nil {
			return err
		}
		if parentScenarioID != episode.ScenarioID {
			return ErrEventScenarioConflict
		}
	}
	for _, episode := range episodes {
		if err := reconcileEventScenarioSegmentsTx(tx, eventID, episode); err != nil {
			return err
		}
	}
	for _, episode := range episodes {
		if _, err := tx.Exec(`DELETE FROM event_story_scenarios WHERE event_id=? AND episode_no=?`, eventID, episode.EpisodeNo); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM event_story_scenarios AS scenario
		WHERE scenario.event_id=? AND NOT EXISTS (
			SELECT 1 FROM event_story_episodes episode
			WHERE episode.event_id=scenario.event_id AND episode.episode_no=scenario.episode_no
			AND episode.scenario_id=scenario.scenario_id
		)`, eventID); err != nil {
		return err
	}
	for _, episode := range episodes {
		if _, err := tx.Exec(`INSERT INTO event_story_scenarios(event_id, episode_no, scenario_id, canonical_json, sha256)
			VALUES (?, ?, ?, ?, ?)`,
			eventID, episode.EpisodeNo, episode.ScenarioID, episode.ScenarioCanonicalJSON, episode.ScenarioSHA256); err != nil {
			return err
		}
	}
	for _, episode := range episodes {
		covered, err := eventEpisodeHasCanonicalSegments(tx, EventScenarioRecord{
			EventID: eventID, EpisodeNo: episode.EpisodeNo, ScenarioID: episode.ScenarioID,
			CanonicalJSON: episode.ScenarioCanonicalJSON, SHA256: episode.ScenarioSHA256,
		})
		if err != nil {
			return err
		}
		if !covered {
			return ErrEventScenarioConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateSummaryCache()
	return nil
}

type eventSegmentQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func currentEventCanonicalSegmentIDs(queryer eventSegmentQueryer, eventID int) (map[string]map[string]bool, error) {
	rows, err := queryer.Query(`SELECT scenario.event_id, scenario.episode_no, scenario.scenario_id,
		scenario.canonical_json, scenario.sha256
		FROM event_story_scenarios scenario
		JOIN event_story_episodes episode
		ON episode.event_id=scenario.event_id AND episode.episode_no=scenario.episode_no
		AND episode.scenario_id=scenario.scenario_id
		WHERE scenario.event_id=?`, eventID)
	if err != nil {
		return nil, err
	}
	result := map[string]map[string]bool{}
	var records []EventScenarioRecord
	for rows.Next() {
		var record EventScenarioRecord
		if err := rows.Scan(&record.EventID, &record.EpisodeNo, &record.ScenarioID, &record.CanonicalJSON, &record.SHA256); err != nil {
			rows.Close()
			return nil, err
		}
		if err := ValidateEventScenarioRecord(record); err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return result, nil
	}
	type epScenarioKey struct {
		episodeNo  string
		scenarioID string
	}
	storedByEpisode := make(map[epScenarioKey]map[string]EventSegmentRecord, len(records))
	segRows, err := queryer.Query(`SELECT segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash
		FROM event_story_segments WHERE event_id=?`, eventID)
	if err != nil {
		return nil, err
	}
	for segRows.Next() {
		var segment EventSegmentRecord
		if err := segRows.Scan(&segment.SegmentID, &segment.EventID, &segment.EpisodeNo, &segment.ScenarioID,
			&segment.Kind, &segment.Position, &segment.JPKey, &segment.SourceText, &segment.SourceHash); err != nil {
			segRows.Close()
			return nil, err
		}
		key := epScenarioKey{episodeNo: segment.EpisodeNo, scenarioID: segment.ScenarioID}
		byID := storedByEpisode[key]
		if byID == nil {
			byID = make(map[string]EventSegmentRecord)
			storedByEpisode[key] = byID
		}
		byID[segment.SegmentID] = segment
	}
	if err := segRows.Err(); err != nil {
		segRows.Close()
		return nil, err
	}
	if err := segRows.Close(); err != nil {
		return nil, err
	}
	for _, record := range records {
		definitions, err := eventScenarioSegmentDefinitions(record, "")
		if err != nil {
			return nil, err
		}
		stored := storedByEpisode[epScenarioKey{episodeNo: record.EpisodeNo, scenarioID: record.ScenarioID}]
		segments, covered := verifyCanonicalSegmentsFromStored(definitions, stored)
		if !covered {
			return nil, ErrEventScenarioConflict
		}
		ids := make(map[string]bool, len(segments))
		for segmentID := range segments {
			ids[segmentID] = true
		}
		result[record.EpisodeNo] = ids
	}
	return result, nil
}

func eventScenarioSegmentDefinitions(record EventScenarioRecord, titleSource string) ([]EventSegmentRecord, error) {
	var scenario parsedScenario
	if err := json.NewDecoder(strings.NewReader(record.CanonicalJSON)).Decode(&scenario); err != nil {
		return nil, fmt.Errorf("%w: parse segment definitions", ErrEventScenarioInvalid)
	}
	definitions := make([]EventSegmentRecord, 0, 1+len(scenario.TalkData)*2)
	definitions = append(definitions, EventSegmentRecord{
		SegmentID: eventSegmentID(record.EventID, record.ScenarioID, record.EpisodeNo, "title", -1),
		EventID:   record.EventID, EpisodeNo: record.EpisodeNo, ScenarioID: record.ScenarioID,
		Kind: "title", Position: -1, SourceText: titleSource, SourceHash: hashText(titleSource),
	})
	for position, talk := range scenario.TalkData {
		for _, field := range []struct {
			name string
			text string
			pos  int
		}{
			{name: "body", text: strings.TrimSpace(talk.Body), pos: position * 2},
			{name: "speaker", text: strings.TrimSpace(talk.WindowDisplayName), pos: position*2 + 1},
		} {
			definitions = append(definitions, EventSegmentRecord{
				SegmentID: eventSegmentID(record.EventID, record.ScenarioID, record.EpisodeNo, "talk", field.pos, field.name),
				EventID:   record.EventID, EpisodeNo: record.EpisodeNo, ScenarioID: record.ScenarioID,
				Kind: "talk", Position: field.pos, JPKey: field.text, SourceText: field.text, SourceHash: hashText(field.text),
			})
		}
	}
	return definitions, nil
}

func eventSegmentMatchesDefinition(segment, definition EventSegmentRecord) bool {
	if segment.SegmentID != definition.SegmentID || segment.EventID != definition.EventID ||
		segment.EpisodeNo != definition.EpisodeNo || segment.ScenarioID != definition.ScenarioID ||
		segment.Kind != definition.Kind || segment.Position != definition.Position || segment.JPKey != definition.JPKey {
		return false
	}
	return segment.SourceText == definition.SourceText && segment.SourceHash == definition.SourceHash
}

func verifyCanonicalSegmentsFromStored(definitions []EventSegmentRecord, stored map[string]EventSegmentRecord) (map[string]EventSegmentRecord, bool) {
	canonical := make(map[string]EventSegmentRecord, len(definitions))
	for _, definition := range definitions {
		segment, ok := stored[definition.SegmentID]
		if !ok {
			return canonical, false
		}
		// The raw scenario does not contain the episode title. Its canonical row
		// is authoritative, but the stored text and hash must still agree.
		if definition.Kind == "title" {
			definition.SourceText = segment.SourceText
			definition.SourceHash = hashText(segment.SourceText)
		}
		if !eventSegmentMatchesDefinition(segment, definition) {
			return canonical, false
		}
		canonical[definition.SegmentID] = definition
	}
	return canonical, len(canonical) == len(definitions)
}

func eventEpisodeCanonicalSegments(queryer eventSegmentQueryer, record EventScenarioRecord) (map[string]EventSegmentRecord, bool, error) {
	definitions, err := eventScenarioSegmentDefinitions(record, "")
	if err != nil {
		return nil, false, err
	}
	rows, err := queryer.Query(`SELECT segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash
		FROM event_story_segments WHERE event_id=? AND episode_no=? AND scenario_id=?`,
		record.EventID, record.EpisodeNo, record.ScenarioID)
	if err != nil {
		return nil, false, err
	}
	stored := make(map[string]EventSegmentRecord, len(definitions))
	for rows.Next() {
		var segment EventSegmentRecord
		if err := rows.Scan(&segment.SegmentID, &segment.EventID, &segment.EpisodeNo, &segment.ScenarioID,
			&segment.Kind, &segment.Position, &segment.JPKey, &segment.SourceText, &segment.SourceHash); err != nil {
			rows.Close()
			return nil, false, err
		}
		stored[segment.SegmentID] = segment
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	canonical, covered := verifyCanonicalSegmentsFromStored(definitions, stored)
	return canonical, covered, nil
}

func eventEpisodeHasCanonicalSegments(queryer eventSegmentQueryer, record EventScenarioRecord) (bool, error) {
	_, covered, err := eventEpisodeCanonicalSegments(queryer, record)
	return covered, err
}

func reconcileEventScenarioSegmentsTx(tx *sql.Tx, eventID int, episode OrderedEpisode) error {
	record := EventScenarioRecord{
		EventID: eventID, EpisodeNo: episode.EpisodeNo, ScenarioID: episode.ScenarioID,
		CanonicalJSON: episode.ScenarioCanonicalJSON, SHA256: episode.ScenarioSHA256,
	}
	definitions, err := eventScenarioSegmentDefinitions(record, strings.TrimSpace(episode.SourceTitle))
	if err != nil {
		return err
	}
	canonicalIDs := make(map[string]bool, len(definitions))
	type sourceIdentity struct{ kind, sourceHash string }
	canonicalCounts := make(map[sourceIdentity]int, len(definitions))
	for _, definition := range definitions {
		canonicalIDs[definition.SegmentID] = true
		canonicalCounts[sourceIdentity{kind: definition.Kind, sourceHash: definition.SourceHash}]++
		var stored EventSegmentRecord
		err := tx.QueryRow(`SELECT segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash
			FROM event_story_segments WHERE segment_id=?`, definition.SegmentID).Scan(&stored.SegmentID, &stored.EventID,
			&stored.EpisodeNo, &stored.ScenarioID, &stored.Kind, &stored.Position, &stored.JPKey,
			&stored.SourceText, &stored.SourceHash)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && !eventSegmentMatchesDefinition(stored, definition) {
			if err := archiveEventSegmentTx(tx, definition.SegmentID); err != nil {
				return err
			}
			err = sql.ErrNoRows
		}
		if err == sql.ErrNoRows {
			if _, err := tx.Exec(`INSERT INTO event_story_segments
			(segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, definition.SegmentID, definition.EventID, definition.EpisodeNo,
				definition.ScenarioID, definition.Kind, definition.Position, definition.JPKey, definition.SourceText, definition.SourceHash); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(`SELECT segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash
			FROM event_story_segments WHERE segment_id=?`, definition.SegmentID).Scan(&stored.SegmentID, &stored.EventID,
			&stored.EpisodeNo, &stored.ScenarioID, &stored.Kind, &stored.Position, &stored.JPKey,
			&stored.SourceText, &stored.SourceHash); err != nil {
			return err
		}
		if !eventSegmentMatchesDefinition(stored, definition) {
			return ErrEventScenarioConflict
		}
	}
	for _, definition := range definitions {
		identity := sourceIdentity{kind: definition.Kind, sourceHash: definition.SourceHash}
		if definition.SourceHash == "" || canonicalCounts[identity] != 1 {
			continue
		}
		rows, err := tx.Query(`SELECT segment_id FROM event_story_segments
			WHERE event_id=? AND episode_no=? AND kind=? AND source_hash=? AND segment_id<>?`,
			eventID, episode.EpisodeNo, definition.Kind, definition.SourceHash, definition.SegmentID)
		if err != nil {
			return err
		}
		var candidates []string
		for rows.Next() {
			var segmentID string
			if err := rows.Scan(&segmentID); err != nil {
				rows.Close()
				return err
			}
			if !canonicalIDs[segmentID] {
				candidates = append(candidates, segmentID)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(candidates) != 1 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO event_story_segment_localizations
			(segment_id, locale, text, source, updated_at, updated_by, revision)
			SELECT ?, locale, text, source, updated_at, updated_by, revision
			FROM event_story_segment_localizations WHERE segment_id=? AND source IN ('human','pinned')
			ON CONFLICT(segment_id, locale) DO UPDATE SET text=excluded.text, source=excluded.source,
			updated_at=excluded.updated_at, updated_by=excluded.updated_by, revision=excluded.revision
			WHERE event_story_segment_localizations.source NOT IN ('human','pinned')`,
			definition.SegmentID, candidates[0]); err != nil {
			return err
		}
	}
	return nil
}

func archiveEventSegmentTx(tx *sql.Tx, segmentID string) error {
	var segment EventSegmentRecord
	if err := tx.QueryRow(`SELECT segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash
		FROM event_story_segments WHERE segment_id=?`, segmentID).Scan(&segment.SegmentID, &segment.EventID,
		&segment.EpisodeNo, &segment.ScenarioID, &segment.Kind, &segment.Position, &segment.JPKey,
		&segment.SourceText, &segment.SourceHash); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	base := segmentID + ":recovery"
	recoveryID := base
	for suffix := 1; ; suffix++ {
		var exists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM event_story_segments WHERE segment_id=?)`, recoveryID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			break
		}
		recoveryID = fmt.Sprintf("%s:%d", base, suffix)
	}
	if _, err := tx.Exec(`INSERT INTO event_story_segments
		(segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, recoveryID, segment.EventID, segment.EpisodeNo, segment.ScenarioID,
		segment.Kind, segment.Position, segment.JPKey, segment.SourceText, segment.SourceHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO event_story_segment_localizations
		(segment_id, locale, text, source, updated_at, updated_by, revision)
		SELECT ?, locale, text, source, updated_at, updated_by, revision
		FROM event_story_segment_localizations WHERE segment_id=?`, recoveryID, segmentID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM event_story_segments WHERE segment_id=?`, segmentID)
	return err
}

func (s *EventStore) EpisodeSnapshot(eventID int, episodeNo, locale string) (EventEpisodeSnapshot, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return EventEpisodeSnapshot{}, err
	}
	defer tx.Rollback()
	var parentScenarioID, title, titleSource string
	if err := tx.QueryRow(`SELECT scenario_id, title, title_source FROM event_story_episodes
		WHERE event_id=? AND episode_no=?`, eventID, episodeNo).Scan(&parentScenarioID, &title, &titleSource); err != nil {
		return EventEpisodeSnapshot{}, err
	}
	var scenario EventScenarioRecord
	scenario.EventID, scenario.EpisodeNo = eventID, episodeNo
	if err := tx.QueryRow(`SELECT scenario_id, canonical_json, sha256 FROM event_story_scenarios
		WHERE event_id=? AND episode_no=?`, eventID, episodeNo).
		Scan(&scenario.ScenarioID, &scenario.CanonicalJSON, &scenario.SHA256); err != nil {
		return EventEpisodeSnapshot{}, err
	}
	if scenario.ScenarioID != parentScenarioID {
		return EventEpisodeSnapshot{}, ErrEventScenarioConflict
	}
	if err := ValidateEventScenarioRecord(scenario); err != nil {
		return EventEpisodeSnapshot{}, err
	}
	expectedSegments, covered, err := eventEpisodeCanonicalSegments(tx, scenario)
	if err != nil {
		return EventEpisodeSnapshot{}, err
	}
	if !covered {
		return EventEpisodeSnapshot{}, ErrEventScenarioConflict
	}
	sourceTalks, err := ParseEventSourceTalks(scenario.CanonicalJSON)
	if err != nil {
		return EventEpisodeSnapshot{}, err
	}
	legacyLines := map[string]model.Entry{}
	if locale == model.LocaleChinese {
		rows, err := tx.Query(`SELECT jp_key, cn_text, source FROM event_story_lines WHERE event_id=? AND episode_no=?`, eventID, episodeNo)
		if err != nil {
			return EventEpisodeSnapshot{}, err
		}
		for rows.Next() {
			var key string
			var entry model.Entry
			if err := rows.Scan(&key, &entry.Text, &entry.Source); err != nil {
				rows.Close()
				return EventEpisodeSnapshot{}, err
			}
			legacyLines[key] = entry
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return EventEpisodeSnapshot{}, err
		}
		if err := rows.Close(); err != nil {
			return EventEpisodeSnapshot{}, err
		}
	}
	rows, err := tx.Query(`SELECT seg.segment_id, seg.scenario_id, seg.kind, seg.position, seg.jp_key,
		seg.source_text, seg.source_hash, loc.text, loc.source, COALESCE(loc.revision, 0)
		FROM event_story_segments seg
		LEFT JOIN event_story_segment_localizations loc ON loc.segment_id=seg.segment_id AND loc.locale=?
		WHERE seg.event_id=? AND seg.episode_no=? AND seg.scenario_id=? ORDER BY seg.position, seg.kind`,
		locale, eventID, episodeNo, parentScenarioID)
	if err != nil {
		return EventEpisodeSnapshot{}, err
	}
	var segments []model.EventStorySegment
	for rows.Next() {
		var segment model.EventStorySegment
		var segmentScenarioID, jpKey string
		var localizedText, localizedSource sql.NullString
		if err := rows.Scan(&segment.ID, &segmentScenarioID, &segment.Kind, &segment.Position, &jpKey,
			&segment.Japanese, &segment.SourceHash, &localizedText, &localizedSource, &segment.Revision); err != nil {
			rows.Close()
			return EventEpisodeSnapshot{}, err
		}
		if _, expected := expectedSegments[segment.ID]; !expected {
			continue
		}
		segment.Text, segment.Source = "", model.SourceUnknown
		if locale == model.LocaleJapanese {
			segment.Text = segment.Japanese
		} else if localizedText.Valid {
			segment.Text, segment.Source = localizedText.String, localizedSource.String
		} else if locale == model.LocaleChinese {
			if segment.Kind == "title" {
				segment.Text, segment.Source = title, titleSource
			} else if legacy, ok := legacyLines[jpKey]; ok {
				segment.Text, segment.Source = legacy.Text, legacy.Source
			}
		}
		segments = append(segments, segment)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return EventEpisodeSnapshot{}, err
	}
	if err := rows.Close(); err != nil {
		return EventEpisodeSnapshot{}, err
	}
	if len(segments) == 0 {
		return EventEpisodeSnapshot{}, ErrEventScenarioConflict
	}
	digest := sha256.New()
	for _, part := range []string{strconv.Itoa(eventID), episodeNo, locale, scenario.SHA256} {
		writeRevisionPart(digest, part)
	}
	for _, segment := range segments {
		for _, part := range []string{segment.ID, segment.Kind, strconv.Itoa(segment.Position), segment.Japanese,
			segment.SourceHash, segment.Text, segment.Source, strconv.Itoa(segment.Revision)} {
			writeRevisionPart(digest, part)
		}
	}
	if err := tx.Commit(); err != nil {
		return EventEpisodeSnapshot{}, err
	}
	return EventEpisodeSnapshot{
		EventID: eventID, EpisodeNo: episodeNo, Locale: locale,
		Revision: base64.RawURLEncoding.EncodeToString(digest.Sum(nil)), Segments: segments,
		Scenario: EventEpisodeScenarioSnapshot{
			ScenarioID: scenario.ScenarioID, FileName: scenario.ScenarioID + ".json", SHA256: scenario.SHA256,
			ParserVersion: EventScenarioParserVersion, RawJSON: scenario.CanonicalJSON, SourceTalks: sourceTalks,
		},
	}, nil
}

type parsedScenario struct {
	Snippets []struct {
		Action         int `json:"Action"`
		ReferenceIndex int `json:"ReferenceIndex"`
	} `json:"Snippets"`
	TalkData []struct {
		WindowDisplayName string `json:"WindowDisplayName"`
		Body              string `json:"Body"`
		Voices            []struct {
			VoiceID       string  `json:"VoiceId"`
			Volume        float64 `json:"Volume"`
			Character2DID int     `json:"Character2dId"`
		} `json:"Voices"`
		WhenFinishCloseWindow int `json:"WhenFinishCloseWindow"`
	} `json:"TalkData"`
	SpecialEffects []struct {
		EffectType int    `json:"EffectType"`
		StringVal  string `json:"StringVal"`
	} `json:"SpecialEffectData"`
	AppearCharacters []struct {
		Character2DID int    `json:"Character2dId"`
		CostumeType   string `json:"CostumeType"`
	} `json:"AppearCharacters"`
}

func ParseEventSourceTalks(canonicalJSON string) ([]EventSourceTalk, error) {
	var scenario parsedScenario
	decoder := json.NewDecoder(bytes.NewBufferString(canonicalJSON))
	if err := decoder.Decode(&scenario); err != nil {
		return nil, fmt.Errorf("%w: parse source talks", ErrEventScenarioInvalid)
	}
	characterByModel := make(map[int]int, len(scenario.AppearCharacters))
	for _, character := range scenario.AppearCharacters {
		if _, exists := characterByModel[character.Character2DID]; !exists {
			characterByModel[character.Character2DID] = scenarioCharacterIndexForCostume(character.CostumeType)
		}
	}
	talks := make([]EventSourceTalk, 0, len(scenario.Snippets))
	for _, snippet := range scenario.Snippets {
		switch snippet.Action {
		case 1:
			if snippet.ReferenceIndex < 0 || snippet.ReferenceIndex >= len(scenario.TalkData) {
				continue
			}
			data := scenario.TalkData[snippet.ReferenceIndex]
			index := snippet.ReferenceIndex
			talk := EventSourceTalk{
				Speaker: strings.SplitN(data.WindowDisplayName, "_", 2)[0], Text: data.Body,
				TalkDataIndex: &index,
			}
			for voiceIndex, voice := range data.Voices {
				talk.Voices = append(talk.Voices, voice.VoiceID)
				talk.Volume = append(talk.Volume, int(voice.Volume))
				if voiceIndex == 0 {
					talk.Chara2D = voice.Character2DID
				}
			}
			talk.CharIndex = scenarioCharacterIndexForSpeaker(talk.Speaker, talk.Chara2D, characterByModel)
			talks = append(talks, talk)
			if data.WhenFinishCloseWindow != 0 {
				talks = append(talks, EventSourceTalk{})
			}
		case 6:
			if snippet.ReferenceIndex < 0 || snippet.ReferenceIndex >= len(scenario.SpecialEffects) {
				continue
			}
			effect := scenario.SpecialEffects[snippet.ReferenceIndex]
			speaker := ""
			switch effect.EffectType {
			case 8:
				speaker = "场景"
			case 18:
				speaker = "左上场景"
			case 23:
				speaker = "选项"
			}
			if speaker != "" {
				talks = append(talks, EventSourceTalk{Speaker: speaker, Text: effect.StringVal})
				talks = append(talks, EventSourceTalk{})
			}
		}
	}
	if len(talks) > 0 && talks[len(talks)-1].Speaker == "" && talks[len(talks)-1].Text == "" {
		talks = talks[:len(talks)-1]
	}
	if talks == nil {
		talks = []EventSourceTalk{}
	}
	return talks, nil
}

var scenarioCharacters = []struct {
	romaji, japanese string
}{
	{"ichika", "一歌"}, {"saki", "咲希"}, {"honami", "穂波"}, {"shiho", "志歩"},
	{"minori", "みのり"}, {"haruka", "遥"}, {"airi", "愛莉"}, {"shizuku", "雫"},
	{"kohane", "こはね"}, {"an", "杏"}, {"akito", "彰人"}, {"touya", "冬弥"},
	{"tsukasa", "司"}, {"emu", "えむ"}, {"nene", "寧々"}, {"rui", "類"},
	{"kanade", "奏"}, {"mafuyu", "まふゆ"}, {"ena", "絵名"}, {"mizuki", "瑞希"},
	{"miku", "ミク"}, {"rin", "リン"}, {"len", "レン"}, {"luka", "ルカ"},
	{"meiko", "MEIKO"}, {"kaito", "KAITO"},
}

func scenarioCharacterIndexForSpeaker(speaker string, character2DID int, characterByModel map[int]int) int {
	if speaker == "" || strings.Trim(speaker, "？?") == "" {
		return -1
	}
	for index, character := range scenarioCharacters {
		if character.japanese == speaker {
			return index
		}
	}
	if index, ok := characterByModel[character2DID]; ok {
		return index
	}
	for _, separator := range []string{"・", "＆"} {
		if !strings.Contains(speaker, separator) {
			continue
		}
		for _, part := range strings.Split(speaker, separator) {
			if index := scenarioCharacterIndexForNamePrefix(part); index >= 0 {
				return index
			}
		}
	}
	return scenarioCharacterIndexForNamePrefix(speaker)
}

func scenarioCharacterIndexForNamePrefix(speaker string) int {
	for index, character := range scenarioCharacters {
		if character.japanese == speaker || strings.HasPrefix(speaker, character.japanese+"の") {
			return index
		}
	}
	return -1
}

var scenarioCostumeCharacter = regexp.MustCompile(`(?:^|_)\d{2}([a-z]+)`)

func scenarioCharacterIndexForCostume(costume string) int {
	match := scenarioCostumeCharacter.FindStringSubmatch(costume)
	if match == nil {
		return -1
	}
	for index, character := range scenarioCharacters {
		if character.romaji == match[1] {
			return index
		}
	}
	return -1
}
