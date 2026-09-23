package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
)

func TestSelectedResumeErrorCodesAllowsExplicitNoneOnly(t *testing.T) {
	selected, err := selectedResumeErrorCodes(options{ResumeReportPath: "prior.json", ResumeErrorCodes: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 0 {
		t.Fatalf("selected error retry codes=%v, want empty", selected)
	}

	selected, err = selectedResumeErrorCodes(options{ResumeReportPath: "prior.json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := selected["rate_limited"]; !ok || len(selected) != 1 {
		t.Fatalf("default selected error retry codes=%v, want rate_limited", selected)
	}

	for _, value := range []string{"none,rate_limited", "rate_limited,none"} {
		if _, err := selectedResumeErrorCodes(options{ResumeReportPath: "prior.json", ResumeErrorCodes: value}); err == nil || !strings.Contains(err.Error(), "must not be combined") {
			t.Fatalf("mixed none value %q error=%v", value, err)
		}
	}
}

func TestTransientFetchErrorResumeRefetchesPinnedCandidateWithoutSearch(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(17, "固定一時障害")})
	candidate := candidateFor(17, "one")
	diagnostics := lyricssource.SearchDiagnostics{SearchHits: 2, TitleMismatch: 1, Verified: 1}
	prior, err := execute(context.Background(), testOptions(path), diagnosticFakeSourceClient{
		diagnosticSearch: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			return []lyricssource.Candidate{candidate}, diagnostics, nil
		},
		fakeSourceClient: fakeSourceClient{
			fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				return lyricssource.FixedRevision{}, &lyricssource.HTTPError{StatusCode: 503}
			},
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Error) != 1 || prior.Error[0].ErrorCode != "source_unavailable" || prior.Error[0].Candidate == nil ||
		prior.Error[0].FetchAttempts != testOptions(path).MaxAttempts || prior.Error[0].SearchDiagnostics == nil {
		t.Fatalf("prior transient fetch error=%+v", prior.Error)
	}
	priorItem := prior.Error[0]
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "source_unavailable"
	var searchCalls, fetchCalls atomic.Int32
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			searchCalls.Add(1)
			return nil, errors.New("transient fetch resume must not Search")
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			if identity.MusicID != 17 || !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, errors.New("transient fetch resume did not use exact prior candidate")
			}
			return fixedFor(candidate, "一時障害復旧"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls.Load() != 0 || fetchCalls.Load() != 1 || len(merged.UniqueComplete) != 1 || len(merged.Error) != 0 {
		t.Fatalf("search=%d fetch=%d summary=%+v", searchCalls.Load(), fetchCalls.Load(), merged.Summary)
	}
	resumed := merged.UniqueComplete[0]
	if resumed.SearchAttempts != priorItem.SearchAttempts || resumed.FetchAttempts != 1 ||
		!reflect.DeepEqual(resumed.SearchDiagnostics, priorItem.SearchDiagnostics) {
		t.Fatalf("resumed audit=%+v prior=%+v", resumed, priorItem)
	}
}

func TestAmbiguousSourceResumeRefetchesOnlyExactFixedCandidates(t *testing.T) {
	fixedIDs := []int{44, 47, 541}
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(44, "固定曖昧44"),
		completeCatalogFixture(47, "固定曖昧47"),
		completeCatalogFixture(541, "固定曖昧541"),
		completeCatalogFixture(600, "検索候補曖昧"),
		completeCatalogFixture(601, "候補なし"),
	})
	candidateByID := map[int]lyricssource.Candidate{
		44: candidateFor(44, "one"), 47: candidateFor(47, "one"), 541: candidateFor(541, "one"),
	}
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			switch identity.MusicID {
			case 44, 47, 541:
				return []lyricssource.Candidate{candidateByID[identity.MusicID]}, nil
			case 600:
				return []lyricssource.Candidate{candidateFor(600, "a"), candidateFor(600, "b")}, nil
			case 601:
				return nil, nil
			default:
				return nil, errors.New("unexpected initial music")
			}
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate, ok := candidateByID[identity.MusicID]
			if !ok || !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, errors.New("initial fetch did not use the unique search candidate")
			}
			return lyricssource.FixedRevision{}, lyricssource.ErrAmbiguous
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Incomplete) != len(fixedIDs) || len(prior.Ambiguous) != 1 || len(prior.Missing) != 1 {
		t.Fatalf("prior summary=%+v incomplete=%+v ambiguous=%+v missing=%+v", prior.Summary, prior.Incomplete, prior.Ambiguous, prior.Missing)
	}
	for _, item := range prior.Incomplete {
		if item.ErrorCode != "ambiguous_source" || item.Candidate == nil {
			t.Fatalf("prior fixed ambiguous item=%+v", item)
		}
	}
	priorAmbiguous := append([]reportItem(nil), prior.Ambiguous...)
	priorMissing := append([]reportItem(nil), prior.Missing...)
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "none"
	opts.ResumeIncompleteCodes = "ambiguous_source"
	var searchCalls, fetch44, fetch47, fetch541 atomic.Int32
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			searchCalls.Add(1)
			return nil, errors.New("fixed ambiguous_source resume must not Search")
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate, ok := candidateByID[identity.MusicID]
			if !ok || !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, errors.New("resume did not re-fetch the exact prior page/revision/SHA")
			}
			switch identity.MusicID {
			case 44:
				fetch44.Add(1)
				return fixedFor(candidate, "44 unique after selector update"), nil
			case 47:
				fetch47.Add(1)
				return lyricssource.FixedRevision{}, lyricssource.ErrAmbiguous
			case 541:
				fetch541.Add(1)
				return fixedFor(candidate, "541 unique after selector update"), nil
			default:
				return lyricssource.FixedRevision{}, errors.New("non-Incomplete bucket reached fixed resume")
			}
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls.Load() != 0 || fetch44.Load() != 1 || fetch47.Load() != 1 || fetch541.Load() != 1 {
		t.Fatalf("search=%d fetch44=%d fetch47=%d fetch541=%d", searchCalls.Load(), fetch44.Load(), fetch47.Load(), fetch541.Load())
	}
	if len(merged.UniqueComplete) != 2 || !equalMusicIDs([]int{merged.UniqueComplete[0].MusicID, merged.UniqueComplete[1].MusicID}, []int{44, 541}) ||
		len(merged.Incomplete) != 1 || merged.Incomplete[0].MusicID != 47 || merged.Incomplete[0].ErrorCode != "ambiguous_source" ||
		merged.Incomplete[0].Candidate == nil {
		t.Fatalf("merged summary=%+v unique=%+v incomplete=%+v", merged.Summary, merged.UniqueComplete, merged.Incomplete)
	}
	if !reportJSONEqual(merged.Ambiguous, priorAmbiguous) || !reportJSONEqual(merged.Missing, priorMissing) {
		t.Fatalf("non-Incomplete buckets changed: ambiguous=%+v missing=%+v", merged.Ambiguous, merged.Missing)
	}
}

