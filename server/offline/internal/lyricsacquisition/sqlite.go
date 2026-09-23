package lyricsacquisition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"

	"errors"
	"fmt"
	"io"
	"os"

	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	sqlite "modernc.org/sqlite"
)

const (
	spoolApplicationID                  = 0x4d4f4541 // "MOEA"
	spoolPageSize                       = 4096
	maxMetadataBytes              int64 = 64 << 20
	maxMetadataPages                    = maxMetadataBytes / spoolPageSize
	metadataValidationTimeout           = 30 * time.Second
	metadataSelectorSchemaVersion       = 1
	maxMetadataSelectorBytes            = 1024
	maxMetadataRecoverySlots            = 32
	maxMetadataSelectorSlots            = 3
	maxMetadataStateEntries             = (maxAcquisitions+maxMetadataRecoverySlots+1)*2 + maxMetadataRecoverySlots + maxMetadataSelectorSlots

	metadataBoundaryAfterRuntimeSerialization metadataPersistenceBoundary = "after_runtime_serialization"

	metadataBoundaryBeforeRecoverySlotNamespaceSync metadataPersistenceBoundary = "before_recovery_slot_namespace_fsync"
	metadataBoundaryAfterRecoverySlotNamespaceSync  metadataPersistenceBoundary = "after_recovery_slot_namespace_fsync"
	metadataBoundaryBeforeSnapshotWrite             metadataPersistenceBoundary = "before_snapshot_write"
	metadataBoundaryAfterSnapshotWrite              metadataPersistenceBoundary = "after_snapshot_write"
	metadataBoundaryBeforeSnapshotFileSync          metadataPersistenceBoundary = "before_snapshot_file_fsync"
	metadataBoundaryAfterSnapshotFileSync           metadataPersistenceBoundary = "after_snapshot_file_fsync"
	metadataBoundaryBeforeSelectorWrite             metadataPersistenceBoundary = "before_selector_write"
	metadataBoundaryAfterSelectorWrite              metadataPersistenceBoundary = "after_selector_write"
	metadataBoundaryBeforeSelectorFileSync          metadataPersistenceBoundary = "before_selector_file_fsync"
	metadataBoundaryAfterSelectorFileSync           metadataPersistenceBoundary = "after_selector_file_fsync"
	metadataBoundaryBeforeSelectorNamespaceSync     metadataPersistenceBoundary = "before_selector_namespace_fsync"
	metadataBoundaryAfterSelectorNamespaceSync      metadataPersistenceBoundary = "after_selector_namespace_fsync"
	metadataBoundaryBeforeConnectorRebind           metadataPersistenceBoundary = "before_connector_rebind"
	metadataBoundaryAfterConnectorRebind            metadataPersistenceBoundary = "after_connector_rebind"
	metadataBoundaryBeforeStandbyVerification       metadataPersistenceBoundary = "before_post_rebind_standby_verification"
	metadataBoundaryAfterStandbyVerification        metadataPersistenceBoundary = "after_post_rebind_standby_verification"
)

type metadataPersistenceBoundary string

type metadataPersistenceOperation string

const (
	metadataOperationRuntimeSerialization      metadataPersistenceOperation = "runtime_serialization"
	metadataOperationRecoverySlotNamespaceSync metadataPersistenceOperation = "recovery_slot_namespace_fsync"
	metadataOperationSnapshotWrite             metadataPersistenceOperation = "snapshot_write"
	metadataOperationSnapshotFileSync          metadataPersistenceOperation = "snapshot_file_fsync"
	metadataOperationSelectorWrite             metadataPersistenceOperation = "selector_write"
	metadataOperationSelectorFileSync          metadataPersistenceOperation = "selector_file_fsync"
	metadataOperationSelectorNamespaceSync     metadataPersistenceOperation = "selector_namespace_fsync"
	metadataOperationConnectorRebind           metadataPersistenceOperation = "connector_rebind"
	metadataOperationStandbyVerification       metadataPersistenceOperation = "post_rebind_standby_verification"
)

type metadataBoundaryPhase string

const (
	metadataBoundaryPhaseBefore metadataBoundaryPhase = "before"
	metadataBoundaryPhaseAfter  metadataBoundaryPhase = "after"
)

type metadataBoundaryContract struct {
	operation metadataPersistenceOperation
	phase     metadataBoundaryPhase
}

type schemaDefinition struct {
	objectType   string
	name         string
	sql          string
	strict       bool
	withoutRowID bool
}

type tableColumn struct {
	name     string
	typeName string
	notNull  int
	primary  int
}

