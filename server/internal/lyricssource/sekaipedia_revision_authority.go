package lyricssource

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// historicalSekaipediaRubyGeneratorAlias is accepted only at the inbound
// compatibility boundary. Every returned or newly serialized value uses the
// canonical non-romaji vocabulary.
const historicalSekaipediaRubyGeneratorAlias = "sekaipedia-romaji-kana-v1"

// VerifySekaipediaRevisionContent binds revision authority to the immutable
// MediaWiki revision tuple and exact main-slot content.
func VerifySekaipediaRevisionContent(raw []byte, expected FixedIndex) error {
	if expected.PageID <= 0 || expected.RevisionID <= 0 || expected.RevisionTimestamp == "" ||
		!HasCanonicalSHA1(expected.SHA1) || !canonicalIndexEvidenceSHA256.MatchString(expected.ContentSHA256) {
		return errors.New("Sekaipedia revision-content authority is invalid")
	}
	page, err := parsePageResponse(raw)
	if err != nil || page.pageID != expected.PageID || page.revisionID != expected.RevisionID ||
		canonicalFetchedAt(page.revisionTimestamp) != expected.RevisionTimestamp || page.sha1 != expected.SHA1 {
		return ErrRevisionChanged
	}
	content := []byte(page.content)
	contentSHA1 := sha1.Sum(content)
	contentSHA256 := sha256.Sum256(content)
	if hex.EncodeToString(contentSHA1[:]) != expected.SHA1 ||
		hex.EncodeToString(contentSHA256[:]) != expected.ContentSHA256 {
		return ErrRevisionChanged
	}
	return nil
}

// CanonicalPersistedRubyGeneratorVersion normalizes the sole immutable
// historical input alias.
func CanonicalPersistedRubyGeneratorVersion(value string) string {
	if value == historicalSekaipediaRubyGeneratorAlias {
		return historicalSekaipediaRubyGeneratorVersion
	}
	return value
}

// RecoveryPersistedRubyGeneratorVersion maps parser-internal generator names
// to the closed persisted recovery vocabulary.
func RecoveryPersistedRubyGeneratorVersion(value string) (string, error) {
	value = CanonicalPersistedRubyGeneratorVersion(value)
	switch value {
	case "":
		return "", nil
	case historicalRubyGeneratorVersion:
		return historicalRubyGeneratorVersion, nil
	case rubyGeneratorVersion:
		return rubyGeneratorVersion, nil
	case historicalSekaipediaRubyGeneratorVersion:
		return historicalSekaipediaRubyGeneratorVersion, nil
	case sekaipediaRubyGeneratorVersion:
		return sekaipediaRubyGeneratorVersion, nil
	default:
		return "", errors.New("recovery ruby generator version is not registered")
	}
}
