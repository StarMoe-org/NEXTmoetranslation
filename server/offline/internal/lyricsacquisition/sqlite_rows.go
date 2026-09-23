package lyricsacquisition

import (
	"context"

	"database/sql"

	"errors"
	"fmt"

	"reflect"
)

func metadataFromManifest(manifest acquisitionManifest, acquisitionID, key string, manifestBytes int) acquisitionMetadata {
	return acquisitionMetadata{
		acquisitionID:            acquisitionID,
		requestKey:               key,
		provider:                 manifest.Request.Provider,
		canonicalRequestIdentity: manifest.Request.CanonicalRequestIdentity,
		requestKind:              manifest.Request.Kind,
		revisionSelector:         manifest.Request.RevisionSelector,
		fetchedAt:                manifest.FetchedAt,
		rawSHA256:                manifest.RawResponse.SHA256,
		rawByteCount:             manifest.RawResponse.ByteCount,
		evidenceSHA256:           manifest.EvidenceProjection.SHA256,
		evidenceByteCount:        manifest.EvidenceProjection.ByteCount,
		evidenceID:               manifest.EvidenceProjection.EvidenceID,
		envelopeSHA256:           manifest.EvidenceEnvelope.SHA256,
		envelopeByteCount:        manifest.EvidenceEnvelope.ByteCount,
		manifestSHA256:           acquisitionID,
		manifestByteCount:        manifestBytes,
		observedRevisionCount:    len(manifest.ObservedRevisions),
	}
}

func scanAcquisitionMetadata(row interface{ Scan(...any) error }) (acquisitionMetadata, error) {
	var metadata acquisitionMetadata
	err := row.Scan(
		&metadata.acquisitionID, &metadata.requestKey, &metadata.provider, &metadata.canonicalRequestIdentity,
		&metadata.requestKind, &metadata.revisionSelector, &metadata.fetchedAt, &metadata.rawSHA256,
		&metadata.rawByteCount, &metadata.evidenceSHA256, &metadata.evidenceByteCount, &metadata.evidenceID,
		&metadata.envelopeSHA256, &metadata.envelopeByteCount,
		&metadata.manifestSHA256, &metadata.manifestByteCount, &metadata.observedRevisionCount,
	)
	return metadata, err
}

const acquisitionMetadataColumns = `acquisition_id,request_key,provider,canonical_request_identity,request_kind,revision_selector,
	fetched_at,raw_sha256,raw_byte_count,evidence_sha256,evidence_byte_count,evidence_id,
	envelope_sha256,envelope_byte_count,manifest_sha256,manifest_byte_count,observed_revision_count`

