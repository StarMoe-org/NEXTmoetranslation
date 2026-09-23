package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
)

func diagnosticsForMissingReason(reason string) lyricssource.SearchDiagnostics {
	switch reason {
	case "no_search_hits":
		return lyricssource.SearchDiagnostics{}
	case "title_mismatch":
		return lyricssource.SearchDiagnostics{SearchHits: 1, TitleMismatch: 1}
	case "credit_mismatch":
		return lyricssource.SearchDiagnostics{SearchHits: 1, CreditMismatch: 1, LyricistCreditMismatch: 1}
	case "missing_song_signal":
		return lyricssource.SearchDiagnostics{SearchHits: 1, SignalMismatch: 1}
	case "restricted":
		return lyricssource.SearchDiagnostics{SearchHits: 1, Restricted: 1, RestrictedTitleMatch: 1}
	default:
		panic("unsupported test missing reason: " + reason)
	}
}

func reportItemsByID(items []reportItem) map[int]reportItem {
	indexed := make(map[int]reportItem, len(items))
	for _, item := range items {
		indexed[item.MusicID] = item
	}
	return indexed
}

func reportJSONEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func TestSelectedResumeMissingReasonsStrictAllowlist(t *testing.T) {
	selected, err := selectedResumeMissingReasons(options{
		ResumeReportPath:     "prior.json",
		ResumeMissingReasons: "no_search_hits,title_mismatch,credit_mismatch,missing_song_signal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != len(safeResumeMissingReasons) {
		t.Fatalf("selected missing reasons=%v", selected)
	}
	for reason := range safeResumeMissingReasons {
		if _, ok := selected[reason]; !ok {
			t.Fatalf("safe missing reason %q was not selected", reason)
		}
	}

	for _, value := range []string{"", "none"} {
		selected, err := selectedResumeMissingReasons(options{ResumeReportPath: "prior.json", ResumeMissingReasons: value})
		if err != nil {
			t.Fatalf("disabled value %q: %v", value, err)
		}
		if len(selected) != 0 {
			t.Fatalf("disabled value %q selected=%v", value, selected)
		}
	}

	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "restricted", want: "unsafe or unsupported"},
		{value: "ambiguous_source", want: "unsafe or unsupported"},
		{value: "missing_lyrics", want: "unsafe or unsupported"},
		{value: "rate_limited", want: "unsafe or unsupported"},
		{value: "unknown_reason", want: "unsafe or unsupported"},
		{value: "none,no_search_hits", want: "must not be combined"},
		{value: "no_search_hits,none", want: "must not be combined"},
		{value: "title_mismatch,title_mismatch", want: "duplicate"},
		{value: "title_mismatch,", want: "without whitespace"},
		{value: ",title_mismatch", want: "without whitespace"},
		{value: "title_mismatch,,credit_mismatch", want: "without whitespace"},
		{value: "title_mismatch, credit_mismatch", want: "without whitespace"},
	} {
		t.Run(test.value, func(t *testing.T) {
			_, err := selectedResumeMissingReasons(options{ResumeReportPath: "prior.json", ResumeMissingReasons: test.value})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("value=%q err=%v, want %q", test.value, err, test.want)
			}
		})
	}

	for _, value := range []string{"title_mismatch", "none"} {
		if _, err := selectedResumeMissingReasons(options{ResumeMissingReasons: value}); err == nil || !strings.Contains(err.Error(), "requires -resume-report") {
			t.Fatalf("value=%q without report error=%v", value, err)
		}
	}
}

