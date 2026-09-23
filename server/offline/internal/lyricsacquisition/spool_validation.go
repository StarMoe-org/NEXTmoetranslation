package lyricsacquisition

import (
	"context"

	"errors"
	"fmt"

	"reflect"

	"strings"
)

func (s *spool) validateState(ctx context.Context) error {
	if err := s.root.verify(); err != nil {
		return err
	}
	if err := s.validateRootEntries(false, false); err != nil {
		return err
	}
	if err := s.validateMetadataState(ctx); err != nil {
		return err
	}
	if err := s.validateRequests(ctx); err != nil {
		return err
	}
	if err := s.validateAcquisitions(ctx); err != nil {
		return err
	}
	if err := s.validateContentAddressedDirectories(); err != nil {
		return err
	}
	return s.root.verify()
}

func (s *spool) validateRootEntries(allowMetadataAbsent, allowJournal bool) error {
	if err := s.root.verify(); err != nil {
		return err
	}
	entries, err := sortedDirectoryEntries(s.root, "", 9+maxMetadataRecoverySlots)
	if err != nil {
		return err
	}
	expected := map[string]bool{
		blobsDirectory: false, manifestsDirectory: false, pendingDirectory: false, quarantineDirectory: true,
		metadataStateDirectory: allowJournal, ledgerLockName: false, metadataFileName: allowMetadataAbsent,
		migrationManifestName: true, metadataFileName + "-journal": true, metadataSnapshotTempName: true,
	}
	for _, entry := range entries {
		optional, found := expected[entry.Name()]
		if !found && !metadataSlotNameAllowed(entry.Name()) {
			return fmt.Errorf("lyrics acquisition spool root contains unexpected entry %q", entry.Name())
		}
		_ = optional
		if found {
			delete(expected, entry.Name())
		}
		stat, err := statAt(s.root.file, entry.Name())
		if err != nil {
			return err
		}
		switch entry.Name() {
		case blobsDirectory, manifestsDirectory, pendingDirectory, quarantineDirectory, metadataStateDirectory:
			if err := validatePrivateDirectoryStat(stat, "lyrics acquisition spool "+entry.Name()+" directory"); err != nil {
				return err
			}
			if err := s.root.verifyDirectory(entry.Name()); err != nil {
				return err
			}
		case ledgerLockName:
			if err := s.root.verifyLock(); err != nil {
				return err
			}
		case metadataFileName:
			if err := validatePrivateRegularStat(stat, "lyrics acquisition metadata file", 1); err != nil {
				return err
			}
			if _, reviewed := s.root.knownLeaves[leafIdentityKey("", metadataFileName)]; reviewed {
				if err := s.root.verifyKnownLeaf("", metadataFileName, stat.identity); err != nil {
					return err
				}
			} else {
				s.root.rememberLeaf("", metadataFileName, stat.identity)
			}
		case metadataFileName + "-journal":
			if !allowJournal {
				return errors.New("lyrics acquisition metadata journal is not allowed in steady state")
			}
			if err := validatePrivateRegularStat(stat, "lyrics acquisition metadata rollback journal", 1); err != nil {
				return err
			}
			if stat.size < 0 || stat.size > maxMetadataBytes+spoolPageSize {
				return errors.New("lyrics acquisition metadata rollback journal exceeds its byte bound")
			}
		case metadataSnapshotTempName:
			if err := validatePrivateRegularStat(stat, "retained prior acquisition metadata snapshot", 1); err != nil {
				return err
			}
			if stat.size < 0 || stat.size > maxMetadataBytes {
				return errors.New("retained prior acquisition metadata snapshot exceeds its byte bound")
			}
			if _, reviewed := s.root.knownLeaves[leafIdentityKey("", metadataSnapshotTempName)]; reviewed {
				if err := s.root.verifyKnownLeaf("", metadataSnapshotTempName, stat.identity); err != nil {
					return err
				}
			} else {
				s.root.rememberLeaf("", metadataSnapshotTempName, stat.identity)
			}
		case migrationManifestName:
			if _, reviewed := s.root.knownLeaves[leafIdentityKey("", migrationManifestName)]; !reviewed {
				if !allowJournal {
					return errors.New("lyrics acquisition migration manifest appeared after initial root validation")
				}
				s.root.rememberLeaf("", migrationManifestName, stat.identity)
			}
			body, info, err := readVerifiedFileAt(s.root, "", migrationManifestName, "lyrics acquisition migration manifest", "", -1, maxMigrationManifestSize, 1)
			if err != nil {
				return err
			}
			identity, ok := fileIdentityFromFileInfo(info)
			if !ok || !sameFileIdentity(stat.identity, identity) {
				return errors.New("lyrics acquisition migration manifest has no stable supported identity")
			}
			if _, err := decodeLedgerMigrationManifest(body); err != nil {
				return err
			}
		default:
			if !metadataSlotNameAllowed(entry.Name()) {
				return fmt.Errorf("lyrics acquisition spool root contains unexpected entry %q", entry.Name())
			}
			if err := validatePrivateRegularStat(stat, "bounded acquisition metadata recovery slot", 1); err != nil {
				return err
			}
			if stat.size < 0 || stat.size > maxMetadataBytes {
				return errors.New("bounded acquisition metadata recovery slot exceeds its byte bound")
			}
			if _, reviewed := s.root.knownLeaves[leafIdentityKey("", entry.Name())]; reviewed {
				if err := s.root.verifyKnownLeaf("", entry.Name(), stat.identity); err != nil {
					return err
				}
			} else {
				s.root.rememberLeaf("", entry.Name(), stat.identity)
			}
		}
	}
	for name, optional := range expected {
		if !optional {
			return fmt.Errorf("lyrics acquisition spool root is missing required entry %q", name)
		}
	}
	return nil
}

