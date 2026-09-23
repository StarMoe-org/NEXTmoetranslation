package lyricsacquisition

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	blobsDirectory           = "blobs"
	manifestsDirectory       = "manifests"
	pendingDirectory         = "pending"
	quarantineDirectory      = "quarantine"
	metadataStateDirectory   = ".metadata-state"
	metadataFileName         = "metadata.db"
	metadataSnapshotTempName = ".metadata.db.snapshot.tmp"
	ledgerLockName           = ".ledger.lock"
	migrationManifestName    = "migration-manifest.json"
	maxMigrationManifestSize = 64 << 10
)

var ledgerLockBody = []byte("moesekai-lyrics-acquisition-ledger-lock-v1\n")

var (
	testHookAtomicPublicationPreflight   func(*os.File) error
	testHookBeforeAtomicNoReplacePublish func() error
	testHookBeforeOwnedFileRetire        func() error
)

type fileIdentity struct {
	device uint64
	inode  uint64
}

type trustedStat struct {
	identity fileIdentity
	mode     uint32
	links    uint64
	owner    uint32
	size     int64
}

type pinnedPathComponent struct {
	name string
	file *os.File
	stat trustedStat
}

type pinnedDirectory struct {
	file *os.File
	stat trustedStat
}

type privateRoot struct {
	path       string
	name       string
	file       *os.File
	stat       trustedStat
	ancestors  []pinnedPathComponent
	parentFile *os.File

	rootLocked bool
	lockFile   *os.File
	lockStat   trustedStat

	directories         map[string]pinnedDirectory
	knownLeaves         map[string]fileIdentity
	capturedDirectories map[string]bool
	created             bool
}

func openPrivateRoot(path string, mustCreate, mustExist bool) (*privateRoot, error) {
	if mustCreate && mustExist {
		return nil, errors.New("lyrics acquisition spool root mode is invalid")
	}
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(os.PathSeparator) {
		return nil, errors.New("lyrics acquisition spool root must be a clean absolute non-root path")
	}
	if strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("lyrics acquisition spool root contains NUL")
	}

	parentPath := filepath.Dir(path)
	baseName := filepath.Base(path)
	if err := validateLeafName(baseName); err != nil {
		return nil, fmt.Errorf("validate lyrics acquisition spool root name: %w", err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, string(os.PathSeparator)), string(os.PathSeparator)) {
		if component == atomicPublicationProbeDirectoryName {
			return nil, errors.New("lyrics acquisition spool path uses the reserved descriptor publication preflight namespace")
		}
	}

	ancestors, err := pinAncestorChain(parentPath)
	if err != nil {
		return nil, err
	}
	closeAncestors := func() {
		for index := len(ancestors) - 1; index >= 0; index-- {
			_ = ancestors[index].file.Close()
		}
	}
	parentFile := ancestors[len(ancestors)-1].file
	if err := validatePrivateParentStat(ancestors[len(ancestors)-1].stat); err != nil {
		closeAncestors()
		return nil, err
	}
	if err := runAtomicPublicationPreflight(parentFile); err != nil {
		closeAncestors()
		return nil, err
	}

	_, statErr := statAt(parentFile, baseName)
	exists := statErr == nil
	switch {
	case exists && mustCreate:
		closeAncestors()
		return nil, errors.New("new lyrics acquisition spool root already exists")
	case errors.Is(statErr, os.ErrNotExist) && mustExist:
		closeAncestors()
		return nil, errors.New("existing lyrics acquisition spool root does not exist")
	case statErr != nil && !errors.Is(statErr, os.ErrNotExist):
		closeAncestors()
		return nil, fmt.Errorf("inspect lyrics acquisition spool root: %w", statErr)
	}

	created := false
	if !exists {
		if err := unix.Mkdirat(int(parentFile.Fd()), baseName, 0o700); err != nil {
			closeAncestors()
			return nil, fmt.Errorf("create lyrics acquisition spool root: %w", err)
		}
		created = true
		if err := parentFile.Sync(); err != nil {
			closeAncestors()
			return nil, fmt.Errorf("sync lyrics acquisition spool root creation: %w", err)
		}
	}

	rootFile, rootStat, err := openDirectoryAt(parentFile, baseName)
	if err != nil {
		closeAncestors()
		return nil, fmt.Errorf("pin lyrics acquisition spool root: %w", err)
	}
	if created {
		if err := unix.Fchmod(int(rootFile.Fd()), 0o700); err != nil {
			_ = rootFile.Close()
			closeAncestors()
			return nil, fmt.Errorf("secure lyrics acquisition spool root: %w", err)
		}
		rootStat, err = fstatFile(rootFile)
		if err != nil {
			_ = rootFile.Close()
			closeAncestors()
			return nil, err
		}
	}
	if err := validatePrivateDirectoryStat(rootStat, "lyrics acquisition spool root"); err != nil {
		_ = rootFile.Close()
		closeAncestors()
		return nil, err
	}
	if rootStat.identity.device != ancestors[len(ancestors)-1].stat.identity.device {
		_ = rootFile.Close()
		closeAncestors()
		return nil, errors.New("HOLD: lyrics acquisition spool root must share the descriptor-publication-preflight filesystem of its pinned parent")
	}
	root := &privateRoot{
		path: path, name: baseName, file: rootFile, stat: rootStat, ancestors: ancestors, parentFile: parentFile,
		directories: make(map[string]pinnedDirectory, 5), knownLeaves: make(map[string]fileIdentity),
		capturedDirectories: make(map[string]bool, 5), created: created,
	}
	if created {
		if err := root.file.Sync(); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("sync new lyrics acquisition spool root: %w", err)
		}
	}
	return root, nil
}

