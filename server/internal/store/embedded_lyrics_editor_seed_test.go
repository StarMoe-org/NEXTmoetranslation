package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/model"
)

func TestApplyEmbeddedLyricsEditorSeedDefersEmptyCatalogWithoutChanges(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "embedded-editor-seed-empty-catalog.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := New(database)
	if err := database.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeHash := embeddedLyricsEditorFileSHA256(t, path)
	result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if !errors.Is(err, ErrEmbeddedLyricsEditorSeedCatalogNotReady) {
		t.Fatalf("empty catalog result=%+v error=%v", result, err)
	}
	if err := database.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if afterHash := embeddedLyricsEditorFileSHA256(t, path); afterHash != beforeHash {
		t.Fatalf("empty catalog changed SQLite main bytes before=%s after=%s", beforeHash, afterHash)
	}
	assertEmbeddedLyricsEditorSeedCounts(t, database, 0, 0, 0, 0)
}

func TestApplyEmbeddedLyricsEditorSeedImportsReal700AndReplaysAfterRestart(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "embedded-editor-seed.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(database)
	seedEmbeddedLyricsEditorCatalog(t, database, bundle, false)
	seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)
	seedEmbeddedLyricsEditorProtectedRows(t, database, bundle.Manifest.Items[0])
	protectedBefore := snapshotEmbeddedLyricsEditorProtectedRows(t, database)

	result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if result.SeedSHA256 != bundle.Manifest.SeedSHA256 || result.Inserted != embeddedlyricsseed.ExpectedCatalogCount ||
		result.PreservedExisting != 0 || result.Replayed != 0 || result.SourceV3 != embeddedlyricsseed.ExpectedSourceV3 ||
		result.Legacy != embeddedlyricsseed.ExpectedLegacy || result.Availability != embeddedlyricsseed.ExpectedAvailability {
		database.Close()
		t.Fatalf("initial embedded editor seed result=%+v", result)
	}
	if after := snapshotEmbeddedLyricsEditorProtectedRows(t, database); !reflect.DeepEqual(after, protectedBefore) {
		database.Close()
		t.Fatalf("embedded editor seed changed protected rows\nbefore=%+v\nafter=%+v", protectedBefore, after)
	}
	assertEmbeddedLyricsEditorSeedCounts(t, database, embeddedlyricsseed.ExpectedSourceV3, embeddedlyricsseed.ExpectedLegacy,
		embeddedlyricsseed.ExpectedAvailability, embeddedlyricsseed.ExpectedCatalogCount)

	list, err := s.ListLyrics(1000, 0)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if len(list.Items) != embeddedlyricsseed.ExpectedSourceV3+embeddedlyricsseed.ExpectedLegacy || list.NextCursor != "" {
		database.Close()
		t.Fatalf("seeded editable lyrics list count=%d next=%q", len(list.Items), list.NextCursor)
	}

	checkedKinds := map[string]int{}
	for index, item := range bundle.Manifest.Items {
		if index >= 5 && index < len(bundle.Manifest.Items)-5 && index%20 != 0 {
			continue
		}
		document, err := s.GetLyricsDocument(item.MusicID)
		switch item.SeedKind {
		case "availability":
			if !errors.Is(err, ErrLyricsNotFound) || document != nil {
				database.Close()
				t.Fatalf("availability music %d detail=%T err=%v", item.MusicID, document, err)
			}
			checkedKinds[item.SeedKind]++
		case "source_v3":
			if err != nil {
				database.Close()
				t.Fatalf("source-v3 music %d detail: %v", item.MusicID, err)
			}
			if _, ok := document.(LyricsRenditionDocument); !ok {
				database.Close()
				t.Fatalf("source-v3 music %d detail type=%T", item.MusicID, document)
			}
			checkedKinds[item.SeedKind]++
		case "legacy":
			if err != nil {
				database.Close()
				t.Fatalf("legacy music %d detail: %v", item.MusicID, err)
			}
			if _, ok := document.(model.SongLyrics); !ok {
				database.Close()
				t.Fatalf("legacy music %d detail type=%T", item.MusicID, document)
			}
			checkedKinds[item.SeedKind]++
		default:
			database.Close()
			t.Fatalf("unsupported test seed kind %q", item.SeedKind)
		}
	}
	if checkedKinds["source_v3"] == 0 || checkedKinds["legacy"] == 0 || checkedKinds["availability"] == 0 {
		database.Close()
		t.Fatalf("did not check all seed kinds: %+v", checkedKinds)
	}

	catalog, err := s.CatalogMusic("", false, 1000, 0)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	catalogDrafts, catalogAvailability := 0, 0
	for _, item := range catalog.Items {
		if item.LyricsStatus == "draft" {
			catalogDrafts++
		}
		if item.LyricsAvailabilityState != "" {
			catalogAvailability++
			if item.LyricsStatus != "" {
				database.Close()
				t.Fatalf("availability music %d also reported lyrics status %q", item.MusicID, item.LyricsStatus)
			}
		}
	}
	if len(catalog.Items) != embeddedlyricsseed.ExpectedCatalogCount ||
		catalogDrafts != embeddedlyricsseed.ExpectedSourceV3+embeddedlyricsseed.ExpectedLegacy ||
		catalogAvailability != embeddedlyricsseed.ExpectedAvailability {
		database.Close()
		t.Fatalf("catalog items=%d drafts=%d availability=%d", len(catalog.Items), catalogDrafts, catalogAvailability)
	}

	var sourceMusicID int
	for _, doc := range bundle.Documents {
		if p, err := s.GetLyricsRenditionDocument(doc.MusicID); err == nil && len(p.Renditions) > 0 && len(p.Renditions[0].Performers) > 0 && p.Renditions[0].Full != nil {
			sourceMusicID = doc.MusicID
			break
		}
	}
	if sourceMusicID == 0 {
		sourceMusicID = bundle.Documents[0].MusicID
	}
	plural, err := s.GetLyricsRenditionDocument(sourceMusicID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	plural.Renditions[0].TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: "embedded seed test translator"}
	if len(plural.Renditions[0].Performers) > 0 && plural.Renditions[0].Full != nil && len(plural.Renditions[0].Full.Lines) > 0 && len(plural.Renditions[0].Full.Lines[0].Segments) > 0 {
		plural.Renditions[0].Full.Lines[0].Segments[0].PerformerIDs = []string{plural.Renditions[0].Performers[0].PerformerID}
	}
	savedPlural, changed, err := s.SaveLyricsRenditionMutation(plural, "embedded-seed-test")
	if err != nil || !changed || savedPlural.Revision != 2 {
		database.Close()
		t.Fatalf("save seeded source-v3 music %d changed=%t revision=%d err=%v", sourceMusicID, changed, savedPlural.Revision, err)
	}
	legacyMusicID := bundle.LegacyDocuments[0].MusicID
	legacyAny, err := s.GetLyricsDocument(legacyMusicID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	legacy, ok := legacyAny.(model.SongLyrics)
	if !ok || len(legacy.Lines) == 0 {
		database.Close()
		t.Fatalf("seeded legacy music %d detail=%T", legacyMusicID, legacyAny)
	}
	legacy.Lines[0].Chinese = "嵌入种子可编辑验证"
	savedLegacy, changed, err := s.SaveLyricsMutation(legacy, "embedded-seed-test")
	if err != nil || !changed || savedLegacy.Revision != legacy.Revision+1 || savedLegacy.Lines[0].Chinese != "嵌入种子可编辑验证" {
		database.Close()
		t.Fatalf("save seeded legacy music %d changed=%t saved=%+v err=%v", legacyMusicID, changed, savedLegacy, err)
	}

	if err := database.Checkpoint(context.Background()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := New(reopened)
	replay, err := restarted.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Inserted != 0 || replay.PreservedExisting != 0 || replay.Replayed != embeddedlyricsseed.ExpectedCatalogCount ||
		replay.SourceV3 != 0 || replay.Legacy != 0 || replay.Availability != 0 {
		t.Fatalf("restart replay result=%+v", replay)
	}
	persistedPlural, err := restarted.GetLyricsRenditionDocument(sourceMusicID)
	if err != nil || persistedPlural.Revision != savedPlural.Revision || persistedPlural.Renditions[0].TranslationCredits == nil ||
		persistedPlural.Renditions[0].TranslationCredits.Translation != "embedded seed test translator" {
		t.Fatalf("replayed source-v3 localization=%+v err=%v", persistedPlural, err)
	}
	persistedLegacy, err := restarted.GetLyrics(legacyMusicID)
	if err != nil || persistedLegacy.Revision != savedLegacy.Revision || persistedLegacy.Lines[0].Chinese != "嵌入种子可编辑验证" {
		t.Fatalf("replayed legacy localization=%+v err=%v", persistedLegacy, err)
	}
}

func TestApplyEmbeddedLyricsEditorSeedPreservesExistingOwnershipAndBusinessRows(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "embedded-editor-seed-preserve.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := New(database)
	seedEmbeddedLyricsEditorCatalog(t, database, bundle, false)
	seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)

	var preserved embeddedlyricsseed.CatalogItem
	for _, item := range bundle.Manifest.Items {
		if item.SeedKind == "source_v3" {
			preserved = item
			break
		}
	}
	if preserved.MusicID == 0 {
		t.Fatal("embedded seed has no source-v3 item")
	}
	seedEmbeddedLyricsEditorExistingLyrics(t, database, preserved.MusicID)
	seedEmbeddedLyricsEditorProtectedRows(t, database, preserved)
	if _, err := database.Exec(`INSERT INTO song_lyrics_publications(music_id,revision,updated_at,payload_json)
		VALUES (?,1,101,'{"existing":true}')`, preserved.MusicID); err != nil {
		t.Fatal(err)
	}
	protectedBefore := snapshotEmbeddedLyricsEditorProtectedRows(t, database)
	var existingSourceHash, existingJapanese, existingPayload string
	if err := database.QueryRow(`SELECT source_hash FROM song_lyrics WHERE music_id=?`, preserved.MusicID).Scan(&existingSourceHash); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT japanese FROM song_lyric_lines WHERE music_id=?`, preserved.MusicID).Scan(&existingJapanese); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, preserved.MusicID).Scan(&existingPayload); err != nil {
		t.Fatal(err)
	}

	result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != embeddedlyricsseed.ExpectedCatalogCount-1 || result.PreservedExisting != 1 || result.Replayed != 0 ||
		result.SourceV3 != embeddedlyricsseed.ExpectedSourceV3-1 || result.Legacy != embeddedlyricsseed.ExpectedLegacy ||
		result.Availability != embeddedlyricsseed.ExpectedAvailability {
		t.Fatalf("preserving embedded editor seed result=%+v", result)
	}
	if after := snapshotEmbeddedLyricsEditorProtectedRows(t, database); !reflect.DeepEqual(after, protectedBefore) {
		t.Fatalf("preserving seed changed protected rows\nbefore=%+v\nafter=%+v", protectedBefore, after)
	}
	var sourceHash, japanese, payload, applyStatus string
	if err := database.QueryRow(`SELECT source_hash FROM song_lyrics WHERE music_id=?`, preserved.MusicID).Scan(&sourceHash); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT japanese FROM song_lyric_lines WHERE music_id=?`, preserved.MusicID).Scan(&japanese); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, preserved.MusicID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT apply_status FROM embedded_lyrics_editor_seed_items WHERE seed_sha256=? AND music_id=?`,
		bundle.Manifest.SeedSHA256, preserved.MusicID).Scan(&applyStatus); err != nil {
		t.Fatal(err)
	}
	if sourceHash != existingSourceHash || japanese != existingJapanese || payload != existingPayload || applyStatus != "preserved_existing" {
		t.Fatalf("existing ownership changed sourceHash=%q japanese=%q payload=%q applyStatus=%q", sourceHash, japanese, payload, applyStatus)
	}
	var seededSourceDocuments int
	if err := database.QueryRow(`SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=?`, preserved.MusicID).Scan(&seededSourceDocuments); err != nil {
		t.Fatal(err)
	}
	if seededSourceDocuments != 0 {
		t.Fatalf("preserved music %d received source-v3 rows", preserved.MusicID)
	}
}

func TestApplyEmbeddedLyricsEditorSeedPreservesExistingRecoveryAvailability(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "embedded-editor-seed-recovery-availability.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := New(database)
	seedEmbeddedLyricsEditorCatalog(t, database, bundle, false)
	var preserved embeddedlyricsseed.CatalogItem
	var availability embeddedlyricsseed.AvailabilityRecord
	for _, item := range bundle.Manifest.Items {
		if item.SeedKind == "availability" {
			preserved = item
			break
		}
	}
	for _, record := range bundle.Availability {
		if record.MusicID == preserved.MusicID {
			availability = record
			break
		}
	}
	if preserved.MusicID == 0 || availability.MusicID == 0 {
		t.Fatal("embedded seed has no availability item")
	}
	batchSHA := strings.Repeat("6", 64)
	if _, err := database.Exec(`INSERT INTO lyrics_recovery_import_batches
		(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
		 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,raw_byte_count,
		 encoded_byte_count,actor,created_at)
		VALUES (?,1,2,'recovery-root',?,700,?,'{"total":700}',?,?,?,0,0,0,0,'test-actor',?)`, batchSHA,
		strings.Repeat("7", 64), strings.Repeat("8", 64), strings.Repeat("9", 64), strings.Repeat("a", 64),
		strings.Repeat("b", 64), availability.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO lyrics_recovery_import_items
		(batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,state,
		 result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at)
		VALUES (?,?,?,?,?,'[]',?,?, '', '', ?,?)`, batchSHA, preserved.MusicID, preserved.JapaneseTitle,
		preserved.CatalogFingerprint, preserved.MusicID, availability.State, availability.ResultSHA256,
		availability.DocumentSHA256, availability.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics_availability_documents
		(batch_sha256,music_id,schema_version,state,reason_code,no_lyrics_reason,document_json,document_sha256,result_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, batchSHA, availability.MusicID, availability.SchemaVersion, availability.State,
		availability.ReasonCode, availability.NoLyricsReason, availability.DocumentJSON, availability.DocumentSHA256,
		availability.ResultSHA256, availability.CreatedAt); err != nil {
		t.Fatal(err)
	}

	result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreservedExisting != 1 || result.Inserted != embeddedlyricsseed.ExpectedCatalogCount-1 ||
		result.Availability != embeddedlyricsseed.ExpectedAvailability-1 {
		t.Fatalf("existing recovery availability result=%+v", result)
	}
	var applyStatus string
	if err := database.QueryRow(`SELECT apply_status FROM embedded_lyrics_editor_seed_items WHERE seed_sha256=? AND music_id=?`,
		bundle.Manifest.SeedSHA256, preserved.MusicID).Scan(&applyStatus); err != nil {
		t.Fatal(err)
	}
	if applyStatus != "preserved_existing" {
		t.Fatalf("existing recovery availability applyStatus=%q", applyStatus)
	}
	catalog, err := s.CatalogMusic(strconv.Itoa(preserved.MusicID), false, 10, 0)
	if err != nil || len(catalog.Items) != 1 || catalog.Items[0].LyricsAvailabilityState != availability.State {
		t.Fatalf("existing recovery availability catalog=%+v err=%v", catalog, err)
	}
}

