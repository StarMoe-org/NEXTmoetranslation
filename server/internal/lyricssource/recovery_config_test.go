package lyricssource

import (
	"testing"

	"moesekai/server/internal/lyricsproviderpolicy"
	"moesekai/server/internal/model"
)

// The recovery paths resolve endpoints from this package's own constants so the
// production binary does not link the offline provider-policy package. This
// test keeps both tables identical.
func TestRecoveryCanonicalEndpointMatchesProviderPolicy(t *testing.T) {
	providers := []model.LyricsSourceProvider{
		ProviderVocaloidFandom,
		ProviderMoegirl,
		ProviderSekaipedia,
		ProviderMoegirlPublicExact,
		model.LyricsSourceProvider("unknown"),
	}
	for _, provider := range providers {
		got, gotOK := recoveryCanonicalEndpoint(provider)
		want, wantOK := lyricsproviderpolicy.CanonicalEndpointV1(lyricsproviderpolicy.Provider(provider))
		if gotOK != wantOK || got != want {
			t.Fatalf("provider %q: recoveryCanonicalEndpoint = (%q, %v), policy = (%q, %v)",
				provider, got, gotOK, want, wantOK)
		}
	}
	for _, spec := range lyricsproviderpolicy.CompiledProviderSpecsV1() {
		if _, ok := recoveryCanonicalEndpoint(model.LyricsSourceProvider(spec.Provider)); !ok {
			t.Fatalf("policy provider %q has no endpoint in lyricssource", spec.Provider)
		}
	}
}
