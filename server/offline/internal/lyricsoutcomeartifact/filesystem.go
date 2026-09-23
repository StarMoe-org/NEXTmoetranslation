package lyricsoutcomeartifact

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrAlreadyPublished = errors.New("provider outcome artifact is already published")

const (
	artifactFSCreate  = "create"
	artifactFSLink    = "link"
	artifactFSVerify  = "verify"
	artifactFSCleanup = "cleanup"
	artifactFSSync    = "sync"
)

// testHookBeforeFilesystemOperation is intentionally package-local. Tests use it
// to substitute the validated parent or a named entry exactly at syscall
// boundaries; production callers never install it.
var (
	testHookBeforeFilesystemOperation func(operation, name string) error
	testHookAfterLink                 func() error
)

type artifactFileIdentity struct {
	device uint64
	inode  uint64
	mode   uint32
	uid    uint32
	links  uint64
	size   int64
}

type artifactDirectory struct {
	path string
	file *os.File
	info os.FileInfo
}

func CreatePrivateDirectory(path string) (result error) {
	absolute, parentPath, name, err := splitArtifactPath(path, "provider outcome directory")
	if err != nil {
		return err
	}
	parent, err := openPrivateDirectory(parentPath)
	if err != nil {
		return err
	}
	defer parent.close()
	if err := parent.lock(); err != nil {
		return err
	}
	defer parent.unlock()
	if err := runArtifactFSHook(artifactFSCreate, name); err != nil {
		return err
	}
	if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyPublished
		}
		return err
	}
	created, err := artifactStatAt(parent.file, name)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if cleanupErr := removeArtifactDirectoryAt(parent, name, created); cleanupErr != nil {
			result = errors.Join(result, cleanupErr)
		}
	}()
	if err := unix.Fchmodat(int(parent.file.Fd()), name, 0o700, 0); err != nil {
		return err
	}
	secured, err := artifactStatAt(parent.file, name)
	if err != nil || !sameArtifactIdentity(created, secured) {
		return errors.Join(errors.New("provider outcome directory changed while securing its mode"), err)
	}
	created = secured
	if err := runArtifactFSHook(artifactFSVerify, name); err != nil {
		return err
	}
	if err := validateArtifactDirectoryIdentity(created); err != nil {
		return errors.New("provider outcome directory was not created privately")
	}
	opened, err := openArtifactDirectoryAt(parent.file, name)
	if err != nil {
		return err
	}
	defer opened.Close()
	openedIdentity, statErr := artifactIdentityForFile(opened)
	if statErr != nil || !sameArtifactMetadata(created, openedIdentity) {
		return errors.Join(errors.New("provider outcome directory creation did not bind its inode"), statErr)
	}
	if err := parent.sync(name); err != nil {
		return err
	}
	finalIdentity, err := artifactStatAt(parent.file, name)
	if err != nil || !sameArtifactMetadata(openedIdentity, finalIdentity) || validateArtifactDirectoryIdentity(finalIdentity) != nil {
		return errors.Join(errors.New("provider outcome directory changed before creation commit"), err)
	}
	if err := parent.verifyPath(); err != nil {
		return err
	}
	committed = true
	_ = absolute
	return nil
}

