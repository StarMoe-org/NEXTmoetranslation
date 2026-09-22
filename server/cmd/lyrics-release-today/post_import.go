package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsimportreceipt"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

const (
	importReceiptSchemaVersion = lyricsimportreceipt.SchemaVersion
	importReceiptProtocol      = lyricsimportreceipt.CommitProtocol
	importReceiptAuditAction   = lyricsimportreceipt.DatabaseAuditAction
	importStateDigestVersion   = lyricsimportreceipt.StateDigestVersion
)

var importReceiptForbiddenFields = map[string]struct{}{
	"lyrics": {}, "text": {}, "raw": {}, "rawbytes": {}, "translation": {},
	"romaji": {}, "romanization": {}, "romanized": {}, "documentjson": {},
	"fixedidentityjson": {}, "privatereview": {}, "secret": {}, "token": {},
	"password": {}, "credential": {},
}

type releaseImportReceiptArtifact = lyricsimportreceipt.Artifact
type releaseImportReceiptItem = lyricsimportreceipt.Item
type releaseImportReceipt = lyricsimportreceipt.Receipt
type releaseImportReceiptAudit = lyricsimportreceipt.Audit

type postImportResult struct {
	ImportReceiptSHA256 string
	BatchSHA256         string
	ItemCount           int
}

func runCheckPostImport(ctx context.Context, arguments []string) (postImportResult, error) {
	var validationReceiptPath, rootPath, manifestPath, evidencePath, importReceiptPath, databasePath string
	var backupReceiptPath, backupCiphertextPath string
	flags := flag.NewFlagSet("check-post-import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&validationReceiptPath, "validation-receipt", "", "exact fresh-validation receipt")
	flags.StringVar(&rootPath, "root-manifest", "", "validated final root manifest")
	flags.StringVar(&manifestPath, "import-manifest", "", "validated import manifest")
	flags.StringVar(&evidencePath, "import-evidence-receipt", "", "private import evidence receipt")
	flags.StringVar(&importReceiptPath, "import-receipt", "", "durable staged-import receipt")
	flags.StringVar(&databasePath, "database", "", "post-import offline SQLite database")
	flags.StringVar(&backupReceiptPath, "backup-receipt", "", "verified encrypted backup receipt")
	flags.StringVar(&backupCiphertextPath, "backup-ciphertext", "", "verified encrypted backup ciphertext")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return postImportResult{}, errors.New("check-post-import requires explicit release, receipt, backup, and database flags")
	}
	for _, path := range []string{validationReceiptPath, rootPath, manifestPath, evidencePath, importReceiptPath, databasePath, backupReceiptPath, backupCiphertextPath} {
		if !canonicalAbsolutePath(path) {
			return postImportResult{}, errors.New("check-post-import paths must be explicit canonical absolute paths")
		}
	}

	backupResult, backupReceipt, bundle, err := checkEncryptedBackup(
		backupReceiptPath, validationReceiptPath, rootPath, manifestPath, backupCiphertextPath,
	)
	if err != nil {
		return postImportResult{}, err
	}
	_ = backupResult
	evidenceBody, evidenceFileSHA256, err := readPinnedRegular(evidencePath, "import evidence receipt", lyricsstaging.MaxPrivateEvidenceReceiptBytes, 0o600)
	if err != nil {
		return postImportResult{}, err
	}
	evidenceReceipt, err := lyricsstaging.DecodePrivateEvidenceReceipt(evidenceBody)
	if err != nil {
		return postImportResult{}, err
	}
	canonicalEvidence, err := lyricsstaging.MarshalPrivateEvidenceReceipt(evidenceReceipt)
	if err != nil || !bytes.Equal(canonicalEvidence, evidenceBody) {
		return postImportResult{}, errors.New("import evidence receipt is not the canonical producer encoding")
	}
	if err := validateBoundImportEvidence(bundle, evidencePath, evidenceFileSHA256, int64(len(evidenceBody)), evidenceReceipt); err != nil {
		return postImportResult{}, err
	}

	receipt, receiptBody, receiptSHA256, err := loadBoundReleaseImportReceipt(importReceiptPath, bundle)
	if err != nil {
		return postImportResult{}, err
	}
	if err := validateReleaseImportReceipt(
		receipt, importReceiptPath, databasePath, evidencePath, evidenceReceipt, bundle, backupReceipt,
	); err != nil {
		return postImportResult{}, err
	}

	database, err := openReadOnlySQLite(ctx, databasePath, "post-import database")
	if err != nil {
		return postImportResult{}, err
	}
	defer database.close()
	if err := verifySQLiteIntegrity(ctx, database.db); err != nil {
		return postImportResult{}, err
	}
	var userVersion int
	if err := database.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil || userVersion < 23 {
		return postImportResult{}, errors.New("post-import database schema is older than the required lyrics provenance schema")
	}
	if err := verifyImportAudit(ctx, database.db, receiptBody, receiptSHA256, importReceiptPath); err != nil {
		return postImportResult{}, err
	}
	if err := verifyImportedManifest(ctx, database.db, bundle.Bindings.Manifest, receipt); err != nil {
		return postImportResult{}, err
	}
	if err := database.verifyUnchanged(); err != nil {
		return postImportResult{}, err
	}
	if err := rejectSQLiteSidecars(databasePath, "post-import database"); err != nil {
		return postImportResult{}, err
	}
	return postImportResult{ImportReceiptSHA256: receiptSHA256, BatchSHA256: receipt.BatchSHA256, ItemCount: len(receipt.Items)}, nil
}

