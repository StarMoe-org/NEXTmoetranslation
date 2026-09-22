package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lyricsimportreceipt"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/offlineimport"
	"moesekai/server/internal/singleinstance"
	"moesekai/server/internal/store"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const (
	receiptSchemaVersion     = lyricsimportreceipt.SchemaVersion
	receiptProtocol          = lyricsimportreceipt.CommitProtocol
	receiptAuditAction       = lyricsimportreceipt.DatabaseAuditAction
	sqliteStateDigestVersion = lyricsimportreceipt.StateDigestVersion
	maxBackupBytes           = int64(16 << 30)
)

var (
	canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sqliteSidecars  = [...]string{"-wal", "-shm", "-journal"}
)

type options struct {
	ValidationReceiptPath string
	RootManifestPath      string
	ManifestPath          string
	EvidenceReceiptPath   string
	DatabasePath          string
	BackupPath            string
	BackupSHA256          string
	ReceiptPath           string
	Operator              string
	ConfirmLocalOffline   bool
}

type importReceiptArtifact = lyricsimportreceipt.Artifact
type importReceiptItem = lyricsimportreceipt.Item
type importReceipt = lyricsimportreceipt.Receipt
type importReceiptAudit = lyricsimportreceipt.Audit

type sqliteSnapshotIdentity struct {
	FileSHA256  string
	StateSHA256 string
}

type executionHooks struct {
	beforeCommitValidation func() error
	afterReceiptReserve    func() error
	checkpoint             func(context.Context, *db.DB) error
}

type pinnedFile struct {
	path string
	file *os.File
	info os.FileInfo
}

type pinnedSQLiteAnchor struct {
	directory string
	path      string
}

type reservedReceipt struct {
	path            string
	name            string
	parentPath      string
	parent          *os.File
	parentInfo      os.FileInfo
	file            *os.File
	fileInfo        os.FileInfo
	durable         bool
	commitAttempted bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lyrics import stage: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout io.Writer) error {
	return runContext(context.Background(), arguments, stdout)
}

func runContext(ctx context.Context, arguments []string, stdout io.Writer) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	flags := flag.NewFlagSet("lyrics-import-stage", flag.ContinueOnError)
	if stdout == nil {
		flags.SetOutput(io.Discard)
	} else {
		flags.SetOutput(stdout)
	}
	validationReceiptPath := flags.String("validation-receipt", "", "absolute path to the immutable canonical release-validation receipt")
	rootManifestPath := flags.String("root-manifest", "", "absolute path to the exact canonical compact root bound by -validation-receipt")
	manifestPath := flags.String("manifest", "", "absolute path to the exact immutable private staging manifest bound by -validation-receipt")
	evidenceReceiptPath := flags.String("evidence-receipt", "", "absolute path to the exact immutable private evidence receipt bound by -validation-receipt")
	databasePath := flags.String("db", "", "absolute path to the offline local SQLite database to edit")
	backupPath := flags.String("backup", "", "absolute path to an existing standalone SQLite backup")
	backupSHA256 := flags.String("backup-sha256", "", "operator-acknowledged SHA-256 of -backup")
	receiptPath := flags.String("receipt", "", "absolute path for a new private no-overwrite import receipt")
	operator := flags.String("operator", "", "explicit local operator identity recorded in audit rows")
	confirm := flags.Bool("confirm-local-offline", false, "confirm that the database is offline, backed up, and not production")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	opts := options{
		ValidationReceiptPath: *validationReceiptPath, RootManifestPath: *rootManifestPath,
		ManifestPath: *manifestPath, EvidenceReceiptPath: *evidenceReceiptPath,
		DatabasePath: *databasePath, BackupPath: *backupPath,
		BackupSHA256: *backupSHA256, ReceiptPath: *receiptPath, Operator: *operator,
		ConfirmLocalOffline: *confirm,
	}
	if err := validateOptions(opts); err != nil {
		return err
	}
	if stdout == nil {
		return errors.New("stdout writer is required")
	}
	receipt, err := execute(ctx, opts)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "imported %d and replayed %d private staged lyrics drafts; batch sha256:%s; receipt:%s\n",
		receipt.ImportedCount, receipt.ReplayedCount, receipt.BatchSHA256, opts.ReceiptPath)
	return err
}

