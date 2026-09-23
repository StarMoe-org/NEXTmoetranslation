package main

import (
	"bytes"
	"context"

	"crypto/sha256"
	"database/sql"

	"encoding/json"
	"errors"

	"fmt"
	"io"

	"net/url"
	"os"

	"path/filepath"

	"strings"

	"moesekai/server/internal/model"

	_ "modernc.org/sqlite"
)

// loadCatalog owns and closes the pinned immutable snapshot before returning
// any rows to the concurrent source-inspection phase.
func loadCatalog(ctx context.Context, databasePath string) ([]catalogItem, error) {
	database, err := openReadOnlyCatalogDB(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	items, loadErr := loadV18Catalog(ctx, database)
	closeErr := database.Close()
	if loadErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("%v; close read-only catalog database: %w", loadErr, closeErr)
		}
		return nil, loadErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close read-only catalog database: %w", closeErr)
	}
	return items, nil
}

type readOnlyCatalogDB struct {
	*sql.DB
	file             *os.File
	absolutePath     string
	sidecarBasePaths []string
	info             os.FileInfo
	size             int64
	digest           [sha256.Size]byte
}

func (database *readOnlyCatalogDB) Close() error {
	if database == nil {
		return nil
	}
	var result error
	if database.DB != nil {
		if err := database.verifyCurrent("before close"); err != nil {
			result = errors.Join(result, err)
		}
		if err := database.DB.Close(); err != nil {
			result = errors.Join(result, err)
		}
		database.DB = nil
		if err := database.verifyCurrent("after close"); err != nil {
			result = errors.Join(result, err)
		}
	}
	if database.file != nil {
		if err := database.file.Close(); err != nil {
			result = errors.Join(result, err)
		}
		database.file = nil
	}
	return result
}

func (database *readOnlyCatalogDB) verifyCurrent(stage string) error {
	if database == nil || database.file == nil || database.info == nil {
		return errors.New("catalog snapshot is not active")
	}
	currentPaths, err := catalogSidecarBasePaths(database.absolutePath)
	if err != nil {
		return fmt.Errorf("catalog snapshot path changed %s: %w", stage, err)
	}
	if err := rejectSQLiteSidecarsAtPaths(append(append([]string{}, database.sidecarBasePaths...), currentPaths...)); err != nil {
		return fmt.Errorf("catalog snapshot is not standalone %s: %w", stage, err)
	}
	fileInfo, err := database.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect pinned catalog snapshot %s: %w", stage, err)
	}
	pathInfo, err := os.Stat(database.absolutePath)
	if err != nil || !fileInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(database.info, fileInfo) || !os.SameFile(database.info, pathInfo) ||
		fileInfo.Size() != database.size || pathInfo.Size() != database.size {
		return fmt.Errorf("catalog snapshot path, inode, or size changed %s", stage)
	}
	digest, err := hashCatalogSnapshot(database.file, database.size)
	if err != nil {
		return fmt.Errorf("hash pinned catalog snapshot %s: %w", stage, err)
	}
	if digest != database.digest {
		return fmt.Errorf("catalog snapshot bytes changed %s", stage)
	}
	return nil
}

func catalogSidecarBasePaths(databasePath string) ([]string, error) {
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog snapshot path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog snapshot: %w", err)
	}
	paths := []string{absolute}
	if resolved != absolute {
		paths = append(paths, resolved)
	}
	return paths, nil
}

func rejectSQLiteSidecarsAtPaths(basePaths []string) error {
	seen := make(map[string]struct{}, len(basePaths))
	for _, basePath := range basePaths {
		if _, exists := seen[basePath]; exists {
			continue
		}
		seen[basePath] = struct{}{}
		for _, suffix := range sqliteSidecarSuffixes {
			sidecarPath := basePath + suffix
			if _, err := os.Lstat(sidecarPath); err == nil {
				return fmt.Errorf("catalog database must be a standalone immutable SQLite snapshot: %s sidecar exists", suffix)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect catalog database %s sidecar: %w", suffix, err)
			}
		}
	}
	return nil
}

func hashCatalogSnapshot(file *os.File, size int64) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if file == nil || size < 0 {
		return digest, errors.New("catalog snapshot file and non-negative size are required")
	}
	hasher := sha256.New()
	read, err := io.Copy(hasher, io.NewSectionReader(file, 0, size))
	if err != nil {
		return digest, err
	}
	if read != size {
		return digest, errors.New("catalog snapshot size changed while hashing")
	}
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

// openReadOnlyCatalogDB pins the requested inode with an open descriptor, then
// asks SQLite to open that descriptor as mode=ro&immutable=1. The original path,
// inode, size, bytes, and absence of sidecars are revalidated before and after
// SQLite close. It never invokes migrations or store code.
func openReadOnlyCatalogDB(ctx context.Context, databasePath string) (*readOnlyCatalogDB, error) {
	return openReadOnlyCatalogDBWithOpeners(ctx, databasePath, os.Open, sql.Open)
}

