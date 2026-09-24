package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/importer"
	"moesekai/server/internal/model"
)

const (
	maxS3ResponseBytes              = 256 << 20
	maxArchiveEntries               = 10_000
	maxArchiveFileBytes             = 64 << 20
	maxLyricsContentFileBytes       = 256 << 20
	maxEventStoriesContentFileBytes = 512 << 20
	// side-stories.json measured against the 2026-09 catalog (4.3k stories,
	// 5.6k episodes, 232k lines): 173-192 MiB with the official CN/EN text,
	// about 218 MiB (±15%) once every line has both locales. One oversize file
	// fails the whole backup on both targets, so the limit leaves room for the
	// catalog to double.
	maxSideStoriesContentFileBytes = 512 << 20
	maxArchiveExpandedBytes        = 1 << 30
)

// s3Settings is a snapshot of the S3 target config.
type s3Settings struct {
	endpoint  string // e.g. https://s3.amazonaws.com or https://<account>.r2.cloudflarestorage.com
	region    string
	bucket    string
	prefix    string
	accessKey string
	secretKey string
}

func (m *Manager) s3Config() (s3Settings, error) {
	s := s3Settings{
		endpoint:  strings.TrimRight(m.cfg.GetOr(config.KeyBackupS3Endpoint, "https://s3.amazonaws.com"), "/"),
		region:    m.cfg.GetOr(config.KeyBackupS3Region, "us-east-1"),
		bucket:    m.cfg.Get(config.KeyBackupS3Bucket),
		prefix:    strings.Trim(m.cfg.GetOr(config.KeyBackupS3Prefix, "moesekai-backups"), "/"),
		accessKey: m.cfg.Get(config.KeyBackupS3AccessKey),
		secretKey: m.cfg.Get(config.KeyBackupS3SecretKey),
	}
	if s.bucket == "" || s.accessKey == "" || s.secretKey == "" {
		return s, fmt.Errorf("s3 backup not fully configured (bucket/accessKey/secretKey required)")
	}
	return s, nil
}

func (m *Manager) backupS3() error {
	return m.backupS3Context(context.Background())
}

func (m *Manager) backupS3Context(ctx context.Context) error {
	cfg, err := m.s3Config()
	if err != nil {
		return err
	}
	encryptionKey, err := loadBackupEncryptionKey()
	if err != nil {
		return err
	}
	defer clear(encryptionKey)
	work := filepath.Join(m.workDir, "s3-backup")
	_ = os.RemoveAll(work)
	defer os.RemoveAll(work)
	translationsDir, contentDir, err := m.materializeBackupPayloadContext(ctx, filepath.Join(work, "materialized"))
	if err != nil {
		return err
	}
	artifact, err := encryptBackupPayloadContext(ctx, filepath.Join(work, "target"), backupPayload{
		translationsDir: translationsDir,
		contentDir:      contentDir,
	}, encryptionKey)
	if err != nil {
		return err
	}
	defer clear(artifact)
	return m.publishS3BackupArtifactContext(ctx, cfg, artifact)
}

func encryptBackupPayloadContext(ctx context.Context, work string, payload backupPayload, encryptionKey []byte) ([]byte, error) {
	if err := os.RemoveAll(work); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	payloadDir := filepath.Join(work, "payload")
	if err := copyDirContext(ctx, payload.translationsDir, payloadDir); err != nil {
		return nil, err
	}
	if err := copyDirContext(ctx, payload.contentDir, filepath.Join(payloadDir, "translation-content")); err != nil {
		return nil, err
	}
	tarball, err := tarGzDirContext(ctx, payloadDir)
	if err != nil {
		return nil, err
	}
	if len(encryptionKey) == 0 {
		out := make([]byte, len(tarball))
		copy(out, tarball)
		clear(tarball)
		return out, nil
	}
	defer clear(tarball)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	artifact, err := encryptBackupEnvelope(tarball, encryptionKey)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		clear(artifact)
		return nil, err
	}
	return artifact, nil
}

