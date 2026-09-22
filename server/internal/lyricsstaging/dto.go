package lyricsstaging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

const (
	PreflightSchemaVersion = 1

	// CatalogSchemaVersion is the immutable catalog evidence contract encoded in
	// preflight reports and staging manifests. Runtime schemas through v24 are
	// compatible because later migrations do not change these columns, the
	// policy version, or the fingerprint calculation. Additive runtime storage
	// migrations through v27 preserve that catalog contract unchanged.
	CatalogSchemaVersion        = 18
	MaximumCatalogRuntimeSchema = 27
	ManifestSchemaVersion       = 3
	MaxManifestBytes            = 256 << 20

	// MaxPreflightReportEnvelopeBytes is the reviewed allowance for the closed
	// 704-item report metadata around a maximum-size private evidence receipt.
	// Keep the receipt's independent 32/64 MiB raw/encoded limits unchanged.
	MaxPreflightReportEnvelopeBytes = 32 << 20
	MaxPreflightReportBytes         = MaxPrivateEvidenceReceiptBytes + MaxPreflightReportEnvelopeBytes

	maxCatalogRecords                  = 100_000
	maxCandidateTitle                  = 2048
	maxCandidateURL                    = 4096
	maxCandidateCategory               = 1024
	maxReportCandidates                = 16
	maxAttempts                        = 5
	maxStagedPerformers                = 256
	maxStagedSegmentsPerLine           = 100
	maxStagedRubySpansPerSegment       = 8 << 10
	maxStagedPerformerIDBytes          = 128
	maxStagedPerformerNameBytes        = 2048
	maxStagedPerformerColorBytes       = 7
	maxStagedVersionLabelBytes         = 2048
	maxStagedRubyGeneratorVersionBytes = 64
	maxStagedRubyTextBytes             = 8 << 10
	maxStagedRubyReadingBytes          = 16 << 10
	maxStagedRubyReadingTotalBytes     = 1 << 20
)

