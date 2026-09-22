package lyricsrecoveryimport

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsoutcomeartifact"
	"moesekai/server/internal/lyricsrecovery"
	"moesekai/server/internal/lyricsreview"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricssourceoffline"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

type CatalogItem struct {
	MusicID                     int
	JapaneseTitle               string
	CatalogFingerprint          string
	TargetMusicID               int
	AssociationMusicIDs         []int
	PerformerSegmentationPolicy lyricssource.PerformerSegmentationPolicy
}

// BuildItem converts one validated recovery SongResult into the additive import
// union. The reviewed 698 release has exactly one provider candidate per lyric
// bearing song; plural-provider composition remains fail closed until its raw
// component reconstruction receives a separately reviewed adapter.
func BuildItem(
	catalog CatalogItem,
	result lyricsrecovery.SongResult,
	outcomes []lyricsoutcomeartifact.Artifact,
	resolver *lyricsevidencepack.Resolver,
) (Item, error) {
	return BuildItemWithReview(catalog, result, outcomes, resolver, nil)
}

func BuildItemWithReview(
	catalog CatalogItem,
	result lyricsrecovery.SongResult,
	outcomes []lyricsoutcomeartifact.Artifact,
	resolver *lyricsevidencepack.Resolver,
	reviewResolver *lyricsreview.Resolver,
) (Item, error) {
	if err := lyricsrecovery.ValidateSongResult(result); err != nil {
		return Item{}, err
	}
	if reviewResolver != nil {
		observation, outcomeObservations := reviewImportObservations(result, outcomes)
		if err := reviewResolver.ValidateResult(observation, outcomeObservations); err != nil {
			return Item{}, err
		}
	}
	item := Item{
		MusicID: catalog.MusicID, JapaneseTitle: catalog.JapaneseTitle,
		CatalogFingerprint: catalog.CatalogFingerprint, TargetMusicID: catalog.TargetMusicID,
		AssociationMusicIDs: append([]int{}, catalog.AssociationMusicIDs...),
		State:               result.State, ResultSHA256: result.ResultSHA256,
	}
	if catalog.AssociationMusicIDs == nil {
		item.AssociationMusicIDs = nil
	}
	if catalog.MusicID != result.MusicID {
		return Item{}, errors.New("catalog and recovery music IDs differ")
	}
	if result.SchemaVersion == lyricsrecovery.SongResultSchemaVersionV3 {
		return buildV3Item(catalog, result, outcomes, resolver, reviewResolver, item)
	}

	switch result.State {
	case lyricscontract.CoverageComplete, lyricscontract.CoverageGameOnly:
		identity, artifact, err := recoverySourceArtifact(
			result, outcomes, resolver, catalog.PerformerSegmentationPolicy, reviewResolver,
		)
		if err != nil {
			return Item{}, err
		}
		component := &model.LyricsSourceComponentRef{RenditionKey: identity.RenditionKey}
		privateReview := recoveryPrivateReview(result)
		if result.State == lyricscontract.CoverageComplete {
			document := model.LyricsSourceDocument{
				SchemaVersion: model.LyricsSourceDocumentSchemaVersion, ReasonCode: result.ReasonCode,
				FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
				Provenance: model.LyricsSourceComponentProvenance{
					FullText: *component, VersionEvidence: *component,
				},
				Full: *result.Full, Game: cloneRecoveryFull(result.Game),
				GameProjection:  cloneProjection(result.GameProjection),
				AlternateVocals: rebindRecoveryAlternateVocals(result.AlternateVocals, *component),
				PrivateReview:   privateReview,
			}
			if result.Game != nil {
				document.Provenance.GameText = component
			}
			if len(result.Components.PerformerSegmentation) != 0 {
				document.Provenance.PerformerSegmentation = component
			}
			if len(result.Components.Ruby) != 0 && sourceFullHasRuby(document.Full) {
				document.Provenance.Ruby = component
			}
			if result.GameProjection != nil {
				document.Provenance.GameProjection = component
			}
			draft, err := lyricsstaging.BuildRecoveryDraft(
				catalog.MusicID, catalog.JapaneseTitle, catalog.CatalogFingerprint, catalog.TargetMusicID,
				catalog.AssociationMusicIDs, document, []lyricsstaging.Artifact{artifact}, result.Translations,
			)
			if err != nil {
				return Item{}, err
			}
			item.Draft = &draft
			return item, nil
		}

		availability := model.LyricsAvailabilityDocument{
			SchemaVersion: model.LyricsAvailabilityDocumentSchemaVersion,
			State:         model.LyricsAvailabilityStateGameOnly, ReasonCode: result.ReasonCode,
			FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
			Provenance: model.LyricsAvailabilityComponentProvenance{
				GameText: component, VersionEvidence: component,
			},
			Game:            result.Game,
			AlternateVocals: rebindRecoveryAlternateVocals(result.AlternateVocals, *component),
			PrivateReview:   privateReview,
		}
		if len(result.Components.PerformerSegmentation) != 0 {
			availability.Provenance.PerformerSegmentation = component
		}
		if len(result.Components.Ruby) != 0 && sourceFullHasRuby(*result.Game) {
			availability.Provenance.Ruby = component
		}
		documentSHA, err := AvailabilityDocumentSHA256(availability)
		if err != nil {
			return Item{}, err
		}
		item.Availability = &availability
		item.AvailabilityDocumentSHA256 = documentSHA
		item.Artifacts = []lyricsstaging.Artifact{artifact}
		item.Translations = append([]string(nil), result.Translations...)
		if result.Translations == nil {
			item.Translations = nil
		}
		return item, nil

	case lyricscontract.CoverageSatisfiedNoLyrics:
		return buildTextFreeItem(item, model.LyricsAvailabilityStateSatisfiedNoLyrics, "",
			model.LyricsAvailabilityNoLyricsCatalogInstrumental)
	case lyricscontract.CoverageAmbiguous:
		return buildTextFreeItem(item, model.LyricsAvailabilityStateAmbiguous,
			model.LyricsSourceVersionReasonVersionConflict, "")
	case lyricscontract.CoverageMissing:
		return buildTextFreeItem(item, model.LyricsAvailabilityStateMissing,
			model.LyricsSourceVersionReasonVersionConflict, "")
	case lyricscontract.CoverageIncomplete:
		return buildTextFreeItem(item, model.LyricsAvailabilityStateIncomplete,
			model.LyricsSourceVersionReasonVersionConflict, "")
	case lyricscontract.CoverageFailed:
		return buildTextFreeItem(item, model.LyricsAvailabilityStateFailed,
			model.LyricsSourceVersionReasonVersionConflict, "")
	default:
		return Item{}, fmt.Errorf("unsupported recovery state %q", result.State)
	}
}

