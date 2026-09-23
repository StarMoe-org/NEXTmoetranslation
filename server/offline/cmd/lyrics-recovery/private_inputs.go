package main

import (
	"crypto/sha256"
	"encoding/hex"

	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"moesekai/server/offline/internal/lyricsacquisition"

	"moesekai/server/offline/internal/lyricsrecovery"
)

const (
	privateInputAfterDirectoryInspect = "after-directory-inspect"
	privateInputAfterDirectoryOpen    = "after-directory-open"
	privateInputAfterLeafInspect      = "after-leaf-inspect"
	privateInputAfterLeafOpen         = "after-leaf-open"
	privateInputAfterLeafRead         = "after-leaf-read"
)

type privateInputHook func(stage, path string, file *os.File) error

var privateInputHooks struct {
	sync.RWMutex
	hook privateInputHook
}

var privateInputAncestryRootPath = string(os.PathSeparator)
var recoveryLiveStateProvisionParentPath = "/private/tmp"

type directoryPolicy struct {
	label        string
	private      bool
	effectiveUID bool
}

type regularFilePolicy struct {
	label            string
	exactPermissions os.FileMode
	requireExactMode bool
	requirePrivate   bool
	maximum          int64
}

type inputStatFingerprint struct {
	mode      os.FileMode
	size      int64
	modTimeNS int64
	uid       uint64
	nlink     uint64
	device    uint64
	inode     uint64
	ctimeSec  int64
	ctimeNSec int64
}

// Directory entry churn changes size, mtime, ctime, and sometimes link count
// even when the opened directory path, ownership, permissions, device, and
// inode remain unchanged. Those volatile fields must not make a long-running
// immutable leaf pin fail because an unrelated sibling appeared.
type inputDirectoryFingerprint struct {
	mode   os.FileMode
	uid    uint64
	device uint64
	inode  uint64
}

type pinnedDirectory struct {
	path   string
	file   *os.File
	info   os.FileInfo
	policy directoryPolicy
}

type pinnedRegularFile struct {
	path   string
	file   *os.File
	info   os.FileInfo
	parent *pinnedDirectory
	policy regularFilePolicy
}

type checkedRecoveryCatalog struct {
	catalog *lyricsrecovery.Catalog
	pinned  *pinnedRegularFile
}

type checkedRecoveryLedger struct {
	ledger *lyricsacquisition.Ledger
	root   *pinnedDirectory
}

func setPrivateInputTestHook(hook privateInputHook) {
	privateInputHooks.Lock()
	privateInputHooks.hook = hook
	privateInputHooks.Unlock()
}

func setPrivateInputAncestryRootForTest(path string) {
	privateInputAncestryRootPath = path
}

func setRecoveryLiveStateProvisionParentForTest(path string) {
	recoveryLiveStateProvisionParentPath = path
}

func invokePrivateInputHook(stage, path string, file *os.File) error {
	privateInputHooks.RLock()
	hook := privateInputHooks.hook
	privateInputHooks.RUnlock()
	if hook == nil {
		return nil
	}
	return hook(stage, path, file)
}

