package offlineimport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsrecoveryimport"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

const (
	// encoded by the existing recovery/staging input contracts. Schema v28 adds
	// the editor-seed ledger, v29 adds peer-side translation storage, v30 adds
	// lazily materialized translation editions, v31 adds yjs collaboration,
	// v32-v34 adds multi-edition translations for song 682, and v35 adds the
	// public withdrawal markers. None changes those inputs or their catalog
	// identity, so reviewed imports may run on any contiguous v27-v35 database.
	lyricsRecoveryImportRuntimeSchema          = 27
	lyricsImportMaximumCompatibleRuntimeSchema = 35
)

var ErrLyricsRecoveryImportDrift = errors.New("lyrics recovery import no longer matches its catalog, root, or evidence pack")

// RecoveryLyricsImportItem is the transaction result for one compact-root song.
// Changed is false only for an exact replay of an already committed batch.
type RecoveryLyricsImportItem struct {
	MusicID                    int
	State                      lyricscontract.CoverageState
	Changed                    bool
	Revision                   int
	DocumentSHA256             string
	AvailabilityDocumentSHA256 string
}

// RecoveryLyricsImportCommitHook runs after every durable row and the ordinary
// recovery audit have been written or verified. The timestamp is the immutable
// batch created_at value, including on exact replay. If the hook returns nil,
// SQLite Commit is the next database call. This is the receipt boundary for the
// all-root importer.
type RecoveryLyricsImportCommitHook func(*sql.Tx, []RecoveryLyricsImportItem, int64) error

func ImportRecoveryLyricsManifest(
	ctx context.Context,
	lyricsStore *store.Store,
	root lyricsrootmanifest.Manifest,
	manifest lyricsrecoveryimport.Manifest,
	receipt lyricsrecoveryimport.EvidenceReceipt,
	resolver *lyricsevidencepack.Resolver,
	actor string,
) ([]RecoveryLyricsImportItem, error) {
	results, _, err := ImportRecoveryLyricsManifestWithCommitHook(ctx, lyricsStore, root, manifest, receipt, resolver, actor, nil)
	return results, err
}

