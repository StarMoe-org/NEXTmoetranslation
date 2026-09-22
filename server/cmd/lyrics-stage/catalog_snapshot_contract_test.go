package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
	"moesekai/server/internal/offlineimport"
	"moesekai/server/internal/store"
)

type snapshotContractRecord struct {
	musicID           int
	title             string
	producer          string
	lyricist          string
	composer          string
	arranger          string
	assetbundle       string
	versionHint       string
	lyricsVersion     string
	presence          model.CatalogEvidencePresence
	vocals            []model.CatalogVocalSignal
	storedFingerprint string
	policyVersion     string
}

func completeSnapshotContractRecord(musicID int, title, version string) snapshotContractRecord {
	return snapshotContractRecord{
		musicID: musicID, title: title, producer: "作詞者 / 作曲者 / 編曲者",
		lyricist: "作詞者", composer: "作曲者", arranger: "編曲者", lyricsVersion: version,
		presence: model.CatalogEvidencePresence{
			Lyricist: true, Composer: true, Arranger: true, LyricsVersion: true,
		},
		vocals:        []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai"}},
		policyVersion: model.LyricsCatalogIdentityPolicyVersion,
	}
}

func reviewSnapshotContractRecord(musicID int, title string) snapshotContractRecord {
	record := completeSnapshotContractRecord(musicID, title, "full")
	record.lyricist = ""
	record.presence.Lyricist = false
	return record
}

func (record snapshotContractRecord) evidence() model.CatalogLyricsEvidence {
	return model.CatalogLyricsEvidence{
		PolicyVersion: model.LyricsCatalogIdentityPolicyVersion, Title: record.title, Lyricist: record.lyricist,
		Composer: record.composer, Arranger: record.arranger, Assetbundle: record.assetbundle,
		VersionHint: record.versionHint, LyricsVersion: record.lyricsVersion,
		Presence: record.presence, Vocals: append([]model.CatalogVocalSignal(nil), record.vocals...),
	}
}

func (record snapshotContractRecord) fingerprint(t *testing.T) string {
	t.Helper()
	fingerprint, err := model.CatalogLyricsEvidenceFingerprint(record.evidence())
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func writeSnapshotContractDatabase(t *testing.T, records []snapshotContractRecord) string {
	t.Helper()
	return writeSnapshotContractDatabaseAtVersion(t, records, lyricsstaging.CatalogSchemaVersion)
}

func writeSnapshotContractDatabaseAtVersion(t *testing.T, records []snapshotContractRecord, runtimeVersion int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.db")
	database, err := sql.Open("sqlite", "file:"+path+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(`
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY);
CREATE TABLE catalog_music (
 music_id INTEGER PRIMARY KEY,
 title_ja TEXT NOT NULL,
 producer_metadata TEXT NOT NULL DEFAULT '',
 lyricist TEXT NOT NULL DEFAULT '',
 composer TEXT NOT NULL DEFAULT '',
 arranger TEXT NOT NULL DEFAULT '',
 assetbundle_name TEXT NOT NULL DEFAULT '',
 version_hint TEXT NOT NULL DEFAULT '',
 lyrics_version TEXT NOT NULL DEFAULT 'unknown',
 lyrics_evidence_presence_json TEXT NOT NULL,
 vocal_signals_json TEXT NOT NULL DEFAULT '[]',
 lyrics_catalog_fingerprint TEXT NOT NULL DEFAULT '',
 lyrics_catalog_policy_version TEXT NOT NULL DEFAULT 'catalog-identity-v2'
);`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	transaction, err := database.Begin()
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	for version := 1; version <= runtimeVersion; version++ {
		if _, err := transaction.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, version); err != nil {
			transaction.Rollback()
			database.Close()
			t.Fatal(err)
		}
	}
	for _, record := range records {
		presenceJSON, err := json.Marshal(record.presence)
		if err != nil {
			t.Fatal(err)
		}
		vocalsJSON, err := json.Marshal(record.vocals)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint := record.storedFingerprint
		if fingerprint == "" {
			fingerprint = record.fingerprint(t)
		}
		if _, err := transaction.Exec(`INSERT INTO catalog_music
			(music_id,title_ja,producer_metadata,lyricist,composer,arranger,assetbundle_name,version_hint,
			 lyrics_version,lyrics_evidence_presence_json,vocal_signals_json,lyrics_catalog_fingerprint,
			 lyrics_catalog_policy_version) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.musicID, record.title,
			record.producer, record.lyricist, record.composer, record.arranger, record.assetbundle,
			record.versionHint, record.lyricsVersion, string(presenceJSON), string(vocalsJSON), fingerprint,
			record.policyVersion); err != nil {
			transaction.Rollback()
			database.Close()
			t.Fatal(err)
		}
	}
	if err := transaction.Commit(); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSnapshotContractImportDatabase(t *testing.T, records []snapshotContractRecord) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog-import.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	catalogStore := store.New(database)
	if err := catalogStore.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{
		PerformerID: 21, JapaneseName: "初音ミク", EnglishName: "Miku",
	}}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	transaction, err := database.Begin()
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	for _, record := range records {
		presenceJSON, err := json.Marshal(record.presence)
		if err != nil {
			transaction.Rollback()
			database.Close()
			t.Fatal(err)
		}
		vocalsJSON, err := json.Marshal(record.vocals)
		if err != nil {
			transaction.Rollback()
			database.Close()
			t.Fatal(err)
		}
		fingerprint := record.storedFingerprint
		if fingerprint == "" {
			fingerprint = record.fingerprint(t)
		}
		if _, err := transaction.Exec(`INSERT INTO catalog_music
			(music_id,title_ja,producer_metadata,lyricist,composer,arranger,assetbundle_name,version_hint,
			 lyrics_version,lyrics_evidence_presence_json,vocal_signals_json,lyrics_catalog_fingerprint,
			 lyrics_catalog_policy_version) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.musicID, record.title,
			record.producer, record.lyricist, record.composer, record.arranger, record.assetbundle,
			record.versionHint, record.lyricsVersion, string(presenceJSON), string(vocalsJSON), fingerprint,
			record.policyVersion); err != nil {
			transaction.Rollback()
			database.Close()
			t.Fatal(err)
		}
	}
	if err := transaction.Commit(); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Checkpoint(context.Background()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("closed mixed import fixture retained SQLite sidecar %s: %v", suffix, err)
		}
	}
	return path
}

func snapshotContractCandidate(musicID int, suffix string) lyricsstaging.CandidateIdentity {
	delta := 0
	for _, current := range suffix {
		delta += int(current)
	}
	delta %= 31
	pageID := musicID*100 + 1 + delta
	revisionID := musicID*100 + 2 + delta
	title := "候補" + suffix
	canonical := url.URL{Scheme: "https", Host: "vocaloid.fandom.com", Path: "/wiki/" + strings.ReplaceAll(title, " ", "_")}
	query := canonical.Query()
	query.Set("oldid", fmt.Sprintf("%d", revisionID))
	canonical.RawQuery = query.Encode()
	wikitext := []byte("== Lyrics ==\n歌詞")
	wikitextDigest := sha1.Sum(wikitext)
	evidenceDigest := sha256.Sum256(wikitext)
	evidenceSHA256 := hex.EncodeToString(evidenceDigest[:])
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		model.LyricsSourceProviderVocaloidFandom,
		fmt.Sprintf("fetch:vocaloid-fandom:%d", pageID),
		time.Unix(100, 0).UTC().Format(time.RFC3339Nano),
		evidenceSHA256,
	)
	return lyricsstaging.CandidateIdentity{
		Provider: model.LyricsSourceProviderVocaloidFandom, Origin: model.LyricsSourceOriginVocaloidFandom,
		PageID: pageID, RevisionID: revisionID, SHA1: hex.EncodeToString(wikitextDigest[:]),
		Title: title, CanonicalURL: canonical.String(),
		Categories: []string{"Lyrics", "Songs"}, Section: "Lyrics/Project SEKAI Version", RenditionKey: "full-sekai",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: evidenceID, SHA256: evidenceSHA256,
		}},
	}
}

