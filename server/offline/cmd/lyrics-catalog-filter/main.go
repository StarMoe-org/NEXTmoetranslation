package main

import (
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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	_ "modernc.org/sqlite"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/offline/internal/lyricsextractionplan"
)

const (
	catalogFileName = "catalog.db"
	receiptFileName = "receipt.json"
)

var canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type options struct {
	sourceCatalogPath, expectedSourceCatalogSHA string
	targetMapPath, expectedTargetMapSHA         string
	outputRoot                                  string
}

type catalogSong struct {
	MusicID       int    `json:"musicId"`
	JapaneseTitle string `json:"japaneseTitle"`
}

type targetMapReport struct {
	SchemaVersion           int                `json:"schemaVersion"`
	CatalogSHA256           string             `json:"catalogSha256"`
	CatalogCount            int                `json:"catalogCount"`
	Inputs                  json.RawMessage    `json:"inputs"`
	MappingCount            int                `json:"mappingCount"`
	SekaipediaCount         int                `json:"sekaipediaCount"`
	MoegirlPublicExactCount int                `json:"moegirlPublicExactCount"`
	MusicIDSetEncoding      string             `json:"musicIdSetEncoding"`
	MusicIDSetSHA256        string             `json:"musicIdSetSha256"`
	MappingsSHA256          string             `json:"mappingsSha256"`
	ExcludedMusic           []catalogSong      `json:"excludedMusic"`
	Mappings                []targetMapMapping `json:"mappings"`
}

type targetMapMapping struct {
	MusicID              int              `json:"musicId"`
	CatalogJapaneseTitle string           `json:"catalogJapaneseTitle"`
	Provider             string           `json:"provider"`
	Sekaipedia           *json.RawMessage `json:"sekaipedia,omitempty"`
	MoegirlPublicExact   *json.RawMessage `json:"moegirlPublicExact,omitempty"`
}

type catalogVerification struct {
	ByteCount             int64  `json:"byteCount"`
	SHA256                string `json:"sha256"`
	SchemaVersion         int    `json:"schemaVersion"`
	RuntimeSchemaVersion  int    `json:"runtimeSchemaVersion"`
	RecordCount           int    `json:"recordCount"`
	IdentityPolicyVersion string `json:"identityPolicyVersion"`
	IdentitySHA256        string `json:"identitySha256"`
	MusicIDsSHA256        string `json:"musicIdsSha256"`
}

type sourceCatalogBinding struct {
	ByteCount   int64  `json:"byteCount"`
	SHA256      string `json:"sha256"`
	RecordCount int    `json:"recordCount"`
}

