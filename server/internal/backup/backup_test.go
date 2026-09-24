package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/importer"
	"moesekai/server/internal/store"
)

func TestTarGzRoundTrip(t *testing.T) {
	src := t.TempDir()
	// Build a small translations-like tree.
	writeFile(t, filepath.Join(src, "cards.json"), `{"prefix":{"こんにちは":"你好"}}`)
	writeFile(t, filepath.Join(src, "eventStory", "event_1.json"), `{"meta":{"source":"official_cn"}}`)

	data, err := tarGzDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty tarball")
	}

	dest := t.TempDir()
	if err := untarGz(data, dest); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dest, "cards.json"))
	if got != `{"prefix":{"こんにちは":"你好"}}` {
		t.Errorf("cards.json mismatch: %q", got)
	}
	got2 := readFile(t, filepath.Join(dest, "eventStory", "event_1.json"))
	if got2 != `{"meta":{"source":"official_cn"}}` {
		t.Errorf("event_1.json mismatch: %q", got2)
	}
}

func TestUntarGzRejectsTraversal(t *testing.T) {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := untarGz(compressed.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("traversal archive error = %v", err)
	}
}

func TestSigV4KeyDeterministic(t *testing.T) {
	// SigV4 signing key derivation must be stable for identical inputs.
	k1 := sigv4Key("secret", "20260531", "us-east-1", "s3")
	k2 := sigv4Key("secret", "20260531", "us-east-1", "s3")
	if string(k1) != string(k2) {
		t.Error("signing key not deterministic")
	}
	k3 := sigv4Key("secret", "20260601", "us-east-1", "s3")
	if string(k1) == string(k3) {
		t.Error("signing key should differ by date")
	}
}

func TestGitErrorsRedactCredentialArgumentsAndOutput(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	secret := "credential-that-must-not-leak"
	err := git(t.TempDir(), "remote", "add", "origin", "https://"+secret+"@github.com/owner/repo.git")
	if err == nil {
		t.Fatal("git outside a repository unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "https://***@github.com") {
		t.Fatalf("credential was not redacted: %v", err)
	}
}

func TestGitRemoteErrorsRedactEntireConfiguredURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	for _, repoURL := range []string{
		"https://backup-token@example.invalid/owner/repo.git?access_token=query-secret#fragment-secret",
		"ssh://deploy-user@example.invalid/owner/private-repo.git",
	} {
		err := gitRemoteContext(context.Background(), t.TempDir(), repoURL,
			"remote", "add", "origin", repoURL)
		if err == nil {
			t.Fatalf("git outside a repository unexpectedly succeeded for %q", repoURL)
		}
		message := err.Error()
		if strings.Contains(message, repoURL) || strings.Contains(message, "backup-token") ||
			strings.Contains(message, "query-secret") || strings.Contains(message, "fragment-secret") ||
			strings.Contains(message, "deploy-user") || strings.Contains(message, "private-repo") ||
			!strings.Contains(message, "<redacted-repository-url>") {
			t.Fatalf("repository URL was not fully redacted: %v", err)
		}
	}
}

func TestGitRemoteContextHonorsCancellationWithoutLeakingRepositoryURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	repoURL := "https://cancel-secret@example.invalid/owner/cancel-private.git?token=query-secret"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := gitRemoteContext(ctx, t.TempDir(), repoURL, "clone", repoURL, filepath.Join(t.TempDir(), "repo"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled remote git error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("canceled remote git took too long: %s", time.Since(started))
	}
	message := err.Error()
	for _, secret := range []string{repoURL, "cancel-secret", "cancel-private", "query-secret"} {
		if strings.Contains(message, secret) {
			t.Fatalf("canceled remote git leaked %q in %q", secret, message)
		}
	}
	if !strings.Contains(message, "<redacted-repository-url>") {
		t.Fatalf("canceled remote git omitted redacted URL marker: %v", err)
	}
}

