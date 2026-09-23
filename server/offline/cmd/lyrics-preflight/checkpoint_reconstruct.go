package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"time"

	"moesekai/server/internal/lyricssource"
)

func decodeCanonicalCheckpointJSON(body []byte, target any) error {
	if err := decodeClosedJSON(body, target); err != nil {
		return err
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, canonical) {
		return errors.New("checkpoint JSON is not canonical")
	}
	return nil
}

func decodeStoredCheckpointEvidence(key string, body []byte, wantSHA string, rawByteCount int64) (lyricssource.IndexEvidence, error) {
	var evidence lyricssource.IndexEvidence
	if err := decodeCanonicalCheckpointJSON(body, &evidence); err != nil {
		return evidence, fmt.Errorf("decode canonical checkpoint exact evidence: %w", err)
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != wantSHA {
		return evidence, errors.New("checkpoint evidence SHA does not bind its canonical JSON")
	}
	if key == "" || evidence.EvidenceID != key {
		return evidence, errors.New("checkpoint evidence table key does not match its embedded evidence ID")
	}
	if rawByteCount != int64(len(evidence.Raw)) {
		return evidence, errors.New("checkpoint evidence raw-byte count does not match decoded raw bytes")
	}
	if len(evidence.Raw) > lyricssource.MaxIndexEvidenceRawBytes {
		return evidence, errors.New("checkpoint evidence exceeds the per-evidence 2 MiB raw-byte bound")
	}
	return evidence, nil
}

func (checkpoint *preflightCheckpoint) reconstruct(ctx context.Context) (report, *preflightEvidenceAggregator, map[int]struct{}, error) {
	generated := report{
		SchemaVersion: reportSchemaVersion, GeneratedAt: checkpoint.generatedAt,
		CatalogSchemaVersion: catalogSchemaVersion, CatalogCount: checkpoint.catalogCount,
		CatalogReview: []reportItem{}, GameSizeEvidence: []reportItem{}, UniqueComplete: []reportItem{},
		Ambiguous: []reportItem{}, Missing: []reportItem{}, Incomplete: []reportItem{}, Error: []reportItem{},
	}
	completed := make(map[int]struct{}, checkpoint.catalogCount)
	rows, err := checkpoint.database.QueryContext(ctx, `SELECT music_id,class,result_json,result_sha256 FROM results ORDER BY music_id`)
	if err != nil {
		return report{}, nil, nil, fmt.Errorf("read checkpoint results: %w", err)
	}
	for rows.Next() {
		var musicID int
		var class string
		var body []byte
		var wantSHA string
		if err := rows.Scan(&musicID, &class, &body, &wantSHA); err != nil {
			rows.Close()
			return report{}, nil, nil, err
		}
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != wantSHA {
			rows.Close()
			return report{}, nil, nil, errors.New("checkpoint result digest does not match")
		}
		var item reportItem
		if err := decodeCanonicalCheckpointJSON(body, &item); err != nil {
			rows.Close()
			return report{}, nil, nil, fmt.Errorf("decode checkpoint result: %w", err)
		}
		if item.MusicID != musicID {
			rows.Close()
			return report{}, nil, nil, errors.New("checkpoint result music ID does not match its key")
		}
		if _, duplicate := completed[musicID]; duplicate {
			rows.Close()
			return report{}, nil, nil, errors.New("checkpoint contains duplicate result music IDs")
		}
		completed[musicID] = struct{}{}
		switch class {
		case "catalog_review":
			generated.CatalogReview = append(generated.CatalogReview, item)
		case "game_size_evidence":
			generated.GameSizeEvidence = append(generated.GameSizeEvidence, item)
		case "unique_complete":
			generated.UniqueComplete = append(generated.UniqueComplete, item)
		case "ambiguous":
			generated.Ambiguous = append(generated.Ambiguous, item)
		case "missing":
			generated.Missing = append(generated.Missing, item)
		case "incomplete":
			generated.Incomplete = append(generated.Incomplete, item)
		case "error":
			generated.Error = append(generated.Error, item)
		default:
			rows.Close()
			return report{}, nil, nil, errors.New("checkpoint contains an unsupported result class")
		}
	}
	if err := rows.Close(); err != nil {
		return report{}, nil, nil, err
	}
	if err := rows.Err(); err != nil {
		return report{}, nil, nil, err
	}

	aggregator := newPreflightEvidenceAggregator()
	evidenceRows, err := checkpoint.database.QueryContext(ctx, `SELECT evidence_id,evidence_json,evidence_sha256,raw_byte_count FROM evidence ORDER BY evidence_id`)
	if err != nil {
		return report{}, nil, nil, fmt.Errorf("read checkpoint exact evidence: %w", err)
	}
	for evidenceRows.Next() {
		var key string
		var body []byte
		var wantSHA string
		var rawByteCount int64
		if err := evidenceRows.Scan(&key, &body, &wantSHA, &rawByteCount); err != nil {
			evidenceRows.Close()
			return report{}, nil, nil, err
		}
		evidence, err := decodeStoredCheckpointEvidence(key, body, wantSHA, rawByteCount)
		if err != nil {
			evidenceRows.Close()
			return report{}, nil, nil, err
		}
		if err := aggregator.add([]lyricssource.IndexEvidence{evidence}); err != nil {
			evidenceRows.Close()
			return report{}, nil, nil, err
		}
	}
	if err := evidenceRows.Close(); err != nil {
		return report{}, nil, nil, err
	}
	if err := evidenceRows.Err(); err != nil {
		return report{}, nil, nil, err
	}
	sortReport(&generated)
	generated.Summary = reportSummary{
		CatalogReview: len(generated.CatalogReview), GameSizeEvidence: len(generated.GameSizeEvidence),
		UniqueComplete: len(generated.UniqueComplete), Ambiguous: len(generated.Ambiguous),
		Missing: len(generated.Missing), Incomplete: len(generated.Incomplete), Error: len(generated.Error),
	}
	return generated, aggregator, completed, nil
}

func (checkpoint *preflightCheckpoint) validateState(ctx context.Context, requireComplete bool) error {
	if checkpoint == nil || checkpoint.database == nil {
		return errors.New("checkpoint database is required")
	}
	if err := checkpoint.verifyFile("during validation"); err != nil {
		return err
	}
	if err := validateCheckpointSharedCapacityContract(); err != nil {
		return err
	}
	var applicationID, userVersion, pageSize, maxPages int64
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		return err
	}
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return err
	}
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return err
	}
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA max_page_count`).Scan(&maxPages); err != nil {
		return err
	}
	if applicationID != checkpointApplicationID || userVersion != checkpointSchemaVersion || pageSize != checkpointPageSize ||
		(!checkpoint.readOnly && maxPages != maxCheckpointPages) {
		return errors.New("checkpoint SQLite envelope or version is invalid")
	}
	var foreignKeys, trustedSchema, tempStore int
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA trusted_schema`).Scan(&trustedSchema); err != nil {
		return err
	}
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&tempStore); err != nil {
		return err
	}
	if foreignKeys != 1 || trustedSchema != 0 || tempStore != 2 {
		return errors.New("checkpoint SQLite safety or in-memory temp-store pragmas are invalid")
	}
	if !checkpoint.readOnly {
		var journalMode string
		var synchronous int
		if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			return err
		}
		if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
			return err
		}
		if !strings.EqualFold(journalMode, "delete") || synchronous != 2 {
			return errors.New("checkpoint durability pragmas are invalid")
		}
	}
	var integrity string
	if err := checkpoint.database.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("check checkpoint integrity: %w", err)
	}
	if integrity != "ok" {
		return errors.New("checkpoint integrity check failed")
	}
	foreignKeyRows, err := checkpoint.database.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check checkpoint foreign keys: %w", err)
	}
	if foreignKeyRows.Next() {
		foreignKeyRows.Close()
		return errors.New("checkpoint foreign-key check failed")
	}
	if err := foreignKeyRows.Close(); err != nil {
		return err
	}
	var mainCount, unexpectedAttachments int
	var mainFile string
	if err := checkpoint.database.QueryRowContext(ctx, `SELECT COUNT(*),MAX(file) FROM pragma_database_list WHERE name='main'`).Scan(&mainCount, &mainFile); err != nil {
		return err
	}
	if err := checkpoint.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_database_list WHERE name NOT IN ('main','temp')`).Scan(&unexpectedAttachments); err != nil {
		return err
	}
	if mainCount != 1 || unexpectedAttachments != 0 || filepath.Clean(mainFile) != filepath.Clean(checkpoint.operationalPath) {
		return errors.New("checkpoint has unexpected attached databases or main-database pathname")
	}
	if err := checkpoint.validateSchema(ctx); err != nil {
		return err
	}

	var schemaVersion, reportVersion, catalogVersion, catalogCount int
	var catalogFingerprint, generatedAt, executionJSON, executionSHA string
	if err := checkpoint.database.QueryRowContext(ctx, `SELECT checkpoint_schema_version,report_schema_version,
		catalog_schema_version,catalog_count,catalog_fingerprint,generated_at,execution_options_json,execution_options_sha256
		FROM checkpoint_metadata WHERE singleton=1`).Scan(&schemaVersion, &reportVersion, &catalogVersion, &catalogCount,
		&catalogFingerprint, &generatedAt, &executionJSON, &executionSHA); err != nil {
		return fmt.Errorf("read checkpoint metadata: %w", err)
	}
	var metadataCount int
	if err := checkpoint.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoint_metadata`).Scan(&metadataCount); err != nil {
		return err
	}
	if metadataCount != 1 || schemaVersion != checkpointSchemaVersion || reportVersion != reportSchemaVersion ||
		catalogVersion != catalogSchemaVersion || catalogCount != checkpoint.catalogCount ||
		catalogFingerprint != checkpoint.catalogFingerprint || executionJSON != string(checkpoint.executionBody) ||
		executionSHA != checkpoint.executionSHA256 {
		return errors.New("checkpoint metadata does not match the current catalog and execution options")
	}
	executionDigest := sha256.Sum256([]byte(executionJSON))
	if hex.EncodeToString(executionDigest[:]) != executionSHA {
		return errors.New("checkpoint execution-option digest does not match")
	}
	var execution checkpointExecutionBinding
	if err := decodeCanonicalCheckpointJSON([]byte(executionJSON), &execution); err != nil ||
		!reflect.DeepEqual(execution, checkpointExecutionBindingFor(options{
			Concurrency: execution.Concurrency, MaxAttempts: execution.MaxAttempts,
			RequestTimeout: time.Duration(execution.RequestTimeoutNanoseconds), RetryDelay: time.Duration(execution.RetryDelayNanoseconds),
		})) {
		return errors.New("checkpoint execution-option binding is invalid")
	}
	parsedGeneratedAt, err := time.Parse(time.RFC3339Nano, generatedAt)
	if err != nil || parsedGeneratedAt.UTC().Format(time.RFC3339Nano) != generatedAt || !strings.HasSuffix(generatedAt, "Z") {
		return errors.New("checkpoint generated timestamp is invalid")
	}
	checkpoint.generatedAt = generatedAt

	if err := checkpoint.validateCatalogTargets(ctx); err != nil {
		return err
	}
	generated, _, completed, err := checkpoint.reconstruct(ctx)
	if err != nil {
		return err
	}
	if len(completed) > checkpoint.catalogCount || requireComplete && len(completed) != checkpoint.catalogCount {
		return errors.New("checkpoint does not contain the required number of results")
	}
	if err := validateClassifiedReportItems(generated); err != nil {
		return fmt.Errorf("validate checkpoint result items: %w", err)
	}
	if err := checkpoint.validateResultEvidence(ctx, generated); err != nil {
		return err
	}
	if err := checkpoint.validateCounters(ctx, generated); err != nil {
		return err
	}
	return checkpoint.verifyFile("after validation")
}

