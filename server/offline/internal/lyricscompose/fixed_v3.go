package lyricscompose

import (
	"fmt"
	"sort"
	"strings"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

type fixedRenditionCandidate struct {
	input     FixedArtifactInput
	rendition model.LyricsSourceRendition
}

func fixedArtifactInputsContainV3(inputs []FixedArtifactInput) bool {
	for _, input := range inputs {
		if input.Fixed.Document != nil &&
			input.Fixed.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			return true
		}
	}
	return false
}

func composeFixedArtifactRenditions(inputs []FixedArtifactInput) (FixedArtifactComposition, error) {
	if len(inputs) == 0 {
		return FixedArtifactComposition{}, fmt.Errorf("%w: no fixed artifacts", ErrInvalidSource)
	}
	groups := make(map[string][]FixedArtifactInput)
	metadata := make(map[string]fixedRenditionCandidate)
	for _, input := range inputs {
		if input.Fixed.Document == nil {
			return FixedArtifactComposition{}, fmt.Errorf("%w: plural composition requires a closed source document", ErrInvalidSource)
		}
		document := *input.Fixed.Document
		if document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV2 {
			upgraded, err := model.UpconvertLyricsSourceDocumentV2(document)
			if err != nil {
				return FixedArtifactComposition{}, fmt.Errorf("%w: up-convert fixed source: %v", ErrInvalidSource, err)
			}
			document = upgraded
		} else if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
			return FixedArtifactComposition{}, fmt.Errorf("%w: plural composition requires source v2 or v3", ErrInvalidSource)
		}
		if err := model.ValidateLyricsSourceDocument(document); err != nil {
			return FixedArtifactComposition{}, fmt.Errorf("%w: plural source document: %v", ErrInvalidSource, err)
		}
		for _, rendition := range document.Renditions {
			synthetic, err := fixedRenditionAsV2Input(input, document, rendition)
			if err != nil {
				return FixedArtifactComposition{}, err
			}
			groups[rendition.RenditionKey] = append(groups[rendition.RenditionKey], synthetic)
			metadata[fixedRenditionCandidateKey(input.SourceKey, rendition.RenditionKey)] = fixedRenditionCandidate{
				input: input, rendition: rendition,
			}
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	renditions := make([]model.LyricsSourceRendition, 0, len(keys))
	for _, key := range keys {
		composed, err := composeFixedArtifactsV2(groups[key])
		if err != nil {
			return FixedArtifactComposition{}, err
		}
		if len(composed.Renditions) != 0 || len(composed.AlternateVocals) != 0 {
			return FixedArtifactComposition{}, fmt.Errorf("%w: nested or auxiliary rendition composition is invalid", ErrVersionConflict)
		}
		owner, found := metadata[fixedRenditionCandidateKey(composed.Components.VersionEvidence, key)]
		if !found {
			return FixedArtifactComposition{}, fmt.Errorf("%w: plural version owner is unavailable", ErrIdentityMismatch)
		}
		rendition, err := fixedCompositionAsRendition(key, owner.rendition.SourceTabPaths, composed)
		if err != nil {
			return FixedArtifactComposition{}, err
		}
		if err := restoreFixedRenditionV3Metadata(&rendition, owner, metadata, composed); err != nil {
			return FixedArtifactComposition{}, err
		}
		if err := model.ValidateLyricsSourceRenditionPayload(rendition); err != nil {
			return FixedArtifactComposition{}, fmt.Errorf("%w: peer rendition payload: %v", ErrInvalidSource, err)
		}
		if _, err := model.EnumerateLyricsSourceRenditionComponents([]model.LyricsSourceRendition{rendition}); err != nil {
			return FixedArtifactComposition{}, fmt.Errorf("%w: peer rendition provenance: %v", ErrInvalidSource, err)
		}
		renditions = append(renditions, rendition)
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(renditions)
	if err != nil {
		return FixedArtifactComposition{}, fmt.Errorf("%w: composed peer renditions: %v", ErrInvalidSource, err)
	}
	selectedSet := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		selectedSet[binding.FixedIdentityKey] = struct{}{}
	}
	selected := make([]string, 0, len(selectedSet))
	for sourceKey := range selectedSet {
		selected = append(selected, sourceKey)
	}
	sort.Strings(selected)
	return FixedArtifactComposition{Renditions: renditions, SelectedSourceKeys: selected}, nil
}

func fixedRenditionCandidateKey(sourceKey, renditionKey string) string {
	return sourceKey + "\x00" + renditionKey
}

func fixedRenditionAsV2Input(
	input FixedArtifactInput,
	document model.LyricsSourceDocument,
	rendition model.LyricsSourceRendition,
) (FixedArtifactInput, error) {
	bindings, err := model.EnumerateLyricsSourceRenditionComponents([]model.LyricsSourceRendition{rendition})
	if err != nil {
		return FixedArtifactInput{}, fmt.Errorf("%w: source rendition components: %v", ErrInvalidSource, err)
	}
	referenced := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		referenced[binding.FixedIdentityKey] = struct{}{}
	}
	identities := make([]model.LyricsSourceFixedIdentity, 0, len(referenced))
	for _, identity := range document.FixedIdentities {
		if _, contributes := referenced[identity.RenditionKey]; !contributes {
			continue
		}
		identity.CompositionRenditionKey = rendition.RenditionKey
		identities = append(identities, identity)
	}
	legacyProvenance := model.LyricsSourceComponentProvenance{
		VersionEvidence: rendition.Provenance.VersionEvidence,
	}
	if rendition.Full != nil {
		legacyProvenance.FullText = *rendition.Provenance.FullText
		legacyProvenance.PerformerSegmentation = cloneModelComponentRef(rendition.Provenance.FullPerformerSegmentation)
		legacyProvenance.Ruby = cloneModelComponentRef(rendition.Provenance.FullRuby)
	} else {
		legacyProvenance.PerformerSegmentation = cloneModelComponentRef(rendition.Provenance.GamePerformerSegmentation)
		legacyProvenance.Ruby = cloneModelComponentRef(rendition.Provenance.GameRuby)
	}
	legacyProvenance.GameText = cloneModelComponentRef(rendition.Provenance.GameText)
	var projection *model.LyricsSourceGameProjection
	if rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
		projection = &model.LyricsSourceGameProjection{LineIDs: append([]string{}, rendition.Relation.LineIDs...)}
		ref := rendition.Provenance.RelationEvidence
		legacyProvenance.GameProjection = &ref
	}
	legacyGame := cloneModelFull(rendition.Game)
	clearModelReadingEvidence(legacyGame)
	preserveGamePerformerMetadata := fixedRenditionV2BridgePreservesPerformerMetadata(
		rendition, model.LyricsSourceRenditionSideGame,
	)
	if legacyGame != nil && !preserveGamePerformerMetadata {
		flattenFixedV2BridgePerformerMetadata(legacyGame)
		if rendition.Full == nil {
			legacyProvenance.PerformerSegmentation = nil
		}
	}
	legacy := model.LyricsSourceDocument{
		SchemaVersion: model.LyricsSourceDocumentSchemaVersionV2,
		ReasonCode:    rendition.ReasonCode, FixedIdentities: identities, Provenance: legacyProvenance,
		Game: legacyGame, GameProjection: projection,
		PrivateReview: clonePrivateReview(rendition.PrivateReview),
	}
	if rendition.Full != nil {
		legacyFull := cloneModelFull(rendition.Full)
		clearModelReadingEvidence(legacyFull)
		preserveFullPerformerMetadata := fixedRenditionV2BridgePreservesPerformerMetadata(
			rendition, model.LyricsSourceRenditionSideFull,
		)
		if !preserveFullPerformerMetadata {
			flattenFixedV2BridgePerformerMetadata(legacyFull)
			legacy.Provenance.PerformerSegmentation = nil
		}
		legacy.Full = *legacyFull
	}
	if err := model.ValidateLyricsSourceDocument(legacy); err != nil {
		return FixedArtifactInput{}, fmt.Errorf("%w: singular rendition bridge: %v", ErrInvalidSource, err)
	}
	result := input
	result.LogicalRenditionKey = rendition.RenditionKey
	result.Fixed.Document = &legacy
	result.Fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, identities...)
	result.Fixed.VersionReason = rendition.ReasonCode
	return result, nil
}

