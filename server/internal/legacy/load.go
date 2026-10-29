// Package legacy loads the pre-refactor translations/ directory faithfully,
// including recovery of known data corruption and preservation of JSON key
// order for event-story lines (which encode story flow).
package legacy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"moesekai/server/internal/jsonstrict"
	"moesekai/server/internal/model"
)

// LoadCategory loads X.full.json (preferred) falling back to flat X.json.
// It tolerates the corrupted virtualLive.full.json where 190/441 entries were
// serialized as Python dict reprs (e.g. "{'text': '...', 'source': 'cn'}")
// instead of JSON objects: text is taken from the authoritative flat file and
// source/ids are recovered from the repr string.
func LoadCategory(src, cat string) (model.Category, []string, error) {
	var warnings []string

	// Flat text authority (always correct in the legacy data).
	flatText := map[string]map[string]string{}
	if data, err := os.ReadFile(filepath.Join(src, cat+".json")); err == nil {
		if err := UnmarshalUnique(data, &flatText); err != nil {
			return nil, warnings, fmt.Errorf("%s.json: %w", cat, err)
		}
	}

	fullPath := filepath.Join(src, cat+".full.json")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		// No full file: build from flat with source=unknown.
		if len(flatText) == 0 {
			return nil, warnings, fmt.Errorf("no data for %s", cat)
		}
		result := make(model.Category)
		for field, entries := range flatText {
			result[field] = make(map[string]model.Entry)
			for k, v := range entries {
				result[field][k] = model.Entry{Text: v, Source: model.SourceUnknown}
			}
		}
		warnings = append(warnings, fmt.Sprintf("%s: no full.json, used flat with source=unknown", cat))
		return result, warnings, nil
	}

	// Decode permissively: each entry is either an object or a (corrupt) string.
	var rawCat map[string]map[string]json.RawMessage
	if err := UnmarshalUnique(data, &rawCat); err != nil {
		return nil, warnings, fmt.Errorf("%s.full.json: %w", cat, err)
	}

	result := make(model.Category)
	recovered := 0
	for field, entries := range rawCat {
		result[field] = make(map[string]model.Entry, len(entries))
		for key, raw := range entries {
			var e model.Entry
			if err := json.Unmarshal(raw, &e); err == nil {
				result[field][key] = e
				continue
			}
			// Corrupt entry: recover from flat text + repr-parsed source/ids.
			var reprStr string
			if err := json.Unmarshal(raw, &reprStr); err != nil {
				return nil, warnings, fmt.Errorf("%s.full.json field %s key %q: unparseable", cat, field, key)
			}
			rec := recoverPyRepr(reprStr)
			if t, ok := flatText[field][key]; ok {
				rec.Text = t
			}
			result[field][key] = rec
			recovered++
		}
	}
	if recovered > 0 {
		warnings = append(warnings, fmt.Sprintf("%s: recovered %d corrupt entries (text from flat, source/ids from repr)", cat, recovered))
	}
	return result, warnings, nil
}

