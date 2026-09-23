package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
)

func TestUniqueCompleteResumeRevalidatesEveryExactFixedCandidateWithoutSearch(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(10, "固定再検証10"),
		completeCatalogFixture(11, "候補なし11"),
		completeCatalogFixture(12, "固定再検証12"),
	})
	candidates := map[int]lyricssource.Candidate{
		10: candidateFor(10, "one"),
		12: candidateFor(12, "one"),
	}
	prior, err := execute(context.Background(), testOptions(path), fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			candidate, ok := candidates[identity.MusicID]
			if !ok {
				return nil, nil
			}
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate, ok := candidates[identity.MusicID]
			if !ok || !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, errors.New("initial fetch did not use the unique candidate")
			}
			return fixedFor(candidate, "initial unique extraction"), nil
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior.UniqueComplete) != 2 || len(prior.Missing) != 1 {
		t.Fatalf("prior summary=%+v unique=%+v missing=%+v", prior.Summary, prior.UniqueComplete, prior.Missing)
	}
	priorMissing := append([]reportItem(nil), prior.Missing...)
	priorPath := filepath.Join(t.TempDir(), "prior.json")
	writeTestReport(t, priorPath, prior)

	opts := testOptions(path)
	opts.ResumeReportPath = priorPath
	opts.ResumeErrorCodes = "none"
	opts.ResumeUniqueComplete = true
	var searchCalls, fetch10, fetch12 atomic.Int32
	merged, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			searchCalls.Add(1)
			return nil, errors.New("unique_complete revalidation must not Search")
		},
		fetchCandidate: func(_ context.Context, identity lyricssource.MusicIdentity, claimed lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			candidate, ok := candidates[identity.MusicID]
			if !ok || !equalSourceCandidate(claimed, candidate) {
				return lyricssource.FixedRevision{}, errors.New("resume did not re-fetch the exact prior page/revision/SHA")
			}
			switch identity.MusicID {
			case 10:
				fetch10.Add(1)
				return lyricssource.FixedRevision{}, lyricssource.ErrAmbiguous
			case 12:
				fetch12.Add(1)
				return fixedFor(candidate, "still unique after parser update"), nil
			default:
				return lyricssource.FixedRevision{}, errors.New("non-unique bucket reached fixed revalidation")
			}
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls.Load() != 0 || fetch10.Load() != 1 || fetch12.Load() != 1 {
		t.Fatalf("search=%d fetch10=%d fetch12=%d", searchCalls.Load(), fetch10.Load(), fetch12.Load())
	}
	if len(merged.UniqueComplete) != 1 || merged.UniqueComplete[0].MusicID != 12 ||
		len(merged.Incomplete) != 1 || merged.Incomplete[0].MusicID != 10 ||
		merged.Incomplete[0].ErrorCode != "ambiguous_source" || merged.Incomplete[0].Candidate == nil {
		t.Fatalf("merged summary=%+v unique=%+v incomplete=%+v", merged.Summary, merged.UniqueComplete, merged.Incomplete)
	}
	if !reportJSONEqual(merged.Missing, priorMissing) {
		t.Fatalf("unselected missing bucket changed: got=%+v want=%+v", merged.Missing, priorMissing)
	}
}

func TestUniqueCompleteResumeRequiresPriorReportBeforeCatalogOrSource(t *testing.T) {
	opts := testOptions("unused")
	opts.ResumeUniqueComplete = true
	var sourceCalls atomic.Int32
	_, err := executeWithCatalogLoader(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			sourceCalls.Add(1)
			return lyricssource.FixedRevision{}, nil
		},
	}, time.Now, func(context.Context, string) ([]catalogItem, error) {
		t.Fatal("catalog loader must not run without the required prior report")
		return nil, nil
	})
	if err == nil || err.Error() != "-resume-unique-complete requires -resume-report" {
		t.Fatalf("error=%v", err)
	}
	if sourceCalls.Load() != 0 {
		t.Fatalf("source calls=%d, want zero", sourceCalls.Load())
	}
}
