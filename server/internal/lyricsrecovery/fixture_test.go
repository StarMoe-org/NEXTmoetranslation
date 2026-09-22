package lyricsrecovery

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
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

const fixtureCatalogDefaultPath = "/private/tmp/moesekai-lyrics-catalog-v18-20260731-704.db"

var fixtureCatalogPath = fixtureCatalogDefaultPath
var fixtureCatalogBindingValue lyricsextractionplan.RecoveryCatalogBinding

type fixtureRoundTripper struct {
	provider model.LyricsSourceProvider
	respond  func(*http.Request) ([]byte, error)

	mu       sync.Mutex
	active   int
	requests int
}

func (transport *fixtureRoundTripper) recoveryOfflineFixture() bool { return true }

func (transport *fixtureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.active++
	transport.requests++
	if transport.active != 1 {
		transport.mu.Unlock()
		return nil, errors.New("fixture observed concurrent provider requests")
	}
	transport.mu.Unlock()
	defer func() {
		transport.mu.Lock()
		transport.active--
		transport.mu.Unlock()
	}()
	if request == nil || request.Method != http.MethodGet || request.URL.Query().Get("maxlag") != "5" {
		return nil, errors.New("fixture received a noncanonical request")
	}
	body, err := transport.respond(request)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request,
	}, nil
}

func (transport *fixtureRoundTripper) requestCount() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.requests
}

