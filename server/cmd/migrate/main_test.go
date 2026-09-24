package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
	"moesekai/server/internal/singleinstance"
	"moesekai/server/internal/store"
)

func TestSeedMigrationRejectsMissingMalformedAndEmptyExpectedObjects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{"missing category", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "cards.full.json")); err != nil {
				t.Fatal(err)
			}
		}, "missing cards.full.json"},
		{"malformed category", func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "music.full.json"), []byte(`{"title":`))
		}, "music.full.json"},
		{"empty category", func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "gacha.json"), []byte(`{}`))
			writeTestFile(t, filepath.Join(root, "gacha.full.json"), []byte(`{}`))
		}, "seed category gacha is empty"},
		{"missing events", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "eventStory", "event_42.json")); err != nil {
				t.Fatal(err)
			}
		}, "no event stories"},
		{"malformed event", func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "eventStory", "event_42.json"), []byte(`{"meta":{},"episodes":{}}`))
		}, "incomplete metadata"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeCompleteSeed(t)
			test.mutate(t, root)
			databasePath := filepath.Join(t.TempDir(), "moesekai.db")
			err := run(root, databasePath, true)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("migration error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed migration published database: %v", statErr)
			}
		})
	}
}

func TestSeedMigrationFailureCleansStagingAndRestartRetries(t *testing.T) {
	root := writeCompleteSeed(t)
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")
	migrationObjectImportedHook = func(count int) error {
		if count == 4 {
			return errors.New("stop mid-import")
		}
		return nil
	}
	err := run(root, databasePath, true)
	migrationObjectImportedHook = nil
	if err == nil || !strings.Contains(err.Error(), "stop mid-import") {
		t.Fatalf("injected migration error = %v", err)
	}
	if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed migration published database: %v", statErr)
	}
	staging, err := filepath.Glob(filepath.Join(filepath.Dir(databasePath), ".moesekai.db.seed-*.db*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("staging debris = %v, err=%v", staging, err)
	}
	if err := run(root, databasePath, true); err != nil {
		t.Fatalf("restart did not retry cleanly: %v", err)
	}
	assertCompleteMigratedEvent(t, databasePath)
	assertPrivateDatabaseMode(t, databasePath)
	staging, err = filepath.Glob(filepath.Join(filepath.Dir(databasePath), ".moesekai.db.seed-*.db*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("successful migration staging debris = %v, err=%v", staging, err)
	}
}

func TestPostRenameDirectorySyncFailureNeverLeavesAcceptedTarget(t *testing.T) {
	root := writeCompleteSeed(t)
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")
	migrationPostRenameDirSyncHook = func(string) error { return errors.New("injected post-rename directory sync failure") }
	err := run(root, databasePath, true)
	migrationPostRenameDirSyncHook = nil
	if err == nil || !strings.Contains(err.Error(), "injected post-rename") {
		t.Fatalf("post-rename sync error = %v", err)
	}
	if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unconfirmed target remained visible: %v", statErr)
	}
	marker := databasePath + ".seed-incomplete"
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("durable incomplete marker missing: %v", statErr)
	}
	if err := run(root, databasePath, true); err != nil {
		t.Fatalf("safe retry did not remove target-free marker: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("successful retry retained marker: %v", statErr)
	}
	assertCompleteMigratedEvent(t, databasePath)
}

func TestExistingTargetWithIncompleteMarkerIsFatal(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")
	writeTestFile(t, databasePath, []byte("unconfirmed"))
	writeTestFile(t, databasePath+".seed-incomplete", []byte("incomplete"))
	err := run(writeCompleteSeed(t), databasePath, true)
	if err == nil || !strings.Contains(err.Error(), "incomplete seed marker exists") {
		t.Fatalf("incomplete target error = %v", err)
	}
	if got, readErr := os.ReadFile(databasePath); readErr != nil || string(got) != "unconfirmed" {
		t.Fatalf("incomplete target was modified: %q err=%v", got, readErr)
	}
}

func TestSeedMigrationAndServerUseOneDatabaseOwner(t *testing.T) {
	root := writeCompleteSeed(t)
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")

	serverOwner, err := singleinstance.Acquire(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(root, databasePath, true); !errors.Is(err, singleinstance.ErrAlreadyOwned) {
		serverOwner.Close()
		t.Fatalf("migration while server owns lock = %v", err)
	}
	if err := serverOwner.Close(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	migrationOwnershipAcquiredHook = func() {
		close(entered)
		<-release
	}
	migrationDone := make(chan error, 1)
	go func() { migrationDone <- run(root, databasePath, true) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		migrationErr := <-migrationDone
		migrationOwnershipAcquiredHook = nil
		t.Fatalf("migration did not report acquired ownership: %v", migrationErr)
	}
	competingServer, err := singleinstance.Acquire(databasePath)
	if competingServer != nil {
		competingServer.Close()
	}
	if !errors.Is(err, singleinstance.ErrAlreadyOwned) {
		close(release)
		migrationErr := <-migrationDone
		migrationOwnershipAcquiredHook = nil
		t.Fatalf("server while migration owns lock = %v; migration error = %v", err, migrationErr)
	}
	close(release)
	if err := <-migrationDone; err != nil {
		migrationOwnershipAcquiredHook = nil
		t.Fatal(err)
	}
	migrationOwnershipAcquiredHook = nil
	assertCompleteMigratedEvent(t, databasePath)
}

func TestSeedMigrationNeverReplacesExistingDatabase(t *testing.T) {
	root := writeCompleteSeed(t)
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")
	original := []byte("existing")
	writeTestFile(t, databasePath, original)
	if err := run(root, databasePath, true); !errors.Is(err, errTargetExists) {
		t.Fatalf("existing target error = %v", err)
	}
	got, err := os.ReadFile(databasePath)
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("existing target changed: %q err=%v", got, err)
	}
}

func TestSeedMigrationVerificationCannotBeDisabled(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")
	if err := run(writeCompleteSeed(t), databasePath, false); err == nil || !strings.Contains(err.Error(), "cannot be disabled") {
		t.Fatalf("disabled verification error = %v", err)
	}
	if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled verification created database: %v", err)
	}
}

