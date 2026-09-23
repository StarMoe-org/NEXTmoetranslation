package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"moesekai/server/offline/internal/lyricsacquisition"
)

const (
	maxExactReplayRuntimeCopyDepth   = 4
	maxExactReplayRuntimeCopyEntries = 8192
	maxExactReplayRuntimeCopyFile    = int64(64 << 20)
	maxExactReplayRuntimeCopyBytes   = int64(512 << 20)
	exactReplayRuntimeCopySuffix     = ".sekaipedia-list-replay-runtime-copy"
)

type exactReplayRuntimeCopyBudget struct {
	entries int
	bytes   int64
}

type exactReplayCopyDirectory struct {
	path string
	file *os.File
	info os.FileInfo
}

func exactReplayRuntimeCopyPath(outputLedgerPath string) string {
	return outputLedgerPath + exactReplayRuntimeCopySuffix
}

// readExactAcquisitionReplaySource is the explicit historical-copy
// compatibility boundary. It never opens the supplied source ledger through
// the mutating reconciliation path. Instead it first makes and byte-verifies a
// create-exclusive private runtime copy, then validates the complete copy with
// OpenLedger and selects only the caller-pinned AcquisitionID.
func readExactAcquisitionReplaySource(
	ctx context.Context,
	sourceLedgerRoot string,
	runtimeCopyRoot string,
	acquisitionID lyricsacquisition.AcquisitionID,
) (result lyricsacquisition.Acquisition, resultErr error) {
	if ctx == nil {
		return lyricsacquisition.Acquisition{}, errors.New("exact replay context is required")
	}
	if err := ctx.Err(); err != nil {
		return lyricsacquisition.Acquisition{}, err
	}
	if !sha256Pattern.MatchString(string(acquisitionID)) {
		return lyricsacquisition.Acquisition{}, errors.New("exact replay acquisition ID is invalid")
	}
	if err := provisionExactReplayRuntimeCopy(sourceLedgerRoot, runtimeCopyRoot); err != nil {
		return lyricsacquisition.Acquisition{}, err
	}
	ledger, err := lyricsacquisition.OpenLedger(ctx, runtimeCopyRoot)
	if err != nil {
		return lyricsacquisition.Acquisition{}, fmt.Errorf("open fully validated exact replay runtime ledger copy: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, ledger.Close()) }()
	replayed, err := ledger.ReplayByAcquisitionID(ctx, acquisitionID)
	if err != nil {
		return lyricsacquisition.Acquisition{}, fmt.Errorf("replay exact plan-bound List acquisition ID: %w", err)
	}
	if !replayed.ReplayOnly || replayed.AcquisitionID != acquisitionID {
		return lyricsacquisition.Acquisition{}, errors.New("exact replay runtime ledger returned a non-exact acquisition")
	}
	return replayed, nil
}

func provisionExactReplayRuntimeCopy(sourceRoot, destinationRoot string) (resultErr error) {
	if sourceRoot == "" || destinationRoot == "" ||
		strings.TrimSpace(destinationRoot) != destinationRoot || !filepath.IsAbs(destinationRoot) ||
		filepath.Clean(destinationRoot) != destinationRoot || sourceRoot == destinationRoot ||
		pathsOverlap(sourceRoot, destinationRoot) {
		return errors.New("exact replay runtime-copy paths are invalid or overlapping")
	}
	source, err := openCanonicalDirectory(sourceRoot, directoryPolicy{
		label: "historical exact replay source ledger", private: true, effectiveUID: true,
	})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	parentPath := filepath.Dir(destinationRoot)
	parent, err := openCanonicalDirectory(parentPath, directoryPolicy{
		label: "exact replay runtime-copy parent", private: true, effectiveUID: true,
	})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	if _, err := os.Lstat(destinationRoot); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("exact replay runtime-copy destination already exists")
		}
		return fmt.Errorf("inspect exact replay runtime-copy destination: %w", err)
	}
	destination, err := createExactReplayCopyDirectory(parent.file, parentPath, filepath.Base(destinationRoot))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, destination.Close()) }()
	budget := &exactReplayRuntimeCopyBudget{}
	if err := copyExactReplayDirectory(source, destination, "", 0, budget); err != nil {
		return err
	}
	if budget.entries == 0 || budget.entries > maxExactReplayRuntimeCopyEntries ||
		budget.bytes <= 0 || budget.bytes > maxExactReplayRuntimeCopyBytes {
		return errors.New("exact replay runtime-copy final bounds are invalid")
	}
	if err := source.verify(); err != nil {
		return fmt.Errorf("historical exact replay source changed during runtime-copy provisioning: %w", err)
	}
	if err := destination.verify(); err != nil {
		return err
	}
	if err := destination.file.Sync(); err != nil {
		return fmt.Errorf("sync exact replay runtime-copy root: %w", err)
	}
	if err := parent.file.Sync(); err != nil {
		return fmt.Errorf("sync exact replay runtime-copy parent: %w", err)
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))) {
			return true
		}
	}
	return false
}

