package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
)

func restoreLyricsRecoveryContentTx(ctx context.Context, tx *sql.Tx, lyrics LyricsContentExport) error {
	for _, record := range lyrics.RecoverySourceEvidence {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := restoreLyricsRecoverySourceEvidenceTx(ctx, tx, record); err != nil {
			return fmt.Errorf("restore lyrics recovery evidence %s/%s: %w", record.Provider, record.EvidenceID, err)
		}
	}
	for _, record := range lyrics.RecoveryBatches {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_batches
			(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
			 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
			 raw_byte_count,encoded_byte_count,actor,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			record.BatchSHA256, record.SchemaVersion, record.RootSchemaVersion, record.RootID, record.RootSHA256,
			record.CatalogCount, record.MusicIDsSHA256, record.CoverageJSON, record.EvidenceReceiptSHA256,
			record.PackSHA256, record.SelectionSHA256, record.EvidenceCount, record.ShardCount,
			record.RawByteCount, record.EncodedByteCount, record.Actor, record.CreatedAt); err != nil {
			return fmt.Errorf("restore lyrics recovery batch %s: %w", record.BatchSHA256, err)
		}
	}
	for _, record := range lyrics.RecoveryItems {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_items
			(batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,association_music_ids_json,
			 state,result_sha256,draft_sha256,document_sha256,availability_document_sha256,created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, record.BatchSHA256, record.MusicID, record.JapaneseTitle,
			record.CatalogFingerprint, record.TargetMusicID, record.AssociationMusicIDsJSON, record.State,
			record.ResultSHA256, record.DraftSHA256, record.DocumentSHA256,
			record.AvailabilityDocumentSHA256, record.CreatedAt); err != nil {
			return fmt.Errorf("restore lyrics recovery item %s/%d: %w", record.BatchSHA256, record.MusicID, err)
		}
	}
	for _, record := range lyrics.RecoveryArtifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_artifacts
			(batch_sha256,music_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,
			 mediawiki_sha1,page_title,canonical_revision_url,fetched_at,categories_json,section,
			 composition_rendition_key,version_reason,index_evidence_refs_json,fixed_identity_json,
			 fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256,created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.BatchSHA256, record.MusicID,
			record.Provider, record.RenditionKey, record.Origin, record.PageID, record.RevisionID,
			record.RevisionTimestamp, record.MediaWikiSHA1, record.PageTitle, record.CanonicalRevisionURL,
			record.FetchedAt, record.CategoriesJSON, record.Section, record.CompositionRenditionKey,
			record.VersionReason, record.IndexEvidenceRefsJSON, record.FixedIdentityJSON,
			record.FixedIdentitySHA256, record.RawByteCount, record.RawWikitextSHA256,
			record.ArtifactSHA256, record.CreatedAt); err != nil {
			return fmt.Errorf("restore lyrics recovery artifact %s/%d/%s: %w",
				record.BatchSHA256, record.MusicID, record.RenditionKey, err)
		}
	}
	for _, record := range lyrics.RecoveryArtifactEvidence {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_artifact_evidence
			(batch_sha256,music_id,rendition_key,position,provider,evidence_id,sha256)
			VALUES (?,?,?,?,?,?,?)`, record.BatchSHA256, record.MusicID, record.RenditionKey,
			record.Position, record.Provider, record.EvidenceID, record.SHA256); err != nil {
			return fmt.Errorf("restore lyrics recovery artifact evidence %s/%d/%s/%d: %w",
				record.BatchSHA256, record.MusicID, record.RenditionKey, record.Position, err)
		}
	}
	for _, record := range lyrics.RecoveryContributions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_component_contributions
			(batch_sha256,music_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?,?)`,
			record.BatchSHA256, record.MusicID, record.Component, record.RenditionKey,
			record.ContributionSHA256); err != nil {
			return fmt.Errorf("restore lyrics recovery contribution %s/%d/%s: %w",
				record.BatchSHA256, record.MusicID, record.Component, err)
		}
	}
	for _, record := range lyrics.AvailabilityDocuments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_availability_documents
			(availability_document_id,batch_sha256,music_id,schema_version,state,reason_code,no_lyrics_reason,
			 document_json,document_sha256,result_sha256,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			record.AvailabilityDocumentID, record.BatchSHA256, record.MusicID, record.SchemaVersion,
			record.State, record.ReasonCode, record.NoLyricsReason, record.DocumentJSON,
			record.DocumentSHA256, record.ResultSHA256, record.CreatedAt); err != nil {
			return fmt.Errorf("restore lyrics availability document %s/%d: %w", record.BatchSHA256, record.MusicID, err)
		}
	}
	for _, record := range lyrics.RecoveryTakeovers {
		if err := ctx.Err(); err != nil {
			return err
		}
		var schemaVersion, reasonCode, documentJSON, documentSHA, createdAt, localizationsJSON any
		if document := record.SupersededDocument; document != nil {
			schemaVersion, reasonCode, documentJSON = document.SchemaVersion, document.ReasonCode, document.DocumentJSON
			documentSHA, createdAt = document.DocumentSHA256, document.CreatedAt
			if document.LocalizationsJSON != "" {
				localizationsJSON = document.LocalizationsJSON
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_takeovers
			(music_id,batch_sha256,item_state,schema_version,reason_code,document_json,document_sha256,
			 document_created_at,localizations_json,taken_over_at,taken_over_by) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			record.MusicID, record.BatchSHA256, record.ItemState, schemaVersion, reasonCode, documentJSON,
			documentSHA, createdAt, localizationsJSON, record.TakenOverAt, record.TakenOverBy); err != nil {
			return fmt.Errorf("restore lyrics recovery takeover %d: %w", record.MusicID, err)
		}
	}
	return nil
}