func buildTextFreeItem(
	item Item,
	state model.LyricsAvailabilityState,
	reason model.LyricsSourceVersionReasonCode,
	noLyricsReason string,
) (Item, error) {
	document := model.LyricsAvailabilityDocument{
		SchemaVersion: model.LyricsAvailabilityDocumentSchemaVersion,
		State:         state, ReasonCode: reason, NoLyricsReason: noLyricsReason,
		FixedIdentities: []model.LyricsSourceFixedIdentity{},
	}
	documentSHA, err := AvailabilityDocumentSHA256(document)
	if err != nil {
		return Item{}, err
	}
	item.Availability = &document
	item.AvailabilityDocumentSHA256 = documentSHA
	return item, nil
}

func recoverySourceArtifact(
	result lyricsrecovery.SongResult,
	outcomes []lyricsoutcomeartifact.Artifact,
	resolver *lyricsevidencepack.Resolver,
	segmentationPolicy lyricssource.PerformerSegmentationPolicy,
	reviewResolver *lyricsreview.Resolver,
) (model.LyricsSourceFixedIdentity, lyricsstaging.Artifact, error) {
	if resolver == nil || len(outcomes) == 0 {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("recovery source adapter requires outcomes and an evidence resolver")
	}
	outcomeByID := make(map[string]lyricsoutcomeartifact.Artifact, len(outcomes))
	for _, outcome := range outcomes {
		if err := lyricsoutcomeartifact.Validate(outcome); err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
		}
		outcomeByID[outcome.OutcomeID] = outcome
	}
	candidates := []lyricsoutcomeartifact.Artifact{}
	for _, reference := range result.ProviderOutcomes {
		outcome, found := outcomeByID[reference.OutcomeID]
		if !found || outcome.Provider != reference.Provider || outcome.ArtifactSHA256 != reference.SHA256 {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("provider outcome does not match the recovery result")
		}
		if outcome.Candidate != nil && outcomeUsesSelectedEvidence(outcome, result.SelectedEvidence) {
			candidates = append(candidates, outcome)
		}
	}
	if len(candidates) != 1 {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reviewed recovery import requires exactly one selected provider candidate")
	}
	outcome := candidates[0]
	candidate := *outcome.Candidate
	if len(outcome.Acquisitions) != len(result.SelectedEvidence) {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("selected provider acquisitions do not equal the song evidence union")
	}

	var revisionEvidence lyricssource.IndexEvidence
	indexRefs := make([]model.LyricsSourceIndexEvidenceRef, len(outcome.Acquisitions))
	for index, acquisition := range outcome.Acquisitions {
		indexRefs[index] = model.LyricsSourceIndexEvidenceRef{EvidenceID: acquisition.EvidenceID, SHA256: acquisition.SHA256}
		selected, found := selectedEvidence(result.SelectedEvidence, acquisition.EvidenceID)
		if !found || selected.Provider != outcome.Provider || selected.AcquisitionID != acquisition.AcquisitionID ||
			selected.SHA256 != acquisition.SHA256 || selected.EnvelopeSHA256 != acquisition.EnvelopeSHA256 {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("provider acquisition is not the exact selected evidence")
		}
		evidence, err := resolver.HydrateExact(selected)
		if err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
		}
		if evidence.PageID == candidate.PageID && evidence.RevisionID == candidate.RevisionID {
			if revisionEvidence.EvidenceID != "" {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("selected candidate resolves multiple revision evidence envelopes")
			}
			revisionEvidence = evidence
		}
	}
	if revisionEvidence.EvidenceID == "" || revisionEvidence.Provider != outcome.Provider ||
		revisionEvidence.PageID != candidate.PageID || revisionEvidence.RevisionID != candidate.RevisionID {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("selected candidate has no exact revision evidence")
	}

	source := result.Full
	if result.State == lyricscontract.CoverageGameOnly {
		source = result.Game
	}
	if source == nil {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("selected recovery candidate has no authoritative rendition")
	}

	section := "Lyrics"
	var sourceRaw []byte
	switch outcome.Provider {
	case model.LyricsSourceProviderSekaipedia:
		projection, err := lyricssourceoffline.RecoverSekaipediaProjectionWithReview(
			revisionEvidence.Raw,
			lyricssource.FixedIndex{
				PageID: candidate.PageID, RevisionID: candidate.RevisionID,
				RevisionTimestamp: revisionEvidence.RevisionTimestamp, SHA1: candidate.SHA1,
				ContentSHA256: candidate.RawSHA256, Title: revisionEvidence.Title,
			},
			segmentationPolicy, result.MusicID, reviewResolver,
		)
		if err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, fmt.Errorf("reparse exact Sekaipedia revision: %w", err)
		}
		if projection.RenditionKey != candidate.RenditionKey || projection.ReasonCode != candidate.VersionReason {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia rendition identity does not match the provider outcome")
		}
		if result.State == lyricscontract.CoverageComplete {
			parsedFull, fullErr := lyricscontract.NormalizePersistedPerformerMetadata(projection.Full)
			if fullErr != nil {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia performer metadata is unsafe")
			}
			parsedRubyVersion, rubyErr := lyricssource.RecoveryPersistedRubyGeneratorVersion(parsedFull.RubyGeneratorVersion)
			if rubyErr != nil {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, rubyErr
			}
			parsedFull.RubyGeneratorVersion = parsedRubyVersion
			parsedFull = projectReparsedRecoveryComponents(parsedFull, result.Components)
			if err := compareRecoveryFullProjection(parsedFull, *source); err != nil {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, fmt.Errorf("reparsed Sekaipedia revision does not match the recovery song result: %w", err)
			}
			if result.Game != nil {
				if projection.Game == nil {
					return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia revision lost the independent Game artifact")
				}
				parsedGame, gameErr := lyricscontract.NormalizePersistedPerformerMetadata(*projection.Game)
				if gameErr != nil {
					return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia Game metadata is unsafe")
				}
				parsedGame = projectReparsedRecoveryComponents(parsedGame, result.Components)
				if gameErr := compareRecoveryFullProjection(parsedGame, *result.Game); gameErr != nil {
					return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, fmt.Errorf("reparsed Sekaipedia Game does not match the recovery song result: %w", gameErr)
				}
			} else if projection.Game != nil {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("recovery result dropped the independent Sekaipedia Game artifact")
			}
		} else {
			if projection.Game == nil {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia Game-only revision has no Game artifact")
			}
			parsedGame, gameErr := lyricscontract.NormalizePersistedPerformerMetadata(*projection.Game)
			if gameErr != nil {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia Game metadata is unsafe")
			}
			parsedGame = projectReparsedRecoveryComponents(parsedGame, result.Components)
			if gameErr := compareRecoveryFullProjection(parsedGame, *source); gameErr != nil {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, fmt.Errorf("reparsed Sekaipedia Game does not match the recovery song result: %w", gameErr)
			}
		}
		if !reflect.DeepEqual(projection.GameProjection, result.GameProjection) {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia Game projection does not match the recovery song result")
		}
		if !recoveryAlternateVocalsEqual(projection.AlternateVocals, result.AlternateVocals) {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("reparsed Sekaipedia alternate vocals do not match the recovery song result")
		}
		section = projection.Section
		sourceRaw = append([]byte(nil), projection.FixedJapaneseWikitext...)
	case model.LyricsSourceProviderMoegirlPublicExact:
		sourceRaw = append([]byte(nil), revisionEvidence.Raw...)
		if candidate.RawSHA256 == "" || revisionEvidence.RawSHA256 != candidate.RawSHA256 {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("exact-public raw bytes do not match the provider candidate")
		}
		if err := validateExactPublicRecoveryProjection(sourceRaw, revisionEvidence, candidate, *source, result.Translations); err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
		}
	default:
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("recovery import source provider is outside the reviewed Sekaipedia/exact-public partition")
	}

	identity := model.LyricsSourceFixedIdentity{
		Provider: outcome.Provider, Origin: revisionEvidence.Origin,
		PageID: candidate.PageID, RevisionID: candidate.RevisionID, RevisionTimestamp: revisionEvidence.RevisionTimestamp,
		SHA1: candidate.SHA1, Title: revisionEvidence.Title, CanonicalURL: revisionEvidence.CanonicalURL,
		FetchedAt: revisionEvidence.FetchedAt, Categories: append([]string{}, revisionEvidence.Categories...),
		Section: section, RenditionKey: candidate.RenditionKey, CompositionRenditionKey: candidate.RenditionKey,
		VersionReason: candidate.VersionReason, IndexEvidenceRefs: indexRefs,
	}
	if err := model.ValidateLyricsSourceFixedIdentity(identity); err != nil {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
	}
	artifact, err := lyricsstaging.NewRecoveryArtifact(identity, sourceRaw)
	if err != nil {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
	}
	return identity, artifact, nil
}

