package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"moesekai/server/internal/jsonstrict"
)

const (
	MaxLyricsSourceDocumentBytes         = 16 << 20
	MaxLyricsSourceJSONDepth             = 64
	maxLyricsSourceFixedIdentities       = 16
	maxLyricsSourceIndexEvidenceRefs     = 64
	maxLyricsSourceTitleBytes            = 2048
	maxLyricsSourceURLBytes              = 4096
	maxLyricsSourceSectionBytes          = 512
	maxLyricsSourceCategoryBytes         = 1024
	maxLyricsSourceReferenceIDBytes      = 256
	maxLyricsSourceRenditionKeyBytes     = 128
	maxLyricsSourcePerformers            = 256
	maxLyricsSourcePerformerIDBytes      = 128
	maxLyricsSourcePerformerNameBytes    = 2048
	maxLyricsSourceVersionLabelBytes     = 2048
	maxLyricsSourceRubyGeneratorBytes    = 64
	maxLyricsSourceLines                 = 1000
	maxLyricsSourceLineBytes             = 8 << 10
	maxLyricsSourceTextBytes             = 1 << 20
	maxLyricsSourceSegmentsPerLine       = 100
	maxLyricsSourceRubySpansPerSegment   = 8 << 10
	maxLyricsSourceRubyTextBytes         = 8 << 10
	maxLyricsSourceRubyReadingBytes      = 16 << 10
	maxLyricsSourceRubyReadingTotalBytes = 1 << 20
)

func isCanonicalLyricsSourceLowerHex(s string, wantLen int) bool {
	if len(s) != wantLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isCanonicalLyricsSourceColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func isCanonicalLyricsSourcePerformerID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			for _, r := range s {
				if r != '_' && r != '-' && !unicode.IsLetter(r) && !unicode.IsNumber(r) {
					return false
				}
			}
			return true
		}
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func isCanonicalLyricsSourceLineID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	c0 := s[0]
	if !((c0 >= 'a' && c0 <= 'z') || (c0 >= 'A' && c0 <= 'Z') || (c0 >= '0' && c0 <= '9')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

func isCanonicalLyricsSourceReferenceID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	c0 := s[0]
	if !((c0 >= 'a' && c0 <= 'z') || (c0 >= 'A' && c0 <= 'Z') || (c0 >= '0' && c0 <= '9')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == ':' || c == '/' || c == '-') {
			return false
		}
	}
	return true
}