func runAtomicPublicationPreflight(directory *os.File) error {
	if testHookAtomicPublicationPreflight != nil {
		if err := testHookAtomicPublicationPreflight(directory); err != nil {
			return err
		}
	}
	return preflightAtomicNamespacePublication(directory)
}

func openAtomicPublicationProbeDirectory(parent *os.File, name string) (*os.File, trustedStat, bool, error) {
	if parent == nil {
		return nil, trustedStat{}, false, errors.New("descriptor publication probe parent is required")
	}
	if err := validateLeafName(name); err != nil {
		return nil, trustedStat{}, false, fmt.Errorf("validate descriptor publication probe directory name: %w", err)
	}
	before, statErr := statAt(parent, name)
	created := false
	if errors.Is(statErr, os.ErrNotExist) {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, trustedStat{}, false, fmt.Errorf("create private descriptor publication probe directory: %w", err)
			}
		} else {
			created = true
			if err := parent.Sync(); err != nil {
				return nil, trustedStat{}, false, fmt.Errorf("sync private descriptor publication probe directory creation: %w", err)
			}
		}
	} else if statErr != nil {
		return nil, trustedStat{}, false, fmt.Errorf("inspect private descriptor publication probe directory: %w", statErr)
	}
	probe, opened, err := openDirectoryAt(parent, name)
	if err != nil {
		return nil, trustedStat{}, false, fmt.Errorf("pin private descriptor publication probe directory: %w", err)
	}
	fail := func(cause error) (*os.File, trustedStat, bool, error) {
		return nil, trustedStat{}, false, errors.Join(cause, probe.Close())
	}
	if created {
		if err := unix.Fchmod(int(probe.Fd()), 0o700); err != nil {
			return fail(fmt.Errorf("secure private descriptor publication probe directory: %w", err))
		}
		opened, err = fstatFile(probe)
		if err != nil {
			return fail(err)
		}
		if err := probe.Sync(); err != nil {
			return fail(fmt.Errorf("sync private descriptor publication probe directory metadata: %w", err))
		}
		if err := parent.Sync(); err != nil {
			return fail(fmt.Errorf("sync private descriptor publication probe parent: %w", err))
		}
	}
	pathStat, pathErr := statAt(parent, name)
	if pathErr != nil || !sameTrustedMetadata(opened, pathStat) || !created && statErr == nil && !sameTrustedMetadata(before, opened) {
		return fail(errors.Join(errors.New("private descriptor publication probe directory changed while being pinned"), pathErr))
	}
	if err := validatePrivateDirectoryStat(opened, "private descriptor publication probe directory"); err != nil {
		return fail(err)
	}
	return probe, opened, created, nil
}

func verifyAtomicPublicationProbeDirectory(parent, probe *os.File, name string, expected trustedStat) error {
	if parent == nil || probe == nil {
		return errors.New("descriptor publication probe directory binding is incomplete")
	}
	opened, openErr := fstatFile(probe)
	pathStat, pathErr := statAt(parent, name)
	stableOpened := sameFileIdentity(expected.identity, opened.identity) && expected.mode == opened.mode && expected.owner == opened.owner
	stablePath := sameFileIdentity(expected.identity, pathStat.identity) && expected.mode == pathStat.mode && expected.owner == pathStat.owner
	if openErr != nil || pathErr != nil || !stableOpened || !stablePath {
		return errors.Join(errors.New("private descriptor publication probe directory binding changed"), openErr, pathErr)
	}
	return validatePrivateDirectoryStat(opened, "private descriptor publication probe directory")
}

