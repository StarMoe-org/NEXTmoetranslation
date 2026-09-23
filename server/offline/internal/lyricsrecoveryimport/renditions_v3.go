package lyricsrecoveryimport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsevidencepack"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
	"moesekai/server/offline/internal/lyricsrecovery"
	"moesekai/server/offline/internal/lyricsreview"
	"moesekai/server/offline/internal/lyricssourceoffline"
	"moesekai/server/offline/internal/lyricsstaging"
)

func buildV3Item(
	catalog CatalogItem,
	result lyricsrecovery.SongResult,
	outcomes []lyricsoutcomeartifact.Artifact,
	resolver *lyricsevidencepack.Resolver,
	reviewResolver *lyricsreview.Resolver,
	item Item,
) (Item, error) {
	document, artifacts, err := buildV3SourceDocument(result, outcomes, resolver, catalog.PerformerSegmentationPolicy, reviewResolver)
	if err != nil {
		return Item{}, err
	}
	translations := renditionTranslationsFromResult(result)
	draft, err := lyricsstaging.BuildRecoveryPeerDraft(
		catalog.MusicID, catalog.JapaneseTitle, catalog.CatalogFingerprint, catalog.TargetMusicID,
		catalog.AssociationMusicIDs, document, artifacts, translations,
	)
	if err != nil {
		return Item{}, err
	}
	item.Draft = &draft
	return item, nil
}

func v3FixedIdentityKey(outcomeID, renditionKey string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-recovery-import-v3-fixed-identity-v1\x00"))
	_, _ = digest.Write([]byte(outcomeID))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(renditionKey))
	return "fixed-" + hex.EncodeToString(digest.Sum(nil))
}

func sourceRenditionFromResult(result lyricsrecovery.SongResultRendition) (model.LyricsSourceRendition, error) {
	provenance := model.LyricsSourceRenditionProvenance{}
	seen := make(map[model.LyricsSourceRenditionComponentKind]struct{}, len(result.Components))
	fullRubyOutcomeID := ""
	gameRubyOutcomeID := ""
	for _, component := range result.Components {
		if model.LyricsSourceRenditionComponentRank(component.Component) < 0 || component.OutcomeID == "" {
			return model.LyricsSourceRendition{}, errors.New("v3 result has an invalid component identity")
		}
		if _, duplicate := seen[component.Component]; duplicate {
			return model.LyricsSourceRendition{}, errors.New("v3 result repeats a component")
		}
		seen[component.Component] = struct{}{}
		ref := model.LyricsSourceComponentRef{
			RenditionKey: v3FixedIdentityKey(component.OutcomeID, result.RenditionKey),
		}
		switch component.Component {
		case model.LyricsSourceRenditionComponentFullText:
			provenance.FullText = &ref
		case model.LyricsSourceRenditionComponentFullPerformerSegmentation:
			provenance.FullPerformerSegmentation = &ref
		case model.LyricsSourceRenditionComponentFullRuby:
			provenance.FullRuby = &ref
			fullRubyOutcomeID = component.OutcomeID
		case model.LyricsSourceRenditionComponentGameText:
			provenance.GameText = &ref
		case model.LyricsSourceRenditionComponentGamePerformerSegmentation:
			provenance.GamePerformerSegmentation = &ref
		case model.LyricsSourceRenditionComponentGameRuby:
			provenance.GameRuby = &ref
			gameRubyOutcomeID = component.OutcomeID
		case model.LyricsSourceRenditionComponentRelation:
			provenance.RelationEvidence = ref
		case model.LyricsSourceRenditionComponentVersion:
			provenance.VersionEvidence = ref
		}
	}
	full := model.CloneLyricsSourceFull(result.Full)
	if err := rebindV3ReadingEvidence(full, fullRubyOutcomeID, provenance.FullRuby); err != nil {
		return model.LyricsSourceRendition{}, err
	}
	game := model.CloneLyricsSourceFull(result.Game)
	if err := rebindV3ReadingEvidence(game, gameRubyOutcomeID, provenance.GameRuby); err != nil {
		return model.LyricsSourceRendition{}, err
	}
	var privateReview *model.LyricsSourcePrivateReview
	if result.PrivateReview != nil {
		copy := *result.PrivateReview
		privateReview = &copy
	}
	return model.LyricsSourceRendition{
		RenditionKey: result.RenditionKey, SourceKind: result.SourceKind,
		SourceTabPaths: cloneTabPaths(result.SourceTabPaths), ReasonCode: result.ReasonCode,
		SourcePerformerIDs:    append([]string(nil), result.SourcePerformerIDs...),
		FullPerformerEvidence: result.FullPerformerEvidence, GamePerformerEvidence: result.GamePerformerEvidence,
		Full: full, Game: game,
		Relation:   model.LyricsSourceRenditionRelation{Kind: result.Relation.Kind, FullRenditionKey: result.Relation.FullRenditionKey, LineIDs: append([]string(nil), result.Relation.LineIDs...)},
		Provenance: provenance, PrivateReview: privateReview,
	}, nil
}

