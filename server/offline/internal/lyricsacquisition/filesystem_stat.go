package lyricsacquisition

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func validateAncestorStat(stat trustedStat) error {
	if stat.mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("lyrics acquisition spool ancestry is not a directory chain")
	}
	if stat.owner != 0 && int(stat.owner) != os.Geteuid() {
		return errors.New("lyrics acquisition spool ancestry is not owned by root or the effective UID")
	}
	if stat.mode&0o022 != 0 && stat.mode&unix.S_ISVTX == 0 {
		return errors.New("lyrics acquisition spool ancestry is writable by an untrusted local UID")
	}
	return nil
}

func validatePrivateParentStat(stat trustedStat) error {
	if stat.mode&unix.S_IFMT != unix.S_IFDIR || int(stat.owner) != os.Geteuid() || stat.mode&0o022 != 0 {
		return errors.New("lyrics acquisition spool parent must be effective-UID-owned and not group/other-writable")
	}
	return nil
}

func validatePrivateDirectoryStat(stat trustedStat, label string) error {
	if stat.mode&unix.S_IFMT != unix.S_IFDIR || stat.mode&0o7777 != 0o700 || int(stat.owner) != os.Geteuid() {
		return fmt.Errorf("%s must be a direct effective-UID-owned mode-0700 directory", label)
	}
	return nil
}

func validatePrivateDirectoryInfo(info os.FileInfo, label string) error {
	stat, ok := trustedStatFromFileInfo(info)
	if !ok {
		return fmt.Errorf("%s has no supported filesystem identity", label)
	}
	return validatePrivateDirectoryStat(stat, label)
}

func validatePrivateRegularStat(stat trustedStat, label string, allowedLinks ...uint64) error {
	if stat.mode&unix.S_IFMT != unix.S_IFREG || stat.mode&0o7777 != 0o600 || int(stat.owner) != os.Geteuid() {
		return fmt.Errorf("%s must be a direct effective-UID-owned mode-0600 regular file", label)
	}
	allowed := len(allowedLinks) == 0
	for _, count := range allowedLinks {
		allowed = allowed || stat.links == count
	}
	if !allowed {
		return fmt.Errorf("%s has an invalid hard-link count", label)
	}
	return nil
}

func validatePrivateRegularInfo(info os.FileInfo, label string, allowedLinks ...uint64) error {
	stat, ok := trustedStatFromFileInfo(info)
	if !ok {
		return fmt.Errorf("%s has no supported filesystem identity", label)
	}
	return validatePrivateRegularStat(stat, label, allowedLinks...)
}

func fileLinkCount(info os.FileInfo) uint64 {
	stat, ok := trustedStatFromFileInfo(info)
	if !ok {
		return 0
	}
	return stat.links
}

func fileOwner(info os.FileInfo) (uint32, bool) {
	stat, ok := trustedStatFromFileInfo(info)
	return stat.owner, ok
}

func trustedStatFromFileInfo(info os.FileInfo) (trustedStat, bool) {
	if info == nil {
		return trustedStat{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return trustedStat{}, false
	}
	return trustedStat{
		identity: fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)},
		mode:     uint32(stat.Mode), links: uint64(stat.Nlink), owner: stat.Uid, size: stat.Size,
	}, true
}

func trustedStatFromUnix(stat *unix.Stat_t) trustedStat {
	return trustedStat{
		identity: fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)},
		mode:     uint32(stat.Mode), links: uint64(stat.Nlink), owner: stat.Uid, size: stat.Size,
	}
}

func sameFileIdentity(left, right fileIdentity) bool {
	return left.device == right.device && left.inode == right.inode
}

func fstatFile(file *os.File) (trustedStat, error) {
	if file == nil {
		return trustedStat{}, errors.New("filesystem descriptor is required")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return trustedStat{}, err
	}
	return trustedStatFromUnix(&stat), nil
}

func statAt(directory *os.File, name string) (trustedStat, error) {
	if directory == nil {
		return trustedStat{}, errors.New("directory descriptor is required")
	}
	if err := validateLeafName(name); err != nil {
		return trustedStat{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return trustedStat{}, err
	}
	return trustedStatFromUnix(&stat), nil
}

func openDirectoryAt(parent *os.File, name string) (*os.File, trustedStat, error) {
	before, err := statAt(parent, name)
	if err != nil {
		return nil, trustedStat{}, err
	}
	if before.mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, trustedStat{}, errors.New("descriptor-relative directory target is not a direct directory")
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, trustedStat{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	opened, err := fstatFile(file)
	if err != nil {
		_ = file.Close()
		return nil, trustedStat{}, err
	}
	after, err := statAt(parent, name)
	if err != nil || !sameFileIdentity(before.identity, opened.identity) || !sameFileIdentity(before.identity, after.identity) {
		_ = file.Close()
		return nil, trustedStat{}, errors.New("descriptor-relative directory changed while being opened")
	}
	return file, opened, nil
}

func validateLeafName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\") || strings.IndexByte(name, 0) >= 0 {
		return errors.New("filesystem leaf name is invalid or aliased")
	}
	return nil
}

