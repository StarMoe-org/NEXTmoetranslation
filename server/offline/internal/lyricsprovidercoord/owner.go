package lyricsprovidercoord

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

var lowercaseSHA256V1 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Owner retains the one global live-acquisition lock for its entire lifetime.
// Provider records are updated only while this lock and the private root
// directory descriptor remain open.
type Owner struct {
	mu       sync.Mutex
	rootPath string
	root     *os.File
	rootInfo os.FileInfo
	lock     *os.File
	lockInfo os.FileInfo
	records  map[lyricsproviderpolicy.Provider]providerRecordV1
	observed map[lyricsproviderpolicy.Provider]string
	failure  error
}

// AcquireDefault acquires the fixed, pre-provisioned private live state root.
// It never creates or repairs state.
func AcquireDefault() (*Owner, error) {
	return Acquire(lyricsproviderpolicy.FixedLiveStateRootV1)
}

// Acquire acquires a pre-provisioned state root. It is exported so local tests
// and reviewed embedding code can use an isolated root; the recovery binary
// calls only AcquireDefault and exposes no root override.
func Acquire(rootPath string) (*Owner, error) {
	root, rootInfo, err := openPrivateRootV1(rootPath)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*Owner, error) {
		_ = root.Close()
		return nil, cause
	}

	lock, err := openRootEntryV1(root, globalLockFileV1, unix.O_RDWR)
	if err != nil {
		return fail(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fail(holdV1("the retained global live-acquisition lock is already owned"))
		}
		return fail(holdV1("acquire the retained global live-acquisition lock: %v", err))
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return fail(holdV1("inspect retained global live-acquisition lock: %v", err))
	}

	owner := &Owner{
		rootPath: rootPath,
		root:     root,
		rootInfo: rootInfo,
		lock:     lock,
		lockInfo: lockInfo,
		records:  make(map[lyricsproviderpolicy.Provider]providerRecordV1, len(providerRecordFilesV1)),
		observed: make(map[lyricsproviderpolicy.Provider]string, len(providerRecordFilesV1)),
	}
	for _, provider := range orderedProvidersV1() {
		record, readErr := owner.readRecordLocked(provider)
		if readErr != nil {
			_ = owner.Close()
			return nil, readErr
		}
		if record.State == stateAdmittedV1 {
			_ = owner.Close()
			return nil, holdV1("provider %q has an unresolved admitted request", provider)
		}
		owner.records[provider] = record
	}
	return owner, nil
}

func orderedProvidersV1() []lyricsproviderpolicy.Provider {
	return []lyricsproviderpolicy.Provider{
		lyricsproviderpolicy.ProviderVocaloidFandom,
		lyricsproviderpolicy.ProviderMoegirl,
		lyricsproviderpolicy.ProviderSekaipedia,
	}
}

func openPrivateRootV1(path string) (*os.File, os.FileInfo, error) {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, holdV1("live state root must be an explicit canonical absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		if err == nil {
			err = errors.New("path contains a symlink")
		}
		return nil, nil, holdV1("live state root is missing or has noncanonical ancestry: %v", err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, holdV1("inspect live state root: %v", err)
	}
	if err := validatePrivateRootInfoV1(before); err != nil {
		return nil, nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, holdV1("open live state root: %v", err)
	}
	root := os.NewFile(uintptr(fd), path)
	opened, openedErr := root.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if openedErr != nil || pathErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, pathInfo) {
		_ = root.Close()
		return nil, nil, holdV1("live state root changed while being opened")
	}
	if err := validatePrivateRootInfoV1(opened); err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return root, opened, nil
}

func validatePrivateRootInfoV1(info os.FileInfo) error {
	owner, links, ok := fileOwnerAndLinksV1(info)
	if info == nil || info.Mode().Type() != os.ModeDir || !ok || int(owner) != os.Geteuid() || links < 2 {
		return holdV1("live state root must be an effective-UID-owned exact directory")
	}
	if info.Mode().Perm() != 0o700 {
		return holdV1("live state root permissions must be exactly 0700")
	}
	return nil
}

