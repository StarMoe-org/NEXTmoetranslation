package lyricsrecovery

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsextractionplan"
	"moesekai/server/internal/lyricsoutcomeartifact"
	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

type providerPrefixScenario struct {
	plan       lyricsextractionplan.RecoveryPlan
	runtime    RuntimeConfig
	ledger     *lyricsacquisition.Ledger
	identity   lyricssource.MusicIdentity
	providers  []ProviderAcquisitionSet
	progress   []AcquisitionProgress
	transports map[model.LyricsSourceProvider]http.RoundTripper
}

func TestProviderPrefixSekaipediaCompleteStopsAcquisitionAndReplay(t *testing.T) {
	scenario := newProviderPrefixScenario(t, nil)
	if len(scenario.providers) != 1 || len(scenario.progress) != 1 ||
		scenario.providers[0].Provider != scenario.runtime.Order[0] ||
		scenario.providers[0].Status != lyricsprovideroutcome.StatusCandidate {
		t.Fatalf("complete authority prefix=%+v progress=%+v", scenario.providers, scenario.progress)
	}
	assertProviderRequestCounts(t, scenario, true, false, false)

	set, err := NewAcquisitionSet(
		scenario.plan.PlanID, strings.Repeat("a", 64), scenario.runtime.Order,
		[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: scenario.providers}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAcquisitionSetAuthorization(
		set, scenario.plan.PlanID, strings.Repeat("a", 64), []int{scenario.identity.MusicID},
		scenario.runtime.Order, scenario.runtime.ProviderMusicIDs,
	); err != nil {
		t.Fatal(err)
	}
	ordered, err := set.OrderedProviders(scenario.identity.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	first, second := replayTwice(t, scenario, ordered)
	if len(first.Providers) != 1 || len(second.Providers) != 1 || first.Composition == nil {
		t.Fatalf("complete exact replay prefix first=%d second=%d composition=%t",
			len(first.Providers), len(second.Providers), first.Composition != nil)
	}
	result, err := NewSongResult(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ProviderOutcomes) != 1 || result.ProviderOutcomes[0].Provider != scenario.runtime.Order[0] {
		t.Fatalf("complete song result provider refs=%+v", result.ProviderOutcomes)
	}
}

func TestSekaipediaOnlyPlanStopsAfterSekaipediaMissWithoutFallback(t *testing.T) {
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	ledger, err := lyricsacquisition.CreateLedger(ctx, root+"/ledger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	plan.Providers.Order = append([]lyricsextractionplan.Provider(nil), plan.Providers.Order[:1]...)
	plan.Providers.Configurations = append([]lyricsextractionplan.RecoveryProviderPlan(nil), plan.Providers.Configurations[:1]...)
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := catalog.MusicIdentity(ctx, plan.Scope.MusicIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	transports := withSekaipediaSongIDMismatch(t, fixtureProviderTransports(t))
	providers, progress, err := AcquireSong(ctx, identity.MusicID, identity, runtime, ledger, transports)
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Order) != 1 || len(providers) != 1 || len(progress) != 1 ||
		providers[0].Provider != model.LyricsSourceProviderSekaipedia ||
		providers[0].Status != lyricsprovideroutcome.StatusNoMatch {
		t.Fatalf("Sekaipedia-only miss runtime=%+v providers=%+v progress=%+v", runtime.Order, providers, progress)
	}
	if transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatal("Sekaipedia-only plan contacted an excluded provider")
	}
	replayed, err := ReplaySong(ctx, identity.MusicID, identity, plan.Versions.ProviderPolicy, runtime, ledger, providers)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewSongResult(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != lyricscontract.CoverageMissing || len(result.ProviderOutcomes) != 1 || result.Full != nil ||
		result.SelectedEvidence == nil {
		t.Fatalf("Sekaipedia-only missing result=%+v", result)
	}
	if _, err := MarshalSongResult(result); err != nil {
		t.Fatalf("Sekaipedia-only missing result is not canonical after cloning: %v", err)
	}
	rootRef, err := RootSongRef(result)
	if err != nil {
		t.Fatal(err)
	}
	if rootRef.ProviderOutcomes == nil || rootRef.SelectedEvidence == nil {
		t.Fatalf("Sekaipedia-only missing root ref lost explicit arrays: %+v", rootRef)
	}
	resolver, err := lyricsevidencepack.OpenResolver(root + "/parent-pack")
	if err != nil {
		t.Fatal(err)
	}
	songs := append([]lyricsrootmanifest.SongResultRef{}, parent.Songs...)
	replaced := false
	for index := range songs {
		if songs[index].MusicID == rootRef.MusicID {
			songs[index] = rootRef
			replaced = true
			break
		}
	}
	if !replaced {
		t.Fatalf("fixture parent does not contain music %d", rootRef.MusicID)
	}
	for index, song := range songs {
		if song.ProviderOutcomes == nil || song.SelectedEvidence == nil {
			t.Fatalf("root assembly fixture song %d lost explicit arrays: %+v", index, song)
		}
	}
	if _, err := lyricsrootmanifest.Assemble(lyricsrootmanifest.AssemblyRequest{
		RootID: "sekaipedia-only-missing-root", Scope: parent.Scope,
		Catalog: parent.Catalog, Plan: parent.Plan, Songs: songs,
	}, resolver); err != nil {
		t.Fatalf("Sekaipedia-only missing root ref is invalid during assembly: %v", err)
	}
}

func TestProviderPrefixClosedSekaipediaMissCallsMoegirl(t *testing.T) {
	scenario := newProviderPrefixScenario(t, func(
		t *testing.T,
		plan *lyricsextractionplan.RecoveryPlan,
		_ lyricssource.MusicIdentity,
	) map[model.LyricsSourceProvider]http.RoundTripper {
		return withSekaipediaSongIDMismatch(t, fixtureProviderTransports(t))
	})
	if len(scenario.providers) < 2 || scenario.providers[0].Status != lyricsprovideroutcome.StatusNoMatch ||
		scenario.providers[1].Provider != scenario.runtime.Order[1] {
		t.Fatalf("closed authority miss prefix=%+v", scenario.providers)
	}
	if scenario.transports[scenario.runtime.Order[1]].(*fixtureRoundTripper).requestCount() == 0 {
		t.Fatal("closed Sekaipedia miss did not call the next provider")
	}
}

func TestProviderPrefixMoegirlCompleteStopsBeforeFandom(t *testing.T) {
	scenario := newProviderPrefixScenario(t, func(
		t *testing.T,
		plan *lyricsextractionplan.RecoveryPlan,
		identity lyricssource.MusicIdentity,
	) map[model.LyricsSourceProvider]http.RoundTripper {
		return withSekaipediaSongIDMismatch(t, moegirlPrefixTransports(t, plan, identity, "完全な歌", nil))
	})
	if len(scenario.providers) != 2 || scenario.providers[1].Status != lyricsprovideroutcome.StatusCandidate {
		t.Fatalf("complete Moegirl prefix=%+v", scenario.providers)
	}
	assertProviderRequestCounts(t, scenario, true, true, false)
	first, _ := replayTwice(t, scenario, scenario.providers)
	if first.Composition == nil || len(first.Providers) != 2 ||
		first.Composition.Components.FullText != first.Providers[1].Artifact.OutcomeID {
		t.Fatalf("complete Moegirl replay composition=%+v providers=%d", first.Composition, len(first.Providers))
	}
}

func TestProviderPrefixMoegirlExplicitGameOnlyStopsWithoutFandom(t *testing.T) {
	game := "同じ歌"
	scenario := newProviderPrefixScenario(t, func(
		t *testing.T,
		plan *lyricsextractionplan.RecoveryPlan,
		identity lyricssource.MusicIdentity,
	) map[model.LyricsSourceProvider]http.RoundTripper {
		original := "<--Tag-Start:Game Ver.-->\n" + game + "\n<--Tag-End-->"
		return withSekaipediaSongIDMismatch(t, moegirlPrefixTransports(t, plan, identity, original, nil))
	})
	if len(scenario.providers) != 2 || scenario.providers[1].Status != lyricsprovideroutcome.StatusCandidate {
		t.Fatalf("explicit Game-only prefix=%+v", scenario.providers)
	}
	assertProviderRequestCounts(t, scenario, true, true, false)
	first, _ := replayTwice(t, scenario, scenario.providers)
	if first.Composition == nil || first.Composition.Game == nil || len(first.Composition.Full.Lines) != 0 ||
		len(first.Composition.SelectedSourceKeys) != 1 ||
		first.Composition.Components.GameText != first.Providers[1].Artifact.OutcomeID ||
		first.Composition.Components.FullText != "" ||
		first.Composition.Components.VersionEvidence != first.Providers[1].Artifact.OutcomeID {
		t.Fatalf("explicit Game-only composed decision=%+v", first.Composition)
	}
	result, err := NewSongResult(first)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != lyricscontract.CoverageGameOnly || result.Full != nil || result.Game == nil ||
		len(result.ProviderOutcomes) != 2 {
		t.Fatalf("Game-only result or provider prefix=%+v", result)
	}
	for index, ref := range result.ProviderOutcomes {
		if ref.Provider != scenario.runtime.Order[index] {
			t.Fatalf("provider outcome %d=%s, want plan order %s", index, ref.Provider, scenario.runtime.Order[index])
		}
	}
	reordered := append([]lyricsrootmanifest.ProviderOutcomeRef(nil), result.ProviderOutcomes...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if err := validatePlanOrderedOutcomeRefs(scenario.runtime.Order, reordered); err == nil {
		t.Fatal("string-sorted or reordered provider outcomes were accepted as the evaluated plan prefix")
	}
}

func TestProviderPrefixTransportAndFallbackDeniedTerminalsStop(t *testing.T) {
	for name, response := range map[string]func(*http.Request) ([]byte, error){
		"transport": func(*http.Request) ([]byte, error) {
			return nil, errors.New("offline fixture transport failure")
		},
		"malformed finalization": func(*http.Request) ([]byte, error) {
			return []byte(`{"query":{}}`), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			scenario := newProviderPrefixScenario(t, func(
				t *testing.T,
				_ *lyricsextractionplan.RecoveryPlan,
				_ lyricssource.MusicIdentity,
			) map[model.LyricsSourceProvider]http.RoundTripper {
				transports := fixtureProviderTransports(t)
				transports[model.LyricsSourceProviderSekaipedia] = &fixtureRoundTripper{
					provider: model.LyricsSourceProviderSekaipedia, respond: response,
				}
				return transports
			})
			if len(scenario.providers) != 1 {
				t.Fatalf("fallback-denied prefix=%+v", scenario.providers)
			}
			if name == "transport" && scenario.providers[0].Status != lyricsprovideroutcome.StatusTransportError {
				t.Fatalf("transport terminal=%+v", scenario.providers[0])
			}
			if name == "malformed finalization" &&
				(scenario.providers[0].Status != lyricsprovideroutcome.StatusUnsupported ||
					scenario.providers[0].ReasonCode != lyricsprovideroutcome.ReasonMalformedResponse) {
				t.Fatalf("malformed terminal=%+v", scenario.providers[0])
			}
			assertProviderRequestCounts(t, scenario, true, false, false)
		})
	}
}

func TestProviderPrefixRetryAttemptsRemainProviderLocal(t *testing.T) {
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	ledger, err := lyricsacquisition.CreateLedger(ctx, root+"/ledger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime.MaxAttempts = 2
	runtime.RetryDelay = 0
	identity, err := catalog.MusicIdentity(ctx, plan.Scope.MusicIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	transports := fixtureProviderTransports(t)
	sekaipediaFixture := transports[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper)
	attempt := 0
	transports[model.LyricsSourceProviderSekaipedia] = &fixtureRoundTripper{
		provider: model.LyricsSourceProviderSekaipedia,
		respond: func(request *http.Request) ([]byte, error) {
			attempt++
			if attempt == 1 {
				return nil, errors.New("retryable provider-local fixture transport failure")
			}
			return sekaipediaFixture.respond(request)
		},
	}
	providers, _, err := AcquireSong(ctx, identity.MusicID, identity, runtime, ledger, transports)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Status != lyricsprovideroutcome.StatusCandidate || attempt < 2 {
		t.Fatalf("provider-local retry prefix=%+v attempts=%d", providers, attempt)
	}
	if transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatal("retry advanced to a later provider before the current provider exhausted its local attempts")
	}
}

func TestAcquisitionSessionSerializesActualProviderRequestsAcrossSongs(t *testing.T) {
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	ledger, err := lyricsacquisition.CreateLedger(ctx, root+"/ledger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	transports := fixtureProviderTransports(t)
	session, err := NewAcquisitionSession(runtime, ledger, transports)
	if err != nil {
		t.Fatal(err)
	}
	identities := make([]lyricssource.MusicIdentity, len(plan.Scope.MusicIDs))
	for index, musicID := range plan.Scope.MusicIDs {
		identities[index], err = catalog.MusicIdentity(ctx, musicID)
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, len(identities))
	for _, identity := range identities {
		identity := identity
		go func() {
			<-start
			providers, _, acquireErr := session.AcquireSong(ctx, identity.MusicID, identity)
			if acquireErr == nil && (len(providers) != 1 || providers[0].Status != lyricsprovideroutcome.StatusCandidate) {
				acquireErr = fmt.Errorf("music %d concurrent provider prefix=%+v", identity.MusicID, providers)
			}
			results <- acquireErr
		}()
	}
	close(start)
	for range identities {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatal("concurrent complete Sekaipedia acquisition touched a later provider")
	}
}

func TestProviderPrefixCancellationFailsClosedBeforeFallback(t *testing.T) {
	setupContext := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(setupContext, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	ledger, err := lyricsacquisition.CreateLedger(setupContext, root+"/ledger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	parent := fixtureParentRoot(t, setupContext, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := catalog.MusicIdentity(setupContext, plan.Scope.MusicIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	transports := fixtureProviderTransports(t)
	ctx, cancel := context.WithCancel(setupContext)
	cancel()
	providers, _, err := AcquireSong(ctx, identity.MusicID, identity, runtime, ledger, transports)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Provider != runtime.Order[0] ||
		providers[0].Status != lyricsprovideroutcome.StatusTransportError ||
		providers[0].ReasonCode != lyricsprovideroutcome.ReasonCanceled {
		t.Fatalf("canceled provider prefix did not fail closed: %+v", providers)
	}
	for _, provider := range runtime.Order {
		if transports[provider].(*fixtureRoundTripper).requestCount() != 0 {
			t.Fatalf("canceled acquisition touched provider transport %s", provider)
		}
	}
}

func TestProviderPrefixTamperAndUndeclaredLaterAcquisitionsFail(t *testing.T) {
	game := "同じ歌"
	scenario := newProviderPrefixScenario(t, func(
		t *testing.T,
		plan *lyricsextractionplan.RecoveryPlan,
		identity lyricssource.MusicIdentity,
	) map[model.LyricsSourceProvider]http.RoundTripper {
		return withSekaipediaSongIDMismatch(t, moegirlPrefixTransports(t, plan, identity,
			"<--Tag-Start:Game Ver.-->\n"+game+"\n<--Tag-End-->",
			[]string{"前の歌", game, "後の歌"},
		))
	})
	if len(scenario.providers) != 2 {
		t.Fatalf("tamper fixture prefix=%+v", scenario.providers)
	}
	for name, providers := range map[string][]ProviderAcquisitionSet{
		"reordered":  {scenario.providers[1], scenario.providers[0]},
		"duplicated": {scenario.providers[0], scenario.providers[0]},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAcquisitionSet(
				scenario.plan.PlanID, strings.Repeat("b", 64), scenario.runtime.Order,
				[]SongAcquisitionSet{{MusicID: scenario.identity.MusicID, Providers: providers}},
			); err == nil {
				t.Fatal("tampered provider prefix was accepted")
			}
		})
	}
	undeclaredLater := scenario.providers[1]
	undeclaredLater.Provider = scenario.runtime.Order[2]
	gapped, err := NewAcquisitionSet(
		scenario.plan.PlanID, strings.Repeat("b", 64), scenario.runtime.Order,
		[]SongAcquisitionSet{{
			MusicID:   scenario.identity.MusicID,
			Providers: []ProviderAcquisitionSet{scenario.providers[0], undeclaredLater},
		}},
	)
	if err != nil {
		t.Fatalf("plan-independent scoped selection shape was rejected: %v", err)
	}
	if err := ValidateAcquisitionSetAuthorization(
		gapped, scenario.plan.PlanID, strings.Repeat("b", 64), []int{scenario.identity.MusicID},
		scenario.runtime.Order, scenario.runtime.ProviderMusicIDs,
	); err == nil {
		t.Fatal("gapped unscoped provider evaluation was authorized")
	}
	crossProvider := cloneProviderSetSlice(scenario.providers)
	crossProvider[0].AcquisitionIDs = append(
		[]lyricsacquisition.AcquisitionID(nil), scenario.providers[1].AcquisitionIDs...,
	)
	if _, err := ReplaySong(
		t.Context(), scenario.identity.MusicID, scenario.identity, scenario.plan.Versions.ProviderPolicy,
		scenario.runtime, scenario.ledger, crossProvider,
	); err == nil {
		t.Fatal("cross-provider acquisition IDs were accepted in an authorized plan prefix")
	}
	if _, err := ReplaySong(
		t.Context(), scenario.identity.MusicID, scenario.identity, scenario.plan.Versions.ProviderPolicy,
		scenario.runtime, scenario.ledger, scenario.providers[:1],
	); err == nil {
		t.Fatal("undeclared later acquisitions were silently treated as unnecessary")
	}
}

func newProviderPrefixScenario(
	t *testing.T,
	configure func(*testing.T, *lyricsextractionplan.RecoveryPlan, lyricssource.MusicIdentity) map[model.LyricsSourceProvider]http.RoundTripper,
) providerPrefixScenario {
	t.Helper()
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	ledger, err := lyricsacquisition.CreateLedger(ctx, root+"/ledger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	identity, err := catalog.MusicIdentity(ctx, plan.Scope.MusicIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	transports := fixtureProviderTransports(t)
	if configure != nil {
		transports = configure(t, &plan, identity)
	}
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	providers, progress, err := AcquireSong(ctx, identity.MusicID, identity, runtime, ledger, transports)
	if err != nil {
		t.Fatal(err)
	}
	return providerPrefixScenario{
		plan: plan, runtime: runtime, ledger: ledger, identity: identity,
		providers: providers, progress: progress, transports: transports,
	}
}

func moegirlPrefixTransports(
	t *testing.T,
	plan *lyricsextractionplan.RecoveryPlan,
	identity lyricssource.MusicIdentity,
	original string,
	fandomFull []string,
) map[model.LyricsSourceProvider]http.RoundTripper {
	t.Helper()
	const targetTitle = "前缀策略测试页"
	const targetAnchor = "前缀策略测试曲"
	indexBody := fmt.Sprintf("* [[%s#%s|%s]]\n", targetTitle, targetAnchor, identity.JapaneseTitle)
	var authority lyricsextractionplan.FixedAuthority
	for index := range plan.Providers.Configurations {
		configured := &plan.Providers.Configurations[index]
		if model.LyricsSourceProvider(configured.Provider) != model.LyricsSourceProviderMoegirl {
			continue
		}
		if len(configured.Authorities) != 1 {
			t.Fatal("Moegirl prefix fixture requires one authority")
		}
		digest := sha1.Sum([]byte(indexBody))
		configured.Authorities[0].SHA1 = hex.EncodeToString(digest[:])
		authority = configured.Authorities[0]
	}
	if authority.RevisionID == 0 {
		t.Fatal("Moegirl prefix fixture authority is missing")
	}
	songBody := moegirlPrefixSongBody(identity, targetAnchor, original)
	transports := fixtureProviderTransports(t)
	transports[model.LyricsSourceProviderMoegirl] = &fixtureRoundTripper{
		provider: model.LyricsSourceProviderMoegirl,
		respond: func(request *http.Request) ([]byte, error) {
			query := request.URL.Query()
			switch {
			case query.Get("revids") == fmt.Sprintf("%d", authority.RevisionID):
				return mediaWikiPageResponse(t, authority.PageID, authority.RevisionID, authority.Title, []byte(indexBody)), nil
			case query.Get("titles") == targetTitle:
				return mediaWikiPageResponse(t, 700001, 800001, targetTitle, []byte(songBody)), nil
			default:
				return nil, errors.New("unexpected Moegirl provider-prefix fixture request")
			}
		},
	}
	transports[model.LyricsSourceProviderVocaloidFandom] = &fixtureRoundTripper{
		provider: model.LyricsSourceProviderVocaloidFandom,
		respond: func(request *http.Request) ([]byte, error) {
			if fandomFull == nil {
				return nil, errors.New("Fandom must not be called after complete Moegirl composition")
			}
			if request.URL.Query().Get("generator") != "search" {
				return nil, errors.New("unexpected Fandom provider-prefix fixture request")
			}
			return fandomPrefixSearchResponse(t, identity, fandomFull), nil
		},
	}
	return transports
}

func moegirlPrefixSongBody(identity lyricssource.MusicIdentity, anchor, original string) string {
	return fmt.Sprintf(`== %s ==
{{ProjectsekaiSongGai
|曲名=%s
|作词=%s
|作曲=%s
|编曲=%s
}}
=== 歌词 ===
{{LyricsKai/ext
|type=colors,multiver
|colors=#39c
|charas=初音未来
|original=%s
}}
`, anchor, identity.JapaneseTitle, identity.Lyricist, identity.Composer, identity.Arranger, original)
}

func fandomPrefixSearchResponse(
	t *testing.T,
	identity lyricssource.MusicIdentity,
	lines []string,
) []byte {
	t.Helper()
	rows := make([]string, len(lines))
	for index, line := range lines {
		rows[index] = "|-\n|" + line
	}
	content := fmt.Sprintf("{{Song box 2\n|lyrics=%s\n|music=%s\n|arranger=%s\n}}\noriginal song\n== Lyrics ==\n<tabber>VOCALOID Version =\n{|\n! Japanese\n%s\n|}\n</tabber>",
		identity.Lyricist, identity.Composer, identity.Arranger, strings.Join(rows, "\n"))
	digest := sha1.Sum([]byte(content))
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"pages": map[string]any{
			"900001": map[string]any{
				"pageid": 900001, "title": identity.JapaneseTitle,
				"categories": []any{
					map[string]any{"title": "Category:Lyrics"},
					map[string]any{"title": "Category:Original songs"},
				},
				"revisions": []any{map[string]any{
					"revid": 900002, "sha1": hex.EncodeToString(digest[:]),
					"slots": map[string]any{"main": map[string]any{"content": content}},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func replayTwice(
	t *testing.T,
	scenario providerPrefixScenario,
	providers []ProviderAcquisitionSet,
) (ReplayResult, ReplayResult) {
	t.Helper()
	first, err := ReplaySong(
		t.Context(), scenario.identity.MusicID, scenario.identity, scenario.plan.Versions.ProviderPolicy,
		scenario.runtime, scenario.ledger, providers,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReplaySong(
		t.Context(), scenario.identity.MusicID, scenario.identity, scenario.plan.Versions.ProviderPolicy,
		scenario.runtime, scenario.ledger, providers,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Providers) != len(second.Providers) {
		t.Fatal("repeated exact replay provider prefix length drifted")
	}
	for index := range first.Providers {
		left, leftErr := lyricsoutcomeartifact.MarshalCanonical(first.Providers[index].Artifact)
		right, rightErr := lyricsoutcomeartifact.MarshalCanonical(second.Providers[index].Artifact)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			t.Fatalf("provider %d repeated exact replay was not byte-identical", index)
		}
	}
	leftResult, leftErr := NewSongResult(first)
	rightResult, rightErr := NewSongResult(second)
	if leftErr != nil || rightErr != nil {
		t.Fatalf("repeated exact replay song result errors left=%v right=%v", leftErr, rightErr)
	}
	left, leftMarshalErr := MarshalSongResult(leftResult)
	right, rightMarshalErr := MarshalSongResult(rightResult)
	if leftMarshalErr != nil || rightMarshalErr != nil {
		t.Fatalf("repeated exact replay song result canonical errors left=%v right=%v", leftMarshalErr, rightMarshalErr)
	}
	if !bytes.Equal(left, right) {
		t.Fatal("repeated exact replay song result was not byte-identical")
	}
	return first, second
}

func assertProviderRequestCounts(
	t *testing.T,
	scenario providerPrefixScenario,
	sekaipedia, moegirl, fandom bool,
) {
	t.Helper()
	want := []bool{sekaipedia, moegirl, fandom}
	for index, provider := range scenario.runtime.Order {
		count := scenario.transports[provider].(*fixtureRoundTripper).requestCount()
		if want[index] && count == 0 || !want[index] && count != 0 {
			t.Fatalf("provider %s request count=%d wantedCalled=%t", provider, count, want[index])
		}
	}
}
