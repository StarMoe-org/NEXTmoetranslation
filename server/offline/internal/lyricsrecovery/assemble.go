package lyricsrecovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsevidencepack"
	"moesekai/server/offline/internal/lyricsextractionplan"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
	"moesekai/server/offline/internal/lyricsrootmanifest"
)

func AssembleAndPublish(
	ctx context.Context,
	plan lyricsextractionplan.RecoveryPlan,
	planSHA256 string,
	ledger *lyricsacquisition.Ledger,
	results []SongResult,
	parent *lyricsrootmanifest.Manifest,
) (lyricsrootmanifest.Manifest, error) {
	if ctx == nil || ledger == nil || len(planSHA256) != 64 || results == nil {
		return lyricsrootmanifest.Manifest{}, errors.New("lyrics recovery root assembly input is invalid")
	}
	if err := lyricsextractionplan.ValidateRecovery(plan); err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	sortedResults := append([]SongResult(nil), results...)
	sort.Slice(sortedResults, func(left, right int) bool { return sortedResults[left].MusicID < sortedResults[right].MusicID })
	if len(sortedResults) != len(plan.Scope.MusicIDs) {
		return lyricsrootmanifest.Manifest{}, errors.New("lyrics recovery results do not exactly cover the plan scope")
	}
	songs := make([]lyricsrootmanifest.SongResultRef, len(sortedResults))
	selectedByEvidence := make(map[string]lyricscontract.EvidenceRef)
	for index, result := range sortedResults {
		if result.MusicID != plan.Scope.MusicIDs[index] {
			return lyricsrootmanifest.Manifest{}, errors.New("lyrics recovery result music IDs do not exactly match the plan scope")
		}
		ref, err := RootSongRef(result)
		if err != nil {
			return lyricsrootmanifest.Manifest{}, err
		}
		songs[index] = ref
		for _, evidence := range result.SelectedEvidence {
			if existing, found := selectedByEvidence[evidence.EvidenceID]; found && existing != evidence {
				return lyricsrootmanifest.Manifest{}, errors.New("lyrics recovery selected evidence union conflicts across songs")
			}
			selectedByEvidence[evidence.EvidenceID] = evidence
		}
	}
	if err := validatePublishedRecoveryArtifacts(plan, sortedResults); err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	selected := make([]lyricscontract.EvidenceRef, 0, len(selectedByEvidence))
	for _, evidence := range selectedByEvidence {
		selected = append(selected, evidence)
	}
	sort.Slice(selected, func(left, right int) bool { return selected[left].EvidenceID < selected[right].EvidenceID })
	if _, err := lyricsevidencepack.Build(ctx, plan.Outputs.EvidencePack, selected, ledger); err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	resolver, err := lyricsevidencepack.OpenResolver(plan.Outputs.EvidencePack)
	if err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	request := lyricsrootmanifest.AssemblyRequest{
		RootID: recoveryRootID(plan, songs),
		Scope: lyricsrootmanifest.ScopeBinding{
			Kind: lyricsrootmanifest.ScopeKind(plan.Scope.Kind), ScopeID: plan.Scope.ScopeID,
			SupersedesRootID:     plan.Scope.SupersedesRootID,
			SupersedesRootSHA256: plan.Scope.SupersedesRootSHA256,
		},
		Catalog: lyricsrootmanifest.CatalogBinding{
			SchemaVersion: plan.Catalog.SchemaVersion, RuntimeSchemaVersion: plan.Catalog.RuntimeSchemaVersion,
			RecordCount: plan.Catalog.RecordCount, IdentityPolicyVersion: plan.Catalog.IdentityPolicyVersion,
			SourceSHA256: plan.Catalog.SourceSHA256, IdentitySHA256: plan.Catalog.IdentitySHA256,
			MusicIDsSHA256: plan.Catalog.MusicIDsSHA256,
		},
		Plan:  lyricsrootmanifest.PlanBinding{PlanID: plan.PlanID, SHA256: planSHA256},
		Songs: songs,
	}
	var manifest lyricsrootmanifest.Manifest
	if request.Scope.Kind == lyricsrootmanifest.ScopeFinal {
		if parent != nil {
			return lyricsrootmanifest.Manifest{}, errors.New("final lyrics recovery root must not receive a parent")
		}
		manifest, err = lyricsrootmanifest.Assemble(request, resolver)
	} else {
		if parent == nil {
			return lyricsrootmanifest.Manifest{}, errors.New("partial or retry lyrics recovery root requires its exact parent")
		}
		manifest, err = lyricsrootmanifest.AssembleAgainstParent(request, resolver, *parent)
	}
	if err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	if parent == nil {
		err = lyricsrootmanifest.Validate(manifest)
	} else {
		err = lyricsrootmanifest.ValidateAgainstParent(manifest, *parent)
	}
	if err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	body, err := lyricsrootmanifest.MarshalCanonical(manifest)
	if err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	if err := lyricsrootmanifest.PublishCreateExclusive(plan.Outputs.RootManifest, body); err != nil {
		return lyricsrootmanifest.Manifest{}, err
	}
	return manifest, nil
}

