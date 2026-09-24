package files

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// Generator rebuilds the public, CDN-cacheable JSON files from the DB so the
// consumer site (pjsk.moe) keeps consuming the exact same formats as before.
type Generator struct {
	store      *store.Store
	eventStore *store.EventStore
	outDir     string // root containing translation/ and data/
}

func NewGenerator(s *store.Store, es *store.EventStore, outDir string) *Generator {
	return &Generator{store: s, eventStore: es, outDir: outDir}
}

// WithOutDir returns a copy of the generator that writes under a different root
// directory. Used by backup to materialize translations into scratch space.
func (g *Generator) WithOutDir(outDir string) *Generator {
	clone := *g
	clone.outDir = outDir
	return &clone
}

// MarshalIndentCompat marshals with two-space indent and HTML-escaping ON,
// matching the legacy category files which were written with the standard
// library's json.MarshalIndent (so &, <, > appear as &, <, >).
func MarshalIndentCompat(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// marshalIndentNoEscape marshals with two-space indent and HTML-escaping OFF,
// matching the legacy event-story files which keep &, <, > literal. Byte-stable
// output avoids needless CDN cache churn on regeneration.
func marshalIndentNoEscape(v any) ([]byte, error) {
	compact, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// marshalNoEscape produces compact JSON without HTML escaping. The encoder
// appends a trailing newline, which is trimmed.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// CategoryFlatJSON returns the flat-format bytes for a category (X.json).
func (g *Generator) CategoryFlatJSON(category string) ([]byte, error) {
	flat, err := g.store.FlatData(category)
	if err != nil {
		return nil, err
	}
	return MarshalIndentCompat(flat)
}

// CategoryFullJSON returns the full-format bytes for a category (X.full.json).
func (g *Generator) CategoryFullJSON(category string) ([]byte, error) {
	cat, err := g.store.CategoryData(category)
	if err != nil {
		return nil, err
	}
	return MarshalIndentCompat(cat)
}

// CategoryLocaleFlatJSON returns the v2 flat projection for an explicit locale.
func (g *Generator) CategoryLocaleFlatJSON(category, locale string) ([]byte, error) {
	categoryData, err := g.store.CategoryDataLocale(category, locale)
	if err != nil {
		return nil, err
	}
	flat := make(map[string]map[string]string, len(categoryData))
	for field, entries := range categoryData {
		flat[field] = make(map[string]string, len(entries))
		for key, entry := range entries {
			flat[field][key] = entry.Text
		}
	}
	return MarshalIndentCompat(flat)
}

// CategoryLocaleFullJSON returns the v2 full projection for an explicit locale.
func (g *Generator) CategoryLocaleFullJSON(category, locale string) ([]byte, error) {
	categoryData, err := g.store.CategoryDataLocale(category, locale)
	if err != nil {
		return nil, err
	}
	return MarshalIndentCompat(categoryData)
}

// EventStoryJSON returns the event_N.json bytes in the public, seed-compatible
// shape: meta + episodes (in order), each episode = {scenarioId, title,
// talkData} with talkData lines in story order. Source/speaker tracking lives
// in the DB and is exposed via the console API, not the public file.
func (g *Generator) EventStoryJSON(eventID int) ([]byte, error) {
	od, err := g.eventStore.OrderedDetail(eventID)
	if err != nil {
		return nil, err
	}
	root := newOrderedMap()

	meta := newOrderedMap()
	meta.set("source", od.Meta.Source)
	meta.set("version", od.Meta.Version)
	meta.set("last_updated", od.Meta.LastUpdated)
	root.set("meta", meta)

	episodes := newOrderedMap()
	for _, ep := range od.Episodes {
		epObj := newOrderedMap()
		epObj.set("scenarioId", ep.ScenarioID)
		epObj.set("title", ep.Title)
		talk := newOrderedMap()
		for _, jp := range ep.TalkKeys {
			talk.set(jp, ep.TalkData[jp])
		}
		epObj.set("talkData", talk)
		episodes.set(ep.EpisodeNo, epObj)
	}
	root.set("episodes", episodes)

	return marshalIndentNoEscape(root)
}

// EventStoryLocaleJSON returns the additive full-fidelity locale projection.
// Legacy event files continue to use EventStoryJSON unchanged.
func (g *Generator) EventStoryLocaleJSON(eventID int, locale string) ([]byte, error) {
	detail, err := g.eventStore.DetailLocale(eventID, locale)
	if err != nil {
		return nil, err
	}
	// Translation revisions are an authenticated editing concern. Keep the
	// existing public v2 projection byte-for-byte stable.
	for episodeNo, episode := range detail.Episodes {
		for i := range episode.Segments {
			episode.Segments[i].Revision = 0
		}
		detail.Episodes[episodeNo] = episode
	}
	return marshalIndentNoEscape(detail)
}

// SideStoryFilesJSON returns every public card-story and area-talk file of
// locale, keyed relative to the translation root ("cardStory/card_<id>.json",
// "areaTalk/group_<n>.json").
func (g *Generator) SideStoryFilesJSON(ctx context.Context, locale string) (map[string][]byte, error) {
	projected, err := g.store.SideStoryPublicFilesContext(ctx, locale)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(projected))
	for key, file := range projected {
		body, err := sideStoryJSON(file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out[key] = body
	}
	return out, nil
}

// SideStoryFileForStoryJSON returns the public file holding one story. key is
// set even when ok is false (the file no longer qualifies), so the caller can
// withdraw it.
func (g *Generator) SideStoryFileForStoryJSON(ctx context.Context, kind, storyID, locale string) (string, []byte, bool, error) {
	key, file, ok, err := g.store.SideStoryPublicFileForStoryContext(ctx, kind, storyID, locale)
	if err != nil || !ok {
		return key, nil, false, err
	}
	body, err := sideStoryJSON(file)
	if err != nil {
		return key, nil, false, err
	}
	return key, body, true, nil
}

// sideStoryJSON encodes a side-story file in the event-story file layout plus
// per-episode source: meta, then episodes in file order with talkData in story
// order, without HTML escaping.
func sideStoryJSON(file store.SideStoryPublicFile) ([]byte, error) {
	root := newOrderedMap()
	meta := newOrderedMap()
	meta.set("source", file.Source)
	meta.set("version", "1")
	meta.set("last_updated", file.LastUpdated)
	root.set("meta", meta)
	episodes := newOrderedMap()
	for _, episode := range file.Episodes {
		object := newOrderedMap()
		object.set("scenarioId", episode.ScenarioID)
		object.set("title", episode.Title)
		object.set("source", episode.Source)
		talk := newOrderedMap()
		for _, line := range episode.Talk {
			talk.set(line.JP, line.Text)
		}
		object.set("talkData", talk)
		episodes.set(episode.Key, object)
	}
	root.set("episodes", episodes)
	return marshalIndentNoEscape(root)
}

// PublishedLyricsJSON builds the complete published lyrics asset set. Callers
// swap the returned map atomically so a malformed publication cannot expose a
// partially rebuilt index/detail set.
func (g *Generator) PublishedLyricsJSON() (map[string][]byte, error) {
	index, details, err := g.store.PublishedLyrics()
	if err != nil {
		return nil, err
	}
	assets := map[string][]byte{}
	indexJSON, err := MarshalIndentCompat(index)
	if err != nil {
		return nil, err
	}
	indexJSON = append(indexJSON, '\n')
	if len(index.Songs) > model.PublicLyricsMaxIndexEntries || len(indexJSON) > model.PublicLyricsMaxArtifactBytes {
		return nil, fmt.Errorf("published lyrics index exceeds the public artifact contract")
	}
	assets["translation/lyrics/index.json"] = indexJSON
	for musicID, detail := range details {
		body, err := MarshalIndentCompat(detail)
		if err != nil {
			return nil, err
		}
		body = append(body, '\n')
		if len(body) > model.PublicLyricsMaxArtifactBytes {
			return nil, fmt.Errorf("published lyrics detail %d exceeds the public artifact contract", musicID)
		}
		assets[fmt.Sprintf("translation/lyrics/music_%d.json", musicID)] = body
	}
	return assets, nil
}

// PublishedLyricsLocalizationProjection returns edited source-v3 rendition
// localizations as validated public v3 index entries and detail documents (and v4 detail documents for multi-edition songs).
// The runtime overlay merges them exactly like legacy database publications.
func (g *Generator) PublishedLyricsLocalizationProjection() ([]store.PublicLyricsIndexSong, map[int]store.PublicLyricsV3DetailDocument, map[int]store.PublicLyricsV4DetailDocument, error) {
	return g.store.PublishedLyricsLocalizationProjection()
}

// WriteAll regenerates the legacy category/event translation/ projection under
// outDir. Published lyrics, locale mirrors, and search indexes are materialized
// by their owning runtime or backup paths rather than changing this legacy
// generator contract. Returns the number of files written.
func (g *Generator) WriteAll() (int, error) {
	return g.WriteAllContext(context.Background())
}

func (g *Generator) WriteAllContext(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	transDir := filepath.Join(g.outDir, "translation")
	if err := os.MkdirAll(transDir, 0o755); err != nil {
		return 0, err
	}
	written := 0
	for _, cat := range model.SupportedCategories {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		flat, err := g.CategoryFlatJSON(cat)
		if err != nil {
			return written, fmt.Errorf("flat %s: %w", cat, err)
		}
		if err := writeAtomicContext(ctx, filepath.Join(transDir, cat+".json"), flat); err != nil {
			return written, err
		}
		written++
		full, err := g.CategoryFullJSON(cat)
		if err != nil {
			return written, fmt.Errorf("full %s: %w", cat, err)
		}
		if err := writeAtomicContext(ctx, filepath.Join(transDir, cat+".full.json"), full); err != nil {
			return written, err
		}
		written++
	}

	esDir := filepath.Join(transDir, "eventStory")
	if err := os.MkdirAll(esDir, 0o755); err != nil {
		return written, err
	}
	summaries, err := g.eventStore.List()
	if err != nil {
		return written, err
	}
	for _, sum := range summaries {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		b, err := g.EventStoryJSON(sum.EventID)
		if err != nil {
			return written, fmt.Errorf("event %d: %w", sum.EventID, err)
		}
		path := filepath.Join(esDir, "event_"+strconv.Itoa(sum.EventID)+".json")
		if err := writeAtomicContext(ctx, path, b); err != nil {
			return written, err
		}
		written++
	}

	return written, nil
}

func writeAtomic(path string, data []byte) error {
	return writeAtomicContext(context.Background(), path, data)
}

func writeAtomicContext(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
