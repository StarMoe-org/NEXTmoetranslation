package lyricsacquisition

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"sort"

	"golang.org/x/sys/unix"
)

func writeStagedFile(root *privateRoot, name string, body []byte) (os.FileInfo, error) {
	return writeStagedFileAt(root, pendingDirectory, name, body)
}

func writeStagedFileAt(root *privateRoot, directory, name string, body []byte) (os.FileInfo, error) {
	if err := requireAtomicNamespacePublication(); err != nil {
		return nil, err
	}
	if err := validateLeafName(name); err != nil {
		return nil, err
	}
	if err := root.verifyDirectory(directory); err != nil {
		return nil, err
	}
	directoryFile := root.directories[directory].file
	fd, err := unix.Openat(int(directoryFile.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if errors.Is(err, os.ErrExist) {
		existing, info, readErr := readVerifiedFileAt(root, directory, name, "existing staged acquisition file", sha256Hex(body), len(body), len(body), 1, 2)
		if readErr != nil {
			return nil, readErr
		}
		if !bytes.Equal(existing, body) {
			return nil, errors.New("existing staged acquisition file has conflicting bytes")
		}
		return info, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create staged acquisition file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("staged acquisition file descriptor is invalid")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return nil, fmt.Errorf("secure staged acquisition file: %w", err)
	}
	if _, err := file.Write(body); err != nil {
		return nil, fmt.Errorf("write staged acquisition file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync staged acquisition file: %w", err)
	}
	stat, err := fstatFile(file)
	if err != nil {
		return nil, fmt.Errorf("inspect staged acquisition file: %w", err)
	}
	if err := validatePrivateRegularStat(stat, "staged acquisition file", 1); err != nil {
		return nil, err
	}
	if stat.size != int64(len(body)) {
		return nil, errors.New("staged acquisition file byte count changed")
	}
	pathStat, err := statAt(directoryFile, name)
	if err != nil || !sameTrustedMetadata(stat, pathStat) {
		return nil, errors.Join(errors.New("staged acquisition file pathname changed before publication"), err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	root.rememberLeaf(directory, name, stat.identity)
	if err := root.syncDirectory(directory); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close staged acquisition file: %w", err)
	}
	closed = true
	return info, nil
}

func publishNoOverwrite(root *privateRoot, stagedName, targetDirectory, targetName, label, expectedSHA string, expectedBytes, maximum int) (os.FileInfo, error) {
	return publishNoOverwriteAt(root, pendingDirectory, stagedName, targetDirectory, targetName, label, expectedSHA, expectedBytes, maximum)
}

func publishNoOverwriteAt(root *privateRoot, sourceDirectory, stagedName, targetDirectory, targetName, label, expectedSHA string, expectedBytes, maximum int) (os.FileInfo, error) {
	if err := requireAtomicNamespacePublication(); err != nil {
		return nil, err
	}
	if err := validateLeafName(stagedName); err != nil {
		return nil, err
	}
	if err := validateLeafName(targetName); err != nil {
		return nil, err
	}
	if err := root.verifyDirectory(sourceDirectory); err != nil {
		return nil, err
	}
	if err := root.verifyDirectory(targetDirectory); err != nil {
		return nil, err
	}
	sourceDirectoryFile := root.directories[sourceDirectory].file
	targetDirectoryFile := root.directories[targetDirectory].file
	for attempt := 0; attempt < 2; attempt++ {
		targetStat, targetErr := statAt(targetDirectoryFile, targetName)
		if targetErr == nil {
			if err := root.verifyKnownLeaf(targetDirectory, targetName, targetStat.identity); err != nil {
				return nil, err
			}
			_, targetInfo, err := readVerifiedFileAt(root, targetDirectory, targetName, label, expectedSHA, expectedBytes, maximum, 1, 2)
			if err != nil {
				return nil, err
			}
			if stagedStat, stageErr := statAt(sourceDirectoryFile, stagedName); stageErr == nil {
				if err := root.verifyKnownLeaf(sourceDirectory, stagedName, stagedStat.identity); err != nil {
					return nil, err
				}
				if _, _, err := readVerifiedFileAt(root, sourceDirectory, stagedName, "retained staged "+label, expectedSHA, expectedBytes, maximum, 1, 2); err != nil {
					return nil, err
				}
			} else if !errors.Is(stageErr, os.ErrNotExist) {
				return nil, fmt.Errorf("inspect staged %s: %w", label, stageErr)
			}
			return targetInfo, nil
		}
		if !errors.Is(targetErr, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect %s publication path: %w", label, targetErr)
		}
		_, stagedInfo, err := readVerifiedFileAt(root, sourceDirectory, stagedName, "staged "+label, expectedSHA, expectedBytes, maximum, 1)
		if err != nil {
			return nil, err
		}
		stagedIdentity, ok := fileIdentityFromFileInfo(stagedInfo)
		if !ok {
			return nil, errors.New("staged publication has no supported filesystem identity")
		}
		fd, err := unix.Openat(int(sourceDirectoryFile.Fd()), stagedName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open staged %s descriptor for publication: %w", label, err)
		}
		source := os.NewFile(uintptr(fd), "lyrics-acquisition-publication-source")
		if source == nil {
			_ = unix.Close(fd)
			return nil, errors.New("descriptor-bound publication source is invalid")
		}
		opened, openErr := fstatFile(source)
		pathOpened, pathErr := statAt(sourceDirectoryFile, stagedName)
		if openErr != nil || pathErr != nil || !sameFileIdentity(stagedIdentity, opened.identity) || !sameTrustedMetadata(opened, pathOpened) {
			_ = source.Close()
			return nil, errors.Join(errors.New("staged publication changed while its descriptor was pinned"), openErr, pathErr)
		}
		if testHookBeforeAtomicNoReplacePublish != nil {
			if err := testHookBeforeAtomicNoReplacePublish(); err != nil {
				_ = source.Close()
				return nil, err
			}
		}
		publishErr := atomicPublishDescriptorNoReplaceAt(source, targetDirectoryFile, targetName)
		openedAfter, openedErr := fstatFile(source)
		closeErr := source.Close()
		if errors.Is(publishErr, os.ErrExist) {
			if openedErr != nil || closeErr != nil || !sameTrustedMetadata(opened, openedAfter) {
				return nil, errors.Join(errors.New("descriptor publication source changed on destination collision"), openedErr, closeErr)
			}
			continue
		}
		if publishErr != nil {
			return nil, errors.Join(fmt.Errorf("publish %s from its pinned descriptor without overwrite: %w", label, publishErr), openedErr, closeErr)
		}
		if openedErr != nil || closeErr != nil || !sameFileIdentity(opened.identity, openedAfter.identity) || openedAfter.size != int64(expectedBytes) {
			return nil, errors.Join(errors.New("descriptor publication source changed during publication"), openedErr, closeErr)
		}
		publishedStat, publishedErr := statAt(targetDirectoryFile, targetName)
		if publishedErr != nil {
			return nil, fmt.Errorf("inspect descriptor-published %s: %w", label, publishedErr)
		}
		if err := validatePrivateRegularStat(publishedStat, "descriptor-published "+label, 1, 2); err != nil {
			return nil, err
		}
		root.rememberLeaf(targetDirectory, targetName, publishedStat.identity)
		if err := root.syncDirectory(targetDirectory); err != nil {
			return nil, err
		}
		_, publishedInfo, readErr := readVerifiedFileAt(root, targetDirectory, targetName, label, expectedSHA, expectedBytes, maximum, 1, 2)
		sourcePathStat, sourcePathErr := statAt(sourceDirectoryFile, stagedName)
		if sourcePathErr == nil && sameFileIdentity(stagedIdentity, sourcePathStat.identity) {
			root.rememberLeaf(sourceDirectory, stagedName, sourcePathStat.identity)
		}
		if readErr != nil {
			return nil, readErr
		}
		if sourcePathErr != nil || !sameFileIdentity(stagedIdentity, sourcePathStat.identity) {
			return nil, errors.Join(fmt.Errorf("%s source pathname changed at the descriptor publication boundary; verified target retained", label), sourcePathErr)
		}
		return publishedInfo, nil
	}
	return nil, fmt.Errorf("%s publication path changed repeatedly; staged descriptor retained", label)
}

func retireOwnedFile(root *privateRoot, directory, name string, owned os.FileInfo, allowedLinks ...uint64) error {
	if owned == nil {
		return errors.New("owned file identity is required for retention")
	}
	ownedIdentity, ok := fileIdentityFromFileInfo(owned)
	if !ok {
		return errors.New("owned file has no supported filesystem identity")
	}
	sourceDirectory, err := root.directoryFile(directory)
	if err != nil {
		return err
	}
	current, err := statAt(sourceDirectory, name)
	if err != nil {
		return fmt.Errorf("inspect owned spool file before bounded retention: %w", err)
	}
	if err := root.verifyKnownLeaf(directory, name, current.identity); err != nil {
		return err
	}
	if !sameFileIdentity(ownedIdentity, current.identity) {
		return errors.New("owned spool file pathname was replaced before bounded retention")
	}
	if err := validatePrivateRegularStat(current, "owned spool file", allowedLinks...); err != nil {
		return err
	}
	fd, err := unix.Openat(int(sourceDirectory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open owned spool file for bounded retention: %w", err)
	}
	file := os.NewFile(uintptr(fd), "lyrics-acquisition-retained-file")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("owned spool retention descriptor is invalid")
	}
	defer file.Close()
	opened, err := fstatFile(file)
	pathOpened, pathErr := statAt(sourceDirectory, name)
	if err != nil || pathErr != nil || !sameTrustedMetadata(current, opened) || !sameTrustedMetadata(current, pathOpened) {
		return errors.Join(errors.New("owned spool file changed while being pinned for retention"), err, pathErr)
	}
	if testHookBeforeOwnedFileRetire != nil {
		if err := testHookBeforeOwnedFileRetire(); err != nil {
			return err
		}
	}
	openedAfter, openedErr := fstatFile(file)
	pathAfter, pathAfterErr := statAt(sourceDirectory, name)
	if openedErr != nil || pathAfterErr != nil || !sameTrustedMetadata(opened, openedAfter) || !sameTrustedMetadata(opened, pathAfter) {
		return errors.Join(errors.New("owned spool pathname changed at the bounded retention boundary; replacement retained untouched"), openedErr, pathAfterErr)
	}
	root.rememberLeaf(directory, name, pathAfter.identity)
	return nil
}

func sortedDirectoryEntries(root *privateRoot, directory string, maximum int) ([]os.DirEntry, error) {
	if maximum < 0 {
		return nil, errors.New("lyrics acquisition spool directory entry bound is invalid")
	}
	directoryFile, err := root.directoryFile(directory)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(directoryFile.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	copyFile := os.NewFile(uintptr(fd), "lyrics-acquisition-directory-snapshot")
	entries, readErr := copyFile.ReadDir(maximum + 1)
	closeErr := copyFile.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > maximum {
		return nil, errors.New("lyrics acquisition spool directory exceeds its entry bound")
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	if directory == "" {
		if err := root.verify(); err != nil {
			return nil, err
		}
	} else if err := root.verifyDirectory(directory); err != nil {
		return nil, err
	}
	return entries, nil
}

func fileIdentityFromFileInfo(info os.FileInfo) (fileIdentity, bool) {
	stat, ok := trustedStatFromFileInfo(info)
	return stat.identity, ok
}
