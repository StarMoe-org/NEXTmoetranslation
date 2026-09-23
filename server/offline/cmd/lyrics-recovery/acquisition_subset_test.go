package main

import (
	"reflect"
	"testing"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsextractionplan"
	"moesekai/server/offline/internal/lyricsrecovery"
)

func TestParseAcquisitionMusicIDsRequiresCanonicalStrictOrder(t *testing.T) {
	got, err := parseAcquisitionMusicIDs("488,794")
	if err != nil || !reflect.DeepEqual(got, []int{488, 794}) {
		t.Fatalf("canonical acquisition subset=%v err=%v", got, err)
	}
	for _, value := range []string{"", " 488,794", "488, 794", "0488,794", "794,488", "488,488", "0,488", "488,"} {
		if _, err := parseAcquisitionMusicIDs(value); err == nil {
			t.Fatalf("noncanonical acquisition subset %q was accepted", value)
		}
	}
}

func TestValidateAcquisitionSubsetRequiresPlanBoundSekaipediaOnlyTargets(t *testing.T) {
	plan := lyricsextractionplan.RecoveryPlan{
		Scope: lyricsextractionplan.RecoveryScopeBinding{MusicIDs: []int{2, 488, 794, 795}},
		Providers: lyricsextractionplan.RecoveryProviderConfiguration{Configurations: []lyricsextractionplan.RecoveryProviderPlan{
			{
				Provider: lyricsextractionplan.ProviderSekaipedia,
				SekaipediaTargets: []lyricsextractionplan.RecoverySekaipediaPageTarget{
					{MusicID: 2, PageTitle: "Roki"},
					{MusicID: 488, PageTitle: "Memoria (song)"},
					{MusicID: 794, PageTitle: "De Los Santos"},
				},
			},
		}},
	}
	runtime := lyricsrecovery.RuntimeConfig{
		Order: []model.LyricsSourceProvider{
			lyricssource.ProviderSekaipedia,
			lyricssource.ProviderMoegirlPublicExact,
		},
		ProviderMusicIDs: map[model.LyricsSourceProvider][]int{
			lyricssource.ProviderSekaipedia:         {2, 488, 794},
			lyricssource.ProviderMoegirlPublicExact: {795},
		},
	}
	if err := validateAcquisitionSubset(plan, runtime, []int{488, 794}); err != nil {
		t.Fatal(err)
	}
	for name, musicIDs := range map[string][]int{
		"outside plan":      {488, 796},
		"not ordered":       {794, 488},
		"fallback provider": {795},
		"duplicate":         {488, 488},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAcquisitionSubset(plan, runtime, musicIDs); err == nil {
				t.Fatalf("invalid subset %v was accepted", musicIDs)
			}
		})
	}
	plan.Providers.Configurations[0].SekaipediaTargets = plan.Providers.Configurations[0].SekaipediaTargets[:2]
	if err := validateAcquisitionSubset(plan, runtime, []int{794}); err == nil {
		t.Fatal("subset without an exact Sekaipedia target was accepted")
	}
}

func TestParseOptionsAcquisitionSubsetIsExplicitAndModeLocal(t *testing.T) {
	arguments := rebindOptionArguments(t)
	arguments = replaceOptionValue(arguments, "-mode", "acquisition-subset")
	arguments = removeOptionPair(arguments, "-rebind-source-ledger")
	arguments = removeOptionPair(arguments, "-rebind-source-acquisition-set")
	arguments = append(arguments,
		"-sekaipedia-list-replay-ledger", "/private/tmp/moesekai-subset-list-source",
		"-acquisition-music-ids", "488,794",
	)
	parsed, err := parseOptions(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.mode != "acquisition-subset" || !reflect.DeepEqual(parsed.acquisitionMusicIDs, []int{488, 794}) {
		t.Fatalf("parsed acquisition subset=%+v", parsed)
	}
	if _, err := parseOptions(removeOptionPair(append([]string(nil), arguments...), "-acquisition-music-ids")); err == nil {
		t.Fatal("acquisition-subset without explicit music IDs was accepted")
	}
	if _, err := parseOptions(replaceOptionValue(append([]string(nil), arguments...), "-mode", "acquisition")); err == nil {
		t.Fatal("full acquisition accepted a subset flag")
	}
}
