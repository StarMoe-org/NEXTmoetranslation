package lyricsacquisition

import (
	"bytes"
	"errors"
	"fmt"

	"os"

	"golang.org/x/sys/unix"
)

func (root *privateRoot) acquireLedgerLock(initialize bool) error {
	if root == nil || root.file == nil {
		return errors.New("lyrics acquisition spool root is not open")
	}
	if err := root.verifyBase(); err != nil {
		return err
	}
	if err := unix.Flock(int(root.file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return errors.New("lyrics acquisition ledger root is already locked by another process")
		}
		return fmt.Errorf("acquire lyrics acquisition ledger-root directory lock: %w", err)
	}
	root.rootLocked = true
	var (
		pathBeforeOpen trustedStat
		err            error
	)
	if !initialize {
		pathBeforeOpen, err = statAt(root.file, ledgerLockName)
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("existing lyrics acquisition spool has no recognized retained lock namespace")
		}
		if err != nil {
			return fmt.Errorf("inspect lyrics acquisition ledger-root lock before open: %w", err)
		}
		if err := validatePrivateRegularStat(pathBeforeOpen, "lyrics acquisition ledger-root lock", 1); err != nil {
			return err
		}
		if pathBeforeOpen.size != int64(len(ledgerLockBody)) {
			return errors.New("lyrics acquisition ledger-root lock identity has an invalid byte count")
		}
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if initialize {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(int(root.file.Fd()), ledgerLockName, flags, 0o600)
	if err != nil {
		return fmt.Errorf("open lyrics acquisition ledger-root lock: %w", err)
	}
	lockFile := os.NewFile(uintptr(fd), "lyrics-acquisition-ledger-root-lock")
	fail := func(cause error) error {
		_ = lockFile.Close()
		return cause
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fail(errors.New("lyrics acquisition ledger root is already locked by another process"))
		}
		return fail(fmt.Errorf("acquire lyrics acquisition ledger-root lock: %w", err))
	}
	if initialize {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			return fail(fmt.Errorf("secure lyrics acquisition ledger-root lock: %w", err))
		}
		if err := lockFile.Truncate(0); err != nil {
			return fail(fmt.Errorf("initialize lyrics acquisition ledger-root lock: %w", err))
		}
		if _, err := lockFile.WriteAt(ledgerLockBody, 0); err != nil {
			return fail(fmt.Errorf("write lyrics acquisition ledger-root lock identity: %w", err))
		}
		if err := lockFile.Sync(); err != nil {
			return fail(fmt.Errorf("sync lyrics acquisition ledger-root lock identity: %w", err))
		}
		if err := root.file.Sync(); err != nil {
			return fail(fmt.Errorf("sync lyrics acquisition ledger-root lock namespace: %w", err))
		}
	}
	lockStat, err := fstatFile(lockFile)
	if err != nil {
		return fail(err)
	}
	if !initialize && !sameFileIdentity(pathBeforeOpen.identity, lockStat.identity) {
		return fail(errors.New("lyrics acquisition ledger-root lock pathname or inode changed while being opened"))
	}
	root.lockFile = lockFile
	root.lockStat = lockStat
	if err := root.verifyLock(); err != nil {
		root.lockFile = nil
		return fail(err)
	}
	return nil
}

func (root *privateRoot) ensureDirectories(initializeMissing bool) error {
	if err := root.verify(); err != nil {
		return err
	}
	for _, name := range []string{blobsDirectory, manifestsDirectory, pendingDirectory} {
		if err := root.ensureDirectory(name, initializeMissing); err != nil {
			return err
		}
	}
	for _, optional := range []string{quarantineDirectory, metadataStateDirectory} {
		if initializeMissing {
			if err := root.ensureDirectory(optional, true); err != nil {
				return err
			}
			continue
		}
		if _, err := statAt(root.file, optional); err == nil {
			if err := root.ensureDirectory(optional, false); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect lyrics acquisition spool %s directory: %w", optional, err)
		}
	}
	return root.verify()
}

func (root *privateRoot) ensureQuarantineDirectory() error {
	if _, found := root.directories[quarantineDirectory]; found {
		return root.verifyDirectory(quarantineDirectory)
	}
	return root.ensureDirectory(quarantineDirectory, true)
}

func (root *privateRoot) ensureMetadataStateDirectory() error {
	if _, found := root.directories[metadataStateDirectory]; found {
		return root.verifyDirectory(metadataStateDirectory)
	}
	return root.ensureDirectory(metadataStateDirectory, true)
}

func (root *privateRoot) ensureDirectory(name string, allowCreate bool) error {
	createdDirectory := false
	stat, err := statAt(root.file, name)
	if errors.Is(err, os.ErrNotExist) {
		if !allowCreate {
			return fmt.Errorf("existing lyrics acquisition spool is missing required %s directory", name)
		}
		if err := unix.Mkdirat(int(root.file.Fd()), name, 0o700); err != nil {
			return fmt.Errorf("create lyrics acquisition spool %s directory: %w", name, err)
		}
		createdDirectory = true
		if err := root.file.Sync(); err != nil {
			return fmt.Errorf("sync lyrics acquisition spool %s directory creation: %w", name, err)
		}
		stat, err = statAt(root.file, name)
	}
	if err != nil {
		return fmt.Errorf("inspect lyrics acquisition spool %s directory: %w", name, err)
	}
	if !createdDirectory {
		if err := validatePrivateDirectoryStat(stat, "lyrics acquisition spool "+name+" directory"); err != nil {
			return err
		}
	}
	directory, openedStat, err := openDirectoryAt(root.file, name)
	if err != nil {
		return fmt.Errorf("pin lyrics acquisition spool %s directory: %w", name, err)
	}
	if createdDirectory {
		if err := unix.Fchmod(int(directory.Fd()), 0o700); err != nil {
			_ = directory.Close()
			return fmt.Errorf("secure lyrics acquisition spool %s directory: %w", name, err)
		}
		openedStat, err = fstatFile(directory)
		if err != nil {
			_ = directory.Close()
			return err
		}
	}
	if !sameFileIdentity(stat.identity, openedStat.identity) {
		_ = directory.Close()
		return fmt.Errorf("lyrics acquisition spool %s directory changed while being pinned", name)
	}
	if err := validatePrivateDirectoryStat(openedStat, "lyrics acquisition spool "+name+" directory"); err != nil {
		_ = directory.Close()
		return err
	}
	root.directories[name] = pinnedDirectory{file: directory, stat: openedStat}
	return nil
}

