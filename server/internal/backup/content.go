package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/legacy"
	"moesekai/server/internal/store"
)

const (
	translationContentSchemaVersion   = 1
	maxTranslationContentManifestSize = 64 << 10
	// Keep typed restore allocations bounded independently of compressed and
	// expanded byte limits. Current production data remains well below this.
	maxTranslationContentRecords = 1_000_000
)

var backupSnapshotCreatedHook func() error

type contentManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Files         []contentManifestFile `json:"files"`
}

type contentManifestFile struct {
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	Count         int    `json:"count"`
	ScenarioCount int    `json:"scenarioCount,omitempty"`
}

type translationContent struct {
	Entries []store.EntryLocalizationRecord
	Events  store.EventContentExport
	Lyrics  store.LyricsContentExport
}

func (m *Manager) materializeTranslationContent(parent string) (string, error) {
	return materializeTranslationContentFromStoreContext(context.Background(), parent, m.store)
}

func materializeTranslationContentFromStore(parent string, source *store.Store) (string, error) {
	return materializeTranslationContentFromStoreContext(context.Background(), parent, source)
}

func materializeTranslationContentFromStoreContext(ctx context.Context, parent string, source *store.Store) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, "translation-content")
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries, err := source.ExportEntryLocalizationsContext(ctx)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	events, err := source.ExportEventContentContext(ctx)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	lyrics, err := source.ExportLyricsContentContext(ctx)
	if err != nil {
		return "", err
	}
	totalRecords := len(entries) + eventContentCount(events) + len(events.Scenarios) + lyricsContentCount(lyrics)
	if totalRecords > maxTranslationContentRecords {
		return "", fmt.Errorf("translation content exceeds %d records", maxTranslationContentRecords)
	}
	files := []struct {
		name  string
		value any
		count int
	}{
		{"entries.json", entries, len(entries)},
		{"event-stories.json", events, eventContentCount(events)},
		{"lyrics.json", lyrics, lyricsContentCount(lyrics)},
	}
	manifest := contentManifest{SchemaVersion: translationContentSchemaVersion, Files: []contentManifestFile{}}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		body, err := json.MarshalIndent(file.value, "", "  ")
		if err != nil {
			return "", err
		}
		limit := archiveFileByteLimit(filepath.Join("translation-content", file.name))
		if int64(len(body)) > limit {
			return "", fmt.Errorf("backup file %s exceeds %d bytes", file.name, limit)
		}
		if err := os.WriteFile(filepath.Join(dir, file.name), body, 0o644); err != nil {
			return "", err
		}
		sum := sha256.Sum256(body)
		record := contentManifestFile{Path: file.name, SHA256: hex.EncodeToString(sum[:]), Count: file.count}
		if file.name == "event-stories.json" {
			record.ScenarioCount = len(events.Scenarios)
		}
		manifest.Files = append(manifest.Files, record)
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if len(manifestJSON) > maxTranslationContentManifestSize {
		return "", fmt.Errorf("translation content manifest exceeds %d bytes", maxTranslationContentManifestSize)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestJSON, 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *Manager) materializeBackupPayload(parent string) (string, string, error) {
	return m.materializeBackupPayloadContext(context.Background(), parent)
}

