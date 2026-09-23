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
	"io"

	"reflect"
	"sort"
	"strings"

	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsstaging"
)

func validateCheckpointSharedCapacityContract() error {
	if lyricssource.MaxIndexEvidenceRawBytes != 2<<20 ||
		lyricsstaging.PrivateEvidenceReceiptSchemaVersion != 1 ||
		lyricsstaging.MaxPrivateEvidenceReceiptItems != 65536 ||
		lyricsstaging.MaxPrivateEvidenceReceiptRawBytes != 32<<20 ||
		lyricsstaging.MaxPrivateEvidenceReceiptBytes != 64<<20 ||
		maxCheckpointEvidenceJSONBytes != int64(lyricsstaging.MaxPrivateEvidenceReceiptBytes) ||
		maxCheckpointBytes != 128<<20 {
		return errors.New("checkpoint evidence or database capacity contract changed without a schema version update")
	}
	return nil
}

func (checkpoint *preflightCheckpoint) initialize(ctx context.Context, generatedAt string) error {
	if checkpoint == nil || checkpoint.database == nil {
		return errors.New("checkpoint database is required")
	}
	parsed, err := time.Parse(time.RFC3339Nano, generatedAt)
	if err != nil || parsed.UTC().Format(time.RFC3339Nano) != generatedAt || !strings.HasSuffix(generatedAt, "Z") {
		return errors.New("checkpoint generated timestamp is invalid")
	}
	for _, statement := range []string{
		fmt.Sprintf(`PRAGMA page_size=%d`, checkpointPageSize),
		`PRAGMA auto_vacuum=NONE`,
		fmt.Sprintf(`PRAGMA application_id=%d`, checkpointApplicationID),
		fmt.Sprintf(`PRAGMA user_version=%d`, checkpointSchemaVersion),
	} {
		if _, err := checkpoint.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize checkpoint SQLite envelope: %w", err)
		}
	}
	if err := validateCheckpointSharedCapacityContract(); err != nil {
		return err
	}
	tx, err := checkpoint.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin checkpoint schema transaction: %w", err)
	}
	defer tx.Rollback()
	for _, definition := range checkpointSchemaDefinitions {
		if _, err := tx.ExecContext(ctx, definition.sql); err != nil {
			return fmt.Errorf("create checkpoint schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_metadata (
		singleton, checkpoint_schema_version, report_schema_version, catalog_schema_version,
		catalog_count, catalog_fingerprint, generated_at, execution_options_json, execution_options_sha256
	) VALUES (1,?,?,?,?,?,?,?,?)`, checkpointSchemaVersion, reportSchemaVersion, catalogSchemaVersion,
		checkpoint.catalogCount, checkpoint.catalogFingerprint, generatedAt, string(checkpoint.executionBody), checkpoint.executionSHA256); err != nil {
		return fmt.Errorf("insert checkpoint metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_counters (
		singleton,catalog_review,game_size_evidence,unique_complete,ambiguous,missing,incomplete,error,
		completed,result_json_bytes,evidence_items,evidence_raw_bytes,evidence_json_bytes,evidence_receipt_bytes
	) VALUES (1,0,0,0,0,0,0,0,0,0,0,0,0,0)`); err != nil {
		return fmt.Errorf("insert checkpoint counters: %w", err)
	}
	musicIDs := make([]int, 0, len(checkpoint.targets))
	for musicID := range checkpoint.targets {
		musicIDs = append(musicIDs, musicID)
	}
	sort.Ints(musicIDs)
	for _, musicID := range musicIDs {
		binding := checkpoint.targets[musicID]
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_targets(music_id,target_kind,target_json,target_sha256) VALUES (?,?,?,?)`,
			musicID, binding.Kind, binding.Body, binding.SHA256); err != nil {
			return fmt.Errorf("insert checkpoint catalog target: %w", err)
		}
	}
	if checkpointBeforeInitializationCommitHook != nil {
		checkpointBeforeInitializationCommitHook(checkpoint.path, checkpoint.operationalPath)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint schema: %w", err)
	}
	if err := checkpoint.syncDurableState("initialization"); err != nil {
		return err
	}
	checkpoint.generatedAt = generatedAt
	return checkpoint.verifyFile("after initialization")
}

func canonicalCheckpointReportItem(item reportItem) reportItem {
	item.AssociationMusicIDs = append([]int(nil), item.AssociationMusicIDs...)
	item.Candidates = append([]candidateSummary(nil), item.Candidates...)
	item.FixedArtifactCandidates = append([]candidateSummary(nil), item.FixedArtifactCandidates...)
	sort.Ints(item.AssociationMusicIDs)
	sort.Slice(item.Candidates, func(left, right int) bool {
		return candidateSummaryLess(item.Candidates[left], item.Candidates[right])
	})
	sort.Slice(item.FixedArtifactCandidates, func(left, right int) bool {
		return candidateSummaryLess(item.FixedArtifactCandidates[left], item.FixedArtifactCandidates[right])
	})
	return item
}

func checkpointItemIdentities(item reportItem) []lyricsstaging.CandidateIdentity {
	if len(item.FixedArtifactCandidates) > 0 {
		identities := make([]lyricsstaging.CandidateIdentity, len(item.FixedArtifactCandidates))
		for index, candidate := range item.FixedArtifactCandidates {
			identities[index] = stagingCandidate(candidate)
		}
		return identities
	}
	result := make([]lyricsstaging.CandidateIdentity, 0, 1+len(item.Candidates))
	if item.Candidate != nil {
		result = append(result, stagingCandidate(*item.Candidate))
	}
	for _, candidate := range item.Candidates {
		result = append(result, stagingCandidate(candidate))
	}
	return result
}

func validateCheckpointResult(binding checkpointTargetBinding, class string, item reportItem,
	evidence []lyricssource.IndexEvidence,
) ([]lyricssource.IndexEvidence, error) {
	if _, supported := checkpointResultClasses[class]; !supported {
		return nil, fmt.Errorf("unsupported checkpoint result class %q", class)
	}
	item = canonicalCheckpointReportItem(item)
	if err := validateResumeReportItem(class, item); err != nil {
		return nil, fmt.Errorf("invalid checkpoint result: %w", err)
	}
	if item.MusicID != binding.Target.MusicID || item.JapaneseTitle != binding.CatalogItem.JapaneseTitle ||
		item.CatalogFingerprint != binding.CatalogItem.CatalogFingerprint ||
		!resumeItemMatchesTarget(class, item, binding.Target) {
		return nil, errors.New("checkpoint result does not match its catalog target")
	}
	switch binding.Kind {
	case checkpointTargetCatalogReview:
		if class != "catalog_review" {
			return nil, errors.New("checkpoint catalog-review target has a provider result")
		}
	case checkpointTargetGameSizeEvidence:
		if class != "game_size_evidence" {
			return nil, errors.New("checkpoint game-size target has a provider result")
		}
	case checkpointTargetProviderWork:
		if class == "catalog_review" || class == "game_size_evidence" {
			return nil, errors.New("checkpoint provider target has a catalog-only result")
		}
	default:
		return nil, errors.New("checkpoint target kind is invalid")
	}
	identities := checkpointItemIdentities(item)
	if len(identities) == 0 {
		if len(evidence) != 0 {
			return nil, errors.New("checkpoint result without candidates has exact evidence")
		}
		return []lyricssource.IndexEvidence{}, nil
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(evidence)
	if err != nil {
		return nil, err
	}
	if err := lyricsstaging.ValidatePrivateEvidenceReceiptForCandidates(receipt, identities); err != nil {
		return nil, err
	}
	return receipt.IndexEvidence, nil
}

func checkpointEvidenceReceiptItemBytes(body []byte) (int64, error) {
	var indented bytes.Buffer
	if err := json.Indent(&indented, body, "    ", "  "); err != nil {
		return 0, fmt.Errorf("indent canonical checkpoint evidence: %w", err)
	}
	// Each receipt array element begins with a newline and four spaces. json.Indent
	// uses that same four-space prefix on continuation lines, so this is byte-for-
	// byte the shared receipt encoder's pretty element representation.
	return int64(1 + 4 + indented.Len()), nil
}

func checkpointEvidenceReceiptBytesAfterInsert(currentItems, currentBytes int64, additions []int64) (int64, error) {
	if currentItems < 0 || currentBytes < 0 || currentItems == 0 && currentBytes != 0 || currentItems > 0 && currentBytes == 0 {
		return 0, errors.New("checkpoint encoded-evidence counters are inconsistent")
	}
	if len(additions) == 0 {
		return currentBytes, nil
	}
	total := currentBytes
	if currentItems == 0 {
		total = int64(len(checkpointEvidenceReceiptPrefix) + len(checkpointEvidenceReceiptSuffix))
	}
	for index, addition := range additions {
		if addition <= 0 {
			return 0, errors.New("checkpoint encoded-evidence contribution is invalid")
		}
		if currentItems > 0 || index > 0 {
			if total == int64(^uint64(0)>>1) {
				return 0, errors.New("checkpoint encoded-evidence byte count overflowed")
			}
			total++ // comma between receipt array elements
		}
		if addition > int64(^uint64(0)>>1)-total {
			return 0, errors.New("checkpoint encoded-evidence byte count overflowed")
		}
		total += addition
	}
	return total, nil
}

func scanCheckpointCounters(row *sql.Row) (checkpointCounters, error) {
	var counters checkpointCounters
	err := row.Scan(
		&counters.Stats.CatalogReview, &counters.Stats.GameSizeEvidence, &counters.Stats.UniqueComplete,
		&counters.Stats.Ambiguous, &counters.Stats.Missing, &counters.Stats.Incomplete, &counters.Stats.Error,
		&counters.Stats.Completed, &counters.ResultJSONBytes, &counters.Stats.EvidenceItems,
		&counters.Stats.EvidenceRawBytes, &counters.EvidenceJSONBytes, &counters.EvidenceReceiptBytes,
	)
	if err != nil {
		return checkpointCounters{}, err
	}
	return counters, nil
}

func checkpointCountersQuery() string {
	return `SELECT catalog_review,game_size_evidence,unique_complete,ambiguous,missing,incomplete,error,
		completed,result_json_bytes,evidence_items,evidence_raw_bytes,evidence_json_bytes,evidence_receipt_bytes
		FROM checkpoint_counters WHERE singleton=1`
}

func checkpointClassCounterColumn(class string) (string, error) {
	switch class {
	case "catalog_review", "game_size_evidence", "unique_complete", "ambiguous", "missing", "incomplete", "error":
		return class, nil
	default:
		return "", errors.New("checkpoint result class has no transactional counter")
	}
}

func (checkpoint *preflightCheckpoint) storeResult(result classifiedResult) error {
	if checkpoint == nil || checkpoint.database == nil {
		return errors.New("checkpoint database is required")
	}
	binding, found := checkpoint.targets[result.item.MusicID]
	if !found {
		return errors.New("checkpoint result has no catalog target")
	}
	result.item = canonicalCheckpointReportItem(result.item)
	evidence, err := validateCheckpointResult(binding, result.class, result.item, result.evidence)
	if err != nil {
		return err
	}
	resultBody, err := json.Marshal(result.item)
	if err != nil {
		return fmt.Errorf("encode checkpoint result: %w", err)
	}
	resultDigest := sha256.Sum256(resultBody)
	resultSHA := hex.EncodeToString(resultDigest[:])

	ctx, cancel := context.WithTimeout(context.Background(), checkpointValidationTimeout)
	defer cancel()
	tx, err := checkpoint.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin checkpoint result transaction: %w", err)
	}
	defer tx.Rollback()
	var existingResult int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM results WHERE music_id=?`, result.item.MusicID).Scan(&existingResult); err != nil {
		return fmt.Errorf("inspect checkpoint result ID: %w", err)
	}
	if existingResult != 0 {
		return errors.New("checkpoint already contains a result for the submitted music ID")
	}

	counters, err := scanCheckpointCounters(tx.QueryRowContext(ctx, checkpointCountersQuery()))
	if err != nil {
		return fmt.Errorf("read checkpoint transactional counters: %w", err)
	}
	if counters.ResultJSONBytes+int64(len(resultBody)) > maxCheckpointResultJSONBytes {
		return errors.New("checkpoint result JSON exceeds its aggregate bound")
	}

	newEvidenceItems := int64(0)
	newEvidenceBytes := int64(0)
	newRawBytes := int64(0)
	newReceiptContributions := []int64{}
	type encodedEvidence struct {
		item lyricssource.IndexEvidence
		body []byte
		sha  string
	}
	encoded := make([]encodedEvidence, 0, len(evidence))
	for _, item := range evidence {
		if len(item.Raw) > lyricssource.MaxIndexEvidenceRawBytes {
			return errors.New("checkpoint exact evidence exceeds the per-evidence raw-byte bound")
		}
		body, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("encode checkpoint evidence: %w", err)
		}
		digest := sha256.Sum256(body)
		sha := hex.EncodeToString(digest[:])
		var existingKey string
		var existingBody []byte
		var existingSHA string
		var existingRaw int64
		err = tx.QueryRowContext(ctx, `SELECT evidence_id,evidence_json,evidence_sha256,raw_byte_count FROM evidence WHERE evidence_id=?`, item.EvidenceID).
			Scan(&existingKey, &existingBody, &existingSHA, &existingRaw)
		switch {
		case err == nil:
			stored, decodeErr := decodeStoredCheckpointEvidence(existingKey, existingBody, existingSHA, existingRaw)
			if decodeErr != nil {
				return fmt.Errorf("validate existing checkpoint evidence: %w", decodeErr)
			}
			if existingSHA != sha || !bytes.Equal(existingBody, body) || !reflect.DeepEqual(stored, item) {
				return errPreflightEvidenceConflict
			}
		case errors.Is(err, sql.ErrNoRows):
			contribution, contributionErr := checkpointEvidenceReceiptItemBytes(body)
			if contributionErr != nil {
				return contributionErr
			}
			newEvidenceItems++
			newEvidenceBytes += int64(len(body))
			newRawBytes += int64(len(item.Raw))
			newReceiptContributions = append(newReceiptContributions, contribution)
		default:
			return fmt.Errorf("inspect checkpoint evidence ID: %w", err)
		}
		encoded = append(encoded, encodedEvidence{item: item, body: body, sha: sha})
	}
	nextReceiptBytes, err := checkpointEvidenceReceiptBytesAfterInsert(
		int64(counters.Stats.EvidenceItems), counters.EvidenceReceiptBytes, newReceiptContributions,
	)
	if err != nil {
		return err
	}
	if int64(counters.Stats.EvidenceItems)+newEvidenceItems > lyricsstaging.MaxPrivateEvidenceReceiptItems ||
		counters.Stats.EvidenceRawBytes+newRawBytes > lyricsstaging.MaxPrivateEvidenceReceiptRawBytes ||
		counters.EvidenceJSONBytes+newEvidenceBytes > maxCheckpointEvidenceJSONBytes ||
		nextReceiptBytes > lyricsstaging.MaxPrivateEvidenceReceiptBytes {
		return errors.New("checkpoint exact evidence exceeds its aggregate receipt capacity")
	}
	for _, value := range encoded {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO evidence(evidence_id,evidence_json,evidence_sha256,raw_byte_count) VALUES (?,?,?,?)`,
			value.item.EvidenceID, value.body, value.sha, len(value.item.Raw)); err != nil {
			return fmt.Errorf("insert checkpoint exact evidence: %w", err)
		}
	}
	rawBytes := 0
	for _, item := range evidence {
		rawBytes += len(item.Raw)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO results(music_id,class,result_json,result_sha256,evidence_item_count,evidence_raw_bytes) VALUES (?,?,?,?,?,?)`,
		result.item.MusicID, result.class, resultBody, resultSHA, len(evidence), rawBytes); err != nil {
		return fmt.Errorf("insert checkpoint result: %w", err)
	}
	for _, item := range evidence {
		if _, err := tx.ExecContext(ctx, `INSERT INTO result_evidence(music_id,evidence_id) VALUES (?,?)`, result.item.MusicID, item.EvidenceID); err != nil {
			return fmt.Errorf("link checkpoint result evidence: %w", err)
		}
	}
	classCounter, err := checkpointClassCounterColumn(result.class)
	if err != nil {
		return err
	}
	counterSQL := `UPDATE checkpoint_counters SET ` + classCounter + `=` + classCounter + `+1,
		completed=completed+1,result_json_bytes=result_json_bytes+?,evidence_items=evidence_items+?,
		evidence_raw_bytes=evidence_raw_bytes+?,evidence_json_bytes=evidence_json_bytes+?,evidence_receipt_bytes=?
		WHERE singleton=1`
	counterResult, err := tx.ExecContext(ctx, counterSQL, len(resultBody), newEvidenceItems, newRawBytes, newEvidenceBytes, nextReceiptBytes)
	if err != nil {
		return fmt.Errorf("update checkpoint transactional counters: %w", err)
	}
	if changed, err := counterResult.RowsAffected(); err != nil || changed != 1 {
		return errors.New("checkpoint transactional counter singleton is missing")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint result and exact evidence: %w", err)
	}
	if checkpointAfterResultCommitHook != nil {
		checkpointAfterResultCommitHook(checkpoint.path, checkpoint.operationalPath)
	}
	if err := checkpoint.syncDurableState("result transaction"); err != nil {
		return err
	}
	return checkpoint.verifyFile("after result commit")
}