func isCanonicalLyricsSourceRenditionKey(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	c0 := s[0]
	if !((c0 >= 'a' && c0 <= 'z') || (c0 >= '0' && c0 <= '9')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func IsValidLyricsSourceProvider(provider LyricsSourceProvider) bool {
	switch provider {
	case LyricsSourceProviderVocaloidFandom, LyricsSourceProviderMoegirl,
		LyricsSourceProviderMoegirlPublicExact, LyricsSourceProviderSekaipedia:
		return true
	default:
		return false
	}
}

func IsValidLyricsSourceVersionReasonCode(reason LyricsSourceVersionReasonCode) bool {
	switch reason {
	case LyricsSourceVersionReasonTaggedFullAndGame,
		LyricsSourceVersionReasonTaggedGameOnly,
		LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid,
		LyricsSourceVersionReasonUntaggedUncutIdentity,
		LyricsSourceVersionReasonUntaggedGameSubset,
		LyricsSourceVersionReasonUntaggedFullOnly,
		LyricsSourceVersionReasonVersionConflict:
		return true
	default:
		return false
	}
}

// IsValidLyricsSourceCandidateVersionReasonCode accepts only reasons that can
// classify one provider candidate. version_conflict is a plural post-fetch
// composition state and must not be attached to an individual artifact.
func IsValidLyricsSourceCandidateVersionReasonCode(reason LyricsSourceVersionReasonCode) bool {
	return IsValidLyricsSourceVersionReasonCode(reason) && reason != LyricsSourceVersionReasonVersionConflict
}

// DecodeLyricsSourceFixedIdentity is a closed source-boundary decoder. Unknown,
// duplicate, trailing, and romanization fields are rejected before semantic
// validation.
func DecodeLyricsSourceFixedIdentity(body []byte) (LyricsSourceFixedIdentity, error) {
	var identity LyricsSourceFixedIdentity
	if err := decodeClosedLyricsSourceJSON(body, &identity); err != nil {
		return LyricsSourceFixedIdentity{}, fmt.Errorf("decode lyrics source fixed identity: %w", err)
	}
	if err := ValidateLyricsSourceFixedIdentity(identity); err != nil {
		return LyricsSourceFixedIdentity{}, err
	}
	return identity, nil
}

// DecodeLyricsSourceDocument rejects romanization at the source boundary. Ruby
// readings remain supported because they are tied to exact source spans rather
// than carrying an independent romanized lyric rendition.
func DecodeLyricsSourceDocument(body []byte) (LyricsSourceDocument, error) {
	var document LyricsSourceDocument
	if err := decodeClosedLyricsSourceJSON(body, &document); err != nil {
		return LyricsSourceDocument{}, fmt.Errorf("decode lyrics source document: %w", err)
	}
	if err := ValidateLyricsSourceDocument(document); err != nil {
		return LyricsSourceDocument{}, err
	}
	return document, nil
}

func ValidateLyricsSourceFixedIdentity(identity LyricsSourceFixedIdentity) error {
	if !IsValidLyricsSourceProvider(identity.Provider) {
		return fmt.Errorf("invalid lyrics source provider %q", identity.Provider)
	}
	if !LyricsSourceProviderAcceptsOrigin(identity.Provider, identity.Origin) {
		return fmt.Errorf("lyrics source provider %q requires one of the origins %q, not %q",
			identity.Provider, LyricsSourceProviderOrigins(identity.Provider), identity.Origin)
	}
	if identity.PageID <= 0 || identity.RevisionID <= 0 || !isCanonicalLyricsSourceLowerHex(identity.SHA1, 40) {
		return errors.New("lyrics source fixed identity has an invalid page, revision, or SHA1")
	}
	if !validLyricsSourceLabel(identity.Title, maxLyricsSourceTitleBytes) {
		return errors.New("lyrics source fixed identity has an invalid title")
	}
	if !validLyricsSourceLabel(identity.Section, maxLyricsSourceSectionBytes) {
		return errors.New("lyrics source fixed identity has an invalid section")
	}
	if len(identity.RenditionKey) > maxLyricsSourceRenditionKeyBytes ||
		!isCanonicalLyricsSourceRenditionKey(identity.RenditionKey) {
		return errors.New("lyrics source fixed identity has an invalid artifact rendition key")
	}
	if identity.CompositionRenditionKey != "" &&
		(len(identity.CompositionRenditionKey) > maxLyricsSourceRenditionKeyBytes ||
			!isCanonicalLyricsSourceRenditionKey(identity.CompositionRenditionKey)) {
		return errors.New("lyrics source fixed identity has an invalid composition rendition key")
	}
	if identity.VersionReason != "" && !IsValidLyricsSourceCandidateVersionReasonCode(identity.VersionReason) {
		return errors.New("lyrics source fixed identity has an invalid provider version reason")
	}
	if err := validateLyricsSourceRevisionURL(identity); err != nil {
		return err
	}
	fetchedAt, validFetchedAt := parseCanonicalLyricsSourceTimestamp(identity.FetchedAt)
	if !validFetchedAt {
		return errors.New("lyrics source fixed identity has an invalid fetchedAt timestamp")
	}
	if identity.Provider == LyricsSourceProviderSekaipedia && identity.RevisionTimestamp == "" {
		return errors.New("sekaipedia fixed identity requires a revisionTimestamp")
	}
	if identity.RevisionTimestamp != "" {
		revisionTimestamp, validRevisionTimestamp := parseCanonicalLyricsSourceTimestamp(identity.RevisionTimestamp)
		if !validRevisionTimestamp {
			return errors.New("lyrics source fixed identity has an invalid revisionTimestamp")
		}
		if revisionTimestamp.After(fetchedAt) {
			return errors.New("lyrics source fixed identity revisionTimestamp is after fetchedAt")
		}
	}
	if identity.Categories == nil || len(identity.Categories) > 256 {
		return errors.New("lyrics source fixed identity has invalid categories")
	}
	seenCategories := make(map[string]struct{}, len(identity.Categories))
	for _, category := range identity.Categories {
		if !validLyricsSourceLabel(category, maxLyricsSourceCategoryBytes) {
			return errors.New("lyrics source fixed identity has an invalid category")
		}
		if _, exists := seenCategories[category]; exists {
			return errors.New("lyrics source fixed identity has duplicate categories")
		}
		seenCategories[category] = struct{}{}
	}
	if identity.IndexEvidenceRefs == nil || len(identity.IndexEvidenceRefs) == 0 ||
		len(identity.IndexEvidenceRefs) > maxLyricsSourceIndexEvidenceRefs {
		return errors.New("lyrics source fixed identity requires bounded index evidence references")
	}
	seenEvidence := make(map[string]struct{}, len(identity.IndexEvidenceRefs))
	for _, reference := range identity.IndexEvidenceRefs {
		if len(reference.EvidenceID) > maxLyricsSourceReferenceIDBytes ||
			!isCanonicalLyricsSourceReferenceID(reference.EvidenceID) ||
			!isCanonicalLyricsSourceLowerHex(reference.SHA256, 64) {
			return errors.New("lyrics source fixed identity has an invalid index evidence reference")
		}
		if _, exists := seenEvidence[reference.EvidenceID]; exists {
			return errors.New("lyrics source fixed identity has duplicate index evidence references")
		}
		seenEvidence[reference.EvidenceID] = struct{}{}
	}
	return nil
}

func ValidateLyricsSourceFull(full LyricsSourceFull) error {
	return validateLyricsSourceFull(full, false)
}

// ValidateLyricsSourceFullWithAuthoritativeVocaloidSegmentation validates a
// standalone VIRTUAL SINGER Full whose performer segmentation has already been
// bound to authoritative complete structured source evidence by the caller.
// Callers without that evidence must use ValidateLyricsSourceFull.
func ValidateLyricsSourceFullWithAuthoritativeVocaloidSegmentation(full LyricsSourceFull) error {
	if full.Version.Kind != "vocaloid" {
		return errors.New("authoritative vocaloid performer segmentation requires a vocaloid Full")
	}
	return validateLyricsSourceFull(full, true)
}

func validateLyricsSourceFull(full LyricsSourceFull, authoritativeVocaloidSegmentation bool) error {
	if err := validateLyricsSourceVersion(full.Version); err != nil {
		return err
	}
	if full.Performers == nil || len(full.Performers) > maxLyricsSourcePerformers {
		return errors.New("lyrics source Full has invalid performer metadata")
	}
	vocaloidOnly := full.Version.Kind == "vocaloid"
	if vocaloidOnly && !authoritativeVocaloidSegmentation && len(full.Performers) != 0 {
		return errors.New("vocaloid-only Full must not contain performer metadata")
	}
	performers := make(map[string]struct{}, len(full.Performers))
	for _, performer := range full.Performers {
		if len(performer.PerformerID) == 0 || len(performer.PerformerID) > maxLyricsSourcePerformerIDBytes ||
			!isCanonicalLyricsSourcePerformerID(performer.PerformerID) ||
			!validLyricsSourceLabel(performer.Name, maxLyricsSourcePerformerNameBytes) ||
			(performer.Color != "" && !isCanonicalLyricsSourceColor(performer.Color)) {
			return errors.New("lyrics source Full has an invalid performer")
		}
		if _, exists := performers[performer.PerformerID]; exists {
			return errors.New("lyrics source Full has duplicate performers")
		}
		performers[performer.PerformerID] = struct{}{}
	}
	if full.Lines == nil || len(full.Lines) == 0 || len(full.Lines) > maxLyricsSourceLines {
		return errors.New("lyrics source Full requires bounded authoritative lines")
	}
	seenLineIDs := make(map[string]struct{}, len(full.Lines))
	totalTextBytes := 0
	totalRubyReadingBytes := 0
	hasRubyReading := false
	for lineIndex, line := range full.Lines {
		if !isCanonicalLyricsSourceLineID(line.ID) {
			return fmt.Errorf("lyrics source Full line %d has an invalid ID", lineIndex+1)
		}
		if _, exists := seenLineIDs[line.ID]; exists {
			return fmt.Errorf("lyrics source Full line %d has a duplicate ID", lineIndex+1)
		}
		seenLineIDs[line.ID] = struct{}{}
		if !validLyricsSourceLineText(line.Text, maxLyricsSourceLineBytes) || line.Segments == nil ||
			len(line.Segments) == 0 || len(line.Segments) > maxLyricsSourceSegmentsPerLine ||
			line.TrailingPerformerIDs == nil {
			return fmt.Errorf("lyrics source Full line %d is empty or incomplete", lineIndex+1)
		}
		if vocaloidOnly {
			if hasLyricsSourceInlinePerformerAnnotation(line.Text) {
				return fmt.Errorf("vocaloid-only Full line %d must not contain performer or color annotations", lineIndex+1)
			}
			if !authoritativeVocaloidSegmentation {
				if len(line.Segments) != 1 || line.Segments[0].Text != line.Text {
					return fmt.Errorf("vocaloid-only Full line %d must have one segment covering the complete text", lineIndex+1)
				}
				if len(line.Segments[0].PerformerIDs) != 0 || len(line.TrailingPerformerIDs) != 0 {
					return fmt.Errorf("vocaloid-only Full line %d must not contain performer references", lineIndex+1)
				}
			}
		}
		totalTextBytes += len(line.Text)
		if totalTextBytes > maxLyricsSourceTextBytes {
			return errors.New("lyrics source Full text exceeds the safe limit")
		}
		if err := validateLyricsSourcePerformerRefs(line.TrailingPerformerIDs, performers); err != nil {
			return fmt.Errorf("lyrics source Full line %d trailing performers: %w", lineIndex+1, err)
		}
		lineOffset := 0
		for segmentIndex, segment := range line.Segments {
			if !validLyricsSourceLineText(segment.Text, maxLyricsSourceLineBytes) || segment.PerformerIDs == nil ||
				segment.Ruby == nil || len(segment.Ruby) == 0 ||
				len(segment.Ruby) > maxLyricsSourceRubySpansPerSegment {
				return fmt.Errorf("lyrics source Full line %d segment %d is empty or incomplete", lineIndex+1, segmentIndex+1)
			}
			if err := validateLyricsSourcePerformerRefs(segment.PerformerIDs, performers); err != nil {
				return fmt.Errorf("lyrics source Full line %d segment %d performers: %w", lineIndex+1, segmentIndex+1, err)
			}
			rubyOffset := 0
			for spanIndex, span := range segment.Ruby {
				if !validLyricsSourceSpanText(span.Text, maxLyricsSourceRubyTextBytes) ||
					len(span.Reading) > maxLyricsSourceRubyReadingBytes || !validLyricsSourceRubyReading(span.Reading) {
					return fmt.Errorf("lyrics source Full line %d segment %d ruby span %d is invalid", lineIndex+1, segmentIndex+1, spanIndex+1)
				}
				if totalRubyReadingBytes > maxLyricsSourceRubyReadingTotalBytes-len(span.Reading) {
					return errors.New("lyrics source Full ruby readings exceed the safe limit")
				}
				totalRubyReadingBytes += len(span.Reading)
				hasRubyReading = hasRubyReading || span.Reading != ""
				if !strings.HasPrefix(segment.Text[rubyOffset:], span.Text) {
					return fmt.Errorf("lyrics source Full line %d segment %d ruby spans do not concatenate to segment text", lineIndex+1, segmentIndex+1)
				}
				rubyOffset += len(span.Text)
			}
			if rubyOffset != len(segment.Text) {
				return fmt.Errorf("lyrics source Full line %d segment %d ruby spans do not concatenate to segment text", lineIndex+1, segmentIndex+1)
			}
			if !strings.HasPrefix(line.Text[lineOffset:], segment.Text) {
				return fmt.Errorf("lyrics source Full line %d segments do not concatenate to authoritative text", lineIndex+1)
			}
			lineOffset += len(segment.Text)
		}
		if lineOffset != len(line.Text) {
			return fmt.Errorf("lyrics source Full line %d segments do not concatenate to authoritative text", lineIndex+1)
		}
	}
	if full.RubyGeneratorVersion != "" && !validLyricsSourceLabel(full.RubyGeneratorVersion, maxLyricsSourceRubyGeneratorBytes) {
		return errors.New("lyrics source Full has an invalid ruby generator version")
	}
	if hasRubyReading && full.RubyGeneratorVersion == "" {
		return errors.New("lyrics source Full ruby readings require a generator version")
	}
	return nil
}

func ValidateLyricsSourceGameProjection(projection LyricsSourceGameProjection, full LyricsSourceFull) error {
	if projection.LineIDs == nil || len(projection.LineIDs) == 0 || len(projection.LineIDs) > len(full.Lines) {
		return errors.New("lyrics source GameProjection requires a bounded Full-line subset")
	}
	positions := make(map[string]int, len(full.Lines))
	for index, line := range full.Lines {
		positions[line.ID] = index
	}
	seen := make(map[string]struct{}, len(projection.LineIDs))
	lastPosition := -1
	for _, lineID := range projection.LineIDs {
		position, exists := positions[lineID]
		if !exists {
			return fmt.Errorf("lyrics source GameProjection references unknown Full line %q", lineID)
		}
		if _, duplicate := seen[lineID]; duplicate {
			return fmt.Errorf("lyrics source GameProjection repeats Full line %q", lineID)
		}
		if position <= lastPosition {
			return errors.New("lyrics source GameProjection line IDs must preserve Full order")
		}
		seen[lineID] = struct{}{}
		lastPosition = position
	}
	return nil
}

// ValidateLyricsSourceAlternateVocalPayload validates the bounded auxiliary
// rendition itself. Fixed-identity membership is validated by the owning
// document contract because SongResult intentionally carries no identity graph.
func ValidateLyricsSourceAlternateVocalPayload(alternate LyricsSourceAlternateVocal) error {
	return validateLyricsSourceAlternateVocalPayload(0, alternate)
}

func validateLyricsSourceAlternateVocalPayload(index int, alternate LyricsSourceAlternateVocal) error {
	if !validLyricsSourceLabel(alternate.TabLabel, maxLyricsSourceVersionLabelBytes) ||
		!validLyricsSourceLabel(alternate.SingerLabel, maxLyricsSourceVersionLabelBytes) ||
		alternate.SingerIDs == nil || len(alternate.SingerIDs) == 0 || len(alternate.SingerIDs) > 26 ||
		(alternate.Full == nil && alternate.Game == nil) {
		return fmt.Errorf("lyrics source alternate vocal %d has invalid identity metadata", index+1)
	}
	seenSingerIDs := make(map[string]struct{}, len(alternate.SingerIDs))
	for _, singerID := range alternate.SingerIDs {
		if len(singerID) == 0 || len(singerID) > maxLyricsSourcePerformerIDBytes ||
			!isCanonicalLyricsSourcePerformerID(singerID) {
			return fmt.Errorf("lyrics source alternate vocal %d has an invalid singer ID", index+1)
		}
		if _, duplicate := seenSingerIDs[singerID]; duplicate {
			return fmt.Errorf("lyrics source alternate vocal %d repeats a singer ID", index+1)
		}
		seenSingerIDs[singerID] = struct{}{}
	}
	for component, full := range map[string]*LyricsSourceFull{"full": alternate.Full, "game": alternate.Game} {
		if full == nil {
			continue
		}
		if full.Version.Kind != "alternate" {
			return fmt.Errorf("lyrics source alternate vocal %d %s has invalid version kind %q", index+1, component, full.Version.Kind)
		}
		if err := ValidateLyricsSourceFull(*full); err != nil {
			return fmt.Errorf("lyrics source alternate vocal %d %s: %w", index+1, component, err)
		}
	}
	if alternate.GameProjection != nil {
		if alternate.Full == nil || alternate.Game == nil {
			return fmt.Errorf("lyrics source alternate vocal %d GameProjection requires Full and Game text", index+1)
		}
		if err := ValidateLyricsSourceGameProjection(*alternate.GameProjection, *alternate.Full); err != nil {
			return fmt.Errorf("lyrics source alternate vocal %d GameProjection: %w", index+1, err)
		}
	}
	return nil
}

func validateLyricsSourceAlternateVocal(
	index int,
	alternate LyricsSourceAlternateVocal,
	identities map[string]LyricsSourceFixedIdentity,
) error {
	if err := validateLyricsSourceAlternateVocalPayload(index, alternate); err != nil {
		return err
	}
	if err := validateConditionalLyricsSourceComponentRef(
		fmt.Sprintf("alternateVocal[%d].fullText", index+1), alternate.Provenance.FullText,
		alternate.Full != nil, identities,
	); err != nil {
		return err
	}
	if err := validateConditionalLyricsSourceComponentRef(
		fmt.Sprintf("alternateVocal[%d].gameText", index+1), alternate.Provenance.GameText,
		alternate.Game != nil, identities,
	); err != nil {
		return err
	}
	if err := validateConditionalLyricsSourceComponentRef(
		fmt.Sprintf("alternateVocal[%d].gameProjection", index+1), alternate.Provenance.GameProjection,
		alternate.GameProjection != nil, identities,
	); err != nil {
		return err
	}
	if err := validateLyricsSourceComponentRef(
		fmt.Sprintf("alternateVocal[%d].versionEvidence", index+1), alternate.Provenance.VersionEvidence, identities,
	); err != nil {
		return err
	}
	return nil
}

func ValidateLyricsSourceDocument(document LyricsSourceDocument) error {
	if document.SchemaVersion == LyricsSourceDocumentSchemaVersionV3 {
		return validateLyricsSourceDocumentV3(document)
	}
	if document.SchemaVersion != LyricsSourceDocumentSchemaVersionV1 &&
		document.SchemaVersion != LyricsSourceDocumentSchemaVersionV2 {
		return fmt.Errorf("unsupported lyrics source document schema version %d", document.SchemaVersion)
	}
	if document.Renditions != nil {
		return errors.New("lyrics source v1/v2 document contains v3 renditions")
	}
	if lyricsSourceLegacyDocumentHasV3ReadingEvidence(document) {
		return errors.New("lyrics source v1/v2 document contains v3 reading evidence")
	}
	if !IsValidLyricsSourceVersionReasonCode(document.ReasonCode) {
		return fmt.Errorf("invalid lyrics source version reason code %q", document.ReasonCode)
	}
	if document.ReasonCode == LyricsSourceVersionReasonVersionConflict {
		return errors.New("lyrics source version_conflict is a fail-closed classification, not an acceptable document")
	}
	if document.FixedIdentities == nil || len(document.FixedIdentities) == 0 ||
		len(document.FixedIdentities) > maxLyricsSourceFixedIdentities {
		return errors.New("lyrics source document requires bounded fixed identities")
	}
	identities := make(map[string]LyricsSourceFixedIdentity, len(document.FixedIdentities))
	for index, identity := range document.FixedIdentities {
		if err := ValidateLyricsSourceFixedIdentity(identity); err != nil {
			return fmt.Errorf("lyrics source fixed identity %d: %w", index+1, err)
		}
		if _, exists := identities[identity.RenditionKey]; exists {
			return fmt.Errorf("lyrics source document repeats rendition key %q", identity.RenditionKey)
		}
		identities[identity.RenditionKey] = identity
	}
	hasFull := len(document.Full.Lines) > 0
	hasGame := document.Game != nil
	if !hasFull && !hasGame {
		return errors.New("lyrics source document requires an authoritative Full or Game rendition")
	}
	hasAuthoritativePerformerSegmentationEvidence := document.PrivateReview != nil
	if hasAuthoritativePerformerSegmentationEvidence &&
		document.PrivateReview.PerformerSegmentationEvidence != LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured {
		return errors.New("lyrics source privateReview has an invalid performerSegmentationEvidence marker")
	}
	if hasFull {
		authoritativeVocaloidSegmentation := hasAuthoritativePerformerSegmentationEvidence && document.Full.Version.Kind == "vocaloid"
		if authoritativeVocaloidSegmentation {
			if err := ValidateLyricsSourceFullWithAuthoritativeVocaloidSegmentation(document.Full); err != nil {
				return err
			}
		} else if err := ValidateLyricsSourceFull(document.Full); err != nil {
			return err
		}
	}
	if hasGame {
		authoritativeVocaloidSegmentation := hasAuthoritativePerformerSegmentationEvidence && document.Game.Version.Kind == "vocaloid"
		if authoritativeVocaloidSegmentation {
			if err := ValidateLyricsSourceFullWithAuthoritativeVocaloidSegmentation(*document.Game); err != nil {
				return fmt.Errorf("lyrics source Game: %w", err)
			}
		} else if err := ValidateLyricsSourceFull(*document.Game); err != nil {
			return fmt.Errorf("lyrics source Game: %w", err)
		}
	}
	if err := validateConditionalLyricsSourceComponentRef(
		"fullText", sourceComponentRefPtr(document.Provenance.FullText), hasFull, identities,
	); err != nil {
		return err
	}
	if err := validateConditionalLyricsSourceComponentRef(
		"gameText", document.Provenance.GameText, hasGame, identities,
	); err != nil {
		return err
	}
	if err := validateLyricsSourceComponentRef("versionEvidence", document.Provenance.VersionEvidence, identities); err != nil {
		return err
	}
	hasPerformerSegmentation := false
	hasRuby := false
	componentFull := document.Full
	if !hasFull && hasGame {
		componentFull = *document.Game
	}
	if hasFull || hasGame {
		hasPerformerSegmentation = len(componentFull.Performers) > 0
		for _, line := range componentFull.Lines {
			hasPerformerSegmentation = hasPerformerSegmentation ||
				len(line.TrailingPerformerIDs) > 0 ||
				len(line.Segments) != 1 ||
				(len(line.Segments) == 1 && line.Segments[0].Text != line.Text)
			for _, segment := range line.Segments {
				hasPerformerSegmentation = hasPerformerSegmentation || len(segment.PerformerIDs) > 0
				for _, span := range segment.Ruby {
					hasRuby = hasRuby || span.Reading != ""
				}
			}
		}
	}
	if hasAuthoritativePerformerSegmentationEvidence && !hasPerformerSegmentation {
		return errors.New("lyrics source privateReview performer segmentation marker is present without component data")
	}
	if err := validateConditionalLyricsSourceComponentRef(
		"performerSegmentation", document.Provenance.PerformerSegmentation, hasPerformerSegmentation, identities,
	); err != nil {
		return err
	}
	if hasFull && document.Full.Version.Kind == "vocaloid" && hasAuthoritativePerformerSegmentationEvidence {
		fullIdentity := identities[document.Provenance.FullText.RenditionKey]
		segmentationIdentity := identities[document.Provenance.PerformerSegmentation.RenditionKey]
		if LyricsSourceCompositionRenditionKey(fullIdentity) != LyricsSourceCompositionRenditionKey(segmentationIdentity) {
			return errors.New("authoritative vocaloid performerSegmentation provenance must resolve to the Full logical rendition")
		}
	}
	if err := validateConditionalLyricsSourceComponentRef("ruby", document.Provenance.Ruby, hasRuby, identities); err != nil {
		return err
	}
	hasGameProjection := document.GameProjection != nil
	if err := validateConditionalLyricsSourceComponentRef(
		"gameProjection", document.Provenance.GameProjection, hasGameProjection, identities,
	); err != nil {
		return err
	}
	if hasGameProjection {
		if !hasFull {
			return errors.New("lyrics source GameProjection requires a Full rendition")
		}
		if err := ValidateLyricsSourceGameProjection(*document.GameProjection, document.Full); err != nil {
			return err
		}
		if hasGame && !lyricsSourceGameMatchesProjection(*document.Game, document.Full, *document.GameProjection) {
			return errors.New("lyrics source Game does not match its declared Full projection")
		}
	}
	if len(document.AlternateVocals) > maxLyricsSourceFixedIdentities {
		return errors.New("lyrics source document has too many alternate vocal entries")
	}
	for index, alternate := range document.AlternateVocals {
		if err := validateLyricsSourceAlternateVocal(index, alternate, identities); err != nil {
			return err
		}
	}
	switch document.ReasonCode {
	case LyricsSourceVersionReasonTaggedFullAndGame:
		if !hasFull {
			return errors.New("tagged_full_and_game requires a Full rendition")
		}
		// Source-v1 documents used a projection without carrying an independent
		// Game artifact. Source-v2 may carry both, but the projection remains
		// optional when the exact Game text is preserved independently.
		if document.SchemaVersion == LyricsSourceDocumentSchemaVersionV1 && document.GameProjection == nil {
			return errors.New("legacy tagged_full_and_game requires a GameProjection")
		}
	case LyricsSourceVersionReasonTaggedGameOnly,
		LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid:
		if hasFull || !hasGame || document.GameProjection != nil {
			return fmt.Errorf("%s must contain Game without Full or projection", document.ReasonCode)
		}
	case LyricsSourceVersionReasonUntaggedUncutIdentity:
		if !hasFull || document.GameProjection == nil {
			return errors.New("untagged_uncut_identity requires an identity Full projection")
		}
		if len(document.GameProjection.LineIDs) != len(document.Full.Lines) {
			return errors.New("untagged_uncut_identity must project every Full line")
		}
		for index, line := range document.Full.Lines {
			if document.GameProjection.LineIDs[index] != line.ID {
				return errors.New("untagged_uncut_identity must be an ordered identity projection")
			}
		}
	case LyricsSourceVersionReasonUntaggedGameSubset,
		LyricsSourceVersionReasonUntaggedFullOnly:
		if document.GameProjection != nil {
			return fmt.Errorf("lyrics source version reason %q does not allow a GameProjection", document.ReasonCode)
		}
	}
	return nil
}

func sourceComponentRefPtr(reference LyricsSourceComponentRef) *LyricsSourceComponentRef {
	if reference.RenditionKey == "" {
		return nil
	}
	copy := reference
	return &copy
}

func lyricsSourceGameMatchesProjection(game, full LyricsSourceFull, projection LyricsSourceGameProjection) bool {
	if len(game.Lines) != len(projection.LineIDs) {
		return false
	}
	positions := make(map[string]int, len(full.Lines))
	for index, line := range full.Lines {
		positions[line.ID] = index
	}
	for index, lineID := range projection.LineIDs {
		position, ok := positions[lineID]
		if !ok || game.Lines[index].Text != full.Lines[position].Text {
			return false
		}
	}
	return true
}

func validateLyricsSourceVersion(version LyricsSourceVersion) error {
	switch version.Kind {
	case "original", "sekai", "vocaloid", "alternate":
	default:
		return fmt.Errorf("lyrics source Full has invalid version kind %q", version.Kind)
	}
	if !validLyricsSourceLabel(version.Label, maxLyricsSourceVersionLabelBytes) {
		return errors.New("lyrics source Full has an invalid version label")
	}
	return nil
}

func validateLyricsSourcePerformerRefs(references []string, performers map[string]struct{}) error {
	if len(references) == 0 {
		return nil
	}
	if len(references) > maxLyricsSourcePerformers {
		return errors.New("too many performer IDs")
	}
	if len(references) <= 8 {
		for i, performerID := range references {
			if len(performerID) == 0 || len(performerID) > maxLyricsSourcePerformerIDBytes ||
				!isCanonicalLyricsSourcePerformerID(performerID) {
				return errors.New("invalid performer ID")
			}
			for j := 0; j < i; j++ {
				if references[j] == performerID {
					return errors.New("duplicate performer ID")
				}
			}
			if _, exists := performers[performerID]; !exists {
				return errors.New("unknown performer ID")
			}
		}
		return nil
	}
	seen := make(map[string]struct{}, len(references))
	for _, performerID := range references {
		if len(performerID) == 0 || len(performerID) > maxLyricsSourcePerformerIDBytes ||
			!isCanonicalLyricsSourcePerformerID(performerID) {
			return errors.New("invalid performer ID")
		}
		if _, exists := seen[performerID]; exists {
			return errors.New("duplicate performer ID")
		}
		seen[performerID] = struct{}{}
		if _, exists := performers[performerID]; !exists {
			return errors.New("unknown performer ID")
		}
	}
	return nil
}

func validateLyricsSourceComponentRef(
	component string,
	reference LyricsSourceComponentRef,
	identities map[string]LyricsSourceFixedIdentity,
) error {
	if _, exists := identities[reference.RenditionKey]; !exists {
		return fmt.Errorf("lyrics source provenance %s references unknown rendition key %q", component, reference.RenditionKey)
	}
	return nil
}

func validateConditionalLyricsSourceComponentRef(
	component string,
	reference *LyricsSourceComponentRef,
	required bool,
	identities map[string]LyricsSourceFixedIdentity,
) error {
	if required && reference == nil {
		return fmt.Errorf("lyrics source provenance %s is required", component)
	}
	if !required && reference != nil {
		return fmt.Errorf("lyrics source provenance %s is present without component data", component)
	}
	if reference == nil {
		return nil
	}
	return validateLyricsSourceComponentRef(component, *reference, identities)
}

func validateLyricsSourceRevisionURL(identity LyricsSourceFixedIdentity) error {
	if identity.CanonicalURL == "" || len(identity.CanonicalURL) > maxLyricsSourceURLBytes ||
		identity.CanonicalURL != strings.TrimSpace(identity.CanonicalURL) {
		return errors.New("lyrics source fixed identity has an invalid canonical URL")
	}
	parsed, err := url.Parse(identity.CanonicalURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" ||
		parsed.Host == "" || parsed.Path == "" || parsed.ForceQuery || parsed.Scheme+"://"+parsed.Host != identity.Origin {
		return errors.New("lyrics source fixed identity has an invalid canonical URL")
	}
	if identity.Provider == LyricsSourceProviderMoegirlPublicExact {
		if parsed.RawQuery != "" || parsed.EscapedPath() == "/" || !strings.HasPrefix(parsed.EscapedPath(), "/") ||
			parsed.String() != identity.CanonicalURL {
			return errors.New("moegirl_public_exact fixed identity requires the exact canonical public page URL")
		}
		return nil
	}
	query := parsed.Query()
	if len(query["oldid"]) != 1 || query.Get("oldid") != strconv.Itoa(identity.RevisionID) ||
		parsed.RawQuery != query.Encode() {
		return errors.New("lyrics source fixed identity has a noncanonical revision query")
	}
	switch identity.Provider {
	case LyricsSourceProviderVocaloidFandom:
		if !strings.HasPrefix(parsed.EscapedPath(), "/wiki/") || parsed.EscapedPath() == "/wiki/" || len(query) != 1 {
			return errors.New("vocaloid_fandom fixed identity requires a canonical /wiki revision URL")
		}
	case LyricsSourceProviderSekaipedia:
		if !strings.HasPrefix(parsed.EscapedPath(), "/wiki/") || parsed.EscapedPath() == "/wiki/" || len(query) != 1 {
			return errors.New("sekaipedia fixed identity requires a canonical /wiki revision URL")
		}
	case LyricsSourceProviderMoegirl:
		switch {
		case strings.HasPrefix(parsed.EscapedPath(), "/wiki/") && parsed.EscapedPath() != "/wiki/":
			if len(query) != 1 {
				return errors.New("moegirl /wiki fixed identity has an invalid query")
			}
		case parsed.EscapedPath() == "/index.php":
			if len(query) != 2 || len(query["title"]) != 1 || strings.TrimSpace(query.Get("title")) == "" {
				return errors.New("moegirl index fixed identity requires one title and one oldid")
			}
		default:
			return errors.New("moegirl fixed identity requires a canonical /wiki or /index.php revision URL")
		}
	}
	return nil
}

func parseCanonicalLyricsSourceTimestamp(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || value == "" || !strings.HasSuffix(value, "Z") || parsed.Unix() <= 0 ||
		parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, false
	}
	return parsed, true
}

func validLyricsSourceLabel(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return false
		}
	}
	return true
}