func copyExactReplayDirectory(
	source *pinnedDirectory,
	destination *exactReplayCopyDirectory,
	relative string,
	depth int,
	budget *exactReplayRuntimeCopyBudget,
) error {
	if source == nil || destination == nil || budget == nil || depth > maxExactReplayRuntimeCopyDepth {
		return errors.New("exact replay runtime-copy directory boundary is invalid")
	}
	if err := source.verify(); err != nil {
		return err
	}
	if err := destination.verify(); err != nil {
		return err
	}
	entries, err := source.file.ReadDir(maxExactReplayRuntimeCopyEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("enumerate historical exact replay source ledger: %w", err)
	}
	if len(entries) > maxExactReplayRuntimeCopyEntries-budget.entries {
		return errors.New("exact replay runtime-copy entry bound is exceeded")
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return errors.New("historical exact replay source contains an invalid entry name")
		}
		budget.entries++
		entryRelative := name
		if relative != "" {
			entryRelative = filepath.Join(relative, name)
		}
		entryPath := filepath.Join(source.path, name)
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect historical exact replay source entry %q: %w", entryRelative, err)
		}
		switch info.Mode().Type() {
		case os.ModeDir:
			if depth == maxExactReplayRuntimeCopyDepth {
				return errors.New("exact replay runtime-copy directory depth is exceeded")
			}
			sourceChild, err := openDirectoryAt(source, name, entryPath, directoryPolicy{
				label: "historical exact replay source directory", private: true, effectiveUID: true,
			})
			if err != nil {
				return err
			}
			destinationChild, createErr := createExactReplayCopyDirectory(destination.file, destination.path, name)
			if createErr != nil {
				_ = sourceChild.Close()
				return createErr
			}
			copyErr := copyExactReplayDirectory(sourceChild, destinationChild, entryRelative, depth+1, budget)
			closeErr := errors.Join(sourceChild.Close(), destinationChild.Close())
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		case 0:
			body, err := readExactReplayCopySourceFile(source, name, entryPath, entryRelative, budget)
			if err != nil {
				return err
			}
			if err := writeExactReplayCopyFile(destination, name, entryRelative, body); err != nil {
				return err
			}
		default:
			return fmt.Errorf("historical exact replay source entry %q is not a regular file or directory", entryRelative)
		}
	}
	if err := source.verify(); err != nil {
		return fmt.Errorf("historical exact replay source directory changed during copy: %w", err)
	}
	if err := destination.verify(); err != nil {
		return err
	}
	return destination.file.Sync()
}

func readExactReplayCopySourceFile(
	parent *pinnedDirectory,
	name, path, relative string,
	budget *exactReplayRuntimeCopyBudget,
) ([]byte, error) {
	if err := parent.verify(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	owner, ownerOK := inputOwner(before)
	links := inputLinkCount(before)
	if before.Mode().Type() != 0 || before.Mode().Perm() != 0o600 || !ownerOK || int(owner) != os.Geteuid() ||
		(links != 1 && links != 2) || before.Size() < 0 || before.Size() > maxExactReplayRuntimeCopyFile ||
		budget.bytes > maxExactReplayRuntimeCopyBytes-before.Size() {
		return nil, fmt.Errorf("historical exact replay source file %q violates the bounded private-ledger contract", relative)
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("historical exact replay source file descriptor is invalid")
	}
	defer file.Close()
	opened, openErr := file.Stat()
	pathOpened, pathErr := os.Lstat(path)
	if openErr != nil || pathErr != nil || !sameStableRegularFile(before, opened) || !sameStableRegularFile(before, pathOpened) {
		return nil, errors.Join(fmt.Errorf("historical exact replay source file %q changed while being pinned", relative), openErr, pathErr)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxExactReplayRuntimeCopyFile+1))
	if err != nil || int64(len(body)) != before.Size() {
		return nil, errors.Join(fmt.Errorf("read historical exact replay source file %q", relative), err)
	}
	after, afterErr := file.Stat()
	pathAfter, pathAfterErr := os.Lstat(path)
	if afterErr != nil || pathAfterErr != nil || !sameStableRegularFile(before, after) || !sameStableRegularFile(before, pathAfter) {
		return nil, errors.Join(fmt.Errorf("historical exact replay source file %q changed while being copied", relative), afterErr, pathAfterErr)
	}
	budget.bytes += int64(len(body))
	return body, nil
}

