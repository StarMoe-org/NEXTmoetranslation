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
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"moesekai/server/offline/internal/lyricsrecoveryimport"
)

const (
	sqliteStateDigestVersion    = "moesekai-recovery-logical-state-v1"
	protectedStateDigestVersion = "moesekai-recovery-protected-state-v2"
	maxBackupBytes              = int64(16 << 30)
)

var sqliteSidecars = [...]string{"-wal", "-shm", "-journal"}

type sqliteSnapshotIdentity struct {
	FileSHA256           string
	StateSHA256          string
	ProtectedStateSHA256 string
	AuditMaxID           int64
	CatalogCount         int
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
	directory, err := os.MkdirTemp(filepath.Dir(pinned.path), ".lyrics-recovery-import-")
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

func verifyPinnedSQLiteSnapshot(ctx context.Context, pinned *pinnedFile, label string, scopes ...recoveryProtectedScope) (identity sqliteSnapshotIdentity, returnErr error) {
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
	var auditMaxID int64
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM audit_log`).Scan(&auditMaxID); err != nil {
		return sqliteSnapshotIdentity{}, fmt.Errorf("read %s audit boundary: %w", label, err)
	}
	var catalogCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_music`).Scan(&catalogCount); err != nil {
		return sqliteSnapshotIdentity{}, fmt.Errorf("read %s catalog count: %w", label, err)
	}
	stateDigest, err := sqliteLogicalStateDigest(ctx, database)
	if err != nil {
		return sqliteSnapshotIdentity{}, fmt.Errorf("digest %s logical state: %w", label, err)
	}
	protectedDigest, err := sqliteProtectedStateDigest(ctx, database, auditMaxID, scopes...)
	if err != nil {
		return sqliteSnapshotIdentity{}, fmt.Errorf("digest %s protected state: %w", label, err)
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
	return sqliteSnapshotIdentity{
		FileSHA256: fileDigest, StateSHA256: stateDigest,
		ProtectedStateSHA256: protectedDigest, AuditMaxID: auditMaxID, CatalogCount: catalogCount,
	}, nil
}

// sqliteLogicalStateDigest hashes persistent header state, the complete main
// schema, and every non-audit table value in logical rowid/primary-key order.
// The audit table and its AUTOINCREMENT sequence are excluded because the
// durable receipt is itself stored in audit_log; including it would create a
// self-referential digest. Exact audit-prefix preservation and the two expected
// import audit rows are verified separately. Page layout and transient SQLite
// counters are also excluded so equivalent standalone backups compare equal.
type sqliteDigestQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func sqliteLogicalStateDigest(ctx context.Context, database sqliteDigestQuery) (string, error) {
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
		switch tableName {
		case "audit_log":
			continue
		case "sqlite_sequence":
			if err := digestSQLiteTableWhere(ctx, database, hasher, tableName, "name<>'audit_log'"); err != nil {
				return "", err
			}
			continue
		}
		if err := digestSQLiteTable(ctx, database, hasher, tableName); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type recoveryProtectedEvidence struct {
	Provider   string `json:"provider"`
	EvidenceID string `json:"evidenceId"`
	SHA256     string `json:"sha256"`
}

type recoveryProtectedScope struct {
	batchSHA256  string
	evidenceJSON string
}

func newRecoveryProtectedScope(manifest lyricsrecoveryimport.Manifest, evidence lyricsrecoveryimport.EvidenceReceipt) (recoveryProtectedScope, error) {
	references := make([]recoveryProtectedEvidence, len(evidence.Evidence))
	for index, reference := range evidence.Evidence {
		references[index] = recoveryProtectedEvidence{
			Provider: string(reference.Provider), EvidenceID: reference.EvidenceID, SHA256: reference.SHA256,
		}
	}
	body, err := json.Marshal(references)
	if err != nil {
		return recoveryProtectedScope{}, err
	}
	return recoveryProtectedScope{batchSHA256: manifest.BatchSHA256, evidenceJSON: string(body)}, nil
}

func recoveryProtectedScopeFromDatabase(ctx context.Context, database sqliteDigestQuery) (*recoveryProtectedScope, error) {
	rows, err := database.QueryContext(ctx, `SELECT batch_sha256 FROM lyrics_recovery_import_batches ORDER BY batch_sha256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batches []string
	for rows.Next() {
		var batchSHA256 string
		if err := rows.Scan(&batchSHA256); err != nil {
			return nil, err
		}
		batches = append(batches, batchSHA256)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return nil, nil
	}
	if len(batches) != 1 {
		return nil, errors.New("offline recovery database contains more than one recovery batch")
	}
	evidenceRows, err := database.QueryContext(ctx, `SELECT provider,evidence_id,sha256 FROM lyrics_recovery_source_evidence
		WHERE EXISTS (SELECT 1 FROM lyrics_recovery_import_artifact_evidence AS link
			WHERE link.batch_sha256=? AND link.provider=lyrics_recovery_source_evidence.provider
			AND link.evidence_id=lyrics_recovery_source_evidence.evidence_id AND link.sha256=lyrics_recovery_source_evidence.sha256)
		ORDER BY provider,evidence_id`, batches[0])
	if err != nil {
		return nil, err
	}
	var references []recoveryProtectedEvidence
	for evidenceRows.Next() {
		var reference recoveryProtectedEvidence
		if err := evidenceRows.Scan(&reference.Provider, &reference.EvidenceID, &reference.SHA256); err != nil {
			evidenceRows.Close()
			return nil, err
		}
		references = append(references, reference)
	}
	if err := evidenceRows.Err(); err != nil {
		evidenceRows.Close()
		return nil, err
	}
	if err := evidenceRows.Close(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(references)
	if err != nil {
		return nil, err
	}
	return &recoveryProtectedScope{batchSHA256: batches[0], evidenceJSON: string(body)}, nil
}

// sqliteProtectedStateDigest hashes every application table value that the
// reviewed recovery transaction is not allowed to change. Before an import it
// uses the existing committed recovery batch, if any, as the only excluded
// ownership scope. During the transaction and post-commit verification the
// caller supplies the exact manifest/evidence scope, so rows for unrelated
// lyrics, batches, source documents, localizations, and evidence remain covered.
func sqliteProtectedStateDigest(ctx context.Context, database sqliteDigestQuery, auditMaxID int64, scopes ...recoveryProtectedScope) (string, error) {
	var scope *recoveryProtectedScope
	switch len(scopes) {
	case 0:
		var err error
		scope, err = recoveryProtectedScopeFromDatabase(ctx, database)
		if err != nil {
			return "", err
		}
	case 1:
		copy := scopes[0]
		scope = &copy
	default:
		return "", errors.New("protected recovery digest accepts at most one ownership scope")
	}

	hasher := sha256.New()
	if err := writeDigestValue(hasher, protectedStateDigestVersion); err != nil {
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
	for _, pragma := range []string{"application_id", "user_version", "encoding", "auto_vacuum"} {
		var value any
		if err := database.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&value); err != nil {
			return "", fmt.Errorf("read protected pragma %s: %w", pragma, err)
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
	for _, tableName := range tableNames {
		if handled, err := digestRecoveryProtectedTable(ctx, database, hasher, tableName, auditMaxID, scope); err != nil {
			return "", err
		} else if handled {
			continue
		}
		if err := digestSQLiteTable(ctx, database, hasher, tableName); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func digestRecoveryProtectedTable(ctx context.Context, database sqliteDigestQuery, hasher io.Writer, tableName string,
	auditMaxID int64, scope *recoveryProtectedScope,
) (bool, error) {
	switch tableName {
	case "audit_log":
		return true, digestSQLiteTableWhere(ctx, database, hasher, tableName, "id<=?", auditMaxID)
	case "sqlite_sequence":
		if scope == nil {
			return false, nil
		}
		return true, digestSQLiteTableWhere(ctx, database, hasher, tableName,
			"name NOT IN ('audit_log','song_lyrics_source_documents','song_lyrics_availability_documents')")
	}
	if scope == nil {
		return false, nil
	}
	switch tableName {
	case "lyrics_recovery_import_batches", "lyrics_recovery_import_items", "lyrics_recovery_import_artifacts",
		"lyrics_recovery_import_artifact_evidence", "lyrics_recovery_import_component_contributions",
		"song_lyrics_availability_documents":
		return true, digestSQLiteTableWhere(ctx, database, hasher, tableName, "batch_sha256<>?", scope.batchSHA256)
	case "song_lyrics", "song_lyric_lines", "song_lyric_segments":
		return true, digestSQLiteTableWhere(ctx, database, hasher, tableName,
			"NOT EXISTS (SELECT 1 FROM lyrics_recovery_import_items AS item WHERE item.batch_sha256=? AND item.music_id="+
				"main."+quoteSQLiteIdentifier(tableName)+".music_id)", scope.batchSHA256)
	case "song_lyrics_source_documents":
		return true, digestSQLiteTableWhere(ctx, database, hasher, tableName, "manifest_batch_sha256<>?", scope.batchSHA256)
	case "song_lyrics_rendition_localizations", "song_lyrics_rendition_translation_lines",
		"song_lyrics_rendition_side_translation_lines":
		return true, digestSQLiteTableWhere(ctx, database, hasher, tableName,
			"NOT EXISTS (SELECT 1 FROM song_lyrics_source_documents AS document WHERE document.document_id="+
				"main."+quoteSQLiteIdentifier(tableName)+".document_id AND document.manifest_batch_sha256=?)", scope.batchSHA256)
	case "lyrics_recovery_source_evidence":
		return true, digestSQLiteTableWhere(ctx, database, hasher, tableName,
			"NOT EXISTS (SELECT 1 FROM json_each(?) AS scoped WHERE "+
				"json_extract(scoped.value,'$.provider')=main.lyrics_recovery_source_evidence.provider AND "+
				"json_extract(scoped.value,'$.evidenceId')=main.lyrics_recovery_source_evidence.evidence_id AND "+
				"json_extract(scoped.value,'$.sha256')=main.lyrics_recovery_source_evidence.sha256)", scope.evidenceJSON)
	}
	return false, nil
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

func digestSQLiteTable(ctx context.Context, database sqliteDigestQuery, writer io.Writer, tableName string) error {
	return digestSQLiteTableWhere(ctx, database, writer, tableName, "")
}

func digestSQLiteTableWhere(ctx context.Context, database sqliteDigestQuery, writer io.Writer, tableName, predicate string, arguments ...any) error {
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
	query := "SELECT " + strings.Join(selectColumns, ",") + " FROM " + quoteSQLiteIdentifier(tableName)
	if predicate != "" {
		query += " WHERE " + predicate
	}
	query += " ORDER BY " + strings.Join(orderColumns, ",")
	rows, err := database.QueryContext(ctx, query, arguments...)
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