func rebindV3ReadingEvidence(
	full *model.LyricsSourceFull,
	outcomeID string,
	fixedIdentity *model.LyricsSourceComponentRef,
) error {
	if full == nil || fixedIdentity == nil {
		return nil
	}
	for lineIndex := range full.Lines {
		for segmentIndex := range full.Lines[lineIndex].Segments {
			for spanIndex := range full.Lines[lineIndex].Segments[segmentIndex].Ruby {
				evidence := full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence
				if evidence == nil || evidence.FixedIdentityKey == "" {
					continue
				}
				if evidence.FixedIdentityKey != outcomeID {
					return errors.New("v3 reading evidence is outside its exact ruby outcome")
				}
				evidence.FixedIdentityKey = fixedIdentity.RenditionKey
			}
		}
	}
	return nil
}

func cloneTabPaths(input []model.LyricsSourceTabPath) []model.LyricsSourceTabPath {
	if input == nil {
		return nil
	}
	result := make([]model.LyricsSourceTabPath, len(input))
	for index, path := range input {
		result[index] = append(model.LyricsSourceTabPath(nil), path...)
	}
	return result
}

func renditionTranslationsFromResult(result lyricsrecovery.SongResult) []lyricscontract.RenditionTranslation {
	any := false
	for _, rendition := range result.Renditions {
		any = any || rendition.Translations != nil || len(rendition.PeerTranslations) != 0
	}
	if !any {
		return nil
	}
	translations := make([]lyricscontract.RenditionTranslation, len(result.Renditions))
	for index, rendition := range result.Renditions {
		translations[index] = lyricscontract.RenditionTranslation{
			RenditionKey: rendition.RenditionKey,
			Translations: append([]string(nil), rendition.Translations...),
		}
		if rendition.Translations == nil {
			translations[index].Translations = nil
		}
		translations[index].PeerTranslations = make([]lyricscontract.RenditionPeerTranslation, len(rendition.PeerTranslations))
		for peerIndex, peer := range rendition.PeerTranslations {
			translations[index].PeerTranslations[peerIndex] = lyricscontract.RenditionPeerTranslation{
				Side: peer.Side, Locale: peer.Locale,
				Translations: append([]string(nil), peer.Translations...),
			}
			if peer.Translations == nil {
				translations[index].PeerTranslations[peerIndex].Translations = nil
			}
		}
		if rendition.PeerTranslations == nil {
			translations[index].PeerTranslations = nil
		}
	}
	return translations
}

type v3FixedIdentityOwner struct {
	OutcomeID    string
	RenditionKey string
}

func buildV3SourceDocument(
	result lyricsrecovery.SongResult,
	outcomes []lyricsoutcomeartifact.Artifact,
	resolver *lyricsevidencepack.Resolver,
	segmentationPolicy lyricssource.PerformerSegmentationPolicy,
	reviewResolver *lyricsreview.Resolver,
) (model.LyricsSourceDocument, []lyricsstaging.Artifact, error) {
	payloads := make([]model.LyricsSourceRendition, len(result.Renditions))
	owners := make(map[string]v3FixedIdentityOwner)
	for index, rendition := range result.Renditions {
		payload, err := sourceRenditionFromResult(rendition)
		if err != nil {
			return model.LyricsSourceDocument{}, nil, err
		}
		payloads[index] = payload
		for _, component := range rendition.Components {
			fixedIdentityKey := v3FixedIdentityKey(component.OutcomeID, rendition.RenditionKey)
			owner := v3FixedIdentityOwner{OutcomeID: component.OutcomeID, RenditionKey: rendition.RenditionKey}
			if existing, found := owners[fixedIdentityKey]; found && existing != owner {
				return model.LyricsSourceDocument{}, nil, errors.New("v3 fixed identity derivation collided")
			}
			owners[fixedIdentityKey] = owner
		}
	}
	if len(owners) == 0 {
		return model.LyricsSourceDocument{}, nil, errors.New("v3 result has no source component outcomes")
	}
	outcomeByID := make(map[string]lyricsoutcomeartifact.Artifact, len(outcomes))
	for _, outcome := range outcomes {
		if err := lyricsoutcomeartifact.Validate(outcome); err != nil {
			return model.LyricsSourceDocument{}, nil, err
		}
		outcomeByID[outcome.OutcomeID] = outcome
	}
	keys := make([]string, 0, len(owners))
	for key := range owners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	identities := make([]model.LyricsSourceFixedIdentity, 0, len(keys))
	artifacts := make([]lyricsstaging.Artifact, 0, len(keys))
	for _, fixedIdentityKey := range keys {
		owner := owners[fixedIdentityKey]
		outcome, found := outcomeByID[owner.OutcomeID]
		if !found || outcome.Candidate == nil {
			return model.LyricsSourceDocument{}, nil, fmt.Errorf("v3 component outcome %q is unavailable", owner.OutcomeID)
		}
		identity, artifact, err := v3OutcomeArtifact(
			outcome, result, fixedIdentityKey, owner.RenditionKey,
			resolver, segmentationPolicy, reviewResolver,
		)
		if err != nil {
			return model.LyricsSourceDocument{}, nil, err
		}
		identities = append(identities, identity)
		artifacts = append(artifacts, artifact)
	}
	document := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersionV3,
		FixedIdentities: identities, Renditions: payloads,
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		return model.LyricsSourceDocument{}, nil, fmt.Errorf("v3 source document: %w", err)
	}
	return document, artifacts, nil
}