func restoreLyricsRecoverySourceEvidenceTx(
	ctx context.Context,
	tx *sql.Tx,
	record LyricsRecoverySourceEvidenceBackupRecord,
) error {
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_source_evidence
		WHERE provider=? AND evidence_id=?`, record.Provider, record.EvidenceID).Scan(&existing); err != nil {
		return err
	}
	if existing == 0 {
		_, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_source_evidence
			(provider,evidence_id,sha256,acquisition_id,envelope_sha256,kind,origin,page_id,revision_id,
			 revision_timestamp,mediawiki_sha1,page_title,canonical_revision_url,categories_json,
			 canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.Provider, record.EvidenceID, record.SHA256,
			record.AcquisitionID, record.EnvelopeSHA256, record.Kind, record.Origin,
			recoveryNullablePositiveInt(record.PageID), recoveryNullablePositiveInt(record.RevisionID),
			record.RevisionTimestamp, record.MediaWikiSHA1, record.PageTitle, record.CanonicalRevisionURL,
			record.CategoriesJSON, record.CanonicalRequestURL, record.FetchedAt, record.RawBytes,
			record.RawByteCount, record.RawSHA256, record.CreatedAt)
		return err
	}
	if existing != 1 {
		return ErrLyricsRecoveryImportConflict
	}
	var stored LyricsRecoverySourceEvidenceBackupRecord
	var pageID, revisionID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT provider,evidence_id,sha256,acquisition_id,envelope_sha256,kind,origin,
		page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,canonical_revision_url,categories_json,
		canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at
		FROM lyrics_recovery_source_evidence WHERE provider=? AND evidence_id=?`, record.Provider, record.EvidenceID).Scan(
		&stored.Provider, &stored.EvidenceID, &stored.SHA256, &stored.AcquisitionID, &stored.EnvelopeSHA256,
		&stored.Kind, &stored.Origin, &pageID, &revisionID, &stored.RevisionTimestamp, &stored.MediaWikiSHA1,
		&stored.PageTitle, &stored.CanonicalRevisionURL, &stored.CategoriesJSON, &stored.CanonicalRequestURL,
		&stored.FetchedAt, &stored.RawBytes, &stored.RawByteCount, &stored.RawSHA256, &stored.CreatedAt); err != nil {
		return err
	}
	if pageID.Valid {
		stored.PageID = int(pageID.Int64)
	}
	if revisionID.Valid {
		stored.RevisionID = int(revisionID.Int64)
	}
	if stored.Provider != record.Provider || stored.EvidenceID != record.EvidenceID || stored.SHA256 != record.SHA256 ||
		stored.AcquisitionID != record.AcquisitionID || stored.EnvelopeSHA256 != record.EnvelopeSHA256 ||
		stored.Kind != record.Kind || stored.Origin != record.Origin || stored.PageID != record.PageID ||
		stored.RevisionID != record.RevisionID || stored.RevisionTimestamp != record.RevisionTimestamp ||
		stored.MediaWikiSHA1 != record.MediaWikiSHA1 || stored.PageTitle != record.PageTitle ||
		stored.CanonicalRevisionURL != record.CanonicalRevisionURL || stored.CategoriesJSON != record.CategoriesJSON ||
		stored.CanonicalRequestURL != record.CanonicalRequestURL || stored.FetchedAt != record.FetchedAt ||
		!bytes.Equal(stored.RawBytes, record.RawBytes) || stored.RawByteCount != record.RawByteCount ||
		stored.RawSHA256 != record.RawSHA256 || stored.CreatedAt != record.CreatedAt {
		return ErrLyricsRecoveryImportConflict
	}
	return nil
}
