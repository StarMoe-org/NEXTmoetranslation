//go:build darwin

package lyricsacquisition

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	atomicPublicationProbeDirectoryName = ".moesekai-fclonefileat-capability-v1"
	atomicPublicationProbeSourceName    = "descriptor-source-fclonefileat.source"
	atomicPublicationProbeTargetName    = "descriptor-source-fclonefileat.published"
)

var (
	errAtomicNamespacePublicationUnsupported = errors.New("HOLD: descriptor-bound lyrics acquisition publication requires a clone-capable APFS volume")
	atomicPublicationProbeBody               = []byte("moesekai-darwin-descriptor-source-fclonefileat-capability-v1\n")
)

func requireAtomicNamespacePublication() error {
	return nil
}

func preflightAtomicNamespacePublication(directory *os.File) error {
	if directory == nil {
		return errors.New("descriptor publication preflight requires a directory descriptor")
	}
	var filesystemStat unix.Statfs_t
	if err := unix.Fstatfs(int(directory.Fd()), &filesystemStat); err != nil {
		return fmt.Errorf("inspect descriptor publication filesystem: %w", err)
	}
	filesystem := strings.TrimRight(string(filesystemStat.Fstypename[:]), "\x00")
	if filesystem != "apfs" {
		return fmt.Errorf("%w: filesystem is %q", errAtomicNamespacePublicationUnsupported, filesystem)
	}
	probe, probeStat, created, err := openAtomicPublicationProbeDirectory(directory, atomicPublicationProbeDirectoryName)
	if err != nil {
		return fmt.Errorf("%w: %v", errAtomicNamespacePublicationUnsupported, err)
	}
	defer probe.Close()
	if !created {
		if err := verifyAtomicNamespacePublicationProbe(directory, probe, probeStat); err != nil {
			return fmt.Errorf("%w: retained fclonefileat probe: %v", errAtomicNamespacePublicationUnsupported, err)
		}
		return nil
	}

	fd, err := unix.Openat(
		int(probe.Fd()), atomicPublicationProbeSourceName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create descriptor publication clone probe source: %w", err)
	}
	writer := os.NewFile(uintptr(fd), "darwin-descriptor-source-fclonefileat-capability")
	if writer == nil {
		_ = unix.Close(fd)
		return errors.New("descriptor publication clone probe source descriptor is invalid")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = writer.Close()
		return fmt.Errorf("secure descriptor publication clone probe source: %w", err)
	}
	for offset := 0; offset < len(atomicPublicationProbeBody); {
		written, writeErr := writer.WriteAt(atomicPublicationProbeBody[offset:], int64(offset))
		if written <= 0 || written > len(atomicPublicationProbeBody)-offset {
			_ = writer.Close()
			return errors.Join(errors.New("descriptor publication clone probe source write returned an invalid byte count"), writeErr)
		}
		offset += written
		if writeErr != nil {
			_ = writer.Close()
			return fmt.Errorf("write descriptor publication clone probe source: %w", writeErr)
		}
	}
	if err := writer.Sync(); err != nil {
		_ = writer.Close()
		return fmt.Errorf("sync descriptor publication clone probe source: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close descriptor publication clone probe source: %w", err)
	}
	if err := probe.Sync(); err != nil {
		return fmt.Errorf("sync descriptor publication clone probe source namespace: %w", err)
	}
	source, sourceStat, err := openVerifiedAtomicPublicationProbeFile(
		probe, atomicPublicationProbeSourceName, "descriptor-source fclonefileat probe source", atomicPublicationProbeBody, 1,
	)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := atomicPublishDescriptorNoReplaceAt(source, probe, atomicPublicationProbeTargetName); err != nil {
		return fmt.Errorf("descriptor-source fclonefileat probe: %w", err)
	}
	sourceAfter, err := fstatFile(source)
	if err != nil || !sameTrustedMetadata(sourceStat, sourceAfter) {
		return errors.Join(errors.New("descriptor publication clone probe source changed during fclonefileat"), err)
	}
	if err := verifyAtomicNamespacePublicationProbe(directory, probe, probeStat); err != nil {
		return fmt.Errorf("verify durable descriptor-source fclonefileat probe: %w", err)
	}
	target, targetStat, err := openVerifiedAtomicPublicationProbeFile(
		probe, atomicPublicationProbeTargetName, "descriptor-source fclonefileat probe target", atomicPublicationProbeBody, 1,
	)
	if err != nil {
		return err
	}
	defer target.Close()
	if sameFileIdentity(sourceStat.identity, targetStat.identity) {
		return errors.New("descriptor-source fclonefileat probe target unexpectedly aliases the source inode")
	}
	return nil
}

func verifyAtomicNamespacePublicationProbe(parent, probe *os.File, probeStat trustedStat) error {
	if err := verifyAtomicPublicationProbeEntries(probe, atomicPublicationProbeSourceName, atomicPublicationProbeTargetName); err != nil {
		return err
	}
	source, sourceStat, err := openVerifiedAtomicPublicationProbeFile(
		probe, atomicPublicationProbeSourceName, "descriptor-source fclonefileat probe source", atomicPublicationProbeBody, 1,
	)
	if err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return fmt.Errorf("close descriptor-source fclonefileat probe source: %w", err)
	}
	target, targetStat, err := openVerifiedAtomicPublicationProbeFile(
		probe, atomicPublicationProbeTargetName, "descriptor-source fclonefileat probe target", atomicPublicationProbeBody, 1,
	)
	if err != nil {
		return err
	}
	if sameFileIdentity(sourceStat.identity, targetStat.identity) {
		_ = target.Close()
		return errors.New("descriptor-source fclonefileat probe target unexpectedly aliases the source inode")
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return fmt.Errorf("sync descriptor-source fclonefileat probe target: %w", err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close descriptor-source fclonefileat probe target: %w", err)
	}
	if err := probe.Sync(); err != nil {
		return fmt.Errorf("sync descriptor-source fclonefileat probe namespace: %w", err)
	}
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync descriptor-source fclonefileat probe parent namespace: %w", err)
	}
	return verifyAtomicPublicationProbeDirectory(parent, probe, atomicPublicationProbeDirectoryName, probeStat)
}

func atomicPublishDescriptorNoReplaceAt(source *os.File, targetDirectory *os.File, targetName string) error {
	if source == nil || targetDirectory == nil {
		return errors.New("descriptor-bound clone publication requires source and target descriptors")
	}
	if err := unix.Fclonefileat(int(source.Fd()), int(targetDirectory.Fd()), targetName, 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return fmt.Errorf("%w: fclonefileat: %v", errAtomicNamespacePublicationUnsupported, err)
		}
		return err
	}
	return nil
}