type filterReceipt struct {
	SchemaVersion   int                  `json:"schemaVersion"`
	SourceCatalog   sourceCatalogBinding `json:"sourceCatalog"`
	TargetMapSHA256 string               `json:"targetMapSha256"`
	CatalogFile     string               `json:"catalogFile"`
	Catalog         catalogVerification  `json:"catalog"`
	ExcludedMusic   []catalogSong        `json:"excludedMusic"`
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) (resultErr error) {
	parsed, err := parseOptions(arguments)
	if err != nil {
		return err
	}
	sourceBody, err := readPinnedFile(parsed.sourceCatalogPath, parsed.expectedSourceCatalogSHA, lyricsextractionplan.MaxCatalogDatabaseBytes)
	if err != nil {
		return err
	}
	targetBody, err := readPinnedFile(parsed.targetMapPath, parsed.expectedTargetMapSHA, 16<<20)
	if err != nil {
		return err
	}
	var target targetMapReport
	if err := decodeStrict(targetBody, &target); err != nil {
		return fmt.Errorf("decode canonical target map: %w", err)
	}
	if err := validateTargetMap(target, parsed.expectedSourceCatalogSHA); err != nil {
		return err
	}
	if err := createPrivateOutputRoot(parsed.outputRoot); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			resultErr = errors.Join(resultErr, os.RemoveAll(parsed.outputRoot))
		}
	}()

	temporaryPath := filepath.Join(parsed.outputRoot, ".catalog-filtering.db")
	if err := writeExclusiveFile(temporaryPath, sourceBody, 0o600); err != nil {
		return err
	}
	if err := filterCatalog(ctx, temporaryPath, target.ExcludedMusic); err != nil {
		return err
	}
	if err := rejectSQLiteSidecars(temporaryPath); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o444); err != nil {
		return err
	}
	verification, musicIDs, titles, err := verifyCatalog(ctx, temporaryPath)
	if err != nil {
		return err
	}
	if err := verifyFilteredCatalogAgainstTarget(target, verification, musicIDs, titles); err != nil {
		return err
	}
	catalogPath := filepath.Join(parsed.outputRoot, catalogFileName)
	if err := os.Link(temporaryPath, catalogPath); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	if err := verifyFinalCatalogFile(catalogPath, verification); err != nil {
		return err
	}
	receipt := filterReceipt{
		SchemaVersion: 1,
		SourceCatalog: sourceCatalogBinding{
			ByteCount: int64(len(sourceBody)), SHA256: parsed.expectedSourceCatalogSHA, RecordCount: target.CatalogCount,
		},
		TargetMapSHA256: parsed.expectedTargetMapSHA,
		CatalogFile:     catalogFileName,
		Catalog:         verification,
		ExcludedMusic:   append([]catalogSong(nil), target.ExcludedMusic...),
	}
	receiptBody, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := writeExclusiveFile(filepath.Join(parsed.outputRoot, receiptFileName), append(receiptBody, '\n'), 0o600); err != nil {
		return err
	}
	if err := syncDirectory(parsed.outputRoot); err != nil {
		return err
	}
	cleanup = false
	_, err = fmt.Fprintf(output,
		"PASS mode=lyrics-catalog-filter records=%d exclusions=%d catalogSHA256=%s output=%s\n",
		verification.RecordCount, len(target.ExcludedMusic), verification.SHA256, parsed.outputRoot,
	)
	return err
}

func validateTargetMap(target targetMapReport, sourceCatalogSHA string) error {
	if target.SchemaVersion != 1 || target.CatalogSHA256 != sourceCatalogSHA || target.CatalogCount <= 0 ||
		target.MappingCount != len(target.Mappings) || target.MappingCount <= 0 ||
		target.MappingCount+len(target.ExcludedMusic) != target.CatalogCount ||
		target.SekaipediaCount+target.MoegirlPublicExactCount != target.MappingCount || target.MoegirlPublicExactCount != 1 ||
		target.MusicIDSetEncoding != "decimal-newline-v1" || !canonicalSHA256.MatchString(target.MusicIDSetSHA256) ||
		!canonicalSHA256.MatchString(target.MappingsSHA256) || len(target.Inputs) == 0 {
		return errors.New("canonical target-map summary is incomplete or inconsistent")
	}
	last := 0
	sekaipediaCount, moegirlCount := 0, 0
	for _, mapping := range target.Mappings {
		if mapping.MusicID <= last || strings.TrimSpace(mapping.CatalogJapaneseTitle) == "" {
			return errors.New("canonical target map has invalid ordered music identities")
		}
		switch mapping.Provider {
		case "sekaipedia":
			if mapping.Sekaipedia == nil || mapping.MoegirlPublicExact != nil {
				return errors.New("canonical target map has an invalid Sekaipedia union")
			}
			sekaipediaCount++
		case "moegirl_public_exact":
			if mapping.MoegirlPublicExact == nil || mapping.Sekaipedia != nil {
				return errors.New("canonical target map has an invalid exact Moegirl union")
			}
			moegirlCount++
		default:
			return errors.New("canonical target map contains an unauthorized provider")
		}
		last = mapping.MusicID
	}
	if sekaipediaCount != target.SekaipediaCount || moegirlCount != target.MoegirlPublicExactCount ||
		decimalMusicIDsSHA256(target.Mappings) != target.MusicIDSetSHA256 {
		return errors.New("canonical target-map provider or music-ID digest does not match its records")
	}
	last = 0
	for _, excluded := range target.ExcludedMusic {
		if excluded.MusicID <= last || strings.TrimSpace(excluded.JapaneseTitle) == "" {
			return errors.New("canonical target map has invalid ordered exclusions")
		}
		last = excluded.MusicID
	}
	return nil
}

