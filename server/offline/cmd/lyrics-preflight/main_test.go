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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"

	_ "modernc.org/sqlite"
)

type fakeSourceClient struct {
	search         func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error)
	fetchCandidate func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error)
}

type diagnosticFakeSourceClient struct {
	fakeSourceClient
	diagnosticSearch func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error)
}

func (f diagnosticFakeSourceClient) SearchWithDiagnostics(ctx context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
	if f.diagnosticSearch == nil {
		candidates, err := f.Search(ctx, identity)
		return candidates, lyricssource.SearchDiagnostics{}, err
	}
	return f.diagnosticSearch(ctx, identity)
}

func (f fakeSourceClient) Search(ctx context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
	if f.search == nil {
		return nil, nil
	}
	return f.search(ctx, identity)
}

func (f fakeSourceClient) FetchFixedCandidateRevision(ctx context.Context, identity lyricssource.MusicIdentity, candidate lyricssource.Candidate) (lyricssource.FixedRevision, error) {
	if f.fetchCandidate == nil {
		return lyricssource.FixedRevision{}, errors.New("unexpected fixed-revision fetch")
	}
	return f.fetchCandidate(ctx, identity, candidate)
}

func equalSourceCandidate(left, right lyricssource.Candidate) bool {
	return reflect.DeepEqual(left, right)
}

func TestPreflightAndCheckpointCapacitiesMatchSharedReceiptContract(t *testing.T) {
	if maxCompleteReportBytes != lyricsstaging.MaxPreflightReportBytes {
		t.Fatalf("preflight report capacity=%d, want shared report capacity %d",
			maxCompleteReportBytes, lyricsstaging.MaxPreflightReportBytes)
	}
	if maxCompleteReportBytes != lyricsstaging.MaxPrivateEvidenceReceiptBytes+lyricsstaging.MaxPreflightReportEnvelopeBytes ||
		maxCompleteReportBytes <= lyricsstaging.MaxPrivateEvidenceReceiptBytes {
		t.Fatalf("complete report capacity=%d does not include the reviewed envelope around receipt=%d",
			maxCompleteReportBytes, lyricsstaging.MaxPrivateEvidenceReceiptBytes)
	}
	if !completeReportSizeWithinReviewedContract(int64(maxCompleteReportBytes)) {
		t.Fatal("shared complete-report upper boundary was rejected")
	}
	if completeReportSizeWithinReviewedContract(int64(maxCompleteReportBytes) + 1) {
		t.Fatal("shared complete-report upper boundary admitted one extra byte")
	}
	if maxCheckpointEvidenceJSONBytes != int64(lyricsstaging.MaxPrivateEvidenceReceiptBytes) {
		t.Fatalf("checkpoint evidence capacity=%d, want receipt capacity %d",
			maxCheckpointEvidenceJSONBytes, lyricsstaging.MaxPrivateEvidenceReceiptBytes)
	}
}

func TestStructuralStagingReportConversionMatchesJSONDTOContract(t *testing.T) {
	candidate := candidateFor(5, "structural-contract")
	summary, err := summarizeCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(candidate.IndexEvidence)
	if err != nil {
		t.Fatal(err)
	}
	generated := report{
		SchemaVersion: reportSchemaVersion, GeneratedAt: time.Unix(100, 0).UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: catalogSchemaVersion, CatalogCount: 1,
		Summary:         reportSummary{UniqueComplete: 1},
		EvidenceReceipt: &receipt,
		CatalogReview:   []reportItem{}, GameSizeEvidence: []reportItem{}, Ambiguous: []reportItem{},
		Missing: []reportItem{}, Incomplete: []reportItem{}, Error: []reportItem{},
		UniqueComplete: []reportItem{{
			MusicID: 5, JapaneseTitle: "構造契約", CatalogFingerprint: strings.Repeat("a", 64),
			TargetMusicID: 5, AssociationMusicIDs: []int{6, 7}, Candidate: &summary,
			FixedArtifactCandidates: []candidateSummary{summary}, PostFetchState: lyricsstaging.PostFetchStateComplete,
			CompositionReason: summary.VersionReason, LineCount: 2, SearchAttempts: 1, FetchAttempts: 1,
			SearchDiagnostics: &searchDiagnostics{SearchHits: 1, Verified: 1},
		}},
	}
	body, err := json.Marshal(generated)
	if err != nil {
		t.Fatal(err)
	}
	var want lyricsstaging.PreflightReport
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	if got := stagingReportContract(generated); !reflect.DeepEqual(got, want) {
		t.Fatalf("structural staging conversion drifted:\ngot=%+v\nwant=%+v", got, want)
	}
}

type catalogFixture struct {
	musicID       int
	title         string
	lyricist      string
	composer      string
	arranger      string
	version       string
	versionKnown  bool
	producer      string
	vocals        []model.CatalogVocalSignal
	catalogPolicy string
}

func completeCatalogFixture(musicID int, title string) catalogFixture {
	return catalogFixture{
		musicID: musicID, title: title, lyricist: "作詞者", composer: "作曲者", arranger: "編曲者",
		version: "full", versionKnown: true, producer: "作詞者 / 作曲者 / 編曲者",
		catalogPolicy: model.LyricsCatalogIdentityPolicyVersion,
	}
}

func writeV18Catalog(t *testing.T, records []catalogFixture) string {
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
	tx, err := database.Begin()
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	for version := 1; version <= catalogSchemaVersion; version++ {
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, version); err != nil {
			tx.Rollback()
			database.Close()
			t.Fatal(err)
		}
	}
	for _, record := range records {
		lyricsVersion := record.version
		if !record.versionKnown {
			lyricsVersion = "unknown"
		}
		presence := model.CatalogEvidencePresence{
			Lyricist: record.lyricist != "", Composer: record.composer != "", Arranger: record.arranger != "",
			LyricsVersion: record.versionKnown,
		}
		evidence := model.CatalogLyricsEvidence{
			Title: record.title, Lyricist: record.lyricist, Composer: record.composer, Arranger: record.arranger,
			LyricsVersion: lyricsVersion, Presence: presence, Vocals: append([]model.CatalogVocalSignal{}, record.vocals...),
		}
		fingerprint, err := model.CatalogLyricsEvidenceFingerprint(evidence)
		if err != nil {
			t.Fatal(err)
		}
		presenceJSON, err := json.Marshal(presence)
		if err != nil {
			t.Fatal(err)
		}
		vocalsJSON, err := json.Marshal(evidence.Vocals)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO catalog_music
			(music_id,title_ja,producer_metadata,lyricist,composer,arranger,lyrics_version,
			 lyrics_evidence_presence_json,vocal_signals_json,lyrics_catalog_fingerprint,lyrics_catalog_policy_version)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, record.musicID, record.title, record.producer, record.lyricist,
			record.composer, record.arranger, lyricsVersion, string(presenceJSON), string(vocalsJSON), fingerprint, record.catalogPolicy); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func testOptions(databasePath string) options {
	return options{
		DatabasePath:   databasePath,
		Concurrency:    4,
		MaxAttempts:    2,
		RequestTimeout: time.Second,
		RetryDelay:     time.Microsecond,
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
		"-output", "new private JSON report path",
		"-request-timeout", "client-side limiter waits", "(default 8m0s)",
		"-max-attempts", "(default 3)", "-retry-delay", "(default 250ms)",
	} {
		if !strings.Contains(help.String(), text) {
			t.Fatalf("help missing %q: %s", text, help.String())
		}
	}
	if strings.Contains(help.String(), "or - for stdout") {
		t.Fatalf("help still advertises complete report stdout: %s", help.String())
	}

	opts := testOptions("unused")
	opts.RequestTimeout = maxRequestTimeout
	if err := validateExecutionOptions(opts); err != nil {
		t.Fatalf("maximum request timeout rejected: %v", err)
	}
	opts.RequestTimeout = maxRequestTimeout + time.Nanosecond
	if err := validateExecutionOptions(opts); err == nil || !strings.Contains(err.Error(), "at most 10m") {
		t.Fatalf("over-limit request timeout error=%v", err)
	}
}

func TestSourceOperationReceivesFullRequestTimeoutBudget(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "operation-budget")})
	opts := testOptions(path)
	opts.MaxAttempts = 1
	opts.RequestTimeout = defaultRequestTimeout
	generated, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(ctx context.Context, _ lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, errors.New("source operation context has no deadline")
			}
			budget := time.Until(deadline)
			if budget > opts.RequestTimeout || budget < opts.RequestTimeout-time.Second {
				return nil, fmt.Errorf("source operation budget=%s, want approximately %s", budget, opts.RequestTimeout)
			}
			return nil, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.Missing) != 1 {
		t.Fatalf("report=%+v", generated.Summary)
	}
}

