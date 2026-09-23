package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"moesekai/server/offline/internal/lyricsacquisition"
)

func TestPinnedPrivateInputRejectsSymlinkHardlinkAndMode(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	tests := []struct {
		name   string
		mutate func(t *testing.T, root, path string) string
	}{
		{
			name: "leaf symlink",
			mutate: func(t *testing.T, root, path string) string {
				t.Helper()
				alias := filepath.Join(root, "alias.json")
				if err := os.Symlink(path, alias); err != nil {
					t.Fatal(err)
				}
				return alias
			},
		},
		{
			name: "hard link",
			mutate: func(t *testing.T, root, path string) string {
				t.Helper()
				alias := filepath.Join(root, "alias.json")
				if err := os.Link(path, alias); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "non-private mode",
			mutate: func(t *testing.T, _, path string) string {
				t.Helper()
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "non-private parent mode",
			mutate: func(t *testing.T, root, path string) string {
				t.Helper()
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, path := newPinnedPrivateInput(t, body)
			candidate := test.mutate(t, root, path)
			if _, err := readPinnedPrivateFile(candidate, 1024); err == nil {
				t.Fatalf("%s was accepted", test.name)
			}
		})
	}
}

func TestPinnedPrivateInputRejectsSymlinkedAncestry(t *testing.T) {
	root, path := newPinnedPrivateInput(t, []byte("private input"))
	aliasRoot := root + "-alias"
	if err := os.Symlink(root, aliasRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(aliasRoot) })
	if _, err := readPinnedPrivateFile(filepath.Join(aliasRoot, filepath.Base(path)), 1024); err == nil ||
		!strings.Contains(err.Error(), "canonical symlink-free ancestry") {
		t.Fatalf("symlinked ancestry error=%v", err)
	}
}

func TestPinnedPrivateInputRejectsForeignOwnerIdentity(t *testing.T) {
	info := fakeInputFileInfo{
		mode: 0o600,
		size: 16,
		when: time.Unix(1, 0),
		stat: &syscall.Stat_t{Uid: uint32(os.Geteuid() + 1), Nlink: 1},
	}
	err := validateRegularFileInfo(info, regularFilePolicy{
		label: "foreign-owner input", exactPermissions: 0o600, requireExactMode: true, maximum: 1024,
	})
	if err == nil || !strings.Contains(err.Error(), "effective-UID-owned") {
		t.Fatalf("foreign owner error=%v", err)
	}
}

func TestPinnedPrivateInputRejectsDirectoryPathSwapBeforeOpen(t *testing.T) {
	root, path := newPinnedPrivateInput(t, []byte("reviewed bytes"))
	moved := root + "-moved"
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	var swapped atomic.Bool
	withPrivateInputHook(t, func(stage, current string, _ *os.File) error {
		if stage != privateInputAfterDirectoryInspect || current != root || !swapped.CompareAndSwap(false, true) {
			return nil
		}
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, filepath.Base(path)), []byte("replacement bytes"), 0o600)
	})
	if _, err := readPinnedPrivateFile(path, 1024); err == nil {
		t.Fatal("directory pathname swap before open was accepted")
	}
	if !swapped.Load() {
		t.Fatal("directory pathname swap hook did not run")
	}
}

func TestPinnedPrivateInputRejectsParentSwapAfterOpen(t *testing.T) {
	root, path := newPinnedPrivateInput(t, []byte("reviewed bytes"))
	moved := root + "-moved"
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	var swapped atomic.Bool
	withPrivateInputHook(t, func(stage, current string, _ *os.File) error {
		if stage != privateInputAfterDirectoryOpen || current != root || !swapped.CompareAndSwap(false, true) {
			return nil
		}
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, filepath.Base(path)), []byte("replacement bytes"), 0o600)
	})
	if _, err := readPinnedPrivateFile(path, 1024); err == nil {
		t.Fatal("parent swap after descriptor open was accepted")
	}
	if !swapped.Load() {
		t.Fatal("parent swap hook did not run")
	}
}