func fixedRenditionV2BridgePreservesPerformerMetadata(
	rendition model.LyricsSourceRendition,
	side model.LyricsSourceRenditionSide,
) bool {
	if rendition.SourceKind != model.LyricsSourceRenditionVocaloid {
		return true
	}
	if rendition.PrivateReview == nil || rendition.PrivateReview.PerformerSegmentationEvidence !=
		model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured {
		return false
	}
	if side == model.LyricsSourceRenditionSideFull {
		return rendition.FullPerformerEvidence == model.LyricsSourcePerformerEvidenceSourceComplete
	}
	return rendition.GamePerformerEvidence == model.LyricsSourcePerformerEvidenceSourceComplete
}

// flattenFixedV2BridgePerformerMetadata creates the anonymous single-segment
// shape required by the legacy v2 Vocaloid validator. The original v3 side is
// retained in the candidate metadata and restored after v2 composition, so the
// bridge never becomes a source-of-truth transformation.
func flattenFixedV2BridgePerformerMetadata(full *model.LyricsSourceFull) {
	if full == nil {
		return
	}
	full.Performers = []model.LyricsSourcePerformer{}
	for lineIndex := range full.Lines {
		line := &full.Lines[lineIndex]
		spans := make([]model.LyricsSourceRubySpan, 0)
		for _, segment := range line.Segments {
			spans = append(spans, segment.Ruby...)
		}
		line.Segments = []model.LyricsSourceSegment{{
			Text:         line.Text,
			PerformerIDs: []string{},
			Ruby:         spans,
		}}
		line.TrailingPerformerIDs = []string{}
	}
}