func PublishCreateExclusive(directory string, artifact Artifact) (string, error) {
	body, err := MarshalCanonical(artifact)
	if err != nil {
		return "", err
	}
	name, err := FileName(artifact)
	if err != nil {
		return "", err
	}
	private, err := openPrivateDirectory(directory)
	if err != nil {
		return "", err
	}
	defer private.close()
	if err := private.lock(); err != nil {
		return "", err
	}
	defer private.unlock()

	tempName := "." + name + ".tmp"
	finalInfo, finalErr := artifactStatAt(private.file, name)
	tempInfo, tempErr := artifactStatAt(private.file, tempName)
	if finalErr == nil {
		if tempErr == nil && sameArtifactMetadata(finalInfo, tempInfo) {
			if err := verifyArtifactFileAt(private, name, body, 2); err != nil {
				return "", err
			}
			if err := verifyArtifactFileAt(private, tempName, body, 2); err != nil {
				return "", err
			}
			if err := removeArtifactFileAt(private, tempName, tempInfo); err != nil {
				return "", err
			}
			if err := private.sync(tempName); err != nil {
				return "", err
			}
			if err := verifyArtifactFileAt(private, name, body, 1); err != nil {
				return "", err
			}
			if err := private.verifyPath(); err != nil {
				return "", err
			}
			return filepath.Join(private.path, name), nil
		}
		return "", ErrAlreadyPublished
	}
	if finalErr != nil && !errors.Is(finalErr, os.ErrNotExist) {
		return "", finalErr
	}
	var stagedInfo artifactFileIdentity
	if tempErr == nil {
		if err := verifyArtifactFileAt(private, tempName, body, 1); err != nil {
			return "", err
		}
		stagedInfo = tempInfo
	} else if errors.Is(tempErr, os.ErrNotExist) {
		stagedInfo, err = createArtifactStageAt(private, tempName, body)
		if err != nil {
			return "", err
		}
	} else {
		return "", tempErr
	}
	if err := runArtifactFSHook(artifactFSLink, name); err != nil {
		return "", err
	}
	if err := verifyArtifactFileAt(private, tempName, body, 1); err != nil {
		return "", err
	}
	currentStage, err := artifactStatAt(private.file, tempName)
	if err != nil || !sameArtifactMetadata(stagedInfo, currentStage) || validateArtifactRegularIdentity(currentStage, 1) != nil {
		return "", errors.Join(errors.New("provider outcome staged inode changed before linking"), err)
	}
	if err := unix.Linkat(int(private.file.Fd()), tempName, int(private.file.Fd()), name, 0); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", ErrAlreadyPublished
		}
		return "", err
	}
	publishedInfo, err := artifactStatAt(private.file, name)
	stagedAfterLink, stagedErr := artifactStatAt(private.file, tempName)
	if err != nil || stagedErr != nil || !sameArtifactContentIdentity(stagedInfo, publishedInfo) ||
		!sameArtifactMetadata(publishedInfo, stagedAfterLink) || publishedInfo.links != 2 {
		return "", errors.Join(errors.New("provider outcome publication did not bind the staged inode"), err, stagedErr)
	}
	if err := private.sync(name); err != nil {
		return "", err
	}
	if testHookAfterLink != nil {
		if err := testHookAfterLink(); err != nil {
			return "", err
		}
	}
	if err := removeArtifactFileAt(private, tempName, stagedAfterLink); err != nil {
		return "", err
	}
	if err := private.sync(tempName); err != nil {
		return "", err
	}
	if err := verifyArtifactFileAt(private, name, body, 1); err != nil {
		return "", err
	}
	if err := private.verifyPath(); err != nil {
		return "", err
	}
	return filepath.Join(private.path, name), nil
}

func Open(path string) (Artifact, error) {
	return OpenWithLinks(path, 1)
}