func validateOptions(opts options) error {
	if raw := strings.TrimSpace(os.Getenv("MOESEKAI_PRODUCTION")); raw != "" {
		production, err := strconv.ParseBool(raw)
		if err != nil || production {
			return errors.New("lyrics-import-stage is a local offline command and refuses MOESEKAI_PRODUCTION")
		}
	}
	if !opts.ConfirmLocalOffline {
		return errors.New("-confirm-local-offline is required")
	}
	for name, value := range map[string]string{
		"-validation-receipt": opts.ValidationReceiptPath, "-root-manifest": opts.RootManifestPath,
		"-manifest": opts.ManifestPath, "-evidence-receipt": opts.EvidenceReceiptPath,
		"-db": opts.DatabasePath, "-backup": opts.BackupPath, "-receipt": opts.ReceiptPath,
		"-operator": opts.Operator, "-backup-sha256": opts.BackupSHA256,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("%s is required without surrounding whitespace", name)
		}
	}
	for name, path := range map[string]string{
		"-validation-receipt": opts.ValidationReceiptPath, "-root-manifest": opts.RootManifestPath,
		"-manifest": opts.ManifestPath, "-evidence-receipt": opts.EvidenceReceiptPath,
		"-db": opts.DatabasePath, "-backup": opts.BackupPath, "-receipt": opts.ReceiptPath,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("%s must be a canonical absolute path", name)
		}
	}
	if len(opts.Operator) > 128 || strings.ContainsAny(opts.Operator, "\r\n") || !canonicalSHA256.MatchString(opts.BackupSHA256) {
		return errors.New("-operator must be at most 128 bytes and -backup-sha256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func execute(ctx context.Context, opts options) (importReceipt, error) {
	return executeWithHooks(ctx, opts, executionHooks{})
}

func executeWithHooks(ctx context.Context, opts options, hooks executionHooks) (receipt importReceipt, returnErr error) {
	if err := ctx.Err(); err != nil {
		return importReceipt{}, err
	}
	validationReceiptInfo, err := inspectExistingPrivateRegular(opts.ValidationReceiptPath, "release validation receipt")
	if err != nil {
		return importReceipt{}, err
	}
	rootManifestInfo, err := inspectExistingPrivateRegular(opts.RootManifestPath, "compact root manifest")
	if err != nil {
		return importReceipt{}, err
	}
	manifestInfo, err := inspectExistingPrivateRegular(opts.ManifestPath, "staging manifest")
	if err != nil {
		return importReceipt{}, err
	}
	evidenceReceiptInfo, err := inspectExistingPrivateRegular(opts.EvidenceReceiptPath, "private evidence receipt")
	if err != nil {
		return importReceipt{}, err
	}
	databaseInfo, err := inspectExistingRegular(opts.DatabasePath, "offline database")
	if err != nil {
		return importReceipt{}, err
	}
	backupInfo, err := inspectExistingRegular(opts.BackupPath, "SQLite backup")
	if err != nil {
		return importReceipt{}, err
	}
	if err := requireDistinctPaths([]namedFileInfo{
		{name: "release validation receipt", path: opts.ValidationReceiptPath, info: validationReceiptInfo},
		{name: "compact root manifest", path: opts.RootManifestPath, info: rootManifestInfo},
		{name: "staging manifest", path: opts.ManifestPath, info: manifestInfo},
		{name: "private evidence receipt", path: opts.EvidenceReceiptPath, info: evidenceReceiptInfo},
		{name: "offline database", path: opts.DatabasePath, info: databaseInfo},
		{name: "SQLite backup", path: opts.BackupPath, info: backupInfo},
	}); err != nil {
		return importReceipt{}, err
	}
	if err := validateNewReceiptPath(opts.ReceiptPath, opts.ValidationReceiptPath, opts.RootManifestPath,
		opts.ManifestPath, opts.EvidenceReceiptPath, opts.DatabasePath, opts.BackupPath); err != nil {
		return importReceipt{}, err
	}
	if err := rejectSQLiteSidecars(opts.DatabasePath, "offline database"); err != nil {
		return importReceipt{}, err
	}
	if err := rejectSQLiteSidecars(opts.BackupPath, "SQLite backup"); err != nil {
		return importReceipt{}, err
	}

	owner, err := singleinstance.Acquire(opts.DatabasePath)
	if err != nil {
		return importReceipt{}, fmt.Errorf("acquire offline database ownership: %w", err)
	}
	defer owner.Close()

	validationReceiptPinned, err := openPinnedFile(opts.ValidationReceiptPath, validationReceiptInfo, "release validation receipt")
	if err != nil {
		return importReceipt{}, err
	}
	defer func() {
		if closeErr := validationReceiptPinned.close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	validationReceiptBody, validationReceiptFileSHA, err := readPinnedFile(
		validationReceiptPinned, lyricsimportreceipt.MaxValidationReceiptBytes, "release validation receipt",
	)
	if err != nil {
		return importReceipt{}, err
	}
	validationReceipt, err := lyricsimportreceipt.DecodeValidationReceipt(validationReceiptBody, opts.ValidationReceiptPath)
	if err != nil {
		return importReceipt{}, err
	}

	rootManifestPinned, err := openPinnedFile(opts.RootManifestPath, rootManifestInfo, "compact root manifest")
	if err != nil {
		return importReceipt{}, err
	}
	defer func() {
		if closeErr := rootManifestPinned.close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	rootManifestBody, rootManifestFileSHA, err := readPinnedFile(
		rootManifestPinned, lyricsimportreceipt.MaxCompactRootBytes, "compact root manifest",
	)
	if err != nil {
		return importReceipt{}, err
	}
	rootManifest, err := lyricsrootmanifest.DecodeCanonical(rootManifestBody)
	if err != nil {
		return importReceipt{}, err
	}

	manifestPinned, err := openPinnedFile(opts.ManifestPath, manifestInfo, "staging manifest")
	if err != nil {
		return importReceipt{}, err
	}
	defer func() {
		if closeErr := manifestPinned.close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	manifestBody, manifestFileSHA, err := readPinnedFile(manifestPinned, lyricsstaging.MaxManifestBytes, "staging manifest")
	if err != nil {
		return importReceipt{}, err
	}
	manifest, err := lyricsstaging.DecodeManifest(manifestBody)
	if err != nil {
		return importReceipt{}, err
	}
	evidenceReceiptPinned, err := openPinnedFile(opts.EvidenceReceiptPath, evidenceReceiptInfo, "private evidence receipt")
	if err != nil {
		return importReceipt{}, err
	}
	defer func() {
		if closeErr := evidenceReceiptPinned.close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	evidenceReceiptBody, evidenceReceiptFileSHA, err := readPinnedFile(
		evidenceReceiptPinned, lyricsstaging.MaxPrivateEvidenceReceiptBytes, "private evidence receipt",
	)
	if err != nil {
		return importReceipt{}, err
	}
	evidenceReceipt, err := lyricsstaging.DecodePrivateEvidenceReceipt(evidenceReceiptBody)
	if err != nil {
		return importReceipt{}, err
	}
	validatedBinding, err := lyricsimportreceipt.ValidateBundleInputs(lyricsimportreceipt.BundleInputs{
		ValidationPath: opts.ValidationReceiptPath, ValidationFileSHA256: validationReceiptFileSHA,
		ValidationFileBytes: int64(len(validationReceiptBody)), Validation: validationReceipt,
		RootPath: opts.RootManifestPath, RootFileSHA256: rootManifestFileSHA,
		RootFileBytes: int64(len(rootManifestBody)), Root: rootManifest,
		ManifestPath: opts.ManifestPath, ManifestFileSHA256: manifestFileSHA,
		ManifestFileBytes: int64(len(manifestBody)), Manifest: manifest,
		EvidencePath: opts.EvidenceReceiptPath, EvidenceFileSHA256: evidenceReceiptFileSHA,
		EvidenceFileBytes: int64(len(evidenceReceiptBody)), Evidence: evidenceReceipt,
		ImportReceiptPath: opts.ReceiptPath,
	})
	if err != nil {
		return importReceipt{}, err
	}

	databasePinned, err := openPinnedWritableFile(opts.DatabasePath, databaseInfo, "offline database")
	if err != nil {
		return importReceipt{}, err
	}
	defer func() {
		if closeErr := databasePinned.close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	backupPinned, err := openPinnedFile(opts.BackupPath, backupInfo, "SQLite backup")
	if err != nil {
		return importReceipt{}, err
	}
	defer func() {
		if closeErr := backupPinned.close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()

	databaseIdentity, err := verifyPinnedSQLiteSnapshot(ctx, databasePinned, "offline database")
	if err != nil {
		return importReceipt{}, err
	}
	backupIdentity, err := verifyPinnedSQLiteSnapshot(ctx, backupPinned, "SQLite backup")
	if err != nil {
		return importReceipt{}, err
	}
	if backupIdentity.FileSHA256 != opts.BackupSHA256 {
		return importReceipt{}, errors.New("-backup-sha256 does not match the verified SQLite backup bytes")
	}
	if backupIdentity.StateSHA256 != databaseIdentity.StateSHA256 {
		return importReceipt{}, errors.New("SQLite backup logical state does not match the exact pre-import offline database")
	}
	if err := rejectSQLiteSidecars(opts.DatabasePath, "offline database after state verification"); err != nil {
		return importReceipt{}, err
	}
	if err := rejectSQLiteSidecars(opts.BackupPath, "SQLite backup after state verification"); err != nil {
		return importReceipt{}, err
	}
	confirmedDatabaseDigest, err := digestPinnedFile(databasePinned, maxBackupBytes, "offline database")
	if err != nil {
		return importReceipt{}, err
	}
	if confirmedDatabaseDigest != databaseIdentity.FileSHA256 {
		return importReceipt{}, errors.New("offline database changed after backup-state binding")
	}

	databaseAnchor, err := createPinnedSQLiteAnchor(databasePinned)
	if err != nil {
		return importReceipt{}, err
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
		return importReceipt{}, err
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
		return importReceipt{}, err
	}
	if err := databasePinned.verifySamePath("after database open", false); err != nil {
		return importReceipt{}, err
	}

	defer func() {
		if receiptReservation == nil {
			return
		}
		if closeErr := receiptReservation.finish(); closeErr != nil {
			if receiptReservation.commitAttempted {
				closeErr = postCommitReceiptError(opts.ReceiptPath, closeErr)
			}
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()

	// The commit hook is the protocol boundary: it records the exact receipt in
	// the importing transaction, then publishes and fsyncs the immutable external
	// copy. Store invokes SQLite Commit as the next database operation.
	results, commitAttempted, importErr := offlineimport.ImportStagedLyricsManifestWithEvidenceReceiptAndCommitHook(
		ctx, store.New(database), manifest, evidenceReceipt, opts.Operator,
		func(tx *sql.Tx, results []offlineimport.StagedLyricsImportItem) error {
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
				{validationReceiptPinned, validationReceiptFileSHA, lyricsimportreceipt.MaxValidationReceiptBytes, "release validation receipt"},
				{rootManifestPinned, rootManifestFileSHA, lyricsimportreceipt.MaxCompactRootBytes, "compact root manifest"},
				{manifestPinned, manifestFileSHA, lyricsstaging.MaxManifestBytes, "staging manifest"},
				{evidenceReceiptPinned, evidenceReceiptFileSHA, lyricsstaging.MaxPrivateEvidenceReceiptBytes, "private evidence receipt"},
			} {
				if err := verifyPinnedImmutableDigest(input.pinned, input.expected, input.maximum, input.label); err != nil {
					return err
				}
			}
			if err := backupPinned.verifySamePath("before receipt preparation", true); err != nil {
				return fmt.Errorf("SQLite backup: %w", err)
			}
			confirmedBackupDigest, err := digestPinnedFile(backupPinned, maxBackupBytes, "SQLite backup")
			if err != nil {
				return err
			}
			if confirmedBackupDigest != backupIdentity.FileSHA256 {
				return errors.New("SQLite backup changed before receipt preparation")
			}
			if err := rejectSQLiteSidecars(opts.DatabasePath, "offline database before commit"); err != nil {
				return err
			}
			if err := rejectSQLiteSidecars(opts.BackupPath, "SQLite backup before commit"); err != nil {
				return err
			}
			if err := validateNewReceiptPath(opts.ReceiptPath, opts.ValidationReceiptPath, opts.RootManifestPath,
				opts.ManifestPath, opts.EvidenceReceiptPath, opts.DatabasePath, opts.BackupPath); err != nil {
				return err
			}

			receipt, err = buildImportReceipt(opts, databaseAnchor.path, validatedBinding, manifest,
				databaseIdentity, backupIdentity, results)
			if err != nil {
				return err
			}
			receiptBody, err := lyricsimportreceipt.MarshalBound(receipt, validatedBinding)
			if err != nil {
				return err
			}
			receiptDigest := sha256.Sum256(receiptBody)
			auditBody, err := json.Marshal(importReceiptAudit{
				SchemaVersion: 1,
				ReceiptPath:   opts.ReceiptPath,
				ReceiptSHA256: hex.EncodeToString(receiptDigest[:]),
				ReceiptJSON:   string(receiptBody),
			})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,?,?)`,
				time.Now().Unix(), opts.Operator, receiptAuditAction, string(auditBody)); err != nil {
				return fmt.Errorf("record transactional import receipt: %w", err)
			}

			receiptReservation, err = reserveReceipt(opts.ReceiptPath)
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
			// Once the prepared receipt is durable it must survive every later
			// outcome, including cancellation or an ambiguous/panicking commit.
			receiptReservation.commitAttempted = true
			return nil
		},
	)
	if receiptReservation != nil {
		receiptReservation.commitAttempted = receiptReservation.commitAttempted || commitAttempted
	}
	if importErr != nil {
		if commitAttempted {
			return receipt, postCommitReceiptError(opts.ReceiptPath, fmt.Errorf("commit staged import: %w", importErr))
		}
		return importReceipt{}, importErr
	}
	_ = results

	checkpoint := hooks.checkpoint
	if checkpoint == nil {
		checkpoint = func(ctx context.Context, database *db.DB) error { return database.Checkpoint(ctx) }
	}
	if err := checkpoint(ctx, database); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, fmt.Errorf("checkpoint imported database: %w", err))
	}
	if err := databaseAnchor.verifyPinned(databasePinned, "after staged import"); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := databasePinned.verifySamePath("after staged import", false); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := receiptReservation.verify("after staged import"); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := database.Close(); err != nil {
		databaseOpen = false
		return receipt, postCommitReceiptError(opts.ReceiptPath, fmt.Errorf("close imported database: %w", err))
	}
	databaseOpen = false
	if err := databaseAnchor.verifyPinned(databasePinned, "after database close"); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := databasePinned.verifySamePath("after database close", false); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := rejectSQLiteSidecars(opts.DatabasePath, "imported database"); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := backupPinned.verifySamePath("after staged import", true); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, fmt.Errorf("SQLite backup: %w", err))
	}
	if err := rejectSQLiteSidecars(opts.BackupPath, "SQLite backup after staged import"); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := receiptReservation.verify("after database close"); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	if err := databaseAnchor.close(); err != nil {
		return receipt, postCommitReceiptError(opts.ReceiptPath, err)
	}
	return receipt, nil
}

func buildImportReceipt(
	opts options,
	recoveryDatabasePath string,
	binding lyricsimportreceipt.Binding,
	manifest lyricsstaging.Manifest,
	databaseIdentity sqliteSnapshotIdentity,
	backupIdentity sqliteSnapshotIdentity,
	results []offlineimport.StagedLyricsImportItem,
) (importReceipt, error) {
	items := make([]importReceiptItem, len(results))
	stagedByMusicID := make(map[int]lyricsstaging.Draft, len(manifest.Items))
	for _, staged := range manifest.Items {
		stagedByMusicID[staged.MusicID] = staged
	}
	for index, result := range results {
		staged := stagedByMusicID[result.MusicID]
		fullRenditionKey := staged.Document.Provenance.FullText.RenditionKey
		sourceFetchedAt := ""
		for _, identity := range staged.Document.FixedIdentities {
			if identity.RenditionKey == fullRenditionKey {
				sourceFetchedAt = identity.FetchedAt
				break
			}
		}
		artifacts := make([]importReceiptArtifact, len(staged.Artifacts))
		for artifactIndex, artifact := range staged.Artifacts {
			artifacts[artifactIndex] = importReceiptArtifact{RenditionKey: artifact.Identity.RenditionKey,
				ArtifactSHA256: artifact.ArtifactSHA256}
		}
		items[index] = importReceiptItem{
			MusicID: result.MusicID, Revision: result.Lyrics.Revision, Changed: result.Changed,
			DocumentSHA256: staged.DocumentSHA256, FullTextRenditionKey: fullRenditionKey,
			SourceFetchedAt: sourceFetchedAt, Artifacts: artifacts,
		}
	}
	return lyricsimportreceipt.New(binding, lyricsimportreceipt.Metadata{
		BackupSHA256:                 backupIdentity.FileSHA256,
		BackupStateSHA256:            backupIdentity.StateSHA256,
		PreImportDatabaseSHA256:      databaseIdentity.FileSHA256,
		PreImportDatabaseStateSHA256: databaseIdentity.StateSHA256,
		DatabasePath:                 opts.DatabasePath,
		RecoveryDatabasePath:         recoveryDatabasePath,
		Operator:                     opts.Operator,
		Items:                        items,
		PreparedAt:                   time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func postCommitReceiptError(receiptPath string, cause error) error {
	return fmt.Errorf("staged import commit was attempted only after durable receipt preparation at %s; verify the transactional %s audit record before retrying: %w",
		receiptPath, receiptAuditAction, cause)
}

type namedFileInfo struct {
	name string
	path string
	info os.FileInfo
}

func inspectExistingRegular(path, label string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a direct regular file, not a symlink or special file", label)
	}
	return info, nil
}

func inspectExistingPrivateRegular(path, label string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%s path must be canonical and absolute", label)
	}
	info, err := inspectExistingRegular(path, label)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%s must be an effective-UID-owned single-link mode-0600 regular file", label)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, fmt.Errorf("%s path must not traverse symlinks or filesystem aliases", label)
	}
	return info, nil
}

func requireDistinctPaths(files []namedFileInfo) error {
	for left := range files {
		for right := left + 1; right < len(files); right++ {
			if filepath.Clean(files[left].path) == filepath.Clean(files[right].path) || os.SameFile(files[left].info, files[right].info) {
				return fmt.Errorf("%s and %s must be distinct files", files[left].name, files[right].name)
			}
		}
	}
	return nil
}

func validateNewReceiptPath(receiptPath string, inputPaths ...string) error {
	parent := filepath.Dir(receiptPath)
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect receipt directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("receipt parent must be a direct existing directory")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || filepath.Clean(resolvedParent) != filepath.Clean(parent) {
		return errors.New("receipt parent must not resolve through symlinks")
	}
	for _, input := range inputPaths {
		if filepath.Clean(receiptPath) == filepath.Clean(input) {
			return errors.New("receipt path must be distinct from every input")
		}
	}
	if _, err := os.Lstat(receiptPath); err == nil {
		return errors.New("import receipt path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect import receipt path: %w", err)
	}
	return nil
}

func reserveReceipt(path string) (*reservedReceipt, error) {
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return nil, errors.New("import receipt must name a file inside its parent directory")
	}
	parent, err := os.Open(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open receipt directory: %w", err)
	}
	reservation := &reservedReceipt{path: path, name: name, parentPath: parentPath, parent: parent}
	cleanup := func(cause error) (*reservedReceipt, error) {
		if closeErr := parent.Close(); closeErr != nil {
			cause = errors.Join(cause, closeErr)
		}
		return nil, cause
	}
	parentInfo, err := parent.Stat()
	if err != nil {
		return cleanup(fmt.Errorf("inspect opened receipt directory: %w", err))
	}
	reservation.parentInfo = parentInfo
	if err := reservation.verifyParent("while reserving receipt"); err != nil {
		return cleanup(err)
	}
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return cleanup(fmt.Errorf("reserve no-overwrite import receipt: %w", err))
	}
	reservation.file = os.NewFile(uintptr(fd), path)
	if reservation.file == nil {
		_ = unix.Close(fd)
		return cleanup(errors.New("reserve no-overwrite import receipt: invalid file descriptor"))
	}
	if err := reservation.file.Chmod(0o600); err != nil {
		_ = reservation.finish()
		return nil, fmt.Errorf("secure import receipt: %w", err)
	}
	fileInfo, err := reservation.file.Stat()
	if err != nil {
		_ = reservation.finish()
		return nil, fmt.Errorf("inspect reserved import receipt: %w", err)
	}
	reservation.fileInfo = fileInfo
	if err := reservation.verify("after receipt reservation"); err != nil {
		_ = reservation.finish()
		return nil, err
	}
	return reservation, nil
}

func (receipt *reservedReceipt) publish(body []byte) error {
	if receipt == nil || receipt.file == nil || receipt.durable {
		return errors.New("import receipt reservation is not writable")
	}
	if len(body) == 0 {
		return errors.New("import receipt body is empty")
	}
	if err := receipt.verify("before receipt publication"); err != nil {
		return err
	}
	for len(body) > 0 {
		written, err := receipt.file.Write(body)
		if err != nil {
			return fmt.Errorf("write import receipt: %w", err)
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	if err := receipt.file.Sync(); err != nil {
		return fmt.Errorf("sync import receipt: %w", err)
	}
	if err := receipt.parent.Sync(); err != nil {
		return fmt.Errorf("sync import receipt directory: %w", err)
	}
	if err := receipt.verify("after durable receipt publication"); err != nil {
		return err
	}
	receipt.durable = true
	return nil
}

func (receipt *reservedReceipt) verifyParent(stage string) error {
	if receipt == nil || receipt.parent == nil || receipt.parentInfo == nil {
		return errors.New("receipt directory is not pinned")
	}
	opened, err := receipt.parent.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(receipt.parentPath)
	if err != nil {
		return fmt.Errorf("inspect receipt directory %s: %w", stage, err)
	}
	if !opened.IsDir() || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(receipt.parentInfo, opened) || !os.SameFile(opened, current) {
		return fmt.Errorf("receipt directory path or inode changed %s", stage)
	}
	return nil
}

func (receipt *reservedReceipt) verify(stage string) error {
	if receipt == nil || receipt.file == nil || receipt.fileInfo == nil {
		return errors.New("import receipt is not reserved")
	}
	if err := receipt.verifyParent(stage); err != nil {
		return err
	}
	opened, err := receipt.file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(receipt.path)
	if err != nil {
		return fmt.Errorf("inspect import receipt %s: %w", stage, err)
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(receipt.fileInfo, opened) || !os.SameFile(opened, current) {
		return fmt.Errorf("import receipt path or inode changed %s", stage)
	}
	if opened.Mode().Perm() != 0o600 || current.Mode().Perm() != 0o600 {
		return fmt.Errorf("import receipt permissions changed %s", stage)
	}
	return nil
}

func (receipt *reservedReceipt) finish() error {
	if receipt == nil {
		return nil
	}
	var result error
	if !receipt.commitAttempted && receipt.file != nil {
		if err := receipt.verify("before aborted receipt cleanup"); err == nil {
			if unlinkErr := unix.Unlinkat(int(receipt.parent.Fd()), receipt.name, 0); unlinkErr != nil {
				result = errors.Join(result, fmt.Errorf("remove aborted import receipt: %w", unlinkErr))
			} else if syncErr := receipt.parent.Sync(); syncErr != nil {
				result = errors.Join(result, fmt.Errorf("sync aborted receipt cleanup: %w", syncErr))
			}
		} else {
			result = errors.Join(result, err)
		}
	}
	if receipt.file != nil {
		if err := receipt.file.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close import receipt: %w", err))
		}
		receipt.file = nil
	}
	if receipt.parent != nil {
		if err := receipt.parent.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close import receipt directory: %w", err))
		}
		receipt.parent = nil
	}
	return result
}

func rejectSQLiteSidecars(path, label string) error {
	for _, suffix := range sqliteSidecars {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("%s must be a standalone offline SQLite file: %s sidecar exists", label, suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s %s sidecar: %w", label, suffix, err)
		}
	}
	return nil
}

func openPinnedFile(path string, inspected os.FileInfo, label string) (*pinnedFile, error) {
	return openPinnedFileWithFlags(path, inspected, label, os.O_RDONLY)
}

func openPinnedWritableFile(path string, inspected os.FileInfo, label string) (*pinnedFile, error) {
	return openPinnedFileWithFlags(path, inspected, label, os.O_RDWR)
}

func openPinnedFileWithFlags(path string, inspected os.FileInfo, label string, flags int) (*pinnedFile, error) {
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	pinned := &pinnedFile{path: path, file: file, info: inspected}
	if err := pinned.verifySamePath("between inspection and open", true); err != nil {
		file.Close()
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return pinned, nil
}

func (pinned *pinnedFile) verifySamePath(stage string, immutable bool) error {
	if pinned == nil || pinned.file == nil || pinned.info == nil {
		return errors.New("pinned file is not active")
	}
	opened, err := pinned.file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(pinned.path)
	if err != nil {
		return fmt.Errorf("path or inode changed %s: %w", stage, err)
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(pinned.info, opened) || !os.SameFile(opened, current) {
		return fmt.Errorf("path or inode changed %s", stage)
	}
	if immutable && (opened.Size() != pinned.info.Size() || current.Size() != pinned.info.Size() ||
		!opened.ModTime().Equal(pinned.info.ModTime()) || !current.ModTime().Equal(pinned.info.ModTime())) {
		return fmt.Errorf("size or modification time changed %s", stage)
	}
	return nil
}

// createPinnedSQLiteAnchor gives SQLite a private stable pathname that is a
// verified hard link to the already-open database inode. SQLite may then
// create and checkpoint its normal sidecars beside the anchor without ever
// reopening the operator-supplied pathname.
func createPinnedSQLiteAnchor(pinned *pinnedFile) (*pinnedSQLiteAnchor, error) {
	if err := pinned.verifySamePath("before creating SQLite anchor", false); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(filepath.Dir(pinned.path), ".lyrics-import-stage-")
	if err != nil {
		return nil, fmt.Errorf("create private SQLite anchor directory: %w", err)
	}
	anchor := &pinnedSQLiteAnchor{directory: directory, path: filepath.Join(directory, "database.sqlite")}
	cleanup := func(cause error) (*pinnedSQLiteAnchor, error) {
		if removeErr := anchor.close(); removeErr != nil {
			return nil, errors.Join(cause, removeErr)
		}
		return nil, cause
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return cleanup(fmt.Errorf("secure private SQLite anchor directory: %w", err))
	}
	if err := os.Link(pinned.path, anchor.path); err != nil {
		return cleanup(fmt.Errorf("hard-link pinned SQLite database: %w", err))
	}
	if err := anchor.verifyPinned(pinned, "while creating SQLite anchor"); err != nil {
		return cleanup(err)
	}
	if err := pinned.verifySamePath("after creating SQLite anchor", false); err != nil {
		return cleanup(err)
	}
	return anchor, nil
}

func (anchor *pinnedSQLiteAnchor) verifyPinned(pinned *pinnedFile, stage string) error {
	if anchor == nil || anchor.directory == "" || anchor.path == "" || pinned == nil || pinned.file == nil {
		return errors.New("pinned SQLite anchor is not active")
	}
	directoryInfo, err := os.Lstat(anchor.directory)
	if err != nil {
		return fmt.Errorf("inspect private SQLite anchor directory: %w", err)
	}
	opened, err := pinned.file.Stat()
	if err != nil {
		return err
	}
	linked, err := os.Lstat(anchor.path)
	if err != nil {
		return fmt.Errorf("inspect private SQLite anchor: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 || !opened.Mode().IsRegular() ||
		!linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return fmt.Errorf("pinned SQLite anchor changed %s", stage)
	}
	return nil
}

func (anchor *pinnedSQLiteAnchor) close() error {
	if anchor == nil || anchor.directory == "" {
		return nil
	}
	directory := anchor.directory
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove private SQLite anchor directory: %w", err)
	}
	anchor.directory = ""
	anchor.path = ""
	return nil
}

func (pinned *pinnedFile) close() error {
	if pinned == nil || pinned.file == nil {
		return nil
	}
	err := pinned.file.Close()
	pinned.file = nil
	return err
}

func readPinnedFile(pinned *pinnedFile, maximum int, label string) ([]byte, string, error) {
	if pinned.info.Size() <= 0 || pinned.info.Size() > int64(maximum) {
		return nil, "", fmt.Errorf("%s must contain between 1 and %d bytes", label, maximum)
	}
	if _, err := pinned.file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	body, err := io.ReadAll(io.LimitReader(pinned.file, int64(maximum)+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maximum || int64(len(body)) != pinned.info.Size() {
		return nil, "", fmt.Errorf("%s changed while being read", label)
	}
	first := sha256.Sum256(body)
	if err := pinned.verifySamePath("while being read", true); err != nil {
		return nil, "", err
	}
	if _, err := pinned.file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	secondBody, err := io.ReadAll(io.LimitReader(pinned.file, int64(maximum)+1))
	if err != nil {
		return nil, "", err
	}
	second := sha256.Sum256(secondBody)
	if !bytes.Equal(body, secondBody) || first != second {
		return nil, "", fmt.Errorf("%s bytes changed during verification", label)
	}
	if err := pinned.verifySamePath("after verification", true); err != nil {
		return nil, "", err
	}
	return body, hex.EncodeToString(first[:]), nil
}

func digestPinnedFile(pinned *pinnedFile, maximum int64, label string) (string, error) {
	if pinned.info.Size() <= 0 || pinned.info.Size() > maximum {
		return "", fmt.Errorf("%s must contain between 1 and %d bytes", label, maximum)
	}
	digestOnce := func() ([sha256.Size]byte, error) {
		if _, err := pinned.file.Seek(0, io.SeekStart); err != nil {
			return [sha256.Size]byte{}, err
		}
		hasher := sha256.New()
		read, err := io.Copy(hasher, io.LimitReader(pinned.file, maximum+1))
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		if read != pinned.info.Size() {
			return [sha256.Size]byte{}, fmt.Errorf("%s changed while being hashed", label)
		}
		var digest [sha256.Size]byte
		copy(digest[:], hasher.Sum(nil))
		return digest, nil
	}
	first, err := digestOnce()
	if err != nil {
		return "", err
	}
	if err := pinned.verifySamePath("while being hashed", true); err != nil {
		return "", err
	}
	second, err := digestOnce()
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("%s bytes changed during verification", label)
	}
	if err := pinned.verifySamePath("after hash verification", true); err != nil {
		return "", err
	}
	return hex.EncodeToString(first[:]), nil
}

func verifyPinnedImmutableDigest(pinned *pinnedFile, expected string, maximum int64, label string) error {
	if err := pinned.verifySamePath("before receipt preparation", true); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	digest, err := digestPinnedFile(pinned, maximum, label)
	if err != nil {
		return err
	}
	if digest != expected {
		return fmt.Errorf("%s changed after validated bundle authorization", label)
	}
	return nil
}

func verifyPinnedSQLiteSnapshot(ctx context.Context, pinned *pinnedFile, label string) (identity sqliteSnapshotIdentity, returnErr error) {
	fileDigest, err := digestPinnedFile(pinned, maxBackupBytes, label)
	if err != nil {
		return sqliteSnapshotIdentity{}, err
	}
	descriptorPath := fmt.Sprintf("/dev/fd/%d", pinned.file.Fd())
	databaseURL := &url.URL{Scheme: "file", Path: descriptorPath}
	query := databaseURL.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "trusted_schema(0)")
	databaseURL.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return sqliteSnapshotIdentity{}, err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	databaseOpen := true
	defer func() {
		if databaseOpen {
			if closeErr := database.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	if err := verifySQLiteIntegrity(ctx, database, label); err != nil {
		return sqliteSnapshotIdentity{}, err
	}
	stateDigest, err := sqliteLogicalStateDigest(ctx, database)
	if err != nil {
		return sqliteSnapshotIdentity{}, fmt.Errorf("digest %s logical state: %w", label, err)
	}
	if err := database.Close(); err != nil {
		databaseOpen = false
		return sqliteSnapshotIdentity{}, fmt.Errorf("close %s logical-state reader: %w", label, err)
	}
	databaseOpen = false
	finalFileDigest, err := digestPinnedFile(pinned, maxBackupBytes, label)
	if err != nil {
		return sqliteSnapshotIdentity{}, err
	}
	if finalFileDigest != fileDigest {
		return sqliteSnapshotIdentity{}, fmt.Errorf("%s bytes changed during SQLite logical-state verification", label)
	}
	if err := pinned.verifySamePath("after SQLite logical-state verification", true); err != nil {
		return sqliteSnapshotIdentity{}, fmt.Errorf("%s: %w", label, err)
	}
	return sqliteSnapshotIdentity{FileSHA256: fileDigest, StateSHA256: stateDigest}, nil
}

// sqliteLogicalStateDigest hashes persistent header state, the complete main
// schema, column metadata, and every table value in logical rowid/primary-key
// order. It deliberately excludes page numbers, freelists, change counters,
// page size, and schema cookies so equivalent SQLite backup representations
// produce the same digest without omitting rollback-relevant logical state.
func sqliteLogicalStateDigest(ctx context.Context, database *sql.DB) (string, error) {
	hasher := sha256.New()
	if err := writeDigestValue(hasher, sqliteStateDigestVersion); err != nil {
		return "", err
	}
	for _, pragma := range []string{"application_id", "user_version", "encoding", "auto_vacuum"} {
		var value any
		if err := database.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&value); err != nil {
			return "", fmt.Errorf("read pragma %s: %w", pragma, err)
		}
		if err := writeDigestValue(hasher, pragma); err != nil {
			return "", err
		}
		if err := writeDigestValue(hasher, value); err != nil {
			return "", err
		}
	}

	schemaRows, err := database.QueryContext(ctx, `SELECT type,name,tbl_name,sql
		FROM sqlite_schema ORDER BY type,name,tbl_name,COALESCE(sql,'')`)
	if err != nil {
		return "", err
	}
	for schemaRows.Next() {
		var objectType, name, tableName string
		var statement sql.NullString
		if err := schemaRows.Scan(&objectType, &name, &tableName, &statement); err != nil {
			schemaRows.Close()
			return "", err
		}
		for _, value := range []any{"schema", objectType, name, tableName, statement} {
			if err := writeDigestValue(hasher, value); err != nil {
				schemaRows.Close()
				return "", err
			}
		}
	}
	if err := schemaRows.Err(); err != nil {
		schemaRows.Close()
		return "", err
	}
	if err := schemaRows.Close(); err != nil {
		return "", err
	}

	tableRows, err := database.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
	if err != nil {
		return "", err
	}
	var tableNames []string
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			tableRows.Close()
			return "", err
		}
		tableNames = append(tableNames, name)
	}
	if err := tableRows.Err(); err != nil {
		tableRows.Close()
		return "", err
	}
	if err := tableRows.Close(); err != nil {
		return "", err
	}
	for _, tableName := range tableNames {
		if err := digestSQLiteTable(ctx, database, hasher, tableName); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type sqliteLogicalColumn struct {
	CID          int
	Name         string
	DeclaredType string
	NotNull      int
	DefaultValue sql.NullString
	PrimaryKey   int
	Hidden       int
}

func digestSQLiteTable(ctx context.Context, database *sql.DB, writer io.Writer, tableName string) error {
	columnRows, err := database.QueryContext(ctx, `SELECT cid,name,type,"notnull",dflt_value,pk,hidden
		FROM pragma_table_xinfo(?) ORDER BY cid`, tableName)
	if err != nil {
		return fmt.Errorf("inspect table %q columns: %w", tableName, err)
	}
	var columns []sqliteLogicalColumn
	for columnRows.Next() {
		var column sqliteLogicalColumn
		if err := columnRows.Scan(&column.CID, &column.Name, &column.DeclaredType, &column.NotNull,
			&column.DefaultValue, &column.PrimaryKey, &column.Hidden); err != nil {
			columnRows.Close()
			return err
		}
		columns = append(columns, column)
	}
	if err := columnRows.Err(); err != nil {
		columnRows.Close()
		return err
	}
	if err := columnRows.Close(); err != nil {
		return err
	}
	if len(columns) == 0 {
		return fmt.Errorf("table %q has no queryable columns", tableName)
	}
	if err := writeDigestValue(writer, "table"); err != nil {
		return err
	}
	if err := writeDigestValue(writer, tableName); err != nil {
		return err
	}
	for _, column := range columns {
		for _, value := range []any{
			"column", int64(column.CID), column.Name, column.DeclaredType, int64(column.NotNull),
			column.DefaultValue, int64(column.PrimaryKey), int64(column.Hidden),
		} {
			if err := writeDigestValue(writer, value); err != nil {
				return err
			}
		}
	}

	var withoutRowID int
	if err := database.QueryRowContext(ctx,
		`SELECT wr FROM pragma_table_list WHERE schema='main' AND name=?`, tableName).Scan(&withoutRowID); err != nil {
		return fmt.Errorf("inspect table %q rowid policy: %w", tableName, err)
	}
	selectColumns := make([]string, 0, len(columns)+1)
	orderColumns := []string{}
	if withoutRowID == 0 {
		rowIDName, err := unshadowedSQLiteRowIDName(columns)
		if err != nil {
			return fmt.Errorf("table %q: %w", tableName, err)
		}
		selectColumns = append(selectColumns, quoteSQLiteIdentifier(rowIDName))
		orderColumns = append(orderColumns, quoteSQLiteIdentifier(rowIDName))
	} else {
		primaryKey := append([]sqliteLogicalColumn(nil), columns...)
		sort.Slice(primaryKey, func(left, right int) bool {
			leftPK, rightPK := primaryKey[left].PrimaryKey, primaryKey[right].PrimaryKey
			if leftPK == 0 {
				leftPK = math.MaxInt
			}
			if rightPK == 0 {
				rightPK = math.MaxInt
			}
			return leftPK < rightPK
		})
		for _, column := range primaryKey {
			if column.PrimaryKey > 0 {
				orderColumns = append(orderColumns, quoteSQLiteIdentifier(column.Name))
			}
		}
		if len(orderColumns) == 0 {
			return fmt.Errorf("WITHOUT ROWID table %q has no primary key", tableName)
		}
	}
	for _, column := range columns {
		selectColumns = append(selectColumns, quoteSQLiteIdentifier(column.Name))
	}
	query := "SELECT " + strings.Join(selectColumns, ",") + " FROM " + quoteSQLiteIdentifier(tableName) +
		" ORDER BY " + strings.Join(orderColumns, ",")
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("read table %q: %w", tableName, err)
	}
	valueCount := len(selectColumns)
	values := make([]any, valueCount)
	destinations := make([]any, valueCount)
	for index := range values {
		destinations[index] = &values[index]
	}
	var rowCount int64
	for rows.Next() {
		for index := range values {
			values[index] = nil
		}
		if err := rows.Scan(destinations...); err != nil {
			rows.Close()
			return err
		}
		if err := writeDigestValue(writer, "row"); err != nil {
			rows.Close()
			return err
		}
		for _, value := range values {
			if err := writeDigestValue(writer, value); err != nil {
				rows.Close()
				return err
			}
		}
		rowCount++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return writeDigestValue(writer, rowCount)
}

func unshadowedSQLiteRowIDName(columns []sqliteLogicalColumn) (string, error) {
	shadowed := make(map[string]bool, len(columns))
	for _, column := range columns {
		shadowed[strings.ToLower(column.Name)] = true
	}
	for _, candidate := range []string{"rowid", "_rowid_", "oid"} {
		if !shadowed[candidate] {
			return candidate, nil
		}
	}
	return "", errors.New("all SQLite rowid aliases are shadowed")
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func writeDigestValue(writer io.Writer, value any) error {
	var tag byte
	var body []byte
	switch typed := value.(type) {
	case nil:
		tag = 'n'
	case sql.NullString:
		if !typed.Valid {
			tag = 'n'
		} else {
			tag = 't'
			body = []byte(typed.String)
		}
	case int:
		tag = 'i'
		body = make([]byte, 8)
		binary.BigEndian.PutUint64(body, uint64(int64(typed)))
	case int64:
		tag = 'i'
		body = make([]byte, 8)
		binary.BigEndian.PutUint64(body, uint64(typed))
	case float64:
		tag = 'f'
		body = make([]byte, 8)
		binary.BigEndian.PutUint64(body, math.Float64bits(typed))
	case string:
		tag = 't'
		body = []byte(typed)
	case []byte:
		tag = 'b'
		body = typed
	default:
		return fmt.Errorf("unsupported SQLite digest value type %T", value)
	}
	var header [9]byte
	header[0] = tag
	binary.BigEndian.PutUint64(header[1:], uint64(len(body)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := writer.Write(body); err != nil {
			return err
		}
	}
	return nil
}

func verifySQLiteIntegrity(ctx context.Context, database *sql.DB, label string) error {
	rows, err := database.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("verify %s SQLite integrity: %w", label, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		count++
		if result != "ok" {
			return fmt.Errorf("%s SQLite integrity check: %s", label, result)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%s SQLite integrity check returned %d rows", label, count)
	}
	return nil
}
