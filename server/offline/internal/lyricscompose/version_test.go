package lyricscompose

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"moesekai/server/internal/model"
)

func TestMapGameToFullRequiresUniqueStrictlyMonotonicExactSubsequence(t *testing.T) {
	for name, test := range map[string]struct {
		full    []string
		game    []string
		want    []int
		wantErr error
	}{
		"unique subset": {
			full: []string{"A", "B", "C", "D"}, game: []string{"A", "C"}, want: []int{0, 2},
		},
		"repetition disambiguated by context": {
			full: []string{"A", "B", "A"}, game: []string{"B", "A"}, want: []int{1, 2},
		},
		"identity with repetitions": {
			full: []string{"A", "A"}, game: []string{"A", "A"}, want: []int{0, 1},
		},
		"repetition has multiple mappings": {
			full: []string{"A", "A", "B"}, game: []string{"A", "B"}, wantErr: ErrProjectionAmbiguous,
		},
		"two repeated slots": {
			full: []string{"A", "A", "A"}, game: []string{"A", "A"}, wantErr: ErrProjectionAmbiguous,
		},
		"not a subsequence": {
			full: []string{"A", "B"}, game: []string{"B", "A"}, wantErr: ErrProjectionMissing,
		},
		"no fuzzy normalization": {
			full: []string{"か\u3099く"}, game: []string{"がく"}, wantErr: ErrProjectionMissing,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := MapGameToFull(test.full, test.game)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mapping = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveVersionUsesClosedReasonAndProjectionMatrix(t *testing.T) {
	for name, test := range map[string]struct {
		evidence       VersionEvidence
		wantReason     model.LyricsSourceVersionReasonCode
		wantProjection []int
		wantErr        bool
	}{
		"tagged Full and Game projects": {
			evidence:   VersionEvidence{TaggedFull: []string{"A", "B", "C"}, TaggedGame: []string{"A", "C"}},
			wantReason: model.LyricsSourceVersionReasonTaggedFullAndGame, wantProjection: []int{0, 2},
		},
		"tagged Game only ignores unrelated valid text and does not project": {
			evidence:   VersionEvidence{TaggedGame: []string{"unrelated Game text"}, VocaloidFull: []string{"A", "B", "C"}},
			wantReason: model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid,
		},
		"untagged uncut identity projects": {
			evidence:   VersionEvidence{Untagged: []string{"A", "B"}, VocaloidFull: []string{"A", "B"}},
			wantReason: model.LyricsSourceVersionReasonUntaggedUncutIdentity, wantProjection: []int{0, 1},
		},
		"untagged subset validates but does not project": {
			evidence:   VersionEvidence{Untagged: []string{"A", "C"}, VocaloidFull: []string{"A", "B", "C"}},
			wantReason: model.LyricsSourceVersionReasonUntaggedGameSubset,
		},
		"untagged Full only": {
			evidence:   VersionEvidence{Untagged: []string{"A", "B"}},
			wantReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		},
		"tagged Game only still requires nonempty valid evidence": {
			evidence:   VersionEvidence{TaggedGame: []string{}, VocaloidFull: []string{"A", "B"}},
			wantReason: model.LyricsSourceVersionReasonVersionConflict, wantErr: true,
		},
		"ambiguous repetition preserves exact Full and Game without projection": {
			evidence:   VersionEvidence{TaggedFull: []string{"A", "A", "B"}, TaggedGame: []string{"A", "B"}},
			wantReason: model.LyricsSourceVersionReasonTaggedFullAndGame,
		},
		"unsupported mixture conflicts": {
			evidence:   VersionEvidence{TaggedFull: []string{"A"}},
			wantReason: model.LyricsSourceVersionReasonVersionConflict, wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolution, err := ResolveVersion(test.evidence)
			if resolution.ReasonCode != test.wantReason {
				t.Fatalf("reason = %q, want %q", resolution.ReasonCode, test.wantReason)
			}
			if test.wantErr {
				if !errors.Is(err, ErrVersionConflict) || resolution.Full != nil || resolution.Game != nil || resolution.GameToFull != nil {
					t.Fatalf("conflict did not fail closed: resolution=%+v err=%v", resolution, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resolution.GameToFull, test.wantProjection) {
				t.Fatalf("projection = %v, want %v", resolution.GameToFull, test.wantProjection)
			}
			if test.wantReason == model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid &&
				(resolution.Game != nil || !reflect.DeepEqual(resolution.Full, test.evidence.VocaloidFull)) {
				t.Fatalf("tagged Game-only text was not ignored: resolution=%+v", resolution)
			}
		})
	}
}

func TestVersionCoverageHasExactlySevenModelReasonCategories(t *testing.T) {
	reasons := []model.LyricsSourceVersionReasonCode{
		model.LyricsSourceVersionReasonTaggedFullAndGame,
		model.LyricsSourceVersionReasonTaggedGameOnly,
		model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid,
		model.LyricsSourceVersionReasonUntaggedUncutIdentity,
		model.LyricsSourceVersionReasonUntaggedGameSubset,
		model.LyricsSourceVersionReasonUntaggedFullOnly,
		model.LyricsSourceVersionReasonVersionConflict,
	}
	var coverage Coverage
	for _, reason := range reasons {
		if err := coverage.Add(reason); err != nil {
			t.Fatal(err)
		}
	}
	counts := coverage.Counts()
	if len(counts) != 7 || coverage.Total() != 7 {
		t.Fatalf("coverage = %+v counts=%+v", coverage, counts)
	}
	for index, count := range counts {
		if count.Category != reasons[index] || count.Count != 1 {
			t.Fatalf("coverage count %d = %+v, want %q=1", index, count, reasons[index])
		}
	}
	if err := coverage.Add("other"); err == nil || coverage.Total() != 7 {
		t.Fatalf("unknown reason changed coverage: coverage=%+v err=%v", coverage, err)
	}
	body, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]int
	if err := json.Unmarshal(body, &encoded); err != nil || len(encoded) != 7 {
		t.Fatalf("encoded coverage = %s err=%v", body, err)
	}
	for _, reason := range reasons {
		if encoded[string(reason)] != 1 {
			t.Fatalf("encoded coverage %q = %d, want 1", reason, encoded[string(reason)])
		}
	}
}
