package lyricsevidencepack

import (
	"crypto/sha256"
	"encoding/hex"
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
	evidenceFSCreate  = "create"
	evidenceFSLink    = "link"
	evidenceFSVerify  = "verify"
	evidenceFSCleanup = "cleanup"
	evidenceFSSync    = "sync"
	evidenceFSList    = "list"
)

var (
	testHookAfterLink                 func(string) error
	testHookBeforeFilesystemOperation func(operation, name string) error
)

type evidenceFileIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	uid    uint32
	links  uint64
	size   int64
}

type pinnedEvidenceDirectoryComponent struct {
	name     string
	file     *os.File
	identity evidenceFileIdentity
}

type pinnedEvidenceDirectoryPath struct {
	components []pinnedEvidenceDirectoryComponent
}

type privateDirectory struct {
	path     string
	name     string
	file     *os.File
	identity evidenceFileIdentity
	parent   *evidenceParentDirectory
}

type evidenceParentDirectory struct {
	file     *os.File
	identity evidenceFileIdentity
	chain    *pinnedEvidenceDirectoryPath
}

func openPrivateDirectory(path string, create bool) (*privateDirectory, error) {
	absolute, parentPath, name, err := splitPrivateEvidenceDirectoryPath(path)
	if err != nil {
		return nil, err
	}
	parent, err := openEvidenceParent(parentPath)
	if err != nil {
		return nil, err
	}
	locked := false
	succeeded := false
	var directoryFile *os.File
	created := false
	createdIdentity := evidenceFileIdentity{}
	defer func() {
		if succeeded {
			return
		}
		if directoryFile != nil {
			_ = directoryFile.Close()
		}
		if created {
			_ = removeEvidenceDirectoryAt(parent, name, createdIdentity)
		}
		if locked {
			parent.unlock()
		}
		parent.close()
	}()
	if err := parent.lock(); err != nil {
		return nil, err
	}
	locked = true

	before, statErr := evidenceStatAt(parent.file, name)
	if errors.Is(statErr, os.ErrNotExist) {
		if !create {
			return nil, os.ErrNotExist
		}
		if err := parent.beforeOperation(evidenceFSCreate, name); err != nil {
			return nil, err
		}
		if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil, errors.New("private evidence pack directory appeared concurrently")
			}
			return nil, fmt.Errorf("create private evidence pack directory: %w", err)
		}
		created = true
		before, statErr = evidenceStatAt(parent.file, name)
		if statErr != nil {
			return nil, statErr
		}
		createdIdentity = before
		if err := validateEvidenceCreatedDirectoryIdentity(before, "private evidence pack directory"); err != nil {
			return nil, err
		}
	}
	if statErr != nil {
		return nil, statErr
	}
	if !created {
		if err := validateEvidenceDirectoryIdentity(before, "private evidence pack directory"); err != nil {
			return nil, err
		}
	}
	if err := parent.beforeOperation(evidenceFSVerify, name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	directoryFile = os.NewFile(uintptr(fd), absolute)
	if directoryFile == nil {
		_ = unix.Close(fd)
		return nil, errors.New("private evidence pack directory descriptor is invalid")
	}
	openedIdentity, identityErr := evidenceIdentityForFile(directoryFile)
	pathAfterOpen, pathErr := evidenceStatAt(parent.file, name)
	if identityErr != nil || pathErr != nil || !sameEvidenceIdentity(before, openedIdentity) ||
		!sameEvidenceIdentity(before, pathAfterOpen) {
		return nil, errors.New("private evidence pack directory changed while being opened")
	}
	if created {
		if err := directoryFile.Chmod(0o700); err != nil {
			return nil, err
		}
		openedIdentity, identityErr = evidenceIdentityForFile(directoryFile)
		pathAfterOpen, pathErr = evidenceStatAt(parent.file, name)
		if identityErr != nil || pathErr != nil || !sameEvidenceIdentity(before, openedIdentity) ||
			!sameEvidenceMetadata(openedIdentity, pathAfterOpen) ||
			validateEvidenceDirectoryIdentity(openedIdentity, "private evidence pack directory") != nil {
			return nil, errors.New("private evidence pack directory changed while securing its mode")
		}
		createdIdentity = openedIdentity
		if err := parent.sync(name); err != nil {
			return nil, fmt.Errorf("sync private evidence pack directory creation: %w", err)
		}
	} else if !sameEvidenceDirectoryBinding(before, openedIdentity) ||
		!sameEvidenceDirectoryBinding(before, pathAfterOpen) ||
		validateEvidenceDirectoryIdentity(openedIdentity, "private evidence pack directory") != nil {
		return nil, errors.New("private evidence pack directory changed while being opened")
	}
	if err := parent.verifyPath(); err != nil {
		return nil, err
	}
	current, err := evidenceStatAt(parent.file, name)
	if err != nil || !sameEvidenceDirectoryBinding(openedIdentity, current) ||
		validateEvidenceDirectoryIdentity(current, "private evidence pack directory") != nil {
		return nil, errors.New("private evidence pack directory path changed while being opened")
	}

	parent.unlock()
	locked = false
	directory := &privateDirectory{
		path: absolute, name: name, file: directoryFile, identity: openedIdentity, parent: parent,
	}
	directoryFile = nil
	succeeded = true
	return directory, nil
}

