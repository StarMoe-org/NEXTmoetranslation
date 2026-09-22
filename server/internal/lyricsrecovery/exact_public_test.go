package lyricsrecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsextractionplan"
	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

const exactPublic795FixtureRoot = "/private/tmp/moesekai-external-runtime/today-release-20260803/sekaipedia-url-preflight-20260803T130023Z-37903"

func TestBuildExactPublicFixedGeneratesCompleteHanOnlyRuby(t *testing.T) {
	target := lyricssource.ExactPublicPageTarget{
		MusicID: 795, PageURL: "https://zh.moegirl.org.cn/%E4%BA%BF%E5%B9%B4%E7%88%B1%E6%81%8B",
		PageTitle: "亿年爱恋", JapaneseTitle: "一億年恋してる", PageID: 649688,
		RevisionID: 8500224, FetchedAt: "2026-08-03T14:58:50.501307Z",
	}
	extracted := lyricssource.MoegirlPublicHTMLExtraction{
		PageURL: target.PageURL, PageTitle: target.PageTitle, JapaneseTitle: target.JapaneseTitle,
		PageID: target.PageID, RevisionID: target.RevisionID, RightsNotice: "fixed rights",
		Lines: []lyricssource.MoegirlPublicHTMLLine{{Japanese: "生命へ A1！", Translation: "translation"}},
	}
	fixed, _, _, err := buildExactPublicFixed(
		target, extracted, []byte("fixed exact public response"),
		time.Date(2026, time.August, 3, 14, 58, 50, 501307000, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Extraction.RubyGeneratorVersion != lyricssource.DeterministicRubyGeneratorVersion() ||
		len(fixed.Extraction.Lines) != 1 || len(fixed.Extraction.Lines[0].Segments) != 1 {
		t.Fatalf("exact public structured ruby metadata=%+v", fixed.Extraction)
	}
	segment := fixed.Extraction.Lines[0].Segments[0]
	var rebuilt strings.Builder
	for _, span := range segment.Ruby {
		rebuilt.WriteString(span.Text)
		for _, current := range span.Text {
			if unicode.In(current, unicode.Han) && span.Reading == "" {
				t.Fatalf("exact public Han has no reading: %+v", span)
			}
			if !unicode.In(current, unicode.Han) && span.Reading != "" {
				t.Fatalf("exact public non-Han received reading: %+v", span)
			}
		}
	}
	if rebuilt.String() != segment.Text || segment.Text != "生命へ A1！" {
		t.Fatalf("exact public ruby changed source text: %+v", segment)
	}
}

func TestReal795ExactPublicArtifactAcquireLedgerReplayWithoutICUOrNetwork(t *testing.T) {
	target := realExactPublic795Target(t)
	runtime := exactPublic795Runtime(t, target)
	if runtime.Order[0] != lyricssource.ProviderMoegirlPublicExact ||
		runtime.Providers[0].APIEndpoint != "" || runtime.Providers[0].Origin != "https://zh.moegirl.org.cn" {
		t.Fatalf("exact public runtime crossed its provider boundary: %+v", runtime.Providers[0])
	}

	ctx := context.Background()
	root := privateRecoveryTempDir(t)
	ledger, err := lyricsacquisition.CreateLedger(ctx, filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	if _, err := NewAcquisitionSession(runtime, ledger, map[model.LyricsSourceProvider]http.RoundTripper{
		lyricssource.ProviderMoegirlPublicExact: http.DefaultTransport,
	}); err == nil || !strings.Contains(err.Error(), "must not receive a live or ICU transport") {
		t.Fatalf("exact public provider accepted a live transport: %v", err)
	}
	session, err := NewAcquisitionSession(
		runtime, ledger, map[model.LyricsSourceProvider]http.RoundTripper{},
	)
	if err != nil {
		t.Fatal(err)
	}
	identity := lyricssource.MusicIdentity{MusicID: 795, JapaneseTitle: "一億年恋してる"}
	sets, progress, err := session.AcquireSong(ctx, 795, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(progress) != 1 || sets[0].Provider != lyricssource.ProviderMoegirlPublicExact ||
		sets[0].Status != lyricsprovideroutcome.StatusCandidate || len(sets[0].AcquisitionIDs) != 1 ||
		progress[0].EnterResult != ProviderOutcomeCompleteCompositionStop {
		t.Fatalf("795 exact public acquisition terminal is invalid: sets=%+v progress=%+v", sets, progress)
	}

	acquired, err := ledger.ReplayByAcquisitionID(ctx, sets[0].AcquisitionIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if acquired.Request.Provider != string(lyricssource.ProviderMoegirlPublicExact) ||
		acquired.Request.CanonicalRequestIdentity != target.PageURL ||
		acquired.Request.RevisionSelector != "public-revision:8500224" ||
		acquired.RawResponseSHA256 != target.RawHTML.SHA256 || len(acquired.RawResponse) != int(target.RawHTML.SizeBytes) {
		t.Fatalf("795 ledger acquisition identity is invalid: %+v", acquired.Request)
	}
	acquiredJSON, _ := json.Marshal(acquired)
	if strings.Contains(strings.ToLower(string(acquiredJSON)), "moegirl.icu") {
		t.Fatal("795 exact public acquisition leaked the ICU origin")
	}

	first, err := ReplaySong(ctx, 795, identity, runtime.PolicyVersion, runtime, ledger, sets)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReplaySong(ctx, 795, identity, runtime.PolicyVersion, runtime, ledger, sets)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Providers) != 1 || first.Providers[0].Fixed == nil ||
		first.Providers[0].Artifact.Provider != lyricssource.ProviderMoegirlPublicExact ||
		first.Providers[0].Fixed.CanonicalURL != target.PageURL ||
		first.Providers[0].Fixed.Extraction.RubyGeneratorVersion != lyricssource.DeterministicRubyGeneratorVersion() ||
		len(first.Providers[0].Fixed.Extraction.Lines) != 65 || len(first.Providers[0].Fixed.Translations) != 65 ||
		first.Providers[0].Fixed.Translations[0] != "带着乱蓬蓬的睡发" ||
		first.Providers[0].Fixed.Translations[64] != "要再来啊！" ||
		len(first.Providers[0].Outcome.Candidates) != 1 {
		t.Fatalf("795 exact public replay did not retain the exact 65 Japanese/Chinese pairs")
	}
	candidate := first.Providers[0].Outcome.Candidates[0]
	stagedCandidate := lyricsstaging.CandidateIdentity{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, RevisionTimestamp: candidate.RevisionTimestamp,
		SHA1: candidate.SHA1, Title: candidate.Title, CanonicalURL: candidate.CanonicalURL,
		Categories: append([]string{}, candidate.Categories...), Section: candidate.Section,
		RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef(nil), candidate.IndexEvidenceRefs...),
	}
	staged, err := lyricsstaging.BuildDraft(
		lyricsstaging.PreflightItem{
			MusicID: 795, JapaneseTitle: identity.JapaneseTitle, CatalogFingerprint: strings.Repeat("b", 64),
			TargetMusicID: 795, AssociationMusicIDs: []int{}, Candidate: &stagedCandidate, LineCount: 65,
			SearchAttempts: 1, FetchAttempts: 1,
		},
		lyricscontract.CatalogIdentity{
			MusicID: 795, JapaneseTitle: identity.JapaneseTitle, ProducerMetadata: "いのうつはSA",
			Lyricist: "いのうつはSA", Composer: "いのうつはSA", Arranger: "いのうつはSA",
			Vocals:             []model.CatalogVocalSignal{{VocalID: 795, VocalType: "original_song", Caption: "バーチャル・シンガーver."}},
			CatalogFingerprint: strings.Repeat("b", 64),
		},
		*first.Providers[0].Fixed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Translations) != 65 || staged.Translations[0] != "带着乱蓬蓬的睡发" ||
		staged.Source.CanonicalURL != target.PageURL || strings.Contains(strings.ToLower(staged.Source.CanonicalURL), "icu") {
		t.Fatalf("795 staging draft lost its exact public URL or translations: %+v", staged.Source)
	}
	firstResult, err := NewSongResult(first)
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := NewSongResult(second)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := MarshalSongResult(firstResult)
	secondBody, _ := MarshalSongResult(secondResult)
	if !bytes.Equal(firstBody, secondBody) || firstResult.State != lyricscontract.CoverageComplete ||
		firstResult.Full == nil || len(firstResult.Full.Lines) != 65 || len(firstResult.Translations) != 65 ||
		firstResult.Translations[0] != "带着乱蓬蓬的睡发" || firstResult.Translations[64] != "要再来啊！" ||
		len(firstResult.ProviderOutcomes) != 1 ||
		firstResult.ProviderOutcomes[0].Provider != lyricssource.ProviderMoegirlPublicExact {
		t.Fatalf("795 exact-ID replay is not a stable complete result: %+v", firstResult)
	}
	lower := strings.ToLower(string(firstBody))
	for _, forbidden := range []string{"moegirl.icu", "romaji", "romanization"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("795 song result crossed a forbidden boundary: %s", forbidden)
		}
	}
}

func TestReal795ExactPublicArtifactRejectsTargetAndReportTampering(t *testing.T) {
	base := realExactPublic795Target(t)

	for name, mutate := range map[string]func(*testing.T, *lyricssource.ExactPublicPageTarget){
		"raw HTML hash": func(_ *testing.T, target *lyricssource.ExactPublicPageTarget) {
			target.RawHTML.SHA256 = strings.Repeat("0", 64)
		},
		"page revision": func(_ *testing.T, target *lyricssource.ExactPublicPageTarget) {
			target.RevisionID++
		},
		"different complete URL": func(_ *testing.T, target *lyricssource.ExactPublicPageTarget) {
			target.PageURL = "https://zh.moegirl.org.cn/%E4%B8%80%E5%84%84%E5%B9%B4%E6%81%8B%E3%81%97%E3%81%A6%E3%82%8B"
			target.PageTitle = "一億年恋してる"
		},
		"report page identity": func(t *testing.T, target *lyricssource.ExactPublicPageTarget) {
			body, err := os.ReadFile(target.ExtractionReport.Path)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.Replace(body, []byte(`"pageId":649688`), []byte(`"pageId":649689`), 1)
			bindTamperedExactPublicReport(t, target, body)
		},
		"report romaji field": func(t *testing.T, target *lyricssource.ExactPublicPageTarget) {
			body, err := os.ReadFile(target.ExtractionReport.Path)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.Replace(body, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"romaji":"forbidden"`), 1)
			bindTamperedExactPublicReport(t, target, body)
		},
	} {
		t.Run(name, func(t *testing.T) {
			target := base
			mutate(t, &target)
			runtime, err := newExactPublic795Runtime(target)
			if err != nil {
				return
			}
			root := privateRecoveryTempDir(t)
			ledger, err := lyricsacquisition.CreateLedger(t.Context(), filepath.Join(root, "ledger"))
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			session, err := NewAcquisitionSession(runtime, ledger, map[model.LyricsSourceProvider]http.RoundTripper{})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = session.AcquireSong(
				t.Context(), 795, lyricssource.MusicIdentity{MusicID: 795, JapaneseTitle: "一億年恋してる"},
			)
			if err == nil {
				t.Fatal("tampered 795 exact public artifact was accepted")
			}
		})
	}

	icu := base
	icu.PageURL = "https://moegirl.icu/api.php"
	if _, err := newExactPublic795Runtime(icu); err == nil {
		t.Fatal("moegirl_public_exact accepted an ICU URL")
	}
}

func realExactPublic795Target(t *testing.T) lyricssource.ExactPublicPageTarget {
	t.Helper()
	rawPath := filepath.Join(exactPublic795FixtureRoot, "moegirl-795-proxy-url-preflight", "response.html")
	reportPath := filepath.Join(exactPublic795FixtureRoot, "moegirl-795-public-html-extraction-v1", "report.json")
	for _, path := range []string{rawPath, reportPath} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			t.Skipf("real fixed 795 artifact is unavailable: %s", path)
		} else if err != nil {
			t.Fatal(err)
		}
	}
	return lyricssource.ExactPublicPageTarget{
		MusicID:   795,
		PageURL:   "https://zh.moegirl.org.cn/%E4%BA%BF%E5%B9%B4%E7%88%B1%E6%81%8B",
		PageTitle: "亿年爱恋", JapaneseTitle: "一億年恋してる",
		PageID: 649688, RevisionID: 8500224, FetchedAt: "2026-08-03T14:58:50.501307Z",
		RawHTML: lyricssource.ExactPublicFileBinding{
			Path: rawPath, SizeBytes: 128236,
			SHA256: "7ef2eda347b9e57f0fd2f3d6912bc4183158173b74f2a2e6fa1eb88083bcd6be",
		},
		ExtractionReport: lyricssource.ExactPublicFileBinding{
			Path: reportPath, SizeBytes: 6344,
			SHA256: "b02112ed132ca06293c64ad939020b6196ca77d1f8541b8a2f4e821b0c8d76ec",
		},
	}
}

func exactPublic795Runtime(t *testing.T, target lyricssource.ExactPublicPageTarget) RuntimeConfig {
	t.Helper()
	runtime, err := newExactPublic795Runtime(target)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func newExactPublic795Runtime(target lyricssource.ExactPublicPageTarget) (RuntimeConfig, error) {
	provider, err := lyricssource.RecoveryExactPublicProviderConfig(
		10*time.Second, 10*time.Second, []lyricssource.ExactPublicPageTarget{target},
	)
	if err != nil {
		return RuntimeConfig{}, err
	}
	parser, ok := lyricsextractionplan.RegisteredRecoveryParserVersion(
		lyricsextractionplan.ProviderMoegirlPublicExact,
	)
	if !ok {
		return RuntimeConfig{}, errors.New("exact public parser is not registered")
	}
	versions := lyricsextractionplan.CompiledRecoveryVersions()
	return RuntimeConfig{
		Order: []model.LyricsSourceProvider{lyricssource.ProviderMoegirlPublicExact},
		ProviderMusicIDs: map[model.LyricsSourceProvider][]int{
			lyricssource.ProviderMoegirlPublicExact: {795},
		},
		Providers: []lyricssource.ProviderConfig{provider},
		Authorities: map[model.LyricsSourceProvider][]lyricssource.FixedIndex{
			lyricssource.ProviderMoegirlPublicExact: {},
		},
		Parsers: map[model.LyricsSourceProvider]string{
			lyricssource.ProviderMoegirlPublicExact: parser,
		},
		RecoveryPlanID:     "real-795-exact-public-fixture",
		RecoveryPlanSHA256: strings.Repeat("a", 64),
		PolicyVersion:      versions.ProviderPolicy,
		MaxAttempts:        1, RequestTimeout: time.Second, RetryDelay: 0,
		ProviderResponseBytes: 2 << 20, MaxActualNetworkInFlight: 1, MediaWikiMaxlag: 5,
		LiveCanaryMusicIDs: []int{},
	}, nil
}

func bindTamperedExactPublicReport(
	t *testing.T,
	target *lyricssource.ExactPublicPageTarget,
	body []byte,
) {
	t.Helper()
	root := privateRecoveryTempDir(t)
	path := filepath.Join(root, "tampered-report.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	target.ExtractionReport = lyricssource.ExactPublicFileBinding{
		Path: path, SizeBytes: int64(len(body)), SHA256: digestHex(body),
	}
}
