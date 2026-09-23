package lyricsacquisition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricssource"
)

const (
	spoolSchemaVersion                = 2
	manifestSchemaVersion             = 2
	pendingMarkerSchemaVersion        = 2
	maxProviderBytes                  = 64
	maxCanonicalRequestBytes          = 8 << 10
	maxRequestKindBytes               = 32
	maxRevisionSelectorBytes          = 1 << 10
	maxEvidenceIDBytes                = 256
	maxObservedRevisionSelector       = 1 << 10
	maxObservedRevisions              = 256
	maxRawResponseBytes               = 2 << 20
	maxEvidenceProjectionBytes        = 2 << 20
	maxEvidenceEnvelopeBytes          = 4 << 20
	maxManifestBytes                  = 64 << 10
	maxPendingMarkerBytes             = 8 << 10
	maxAcquisitions                   = 65_536
	maxPendingAcquisitions            = 1_024
	maxQuarantineEntries              = maxAcquisitions + maxPendingAcquisitions*6
	maxAggregateRawBytes        int64 = 32 << 30
	maxAggregateEvidence        int64 = 32 << 30
	maxAggregateEnvelope        int64 = 32 << 30
	maxAggregateManifest        int64 = 4 << 30
)

var (
	canonicalSHA256     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalSHA1       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	canonicalEvidenceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

	errSpoolClosed = errors.New("lyrics acquisition spool is closed")
)

type acquisitionRequestKind string

const (
	acquisitionRequestSearch     acquisitionRequestKind = "search"
	acquisitionRequestRevision   acquisitionRequestKind = "revision"
	acquisitionRequestFixedIndex acquisitionRequestKind = "fixed_index"
)

type acquisitionRequest struct {
	provider                 string
	canonicalRequestIdentity string
	kind                     acquisitionRequestKind
	revisionSelector         string
}

type observedRevision struct {
	selector   string
	revisionID int64
	timestamp  string
	sha1       string
}

type evidenceProjection struct {
	evidenceID string
	raw        []byte
	rawSHA256  string
}

type evidenceEnvelope struct {
	raw    []byte
	sha256 string
}

type validatedProviderResponse struct {
	request           acquisitionRequest
	fetchedAt         string
	rawResponse       []byte
	rawResponseSHA256 string
	evidence          evidenceProjection
	envelope          evidenceEnvelope
	observedRevisions []observedRevision
}

type acquiredProviderResponse struct {
	acquisitionID     string
	request           acquisitionRequest
	fetchedAt         string
	rawResponse       []byte
	rawResponseSHA256 string
	evidence          evidenceProjection
	envelope          evidenceEnvelope
	observedRevisions []observedRevision
	replayOnly        bool
}

type manifestRequest struct {
	Provider                 string `json:"provider"`
	CanonicalRequestIdentity string `json:"canonicalRequestIdentity"`
	Kind                     string `json:"kind"`
	RevisionSelector         string `json:"revisionSelector"`
}

type manifestBlob struct {
	SHA256    string `json:"sha256"`
	ByteCount int    `json:"byteCount"`
}

type manifestEvidence struct {
	EvidenceID string `json:"evidenceId"`
	SHA256     string `json:"sha256"`
	ByteCount  int    `json:"byteCount"`
}

type manifestObservedRevision struct {
	Selector   string `json:"selector"`
	RevisionID int64  `json:"revisionId"`
	Timestamp  string `json:"timestamp"`
	SHA1       string `json:"sha1"`
}

type acquisitionManifest struct {
	SchemaVersion      int                        `json:"schemaVersion"`
	Request            manifestRequest            `json:"request"`
	FetchedAt          string                     `json:"fetchedAt"`
	RawResponse        manifestBlob               `json:"rawResponse"`
	EvidenceProjection manifestEvidence           `json:"evidenceRawProjection"`
	EvidenceEnvelope   manifestBlob               `json:"evidenceEnvelope"`
	ObservedRevisions  []manifestObservedRevision `json:"observedRevisions"`
}

type ledgerMigrationCheckpointSummary struct {
	CheckpointSHA256   string `json:"checkpointSha256"`
	CheckpointBytes    int64  `json:"checkpointBytes"`
	CatalogCount       int64  `json:"catalogCount"`
	ResultCount        int64  `json:"resultCount"`
	EvidenceCount      int64  `json:"evidenceCount"`
	EvidenceRawBytes   int64  `json:"evidenceRawBytes"`
	EvidenceJSONBytes  int64  `json:"evidenceJsonBytes"`
	EvidenceRowsSHA256 string `json:"evidenceRowsSha256"`
}

