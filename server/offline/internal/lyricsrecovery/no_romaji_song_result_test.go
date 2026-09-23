package lyricsrecovery

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricscompose"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
)

func TestNoRomajiSongResultNormalizesPerformerValuesWithoutRejectingEnglishLyrics(t *testing.T) {
	result, err := NewSongResult(noRomajiReplayFixture("ichika", "Hoshino Ichika"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Full == nil || len(result.Full.Performers) != 1 ||
		result.Full.Performers[0] != (model.LyricsSourcePerformer{
			PerformerID: "歌唱者-01", Name: "星乃一歌", Color: "#33CCBB",
		}) {
		t.Fatalf("persisted performer=%+v", result.Full)
	}
	for _, line := range result.Full.Lines {
		if len(line.Segments) != 1 ||
			!equalNoRomajiTestStrings(line.Segments[0].PerformerIDs, []string{"歌唱者-01"}) ||
			!equalNoRomajiTestStrings(line.TrailingPerformerIDs, []string{"歌唱者-01"}) {
			t.Fatal("persisted performer references were not remapped")
		}
	}
	if result.Full.Lines[0].Text != "歌う" || result.Full.Lines[0].Segments[0].Ruby[0].Reading != "うた" ||
		result.Full.Lines[1].Text != "Jo-jo-jo-journey" || result.Full.Lines[2].Text != "VOX AC30w" {
		t.Fatal("performer normalization changed Japanese, ruby, English lyrics, or brand text")
	}
	body, err := MarshalSongResult(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Jo-jo-jo-journey", "VOX AC30w"} {
		if !strings.Contains(string(body), required) {
			t.Fatal("legitimate Latin lyric value was removed")
		}
	}
	lower := strings.ToLower(string(body))
	for _, prohibited := range []string{"\"performerid\":\"ichika\"", "hoshino ichika"} {
		if strings.Contains(lower, prohibited) {
			t.Fatal("canonical song result persisted prohibited romanized performer metadata")
		}
	}
}

func TestNoRomajiSongResultKeepsOnlyExplicitLatinPerformerBrands(t *testing.T) {
	for _, test := range []struct {
		name   string
		id     string
		brand  string
		wantID string
	}{
		{name: "MEIKO", id: "meiko", brand: "MEIKO", wantID: "歌唱者-25"},
		{name: "KAITO", id: "kaito", brand: "KAITO", wantID: "歌唱者-26"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := NewSongResult(noRomajiReplayFixture(test.id, test.brand))
			if err != nil {
				t.Fatal(err)
			}
			if result.Full == nil || len(result.Full.Performers) != 1 ||
				result.Full.Performers[0].PerformerID != test.wantID || result.Full.Performers[0].Name != test.brand {
				t.Fatalf("persisted explicit performer brand=%+v", result.Full)
			}
			for _, line := range result.Full.Lines {
				if !equalNoRomajiTestStrings(line.Segments[0].PerformerIDs, []string{test.wantID}) ||
					!equalNoRomajiTestStrings(line.TrailingPerformerIDs, []string{test.wantID}) {
					t.Fatal("explicit performer brand references were not canonicalized")
				}
			}
		})
	}
}

func TestNoRomajiSongResultOmitsUnknownPerformerSegmentationWithoutRemovingEnglishLyrics(t *testing.T) {
	for _, test := range []struct {
		sourceID   string
		sourceName string
	}{
		{sourceID: "mikito-p", sourceName: "Mikito-P"},
		{sourceID: "provider_mikito_p", sourceName: "Mikito-P"},
		{sourceID: "miku", sourceName: "Mikito-P"},
		{sourceID: "歌唱者-21", sourceName: "Mikito-P"},
		{sourceID: "meiko", sourceName: "MEI-KO"},
		{sourceID: "kaito", sourceName: "KAI-TO"},
	} {
		replay := noRomajiReplayFixture(test.sourceID, test.sourceName)
		result, err := NewSongResult(replay)
		if err != nil {
			t.Fatal(err)
		}
		if result.Full == nil || len(result.Full.Performers) != 0 || len(result.Components.PerformerSegmentation) != 0 {
			t.Fatalf("unknown performer legend or evidence escaped: Full=%+v components=%+v", result.Full, result.Components)
		}
		for _, line := range result.Full.Lines {
			if len(line.Segments) != 1 || line.Segments[0].Text != line.Text ||
				len(line.Segments[0].PerformerIDs) != 0 || len(line.TrailingPerformerIDs) != 0 {
				t.Fatal("unknown performer segmentation was not omitted")
			}
		}
		body, err := MarshalSongResult(result)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, strings.ToLower(test.sourceID)) || strings.Contains(lower, strings.ToLower(test.sourceName)) ||
			!strings.Contains(string(body), "Jo-jo-jo-journey") || !strings.Contains(string(body), "VOX AC30w") {
			t.Fatal("canonical song result leaked an unknown performer or removed legitimate English lyric text")
		}
	}
}

