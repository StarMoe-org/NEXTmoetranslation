package lyricsrootmanifest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	rootFSOpen    = "open"
	rootFSCreate  = "create"
	rootFSWrite   = "write"
	rootFSLink    = "link"
	rootFSVerify  = "verify"
	rootFSCleanup = "cleanup"
	rootFSSync    = "sync"
)

var (
	testHookAfterLink                 func() error
	testHookBeforeFilesystemOperation func(operation, name string) error
)

type rootFileIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	uid    uint32
	links  uint64
	size   int64
}

type rootDirectoryIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	uid    uint32
	links  uint64
}

type rootPinnedDirectory struct {
	name     string
	file     *os.File
	identity rootDirectoryIdentity
	private  bool
}

type rootDirectory struct {
	path      string
	file      *os.File
	ancestors []rootPinnedDirectory
}

type rootDirectoryEntryChange int

const (
	rootDirectoryRefreshLinks rootDirectoryEntryChange = iota
	rootDirectoryEntryCreated
	rootDirectoryEntryRemoved
)

// PublishCreateExclusive atomically links a synced mode-0600 staging inode into
// place without overwrite, syncs the pinned parent directory, and recovers its
// own crash pair. The canonical, symlink-free parent ancestry is opened one
// component at a time with no-follow and retained so every mutation can be
// verified and performed descriptor-relatively. For partial or retry bytes,
// callers must first retain parent-aware proof from DecodeCanonicalAgainstParent
// or ValidateAgainstParent.
func PublishCreateExclusive(path string, body []byte) error {
	if _, err := DecodeCanonical(body); err != nil {
		return err
	}
	_, parentPath, name, err := splitRootOutputPath(path)
	if err != nil {
		return err
	}
	parent, err := openRootDirectory(parentPath)
	if err != nil {
		return err
	}
	defer parent.close()
	if err := parent.lock(); err != nil {
		return err
	}
	defer parent.unlock()

	tempName := "." + name + ".lyrics-root-v1.tmp"
	finalInfo, finalErr := rootStatAt(parent.file, name)
	tempInfo, tempErr := rootStatAt(parent.file, tempName)
	if finalErr == nil {
		if tempErr == nil && sameRootMetadata(finalInfo, tempInfo) {
			if err := verifyRootFileAt(parent, name, body, 2); err != nil {
				return err
			}
			if err := verifyRootFileAt(parent, tempName, body, 2); err != nil {
				return err
			}
			if err := discardRecoverableRootStage(parent, tempName, tempInfo, 2); err != nil {
				return err
			}
			if err := parent.sync(tempName); err != nil {
				return err
			}
			if err := verifyCommittedRootAt(parent, name, tempName, finalInfo); err != nil {
				return err
			}
			if err := verifyRootFileAt(parent, name, body, 1); err != nil {
				return err
			}
			return parent.verifyPath()
		}
		if tempErr != nil && !errors.Is(tempErr, os.ErrNotExist) {
			return tempErr
		}
		return ErrAlreadyPublished
	}
	if finalErr != nil && !errors.Is(finalErr, os.ErrNotExist) {
		return finalErr
	}
	if tempErr == nil {
		if verifyErr := verifyRootFileAt(parent, tempName, body, 1); verifyErr != nil {
			if validateRootRegularIdentity(tempInfo, 1) != nil {
				return verifyErr
			}
			if err := discardRecoverableRootStage(parent, tempName, tempInfo, 1); err != nil {
				return errors.Join(verifyErr, err)
			}
			if err := parent.sync(tempName); err != nil {
				return errors.Join(verifyErr, err)
			}
			tempErr = os.ErrNotExist
		}
	}
	var stagedInfo rootFileIdentity
	if errors.Is(tempErr, os.ErrNotExist) {
		stagedInfo, err = createRootStageAt(parent, tempName, body)
		if err != nil {
			return err
		}
	} else if tempErr != nil {
		return tempErr
	} else {
		stagedInfo = tempInfo
	}
	if err := runRootFSHook(rootFSLink, name); err != nil {
		return err
	}
	if err := parent.rebindPrivateDirectoryLinks(rootDirectoryRefreshLinks); err != nil {
		return err
	}
	if err := verifyRootFileAt(parent, tempName, body, 1); err != nil {
		return err
	}
	currentStage, err := rootStatAt(parent.file, tempName)
	if err != nil || !sameRootMetadata(stagedInfo, currentStage) || validateRootRegularIdentity(currentStage, 1) != nil {
		return errors.Join(errors.New("staged lyrics root inode changed before linking"), err)
	}
	if err := parent.verifyPath(); err != nil {
		return err
	}
	if err := unix.Linkat(int(parent.file.Fd()), tempName, int(parent.file.Fd()), name, 0); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyPublished
		}
		return err
	}
	if err := parent.rebindPrivateDirectoryLinks(rootDirectoryEntryCreated); err != nil {
		return err
	}
	publishedInfo, err := rootStatAt(parent.file, name)
	stagedAfterLink, stagedErr := rootStatAt(parent.file, tempName)
	if err != nil || stagedErr != nil || !sameRootContentIdentity(stagedInfo, publishedInfo) ||
		!sameRootMetadata(publishedInfo, stagedAfterLink) || validateRootRegularIdentity(publishedInfo, 2) != nil {
		return errors.Join(errors.New("lyrics root publication did not bind the staged inode exactly"), err, stagedErr)
	}
	if err := parent.sync(name); err != nil {
		return err
	}
	if err := verifyRootCrashPairAt(parent, name, tempName, publishedInfo); err != nil {
		return err
	}
	if testHookAfterLink != nil {
		if err := testHookAfterLink(); err != nil {
			return err
		}
	}
	if err := discardRecoverableRootStage(parent, tempName, stagedAfterLink, 2); err != nil {
		return err
	}
	if err := parent.sync(tempName); err != nil {
		return err
	}
	if err := verifyCommittedRootAt(parent, name, tempName, publishedInfo); err != nil {
		return err
	}
	if err := verifyRootFileAt(parent, name, body, 1); err != nil {
		return err
	}
	return parent.verifyPath()
}

