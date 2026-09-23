package lyricsrecovery

import (
	"reflect"
	"testing"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func TestRuntimeProviderScopesAuthorizeExactlyOneProviderPerSong(t *testing.T) {
	runtime := RuntimeConfig{
		Order: []model.LyricsSourceProvider{
			lyricssource.ProviderSekaipedia,
			lyricssource.ProviderMoegirl,
		},
		ProviderMusicIDs: map[model.LyricsSourceProvider][]int{
			lyricssource.ProviderSekaipedia: {2, 794},
			lyricssource.ProviderMoegirl:    {795},
		},
	}
	for musicID, want := range map[int][]model.LyricsSourceProvider{
		2:   {lyricssource.ProviderSekaipedia},
		794: {lyricssource.ProviderSekaipedia},
		795: {lyricssource.ProviderMoegirl},
	} {
		got, err := runtime.ProviderOrderForMusicID(musicID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("music %d provider order=%v want=%v err=%v", musicID, got, want, err)
		}
	}
	if _, err := runtime.ProviderOrderForMusicID(796); err == nil {
		t.Fatal("unassigned music ID was accepted")
	}

	cloned := cloneRuntimeConfig(runtime)
	cloned.ProviderMusicIDs[lyricssource.ProviderMoegirl][0] = 999
	got, err := runtime.ProviderOrderForMusicID(795)
	if err != nil || !reflect.DeepEqual(got, []model.LyricsSourceProvider{lyricssource.ProviderMoegirl}) {
		t.Fatalf("runtime provider scope clone aliased source: order=%v err=%v", got, err)
	}
}
