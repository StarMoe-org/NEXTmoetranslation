package lyricsacquisition

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
	"os"

	"golang.org/x/sys/unix"
	sqlite "modernc.org/sqlite"
)

type sqliteRuntimeRestorer interface {
	NewRestore(string) (*sqlite.Backup, error)
}

func restoreMetadataRuntime(ctx context.Context, database *sql.DB, root *privateRoot, metadataFile *os.File, binding *pinnedMetadataBinding) error {
	if database == nil || root == nil || metadataFile == nil || binding == nil {
		return errors.New("pinned acquisition metadata restore boundary is invalid")
	}
	if err := verifyPinnedMetadataDescriptor(root, metadataFile, binding); err != nil {
		return err
	}
	expected, _, _ := binding.snapshot()
	descriptorPath := fmt.Sprintf("/dev/fd/%d", metadataFile.Fd())
	descriptor, err := os.Open(descriptorPath)
	if err != nil {
		return fmt.Errorf("open pinned acquisition metadata descriptor path: %w", err)
	}
	descriptorStat, statErr := fstatFile(descriptor)
	closeErr := descriptor.Close()
	if statErr != nil || closeErr != nil || !sameTrustedMetadata(expected, descriptorStat) {
		return fmt.Errorf("pinned acquisition metadata descriptor path changed: %w", errors.Join(statErr, closeErr))
	}
	if testHookBeforeSQLiteRestore != nil {
		if err := testHookBeforeSQLiteRestore(); err != nil {
			return err
		}
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("obtain acquisition metadata restore connection: %w", err)
	}
	rawErr := connection.Raw(func(raw any) error {
		restorer, ok := raw.(sqliteRuntimeRestorer)
		if !ok {
			return errors.New("reviewed modernc SQLite connection has no restore boundary")
		}
		backup, err := restorer.NewRestore("file:" + descriptorPath + "?mode=ro&immutable=1")
		if err != nil {
			return err
		}
		more, stepErr := backup.Step(-1)
		finishErr := backup.Finish()
		if more {
			stepErr = errors.Join(stepErr, errors.New("acquisition metadata restore did not finish in one bounded step"))
		}
		return errors.Join(stepErr, finishErr)
	})
	closeErr = connection.Close()
	if rawErr != nil || closeErr != nil {
		return fmt.Errorf("restore pinned acquisition metadata snapshot: %w", errors.Join(rawErr, closeErr))
	}
	if err := verifyPinnedMetadataDescriptor(root, metadataFile, binding); err != nil {
		return err
	}
	return nil
}

func (s *spool) captureMetadataRuntimeSnapshot(ctx context.Context) ([]byte, error) {
	if s.database == nil || s.metadataConnector == nil {
		return nil, errors.New("private acquisition metadata runtime is not open")
	}
	connection, err := s.database.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("obtain private acquisition metadata snapshot connection: %w", err)
	}
	defer connection.Close()
	var body []byte
	if err := connection.Raw(func(raw any) error {
		serializer, ok := raw.(sqliteRuntimeSerializer)
		if !ok {
			return errors.New("reviewed modernc SQLite connection has no serialization boundary")
		}
		serialized, err := serializer.Serialize()
		if err != nil {
			return err
		}
		body = append([]byte(nil), serialized...)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("serialize private acquisition metadata runtime: %w", err)
	}
	if len(body) < 100 || int64(len(body)) > maxMetadataBytes || string(body[:16]) != "SQLite format 3\x00" {
		return nil, errors.New("private acquisition metadata runtime serialization is invalid")
	}
	return body, nil
}