func createExactReplayCopyDirectory(parent *os.File, parentPath, name string) (*exactReplayCopyDirectory, error) {
	if parent == nil || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, errors.New("exact replay runtime-copy directory name is invalid")
	}
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
		return nil, fmt.Errorf("create exact replay runtime-copy directory: %w", err)
	}
	path := filepath.Join(parentPath, name)
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("pin exact replay runtime-copy directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("exact replay runtime-copy directory descriptor is invalid")
	}
	fail := func(cause error) (*exactReplayCopyDirectory, error) {
		return nil, errors.Join(cause, file.Close())
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return fail(err)
	}
	info, infoErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	owner, ownerOK := inputOwner(info)
	if infoErr != nil || pathErr != nil || !os.SameFile(info, pathInfo) || info.Mode().Type() != os.ModeDir ||
		info.Mode().Perm() != 0o700 || !ownerOK || int(owner) != os.Geteuid() {
		return fail(errors.Join(errors.New("exact replay runtime-copy directory changed while being pinned"), infoErr, pathErr))
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := parent.Sync(); err != nil {
		return fail(err)
	}
	return &exactReplayCopyDirectory{path: path, file: file, info: info}, nil
}

func (directory *exactReplayCopyDirectory) verify() error {
	if directory == nil || directory.file == nil || directory.info == nil {
		return errors.New("exact replay runtime-copy directory is not open")
	}
	opened, openErr := directory.file.Stat()
	pathInfo, pathErr := os.Lstat(directory.path)
	owner, ownerOK := inputOwner(opened)
	if openErr != nil || pathErr != nil || !os.SameFile(directory.info, opened) || !os.SameFile(directory.info, pathInfo) ||
		opened.Mode().Type() != os.ModeDir || opened.Mode().Perm() != 0o700 || !ownerOK || int(owner) != os.Geteuid() {
		return errors.Join(errors.New("exact replay runtime-copy directory path or identity changed"), openErr, pathErr)
	}
	return nil
}

func (directory *exactReplayCopyDirectory) Close() error {
	if directory == nil || directory.file == nil {
		return nil
	}
	err := directory.file.Close()
	directory.file = nil
	return err
}

func writeExactReplayCopyFile(destination *exactReplayCopyDirectory, name, relative string, body []byte) error {
	if err := destination.verify(); err != nil {
		return err
	}
	fd, err := unix.Openat(
		int(destination.file.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create exact replay runtime-copy file %q: %w", relative, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(destination.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("exact replay runtime-copy file descriptor is invalid")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()
		return err
	}
	for offset := 0; offset < len(body); {
		written, writeErr := file.WriteAt(body[offset:], int64(offset))
		if written <= 0 || written > len(body)-offset {
			_ = file.Close()
			return errors.Join(fmt.Errorf("exact replay runtime-copy file %q write returned an invalid byte count", relative), writeErr)
		}
		offset += written
		if writeErr != nil {
			_ = file.Close()
			return fmt.Errorf("write exact replay runtime-copy file %q: %w", relative, writeErr)
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync exact replay runtime-copy file %q: %w", relative, err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := destination.file.Sync(); err != nil {
		return err
	}
	path := filepath.Join(destination.path, name)
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ownerOK := inputOwner(before)
	if before.Mode().Type() != 0 || before.Mode().Perm() != 0o600 || !ownerOK || int(owner) != os.Geteuid() ||
		inputLinkCount(before) != 1 || before.Size() != int64(len(body)) {
		return fmt.Errorf("exact replay runtime-copy file %q has invalid persisted metadata", relative)
	}
	readFD, err := unix.Openat(int(destination.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	reader := os.NewFile(uintptr(readFD), path)
	if reader == nil {
		_ = unix.Close(readFD)
		return errors.New("exact replay runtime-copy verification descriptor is invalid")
	}
	persisted, readErr := io.ReadAll(io.LimitReader(reader, int64(len(body))+1))
	opened, statErr := reader.Stat()
	closeErr := reader.Close()
	pathAfter, pathErr := os.Lstat(path)
	if readErr != nil || statErr != nil || closeErr != nil || pathErr != nil ||
		!sameStableRegularFile(before, opened) || !sameStableRegularFile(before, pathAfter) || !bytes.Equal(persisted, body) {
		return errors.Join(fmt.Errorf("exact replay runtime-copy file %q failed byte verification", relative), readErr, statErr, closeErr, pathErr)
	}
	return destination.verify()
}
