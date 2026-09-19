package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/files"
	"moesekai/server/internal/importer"
)

// materializeTranslations writes the current DB-backed legacy category/event
// projection into a fresh "translations" directory under parent. Backup-only
// public lyrics assets are added separately by materializeBackupPayload from
// the same SQLite snapshot.
func (m *Manager) materializeTranslations(parent string) (string, error) {
	return materializeTranslationsWithGenerator(parent, m.gen)
}

func materializeTranslationsWithGenerator(parent string, generator *files.Generator) (string, error) {
	return materializeTranslationsWithGeneratorContext(context.Background(), parent, generator)
}

func materializeTranslationsWithGeneratorContext(ctx context.Context, parent string, generator *files.Generator) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	// Generator.WriteAll writes <outDir>/translation/...; backups historically
	// used a top-level "translations" dir, so generate then point at it.
	gen := generator.WithOutDir(parent)
	if _, err := gen.WriteAllContext(ctx); err != nil {
		return "", err
	}
	// WriteAll produces parent/translation/...; rename to parent/translations.
	src := filepath.Join(parent, "translation")
	dst := filepath.Join(parent, "translations")
	_ = os.RemoveAll(dst)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// ---- GitHub backup (git commit/push) ----

func (m *Manager) backupGit() error {
	return m.backupGitContext(context.Background())
}

func (m *Manager) backupGitContext(ctx context.Context) error {
	repoURL, branch, err := m.gitConfig()
	if err != nil {
		return err
	}
	// An unset key yields (nil, nil); only a malformed one errors, and that must
	// not silently fall through to the unencrypted path.
	encryptionKey, err := loadBackupEncryptionKey()
	if err != nil {
		return err
	}
	defer clear(encryptionKey)
	work := filepath.Join(m.workDir, "git-backup")
	_ = os.RemoveAll(work)
	defer os.RemoveAll(work)
	translationsDir, contentDir, err := m.materializeBackupPayloadContext(ctx, filepath.Join(work, "materialized"))
	if err != nil {
		return err
	}
	repoDir, err := m.prepareGitBackupRepoContext(ctx, filepath.Join(work, "target"), repoURL, branch)
	if err != nil {
		return err
	}
	if len(encryptionKey) > 0 {
		artifact, err := encryptBackupPayloadContext(ctx, filepath.Join(work, "artifact"), backupPayload{
			translationsDir: translationsDir,
			contentDir:      contentDir,
		}, encryptionKey)
		if err != nil {
			return err
		}
		defer clear(artifact)
		return m.publishGitBackupArtifactContext(ctx, repoDir, repoURL, branch, artifact)
	}
	return m.publishGitBackupUnencryptedContext(ctx, repoDir, repoURL, branch, backupPayload{
		translationsDir: translationsDir,
		contentDir:      contentDir,
	})
}

func (m *Manager) gitConfig() (string, string, error) {
	repoURL := m.cfg.Get(config.KeyBackupGitRepoURL)
	branch := m.cfg.GetOr(config.KeyBackupGitBranch, "backup-translations")
	if strings.TrimSpace(repoURL) == "" {
		return "", "", fmt.Errorf("backup git repo url not configured")
	}
	return repoURL, branch, nil
}

func (m *Manager) prepareGitBackupRepoContext(ctx context.Context, work, repoURL, branch string) (string, error) {
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", err
	}
	repoDir := filepath.Join(work, "repo")
	// Clone the backup branch shallowly. If the branch doesn't exist yet, init
	// a fresh repo on that branch instead.
	if err := gitRemoteContext(ctx, work, repoURL, "clone", "--depth", "1", "--branch", branch, repoURL, repoDir); err != nil {
		if err := m.initFreshBackupRepoContext(ctx, repoDir, repoURL, branch); err != nil {
			return "", fmt.Errorf("clone and init both failed: %w", err)
		}
	}
	if err := gitContext(ctx, repoDir, "config", "user.name", "MoeSekai Bot"); err != nil {
		return "", err
	}
	if err := gitContext(ctx, repoDir, "config", "user.email", "bot@moesekai.com"); err != nil {
		return "", err
	}
	return repoDir, nil
}

