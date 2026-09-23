package lyricsacquisition

import (
	"context"

	"database/sql"
	"database/sql/driver"

	"errors"
	"fmt"

	"reflect"

	"sort"

	"strings"
)

func (s *spool) configureMetadataDatabase(ctx context.Context, _ bool) error {
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
		if _, err := s.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure private in-memory acquisition metadata runtime: %w", err)
		}
	}
	var journalMode string
	if err := s.database.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("read acquisition metadata runtime journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "memory") {
		return errors.New("acquisition metadata runtime journal mode must be MEMORY")
	}
	return nil
}

func (s *spool) initializeMetadataSchema(ctx context.Context) error {
	for _, statement := range []string{
		`PRAGMA auto_vacuum=NONE`,
		fmt.Sprintf(`PRAGMA application_id=%d`, spoolApplicationID),
		fmt.Sprintf(`PRAGMA user_version=%d`, spoolSchemaVersion),
	} {
		if _, err := s.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize acquisition metadata SQLite envelope: %w", err)
		}
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin acquisition metadata schema transaction: %w", err)
	}
	defer tx.Rollback()
	for _, definition := range spoolSchemaDefinitions {
		if _, err := tx.ExecContext(ctx, definition.sql); err != nil {
			return fmt.Errorf("create acquisition metadata schema object %s: %w", definition.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO spool_metadata(singleton,schema_version) VALUES (1,2)`); err != nil {
		return fmt.Errorf("initialize acquisition metadata version row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO spool_counters(singleton,request_count,acquisition_count,raw_bytes,evidence_bytes,envelope_bytes,manifest_bytes) VALUES (1,0,0,0,0,0,0)`); err != nil {
		return fmt.Errorf("initialize acquisition metadata counters: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit acquisition metadata schema: %w", err)
	}
	return s.syncMetadata("schema initialization")
}

func (s *spool) syncMetadata(stage string) error {
	if s.metadataFile == nil || s.root == nil {
		return errors.New("private acquisition metadata file and root are required")
	}
	if err := s.verifyMetadataFile("before " + stage + " sync"); err != nil {
		return err
	}
	if err := s.metadataFile.Sync(); err != nil {
		return fmt.Errorf("sync acquisition metadata %s: %w", stage, err)
	}
	if err := s.root.file.Sync(); err != nil {
		return fmt.Errorf("sync acquisition spool root after %s: %w", stage, err)
	}
	return s.verifyMetadataFile("after " + stage + " sync")
}

func (s *spool) verifyMetadataFile(stage string) error {
	if err := verifyPinnedMetadataDescriptor(s.root, s.metadataFile, s.metadataBinding); err != nil {
		return fmt.Errorf("acquisition metadata path, inode, or bytes changed %s: %w", stage, err)
	}
	return validateMetadataSidecars(s.root, false)
}

func (s *spool) verifyMetadataConnection(ctx context.Context) error {
	connection, err := s.database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("obtain private acquisition metadata connection: %w", err)
	}
	defer connection.Close()
	return connection.Raw(func(raw any) error {
		driverConnection, ok := raw.(driver.Conn)
		if !ok {
			return fmt.Errorf("SQLite raw connection type %T does not implement driver.Conn", raw)
		}
		stat, _, _ := s.metadataBinding.snapshot()
		return verifyModerncSQLiteMainFile(driverConnection, stat)
	})
}

func (s *spool) ensureOperationalIdentity(ctx context.Context) error {
	if err := s.verifyMetadataFile("before ledger operation"); err != nil {
		return err
	}
	if err := s.validateReviewedHardlinkGraph(); err != nil {
		return fmt.Errorf("validate reviewed acquisition hard-link graph before ledger operation: %w", err)
	}
	return s.verifyMetadataConnection(ctx)
}