func TestSeedMigrationAcceptsSeedPredatingGachaInfo(t *testing.T) {
	root := writeCompleteSeed(t)
	for _, name := range []string{"gachaInfo.json", "gachaInfo.full.json"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(t.TempDir(), "predating-gacha-info.db")
	if err := run(root, databasePath, true); err != nil {
		t.Fatalf("seed without gachaInfo: %v", err)
	}
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	gachaInfo, err := s.CategoryData("gachaInfo")
	if err != nil {
		t.Fatal(err)
	}
	if len(normalizeCategory(gachaInfo)) != 0 {
		t.Fatalf("gachaInfo after seed without it = %#v", gachaInfo)
	}
	gacha, err := s.CategoryData("gacha")
	if err != nil {
		t.Fatal(err)
	}
	if got := gacha["name"]["jp-gacha"]; got.Text != "translated-gacha" || got.Source != model.SourceHuman {
		t.Fatalf("gacha after seed without gachaInfo = %#v", gacha)
	}
}

func TestSeedMigrationRejectsHalfPresentGachaInfo(t *testing.T) {
	root := writeCompleteSeed(t)
	if err := os.Remove(filepath.Join(root, "gachaInfo.full.json")); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "half-gacha-info.db")
	if err := run(root, databasePath, true); err == nil || !strings.Contains(err.Error(), "gachaInfo.full.json") {
		t.Fatalf("half-present gachaInfo error = %v", err)
	}
	if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed migration published database: %v", err)
	}
}

func TestKnownGachaFlatFullDiscrepancyUsesLosslessNonconflictingUnion(t *testing.T) {
	root := writeCompleteSeed(t)
	for _, name := range []string{"gacha.json", "gacha.full.json"} {
		fixture, err := os.ReadFile(filepath.Join("testdata", "legacy-gacha-discrepancy", name))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(root, name), fixture)
	}
	databasePath := filepath.Join(t.TempDir(), "gacha-union.db")
	if err := run(root, databasePath, true); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	got, err := store.New(database).CategoryData("gacha")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalizeCategory(got), normalizeCategory(model.Category{"name": {
		"shared":          {Text: "shared text", Source: model.SourceHuman, Ids: []string{"shared-id"}},
		"flat-only-gacha": {Text: "flat-only text", Source: model.SourceUnknown},
		"full-only-gacha": {Text: "full-only text", Source: model.SourcePinned, Ids: []string{"full-only-id"}},
	}})) {
		t.Fatalf("gacha union lost source data: %#v", got)
	}
}

func TestSeedMigrationRejectsConflictingFlatFullText(t *testing.T) {
	root := writeCompleteSeed(t)
	writeJSONTestFile(t, filepath.Join(root, "gacha.json"), map[string]map[string]string{"name": {"same": "flat"}})
	writeJSONTestFile(t, filepath.Join(root, "gacha.full.json"), model.Category{"name": {
		"same": {Text: "full", Source: model.SourceHuman},
	}})
	databasePath := filepath.Join(t.TempDir(), "conflict.db")
	err := run(root, databasePath, true)
	if err == nil || !strings.Contains(err.Error(), "flat/full conflict") {
		t.Fatalf("flat/full conflict error = %v", err)
	}
	if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("conflicting seed published database: %v", statErr)
	}
}

func TestSeedMigrationRejectsNonGachaFlatFullDiscrepancy(t *testing.T) {
	root := writeCompleteSeed(t)
	writeJSONTestFile(t, filepath.Join(root, "cards.json"), map[string]map[string]string{"name": {
		"shared": "shared text", "flat-only-card": "unexpected text",
	}})
	writeJSONTestFile(t, filepath.Join(root, "cards.full.json"), model.Category{"name": {
		"shared": {Text: "shared text", Source: model.SourceHuman},
	}})
	databasePath := filepath.Join(t.TempDir(), "non-gacha-discrepancy.db")
	err := run(root, databasePath, true)
	if err == nil || !strings.Contains(err.Error(), "cards flat/full projections do not match") {
		t.Fatalf("non-gacha discrepancy error = %v", err)
	}
	if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("non-gacha discrepancy published database: %v", statErr)
	}
}

