package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/model"
)

var (
	ErrEmbeddedLyricsEditorSeedCatalogMismatch = errors.New("embedded lyrics editor seed catalog does not match the database")
	ErrEmbeddedLyricsEditorSeedCatalogNotReady = errors.New("embedded lyrics editor seed catalog is not initialized")
)

type EmbeddedLyricsEditorSeedResult struct {
	SeedSHA256        string
	Inserted          int
	PreservedExisting int
	Replayed          int
	SourceV3          int
	Legacy            int
	Availability      int
}

func (s *Store) ApplyEmbeddedLyricsEditorSeed(ctx context.Context, bundle embeddedlyricsseed.Bundle) (EmbeddedLyricsEditorSeedResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := EmbeddedLyricsEditorSeedResult{SeedSHA256: bundle.Manifest.SeedSHA256}
	release, err := s.LockContentExclusiveContext(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var existingArchiveSHA, existingReleaseID, existingSourceBatchSHA, existingRootSHA, existingPolicy string
	var existingSchemaVersion, existingCatalogCount int
	var existingMusicIDsSHA, existingFingerprintsSHA string
	var existingCreatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT archive_sha256,release_id,schema_version,source_batch_sha256,root_sha256,
		catalog_policy_version,catalog_count,music_ids_sha256,catalog_fingerprints_sha256,created_at
		FROM embedded_lyrics_editor_seed_batches WHERE seed_sha256=?`, bundle.Manifest.SeedSHA256).Scan(
		&existingArchiveSHA, &existingReleaseID, &existingSchemaVersion, &existingSourceBatchSHA, &existingRootSHA,
		&existingPolicy, &existingCatalogCount, &existingMusicIDsSHA, &existingFingerprintsSHA, &existingCreatedAt)
	if err == nil {
		if existingArchiveSHA != bundle.ArchiveSHA256 || existingReleaseID != bundle.Manifest.ReleaseID ||
			existingSchemaVersion != bundle.Manifest.SchemaVersion || existingSourceBatchSHA != bundle.Manifest.SourceBatchSHA256 ||
			existingRootSHA != bundle.Manifest.RootSHA256 || existingPolicy != bundle.Manifest.CatalogPolicyVersion ||
			existingCatalogCount != bundle.Manifest.CatalogCount || existingMusicIDsSHA != bundle.Manifest.MusicIDsSHA256 ||
			existingFingerprintsSHA != bundle.Manifest.CatalogFingerprintsSHA256 || existingCreatedAt != bundle.Manifest.CreatedAt {
			return result, errors.New("embedded lyrics editor seed batch identity changed")
		}
		if err := validateEmbeddedLyricsEditorSeedCatalogTx(ctx, tx, bundle); err != nil {
			return result, err
		}
		if err := validateEmbeddedLyricsEditorSeedReplayTx(ctx, tx, bundle); err != nil {
			return result, err
		}
		result.Replayed = len(bundle.Manifest.Items)
		if err := tx.Commit(); err != nil {
			return result, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err := validateEmbeddedLyricsEditorSeedCatalogTx(ctx, tx, bundle); err != nil {
		return result, err
	}
	if err := ensureNoOtherEmbeddedLyricsSeedBatchTx(ctx, tx, bundle.Manifest.SeedSHA256); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO embedded_lyrics_editor_seed_batches
		(seed_sha256,archive_sha256,release_id,schema_version,source_batch_sha256,root_sha256,
		 catalog_policy_version,catalog_count,music_ids_sha256,catalog_fingerprints_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, bundle.Manifest.SeedSHA256, bundle.ArchiveSHA256, bundle.Manifest.ReleaseID,
		bundle.Manifest.SchemaVersion, bundle.Manifest.SourceBatchSHA256, bundle.Manifest.RootSHA256,
		bundle.Manifest.CatalogPolicyVersion, bundle.Manifest.CatalogCount, bundle.Manifest.MusicIDsSHA256,
		bundle.Manifest.CatalogFingerprintsSHA256, bundle.Manifest.CreatedAt); err != nil {
		return result, err
	}

	documents := make(map[int]embeddedlyricsseed.SourceDocumentRecord, len(bundle.Documents))
	artifacts := make(map[int][]embeddedlyricsseed.SourceArtifactRecord, len(bundle.Documents))
	contributions := make(map[int][]embeddedlyricsseed.SourceContributionRecord, len(bundle.Documents))
	legacyDocuments := make(map[int]embeddedlyricsseed.LegacyDocumentRecord, len(bundle.LegacyDocuments))
	legacyLines := make(map[int][]embeddedlyricsseed.LegacyLineRecord, len(bundle.LegacyDocuments))
	legacySegments := make(map[int][]embeddedlyricsseed.LegacySegmentRecord, len(bundle.LegacyDocuments))
	availability := make(map[int]embeddedlyricsseed.AvailabilityRecord, len(bundle.Availability))
	for _, record := range bundle.Documents {
		documents[record.MusicID] = record
	}
	for _, record := range bundle.Artifacts {
		artifacts[record.MusicID] = append(artifacts[record.MusicID], record)
	}
	for _, record := range bundle.Contributions {
		contributions[record.MusicID] = append(contributions[record.MusicID], record)
	}
	for _, record := range bundle.LegacyDocuments {
		legacyDocuments[record.MusicID] = record
	}
	for _, record := range bundle.LegacyLines {
		legacyLines[record.MusicID] = append(legacyLines[record.MusicID], record)
	}
	for _, record := range bundle.LegacySegments {
		legacySegments[record.MusicID] = append(legacySegments[record.MusicID], record)
	}
	for _, record := range bundle.Availability {
		availability[record.MusicID] = record
	}

	verifyCustomBundle := bundle.ArchiveSHA256 != embeddedlyricsseed.ExpectedArchiveSHA256
	for _, item := range bundle.Manifest.Items {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		owned, err := lyricsOwnershipExistsTx(ctx, tx, item.MusicID)
		if err != nil {
			return result, err
		}
		applyStatus := "preserved_existing"
		if !owned {
			switch item.SeedKind {
			case "source_v3":
				record, ok := documents[item.MusicID]
				if !ok {
					return result, fmt.Errorf("embedded lyrics editor seed source-v3 item %d has no document", item.MusicID)
				}
				if err := insertEmbeddedSourceV3Tx(ctx, tx, record, artifacts[item.MusicID], contributions[item.MusicID], verifyCustomBundle); err != nil {
					return result, fmt.Errorf("insert embedded source-v3 lyrics %d: %w", item.MusicID, err)
				}
				result.SourceV3++
			case "legacy":
				record, ok := legacyDocuments[item.MusicID]
				if !ok {
					return result, fmt.Errorf("embedded lyrics editor seed legacy item %d has no document", item.MusicID)
				}
				if err := s.insertEmbeddedLegacyLyricsTx(ctx, tx, record, legacyLines[item.MusicID], legacySegments[item.MusicID]); err != nil {
					return result, fmt.Errorf("insert embedded legacy lyrics %d: %w", item.MusicID, err)
				}
				result.Legacy++
			case "availability":
				if _, ok := availability[item.MusicID]; !ok {
					return result, fmt.Errorf("embedded lyrics editor seed availability item %d has no document", item.MusicID)
				}
				result.Availability++
			default:
				return result, fmt.Errorf("embedded lyrics editor seed item %d has unsupported kind %q", item.MusicID, item.SeedKind)
			}
			applyStatus = "inserted"
			result.Inserted++
		} else {
			result.PreservedExisting++
		}
		if err := insertEmbeddedLyricsSeedItemTx(ctx, tx, bundle.Manifest.SeedSHA256, item, availability[item.MusicID], applyStatus); err != nil {
			return result, err
		}
	}
	if result.Inserted+result.PreservedExisting != embeddedlyricsseed.ExpectedCatalogCount {
		return result, errors.New("embedded lyrics editor seed did not account for every catalog item")
	}
	if err := validateEmbeddedLyricsEditorSeedReplayTx(ctx, tx, bundle); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	if result.Inserted > 0 {
		s.NotifyChange()
	}
	return result, nil
}

func validateEmbeddedLyricsEditorSeedCatalogTx(ctx context.Context, tx *sql.Tx, bundle embeddedlyricsseed.Bundle) error {
	rows, err := tx.QueryContext(ctx, `SELECT music_id,title_ja,producer_metadata,lyricist,composer,arranger,
		assetbundle_name,version_hint,lyrics_version,lyrics_evidence_presence_json,vocal_signals_json,
		lyrics_catalog_fingerprint,lyrics_catalog_policy_version
		FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	expectedItems := make(map[int]embeddedlyricsseed.CatalogItem, len(bundle.Manifest.Items))
	for _, item := range bundle.Manifest.Items {
		expectedItems[item.MusicID] = item
	}
	seen := make(map[int]struct{}, len(bundle.Manifest.Items))
	for rows.Next() {
		var musicID int
		var title, producerMetadata, lyricist, composer, arranger, assetbundleName, versionHint, lyricsVersion string
		var presenceJSON, vocalsJSON, fingerprint, policy string
		if err := rows.Scan(&musicID, &title, &producerMetadata, &lyricist, &composer, &arranger,
			&assetbundleName, &versionHint, &lyricsVersion, &presenceJSON, &vocalsJSON, &fingerprint, &policy); err != nil {
			return err
		}
		if expected, ok := expectedItems[musicID]; ok {
			if title != expected.JapaneseTitle || policy != bundle.Manifest.CatalogPolicyVersion ||
				!catalogFingerprintMatchesCurrentEvidence(musicID, title, producerMetadata, lyricist, composer, arranger,
					assetbundleName, versionHint, lyricsVersion, presenceJSON, vocalsJSON, fingerprint, expected.CatalogFingerprint) {
				return fmt.Errorf("%w at music %d", ErrEmbeddedLyricsEditorSeedCatalogMismatch, musicID)
			}
			seen[musicID] = struct{}{}
			continue
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(seen) != len(bundle.Manifest.Items) {
		if len(seen) == 0 {
			return ErrEmbeddedLyricsEditorSeedCatalogNotReady
		}
		return fmt.Errorf("%w: database catalog count or digest differs", ErrEmbeddedLyricsEditorSeedCatalogMismatch)
	}
	musicIDs := make([]int, 0, len(bundle.Manifest.Items))
	catalogDigest := sha256.New()
	for _, item := range bundle.Manifest.Items {
		musicIDs = append(musicIDs, item.MusicID)
		catalogDigest.Write([]byte(fmt.Sprintf("%d\x00%s\n", item.MusicID, item.CatalogFingerprint)))
	}
	if embeddedlyricsseed.MusicIDsSHA256(musicIDs) != bundle.Manifest.MusicIDsSHA256 ||
		hex.EncodeToString(catalogDigest.Sum(nil)) != bundle.Manifest.CatalogFingerprintsSHA256 {
		return fmt.Errorf("%w: database catalog count or digest differs", ErrEmbeddedLyricsEditorSeedCatalogMismatch)
	}
	return nil
}

func catalogFingerprintMatchesCurrentEvidence(musicID int, title, producerMetadata, lyricist, composer, arranger,
	assetbundleName, versionHint, lyricsVersion, presenceJSON, vocalsJSON, actualFingerprint, seedFingerprint string,
) bool {
	if actualFingerprint == seedFingerprint {
		return true
	}
	var presence model.CatalogEvidencePresence
	if err := json.Unmarshal([]byte(presenceJSON), &presence); err != nil {
		return false
	}
	var vocals []model.CatalogVocalSignal
	if err := json.Unmarshal([]byte(vocalsJSON), &vocals); err != nil {
		return false
	}
	evidence := model.CatalogLyricsEvidence{
		PolicyVersion: model.LyricsCatalogIdentityPolicyVersion,
		Title:         title,
		Lyricist:      lyricist,
		Composer:      composer,
		Arranger:      arranger,
		Assetbundle:   assetbundleName,
		VersionHint:   versionHint,
		LyricsVersion: lyricsVersion,
		Vocals:        vocals,
		Presence:      presence,
	}
	computed, err := model.CatalogLyricsEvidenceFingerprint(evidence)
	return err == nil && computed == actualFingerprint && musicID > 0
}

func ensureNoOtherEmbeddedLyricsSeedBatchTx(ctx context.Context, tx *sql.Tx, expectedSeedSHA string) error {
	var seedSHA string
	err := tx.QueryRowContext(ctx, `SELECT seed_sha256 FROM embedded_lyrics_editor_seed_batches
		WHERE seed_sha256<>? ORDER BY created_at,seed_sha256 LIMIT 1`, expectedSeedSHA).Scan(&seedSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("database already contains a different embedded lyrics editor seed %s", seedSHA)
}

func lyricsOwnershipExistsTx(ctx context.Context, tx *sql.Tx, musicID int) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM song_lyrics WHERE music_id=?)+
		(SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=?)+
		(SELECT COUNT(*) FROM song_lyrics_availability_documents WHERE music_id=?)+
		(SELECT COUNT(*) FROM embedded_lyrics_editor_seed_items WHERE music_id=? AND seed_kind='availability' AND apply_status='inserted')`,
		musicID, musicID, musicID, musicID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func insertEmbeddedSourceV3Tx(ctx context.Context, tx *sql.Tx, record embeddedlyricsseed.SourceDocumentRecord,
	artifacts []embeddedlyricsseed.SourceArtifactRecord, contributions []embeddedlyricsseed.SourceContributionRecord,
	verifyDecode bool,
) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_documents
		(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,?,?,?,?,?)`, record.MusicID, record.SchemaVersion, record.ReasonCode, record.DocumentJSON,
		record.DocumentSHA256, record.ManifestBatchSHA256, record.CreatedAt)
	if err != nil {
		return err
	}
	documentID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_artifacts
			(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,
			 page_title,canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,
			 version_reason,index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,
			 raw_wikitext_sha256,artifact_sha256)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, documentID, artifact.Provider, artifact.RenditionKey,
			artifact.Origin, artifact.PageID, artifact.RevisionID, artifact.RevisionTimestamp, artifact.MediaWikiSHA1,
			artifact.PageTitle, artifact.CanonicalRevisionURL, artifact.FetchedAt, artifact.CategoriesJSON, artifact.Section,
			artifact.CompositionRenditionKey, artifact.VersionReason, artifact.IndexEvidenceRefsJSON,
			artifact.FixedIdentityJSON, artifact.FixedIdentitySHA256, artifact.RawByteCount,
			artifact.RawWikitextSHA256, artifact.ArtifactSHA256); err != nil {
			return err
		}
	}
	for _, contribution := range contributions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_component_contributions
			(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, documentID,
			contribution.Component, contribution.RenditionKey, contribution.ContributionSHA256); err != nil {
			return err
		}
	}
	if verifyDecode {
		if _, err := loadLyricsRenditionEditorBundle(tx, record.MusicID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertEmbeddedLegacyLyricsTx(ctx context.Context, tx *sql.Tx, record embeddedlyricsseed.LegacyDocumentRecord,
	lines []embeddedlyricsseed.LegacyLineRecord, segments []embeddedlyricsseed.LegacySegmentRecord,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics
		(music_id,revision,updated_at,updated_by,attribution,translation_credit,proofreading_credit,source_note,
		 source_url,license_note,source_hash,source_page_id,source_revision_id,source_sha1,source_fetched_at,
		 source_fetched_at_rfc3339) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.MusicID, record.Revision,
		record.UpdatedAt, record.UpdatedBy, record.Attribution, record.TranslationCredit, record.ProofreadingCredit,
		record.SourceNote, record.SourceURL, record.LicenseNote, record.SourceHash, record.SourcePageID,
		record.SourceRevisionID, record.SourceSHA1, record.SourceFetchedAt, record.SourceFetchedAtRFC3339); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_lines
			(music_id,line_id,position,japanese,zh_cn,en_us,stanza_break_before) VALUES (?,?,?,?,?,?,?)`,
			line.MusicID, line.LineID, line.Position, line.Japanese, line.Chinese, line.English, line.StanzaBreakBefore); err != nil {
			return err
		}
	}
	for _, segment := range segments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_segments
			(music_id,line_id,position,text,performer_ids_json,ruby_json) VALUES (?,?,?,?,?,?)`, segment.MusicID,
			segment.LineID, segment.Position, segment.Text, segment.PerformerIDsJSON, segment.RubyJSON); err != nil {
			return err
		}
	}
	stored, err := s.loadLyrics(tx, record.MusicID)
	if err != nil {
		return err
	}
	if stored.lyrics.Revision != record.Revision || stored.sourceHash != record.SourceHash || len(stored.lyrics.Lines) != len(lines) {
		return errors.New("embedded legacy lyrics verification differs")
	}
	return nil
}

