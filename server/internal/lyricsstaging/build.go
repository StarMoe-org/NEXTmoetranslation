package lyricsstaging

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func effectiveCompositionReason(item PreflightItem) model.LyricsSourceVersionReasonCode {
	if item.CompositionReason != "" {
		return item.CompositionReason
	}
	if item.Candidate != nil {
		return item.Candidate.VersionReason
	}
	return ""
}

func canonicalizeStagedFull(full model.LyricsSourceFull) (model.LyricsSourceFull, error) {
	canonical, err := lyricscontract.NormalizePersistedPerformerMetadata(full)
	if err != nil {
		return model.LyricsSourceFull{}, errors.New("unsafe persisted lyrics performer metadata")
	}
	canonicalRubyVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(canonical.RubyGeneratorVersion)
	if err != nil {
		return model.LyricsSourceFull{}, errors.New("unsafe persisted lyrics ruby generator metadata")
	}
	canonical.RubyGeneratorVersion = canonicalRubyVersion
	return canonical, nil
}

func canonicalizeStagedDocument(document model.LyricsSourceDocument) (model.LyricsSourceDocument, error) {
	if document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		document.FixedIdentities = append([]model.LyricsSourceFixedIdentity(nil), document.FixedIdentities...)
		document.Renditions = model.CloneLyricsSourceRenditions(document.Renditions)
		for index := range document.Renditions {
			for _, target := range []*model.LyricsSourceFull{document.Renditions[index].Full, document.Renditions[index].Game} {
				if target == nil {
					continue
				}
				canonical, err := canonicalizeStagedFull(*target)
				if err != nil {
					return model.LyricsSourceDocument{}, fmt.Errorf("unsafe rendition %q metadata", document.Renditions[index].RenditionKey)
				}
				if document.Renditions[index].Full == target {
					document.Renditions[index].Full = &canonical
				} else {
					document.Renditions[index].Game = &canonical
				}
			}
		}
		return document, nil
	}
	if len(document.Full.Lines) > 0 {
		full, err := canonicalizeStagedFull(document.Full)
		if err != nil {
			return model.LyricsSourceDocument{}, err
		}
		document.Full = full
	}
	if document.Game != nil {
		game, err := canonicalizeStagedFull(*document.Game)
		if err != nil {
			return model.LyricsSourceDocument{}, err
		}
		document.Game = &game
	}
	for index := range document.AlternateVocals {
		if document.AlternateVocals[index].Full != nil {
			full, err := canonicalizeStagedFull(*document.AlternateVocals[index].Full)
			if err != nil {
				return model.LyricsSourceDocument{}, fmt.Errorf("unsafe alternate vocal %d Full metadata", index+1)
			}
			document.AlternateVocals[index].Full = &full
		}
		if document.AlternateVocals[index].Game != nil {
			game, err := canonicalizeStagedFull(*document.AlternateVocals[index].Game)
			if err != nil {
				return model.LyricsSourceDocument{}, fmt.Errorf("unsafe alternate vocal %d Game metadata", index+1)
			}
			document.AlternateVocals[index].Game = &game
		}
	}
	if len(document.Full.Lines) > 0 {
		hasPerformerSegmentation, _ := lyricsSourceFullComponents(document.Full)
		if !hasPerformerSegmentation {
			document.Provenance.PerformerSegmentation = nil
			document.PrivateReview = nil
		}
	}
	return document, nil
}

func rebindDraftExtractionProjection(draft Draft, document model.LyricsSourceDocument) Draft {
	draft.SelectedVersion = document.Full.Version
	draft.Performers = append([]model.LyricsSourcePerformer{}, document.Full.Performers...)
	draft.RubyGeneratorVersion = document.Full.RubyGeneratorVersion
	draft.Lines = document.Full.LegacyExtractedLines()
	draft.ExtractedLinesSHA256 = model.LyricsSourceExtractedLinesSHA256(draft.Lines)
	return draft
}

