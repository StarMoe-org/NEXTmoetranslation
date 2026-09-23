package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsevidencepack"
	"moesekai/server/offline/internal/lyricsimportreceipt"
	"moesekai/server/offline/internal/lyricsrootmanifest"
	"moesekai/server/offline/internal/lyricsstaging"
)

const (
	expectedReceiptSchemaVersion     = 5
	expectedReceiptCommitProtocol    = "durable-precommit-v1"
	expectedReceiptAuditAction       = "lyrics.import_stage.receipt"
	expectedSQLiteStateDigestVersion = "moesekai-sqlite-logical-state-v1"
)

type commandFixture struct {
	directory             string
	validationReceiptPath string
	rootManifestPath      string
	databasePath          string
	manifestPath          string
	evidenceReceiptPath   string
	backupPath            string
	backupSHA256          string
}

type commandAcquisitionSource struct {
	acquisition lyricsacquisition.Acquisition
}

func (source commandAcquisitionSource) ReplayByAcquisitionID(
	_ context.Context,
	acquisitionID lyricsacquisition.AcquisitionID,
) (lyricsacquisition.Acquisition, error) {
	if acquisitionID != source.acquisition.AcquisitionID {
		return lyricsacquisition.Acquisition{}, lyricsacquisition.ErrAcquisitionNotFound
	}
	return source.acquisition, nil
}

func writeCommandFixture(t *testing.T) commandFixture {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(directory, "offline.db")
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	s := store.New(database)
	if _, err := database.Exec(`DELETE FROM catalog_music WHERE music_id=682; DELETE FROM song_lyrics_source_documents WHERE music_id=682;`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]store.PerformerCatalogRecord{{PerformerID: 21, JapaneseName: "初音ミク", EnglishName: "Miku"}}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := s.UpsertMusicCatalog([]store.MusicCatalogRecord{{
		MusicID: 10, JapaneseTitle: "合成試験曲", Lyricist: "作詞者", Composer: "作曲者", Arranger: "編曲者",
		LyricsVersion: "full", LyricsVersionKnown: true,
		Vocals: []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 21}},
	}}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	identity, err := s.CatalogMusicIdentity(10)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Checkpoint(t.Context()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	wikitext := []byte("== Lyrics ==\n初音歌う")
	wikitextSHA1 := sha1.Sum(wikitext)
	wikitextSHA256 := sha256.Sum256(wikitext)
	wikitextSHA256Hex := hex.EncodeToString(wikitextSHA256[:])
	fetchedAt := time.Date(2026, 7, 30, 12, 34, 57, 0, time.UTC)
	fetchedAtText := fetchedAt.Format(time.RFC3339Nano)
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		model.LyricsSourceProviderVocaloidFandom, "fetch:vocaloid-fandom:12", fetchedAtText, wikitextSHA256Hex,
	)
	candidate := lyricsstaging.CandidateIdentity{
		Provider: model.LyricsSourceProviderVocaloidFandom, Origin: model.LyricsSourceOriginVocaloidFandom,
		PageID: 12, RevisionID: 34, SHA1: hex.EncodeToString(wikitextSHA1[:]), Title: "合成試験曲",
		CanonicalURL: "https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34", Categories: []string{"Lyrics", "Songs"},
		Section: "Lyrics/Project SEKAI Version", RenditionKey: "full-sekai",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: evidenceID, SHA256: wikitextSHA256Hex,
		}},
	}
	item := lyricsstaging.PreflightItem{
		MusicID: 10, JapaneseTitle: "合成試験曲", CatalogFingerprint: identity.CatalogFingerprint,
		TargetMusicID: 10, AssociationMusicIDs: []int{}, Candidate: &candidate,
		LineCount: 1, SearchAttempts: 1, FetchAttempts: 1,
	}
	report := lyricsstaging.PreflightReport{
		SchemaVersion: lyricsstaging.PreflightSchemaVersion, GeneratedAt: "2026-07-30T12:34:56Z",
		CatalogSchemaVersion: lyricsstaging.CatalogSchemaVersion, CatalogCount: 1,
		Summary:       lyricsstaging.PreflightSummary{UniqueComplete: 1},
		CatalogReview: []lyricsstaging.PreflightItem{}, GameSizeEvidence: []lyricsstaging.PreflightItem{},
		UniqueComplete: []lyricsstaging.PreflightItem{item}, Ambiguous: []lyricsstaging.PreflightItem{},
		Missing: []lyricsstaging.PreflightItem{}, Incomplete: []lyricsstaging.PreflightItem{}, Error: []lyricsstaging.PreflightItem{},
	}
	fixed := lyricssource.FixedRevision{
		Provider: candidate.Provider, Origin: candidate.Origin,
		PageID: candidate.PageID, RevisionID: candidate.RevisionID, SHA1: candidate.SHA1,
		PageTitle: candidate.Title, CanonicalURL: candidate.CanonicalURL, Categories: candidate.Categories,
		Section: candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
		FetchedAt:         fetchedAt,
		Wikitext:          wikitext, Lines: []lyricssource.ExtractedLine{{Japanese: "初音歌う"}},
		Extraction: lyricssource.Extraction{
			Version:              lyricssource.LyricsVersion{Kind: "sekai", Label: "Project SEKAI Version"},
			Performers:           []lyricssource.Performer{{PerformerID: "歌唱者-21", Name: "初音ミク", Color: "#33CCBB"}},
			RubyGeneratorVersion: "kagome-ipadic-v1",
			Lines: []lyricssource.StructuredLine{{Japanese: "初音歌う", Segments: []lyricssource.LyricsSegment{{
				Text: "初音歌う", PerformerIDs: []string{"歌唱者-21"}, Ruby: []lyricssource.RubySpan{{Text: "初音", Reading: "はつね"}, {Text: "歌", Reading: "うた"}, {Text: "う"}},
			}}, TrailingPerformerIDs: []string{"歌唱者-21"}}},
		},
	}
	evidence := lyricssource.IndexEvidence{
		EvidenceID: candidate.IndexEvidenceRefs[0].EvidenceID, SHA256: candidate.IndexEvidenceRefs[0].SHA256,
		Kind:     lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
		MediaWikiSHA1: candidate.SHA1, Title: candidate.Title, CanonicalURL: candidate.CanonicalURL,
		Categories: append([]string{}, candidate.Categories...), FetchedAt: fetchedAtText,
		Raw: append([]byte{}, wikitext...), RawSHA256: candidate.IndexEvidenceRefs[0].SHA256,
	}
	evidenceReceipt, err := lyricsstaging.NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{evidence})
	if err != nil {
		t.Fatal(err)
	}
	report.EvidenceReceipt = &evidenceReceipt
	draft, err := lyricsstaging.BuildDraft(item, lyricscontract.CatalogIdentity{
		MusicID: identity.MusicID, JapaneseTitle: identity.JapaneseTitle, ProducerMetadata: identity.ProducerMetadata,
		Lyricist: identity.Lyricist, Composer: identity.Composer, Arranger: identity.Arranger,
		Vocals: append([]model.CatalogVocalSignal{}, identity.Vocals...), CatalogFingerprint: identity.CatalogFingerprint,
	}, fixed)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := lyricsstaging.NewManifest(report, strings.Repeat("a", 64), []lyricsstaging.Draft{draft})
	if err != nil {
		t.Fatal(err)
	}
	manifestBody, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "staging.json")
	if err := os.WriteFile(manifestPath, manifestBody, 0o600); err != nil {
		t.Fatal(err)
	}
	evidenceReceiptBody, err := lyricsstaging.MarshalPrivateEvidenceReceipt(evidenceReceipt)
	if err != nil {
		t.Fatal(err)
	}
	evidenceReceiptPath := filepath.Join(directory, "evidence-receipt.json")
	if err := os.WriteFile(evidenceReceiptPath, evidenceReceiptBody, 0o600); err != nil {
		t.Fatal(err)
	}
	rootManifestPath, validationReceiptPath := writeCommandBindingArtifacts(
		t, directory, manifestPath, manifestBody, manifest, evidenceReceiptPath, evidenceReceiptBody, evidenceReceipt, evidence,
	)
	databaseBody, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(directory, "offline.backup.db")
	if err := os.WriteFile(backupPath, databaseBody, 0o600); err != nil {
		t.Fatal(err)
	}
	backupDigest := sha256.Sum256(databaseBody)
	return commandFixture{
		directory: directory, validationReceiptPath: validationReceiptPath, rootManifestPath: rootManifestPath,
		databasePath: databasePath, manifestPath: manifestPath, evidenceReceiptPath: evidenceReceiptPath,
		backupPath: backupPath, backupSHA256: hex.EncodeToString(backupDigest[:]),
	}
}

