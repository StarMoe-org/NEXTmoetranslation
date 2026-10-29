package model

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxLyricsSourceRenditions        = 16
	maxLyricsSourceTabPaths          = 32
	maxLyricsSourceTabPathDepth      = 8
	maxLyricsSourceTabPathLabelBytes = 512
)

func IsValidLyricsSourceRenditionKind(kind LyricsSourceRenditionKind) bool {
	switch kind {
	case LyricsSourceRenditionOriginal, LyricsSourceRenditionSekai,
		LyricsSourceRenditionVocaloid, LyricsSourceRenditionAlternate:
		return true
	default:
		return false
	}
}

func IsValidLyricsSourceRenditionRelationKind(kind LyricsSourceRenditionRelationKind) bool {
	return kind == LyricsSourceRenditionRelationNone || kind == LyricsSourceRenditionRelationExactProjection
}

// ValidateLyricsSourceRenditionSetPayload validates the peer rendition data
// independently of a document's fixed-identity graph. Source and recovery v3
// share this semantic boundary; each owning contract validates its own refs.
func ValidateLyricsSourceRenditionSetPayload(renditions []LyricsSourceRendition) error {
	if renditions == nil || len(renditions) == 0 || len(renditions) > maxLyricsSourceRenditions {
		return errors.New("lyrics source v3 requires a bounded non-empty rendition set")
	}
	seenKeys := make(map[string]struct{}, len(renditions))
	seenPaths := make(map[string]string)
	for index, rendition := range renditions {
		if _, duplicate := seenKeys[rendition.RenditionKey]; duplicate {
			return fmt.Errorf("lyrics source v3 repeats rendition key %q", rendition.RenditionKey)
		}
		seenKeys[rendition.RenditionKey] = struct{}{}
		if err := validateLyricsSourceRenditionPayload(index, rendition); err != nil {
			return err
		}
		for _, path := range rendition.SourceTabPaths {
			key := lyricsSourceTabPathKey(path)
			if owner, duplicate := seenPaths[key]; duplicate {
				return fmt.Errorf("lyrics source v3 source tab path is duplicated by renditions %q and %q", owner, rendition.RenditionKey)
			}
			seenPaths[key] = rendition.RenditionKey
		}
	}
	return nil
}

func ValidateLyricsSourceRenditionPayload(rendition LyricsSourceRendition) error {
	return validateLyricsSourceRenditionPayload(0, rendition)
}

