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
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/singleinstance"
	"moesekai/server/internal/store"
	"moesekai/server/offline/internal/lyricsevidencepack"
	"moesekai/server/offline/internal/lyricsrecoveryimport"
	"moesekai/server/offline/internal/lyricsrootmanifest"
	"moesekai/server/offline/internal/offlineimport"
)

var canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type options struct {
	rootPath            string
	manifestPath        string
	evidenceReceiptPath string
	evidencePath        string
	databasePath        string
	backupPath          string
	backupSHA256        string
	importReceiptPath   string
	actor               string
	confirmLocalOffline bool
}

type executionHooks struct {
	beforeCommitValidation func() error
	afterReceiptReserve    func() error
	checkpoint             func(context.Context, *db.DB) error
}

type recoveryReceiptAudit struct {
	SchemaVersion int    `json:"schemaVersion"`
	ReceiptPath   string `json:"receiptPath"`
	ReceiptSHA256 string `json:"receiptSha256"`
	ReceiptJSON   string `json:"receiptJson"`
}

type committedRecoveryState struct {
	batchCount     int
	batchCreatedAt int64
	changedCount   int
	protectedScope recoveryProtectedScope
	counts         lyricsrecoveryimport.ImportStorageCounts
	items          []lyricsrecoveryimport.ImportReceiptItem
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lyrics recovery import: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	if ctx == nil || output == nil {
		return errors.New("recovery import requires context and output")
	}
	opts, err := parseOptions(arguments)
	if err != nil {
		return err
	}
	receipt, results, state, err := executeWithHooks(ctx, opts, executionHooks{})
	if err != nil {
		return err
	}
	changed := 0
	states := map[string]int{}
	for _, result := range results {
		if result.Changed {
			changed++
		}
		states[string(result.State)]++
	}
	_, err = fmt.Fprintf(output,
		"PASS recoveryImportReplay items=%d changed=%d replayed=%d complete=%d gameOnly=%d noLyrics=%d unresolved=%d batches=%d sourceDocuments=%d availabilityDocuments=%d artifacts=%d evidence=%d links=%d contributions=%d batchSha256=%s rootSha256=%s importReceiptSha256=%s importReceipt=%s\n",
		state.counts.BatchItems, changed, len(results)-changed, states["complete"], states["game_only"], states["satisfied_no_lyrics"],
		states["ambiguous"]+states["missing"]+states["incomplete"]+states["failed"], state.batchCount,
		state.counts.SourceDocuments, state.counts.AvailabilityDocuments, state.counts.Artifacts, state.counts.EvidenceSelection,
		state.counts.ArtifactEvidenceLinks, state.counts.ComponentContributions, receipt.BatchSHA256, receipt.RootSHA256,
		receipt.ReceiptSHA256, opts.importReceiptPath)
	return err
}

func parseOptions(arguments []string) (options, error) {
	var opts options
	flags := flag.NewFlagSet("lyrics-recovery-import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.rootPath, "root-manifest", "", "canonical compact recovery root")
	flags.StringVar(&opts.manifestPath, "import-manifest", "", "canonical all-root recovery import manifest")
	flags.StringVar(&opts.evidenceReceiptPath, "evidence-receipt", "", "canonical compact recovery evidence receipt")
	flags.StringVar(&opts.evidencePath, "evidence-pack", "", "private exact evidence-pack directory")
	flags.StringVar(&opts.databasePath, "database", "", "existing offline SQLite database")
	flags.StringVar(&opts.backupPath, "backup", "", "existing standalone SQLite backup")
	flags.StringVar(&opts.backupSHA256, "backup-sha256", "", "operator-acknowledged SHA-256 of -backup")
	flags.StringVar(&opts.importReceiptPath, "import-receipt", "", "new durable no-overwrite recovery import receipt")
	flags.StringVar(&opts.actor, "actor", "", "offline import operator")
	flags.BoolVar(&opts.confirmLocalOffline, "confirm-local-offline", false, "confirm that the database is offline, backed up, and not production")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return options{}, errors.New("recovery import requires only explicit named flags")
	}
	if err := validateOptions(opts); err != nil {
		return options{}, err
	}
	return opts, nil
}