func TestMigrateCommandFailureRestartConcurrencyAndMetadata(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "moesekai-migrate")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build migrate command: %v\n%s", err, output)
	}
	root := writeCompleteSeed(t)
	databasePath := filepath.Join(t.TempDir(), "moesekai.db")
	arguments := []string{"-src", root, "-db", databasePath}

	owner, err := singleinstance.Acquire(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, arguments...)
	output, commandErr := command.CombinedOutput()
	if closeErr := owner.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if commandErr == nil || !strings.Contains(string(output), singleinstance.ErrAlreadyOwned.Error()) {
		t.Fatalf("command under competing owner error=%v output=%s", commandErr, output)
	}

	command = exec.Command(binary, arguments...)
	command.Env = append(envWithout("MOESEKAI_MIGRATE_FAIL_AFTER_OBJECTS"), "MOESEKAI_MIGRATE_FAIL_AFTER_OBJECTS=4")
	output, commandErr = command.CombinedOutput()
	if commandErr == nil || !strings.Contains(string(output), "injected failure after object 4") {
		t.Fatalf("command injected failure error=%v output=%s", commandErr, output)
	}
	if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed command published database: %v", err)
	}
	staging, err := filepath.Glob(filepath.Join(filepath.Dir(databasePath), ".moesekai.db.seed-*.db*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("failed command staging debris=%v err=%v", staging, err)
	}

	command = exec.Command(binary, arguments...)
	command.Env = envWithout("MOESEKAI_MIGRATE_FAIL_AFTER_OBJECTS")
	output, commandErr = command.CombinedOutput()
	if commandErr != nil || !strings.Contains(string(output), "integrity_check=ok") {
		t.Fatalf("command restart error=%v output=%s", commandErr, output)
	}
	assertCompleteMigratedEvent(t, databasePath)
}

func assertPrivateDatabaseMode(t *testing.T, databasePath string) {
	t.Helper()
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("seed database mode=%#o want %#o", got, os.FileMode(0o600))
	}
	parentInfo, err := os.Stat(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("seed database parent mode=%#o want %#o", got, os.FileMode(0o700))
	}
}

func envWithout(name string) []string {
	prefix := name + "="
	environment := os.Environ()
	filtered := environment[:0]
	for _, value := range environment {
		if !strings.HasPrefix(value, prefix) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func assertCompleteMigratedEvent(t *testing.T, databasePath string) {
	t.Helper()
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	detail, err := store.NewEventStore(database).OrderedDetail(42)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Meta != (model.EventStoryMeta{Source: "official_cn", Version: "2.5", LastUpdated: 1700000000}) {
		t.Fatalf("event metadata = %+v", detail.Meta)
	}
	if len(detail.Episodes) != 1 {
		t.Fatalf("event episodes = %+v", detail.Episodes)
	}
	episode := detail.Episodes[0]
	if episode.EpisodeNo != "1" || episode.ScenarioID != "scenario-42" || episode.Title != "标题" || episode.TitleSource != model.SourceHuman {
		t.Fatalf("event episode metadata = %+v", episode)
	}
	if !reflect.DeepEqual(episode.TalkKeys, []string{"second", "first"}) ||
		!reflect.DeepEqual(episode.TalkData, map[string]string{"first": "第一句", "second": "第二句"}) ||
		!reflect.DeepEqual(episode.TalkSources, map[string]string{"first": model.SourcePinned, "second": model.SourceHuman}) ||
		!reflect.DeepEqual(episode.SpeakerNames, map[string]string{"second": "角色"}) {
		t.Fatalf("event line representation = %+v", episode)
	}
	if err := database.IntegrityCheck(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func writeCompleteSeed(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, category := range model.SupportedCategories {
		key := "jp-" + category
		text := "translated-" + category
		flat := map[string]map[string]string{"name": {key: text}}
		full := model.Category{"name": {key: {Text: text, Source: model.SourceHuman, Ids: []string{"id-" + category}}}}
		writeJSONTestFile(t, filepath.Join(root, category+".json"), flat)
		writeJSONTestFile(t, filepath.Join(root, category+".full.json"), full)
	}
	eventDir := filepath.Join(root, "eventStory")
	if err := os.MkdirAll(eventDir, 0o700); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{
		"meta": map[string]any{"source": "official_cn", "version": "2.5", "last_updated": 1700000000},
		"episodes": map[string]any{
			"1": map[string]any{
				"scenarioId": "scenario-42", "title": "标题", "titleSource": model.SourceHuman,
				"talkData":     map[string]string{"first": "第一句", "second": "第二句"},
				"talkOrder":    []string{"second", "first"},
				"talkSources":  map[string]string{"first": model.SourcePinned, "second": model.SourceHuman},
				"speakerNames": map[string]string{"second": "角色"},
			},
		},
	}
	writeJSONTestFile(t, filepath.Join(eventDir, "event_42.json"), event)
	return root
}

func writeJSONTestFile(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, body)
}

func writeTestFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
