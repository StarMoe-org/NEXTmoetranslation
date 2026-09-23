package lyricsrecovery

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrAlreadyPublished = errors.New("lyrics recovery output is already published")

const (
	recoveryFSCreate  = "create"
	recoveryFSLink    = "link"
	recoveryFSVerify  = "verify"
	recoveryFSCleanup = "cleanup"
	recoveryFSSync    = "sync"
)

var (
	testHookAfterPublishLink         func() error
	testHookBeforePublishFSOperation func(operation, name string) error
)

type recoveryFileIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	uid    uint32
	links  uint64
	size   int64
}

type lockedRecoveryDirectory struct {
	path string
	file *os.File
	info os.FileInfo
}

func PublishSongResult(path string, result SongResult) error {
	body, err := MarshalSongResult(result)
	if err != nil {
		return err
	}
	return publishPrivateFile(path, body, func(candidate []byte) error {
		_, err := DecodeSongResult(candidate)
		return err
	})
}

func OpenSongResult(path string) (SongResult, error) {
	body, err := readPrivateFile(path, MaxSongResultBytes, 1)
	if err != nil {
		return SongResult{}, err
	}
	return DecodeSongResult(body)
}

func publishPrivateFile(path string, body []byte, validate func([]byte) error) error {
	if path == "" || strings.TrimSpace(path) != path || len(body) == 0 || validate == nil {
		return errors.New("lyrics recovery private publication input is invalid")
	}
	if err := validate(body); err != nil {
		return err
	}
	absolute, parentPath, name, err := splitRecoveryPath(path, true)
	if err != nil {
		return err
	}
	parent, err := openLockedRecoveryDirectory(parentPath)
	if err != nil {
		return err
	}
	defer parent.close()
	defer parent.unlock()

	tempName := "." + name + ".lyrics-recovery-v2.tmp"
	finalInfo, finalErr := recoveryStatAt(parent.file, name)
	tempInfo, tempErr := recoveryStatAt(parent.file, tempName)
	if finalErr == nil {
		if tempErr == nil && sameRecoveryMetadata(finalInfo, tempInfo) {
			finalBody, err := readPrivateFileAt(parent, name, len(body), 2)
			if err != nil || !bytes.Equal(finalBody, body) {
				return errors.New("lyrics recovery crash pair final bytes do not match")
			}
			tempBody, err := readPrivateFileAt(parent, tempName, len(body), 2)
			if err != nil || !bytes.Equal(tempBody, body) {
				return errors.New("lyrics recovery crash pair staged bytes do not match")
			}
			if err := removeRecoveryFileAt(parent, tempName, tempInfo); err != nil {
				return err
			}
			if err := parent.sync(tempName); err != nil {
				return err
			}
			publishedBody, err := readPrivateFileAt(parent, name, len(body), 1)
			if err != nil || !bytes.Equal(publishedBody, body) {
				return errors.New("lyrics recovery recovered bytes do not match")
			}
			return parent.verifyPath()
		}
		return ErrAlreadyPublished
	}
	if finalErr != nil && !errors.Is(finalErr, os.ErrNotExist) {
		return finalErr
	}
	var stagedInfo recoveryFileIdentity
	if tempErr == nil {
		staged, err := readPrivateFileAt(parent, tempName, len(body), 1)
		if err != nil || !bytes.Equal(staged, body) || validate(staged) != nil {
			return errors.New("lyrics recovery staged output is not the exact recoverable artifact")
		}
		stagedInfo = tempInfo
	} else if errors.Is(tempErr, os.ErrNotExist) {
		stagedInfo, err = createRecoveryStageAt(parent, tempName, body)
		if err != nil {
			return err
		}
	} else {
		return tempErr
	}
	if err := runRecoveryFSHook(recoveryFSLink, name); err != nil {
		return err
	}
	staged, err := readPrivateFileAt(parent, tempName, len(body), 1)
	if err != nil || !bytes.Equal(staged, body) || validate(staged) != nil {
		return errors.New("lyrics recovery staged output changed before linking")
	}
	currentStage, err := recoveryStatAt(parent.file, tempName)
	if err != nil || !sameRecoveryMetadata(stagedInfo, currentStage) || !validRecoveryRegularIdentity(currentStage, 1) {
		return errors.Join(errors.New("lyrics recovery staged inode changed before linking"), err)
	}
	if err := unix.Linkat(int(parent.file.Fd()), tempName, int(parent.file.Fd()), name, 0); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyPublished
		}
		return err
	}
	published, err := recoveryStatAt(parent.file, name)
	stagedAfterLink, stagedErr := recoveryStatAt(parent.file, tempName)
	if err != nil || stagedErr != nil || !sameRecoveryContentIdentity(stagedInfo, published) ||
		!sameRecoveryMetadata(published, stagedAfterLink) || published.links != 2 {
		return errors.Join(errors.New("lyrics recovery output did not bind the staged inode"), err, stagedErr)
	}
	if err := parent.sync(name); err != nil {
		return err
	}
	if testHookAfterPublishLink != nil {
		if err := testHookAfterPublishLink(); err != nil {
			return err
		}
	}
	if err := removeRecoveryFileAt(parent, tempName, stagedAfterLink); err != nil {
		return err
	}
	if err := parent.sync(tempName); err != nil {
		return err
	}
	publishedBody, err := readPrivateFileAt(parent, name, len(body), 1)
	if err != nil || !bytes.Equal(publishedBody, body) {
		return errors.New("lyrics recovery published bytes do not match")
	}
	if err := parent.verifyPath(); err != nil {
		return err
	}
	_ = absolute
	return nil
}