func canonicalAbsolutePath(path string) bool {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func openCanonicalDirectory(path string, policy directoryPolicy) (*pinnedDirectory, error) {
	if !canonicalAbsolutePath(path) {
		return nil, fmt.Errorf("%s must have canonical symlink-free ancestry", policy.label)
	}
	volume := filepath.VolumeName(path)
	rootPath := volume + string(os.PathSeparator)
	if relative, err := filepath.Rel(privateInputAncestryRootPath, path); err == nil &&
		(relative == "." || filepath.IsLocal(relative)) {
		rootPath = privateInputAncestryRootPath
	}
	rootBefore, err := os.Lstat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("inspect %s ancestry root: %w", policy.label, err)
	}
	rootPolicy := directoryPolicy{label: policy.label + " ancestry root"}
	if err := validateDirectoryInfo(rootBefore, rootPolicy); err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s ancestry root: %w", policy.label, err)
	}
	currentFile := os.NewFile(uintptr(rootFD), rootPath)
	rootOpened, openedErr := currentFile.Stat()
	rootPathInfo, pathErr := os.Lstat(rootPath)
	if openedErr != nil || pathErr != nil || !os.SameFile(rootBefore, rootOpened) || !os.SameFile(rootBefore, rootPathInfo) ||
		!sameDirectoryAttributes(rootBefore, rootOpened) || !sameDirectoryAttributes(rootBefore, rootPathInfo) {
		_ = currentFile.Close()
		return nil, fmt.Errorf("%s ancestry root changed while being opened", policy.label)
	}
	currentPath := rootPath
	components := strings.Split(strings.TrimPrefix(path, rootPath), string(os.PathSeparator))
	for index, component := range components {
		if component == "" {
			continue
		}
		currentPath = filepath.Join(currentPath, component)
		before, err := os.Lstat(currentPath)
		if err != nil {
			_ = currentFile.Close()
			return nil, fmt.Errorf("inspect %s ancestry: %w", policy.label, err)
		}
		componentPolicy := directoryPolicy{label: policy.label + " ancestry"}
		if index == len(components)-1 {
			componentPolicy = policy
		}
		if err := validateDirectoryInfo(before, componentPolicy); err != nil {
			_ = currentFile.Close()
			return nil, err
		}
		if err := invokePrivateInputHook(privateInputAfterDirectoryInspect, currentPath, currentFile); err != nil {
			_ = currentFile.Close()
			return nil, err
		}
		fd, err := unix.Openat(int(currentFile.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = currentFile.Close()
			return nil, fmt.Errorf("open %s ancestry component: %w", policy.label, err)
		}
		opened := os.NewFile(uintptr(fd), currentPath)
		openedInfo, openedErr := opened.Stat()
		pathInfo, pathErr := os.Lstat(currentPath)
		if openedErr != nil || pathErr != nil || !os.SameFile(before, openedInfo) || !os.SameFile(before, pathInfo) ||
			!sameDirectoryAttributes(before, openedInfo) || !sameDirectoryAttributes(before, pathInfo) {
			_ = opened.Close()
			_ = currentFile.Close()
			return nil, fmt.Errorf("%s ancestry changed while being opened", policy.label)
		}
		if err := validateDirectoryInfo(openedInfo, componentPolicy); err != nil {
			_ = opened.Close()
			_ = currentFile.Close()
			return nil, err
		}
		_ = currentFile.Close()
		currentFile = opened
		if err := invokePrivateInputHook(privateInputAfterDirectoryOpen, currentPath, currentFile); err != nil {
			_ = currentFile.Close()
			return nil, err
		}
	}
	info, err := currentFile.Stat()
	if err != nil {
		_ = currentFile.Close()
		return nil, fmt.Errorf("inspect opened %s: %w", policy.label, err)
	}
	pinned := &pinnedDirectory{path: path, file: currentFile, info: info, policy: policy}
	if err := pinned.verify(); err != nil {
		_ = pinned.Close()
		return nil, err
	}
	return pinned, nil
}

func openDirectoryAt(parent *pinnedDirectory, name, path string, policy directoryPolicy) (*pinnedDirectory, error) {
	if parent == nil || parent.file == nil || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("%s path component is invalid", policy.label)
	}
	if err := parent.verify(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", policy.label, err)
	}
	if err := validateDirectoryInfo(before, policy); err != nil {
		return nil, err
	}
	if err := invokePrivateInputHook(privateInputAfterDirectoryInspect, path, parent.file); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", policy.label, err)
	}
	file := os.NewFile(uintptr(fd), path)
	openedInfo, openedErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if openedErr != nil || pathErr != nil || !os.SameFile(before, openedInfo) || !os.SameFile(before, pathInfo) ||
		!sameDirectoryAttributes(before, openedInfo) || !sameDirectoryAttributes(before, pathInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("%s changed while being opened", policy.label)
	}
	pinned := &pinnedDirectory{path: path, file: file, info: openedInfo, policy: policy}
	if err := invokePrivateInputHook(privateInputAfterDirectoryOpen, path, file); err != nil {
		_ = pinned.Close()
		return nil, err
	}
	if err := pinned.verify(); err != nil {
		_ = pinned.Close()
		return nil, err
	}
	return pinned, nil
}