func TestUnsafeMissingResumeReasonFailsBeforeCatalogOrSourceWork(t *testing.T) {
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, report{})
	for _, reason := range []string{"restricted", "unknown_reason", "missing_lyrics", "rate_limited"} {
		t.Run(reason, func(t *testing.T) {
			loaderCalled := false
			sourceCalled := false
			opts := testOptions("unused")
			opts.ResumeReportPath = priorPath
			opts.ResumeMissingReasons = reason
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

func TestMissingResumeRetriesOnlySelectedReasonsAndReclassifies(t *testing.T) {
	reasons := map[int]string{
		1: "no_search_hits",
		2: "title_mismatch",
		3: "credit_mismatch",
		4: "missing_song_signal",
		5: "restricted",
	}
	records := make([]catalogFixture, 0, len(reasons))
	for musicID := 1; musicID <= len(reasons); musicID++ {
		records = append(records, completeCatalogFixture(musicID, fmt.Sprintf("missing-%d", musicID)))
	}
	path := writeV18Catalog(t, records)
	prior, err := execute(context.Background(), testOptions(path), diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			return nil, diagnosticsForMissingReason(reasons[identity.MusicID]), nil
		},
	}, func() time.Time { return time.Unix(100, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Missing) != len(records) {
		t.Fatalf("prior summary=%+v", prior.Summary)
	}
	priorByID := reportItemsByID(prior.Missing)
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)
	priorBytes, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "none"
	opts.ResumeMissingReasons = "title_mismatch,credit_mismatch"
	var retriedMu sync.Mutex
	retriedIDs := []int{}
	var fetchCalls atomic.Int32
	merged, err := execute(context.Background(), opts, diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			retriedMu.Lock()
			retriedIDs = append(retriedIDs, identity.MusicID)
			retriedMu.Unlock()
			switch identity.MusicID {
			case 2:
				return []lyricssource.Candidate{candidateFor(2, "one")}, lyricssource.SearchDiagnostics{SearchHits: 1, Verified: 1}, nil
			case 3:
				return nil, diagnosticsForMissingReason("no_search_hits"), nil
			default:
				return nil, lyricssource.SearchDiagnostics{}, fmt.Errorf("unexpected missing resume search for music %d", identity.MusicID)
			}
		},
		fakeSourceClient: fakeSourceClient{
			fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				fetchCalls.Add(1)
				candidate := candidateFor(identity.MusicID, "one")
				return fixedFor(candidate, "reclassified"), nil
			},
		},
	}, func() time.Time { return time.Unix(200, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	sort.Ints(retriedIDs)
	if !equalMusicIDs(retriedIDs, []int{2, 3}) || fetchCalls.Load() != 1 {
		t.Fatalf("retried=%v fetchCalls=%d", retriedIDs, fetchCalls.Load())
	}
	if reportItemCount(merged) != len(records) || len(merged.UniqueComplete) != 1 || merged.UniqueComplete[0].MusicID != 2 {
		t.Fatalf("merged summary=%+v unique=%+v", merged.Summary, merged.UniqueComplete)
	}
	mergedMissingByID := reportItemsByID(merged.Missing)
	if got := mergedMissingByID[3].ReasonCode; got != "no_search_hits" {
		t.Fatalf("reclassified missing reason=%q", got)
	}
	for _, musicID := range []int{1, 4, 5} {
		if !reportJSONEqual(mergedMissingByID[musicID], priorByID[musicID]) {
			t.Fatalf("unselected missing music %d changed: got=%+v prior=%+v", musicID, mergedMissingByID[musicID], priorByID[musicID])
		}
	}
	currentPriorBytes, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priorBytes, currentPriorBytes) {
		t.Fatal("missing resume modified the input report")
	}
}

func TestMissingResumeIsDisabledByDefault(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "default-off-one"),
		completeCatalogFixture(2, "default-off-two"),
	})
	prior, err := execute(context.Background(), testOptions(path), diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			if identity.MusicID == 1 {
				return nil, diagnosticsForMissingReason("title_mismatch"), nil
			}
			return nil, diagnosticsForMissingReason("credit_mismatch"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	for _, disabledValue := range []string{"", "none"} {
		t.Run(fmt.Sprintf("value-%q", disabledValue), func(t *testing.T) {
			var sourceCalls atomic.Int32
			opts := testOptions(path)
			opts.ResumeReportPath = priorPath
			opts.ResumeErrorCodes = "none"
			opts.ResumeMissingReasons = disabledValue
			merged, err := execute(context.Background(), opts, fakeSourceClient{
				search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
					sourceCalls.Add(1)
					return nil, errors.New("disabled missing resume must not search")
				},
				fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
					sourceCalls.Add(1)
					return lyricssource.FixedRevision{}, errors.New("disabled missing resume must not fetch")
				},
			}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if sourceCalls.Load() != 0 || reportItemCount(merged) != 2 || !reportJSONEqual(merged.Missing, prior.Missing) {
				t.Fatalf("sourceCalls=%d merged=%+v", sourceCalls.Load(), merged.Summary)
			}
		})
	}
}