// BuildRecoveryDraft creates the existing Full-owning staging draft from an
// independently validated recovery result. It does not fabricate a preflight
// classification: callers must bind the returned draft to the compact recovery
// root in the additive recovery-import manifest.
func BuildRecoveryDraft(
	musicID int,
	japaneseTitle string,
	catalogFingerprint string,
	targetMusicID int,
	associationMusicIDs []int,
	document model.LyricsSourceDocument,
	artifacts []Artifact,
	translations []string,
) (Draft, error) {
	if musicID <= 0 || targetMusicID != musicID || strings.TrimSpace(japaneseTitle) == "" ||
		!canonicalSHA256.MatchString(catalogFingerprint) {
		return Draft{}, errors.New("recovery draft catalog identity is invalid")
	}
	associations, err := sortedUniqueInts(associationMusicIDs)
	if err != nil {
		return Draft{}, fmt.Errorf("music %d associations: %w", musicID, err)
	}
	document, err = canonicalizeStagedDocument(document)
	if err != nil {
		return Draft{}, err
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		return Draft{}, fmt.Errorf("music %d recovery source document: %w", musicID, err)
	}
	canonicalArtifacts := append([]Artifact(nil), artifacts...)
	sort.Slice(canonicalArtifacts, func(left, right int) bool {
		return canonicalArtifacts[left].Identity.RenditionKey < canonicalArtifacts[right].Identity.RenditionKey
	})
	fullIdentityKey := document.Provenance.FullText.RenditionKey
	var sourceArtifact *Artifact
	for index := range canonicalArtifacts {
		artifact := &canonicalArtifacts[index]
		if artifact.Identity.RenditionKey == fullIdentityKey {
			sourceArtifact = artifact
		}
	}
	if sourceArtifact == nil {
		return Draft{}, errors.New("recovery source document has no Full-text artifact")
	}
	documentSHA, err := lyricsSourceDocumentDigest(document)
	if err != nil {
		return Draft{}, err
	}
	lines := document.Full.LegacyExtractedLines()
	draft := Draft{
		MusicID: musicID, JapaneseTitle: japaneseTitle, CatalogFingerprint: catalogFingerprint,
		TargetMusicID: targetMusicID, AssociationMusicIDs: associations,
		Source: FixedSource{
			PageID: sourceArtifact.Identity.PageID, RevisionID: sourceArtifact.Identity.RevisionID,
			SHA1: sourceArtifact.Identity.SHA1, PageTitle: sourceArtifact.Identity.Title,
			CanonicalURL:         sourceArtifact.Identity.CanonicalURL,
			Categories:           append([]string{}, sourceArtifact.Identity.Categories...),
			FetchedAt:            sourceArtifact.Identity.FetchedAt,
			RawWikitextByteCount: sourceArtifact.RawWikitextByteCount,
			RawWikitextSHA256:    sourceArtifact.RawWikitextSHA256,
		},
		SelectedVersion: document.Full.Version, Performers: append([]model.LyricsSourcePerformer{}, document.Full.Performers...),
		RubyGeneratorVersion: document.Full.RubyGeneratorVersion, Lines: lines,
		Translations:         append([]string(nil), translations...),
		ExtractedLinesSHA256: model.LyricsSourceExtractedLinesSHA256(lines),
		Artifacts:            canonicalArtifacts, Document: document, DocumentSHA256: documentSHA,
	}
	if translations == nil {
		draft.Translations = nil
	}
	draftSHA, err := draftDigest(draft)
	if err != nil {
		return Draft{}, err
	}
	draft.DraftSHA256 = draftSHA
	if err := ValidateDraft(draft); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

func BuildDraft(item PreflightItem, identity lyricscontract.CatalogIdentity, fixed lyricssource.FixedRevision) (Draft, error) {
	if err := lyricscontract.ValidateFixedPerformerSegmentationPolicy(identity, fixed); err != nil {
		return Draft{}, catalogPerformerPolicyError(item.MusicID, err)
	}
	if item.Candidate == nil {
		return Draft{}, fmt.Errorf("%w: legacy draft has no provider-aware fixed candidate", ErrManifestRebuildRequired)
	}
	candidate := item.Candidate.SourceCandidate()
	artifactKeys, err := ResolveArtifactRenditionKeys([]CandidateIdentity{*item.Candidate})
	if err != nil {
		return Draft{}, err
	}
	fixedIdentity := model.LyricsSourceFixedIdentity{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: fixed.PageID, RevisionID: fixed.RevisionID,
		SHA1: fixed.SHA1, Title: fixed.PageTitle, CanonicalURL: fixed.CanonicalURL,
		RevisionTimestamp: candidate.RevisionTimestamp,
		FetchedAt:         fixed.FetchedAt.UTC().Format(time.RFC3339Nano), Categories: append([]string{}, fixed.Categories...),
		Section: candidate.Section, RenditionKey: artifactKeys[0], CompositionRenditionKey: candidate.RenditionKey,
		VersionReason:     candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
	}
	if fixed.Document == nil {
		return BuildDraftWithProvenance(item, identity, fixed, fixedIdentity, effectiveCompositionReason(item), nil)
	}
	if err := model.ValidateLyricsSourceDocument(*fixed.Document); err != nil {
		return Draft{}, fmt.Errorf("music %d fixed source document: %w", item.MusicID, err)
	}
	if item.CompositionReason != "" && fixed.Document.ReasonCode != item.CompositionReason {
		return Draft{}, fmt.Errorf("%w: fixed document composition reason drifted from preflight", ErrManifestRebuildRequired)
	}
	if len(fixed.FixedIdentities) == 0 || !reflect.DeepEqual(fixed.FixedIdentities, fixed.Document.FixedIdentities) {
		return Draft{}, fmt.Errorf("%w: fixed source identities drifted from the source document", ErrManifestRebuildRequired)
	}
	fullRenditionKey := fixed.Document.Provenance.FullText.RenditionKey
	foundFullIdentity := false
	for _, documented := range fixed.FixedIdentities {
		if documented.RenditionKey == fullRenditionKey {
			fixedIdentity = documented
			foundFullIdentity = true
			break
		}
	}
	if !foundFullIdentity {
		return Draft{}, errors.New("fixed source document has no full-text identity")
	}
	draft, err := BuildDraftWithProvenance(item, identity, fixed, fixedIdentity, fixed.Document.ReasonCode,
		fixed.Document.GameProjection)
	if err != nil {
		return Draft{}, err
	}
	return rebindDraftDocument(draft, fixed, *fixed.Document)
}

func rebindDraftDocument(draft Draft, fixed lyricssource.FixedRevision, document model.LyricsSourceDocument) (Draft, error) {
	var err error
	document, err = canonicalizeStagedDocument(document)
	if err != nil {
		return Draft{}, err
	}
	draft = rebindDraftExtractionProjection(draft, document)
	rawDigest := sha256.Sum256(fixed.Wikitext)
	rawSHA := hex.EncodeToString(rawDigest[:])
	artifacts := make([]Artifact, len(document.FixedIdentities))
	for index, identity := range document.FixedIdentities {
		if identity.PageID != fixed.PageID || identity.RevisionID != fixed.RevisionID || identity.SHA1 != fixed.SHA1 ||
			identity.Title != fixed.PageTitle || identity.CanonicalURL != fixed.CanonicalURL ||
			identity.RevisionTimestamp != canonicalFixedRevisionTimestamp(fixed) ||
			identity.FetchedAt != fixed.FetchedAt.UTC().Format(time.RFC3339Nano) || !equalStrings(identity.Categories, fixed.Categories) {
			return Draft{}, fmt.Errorf("%w: fixed source document requires unavailable raw bytes for rendition %q",
				ErrManifestRebuildRequired, identity.RenditionKey)
		}
		artifact := Artifact{Identity: identity, RawWikitextByteCount: len(fixed.Wikitext), RawWikitextSHA256: rawSHA}
		artifactSHA, err := stagedArtifactDigest(artifact)
		if err != nil {
			return Draft{}, err
		}
		artifact.ArtifactSHA256 = artifactSHA
		artifacts[index] = artifact
	}
	documentSHA, err := lyricsSourceDocumentDigest(document)
	if err != nil {
		return Draft{}, err
	}
	draft.Document = document
	draft.DocumentSHA256 = documentSHA
	draft.Artifacts = artifacts
	draft.DraftSHA256 = ""
	draftSHA, err := draftDigest(draft)
	if err != nil {
		return Draft{}, err
	}
	draft.DraftSHA256 = draftSHA
	if err := ValidateDraft(draft); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

// NewRecoveryArtifact binds one fixed identity to the exact minimized source
// bytes reconstructed or hydrated by the recovery adapter.
func NewRecoveryArtifact(identity model.LyricsSourceFixedIdentity, raw []byte) (Artifact, error) {
	if err := model.ValidateLyricsSourceFixedIdentity(identity); err != nil {
		return Artifact{}, err
	}
	if len(raw) == 0 || len(raw) > 2<<20 || !utf8.Valid(raw) {
		return Artifact{}, errors.New("recovery artifact raw bytes are invalid")
	}
	digest := sha256.Sum256(raw)
	artifact := Artifact{
		Identity: identity, RawWikitextByteCount: len(raw), RawWikitextSHA256: hex.EncodeToString(digest[:]),
	}
	var err error
	artifact.ArtifactSHA256, err = stagedArtifactDigest(artifact)
	if err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

// ValidateRecoveryArtifact verifies the compact artifact without requiring its
// private raw bytes to remain resident in the import manifest.
func ValidateRecoveryArtifact(artifact Artifact) error {
	if err := model.ValidateLyricsSourceFixedIdentity(artifact.Identity); err != nil {
		return err
	}
	if artifact.RawWikitextByteCount <= 0 || artifact.RawWikitextByteCount > 2<<20 ||
		!canonicalSHA256.MatchString(artifact.RawWikitextSHA256) || !canonicalSHA256.MatchString(artifact.ArtifactSHA256) {
		return errors.New("recovery artifact metadata is invalid")
	}
	digest, err := stagedArtifactDigest(artifact)
	if err != nil {
		return err
	}
	if digest != artifact.ArtifactSHA256 {
		return errors.New("recovery artifact digest does not match")
	}
	return nil
}

func validateFixedRevisionContentBinding(candidate CandidateIdentity, fixed lyricssource.FixedRevision) error {
	if candidate.Provider == model.LyricsSourceProviderSekaipedia {
		if !canonicalSHA256.MatchString(fixed.RawSHA256) {
			return errors.New("Sekaipedia full-revision SHA256 is missing")
		}
		if expected := lyricssource.SekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines); len(expected) == 0 || !bytes.Equal(fixed.Wikitext, expected) {
			return errors.New("selected Japanese bytes do not match the structured extraction")
		}

		// Sekaipedia's MediaWiki SHA1 identifies the complete immutable page
		// revision, while FixedRevision carries only the selected Japanese column.
		// Require the exact private API response to bind that full revision before
		// accepting provider-minimized bytes whose SHA1 necessarily differs.
		sourceCandidate := candidate.SourceCandidate()
		sourceCandidate.RawSHA256 = fixed.RawSHA256
		sourceCandidate.IndexEvidence = fixed.IndexEvidence
		if err := lyricssource.ValidateCandidateIndexEvidence(sourceCandidate); err != nil {
			return fmt.Errorf("selected Japanese bytes are not backed by exact Sekaipedia revision evidence: %w", err)
		}
		return nil
	}

	digest := sha1.Sum(fixed.Wikitext)
	wikitextSHA1 := hex.EncodeToString(digest[:])
	if wikitextSHA1 != fixed.SHA1 || wikitextSHA1 != candidate.SHA1 {
		return errors.New("SHA1 does not match its exact wikitext bytes")
	}
	return nil
}

// BuildDraftWithProvenance creates a manifest-v3 draft only when the caller
// supplies the exact provider-aware fixed identity and auditable version
// classification that the legacy FixedRevision shape cannot represent.
func BuildDraftWithProvenance(
	item PreflightItem,
	identity lyricscontract.CatalogIdentity,
	fixed lyricssource.FixedRevision,
	fixedIdentity model.LyricsSourceFixedIdentity,
	reasonCode model.LyricsSourceVersionReasonCode,
	gameProjection *model.LyricsSourceGameProjection,
) (Draft, error) {
	if err := validatePreflightItem("unique_complete", item); err != nil {
		return Draft{}, err
	}
	if identity.MusicID != item.MusicID || identity.JapaneseTitle != item.JapaneseTitle ||
		identity.CatalogFingerprint != item.CatalogFingerprint {
		return Draft{}, fmt.Errorf("music %d catalog identity does not match the complete preflight report", item.MusicID)
	}
	candidate := item.Candidate.SourceCandidate()
	artifactKeys, err := ResolveArtifactRenditionKeys([]CandidateIdentity{*item.Candidate})
	if err != nil {
		return Draft{}, err
	}
	if fixed.Provider != candidate.Provider || fixed.Origin != candidate.Origin || fixed.PageID != candidate.PageID ||
		fixed.RevisionID != candidate.RevisionID || canonicalFixedRevisionTimestamp(fixed) != candidate.RevisionTimestamp ||
		fixed.SHA1 != candidate.SHA1 || fixed.PageTitle != candidate.Title || fixed.CanonicalURL != candidate.CanonicalURL ||
		!equalStrings(fixed.Categories, candidate.Categories) ||
		fixed.Section != candidate.Section || fixed.RenditionKey != candidate.RenditionKey ||
		fixed.VersionReason != candidate.VersionReason || !equalIndexEvidenceRefs(fixed.IndexEvidenceRefs, candidate.IndexEvidenceRefs) ||
		reasonCode != effectiveCompositionReason(item) || reasonCode == model.LyricsSourceVersionReasonVersionConflict {
		return Draft{}, fmt.Errorf("music %d fixed source identity drifted from the complete preflight report", item.MusicID)
	}
	if err := model.ValidateLyricsSourceFixedIdentity(fixedIdentity); err != nil {
		return Draft{}, fmt.Errorf("music %d fixed source provenance: %w", item.MusicID, err)
	}
	if fixedIdentity.Provider != candidate.Provider || fixedIdentity.Origin != candidate.Origin ||
		fixedIdentity.PageID != fixed.PageID || fixedIdentity.RevisionID != fixed.RevisionID || fixedIdentity.SHA1 != fixed.SHA1 ||
		fixedIdentity.Title != fixed.PageTitle || fixedIdentity.CanonicalURL != fixed.CanonicalURL ||
		fixedIdentity.RevisionTimestamp != candidate.RevisionTimestamp ||
		fixedIdentity.FetchedAt != fixed.FetchedAt.UTC().Format(time.RFC3339Nano) ||
		!equalStrings(fixedIdentity.Categories, fixed.Categories) || fixedIdentity.Section != candidate.Section ||
		fixedIdentity.RenditionKey != artifactKeys[0] ||
		model.LyricsSourceCompositionRenditionKey(fixedIdentity) != candidate.RenditionKey ||
		(fixedIdentity.VersionReason != "" && fixedIdentity.VersionReason != candidate.VersionReason) ||
		!equalIndexEvidenceRefs(fixedIdentity.IndexEvidenceRefs, candidate.IndexEvidenceRefs) {
		return Draft{}, fmt.Errorf("music %d fixed source provenance does not identify the fetched bytes", item.MusicID)
	}
	if len(fixed.Wikitext) == 0 || len(fixed.Wikitext) > 2<<20 || !utf8.Valid(fixed.Wikitext) ||
		len(fixed.Lines) != item.LineCount || len(fixed.Extraction.Lines) != item.LineCount {
		return Draft{}, fmt.Errorf("music %d fixed source extraction does not match its preflight line count", item.MusicID)
	}
	if err := validateFixedRevisionContentBinding(*item.Candidate, fixed); err != nil {
		return Draft{}, fmt.Errorf("music %d fixed source: %w", item.MusicID, err)
	}
	for index, line := range fixed.Lines {
		if line.Japanese != fixed.Extraction.Lines[index].Japanese || line.StanzaBreakBefore != fixed.Extraction.Lines[index].StanzaBreakBefore {
			return Draft{}, fmt.Errorf("music %d legacy and structured extraction differ at line %d", item.MusicID, index+1)
		}
	}
	lines := make([]model.LyricsSourceExtractedLine, len(fixed.Extraction.Lines))
	for lineIndex, line := range fixed.Extraction.Lines {
		segments := make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			ruby := make([]model.LyricsSourceRubySpan, len(segment.Ruby))
			for rubyIndex, span := range segment.Ruby {
				ruby[rubyIndex] = model.LyricsSourceRubySpan{Text: span.Text, Reading: span.Reading}
			}
			segments[segmentIndex] = model.LyricsSourceSegment{
				Text: segment.Text, PerformerIDs: append([]string{}, segment.PerformerIDs...), Ruby: ruby,
			}
		}
		lines[lineIndex] = model.LyricsSourceExtractedLine{
			Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore, Segments: segments,
			TrailingPerformerIDs: append([]string{}, line.TrailingPerformerIDs...),
		}
	}
	performers := make([]model.LyricsSourcePerformer, len(fixed.Extraction.Performers))
	for index, performer := range fixed.Extraction.Performers {
		performers[index] = model.LyricsSourcePerformer{
			PerformerID: performer.PerformerID, Name: performer.Name, Color: performer.Color,
		}
	}
	rawDigest := sha256.Sum256(fixed.Wikitext)
	full := model.NewLyricsSourceFullFromLegacy(
		model.LyricsSourceVersion{Kind: fixed.Extraction.Version.Kind, Label: fixed.Extraction.Version.Label},
		performers,
		fixed.Extraction.RubyGeneratorVersion,
		lines,
	)
	full, err = canonicalizeStagedFull(full)
	if err != nil {
		return Draft{}, fmt.Errorf("music %d source document: %w", item.MusicID, err)
	}
	component := model.LyricsSourceComponentRef{RenditionKey: fixedIdentity.RenditionKey}
	hasPerformerSegmentation, hasRuby := lyricsSourceFullComponents(full)
	document := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersion,
		ReasonCode:      reasonCode,
		FixedIdentities: []model.LyricsSourceFixedIdentity{fixedIdentity},
		Provenance: model.LyricsSourceComponentProvenance{
			FullText: component, VersionEvidence: component,
		},
		Full: full, GameProjection: cloneGameProjection(gameProjection),
		PrivateReview: cloneFixedPrivateReview(fixed),
	}
	if hasPerformerSegmentation {
		document.Provenance.PerformerSegmentation = &component
	}
	if hasRuby {
		document.Provenance.Ruby = &component
	}
	if gameProjection != nil {
		document.Provenance.GameProjection = &component
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		return Draft{}, fmt.Errorf("music %d source document: %w", item.MusicID, err)
	}
	documentSHA, err := lyricsSourceDocumentDigest(document)
	if err != nil {
		return Draft{}, err
	}
	artifact := Artifact{
		Identity: fixedIdentity, RawWikitextByteCount: len(fixed.Wikitext),
		RawWikitextSHA256: hex.EncodeToString(rawDigest[:]),
	}
	artifact.ArtifactSHA256, err = stagedArtifactDigest(artifact)
	if err != nil {
		return Draft{}, err
	}
	associations, err := sortedUniqueInts(item.AssociationMusicIDs)
	if err != nil {
		return Draft{}, fmt.Errorf("music %d associations: %w", item.MusicID, err)
	}
	fetchedAt := fixed.FetchedAt.UTC().Format(time.RFC3339Nano)
	draft := Draft{
		MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle, CatalogFingerprint: item.CatalogFingerprint,
		TargetMusicID: item.TargetMusicID, AssociationMusicIDs: associations,
		Source: FixedSource{
			PageID: fixed.PageID, RevisionID: fixed.RevisionID, SHA1: fixed.SHA1, PageTitle: fixed.PageTitle,
			CanonicalURL: fixed.CanonicalURL, Categories: append([]string{}, fixed.Categories...), FetchedAt: fetchedAt,
			RawWikitextByteCount: len(fixed.Wikitext), RawWikitextSHA256: hex.EncodeToString(rawDigest[:]),
		},
		SelectedVersion:      full.Version,
		Performers:           append([]model.LyricsSourcePerformer{}, full.Performers...),
		RubyGeneratorVersion: full.RubyGeneratorVersion,
		Lines:                full.LegacyExtractedLines(),
		Translations:         append([]string(nil), fixed.Translations...),
		ExtractedLinesSHA256: model.LyricsSourceExtractedLinesSHA256(full.LegacyExtractedLines()),
		Artifacts:            []Artifact{artifact}, Document: document, DocumentSHA256: documentSHA,
	}
	digest, err := draftDigest(draft)
	if err != nil {
		return Draft{}, err
	}
	draft.DraftSHA256 = digest
	if err := ValidateDraft(draft); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

func ValidateDraft(draft Draft) error {
	if !canonicalSHA256.MatchString(draft.DraftSHA256) {
		return fmt.Errorf("staged music %d has an invalid draft digest", draft.MusicID)
	}
	if err := validateDraftPublicFields(draft); err != nil {
		return err
	}
	digest, err := draftDigest(draft)
	if err != nil {
		return err
	}
	if digest != draft.DraftSHA256 {
		return fmt.Errorf("staged music %d draft digest does not match", draft.MusicID)
	}
	return nil
}

func NewManifest(report PreflightReport, reportSHA256 string, drafts []Draft) (Manifest, error) {
	if err := ValidatePreflight(report); err != nil {
		return Manifest{}, err
	}
	return newManifest(report, reportSHA256, drafts)
}

// NewManifestFromValidatedPreflight builds from a report that the caller has
// already decoded and validated and whose canonical bytes it rechecks before
// publication. It avoids a second receipt-wide evidence validation pass.
func NewManifestFromValidatedPreflight(report PreflightReport, reportSHA256 string, drafts []Draft) (Manifest, error) {
	return newManifest(report, reportSHA256, drafts)
}

func newManifest(report PreflightReport, reportSHA256 string, drafts []Draft) (Manifest, error) {
	if !canonicalSHA256.MatchString(reportSHA256) {
		return Manifest{}, errors.New("complete preflight report digest is invalid")
	}
	if len(report.UniqueComplete) == 0 {
		return Manifest{}, errors.New("complete preflight report contains no unique_complete items")
	}
	canonicalDrafts := append([]Draft{}, drafts...)
	for index := range canonicalDrafts {
		canonicalDrafts[index].RenditionTranslations = cloneRenditionTranslations(canonicalDrafts[index].RenditionTranslations)
	}
	sort.Slice(canonicalDrafts, func(i, j int) bool { return canonicalDrafts[i].MusicID < canonicalDrafts[j].MusicID })
	if len(canonicalDrafts) != len(report.UniqueComplete) {
		return Manifest{}, fmt.Errorf("staged %d drafts, want all %d unique_complete items", len(canonicalDrafts), len(report.UniqueComplete))
	}
	references := make([]CatalogReference, len(report.UniqueComplete))
	for index, item := range report.UniqueComplete {
		draft := canonicalDrafts[index]
		if draft.MusicID != item.MusicID || draft.JapaneseTitle != item.JapaneseTitle ||
			draft.CatalogFingerprint != item.CatalogFingerprint || draft.TargetMusicID != item.TargetMusicID ||
			!equalInts(draft.AssociationMusicIDs, item.AssociationMusicIDs) || item.Candidate == nil ||
			draft.Source.PageID != item.Candidate.PageID || draft.Source.RevisionID != item.Candidate.RevisionID ||
			draft.Source.SHA1 != item.Candidate.SHA1 || draft.Source.PageTitle != item.Candidate.Title ||
			draft.Source.CanonicalURL != item.Candidate.CanonicalURL || !equalStrings(draft.Source.Categories, item.Candidate.Categories) ||
			len(draft.Lines) != item.LineCount {
			return Manifest{}, fmt.Errorf("staged music %d does not match its unique_complete report item", item.MusicID)
		}
		if err := ValidateDraft(draft); err != nil {
			return Manifest{}, err
		}
		if !preflightCandidateMatchesDraft(*item.Candidate, item.FixedArtifactCandidates, draft) {
			return Manifest{}, fmt.Errorf("staged music %d provider candidate drifted from its complete report item", item.MusicID)
		}
		references[index] = CatalogReference{
			MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle, CatalogFingerprint: item.CatalogFingerprint,
			TargetMusicID: item.TargetMusicID, AssociationMusicIDs: append([]int{}, item.AssociationMusicIDs...),
			LineCount: item.LineCount, PageID: item.Candidate.PageID, RevisionID: item.Candidate.RevisionID,
			SHA1: item.Candidate.SHA1,
		}
	}
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Preflight: PreflightReference{
			SchemaVersion: report.SchemaVersion, GeneratedAt: report.GeneratedAt,
			CatalogSchemaVersion: report.CatalogSchemaVersion, CatalogCount: report.CatalogCount,
			UniqueCompleteCount: len(report.UniqueComplete), ReportSHA256: reportSHA256,
		},
		CatalogReference: references,
		Items:            canonicalDrafts,
	}
	sizedManifest := manifest
	sizedManifest.BatchSHA256 = strings.Repeat("0", 64)
	if err := validateManifestSerializedSize(sizedManifest, MaxManifestBytes); err != nil {
		return Manifest{}, err
	}
	digest, err := manifestDigest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.BatchSHA256 = digest
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.CatalogReference == nil || manifest.Items == nil ||
		len(manifest.Items) != manifest.Preflight.UniqueCompleteCount || len(manifest.CatalogReference) != len(manifest.Items) ||
		len(manifest.Items) == 0 || !canonicalSHA256.MatchString(manifest.BatchSHA256) {
		return errors.New("staging manifest envelope is invalid")
	}
	if err := validatePreflightReference(manifest.Preflight); err != nil {
		return err
	}
	lastMusicID := 0
	for index, draft := range manifest.Items {
		if draft.MusicID <= lastMusicID {
			return errors.New("staging manifest drafts are not strictly ordered")
		}
		lastMusicID = draft.MusicID
		if err := ValidateDraft(draft); err != nil {
			return err
		}
		reference := manifest.CatalogReference[index]
		if reference.MusicID != draft.MusicID || reference.JapaneseTitle != draft.JapaneseTitle ||
			reference.CatalogFingerprint != draft.CatalogFingerprint || reference.TargetMusicID != draft.TargetMusicID ||
			!equalInts(reference.AssociationMusicIDs, draft.AssociationMusicIDs) || reference.LineCount != len(draft.Lines) ||
			reference.PageID != draft.Source.PageID || reference.RevisionID != draft.Source.RevisionID || reference.SHA1 != draft.Source.SHA1 {
			return fmt.Errorf("staged music %d does not match its embedded catalog reference", draft.MusicID)
		}
		if reference.MusicID <= 0 || reference.TargetMusicID != reference.MusicID || reference.JapaneseTitle == "" ||
			strings.TrimSpace(reference.JapaneseTitle) != reference.JapaneseTitle || !canonicalSHA256.MatchString(reference.CatalogFingerprint) ||
			reference.LineCount <= 0 || reference.LineCount > 1000 || reference.PageID <= 0 || reference.RevisionID <= 0 ||
			!canonicalSHA1.MatchString(reference.SHA1) || !strictlyIncreasingPositiveInts(reference.AssociationMusicIDs) {
			return fmt.Errorf("staged music %d has an invalid embedded catalog reference", draft.MusicID)
		}
	}
	if err := validateManifestSerializedSize(manifest, MaxManifestBytes); err != nil {
		return err
	}
	digest, err := manifestDigest(manifest)
	if err != nil {
		return err
	}
	if digest != manifest.BatchSHA256 {
		return errors.New("staging manifest batch digest does not match")
	}
	return nil
}