func TestRunGitContextForcesNoninteractiveCredentialEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell credential helper fixture requires POSIX sh")
	}
	bin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "git-env.txt")
	script := filepath.Join(bin, "git")
	body := `#!/bin/sh
printf '%s\n%s\n%s\n%s\n%s\n%s\n' "$GIT_TERMINAL_PROMPT" "$GCM_INTERACTIVE" "$GIT_ASKPASS" "$SSH_ASKPASS" "$MOESEKAI_BACKUP_ENCRYPTION_KEY" "$GIT_SSH_COMMAND" > "$GIT_ENV_CAPTURE"
exit 7
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_ENV_CAPTURE", capture)
	t.Setenv(backupEncryptionKeyEnv, "must-not-reach-git-subprocess")
	err := gitRemoteContext(context.Background(), t.TempDir(), "https://secret@example.invalid/private.git",
		"clone", "https://secret@example.invalid/private.git", filepath.Join(t.TempDir(), "repo"))
	if err == nil {
		t.Fatal("fake git unexpectedly succeeded")
	}
	values := strings.Split(strings.TrimSpace(readFile(t, capture)), "\n")
	want := []string{"0", "never", "", "", "", "ssh -oBatchMode=yes"}
	if len(values) != len(want) {
		t.Fatalf("captured git environment = %#v", values)
	}
	for index := range want {
		if values[index] != want[index] {
			t.Fatalf("git environment[%d]=%q want %q", index, values[index], want[index])
		}
	}
}

func TestSanitizeGitRedactsURLQueryFragmentAndSchemeUserinfo(t *testing.T) {
	input := "failed https://token@example.invalid/repo.git?secret=one#two ssh://deploy@example.invalid/private.git"
	got := sanitizeGit(input)
	for _, secret := range []string{"token", "secret=one", "two", "deploy"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitizeGit(%q) leaked %q in %q", input, secret, got)
		}
	}
	if !strings.Contains(got, "https://***@example.invalid/repo.git?<redacted>") ||
		!strings.Contains(got, "ssh://***@example.invalid/private.git") {
		t.Fatalf("sanitizeGit(%q) = %q", input, got)
	}
}

func TestGitRestoreTreeRejectsUnsafeAndOversizedEntries(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, root string) {
				if err := os.Symlink(filepath.Join(root, "outside.json"), filepath.Join(root, "translations", "cards.json")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
			want: "non-regular",
		},
		{
			name: "file limit",
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "translations", "cards.json"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(filepath.Join(root, "translations", "cards.json"), maxArchiveFileBytes+1); err != nil {
					t.Fatal(err)
				}
			},
			want: "exceeds",
		},
		{
			name: "aggregate limit",
			setup: func(t *testing.T, root string) {
				for index := 0; index < int(maxArchiveExpandedBytes/maxArchiveFileBytes)+1; index++ {
					path := filepath.Join(root, "translations", fmt.Sprintf("part-%d.json", index))
					if err := os.WriteFile(path, nil, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Truncate(path, maxArchiveFileBytes); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: "aggregate",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "translations"), 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, root)
			if err := validateGitRestoreTree(root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe tree error = %v", err)
			}
		})
	}
}

func TestRestoreSizeLimitsAreScopedToBoundedTranslationContentFiles(t *testing.T) {
	for _, name := range []string{
		"translation-content/event-stories.json",
		"translations/translation-content/event-stories.json",
	} {
		if got := archiveFileByteLimit(name); got != maxEventStoriesContentFileBytes {
			t.Fatalf("event story content %q limit = %d", name, got)
		}
	}
	for _, name := range []string{
		"translation-content/lyrics.json",
		"translations/translation-content/lyrics.json",
	} {
		if got := archiveFileByteLimit(name); got != maxLyricsContentFileBytes {
			t.Fatalf("lyrics content %q limit = %d", name, got)
		}
	}
	for _, name := range []string{
		"translations/event-stories.json",
		"translation-content/nested/event-stories.json",
		"nested/translation-content/event-stories.json",
		"translation-content/event-stories.json.bak",
		"translation-content/nested/lyrics.json",
		"nested/translation-content/lyrics.json",
		"translation-content/lyrics.json.bak",
	} {
		if got := archiveFileByteLimit(name); got != maxArchiveFileBytes {
			t.Fatalf("ordinary file %q limit = %d", name, got)
		}
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "translations"), 0o700); err != nil {
		t.Fatal(err)
	}
	contentDir := filepath.Join(root, "translation-content")
	if err := os.MkdirAll(contentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(contentDir, "event-stories.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxArchiveFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := validateGitRestoreTree(root); err != nil {
		t.Fatalf("scoped event story content limit: %v", err)
	}
}

func TestTarWriterRejectsPayloadItsRestoreCannotAccept(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ordinary.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxArchiveFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := tarGzDir(root); err == nil || !strings.Contains(err.Error(), "archive file") {
		t.Fatalf("oversized backup error = %v", err)
	}
}

func TestUntarGzRejectsDirectoryWithDataSize(t *testing.T) {
	var raw bytes.Buffer
	tarWriter := tar.NewWriter(&raw)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "bad-dir", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	header := raw.Bytes()[:512]
	copy(header[124:136], []byte("00000000001\x00"))
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	checksum := 0
	for _, value := range header {
		checksum += int(value)
	}
	copy(header[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := untarGz(compressed.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "non-zero size") {
		t.Fatalf("directory size error = %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestUntarGzRejectsOversizedFile(t *testing.T) {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	size := int64(maxArchiveFileBytes + 1)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "too-large", Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tarWriter, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := untarGz(compressed.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "archive file") {
		t.Fatalf("oversized archive error = %v", err)
	}
}

func TestTranslationContentPreflightCountsRecoveryAndRenditionLyricsGraph(t *testing.T) {
	body := []byte(`{"music":[{}],"performers":[],"documents":[],"lines":[],"segments":[],"publications":[],` +
		`"sourceDocuments":[],"sourceArtifacts":[],"sourceIndexEvidence":[],"sourceArtifactEvidence":[],"sourceContributions":[],` +
		`"renditionLocalizations":[{}],"renditionTranslationLines":[{}],` +
		`"translationEditionStates":[{}],"translationEditions":[{}],` +
		`"translationEditionLocalizations":[{}],"translationEditionLines":[{}],` +
		`"recoveryBatches":[{}],"recoveryItems":[{}],"recoverySourceEvidence":[{}],` +
		`"recoveryArtifacts":[{}],"recoveryArtifactEvidence":[{}],"recoveryContributions":[{}],` +
		`"availabilityDocuments":[{}],"recoveryTakeovers":[{}]}`)
	count, scenarios, total, err := preflightTranslationContentJSON("lyrics.json", body, maxTranslationContentRecords)
	if err != nil {
		t.Fatal(err)
	}
	if count != 15 || scenarios != 0 || total != 15 {
		t.Fatalf("recovery/rendition/edition lyrics preflight count=%d scenarios=%d total=%d", count, scenarios, total)
	}
	if got := lyricsContentCount(store.LyricsContentExport{
		RenditionLocalizations:          make([]store.LyricsRenditionLocalizationBackupRecord, 2),
		RenditionTranslationLines:       make([]store.LyricsRenditionTranslationLineBackupRecord, 3),
		TranslationEditionStates:        make([]store.LyricsTranslationEditionStateBackupRecord, 1),
		TranslationEditions:             make([]store.LyricsTranslationEditionBackupRecord, 1),
		TranslationEditionLocalizations: make([]store.LyricsTranslationEditionLocalizationBackupRecord, 1),
		TranslationEditionLines:         make([]store.LyricsTranslationEditionLineBackupRecord, 1),
		RecoveryTakeovers:               make([]store.LyricsRecoveryTakeoverBackupRecord, 2),
	}); got != 11 {
		t.Fatalf("lyrics content rendition/edition/takeover record count=%d want=11", got)
	}
	takeovers := store.LyricsContentExport{RecoveryTakeovers: make([]store.LyricsRecoveryTakeoverBackupRecord, 2)}
	takeoverBody, err := json.Marshal(takeovers)
	if err != nil {
		t.Fatal(err)
	}
	declared, _, _, err := preflightTranslationContentJSON("lyrics.json", takeoverBody, maxTranslationContentRecords)
	if err != nil {
		t.Fatal(err)
	}
	if declared != lyricsContentCount(takeovers) || declared != 2 {
		t.Fatalf("takeover preflight count=%d written count=%d want=2", declared, lyricsContentCount(takeovers))
	}
}

func TestTranslationContentPreflightRejectsMatchingExcessiveSmallObjectArray(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "translation-content")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries := []byte("[" + strings.Repeat("{},", maxTranslationContentRecords) + "{}]")
	bodies := map[string][]byte{
		"entries.json":       entries,
		"event-stories.json": []byte(`{"segments":[],"localizations":[],"localeMeta":[],"scenarios":[]}`),
		"lyrics.json":        []byte(`{"music":[],"performers":[],"documents":[],"lines":[],"segments":[],"publications":[]}`),
	}
	manifest := contentManifest{SchemaVersion: translationContentSchemaVersionV1}
	for _, name := range []string{"entries.json", "event-stories.json", "lyrics.json"} {
		body := bodies[name]
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		count := 0
		if name == "entries.json" {
			count = maxTranslationContentRecords + 1
		}
		manifest.Files = append(manifest.Files, contentManifestFile{Path: name, SHA256: hex.EncodeToString(sum[:]), Count: count})
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := readTranslationContent(dir); err == nil || !present || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("excessive matching array present=%v err=%v", present, err)
	}
	if _, _, _, err := preflightTranslationContentJSON("entries.json", entries, maxTranslationContentRecords); err == nil || !strings.Contains(err.Error(), "record count") {
		t.Fatalf("actual excessive record error=%v", err)
	}
	if _, _, _, err := topLevelJSONArrays([]byte(`[{}, {}, {}]`), 2); err == nil || !strings.Contains(err.Error(), "record count") {
		t.Fatalf("record limit error=%v", err)
	}
}

// canceledAfterChecks reports cancellation from its checks-th Err call on, so
// a cancellation lands in the middle of a scan whatever the machine's speed.
type canceledAfterChecks struct {
	context.Context
	checks, calls int
}

func (c *canceledAfterChecks) Err() error {
	c.calls++
	if c.calls >= c.checks {
		return context.Canceled
	}
	return nil
}

func TestLargeTranslationContentPreflightHonorsCancellation(t *testing.T) {
	body := []byte("[" + strings.Repeat("{},", 200_000) + "{}]")
	ctx := &canceledAfterChecks{Context: context.Background(), checks: 1000}
	if _, _, _, err := topLevelJSONArraysContext(ctx, body, maxTranslationContentRecords); !errors.Is(err, context.Canceled) {
		t.Fatalf("large preflight cancellation error = %v", err)
	}
	if ctx.calls != ctx.checks {
		t.Fatalf("the preflight checked the context %d times after the cancellation at check %d", ctx.calls-ctx.checks, ctx.checks)
	}
}

func TestLargeTranslationContentDecodeHonorsCancellation(t *testing.T) {
	body := []byte("[" + strings.Repeat(`{"category":"cards","field":"name","key":"jp","locale":"en-US","text":"value","source":"human"},`, 200_000) + `{}` + "]")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(time.Millisecond)
		cancel()
	}()
	var records []map[string]any
	if err := decodeJSONContext(ctx, body, &records); !errors.Is(err, context.Canceled) {
		t.Fatalf("large decode cancellation error = %v", err)
	}
}

func TestTranslationContentRejectsNestedDuplicateObjectKeys(t *testing.T) {
	for name, body := range map[string][]byte{
		"object in root array":   []byte(`[{"category":"cards","category":"events"}]`),
		"nested object":          []byte(`{"documents":[{"musicId":10,"musicId":11}]}`),
		"object in nested array": []byte(`{"publications":[{"payloadJson":"{}","payloadJson":"[]"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			var decoded any
			if err := decodeJSONContext(context.Background(), body, &decoded); err == nil || !strings.Contains(err.Error(), "duplicate object key") {
				t.Fatalf("duplicate JSON error = %v", err)
			}
			if _, _, _, err := topLevelJSONArrays(body, maxTranslationContentRecords); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("duplicate JSON preflight error = %v", err)
			}
		})
	}
}