func (s *spool) ensureMetadataCapacity(ctx context.Context, manifest acquisitionManifest, acquisitionID, key string, manifestBytes int) error {
	metadata := metadataFromManifest(manifest, acquisitionID, key, manifestBytes)
	existing, err := s.metadataByID(ctx, acquisitionID)
	if err == nil {
		if !reflect.DeepEqual(existing, metadata) {
			return errors.New("existing acquisition metadata conflicts with its content-addressed manifest")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect existing acquisition metadata capacity: %w", err)
	}
	requestExists := false
	var provider, canonicalIdentity, requestKind, revisionSelector string
	err = s.database.QueryRowContext(ctx, `SELECT provider,canonical_request_identity,request_kind,revision_selector FROM requests WHERE request_key=? LIMIT 2`, key).
		Scan(&provider, &canonicalIdentity, &requestKind, &revisionSelector)
	switch {
	case err == nil:
		requestExists = true
		if provider != metadata.provider || canonicalIdentity != metadata.canonicalRequestIdentity || requestKind != metadata.requestKind || revisionSelector != metadata.revisionSelector {
			return errors.New("acquisition request key conflicts with an existing exact request")
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return fmt.Errorf("inspect exact acquisition request metadata capacity: %w", err)
	}
	counters, err := s.readCounters(ctx, s.database)
	if err != nil {
		return fmt.Errorf("read acquisition metadata counters for capacity: %w", err)
	}
	nextRequestCount := counters.requestCount
	if !requestExists {
		nextRequestCount++
	}
	if nextRequestCount > maxAcquisitions || counters.acquisitionCount+1 > maxAcquisitions ||
		counters.rawBytes+int64(metadata.rawByteCount) > maxAggregateRawBytes ||
		counters.evidenceBytes+int64(metadata.evidenceByteCount) > maxAggregateEvidence ||
		counters.envelopeBytes+int64(metadata.envelopeByteCount) > maxAggregateEnvelope ||
		counters.manifestBytes+int64(metadata.manifestByteCount) > maxAggregateManifest {
		return errors.New("lyrics acquisition spool exceeds its bounded v2 capacity before publication")
	}
	return nil
}

func insertMetadataRow(ctx context.Context, tx *sql.Tx, metadata acquisitionMetadata, counters *metadataCounters) (bool, error) {
	existing, err := scanAcquisitionMetadata(tx.QueryRowContext(ctx, `SELECT `+acquisitionMetadataColumns+` FROM acquisitions WHERE acquisition_id=? LIMIT 2`, metadata.acquisitionID))
	if err == nil {
		if !reflect.DeepEqual(existing, metadata) {
			return false, errors.New("existing acquisition metadata conflicts with its content-addressed manifest")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("inspect existing acquisition metadata: %w", err)
	}
	requestExists := false
	var provider, canonicalIdentity, requestKind, revisionSelector string
	err = tx.QueryRowContext(ctx, `SELECT provider,canonical_request_identity,request_kind,revision_selector FROM requests WHERE request_key=? LIMIT 2`, metadata.requestKey).
		Scan(&provider, &canonicalIdentity, &requestKind, &revisionSelector)
	switch {
	case err == nil:
		requestExists = true
		if provider != metadata.provider || canonicalIdentity != metadata.canonicalRequestIdentity || requestKind != metadata.requestKind || revisionSelector != metadata.revisionSelector {
			return false, errors.New("acquisition request key conflicts with an existing exact request")
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, fmt.Errorf("inspect exact acquisition request metadata: %w", err)
	}
	next := *counters
	if !requestExists {
		next.requestCount++
	}
	next.acquisitionCount++
	next.rawBytes += int64(metadata.rawByteCount)
	next.evidenceBytes += int64(metadata.evidenceByteCount)
	next.envelopeBytes += int64(metadata.envelopeByteCount)
	next.manifestBytes += int64(metadata.manifestByteCount)
	if next.requestCount > maxAcquisitions || next.acquisitionCount > maxAcquisitions || next.rawBytes > maxAggregateRawBytes ||
		next.evidenceBytes > maxAggregateEvidence || next.envelopeBytes > maxAggregateEnvelope || next.manifestBytes > maxAggregateManifest {
		return false, errors.New("lyrics acquisition spool exceeds its bounded v2 capacity")
	}
	if !requestExists {
		if _, err := tx.ExecContext(ctx, `INSERT INTO requests(request_key,provider,canonical_request_identity,request_kind,revision_selector) VALUES (?,?,?,?,?)`,
			metadata.requestKey, metadata.provider, metadata.canonicalRequestIdentity, metadata.requestKind, metadata.revisionSelector); err != nil {
			return false, fmt.Errorf("insert exact acquisition request metadata: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO acquisitions(`+acquisitionMetadataColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		metadata.acquisitionID, metadata.requestKey, metadata.provider, metadata.canonicalRequestIdentity,
		metadata.requestKind, metadata.revisionSelector, metadata.fetchedAt, metadata.rawSHA256,
		metadata.rawByteCount, metadata.evidenceSHA256, metadata.evidenceByteCount, metadata.evidenceID,
		metadata.envelopeSHA256, metadata.envelopeByteCount,
		metadata.manifestSHA256, metadata.manifestByteCount, metadata.observedRevisionCount); err != nil {
		return false, fmt.Errorf("insert exact acquisition metadata: %w", err)
	}
	*counters = next
	return true, nil
}

func updateMetadataCounters(ctx context.Context, tx *sql.Tx, counters metadataCounters) error {
	result, err := tx.ExecContext(ctx, `UPDATE spool_counters SET request_count=?,acquisition_count=?,raw_bytes=?,evidence_bytes=?,envelope_bytes=?,manifest_bytes=? WHERE singleton=1`,
		counters.requestCount, counters.acquisitionCount, counters.rawBytes, counters.evidenceBytes, counters.envelopeBytes, counters.manifestBytes)
	if err != nil {
		return fmt.Errorf("update acquisition metadata counters: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return errors.New("acquisition metadata counter singleton is missing")
	}
	return nil
}

func (s *spool) insertMetadataBatch(ctx context.Context, metadataRows []acquisitionMetadata, stage string) error {
	if len(metadataRows) == 0 {
		return nil
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin acquisition metadata transaction: %w", err)
	}
	defer tx.Rollback()
	counters, err := s.readCounters(ctx, tx)
	if err != nil {
		return fmt.Errorf("read acquisition metadata counters for insert: %w", err)
	}
	changed := false
	for _, metadata := range metadataRows {
		inserted, err := insertMetadataRow(ctx, tx, metadata, &counters)
		if err != nil {
			return err
		}
		changed = changed || inserted
	}
	if !changed {
		return nil
	}
	if err := updateMetadataCounters(ctx, tx, counters); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit exact acquisition metadata: %w", err)
	}
	if err := s.persistMetadataRuntimeSnapshot(ctx, stage); err != nil {
		if s.failure == nil {
			s.failure = fmt.Errorf("persist committed acquisition metadata %s snapshot: %w", stage, err)
		}
		return s.failure
	}
	return nil
}

func (s *spool) insertMetadata(ctx context.Context, manifest acquisitionManifest, acquisitionID, key string, manifestBytes int) error {
	metadata := metadataFromManifest(manifest, acquisitionID, key, manifestBytes)
	return s.insertMetadataBatch(ctx, []acquisitionMetadata{metadata}, "acquisition transaction")
}

func (s *spool) metadataByID(ctx context.Context, acquisitionID string) (acquisitionMetadata, error) {
	return scanAcquisitionMetadata(s.database.QueryRowContext(ctx, `SELECT `+acquisitionMetadataColumns+` FROM acquisitions WHERE acquisition_id=? LIMIT 2`, acquisitionID))
}