func (checkpoint *preflightCheckpoint) stats(ctx context.Context) (checkpointStats, error) {
	var stats checkpointStats
	if checkpoint == nil || checkpoint.database == nil {
		return stats, errors.New("checkpoint database is required")
	}
	counters, err := scanCheckpointCounters(checkpoint.database.QueryRowContext(ctx, checkpointCountersQuery()))
	if err != nil {
		return stats, fmt.Errorf("read checkpoint progress counters: %w", err)
	}
	stats = counters.Stats
	stats.MissingWork = checkpoint.catalogCount - stats.Completed
	if stats.MissingWork < 0 {
		return stats, errors.New("checkpoint result count exceeds the catalog count")
	}
	return stats, nil
}

func checkpointStatsForReport(generated report) checkpointStats {
	return checkpointStats{
		CatalogReview: len(generated.CatalogReview), GameSizeEvidence: len(generated.GameSizeEvidence),
		UniqueComplete: len(generated.UniqueComplete), Ambiguous: len(generated.Ambiguous),
		Missing: len(generated.Missing), Incomplete: len(generated.Incomplete), Error: len(generated.Error),
		Completed: len(generated.CatalogReview) + len(generated.GameSizeEvidence) + len(generated.UniqueComplete) +
			len(generated.Ambiguous) + len(generated.Missing) + len(generated.Incomplete) + len(generated.Error),
	}
}