func loadBoundReleaseImportReceipt(
	path string,
	bundle validatedReleaseBundle,
) (releaseImportReceipt, []byte, string, error) {
	body, fileSHA256, err := readPinnedRegular(path, "durable import receipt", maxReceiptBytes, 0o600)
	if err != nil {
		return releaseImportReceipt{}, nil, "", err
	}
	if err := rejectJSONKeys(body, importReceiptForbiddenFields, "durable import receipt"); err != nil {
		return releaseImportReceipt{}, nil, "", err
	}
	receipt, err := lyricsimportreceipt.DecodeCanonical(body)
	if err != nil {
		return releaseImportReceipt{}, nil, "", err
	}
	canonical, err := marshalBoundReleaseImportReceipt(receipt, path, bundle)
	if err != nil {
		return releaseImportReceipt{}, nil, "", err
	}
	if !bytes.Equal(canonical, body) {
		return releaseImportReceipt{}, nil, "", errors.New("durable import receipt is not the canonical producer encoding")
	}
	return receipt, body, fileSHA256, nil
}

func marshalBoundReleaseImportReceipt(
	receipt releaseImportReceipt,
	receiptPath string,
	bundle validatedReleaseBundle,
) ([]byte, error) {
	if err := validateReleaseImportReceiptBundleBinding(receipt, receiptPath, bundle); err != nil {
		return nil, err
	}
	return marshalReleaseImportReceipt(receipt)
}

func marshalReleaseImportReceipt(receipt releaseImportReceipt) ([]byte, error) {
	return lyricsimportreceipt.MarshalCanonical(receipt)
}

func importReceiptBindingForBundle(receiptPath string, bundle validatedReleaseBundle) lyricsimportreceipt.Binding {
	return lyricsimportreceipt.Binding{
		ValidationReceiptSHA256: bundle.Validation.ReceiptSHA256,
		RootManifestSHA256:      bundle.Bindings.RootFileSHA256,
		RootID:                  bundle.Bindings.Root.RootID,
		RootSHA256:              bundle.Bindings.Root.RootSHA256,
		ManifestSchemaVersion:   bundle.Bindings.Manifest.SchemaVersion,
		ManifestSHA256:          bundle.Bindings.ManifestFileSHA256,
		BatchSHA256:             bundle.Bindings.Manifest.BatchSHA256,
		EvidenceReceiptPath:     bundle.Validation.ImportEvidence.File.Path,
		EvidenceReceiptSHA256:   bundle.Validation.ImportEvidence.ReceiptSHA256,
		ReceiptPath:             receiptPath,
	}
}