func TestRetryOperationRejectsValueReturnedAfterAttemptDeadline(t *testing.T) {
	opts := testOptions("unused")
	opts.MaxAttempts = 1
	opts.RequestTimeout = 10 * time.Millisecond
	result := retryOperation(context.Background(), opts, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "late-success", nil
	})
	if !errors.Is(result.err, context.DeadlineExceeded) || result.value != "" || result.attempts != 1 {
		t.Fatalf("late attempt result=%+v", result)
	}
}

func writeTestReport(t *testing.T, path string, generated report) {
	t.Helper()
	body, err := json.MarshalIndent(generated, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadResumeReportPinsAndRevalidatesPathSizeAndContent(t *testing.T) {
	validPath := filepath.Join(t.TempDir(), "valid.json")
	if err := os.WriteFile(validPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadResumeReport(validPath); err != nil {
		t.Fatalf("stable resume report error=%v", err)
	}

	for _, test := range []struct {
		name   string
		stage  string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name:  "inode replacement after initial stat",
			stage: resumeReportLoadAfterInitialStat,
			mutate: func(t *testing.T, path string) {
				replacement := filepath.Join(filepath.Dir(path), "replacement.json")
				if err := os.WriteFile(replacement, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			},
			want: "path, inode, or size changed",
		},
		{
			name:  "size change after open",
			stage: resumeReportLoadAfterOpen,
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"x":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "path, inode, or size changed",
		},
		{
			name:  "same-size bytes after open",
			stage: resumeReportLoadAfterOpen,
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "snapshot bytes changed",
		},
		{
			name:  "inode replacement after read",
			stage: resumeReportLoadAfterRead,
			mutate: func(t *testing.T, path string) {
				replacement := filepath.Join(filepath.Dir(path), "replacement-after-read.json")
				if err := os.WriteFile(replacement, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			},
			want: "path, inode, or size changed",
		},
		{
			name:  "same-size bytes after read",
			stage: resumeReportLoadAfterRead,
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "snapshot bytes changed",
		},
		{
			name:  "same-size bytes after decode",
			stage: resumeReportLoadAfterDecode,
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "snapshot bytes changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resume.json")
			if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadResumeReportWithHook(path, func(stage, absolutePath string) error {
				if stage == test.stage {
					test.mutate(t, absolutePath)
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("TOCTOU error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadResumeReportRejectsDuplicateJSONKeysBeforeMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duplicate.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"schemaVersion":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadResumeReport(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate-key resume error=%v", err)
	}
}

func reportItemCount(generated report) int {
	return len(generated.CatalogReview) + len(generated.GameSizeEvidence) + len(generated.UniqueComplete) +
		len(generated.Ambiguous) + len(generated.Missing) + len(generated.Incomplete) + len(generated.Error)
}

func candidateFor(musicID int, suffix string) lyricssource.Candidate {
	delta := 0
	if suffix != "" && suffix != "one" && suffix != "safe" {
		for _, character := range suffix {
			delta += int(character)
		}
		delta %= 7
		delta++
	}
	pageID := musicID*100 + 1 + delta
	revisionID := musicID*100 + 20 + delta
	title := "候補" + suffix
	canonical := url.URL{Scheme: "https", Host: "vocaloid.fandom.com", Path: "/wiki/" + strings.ReplaceAll(title, " ", "_")}
	query := canonical.Query()
	query.Set("oldid", fmt.Sprintf("%d", revisionID))
	canonical.RawQuery = query.Encode()
	raw := []byte(fmt.Sprintf("fixed-candidate-%d-%s", musicID, suffix))
	rawSHA1 := sha1.Sum(raw)
	evidenceSHA := sha256.Sum256(raw)
	evidenceSHA256 := hex.EncodeToString(evidenceSHA[:])
	fetchedAt := time.Unix(100, 0).UTC().Format(time.RFC3339Nano)
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		lyricssource.ProviderVocaloidFandom,
		fmt.Sprintf("fetch:vocaloid-fandom:%d", pageID),
		fetchedAt,
		evidenceSHA256,
	)
	candidate := lyricssource.Candidate{
		Provider: lyricssource.ProviderVocaloidFandom, Origin: lyricssource.OriginVocaloidFandom,
		PageID: pageID, Title: title, CanonicalURL: canonical.String(),
		RevisionID: revisionID, SHA1: hex.EncodeToString(rawSHA1[:]),
		Categories: []string{"Lyrics", "Songs"}, Section: "Lyrics", RenditionKey: "full-vocaloid",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: evidenceID, SHA256: evidenceSHA256,
		}},
	}
	candidate.IndexEvidence = []lyricssource.IndexEvidence{{
		EvidenceID: candidate.IndexEvidenceRefs[0].EvidenceID, SHA256: candidate.IndexEvidenceRefs[0].SHA256,
		Kind:     lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, MediaWikiSHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		FetchedAt: fetchedAt, Raw: raw,
		RawSHA256: candidate.IndexEvidenceRefs[0].SHA256,
	}}
	return candidate
}

func attachCandidateRevisionEvidence(candidate lyricssource.Candidate, raw []byte) lyricssource.Candidate {
	rawSHA1 := sha1.Sum(raw)
	rawSHA256 := sha256.Sum256(raw)
	rawSHA256Hex := hex.EncodeToString(rawSHA256[:])
	fetchedAt := time.Unix(100, 0).UTC().Format(time.RFC3339Nano)
	baseID := fmt.Sprintf("fetch:vocaloid-fandom:%d", candidate.PageID)
	if candidate.Provider == lyricssource.ProviderMoegirl {
		baseID = fmt.Sprintf("search:moegirl:%d", candidate.PageID)
	}
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		candidate.Provider, baseID, fetchedAt, rawSHA256Hex,
	)
	candidate.SHA1 = hex.EncodeToString(rawSHA1[:])
	candidate.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{{
		EvidenceID: evidenceID, SHA256: rawSHA256Hex,
	}}
	candidate.IndexEvidence = []lyricssource.IndexEvidence{{
		EvidenceID: evidenceID, SHA256: rawSHA256Hex,
		Kind:     lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, MediaWikiSHA1: candidate.SHA1, Title: candidate.Title,
		CanonicalURL: candidate.CanonicalURL, Categories: append([]string{}, candidate.Categories...),
		FetchedAt: fetchedAt, Raw: append([]byte{}, raw...), RawSHA256: rawSHA256Hex,
	}}
	return candidate
}

func fixedFor(candidate lyricssource.Candidate, lyricText string) lyricssource.FixedRevision {
	wikitext := append([]byte{}, candidate.IndexEvidence[0].Raw...)
	version := lyricssource.LyricsVersion{Kind: "vocaloid", Label: "Vocaloid Version"}
	if candidate.RenditionKey == "full-sekai" || candidate.RenditionKey == "game-sekai" {
		version = lyricssource.LyricsVersion{Kind: "sekai", Label: "Project SEKAI Version"}
	}
	return lyricssource.FixedRevision{
		Provider: candidate.Provider, Origin: candidate.Origin,
		CanonicalURL: candidate.CanonicalURL, PageID: candidate.PageID, PageTitle: candidate.Title,
		RevisionID: candidate.RevisionID, SHA1: candidate.SHA1, Categories: append([]string(nil), candidate.Categories...),
		Section: candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
		IndexEvidence:     append([]lyricssource.IndexEvidence{}, candidate.IndexEvidence...),
		FetchedAt:         time.Unix(200, 0).UTC(), Wikitext: wikitext,
		Lines: []lyricssource.ExtractedLine{{Japanese: lyricText}},
		Extraction: lyricssource.Extraction{
			Version: version,
			Lines: []lyricssource.StructuredLine{{
				Japanese: lyricText,
				Segments: []lyricssource.LyricsSegment{{
					Text: lyricText, PerformerIDs: []string{}, Ruby: []lyricssource.RubySpan{{Text: lyricText}},
				}},
				TrailingPerformerIDs: []string{},
			}},
		},
	}
}

func TestPreflightPropagatesCatalogPerformerPolicyAndProviderAwareCandidate(t *testing.T) {
	fixture := completeCatalogFixture(17, "提供者候補")
	fixture.vocals = []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai"}}
	path := writeV18Catalog(t, []catalogFixture{fixture})
	canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
	query := canonical.Query()
	query.Set("oldid", "1720")
	query.Set("title", "提供者候補")
	canonical.RawQuery = query.Encode()
	candidate := lyricssource.Candidate{
		Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
		PageID: 1701, RevisionID: 1720, Title: "提供者候補",
		CanonicalURL: canonical.String(), Categories: []string{"Lyrics", "Project SEKAI songs"},
		Section: "歌词", RenditionKey: "full-sekai", VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
	}
	candidate = attachCandidateRevisionEvidence(candidate, []byte("moegirl-provider-aware"))
	generated, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			if identity.PerformerSegmentationPolicy != lyricssource.PerformerSegmentationSekaiEligible {
				return nil, fmt.Errorf("search performer policy=%q", identity.PerformerSegmentationPolicy)
			}
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			if identity.PerformerSegmentationPolicy != lyricssource.PerformerSegmentationSekaiEligible || !reflect.DeepEqual(claimed, candidate) {
				return lyricssource.FixedRevision{}, fmt.Errorf("fetch claim identity=%+v candidate=%+v", identity, claimed)
			}
			return fixedFor(candidate, "完成"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.UniqueComplete) != 1 || generated.UniqueComplete[0].Candidate == nil || generated.EvidenceReceipt == nil {
		t.Fatalf("provider-aware report=%+v", generated)
	}
	hydrated, err := generated.EvidenceReceipt.HydrateCandidate(stagingCandidate(*generated.UniqueComplete[0].Candidate))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hydrated, candidate) {
		t.Fatalf("hydrated candidate=%+v, want %+v", hydrated, candidate)
	}
}

func TestProviderAwareCandidatesWithCollidingPageIDsRemainDistinct(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(17, "提供者衝突")})
	fandom := candidateFor(17, "one")
	moegirlTitle := "提供者衝突/歌詞"
	canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
	query := canonical.Query()
	query.Set("oldid", fmt.Sprintf("%d", fandom.RevisionID))
	query.Set("title", moegirlTitle)
	canonical.RawQuery = query.Encode()
	moegirl := lyricssource.Candidate{
		Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
		PageID: fandom.PageID, RevisionID: fandom.RevisionID, Title: moegirlTitle,
		CanonicalURL: canonical.String(), Categories: []string{"Lyrics", "Project SEKAI songs"},
		Section: "提供者衝突/歌词", RenditionKey: "full-sekai",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
	}
	moegirl = attachCandidateRevisionEvidence(moegirl, []byte("moegirl-colliding-page"))
	var fetchCalls atomic.Int32
	generated, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{fandom, moegirl}, nil
		},
		fetchCandidate: func(_ context.Context, _ lyricssource.MusicIdentity, candidate lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			return fixedFor(candidate, "共通歌詞"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if fetchCalls.Load() != 2 || len(generated.UniqueComplete) != 1 || len(generated.Ambiguous) != 0 {
		t.Fatalf("fetches=%d report=%+v", fetchCalls.Load(), generated.Summary)
	}
	complete := generated.UniqueComplete[0]
	if complete.Candidate == nil || complete.Candidate.Provider != model.LyricsSourceProviderVocaloidFandom ||
		len(complete.FixedArtifactCandidates) != 2 ||
		complete.CompositionReason != model.LyricsSourceVersionReasonUntaggedUncutIdentity ||
		complete.PostFetchState != lyricsstaging.PostFetchStateComplete || generated.EvidenceReceipt == nil {
		t.Fatalf("provider-aware composition=%+v receipt=%+v", complete, generated.EvidenceReceipt)
	}
	output := filepath.Join(t.TempDir(), "provider-aware.json")
	if err := writeReport(output, path, generated, io.Discard); err != nil {
		t.Fatalf("write provider-aware report: %v", err)
	}
}

func TestPluralPostFetchVersionConflictRetainsExactArtifactsOutsideUniqueComplete(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(18, "版本衝突")})
	fandom := candidateFor(18, "one")
	moegirlTitle := "版本衝突/歌詞"
	canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
	query := canonical.Query()
	query.Set("oldid", "1821")
	query.Set("title", moegirlTitle)
	canonical.RawQuery = query.Encode()
	moegirl := lyricssource.Candidate{
		Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
		PageID: 1802, RevisionID: 1821, Title: moegirlTitle,
		CanonicalURL: canonical.String(), Categories: []string{"Lyrics", "Project SEKAI songs"},
		Section: "版本衝突/歌词", RenditionKey: "full-sekai",
		VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
	}
	moegirl = attachCandidateRevisionEvidence(moegirl, []byte("moegirl-version-conflict"))

	var fetchCalls atomic.Int32
	generated, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{fandom, moegirl}, nil
		},
		fetchCandidate: func(_ context.Context, _ lyricssource.MusicIdentity, candidate lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			switch candidate.Provider {
			case model.LyricsSourceProviderVocaloidFandom:
				if !reflect.DeepEqual(candidate, fandom) {
					return lyricssource.FixedRevision{}, fmt.Errorf("fandom candidate=%+v", candidate)
				}
				return fixedFor(candidate, "完全版"), nil
			case model.LyricsSourceProviderMoegirl:
				if !reflect.DeepEqual(candidate, moegirl) {
					return lyricssource.FixedRevision{}, fmt.Errorf("moegirl candidate=%+v", candidate)
				}
				return fixedFor(candidate, "別歌詞"), nil
			default:
				return lyricssource.FixedRevision{}, fmt.Errorf("unexpected provider %q", candidate.Provider)
			}
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if fetchCalls.Load() != 2 || len(generated.UniqueComplete) != 0 || len(generated.Incomplete) != 1 {
		t.Fatalf("fetches=%d summary=%+v", fetchCalls.Load(), generated.Summary)
	}
	conflict := generated.Incomplete[0]
	if conflict.Candidate != nil || len(conflict.FixedArtifactCandidates) != 2 ||
		conflict.PostFetchState != lyricsstaging.PostFetchStateVersionConflict ||
		conflict.CompositionReason != model.LyricsSourceVersionReasonVersionConflict ||
		conflict.ErrorCode != string(model.LyricsSourceVersionReasonVersionConflict) || generated.EvidenceReceipt == nil {
		t.Fatalf("post-fetch conflict=%+v receipt=%+v", conflict, generated.EvidenceReceipt)
	}
	for _, summary := range conflict.FixedArtifactCandidates {
		if summary.ArtifactRenditionKey == "" {
			t.Fatalf("conflict artifact has no unique key: %+v", summary)
		}
		if _, err := generated.EvidenceReceipt.HydrateCandidate(stagingCandidate(summary)); err != nil {
			t.Fatalf("hydrate conflict artifact: %v", err)
		}
	}
	output := filepath.Join(t.TempDir(), "version-conflict.json")
	if err := writeReport(output, path, generated, io.Discard); err != nil {
		t.Fatalf("write version conflict report: %v", err)
	}
}