func TestAmbiguousSourceResumeRejectsMalformedOrStaleFixedCandidateBeforeSource(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(44, "固定候補検証")})
	candidate := candidateFor(44, "one")
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return lyricssource.FixedRevision{}, lyricssource.ErrAmbiguous
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Incomplete) != 1 || prior.Incomplete[0].Candidate == nil {
		t.Fatalf("prior incomplete=%+v", prior.Incomplete)
	}

	tests := []struct {
		name string
		edit func(*report)
		want string
	}{
		{name: "missing candidate", edit: func(mutated *report) { mutated.Incomplete[0].Candidate = nil }, want: "invalid incomplete item"},
		{name: "malformed candidate", edit: func(mutated *report) {
			copy := *mutated.Incomplete[0].Candidate
			copy.SHA1 = "not-a-canonical-sha1"
			mutated.Incomplete[0].Candidate = &copy
		}, want: "candidate"},
		{name: "catalog fingerprint drift", edit: func(mutated *report) {
			mutated.Incomplete[0].CatalogFingerprint = strings.Repeat("f", 64)
		}, want: "fingerprint"},
		{name: "current target mismatch", edit: func(mutated *report) {
			mutated.Incomplete[0].TargetMusicID++
		}, want: "classification"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := prior
			mutated.Incomplete = append([]reportItem(nil), prior.Incomplete...)
			test.edit(&mutated)
			priorPath := filepath.Join(t.TempDir(), "prior.json")
			writeTestReport(t, priorPath, mutated)

			opts := testOptions(path)
			opts.ResumeReportPath = priorPath
			opts.ResumeErrorCodes = "none"
			opts.ResumeIncompleteCodes = "ambiguous_source"
			var sourceCalls atomic.Int32
			_, err := execute(context.Background(), opts, fakeSourceClient{
				search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
					sourceCalls.Add(1)
					return nil, errors.New("invalid fixed resume reached Search")
				},
				fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
					sourceCalls.Add(1)
					return lyricssource.FixedRevision{}, errors.New("invalid fixed resume reached fetch")
				},
			}, time.Now)
			if err == nil || !strings.Contains(err.Error(), test.want) || sourceCalls.Load() != 0 {
				t.Fatalf("err=%v want=%q sourceCalls=%d", err, test.want, sourceCalls.Load())
			}
		})
	}
}

