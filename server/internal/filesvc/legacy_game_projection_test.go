package filesvc

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"testing"

	"moesekai/server/internal/store"
)

//go:embed legacy_game_projection_550_v1.json
var legacyGameProjection550V1 []byte

func TestUpconvertLegacyPublication550RestoresGameVersion(t *testing.T) {
	body := legacyGameProjection550V1
	converted, versions, err := upconvertLegacyPublication(550, body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(versions, []string{"full", "game"}) {
		t.Fatalf("available versions=%v", versions)
	}
	detail, err := store.DecodePublicLyricsV3Detail(converted)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Version != 3 || detail.MusicID != 550 || detail.Revision != 5 || detail.State != store.PublicLyricsStateComplete {
		t.Fatalf("detail envelope=%+v", detail)
	}
	if len(detail.Renditions) != 1 {
		t.Fatalf("renditions=%d", len(detail.Renditions))
	}
	rendition := detail.Renditions[0]
	if rendition.Key != "sekai" || !reflect.DeepEqual(rendition.AvailableVersions, []string{"full", "game"}) {
		t.Fatalf("rendition header=%+v", rendition)
	}
	if rendition.Full == nil || rendition.Game == nil {
		t.Fatal("missing Full or Game side")
	}
	if len(rendition.Full.Lines) != 57 {
		t.Fatalf("full lines=%d", len(rendition.Full.Lines))
	}
	if len(rendition.Game.Lines) != 37 {
		t.Fatalf("game lines=%d", len(rendition.Game.Lines))
	}
	if rendition.Relation.Kind != "exact_projection" || len(rendition.Relation.LineIDs) != 37 {
		t.Fatalf("relation=%+v", rendition.Relation)
	}
	if rendition.TranslationCredits == nil || rendition.TranslationCredits.Translation != "雪莹ちゃん" {
		t.Fatalf("credits=%+v", rendition.TranslationCredits)
	}
	if rendition.Full.Lines[0].Chinese == "" || rendition.Game.Lines[0].Chinese == "" {
		t.Fatal("missing projected translations")
	}
	if rendition.Game.Lines[0].Japanese != rendition.Full.Lines[0].Japanese {
		t.Fatalf("game/full japanese mismatch: %q vs %q", rendition.Game.Lines[0].Japanese, rendition.Full.Lines[0].Japanese)
	}
}

func TestUpconvertLegacyPublicationSkipsUnknownSongs(t *testing.T) {
	converted, versions, err := upconvertLegacyPublication(1, []byte(`{"version":1,"musicId":1}`))
	if err != nil || converted != nil || versions != nil {
		t.Fatalf("converted=%q versions=%v err=%v", converted, versions, err)
	}
}

func TestApplyLegacyGameProjectionsUpdatesIndex(t *testing.T) {
	body := legacyGameProjection550V1
	projected := map[string][]byte{"translation/lyrics/music_550.json": body}
	songs := []store.PublicLyricsIndexSong{{
		MusicID: 550, Revision: 5, State: store.PublicLyricsStateComplete,
		AvailableVersions: []string{"full"},
	}}
	provenance := map[int]SongProvenance{550: {
		MusicID: 550, Source: string(sourceDBPublication), Revision: 5,
		State: string(store.PublicLyricsStateComplete), AvailableVersions: []string{"full"}, HasDetail: true,
	}}
	applyLegacyGameProjections(projected, songs, provenance, map[int]bool{550: true})
	if !reflect.DeepEqual(songs[0].AvailableVersions, []string{"full", "game"}) {
		t.Fatalf("index versions=%v", songs[0].AvailableVersions)
	}
	if !reflect.DeepEqual(provenance[550].AvailableVersions, []string{"full", "game"}) {
		t.Fatalf("provenance versions=%v", provenance[550].AvailableVersions)
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(projected["translation/lyrics/music_550.json"], &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 3 {
		t.Fatalf("projected version=%d", envelope.Version)
	}
}