func TestFixtureRecoveryPlanUsesCanonicalRecoverySourceSnapshotV2(t *testing.T) {
	root := privateRecoveryTempDir(t)
	parent := lyricsrootmanifest.Manifest{
		RootID: "fixture-parent-root", RootSHA256: strings.Repeat("d", 64),
		Scope: lyricsrootmanifest.ScopeBinding{ScopeID: "catalog-704"},
	}
	plan := fixtureRecoveryPlan(t, root, fixtureCatalogBinding(), parent)
	wantFiles := []lyricsextractionplan.SourceFileIdentity{{
		Path: "server/internal/lyricsrecovery/config.go", SizeBytes: 1, SHA256: strings.Repeat("f", 64),
	}}
	wantSHA, err := lyricsextractionplan.RecoverySourceSnapshotSHA256(wantFiles)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceSnapshot.Algorithm != lyricsextractionplan.RecoverySourceSnapshotAlgorithmV2 ||
		plan.SourceSnapshot.SHA256 != wantSHA || !reflect.DeepEqual(plan.SourceSnapshot.Files, wantFiles) {
		t.Fatalf("fixture recovery source snapshot=%+v", plan.SourceSnapshot)
	}
	if err := lyricsextractionplan.ValidateRecovery(plan); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineFixtureCanaryRealCatalogExactReplayAndCompactRoot(t *testing.T) {
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	catalogBinding := fixtureCatalogBinding()
	catalog, verification, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, catalogBinding)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if verification.RecordCount != 704 || len(catalog.MusicIDs()) != 704 {
		t.Fatalf("immutable catalog verification=%+v", verification)
	}

	ledger, err := lyricsacquisition.CreateLedger(ctx, filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	parent := fixtureParentRoot(t, ctx, root, ledger, catalogBinding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, catalogBinding, parent)
	planBody, err := lyricsextractionplan.MarshalRecoveryCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	planSHA, err := lyricsextractionplan.RecoveryCanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.MaxAttempts != plan.Execution.MaxAttempts || runtime.MediaWikiMaxlag != 5 ||
		len(runtime.Providers[0].ContributorAliases) != 1 {
		t.Fatalf("plan-derived runtime=%+v", runtime)
	}
	live := fixtureProviderTransports(t)
	session, err := NewAcquisitionSession(runtime, ledger, live)
	if err != nil {
		t.Fatal(err)
	}
	songs := make([]SongAcquisitionSet, 0, 2)
	for _, musicID := range plan.Scope.MusicIDs {
		identity, err := catalog.MusicIdentity(ctx, musicID)
		if err != nil {
			t.Fatal(err)
		}
		providers, progress, err := session.AcquireSong(ctx, musicID, identity)
		if err != nil {
			t.Fatal(err)
		}
		if len(providers) != 1 || len(progress) != 1 {
			t.Fatalf("music %d evaluated prefix providers=%d progress=%d", musicID, len(providers), len(progress))
		}
		if musicID == 2 {
			for _, provider := range providers {
				if provider.Provider == model.LyricsSourceProviderSekaipedia && provider.Status != lyricsprovideroutcome.StatusCandidate {
					t.Fatalf("Roki Sekaipedia acquisition terminal=%+v", provider)
				}
			}
		}
		songs = append(songs, SongAcquisitionSet{MusicID: musicID, Providers: providers})
	}
	if live[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper).requestCount() == 0 ||
		live[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		live[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatalf("complete Sekaipedia fixture crossed its provider stopping point")
	}
	sort.Slice(songs, func(left, right int) bool { return songs[left].MusicID < songs[right].MusicID })
	for _, song := range songs {
		for _, provider := range song.Providers {
			if err := validateProviderTerminal(provider); err != nil {
				t.Fatalf("music %d provider %s terminal: %v terminal=%+v", song.MusicID, provider.Provider, err, provider)
			}
		}
	}
	set, err := NewAcquisitionSet(plan.PlanID, planSHA, runtime.Order, songs)
	if err != nil {
		t.Fatalf("new acquisition set: %v songs=%+v", err, songs)
	}
	if err := ValidateAcquisitionSetAuthorization(
		set, plan.PlanID, planSHA, plan.Scope.MusicIDs, runtime.Order, runtime.ProviderMusicIDs,
	); err != nil {
		t.Fatal(err)
	}
	setBody, err := MarshalAcquisitionSet(set)
	if err != nil {
		t.Fatal(err)
	}
	if err := PublishAcquisitionSet(plan.Outputs.AcquisitionSet, set); err != nil {
		t.Fatal(err)
	}

	firstResults := make([]SongResult, 0, 2)
	for _, musicID := range plan.Scope.MusicIDs {
		identity, _ := catalog.MusicIdentity(ctx, musicID)
		ordered, err := set.OrderedProviders(musicID)
		if err != nil {
			t.Fatal(err)
		}
		first, err := ReplaySong(ctx, musicID, identity, plan.Versions.ProviderPolicy, runtime, ledger, ordered)
		if err != nil {
			t.Fatalf("first replay music %d: %v", musicID, err)
		}
		second, err := ReplaySong(ctx, musicID, identity, plan.Versions.ProviderPolicy, runtime, ledger, ordered)
		if err != nil {
			t.Fatalf("second replay music %d: %v", musicID, err)
		}
		firstResult, err := NewSongResult(first)
		if err != nil {
			t.Fatal(err)
		}
		secondResult, err := NewSongResult(second)
		if err != nil {
			t.Fatal(err)
		}
		firstBody, _ := MarshalSongResult(firstResult)
		secondBody, _ := MarshalSongResult(secondResult)
		if !bytes.Equal(firstBody, secondBody) {
			t.Fatalf("music %d exact replay was not byte-identical", musicID)
		}
		fulls := make([]*model.LyricsSourceFull, 0, len(firstResult.Renditions))
		if firstResult.SchemaVersion == SongResultSchemaVersionV3 {
			for index := range firstResult.Renditions {
				if firstResult.Renditions[index].Full != nil {
					fulls = append(fulls, firstResult.Renditions[index].Full)
				}
			}
		}
		if firstResult.State != lyricscontract.CoverageComplete ||
			firstResult.SchemaVersion != SongResultSchemaVersionV3 || firstResult.Full != nil ||
			len(firstResult.Renditions) < 2 || len(fulls) == 0 || len(firstResult.SelectedEvidence) != 2 {
			terminals := make([]string, len(first.Providers))
			for index, provider := range first.Providers {
				terminals[index] = fmt.Sprintf("%s:%s:%s:%s", provider.Outcome.Provider, provider.Outcome.Status,
					provider.Outcome.Diagnostic.ReasonCode, provider.Outcome.Diagnostic.Phase)
			}
			t.Fatalf("music %d composition boundary schema=%d state=%s legacyFull=%t renditions=%d fullSides=%d selectedEvidence=%d terminals=%v",
				musicID, firstResult.SchemaVersion, firstResult.State, firstResult.Full != nil,
				len(firstResult.Renditions), len(fulls), len(firstResult.SelectedEvidence), terminals)
		}
		for _, full := range fulls {
			for index, line := range full.Lines {
				if line.ID != fmt.Sprintf("full-%06d", index+1) {
					t.Fatalf("music %d unstable Full line ID %q", musicID, line.ID)
				}
			}
		}
		lowerResult := strings.ToLower(string(firstBody))
		for _, forbidden := range []string{"romaji", "romanization"} {
			if strings.Contains(lowerResult, forbidden) {
				t.Fatalf("music %d song result crossed the no-romanization boundary token=%s paths=%v",
					musicID, forbidden, jsonStringPathsContaining(firstBody, forbidden))
			}
		}
		if len(first.Providers) != 1 || first.Providers[0].Outcome.Status != lyricsprovideroutcome.StatusCandidate ||
			first.Providers[0].Artifact.Provider != model.LyricsSourceProviderSekaipedia {
			firstProvider := model.LyricsSourceProvider("")
			firstStatus := lyricsprovideroutcome.Status("")
			if len(first.Providers) > 0 {
				firstProvider = first.Providers[0].Artifact.Provider
				firstStatus = first.Providers[0].Outcome.Status
			}
			t.Fatalf("music %d provider outcome boundary count=%d firstProvider=%s firstStatus=%s",
				musicID, len(first.Providers), firstProvider, firstStatus)
		}
		for index := range first.Providers {
			left, _ := lyricsoutcomeartifact.MarshalCanonical(first.Providers[index].Artifact)
			right, _ := lyricsoutcomeartifact.MarshalCanonical(second.Providers[index].Artifact)
			if !bytes.Equal(left, right) {
				t.Fatalf("music %d provider %d artifact replay drifted", musicID, index)
			}
		}
		firstResults = append(firstResults, firstResult)
	}

	if err := lyricsoutcomeartifact.CreatePrivateDirectory(plan.Outputs.ProviderOutcomes); err != nil {
		t.Fatal(err)
	}
	if err := lyricsoutcomeartifact.CreatePrivateDirectory(plan.Outputs.SongResults); err != nil {
		t.Fatal(err)
	}
	for _, result := range firstResults {
		ordered, _ := set.OrderedProviders(result.MusicID)
		identity, _ := catalog.MusicIdentity(ctx, result.MusicID)
		replayed, err := ReplaySong(ctx, result.MusicID, identity, plan.Versions.ProviderPolicy, runtime, ledger, ordered)
		if err != nil {
			t.Fatal(err)
		}
		for _, provider := range replayed.Providers {
			if _, err := lyricsoutcomeartifact.PublishCreateExclusive(plan.Outputs.ProviderOutcomes, provider.Artifact); err != nil {
				t.Fatal(err)
			}
		}
		name, _ := SongResultFileName(result)
		if err := PublishSongResult(filepath.Join(plan.Outputs.SongResults, name), result); err != nil {
			t.Fatal(err)
		}
	}
	outcomeEntries, _ := os.ReadDir(plan.Outputs.ProviderOutcomes)
	if len(outcomeEntries) != 2 {
		t.Fatalf("independent evaluated-prefix outcome artifact count=%d, want 2", len(outcomeEntries))
	}

	manifest, err := AssembleAndPublish(ctx, plan, planSHA, ledger, firstResults, &parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := lyricsrootmanifest.ValidateAgainstParent(manifest, parent); err != nil {
		t.Fatal(err)
	}
	resolver, err := lyricsevidencepack.OpenResolver(plan.Outputs.EvidencePack)
	if err != nil {
		t.Fatal(err)
	}
	selected := exactSelectedUnion(firstResults)
	if err := resolver.ValidateSelected(selected); err != nil {
		t.Fatal(err)
	}
	if manifest.Catalog.RecordCount != 704 || len(manifest.Songs) != 2 || manifest.EvidencePack.ItemCount != len(selected) ||
		manifest.Scope.Kind != lyricsrootmanifest.ScopePartial {
		t.Fatalf("compact partial root=%+v", manifest)
	}
	rootBody, _ := lyricsrootmanifest.MarshalCanonical(manifest)
	if len(rootBody) >= len(planBody)+len(setBody)+64<<10 {
		t.Fatalf("compact root unexpectedly monolithic: root=%d plan=%d set=%d", len(rootBody), len(planBody), len(setBody))
	}
	if strings.Contains(strings.ToLower(string(rootBody)), "romaji") || strings.Contains(strings.ToLower(string(rootBody)), "romanization") {
		t.Fatal("compact root crossed the no-romanization boundary")
	}
	if err := os.WriteFile(filepath.Join(plan.Outputs.ProviderOutcomes, "orphan.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedRecoveryArtifacts(plan, firstResults); err == nil {
		t.Fatal("orphan provider outcome entry was accepted after exact root assembly")
	}
}

func TestRecoveryExactSongIDDoesNotRequireContributorAliasForRealCatalogRokiFixture(t *testing.T) {
	ctx := t.Context()
	root := privateRecoveryTempDir(t)
	binding := fixtureCatalogBinding()
	catalog, _, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	ledger, err := lyricsacquisition.CreateLedger(ctx, filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, catalog.MusicIDs())
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	plan.Providers.Configurations[0].ContributorAliases = []lyricsextractionplan.RecoveryContributorAlias{}
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := catalog.MusicIdentity(ctx, 2)
	providers, _, err := AcquireSong(ctx, 2, identity, runtime, ledger, fixtureProviderTransports(t))
	if err != nil {
		t.Fatal(err)
	}
	var sekaipedia ProviderAcquisitionSet
	for _, provider := range providers {
		if provider.Provider == model.LyricsSourceProviderSekaipedia {
			sekaipedia = provider
		}
	}
	if sekaipedia.Status != lyricsprovideroutcome.StatusCandidate || sekaipedia.ReasonCode != lyricsprovideroutcome.ReasonCandidate {
		t.Fatalf("exact-ID Roki without contributor alias terminal=%+v", sekaipedia)
	}
}

func TestAllowsFallbackUsesOnlyClosedReviewedReasons(t *testing.T) {
	outcome := func(provider model.LyricsSourceProvider, status lyricsprovideroutcome.Status, reason lyricsprovideroutcome.ReasonCode,
		phase lyricsprovideroutcome.Phase, counts lyricsprovideroutcome.Counts) lyricsprovideroutcome.Outcome[lyricssource.Candidate] {
		result, err := lyricsprovideroutcome.New(provider, status, []lyricssource.Candidate{}, lyricsprovideroutcome.Diagnostic{
			Provider: provider, Phase: phase, ReasonCode: reason, Counts: counts,
			AcquisitionRefs: []model.LyricsSourceIndexEvidenceRef{},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if !AllowsFallback(model.LyricsSourceProviderSekaipedia, outcome(
		model.LyricsSourceProviderSekaipedia, lyricsprovideroutcome.StatusNoMatch,
		lyricsprovideroutcome.ReasonNoSearchHits, lyricsprovideroutcome.PhaseResolveTargets,
		lyricsprovideroutcome.Counts{NoMatch: 1},
	)) {
		t.Fatal("reviewed Sekaipedia absence did not allow fallback")
	}
	if AllowsFallback(model.LyricsSourceProviderSekaipedia, outcome(
		model.LyricsSourceProviderSekaipedia, lyricsprovideroutcome.StatusTransportError,
		lyricsprovideroutcome.ReasonTransport, lyricsprovideroutcome.PhaseAcquireTarget,
		lyricsprovideroutcome.Counts{TransportErrors: 1},
	)) {
		t.Fatal("transport failure was collapsed into fallback absence")
	}
}

func TestOfflinePolicyTreatsExplicitGameOnlyAsAClosedAuthoritativeRendition(t *testing.T) {
	sekaipedia := recoveryReplayNoMatch(t, model.LyricsSourceProviderSekaipedia)
	game := recoveryReplayCandidate(
		t, model.LyricsSourceProviderMoegirl, "outcome:moegirl:42:game", "game-sekai",
		recoveryFixedRevision("sekai", []string{"二"}, model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid),
		"evidence:moegirl:game", "1",
	)
	full := recoveryReplayCandidate(
		t, model.LyricsSourceProviderVocaloidFandom, "outcome:vocaloid_fandom:42:full", "full-vocaloid",
		recoveryFixedRevision("vocaloid", []string{"一", "二", "三"}, model.LyricsSourceVersionReasonUntaggedFullOnly),
		"evidence:fandom:full", "2",
	)
	composed, err := applyOfflinePolicyAndComposition(ReplayResult{
		MusicID: 42, Providers: []ProviderReplay{sekaipedia, game},
	})
	if err != nil {
		t.Fatal(err)
	}
	if composed.Composition == nil || composed.Composition.Game == nil || len(composed.Composition.Full.Lines) != 0 ||
		composed.Composition.Components.GameText != game.Artifact.OutcomeID ||
		composed.Composition.Components.FullText != "" ||
		composed.Composition.Components.VersionEvidence != game.Artifact.OutcomeID ||
		!reflect.DeepEqual(composed.Composition.SelectedSourceKeys, []string{game.Artifact.OutcomeID}) ||
		len(composed.Selected) != 1 || len(composed.Components.GameText) != 1 ||
		composed.Components.GameText[0].EvidenceID != game.EvidenceRefs[0].EvidenceID ||
		len(composed.Components.VersionEvidence) != 1 ||
		composed.Components.VersionEvidence[0].EvidenceID != game.EvidenceRefs[0].EvidenceID {
		t.Fatalf("closed Game-only composition=%+v selected=%+v components=%+v", composed.Composition, composed.Selected, composed.Components)
	}
	if _, err := applyOfflinePolicyAndComposition(ReplayResult{
		MusicID: 42, Providers: []ProviderReplay{sekaipedia, game, full},
	}); err == nil {
		t.Fatal("provider after the explicit Game-only stopping point was accepted")
	}

	moegirlFull := recoveryReplayCandidate(
		t, model.LyricsSourceProviderMoegirl, "outcome:moegirl:42:complete", "full-sekai",
		recoveryFixedRevision("sekai", []string{"先行"}, model.LyricsSourceVersionReasonUntaggedFullOnly),
		"evidence:moegirl:complete", "3",
	)
	laterFull := recoveryReplayCandidate(
		t, model.LyricsSourceProviderVocaloidFandom, "outcome:vocaloid_fandom:42:later", "full-sekai",
		recoveryFixedRevision("sekai", []string{"上書き"}, model.LyricsSourceVersionReasonUntaggedFullOnly),
		"evidence:fandom:later", "4",
	)
	selected, err := applyOfflinePolicyAndComposition(ReplayResult{
		MusicID: 42, Providers: []ProviderReplay{sekaipedia, moegirlFull},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Composition == nil || selected.Composition.Components.FullText != moegirlFull.Artifact.OutcomeID ||
		!reflect.DeepEqual(selected.Composition.SelectedSourceKeys, []string{moegirlFull.Artifact.OutcomeID}) ||
		len(selected.Selected) != 1 || selected.Selected[0].EvidenceID != moegirlFull.EvidenceRefs[0].EvidenceID ||
		selected.Composition.Full.Lines[0].Text != "先行" {
		t.Fatalf("complete earlier provider composition=%+v selected=%+v", selected.Composition, selected.Selected)
	}
	if _, err := applyOfflinePolicyAndComposition(ReplayResult{
		MusicID: 42, Providers: []ProviderReplay{sekaipedia, moegirlFull, laterFull},
	}); err == nil {
		t.Fatal("provider after the complete composition stopping point was accepted")
	}
}

func recoveryReplayNoMatch(t *testing.T, provider model.LyricsSourceProvider) ProviderReplay {
	t.Helper()
	outcome, err := lyricsprovideroutcome.New(
		provider, lyricsprovideroutcome.StatusNoMatch, []lyricssource.Candidate{},
		lyricsprovideroutcome.Diagnostic{
			Provider: provider, Phase: lyricsprovideroutcome.PhaseResolveTargets,
			ReasonCode:      lyricsprovideroutcome.ReasonNoSearchHits,
			Counts:          lyricsprovideroutcome.Counts{NoMatch: 1},
			AcquisitionRefs: []model.LyricsSourceIndexEvidenceRef{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return ProviderReplay{Outcome: outcome, Artifact: lyricsoutcomeartifact.Artifact{Provider: provider}}
}

func recoveryReplayCandidate(
	t *testing.T,
	provider model.LyricsSourceProvider,
	outcomeID string,
	renditionKey string,
	fixed lyricssource.FixedRevision,
	evidenceID string,
	digit string,
) ProviderReplay {
	t.Helper()
	ref := lyricscontract.EvidenceRef{
		Provider: provider, AcquisitionID: strings.Repeat(digit, 64), EvidenceID: evidenceID,
		SHA256: strings.Repeat("a", 64), EnvelopeSHA256: strings.Repeat("b", 64),
	}
	outcome, err := lyricsprovideroutcome.New(
		provider, lyricsprovideroutcome.StatusCandidate,
		[]lyricssource.Candidate{{Provider: provider}},
		lyricsprovideroutcome.Diagnostic{
			Provider: provider, Phase: lyricsprovideroutcome.PhaseFinalize,
			ReasonCode:      lyricsprovideroutcome.ReasonCandidate,
			Counts:          lyricsprovideroutcome.Counts{Targets: 1, Evaluated: 1, Candidates: 1},
			AcquisitionRefs: []model.LyricsSourceIndexEvidenceRef{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return ProviderReplay{
		Outcome: outcome,
		Artifact: lyricsoutcomeartifact.Artifact{
			Provider: provider, OutcomeID: outcomeID,
			Candidate: &lyricsoutcomeartifact.CandidateIdentity{RenditionKey: renditionKey},
		},
		Fixed: &fixed, EvidenceRefs: []lyricscontract.EvidenceRef{ref},
	}
}

func recoveryFixedRevision(
	versionKind string,
	texts []string,
	reason model.LyricsSourceVersionReasonCode,
) lyricssource.FixedRevision {
	extraction := lyricssource.Extraction{
		Version:    lyricssource.LyricsVersion{Kind: versionKind, Label: versionKind},
		Performers: []lyricssource.Performer{}, Lines: make([]lyricssource.StructuredLine, len(texts)),
	}
	for index, text := range texts {
		extraction.Lines[index] = lyricssource.StructuredLine{
			Japanese: text,
			Segments: []lyricssource.LyricsSegment{{
				Text: text, PerformerIDs: []string{}, Ruby: []lyricssource.RubySpan{{Text: text}},
			}},
			TrailingPerformerIDs: []string{},
		}
	}
	return lyricssource.FixedRevision{VersionReason: reason, Extraction: extraction}
}

func jsonStringPathsContaining(body []byte, token string) []string {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return []string{"<invalid-json>"}
	}
	var paths []string
	var walk func(any, string)
	walk = func(current any, path string) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if strings.Contains(strings.ToLower(key), token) {
					paths = append(paths, path+"."+key+"<key>")
				}
				walk(child, path+"."+key)
			}
		case []any:
			for index, child := range typed {
				walk(child, fmt.Sprintf("%s[%d]", path, index))
			}
		case string:
			if strings.Contains(strings.ToLower(typed), token) {
				paths = append(paths, path)
			}
		}
	}
	walk(value, "$")
	sort.Strings(paths)
	return paths
}

func exactSelectedUnion(results []SongResult) []lyricscontract.EvidenceRef {
	byID := make(map[string]lyricscontract.EvidenceRef)
	for _, result := range results {
		for _, ref := range result.SelectedEvidence {
			byID[ref.EvidenceID] = ref
		}
	}
	refs := make([]lyricscontract.EvidenceRef, 0, len(byID))
	for _, ref := range byID {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].EvidenceID < refs[right].EvidenceID })
	return refs
}

func fixtureProviderTransports(t *testing.T) map[model.LyricsSourceProvider]http.RoundTripper {
	t.Helper()
	list := mustFixture(t, "sekaipedia-list-335193.json")
	roki := mustFixture(t, "sekaipedia-roki-330574.json")
	journey := mustFixture(t, "sekaipedia-journey-326737.json")
	moegirlBody := []byte("* [[Other#Other|Other]]\n")
	moegirl := mediaWikiPageResponse(t, 488279, 8073049,
		"世界计划 彩色舞台 feat. 初音未来/歌曲", moegirlBody)
	return map[model.LyricsSourceProvider]http.RoundTripper{
		model.LyricsSourceProviderSekaipedia: &fixtureRoundTripper{provider: model.LyricsSourceProviderSekaipedia, respond: func(request *http.Request) ([]byte, error) {
			query := request.URL.Query()
			if request.URL.Host != "www.sekaipedia.org" {
				return nil, fmt.Errorf("unexpected Sekaipedia fixture host %s", request.URL.Host)
			}
			switch {
			case query.Get("revids") == "335193":
				return list, nil
			case query.Get("titles") == "Journey" || query.Get("revids") == "326737" || query.Get("pageids") == "28040":
				return journey, nil
			default:
				return roki, nil
			}
		}},
		model.LyricsSourceProviderMoegirl: &fixtureRoundTripper{provider: model.LyricsSourceProviderMoegirl, respond: func(request *http.Request) ([]byte, error) {
			if request.URL.Host != "moegirl.icu" || request.URL.Query().Get("revids") != "8073049" {
				return nil, errors.New("unexpected Moegirl fixture request")
			}
			return moegirl, nil
		}},
		model.LyricsSourceProviderVocaloidFandom: &fixtureRoundTripper{provider: model.LyricsSourceProviderVocaloidFandom, respond: func(request *http.Request) ([]byte, error) {
			if request.URL.Host != "vocaloid.fandom.com" || request.URL.Query().Get("generator") != "search" {
				return nil, errors.New("unexpected Fandom fixture request")
			}
			if strings.Contains(request.URL.Query().Get("gsrsearch"), "Journey") {
				return nil, errors.New("deterministic fixture transport failure")
			}
			return []byte(`{"batchcomplete":true}`), nil
		}},
	}
}

func withSekaipediaSongIDMismatch(
	t *testing.T,
	transports map[model.LyricsSourceProvider]http.RoundTripper,
) map[model.LyricsSourceProvider]http.RoundTripper {
	t.Helper()
	original, ok := transports[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper)
	if !ok || original == nil {
		t.Fatal("Sekaipedia fixture transport is missing")
	}
	transports[model.LyricsSourceProviderSekaipedia] = &fixtureRoundTripper{
		provider: model.LyricsSourceProviderSekaipedia,
		respond: func(request *http.Request) ([]byte, error) {
			body, err := original.respond(request)
			if err != nil || request.URL.Query().Get("revids") == "335193" {
				return body, err
			}
			return sekaipediaSongIDMismatchResponse(t, body), nil
		},
	}
	return transports
}

func sekaipediaSongIDMismatchResponse(t *testing.T, body []byte) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	query, ok := envelope["query"].(map[string]any)
	if !ok {
		t.Fatal("Sekaipedia mismatch fixture query is missing")
	}
	pages, ok := query["pages"].([]any)
	if !ok || len(pages) != 1 {
		t.Fatal("Sekaipedia mismatch fixture page boundary is invalid")
	}
	page, ok := pages[0].(map[string]any)
	if !ok {
		t.Fatal("Sekaipedia mismatch fixture page is invalid")
	}
	revisions, ok := page["revisions"].([]any)
	if !ok || len(revisions) != 1 {
		t.Fatal("Sekaipedia mismatch fixture revision boundary is invalid")
	}
	revision, ok := revisions[0].(map[string]any)
	if !ok {
		t.Fatal("Sekaipedia mismatch fixture revision is invalid")
	}
	slots, ok := revision["slots"].(map[string]any)
	if !ok {
		t.Fatal("Sekaipedia mismatch fixture slots are invalid")
	}
	main, ok := slots["main"].(map[string]any)
	if !ok {
		t.Fatal("Sekaipedia mismatch fixture main slot is invalid")
	}
	content, ok := main["content"].(string)
	if !ok || strings.Count(content, "| song id   = 2") != 1 {
		t.Fatal("Sekaipedia mismatch fixture exact song ID binding is absent or ambiguous")
	}
	content = strings.Replace(content, "| song id   = 2", "| song id   = 999", 1)
	digest := sha1.Sum([]byte(content))
	main["content"] = content
	revision["sha1"] = hex.EncodeToString(digest[:])
	result, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mediaWikiPageResponse(t *testing.T, pageID, revisionID int, title string, content []byte) []byte {
	t.Helper()
	digest := sha1.Sum(content)
	body, err := json.Marshal(map[string]any{
		"batchcomplete": true,
		"query": map[string]any{"pages": []any{map[string]any{
			"pageid": pageID, "ns": 0, "title": title,
			"revisions": []any{map[string]any{
				"revid": revisionID, "sha1": hex.EncodeToString(digest[:]),
				"slots": map[string]any{"main": map[string]any{"contentmodel": "wikitext", "contentformat": "text/x-wiki", "content": string(content)}},
			}},
			"categories": []any{},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	path, err := recoveryPackageFixturePath(name)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func fixtureCatalogBinding() lyricsextractionplan.RecoveryCatalogBinding {
	binding := fixtureCatalogBindingValue
	binding.Path = fixtureCatalogPath
	return binding
}

func fixtureRecoveryPlan(t *testing.T, root string, catalog lyricsextractionplan.RecoveryCatalogBinding,
	parent lyricsrootmanifest.Manifest) lyricsextractionplan.RecoveryPlan {
	t.Helper()
	floors := lyricsextractionplan.CompiledSafetyFloors()
	list := fixtureAuthority(t, lyricsextractionplan.ProviderSekaipedia, 268, 335193, "List of songs",
		"b216a827f88c59f5e954a120027832fe9cd74413", "aaddff2922548aab7e522124ff2bad86427501930d549c9d94c9b4e473c35f92",
		"c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd")
	moegirlBody := []byte("* [[Other#Other|Other]]\n")
	moegirlDigest := sha1.Sum(moegirlBody)
	moegirl := fixtureAuthority(t, lyricsextractionplan.ProviderMoegirl, 488279, 8073049,
		"世界计划 彩色舞台 feat. 初音未来/歌曲", hex.EncodeToString(moegirlDigest[:]), "", "")
	sourceFiles := []lyricsextractionplan.SourceFileIdentity{{
		Path: "server/internal/lyricsrecovery/config.go", SizeBytes: 1, SHA256: strings.Repeat("f", 64),
	}}
	sourceSnapshotSHA, err := lyricsextractionplan.RecoverySourceSnapshotSHA256(sourceFiles)
	if err != nil {
		t.Fatal(err)
	}
	return lyricsextractionplan.RecoveryPlan{
		SchemaVersion:     lyricsextractionplan.RecoverySchemaVersionV2,
		CanonicalEncoding: lyricsextractionplan.RecoveryCanonicalEncodingV2,
		DigestAlgorithm:   lyricsextractionplan.RecoveryDigestAlgorithmV2,
		PlanID:            "fixture-recovery-v2", CreatedAt: "2026-08-02T00:00:00Z", Catalog: catalog,
		SourceSnapshot: lyricsextractionplan.SourceSnapshot{
			Algorithm: lyricsextractionplan.RecoverySourceSnapshotAlgorithmV2, CapturedAt: "2026-08-02T00:00:00Z",
			Files: sourceFiles, SHA256: sourceSnapshotSHA,
		},
		Scope: lyricsextractionplan.RecoveryScopeBinding{
			Kind: lyricsextractionplan.RecoveryScopePartial, ScopeID: parent.Scope.ScopeID, MusicIDs: []int{2, 235},
			SupersedesRootID: parent.RootID, SupersedesRootSHA256: parent.RootSHA256,
		},
		Providers: lyricsextractionplan.RecoveryProviderConfiguration{
			Order: []lyricsextractionplan.Provider{lyricsextractionplan.ProviderSekaipedia, lyricsextractionplan.ProviderMoegirl, lyricsextractionplan.ProviderVocaloidFandom},
			Configurations: []lyricsextractionplan.RecoveryProviderPlan{
				{Provider: lyricsextractionplan.ProviderSekaipedia, Mode: lyricsextractionplan.ProviderModeActive,
					CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
					Authorities: []lyricsextractionplan.FixedAuthority{list}, ContributorAliases: []lyricsextractionplan.RecoveryContributorAlias{
						{MusicID: 2, CatalogContributor: "みきとP", ProviderContributor: "MikitoP"},
					}, SekaipediaTargets: []lyricsextractionplan.RecoverySekaipediaPageTarget{
						{MusicID: 2, PageTitle: "Roki"}, {MusicID: 235, PageTitle: "Journey"},
					}},
				{Provider: lyricsextractionplan.ProviderMoegirl, Mode: lyricsextractionplan.ProviderModeActive,
					CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
					Authorities: []lyricsextractionplan.FixedAuthority{moegirl}, ContributorAliases: []lyricsextractionplan.RecoveryContributorAlias{}},
				{Provider: lyricsextractionplan.ProviderVocaloidFandom, Mode: lyricsextractionplan.ProviderModeActive,
					CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
					Authorities: []lyricsextractionplan.FixedAuthority{}, ContributorAliases: []lyricsextractionplan.RecoveryContributorAlias{}},
			},
		},
		Versions: lyricsextractionplan.CompiledRecoveryVersions(),
		Execution: lyricsextractionplan.RecoveryExecutionSettings{
			MaxAttempts: 1, RequestTimeoutMillis: 30_000, RetryDelayMillis: floors.RetryDelayMillis,
			ProviderResponseBytes:    lyricsextractionplan.CompiledHardCeilings().ProviderResponseBytes,
			MaxActualNetworkInFlight: 1, MediaWikiMaxlag: 5, LiveCanaryMusicIDs: []int{2},
		},
		Outputs: lyricsextractionplan.RequiredRecoveryOutputs([6]string{
			filepath.Join(root, "ledger"), filepath.Join(root, "acquisition-set.json"),
			filepath.Join(root, "provider-outcomes"), filepath.Join(root, "song-results"),
			filepath.Join(root, "evidence-pack"), filepath.Join(root, "root.json"),
		}),
		Deployment: lyricsextractionplan.RequiredDeploymentPolicy(),
	}
}

func fixtureAuthority(t *testing.T, provider lyricsextractionplan.Provider, pageID, revisionID int,
	title, sha1Value, contentSHA256, rawSHA string) lyricsextractionplan.FixedAuthority {
	t.Helper()
	authority := lyricsextractionplan.FixedAuthority{
		Disposition: lyricsextractionplan.AuthorityActive, Role: lyricsextractionplan.AuthorityRoleSongIndex,
		PageID: pageID, RevisionID: revisionID, SHA1: sha1Value, ContentSHA256: contentSHA256, RawSHA256: rawSHA, Title: title,
		CanonicalURL: lyricsextractionplan.FixedAuthorityCanonicalURL(provider, title, revisionID),
	}
	if provider == lyricsextractionplan.ProviderSekaipedia {
		authority.CaptureProfile = lyricsextractionplan.CaptureProfileMediaWikiAPIRevisionResponseV1
		authority.RevisionTimestamp = "2026-07-27T16:29:13Z"
	} else {
		authority.CaptureProfile = lyricsextractionplan.CaptureProfileMediaWikiRevisionContentV1
	}
	var err error
	authority.EvidenceID, err = lyricsextractionplan.FixedAuthorityEvidenceID(provider, authority.Role, pageID, revisionID, title)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func fixtureParentRoot(t *testing.T, ctx context.Context, root string, ledger *lyricsacquisition.Ledger,
	catalog lyricsextractionplan.RecoveryCatalogBinding, musicIDs []int) lyricsrootmanifest.Manifest {
	t.Helper()
	packPath := filepath.Join(root, "parent-pack")
	if _, err := lyricsevidencepack.Build(ctx, packPath, []lyricscontract.EvidenceRef{}, ledger); err != nil {
		t.Fatal(err)
	}
	resolver, err := lyricsevidencepack.OpenResolver(packPath)
	if err != nil {
		t.Fatal(err)
	}
	songs := make([]lyricsrootmanifest.SongResultRef, len(musicIDs))
	for index, musicID := range musicIDs {
		songs[index] = lyricsrootmanifest.SongResultRef{
			MusicID: musicID, State: lyricscontract.CoverageMissing,
			ResultSHA256: fmt.Sprintf("%064x", musicID), ProviderOutcomes: []lyricsrootmanifest.ProviderOutcomeRef{},
			SelectedEvidence: []lyricscontract.EvidenceRef{},
		}
	}
	parent, err := lyricsrootmanifest.Assemble(lyricsrootmanifest.AssemblyRequest{
		RootID: "fixture-parent-root", Scope: lyricsrootmanifest.ScopeBinding{Kind: lyricsrootmanifest.ScopeFinal, ScopeID: "catalog-704"},
		Catalog: lyricsrootmanifest.CatalogBinding{
			SchemaVersion: catalog.SchemaVersion, RuntimeSchemaVersion: catalog.RuntimeSchemaVersion,
			RecordCount: catalog.RecordCount, IdentityPolicyVersion: catalog.IdentityPolicyVersion,
			SourceSHA256: catalog.SourceSHA256, IdentitySHA256: catalog.IdentitySHA256, MusicIDsSHA256: catalog.MusicIDsSHA256,
		},
		Plan: lyricsrootmanifest.PlanBinding{PlanID: "fixture-parent-plan", SHA256: strings.Repeat("e", 64)}, Songs: songs,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return parent
}