func (s *spool) validateMetadataState(ctx context.Context) error {
	if s.database == nil {
		return errors.New("private acquisition metadata SQLite index is required")
	}
	validationCtx, cancel := context.WithTimeout(ctx, metadataValidationTimeout)
	defer cancel()
	if err := s.verifyMetadataFile("during validation"); err != nil {
		return err
	}
	if err := s.verifyMetadataConnection(validationCtx); err != nil {
		return err
	}
	var applicationID, userVersion, pageSize, maxPages int64
	for query, destination := range map[string]*int64{
		`PRAGMA application_id`: &applicationID,
		`PRAGMA user_version`:   &userVersion,
		`PRAGMA page_size`:      &pageSize,
		`PRAGMA max_page_count`: &maxPages,
	} {
		if err := s.database.QueryRowContext(validationCtx, query).Scan(destination); err != nil {
			return err
		}
	}
	if applicationID != spoolApplicationID || userVersion != spoolSchemaVersion || pageSize != spoolPageSize || maxPages != maxMetadataPages {
		return errors.New("acquisition metadata SQLite envelope or capacity is invalid")
	}
	var journalMode string
	var busyTimeout, synchronous, foreignKeys, trustedSchema, tempStore int
	if err := s.database.QueryRowContext(validationCtx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return err
	}
	for query, destination := range map[string]*int{
		`PRAGMA busy_timeout`:   &busyTimeout,
		`PRAGMA synchronous`:    &synchronous,
		`PRAGMA foreign_keys`:   &foreignKeys,
		`PRAGMA trusted_schema`: &trustedSchema,
		`PRAGMA temp_store`:     &tempStore,
	} {
		if err := s.database.QueryRowContext(validationCtx, query).Scan(destination); err != nil {
			return err
		}
	}
	if !strings.EqualFold(journalMode, "memory") || busyTimeout != 5000 || synchronous != 2 || foreignKeys != 1 || trustedSchema != 0 || tempStore != 2 {
		return errors.New("acquisition metadata SQLite runtime safety pragmas are invalid")
	}
	rows, err := s.database.QueryContext(validationCtx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("check acquisition metadata integrity: %w", err)
	}
	integrityRows := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			rows.Close()
			return err
		}
		integrityRows++
		if result != "ok" {
			rows.Close()
			return errors.New("acquisition metadata integrity check failed")
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if integrityRows != 1 {
		return errors.New("acquisition metadata integrity check returned an invalid row count")
	}
	foreignRows, err := s.database.QueryContext(validationCtx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check acquisition metadata foreign keys: %w", err)
	}
	if foreignRows.Next() {
		foreignRows.Close()
		return errors.New("acquisition metadata foreign-key check failed")
	}
	if err := foreignRows.Close(); err != nil {
		return err
	}
	var mainCount, unexpectedAttachments int
	var mainFile string
	if err := s.database.QueryRowContext(validationCtx, `SELECT COUNT(*),MAX(file) FROM pragma_database_list WHERE name='main'`).Scan(&mainCount, &mainFile); err != nil {
		return err
	}
	if err := s.database.QueryRowContext(validationCtx, `SELECT COUNT(*) FROM pragma_database_list WHERE name NOT IN ('main','temp')`).Scan(&unexpectedAttachments); err != nil {
		return err
	}
	if mainCount != 1 || unexpectedAttachments != 0 || mainFile != "" {
		return errors.New("acquisition metadata runtime is not an isolated in-memory main database")
	}
	if err := s.validateMetadataSchema(validationCtx); err != nil {
		return err
	}
	var metadataRows, schemaVersion int
	if err := s.database.QueryRowContext(validationCtx, `SELECT COUNT(*),COALESCE(MAX(schema_version),0) FROM spool_metadata`).Scan(&metadataRows, &schemaVersion); err != nil {
		return err
	}
	if metadataRows != 1 || schemaVersion != spoolSchemaVersion {
		return errors.New("acquisition metadata version singleton is invalid")
	}
	return s.validateMetadataCounters(validationCtx)
}