func openVerifiedAtomicPublicationProbeFile(directory *os.File, name, label string, expected []byte, allowedLinks ...uint64) (*os.File, trustedStat, error) {
	if directory == nil {
		return nil, trustedStat{}, errors.New("descriptor publication probe directory is required")
	}
	if err := validateLeafName(name); err != nil {
		return nil, trustedStat{}, err
	}
	before, err := statAt(directory, name)
	if err != nil {
		return nil, trustedStat{}, fmt.Errorf("inspect %s: %w", label, err)
	}
	if err := validatePrivateRegularStat(before, label, allowedLinks...); err != nil {
		return nil, trustedStat{}, err
	}
	if before.size != int64(len(expected)) {
		return nil, trustedStat{}, fmt.Errorf("%s has an invalid byte count", label)
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, trustedStat{}, fmt.Errorf("open %s: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), label)
	if file == nil {
		_ = unix.Close(fd)
		return nil, trustedStat{}, fmt.Errorf("%s descriptor is invalid", label)
	}
	fail := func(cause error) (*os.File, trustedStat, error) {
		return nil, trustedStat{}, errors.Join(cause, file.Close())
	}
	opened, openErr := fstatFile(file)
	pathOpened, pathErr := statAt(directory, name)
	if openErr != nil || pathErr != nil || !sameTrustedMetadata(before, opened) || !sameTrustedMetadata(opened, pathOpened) {
		return fail(errors.Join(fmt.Errorf("%s changed while being pinned", label), openErr, pathErr))
	}
	body := make([]byte, len(expected))
	if _, err := file.ReadAt(body, 0); err != nil {
		return fail(fmt.Errorf("read %s: %w", label, err))
	}
	if !bytes.Equal(body, expected) {
		return fail(fmt.Errorf("%s has unrecognized bytes", label))
	}
	openedAfter, openedAfterErr := fstatFile(file)
	pathAfter, pathAfterErr := statAt(directory, name)
	if openedAfterErr != nil || pathAfterErr != nil || !sameTrustedMetadata(opened, openedAfter) || !sameTrustedMetadata(opened, pathAfter) {
		return fail(errors.Join(fmt.Errorf("%s changed while being verified", label), openedAfterErr, pathAfterErr))
	}
	return file, opened, nil
}

func verifyAtomicPublicationProbeEntries(directory *os.File, expected ...string) error {
	if directory == nil {
		return errors.New("descriptor publication probe directory is required")
	}
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open descriptor publication probe directory for bounded enumeration: %w", err)
	}
	listed := os.NewFile(uintptr(fd), "descriptor-publication-probe-directory-enumeration")
	if listed == nil {
		_ = unix.Close(fd)
		return errors.New("descriptor publication probe directory enumeration descriptor is invalid")
	}
	entries, readErr := listed.ReadDir(len(expected) + 1)
	closeErr := listed.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return errors.Join(fmt.Errorf("enumerate descriptor publication probe directory: %w", readErr), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close descriptor publication probe directory enumeration: %w", closeErr)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	wanted := append([]string(nil), expected...)
	sort.Strings(wanted)
	if !reflect.DeepEqual(names, wanted) {
		return fmt.Errorf("private descriptor publication probe directory entries are invalid: got %v", names)
	}
	return nil
}

func pinAncestorChain(path string) ([]pinnedPathComponent, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("lyrics acquisition spool ancestor path is not clean and absolute")
	}
	rootFD, err := unix.Open(string(os.PathSeparator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root for lyrics acquisition ancestry: %w", err)
	}
	rootFile := os.NewFile(uintptr(rootFD), "lyrics-acquisition-ancestor-root")
	rootStat, err := fstatFile(rootFile)
	if err != nil {
		_ = rootFile.Close()
		return nil, err
	}
	if err := validateAncestorStat(rootStat); err != nil {
		_ = rootFile.Close()
		return nil, err
	}
	result := []pinnedPathComponent{{file: rootFile, stat: rootStat}}
	fail := func(cause error) ([]pinnedPathComponent, error) {
		for index := len(result) - 1; index >= 0; index-- {
			_ = result[index].file.Close()
		}
		return nil, cause
	}
	current := rootFile
	trimmed := strings.TrimPrefix(path, string(os.PathSeparator))
	if trimmed == "" {
		return result, nil
	}
	for _, component := range strings.Split(trimmed, string(os.PathSeparator)) {
		if err := validateLeafName(component); err != nil {
			return fail(fmt.Errorf("validate lyrics acquisition spool ancestry component: %w", err))
		}
		file, stat, err := openDirectoryAt(current, component)
		if err != nil {
			return fail(fmt.Errorf("pin lyrics acquisition spool ancestry component %q: %w", component, err))
		}
		if err := validateAncestorStat(stat); err != nil {
			_ = file.Close()
			return fail(err)
		}
		result = append(result, pinnedPathComponent{name: component, file: file, stat: stat})
		current = file
	}
	return result, nil
}
