package main

import (
	"database/sql"

	"os"

	"time"

	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"
)

const (
	checkpointSchemaVersion                 = 2
	checkpointExecutionBindingVersion       = 1
	checkpointApplicationID                 = 0x4d4f4550 // "MOEP"
	checkpointPageSize                      = 4096
	maxCheckpointBytes                int64 = 128 << 20
	maxCheckpointPages                      = maxCheckpointBytes / checkpointPageSize
	maxCheckpointResultJSONBytes      int64 = 8 << 20
	maxCheckpointEvidenceJSONBytes    int64 = lyricsstaging.MaxPrivateEvidenceReceiptBytes
	checkpointValidationTimeout             = 30 * time.Second

	checkpointEvidenceReceiptPrefix = "{\n  \"schemaVersion\": 1,\n  \"indexEvidence\": ["
	checkpointEvidenceReceiptSuffix = "\n  ],\n  \"receiptSha256\": \"0000000000000000000000000000000000000000000000000000000000000000\"\n}\n"
)

const (
	checkpointTargetCatalogReview    = "catalog_review"
	checkpointTargetGameSizeEvidence = "game_size_evidence"
	checkpointTargetProviderWork     = "provider_work"
)

var checkpointResultClasses = map[string]struct{}{
	"catalog_review": {}, "game_size_evidence": {}, "unique_complete": {},
	"ambiguous": {}, "missing": {}, "incomplete": {}, "error": {},
}

type checkpointExecutionBinding struct {
	SchemaVersion             int   `json:"schemaVersion"`
	Concurrency               int   `json:"concurrency"`
	MaxAttempts               int   `json:"maxAttempts"`
	RequestTimeoutNanoseconds int64 `json:"requestTimeoutNanoseconds"`
	RetryDelayNanoseconds     int64 `json:"retryDelayNanoseconds"`
}

type checkpointCatalogRecord struct {
	MusicID             int    `json:"musicId"`
	JapaneseTitle       string `json:"japaneseTitle"`
	ProducerMetadata    string `json:"producerMetadata"`
	Lyricist            string `json:"lyricist"`
	Composer            string `json:"composer"`
	Arranger            string `json:"arranger"`
	CatalogFingerprint  string `json:"catalogFingerprint"`
	Disposition         string `json:"disposition"`
	TargetMusicID       int    `json:"targetMusicId"`
	AssociationMusicIDs []int  `json:"associationMusicIds"`
	ReasonCode          string `json:"reasonCode"`
}

type checkpointTargetBinding struct {
	CatalogItem catalogItem
	Target      model.CatalogLyricsTarget
	Kind        string
	Body        []byte
	SHA256      string
}

type checkpointStats struct {
	CatalogReview    int
	GameSizeEvidence int
	UniqueComplete   int
	Ambiguous        int
	Missing          int
	Incomplete       int
	Error            int
	Completed        int
	MissingWork      int
	EvidenceItems    int
	EvidenceRawBytes int64
}

type checkpointCounters struct {
	Stats                checkpointStats
	ResultJSONBytes      int64
	EvidenceJSONBytes    int64
	EvidenceReceiptBytes int64
}

type preflightCheckpoint struct {
	path               string
	operationalPath    string
	readOnly           bool
	database           *sql.DB
	pinnedFile         *os.File
	pinnedInfo         os.FileInfo
	parentFile         *os.File
	parentInfo         os.FileInfo
	catalogCount       int
	catalogFingerprint string
	executionBody      []byte
	executionSHA256    string
	generatedAt        string
	targets            map[int]checkpointTargetBinding
}

type checkpointTableColumn struct {
	name     string
	typeName string
	notNull  int
	primary  int
}

type checkpointForeignKey struct {
	table    string
	from     string
	to       string
	onUpdate string
	onDelete string
	match    string
}

type checkpointSchemaDefinition struct {
	name         string
	sql          string
	withoutRowID int
}

type checkpointPrivatePath struct {
	path       string
	parentPath string
	parentFile *os.File
	parentInfo os.FileInfo
}