func (directory *pinnedDirectory) verify() error {
	if directory == nil || directory.file == nil || directory.info == nil {
		return errors.New("pinned directory is not open")
	}
	opened, openedErr := directory.file.Stat()
	pathInfo, pathErr := os.Lstat(directory.path)
	if openedErr != nil || pathErr != nil || !os.SameFile(directory.info, opened) || !os.SameFile(directory.info, pathInfo) ||
		!sameDirectoryAttributes(directory.info, opened) || !sameDirectoryAttributes(directory.info, pathInfo) {
		return fmt.Errorf("%s path or identity changed", directory.policy.label)
	}
	if !canonicalAbsolutePath(directory.path) {
		return fmt.Errorf("%s ancestry is no longer canonical", directory.policy.label)
	}
	return validateDirectoryInfo(opened, directory.policy)
}

func (directory *pinnedDirectory) Close() error {
	if directory == nil || directory.file == nil {
		return nil
	}
	err := directory.file.Close()
	directory.file = nil
	return err
}

func validateDirectoryInfo(info os.FileInfo, policy directoryPolicy) error {
	owner, ownerOK := inputOwner(info)
	if info == nil || info.Mode().Type() != os.ModeDir || !ownerOK {
		return fmt.Errorf("%s must be an exact directory with a known owner", policy.label)
	}
	if policy.effectiveUID || policy.private {
		if int(owner) != os.Geteuid() {
			return fmt.Errorf("%s must be owned by the effective UID", policy.label)
		}
	} else if owner != 0 && int(owner) != os.Geteuid() {
		return fmt.Errorf("%s ancestry owner is not trusted", policy.label)
	}
	if policy.private {
		if info.Mode().Perm() != 0o700 {
			return fmt.Errorf("%s must have permissions exactly 0700", policy.label)
		}
	} else if info.Mode().Perm()&0o022 != 0 && !(owner == 0 && info.Mode()&os.ModeSticky != 0) {
		return fmt.Errorf("%s is writable by an untrusted local UID", policy.label)
	}
	return nil
}

func sameDirectoryAttributes(expected, actual os.FileInfo) bool {
	if expected == nil || actual == nil || !os.SameFile(expected, actual) {
		return false
	}
	expectedFingerprint, expectedOK := directoryFingerprint(expected)
	actualFingerprint, actualOK := directoryFingerprint(actual)
	return expectedOK && actualOK && expectedFingerprint == actualFingerprint
}

func directoryFingerprint(info os.FileInfo) (inputDirectoryFingerprint, bool) {
	if info == nil || info.Mode().Type() != os.ModeDir {
		return inputDirectoryFingerprint{}, false
	}
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return inputDirectoryFingerprint{}, false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return inputDirectoryFingerprint{}, false
		}
		value = value.Elem()
	}
	uid, uidOK := inputNumericStatField(value, "Uid")
	device, deviceOK := inputNumericStatField(value, "Dev")
	inode, inodeOK := inputNumericStatField(value, "Ino")
	if !uidOK || !deviceOK || !inodeOK {
		return inputDirectoryFingerprint{}, false
	}
	return inputDirectoryFingerprint{
		mode: info.Mode(), uid: uid, device: device, inode: inode,
	}, true
}

