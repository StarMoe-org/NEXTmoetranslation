package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsrecoveryimport"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

// Test seeds for the recovery-import provenance graph. The offline importer
// (internal/offlineimport) owns the production write path; these helpers write
// the same rows so store tests can exercise readers of a recovery-imported song
// without importing that package.

func seedRecoveryEvidenceTx(ctx context.Context, tx *sql.Tx, ref lyricsevidencepack.EvidenceRef,
	evidence lyricssource.IndexEvidence, now int64,
) error {
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_source_evidence WHERE provider=? AND evidence_id=?`,
		ref.Provider, ref.EvidenceID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return nil
	}
	categoriesJSON, err := seedCategoriesJSON(evidence.Categories)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_source_evidence
		(provider,evidence_id,sha256,acquisition_id,envelope_sha256,kind,origin,page_id,revision_id,
		 revision_timestamp,mediawiki_sha1,page_title,canonical_revision_url,categories_json,canonical_request_url,
		 fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, evidence.Provider, evidence.EvidenceID, evidence.SHA256,
		ref.AcquisitionID, ref.EnvelopeSHA256, evidence.Kind, evidence.Origin,
		recoveryNullablePositiveInt(evidence.PageID), recoveryNullablePositiveInt(evidence.RevisionID),
		evidence.RevisionTimestamp, evidence.MediaWikiSHA1, evidence.Title, evidence.CanonicalURL,
		categoriesJSON, evidence.CanonicalRequestURL, evidence.FetchedAt, evidence.Raw, len(evidence.Raw),
		evidence.RawSHA256, now)
	if err != nil {
		return fmt.Errorf("seed recovery evidence %s: %w", ref.EvidenceID, err)
	}
	return nil
}

func seedRecoveryImportItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, now int64,
) error {
	associationsJSON, err := json.Marshal(item.AssociationMusicIDs)
	if err != nil {
		return err
	}
	draftSHA, documentSHA := "", ""
	if item.Draft != nil {
		draftSHA, documentSHA = item.Draft.DraftSHA256, item.Draft.DocumentSHA256
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_items
		(batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,
		 state,result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, batchSHA256, item.MusicID, item.JapaneseTitle, item.CatalogFingerprint,
		item.TargetMusicID, string(associationsJSON), item.State, item.ResultSHA256, draftSHA, documentSHA,
		item.AvailabilityDocumentSHA256, now)
	return err
}

// seedRecoveryV3DraftItemTx persists the item's source-v3 document and its
// rendition localizations exactly as a recovery import would.
func seedRecoveryV3DraftItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, now int64,
) error {
	if item.Draft == nil || item.Draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
		return fmt.Errorf("music %d v3 Draft is missing", item.MusicID)
	}
	if err := ValidatePersistedLyricsSourceDocument(item.Draft.Document); err != nil {
		return fmt.Errorf("music %d Full source document is invalid: %w", item.MusicID, err)
	}
	var editable int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM song_lyrics WHERE music_id=?`, item.MusicID).Scan(&editable); err != nil {
		return err
	}
	if editable != 0 {
		return fmt.Errorf("music %d already has editable lyrics", item.MusicID)
	}
	documentJSON, err := json.Marshal(item.Draft.Document)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,?,?,?,?,?)`, item.MusicID, item.Draft.Document.SchemaVersion, item.Draft.Document.ReasonCode,
		string(documentJSON), item.Draft.DocumentSHA256, batchSHA256, now)
	if err != nil {
		return err
	}
	documentID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	return insertLyricsRenditionLocalizationsTx(ctx, tx, documentID, item.Draft.Document, item.Draft.RenditionTranslations, "recovery-import", now)
}

func seedRecoveryAvailabilityItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, documentJSON string, now int64,
) error {
	document := item.Availability
	_, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_availability_documents
		(batch_sha256,music_id,schema_version,state,reason_code,no_lyrics_reason,document_json,document_sha256,result_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, batchSHA256, item.MusicID, document.SchemaVersion, document.State,
		document.ReasonCode, document.NoLyricsReason, documentJSON, item.AvailabilityDocumentSHA256, item.ResultSHA256, now)
	return err
}

// seedRecoveryProvenanceGraphTx writes the batch-scoped artifacts, their
// evidence links, and the component contributions of one recovery item.
func seedRecoveryProvenanceGraphTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, now int64,
) error {
	artifacts := item.Artifacts
	if item.Draft != nil {
		artifacts = item.Draft.Artifacts
	}
	for _, artifact := range artifacts {
		if err := seedRecoveryArtifactTx(ctx, tx, batchSHA256, item.MusicID, artifact, now); err != nil {
			return err
		}
	}
	components := map[string]string{}
	ownerSHA := item.AvailabilityDocumentSHA256
	if item.Draft != nil {
		components = LyricsSourceComponentRefs(item.Draft.Document)
		ownerSHA = item.Draft.DocumentSHA256
	}
	keys := make([]string, 0, len(components))
	for component := range components {
		keys = append(keys, component)
	}
	sort.Strings(keys)
	for _, component := range keys {
		renditionKey := components[component]
		digest := sha256.Sum256([]byte(ownerSHA + "\x00" + component + "\x00" + renditionKey))
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_component_contributions
			(batch_sha256,music_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?,?)`,
			batchSHA256, item.MusicID, component, renditionKey, hex.EncodeToString(digest[:])); err != nil {
			return err
		}
	}
	return nil
}

func seedRecoveryArtifactTx(ctx context.Context, tx *sql.Tx, batchSHA256 string, musicID int,
	artifact lyricsstaging.Artifact, now int64,
) error {
	identityJSON, err := json.Marshal(artifact.Identity)
	if err != nil {
		return err
	}
	identityDigest := sha256.Sum256(identityJSON)
	categoriesJSON, err := seedCategoriesJSON(artifact.Identity.Categories)
	if err != nil {
		return err
	}
	evidenceJSON, err := json.Marshal(artifact.Identity.IndexEvidenceRefs)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_artifacts
		(batch_sha256,music_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,
		 page_title,canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
		 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,
		 artifact_sha256,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, batchSHA256, musicID,
		artifact.Identity.Provider, artifact.Identity.RenditionKey, artifact.Identity.Origin, artifact.Identity.PageID,
		artifact.Identity.RevisionID, artifact.Identity.RevisionTimestamp, artifact.Identity.SHA1, artifact.Identity.Title,
		artifact.Identity.CanonicalURL, artifact.Identity.FetchedAt, categoriesJSON, artifact.Identity.Section,
		artifact.Identity.CompositionRenditionKey, artifact.Identity.VersionReason, string(evidenceJSON), string(identityJSON),
		hex.EncodeToString(identityDigest[:]), artifact.RawWikitextByteCount, artifact.RawWikitextSHA256,
		artifact.ArtifactSHA256, now); err != nil {
		return err
	}
	for position, reference := range artifact.Identity.IndexEvidenceRefs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_artifact_evidence
			(batch_sha256,music_id,rendition_key,position,provider,evidence_id,sha256) VALUES (?,?,?,?,?,?,?)`,
			batchSHA256, musicID, artifact.Identity.RenditionKey, position, artifact.Identity.Provider,
			reference.EvidenceID, reference.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func seedCategoriesJSON(categories []string) (string, error) {
	if categories == nil {
		categories = []string{}
	}
	body, err := json.Marshal(categories)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
