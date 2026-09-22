package lyricsrootmanifest

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

const currentCatalogCount = 704

type rootSliceSource struct {
	items []lyricssource.IndexEvidence
}

func (source rootSliceSource) ReplayByAcquisitionID(
	_ context.Context,
	acquisitionID lyricsacquisition.AcquisitionID,
) (lyricsacquisition.Acquisition, error) {
	for _, item := range source.items {
		acquired := rootTestAcquisition(item)
		if acquired.AcquisitionID == acquisitionID {
			return acquired, nil
		}
	}
	return lyricsacquisition.Acquisition{}, lyricsacquisition.ErrAcquisitionNotFound
}

func TestProviderOutcomeRefsPreservePlanOrderWithoutStringSorting(t *testing.T) {
	refs := []ProviderOutcomeRef{
		{Provider: model.LyricsSourceProviderSekaipedia, OutcomeID: "outcome:sekaipedia:1", SHA256: strings.Repeat("1", 64)},
		{Provider: model.LyricsSourceProviderMoegirl, OutcomeID: "outcome:moegirl:1", SHA256: strings.Repeat("2", 64)},
		{Provider: model.LyricsSourceProviderVocaloidFandom, OutcomeID: "outcome:vocaloid-fandom:1", SHA256: strings.Repeat("3", 64)},
	}
	if err := validateProviderOutcomes(refs); err != nil {
		t.Fatalf("plan-ordered provider outcome refs were rejected by string ordering: %v", err)
	}
	duplicate := append([]ProviderOutcomeRef(nil), refs...)
	duplicate[1] = ProviderOutcomeRef{
		Provider: refs[0].Provider, OutcomeID: "outcome:sekaipedia:2", SHA256: strings.Repeat("4", 64),
	}
	if err := validateProviderOutcomes(duplicate); err == nil {
		t.Fatal("duplicate evaluated provider outcome refs were accepted")
	}
}

func rootTestEvidence(t *testing.T, index int) lyricssource.IndexEvidence {
	t.Helper()
	pageID := 3000 + index
	revisionID := 4000 + index
	title := fmt.Sprintf("Root Evidence %06d", index)
	raw := []byte(fmt.Sprintf("root-exact-evidence-%06d", index))
	rawDigest := sha256.Sum256(raw)
	rawSHA256 := hex.EncodeToString(rawDigest[:])
	contentSHA1 := sha1.Sum(raw)
	fetchedAt := "2026-08-01T00:00:00Z"
	canonical := url.URL{Scheme: "https", Host: "vocaloid.fandom.com", Path: "/wiki/" + strings.ReplaceAll(title, " ", "_")}
	query := canonical.Query()
	query.Set("oldid", fmt.Sprintf("%d", revisionID))
	canonical.RawQuery = query.Encode()
	item := lyricssource.IndexEvidence{
		EvidenceID: lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			model.LyricsSourceProviderVocaloidFandom, fmt.Sprintf("fetch:vocaloid-fandom:%d", pageID), fetchedAt, rawSHA256,
		),
		SHA256: rawSHA256, Kind: lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: model.LyricsSourceProviderVocaloidFandom, Origin: model.LyricsSourceOriginVocaloidFandom,
		PageID: pageID, RevisionID: revisionID, MediaWikiSHA1: hex.EncodeToString(contentSHA1[:]),
		Title: title, CanonicalURL: canonical.String(), Categories: []string{"Lyrics"}, FetchedAt: fetchedAt,
		Raw: raw, RawSHA256: rawSHA256,
	}
	if err := lyricssource.ValidateIndexEvidenceEnvelope(item); err != nil {
		t.Fatalf("root test evidence is invalid: %v", err)
	}
	return item
}

func rootTestAcquisition(item lyricssource.IndexEvidence) lyricsacquisition.Acquisition {
	envelope, _ := json.Marshal(item)
	envelopeDigest := sha256.Sum256(envelope)
	acquisitionDigest := sha256.Sum256([]byte("root-test-acquisition-v1\x00" + item.EvidenceID))
	return lyricsacquisition.Acquisition{
		AcquisitionID: lyricsacquisition.AcquisitionID(hex.EncodeToString(acquisitionDigest[:])),
		Request:       lyricsacquisition.Request{Provider: string(item.Provider)},
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: item.EvidenceID, Raw: append([]byte(nil), item.Raw...), RawSHA256: item.RawSHA256,
		},
		EvidenceEnvelope: append([]byte(nil), envelope...), EvidenceEnvelopeSHA256: hex.EncodeToString(envelopeDigest[:]),
		ReplayOnly: true,
	}
}