var checkpointSchemaColumns = map[string][]checkpointTableColumn{
	"checkpoint_metadata": {
		{name: "singleton", typeName: "INTEGER", notNull: 1, primary: 1},
		{name: "checkpoint_schema_version", typeName: "INTEGER", notNull: 1},
		{name: "report_schema_version", typeName: "INTEGER", notNull: 1},
		{name: "catalog_schema_version", typeName: "INTEGER", notNull: 1},
		{name: "catalog_count", typeName: "INTEGER", notNull: 1},
		{name: "catalog_fingerprint", typeName: "TEXT", notNull: 1},
		{name: "generated_at", typeName: "TEXT", notNull: 1},
		{name: "execution_options_json", typeName: "TEXT", notNull: 1},
		{name: "execution_options_sha256", typeName: "TEXT", notNull: 1},
	},
	"checkpoint_counters": {
		{name: "singleton", typeName: "INTEGER", notNull: 1, primary: 1},
		{name: "catalog_review", typeName: "INTEGER", notNull: 1},
		{name: "game_size_evidence", typeName: "INTEGER", notNull: 1},
		{name: "unique_complete", typeName: "INTEGER", notNull: 1},
		{name: "ambiguous", typeName: "INTEGER", notNull: 1},
		{name: "missing", typeName: "INTEGER", notNull: 1},
		{name: "incomplete", typeName: "INTEGER", notNull: 1},
		{name: "error", typeName: "INTEGER", notNull: 1},
		{name: "completed", typeName: "INTEGER", notNull: 1},
		{name: "result_json_bytes", typeName: "INTEGER", notNull: 1},
		{name: "evidence_items", typeName: "INTEGER", notNull: 1},
		{name: "evidence_raw_bytes", typeName: "INTEGER", notNull: 1},
		{name: "evidence_json_bytes", typeName: "INTEGER", notNull: 1},
		{name: "evidence_receipt_bytes", typeName: "INTEGER", notNull: 1},
	},
	"catalog_targets": {
		{name: "music_id", typeName: "INTEGER", notNull: 1, primary: 1},
		{name: "target_kind", typeName: "TEXT", notNull: 1},
		{name: "target_json", typeName: "BLOB", notNull: 1},
		{name: "target_sha256", typeName: "TEXT", notNull: 1},
	},
	"results": {
		{name: "music_id", typeName: "INTEGER", notNull: 1, primary: 1},
		{name: "class", typeName: "TEXT", notNull: 1},
		{name: "result_json", typeName: "BLOB", notNull: 1},
		{name: "result_sha256", typeName: "TEXT", notNull: 1},
		{name: "evidence_item_count", typeName: "INTEGER", notNull: 1},
		{name: "evidence_raw_bytes", typeName: "INTEGER", notNull: 1},
	},
	"evidence": {
		{name: "evidence_id", typeName: "TEXT", notNull: 1, primary: 1},
		{name: "evidence_json", typeName: "BLOB", notNull: 1},
		{name: "evidence_sha256", typeName: "TEXT", notNull: 1},
		{name: "raw_byte_count", typeName: "INTEGER", notNull: 1},
	},
	"result_evidence": {
		{name: "music_id", typeName: "INTEGER", notNull: 1, primary: 1},
		{name: "evidence_id", typeName: "TEXT", notNull: 1, primary: 2},
	},
}