func openReadOnlyCatalogDBWithOpeners(ctx context.Context, databasePath string,
	openFile func(string) (*os.File, error), openDatabase func(string, string) (*sql.DB, error),
) (*readOnlyCatalogDB, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if openFile == nil || openDatabase == nil {
		return nil, errors.New("catalog snapshot openers are required")
	}
	absolutePath, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	sidecarBasePaths, err := catalogSidecarBasePaths(absolutePath)
	if err != nil {
		return nil, err
	}
	if err := rejectSQLiteSidecarsAtPaths(sidecarBasePaths); err != nil {
		return nil, err
	}
	inspectedInfo, err := os.Stat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("inspect catalog snapshot: %w", err)
	}
	if !inspectedInfo.Mode().IsRegular() {
		return nil, errors.New("database path must identify a regular file")
	}
	file, err := openFile(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("open catalog snapshot: %w", err)
	}
	database := &readOnlyCatalogDB{file: file, absolutePath: absolutePath, sidecarBasePaths: sidecarBasePaths}
	fail := func(err error) (*readOnlyCatalogDB, error) {
		if database.DB != nil {
			_ = database.DB.Close()
			database.DB = nil
		}
		if database.file != nil {
			_ = database.file.Close()
			database.file = nil
		}
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("inspect opened catalog snapshot: %w", err))
	}
	pathInfo, err := os.Stat(absolutePath)
	if err != nil || !info.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(inspectedInfo, info) || !os.SameFile(inspectedInfo, pathInfo) ||
		info.Size() != inspectedInfo.Size() || pathInfo.Size() != inspectedInfo.Size() {
		return fail(errors.New("catalog snapshot path, inode, or size changed while being opened"))
	}
	database.info = info
	database.size = info.Size()
	database.digest, err = hashCatalogSnapshot(file, database.size)
	if err != nil {
		return fail(fmt.Errorf("hash catalog snapshot: %w", err))
	}
	if err := database.verifyCurrent("before SQLite open"); err != nil {
		return fail(err)
	}

	descriptorPath := fmt.Sprintf("/dev/fd/%d", file.Fd())
	databaseURL := &url.URL{Scheme: "file", Path: descriptorPath}
	query := databaseURL.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "trusted_schema(0)")
	query.Add("_pragma", "busy_timeout(5000)")
	databaseURL.RawQuery = query.Encode()
	database.DB, err = openDatabase("sqlite", databaseURL.String())
	if err != nil {
		return fail(fmt.Errorf("open immutable catalog database: %w", err))
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(0)
	if err := database.PingContext(ctx); err != nil {
		return fail(fmt.Errorf("ping immutable catalog database: %w", err))
	}
	var databaseListName, databaseListPath string
	var databaseListSequence int
	if err := database.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&databaseListSequence, &databaseListName, &databaseListPath); err != nil {
		return fail(fmt.Errorf("verify opened SQLite database: %w", err))
	}
	if databaseListSequence != 0 || databaseListName != "main" || databaseListPath == "" {
		return fail(errors.New("opened SQLite database does not match pinned catalog snapshot"))
	}
	if filepath.Clean(databaseListPath) != filepath.Clean(descriptorPath) {
		openedInfo, err := os.Stat(databaseListPath)
		if err != nil {
			return fail(fmt.Errorf("inspect opened SQLite database %q: %w", databaseListPath, err))
		}
		if !os.SameFile(info, openedInfo) || openedInfo.Size() != database.size {
			return fail(fmt.Errorf("opened SQLite database %q does not match pinned catalog snapshot", databaseListPath))
		}
	}
	var queryOnly int
	if err := database.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil {
		return fail(fmt.Errorf("verify SQLite query_only: %w", err))
	}
	if queryOnly != 1 {
		return fail(errors.New("SQLite query_only is not enabled"))
	}
	var trustedSchema int
	if err := database.QueryRowContext(ctx, `PRAGMA trusted_schema`).Scan(&trustedSchema); err != nil {
		return fail(fmt.Errorf("verify SQLite trusted_schema: %w", err))
	}
	if trustedSchema != 0 {
		return fail(errors.New("SQLite trusted_schema is not disabled"))
	}
	var attachedCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_database_list`).Scan(&attachedCount); err != nil {
		return fail(fmt.Errorf("verify SQLite attachment count: %w", err))
	}
	if attachedCount != 1 {
		return fail(errors.New("unexpected attached SQLite databases"))
	}
	if err := database.verifyCurrent("after SQLite open"); err != nil {
		return fail(err)
	}
	return database, nil
}

type catalogQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadV18Catalog(ctx context.Context, database catalogQuerier) ([]catalogItem, error) {
	if ctx == nil || database == nil {
		return nil, errors.New("catalog database and context are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var minimumVersion, maximumVersion, versionCount int
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(MIN(version),0), COALESCE(MAX(version),0), COUNT(*) FROM schema_migrations`).
		Scan(&minimumVersion, &maximumVersion, &versionCount); err != nil {
		return nil, fmt.Errorf("read catalog schema version: %w", err)
	}
	if minimumVersion != 1 || maximumVersion != catalogSchemaVersion || versionCount != catalogSchemaVersion {
		return nil, fmt.Errorf("catalog schema must be exactly v%d", catalogSchemaVersion)
	}
	rows, err := database.QueryContext(ctx, `SELECT music_id, title_ja, producer_metadata, lyricist, composer, arranger,
		assetbundle_name, version_hint, lyrics_version, lyrics_evidence_presence_json, vocal_signals_json,
		lyrics_catalog_fingerprint, lyrics_catalog_policy_version FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return nil, fmt.Errorf("query v18 catalog: %w", err)
	}
	defer rows.Close()
	items := make([]catalogItem, 0, 1024)
	lastMusicID := 0
	for rows.Next() {
		if len(items) >= maxCatalogRecords {
			return nil, fmt.Errorf("catalog exceeds %d records", maxCatalogRecords)
		}
		var item catalogItem
		var assetbundle, versionHint, lyricsVersion, presenceJSON, vocalsJSON, policyVersion string
		if err := rows.Scan(&item.MusicID, &item.JapaneseTitle, &item.ProducerMetadata, &item.Lyricist,
			&item.Composer, &item.Arranger, &assetbundle, &versionHint, &lyricsVersion, &presenceJSON,
			&vocalsJSON, &item.CatalogFingerprint, &policyVersion); err != nil {
			return nil, fmt.Errorf("scan v18 catalog: %w", err)
		}
		if item.MusicID <= lastMusicID || strings.TrimSpace(item.JapaneseTitle) == "" || len(item.JapaneseTitle) > maxCandidateTitle ||
			strings.ContainsAny(item.JapaneseTitle, "\r\n") || len(item.ProducerMetadata) > maxCandidateTitle ||
			len(item.Lyricist) > maxCandidateTitle || len(item.Composer) > maxCandidateTitle || len(item.Arranger) > maxCandidateTitle {
			return nil, errors.New("catalog contains an invalid music identity")
		}
		lastMusicID = item.MusicID
		if policyVersion != model.LyricsCatalogIdentityPolicyVersion {
			return nil, fmt.Errorf("catalog music %d has unsupported identity policy", item.MusicID)
		}
		if len(presenceJSON) > maxCatalogJSONBytes || len(vocalsJSON) > maxCatalogJSONBytes {
			return nil, fmt.Errorf("catalog music %d evidence exceeds safe limits", item.MusicID)
		}
		item.Evidence = model.CatalogLyricsEvidence{
			Title: item.JapaneseTitle, Lyricist: item.Lyricist, Composer: item.Composer, Arranger: item.Arranger,
			Assetbundle: assetbundle, VersionHint: versionHint, LyricsVersion: lyricsVersion,
		}
		if len(assetbundle) > maxCandidateTitle || len(versionHint) > maxCandidateTitle || len(lyricsVersion) > maxCandidateTitle ||
			strings.ContainsAny(lyricsVersion, "\r\n") {
			return nil, fmt.Errorf("catalog music %d evidence text exceeds safe limits", item.MusicID)
		}
		if err := decodeClosedJSON([]byte(presenceJSON), &item.Evidence.Presence); err != nil {
			return nil, fmt.Errorf("catalog music %d has malformed evidence presence", item.MusicID)
		}
		if err := decodeClosedJSON([]byte(vocalsJSON), &item.Evidence.Vocals); err != nil {
			return nil, fmt.Errorf("catalog music %d has malformed vocal signals", item.MusicID)
		}
		if len(item.Evidence.Vocals) > maxCatalogRecords {
			return nil, fmt.Errorf("catalog music %d has excessive vocal signals", item.MusicID)
		}
		computedFingerprint, err := model.CatalogLyricsEvidenceFingerprint(item.Evidence)
		if err != nil {
			return nil, fmt.Errorf("catalog music %d fingerprint: %w", item.MusicID, err)
		}
		if item.CatalogFingerprint != computedFingerprint {
			return nil, fmt.Errorf("catalog music %d fingerprint does not match v18 evidence", item.MusicID)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read v18 catalog: %w", err)
	}
	return items, nil
}

func decodeClosedJSON(body []byte, target any) error {
	if len(body) == 0 || target == nil {
		return errors.New("JSON body and target are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON value")
	}
	return nil
}
