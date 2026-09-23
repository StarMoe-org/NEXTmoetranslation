package lyricsrecovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsextractionplan"
)

var catalogSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

var catalogSQLiteSidecarSuffixes = [...]string{"-wal", "-shm", "-journal"}

type Catalog struct {
	mu         sync.Mutex
	database   *sql.DB
	connection *sql.Conn
	file       *os.File
	path       string
	descriptor string
	info       os.FileInfo
	size       int64
	digest     string
	musicIDs   []int

	testHookAfterQueryValidation func() error
}
type CatalogVerification struct {
	SizeBytes             int64
	SourceSHA256          string
	SchemaVersion         int
	RecordCount           int
	IdentitySHA256        string
	MusicIDsSHA256        string
	IdentityPolicyVersion string
}

type catalogQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type catalogFileOpener func(string) (*os.File, error)
type catalogDatabaseOpener func(string, string) (*sql.DB, error)

func OpenCatalogAgainstPlan(
	ctx context.Context,
	path string,
	binding lyricsextractionplan.RecoveryCatalogBinding,
) (*Catalog, CatalogVerification, error) {
	return openCatalogAgainstPlanWithOpeners(ctx, path, binding, os.Open, sql.Open)
}

// OpenCatalogAgainstPinnedFile duplicates an already validated descriptor and
// gives SQLite only that duplicate. The caller retains ownership of pinned;
// the catalog owns and closes the duplicate on every success or failure path.
func OpenCatalogAgainstPinnedFile(
	ctx context.Context,
	path string,
	binding lyricsextractionplan.RecoveryCatalogBinding,
	pinned *os.File,
) (*Catalog, CatalogVerification, error) {
	if pinned == nil {
		return nil, CatalogVerification{}, errors.New("pinned recovery catalog descriptor is required")
	}
	duplicateFD, err := unix.Dup(int(pinned.Fd()))
	if err != nil {
		return nil, CatalogVerification{}, fmt.Errorf("duplicate pinned recovery catalog descriptor: %w", err)
	}
	unix.CloseOnExec(duplicateFD)
	duplicate := os.NewFile(uintptr(duplicateFD), path)
	if duplicate == nil {
		_ = unix.Close(duplicateFD)
		return nil, CatalogVerification{}, errors.New("duplicated recovery catalog descriptor is invalid")
	}
	consumed := false
	catalog, verification, openErr := openCatalogAgainstPlanWithOpeners(ctx, path, binding,
		func(openPath string) (*os.File, error) {
			if consumed || openPath != path {
				return nil, errors.New("pinned recovery catalog descriptor was requested inconsistently")
			}
			consumed = true
			return duplicate, nil
		}, sql.Open)
	if !consumed {
		_ = duplicate.Close()
	}
	return catalog, verification, openErr
}