func (root *privateRoot) directoryFile(name string) (*os.File, error) {
	if name == "" {
		if err := root.verify(); err != nil {
			return nil, err
		}
		return root.file, nil
	}
	if err := root.verifyDirectory(name); err != nil {
		return nil, err
	}
	return root.directories[name].file, nil
}

func leafIdentityKey(directory, name string) string {
	return directory + "\x00" + name
}

func (root *privateRoot) rememberLeaf(directory, name string, identity fileIdentity) {
	root.knownLeaves[leafIdentityKey(directory, name)] = identity
}

func (root *privateRoot) forgetLeaf(directory, name string) {
	delete(root.knownLeaves, leafIdentityKey(directory, name))
}

func (root *privateRoot) verifyKnownLeaf(directory, name string, identity fileIdentity) error {
	known, found := root.knownLeaves[leafIdentityKey(directory, name)]
	if !found {
		return fmt.Errorf("lyrics acquisition spool %s/%s appeared after validation", directory, name)
	}
	if !sameFileIdentity(known, identity) {
		return fmt.Errorf("lyrics acquisition spool %s/%s was replaced after validation", directory, name)
	}
	return nil
}

func (root *privateRoot) captureDirectoryLeaves(directory string, maximum int) error {
	entries, err := sortedDirectoryEntries(root, directory, maximum)
	if err != nil {
		return err
	}
	file, err := root.directoryFile(directory)
	if err != nil {
		return err
	}
	alreadyCaptured := root.capturedDirectories[directory]
	for _, entry := range entries {
		stat, err := statAt(file, entry.Name())
		if err != nil {
			return err
		}
		if err := validatePrivateRegularStat(stat, "reviewed lyrics acquisition spool leaf", 1, 2); err != nil {
			return err
		}
		key := leafIdentityKey(directory, entry.Name())
		if _, known := root.knownLeaves[key]; known {
			if err := root.verifyKnownLeaf(directory, entry.Name(), stat.identity); err != nil {
				return err
			}
			continue
		}
		if alreadyCaptured {
			return fmt.Errorf("lyrics acquisition spool %s/%s appeared after directory validation", directory, entry.Name())
		}
		root.rememberLeaf(directory, entry.Name(), stat.identity)
	}
	root.capturedDirectories[directory] = true
	return nil
}

func readVerifiedFileAt(root *privateRoot, directory, name, label, expectedSHA string, expectedBytes, maximum int, allowedLinks ...uint64) ([]byte, os.FileInfo, error) {
	directoryFile, err := root.directoryFile(directory)
	if err != nil {
		return nil, nil, err
	}
	before, err := statAt(directoryFile, name)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect %s: %w", label, err)
	}
	if err := root.verifyKnownLeaf(directory, name, before.identity); err != nil {
		return nil, nil, err
	}
	if err := validatePrivateRegularStat(before, label, allowedLinks...); err != nil {
		return nil, nil, err
	}
	if before.size < 0 || before.size > int64(maximum) || expectedBytes >= 0 && before.size != int64(expectedBytes) {
		return nil, nil, fmt.Errorf("%s has an invalid byte count", label)
	}
	fd, err := unix.Openat(int(directoryFile.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), label)
	defer file.Close()
	openedStat, err := fstatFile(file)
	pathAfterOpen, pathErr := statAt(directoryFile, name)
	if err != nil || pathErr != nil || !sameFileIdentity(before.identity, openedStat.identity) || !sameFileIdentity(before.identity, pathAfterOpen.identity) {
		return nil, nil, fmt.Errorf("%s path or inode changed while being opened", label)
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(body) > maximum || expectedBytes >= 0 && len(body) != expectedBytes {
		return nil, nil, fmt.Errorf("%s exceeds or disagrees with its byte bound", label)
	}
	afterStat, statErr := fstatFile(file)
	pathAfterRead, pathErr := statAt(directoryFile, name)
	if statErr != nil || pathErr != nil || !sameFileIdentity(before.identity, afterStat.identity) ||
		!sameFileIdentity(before.identity, pathAfterRead.identity) || afterStat.size != int64(len(body)) {
		return nil, nil, fmt.Errorf("%s path, inode, or size changed while being read", label)
	}
	if err := validatePrivateRegularStat(afterStat, label, allowedLinks...); err != nil {
		return nil, nil, err
	}
	if expectedSHA != "" && sha256Hex(body) != expectedSHA {
		return nil, nil, fmt.Errorf("%s SHA-256 does not match its content address", label)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	return body, info, nil
}