func splitPrivateEvidenceDirectoryPath(path string) (absolute, parent, name string, err error) {
	if path == "" || strings.TrimSpace(path) != path {
		return "", "", "", errors.New("private evidence pack directory path is invalid")
	}
	absolute, err = filepath.Abs(path)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve private evidence pack directory: %w", err)
	}
	name = filepath.Base(absolute)
	if err := validEvidenceName(name); err != nil {
		return "", "", "", err
	}
	parent = filepath.Dir(absolute)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve private evidence pack parent: %w", err)
	}
	resolvedParent, err = filepath.Abs(resolvedParent)
	if err != nil {
		return "", "", "", err
	}
	if resolvedParent != parent {
		return "", "", "", errors.New("private evidence pack lexical parent must equal its resolved parent")
	}
	return absolute, parent, name, nil
}

func openEvidenceParent(path string) (*evidenceParentDirectory, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("private evidence pack parent path is invalid")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, errors.New("private evidence pack lexical parent must equal its resolved parent")
	}
	chain, err := openPinnedEvidenceDirectoryPath(path)
	if err != nil {
		return nil, err
	}
	final := chain.components[len(chain.components)-1]
	return &evidenceParentDirectory{file: final.file, identity: final.identity, chain: chain}, nil
}

func openPinnedEvidenceDirectoryPath(path string) (_ *pinnedEvidenceDirectoryPath, result error) {
	root, names, err := splitAbsoluteEvidencePath(path)
	if err != nil {
		return nil, err
	}
	chain := &pinnedEvidenceDirectoryPath{components: make([]pinnedEvidenceDirectoryComponent, 0, len(names)+1)}
	defer func() {
		if result != nil {
			chain.close()
		}
	}()
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	rootFile := os.NewFile(uintptr(rootFD), root)
	if rootFile == nil {
		_ = unix.Close(rootFD)
		return nil, errors.New("private evidence pack ancestry root descriptor is invalid")
	}
	rootIdentity, err := evidenceIdentityForFile(rootFile)
	if err != nil || validateEvidenceAncestorIdentity(rootIdentity) != nil {
		_ = rootFile.Close()
		return nil, errors.New("private evidence pack ancestry root is invalid")
	}
	chain.components = append(chain.components, pinnedEvidenceDirectoryComponent{file: rootFile, identity: rootIdentity})

	for index, name := range names {
		parent := chain.components[len(chain.components)-1].file
		before, err := evidenceStatAt(parent, name)
		if err != nil {
			return nil, fmt.Errorf("inspect private evidence pack ancestry: %w", err)
		}
		final := index == len(names)-1
		if err := validatePinnedEvidenceDirectoryIdentity(before, final); err != nil {
			return nil, err
		}
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), filepath.Join(root, filepath.Join(names[:index+1]...)))
		if file == nil {
			_ = unix.Close(fd)
			return nil, errors.New("private evidence pack ancestry descriptor is invalid")
		}
		opened, openErr := evidenceIdentityForFile(file)
		after, pathErr := evidenceStatAt(parent, name)
		if openErr != nil || pathErr != nil || !sameEvidenceDirectoryBinding(before, opened) ||
			!sameEvidenceDirectoryBinding(before, after) {
			_ = file.Close()
			return nil, errors.New("private evidence pack ancestry changed while being opened")
		}
		chain.components = append(chain.components, pinnedEvidenceDirectoryComponent{name: name, file: file, identity: opened})
	}
	if len(names) == 0 {
		if err := validateEvidenceParentIdentity(rootIdentity); err != nil {
			return nil, err
		}
	}
	if err := chain.verify(); err != nil {
		return nil, err
	}
	return chain, nil
}