func openCatalogAgainstPlanWithOpeners(
	ctx context.Context,
	path string,
	binding lyricsextractionplan.RecoveryCatalogBinding,
	openFile catalogFileOpener,
	openDatabase catalogDatabaseOpener,
) (*Catalog, CatalogVerification, error) {
	if ctx == nil || path == "" || path != binding.Path {
		return nil, CatalogVerification{}, errors.New("recovery catalog path does not exactly match the immutable plan")
	}
	if err := ctx.Err(); err != nil {
		return nil, CatalogVerification{}, err
	}
	if openFile == nil || openDatabase == nil {
		return nil, CatalogVerification{}, errors.New("recovery catalog openers are required")
	}
	if binding.SizeBytes <= 0 || !catalogSHA256.MatchString(binding.SourceSHA256) ||
		binding.SchemaVersion <= 0 || binding.RuntimeSchemaVersion <= 0 || binding.RecordCount <= 0 ||
		binding.IdentityPolicyVersion == "" || !catalogSHA256.MatchString(binding.IdentitySHA256) ||
		!catalogSHA256.MatchString(binding.MusicIDsSHA256) {
		return nil, CatalogVerification{}, errors.New("recovery catalog immutable plan binding is invalid")
	}
	absolute, info, err := inspectCatalogPath(path)
	if err != nil {
		return nil, CatalogVerification{}, err
	}
	if info.Size() != binding.SizeBytes {
		return nil, CatalogVerification{}, errors.New("recovery catalog size or SHA-256 does not match the plan")
	}

	file, err := openFile(absolute)
	if err != nil {
		return nil, CatalogVerification{}, fmt.Errorf("open recovery catalog descriptor: %w", err)
	}
	if file == nil {
		return nil, CatalogVerification{}, errors.New("open recovery catalog descriptor returned no file")
	}
	catalog := &Catalog{file: file, path: absolute}
	fail := func(failure error) (*Catalog, CatalogVerification, error) {
		return nil, CatalogVerification{}, errors.Join(failure, catalog.closeResources(false))
	}
	opened, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("inspect opened recovery catalog descriptor: %w", err))
	}
	_, current, currentErr := inspectCatalogPath(absolute)
	if currentErr != nil || !os.SameFile(info, opened) || !os.SameFile(info, current) || opened.Size() != info.Size() {
		return fail(errors.New("recovery catalog path or inode changed while being opened"))
	}
	if err := validateCatalogFileInfo(opened); err != nil {
		return fail(err)
	}
	catalog.info = opened
	catalog.size = opened.Size()
	catalog.digest, err = hashCatalogDescriptor(file, binding.SizeBytes)
	if err != nil {
		return fail(err)
	}
	if catalog.digest != binding.SourceSHA256 {
		return fail(errors.New("recovery catalog size or SHA-256 does not match the plan"))
	}
	if err := catalog.verifyCurrent("after byte hashing"); err != nil {
		return fail(err)
	}

	// SQLite receives only the kernel descriptor path. The immutable plan path is
	// retained solely for independent revalidation, so a later path replacement
	// can never select the inode used by SQLite queries.
	catalog.descriptor = fmt.Sprintf("/dev/fd/%d", file.Fd())
	if err := verifyCatalogDescriptorPath(catalog.descriptor, catalog.info, catalog.size, catalog.digest); err != nil {
		return fail(err)
	}

	databaseURL := &url.URL{Scheme: "file", Path: catalog.descriptor}
	query := databaseURL.Query()
	query.Set("immutable", "1")
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "trusted_schema(0)")
	databaseURL.RawQuery = query.Encode()
	catalog.database, err = openDatabase("sqlite", databaseURL.String())
	if err != nil {
		return fail(fmt.Errorf("open immutable recovery catalog database: %w", err))
	}
	if catalog.database == nil {
		return fail(errors.New("open immutable recovery catalog database returned no database"))
	}
	catalog.database.SetMaxOpenConns(1)
	catalog.database.SetMaxIdleConns(1)
	catalog.database.SetConnMaxLifetime(0)
	catalog.connection, err = catalog.database.Conn(ctx)
	if err != nil {
		return fail(fmt.Errorf("reserve immutable recovery catalog connection: %w", err))
	}
	if _, err := catalog.connection.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		return fail(err)
	}
	if err := catalog.verifySQLiteConnection(ctx); err != nil {
		return fail(err)
	}
	if err := catalog.verifyCurrent("after SQLite open"); err != nil {
		return fail(err)
	}

	verification, musicIDs, err := verifyCatalogRows(ctx, catalog.connection, catalog.size, catalog.digest)
	if err != nil {
		return fail(err)
	}
	if err := catalog.verifyCurrent("after catalog row verification"); err != nil {
		return fail(err)
	}
	if verification.SchemaVersion != binding.SchemaVersion || binding.RuntimeSchemaVersion < verification.SchemaVersion ||
		verification.RecordCount != binding.RecordCount || verification.IdentityPolicyVersion != binding.IdentityPolicyVersion ||
		verification.IdentitySHA256 != binding.IdentitySHA256 || verification.MusicIDsSHA256 != binding.MusicIDsSHA256 {
		return fail(errors.New("recovery catalog schema, count, policy, or ordered identity digest does not match the plan"))
	}
	catalog.musicIDs = musicIDs
	return catalog, verification, nil
}

