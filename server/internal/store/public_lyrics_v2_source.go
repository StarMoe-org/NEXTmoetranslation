package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"reflect"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func (s *Store) loadPublicLyricsSourceBundle(q queryRower, musicID int) (*publicLyricsSourceBundle, error) {
	var bundle publicLyricsSourceBundle
	var schemaVersion int
	var reasonCode string
	if err := q.QueryRow(`SELECT document_id,schema_version,reason_code,document_json,document_sha256
		FROM song_lyrics_source_documents WHERE music_id=?`, musicID).Scan(
		&bundle.documentID, &schemaVersion, &reasonCode, &bundle.documentJSON, &bundle.documentSHA,
	); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(bundle.documentJSON))
	if hex.EncodeToString(digest[:]) != bundle.documentSHA {
		return nil, fmt.Errorf("lyrics source document %d checksum does not match its stored JSON", musicID)
	}
	document, err := model.DecodeLyricsSourceDocument([]byte(bundle.documentJSON))
	if err != nil || document.SchemaVersion != schemaVersion || string(document.ReasonCode) != reasonCode {
		return nil, fmt.Errorf("lyrics source document %d is invalid or stale", musicID)
	}
	validatePublicFull := func(full model.LyricsSourceFull) bool {
		canonicalRubyVersion, rubyVersionErr := lyricssource.RecoveryPersistedRubyGeneratorVersion(full.RubyGeneratorVersion)
		return lyricscontract.ValidatePersistedPerformerMetadata(full) == nil && rubyVersionErr == nil &&
			canonicalRubyVersion == full.RubyGeneratorVersion
	}
	if !validatePublicFull(document.Full) {
		return nil, fmt.Errorf("lyrics source document %d has unsafe public metadata", musicID)
	}
	for _, alternate := range document.AlternateVocals {
		for _, full := range []*model.LyricsSourceFull{alternate.Full, alternate.Game} {
			if full != nil && !validatePublicFull(*full) {
				return nil, fmt.Errorf("lyrics source document %d has unsafe alternate vocal metadata", musicID)
			}
		}
	}
	canonicalDocumentJSON, err := json.Marshal(document)
	if err != nil || string(canonicalDocumentJSON) != bundle.documentJSON {
		return nil, fmt.Errorf("lyrics source document %d JSON is not canonical", musicID)
	}
	bundle.document = document
	identities := make(map[string]model.LyricsSourceFixedIdentity, len(document.FixedIdentities))
	for _, identity := range document.FixedIdentities {
		identities[identity.RenditionKey] = identity
	}
	rows, err := q.Query(`SELECT provider,rendition_key,origin,page_id,revision_id,revision_timestamp,
		mediawiki_sha1,page_title,canonical_revision_url,fetched_at,categories_json,section,
		composition_rendition_key,version_reason,index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256
		FROM song_lyrics_source_artifacts WHERE document_id=? ORDER BY rendition_key`, bundle.documentID)
	if err != nil {
		return nil, err
	}
	seenArtifacts := make(map[string]bool, len(identities))
	for rows.Next() {
		var stored model.LyricsSourceFixedIdentity
		var categoriesJSON, evidenceJSON, identityJSON, identitySHA string
		if err := rows.Scan(&stored.Provider, &stored.RenditionKey, &stored.Origin, &stored.PageID, &stored.RevisionID,
			&stored.RevisionTimestamp, &stored.SHA1, &stored.Title, &stored.CanonicalURL, &stored.FetchedAt,
			&categoriesJSON, &stored.Section, &stored.CompositionRenditionKey, &stored.VersionReason,
			&evidenceJSON, &identityJSON, &identitySHA); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(categoriesJSON), &stored.Categories); err != nil {
			rows.Close()
			return nil, fmt.Errorf("lyrics source document %d artifact categories: %w", musicID, err)
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &stored.IndexEvidenceRefs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("lyrics source document %d artifact index evidence: %w", musicID, err)
		}
		decoded, err := model.DecodeLyricsSourceFixedIdentity([]byte(identityJSON))
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("lyrics source document %d artifact identity: %w", musicID, err)
		}
		canonicalIdentityJSON, err := json.Marshal(decoded)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("lyrics source document %d artifact identity: %w", musicID, err)
		}
		identityDigest := sha256.Sum256([]byte(identityJSON))
		expected, exists := identities[stored.RenditionKey]
		if !exists || seenArtifacts[stored.RenditionKey] ||
			!publicLyricsFixedIdentityScalarsMatch(stored, decoded) ||
			!reflect.DeepEqual(decoded, expected) || string(canonicalIdentityJSON) != identityJSON ||
			hex.EncodeToString(identityDigest[:]) != identitySHA {
			rows.Close()
			return nil, fmt.Errorf("lyrics source document %d artifact %q does not match its fixed identity", musicID, stored.RenditionKey)
		}
		seenArtifacts[stored.RenditionKey] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(seenArtifacts) != len(identities) {
		return nil, fmt.Errorf("lyrics source document %d does not have one artifact for every fixed identity", musicID)
	}

	expectedContributions := publicLyricsSourceComponentRefs(document)
	bundle.contributions = make(map[string]string, len(expectedContributions))
	rows, err = q.Query(`SELECT component,rendition_key,contribution_sha256
		FROM song_lyrics_component_contributions WHERE document_id=? ORDER BY component`, bundle.documentID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var component, renditionKey, contributionSHA string
		if err := rows.Scan(&component, &renditionKey, &contributionSHA); err != nil {
			rows.Close()
			return nil, err
		}
		expectedKey, exists := expectedContributions[component]
		contributionDigest := sha256.Sum256([]byte(bundle.documentSHA + "\x00" + component + "\x00" + renditionKey))
		if !exists || expectedKey != renditionKey || bundle.contributions[component] != "" ||
			hex.EncodeToString(contributionDigest[:]) != contributionSHA {
			rows.Close()
			return nil, fmt.Errorf("lyrics source document %d has a stale %s contribution", musicID, component)
		}
		bundle.contributions[component] = renditionKey
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(bundle.contributions) != len(expectedContributions) {
		return nil, fmt.Errorf("lyrics source document %d has incomplete component contributions", musicID)
	}
	if err := s.validatePublicLyricsSourceBundleMatchesStoredLyrics(q, musicID, &bundle); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func publicLyricsFixedIdentityScalarsMatch(stored, canonical model.LyricsSourceFixedIdentity) bool {
	return stored.Provider == canonical.Provider && stored.Origin == canonical.Origin &&
		stored.PageID == canonical.PageID && stored.RevisionID == canonical.RevisionID &&
		stored.RevisionTimestamp == canonical.RevisionTimestamp && stored.SHA1 == canonical.SHA1 &&
		stored.Title == canonical.Title && stored.CanonicalURL == canonical.CanonicalURL &&
		stored.FetchedAt == canonical.FetchedAt && reflect.DeepEqual(stored.Categories, canonical.Categories) &&
		stored.Section == canonical.Section && stored.RenditionKey == canonical.RenditionKey &&
		stored.CompositionRenditionKey == canonical.CompositionRenditionKey &&
		stored.VersionReason == canonical.VersionReason &&
		reflect.DeepEqual(stored.IndexEvidenceRefs, canonical.IndexEvidenceRefs)
}

func (s *Store) validatePublicLyricsSourceBundleMatchesStoredLyrics(q queryRower, musicID int, bundle *publicLyricsSourceBundle) error {
	stored, err := s.loadLyrics(q, musicID)
	if err != nil {
		return err
	}
	lyrics := stored.lyrics
	fullIdentity, ok := publicLyricsFixedIdentity(bundle.document, bundle.document.Provenance.FullText.RenditionKey)
	if !ok || lyrics.SourcePageID != fullIdentity.PageID || lyrics.SourceRevisionID != fullIdentity.RevisionID ||
		lyrics.SourceSHA1 != fullIdentity.SHA1 || lyrics.SourceURL != fullIdentity.CanonicalURL ||
		lyrics.SourceFetchedAt != fullIdentity.FetchedAt || len(lyrics.Lines) != len(bundle.document.Full.Lines) {
		return fmt.Errorf("lyrics source document %d does not match its editable source identity", musicID)
	}
	for lineIndex, sourceLine := range bundle.document.Full.Lines {
		line := lyrics.Lines[lineIndex]
		if line.ID != sourceLine.ID || line.Order != lineIndex || line.Japanese != sourceLine.Text ||
			line.StanzaBreakBefore != sourceLine.StanzaBreakBefore || len(line.Segments) != len(sourceLine.Segments) {
			return fmt.Errorf("lyrics source document %d authoritative Full line %d is stale", musicID, lineIndex+1)
		}
		for segmentIndex, sourceSegment := range sourceLine.Segments {
			segment := line.Segments[segmentIndex]
			if segment.Text != sourceSegment.Text || !publicLyricsRubyMatches(segment.Ruby, sourceSegment.Ruby) {
				return fmt.Errorf("lyrics source document %d authoritative Full line %d segment %d is stale", musicID, lineIndex+1, segmentIndex+1)
			}
		}
	}
	return nil
}

func publicLyricsSourceComponentRefs(document model.LyricsSourceDocument) map[string]string {
	refs := map[string]string{
		"full_text":        document.Provenance.FullText.RenditionKey,
		"version_evidence": document.Provenance.VersionEvidence.RenditionKey,
	}
	if document.Provenance.PerformerSegmentation != nil {
		refs["performer_segmentation"] = document.Provenance.PerformerSegmentation.RenditionKey
	}
	if document.Provenance.GameProjection != nil {
		refs["game_projection"] = document.Provenance.GameProjection.RenditionKey
	}
	if document.Provenance.Ruby != nil {
		refs["ruby"] = document.Provenance.Ruby.RenditionKey
	}
	for index, alternate := range document.AlternateVocals {
		prefix := fmt.Sprintf("alternate_vocal_%06d_", index+1)
		refs[prefix+"version_evidence"] = alternate.Provenance.VersionEvidence.RenditionKey
		if alternate.Provenance.FullText != nil {
			refs[prefix+"full_text"] = alternate.Provenance.FullText.RenditionKey
		}
		if alternate.Provenance.GameText != nil {
			refs[prefix+"game_text"] = alternate.Provenance.GameText.RenditionKey
		}
		if alternate.Provenance.GameProjection != nil {
			refs[prefix+"game_projection"] = alternate.Provenance.GameProjection.RenditionKey
		}
	}
	return refs
}

type publicLyricsSourcePerformerCatalog struct {
	aliases          map[string]int
	declaredUnmapped map[string]bool
}

func newPublicLyricsSourcePerformerCatalog(
	document model.LyricsSourceDocument,
	catalogPerformers catalogPerformerAliases,
) (publicLyricsSourcePerformerCatalog, error) {
	return newPublicLyricsSourcePerformerCatalogForPerformers(document.Full.Performers, catalogPerformers)
}

func newPublicLyricsSourcePerformerCatalogForPerformers(
	performers []model.LyricsSourcePerformer,
	catalogPerformers catalogPerformerAliases,
) (publicLyricsSourcePerformerCatalog, error) {
	aliases, declaredUnmapped := resolveLyricsSourcePerformerAliases(catalogPerformers, performers)
	seenCatalogIDs := make(map[int]bool, len(performers))
	for _, performer := range performers {
		mappedID := 0
		for _, candidate := range []string{
			normalizeLyricsSourcePerformerAlias(performer.PerformerID),
			normalizeLyricsSourcePerformerAlias(performer.Name),
		} {
			if performerID := aliases[candidate]; performerID > 0 {
				mappedID = performerID
				break
			}
		}
		if mappedID == 0 {
			continue
		}
		if !catalogPerformers.validIDs[mappedID] {
			return publicLyricsSourcePerformerCatalog{}, ErrLyricsSourcePerformerMapping
		}
		if seenCatalogIDs[mappedID] {
			return publicLyricsSourcePerformerCatalog{}, errors.New("lyrics source performer mapping is duplicated")
		}
		seenCatalogIDs[mappedID] = true
	}
	return publicLyricsSourcePerformerCatalog{aliases: aliases, declaredUnmapped: declaredUnmapped}, nil
}

func publicLyricsSourceSegmentPerformerIDs(
	sourceIDs []string,
	sourceCatalog publicLyricsSourcePerformerCatalog,
) ([]int, error) {
	seen := map[int]bool{}
	result := make([]int, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		normalized := normalizeLyricsSourcePerformerAlias(sourceID)
		if normalized == "" || normalized == "chorus" || normalized == "ensemble" ||
			normalized == "all" || normalized == "everyone" {
			continue
		}
		performerID := sourceCatalog.aliases[normalized]
		if performerID <= 0 {
			if sourceCatalog.declaredUnmapped[normalized] {
				continue
			}
			return nil, ErrLyricsSourcePerformerMapping
		}
		if !seen[performerID] {
			seen[performerID] = true
			result = append(result, performerID)
		}
	}
	return result, nil
}