// ImportRecoveryLyricsManifestWithCommitHook atomically imports every compact-
// root state. It never publishes. Complete items create the existing editable
// Full draft and source document v2; all recovery artifacts/evidence use the
// additive v24 graph plus the v25 source-document schema migration. Non-Full
// states create only availability-document v1.
func ImportRecoveryLyricsManifestWithCommitHook(
	ctx context.Context,
	lyricsStore *store.Store,
	root lyricsrootmanifest.Manifest,
	manifest lyricsrecoveryimport.Manifest,
	receipt lyricsrecoveryimport.EvidenceReceipt,
	resolver *lyricsevidencepack.Resolver,
	actor string,
	beforeCommit RecoveryLyricsImportCommitHook,
) ([]RecoveryLyricsImportItem, bool, error) {
	if ctx == nil || resolver == nil {
		return nil, false, errors.New("recovery lyrics import requires context and an exact evidence resolver")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > store.MaxLyricsImportActorBytes || !utf8.ValidString(actor) || strings.ContainsAny(actor, "\r\n") {
		return nil, false, store.ErrLyricsSourceInvalidRequest
	}
	pack := resolver.Manifest()
	if err := lyricsrecoveryimport.ValidateEvidenceReceiptAgainst(receipt, root, manifest, pack); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrLyricsRecoveryImportDrift, err)
	}
	if err := resolver.ValidateSelected(receipt.Evidence); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrLyricsRecoveryImportDrift, err)
	}

	session, err := lyricsStore.BeginOfflineLyricsImport(ctx, recoveryManifestMusicIDs(manifest))
	if err != nil {
		return nil, false, err
	}
	defer session.Close()
	tx := session.Tx()
	if err := validateRecoveryImportRuntimeSchema(ctx, tx); err != nil {
		return nil, false, err
	}
	if err := validatePeerTranslationRuntimeSchema(ctx, tx, recoveryManifestHasPeerTranslations(manifest), "recovery lyrics import"); err != nil {
		return nil, false, err
	}
	catalog, err := loadStagedImportCatalog(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	catalogByMusicID, targets, performers, err := validateRecoveryImportCatalog(tx, catalog, manifest)
	if err != nil {
		return nil, false, err
	}

	preparedLyrics := make(map[int]model.SongLyrics, manifest.Root.Coverage.Complete)
	preparedEditable := make(map[int]bool, manifest.Root.Coverage.Complete)
	availabilityJSON := make(map[int]string, len(manifest.Items)-manifest.Root.Coverage.Complete)
	for _, item := range manifest.Items {
		catalogItem := catalogByMusicID[item.MusicID]
		target := targets[item.MusicID]
		if !recoveryImportCatalogTargetMatches(item, target) {
			return nil, false, fmt.Errorf("%w: music %d catalog target or associations changed", ErrLyricsRecoveryImportDrift, item.MusicID)
		}
		if item.Draft != nil && item.Draft.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			if err := store.ValidatePersistedLyricsSourceDocument(item.Draft.Document); err != nil {
				return nil, false, fmt.Errorf("%w: music %d source v3: %v", ErrLyricsRecoveryImportDrift, item.MusicID, err)
			}
			// Source-v3 is always plural-editor owned. Never materialize a
			// legacy SongLyrics projection alongside the rendition localizations:
			// the two stores have independent revisions and would silently drift.
			continue
		}
		if item.State == lyricscontract.CoverageComplete {
			lyrics, err := stagedManifestLyricsDraft(*item.Draft, catalogItem.vocals, performers)
			if err != nil {
				return nil, false, err
			}
			if err := store.ValidateImportedLyrics(lyrics, performers); err != nil {
				return nil, false, err
			}
			preparedLyrics[item.MusicID] = lyrics
			preparedEditable[item.MusicID] = true
			continue
		}
		if err := validateStoreLyricsAvailabilityDocument(*item.Availability); err != nil {
			return nil, false, fmt.Errorf("%w: music %d availability: %v", ErrLyricsRecoveryImportDrift, item.MusicID, err)
		}
		body, err := json.Marshal(item.Availability)
		if err != nil {
			return nil, false, err
		}
		availabilityJSON[item.MusicID] = string(body)
	}

	batchExists, err := recoveryImportBatchExists(ctx, tx, manifest.BatchSHA256)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().Unix()
	if batchExists {
		if err := verifyRecoveryImportBatchTx(ctx, tx, root, manifest, receipt, actor); err != nil {
			return nil, false, err
		}
	} else if err := insertRecoveryImportBatchTx(ctx, tx, root, manifest, receipt, actor, now); err != nil {
		return nil, false, err
	}

	for _, ref := range receipt.Evidence {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		evidence, err := resolver.HydrateExact(ref)
		if err != nil {
			return nil, false, fmt.Errorf("%w: hydrate evidence %s: %v", ErrLyricsRecoveryImportDrift, ref.EvidenceID, err)
		}
		if err := insertOrVerifyRecoveryEvidenceTx(ctx, tx, ref, evidence, now); err != nil {
			return nil, false, err
		}
	}

	results := make([]RecoveryLyricsImportItem, len(manifest.Items))
	for index, item := range manifest.Items {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if batchExists {
			if err := verifyRecoveryImportItemTx(ctx, tx, manifest.BatchSHA256, item); err != nil {
				return nil, false, err
			}
		} else if err := insertRecoveryImportItemTx(ctx, tx, manifest.BatchSHA256, item, now); err != nil {
			return nil, false, err
		}

		result := RecoveryLyricsImportItem{
			MusicID: item.MusicID, State: item.State, Changed: !batchExists,
			AvailabilityDocumentSHA256: item.AvailabilityDocumentSHA256,
		}
		if item.Draft != nil {
			result.DocumentSHA256 = item.Draft.DocumentSHA256
		}
		if item.Draft != nil && item.Draft.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			if err := ensureRecoveryItemOwnsNoEditableLyrics(ctx, tx, item.MusicID); err != nil {
				return nil, false, err
			}
			if batchExists {
				if err := verifyRecoveryV3DraftItemTx(ctx, tx, manifest.BatchSHA256, item); err != nil {
					return nil, false, err
				}
			} else if err := insertRecoveryV3DraftItemTx(ctx, tx, manifest.BatchSHA256, item, now); err != nil {
				return nil, false, err
			}
			// Plural source-v3 editor documents start at revision 1 in their
			// localization envelope, but never own a legacy SongLyrics revision.
			result.Revision = 0
		} else if item.State == lyricscontract.CoverageComplete {
			if batchExists {
				revision, err := verifyRecoveryCompleteItemTx(ctx, tx, manifest.BatchSHA256, item, preparedLyrics[item.MusicID])
				if err != nil {
					return nil, false, err
				}
				result.Revision = revision
			} else {
				revision, err := insertRecoveryCompleteItemTx(ctx, tx, manifest.BatchSHA256, item, preparedLyrics[item.MusicID], actor, now)
				if err != nil {
					return nil, false, err
				}
				result.Revision = revision
			}
		} else {
			if err := ensureRecoveryItemOwnsNoEditableLyrics(ctx, tx, item.MusicID); err != nil {
				return nil, false, err
			}
			if batchExists {
				if err := verifyRecoveryAvailabilityItemTx(ctx, tx, manifest.BatchSHA256, item, availabilityJSON[item.MusicID]); err != nil {
					return nil, false, err
				}
			} else if err := insertRecoveryAvailabilityItemTx(ctx, tx, manifest.BatchSHA256, item, availabilityJSON[item.MusicID], now); err != nil {
				return nil, false, err
			}
		}
		if batchExists {
			if err := verifyRecoveryProvenanceGraphTx(ctx, tx, manifest.BatchSHA256, item); err != nil {
				return nil, false, err
			}
		} else if err := insertRecoveryProvenanceGraphTx(ctx, tx, manifest.BatchSHA256, item, now); err != nil {
			return nil, false, err
		}
		results[index] = result
	}
	if batchExists {
		var itemCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_import_items WHERE batch_sha256=?`, manifest.BatchSHA256).Scan(&itemCount); err != nil {
			return nil, false, err
		}
		if itemCount != len(manifest.Items) {
			return nil, false, fmt.Errorf("%w: committed recovery batch item count changed", ErrLyricsRecoveryImportDrift)
		}
	}
	if !batchExists {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.import_recovery',?)`,
			now, actor, fmt.Sprintf("rootId=%s rootSha256=%s batchSha256=%s items=%d", root.RootID, root.RootSHA256, manifest.BatchSHA256, len(manifest.Items))); err != nil {
			return nil, false, err
		}
	}
	var batchCreatedAt int64
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM lyrics_recovery_import_batches WHERE batch_sha256=?`, manifest.BatchSHA256).
		Scan(&batchCreatedAt); err != nil {
		return nil, false, err
	}
	if beforeCommit != nil {
		if err := beforeCommit(tx, results, batchCreatedAt); err != nil {
			return nil, false, err
		}
	}
	if err := session.Commit(); err != nil {
		return nil, true, err
	}
	if !batchExists {
		session.NotifyChange()
	}
	return results, true, nil
}

func recoveryManifestHasPeerTranslations(manifest lyricsrecoveryimport.Manifest) bool {
	for _, item := range manifest.Items {
		if item.Draft == nil {
			continue
		}
		for _, rendition := range item.Draft.RenditionTranslations {
			if len(rendition.PeerTranslations) != 0 {
				return true
			}
		}
	}
	return false
}

func validatePeerTranslationRuntimeSchema(ctx context.Context, tx *sql.Tx, required bool, operation string) error {
	return db.ValidateLyricsPeerTranslationSchema(ctx, tx, required, operation)
}

func validateRecoveryImportRuntimeSchema(ctx context.Context, tx *sql.Tx) error {
	var minimumVersion, maximumVersion, count int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(version),0),COALESCE(MAX(version),0),COUNT(*) FROM schema_migrations`).
		Scan(&minimumVersion, &maximumVersion, &count); err != nil {
		return fmt.Errorf("read recovery-import runtime schema: %w", err)
	}
	if minimumVersion != 1 || count != maximumVersion || maximumVersion < lyricsRecoveryImportRuntimeSchema ||
		maximumVersion > lyricsImportMaximumCompatibleRuntimeSchema {
		return fmt.Errorf("recovery lyrics import requires a contiguous schema-v%d through schema-v%d runtime",
			lyricsRecoveryImportRuntimeSchema, lyricsImportMaximumCompatibleRuntimeSchema)
	}
	if maximumVersion >= db.LyricsPeerTranslationSchemaVersion {
		if err := db.ValidateLyricsPeerTranslationSchema(ctx, tx, true, "recovery lyrics import"); err != nil {
			return err
		}
	}
	if maximumVersion >= db.LyricsTranslationEditionSchemaVersion {
		if err := db.ValidateLyricsTranslationEditionSchema(ctx, tx, true, "recovery lyrics import"); err != nil {
			return err
		}
	}
	return nil
}