// EvidenceCandidatesFromValidatedManifest reconstructs the compact candidate
// identities whose evidence references are reachable from fixed artifacts that
// actually entered an already-validated manifest. Callers must validate or
// construct the manifest in the same closed call chain before using this seam.
func EvidenceCandidatesFromValidatedManifest(manifest Manifest) ([]CandidateIdentity, error) {
	candidates := make([]CandidateIdentity, 0, len(manifest.Items))
	for _, staged := range manifest.Items {
		for _, artifact := range staged.Artifacts {
			identity := artifact.Identity
			versionReason := identity.VersionReason
			if versionReason == "" {
				versionReason = staged.Document.ReasonCode
			}
			candidates = append(candidates, CandidateIdentity{
				Provider: identity.Provider, Origin: identity.Origin, PageID: identity.PageID,
				RevisionID: identity.RevisionID, RevisionTimestamp: identity.RevisionTimestamp,
				SHA1: identity.SHA1, Title: identity.Title,
				CanonicalURL: identity.CanonicalURL, Categories: append([]string(nil), identity.Categories...),
				Section: identity.Section, RenditionKey: model.LyricsSourceCompositionRenditionKey(identity),
				ArtifactRenditionKey: identity.RenditionKey, VersionReason: versionReason,
				IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef(nil), identity.IndexEvidenceRefs...),
			})
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: staging manifest contains no fixed artifact evidence", ErrManifestRebuildRequired)
	}
	return candidates, nil
}

