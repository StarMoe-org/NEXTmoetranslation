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
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

type fakeSourceClient struct {
	fetchCandidate func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error)
}

func (fake fakeSourceClient) FetchFixedCandidateRevision(ctx context.Context, identity lyricssource.MusicIdentity, candidate lyricssource.Candidate) (lyricssource.FixedRevision, error) {
	if fake.fetchCandidate == nil {
		return lyricssource.FixedRevision{}, errors.New("unexpected fixed-revision fetch")
	}
	return fake.fetchCandidate(ctx, identity, candidate)
}

func stageIndexEvidence(candidate lyricsstaging.CandidateIdentity, raw []byte, fetchedAt time.Time) lyricssource.IndexEvidence {
	digest := sha256.Sum256(raw)
	sha := hex.EncodeToString(digest[:])
	fetchedAtText := fetchedAt.UTC().Format(time.RFC3339Nano)
	baseID := fmt.Sprintf("fetch:vocaloid-fandom:%d", candidate.PageID)
	switch candidate.Provider {
	case model.LyricsSourceProviderMoegirl:
		baseID = fmt.Sprintf("search:moegirl:%d", candidate.PageID)
	case model.LyricsSourceProviderSekaipedia:
		baseID = fmt.Sprintf("revision:sekaipedia:%d:%d", candidate.PageID, candidate.RevisionID)
	}
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		candidate.Provider, baseID, fetchedAtText, sha,
	)
	return lyricssource.IndexEvidence{
		EvidenceID: evidenceID, SHA256: sha,
		Kind:     lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, MediaWikiSHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		FetchedAt: fetchedAtText, Raw: append([]byte{}, raw...), RawSHA256: sha,
	}
}

func hydratedStageCandidate(t *testing.T, report lyricsstaging.PreflightReport, candidate lyricsstaging.CandidateIdentity) lyricssource.Candidate {
	t.Helper()
	if report.EvidenceReceipt == nil {
		t.Fatal("stage report has no evidence receipt")
	}
	hydrated, err := report.EvidenceReceipt.HydrateCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return hydrated
}

func writeLocalInputs(t *testing.T) (options, lyricsstaging.PreflightReport, lyricssource.FixedRevision) {
	t.Helper()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "catalog.db")
	database, err := sql.Open("sqlite", "file:"+databasePath+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY);
CREATE TABLE catalog_music (
 music_id INTEGER PRIMARY KEY,
 title_ja TEXT NOT NULL,
 producer_metadata TEXT NOT NULL,
 lyricist TEXT NOT NULL,
 composer TEXT NOT NULL,
 arranger TEXT NOT NULL,
 assetbundle_name TEXT NOT NULL,
 version_hint TEXT NOT NULL,
 lyrics_version TEXT NOT NULL,
 lyrics_evidence_presence_json TEXT NOT NULL,
 vocal_signals_json TEXT NOT NULL,
 lyrics_catalog_fingerprint TEXT NOT NULL,
 lyrics_catalog_policy_version TEXT NOT NULL
);`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	for version := 1; version <= lyricsstaging.CatalogSchemaVersion; version++ {
		if _, err := database.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, version); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	evidence := model.CatalogLyricsEvidence{
		Title: "合成試験曲", Lyricist: "制作者", Composer: "制作者", Arranger: "制作者",
		LyricsVersion: "full", Vocals: []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai"}}, Presence: model.CatalogEvidencePresence{
			Lyricist: true, Composer: true, Arranger: true, LyricsVersion: true,
		},
	}
	fingerprint, err := model.CatalogLyricsEvidenceFingerprint(evidence)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	presenceJSON, err := json.Marshal(evidence.Presence)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	vocalsJSON, err := json.Marshal(evidence.Vocals)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO catalog_music VALUES (10,'合成試験曲','制作者','制作者','制作者','制作者','','','full',?,?,?,?)`,
		string(presenceJSON), string(vocalsJSON), fingerprint, model.LyricsCatalogIdentityPolicyVersion); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	wikitext := []byte("== Lyrics ==\n初音歌う")
	wikitextDigest := sha1.Sum(wikitext)
	candidate := lyricsstaging.CandidateIdentity{
		Provider: model.LyricsSourceProviderVocaloidFandom, Origin: model.LyricsSourceOriginVocaloidFandom,
		PageID: 12, RevisionID: 34, SHA1: hex.EncodeToString(wikitextDigest[:]), Title: "合成試験曲",
		CanonicalURL: "https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34",
		Categories:   []string{"Lyrics", "Songs"}, Section: "Lyrics/Project SEKAI Version", RenditionKey: "full-sekai",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "search:vocaloid-fandom:12", SHA256: strings.Repeat("a", 64),
		}},
	}
	indexEvidence := stageIndexEvidence(candidate, wikitext, time.Unix(120, 0).UTC())
	candidate.IndexEvidenceRefs[0] = model.LyricsSourceIndexEvidenceRef{
		EvidenceID: indexEvidence.EvidenceID, SHA256: indexEvidence.SHA256,
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{indexEvidence})
	if err != nil {
		t.Fatal(err)
	}
	item := lyricsstaging.PreflightItem{MusicID: 10, JapaneseTitle: "合成試験曲", CatalogFingerprint: fingerprint,
		TargetMusicID: 10, AssociationMusicIDs: []int{}, Candidate: &candidate,
		FixedArtifactCandidates: []lyricsstaging.CandidateIdentity{candidate},
		PostFetchState:          lyricsstaging.PostFetchStateComplete,
		CompositionReason:       model.LyricsSourceVersionReasonUntaggedFullOnly,
		LineCount:               1, SearchAttempts: 1, FetchAttempts: 1}
	report := lyricsstaging.PreflightReport{SchemaVersion: 1, GeneratedAt: time.Unix(123, 0).UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: 18, CatalogCount: 1, Summary: lyricsstaging.PreflightSummary{UniqueComplete: 1},
		EvidenceReceipt: &receipt,
		CatalogReview:   []lyricsstaging.PreflightItem{}, GameSizeEvidence: []lyricsstaging.PreflightItem{},
		UniqueComplete: []lyricsstaging.PreflightItem{item}, Ambiguous: []lyricsstaging.PreflightItem{},
		Missing: []lyricsstaging.PreflightItem{}, Incomplete: []lyricsstaging.PreflightItem{}, Error: []lyricsstaging.PreflightItem{}}
	reportBody, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	reportBody = append(reportBody, '\n')
	reportPath := filepath.Join(directory, "preflight.json")
	if err := os.WriteFile(reportPath, reportBody, 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := lyricssource.FixedRevision{
		Provider: candidate.Provider, Origin: candidate.Origin,
		PageID: candidate.PageID, RevisionID: candidate.RevisionID, SHA1: candidate.SHA1, PageTitle: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		Section: candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
		IndexEvidence:     []lyricssource.IndexEvidence{indexEvidence},
		FetchedAt:         time.Unix(456, 0).UTC(), Wikitext: wikitext,
		Lines: []lyricssource.ExtractedLine{{Japanese: "初音歌う"}},
		Extraction: lyricssource.Extraction{
			Version:              lyricssource.LyricsVersion{Kind: "sekai", Label: "Project SEKAI Version"},
			Performers:           []lyricssource.Performer{{PerformerID: "歌唱者-21", Name: "初音ミク", Color: "#33CCBB"}},
			RubyGeneratorVersion: "kagome-ipadic-v1",
			Lines: []lyricssource.StructuredLine{{
				Japanese: "初音歌う",
				Segments: []lyricssource.LyricsSegment{{
					Text: "初音歌う", PerformerIDs: []string{"歌唱者-21"},
					Ruby: []lyricssource.RubySpan{{Text: "初音", Reading: "はつね"}, {Text: "歌", Reading: "うた"}, {Text: "う"}},
				}},
				TrailingPerformerIDs: []string{"歌唱者-21"},
			}},
		},
	}
	return options{ReportPath: reportPath, DatabasePath: databasePath, OutputPath: filepath.Join(directory, "staging.json"),
		Concurrency: 2, MaxAttempts: 2, RequestTimeout: time.Second, RetryDelay: time.Microsecond}, report, fixed
}

