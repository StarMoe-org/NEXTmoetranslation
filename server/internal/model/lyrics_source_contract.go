package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

const (
	LyricsSourceDocumentSchemaVersionV1 = 1
	LyricsSourceDocumentSchemaVersionV2 = 2
	LyricsSourceDocumentSchemaVersionV3 = 3

	// LyricsSourceDocumentSchemaVersion remains pinned to v2 until the later
	// staging/store/public propagation phase. Phase-1 callers opt into v3
	// explicitly, which keeps every existing v2 producer byte-stable.
	LyricsSourceDocumentSchemaVersion = LyricsSourceDocumentSchemaVersionV2
)

type LyricsSourceProvider string

const (
	LyricsSourceProviderVocaloidFandom     LyricsSourceProvider = "vocaloid_fandom"
	LyricsSourceProviderMoegirl            LyricsSourceProvider = "moegirl"
	LyricsSourceProviderMoegirlPublicExact LyricsSourceProvider = "moegirl_public_exact"
	LyricsSourceProviderSekaipedia         LyricsSourceProvider = "sekaipedia"

	LyricsSourceOriginVocaloidFandom     = "https://vocaloid.fandom.com"
	LyricsSourceOriginProjectSekaiFandom = "https://projectsekai.fandom.com"
	LyricsSourceOriginMoegirl            = "https://moegirl.icu"
	LyricsSourceOriginMoegirlPublicExact = "https://zh.moegirl.org.cn"
	LyricsSourceOriginSekaipedia         = "https://www.sekaipedia.org"
)

// LyricsSourceProviderOrigins lists the origins whose revision URLs a provider
// accepts, primary origin first. The Project SEKAI Fandom wiki shares the
// vocaloid_fandom provider and license with the VOCALOID Fandom wiki.
func LyricsSourceProviderOrigins(provider LyricsSourceProvider) []string {
	switch provider {
	case LyricsSourceProviderVocaloidFandom:
		return []string{LyricsSourceOriginVocaloidFandom, LyricsSourceOriginProjectSekaiFandom}
	case LyricsSourceProviderMoegirl:
		return []string{LyricsSourceOriginMoegirl}
	case LyricsSourceProviderMoegirlPublicExact:
		return []string{LyricsSourceOriginMoegirlPublicExact}
	case LyricsSourceProviderSekaipedia:
		return []string{LyricsSourceOriginSekaipedia}
	default:
		return nil
	}
}

// LyricsSourceProviderAcceptsOrigin reports whether origin is one of the
// provider's accepted revision origins.
func LyricsSourceProviderAcceptsOrigin(provider LyricsSourceProvider, origin string) bool {
	for _, accepted := range LyricsSourceProviderOrigins(provider) {
		if origin == accepted {
			return true
		}
	}
	return false
}

// LyricsSourceIndexEvidenceRef points at immutable evidence used to select a
// provider page before its revision is fetched. EvidenceID is an opaque stable
// key owned by the indexing boundary; SHA256 fixes the referenced bytes.
type LyricsSourceIndexEvidenceRef struct {
	EvidenceID string `json:"evidenceId"`
	SHA256     string `json:"sha256"`
}

// LyricsSourceFixedIdentity identifies one immutable provider artifact.
// RenditionKey is the unique artifact/component key used by provenance refs.
// CompositionRenditionKey is the provider-independent logical rendition used
// during composition; it may be shared by multiple providers. VersionReason is
// the provider candidate's classification and is intentionally independent of
// LyricsSourceDocument.ReasonCode, which records the final composition result.
// Empty composition/reason fields retain the pre-split v1 interpretation for
// previously produced private documents.
type LyricsSourceFixedIdentity struct {
	Provider                LyricsSourceProvider           `json:"provider"`
	Origin                  string                         `json:"origin"`
	PageID                  int                            `json:"pageId"`
	RevisionID              int                            `json:"revisionId"`
	SHA1                    string                         `json:"sha1"`
	Title                   string                         `json:"title"`
	CanonicalURL            string                         `json:"canonicalUrl"`
	RevisionTimestamp       string                         `json:"revisionTimestamp,omitempty"`
	FetchedAt               string                         `json:"fetchedAt"`
	Categories              []string                       `json:"categories"`
	Section                 string                         `json:"section"`
	RenditionKey            string                         `json:"renditionKey"`
	CompositionRenditionKey string                         `json:"compositionRenditionKey,omitempty"`
	VersionReason           LyricsSourceVersionReasonCode  `json:"versionReason,omitempty"`
	IndexEvidenceRefs       []LyricsSourceIndexEvidenceRef `json:"indexEvidenceRefs"`
}