func validateRecoveryImportCatalog(
	tx *sql.Tx,
	catalog []stagedImportCatalogItem,
	manifest lyricsrecoveryimport.Manifest,
) (map[int]stagedImportCatalogItem, map[int]model.CatalogLyricsTarget, store.CatalogPerformerAliases, error) {
	if len(catalog) != manifest.Root.CatalogCount {
		return nil, nil, store.CatalogPerformerAliases{}, fmt.Errorf("%w: manifest catalog count %d does not match current catalog count %d",
			ErrLyricsRecoveryImportDrift, manifest.Root.CatalogCount, len(catalog))
	}
	catalogByMusicID := make(map[int]stagedImportCatalogItem, len(catalog))
	grouping := make([]model.CatalogLyricsGroupingRecord, 0, len(catalog))
	for _, item := range catalog {
		catalogByMusicID[item.musicID] = item
		grouping = append(grouping, model.CatalogLyricsGroupingRecord{
			MusicID: item.musicID, Fingerprint: item.catalogFingerprint, Evidence: item.evidence,
		})
	}
	for _, item := range manifest.Items {
		catalogItem, exists := catalogByMusicID[item.MusicID]
		if !exists || catalogItem.japaneseTitle != item.JapaneseTitle || catalogItem.catalogFingerprint != item.CatalogFingerprint {
			return nil, nil, store.CatalogPerformerAliases{}, fmt.Errorf("%w: music %d catalog identity changed", ErrLyricsRecoveryImportDrift, item.MusicID)
		}
	}
	targets := model.ClassifyCatalogLyricsTargets(grouping)
	targetByMusicID := make(map[int]model.CatalogLyricsTarget, len(targets))
	for _, target := range targets {
		targetByMusicID[target.MusicID] = target
	}
	if len(targetByMusicID) != len(catalogByMusicID) {
		return nil, nil, store.CatalogPerformerAliases{}, fmt.Errorf("%w: current catalog classification is incomplete", ErrLyricsRecoveryImportDrift)
	}
	performers, err := store.LoadCatalogPerformerAliases(tx)
	if err != nil {
		return nil, nil, store.CatalogPerformerAliases{}, err
	}
	return catalogByMusicID, targetByMusicID, performers, nil
}

func recoveryImportCatalogTargetMatches(item lyricsrecoveryimport.Item, target model.CatalogLyricsTarget) bool {
	if target.MusicID != item.MusicID || target.CatalogFingerprint != item.CatalogFingerprint {
		return false
	}
	switch item.State {
	case lyricscontract.CoverageSatisfiedNoLyrics:
		// Catalog review targets deliberately have no elected Full anchor. The
		// recovery item remains self-targeted, and only the closed instrumental
		// reason may satisfy the reviewed no-lyrics state.
		return target.Disposition == model.LyricsCatalogTargetReview &&
			target.ReasonCode == "instrumental_no_vocals" &&
			target.TargetMusicID == 0 && len(target.AssociationMusicIDs) == 0 &&
			item.TargetMusicID == item.MusicID && len(item.AssociationMusicIDs) == 0
	case lyricscontract.CoverageComplete, lyricscontract.CoverageGameOnly,
		lyricscontract.CoverageAmbiguous, lyricscontract.CoverageMissing,
		lyricscontract.CoverageIncomplete, lyricscontract.CoverageFailed:
		return target.Disposition == model.LyricsCatalogTargetFullTarget &&
			target.TargetMusicID == item.TargetMusicID &&
			sameStagedAssociationIDs(target.AssociationMusicIDs, item.AssociationMusicIDs)
	default:
		return false
	}
}