func TestIncompleteOnlyResumeRetriesFixedCandidateWithoutSearch(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "固定再開"),
		completeCatalogFixture(2, "制限維持"),
	})
	candidate := candidateFor(1, "one")
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			switch identity.MusicID {
			case 1:
				return []lyricssource.Candidate{candidate}, nil
			case 2:
				return nil, &lyricssource.HTTPError{StatusCode: 429}
			default:
				return nil, errors.New("unexpected initial music")
			}
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return lyricssource.FixedRevision{}, lyricssource.ErrMissingLyrics
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Incomplete) != 1 || prior.Incomplete[0].MusicID != 1 || prior.Incomplete[0].ErrorCode != "missing_lyrics" ||
		len(prior.Error) != 1 || prior.Error[0].MusicID != 2 || prior.Error[0].ErrorCode != "rate_limited" {
		t.Fatalf("prior report summary=%+v incomplete=%+v error=%+v", prior.Summary, prior.Incomplete, prior.Error)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "none"
	opts.ResumeIncompleteCodes = "missing_lyrics"
	var searchCalls, fetchCalls atomic.Int32
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			searchCalls.Add(1)
			return nil, errors.New("ordinary search must not run during fixed incomplete resume")
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			if identity.MusicID != 1 || !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, errors.New("fixed resume did not use the prior candidate identity")
			}
			return fixedFor(candidate, "修復済み"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls.Load() != 0 || fetchCalls.Load() != 1 {
		t.Fatalf("search calls=%d fetch calls=%d", searchCalls.Load(), fetchCalls.Load())
	}
	if len(merged.UniqueComplete) != 1 || merged.UniqueComplete[0].MusicID != 1 ||
		len(merged.Error) != 1 || merged.Error[0].MusicID != 2 || merged.Error[0].ErrorCode != "rate_limited" ||
		len(merged.Incomplete) != 0 {
		t.Fatalf("merged report summary=%+v unique=%+v error=%+v", merged.Summary, merged.UniqueComplete, merged.Error)
	}
}