func validLyricsSourceLineText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && strings.TrimSpace(value) != "" && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\r\n\x00")
}

func validLyricsSourceSpanText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}

func hasLyricsSourceInlinePerformerAnnotation(value string) bool {
	if idx := strings.IndexByte(value, '@'); idx >= 0 {
		for i := idx; i+1 < len(value); i++ {
			if value[i] == '@' && value[i+1] >= '0' && value[i+1] <= '9' {
				return true
			}
		}
	}
	if idx := strings.IndexByte(value, '#'); idx >= 0 {
		for i := idx; i+6 < len(value); i++ {
			if value[i] != '#' {
				continue
			}
			allHex := true
			for j := i + 1; j <= i+6; j++ {
				c := value[j]
				if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
					allHex = false
					break
				}
			}
			if allHex {
				return true
			}
		}
	}
	return false
}

func validLyricsSourceRubyReading(value string) bool {
	if value == "" {
		return true
	}
	if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	hasKana := false
	for _, current := range value {
		switch {
		case unicode.In(current, unicode.Hiragana, unicode.Katakana):
			hasKana = true
		case current == 'ー' || current == '・':
			if !hasKana {
				return false
			}
		case unicode.Is(unicode.Mn, current) || unicode.Is(unicode.Mc, current):
			if !hasKana {
				return false
			}
		default:
			return false
		}
	}
	return hasKana
}