func LyricsSourceCompositionRenditionKey(identity LyricsSourceFixedIdentity) string {
	if identity.CompositionRenditionKey != "" {
		return identity.CompositionRenditionKey
	}
	return identity.RenditionKey
}

// LyricsSourceComponentRef resolves through LyricsSourceDocument.FixedIdentities.
// This keeps component provenance explicit without duplicating fixed identity
// records for text, segmentation, projection, ruby, and version evidence.
type LyricsSourceComponentRef struct {
	RenditionKey string `json:"renditionKey"`
}

type LyricsSourceComponentProvenance struct {
	// FullText is retained as a value for source-v1 Go compatibility. In source-v2
	// JSON it is omitted when the primary Full rendition is absent.
	FullText              LyricsSourceComponentRef  `json:"fullText"`
	GameText              *LyricsSourceComponentRef `json:"gameText,omitempty"`
	PerformerSegmentation *LyricsSourceComponentRef `json:"performerSegmentation,omitempty"`
	GameProjection        *LyricsSourceComponentRef `json:"gameProjection,omitempty"`
	Ruby                  *LyricsSourceComponentRef `json:"ruby,omitempty"`
	VersionEvidence       LyricsSourceComponentRef  `json:"versionEvidence"`
}

func (provenance LyricsSourceComponentProvenance) MarshalJSON() ([]byte, error) {
	type wire struct {
		FullText              *LyricsSourceComponentRef `json:"fullText,omitempty"`
		GameText              *LyricsSourceComponentRef `json:"gameText,omitempty"`
		PerformerSegmentation *LyricsSourceComponentRef `json:"performerSegmentation,omitempty"`
		GameProjection        *LyricsSourceComponentRef `json:"gameProjection,omitempty"`
		Ruby                  *LyricsSourceComponentRef `json:"ruby,omitempty"`
		VersionEvidence       LyricsSourceComponentRef  `json:"versionEvidence"`
	}
	var fullText *LyricsSourceComponentRef
	if provenance.FullText.RenditionKey != "" {
		copy := provenance.FullText
		fullText = &copy
	}
	return json.Marshal(wire{
		FullText: fullText, GameText: provenance.GameText,
		PerformerSegmentation: provenance.PerformerSegmentation,
		GameProjection:        provenance.GameProjection, Ruby: provenance.Ruby,
		VersionEvidence: provenance.VersionEvidence,
	})
}

