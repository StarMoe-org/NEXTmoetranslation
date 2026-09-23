package main

import (
	"errors"
	"fmt"

	"os"
	"path/filepath"
)

func (checkpoint *preflightCheckpoint) verifyFile(stage string) error {
	if checkpoint == nil || checkpoint.pinnedFile == nil || checkpoint.pinnedInfo == nil ||
		checkpoint.parentFile == nil || checkpoint.parentInfo == nil || checkpoint.operationalPath == "" {
		return errors.New("checkpoint inode and private directory are not pinned")
	}
	fileInfo, fileErr := checkpoint.pinnedFile.Stat()
	pathInfo, pathErr := os.Lstat(checkpoint.path)
	anchorInfo, anchorErr := os.Lstat(checkpoint.operationalPath)
	parentInfo, parentErr := checkpoint.parentFile.Stat()
	parentPathInfo, parentPathErr := os.Stat(filepath.Dir(checkpoint.path))
	fileOwner, fileOwnerOK := checkpointOwnerID(fileInfo)
	pathOwner, pathOwnerOK := checkpointOwnerID(pathInfo)
	anchorOwner, anchorOwnerOK := checkpointOwnerID(anchorInfo)
	parentOwner, parentOwnerOK := checkpointOwnerID(parentInfo)
	if fileErr != nil || pathErr != nil || anchorErr != nil || parentErr != nil || parentPathErr != nil ||
		!fileInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() || !anchorInfo.Mode().IsRegular() ||
		!os.SameFile(checkpoint.pinnedInfo, fileInfo) || !os.SameFile(checkpoint.pinnedInfo, pathInfo) ||
		!os.SameFile(checkpoint.pinnedInfo, anchorInfo) || !os.SameFile(checkpoint.parentInfo, parentInfo) ||
		!os.SameFile(checkpoint.parentInfo, parentPathInfo) ||
		fileInfo.Mode().Perm() != 0o600 || pathInfo.Mode().Perm() != 0o600 || anchorInfo.Mode().Perm() != 0o600 ||
		!fileOwnerOK || !pathOwnerOK || !anchorOwnerOK || int(fileOwner) != os.Geteuid() ||
		int(pathOwner) != os.Geteuid() || int(anchorOwner) != os.Geteuid() ||
		checkpointLinkCount(fileInfo) != 2 || checkpointLinkCount(pathInfo) != 2 || checkpointLinkCount(anchorInfo) != 2 ||
		fileInfo.Size() < 0 || pathInfo.Size() < 0 || anchorInfo.Size() < 0 ||
		fileInfo.Size() > maxCheckpointBytes || pathInfo.Size() > maxCheckpointBytes || anchorInfo.Size() > maxCheckpointBytes ||
		!parentInfo.IsDir() || parentInfo.Mode().Perm()&0o022 != 0 || !parentOwnerOK || int(parentOwner) != os.Geteuid() {
		return fmt.Errorf("checkpoint visible path, operational inode anchor, ownership, links, or private directory changed %s", stage)
	}
	if err := validateCheckpointAncestorChain(filepath.Dir(checkpoint.path)); err != nil {
		return fmt.Errorf("checkpoint private path ancestry changed %s: %w", stage, err)
	}
	if err := rejectCheckpointSidecars(checkpoint.path); err != nil {
		return fmt.Errorf("checkpoint visible pathname sidecar changed %s: %w", stage, err)
	}
	if err := rejectCheckpointSidecars(checkpoint.operationalPath); err != nil {
		return fmt.Errorf("checkpoint operational sidecar changed %s: %w", stage, err)
	}
	return nil
}

func (checkpoint *preflightCheckpoint) verifyFinalFile(stage string) error {
	fileInfo, fileErr := checkpoint.pinnedFile.Stat()
	pathInfo, pathErr := os.Lstat(checkpoint.path)
	_, anchorErr := os.Lstat(checkpoint.operationalPath)
	if fileErr != nil || pathErr != nil || !errors.Is(anchorErr, os.ErrNotExist) ||
		!fileInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(checkpoint.pinnedInfo, fileInfo) || !os.SameFile(checkpoint.pinnedInfo, pathInfo) ||
		fileInfo.Mode().Perm() != 0o600 || pathInfo.Mode().Perm() != 0o600 ||
		checkpointLinkCount(fileInfo) != 1 || checkpointLinkCount(pathInfo) != 1 ||
		fileInfo.Size() < 0 || fileInfo.Size() > maxCheckpointBytes || pathInfo.Size() != fileInfo.Size() {
		return fmt.Errorf("checkpoint final standalone inode is invalid %s", stage)
	}
	return rejectCheckpointSidecars(checkpoint.path)
}

func (checkpoint *preflightCheckpoint) Close() error {
	if checkpoint == nil {
		return nil
	}
	var result error
	releaseAnchor := true
	if checkpoint.database != nil {
		if err := checkpoint.verifyFile("before close"); err != nil {
			result = errors.Join(result, err)
			releaseAnchor = false
		}
		if err := checkpoint.database.Close(); err != nil {
			result = errors.Join(result, err)
			releaseAnchor = false
		}
		checkpoint.database = nil
		if err := checkpoint.verifyFile("after SQLite close"); err != nil {
			result = errors.Join(result, err)
			releaseAnchor = false
		}
	}
	if releaseAnchor && checkpoint.pinnedFile != nil && checkpoint.parentFile != nil {
		if !checkpoint.readOnly {
			if err := checkpoint.pinnedFile.Sync(); err != nil {
				result = errors.Join(result, fmt.Errorf("sync checkpoint before releasing operational anchor: %w", err))
				releaseAnchor = false
			}
		}
		if releaseAnchor {
			if err := checkpoint.parentFile.Sync(); err != nil {
				result = errors.Join(result, fmt.Errorf("sync checkpoint private directory before releasing operational anchor: %w", err))
				releaseAnchor = false
			}
		}
	}
	if releaseAnchor {
		anchorInfo, err := os.Lstat(checkpoint.operationalPath)
		if err != nil || !os.SameFile(checkpoint.pinnedInfo, anchorInfo) || checkpointLinkCount(anchorInfo) != 2 {
			result = errors.Join(result, errors.New("checkpoint operational anchor changed before release"))
			releaseAnchor = false
		}
	}
	if releaseAnchor {
		if err := os.Remove(checkpoint.operationalPath); err != nil {
			result = errors.Join(result, fmt.Errorf("release checkpoint operational anchor: %w", err))
			releaseAnchor = false
		} else if err := checkpoint.parentFile.Sync(); err != nil {
			result = errors.Join(result, fmt.Errorf("sync checkpoint private directory after releasing operational anchor: %w", err))
			releaseAnchor = false
		}
	}
	if releaseAnchor {
		if err := checkpoint.verifyFinalFile("after close"); err != nil {
			result = errors.Join(result, err)
		}
	}
	if checkpoint.pinnedFile != nil {
		if err := checkpoint.pinnedFile.Close(); err != nil {
			result = errors.Join(result, err)
		}
		checkpoint.pinnedFile = nil
	}
	if checkpoint.parentFile != nil {
		if err := checkpoint.parentFile.Close(); err != nil {
			result = errors.Join(result, err)
		}
		checkpoint.parentFile = nil
	}
	return result
}

func rejectCheckpointSidecars(path string) error {
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("checkpoint SQLite sidecar %s remains", suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect checkpoint SQLite sidecar: %w", err)
		}
	}
	return nil
}