type expectedPublishedOutcome struct {
	musicID       int
	provider      model.LyricsSourceProvider
	outcomeID     string
	sha256        string
	parserVersion string
}

func validatePlanOrderedOutcomeRefs(
	providerOrder []model.LyricsSourceProvider,
	refs []lyricsrootmanifest.ProviderOutcomeRef,
) error {
	if len(refs) == 0 || len(refs) > len(providerOrder) {
		return errors.New("published recovery song does not reference one non-empty evaluated provider prefix")
	}
	for index, ref := range refs {
		if ref.Provider != providerOrder[index] {
			return errors.New("published recovery song references a gapped, reordered, or unevaluated provider")
		}
	}
	return nil
}

func validatePublishedRecoveryArtifacts(
	plan lyricsextractionplan.RecoveryPlan,
	results []SongResult,
) error {
	parserVersions := make(map[model.LyricsSourceProvider]string, len(plan.Versions.Parsers))
	for _, parser := range plan.Versions.Parsers {
		parserVersions[model.LyricsSourceProvider(parser.Provider)] = parser.ParserVersion
	}
	providerOrder := make([]model.LyricsSourceProvider, len(plan.Providers.Order))
	for index, provider := range plan.Providers.Order {
		providerOrder[index] = model.LyricsSourceProvider(provider)
	}
	expectedOutcomes := make(map[string]expectedPublishedOutcome, len(results)*len(providerOrder))
	expectedResults := make(map[string]SongResult, len(results))
	for _, result := range results {
		if err := ValidateSongResult(result); err != nil {
			return err
		}
		effectivePlanOrder, err := lyricsextractionplan.EffectiveRecoveryProviderOrder(plan.Providers, result.MusicID)
		if err != nil {
			return err
		}
		effectiveProviderOrder := make([]model.LyricsSourceProvider, len(effectivePlanOrder))
		for index, provider := range effectivePlanOrder {
			effectiveProviderOrder[index] = model.LyricsSourceProvider(provider)
		}
		if err := validatePlanOrderedOutcomeRefs(effectiveProviderOrder, result.ProviderOutcomes); err != nil {
			return err
		}
		for _, ref := range result.ProviderOutcomes {
			if parserVersions[ref.Provider] == "" {
				return errors.New("published recovery song references a provider without an immutable parser version")
			}
			name := fmt.Sprintf("music-%d-%s-%s.json", result.MusicID, ref.Provider, ref.SHA256)
			if _, duplicate := expectedOutcomes[name]; duplicate {
				return errors.New("published provider outcome filename is duplicated")
			}
			expectedOutcomes[name] = expectedPublishedOutcome{
				musicID: result.MusicID, provider: ref.Provider, outcomeID: ref.OutcomeID,
				sha256: ref.SHA256, parserVersion: parserVersions[ref.Provider],
			}
		}
		name, err := SongResultFileName(result)
		if err != nil {
			return err
		}
		if _, duplicate := expectedResults[name]; duplicate {
			return errors.New("published song result filename is duplicated")
		}
		expectedResults[name] = result
	}

	outcomeNames, err := exactPrivateDirectoryEntries(plan.Outputs.ProviderOutcomes, expectedOutcomeNames(expectedOutcomes))
	if err != nil {
		return err
	}
	availableBySong := make(map[int]map[lyricscontract.EvidenceRef]struct{}, len(results))
	seenAcquisitionIDs := make(map[string]struct{})
	evidenceBySong := make(map[int]map[string]lyricscontract.EvidenceRef, len(results))
	for _, name := range outcomeNames {
		expected := expectedOutcomes[name]
		artifact, err := lyricsoutcomeartifact.Open(filepath.Join(plan.Outputs.ProviderOutcomes, name))
		if err != nil {
			return err
		}
		canonicalName, err := lyricsoutcomeartifact.FileName(artifact)
		if err != nil || canonicalName != name || artifact.MusicID != expected.musicID ||
			artifact.Provider != expected.provider || artifact.OutcomeID != expected.outcomeID ||
			artifact.ArtifactSHA256 != expected.sha256 || artifact.ParserVersion != expected.parserVersion ||
			artifact.PolicyVersion != plan.Versions.ProviderPolicy {
			return errors.New("published provider outcome does not exactly bind its song, provider, plan versions, and digest")
		}
		available := availableBySong[artifact.MusicID]
		if available == nil {
			available = make(map[lyricscontract.EvidenceRef]struct{})
			availableBySong[artifact.MusicID] = available
		}
		for _, ref := range artifact.Acquisitions {
			exact := lyricscontract.EvidenceRef{
				Provider: artifact.Provider, AcquisitionID: ref.AcquisitionID, EvidenceID: ref.EvidenceID,
				SHA256: ref.SHA256, EnvelopeSHA256: ref.EnvelopeSHA256,
			}
			if _, duplicate := seenAcquisitionIDs[ref.AcquisitionID]; duplicate {
				return errors.New("published provider outcomes duplicate one exact AcquisitionID")
			}
			seenAcquisitionIDs[ref.AcquisitionID] = struct{}{}
			byEvidence := evidenceBySong[artifact.MusicID]
			if byEvidence == nil {
				byEvidence = make(map[string]lyricscontract.EvidenceRef)
				evidenceBySong[artifact.MusicID] = byEvidence
			}
			if existing, duplicate := byEvidence[ref.EvidenceID]; duplicate && existing != exact {
				return errors.New("published provider outcomes conflict for one song evidence ID")
			}
			byEvidence[ref.EvidenceID] = exact
			available[exact] = struct{}{}
		}
	}

	resultNames, err := exactPrivateDirectoryEntries(plan.Outputs.SongResults, expectedResultNames(expectedResults))
	if err != nil {
		return err
	}
	for _, name := range resultNames {
		expected := expectedResults[name]
		published, err := OpenSongResult(filepath.Join(plan.Outputs.SongResults, name))
		if err != nil {
			return err
		}
		expectedBody, expectedErr := MarshalSongResult(expected)
		publishedBody, publishedErr := MarshalSongResult(published)
		if expectedErr != nil || publishedErr != nil || !bytes.Equal(expectedBody, publishedBody) {
			return errors.New("published song result does not exactly match the root assembly input")
		}
		available := availableBySong[published.MusicID]
		for _, selected := range published.SelectedEvidence {
			if _, found := available[selected]; !found {
				return errors.New("published song result selects evidence outside its exact provider outcomes")
			}
		}
	}
	return nil
}