func (m *Manager) materializeBackupPayloadContext(ctx context.Context, parent string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", "", err
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return "", "", err
	}
	snapshotPath := filepath.Join(parent, "backup-snapshot.db")
	_ = os.Remove(snapshotPath)
	if err := m.store.SnapshotContext(ctx, snapshotPath); err != nil {
		return "", "", err
	}
	if err := os.Chmod(snapshotPath, 0o600); err != nil {
		return "", "", err
	}
	defer func() {
		_ = os.Remove(snapshotPath)
		_ = os.Remove(snapshotPath + "-wal")
		_ = os.Remove(snapshotPath + "-shm")
	}()
	if backupSnapshotCreatedHook != nil {
		if err := backupSnapshotCreatedHook(); err != nil {
			return "", "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	snapshotDB, err := db.Open(snapshotPath)
	if err != nil {
		return "", "", err
	}
	defer snapshotDB.Close()
	snapshotStore := store.New(snapshotDB)
	snapshotEvents := store.NewEventStore(snapshotDB)
	snapshotGenerator := files.NewGenerator(snapshotStore, snapshotEvents, parent)
	// Keep Generator.WriteAllContext's legacy category/event semantics intact.
	// Backup-only canonical public lyrics assets are written explicitly below,
	// using the same snapshot-backed generator as the durable content export.
	translations, err := materializeTranslationsWithGeneratorContext(ctx, parent, snapshotGenerator)
	if err != nil {
		return "", "", err
	}
	if err := materializeBackupLyricsAssetsContext(ctx, translations, snapshotGenerator); err != nil {
		return "", "", err
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	content, err := materializeTranslationContentFromStoreContext(ctx, parent, snapshotStore)
	if err != nil {
		return "", "", err
	}
	return translations, content, nil
}

// materializeBackupLyricsAssetsContext archives the canonical public lyrics
// projection inside translations/ without broadening Generator.WriteAllContext.
// The supplied generator must be backed by the same SQLite snapshot used for
// the rest of the backup payload.
func materializeBackupLyricsAssetsContext(ctx context.Context, translations string, generator *files.Generator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	assets, err := generator.PublishedLyricsJSON()
	if err != nil {
		return fmt.Errorf("published lyrics: %w", err)
	}
	paths := make([]string, 0, len(assets))
	for path := range assets {
		if path != "translation/lyrics/index.json" &&
			(!strings.HasPrefix(path, "translation/lyrics/music_") || !strings.HasSuffix(path, ".json")) {
			return fmt.Errorf("unexpected published lyrics asset path %q", path)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative := strings.TrimPrefix(path, "translation/")
		destination := filepath.Join(translations, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := writeBackupAssetAtomicContext(ctx, destination, assets[path]); err != nil {
			return err
		}
	}
	return nil
}

func writeBackupAssetAtomicContext(ctx context.Context, destination string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary := destination + ".tmp"
	if err := os.WriteFile(temporary, body, 0o644); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func readTranslationContent(dir string) (translationContent, bool, error) {
	return readTranslationContentContext(context.Background(), dir)
}

func readTranslationContentContext(ctx context.Context, dir string) (translationContent, bool, error) {
	if err := ctx.Err(); err != nil {
		return translationContent{}, false, err
	}
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return translationContent{}, false, nil
	}
	if err != nil {
		return translationContent{}, false, err
	}
	if !info.IsDir() {
		return translationContent{}, true, fmt.Errorf("translation content path is not a directory")
	}
	manifestBytes, err := readFileWithLimitContext(ctx, filepath.Join(dir, "manifest.json"), maxTranslationContentManifestSize)
	if os.IsNotExist(err) {
		return translationContent{}, true, fmt.Errorf("translation content manifest is missing")
	}
	if err != nil {
		return translationContent{}, true, err
	}
	var manifest contentManifest
	if err := decodeJSONContext(ctx, manifestBytes, &manifest); err != nil {
		return translationContent{}, true, fmt.Errorf("translation content manifest: %w", err)
	}
	if manifest.SchemaVersion != translationContentSchemaVersion {
		return translationContent{}, true, fmt.Errorf("unsupported translation content schemaVersion %d", manifest.SchemaVersion)
	}
	if len(manifest.Files) != 3 {
		return translationContent{}, true, fmt.Errorf("translation content manifest must contain exactly three files")
	}
	expected := map[string]bool{"entries.json": true, "event-stories.json": true, "lyrics.json": true}
	seen := map[string]bool{}
	declaredRecords := 0
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return translationContent{}, true, err
		}
		if file.Count < 0 || file.ScenarioCount < 0 || file.Count > maxTranslationContentRecords-declaredRecords {
			return translationContent{}, true, fmt.Errorf("translation content manifest exceeds %d records", maxTranslationContentRecords)
		}
		declaredRecords += file.Count
		if file.Path == "event-stories.json" {
			if file.ScenarioCount > maxTranslationContentRecords-declaredRecords {
				return translationContent{}, true, fmt.Errorf("translation content manifest exceeds %d records", maxTranslationContentRecords)
			}
			declaredRecords += file.ScenarioCount
		} else if file.ScenarioCount != 0 {
			return translationContent{}, true, fmt.Errorf("unexpected scenarioCount for %s", file.Path)
		}
	}
	content := translationContent{}
	actualRecords := 0
	for _, file := range manifest.Files {
		if !expected[file.Path] || seen[file.Path] || filepath.Base(file.Path) != file.Path {
			return translationContent{}, true, fmt.Errorf("invalid translation content manifest path %q", file.Path)
		}
		seen[file.Path] = true
		body, err := readFileWithLimitContext(ctx, filepath.Join(dir, file.Path), int(archiveFileByteLimit(filepath.Join("translation-content", file.Path))))
		if err != nil {
			return translationContent{}, true, err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return translationContent{}, true, fmt.Errorf("translation content checksum mismatch: %s", file.Path)
		}
		count, scenarioCount, records, err := preflightTranslationContentJSONContext(ctx, file.Path, body, maxTranslationContentRecords-actualRecords)
		if err != nil {
			return translationContent{}, true, fmt.Errorf("translation content %s: %w", file.Path, err)
		}
		actualRecords += records
		if count != file.Count {
			return translationContent{}, true, fmt.Errorf("translation content count mismatch: %s", file.Path)
		}
		if scenarioCount != file.ScenarioCount {
			return translationContent{}, true, fmt.Errorf("translation content scenario count mismatch: %s", file.Path)
		}
		switch file.Path {
		case "entries.json":
			if err := decodeJSONContext(ctx, body, &content.Entries); err != nil {
				return translationContent{}, true, err
			}
		case "event-stories.json":
			if err := decodeJSONContext(ctx, body, &content.Events); err != nil {
				return translationContent{}, true, err
			}
		case "lyrics.json":
			if err := decodeJSONContext(ctx, body, &content.Lyrics); err != nil {
				return translationContent{}, true, err
			}
		}
	}
	if len(seen) != len(expected) {
		return translationContent{}, true, fmt.Errorf("translation content manifest is incomplete")
	}
	return content, true, nil
}

func readFileWithLimit(path string, limit int) ([]byte, error) {
	return readFileWithLimitContext(context.Background(), path, limit)
}

func readFileWithLimitContext(ctx context.Context, path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("translation content manifest exceeds %d bytes", limit)
	}
	body, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("translation content manifest exceeds %d bytes", limit)
	}
	return body, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(buffer) > 64<<10 {
		buffer = buffer[:64<<10]
	}
	return r.reader.Read(buffer)
}

