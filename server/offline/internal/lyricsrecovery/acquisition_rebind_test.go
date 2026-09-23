package lyricsrecovery

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsacquisition"
)

func TestRebindAcquisitionSetRegeneratesTerminalAndPreservesExactIDs(t *testing.T) {
	scenario := newProviderPrefixScenario(t, nil)
	sourceProviders := cloneProviderSetSlice(scenario.providers)
	sourceProviders[0].Status = lyricsprovideroutcome.StatusStale
	sourceProviders[0].ReasonCode = lyricsprovideroutcome.ReasonRevisionChanged
	sourceProviders[0].Phase = lyricsprovideroutcome.PhaseAcquireAuthority
	sourceProviders[0].Counts = lyricsprovideroutcome.Counts{
		Acquisitions: len(sourceProviders[0].AcquisitionIDs), Stale: 1,
	}
	sourceSet, err := NewAcquisitionSet(
		"historical-rebind-source",
		strings.Repeat("a", 64),
		scenario.runtime.Order,
		[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: sourceProviders}},
	)
	if err != nil {
		t.Fatal(err)
	}

	destination, err := lyricsacquisition.CreateLedger(
		t.Context(), filepath.Join(t.TempDir(), "destination-ledger"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	runtime := cloneRuntimeConfig(scenario.runtime)
	runtime.RecoveryPlanID = "current-rebind-target"
	runtime.RecoveryPlanSHA256 = strings.Repeat("b", 64)

	rebound, err := RebindAcquisitionSet(
		t.Context(), sourceSet, scenario.ledger, destination, runtime,
		[]lyricssource.MusicIdentity{scenario.identity},
	)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := rebound.OrderedProviders(scenario.identity.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.PlanID != runtime.RecoveryPlanID || rebound.PlanSHA256 != runtime.RecoveryPlanSHA256 ||
		len(providers) != len(sourceProviders) || providers[0].Status != lyricsprovideroutcome.StatusCandidate ||
		providers[0].ReasonCode != lyricsprovideroutcome.ReasonCandidate ||
		providers[0].Phase != lyricsprovideroutcome.PhaseFinalize {
		t.Fatalf("rebound set=%+v providers=%+v", rebound, providers)
	}
	for providerIndex, provider := range providers {
		if provider.Provider != sourceProviders[providerIndex].Provider ||
			!reflect.DeepEqual(provider.AcquisitionIDs, sourceProviders[providerIndex].AcquisitionIDs) {
			t.Fatal("rebind changed an exact provider or AcquisitionID sequence")
		}
		for _, acquisitionID := range provider.AcquisitionIDs {
			if _, err := destination.ReplayByAcquisitionID(t.Context(), acquisitionID); err != nil {
				t.Fatalf("destination exact replay %s: %v", acquisitionID, err)
			}
		}
	}
	if err := ValidateAcquisitionSetClosedReplay(
		t.Context(), rebound, destination, runtime, []lyricssource.MusicIdentity{scenario.identity},
	); err != nil {
		t.Fatal(err)
	}
}

func TestRebindAcquisitionSetWithSekaipediaListReplacesOnlyListObservation(t *testing.T) {
	scenario := newProviderPrefixScenario(t, nil)
	sourceSet, err := NewAcquisitionSet(
		"historical-list-source", strings.Repeat("a", 64), scenario.runtime.Order,
		[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: scenario.providers}},
	)
	if err != nil {
		t.Fatal(err)
	}
	oldListID := scenario.providers[0].AcquisitionIDs[0]
	list, err := scenario.ledger.ReplayByAcquisitionID(t.Context(), oldListID)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := lyricsacquisition.CreateLedger(t.Context(), filepath.Join(t.TempDir(), "destination-ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	runtime := cloneRuntimeConfig(scenario.runtime)
	runtime.RecoveryPlanID = "current-list-target"
	runtime.RecoveryPlanSHA256 = strings.Repeat("b", 64)
	runtime.SekaipediaCanary = &SekaipediaCanaryPlan{
		RecoveryPlanID: runtime.RecoveryPlanID, RecoveryPlanSHA256: runtime.RecoveryPlanSHA256,
		ListAcquisitionID: string(list.AcquisitionID), List: runtime.Authorities[lyricssource.ProviderSekaipedia][0],
		Songs: []SekaipediaCanarySongPlan{},
	}
	rebound, err := RebindAcquisitionSetWithSekaipediaList(
		t.Context(), sourceSet, scenario.ledger, list, destination, runtime,
		[]lyricssource.MusicIdentity{scenario.identity},
	)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := rebound.OrderedProviders(scenario.identity.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != len(scenario.providers) || len(providers[0].AcquisitionIDs) < 2 ||
		providers[0].AcquisitionIDs[0] == oldListID ||
		!reflect.DeepEqual(providers[0].AcquisitionIDs[1:], scenario.providers[0].AcquisitionIDs[1:]) {
		t.Fatalf("List-rebound providers=%+v source=%+v", providers, scenario.providers)
	}
	newList, err := destination.ReplayByAcquisitionID(t.Context(), providers[0].AcquisitionIDs[0])
	if err != nil || newList.RawResponseSHA256 != list.RawResponseSHA256 || !reflect.DeepEqual(newList.RawResponse, list.RawResponse) {
		t.Fatalf("rebound List=%+v err=%v", newList, err)
	}
}

func TestPartitionRebindMusicIDsAllowsSupplementToAddMissingDestinationSongs(t *testing.T) {
	supplement := AcquisitionSet{Songs: []SongAcquisitionSet{{MusicID: 765}, {MusicID: 789}}}
	primary, supplemented, ordered, err := partitionRebindMusicIDs([]int{2, 765, 789, 795}, &supplement)
	if err != nil || !reflect.DeepEqual(primary, []int{2, 795}) ||
		!reflect.DeepEqual(ordered, []int{765, 789}) || len(supplemented) != 2 {
		t.Fatalf("primary=%v supplemented=%v ordered=%v err=%v", primary, supplemented, ordered, err)
	}
	if _, ok := supplemented[765]; !ok {
		t.Fatal("REM was not assigned to the supplemental source")
	}
	if _, ok := supplemented[789]; !ok {
		t.Fatal("Tenbin was not assigned to the supplemental source")
	}
	outside := AcquisitionSet{Songs: []SongAcquisitionSet{{MusicID: 999}}}
	if _, _, _, err := partitionRebindMusicIDs([]int{2, 765}, &outside); err == nil {
		t.Fatal("supplement outside destination scope was accepted")
	}
}

func TestRebindAcquisitionSetWithSupplementReplacesOnlyDeclaredSongSource(t *testing.T) {
	scenario := newProviderPrefixScenario(t, nil)
	baseProviders := cloneProviderSetSlice(scenario.providers)
	baseProviders[0].AcquisitionIDs[0] = lyricsacquisition.AcquisitionID(strings.Repeat("f", 64))
	baseSet, err := NewAcquisitionSet(
		"historical-rebind-source", strings.Repeat("a", 64), scenario.runtime.Order,
		[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: baseProviders}},
	)
	if err != nil {
		t.Fatal(err)
	}
	supplementSet, err := NewAcquisitionSet(
		"reviewed-gap-supplement", strings.Repeat("d", 64), scenario.runtime.Order,
		[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: scenario.providers}},
	)
	if err != nil {
		t.Fatal(err)
	}
	supplementLedger, err := lyricsacquisition.CreateLedger(
		t.Context(), filepath.Join(t.TempDir(), "supplement-ledger"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer supplementLedger.Close()
	if err := copyReboundAcquisitions(
		t.Context(), supplementSet, scenario.ledger, supplementLedger,
	); err != nil {
		t.Fatal(err)
	}
	destination, err := lyricsacquisition.CreateLedger(
		t.Context(), filepath.Join(t.TempDir(), "destination-ledger"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	runtime := cloneRuntimeConfig(scenario.runtime)
	runtime.RecoveryPlanID = "current-supplemented-target"
	runtime.RecoveryPlanSHA256 = strings.Repeat("e", 64)

	rebound, err := RebindAcquisitionSetWithSupplement(
		t.Context(), baseSet, scenario.ledger, supplementSet, supplementLedger,
		destination, runtime, []lyricssource.MusicIdentity{scenario.identity},
	)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := rebound.OrderedProviders(scenario.identity.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != len(scenario.providers) || len(providers[0].AcquisitionIDs) == 0 ||
		providers[0].AcquisitionIDs[0] == baseProviders[0].AcquisitionIDs[0] {
		t.Fatalf("supplemented providers=%+v base=%+v", providers, baseProviders)
	}
	for providerIndex, provider := range providers {
		if provider.Provider != scenario.providers[providerIndex].Provider ||
			!reflect.DeepEqual(provider.AcquisitionIDs, scenario.providers[providerIndex].AcquisitionIDs) {
			t.Fatalf("supplement changed provider identity: got=%+v want=%+v", provider, scenario.providers[providerIndex])
		}
		for _, acquisitionID := range provider.AcquisitionIDs {
			if _, err := destination.ReplayByAcquisitionID(t.Context(), acquisitionID); err != nil {
				t.Fatalf("destination supplement replay %s: %v", acquisitionID, err)
			}
		}
	}
}

func TestRebindAcquisitionSetFailsClosed(t *testing.T) {
	scenario := newProviderPrefixScenario(t, nil)
	runtime := cloneRuntimeConfig(scenario.runtime)
	runtime.RecoveryPlanID = "current-rebind-target"
	runtime.RecoveryPlanSHA256 = strings.Repeat("c", 64)
	identities := []lyricssource.MusicIdentity{scenario.identity}

	t.Run("source and destination alias", func(t *testing.T) {
		sourceSet, err := NewAcquisitionSet(
			"historical-rebind-source", strings.Repeat("a", 64), scenario.runtime.Order,
			[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: scenario.providers}},
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RebindAcquisitionSet(
			t.Context(), sourceSet, scenario.ledger, scenario.ledger, runtime, identities,
		); err == nil {
			t.Fatal("aliased source and destination ledgers were accepted")
		}
	})

	t.Run("missing exact acquisition", func(t *testing.T) {
		providers := cloneProviderSetSlice(scenario.providers)
		providers[0].AcquisitionIDs[0] = lyricsacquisition.AcquisitionID(strings.Repeat("f", 64))
		sourceSet, err := NewAcquisitionSet(
			"historical-rebind-source", strings.Repeat("a", 64), scenario.runtime.Order,
			[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: providers}},
		)
		if err != nil {
			t.Fatal(err)
		}
		destination, err := lyricsacquisition.CreateLedger(
			t.Context(), filepath.Join(t.TempDir(), "destination-ledger"),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer destination.Close()
		if _, err := RebindAcquisitionSet(
			t.Context(), sourceSet, scenario.ledger, destination, runtime, identities,
		); err == nil {
			t.Fatal("missing exact AcquisitionID was accepted")
		}
	})

	t.Run("identity order", func(t *testing.T) {
		sourceSet, err := NewAcquisitionSet(
			"historical-rebind-source", strings.Repeat("a", 64), scenario.runtime.Order,
			[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: scenario.providers}},
		)
		if err != nil {
			t.Fatal(err)
		}
		destination, err := lyricsacquisition.CreateLedger(
			t.Context(), filepath.Join(t.TempDir(), "destination-ledger"),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer destination.Close()
		duplicated := []lyricssource.MusicIdentity{scenario.identity, scenario.identity}
		if _, err := RebindAcquisitionSet(
			t.Context(), sourceSet, scenario.ledger, destination, runtime, duplicated,
		); err == nil {
			t.Fatal("noncanonical identity order was accepted")
		}
	})
}