func rewriteStagePreflight(t *testing.T, path string, report lyricsstaging.PreflightReport) {
	t.Helper()
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultCommandDependenciesUseProviderRegistry(t *testing.T) {
	source, err := defaultCommandDependencies().NewSourceClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := source.(*lyricssource.Registry); !ok {
		t.Fatalf("default lyrics source=%T, want *lyricssource.Registry", source)
	}
}

func TestRequestTimeoutDefaultsHelpAndBounds(t *testing.T) {
	if defaultRequestTimeout != 8*time.Minute {
		t.Fatalf("default request timeout=%s, want 8m", defaultRequestTimeout)
	}
	if maxRequestTimeout != 10*time.Minute {
		t.Fatalf("maximum request timeout=%s, want 10m", maxRequestTimeout)
	}

	var help bytes.Buffer
	if err := run([]string{"-h"}, &help); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error=%v", err)
	}
	for _, text := range []string{
		"-request-timeout", "client-side limiter waits", "(default 8m0s)",
		"-max-attempts", "(default 3)", "-retry-delay", "(default 250ms)",
		"-evidence-receipt-output", "EvidenceReceipt-v1", "lyrics-import-stage -evidence-receipt",
	} {
		if !strings.Contains(help.String(), text) {
			t.Fatalf("help missing %q: %s", text, help.String())
		}
	}

	opts, _, _ := writeLocalInputs(t)
	opts.RequestTimeout = maxRequestTimeout
	if err := validateOptions(opts); err != nil {
		t.Fatalf("maximum request timeout rejected: %v", err)
	}
	opts.RequestTimeout = maxRequestTimeout + time.Nanosecond
	if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "at most 10m") {
		t.Fatalf("over-limit request timeout error=%v", err)
	}
}

