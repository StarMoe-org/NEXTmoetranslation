package lyricsproviderpolicy

import (
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

// The production lyricssource package resolves recovery endpoints from its own
// constants so the server binary never links this offline policy package. This
// test lives on the offline side and keeps both endpoint tables identical.
func TestRecoveryProviderConfigEndpointsMatchProviderPolicy(t *testing.T) {
	const minimum = 10 * time.Second
	sekaipediaAuthority := []lyricssource.FixedIndex{{
		PageID: 268, RevisionID: 335193, RevisionTimestamp: "2026-07-27T16:29:13Z",
		SHA1:          "b216a827f88c59f5e954a120027832fe9cd74413",
		ContentSHA256: "aaddff2922548aab7e522124ff2bad86427501930d549c9d94c9b4e473c35f92",
		RawSHA256:     "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
		Title:         "List of songs",
	}}
	moegirlIndex := []lyricssource.FixedIndex{{
		PageID: 1, RevisionID: 1, SHA1: "b216a827f88c59f5e954a120027832fe9cd74413", Title: "Song",
	}}
	indexes := map[Provider][]lyricssource.FixedIndex{
		ProviderVocaloidFandom: nil,
		ProviderMoegirl:        moegirlIndex,
		ProviderSekaipedia:     sekaipediaAuthority,
	}

	specs := CompiledProviderSpecsV1()
	if len(specs) != len(indexes) {
		t.Fatalf("policy compiles %d providers, test fixtures cover %d", len(specs), len(indexes))
	}
	for _, spec := range specs {
		fixture, ok := indexes[spec.Provider]
		if !ok {
			t.Fatalf("policy provider %q has no fixture", spec.Provider)
		}
		want, ok := CanonicalEndpointV1(spec.Provider)
		if !ok {
			t.Fatalf("policy provider %q has no canonical endpoint", spec.Provider)
		}
		config, err := lyricssource.RecoveryProviderConfig(
			model.LyricsSourceProvider(spec.Provider), minimum, minimum, fixture, nil,
		)
		if err != nil {
			t.Fatalf("provider %q: RecoveryProviderConfig: %v", spec.Provider, err)
		}
		if config.APIEndpoint != want {
			t.Fatalf("provider %q: lyricssource endpoint %q, policy endpoint %q", spec.Provider, config.APIEndpoint, want)
		}
	}

	for _, provider := range []model.LyricsSourceProvider{
		lyricssource.ProviderMoegirlPublicExact,
		model.LyricsSourceProvider("unknown"),
	} {
		if _, ok := CanonicalEndpointV1(Provider(provider)); ok {
			t.Fatalf("policy unexpectedly compiles an endpoint for %q", provider)
		}
		if _, err := lyricssource.RecoveryProviderConfig(provider, minimum, minimum, nil, nil); err == nil {
			t.Fatalf("lyricssource unexpectedly accepts recovery provider %q", provider)
		}
	}
}