func openRootEntryV1(root *os.File, name string, flags int) (*os.File, error) {
	if root == nil || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, holdV1("live state entry name is invalid")
	}
	fd, err := unix.Openat(int(root.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, holdV1("open required live state entry %q: %v", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, holdV1("inspect required live state entry %q: %v", name, statErr)
	}
	owner, links, ok := fileOwnerAndLinksV1(info)
	if info.Mode().Type() != 0 || !ok || int(owner) != os.Geteuid() || links != 1 || info.Mode().Perm() != 0o600 {
		_ = file.Close()
		return nil, holdV1("live state entry %q must be an effective-UID-owned single-link 0600 regular file", name)
	}
	return file, nil
}

func fileOwnerAndLinksV1(info os.FileInfo) (uint32, uint64, bool) {
	if info == nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return stat.Uid, uint64(stat.Nlink), true
}

func (owner *Owner) verifyRootLocked() error {
	if owner == nil || owner.root == nil || owner.rootInfo == nil || owner.lock == nil {
		return holdV1("live acquisition ownership is not open")
	}
	opened, openedErr := owner.root.Stat()
	pathInfo, pathErr := os.Lstat(owner.rootPath)
	if openedErr != nil || pathErr != nil || !os.SameFile(owner.rootInfo, opened) || !os.SameFile(owner.rootInfo, pathInfo) {
		return holdV1("live state root identity changed while ownership was retained")
	}
	if err := validatePrivateRootInfoV1(opened); err != nil {
		return err
	}
	lockOpened, lockErr := owner.lock.Stat()
	lockPathInfo, lockPathErr := os.Lstat(filepath.Join(owner.rootPath, globalLockFileV1))
	if owner.lockInfo == nil || lockErr != nil || lockPathErr != nil ||
		!os.SameFile(owner.lockInfo, lockOpened) || !os.SameFile(owner.lockInfo, lockPathInfo) {
		return holdV1("retained global live-acquisition lock identity changed")
	}
	lockOwner, lockLinks, lockOK := fileOwnerAndLinksV1(lockOpened)
	if lockOpened.Mode().Type() != 0 || !lockOK || int(lockOwner) != os.Geteuid() || lockLinks != 1 || lockOpened.Mode().Perm() != 0o600 {
		return holdV1("retained global live-acquisition lock no longer has its private file identity")
	}
	resolved, err := filepath.EvalSymlinks(owner.rootPath)
	if err != nil || resolved != owner.rootPath {
		return holdV1("live state root ancestry changed while ownership was retained")
	}
	return nil
}

func (owner *Owner) readRecordLocked(provider lyricsproviderpolicy.Provider) (providerRecordV1, error) {
	if err := owner.verifyRootLocked(); err != nil {
		return providerRecordV1{}, err
	}
	name, supported := providerRecordFilesV1[provider]
	if !supported {
		return providerRecordV1{}, holdV1("unsupported live provider %q", provider)
	}
	file, err := openRootEntryV1(owner.root, name, unix.O_RDONLY)
	if err != nil {
		return providerRecordV1{}, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maximumRecordBytesV1+1))
	if err != nil || len(body) == 0 || len(body) > maximumRecordBytesV1 {
		return providerRecordV1{}, holdV1("read provider %q live state within its byte boundary: %v", provider, err)
	}
	return decodeRecordV1(provider, body)
}