func rootEvidenceRef(item lyricssource.IndexEvidence) lyricscontract.EvidenceRef {
	ref, err := lyricsevidencepack.EvidenceRefFromAcquisition(rootTestAcquisition(item))
	if err != nil {
		panic(err)
	}
	return ref
}

func rootResolver(t *testing.T, items ...lyricssource.IndexEvidence) *lyricsevidencepack.Resolver {
	t.Helper()
	refs := make([]lyricscontract.EvidenceRef, len(items))
	for index, item := range items {
		refs[index] = rootEvidenceRef(item)
	}
	directory := filepath.Join(canonicalRootTestBase(t), "pack")
	if _, err := lyricsevidencepack.Build(context.Background(), directory, refs, rootSliceSource{items: items}); err != nil {
		t.Fatal(err)
	}
	resolver, err := lyricsevidencepack.OpenResolver(directory)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func sequentialMusicIDs(first, count int) []int {
	musicIDs := make([]int, count)
	for index := range musicIDs {
		musicIDs[index] = first + index
	}
	return musicIDs
}

func requestForMusicIDs(kind ScopeKind, catalogMusicIDs, songMusicIDs []int) AssemblyRequest {
	musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256(catalogMusicIDs)
	if err != nil {
		panic(err)
	}
	rootID := "lyrics-root-001"
	if kind != ScopeFinal {
		rootID = "lyrics-root-002"
	}
	request := AssemblyRequest{
		RootID: rootID,
		Scope:  ScopeBinding{Kind: kind, ScopeID: "catalog-scope-001"},
		Catalog: CatalogBinding{
			SchemaVersion: 18, RuntimeSchemaVersion: 23, RecordCount: len(catalogMusicIDs),
			IdentityPolicyVersion: "lyrics-catalog-identity-v1",
			SourceSHA256:          strings.Repeat("a", 64), IdentitySHA256: strings.Repeat("b", 64),
			MusicIDsSHA256: musicIDsSHA256,
		},
		Plan:  PlanBinding{PlanID: "plan-001", SHA256: strings.Repeat("c", 64)},
		Songs: make([]SongResultRef, len(songMusicIDs)),
	}
	if kind != ScopeFinal {
		request.Scope.SupersedesRootID = "lyrics-root-001"
		request.Scope.SupersedesRootSHA256 = strings.Repeat("d", 64)
	}
	for index, musicID := range songMusicIDs {
		request.Songs[index] = SongResultRef{
			MusicID: musicID, State: lyricscontract.CoverageMissing, ResultSHA256: digestText(fmt.Sprintf("result-%d", musicID)),
			ProviderOutcomes: []ProviderOutcomeRef{}, SelectedEvidence: []SelectedEvidenceRef{},
		}
	}
	return request
}

func baseRequest(kind ScopeKind, count int) AssemblyRequest {
	catalogMusicIDs := sequentialMusicIDs(1, currentCatalogCount)
	return requestForMusicIDs(kind, catalogMusicIDs, catalogMusicIDs[:count])
}

func bindToParent(request *AssemblyRequest, parent Manifest) {
	request.Scope.SupersedesRootID = parent.RootID
	request.Scope.SupersedesRootSHA256 = parent.RootSHA256
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestFinalRootUsesBoundedCatalogCountAndOrderedMusicIDBinding(t *testing.T) {
	resolver := rootResolver(t)
	request := baseRequest(ScopeFinal, currentCatalogCount)
	manifest, err := Assemble(request, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Coverage.Total != currentCatalogCount || manifest.Coverage.Missing != currentCatalogCount ||
		manifest.Coverage.UniqueEvidenceCount != 0 || len(manifest.Songs) != currentCatalogCount {
		t.Fatalf("current final root coverage=%+v songs=%d", manifest.Coverage, len(manifest.Songs))
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCanonical(body)
	if err != nil || !reflect.DeepEqual(decoded, manifest) {
		t.Fatalf("decoded final root differs: err=%v", err)
	}

	tooShort := baseRequest(ScopeFinal, currentCatalogCount-1)
	if _, err := Assemble(tooShort, resolver); err == nil || !strings.Contains(err.Error(), "record count") {
		t.Fatalf("short final error=%v", err)
	}
	duplicate := baseRequest(ScopeFinal, currentCatalogCount)
	duplicate.Songs[1].MusicID = duplicate.Songs[0].MusicID
	if _, err := Assemble(duplicate, resolver); err == nil || !strings.Contains(err.Error(), "song result") {
		t.Fatalf("duplicate music ID error=%v", err)
	}
	outOfOrder := baseRequest(ScopeFinal, currentCatalogCount)
	outOfOrder.Songs[1].MusicID, outOfOrder.Songs[2].MusicID = outOfOrder.Songs[2].MusicID, outOfOrder.Songs[1].MusicID
	if _, err := Assemble(outOfOrder, resolver); err == nil {
		t.Fatal("out-of-order final music IDs were accepted")
	}
	mismatchedDigest := baseRequest(ScopeFinal, currentCatalogCount)
	otherMusicIDs := sequentialMusicIDs(1, currentCatalogCount)
	otherMusicIDs[len(otherMusicIDs)-1] += 100
	mismatchedDigest.Catalog.MusicIDsSHA256, err = lyricscontract.OrderedMusicIDsSHA256(otherMusicIDs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Assemble(mismatchedDigest, resolver); err == nil || !strings.Contains(err.Error(), "ordered music IDs") {
		t.Fatalf("mismatched ordered ID digest error=%v", err)
	}

	overMaximum := baseRequest(ScopeFinal, currentCatalogCount)
	overMaximum.Catalog.RecordCount = lyricscontract.MaxCatalogRecordCount + 1
	if _, err := Assemble(overMaximum, resolver); err == nil || !strings.Contains(err.Error(), "identity bindings") {
		t.Fatalf("over-maximum catalog error=%v", err)
	}
}

func TestFinalRootSupportsLargerFutureCatalog(t *testing.T) {
	const futureCatalogCount = 1024
	musicIDs := sequentialMusicIDs(10_000, futureCatalogCount)
	request := requestForMusicIDs(ScopeFinal, musicIDs, musicIDs)
	manifest, err := Assemble(request, rootResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Catalog.RecordCount != futureCatalogCount || manifest.Coverage.Total != futureCatalogCount ||
		manifest.Songs[0].MusicID != musicIDs[0] || manifest.Songs[len(manifest.Songs)-1].MusicID != musicIDs[len(musicIDs)-1] {
		t.Fatalf("future final root catalog=%+v coverage=%+v", manifest.Catalog, manifest.Coverage)
	}
}

func TestPartialAndRetryRootsRequireValidatedParentStrictSubset(t *testing.T) {
	resolver := rootResolver(t)
	catalogMusicIDs := sequentialMusicIDs(1, currentCatalogCount)
	parent, err := Assemble(requestForMusicIDs(ScopeFinal, catalogMusicIDs, catalogMusicIDs), resolver)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ScopeKind{ScopePartial, ScopeRetry} {
		t.Run(string(kind), func(t *testing.T) {
			request := requestForMusicIDs(kind, catalogMusicIDs, []int{1, 17, currentCatalogCount})
			bindToParent(&request, parent)
			manifest, err := AssembleAgainstParent(request, resolver, parent)
			if err != nil {
				t.Fatalf("valid %s root: %v", kind, err)
			}
			if err := ValidateAgainstParent(manifest, parent); err != nil {
				t.Fatalf("validate %s against parent: %v", kind, err)
			}
			if kind == ScopeRetry {
				nonSubset := manifest
				nonSubset.Songs = cloneSongs(manifest.Songs)
				nonSubset.Songs[len(nonSubset.Songs)-1].MusicID = currentCatalogCount + 1
				nonSubset.RootSHA256, err = rootDigest(nonSubset)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateAgainstParent(nonSubset, parent); err == nil || !strings.Contains(err.Error(), "strict subset") {
					t.Fatalf("validated arbitrary retry IDs error=%v", err)
				}
			}
			if _, err := Assemble(request, resolver); err == nil || !strings.Contains(err.Error(), "parent root") {
				t.Fatalf("parentless %s assembly error=%v", kind, err)
			}
			missing := request
			missing.Scope.SupersedesRootID = ""
			missing.Scope.SupersedesRootSHA256 = ""
			if _, err := AssembleAgainstParent(missing, resolver, parent); err == nil || !strings.Contains(err.Error(), "supersession") {
				t.Fatalf("missing %s supersession error=%v", kind, err)
			}
			fullParentSet := requestForMusicIDs(kind, catalogMusicIDs, catalogMusicIDs)
			bindToParent(&fullParentSet, parent)
			if _, err := AssembleAgainstParent(fullParentSet, resolver, parent); err == nil || !strings.Contains(err.Error(), "strict subset") {
				t.Fatalf("full-parent-set %s error=%v", kind, err)
			}
		})
	}

	finalWithParent := requestForMusicIDs(ScopeFinal, catalogMusicIDs, catalogMusicIDs)
	if _, err := AssembleAgainstParent(finalWithParent, resolver, parent); err == nil || !strings.Contains(err.Error(), "must not specify a parent") {
		t.Fatalf("final assembly with parent error=%v", err)
	}

	arbitraryRetry := requestForMusicIDs(ScopeRetry, catalogMusicIDs, []int{currentCatalogCount + 1})
	bindToParent(&arbitraryRetry, parent)
	if _, err := AssembleAgainstParent(arbitraryRetry, resolver, parent); err == nil || !strings.Contains(err.Error(), "strict subset") {
		t.Fatalf("arbitrary non-subset retry IDs error=%v", err)
	}
}

func TestParentBindingRejectsWrongDigestAndCatalog(t *testing.T) {
	resolver := rootResolver(t)
	catalogMusicIDs := sequentialMusicIDs(1, currentCatalogCount)
	parent, err := Assemble(requestForMusicIDs(ScopeFinal, catalogMusicIDs, catalogMusicIDs), resolver)
	if err != nil {
		t.Fatal(err)
	}
	request := requestForMusicIDs(ScopePartial, catalogMusicIDs, []int{2, 3})
	bindToParent(&request, parent)
	manifest, err := AssembleAgainstParent(request, resolver, parent)
	if err != nil {
		t.Fatal(err)
	}

	wrongDigestManifest := manifest
	wrongDigestManifest.Scope.SupersedesRootSHA256 = strings.Repeat("e", 64)
	wrongDigestManifest.RootSHA256, err = rootDigest(wrongDigestManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgainstParent(wrongDigestManifest, parent); err == nil || !strings.Contains(err.Error(), "supersession") {
		t.Fatalf("validated wrong parent digest binding error=%v", err)
	}
	wrongCatalogManifest := manifest
	wrongCatalogManifest.Catalog.IdentitySHA256 = strings.Repeat("e", 64)
	wrongCatalogManifest.RootSHA256, err = rootDigest(wrongCatalogManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgainstParent(wrongCatalogManifest, parent); err == nil || !strings.Contains(err.Error(), "catalog identity") {
		t.Fatalf("validated wrong parent catalog binding error=%v", err)
	}

	wrongID := request
	wrongID.Scope.SupersedesRootID = "lyrics-root-other"
	if _, err := AssembleAgainstParent(wrongID, resolver, parent); err == nil || !strings.Contains(err.Error(), "supersession") {
		t.Fatalf("wrong parent ID binding error=%v", err)
	}
	wrongDigest := request
	wrongDigest.Scope.SupersedesRootSHA256 = strings.Repeat("e", 64)
	if _, err := AssembleAgainstParent(wrongDigest, resolver, parent); err == nil || !strings.Contains(err.Error(), "supersession") {
		t.Fatalf("wrong parent digest binding error=%v", err)
	}
	wrongCatalog := request
	wrongCatalog.Catalog.IdentitySHA256 = strings.Repeat("e", 64)
	if _, err := AssembleAgainstParent(wrongCatalog, resolver, parent); err == nil || !strings.Contains(err.Error(), "catalog identity") {
		t.Fatalf("wrong parent catalog binding error=%v", err)
	}
	tamperedParent := parent
	tamperedParent.RootSHA256 = strings.Repeat("e", 64)
	if _, err := AssembleAgainstParent(request, resolver, tamperedParent); err == nil || !strings.Contains(err.Error(), "parent lyrics root") {
		t.Fatalf("invalid parent root digest error=%v", err)
	}
}

func TestCanonicalDecodeSeparatesStructureFromParentProof(t *testing.T) {
	resolver := rootResolver(t)
	catalogMusicIDs := sequentialMusicIDs(1, currentCatalogCount)
	parent, err := Assemble(requestForMusicIDs(ScopeFinal, catalogMusicIDs, catalogMusicIDs), resolver)
	if err != nil {
		t.Fatal(err)
	}
	request := requestForMusicIDs(ScopePartial, catalogMusicIDs, []int{3, 8})
	bindToParent(&request, parent)
	manifest, err := AssembleAgainstParent(request, resolver, parent)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := DecodeCanonical(body)
	if err != nil || !reflect.DeepEqual(structural, manifest) {
		t.Fatalf("standalone structural decode differs: err=%v", err)
	}
	validated, err := DecodeCanonicalAgainstParent(body, parent)
	if err != nil || !reflect.DeepEqual(validated, manifest) {
		t.Fatalf("parent-aware decode differs: err=%v", err)
	}

	wrongParentRequest := requestForMusicIDs(ScopeFinal, catalogMusicIDs, catalogMusicIDs)
	wrongParentRequest.RootID = "lyrics-root-other"
	wrongParent, err := Assemble(wrongParentRequest, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCanonicalAgainstParent(body, wrongParent); err == nil || !strings.Contains(err.Error(), "supersession") {
		t.Fatalf("parent-aware decode accepted the wrong direct parent: %v", err)
	}
	parentBody, err := MarshalCanonical(parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCanonicalAgainstParent(parentBody, parent); err == nil || !strings.Contains(err.Error(), "only partial or retry") {
		t.Fatalf("parent-aware decode accepted a final root: %v", err)
	}
}

func TestRootBindsSelectedAcquisitionsEvidenceOutcomesAndPackUnion(t *testing.T) {
	item := rootTestEvidence(t, 1)
	resolver := rootResolver(t, item)
	catalogMusicIDs := sequentialMusicIDs(1, currentCatalogCount)
	parent, err := Assemble(requestForMusicIDs(ScopeFinal, catalogMusicIDs, catalogMusicIDs), rootResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	request := requestForMusicIDs(ScopePartial, catalogMusicIDs, []int{1, 2})
	bindToParent(&request, parent)
	packRef := rootEvidenceRef(item)
	selection := SelectedEvidenceRef{
		Provider: packRef.Provider, AcquisitionID: packRef.AcquisitionID, EvidenceID: packRef.EvidenceID,
		SHA256: packRef.SHA256, EnvelopeSHA256: packRef.EnvelopeSHA256,
	}
	outcome := ProviderOutcomeRef{Provider: item.Provider, OutcomeID: "provider-outcome-001", SHA256: strings.Repeat("f", 64)}
	for index := range request.Songs {
		request.Songs[index].State = lyricscontract.CoverageComplete
		request.Songs[index].ProviderOutcomes = []ProviderOutcomeRef{outcome}
		request.Songs[index].SelectedEvidence = []SelectedEvidenceRef{selection}
	}
	manifest, err := AssembleAgainstParent(request, resolver, parent)
	if err != nil {
		t.Fatal(err)
	}
	pack := resolver.Manifest()
	if manifest.Coverage.Complete != 2 || manifest.Coverage.SelectionRefCount != 2 ||
		manifest.Coverage.UniqueAcquisitionCount != 1 || manifest.Coverage.UniqueEvidenceCount != 1 ||
		manifest.EvidencePack.PackSHA256 != pack.PackSHA256 || manifest.EvidencePack.SelectionSHA256 != pack.SelectionSHA256 ||
		len(manifest.EvidencePack.Shards) != len(pack.Shards) || manifest.EvidencePack.Shards[0].SHA256 != pack.Shards[0].SHA256 {
		t.Fatalf("root compact bindings=%+v coverage=%+v", manifest.EvidencePack, manifest.Coverage)
	}
	if err := ValidateAgainstPack(manifest, resolver); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgainstParent(manifest, parent); err != nil {
		t.Fatal(err)
	}

	wrongDigest := request
	wrongDigest.Songs = cloneSongs(request.Songs)
	wrongDigest.Songs[0].SelectedEvidence[0].SHA256 = strings.Repeat("0", 64)
	wrongDigest.Songs[1].SelectedEvidence[0].SHA256 = strings.Repeat("0", 64)
	if _, err := AssembleAgainstParent(wrongDigest, resolver, parent); err == nil || !strings.Contains(err.Error(), "does not equal") {
		t.Fatalf("wrong selected digest error=%v", err)
	}
	orphanPack := requestForMusicIDs(ScopePartial, catalogMusicIDs, []int{1})
	bindToParent(&orphanPack, parent)
	if _, err := AssembleAgainstParent(orphanPack, resolver, parent); err == nil || !strings.Contains(err.Error(), "does not equal") {
		t.Fatalf("orphan pack evidence error=%v", err)
	}
	wrongProvider := request
	wrongProvider.Songs = cloneSongs(request.Songs)
	wrongProvider.Songs[0].SelectedEvidence[0].Provider = model.LyricsSourceProviderMoegirl
	wrongProvider.Songs[1].SelectedEvidence[0].Provider = model.LyricsSourceProviderMoegirl
	if _, err := AssembleAgainstParent(wrongProvider, resolver, parent); err == nil || !strings.Contains(err.Error(), "does not equal") {
		t.Fatalf("wrong selected provider error=%v", err)
	}
	wrongAcquisition := request
	wrongAcquisition.Songs = cloneSongs(request.Songs)
	for index := range wrongAcquisition.Songs {
		wrongAcquisition.Songs[index].SelectedEvidence[0].AcquisitionID = strings.Repeat("0", 64)
	}
	if _, err := AssembleAgainstParent(wrongAcquisition, resolver, parent); err == nil || !strings.Contains(err.Error(), "does not equal") {
		t.Fatalf("wrong selected acquisition error=%v", err)
	}
	wrongEnvelope := request
	wrongEnvelope.Songs = cloneSongs(request.Songs)
	for index := range wrongEnvelope.Songs {
		wrongEnvelope.Songs[index].SelectedEvidence[0].EnvelopeSHA256 = strings.Repeat("0", 64)
	}
	if _, err := AssembleAgainstParent(wrongEnvelope, resolver, parent); err == nil || !strings.Contains(err.Error(), "does not equal") {
		t.Fatalf("wrong selected envelope error=%v", err)
	}
	conflictingAcquisition := request
	conflictingAcquisition.Songs = cloneSongs(request.Songs)
	conflictingAcquisition.Songs[1].SelectedEvidence[0].AcquisitionID = strings.Repeat("1", 64)
	if _, err := AssembleAgainstParent(conflictingAcquisition, resolver, parent); err == nil || !strings.Contains(err.Error(), "one EvidenceID") {
		t.Fatalf("conflicting selected acquisition error=%v", err)
	}
}

func TestRootV2CountsGameOnlyAndSatisfiedNoLyrics(t *testing.T) {
	item := rootTestEvidence(t, 91)
	selection := rootEvidenceRef(item)
	outcome := ProviderOutcomeRef{
		Provider: item.Provider, OutcomeID: "provider-outcome-recovery-v2", SHA256: strings.Repeat("f", 64),
	}
	request := baseRequest(ScopeFinal, currentCatalogCount)
	request.Songs[0].State = lyricscontract.CoverageGameOnly
	request.Songs[0].ProviderOutcomes = []ProviderOutcomeRef{outcome}
	request.Songs[0].SelectedEvidence = []SelectedEvidenceRef{selection}
	request.Songs[1].State = lyricscontract.CoverageSatisfiedNoLyrics
	request.Songs[1].ProviderOutcomes = []ProviderOutcomeRef{outcome}
	request.Songs[1].SelectedEvidence = []SelectedEvidenceRef{}

	manifest, err := Assemble(request, rootResolver(t, item))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != lyricscontract.SchemaVersionV2 || manifest.Coverage.GameOnly != 1 ||
		manifest.Coverage.SatisfiedNoLyrics != 1 || manifest.Coverage.Missing != currentCatalogCount-2 ||
		manifest.Coverage.SelectionRefCount != 1 || manifest.Coverage.UniqueEvidenceCount != 1 {
		t.Fatalf("recovery-v2 root coverage=%+v", manifest.Coverage)
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCanonical(body)
	if err != nil || !reflect.DeepEqual(decoded, manifest) {
		t.Fatalf("recovery-v2 root round trip: err=%v", err)
	}

	legacy := manifest
	legacy.SchemaVersion = SchemaVersionV1
	legacy.CanonicalEncoding = CanonicalEncodingV1
	legacy.DigestAlgorithm = DigestAlgorithmV1
	legacy.RootSHA256 = ""
	legacy.RootSHA256, err = rootDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(legacy); err == nil {
		t.Fatal("root v1 accepted recovery-v2 coverage states")
	}
}

func TestRootV1CanonicalDigestRemainsCompatible(t *testing.T) {
	manifest, err := Assemble(baseRequest(ScopeFinal, currentCatalogCount), rootResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = SchemaVersionV1
	manifest.CanonicalEncoding = CanonicalEncodingV1
	manifest.DigestAlgorithm = DigestAlgorithmV1
	manifest.RootSHA256 = ""
	manifest.RootSHA256, err = rootDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range []string{`"gameOnly"`, `"satisfiedNoLyrics"`} {
		if bytes.Contains(body, []byte(prohibited)) {
			t.Fatalf("v1 canonical root gained v2 field %s", prohibited)
		}
	}
	decoded, err := DecodeCanonical(body)
	if err != nil || !reflect.DeepEqual(decoded, manifest) {
		t.Fatalf("v1 canonical root compatibility drifted: err=%v", err)
	}
}

func TestDecodeRootRejectsStrictJSONBoundariesAndForbiddenFields(t *testing.T) {
	manifest, err := Assemble(baseRequest(ScopeFinal, currentCatalogCount), rootResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	deep := []byte(`{"schemaVersion":1,"unknown":` + strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1) + `}`)
	mutations := map[string][]byte{
		"duplicate":       bytes.Replace(body, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1),
		"unknown":         bytes.Replace(body, []byte(`"rootId":`), []byte(`"title":"forbidden","rootId":`), 1),
		"trailing value":  append(append([]byte(nil), body...), []byte(`{}`)...),
		"trailing space":  append(append([]byte(nil), body...), ' '),
		"invalid UTF-8":   append([]byte{0xff}, body...),
		"lone surrogate":  []byte(`{"schemaVersion":1,"unknown":"\uD800"}`),
		"excessive depth": deep,
		"oversized":       bytes.Repeat([]byte{' '}, MaxManifestBytes+1),
	}
	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCanonical(mutated); err == nil {
				t.Fatalf("hostile %s root JSON was accepted", name)
			}
		})
	}
	for _, forbidden := range []string{"raw", "lyrics", "title", "translation", "romanization", "romaji", "privateError", "path"} {
		mutated := bytes.Replace(body, []byte(`"rootId":`), []byte(fmt.Sprintf(`%q:"forbidden","rootId":`, forbidden)), 1)
		if _, err := DecodeCanonical(mutated); err == nil {
			t.Fatalf("root accepted forbidden field %q", forbidden)
		}
	}
	for _, forbidden := range []string{`"raw"`, `"lyrics"`, `"title"`, `"translation"`, `"romanization"`, `"romaji"`, `"privateError"`, `"path"`} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("compact root emitted forbidden field %s", forbidden)
		}
	}
}

func TestRootDigestDetectsTamper(t *testing.T) {
	manifest, err := Assemble(baseRequest(ScopeFinal, currentCatalogCount), rootResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Songs[0].ResultSHA256 = strings.Repeat("f", 64)
	if err := Validate(manifest); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered root error=%v", err)
	}
}