type foreignKey struct {
	table    string
	from     string
	to       string
	onUpdate string
	onDelete string
	match    string
}

type metadataCounters struct {
	requestCount     int64
	acquisitionCount int64
	rawBytes         int64
	evidenceBytes    int64
	envelopeBytes    int64
	manifestBytes    int64
}

type metadataSelector struct {
	SchemaVersion    int    `json:"schemaVersion"`
	Sequence         uint64 `json:"sequence"`
	Slot             string `json:"slot"`
	SHA256           string `json:"sha256"`
	ByteCount        int64  `json:"byteCount"`
	AcquisitionCount int64  `json:"acquisitionCount"`
}

type openedMetadataSlot struct {
	name             string
	file             *os.File
	stat             trustedStat
	digest           string
	acquisitionCount int64
	selectorBound    bool
}

type metadataSelectorRecord struct {
	name     string
	selector metadataSelector
}

var (
	metadataRecoverySlotPattern     = regexp.MustCompile(`^\.metadata\.db\.recovery\.([0-9]{2})$`)
	metadataSelectorNamePattern     = regexp.MustCompile(`^([0-9]{20})\.json(?:\.stage)?$`)
	metadataSelectorSlotNamePattern = regexp.MustCompile(`^selector\.([0-9]{2})\.json$`)
)

type acquisitionMetadata struct {
	acquisitionID            string
	requestKey               string
	provider                 string
	canonicalRequestIdentity string
	requestKind              string
	revisionSelector         string
	fetchedAt                string
	rawSHA256                string
	rawByteCount             int
	evidenceSHA256           string
	evidenceByteCount        int
	evidenceID               string
	envelopeSHA256           string
	envelopeByteCount        int
	manifestSHA256           string
	manifestByteCount        int
	observedRevisionCount    int
}

var spoolSchemaDefinitions = []schemaDefinition{
	{objectType: "table", name: "spool_metadata", strict: true, sql: `CREATE TABLE spool_metadata (
			singleton INTEGER NOT NULL PRIMARY KEY CHECK (singleton = 1),
			schema_version INTEGER NOT NULL CHECK (schema_version = 2)
		) STRICT`},
	{objectType: "table", name: "spool_counters", strict: true, sql: `CREATE TABLE spool_counters (
			singleton INTEGER NOT NULL PRIMARY KEY CHECK (singleton = 1),
			request_count INTEGER NOT NULL CHECK (request_count >= 0 AND request_count <= 65536),
			acquisition_count INTEGER NOT NULL CHECK (acquisition_count >= 0 AND acquisition_count <= 65536),
			raw_bytes INTEGER NOT NULL CHECK (raw_bytes >= 0 AND raw_bytes <= 34359738368),
			evidence_bytes INTEGER NOT NULL CHECK (evidence_bytes >= 0 AND evidence_bytes <= 34359738368),
			envelope_bytes INTEGER NOT NULL CHECK (envelope_bytes >= 0 AND envelope_bytes <= 34359738368),
			manifest_bytes INTEGER NOT NULL CHECK (manifest_bytes >= 0 AND manifest_bytes <= 4294967296),
			CHECK (request_count <= acquisition_count)
		) STRICT`},
	{objectType: "table", name: "requests", strict: true, withoutRowID: true, sql: `CREATE TABLE requests (
			request_key TEXT NOT NULL PRIMARY KEY CHECK (length(request_key) = 64),
			provider TEXT NOT NULL CHECK (length(provider) BETWEEN 1 AND 64),
			canonical_request_identity TEXT NOT NULL CHECK (length(CAST(canonical_request_identity AS BLOB)) BETWEEN 1 AND 8192),
			request_kind TEXT NOT NULL CHECK (request_kind IN ('search','revision','fixed_index')),
			revision_selector TEXT NOT NULL CHECK (length(CAST(revision_selector AS BLOB)) <= 1024)
		) STRICT, WITHOUT ROWID`},
	{objectType: "table", name: "acquisitions", strict: true, withoutRowID: true, sql: `CREATE TABLE acquisitions (
			acquisition_id TEXT NOT NULL PRIMARY KEY CHECK (length(acquisition_id) = 64),
			request_key TEXT NOT NULL REFERENCES requests(request_key) ON DELETE RESTRICT,
			provider TEXT NOT NULL CHECK (length(provider) BETWEEN 1 AND 64),
			canonical_request_identity TEXT NOT NULL CHECK (length(CAST(canonical_request_identity AS BLOB)) BETWEEN 1 AND 8192),
			request_kind TEXT NOT NULL CHECK (request_kind IN ('search','revision','fixed_index')),
			revision_selector TEXT NOT NULL CHECK (length(CAST(revision_selector AS BLOB)) <= 1024),
			fetched_at TEXT NOT NULL CHECK (length(fetched_at) BETWEEN 20 AND 35),
			raw_sha256 TEXT NOT NULL CHECK (length(raw_sha256) = 64),
			raw_byte_count INTEGER NOT NULL CHECK (raw_byte_count BETWEEN 1 AND 2097152),
			evidence_sha256 TEXT NOT NULL CHECK (length(evidence_sha256) = 64),
			evidence_byte_count INTEGER NOT NULL CHECK (evidence_byte_count BETWEEN 1 AND 2097152),
			evidence_id TEXT NOT NULL CHECK (length(CAST(evidence_id AS BLOB)) <= 256),
			envelope_sha256 TEXT NOT NULL CHECK (length(envelope_sha256) = 64),
			envelope_byte_count INTEGER NOT NULL CHECK (envelope_byte_count BETWEEN 1 AND 4194304),
			manifest_sha256 TEXT NOT NULL UNIQUE CHECK (length(manifest_sha256) = 64 AND manifest_sha256 = acquisition_id),
			manifest_byte_count INTEGER NOT NULL CHECK (manifest_byte_count BETWEEN 1 AND 65536),
			observed_revision_count INTEGER NOT NULL CHECK (observed_revision_count BETWEEN 0 AND 256)
		) STRICT, WITHOUT ROWID`},
}