func readPrivateFile(path string, maximum int, links uint64) ([]byte, error) {
	_, parentPath, name, err := splitRecoveryPath(path, false)
	if err != nil {
		return nil, err
	}
	parent, err := openLockedRecoveryDirectory(parentPath)
	if err != nil {
		return nil, err
	}
	defer parent.close()
	defer parent.unlock()
	body, err := readPrivateFileAt(parent, name, maximum, links)
	if err != nil {
		return nil, err
	}
	if err := parent.verifyPath(); err != nil {
		return nil, err
	}
	return body, nil
}

func createRecoveryStageAt(parent *lockedRecoveryDirectory, name string, body []byte) (recoveryFileIdentity, error) {
	if err := runRecoveryFSHook(recoveryFSCreate, name); err != nil {
		return recoveryFileIdentity{}, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return recoveryFileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return recoveryFileIdentity{}, errors.New("lyrics recovery staging descriptor is invalid")
	}
	created := true
	defer func() {
		if created {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return recoveryFileIdentity{}, err
	}
	if err := writeRecoveryBytes(file, body); err != nil {
		return recoveryFileIdentity{}, err
	}
	if err := file.Sync(); err != nil {
		return recoveryFileIdentity{}, err
	}
	opened, err := recoveryIdentityForFile(file)
	if err != nil || !validRecoveryRegularIdentity(opened, 1) || opened.size != int64(len(body)) {
		return recoveryFileIdentity{}, errors.Join(errors.New("lyrics recovery staged output identity is invalid"), err)
	}
	if err := file.Close(); err != nil {
		return recoveryFileIdentity{}, err
	}
	created = false
	if err := runRecoveryFSHook(recoveryFSVerify, name); err != nil {
		return recoveryFileIdentity{}, err
	}
	named, err := recoveryStatAt(parent.file, name)
	if err != nil || !sameRecoveryMetadata(opened, named) {
		return recoveryFileIdentity{}, errors.Join(errors.New("lyrics recovery staged filename changed after creation"), err)
	}
	if err := parent.sync(name); err != nil {
		return recoveryFileIdentity{}, err
	}
	committed, err := recoveryStatAt(parent.file, name)
	if err != nil || !sameRecoveryMetadata(opened, committed) || !validRecoveryRegularIdentity(committed, 1) {
		return recoveryFileIdentity{}, errors.Join(errors.New("lyrics recovery staged inode changed before commit"), err)
	}
	return opened, nil
}

func writeRecoveryBytes(file *os.File, body []byte) error {
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

func readPrivateFileAt(parent *lockedRecoveryDirectory, name string, maximum int, links uint64) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("lyrics recovery private file boundary is invalid")
	}
	if err := validRecoveryName(name); err != nil {
		return nil, err
	}
	if err := parent.verifyDescriptor(); err != nil {
		return nil, err
	}
	before, err := recoveryStatAt(parent.file, name)
	if err != nil || !validRecoveryRegularIdentity(before, links) || before.size <= 0 || before.size > int64(maximum) {
		return nil, errors.New("lyrics recovery private file identity is invalid")
	}
	if err := runRecoveryFSHook(recoveryFSVerify, name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("lyrics recovery private file descriptor is invalid")
	}
	defer file.Close()
	opened, err := recoveryIdentityForFile(file)
	afterOpen, pathErr := recoveryStatAt(parent.file, name)
	if err != nil || pathErr != nil || !sameRecoveryMetadata(before, opened) || !sameRecoveryMetadata(before, afterOpen) {
		return nil, errors.New("lyrics recovery private file changed while being opened")
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || len(body) > maximum {
		return nil, errors.New("lyrics recovery private file exceeds its boundary")
	}
	after, statErr := recoveryIdentityForFile(file)
	afterPath, pathStatErr := recoveryStatAt(parent.file, name)
	if statErr != nil || pathStatErr != nil || !sameRecoveryMetadata(before, after) || !sameRecoveryMetadata(before, afterPath) ||
		after.size != int64(len(body)) || !validRecoveryRegularIdentity(after, links) {
		return nil, errors.New("lyrics recovery private file changed while being read")
	}
	if err := parent.verifyPath(); err != nil {
		return nil, err
	}
	return body, nil
}

func removeRecoveryFileAt(parent *lockedRecoveryDirectory, name string, expected recoveryFileIdentity) error {
	if err := runRecoveryFSHook(recoveryFSCleanup, name); err != nil {
		return err
	}
	current, err := recoveryStatAt(parent.file, name)
	if err != nil || !sameRecoveryMetadata(expected, current) {
		return errors.New("lyrics recovery staged output changed before cleanup")
	}
	if err := unix.Unlinkat(int(parent.file.Fd()), name, 0); err != nil {
		return err
	}
	return nil
}

func splitRecoveryPath(path string, requireCanonical bool) (absolute, parent, name string, err error) {
	if path == "" || strings.TrimSpace(path) != path {
		return "", "", "", errors.New("lyrics recovery private file path is invalid")
	}
	absolute, err = filepath.Abs(path)
	if err != nil {
		return "", "", "", err
	}
	if requireCanonical && absolute != path {
		return "", "", "", errors.New("lyrics recovery output path must be absolute and canonical")
	}
	name = filepath.Base(absolute)
	if err := validRecoveryName(name); err != nil {
		return "", "", "", err
	}
	return absolute, filepath.Dir(absolute), name, nil
}

func validRecoveryName(name string) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return errors.New("lyrics recovery private filename is invalid")
	}
	return nil
}