func expectedOutcomeNames(input map[string]expectedPublishedOutcome) map[string]struct{} {
	result := make(map[string]struct{}, len(input))
	for name := range input {
		result[name] = struct{}{}
	}
	return result
}

func expectedResultNames(input map[string]SongResult) map[string]struct{} {
	result := make(map[string]struct{}, len(input))
	for name := range input {
		result[name] = struct{}{}
	}
	return result
}

func exactPrivateDirectoryEntries(path string, expected map[string]struct{}) ([]string, error) {
	directory, openedInfo, err := openStablePrivateDirectory(path)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil || len(entries) != len(expected) {
		return nil, errors.New("recovery artifact directory does not contain the exact declared file set")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, found := expected[entry.Name()]; !found || entry.IsDir() {
			return nil, errors.New("recovery artifact directory contains an orphan or unexpected entry")
		}
		names = append(names, entry.Name())
	}
	after, statErr := directory.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !os.SameFile(openedInfo, after) || !os.SameFile(openedInfo, pathInfo) {
		return nil, errors.New("recovery artifact directory changed while being enumerated")
	}
	sort.Strings(names)
	return names, nil
}

func recoveryRootID(plan lyricsextractionplan.RecoveryPlan, songs []lyricsrootmanifest.SongResultRef) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-recovery-root-id-v1\x00"))
	_, _ = digest.Write([]byte(plan.PlanID))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(plan.Scope.ScopeID))
	for _, song := range songs {
		_, _ = digest.Write([]byte(fmt.Sprintf("\x00%d\x00%s", song.MusicID, song.ResultSHA256)))
	}
	return "recovery-root:" + hex.EncodeToString(digest.Sum(nil))[:32]
}