func decodeJSONContext(ctx context.Context, body []byte, target any) error {
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: bytes.NewReader(body)})
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON")
		}
		return err
	}
	return nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	return io.Copy(destination, &contextReader{ctx: ctx, reader: source})
}

func preflightTranslationContentJSON(path string, body []byte, maxRecords int) (int, int, int, error) {
	return preflightTranslationContentJSONContext(context.Background(), path, body, maxRecords)
}

func preflightTranslationContentJSONContext(ctx context.Context, path string, body []byte, maxRecords int) (int, int, int, error) {
	counts, rootArray, total, err := topLevelJSONArraysContext(ctx, body, maxRecords)
	if err != nil {
		return 0, 0, 0, err
	}
	switch path {
	case "entries.json":
		if !rootArray {
			return 0, 0, 0, fmt.Errorf("top level must be an array")
		}
		return counts[""], 0, total, nil
	case "event-stories.json":
		if rootArray {
			return 0, 0, 0, fmt.Errorf("top level must be an object")
		}
		return counts["segments"] + counts["localizations"] + counts["localeMeta"], counts["scenarios"], total, nil
	case "lyrics.json":
		if rootArray {
			return 0, 0, 0, fmt.Errorf("top level must be an object")
		}
		return counts["music"] + counts["performers"] + counts["documents"] + counts["lines"] +
			counts["segments"] + counts["publications"] + counts["sourceDocuments"] + counts["sourceArtifacts"] +
			counts["sourceIndexEvidence"] + counts["sourceArtifactEvidence"] + counts["sourceContributions"] +
			counts["renditionLocalizations"] + counts["renditionTranslationLines"] +
			counts["translationEditionStates"] + counts["translationEditions"] +
			counts["translationEditionLocalizations"] + counts["translationEditionLines"] +
			counts["recoveryBatches"] + counts["recoveryItems"] + counts["recoverySourceEvidence"] +
			counts["recoveryArtifacts"] + counts["recoveryArtifactEvidence"] + counts["recoveryContributions"] +
			counts["availabilityDocuments"] + counts["recoveryTakeovers"], 0, total, nil
	default:
		return 0, 0, 0, fmt.Errorf("unexpected content path")
	}
}