func (s *spool) persistInitialMetadataSnapshot(body []byte) error {
	if s.metadataFile == nil || s.metadataBinding == nil || s.metadataConnector == nil || len(body) < 100 || int64(len(body)) > maxMetadataBytes {
		return errors.New("initial private acquisition metadata snapshot is invalid")
	}
	if err := verifyPinnedMetadataDescriptor(s.root, s.metadataFile, s.metadataBinding); err != nil {
		return err
	}
	expected, _, pathName := s.metadataBinding.snapshot()
	if expected.size != 0 {
		return errors.New("initial private acquisition metadata snapshot target is not empty")
	}
	for offset := 0; offset < len(body); {
		written, err := s.metadataFile.WriteAt(body[offset:], int64(offset))
		if err != nil {
			return fmt.Errorf("write initial private acquisition metadata snapshot: %w", err)
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		offset += written
	}
	if err := s.metadataFile.Truncate(int64(len(body))); err != nil {
		return fmt.Errorf("truncate initial private acquisition metadata snapshot: %w", err)
	}
	if err := s.metadataFile.Sync(); err != nil {
		return fmt.Errorf("sync initial private acquisition metadata snapshot: %w", err)
	}
	if err := s.root.file.Sync(); err != nil {
		return fmt.Errorf("sync initial private acquisition metadata publication: %w", err)
	}
	stat, err := fstatFile(s.metadataFile)
	pathStat, pathErr := statAt(s.root.file, pathName)
	if err != nil || pathErr != nil || !sameFileIdentity(expected.identity, stat.identity) || !sameTrustedMetadata(stat, pathStat) ||
		stat.size != int64(len(body)) || validatePrivateRegularStat(stat, "initial private acquisition metadata snapshot", 1) != nil {
		return errors.Join(errors.New("initial private acquisition metadata snapshot changed during publication"), err, pathErr)
	}
	digest := sha256.Sum256(body)
	s.metadataBinding.update(stat, hex.EncodeToString(digest[:]), pathName)
	return verifyPinnedMetadataDescriptor(s.root, s.metadataFile, s.metadataBinding)
}

func (s *spool) persistMetadataRuntimeSnapshot(ctx context.Context, stage string) error {
	body, err := s.captureMetadataRuntimeSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := s.reachMetadataBoundary(metadataBoundaryAfterRuntimeSerialization); err != nil {
		return err
	}
	return s.persistMetadataSnapshot(body, stage)
}

// metadataBoundaryContractFor is the read-only boundary map. Its switch makes
// duplicate boundary values a compile-time error and rejects aliases that do
// not name the exact persistence operation and phase.
func metadataBoundaryContractFor(boundary metadataPersistenceBoundary) (metadataBoundaryContract, bool) {
	switch boundary {
	case metadataBoundaryAfterRuntimeSerialization:
		return metadataBoundaryContract{operation: metadataOperationRuntimeSerialization, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeRecoverySlotNamespaceSync:
		return metadataBoundaryContract{operation: metadataOperationRecoverySlotNamespaceSync, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterRecoverySlotNamespaceSync:
		return metadataBoundaryContract{operation: metadataOperationRecoverySlotNamespaceSync, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeSnapshotWrite:
		return metadataBoundaryContract{operation: metadataOperationSnapshotWrite, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterSnapshotWrite:
		return metadataBoundaryContract{operation: metadataOperationSnapshotWrite, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeSnapshotFileSync:
		return metadataBoundaryContract{operation: metadataOperationSnapshotFileSync, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterSnapshotFileSync:
		return metadataBoundaryContract{operation: metadataOperationSnapshotFileSync, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeSelectorWrite:
		return metadataBoundaryContract{operation: metadataOperationSelectorWrite, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterSelectorWrite:
		return metadataBoundaryContract{operation: metadataOperationSelectorWrite, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeSelectorFileSync:
		return metadataBoundaryContract{operation: metadataOperationSelectorFileSync, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterSelectorFileSync:
		return metadataBoundaryContract{operation: metadataOperationSelectorFileSync, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeSelectorNamespaceSync:
		return metadataBoundaryContract{operation: metadataOperationSelectorNamespaceSync, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterSelectorNamespaceSync:
		return metadataBoundaryContract{operation: metadataOperationSelectorNamespaceSync, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeConnectorRebind:
		return metadataBoundaryContract{operation: metadataOperationConnectorRebind, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterConnectorRebind:
		return metadataBoundaryContract{operation: metadataOperationConnectorRebind, phase: metadataBoundaryPhaseAfter}, true
	case metadataBoundaryBeforeStandbyVerification:
		return metadataBoundaryContract{operation: metadataOperationStandbyVerification, phase: metadataBoundaryPhaseBefore}, true
	case metadataBoundaryAfterStandbyVerification:
		return metadataBoundaryContract{operation: metadataOperationStandbyVerification, phase: metadataBoundaryPhaseAfter}, true
	default:
		return metadataBoundaryContract{}, false
	}
}

func (s *spool) reachMetadataBoundary(boundary metadataPersistenceBoundary) error {
	if _, valid := metadataBoundaryContractFor(boundary); !valid {
		return fmt.Errorf("unknown acquisition metadata persistence boundary %q", boundary)
	}
	if s.hooks.metadataBoundary == nil {
		return nil
	}
	return s.hooks.metadataBoundary(boundary)
}

func (s *spool) metadataSnapshotWriteAt(file *os.File, body []byte, offset int64) (int, error) {
	if s.hooks.metadataSnapshotWriteAt != nil {
		return s.hooks.metadataSnapshotWriteAt(file, body, offset)
	}
	return file.WriteAt(body, offset)
}

func (s *spool) syncMetadataRecoverySlotNamespace() error {
	return s.root.file.Sync()
}

func (s *spool) syncMetadataSelectorNamespace() error {
	if err := s.root.verifyDirectory(metadataStateDirectory); err != nil {
		return err
	}
	return s.root.directories[metadataStateDirectory].file.Sync()
}

func (s *spool) verifyMetadataStandbySlot() error {
	if s.metadataStandbyFile == nil {
		return nil
	}
	if !metadataSlotNameAllowed(s.metadataStandbyPath) || s.metadataStandbyDigest == "" {
		return errors.New("private acquisition metadata standby slot is not fully validated")
	}
	_, digest, err := readPinnedMetadataDescriptorAt(
		s.root, s.metadataStandbyFile, s.metadataStandbyStat, false,
		s.metadataStandbyPath, "private acquisition metadata standby slot",
	)
	if err != nil {
		return err
	}
	if digest != s.metadataStandbyDigest {
		return errors.New("private acquisition metadata standby slot bytes changed")
	}
	return nil
}

func (s *spool) verifyMetadataWriteSlot() error {
	if s.metadataWriteFile == nil || !metadataSlotNameAllowed(s.metadataWritePath) {
		return errors.New("private acquisition metadata reusable slot is not descriptor-pinned")
	}
	opened, err := fstatFile(s.metadataWriteFile)
	pathStat, pathErr := statAt(s.root.file, s.metadataWritePath)
	if err != nil || pathErr != nil || !sameTrustedMetadata(s.metadataWriteStat, opened) || !sameTrustedMetadata(opened, pathStat) {
		return errors.Join(errors.New("private acquisition metadata reusable slot changed before reuse"), err, pathErr)
	}
	if err := validatePrivateRegularStat(opened, "private acquisition metadata reusable slot", 1); err != nil {
		return err
	}
	if opened.size < 0 || opened.size > maxMetadataBytes {
		return errors.New("private acquisition metadata reusable slot exceeds its byte bound")
	}
	active, _, activePath := s.metadataBinding.snapshot()
	if s.metadataWritePath == activePath || sameFileIdentity(opened.identity, active.identity) {
		return errors.New("private acquisition metadata reusable slot aliases the active snapshot")
	}
	if s.metadataStandbyFile != nil &&
		(s.metadataWritePath == s.metadataStandbyPath || sameFileIdentity(opened.identity, s.metadataStandbyStat.identity)) {
		return errors.New("private acquisition metadata reusable slot aliases the complete standby snapshot")
	}
	return nil
}

func (s *spool) allocateMetadataWriteSlot() error {
	if s.metadataWriteFile != nil {
		return s.verifyMetadataWriteSlot()
	}
	_, _, activePath := s.metadataBinding.snapshot()
	candidates := make([]string, 0, maxMetadataRecoverySlots+1)
	candidates = append(candidates, metadataSnapshotTempName)
	for index := 0; index < maxMetadataRecoverySlots; index++ {
		candidates = append(candidates, metadataRecoverySlotName(index))
	}
	for _, name := range candidates {
		if name == activePath || name == s.metadataStandbyPath {
			continue
		}
		_, statErr := statAt(s.root.file, name)
		create := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !create {
			return fmt.Errorf("inspect bounded metadata recovery slot %s: %w", name, statErr)
		}
		slot, err := openMetadataSlot(s.root, name, create)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		s.metadataWriteFile = slot.file
		s.metadataWriteStat = slot.stat
		s.metadataWritePath = slot.name
		if create {
			if err := s.reachMetadataBoundary(metadataBoundaryBeforeRecoverySlotNamespaceSync); err != nil {
				return err
			}
			if err := s.syncMetadataRecoverySlotNamespace(); err != nil {
				return fmt.Errorf("sync acquisition metadata reusable-slot namespace: %w", err)
			}
			if err := s.reachMetadataBoundary(metadataBoundaryAfterRecoverySlotNamespaceSync); err != nil {
				return err
			}
		}
		return s.verifyMetadataWriteSlot()
	}
	return errors.New("HOLD: no descriptor-verified inactive metadata slot can be reused while preserving complete active and standby snapshots")
}

func (s *spool) prepareMetadataWriteSnapshot(body []byte) (trustedStat, string, error) {
	if err := s.allocateMetadataWriteSlot(); err != nil {
		return trustedStat{}, "", err
	}
	if err := s.verifyMetadataFile("immediately before reusable metadata write"); err != nil {
		return trustedStat{}, "", err
	}
	if err := s.verifyRetainedMetadataSnapshot(); err != nil {
		return trustedStat{}, "", err
	}
	writeFile := s.metadataWriteFile
	writePath := s.metadataWritePath
	before, err := fstatFile(writeFile)
	pathBefore, pathErr := statAt(s.root.file, writePath)
	if err != nil || pathErr != nil || !sameTrustedMetadata(s.metadataWriteStat, before) || !sameTrustedMetadata(before, pathBefore) {
		return trustedStat{}, "", errors.Join(errors.New("private acquisition metadata reusable slot changed before write"), err, pathErr)
	}
	if err := s.reachMetadataBoundary(metadataBoundaryBeforeSnapshotWrite); err != nil {
		return trustedStat{}, "", err
	}
	descriptorBeforeWrite, descriptorErr := fstatFile(writeFile)
	pathBefore, pathErr = statAt(s.root.file, writePath)
	if descriptorErr != nil || pathErr != nil || !sameTrustedMetadata(before, descriptorBeforeWrite) ||
		!sameTrustedMetadata(descriptorBeforeWrite, pathBefore) {
		return trustedStat{}, "", errors.Join(
			errors.New("private acquisition metadata reusable slot descriptor or pathname changed at the read-only write boundary"),
			descriptorErr, pathErr,
		)
	}
	if err := validatePrivateRegularStat(descriptorBeforeWrite, "private acquisition metadata reusable slot", 1); err != nil {
		return trustedStat{}, "", err
	}
	if err := writeFile.Truncate(0); err != nil {
		return trustedStat{}, "", fmt.Errorf("truncate pinned acquisition metadata reusable slot: %w", err)
	}
	for offset := 0; offset < len(body); {
		written, writeErr := s.metadataSnapshotWriteAt(writeFile, body[offset:], int64(offset))
		if written < 0 || written > len(body)-offset {
			return trustedStat{}, "", errors.New("pinned acquisition metadata reusable write returned an invalid byte count")
		}
		offset += written
		if writeErr != nil {
			return trustedStat{}, "", fmt.Errorf("write pinned acquisition metadata reusable snapshot: %w", writeErr)
		}
		if written == 0 {
			return trustedStat{}, "", io.ErrShortWrite
		}
	}
	if err := writeFile.Truncate(int64(len(body))); err != nil {
		return trustedStat{}, "", fmt.Errorf("truncate pinned acquisition metadata reusable snapshot: %w", err)
	}
	if err := s.reachMetadataBoundary(metadataBoundaryAfterSnapshotWrite); err != nil {
		return trustedStat{}, "", err
	}
	if err := s.reachMetadataBoundary(metadataBoundaryBeforeSnapshotFileSync); err != nil {
		return trustedStat{}, "", err
	}
	if err := writeFile.Sync(); err != nil {
		return trustedStat{}, "", fmt.Errorf("sync pinned acquisition metadata reusable snapshot: %w", err)
	}
	if err := s.reachMetadataBoundary(metadataBoundaryAfterSnapshotFileSync); err != nil {
		return trustedStat{}, "", err
	}
	synced, err := fstatFile(writeFile)
	pathSynced, pathErr := statAt(s.root.file, writePath)
	if err != nil || pathErr != nil || !sameFileIdentity(before.identity, synced.identity) || !sameTrustedMetadata(synced, pathSynced) ||
		synced.size != int64(len(body)) || validatePrivateRegularStat(synced, "pinned acquisition metadata reusable snapshot", 1) != nil {
		return trustedStat{}, "", errors.Join(errors.New("pinned acquisition metadata reusable snapshot changed at the final write interval"), err, pathErr)
	}
	readBack, err := io.ReadAll(io.NewSectionReader(writeFile, 0, int64(len(body))+1))
	if err != nil || !bytes.Equal(readBack, body) {
		return trustedStat{}, "", errors.Join(errors.New("pinned acquisition metadata reusable bytes do not match the serialized runtime"), err)
	}
	if err := s.verifyMetadataFile("after reusable metadata write"); err != nil {
		return trustedStat{}, "", err
	}
	if err := s.verifyRetainedMetadataSnapshot(); err != nil {
		return trustedStat{}, "", err
	}
	s.metadataWriteStat = synced
	digest := sha256.Sum256(body)
	return synced, hex.EncodeToString(digest[:]), nil
}

func (s *spool) publishMetadataSelector(slotName, digest string, byteCount, acquisitionCount int64) (metadataSelector, error) {
	if err := s.root.verifyDirectory(metadataStateDirectory); err != nil {
		return metadataSelector{}, err
	}
	_, _, maximumSequence, records, err := s.readMetadataSelectors()
	if err != nil {
		return metadataSelector{}, err
	}
	if maximumSequence == ^uint64(0) {
		return metadataSelector{}, errors.New("metadata selector sequence is exhausted")
	}
	selector := metadataSelector{
		SchemaVersion: metadataSelectorSchemaVersion, Sequence: maximumSequence + 1,
		Slot: slotName, SHA256: digest, ByteCount: byteCount, AcquisitionCount: acquisitionCount,
	}
	body, err := json.Marshal(selector)
	if err != nil {
		return metadataSelector{}, err
	}
	if len(body) > maxMetadataSelectorBytes {
		return metadataSelector{}, errors.New("metadata selector exceeds its byte bound")
	}
	protected := make(map[string]bool, 2)
	for _, record := range records {
		if len(protected) == 2 {
			break
		}
		if metadataSelectorSlotNamePattern.MatchString(record.name) {
			protected[record.name] = true
		}
	}
	var name string
	for index := 0; index < maxMetadataSelectorSlots; index++ {
		candidate := metadataSelectorSlotName(index)
		if !protected[candidate] {
			name = candidate
			break
		}
	}
	if name == "" {
		return metadataSelector{}, errors.New("metadata selector reusable-slot state has no inactive descriptor")
	}
	stateDirectory := s.root.directories[metadataStateDirectory].file
	before, statErr := statAt(stateDirectory, name)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return metadataSelector{}, fmt.Errorf("inspect reusable metadata selector slot: %w", statErr)
	}
	if !created {
		if err := validatePrivateRegularStat(before, "reusable metadata selector slot", 1); err != nil {
			return metadataSelector{}, err
		}
		if before.size < 0 || before.size > maxMetadataSelectorBytes {
			return metadataSelector{}, errors.New("reusable metadata selector slot exceeds its byte bound")
		}
		if err := s.root.verifyKnownLeaf(metadataStateDirectory, name, before.identity); err != nil {
			return metadataSelector{}, err
		}
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if created {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(int(stateDirectory.Fd()), name, flags, 0o600)
	if err != nil {
		return metadataSelector{}, fmt.Errorf("open reusable metadata selector slot: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return metadataSelector{}, errors.New("reusable metadata selector descriptor is invalid")
	}
	fail := func(cause error) (metadataSelector, error) {
		return metadataSelector{}, errors.Join(cause, file.Close())
	}
	if created {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			return fail(fmt.Errorf("secure reusable metadata selector slot: %w", err))
		}
	}
	opened, err := fstatFile(file)
	pathOpened, pathErr := statAt(stateDirectory, name)
	if err != nil || pathErr != nil || !sameTrustedMetadata(opened, pathOpened) || !created && !sameTrustedMetadata(before, opened) {
		return fail(errors.Join(errors.New("reusable metadata selector slot changed while being pinned"), err, pathErr))
	}
	if err := validatePrivateRegularStat(opened, "reusable metadata selector slot", 1); err != nil {
		return fail(err)
	}
	if err := s.reachMetadataBoundary(metadataBoundaryBeforeSelectorWrite); err != nil {
		return fail(err)
	}
	pathBeforeWrite, pathErr := statAt(stateDirectory, name)
	if pathErr != nil || !sameTrustedMetadata(opened, pathBeforeWrite) {
		return fail(errors.Join(errors.New("reusable metadata selector pathname changed at the write boundary"), pathErr))
	}
	if err := file.Truncate(0); err != nil {
		return fail(fmt.Errorf("truncate reusable metadata selector slot: %w", err))
	}
	for offset := 0; offset < len(body); {
		written, writeErr := file.WriteAt(body[offset:], int64(offset))
		if written <= 0 || written > len(body)-offset {
			return fail(errors.Join(io.ErrShortWrite, writeErr))
		}
		offset += written
		if writeErr != nil {
			return fail(writeErr)
		}
	}
	if err := file.Truncate(int64(len(body))); err != nil {
		return fail(fmt.Errorf("truncate complete reusable metadata selector: %w", err))
	}
	if err := s.reachMetadataBoundary(metadataBoundaryAfterSelectorWrite); err != nil {
		return fail(err)
	}
	if err := s.reachMetadataBoundary(metadataBoundaryBeforeSelectorFileSync); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(fmt.Errorf("sync reusable metadata selector: %w", err))
	}
	if err := s.reachMetadataBoundary(metadataBoundaryAfterSelectorFileSync); err != nil {
		return fail(err)
	}
	synced, err := fstatFile(file)
	pathSynced, pathErr := statAt(stateDirectory, name)
	readBack, readErr := io.ReadAll(io.NewSectionReader(file, 0, int64(len(body))+1))
	if err != nil || pathErr != nil || readErr != nil || !sameFileIdentity(opened.identity, synced.identity) ||
		!sameTrustedMetadata(synced, pathSynced) || synced.size != int64(len(body)) || !bytes.Equal(readBack, body) {
		return fail(errors.Join(errors.New("reusable metadata selector changed during durable write"), err, pathErr, readErr))
	}
	if created {
		s.root.rememberLeaf(metadataStateDirectory, name, synced.identity)
		if err := s.reachMetadataBoundary(metadataBoundaryBeforeSelectorNamespaceSync); err != nil {
			return fail(err)
		}
		if err := s.syncMetadataSelectorNamespace(); err != nil {
			return fail(fmt.Errorf("sync reusable metadata selector namespace: %w", err))
		}
		if err := s.reachMetadataBoundary(metadataBoundaryAfterSelectorNamespaceSync); err != nil {
			return fail(err)
		}
	}
	if err := file.Close(); err != nil {
		return metadataSelector{}, err
	}
	return selector, nil
}

func (s *spool) verifyLatestMetadataSelector() error {
	latest, _, _, _, err := s.readMetadataSelectors()
	if err != nil {
		return err
	}
	if latest == nil {
		return errors.New("private acquisition metadata has no durable selector after slot transition")
	}
	active, digest, pathName := s.metadataBinding.snapshot()
	var acquisitionCount int64
	if err := s.database.QueryRow(`SELECT acquisition_count FROM spool_counters WHERE singleton=1`).Scan(&acquisitionCount); err != nil {
		return err
	}
	if latest.Slot != pathName || latest.SHA256 != digest || latest.ByteCount != active.size || latest.AcquisitionCount != acquisitionCount {
		return errors.New("latest metadata selector does not bind the active pinned snapshot")
	}
	return nil
}

func (s *spool) persistMetadataSnapshot(body []byte, stage string) error {
	if s.metadataFile == nil || s.metadataBinding == nil || s.metadataConnector == nil || s.root == nil {
		return errors.New("private acquisition metadata persistence boundary is not open")
	}
	if err := requireAtomicNamespacePublication(); err != nil {
		return err
	}
	if len(body) < 100 || int64(len(body)) > maxMetadataBytes || !bytes.Equal(body[:16], []byte("SQLite format 3\x00")) {
		return errors.New("private acquisition metadata snapshot is invalid")
	}
	if err := s.verifyMetadataFile("before " + stage + " snapshot persistence"); err != nil {
		return err
	}
	writeStat, digest, err := s.prepareMetadataWriteSnapshot(body)
	if err != nil {
		return fmt.Errorf("write acquisition metadata %s reusable snapshot: %w", stage, err)
	}
	var acquisitionCount int64
	if err := s.database.QueryRow(`SELECT acquisition_count FROM spool_counters WHERE singleton=1`).Scan(&acquisitionCount); err != nil {
		return err
	}
	selector, err := s.publishMetadataSelector(s.metadataWritePath, digest, writeStat.size, acquisitionCount)
	if err != nil {
		return fmt.Errorf("publish acquisition metadata %s selector: %w", stage, err)
	}
	previousStat, previousDigest, previousPath := s.metadataBinding.snapshot()
	previousFile := s.metadataFile
	newFile := s.metadataWriteFile
	oldStandbyFile := s.metadataStandbyFile
	oldStandbyStat := s.metadataStandbyStat
	oldStandbyPath := s.metadataStandbyPath
	previous, rebound, rebindErr := s.metadataConnector.rebindMetadataFile(
		newFile,
		writeStat,
		digest,
		selector.Slot,
		func() error { return s.reachMetadataBoundary(metadataBoundaryBeforeConnectorRebind) },
		func() error { return s.reachMetadataBoundary(metadataBoundaryAfterConnectorRebind) },
	)
	if rebound {
		s.metadataFile = newFile
		s.metadataStandbyFile = previousFile
		s.metadataStandbyStat = previousStat
		s.metadataStandbyDigest = previousDigest
		s.metadataStandbyPath = previousPath
		s.metadataWriteFile = oldStandbyFile
		s.metadataWriteStat = oldStandbyStat
		s.metadataWritePath = oldStandbyPath
	}
	if !rebound {
		return fmt.Errorf("rebind acquisition metadata %s snapshot: %w", stage, rebindErr)
	}
	if previous != previousFile {
		rebindErr = errors.Join(rebindErr, errors.New("SQLite connector retained an unexpected previous metadata descriptor"))
	}
	if rebindErr != nil {
		return fmt.Errorf("rebind acquisition metadata %s snapshot: %w", stage, rebindErr)
	}
	if err := s.reachMetadataBoundary(metadataBoundaryBeforeStandbyVerification); err != nil {
		return err
	}
	if err := s.verifyRetainedMetadataSnapshot(); err != nil {
		return err
	}
	if err := s.reachMetadataBoundary(metadataBoundaryAfterStandbyVerification); err != nil {
		return err
	}
	if err := verifyPinnedMetadataDescriptor(s.root, s.metadataFile, s.metadataBinding); err != nil {
		return fmt.Errorf("verify acquisition metadata %s snapshot publication: %w", stage, err)
	}
	if err := s.verifyLatestMetadataSelector(); err != nil {
		return err
	}
	if s.metadataWriteFile == nil {
		return nil
	}
	return s.verifyMetadataWriteSlot()
}

func (s *spool) verifyRetainedMetadataSnapshot() error {
	if s.root == nil || s.metadataFile == nil || s.metadataBinding == nil {
		return errors.New("private acquisition metadata retained-snapshot boundary is not open")
	}
	if s.metadataStandbyFile == nil {
		return nil
	}
	active, _, _ := s.metadataBinding.snapshot()
	if sameFileIdentity(active.identity, s.metadataStandbyStat.identity) {
		return errors.New("active and standby acquisition metadata snapshots alias one inode")
	}
	return s.verifyMetadataStandbySlot()
}

func validateMetadataSidecars(root *privateRoot, allowJournal bool) error {
	if err := root.verify(); err != nil {
		return err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		name := metadataFileName + suffix
		stat, err := statAt(root.file, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect acquisition metadata SQLite sidecar: %w", err)
		}
		if suffix != "-journal" || !allowJournal {
			return fmt.Errorf("acquisition metadata has unsupported SQLite sidecar %s", suffix)
		}
		if err := validatePrivateRegularStat(stat, "acquisition metadata rollback journal", 1); err != nil {
			return err
		}
		if stat.size < 0 || stat.size > maxMetadataBytes+spoolPageSize {
			return errors.New("acquisition metadata rollback journal exceeds its byte bound")
		}
	}
	return nil
}
