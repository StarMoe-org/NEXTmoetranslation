package lyricsextractionplan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VerifyDeclaredFiles performs a read-only, offline verification of every
// catalog/resume input and source-snapshot file beneath root. Symlinks and path
// escapes are rejected; extraction and output publication are never invoked.
func VerifyDeclaredFiles(root string, plan Plan) error {
	if err := Validate(plan); err != nil {
		return err
	}
	absoluteRoot, err := directVerificationRoot(root)
	if err != nil {
		return err
	}
	for _, input := range plan.Inputs {
		maximum := MaxCatalogDatabaseBytes
		switch input.Kind {
		case InputResumeReport:
			maximum = int64(plan.Execution.Ceilings.PreflightReportBytes)
		case InputResumeCheckpoint:
			maximum = MaxResumeCheckpointBytes
		}
		if err := verifyDeclaredFile(absoluteRoot, input.Path, input.SizeBytes, input.SHA256, maximum); err != nil {
			return fmt.Errorf("verify input %q: %w", input.ID, err)
		}
		if input.Kind == InputCatalogDatabase {
			if err := verifyNoSQLiteSidecars(absoluteRoot, input.Path); err != nil {
				return fmt.Errorf("verify input %q: %w", input.ID, err)
			}
		}
	}
	for _, source := range plan.SourceSnapshot.Files {
		if err := verifyDeclaredFile(absoluteRoot, source.Path, source.SizeBytes, source.SHA256, MaxSourceFileBytes); err != nil {
			return fmt.Errorf("verify source snapshot file %q: %w", source.Path, err)
		}
	}
	for _, output := range plan.Outputs {
		if err := verifyOutputPathAvailable(absoluteRoot, output.Path); err != nil {
			return fmt.Errorf("verify output %q: %w", output.Kind, err)
		}
	}
	return nil
}

// VerifyRecoverySourceSnapshot independently rederives the exact current source
// set selected by the compiled recovery policy, then compares every identity to
// the plan before fixture acquisition, replay, or publication is allowed.
func VerifyRecoverySourceSnapshot(root string, plan RecoveryPlan) error {
	return verifyRecoverySourceSnapshot(root, plan, false)
}

// VerifyRecoverySourceSnapshotForInspection is the read-only compatibility
// counterpart for historical plans accepted by ValidateRecoveryForInspection.
// It does not make such a plan valid for replay, acquisition, or live canary.
func VerifyRecoverySourceSnapshotForInspection(root string, plan RecoveryPlan) error {
	return verifyRecoverySourceSnapshot(root, plan, true)
}

func verifyRecoverySourceSnapshot(root string, plan RecoveryPlan, inspection bool) error {
	var err error
	if inspection {
		err = ValidateRecoveryForInspection(plan)
	} else {
		err = ValidateRecovery(plan)
	}
	if err != nil {
		return err
	}
	if err := verifyRecoverySourceSnapshotIdentity(root, plan.SourceSnapshot, nil); err != nil {
		return fmt.Errorf("verify recovery source snapshot: %w", err)
	}
	return nil
}

func directVerificationRoot(root string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve verification root: %w", err)
	}
	if root != absoluteRoot || filepath.Clean(root) != root {
		return "", errors.New("verification root must be an explicit canonical absolute path")
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("inspect verification root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", errors.New("verification root must be a real directory, not a symlink")
	}
	resolved, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil || resolved != absoluteRoot {
		return "", errors.New("verification root must not traverse a symlink or filesystem alias")
	}
	return absoluteRoot, nil
}

func verifyDeclaredFile(root, relativePath string, declaredSize int64, declaredSHA256 string, maximum int64) error {
	if !validDataPath(relativePath) || declaredSize <= 0 || declaredSize > maximum || !canonicalSHA256.MatchString(declaredSHA256) {
		return errors.New("declared file identity is invalid")
	}
	resolved, initialInfo, err := resolveRegularFileWithoutSymlinks(root, relativePath)
	if err != nil {
		return err
	}
	if initialInfo.Size() != declaredSize {
		return fmt.Errorf("size mismatch: got %d, want %d", initialInfo.Size(), declaredSize)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened file: %w", err)
	}
	if !os.SameFile(initialInfo, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() != declaredSize {
		return errors.New("path identity or size changed while opening")
	}
	digest := sha256.New()
	readBytes, err := io.Copy(digest, io.LimitReader(file, declaredSize+1))
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}
	if readBytes != declaredSize {
		return fmt.Errorf("size changed while hashing: got %d, want %d", readBytes, declaredSize)
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect hashed file: %w", err)
	}
	if !os.SameFile(openedInfo, finalInfo) || finalInfo.Size() != openedInfo.Size() ||
		!finalInfo.ModTime().Equal(openedInfo.ModTime()) {
		return errors.New("file identity, size, or modification time changed while hashing")
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != declaredSHA256 {
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", actual, declaredSHA256)
	}
	return nil
}

func verifyNoSQLiteSidecars(root, relativePath string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidecar := filepath.Join(root, filepath.FromSlash(relativePath+suffix))
		if _, err := os.Lstat(sidecar); err == nil {
			return fmt.Errorf("catalog snapshot has SQLite sidecar %q", suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect catalog SQLite sidecar %q: %w", suffix, err)
		}
	}
	return nil
}

func verifyOutputPathAvailable(root, relativePath string) error {
	current := root
	segments := strings.Split(relativePath, "/")
	for index, segment := range segments {
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect create-exclusive path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("create-exclusive output path must not traverse a symlink")
		}
		if index == len(segments)-1 {
			return errors.New("create-exclusive output path already exists")
		}
		if !info.IsDir() {
			return errors.New("create-exclusive output parent component is not a directory")
		}
	}
	return errors.New("create-exclusive output path is empty")
}

func resolveRegularFileWithoutSymlinks(root, relativePath string) (string, os.FileInfo, error) {
	current := root
	segments := strings.Split(relativePath, "/")
	for index, segment := range segments {
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if err != nil {
			return "", nil, fmt.Errorf("inspect path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, errors.New("declared file path must not traverse a symlink")
		}
		if index < len(segments)-1 {
			if !info.IsDir() {
				return "", nil, errors.New("declared file parent component is not a directory")
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return "", nil, errors.New("declared file must be regular")
		}
		return current, info, nil
	}
	return "", nil, errors.New("declared file path is empty")
}