func verifyRootCrashPairAt(parent *rootDirectory, finalName, tempName string, expected rootFileIdentity) error {
	if err := parent.verifyPath(); err != nil {
		return err
	}
	finalInfo, finalErr := rootStatAt(parent.file, finalName)
	tempInfo, tempErr := rootStatAt(parent.file, tempName)
	if finalErr != nil || tempErr != nil || !sameRootMetadata(expected, finalInfo) ||
		!sameRootMetadata(expected, tempInfo) || validateRootRegularIdentity(finalInfo, 2) != nil {
		return errors.Join(errors.New("lyrics root crash pair changed before commit"), finalErr, tempErr)
	}
	return nil
}

func verifyCommittedRootAt(parent *rootDirectory, finalName, tempName string, expected rootFileIdentity) error {
	if err := parent.verifyPath(); err != nil {
		return err
	}
	finalInfo, finalErr := rootStatAt(parent.file, finalName)
	_, tempErr := rootStatAt(parent.file, tempName)
	if finalErr != nil || !errors.Is(tempErr, os.ErrNotExist) || !sameRootContentIdentity(expected, finalInfo) ||
		validateRootRegularIdentity(finalInfo, 1) != nil {
		return errors.Join(errors.New("committed lyrics root identity is invalid"), finalErr, tempErr)
	}
	return nil
}

