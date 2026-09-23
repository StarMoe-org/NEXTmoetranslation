package lyricscompose

import (
	"encoding/json"
	"testing"
)

func TestSourceCoverageHasExactlySixClosedProviderCategories(t *testing.T) {
	categories := []SourceCoverageCategory{
		SourceCoverageFandomOnly,
		SourceCoverageMoegirlOnly,
		SourceCoverageBothEqual,
		SourceCoverageBothComplementary,
		SourceCoverageBothConflict,
		SourceCoverageNeitherException,
	}
	var coverage SourceCoverage
	for _, category := range categories {
		if err := coverage.Add(category); err != nil {
			t.Fatal(err)
		}
	}
	counts := coverage.Counts()
	if len(counts) != 6 || coverage.Total() != 6 {
		t.Fatalf("source coverage = %+v counts=%+v", coverage, counts)
	}
	for index, count := range counts {
		if count.Category != categories[index] || count.Count != 1 {
			t.Fatalf("source coverage count %d = %+v, want %q=1", index, count, categories[index])
		}
	}
	if err := coverage.Add("other"); err == nil || coverage.Total() != 6 {
		t.Fatalf("unknown source category changed coverage: coverage=%+v err=%v", coverage, err)
	}

	body, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]int
	if err := json.Unmarshal(body, &encoded); err != nil || len(encoded) != 6 {
		t.Fatalf("encoded source coverage = %s err=%v", body, err)
	}
	for _, category := range categories {
		if encoded[string(category)] != 1 {
			t.Fatalf("encoded source coverage %q = %d, want 1", category, encoded[string(category)])
		}
	}
}