var spoolSchemaColumns = map[string][]tableColumn{
	"spool_metadata": {
		{name: "singleton", typeName: "INTEGER", notNull: 1, primary: 1},
		{name: "schema_version", typeName: "INTEGER", notNull: 1},
	},
	"spool_counters": {
		{name: "singleton", typeName: "INTEGER", notNull: 1, primary: 1},
		{name: "request_count", typeName: "INTEGER", notNull: 1},
		{name: "acquisition_count", typeName: "INTEGER", notNull: 1},
		{name: "raw_bytes", typeName: "INTEGER", notNull: 1},
		{name: "evidence_bytes", typeName: "INTEGER", notNull: 1},
		{name: "envelope_bytes", typeName: "INTEGER", notNull: 1},
		{name: "manifest_bytes", typeName: "INTEGER", notNull: 1},
	},
	"requests": {
		{name: "request_key", typeName: "TEXT", notNull: 1, primary: 1},
		{name: "provider", typeName: "TEXT", notNull: 1},
		{name: "canonical_request_identity", typeName: "TEXT", notNull: 1},
		{name: "request_kind", typeName: "TEXT", notNull: 1},
		{name: "revision_selector", typeName: "TEXT", notNull: 1},
	},
	"acquisitions": {
		{name: "acquisition_id", typeName: "TEXT", notNull: 1, primary: 1},
		{name: "request_key", typeName: "TEXT", notNull: 1},
		{name: "provider", typeName: "TEXT", notNull: 1},
		{name: "canonical_request_identity", typeName: "TEXT", notNull: 1},
		{name: "request_kind", typeName: "TEXT", notNull: 1},
		{name: "revision_selector", typeName: "TEXT", notNull: 1},
		{name: "fetched_at", typeName: "TEXT", notNull: 1},
		{name: "raw_sha256", typeName: "TEXT", notNull: 1},
		{name: "raw_byte_count", typeName: "INTEGER", notNull: 1},
		{name: "evidence_sha256", typeName: "TEXT", notNull: 1},
		{name: "evidence_byte_count", typeName: "INTEGER", notNull: 1},
		{name: "evidence_id", typeName: "TEXT", notNull: 1},
		{name: "envelope_sha256", typeName: "TEXT", notNull: 1},
		{name: "envelope_byte_count", typeName: "INTEGER", notNull: 1},
		{name: "manifest_sha256", typeName: "TEXT", notNull: 1},
		{name: "manifest_byte_count", typeName: "INTEGER", notNull: 1},
		{name: "observed_revision_count", typeName: "INTEGER", notNull: 1},
	},
}

var spoolSchemaForeignKeys = map[string][]foreignKey{
	"acquisitions": {
		{table: "requests", from: "request_key", to: "request_key", onUpdate: "NO ACTION", onDelete: "RESTRICT", match: "NONE"},
	},
}

type pinnedMetadataBinding struct {
	mu       sync.RWMutex
	stat     trustedStat
	sha256   string
	pathName string
}