func (checkpoint *preflightCheckpoint) validateCounters(ctx context.Context, generated report) error {
	counters, err := scanCheckpointCounters(checkpoint.database.QueryRowContext(ctx, checkpointCountersQuery()))
	if err != nil {
		return fmt.Errorf("read checkpoint validation counters: %w", err)
	}
	expectedStats := checkpointStatsForReport(generated)
	var resultCount int
	var resultJSONBytes int64
	if err := checkpoint.database.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(result_json)),0) FROM results`).
		Scan(&resultCount, &resultJSONBytes); err != nil {
		return fmt.Errorf("validate checkpoint result counters: %w", err)
	}
	if resultCount != expectedStats.Completed || resultJSONBytes != counters.ResultJSONBytes ||
		resultJSONBytes > maxCheckpointResultJSONBytes {
		return errors.New("checkpoint result counters do not bind the exact result rows")
	}

	var evidenceItems int64
	var evidenceRawBytes, evidenceJSONBytes, evidenceReceiptBytes int64
	rows, err := checkpoint.database.QueryContext(ctx, `SELECT evidence_json,raw_byte_count FROM evidence ORDER BY evidence_id`)
	if err != nil {
		return fmt.Errorf("validate checkpoint evidence counters: %w", err)
	}
	for rows.Next() {
		var body []byte
		var rawBytes int64
		if err := rows.Scan(&body, &rawBytes); err != nil {
			rows.Close()
			return err
		}
		contribution, err := checkpointEvidenceReceiptItemBytes(body)
		if err != nil {
			rows.Close()
			return err
		}
		evidenceReceiptBytes, err = checkpointEvidenceReceiptBytesAfterInsert(
			evidenceItems, evidenceReceiptBytes, []int64{contribution},
		)
		if err != nil {
			rows.Close()
			return err
		}
		evidenceItems++
		evidenceRawBytes += rawBytes
		evidenceJSONBytes += int64(len(body))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	expectedStats.EvidenceItems = int(evidenceItems)
	expectedStats.EvidenceRawBytes = evidenceRawBytes
	if !reflect.DeepEqual(counters.Stats, expectedStats) || counters.EvidenceJSONBytes != evidenceJSONBytes ||
		counters.EvidenceReceiptBytes != evidenceReceiptBytes {
		return errors.New("checkpoint transactional counters do not bind the exact result and evidence rows")
	}
	if evidenceItems > lyricsstaging.MaxPrivateEvidenceReceiptItems ||
		evidenceRawBytes > lyricsstaging.MaxPrivateEvidenceReceiptRawBytes ||
		evidenceJSONBytes > maxCheckpointEvidenceJSONBytes ||
		evidenceReceiptBytes > lyricsstaging.MaxPrivateEvidenceReceiptBytes {
		return errors.New("checkpoint encoded evidence exceeds the shared private receipt capacity")
	}
	return nil
}

func emitCheckpointProgress(writer io.Writer, phase string, stats checkpointStats) error {
	if writer == nil {
		return nil
	}
	_, err := fmt.Fprintf(writer,
		"checkpoint phase=%s completed=%d missing=%d catalog_review=%d game_size_evidence=%d unique_complete=%d ambiguous=%d missing_class=%d incomplete=%d error=%d evidence_items=%d evidence_raw_bytes=%d\n",
		phase, stats.Completed, stats.MissingWork, stats.CatalogReview, stats.GameSizeEvidence,
		stats.UniqueComplete, stats.Ambiguous, stats.Missing, stats.Incomplete, stats.Error,
		stats.EvidenceItems, stats.EvidenceRawBytes)
	if err != nil {
		return fmt.Errorf("write checkpoint progress statistics: %w", err)
	}
	return nil
}

func (checkpoint *preflightCheckpoint) progress(writer io.Writer, phase string) error {
	ctx, cancel := context.WithTimeout(context.Background(), checkpointValidationTimeout)
	defer cancel()
	stats, err := checkpoint.stats(ctx)
	if err != nil {
		return err
	}
	return emitCheckpointProgress(writer, phase, stats)
}