func projectReparsedRecoveryComponents(
	full model.LyricsSourceFull,
	components lyricsrecovery.ComponentEvidence,
) model.LyricsSourceFull {
	if len(components.PerformerSegmentation) == 0 {
		full.Performers = []model.LyricsSourcePerformer{}
		for lineIndex := range full.Lines {
			line := &full.Lines[lineIndex]
			ruby := []model.LyricsSourceRubySpan{}
			for _, segment := range line.Segments {
				ruby = append(ruby, segment.Ruby...)
			}
			line.Segments = []model.LyricsSourceSegment{{
				Text: line.Text, PerformerIDs: []string{}, Ruby: ruby,
			}}
			line.TrailingPerformerIDs = []string{}
		}
	}
	if len(components.Ruby) == 0 {
		full.RubyGeneratorVersion = ""
		for lineIndex := range full.Lines {
			for segmentIndex := range full.Lines[lineIndex].Segments {
				segment := &full.Lines[lineIndex].Segments[segmentIndex]
				segment.Ruby = []model.LyricsSourceRubySpan{{Text: segment.Text}}
			}
		}
	}
	return full
}

func compareRecoveryFullProjection(reparsed, recovered model.LyricsSourceFull) error {
	if reparsed.Version != recovered.Version {
		return errors.New("version differs")
	}
	if reparsed.RubyGeneratorVersion != recovered.RubyGeneratorVersion {
		return errors.New("ruby generator differs")
	}
	if !reflect.DeepEqual(reparsed.Performers, recovered.Performers) {
		return errors.New("performer legend differs")
	}
	if len(reparsed.Lines) != len(recovered.Lines) {
		return errors.New("line count differs")
	}
	for lineIndex := range reparsed.Lines {
		left, right := reparsed.Lines[lineIndex], recovered.Lines[lineIndex]
		if left.ID != right.ID {
			return fmt.Errorf("line %d ID differs", lineIndex+1)
		}
		if left.Text != right.Text {
			return fmt.Errorf("line %d text differs", lineIndex+1)
		}
		if left.StanzaBreakBefore != right.StanzaBreakBefore {
			return fmt.Errorf("line %d stanza boundary differs", lineIndex+1)
		}
		if !reflect.DeepEqual(left.TrailingPerformerIDs, right.TrailingPerformerIDs) {
			return fmt.Errorf("line %d trailing performers differ", lineIndex+1)
		}
		if len(left.Segments) != len(right.Segments) {
			return fmt.Errorf("line %d segment count differs", lineIndex+1)
		}
		for segmentIndex := range left.Segments {
			leftSegment, rightSegment := left.Segments[segmentIndex], right.Segments[segmentIndex]
			if leftSegment.Text != rightSegment.Text {
				return fmt.Errorf("line %d segment %d text differs", lineIndex+1, segmentIndex+1)
			}
			if !reflect.DeepEqual(leftSegment.PerformerIDs, rightSegment.PerformerIDs) {
				return fmt.Errorf("line %d segment %d performers differ", lineIndex+1, segmentIndex+1)
			}
			if !reflect.DeepEqual(leftSegment.Ruby, rightSegment.Ruby) {
				return fmt.Errorf("line %d segment %d ruby differs", lineIndex+1, segmentIndex+1)
			}
		}
	}
	return nil
}