var (
	ErrManifestRebuildRequired = errors.New("staging manifest requires provenance rebuild")

	canonicalSHA1            = regexp.MustCompile(`^[0-9a-f]{40}$`)
	canonicalSHA256          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalColor           = regexp.MustCompile(`^#[0-9A-F]{6}$`)
	canonicalPerformerID     = regexp.MustCompile(`^[\pL\pN_-]+$`)
	canonicalRenditionKey    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	canonicalIndexEvidenceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

// CandidateIdentity is the complete provider-aware fixed candidate copied from
// a complete lyrics-preflight report. It deliberately contains no source or
// lyrics text, but it retains every compact field required to make the later
// fetch claim provider-, rendition-, and evidence-specific.
type CandidateIdentity struct {
	Provider             model.LyricsSourceProvider           `json:"provider"`
	Origin               string                               `json:"origin"`
	PageID               int                                  `json:"pageId"`
	RevisionID           int                                  `json:"revisionId"`
	RevisionTimestamp    string                               `json:"revisionTimestamp,omitempty"`
	SHA1                 string                               `json:"sha1"`
	Title                string                               `json:"title"`
	CanonicalURL         string                               `json:"canonicalUrl"`
	Categories           []string                             `json:"categories"`
	Section              string                               `json:"section"`
	RenditionKey         string                               `json:"renditionKey"`
	ArtifactRenditionKey string                               `json:"artifactRenditionKey,omitempty"`
	VersionReason        model.LyricsSourceVersionReasonCode  `json:"versionReason"`
	IndexEvidenceRefs    []model.LyricsSourceIndexEvidenceRef `json:"indexEvidenceRefs"`
}

func (candidate CandidateIdentity) SourceCandidate() lyricssource.Candidate {
	return lyricssource.Candidate{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
		RevisionID: candidate.RevisionID, RevisionTimestamp: candidate.RevisionTimestamp,
		SHA1: candidate.SHA1, Title: candidate.Title, CanonicalURL: candidate.CanonicalURL,
		Categories: append([]string{}, candidate.Categories...),
		Section:    candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
	}
}

// ResolveArtifactRenditionKeys separates provider-independent composition keys
// from unique artifact/component keys. A single logical rendition retains its
// existing key; collisions default to provider-scoped keys unless the caller
// supplied an explicit ArtifactRenditionKey.
func ResolveArtifactRenditionKeys(candidates []CandidateIdentity) ([]string, error) {
	logicalCounts := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		logicalCounts[candidate.RenditionKey]++
	}
	keys := make([]string, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for index, candidate := range candidates {
		key := candidate.ArtifactRenditionKey
		if key == "" {
			key = candidate.RenditionKey
			if logicalCounts[candidate.RenditionKey] > 1 {
				key = string(candidate.Provider) + "." + candidate.RenditionKey
			}
		}
		if !canonicalRenditionKey.MatchString(key) {
			return nil, fmt.Errorf("%w: invalid artifact rendition key %q", ErrManifestRebuildRequired, key)
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("%w: repeated artifact rendition key %q", ErrManifestRebuildRequired, key)
		}
		seen[key] = struct{}{}
		keys[index] = key
	}
	return keys, nil
}

type CatalogReference struct {
	MusicID             int    `json:"musicId"`
	JapaneseTitle       string `json:"japaneseTitle"`
	CatalogFingerprint  string `json:"catalogFingerprint"`
	TargetMusicID       int    `json:"targetMusicId"`
	AssociationMusicIDs []int  `json:"associationMusicIds"`
	LineCount           int    `json:"lineCount"`
	PageID              int    `json:"pageId"`
	RevisionID          int    `json:"revisionId"`
	SHA1                string `json:"sha1"`
}

type SearchDiagnostics struct {
	SearchHits             int `json:"searchHits"`
	Restricted             int `json:"restricted"`
	RestrictedTitleMatch   int `json:"restrictedTitleMatch"`
	TitleMismatch          int `json:"titleMismatch"`
	CreditMismatch         int `json:"creditMismatch"`
	LyricistCreditMissing  int `json:"lyricistCreditMissing"`
	LyricistCreditMismatch int `json:"lyricistCreditMismatch"`
	ComposerCreditMissing  int `json:"composerCreditMissing"`
	ComposerCreditMismatch int `json:"composerCreditMismatch"`
	ArrangerCreditMissing  int `json:"arrangerCreditMissing"`
	ArrangerCreditMismatch int `json:"arrangerCreditMismatch"`
	SignalMismatch         int `json:"signalMismatch"`
	Verified               int `json:"verified"`
}

type PostFetchState string

const (
	PostFetchStateComplete        PostFetchState = "complete"
	PostFetchStateVersionConflict PostFetchState = "version_conflict"
)

// PreflightItem mirrors the closed, text-free report item DTO emitted by the
// lyrics-preflight command. VersionReason remains provider-candidate evidence;
// CompositionReason is the final cross-provider composition classification.
// A plural post-fetch conflict is retained in incomplete with PostFetchState
// version_conflict and is never admitted to a staging manifest.
type PreflightItem struct {
	MusicID                 int                                 `json:"musicId"`
	JapaneseTitle           string                              `json:"japaneseTitle"`
	CatalogFingerprint      string                              `json:"catalogFingerprint"`
	TargetMusicID           int                                 `json:"targetMusicId,omitempty"`
	AssociationMusicIDs     []int                               `json:"associationMusicIds,omitempty"`
	ReasonCode              string                              `json:"reasonCode,omitempty"`
	PostFetchState          PostFetchState                      `json:"postFetchState,omitempty"`
	CompositionReason       model.LyricsSourceVersionReasonCode `json:"compositionReason,omitempty"`
	Candidate               *CandidateIdentity                  `json:"candidate,omitempty"`
	Candidates              []CandidateIdentity                 `json:"candidates,omitempty"`
	FixedArtifactCandidates []CandidateIdentity                 `json:"fixedArtifactCandidates,omitempty"`
	LineCount               int                                 `json:"lineCount,omitempty"`
	SearchAttempts          int                                 `json:"searchAttempts,omitempty"`
	FetchAttempts           int                                 `json:"fetchAttempts,omitempty"`
	ErrorCode               string                              `json:"errorCode,omitempty"`
	SearchDiagnostics       *SearchDiagnostics                  `json:"searchDiagnostics,omitempty"`
}

type PreflightSummary struct {
	CatalogReview    int `json:"catalog_review"`
	GameSizeEvidence int `json:"game_size_evidence"`
	UniqueComplete   int `json:"unique_complete"`
	Ambiguous        int `json:"ambiguous"`
	Missing          int `json:"missing"`
	Incomplete       int `json:"incomplete"`
	Error            int `json:"error"`
}

type PreflightReport struct {
	SchemaVersion        int                     `json:"schemaVersion"`
	GeneratedAt          string                  `json:"generatedAt"`
	CatalogSchemaVersion int                     `json:"catalogSchemaVersion"`
	CatalogCount         int                     `json:"catalogCount"`
	Summary              PreflightSummary        `json:"summary"`
	EvidenceReceipt      *PrivateEvidenceReceipt `json:"evidenceReceipt,omitempty"`
	CatalogReview        []PreflightItem         `json:"catalog_review"`
	GameSizeEvidence     []PreflightItem         `json:"game_size_evidence"`
	UniqueComplete       []PreflightItem         `json:"unique_complete"`
	Ambiguous            []PreflightItem         `json:"ambiguous"`
	Missing              []PreflightItem         `json:"missing"`
	Incomplete           []PreflightItem         `json:"incomplete"`
	Error                []PreflightItem         `json:"error"`
}

type PreflightReference struct {
	SchemaVersion        int    `json:"schemaVersion"`
	GeneratedAt          string `json:"generatedAt"`
	CatalogSchemaVersion int    `json:"catalogSchemaVersion"`
	CatalogCount         int    `json:"catalogCount"`
	UniqueCompleteCount  int    `json:"uniqueCompleteCount"`
	ReportSHA256         string `json:"reportSha256"`
}

type FixedSource struct {
	PageID               int      `json:"pageId"`
	RevisionID           int      `json:"revisionId"`
	SHA1                 string   `json:"sha1"`
	PageTitle            string   `json:"pageTitle"`
	CanonicalURL         string   `json:"canonicalUrl"`
	Categories           []string `json:"categories"`
	FetchedAt            string   `json:"fetchedAt"`
	RawWikitextByteCount int      `json:"rawWikitextByteCount"`
	RawWikitextSHA256    string   `json:"rawWikitextSha256"`
}

// Artifact fixes one set of private raw source bytes without copying those
// bytes into the manifest. Identity contains provider, section, rendition, and
// index-evidence provenance; ArtifactSHA256 binds that identity to the exact
// raw byte count and SHA-256.
type Artifact struct {
	Identity             model.LyricsSourceFixedIdentity `json:"identity"`
	RawWikitextByteCount int                             `json:"rawWikitextByteCount"`
	RawWikitextSHA256    string                          `json:"rawWikitextSha256"`
	ArtifactSHA256       string                          `json:"artifactSha256"`
}

// Draft contains only fetched source evidence. The legacy projection fields
// remain closed and checksum-bound for the editable import bridge, while
// Document is the authoritative provider/component contract and Artifacts may
// contain more than one immutable rendition.
type Draft struct {
	MusicID               int                                   `json:"musicId"`
	JapaneseTitle         string                                `json:"japaneseTitle"`
	CatalogFingerprint    string                                `json:"catalogFingerprint"`
	TargetMusicID         int                                   `json:"targetMusicId"`
	AssociationMusicIDs   []int                                 `json:"associationMusicIds"`
	Source                FixedSource                           `json:"source"`
	SelectedVersion       model.LyricsSourceVersion             `json:"selectedVersion"`
	Performers            []model.LyricsSourcePerformer         `json:"performers"`
	RubyGeneratorVersion  string                                `json:"rubyGeneratorVersion"`
	Lines                 []model.LyricsSourceExtractedLine     `json:"lines"`
	Translations          []string                              `json:"translations,omitempty"`
	RenditionTranslations []lyricscontract.RenditionTranslation `json:"renditionTranslations,omitempty"`
	ExtractedLinesSHA256  string                                `json:"extractedLinesSha256"`
	Artifacts             []Artifact                            `json:"artifacts"`
	Document              model.LyricsSourceDocument            `json:"document"`
	DocumentSHA256        string                                `json:"documentSha256"`
	DraftSHA256           string                                `json:"draftSha256"`
}

type Manifest struct {
	SchemaVersion    int                `json:"schemaVersion"`
	Preflight        PreflightReference `json:"preflight"`
	CatalogReference []CatalogReference `json:"catalogReference"`
	Items            []Draft            `json:"items"`
	BatchSHA256      string             `json:"batchSha256"`
}

func DecodePreflight(body []byte) (PreflightReport, error) {
	report, err := decodePreflightBody(body)
	if err != nil {
		return PreflightReport{}, err
	}
	if err := ValidatePreflight(report); err != nil {
		return PreflightReport{}, err
	}
	return report, nil
}

// DecodePreflightWithEvidenceResolver validates a complete report and returns
// the sole immutable receipt resolver constructed during that validation. Stage
// callers reuse it instead of repeating receipt-wide validation and raw clones.
func DecodePreflightWithEvidenceResolver(body []byte) (PreflightReport, *PrivateEvidenceResolver, error) {
	report, err := decodePreflightBody(body)
	if err != nil {
		return PreflightReport{}, nil, err
	}
	resolver, err := validatePreflightWithEvidenceResolver(report)
	if err != nil {
		return PreflightReport{}, nil, err
	}
	return report, resolver, nil
}

func decodePreflightBody(body []byte) (PreflightReport, error) {
	if err := validatePreflightReportSize(len(body)); err != nil {
		return PreflightReport{}, err
	}
	var report PreflightReport
	if err := decodeClosedUniqueJSON(body, &report); err != nil {
		return PreflightReport{}, fmt.Errorf("decode complete preflight report: %w", err)
	}
	return report, nil
}

func validatePreflightReportSize(size int) error {
	if size > MaxPreflightReportBytes {
		return fmt.Errorf("complete preflight report exceeds %d bytes", MaxPreflightReportBytes)
	}
	return nil
}

// RequireCanonicalPreflightBytes verifies the exact lyrics-preflight wire
// serialization without materializing a second report-sized canonical buffer.
// DecodePreflight remains responsible for closed-field, unique-key, and
// semantic validation before callers use this canonical byte check.
func RequireCanonicalPreflightBytes(body []byte, report PreflightReport) error {
	if len(body) == 0 {
		return errors.New("complete preflight report is empty")
	}
	if err := validatePreflightReportSize(len(body)); err != nil {
		return err
	}
	comparator := &exactPreflightBytesWriter{expected: body}
	if err := writeCanonicalPreflightJSON(comparator, report); err != nil {
		if errors.Is(err, errNoncanonicalPreflightBytes) {
			return errors.New("complete preflight report must use the canonical lyrics-preflight JSON serialization")
		}
		return fmt.Errorf("encode canonical complete preflight report: %w", err)
	}
	if comparator.offset != len(body) {
		return errors.New("complete preflight report must use the canonical lyrics-preflight JSON serialization")
	}
	return nil
}

var errNoncanonicalPreflightBytes = errors.New("noncanonical complete preflight bytes")

type exactPreflightBytesWriter struct {
	expected []byte
	offset   int
}

func (writer *exactPreflightBytesWriter) Write(data []byte) (int, error) {
	if len(data) > len(writer.expected)-writer.offset ||
		!bytes.Equal(data, writer.expected[writer.offset:writer.offset+len(data)]) {
		return 0, errNoncanonicalPreflightBytes
	}
	writer.offset += len(data)
	return len(data), nil
}

type canonicalPreflightJSONWriter struct {
	writer     io.Writer
	fieldCount int
}

func writeCanonicalPreflightJSON(output io.Writer, report PreflightReport) error {
	if output == nil {
		return errors.New("canonical preflight writer is required")
	}
	writer := canonicalPreflightJSONWriter{writer: output}
	if err := writeCanonicalJSONBytes(output, []byte("{")); err != nil {
		return err
	}
	fields := []struct {
		name  string
		value any
	}{
		{name: "schemaVersion", value: report.SchemaVersion},
		{name: "generatedAt", value: report.GeneratedAt},
		{name: "catalogSchemaVersion", value: report.CatalogSchemaVersion},
		{name: "catalogCount", value: report.CatalogCount},
		{name: "summary", value: report.Summary},
	}
	for _, field := range fields {
		value := field.value
		if err := writer.field(field.name, func(output io.Writer) error {
			return writeCanonicalEmbeddedJSON(output, value, "  ")
		}); err != nil {
			return err
		}
	}
	if report.EvidenceReceipt != nil {
		if err := writer.field("evidenceReceipt", func(output io.Writer) error {
			prefixed := &continuationPrefixWriter{writer: output, prefix: []byte("  ")}
			return writePrivateEvidenceReceiptJSON(prefixed, *report.EvidenceReceipt, true, false)
		}); err != nil {
			return err
		}
	}
	arrays := []struct {
		name  string
		items []PreflightItem
	}{
		{name: "catalog_review", items: report.CatalogReview},
		{name: "game_size_evidence", items: report.GameSizeEvidence},
		{name: "unique_complete", items: report.UniqueComplete},
		{name: "ambiguous", items: report.Ambiguous},
		{name: "missing", items: report.Missing},
		{name: "incomplete", items: report.Incomplete},
		{name: "error", items: report.Error},
	}
	for _, array := range arrays {
		items := array.items
		if err := writer.field(array.name, func(output io.Writer) error {
			return writeCanonicalPreflightItems(output, items)
		}); err != nil {
			return err
		}
	}
	return writeCanonicalJSONBytes(output, []byte("\n}\n"))
}

func (writer *canonicalPreflightJSONWriter) field(name string, writeValue func(io.Writer) error) error {
	separator := "\n  "
	if writer.fieldCount > 0 {
		separator = ",\n  "
	}
	if err := writeCanonicalJSONBytes(writer.writer, []byte(separator)); err != nil {
		return err
	}
	encodedName, err := json.Marshal(name)
	if err != nil {
		return err
	}
	if err := writeCanonicalJSONBytes(writer.writer, encodedName); err != nil {
		return err
	}
	if err := writeCanonicalJSONBytes(writer.writer, []byte(": ")); err != nil {
		return err
	}
	if err := writeValue(writer.writer); err != nil {
		return err
	}
	writer.fieldCount++
	return nil
}

func writeCanonicalPreflightItems(output io.Writer, items []PreflightItem) error {
	if items == nil {
		return writeCanonicalJSONBytes(output, []byte("null"))
	}
	if err := writeCanonicalJSONBytes(output, []byte("[")); err != nil {
		return err
	}
	for index, item := range items {
		separator := "\n    "
		if index > 0 {
			separator = ",\n    "
		}
		if err := writeCanonicalJSONBytes(output, []byte(separator)); err != nil {
			return err
		}
		if err := writeCanonicalEmbeddedJSON(output, item, "    "); err != nil {
			return err
		}
	}
	if len(items) > 0 {
		if err := writeCanonicalJSONBytes(output, []byte("\n  ")); err != nil {
			return err
		}
	}
	return writeCanonicalJSONBytes(output, []byte("]"))
}

func writeCanonicalEmbeddedJSON(output io.Writer, value any, prefix string) error {
	body, err := json.MarshalIndent(value, prefix, "  ")
	if err != nil {
		return err
	}
	return writeCanonicalJSONBytes(output, body)
}

func writeCanonicalJSONBytes(output io.Writer, data []byte) error {
	written, err := output.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

type continuationPrefixWriter struct {
	writer      io.Writer
	prefix      []byte
	atLineStart bool
}

func (writer *continuationPrefixWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	consumed := 0
	for len(data) > 0 {
		if writer.atLineStart {
			if err := writeCanonicalJSONBytes(writer.writer, writer.prefix); err != nil {
				return consumed, err
			}
			writer.atLineStart = false
		}
		lineLength := len(data)
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			lineLength = newline + 1
		}
		written, err := writer.writer.Write(data[:lineLength])
		consumed += written
		if written > 0 && data[written-1] == '\n' {
			writer.atLineStart = true
		}
		if err != nil {
			return consumed, err
		}
		if written != lineLength {
			return consumed, io.ErrShortWrite
		}
		data = data[lineLength:]
	}
	return originalLength, nil
}

func DecodeManifest(body []byte) (Manifest, error) {
	if len(body) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("staging manifest exceeds %d bytes", MaxManifestBytes)
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return Manifest{}, fmt.Errorf("decode staging manifest: %w", err)
	}
	var envelope struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Manifest{}, fmt.Errorf("decode staging manifest: %w", err)
	}
	if envelope.SchemaVersion != ManifestSchemaVersion {
		return Manifest{}, fmt.Errorf("%w: schema v%d lacks required provider/component provenance", ErrManifestRebuildRequired, envelope.SchemaVersion)
	}
	var manifest Manifest
	if err := decodeClosedUniqueJSON(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode staging manifest: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func decodeClosedUniqueJSON(body []byte, target any) error {
	if len(body) == 0 || target == nil {
		return errors.New("JSON body and target are required")
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
	return nil
}

func ValidatePreflight(report PreflightReport) error {
	receiptCandidates, err := validatePreflightEnvelope(report)
	if err != nil {
		return err
	}
	if report.EvidenceReceipt == nil {
		return nil
	}
	if len(receiptCandidates) == 0 {
		return errors.New("complete preflight report has private evidence without candidates")
	}
	if err := ValidatePrivateEvidenceReceiptForCandidates(*report.EvidenceReceipt, receiptCandidates); err != nil {
		return fmt.Errorf("complete preflight evidence receipt: %w", err)
	}
	return nil
}

func validatePreflightWithEvidenceResolver(report PreflightReport) (*PrivateEvidenceResolver, error) {
	receiptCandidates, err := validatePreflightEnvelope(report)
	if err != nil {
		return nil, err
	}
	if report.EvidenceReceipt == nil {
		return nil, nil
	}
	if len(receiptCandidates) == 0 {
		return nil, errors.New("complete preflight report has private evidence without candidates")
	}
	resolver, err := NewPrivateEvidenceResolver(*report.EvidenceReceipt)
	if err != nil {
		return nil, fmt.Errorf("complete preflight evidence receipt: %w", err)
	}
	if err := resolver.ValidateCandidates(receiptCandidates); err != nil {
		return nil, fmt.Errorf("complete preflight evidence receipt: %w", err)
	}
	return resolver, nil
}

func validatePreflightEnvelope(report PreflightReport) ([]CandidateIdentity, error) {
	parsedGeneratedAt, err := time.Parse(time.RFC3339Nano, report.GeneratedAt)
	if err != nil || parsedGeneratedAt.Format(time.RFC3339Nano) != report.GeneratedAt || !strings.HasSuffix(report.GeneratedAt, "Z") {
		return nil, errors.New("complete preflight report has an invalid timestamp")
	}
	if report.CatalogReview == nil || report.GameSizeEvidence == nil || report.UniqueComplete == nil ||
		report.Ambiguous == nil || report.Missing == nil || report.Incomplete == nil || report.Error == nil {
		return nil, errors.New("complete preflight report must contain every classification array")
	}
	classifiedCount := len(report.CatalogReview) + len(report.GameSizeEvidence) + len(report.UniqueComplete) +
		len(report.Ambiguous) + len(report.Missing) + len(report.Incomplete) + len(report.Error)
	if report.SchemaVersion != PreflightSchemaVersion || report.CatalogSchemaVersion != CatalogSchemaVersion ||
		report.CatalogCount < 0 || report.CatalogCount > maxCatalogRecords || classifiedCount != report.CatalogCount ||
		report.Summary.CatalogReview != len(report.CatalogReview) ||
		report.Summary.GameSizeEvidence != len(report.GameSizeEvidence) ||
		report.Summary.UniqueComplete != len(report.UniqueComplete) || report.Summary.Ambiguous != len(report.Ambiguous) ||
		report.Summary.Missing != len(report.Missing) || report.Summary.Incomplete != len(report.Incomplete) ||
		report.Summary.Error != len(report.Error) {
		return nil, errors.New("complete preflight report envelope is inconsistent")
	}
	seen := make(map[int]string, report.CatalogCount)
	receiptCandidates := []CandidateIdentity{}
	classes := []struct {
		name  string
		items []PreflightItem
	}{
		{name: "catalog_review", items: report.CatalogReview},
		{name: "game_size_evidence", items: report.GameSizeEvidence},
		{name: "unique_complete", items: report.UniqueComplete},
		{name: "ambiguous", items: report.Ambiguous},
		{name: "missing", items: report.Missing},
		{name: "incomplete", items: report.Incomplete},
		{name: "error", items: report.Error},
	}
	for _, class := range classes {
		lastMusicID := 0
		for _, item := range class.items {
			if item.MusicID <= lastMusicID {
				return nil, fmt.Errorf("complete preflight %s items are not strictly ordered", class.name)
			}
			lastMusicID = item.MusicID
			if prior, exists := seen[item.MusicID]; exists {
				return nil, fmt.Errorf("complete preflight music %d appears in both %s and %s", item.MusicID, prior, class.name)
			}
			seen[item.MusicID] = class.name
			if err := validatePreflightItem(class.name, item); err != nil {
				return nil, err
			}
			if len(item.FixedArtifactCandidates) > 0 {
				receiptCandidates = append(receiptCandidates, item.FixedArtifactCandidates...)
			} else {
				if item.Candidate != nil {
					receiptCandidates = append(receiptCandidates, *item.Candidate)
				}
				receiptCandidates = append(receiptCandidates, item.Candidates...)
			}
		}
	}
	if len(seen) != report.CatalogCount {
		return nil, errors.New("complete preflight report does not classify every catalog item exactly once")
	}
	return receiptCandidates, nil
}

func validatePreflightItem(class string, item PreflightItem) error {
	if item.MusicID <= 0 || item.JapaneseTitle == "" || strings.TrimSpace(item.JapaneseTitle) != item.JapaneseTitle ||
		len(item.JapaneseTitle) > maxCandidateTitle || strings.ContainsAny(item.JapaneseTitle, "\r\n") ||
		!canonicalSHA256.MatchString(item.CatalogFingerprint) || item.SearchAttempts < 0 || item.SearchAttempts > maxAttempts ||
		item.FetchAttempts < 0 || item.FetchAttempts > maxAttempts || item.LineCount < 0 || item.LineCount > 1000 ||
		len(item.AssociationMusicIDs) > maxCatalogRecords || len(item.Candidates) > maxReportCandidates ||
		len(item.FixedArtifactCandidates) > MaxFixedArtifactBundleArtifacts ||
		(item.PostFetchState != "" && item.PostFetchState != PostFetchStateComplete && item.PostFetchState != PostFetchStateVersionConflict) ||
		(item.CompositionReason != "" && !model.IsValidLyricsSourceVersionReasonCode(item.CompositionReason)) {
		return fmt.Errorf("complete preflight %s music %d has an invalid public surface", class, item.MusicID)
	}
	if item.Candidate != nil {
		if err := validateCandidate(*item.Candidate); err != nil {
			return fmt.Errorf("complete preflight music %d candidate: %w", item.MusicID, err)
		}
	}
	seenCandidates := make(map[string]struct{}, len(item.Candidates))
	for _, candidate := range item.Candidates {
		if err := validateCandidate(candidate); err != nil {
			return fmt.Errorf("complete preflight music %d candidate: %w", item.MusicID, err)
		}
		key := fmt.Sprintf("%s/%d/%d/%s", candidate.Provider, candidate.PageID, candidate.RevisionID, candidate.RenditionKey)
		if _, exists := seenCandidates[key]; exists {
			return fmt.Errorf("complete preflight music %d has duplicate provider candidate identities", item.MusicID)
		}
		seenCandidates[key] = struct{}{}
	}
	containsSelectedCandidate := false
	for _, candidate := range item.FixedArtifactCandidates {
		if err := validateCandidate(candidate); err != nil {
			return fmt.Errorf("complete preflight music %d fixed artifact candidate: %w", item.MusicID, err)
		}
		containsSelectedCandidate = containsSelectedCandidate || item.Candidate != nil && reflect.DeepEqual(candidate, *item.Candidate)
	}
	if _, err := ResolveArtifactRenditionKeys(item.FixedArtifactCandidates); err != nil {
		return fmt.Errorf("complete preflight music %d fixed artifacts: %w", item.MusicID, err)
	}
	if len(item.FixedArtifactCandidates) > 0 && item.PostFetchState != PostFetchStateVersionConflict && !containsSelectedCandidate {
		return fmt.Errorf("complete preflight music %d fixed artifacts omit the selected candidate", item.MusicID)
	}
	if !strictlyIncreasingPositiveInts(item.AssociationMusicIDs) {
		return fmt.Errorf("complete preflight music %d has invalid association ordering", item.MusicID)
	}
	for _, association := range item.AssociationMusicIDs {
		if association == item.TargetMusicID || association == item.MusicID && item.TargetMusicID == item.MusicID {
			return fmt.Errorf("complete preflight music %d has an invalid association", item.MusicID)
		}
	}
	if err := validateSearchDiagnostics(item.MusicID, item.SearchDiagnostics); err != nil {
		return err
	}
	switch class {
	case "catalog_review", "game_size_evidence":
		if item.Candidate != nil || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 || item.SearchAttempts != 0 ||
			item.FetchAttempts != 0 || item.ErrorCode != "" || item.SearchDiagnostics != nil || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("complete preflight music %d has an invalid catalog-only classification", item.MusicID)
		}
		if class == "game_size_evidence" && (item.TargetMusicID <= 0 || item.TargetMusicID == item.MusicID) {
			return fmt.Errorf("complete preflight music %d has an invalid game-size target", item.MusicID)
		}
	case "unique_complete":
		if item.TargetMusicID != item.MusicID || item.ReasonCode != "" || item.Candidate == nil || len(item.Candidates) != 0 ||
			item.LineCount <= 0 || item.SearchAttempts <= 0 || item.FetchAttempts <= 0 || item.ErrorCode != "" ||
			item.PostFetchState == PostFetchStateVersionConflict || item.CompositionReason == model.LyricsSourceVersionReasonVersionConflict {
			return fmt.Errorf("complete preflight music %d has an invalid unique_complete classification", item.MusicID)
		}
		if len(item.FixedArtifactCandidates) > 1 && item.CompositionReason == "" {
			return fmt.Errorf("complete preflight music %d plural artifacts require an explicit composition reason", item.MusicID)
		}
	case "ambiguous":
		if item.TargetMusicID != item.MusicID || item.ReasonCode != "" || item.Candidate != nil || len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 ||
			item.SearchAttempts <= 0 || item.FetchAttempts != 0 || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("complete preflight music %d has an invalid ambiguous classification", item.MusicID)
		}
		if item.ErrorCode == "candidate_limit_exceeded" {
			if len(item.Candidates) != 0 {
				return fmt.Errorf("complete preflight music %d has invalid candidate-limit evidence", item.MusicID)
			}
		} else if item.ErrorCode != "" || len(item.Candidates) < 2 {
			return fmt.Errorf("complete preflight music %d has invalid ambiguous evidence", item.MusicID)
		}
	case "missing":
		if item.TargetMusicID != item.MusicID || item.Candidate != nil || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 ||
			item.SearchAttempts <= 0 || item.FetchAttempts != 0 || item.ErrorCode != "" || item.SearchDiagnostics == nil ||
			item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("complete preflight music %d has an invalid missing classification", item.MusicID)
		}
		reasonCode, ok := missingReasonCode(item.SearchDiagnostics)
		if !ok || item.ReasonCode != reasonCode {
			return fmt.Errorf("complete preflight music %d has inconsistent missing diagnostics", item.MusicID)
		}
	case "incomplete":
		if item.PostFetchState == PostFetchStateVersionConflict {
			if item.TargetMusicID != item.MusicID || item.ReasonCode != "" || item.Candidate != nil || len(item.Candidates) != 0 ||
				len(item.FixedArtifactCandidates) < 2 || item.LineCount != 0 || item.SearchAttempts <= 0 || item.FetchAttempts <= 0 ||
				item.ErrorCode != string(model.LyricsSourceVersionReasonVersionConflict) ||
				item.CompositionReason != model.LyricsSourceVersionReasonVersionConflict {
				return fmt.Errorf("complete preflight music %d has an invalid post-fetch version conflict", item.MusicID)
			}
			break
		}
		if item.TargetMusicID != item.MusicID || item.ReasonCode != "" || item.Candidate == nil || len(item.Candidates) != 0 ||
			len(item.FixedArtifactCandidates) != 0 || item.LineCount != 0 || item.SearchAttempts <= 0 || item.FetchAttempts <= 0 ||
			item.ErrorCode == "" || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("complete preflight music %d has an invalid incomplete classification", item.MusicID)
		}
	case "error":
		if item.TargetMusicID != item.MusicID || item.ErrorCode == "" || item.ReasonCode != "" || item.LineCount != 0 ||
			item.SearchAttempts <= 0 || len(item.Candidates) != 0 || len(item.FixedArtifactCandidates) != 0 || item.Candidate == nil && item.FetchAttempts != 0 ||
			item.Candidate != nil && item.FetchAttempts <= 0 || item.PostFetchState != "" || item.CompositionReason != "" {
			return fmt.Errorf("complete preflight music %d has an invalid error classification", item.MusicID)
		}
	default:
		return fmt.Errorf("unsupported preflight classification %q", class)
	}
	return nil
}