func TestNoRomajiSongResultOmissionDropsExclusivePerformerEvidence(t *testing.T) {
	replay := noRomajiReplayFixture("miku", "Mikito-P")
	performerEvidence := replay.Selected[0]
	performerEvidence.AcquisitionID = strings.Repeat("6", 64)
	performerEvidence.EvidenceID = "revision:sekaipedia:200:2000:" + strings.Repeat("7", 64)
	performerEvidence.SHA256 = strings.Repeat("8", 64)
	performerEvidence.EnvelopeSHA256 = strings.Repeat("9", 64)
	replay.Selected = append(replay.Selected, performerEvidence)
	replay.Components.PerformerSegmentation = []lyricscontract.EvidenceRef{performerEvidence}

	result, err := NewSongResult(replay)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Components.PerformerSegmentation) != 0 || len(result.SelectedEvidence) != 1 ||
		result.SelectedEvidence[0] == performerEvidence {
		t.Fatalf("omitted performer evidence remained selected: selected=%+v components=%+v", result.SelectedEvidence, result.Components)
	}
}

func TestNoRomajiSongResultUnsafePerformerOmissionFailsWithValueFreeSentinel(t *testing.T) {
	replay := noRomajiReplayFixture("miku", "Mikito-P")
	replay.Composition.Full.Lines[0].Segments[0].Text = "BROKEN"

	_, err := NewSongResult(replay)
	if !errors.Is(err, lyricscontract.ErrUnsafePerformerMetadata) {
		t.Fatalf("unsafe performer omission error=%v", err)
	}
	lower := strings.ToLower(err.Error())
	for _, prohibited := range []string{"miku", "mikito"} {
		if strings.Contains(lower, prohibited) {
			t.Fatal("unsafe performer omission error echoed source metadata")
		}
	}
}

func TestNoRomajiSongResultRejectsRomanizedPerformerValuesWithoutEcho(t *testing.T) {
	base, err := NewSongResult(noRomajiReplayFixture("ichika", "Hoshino Ichika"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		id   string
		text string
	}{
		{name: "ichika value", id: "ichika", text: "Hoshino Ichika"},
		{name: "miku value", id: "miku", text: "Hatsune Miku"},
	} {
		t.Run(test.name, func(t *testing.T) {
			unsafe := cloneSongResult(base)
			unsafe.Full.Performers[0].PerformerID = test.id
			unsafe.Full.Performers[0].Name = test.text
			for lineIndex := range unsafe.Full.Lines {
				unsafe.Full.Lines[lineIndex].Segments[0].PerformerIDs = []string{test.id}
				unsafe.Full.Lines[lineIndex].TrailingPerformerIDs = []string{test.id}
			}
			unsafe.ResultSHA256 = ""
			digest, err := songResultDigest(unsafe)
			if err != nil {
				t.Fatal(err)
			}
			unsafe.ResultSHA256 = digest
			body, err := json.Marshal(unsafe)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), test.id) || !strings.Contains(string(body), test.text) {
				t.Fatal("negative fixture did not place prohibited metadata in performer values")
			}
			boundaryErrors := map[string]error{
				"validation": ValidateSongResult(unsafe),
				"API marshal": func() error {
					_, marshalErr := MarshalSongResult(unsafe)
					return marshalErr
				}(),
				"file name": func() error {
					_, fileNameErr := SongResultFileName(unsafe)
					return fileNameErr
				}(),
				"root reference": func() error {
					_, rootRefErr := RootSongRef(unsafe)
					return rootRefErr
				}(),
				"persisted decode": func() error {
					_, decodeErr := DecodeSongResult(body)
					return decodeErr
				}(),
			}
			for boundary, boundaryErr := range boundaryErrors {
				if !errors.Is(boundaryErr, lyricscontract.ErrUnsafePerformerMetadata) {
					t.Fatalf("%s unsafe performer error=%v", boundary, boundaryErr)
				}
				lowerError := strings.ToLower(boundaryErr.Error())
				for _, prohibited := range []string{strings.ToLower(test.id), strings.ToLower(test.text)} {
					if strings.Contains(lowerError, prohibited) {
						t.Fatalf("%s error echoed prohibited performer metadata", boundary)
					}
				}
			}
		})
	}
}