func (m *Manager) publishGitBackupUnencryptedContext(ctx context.Context, repoDir, repoURL, branch string, payload backupPayload) error {
	destTranslations := filepath.Join(repoDir, "translations")
	_ = os.RemoveAll(destTranslations)
	_ = os.Remove(filepath.Join(repoDir, "backup.tar.gz"))
	_ = os.Remove(filepath.Join(repoDir, backupEnvelopeFilename))
	if err := copyDirContext(ctx, payload.translationsDir, destTranslations); err != nil {
		return err
	}
	if payload.contentDir != "" {
		destContent := filepath.Join(destTranslations, "translation-content")
		if err := copyDirContext(ctx, payload.contentDir, destContent); err != nil {
			return err
		}
	}
	if err := gitContext(ctx, repoDir, "add", "--all"); err != nil {
		return err
	}
	msg := fmt.Sprintf("chore: backup translations %s", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	if err := gitContext(ctx, repoDir, "commit", "-m", msg); err != nil {
		return err
	}
	return gitRemoteContext(ctx, repoDir, repoURL, "push", "origin", branch)
}

func (m *Manager) publishGitBackupArtifactContext(ctx context.Context, repoDir, repoURL, branch string, artifact []byte) error {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name() == ".git" || entry.Name() == ".gitignore" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(repoDir, entry.Name())); err != nil {
			return err
		}
	}
	fileName := "backup.tar.gz"
	if !bytes.HasPrefix(artifact, []byte{0x1f, 0x8b}) {
		fileName = backupEnvelopeFilename
	}
	artifactPath := filepath.Join(repoDir, fileName)
	temporaryPath := artifactPath + ".tmp"
	if err := os.WriteFile(temporaryPath, artifact, 0o600); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, artifactPath); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := gitContext(ctx, repoDir, "add", "--all"); err != nil {
		return err
	}
	msg := fmt.Sprintf("chore: backup translations %s", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	if err := gitContext(ctx, repoDir, "commit", "-m", msg); err != nil {
		return err
	}
	return gitRemoteContext(ctx, repoDir, repoURL, "push", "origin", branch)
}

func (m *Manager) initFreshBackupRepo(repoDir, repoURL, branch string) error {
	return m.initFreshBackupRepoContext(context.Background(), repoDir, repoURL, branch)
}

func (m *Manager) initFreshBackupRepoContext(ctx context.Context, repoDir, repoURL, branch string) error {
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}
	if err := gitContext(ctx, repoDir, "init"); err != nil {
		return err
	}
	if err := gitContext(ctx, repoDir, "checkout", "-b", branch); err != nil {
		return err
	}
	return gitRemoteContext(ctx, repoDir, repoURL, "remote", "add", "origin", repoURL)
}

func (m *Manager) restoreGit(actors ...string) (importer.Result, error) {
	return m.restoreGitContext(context.Background(), actors...)
}

func (m *Manager) restoreGitContext(ctx context.Context, actors ...string) (importer.Result, error) {
	actor := ""
	if len(actors) > 0 {
		actor = actors[0]
	}
	candidate, err := m.prepareGitRestoreContext(ctx)
	if err != nil {
		return candidate.result, err
	}
	if err := m.applyRestoreCandidate(ctx, candidate, actor); err != nil {
		return candidate.result, err
	}
	return candidate.result, nil
}

func (m *Manager) prepareGitRestoreContext(ctx context.Context) (restoreCandidate, error) {
	repoURL, branch, err := m.gitConfig()
	if err != nil {
		return restoreCandidate{}, err
	}
	encryptionKey, err := loadBackupEncryptionKey()
	if err != nil {
		return restoreCandidate{}, err
	}
	defer clear(encryptionKey)
	work := filepath.Join(m.workDir, "git-restore")
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return restoreCandidate{}, err
	}
	defer os.RemoveAll(work)

	repoDir := filepath.Join(work, "repo")
	if err := gitRemoteContext(ctx, work, repoURL, "clone", "--depth", "1", "--branch", branch, repoURL, repoDir); err != nil {
		return restoreCandidate{}, err
	}
	artifact, hasArtifact, err := readGitBackupArtifactContext(ctx, repoDir)
	if err != nil {
		return restoreCandidate{}, err
	}
	extracted := filepath.Join(work, "extracted")
	if err := os.MkdirAll(extracted, 0o700); err != nil {
		return restoreCandidate{}, err
	}
	if hasArtifact {
		var archive []byte
		if bytes.HasPrefix(artifact, []byte{0x1f, 0x8b}) {
			archive = artifact
		} else if len(encryptionKey) > 0 {
			var errDecrypt error
			archive, errDecrypt = decryptBackupEnvelope(artifact, encryptionKey)
			clear(artifact)
			if errDecrypt != nil {
				return restoreCandidate{}, errDecrypt
			}
		} else {
			clear(artifact)
			return restoreCandidate{}, errors.New("git backup artifact is encrypted but no encryption key was provided")
		}
		defer clear(archive)
		if err := untarGzContext(ctx, archive, extracted); err != nil {
			return restoreCandidate{}, err
		}
	} else {
		extracted = repoDir
	}
	src, contentDir, err := s3RestoreDirs(ctx, extracted)
	if err != nil {
		return restoreCandidate{}, err
	}
	content, present, err := readTranslationContentContext(ctx, contentDir)
	if err != nil {
		return restoreCandidate{}, err
	}
	payload, result, err := importer.ReadDirContext(ctx, src)
	if err != nil {
		return restoreCandidate{result: result}, err
	}
	if err := ctx.Err(); err != nil {
		return restoreCandidate{result: result}, err
	}
	return restoreCandidate{payload: payload, result: result, content: content, contentPresent: present}, nil
}