func recoveryImportBatchExists(ctx context.Context, tx *sql.Tx, batchSHA256 string) (bool, error) {
	var found string
	if err := tx.QueryRowContext(ctx, `SELECT batch_sha256 FROM lyrics_recovery_import_batches WHERE batch_sha256=?`, batchSHA256).Scan(&found); err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return found == batchSHA256, nil
}

func recoveryBatchValues(root lyricsrootmanifest.Manifest, manifest lyricsrecoveryimport.Manifest,
	receipt lyricsrecoveryimport.EvidenceReceipt,
) (string, []any, error) {
	coverageJSON, err := json.Marshal(manifest.Root.Coverage)
	if err != nil {
		return "", nil, err
	}
	return string(coverageJSON), []any{
		manifest.BatchSHA256, manifest.SchemaVersion, root.SchemaVersion, root.RootID, root.RootSHA256,
		manifest.Root.CatalogCount, manifest.Root.MusicIDsSHA256, string(coverageJSON), receipt.ReceiptSHA256,
		receipt.PackSHA256, receipt.SelectionSHA256, receipt.EvidenceCount, receipt.ShardCount,
		receipt.RawByteCount, receipt.EncodedByteCount,
	}, nil
}

func insertRecoveryImportBatchTx(ctx context.Context, tx *sql.Tx, root lyricsrootmanifest.Manifest,
	manifest lyricsrecoveryimport.Manifest, receipt lyricsrecoveryimport.EvidenceReceipt, actor string, now int64,
) error {
	_, values, err := recoveryBatchValues(root, manifest, receipt)
	if err != nil {
		return err
	}
	values = append(values, actor, now)
	_, err = tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_import_batches
		(batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,music_ids_sha256,
		 coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,shard_count,
		 raw_byte_count,encoded_byte_count,actor,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, values...)
	return err
}

func verifyRecoveryImportBatchTx(ctx context.Context, tx *sql.Tx, root lyricsrootmanifest.Manifest,
	manifest lyricsrecoveryimport.Manifest, receipt lyricsrecoveryimport.EvidenceReceipt, actor string,
) error {
	coverageJSON, values, err := recoveryBatchValues(root, manifest, receipt)
	if err != nil {
		return err
	}
	var stored struct {
		batch, rootID, rootSHA, musicIDs, coverage, evidenceReceipt, pack, selection, actor string
		schema, rootSchema, catalogCount, evidenceCount, shardCount                         int
		rawBytes, encodedBytes, createdAt                                                   int64
	}
	err = tx.QueryRowContext(ctx, `SELECT batch_sha256,schema_version,root_schema_version,root_id,root_sha256,
		catalog_count,music_ids_sha256,coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,
		evidence_count,shard_count,raw_byte_count,encoded_byte_count,actor,created_at
		FROM lyrics_recovery_import_batches WHERE batch_sha256=?`, manifest.BatchSHA256).Scan(
		&stored.batch, &stored.schema, &stored.rootSchema, &stored.rootID, &stored.rootSHA, &stored.catalogCount,
		&stored.musicIDs, &stored.coverage, &stored.evidenceReceipt, &stored.pack, &stored.selection,
		&stored.evidenceCount, &stored.shardCount, &stored.rawBytes, &stored.encodedBytes, &stored.actor, &stored.createdAt)
	if err != nil {
		return err
	}
	_ = values
	if stored.batch != manifest.BatchSHA256 || stored.schema != manifest.SchemaVersion || stored.rootSchema != root.SchemaVersion ||
		stored.rootID != root.RootID || stored.rootSHA != root.RootSHA256 || stored.catalogCount != manifest.Root.CatalogCount ||
		stored.musicIDs != manifest.Root.MusicIDsSHA256 || stored.coverage != coverageJSON ||
		stored.evidenceReceipt != receipt.ReceiptSHA256 || stored.pack != receipt.PackSHA256 ||
		stored.selection != receipt.SelectionSHA256 || stored.evidenceCount != receipt.EvidenceCount ||
		stored.shardCount != receipt.ShardCount || stored.rawBytes != receipt.RawByteCount ||
		stored.encodedBytes != receipt.EncodedByteCount || stored.actor != actor || stored.createdAt <= 0 {
		return fmt.Errorf("%w: committed recovery batch binding changed", ErrLyricsRecoveryImportDrift)
	}
	return nil
}

func insertRecoveryImportItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
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

func verifyRecoveryImportItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item,
) error {
	associationsJSON, err := json.Marshal(item.AssociationMusicIDs)
	if err != nil {
		return err
	}
	draftSHA, documentSHA := "", ""
	if item.Draft != nil {
		draftSHA, documentSHA = item.Draft.DraftSHA256, item.Draft.DocumentSHA256
	}
	var count int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_import_items WHERE batch_sha256=? AND music_id=? AND
		japanese_title=? AND catalog_fingerprint=? AND target_music_id=? AND association_music_ids_json=? AND state=? AND
		result_sha256=? AND draft_sha256=? AND document_sha256=? AND availability_document_sha256=? AND created_at>0`,
		batchSHA256, item.MusicID, item.JapaneseTitle, item.CatalogFingerprint, item.TargetMusicID,
		string(associationsJSON), item.State, item.ResultSHA256, draftSHA, documentSHA, item.AvailabilityDocumentSHA256).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: committed recovery item %d changed", ErrLyricsRecoveryImportDrift, item.MusicID)
	}
	return nil
}

func insertOrVerifyRecoveryEvidenceTx(ctx context.Context, tx *sql.Tx, ref lyricscontract.EvidenceRef,
	evidence lyricssource.IndexEvidence, now int64,
) error {
	categoriesJSON, err := recoveryCategoriesJSON(evidence.Categories)
	if err != nil {
		return err
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_source_evidence WHERE provider=? AND evidence_id=?`,
		ref.Provider, ref.EvidenceID).Scan(&existing); err != nil {
		return err
	}
	pageID, revisionID := nullablePositiveInt(evidence.PageID), nullablePositiveInt(evidence.RevisionID)
	if existing == 0 {
		_, err := tx.ExecContext(ctx, `INSERT INTO lyrics_recovery_source_evidence
			(provider,evidence_id,sha256,acquisition_id,envelope_sha256,kind,origin,page_id,revision_id,
			 revision_timestamp,mediawiki_sha1,page_title,canonical_revision_url,categories_json,canonical_request_url,
			 fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, evidence.Provider, evidence.EvidenceID, evidence.SHA256,
			ref.AcquisitionID, ref.EnvelopeSHA256, evidence.Kind, evidence.Origin, pageID, revisionID,
			evidence.RevisionTimestamp, evidence.MediaWikiSHA1, evidence.Title, evidence.CanonicalURL,
			categoriesJSON, evidence.CanonicalRequestURL, evidence.FetchedAt, evidence.Raw, len(evidence.Raw),
			evidence.RawSHA256, now)
		if err != nil {
			return fmt.Errorf("insert recovery evidence %s: %w", ref.EvidenceID, err)
		}
		return nil
	}
	var stored struct {
		provider, evidenceID, sha, acquisition, envelope, kind, origin, revisionTimestamp string
		mediawikiSHA1, title, canonicalURL, categories, requestURL, fetchedAt, rawSHA     string
		pageID, revisionID                                                                sql.NullInt64
		raw                                                                               []byte
		rawCount                                                                          int
		createdAt                                                                         int64
	}
	err = tx.QueryRowContext(ctx, `SELECT provider,evidence_id,sha256,acquisition_id,envelope_sha256,kind,origin,
		page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,canonical_revision_url,categories_json,
		canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at
		FROM lyrics_recovery_source_evidence WHERE provider=? AND evidence_id=?`, ref.Provider, ref.EvidenceID).Scan(
		&stored.provider, &stored.evidenceID, &stored.sha, &stored.acquisition, &stored.envelope, &stored.kind,
		&stored.origin, &stored.pageID, &stored.revisionID, &stored.revisionTimestamp, &stored.mediawikiSHA1,
		&stored.title, &stored.canonicalURL, &stored.categories, &stored.requestURL, &stored.fetchedAt,
		&stored.raw, &stored.rawCount, &stored.rawSHA, &stored.createdAt)
	if err != nil {
		return err
	}
	if stored.provider != string(evidence.Provider) || stored.evidenceID != evidence.EvidenceID || stored.sha != evidence.SHA256 ||
		stored.acquisition != ref.AcquisitionID || stored.envelope != ref.EnvelopeSHA256 || stored.kind != string(evidence.Kind) ||
		stored.origin != evidence.Origin || !nullableIntMatches(stored.pageID, evidence.PageID) ||
		!nullableIntMatches(stored.revisionID, evidence.RevisionID) || stored.revisionTimestamp != evidence.RevisionTimestamp ||
		stored.mediawikiSHA1 != evidence.MediaWikiSHA1 || stored.title != evidence.Title || stored.canonicalURL != evidence.CanonicalURL ||
		stored.categories != categoriesJSON || stored.requestURL != evidence.CanonicalRequestURL ||
		stored.fetchedAt != evidence.FetchedAt || !bytes.Equal(stored.raw, evidence.Raw) || stored.rawCount != len(evidence.Raw) ||
		stored.rawSHA != evidence.RawSHA256 || stored.createdAt <= 0 {
		return fmt.Errorf("%w: recovery evidence %s conflicts with stored bytes", store.ErrLyricsRecoveryImportConflict, ref.EvidenceID)
	}
	return nil
}

func recoveryCategoriesJSON(categories []string) (string, error) {
	if categories == nil {
		categories = []string{}
	}
	body, err := json.Marshal(categories)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func nullablePositiveInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullableIntMatches(stored sql.NullInt64, expected int) bool {
	return expected == 0 && !stored.Valid || expected > 0 && stored.Valid && stored.Int64 == int64(expected)
}

func insertRecoveryCompleteItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, lyrics model.SongLyrics, actor string, now int64,
) (int, error) {
	if _, err := store.LoadLyricsTx(tx, item.MusicID); !errors.Is(err, store.ErrLyricsNotFound) {
		if err == nil {
			return 0, fmt.Errorf("%w: music %d already has editable lyrics", store.ErrLyricsRecoveryImportConflict, item.MusicID)
		}
		return 0, err
	}
	if err := insertRecoveryEditableLyricsTx(ctx, tx, lyrics, actor, now); err != nil {
		return 0, err
	}
	if err := insertRecoveryFullSourceDocumentTx(ctx, tx, batchSHA256, item, now); err != nil {
		return 0, err
	}
	return 1, nil
}

func insertRecoveryV3DraftItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, now int64,
) error {
	if item.Draft == nil || item.Draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
		return fmt.Errorf("%w: music %d v3 Draft is missing", ErrLyricsRecoveryImportDrift, item.MusicID)
	}
	if err := ensureRecoveryItemOwnsNoEditableLyrics(ctx, tx, item.MusicID); err != nil {
		return err
	}
	if err := insertRecoveryFullSourceDocumentTx(ctx, tx, batchSHA256, item, now); err != nil {
		return err
	}
	return nil
}

func recoveryEditableReplayRevision(musicID int, requested, current model.SongLyrics) (int, error) {
	if !store.SameLyricsContent(requested, current) {
		return 0, fmt.Errorf("%w: music %d editable Full changed", store.ErrLyricsRecoveryImportConflict, musicID)
	}
	if current.Revision <= 0 {
		return 0, fmt.Errorf("%w: music %d editable Full has no durable revision", ErrLyricsRecoveryImportDrift, musicID)
	}
	return current.Revision, nil
}

func verifyRecoveryV3DraftItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item,
) error {
	if item.Draft == nil || item.Draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
		return fmt.Errorf("%w: music %d v3 Draft is missing", ErrLyricsRecoveryImportDrift, item.MusicID)
	}
	if err := ensureRecoveryItemOwnsNoEditableLyrics(ctx, tx, item.MusicID); err != nil {
		return err
	}
	return verifyRecoveryFullSourceDocumentTx(ctx, tx, batchSHA256, item)
}

func verifyRecoveryCompleteItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, requested model.SongLyrics,
) (int, error) {
	current, err := store.LoadLyricsTx(tx, item.MusicID)
	if err != nil {
		return 0, err
	}
	if !store.SameLyricsContent(requested, current) {
		return 0, fmt.Errorf("%w: music %d editable Full changed", store.ErrLyricsRecoveryImportConflict, item.MusicID)
	}
	if err := verifyRecoveryFullSourceDocumentTx(ctx, tx, batchSHA256, item); err != nil {
		return 0, err
	}
	return current.Revision, nil
}

func insertRecoveryEditableLyricsTx(ctx context.Context, tx *sql.Tx, lyrics model.SongLyrics, actor string, now int64) error {
	sourceHash := store.LyricsSourceHash(lyrics.Lines)
	if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics
		(music_id,revision,updated_at,updated_by,attribution,translation_credit,proofreading_credit,
		 source_note,source_url,license_note,source_hash,source_page_id,source_revision_id,source_sha1,
		 source_fetched_at,source_fetched_at_rfc3339)
		VALUES (?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, lyrics.MusicID, now, actor, lyrics.Attribution,
		lyrics.TranslationCredit, lyrics.ProofreadingCredit, lyrics.SourceNote, lyrics.SourceURL,
		lyrics.LicenseNote, sourceHash, lyrics.SourcePageID, lyrics.SourceRevisionID, lyrics.SourceSHA1,
		mustParseStagedTimestamp(lyrics.SourceFetchedAt), lyrics.SourceFetchedAt); err != nil {
		return err
	}
	for _, line := range lyrics.Lines {
		stanzaBreak := 0
		if line.StanzaBreakBefore {
			stanzaBreak = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_lines
			(music_id,line_id,position,japanese,zh_cn,en_us,stanza_break_before) VALUES (?,?,?,?,?,?,?)`,
			lyrics.MusicID, line.ID, line.Order, line.Japanese, line.Chinese, line.English, stanzaBreak); err != nil {
			return err
		}
		for position, segment := range line.Segments {
			performersJSON, err := json.Marshal(segment.PerformerIDs)
			if err != nil {
				return err
			}
			rubyJSON, err := json.Marshal(segment.Ruby)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_segments
				(music_id,line_id,position,text,performer_ids_json,ruby_json) VALUES (?,?,?,?,?,?)`,
				lyrics.MusicID, line.ID, position, segment.Text, string(performersJSON), string(rubyJSON)); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertRecoveryFullSourceDocumentTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, now int64,
) error {
	if item.Draft == nil || store.ValidatePersistedLyricsSourceDocument(item.Draft.Document) != nil {
		return fmt.Errorf("%w: music %d Full source document is invalid", ErrLyricsRecoveryImportDrift, item.MusicID)
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
	return store.InsertLyricsRenditionLocalizationsTx(ctx, tx, documentID, item.Draft.Document, item.Draft.RenditionTranslations, "recovery-import", now)
}

func verifyRecoveryFullSourceDocumentTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item,
) error {
	if item.Draft == nil {
		return fmt.Errorf("%w: complete music %d lost its Full draft", ErrLyricsRecoveryImportDrift, item.MusicID)
	}
	documentJSON, err := json.Marshal(item.Draft.Document)
	if err != nil {
		return err
	}
	var documentID int64
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT document_id FROM song_lyrics_source_documents WHERE music_id=? AND
		schema_version=? AND reason_code=? AND document_json=? AND document_sha256=? AND manifest_batch_sha256=? AND created_at>0`,
		item.MusicID, item.Draft.Document.SchemaVersion, item.Draft.Document.ReasonCode, string(documentJSON),
		item.Draft.DocumentSHA256, batchSHA256).Scan(&documentID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: music %d Full source document changed", store.ErrLyricsRecoveryImportConflict, item.MusicID)
		}
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM song_lyrics_source_artifacts WHERE document_id=?)+
		(SELECT COUNT(*) FROM song_lyrics_component_contributions WHERE document_id=?)`, documentID, documentID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("%w: music %d mixed legacy and recovery provenance graphs", store.ErrLyricsRecoveryImportConflict, item.MusicID)
	}
	if item.Draft.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		storedTranslations, err := store.ExportLyricsRenditionLocalizationsTx(ctx, tx, documentID, item.Draft.Document)
		if err != nil {
			return err
		}
		storedDigest, err := store.LyricsRenditionTranslationsDigest(storedTranslations)
		if err != nil {
			return err
		}
		expectedDigest, err := store.LyricsRenditionTranslationsDigest(item.Draft.RenditionTranslations)
		if err != nil || storedDigest != expectedDigest {
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: music %d rendition localizations changed", ErrLyricsRecoveryImportDrift, item.MusicID)
		}
	}
	return nil
}

func ensureRecoveryItemOwnsNoEditableLyrics(ctx context.Context, tx *sql.Tx, musicID int) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM song_lyrics WHERE music_id=?`, musicID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("%w: non-Full music %d already has editable lyrics", store.ErrLyricsRecoveryImportConflict, musicID)
	}
	return nil
}

func insertRecoveryAvailabilityItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, documentJSON string, now int64,
) error {
	document := item.Availability
	_, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_availability_documents
		(batch_sha256,music_id,schema_version,state,reason_code,no_lyrics_reason,document_json,document_sha256,result_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, batchSHA256, item.MusicID, document.SchemaVersion, document.State,
		document.ReasonCode, document.NoLyricsReason, documentJSON, item.AvailabilityDocumentSHA256, item.ResultSHA256, now)
	return err
}

func verifyRecoveryAvailabilityItemTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, documentJSON string,
) error {
	document := item.Availability
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM song_lyrics_availability_documents WHERE
		batch_sha256=? AND music_id=? AND schema_version=? AND state=? AND reason_code=? AND no_lyrics_reason=? AND
		document_json=? AND document_sha256=? AND result_sha256=? AND created_at>0`, batchSHA256, item.MusicID,
		document.SchemaVersion, document.State, document.ReasonCode, document.NoLyricsReason, documentJSON,
		item.AvailabilityDocumentSHA256, item.ResultSHA256).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: music %d availability document changed", store.ErrLyricsRecoveryImportConflict, item.MusicID)
	}
	return nil
}

func validateStoreLyricsAvailabilityDocument(document model.LyricsAvailabilityDocument) error {
	if err := model.ValidateLyricsAvailabilityDocument(document); err != nil {
		return err
	}
	if document.Game == nil {
		return nil
	}
	if err := lyricscontract.ValidatePersistedPerformerMetadata(*document.Game); err != nil {
		return errors.New("unsafe persisted lyrics performer metadata")
	}
	canonicalRubyVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(document.Game.RubyGeneratorVersion)
	if err != nil || canonicalRubyVersion != document.Game.RubyGeneratorVersion {
		return errors.New("unsafe persisted lyrics ruby generator metadata")
	}
	return nil
}

func recoveryItemArtifacts(item lyricsrecoveryimport.Item) []lyricsstaging.Artifact {
	if item.Draft != nil {
		return item.Draft.Artifacts
	}
	return item.Artifacts
}

func recoveryItemComponentRefs(item lyricsrecoveryimport.Item) map[string]string {
	if item.Draft != nil {
		return store.LyricsSourceComponentRefs(item.Draft.Document)
	}
	if item.Availability == nil || item.Availability.State != model.LyricsAvailabilityStateGameOnly {
		return map[string]string{}
	}
	refs := map[string]string{
		"game_text":        item.Availability.Provenance.GameText.RenditionKey,
		"version_evidence": item.Availability.Provenance.VersionEvidence.RenditionKey,
	}
	if item.Availability.Provenance.PerformerSegmentation != nil {
		refs["performer_segmentation"] = item.Availability.Provenance.PerformerSegmentation.RenditionKey
	}
	if item.Availability.Provenance.Ruby != nil {
		refs["ruby"] = item.Availability.Provenance.Ruby.RenditionKey
	}
	return refs
}

func recoveryItemDocumentSHA256(item lyricsrecoveryimport.Item) string {
	if item.Draft != nil {
		return item.Draft.DocumentSHA256
	}
	return item.AvailabilityDocumentSHA256
}

func insertRecoveryProvenanceGraphTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item, now int64,
) error {
	for _, artifact := range recoveryItemArtifacts(item) {
		if err := insertRecoveryArtifactTx(ctx, tx, batchSHA256, item.MusicID, artifact, now); err != nil {
			return err
		}
	}
	components := recoveryItemComponentRefs(item)
	keys := make([]string, 0, len(components))
	for component := range components {
		keys = append(keys, component)
	}
	sort.Strings(keys)
	ownerSHA := recoveryItemDocumentSHA256(item)
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

func insertRecoveryArtifactTx(ctx context.Context, tx *sql.Tx, batchSHA256 string, musicID int,
	artifact lyricsstaging.Artifact, now int64,
) error {
	identityJSON, err := json.Marshal(artifact.Identity)
	if err != nil {
		return err
	}
	identityDigest := sha256.Sum256(identityJSON)
	categoriesJSON, err := recoveryCategoriesJSON(artifact.Identity.Categories)
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

func verifyRecoveryProvenanceGraphTx(ctx context.Context, tx *sql.Tx, batchSHA256 string,
	item lyricsrecoveryimport.Item,
) error {
	artifacts := recoveryItemArtifacts(item)
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_import_artifacts WHERE batch_sha256=? AND music_id=?`,
		batchSHA256, item.MusicID).Scan(&count); err != nil {
		return err
	}
	if count != len(artifacts) {
		return fmt.Errorf("%w: music %d recovery artifact count changed", ErrLyricsRecoveryImportDrift, item.MusicID)
	}
	for _, artifact := range artifacts {
		if err := verifyRecoveryArtifactTx(ctx, tx, batchSHA256, item.MusicID, artifact); err != nil {
			return err
		}
	}
	components := recoveryItemComponentRefs(item)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_import_component_contributions
		WHERE batch_sha256=? AND music_id=?`, batchSHA256, item.MusicID).Scan(&count); err != nil {
		return err
	}
	if count != len(components) {
		return fmt.Errorf("%w: music %d recovery component count changed", ErrLyricsRecoveryImportDrift, item.MusicID)
	}
	ownerSHA := recoveryItemDocumentSHA256(item)
	for component, renditionKey := range components {
		digest := sha256.Sum256([]byte(ownerSHA + "\x00" + component + "\x00" + renditionKey))
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT contribution_sha256 FROM lyrics_recovery_import_component_contributions
			WHERE batch_sha256=? AND music_id=? AND component=? AND rendition_key=?`, batchSHA256, item.MusicID,
			component, renditionKey).Scan(&stored); err != nil {
			return err
		}
		if stored != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("%w: music %d recovery %s contribution changed", ErrLyricsRecoveryImportDrift, item.MusicID, component)
		}
	}
	return nil
}