func validateSearchDiagnostics(musicID int, diagnostics *SearchDiagnostics) error {
	if diagnostics == nil {
		return nil
	}
	counts := []int{
		diagnostics.SearchHits, diagnostics.Restricted, diagnostics.RestrictedTitleMatch,
		diagnostics.TitleMismatch, diagnostics.CreditMismatch,
		diagnostics.LyricistCreditMissing, diagnostics.LyricistCreditMismatch,
		diagnostics.ComposerCreditMissing, diagnostics.ComposerCreditMismatch,
		diagnostics.ArrangerCreditMissing, diagnostics.ArrangerCreditMismatch,
		diagnostics.SignalMismatch, diagnostics.Verified,
	}
	for _, count := range counts {
		if count < 0 || count > maxCatalogRecords {
			return fmt.Errorf("complete preflight music %d has invalid diagnostics", musicID)
		}
	}
	if diagnostics.Restricted > diagnostics.SearchHits || diagnostics.RestrictedTitleMatch > diagnostics.Restricted ||
		diagnostics.TitleMismatch > diagnostics.SearchHits || diagnostics.CreditMismatch > diagnostics.SearchHits ||
		diagnostics.SignalMismatch > diagnostics.SearchHits || diagnostics.Verified > diagnostics.SearchHits ||
		diagnostics.LyricistCreditMissing+diagnostics.LyricistCreditMismatch > diagnostics.CreditMismatch ||
		diagnostics.ComposerCreditMissing+diagnostics.ComposerCreditMismatch > diagnostics.CreditMismatch ||
		diagnostics.ArrangerCreditMissing+diagnostics.ArrangerCreditMismatch > diagnostics.CreditMismatch {
		return fmt.Errorf("complete preflight music %d has inconsistent diagnostics", musicID)
	}
	return nil
}