func MarshalManifest(manifest Manifest) ([]byte, error) {
	if err := ValidateManifest(manifest); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode staging manifest: %w", err)
	}
	if len(body) >= MaxManifestBytes {
		return nil, fmt.Errorf("staging manifest serialized size exceeds %d bytes", MaxManifestBytes)
	}
	return append(body, '\n'), nil
}

func validateManifestSerializedSize(manifest Manifest, maximum int) error {
	if maximum <= 0 {
		return errors.New("staging manifest serialized-size budget is invalid")
	}
	size, err := manifestSerializedSize(manifest, maximum)
	if err != nil {
		return err
	}
	if size > maximum {
		return fmt.Errorf("staging manifest serialized size exceeds %d bytes", maximum)
	}
	return nil
}

func manifestSerializedSize(manifest Manifest, maximum int) (int, error) {
	skeleton := manifest
	skeleton.CatalogReference = []CatalogReference{}
	skeleton.Items = []Draft{}
	body, err := json.MarshalIndent(skeleton, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("measure staging manifest envelope: %w", err)
	}
	total := len(body) + 1
	add := func(encoded []byte) bool {
		lineCount := bytes.Count(encoded, []byte("\n")) + 1
		addition := len(encoded) + 4*lineCount + 2
		if addition > maximum || total > maximum-addition {
			total = maximum + 1
			return false
		}
		total += addition
		return true
	}
	if len(manifest.CatalogReference) > 0 {
		total += 2
	}
	for _, reference := range manifest.CatalogReference {
		encoded, err := json.MarshalIndent(reference, "", "  ")
		if err != nil {
			return 0, fmt.Errorf("measure staging catalog reference: %w", err)
		}
		if !add(encoded) {
			return total, nil
		}
	}
	if len(manifest.Items) > 0 {
		if total > maximum-2 {
			return maximum + 1, nil
		}
		total += 2
	}
	for _, draft := range manifest.Items {
		encoded, err := json.MarshalIndent(draft, "", "  ")
		if err != nil {
			return 0, fmt.Errorf("measure staged draft: %w", err)
		}
		if !add(encoded) {
			return total, nil
		}
	}
	return total, nil
}