func openPinnedRegularFile(path string, policy regularFilePolicy) (*pinnedRegularFile, error) {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%s path must be explicit, canonical, and absolute", policy.label)
	}
	parentPolicy := directoryPolicy{label: policy.label + " parent"}
	if policy.requirePrivate {
		parentPolicy.private = true
		parentPolicy.effectiveUID = true
	}
	parent, err := openCanonicalDirectory(filepath.Dir(path), parentPolicy)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		_ = parent.Close()
		return nil, fmt.Errorf("inspect %s: %w", policy.label, err)
	}
	if err := validateRegularFileInfo(before, policy); err != nil {
		_ = parent.Close()
		return nil, err
	}
	if err := invokePrivateInputHook(privateInputAfterLeafInspect, path, parent.file); err != nil {
		_ = parent.Close()
		return nil, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = parent.Close()
		return nil, fmt.Errorf("open %s: %w", policy.label, err)
	}
	file := os.NewFile(uintptr(fd), path)
	opened, openedErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if openedErr != nil || pathErr != nil || !sameStableRegularFile(before, opened) || !sameStableRegularFile(before, pathInfo) {
		_ = file.Close()
		_ = parent.Close()
		return nil, fmt.Errorf("%s path or inode changed while being opened", policy.label)
	}
	pinned := &pinnedRegularFile{path: path, file: file, info: opened, parent: parent, policy: policy}
	if err := invokePrivateInputHook(privateInputAfterLeafOpen, path, file); err != nil {
		_ = pinned.Close()
		return nil, err
	}
	if err := pinned.verify(); err != nil {
		_ = pinned.Close()
		return nil, err
	}
	return pinned, nil
}

func openPinnedRegularFileAt(parent *pinnedDirectory, name, path string, policy regularFilePolicy) (*pinnedRegularFile, error) {
	if parent == nil || parent.file == nil || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("%s leaf is invalid", policy.label)
	}
	if err := parent.verify(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", policy.label, err)
	}
	if err := validateRegularFileInfo(before, policy); err != nil {
		return nil, err
	}
	if err := invokePrivateInputHook(privateInputAfterLeafInspect, path, parent.file); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", policy.label, err)
	}
	file := os.NewFile(uintptr(fd), path)
	opened, openedErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if openedErr != nil || pathErr != nil || !sameStableRegularFile(before, opened) || !sameStableRegularFile(before, pathInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("%s path or inode changed while being opened", policy.label)
	}
	pinned := &pinnedRegularFile{path: path, file: file, info: opened, parent: parent, policy: policy}
	if err := invokePrivateInputHook(privateInputAfterLeafOpen, path, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := pinned.verify(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return pinned, nil
}

func (file *pinnedRegularFile) verify() error {
	if file == nil || file.file == nil || file.info == nil || file.parent == nil {
		return errors.New("pinned regular file is not open")
	}
	if err := file.parent.verify(); err != nil {
		return err
	}
	opened, openedErr := file.file.Stat()
	pathInfo, pathErr := os.Lstat(file.path)
	if openedErr != nil || pathErr != nil || !sameStableRegularFile(file.info, opened) || !sameStableRegularFile(file.info, pathInfo) {
		return fmt.Errorf("%s path, inode, size, mode, links, owner, or modification time changed", file.policy.label)
	}
	return validateRegularFileInfo(opened, file.policy)
}

func (file *pinnedRegularFile) readAll() ([]byte, error) {
	if file == nil || file.policy.maximum <= 0 || file.policy.maximum > int64(^uint(0)>>1) {
		return nil, errors.New("pinned private input byte boundary is invalid")
	}
	if err := file.verify(); err != nil {
		return nil, err
	}
	if _, err := file.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(file.file, file.policy.maximum+1))
	if err != nil {
		return nil, err
	}
	if err := invokePrivateInputHook(privateInputAfterLeafRead, file.path, file.file); err != nil {
		return nil, err
	}
	if int64(len(body)) != file.info.Size() || int64(len(body)) > file.policy.maximum {
		return nil, fmt.Errorf("%s changed size or exceeded its byte boundary while being read", file.policy.label)
	}
	if err := file.verify(); err != nil {
		return nil, err
	}
	return body, nil
}