func missingReasonCode(diagnostics *SearchDiagnostics) (string, bool) {
	if diagnostics == nil {
		return "", false
	}
	reason, ok := (lyricssource.SearchDiagnostics{
		SearchHits: diagnostics.SearchHits, Restricted: diagnostics.Restricted,
		RestrictedTitleMatch: diagnostics.RestrictedTitleMatch, TitleMismatch: diagnostics.TitleMismatch,
		CreditMismatch: diagnostics.CreditMismatch, LyricistCreditMissing: diagnostics.LyricistCreditMissing,
		LyricistCreditMismatch: diagnostics.LyricistCreditMismatch,
		ComposerCreditMissing:  diagnostics.ComposerCreditMissing,
		ComposerCreditMismatch: diagnostics.ComposerCreditMismatch,
		ArrangerCreditMissing:  diagnostics.ArrangerCreditMissing,
		ArrangerCreditMismatch: diagnostics.ArrangerCreditMismatch,
		SignalMismatch:         diagnostics.SignalMismatch, Verified: diagnostics.Verified,
	}).ZeroCandidateReason()
	return string(reason), ok
}

func validateCandidate(candidate CandidateIdentity) error {
	if !model.IsValidLyricsSourceProvider(candidate.Provider) || candidate.PageID <= 0 || candidate.RevisionID <= 0 ||
		!canonicalSHA1.MatchString(candidate.SHA1) || candidate.Title == "" || strings.TrimSpace(candidate.Title) != candidate.Title ||
		len(candidate.Title) > maxCandidateTitle || strings.ContainsAny(candidate.Title, "\r\n") || candidate.CanonicalURL == "" ||
		strings.TrimSpace(candidate.CanonicalURL) != candidate.CanonicalURL || len(candidate.CanonicalURL) > maxCandidateURL ||
		candidate.Categories == nil || len(candidate.Categories) > 256 || candidate.Section == "" ||
		candidate.Section != strings.TrimSpace(candidate.Section) || len(candidate.Section) > 512 ||
		!canonicalRenditionKey.MatchString(candidate.RenditionKey) ||
		(candidate.ArtifactRenditionKey != "" && !canonicalRenditionKey.MatchString(candidate.ArtifactRenditionKey)) ||
		!model.IsValidLyricsSourceCandidateVersionReasonCode(candidate.VersionReason) || len(candidate.IndexEvidenceRefs) == 0 ||
		len(candidate.IndexEvidenceRefs) > 64 {
		return fmt.Errorf("%w: invalid provider-aware fixed revision identity", ErrManifestRebuildRequired)
	}
	wantOrigin := model.LyricsSourceOriginVocaloidFandom
	switch candidate.Provider {
	case model.LyricsSourceProviderMoegirl:
		wantOrigin = model.LyricsSourceOriginMoegirl
	case model.LyricsSourceProviderMoegirlPublicExact:
		wantOrigin = model.LyricsSourceOriginMoegirlPublicExact
	case model.LyricsSourceProviderSekaipedia:
		wantOrigin = model.LyricsSourceOriginSekaipedia
	}
	if candidate.Origin != wantOrigin || !canonicalProviderRevisionURL(candidate) {
		return errors.New("invalid provider-aware fixed revision URL")
	}
	if candidate.Provider == model.LyricsSourceProviderSekaipedia && candidate.RevisionTimestamp == "" {
		return errors.New("sekaipedia fixed revision requires a revisionTimestamp")
	}
	if candidate.RevisionTimestamp != "" && !canonicalCandidateRevisionTimestamp(candidate.RevisionTimestamp) {
		return errors.New("invalid provider-aware fixed revision timestamp")
	}
	seenCategories := make(map[string]struct{}, len(candidate.Categories))
	for index, category := range candidate.Categories {
		if category == "" || strings.TrimSpace(category) != category || len(category) > maxCandidateCategory ||
			strings.ContainsAny(category, "\r\n") || index > 0 && candidate.Categories[index-1] >= category {
			return errors.New("invalid candidate categories")
		}
		if _, exists := seenCategories[category]; exists {
			return errors.New("invalid candidate categories")
		}
		seenCategories[category] = struct{}{}
	}
	seenEvidence := make(map[string]struct{}, len(candidate.IndexEvidenceRefs))
	for _, reference := range candidate.IndexEvidenceRefs {
		if !canonicalIndexEvidenceID.MatchString(reference.EvidenceID) || !canonicalSHA256.MatchString(reference.SHA256) {
			return errors.New("invalid candidate index evidence")
		}
		if _, exists := seenEvidence[reference.EvidenceID]; exists {
			return errors.New("duplicate candidate index evidence")
		}
		seenEvidence[reference.EvidenceID] = struct{}{}
	}
	return nil
}