func (m *Manager) publishS3BackupArtifactContext(ctx context.Context, cfg s3Settings, artifact []byte) error {
	ts := time.Now().UTC().Format("20060102-150405")
	ext, supersededExt := "tar.gz", "enc"
	if !bytes.HasPrefix(artifact, []byte{0x1f, 0x8b}) {
		ext, supersededExt = "enc", "tar.gz"
	}
	key := fmt.Sprintf("%s/translations-%s.%s", cfg.prefix, ts, ext)
	if err := m.s3PutContext(ctx, cfg, key, artifact); err != nil {
		return err
	}
	latestKey := fmt.Sprintf("%s/latest.%s", cfg.prefix, ext)
	if err := m.s3PutContext(ctx, cfg, latestKey, artifact); err != nil {
		return err
	}
	// Turning the encryption key on or off switches the artifact format, so the
	// other format's pointer should go: it would otherwise survive as a stale
	// latest object. The backup is durable at this point and restore already
	// prefers the readable format, so a key without DeleteObject only logs.
	supersededKey := fmt.Sprintf("%s/latest.%s", cfg.prefix, supersededExt)
	if err := m.s3DeleteContext(ctx, cfg, supersededKey); err != nil {
		log.Printf("[backup] delete superseded S3 pointer %s: %v", supersededKey, err)
	}
	return nil
}

func (m *Manager) restoreS3(actors ...string) (importer.Result, error) {
	return m.restoreS3Context(context.Background(), actors...)
}

func (m *Manager) restoreS3Context(ctx context.Context, actors ...string) (importer.Result, error) {
	actor := ""
	if len(actors) > 0 {
		actor = actors[0]
	}
	candidate, err := m.prepareS3RestoreContext(ctx)
	if err != nil {
		return candidate.result, err
	}
	if err := m.applyRestoreCandidate(ctx, candidate, actor); err != nil {
		return candidate.result, err
	}
	return candidate.result, nil
}

func (m *Manager) prepareS3RestoreContext(ctx context.Context) (restoreCandidate, error) {
	cfg, err := m.s3Config()
	if err != nil {
		return restoreCandidate{}, err
	}
	encryptionKey, err := loadBackupEncryptionKey()
	if err != nil {
		return restoreCandidate{}, err
	}
	defer clear(encryptionKey)
	// Prefer the format this process can actually read so a pointer left over
	// from before an encryption-key change cannot shadow the current artifact.
	latestKey := fmt.Sprintf("%s/latest.tar.gz", cfg.prefix)
	fallbackKey := fmt.Sprintf("%s/latest.enc", cfg.prefix)
	if len(encryptionKey) > 0 {
		latestKey, fallbackKey = fallbackKey, latestKey
	}
	data, err := m.s3GetContext(ctx, cfg, latestKey)
	if err != nil {
		var errFallback error
		data, errFallback = m.s3GetContext(ctx, cfg, fallbackKey)
		if errFallback != nil {
			return restoreCandidate{}, fmt.Errorf("s3 restore artifact not found (checked %s and %s): %w", latestKey, fallbackKey, err)
		}
	}
	var archive []byte
	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		archive = data
	} else if len(encryptionKey) > 0 {
		var errDecrypt error
		archive, errDecrypt = decryptBackupEnvelope(data, encryptionKey)
		clear(data)
		if errDecrypt != nil {
			return restoreCandidate{}, errDecrypt
		}
	} else {
		clear(data)
		return restoreCandidate{}, errors.New("s3 backup artifact is encrypted but no encryption key was provided")
	}
	defer clear(archive)
	work := filepath.Join(m.workDir, "s3-restore")
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return restoreCandidate{}, err
	}
	defer os.RemoveAll(work)
	if err := untarGzContext(ctx, archive, work); err != nil {
		return restoreCandidate{}, err
	}
	src, contentDir, err := s3RestoreDirs(ctx, work)
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