func (file *pinnedRegularFile) sha256(expectedSize int64) (string, error) {
	if file == nil || expectedSize <= 0 || expectedSize > file.policy.maximum || file.info.Size() != expectedSize {
		return "", fmt.Errorf("%s size does not match its immutable pin", file.policy.label)
	}
	if err := file.verify(); err != nil {
		return "", err
	}
	if _, err := file.file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	digest := sha256.New()
	readBytes, err := io.Copy(digest, io.LimitReader(file.file, expectedSize+1))
	if err != nil {
		return "", err
	}
	if err := invokePrivateInputHook(privateInputAfterLeafRead, file.path, file.file); err != nil {
		return "", err
	}
	if readBytes != expectedSize {
		return "", fmt.Errorf("%s changed size while being hashed", file.policy.label)
	}
	if err := file.verify(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (file *pinnedRegularFile) Close() error {
	if file == nil {
		return nil
	}
	var result error
	if file.file != nil {
		result = errors.Join(result, file.file.Close())
		file.file = nil
	}
	if file.parent != nil {
		result = errors.Join(result, file.parent.Close())
		file.parent = nil
	}
	return result
}

func validateRegularFileInfo(info os.FileInfo, policy regularFilePolicy) error {
	owner, ownerOK := inputOwner(info)
	if info == nil || info.Mode().Type() != 0 || !ownerOK || int(owner) != os.Geteuid() || inputLinkCount(info) != 1 {
		return fmt.Errorf("%s must be an effective-UID-owned single-link regular file", policy.label)
	}
	if policy.requireExactMode {
		if info.Mode().Perm() != policy.exactPermissions {
			return fmt.Errorf("%s must have permissions exactly %04o", policy.label, policy.exactPermissions)
		}
	} else if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must not be group/other-writable", policy.label)
	}
	if info.Size() <= 0 || info.Size() > policy.maximum {
		return fmt.Errorf("%s must be non-empty and within its exact byte boundary", policy.label)
	}
	return nil
}

func sameStableRegularFile(expected, actual os.FileInfo) bool {
	if expected == nil || actual == nil || !os.SameFile(expected, actual) {
		return false
	}
	expectedFingerprint, expectedOK := inputFingerprint(expected)
	actualFingerprint, actualOK := inputFingerprint(actual)
	return expectedOK && actualOK && expectedFingerprint == actualFingerprint
}

func inputFingerprint(info os.FileInfo) (inputStatFingerprint, bool) {
	if info == nil {
		return inputStatFingerprint{}, false
	}
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return inputStatFingerprint{}, false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return inputStatFingerprint{}, false
		}
		value = value.Elem()
	}
	uid, uidOK := inputNumericStatField(value, "Uid")
	nlink, nlinkOK := inputNumericStatField(value, "Nlink")
	device, deviceOK := inputNumericStatField(value, "Dev")
	inode, inodeOK := inputNumericStatField(value, "Ino")
	ctimeSec, ctimeNSec, ctimeOK := inputChangeTimeStatFields(value)
	if !uidOK || !nlinkOK || !deviceOK || !inodeOK || !ctimeOK {
		return inputStatFingerprint{}, false
	}
	return inputStatFingerprint{
		mode: info.Mode(), size: info.Size(), modTimeNS: info.ModTime().UnixNano(),
		uid: uid, nlink: nlink, device: device, inode: inode, ctimeSec: ctimeSec, ctimeNSec: ctimeNSec,
	}, true
}

func inputNumericStatField(value reflect.Value, name string) (uint64, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() < 0 {
			return 0, false
		}
		return uint64(field.Int()), true
	default:
		return 0, false
	}
}

func inputSignedStatField(value reflect.Value, name string) (int64, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return field.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if field.Uint() > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(field.Uint()), true
	default:
		return 0, false
	}
}

func inputChangeTimeStatFields(value reflect.Value) (int64, int64, bool) {
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := value.FieldByName(name)
		if field.IsValid() {
			sec, secOK := inputSignedStatField(field, "Sec")
			nsec, nsecOK := inputSignedStatField(field, "Nsec")
			if secOK && nsecOK {
				return sec, nsec, true
			}
		}
	}
	sec, secOK := inputSignedStatField(value, "Ctime")
	nsec, nsecOK := inputSignedStatField(value, "Ctimensec")
	return sec, nsec, secOK && nsecOK
}

func inputOwner(info os.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func inputLinkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