func createRootStageAt(parent *rootDirectory, name string, body []byte) (rootFileIdentity, error) {
	if err := runRootFSHook(rootFSCreate, name); err != nil {
		return rootFileIdentity{}, err
	}
	if err := parent.verifyPath(); err != nil {
		return rootFileIdentity{}, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return rootFileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return rootFileIdentity{}, errors.New("staged lyrics root descriptor is invalid")
	}
	created := true
	defer func() {
		if created {
			_ = file.Close()
		}
	}()
	if err := parent.rebindPrivateDirectoryLinks(rootDirectoryEntryCreated); err != nil {
		return rootFileIdentity{}, err
	}
	if err := file.Chmod(0o600); err != nil {
		return rootFileIdentity{}, err
	}
	opened, err := rootIdentityForFile(file)
	named, namedErr := rootStatAt(parent.file, name)
	if err != nil || namedErr != nil || !sameRootMetadata(opened, named) ||
		validateRootRegularIdentity(opened, 1) != nil || opened.size != 0 {
		return rootFileIdentity{}, errors.Join(errors.New("staged lyrics root file identity is invalid before writing"), err, namedErr)
	}
	if err := runRootFSHook(rootFSWrite, name); err != nil {
		return rootFileIdentity{}, err
	}
	if err := parent.verifyPath(); err != nil {
		return rootFileIdentity{}, err
	}
	writeReady, err := rootIdentityForFile(file)
	writeName, namedErr := rootStatAt(parent.file, name)
	if err != nil || namedErr != nil || !sameRootMetadata(opened, writeReady) || !sameRootMetadata(opened, writeName) ||
		validateRootRegularIdentity(writeReady, 1) != nil || writeReady.size != 0 {
		return rootFileIdentity{}, errors.Join(errors.New("staged lyrics root changed before writing"), err, namedErr)
	}
	if err := writeRootBytes(file, body); err != nil {
		return rootFileIdentity{}, err
	}
	if err := file.Sync(); err != nil {
		return rootFileIdentity{}, err
	}
	if err := parent.verifyPath(); err != nil {
		return rootFileIdentity{}, err
	}
	synced, err := rootIdentityForFile(file)
	syncedName, namedErr := rootStatAt(parent.file, name)
	if err != nil || namedErr != nil || !sameRootStableIdentity(opened, synced) || !sameRootMetadata(synced, syncedName) ||
		validateRootRegularIdentity(synced, 1) != nil || synced.size != int64(len(body)) {
		return rootFileIdentity{}, errors.Join(errors.New("staged lyrics root file identity is invalid after writing"), err, namedErr)
	}
	if err := file.Close(); err != nil {
		return rootFileIdentity{}, err
	}
	created = false
	if err := runRootFSHook(rootFSVerify, name); err != nil {
		return rootFileIdentity{}, err
	}
	if err := parent.verifyPath(); err != nil {
		return rootFileIdentity{}, err
	}
	named, err = rootStatAt(parent.file, name)
	if err != nil || !sameRootMetadata(synced, named) {
		return rootFileIdentity{}, errors.Join(errors.New("staged lyrics root filename changed after creation"), err)
	}
	if err := parent.sync(name); err != nil {
		return rootFileIdentity{}, err
	}
	committed, err := rootStatAt(parent.file, name)
	if err != nil || !sameRootMetadata(synced, committed) || validateRootRegularIdentity(committed, 1) != nil {
		return rootFileIdentity{}, errors.Join(errors.New("staged lyrics root inode changed before commit"), err)
	}
	return synced, nil
}