func OpenWithLinks(path string, links uint64) (Artifact, error) {
	_, parentPath, name, err := splitArtifactPath(path, "provider outcome artifact")
	if err != nil {
		return Artifact{}, err
	}
	parent, err := openPrivateDirectory(parentPath)
	if err != nil {
		return Artifact{}, err
	}
	defer parent.close()
	if err := parent.lock(); err != nil {
		return Artifact{}, err
	}
	defer parent.unlock()
	body, err := readArtifactFileAt(parent, name, links)
	if err != nil {
		return Artifact{}, err
	}
	artifact, err := DecodeCanonical(body)
	if err != nil {
		return Artifact{}, err
	}
	if err := parent.verifyPath(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func createArtifactStageAt(directory *artifactDirectory, name string, body []byte) (artifactFileIdentity, error) {
	if err := runArtifactFSHook(artifactFSCreate, name); err != nil {
		return artifactFileIdentity{}, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return artifactFileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return artifactFileIdentity{}, errors.New("provider outcome staging descriptor is invalid")
	}
	created := true
	defer func() {
		if created {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return artifactFileIdentity{}, err
	}
	if err := writeArtifactBytes(file, body); err != nil {
		return artifactFileIdentity{}, err
	}
	if err := file.Sync(); err != nil {
		return artifactFileIdentity{}, err
	}
	opened, err := artifactIdentityForFile(file)
	if err != nil || validateArtifactRegularIdentity(opened, 1) != nil || opened.size != int64(len(body)) {
		return artifactFileIdentity{}, errors.Join(errors.New("provider outcome staged file identity is invalid"), err)
	}
	if err := file.Close(); err != nil {
		return artifactFileIdentity{}, err
	}
	created = false
	if err := runArtifactFSHook(artifactFSVerify, name); err != nil {
		return artifactFileIdentity{}, err
	}
	named, err := artifactStatAt(directory.file, name)
	if err != nil || !sameArtifactMetadata(opened, named) {
		return artifactFileIdentity{}, errors.Join(errors.New("provider outcome staged filename changed after creation"), err)
	}
	if err := directory.sync(name); err != nil {
		return artifactFileIdentity{}, err
	}
	committed, err := artifactStatAt(directory.file, name)
	if err != nil || !sameArtifactMetadata(opened, committed) || validateArtifactRegularIdentity(committed, 1) != nil {
		return artifactFileIdentity{}, errors.Join(errors.New("provider outcome staged inode changed before commit"), err)
	}
	return opened, nil
}

func writeArtifactBytes(file *os.File, body []byte) error {
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

func verifyArtifactFileAt(directory *artifactDirectory, name string, expected []byte, links uint64) error {
	body, err := readArtifactFileAt(directory, name, links)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, expected) {
		return errors.New("provider outcome artifact bytes changed")
	}
	_, err = DecodeCanonical(body)
	return err
}

func readArtifactFileAt(directory *artifactDirectory, name string, links uint64) ([]byte, error) {
	if err := validArtifactName(name); err != nil {
		return nil, err
	}
	if err := directory.verifyDescriptor(); err != nil {
		return nil, err
	}
	before, err := artifactStatAt(directory.file, name)
	if err != nil || validateArtifactRegularIdentity(before, links) != nil || before.size <= 0 || before.size > MaxArtifactBytes {
		return nil, errors.New("provider outcome artifact file identity is invalid")
	}
	if err := runArtifactFSHook(artifactFSVerify, name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directory.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("provider outcome artifact descriptor is invalid")
	}
	defer file.Close()
	opened, err := artifactIdentityForFile(file)
	afterOpen, pathErr := artifactStatAt(directory.file, name)
	if err != nil || pathErr != nil || !sameArtifactMetadata(before, opened) || !sameArtifactMetadata(before, afterOpen) {
		return nil, errors.New("provider outcome artifact changed while being opened")
	}
	body, err := io.ReadAll(io.LimitReader(file, MaxArtifactBytes+1))
	if err != nil || len(body) > MaxArtifactBytes {
		return nil, errors.New("provider outcome artifact exceeds its file boundary")
	}
	after, statErr := artifactIdentityForFile(file)
	afterPath, pathStatErr := artifactStatAt(directory.file, name)
	if statErr != nil || pathStatErr != nil || !sameArtifactMetadata(before, after) || !sameArtifactMetadata(before, afterPath) ||
		after.size != int64(len(body)) || validateArtifactRegularIdentity(after, links) != nil {
		return nil, errors.New("provider outcome artifact changed while being read")
	}
	if err := directory.verifyPath(); err != nil {
		return nil, err
	}
	return body, nil
}

func removeArtifactFileAt(directory *artifactDirectory, name string, expected artifactFileIdentity) error {
	if err := runArtifactFSHook(artifactFSCleanup, name); err != nil {
		return err
	}
	current, err := artifactStatAt(directory.file, name)
	if err != nil || !sameArtifactMetadata(expected, current) {
		return errors.New("provider outcome staged file changed before cleanup")
	}
	if err := unix.Unlinkat(int(directory.file.Fd()), name, 0); err != nil {
		return err
	}
	return nil
}

func removeArtifactDirectoryAt(parent *artifactDirectory, name string, expected artifactFileIdentity) error {
	if err := runArtifactFSHook(artifactFSCleanup, name); err != nil {
		return err
	}
	current, err := artifactStatAt(parent.file, name)
	if err != nil || !sameArtifactMetadata(expected, current) {
		return errors.New("provider outcome directory changed before cleanup")
	}
	if err := unix.Unlinkat(int(parent.file.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.sync(name)
}

func splitArtifactPath(path, label string) (absolute, parent, name string, err error) {
	if path == "" || strings.TrimSpace(path) != path {
		return "", "", "", fmt.Errorf("%s path is invalid", label)
	}
	absolute, err = filepath.Abs(path)
	if err != nil {
		return "", "", "", err
	}
	name = filepath.Base(absolute)
	if err := validArtifactName(name); err != nil {
		return "", "", "", err
	}
	return absolute, filepath.Dir(absolute), name, nil
}

func validArtifactName(name string) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return errors.New("provider outcome filename is invalid")
	}
	return nil
}

func openPrivateDirectory(path string) (*artifactDirectory, error) {
	if path == "" || strings.TrimSpace(path) != path {
		return nil, errors.New("provider outcome directory path is invalid")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || resolved != absolute {
		return nil, errors.New("provider outcome directory must not be a symlink or alias")
	}
	before, err := os.Lstat(absolute)
	if err != nil || validatePrivateDirectory(before) != nil {
		return nil, errors.New("provider outcome directory must be effective-UID-owned mode-0700")
	}
	fd, err := unix.Open(absolute, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), absolute)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("provider outcome directory descriptor is invalid")
	}
	opened, err := file.Stat()
	after, pathErr := os.Lstat(absolute)
	if err != nil || pathErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) || validatePrivateDirectory(opened) != nil {
		file.Close()
		return nil, errors.New("provider outcome directory changed while being opened")
	}
	return &artifactDirectory{path: absolute, file: file, info: opened}, nil
}

func openArtifactDirectoryAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("provider outcome directory descriptor is invalid")
	}
	return file, nil
}