func canonicalProviderRevisionURL(candidate CandidateIdentity) bool {
	parsed, err := url.Parse(candidate.CanonicalURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" ||
		parsed.Host == "" || parsed.Path == "" || parsed.ForceQuery || parsed.Scheme+"://"+parsed.Host != candidate.Origin {
		return false
	}
	switch candidate.Provider {
	case model.LyricsSourceProviderVocaloidFandom:
		if parsed.Host != "vocaloid.fandom.com" || !strings.HasPrefix(parsed.EscapedPath(), "/wiki/") || parsed.EscapedPath() == "/wiki/" {
			return false
		}
		query := parsed.Query()
		return len(query) == 1 && len(query["oldid"]) == 1 && query.Get("oldid") == strconv.Itoa(candidate.RevisionID) &&
			parsed.RawQuery == query.Encode()
	case model.LyricsSourceProviderMoegirl:
		canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
		query := canonical.Query()
		query.Set("oldid", strconv.Itoa(candidate.RevisionID))
		query.Set("title", candidate.Title)
		canonical.RawQuery = query.Encode()
		return candidate.CanonicalURL == canonical.String()
	case model.LyricsSourceProviderMoegirlPublicExact:
		target, err := lyricssource.MoegirlPageURLTargetForURL(candidate.CanonicalURL)
		return err == nil && parsed.RawQuery == "" && target.PageTitle == candidate.Title
	case model.LyricsSourceProviderSekaipedia:
		if parsed.Host != "www.sekaipedia.org" || !strings.HasPrefix(parsed.EscapedPath(), "/wiki/") || parsed.EscapedPath() == "/wiki/" {
			return false
		}
		query := parsed.Query()
		return len(query) == 1 && len(query["oldid"]) == 1 && query.Get("oldid") == strconv.Itoa(candidate.RevisionID) &&
			parsed.RawQuery == query.Encode()
	default:
		return false
	}
}

func canonicalCandidateRevisionTimestamp(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && strings.HasSuffix(value, "Z") && parsed.Unix() > 0 &&
		parsed.UTC().Format(time.RFC3339Nano) == value
}

func validatePreflightReference(reference PreflightReference) error {
	parsedGeneratedAt, err := time.Parse(time.RFC3339Nano, reference.GeneratedAt)
	if err != nil || parsedGeneratedAt.Format(time.RFC3339Nano) != reference.GeneratedAt ||
		!strings.HasSuffix(reference.GeneratedAt, "Z") || reference.SchemaVersion != PreflightSchemaVersion ||
		reference.CatalogSchemaVersion != CatalogSchemaVersion || reference.CatalogCount < reference.UniqueCompleteCount ||
		reference.CatalogCount < 0 || reference.CatalogCount > maxCatalogRecords || reference.UniqueCompleteCount <= 0 ||
		!canonicalSHA256.MatchString(reference.ReportSHA256) {
		return errors.New("staging manifest has an invalid preflight reference")
	}
	return nil
}

func validateDraftPublicFields(draft Draft) error {
	if draft.MusicID <= 0 || draft.TargetMusicID != draft.MusicID || draft.JapaneseTitle == "" ||
		strings.TrimSpace(draft.JapaneseTitle) != draft.JapaneseTitle || len(draft.JapaneseTitle) > maxCandidateTitle ||
		strings.ContainsAny(draft.JapaneseTitle, "\r\n") || !canonicalSHA256.MatchString(draft.CatalogFingerprint) ||
		draft.AssociationMusicIDs == nil || !strictlyIncreasingPositiveInts(draft.AssociationMusicIDs) {
		return fmt.Errorf("staged music %d has invalid catalog identity", draft.MusicID)
	}
	for _, association := range draft.AssociationMusicIDs {
		if association == draft.MusicID {
			return fmt.Errorf("staged music %d contains itself as an association", draft.MusicID)
		}
	}
	fetchedAt, fetchedAtErr := time.Parse(time.RFC3339Nano, draft.Source.FetchedAt)
	if draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 &&
		(draft.Source.PageID <= 0 || draft.Source.RevisionID <= 0 || !canonicalSHA1.MatchString(draft.Source.SHA1) ||
			draft.Source.PageTitle == "" || strings.TrimSpace(draft.Source.PageTitle) != draft.Source.PageTitle ||
			len(draft.Source.PageTitle) > maxCandidateTitle || strings.ContainsAny(draft.Source.PageTitle, "\r\n") ||
			draft.Source.Categories == nil || fetchedAtErr != nil || fetchedAt.Unix() <= 0 ||
			fetchedAt.UTC().Format(time.RFC3339Nano) != draft.Source.FetchedAt || !strings.HasSuffix(draft.Source.FetchedAt, "Z") ||
			draft.Source.RawWikitextByteCount <= 0 || draft.Source.RawWikitextByteCount > 2<<20 ||
			!canonicalSHA256.MatchString(draft.Source.RawWikitextSHA256)) {
		return fmt.Errorf("staged music %d has invalid fixed source metadata", draft.MusicID)
	}
	if draft.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		if err := validateV3DraftPublicFields(draft); err != nil {
			return err
		}
		return validateDraftProvenance(draft)
	}
	if draft.SelectedVersion.Kind != "original" && draft.SelectedVersion.Kind != "sekai" && draft.SelectedVersion.Kind != "vocaloid" {
		return fmt.Errorf("staged music %d has an invalid selected version kind", draft.MusicID)
	}
	canonicalRubyVersion, rubyVersionErr := lyricssource.RecoveryPersistedRubyGeneratorVersion(draft.RubyGeneratorVersion)
	if strings.TrimSpace(draft.SelectedVersion.Label) == "" || draft.SelectedVersion.Label != strings.TrimSpace(draft.SelectedVersion.Label) ||
		len(draft.SelectedVersion.Label) > maxStagedVersionLabelBytes || strings.ContainsAny(draft.SelectedVersion.Label, "\r\n") ||
		draft.RubyGeneratorVersion != strings.TrimSpace(draft.RubyGeneratorVersion) || rubyVersionErr != nil ||
		draft.RubyGeneratorVersion != canonicalRubyVersion || len(draft.RubyGeneratorVersion) > maxStagedRubyGeneratorVersionBytes ||
		strings.ContainsAny(draft.RubyGeneratorVersion, "\r\n") || draft.Performers == nil ||
		len(draft.Performers) > maxStagedPerformers || draft.Lines == nil || len(draft.Lines) == 0 || len(draft.Lines) > 1000 {
		return fmt.Errorf("staged music %d has invalid extraction metadata", draft.MusicID)
	}
	seenPerformers := make(map[string]struct{}, len(draft.Performers))
	for _, performer := range draft.Performers {
		if len(performer.PerformerID) == 0 || len(performer.PerformerID) > maxStagedPerformerIDBytes ||
			!canonicalPerformerID.MatchString(performer.PerformerID) || len(performer.Name) == 0 ||
			len(performer.Name) > maxStagedPerformerNameBytes || strings.TrimSpace(performer.Name) == "" ||
			performer.Name != strings.TrimSpace(performer.Name) || strings.ContainsAny(performer.Name, "\r\n") ||
			len(performer.Color) > maxStagedPerformerColorBytes || performer.Color != "" && !canonicalColor.MatchString(performer.Color) {
			return fmt.Errorf("staged music %d has an invalid performer", draft.MusicID)
		}
		if _, exists := seenPerformers[performer.PerformerID]; exists {
			return fmt.Errorf("staged music %d has duplicate performers", draft.MusicID)
		}
		seenPerformers[performer.PerformerID] = struct{}{}
	}
	if err := validateLines(draft.MusicID, draft.Lines, seenPerformers); err != nil {
		return err
	}
	if err := validateDraftTranslations(draft.MusicID, draft.Translations, len(draft.Lines)); err != nil {
		return err
	}
	if model.LyricsSourceExtractedLinesSHA256(draft.Lines) != draft.ExtractedLinesSHA256 ||
		!canonicalSHA256.MatchString(draft.ExtractedLinesSHA256) {
		return fmt.Errorf("staged music %d extracted-lines digest does not match", draft.MusicID)
	}
	if err := validateDraftProvenance(draft); err != nil {
		return err
	}
	return nil
}