func writeRootBytes(file *os.File, body []byte) error {
	for len(body) > 0 {
		written, err := file.Write(body)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}

func discardRecoverableRootStage(parent *rootDirectory, name string, expected rootFileIdentity, links uint64) error {
	if err := validateRootRegularIdentity(expected, links); err != nil {
		return err
	}
	if err := runRootFSHook(rootFSCleanup, name); err != nil {
		return err
	}
	if err := parent.verifyPath(); err != nil {
		return err
	}
	current, err := rootStatAt(parent.file, name)
	if err != nil || !sameRootMetadata(expected, current) || validateRootRegularIdentity(current, links) != nil {
		return errors.Join(errors.New("staged lyrics root changed before crash recovery"), err)
	}
	if err := unix.Unlinkat(int(parent.file.Fd()), name, 0); err != nil {
		return fmt.Errorf("remove incomplete staged lyrics root: %w", err)
	}
	if err := parent.rebindPrivateDirectoryLinks(rootDirectoryEntryRemoved); err != nil {
		return err
	}
	return nil
}

func verifyRootFileAt(parent *rootDirectory, name string, expected []byte, links uint64) error {
	if err := parent.verifyPath(); err != nil {
		return err
	}
	before, err := rootStatAt(parent.file, name)
	if err != nil || validateRootRegularIdentity(before, links) != nil || before.size != int64(len(expected)) {
		return errors.New("lyrics root file identity is invalid")
	}
	if err := runRootFSHook(rootFSVerify, name); err != nil {
		return err
	}
	if err := parent.verifyPath(); err != nil {
		return err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("lyrics root file descriptor is invalid")
	}
	defer file.Close()
	opened, err := rootIdentityForFile(file)
	afterOpen, pathErr := rootStatAt(parent.file, name)
	if err != nil || pathErr != nil || !sameRootMetadata(before, opened) || !sameRootMetadata(before, afterOpen) {
		return errors.New("lyrics root file changed while being opened")
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(MaxManifestBytes)+1))
	if err != nil {
		return err
	}
	afterRead, statErr := rootIdentityForFile(file)
	afterReadPath, pathStatErr := rootStatAt(parent.file, name)
	if statErr != nil || pathStatErr != nil || !sameRootMetadata(before, afterRead) ||
		!sameRootMetadata(before, afterReadPath) || afterRead.size != int64(len(body)) ||
		!bytes.Equal(body, expected) || validateRootRegularIdentity(afterRead, links) != nil {
		return errors.New("lyrics root file bytes changed")
	}
	if sha256.Sum256(body) != sha256.Sum256(expected) {
		return errors.New("lyrics root file digest changed")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	afterSync, syncErr := rootIdentityForFile(file)
	afterSyncPath, syncPathErr := rootStatAt(parent.file, name)
	if syncErr != nil || syncPathErr != nil || !sameRootMetadata(before, afterSync) ||
		!sameRootMetadata(before, afterSyncPath) || validateRootRegularIdentity(afterSync, links) != nil {
		return errors.New("lyrics root file changed while being synced")
	}
	return parent.verifyPath()
}

func splitRootOutputPath(path string) (absolute, parent, name string, err error) {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", "", errors.New("lyrics root output path must be absolute and lexically canonical")
	}
	absolute, err = filepath.Abs(path)
	if err != nil || absolute != path {
		return "", "", "", errors.New("lyrics root output path must be absolute and lexically canonical")
	}
	name = filepath.Base(path)
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return "", "", "", errors.New("lyrics root output filename is invalid")
	}
	parent = filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", "", err
	}
	if resolved != parent {
		return "", "", "", errors.New("lyrics root output parent must not be a symlink or resolved alias")
	}
	return absolute, parent, name, nil
}