func TestTranslationContentRejectsUnknownFields(t *testing.T) {
	var decoded []struct {
		Category string `json:"category"`
	}
	if err := decodeJSONContext(context.Background(), []byte(`[{"category":"cards","unknown":"must fail"}]`), &decoded); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown translation content field error=%v", err)
	}
}

func TestTranslationContentManifestRejectsDuplicateObjectKeys(t *testing.T) {
	for name, manifest := range map[string]string{
		"top level":   `{"schemaVersion":1,"schemaVersion":1,"files":[]}`,
		"nested file": `{"schemaVersion":1,"files":[{"path":"entries.json","path":"entries.json"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "translation-content")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, present, err := readTranslationContent(dir); err == nil || !present || !strings.Contains(err.Error(), "duplicate object key") {
				t.Fatalf("duplicate manifest present=%v err=%v", present, err)
			}
		})
	}
}

func TestTranslationContentManifestSizeIsBounded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "translation-content")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), bytes.Repeat([]byte(" "), maxTranslationContentManifestSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := readTranslationContent(dir); err == nil || !present || !strings.Contains(err.Error(), "manifest exceeds") {
		t.Fatalf("oversized manifest present=%v err=%v", present, err)
	}
}

func TestScheduledBackupDoesNotTreatDisabledTargetsAsSuccess(t *testing.T) {
	h := setupLegacyBackup(t)
	_ = h.cfg.Set(config.KeyBackupGitEnabled, "true")
	h.manager.now = func() time.Time { return time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC) }
	lastSuccessDay := ""
	h.manager.runScheduledIfDue(&lastSuccessDay)
	if lastSuccessDay != "" {
		t.Fatalf("failed attempt consumed day %q", lastSuccessDay)
	}
	_ = h.cfg.Set(config.KeyBackupGitEnabled, "false")
	h.manager.runScheduledIfDue(&lastSuccessDay)
	if lastSuccessDay != "" {
		t.Fatalf("disabled targets consumed scheduler day: %q", lastSuccessDay)
	}
}

func TestBackupAllFailsWhenEveryTargetIsDisabled(t *testing.T) {
	h := setupLegacyBackup(t)
	before := h.manager.Status().LastBackup
	results, err := h.manager.BackupAll()
	if err == nil || !strings.Contains(err.Error(), "no backup targets enabled") {
		t.Fatalf("disabled backup error = %v", err)
	}
	if results["s3"] != "disabled" || results["git"] != "disabled" {
		t.Fatalf("disabled backup results = %#v", results)
	}
	status := h.manager.Status()
	if status.LastBackup != before || status.LastError != err.Error() || status.Running {
		t.Fatalf("disabled backup status = %+v", status)
	}
}

func TestCancelStopsBlockedActiveBackupAndWaits(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-releaseServer:
		}
	}))
	defer func() {
		close(releaseServer)
		server.Close()
	}()
	h := setupLegacyBackup(t)
	for key, value := range map[string]string{
		config.KeyBackupS3Enabled:   "true",
		config.KeyBackupS3Endpoint:  server.URL,
		config.KeyBackupS3Region:    "test-region",
		config.KeyBackupS3Bucket:    "test-bucket",
		config.KeyBackupS3AccessKey: "test-access",
		config.KeyBackupS3SecretKey: "test-secret",
	} {
		if err := h.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	backupDone := make(chan error, 1)
	go func() {
		_, err := h.manager.BackupAll()
		backupDone <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("S3 backup request did not start")
	}
	h.manager.Cancel()
	h.manager.Cancel()
	waitDone := make(chan struct{})
	go func() {
		h.manager.Wait()
		close(waitDone)
	}()
	select {
	case err := <-backupDone:
		if err == nil {
			t.Fatal("canceled backup returned nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("active backup ignored cancellation")
	}
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("backup manager wait did not finish")
	}
	status := h.manager.Status()
	if status.Running || status.LastOperation != "backup" || status.LastFinished == "" ||
		!strings.Contains(status.LastError, context.Canceled.Error()) {
		t.Fatalf("canceled backup terminal status = %+v", status)
	}
}

func TestInvalidRestoreTargetIsRejectedBeforeProducerGate(t *testing.T) {
	h := setupLegacyBackup(t)
	gate, err := editorgate.New()
	if err != nil {
		t.Fatal(err)
	}
	h.manager.SetEditorGate(gate)
	if _, err := h.manager.RestoreFromAs("invalid", "operator"); !errors.Is(err, ErrInvalidRestoreTarget) {
		t.Fatalf("restore error = %v", err)
	}
	status := gate.Status()
	if status.Running || status.Generation != 0 || status.CompletedGeneration != 0 || status.LastRun != "" {
		t.Fatalf("invalid target changed producer gate = %+v", status)
	}
}

func TestAdmittedRestoreRecordsProducerGateFailure(t *testing.T) {
	h := setupLegacyBackup(t)
	gate, err := editorgate.New()
	if err != nil {
		t.Fatal(err)
	}
	h.manager.SetEditorGate(gate)
	release, err := gate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	_, restoreErr := h.manager.RestoreFromAs("s3", "operator")
	release()
	if !errors.Is(restoreErr, editorgate.ErrProducerRunning) {
		t.Fatalf("restore gate error = %v", restoreErr)
	}
	status := h.manager.Status()
	if status.Running || status.LastOperation != "restore:s3" || status.LastFinished == "" ||
		!strings.Contains(status.LastError, editorgate.ErrProducerRunning.Error()) {
		t.Fatalf("terminal gate failure status = %+v", status)
	}
}

func TestRestorePreparationRunsBeforeContentLock(t *testing.T) {
	h := setupLegacyBackup(t)
	releaseContent := h.store.LockContentShared()
	started := time.Now()
	_, err := h.manager.RestoreFromAsContext(context.Background(), "s3", "operator")
	releaseContent()
	if err == nil || !strings.Contains(err.Error(), "s3 backup not fully configured") || time.Since(started) > time.Second {
		t.Fatalf("restore preparation error=%v elapsed=%s", err, time.Since(started))
	}
	status := h.manager.Status()
	if status.Running || !strings.Contains(status.LastError, "s3 backup not fully configured") || status.LastFinished == "" {
		t.Fatalf("terminal preparation failure status = %+v", status)
	}
}

func TestRestoreProducerGateCoversRemotePreparation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	h := setupLegacyBackup(t)
	for key, value := range map[string]string{
		config.KeyBackupS3Endpoint: server.URL, config.KeyBackupS3Region: "region",
		config.KeyBackupS3Bucket: "bucket", config.KeyBackupS3AccessKey: "access",
		config.KeyBackupS3SecretKey: "secret",
	} {
		if err := h.cfg.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := editorgate.New()
	if err != nil {
		t.Fatal(err)
	}
	h.manager.SetEditorGate(gate)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, restoreErr := h.manager.RestoreFromAsContext(ctx, "s3", "operator")
		done <- restoreErr
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("restore preparation did not start remote request")
	}
	status := gate.Status()
	if !status.Running || status.Generation != 1 || status.CompletedGeneration != 0 {
		t.Fatalf("remote preparation producer status = %+v", status)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled remote preparation error = %v", err)
	}
	status = gate.Status()
	if status.Running || status.CompletedGeneration != 1 {
		t.Fatalf("canceled preparation terminal producer status = %+v", status)
	}
}

func TestPreparedRestoreContentWaitIsCancellationAware(t *testing.T) {
	h := setupLegacyBackup(t)
	translations, contentDir, err := h.manager.materializeBackupPayload(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload, result, err := importer.ReadDir(translations)
	if err != nil {
		t.Fatal(err)
	}
	content, present, err := readTranslationContent(contentDir)
	if err != nil {
		t.Fatal(err)
	}
	candidate := restoreCandidate{payload: payload, result: result, content: content, contentPresent: present}
	releaseContent := h.store.LockContentShared()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = h.manager.applyRestoreCandidate(ctx, candidate, "operator")
	releaseContent()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("prepared restore content wait error = %v", err)
	}
}

func TestCanceledArchiveExtractionLeavesNoRunningOperation(t *testing.T) {
	archive, err := tarGzDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := untarGzContext(ctx, archive, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled extraction error = %v", err)
	}
}

func TestArchiveAndCopyLoopsHonorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tarGzDirContext(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled archive error = %v", err)
	}
	if err := copyDirContext(ctx, t.TempDir(), filepath.Join(t.TempDir(), "copy")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled copy error = %v", err)
	}
}

func TestAdmittedOperationRecordsSanitizedTerminalFailure(t *testing.T) {
	h := setupLegacyBackup(t)
	h.manager.mu.Lock()
	h.manager.status.LastError = "stale failure"
	h.manager.mu.Unlock()
	_, finish, err := h.manager.beginOperation(context.Background(), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if status := h.manager.Status(); !status.Running || status.LastError != "" {
		t.Fatalf("newly admitted operation status = %+v", status)
	}
	finish(errors.New("https://credential-that-must-not-leak@example.invalid failed\nwith detail"))
	finish(nil)
	status := h.manager.Status()
	if status.Running || status.LastOperation != "backup" || status.LastFinished == "" ||
		strings.Contains(status.LastError, "credential-that-must-not-leak") || strings.ContainsAny(status.LastError, "\r\n") ||
		!strings.Contains(status.LastError, "https://***@example.invalid") {
		t.Fatalf("sanitized terminal status = %+v", status)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