func TestCatalogPerformerPolicyUsesRuntimeSignalsFor789And794Through795(t *testing.T) {
	record789 := completeCatalogFixture(789, "eligible-789")
	record789.vocals = []model.CatalogVocalSignal{{VocalID: 1, VocalType: "sekai"}}
	record794 := completeCatalogFixture(794, "disabled-794")
	record794.vocals = []model.CatalogVocalSignal{{VocalID: 2, VocalType: "original_song"}}
	record795 := completeCatalogFixture(795, "disabled-795")
	record795.vocals = []model.CatalogVocalSignal{{VocalID: 3, VocalType: "virtual_singer"}}
	path := writeV18Catalog(t, []catalogFixture{record789, record794, record795})

	want := map[int]lyricssource.PerformerSegmentationPolicy{
		789: lyricssource.PerformerSegmentationSekaiEligible,
		794: lyricssource.PerformerSegmentationDisabled,
		795: lyricssource.PerformerSegmentationDisabled,
	}
	observed := make(map[int]lyricssource.PerformerSegmentationPolicy, len(want))
	var observedMu sync.Mutex
	generated, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			observedMu.Lock()
			observed[identity.MusicID] = identity.PerformerSegmentationPolicy
			observedMu.Unlock()
			return nil, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.Missing) != len(want) {
		t.Fatalf("summary=%+v", generated.Summary)
	}
	observedMu.Lock()
	defer observedMu.Unlock()
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("performer policies=%v, want %v", observed, want)
	}
}

