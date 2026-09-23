package lyricscompose

import "fmt"

// SourceCoverageCategory is the closed provider-composition coverage matrix.
type SourceCoverageCategory string

const (
	SourceCoverageFandomOnly        SourceCoverageCategory = "fandom_only"
	SourceCoverageMoegirlOnly       SourceCoverageCategory = "moegirl_only"
	SourceCoverageBothEqual         SourceCoverageCategory = "both_equal"
	SourceCoverageBothComplementary SourceCoverageCategory = "both_complementary"
	SourceCoverageBothConflict      SourceCoverageCategory = "both_conflict"
	SourceCoverageNeitherException  SourceCoverageCategory = "neither_exception"
)

// SourceCoverage classifies each catalog target exactly once by provider
// availability and safe composition outcome.
type SourceCoverage struct {
	FandomOnly        int `json:"fandom_only"`
	MoegirlOnly       int `json:"moegirl_only"`
	BothEqual         int `json:"both_equal"`
	BothComplementary int `json:"both_complementary"`
	BothConflict      int `json:"both_conflict"`
	NeitherException  int `json:"neither_exception"`
}

type SourceCoverageCount struct {
	Category SourceCoverageCategory
	Count    int
}

func (coverage *SourceCoverage) Add(category SourceCoverageCategory) error {
	if coverage == nil {
		return fmt.Errorf("nil lyrics source coverage")
	}
	switch category {
	case SourceCoverageFandomOnly:
		coverage.FandomOnly++
	case SourceCoverageMoegirlOnly:
		coverage.MoegirlOnly++
	case SourceCoverageBothEqual:
		coverage.BothEqual++
	case SourceCoverageBothComplementary:
		coverage.BothComplementary++
	case SourceCoverageBothConflict:
		coverage.BothConflict++
	case SourceCoverageNeitherException:
		coverage.NeitherException++
	default:
		return fmt.Errorf("unknown lyrics source coverage category %q", category)
	}
	return nil
}

func (coverage SourceCoverage) Counts() []SourceCoverageCount {
	return []SourceCoverageCount{
		{Category: SourceCoverageFandomOnly, Count: coverage.FandomOnly},
		{Category: SourceCoverageMoegirlOnly, Count: coverage.MoegirlOnly},
		{Category: SourceCoverageBothEqual, Count: coverage.BothEqual},
		{Category: SourceCoverageBothComplementary, Count: coverage.BothComplementary},
		{Category: SourceCoverageBothConflict, Count: coverage.BothConflict},
		{Category: SourceCoverageNeitherException, Count: coverage.NeitherException},
	}
}

func (coverage SourceCoverage) Total() int {
	return coverage.FandomOnly + coverage.MoegirlOnly + coverage.BothEqual +
		coverage.BothComplementary + coverage.BothConflict + coverage.NeitherException
}
