package lyricsstaging

import (
	"fmt"
	"strings"
	"testing"
)

func TestPreflightCatalogCountAcceptsHistorical701PlusThreeSongDelta(t *testing.T) {
	const historicalBaseline = 701
	const newSongDelta = 3
	items := make([]PreflightItem, historicalBaseline+newSongDelta)
	for index := range items {
		items[index] = PreflightItem{
			MusicID: index + 1, JapaneseTitle: fmt.Sprintf("catalog-%03d", index+1),
			CatalogFingerprint: strings.Repeat(fmt.Sprintf("%x", (index%15)+1), 64),
			TargetMusicID:      index + 1, AssociationMusicIDs: []int{},
		}
	}
	report := PreflightReport{
		SchemaVersion: PreflightSchemaVersion, GeneratedAt: "2026-07-31T00:00:00Z",
		CatalogSchemaVersion: CatalogSchemaVersion, CatalogCount: len(items),
		Summary: PreflightSummary{CatalogReview: len(items)}, CatalogReview: items,
		GameSizeEvidence: []PreflightItem{}, UniqueComplete: []PreflightItem{}, Ambiguous: []PreflightItem{},
		Missing: []PreflightItem{}, Incomplete: []PreflightItem{}, Error: []PreflightItem{},
	}
	if err := ValidatePreflight(report); err != nil {
		t.Fatalf("dynamic %d-song catalog: %v", len(items), err)
	}
}