func TestApplyEmbeddedLyricsEditorSeedReplayRejectsLostInsertedOwnership(t *testing.T) {
	s, bundle := seededContentBackupStore(t)
	database := s.db
	musicID := bundle.Documents[0].MusicID
	if _, err := database.Exec(`DROP TRIGGER song_lyrics_source_documents_immutable_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DROP TRIGGER song_lyrics_source_v3_reject_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DROP TRIGGER song_lyrics_source_artifacts_immutable_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DROP TRIGGER song_lyrics_component_contributions_immutable_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM song_lyrics_source_documents WHERE music_id=?`, musicID); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err == nil || !strings.Contains(err.Error(), "replay source-v3 item") || result.Replayed != 0 {
		t.Fatalf("lost inserted ownership replay result=%+v err=%v", result, err)
	}
}

func TestApplyEmbeddedLyricsEditorSeedAcceptsCatalogWithExtraSongsAndReplaysAfterRestart(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "embedded-editor-seed-extra-songs.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(database)
	seedEmbeddedLyricsEditorCatalog(t, database, bundle, true)
	seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)
	result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err != nil || result.Inserted != embeddedlyricsseed.ExpectedCatalogCount || result.PreservedExisting != 0 {
		database.Close()
		t.Fatalf("catalog with extras initial seed result=%+v err=%v", result, err)
	}
	var catalogCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM catalog_music`).Scan(&catalogCount); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if catalogCount != embeddedlyricsseed.ExpectedCatalogCount+7 {
		database.Close()
		t.Fatalf("catalog count after initial seed=%d", catalogCount)
	}
	if err := database.Checkpoint(context.Background()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := New(reopened).ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Inserted != 0 || replay.PreservedExisting != 0 || replay.Replayed != embeddedlyricsseed.ExpectedCatalogCount ||
		replay.SourceV3 != 0 || replay.Legacy != 0 || replay.Availability != 0 {
		t.Fatalf("catalog with extras replay result=%+v", replay)
	}
}

func TestApplyEmbeddedLyricsEditorSeedRejectsSeededCatalogDriftWithoutChanges(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "embedded-editor-seed-drift.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := New(database)
	seedEmbeddedLyricsEditorCatalog(t, database, bundle, false)
	seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)
	if _, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	item := bundle.Manifest.Items[0]
	if _, err := database.Exec(`UPDATE catalog_music SET title_ja=? WHERE music_id=?`, item.JapaneseTitle+" drift", item.MusicID); err != nil {
		t.Fatal(err)
	}
	if err := database.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeHash := embeddedLyricsEditorFileSHA256(t, path)
	result, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if !errors.Is(err, ErrEmbeddedLyricsEditorSeedCatalogMismatch) {
		t.Fatalf("drift result=%+v error=%v", result, err)
	}
	if result.Inserted != 0 || result.PreservedExisting != 0 || result.Replayed != 0 {
		t.Fatalf("seeded catalog drift returned mutations: %+v", result)
	}
	if err := database.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if afterHash := embeddedLyricsEditorFileSHA256(t, path); afterHash != beforeHash {
		t.Fatalf("seeded catalog drift changed SQLite main bytes before=%s after=%s", beforeHash, afterHash)
	}
}

func seedEmbeddedLyricsEditorCatalog(t *testing.T, database *db.DB, bundle embeddedlyricsseed.Bundle, addSeven bool) {
	t.Helper()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, item := range bundle.Manifest.Items {
		if _, err := tx.Exec(`INSERT INTO catalog_music
			(music_id,title_ja,lyrics_catalog_fingerprint,lyrics_catalog_policy_version) VALUES (?,?,?,?)`,
			item.MusicID, item.JapaneseTitle, item.CatalogFingerprint, bundle.Manifest.CatalogPolicyVersion); err != nil {
			t.Fatal(err)
		}
	}
	if addSeven {
		for index := 0; index < 7; index++ {
			if _, err := tx.Exec(`INSERT INTO catalog_music
				(music_id,title_ja,lyrics_catalog_fingerprint,lyrics_catalog_policy_version) VALUES (?,?,?,?)`,
				900001+index, "追加曲", strings.Repeat("f", 64), bundle.Manifest.CatalogPolicyVersion); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedEmbeddedLyricsEditorLegacyPerformers(t *testing.T, database *db.DB, bundle embeddedlyricsseed.Bundle) {
	t.Helper()
	performers := map[int]struct{}{}
	for _, segment := range bundle.LegacySegments {
		var ids []int
		if err := json.Unmarshal([]byte(segment.PerformerIDsJSON), &ids); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			performers[id] = struct{}{}
		}
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for id := range performers {
		if _, err := tx.Exec(`INSERT INTO catalog_performers(performer_id,name_ja) VALUES (?,?)`, id, "seed performer"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedEmbeddedLyricsEditorExistingLyrics(t *testing.T, database *db.DB, musicID int) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO song_lyrics
		(music_id,revision,updated_at,updated_by,attribution,translation_credit,proofreading_credit,source_note,
		 source_url,license_note,source_hash,source_page_id,source_revision_id,source_sha1,source_fetched_at,source_fetched_at_rfc3339)
		VALUES (?,1,100,'existing-editor','existing attribution','','','existing note','','',?,0,0,'',0,'')`,
		musicID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyric_lines
		(music_id,line_id,position,japanese,zh_cn,en_us,stanza_break_before) VALUES (?,'existing-line',0,'既存歌詞','既有翻译','',0)`, musicID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyric_segments
		(music_id,line_id,position,text,performer_ids_json,ruby_json) VALUES (?,'existing-line',0,'既存歌詞','[]','[{"text":"既存歌詞"}]')`, musicID); err != nil {
		t.Fatal(err)
	}
}

