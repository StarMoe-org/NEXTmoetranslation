package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/importer"
	"moesekai/server/internal/legacy"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

type legacyBackupHarness struct {
	manager  *Manager
	store    *store.Store
	events   *store.EventStore
	cfg      *config.Config
	gen      *files.Generator
	database *db.DB
}

func setupLegacyBackup(t *testing.T) *legacyBackupHarness {
	t.Helper()
	t.Setenv(backupEncryptionKeyEnv, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	database, err := db.Open(filepath.Join(t.TempDir(), "legacy-backup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := store.New(database)
	es := store.NewEventStore(database)
	cfg, err := config.New(database, "legacy-backup-master-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportCategory("cards", model.Category{
		"prefix": {
			"こんにちは": {Text: "你好", Source: model.SourceHuman, Ids: []string{"legacy-1"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, category := range model.SupportedCategories {
		if category == "cards" {
			continue
		}
		if _, err := s.ImportCategory(category, model.Category{"name": {
			category + "-jp": {Text: category + "-zh", Source: model.SourceHuman},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := es.ImportOrdered(42, model.EventStoryMeta{
		Source: "official_cn", Version: "1.0", LastUpdated: 1700000000,
	}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario-1", Title: "人工标题", TitleSource: model.SourceHuman,
		TalkKeys:     []string{"一", "二"},
		TalkData:     map[string]string{"一": "第一句", "二": "第二句"},
		TalkSources:  map[string]string{"一": model.SourcePinned, "二": model.SourceHuman},
		SpeakerNames: map[string]string{"一": "角色"},
	}}); err != nil {
		t.Fatal(err)
	}
	canonical, digest, err := store.CanonicalizeEventScenario(map[string]any{
		"ScenarioId":        "scenario-1",
		"Snippets":          []any{map[string]any{"Action": float64(1), "ReferenceIndex": float64(0)}},
		"TalkData":          []any{map[string]any{"WindowDisplayName": "角色", "Body": "一", "Voices": []any{}}},
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, "scenario-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.BackfillScenarios(42, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "scenario-1", ScenarioCanonicalJSON: canonical, ScenarioSHA256: digest,
	}}); err != nil {
		t.Fatal(err)
	}
	gen := files.NewGenerator(s, es, "")
	manager := NewManager(cfg, gen, s, es, filepath.Join(t.TempDir(), "work"))
	return &legacyBackupHarness{manager: manager, store: s, events: es, cfg: cfg, gen: gen, database: database}
}

func TestLegacyBackupRestoreRoundTrip(t *testing.T) {
	source := setupLegacyBackup(t)
	backupRoot := t.TempDir()
	translations, err := source.manager.materializeTranslations(backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(translations) != "translations" {
		t.Fatalf("backup directory = %q", translations)
	}
	for _, path := range []string{
		"cards.json", "cards.full.json", filepath.Join("eventStory", "event_42.json"),
	} {
		if _, err := os.Stat(filepath.Join(translations, path)); err != nil {
			t.Fatalf("missing backup payload %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(translations, "lyrics")); !os.IsNotExist(err) {
		t.Fatalf("standalone legacy materialization unexpectedly included backup-only lyrics: %v", err)
	}

	destDB, err := db.Open(filepath.Join(t.TempDir(), "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer destDB.Close()
	destStore := store.New(destDB)
	destEvents := store.NewEventStore(destDB)
	hooks := 0
	destStore.OnChange(func() { hooks++ })
	result, err := importer.ImportDir(translations, destStore, destEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Categories != len(model.SupportedCategories) || result.Entries != len(model.SupportedCategories) || result.EventStories != 1 {
		t.Fatalf("restore result = %+v", result)
	}
	if hooks != 1 {
		t.Fatalf("restore change hooks = %d, want 1", hooks)
	}

	destGen := files.NewGenerator(destStore, destEvents, "")
	compareGenerated := func(name string, sourceFn, destFn func() ([]byte, error)) {
		t.Helper()
		want, err := sourceFn()
		if err != nil {
			t.Fatal(err)
		}
		got, err := destFn()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s changed across backup/restore\ngot:\n%s\nwant:\n%s", name, got, want)
		}
	}
	compareGenerated("cards flat", func() ([]byte, error) { return source.gen.CategoryFlatJSON("cards") }, func() ([]byte, error) { return destGen.CategoryFlatJSON("cards") })
	compareGenerated("cards full", func() ([]byte, error) { return source.gen.CategoryFullJSON("cards") }, func() ([]byte, error) { return destGen.CategoryFullJSON("cards") })
	compareGenerated("event public", func() ([]byte, error) { return source.gen.EventStoryJSON(42) }, func() ([]byte, error) { return destGen.EventStoryJSON(42) })

	// The legacy backup is a public projection: these private fields are lost on restore.
	detail, err := destEvents.Detail(42)
	if err != nil {
		t.Fatal(err)
	}
	ep := detail.Episodes["1"]
	if ep.TitleSource != "" || ep.TalkSources["一"] != "official_cn" || ep.TalkSources["二"] != "official_cn" || len(ep.SpeakerNames) != 0 {
		t.Fatalf("restored legacy provenance contract changed: %+v", ep)
	}
}

func TestLegacyS3BackupTriggerAndRestoreSemantics(t *testing.T) {
	h := setupLegacyBackup(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ChineseTitle: "新歌", EnglishTitle: "New Song", IsNewlyWrittenMusic: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	saved, err := h.store.SaveLyrics(model.SongLyrics{
		MusicID: 10, Attribution: "MoeSeka translation team",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "歌う", Chinese: "歌唱", English: "Sings",
			Segments: []model.LyricSegment{{Text: "歌う", PerformerIDs: []int{1}}},
		}},
	}, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.PublishLyrics(10, saved.Revision); err != nil {
		t.Fatal(err)
	}
	wantAssets, err := h.gen.PublishedLyricsJSON()
	if err != nil {
		t.Fatal(err)
	}
	type requestRecord struct {
		method, path, auth, contentType string
		body                            []byte
	}
	var mu sync.Mutex
	var requests []requestRecord
	var latest []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, requestRecord{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: append([]byte(nil), body...),
		})
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/latest.enc") {
			latest = append([]byte(nil), body...)
		}
		response := append([]byte(nil), latest...)
		mu.Unlock()
		if r.Method == http.MethodGet {
			_, _ = w.Write(response)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	settings := map[string]string{
		config.KeyBackupS3Endpoint:  server.URL,
		config.KeyBackupS3Region:    "legacy-region-1",
		config.KeyBackupS3Bucket:    "legacy-bucket",
		config.KeyBackupS3Prefix:    "/snapshots/",
		config.KeyBackupS3AccessKey: "legacy-access",
		config.KeyBackupS3SecretKey: "legacy-secret",
	}
	for key, value := range settings {
		if err := h.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	puts := append([]requestRecord(nil), requests...)
	mu.Unlock()
	if len(puts) != 3 {
		t.Fatalf("S3 backup requests = %d, want timestamped, latest, and the superseded pointer delete", len(puts))
	}
	timestamped := regexp.MustCompile(`^/legacy-bucket/snapshots/translations-[0-9]{8}-[0-9]{6}\.enc$`)
	if puts[0].method != http.MethodPut || !timestamped.MatchString(puts[0].path) {
		t.Fatalf("timestamped PUT = %s %s", puts[0].method, puts[0].path)
	}
	if puts[1].method != http.MethodPut || puts[1].path != "/legacy-bucket/snapshots/latest.enc" {
		t.Fatalf("latest PUT = %s %s", puts[1].method, puts[1].path)
	}
	if puts[2].method != http.MethodDelete || puts[2].path != "/legacy-bucket/snapshots/latest.tar.gz" {
		t.Fatalf("superseded pointer request = %s %s", puts[2].method, puts[2].path)
	}
	for _, put := range puts[:2] {
		if !strings.HasPrefix(put.auth, "AWS4-HMAC-SHA256 Credential=legacy-access/") {
			t.Fatalf("missing SigV4 Authorization: %q", put.auth)
		}
		if put.contentType != backupEnvelopeMediaType {
			t.Fatalf("PUT Content-Type = %q", put.contentType)
		}
		if bytes.HasPrefix(put.body, []byte{0x1f, 0x8b}) || json.Valid(put.body) || bytes.Contains(put.body, []byte("MoeSeka translation team")) {
			t.Fatalf("S3 uploaded plaintext or directly parseable backup bytes: %x", put.body[:min(len(put.body), 32)])
		}
	}
	if !bytes.Equal(puts[0].body, puts[1].body) || len(puts[0].body) == 0 {
		t.Fatal("timestamped and latest objects must contain the same non-empty encrypted artifact")
	}
	archiveRoot := decryptBackupArtifactToDir(t, puts[1].body)
	for path, want := range wantAssets {
		archived, err := os.ReadFile(filepath.Join(archiveRoot, filepath.FromSlash(strings.TrimPrefix(path, "translation/"))))
		if err != nil {
			t.Fatalf("S3 archive lyrics asset %s: %v", path, err)
		}
		if !bytes.Equal(archived, want) {
			t.Fatalf("S3 archive lyrics asset %s changed\ngot: %q\nwant: %q", path, archived, want)
		}
	}

	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "被覆盖", model.SourceLLM, "test"); err != nil {
		t.Fatal(err)
	}
	result, err := h.manager.restoreS3()
	if err != nil {
		t.Fatal(err)
	}
	if result.Entries != len(model.SupportedCategories) || result.EventStories != 1 {
		t.Fatalf("S3 restore result = %+v", result)
	}
	category, err := h.store.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	if got := category["prefix"]["こんにちは"]; got.Text != "你好" || got.Source != model.SourceHuman {
		t.Fatalf("S3 restore entry = %+v", got)
	}
	gotAssets, err := h.gen.PublishedLyricsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAssets) != len(wantAssets) {
		t.Fatalf("S3 restored lyrics asset count = %d, want %d", len(gotAssets), len(wantAssets))
	}
	for path, want := range wantAssets {
		if got, ok := gotAssets[path]; !ok || !bytes.Equal(got, want) {
			t.Fatalf("S3 restored lyrics asset %s changed\ngot: %q\nwant: %q", path, got, want)
		}
	}
}

func TestLegacyGitBackupCommitsOnlyEncryptedArtifact(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	h := setupLegacyBackup(t)
	remote := filepath.Join(t.TempDir(), "legacy-remote.git")
	cmd := exec.Command("git", "init", "--bare", "--initial-branch=legacy-backup", remote)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	if err := h.cfg.Set(config.KeyBackupGitRepoURL, "file://"+remote); err != nil {
		t.Fatal(err)
	}
	if err := h.cfg.Set(config.KeyBackupGitBranch, "legacy-backup"); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.backupGit(); err != nil {
		t.Fatal(err)
	}
	first := gitOutput(t, remote, "rev-parse", "refs/heads/legacy-backup")
	if tree := gitOutput(t, remote, "ls-tree", "--name-only", "refs/heads/legacy-backup"); tree != backupEnvelopeFilename {
		t.Fatalf("Git backup tree = %q, want only %q", tree, backupEnvelopeFilename)
	}
	artifact := gitOutputBytes(t, remote, "show", "refs/heads/legacy-backup:"+backupEnvelopeFilename)
	if bytes.Contains(artifact, []byte(`"legacy-1"`)) || bytes.Contains(artifact, []byte(`"source": "human"`)) {
		t.Fatal("Git remote artifact exposed plaintext translation metadata")
	}
	archiveRoot := decryptBackupArtifactToDir(t, artifact)
	full, err := os.ReadFile(filepath.Join(archiveRoot, "cards.full.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(full, []byte(`"source": "human"`)) || !bytes.Contains(full, []byte(`"legacy-1"`)) {
		t.Fatalf("decrypted Git backup projection missing full entry metadata: %s", full)
	}
	if err := h.manager.backupGit(); err != nil {
		t.Fatal(err)
	}
	second := gitOutput(t, remote, "rev-parse", "refs/heads/legacy-backup")
	if first == second {
		t.Fatal("fresh random envelope unexpectedly reused the previous Git commit")
	}
}

func TestBackupMaterializesPublishedLyricsWithoutChangingLegacyGenerator(t *testing.T) {
	h := setupLegacyBackup(t)
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ChineseTitle: "新歌", EnglishTitle: "New Song", IsNewlyWrittenMusic: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	saved, err := h.store.SaveLyrics(model.SongLyrics{
		MusicID: 10, Revision: 0, Attribution: "MoeSeka translation team",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "歌う", Chinese: "歌唱", English: "Sings",
			Segments: []model.LyricSegment{{Text: "歌う", PerformerIDs: []int{1}}},
		}},
	}, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.PublishLyrics(10, saved.Revision); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	generator := files.NewGenerator(h.store, h.events, root)
	written, err := generator.WriteAllContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if written == 0 {
		t.Fatal("legacy projection wrote no files")
	}
	if _, err := os.Stat(filepath.Join(root, "translation", "cards.json")); err != nil {
		t.Fatalf("legacy category projection missing: %v", err)
	}
	for _, path := range []string{
		filepath.Join(root, "translation", "lyrics"),
		filepath.Join(root, "v2"),
		filepath.Join(root, "data", "search-index.json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("WriteAllContext unexpectedly materialized non-legacy public asset %s: %v", path, err)
		}
	}

	assets, err := generator.PublishedLyricsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 || len(assets["translation/lyrics/index.json"]) == 0 || len(assets["translation/lyrics/music_10.json"]) == 0 {
		t.Fatalf("runtime lyrics generator assets = %v", assets)
	}
	backupRoot := t.TempDir()
	translations, contentDir, err := h.manager.materializeBackupPayload(backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range assets {
		archivedPath := filepath.Join(translations, filepath.FromSlash(strings.TrimPrefix(path, "translation/")))
		got, err := os.ReadFile(archivedPath)
		if err != nil {
			t.Fatalf("backup lyrics asset %s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("backup lyrics asset %s differs from PublishedLyricsJSON\ngot: %q\nwant: %q", path, got, want)
		}
	}
	lyricsBackup, err := os.ReadFile(filepath.Join(contentDir, "lyrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(lyricsBackup, []byte(`"publications"`)) || !bytes.Contains(lyricsBackup, []byte(`"musicId": 10`)) {
		t.Fatalf("durable lyrics backup omitted publication snapshot: %s", lyricsBackup)
	}
}

func TestGitBackupArchivesAndRestoresPublishedLyricsAssets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	source := setupLegacyBackup(t)
	if err := source.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ChineseTitle: "新歌", EnglishTitle: "New Song", IsNewlyWrittenMusic: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := source.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	saved, _, err := source.store.SaveImportedLyricsMutation(model.SongLyrics{
		MusicID: 10, Revision: 0, Attribution: "MoeSeka translation team",
		SourceURL: "https://legacy.invalid/wiki", SourcePageID: 12, SourceRevisionID: 34,
		SourceSHA1: strings.Repeat("a", 40), SourceFetchedAt: "2026-07-22T12:00:00Z",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "歌う", Chinese: "歌唱", English: "Sings",
			Segments: []model.LyricSegment{{Text: "歌う", PerformerIDs: []int{1}}},
		}},
	}, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.PublishLyrics(10, saved.Revision); err != nil {
		t.Fatal(err)
	}
	wantAssets, err := source.gen.PublishedLyricsJSON()
	if err != nil {
		t.Fatal(err)
	}

	remote := filepath.Join(t.TempDir(), "lyrics-remote.git")
	command := exec.Command("git", "init", "--bare", "--initial-branch=lyrics-backup", remote)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, output)
	}
	if err := source.cfg.Set(config.KeyBackupGitRepoURL, "file://"+remote); err != nil {
		t.Fatal(err)
	}
	if err := source.cfg.Set(config.KeyBackupGitBranch, "lyrics-backup"); err != nil {
		t.Fatal(err)
	}
	if err := source.manager.backupGit(); err != nil {
		t.Fatal(err)
	}
	artifact := gitOutputBytes(t, remote, "show", "refs/heads/lyrics-backup:"+backupEnvelopeFilename)
	for _, plaintext := range [][]byte{[]byte("https://legacy.invalid/wiki"), []byte("MoeSeka translation team"), []byte(strings.Repeat("a", 40))} {
		if bytes.Contains(artifact, plaintext) {
			t.Fatalf("Git remote artifact exposed private lyrics provenance %q", plaintext)
		}
	}
	archiveRoot := decryptBackupArtifactToDir(t, artifact)
	backupLyrics, err := os.ReadFile(filepath.Join(archiveRoot, "translation-content", "lyrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(backupLyrics, []byte(`"publications"`)) || !bytes.Contains(backupLyrics, []byte(`"musicId": 10`)) {
		t.Fatalf("decrypted Git backup omitted published lyrics snapshot: %s", backupLyrics)
	}
	for path, want := range wantAssets {
		archivePath := filepath.Join(archiveRoot, filepath.FromSlash(strings.TrimPrefix(path, "translation/")))
		archived, err := os.ReadFile(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(archived, want) {
			t.Fatalf("Git backup lyrics asset %s changed\ngot: %q\nwant: %q", path, archived, want)
		}
	}

	destination := setupLegacyBackup(t)
	if err := destination.cfg.Set(config.KeyBackupGitRepoURL, "file://"+remote); err != nil {
		t.Fatal(err)
	}
	if err := destination.cfg.Set(config.KeyBackupGitBranch, "lyrics-backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.manager.restoreGit(); err != nil {
		t.Fatal(err)
	}
	gotAssets, err := destination.gen.PublishedLyricsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAssets) != len(wantAssets) {
		t.Fatalf("regenerated lyrics asset count = %d, want %d", len(gotAssets), len(wantAssets))
	}
	for key, want := range wantAssets {
		if got, ok := gotAssets[key]; !ok || !bytes.Equal(got, want) {
			t.Fatalf("regenerated asset %s changed\ngot: %s\nwant: %s", key, got, want)
		}
	}
	restoredLyrics, err := destination.store.GetLyrics(10)
	if err != nil {
		t.Fatal(err)
	}
	if restoredLyrics.SourceSHA1 != strings.Repeat("a", 40) || restoredLyrics.SourceRevisionID != 34 {
		t.Fatalf("canonical provenance changed across Git backup restore: %+v", restoredLyrics)
	}
}

func TestGitRestoreRejectsNestedEventDuplicateBeforeApply(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	source := setupLegacyBackup(t)
	remote := filepath.Join(t.TempDir(), "duplicate-event-remote.git")
	command := exec.Command("git", "init", "--bare", "--initial-branch=duplicate-event", remote)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, output)
	}
	if err := source.cfg.Set(config.KeyBackupGitRepoURL, "file://"+remote); err != nil {
		t.Fatal(err)
	}
	if err := source.cfg.Set(config.KeyBackupGitBranch, "duplicate-event"); err != nil {
		t.Fatal(err)
	}
	if err := source.manager.backupGit(); err != nil {
		t.Fatal(err)
	}

	clone := filepath.Join(t.TempDir(), "tamper")
	command = exec.Command("git", "clone", "--branch", "duplicate-event", "file://"+remote, clone)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, output)
	}
	artifactPath := filepath.Join(clone, backupEnvelopeFilename)
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	archiveRoot := decryptBackupArtifactToDir(t, artifact)
	eventPath := filepath.Join(archiveRoot, "eventStory", "event_42.json")
	event, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	event = bytes.Replace(event, []byte(`"title": "人工标题"`), []byte(`"title": "人工标题", "title": "篡改标题"`), 1)
	if err := os.WriteFile(eventPath, event, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperedArchive, err := tarGzDir(archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	encryptionKey := testBackupEncryptionKey(t)
	tamperedArtifact, err := encryptBackupEnvelope(tamperedArchive, encryptionKey)
	clear(encryptionKey)
	clear(tamperedArchive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, tamperedArtifact, 0o600); err != nil {
		t.Fatal(err)
	}
	clear(tamperedArtifact)
	for _, args := range [][]string{
		{"-C", clone, "config", "user.name", "test"},
		{"-C", clone, "config", "user.email", "test@example.invalid"},
		{"-C", clone, "add", backupEnvelopeFilename},
		{"-C", clone, "commit", "-m", "tamper nested duplicate event"},
		{"-C", clone, "push", "origin", "duplicate-event"},
	} {
		command = exec.Command("git", args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}

	destination := setupLegacyBackup(t)
	beforeCategory, err := destination.store.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	beforeEvent, err := destination.events.OrderedDetail(42)
	if err != nil {
		t.Fatal(err)
	}
	beforeStatus := destination.manager.Status()
	var beforeAudits int
	if err := destination.database.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='backup.restore'`).Scan(&beforeAudits); err != nil {
		t.Fatal(err)
	}
	changeNotifications := 0
	destination.store.OnChange(func() { changeNotifications++ })
	if err := destination.cfg.Set(config.KeyBackupGitRepoURL, "file://"+remote); err != nil {
		t.Fatal(err)
	}
	if err := destination.cfg.Set(config.KeyBackupGitBranch, "duplicate-event"); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.manager.RestoreFromAs("git", "operator"); err == nil || !strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("duplicate event restore error = %v", err)
	}
	afterCategory, err := destination.store.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	afterEvent, err := destination.events.OrderedDetail(42)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterCategory, beforeCategory) || !reflect.DeepEqual(afterEvent, beforeEvent) {
		t.Fatalf("duplicate event restore changed destination\ncategory before=%+v\ncategory after=%+v\nevent before=%+v\nevent after=%+v", beforeCategory, afterCategory, beforeEvent, afterEvent)
	}
	var afterAudits int
	if err := destination.database.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='backup.restore'`).Scan(&afterAudits); err != nil {
		t.Fatal(err)
	}
	afterStatus := destination.manager.Status()
	if afterAudits != beforeAudits || changeNotifications != 0 || afterStatus.LastRestore != beforeStatus.LastRestore {
		t.Fatalf("duplicate event restore committed side effects: audits %d->%d notifications=%d lastRestore %q->%q", beforeAudits, afterAudits, changeNotifications, beforeStatus.LastRestore, afterStatus.LastRestore)
	}
	if afterStatus.Running || afterStatus.LastOperation != "restore:git" || afterStatus.LastFinished == "" || !strings.Contains(afterStatus.LastError, "duplicate object key") {
		t.Fatalf("duplicate event restore terminal status = %+v", afterStatus)
	}
}

func TestTranslationContentManifestRoundTripAndAtomicFailure(t *testing.T) {
	source := setupLegacyBackup(t)
	if _, err := source.store.UpdateEntryLocale("cards", "prefix", "こんにちは", "Hello", model.SourceHuman, "editor", model.LocaleEnglish); err != nil {
		t.Fatal(err)
	}
	if err := source.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", EnglishTitle: "New Song", IsNewlyWrittenMusic: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := source.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	saved, err := source.store.SaveLyrics(model.SongLyrics{
		MusicID: 10, Revision: 0, Attribution: "MoeSeka translation team",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "歌う", Chinese: "歌唱", English: "Sings",
			Segments: []model.LyricSegment{{Text: "歌う", PerformerIDs: []int{1}}},
		}},
	}, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.PublishLyrics(10, saved.Revision); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	translations, err := source.manager.materializeTranslations(root)
	if err != nil {
		t.Fatal(err)
	}
	contentDir, err := source.manager.materializeTranslationContent(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(contentDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"password_hash", "legacy-admin", "llm.openai.key"} {
		if bytes.Contains(manifest, []byte(forbidden)) {
			t.Fatalf("manifest contains forbidden user/secret field %q", forbidden)
		}
	}
	content, present, err := readTranslationContent(contentDir)
	if err != nil || !present {
		t.Fatalf("read content present=%v err=%v", present, err)
	}
	if len(content.Events.Scenarios) != 1 || content.Events.Scenarios[0].ScenarioID != "scenario-1" {
		t.Fatalf("backup scenarios = %+v", content.Events.Scenarios)
	}
	if !bytes.Contains(manifest, []byte(`"scenarioCount": 1`)) {
		t.Fatalf("manifest omitted scenarioCount: %s", manifest)
	}

	destDB, err := db.Open(filepath.Join(t.TempDir(), "content-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer destDB.Close()
	destStore := store.New(destDB)
	destEvents := store.NewEventStore(destDB)
	if _, err := importer.ImportDir(translations, destStore, destEvents); err != nil {
		t.Fatal(err)
	}
	if err := destStore.ImportTranslationContent(content.Entries, content.Events, content.Lyrics); err != nil {
		t.Fatal(err)
	}
	english, err := destStore.GetEntriesLocale("cards", "prefix", "", model.LocaleEnglish)
	if err != nil {
		t.Fatal(err)
	}
	if len(english) != 1 || english[0].Text != "Hello" {
		t.Fatalf("restored English entries = %+v", english)
	}
	restoredLyrics, err := destStore.GetLyrics(10)
	if err != nil || restoredLyrics.Status != "published" || restoredLyrics.Attribution != "MoeSeka translation team" {
		t.Fatalf("restored lyrics = %+v err=%v", restoredLyrics, err)
	}
	restoredEventContent, err := destStore.ExportEventContent()
	if err != nil || len(restoredEventContent.Scenarios) != 1 || restoredEventContent.Scenarios[0].SHA256 != content.Events.Scenarios[0].SHA256 {
		t.Fatalf("restored scenarios=%+v err=%v", restoredEventContent.Scenarios, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*store.EventScenarioRecord)
	}{
		{"sha", func(record *store.EventScenarioRecord) { record.SHA256 = strings.Repeat("0", 64) }},
		{"parent identity", func(record *store.EventScenarioRecord) {
			canonical, digest, canonicalErr := store.CanonicalizeEventScenario(map[string]any{
				"ScenarioId": "other", "Snippets": []any{}, "TalkData": []any{},
				"SpecialEffectData": []any{}, "AppearCharacters": []any{},
			}, "other")
			if canonicalErr != nil {
				t.Fatal(canonicalErr)
			}
			record.ScenarioID, record.CanonicalJSON, record.SHA256 = "other", canonical, digest
		}},
	} {
		t.Run("invalid scenario "+test.name, func(t *testing.T) {
			invalid := content
			invalid.Events.Scenarios = append([]store.EventScenarioRecord(nil), content.Events.Scenarios...)
			test.mutate(&invalid.Events.Scenarios[0])
			if err := destStore.ImportTranslationContent(invalid.Entries, invalid.Events, invalid.Lyrics); err == nil {
				t.Fatal("invalid scenario content unexpectedly imported")
			}
			after, err := destStore.ExportEventContent()
			if err != nil || len(after.Scenarios) != 1 || after.Scenarios[0].SHA256 != restoredEventContent.Scenarios[0].SHA256 {
				t.Fatalf("invalid scenario import was not atomic: scenarios=%+v err=%v", after.Scenarios, err)
			}
		})
	}

	entriesPath := filepath.Join(contentDir, "entries.json")
	entriesBody, err := os.ReadFile(entriesPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entriesPath, append(entriesBody, ' '), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readTranslationContent(contentDir); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered manifest error = %v", err)
	}

	invalidContent := content
	invalidContent.Events.Segments = append(invalidContent.Events.Segments, store.EventSegmentRecord{
		SegmentID: "missing-parent", EventID: 999, EpisodeNo: "1", Kind: "talk", Position: 0,
	})
	if err := destStore.ImportTranslationContent(invalidContent.Entries, invalidContent.Events, invalidContent.Lyrics); err == nil {
		t.Fatal("expected foreign-key failure for invalid content")
	}
	english, err = destStore.GetEntriesLocale("cards", "prefix", "", model.LocaleEnglish)
	if err != nil || len(english) != 1 || english[0].Text != "Hello" {
		t.Fatalf("failed import was not atomic: entries=%+v err=%v", english, err)
	}
	invalidSegmentIdentity := content
	invalidSegmentIdentity.Events.Segments = append([]store.EventSegmentRecord(nil), content.Events.Segments...)
	invalidSegmentIdentity.Events.Segments[0].ScenarioID = "wrong-parent-scenario"
	if err := destStore.ImportTranslationContent(invalidSegmentIdentity.Entries, invalidSegmentIdentity.Events, invalidSegmentIdentity.Lyrics); err == nil ||
		!strings.Contains(err.Error(), "scenario identity") {
		t.Fatalf("mismatched event segment error = %v", err)
	}
}

func TestBackupPayloadSecuresSnapshotAndParentDirectory(t *testing.T) {
	h := setupLegacyBackup(t)
	parent := filepath.Join(t.TempDir(), "backup-payload")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	backupSnapshotCreatedHook = func() error {
		parentInfo, err := os.Stat(parent)
		if err != nil {
			return err
		}
		if got := parentInfo.Mode().Perm(); got != 0o700 {
			return fmt.Errorf("backup parent mode=%#o want %#o", got, os.FileMode(0o700))
		}
		snapshotInfo, err := os.Stat(filepath.Join(parent, "backup-snapshot.db"))
		if err != nil {
			return err
		}
		if got := snapshotInfo.Mode().Perm(); got != 0o600 {
			return fmt.Errorf("backup snapshot mode=%#o want %#o", got, os.FileMode(0o600))
		}
		return nil
	}
	t.Cleanup(func() { backupSnapshotCreatedHook = nil })
	if _, _, err := h.manager.materializeBackupPayload(parent); err != nil {
		t.Fatal(err)
	}
	backupSnapshotCreatedHook = nil
	if _, err := os.Stat(filepath.Join(parent, "backup-snapshot.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup snapshot remains after materialization: %v", err)
	}
}

func TestBackupPayloadUsesSingleSQLiteSnapshot(t *testing.T) {
	h := setupLegacyBackup(t)
	if _, err := h.store.UpdateEntryLocale("cards", "prefix", "こんにちは", "Old English", model.SourceHuman, "editor", model.LocaleEnglish); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "新曲", ChineseTitle: "新歌", EnglishTitle: "New Song", IsNewlyWrittenMusic: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "初音ミク"}}); err != nil {
		t.Fatal(err)
	}
	saved, err := h.store.SaveLyrics(model.SongLyrics{
		MusicID: 10, Attribution: "MoeSeka translation team",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "歌う", Chinese: "歌唱", English: "Sings",
			Segments: []model.LyricSegment{{Text: "歌う", PerformerIDs: []int{1}}},
		}},
	}, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.PublishLyrics(10, saved.Revision); err != nil {
		t.Fatal(err)
	}
	wantAssets, err := h.gen.PublishedLyricsJSON()
	if err != nil {
		t.Fatal(err)
	}
	backupSnapshotCreatedHook = func() error {
		if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "新中文", model.SourceHuman, "editor"); err != nil {
			return err
		}
		if _, err := h.store.UpdateEntryLocale("cards", "prefix", "こんにちは", "New English", model.SourceHuman, "editor", model.LocaleEnglish); err != nil {
			return err
		}
		updated := saved
		updated.Attribution = "Changed after snapshot"
		updated, err := h.store.SaveLyrics(updated, "editor")
		if err != nil {
			return err
		}
		_, err = h.store.PublishLyrics(10, updated.Revision)
		return err
	}
	t.Cleanup(func() { backupSnapshotCreatedHook = nil })
	translations, contentDir, err := h.manager.materializeBackupPayload(t.TempDir())
	backupSnapshotCreatedHook = nil
	if err != nil {
		t.Fatal(err)
	}
	category, _, err := legacy.LoadCategory(translations, "cards")
	if err != nil {
		t.Fatal(err)
	}
	if got := category["prefix"]["こんにちは"].Text; got != "你好" {
		t.Fatalf("legacy snapshot text = %q", got)
	}
	content, present, err := readTranslationContent(contentDir)
	if err != nil || !present {
		t.Fatalf("content present=%v err=%v", present, err)
	}
	if len(content.Entries) != 1 || content.Entries[0].Text != "Old English" {
		t.Fatalf("additive snapshot entries = %+v", content.Entries)
	}
	if len(content.Lyrics.Publications) != 1 || content.Lyrics.Publications[0].Revision != saved.Revision {
		t.Fatalf("lyrics snapshot publications = %+v", content.Lyrics.Publications)
	}
	updatedAssets, err := h.gen.PublishedLyricsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(updatedAssets["translation/lyrics/music_10.json"], wantAssets["translation/lyrics/music_10.json"]) {
		t.Fatal("post-snapshot lyrics update did not change the live publication")
	}

	for path, want := range wantAssets {
		got, err := os.ReadFile(filepath.Join(translations, filepath.FromSlash(strings.TrimPrefix(path, "translation/"))))
		if err != nil {
			t.Fatalf("snapshot backup lyrics asset %s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("snapshot backup lyrics asset %s changed\ngot: %q\nwant: %q", path, got, want)
		}
	}
}

func TestBackupAllUsesOneSnapshotForS3AndGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	h := setupLegacyBackup(t)
	var mu sync.Mutex
	var latest []byte
	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/latest.enc") {
			mu.Lock()
			latest = append([]byte(nil), body...)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Server.Close()
	remote := filepath.Join(t.TempDir(), "shared-snapshot.git")
	command := exec.Command("git", "init", "--bare", "--initial-branch=shared-snapshot", remote)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, output)
	}
	for key, value := range map[string]string{
		config.KeyBackupS3Enabled:   "true",
		config.KeyBackupS3Endpoint:  s3Server.URL,
		config.KeyBackupS3Region:    "test-region",
		config.KeyBackupS3Bucket:    "test-bucket",
		config.KeyBackupS3Prefix:    "snapshots",
		config.KeyBackupS3AccessKey: "test-access",
		config.KeyBackupS3SecretKey: "test-secret",
		config.KeyBackupGitEnabled:  "true",
		config.KeyBackupGitRepoURL:  "file://" + remote,
		config.KeyBackupGitBranch:   "shared-snapshot",
	} {
		if err := h.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	snapshotCount := 0
	backupSnapshotCreatedHook = func() error {
		snapshotCount++
		_, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "快照后修改", model.SourceHuman, "editor")
		return err
	}
	t.Cleanup(func() { backupSnapshotCreatedHook = nil })
	results, err := h.manager.BackupAll()
	backupSnapshotCreatedHook = nil
	if err != nil {
		t.Fatalf("BackupAll error=%v results=%v", err, results)
	}
	if snapshotCount != 1 {
		t.Fatalf("BackupAll snapshot count=%d, want 1", snapshotCount)
	}
	if results["s3"] != "ok" || results["git"] != "ok" {
		t.Fatalf("BackupAll results=%v", results)
	}
	mu.Lock()
	artifact := append([]byte(nil), latest...)
	mu.Unlock()
	gitArtifact := gitOutputBytes(t, remote, "show", "refs/heads/shared-snapshot:"+backupEnvelopeFilename)
	if !bytes.Equal(artifact, gitArtifact) {
		t.Fatal("S3 and Git did not receive the same encrypted single-snapshot artifact")
	}
	if bytes.HasPrefix(artifact, []byte{0x1f, 0x8b}) || json.Valid(artifact) || bytes.Contains(artifact, []byte("快照后修改")) {
		t.Fatal("shared remote artifact exposed plaintext or directly parseable backup bytes")
	}
	archiveRoot := decryptBackupArtifactToDir(t, artifact)
	for _, path := range []string{"cards.json", "cards.full.json", filepath.Join("translation-content", "entries.json")} {
		body, err := os.ReadFile(filepath.Join(archiveRoot, path))
		if err != nil {
			t.Fatalf("decrypted shared archive %s: %v", path, err)
		}
		if bytes.Contains(body, []byte("快照后修改")) {
			t.Fatalf("shared backup %s included post-snapshot mutation", path)
		}
	}
}

func TestTranslationContentDirectoryWithoutManifestIsRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "translation-content")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, present, err := readTranslationContent(dir); err == nil || !present || !strings.Contains(err.Error(), "manifest is missing") {
		t.Fatalf("missing manifest present=%v err=%v", present, err)
	}
}