func TestResumeCatalogCountIsDynamicAndRejects701Against704BeforeSource(t *testing.T) {
	priorRecords := make([]catalogFixture, 701)
	for index := range priorRecords {
		priorRecords[index] = completeCatalogFixture(index+1, fmt.Sprintf("dynamic-resume-%03d", index+1))
	}
	priorDatabasePath := writeV18Catalog(t, priorRecords)
	priorCatalog, err := loadCatalog(context.Background(), priorDatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	grouping := make([]model.CatalogLyricsGroupingRecord, 0, len(priorCatalog))
	itemsByID := make(map[int]catalogItem, len(priorCatalog))
	for _, item := range priorCatalog {
		grouping = append(grouping, model.CatalogLyricsGroupingRecord{
			MusicID: item.MusicID, Fingerprint: item.CatalogFingerprint, Evidence: item.Evidence,
		})
		itemsByID[item.MusicID] = item
	}
	prior := report{
		SchemaVersion: reportSchemaVersion, GeneratedAt: time.Unix(123, 0).UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: catalogSchemaVersion, CatalogCount: len(priorCatalog),
		CatalogReview: []reportItem{}, GameSizeEvidence: []reportItem{}, UniqueComplete: []reportItem{},
		Ambiguous: []reportItem{}, Missing: []reportItem{}, Incomplete: []reportItem{}, Error: []reportItem{},
	}
	for _, target := range model.ClassifyCatalogLyricsTargets(grouping) {
		item := baseReportItem(itemsByID[target.MusicID], target)
		item.SearchAttempts = 1
		item.ReasonCode = string(lyricssource.ZeroCandidateNoSearchHits)
		item.SearchDiagnostics = &searchDiagnostics{}
		prior.Missing = append(prior.Missing, item)
	}
	sortReport(&prior)
	prior.Summary.Missing = len(prior.Missing)
	priorReportPath := filepath.Join(t.TempDir(), "prior-701.json")
	writeTestReport(t, priorReportPath, prior)

	currentRecords := append([]catalogFixture{}, priorRecords...)
	for musicID := 702; musicID <= 704; musicID++ {
		currentRecords = append(currentRecords, completeCatalogFixture(musicID, fmt.Sprintf("dynamic-resume-%03d", musicID)))
	}
	currentDatabasePath := writeV18Catalog(t, currentRecords)
	opts := testOptions(currentDatabasePath)
	opts.ResumeReportPath = priorReportPath
	opts.ResumeErrorCodes = "none"
	var sourceCalls atomic.Int32
	_, err = execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			sourceCalls.Add(1)
			return lyricssource.FixedRevision{}, nil
		},
	}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "resume report catalog count 701 does not match current catalog count 704") {
		t.Fatalf("dynamic resume error=%v", err)
	}
	if sourceCalls.Load() != 0 {
		t.Fatalf("dynamic resume mismatch admitted %d source calls", sourceCalls.Load())
	}
}