func s3RestoreDirs(ctx context.Context, work string) (string, string, error) {
	if ctx == nil {
		return "", "", errors.New("restore context is required")
	}
	rootComplete, err := hasLegacyRestoreLayout(ctx, work)
	if err != nil {
		return "", "", err
	}
	nested := filepath.Join(work, "translations")
	nestedComplete, err := hasLegacyRestoreLayout(ctx, nested)
	if err != nil {
		return "", "", err
	}
	if rootComplete && nestedComplete {
		return "", "", errors.New("restore archive contains ambiguous root and nested translations layouts")
	}
	if !rootComplete && !nestedComplete {
		return "", "", errors.New("restore archive is missing a complete translations layout")
	}
	rootContent := filepath.Join(work, "translation-content")
	nestedContent := filepath.Join(nested, "translation-content")
	rootContentPresent, err := pathExists(rootContent)
	if err != nil {
		return "", "", err
	}
	nestedContentPresent, err := pathExists(nestedContent)
	if err != nil {
		return "", "", err
	}
	if rootContentPresent && nestedContentPresent {
		return "", "", errors.New("restore archive contains ambiguous root and nested translation-content layouts")
	}
	if rootComplete {
		if nestedContentPresent {
			return "", "", errors.New("restore archive contains translation-content outside the root translations layout")
		}
		return work, rootContent, nil
	}
	if rootContentPresent {
		return nested, rootContent, nil
	}
	return nested, nestedContent, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// hasLegacyRestoreLayout reports whether root holds every category file pair.
// A restore-optional category may be absent as a whole, because archives made
// before it existed lack both files; one file without the other is incomplete.
func hasLegacyRestoreLayout(ctx context.Context, root string) (bool, error) {
	if exists, err := pathExistsContext(ctx, filepath.Join(root, "eventStory")); err != nil || !exists {
		return false, err
	}
	for _, category := range model.SupportedCategories {
		flat, err := pathExistsContext(ctx, filepath.Join(root, category+".json"))
		if err != nil {
			return false, err
		}
		full, err := pathExistsContext(ctx, filepath.Join(root, category+".full.json"))
		if err != nil {
			return false, err
		}
		if flat && full {
			continue
		}
		if flat || full || !model.IsRestoreOptionalCategory(category) {
			return false, nil
		}
	}
	return true, nil
}

func pathExistsContext(ctx context.Context, path string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return pathExists(path)
}

// ---- tar.gz helpers ----

func tarGzDir(dir string) ([]byte, error) {
	return tarGzDirContext(context.Background(), dir)
}

func tarGzDirContext(ctx context.Context, dir string) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	entries := 0
	var expanded int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		entries++
		if entries > maxArchiveEntries {
			return fmt.Errorf("archive exceeds %d entries", maxArchiveEntries)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("archive contains non-regular entry %s", rel)
		}
		if info.Mode().IsRegular() {
			fileLimit := archiveFileByteLimit(rel)
			if info.Size() < 0 || info.Size() > fileLimit {
				return fmt.Errorf("archive file %s exceeds %d bytes", rel, fileLimit)
			}
			if expanded+info.Size() > maxArchiveExpandedBytes {
				return fmt.Errorf("archive exceeds %d expanded bytes", maxArchiveExpandedBytes)
			}
			expanded += info.Size()
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := copyWithContext(ctx, tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > maxS3ResponseBytes {
		return nil, fmt.Errorf("archive exceeds %d compressed bytes", maxS3ResponseBytes)
	}
	return buf.Bytes(), nil
}

func untarGz(data []byte, dest string) error {
	return untarGzContext(context.Background(), data, dest)
}

func untarGzContext(ctx context.Context, data []byte, dest string) error {
	if len(data) > maxS3ResponseBytes {
		return fmt.Errorf("archive exceeds %d compressed bytes", maxS3ResponseBytes)
	}
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	entries := 0
	var expanded int64
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveEntries {
			return fmt.Errorf("archive exceeds %d entries", maxArchiveEntries)
		}
		// Guard against path traversal in archive entries.
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		target := filepath.Join(dest, name)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != dest {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		if seen[target] {
			return fmt.Errorf("duplicate path in archive: %s", hdr.Name)
		}
		seen[target] = true
		switch hdr.Typeflag {
		case tar.TypeDir:
			if hdr.Size != 0 {
				return fmt.Errorf("archive directory %s has non-zero size", hdr.Name)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			fileLimit := archiveFileByteLimit(hdr.Name)
			if hdr.Size < 0 || hdr.Size > fileLimit {
				return fmt.Errorf("archive file %s exceeds %d bytes", hdr.Name, fileLimit)
			}
			if expanded+hdr.Size > maxArchiveExpandedBytes {
				return fmt.Errorf("archive exceeds %d expanded bytes", maxArchiveExpandedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, &contextReader{ctx: ctx, reader: tr}, hdr.Size); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			expanded += hdr.Size
		default:
			return fmt.Errorf("unsupported archive entry type %d for %s", hdr.Typeflag, hdr.Name)
		}
	}
	return nil
}

func archiveFileByteLimit(name string) int64 {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "./")
	switch clean {
	case "translation-content/event-stories.json", "translations/translation-content/event-stories.json":
		return maxEventStoriesContentFileBytes
	case "translation-content/lyrics.json", "translations/translation-content/lyrics.json":
		return maxLyricsContentFileBytes
	case "translation-content/side-stories.json", "translations/translation-content/side-stories.json":
		return maxSideStoriesContentFileBytes
	default:
		return maxArchiveFileBytes
	}
}

// ---- minimal S3 SigV4 (PUT/GET single object) ----

func (m *Manager) s3Put(cfg s3Settings, key string, body []byte) error {
	return m.s3PutContext(context.Background(), cfg, key, body)
}

func (m *Manager) s3PutContext(ctx context.Context, cfg s3Settings, key string, body []byte) error {
	return m.s3DoContext(ctx, cfg, http.MethodPut, key, body)
}

func (m *Manager) s3Get(cfg s3Settings, key string) ([]byte, error) {
	return m.s3GetContext(context.Background(), cfg, key)
}

func (m *Manager) s3GetContext(ctx context.Context, cfg s3Settings, key string) ([]byte, error) {
	return m.s3DoRespContext(ctx, cfg, http.MethodGet, key, nil)
}

func (m *Manager) s3DeleteContext(ctx context.Context, cfg s3Settings, key string) error {
	return m.s3DoContext(ctx, cfg, http.MethodDelete, key, nil)
}

func (m *Manager) s3Do(cfg s3Settings, method, key string, body []byte) error {
	return m.s3DoContext(context.Background(), cfg, method, key, body)
}

func (m *Manager) s3DoContext(ctx context.Context, cfg s3Settings, method, key string, body []byte) error {
	_, err := m.s3DoRespContext(ctx, cfg, method, key, body)
	return err
}

// s3DoResp performs a SigV4-signed request to <endpoint>/<bucket>/<key> (path
// style, which works for AWS S3, Cloudflare R2, MinIO, and most S3-compatibles).
func (m *Manager) s3DoResp(cfg s3Settings, method, key string, body []byte) ([]byte, error) {
	return m.s3DoRespContext(context.Background(), cfg, method, key, body)
}

func (m *Manager) s3DoRespContext(ctx context.Context, cfg s3Settings, method, key string, body []byte) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/%s", cfg.endpoint, cfg.bucket, key)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(body)

	host := req.URL.Host
	req.Header.Set("Host", host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", backupEnvelopeMediaType)
	}

	canonicalURI := req.URL.EscapedPath()
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		method, canonicalURI, "", canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, cfg.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := sigv4Key(cfg.secretKey, dateStamp, cfg.region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cfg.accessKey, scope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)

	client := httpx.NewHTTPSCredentialClient(2*time.Minute, 10*time.Second, 12*time.Second, 20*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > int64(maxBackupEnvelopeBytes) {
		return nil, fmt.Errorf("s3 %s %s: response exceeds %d bytes", method, key, maxBackupEnvelopeBytes)
	}
	respBody, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: resp.Body}, int64(maxBackupEnvelopeBytes)+1))
	if readErr != nil {
		return nil, readErr
	}
	if len(respBody) > maxBackupEnvelopeBytes {
		return nil, fmt.Errorf("s3 %s %s: response exceeds %d bytes", method, key, maxBackupEnvelopeBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 %s %s: http %d: %s", method, key, resp.StatusCode, s3ErrMsg(respBody))
	}
	return respBody, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sigv4Key(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func s3ErrMsg(body []byte) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