func fixedCompositionAsRendition(
	key string,
	paths []model.LyricsSourceTabPath,
	composition FixedArtifactComposition,
) (model.LyricsSourceRendition, error) {
	if len(composition.Full.Lines) == 0 && composition.Game == nil {
		return model.LyricsSourceRendition{}, fmt.Errorf("%w: peer rendition has no text component", ErrComponentsIncomplete)
	}
	kind := ""
	if len(composition.Full.Lines) != 0 {
		kind = composition.Full.Version.Kind
	}
	if composition.Game != nil {
		if kind != "" && composition.Game.Version.Kind != kind {
			return model.LyricsSourceRendition{}, fmt.Errorf("%w: peer Full/Game source kinds differ", ErrVersionConflict)
		}
		kind = composition.Game.Version.Kind
	}
	if !model.IsValidLyricsSourceRenditionKind(model.LyricsSourceRenditionKind(kind)) {
		return model.LyricsSourceRendition{}, fmt.Errorf("%w: peer rendition source kind is invalid", ErrVersionConflict)
	}
	rendition := model.LyricsSourceRendition{
		RenditionKey: key, SourceKind: model.LyricsSourceRenditionKind(kind),
		SourceTabPaths: cloneModelTabPaths(paths), ReasonCode: composition.ReasonCode,
		Relation:      model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone},
		PrivateReview: clonePrivateReview(composition.PrivateReview),
	}
	if len(composition.Full.Lines) != 0 {
		rendition.Full = cloneModelFull(&composition.Full)
		rendition.Provenance.FullText = modelComponentRef(composition.Components.FullText)
		rendition.Provenance.FullPerformerSegmentation = modelComponentRef(composition.Components.PerformerSegmentation)
		rendition.Provenance.FullRuby = modelComponentRef(composition.Components.Ruby)
	}
	if composition.Game != nil {
		rendition.Game = cloneModelFull(composition.Game)
		rendition.Provenance.GameText = modelComponentRef(composition.Components.GameText)
		if len(composition.Full.Lines) == 0 {
			rendition.Provenance.GamePerformerSegmentation = modelComponentRef(composition.Components.PerformerSegmentation)
			rendition.Provenance.GameRuby = modelComponentRef(composition.Components.Ruby)
		} else {
			if persistedFullHasPerformerSegmentation(*composition.Game) {
				rendition.Provenance.GamePerformerSegmentation = modelComponentRef(composition.Components.GameText)
			}
			if fullHasRuby(*composition.Game) {
				rendition.Provenance.GameRuby = modelComponentRef(composition.Components.GameText)
			}
		}
	}
	if composition.GameProjection != nil {
		rendition.Relation = model.LyricsSourceRenditionRelation{
			Kind: model.LyricsSourceRenditionRelationExactProjection, FullRenditionKey: key,
			LineIDs: append([]string{}, composition.GameProjection.LineIDs...),
		}
		rendition.Provenance.RelationEvidence = model.LyricsSourceComponentRef{
			RenditionKey: composition.Components.GameProjection,
		}
	} else if composition.Components.GameText != "" {
		rendition.Provenance.RelationEvidence = model.LyricsSourceComponentRef{
			RenditionKey: composition.Components.GameText,
		}
	} else {
		rendition.Provenance.RelationEvidence = model.LyricsSourceComponentRef{
			RenditionKey: composition.Components.VersionEvidence,
		}
	}
	rendition.Provenance.VersionEvidence = model.LyricsSourceComponentRef{
		RenditionKey: composition.Components.VersionEvidence,
	}
	return rendition, nil
}