func v3OutcomeArtifact(
	outcome lyricsoutcomeartifact.Artifact,
	result lyricsrecovery.SongResult,
	fixedIdentityKey string,
	compositionRenditionKey string,
	resolver *lyricsevidencepack.Resolver,
	segmentationPolicy lyricssource.PerformerSegmentationPolicy,
	reviewResolver *lyricsreview.Resolver,
) (model.LyricsSourceFixedIdentity, lyricsstaging.Artifact, error) {
	if resolver == nil || outcome.Candidate == nil || len(outcome.Acquisitions) == 0 {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("v3 outcome requires an exact candidate and evidence")
	}
	candidate := *outcome.Candidate
	if len(outcome.Acquisitions) == 0 {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("v3 outcome has no acquisitions")
	}
	var revisionEvidence lyricssource.IndexEvidence
	indexRefs := make([]model.LyricsSourceIndexEvidenceRef, len(outcome.Acquisitions))
	for index, acquisition := range outcome.Acquisitions {
		indexRefs[index] = model.LyricsSourceIndexEvidenceRef{EvidenceID: acquisition.EvidenceID, SHA256: acquisition.SHA256}
		selected, found := selectedEvidence(result.SelectedEvidence, acquisition.EvidenceID)
		if !found || !matchesV3SelectedAcquisition(selected, outcome.Provider, acquisition) {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("v3 outcome acquisition is outside the selected exact evidence")
		}
		evidence, err := resolver.HydrateExact(selected)
		if err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
		}
		if evidence.PageID == candidate.PageID && evidence.RevisionID == candidate.RevisionID {
			if revisionEvidence.EvidenceID != "" {
				return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("v3 outcome resolves multiple revision evidence envelopes")
			}
			revisionEvidence = evidence
		}
	}
	if revisionEvidence.EvidenceID == "" {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("v3 outcome has no exact revision evidence")
	}
	var raw []byte
	section := "Lyrics"
	switch outcome.Provider {
	case model.LyricsSourceProviderSekaipedia:
		projection, err := lyricssourceoffline.RecoverSekaipediaProjectionWithReview(
			revisionEvidence.Raw,
			lyricssource.FixedIndex{PageID: candidate.PageID, RevisionID: candidate.RevisionID,
				RevisionTimestamp: revisionEvidence.RevisionTimestamp, SHA1: candidate.SHA1,
				ContentSHA256: candidate.RawSHA256, Title: revisionEvidence.Title},
			segmentationPolicy, result.MusicID, reviewResolver,
		)
		if err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, fmt.Errorf("reparse exact Sekaipedia v3 revision: %w", err)
		}
		if err := compareV3Projection(result, outcome.OutcomeID, candidate, projection); err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
		}
		raw = append([]byte(nil), projection.FixedJapaneseWikitext...)
		section = projection.Section
	case model.LyricsSourceProviderMoegirlPublicExact:
		raw = append([]byte(nil), revisionEvidence.Raw...)
		if candidate.RawSHA256 == "" || revisionEvidence.RawSHA256 != candidate.RawSHA256 {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("v3 exact-public raw bytes do not match the provider candidate")
		}
		rendition, err := exactPublicV3Rendition(result, outcome.OutcomeID, candidate)
		if err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
		}
		if err := validateExactPublicRecoveryProjection(raw, revisionEvidence, candidate, *rendition.Full, rendition.Translations); err != nil {
			return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
		}
	default:
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, errors.New("v3 recovery import provider is outside the reviewed exact partition")
	}
	identity := model.LyricsSourceFixedIdentity{
		Provider: outcome.Provider, Origin: revisionEvidence.Origin,
		PageID: candidate.PageID, RevisionID: candidate.RevisionID,
		RevisionTimestamp: revisionEvidence.RevisionTimestamp, SHA1: candidate.SHA1,
		Title: revisionEvidence.Title, CanonicalURL: revisionEvidence.CanonicalURL,
		FetchedAt: revisionEvidence.FetchedAt, Categories: append([]string{}, revisionEvidence.Categories...),
		Section: section, RenditionKey: fixedIdentityKey, CompositionRenditionKey: compositionRenditionKey,
		VersionReason: candidate.VersionReason, IndexEvidenceRefs: indexRefs,
	}
	if err := model.ValidateLyricsSourceFixedIdentity(identity); err != nil {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
	}
	artifact, err := lyricsstaging.NewRecoveryArtifact(identity, raw)
	if err != nil {
		return model.LyricsSourceFixedIdentity{}, lyricsstaging.Artifact{}, err
	}
	return identity, artifact, nil
}