func TestReadOnlyDatabaseAndReportNeverModifyCatalog(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "読取専用曲")})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)

	readonly, err := openReadOnlyCatalogDB(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var queryOnly, trustedSchema int
	if err := readonly.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil {
		readonly.Close()
		t.Fatal(err)
	}
	if queryOnly != 1 {
		readonly.Close()
		t.Fatalf("query_only=%d, want 1", queryOnly)
	}
	if err := readonly.QueryRow(`PRAGMA trusted_schema`).Scan(&trustedSchema); err != nil {
		readonly.Close()
		t.Fatal(err)
	}
	if trustedSchema != 0 {
		readonly.Close()
		t.Fatalf("trusted_schema=%d, want 0", trustedSchema)
	}
	if _, err := readonly.Exec(`UPDATE catalog_music SET title_ja='written' WHERE music_id=1`); err == nil {
		readonly.Close()
		t.Fatal("read-only connection accepted a write")
	}
	if err := readonly.Close(); err != nil {
		t.Fatal(err)
	}

	sourceStarted := false
	report, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceStarted = true
			return nil, nil
		},
	}, func() time.Time { return time.Unix(123, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if !sourceStarted {
		t.Fatal("source request did not run")
	}
	output := filepath.Join(t.TempDir(), "preflight.json")
	if err := writeReport(output, path, report, io.Discard); err != nil {
		t.Fatal(err)
	}
	outputInfo, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if outputInfo.Mode().Perm() != 0o600 {
		t.Fatalf("preflight report permissions=%#o, want 0600", outputInfo.Mode().Perm())
	}
	if err := writeReport(output, path, report, io.Discard); err == nil {
		t.Fatal("report writer overwrote an existing file")
	}
	if err := writeReport(path, path, report, io.Discard); err == nil {
		t.Fatal("report writer accepted the database path")
	}
	linkDirectory := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(filepath.Dir(path), linkDirectory); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(filepath.Join(linkDirectory, filepath.Base(path)), path, report, io.Discard); err == nil {
		t.Fatal("report writer accepted a symlink alias of the database path")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var title string
	check, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT title_ja FROM catalog_music WHERE music_id=1`).Scan(&title); err != nil {
		check.Close()
		t.Fatal(err)
	}
	if err := check.Close(); err != nil {
		t.Fatal(err)
	}
	if title != "読取専用曲" {
		t.Fatalf("catalog title changed to %q", title)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("catalog database bytes changed")
	}
	if afterHash := sha256.Sum256(after); afterHash != beforeHash {
		t.Fatal("catalog database hash changed")
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read-only preflight left SQLite sidecar %s: %v", suffix, err)
		}
	}
}

func TestCatalogSnapshotUsesPinnedImmutableDescriptor(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "immutable")})
	var dataSourceName string
	database, err := openReadOnlyCatalogDBWithOpeners(context.Background(), path, os.Open, func(driverName, sourceName string) (*sql.DB, error) {
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
	pragmas := query["_pragma"]
	if !containsString(pragmas, "query_only(1)") || !containsString(pragmas, "trusted_schema(0)") {
		t.Fatalf("immutable descriptor pragmas=%v", pragmas)
	}
}

func TestCatalogSnapshotRejectsEverySQLiteSidecarBeforeSource(t *testing.T) {
	for _, suffix := range sqliteSidecarSuffixes {
		t.Run(strings.TrimPrefix(suffix, "-"), func(t *testing.T) {
			path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "sidecar")})
			if err := os.WriteFile(path+suffix, []byte("must be rejected"), 0o600); err != nil {
				t.Fatal(err)
			}
			sourceCalled := false
			_, err := execute(context.Background(), testOptions(path), fakeSourceClient{
				search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
					sourceCalled = true
					return nil, nil
				},
			}, time.Now)
			if err == nil || !strings.Contains(err.Error(), "standalone immutable SQLite snapshot") ||
				!strings.Contains(err.Error(), suffix) || sourceCalled {
				t.Fatalf("sidecar=%s err=%v sourceCalled=%t", suffix, err, sourceCalled)
			}
		})
	}
}

func TestCatalogSnapshotRejectsActiveWALBeforeSource(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "active-wal")})
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
			t.Fatalf("active WAL did not create %s: %v", suffix, err)
		}
	}
	sourceCalled := false
	_, err = execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalled = true
			return nil, nil
		},
	}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "standalone immutable SQLite snapshot") || sourceCalled {
		t.Fatalf("active WAL err=%v sourceCalled=%t", err, sourceCalled)
	}
}

func TestCatalogSnapshotRejectsPathReplacementDuringOpen(t *testing.T) {
	t.Run("between inspection and descriptor open", func(t *testing.T) {
		path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "original")})
		replacement := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(2, "replacement")})
		moved := path + ".opened"
		_, err := openReadOnlyCatalogDBWithOpeners(context.Background(), path, func(openPath string) (*os.File, error) {
			if err := os.Rename(openPath, moved); err != nil {
				return nil, err
			}
			if err := os.Rename(replacement, openPath); err != nil {
				return nil, err
			}
			return os.Open(openPath)
		}, sql.Open)
		if err == nil || !strings.Contains(err.Error(), "changed while being opened") {
			t.Fatalf("replacement error=%v", err)
		}
	})

	t.Run("after descriptor open before SQLite open", func(t *testing.T) {
		path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "original")})
		replacement := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(2, "replacement")})
		moved := path + ".opened"
		_, err := openReadOnlyCatalogDBWithOpeners(context.Background(), path, os.Open, func(driverName, sourceName string) (*sql.DB, error) {
			if err := os.Rename(path, moved); err != nil {
				return nil, err
			}
			if err := os.Rename(replacement, path); err != nil {
				return nil, err
			}
			return sql.Open(driverName, sourceName)
		})
		if err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("replacement error=%v", err)
		}
	})
}

func TestCatalogSnapshotCloseRejectsPathBytesAndSidecarChanges(t *testing.T) {
	t.Run("path replacement", func(t *testing.T) {
		path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "original")})
		replacement := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(2, "replacement")})
		database, err := openReadOnlyCatalogDB(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, path+".opened"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		if err := database.Close(); err == nil || !strings.Contains(err.Error(), "path, inode, or size changed") {
			t.Fatalf("close replacement error=%v", err)
		}
	})

	t.Run("same-size byte change", func(t *testing.T) {
		path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "bytes")})
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		database, err := openReadOnlyCatalogDB(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		changed := []byte{body[len(body)-1] ^ 0xff}
		if _, err := file.WriteAt(changed, int64(len(body)-1)); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := database.Close(); err == nil || !strings.Contains(err.Error(), "bytes changed") {
			t.Fatalf("close byte-change error=%v", err)
		}
	})

	for _, suffix := range sqliteSidecarSuffixes {
		t.Run("sidecar "+strings.TrimPrefix(suffix, "-"), func(t *testing.T) {
			path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "sidecar-close")})
			database, err := openReadOnlyCatalogDB(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+suffix, []byte("appeared while open"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err == nil || !strings.Contains(err.Error(), suffix) {
				t.Fatalf("close sidecar %s error=%v", suffix, err)
			}
		})
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestSourceRequestsStartOnlyAfterCatalogLoaderReturns(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "关闭顺序")})
	catalog, err := loadCatalog(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	loaderReturned := false
	loader := func(context.Context, string) ([]catalogItem, error) {
		defer func() { loaderReturned = true }()
		return catalog, nil
	}
	_, err = executeWithCatalogLoader(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			if !loaderReturned {
				return nil, errors.New("source request started before catalog loader returned")
			}
			return nil, nil
		},
	}, time.Now, loader)
	if err != nil {
		t.Fatal(err)
	}
}

func TestClassificationCoversAllReportClasses(t *testing.T) {
	review := completeCatalogFixture(1, "需复核")
	review.lyricist = ""
	review.producer = ""
	gameAnchor := completeCatalogFixture(7, "短版关联")
	gameAnchor.version = "game_size"
	gameSibling := completeCatalogFixture(8, "短版关联")
	gameSibling.version = "game_size"
	path := writeV18Catalog(t, []catalogFixture{
		review,
		completeCatalogFixture(2, "唯一完整"),
		completeCatalogFixture(3, "多个候选"),
		completeCatalogFixture(4, "没有候选"),
		completeCatalogFixture(5, "内容不完整"),
		completeCatalogFixture(6, "上游错误"),
		gameAnchor,
		gameSibling,
	})
	var errorCalls atomic.Int32
	source := fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			if identity.Lyricist != "作詞者" || identity.Composer != "作曲者" || identity.Arranger != "編曲者" {
				return nil, fmt.Errorf("role-bound identity was not forwarded: %+v", identity)
			}
			switch identity.MusicID {
			case 2, 5:
				return []lyricssource.Candidate{candidateFor(identity.MusicID, "one")}, nil
			case 7:
				return nil, nil
			case 3:
				return []lyricssource.Candidate{candidateFor(3, "a"), candidateFor(3, "b")}, nil
			case 4:
				return nil, nil
			case 6:
				errorCalls.Add(1)
				return nil, &lyricssource.HTTPError{StatusCode: 503}
			default:
				return nil, fmt.Errorf("unexpected catalog-review search for music %d", identity.MusicID)
			}
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate := candidateFor(identity.MusicID, "one")
			if !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, fmt.Errorf("fixed candidate=%+v, want %+v", claimed, candidate)
			}
			if identity.MusicID == 5 {
				return lyricssource.FixedRevision{}, lyricssource.ErrMissingLyrics
			}
			return fixedFor(candidate, "本文は報告に出さない"), nil
		},
	}
	report, err := execute(context.Background(), testOptions(path), source, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CatalogReview) != 1 || len(report.GameSizeEvidence) != 1 || len(report.UniqueComplete) != 1 ||
		len(report.Ambiguous) != 1 || len(report.Missing) != 2 || len(report.Incomplete) != 1 || len(report.Error) != 1 {
		t.Fatalf("unexpected classes: %+v", report.Summary)
	}
	if report.CatalogReview[0].ReasonCode != "missing_role_bound_credits" {
		t.Fatalf("catalog review=%+v", report.CatalogReview[0])
	}
	if report.GameSizeEvidence[0].MusicID != 8 || report.GameSizeEvidence[0].TargetMusicID != 7 {
		t.Fatalf("game-size evidence=%+v", report.GameSizeEvidence)
	}
	if report.UniqueComplete[0].LineCount != 1 || report.UniqueComplete[0].Candidate == nil {
		t.Fatalf("unique complete=%+v", report.UniqueComplete[0])
	}
	if len(report.Ambiguous[0].Candidates) != 2 || len(report.Missing) != 2 ||
		report.Missing[0].SearchAttempts != 1 || report.Missing[1].SearchAttempts != 1 {
		t.Fatalf("ambiguous=%+v missing=%+v", report.Ambiguous[0], report.Missing)
	}
	if report.Incomplete[0].ErrorCode != "missing_lyrics" || report.Error[0].ErrorCode != "source_unavailable" {
		t.Fatalf("incomplete=%+v error=%+v", report.Incomplete[0], report.Error[0])
	}
	if errorCalls.Load() != 2 || report.Error[0].SearchAttempts != 2 {
		t.Fatalf("retry calls=%d item=%+v", errorCalls.Load(), report.Error[0])
	}
	if report.Summary.CatalogReview != 1 || report.Summary.GameSizeEvidence != 1 || report.Summary.UniqueComplete != 1 ||
		report.Summary.Ambiguous != 1 || report.Summary.Missing != 2 || report.Summary.Incomplete != 1 || report.Summary.Error != 1 {
		t.Fatalf("summary=%+v", report.Summary)
	}
}

func TestMissingSearchDiagnosticsRemainAggregateAndClassifyDeepestReason(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "无命中"), completeCatalogFixture(2, "署名失败"), completeCatalogFixture(3, "受限来源"),
	})
	report, err := execute(context.Background(), testOptions(path), diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			switch identity.MusicID {
			case 1:
				return nil, lyricssource.SearchDiagnostics{}, nil
			case 2:
				return nil, lyricssource.SearchDiagnostics{SearchHits: 8, TitleMismatch: 6, CreditMismatch: 2,
					LyricistCreditMismatch: 2, ComposerCreditMissing: 1}, nil
			case 3:
				return nil, lyricssource.SearchDiagnostics{SearchHits: 2, Restricted: 1, RestrictedTitleMatch: 1, TitleMismatch: 1}, nil
			default:
				return nil, lyricssource.SearchDiagnostics{}, errors.New("unexpected music")
			}
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 3 || report.Missing[0].ReasonCode != "no_search_hits" ||
		report.Missing[1].ReasonCode != "credit_mismatch" || report.Missing[2].ReasonCode != "restricted" {
		t.Fatalf("missing diagnostics=%+v", report.Missing)
	}
	if got := report.Missing[1].SearchDiagnostics; got == nil || got.SearchHits != 8 || got.TitleMismatch != 6 ||
		got.CreditMismatch != 2 || got.LyricistCreditMismatch != 2 || got.ComposerCreditMissing != 1 {
		t.Fatalf("aggregate diagnostics=%+v", got)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"source text", "lyrics body", "creator name"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("diagnostics leaked %q: %s", forbidden, body)
		}
	}
}

func TestFakeClientRetryAndConcurrencyAreBounded(t *testing.T) {
	t.Run("retry recovers", func(t *testing.T) {
		path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "重试")})
		var calls atomic.Int32
		report, err := execute(context.Background(), testOptions(path), fakeSourceClient{
			search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				if calls.Add(1) == 1 {
					return nil, &lyricssource.HTTPError{StatusCode: 429}
				}
				return nil, nil
			},
		}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 || len(report.Missing) != 1 || report.Missing[0].SearchAttempts != 2 {
			t.Fatalf("calls=%d report=%+v", calls.Load(), report)
		}
	})

	t.Run("final 429 remains rate limited", func(t *testing.T) {
		path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "最终限流")})
		opts := testOptions(path)
		opts.MaxAttempts = 3
		opts.RetryDelay = 0
		var calls atomic.Int32
		generated, err := execute(context.Background(), opts, fakeSourceClient{
			search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				calls.Add(1)
				return nil, &lyricssource.HTTPError{StatusCode: 429}
			},
		}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != int32(opts.MaxAttempts) || len(generated.Error) != 1 ||
			generated.Error[0].ErrorCode != "rate_limited" || generated.Error[0].SearchAttempts != opts.MaxAttempts {
			t.Fatalf("calls=%d report=%+v", calls.Load(), generated)
		}
	})

	t.Run("fetch retry recovers", func(t *testing.T) {
		path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "抓取重试")})
		candidate := candidateFor(1, "one")
		var calls atomic.Int32
		report, err := execute(context.Background(), testOptions(path), fakeSourceClient{
			search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				return []lyricssource.Candidate{candidate}, nil
			},
			fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				if calls.Add(1) == 1 {
					return lyricssource.FixedRevision{}, &lyricssource.HTTPError{StatusCode: 503}
				}
				return fixedFor(candidate, "成功"), nil
			},
		}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 || len(report.UniqueComplete) != 1 || report.UniqueComplete[0].FetchAttempts != 2 {
			t.Fatalf("calls=%d report=%+v", calls.Load(), report)
		}
	})

	t.Run("bounded concurrency", func(t *testing.T) {
		records := make([]catalogFixture, 24)
		for index := range records {
			records[index] = completeCatalogFixture(index+1, fmt.Sprintf("并发-%02d", index+1))
		}
		path := writeV18Catalog(t, records)
		opts := testOptions(path)
		opts.Concurrency = 3
		var active, maximum atomic.Int32
		report, err := execute(context.Background(), opts, fakeSourceClient{
			search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					observed := maximum.Load()
					if current <= observed || maximum.CompareAndSwap(observed, current) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				return nil, nil
			},
		}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if maximum.Load() < 2 || maximum.Load() > int32(opts.Concurrency) {
			t.Fatalf("maximum fake-client concurrency=%d, bound=%d", maximum.Load(), opts.Concurrency)
		}
		if len(report.Missing) != len(records) {
			t.Fatalf("missing=%d want=%d", len(report.Missing), len(records))
		}
	})
}

func TestFixedRevisionWithoutStructuredExtractionIsIncomplete(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "结构提取")})
	candidate := candidateFor(1, "one")
	report, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fixed := fixedFor(candidate, "正文")
			fixed.Extraction = lyricssource.Extraction{}
			return fixed, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Incomplete) != 1 || report.Incomplete[0].ErrorCode != "invalid_extraction_size" {
		t.Fatalf("report=%+v", report)
	}
}

func TestReportDoesNotLeakWikitextLyricsOrRawErrors(t *testing.T) {
	const (
		secretWikitext = "SECRET_RAW_WIKITEXT_9e2f"
		secretLyrics   = "SECRET_LYRICS_BODY_3b71"
		secretError    = "SECRET_UPSTREAM_ERROR_f991"
	)
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "安全结果"),
		completeCatalogFixture(2, "安全错误"),
	})
	report, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			if identity.MusicID == 2 {
				return nil, errors.New(secretError)
			}
			return []lyricssource.Candidate{candidateFor(1, "safe")}, nil
		},
		fetchCandidate: func(_ context.Context, _ lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate := candidateFor(1, "safe")
			fixed := fixedFor(candidate, secretLyrics)
			fixed.Wikitext = []byte(secretWikitext)
			return fixed, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(body)
	for _, forbidden := range []string{secretWikitext, secretLyrics, secretError, `"wikitext"`, `"lines"`, `"japanese"`} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("report leaked %q: %s", forbidden, serialized)
		}
	}
	if len(report.Incomplete) != 1 || len(report.Error) != 1 || report.Error[0].ErrorCode == "" {
		t.Fatalf("report=%+v", report)
	}
	outputPath := filepath.Join(t.TempDir(), "private-report.json")
	if err := writeReport(outputPath, path, report, io.Discard); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secretWikitext, secretLyrics, secretError, `"wikitext"`, `"lines"`, `"japanese"`} {
		if bytes.Contains(output, []byte(forbidden)) {
			t.Fatalf("written report leaked %q: %s", forbidden, output)
		}
	}
}

func TestExecuteRejectsDuplicateLoaderMusicIDs(t *testing.T) {
	evidence := model.CatalogLyricsEvidence{
		Title: "重复", Lyricist: "作詞者", Composer: "作曲者", Arranger: "編曲者", LyricsVersion: "full",
		Presence: model.CatalogEvidencePresence{Lyricist: true, Composer: true, Arranger: true, LyricsVersion: true},
		Vocals:   []model.CatalogVocalSignal{},
	}
	fingerprint, err := model.CatalogLyricsEvidenceFingerprint(evidence)
	if err != nil {
		t.Fatal(err)
	}
	item := catalogItem{MusicID: 1, JapaneseTitle: "重复", ProducerMetadata: "作詞者", Lyricist: "作詞者",
		Composer: "作曲者", Arranger: "編曲者", Evidence: evidence, CatalogFingerprint: fingerprint}
	_, err = executeWithCatalogLoader(context.Background(), options{
		DatabasePath: "unused", Concurrency: 1, MaxAttempts: 1, RequestTimeout: time.Second,
	}, fakeSourceClient{}, time.Now, func(context.Context, string) ([]catalogItem, error) {
		return []catalogItem{item, item}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate music IDs") {
		t.Fatalf("duplicate loader error=%v", err)
	}
}

func TestCancellationDoesNotStartCatalogOrSourceWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loaderCalled := false
	sourceCalled := false
	_, err := executeWithCatalogLoader(ctx, options{
		DatabasePath: "unused", Concurrency: 1, MaxAttempts: 1, RequestTimeout: time.Second, RetryDelay: 0,
	}, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalled = true
			return nil, nil
		},
	}, time.Now, func(context.Context, string) ([]catalogItem, error) {
		loaderCalled = true
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || loaderCalled || sourceCalled {
		t.Fatalf("err=%v loaderCalled=%t sourceCalled=%t", err, loaderCalled, sourceCalled)
	}
}

func TestCancellationStopsInFlightFakeClient(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "取消")})
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := execute(ctx, testOptions(path), fakeSourceClient{
			search: func(ctx context.Context, _ lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}, time.Now)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fake client did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled execute error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled execute leaked")
	}
}

func TestCancellationStopsRetryDelay(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "取消重试等待")})
	opts := testOptions(path)
	opts.MaxAttempts = 2
	opts.RetryDelay = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstAttempt := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		_, err := execute(ctx, opts, fakeSourceClient{
			search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				calls.Add(1)
				close(firstAttempt)
				return nil, &lyricssource.HTTPError{StatusCode: 429}
			},
		}, time.Now)
		done <- err
	}()
	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("first source attempt did not run")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled retry delay error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled retry delay did not return")
	}
	if calls.Load() != 1 {
		t.Fatalf("source calls=%d, want 1", calls.Load())
	}
}

func TestRunContextHonorsCanceledParentWithoutOutput(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	outputPath := filepath.Join(filepath.Dir(path), "canceled.json")
	err := runContext(ctx, []string{"-db", path, "-output", outputPath}, &output)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runContext error=%v, want context.Canceled", err)
	}
	if output.Len() != 0 {
		t.Fatalf("canceled runContext wrote output: %q", output.String())
	}
	if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canceled runContext created report: %v", statErr)
	}
}

func TestAmbiguousCandidateLimitIsBounded(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "候选上限")})
	candidates := make([]lyricssource.Candidate, maxReportCandidates+1)
	for index := range candidates {
		candidates[index] = candidateFor(index+1, fmt.Sprintf("limit-%d", index))
	}
	report, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return candidates, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Ambiguous) != 1 || report.Ambiguous[0].ErrorCode != "candidate_limit_exceeded" ||
		len(report.Ambiguous[0].Candidates) != 0 {
		t.Fatalf("ambiguous limit report=%+v", report)
	}
}

func TestGameSizeOnlyCatalogSearchesOnlyDeterministicAnchor(t *testing.T) {
	first := completeCatalogFixture(11, "同曲")
	first.version = "game_size"
	second := completeCatalogFixture(12, "同曲")
	second.version = "game_size"
	path := writeV18Catalog(t, []catalogFixture{first, second})
	var searched atomic.Int32
	report, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			if identity.MusicID != 11 {
				return nil, fmt.Errorf("searched non-anchor music %d", identity.MusicID)
			}
			searched.Add(1)
			return nil, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if searched.Load() != 1 || len(report.GameSizeEvidence) != 1 ||
		report.GameSizeEvidence[0].MusicID != 12 || report.GameSizeEvidence[0].TargetMusicID != 11 {
		t.Fatalf("searched=%d report=%+v", searched.Load(), report)
	}
}

func TestLoadCatalogRejectsSchemaOtherThanV18(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "版本")})
	database, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM schema_migrations WHERE version=18`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCatalog(context.Background(), path); err == nil || !strings.Contains(err.Error(), "exactly v18") {
		t.Fatalf("schema mismatch error=%v", err)
	}
}

func TestCatalogCapacityIncludes701Records(t *testing.T) {
	records := make([]catalogFixture, 701)
	for index := range records {
		records[index] = completeCatalogFixture(index+1, fmt.Sprintf("容量-%03d", index+1))
	}
	path := writeV18Catalog(t, records)
	var searches atomic.Int32
	opts := testOptions(path)
	opts.Concurrency = 8
	report, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			searches.Add(1)
			return nil, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if report.CatalogCount != 701 || len(report.Missing) != 701 || searches.Load() != 701 {
		t.Fatalf("catalog=%d missing=%d searches=%d", report.CatalogCount, len(report.Missing), searches.Load())
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("empty 701-record report")
	}
}

func TestResumeReportRetriesOnlySelectedSafeErrorsAndMergesComplete701Items(t *testing.T) {
	records := make([]catalogFixture, 701)
	for index := range records {
		records[index] = completeCatalogFixture(index+1, fmt.Sprintf("续跑-%03d", index+1))
	}
	path := writeV18Catalog(t, records)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var initialCalls atomic.Int32
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			initialCalls.Add(1)
			switch identity.MusicID {
			case 17:
				return nil, &lyricssource.HTTPError{StatusCode: 429}
			case 33:
				return nil, &lyricssource.HTTPError{StatusCode: 503}
			case 44:
				return nil, lyricssource.ErrMalformedResponse
			case 2:
				return []lyricssource.Candidate{candidateFor(2, "one")}, nil
			default:
				return nil, nil
			}
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate := candidateFor(identity.MusicID, "one")
			return fixedFor(candidate, "成功"), nil
		},
	}, func() time.Time { return time.Unix(100, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if initialCalls.Load() != 703 || prior.CatalogCount != 701 || reportItemCount(prior) != 701 ||
		len(prior.UniqueComplete) != 1 || len(prior.Missing) != 697 || len(prior.Error) != 3 {
		t.Fatalf("initial calls=%d report=%+v", initialCalls.Load(), prior.Summary)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)
	priorBytes, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "rate_limited,source_unavailable"
	var retriedMu sync.Mutex
	retriedIDs := []int{}
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			retriedMu.Lock()
			retriedIDs = append(retriedIDs, identity.MusicID)
			retriedMu.Unlock()
			if identity.MusicID == 17 {
				return []lyricssource.Candidate{candidateFor(17, "one")}, nil
			}
			if identity.MusicID == 33 {
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected resume request for music %d", identity.MusicID)
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate := candidateFor(identity.MusicID, "one")
			return fixedFor(candidate, "恢复"), nil
		},
	}, func() time.Time { return time.Unix(200, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	sort.Ints(retriedIDs)
	if !equalMusicIDs(retriedIDs, []int{17, 33}) {
		t.Fatalf("retried IDs=%v", retriedIDs)
	}
	if merged.CatalogCount != 701 || reportItemCount(merged) != 701 || len(merged.UniqueComplete) != 2 ||
		len(merged.Missing) != 698 || len(merged.Error) != 1 || merged.Error[0].MusicID != 44 ||
		merged.Error[0].ErrorCode != "malformed_response" {
		t.Fatalf("merged report=%+v error=%+v", merged.Summary, merged.Error)
	}
	var retained *reportItem
	for index := range merged.UniqueComplete {
		if merged.UniqueComplete[index].MusicID == 2 {
			retained = &merged.UniqueComplete[index]
			break
		}
	}
	retainedJSON, _ := json.Marshal(retained)
	priorJSON, _ := json.Marshal(prior.UniqueComplete[0])
	if retained == nil || !bytes.Equal(retainedJSON, priorJSON) {
		t.Fatalf("successful prior item was not retained exactly: retained=%+v prior=%+v", retained, prior.UniqueComplete[0])
	}
	output := filepath.Join(t.TempDir(), "merged.json")
	if err := writeReport(output, path, merged, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("resume preflight modified the catalog database")
	}
	currentPriorBytes, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priorBytes, currentPriorBytes) {
		t.Fatal("resume preflight modified the input report")
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("resume preflight left SQLite sidecar %s: %v", suffix, err)
		}
	}
}

func TestResumeReportRetriesSelectedDeterministicIncompleteCodes(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "格式一"), completeCatalogFixture(2, "格式二"), completeCatalogFixture(3, "格式三"),
	})
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidateFor(identity.MusicID, "one")}, nil
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			switch identity.MusicID {
			case 1:
				return lyricssource.FixedRevision{}, lyricssource.ErrUnsupportedTable
			case 2:
				return lyricssource.FixedRevision{}, lyricssource.ErrMissingLyrics
			default:
				return lyricssource.FixedRevision{}, lyricssource.ErrAmbiguous
			}
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Incomplete) != 3 {
		t.Fatalf("prior summary=%+v", prior.Summary)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeIncompleteCodes = "unsupported_format,missing_lyrics"
	var retriedMu sync.Mutex
	retriedIDs := []int{}
	var searchCalls atomic.Int32
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			searchCalls.Add(1)
			return nil, fmt.Errorf("unexpected search during fixed incomplete resume for music %d", identity.MusicID)
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			retriedMu.Lock()
			retriedIDs = append(retriedIDs, identity.MusicID)
			retriedMu.Unlock()
			candidate := candidateFor(identity.MusicID, "one")
			return fixedFor(candidate, "修复后完成"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sort.Ints(retriedIDs)
	if searchCalls.Load() != 0 || !equalMusicIDs(retriedIDs, []int{1, 2}) || len(merged.UniqueComplete) != 2 || len(merged.Incomplete) != 1 ||
		merged.Incomplete[0].MusicID != 3 || merged.Incomplete[0].ErrorCode != "ambiguous_source" {
		t.Fatalf("searchCalls=%d retried=%v summary=%+v incomplete=%+v", searchCalls.Load(), retriedIDs, merged.Summary, merged.Incomplete)
	}
	priorByID := make(map[int]reportItem, len(prior.Incomplete))
	for _, item := range prior.Incomplete {
		priorByID[item.MusicID] = item
	}
	for _, item := range merged.UniqueComplete {
		previous := priorByID[item.MusicID]
		if item.SearchAttempts != previous.SearchAttempts || !reflect.DeepEqual(item.SearchDiagnostics, previous.SearchDiagnostics) {
			t.Fatalf("music %d did not retain prior search audit fields: got=%+v prior=%+v", item.MusicID, item, previous)
		}
	}
}

func TestResumeReportAcceptsClassifierAssociationSetOnGameSizeEvidence(t *testing.T) {
	item := reportItem{
		MusicID: 131, JapaneseTitle: "初音ミクの激唱", CatalogFingerprint: strings.Repeat("a", 64),
		TargetMusicID: 388, AssociationMusicIDs: []int{131},
	}
	if err := validateResumeReportItem("game_size_evidence", item); err != nil {
		t.Fatalf("valid game-size association set rejected: %v", err)
	}
}

func TestResumeReportRetriesOnlySearchPhaseMalformedResponses(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "异常一"),
		completeCatalogFixture(2, "异常二"),
		completeCatalogFixture(3, "稳定缺失"),
	})
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			if identity.MusicID == 3 {
				return nil, nil
			}
			return nil, lyricssource.ErrMalformedResponse
		},
	}, time.Now)
	if err != nil || len(prior.Error) != 2 || len(prior.Missing) != 1 {
		t.Fatalf("prior summary=%+v err=%v", prior.Summary, err)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "malformed_response"
	opts.ResumeMissingReasons = "none"
	var retriedMu sync.Mutex
	retried := []int{}
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			retriedMu.Lock()
			retried = append(retried, identity.MusicID)
			retriedMu.Unlock()
			return []lyricssource.Candidate{candidateFor(identity.MusicID, "one")}, nil
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate := candidateFor(identity.MusicID, "one")
			return fixedFor(candidate, "恢复完成"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sort.Ints(retried)
	priorStable, marshalErr := json.Marshal(prior.Missing[0])
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	mergedStable, marshalErr := json.Marshal(merged.Missing[0])
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !equalMusicIDs(retried, []int{1, 2}) || len(merged.UniqueComplete) != 2 || len(merged.Missing) != 1 ||
		merged.Missing[0].MusicID != 3 || !bytes.Equal(mergedStable, priorStable) {
		t.Fatalf("retried=%v summary=%+v missing=%+v", retried, merged.Summary, merged.Missing)
	}
}

func TestResumeReportRejectsFetchPhaseMalformedResponse(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "固定异常")})
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidateFor(identity.MusicID, "one")}, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return lyricssource.FixedRevision{}, lyricssource.ErrMalformedResponse
		},
	}, time.Now)
	if err != nil || len(prior.Error) != 1 || prior.Error[0].Candidate == nil || prior.Error[0].FetchAttempts != 1 {
		t.Fatalf("prior=%+v err=%v", prior, err)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)
	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "malformed_response"
	var sourceCalls atomic.Int32
	_, err = execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, nil
		},
	}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "not safe to retry from its recorded phase") || sourceCalls.Load() != 0 {
		t.Fatalf("err=%v sourceCalls=%d", err, sourceCalls.Load())
	}
}