func openRootDirectory(path string) (*rootDirectory, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("lyrics root output parent must be absolute and lexically canonical")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, errors.New("lyrics root output parent must not be a symlink or resolved alias")
	}

	volume := filepath.VolumeName(path)
	rootPath := volume + string(os.PathSeparator)
	rootFD, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	rootFile := os.NewFile(uintptr(rootFD), rootPath)
	if rootFile == nil {
		_ = unix.Close(rootFD)
		return nil, errors.New("lyrics root ancestry descriptor is invalid")
	}
	rootIdentity, err := rootDirectoryIdentityForFile(rootFile)
	if err != nil || validateRootAncestorIdentity(rootIdentity) != nil {
		rootFile.Close()
		return nil, errors.Join(errors.New("lyrics root filesystem root is not trusted"), err)
	}
	directory := &rootDirectory{
		path: path,
		file: rootFile,
		ancestors: []rootPinnedDirectory{{
			file: rootFile, identity: rootIdentity,
		}},
	}

	relative := strings.TrimPrefix(path, rootPath)
	components := strings.Split(relative, string(os.PathSeparator))
	currentPath := rootPath
	for index, component := range components {
		if component == "" {
			continue
		}
		currentPath = filepath.Join(currentPath, component)
		private := index == len(components)-1
		validate := validateRootAncestorIdentity
		if private {
			validate = validateRootPrivateDirectoryIdentity
		}
		if err := directory.verifyPath(); err != nil {
			directory.close()
			return nil, err
		}
		parent := directory.file
		before, beforeErr := rootDirectoryStatAt(parent, component)
		if beforeErr != nil || validate(before) != nil {
			directory.close()
			if private {
				return nil, errors.Join(errors.New("lyrics root output parent must be a direct effective-UID-owned mode-0700 directory"), beforeErr)
			}
			return nil, errors.Join(errors.New("lyrics root output ancestry is not trusted"), beforeErr)
		}
		if err := runRootFSHook(rootFSOpen, currentPath); err != nil {
			directory.close()
			return nil, err
		}
		if err := directory.verifyPath(); err != nil {
			directory.close()
			return nil, err
		}
		fd, err := unix.Openat(int(parent.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			directory.close()
			return nil, fmt.Errorf("open lyrics root output ancestry %q: %w", currentPath, err)
		}
		file := os.NewFile(uintptr(fd), currentPath)
		if file == nil {
			_ = unix.Close(fd)
			directory.close()
			return nil, errors.New("lyrics root output ancestry descriptor is invalid")
		}
		opened, openErr := rootDirectoryIdentityForFile(file)
		named, namedErr := rootDirectoryStatAt(parent, component)
		bindingStable := sameRootDirectoryStableIdentity(before, opened) &&
			sameRootDirectoryStableIdentity(before, named)
		if openErr != nil || namedErr != nil || !bindingStable || validate(opened) != nil {
			file.Close()
			directory.close()
			if private {
				return nil, errors.Join(errors.New("lyrics root output parent changed while being opened"), openErr, namedErr)
			}
			return nil, errors.Join(errors.New("lyrics root output ancestry changed while being opened"), openErr, namedErr)
		}
		directory.ancestors = append(directory.ancestors, rootPinnedDirectory{
			name: component, file: file, identity: opened, private: private,
		})
		directory.file = file
	}
	if len(directory.ancestors) == 1 || !directory.ancestors[len(directory.ancestors)-1].private {
		directory.close()
		return nil, errors.New("lyrics root output parent must be a direct effective-UID-owned mode-0700 directory")
	}
	if err := directory.verifyStablePath(); err != nil {
		directory.close()
		return nil, err
	}
	return directory, nil
}