func (binding *pinnedMetadataBinding) snapshot() (trustedStat, string, string) {
	binding.mu.RLock()
	defer binding.mu.RUnlock()
	return binding.stat, binding.sha256, binding.pathName
}

func (binding *pinnedMetadataBinding) update(stat trustedStat, digest, pathName string) {
	binding.mu.Lock()
	binding.stat = stat
	binding.sha256 = digest
	binding.pathName = pathName
	binding.mu.Unlock()
}

type ledgerSQLiteConnector struct {
	mu sync.RWMutex

	driver       *sqlite.Driver
	dsn          string
	root         *privateRoot
	metadataFile *os.File
	binding      *pinnedMetadataBinding
}

var (
	testHookBeforeSQLiteRuntimeOpen func() error
	testHookBeforeSQLiteRestore     func() error
)

type sqliteRuntimeSerializer interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

func (connector *ledgerSQLiteConnector) Driver() driver.Driver {
	return connector.driver
}

func (connector *ledgerSQLiteConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connector.mu.RLock()
	defer connector.mu.RUnlock()
	if err := verifyPinnedMetadataDescriptor(connector.root, connector.metadataFile, connector.binding); err != nil {
		return nil, err
	}
	if testHookBeforeSQLiteRuntimeOpen != nil {
		if err := testHookBeforeSQLiteRuntimeOpen(); err != nil {
			return nil, err
		}
	}
	connection, err := connector.driver.Open(connector.dsn)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (driver.Conn, error) {
		_ = connection.Close()
		return nil, cause
	}
	if _, ok := connection.(sqliteRuntimeSerializer); !ok {
		return fail(errors.New("reviewed modernc SQLite connection has no serialization boundary"))
	}
	if err := configureSQLiteRuntimeConnection(ctx, connection); err != nil {
		return fail(err)
	}
	stat, _, _ := connector.binding.snapshot()
	if err := verifyModerncSQLiteMainFile(connection, stat); err != nil {
		return fail(err)
	}
	if err := verifyPinnedMetadataDescriptor(connector.root, connector.metadataFile, connector.binding); err != nil {
		return fail(err)
	}
	return connection, nil
}

func (connector *ledgerSQLiteConnector) rebindMetadataFile(
	file *os.File,
	stat trustedStat,
	digest string,
	pathName string,
	before func() error,
	after func() error,
) (*os.File, bool, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			return nil, false, err
		}
	}
	previous := connector.metadataFile
	connector.binding.update(stat, digest, pathName)
	connector.metadataFile = file
	if after != nil {
		if err := after(); err != nil {
			return previous, true, err
		}
	}
	return previous, true, nil
}