func validateReleaseImportReceiptBundleBinding(
	receipt releaseImportReceipt,
	receiptPath string,
	bundle validatedReleaseBundle,
) error {
	bindings := bundle.Bindings
	if err := lyricsimportreceipt.ValidateBound(receipt, importReceiptBindingForBundle(receiptPath, bundle)); err != nil ||
		receipt.RootManifestSHA256 != bundle.Validation.RootManifest.File.SHA256 ||
		receipt.RootID != bundle.Validation.RootManifest.RootID ||
		receipt.RootSHA256 != bundle.Validation.RootManifest.RootSHA256 ||
		len(receipt.Items) != releaseCatalogTargetCount {
		return errors.New("durable import receipt does not match the exact validated release bundle")
	}
	return validateReleaseImportReceiptItems(receipt, bindings.Manifest)
}

func validateReleaseImportReceipt(
	receipt releaseImportReceipt,
	receiptPath, databasePath, evidencePath string,
	evidence lyricsstaging.PrivateEvidenceReceipt,
	bundle validatedReleaseBundle,
	backup encryptedBackupReceipt,
) error {
	if err := validateReleaseImportReceiptBundleBinding(receipt, receiptPath, bundle); err != nil {
		return err
	}
	if receipt.EvidenceReceiptPath != evidencePath || receipt.EvidenceReceiptSHA256 != evidence.ReceiptSHA256 ||
		receipt.BackupSHA256 != backup.PlaintextDatabaseSHA256 || receipt.StateDigestVersion != importStateDigestVersion ||
		receipt.BackupStateSHA256 != backup.PlaintextDatabaseStateSHA256 ||
		receipt.PreImportDatabaseStateSHA256 != backup.PlaintextDatabaseStateSHA256 ||
		!lowerSHA256Pattern.MatchString(receipt.PreImportDatabaseSHA256) ||
		receipt.DatabasePath != databasePath || !canonicalAbsolutePath(receipt.RecoveryDatabasePath) {
		return errors.New("durable import receipt does not match the exact release, backup, and database inputs")
	}
	return nil
}

func validateReleaseImportReceiptItems(receipt releaseImportReceipt, manifest lyricsstaging.Manifest) error {
	changed := 0
	for index, item := range receipt.Items {
		draft := manifest.Items[index]
		if item.MusicID != draft.MusicID || item.Revision <= 0 || item.DocumentSHA256 != draft.DocumentSHA256 ||
			item.FullTextRenditionKey != draft.Document.Provenance.FullText.RenditionKey ||
			item.SourceFetchedAt != fullIdentityFetchedAt(draft.Document) || len(item.Artifacts) != len(draft.Artifacts) {
			return fmt.Errorf("durable import receipt item %d drifted from the release manifest", index)
		}
		for artifactIndex, artifact := range item.Artifacts {
			staged := draft.Artifacts[artifactIndex]
			if artifact.RenditionKey != staged.Identity.RenditionKey || artifact.ArtifactSHA256 != staged.ArtifactSHA256 {
				return fmt.Errorf("durable import receipt music %d artifact drifted from the release manifest", item.MusicID)
			}
		}
		if item.Changed {
			changed++
		}
	}
	if changed != receipt.ImportedCount {
		return errors.New("durable import receipt changed counters do not match its item set")
	}
	return nil
}

func fullIdentityFetchedAt(document model.LyricsSourceDocument) string {
	for _, identity := range document.FixedIdentities {
		if identity.RenditionKey == document.Provenance.FullText.RenditionKey {
			return identity.FetchedAt
		}
	}
	return ""
}