func validateLyricsSourceRenditionPayload(index int, rendition LyricsSourceRendition) error {
	prefix := fmt.Sprintf("lyrics source rendition %d", index+1)
	if len(rendition.RenditionKey) > maxLyricsSourceRenditionKeyBytes ||
		!isCanonicalLyricsSourceRenditionKey(rendition.RenditionKey) {
		return fmt.Errorf("%s has an invalid logical key", prefix)
	}
	if !IsValidLyricsSourceRenditionKind(rendition.SourceKind) {
		return fmt.Errorf("%s has unknown source kind %q", prefix, rendition.SourceKind)
	}
	if !IsValidLyricsSourceVersionReasonCode(rendition.ReasonCode) ||
		rendition.ReasonCode == LyricsSourceVersionReasonVersionConflict {
		return fmt.Errorf("%s has an invalid version reason", prefix)
	}
	if err := validateLyricsSourceTabPaths(rendition.SourceTabPaths); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	hasAlternateLabel := lyricsSourceTabPathsDeclareAlternate(rendition.SourceTabPaths)
	if rendition.SourceKind == LyricsSourceRenditionAlternate && !hasAlternateLabel {
		return fmt.Errorf("%s uses alternate without an explicit Alternate/Another Vocal source label", prefix)
	}
	if rendition.SourceKind != LyricsSourceRenditionAlternate && hasAlternateLabel {
		return fmt.Errorf("%s has an explicit alternate/archive source label with non-alternate kind", prefix)
	}
	if err := validateLyricsSourceRenditionSourcePerformerIDs(rendition.SourcePerformerIDs); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if rendition.Full == nil && rendition.Game == nil {
		return fmt.Errorf("%s has no Full or Game component", prefix)
	}
	for component, full := range map[string]*LyricsSourceFull{"Full": rendition.Full, "Game": rendition.Game} {
		if full == nil {
			continue
		}
		if full.Version.Kind != string(rendition.SourceKind) {
			return fmt.Errorf("%s %s version kind %q does not match source kind %q", prefix, component, full.Version.Kind, rendition.SourceKind)
		}
		segmentationRef := rendition.Provenance.FullPerformerSegmentation
		if component == "Game" {
			segmentationRef = rendition.Provenance.GamePerformerSegmentation
		}
		if rendition.SourceKind == LyricsSourceRenditionVocaloid && segmentationRef != nil {
			if err := ValidateLyricsSourceFullWithAuthoritativeVocaloidSegmentation(*full); err != nil {
				return fmt.Errorf("%s %s: %w", prefix, component, err)
			}
		} else if err := ValidateLyricsSourceFull(*full); err != nil {
			return fmt.Errorf("%s %s: %w", prefix, component, err)
		}
	}
	if err := validateLyricsSourceV3Side(
		renderingSideValidation{
			name: "Full", side: LyricsSourceRenditionSideFull, full: rendition.Full,
			performerEvidence: rendition.FullPerformerEvidence,
			rubyReference:     rendition.Provenance.FullRuby,
		}, rendition,
	); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if err := validateLyricsSourceV3Side(
		renderingSideValidation{
			name: "Game", side: LyricsSourceRenditionSideGame, full: rendition.Game,
			performerEvidence: rendition.GamePerformerEvidence,
			rubyReference:     rendition.Provenance.GameRuby,
		}, rendition,
	); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if rendition.Full != nil && rendition.Game != nil {
		fullPerformers := make(map[string]LyricsSourcePerformer, len(rendition.Full.Performers))
		for _, performer := range rendition.Full.Performers {
			fullPerformers[performer.PerformerID] = performer
		}
		for _, performer := range rendition.Game.Performers {
			if existing, shared := fullPerformers[performer.PerformerID]; shared && existing != performer {
				return fmt.Errorf("%s Full and Game performer metadata conflict for shared ID %q", prefix, performer.PerformerID)
			}
		}
	}
	if rendition.PrivateReview != nil {
		if rendition.PrivateReview.PerformerSegmentationEvidence !=
			LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured {
			return fmt.Errorf("%s has an invalid private performer segmentation marker", prefix)
		}
		if rendition.FullPerformerEvidence != LyricsSourcePerformerEvidenceSourceComplete &&
			rendition.GamePerformerEvidence != LyricsSourcePerformerEvidenceSourceComplete {
			return fmt.Errorf("%s has a private complete marker without a complete side", prefix)
		}
	}
	if err := validateLyricsSourceRenditionRelation(rendition); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return validateLyricsSourceRenditionReasonShape(rendition)
}