func splitAbsoluteEvidencePath(path string) (string, []string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", nil, errors.New("private evidence pack ancestry path is invalid")
	}
	volume := filepath.VolumeName(path)
	root := volume + string(os.PathSeparator)
	relative := strings.TrimPrefix(path, root)
	if relative == path {
		return "", nil, errors.New("private evidence pack ancestry path is invalid")
	}
	if relative == "" {
		return root, nil, nil
	}
	names := strings.Split(relative, string(os.PathSeparator))
	for _, name := range names {
		if err := validEvidenceName(name); err != nil {
			return "", nil, errors.New("private evidence pack ancestry component is invalid")
		}
	}
	return root, names, nil
}

func validatePinnedEvidenceDirectoryIdentity(identity evidenceFileIdentity, final bool) error {
	if final {
		return validateEvidenceParentIdentity(identity)
	}
	return validateEvidenceAncestorIdentity(identity)
}

func validateEvidenceAncestorIdentity(identity evidenceFileIdentity) error {
	if identity.mode&unix.S_IFMT != unix.S_IFDIR || identity.uid != 0 && int(identity.uid) != os.Geteuid() {
		return errors.New("private evidence pack ancestry is not owned by root or the effective UID")
	}
	if identity.mode&0o022 != 0 && identity.mode&unix.S_ISVTX == 0 {
		return errors.New("private evidence pack ancestry is writable by an untrusted local UID")
	}
	return nil
}

func validateEvidenceParentIdentity(identity evidenceFileIdentity) error {
	if identity.mode&unix.S_IFMT != unix.S_IFDIR || identity.mode&0o022 != 0 || int(identity.uid) != os.Geteuid() {
		return errors.New("private evidence pack parent must be stable, effective-UID-owned, and not group/other-writable")
	}
	return nil
}

func (chain *pinnedEvidenceDirectoryPath) verify() error {
	if chain == nil || len(chain.components) == 0 {
		return errors.New("private evidence pack ancestry is not open")
	}
	for index := range chain.components {
		component := &chain.components[index]
		if component.file == nil {
			return errors.New("private evidence pack ancestry descriptor is closed")
		}
		opened, err := evidenceIdentityForFile(component.file)
		final := index == len(chain.components)-1
		if err != nil || !sameEvidenceDirectoryBinding(component.identity, opened) ||
			validatePinnedEvidenceDirectoryIdentity(opened, final) != nil {
			return errors.New("private evidence pack ancestry inode changed")
		}
		if index == 0 {
			continue
		}
		parent := chain.components[index-1].file
		named, err := evidenceStatAt(parent, component.name)
		if err != nil || !sameEvidenceDirectoryBinding(component.identity, named) ||
			validatePinnedEvidenceDirectoryIdentity(named, final) != nil {
			return errors.New("private evidence pack ancestry path changed")
		}
	}
	return nil
}

func (chain *pinnedEvidenceDirectoryPath) close() {
	if chain == nil {
		return
	}
	for index := len(chain.components) - 1; index >= 0; index-- {
		if chain.components[index].file != nil {
			_ = chain.components[index].file.Close()
			chain.components[index].file = nil
		}
	}
	chain.components = nil
}