func validateOptions(opts options) error {
	if raw := strings.TrimSpace(os.Getenv("MOESEKAI_PRODUCTION")); raw != "" {
		production, err := strconv.ParseBool(raw)
		if err != nil || production {
			return errors.New("lyrics-recovery-import is a local offline command and refuses MOESEKAI_PRODUCTION")
		}
	}
	if !opts.confirmLocalOffline {
		return errors.New("-confirm-local-offline is required")
	}
	for name, value := range map[string]string{
		"-root-manifest": opts.rootPath, "-import-manifest": opts.manifestPath,
		"-evidence-receipt": opts.evidenceReceiptPath, "-evidence-pack": opts.evidencePath,
		"-database": opts.databasePath, "-backup": opts.backupPath, "-backup-sha256": opts.backupSHA256,
		"-import-receipt": opts.importReceiptPath, "-actor": opts.actor,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("%s is required without surrounding whitespace", name)
		}
	}
	for name, path := range map[string]string{
		"-root-manifest": opts.rootPath, "-import-manifest": opts.manifestPath,
		"-evidence-receipt": opts.evidenceReceiptPath, "-evidence-pack": opts.evidencePath,
		"-database": opts.databasePath, "-backup": opts.backupPath, "-import-receipt": opts.importReceiptPath,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("%s must be a canonical absolute path", name)
		}
	}
	if len(opts.actor) > 128 || strings.ContainsAny(opts.actor, "\x00\r\n") || !canonicalSHA256.MatchString(opts.backupSHA256) {
		return errors.New("-actor must be at most 128 bytes and -backup-sha256 must be 64 lowercase hexadecimal characters")
	}
	if pathInsideDirectory(opts.evidencePath, opts.importReceiptPath) {
		return errors.New("recovery import receipt must remain outside the evidence pack")
	}
	return nil
}

