package lyricsperformers

import (
	"regexp"
	"testing"
)

func TestAuditedExternalRegistryIsStableAndClosed(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("audited external registry is empty, want > 0 performers")
	}
	colorPattern := regexp.MustCompile(`^#[0-9A-F]{6}$`)
	seenNumeric := map[int]bool{}
	seenSource := map[string]bool{}
	for _, performer := range all {
		if performer.NumericID < 1001 {
			t.Fatalf("performer %q numeric ID=%d, want >= 1001", performer.SourceID, performer.NumericID)
		}
		if performer.SourceID == "" {
			t.Fatalf("performer has empty SourceID: %+v", performer)
		}
		if performer.Name == "" {
			t.Fatalf("performer %q has empty Name: %+v", performer.SourceID, performer)
		}
		if performer.Color != "" && !colorPattern.MatchString(performer.Color) {
			t.Fatalf("performer %q has invalid hex color %q (want #RRGGBB): %+v", performer.SourceID, performer.Color, performer)
		}
		if seenNumeric[performer.NumericID] {
			t.Fatalf("duplicate numeric ID %d for performer=%+v", performer.NumericID, performer)
		}
		if seenSource[performer.SourceID] {
			t.Fatalf("duplicate source ID %q for performer=%+v", performer.SourceID, performer)
		}
		seenNumeric[performer.NumericID] = true
		seenSource[performer.SourceID] = true
		for _, alias := range append([]string{performer.SourceID, performer.Name}, performer.Aliases...) {
			if alias == "" {
				t.Fatalf("performer %q has empty alias", performer.SourceID)
			}
			resolved, found := ByAlias(alias)
			if !found || resolved.SourceID != performer.SourceID {
				t.Fatalf("alias lookup for %q=%+v found=%t, want source ID %q", alias, resolved, found, performer.SourceID)
			}
		}
	}
	if _, found := ByAlias("unreviewed external singer"); found {
		t.Fatal("unreviewed external singer entered the closed registry")
	}

	mutated := false
	for _, performer := range all {
		if len(performer.Aliases) > 0 {
			originalAlias := performer.Aliases[0]
			performer.Aliases[0] = "mutated"
			if resolved, found := ByAlias(originalAlias); !found || resolved.SourceID != performer.SourceID {
				t.Fatalf("All returned aliases that mutate the registry for %q", performer.SourceID)
			}
			mutated = true
			break
		}
	}
	if !mutated {
		t.Fatal("no performer in registry has aliases to test detachment")
	}
}