func readGitBackupArtifactContext(ctx context.Context, repoDir string) ([]byte, bool, error) {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, false, err
	}
	var artifactName string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		name := entry.Name()
		if name == ".git" || name == ".gitignore" || name == ".gitattributes" || strings.HasPrefix(name, "README") {
			continue
		}
		if name == "backup.tar.gz" || name == backupEnvelopeFilename {
			if artifactName != "" && artifactName != name {
				return nil, false, fmt.Errorf("git backup checkout contains multiple artifact entries %q and %q", artifactName, name)
			}
			artifactName = name
		}
	}
	if artifactName == "" {
		return nil, false, nil
	}
	file, err := os.Open(filepath.Join(repoDir, artifactName))
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > int64(maxBackupEnvelopeBytes) {
		return nil, false, errors.New("git backup artifact is not a regular file")
	}
	artifact, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, int64(maxBackupEnvelopeBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(artifact) > maxBackupEnvelopeBytes {
		return nil, false, fmt.Errorf("git backup artifact exceeds %d bytes", maxBackupEnvelopeBytes)
	}
	return artifact, true, nil
}

func validateGitRestoreTree(repoDir string) error {
	return validateGitRestoreTreeContext(context.Background(), repoDir)
}

func validateGitRestoreTreeContext(ctx context.Context, repoDir string) error {
	resolvedRoot, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		return fmt.Errorf("resolve git restore root: %w", err)
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return err
	}
	entries := 0
	var totalBytes int64
	for _, rootName := range []string{"translations", "translation-content"} {
		root := filepath.Join(repoDir, rootName)
		if _, err := os.Lstat(root); err != nil {
			if os.IsNotExist(err) && rootName == "translation-content" {
				continue
			}
			return fmt.Errorf("git restore %s: %w", rootName, err)
		}
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			entries++
			if entries > maxArchiveEntries {
				return fmt.Errorf("git restore exceeds %d entries", maxArchiveEntries)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
				return fmt.Errorf("git restore contains non-regular entry %s", path)
			}
			resolvedPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			resolvedPath, err = filepath.Abs(resolvedPath)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(resolvedRoot, resolvedPath)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("git restore entry escapes checkout root: %s", path)
			}
			if info.Mode().IsRegular() {
				fileLimit := archiveFileByteLimit(relative)
				if info.Size() < 0 || info.Size() > fileLimit {
					return fmt.Errorf("git restore file %s exceeds %d bytes", path, fileLimit)
				}
				if totalBytes+info.Size() > maxArchiveExpandedBytes {
					return fmt.Errorf("git restore exceeds %d aggregate bytes", maxArchiveExpandedBytes)
				}
				totalBytes += info.Size()
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// git runs a git command in dir with non-interactive credentials.
func git(dir string, args ...string) error {
	return gitContext(context.Background(), dir, args...)
}

func gitContext(parent context.Context, dir string, args ...string) error {
	return runGitContext(parent, dir, sanitizeGit, args...)
}

func gitRemoteContext(parent context.Context, dir, repoURL string, args ...string) error {
	sanitize := func(value string) string {
		if repoURL != "" {
			value = strings.ReplaceAll(value, repoURL, "<redacted-repository-url>")
		}
		return sanitizeGit(value)
	}
	return runGitContext(parent, dir, sanitize, args...)
}

func runGitContext(parent context.Context, dir string, sanitize func(string) string, args ...string) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	environment := os.Environ()
	filteredEnvironment := make([]string, 0, len(environment)+5)
	secretPrefix := backupEncryptionKeyEnv + "="
	for _, value := range environment {
		if !strings.HasPrefix(value, secretPrefix) {
			filteredEnvironment = append(filteredEnvironment, value)
		}
	}
	cmd.Env = append(filteredEnvironment,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("git %s: %w", sanitize(strings.Join(args, " ")), ctxErr)
		}
		message := sanitize(strings.TrimSpace(string(out)))
		if message == "" {
			return fmt.Errorf("git %s: %v", sanitize(strings.Join(args, " ")), err)
		}
		return fmt.Errorf("git %s: %v: %s", sanitize(strings.Join(args, " ")), err, message)
	}
	return nil
}

var (
	credentialURL      = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
	sensitiveURLSuffix = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^\s?#]+)[?#][^\s]*`)
)

// sanitizeGit masks URL userinfo and query or fragment data in commands,
// stderr, logs, and status payloads.
func sanitizeGit(s string) string {
	s = credentialURL.ReplaceAllString(s, `${1}***@`)
	return sensitiveURLSuffix.ReplaceAllString(s, `${1}?<redacted>`)
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return copyDirContext(context.Background(), src, dst)
}

func copyDirContext(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		destination, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			_ = source.Close()
			return err
		}
		_, copyErr := copyWithContext(ctx, destination, source)
		return errors.Join(copyErr, destination.Close(), source.Close())
	})
}