type ledgerMigrationManifest struct {
	SchemaVersion            int                              `json:"schemaVersion"`
	Checkpoint               ledgerMigrationCheckpointSummary `json:"checkpoint"`
	ImportedAcquisitionCount int64                            `json:"importedAcquisitionCount"`
	AcquisitionIDsSHA256     string                           `json:"acquisitionIdsSha256"`
}

type pendingMarker struct {
	SchemaVersion     int    `json:"schemaVersion"`
	AcquisitionID     string `json:"acquisitionId"`
	RequestKey        string `json:"requestKey"`
	RawSHA256         string `json:"rawSha256"`
	RawByteCount      int    `json:"rawByteCount"`
	EvidenceSHA256    string `json:"evidenceSha256"`
	EvidenceByteCount int    `json:"evidenceByteCount"`
	EnvelopeSHA256    string `json:"envelopeSha256"`
	EnvelopeByteCount int    `json:"envelopeByteCount"`
	ManifestSHA256    string `json:"manifestSha256"`
	ManifestByteCount int    `json:"manifestByteCount"`
}

func requestKey(request acquisitionRequest) (string, error) {
	if err := validateRequest(request); err != nil {
		return "", err
	}
	identity := strings.Join([]string{
		"lyrics-acquisition-request-v1",
		request.provider,
		request.canonicalRequestIdentity,
		string(request.kind),
		request.revisionSelector,
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:]), nil
}

func validateRequest(request acquisitionRequest) error {
	switch request.provider {
	case "vocaloid_fandom", "moegirl", "moegirl_public_exact", "sekaipedia":
	default:
		return errors.New("lyrics acquisition provider is invalid")
	}
	if err := validateBoundedText(request.provider, maxProviderBytes, false); err != nil {
		return fmt.Errorf("validate lyrics acquisition provider: %w", err)
	}
	if err := validateBoundedText(request.canonicalRequestIdentity, maxCanonicalRequestBytes, false); err != nil {
		return fmt.Errorf("validate canonical request identity: %w", err)
	}
	if err := validateBoundedText(string(request.kind), maxRequestKindBytes, false); err != nil {
		return fmt.Errorf("validate request kind: %w", err)
	}
	switch request.kind {
	case acquisitionRequestSearch:
		if request.revisionSelector != "" {
			return errors.New("search acquisition must not carry a revision selector")
		}
	case acquisitionRequestRevision, acquisitionRequestFixedIndex:
		if err := validateBoundedText(request.revisionSelector, maxRevisionSelectorBytes, false); err != nil {
			return fmt.Errorf("validate revision selector: %w", err)
		}
	default:
		return errors.New("lyrics acquisition request kind is invalid")
	}
	return nil
}

func validateBoundedText(value string, maximum int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return errors.New("text is empty, noncanonical, or outside its byte bound")
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f {
			return errors.New("text contains a control character")
		}
	}
	return nil
}

func validateCanonicalTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !strings.HasSuffix(value, "Z") || parsed.UTC().Format(time.RFC3339Nano) != value || parsed.UnixNano() <= 0 {
		return time.Time{}, errors.New("timestamp must be positive canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func validateObservedRevisions(revisions []observedRevision, fetchedAt time.Time) error {
	if revisions == nil {
		return errors.New("lyrics acquisition observed revisions must be an explicit array")
	}
	if len(revisions) > maxObservedRevisions {
		return errors.New("lyrics acquisition has too many observed revisions")
	}
	seen := make(map[string]struct{}, len(revisions))
	for _, revision := range revisions {
		if err := validateBoundedText(revision.selector, maxObservedRevisionSelector, false); err != nil {
			return fmt.Errorf("validate observed revision selector: %w", err)
		}
		if revision.revisionID <= 0 {
			return errors.New("observed revision ID must be positive")
		}
		if revision.timestamp != "" {
			parsed, err := validateCanonicalTimestamp(revision.timestamp)
			if err != nil || parsed.After(fetchedAt) {
				return errors.New("observed revision timestamp is invalid")
			}
		}
		if revision.sha1 != "" && !canonicalSHA1.MatchString(revision.sha1) {
			return errors.New("observed revision SHA-1 is invalid")
		}
		key := fmt.Sprintf("%s\x00%d\x00%s\x00%s", revision.selector, revision.revisionID, revision.timestamp, revision.sha1)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("lyrics acquisition contains a duplicate observed revision")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateEvidenceEnvelope(response validatedProviderResponse) error {
	if len(response.envelope.raw) == 0 || len(response.envelope.raw) > maxEvidenceEnvelopeBytes {
		return errors.New("canonical evidence envelope is empty or exceeds its v2 byte bound")
	}
	if !canonicalSHA256.MatchString(response.envelope.sha256) || sha256Hex(response.envelope.raw) != response.envelope.sha256 {
		return errors.New("canonical evidence envelope SHA-256 does not match its bytes")
	}
	var envelope lyricssource.IndexEvidence
	if err := decodeClosedCanonicalJSON(response.envelope.raw, &envelope); err != nil {
		return fmt.Errorf("decode canonical evidence envelope: %w", err)
	}
	if err := lyricssource.ValidateIndexEvidenceEnvelope(envelope); err != nil {
		return fmt.Errorf("validate canonical evidence envelope: %w", err)
	}
	if string(envelope.Provider) != response.request.provider || envelope.FetchedAt != response.fetchedAt ||
		envelope.EvidenceID != response.evidence.evidenceID || envelope.SHA256 != response.evidence.rawSHA256 ||
		envelope.RawSHA256 != response.evidence.rawSHA256 || !bytes.Equal(envelope.Raw, response.evidence.raw) {
		return errors.New("canonical evidence envelope does not preserve the exact provider, fetchedAt, projection, raw SHA, and evidence ID")
	}
	switch envelope.Kind {
	case lyricssource.IndexEvidenceKindMediaWikiSearchResponse:
		if response.request.kind != acquisitionRequestSearch || response.request.canonicalRequestIdentity != envelope.CanonicalRequestURL ||
			response.request.revisionSelector != "" {
			return errors.New("canonical search evidence envelope does not preserve the exact request")
		}
	case lyricssource.IndexEvidenceKindMediaWikiRevision:
		selector := "oldid:" + strconv.Itoa(envelope.RevisionID)
		requestIdentityValid := response.request.canonicalRequestIdentity == envelope.CanonicalURL
		if !requestIdentityValid {
			requestIdentityValid = lyricssource.ValidateRecoveryRevisionRequestIdentity(
				envelope.Provider, response.request.canonicalRequestIdentity, envelope,
			) == nil
		}
		if response.request.kind != acquisitionRequestRevision && response.request.kind != acquisitionRequestFixedIndex ||
			!requestIdentityValid || response.request.revisionSelector != selector {
			return errors.New("canonical revision evidence envelope does not preserve the exact request")
		}
	case lyricssource.IndexEvidenceKindExactPublicHTML:
		selector := "public-revision:" + strconv.Itoa(envelope.RevisionID)
		if envelope.Provider != lyricssource.ProviderMoegirlPublicExact ||
			response.request.kind != acquisitionRequestRevision ||
			response.request.canonicalRequestIdentity != envelope.CanonicalURL ||
			response.request.canonicalRequestIdentity != envelope.CanonicalRequestURL ||
			response.request.revisionSelector != selector {
			return errors.New("canonical exact public evidence envelope does not preserve the complete page request")
		}
	default:
		return errors.New("canonical evidence envelope kind is invalid")
	}
	return nil
}

func validateResponse(response validatedProviderResponse) error {
	if err := validateRequest(response.request); err != nil {
		return err
	}
	fetchedAt, err := validateCanonicalTimestamp(response.fetchedAt)
	if err != nil {
		return fmt.Errorf("validate fetchedAt: %w", err)
	}
	if len(response.rawResponse) == 0 || len(response.rawResponse) > maxRawResponseBytes {
		return errors.New("raw provider response is empty or exceeds the v2 byte bound")
	}
	if !canonicalSHA256.MatchString(response.rawResponseSHA256) || sha256Hex(response.rawResponse) != response.rawResponseSHA256 {
		return errors.New("raw provider response SHA-256 does not match its bytes")
	}
	if len(response.evidence.raw) == 0 || len(response.evidence.raw) > maxEvidenceProjectionBytes {
		return errors.New("evidence raw projection is empty or exceeds the v2 byte bound")
	}
	if !canonicalSHA256.MatchString(response.evidence.rawSHA256) || sha256Hex(response.evidence.raw) != response.evidence.rawSHA256 {
		return errors.New("evidence raw projection SHA-256 does not match its bytes")
	}
	if response.evidence.evidenceID == "" || !canonicalEvidenceID.MatchString(response.evidence.evidenceID) || len(response.evidence.evidenceID) > maxEvidenceIDBytes {
		return errors.New("evidence ID is invalid")
	}
	if err := validateEvidenceEnvelope(response); err != nil {
		return err
	}
	return validateObservedRevisions(response.observedRevisions, fetchedAt)
}

func buildManifest(response validatedProviderResponse) (acquisitionManifest, []byte, string, string, error) {
	if err := validateResponse(response); err != nil {
		return acquisitionManifest{}, nil, "", "", err
	}
	key, err := requestKey(response.request)
	if err != nil {
		return acquisitionManifest{}, nil, "", "", err
	}
	observed := make([]manifestObservedRevision, len(response.observedRevisions))
	for index, revision := range response.observedRevisions {
		observed[index] = manifestObservedRevision{
			Selector: revision.selector, RevisionID: revision.revisionID,
			Timestamp: revision.timestamp, SHA1: revision.sha1,
		}
	}
	manifest := acquisitionManifest{
		SchemaVersion: manifestSchemaVersion,
		Request: manifestRequest{
			Provider: response.request.provider, CanonicalRequestIdentity: response.request.canonicalRequestIdentity,
			Kind: string(response.request.kind), RevisionSelector: response.request.revisionSelector,
		},
		FetchedAt:   response.fetchedAt,
		RawResponse: manifestBlob{SHA256: response.rawResponseSHA256, ByteCount: len(response.rawResponse)},
		EvidenceProjection: manifestEvidence{
			EvidenceID: response.evidence.evidenceID, SHA256: response.evidence.rawSHA256,
			ByteCount: len(response.evidence.raw),
		},
		EvidenceEnvelope:  manifestBlob{SHA256: response.envelope.sha256, ByteCount: len(response.envelope.raw)},
		ObservedRevisions: observed,
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return acquisitionManifest{}, nil, "", "", fmt.Errorf("encode acquisition manifest: %w", err)
	}
	if len(body) > maxManifestBytes {
		return acquisitionManifest{}, nil, "", "", errors.New("acquisition manifest exceeds the v2 byte bound")
	}
	acquisitionID := sha256Hex(body)
	return manifest, body, acquisitionID, key, nil
}

func decodeManifest(body []byte) (acquisitionManifest, error) {
	var manifest acquisitionManifest
	if len(body) == 0 || len(body) > maxManifestBytes {
		return manifest, errors.New("acquisition manifest is empty or exceeds its v2 byte bound")
	}
	if err := decodeClosedCanonicalJSON(body, &manifest); err != nil {
		return manifest, fmt.Errorf("decode acquisition manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return manifest, fmt.Errorf("validate acquisition manifest: %w", err)
	}
	return manifest, nil
}

func validateManifest(manifest acquisitionManifest) error {
	if manifest.SchemaVersion != manifestSchemaVersion {
		return errors.New("acquisition manifest schema version is invalid")
	}
	request := manifest.response(nil, nil, nil).request
	if err := validateRequest(request); err != nil {
		return err
	}
	fetchedAt, err := validateCanonicalTimestamp(manifest.FetchedAt)
	if err != nil {
		return err
	}
	if !canonicalSHA256.MatchString(manifest.RawResponse.SHA256) || manifest.RawResponse.ByteCount <= 0 ||
		manifest.RawResponse.ByteCount > maxRawResponseBytes {
		return errors.New("acquisition manifest raw response binding is invalid")
	}
	if !canonicalSHA256.MatchString(manifest.EvidenceProjection.SHA256) || manifest.EvidenceProjection.ByteCount <= 0 ||
		manifest.EvidenceProjection.ByteCount > maxEvidenceProjectionBytes ||
		manifest.EvidenceProjection.SHA256 == manifest.RawResponse.SHA256 && manifest.EvidenceProjection.ByteCount != manifest.RawResponse.ByteCount {
		return errors.New("acquisition manifest evidence projection binding is invalid")
	}
	if manifest.EvidenceProjection.EvidenceID == "" || !canonicalEvidenceID.MatchString(manifest.EvidenceProjection.EvidenceID) ||
		len(manifest.EvidenceProjection.EvidenceID) > maxEvidenceIDBytes {
		return errors.New("acquisition manifest evidence ID is invalid")
	}
	if !canonicalSHA256.MatchString(manifest.EvidenceEnvelope.SHA256) || manifest.EvidenceEnvelope.ByteCount <= 0 ||
		manifest.EvidenceEnvelope.ByteCount > maxEvidenceEnvelopeBytes {
		return errors.New("acquisition manifest evidence envelope binding is invalid")
	}
	if manifest.ObservedRevisions == nil {
		return errors.New("acquisition manifest observed revisions must be an explicit array")
	}
	return validateObservedRevisions(manifest.response(nil, nil, nil).observedRevisions, fetchedAt)
}

func (manifest acquisitionManifest) response(raw, evidence, envelope []byte) validatedProviderResponse {
	observed := make([]observedRevision, len(manifest.ObservedRevisions))
	for index, revision := range manifest.ObservedRevisions {
		observed[index] = observedRevision{
			selector: revision.Selector, revisionID: revision.RevisionID,
			timestamp: revision.Timestamp, sha1: revision.SHA1,
		}
	}
	return validatedProviderResponse{
		request: acquisitionRequest{
			provider: manifest.Request.Provider, canonicalRequestIdentity: manifest.Request.CanonicalRequestIdentity,
			kind: acquisitionRequestKind(manifest.Request.Kind), revisionSelector: manifest.Request.RevisionSelector,
		},
		fetchedAt:   manifest.FetchedAt,
		rawResponse: raw, rawResponseSHA256: manifest.RawResponse.SHA256,
		evidence: evidenceProjection{
			evidenceID: manifest.EvidenceProjection.EvidenceID,
			raw:        evidence, rawSHA256: manifest.EvidenceProjection.SHA256,
		},
		envelope:          evidenceEnvelope{raw: envelope, sha256: manifest.EvidenceEnvelope.SHA256},
		observedRevisions: observed,
	}
}

func (manifest acquisitionManifest) acquired(acquisitionID string, raw, evidence, envelope []byte, replayOnly bool) acquiredProviderResponse {
	response := manifest.response(raw, evidence, envelope)
	return acquiredProviderResponse{
		acquisitionID:     acquisitionID,
		request:           response.request,
		fetchedAt:         response.fetchedAt,
		rawResponse:       append([]byte(nil), response.rawResponse...),
		rawResponseSHA256: response.rawResponseSHA256,
		evidence: evidenceProjection{
			evidenceID: response.evidence.evidenceID,
			raw:        append([]byte(nil), response.evidence.raw...),
			rawSHA256:  response.evidence.rawSHA256,
		},
		envelope: evidenceEnvelope{
			raw: append([]byte(nil), response.envelope.raw...), sha256: response.envelope.sha256,
		},
		observedRevisions: cloneObservedRevisions(response.observedRevisions),
		replayOnly:        replayOnly,
	}
}

func cloneObservedRevisions(input []observedRevision) []observedRevision {
	if input == nil {
		return []observedRevision{}
	}
	return append([]observedRevision(nil), input...)
}

func buildPendingMarker(manifest acquisitionManifest, acquisitionID, key string, manifestBytes int) (pendingMarker, []byte, error) {
	marker := pendingMarker{
		SchemaVersion:     pendingMarkerSchemaVersion,
		AcquisitionID:     acquisitionID,
		RequestKey:        key,
		RawSHA256:         manifest.RawResponse.SHA256,
		RawByteCount:      manifest.RawResponse.ByteCount,
		EvidenceSHA256:    manifest.EvidenceProjection.SHA256,
		EvidenceByteCount: manifest.EvidenceProjection.ByteCount,
		EnvelopeSHA256:    manifest.EvidenceEnvelope.SHA256,
		EnvelopeByteCount: manifest.EvidenceEnvelope.ByteCount,
		ManifestSHA256:    acquisitionID,
		ManifestByteCount: manifestBytes,
	}
	body, err := json.Marshal(marker)
	if err != nil {
		return pendingMarker{}, nil, fmt.Errorf("encode pending acquisition marker: %w", err)
	}
	if len(body) > maxPendingMarkerBytes {
		return pendingMarker{}, nil, errors.New("pending acquisition marker exceeds its v2 byte bound")
	}
	return marker, body, nil
}

func decodeLedgerMigrationManifest(body []byte) (ledgerMigrationManifest, error) {
	var manifest ledgerMigrationManifest
	if len(body) == 0 || len(body) > maxMigrationManifestSize {
		return manifest, errors.New("migration manifest is empty or exceeds its byte bound")
	}
	if err := decodeClosedCanonicalJSON(body, &manifest); err != nil {
		return manifest, fmt.Errorf("decode migration manifest: %w", err)
	}
	checkpoint := manifest.Checkpoint
	if manifest.SchemaVersion != 1 || !canonicalSHA256.MatchString(checkpoint.CheckpointSHA256) ||
		!canonicalSHA256.MatchString(checkpoint.EvidenceRowsSHA256) || !canonicalSHA256.MatchString(manifest.AcquisitionIDsSHA256) ||
		checkpoint.CheckpointBytes <= 0 || checkpoint.CatalogCount <= 0 || checkpoint.ResultCount < 0 ||
		checkpoint.ResultCount > checkpoint.CatalogCount || checkpoint.EvidenceCount <= 0 ||
		checkpoint.EvidenceRawBytes <= 0 || checkpoint.EvidenceJSONBytes <= 0 ||
		manifest.ImportedAcquisitionCount != checkpoint.EvidenceCount {
		return manifest, errors.New("migration manifest counts or digests are invalid")
	}
	return manifest, nil
}

func decodePendingMarker(body []byte) (pendingMarker, error) {
	var marker pendingMarker
	if len(body) == 0 || len(body) > maxPendingMarkerBytes {
		return marker, errors.New("pending acquisition marker is empty or exceeds its v2 byte bound")
	}
	if err := decodeClosedCanonicalJSON(body, &marker); err != nil {
		return marker, fmt.Errorf("decode pending acquisition marker: %w", err)
	}
	if marker.SchemaVersion != pendingMarkerSchemaVersion || !canonicalSHA256.MatchString(marker.AcquisitionID) ||
		!canonicalSHA256.MatchString(marker.RequestKey) || !canonicalSHA256.MatchString(marker.RawSHA256) ||
		!canonicalSHA256.MatchString(marker.EvidenceSHA256) || !canonicalSHA256.MatchString(marker.EnvelopeSHA256) ||
		!canonicalSHA256.MatchString(marker.ManifestSHA256) || marker.AcquisitionID != marker.ManifestSHA256 ||
		marker.RawByteCount <= 0 || marker.RawByteCount > maxRawResponseBytes ||
		marker.EvidenceByteCount <= 0 || marker.EvidenceByteCount > maxEvidenceProjectionBytes ||
		marker.EnvelopeByteCount <= 0 || marker.EnvelopeByteCount > maxEvidenceEnvelopeBytes ||
		marker.ManifestByteCount <= 0 || marker.ManifestByteCount > maxManifestBytes {
		return marker, errors.New("pending acquisition marker binding is invalid")
	}
	return marker, nil
}

func validateMarkerManifest(marker pendingMarker, manifest acquisitionManifest, body []byte) error {
	key, err := requestKey(manifest.response(nil, nil, nil).request)
	if err != nil {
		return err
	}
	if marker.AcquisitionID != sha256Hex(body) || marker.RequestKey != key || marker.RawSHA256 != manifest.RawResponse.SHA256 ||
		marker.RawByteCount != manifest.RawResponse.ByteCount || marker.EvidenceSHA256 != manifest.EvidenceProjection.SHA256 ||
		marker.EvidenceByteCount != manifest.EvidenceProjection.ByteCount || marker.EnvelopeSHA256 != manifest.EvidenceEnvelope.SHA256 ||
		marker.EnvelopeByteCount != manifest.EvidenceEnvelope.ByteCount || marker.ManifestSHA256 != sha256Hex(body) ||
		marker.ManifestByteCount != len(body) {
		return errors.New("pending acquisition marker does not bind the exact manifest")
	}
	return nil
}

func decodeClosedCanonicalJSON(body []byte, target any) error {
	if target == nil {
		return errors.New("JSON target is required")
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON value")
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, canonical) {
		return errors.New("JSON is not canonical")
	}
	return nil
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