func snapshotContractBaseItem(record snapshotContractRecord, target model.CatalogLyricsTarget) lyricsstaging.PreflightItem {
	associations := append([]int(nil), target.AssociationMusicIDs...)
	if associations == nil {
		associations = []int{}
	}
	return lyricsstaging.PreflightItem{
		MusicID: record.musicID, JapaneseTitle: record.title, CatalogFingerprint: target.CatalogFingerprint,
		TargetMusicID: target.TargetMusicID, AssociationMusicIDs: associations, ReasonCode: target.ReasonCode,
	}
}

func buildSevenClassSnapshotReport(t *testing.T, records []snapshotContractRecord) lyricsstaging.PreflightReport {
	t.Helper()
	recordsByMusicID := make(map[int]snapshotContractRecord, len(records))
	grouping := make([]model.CatalogLyricsGroupingRecord, 0, len(records))
	for _, record := range records {
		fingerprint := record.fingerprint(t)
		recordsByMusicID[record.musicID] = record
		grouping = append(grouping, model.CatalogLyricsGroupingRecord{
			MusicID: record.musicID, Fingerprint: fingerprint, Evidence: record.evidence(),
		})
	}
	targets := model.ClassifyCatalogLyricsTargets(grouping)
	report := lyricsstaging.PreflightReport{
		SchemaVersion:        lyricsstaging.PreflightSchemaVersion,
		GeneratedAt:          time.Unix(123, 0).UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: lyricsstaging.CatalogSchemaVersion,
		CatalogCount:         len(records),
		CatalogReview:        []lyricsstaging.PreflightItem{}, GameSizeEvidence: []lyricsstaging.PreflightItem{},
		UniqueComplete: []lyricsstaging.PreflightItem{}, Ambiguous: []lyricsstaging.PreflightItem{},
		Missing: []lyricsstaging.PreflightItem{}, Incomplete: []lyricsstaging.PreflightItem{},
		Error: []lyricsstaging.PreflightItem{},
	}
	for _, target := range targets {
		record := recordsByMusicID[target.MusicID]
		item := snapshotContractBaseItem(record, target)
		switch target.Disposition {
		case model.LyricsCatalogTargetReview:
			report.CatalogReview = append(report.CatalogReview, item)
		case model.LyricsCatalogTargetGameSizeEvidence:
			report.GameSizeEvidence = append(report.GameSizeEvidence, item)
		case model.LyricsCatalogTargetFullTarget:
			suffix := fmt.Sprintf("-%d", target.MusicID)
			switch target.MusicID {
			case 3:
				candidate := snapshotContractCandidate(target.MusicID, suffix)
				item.Candidate = &candidate
				item.FixedArtifactCandidates = []lyricsstaging.CandidateIdentity{candidate}
				item.PostFetchState = lyricsstaging.PostFetchStateComplete
				item.CompositionReason = model.LyricsSourceVersionReasonUntaggedFullOnly
				item.LineCount = 1
				item.SearchAttempts = 1
				item.FetchAttempts = 1
				report.UniqueComplete = append(report.UniqueComplete, item)
			case 5:
				item.Candidates = []lyricsstaging.CandidateIdentity{
					snapshotContractCandidate(target.MusicID, "-a"),
					snapshotContractCandidate(target.MusicID+100, "-b"),
				}
				item.SearchAttempts = 1
				report.Ambiguous = append(report.Ambiguous, item)
			case 6:
				item.SearchAttempts = 1
				item.SearchDiagnostics = &lyricsstaging.SearchDiagnostics{}
				item.ReasonCode = string(lyricssource.ZeroCandidateNoSearchHits)
				report.Missing = append(report.Missing, item)
			case 7:
				candidate := snapshotContractCandidate(target.MusicID, suffix)
				item.Candidate = &candidate
				item.SearchAttempts = 1
				item.FetchAttempts = 1
				item.ErrorCode = "missing_lyrics"
				report.Incomplete = append(report.Incomplete, item)
			case 8:
				candidate := snapshotContractCandidate(target.MusicID, suffix)
				item.Candidate = &candidate
				item.SearchAttempts = 1
				item.FetchAttempts = 1
				item.ErrorCode = "rate_limited"
				report.Error = append(report.Error, item)
			case 9:
				first := snapshotContractCandidate(target.MusicID, "-conflict-a")
				second := snapshotContractCandidate(target.MusicID+100, "-conflict-b")
				first.ArtifactRenditionKey = "conflict-a"
				second.ArtifactRenditionKey = "conflict-b"
				item.FixedArtifactCandidates = []lyricsstaging.CandidateIdentity{first, second}
				item.PostFetchState = lyricsstaging.PostFetchStateVersionConflict
				item.CompositionReason = model.LyricsSourceVersionReasonVersionConflict
				item.SearchAttempts = 1
				item.FetchAttempts = 1
				item.ErrorCode = string(model.LyricsSourceVersionReasonVersionConflict)
				report.Incomplete = append(report.Incomplete, item)
			default:
				t.Fatalf("unexpected full target %d", target.MusicID)
			}
		default:
			t.Fatalf("unexpected disposition %q", target.Disposition)
		}
	}
	report.Summary = lyricsstaging.PreflightSummary{
		CatalogReview: len(report.CatalogReview), GameSizeEvidence: len(report.GameSizeEvidence),
		UniqueComplete: len(report.UniqueComplete), Ambiguous: len(report.Ambiguous), Missing: len(report.Missing),
		Incomplete: len(report.Incomplete), Error: len(report.Error),
	}
	evidence := []lyricssource.IndexEvidence{}
	for _, items := range [][]lyricsstaging.PreflightItem{report.UniqueComplete, report.Ambiguous, report.Incomplete, report.Error} {
		for _, item := range items {
			if len(item.FixedArtifactCandidates) > 0 {
				for _, candidate := range item.FixedArtifactCandidates {
					evidence = append(evidence, stageIndexEvidence(candidate, []byte("== Lyrics ==\n歌詞"), time.Unix(100, 0).UTC()))
				}
				continue
			}
			if item.Candidate != nil {
				evidence = append(evidence, stageIndexEvidence(*item.Candidate, []byte("== Lyrics ==\n歌詞"), time.Unix(100, 0).UTC()))
			}
			for _, candidate := range item.Candidates {
				evidence = append(evidence, stageIndexEvidence(candidate, []byte("== Lyrics ==\n歌詞"), time.Unix(100, 0).UTC()))
			}
		}
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(evidence)
	if err != nil {
		t.Fatal(err)
	}
	report.EvidenceReceipt = &receipt
	if err := lyricsstaging.ValidatePreflight(report); err != nil {
		t.Fatalf("built invalid seven-class report: %v\n%+v", err, report)
	}
	return report
}

func sevenClassSnapshotRecords() []snapshotContractRecord {
	return []snapshotContractRecord{
		reviewSnapshotContractRecord(1, "要確認曲"),
		completeSnapshotContractRecord(2, "共有曲", "game_size"),
		completeSnapshotContractRecord(3, "共有曲", "full"),
		completeSnapshotContractRecord(5, "曖昧曲", "full"),
		completeSnapshotContractRecord(6, "欠落曲", "full"),
		completeSnapshotContractRecord(7, "不完全曲", "full"),
		completeSnapshotContractRecord(8, "失敗曲", "full"),
		completeSnapshotContractRecord(9, "版競合曲", "full"),
	}
}

func writeSnapshotContractReport(t *testing.T, report lyricsstaging.PreflightReport) string {
	t.Helper()
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	path := filepath.Join(t.TempDir(), "preflight.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func snapshotContractOptions(t *testing.T, databasePath string, report lyricsstaging.PreflightReport) options {
	t.Helper()
	return options{
		ReportPath: writeSnapshotContractReport(t, report), DatabasePath: databasePath,
		OutputPath: filepath.Join(t.TempDir(), "staging.json"), Concurrency: 2, MaxAttempts: 1,
		RequestTimeout: time.Second, RetryDelay: 0,
	}
}

func snapshotContractFixed(item lyricsstaging.PreflightItem) lyricssource.FixedRevision {
	candidate := *item.Candidate
	return lyricssource.FixedRevision{
		Provider: candidate.Provider, Origin: candidate.Origin,
		PageID: candidate.PageID, RevisionID: candidate.RevisionID, SHA1: candidate.SHA1,
		PageTitle: candidate.Title, CanonicalURL: candidate.CanonicalURL,
		Categories: append([]string(nil), candidate.Categories...),
		Section:    candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
		IndexEvidence: []lyricssource.IndexEvidence{
			stageIndexEvidence(candidate, []byte("== Lyrics ==\n歌詞"), time.Unix(100, 0).UTC()),
		},
		FetchedAt: time.Unix(int64(candidate.RevisionID), 0).UTC(),
		Wikitext:  []byte("== Lyrics ==\n歌詞"),
		Lines:     []lyricssource.ExtractedLine{{Japanese: "歌詞"}},
		Extraction: lyricssource.Extraction{
			Version:              lyricssource.LyricsVersion{Kind: "sekai", Label: "Project SEKAI Version"},
			Performers:           []lyricssource.Performer{{PerformerID: "歌唱者-21", Name: "初音ミク", Color: "#33CCBB"}},
			RubyGeneratorVersion: "kagome-ipadic-v1",
			Lines: []lyricssource.StructuredLine{{Japanese: "歌詞", Segments: []lyricssource.LyricsSegment{{
				Text: "歌詞", PerformerIDs: []string{"歌唱者-21"}, Ruby: []lyricssource.RubySpan{{Text: "歌", Reading: "うた"}, {Text: "詞", Reading: "し"}},
			}}, TrailingPerformerIDs: []string{"歌唱者-21"}}},
		},
	}
}

func executeSnapshotContract(t *testing.T, records []snapshotContractRecord, report lyricsstaging.PreflightReport) (lyricsstaging.Manifest, error, int32) {
	t.Helper()
	databasePath := writeSnapshotContractDatabase(t, records)
	opts := snapshotContractOptions(t, databasePath, report)
	var calls atomic.Int32
	manifest, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		calls.Add(1)
		item := report.UniqueComplete[0]
		if identity.MusicID != item.MusicID || identity.JapaneseTitle != item.JapaneseTitle {
			return lyricssource.FixedRevision{}, fmt.Errorf("unexpected source identity %+v", identity)
		}
		return snapshotContractFixed(item), nil
	}})
	return manifest, err, calls.Load()
}