func validateLyricsSourceTabPaths(paths []LyricsSourceTabPath) error {
	if paths == nil || len(paths) == 0 || len(paths) > maxLyricsSourceTabPaths {
		return errors.New("source tab paths must be a bounded non-empty array")
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == nil || len(path) == 0 || len(path) > maxLyricsSourceTabPathDepth {
			return errors.New("source tab path has invalid depth")
		}
		for _, label := range path {
			if !validLyricsSourceLabel(label, maxLyricsSourceTabPathLabelBytes) {
				return errors.New("source tab path has an invalid label")
			}
		}
		key := lyricsSourceTabPathKey(path)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("source tab paths contain a duplicate")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func lyricsSourceTabPathKey(path LyricsSourceTabPath) string {
	var key strings.Builder
	for _, label := range path {
		key.WriteString(strconv.Itoa(len(label)))
		key.WriteByte(':')
		key.WriteString(label)
		key.WriteByte(';')
	}
	return key.String()
}

func lyricsSourceTabPathsDeclareAlternate(paths []LyricsSourceTabPath) bool {
	for _, path := range paths {
		for _, label := range path {
			var normalized strings.Builder
			previousSpace := true
			for _, current := range strings.ToLower(label) {
				if unicode.IsLetter(current) || unicode.IsNumber(current) {
					normalized.WriteRune(current)
					previousSpace = false
					continue
				}
				if !previousSpace {
					normalized.WriteByte(' ')
					previousSpace = true
				}
			}
			value := " " + strings.TrimSpace(normalized.String()) + " "
			for _, marker := range []string{
				" alternate vocal ", " alternate vocals ",
				" another vocal ", " another vocals ",
				" archive ", " april fools ",
				" alt group covers ", " colorful live ",
				" full version movie ", " game size ",
				" project sekai the movie ", " connect live ",
				" ensemble stars ",
			} {
				if strings.Contains(value, marker) {
					return true
				}
			}
		}
	}
	return false
}

func validateLyricsSourceRenditionSourcePerformerIDs(ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	last := ""
	for _, id := range ids {
		if len(id) == 0 || len(id) > maxLyricsSourcePerformerIDBytes ||
			!isCanonicalLyricsSourcePerformerID(id) || last != "" && last >= id {
			return errors.New("source performer IDs are invalid or not canonically ordered")
		}
		if _, duplicate := seen[id]; duplicate {
			return errors.New("source performer IDs contain a duplicate")
		}
		seen[id] = struct{}{}
		last = id
	}
	return nil
}

type renderingSideValidation struct {
	name              string
	side              LyricsSourceRenditionSide
	full              *LyricsSourceFull
	performerEvidence LyricsSourcePerformerEvidenceState
	rubyReference     *LyricsSourceComponentRef
}

func validateLyricsSourceV3Side(side renderingSideValidation, rendition LyricsSourceRendition) error {
	if side.full == nil {
		if side.performerEvidence != LyricsSourcePerformerEvidenceNone {
			return fmt.Errorf("%s performer evidence exists without the side", side.name)
		}
		return nil
	}
	if err := validateLyricsSourceV3PerformerEvidence(
		*side.full, rendition.SourcePerformerIDs, side.performerEvidence,
	); err != nil {
		return fmt.Errorf("%s performer evidence: %w", side.name, err)
	}
	if err := validateLyricsSourceV3ReadingEvidence(
		*side.full, rendition.RenditionKey, side.side, side.rubyReference,
	); err != nil {
		return fmt.Errorf("%s reading evidence: %w", side.name, err)
	}
	return nil
}

func validateLyricsSourceV3PerformerEvidence(
	full LyricsSourceFull,
	sourcePerformerIDs []string,
	state LyricsSourcePerformerEvidenceState,
) error {
	if len(sourcePerformerIDs) != 0 {
		allowed := make(map[string]struct{}, len(sourcePerformerIDs))
		for _, id := range sourcePerformerIDs {
			allowed[id] = struct{}{}
		}
		for _, performer := range full.Performers {
			if _, ok := allowed[performer.PerformerID]; !ok {
				return fmt.Errorf("performer %q is outside the source roster", performer.PerformerID)
			}
		}
	}
	hasSegmentation := LyricsSourceFullHasPerformerSegmentation(full)
	complete := LyricsSourceFullHasCompletePerformerEvidence(full, sourcePerformerIDs)
	switch state {
	case LyricsSourcePerformerEvidenceNone:
		if hasSegmentation {
			return errors.New("none state contains structured performer data")
		}
	case LyricsSourcePerformerEvidenceSourcePartial:
		// Partial is an evidence-completeness state, not a claim that the
		// persisted assignment must contain an anonymous segment. A bounded
		// source may witness every retained segment while still not proving that
		// its singer roster or nested attribution is globally complete.
		if !hasSegmentation {
			return errors.New("partial state contains no structured assignment")
		}
	case LyricsSourcePerformerEvidenceSourceComplete:
		if !complete {
			return errors.New("complete state lacks exact roster and segment assignment")
		}
	default:
		return fmt.Errorf("unknown state %q", state)
	}
	return nil
}

func LyricsSourceFullHasCompletePerformerEvidence(
	full LyricsSourceFull,
	sourcePerformerIDs []string,
) bool {
	if len(sourcePerformerIDs) == 0 || len(full.Performers) != len(sourcePerformerIDs) || len(full.Lines) == 0 {
		return false
	}
	registry := make([]string, len(full.Performers))
	for index, performer := range full.Performers {
		registry[index] = performer.PerformerID
	}
	sort.Strings(registry)
	if !reflect.DeepEqual(registry, sourcePerformerIDs) {
		return false
	}
	used := make(map[string]struct{}, len(registry))
	for _, line := range full.Lines {
		if len(line.Segments) == 0 {
			return false
		}
		for _, id := range line.TrailingPerformerIDs {
			used[id] = struct{}{}
		}
		for _, segment := range line.Segments {
			if segment.Text == "" || len(segment.PerformerIDs) == 0 {
				return false
			}
			for _, id := range segment.PerformerIDs {
				used[id] = struct{}{}
			}
		}
	}
	return len(used) == len(registry)
}

func validateLyricsSourceV3ReadingEvidence(
	full LyricsSourceFull,
	renditionKey string,
	side LyricsSourceRenditionSide,
	rubyReference *LyricsSourceComponentRef,
) error {
	hasSourceEvidence := false
	for lineIndex, line := range full.Lines {
		for segmentIndex, segment := range line.Segments {
			for spanIndex, span := range segment.Ruby {
				evidence := span.ReadingEvidence
				if span.Reading == "" {
					if evidence != nil {
						return fmt.Errorf("line %d segment %d span %d has evidence without a reading", lineIndex+1, segmentIndex+1, spanIndex+1)
					}
					continue
				}
				if evidence == nil {
					return fmt.Errorf("line %d segment %d span %d lacks evidence", lineIndex+1, segmentIndex+1, spanIndex+1)
				}
				switch evidence.Kind {
				case LyricsSourceReadingEvidenceExplicitSourceKana,
					LyricsSourceReadingEvidenceSourceTransliteration,
					LyricsSourceReadingEvidenceLegacyV2Component:
					hasSourceEvidence = true
					if rubyReference == nil || evidence.FixedIdentityKey == "" ||
						evidence.FixedIdentityKey != rubyReference.RenditionKey ||
						evidence.RenditionKey != renditionKey || evidence.Side != side ||
						evidence.SourceRowOrdinal <= 0 || evidence.SourceSegmentOrdinal <= 0 ||
						evidence.GeneratorVersion != "" {
						return fmt.Errorf("line %d segment %d span %d has invalid source locator", lineIndex+1, segmentIndex+1, spanIndex+1)
					}
				case LyricsSourceReadingEvidenceDeterministicDictionary,
					LyricsSourceReadingEvidenceFixedReviewedToken:
					if evidence.FixedIdentityKey != "" || evidence.RenditionKey != "" || evidence.Side != "" ||
						evidence.SourceRowOrdinal != 0 || evidence.SourceSegmentOrdinal != 0 ||
						!validLyricsSourceLabel(evidence.GeneratorVersion, maxLyricsSourceRubyGeneratorBytes) {
						return fmt.Errorf("line %d segment %d span %d has invalid generated evidence", lineIndex+1, segmentIndex+1, spanIndex+1)
					}
				default:
					return fmt.Errorf("line %d segment %d span %d has unknown evidence kind", lineIndex+1, segmentIndex+1, spanIndex+1)
				}
			}
		}
	}
	if hasSourceEvidence != (rubyReference != nil) {
		return errors.New("source reading evidence and coarse ruby reference disagree")
	}
	return nil
}

func lyricsSourceFullHasSourceReadingEvidence(full LyricsSourceFull) bool {
	for _, line := range full.Lines {
		for _, segment := range line.Segments {
			for _, span := range segment.Ruby {
				if span.ReadingEvidence == nil {
					continue
				}
				switch span.ReadingEvidence.Kind {
				case LyricsSourceReadingEvidenceExplicitSourceKana,
					LyricsSourceReadingEvidenceSourceTransliteration,
					LyricsSourceReadingEvidenceLegacyV2Component:
					return true
				}
			}
		}
	}
	return false
}

func validateLyricsSourceRenditionRelation(rendition LyricsSourceRendition) error {
	relation := rendition.Relation
	if !IsValidLyricsSourceRenditionRelationKind(relation.Kind) {
		return fmt.Errorf("relation has unknown kind %q", relation.Kind)
	}
	switch relation.Kind {
	case LyricsSourceRenditionRelationNone:
		if relation.FullRenditionKey != "" || relation.LineIDs != nil {
			return errors.New("none relation contains projection data")
		}
	case LyricsSourceRenditionRelationExactProjection:
		if rendition.Full == nil || relation.FullRenditionKey == "" || relation.FullRenditionKey != rendition.RenditionKey {
			return errors.New("exact_projection must target the same rendition Full")
		}
		projection := LyricsSourceGameProjection{LineIDs: relation.LineIDs}
		if err := ValidateLyricsSourceGameProjection(projection, *rendition.Full); err != nil {
			return fmt.Errorf("exact_projection: %w", err)
		}
		if rendition.Game != nil &&
			!lyricsSourceGameExactlyMatchesProjection(*rendition.Game, *rendition.Full, projection) {
			return errors.New("exact_projection Game is not an exact Full projection")
		}
	}
	return nil
}

func validateLyricsSourceRenditionReasonShape(rendition LyricsSourceRendition) error {
	hasFull, hasGame := rendition.Full != nil, rendition.Game != nil
	exact := rendition.Relation.Kind == LyricsSourceRenditionRelationExactProjection
	switch rendition.ReasonCode {
	case LyricsSourceVersionReasonTaggedFullAndGame:
		if !hasFull || (!hasGame && !exact) {
			return errors.New("tagged_full_and_game rendition requires Full and a Game component or exact projection")
		}
	case LyricsSourceVersionReasonTaggedGameOnly,
		LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid:
		if hasFull || !hasGame || exact {
			return fmt.Errorf("%s rendition must contain independent Game only", rendition.ReasonCode)
		}
	case LyricsSourceVersionReasonUntaggedUncutIdentity:
		if !hasFull || !exact || len(rendition.Relation.LineIDs) != len(rendition.Full.Lines) {
			return errors.New("untagged_uncut_identity rendition requires an identity projection")
		}
		for index, line := range rendition.Full.Lines {
			if rendition.Relation.LineIDs[index] != line.ID {
				return errors.New("untagged_uncut_identity rendition projection is not identity")
			}
		}
	case LyricsSourceVersionReasonUntaggedGameSubset,
		LyricsSourceVersionReasonUntaggedFullOnly:
		if exact {
			return fmt.Errorf("%s rendition does not allow exact_projection", rendition.ReasonCode)
		}
	}
	return nil
}

func lyricsSourceGameExactlyMatchesProjection(
	game, full LyricsSourceFull,
	projection LyricsSourceGameProjection,
) bool {
	if len(game.Lines) != len(projection.LineIDs) {
		return false
	}
	byID := make(map[string]LyricsSourceFullLine, len(full.Lines))
	for _, line := range full.Lines {
		byID[line.ID] = line
	}
	for index, lineID := range projection.LineIDs {
		line, found := byID[lineID]
		if !found || !lyricsSourceProjectionLineEqual(game.Lines[index], line) {
			return false
		}
	}
	return true
}

func lyricsSourceExactProjectionMappingCount(game, full LyricsSourceFull) int {
	if len(game.Lines) == 0 || len(game.Lines) > len(full.Lines) {
		return 0
	}
	counts := make([]uint8, len(game.Lines)+1)
	counts[0] = 1
	for _, fullLine := range full.Lines {
		for gameIndex := len(game.Lines); gameIndex > 0; gameIndex-- {
			if !lyricsSourceProjectionLineEqual(game.Lines[gameIndex-1], fullLine) {
				continue
			}
			count := int(counts[gameIndex]) + int(counts[gameIndex-1])
			if count > 2 {
				count = 2
			}
			counts[gameIndex] = uint8(count)
		}
	}
	return int(counts[len(game.Lines)])
}

func lyricsSourceProjectionLineEqual(left, right LyricsSourceFullLine) bool {
	// exact_projection is a source-text/order relation only. Owned Game may
	// retain independent stanza, segmentation, performer, and ruby metadata.
	return left.Text == right.Text
}

func LyricsSourceFullHasPerformerSegmentation(full LyricsSourceFull) bool {
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

func lyricsSourceFullHasRuby(full LyricsSourceFull) bool {
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

func lyricsSourceFullHasV3ReadingEvidence(full *LyricsSourceFull) bool {
	if full == nil {
		return false
	}
	for _, line := range full.Lines {
		for _, segment := range line.Segments {
			for _, span := range segment.Ruby {
				if span.ReadingEvidence != nil {
					return true
				}
			}
		}
	}
	return false
}

func lyricsSourceLegacyDocumentHasV3ReadingEvidence(document LyricsSourceDocument) bool {
	if lyricsSourceFullHasV3ReadingEvidence(func() *LyricsSourceFull {
		if !document.hasFull() {
			return nil
		}
		return &document.Full
	}()) || lyricsSourceFullHasV3ReadingEvidence(document.Game) {
		return true
	}
	for _, alternate := range document.AlternateVocals {
		if lyricsSourceFullHasV3ReadingEvidence(alternate.Full) || lyricsSourceFullHasV3ReadingEvidence(alternate.Game) {
			return true
		}
	}
	return false
}

func validateLyricsSourceDocumentV3(document LyricsSourceDocument) error {
	if document.ReasonCode != "" || !reflect.DeepEqual(document.Provenance, LyricsSourceComponentProvenance{}) ||
		!reflect.DeepEqual(document.Full, LyricsSourceFull{}) || document.Game != nil ||
		document.GameProjection != nil || document.AlternateVocals != nil || document.PrivateReview != nil {
		return errors.New("lyrics source v3 contains legacy singular rendition fields")
	}
	if document.FixedIdentities == nil || len(document.FixedIdentities) == 0 ||
		len(document.FixedIdentities) > maxLyricsSourceFixedIdentities {
		return errors.New("lyrics source v3 requires bounded fixed identities")
	}
	identities := make(map[string]LyricsSourceFixedIdentity, len(document.FixedIdentities))
	for index, identity := range document.FixedIdentities {
		if err := ValidateLyricsSourceFixedIdentity(identity); err != nil {
			return fmt.Errorf("lyrics source fixed identity %d: %w", index+1, err)
		}
		if _, duplicate := identities[identity.RenditionKey]; duplicate {
			return fmt.Errorf("lyrics source v3 repeats fixed identity key %q", identity.RenditionKey)
		}
		identities[identity.RenditionKey] = identity
	}
	bindings, err := EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		return err
	}
	contributing := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if _, found := identities[binding.FixedIdentityKey]; !found {
			return fmt.Errorf("lyrics source v3 component %q references unknown fixed identity %q", binding.ComponentKey, binding.FixedIdentityKey)
		}
		contributing[binding.FixedIdentityKey] = struct{}{}
	}
	for _, identity := range document.FixedIdentities {
		if _, contributes := contributing[identity.RenditionKey]; !contributes {
			return fmt.Errorf("lyrics source v3 fixed identity %q contributes no rendition component", identity.RenditionKey)
		}
	}
	return nil
}

func lyricsSourceV2SourcePerformerIDs(document LyricsSourceDocument) []string {
	seen := map[string]struct{}{}
	if document.hasFull() {
		for _, performer := range document.Full.Performers {
			seen[performer.PerformerID] = struct{}{}
		}
	}
	if document.Game != nil {
		for _, performer := range document.Game.Performers {
			seen[performer.PerformerID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func lyricsSourceV2PerformerEvidenceState(
	full LyricsSourceFull,
	sourcePerformerIDs []string,
	privateComplete bool,
) LyricsSourcePerformerEvidenceState {
	if !LyricsSourceFullHasPerformerSegmentation(full) {
		return LyricsSourcePerformerEvidenceNone
	}
	if privateComplete && LyricsSourceFullHasCompletePerformerEvidence(full, sourcePerformerIDs) {
		return LyricsSourcePerformerEvidenceSourceComplete
	}
	return LyricsSourcePerformerEvidenceSourcePartial
}

func populateLyricsSourceLegacyV2ReadingEvidence(
	full *LyricsSourceFull,
	renditionKey string,
	side LyricsSourceRenditionSide,
	rubyReference *LyricsSourceComponentRef,
) {
	if full == nil || rubyReference == nil {
		return
	}
	for lineIndex := range full.Lines {
		for segmentIndex := range full.Lines[lineIndex].Segments {
			for spanIndex := range full.Lines[lineIndex].Segments[segmentIndex].Ruby {
				span := &full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex]
				if span.Reading == "" {
					continue
				}
				span.ReadingEvidence = &LyricsSourceReadingEvidence{
					Kind:             LyricsSourceReadingEvidenceLegacyV2Component,
					FixedIdentityKey: rubyReference.RenditionKey,
					RenditionKey:     renditionKey, Side: side,
					SourceRowOrdinal: lineIndex + 1, SourceSegmentOrdinal: segmentIndex + 1,
				}
			}
		}
	}
}

// UpconvertLyricsSourceDocumentV2 performs the only automatic source upgrade:
// one singular v2 rendition with no auxiliary alternates. It preserves Full,
// Game, relation, reason, private marker, identities, and component ownership;
// cross-kind singular pairs and non-contributing identities fail closed.
func UpconvertLyricsSourceDocumentV2(document LyricsSourceDocument) (LyricsSourceDocument, error) {
	if document.SchemaVersion != LyricsSourceDocumentSchemaVersionV2 {
		return LyricsSourceDocument{}, errors.New("lyrics source up-conversion requires schema v2")
	}
	if err := ValidateLyricsSourceDocument(document); err != nil {
		return LyricsSourceDocument{}, err
	}
	if len(document.AlternateVocals) != 0 {
		return LyricsSourceDocument{}, errors.New("lyrics source v2 with auxiliary alternates is not a one-rendition document")
	}
	hasFull, hasGame := document.hasFull(), document.Game != nil
	kind := ""
	if hasFull {
		kind = document.Full.Version.Kind
	}
	if hasGame {
		if kind != "" && document.Game.Version.Kind != kind {
			return LyricsSourceDocument{}, errors.New("lyrics source v2 Full/Game kinds do not form one rendition")
		}
		kind = document.Game.Version.Kind
	}
	if !IsValidLyricsSourceRenditionKind(LyricsSourceRenditionKind(kind)) {
		return LyricsSourceDocument{}, errors.New("lyrics source v2 has no up-convertible rendition kind")
	}
	paths := make([]LyricsSourceTabPath, 0, len(document.FixedIdentities)+2)
	seenPaths := map[string]struct{}{}
	appendPath := func(label string) {
		if label == "" {
			return
		}
		path := LyricsSourceTabPath{label}
		key := lyricsSourceTabPathKey(path)
		if _, duplicate := seenPaths[key]; duplicate {
			return
		}
		seenPaths[key] = struct{}{}
		paths = append(paths, path)
	}
	if hasFull {
		appendPath(document.Full.Version.Label)
	}
	if hasGame {
		appendPath(document.Game.Version.Label)
	}
	for _, identity := range document.FixedIdentities {
		appendPath(identity.Section)
	}
	sort.Slice(paths, func(left, right int) bool {
		return lyricsSourceTabPathKey(paths[left]) < lyricsSourceTabPathKey(paths[right])
	})
	key := kind
	rendering := LyricsSourceRendition{
		RenditionKey: key, SourceKind: LyricsSourceRenditionKind(kind), SourceTabPaths: paths,
		ReasonCode:            document.ReasonCode,
		SourcePerformerIDs:    lyricsSourceV2SourcePerformerIDs(document),
		FullPerformerEvidence: LyricsSourcePerformerEvidenceNone,
		GamePerformerEvidence: LyricsSourcePerformerEvidenceNone,
		Full: CloneLyricsSourceFull(func() *LyricsSourceFull {
			if !hasFull {
				return nil
			}
			return &document.Full
		}()), Game: CloneLyricsSourceFull(document.Game),
		Relation: LyricsSourceRenditionRelation{Kind: LyricsSourceRenditionRelationNone},
		PrivateReview: func() *LyricsSourcePrivateReview {
			if document.PrivateReview == nil {
				return nil
			}
			copy := *document.PrivateReview
			return &copy
		}(),
	}
	if rendering.Full != nil {
		rendering.FullPerformerEvidence = lyricsSourceV2PerformerEvidenceState(
			*rendering.Full, rendering.SourcePerformerIDs, document.PrivateReview != nil,
		)
	}
	if rendering.Game != nil {
		rendering.GamePerformerEvidence = lyricsSourceV2PerformerEvidenceState(
			*rendering.Game, rendering.SourcePerformerIDs, document.PrivateReview != nil,
		)
	}
	if hasFull {
		ref := document.Provenance.FullText
		rendering.Provenance.FullText = &ref
		if LyricsSourceFullHasPerformerSegmentation(document.Full) && document.Provenance.PerformerSegmentation != nil {
			ref := *document.Provenance.PerformerSegmentation
			rendering.Provenance.FullPerformerSegmentation = &ref
		}
		if lyricsSourceFullHasRuby(document.Full) && document.Provenance.Ruby != nil {
			ref := *document.Provenance.Ruby
			rendering.Provenance.FullRuby = &ref
		}
	}
	if hasGame {
		ref := *document.Provenance.GameText
		rendering.Provenance.GameText = &ref
		if !hasFull {
			if document.Provenance.PerformerSegmentation != nil {
				ref := *document.Provenance.PerformerSegmentation
				rendering.Provenance.GamePerformerSegmentation = &ref
			}
			if document.Provenance.Ruby != nil {
				ref := *document.Provenance.Ruby
				rendering.Provenance.GameRuby = &ref
			}
		} else {
			if LyricsSourceFullHasPerformerSegmentation(*document.Game) {
				ref := *document.Provenance.GameText
				rendering.Provenance.GamePerformerSegmentation = &ref
			}
			if lyricsSourceFullHasRuby(*document.Game) {
				ref := *document.Provenance.GameText
				rendering.Provenance.GameRuby = &ref
			}
		}
	}
	if rendering.Full != nil {
		populateLyricsSourceLegacyV2ReadingEvidence(
			rendering.Full, key, LyricsSourceRenditionSideFull, rendering.Provenance.FullRuby,
		)
	}
	if rendering.Game != nil {
		populateLyricsSourceLegacyV2ReadingEvidence(
			rendering.Game, key, LyricsSourceRenditionSideGame, rendering.Provenance.GameRuby,
		)
	}
	if document.GameProjection != nil {
		rendering.Relation = LyricsSourceRenditionRelation{
			Kind: LyricsSourceRenditionRelationExactProjection, FullRenditionKey: key,
			LineIDs: cloneStringsPreservingNil(document.GameProjection.LineIDs),
		}
		rendering.Provenance.RelationEvidence = *document.Provenance.GameProjection
	} else if document.Provenance.GameText != nil {
		rendering.Provenance.RelationEvidence = *document.Provenance.GameText
	} else {
		rendering.Provenance.RelationEvidence = document.Provenance.VersionEvidence
	}
	rendering.Provenance.VersionEvidence = document.Provenance.VersionEvidence
	upgraded := LyricsSourceDocument{
		SchemaVersion:   LyricsSourceDocumentSchemaVersionV3,
		FixedIdentities: append([]LyricsSourceFixedIdentity{}, document.FixedIdentities...),
		Renditions:      []LyricsSourceRendition{rendering},
	}
	if err := ValidateLyricsSourceDocument(upgraded); err != nil {
		return LyricsSourceDocument{}, fmt.Errorf("up-convert lyrics source v2: %w", err)
	}
	return upgraded, nil
}

// LyricsSourceDocumentV3ToV2Compatibility is the explicit fail-closed
// compatibility predicate. It does not coerce data; callers may attempt a v2
// representation only when this function returns nil.
func LyricsSourceDocumentV3ToV2Compatibility(document LyricsSourceDocument) error {
	if document.SchemaVersion != LyricsSourceDocumentSchemaVersionV3 {
		return errors.New("lyrics source v3-to-v2 compatibility requires schema v3")
	}
	if err := ValidateLyricsSourceDocument(document); err != nil {
		return err
	}
	if len(document.Renditions) != 1 {
		return errors.New("lyrics source v3 peer renditions are not representable as v2")
	}
	rendition := document.Renditions[0]
	if rendition.FullPerformerEvidence == LyricsSourcePerformerEvidenceSourcePartial ||
		rendition.GamePerformerEvidence == LyricsSourcePerformerEvidenceSourcePartial {
		return errors.New("lyrics source v3 partial performer evidence is not representable as v2")
	}
	if rendition.Full != nil && rendition.Game != nil &&
		rendition.Relation.Kind != LyricsSourceRenditionRelationExactProjection {
		return errors.New("lyrics source v3 independent Full/Game text is not representable as v2")
	}
	for _, full := range []*LyricsSourceFull{rendition.Full, rendition.Game} {
		if full == nil {
			continue
		}
		for _, line := range full.Lines {
			for _, segment := range line.Segments {
				for _, span := range segment.Ruby {
					if span.Reading == "" {
						continue
					}
					if span.ReadingEvidence == nil ||
						span.ReadingEvidence.Kind != LyricsSourceReadingEvidenceLegacyV2Component {
						return errors.New("lyrics source v3 reading evidence is not an exact legacy-v2 shape")
					}
				}
			}
		}
	}
	return nil
}