func (checkpoint *preflightCheckpoint) validateSchema(ctx context.Context) error {
	rows, err := checkpoint.database.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)
	if err != nil {
		return err
	}
	type schemaObject struct {
		objectType string
		sql        string
	}
	objects := make(map[string]schemaObject)
	for rows.Next() {
		var objectType, name, createSQL string
		if err := rows.Scan(&objectType, &name, &createSQL); err != nil {
			rows.Close()
			return err
		}
		objects[name] = schemaObject{objectType: objectType, sql: createSQL}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(objects) != len(checkpointSchemaDefinitions) {
		return errors.New("checkpoint schema contains unexpected objects")
	}
	for _, definition := range checkpointSchemaDefinitions {
		table := definition.name
		expected := checkpointSchemaColumns[table]
		object, found := objects[table]
		if !found || object.objectType != "table" {
			return errors.New("checkpoint schema is missing an expected table")
		}
		if object.sql != definition.sql {
			return fmt.Errorf("checkpoint table %s CREATE SQL does not match the exact versioned constraints", table)
		}
		var schemaName, tableName, objectType string
		var columnCount, withoutRowID, strict int
		if err := checkpoint.database.QueryRowContext(ctx, `SELECT schema,name,type,ncol,wr,strict FROM pragma_table_list WHERE schema='main' AND name=?`, table).
			Scan(&schemaName, &tableName, &objectType, &columnCount, &withoutRowID, &strict); err != nil {
			return err
		}
		if schemaName != "main" || tableName != table || objectType != "table" || columnCount != len(expected) ||
			withoutRowID != definition.withoutRowID || strict != 1 {
			return errors.New("checkpoint table does not match its strict explicit schema")
		}
		columns, err := checkpoint.database.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
		if err != nil {
			return err
		}
		actual := []checkpointTableColumn{}
		for columns.Next() {
			var sequence int
			var name, typeName string
			var notNull, primary int
			var defaultValue any
			if err := columns.Scan(&sequence, &name, &typeName, &notNull, &defaultValue, &primary); err != nil {
				columns.Close()
				return err
			}
			actual = append(actual, checkpointTableColumn{name: name, typeName: strings.ToUpper(typeName), notNull: notNull, primary: primary})
		}
		if err := columns.Close(); err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, expected) {
			return errors.New("checkpoint table schema does not match its explicit version")
		}
		foreignKeys, err := checkpoint.database.QueryContext(ctx, `PRAGMA foreign_key_list("`+table+`")`)
		if err != nil {
			return err
		}
		actualForeignKeys := []checkpointForeignKey{}
		for foreignKeys.Next() {
			var id, sequence int
			var key checkpointForeignKey
			if err := foreignKeys.Scan(&id, &sequence, &key.table, &key.from, &key.to, &key.onUpdate, &key.onDelete, &key.match); err != nil {
				foreignKeys.Close()
				return err
			}
			actualForeignKeys = append(actualForeignKeys, key)
		}
		if err := foreignKeys.Close(); err != nil {
			return err
		}
		sort.Slice(actualForeignKeys, func(left, right int) bool {
			leftKey := actualForeignKeys[left].table + "\x00" + actualForeignKeys[left].from
			rightKey := actualForeignKeys[right].table + "\x00" + actualForeignKeys[right].from
			return leftKey < rightKey
		})
		expectedForeignKeys := append([]checkpointForeignKey{}, checkpointSchemaForeignKeys[table]...)
		sort.Slice(expectedForeignKeys, func(left, right int) bool {
			leftKey := expectedForeignKeys[left].table + "\x00" + expectedForeignKeys[left].from
			rightKey := expectedForeignKeys[right].table + "\x00" + expectedForeignKeys[right].from
			return leftKey < rightKey
		})
		if !reflect.DeepEqual(actualForeignKeys, expectedForeignKeys) {
			return fmt.Errorf("checkpoint table %s foreign-key graph or actions do not match the exact version", table)
		}
	}
	return nil
}