func TestClassifyFixedRevisionRejectsTitleAndCanonicalCategoryDrift(t *testing.T) {
	candidate := candidateFor(1, "one")
	baseItem := reportItem{SearchAttempts: 1, FetchAttempts: 1}

	t.Run("canonical category order is stable", func(t *testing.T) {
		fixed := fixedFor(candidate, "本文")
		fixed.Categories = []string{"Songs", "Lyrics"}
		result := classifyFixedRevision(baseItem, candidate, attemptResult[lyricssource.FixedRevision]{value: fixed, attempts: 1})
		if result.class != "unique_complete" || result.item.ErrorCode != "" {
			t.Fatalf("classification=%+v", result)
		}
	})

	for _, test := range []struct {
		name string
		edit func(*lyricssource.FixedRevision)
	}{
		{name: "page title", edit: func(fixed *lyricssource.FixedRevision) { fixed.PageTitle += " drift" }},
		{name: "categories", edit: func(fixed *lyricssource.FixedRevision) { fixed.Categories = []string{"Lyrics", "Other"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixed := fixedFor(candidate, "本文")
			test.edit(&fixed)
			result := classifyFixedRevision(baseItem, candidate, attemptResult[lyricssource.FixedRevision]{value: fixed, attempts: 1})
			if result.class != "incomplete" || result.item.ErrorCode != "source_identity_drift" {
				t.Fatalf("classification=%+v", result)
			}
		})
	}
}

func TestFixedResumePreservesSearchAuditAndRemainsResumable(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(7, "監査継続")})
	candidate := candidateFor(7, "one")
	var initialSearchCalls atomic.Int32
	prior, err := execute(context.Background(), testOptions(path), diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			if identity.MusicID != 7 {
				return nil, lyricssource.SearchDiagnostics{}, errors.New("unexpected initial identity")
			}
			if initialSearchCalls.Add(1) == 1 {
				return nil, lyricssource.SearchDiagnostics{}, &lyricssource.HTTPError{StatusCode: 429}
			}
			return []lyricssource.Candidate{candidate}, lyricssource.SearchDiagnostics{
				SearchHits: 3, TitleMismatch: 1, CreditMismatch: 1, Verified: 1,
			}, nil
		},
		fakeSourceClient: fakeSourceClient{
			fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				return lyricssource.FixedRevision{}, lyricssource.ErrMissingLyrics
			},
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Incomplete) != 1 || prior.Incomplete[0].SearchAttempts != 2 || prior.Incomplete[0].SearchDiagnostics == nil {
		t.Fatalf("prior incomplete=%+v", prior.Incomplete)
	}
	priorItem := prior.Incomplete[0]
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "none"
	opts.ResumeIncompleteCodes = "missing_lyrics"
	var fetchCalls atomic.Int32
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return nil, errors.New("fixed resume must not search")
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			if identity.MusicID != 7 || identity.JapaneseTitle != "監査継続" ||
				identity.ProducerMetadata != "作詞者 / 作曲者 / 編曲者" || identity.Lyricist != "作詞者" ||
				identity.Composer != "作曲者" || identity.Arranger != "編曲者" {
				return lyricssource.FixedRevision{}, errors.New("catalog source identity was not revalidated")
			}
			if !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, errors.New("exact prior fixed identity and evidence were not forwarded")
			}
			fixed := fixedFor(candidate, "再抽出成功")
			fixed.Categories = []string{"Songs", "Lyrics"}
			return fixed, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if fetchCalls.Load() != 1 || len(merged.UniqueComplete) != 1 {
		t.Fatalf("fetch calls=%d merged=%+v", fetchCalls.Load(), merged.Summary)
	}
	mergedItem := merged.UniqueComplete[0]
	if mergedItem.SearchAttempts != priorItem.SearchAttempts || !reflect.DeepEqual(mergedItem.SearchDiagnostics, priorItem.SearchDiagnostics) {
		t.Fatalf("search audit changed: merged=%+v prior=%+v", mergedItem, priorItem)
	}

	mergedPath := filepath.Join(t.TempDir(), "merged.json")
	if err := writeReport(mergedPath, path, merged, io.Discard); err != nil {
		t.Fatal(err)
	}
	secondOpts := testOptions(path)
	secondOpts.ResumeReportPath = mergedPath
	secondOpts.ResumeErrorCodes = "none"
	secondOpts.ResumeIncompleteCodes = "missing_lyrics"
	resumedAgain, err := execute(context.Background(), secondOpts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return nil, errors.New("complete second resume must not search")
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumedAgain.UniqueComplete) != 1 || resumedAgain.UniqueComplete[0].SearchAttempts != priorItem.SearchAttempts ||
		!reflect.DeepEqual(resumedAgain.UniqueComplete[0].SearchDiagnostics, priorItem.SearchDiagnostics) {
		t.Fatalf("second resume report=%+v", resumedAgain)
	}
}

func TestWriteReportRejectsClassInconsistentItemBeforeEmission(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "書込検証")})
	generated, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return nil, nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	generated.Missing[0].FetchAttempts = 1

	var stdout bytes.Buffer
	if err := writeReport("-", path, generated, &stdout); err == nil || !strings.Contains(err.Error(), "stdout is not supported") {
		t.Fatalf("stdout write error=%v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("report was emitted to stdout: %q", stdout.String())
	}

	outputPath := filepath.Join(t.TempDir(), "invalid.json")
	if err := writeReport(outputPath, path, generated, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid missing item") {
		t.Fatalf("file write error=%v", err)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid report file exists: %v", err)
	}
}