func draftDigest(draft Draft) (string, error) {
	draft.DraftSHA256 = ""
	body, err := json.Marshal(draft)
	if err != nil {
		return "", fmt.Errorf("encode staged draft digest: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func manifestDigest(manifest Manifest) (string, error) {
	manifest.BatchSHA256 = ""
	body, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode staging manifest digest: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func lyricsSourceDocumentDigest(document model.LyricsSourceDocument) (string, error) {
	body, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode lyrics source document digest: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func stagedArtifactDigest(artifact Artifact) (string, error) {
	artifact.ArtifactSHA256 = ""
	body, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("encode staged artifact digest: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func lyricsSourceFullComponents(full model.LyricsSourceFull) (bool, bool) {
	hasPerformerSegmentation := len(full.Performers) > 0
	hasRuby := false
	for _, line := range full.Lines {
		hasPerformerSegmentation = hasPerformerSegmentation || len(line.TrailingPerformerIDs) > 0
		for _, segment := range line.Segments {
			hasPerformerSegmentation = hasPerformerSegmentation || len(segment.PerformerIDs) > 0
			for _, span := range segment.Ruby {
				hasRuby = hasRuby || span.Reading != ""
			}
		}
	}
	return hasPerformerSegmentation, hasRuby
}

func cloneGameProjection(projection *model.LyricsSourceGameProjection) *model.LyricsSourceGameProjection {
	if projection == nil {
		return nil
	}
	return &model.LyricsSourceGameProjection{LineIDs: append([]string{}, projection.LineIDs...)}
}

func cloneFixedPrivateReview(fixed lyricssource.FixedRevision) *model.LyricsSourcePrivateReview {
	if fixed.Document == nil || fixed.Document.PrivateReview == nil {
		return nil
	}
	privateReview := *fixed.Document.PrivateReview
	return &privateReview
}

func canonicalFixedRevisionTimestamp(fixed lyricssource.FixedRevision) string {
	if fixed.RevisionTimestamp.IsZero() {
		return ""
	}
	return fixed.RevisionTimestamp.UTC().Format(time.RFC3339Nano)
}

func equalFixedIdentity(left, right model.LyricsSourceFixedIdentity) bool {
	return reflect.DeepEqual(left, right)
}

func equalLyricsSourcePerformers(left, right []model.LyricsSourcePerformer) bool {
	return reflect.DeepEqual(left, right)
}

func equalLyricsSourceLines(left, right []model.LyricsSourceExtractedLine) bool {
	return reflect.DeepEqual(left, right)
}

func preflightCandidateMatchesDraft(candidate CandidateIdentity, fixedArtifactCandidates []CandidateIdentity, draft Draft) bool {
	artifactKey := ""
	if len(fixedArtifactCandidates) == 0 {
		keys, err := ResolveArtifactRenditionKeys([]CandidateIdentity{candidate})
		if err != nil {
			return false
		}
		artifactKey = keys[0]
	} else {
		keys, err := ResolveArtifactRenditionKeys(fixedArtifactCandidates)
		if err != nil {
			return false
		}
		for index, fixedCandidate := range fixedArtifactCandidates {
			if reflect.DeepEqual(fixedCandidate, candidate) {
				if artifactKey != "" {
					return false
				}
				artifactKey = keys[index]
			}
		}
		if artifactKey == "" {
			return false
		}
	}
	fullRenditionKey := draft.Document.Provenance.FullText.RenditionKey
	for _, identity := range draft.Document.FixedIdentities {
		if identity.RenditionKey != fullRenditionKey {
			continue
		}
		return identity.Provider == candidate.Provider && identity.Origin == candidate.Origin &&
			identity.PageID == candidate.PageID && identity.RevisionID == candidate.RevisionID &&
			identity.RevisionTimestamp == candidate.RevisionTimestamp && identity.SHA1 == candidate.SHA1 &&
			identity.Title == candidate.Title && identity.CanonicalURL == candidate.CanonicalURL &&
			equalStrings(identity.Categories, candidate.Categories) && identity.Section == candidate.Section &&
			identity.RenditionKey == artifactKey &&
			model.LyricsSourceCompositionRenditionKey(identity) == candidate.RenditionKey &&
			(identity.VersionReason == "" || identity.VersionReason == candidate.VersionReason) &&
			equalIndexEvidenceRefs(identity.IndexEvidenceRefs, candidate.IndexEvidenceRefs)
	}
	return false
}

func equalStrings(left, right []string) bool {
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

func equalIndexEvidenceRefs(left, right []model.LyricsSourceIndexEvidenceRef) bool {
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

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]int{}, left...)
	rightCopy := append([]int{}, right...)
	sort.Ints(leftCopy)
	sort.Ints(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}