func (parent *evidenceParentDirectory) lock() error {
	if err := parent.verifyPath(); err != nil {
		return err
	}
	if err := syscall.Flock(int(parent.file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock private evidence pack parent: %w", err)
	}
	if err := parent.verifyPath(); err != nil {
		_ = syscall.Flock(int(parent.file.Fd()), syscall.LOCK_UN)
		return err
	}
	return nil
}

func (parent *evidenceParentDirectory) unlock() {
	if parent != nil && parent.file != nil {
		_ = syscall.Flock(int(parent.file.Fd()), syscall.LOCK_UN)
	}
}

func (parent *evidenceParentDirectory) beforeOperation(operation, name string) error {
	if err := parent.verifyPath(); err != nil {
		return err
	}
	if err := runEvidenceFSHook(operation, name); err != nil {
		return err
	}
	return parent.verifyPath()
}

func (parent *evidenceParentDirectory) sync(name string) error {
	if err := parent.beforeOperation(evidenceFSSync, name); err != nil {
		return err
	}
	if err := parent.file.Sync(); err != nil {
		return err
	}
	return parent.verifyPath()
}

func (parent *evidenceParentDirectory) verifyDescriptor() error {
	if parent == nil || parent.file == nil || parent.chain == nil {
		return errors.New("private evidence pack parent is not open")
	}
	opened, err := evidenceIdentityForFile(parent.file)
	if err != nil || !sameEvidenceDirectoryBinding(parent.identity, opened) || validateEvidenceParentIdentity(opened) != nil {
		return errors.New("private evidence pack parent inode changed")
	}
	return nil
}

func (parent *evidenceParentDirectory) verifyPath() error {
	if err := parent.verifyDescriptor(); err != nil {
		return err
	}
	if err := parent.chain.verify(); err != nil {
		return err
	}
	return nil
}

func (parent *evidenceParentDirectory) close() {
	if parent == nil {
		return
	}
	if parent.chain != nil {
		parent.chain.close()
	}
	parent.file = nil
	parent.chain = nil
}

func removeEvidenceDirectoryAt(parent *evidenceParentDirectory, name string, expected evidenceFileIdentity) error {
	if err := parent.beforeOperation(evidenceFSCleanup, name); err != nil {
		return err
	}
	current, err := evidenceStatAt(parent.file, name)
	if err != nil || !sameEvidenceMetadata(expected, current) ||
		validateEvidenceDirectoryIdentity(current, "private evidence pack directory") != nil {
		return errors.New("private evidence pack directory changed before cleanup")
	}
	if err := unix.Unlinkat(int(parent.file.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.sync(name)
}

func (directory *privateDirectory) verify() error {
	if directory == nil || directory.file == nil || directory.parent == nil {
		return errors.New("private evidence pack directory is not open")
	}
	opened, err := evidenceIdentityForFile(directory.file)
	if err != nil || !sameEvidenceDirectoryBinding(directory.identity, opened) ||
		validateEvidenceDirectoryIdentity(opened, "private evidence pack directory") != nil {
		return errors.New("private evidence pack directory inode changed")
	}
	return nil
}

func (directory *privateDirectory) verifyPath() error {
	if err := directory.verify(); err != nil {
		return err
	}
	if err := directory.parent.verifyPath(); err != nil {
		return err
	}
	current, err := evidenceStatAt(directory.parent.file, directory.name)
	if err != nil || !sameEvidenceDirectoryBinding(directory.identity, current) ||
		validateEvidenceDirectoryIdentity(current, "private evidence pack directory") != nil {
		return errors.New("private evidence pack directory path changed")
	}
	return nil
}

func (directory *privateDirectory) lock() error {
	if err := directory.verifyPath(); err != nil {
		return err
	}
	if err := syscall.Flock(int(directory.file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock private evidence pack directory: %w", err)
	}
	if err := directory.verifyPath(); err != nil {
		_ = syscall.Flock(int(directory.file.Fd()), syscall.LOCK_UN)
		return err
	}
	return nil
}

func (directory *privateDirectory) unlock() {
	if directory != nil && directory.file != nil {
		_ = syscall.Flock(int(directory.file.Fd()), syscall.LOCK_UN)
	}
}

func (directory *privateDirectory) beforeOperation(operation, name string) error {
	if err := directory.verifyPath(); err != nil {
		return err
	}
	if err := runEvidenceFSHook(operation, name); err != nil {
		return err
	}
	return directory.verifyPath()
}

func (directory *privateDirectory) sync() error {
	if err := directory.beforeOperation(evidenceFSSync, "directory"); err != nil {
		return err
	}
	if err := directory.file.Sync(); err != nil {
		return fmt.Errorf("sync private evidence pack directory: %w", err)
	}
	return directory.verifyPath()
}

func (directory *privateDirectory) close() error {
	if directory == nil {
		return nil
	}
	var result error
	if directory.file != nil {
		result = directory.file.Close()
		directory.file = nil
	}
	if directory.parent != nil {
		directory.parent.close()
		directory.parent = nil
	}
	return result
}

func fileOwner(info os.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func fileLinkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func boundedEntries(directory *privateDirectory, maximum int) ([]os.DirEntry, error) {
	if maximum < 0 {
		return nil, errors.New("private evidence pack directory entry bound is invalid")
	}
	if err := directory.beforeOperation(evidenceFSList, "directory"); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	opened := os.NewFile(uintptr(fd), directory.path)
	if opened == nil {
		_ = unix.Close(fd)
		return nil, errors.New("private evidence pack listing descriptor is invalid")
	}
	openedIdentity, statErr := evidenceIdentityForFile(opened)
	if statErr != nil || !sameEvidenceDirectoryBinding(directory.identity, openedIdentity) ||
		validateEvidenceDirectoryIdentity(openedIdentity, "private evidence pack directory") != nil {
		opened.Close()
		return nil, errors.New("private evidence pack directory changed while listing")
	}
	entries, readErr := opened.ReadDir(maximum + 1)
	closeErr := opened.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > maximum {
		return nil, errors.New("private evidence pack directory exceeds its entry bound")
	}
	if err := directory.verifyPath(); err != nil {
		return nil, err
	}
	return entries, nil
}

func readVerifiedFile(directory *privateDirectory, name, label, expectedSHA string, expectedBytes, maximum int, allowedLinks ...uint64) ([]byte, evidenceFileIdentity, error) {
	if err := validEvidenceName(name); err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	if err := directory.verifyPath(); err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	before, err := evidenceStatAt(directory.file, name)
	if err != nil {
		return nil, evidenceFileIdentity{}, fmt.Errorf("inspect %s: %w", label, err)
	}
	if err := validateEvidenceRegularIdentity(before, label, allowedLinks...); err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	if before.size < 0 || before.size > int64(maximum) || expectedBytes >= 0 && before.size != int64(expectedBytes) {
		return nil, evidenceFileIdentity{}, fmt.Errorf("%s has an invalid byte count", label)
	}
	if err := directory.beforeOperation(evidenceFSVerify, name); err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, evidenceFileIdentity{}, errors.New("private evidence pack file descriptor is invalid")
	}
	defer file.Close()
	openedIdentity, err := evidenceIdentityForFile(file)
	pathAfterOpen, pathErr := evidenceStatAt(directory.file, name)
	if err != nil || pathErr != nil || !sameEvidenceMetadata(before, openedIdentity) || !sameEvidenceMetadata(before, pathAfterOpen) {
		return nil, evidenceFileIdentity{}, fmt.Errorf("%s changed while being opened", label)
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	if len(body) > maximum || expectedBytes >= 0 && len(body) != expectedBytes {
		return nil, evidenceFileIdentity{}, fmt.Errorf("%s exceeds or disagrees with its byte bound", label)
	}
	afterIdentity, statErr := evidenceIdentityForFile(file)
	pathAfterRead, pathErr := evidenceStatAt(directory.file, name)
	if statErr != nil || pathErr != nil || !sameEvidenceMetadata(before, afterIdentity) ||
		!sameEvidenceMetadata(before, pathAfterRead) || afterIdentity.size != int64(len(body)) {
		return nil, evidenceFileIdentity{}, fmt.Errorf("%s changed while being read", label)
	}
	if err := validateEvidenceRegularIdentity(afterIdentity, label, allowedLinks...); err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	if expectedSHA != "" && sha256Hex(body) != expectedSHA {
		return nil, evidenceFileIdentity{}, fmt.Errorf("%s SHA-256 does not match", label)
	}
	if err := directory.verifyPath(); err != nil {
		return nil, evidenceFileIdentity{}, err
	}
	return body, afterIdentity, nil
}

type countHashWriter struct {
	hash  hashWriter
	count int
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func (writer *countHashWriter) Write(body []byte) (int, error) {
	written, err := writer.hash.Write(body)
	writer.count += written
	return written, err
}

func discardRecoverableStage(directory *privateDirectory, name, label string, expected evidenceFileIdentity) error {
	if err := validateEvidenceRegularIdentity(expected, "staged "+label, 1); err != nil {
		return err
	}
	if err := directory.beforeOperation(evidenceFSCleanup, name); err != nil {
		return err
	}
	current, err := evidenceStatAt(directory.file, name)
	if err != nil || !sameEvidenceMetadata(expected, current) {
		return fmt.Errorf("staged %s changed before crash recovery", label)
	}
	if err := unix.Unlinkat(int(directory.file.Fd()), name, 0); err != nil {
		return fmt.Errorf("remove incomplete staged %s: %w", label, err)
	}
	return directory.sync()
}

func publishFile(directory *privateDirectory, name, label string, expectedBytes int, expectedSHA string, allowExisting bool, writeBody func(io.Writer) error) (evidenceFileIdentity, error) {
	if err := validEvidenceName(name); err != nil || expectedBytes <= 0 || !canonicalSHA256.MatchString(expectedSHA) {
		return evidenceFileIdentity{}, errors.New("private publication identity is invalid")
	}
	if err := directory.verifyPath(); err != nil {
		return evidenceFileIdentity{}, err
	}
	tempName := "." + name + ".tmp"
	finalInfo, finalErr := evidenceStatAt(directory.file, name)
	tempInfo, tempErr := evidenceStatAt(directory.file, tempName)
	if finalErr == nil {
		if tempErr == nil && sameEvidenceMetadata(finalInfo, tempInfo) {
			if _, _, err := readVerifiedFile(directory, name, label, expectedSHA, expectedBytes, expectedBytes, 2); err != nil {
				return evidenceFileIdentity{}, err
			}
			if _, _, err := readVerifiedFile(directory, tempName, "staged "+label, expectedSHA, expectedBytes, expectedBytes, 2); err != nil {
				return evidenceFileIdentity{}, err
			}
			if err := removeEvidenceFileAt(directory, tempName, tempInfo, "staged "+label); err != nil {
				return evidenceFileIdentity{}, err
			}
			if err := directory.sync(); err != nil {
				return evidenceFileIdentity{}, err
			}
			_, identity, err := readVerifiedFile(directory, name, label, expectedSHA, expectedBytes, expectedBytes, 1)
			return identity, err
		}
		if tempErr != nil && !errors.Is(tempErr, os.ErrNotExist) {
			return evidenceFileIdentity{}, tempErr
		}
		if !allowExisting {
			return evidenceFileIdentity{}, ErrAlreadyPublished
		}
		_, identity, err := readVerifiedFile(directory, name, label, expectedSHA, expectedBytes, expectedBytes, 1)
		return identity, err
	}
	if finalErr != nil && !errors.Is(finalErr, os.ErrNotExist) {
		return evidenceFileIdentity{}, finalErr
	}
	if tempErr == nil {
		if _, _, verifyErr := readVerifiedFile(directory, tempName, "staged "+label, expectedSHA, expectedBytes, expectedBytes, 1); verifyErr != nil {
			if err := discardRecoverableStage(directory, tempName, label, tempInfo); err != nil {
				return evidenceFileIdentity{}, errors.Join(verifyErr, err)
			}
			tempErr = os.ErrNotExist
		}
	}
	var (
		stagedInfo evidenceFileIdentity
		err        error
	)
	if errors.Is(tempErr, os.ErrNotExist) {
		stagedInfo, err = createEvidenceStageAt(directory, tempName, label, expectedBytes, expectedSHA, writeBody)
		if err != nil {
			return evidenceFileIdentity{}, err
		}
	} else if tempErr != nil {
		return evidenceFileIdentity{}, tempErr
	} else {
		stagedInfo = tempInfo
	}
	if err := directory.beforeOperation(evidenceFSLink, name); err != nil {
		return evidenceFileIdentity{}, err
	}
	if _, _, err := readVerifiedFile(directory, tempName, "staged "+label, expectedSHA, expectedBytes, expectedBytes, 1); err != nil {
		return evidenceFileIdentity{}, err
	}
	currentStage, err := evidenceStatAt(directory.file, tempName)
	if err != nil || !sameEvidenceMetadata(stagedInfo, currentStage) || validateEvidenceRegularIdentity(currentStage, "staged "+label, 1) != nil {
		return evidenceFileIdentity{}, errors.Join(errors.New("staged private publication inode changed before linking"), err)
	}
	if err := unix.Linkat(int(directory.file.Fd()), tempName, int(directory.file.Fd()), name, 0); err != nil {
		if errors.Is(err, os.ErrExist) {
			return evidenceFileIdentity{}, errors.New("private publication path appeared concurrently")
		}
		return evidenceFileIdentity{}, err
	}
	publishedInfo, err := evidenceStatAt(directory.file, name)
	stagedAfterLink, stagedErr := evidenceStatAt(directory.file, tempName)
	if err != nil || stagedErr != nil || !sameEvidenceContentIdentity(stagedInfo, publishedInfo) ||
		!sameEvidenceMetadata(publishedInfo, stagedAfterLink) || publishedInfo.links != 2 {
		return evidenceFileIdentity{}, errors.Join(errors.New("private publication did not bind the staged inode exactly"), err, stagedErr)
	}
	if err := directory.sync(); err != nil {
		return evidenceFileIdentity{}, err
	}
	if testHookAfterLink != nil {
		if err := testHookAfterLink(label); err != nil {
			return evidenceFileIdentity{}, err
		}
	}
	if err := removeEvidenceFileAt(directory, tempName, stagedAfterLink, "staged "+label); err != nil {
		return evidenceFileIdentity{}, err
	}
	if err := directory.sync(); err != nil {
		return evidenceFileIdentity{}, err
	}
	_, identity, err := readVerifiedFile(directory, name, label, expectedSHA, expectedBytes, expectedBytes, 1)
	return identity, err
}

func createEvidenceStageAt(directory *privateDirectory, name, label string, expectedBytes int, expectedSHA string, writeBody func(io.Writer) error) (evidenceFileIdentity, error) {
	if err := directory.beforeOperation(evidenceFSCreate, name); err != nil {
		return evidenceFileIdentity{}, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return evidenceFileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return evidenceFileIdentity{}, errors.New("staged private publication descriptor is invalid")
	}
	created := true
	defer func() {
		if created {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return evidenceFileIdentity{}, err
	}
	digest := sha256.New()
	counter := &countHashWriter{hash: digest}
	if err := writeBody(io.MultiWriter(file, counter)); err != nil {
		return evidenceFileIdentity{}, err
	}
	if counter.count != expectedBytes || hex.EncodeToString(digest.Sum(nil)) != expectedSHA {
		return evidenceFileIdentity{}, errors.New("staged private publication bytes differ from their plan")
	}
	if err := file.Sync(); err != nil {
		return evidenceFileIdentity{}, err
	}
	opened, err := evidenceIdentityForFile(file)
	if err != nil || validateEvidenceRegularIdentity(opened, "staged "+label, 1) != nil || opened.size != int64(expectedBytes) {
		return evidenceFileIdentity{}, errors.Join(errors.New("staged private publication identity is invalid"), err)
	}
	if err := file.Close(); err != nil {
		return evidenceFileIdentity{}, err
	}
	created = false
	if err := directory.beforeOperation(evidenceFSVerify, name); err != nil {
		return evidenceFileIdentity{}, err
	}
	named, err := evidenceStatAt(directory.file, name)
	if err != nil || !sameEvidenceMetadata(opened, named) {
		return evidenceFileIdentity{}, errors.Join(errors.New("staged private publication filename changed after creation"), err)
	}
	if err := directory.sync(); err != nil {
		return evidenceFileIdentity{}, err
	}
	committed, err := evidenceStatAt(directory.file, name)
	if err != nil || !sameEvidenceMetadata(opened, committed) || validateEvidenceRegularIdentity(committed, "staged "+label, 1) != nil {
		return evidenceFileIdentity{}, errors.Join(errors.New("staged private publication inode changed before commit"), err)
	}
	return opened, nil
}

func removeEvidenceFileAt(directory *privateDirectory, name string, expected evidenceFileIdentity, label string) error {
	if err := directory.beforeOperation(evidenceFSCleanup, name); err != nil {
		return err
	}
	current, err := evidenceStatAt(directory.file, name)
	if err != nil || !sameEvidenceMetadata(expected, current) ||
		validateEvidenceRegularIdentity(current, label, expected.links) != nil {
		return fmt.Errorf("%s changed before cleanup", label)
	}
	if err := unix.Unlinkat(int(directory.file.Fd()), name, 0); err != nil {
		return err
	}
	return directory.verifyPath()
}

func validEvidenceName(name string) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return errors.New("private evidence pack filename is invalid")
	}
	return nil
}

func runEvidenceFSHook(operation, name string) error {
	if testHookBeforeFilesystemOperation != nil {
		return testHookBeforeFilesystemOperation(operation, name)
	}
	return nil
}

func evidenceStatAt(directory *os.File, name string) (evidenceFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return evidenceFileIdentity{}, err
	}
	return evidenceIdentityFromStat(&stat), nil
}

func evidenceIdentityForFile(file *os.File) (evidenceFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return evidenceFileIdentity{}, err
	}
	return evidenceIdentityFromStat(&stat), nil
}

func evidenceIdentityFromStat(stat *unix.Stat_t) evidenceFileIdentity {
	return evidenceFileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), mode: uint32(stat.Mode), uid: stat.Uid,
		links: uint64(stat.Nlink), size: stat.Size,
	}
}

func sameEvidenceIdentity(left, right evidenceFileIdentity) bool {
	return left.device == right.device && left.inode == right.inode
}

func sameEvidenceDirectoryBinding(left, right evidenceFileIdentity) bool {
	return sameEvidenceIdentity(left, right) && left.mode == right.mode && left.uid == right.uid
}

func sameEvidenceMetadata(left, right evidenceFileIdentity) bool {
	return left == right
}

func sameEvidenceContentIdentity(left, right evidenceFileIdentity) bool {
	return sameEvidenceIdentity(left, right) && left.mode == right.mode && left.uid == right.uid && left.size == right.size
}

func validateEvidenceCreatedDirectoryIdentity(info evidenceFileIdentity, label string) error {
	if info.mode&unix.S_IFMT != unix.S_IFDIR || int(info.uid) != os.Geteuid() {
		return fmt.Errorf("%s must be a direct effective-UID-owned directory", label)
	}
	return nil
}

func validateEvidenceDirectoryIdentity(info evidenceFileIdentity, label string) error {
	if info.mode&unix.S_IFMT != unix.S_IFDIR || info.mode&0o777 != 0o700 || int(info.uid) != os.Geteuid() {
		return fmt.Errorf("%s must be a direct effective-UID-owned mode-0700 directory", label)
	}
	return nil
}

func validateEvidenceRegularIdentity(info evidenceFileIdentity, label string, allowedLinks ...uint64) error {
	if info.mode&unix.S_IFMT != unix.S_IFREG || info.mode&0o777 != 0o600 || int(info.uid) != os.Geteuid() {
		return fmt.Errorf("%s must be a direct effective-UID-owned mode-0600 regular file", label)
	}
	allowed := len(allowedLinks) == 0
	for _, count := range allowedLinks {
		allowed = allowed || info.links == count
	}
	if !allowed {
		return fmt.Errorf("%s has an invalid hard-link count", label)
	}
	return nil
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