func openLockedRecoveryDirectory(path string) (*lockedRecoveryDirectory, error) {
	file, info, err := openStablePrivateDirectory(path)
	if err != nil {
		return nil, err
	}
	directory := &lockedRecoveryDirectory{path: path, file: file, info: info}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	if err := directory.verifyPath(); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		return nil, err
	}
	return directory, nil
}

func openStablePrivateDirectory(path string) (*os.File, os.FileInfo, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || absolute != path {
		return nil, nil, errors.New("lyrics recovery output parent must be absolute and canonical")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || resolved != absolute {
		return nil, nil, errors.New("lyrics recovery output parent must not be a symlink or alias")
	}
	before, err := os.Lstat(absolute)
	if err != nil || !privateDirectory(before) {
		return nil, nil, errors.New("lyrics recovery output parent must be effective-UID-owned mode-0700")
	}
	fd, err := unix.Open(absolute, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	directory := os.NewFile(uintptr(fd), absolute)
	if directory == nil {
		_ = unix.Close(fd)
		return nil, nil, errors.New("lyrics recovery output parent descriptor is invalid")
	}
	opened, err := directory.Stat()
	after, pathErr := os.Lstat(absolute)
	if err != nil || pathErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) || !privateDirectory(opened) {
		directory.Close()
		return nil, nil, errors.New("lyrics recovery output parent changed while being opened")
	}
	return directory, opened, nil
}

func (directory *lockedRecoveryDirectory) unlock() {
	if directory != nil && directory.file != nil {
		_ = syscall.Flock(int(directory.file.Fd()), syscall.LOCK_UN)
	}
}

func (directory *lockedRecoveryDirectory) sync(name string) error {
	if err := directory.verifyDescriptor(); err != nil {
		return err
	}
	if err := runRecoveryFSHook(recoveryFSSync, name); err != nil {
		return err
	}
	if err := directory.file.Sync(); err != nil {
		return err
	}
	return directory.verifyPath()
}

func (directory *lockedRecoveryDirectory) verifyDescriptor() error {
	if directory == nil || directory.file == nil || directory.info == nil {
		return errors.New("lyrics recovery output parent is not open")
	}
	opened, err := directory.file.Stat()
	if err != nil || !os.SameFile(directory.info, opened) || !privateDirectory(opened) {
		return errors.New("lyrics recovery output parent inode changed")
	}
	return nil
}

func (directory *lockedRecoveryDirectory) verifyPath() error {
	if err := directory.verifyDescriptor(); err != nil {
		return err
	}
	current, err := os.Lstat(directory.path)
	if err != nil || !os.SameFile(directory.info, current) || !privateDirectory(current) {
		return errors.New("lyrics recovery output parent path changed")
	}
	return nil
}

func (directory *lockedRecoveryDirectory) close() {
	if directory != nil && directory.file != nil {
		_ = directory.file.Close()
		directory.file = nil
	}
}

func runRecoveryFSHook(operation, name string) error {
	if testHookBeforePublishFSOperation != nil {
		return testHookBeforePublishFSOperation(operation, name)
	}
	return nil
}

func recoveryStatAt(directory *os.File, name string) (recoveryFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return recoveryFileIdentity{}, err
	}
	return recoveryIdentityFromStat(&stat), nil
}