func configureSQLiteRuntimeConnection(ctx context.Context, connection driver.Conn) error {
	execer, ok := connection.(driver.ExecerContext)
	if !ok {
		return errors.New("reviewed modernc SQLite connection has no context-aware execution boundary")
	}
	for _, statement := range []string{
		`PRAGMA journal_mode=MEMORY`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA trusted_schema=OFF`,
		`PRAGMA temp_store=MEMORY`,
		`PRAGMA locking_mode=NORMAL`,
		fmt.Sprintf(`PRAGMA max_page_count=%d`, maxMetadataPages),
	} {
		if _, err := execer.ExecContext(ctx, statement, nil); err != nil {
			return fmt.Errorf("configure private in-memory acquisition metadata runtime: %w", err)
		}
	}
	return nil
}

func metadataSlotNameAllowed(name string) bool {
	if name == metadataFileName || name == metadataSnapshotTempName {
		return true
	}
	match := metadataRecoverySlotPattern.FindStringSubmatch(name)
	if match == nil {
		return false
	}
	index, err := strconv.Atoi(match[1])
	return err == nil && index >= 0 && index < maxMetadataRecoverySlots
}

func metadataRecoverySlotName(index int) string {
	return fmt.Sprintf(".metadata.db.recovery.%02d", index)
}

func metadataSelectorFileName(sequence uint64) string {
	return fmt.Sprintf("%020d.json", sequence)
}

func metadataSelectorStageName(sequence uint64) string {
	return metadataSelectorFileName(sequence) + ".stage"
}

func metadataSelectorSlotName(index int) string {
	return fmt.Sprintf("selector.%02d.json", index)
}

func metadataSelectorNameAllowed(name string) bool {
	if metadataSelectorNamePattern.MatchString(name) {
		return true
	}
	match := metadataSelectorSlotNamePattern.FindStringSubmatch(name)
	if match == nil {
		return false
	}
	index, err := strconv.Atoi(match[1])
	return err == nil && index >= 0 && index < maxMetadataSelectorSlots
}

func openMetadataSlot(root *privateRoot, name string, create bool) (*openedMetadataSlot, error) {
	if root == nil || !metadataSlotNameAllowed(name) {
		return nil, errors.New("private acquisition metadata slot name is invalid")
	}
	var before trustedStat
	var err error
	if !create {
		before, err = statAt(root.file, name)
		if err != nil {
			return nil, err
		}
		if err := validatePrivateRegularStat(before, "private acquisition metadata slot", 1); err != nil {
			return nil, err
		}
		if before.size < 0 || before.size > maxMetadataBytes {
			return nil, errors.New("private acquisition metadata slot has an invalid byte count")
		}
		if err := root.verifyKnownLeaf("", name, before.identity); err != nil {
			return nil, err
		}
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(int(root.file.Fd()), name, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open private acquisition metadata slot %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("private acquisition metadata slot descriptor is invalid")
	}
	fail := func(cause error) (*openedMetadataSlot, error) {
		return nil, errors.Join(cause, file.Close())
	}
	if create {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			return fail(fmt.Errorf("secure private acquisition metadata slot: %w", err))
		}
	}
	opened, err := fstatFile(file)
	if err != nil {
		return fail(err)
	}
	pathStat, pathErr := statAt(root.file, name)
	if err != nil || pathErr != nil || !sameTrustedMetadata(opened, pathStat) || !create && !sameTrustedMetadata(before, opened) {
		return fail(errors.Join(errors.New("private acquisition metadata slot changed while being pinned"), err, pathErr))
	}
	if err := validatePrivateRegularStat(opened, "private acquisition metadata slot", 1); err != nil {
		return fail(err)
	}
	if opened.size < 0 || opened.size > maxMetadataBytes {
		return fail(errors.New("private acquisition metadata slot has an invalid byte count"))
	}
	if create {
		if err := file.Sync(); err != nil {
			return fail(fmt.Errorf("sync private acquisition metadata slot reservation: %w", err))
		}
		root.rememberLeaf("", name, opened.identity)
	}
	_, digest, err := readPinnedMetadataDescriptorAt(root, file, opened, true, name, "private acquisition metadata slot")
	if err != nil {
		return fail(err)
	}
	return &openedMetadataSlot{name: name, file: file, stat: opened, digest: digest}, nil
}

func inspectMetadataSnapshotCandidate(ctx context.Context, root *privateRoot, slot *openedMetadataSlot) error {
	if ctx == nil || root == nil || slot == nil || slot.file == nil || slot.stat.size < 100 {
		return errors.New("private acquisition metadata candidate is incomplete")
	}
	body, digest, err := readPinnedMetadataDescriptorAt(root, slot.file, slot.stat, false, slot.name, "private acquisition metadata candidate")
	if err != nil {
		return err
	}
	if len(body) < 100 || !bytes.Equal(body[:16], []byte("SQLite format 3\x00")) {
		return errors.New("private acquisition metadata candidate has an invalid SQLite header")
	}
	descriptorPath := fmt.Sprintf("/dev/fd/%d", slot.file.Fd())
	database, err := sql.Open("sqlite", "file:"+descriptorPath+"?mode=ro&immutable=1")
	if err != nil {
		return err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	validationCtx, cancel := context.WithTimeout(ctx, metadataValidationTimeout)
	defer cancel()
	var applicationID, userVersion, pageSize int64
	for query, destination := range map[string]*int64{
		`PRAGMA application_id`: &applicationID,
		`PRAGMA user_version`:   &userVersion,
		`PRAGMA page_size`:      &pageSize,
	} {
		if err := database.QueryRowContext(validationCtx, query).Scan(destination); err != nil {
			return err
		}
	}
	if applicationID != spoolApplicationID || userVersion != spoolSchemaVersion || pageSize != spoolPageSize {
		return errors.New("private acquisition metadata candidate envelope is invalid")
	}
	var integrity string
	if err := database.QueryRowContext(validationCtx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return errors.Join(errors.New("private acquisition metadata candidate integrity check failed"), err)
	}
	candidate := &spool{database: database}
	if err := candidate.validateMetadataSchema(validationCtx); err != nil {
		return err
	}
	if err := candidate.validateMetadataCounters(validationCtx); err != nil {
		return err
	}
	if err := database.QueryRowContext(validationCtx, `SELECT acquisition_count FROM spool_counters WHERE singleton=1`).Scan(&slot.acquisitionCount); err != nil {
		return err
	}
	if _, actualDigest, err := readPinnedMetadataDescriptorAt(root, slot.file, slot.stat, false, slot.name, "private acquisition metadata candidate"); err != nil || actualDigest != digest {
		return errors.Join(errors.New("private acquisition metadata candidate changed while being inspected"), err)
	}
	slot.digest = digest
	return nil
}

func decodeMetadataSelector(body []byte, name string) (metadataSelector, error) {
	var selector metadataSelector
	if len(body) == 0 || len(body) > maxMetadataSelectorBytes {
		return selector, errors.New("metadata selector exceeds its byte bound")
	}
	if err := decodeClosedCanonicalJSON(body, &selector); err != nil {
		return selector, err
	}
	legacy := metadataSelectorNamePattern.FindStringSubmatch(name)
	fixed := metadataSelectorSlotNamePattern.FindStringSubmatch(name)
	if strings.HasSuffix(name, ".stage") || legacy == nil && fixed == nil {
		return selector, errors.New("metadata selector name is invalid")
	}
	if selector.SchemaVersion != metadataSelectorSchemaVersion || selector.Sequence == 0 ||
		!metadataSlotNameAllowed(selector.Slot) || !canonicalSHA256.MatchString(selector.SHA256) || selector.ByteCount < 100 ||
		selector.ByteCount > maxMetadataBytes || selector.AcquisitionCount < 0 || selector.AcquisitionCount > maxAcquisitions {
		return selector, errors.New("metadata selector binding is invalid")
	}
	if legacy != nil {
		sequence, err := strconv.ParseUint(legacy[1], 10, 64)
		if err != nil || selector.Sequence != sequence {
			return selector, errors.New("legacy metadata selector sequence is invalid")
		}
	}
	return selector, nil
}

func (s *spool) readMetadataSelectors() (*metadataSelector, map[string]metadataSelector, uint64, []metadataSelectorRecord, error) {
	latestBySlot := make(map[string]metadataSelector)
	if _, found := s.root.directories[metadataStateDirectory]; !found {
		return nil, latestBySlot, 0, nil, nil
	}
	if err := s.root.captureDirectoryLeaves(metadataStateDirectory, maxMetadataStateEntries); err != nil {
		return nil, nil, 0, nil, err
	}
	entries, err := sortedDirectoryEntries(s.root, metadataStateDirectory, maxMetadataStateEntries)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	var latest *metadataSelector
	var maximumSequence uint64
	records := make([]metadataSelectorRecord, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !metadataSelectorNameAllowed(name) {
			return nil, nil, 0, nil, fmt.Errorf("metadata state directory contains unknown entry %q", name)
		}
		stat, err := statAt(s.root.directories[metadataStateDirectory].file, name)
		if err != nil {
			return nil, nil, 0, nil, err
		}
		if err := validatePrivateRegularStat(stat, "metadata selector state", 1, 2); err != nil {
			return nil, nil, 0, nil, err
		}
		if stat.size < 0 || stat.size > maxMetadataSelectorBytes {
			return nil, nil, 0, nil, errors.New("metadata selector state exceeds its byte bound")
		}
		if strings.HasSuffix(name, ".stage") {
			continue
		}
		body, _, err := readVerifiedFileAt(s.root, metadataStateDirectory, name, "metadata selector", "", -1, maxMetadataSelectorBytes, 1, 2)
		if err != nil {
			return nil, nil, 0, nil, err
		}
		selector, decodeErr := decodeMetadataSelector(body, name)
		if decodeErr != nil {
			if metadataSelectorSlotNamePattern.MatchString(name) {
				continue
			}
			return nil, nil, 0, nil, decodeErr
		}
		records = append(records, metadataSelectorRecord{name: name, selector: selector})
		if selector.Sequence > maximumSequence {
			maximumSequence = selector.Sequence
		}
		previous, found := latestBySlot[selector.Slot]
		if !found || selector.Sequence > previous.Sequence {
			latestBySlot[selector.Slot] = selector
		}
		if latest == nil || selector.Sequence > latest.Sequence {
			copy := selector
			latest = &copy
		}
	}
	sort.Slice(records, func(left, right int) bool {
		return records[left].selector.Sequence > records[right].selector.Sequence
	})
	return latest, latestBySlot, maximumSequence, records, nil
}

func (s *spool) openExistingMetadataSlots(ctx context.Context) (*openedMetadataSlot, *openedMetadataSlot, *openedMetadataSlot, error) {
	latest, latestBySlot, _, _, err := s.readMetadataSelectors()
	if err != nil {
		return nil, nil, nil, err
	}
	entries, err := sortedDirectoryEntries(s.root, "", 9+maxMetadataRecoverySlots)
	if err != nil {
		return nil, nil, nil, err
	}
	names := make([]string, 0, maxMetadataRecoverySlots+2)
	for _, entry := range entries {
		if metadataSlotNameAllowed(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	slots := make(map[string]*openedMetadataSlot, len(names))
	closeSlots := func(except ...*openedMetadataSlot) {
		kept := make(map[*openedMetadataSlot]bool, len(except))
		for _, slot := range except {
			kept[slot] = true
		}
		for _, slot := range slots {
			if slot != nil && !kept[slot] {
				_ = slot.file.Close()
			}
		}
	}
	for _, name := range names {
		slot, openErr := openMetadataSlot(s.root, name, false)
		if openErr != nil {
			closeSlots()
			return nil, nil, nil, openErr
		}
		slots[name] = slot
	}
	metadata, found := slots[metadataFileName]
	if !found {
		closeSlots()
		return nil, nil, nil, errors.New("existing lyrics acquisition spool is missing its historical metadata.db slot")
	}
	var active *openedMetadataSlot
	if latest == nil {
		if err := inspectMetadataSnapshotCandidate(ctx, s.root, metadata); err != nil {
			closeSlots()
			return nil, nil, nil, fmt.Errorf("validate historical metadata.db snapshot: %w", err)
		}
		metadata.selectorBound = true
		active = metadata
	} else {
		selected, found := slots[latest.Slot]
		if !found {
			closeSlots()
			return nil, nil, nil, errors.New("latest metadata selector references a missing slot")
		}
		if selected.stat.size != latest.ByteCount || selected.digest != latest.SHA256 {
			closeSlots()
			return nil, nil, nil, errors.New("latest metadata selector digest does not match its pinned slot")
		}
		if err := inspectMetadataSnapshotCandidate(ctx, s.root, selected); err != nil {
			closeSlots()
			return nil, nil, nil, fmt.Errorf("validate latest selected metadata snapshot: %w", err)
		}
		if selected.acquisitionCount != latest.AcquisitionCount {
			closeSlots()
			return nil, nil, nil, errors.New("latest metadata selector acquisition count does not match its slot")
		}
		selected.selectorBound = true
		active = selected
	}
	var standby *openedMetadataSlot
	var standbySequence uint64
	var fallback *openedMetadataSlot
	for _, name := range names {
		candidate := slots[name]
		if candidate == active || inspectMetadataSnapshotCandidate(ctx, s.root, candidate) != nil {
			continue
		}
		binding, bound := latestBySlot[name]
		if bound && candidate.stat.size == binding.ByteCount && candidate.digest == binding.SHA256 &&
			candidate.acquisitionCount == binding.AcquisitionCount {
			candidate.selectorBound = true
			if standby == nil || binding.Sequence > standbySequence {
				standby = candidate
				standbySequence = binding.Sequence
			}
			continue
		}
		if fallback == nil || candidate.acquisitionCount > fallback.acquisitionCount {
			fallback = candidate
		}
	}
	if standby == nil {
		standby = fallback
	}
	var reusable *openedMetadataSlot
	for _, name := range names {
		candidate := slots[name]
		if candidate != active && candidate != standby {
			reusable = candidate
			break
		}
	}
	closeSlots(active, standby, reusable)
	return active, standby, reusable, nil
}

func (s *spool) openMetadata(ctx context.Context, initializeMissing bool) error {
	if err := requireAtomicNamespacePublication(); err != nil {
		return err
	}
	if err := s.root.verify(); err != nil {
		return err
	}
	var active, standby, reusable *openedMetadataSlot
	if initializeMissing {
		created, err := openMetadataSlot(s.root, metadataFileName, true)
		if err != nil {
			return err
		}
		active = created
	} else {
		var err error
		active, standby, reusable, err = s.openExistingMetadataSlots(ctx)
		if err != nil {
			return err
		}
	}
	if err := validateMetadataSidecars(s.root, false); err != nil {
		_ = active.file.Close()
		if standby != nil {
			_ = standby.file.Close()
		}
		if reusable != nil {
			_ = reusable.file.Close()
		}
		return err
	}
	binding := &pinnedMetadataBinding{stat: active.stat, sha256: active.digest, pathName: active.name}
	connector := &ledgerSQLiteConnector{
		driver: &sqlite.Driver{},
		dsn: fmt.Sprintf("file:lyrics-acquisition-runtime-%x-%x-%x?mode=memory&cache=shared&_txlock=immediate",
			s.root.stat.identity.device, s.root.stat.identity.inode, active.stat.identity.inode),
		root: s.root, metadataFile: active.file, binding: binding,
	}
	database := sql.OpenDB(connector)
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(0)
	s.metadataFile = active.file
	s.metadataBinding = binding
	if standby != nil {
		s.metadataStandbyFile = standby.file
		s.metadataStandbyStat = standby.stat
		s.metadataStandbyDigest = standby.digest
		s.metadataStandbyPath = standby.name
	}
	if reusable != nil {
		s.metadataWriteFile = reusable.file
		s.metadataWriteStat = reusable.stat
		s.metadataWritePath = reusable.name
	}
	s.metadataConnector = connector
	s.database = database
	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("open private in-memory acquisition metadata runtime: %w", err)
	}
	if !initializeMissing {
		if err := restoreMetadataRuntime(ctx, database, s.root, active.file, binding); err != nil {
			return err
		}
	}
	if initializeMissing {
		if _, err := database.ExecContext(ctx, fmt.Sprintf(`PRAGMA page_size=%d`, spoolPageSize)); err != nil {
			return fmt.Errorf("configure private acquisition metadata page size: %w", err)
		}
	}
	if err := s.configureMetadataDatabase(ctx, initializeMissing); err != nil {
		return err
	}
	if initializeMissing {
		if err := s.initializeMetadataSchema(ctx); err != nil {
			return err
		}
		snapshot, err := s.captureMetadataRuntimeSnapshot(ctx)
		if err != nil {
			return err
		}
		if err := s.persistInitialMetadataSnapshot(snapshot); err != nil {
			return err
		}
	}
	anchor, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("retain private acquisition metadata runtime owner: %w", err)
	}
	s.metadataAnchor = anchor
	if err := s.syncMetadata("SQLite runtime open"); err != nil {
		return err
	}
	if err := s.validateMetadataState(ctx); err != nil {
		return err
	}
	return s.verifyRetainedMetadataSnapshot()
}

func sameTrustedMetadata(left, right trustedStat) bool {
	return sameTrustedMetadataIdentity(left, right) && left.size == right.size
}

func sameTrustedMetadataIdentity(left, right trustedStat) bool {
	return sameFileIdentity(left.identity, right.identity) && left.mode == right.mode && left.links == right.links && left.owner == right.owner
}

func readPinnedMetadataDescriptorAt(
	root *privateRoot,
	file *os.File,
	expected trustedStat,
	allowEmpty bool,
	pathName string,
	label string,
) ([]byte, string, error) {
	if err := root.verify(); err != nil {
		return nil, "", err
	}
	opened, err := fstatFile(file)
	if err != nil || !sameTrustedMetadata(expected, opened) {
		return nil, "", fmt.Errorf("%s descriptor changed", label)
	}
	pathStat, err := statAt(root.file, pathName)
	if err != nil || !sameTrustedMetadata(expected, pathStat) {
		return nil, "", fmt.Errorf("%s pathname, inode, or size changed", label)
	}
	if err := validatePrivateRegularStat(opened, label, 1); err != nil {
		return nil, "", err
	}
	if opened.size < 0 || opened.size > maxMetadataBytes || !allowEmpty && opened.size == 0 {
		return nil, "", fmt.Errorf("%s has an invalid byte count", label)
	}
	body, err := io.ReadAll(io.NewSectionReader(file, 0, opened.size+1))
	if err != nil {
		return nil, "", fmt.Errorf("read pinned %s: %w", label, err)
	}
	if int64(len(body)) != opened.size {
		return nil, "", fmt.Errorf("%s size changed while being read", label)
	}
	after, statErr := fstatFile(file)
	pathAfter, pathErr := statAt(root.file, pathName)
	if statErr != nil || pathErr != nil || !sameTrustedMetadata(opened, after) || !sameTrustedMetadata(opened, pathAfter) {
		return nil, "", fmt.Errorf("%s pathname, inode, or size changed while being read", label)
	}
	digest := sha256.Sum256(body)
	return body, hex.EncodeToString(digest[:]), nil
}

func verifyPinnedMetadataDescriptor(root *privateRoot, metadataFile *os.File, binding *pinnedMetadataBinding) error {
	if binding == nil {
		return errors.New("private acquisition metadata binding is required")
	}
	expected, expectedDigest, pathName := binding.snapshot()
	_, actualDigest, err := readPinnedMetadataDescriptorAt(root, metadataFile, expected, expected.size == 0, pathName, "private acquisition metadata index")
	if err != nil {
		return err
	}
	if actualDigest != expectedDigest {
		return errors.New("private acquisition metadata bytes changed")
	}
	return nil
}