var (
	romanizationForbiddenPatterns = [][]byte{
		[]byte("romaji"),
		[]byte("romanization"),
		[]byte("romanisation"),
		[]byte("romanized"),
		[]byte("romanised"),
	}
)

func checkLyricsSourceJSONKey(key []byte) error {
	if isLyricsSourceRomanizationKeyBytes(key) {
		return fmt.Errorf("romanization field %q is forbidden at the lyrics source boundary", string(key))
	}
	return nil
}

func isLyricsSourceRomanizationKeyBytes(key []byte) bool {
	hasR := false
	hasNonASCII := false
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 0x80 {
			hasNonASCII = true
			break
		}
		if c == 'r' || c == 'R' {
			hasR = true
		}
	}
	if !hasNonASCII {
		if !hasR {
			return false
		}
		if len(key) <= 64 {
			var norm [64]byte
			n := 0
			for i := 0; i < len(key); i++ {
				c := key[i]
				switch {
				case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
					norm[n] = c
					n++
				case c >= 'A' && c <= 'Z':
					norm[n] = c + ('a' - 'A')
					n++
				}
			}
			slice := norm[:n]
			for _, pat := range romanizationForbiddenPatterns {
				if bytes.Contains(slice, pat) {
					return true
				}
			}
			return false
		}
	}
	return isLyricsSourceRomanizationField(string(key))
}

func decodeClosedLyricsSourceJSON(body []byte, target any) error {
	if target == nil || len(body) == 0 {
		return errors.New("JSON body and target are required")
	}
	if len(body) > MaxLyricsSourceDocumentBytes {
		return fmt.Errorf("lyrics source JSON exceeds %d bytes", MaxLyricsSourceDocumentBytes)
	}
	if err := jsonstrict.Scan(body, jsonstrict.Options{
		MaxDepth:             MaxLyricsSourceJSONDepth,
		ForbidLoneSurrogates: true,
		RequireValidUTF8:     true,
		CheckKey:             checkLyricsSourceJSONKey,
	}); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON value")
	}
	return nil
}

func isLyricsSourceRomanizationField(field string) bool {
	var normalized strings.Builder
	for _, current := range strings.ToLower(field) {
		if unicode.IsLetter(current) || unicode.IsNumber(current) {
			normalized.WriteRune(current)
		}
	}
	value := normalized.String()
	for _, forbidden := range []string{"romaji", "romanization", "romanisation", "romanized", "romanised"} {
		if strings.Contains(value, forbidden) {
			return true
		}
	}
	return false
}
