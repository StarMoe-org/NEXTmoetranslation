package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/model"
)

// rewriteLatestS3Archive replaces the plaintext latest archive with a copy
// edited by mutate.
func rewriteLatestS3Archive(t *testing.T, bucket *fakeS3Bucket, key string, mutate func(root string)) {
	t.Helper()
	bucket.mu.Lock()
	archive := append([]byte(nil), bucket.objects[key]...)
	bucket.mu.Unlock()
	if len(archive) == 0 {
		t.Fatalf("no archive at %s: %v", key, bucket.keys())
	}
	root := t.TempDir()
	if err := untarGz(archive, root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	rewritten, err := tarGzDir(root)
	if err != nil {
		t.Fatal(err)
	}
	bucket.mu.Lock()
	bucket.objects[key] = rewritten
	bucket.mu.Unlock()
}

func TestS3RestoreAcceptsArchivePredatingRestoreOptionalCategory(t *testing.T) {
	h := setupLegacyBackup(t)
	bucket, endpoint := newFakeS3Bucket(t)
	for key, value := range map[string]string{
		config.KeyBackupS3Endpoint:  endpoint,
		config.KeyBackupS3Region:    "test-region",
		config.KeyBackupS3Bucket:    "test-bucket",
		config.KeyBackupS3Prefix:    "snapshots",
		config.KeyBackupS3AccessKey: "test-access",
		config.KeyBackupS3SecretKey: "test-secret",
	} {
		if err := h.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(backupEncryptionKeyEnv, "")
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}
	const latest = "/test-bucket/snapshots/latest.tar.gz"
	rewriteLatestS3Archive(t, bucket, latest, func(root string) {
		for _, name := range []string{"gachaInfo.json", "gachaInfo.full.json"} {
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
	})
	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "被覆盖", model.SourceLLM, "editor"); err != nil {
		t.Fatal(err)
	}
	if before, err := h.store.CategoryData("gachaInfo"); err != nil || len(before["name"]) != 1 {
		t.Fatalf("gachaInfo before restore=%v err=%v", before, err)
	}

	result, err := h.manager.restoreS3()
	if err != nil {
		t.Fatalf("restore archive without gachaInfo: %v", err)
	}
	if result.Categories != len(model.SupportedCategories) || result.Entries != len(model.SupportedCategories)-1 {
		t.Fatalf("restore result = %+v", result)
	}
	cards, err := h.store.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	if got := cards["prefix"]["こんにちは"]; got.Text != "你好" || got.Source != model.SourceHuman {
		t.Fatalf("restored cards entry = %+v", got)
	}
	gachaInfo, err := h.store.CategoryData("gachaInfo")
	if err != nil {
		t.Fatal(err)
	}
	for field, entries := range gachaInfo {
		if len(entries) != 0 {
			t.Fatalf("gachaInfo after restoring an archive without it: %s=%v", field, entries)
		}
	}
}

func TestS3RestoreRejectsHalfPresentRestoreOptionalCategory(t *testing.T) {
	h := setupLegacyBackup(t)
	bucket, endpoint := newFakeS3Bucket(t)
	for key, value := range map[string]string{
		config.KeyBackupS3Endpoint:  endpoint,
		config.KeyBackupS3Region:    "test-region",
		config.KeyBackupS3Bucket:    "test-bucket",
		config.KeyBackupS3Prefix:    "snapshots",
		config.KeyBackupS3AccessKey: "test-access",
		config.KeyBackupS3SecretKey: "test-secret",
	} {
		if err := h.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(backupEncryptionKeyEnv, "")
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}
	rewriteLatestS3Archive(t, bucket, "/test-bucket/snapshots/latest.tar.gz", func(root string) {
		if err := os.Remove(filepath.Join(root, "gachaInfo.full.json")); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := h.manager.restoreS3(); err == nil || !strings.Contains(err.Error(), "missing a complete translations layout") {
		t.Fatalf("half-present gachaInfo restore error = %v", err)
	}
	gachaInfo, err := h.store.CategoryData("gachaInfo")
	if err != nil || len(gachaInfo["name"]) != 1 {
		t.Fatalf("rejected restore changed gachaInfo=%v err=%v", gachaInfo, err)
	}
}