var checkpointSchemaDefinitions = []checkpointSchemaDefinition{
	{name: "checkpoint_metadata", sql: `CREATE TABLE checkpoint_metadata (
			singleton INTEGER NOT NULL PRIMARY KEY CHECK (singleton = 1),
			checkpoint_schema_version INTEGER NOT NULL CHECK (checkpoint_schema_version = 2),
			report_schema_version INTEGER NOT NULL CHECK (report_schema_version = 1),
			catalog_schema_version INTEGER NOT NULL CHECK (catalog_schema_version = 18),
			catalog_count INTEGER NOT NULL CHECK (catalog_count >= 0 AND catalog_count <= 100000),
			catalog_fingerprint TEXT NOT NULL CHECK (length(catalog_fingerprint) = 64),
			generated_at TEXT NOT NULL,
			execution_options_json TEXT NOT NULL,
			execution_options_sha256 TEXT NOT NULL CHECK (length(execution_options_sha256) = 64)
		) STRICT`},
	{name: "checkpoint_counters", sql: `CREATE TABLE checkpoint_counters (
			singleton INTEGER NOT NULL PRIMARY KEY CHECK (singleton = 1),
			catalog_review INTEGER NOT NULL CHECK (catalog_review >= 0 AND catalog_review <= 100000),
			game_size_evidence INTEGER NOT NULL CHECK (game_size_evidence >= 0 AND game_size_evidence <= 100000),
			unique_complete INTEGER NOT NULL CHECK (unique_complete >= 0 AND unique_complete <= 100000),
			ambiguous INTEGER NOT NULL CHECK (ambiguous >= 0 AND ambiguous <= 100000),
			missing INTEGER NOT NULL CHECK (missing >= 0 AND missing <= 100000),
			incomplete INTEGER NOT NULL CHECK (incomplete >= 0 AND incomplete <= 100000),
			error INTEGER NOT NULL CHECK (error >= 0 AND error <= 100000),
			completed INTEGER NOT NULL CHECK (completed >= 0 AND completed <= 100000 AND completed = catalog_review + game_size_evidence + unique_complete + ambiguous + missing + incomplete + error),
			result_json_bytes INTEGER NOT NULL CHECK (result_json_bytes >= 0 AND result_json_bytes <= 8388608),
			evidence_items INTEGER NOT NULL CHECK (evidence_items >= 0 AND evidence_items <= 65536),
			evidence_raw_bytes INTEGER NOT NULL CHECK (evidence_raw_bytes >= 0 AND evidence_raw_bytes <= 33554432),
			evidence_json_bytes INTEGER NOT NULL CHECK (evidence_json_bytes >= 0 AND evidence_json_bytes <= 67108864),
			evidence_receipt_bytes INTEGER NOT NULL CHECK (evidence_receipt_bytes >= 0 AND evidence_receipt_bytes <= 67108864),
			CHECK ((evidence_items = 0 AND evidence_raw_bytes = 0 AND evidence_json_bytes = 0 AND evidence_receipt_bytes = 0) OR (evidence_items > 0 AND evidence_raw_bytes > 0 AND evidence_json_bytes > 0 AND evidence_receipt_bytes > 0))
		) STRICT`},
	{name: "catalog_targets", sql: `CREATE TABLE catalog_targets (
			music_id INTEGER NOT NULL PRIMARY KEY,
			target_kind TEXT NOT NULL CHECK (target_kind IN ('catalog_review','game_size_evidence','provider_work')),
			target_json BLOB NOT NULL,
			target_sha256 TEXT NOT NULL CHECK (length(target_sha256) = 64)
		) STRICT`},
	{name: "results", sql: `CREATE TABLE results (
			music_id INTEGER NOT NULL PRIMARY KEY REFERENCES catalog_targets(music_id) ON DELETE RESTRICT,
			class TEXT NOT NULL CHECK (class IN ('catalog_review','game_size_evidence','unique_complete','ambiguous','missing','incomplete','error')),
			result_json BLOB NOT NULL,
			result_sha256 TEXT NOT NULL CHECK (length(result_sha256) = 64),
			evidence_item_count INTEGER NOT NULL CHECK (evidence_item_count >= 0 AND evidence_item_count <= 65536),
			evidence_raw_bytes INTEGER NOT NULL CHECK (evidence_raw_bytes >= 0 AND evidence_raw_bytes <= 33554432)
		) STRICT`},
	{name: "evidence", withoutRowID: 1, sql: `CREATE TABLE evidence (
			evidence_id TEXT NOT NULL PRIMARY KEY,
			evidence_json BLOB NOT NULL,
			evidence_sha256 TEXT NOT NULL CHECK (length(evidence_sha256) = 64),
			raw_byte_count INTEGER NOT NULL CHECK (raw_byte_count >= 0 AND raw_byte_count <= 2097152)
		) STRICT, WITHOUT ROWID`},
	{name: "result_evidence", withoutRowID: 1, sql: `CREATE TABLE result_evidence (
			music_id INTEGER NOT NULL REFERENCES results(music_id) ON DELETE RESTRICT,
			evidence_id TEXT NOT NULL REFERENCES evidence(evidence_id) ON DELETE RESTRICT,
			PRIMARY KEY (music_id, evidence_id)
		) STRICT, WITHOUT ROWID`},
}

var checkpointSchemaForeignKeys = map[string][]checkpointForeignKey{
	"results": {
		{table: "catalog_targets", from: "music_id", to: "music_id", onUpdate: "NO ACTION", onDelete: "RESTRICT", match: "NONE"},
	},
	"result_evidence": {
		{table: "evidence", from: "evidence_id", to: "evidence_id", onUpdate: "NO ACTION", onDelete: "RESTRICT", match: "NONE"},
		{table: "results", from: "music_id", to: "music_id", onUpdate: "NO ACTION", onDelete: "RESTRICT", match: "NONE"},
	},
}

// These hooks are nil outside deterministic cancellation, pathname-race, and crash-window tests.
var checkpointBeforeSQLiteOpenHook func(string, string)
var checkpointBeforeInitializationCommitHook func(string, string)
var checkpointAfterResultCommitHook func(string, string)