func (checkpoint *preflightCheckpoint) validateCatalogTargets(ctx context.Context) error {
	rows, err := checkpoint.database.QueryContext(ctx, `SELECT music_id,target_kind,target_json,target_sha256 FROM catalog_targets ORDER BY music_id`)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var musicID int
		var kind string
		var body []byte
		var digest string
		if err := rows.Scan(&musicID, &kind, &body, &digest); err != nil {
			rows.Close()
			return err
		}
		binding, found := checkpoint.targets[musicID]
		if !found || binding.Kind != kind || binding.SHA256 != digest || !bytes.Equal(binding.Body, body) {
			rows.Close()
			return errors.New("checkpoint catalog target binding does not match the current catalog")
		}
		var decoded checkpointCatalogRecord
		if err := decodeCanonicalCheckpointJSON(body, &decoded); err != nil || decoded.MusicID != musicID {
			rows.Close()
			return errors.New("checkpoint catalog target JSON is invalid")
		}
		count++
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if count != checkpoint.catalogCount {
		return errors.New("checkpoint catalog target count does not match the current catalog")
	}
	return nil
}

func reportItemsByMusicID(generated report) map[int]struct {
	class string
	item  reportItem
} {
	result := make(map[int]struct {
		class string
		item  reportItem
	}, generated.CatalogCount)
	for _, classified := range []struct {
		class string
		items []reportItem
	}{
		{class: "catalog_review", items: generated.CatalogReview},
		{class: "game_size_evidence", items: generated.GameSizeEvidence},
		{class: "unique_complete", items: generated.UniqueComplete},
		{class: "ambiguous", items: generated.Ambiguous},
		{class: "missing", items: generated.Missing},
		{class: "incomplete", items: generated.Incomplete},
		{class: "error", items: generated.Error},
	} {
		for _, item := range classified.items {
			result[item.MusicID] = struct {
				class string
				item  reportItem
			}{class: classified.class, item: item}
		}
	}
	return result
}

