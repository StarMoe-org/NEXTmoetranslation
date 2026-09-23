package lyricsextractionplan

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// RecoveryFixtureManifestV1 and RecoveryFixtureIdentityV1 retain the historical
// decoder contract. Recovery source closure v2 never prepares from this shape.
type RecoveryFixtureManifestV1 struct {
	SchemaVersion   int                         `json:"schemaVersion"`
	SelectionPolicy string                      `json:"selectionPolicy"`
	Fixtures        []RecoveryFixtureIdentityV1 `json:"fixtures"`
}

type RecoveryFixtureIdentityV1 struct {
	Path       string `json:"path"`
	Format     string `json:"format"`
	PageID     int    `json:"pageId"`
	RevisionID int    `json:"revisionId"`
	SHA1       string `json:"sha1"`
}

// RecoveryFixtureManifestV2 pins the raw bytes and the derived content identity
// for every reviewed testdata input admitted to the recovery source closure.
type RecoveryFixtureManifestV2 struct {
	SchemaVersion     int                         `json:"schemaVersion"`
	SelectionPolicy   string                      `json:"selectionPolicy"`
	SnapshotAlgorithm string                      `json:"snapshotAlgorithm"`
	Fixtures          []RecoveryFixtureIdentityV2 `json:"fixtures"`
}

type RecoveryFixtureIdentityV2 struct {
	Path          string `json:"path"`
	Format        string `json:"format"`
	RawSizeBytes  int64  `json:"rawSizeBytes"`
	RawSHA256     string `json:"rawSha256"`
	ContentSHA1   string `json:"contentSha1"`
	ContentSHA256 string `json:"contentSha256"`
	PageID        int    `json:"pageId,omitempty"`
	RevisionID    int    `json:"revisionId,omitempty"`
}