func validateDraftProvenance(draft Draft) error {
	if draft.Artifacts == nil || len(draft.Artifacts) == 0 || len(draft.Artifacts) > 16 {
		return fmt.Errorf("staged music %d requires bounded source artifacts", draft.MusicID)
	}
	if err := model.ValidateLyricsSourceDocument(draft.Document); err != nil {
		return fmt.Errorf("staged music %d source document: %w", draft.MusicID, err)
	}
	if draft.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		if err := validateV3FullMetadata(draft.Document); err != nil {
			return fmt.Errorf("staged music %d source document metadata: %w", draft.MusicID, err)
		}
		return validateV3DraftProvenance(draft)
	}
	if err := lyricscontract.ValidatePersistedPerformerMetadata(draft.Document.Full); err != nil {
		return fmt.Errorf("staged music %d source document has unsafe persisted performer metadata", draft.MusicID)
	}
	if err := validateVNextLyricsSourceDocument(draft.Document); err != nil {
		return fmt.Errorf("staged music %d source document: %w", draft.MusicID, err)
	}
	documentDigest, err := lyricsSourceDocumentDigest(draft.Document)
	if err != nil || !canonicalSHA256.MatchString(draft.DocumentSHA256) || documentDigest != draft.DocumentSHA256 {
		return fmt.Errorf("staged music %d source-document digest does not match", draft.MusicID)
	}
	artifactsByRendition := make(map[string]Artifact, len(draft.Artifacts))
	lastKey := ""
	for index, artifact := range draft.Artifacts {
		if err := model.ValidateLyricsSourceFixedIdentity(artifact.Identity); err != nil {
			return fmt.Errorf("staged music %d artifact %d: %w", draft.MusicID, index+1, err)
		}
		if artifact.Identity.RenditionKey <= lastKey {
			return fmt.Errorf("staged music %d artifacts are not strictly ordered by rendition key", draft.MusicID)
		}
		lastKey = artifact.Identity.RenditionKey
		if artifact.RawWikitextByteCount <= 0 || artifact.RawWikitextByteCount > 2<<20 ||
			!canonicalSHA256.MatchString(artifact.RawWikitextSHA256) || !canonicalSHA256.MatchString(artifact.ArtifactSHA256) {
			return fmt.Errorf("staged music %d artifact %q has invalid raw-byte evidence", draft.MusicID, artifact.Identity.RenditionKey)
		}
		artifactDigest, digestErr := stagedArtifactDigest(artifact)
		if digestErr != nil || artifactDigest != artifact.ArtifactSHA256 {
			return fmt.Errorf("staged music %d artifact %q digest does not match", draft.MusicID, artifact.Identity.RenditionKey)
		}
		artifactsByRendition[artifact.Identity.RenditionKey] = artifact
	}
	if len(artifactsByRendition) != len(draft.Document.FixedIdentities) {
		return fmt.Errorf("staged music %d source document and artifacts differ", draft.MusicID)
	}
	for index, identity := range draft.Document.FixedIdentities {
		artifact, exists := artifactsByRendition[identity.RenditionKey]
		if !exists || !equalFixedIdentity(artifact.Identity, identity) {
			return fmt.Errorf("staged music %d fixed identity %q has no exact artifact", draft.MusicID, identity.RenditionKey)
		}
		if draft.Artifacts[index].Identity.RenditionKey != identity.RenditionKey {
			return fmt.Errorf("staged music %d fixed identities are not ordered with artifacts", draft.MusicID)
		}
	}
	referenced := map[string]bool{
		draft.Document.Provenance.FullText.RenditionKey:        true,
		draft.Document.Provenance.VersionEvidence.RenditionKey: true,
	}
	for _, reference := range []*model.LyricsSourceComponentRef{
		draft.Document.Provenance.GameText, draft.Document.Provenance.PerformerSegmentation,
		draft.Document.Provenance.GameProjection, draft.Document.Provenance.Ruby,
	} {
		if reference != nil {
			referenced[reference.RenditionKey] = true
		}
	}
	for _, alternate := range draft.Document.AlternateVocals {
		referenced[alternate.Provenance.VersionEvidence.RenditionKey] = true
		for _, reference := range []*model.LyricsSourceComponentRef{
			alternate.Provenance.FullText, alternate.Provenance.GameText, alternate.Provenance.GameProjection,
		} {
			if reference != nil {
				referenced[reference.RenditionKey] = true
			}
		}
	}
	for renditionKey := range artifactsByRendition {
		if !referenced[renditionKey] {
			return fmt.Errorf("staged music %d artifact %q has no component contribution", draft.MusicID, renditionKey)
		}
	}
	fullArtifact, exists := artifactsByRendition[draft.Document.Provenance.FullText.RenditionKey]
	if !exists {
		return fmt.Errorf("staged music %d full-text provenance has no artifact", draft.MusicID)
	}
	identity := fullArtifact.Identity
	if draft.Source.PageID != identity.PageID || draft.Source.RevisionID != identity.RevisionID ||
		draft.Source.SHA1 != identity.SHA1 || draft.Source.PageTitle != identity.Title ||
		draft.Source.CanonicalURL != identity.CanonicalURL || draft.Source.FetchedAt != identity.FetchedAt ||
		!equalStrings(draft.Source.Categories, identity.Categories) ||
		draft.Source.RawWikitextByteCount != fullArtifact.RawWikitextByteCount ||
		draft.Source.RawWikitextSHA256 != fullArtifact.RawWikitextSHA256 {
		return fmt.Errorf("staged music %d legacy source projection differs from full-text provenance", draft.MusicID)
	}
	legacyLines := draft.Document.Full.LegacyExtractedLines()
	if draft.SelectedVersion != draft.Document.Full.Version || !equalLyricsSourcePerformers(draft.Performers, draft.Document.Full.Performers) ||
		draft.RubyGeneratorVersion != draft.Document.Full.RubyGeneratorVersion || !equalLyricsSourceLines(draft.Lines, legacyLines) {
		return fmt.Errorf("staged music %d legacy extraction projection differs from source document", draft.MusicID)
	}
	return nil
}