func (directory *rootDirectory) lock() error {
	if err := directory.verifyStablePath(); err != nil {
		return err
	}
	if err := syscall.Flock(int(directory.file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock lyrics root output parent: %w", err)
	}
	if err := directory.rebindPrivateDirectoryLinks(rootDirectoryRefreshLinks); err != nil {
		_ = syscall.Flock(int(directory.file.Fd()), syscall.LOCK_UN)
		return err
	}
	return nil
}

func (directory *rootDirectory) unlock() {
	if directory != nil && directory.file != nil {
		_ = syscall.Flock(int(directory.file.Fd()), syscall.LOCK_UN)
	}
}

func (directory *rootDirectory) sync(name string) error {
	if err := runRootFSHook(rootFSSync, name); err != nil {
		return err
	}
	if err := directory.verifyPath(); err != nil {
		return err
	}
	if err := directory.file.Sync(); err != nil {
		return err
	}
	return directory.verifyPath()
}

func (directory *rootDirectory) rebindPrivateDirectoryLinks(change rootDirectoryEntryChange) error {
	if directory == nil || directory.file == nil || len(directory.ancestors) < 2 {
		return errors.New("lyrics root output parent is not open")
	}
	last := len(directory.ancestors) - 1
	for index := 0; index < last; index++ {
		current := &directory.ancestors[index]
		opened, err := rootDirectoryIdentityForFile(current.file)
		if err != nil || !sameRootDirectoryStableIdentity(current.identity, opened) || validateRootAncestorIdentity(opened) != nil {
			return errors.New("lyrics root output ancestry descriptor changed")
		}
		if index == 0 {
			continue
		}
		parent := directory.ancestors[index-1].file
		named, err := rootDirectoryStatAt(parent, current.name)
		if err != nil || !sameRootDirectoryStableIdentity(current.identity, named) || validateRootAncestorIdentity(named) != nil {
			return errors.New("lyrics root output ancestry path changed")
		}
	}
	parentComponent := &directory.ancestors[last]
	opened, openErr := rootDirectoryIdentityForFile(parentComponent.file)
	named, namedErr := rootDirectoryStatAt(directory.ancestors[last-1].file, parentComponent.name)
	if openErr != nil || namedErr != nil || !sameRootDirectoryStableIdentity(parentComponent.identity, opened) ||
		!sameRootDirectoryMetadata(opened, named) || validateRootPrivateDirectoryIdentity(opened) != nil {
		return errors.Join(errors.New("lyrics root output parent identity changed"), openErr, namedErr)
	}
	oldLinks := parentComponent.identity.links
	newLinks := opened.links
	validLinks := false
	switch change {
	case rootDirectoryRefreshLinks:
		validLinks = true
	case rootDirectoryEntryCreated:
		validLinks = newLinks == oldLinks || newLinks == oldLinks+1
	case rootDirectoryEntryRemoved:
		validLinks = newLinks == oldLinks || oldLinks == newLinks+1
	}
	if !validLinks {
		return errors.New("lyrics root output parent link count changed unexpectedly")
	}
	parentComponent.identity = opened
	return nil
}

func (directory *rootDirectory) verifyDescriptor() error {
	return directory.verifyPath()
}

func (directory *rootDirectory) verifyStablePath() error {
	if directory == nil || directory.file == nil || len(directory.ancestors) == 0 {
		return errors.New("lyrics root output parent is not open")
	}
	for index := range directory.ancestors {
		current := &directory.ancestors[index]
		opened, err := rootDirectoryIdentityForFile(current.file)
		validate := validateRootAncestorIdentity
		if current.private {
			validate = validateRootPrivateDirectoryIdentity
		}
		if err != nil || !sameRootDirectoryStableIdentity(current.identity, opened) || validate(opened) != nil {
			return errors.New("lyrics root output ancestry descriptor changed")
		}
		if index == 0 {
			continue
		}
		parent := directory.ancestors[index-1].file
		named, err := rootDirectoryStatAt(parent, current.name)
		if err != nil || !sameRootDirectoryStableIdentity(current.identity, named) || validate(named) != nil {
			return errors.New("lyrics root output ancestry path changed")
		}
	}
	return nil
}

func (directory *rootDirectory) verifyPath() error {
	if directory == nil || directory.file == nil || len(directory.ancestors) == 0 {
		return errors.New("lyrics root output parent is not open")
	}
	for index := range directory.ancestors {
		current := &directory.ancestors[index]
		opened, err := rootDirectoryIdentityForFile(current.file)
		validate := validateRootAncestorIdentity
		identityMatches := sameRootDirectoryStableIdentity
		if current.private {
			validate = validateRootPrivateDirectoryIdentity
			identityMatches = sameRootDirectoryMetadata
		}
		if err != nil || !identityMatches(current.identity, opened) || validate(opened) != nil {
			return errors.New("lyrics root output ancestry descriptor changed")
		}
		if index == 0 {
			continue
		}
		parent := directory.ancestors[index-1].file
		named, err := rootDirectoryStatAt(parent, current.name)
		if err != nil || !identityMatches(current.identity, named) || validate(named) != nil {
			return errors.New("lyrics root output ancestry path changed")
		}
	}
	return nil
}

func (directory *rootDirectory) close() {
	if directory == nil {
		return
	}
	for index := len(directory.ancestors) - 1; index >= 0; index-- {
		if directory.ancestors[index].file != nil {
			_ = directory.ancestors[index].file.Close()
			directory.ancestors[index].file = nil
		}
	}
	directory.file = nil
}

func runRootFSHook(operation, name string) error {
	if testHookBeforeFilesystemOperation != nil {
		return testHookBeforeFilesystemOperation(operation, name)
	}
	return nil
}

func rootStatAt(directory *os.File, name string) (rootFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return rootFileIdentity{}, err
	}
	return rootIdentityFromStat(&stat), nil
}