func TestFixedRevisionOperationReceivesFullRequestTimeoutBudget(t *testing.T) {
	opts, _, fixed := writeLocalInputs(t)
	opts.MaxAttempts = 1
	opts.RequestTimeout = defaultRequestTimeout
	manifest, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(ctx context.Context, _ lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return lyricssource.FixedRevision{}, errors.New("fixed-revision operation context has no deadline")
		}
		budget := time.Until(deadline)
		if budget > opts.RequestTimeout || budget < opts.RequestTimeout-time.Second {
			return lyricssource.FixedRevision{}, fmt.Errorf("fixed-revision operation budget=%s, want approximately %s", budget, opts.RequestTimeout)
		}
		return fixed, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 1 {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestExpiredFixedRevisionAttemptCannotReturnSuccess(t *testing.T) {
	opts, _, fixed := writeLocalInputs(t)
	opts.MaxAttempts = 1
	opts.RequestTimeout = 10 * time.Millisecond
	manifest, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(ctx context.Context, _ lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		<-ctx.Done()
		return fixed, nil
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired successful operation error=%v, want context deadline exceeded", err)
	}
	if !reflect.DeepEqual(manifest, lyricsstaging.Manifest{}) {
		t.Fatalf("expired successful operation returned manifest: %+v", manifest)
	}
}

func TestValidateOptionsRejectsSensitiveManifestStdout(t *testing.T) {
	opts, _, _ := writeLocalInputs(t)
	opts.OutputPath = "-"
	if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "stdout is not supported") {
		t.Fatalf("stdout validation error=%v", err)
	}
	if err := writeManifest("-", lyricsstaging.Manifest{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "stdout is not supported") {
		t.Fatalf("stdout writer error=%v", err)
	}
}

func TestPreflightReportLimitIncludesReceiptAndClosedEnvelopeAllowance(t *testing.T) {
	if lyricsstaging.MaxPrivateEvidenceReceiptBytes != 64<<20 ||
		lyricsstaging.MaxPrivateEvidenceReceiptRawBytes != 32<<20 ||
		lyricsstaging.MaxPreflightReportEnvelopeBytes != 32<<20 ||
		lyricsstaging.MaxPreflightReportBytes != 96<<20 {
		t.Fatalf(
			"reviewed transport limits changed: rawReceipt=%d encodedReceipt=%d envelope=%d report=%d",
			lyricsstaging.MaxPrivateEvidenceReceiptRawBytes,
			lyricsstaging.MaxPrivateEvidenceReceiptBytes,
			lyricsstaging.MaxPreflightReportEnvelopeBytes,
			lyricsstaging.MaxPreflightReportBytes,
		)
	}
	if lyricsstaging.MaxPreflightReportBytes <= lyricsstaging.MaxPrivateEvidenceReceiptBytes {
		t.Fatal("preflight report limit does not leave room around a maximum-size evidence receipt")
	}

	path := filepath.Join(t.TempDir(), "oversize-preflight.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(lyricsstaging.MaxPreflightReportBytes) + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBoundedFile(path, lyricsstaging.MaxPreflightReportBytes, "preflight report"); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("%d bytes", lyricsstaging.MaxPreflightReportBytes)) {
		t.Fatalf("oversize preflight report error=%v", err)
	}
}

func TestHydrateFetchJobsClonesSharedReceiptEvidenceOnceFor704Items(t *testing.T) {
	_, report, _ := writeLocalInputs(t)
	const itemCount = 704
	item := report.UniqueComplete[0]
	items := make([]lyricsstaging.PreflightItem, itemCount)
	for index := range items {
		items[index] = item
		items[index].MusicID = index + 1
	}
	resolver, err := lyricsstaging.NewPrivateEvidenceResolver(*report.EvidenceReceipt)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := hydrateFetchJobs(items, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != itemCount {
		t.Fatalf("hydrated jobs=%d, want %d", len(jobs), itemCount)
	}
	firstRaw := jobs[0].hydratedCandidates[0].IndexEvidence[0].Raw
	receiptRaw := report.EvidenceReceipt.IndexEvidence[0].Raw
	if len(firstRaw) == 0 || len(receiptRaw) == 0 {
		t.Fatal("shared evidence fixture has empty raw bytes")
	}
	if &firstRaw[0] == &receiptRaw[0] {
		t.Fatal("hydrated batch aliases the canonical receipt raw bytes")
	}
	for index, job := range jobs {
		if len(job.hydratedCandidates) != 1 || len(job.hydratedCandidates[0].IndexEvidence) != 1 {
			t.Fatalf("job %d hydrated candidates=%+v", index, job.hydratedCandidates)
		}
		raw := job.hydratedCandidates[0].IndexEvidence[0].Raw
		if len(raw) == 0 || &raw[0] != &firstRaw[0] {
			t.Fatalf("job %d received a separate raw clone", index)
		}
	}
}

func TestRunPublishesCanonicalEvidenceReceiptSeparateFromManifest(t *testing.T) {
	opts, report, fixed := writeLocalInputs(t)
	opts.EvidenceReceiptOutputPath = filepath.Join(filepath.Dir(opts.OutputPath), "evidence-receipt.json")
	arguments := []string{
		"-report", opts.ReportPath,
		"-db", opts.DatabasePath,
		"-output", opts.OutputPath,
		"-evidence-receipt-output", opts.EvidenceReceiptOutputPath,
		"-concurrency", "2",
		"-max-attempts", "2",
		"-request-timeout", "1s",
		"-retry-delay", "1us",
	}
	dependencies := commandDependencies{
		NewSourceClient: func() (sourceClient, error) {
			return fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				return fixed, nil
			}}, nil
		},
		LoadCatalog: loadCatalogSnapshot,
		BuildDraft:  lyricsstaging.BuildDraftFromFixedArtifacts,
	}
	var stdout bytes.Buffer
	if err := runContextWithDependencies(context.Background(), arguments, &stdout, dependencies); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stage wrote sensitive output to stdout: %q", stdout.String())
	}

	receiptBody, err := os.ReadFile(opts.EvidenceReceiptOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptBody, err := lyricsstaging.MarshalPrivateEvidenceReceipt(*report.EvidenceReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(receiptBody, wantReceiptBody) {
		t.Fatal("published evidence receipt is not the exact canonical validated report receipt")
	}
	decodedReceipt, err := lyricsstaging.DecodePrivateEvidenceReceipt(receiptBody)
	if err != nil {
		t.Fatal(err)
	}
	if decodedReceipt.SchemaVersion != lyricsstaging.PrivateEvidenceReceiptSchemaVersion ||
		decodedReceipt.ReceiptSHA256 != report.EvidenceReceipt.ReceiptSHA256 {
		t.Fatalf("published receipt=%+v report digest=%s", decodedReceipt, report.EvidenceReceipt.ReceiptSHA256)
	}
	for _, path := range []string{opts.OutputPath, opts.EvidenceReceiptOutputPath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("private artifact %s mode=%v", path, info.Mode())
		}
	}
	manifestBody, err := os.ReadFile(opts.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifestBody, []byte(`"evidenceReceipt"`)) ||
		bytes.Contains(manifestBody, []byte(`"indexEvidence"`)) ||
		bytes.Contains(manifestBody, fixed.Wikitext) {
		t.Fatalf("staging manifest leaked private raw evidence: %s", manifestBody)
	}
	manifest, err := lyricsstaging.DecodeManifest(manifestBody)
	if err != nil {
		t.Fatal(err)
	}
	reportBody, err := os.ReadFile(opts.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest := sha256.Sum256(reportBody)
	if manifest.Preflight.ReportSHA256 != hex.EncodeToString(reportDigest[:]) {
		t.Fatalf("manifest report digest=%s, want exact validated report digest %x", manifest.Preflight.ReportSHA256, reportDigest)
	}

	beforeReceipt := append([]byte(nil), receiptBody...)
	if err := runContextWithDependencies(context.Background(), arguments, io.Discard, dependencies); err == nil ||
		!strings.Contains(err.Error(), "path already exists") {
		t.Fatalf("no-overwrite rerun error=%v", err)
	}
	afterReceipt, err := os.ReadFile(opts.EvidenceReceiptOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeReceipt, afterReceipt) {
		t.Fatal("no-overwrite rerun changed the evidence receipt")
	}
	afterManifest, err := os.ReadFile(opts.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBody, afterManifest) {
		t.Fatal("no-overwrite rerun changed the staging manifest")
	}
}

func TestValidateOptionsRejectsEvidenceReceiptOutputAlias(t *testing.T) {
	opts, _, _ := writeLocalInputs(t)
	opts.EvidenceReceiptOutputPath = opts.OutputPath
	if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("receipt/manifest alias error=%v", err)
	}
}

func TestValidateOptionsRecognizesIncompleteManifestEvidencePublicationPair(t *testing.T) {
	for name, createManifest := range map[string]bool{
		"manifest only": true,
		"receipt only":  false,
	} {
		t.Run(name, func(t *testing.T) {
			opts, _, _ := writeLocalInputs(t)
			opts.EvidenceReceiptOutputPath = filepath.Join(filepath.Dir(opts.OutputPath), "evidence-receipt.json")
			existingPath := opts.EvidenceReceiptOutputPath
			missingPath := opts.OutputPath
			if createManifest {
				existingPath, missingPath = opts.OutputPath, opts.EvidenceReceiptOutputPath
			}
			if err := os.WriteFile(existingPath, []byte("forensic incomplete artifact"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := validateOptions(opts)
			if err == nil || !strings.Contains(err.Error(), "incomplete staging publication pair") ||
				!strings.Contains(err.Error(), "refusing to overwrite or delete") {
				t.Fatalf("incomplete pair error=%v", err)
			}
			body, readErr := os.ReadFile(existingPath)
			if readErr != nil || string(body) != "forensic incomplete artifact" {
				t.Fatalf("incomplete artifact changed: body=%q err=%v", body, readErr)
			}
			if _, statErr := os.Lstat(missingPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("validation created missing pair path: %v", statErr)
			}
		})
	}
}

func TestExecuteStagesOnlyFixedUniqueCompleteAndDoesNotModifyDatabase(t *testing.T) {
	opts, report, fixed := writeLocalInputs(t)
	before, err := os.ReadFile(opts.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	manifest, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, candidate lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		calls.Add(1)
		want := hydratedStageCandidate(t, report, *report.UniqueComplete[0].Candidate)
		if identity.MusicID != 10 || identity.JapaneseTitle != "合成試験曲" ||
			identity.PerformerSegmentationPolicy != lyricssource.PerformerSegmentationSekaiEligible ||
			!reflect.DeepEqual(candidate, want) {
			return lyricssource.FixedRevision{}, fmt.Errorf("unexpected provider-aware fixed claim: identity=%+v candidate=%+v want=%+v", identity, candidate, want)
		}
		return fixed, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(manifest.Items) != 1 || manifest.Items[0].Source.RevisionID != 34 ||
		manifest.CatalogReference[0].CatalogFingerprint != report.UniqueComplete[0].CatalogFingerprint {
		t.Fatalf("calls=%d manifest=%+v", calls.Load(), manifest)
	}
	if err := writeManifest(opts.OutputPath, manifest, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	outputInfo, err := os.Stat(opts.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if outputInfo.Mode().Perm() != 0o600 {
		t.Fatalf("staging manifest permissions=%#o, want 0600", outputInfo.Mode().Perm())
	}
	if err := writeManifest(opts.OutputPath, manifest, &bytes.Buffer{}); err == nil {
		t.Fatal("writer overwrote an existing staging manifest")
	}
	after, err := os.ReadFile(opts.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only staging modified the local database")
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(opts.DatabasePath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read-only staging left SQLite sidecar %s: %v", suffix, err)
		}
	}
}

func TestExecuteRoutesMoegirlCandidateThroughProviderAwareFixedFetch(t *testing.T) {
	opts, report, fixed := writeLocalInputs(t)
	candidate := report.UniqueComplete[0].Candidate
	candidate.Provider = model.LyricsSourceProviderMoegirl
	candidate.Origin = model.LyricsSourceOriginMoegirl
	candidate.Section = "合成試験曲/歌词"
	candidate.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{{EvidenceID: "search:moegirl:12"}}
	candidateURL := "https://moegirl.icu/index.php?oldid=34&title=" + url.QueryEscape(candidate.Title)
	candidate.CanonicalURL = candidateURL
	evidence := stageIndexEvidence(*candidate, fixed.Wikitext, time.Unix(120, 0).UTC())
	candidate.IndexEvidenceRefs[0] = model.LyricsSourceIndexEvidenceRef{
		EvidenceID: evidence.EvidenceID, SHA256: evidence.SHA256,
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{evidence})
	if err != nil {
		t.Fatal(err)
	}
	report.EvidenceReceipt = &receipt
	report.UniqueComplete[0].FixedArtifactCandidates = []lyricsstaging.CandidateIdentity{*candidate}

	fixed.Provider = candidate.Provider
	fixed.Origin = candidate.Origin
	fixed.CanonicalURL = candidate.CanonicalURL
	fixed.Section = candidate.Section
	fixed.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...)
	fixed.IndexEvidence = []lyricssource.IndexEvidence{evidence}
	rewriteStagePreflight(t, opts.ReportPath, report)

	manifest, err := execute(context.Background(), opts, fakeSourceClient{
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			if identity.PerformerSegmentationPolicy != lyricssource.PerformerSegmentationSekaiEligible ||
				claimed.Provider != model.LyricsSourceProviderMoegirl || claimed.Origin != model.LyricsSourceOriginMoegirl ||
				claimed.CanonicalURL != candidateURL || !reflect.DeepEqual(claimed, hydratedStageCandidate(t, report, *candidate)) {
				return lyricssource.FixedRevision{}, fmt.Errorf("provider route identity=%+v candidate=%+v", identity, claimed)
			}
			return fixed, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 1 || len(manifest.Items[0].Artifacts) != 1 ||
		manifest.Items[0].Artifacts[0].Identity.Provider != model.LyricsSourceProviderMoegirl ||
		manifest.Items[0].Artifacts[0].Identity.Origin != model.LyricsSourceOriginMoegirl {
		t.Fatalf("provider-aware manifest=%+v", manifest)
	}
}

func TestExecutePreservesTaggedProjectionWithoutInventingDuplicateRawArtifactsOrRomanization(t *testing.T) {
	opts, report, fixed := writeLocalInputs(t)
	candidate := report.UniqueComplete[0].Candidate
	candidate.VersionReason = model.LyricsSourceVersionReasonTaggedFullAndGame
	report.UniqueComplete[0].FixedArtifactCandidates = []lyricsstaging.CandidateIdentity{*candidate}
	report.UniqueComplete[0].CompositionReason = model.LyricsSourceVersionReasonTaggedFullAndGame
	fixed.VersionReason = candidate.VersionReason
	rewriteStagePreflight(t, opts.ReportPath, report)

	fullIdentity := model.LyricsSourceFixedIdentity{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, SHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, FetchedAt: fixed.FetchedAt.UTC().Format(time.RFC3339Nano),
		Categories: append([]string{}, candidate.Categories...), Section: candidate.Section,
		RenditionKey:      candidate.RenditionKey,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
	}
	gameIdentity := fullIdentity
	gameIdentity.RenditionKey = "game-sekai"
	fullRef := model.LyricsSourceComponentRef{RenditionKey: fullIdentity.RenditionKey}
	gameRef := model.LyricsSourceComponentRef{RenditionKey: gameIdentity.RenditionKey}
	full := model.NewLyricsSourceFullFromLegacy(
		model.LyricsSourceVersion{Kind: fixed.Extraction.Version.Kind, Label: fixed.Extraction.Version.Label},
		[]model.LyricsSourcePerformer{{PerformerID: "歌唱者-21", Name: "初音ミク", Color: "#33CCBB"}},
		fixed.Extraction.RubyGeneratorVersion,
		[]model.LyricsSourceExtractedLine{{
			Japanese: "初音歌う",
			Segments: []model.LyricsSourceSegment{{
				Text: "初音歌う", PerformerIDs: []string{"歌唱者-21"}, Ruby: []model.LyricsSourceRubySpan{
					{Text: "初音", Reading: "はつね"}, {Text: "歌", Reading: "うた"}, {Text: "う"},
				},
			}},
			TrailingPerformerIDs: []string{"歌唱者-21"},
		}},
	)
	performerRef := fullRef
	rubyRef := fullRef
	gameProjectionRef := gameRef
	document := model.LyricsSourceDocument{
		SchemaVersion: model.LyricsSourceDocumentSchemaVersion,
		ReasonCode:    model.LyricsSourceVersionReasonTaggedFullAndGame,
		FixedIdentities: []model.LyricsSourceFixedIdentity{
			fullIdentity, gameIdentity,
		},
		Provenance: model.LyricsSourceComponentProvenance{
			FullText: fullRef, PerformerSegmentation: &performerRef, GameProjection: &gameProjectionRef,
			Ruby: &rubyRef, VersionEvidence: fullRef,
		},
		Full: full,
		GameProjection: &model.LyricsSourceGameProjection{
			LineIDs: []string{full.Lines[0].ID},
		},
	}
	fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)
	fixed.Document = &document

	manifest, err := execute(context.Background(), opts, fakeSourceClient{
		fetchCandidate: func(_ context.Context, _ lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			if !reflect.DeepEqual(claimed, hydratedStageCandidate(t, report, *candidate)) {
				return lyricssource.FixedRevision{}, fmt.Errorf("fixed artifact candidate=%+v", claimed)
			}
			return fixed, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 1 || len(manifest.Items[0].Artifacts) != 1 ||
		manifest.Items[0].Artifacts[0].Identity.RenditionKey != "full-sekai" ||
		manifest.Items[0].Document.Provenance.GameProjection == nil ||
		manifest.Items[0].Document.Provenance.GameProjection.RenditionKey != "full-sekai" {
		t.Fatalf("tagged projection did not remain bound to its one exact raw artifact: %+v", manifest.Items)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(body), []byte("roman")) {
		t.Fatalf("staging manifest contains a romanization field: %s", body)
	}
}

func TestExecuteFetchesEveryExactArtifactAndKeepsLogicalRenditionSeparate(t *testing.T) {
	opts, report, fandomFixed := writeLocalInputs(t)
	fandom := *report.UniqueComplete[0].Candidate

	moegirlRaw := []byte("== Lyrics ==\n初音歌う\n<!-- moegirl exact revision -->")
	moegirlSHA1 := sha1.Sum(moegirlRaw)
	canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
	query := canonical.Query()
	query.Set("oldid", "100")
	query.Set("title", "合成試験曲/歌词")
	canonical.RawQuery = query.Encode()
	moegirl := lyricsstaging.CandidateIdentity{
		Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
		PageID: 99, RevisionID: 100, SHA1: hex.EncodeToString(moegirlSHA1[:]), Title: "合成試験曲/歌词",
		CanonicalURL: canonical.String(), Categories: []string{"Lyrics", "Project SEKAI songs"},
		Section: "歌词", RenditionKey: "full-sekai", VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{EvidenceID: "revision:moegirl:99:100"}},
	}
	moegirlEvidence := stageIndexEvidence(moegirl, moegirlRaw, time.Unix(121, 0).UTC())
	moegirl.IndexEvidenceRefs[0] = model.LyricsSourceIndexEvidenceRef{
		EvidenceID: moegirlEvidence.EvidenceID, SHA256: moegirlEvidence.SHA256,
	}
	allEvidence := append([]lyricssource.IndexEvidence{}, report.EvidenceReceipt.IndexEvidence...)
	allEvidence = append(allEvidence, moegirlEvidence)
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(allEvidence)
	if err != nil {
		t.Fatal(err)
	}
	report.EvidenceReceipt = &receipt
	report.UniqueComplete[0].Candidate = &moegirl
	report.UniqueComplete[0].FixedArtifactCandidates = []lyricsstaging.CandidateIdentity{fandom, moegirl}
	report.UniqueComplete[0].CompositionReason = model.LyricsSourceVersionReasonUntaggedFullOnly
	artifactKeys, err := lyricsstaging.ResolveArtifactRenditionKeys(report.UniqueComplete[0].FixedArtifactCandidates)
	if err != nil {
		t.Fatal(err)
	}
	rewriteStagePreflight(t, opts.ReportPath, report)

	moegirlFixed := lyricssource.FixedRevision{
		Provider: moegirl.Provider, Origin: moegirl.Origin, PageID: moegirl.PageID, RevisionID: moegirl.RevisionID,
		SHA1: moegirl.SHA1, PageTitle: moegirl.Title, CanonicalURL: moegirl.CanonicalURL,
		Categories: append([]string{}, moegirl.Categories...), Section: moegirl.Section,
		RenditionKey: moegirl.RenditionKey, VersionReason: moegirl.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, moegirl.IndexEvidenceRefs...),
		IndexEvidence:     []lyricssource.IndexEvidence{moegirlEvidence},
		FetchedAt:         time.Unix(457, 0).UTC(), Wikitext: moegirlRaw,
		Lines: []lyricssource.ExtractedLine{{Japanese: "初音歌う"}},
		Extraction: lyricssource.Extraction{
			Version: lyricssource.LyricsVersion{Kind: "sekai", Label: "Project SEKAI Version"},
			Lines: []lyricssource.StructuredLine{{
				Japanese: "初音歌う",
				Segments: []lyricssource.LyricsSegment{{
					Text: "初音歌う", PerformerIDs: []string{}, Ruby: []lyricssource.RubySpan{{Text: "初音歌う"}},
				}},
				TrailingPerformerIDs: []string{},
			}},
		},
	}

	var fandomFetches, moegirlFetches atomic.Int32
	manifest, err := execute(context.Background(), opts, fakeSourceClient{
		fetchCandidate: func(_ context.Context, _ lyricssource.MusicIdentity, candidate lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			switch candidate.Provider {
			case model.LyricsSourceProviderVocaloidFandom:
				fandomFetches.Add(1)
				if !reflect.DeepEqual(candidate, hydratedStageCandidate(t, report, fandom)) {
					return lyricssource.FixedRevision{}, fmt.Errorf("fandom candidate=%+v", candidate)
				}
				return fandomFixed, nil
			case model.LyricsSourceProviderMoegirl:
				moegirlFetches.Add(1)
				if !reflect.DeepEqual(candidate, hydratedStageCandidate(t, report, moegirl)) {
					return lyricssource.FixedRevision{}, fmt.Errorf("moegirl candidate=%+v", candidate)
				}
				return moegirlFixed, nil
			default:
				return lyricssource.FixedRevision{}, fmt.Errorf("unexpected provider %q", candidate.Provider)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fandomFetches.Load() != 1 || moegirlFetches.Load() != 1 || len(manifest.Items) != 1 || len(manifest.Items[0].Artifacts) != 2 {
		t.Fatalf("fandom=%d moegirl=%d manifest=%+v", fandomFetches.Load(), moegirlFetches.Load(), manifest)
	}
	document := manifest.Items[0].Document
	identities := make(map[model.LyricsSourceProvider]model.LyricsSourceFixedIdentity, len(document.FixedIdentities))
	for _, identity := range document.FixedIdentities {
		identities[identity.Provider] = identity
	}
	fandomIdentity := identities[model.LyricsSourceProviderVocaloidFandom]
	moegirlIdentity := identities[model.LyricsSourceProviderMoegirl]
	if len(identities) != 2 || fandomIdentity.RenditionKey != artifactKeys[0] || moegirlIdentity.RenditionKey != artifactKeys[1] ||
		fandomIdentity.RenditionKey == moegirlIdentity.RenditionKey ||
		fandomIdentity.CompositionRenditionKey != "full-sekai" || moegirlIdentity.CompositionRenditionKey != "full-sekai" ||
		document.ReasonCode != model.LyricsSourceVersionReasonUntaggedFullOnly ||
		document.Provenance.FullText.RenditionKey != artifactKeys[1] ||
		document.Provenance.VersionEvidence.RenditionKey != artifactKeys[1] ||
		document.Provenance.PerformerSegmentation == nil || document.Provenance.PerformerSegmentation.RenditionKey != artifactKeys[0] ||
		document.Provenance.Ruby == nil || document.Provenance.Ruby.RenditionKey != artifactKeys[0] {
		t.Fatalf("logical/artifact composition seam=%+v", document)
	}
}

func TestExecuteKeepsProviderFetchAndDraftBuilderInjectable(t *testing.T) {
	opts, _, fixed := writeLocalInputs(t)
	var fetchCalls, buildCalls atomic.Int32
	manifest, err := executeWithDependencies(
		context.Background(),
		opts,
		fakeSourceClient{fetchCandidate: func(_ context.Context, _ lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			return fixed, nil
		}},
		loadCatalogSnapshot,
		func(item lyricsstaging.PreflightItem, identity lyricscontract.CatalogIdentity, bundle lyricsstaging.FixedArtifactBundle) (lyricsstaging.Draft, error) {
			buildCalls.Add(1)
			if bundle.EvidenceResolver == nil || bundle.EvidenceReceipt.SchemaVersion != 0 ||
				bundle.EvidenceReceipt.IndexEvidence != nil || bundle.EvidenceReceipt.ReceiptSHA256 != "" {
				return lyricsstaging.Draft{}, errors.New("stage rebuilt an item-local evidence receipt instead of reusing the validated resolver")
			}
			return lyricsstaging.BuildDraftFromFixedArtifacts(item, identity, bundle)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fetchCalls.Load() != 1 || buildCalls.Load() != 1 || len(manifest.Items) != 1 {
		t.Fatalf("fetches=%d builds=%d manifest=%+v", fetchCalls.Load(), buildCalls.Load(), manifest)
	}
}

func TestPairPublicationRollsBackOwnedManifestWhenReceiptLinkLosesNoOverwriteRace(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "staging.json")
	receiptPath := filepath.Join(directory, "evidence-receipt.json")
	publishCount := 0
	syncCount := 0
	err := writeFilePairNoReplaceWithPublisherContext(
		context.Background(),
		manifestPath,
		[]byte("private manifest"),
		receiptPath,
		[]byte("private receipt"),
		func(privatePath, finalPath string) error {
			publishCount++
			if publishCount == 2 {
				if err := os.WriteFile(finalPath, []byte("competing receipt"), 0o600); err != nil {
					return err
				}
			}
			return os.Link(privatePath, finalPath)
		},
		func([]string) error {
			syncCount++
			return nil
		},
		nil,
	)
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("receipt no-overwrite race error=%v", err)
	}
	if publishCount != 2 || syncCount == 0 {
		t.Fatalf("publishCount=%d syncCount=%d", publishCount, syncCount)
	}
	if _, statErr := os.Lstat(manifestPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("owned manifest was not rolled back: %v", statErr)
	}
	receiptBody, readErr := os.ReadFile(receiptPath)
	if readErr != nil || string(receiptBody) != "competing receipt" {
		t.Fatalf("competing receipt changed: body=%q err=%v", receiptBody, readErr)
	}
	for _, pattern := range []string{".staging.json.tmp-*", ".evidence-receipt.json.tmp-*"} {
		matches, globErr := filepath.Glob(filepath.Join(directory, pattern))
		if globErr != nil || len(matches) != 0 {
			t.Fatalf("temporary pattern %s matches=%v err=%v", pattern, matches, globErr)
		}
	}
}

func TestPairPublicationRollsBackBothOwnedInodesWhenParentSyncFails(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "staging.json")
	receiptPath := filepath.Join(directory, "evidence-receipt.json")
	syncCount := 0
	err := writeFilePairNoReplaceWithPublisherContext(
		context.Background(),
		manifestPath,
		[]byte("private manifest"),
		receiptPath,
		[]byte("private receipt"),
		os.Link,
		func([]string) error {
			syncCount++
			if syncCount == 1 {
				return errors.New("injected parent fsync failure")
			}
			return nil
		},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "injected parent fsync failure") {
		t.Fatalf("parent fsync error=%v", err)
	}
	if syncCount != 2 {
		t.Fatalf("parent sync calls=%d, want publication plus rollback", syncCount)
	}
	for _, path := range []string{manifestPath, receiptPath} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("sync failure left published path %s: %v", path, statErr)
		}
	}
}

func TestPairPublicationCancellationAfterFirstLinkRollsBackAndSyncs(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "staging.json")
	receiptPath := filepath.Join(directory, "evidence-receipt.json")
	ctx, cancel := context.WithCancel(context.Background())
	syncCount := 0
	err := writeFilePairNoReplaceWithPublisherContext(
		ctx,
		manifestPath,
		[]byte("private manifest"),
		receiptPath,
		[]byte("private receipt"),
		os.Link,
		func([]string) error {
			syncCount++
			return nil
		},
		func(index int) {
			if index == 0 {
				cancel()
			}
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pair cancellation error=%v, want context.Canceled", err)
	}
	if syncCount != 1 {
		t.Fatalf("rollback parent sync calls=%d, want 1", syncCount)
	}
	for _, path := range []string{manifestPath, receiptPath} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled pair left path %s: %v", path, statErr)
		}
	}
}

func TestPairPublicationFailsClosedIfPublishedManifestIsReplacedBeforeCommit(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "staging.json")
	receiptPath := filepath.Join(directory, "evidence-receipt.json")
	syncCount := 0
	err := writeFilePairNoReplaceWithPublisherContext(
		context.Background(),
		manifestPath,
		[]byte("private manifest"),
		receiptPath,
		[]byte("private receipt"),
		os.Link,
		func([]string) error {
			syncCount++
			return nil
		},
		func(index int) {
			if index != 1 {
				return
			}
			if removeErr := os.Remove(manifestPath); removeErr != nil {
				t.Fatal(removeErr)
			}
			if writeErr := os.WriteFile(manifestPath, []byte("competing manifest"), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "no longer matches its owned mode-0600 private inode") {
		t.Fatalf("replaced pair error=%v", err)
	}
	if syncCount != 1 {
		t.Fatalf("rollback parent sync calls=%d, want 1", syncCount)
	}
	manifestBody, readErr := os.ReadFile(manifestPath)
	if readErr != nil || string(manifestBody) != "competing manifest" {
		t.Fatalf("competing manifest changed: body=%q err=%v", manifestBody, readErr)
	}
	if _, statErr := os.Lstat(receiptPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("owned receipt was not rolled back: %v", statErr)
	}
	if pairErr := requireAbsentPublicationPair(manifestPath, receiptPath); pairErr == nil ||
		!strings.Contains(pairErr.Error(), "incomplete staging publication pair") {
		t.Fatalf("replaced pair was not recognized as incomplete: %v", pairErr)
	}
}

func TestOutputPublicationFailurePreservesCompetingFinalAndCleansPrivateTemporary(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "staging.json")
	var temporaryPath string
	err := writeFileNoReplaceWithPublisher(path, []byte("private staged bytes"), func(privatePath, finalPath string) error {
		temporaryPath = privatePath
		if err := os.WriteFile(finalPath, []byte("competing final"), 0o600); err != nil {
			return err
		}
		return os.Link(privatePath, finalPath)
	})
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("publication error=%v, want path-exists failure", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "competing final" {
		t.Fatalf("competing final changed: %q", body)
	}
	if temporaryPath == "" {
		t.Fatal("publisher did not receive private temporary path")
	}
	if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private temporary artifact was not cleaned: %v", err)
	}
}

func TestOutputCleanupNeverUnlinksFinalReplacedAfterPublication(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "staging.json")
	var temporaryPath string
	err := writeFileNoReplaceWithPublisher(path, []byte("published bytes"), func(privatePath, finalPath string) error {
		temporaryPath = privatePath
		if err := os.Link(privatePath, finalPath); err != nil {
			return err
		}
		replacementPath := filepath.Join(directory, "replacement.json")
		if err := os.WriteFile(replacementPath, []byte("replacement after publication"), 0o600); err != nil {
			return err
		}
		if err := os.Remove(finalPath); err != nil {
			return err
		}
		return os.Rename(replacementPath, finalPath)
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "replacement after publication" {
		t.Fatalf("replacement final changed by cleanup: %q", body)
	}
	if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private temporary artifact was not cleaned: %v", err)
	}
}

func TestCancellationAfterTemporarySyncPreventsManifestPublicationAndCleansTemporary(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "staging.json")
	ctx, cancel := context.WithCancel(context.Background())
	publisherCalled := false
	err := writeFileNoReplaceWithPublisherContext(ctx, path, []byte("private staged bytes"), func(privatePath, finalPath string) error {
		publisherCalled = true
		return os.Link(privatePath, finalPath)
	}, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("publication cancellation error=%v, want context.Canceled", err)
	}
	if publisherCalled {
		t.Fatal("no-overwrite publisher ran after parent cancellation")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled publication created final output: %v", err)
	}
	temporaryMatches, err := filepath.Glob(filepath.Join(directory, ".staging.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryMatches) != 0 {
		t.Fatalf("canceled publication left temporary outputs: %v", temporaryMatches)
	}
}

func TestCancellationImmediatelyBeforeNoOverwriteLinkRemovesPublishedManifest(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "staging.json")
	ctx, cancel := context.WithCancel(context.Background())
	err := writeFileNoReplaceWithPublisherContext(ctx, path, []byte("private staged bytes"), func(privatePath, finalPath string) error {
		cancel()
		return os.Link(privatePath, finalPath)
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("link-boundary cancellation error=%v, want context.Canceled", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link-boundary cancellation left final output: %v", err)
	}
	temporaryMatches, err := filepath.Glob(filepath.Join(directory, ".staging.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryMatches) != 0 {
		t.Fatalf("link-boundary cancellation left temporary outputs: %v", temporaryMatches)
	}
}

func TestReadBoundedFileRejectsReportPathReplacementBetweenInspectionAndOpen(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "preflight.json")
	replacementPath := filepath.Join(directory, "replacement.json")
	originalMovedPath := filepath.Join(directory, "original.json")
	if err := os.WriteFile(path, []byte("original report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, []byte("replaced report"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readBoundedFileWithOpener(path, lyricsstaging.MaxPreflightReportBytes, "preflight report", func(openPath string) (*os.File, error) {
		if err := os.Rename(openPath, originalMovedPath); err != nil {
			return nil, err
		}
		if err := os.Rename(replacementPath, openPath); err != nil {
			return nil, err
		}
		return os.Open(openPath)
	})
	if err == nil || !strings.Contains(err.Error(), "changed between inspection and open") {
		t.Fatalf("report replacement error=%v", err)
	}
}

func TestReadBoundedFileRejectsEqualLengthSameInodeMutationAfterDigestPin(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "preflight.json")
	original := []byte("canonical-report-A")
	replacement := []byte("canonical-report-B")
	if len(original) != len(replacement) {
		t.Fatal("test mutation must preserve file size")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = readBoundedFileWithSnapshotHook(path, lyricsstaging.MaxPreflightReportBytes, "preflight report", os.Open, func() error {
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		if _, err := file.WriteAt(replacement, 0); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		return file.Close()
	})
	if err == nil || !strings.Contains(err.Error(), "bytes changed after digest pin") {
		t.Fatalf("same-inode report mutation error=%v", err)
	}
	after, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() {
		t.Fatal("test mutation unexpectedly changed the report inode or size")
	}
}

func TestExecuteRejectsCatalogAndFixedIdentityDrift(t *testing.T) {
	t.Run("catalog drift before source fetch", func(t *testing.T) {
		opts, report, fixed := writeLocalInputs(t)
		body, err := os.ReadFile(opts.ReportPath)
		if err != nil {
			t.Fatal(err)
		}
		var decoded lyricsstaging.PreflightReport
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		decoded.UniqueComplete[0].CatalogFingerprint = strings.Repeat("c", 64)
		changed, _ := json.MarshalIndent(decoded, "", "  ")
		changed = append(changed, '\n')
		if err := os.WriteFile(opts.ReportPath, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		called := false
		_, err = execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			called = true
			return fixed, nil
		}})
		if err == nil || called || !strings.Contains(err.Error(), "fingerprint does not match the local catalog snapshot") {
			t.Fatalf("err=%v called=%t report=%+v", err, called, report)
		}
	})

	t.Run("fixed revision drift", func(t *testing.T) {
		opts, _, fixed := writeLocalInputs(t)
		fixed.SHA1 = strings.Repeat("d", 40)
		_, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return fixed, nil
		}})
		if err == nil || !strings.Contains(err.Error(), "identity drifted") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestExecuteRetriesTransientFetchWithoutSearching(t *testing.T) {
	opts, _, fixed := writeLocalInputs(t)
	var calls atomic.Int32
	manifest, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		if calls.Add(1) == 1 {
			return lyricssource.FixedRevision{}, &lyricssource.HTTPError{StatusCode: 503}
		}
		return fixed, nil
	}})
	if err != nil || calls.Load() != 2 || len(manifest.Items) != 1 {
		t.Fatalf("calls=%d manifest=%+v err=%v", calls.Load(), manifest, err)
	}
}

func TestExecuteFinal429FailsClosedWithoutManifest(t *testing.T) {
	opts, _, _ := writeLocalInputs(t)
	opts.MaxAttempts = 3
	opts.RetryDelay = 0
	var calls atomic.Int32
	manifest, err := execute(context.Background(), opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
		calls.Add(1)
		return lyricssource.FixedRevision{}, &lyricssource.HTTPError{StatusCode: 429}
	}})
	var httpError *lyricssource.HTTPError
	if !errors.As(err, &httpError) || httpError.StatusCode != 429 {
		t.Fatalf("error=%v, want final HTTP 429", err)
	}
	if calls.Load() != int32(opts.MaxAttempts) {
		t.Fatalf("fetch calls=%d, want %d", calls.Load(), opts.MaxAttempts)
	}
	if !reflect.DeepEqual(manifest, lyricsstaging.Manifest{}) {
		t.Fatalf("final 429 returned partial manifest: %+v", manifest)
	}
	if _, statErr := os.Stat(opts.OutputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final 429 created output path: %v", statErr)
	}
}

func TestRunContextHonorsCanceledParentWithoutManifest(t *testing.T) {
	opts, _, _ := writeLocalInputs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	err := runContext(ctx, []string{
		"-report", opts.ReportPath,
		"-db", opts.DatabasePath,
		"-output", opts.OutputPath,
	}, &stdout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runContext error=%v, want context.Canceled", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("canceled runContext wrote stdout: %q", stdout.String())
	}
	if _, statErr := os.Stat(opts.OutputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled runContext created manifest: %v", statErr)
	}
}