// DecodeRecoveryFixtureManifestV1 preserves the historical canonical decoder.
func DecodeRecoveryFixtureManifestV1(body []byte) (RecoveryFixtureManifestV1, error) {
	var manifest RecoveryFixtureManifestV1
	if len(body) == 0 || len(body) > MaxRecoveryFixtureManifestBytes || !utf8.Valid(body) {
		return manifest, errors.New("recovery fixture manifest exceeds its encoded boundary")
	}
	if err := inspectStrictJSON(body, MaxRecoveryFixtureJSONDepth); err != nil {
		return manifest, fmt.Errorf("inspect recovery fixture manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode recovery fixture manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return manifest, errors.New("recovery fixture manifest contains trailing JSON")
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(body, canonical) {
		return manifest, errors.New("recovery fixture manifest is not canonical ordered JSON")
	}
	if err := validateRecoveryFixtureManifestV1(manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func validateRecoveryFixtureManifestV1(manifest RecoveryFixtureManifestV1) error {
	if manifest.SchemaVersion != RecoveryFixtureManifestSchemaVersionV1 ||
		manifest.SelectionPolicy != RecoverySourceSelectionPolicyV1 || manifest.Fixtures == nil ||
		len(manifest.Fixtures) == 0 || len(manifest.Fixtures) > MaxRecoveryFixtureFiles {
		return errors.New("recovery fixture manifest version or fixture count is unsupported")
	}
	lastPath := ""
	for index, fixture := range manifest.Fixtures {
		if !validDataPath(fixture.Path) || !strings.HasPrefix(fixture.Path, "server/") ||
			!strings.Contains(fixture.Path, "/testdata/") || path.Ext(fixture.Path) != ".json" ||
			fixture.Path == recoveryFixtureManifestPathV1 || index > 0 && fixture.Path <= lastPath {
			return errors.New("recovery fixture manifest paths must be unique canonical testdata JSON paths in ascending order")
		}
		lastPath = fixture.Path
		if fixture.Format != RecoveryFixtureFormatMediaWikiPageV1 || fixture.PageID <= 0 || fixture.PageID > MaxMediaWikiIdentity ||
			fixture.RevisionID <= 0 || fixture.RevisionID > MaxMediaWikiIdentity || !canonicalSHA1.MatchString(fixture.SHA1) {
			return fmt.Errorf("recovery fixture %q has an invalid structural identity", fixture.Path)
		}
	}
	return nil
}

func validateRecoveryFixtureBody(body []byte, expected RecoveryFixtureIdentityV1) error {
	if len(body) == 0 || len(body) > MaxSourceFileBytes || !utf8.Valid(body) {
		return errors.New("fixture body exceeds its JSON boundary")
	}
	if err := inspectStrictJSON(body, MaxRecoveryFixtureJSONDepth); err != nil {
		return err
	}
	page, err := decodeSingleMediaWikiFixture(body)
	if err != nil {
		return err
	}
	if page.PageID != expected.PageID || page.LastRevID != expected.RevisionID ||
		page.RevisionID != expected.RevisionID || page.SHA1 != expected.SHA1 || page.Content == "" {
		return errors.New("fixture revision identity or main content does not match its manifest")
	}
	return nil
}

// DecodeRecoveryFixtureManifestV2 rejects duplicate-key, unknown-field,
// unordered, noncanonical, unversioned, or structurally unsafe manifests.
func DecodeRecoveryFixtureManifestV2(body []byte) (RecoveryFixtureManifestV2, error) {
	var manifest RecoveryFixtureManifestV2
	if len(body) == 0 || len(body) > MaxRecoveryFixtureManifestBytesV2 || !utf8.Valid(body) {
		return manifest, errors.New("recovery fixture manifest v2 exceeds its encoded boundary")
	}
	if err := inspectStrictJSON(body, MaxRecoveryFixtureJSONDepth); err != nil {
		return manifest, fmt.Errorf("inspect recovery fixture manifest v2: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode recovery fixture manifest v2: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return manifest, errors.New("recovery fixture manifest v2 contains trailing JSON")
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(body, canonical) {
		return manifest, errors.New("recovery fixture manifest v2 is not canonical ordered JSON")
	}
	if err := validateRecoveryFixtureManifestV2(manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func validateRecoveryFixtureManifestV2(manifest RecoveryFixtureManifestV2) error {
	policy := CompiledRecoverySourceSelectionPolicy()
	if manifest.SchemaVersion != policy.FixtureManifestVersion || manifest.SelectionPolicy != policy.Version ||
		manifest.SnapshotAlgorithm != policy.SnapshotAlgorithm || manifest.Fixtures == nil ||
		len(manifest.Fixtures) == 0 || len(manifest.Fixtures) > MaxRecoveryFixtureFilesV2 {
		return errors.New("recovery fixture manifest v2 version or fixture count is unsupported")
	}
	allowedRoots := make(map[string]struct{}, len(policy.PackageRoots))
	for _, packageRoot := range policy.PackageRoots {
		allowedRoots[packageRoot] = struct{}{}
	}
	lastPath := ""
	for index, fixture := range manifest.Fixtures {
		packageRoot, underTestdata := recoveryFixturePackageRoot(fixture.Path)
		_, allowedRoot := allowedRoots[packageRoot]
		if !validDataPath(fixture.Path) || !underTestdata || !allowedRoot || fixture.Path == policy.FixtureManifestPath ||
			sourceBuildExtensionAllowed(path.Ext(fixture.Path), policy.BuildFileExtensions) ||
			index > 0 && fixture.Path <= lastPath {
			return errors.New("recovery fixture manifest v2 paths must be unique reviewed testdata files in ascending order")
		}
		lastPath = fixture.Path
		if fixture.RawSizeBytes <= 0 || fixture.RawSizeBytes > policy.MaximumFileBytes ||
			!canonicalSHA256.MatchString(fixture.RawSHA256) || !canonicalSHA1.MatchString(fixture.ContentSHA1) ||
			!canonicalSHA256.MatchString(fixture.ContentSHA256) {
			return fmt.Errorf("recovery fixture %q has an invalid raw or content identity", fixture.Path)
		}
		switch fixture.Format {
		case RecoveryFixtureFormatMediaWikiPageV1:
			if path.Ext(fixture.Path) != ".json" || fixture.PageID <= 0 || fixture.PageID > MaxMediaWikiIdentity ||
				fixture.RevisionID <= 0 || fixture.RevisionID > MaxMediaWikiIdentity {
				return fmt.Errorf("recovery fixture %q has an invalid MediaWiki identity", fixture.Path)
			}
		case RecoveryFixtureFormatRawFileV1:
			if fixture.PageID != 0 || fixture.RevisionID != 0 {
				return fmt.Errorf("recovery fixture %q carries unsupported raw-fixture routing data", fixture.Path)
			}
		default:
			return fmt.Errorf("recovery fixture %q has an unsupported format", fixture.Path)
		}
	}
	return nil
}

func validateRecoveryFixtureBodyV2(body []byte, expected RecoveryFixtureIdentityV2) error {
	if len(body) == 0 || len(body) > MaxSourceFileBytes || int64(len(body)) != expected.RawSizeBytes {
		return errors.New("fixture raw size does not match its nonempty manifest pin")
	}
	rawDigest := sha256.Sum256(body)
	if hex.EncodeToString(rawDigest[:]) != expected.RawSHA256 {
		return errors.New("fixture raw SHA-256 does not match its manifest pin")
	}
	var content []byte
	switch expected.Format {
	case RecoveryFixtureFormatRawFileV1:
		content = body
	case RecoveryFixtureFormatMediaWikiPageV1:
		if !utf8.Valid(body) {
			return errors.New("MediaWiki fixture is not valid UTF-8")
		}
		if err := inspectStrictJSON(body, MaxRecoveryFixtureJSONDepth); err != nil {
			return err
		}
		page, err := decodeSingleMediaWikiFixture(body)
		if err != nil {
			return err
		}
		if page.PageID != expected.PageID || page.LastRevID != expected.RevisionID ||
			page.RevisionID != expected.RevisionID || page.Content == "" || !canonicalSHA1.MatchString(page.SHA1) {
			return errors.New("fixture MediaWiki page/revision identity does not match its manifest")
		}
		content = []byte(page.Content)
		contentDigest := sha1.Sum(content)
		actualContentSHA1 := hex.EncodeToString(contentDigest[:])
		if page.SHA1 != actualContentSHA1 {
			return errors.New("fixture MediaWiki revision SHA-1 does not match recomputed main-slot content")
		}
	default:
		return errors.New("fixture format is unsupported")
	}
	contentSHA1 := sha1.Sum(content)
	contentSHA256 := sha256.Sum256(content)
	if hex.EncodeToString(contentSHA1[:]) != expected.ContentSHA1 ||
		hex.EncodeToString(contentSHA256[:]) != expected.ContentSHA256 {
		return errors.New("fixture content SHA-1 or SHA-256 does not match its manifest pin")
	}
	return nil
}

type decodedMediaWikiFixture struct {
	PageID     int
	LastRevID  int
	RevisionID int
	SHA1       string
	Content    string
}

func decodeSingleMediaWikiFixture(body []byte) (decodedMediaWikiFixture, error) {
	var envelope struct {
		BatchComplete bool `json:"batchcomplete"`
		Query         struct {
			Pages []struct {
				PageID    int `json:"pageid"`
				LastRevID int `json:"lastrevid"`
				Revisions []struct {
					RevisionID int    `json:"revid"`
					SHA1       string `json:"sha1"`
					Slots      map[string]struct {
						Content string `json:"content"`
					} `json:"slots"`
				} `json:"revisions"`
			} `json:"pages"`
		} `json:"query"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return decodedMediaWikiFixture{}, fmt.Errorf("decode fixture envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return decodedMediaWikiFixture{}, errors.New("fixture envelope contains trailing JSON")
	}
	if !envelope.BatchComplete || len(envelope.Query.Pages) != 1 {
		return decodedMediaWikiFixture{}, errors.New("fixture must contain one complete page envelope")
	}
	page := envelope.Query.Pages[0]
	if len(page.Revisions) != 1 {
		return decodedMediaWikiFixture{}, errors.New("fixture must contain one exact revision")
	}
	revision := page.Revisions[0]
	mainSlot, hasMain := revision.Slots["main"]
	if !hasMain {
		return decodedMediaWikiFixture{}, errors.New("fixture revision has no main slot")
	}
	return decodedMediaWikiFixture{
		PageID: page.PageID, LastRevID: page.LastRevID, RevisionID: revision.RevisionID,
		SHA1: revision.SHA1, Content: mainSlot.Content,
	}, nil
}

func inspectStrictJSON(body []byte, maximumDepth int) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := inspectStrictJSONValue(decoder, 1, maximumDepth); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func inspectStrictJSONValue(decoder *json.Decoder, depth, maximumDepth int) error {
	if depth > maximumDepth {
		return fmt.Errorf("JSON exceeds maximum nesting depth %d", maximumDepth)
	}
	tokenValue, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := tokenValue.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object contains non-string key")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("JSON field %q is duplicated", key)
			}
			seen[key] = struct{}{}
			if err := inspectStrictJSONValue(decoder, depth+1, maximumDepth); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := inspectStrictJSONValue(decoder, depth+1, maximumDepth); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func sortedFixtureIdentities(fixtures []RecoveryFixtureIdentityV2) []RecoveryFixtureIdentityV2 {
	result := append([]RecoveryFixtureIdentityV2(nil), fixtures...)
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result
}