func seedEmbeddedLyricsEditorProtectedRows(t *testing.T, database *db.DB, item embeddedlyricsseed.CatalogItem) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(username,password_hash,role,created_at) VALUES ('seed-user','private-hash','editor',11)`, nil},
		{`INSERT INTO settings(key,value,encrypted) VALUES ('seed-setting','seed-value',0)`, nil},
		{`INSERT INTO entries(category,field,jp_key,cn_text,source,ids_json,updated_at,updated_by)
			VALUES ('music','title','保護翻訳','受保护翻译','human','["1"]',12,'seed-user')`, nil},
		{`INSERT INTO event_stories(event_id,source,version,last_updated) VALUES (123,'human','protected',13)`, nil},
		{`INSERT INTO audit_log(ts,user,action,detail) VALUES (14,'seed-user','protected.action','protected detail')`, nil},
		{`INSERT INTO lyrics_source_review_items
			(domain_key,kind,analysis_id,music_id,catalog_fingerprint,review_policy_version,reason_code,evidence_json,state,
			 identity_gate,source_use_gate,parse_gate,version,priority,created_at,updated_at,completed_at,provider)
			VALUES (?,'candidate_selection',NULL,?,?,'review-v1','ambiguous_candidates','{}','pending',
			 'not_applicable','not_applicable','not_applicable',1,0,15,15,NULL,'vocaloid_fandom')`,
			[]any{strings.Repeat("d", 64), item.MusicID, item.CatalogFingerprint}},
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

type embeddedLyricsEditorProtectedState struct {
	User        string
	Setting     string
	Entry       string
	Event       string
	Audit       string
	Review      string
	Publication string
}

func snapshotEmbeddedLyricsEditorProtectedRows(t *testing.T, database *db.DB) embeddedLyricsEditorProtectedState {
	t.Helper()
	state := embeddedLyricsEditorProtectedState{}
	queries := []struct {
		target *string
		query  string
	}{
		{&state.User, `SELECT username||'|'||password_hash||'|'||role||'|'||created_at FROM users WHERE username='seed-user'`},
		{&state.Setting, `SELECT key||'|'||value||'|'||encrypted FROM settings WHERE key='seed-setting'`},
		{&state.Entry, `SELECT category||'|'||field||'|'||jp_key||'|'||cn_text||'|'||source||'|'||ids_json||'|'||updated_at||'|'||updated_by
			FROM entries WHERE category='music' AND field='title' AND jp_key='保護翻訳'`},
		{&state.Event, `SELECT event_id||'|'||source||'|'||version||'|'||last_updated FROM event_stories WHERE event_id=123`},
		{&state.Audit, `SELECT ts||'|'||user||'|'||action||'|'||detail FROM audit_log WHERE action='protected.action'`},
		{&state.Review, `SELECT domain_key||'|'||music_id||'|'||catalog_fingerprint||'|'||state||'|'||version||'|'||updated_at
			FROM lyrics_source_review_items WHERE domain_key=?`},
	}
	for _, query := range queries {
		args := []any{}
		if strings.Contains(query.query, "domain_key=?") {
			args = append(args, strings.Repeat("d", 64))
		}
		if err := database.QueryRow(query.query, args...).Scan(query.target); err != nil {
			t.Fatal(err)
		}
	}
	_ = database.QueryRow(`SELECT payload_json FROM song_lyrics_publications ORDER BY music_id LIMIT 1`).Scan(&state.Publication)
	return state
}

func assertEmbeddedLyricsEditorSeedCounts(t *testing.T, database *db.DB, sourceDocuments, legacyDocuments, availability, ledgerItems int) {
	t.Helper()
	var gotSource, gotArtifacts, gotContributions, gotLegacy, gotAvailability, gotItems, gotBatches int
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics_source_documents),
		(SELECT COUNT(*) FROM song_lyrics_source_artifacts),
		(SELECT COUNT(*) FROM song_lyrics_component_contributions),
		(SELECT COUNT(*) FROM song_lyrics),
		(SELECT COUNT(*) FROM embedded_lyrics_editor_seed_items WHERE seed_kind='availability' AND apply_status='inserted'),
		(SELECT COUNT(*) FROM embedded_lyrics_editor_seed_items),
		(SELECT COUNT(*) FROM embedded_lyrics_editor_seed_batches)`).Scan(
		&gotSource, &gotArtifacts, &gotContributions, &gotLegacy, &gotAvailability, &gotItems, &gotBatches); err != nil {
		t.Fatal(err)
	}
	wantArtifacts, wantContributions, wantBatches := 0, 0, 0
	if sourceDocuments > 0 {
		wantArtifacts, wantContributions = embeddedlyricsseed.ExpectedArtifacts, embeddedlyricsseed.ExpectedContributions
		if sourceDocuments != embeddedlyricsseed.ExpectedSourceV3 {
			wantArtifacts--
			wantContributions = -1
		}
	}
	if ledgerItems > 0 {
		wantBatches = 1
	}
	if gotSource != sourceDocuments || gotLegacy != legacyDocuments || gotAvailability != availability ||
		gotItems != ledgerItems || gotBatches != wantBatches {
		t.Fatalf("seed counts source=%d legacy=%d availability=%d items=%d batches=%d", gotSource, gotLegacy, gotAvailability, gotItems, gotBatches)
	}
	if sourceDocuments == embeddedlyricsseed.ExpectedSourceV3 && (gotArtifacts != wantArtifacts || gotContributions != wantContributions) {
		t.Fatalf("seed provenance artifacts=%d contributions=%d", gotArtifacts, gotContributions)
	}
	if sourceDocuments == 0 && (gotArtifacts != 0 || gotContributions != 0) {
		t.Fatalf("unexpected seed provenance artifacts=%d contributions=%d", gotArtifacts, gotContributions)
	}
}

func embeddedLyricsEditorFileSHA256(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