func validateExactPublicRecoveryProjection(
	raw []byte,
	evidence lyricssource.IndexEvidence,
	candidate lyricsoutcomeartifact.CandidateIdentity,
	full model.LyricsSourceFull,
	translations []string,
) error {
	extracted, err := lyricssource.ParseMoegirlPublicPageHTML(raw, evidence.CanonicalURL)
	if err != nil {
		return fmt.Errorf("reparse exact public page: %w", err)
	}
	if extracted.PageID != candidate.PageID || extracted.RevisionID != candidate.RevisionID ||
		extracted.PageTitle != evidence.Title || extracted.PageURL != evidence.CanonicalURL ||
		candidate.RenditionKey != "full-vocaloid" ||
		candidate.VersionReason != model.LyricsSourceVersionReasonUntaggedFullOnly ||
		full.Version != (model.LyricsSourceVersion{Kind: "vocaloid", Label: "Virtual Singer Version"}) ||
		len(full.Performers) != 0 ||
		full.RubyGeneratorVersion != lyricssource.DeterministicRubyGeneratorVersion() ||
		len(full.Lines) != len(extracted.Lines) || len(translations) != len(extracted.Lines) {
		return errors.New("exact-public identity or rendition does not match the recovery song result")
	}
	for index, line := range extracted.Lines {
		persisted := full.Lines[index]
		if persisted.ID != fmt.Sprintf("full-%06d", index+1) || persisted.Text != line.Japanese ||
			persisted.StanzaBreakBefore != line.StanzaBreakBefore || len(persisted.Segments) != 1 ||
			len(persisted.TrailingPerformerIDs) != 0 || translations[index] != line.Translation {
			return fmt.Errorf("exact-public line %d does not match the recovery song result", index+1)
		}
		segment := persisted.Segments[0]
		if segment.Text != line.Japanese || len(segment.PerformerIDs) != 0 {
			return fmt.Errorf("exact-public line %d source structure does not match", index+1)
		}
		if err := compareDeterministicPublicRuby(segment.Ruby, line.Japanese); err != nil {
			return fmt.Errorf("exact-public line %d ruby does not match: %w", index+1, err)
		}
	}
	return nil
}