func restoreFixedRenditionV3Metadata(
	target *model.LyricsSourceRendition,
	versionOwner fixedRenditionCandidate,
	metadata map[string]fixedRenditionCandidate,
	composition FixedArtifactComposition,
) error {
	if target == nil {
		return fmt.Errorf("%w: peer rendition target is nil", ErrInvalidSource)
	}
	sourcePerformerIDs, err := normalizedFixedSourcePerformerIDs(versionOwner.rendition)
	if err != nil {
		return err
	}
	target.SourcePerformerIDs = sourcePerformerIDs
	target.FullPerformerEvidence = model.LyricsSourcePerformerEvidenceNone
	target.GamePerformerEvidence = model.LyricsSourcePerformerEvidenceNone
	candidateFor := func(sourceKey string) (fixedRenditionCandidate, error) {
		candidate, found := metadata[fixedRenditionCandidateKey(sourceKey, target.RenditionKey)]
		if !found {
			return fixedRenditionCandidate{}, fmt.Errorf("%w: selected peer component owner is unavailable", ErrIdentityMismatch)
		}
		candidateSourcePerformerIDs, err := normalizedFixedSourcePerformerIDs(candidate.rendition)
		if err != nil {
			return fixedRenditionCandidate{}, err
		}
		if !equalFixedStrings(candidateSourcePerformerIDs, target.SourcePerformerIDs) {
			return fixedRenditionCandidate{}, fmt.Errorf("%w: selected peer source rosters differ", ErrVersionConflict)
		}
		return candidate, nil
	}

	restorePerformerSide := func(
		side *model.LyricsSourceFull,
		renditionSide model.LyricsSourceRenditionSide,
		preferredSourceKey string,
		evidence *model.LyricsSourcePerformerEvidenceState,
		provenance **model.LyricsSourceComponentRef,
	) error {
		candidate, found, err := selectFixedRenditionPerformerCandidate(
			*target, metadata, preferredSourceKey, renditionSide, target.SourcePerformerIDs,
		)
		if err != nil {
			return err
		}
		if !found {
			if persistedFullHasPerformerSegmentation(*side) {
				return fmt.Errorf("%w: composed performer segmentation owner is unavailable", ErrIdentityMismatch)
			}
			*evidence = model.LyricsSourcePerformerEvidenceNone
			*provenance = nil
			return nil
		}
		source := fixedRenditionSideFull(candidate.rendition, renditionSide)
		if source == nil {
			return fmt.Errorf("%w: selected performer segmentation owner has no side", ErrIdentityMismatch)
		}
		if err := copyFixedPerformerSegmentation(side, source); err != nil {
			return err
		}
		*evidence = fixedRenditionSideEvidence(candidate.rendition, renditionSide)
		*provenance = modelComponentRef(candidate.input.SourceKey)
		return nil
	}

	if target.Full != nil {
		if err := restorePerformerSide(
			target.Full, model.LyricsSourceRenditionSideFull,
			composition.Components.PerformerSegmentation,
			&target.FullPerformerEvidence, &target.Provenance.FullPerformerSegmentation,
		); err != nil {
			return err
		}
		if composition.Components.Ruby != "" {
			candidate, err := candidateFor(composition.Components.Ruby)
			if err != nil {
				return err
			}
			if candidate.rendition.Full == nil {
				return fmt.Errorf("%w: selected Full ruby owner has no Full side", ErrIdentityMismatch)
			}
			if err := copyFixedReadingEvidence(
				target.Full, candidate.rendition.Full, target.RenditionKey,
				model.LyricsSourceRenditionSideFull, composition.Components.Ruby,
			); err != nil {
				return err
			}
			if candidate.rendition.Provenance.FullRuby == nil {
				target.Provenance.FullRuby = nil
			}
		}
	}

	if target.Game != nil {
		segmentationOwner := composition.Components.GameText
		rubyOwner := composition.Components.GameText
		if target.Full == nil {
			segmentationOwner = composition.Components.PerformerSegmentation
			rubyOwner = composition.Components.Ruby
		}
		if err := restorePerformerSide(
			target.Game, model.LyricsSourceRenditionSideGame,
			segmentationOwner,
			&target.GamePerformerEvidence, &target.Provenance.GamePerformerSegmentation,
		); err != nil {
			return err
		}
		if rubyOwner != "" {
			candidate, err := candidateFor(rubyOwner)
			if err != nil {
				return err
			}
			if candidate.rendition.Game == nil {
				return fmt.Errorf("%w: selected Game ruby owner has no Game side", ErrIdentityMismatch)
			}
			if err := copyFixedReadingEvidence(
				target.Game, candidate.rendition.Game, target.RenditionKey,
				model.LyricsSourceRenditionSideGame, rubyOwner,
			); err != nil {
				return err
			}
			if candidate.rendition.Provenance.GameRuby == nil {
				target.Provenance.GameRuby = nil
			}
		}
	}
	return nil
}