func verifyImportAudit(ctx context.Context, database *sql.DB, receiptBody []byte, receiptSHA256, receiptPath string) error {
	rows, err := database.QueryContext(ctx, `SELECT detail FROM audit_log WHERE action=? ORDER BY id`, importReceiptAuditAction)
	if err != nil {
		return fmt.Errorf("query transactional import receipt audit: %w", err)
	}
	defer rows.Close()
	matches := 0
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return err
		}
		var audit releaseImportReceiptAudit
		if err := decodeStrictJSON([]byte(detail), &audit, "transactional import receipt audit"); err != nil {
			return err
		}
		if audit.SchemaVersion == 1 && audit.ReceiptPath == receiptPath && audit.ReceiptSHA256 == receiptSHA256 &&
			audit.ReceiptJSON == string(receiptBody) {
			matches++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if matches != 1 {
		return errors.New("post-import database does not contain exactly one matching transactional import receipt audit")
	}
	return nil
}

func verifyImportedManifest(ctx context.Context, database *sql.DB, manifest lyricsstaging.Manifest, receipt releaseImportReceipt) error {
	for index, draft := range manifest.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := lyricscontract.ValidatePersistedPerformerMetadata(draft.Document.Full); err != nil {
			return fmt.Errorf("imported music %d has unsafe performer metadata", draft.MusicID)
		}
		documentJSON, err := json.Marshal(draft.Document)
		if err != nil {
			return err
		}
		var documentID int64
		var schemaVersion int
		var reasonCode, storedJSON, storedSHA, storedBatch string
		if err := database.QueryRowContext(ctx, `SELECT document_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256
			FROM song_lyrics_source_documents WHERE music_id=?`, draft.MusicID).
			Scan(&documentID, &schemaVersion, &reasonCode, &storedJSON, &storedSHA, &storedBatch); err != nil {
			return fmt.Errorf("query imported music %d source document: %w", draft.MusicID, err)
		}
		if schemaVersion != draft.Document.SchemaVersion || reasonCode != string(draft.Document.ReasonCode) ||
			storedJSON != string(documentJSON) || storedSHA != draft.DocumentSHA256 ||
			receipt.Items[index].Changed && storedBatch != manifest.BatchSHA256 {
			return fmt.Errorf("imported music %d source document drifted from the release manifest", draft.MusicID)
		}
		decoded, err := model.DecodeLyricsSourceDocument([]byte(storedJSON))
		if err != nil || !reflect.DeepEqual(decoded, draft.Document) {
			return fmt.Errorf("imported music %d source document is not the exact closed document", draft.MusicID)
		}
		var revision int
		if err := database.QueryRowContext(ctx, `SELECT revision FROM song_lyrics WHERE music_id=?`, draft.MusicID).Scan(&revision); err != nil ||
			revision != receipt.Items[index].Revision {
			return fmt.Errorf("imported music %d revision does not match the durable receipt", draft.MusicID)
		}
		if err := verifyImportedArtifacts(ctx, database, documentID, draft); err != nil {
			return err
		}
		if err := verifyImportedContributions(ctx, database, documentID, draft.Document, draft.DocumentSHA256); err != nil {
			return err
		}
	}
	return nil
}