func (catalog *Catalog) Close() error {
	if catalog == nil {
		return nil
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return catalog.closeResources(true)
}

func (catalog *Catalog) closeResources(verify bool) error {
	if catalog == nil {
		return nil
	}
	var result error
	hadSQLite := catalog.connection != nil || catalog.database != nil
	if verify && hadSQLite && catalog.file != nil {
		result = errors.Join(result, catalog.verifyCurrent("before close"))
	}
	if catalog.connection != nil {
		result = errors.Join(result, catalog.connection.Close())
		catalog.connection = nil
	}
	if catalog.database != nil {
		result = errors.Join(result, catalog.database.Close())
		catalog.database = nil
	}
	if verify && hadSQLite && catalog.file != nil {
		result = errors.Join(result, catalog.verifyCurrent("after SQLite close"))
	}
	if catalog.file != nil {
		result = errors.Join(result, catalog.file.Close())
		catalog.file = nil
	}
	return result
}

func (catalog *Catalog) MusicIDs() []int {
	if catalog == nil {
		return nil
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return append([]int(nil), catalog.musicIDs...)
}

func (catalog *Catalog) MusicIdentity(ctx context.Context, musicID int) (lyricssource.MusicIdentity, error) {
	if catalog == nil || ctx == nil || musicID <= 0 {
		return lyricssource.MusicIdentity{}, errors.New("recovery catalog music identity request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return lyricssource.MusicIdentity{}, err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.connection == nil || catalog.file == nil {
		return lyricssource.MusicIdentity{}, errors.New("recovery catalog music identity request is invalid")
	}
	if err := catalog.verifyCurrent("before music identity query"); err != nil {
		return lyricssource.MusicIdentity{}, err
	}
	if catalog.testHookAfterQueryValidation != nil {
		if err := catalog.testHookAfterQueryValidation(); err != nil {
			return lyricssource.MusicIdentity{}, err
		}
	}
	var identity lyricssource.MusicIdentity
	var vocalsJSON string
	queryErr := catalog.connection.QueryRowContext(ctx, `SELECT music_id,title_ja,producer_metadata,lyricist,composer,arranger,vocal_signals_json
		FROM catalog_music WHERE music_id=?`, musicID).Scan(
		&identity.MusicID, &identity.JapaneseTitle, &identity.ProducerMetadata,
		&identity.Lyricist, &identity.Composer, &identity.Arranger, &vocalsJSON,
	)
	currentErr := catalog.verifyCurrent("after music identity query")
	if queryErr != nil || currentErr != nil {
		return lyricssource.MusicIdentity{}, errors.Join(queryErr, currentErr)
	}
	var vocals []model.CatalogVocalSignal
	if err := json.Unmarshal([]byte(vocalsJSON), &vocals); err != nil || vocals == nil {
		return lyricssource.MusicIdentity{}, errors.New("recovery catalog vocal signals are malformed")
	}
	identity.PerformerSegmentationPolicy = lyricssource.PerformerSegmentationPolicyFromCatalogVocals(vocals)
	identity.Instrumental = model.CatalogLyricsAreInstrumental(vocals, identity.Lyricist)
	if strings.TrimSpace(identity.JapaneseTitle) == "" || strings.TrimSpace(identity.ProducerMetadata) == "" {
		return lyricssource.MusicIdentity{}, errors.New("recovery catalog music identity is incomplete")
	}
	return identity, nil
}
func (catalog *Catalog) verifySQLiteConnection(ctx context.Context) error {
	if catalog == nil || catalog.connection == nil || catalog.info == nil || catalog.descriptor == "" {
		return errors.New("recovery catalog SQLite connection is not pinned")
	}
	var sequence int
	var name, openedPath string
	if err := catalog.connection.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&sequence, &name, &openedPath); err != nil {
		return fmt.Errorf("verify opened recovery catalog database: %w", err)
	}
	if sequence != 0 || name != "main" || openedPath == "" {
		return errors.New("opened SQLite database does not match the pinned recovery catalog")
	}
	if filepath.Clean(openedPath) != filepath.Clean(catalog.descriptor) {
		openedInfo, err := os.Stat(openedPath)
		if err != nil || !os.SameFile(catalog.info, openedInfo) || openedInfo.Size() != catalog.size {
			return errors.New("opened SQLite database does not match the pinned recovery catalog")
		}
	}
	var queryOnly int
	if err := catalog.connection.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		return errors.New("recovery catalog connection is not query-only")
	}
	var trustedSchema int
	if err := catalog.connection.QueryRowContext(ctx, `PRAGMA trusted_schema`).Scan(&trustedSchema); err != nil || trustedSchema != 0 {
		return errors.New("recovery catalog trusted schema defense is not active")
	}
	var attachedCount int
	if err := catalog.connection.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_database_list`).Scan(&attachedCount); err != nil || attachedCount != 1 {
		return errors.New("recovery catalog has an unexpected attached database")
	}
	return nil
}

func (catalog *Catalog) verifyCurrent(stage string) error {
	if catalog == nil || catalog.file == nil || catalog.info == nil || catalog.path == "" {
		return errors.New("recovery catalog snapshot is not active")
	}
	_, pathInfo, err := inspectCatalogPath(catalog.path)
	if err != nil {
		return fmt.Errorf("recovery catalog path changed %s: %w", stage, err)
	}
	fileInfo, err := catalog.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect pinned recovery catalog %s: %w", stage, err)
	}
	if err := validateCatalogFileInfo(fileInfo); err != nil {
		return fmt.Errorf("recovery catalog descriptor policy changed %s: %w", stage, err)
	}
	if !os.SameFile(catalog.info, fileInfo) || !os.SameFile(catalog.info, pathInfo) ||
		fileInfo.Size() != catalog.size || pathInfo.Size() != catalog.size {
		return fmt.Errorf("recovery catalog path, inode, or size changed %s", stage)
	}
	if catalog.descriptor != "" {
		if err := verifyCatalogDescriptorPath(catalog.descriptor, catalog.info, catalog.size, ""); err != nil {
			return fmt.Errorf("recovery catalog descriptor changed %s: %w", stage, err)
		}
	}
	digest, err := hashCatalogDescriptor(catalog.file, catalog.size)
	if err != nil {
		return fmt.Errorf("hash pinned recovery catalog %s: %w", stage, err)
	}
	if digest != catalog.digest {
		return fmt.Errorf("recovery catalog bytes changed %s", stage)
	}
	return nil
}

func verifyCatalogDescriptorPath(path string, expected os.FileInfo, size int64, digest string) error {
	descriptor, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open recovery catalog descriptor path: %w", err)
	}
	defer descriptor.Close()
	info, err := descriptor.Stat()
	if err != nil || !os.SameFile(expected, info) || info.Size() != size {
		return errors.New("recovery catalog descriptor path does not bind the inspected inode")
	}
	if err := validateCatalogFileInfo(info); err != nil {
		return err
	}
	if digest != "" {
		actual, err := hashCatalogDescriptor(descriptor, size)
		if err != nil {
			return err
		}
		if actual != digest {
			return errors.New("recovery catalog descriptor path does not bind the inspected bytes")
		}
	}
	return nil
}

func inspectCatalogPath(path string) (string, os.FileInfo, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || absolute != path || filepath.Clean(path) != path {
		return "", nil, errors.New("recovery catalog path must be absolute and canonical")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("inspect recovery catalog: %w", err)
	}
	if err := validateCatalogFileInfo(info); err != nil {
		return "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || resolved != absolute {
		return "", nil, errors.New("recovery catalog path must not traverse a symlink or filesystem alias")
	}
	if err := rejectCatalogSQLiteSidecars(absolute); err != nil {
		return "", nil, err
	}
	return absolute, info, nil
}

func validateCatalogFileInfo(info os.FileInfo) error {
	owner, ownerOK := catalogFileOwner(info)
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o444 ||
		!ownerOK || int(owner) != os.Geteuid() || catalogFileLinkCount(info) != 1 {
		return errors.New("recovery catalog must be a direct effective-UID-owned mode-0444 regular file with one link")
	}
	return nil
}

func catalogFileOwner(info os.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func catalogFileLinkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func rejectCatalogSQLiteSidecars(path string) error {
	for _, suffix := range catalogSQLiteSidecarSuffixes {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("recovery catalog must be a standalone immutable SQLite snapshot: %s sidecar exists", suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect recovery catalog %s sidecar: %w", suffix, err)
		}
	}
	return nil
}

func verifyCatalogRows(
	ctx context.Context,
	database catalogQuerier,
	size int64,
	sourceSHA string,
) (CatalogVerification, []int, error) {
	var schemaVersion int
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		return CatalogVerification{}, nil, err
	}
	rows, err := database.QueryContext(ctx, `SELECT music_id,lyrics_catalog_fingerprint,lyrics_catalog_policy_version
		FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return CatalogVerification{}, nil, err
	}
	defer rows.Close()
	musicIDs := make([]int, 0, 1024)
	identityDigest := sha256.New()
	_, _ = identityDigest.Write([]byte("moesekai-lyrics-recovery-catalog-fingerprints-v1\x00"))
	var encoded [8]byte
	last := 0
	policy := ""
	for rows.Next() {
		var musicID int
		var fingerprint, rowPolicy string
		if err := rows.Scan(&musicID, &fingerprint, &rowPolicy); err != nil {
			return CatalogVerification{}, nil, err
		}
		if musicID <= last || !catalogSHA256.MatchString(fingerprint) || rowPolicy == "" ||
			(policy != "" && policy != rowPolicy) {
			return CatalogVerification{}, nil, errors.New("recovery catalog ordered fingerprint identity is invalid")
		}
		policy = rowPolicy
		musicIDs = append(musicIDs, musicID)
		binary.BigEndian.PutUint64(encoded[:], uint64(musicID))
		_, _ = identityDigest.Write(encoded[:])
		fingerprintBytes, _ := hex.DecodeString(fingerprint)
		_, _ = identityDigest.Write(fingerprintBytes)
		last = musicID
	}
	if err := rows.Err(); err != nil || len(musicIDs) == 0 {
		return CatalogVerification{}, nil, errors.New("recovery catalog has no valid ordered music records")
	}
	musicIDsSHA, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil {
		return CatalogVerification{}, nil, err
	}
	return CatalogVerification{
		SizeBytes: size, SourceSHA256: sourceSHA, SchemaVersion: schemaVersion,
		RecordCount: len(musicIDs), IdentitySHA256: hex.EncodeToString(identityDigest.Sum(nil)),
		MusicIDsSHA256: musicIDsSHA, IdentityPolicyVersion: policy,
	}, musicIDs, nil
}

func hashCatalogDescriptor(file *os.File, expectedSize int64) (string, error) {
	if file == nil || expectedSize < 0 {
		return "", errors.New("hash recovery catalog exact bytes: descriptor and non-negative size are required")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.NewSectionReader(file, 0, expectedSize+1))
	if err != nil {
		return "", fmt.Errorf("hash recovery catalog exact bytes: %w", err)
	}
	if written != expectedSize {
		return "", fmt.Errorf("hash recovery catalog exact bytes: size=%d expected=%d", written, expectedSize)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