func validateVNextLyricsSourceDocument(document model.LyricsSourceDocument) error {
	switch document.Full.Version.Kind {
	case "sekai":
		return nil
	case "original", "vocaloid":
		if document.Provenance.PerformerSegmentation != nil {
			if lyricscontract.AcceptsAuthoritativePerformerSegmentation(document) {
				return nil
			}
			return errors.New("non-SEKAI performer segmentation requires authoritative structured source evidence")
		}
		if !lyricscontract.FullIsCompleteAndPerformerFree(document.Full) {
			return errors.New("unsegmented Full must retain one complete performer-free segment per line")
		}
		return nil
	default:
		return nil
	}
}

func validateDraftTranslations(musicID int, translations []string, lineCount int) error {
	if translations == nil {
		return nil
	}
	if len(translations) == 0 || len(translations) != lineCount {
		return fmt.Errorf("staged music %d translations do not align one-to-one with Japanese lines", musicID)
	}
	totalBytes := 0
	for index, translation := range translations {
		if translation == "" || strings.TrimSpace(translation) != translation || !utf8.ValidString(translation) ||
			strings.ContainsAny(translation, "\r\n\x00") || len(translation) > 16<<10 ||
			totalBytes > 2<<20-len(translation) {
			return fmt.Errorf("staged music %d translation %d exceeds the private text boundary", musicID, index+1)
		}
		totalBytes += len(translation)
	}
	return nil
}