func (root *privateRoot) verifyBase() error {
	if root == nil || root.file == nil || len(root.ancestors) == 0 || root.parentFile == nil {
		return errors.New("lyrics acquisition spool root is not open")
	}
	for index, component := range root.ancestors {
		opened, err := fstatFile(component.file)
		if err != nil || !sameFileIdentity(component.stat.identity, opened.identity) {
			return errors.New("lyrics acquisition spool ancestor descriptor changed")
		}
		if err := validateAncestorStat(opened); err != nil {
			return err
		}
		if index == 0 {
			continue
		}
		pathStat, err := statAt(root.ancestors[index-1].file, component.name)
		if err != nil || !sameFileIdentity(component.stat.identity, pathStat.identity) {
			return errors.New("lyrics acquisition spool ancestor pathname changed")
		}
	}
	opened, err := fstatFile(root.file)
	if err != nil || !sameFileIdentity(root.stat.identity, opened.identity) {
		return errors.New("lyrics acquisition spool root descriptor changed")
	}
	pathStat, err := statAt(root.parentFile, root.name)
	if err != nil || !sameFileIdentity(root.stat.identity, pathStat.identity) {
		return errors.New("lyrics acquisition spool root pathname or inode changed")
	}
	return validatePrivateDirectoryStat(opened, "lyrics acquisition spool root")
}

func (root *privateRoot) verify() error {
	if err := root.verifyBase(); err != nil {
		return err
	}
	return root.verifyLock()
}

func (root *privateRoot) verifyLock() error {
	if root == nil || root.lockFile == nil {
		return errors.New("lyrics acquisition ledger-root lock is not retained")
	}
	opened, err := fstatFile(root.lockFile)
	if err != nil || !sameFileIdentity(root.lockStat.identity, opened.identity) {
		return errors.New("lyrics acquisition ledger-root lock descriptor changed")
	}
	pathStat, err := statAt(root.file, ledgerLockName)
	if err != nil || !sameFileIdentity(root.lockStat.identity, pathStat.identity) {
		return errors.New("lyrics acquisition ledger-root lock pathname or inode changed")
	}
	if err := validatePrivateRegularStat(opened, "lyrics acquisition ledger-root lock", 1); err != nil {
		return err
	}
	if opened.size != int64(len(ledgerLockBody)) {
		return errors.New("lyrics acquisition ledger-root lock identity has an invalid byte count")
	}
	body := make([]byte, len(ledgerLockBody))
	if _, err := root.lockFile.ReadAt(body, 0); err != nil {
		return fmt.Errorf("read lyrics acquisition ledger-root lock identity: %w", err)
	}
	if !bytes.Equal(body, ledgerLockBody) {
		return errors.New("lyrics acquisition ledger-root lock identity is unrecognized")
	}
	return nil
}

func (root *privateRoot) verifyDirectory(name string) error {
	if err := root.verify(); err != nil {
		return err
	}
	pinned, found := root.directories[name]
	if !found || pinned.file == nil {
		return fmt.Errorf("lyrics acquisition spool %s directory is not pinned", name)
	}
	opened, err := fstatFile(pinned.file)
	if err != nil || !sameFileIdentity(pinned.stat.identity, opened.identity) {
		return fmt.Errorf("lyrics acquisition spool %s directory descriptor changed", name)
	}
	pathStat, err := statAt(root.file, name)
	if err != nil || !sameFileIdentity(pinned.stat.identity, pathStat.identity) {
		return fmt.Errorf("lyrics acquisition spool %s directory pathname or inode changed", name)
	}
	return validatePrivateDirectoryStat(opened, "lyrics acquisition spool "+name+" directory")
}

func (root *privateRoot) syncDirectory(name string) error {
	if err := root.verifyDirectory(name); err != nil {
		return err
	}
	if err := root.directories[name].file.Sync(); err != nil {
		return fmt.Errorf("sync lyrics acquisition spool %s directory: %w", name, err)
	}
	return root.verifyDirectory(name)
}

func (root *privateRoot) Close() error {
	if root == nil {
		return nil
	}
	var result error
	for name, pinned := range root.directories {
		if pinned.file != nil {
			result = errors.Join(result, pinned.file.Close())
		}
		delete(root.directories, name)
	}
	if root.lockFile != nil {
		result = errors.Join(result, root.lockFile.Close())
		root.lockFile = nil
	}
	for index := len(root.ancestors) - 1; index >= 0; index-- {
		if root.ancestors[index].file != nil && root.ancestors[index].file != root.file {
			result = errors.Join(result, root.ancestors[index].file.Close())
			root.ancestors[index].file = nil
		}
	}
	root.parentFile = nil
	if root.file != nil {
		if root.rootLocked {
			result = errors.Join(result, unix.Flock(int(root.file.Fd()), unix.LOCK_UN))
			root.rootLocked = false
		}
		result = errors.Join(result, root.file.Close())
		root.file = nil
	}
	return result
}