func TestResumeReportRejectsUnsafeRetryCodeBeforeCatalogOrSourceWork(t *testing.T) {
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, report{})
	loaderCalled := false
	sourceCalled := false
	opts := testOptions("unused")
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "source_http_error"
	_, err := executeWithCatalogLoader(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalled = true
			return nil, nil
		},
	}, time.Now, func(context.Context, string) ([]catalogItem, error) {
		loaderCalled = true
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe or unsupported") || loaderCalled || sourceCalled {
		t.Fatalf("err=%v loaderCalled=%t sourceCalled=%t", err, loaderCalled, sourceCalled)
	}
}

func TestResumeReportRejectsUnsafeIncompleteRetryCodeBeforeCatalogOrSourceWork(t *testing.T) {
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, report{})
	for _, code := range []string{"source_drift", "restricted_reprint", "lyrics_unpublished", "unknown_incomplete"} {
		t.Run(code, func(t *testing.T) {
			loaderCalled := false
			sourceCalled := false
			opts := testOptions("unused")
			opts.ResumeReportPath = priorPath
			opts.ResumeIncompleteCodes = code
			_, err := executeWithCatalogLoader(context.Background(), opts, fakeSourceClient{
				search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
					sourceCalled = true
					return nil, nil
				},
			}, time.Now, func(context.Context, string) ([]catalogItem, error) {
				loaderCalled = true
				return nil, nil
			})
			if err == nil || !strings.Contains(err.Error(), "unsafe or unsupported") || loaderCalled || sourceCalled {
				t.Fatalf("err=%v loaderCalled=%t sourceCalled=%t", err, loaderCalled, sourceCalled)
			}
		})
	}
}