func verifyRecoveryArtifactTx(ctx context.Context, tx *sql.Tx, batchSHA256 string, musicID int,
	artifact lyricsstaging.Artifact,
) error {
	identityJSON, err := json.Marshal(artifact.Identity)
	if err != nil {
		return err
	}
	identityDigest := sha256.Sum256(identityJSON)
	categoriesJSON, err := recoveryCategoriesJSON(artifact.Identity.Categories)
	if err != nil {
		return err
	}
	evidenceJSON, err := json.Marshal(artifact.Identity.IndexEvidenceRefs)
	if err != nil {
		return err
	}
	var count int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_import_artifacts WHERE
		batch_sha256=? AND music_id=? AND provider=? AND rendition_key=? AND origin=? AND page_id=? AND revision_id=? AND
		revision_timestamp=? AND mediawiki_sha1=? AND page_title=? AND canonical_revision_url=? AND fetched_at=? AND
		categories_json=? AND section=? AND composition_rendition_key=? AND version_reason=? AND index_evidence_refs_json=? AND
		fixed_identity_json=? AND fixed_identity_sha256=? AND raw_byte_count=? AND raw_wikitext_sha256=? AND artifact_sha256=? AND created_at>0`,
		batchSHA256, musicID, artifact.Identity.Provider, artifact.Identity.RenditionKey, artifact.Identity.Origin,
		artifact.Identity.PageID, artifact.Identity.RevisionID, artifact.Identity.RevisionTimestamp, artifact.Identity.SHA1,
		artifact.Identity.Title, artifact.Identity.CanonicalURL, artifact.Identity.FetchedAt, categoriesJSON,
		artifact.Identity.Section, artifact.Identity.CompositionRenditionKey, artifact.Identity.VersionReason,
		string(evidenceJSON), string(identityJSON), hex.EncodeToString(identityDigest[:]), artifact.RawWikitextByteCount,
		artifact.RawWikitextSHA256, artifact.ArtifactSHA256).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: music %d recovery artifact %q changed", ErrLyricsRecoveryImportDrift, musicID, artifact.Identity.RenditionKey)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_import_artifact_evidence
		WHERE batch_sha256=? AND music_id=? AND rendition_key=?`, batchSHA256, musicID,
		artifact.Identity.RenditionKey).Scan(&count); err != nil {
		return err
	}
	if count != len(artifact.Identity.IndexEvidenceRefs) {
		return fmt.Errorf("%w: music %d recovery artifact evidence count changed", ErrLyricsRecoveryImportDrift, musicID)
	}
	for position, reference := range artifact.Identity.IndexEvidenceRefs {
		var provider model.LyricsSourceProvider
		var evidenceID, digest string
		if err := tx.QueryRowContext(ctx, `SELECT provider,evidence_id,sha256 FROM lyrics_recovery_import_artifact_evidence
			WHERE batch_sha256=? AND music_id=? AND rendition_key=? AND position=?`, batchSHA256, musicID,
			artifact.Identity.RenditionKey, position).Scan(&provider, &evidenceID, &digest); err != nil {
			return err
		}
		if provider != artifact.Identity.Provider || evidenceID != reference.EvidenceID || digest != reference.SHA256 {
			return fmt.Errorf("%w: music %d recovery artifact evidence changed", ErrLyricsRecoveryImportDrift, musicID)
		}
	}
	return nil
}

func recoveryManifestMusicIDs(manifest lyricsrecoveryimport.Manifest) []int {
	musicIDs := make([]int, 0, len(manifest.Items))
	for _, item := range manifest.Items {
		musicIDs = append(musicIDs, item.MusicID)
	}
	return musicIDs
}