func writeCommandBindingArtifacts(
	t *testing.T,
	directory, manifestPath string,
	manifestBody []byte,
	manifest lyricsstaging.Manifest,
	evidenceReceiptPath string,
	evidenceReceiptBody []byte,
	evidenceReceipt lyricsstaging.PrivateEvidenceReceipt,
	evidence lyricssource.IndexEvidence,
) (string, string) {
	t.Helper()
	evidenceEnvelope, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidenceEnvelopeDigest := sha256.Sum256(evidenceEnvelope)
	acquisition := lyricsacquisition.Acquisition{
		AcquisitionID: lyricsacquisition.AcquisitionID(strings.Repeat("d", 64)),
		Request: lyricsacquisition.Request{
			Provider: string(evidence.Provider), CanonicalRequestIdentity: "fixture:revision:12:34",
			Kind: lyricsacquisition.RequestKindRevision, RevisionSelector: "34",
		},
		FetchedAt: evidence.FetchedAt, RawResponse: append([]byte(nil), evidence.Raw...),
		RawResponseSHA256: evidence.RawSHA256,
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: evidence.EvidenceID, Raw: append([]byte(nil), evidence.Raw...), RawSHA256: evidence.RawSHA256,
		},
		EvidenceEnvelope: evidenceEnvelope, EvidenceEnvelopeSHA256: hex.EncodeToString(evidenceEnvelopeDigest[:]),
		ReplayOnly: true,
	}
	evidenceRef, err := lyricsevidencepack.EvidenceRefFromAcquisition(acquisition)
	if err != nil {
		t.Fatal(err)
	}
	evidencePackPath := filepath.Join(directory, "evidence-pack")
	pack, err := lyricsevidencepack.Build(t.Context(), evidencePackPath, []lyricscontract.EvidenceRef{evidenceRef},
		commandAcquisitionSource{acquisition: acquisition})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := lyricsevidencepack.OpenResolver(evidencePackPath)
	if err != nil {
		t.Fatal(err)
	}
	musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256([]int{10})
	if err != nil {
		t.Fatal(err)
	}
	root, err := lyricsrootmanifest.Assemble(lyricsrootmanifest.AssemblyRequest{
		RootID: "root-command-fixture",
		Scope:  lyricsrootmanifest.ScopeBinding{Kind: lyricsrootmanifest.ScopeFinal, ScopeID: "scope-command-fixture"},
		Catalog: lyricsrootmanifest.CatalogBinding{
			SchemaVersion: lyricsstaging.CatalogSchemaVersion, RuntimeSchemaVersion: lyricsstaging.MaximumCatalogRuntimeSchema,
			RecordCount: 1, IdentityPolicyVersion: "catalog-identity-v2", SourceSHA256: strings.Repeat("8", 64),
			IdentitySHA256: strings.Repeat("9", 64), MusicIDsSHA256: musicIDsSHA256,
		},
		Plan: lyricsrootmanifest.PlanBinding{PlanID: "plan-command-fixture", SHA256: strings.Repeat("7", 64)},
		Songs: []lyricsrootmanifest.SongResultRef{{
			MusicID: 10, State: lyricscontract.CoverageComplete, ResultSHA256: strings.Repeat("6", 64),
			ProviderOutcomes: []lyricsrootmanifest.ProviderOutcomeRef{{
				Provider: evidence.Provider, OutcomeID: "outcome:command-fixture", SHA256: strings.Repeat("5", 64),
			}},
			SelectedEvidence: []lyricscontract.EvidenceRef{evidenceRef},
		}},
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	rootBody, err := lyricsrootmanifest.MarshalCanonical(root)
	if err != nil {
		t.Fatal(err)
	}
	rootManifestPath := filepath.Join(directory, "root-manifest.json")
	if err := os.WriteFile(rootManifestPath, rootBody, 0o600); err != nil {
		t.Fatal(err)
	}
	rootFileDigest := sha256.Sum256(rootBody)
	manifestFileDigest := sha256.Sum256(manifestBody)
	evidenceFileDigest := sha256.Sum256(evidenceReceiptBody)
	path := func(name string) string { return filepath.Join(directory, name) }
	validationReceiptPath := path("validation-receipt.json")
	validationReceipt := lyricsimportreceipt.ValidationReceipt{
		SchemaVersion: lyricsimportreceipt.ValidationReceiptSchemaVersion,
		Kind:          lyricsimportreceipt.ValidationReceiptKind,
		Plan: lyricsimportreceipt.ValidationPlanBinding{
			File: lyricsimportreceipt.ValidationFileBinding{
				Path: path("plan.json"), SHA256: root.Plan.SHA256, ByteCount: 1,
			},
			PlanID: root.Plan.PlanID, PlanSHA256: root.Plan.SHA256, SourceRoot: path("source-root"),
			SourceSnapshotSHA256: strings.Repeat("4", 64), SourceFileCount: 1,
		},
		Catalog: lyricsimportreceipt.ValidationCatalogBinding{
			File: lyricsimportreceipt.ValidationFileBinding{
				Path: path("catalog.db"), SHA256: root.Catalog.SourceSHA256, ByteCount: 1,
			},
			RecordCount: root.Catalog.RecordCount, IdentitySHA256: root.Catalog.IdentitySHA256,
			MusicIDsSHA256: root.Catalog.MusicIDsSHA256, IdentityPolicyVersion: root.Catalog.IdentityPolicyVersion,
		},
		Ledger: lyricsimportreceipt.ValidationTreeBinding{
			Path: path("ledger"), SHA256: strings.Repeat("3", 64), FileCount: 1, ByteCount: 1,
		},
		AcquisitionSet: lyricsimportreceipt.ValidationAcquisitionSetBinding{
			File: lyricsimportreceipt.ValidationFileBinding{
				Path: path("acquisition-set.json"), SHA256: strings.Repeat("2", 64), ByteCount: 1,
			},
			SetSHA256: strings.Repeat("1", 64),
		},
		ProviderOutcomes: lyricsimportreceipt.ValidationTreeBinding{
			Path: path("provider-outcomes"), SHA256: strings.Repeat("a", 64), FileCount: 1, ByteCount: 1,
		},
		SongResults: lyricsimportreceipt.ValidationTreeBinding{
			Path: path("song-results"), SHA256: strings.Repeat("b", 64), FileCount: 1, ByteCount: 1,
		},
		EvidencePack: lyricsimportreceipt.ValidationEvidencePackBinding{
			Tree: lyricsimportreceipt.ValidationTreeBinding{
				Path: evidencePackPath, SHA256: strings.Repeat("c", 64), FileCount: pack.Totals.ShardCount + 1, ByteCount: 1,
			},
			PackSHA256: pack.PackSHA256, SelectionSHA256: pack.SelectionSHA256,
			EvidenceCount: pack.Totals.ItemCount, ShardCount: pack.Totals.ShardCount,
		},
		RootManifest: lyricsimportreceipt.ValidationRootBinding{
			File: lyricsimportreceipt.ValidationFileBinding{
				Path: rootManifestPath, SHA256: hex.EncodeToString(rootFileDigest[:]), ByteCount: int64(len(rootBody)),
			},
			RootID: root.RootID, RootSHA256: root.RootSHA256,
		},
		ImportManifest: lyricsimportreceipt.ValidationImportManifestBinding{
			File: lyricsimportreceipt.ValidationFileBinding{
				Path: manifestPath, SHA256: hex.EncodeToString(manifestFileDigest[:]), ByteCount: int64(len(manifestBody)),
			},
			BatchSHA256: manifest.BatchSHA256, ItemCount: len(manifest.Items),
		},
		ImportEvidence: lyricsimportreceipt.ValidationImportEvidenceBinding{
			File: lyricsimportreceipt.ValidationFileBinding{
				Path: evidenceReceiptPath, SHA256: hex.EncodeToString(evidenceFileDigest[:]), ByteCount: int64(len(evidenceReceiptBody)),
			},
			ReceiptSHA256: evidenceReceipt.ReceiptSHA256, EvidenceCount: len(evidenceReceipt.IndexEvidence),
		},
		AcquisitionCount: root.Coverage.UniqueAcquisitionCount, ProviderOutcomeCount: root.Coverage.ProviderOutcomeRefCount,
		ReceiptPath: validationReceiptPath,
	}
	validationReceipt.ReceiptSHA256, err = lyricsimportreceipt.ValidationReceiptDigest(validationReceipt)
	if err != nil {
		t.Fatal(err)
	}
	validationBody, err := lyricsimportreceipt.MarshalValidationReceipt(validationReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(validationReceiptPath, validationBody, 0o600); err != nil {
		t.Fatal(err)
	}
	return rootManifestPath, validationReceiptPath
}

func commandArguments(fixture commandFixture, receiptPath string) []string {
	return []string{
		"-validation-receipt", fixture.validationReceiptPath, "-root-manifest", fixture.rootManifestPath,
		"-manifest", fixture.manifestPath, "-evidence-receipt", fixture.evidenceReceiptPath,
		"-db", fixture.databasePath,
		"-backup", fixture.backupPath, "-backup-sha256", fixture.backupSHA256,
		"-receipt", receiptPath, "-operator", "offline-operator", "-confirm-local-offline",
	}
}

func commandOptions(fixture commandFixture, receiptPath string) options {
	return options{
		ValidationReceiptPath: fixture.validationReceiptPath, RootManifestPath: fixture.rootManifestPath,
		ManifestPath: fixture.manifestPath, EvidenceReceiptPath: fixture.evidenceReceiptPath,
		DatabasePath: fixture.databasePath,
		BackupPath:   fixture.backupPath, BackupSHA256: fixture.backupSHA256,
		ReceiptPath: receiptPath, Operator: "offline-operator", ConfirmLocalOffline: true,
	}
}

func refreshCommandBackup(t *testing.T, fixture *commandFixture) {
	t.Helper()
	body, err := os.ReadFile(fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.backupPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	fixture.backupSHA256 = hex.EncodeToString(digest[:])
}

func TestRunImportsOfflineManifestWritesExclusiveReceiptAndReplays(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "")
	fixture := writeCommandFixture(t)
	firstReceiptPath := filepath.Join(fixture.directory, "first-receipt.json")
	var output bytes.Buffer
	if err := run(commandArguments(fixture, firstReceiptPath), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "imported 1 and replayed 0") {
		t.Fatalf("first output=%q", output.String())
	}
	firstInfo, err := os.Stat(firstReceiptPath)
	if err != nil || firstInfo.Mode().Perm() != 0o600 {
		t.Fatalf("first receipt info=%v err=%v", firstInfo, err)
	}
	var first importReceipt
	body, err := os.ReadFile(firstReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	evidenceReceiptBody, err := os.ReadFile(fixture.evidenceReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	evidenceReceipt, err := lyricsstaging.DecodePrivateEvidenceReceipt(evidenceReceiptBody)
	if err != nil {
		t.Fatal(err)
	}
	validationBody, err := os.ReadFile(fixture.validationReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	validationReceipt, err := lyricsimportreceipt.DecodeValidationReceipt(validationBody, fixture.validationReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	rootBody, err := os.ReadFile(fixture.rootManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := lyricsrootmanifest.DecodeCanonical(rootBody)
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := sha256.Sum256(rootBody)
	if first.SchemaVersion != expectedReceiptSchemaVersion || first.CommitProtocol != expectedReceiptCommitProtocol || first.DatabaseAuditAction != expectedReceiptAuditAction ||
		first.ValidationReceiptSHA256 != validationReceipt.ReceiptSHA256 ||
		first.RootManifestSHA256 != hex.EncodeToString(rootDigest[:]) || first.RootID != root.RootID || first.RootSHA256 != root.RootSHA256 ||
		first.ImportedCount != 1 || first.ReplayedCount != 0 || first.BackupSHA256 != fixture.backupSHA256 ||
		first.StateDigestVersion != expectedSQLiteStateDigestVersion || first.BackupStateSHA256 == "" ||
		first.BackupStateSHA256 != first.PreImportDatabaseStateSHA256 ||
		first.PreImportDatabaseSHA256 == "" || first.RecoveryDatabasePath == "" || first.ReceiptPath != firstReceiptPath ||
		first.EvidenceReceiptPath != fixture.evidenceReceiptPath || first.EvidenceReceiptSHA256 != evidenceReceipt.ReceiptSHA256 ||
		len(first.Items) != 1 || !first.Items[0].Changed {
		t.Fatalf("first receipt=%+v", first)
	}
	if _, err := os.Lstat(first.RecoveryDatabasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful import retained recovery database err=%v", err)
	}

	database, err := db.Open(fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	lyrics, err := store.New(database).GetLyrics(10)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if lyrics.Revision != 1 || lyrics.Status != "draft" || lyrics.Lines[0].Chinese != "" || lyrics.Lines[0].English != "" ||
		fmt.Sprint(lyrics.Lines[0].Segments[0].PerformerIDs) != "[21]" {
		database.Close()
		t.Fatalf("imported lyrics=%+v", lyrics)
	}
	var evidenceParents, evidenceLinks int
	var linkedRaw []byte
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM lyrics_source_index_evidence),
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence),
		(SELECT evidence.raw_bytes FROM song_lyrics_source_artifact_index_evidence link
		 JOIN lyrics_source_index_evidence evidence
		   ON evidence.provider=link.provider AND evidence.evidence_id=link.evidence_id AND evidence.sha256=link.sha256
		 LIMIT 1)`).Scan(&evidenceParents, &evidenceLinks, &linkedRaw); err != nil ||
		evidenceParents != 1 || evidenceLinks != 1 || string(linkedRaw) != string(evidenceReceipt.IndexEvidence[0].Raw) {
		database.Close()
		t.Fatalf("evidence parents=%d links=%d raw=%q err=%v", evidenceParents, evidenceLinks, linkedRaw, err)
	}
	if err := database.Checkpoint(t.Context()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	refreshCommandBackup(t, &fixture)
	secondReceiptPath := filepath.Join(fixture.directory, "second-receipt.json")
	output.Reset()
	if err := run(commandArguments(fixture, secondReceiptPath), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "imported 0 and replayed 1") {
		t.Fatalf("replay output=%q", output.String())
	}
	database, err = db.Open(fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM lyrics_source_index_evidence),
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence)`).Scan(&evidenceParents, &evidenceLinks); err != nil ||
		evidenceParents != 1 || evidenceLinks != 1 {
		t.Fatalf("replayed evidence parents=%d links=%d err=%v", evidenceParents, evidenceLinks, err)
	}
}

func TestPinnedSQLiteAnchorBindsWritesToInspectedInodeAndDetectsPathSwap(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(directory, "offline.db")
	original, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Checkpoint(t.Context()); err != nil {
		original.Close()
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := inspectExistingRegular(databasePath, "offline database")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := openPinnedWritableFile(databasePath, info, "offline database")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.close()
	anchor, err := createPinnedSQLiteAnchor(pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.close()

	displacedPath := filepath.Join(directory, "inspected-inode.db")
	if err := os.Rename(databasePath, displacedPath); err != nil {
		t.Fatal(err)
	}
	replacement, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Exec(`INSERT INTO settings(key,value) VALUES ('replacement','yes')`); err != nil {
		replacement.Close()
		t.Fatal(err)
	}
	if err := replacement.Checkpoint(t.Context()); err != nil {
		replacement.Close()
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}

	anchored, err := db.Open(anchor.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.verifyPinned(pinned, "after pathname swap"); err != nil {
		anchored.Close()
		t.Fatal(err)
	}
	if err := pinned.verifySamePath("after pathname swap", false); err == nil {
		anchored.Close()
		t.Fatal("operator pathname swap was not detected before import")
	}
	if _, err := anchored.Exec(`INSERT INTO settings(key,value) VALUES ('anchored','yes')`); err != nil {
		anchored.Close()
		t.Fatal(err)
	}
	if err := anchored.Checkpoint(t.Context()); err != nil {
		anchored.Close()
		t.Fatal(err)
	}
	if err := anchored.Close(); err != nil {
		t.Fatal(err)
	}
	if err := anchor.verifyPinned(pinned, "after anchored write"); err != nil {
		t.Fatal(err)
	}
	if err := anchor.close(); err != nil {
		t.Fatal(err)
	}
	if err := pinned.close(); err != nil {
		t.Fatal(err)
	}

	readSetting := func(path, key string) (string, error) {
		database, err := db.Open(path)
		if err != nil {
			return "", err
		}
		defer database.Close()
		var value string
		err = database.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
		return value, err
	}
	if value, err := readSetting(displacedPath, "anchored"); err != nil || value != "yes" {
		t.Fatalf("pinned inode anchored marker=%q err=%v", value, err)
	}
	if value, err := readSetting(databasePath, "replacement"); err != nil || value != "yes" {
		t.Fatalf("replacement marker=%q err=%v", value, err)
	}
	if value, err := readSetting(databasePath, "anchored"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replacement pathname received anchored write=%q err=%v", value, err)
	}
}

func TestRunRejectsProductionWrongBackupAndExistingReceiptWithoutImport(t *testing.T) {
	t.Run("production", func(t *testing.T) {
		t.Setenv("MOESEKAI_PRODUCTION", "true")
		fixture := writeCommandFixture(t)
		err := run(commandArguments(fixture, filepath.Join(fixture.directory, "receipt.json")), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "refuses MOESEKAI_PRODUCTION") {
			t.Fatalf("production error=%v", err)
		}
	})

	for name, mutate := range map[string]func(*commandFixture, string){
		"wrong backup": func(fixture *commandFixture, _ string) { fixture.backupSHA256 = strings.Repeat("f", 64) },
		"existing receipt": func(_ *commandFixture, receiptPath string) {
			if err := os.WriteFile(receiptPath, []byte("operator-owned"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MOESEKAI_PRODUCTION", "")
			fixture := writeCommandFixture(t)
			receiptPath := filepath.Join(fixture.directory, "receipt.json")
			mutate(&fixture, receiptPath)
			err := run(commandArguments(fixture, receiptPath), &bytes.Buffer{})
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", name)
			}
			database, openErr := db.Open(fixture.databasePath)
			if openErr != nil {
				t.Fatal(openErr)
			}
			_, lyricsErr := store.New(database).GetLyrics(10)
			if !errors.Is(lyricsErr, store.ErrLyricsNotFound) {
				database.Close()
				t.Fatalf("%s imported lyrics despite refusal: %v", name, lyricsErr)
			}
			if checkpointErr := database.Checkpoint(t.Context()); checkpointErr != nil {
				database.Close()
				t.Fatal(checkpointErr)
			}
			if closeErr := database.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		})
	}
}

func TestRunRejectsUnboundValidationInputsBeforeMutationOrReceiptReservation(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "")
	t.Run("missing validation receipt", func(t *testing.T) {
		fixture := writeCommandFixture(t)
		receiptPath := filepath.Join(fixture.directory, "missing-binding-receipt.json")
		arguments := commandArguments(fixture, receiptPath)
		arguments = arguments[2:]
		if err := run(arguments, &bytes.Buffer{}); err == nil {
			t.Fatal("missing immutable validation receipt was accepted")
		}
		assertNoImportedLyrics(t, fixture.databasePath)
		if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing-binding receipt exists: %v", err)
		}
	})

	t.Run("unknown validation field", func(t *testing.T) {
		fixture := writeCommandFixture(t)
		receiptPath := filepath.Join(fixture.directory, "unknown-binding-receipt.json")
		body, err := os.ReadFile(fixture.validationReceiptPath)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.Replace(body, []byte(`"schemaVersion": 1,`), []byte("\"schemaVersion\": 1,\n  \"unknownBinding\": \"forbidden\","), 1)
		if err := os.WriteFile(fixture.validationReceiptPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run(commandArguments(fixture, receiptPath), &bytes.Buffer{}); err == nil {
			t.Fatal("validation receipt with an unknown field was accepted")
		}
		assertNoImportedLyrics(t, fixture.databasePath)
		if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unknown-binding receipt exists: %v", err)
		}
	})

	for name, mutateBody := range map[string]func([]byte) []byte{
		"excessive validation depth": func(body []byte) []byte {
			return bytes.Replace(body, []byte(`"schemaVersion": 1,`), []byte("\"schemaVersion\": 1,\n  \"deep\": "+strings.Repeat("[", lyricsimportreceipt.MaxJSONDepth+1)+"0"+strings.Repeat("]", lyricsimportreceipt.MaxJSONDepth+1)+","), 1)
		},
		"invalid validation UTF-8": func(body []byte) []byte {
			mutated := append([]byte(nil), body...)
			mutated[len(mutated)/2] = 0xff
			return mutated
		},
		"oversized validation receipt": func([]byte) []byte {
			return bytes.Repeat([]byte{'x'}, lyricsimportreceipt.MaxValidationReceiptBytes+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := writeCommandFixture(t)
			receiptPath := filepath.Join(fixture.directory, "bounded-binding-receipt.json")
			body, err := os.ReadFile(fixture.validationReceiptPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.validationReceiptPath, mutateBody(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := run(commandArguments(fixture, receiptPath), &bytes.Buffer{}); err == nil {
				t.Fatal("invalid bounded validation receipt was accepted")
			}
			assertNoImportedLyrics(t, fixture.databasePath)
			if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bounded-binding receipt exists: %v", err)
			}
		})
	}

	t.Run("noncanonical manifest even when file hash is rebound", func(t *testing.T) {
		fixture := writeCommandFixture(t)
		receiptPath := filepath.Join(fixture.directory, "noncanonical-manifest-receipt.json")
		manifestBody, err := os.ReadFile(fixture.manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, manifestBody); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.manifestPath, compact.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		validationBody, err := os.ReadFile(fixture.validationReceiptPath)
		if err != nil {
			t.Fatal(err)
		}
		validation, err := lyricsimportreceipt.DecodeValidationReceipt(validationBody, fixture.validationReceiptPath)
		if err != nil {
			t.Fatal(err)
		}
		manifestDigest := sha256.Sum256(compact.Bytes())
		validation.ImportManifest.File.SHA256 = hex.EncodeToString(manifestDigest[:])
		validation.ImportManifest.File.ByteCount = int64(compact.Len())
		validation.ReceiptSHA256 = ""
		validation.ReceiptSHA256, err = lyricsimportreceipt.ValidationReceiptDigest(validation)
		if err != nil {
			t.Fatal(err)
		}
		validationBody, err = lyricsimportreceipt.MarshalValidationReceipt(validation)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.validationReceiptPath, validationBody, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run(commandArguments(fixture, receiptPath), &bytes.Buffer{}); err == nil {
			t.Fatal("noncanonical manifest was accepted after rebinding its file hash")
		}
		assertNoImportedLyrics(t, fixture.databasePath)
		if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("noncanonical-manifest receipt exists: %v", err)
		}
	})

	for name, mutate := range map[string]func(*lyricsimportreceipt.ValidationReceipt){
		"manifest file digest mismatch": func(receipt *lyricsimportreceipt.ValidationReceipt) {
			receipt.ImportManifest.File.SHA256 = strings.Repeat("f", 64)
		},
		"compact root identity mismatch": func(receipt *lyricsimportreceipt.ValidationReceipt) {
			receipt.RootManifest.RootID = "root-crossed-fixture"
		},
		"evidence receipt digest mismatch": func(receipt *lyricsimportreceipt.ValidationReceipt) {
			receipt.ImportEvidence.ReceiptSHA256 = strings.Repeat("e", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := writeCommandFixture(t)
			receiptPath := filepath.Join(fixture.directory, "mismatched-binding-receipt.json")
			body, err := os.ReadFile(fixture.validationReceiptPath)
			if err != nil {
				t.Fatal(err)
			}
			validation, err := lyricsimportreceipt.DecodeValidationReceipt(body, fixture.validationReceiptPath)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&validation)
			validation.ReceiptSHA256 = ""
			validation.ReceiptSHA256, err = lyricsimportreceipt.ValidationReceiptDigest(validation)
			if err != nil {
				t.Fatal(err)
			}
			body, err = lyricsimportreceipt.MarshalValidationReceipt(validation)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.validationReceiptPath, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := run(commandArguments(fixture, receiptPath), &bytes.Buffer{}); err == nil {
				t.Fatal("mismatched immutable binding was accepted")
			}
			assertNoImportedLyrics(t, fixture.databasePath)
			if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("mismatched-binding receipt exists: %v", err)
			}
		})
	}
}

func TestRunRejectsValidUnrelatedSQLiteBackupWithMatchingSelfSuppliedHash(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "")
	fixture := writeCommandFixture(t)
	unrelatedPath := filepath.Join(fixture.directory, "valid-unrelated.db")
	unrelated, err := db.Open(unrelatedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unrelated.Exec(`INSERT INTO settings(key,value) VALUES ('unrelated','valid sqlite')`); err != nil {
		unrelated.Close()
		t.Fatal(err)
	}
	if err := unrelated.Checkpoint(t.Context()); err != nil {
		unrelated.Close()
		t.Fatal(err)
	}
	if err := unrelated.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(unrelatedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.backupPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	fixture.backupSHA256 = hex.EncodeToString(digest[:])
	receiptPath := filepath.Join(fixture.directory, "unrelated-receipt.json")
	err = run(commandArguments(fixture, receiptPath), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "does not match the exact pre-import") {
		t.Fatalf("unrelated backup error=%v", err)
	}
	assertNoImportedLyrics(t, fixture.databasePath)
	if _, statErr := os.Lstat(receiptPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unrelated backup receipt exists err=%v", statErr)
	}
}

func TestRunAcceptsLogicallyExactBackupWithDifferentSQLiteRepresentation(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "")
	fixture := writeCommandFixture(t)
	database, err := db.Open(fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("representation-only-freelist-data", 4096)
	if _, err := database.Exec(`INSERT INTO settings(key,value) VALUES ('temporary-representation',?)`, payload); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM settings WHERE key='temporary-representation'`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Checkpoint(t.Context()); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.backupPath); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", "file:"+fixture.databasePath+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`VACUUM INTO ?`, fixture.backupPath); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	databaseBody, err := os.ReadFile(fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	backupBody, err := os.ReadFile(fixture.backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(databaseBody, backupBody) {
		t.Fatal("logical backup regression did not create a distinct SQLite representation")
	}
	digest := sha256.Sum256(backupBody)
	fixture.backupSHA256 = hex.EncodeToString(digest[:])
	receiptPath := filepath.Join(fixture.directory, "logical-receipt.json")
	if err := run(commandArguments(fixture, receiptPath), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteFailuresAreFailClosedAroundCommitReceiptBoundary(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "")
	t.Run("checkpoint failure retains durable receipt for committed mutation", func(t *testing.T) {
		fixture := writeCommandFixture(t)
		receiptPath := filepath.Join(fixture.directory, "checkpoint-receipt.json")
		_, err := executeWithHooks(t.Context(), commandOptions(fixture, receiptPath), executionHooks{
			checkpoint: func(context.Context, *db.DB) error {
				return errors.New("injected checkpoint failure")
			},
		})
		if err == nil || !strings.Contains(err.Error(), "injected checkpoint failure") ||
			!strings.Contains(err.Error(), "durable receipt preparation") {
			t.Fatalf("checkpoint failure error=%v", err)
		}
		body, readErr := os.ReadFile(receiptPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var receipt importReceipt
		if err := json.Unmarshal(body, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.CommitProtocol != expectedReceiptCommitProtocol || receipt.ImportedCount != 1 || receipt.RecoveryDatabasePath == "" {
			t.Fatalf("checkpoint receipt=%+v", receipt)
		}
		if _, err := os.Stat(receipt.RecoveryDatabasePath); err != nil {
			t.Fatalf("checkpoint recovery database: %v", err)
		}
		assertImportedLyricsAndReceiptAudit(t, receipt.RecoveryDatabasePath, receiptPath, body)
	})

	t.Run("path swap before receipt preparation rolls back", func(t *testing.T) {
		fixture := writeCommandFixture(t)
		receiptPath := filepath.Join(fixture.directory, "path-receipt.json")
		displacedPath := filepath.Join(fixture.directory, "path-displaced.db")
		_, err := executeWithHooks(t.Context(), commandOptions(fixture, receiptPath), executionHooks{
			beforeCommitValidation: func() error {
				return os.Rename(fixture.databasePath, displacedPath)
			},
		})
		if err == nil || !strings.Contains(err.Error(), "path or inode changed") {
			t.Fatalf("path failure error=%v", err)
		}
		if _, statErr := os.Lstat(receiptPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("path failure receipt exists err=%v", statErr)
		}
		if err := os.Rename(displacedPath, fixture.databasePath); err != nil {
			t.Fatal(err)
		}
		assertNoImportedLyrics(t, fixture.databasePath)
	})

	t.Run("validated binding mutation before receipt preparation rolls back", func(t *testing.T) {
		fixture := writeCommandFixture(t)
		receiptPath := filepath.Join(fixture.directory, "binding-mutation-receipt.json")
		_, err := executeWithHooks(t.Context(), commandOptions(fixture, receiptPath), executionHooks{
			beforeCommitValidation: func() error {
				body, readErr := os.ReadFile(fixture.validationReceiptPath)
				if readErr != nil {
					return readErr
				}
				body[len(body)/2] ^= 1
				return os.WriteFile(fixture.validationReceiptPath, body, 0o600)
			},
		})
		if err == nil || !strings.Contains(err.Error(), "release validation receipt") {
			t.Fatalf("binding mutation error=%v", err)
		}
		if _, statErr := os.Lstat(receiptPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("binding mutation receipt exists err=%v", statErr)
		}
		assertNoImportedLyrics(t, fixture.databasePath)
	})

	t.Run("receipt publication failure rolls back", func(t *testing.T) {
		fixture := writeCommandFixture(t)
		receiptPath := filepath.Join(fixture.directory, "publish-receipt.json")
		_, err := executeWithHooks(t.Context(), commandOptions(fixture, receiptPath), executionHooks{
			afterReceiptReserve: func() error {
				return errors.New("injected receipt publication failure")
			},
		})
		if err == nil || !strings.Contains(err.Error(), "injected receipt publication failure") {
			t.Fatalf("receipt failure error=%v", err)
		}
		if _, statErr := os.Lstat(receiptPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("receipt failure path exists err=%v", statErr)
		}
		assertNoImportedLyrics(t, fixture.databasePath)
	})
}

func assertNoImportedLyrics(t *testing.T, databasePath string) {
	t.Helper()
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := store.New(database).GetLyrics(10); !errors.Is(err, store.ErrLyricsNotFound) {
		t.Fatalf("unexpected imported lyrics: %v", err)
	}
	var receipts int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=?`, expectedReceiptAuditAction).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("rolled-back import left %d transactional receipts", receipts)
	}
}

func assertImportedLyricsAndReceiptAudit(t *testing.T, databasePath, receiptPath string, receiptBody []byte) {
	t.Helper()
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	lyrics, err := store.New(database).GetLyrics(10)
	if err != nil || lyrics.Revision != 1 {
		t.Fatalf("committed lyrics=%+v err=%v", lyrics, err)
	}
	var detail string
	if err := database.QueryRow(`SELECT detail FROM audit_log WHERE action=?`, expectedReceiptAuditAction).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var audit importReceiptAudit
	if err := json.Unmarshal([]byte(detail), &audit); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(receiptBody)
	if audit.ReceiptPath != receiptPath || audit.ReceiptSHA256 != hex.EncodeToString(digest[:]) || audit.ReceiptJSON != string(receiptBody) {
		t.Fatalf("transactional receipt audit=%+v", audit)
	}
}