func (checkpoint *preflightCheckpoint) validateResultEvidence(ctx context.Context, generated report) error {
	items := reportItemsByMusicID(generated)
	rows, err := checkpoint.database.QueryContext(ctx, `SELECT r.music_id,r.class,r.evidence_item_count,r.evidence_raw_bytes,
		re.evidence_id,e.evidence_id,e.evidence_json,e.evidence_sha256,e.raw_byte_count
		FROM results r LEFT JOIN result_evidence re ON re.music_id=r.music_id
		LEFT JOIN evidence e ON e.evidence_id=re.evidence_id
		ORDER BY r.music_id,re.evidence_id`)
	if err != nil {
		return err
	}
	type linked struct {
		class          string
		declaredItems  int64
		declaredRaw    int64
		evidence       []lyricssource.IndexEvidence
		linkedRawBytes int64
		linkedIDs      map[string]struct{}
	}
	byMusicID := make(map[int]*linked, len(items))
	for rows.Next() {
		var musicID int
		var declaredItems, declaredRaw int64
		var class string
		var linkKey, evidenceKey, evidenceSHA sql.NullString
		var evidenceBody []byte
		var rawBytes sql.NullInt64
		if err := rows.Scan(&musicID, &class, &declaredItems, &declaredRaw, &linkKey, &evidenceKey,
			&evidenceBody, &evidenceSHA, &rawBytes); err != nil {
			rows.Close()
			return err
		}
		entry := byMusicID[musicID]
		if entry == nil {
			entry = &linked{
				class: class, declaredItems: declaredItems, declaredRaw: declaredRaw,
				linkedIDs: make(map[string]struct{}),
			}
			byMusicID[musicID] = entry
		} else if entry.class != class || entry.declaredItems != declaredItems || entry.declaredRaw != declaredRaw {
			rows.Close()
			return errors.New("checkpoint result evidence declarations are not stable across their links")
		}
		if !linkKey.Valid {
			if evidenceKey.Valid || evidenceBody != nil || evidenceSHA.Valid || rawBytes.Valid {
				rows.Close()
				return errors.New("checkpoint result has a partial exact-evidence link")
			}
			continue
		}
		if !evidenceKey.Valid || linkKey.String != evidenceKey.String || evidenceBody == nil || !evidenceSHA.Valid || !rawBytes.Valid {
			rows.Close()
			return errors.New("checkpoint result_evidence key does not resolve exactly to its evidence row")
		}
		if _, duplicate := entry.linkedIDs[linkKey.String]; duplicate {
			rows.Close()
			return errors.New("checkpoint result contains a duplicate exact-evidence link")
		}
		evidence, err := decodeStoredCheckpointEvidence(evidenceKey.String, evidenceBody, evidenceSHA.String, rawBytes.Int64)
		if err != nil {
			rows.Close()
			return err
		}
		entry.linkedIDs[linkKey.String] = struct{}{}
		entry.evidence = append(entry.evidence, evidence)
		entry.linkedRawBytes += int64(len(evidence.Raw))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(byMusicID) != len(items) {
		return errors.New("checkpoint result_evidence graph does not cover every result exactly")
	}
	for musicID, classified := range items {
		entry := byMusicID[musicID]
		if entry == nil || entry.class != classified.class || entry.declaredItems != int64(len(entry.evidence)) ||
			entry.declaredRaw != entry.linkedRawBytes {
			return errors.New("checkpoint result evidence statistics do not match their exact links")
		}
		binding := checkpoint.targets[musicID]
		canonicalEvidence, err := validateCheckpointResult(binding, classified.class, classified.item, entry.evidence)
		if err != nil {
			return fmt.Errorf("validate checkpoint result and exact evidence: %w", err)
		}
		if len(canonicalEvidence) != len(entry.linkedIDs) {
			return errors.New("checkpoint result exact-evidence links do not match the canonical result references")
		}
		for _, evidence := range canonicalEvidence {
			if _, found := entry.linkedIDs[evidence.EvidenceID]; !found {
				return errors.New("checkpoint result exact-evidence link key does not match its canonical result reference")
			}
		}
	}
	var orphanEvidence int
	if err := checkpoint.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM evidence e WHERE NOT EXISTS (
		SELECT 1 FROM result_evidence re WHERE re.evidence_id=e.evidence_id)`).Scan(&orphanEvidence); err != nil {
		return err
	}
	if orphanEvidence != 0 {
		return errors.New("checkpoint contains orphan exact evidence")
	}
	return nil
}