func TestMissingResumeMergesComplete701ReportWithoutMutation(t *testing.T) {
	const selectedMusicID = 517
	records := make([]catalogFixture, 701)
	for index := range records {
		records[index] = completeCatalogFixture(index+1, fmt.Sprintf("missing-701-%03d", index+1))
	}
	path := writeV18Catalog(t, records)
	beforeDatabase, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	initialOptions := testOptions(path)
	initialOptions.Concurrency = 8
	prior, err := execute(context.Background(), initialOptions, diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			if identity.MusicID == selectedMusicID {
				return nil, diagnosticsForMissingReason("title_mismatch"), nil
			}
			return nil, diagnosticsForMissingReason("no_search_hits"), nil
		},
	}, func() time.Time { return time.Unix(300, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if prior.CatalogCount != 701 || reportItemCount(prior) != 701 || len(prior.Missing) != 701 {
		t.Fatalf("prior summary=%+v", prior.Summary)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)
	beforePrior, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "none"
	opts.ResumeMissingReasons = "title_mismatch"
	var searchCalls, fetchCalls atomic.Int32
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			searchCalls.Add(1)
			if identity.MusicID != selectedMusicID {
				return nil, fmt.Errorf("unexpected 701 resume search for music %d", identity.MusicID)
			}
			return []lyricssource.Candidate{candidateFor(identity.MusicID, "one")}, nil
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			fetchCalls.Add(1)
			candidate := candidateFor(identity.MusicID, "one")
			return fixedFor(candidate, "complete"), nil
		},
	}, func() time.Time { return time.Unix(400, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls.Load() != 1 || fetchCalls.Load() != 1 || merged.CatalogCount != 701 || reportItemCount(merged) != 701 ||
		len(merged.UniqueComplete) != 1 || merged.UniqueComplete[0].MusicID != selectedMusicID || len(merged.Missing) != 700 {
		t.Fatalf("search=%d fetch=%d summary=%+v", searchCalls.Load(), fetchCalls.Load(), merged.Summary)
	}
	priorMissingByID := reportItemsByID(prior.Missing)
	mergedMissingByID := reportItemsByID(merged.Missing)
	if !reportJSONEqual(mergedMissingByID[1], priorMissingByID[1]) {
		t.Fatalf("unselected 701 item changed: got=%+v prior=%+v", mergedMissingByID[1], priorMissingByID[1])
	}

	output := filepath.Join(t.TempDir(), "merged.json")
	if err := writeReport(output, path, merged, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(output, path, merged, io.Discard); err == nil {
		t.Fatal("missing resume report overwrote an existing output")
	}
	afterDatabase, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterPrior, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeDatabase, afterDatabase) {
		t.Fatal("missing resume modified the catalog database")
	}
	if !bytes.Equal(beforePrior, afterPrior) {
		t.Fatal("missing resume modified the prior report")
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing resume left SQLite sidecar %s: %v", suffix, err)
		}
	}
}

