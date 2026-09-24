package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// Every side-story string in these tests is synthetic test data.
var sideStoryBackupTestNow = time.Unix(1790000000, 0)

func sideStoryBackupTestScript(t *testing.T, scenarioID string, talks ...[2]string) *store.SideStoryScript {
	t.Helper()
	talkData := make([]any, len(talks))
	for index, talk := range talks {
		talkData[index] = map[string]any{"WindowDisplayName": talk[0], "Body": talk[1], "Voices": []any{}}
	}
	script, err := store.ParseSideStoryScript(map[string]any{
		"ScenarioId": scenarioID, "Snippets": []any{}, "TalkData": talkData,
		"SpecialEffectData": []any{}, "AppearCharacters": []any{},
	}, scenarioID)
	if err != nil {
		t.Fatal(err)
	}
	return &script
}

// seedBackupSideStories writes an imported, a failed and an unfetched
// episode with official, human, human-empty and revision-2 rows.
func seedBackupSideStories(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	card := store.SideStoryCatalogStory{
		Kind: store.SideStoryKindCard, StoryID: "100", Title: "テスト称号", CharacterID: 1, ReleasedAt: 1790000000000,
		Episodes: []store.SideStoryCatalogEpisode{
			{Key: "1", ScenarioID: "test_card_100_01", TitleJP: "テスト話一", Position: 1,
				JPAssetPath: "character/member/test_res_100/test_card_100_01",
				CNAssetPath: "character/member/test_res_100/test_card_100_01", CNTitle: "测试标题一"},
			{Key: "2", ScenarioID: "test_card_100_02", TitleJP: "テスト話二", Position: 2,
				JPAssetPath: "character/member/test_res_100/test_card_100_02"},
		},
	}
	area := store.SideStoryCatalogStory{
		Kind: store.SideStoryKindArea, StoryID: "test_area_001", Title: "テスト区域", AreaID: 3, AreaCategory: "grade1",
		ActionSetID: 501, ReleasedAt: 1790000000001,
		Episodes: []store.SideStoryCatalogEpisode{{Key: "1", ScenarioID: "test_area_001", JPAssetPath: "scenario/actionset/group5/test_area_001"}},
	}
	for _, story := range []store.SideStoryCatalogStory{card, area} {
		if _, err := s.SyncSideStoryCatalogContext(ctx, story.Kind, []store.SideStoryCatalogStory{story}, sideStoryBackupTestNow); err != nil {
			t.Fatal(err)
		}
	}
	jp := sideStoryBackupTestScript(t, "test_card_100_01", [2]string{"テスト話者", "テスト台詞一"}, [2]string{"テスト相手", "テスト台詞二"})
	cn := sideStoryBackupTestScript(t, "test_card_100_01", [2]string{"测试说话人", "测试台词一"}, [2]string{"测试对象", "测试台词二"})
	if _, err := s.ApplySideStoryFetchesContext(ctx, []store.SideStoryEpisodeFetch{
		{Kind: "card", StoryID: "100", EpisodeKey: "1",
			JP: store.SideStoryFetchOutcome{Attempted: true, Script: jp}, CN: store.SideStoryFetchOutcome{Attempted: true, Script: cn}},
		{Kind: "card", StoryID: "100", EpisodeKey: "2", JP: store.SideStoryFetchOutcome{Attempted: true, Err: "测试错误", Transient: true}},
	}, sideStoryBackupTestNow); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []struct {
		locale string
		edit   store.SideStoryLineEdit
	}{
		{"zh-CN", store.SideStoryLineEdit{JP: "テスト台詞二", Text: ""}},
		{"en-US", store.SideStoryLineEdit{JP: "テスト台詞一", Text: "Test line one"}},
		{"en-US", store.SideStoryLineEdit{JP: "テスト台詞一", Text: "Test line one, revised"}},
	} {
		if _, err := s.UpdateSideStoryLinesContext(ctx, "card", "100", "1", edit.locale, "test-editor",
			[]store.SideStoryLineEdit{edit.edit}, sideStoryBackupTestNow.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}

// sideStoryRows renders every column of the four side-story tables with its
// driver type.
func sideStoryRows(t *testing.T, database *db.DB) []string {
	t.Helper()
	var dump []string
	for _, table := range []struct{ name, order string }{
		{"side_stories", "kind,story_id"},
		{"side_story_episodes", "kind,story_id,episode_key"},
		{"side_story_lines", "kind,story_id,episode_key,jp_key"},
		{"side_story_line_localizations", "kind,story_id,episode_key,jp_key,locale"},
	} {
		rows, err := database.Query(`SELECT * FROM ` + table.name + ` ORDER BY ` + table.order)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			parts := []string{table.name}
			for index, value := range values {
				parts = append(parts, fmt.Sprintf("%s=%T:%q", columns[index], value, fmt.Sprint(value)))
			}
			dump = append(dump, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return dump
}

func configureFakeS3(t *testing.T, h *legacyBackupHarness) *fakeS3Bucket {
	t.Helper()
	bucket, endpoint := newFakeS3Bucket(t)
	for key, value := range map[string]string{
		config.KeyBackupS3Endpoint:  endpoint,
		config.KeyBackupS3Region:    "test-region",
		config.KeyBackupS3Bucket:    "test-bucket",
		config.KeyBackupS3Prefix:    "snapshots",
		config.KeyBackupS3AccessKey: "test-access",
		config.KeyBackupS3SecretKey: "test-secret",
	} {
		if err := h.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(backupEncryptionKeyEnv, "")
	return bucket
}

const sideStoryTestLatestArchive = "/test-bucket/snapshots/latest.tar.gz"

func readContentManifest(t *testing.T, dir string) contentManifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest contentManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeContentManifest(t *testing.T, dir string, manifest contentManifest) {
	t.Helper()
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestS3BackupCarriesSideStoriesThroughTheManager(t *testing.T) {
	h := setupLegacyBackup(t)
	seedBackupSideStories(t, h.store)
	want := sideStoryRows(t, h.database)
	bucket := configureFakeS3(t, h)
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}
	rewriteLatestS3Archive(t, bucket, sideStoryTestLatestArchive, func(root string) {
		manifest := readContentManifest(t, filepath.Join(root, "translation-content"))
		if manifest.SchemaVersion != 2 || len(manifest.Files) != 4 {
			t.Fatalf("archived manifest = %+v", manifest)
		}
		var sideStories *contentManifestFile
		for index := range manifest.Files {
			if manifest.Files[index].Path == "side-stories.json" {
				sideStories = &manifest.Files[index]
			}
		}
		if sideStories == nil || sideStories.Count != len(want) {
			t.Fatalf("side-stories manifest entry = %+v, want count %d", sideStories, len(want))
		}
	})

	if _, err := h.store.UpdateSideStoryLinesContext(context.Background(), "card", "100", "1", "zh-CN", "test-editor",
		[]store.SideStoryLineEdit{{JP: "テスト台詞一", Text: "测试覆盖"}}, sideStoryBackupTestNow.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.SyncSideStoryCatalogContext(context.Background(), "card", []store.SideStoryCatalogStory{{
		Kind: "card", StoryID: "200", Title: "テスト新称号",
		Episodes: []store.SideStoryCatalogEpisode{{Key: "1", ScenarioID: "test_card_200_01", JPAssetPath: "character/member/test_res_200/test_card_200_01"}},
	}}, sideStoryBackupTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.restoreS3(); err != nil {
		t.Fatal(err)
	}
	got := sideStoryRows(t, h.database)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("side stories after S3 restore\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestS3RestoreOfSchemaVersion1BackupClearsSideStories(t *testing.T) {
	h := setupLegacyBackup(t)
	seedBackupSideStories(t, h.store)
	bucket := configureFakeS3(t, h)
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}
	rewriteLatestS3Archive(t, bucket, sideStoryTestLatestArchive, func(root string) {
		dir := filepath.Join(root, "translation-content")
		if err := os.Remove(filepath.Join(dir, "side-stories.json")); err != nil {
			t.Fatal(err)
		}
		manifest := readContentManifest(t, dir)
		legacy := contentManifest{SchemaVersion: 1}
		for _, file := range manifest.Files {
			if file.Path != "side-stories.json" {
				legacy.Files = append(legacy.Files, file)
			}
		}
		writeContentManifest(t, dir, legacy)
	})
	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "被覆盖", model.SourceLLM, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.restoreS3(); err != nil {
		t.Fatal(err)
	}
	if rows := sideStoryRows(t, h.database); len(rows) != 0 {
		t.Fatalf("schemaVersion 1 restore kept side stories:\n%s", strings.Join(rows, "\n"))
	}
	cards, err := h.store.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	if got := cards["prefix"]["こんにちは"]; got.Text != "你好" {
		t.Fatalf("schemaVersion 1 restore entry = %+v", got)
	}
}

func TestTranslationContentManifestFileSetMatchesItsSchemaVersion(t *testing.T) {
	h := setupLegacyBackup(t)
	seedBackupSideStories(t, h.store)
	dir, err := materializeTranslationContentFromStore(t.TempDir(), h.store)
	if err != nil {
		t.Fatal(err)
	}
	current := readContentManifest(t, dir)
	withFiles := func(version int, keep func(contentManifestFile) bool) contentManifest {
		manifest := contentManifest{SchemaVersion: version}
		for _, file := range current.Files {
			if keep(file) {
				manifest.Files = append(manifest.Files, file)
			}
		}
		return manifest
	}
	all := func(contentManifestFile) bool { return true }
	withoutSideStories := func(file contentManifestFile) bool { return file.Path != "side-stories.json" }
	withoutLyrics := func(file contentManifestFile) bool { return file.Path != "lyrics.json" }
	miscounted := withFiles(2, all)
	for index := range miscounted.Files {
		if miscounted.Files[index].Path == "side-stories.json" {
			miscounted.Files[index].Count++
		}
	}
	for _, test := range []struct {
		name     string
		manifest contentManifest
		want     string
	}{
		{"schemaVersion 2 with the three old files", withFiles(2, withoutSideStories), "schemaVersion 2 manifest must contain exactly 4 files, found 3"},
		{"schemaVersion 1 with four files", withFiles(1, all), "schemaVersion 1 manifest must contain exactly 3 files, found 4"},
		{"schemaVersion 1 naming side stories", withFiles(1, withoutLyrics), `invalid translation content manifest path "side-stories.json"`},
		{"unknown schemaVersion", withFiles(3, all), "unsupported translation content schemaVersion 3"},
		{"side-story count", miscounted, "count mismatch: side-stories.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeContentManifest(t, dir, test.manifest)
			if _, present, err := readTranslationContent(dir); err == nil || !present || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("present=%v err=%v, want %q", present, err, test.want)
			}
		})
	}

	writeContentManifest(t, dir, withFiles(1, withoutSideStories))
	legacy, present, err := readTranslationContent(dir)
	if err != nil || !present {
		t.Fatalf("schemaVersion 1 present=%v err=%v", present, err)
	}
	if sideStoryContentCount(legacy.SideStories) != 0 || len(legacy.Events.Segments) == 0 {
		t.Fatalf("schemaVersion 1 content = %+v", legacy)
	}
	writeContentManifest(t, dir, current)
	content, present, err := readTranslationContent(dir)
	if err != nil || !present {
		t.Fatalf("schemaVersion 2 present=%v err=%v", present, err)
	}
	if got := sideStoryContentCount(content.SideStories); got != len(sideStoryRows(t, h.database)) || got == 0 {
		t.Fatalf("schemaVersion 2 side-story records = %d", got)
	}
}

func TestSideStoriesContentFileHasItsOwnSizeLimit(t *testing.T) {
	for _, name := range []string{"translation-content/side-stories.json", "translations/translation-content/side-stories.json"} {
		if got := archiveFileByteLimit(name); got != maxSideStoriesContentFileBytes {
			t.Fatalf("side-story content %q limit = %d", name, got)
		}
	}
	for _, name := range []string{"translation-content/nested/side-stories.json", "translation-content/side-stories.json.bak", "translations/side-stories.json"} {
		if got := archiveFileByteLimit(name); got != maxArchiveFileBytes {
			t.Fatalf("ordinary file %q limit = %d", name, got)
		}
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "translations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "translation-content"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "translation-content", "side-stories.json")
	// About the indented size at production scale (contract §3).
	const productionScaleBytes = 160 << 20
	for _, test := range []struct {
		size int64
		ok   bool
	}{{productionScaleBytes, true}, {maxSideStoriesContentFileBytes + 1, false}} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, test.size); err != nil {
			t.Fatal(err)
		}
		err := validateGitRestoreTree(root)
		if test.ok && err != nil {
			t.Fatalf("side-stories.json of %d bytes rejected: %v", test.size, err)
		}
		if !test.ok && (err == nil || !strings.Contains(err.Error(), "exceeds")) {
			t.Fatalf("side-stories.json of %d bytes error = %v", test.size, err)
		}
	}
}

func TestTranslationContentRecordLimitLeavesRoomForProductionScaleSideStories(t *testing.T) {
	array := func(count int) string {
		if count == 0 {
			return "[]"
		}
		return "[" + strings.Repeat("{},", count-1) + "{}]"
	}
	const stories, episodes, lines, rows = 4_300, 5_700, 150_000, 300_000
	body := []byte(`{"stories":` + array(stories) + `,"episodes":` + array(episodes) +
		`,"lines":` + array(lines) + `,"localizations":` + array(rows) + `}`)
	// The other three files keep the 1M records they were allowed before.
	count, scenarios, total, err := preflightTranslationContentJSON("side-stories.json", body, maxTranslationContentRecords-1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if count != stories+episodes+lines+rows || scenarios != 0 || total != count {
		t.Fatalf("side-story preflight count=%d scenarios=%d total=%d", count, scenarios, total)
	}
	if _, _, _, err := preflightTranslationContentJSON("side-stories.json", []byte(`[]`), maxTranslationContentRecords); err == nil ||
		!strings.Contains(err.Error(), "top level must be an object") {
		t.Fatalf("array side-stories.json error = %v", err)
	}
}