func compareDeterministicPublicRuby(actual []model.LyricsSourceRubySpan, text string) error {
	expected, err := lyricssource.GenerateDeterministicRubySpans(text)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("deterministic ruby span count differs")
	}
	for index := range expected {
		if actual[index].Text != expected[index].Text || actual[index].Reading != expected[index].Reading ||
			actual[index].ReadingEvidence != nil {
			return fmt.Errorf("deterministic ruby span %d differs", index+1)
		}
	}
	return nil
}

func outcomeUsesSelectedEvidence(outcome lyricsoutcomeartifact.Artifact, selected []lyricscontract.EvidenceRef) bool {
	for _, acquisition := range outcome.Acquisitions {
		if _, found := selectedEvidence(selected, acquisition.EvidenceID); found {
			return true
		}
	}
	return false
}

func selectedEvidence(selected []lyricscontract.EvidenceRef, evidenceID string) (lyricscontract.EvidenceRef, bool) {
	index := sort.Search(len(selected), func(index int) bool { return selected[index].EvidenceID >= evidenceID })
	if index < len(selected) && selected[index].EvidenceID == evidenceID {
		return selected[index], true
	}
	return lyricscontract.EvidenceRef{}, false
}

func reviewImportObservations(
	result lyricsrecovery.SongResult,
	outcomes []lyricsoutcomeartifact.Artifact,
) (lyricsreview.ResultObservation, []lyricsreview.OutcomeObservation) {
	observation := lyricsreview.ResultObservation{
		MusicID: result.MusicID, State: string(result.State),
		HasFull: result.Full != nil, HasGame: result.Game != nil,
		HasGameProjection: result.GameProjection != nil,
	}
	for _, alternate := range result.AlternateVocals {
		kind := ""
		if alternate.Full != nil {
			kind = alternate.Full.Version.Kind
		} else if alternate.Game != nil {
			kind = alternate.Game.Version.Kind
		}
		if kind == "another" || strings.Contains(strings.ToLower(alternate.TabLabel), "another") {
			observation.AnotherCount++
		} else {
			observation.AlternateCount++
		}
	}
	observed := make([]lyricsreview.OutcomeObservation, len(outcomes))
	for index, outcome := range outcomes {
		observed[index] = lyricsreview.OutcomeObservation{
			Provider: outcome.Provider, OutcomeID: outcome.OutcomeID,
			ArtifactSHA256: outcome.ArtifactSHA256,
			Acquisitions: func() []lyricsreview.AcquisitionObservation {
				refs := make([]lyricsreview.AcquisitionObservation, len(outcome.Acquisitions))
				for refIndex, ref := range outcome.Acquisitions {
					refs[refIndex] = lyricsreview.AcquisitionObservation{
						AcquisitionID: ref.AcquisitionID, EvidenceID: ref.EvidenceID,
						SHA256: ref.SHA256, EnvelopeSHA256: ref.EnvelopeSHA256,
					}
				}
				return refs
			}(),
		}
		if outcome.Candidate != nil {
			observed[index].Candidate = &lyricsreview.CandidateObservation{
				PageID: outcome.Candidate.PageID, RevisionID: outcome.Candidate.RevisionID,
				SHA1: outcome.Candidate.SHA1, ContentSHA256: outcome.Candidate.RawSHA256,
			}
		}
	}
	return observation, observed
}