func TestResumeReportValidationRejectsStaleOrMalformedInputBeforeSourceRequests(t *testing.T) {
	testCases := []struct {
		name string
		edit func(*report)
		want string
	}{
		{name: "schema", edit: func(prior *report) { prior.SchemaVersion++ }, want: "envelope"},
		{name: "summary", edit: func(prior *report) { prior.Summary.Missing++ }, want: "envelope"},
		{name: "catalog count", edit: func(prior *report) { prior.CatalogCount++ }, want: "envelope"},
		{name: "item moved to incompatible class", edit: func(prior *report) {
			prior.Missing = append(prior.Missing, prior.UniqueComplete[0])
			prior.UniqueComplete = prior.UniqueComplete[:0]
			prior.Summary.Missing++
			prior.Summary.UniqueComplete--
		}, want: "invalid missing item"},
		{name: "fingerprint", edit: func(prior *report) { prior.Missing[0].CatalogFingerprint = strings.Repeat("f", 64) }, want: "fingerprint"},
		{name: "title", edit: func(prior *report) { prior.Missing[0].JapaneseTitle = "stale" }, want: "title"},
		{name: "current catalog-only classification", edit: func(prior *report) {
			moved := prior.Missing[0]
			moved.SearchAttempts = 0
			moved.SearchDiagnostics = nil
			moved.ReasonCode = "missing_role_bound_credits"
			prior.CatalogReview = append(prior.CatalogReview, moved)
			prior.Missing = prior.Missing[1:]
			prior.Summary.Missing--
			prior.Summary.CatalogReview++
		}, want: "classification"},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "校验一"), completeCatalogFixture(2, "校验二")})
			prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
				search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
					if identity.MusicID == 2 {
						return []lyricssource.Candidate{candidateFor(2, "one")}, nil
					}
					return nil, nil
				},
				fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
					candidate := candidateFor(identity.MusicID, "one")
					return fixedFor(candidate, "完成"), nil
				},
			}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&prior)
			priorPath := filepath.Join(t.TempDir(), "prior.json")
			writeTestReport(t, priorPath, prior)
			opts := testOptions(path)
			opts.ResumeReportPath = priorPath
			opts.ResumeErrorCodes = "rate_limited"
			sourceCalled := false
			_, err = execute(context.Background(), opts, fakeSourceClient{
				search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
					sourceCalled = true
					return nil, nil
				},
			}, time.Now)
			if err == nil || !strings.Contains(err.Error(), test.want) || sourceCalled {
				t.Fatalf("err=%v want=%q sourceCalled=%t", err, test.want, sourceCalled)
			}
		})
	}
}