func insertEmbeddedLyricsSeedItemTx(ctx context.Context, tx *sql.Tx, seedSHA string, item embeddedlyricsseed.CatalogItem,
	availability embeddedlyricsseed.AvailabilityRecord, applyStatus string,
) error {
	availabilitySchemaVersion := 0
	reasonCode, noLyricsReason, documentJSON, documentSHA := "", "", "", ""
	if item.SeedKind == "availability" {
		availabilitySchemaVersion = availability.SchemaVersion
		reasonCode = availability.ReasonCode
		noLyricsReason = availability.NoLyricsReason
		documentJSON = availability.DocumentJSON
		documentSHA = availability.DocumentSHA256
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO embedded_lyrics_editor_seed_items
		(seed_sha256,music_id,japanese_title,catalog_fingerprint,state,seed_kind,apply_status,result_sha256,
		 source_document_sha256,availability_schema_version,reason_code,no_lyrics_reason,
		 availability_document_json,availability_document_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, seedSHA, item.MusicID, item.JapaneseTitle, item.CatalogFingerprint,
		item.State, item.SeedKind, applyStatus, item.ResultSHA256, item.DocumentSHA256, availabilitySchemaVersion,
		reasonCode, noLyricsReason, documentJSON, documentSHA, item.CreatedAt)
	return err
}

func validateEmbeddedLyricsEditorSeedReplayTx(ctx context.Context, tx *sql.Tx, bundle embeddedlyricsseed.Bundle) error {
	rows, err := tx.QueryContext(ctx, `SELECT music_id,japanese_title,catalog_fingerprint,state,seed_kind,apply_status,
		result_sha256,source_document_sha256,availability_schema_version,reason_code,no_lyrics_reason,
		availability_document_json,availability_document_sha256,created_at
		FROM embedded_lyrics_editor_seed_items WHERE seed_sha256=? ORDER BY music_id`, bundle.Manifest.SeedSHA256)
	if err != nil {
		return err
	}
	defer rows.Close()
	documents := make(map[int]embeddedlyricsseed.SourceDocumentRecord, len(bundle.Documents))
	legacy := make(map[int]embeddedlyricsseed.LegacyDocumentRecord, len(bundle.LegacyDocuments))
	availability := make(map[int]embeddedlyricsseed.AvailabilityRecord, len(bundle.Availability))
	for _, record := range bundle.Documents {
		documents[record.MusicID] = record
	}
	for _, record := range bundle.LegacyDocuments {
		legacy[record.MusicID] = record
	}
	for _, record := range bundle.Availability {
		availability[record.MusicID] = record
	}
	index := 0
	for rows.Next() {
		if index >= len(bundle.Manifest.Items) {
			return errors.New("embedded lyrics editor seed replay ledger has extra items")
		}
		expected := bundle.Manifest.Items[index]
		var musicID, availabilitySchemaVersion int
		var title, fingerprint, state, seedKind, applyStatus, resultSHA, sourceDocumentSHA string
		var reasonCode, noLyricsReason, availabilityJSON, availabilitySHA string
		var createdAt int64
		if err := rows.Scan(&musicID, &title, &fingerprint, &state, &seedKind, &applyStatus, &resultSHA,
			&sourceDocumentSHA, &availabilitySchemaVersion, &reasonCode, &noLyricsReason, &availabilityJSON,
			&availabilitySHA, &createdAt); err != nil {
			return err
		}
		if musicID != expected.MusicID || title != expected.JapaneseTitle || fingerprint != expected.CatalogFingerprint ||
			state != expected.State || seedKind != expected.SeedKind || (applyStatus != "inserted" && applyStatus != "preserved_existing") ||
			resultSHA != expected.ResultSHA256 || sourceDocumentSHA != expected.DocumentSHA256 || createdAt != expected.CreatedAt {
			return fmt.Errorf("embedded lyrics editor seed replay ledger item %d changed", expected.MusicID)
		}
		if expected.SeedKind == "availability" {
			record := availability[expected.MusicID]
			if availabilitySchemaVersion != record.SchemaVersion || reasonCode != record.ReasonCode ||
				noLyricsReason != record.NoLyricsReason || availabilityJSON != record.DocumentJSON ||
				availabilitySHA != record.DocumentSHA256 {
				return fmt.Errorf("embedded lyrics editor seed availability ledger item %d changed", expected.MusicID)
			}
		} else if availabilitySchemaVersion != 0 || reasonCode != "" || noLyricsReason != "" || availabilityJSON != "" || availabilitySHA != "" {
			return fmt.Errorf("embedded lyrics editor seed text-bearing ledger item %d has availability data", expected.MusicID)
		}
		if applyStatus == "preserved_existing" {
			owned, err := lyricsOwnershipExistsTx(ctx, tx, expected.MusicID)
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("embedded lyrics editor seed replay preserved item %d lost ownership", expected.MusicID)
			}
		}
		if applyStatus == "inserted" {
			switch expected.SeedKind {
			case "source_v3":
				record, ok := documents[expected.MusicID]
				if !ok {
					return fmt.Errorf("embedded lyrics editor seed replay source-v3 item %d has no document", expected.MusicID)
				}
				if err := validateEmbeddedSourceV3ReplayTx(tx, record, bundle.ArchiveSHA256 != embeddedlyricsseed.ExpectedArchiveSHA256); err != nil {
					return err
				}
			case "legacy":
				record, ok := legacy[expected.MusicID]
				if !ok {
					return fmt.Errorf("embedded lyrics editor seed replay legacy item %d has no document", expected.MusicID)
				}
				if err := validateEmbeddedLegacyReplayTx(tx, record); err != nil {
					return err
				}
			case "availability":
				owned, err := lyricsOwnershipExistsTx(ctx, tx, expected.MusicID)
				if err != nil {
					return err
				}
				if !owned {
					return fmt.Errorf("embedded lyrics editor seed replay availability item %d lost ownership", expected.MusicID)
				}
			}
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if index != len(bundle.Manifest.Items) {
		return errors.New("embedded lyrics editor seed replay ledger is incomplete")
	}
	return nil
}

func validateEmbeddedSourceV3ReplayTx(tx *sql.Tx, record embeddedlyricsseed.SourceDocumentRecord, verifyDecode bool) error {
	var documentSHA, manifestBatchSHA string
	var schemaVersion int
	if err := tx.QueryRow(`SELECT schema_version,document_sha256,manifest_batch_sha256
		FROM song_lyrics_source_documents WHERE music_id=?`, record.MusicID).Scan(
		&schemaVersion, &documentSHA, &manifestBatchSHA); err != nil {
		return fmt.Errorf("embedded lyrics editor seed replay source-v3 item %d: %w", record.MusicID, err)
	}
	if schemaVersion != record.SchemaVersion || documentSHA != record.DocumentSHA256 || manifestBatchSHA != record.ManifestBatchSHA256 {
		return fmt.Errorf("embedded lyrics editor seed replay source-v3 item %d changed", record.MusicID)
	}
	if verifyDecode {
		if _, err := loadLyricsRenditionEditorBundle(tx, record.MusicID); err != nil {
			return fmt.Errorf("embedded lyrics editor seed replay source-v3 item %d: %w", record.MusicID, err)
		}
	}
	return nil
}

func validateEmbeddedLegacyReplayTx(tx *sql.Tx, record embeddedlyricsseed.LegacyDocumentRecord) error {
	var sourceHash string
	var revision int
	if err := tx.QueryRow(`SELECT revision,source_hash FROM song_lyrics WHERE music_id=?`, record.MusicID).Scan(&revision, &sourceHash); err != nil {
		return fmt.Errorf("embedded lyrics editor seed replay legacy item %d: %w", record.MusicID, err)
	}
	if revision < record.Revision || strings.TrimSpace(sourceHash) != record.SourceHash {
		return fmt.Errorf("embedded lyrics editor seed replay legacy item %d changed", record.MusicID)
	}
	return nil
}