func (s *spool) validateRequests(ctx context.Context) error {
	rows, err := s.database.QueryContext(ctx, `SELECT request_key,provider,canonical_request_identity,request_kind,revision_selector FROM requests ORDER BY request_key LIMIT ?`, maxAcquisitions+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var key string
		var request acquisitionRequest
		var kind string
		if err := rows.Scan(&key, &request.provider, &request.canonicalRequestIdentity, &kind, &request.revisionSelector); err != nil {
			return err
		}
		request.kind = acquisitionRequestKind(kind)
		want, err := requestKey(request)
		if err != nil || key != want {
			return errors.New("acquisition request metadata does not match its exact request key")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count > maxAcquisitions {
		return errors.New("acquisition request metadata exceeds its row bound")
	}
	return nil
}

func (s *spool) validateAcquisitions(ctx context.Context) error {
	rows, err := s.database.QueryContext(ctx, `SELECT `+acquisitionMetadataColumns+` FROM acquisitions ORDER BY acquisition_id LIMIT ?`, maxAcquisitions+1)
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for rows.Next() {
		metadata, err := scanAcquisitionMetadata(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if !canonicalSHA256.MatchString(metadata.acquisitionID) || metadata.acquisitionID != metadata.manifestSHA256 {
			rows.Close()
			return errors.New("acquisition metadata contains an invalid content address")
		}
		ids = append(ids, metadata.acquisitionID)
		if len(ids) > maxAcquisitions {
			rows.Close()
			return errors.New("acquisition metadata exceeds its row bound")
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.loadAcquisition(ctx, id, true); err != nil {
			return fmt.Errorf("validate committed acquisition %s: %w", id, err)
		}
	}
	return nil
}

func (s *spool) validateContentAddressedDirectories() error {
	if err := s.validateReviewedHardlinkGraph(); err != nil {
		return err
	}
	pending, err := sortedDirectoryEntries(s.root, pendingDirectory, maxRetainedPendingEntries)
	if err != nil {
		return err
	}
	for _, entry := range pending {
		if pendingEntryPattern.FindStringSubmatch(entry.Name()) == nil {
			return fmt.Errorf("retained acquisition stage has unknown entry %q", entry.Name())
		}
	}
	blobEntries, err := sortedDirectoryEntries(s.root, blobsDirectory, maxAcquisitions*3+maxPendingAcquisitions*3)
	if err != nil {
		return err
	}
	var physicalBlobBytes int64
	for _, entry := range blobEntries {
		if !canonicalSHA256.MatchString(entry.Name()) {
			return fmt.Errorf("blob directory contains non-content-addressed entry %q", entry.Name())
		}
		body, _, err := readVerifiedFileAt(s.root, blobsDirectory, entry.Name(),
			"content-addressed acquisition blob", entry.Name(), -1, maxEvidenceEnvelopeBytes, 1, 2)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return errors.New("content-addressed acquisition blob must not be empty")
		}
		physicalBlobBytes += int64(len(body))
		if physicalBlobBytes > maxAggregateRawBytes+maxAggregateEvidence+maxAggregateEnvelope {
			return errors.New("physical acquisition blobs exceed the bounded v2 capacity")
		}
	}
	manifestEntries, err := sortedDirectoryEntries(s.root, manifestsDirectory, maxAcquisitions+maxPendingAcquisitions)
	if err != nil {
		return err
	}
	var physicalManifestBytes int64
	for _, entry := range manifestEntries {
		if !strings.HasSuffix(entry.Name(), ".json") || !canonicalSHA256.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
			return fmt.Errorf("manifest directory contains non-content-addressed entry %q", entry.Name())
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		body, _, err := readVerifiedFileAt(s.root, manifestsDirectory, entry.Name(),
			"content-addressed acquisition manifest", id, -1, maxManifestBytes, 1, 2)
		if err != nil {
			return err
		}
		if _, err := decodeManifest(body); err != nil {
			return err
		}
		physicalManifestBytes += int64(len(body))
		if physicalManifestBytes > maxAggregateManifest {
			return errors.New("physical acquisition manifests exceed the bounded v2 capacity")
		}
	}
	return nil
}

func sameAcquisitionIdentity(left, right acquiredProviderResponse) bool {
	left.replayOnly = false
	right.replayOnly = false
	return reflect.DeepEqual(left, right)
}
