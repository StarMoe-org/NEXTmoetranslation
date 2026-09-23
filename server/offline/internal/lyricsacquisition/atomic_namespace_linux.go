//go:build linux

package lyricsacquisition

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const (
	atomicPublicationProbeDirectoryName = ".moesekai-linkat-capability-v1"
	atomicPublicationProbeTargetName    = "descriptor-source-linkat.published"
)

var (
	errAtomicNamespacePublicationUnsupported = errors.New("HOLD: descriptor-bound lyrics acquisition publication is unsupported by this Linux filesystem")
	atomicPublicationProbeBody               = []byte("moesekai-linux-descriptor-source-linkat-capability-v1\n")
)

func requireAtomicNamespacePublication() error {
	return nil
}

func preflightAtomicNamespacePublication(directory *os.File) error {
	if directory == nil {
		return errors.New("descriptor publication preflight requires a directory descriptor")
	}
	probe, probeStat, created, err := openAtomicPublicationProbeDirectory(directory, atomicPublicationProbeDirectoryName)
	if err != nil {
		return fmt.Errorf("%w: %v", errAtomicNamespacePublicationUnsupported, err)
	}
	defer probe.Close()
	if !created {
		if err := verifyAtomicNamespacePublicationProbe(directory, probe, probeStat); err != nil {
			return fmt.Errorf("%w: retained Linkat probe: %v", errAtomicNamespacePublicationUnsupported, err)
		}
		return nil
	}

	fd, err := unix.Openat(int(probe.Fd()), ".", unix.O_RDWR|unix.O_TMPFILE|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("%w: O_TMPFILE probe: %v", errAtomicNamespacePublicationUnsupported, err)
	}
	source := os.NewFile(uintptr(fd), "linux-descriptor-source-linkat-capability")
	if source == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("%w: O_TMPFILE probe descriptor is invalid", errAtomicNamespacePublicationUnsupported)
	}
	defer source.Close()
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fmt.Errorf("secure descriptor publication probe source: %w", err)
	}
	for offset := 0; offset < len(atomicPublicationProbeBody); {
		written, writeErr := source.WriteAt(atomicPublicationProbeBody[offset:], int64(offset))
		if written <= 0 || written > len(atomicPublicationProbeBody)-offset {
			return errors.Join(errors.New("descriptor publication probe source write returned an invalid byte count"), writeErr)
		}
		offset += written
		if writeErr != nil {
			return fmt.Errorf("write descriptor publication probe source: %w", writeErr)
		}
	}
	if err := source.Sync(); err != nil {
		return fmt.Errorf("sync descriptor publication probe source: %w", err)
	}
	before, err := fstatFile(source)
	if err != nil {
		return fmt.Errorf("inspect descriptor publication probe source: %w", err)
	}
	if err := validatePrivateRegularStat(before, "descriptor publication probe source", 0); err != nil {
		return err
	}
	if before.size != int64(len(atomicPublicationProbeBody)) {
		return errors.New("descriptor publication probe source has an invalid byte count")
	}
	descriptorPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	var descriptorPathStat unix.Stat_t
	if err := unix.Fstatat(unix.AT_FDCWD, descriptorPath, &descriptorPathStat, 0); err != nil {
		return fmt.Errorf("%w: resolve descriptor publication source: %v", errAtomicNamespacePublicationUnsupported, err)
	}
	if before.identity.device != uint64(descriptorPathStat.Dev) || before.identity.inode != uint64(descriptorPathStat.Ino) {
		return fmt.Errorf("%w: /proc/self/fd did not resolve the pinned publication source", errAtomicNamespacePublicationUnsupported)
	}
	if err := atomicPublishDescriptorNoReplaceAt(source, probe, atomicPublicationProbeTargetName); err != nil {
		return fmt.Errorf("%w: descriptor-source Linkat probe: %v", errAtomicNamespacePublicationUnsupported, err)
	}
	after, err := fstatFile(source)
	if err != nil || !sameFileIdentity(before.identity, after.identity) || after.links != 1 || after.size != before.size {
		return errors.Join(errors.New("descriptor publication probe source changed during Linkat"), err)
	}
	if err := verifyAtomicNamespacePublicationProbe(directory, probe, probeStat); err != nil {
		return fmt.Errorf("verify durable descriptor-source Linkat probe: %w", err)
	}
	published, publishedStat, err := openVerifiedAtomicPublicationProbeFile(
		probe, atomicPublicationProbeTargetName, "descriptor-source Linkat probe target", atomicPublicationProbeBody, 1,
	)
	if err != nil {
		return err
	}
	defer published.Close()
	if !sameFileIdentity(after.identity, publishedStat.identity) {
		return errors.New("descriptor-source Linkat probe target is not the pinned O_TMPFILE source inode")
	}
	return nil
}

func verifyAtomicNamespacePublicationProbe(parent, probe *os.File, probeStat trustedStat) error {
	if err := verifyAtomicPublicationProbeEntries(probe, atomicPublicationProbeTargetName); err != nil {
		return err
	}
	target, _, err := openVerifiedAtomicPublicationProbeFile(
		probe, atomicPublicationProbeTargetName, "descriptor-source Linkat probe target", atomicPublicationProbeBody, 1,
	)
	if err != nil {
		return err
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return fmt.Errorf("sync descriptor-source Linkat probe target: %w", err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close descriptor-source Linkat probe target: %w", err)
	}
	if err := probe.Sync(); err != nil {
		return fmt.Errorf("sync descriptor-source Linkat probe namespace: %w", err)
	}
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync descriptor-source Linkat probe parent namespace: %w", err)
	}
	return verifyAtomicPublicationProbeDirectory(parent, probe, atomicPublicationProbeDirectoryName, probeStat)
}

func atomicPublishDescriptorNoReplaceAt(source *os.File, targetDirectory *os.File, targetName string) error {
	if source == nil || targetDirectory == nil {
		return errors.New("descriptor-bound hard-link publication requires source and target descriptors")
	}
	sourcePath := fmt.Sprintf("/proc/self/fd/%d", source.Fd())
	if err := unix.Linkat(unix.AT_FDCWD, sourcePath, int(targetDirectory.Fd()), targetName, unix.AT_SYMLINK_FOLLOW); err != nil {
		return err
	}
	return nil
}