func (provenance *LyricsSourceComponentProvenance) UnmarshalJSON(body []byte) error {
	if provenance == nil {
		return fmt.Errorf("lyrics source provenance target is nil")
	}
	type wire struct {
		FullText              *LyricsSourceComponentRef `json:"fullText,omitempty"`
		GameText              *LyricsSourceComponentRef `json:"gameText,omitempty"`
		PerformerSegmentation *LyricsSourceComponentRef `json:"performerSegmentation,omitempty"`
		GameProjection        *LyricsSourceComponentRef `json:"gameProjection,omitempty"`
		Ruby                  *LyricsSourceComponentRef `json:"ruby,omitempty"`
		VersionEvidence       LyricsSourceComponentRef  `json:"versionEvidence"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value wire
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("lyrics source provenance contains trailing JSON")
		}
		return err
	}
	var fullText LyricsSourceComponentRef
	if value.FullText != nil {
		fullText = *value.FullText
	}
	*provenance = LyricsSourceComponentProvenance{
		FullText: fullText, GameText: value.GameText,
		PerformerSegmentation: value.PerformerSegmentation,
		GameProjection:        value.GameProjection, Ruby: value.Ruby,
		VersionEvidence: value.VersionEvidence,
	}
	return nil
}

// LyricsSourceFullLine is authoritative source text. Game-size data references
// these stable IDs instead of carrying a second, potentially divergent copy of
// lyric text. Existing ruby spans are reused without adding a romanized field.
type LyricsSourceFullLine struct {
	ID                   string                `json:"id"`
	Text                 string                `json:"text"`
	StanzaBreakBefore    bool                  `json:"stanzaBreakBefore,omitempty"`
	Segments             []LyricsSourceSegment `json:"segments"`
	TrailingPerformerIDs []string              `json:"trailingPerformerIds"`
}

type LyricsSourceFull struct {
	Version              LyricsSourceVersion     `json:"version"`
	Performers           []LyricsSourcePerformer `json:"performers"`
	RubyGeneratorVersion string                  `json:"rubyGeneratorVersion,omitempty"`
	Lines                []LyricsSourceFullLine  `json:"lines"`
}

type LyricsSourceVersionReasonCode string

const (
	LyricsSourceVersionReasonTaggedFullAndGame              LyricsSourceVersionReasonCode = "tagged_full_and_game"
	LyricsSourceVersionReasonTaggedGameOnly                 LyricsSourceVersionReasonCode = "tagged_game_only"
	LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid LyricsSourceVersionReasonCode = "tagged_game_only_full_from_vocaloid"
	LyricsSourceVersionReasonUntaggedUncutIdentity          LyricsSourceVersionReasonCode = "untagged_uncut_identity"
	LyricsSourceVersionReasonUntaggedGameSubset             LyricsSourceVersionReasonCode = "untagged_game_subset"
	LyricsSourceVersionReasonUntaggedFullOnly               LyricsSourceVersionReasonCode = "untagged_full_only"
	LyricsSourceVersionReasonVersionConflict                LyricsSourceVersionReasonCode = "version_conflict"
)

// LyricsSourceGameProjection is an ordered subset of Full. LineIDs must be
// unique and preserve Full order; the projection never owns lyric text. The
// document-level version reason code explains whether a projection is allowed.
type LyricsSourceGameProjection struct {
	LineIDs []string `json:"lineIds"`
}

// LyricsSourceAlternateVocalProvenance keeps every auxiliary vocal tab bound to
// the same immutable fixed-identity graph as the primary rendition. Alternate
// Game text is retained independently when the provider does not expose a
// safe identity projection into the alternate Full text.
type LyricsSourceAlternateVocalProvenance struct {
	FullText        *LyricsSourceComponentRef `json:"fullText,omitempty"`
	GameText        *LyricsSourceComponentRef `json:"gameText,omitempty"`
	GameProjection  *LyricsSourceComponentRef `json:"gameProjection,omitempty"`
	VersionEvidence LyricsSourceComponentRef  `json:"versionEvidence"`
}

type LyricsSourceAlternateVocal struct {
	TabLabel       string                               `json:"tabLabel"`
	SingerLabel    string                               `json:"singerLabel"`
	SingerIDs      []string                             `json:"singerIds"`
	Full           *LyricsSourceFull                    `json:"full,omitempty"`
	Game           *LyricsSourceFull                    `json:"game,omitempty"`
	GameProjection *LyricsSourceGameProjection          `json:"gameProjection,omitempty"`
	Provenance     LyricsSourceAlternateVocalProvenance `json:"provenance"`
}

// CloneLyricsSourceFull returns a deep copy suitable for crossing a contract
// boundary without sharing mutable line, segment, performer, or ruby slices.
func CloneLyricsSourceFull(input *LyricsSourceFull) *LyricsSourceFull {
	if input == nil {
		return nil
	}
	result := *input
	result.Performers = cloneLyricsSourcePerformers(input.Performers)
	result.Lines = make([]LyricsSourceFullLine, len(input.Lines))
	for lineIndex, line := range input.Lines {
		result.Lines[lineIndex] = line
		result.Lines[lineIndex].Segments = cloneLyricsSourceSegments(line.Segments)
		result.Lines[lineIndex].TrailingPerformerIDs = cloneStringsPreservingNil(line.TrailingPerformerIDs)
	}
	return &result
}

// CloneLyricsSourceAlternateVocals deep-copies every auxiliary rendition,
// including its independent Full/Game artifacts and optional projection.
func CloneLyricsSourceAlternateVocals(input []LyricsSourceAlternateVocal) []LyricsSourceAlternateVocal {
	if input == nil {
		return nil
	}
	result := make([]LyricsSourceAlternateVocal, len(input))
	for index, alternate := range input {
		result[index] = alternate
		result[index].SingerIDs = cloneStringsPreservingNil(alternate.SingerIDs)
		result[index].Full = CloneLyricsSourceFull(alternate.Full)
		result[index].Game = CloneLyricsSourceFull(alternate.Game)
		if alternate.GameProjection != nil {
			projection := *alternate.GameProjection
			projection.LineIDs = cloneStringsPreservingNil(alternate.GameProjection.LineIDs)
			result[index].GameProjection = &projection
		}
		result[index].Provenance = alternate.Provenance
		if alternate.Provenance.FullText != nil {
			ref := *alternate.Provenance.FullText
			result[index].Provenance.FullText = &ref
		}
		if alternate.Provenance.GameText != nil {
			ref := *alternate.Provenance.GameText
			result[index].Provenance.GameText = &ref
		}
		if alternate.Provenance.GameProjection != nil {
			ref := *alternate.Provenance.GameProjection
			result[index].Provenance.GameProjection = &ref
		}
	}
	return result
}

type LyricsSourcePerformerSegmentationEvidence string

const LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured LyricsSourcePerformerSegmentationEvidence = "authoritative_complete_structured"

type LyricsSourcePrivateReview struct {
	PerformerSegmentationEvidence LyricsSourcePerformerSegmentationEvidence `json:"performerSegmentationEvidence"`
}

// LyricsSourceDocument is the provider-aware source contract. ReasonCode is a
// required, auditable version classification; GameProjection is permitted only
// for the classifications defined by its validator. The pre-v21 extraction
// structs remain unchanged; NewLyricsSourceFullFromLegacy and
// LegacyExtractedLines provide a lossless bridge for their Full data.
type LyricsSourceDocument struct {
	SchemaVersion   int                             `json:"schemaVersion"`
	ReasonCode      LyricsSourceVersionReasonCode   `json:"reasonCode"`
	FixedIdentities []LyricsSourceFixedIdentity     `json:"fixedIdentities"`
	Provenance      LyricsSourceComponentProvenance `json:"provenance"`
	// Full remains a value in Go so existing source-v1 callers keep compiling.
	// For source-v2 game-only documents it is the zero value and is omitted from
	// JSON by MarshalJSON; Game owns the authoritative text instead.
	Full            LyricsSourceFull             `json:"full"`
	Game            *LyricsSourceFull            `json:"game,omitempty"`
	GameProjection  *LyricsSourceGameProjection  `json:"gameProjection,omitempty"`
	AlternateVocals []LyricsSourceAlternateVocal `json:"alternateVocals,omitempty"`
	PrivateReview   *LyricsSourcePrivateReview   `json:"privateReview,omitempty"`
	Renditions      []LyricsSourceRendition      `json:"renditions,omitempty"`
}

func (document LyricsSourceDocument) hasFull() bool {
	return len(document.Full.Lines) > 0
}

func (document LyricsSourceDocument) MarshalJSON() ([]byte, error) {
	if document.SchemaVersion == LyricsSourceDocumentSchemaVersionV3 {
		type v3Wire struct {
			SchemaVersion   int                         `json:"schemaVersion"`
			FixedIdentities []LyricsSourceFixedIdentity `json:"fixedIdentities"`
			Renditions      []LyricsSourceRendition     `json:"renditions"`
		}
		return json.Marshal(v3Wire{
			SchemaVersion: document.SchemaVersion, FixedIdentities: document.FixedIdentities,
			Renditions: document.Renditions,
		})
	}
	type legacyWire struct {
		SchemaVersion   int                             `json:"schemaVersion"`
		ReasonCode      LyricsSourceVersionReasonCode   `json:"reasonCode"`
		FixedIdentities []LyricsSourceFixedIdentity     `json:"fixedIdentities"`
		Provenance      LyricsSourceComponentProvenance `json:"provenance"`
		Full            *LyricsSourceFull               `json:"full,omitempty"`
		Game            *LyricsSourceFull               `json:"game,omitempty"`
		GameProjection  *LyricsSourceGameProjection     `json:"gameProjection,omitempty"`
		AlternateVocals []LyricsSourceAlternateVocal    `json:"alternateVocals,omitempty"`
		PrivateReview   *LyricsSourcePrivateReview      `json:"privateReview,omitempty"`
		Renditions      []LyricsSourceRendition         `json:"renditions,omitempty"`
	}
	var full *LyricsSourceFull
	if document.hasFull() {
		copy := document.Full
		full = &copy
	}
	return json.Marshal(legacyWire{
		SchemaVersion: document.SchemaVersion, ReasonCode: document.ReasonCode,
		FixedIdentities: document.FixedIdentities, Provenance: document.Provenance,
		Full: full, Game: document.Game, GameProjection: document.GameProjection,
		AlternateVocals: document.AlternateVocals, PrivateReview: document.PrivateReview,
		Renditions: document.Renditions,
	})
}

func (document *LyricsSourceDocument) UnmarshalJSON(body []byte) error {
	if document == nil {
		return fmt.Errorf("lyrics source document target is nil")
	}
	var envelope struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if envelope.SchemaVersion == LyricsSourceDocumentSchemaVersionV3 {
		type v3Wire struct {
			SchemaVersion   int                         `json:"schemaVersion"`
			FixedIdentities []LyricsSourceFixedIdentity `json:"fixedIdentities"`
			Renditions      []LyricsSourceRendition     `json:"renditions"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var value v3Wire
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				return fmt.Errorf("lyrics source document contains trailing JSON")
			}
			return err
		}
		*document = LyricsSourceDocument{
			SchemaVersion: value.SchemaVersion, FixedIdentities: value.FixedIdentities,
			Renditions: value.Renditions,
		}
		return nil
	}
	type legacyWire struct {
		SchemaVersion   int                             `json:"schemaVersion"`
		ReasonCode      LyricsSourceVersionReasonCode   `json:"reasonCode"`
		FixedIdentities []LyricsSourceFixedIdentity     `json:"fixedIdentities"`
		Provenance      LyricsSourceComponentProvenance `json:"provenance"`
		Full            *LyricsSourceFull               `json:"full,omitempty"`
		Game            *LyricsSourceFull               `json:"game,omitempty"`
		GameProjection  *LyricsSourceGameProjection     `json:"gameProjection,omitempty"`
		AlternateVocals []LyricsSourceAlternateVocal    `json:"alternateVocals,omitempty"`
		PrivateReview   *LyricsSourcePrivateReview      `json:"privateReview,omitempty"`
		Renditions      []LyricsSourceRendition         `json:"renditions,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value legacyWire
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("lyrics source document contains trailing JSON")
		}
		return err
	}
	var full LyricsSourceFull
	if value.Full != nil {
		full = *value.Full
	}
	*document = LyricsSourceDocument{
		SchemaVersion: value.SchemaVersion, ReasonCode: value.ReasonCode,
		FixedIdentities: value.FixedIdentities, Provenance: value.Provenance,
		Full: full, Game: value.Game, GameProjection: value.GameProjection,
		AlternateVocals: value.AlternateVocals, PrivateReview: value.PrivateReview,
		Renditions: value.Renditions,
	}
	return nil
}

// NewLyricsSourceFullFromLegacy upgrades the existing Full-only extraction
// model with deterministic line IDs while preserving text, performer metadata,
// stanza markers, and every ruby span exactly.
func NewLyricsSourceFullFromLegacy(
	version LyricsSourceVersion,
	performers []LyricsSourcePerformer,
	rubyGeneratorVersion string,
	lines []LyricsSourceExtractedLine,
) LyricsSourceFull {
	full := LyricsSourceFull{
		Version:              version,
		Performers:           cloneLyricsSourcePerformers(performers),
		RubyGeneratorVersion: rubyGeneratorVersion,
		Lines:                make([]LyricsSourceFullLine, len(lines)),
	}
	for index, line := range lines {
		full.Lines[index] = LyricsSourceFullLine{
			ID:                   fmt.Sprintf("full-%06d", index+1),
			Text:                 line.Japanese,
			StanzaBreakBefore:    line.StanzaBreakBefore,
			Segments:             cloneLyricsSourceSegments(line.Segments),
			TrailingPerformerIDs: cloneStringsPreservingNil(line.TrailingPerformerIDs),
		}
	}
	return full
}

// LegacyExtractedLines drops only the vNext line IDs. It is intentionally
// lossless for every field understood by the existing Full-only model.
func (full LyricsSourceFull) LegacyExtractedLines() []LyricsSourceExtractedLine {
	if full.Lines == nil {
		return nil
	}
	lines := make([]LyricsSourceExtractedLine, len(full.Lines))
	for index, line := range full.Lines {
		lines[index] = LyricsSourceExtractedLine{
			Japanese:             line.Text,
			StanzaBreakBefore:    line.StanzaBreakBefore,
			Segments:             cloneLyricsSourceSegments(line.Segments),
			TrailingPerformerIDs: cloneStringsPreservingNil(line.TrailingPerformerIDs),
		}
	}
	return lines
}

func cloneLyricsSourcePerformers(input []LyricsSourcePerformer) []LyricsSourcePerformer {
	if input == nil {
		return nil
	}
	return append([]LyricsSourcePerformer{}, input...)
}

func cloneLyricsSourceSegments(input []LyricsSourceSegment) []LyricsSourceSegment {
	if input == nil {
		return nil
	}
	result := make([]LyricsSourceSegment, len(input))
	for index, segment := range input {
		result[index] = segment
		result[index].PerformerIDs = cloneStringsPreservingNil(segment.PerformerIDs)
		if segment.Ruby == nil {
			result[index].Ruby = nil
		} else {
			result[index].Ruby = append([]LyricsSourceRubySpan{}, segment.Ruby...)
			for spanIndex, span := range segment.Ruby {
				if span.ReadingEvidence != nil {
					evidence := *span.ReadingEvidence
					result[index].Ruby[spanIndex].ReadingEvidence = &evidence
				}
			}
		}
	}
	return result
}

func cloneStringsPreservingNil(input []string) []string {
	if input == nil {
		return nil
	}
	return append([]string{}, input...)
}