func (s *spool) validateMetadataSchema(ctx context.Context) error {
	rows, err := s.database.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)
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
		if _, duplicate := objects[name]; duplicate {
			rows.Close()
			return errors.New("acquisition metadata schema contains a duplicate object name")
		}
		objects[name] = schemaObject{objectType: objectType, sql: createSQL}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(objects) != len(spoolSchemaDefinitions) {
		return errors.New("acquisition metadata schema contains unexpected objects")
	}
	for _, definition := range spoolSchemaDefinitions {
		object, found := objects[definition.name]
		if !found || object.objectType != definition.objectType || object.sql != definition.sql {
			return fmt.Errorf("acquisition metadata schema object %s does not match its exact v2 definition", definition.name)
		}
		if definition.objectType != "table" {
			continue
		}
		var schemaName, tableName, objectType string
		var columnCount, withoutRowID, strict int
		if err := s.database.QueryRowContext(ctx, `SELECT schema,name,type,ncol,wr,strict FROM pragma_table_list WHERE schema='main' AND name=?`, definition.name).
			Scan(&schemaName, &tableName, &objectType, &columnCount, &withoutRowID, &strict); err != nil {
			return err
		}
		if schemaName != "main" || tableName != definition.name || objectType != "table" ||
			columnCount != len(spoolSchemaColumns[definition.name]) || withoutRowID != boolInt(definition.withoutRowID) || strict != boolInt(definition.strict) {
			return errors.New("acquisition metadata table does not match its strict explicit schema")
		}
		columns, err := s.database.QueryContext(ctx, `PRAGMA table_info("`+definition.name+`")`)
		if err != nil {
			return err
		}
		actualColumns := []tableColumn{}
		for columns.Next() {
			var sequence int
			var name, typeName string
			var notNull, primary int
			var defaultValue any
			if err := columns.Scan(&sequence, &name, &typeName, &notNull, &defaultValue, &primary); err != nil {
				columns.Close()
				return err
			}
			actualColumns = append(actualColumns, tableColumn{name: name, typeName: strings.ToUpper(typeName), notNull: notNull, primary: primary})
		}
		if err := columns.Close(); err != nil {
			return err
		}
		if !reflect.DeepEqual(actualColumns, spoolSchemaColumns[definition.name]) {
			return fmt.Errorf("acquisition metadata table %s columns changed", definition.name)
		}
		foreignRows, err := s.database.QueryContext(ctx, `PRAGMA foreign_key_list("`+definition.name+`")`)
		if err != nil {
			return err
		}
		actualForeignKeys := []foreignKey{}
		for foreignRows.Next() {
			var id, sequence int
			var key foreignKey
			if err := foreignRows.Scan(&id, &sequence, &key.table, &key.from, &key.to, &key.onUpdate, &key.onDelete, &key.match); err != nil {
				foreignRows.Close()
				return err
			}
			actualForeignKeys = append(actualForeignKeys, key)
		}
		if err := foreignRows.Close(); err != nil {
			return err
		}
		sort.Slice(actualForeignKeys, func(left, right int) bool {
			return actualForeignKeys[left].table+"\x00"+actualForeignKeys[left].from < actualForeignKeys[right].table+"\x00"+actualForeignKeys[right].from
		})
		expectedForeignKeys := append([]foreignKey{}, spoolSchemaForeignKeys[definition.name]...)
		sort.Slice(expectedForeignKeys, func(left, right int) bool {
			return expectedForeignKeys[left].table+"\x00"+expectedForeignKeys[left].from < expectedForeignKeys[right].table+"\x00"+expectedForeignKeys[right].from
		})
		if !reflect.DeepEqual(actualForeignKeys, expectedForeignKeys) {
			return fmt.Errorf("acquisition metadata table %s foreign-key graph changed", definition.name)
		}
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *spool) readCounters(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (metadataCounters, error) {
	var counters metadataCounters
	err := queryer.QueryRowContext(ctx, `SELECT request_count,acquisition_count,raw_bytes,evidence_bytes,envelope_bytes,manifest_bytes FROM spool_counters WHERE singleton=1`).
		Scan(&counters.requestCount, &counters.acquisitionCount, &counters.rawBytes, &counters.evidenceBytes, &counters.envelopeBytes, &counters.manifestBytes)
	return counters, err
}

func (s *spool) validateMetadataCounters(ctx context.Context) error {
	counters, err := s.readCounters(ctx, s.database)
	if err != nil {
		return fmt.Errorf("read acquisition metadata counters: %w", err)
	}
	var requestCount, referencedRequestCount, acquisitionCount int64
	var rawBytes, evidenceBytes, envelopeBytes, manifestBytes int64
	if err := s.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&requestCount); err != nil {
		return err
	}
	if err := s.database.QueryRowContext(ctx, `SELECT COUNT(DISTINCT request_key) FROM acquisitions`).Scan(&referencedRequestCount); err != nil {
		return err
	}
	if err := s.database.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(raw_byte_count),0),COALESCE(SUM(evidence_byte_count),0),COALESCE(SUM(envelope_byte_count),0),COALESCE(SUM(manifest_byte_count),0) FROM acquisitions`).
		Scan(&acquisitionCount, &rawBytes, &evidenceBytes, &envelopeBytes, &manifestBytes); err != nil {
		return err
	}
	var mismatchedRequestBindings int64
	if err := s.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM acquisitions a JOIN requests r ON r.request_key=a.request_key
		WHERE a.provider!=r.provider OR a.canonical_request_identity!=r.canonical_request_identity OR
			a.request_kind!=r.request_kind OR a.revision_selector!=r.revision_selector`).Scan(&mismatchedRequestBindings); err != nil {
		return err
	}
	if counters.requestCount != requestCount || requestCount != referencedRequestCount || counters.acquisitionCount != acquisitionCount ||
		counters.rawBytes != rawBytes || counters.evidenceBytes != evidenceBytes || counters.envelopeBytes != envelopeBytes ||
		counters.manifestBytes != manifestBytes || mismatchedRequestBindings != 0 || requestCount > maxAcquisitions ||
		acquisitionCount > maxAcquisitions || rawBytes > maxAggregateRawBytes || evidenceBytes > maxAggregateEvidence ||
		envelopeBytes > maxAggregateEnvelope || manifestBytes > maxAggregateManifest {
		return errors.New("acquisition metadata counters and request graph do not bind the exact bounded rows")
	}
	return nil
}