func executeWithHooks(ctx context.Context, opts options, hooks executionHooks) (
	receipt lyricsrecoveryimport.ImportReceipt,
	results []offlineimport.RecoveryLyricsImportItem,
	state committedRecoveryState,
	returnErr error,
) {
	if err := ctx.Err(); err != nil {
		return receipt, nil, state, err
	}
	rootInfo, err := inspectExistingPrivateRegular(opts.rootPath, "compact recovery root")
	if err != nil {
		return receipt, nil, state, err
	}
	manifestInfo, err := inspectExistingPrivateRegular(opts.manifestPath, "recovery import manifest")
	if err != nil {
		return receipt, nil, state, err
	}
	evidenceReceiptInfo, err := inspectExistingPrivateRegular(opts.evidenceReceiptPath, "recovery evidence receipt")
	if err != nil {
		return receipt, nil, state, err
	}
	databaseInfo, err := inspectExistingPrivateRegular(opts.databasePath, "offline database")
	if err != nil {
		return receipt, nil, state, err
	}
	backupInfo, err := inspectExistingPrivateRegular(opts.backupPath, "SQLite backup")
	if err != nil {
		return receipt, nil, state, err
	}
	if err := requireDistinctPaths([]namedFileInfo{
		{name: "compact recovery root", path: opts.rootPath, info: rootInfo},
		{name: "recovery import manifest", path: opts.manifestPath, info: manifestInfo},
		{name: "recovery evidence receipt", path: opts.evidenceReceiptPath, info: evidenceReceiptInfo},
		{name: "offline database", path: opts.databasePath, info: databaseInfo},
		{name: "SQLite backup", path: opts.backupPath, info: backupInfo},
	}); err != nil {
		return receipt, nil, state, err
	}
	if err := validateNewReceiptPath(opts.importReceiptPath, opts.rootPath, opts.manifestPath, opts.evidenceReceiptPath, opts.databasePath, opts.backupPath); err != nil {
		return receipt, nil, state, err
	}
	if err := rejectSQLiteSidecars(opts.databasePath, "offline database"); err != nil {
		return receipt, nil, state, err
	}
	if err := rejectSQLiteSidecars(opts.backupPath, "SQLite backup"); err != nil {
		return receipt, nil, state, err
	}

	owner, err := singleinstance.Acquire(opts.databasePath)
	if err != nil {
		return receipt, nil, state, fmt.Errorf("acquire offline database ownership: %w", err)
	}
	defer func() {
		if closeErr := owner.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()

	rootPinned, err := openPinnedFile(opts.rootPath, rootInfo, "compact recovery root")
	if err != nil {
		return receipt, nil, state, err
	}
	defer joinPinnedClose(&returnErr, rootPinned)
	rootBody, rootFileSHA, err := readPinnedFile(rootPinned, lyricsrootmanifest.MaxManifestBytes, "compact recovery root")
	if err != nil {
		return receipt, nil, state, err
	}
	root, err := lyricsrootmanifest.DecodeCanonical(rootBody)
	if err != nil {
		return receipt, nil, state, err
	}

	manifestPinned, err := openPinnedFile(opts.manifestPath, manifestInfo, "recovery import manifest")
	if err != nil {
		return receipt, nil, state, err
	}
	defer joinPinnedClose(&returnErr, manifestPinned)
	manifestBody, manifestFileSHA, err := readPinnedFile(manifestPinned, lyricsrecoveryimport.MaxManifestBytes, "recovery import manifest")
	if err != nil {
		return receipt, nil, state, err
	}
	manifest, err := lyricsrecoveryimport.DecodeCanonical(manifestBody)
	if err != nil {
		return receipt, nil, state, err
	}

	evidenceReceiptPinned, err := openPinnedFile(opts.evidenceReceiptPath, evidenceReceiptInfo, "recovery evidence receipt")
	if err != nil {
		return receipt, nil, state, err
	}
	defer joinPinnedClose(&returnErr, evidenceReceiptPinned)
	evidenceReceiptBody, evidenceReceiptFileSHA, err := readPinnedFile(evidenceReceiptPinned, lyricsrecoveryimport.MaxEvidenceReceiptBytes, "recovery evidence receipt")
	if err != nil {
		return receipt, nil, state, err
	}
	evidenceReceipt, err := lyricsrecoveryimport.DecodeEvidenceReceipt(evidenceReceiptBody)
	if err != nil {
		return receipt, nil, state, err
	}
	if err := validateRecoveryBindings(root, manifest, evidenceReceipt); err != nil {
		return receipt, nil, state, err
	}
	resolver, err := lyricsevidencepack.OpenResolver(opts.evidencePath)
	if err != nil {
		return receipt, nil, state, err
	}
	if err := lyricsrecoveryimport.ValidateEvidenceReceiptAgainst(evidenceReceipt, root, manifest, resolver.Manifest()); err != nil {
		return receipt, nil, state, err
	}
	protectedScope, err := newRecoveryProtectedScope(manifest, evidenceReceipt)
	if err != nil {
		return receipt, nil, state, fmt.Errorf("prepare protected recovery scope: %w", err)
	}

	databasePinned, err := openPinnedWritableFile(opts.databasePath, databaseInfo, "offline database")
	if err != nil {
		return receipt, nil, state, err
	}
	defer joinPinnedClose(&returnErr, databasePinned)
	backupPinned, err := openPinnedFile(opts.backupPath, backupInfo, "SQLite backup")
	if err != nil {
		return receipt, nil, state, err
	}
	defer joinPinnedClose(&returnErr, backupPinned)
	databaseIdentity, err := verifyPinnedSQLiteSnapshot(ctx, databasePinned, "offline database", protectedScope)
	if err != nil {
		return receipt, nil, state, err
	}
	backupIdentity, err := verifyPinnedSQLiteSnapshot(ctx, backupPinned, "SQLite backup", protectedScope)
	if err != nil {
		return receipt, nil, state, err
	}
	if backupIdentity.FileSHA256 != opts.backupSHA256 {
		return receipt, nil, state, errors.New("-backup-sha256 does not match the verified SQLite backup bytes")
	}
	if backupIdentity.StateSHA256 != databaseIdentity.StateSHA256 ||
		backupIdentity.ProtectedStateSHA256 != databaseIdentity.ProtectedStateSHA256 ||
		backupIdentity.AuditMaxID != databaseIdentity.AuditMaxID || backupIdentity.CatalogCount != databaseIdentity.CatalogCount {
		return receipt, nil, state, errors.New("SQLite backup logical, protected, catalog, or audit-prefix state does not match the exact pre-import offline database")
	}
	if databaseIdentity.CatalogCount != manifest.Root.CatalogCount {
		return receipt, nil, state, fmt.Errorf("recovery import manifest catalog count %d does not match offline database catalog count %d",
			manifest.Root.CatalogCount, databaseIdentity.CatalogCount)
	}
	if err := rejectSQLiteSidecars(opts.databasePath, "offline database after state verification"); err != nil {
		return receipt, nil, state, err
	}
	if err := rejectSQLiteSidecars(opts.backupPath, "SQLite backup after state verification"); err != nil {
		return receipt, nil, state, err
	}
	confirmedDatabaseSHA, err := digestPinnedFile(databasePinned, maxBackupBytes, "offline database")
	if err != nil || confirmedDatabaseSHA != databaseIdentity.FileSHA256 {
		if err != nil {
			return receipt, nil, state, err
		}
		return receipt, nil, state, errors.New("offline database changed after backup-state binding")
	}

	databaseAnchor, err := createPinnedSQLiteAnchor(databasePinned)
	if err != nil {
		return receipt, nil, state, err
	}
	var receiptReservation *reservedReceipt
	defer func() {
		if receiptReservation != nil && receiptReservation.commitAttempted && databaseAnchor.directory != "" {
			return
		}
		if closeErr := databaseAnchor.close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	database, err := db.OpenOfflinePinned(databaseAnchor.path)
	if err != nil {
		return receipt, nil, state, err
	}
	databaseOpen := true
	defer func() {
		if databaseOpen {
			if closeErr := database.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	if err := databaseAnchor.verifyPinned(databasePinned, "after database open"); err != nil {
		return receipt, nil, state, err
	}
	if err := databasePinned.verifySamePath("after database open", false); err != nil {
		return receipt, nil, state, err
	}
	defer func() {
		if receiptReservation == nil {
			return
		}
		if closeErr := receiptReservation.finish(); closeErr != nil {
			if receiptReservation.commitAttempted {
				closeErr = postCommitRecoveryError(opts.importReceiptPath, closeErr)
			}
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()

	results, commitAttempted, importErr := offlineimport.ImportRecoveryLyricsManifestWithCommitHook(
		ctx, store.New(database), root, manifest, evidenceReceipt, resolver, opts.actor,
		func(tx *sql.Tx, hookResults []offlineimport.RecoveryLyricsImportItem, batchCreatedAt int64) error {
			if hooks.beforeCommitValidation != nil {
				if err := hooks.beforeCommitValidation(); err != nil {
					return err
				}
			}
			if err := databaseAnchor.verifyPinned(databasePinned, "before receipt preparation"); err != nil {
				return err
			}
			if err := databasePinned.verifySamePath("before receipt preparation", false); err != nil {
				return err
			}
			for _, input := range []struct {
				pinned   *pinnedFile
				expected string
				maximum  int64
				label    string
			}{
				{rootPinned, rootFileSHA, lyricsrootmanifest.MaxManifestBytes, "compact recovery root"},
				{manifestPinned, manifestFileSHA, lyricsrecoveryimport.MaxManifestBytes, "recovery import manifest"},
				{evidenceReceiptPinned, evidenceReceiptFileSHA, lyricsrecoveryimport.MaxEvidenceReceiptBytes, "recovery evidence receipt"},
			} {
				if err := verifyPinnedImmutableDigest(input.pinned, input.expected, input.maximum, input.label); err != nil {
					return err
				}
			}
			if err := backupPinned.verifySamePath("before receipt preparation", true); err != nil {
				return fmt.Errorf("SQLite backup: %w", err)
			}
			confirmedBackupSHA, err := digestPinnedFile(backupPinned, maxBackupBytes, "SQLite backup")
			if err != nil || confirmedBackupSHA != backupIdentity.FileSHA256 {
				if err != nil {
					return err
				}
				return errors.New("SQLite backup changed before receipt preparation")
			}
			if err := rejectSQLiteSidecars(opts.databasePath, "offline database before commit"); err != nil {
				return err
			}
			if err := rejectSQLiteSidecars(opts.backupPath, "SQLite backup before commit"); err != nil {
				return err
			}
			if err := validateNewReceiptPath(opts.importReceiptPath, opts.rootPath, opts.manifestPath, opts.evidenceReceiptPath, opts.databasePath, opts.backupPath); err != nil {
				return err
			}
			protectedAfter, err := sqliteProtectedStateDigest(ctx, tx, databaseIdentity.AuditMaxID, protectedScope)
			if err != nil {
				return fmt.Errorf("digest protected state before commit: %w", err)
			}
			if protectedAfter != databaseIdentity.ProtectedStateSHA256 {
				return errors.New("recovery import changed protected non-recovery database state")
			}
			state, err = committedRecoveryStateTx(ctx, tx, manifest, evidenceReceipt, hookResults, batchCreatedAt)
			if err != nil {
				return err
			}
			state.protectedScope = protectedScope
			preparedAt := time.Now().UTC()
			if preparedAt.Unix() < batchCreatedAt {
				return errors.New("receipt preparation clock precedes the immutable recovery batch creation time")
			}
			postStateSHA, err := sqliteLogicalStateDigest(ctx, tx)
			if err != nil {
				return fmt.Errorf("digest pre-commit recovery database state: %w", err)
			}
			receipt, err = lyricsrecoveryimport.NewImportReceipt(root, manifest, evidenceReceipt, lyricsrecoveryimport.ImportReceiptBinding{
				RootManifestFileSHA256: rootFileSHA, ImportManifestFileSHA256: manifestFileSHA,
				EvidenceReceiptFileSHA256: evidenceReceiptFileSHA, BackupSHA256: backupIdentity.FileSHA256,
				BackupStateSHA256: backupIdentity.StateSHA256, PreImportDatabaseSHA256: databaseIdentity.FileSHA256,
				PreImportStateSHA256: databaseIdentity.StateSHA256, PostImportStateSHA256: postStateSHA,
				PreImportProtectedSHA256: databaseIdentity.ProtectedStateSHA256, PostImportProtectedSHA256: protectedAfter,
				AuditBoundaryID: databaseIdentity.AuditMaxID,
				DatabasePath:    opts.databasePath, RecoveryDatabasePath: databaseAnchor.path,
				ReceiptPath: opts.importReceiptPath, Actor: opts.actor,
				BatchCreatedAt: time.Unix(batchCreatedAt, 0).UTC().Format(time.RFC3339Nano),
				PreparedAt:     preparedAt.Format(time.RFC3339Nano),
				Counts:         state.counts, Items: state.items,
			})
			if err != nil {
				return err
			}
			receiptBody, err := lyricsrecoveryimport.MarshalImportReceiptCanonical(receipt)
			if err != nil {
				return err
			}
			receiptDigest := sha256.Sum256(receiptBody)
			auditBody, err := json.Marshal(recoveryReceiptAudit{
				SchemaVersion: 1, ReceiptPath: opts.importReceiptPath,
				ReceiptSHA256: hex.EncodeToString(receiptDigest[:]), ReceiptJSON: string(receiptBody),
			})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,?,?)`,
				preparedAt.Unix(), opts.actor, lyricsrecoveryimport.ImportReceiptReceiptAuditAction, string(auditBody)); err != nil {
				return fmt.Errorf("record transactional recovery import receipt: %w", err)
			}
			receiptReservation, err = reserveReceipt(opts.importReceiptPath)
			if err != nil {
				return err
			}
			if hooks.afterReceiptReserve != nil {
				if err := hooks.afterReceiptReserve(); err != nil {
					return err
				}
			}
			if err := receiptReservation.publish(receiptBody); err != nil {
				return err
			}
			receiptReservation.commitAttempted = true
			return nil
		},
	)
	if receiptReservation != nil {
		receiptReservation.commitAttempted = receiptReservation.commitAttempted || commitAttempted
	}
	if importErr != nil {
		if commitAttempted {
			return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, fmt.Errorf("commit recovery import: %w", importErr))
		}
		return lyricsrecoveryimport.ImportReceipt{}, nil, committedRecoveryState{}, importErr
	}

	checkpoint := hooks.checkpoint
	if checkpoint == nil {
		checkpoint = func(ctx context.Context, database *db.DB) error { return database.Checkpoint(ctx) }
	}
	if err := checkpoint(ctx, database); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, fmt.Errorf("checkpoint recovery import database: %w", err))
	}
	if err := database.IntegrityCheck(ctx); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, fmt.Errorf("verify recovery import database: %w", err))
	}
	if err := verifyCommittedRecoveryState(ctx, database, receipt, state); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := databaseAnchor.verifyPinned(databasePinned, "after recovery import"); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := databasePinned.verifySamePath("after recovery import", false); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := receiptReservation.verify("after recovery import"); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := database.Close(); err != nil {
		databaseOpen = false
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, fmt.Errorf("close imported database: %w", err))
	}
	databaseOpen = false
	if err := databaseAnchor.verifyPinned(databasePinned, "after database close"); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := databasePinned.verifySamePath("after database close", false); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := rejectSQLiteSidecars(opts.databasePath, "imported database"); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := backupPinned.verifySamePath("after recovery import", true); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, fmt.Errorf("SQLite backup: %w", err))
	}
	if err := rejectSQLiteSidecars(opts.backupPath, "SQLite backup after recovery import"); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := receiptReservation.verify("after database close"); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	if err := databaseAnchor.close(); err != nil {
		return receipt, results, state, postCommitRecoveryError(opts.importReceiptPath, err)
	}
	return receipt, results, state, nil
}

func validateRecoveryBindings(root lyricsrootmanifest.Manifest, manifest lyricsrecoveryimport.Manifest, evidence lyricsrecoveryimport.EvidenceReceipt) error {
	if manifest.Root.SchemaVersion != root.SchemaVersion || manifest.Root.RootID != root.RootID ||
		manifest.Root.RootSHA256 != root.RootSHA256 || manifest.Root.CatalogCount != root.Catalog.RecordCount ||
		manifest.Root.MusicIDsSHA256 != root.Catalog.MusicIDsSHA256 || manifest.Root.Coverage != root.Coverage ||
		len(manifest.Items) != len(root.Songs) || evidence.RootID != root.RootID || evidence.RootSHA256 != root.RootSHA256 {
		return errors.New("recovery root, import manifest, and evidence receipt bindings do not match")
	}
	for index, item := range manifest.Items {
		rootSong := root.Songs[index]
		if item.MusicID != rootSong.MusicID || item.State != rootSong.State || item.ResultSHA256 != rootSong.ResultSHA256 {
			return fmt.Errorf("recovery import item %d does not exactly follow the compact root", index)
		}
	}
	return nil
}

func committedRecoveryStateTx(ctx context.Context, tx *sql.Tx, manifest lyricsrecoveryimport.Manifest,
	evidence lyricsrecoveryimport.EvidenceReceipt, results []offlineimport.RecoveryLyricsImportItem, batchCreatedAt int64,
) (committedRecoveryState, error) {
	state := committedRecoveryState{counts: lyricsrecoveryimport.ExpectedImportStorageCounts(manifest, evidence), batchCreatedAt: batchCreatedAt}
	if len(results) != len(manifest.Items) {
		return committedRecoveryState{}, errors.New("recovery import result count does not match the manifest")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lyrics_recovery_import_batches`).Scan(&state.batchCount); err != nil {
		return committedRecoveryState{}, err
	}
	actualCounts, err := queryRecoveryStorageCounts(ctx, tx, manifest.BatchSHA256, evidence.EvidenceCount)
	if err != nil {
		return committedRecoveryState{}, err
	}
	if actualCounts != state.counts {
		return committedRecoveryState{}, errors.New("pre-commit recovery storage counts do not match the exact import manifest")
	}
	var storedBatchCreatedAt int64
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM lyrics_recovery_import_batches WHERE batch_sha256=?`, manifest.BatchSHA256).Scan(&storedBatchCreatedAt); err != nil {
		return committedRecoveryState{}, err
	}
	if storedBatchCreatedAt != batchCreatedAt {
		return committedRecoveryState{}, errors.New("recovery import batch creation timestamp drifted before receipt preparation")
	}
	for _, result := range results {
		if result.Changed {
			state.changedCount++
		}
	}
	if state.changedCount != 0 && state.changedCount != len(results) {
		return committedRecoveryState{}, errors.New("recovery import mixed changed and replayed results in one atomic batch")
	}
	state.items = receiptItems(results)
	return state, nil
}

func verifyCommittedRecoveryState(ctx context.Context, database *db.DB, receipt lyricsrecoveryimport.ImportReceipt, expected committedRecoveryState) error {
	actualCounts, err := queryRecoveryStorageCounts(ctx, database, receipt.BatchSHA256, receipt.Counts.EvidenceSelection)
	if err != nil {
		return err
	}
	if actualCounts != expected.counts || actualCounts != receipt.Counts {
		return errors.New("committed recovery storage counts do not match the durable receipt")
	}
	var batchCount, batchCreatedAt int
	if err := database.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM lyrics_recovery_import_batches),created_at
		FROM lyrics_recovery_import_batches WHERE batch_sha256=?`, receipt.BatchSHA256).Scan(&batchCount, &batchCreatedAt); err != nil {
		return err
	}
	if batchCount != expected.batchCount || int64(batchCreatedAt) != expected.batchCreatedAt {
		return errors.New("committed recovery batch identity changed after commit")
	}
	protectedAfter, err := sqliteProtectedStateDigest(ctx, database, receipt.AuditBoundaryID, expected.protectedScope)
	if err != nil {
		return err
	}
	if protectedAfter != receipt.PostImportProtectedSHA256 {
		return errors.New("protected non-recovery database state changed after commit")
	}
	stateAfter, err := sqliteLogicalStateDigest(ctx, database)
	if err != nil {
		return err
	}
	if stateAfter != receipt.PostImportStateSHA256 {
		return errors.New("committed database logical state does not match the durable receipt")
	}
	body, err := os.ReadFile(receipt.ReceiptPath)
	if err != nil {
		return err
	}
	decoded, err := lyricsrecoveryimport.DecodeImportReceiptCanonical(body)
	if err != nil {
		return err
	}
	decodedBody, err := lyricsrecoveryimport.MarshalImportReceiptCanonical(decoded)
	if err != nil || !bytes.Equal(decodedBody, body) {
		return errors.New("durable recovery import receipt changed after commit")
	}
	digest := sha256.Sum256(body)
	preparedAt, err := time.Parse(time.RFC3339Nano, receipt.PreparedAt)
	if err != nil {
		return err
	}
	expectedAuditDetail, err := json.Marshal(recoveryReceiptAudit{
		SchemaVersion: 1, ReceiptPath: receipt.ReceiptPath,
		ReceiptSHA256: hex.EncodeToString(digest[:]), ReceiptJSON: string(body),
	})
	if err != nil {
		return err
	}
	var receiptAuditCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE id>? AND ts=? AND user=? AND action=? AND detail=?`,
		receipt.AuditBoundaryID, preparedAt.Unix(), receipt.Actor, lyricsrecoveryimport.ImportReceiptReceiptAuditAction,
		string(expectedAuditDetail)).Scan(&receiptAuditCount); err != nil {
		return err
	}
	if receiptAuditCount != 1 {
		return errors.New("transactional recovery receipt audit does not uniquely match the exact receipt JSON")
	}
	var importAuditCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE id>? AND action=?`,
		receipt.AuditBoundaryID, lyricsrecoveryimport.ImportReceiptAuditAction).Scan(&importAuditCount); err != nil {
		return err
	}
	expectedImportAuditCount := 0
	if expected.changedCount > 0 {
		expectedImportAuditCount = 1
	}
	if importAuditCount != expectedImportAuditCount {
		return errors.New("ordinary recovery import audit count does not match first-import or replay semantics")
	}
	if expectedImportAuditCount == 1 {
		expectedDetail := fmt.Sprintf("rootId=%s rootSha256=%s batchSha256=%s items=%d",
			receipt.RootID, receipt.RootSHA256, receipt.BatchSHA256, receipt.CatalogCount)
		var exactImportAuditCount int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE id>? AND ts=? AND user=? AND action=? AND detail=?`,
			receipt.AuditBoundaryID, expected.batchCreatedAt, receipt.Actor, lyricsrecoveryimport.ImportReceiptAuditAction,
			expectedDetail).Scan(&exactImportAuditCount); err != nil {
			return err
		}
		if exactImportAuditCount != 1 {
			return errors.New("ordinary recovery import audit does not exactly match the imported batch")
		}
	}
	var totalNewAuditCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE id>?`, receipt.AuditBoundaryID).Scan(&totalNewAuditCount); err != nil {
		return err
	}
	if totalNewAuditCount != expectedImportAuditCount+1 {
		return errors.New("recovery import appended an unexpected number of audit rows")
	}
	var unexpectedAuditCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE id>? AND action NOT IN (?,?)`,
		receipt.AuditBoundaryID, lyricsrecoveryimport.ImportReceiptAuditAction,
		lyricsrecoveryimport.ImportReceiptReceiptAuditAction).Scan(&unexpectedAuditCount); err != nil {
		return err
	}
	if unexpectedAuditCount != 0 {
		return errors.New("recovery import appended an unexpected audit action")
	}
	return nil
}

type recoveryCountQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func queryRecoveryStorageCounts(ctx context.Context, query recoveryCountQuery, batchSHA256 string, evidenceCount int) (lyricsrecoveryimport.ImportStorageCounts, error) {
	counts := lyricsrecoveryimport.ImportStorageCounts{EvidenceSelection: evidenceCount}
	if err := query.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM lyrics_recovery_import_items WHERE batch_sha256=?),
		(SELECT COUNT(*) FROM song_lyrics AS lyrics JOIN lyrics_recovery_import_items AS item
		 ON item.music_id=lyrics.music_id WHERE item.batch_sha256=? AND item.state='complete'),
		(SELECT COUNT(*) FROM song_lyrics_source_documents WHERE manifest_batch_sha256=?),
		(SELECT COUNT(*) FROM song_lyrics_availability_documents WHERE batch_sha256=?),
		(SELECT COUNT(*) FROM lyrics_recovery_import_artifacts WHERE batch_sha256=?),
		(SELECT COUNT(*) FROM lyrics_recovery_import_artifact_evidence WHERE batch_sha256=?),
		(SELECT COUNT(*) FROM lyrics_recovery_import_component_contributions WHERE batch_sha256=?)`,
		batchSHA256, batchSHA256, batchSHA256, batchSHA256, batchSHA256, batchSHA256, batchSHA256).Scan(
		&counts.BatchItems, &counts.EditableLyrics, &counts.SourceDocuments, &counts.AvailabilityDocuments,
		&counts.Artifacts, &counts.ArtifactEvidenceLinks, &counts.ComponentContributions); err != nil {
		return lyricsrecoveryimport.ImportStorageCounts{}, err
	}
	return counts, nil
}

func receiptItems(results []offlineimport.RecoveryLyricsImportItem) []lyricsrecoveryimport.ImportReceiptItem {
	items := make([]lyricsrecoveryimport.ImportReceiptItem, len(results))
	for index, result := range results {
		items[index] = lyricsrecoveryimport.ImportReceiptItem{
			MusicID: result.MusicID, State: result.State, Revision: result.Revision,
			DocumentSHA256: result.DocumentSHA256, AvailabilityDocumentSHA256: result.AvailabilityDocumentSHA256,
		}
	}
	return items
}

func pathInsideDirectory(directory, candidate string) bool {
	relative, err := filepath.Rel(directory, candidate)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func joinPinnedClose(target *error, pinned *pinnedFile) {
	if closeErr := pinned.close(); closeErr != nil {
		*target = errors.Join(*target, closeErr)
	}
}

func postCommitRecoveryError(receiptPath string, cause error) error {
	return fmt.Errorf("recovery import commit was attempted only after durable receipt preparation at %s; preserve the receipt and recovery database anchor, then verify the transactional %s audit record before retrying: %w",
		receiptPath, lyricsrecoveryimport.ImportReceiptReceiptAuditAction, cause)
}