func TestNoRomajiSongResultNewBoundaryDoesNotEchoConflictingRomanizedValues(t *testing.T) {
	_, err := NewSongResult(noRomajiReplayFixture("miku", "Hoshino Ichika"))
	if !errors.Is(err, lyricscontract.ErrUnsafePerformerMetadata) {
		t.Fatalf("conflicting romanized performer error=%v", err)
	}
	lowerError := strings.ToLower(err.Error())
	for _, prohibited := range []string{"miku", "hoshino", "ichika"} {
		if strings.Contains(lowerError, prohibited) {
			t.Fatal("new song-result boundary error echoed prohibited performer metadata")
		}
	}
}

func noRomajiReplayFixture(performerID, performerName string) ReplayResult {
	ref := lyricscontract.EvidenceRef{
		Provider:      model.LyricsSourceProviderSekaipedia,
		AcquisitionID: strings.Repeat("1", 64),
		EvidenceID:    "revision:sekaipedia:100:1000:" + strings.Repeat("2", 64),
		SHA256:        strings.Repeat("3", 64), EnvelopeSHA256: strings.Repeat("4", 64),
	}
	full := model.LyricsSourceFull{
		Version: model.LyricsSourceVersion{Kind: "sekai", Label: "SEKAI Version"},
		Performers: []model.LyricsSourcePerformer{{
			PerformerID: performerID, Name: performerName, Color: "#33CCBB",
		}},
		RubyGeneratorVersion: "kagome-ipadic-v1",
		Lines: []model.LyricsSourceFullLine{
			{
				ID: "full-000001", Text: "歌う",
				Segments: []model.LyricsSourceSegment{{
					Text: "歌う", PerformerIDs: []string{performerID},
					Ruby: []model.LyricsSourceRubySpan{{Text: "歌", Reading: "うた"}, {Text: "う"}},
				}},
				TrailingPerformerIDs: []string{performerID},
			},
			{
				ID: "full-000002", Text: "Jo-jo-jo-journey",
				Segments: []model.LyricsSourceSegment{{
					Text: "Jo-jo-jo-journey", PerformerIDs: []string{performerID},
					Ruby: []model.LyricsSourceRubySpan{{Text: "Jo-jo-jo-journey"}},
				}},
				TrailingPerformerIDs: []string{performerID},
			},
			{
				ID: "full-000003", Text: "VOX AC30w",
				Segments: []model.LyricsSourceSegment{{
					Text: "VOX AC30w", PerformerIDs: []string{performerID},
					Ruby: []model.LyricsSourceRubySpan{{Text: "VOX AC30w"}},
				}},
				TrailingPerformerIDs: []string{performerID},
			},
		},
	}
	composition := &lyricscompose.FixedArtifactComposition{
		ReasonCode: model.LyricsSourceVersionReasonUntaggedFullOnly,
		Full:       full,
		Components: lyricscompose.FixedArtifactComponents{
			FullText: "selected-source", PerformerSegmentation: "selected-source",
			Ruby: "selected-source", VersionEvidence: "selected-source",
		},
		SelectedSourceKeys: []string{"selected-source"},
	}
	return ReplayResult{
		MusicID: 42,
		Providers: []ProviderReplay{{Artifact: lyricsoutcomeartifact.Artifact{
			Provider: model.LyricsSourceProviderSekaipedia, OutcomeID: "selected-source",
			ArtifactSHA256: strings.Repeat("5", 64),
		}}},
		Composition: composition,
		Selected:    []lyricscontract.EvidenceRef{ref},
		Components: ComponentEvidence{
			FullText:              []lyricscontract.EvidenceRef{ref},
			PerformerSegmentation: []lyricscontract.EvidenceRef{ref},
			GameProjection:        []lyricscontract.EvidenceRef{},
			Ruby:                  []lyricscontract.EvidenceRef{ref},
			VersionEvidence:       []lyricscontract.EvidenceRef{ref},
		},
	}
}

func equalNoRomajiTestStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
