package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"moesekai/server/internal/config"
)

func TestBackupEnvelopeRoundTripRejectsWrongKeyAndTampering(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, backupEnvelopeDataKeySize)
	wrongKey := bytes.Repeat([]byte{0x42}, backupEnvelopeDataKeySize)
	plaintext := []byte("private-translation-content-and-provenance-sentinel-2026-07-31")

	first, err := encryptBackupEnvelope(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := encryptBackupEnvelope(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("random data keys and nonces produced identical envelopes")
	}
	if bytes.HasPrefix(first, []byte{0x1f, 0x8b}) || bytes.HasPrefix(first, []byte("{")) || bytes.Contains(first, plaintext) {
		t.Fatal("encrypted envelope exposed plaintext or a directly parseable archive format")
	}
	decrypted, err := decryptBackupEnvelope(first, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted bytes = %q, want %q", decrypted, plaintext)
	}
	clear(decrypted)

	if _, err := decryptBackupEnvelope(first, wrongKey); !errors.Is(err, ErrBackupEnvelopeAuthentication) {
		t.Fatalf("wrong-key error = %v", err)
	}
	for _, test := range []struct {
		name   string
		offset int
	}{
		{name: "authenticated key-wrap metadata", offset: backupEnvelopeWrapNonceOffset},
		{name: "wrapped data key", offset: backupEnvelopePrefixSize},
		{name: "payload ciphertext", offset: backupEnvelopeHeaderSize},
	} {
		t.Run(test.name, func(t *testing.T) {
			tampered := append([]byte(nil), first...)
			tampered[test.offset] ^= 0x80
			if _, err := decryptBackupEnvelope(tampered, key); !errors.Is(err, ErrBackupEnvelopeAuthentication) {
				t.Fatalf("tamper error = %v", err)
			}
		})
	}
}

func TestMissingBackupEncryptionKeySelectsLegacyUnencryptedArtifact(t *testing.T) {
	t.Setenv(backupEncryptionKeyEnv, "")
	key, err := loadBackupEncryptionKey()
	if err != nil || len(key) != 0 {
		t.Fatalf("missing backup encryption key = %d bytes, err=%v", len(key), err)
	}
}

func TestBackupAllRejectsInvalidEncryptionKeyBeforeRemoteIO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git sentinel uses a POSIX shell script")
	}
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "invalid", value: "not-canonical-base64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := setupLegacyBackup(t)
			t.Setenv(backupEncryptionKeyEnv, test.value)

			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			bin := t.TempDir()
			marker := filepath.Join(t.TempDir(), "git-invoked")
			fakeGit := filepath.Join(bin, "git")
			if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_REMOTE_MARKER\"\nexit 97\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GIT_REMOTE_MARKER", marker)

			for key, value := range map[string]string{
				config.KeyBackupS3Enabled:   "true",
				config.KeyBackupS3Endpoint:  server.URL,
				config.KeyBackupS3Region:    "test-region",
				config.KeyBackupS3Bucket:    "test-bucket",
				config.KeyBackupS3AccessKey: "test-access",
				config.KeyBackupS3SecretKey: "test-secret",
				config.KeyBackupGitEnabled:  "true",
				config.KeyBackupGitRepoURL:  "https://example.invalid/private-backup.git",
				config.KeyBackupGitBranch:   "encrypted-backup",
			} {
				if err := h.cfg.Set(key, value); err != nil {
					t.Fatal(err)
				}
			}

			results, err := h.manager.BackupAll()
			if err == nil || (!strings.Contains(err.Error(), "backup encryption key") && !strings.Contains(err.Error(), backupEncryptionKeyEnv)) {
				t.Fatalf("BackupAll error = %v", err)
			}
			if !strings.HasPrefix(results["s3"], "error:") || !strings.HasPrefix(results["git"], "error:") {
				t.Fatalf("BackupAll results = %#v", results)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("invalid key allowed %d S3 requests", got)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid key invoked remote git: %v", statErr)
			}
		})
	}
}