func verifyImportedArtifacts(ctx context.Context, database *sql.DB, documentID int64, draft lyricsstaging.Draft) error {
	var count int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM song_lyrics_source_artifacts WHERE document_id=?`, documentID).Scan(&count); err != nil || count != len(draft.Artifacts) {
		return fmt.Errorf("imported music %d artifact count does not match the release manifest", draft.MusicID)
	}
	for _, artifact := range draft.Artifacts {
		identityJSON, err := json.Marshal(artifact.Identity)
		if err != nil {
			return err
		}
		identityDigest := sha256.Sum256(identityJSON)
		var storedIdentityJSON, storedIdentitySHA, rawSHA, artifactSHA string
		var rawCount int
		if err := database.QueryRowContext(ctx, `SELECT fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256
			FROM song_lyrics_source_artifacts WHERE document_id=? AND rendition_key=?`, documentID, artifact.Identity.RenditionKey).
			Scan(&storedIdentityJSON, &storedIdentitySHA, &rawCount, &rawSHA, &artifactSHA); err != nil {
			return fmt.Errorf("query imported music %d artifact %q: %w", draft.MusicID, artifact.Identity.RenditionKey, err)
		}
		if storedIdentityJSON != string(identityJSON) || storedIdentitySHA != hex.EncodeToString(identityDigest[:]) ||
			rawCount != artifact.RawWikitextByteCount || rawSHA != artifact.RawWikitextSHA256 || artifactSHA != artifact.ArtifactSHA256 {
			return fmt.Errorf("imported music %d artifact %q drifted from the release manifest", draft.MusicID, artifact.Identity.RenditionKey)
		}
		rows, err := database.QueryContext(ctx, `SELECT position,provider,evidence_id,sha256 FROM song_lyrics_source_artifact_index_evidence
			WHERE document_id=? AND rendition_key=? ORDER BY position`, documentID, artifact.Identity.RenditionKey)
		if err != nil {
			return err
		}
		position := 0
		for rows.Next() {
			var storedPosition int
			var provider model.LyricsSourceProvider
			var evidenceID, evidenceSHA string
			if err := rows.Scan(&storedPosition, &provider, &evidenceID, &evidenceSHA); err != nil {
				rows.Close()
				return err
			}
			if position >= len(artifact.Identity.IndexEvidenceRefs) || storedPosition != position || provider != artifact.Identity.Provider ||
				evidenceID != artifact.Identity.IndexEvidenceRefs[position].EvidenceID ||
				evidenceSHA != artifact.Identity.IndexEvidenceRefs[position].SHA256 {
				rows.Close()
				return fmt.Errorf("imported music %d artifact evidence links drifted", draft.MusicID)
			}
			position++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if position != len(artifact.Identity.IndexEvidenceRefs) {
			return fmt.Errorf("imported music %d artifact evidence link count drifted", draft.MusicID)
		}
	}
	return nil
}

func verifyImportedContributions(ctx context.Context, database *sql.DB, documentID int64, document model.LyricsSourceDocument, documentSHA string) error {
	expected := releaseComponentRefs(document)
	rows, err := database.QueryContext(ctx, `SELECT component,rendition_key,contribution_sha256
		FROM song_lyrics_component_contributions WHERE document_id=? ORDER BY component`, documentID)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := make(map[string]struct{}, len(expected))
	for rows.Next() {
		var component, renditionKey, digest string
		if err := rows.Scan(&component, &renditionKey, &digest); err != nil {
			return err
		}
		expectedKey, found := expected[component]
		calculated := sha256.Sum256([]byte(documentSHA + "\x00" + component + "\x00" + renditionKey))
		if !found || expectedKey != renditionKey || digest != hex.EncodeToString(calculated[:]) {
			return errors.New("imported component contribution drifted from the release manifest")
		}
		seen[component] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("imported component contribution set is incomplete")
	}
	return nil
}

func releaseComponentRefs(document model.LyricsSourceDocument) map[string]string {
	result := map[string]string{
		"full_text":        document.Provenance.FullText.RenditionKey,
		"version_evidence": document.Provenance.VersionEvidence.RenditionKey,
	}
	if document.Provenance.PerformerSegmentation != nil {
		result["performer_segmentation"] = document.Provenance.PerformerSegmentation.RenditionKey
	}
	if document.Provenance.GameProjection != nil {
		result["game_projection"] = document.Provenance.GameProjection.RenditionKey
	}
	if document.Provenance.Ruby != nil {
		result["ruby"] = document.Provenance.Ruby.RenditionKey
	}
	return result
}