func rootIdentityForFile(file *os.File) (rootFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return rootFileIdentity{}, err
	}
	return rootIdentityFromStat(&stat), nil
}

func rootIdentityFromStat(stat *unix.Stat_t) rootFileIdentity {
	return rootFileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), mode: uint32(stat.Mode), uid: stat.Uid,
		links: uint64(stat.Nlink), size: stat.Size,
	}
}

func rootDirectoryStatAt(directory *os.File, name string) (rootDirectoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return rootDirectoryIdentity{}, err
	}
	return rootDirectoryIdentityFromStat(&stat), nil
}

func rootDirectoryIdentityForFile(file *os.File) (rootDirectoryIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return rootDirectoryIdentity{}, err
	}
	return rootDirectoryIdentityFromStat(&stat), nil
}

func rootDirectoryIdentityFromStat(stat *unix.Stat_t) rootDirectoryIdentity {
	return rootDirectoryIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), mode: uint32(stat.Mode), uid: stat.Uid,
		links: uint64(stat.Nlink),
	}
}

func sameRootIdentity(left, right rootFileIdentity) bool {
	return left.device == right.device && left.inode == right.inode
}

func sameRootMetadata(left, right rootFileIdentity) bool {
	return left == right
}

func sameRootStableIdentity(left, right rootFileIdentity) bool {
	return sameRootIdentity(left, right) && left.mode == right.mode && left.uid == right.uid
}

func sameRootContentIdentity(left, right rootFileIdentity) bool {
	return sameRootStableIdentity(left, right) && left.size == right.size
}

func sameRootDirectoryStableIdentity(left, right rootDirectoryIdentity) bool {
	return left.device == right.device && left.inode == right.inode && left.mode == right.mode && left.uid == right.uid
}

func sameRootDirectoryMetadata(left, right rootDirectoryIdentity) bool {
	return left == right
}

func validateRootRegularIdentity(info rootFileIdentity, links uint64) error {
	if info.mode&unix.S_IFMT != unix.S_IFREG || info.mode&0o7777 != 0o600 || int(info.uid) != os.Geteuid() || info.links != links {
		return fmt.Errorf("lyrics root file must be a direct effective-UID-owned mode-0600 regular file with %d links", links)
	}
	return nil
}

func validateRootAncestorIdentity(info rootDirectoryIdentity) error {
	if info.mode&unix.S_IFMT != unix.S_IFDIR || info.links == 0 || int(info.uid) != 0 && int(info.uid) != os.Geteuid() {
		return errors.New("lyrics root output ancestry is not owned by root or the effective UID")
	}
	if info.mode&0o022 != 0 && info.mode&0o1000 == 0 {
		return errors.New("lyrics root output ancestry is writable by an untrusted local UID")
	}
	return nil
}

func validateRootPrivateDirectoryIdentity(info rootDirectoryIdentity) error {
	if info.mode&unix.S_IFMT != unix.S_IFDIR || info.mode&0o7777 != 0o700 || int(info.uid) != os.Geteuid() || info.links == 0 {
		return errors.New("lyrics root output parent must be a direct effective-UID-owned mode-0700 directory")
	}
	return nil
}

func validateRootAncestorChain(path string) error {
	directory, err := openRootDirectory(path)
	if err != nil {
		return err
	}
	directory.close()
	return nil
}

func validatePrivateDirectory(info os.FileInfo) error {
	owner, ok := rootFileOwner(info)
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 ||
		!ok || int(owner) != os.Geteuid() {
		return errors.New("lyrics root output parent must be a direct effective-UID-owned mode-0700 directory")
	}
	return nil
}

func validatePrivateRegular(info os.FileInfo, links uint64) error {
	owner, ok := rootFileOwner(info)
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
		!ok || int(owner) != os.Geteuid() || rootFileLinkCount(info) != links {
		return fmt.Errorf("lyrics root file must be a direct effective-UID-owned mode-0600 regular file with %d links", links)
	}
	return nil
}

func rootFileOwner(info os.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func rootFileLinkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