func recoveryIdentityForFile(file *os.File) (recoveryFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return recoveryFileIdentity{}, err
	}
	return recoveryIdentityFromStat(&stat), nil
}

func recoveryIdentityFromStat(stat *unix.Stat_t) recoveryFileIdentity {
	return recoveryFileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), mode: uint32(stat.Mode), uid: stat.Uid,
		links: uint64(stat.Nlink), size: stat.Size,
	}
}

func sameRecoveryIdentity(left, right recoveryFileIdentity) bool {
	return left.device == right.device && left.inode == right.inode
}

func sameRecoveryMetadata(left, right recoveryFileIdentity) bool {
	return left == right
}

func sameRecoveryContentIdentity(left, right recoveryFileIdentity) bool {
	return sameRecoveryIdentity(left, right) && left.mode == right.mode && left.uid == right.uid && left.size == right.size
}

func validRecoveryRegularIdentity(info recoveryFileIdentity, links uint64) bool {
	return info.mode&unix.S_IFMT == unix.S_IFREG && info.mode&0o777 == 0o600 && int(info.uid) == os.Geteuid() && info.links == links
}

func privateDirectory(info os.FileInfo) bool {
	owner, ok := privateOwner(info)
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o700 &&
		ok && int(owner) == os.Geteuid()
}

func privateRegular(info os.FileInfo, links uint64) bool {
	owner, ok := privateOwner(info)
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o600 &&
		ok && int(owner) == os.Geteuid() && privateLinkCount(info) == links
}

func privateOwner(info os.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func privateLinkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