func filterCatalog(ctx context.Context, path string, excluded []catalogSong) error {
	databaseURL := &url.URL{Scheme: "file", Path: path}
	query := databaseURL.Query()
	query.Set("mode", "rw")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "trusted_schema(0)")
	databaseURL.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	defer database.Close()
	connection, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	for _, statement := range []string{
		`PRAGMA journal_mode=DELETE`, `PRAGMA secure_delete=ON`, `PRAGMA temp_store=MEMORY`, `PRAGMA trusted_schema=OFF`,
	} {
		if _, err := connection.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	transaction, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback()
		}
	}()
	for _, song := range excluded {
		result, err := transaction.ExecContext(ctx, `DELETE FROM catalog_music WHERE music_id=? AND title_ja=?`, song.MusicID, song.JapaneseTitle)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return fmt.Errorf("excluded catalog music ID %d did not match exactly one source row", song.MusicID)
		}
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	committed = true
	if _, err := connection.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	var integrity string
	if err := connection.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("filtered catalog integrity=%q: %w", integrity, err)
	}
	return nil
}

func verifyCatalog(ctx context.Context, path string) (catalogVerification, []int, map[int]string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 || info.Size() <= 0 {
		return catalogVerification{}, nil, nil, errors.New("filtered catalog is not a mode-0444 regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return catalogVerification{}, nil, nil, err
	}
	databaseURL := &url.URL{Scheme: "file", Path: path}
	query := databaseURL.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "trusted_schema(0)")
	databaseURL.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return catalogVerification{}, nil, nil, err
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	var schemaVersion int
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		return catalogVerification{}, nil, nil, err
	}
	rows, err := database.QueryContext(ctx, `SELECT music_id,title_ja,lyrics_catalog_fingerprint,lyrics_catalog_policy_version
		FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return catalogVerification{}, nil, nil, err
	}
	defer rows.Close()
	musicIDs := []int{}
	titles := map[int]string{}
	identityDigest := sha256.New()
	_, _ = identityDigest.Write([]byte("moesekai-lyrics-recovery-catalog-fingerprints-v1\x00"))
	var encoded [8]byte
	last := 0
	policy := ""
	for rows.Next() {
		var musicID int
		var title, fingerprint, rowPolicy string
		if err := rows.Scan(&musicID, &title, &fingerprint, &rowPolicy); err != nil {
			return catalogVerification{}, nil, nil, err
		}
		if musicID <= last || strings.TrimSpace(title) == "" || !canonicalSHA256.MatchString(fingerprint) || rowPolicy == "" ||
			(policy != "" && policy != rowPolicy) {
			return catalogVerification{}, nil, nil, errors.New("filtered catalog ordered identity is invalid")
		}
		policy = rowPolicy
		musicIDs = append(musicIDs, musicID)
		titles[musicID] = title
		binary.BigEndian.PutUint64(encoded[:], uint64(musicID))
		_, _ = identityDigest.Write(encoded[:])
		fingerprintBytes, _ := hex.DecodeString(fingerprint)
		_, _ = identityDigest.Write(fingerprintBytes)
		last = musicID
	}
	if err := rows.Err(); err != nil || len(musicIDs) == 0 {
		return catalogVerification{}, nil, nil, errors.New("filtered catalog has no valid ordered records")
	}
	musicIDsSHA, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil {
		return catalogVerification{}, nil, nil, err
	}
	return catalogVerification{
		ByteCount: int64(len(body)), SHA256: sha256Hex(body), SchemaVersion: schemaVersion,
		RuntimeSchemaVersion: lyricsextractionplan.MaximumCatalogRuntimeSchema,
		RecordCount:          len(musicIDs), IdentityPolicyVersion: policy,
		IdentitySHA256: hex.EncodeToString(identityDigest.Sum(nil)), MusicIDsSHA256: musicIDsSHA,
	}, musicIDs, titles, nil
}

func verifyFilteredCatalogAgainstTarget(
	target targetMapReport,
	verification catalogVerification,
	musicIDs []int,
	titles map[int]string,
) error {
	if verification.SchemaVersion != lyricsextractionplan.CatalogSchemaVersion ||
		verification.RuntimeSchemaVersion < verification.SchemaVersion || verification.RecordCount != target.MappingCount ||
		verification.IdentityPolicyVersion != lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity ||
		len(musicIDs) != len(target.Mappings) {
		return errors.New("filtered catalog schema, count, or identity policy is invalid")
	}
	for index, mapping := range target.Mappings {
		if musicIDs[index] != mapping.MusicID || titles[mapping.MusicID] != mapping.CatalogJapaneseTitle {
			return errors.New("filtered catalog does not exactly match the canonical target-map identities")
		}
	}
	return nil
}

func verifyFinalCatalogFile(path string, expected catalogVerification) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 || info.Size() != expected.ByteCount {
		return errors.New("final filtered catalog file identity is invalid")
	}
	if linkCount(info) != 1 {
		return errors.New("final filtered catalog must have exactly one hard link")
	}
	if err := rejectSQLiteSidecars(path); err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256Hex(body) != expected.SHA256 {
		return errors.New("final filtered catalog bytes changed during publication")
	}
	return nil
}

func parseOptions(arguments []string) (options, error) {
	var parsed options
	flags := flag.NewFlagSet("lyrics-catalog-filter", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&parsed.sourceCatalogPath, "source-catalog", "", "fixed immutable source catalog")
	flags.StringVar(&parsed.expectedSourceCatalogSHA, "expected-source-catalog-sha256", "", "exact source catalog SHA-256")
	flags.StringVar(&parsed.targetMapPath, "target-map", "", "fixed canonical target map")
	flags.StringVar(&parsed.expectedTargetMapSHA, "expected-target-map-sha256", "", "exact target-map SHA-256")
	flags.StringVar(&parsed.outputRoot, "output", "", "create-exclusive private output root")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return options{}, errors.New("lyrics catalog filtering accepts only named flags")
	}
	for _, path := range []string{parsed.sourceCatalogPath, parsed.targetMapPath, parsed.outputRoot} {
		if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return options{}, errors.New("lyrics catalog filtering paths must be canonical and absolute")
		}
	}
	if !canonicalSHA256.MatchString(parsed.expectedSourceCatalogSHA) || !canonicalSHA256.MatchString(parsed.expectedTargetMapSHA) {
		return options{}, errors.New("lyrics catalog filtering SHA-256 pins are invalid")
	}
	return parsed, nil
}

func decodeStrict(body []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func decimalMusicIDsSHA256(mappings []targetMapMapping) string {
	var encoded strings.Builder
	for _, mapping := range mappings {
		fmt.Fprintf(&encoded, "%d\n", mapping.MusicID)
	}
	return sha256Hex([]byte(encoded.String()))
}

func createPrivateOutputRoot(path string) error {
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0o700 {
		return errors.New("catalog filter output parent must exist with mode 0700")
	}
	return os.Mkdir(path, 0o700)
}

func readPinnedFile(path, expectedSHA string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Type() != 0 || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("catalog filter input is not a bounded regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if actual := sha256Hex(body); actual != expectedSHA {
		return nil, fmt.Errorf("catalog filter input SHA-256=%s, want %s", actual, expectedSHA)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("catalog filter input changed while being read")
	}
	return body, nil
}

func writeExclusiveFile(path string, body []byte, mode os.FileMode) error {
	if len(body) == 0 {
		return errors.New("refusing to write an empty catalog filter artifact")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(body)
	if writeErr == nil && written != len(body) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	return errors.Join(writeErr, file.Close())
}

func rejectSQLiteSidecars(path string) error {
	for _, suffix := range []string{"-journal", "-shm", "-wal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("filtered catalog has an unexpected SQLite %s sidecar", suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func linkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