func TestResumeReportRejectsUnknownJSONFieldBeforeCatalogOrSourceWork(t *testing.T) {
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	if err := os.WriteFile(priorPath, []byte(`{"schemaVersion":1,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaderCalled := false
	sourceCalled := false
	opts := testOptions("unused")
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "rate_limited"
	_, err := executeWithCatalogLoader(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalled = true
			return nil, nil
		},
	}, time.Now, func(context.Context, string) ([]catalogItem, error) {
		loaderCalled = true
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") || loaderCalled || sourceCalled {
		t.Fatalf("err=%v loaderCalled=%t sourceCalled=%t", err, loaderCalled, sourceCalled)
	}
}

func TestValidateOptionsRejectsResumeReportAsOutput(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	prior := report{
		SchemaVersion: reportSchemaVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		CatalogSchemaVersion: catalogSchemaVersion,
		CatalogReview:        []reportItem{}, GameSizeEvidence: []reportItem{}, UniqueComplete: []reportItem{},
		Ambiguous: []reportItem{}, Missing: []reportItem{}, Incomplete: []reportItem{}, Error: []reportItem{},
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)
	opts := testOptions(path)
	opts.OutputPath = priorPath
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "rate_limited"
	if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "path already exists") {
		t.Fatalf("validation error=%v", err)
	}
}

func TestReportOutputFailureDoesNotTouchDatabase(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	missingParent := filepath.Join(t.TempDir(), "missing", "report.json")
	validEmpty := report{
		SchemaVersion: reportSchemaVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), CatalogSchemaVersion: catalogSchemaVersion,
		CatalogReview: []reportItem{}, GameSizeEvidence: []reportItem{}, UniqueComplete: []reportItem{},
		Ambiguous: []reportItem{}, Missing: []reportItem{}, Incomplete: []reportItem{}, Error: []reportItem{},
	}
	if err := writeReport(missingParent, path, validEmpty, io.Discard); err == nil {
		t.Fatal("report writer accepted a missing parent directory")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("report output failure modified the database")
	}
}

func TestValidateOptionsRejectsOutputBeforeDatabaseInspection(t *testing.T) {
	output := filepath.Join(t.TempDir(), "existing.json")
	if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(filepath.Join(t.TempDir(), "missing.db"))
	opts.OutputPath = output
	if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "path already exists") {
		t.Fatalf("validation error=%v", err)
	}
}

func TestRunRejectsExistingOutputBeforeSourceRequests(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	output := filepath.Join(t.TempDir(), "existing.json")
	if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-db", path, "-output", output, "-request-timeout", "1ms"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "path already exists") {
		t.Fatalf("existing-output error=%v", err)
	}
	body, readErr := os.ReadFile(output)
	if readErr != nil || string(body) != "keep" {
		t.Fatalf("existing output changed: %q err=%v", body, readErr)
	}
}

func TestRunRejectsDatabaseAsOutputBeforeSourceRequests(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	if err := run([]string{"-db", path, "-output", path}, io.Discard); err == nil || !strings.Contains(err.Error(), "database path") {
		t.Fatalf("database-output error=%v", err)
	}
}

func TestWriteReportRejectsStdoutAndInconsistentEnvelope(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	var stdout bytes.Buffer
	if err := writeReport("-", path, report{}, &stdout); err == nil ||
		!strings.Contains(err.Error(), "stdout is not supported") || stdout.Len() != 0 {
		t.Fatalf("stdout report err=%v output=%q", err, stdout.String())
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.json")
	if err := writeReport(invalidPath, path, report{}, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid preflight report") {
		t.Fatalf("invalid report error=%v", err)
	}
	if _, err := os.Stat(invalidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid report file exists: %v", err)
	}

	existing := filepath.Join(t.TempDir(), "existing.json")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(existing, path, report{}, io.Discard); err == nil || !strings.Contains(err.Error(), "path already exists") {
		t.Fatalf("invalid existing report error=%v", err)
	}
}

func TestRunRejectsStdoutBeforeCatalogOrSourceWork(t *testing.T) {
	missingDatabase := filepath.Join(t.TempDir(), "missing.db")
	var stdout bytes.Buffer
	err := run([]string{"-db", missingDatabase, "-output", "-"}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "stdout is not supported") {
		t.Fatalf("stdout validation error=%v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout validation emitted report bytes: %q", stdout.String())
	}
}

func TestRunWritesPrivateJSONFile(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	outputPath := filepath.Join(t.TempDir(), "preflight.json")
	var stdout bytes.Buffer
	if err := run([]string{"-db", path, "-output", outputPath}, &stdout); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("successful private report wrote stdout: %q", stdout.String())
	}
	body, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var generated report
	if err := json.Unmarshal(body, &generated); err != nil {
		t.Fatalf("private JSON: %v body=%q", err, body)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private report permissions=%#o, want 0600", info.Mode().Perm())
	}
	if generated.CatalogSchemaVersion != catalogSchemaVersion || generated.CatalogCount != 0 ||
		generated.CatalogReview == nil || generated.GameSizeEvidence == nil || generated.UniqueComplete == nil ||
		generated.Ambiguous == nil || generated.Missing == nil || generated.Incomplete == nil || generated.Error == nil {
		t.Fatalf("private report=%+v", generated)
	}
}

func TestCommandSourceDoesNotImportForbiddenPersistencePackages(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"internal/db", "internal/store", "internal/lyricsdiscovery"} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("%s imports forbidden package %q", entry.Name(), forbidden)
			}
		}
	}
}