func matchesV3SelectedAcquisition(
	selected lyricscontract.EvidenceRef,
	provider model.LyricsSourceProvider,
	acquisition lyricsoutcomeartifact.AcquisitionRef,
) bool {
	return selected.Provider == provider && selected.AcquisitionID == acquisition.AcquisitionID &&
		selected.SHA256 == acquisition.SHA256 && selected.EnvelopeSHA256 == acquisition.EnvelopeSHA256
}

func exactPublicV3Rendition(
	result lyricsrecovery.SongResult,
	outcomeID string,
	candidate lyricsoutcomeartifact.CandidateIdentity,
) (*lyricsrecovery.SongResultRendition, error) {
	if candidate.RenditionKey != "full-vocaloid" ||
		candidate.VersionReason != model.LyricsSourceVersionReasonUntaggedFullOnly {
		return nil, errors.New("v3 exact-public candidate is not the canonical Full Vocaloid rendition")
	}
	var matching *lyricsrecovery.SongResultRendition
	for index := range result.Renditions {
		rendition := &result.Renditions[index]
		if !v3RenditionUsesOutcome(*rendition, outcomeID) {
			continue
		}
		if matching != nil {
			return nil, errors.New("v3 exact-public outcome ambiguously owns multiple rendition families")
		}
		matching = rendition
	}
	if matching == nil || matching.RenditionKey != "vocaloid" ||
		matching.SourceKind != model.LyricsSourceRenditionVocaloid ||
		matching.ReasonCode != candidate.VersionReason || matching.Full == nil || matching.Game != nil ||
		matching.Relation.Kind != model.LyricsSourceRenditionRelationNone || len(matching.PeerTranslations) != 0 {
		return nil, errors.New("v3 exact-public outcome does not match one Full-only Vocaloid rendition")
	}
	return matching, nil
}

func compareV3Projection(
	result lyricsrecovery.SongResult,
	outcomeID string,
	candidate lyricsoutcomeartifact.CandidateIdentity,
	projection lyricssource.SekaipediaRecoveryProjection,
) error {
	if projection.RenditionKey != candidate.RenditionKey || projection.ReasonCode != candidate.VersionReason {
		return errors.New("reparsed Sekaipedia v3 rendition identity does not match the provider outcome")
	}
	family, found := v3CandidateRenditionFamily(candidate.RenditionKey)
	if !found {
		return errors.New("reparsed Sekaipedia v3 provider rendition family is invalid")
	}
	var matching *lyricsrecovery.SongResultRendition
	for index := range result.Renditions {
		rendition := &result.Renditions[index]
		if rendition.RenditionKey != family || !v3RenditionUsesOutcome(*rendition, outcomeID) {
			continue
		}
		if matching != nil {
			return errors.New("reparsed Sekaipedia v3 revision ambiguously owns a rendition family")
		}
		matching = rendition
	}
	if matching == nil || string(matching.SourceKind) != family {
		return errors.New("reparsed Sekaipedia v3 revision has no matching rendition family")
	}
	if matching.ReasonCode != candidate.VersionReason {
		return errors.New("reparsed Sekaipedia v3 version reason does not match the recovery rendition")
	}
	if projection.Full.Lines != nil && matching.Full == nil || projection.Full.Lines == nil && matching.Full != nil {
		return errors.New("reparsed Sekaipedia v3 Full presence differs from the recovery rendition")
	}
	if matching.Full != nil {
		comparableFull, err := comparableV3ProjectionFull(
			&projection.Full, matching.Full, *matching, model.LyricsSourceRenditionSideFull,
		)
		if err != nil || !reflect.DeepEqual(legacyComparableV3Full(matching.Full), comparableFull) {
			return errors.New("reparsed Sekaipedia v3 Full does not match the recovery rendition")
		}
	}
	comparableGame, err := comparableV3ProjectionFull(
		projection.Game, matching.Game, *matching, model.LyricsSourceRenditionSideGame,
	)
	if err != nil || !reflect.DeepEqual(legacyComparableV3Full(matching.Game), comparableGame) ||
		!reflect.DeepEqual(matching.Relation, projectionRelation(projection, family)) {
		return errors.New("reparsed Sekaipedia v3 Game or relation does not match the recovery rendition")
	}
	return nil
}