func TestPinnedPrivateInputAllowsUnrelatedSiblingChurnInParentDirectory(t *testing.T) {
	root, path := newPinnedPrivateInput(t, []byte("reviewed bytes"))
	pinned, err := openPinnedRegularFile(path, regularFilePolicy{
		label: "immutable test input", exactPermissions: 0o600, requireExactMode: true,
		requirePrivate: true, maximum: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()

	sibling := filepath.Join(root, "unrelated-sibling")
	if err := os.WriteFile(sibling, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pinned.verify(); err != nil {
		t.Fatalf("unrelated sibling creation invalidated the pinned leaf: %v", err)
	}
	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	if err := pinned.verify(); err != nil {
		t.Fatalf("unrelated sibling removal invalidated the pinned leaf: %v", err)
	}
}

func TestPinnedPrivateInputRejectsLeafSwapBeforeOpen(t *testing.T) {
	_, path := newPinnedPrivateInput(t, []byte("reviewed bytes"))
	moved := path + ".moved"
	t.Cleanup(func() { _ = os.Remove(moved) })
	var swapped atomic.Bool
	withPrivateInputHook(t, func(stage, current string, _ *os.File) error {
		if stage != privateInputAfterLeafInspect || current != path || !swapped.CompareAndSwap(false, true) {
			return nil
		}
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("replacement bytes"), 0o600)
	})
	if _, err := readPinnedPrivateFile(path, 1024); err == nil {
		t.Fatal("leaf swap before descriptor open was accepted")
	}
	if !swapped.Load() {
		t.Fatal("leaf swap hook did not run")
	}
}

func TestPinnedPrivateInputRejectsSameBytesInodeReplacement(t *testing.T) {
	body := []byte("same reviewed bytes")
	_, path := newPinnedPrivateInput(t, body)
	moved := path + ".moved"
	t.Cleanup(func() { _ = os.Remove(moved) })
	var swapped atomic.Bool
	withPrivateInputHook(t, func(stage, current string, _ *os.File) error {
		if stage != privateInputAfterLeafRead || current != path || !swapped.CompareAndSwap(false, true) {
			return nil
		}
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.WriteFile(path, append([]byte(nil), body...), 0o600)
	})
	if _, err := readPinnedPrivateFile(path, 1024); err == nil {
		t.Fatal("same-bytes inode replacement was accepted")
	}
	if !swapped.Load() {
		t.Fatal("same-bytes replacement hook did not run")
	}
}

func TestPinnedPrivateInputRejectsTruncationGrowthAndModtimeChange(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 64)
	tests := []struct {
		name   string
		mutate func(path string) error
	}{
		{name: "truncation", mutate: func(path string) error { return os.Truncate(path, 16) }},
		{name: "growth", mutate: func(path string) error {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			_, writeErr := file.Write(bytes.Repeat([]byte("b"), 128))
			return errors.Join(writeErr, file.Close())
		}},
		{name: "same-size modtime change", mutate: func(path string) error {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			_, writeErr := file.Write(bytes.Repeat([]byte("c"), len(body)))
			closeErr := file.Close()
			stamp := time.Unix(946684800, 123456789)
			return errors.Join(writeErr, closeErr, os.Chtimes(path, stamp, stamp))
		}},
		{name: "same-size rewrite with restored modtime", mutate: func(path string) error {
			before, err := os.Stat(path)
			if err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			_, writeErr := file.Write(bytes.Repeat([]byte("d"), len(body)))
			closeErr := file.Close()
			return errors.Join(writeErr, closeErr, os.Chtimes(path, before.ModTime(), before.ModTime()))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, path := newPinnedPrivateInput(t, body)
			var mutated atomic.Bool
			withPrivateInputHook(t, func(stage, current string, _ *os.File) error {
				if stage != privateInputAfterLeafRead || current != path || !mutated.CompareAndSwap(false, true) {
					return nil
				}
				return test.mutate(path)
			})
			if _, err := readPinnedPrivateFile(path, 1024); err == nil {
				t.Fatalf("%s during read was accepted", test.name)
			}
			if !mutated.Load() {
				t.Fatalf("%s hook did not run", test.name)
			}
		})
	}
}

func TestCheckedRecoveryLedgerRejectsSymlinkAndWrongMode(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, ledgerPath string) string
	}{
		{
			name: "symlink alias",
			mutate: func(t *testing.T, ledgerPath string) string {
				t.Helper()
				alias := ledgerPath + "-alias"
				if err := os.Symlink(ledgerPath, alias); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(alias) })
				return alias
			},
		},
		{
			name: "wrong mode",
			mutate: func(t *testing.T, ledgerPath string) string {
				t.Helper()
				if err := os.Chmod(ledgerPath, 0o755); err != nil {
					t.Fatal(err)
				}
				return ledgerPath
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent, _ := newPinnedPrivateInput(t, []byte("parent sentinel"))
			ledgerPath := filepath.Join(parent, "ledger")
			ledger, err := lyricsacquisition.CreateLedger(context.Background(), ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := ledger.Close(); err != nil {
				t.Fatal(err)
			}
			candidate := test.mutate(t, ledgerPath)
			if opened, err := openCheckedRecoveryLedger(context.Background(), candidate); err == nil {
				_ = opened.Close()
				t.Fatalf("%s ledger was accepted", test.name)
			}
		})
	}
}

func newPinnedPrivateInput(t *testing.T, body []byte) (string, string) {
	t.Helper()
	root, err := os.MkdirTemp(recoveryCommandTestRoot, "private-input-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "input.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func withPrivateInputHook(t *testing.T, hook privateInputHook) {
	t.Helper()
	setPrivateInputTestHook(hook)
	t.Cleanup(func() { setPrivateInputTestHook(nil) })
}

type fakeInputFileInfo struct {
	mode os.FileMode
	size int64
	when time.Time
	stat *syscall.Stat_t
}

func (info fakeInputFileInfo) Name() string       { return "fake" }
func (info fakeInputFileInfo) Size() int64        { return info.size }
func (info fakeInputFileInfo) Mode() os.FileMode  { return info.mode }
func (info fakeInputFileInfo) ModTime() time.Time { return info.when }
func (info fakeInputFileInfo) IsDir() bool        { return info.mode.IsDir() }
func (info fakeInputFileInfo) Sys() any           { return info.stat }