func (directory *artifactDirectory) lock() error {
	if err := directory.verifyDescriptor(); err != nil {
		return err
	}
	if err := syscall.Flock(int(directory.file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if err := directory.verifyPath(); err != nil {
		_ = syscall.Flock(int(directory.file.Fd()), syscall.LOCK_UN)
		return err
	}
	return nil
}

func (directory *artifactDirectory) unlock() {
	if directory != nil && directory.file != nil {
		_ = syscall.Flock(int(directory.file.Fd()), syscall.LOCK_UN)
	}
}

func (directory *artifactDirectory) sync(name string) error {
	if err := directory.verifyDescriptor(); err != nil {
		return err
	}
	if err := runArtifactFSHook(artifactFSSync, name); err != nil {
		return err
	}
	if err := directory.file.Sync(); err != nil {
		return err
	}
	return directory.verifyPath()
}

func (directory *artifactDirectory) verifyDescriptor() error {
	if directory == nil || directory.file == nil || directory.info == nil {
		return errors.New("provider outcome directory is not open")
	}
	opened, err := directory.file.Stat()
	if err != nil || !os.SameFile(directory.info, opened) || validatePrivateDirectory(opened) != nil {
		return errors.New("provider outcome directory inode changed")
	}
	return nil
}

func (directory *artifactDirectory) verifyPath() error {
	if err := directory.verifyDescriptor(); err != nil {
		return err
	}
	current, err := os.Lstat(directory.path)
	if err != nil || !os.SameFile(directory.info, current) || validatePrivateDirectory(current) != nil {
		return errors.New("provider outcome directory path changed")
	}
	return nil
}

func (directory *artifactDirectory) close() {
	if directory != nil && directory.file != nil {
		_ = directory.file.Close()
		directory.file = nil
	}
}

func runArtifactFSHook(operation, name string) error {
	if testHookBeforeFilesystemOperation != nil {
		return testHookBeforeFilesystemOperation(operation, name)
	}
	return nil
}

func artifactStatAt(directory *os.File, name string) (artifactFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return artifactFileIdentity{}, err
	}
	return artifactIdentityFromStat(&stat), nil
}

func artifactIdentityForFile(file *os.File) (artifactFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return artifactFileIdentity{}, err
	}
	return artifactIdentityFromStat(&stat), nil
}

func artifactIdentityFromStat(stat *unix.Stat_t) artifactFileIdentity {
	return artifactFileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), mode: uint32(stat.Mode), uid: stat.Uid,
		links: uint64(stat.Nlink), size: stat.Size,
	}
}

func sameArtifactIdentity(left, right artifactFileIdentity) bool {
	return left.device == right.device && left.inode == right.inode
}

func sameArtifactMetadata(left, right artifactFileIdentity) bool {
	return left == right
}

func sameArtifactContentIdentity(left, right artifactFileIdentity) bool {
	return sameArtifactIdentity(left, right) && left.mode == right.mode && left.uid == right.uid && left.size == right.size
}

func validateArtifactDirectoryIdentity(info artifactFileIdentity) error {
	if info.mode&unix.S_IFMT != unix.S_IFDIR || info.mode&0o777 != 0o700 || int(info.uid) != os.Geteuid() {
		return errors.New("directory is not private")
	}
	return nil
}

func validateArtifactRegularIdentity(info artifactFileIdentity, links uint64) error {
	if info.mode&unix.S_IFMT != unix.S_IFREG || info.mode&0o777 != 0o600 || int(info.uid) != os.Geteuid() || info.links != links {
		return fmt.Errorf("file is not a private regular file with %d links", links)
	}
	return nil
}

func validatePrivateDirectory(info os.FileInfo) error {
	owner, ok := fileOwner(info)
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 ||
		!ok || int(owner) != os.Geteuid() {
		return errors.New("directory is not private")
	}
	return nil
}

func validatePrivateRegular(info os.FileInfo, links uint64) error {
	owner, ok := fileOwner(info)
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
		!ok || int(owner) != os.Geteuid() || fileLinkCount(info) != links {
		return fmt.Errorf("file is not a private regular file with %d links", links)
	}
	return nil
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