func TestS3RestoreSupportsRootAndNestedContentLayouts(t *testing.T) {
	h := setupLegacyBackup(t)
	translations, content, err := h.manager.materializeBackupPayload(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		contentNested bool
	}{
		{name: "root translations and content"},
		{name: "nested legacy content", contentNested: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			translationTarget := filepath.Join(root, "translations")
			if err := copyDir(translations, translationTarget); err != nil {
				t.Fatal(err)
			}
			contentTarget := filepath.Join(root, "translation-content")
			if test.contentNested {
				contentTarget = filepath.Join(translationTarget, "translation-content")
			}
			if err := copyDir(content, contentTarget); err != nil {
				t.Fatal(err)
			}
			src, contentDir, err := s3RestoreDirs(t.Context(), root)
			if err != nil || src != translationTarget || contentDir != contentTarget {
				t.Fatalf("layout src=%q content=%q err=%v", src, contentDir, err)
			}
			if _, present, err := readTranslationContent(contentDir); err != nil || !present {
				t.Fatalf("layout content present=%v err=%v", present, err)
			}
		})
	}
}

func TestS3RestoreLayoutSelectionHonorsCancellationBeforeValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s3RestoreDirs(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled layout selection error = %v", err)
	}
}

func TestS3RestoreRejectsAmbiguousRootAndNestedLayouts(t *testing.T) {
	h := setupLegacyBackup(t)
	translations, content, err := h.manager.materializeBackupPayload(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("legacy projections", func(t *testing.T) {
		root := t.TempDir()
		if err := copyDir(translations, root); err != nil {
			t.Fatal(err)
		}
		if err := copyDir(translations, filepath.Join(root, "translations")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s3RestoreDirs(t.Context(), root); err == nil || !strings.Contains(err.Error(), "ambiguous root and nested translations") {
			t.Fatalf("ambiguous projection error = %v", err)
		}
	})
	t.Run("additive content", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "translations")
		if err := copyDir(translations, nested); err != nil {
			t.Fatal(err)
		}
		if err := copyDir(content, filepath.Join(root, "translation-content")); err != nil {
			t.Fatal(err)
		}
		if err := copyDir(content, filepath.Join(nested, "translation-content")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s3RestoreDirs(t.Context(), root); err == nil || !strings.Contains(err.Error(), "ambiguous root and nested translation-content") {
			t.Fatalf("ambiguous additive error = %v", err)
		}
	})
}

func TestRestoreIsAtomicAndOldBackupClearsAdditiveState(t *testing.T) {
	source := setupLegacyBackup(t)
	translations, contentDir, err := source.manager.materializeBackupPayload(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := importer.ReadDir(translations)
	if err != nil {
		t.Fatal(err)
	}
	content, present, err := readTranslationContent(contentDir)
	if err != nil || !present {
		t.Fatalf("content present=%v err=%v", present, err)
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "atomic-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	destination := store.New(database)
	destinationEvents := store.NewEventStore(database)
	if err := destinationEvents.ImportOrdered(99, model.EventStoryMeta{Source: model.SourceHuman}, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "newer", Title: "must disappear", TalkData: map[string]string{},
	}}); err != nil {
		t.Fatal(err)
	}
	newerCanonical, newerDigest, err := store.CanonicalizeEventScenario(map[string]any{
		"ScenarioId": "newer", "Snippets": []any{}, "TalkData": []any{},
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, "newer")
	if err != nil {
		t.Fatal(err)
	}
	if err := destinationEvents.BackfillScenarios(99, []store.OrderedEpisode{{
		EpisodeNo: "1", ScenarioID: "newer", ScenarioCanonicalJSON: newerCanonical, ScenarioSHA256: newerDigest,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.ImportCategory("cards", model.Category{"prefix": {
		"こんにちは": {Text: "Before Chinese", Source: model.SourceHuman},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.UpdateEntryLocale("cards", "prefix", "こんにちは", "Before English", model.SourceHuman, "editor", model.LocaleEnglish); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO event_story_locale_meta(event_id, locale, last_updated) VALUES (99, 'en-US', 1234)`); err != nil {
		t.Fatal(err)
	}
	if err := destination.UpsertMusicCatalog([]store.MusicCatalogRecord{{MusicID: 10, JapaneseTitle: "歌", IsNewlyWrittenMusic: true}}); err != nil {
		t.Fatal(err)
	}
	if err := destination.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 1, JapaneseName: "歌手"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.SaveLyrics(model.SongLyrics{MusicID: 10, Lines: []model.LyricLine{{
		ID: "line", Order: 0, Japanese: "歌", Chinese: "歌", English: "Song",
		Segments: []model.LyricSegment{{Text: "歌", PerformerIDs: []int{1}}},
	}}}, "editor"); err != nil {
		t.Fatal(err)
	}

	invalid := content
	invalid.Events.Segments = append(invalid.Events.Segments, store.EventSegmentRecord{
		SegmentID: "missing-parent", EventID: 999, EpisodeNo: "1", Kind: "talk", Position: 0,
	})
	if err := destination.RestoreBackup(payload.Categories, payload.Events, invalid.Entries, invalid.Events, invalid.Lyrics, true, "admin"); err == nil {
		t.Fatal("invalid additive restore unexpectedly succeeded")
	}
	incomplete := content
	incomplete.Events.Segments = append([]store.EventSegmentRecord(nil), content.Events.Segments...)
	removedSegmentID := incomplete.Events.Segments[len(incomplete.Events.Segments)-1].SegmentID
	incomplete.Events.Segments = incomplete.Events.Segments[:len(incomplete.Events.Segments)-1]
	incomplete.Events.Localizations = nil
	for _, localization := range content.Events.Localizations {
		if localization.SegmentID != removedSegmentID {
			incomplete.Events.Localizations = append(incomplete.Events.Localizations, localization)
		}
	}
	if err := destination.RestoreBackup(payload.Categories, payload.Events, incomplete.Entries, incomplete.Events,
		incomplete.Lyrics, true, "admin"); err == nil {
		t.Fatal("incomplete canonical restore unexpectedly succeeded")
	}
	legacyBefore, err := destination.GetEntries("cards", "prefix", "")
	if err != nil || len(legacyBefore) != 1 || legacyBefore[0].Text != "Before Chinese" {
		t.Fatalf("failed restore changed legacy content: %+v err=%v", legacyBefore, err)
	}
	englishBefore, err := destination.GetEntriesLocale("cards", "prefix", "", model.LocaleEnglish)
	if err != nil || len(englishBefore) != 1 || englishBefore[0].Text != "Before English" {
		t.Fatalf("failed restore changed additive content: %+v err=%v", englishBefore, err)
	}
	beforeEventContent, err := destination.ExportEventContent()
	if err != nil || len(beforeEventContent.Scenarios) != 1 || beforeEventContent.Scenarios[0].ScenarioID != "newer" {
		t.Fatalf("failed restore changed scenario snapshots=%+v err=%v", beforeEventContent.Scenarios, err)
	}

	if err := destination.RestoreBackup(payload.Categories, payload.Events, nil, store.EventContentExport{}, store.LyricsContentExport{}, false, "admin"); err != nil {
		t.Fatal(err)
	}
	englishAfter, err := destination.GetEntriesLocale("cards", "prefix", "", model.LocaleEnglish)
	if err != nil || len(englishAfter) != 1 || englishAfter[0].Text != "" {
		t.Fatalf("old backup retained English content: %+v err=%v", englishAfter, err)
	}
	if _, err := destination.GetLyrics(10); err != store.ErrLyricsNotFound {
		t.Fatalf("old backup retained lyrics: %v", err)
	}
	afterEventContent, err := destination.ExportEventContent()
	if err != nil || len(afterEventContent.Scenarios) != 0 {
		t.Fatalf("old backup retained scenarios=%+v err=%v", afterEventContent.Scenarios, err)
	}
	if exists, err := destinationEvents.Exists(99); err != nil || exists {
		t.Fatalf("complete restore retained newer event: exists=%v err=%v", exists, err)
	}
	var localeMetadata int
	if err := database.QueryRow(`SELECT COUNT(*) FROM event_story_locale_meta`).Scan(&localeMetadata); err != nil || localeMetadata != 0 {
		t.Fatalf("old backup retained locale metadata: count=%d err=%v", localeMetadata, err)
	}
	var audits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='backup.restore' AND user='admin'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("restore audit count=%d err=%v", audits, err)
	}
}

func TestRestoreRejectsCorruptOrIncompleteLegacyProjection(t *testing.T) {
	h := setupLegacyBackup(t)
	translations, _, err := h.manager.materializeBackupPayload(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(translations, "cards.full.json"), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importer.ValidateDir(translations); err == nil {
		t.Fatal("corrupt complete backup unexpectedly validated")
	}
	if err := os.Remove(filepath.Join(translations, "music.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := importer.ReadDir(translations); err == nil {
		t.Fatal("incomplete backup unexpectedly parsed")
	}
}

func TestRestoreRejectsEmptyProjectionAndDuplicateCategoryKeys(t *testing.T) {
	h := setupLegacyBackup(t)
	t.Run("empty projection", func(t *testing.T) {
		translations, err := h.manager.materializeTranslations(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, category := range model.SupportedCategories {
			for _, suffix := range []string{".json", ".full.json"} {
				if err := os.WriteFile(filepath.Join(translations, category+suffix), []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, _, err := importer.ReadDir(translations); err == nil || !strings.Contains(err.Error(), "restore category cards is empty") {
			t.Fatalf("empty restore error = %v", err)
		}
	})
	t.Run("incomplete event", func(t *testing.T) {
		translations, err := h.manager.materializeTranslations(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"meta":{},"episodes":{"1":{"scenarioId":"scenario-1","title":"title","talkData":{}}}}`)
		if err := os.WriteFile(filepath.Join(translations, "eventStory", "event_42.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := importer.ReadDir(translations); err == nil || !strings.Contains(err.Error(), "restore event 42 has incomplete metadata") {
			t.Fatalf("incomplete event restore error = %v", err)
		}
	})
	for _, test := range []struct {
		name string
		file string
		body string
	}{
		{"flat", "cards.json", `{"prefix":{"こんにちは":"你好"},"prefix":{"こんにちは":"你好"}}`},
		{"full", "cards.full.json", `{"prefix":{"こんにちは":{"text":"你好","source":"human"},"こんにちは":{"text":"你好","source":"human"}}}`},
	} {
		t.Run("duplicate "+test.name, func(t *testing.T) {
			translations, err := h.manager.materializeTranslations(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(translations, test.file), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := importer.ReadDir(translations); err == nil || !strings.Contains(err.Error(), "duplicate object key") {
				t.Fatalf("duplicate %s error = %v", test.name, err)
			}
		})
	}
}

func gitOutput(t *testing.T, gitDir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gitOutputBytes(t, gitDir, args...)))
}

func gitOutputBytes(t *testing.T, gitDir string, args ...string) []byte {
	t.Helper()
	allArgs := append([]string{"--git-dir", gitDir}, args...)
	cmd := exec.Command("git", allArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return out
}

func testBackupEncryptionKey(t *testing.T) []byte {
	t.Helper()
	key, err := loadBackupEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func decryptBackupArtifactToDir(t *testing.T, artifact []byte) string {
	t.Helper()
	key := testBackupEncryptionKey(t)
	archive, err := decryptBackupEnvelope(artifact, key)
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(archive)
	root := t.TempDir()
	if err := untarGz(archive, root); err != nil {
		t.Fatal(err)
	}
	return root
}