func v3CandidateRenditionFamily(renditionKey string) (string, bool) {
	switch renditionKey {
	case "full-original":
		return "original", true
	case "full-sekai", "game-sekai":
		return "sekai", true
	case "full-vocaloid", "game-vocaloid":
		return "vocaloid", true
	default:
		return "", false
	}
}

func v3RenditionUsesOutcome(rendition lyricsrecovery.SongResultRendition, outcomeID string) bool {
	for _, component := range rendition.Components {
		if component.OutcomeID == outcomeID {
			return true
		}
	}
	return false
}

func comparableV3ProjectionFull(
	full *model.LyricsSourceFull,
	recovered *model.LyricsSourceFull,
	rendition lyricsrecovery.SongResultRendition,
	side model.LyricsSourceRenditionSide,
) (*model.LyricsSourceFull, error) {
	if full == nil {
		return nil, nil
	}
	comparable, err := lyricscontract.NormalizePersistedPerformerMetadata(*full)
	if err != nil {
		return nil, err
	}
	generatorVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(comparable.RubyGeneratorVersion)
	if err != nil {
		return nil, err
	}
	comparable.RubyGeneratorVersion = generatorVersion
	segmentationComponent := model.LyricsSourceRenditionComponentFullPerformerSegmentation
	if side == model.LyricsSourceRenditionSideGame {
		segmentationComponent = model.LyricsSourceRenditionComponentGamePerformerSegmentation
	} else if side != model.LyricsSourceRenditionSideFull {
		return nil, errors.New("v3 projection comparison has an invalid rendition side")
	}
	if !v3RenditionHasComponent(rendition, segmentationComponent) {
		comparable.Performers = []model.LyricsSourcePerformer{}
		for lineIndex := range comparable.Lines {
			line := &comparable.Lines[lineIndex]
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
	if recovered != nil && recovered.RubyGeneratorVersion == "" {
		comparable.RubyGeneratorVersion = ""
		for lineIndex := range comparable.Lines {
			for segmentIndex := range comparable.Lines[lineIndex].Segments {
				segment := &comparable.Lines[lineIndex].Segments[segmentIndex]
				segment.Ruby = []model.LyricsSourceRubySpan{{Text: segment.Text}}
			}
		}
	}
	return &comparable, nil
}

func v3RenditionHasComponent(
	rendition lyricsrecovery.SongResultRendition,
	kind model.LyricsSourceRenditionComponentKind,
) bool {
	for _, component := range rendition.Components {
		if component.Component == kind {
			return true
		}
	}
	return false
}

func legacyComparableV3Full(full *model.LyricsSourceFull) *model.LyricsSourceFull {
	cloned := model.CloneLyricsSourceFull(full)
	if cloned == nil {
		return nil
	}
	for lineIndex := range cloned.Lines {
		for segmentIndex := range cloned.Lines[lineIndex].Segments {
			for spanIndex := range cloned.Lines[lineIndex].Segments[segmentIndex].Ruby {
				cloned.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = nil
			}
		}
	}
	return cloned
}

func projectionRelation(
	projection lyricssource.SekaipediaRecoveryProjection,
	compositionRenditionKey string,
) model.LyricsSourceRenditionRelation {
	if projection.GameProjection == nil {
		return model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone}
	}
	return model.LyricsSourceRenditionRelation{
		Kind:             model.LyricsSourceRenditionRelationExactProjection,
		FullRenditionKey: compositionRenditionKey,
		LineIDs:          append([]string(nil), projection.GameProjection.LineIDs...),
	}
}
