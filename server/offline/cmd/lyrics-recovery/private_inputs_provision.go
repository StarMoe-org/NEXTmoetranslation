package main

import (
	"encoding/json"
	"errors"

	"io"
	"os"
	"path/filepath"

	"strings"

	"time"

	"golang.org/x/sys/unix"

	"moesekai/server/offline/internal/lyricsprovidercoord"
	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

const (
	provisionedLiveStateSchemaV1 = "lyrics-provider-live-state/v1"
	provisionedLiveStateIdleV1   = "idle"
	provisionedGlobalLockFileV1  = "global-live.lock"
)

type provisionedLiveStateRecordV1 struct {
	SchemaVersion string                        `json:"schemaVersion"`
	Provider      lyricsproviderpolicy.Provider `json:"provider"`
	Generation    uint64                        `json:"generation"`
	State         string                        `json:"state"`
	NotBefore     string                        `json:"notBefore"`
	FailureCount  uint32                        `json:"failureCount"`
}

func provisionRecoveryLiveStateRoot(rootPath string) (resultErr error) {
	if rootPath == "" || strings.TrimSpace(rootPath) != rootPath || !filepath.IsAbs(rootPath) ||
		filepath.Clean(rootPath) != rootPath || filepath.Dir(rootPath) != recoveryLiveStateProvisionParentPath {
		return errors.New("live-state provisioning requires one canonical direct /private/tmp root")
	}
	parent, err := openCanonicalDirectory(filepath.Dir(rootPath), directoryPolicy{label: "live-state provisioning parent"})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	name := filepath.Base(rootPath)
	var existing unix.Stat_t
	if err := unix.Fstatat(int(parent.file.Fd()), name, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return errors.New("live-state provisioning refuses an existing root")
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if _, err := os.Lstat(rootPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("live-state provisioning refuses an existing or ambiguous root")
		}
		return err
	}
	if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errors.New("live-state provisioning lost create-exclusive root ownership")
		}
		return err
	}
	if err := parent.file.Sync(); err != nil {
		return err
	}
	if err := refreshPinnedDirectoryAfterOwnedMutation(parent); err != nil {
		return err
	}
	root, err := openDirectoryAt(parent, name, rootPath, directoryPolicy{
		label: "provisioned live-state root", private: true, effectiveUID: true,
	})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	if err := createProvisionedLiveStateFile(root, provisionedGlobalLockFileV1, []byte("retained global lock\n")); err != nil {
		return err
	}
	seen := make(map[lyricsproviderpolicy.Provider]struct{})
	for _, spec := range lyricsproviderpolicy.CompiledProviderSpecsV1() {
		filename, ok := provisionedLiveStateRecordFile(spec.Provider)
		if !ok {
			return errors.New("live-state provisioning encountered an unsupported compiled provider")
		}
		if _, duplicate := seen[spec.Provider]; duplicate {
			return errors.New("live-state provisioning encountered a duplicate compiled provider")
		}
		seen[spec.Provider] = struct{}{}
		record := provisionedLiveStateRecordV1{
			SchemaVersion: provisionedLiveStateSchemaV1, Provider: spec.Provider, Generation: 1,
			State: provisionedLiveStateIdleV1, NotBefore: time.Unix(0, 0).UTC().Format(time.RFC3339Nano),
		}
		body, err := json.Marshal(record)
		if err != nil {
			return err
		}
		body = append(body, '\n')
		if err := createProvisionedLiveStateFile(root, filename, body); err != nil {
			return err
		}
	}
	if len(seen) != 3 || root.verify() != nil {
		return errors.New("live-state provisioning did not create the exact provider record set")
	}
	if err := root.file.Sync(); err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return err
	}
	if err := parent.Close(); err != nil {
		return err
	}
	owner, err := lyricsprovidercoord.Acquire(rootPath)
	if err != nil {
		return err
	}
	return owner.Close()
}

func provisionedLiveStateRecordFile(provider lyricsproviderpolicy.Provider) (string, bool) {
	switch provider {
	case lyricsproviderpolicy.ProviderVocaloidFandom:
		return "vocaloid_fandom.json", true
	case lyricsproviderpolicy.ProviderMoegirl:
		return "moegirl.json", true
	case lyricsproviderpolicy.ProviderSekaipedia:
		return "sekaipedia.json", true
	default:
		return "", false
	}
}

func createProvisionedLiveStateFile(root *pinnedDirectory, name string, body []byte) error {
	if root == nil || root.file == nil || filepath.Base(name) != name || name == "." || name == ".." || len(body) == 0 ||
		len(body) > 16<<10 {
		return errors.New("live-state provisioning entry input is invalid")
	}
	if err := root.verify(); err != nil {
		return err
	}
	var existing unix.Stat_t
	if err := unix.Fstatat(int(root.file.Fd()), name, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return errors.New("live-state provisioning refuses an existing provider entry")
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	fd, err := unix.Openat(int(root.file.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("live-state provisioning entry descriptor is invalid")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	for pending := body; len(pending) > 0; {
		written, err := file.Write(pending)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		pending = pending[written:]
	}
	if err := file.Sync(); err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || validateRegularFileInfo(opened, regularFilePolicy{
		label: "provisioned live-state entry", exactPermissions: 0o600, requireExactMode: true,
		maximum: 16 << 10,
	}) != nil || opened.Size() != int64(len(body)) {
		return errors.New("provisioned live-state entry identity is invalid")
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	pathInfo, err := os.Lstat(filepath.Join(root.path, name))
	if err != nil || !sameStableRegularFile(opened, pathInfo) {
		return errors.New("provisioned live-state entry changed before publication")
	}
	if err := root.file.Sync(); err != nil {
		return err
	}
	return refreshPinnedDirectoryAfterOwnedMutation(root)
}

func refreshPinnedDirectoryAfterOwnedMutation(directory *pinnedDirectory) error {
	if directory == nil || directory.file == nil || directory.info == nil {
		return errors.New("provisioned directory is not open")
	}
	opened, openedErr := directory.file.Stat()
	pathInfo, pathErr := os.Lstat(directory.path)
	if openedErr != nil || pathErr != nil || !os.SameFile(directory.info, opened) ||
		!os.SameFile(directory.info, pathInfo) || !os.SameFile(opened, pathInfo) {
		return errors.New("provisioned directory identity changed during owned mutation")
	}
	if err := validateDirectoryInfo(opened, directory.policy); err != nil {
		return err
	}
	if !canonicalAbsolutePath(directory.path) {
		return errors.New("provisioned directory ancestry became ambiguous")
	}
	directory.info = opened
	return directory.verify()
}