func recoveryAlternateVocalsEqual(left, right []model.LyricsSourceAlternateVocal) bool {
	leftCopy := model.CloneLyricsSourceAlternateVocals(left)
	rightCopy := model.CloneLyricsSourceAlternateVocals(right)
	for index := range leftCopy {
		leftCopy[index].Provenance = model.LyricsSourceAlternateVocalProvenance{}
	}
	for index := range rightCopy {
		rightCopy[index].Provenance = model.LyricsSourceAlternateVocalProvenance{}
	}
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func recoveryPrivateReview(result lyricsrecovery.SongResult) *model.LyricsSourcePrivateReview {
	var source *model.LyricsSourceFull
	if result.Full != nil {
		source = result.Full
	} else {
		source = result.Game
	}
	if source == nil || len(result.Components.PerformerSegmentation) == 0 || !sourceFullHasPerformerSegmentation(*source) {
		return nil
	}
	return &model.LyricsSourcePrivateReview{
		PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
	}
}

func sourceFullHasPerformerSegmentation(full model.LyricsSourceFull) bool {
	if len(full.Performers) != 0 {
		return true
	}
	for _, line := range full.Lines {
		if len(line.TrailingPerformerIDs) != 0 || len(line.Segments) != 1 ||
			len(line.Segments) == 1 && line.Segments[0].Text != line.Text {
			return true
		}
		for _, segment := range line.Segments {
			if len(segment.PerformerIDs) != 0 {
				return true
			}
		}
	}
	return false
}

func sourceFullHasRuby(full model.LyricsSourceFull) bool {
	for _, line := range full.Lines {
		for _, segment := range line.Segments {
			for _, span := range segment.Ruby {
				if span.Reading != "" {
					return true
				}
			}
		}
	}
	return false
}

func rebindRecoveryAlternateVocals(
	alternates []model.LyricsSourceAlternateVocal,
	component model.LyricsSourceComponentRef,
) []model.LyricsSourceAlternateVocal {
	result := model.CloneLyricsSourceAlternateVocals(alternates)
	for index := range result {
		result[index].Provenance.VersionEvidence = component
		if result[index].Full != nil {
			ref := component
			result[index].Provenance.FullText = &ref
		}
		if result[index].Game != nil {
			ref := component
			result[index].Provenance.GameText = &ref
		}
		if result[index].GameProjection != nil {
			ref := component
			result[index].Provenance.GameProjection = &ref
		}
	}
	return result
}

func cloneRecoveryFull(full *model.LyricsSourceFull) *model.LyricsSourceFull {
	if full == nil {
		return nil
	}
	copy := *full
	copy.Performers = append([]model.LyricsSourcePerformer{}, full.Performers...)
	copy.Lines = make([]model.LyricsSourceFullLine, len(full.Lines))
	for index, line := range full.Lines {
		copy.Lines[index] = line
		copy.Lines[index].TrailingPerformerIDs = append([]string{}, line.TrailingPerformerIDs...)
		copy.Lines[index].Segments = make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			copy.Lines[index].Segments[segmentIndex] = segment
			copy.Lines[index].Segments[segmentIndex].PerformerIDs = append([]string{}, segment.PerformerIDs...)
			copy.Lines[index].Segments[segmentIndex].Ruby = append([]model.LyricsSourceRubySpan{}, segment.Ruby...)
		}
	}
	return &copy
}

func cloneProjection(projection *model.LyricsSourceGameProjection) *model.LyricsSourceGameProjection {
	if projection == nil {
		return nil
	}
	return &model.LyricsSourceGameProjection{LineIDs: append([]string(nil), projection.LineIDs...)}
}