func decodeRecordV1(provider lyricsproviderpolicy.Provider, body []byte) (providerRecordV1, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var record providerRecordV1
	if err := decoder.Decode(&record); err != nil {
		return providerRecordV1{}, holdV1("decode provider %q live state: %v", provider, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return providerRecordV1{}, holdV1("provider %q live state contains trailing JSON", provider)
	}
	if err := validateRecordV1(provider, record); err != nil {
		return providerRecordV1{}, err
	}
	canonical, err := encodeRecordV1(record)
	if err != nil || !bytes.Equal(body, canonical) {
		return providerRecordV1{}, holdV1("provider %q live state is not canonical", provider)
	}
	return record, nil
}

func validateRecordV1(provider lyricsproviderpolicy.Provider, record providerRecordV1) error {
	if record.SchemaVersion != stateSchemaVersionV1 || record.Provider != provider || record.Generation == 0 {
		return holdV1("provider %q live state identity is corrupt or unprovisioned", provider)
	}
	if record.FailureCount > maximumFailureCountV1 {
		return holdV1("provider %q live failure count exceeds its safe boundary", provider)
	}
	if _, err := parseCanonicalTimeV1(record.NotBefore); err != nil {
		return holdV1("provider %q not-before is corrupt: %v", provider, err)
	}
	switch record.State {
	case stateIdleV1:
		if record.Admission != nil {
			return holdV1("provider %q idle state contains an admission", provider)
		}
	case stateAdmittedV1:
		if record.Admission == nil || !lowercaseSHA256V1.MatchString(record.Admission.ID) ||
			!lowercaseSHA256V1.MatchString(record.Admission.RequestSHA256) {
			return holdV1("provider %q admitted state is incomplete", provider)
		}
		if _, err := parseCanonicalTimeV1(record.Admission.AdmittedAt); err != nil {
			return holdV1("provider %q admission time is corrupt: %v", provider, err)
		}
	default:
		return holdV1("provider %q live state disposition is corrupt", provider)
	}
	return nil
}

func parseCanonicalTimeV1(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || canonicalTimeV1(parsed) != value || parsed.Year() < 1970 {
		return time.Time{}, errors.New("expected canonical UTC RFC3339 at or after 1970")
	}
	return parsed, nil
}

func encodeRecordV1(record providerRecordV1) ([]byte, error) {
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func cloneRecordV1(record providerRecordV1) providerRecordV1 {
	if record.Admission != nil {
		admission := *record.Admission
		record.Admission = &admission
	}
	return record
}

func (owner *Owner) persistRecordLocked(provider lyricsproviderpolicy.Provider, expected, replacement providerRecordV1) error {
	if owner.failure != nil {
		return owner.failure
	}
	if err := owner.verifyRootLocked(); err != nil {
		return owner.failLocked(err)
	}
	current, err := owner.readRecordLocked(provider)
	if err != nil {
		return owner.failLocked(err)
	}
	if !reflect.DeepEqual(current, expected) {
		return owner.failLocked(holdV1("provider %q live state changed outside the retained owner", provider))
	}
	if err := validateRecordV1(provider, replacement); err != nil {
		return owner.failLocked(err)
	}
	body, err := encodeRecordV1(replacement)
	if err != nil {
		return owner.failLocked(holdV1("encode provider %q live state: %v", provider, err))
	}
	name := providerRecordFilesV1[provider]
	temporaryName, err := randomTemporaryNameV1(provider)
	if err != nil {
		return owner.failLocked(err)
	}
	fd, err := unix.Openat(int(owner.root.Fd()), temporaryName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return owner.failLocked(holdV1("create provider %q durable state replacement: %v", provider, err))
	}
	temporary := os.NewFile(uintptr(fd), temporaryName)
	cleanup := func() {
		_ = temporary.Close()
		_ = unix.Unlinkat(int(owner.root.Fd()), temporaryName, 0)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return owner.failLocked(holdV1("fix provider %q durable state permissions: %v", provider, err))
	}
	if _, err := temporary.Write(body); err != nil {
		cleanup()
		return owner.failLocked(holdV1("write provider %q durable state: %v", provider, err))
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return owner.failLocked(holdV1("sync provider %q durable state: %v", provider, err))
	}
	info, err := temporary.Stat()
	if err != nil {
		cleanup()
		return owner.failLocked(holdV1("inspect provider %q durable state replacement: %v", provider, err))
	}
	entryOwner, links, ok := fileOwnerAndLinksV1(info)
	if !ok || int(entryOwner) != os.Geteuid() || links != 1 || info.Mode().Type() != 0 || info.Mode().Perm() != 0o600 || info.Size() != int64(len(body)) {
		cleanup()
		return owner.failLocked(holdV1("provider %q durable state replacement failed identity validation", provider))
	}
	if err := temporary.Close(); err != nil {
		_ = unix.Unlinkat(int(owner.root.Fd()), temporaryName, 0)
		return owner.failLocked(holdV1("close provider %q durable state replacement: %v", provider, err))
	}
	if err := unix.Renameat(int(owner.root.Fd()), temporaryName, int(owner.root.Fd()), name); err != nil {
		_ = unix.Unlinkat(int(owner.root.Fd()), temporaryName, 0)
		return owner.failLocked(holdV1("publish provider %q durable state: %v", provider, err))
	}
	if err := unix.Fsync(int(owner.root.Fd())); err != nil {
		return owner.failLocked(holdV1("sync live state root after provider %q update: %v", provider, err))
	}
	confirmed, err := owner.readRecordLocked(provider)
	if err != nil || !reflect.DeepEqual(confirmed, replacement) {
		if err == nil {
			err = errors.New("published bytes do not match")
		}
		return owner.failLocked(holdV1("verify provider %q durable state publication: %v", provider, err))
	}
	owner.records[provider] = cloneRecordV1(replacement)
	return nil
}

func randomTemporaryNameV1(provider lyricsproviderpolicy.Provider) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", holdV1("generate durable state replacement identity: %v", err)
	}
	return fmt.Sprintf(".%s.%s.tmp", provider, hex.EncodeToString(random[:])), nil
}

func (owner *Owner) failLocked(err error) error {
	if err == nil {
		err = ErrHold
	}
	if !errors.Is(err, ErrHold) {
		err = holdV1("%v", err)
	}
	if owner.failure == nil {
		owner.failure = err
	}
	return owner.failure
}

// Close releases the retained global lock. Lock and state files are never
// deleted, because deletion could create split ownership across different
// inodes or erase forensic HOLD state.
func (owner *Owner) Close() error {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	var result error
	if owner.lock != nil {
		result = errors.Join(result, unix.Flock(int(owner.lock.Fd()), unix.LOCK_UN), owner.lock.Close())
		owner.lock = nil
		owner.lockInfo = nil
	}
	if owner.root != nil {
		result = errors.Join(result, owner.root.Close())
		owner.root = nil
	}
	return result
}

func holdV1(format string, args ...any) error {
	if format == "" {
		return ErrHold
	}
	return fmt.Errorf("%w: %s", ErrHold, fmt.Sprintf(format, args...))
}