func TestBackupEnvelopeChunkedStreamingAndLegacyV1Compatibility(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, backupEnvelopeDataKeySize)

	// Multi-chunk payload (~2.4 MiB -> 3 AEAD chunks).
	multiChunkPlaintext := make([]byte, 2*backupEnvelopeChunkSize+384*1024+17)
	for i := range multiChunkPlaintext {
		multiChunkPlaintext[i] = byte((i * 31) ^ (i >> 9))
	}

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "payload.tar.gz")
	encPath := filepath.Join(dir, "backup.enc")
	decPath := filepath.Join(dir, "decrypted.tar.gz")
	if err := os.WriteFile(srcPath, multiChunkPlaintext, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := encryptBackupEnvelopeFile(context.Background(), encPath, srcPath, key); err != nil {
		t.Fatal(err)
	}
	if err := decryptBackupEnvelopeFile(context.Background(), decPath, encPath, key); err != nil {
		t.Fatal(err)
	}
	decryptedFileBytes, err := os.ReadFile(decPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decryptedFileBytes, multiChunkPlaintext) {
		t.Fatal("multi-chunk file round-trip mismatch")
	}

	// Swapping chunk 0 and chunk 1 (both 1 MiB + 16 B tag) must fail chunk-index AAD authentication.
	rawEnc, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}
	frameLen := backupEnvelopeChunkSize + backupEnvelopeAuthenticationTagSize
	swapped := bytes.Clone(rawEnc)
	chunk0Start := backupEnvelopeHeaderSize
	chunk1Start := chunk0Start + frameLen
	copy(swapped[chunk0Start:chunk1Start], rawEnc[chunk1Start:chunk1Start+frameLen])
	copy(swapped[chunk1Start:chunk1Start+frameLen], rawEnc[chunk0Start:chunk1Start])
	if _, err := decryptBackupEnvelope(swapped, key); !errors.Is(err, ErrBackupEnvelopeAuthentication) {
		t.Fatalf("swapped chunks error = %v, want %v", err, ErrBackupEnvelopeAuthentication)
	}

	// Legacy single-shot dataAlgorithm == 1 envelope must still decrypt cleanly.
	legacyPlain := []byte("legacy-single-shot-moebak01-artifact-payload")
	dataKey := make([]byte, backupEnvelopeDataKeySize)
	wrapNonce := make([]byte, backupEnvelopeNonceSize)
	dataNonce := make([]byte, backupEnvelopeNonceSize)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rand.Reader, wrapNonce); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rand.Reader, dataNonce); err != nil {
		t.Fatal(err)
	}
	keyIDDigest := sha256.Sum256(key)
	var prefix [backupEnvelopePrefixSize]byte
	copy(prefix[:len(backupEnvelopeMagic)], backupEnvelopeMagic)
	prefix[backupEnvelopeVersionOffset] = backupEnvelopeVersion
	prefix[backupEnvelopeKeyWrapOffset] = backupEnvelopeKeyWrapAlgorithm
	prefix[backupEnvelopeDataAlgorithmOffset] = backupEnvelopeDataAlgorithm
	prefix[backupEnvelopeFlagsOffset] = backupEnvelopeFlags
	copy(prefix[backupEnvelopeKeyIDOffset:backupEnvelopeWrapNonceOffset], keyIDDigest[:backupEnvelopeKeyIDSize])
	copy(prefix[backupEnvelopeWrapNonceOffset:backupEnvelopeDataNonceOffset], wrapNonce)
	copy(prefix[backupEnvelopeDataNonceOffset:backupEnvelopePlaintextSizeOffset], dataNonce)
	binary.BigEndian.PutUint64(prefix[backupEnvelopePlaintextSizeOffset:backupEnvelopePrefixSize], uint64(len(legacyPlain)))

	wrapGCM, err := newAES256GCM(key)
	if err != nil {
		t.Fatal(err)
	}
	wrappedKey := wrapGCM.Seal(nil, wrapNonce, dataKey, prefix[:])
	var header [backupEnvelopeHeaderSize]byte
	copy(header[:backupEnvelopePrefixSize], prefix[:])
	copy(header[backupEnvelopePrefixSize:], wrappedKey)

	dataGCM, err := newAES256GCM(dataKey)
	if err != nil {
		t.Fatal(err)
	}
	legacyEnvelope := make([]byte, 0, backupEnvelopeHeaderSize+len(legacyPlain)+backupEnvelopeAuthenticationTagSize)
	legacyEnvelope = append(legacyEnvelope, header[:]...)
	legacyEnvelope = dataGCM.Seal(legacyEnvelope, dataNonce, legacyPlain, header[:])

	gotLegacy, err := decryptBackupEnvelope(legacyEnvelope, key)
	if err != nil {
		t.Fatalf("legacy v1 decrypt error = %v", err)
	}
	if !bytes.Equal(gotLegacy, legacyPlain) {
		t.Fatalf("legacy v1 decrypted = %q, want %q", gotLegacy, legacyPlain)
	}
	legacyFile := filepath.Join(dir, "legacy.enc")
	legacyOut := filepath.Join(dir, "legacy.out")
	if err := os.WriteFile(legacyFile, legacyEnvelope, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := decryptBackupEnvelopeFile(context.Background(), legacyOut, legacyFile, key); err != nil {
		t.Fatalf("legacy v1 file decrypt error = %v", err)
	}
	gotLegacyFile, err := os.ReadFile(legacyOut)
	if err != nil || !bytes.Equal(gotLegacyFile, legacyPlain) {
		t.Fatalf("legacy v1 file decrypted = %q, err = %v", gotLegacyFile, err)
	}
}