func TestReadOnlyCatalogSnapshotPinsVerifiedConnectionAndInode(t *testing.T) {
	originalRecord := completeSnapshotContractRecord(3, "固定元曲", "full")
	path := writeSnapshotContractDatabase(t, []snapshotContractRecord{originalRecord})
	database, err := openReadOnlyDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	openedPath := path + ".opened"
	if err := os.Rename(path, openedPath); err != nil {
		t.Fatal(err)
	}
	replacementPath := writeSnapshotContractDatabase(t, []snapshotContractRecord{
		completeSnapshotContractRecord(99, "置換後曲", "full"),
	})
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatal(err)
	}

	var musicID, queryOnly, trustedSchema, attachedCount int
	var title string
	if err := database.transaction.QueryRowContext(context.Background(),
		`SELECT music_id,title_ja FROM catalog_music`).Scan(&musicID, &title); err != nil {
		t.Fatal(err)
	}
	if err := database.transaction.QueryRowContext(context.Background(), `PRAGMA query_only`).Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	if err := database.transaction.QueryRowContext(context.Background(), `PRAGMA trusted_schema`).Scan(&trustedSchema); err != nil {
		t.Fatal(err)
	}
	if err := database.transaction.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pragma_database_list`).Scan(&attachedCount); err != nil {
		t.Fatal(err)
	}
	if musicID != originalRecord.musicID || title != originalRecord.title || queryOnly != 1 || trustedSchema != 0 || attachedCount != 1 {
		t.Fatalf("snapshot read replacement or lost defenses: music=%d title=%q queryOnly=%d trusted=%d attached=%d",
			musicID, title, queryOnly, trustedSchema, attachedCount)
	}
	if err := database.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogSnapshotContractUsesPinnedImmutableDescriptor(t *testing.T) {
	path := writeSnapshotContractDatabase(t, []snapshotContractRecord{
		completeSnapshotContractRecord(3, "固定記述子", "full"),
	})
	var dataSourceName string
	database, err := openReadOnlyDatabaseWithOpeners(context.Background(), path, os.Open, func(driverName, sourceName string) (*sql.DB, error) {
		dataSourceName = sourceName
		return sql.Open(driverName, sourceName)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	parsed, err := url.Parse(dataSourceName)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Scheme != "file" || !strings.HasPrefix(parsed.Path, "/dev/fd/") ||
		query.Get("mode") != "ro" || query.Get("immutable") != "1" {
		t.Fatalf("immutable descriptor URI=%q", dataSourceName)
	}
	if !containsSnapshotString(query["_pragma"], "query_only(1)") ||
		!containsSnapshotString(query["_pragma"], "trusted_schema(0)") {
		t.Fatalf("immutable descriptor pragmas=%v", query["_pragma"])
	}
}

func containsSnapshotString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestCatalogSnapshotContractRejectsEverySQLiteSidecar(t *testing.T) {
	for _, suffix := range sqliteSidecarSuffixes {
		t.Run(strings.TrimPrefix(suffix, "-"), func(t *testing.T) {
			path := writeSnapshotContractDatabase(t, []snapshotContractRecord{
				completeSnapshotContractRecord(3, "単独スナップショット", "full"),
			})
			if err := os.WriteFile(path+suffix, []byte("sidecar must be rejected"), 0o600); err != nil {
				t.Fatal(err)
			}
			database, err := openReadOnlyDatabase(context.Background(), path)
			if database != nil {
				database.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "standalone offline SQLite snapshot") || !strings.Contains(err.Error(), suffix) {
				t.Fatalf("sidecar %s error=%v", suffix, err)
			}
		})
	}
}

func TestCatalogSnapshotContractRejectsSidecarsAtOriginalAndResolvedPaths(t *testing.T) {
	for _, location := range []string{"original", "resolved"} {
		t.Run(location, func(t *testing.T) {
			realPath := writeSnapshotContractDatabase(t, []snapshotContractRecord{
				completeSnapshotContractRecord(3, "別名スナップショット", "full"),
			})
			linkPath := filepath.Join(t.TempDir(), "catalog-link.db")
			if err := os.Symlink(realPath, linkPath); err != nil {
				t.Fatal(err)
			}
			sidecarBase := linkPath
			if location == "resolved" {
				sidecarBase = realPath
			}
			if err := os.WriteFile(sidecarBase+"-journal", []byte("must be rejected"), 0o600); err != nil {
				t.Fatal(err)
			}
			database, err := openReadOnlyDatabase(context.Background(), linkPath)
			if database != nil {
				database.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "-journal") {
				t.Fatalf("%s-path sidecar error=%v", location, err)
			}
		})
	}
}

func TestCatalogSnapshotContractRejectsActiveWALDatabase(t *testing.T) {
	path := writeSnapshotContractDatabase(t, []snapshotContractRecord{
		completeSnapshotContractRecord(3, "活動中WAL", "full"),
	})
	writer, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	var journalMode string
	if err := writer.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal mode=%q, want wal", journalMode)
	}
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`CREATE TABLE active_wal_probe (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO active_wal_probe(value) VALUES ('uncheckpointed')`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); err != nil {
			t.Fatalf("active WAL database did not create %s: %v", suffix, err)
		}
	}
	database, err := openReadOnlyDatabase(context.Background(), path)
	if database != nil {
		database.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "standalone offline SQLite snapshot") || !strings.Contains(err.Error(), "-wal") {
		t.Fatalf("active WAL database error=%v", err)
	}
}

func TestCatalogSnapshotContractRejectsPostLoadMutationBeforeSource(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	databasePath := writeSnapshotContractDatabase(t, records)
	opts := snapshotContractOptions(t, databasePath, report)
	body, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	_, err = executeWithCatalogLoader(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		calls.Add(1)
		return snapshotContractFixed(report.UniqueComplete[0]), nil
	}}, func(ctx context.Context, path string) ([]catalogSnapshotItem, error) {
		return loadCatalogSnapshotWithHook(ctx, path, func() error {
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			changed := []byte{body[len(body)-1] ^ 0xff}
			if _, err := file.WriteAt(changed, int64(len(body)-1)); err != nil {
				file.Close()
				return err
			}
			if err := file.Sync(); err != nil {
				file.Close()
				return err
			}
			return file.Close()
		})
	})
	if err == nil || !strings.Contains(err.Error(), "bytes changed after catalog load") || calls.Load() != 0 {
		t.Fatalf("post-load mutation err=%v source calls=%d", err, calls.Load())
	}
}

func TestCatalogSnapshotContractRejectsPostLoadSidecarsBeforeSource(t *testing.T) {
	for _, suffix := range sqliteSidecarSuffixes {
		t.Run(strings.TrimPrefix(suffix, "-"), func(t *testing.T) {
			records := sevenClassSnapshotRecords()
			report := buildSevenClassSnapshotReport(t, records)
			databasePath := writeSnapshotContractDatabase(t, records)
			opts := snapshotContractOptions(t, databasePath, report)
			var calls atomic.Int32
			_, err := executeWithCatalogLoader(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				calls.Add(1)
				return snapshotContractFixed(report.UniqueComplete[0]), nil
			}}, func(ctx context.Context, path string) ([]catalogSnapshotItem, error) {
				return loadCatalogSnapshotWithHook(ctx, path, func() error {
					return os.WriteFile(path+suffix, []byte("appeared after catalog load"), 0o600)
				})
			})
			if err == nil || !strings.Contains(err.Error(), "after catalog load") ||
				!strings.Contains(err.Error(), suffix) || calls.Load() != 0 {
				t.Fatalf("post-load sidecar %s err=%v source calls=%d", suffix, err, calls.Load())
			}
		})
	}
}

func TestCatalogSnapshotContractVerifiesAgainAfterSQLiteClose(t *testing.T) {
	t.Run("same-size bytes", func(t *testing.T) {
		path := writeSnapshotContractDatabase(t, []snapshotContractRecord{
			completeSnapshotContractRecord(3, "閉鎖後検証", "full"),
		})
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		database, err := openReadOnlyDatabase(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		err = database.closeWithHook(func() error {
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			changed := []byte{body[len(body)-1] ^ 0xff}
			if _, err := file.WriteAt(changed, int64(len(body)-1)); err != nil {
				file.Close()
				return err
			}
			if err := file.Sync(); err != nil {
				file.Close()
				return err
			}
			return file.Close()
		})
		if err == nil || !strings.Contains(err.Error(), "bytes changed after close") {
			t.Fatalf("after-close byte mutation error=%v", err)
		}
	})

	for _, suffix := range sqliteSidecarSuffixes {
		t.Run(strings.TrimPrefix(suffix, "-"), func(t *testing.T) {
			path := writeSnapshotContractDatabase(t, []snapshotContractRecord{
				completeSnapshotContractRecord(3, "閉鎖後副作用", "full"),
			})
			database, err := openReadOnlyDatabase(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			err = database.closeWithHook(func() error {
				return os.WriteFile(path+suffix, []byte("appeared after SQLite close"), 0o600)
			})
			if err == nil || !strings.Contains(err.Error(), "after close") || !strings.Contains(err.Error(), suffix) {
				t.Fatalf("after-close sidecar %s error=%v", suffix, err)
			}
		})
	}
}

func TestCatalogSnapshotContractStartsSourceOnlyAfterLoaderReturns(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	databasePath := writeSnapshotContractDatabase(t, records)
	opts := snapshotContractOptions(t, databasePath, report)
	catalog, err := loadCatalogSnapshot(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	loaderReturned := false
	manifest, err := executeWithCatalogLoader(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		if !loaderReturned {
			return lyricssource.FixedRevision{}, fmt.Errorf("source request started before catalog loader returned")
		}
		return snapshotContractFixed(report.UniqueComplete[0]), nil
	}}, func(context.Context, string) ([]catalogSnapshotItem, error) {
		defer func() { loaderReturned = true }()
		return catalog, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !loaderReturned || len(manifest.Items) != 1 {
		t.Fatalf("loaderReturned=%t manifest=%+v", loaderReturned, manifest)
	}
}

func TestCatalogSnapshotContractAcceptsSupportedRuntimeSnapshots(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	for _, runtimeVersion := range []int{lyricsstaging.CatalogSchemaVersion, 19, lyricsstaging.MaximumCatalogRuntimeSchema, maximumCompatibleCatalogRuntimeSchema} {
		t.Run(fmt.Sprintf("v%d", runtimeVersion), func(t *testing.T) {
			databasePath := writeSnapshotContractDatabaseAtVersion(t, records, runtimeVersion)
			opts := snapshotContractOptions(t, databasePath, report)
			var calls atomic.Int32
			manifest, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				calls.Add(1)
				return snapshotContractFixed(report.UniqueComplete[0]), nil
			}})
			if err != nil || calls.Load() != 1 || len(manifest.Items) != 1 {
				t.Fatalf("runtime v%d calls=%d manifest=%+v err=%v", runtimeVersion, calls.Load(), manifest, err)
			}
		})
	}
}

func TestCatalogSnapshotContractRejectsFutureRuntimeSchemaBeforeSource(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	databasePath := writeSnapshotContractDatabaseAtVersion(t, records, maximumCompatibleCatalogRuntimeSchema+1)
	opts := snapshotContractOptions(t, databasePath, report)
	var calls atomic.Int32
	_, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		calls.Add(1)
		return snapshotContractFixed(report.UniqueComplete[0]), nil
	}})
	wantHistory := fmt.Sprintf("contiguous v%d through v%d history",
		lyricsstaging.CatalogSchemaVersion, maximumCompatibleCatalogRuntimeSchema)
	if err == nil || calls.Load() != 0 || !strings.Contains(err.Error(), wantHistory) {
		t.Fatalf("future runtime schema err=%v calls=%d want=%q", err, calls.Load(), wantHistory)
	}
}

func TestCatalogSnapshotContractRejectsMissingPinnedColumnBeforeSource(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	databasePath := writeSnapshotContractDatabaseAtVersion(t, records, maximumCompatibleCatalogRuntimeSchema)
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`ALTER TABLE catalog_music DROP COLUMN lyrics_catalog_policy_version`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	opts := snapshotContractOptions(t, databasePath, report)
	var calls atomic.Int32
	_, err = execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		calls.Add(1)
		return snapshotContractFixed(report.UniqueComplete[0]), nil
	}})
	if err == nil || calls.Load() != 0 || !strings.Contains(err.Error(), "pinned v18 catalog contract column lyrics_catalog_policy_version") {
		t.Fatalf("missing catalog column err=%v calls=%d", err, calls.Load())
	}
}

func TestCatalogSnapshotContractAcceptsValidSevenClassReport(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	manifest, err, calls := executeSnapshotContract(t, records, report)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(manifest.Items) != 1 || manifest.Items[0].MusicID != report.UniqueComplete[0].MusicID {
		t.Fatalf("calls=%d manifest=%+v", calls, manifest)
	}
}

func TestMixedClassificationStagePublishesManifestReachableReceiptAndRealImportAcceptsPair(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	if len(report.UniqueComplete) != 1 || len(report.Ambiguous) != 1 || len(report.Error) != 1 ||
		len(report.Incomplete) != 2 || report.EvidenceReceipt == nil {
		t.Fatalf("mixed report classifications=%+v", report.Summary)
	}
	foundVersionConflict := false
	for _, item := range report.Incomplete {
		foundVersionConflict = foundVersionConflict || item.PostFetchState == lyricsstaging.PostFetchStateVersionConflict
	}
	if !foundVersionConflict {
		t.Fatal("mixed report has no post-fetch version-conflict evidence")
	}

	databasePath := writeSnapshotContractImportDatabase(t, records)
	opts := snapshotContractOptions(t, databasePath, report)
	opts.EvidenceReceiptOutputPath = filepath.Join(filepath.Dir(opts.OutputPath), "evidence-receipt.json")
	var fetchCalls atomic.Int32
	result, err := executeStagingWithDependencies(
		context.Background(),
		opts,
		fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			return snapshotContractFixed(report.UniqueComplete[0]), nil
		}},
		loadCatalogSnapshot,
		lyricsstaging.BuildDraftFromFixedArtifacts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if fetchCalls.Load() != 1 || len(result.manifest.Items) != 1 || len(result.evidenceReceipt.IndexEvidence) != 1 ||
		result.evidenceReceiptSHA256 != result.evidenceReceipt.ReceiptSHA256 ||
		result.evidenceReceiptSHA256 == report.EvidenceReceipt.ReceiptSHA256 {
		t.Fatalf("mixed stage calls=%d fullEvidence=%d projected=%+v", fetchCalls.Load(), len(report.EvidenceReceipt.IndexEvidence), result.evidenceReceipt)
	}
	manifestCandidates, err := lyricsstaging.EvidenceCandidatesFromValidatedManifest(result.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := lyricsstaging.ValidatePrivateEvidenceReceiptForCandidates(result.evidenceReceipt, manifestCandidates); err != nil {
		t.Fatalf("projected manifest exact union: %v", err)
	}
	projectedIDs := make(map[string]struct{}, len(result.evidenceReceipt.IndexEvidence))
	for _, evidence := range result.evidenceReceipt.IndexEvidence {
		projectedIDs[evidence.EvidenceID] = struct{}{}
	}
	for _, evidence := range report.EvidenceReceipt.IndexEvidence {
		_, projected := projectedIDs[evidence.EvidenceID]
		manifestReachable := false
		for _, candidate := range manifestCandidates {
			for _, reference := range candidate.IndexEvidenceRefs {
				manifestReachable = manifestReachable || reference.EvidenceID == evidence.EvidenceID
			}
		}
		if projected != manifestReachable {
			t.Fatalf("evidence %s projected=%t manifestReachable=%t", evidence.EvidenceID, projected, manifestReachable)
		}
	}

	reportBody, err := os.ReadFile(opts.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lyricsstaging.DecodePreflightWithEvidenceResolver(reportBody); err != nil {
		t.Fatalf("full mixed report receipt validation: %v", err)
	}
	reportDigest := sha256.Sum256(reportBody)
	if result.manifest.Preflight.ReportSHA256 != hex.EncodeToString(reportDigest[:]) || result.reportSHA256 != result.manifest.Preflight.ReportSHA256 {
		t.Fatalf("manifest full report binding=%s result=%s want=%x", result.manifest.Preflight.ReportSHA256, result.reportSHA256, reportDigest)
	}

	mutatedProjected := result.evidenceReceipt
	mutatedProjected.IndexEvidence = append([]lyricssource.IndexEvidence(nil), result.evidenceReceipt.IndexEvidence...)
	mutatedProjected.IndexEvidence[0].Raw = append([]byte(nil), result.evidenceReceipt.IndexEvidence[0].Raw...)
	mutatedProjected.IndexEvidence[0].Raw[0] ^= 1
	if err := writeManifestEvidencePairContext(
		context.Background(), opts.OutputPath, result.manifest, opts.EvidenceReceiptOutputPath,
		mutatedProjected, result.evidenceReceiptSHA256,
	); err == nil || !strings.Contains(err.Error(), "private evidence receipt digest does not match") {
		t.Fatalf("mutated projected publication receipt error=%v", err)
	}
	if err := writeManifestEvidencePairContext(
		context.Background(), opts.OutputPath, result.manifest, opts.EvidenceReceiptOutputPath,
		result.evidenceReceipt, report.EvidenceReceipt.ReceiptSHA256,
	); err == nil || !strings.Contains(err.Error(), "does not bind the projected evidence receipt digest") {
		t.Fatalf("crossed publication receipt binding error=%v", err)
	}
	for _, path := range []string{opts.OutputPath, opts.EvidenceReceiptOutputPath} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("rejected publication created %s: %v", path, statErr)
		}
	}
	if err := writeManifestEvidencePairContext(
		context.Background(), opts.OutputPath, result.manifest, opts.EvidenceReceiptOutputPath,
		result.evidenceReceipt, result.evidenceReceiptSHA256,
	); err != nil {
		t.Fatal(err)
	}
	publishedReceiptBody, err := os.ReadFile(opts.EvidenceReceiptOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptBody, err := lyricsstaging.MarshalPrivateEvidenceReceipt(result.evidenceReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(publishedReceiptBody, wantReceiptBody) {
		t.Fatal("stage pair did not publish the canonical projected receipt")
	}
	publishedManifestBody, err := os.ReadFile(opts.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	publishedManifest, err := lyricsstaging.DecodeManifest(publishedManifestBody)
	if err != nil {
		t.Fatal(err)
	}
	publishedReceipt, err := lyricsstaging.DecodePrivateEvidenceReceipt(publishedReceiptBody)
	if err != nil {
		t.Fatal(err)
	}

	importDatabase, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer importDatabase.Close()
	results, err := offlineimport.ImportStagedLyricsManifestWithEvidenceReceipt(
		context.Background(), store.New(importDatabase), publishedManifest, publishedReceipt, "offline-operator",
	)
	if err != nil || len(results) != 1 || !results[0].Changed || results[0].MusicID != report.UniqueComplete[0].MusicID {
		t.Fatalf("real mixed staged import results=%+v err=%v", results, err)
	}
	if _, err := offlineimport.ImportStagedLyricsManifestWithEvidenceReceipt(
		context.Background(), store.New(importDatabase), publishedManifest, *report.EvidenceReceipt, "offline-operator",
	); err == nil || !strings.Contains(err.Error(), "orphan evidence") {
		t.Fatalf("real import accepted full-report orphan evidence: %v", err)
	}
}

func TestMixedClassificationStageRejectsFullReportReceiptMutationBeforeFetch(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	if report.EvidenceReceipt == nil || len(report.EvidenceReceipt.IndexEvidence) < 2 {
		t.Fatal("mixed report receipt lacks non-manifest evidence")
	}
	report.EvidenceReceipt.IndexEvidence[1].Raw[0] ^= 1
	databasePath := writeSnapshotContractDatabase(t, records)
	opts := snapshotContractOptions(t, databasePath, report)
	var calls atomic.Int32
	_, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		calls.Add(1)
		return snapshotContractFixed(report.UniqueComplete[0]), nil
	}})
	if err == nil || calls.Load() != 0 || !strings.Contains(err.Error(), "private evidence receipt digest does not match") {
		t.Fatalf("mutated full report receipt err=%v calls=%d", err, calls.Load())
	}
}

func TestCatalogSnapshotContractRejectsCountAndMusicIDSetDriftBeforeSource(t *testing.T) {
	baseRecords := sevenClassSnapshotRecords()
	baseReport := buildSevenClassSnapshotReport(t, baseRecords)
	for name, records := range map[string][]snapshotContractRecord{
		"database extra record":   append(append([]snapshotContractRecord{}, baseRecords...), completeSnapshotContractRecord(10, "余分曲", "full")),
		"database missing record": append([]snapshotContractRecord{}, baseRecords[:len(baseRecords)-1]...),
	} {
		t.Run(name, func(t *testing.T) {
			_, err, calls := executeSnapshotContract(t, records, baseReport)
			if err == nil || calls != 0 || !strings.Contains(err.Error(), "catalog count") {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}

	t.Run("different music ID with same count", func(t *testing.T) {
		records := append([]snapshotContractRecord{}, baseRecords...)
		records[len(records)-1] = completeSnapshotContractRecord(10, "置換曲", "full")
		_, err, calls := executeSnapshotContract(t, records, baseReport)
		if err == nil || calls != 0 || !strings.Contains(err.Error(), "absent from the complete preflight report") {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
}

func TestCatalogSnapshotContractRejectsNonUniqueClassFingerprintDriftBeforeSource(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	report.Missing[0].CatalogFingerprint = strings.Repeat("f", 64)
	_, err, calls := executeSnapshotContract(t, records, report)
	if err == nil || calls != 0 || !strings.Contains(err.Error(), "fingerprint does not match") {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestCatalogSnapshotContractRejectsStoredFingerprintStaleAgainstChangedEvidenceBeforeSource(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	originalFingerprint := records[5].fingerprint(t)
	records[5].composer = "変更後の作曲者"
	records[5].storedFingerprint = originalFingerprint
	_, err, calls := executeSnapshotContract(t, records, report)
	if err == nil || calls != 0 || !strings.Contains(err.Error(), "stored fingerprint does not match its lyrics evidence") {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestCatalogSnapshotContractRejectsPolicyMismatchBeforeSource(t *testing.T) {
	records := sevenClassSnapshotRecords()
	report := buildSevenClassSnapshotReport(t, records)
	records[4].policyVersion = "catalog-identity-v999"
	_, err, calls := executeSnapshotContract(t, records, report)
	if err == nil || calls != 0 || !strings.Contains(err.Error(), "unsupported lyrics identity policy") {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestCatalogSnapshotContractRejectsTargetAndAssociationDriftBeforeSource(t *testing.T) {
	records := sevenClassSnapshotRecords()
	baseReport := buildSevenClassSnapshotReport(t, records)
	for name, mutate := range map[string]func(*lyricsstaging.PreflightReport){
		"target drift": func(report *lyricsstaging.PreflightReport) {
			report.GameSizeEvidence[0].TargetMusicID = 8
		},
		"association drift": func(report *lyricsstaging.PreflightReport) {
			report.UniqueComplete[0].AssociationMusicIDs = []int{5}
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := baseReport
			report.CatalogReview = append([]lyricsstaging.PreflightItem{}, baseReport.CatalogReview...)
			report.GameSizeEvidence = append([]lyricsstaging.PreflightItem{}, baseReport.GameSizeEvidence...)
			report.UniqueComplete = append([]lyricsstaging.PreflightItem{}, baseReport.UniqueComplete...)
			mutate(&report)
			_, err, calls := executeSnapshotContract(t, records, report)
			if err == nil || calls != 0 || !strings.Contains(err.Error(), "classification does not match") {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}
