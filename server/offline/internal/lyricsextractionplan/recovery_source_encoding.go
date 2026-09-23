package lyricsextractionplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RecoverySourceSnapshotSHA256 is the recovery-only v2 digest. Unlike the
// legacy snapshot-v1 digest, it admits exact zero-byte ordinary, embed, and
// go.sum identities while retaining the same ordered JSON identity shape.
func RecoverySourceSnapshotSHA256(files []SourceFileIdentity) (string, error) {
	if files == nil || len(files) == 0 || len(files) > MaxSourceSnapshotFiles {
		return "", errors.New("recovery source snapshot file identities are required")
	}
	var total int64
	lastPath := ""
	for index, file := range files {
		if !validDataPath(file.Path) || index > 0 && file.Path <= lastPath {
			return "", errors.New("recovery source snapshot file identities must use unique paths in ascending order")
		}
		lastPath = file.Path
		if file.SizeBytes < 0 || file.SizeBytes > MaxSourceFileBytes || !canonicalSHA256.MatchString(file.SHA256) {
			return "", fmt.Errorf("recovery source snapshot file %q has an invalid exact identity", file.Path)
		}
		if file.SizeBytes > MaxSourceSnapshotBytes-total {
			return "", errors.New("recovery source snapshot exceeds its total byte ceiling")
		}
		total += file.SizeBytes
	}
	body, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("encode recovery source snapshot identities: %w", err)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(RecoverySourceSnapshotAlgorithmV2 + "\x00"))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func validateRecoverySourceSnapshot(snapshot SourceSnapshot) (time.Time, error) {
	if snapshot.Algorithm != RecoverySourceSnapshotAlgorithmV2 {
		return time.Time{}, errors.New("recovery source snapshot uses an unsupported digest algorithm")
	}
	capturedAt, err := parseCanonicalTimestamp(snapshot.CapturedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("recovery source snapshot capturedAt: %w", err)
	}
	if snapshot.Files == nil || len(snapshot.Files) == 0 || len(snapshot.Files) > MaxSourceSnapshotFiles {
		return time.Time{}, errors.New("recovery source snapshot has an invalid file count")
	}
	if !canonicalSHA256.MatchString(snapshot.SHA256) {
		return time.Time{}, errors.New("recovery source snapshot has a noncanonical SHA-256")
	}
	digest, err := RecoverySourceSnapshotSHA256(snapshot.Files)
	if err != nil {
		return time.Time{}, err
	}
	if digest != snapshot.SHA256 {
		return time.Time{}, errors.New("recovery source snapshot digest does not match its exact file identities")
	}
	return capturedAt, nil
}