func selectFixedRenditionPerformerCandidate(
	target model.LyricsSourceRendition,
	metadata map[string]fixedRenditionCandidate,
	preferredSourceKey string,
	side model.LyricsSourceRenditionSide,
	sourcePerformerIDs []string,
) (fixedRenditionCandidate, bool, error) {
	keys := make([]string, 0, len(metadata))
	for key, candidate := range metadata {
		if candidate.rendition.RenditionKey == target.RenditionKey {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if preferredSourceKey != "" {
		preferred := fixedRenditionCandidateKey(preferredSourceKey, target.RenditionKey)
		found := false
		for _, key := range keys {
			if key == preferred {
				found = true
				break
			}
		}
		if !found {
			return fixedRenditionCandidate{}, false, fmt.Errorf("%w: selected peer component owner is unavailable", ErrIdentityMismatch)
		}
		keys = append([]string{preferred}, keys...)
	}

	var selected fixedRenditionCandidate
	selectedFound := false
	seen := map[string]struct{}{}
	for _, key := range keys {
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidate := metadata[key]
		candidateSourcePerformerIDs, err := normalizedFixedSourcePerformerIDs(candidate.rendition)
		if err != nil {
			return fixedRenditionCandidate{}, false, err
		}
		if !equalFixedStrings(candidateSourcePerformerIDs, sourcePerformerIDs) {
			if key == fixedRenditionCandidateKey(preferredSourceKey, target.RenditionKey) {
				return fixedRenditionCandidate{}, false, fmt.Errorf("%w: selected peer source rosters differ", ErrVersionConflict)
			}
			continue
		}
		full := fixedRenditionSideFull(candidate.rendition, side)
		if full == nil || fixedRenditionSideEvidence(candidate.rendition, side) ==
			model.LyricsSourcePerformerEvidenceNone || !persistedFullHasPerformerSegmentation(*full) {
			continue
		}
		if !selectedFound {
			selected = candidate
			selectedFound = true
			continue
		}
		selectedFull := fixedRenditionSideFull(selected.rendition, side)
		normalizedSelected, err := lyricscontract.NormalizePersistedPerformerMetadata(*selectedFull)
		if err != nil {
			return fixedRenditionCandidate{}, false, err
		}
		normalizedFull, err := lyricscontract.NormalizePersistedPerformerMetadata(*full)
		if err != nil {
			return fixedRenditionCandidate{}, false, err
		}
		if !fixedPerformerSegmentationEquivalent(&normalizedSelected, &normalizedFull) {
			return fixedRenditionCandidate{}, false, fmt.Errorf("%w: peer performer segmentation sources conflict", ErrComponentConflict)
		}
	}
	return selected, selectedFound, nil
}

func fixedRenditionSideFull(
	rendition model.LyricsSourceRendition,
	side model.LyricsSourceRenditionSide,
) *model.LyricsSourceFull {
	if side == model.LyricsSourceRenditionSideFull {
		return rendition.Full
	}
	return rendition.Game
}

func fixedRenditionSideEvidence(
	rendition model.LyricsSourceRendition,
	side model.LyricsSourceRenditionSide,
) model.LyricsSourcePerformerEvidenceState {
	if side == model.LyricsSourceRenditionSideFull {
		return rendition.FullPerformerEvidence
	}
	return rendition.GamePerformerEvidence
}

func fixedPerformerSegmentationEquivalent(left, right *model.LyricsSourceFull) bool {
	if left == nil || right == nil || len(left.Performers) != len(right.Performers) || len(left.Lines) != len(right.Lines) {
		return false
	}
	for index := range left.Performers {
		if left.Performers[index] != right.Performers[index] {
			return false
		}
	}
	for lineIndex := range left.Lines {
		leftLine, rightLine := left.Lines[lineIndex], right.Lines[lineIndex]
		if leftLine.Text != rightLine.Text || !equalFixedStrings(leftLine.TrailingPerformerIDs, rightLine.TrailingPerformerIDs) || len(leftLine.Segments) != len(rightLine.Segments) {
			return false
		}
		for segmentIndex := range leftLine.Segments {
			leftSegment, rightSegment := leftLine.Segments[segmentIndex], rightLine.Segments[segmentIndex]
			if leftSegment.Text != rightSegment.Text || !equalFixedStrings(leftSegment.PerformerIDs, rightSegment.PerformerIDs) {
				return false
			}
		}
	}
	return true
}

func copyFixedPerformerSegmentation(target, source *model.LyricsSourceFull) error {
	if target == nil || source == nil || len(target.Lines) != len(source.Lines) {
		return fmt.Errorf("%w: selected performer source line shape differs", ErrComponentConflict)
	}
	normalizedSource, err := lyricscontract.NormalizePersistedPerformerMetadata(*source)
	if err != nil {
		return err
	}
	source = &normalizedSource
	for lineIndex := range target.Lines {
		if target.Lines[lineIndex].Text != source.Lines[lineIndex].Text {
			return fmt.Errorf("%w: selected performer source text differs", ErrComponentConflict)
		}
		segments, err := repartitionFixedRubySpans(target.Lines[lineIndex], source.Lines[lineIndex])
		if err != nil {
			return err
		}
		for segmentIndex := range segments {
			segments[segmentIndex].PerformerIDs = append([]string{}, source.Lines[lineIndex].Segments[segmentIndex].PerformerIDs...)
		}
		target.Lines[lineIndex].Segments = segments
		target.Lines[lineIndex].TrailingPerformerIDs = append([]string{}, source.Lines[lineIndex].TrailingPerformerIDs...)
	}
	target.Performers = append([]model.LyricsSourcePerformer{}, source.Performers...)
	return nil
}

func repartitionFixedRubySpans(
	targetLine, sourceLine model.LyricsSourceFullLine,
) ([]model.LyricsSourceSegment, error) {
	if len(sourceLine.Segments) == 0 {
		return nil, fmt.Errorf("%w: selected performer source has no segments", ErrComponentConflict)
	}
	targetSpans := flattenModelRubySpans(targetLine)
	spanIndex := 0
	spanOffset := 0
	segments := make([]model.LyricsSourceSegment, len(sourceLine.Segments))
	for segmentIndex, sourceSegment := range sourceLine.Segments {
		segments[segmentIndex] = model.LyricsSourceSegment{
			Text:         sourceSegment.Text,
			PerformerIDs: []string{},
			Ruby:         []model.LyricsSourceRubySpan{},
		}
		remaining := sourceSegment.Text
		for len(remaining) > 0 {
			if spanIndex >= len(targetSpans) {
				return nil, fmt.Errorf("%w: selected performer source exceeds target ruby spans", ErrComponentConflict)
			}
			span := *targetSpans[spanIndex]
			if spanOffset >= len(span.Text) {
				return nil, fmt.Errorf("%w: selected performer source has an invalid ruby cursor", ErrComponentConflict)
			}
			available := span.Text[spanOffset:]
			var take int
			switch {
			case strings.HasPrefix(remaining, available):
				take = len(available)
			case strings.HasPrefix(available, remaining):
				take = len(remaining)
			default:
				return nil, fmt.Errorf("%w: selected performer source segment and ruby text differ", ErrComponentConflict)
			}
			piece := span
			piece.Text = available[:take]
			if take != len(available) {
				if piece.Reading != "" {
					return nil, fmt.Errorf("%w: selected performer source splits a ruby reading", ErrComponentConflict)
				}
				piece.ReadingEvidence = nil
			}
			segments[segmentIndex].Ruby = append(segments[segmentIndex].Ruby, piece)
			remaining = remaining[take:]
			if take == len(available) {
				spanIndex++
				spanOffset = 0
			} else {
				spanOffset += take
			}
		}
	}
	if spanIndex != len(targetSpans) || spanIndex == len(targetSpans) && spanOffset != 0 {
		return nil, fmt.Errorf("%w: selected performer source does not cover target ruby text", ErrComponentConflict)
	}
	return segments, nil
}

func copyFixedReadingEvidence(
	target, source *model.LyricsSourceFull,
	renditionKey string,
	side model.LyricsSourceRenditionSide,
	fixedIdentityKey string,
) error {
	if target == nil || source == nil || len(target.Lines) != len(source.Lines) {
		return fmt.Errorf("%w: selected ruby source line shape differs", ErrComponentConflict)
	}
	for lineIndex := range target.Lines {
		targetSpans := flattenModelRubySpans(target.Lines[lineIndex])
		sourceSpans := flattenModelRubySpans(source.Lines[lineIndex])
		if len(targetSpans) != len(sourceSpans) {
			return fmt.Errorf("%w: selected ruby source span shape differs", ErrComponentConflict)
		}
		for spanIndex := range targetSpans {
			if targetSpans[spanIndex].Text != sourceSpans[spanIndex].Text ||
				targetSpans[spanIndex].Reading != sourceSpans[spanIndex].Reading {
				return fmt.Errorf("%w: selected ruby source span differs", ErrComponentConflict)
			}
			evidence := sourceSpans[spanIndex].ReadingEvidence
			if evidence == nil {
				targetSpans[spanIndex].ReadingEvidence = nil
				continue
			}
			copy := *evidence
			switch copy.Kind {
			case model.LyricsSourceReadingEvidenceExplicitSourceKana,
				model.LyricsSourceReadingEvidenceSourceTransliteration,
				model.LyricsSourceReadingEvidenceLegacyV2Component:
				copy.FixedIdentityKey = fixedIdentityKey
				copy.RenditionKey = renditionKey
				copy.Side = side
			}
			targetSpans[spanIndex].ReadingEvidence = &copy
		}
	}
	return nil
}

func flattenModelRubySpans(line model.LyricsSourceFullLine) []*model.LyricsSourceRubySpan {
	spans := make([]*model.LyricsSourceRubySpan, 0)
	for segmentIndex := range line.Segments {
		for spanIndex := range line.Segments[segmentIndex].Ruby {
			spans = append(spans, &line.Segments[segmentIndex].Ruby[spanIndex])
		}
	}
	return spans
}

func normalizedFixedSourcePerformerIDs(rendition model.LyricsSourceRendition) ([]string, error) {
	if len(rendition.SourcePerformerIDs) == 0 {
		return nil, nil
	}
	remapped := make(map[string]string, len(rendition.SourcePerformerIDs))
	for _, full := range []*model.LyricsSourceFull{rendition.Full, rendition.Game} {
		if full == nil {
			continue
		}
		normalized, err := lyricscontract.NormalizePersistedPerformerMetadata(*full)
		if err != nil || len(normalized.Performers) != len(full.Performers) {
			return nil, fmt.Errorf("%w: peer source roster cannot be normalized", ErrInvalidSource)
		}
		for index, performer := range full.Performers {
			canonical := normalized.Performers[index].PerformerID
			if existing, found := remapped[performer.PerformerID]; found && existing != canonical {
				return nil, fmt.Errorf("%w: peer source roster normalization conflicts", ErrVersionConflict)
			}
			remapped[performer.PerformerID] = canonical
		}
	}
	result := make([]string, len(rendition.SourcePerformerIDs))
	for index, performerID := range rendition.SourcePerformerIDs {
		canonical, found := remapped[performerID]
		if !found {
			// Native v3 extraction already uses persisted canonical IDs even when a
			// source-partial roster includes an unwitnessed singer.
			canonical = performerID
		}
		result[index] = canonical
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index-1] == result[index] {
			return nil, fmt.Errorf("%w: peer source roster normalization duplicates an ID", ErrVersionConflict)
		}
	}
	return result, nil
}

func equalFixedStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func clearModelReadingEvidence(full *model.LyricsSourceFull) {
	if full == nil {
		return
	}
	for lineIndex := range full.Lines {
		for segmentIndex := range full.Lines[lineIndex].Segments {
			for spanIndex := range full.Lines[lineIndex].Segments[segmentIndex].Ruby {
				full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = nil
			}
		}
	}
}

func modelComponentRef(sourceKey string) *model.LyricsSourceComponentRef {
	if sourceKey == "" {
		return nil
	}
	return &model.LyricsSourceComponentRef{RenditionKey: sourceKey}
}

func cloneModelComponentRef(reference *model.LyricsSourceComponentRef) *model.LyricsSourceComponentRef {
	if reference == nil {
		return nil
	}
	copy := *reference
	return &copy
}

func cloneModelTabPaths(input []model.LyricsSourceTabPath) []model.LyricsSourceTabPath {
	if input == nil {
		return nil
	}
	result := make([]model.LyricsSourceTabPath, len(input))
	for index, path := range input {
		result[index] = append(model.LyricsSourceTabPath{}, path...)
	}
	return result
}
