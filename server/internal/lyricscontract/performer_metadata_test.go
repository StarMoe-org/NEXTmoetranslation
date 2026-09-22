package lyricscontract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

func TestNormalizePersistedPerformerMetadataOmitsUnknownLatinLabelEvenWithAuditedLookingID(t *testing.T) {
	makeFull := func(sourceID string) model.LyricsSourceFull {
		return model.LyricsSourceFull{
			Version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
			Performers: []model.LyricsSourcePerformer{{
				PerformerID: sourceID, Name: "Mikito-P", Color: "#33CCBB",
			}},
			Lines: []model.LyricsSourceFullLine{{
				ID: "full-000001", Text: "ROCK 'N' ROLL",
				Segments: []model.LyricsSourceSegment{{
					Text: "ROCK ", PerformerIDs: []string{sourceID},
					Ruby: []model.LyricsSourceRubySpan{{Text: "ROCK "}},
				}, {
					Text: "'N' ROLL", PerformerIDs: []string{sourceID},
					Ruby: []model.LyricsSourceRubySpan{{Text: "'N' ROLL"}},
				}},
				TrailingPerformerIDs: []string{sourceID},
			}},
		}
	}

	for _, sourceID := range []string{"mikito-p", "provider_mikito_p", "miku", "歌唱者-21"} {
		canonical, err := NormalizePersistedPerformerMetadata(makeFull(sourceID))
		if err != nil {
			t.Fatal(err)
		}
		if len(canonical.Performers) != 0 || len(canonical.Lines) != 1 || len(canonical.Lines[0].Segments) != 1 ||
			canonical.Lines[0].Text != "ROCK 'N' ROLL" || canonical.Lines[0].Segments[0].Text != "ROCK 'N' ROLL" ||
			len(canonical.Lines[0].Segments[0].PerformerIDs) != 0 || len(canonical.Lines[0].TrailingPerformerIDs) != 0 ||
			len(canonical.Lines[0].Segments[0].Ruby) != 2 {
			t.Fatalf("unknown performer segmentation was not safely omitted: %+v", canonical)
		}
		body, err := json.Marshal(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(body)), "mikito") || !strings.Contains(string(body), "ROCK 'N' ROLL") {
			t.Fatal("unknown performer escaped or legitimate English lyric text was removed")
		}
		if err := ValidatePersistedPerformerMetadata(canonical); err != nil {
			t.Fatalf("performer-free canonical Full was rejected: %v", err)
		}
	}

	unmapped := makeFull("miku")
	unmapped.Performers[0].Name = "Hatsune Miku"
	unmapped.Lines[0].Segments[0].PerformerIDs = []string{"external-singer"}
	unmapped.Lines[0].TrailingPerformerIDs = []string{"external-singer"}
	omitted, err := NormalizePersistedPerformerMetadata(unmapped)
	if err != nil || len(omitted.Performers) != 0 || len(omitted.Lines[0].Segments) != 1 ||
		omitted.Lines[0].Segments[0].Text != "ROCK 'N' ROLL" {
		t.Fatalf("contractually safe unmapped references were not omitted: Full=%+v err=%v", omitted, err)
	}

	unsafe := makeFull("miku")
	unsafe.Lines[0].Segments[0].Text = "BROKEN"
	_, err = NormalizePersistedPerformerMetadata(unsafe)
	if !errors.Is(err, ErrUnsafePerformerMetadata) {
		t.Fatalf("unsafe omission error=%v", err)
	}
	lower := strings.ToLower(err.Error())
	for _, prohibited := range []string{"miku", "mikito"} {
		if strings.Contains(lower, prohibited) {
			t.Fatal("unsafe omission sentinel echoed source performer metadata")
		}
	}
}