func topLevelJSONArrays(body []byte, maxRecords int) (map[string]int, bool, int, error) {
	return topLevelJSONArraysContext(context.Background(), body, maxRecords)
}

func topLevelJSONArraysContext(ctx context.Context, body []byte, maxRecords int) (map[string]int, bool, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, 0, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil {
		return nil, false, 0, err
	}
	delimiter, ok := first.(json.Delim)
	if !ok || (delimiter != '[' && delimiter != '{') {
		return nil, false, 0, fmt.Errorf("top level must be an array or object")
	}
	counts := map[string]int{}
	total := 0
	rootArray := delimiter == '['
	if rootArray {
		count, err := countJSONArray(ctx, decoder, &total, maxRecords)
		if err != nil {
			return nil, false, 0, err
		}
		counts[""] = count
	} else {
		seen := map[string]bool{}
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return nil, false, 0, err
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, false, 0, err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return nil, false, 0, fmt.Errorf("invalid or duplicate top-level field")
			}
			seen[key] = true
			value, err := decoder.Token()
			if err != nil {
				return nil, false, 0, err
			}
			if value == json.Delim('[') {
				count, err := countJSONArray(ctx, decoder, &total, maxRecords)
				if err != nil {
					return nil, false, 0, err
				}
				counts[key] = count
			} else if err := consumeJSONValue(ctx, decoder, value); err != nil {
				return nil, false, 0, err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return nil, false, 0, fmt.Errorf("invalid top-level object")
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false, 0, fmt.Errorf("trailing JSON")
	}
	return counts, rootArray, total, nil
}

func countJSONArray(ctx context.Context, decoder *json.Decoder, total *int, maxRecords int) (int, error) {
	count := 0
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if *total >= maxRecords {
			return 0, fmt.Errorf("record count exceeds %d", maxRecords)
		}
		value, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		if err := consumeJSONValue(ctx, decoder, value); err != nil {
			return 0, err
		}
		count++
		*total++
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
		return 0, fmt.Errorf("invalid array")
	}
	return count, nil
}

func consumeJSONValue(ctx context.Context, decoder *json.Decoder, value json.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delimiter, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return fmt.Errorf("invalid or duplicate object field")
			}
			seen[key] = true
			nested, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := consumeJSONValue(ctx, decoder, nested); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("invalid object")
		}
	case '[':
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			nested, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := consumeJSONValue(ctx, decoder, nested); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("invalid array")
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	return nil
}

func eventContentCount(content store.EventContentExport) int {
	// Preserve schemaVersion 1 Count semantics; scenarios have their own count.
	return len(content.Segments) + len(content.Localizations) + len(content.LocaleMeta)
}

func lyricsContentCount(content store.LyricsContentExport) int {
	return len(content.Music) + len(content.Performers) + len(content.Documents) +
		len(content.Lines) + len(content.Segments) + len(content.Publications) +
		len(content.SourceDocuments) + len(content.SourceArtifacts) + len(content.SourceIndexEvidence) +
		len(content.SourceArtifactEvidence) + len(content.SourceContributions) +
		len(content.RenditionLocalizations) + len(content.RenditionTranslationLines) +
		len(content.TranslationEditionStates) + len(content.TranslationEditions) +
		len(content.TranslationEditionLocalizations) + len(content.TranslationEditionLines) +
		len(content.RecoveryBatches) + len(content.RecoveryItems) + len(content.RecoverySourceEvidence) +
		len(content.RecoveryArtifacts) + len(content.RecoveryArtifactEvidence) +
		len(content.RecoveryContributions) + len(content.AvailabilityDocuments) + len(content.RecoveryTakeovers)
}

func (m *Manager) importTranslationContent(content translationContent, present bool) error {
	if !present {
		return nil
	}
	return m.store.ImportTranslationContent(content.Entries, content.Events, content.Lyrics)
}
