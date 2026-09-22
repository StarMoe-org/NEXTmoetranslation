package backup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/config"
	"moesekai/server/internal/model"
)

// fakeS3Bucket serves the object semantics the backup manager relies on:
// PUT stores, GET returns 404 for missing keys, DELETE removes.
type fakeS3Bucket struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3Bucket(t *testing.T) (*fakeS3Bucket, string) {
	t.Helper()
	bucket := &fakeS3Bucket{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bucket.mu.Lock()
		defer bucket.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			bucket.objects[r.URL.Path] = body
		case http.MethodDelete:
			delete(bucket.objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			object, ok := bucket.objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`))
				return
			}
			_, _ = w.Write(object)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return bucket, server.URL
}

func (b *fakeS3Bucket) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.objects))
	for key := range b.objects {
		keys = append(keys, key)
	}
	return keys
}

func (b *fakeS3Bucket) has(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objects[key]
	return ok
}

func TestS3RestoreAfterEnablingEncryptionReadsTheNewerArtifact(t *testing.T) {
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
	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "明文旧版", model.SourceHuman, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}
	if !bucket.has("/test-bucket/snapshots/latest.tar.gz") {
		t.Fatalf("plaintext backup keys = %v", bucket.keys())
	}

	t.Setenv(backupEncryptionKeyEnv, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "加密新版", model.SourceHuman, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "被覆盖", model.SourceLLM, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.restoreS3(); err != nil {
		t.Fatal(err)
	}
	category, err := h.store.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	if got := category["prefix"]["こんにちは"]; got.Text != "加密新版" {
		t.Fatalf("restored entry = %+v, want the encrypted backup content", got)
	}
	if !bucket.has("/test-bucket/snapshots/latest.enc") || bucket.has("/test-bucket/snapshots/latest.tar.gz") {
		t.Fatalf("encrypted backup did not supersede the plaintext pointer: %v", bucket.keys())
	}
}

func TestS3RestoreAfterDisablingEncryptionReadsThePlaintextArtifact(t *testing.T) {
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
	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "加密旧版", model.SourceHuman, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}

	t.Setenv(backupEncryptionKeyEnv, "")
	if _, err := h.store.UpdateEntry("cards", "prefix", "こんにちは", "明文新版", model.SourceHuman, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.backupS3(); err != nil {
		t.Fatal(err)
	}
	if bucket.has("/test-bucket/snapshots/latest.enc") {
		t.Fatalf("plaintext backup left the encrypted pointer behind: %v", bucket.keys())
	}
	if _, err := h.manager.restoreS3(); err != nil {
		t.Fatal(err)
	}
	category, err := h.store.CategoryData("cards")
	if err != nil {
		t.Fatal(err)
	}
	if got := category["prefix"]["こんにちは"]; got.Text != "明文新版" {
		t.Fatalf("restored entry = %+v, want the plaintext backup content", got)
	}
	for _, key := range bucket.keys() {
		if strings.HasSuffix(key, "/latest.enc") {
			t.Fatalf("stale encrypted pointer survived: %v", bucket.keys())
		}
	}
}
