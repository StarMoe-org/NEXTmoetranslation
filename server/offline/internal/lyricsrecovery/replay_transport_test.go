package lyricsrecovery

import (
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

func TestReplayTransportFailsClosedOnExactSetDrift(t *testing.T) {
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	ledger, err := lyricsacquisition.CreateLedger(ctx, root+"/ledger")
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := catalog.MusicIdentity(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	providers, _, err := AcquireSong(ctx, 2, identity, runtime, ledger, fixtureProviderTransports(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Provider != model.LyricsSourceProviderSekaipedia ||
		len(providers[0].AcquisitionIDs) < 2 {
		t.Fatalf("fixture did not produce one complete exact Sekaipedia prefix: %+v", providers)
	}
	sekaipedia := providers[0]

	t.Run("unauthorized provider set", func(t *testing.T) {
		hostile := appendProviderSets(providers, ProviderAcquisitionSet{
			Provider: model.LyricsSourceProvider("arbitrary"), AcquisitionIDs: []lyricsacquisition.AcquisitionID{},
		})
		if _, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, hostile); err == nil {
			t.Fatal("unauthorized provider acquisition set was silently ignored")
		}
	})

	t.Run("provider after stopping point", func(t *testing.T) {
		extra := appendProviderSets(providers, ProviderAcquisitionSet{
			Provider: model.LyricsSourceProviderMoegirl, AcquisitionIDs: []lyricsacquisition.AcquisitionID{},
			Status: lyricsprovideroutcome.StatusNoMatch, ReasonCode: lyricsprovideroutcome.ReasonNoSearchHits,
			Phase: lyricsprovideroutcome.PhaseResolveTargets, Counts: lyricsprovideroutcome.Counts{NoMatch: 1},
		})
		if _, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, extra); err == nil {
			t.Fatal("declared provider after the complete stopping point was accepted")
		}
	})

	t.Run("missing acquisition ID", func(t *testing.T) {
		terminal := sekaipedia
		terminal.AcquisitionIDs = []lyricsacquisition.AcquisitionID{lyricsacquisition.AcquisitionID(strings.Repeat("f", 64))}
		if _, err := NewReplayTransport(ctx, terminal.Provider, runtime.Authorities[terminal.Provider], ledger, terminal); err == nil {
			t.Fatal("missing exact acquisition ID was accepted")
		}
	})

	t.Run("duplicate acquisition ID", func(t *testing.T) {
		terminal := sekaipedia
		terminal.AcquisitionIDs = []lyricsacquisition.AcquisitionID{sekaipedia.AcquisitionIDs[0], sekaipedia.AcquisitionIDs[0]}
		if _, err := NewReplayTransport(ctx, terminal.Provider, runtime.Authorities[terminal.Provider], ledger, terminal); err == nil {
			t.Fatal("duplicate exact acquisition ID was accepted")
		}
	})

	t.Run("conflicting exact request", func(t *testing.T) {
		first, err := ledger.ReplayByAcquisitionID(ctx, sekaipedia.AcquisitionIDs[0])
		if err != nil {
			t.Fatal(err)
		}
		fetchedAt, err := time.Parse(time.RFC3339Nano, first.FetchedAt)
		if err != nil {
			t.Fatal(err)
		}
		capture, err := lyricssource.CaptureRecoveryHTTPResponse(
			model.LyricsSourceProviderSekaipedia,
			runtime.Authorities[model.LyricsSourceProviderSekaipedia],
			lyricssource.RecoveryHTTPResponse{
				Action: "page", CanonicalRequestURL: first.Request.CanonicalRequestIdentity,
				FetchedAt: fetchedAt.Add(time.Second), Raw: first.RawResponse,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		second, err := ledger.Commit(ctx, recordInputFromCapture(capture))
		if err != nil {
			t.Fatal(err)
		}
		terminal := sekaipedia
		terminal.AcquisitionIDs = []lyricsacquisition.AcquisitionID{first.AcquisitionID, second.AcquisitionID}
		if _, err := NewReplayTransport(ctx, terminal.Provider, runtime.Authorities[terminal.Provider], ledger, terminal); err == nil {
			t.Fatal("conflicting acquisitions for one canonical request were accepted")
		}
	})

	t.Run("unconsumed acquisition", func(t *testing.T) {
		transport, err := NewReplayTransport(ctx, sekaipedia.Provider, runtime.Authorities[sekaipedia.Provider], ledger, sekaipedia)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.AssertOutcomeConsumed(lyricsprovideroutcome.Outcome[lyricssource.Candidate]{}); err == nil {
			t.Fatal("unconsumed exact acquisition set was accepted")
		}
	})

	t.Run("required request missing from closed set", func(t *testing.T) {
		changed := cloneProviderSetSlice(providers)
		changed[0].AcquisitionIDs = append([]lyricsacquisition.AcquisitionID{}, changed[0].AcquisitionIDs[:1]...)
		if _, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, changed); err == nil {
			t.Fatal("closed set missing a parser-required request was accepted")
		}
	})

	t.Run("request order conflict", func(t *testing.T) {
		changed := cloneProviderSetSlice(providers)
		terminal := &changed[0]
		for left, right := 0, len(terminal.AcquisitionIDs)-1; left < right; left, right = left+1, right-1 {
			terminal.AcquisitionIDs[left], terminal.AcquisitionIDs[right] = terminal.AcquisitionIDs[right], terminal.AcquisitionIDs[left]
		}
		if _, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, changed); err == nil {
			t.Fatal("out-of-order exact acquisition set was accepted")
		}
	})

	t.Run("not plan authorized", func(t *testing.T) {
		unauthorized := cloneRuntimeConfig(runtime)
		unauthorized.Authorities[model.LyricsSourceProviderSekaipedia] = []lyricssource.FixedIndex{}
		if _, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, unauthorized, ledger, cloneProviderSetSlice(providers)); err == nil {
			t.Fatal("acquisition outside the supplied immutable authority set was accepted")
		}
	})

	t.Run("stale terminal conflict", func(t *testing.T) {
		changed := cloneProviderSetSlice(providers)
		terminal := &changed[0]
		terminal.Status = lyricsprovideroutcome.StatusStale
		terminal.ReasonCode = lyricsprovideroutcome.ReasonRevisionChanged
		terminal.Phase = lyricsprovideroutcome.PhaseAcquireAuthority
		terminal.Counts = lyricsprovideroutcome.Counts{Acquisitions: len(terminal.AcquisitionIDs), Stale: 1}
		if _, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, changed); err == nil {
			t.Fatal("stale closed terminal conflicting with exact replay was accepted")
		}
		rebound, err := replaySong(
			ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, changed, false,
		)
		if err != nil || rebound.Composition == nil {
			t.Fatalf("exact acquisition rebind did not derive the current parser terminal: result=%+v err=%v", rebound, err)
		}
	})

	t.Run("cross-song acquisitions", func(t *testing.T) {
		otherIdentity, err := catalog.MusicIdentity(ctx, 235)
		if err != nil {
			t.Fatal(err)
		}
		otherProviders, _, err := AcquireSong(
			ctx, otherIdentity.MusicID, otherIdentity, runtime, ledger, fixtureProviderTransports(t),
		)
		if err != nil || len(otherProviders) != 1 {
			t.Fatalf("other song exact prefix=%+v err=%v", otherProviders, err)
		}
		changed := cloneProviderSetSlice(providers)
		changed[0].AcquisitionIDs = append(
			[]lyricsacquisition.AcquisitionID(nil), otherProviders[0].AcquisitionIDs...,
		)
		if _, err := ReplaySong(ctx, 2, identity, plan.Versions.ProviderPolicy, runtime, ledger, changed); err == nil {
			t.Fatal("cross-song exact acquisitions were accepted")
		}
	})

	t.Run("no hidden full-registry requirement", func(t *testing.T) {
		minimal := cloneRuntimeConfig(runtime)
		minimal.Providers = append([]lyricssource.ProviderConfig(nil), minimal.Providers[:1]...)
		delete(minimal.Authorities, model.LyricsSourceProviderMoegirl)
		delete(minimal.Authorities, model.LyricsSourceProviderVocaloidFandom)
		delete(minimal.Parsers, model.LyricsSourceProviderMoegirl)
		delete(minimal.Parsers, model.LyricsSourceProviderVocaloidFandom)
		result, err := ReplaySong(
			ctx, 2, identity, plan.Versions.ProviderPolicy, minimal, ledger, cloneProviderSetSlice(providers),
		)
		if err != nil || len(result.Providers) != 1 || result.Composition == nil {
			t.Fatalf("complete exact prefix acquired a hidden later-provider requirement: result=%+v err=%v", result, err)
		}
	})

	t.Run("policy drift", func(t *testing.T) {
		if _, err := ReplaySong(
			ctx, 2, identity, plan.Versions.ProviderPolicy+"-drift", runtime, ledger, cloneProviderSetSlice(providers),
		); err == nil {
			t.Fatal("provider policy drift was accepted")
		}
	})
}

func cloneProviderSetSlice(input []ProviderAcquisitionSet) []ProviderAcquisitionSet {
	return cloneProviderAcquisitionSets(input)
}

func appendProviderSets(input []ProviderAcquisitionSet, values ...ProviderAcquisitionSet) []ProviderAcquisitionSet {
	result := cloneProviderAcquisitionSets(input)
	return append(result, values...)
}