func validateLines(musicID int, lines []model.LyricsSourceExtractedLine, performerLegend map[string]struct{}) error {
	totalBytes := 0
	totalRubyReadingBytes := 0
	requireLegendReferences := len(performerLegend) > 0
	for lineIndex, line := range lines {
		if line.Japanese == "" || strings.TrimSpace(line.Japanese) == "" || len(line.Japanese) > 8<<10 || line.Segments == nil ||
			len(line.Segments) == 0 || len(line.Segments) > maxStagedSegmentsPerLine || line.TrailingPerformerIDs == nil {
			return fmt.Errorf("staged music %d line %d is empty or incomplete", musicID, lineIndex+1)
		}
		totalBytes += len(line.Japanese)
		if totalBytes > 1<<20 {
			return fmt.Errorf("staged music %d extracted text exceeds the safe limit", musicID)
		}
		if err := validatePerformerReferences(line.TrailingPerformerIDs, performerLegend, requireLegendReferences); err != nil {
			return fmt.Errorf("staged music %d line %d trailing performers: %w", musicID, lineIndex+1, err)
		}
		var lineText strings.Builder
		for segmentIndex, segment := range line.Segments {
			if segment.Text == "" || strings.TrimSpace(segment.Text) == "" || segment.PerformerIDs == nil ||
				segment.Ruby == nil || len(segment.Ruby) == 0 || len(segment.Ruby) > maxStagedRubySpansPerSegment {
				return fmt.Errorf("staged music %d line %d segment %d is empty or incomplete", musicID, lineIndex+1, segmentIndex+1)
			}
			if err := validatePerformerReferences(segment.PerformerIDs, performerLegend, requireLegendReferences); err != nil {
				return fmt.Errorf("staged music %d line %d segment %d performers: %w", musicID, lineIndex+1, segmentIndex+1, err)
			}
			lineText.WriteString(segment.Text)
			var rubyText strings.Builder
			for spanIndex, span := range segment.Ruby {
				if span.Text == "" || len(span.Text) > maxStagedRubyTextBytes || len(span.Reading) > maxStagedRubyReadingBytes {
					return fmt.Errorf("staged music %d line %d segment %d ruby span %d has invalid text or reading", musicID, lineIndex+1, segmentIndex+1, spanIndex+1)
				}
				if totalRubyReadingBytes > maxStagedRubyReadingTotalBytes-len(span.Reading) {
					return fmt.Errorf("staged music %d ruby readings exceed the safe limit", musicID)
				}
				totalRubyReadingBytes += len(span.Reading)
				rubyText.WriteString(span.Text)
			}
			if rubyText.String() != segment.Text {
				return fmt.Errorf("staged music %d line %d segment %d ruby spans do not concatenate to segment text", musicID, lineIndex+1, segmentIndex+1)
			}
		}
		if lineText.String() != line.Japanese {
			return fmt.Errorf("staged music %d line %d segments do not concatenate to Japanese text", musicID, lineIndex+1)
		}
	}
	return nil
}

func validatePerformerReferences(references []string, performerLegend map[string]struct{}, requireLegendReferences bool) error {
	if len(references) > maxStagedPerformers {
		return errors.New("too many performer IDs")
	}
	seen := make(map[string]struct{}, len(references))
	for _, performerID := range references {
		if len(performerID) == 0 || len(performerID) > maxStagedPerformerIDBytes || !canonicalPerformerID.MatchString(performerID) {
			return errors.New("invalid performer ID")
		}
		if _, exists := seen[performerID]; exists {
			return errors.New("duplicate performer ID")
		}
		seen[performerID] = struct{}{}
		if requireLegendReferences {
			if _, exists := performerLegend[performerID]; !exists {
				return errors.New("unknown performer ID")
			}
		}
	}
	return nil
}

func strictlyIncreasingPositiveInts(values []int) bool {
	last := 0
	for _, value := range values {
		if value <= last {
			return false
		}
		last = value
	}
	return true
}

func sortedUniqueInts(values []int) ([]int, error) {
	result := append([]int{}, values...)
	sort.Ints(result)
	if !strictlyIncreasingPositiveInts(result) {
		if len(result) == 0 {
			return result, nil
		}
		return nil, errors.New("association IDs must be positive and unique")
	}
	return result, nil
}