// UnmarshalUnique rejects duplicate object keys at every nesting level before
// decoding. encoding/json otherwise silently keeps the final value, which can
// make a reviewed flat/full backup mean something different after restore.
func UnmarshalUnique(data []byte, target any) error {
	if err := ValidateUniqueJSON(data); err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// ValidateUniqueJSON rejects duplicate object keys at every nesting level and
// rejects additional top-level values without otherwise materializing the JSON.
func ValidateUniqueJSON(data []byte) error {
	return jsonstrict.Scan(data, jsonstrict.Options{})
}

var (
	reSource = regexp.MustCompile(`'source'\s*:\s*'([^']*)'`)
	reIDList = regexp.MustCompile(`'ids'\s*:\s*\[([^]]*)\]`)
	reIDItem = regexp.MustCompile(`'([^']*)'`)
)

// recoverPyRepr extracts source and ids from a Python dict repr string.
// Text is intentionally not parsed here (recovered from the flat file instead),
// since text may itself contain quote characters that break naive parsing.
func recoverPyRepr(s string) model.Entry {
	e := model.Entry{Source: model.SourceUnknown}
	if m := reSource.FindStringSubmatch(s); m != nil {
		e.Source = m[1]
	}
	if m := reIDList.FindStringSubmatch(s); m != nil {
		for _, item := range reIDItem.FindAllStringSubmatch(m[1], -1) {
			e.Ids = append(e.Ids, item[1])
		}
	}
	return e
}

// EventStory is an order-preserving representation of event_N.json.
type EventStory struct {
	Meta        model.EventStoryMeta
	EpisodeKeys []string // episode numbers in file order
	Episodes    map[string]*EventEpisode
}

type EventEpisode struct {
	ScenarioID string
	Title      string
	TalkKeys   []string          // jp keys in file (story) order
	TalkData   map[string]string // jp -> cn
	// Optional fields preserved if present (older editor writes may include them).
	TitleSource  string
	TalkSources  map[string]string
	SpeakerNames map[string]string
}

// LoadEventStory parses an event_N.json preserving talkData key order, which
// encodes the dialogue flow and must survive the DB round-trip.
func LoadEventStory(path string) (*EventStory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := ValidateUniqueJSON(raw); err != nil {
		return nil, err
	}
	// First a normal decode for meta + non-ordered fields.
	var plain struct {
		Meta     model.EventStoryMeta `json:"meta"`
		Episodes map[string]struct {
			ScenarioID   string            `json:"scenarioId"`
			Title        string            `json:"title"`
			TitleSource  string            `json:"titleSource"`
			TalkData     map[string]string `json:"talkData"`
			TalkSources  map[string]string `json:"talkSources"`
			TalkOrder    []string          `json:"talkOrder"`
			SpeakerNames map[string]string `json:"speakerNames"`
		} `json:"episodes"`
	}
	if err := json.Unmarshal(raw, &plain); err != nil {
		return nil, err
	}

	es := &EventStory{Meta: plain.Meta, Episodes: map[string]*EventEpisode{}}
	// Capture episode + talkData key order via streaming token parse.
	epOrder, talkOrders, err := parseEventOrder(raw)
	if err != nil {
		return nil, err
	}
	es.EpisodeKeys = epOrder

	for epNo, ep := range plain.Episodes {
		ee := &EventEpisode{
			ScenarioID:   ep.ScenarioID,
			Title:        ep.Title,
			TitleSource:  ep.TitleSource,
			TalkData:     ep.TalkData,
			TalkSources:  ep.TalkSources,
			SpeakerNames: ep.SpeakerNames,
		}
		// Prefer explicit talkOrder, else file order, else map iteration.
		switch {
		case len(ep.TalkOrder) > 0:
			ee.TalkKeys = dedupeKeys(ep.TalkOrder, ep.TalkData)
		case len(talkOrders[epNo]) > 0:
			ee.TalkKeys = dedupeKeys(talkOrders[epNo], ep.TalkData)
		default:
			for k := range ep.TalkData {
				ee.TalkKeys = append(ee.TalkKeys, k)
			}
		}
		es.Episodes[epNo] = ee
	}
	// Episodes present in token order but missing from struct (shouldn't happen).
	if len(es.EpisodeKeys) == 0 {
		for k := range es.Episodes {
			es.EpisodeKeys = append(es.EpisodeKeys, k)
		}
	}
	return es, nil
}

// dedupeKeys returns order keys that exist in data, then any data keys not in
// order, so no talk line is dropped even if talkOrder is incomplete.
func dedupeKeys(order []string, data map[string]string) []string {
	seen := make(map[string]bool, len(order))
	out := make([]string, 0, len(data))
	for _, k := range order {
		if _, ok := data[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	for k := range data {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	return out
}

// parseEventOrder streams the JSON to record the order of episode keys and,
// within each episode, the order of talkData keys.
func parseEventOrder(raw []byte) (epOrder []string, talkOrders map[string][]string, err error) {
	talkOrders = map[string][]string{}
	var top map[string]json.RawMessage
	if err = json.Unmarshal(raw, &top); err != nil {
		return nil, nil, err
	}
	episodesRaw, ok := top["episodes"]
	if !ok {
		return nil, talkOrders, nil
	}
	epOrder, err = objectKeyOrder(episodesRaw)
	if err != nil {
		return nil, nil, err
	}
	var episodes map[string]json.RawMessage
	if err = json.Unmarshal(episodesRaw, &episodes); err != nil {
		return nil, nil, err
	}
	for epNo, epRaw := range episodes {
		var epObj map[string]json.RawMessage
		if err = json.Unmarshal(epRaw, &epObj); err != nil {
			return nil, nil, err
		}
		if td, ok := epObj["talkData"]; ok {
			order, oerr := objectKeyOrder(td)
			if oerr != nil {
				return nil, nil, oerr
			}
			talkOrders[epNo] = order
		}
	}
	return epOrder, talkOrders, nil
}

// objectKeyOrder returns the keys of a JSON object in source order, using the
// streaming decoder: read '{', then alternately read each key and skip its
// value (recursing into nested objects/arrays).
func objectKeyOrder(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected object")
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", kt)
		}
		keys = append(keys, key)
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// skipValue consumes exactly one JSON value from the decoder, descending into
// objects and arrays to consume their full contents.
func skipValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return nil // scalar: already consumed
	}
	if d != '{' && d != '[' {
		return nil
	}
	isObj := d == '{'
	for dec.More() {
		if isObj {
			if _, err := dec.Token(); err != nil { // key
				return err
			}
		}
		if err := skipValue(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token() // closing delim
	return err
}