func TestMissingResumeCombinesWithSelectedErrorAndFixedIncompleteWithoutDuplicateWork(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "combined-missing"),
		completeCatalogFixture(2, "combined-error"),
		completeCatalogFixture(3, "combined-incomplete"),
		completeCatalogFixture(4, "combined-complete"),
	})
	prior, err := execute(context.Background(), testOptions(path), diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			switch identity.MusicID {
			case 1:
				return nil, diagnosticsForMissingReason("title_mismatch"), nil
			case 2:
				return nil, lyricssource.SearchDiagnostics{}, &lyricssource.HTTPError{StatusCode: 429}
			case 3, 4:
				return []lyricssource.Candidate{candidateFor(identity.MusicID, "one")}, lyricssource.SearchDiagnostics{SearchHits: 1, Verified: 1}, nil
			default:
				return nil, lyricssource.SearchDiagnostics{}, errors.New("unexpected initial identity")
			}
		},
		fakeSourceClient: fakeSourceClient{
			fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				candidate := candidateFor(identity.MusicID, "one")
				if identity.MusicID == 3 {
					return lyricssource.FixedRevision{}, lyricssource.ErrAmbiguous
				}
				return fixedFor(candidate, "initial complete"), nil
			},
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.Missing) != 1 || len(prior.Error) != 1 || len(prior.Incomplete) != 1 || len(prior.UniqueComplete) != 1 {
		t.Fatalf("prior summary=%+v", prior.Summary)
	}
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "rate_limited"
	opts.ResumeMissingReasons = "title_mismatch"
	opts.ResumeIncompleteCodes = "ambiguous_source"
	var callsMu sync.Mutex
	searchCalls := map[int]int{}
	fetchCalls := map[int]int{}
	merged, err := execute(context.Background(), opts, diagnosticFakeSourceClient{
		diagnosticSearch: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			callsMu.Lock()
			searchCalls[identity.MusicID]++
			callsMu.Unlock()
			if identity.MusicID != 1 && identity.MusicID != 2 {
				return nil, lyricssource.SearchDiagnostics{}, fmt.Errorf("fixed incomplete resume searched music %d", identity.MusicID)
			}
			return nil, diagnosticsForMissingReason("no_search_hits"), nil
		},
		fakeSourceClient: fakeSourceClient{
			fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				callsMu.Lock()
				fetchCalls[identity.MusicID]++
				callsMu.Unlock()
				if identity.MusicID != 3 {
					return lyricssource.FixedRevision{}, fmt.Errorf("unexpected combined fetch for music %d", identity.MusicID)
				}
				candidate := candidateFor(identity.MusicID, "one")
				return fixedFor(candidate, "fixed resume complete"), nil
			},
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(searchCalls, map[int]int{1: 1, 2: 1}) || !reflect.DeepEqual(fetchCalls, map[int]int{3: 1}) {
		t.Fatalf("search calls=%v fetch calls=%v", searchCalls, fetchCalls)
	}
	if reportItemCount(merged) != 4 || len(merged.Missing) != 2 || len(merged.UniqueComplete) != 2 ||
		len(merged.Error) != 0 || len(merged.Incomplete) != 0 {
		t.Fatalf("merged summary=%+v", merged.Summary)
	}
	missingIDs := []int{merged.Missing[0].MusicID, merged.Missing[1].MusicID}
	uniqueIDs := []int{merged.UniqueComplete[0].MusicID, merged.UniqueComplete[1].MusicID}
	if !equalMusicIDs(missingIDs, []int{1, 2}) || !equalMusicIDs(uniqueIDs, []int{3, 4}) {
		t.Fatalf("missing IDs=%v unique IDs=%v", missingIDs, uniqueIDs)
	}
}

func TestMissingResumeRejectsMalformedPriorMissingBeforeSourceRequests(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "malformed-missing")})
	prior, err := execute(context.Background(), testOptions(path), diagnosticFakeSourceClient{
		diagnosticSearch: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, lyricssource.SearchDiagnostics, error) {
			return nil, diagnosticsForMissingReason("title_mismatch"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	prior.Missing[0].ReasonCode = "credit_mismatch"
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "none"
	opts.ResumeMissingReasons = "credit_mismatch"
	var sourceCalls atomic.Int32
	_, err = execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, nil
		},
	}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "inconsistent missing diagnostics") || sourceCalls.Load() != 0 {
		t.Fatalf("err=%v sourceCalls=%d", err, sourceCalls.Load())
	}
}

func TestMissingResumeHelpAndOptionsValidation(t *testing.T) {
	var help bytes.Buffer
	err := run([]string{"-h"}, &help)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error=%v", err)
	}
	for _, text := range []string{"-resume-missing-reasons", "no_search_hits,title_mismatch,credit_mismatch,missing_song_signal", "default off"} {
		if !strings.Contains(help.String(), text) {
			t.Fatalf("help missing %q: %s", text, help.String())
		}
	}

	path := writeV18Catalog(t, []catalogFixture{})
	for _, value := range []string{"title_mismatch", "none"} {
		opts := testOptions(path)
		opts.OutputPath = filepath.Join(t.TempDir(), "report.json")
		opts.ResumeMissingReasons = value
		if err := validateOptions(opts); err == nil || !strings.Contains(err.Error(), "requires -resume-report") {
			t.Fatalf("value=%q validation error=%v", value, err)
		}
	}
}
