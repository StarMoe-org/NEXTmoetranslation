package lyricsstaging

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

const (
	PrivateEvidenceReceiptSchemaVersion = 1
	MaxPrivateEvidenceReceiptBytes      = 64 << 20
	MaxPrivateEvidenceReceiptRawBytes   = 32 << 20
	MaxPrivateEvidenceReceiptItems      = 64 << 10

	MaxFixedArtifactBundleArtifacts = 16
	MaxFixedArtifactBundleRawBytes  = MaxFixedArtifactBundleArtifacts * (2 << 20)
)

// PrivateEvidenceReceipt is the private preflight-to-stage handoff for exact
// candidate evidence. It is deliberately separate from Manifest: staged
// manifests retain only compact {evidenceId,sha256} references and never copy
// concrete evidence or raw source bytes.
type PrivateEvidenceReceipt struct {
	SchemaVersion int                          `json:"schemaVersion"`
	IndexEvidence []lyricssource.IndexEvidence `json:"indexEvidence"`
	ReceiptSHA256 string                       `json:"receiptSha256"`
}

// FixedArtifact is one provider/rendition-specific immutable fetch. Candidate
// is refs-only; Fixed owns the exact revision bytes for that identity.
type FixedArtifact struct {
	Candidate CandidateIdentity
	Fixed     lyricssource.FixedRevision
}

// FixedArtifactComponentSelection maps each composed component to the unique
// artifact key that supplied it. Logical candidate rendition keys may collide
// across providers; component refs never do.
type FixedArtifactComponentSelection struct {
	FullText              string
	GameText              string
	PerformerSegmentation string
	GameProjection        string
	Ruby                  string
	VersionEvidence       string
}

// FixedArtifactBundle is the bounded private input to the staging builder. A
// multi-provider document must provide one exact raw revision per fixed
// identity rather than reusing the primary FixedRevision.Wikitext for every
// artifact. CompositionReason is independent of each candidate VersionReason.
type FixedArtifactBundle struct {
	PostFetchState    PostFetchState
	CompositionReason model.LyricsSourceVersionReasonCode
	Artifacts         []FixedArtifact
	Components        FixedArtifactComponentSelection
	EvidenceReceipt   PrivateEvidenceReceipt
	EvidenceResolver  *PrivateEvidenceResolver
}

func NewPrivateEvidenceReceipt(evidence []lyricssource.IndexEvidence) (PrivateEvidenceReceipt, error) {
	if len(evidence) > MaxPrivateEvidenceReceiptItems {
		return PrivateEvidenceReceipt{}, privateEvidenceReceiptCapacityError(len(evidence), 0)
	}

	// Reject every per-envelope raw overflow before allocating the canonical
	// index. The exact aggregate remains canonical-unique so repeated immutable
	// acquisitions are charged once.
	for _, item := range evidence {
		if len(item.Raw) > lyricssource.MaxIndexEvidenceRawBytes {
			return PrivateEvidenceReceipt{}, lyricssource.ValidateIndexEvidenceEnvelope(item)
		}
	}

	// Keep only shallow indexes until all canonical-unique capacity and exact
	// envelope checks have passed. This prevents oversized or malformed input
	// from triggering raw-byte clones, and bounds the index allocation by the
	// reviewed receipt item limit.
	byID := make(map[string]int, len(evidence))
	canonicalInput := make([]lyricssource.IndexEvidence, 0, len(evidence))
	totalRaw := int64(0)
	for _, item := range evidence {
		if existingIndex, found := byID[item.EvidenceID]; found {
			if !samePrivateIndexEvidence(canonicalInput[existingIndex], item) {
				return PrivateEvidenceReceipt{}, errors.New("private evidence ID has conflicting exact resolutions")
			}
			continue
		}
		if int64(len(item.Raw)) > int64(MaxPrivateEvidenceReceiptRawBytes)-totalRaw {
			return PrivateEvidenceReceipt{}, privateEvidenceReceiptCapacityError(
				len(evidence), totalRaw+int64(len(item.Raw)),
			)
		}
		byID[item.EvidenceID] = len(canonicalInput)
		canonicalInput = append(canonicalInput, item)
		totalRaw += int64(len(item.Raw))
	}

	sort.Slice(canonicalInput, func(left, right int) bool {
		return canonicalInput[left].EvidenceID < canonicalInput[right].EvidenceID
	})
	encodedProbe := PrivateEvidenceReceipt{
		SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
		IndexEvidence: canonicalInput,
		ReceiptSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
	}
	if _, err := privateEvidenceReceiptJSONSize(encodedProbe, true, true); err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	for _, item := range canonicalInput {
		if err := lyricssource.ValidateIndexEvidenceEnvelope(item); err != nil {
			return PrivateEvidenceReceipt{}, err
		}
	}
	canonical := make([]lyricssource.IndexEvidence, len(canonicalInput))
	for index, item := range canonicalInput {
		canonical[index] = clonePrivateIndexEvidence(item)
	}

	receipt := PrivateEvidenceReceipt{
		SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
		IndexEvidence: canonical,
	}
	digest, err := privateEvidenceReceiptDigest(receipt)
	if err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest
	if err := validatePrivateEvidenceReceipt(receipt, true, true); err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	return receipt, nil
}

func DecodePrivateEvidenceReceipt(body []byte) (PrivateEvidenceReceipt, error) {
	if len(body) > MaxPrivateEvidenceReceiptBytes {
		return PrivateEvidenceReceipt{}, fmt.Errorf("private evidence receipt exceeds %d bytes", MaxPrivateEvidenceReceiptBytes)
	}
	var receipt PrivateEvidenceReceipt
	if err := decodeClosedUniqueJSON(body, &receipt); err != nil {
		return PrivateEvidenceReceipt{}, fmt.Errorf("decode private evidence receipt: %w", err)
	}
	if err := ValidatePrivateEvidenceReceipt(receipt); err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	return receipt, nil
}

func MarshalPrivateEvidenceReceipt(receipt PrivateEvidenceReceipt) ([]byte, error) {
	if err := ValidatePrivateEvidenceReceipt(receipt); err != nil {
		return nil, err
	}
	return MarshalValidatedPrivateEvidenceReceipt(receipt)
}

// MarshalValidatedPrivateEvidenceReceipt serializes a receipt that was already
// validated in the same closed call chain. It retains the encoded-size bound
// without repeating digest, envelope, and provider-specific raw validation.
func MarshalValidatedPrivateEvidenceReceipt(receipt PrivateEvidenceReceipt) ([]byte, error) {
	encodedBytes, err := privateEvidenceReceiptJSONSize(receipt, true, true)
	if err != nil {
		if errors.Is(err, errPrivateEvidenceReceiptEncodedLimit) {
			return nil, fmt.Errorf("private evidence receipt exceeds %d bytes", MaxPrivateEvidenceReceiptBytes)
		}
		return nil, fmt.Errorf("encode private evidence receipt: %w", err)
	}
	var body bytes.Buffer
	body.Grow(encodedBytes)
	writer := &privateEvidenceReceiptLimitWriter{writer: &body, remaining: MaxPrivateEvidenceReceiptBytes}
	if err := writePrivateEvidenceReceiptJSON(writer, receipt, true, true); err != nil {
		if errors.Is(err, errPrivateEvidenceReceiptEncodedLimit) {
			return nil, fmt.Errorf("private evidence receipt exceeds %d bytes", MaxPrivateEvidenceReceiptBytes)
		}
		return nil, fmt.Errorf("encode private evidence receipt: %w", err)
	}
	return body.Bytes(), nil
}

func ValidatePrivateEvidenceReceipt(receipt PrivateEvidenceReceipt) error {
	return validatePrivateEvidenceReceipt(receipt, false, false)
}

func validatePrivateEvidenceReceipt(receipt PrivateEvidenceReceipt, digestVerified, evidenceVerified bool) error {
	if receipt.SchemaVersion != PrivateEvidenceReceiptSchemaVersion || receipt.IndexEvidence == nil ||
		len(receipt.IndexEvidence) == 0 || !canonicalSHA256.MatchString(receipt.ReceiptSHA256) {
		return errors.New("private evidence receipt envelope is invalid")
	}
	if err := validatePrivateEvidenceReceiptCapacity(receipt.IndexEvidence); err != nil {
		return err
	}
	lastID := ""
	for index, evidence := range receipt.IndexEvidence {
		if evidence.EvidenceID == "" || index > 0 && lastID >= evidence.EvidenceID {
			return errors.New("private evidence receipt is not uniquely ordered by evidence ID")
		}
		lastID = evidence.EvidenceID
	}
	if _, err := privateEvidenceReceiptJSONSize(receipt, true, true); err != nil {
		return err
	}
	if !digestVerified {
		digest, err := privateEvidenceReceiptDigest(receipt)
		if err != nil {
			return err
		}
		if digest != receipt.ReceiptSHA256 {
			return errors.New("private evidence receipt digest does not match")
		}
	}
	if !evidenceVerified {
		for _, evidence := range receipt.IndexEvidence {
			if err := lyricssource.ValidateIndexEvidenceEnvelope(evidence); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePrivateEvidenceReceiptCapacity(evidence []lyricssource.IndexEvidence) error {
	if len(evidence) > MaxPrivateEvidenceReceiptItems {
		return privateEvidenceReceiptCapacityError(len(evidence), 0)
	}
	totalRaw := int64(0)
	for _, item := range evidence {
		if len(item.Raw) > lyricssource.MaxIndexEvidenceRawBytes {
			return lyricssource.ValidateIndexEvidenceEnvelope(item)
		}
		totalRaw += int64(len(item.Raw))
	}
	if totalRaw > int64(MaxPrivateEvidenceReceiptRawBytes) {
		return privateEvidenceReceiptCapacityError(len(evidence), totalRaw)
	}
	return nil
}

func privateEvidenceReceiptCapacityError(itemCount int, rawBytes int64) error {
	return fmt.Errorf(
		"private evidence receipt capacity exceeded: item count %d (limit %d), aggregate raw bytes %d (limit %d)",
		itemCount, MaxPrivateEvidenceReceiptItems, rawBytes, MaxPrivateEvidenceReceiptRawBytes,
	)
}

func privateEvidenceReceiptDigest(receipt PrivateEvidenceReceipt) (string, error) {
	receipt.ReceiptSHA256 = ""
	digest := sha256.New()
	writer := &privateEvidenceReceiptLimitWriter{writer: digest, remaining: MaxPrivateEvidenceReceiptBytes}
	if err := writePrivateEvidenceReceiptJSON(writer, receipt, false, false); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

var errPrivateEvidenceReceiptEncodedLimit = errors.New("private evidence receipt exceeds its encoded safe limit")

type privateEvidenceReceiptLimitWriter struct {
	writer    io.Writer
	remaining int
	count     int
	scratch   [32 << 10]byte
}

func (writer *privateEvidenceReceiptLimitWriter) Write(data []byte) (int, error) {
	if len(data) > writer.remaining {
		return 0, errPrivateEvidenceReceiptEncodedLimit
	}
	written, err := writer.writer.Write(data)
	writer.remaining -= written
	writer.count += written
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	return written, err
}

func (writer *privateEvidenceReceiptLimitWriter) WriteString(data string) (int, error) {
	if len(data) > writer.remaining {
		return 0, errPrivateEvidenceReceiptEncodedLimit
	}
	if stringWriter, ok := writer.writer.(io.StringWriter); ok {
		written, err := stringWriter.WriteString(data)
		writer.remaining -= written
		writer.count += written
		if err == nil && written != len(data) {
			err = io.ErrShortWrite
		}
		return written, err
	}

	total := 0
	for len(data) > 0 {
		chunk := len(data)
		if chunk > len(writer.scratch) {
			chunk = len(writer.scratch)
		}
		copy(writer.scratch[:chunk], data[:chunk])
		written, err := writer.Write(writer.scratch[:chunk])
		total += written
		if err != nil {
			return total, err
		}
		data = data[chunk:]
	}
	return total, nil
}

func privateEvidenceReceiptJSONSize(receipt PrivateEvidenceReceipt, pretty, trailingNewline bool) (int, error) {
	writer := &privateEvidenceReceiptLimitWriter{writer: io.Discard, remaining: MaxPrivateEvidenceReceiptBytes}
	if err := writePrivateEvidenceReceiptJSON(writer, receipt, pretty, trailingNewline); err != nil {
		return 0, err
	}
	return writer.count, nil
}

type privateEvidenceJSONEncoder struct {
	writer io.Writer
	pretty bool
}

func writePrivateEvidenceReceiptJSON(
	writer io.Writer,
	receipt PrivateEvidenceReceipt,
	pretty, trailingNewline bool,
) error {
	encoder := privateEvidenceJSONEncoder{writer: writer, pretty: pretty}
	if err := encoder.write("{"); err != nil {
		return err
	}
	if err := encoder.field(1, 0, "schemaVersion"); err != nil {
		return err
	}
	if err := encoder.writeInt(receipt.SchemaVersion); err != nil {
		return err
	}
	if err := encoder.field(1, 1, "indexEvidence"); err != nil {
		return err
	}
	if err := encoder.write("["); err != nil {
		return err
	}
	for index, evidence := range receipt.IndexEvidence {
		if err := encoder.element(2, index); err != nil {
			return err
		}
		if err := encoder.writeIndexEvidence(evidence, 2); err != nil {
			return err
		}
	}
	if err := encoder.closeComposite("]", 1, len(receipt.IndexEvidence) > 0); err != nil {
		return err
	}
	if err := encoder.field(1, 2, "receiptSha256"); err != nil {
		return err
	}
	if err := encoder.writeString(receipt.ReceiptSHA256); err != nil {
		return err
	}
	if err := encoder.closeComposite("}", 0, true); err != nil {
		return err
	}
	if trailingNewline {
		return encoder.write("\n")
	}
	return nil
}

func (encoder *privateEvidenceJSONEncoder) writeIndexEvidence(
	evidence lyricssource.IndexEvidence,
	depth int,
) error {
	if err := encoder.write("{"); err != nil {
		return err
	}
	fieldIndex := 0
	writeStringField := func(name, value string) error {
		if err := encoder.field(depth+1, fieldIndex, name); err != nil {
			return err
		}
		fieldIndex++
		return encoder.writeString(value)
	}
	writeIntField := func(name string, value int) error {
		if err := encoder.field(depth+1, fieldIndex, name); err != nil {
			return err
		}
		fieldIndex++
		return encoder.writeInt(value)
	}
	if err := writeStringField("evidenceId", evidence.EvidenceID); err != nil {
		return err
	}
	if err := writeStringField("sha256", evidence.SHA256); err != nil {
		return err
	}
	if err := writeStringField("kind", string(evidence.Kind)); err != nil {
		return err
	}
	if err := writeStringField("provider", string(evidence.Provider)); err != nil {
		return err
	}
	if err := writeStringField("origin", evidence.Origin); err != nil {
		return err
	}
	if evidence.PageID != 0 {
		if err := writeIntField("pageId", evidence.PageID); err != nil {
			return err
		}
	}
	if evidence.RevisionID != 0 {
		if err := writeIntField("revisionId", evidence.RevisionID); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "revisionTimestamp", value: evidence.RevisionTimestamp},
		{name: "mediawikiSha1", value: evidence.MediaWikiSHA1},
		{name: "title", value: evidence.Title},
		{name: "canonicalRevisionUrl", value: evidence.CanonicalURL},
	} {
		if field.value != "" {
			if err := writeStringField(field.name, field.value); err != nil {
				return err
			}
		}
	}
	if err := encoder.field(depth+1, fieldIndex, "categories"); err != nil {
		return err
	}
	fieldIndex++
	if err := encoder.writeStringArray(evidence.Categories, depth+1); err != nil {
		return err
	}
	if evidence.CanonicalRequestURL != "" {
		if err := writeStringField("canonicalRequestUrl", evidence.CanonicalRequestURL); err != nil {
			return err
		}
	}
	if err := writeStringField("fetchedAt", evidence.FetchedAt); err != nil {
		return err
	}
	if err := encoder.field(depth+1, fieldIndex, "raw"); err != nil {
		return err
	}
	fieldIndex++
	if err := encoder.writeBytes(evidence.Raw); err != nil {
		return err
	}
	if err := writeStringField("rawSha256", evidence.RawSHA256); err != nil {
		return err
	}
	return encoder.closeComposite("}", depth, fieldIndex > 0)
}

func (encoder *privateEvidenceJSONEncoder) field(depth, index int, name string) error {
	if index > 0 {
		if err := encoder.write(","); err != nil {
			return err
		}
	}
	if encoder.pretty {
		if err := encoder.write("\n"); err != nil {
			return err
		}
		if err := encoder.indent(depth); err != nil {
			return err
		}
	}
	if err := encoder.writeString(name); err != nil {
		return err
	}
	if encoder.pretty {
		return encoder.write(": ")
	}
	return encoder.write(":")
}

func (encoder *privateEvidenceJSONEncoder) element(depth, index int) error {
	if index > 0 {
		if err := encoder.write(","); err != nil {
			return err
		}
	}
	if encoder.pretty {
		if err := encoder.write("\n"); err != nil {
			return err
		}
		return encoder.indent(depth)
	}
	return nil
}

func (encoder *privateEvidenceJSONEncoder) closeComposite(token string, depth int, multiline bool) error {
	if encoder.pretty && multiline {
		if err := encoder.write("\n"); err != nil {
			return err
		}
		if err := encoder.indent(depth); err != nil {
			return err
		}
	}
	return encoder.write(token)
}

func (encoder *privateEvidenceJSONEncoder) indent(depth int) error {
	for index := 0; index < depth; index++ {
		if err := encoder.write("  "); err != nil {
			return err
		}
	}
	return nil
}

func (encoder *privateEvidenceJSONEncoder) write(value string) error {
	_, err := io.WriteString(encoder.writer, value)
	return err
}

func (encoder *privateEvidenceJSONEncoder) writeInt(value int) error {
	var buffer [32]byte
	encoded := strconv.AppendInt(buffer[:0], int64(value), 10)
	_, err := encoder.writer.Write(encoded)
	return err
}

func (encoder *privateEvidenceJSONEncoder) writeStringArray(values []string, depth int) error {
	if values == nil {
		return encoder.write("null")
	}
	if err := encoder.write("["); err != nil {
		return err
	}
	for index, value := range values {
		if err := encoder.element(depth+1, index); err != nil {
			return err
		}
		if err := encoder.writeString(value); err != nil {
			return err
		}
	}
	return encoder.closeComposite("]", depth, len(values) > 0)
}

func (encoder *privateEvidenceJSONEncoder) writeBytes(value []byte) error {
	if value == nil {
		return encoder.write("null")
	}
	if err := encoder.write("\""); err != nil {
		return err
	}
	base64Writer := base64.NewEncoder(base64.StdEncoding, encoder.writer)
	if _, err := base64Writer.Write(value); err != nil {
		return err
	}
	if err := base64Writer.Close(); err != nil {
		return err
	}
	return encoder.write("\"")
}

func (encoder *privateEvidenceJSONEncoder) writeString(value string) error {
	if err := encoder.write("\""); err != nil {
		return err
	}
	start := 0
	for index := 0; index < len(value); {
		character := value[index]
		if character < utf8.RuneSelf {
			if character >= 0x20 && character != '\\' && character != '"' &&
				character != '<' && character != '>' && character != '&' {
				index++
				continue
			}
			if start < index {
				if err := encoder.write(value[start:index]); err != nil {
					return err
				}
			}
			switch character {
			case '\\', '"':
				escaped := [2]byte{'\\', character}
				if _, err := encoder.writer.Write(escaped[:]); err != nil {
					return err
				}
			case '\n':
				if err := encoder.write("\\n"); err != nil {
					return err
				}
			case '\r':
				if err := encoder.write("\\r"); err != nil {
					return err
				}
			case '\t':
				if err := encoder.write("\\t"); err != nil {
					return err
				}
			case '\b':
				if err := encoder.write("\\b"); err != nil {
					return err
				}
			case '\f':
				if err := encoder.write("\\f"); err != nil {
					return err
				}
			default:
				const hexadecimal = "0123456789abcdef"
				escaped := [6]byte{'\\', 'u', '0', '0', hexadecimal[character>>4], hexadecimal[character&0x0f]}
				if _, err := encoder.writer.Write(escaped[:]); err != nil {
					return err
				}
			}
			index++
			start = index
			continue
		}

		runeValue, size := utf8.DecodeRuneInString(value[index:])
		if runeValue == utf8.RuneError && size == 1 {
			if start < index {
				if err := encoder.write(value[start:index]); err != nil {
					return err
				}
			}
			if err := encoder.write("\\ufffd"); err != nil {
				return err
			}
			index++
			start = index
			continue
		}
		if runeValue == '\u2028' || runeValue == '\u2029' {
			if start < index {
				if err := encoder.write(value[start:index]); err != nil {
					return err
				}
			}
			if runeValue == '\u2028' {
				if err := encoder.write("\\u2028"); err != nil {
					return err
				}
			} else if err := encoder.write("\\u2029"); err != nil {
				return err
			}
			index += size
			start = index
			continue
		}
		index += size
	}
	if start < len(value) {
		if err := encoder.write(value[start:]); err != nil {
			return err
		}
	}
	return encoder.write("\"")
}

func clonePrivateIndexEvidence(evidence lyricssource.IndexEvidence) lyricssource.IndexEvidence {
	evidence.Categories = append([]string(nil), evidence.Categories...)
	evidence.Raw = append([]byte(nil), evidence.Raw...)
	return evidence
}

func samePrivateIndexEvidence(left, right lyricssource.IndexEvidence) bool {
	return left.EvidenceID == right.EvidenceID && left.SHA256 == right.SHA256 && left.Kind == right.Kind &&
		left.Provider == right.Provider && left.Origin == right.Origin && left.PageID == right.PageID &&
		left.RevisionID == right.RevisionID && left.RevisionTimestamp == right.RevisionTimestamp &&
		left.MediaWikiSHA1 == right.MediaWikiSHA1 && left.Title == right.Title &&
		left.CanonicalURL == right.CanonicalURL && reflect.DeepEqual(left.Categories, right.Categories) &&
		left.CanonicalRequestURL == right.CanonicalRequestURL && left.FetchedAt == right.FetchedAt &&
		left.RawSHA256 == right.RawSHA256 && bytes.Equal(left.Raw, right.Raw)
}

// PrivateEvidenceResolver validates a private receipt once, then resolves any
// number of candidate subsets through an immutable exact-evidence index. It is
// safe for concurrent readers; returned candidates always receive defensive
// copies of concrete evidence bytes.
type privateEvidenceResolution struct {
	sha256    string
	rawSHA256 string
}

type PrivateEvidenceResolver struct {
	byID                           map[string]privateEvidenceResolution
	candidateResolver              *lyricssource.CandidateIndexEvidenceResolver
	sekaipediaEvidenceByID         map[string]lyricssource.IndexEvidence
	sekaipediaAuthority            lyricssource.FixedIndex
	sekaipediaAuthorityEvidenceIDs map[string]struct{}
}

func privateEvidenceIsSekaipediaOnly(evidence []lyricssource.IndexEvidence) bool {
	if len(evidence) == 0 {
		return false
	}
	for _, item := range evidence {
		if item.Provider != model.LyricsSourceProviderSekaipedia {
			return false
		}
	}
	return true
}

func newPrivateSekaipediaEvidenceResolver(
	evidence []lyricssource.IndexEvidence,
	cloneEvidence bool,
) (*PrivateEvidenceResolver, error) {
	resolver := &PrivateEvidenceResolver{
		byID:                           make(map[string]privateEvidenceResolution, len(evidence)),
		sekaipediaEvidenceByID:         make(map[string]lyricssource.IndexEvidence, len(evidence)),
		sekaipediaAuthorityEvidenceIDs: make(map[string]struct{}),
	}
	for _, item := range evidence {
		if _, found := resolver.byID[item.EvidenceID]; found {
			return nil, errors.New("private evidence receipt contains duplicate evidence")
		}
		if strings.HasPrefix(item.EvidenceID, "authority:sekaipedia:") {
			authority, err := lyricssource.SekaipediaAuthorityFromIndexEvidence(item)
			if err != nil {
				return nil, err
			}
			if resolver.sekaipediaAuthority.PageID != 0 && resolver.sekaipediaAuthority != authority {
				return nil, errors.New("private evidence receipt contains conflicting Sekaipedia authorities")
			}
			resolver.sekaipediaAuthority = authority
			resolver.sekaipediaAuthorityEvidenceIDs[item.EvidenceID] = struct{}{}
		}
		resolver.byID[item.EvidenceID] = privateEvidenceResolution{
			sha256: item.SHA256, rawSHA256: item.RawSHA256,
		}
		if cloneEvidence {
			item = clonePrivateIndexEvidence(item)
		}
		resolver.sekaipediaEvidenceByID[item.EvidenceID] = item
	}
	if resolver.sekaipediaAuthority.PageID == 0 || len(resolver.sekaipediaAuthorityEvidenceIDs) == 0 {
		return nil, errors.New("private evidence receipt has no Sekaipedia authority acquisition")
	}
	return resolver, nil
}

func NewPrivateEvidenceResolver(receipt PrivateEvidenceReceipt) (*PrivateEvidenceResolver, error) {
	if privateEvidenceIsSekaipediaOnly(receipt.IndexEvidence) {
		// The closed 704-item path shares one fixed List authority. Validate every
		// raw envelope exactly once, then retain one immutable receipt clone for
		// allocation-light candidate binding and batch-local hydration.
		if err := ValidatePrivateEvidenceReceipt(receipt); err != nil {
			return nil, err
		}
		return newPrivateSekaipediaEvidenceResolver(receipt.IndexEvidence, true)
	}

	// Validate the receipt envelope, capacity, ordering, encoded size, and digest
	// here. The candidate resolver then validates every exact evidence envelope
	// once while constructing the sole receipt-wide raw index.
	if err := validatePrivateEvidenceReceipt(receipt, false, true); err != nil {
		return nil, err
	}
	candidateResolver, err := lyricssource.NewCandidateIndexEvidenceResolver(receipt.IndexEvidence)
	if err != nil {
		return nil, err
	}
	resolver := &PrivateEvidenceResolver{
		byID:              make(map[string]privateEvidenceResolution, len(receipt.IndexEvidence)),
		candidateResolver: candidateResolver,
	}
	for _, evidence := range receipt.IndexEvidence {
		if _, found := resolver.byID[evidence.EvidenceID]; found {
			return nil, errors.New("private evidence receipt contains duplicate evidence")
		}
		resolver.byID[evidence.EvidenceID] = privateEvidenceResolution{
			sha256: evidence.SHA256, rawSHA256: evidence.RawSHA256,
		}
	}
	return resolver, nil
}

func (resolver *PrivateEvidenceResolver) validateCandidateReferences(candidate CandidateIdentity) error {
	if resolver == nil || resolver.byID == nil {
		return errors.New("private evidence resolver is required")
	}
	for _, reference := range candidate.IndexEvidenceRefs {
		evidence, found := resolver.byID[reference.EvidenceID]
		if !found || evidence.sha256 != reference.SHA256 || evidence.rawSHA256 != reference.SHA256 {
			return errors.New("candidate reference is unresolved by the private evidence receipt")
		}
	}
	return nil
}

func (resolver *PrivateEvidenceResolver) validateSekaipediaCandidate(candidate CandidateIdentity) error {
	if resolver == nil || resolver.sekaipediaEvidenceByID == nil ||
		!model.IsValidLyricsSourceProvider(candidate.Provider) || len(candidate.IndexEvidenceRefs) == 0 ||
		len(candidate.IndexEvidenceRefs) > 64 {
		return errors.New("candidate index evidence is incomplete")
	}
	if candidate.Origin != model.LyricsSourceOriginSekaipedia {
		return errors.New("candidate index evidence provider origin is invalid")
	}

	listCount := 0
	songCount := 0
	for index, reference := range candidate.IndexEvidenceRefs {
		for previous := 0; previous < index; previous++ {
			if candidate.IndexEvidenceRefs[previous].EvidenceID == reference.EvidenceID {
				return errors.New("candidate index evidence reference is duplicated")
			}
		}
		evidence, found := resolver.sekaipediaEvidenceByID[reference.EvidenceID]
		if !found || evidence.Provider != candidate.Provider || evidence.Origin != candidate.Origin {
			return errors.New("candidate index evidence provider does not match candidate")
		}
		_, authorityEvidence := resolver.sekaipediaAuthorityEvidenceIDs[evidence.EvidenceID]
		switch {
		case authorityEvidence:
			listCount++
		case privateSekaipediaRevisionEvidenceMatchesCandidate(evidence, candidate):
			songCount++
		default:
			return errors.New("Sekaipedia revision evidence is neither the fixed List nor the candidate song")
		}
	}
	if len(candidate.IndexEvidenceRefs) != 2 {
		return errors.New("Sekaipedia candidate requires fixed List and song revision evidence")
	}
	if listCount != 1 || songCount != 1 {
		return errors.New("Sekaipedia candidate evidence is not exactly one fixed List plus one song revision")
	}
	return nil
}

func privateSekaipediaRevisionEvidenceMatchesCandidate(
	evidence lyricssource.IndexEvidence,
	candidate CandidateIdentity,
) bool {
	if evidence.Kind != lyricssource.IndexEvidenceKindMediaWikiRevision ||
		evidence.Provider != model.LyricsSourceProviderSekaipedia ||
		evidence.Origin != model.LyricsSourceOriginSekaipedia ||
		evidence.EvidenceID != lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			model.LyricsSourceProviderSekaipedia,
			fmt.Sprintf("revision:sekaipedia:%d:%d", candidate.PageID, candidate.RevisionID),
			evidence.FetchedAt,
			evidence.RawSHA256,
		) || evidence.PageID != candidate.PageID || evidence.RevisionID != candidate.RevisionID ||
		evidence.RevisionTimestamp != candidate.RevisionTimestamp ||
		evidence.MediaWikiSHA1 != candidate.SHA1 || evidence.Title != candidate.Title ||
		evidence.CanonicalURL != candidate.CanonicalURL || len(evidence.Categories) != len(candidate.Categories) {
		return false
	}
	for index := range evidence.Categories {
		if evidence.Categories[index] != candidate.Categories[index] {
			return false
		}
	}
	return true
}

func (resolver *PrivateEvidenceResolver) hydrateSekaipediaCandidates(
	candidates []CandidateIdentity,
) ([]lyricssource.Candidate, error) {
	resolved := make([]lyricssource.Candidate, len(candidates))
	for index, candidate := range candidates {
		if err := resolver.validateCandidateReferences(candidate); err != nil {
			return nil, err
		}
		if err := resolver.validateSekaipediaCandidate(candidate); err != nil {
			return nil, err
		}
		resolved[index] = candidate.SourceCandidate()
	}
	clonedByID := make(map[string]lyricssource.IndexEvidence, len(resolver.sekaipediaEvidenceByID))
	for candidateIndex, candidate := range candidates {
		resolved[candidateIndex].IndexEvidence = make([]lyricssource.IndexEvidence, len(candidate.IndexEvidenceRefs))
		for evidenceIndex, reference := range candidate.IndexEvidenceRefs {
			cloned, found := clonedByID[reference.EvidenceID]
			if !found {
				cloned = clonePrivateIndexEvidence(resolver.sekaipediaEvidenceByID[reference.EvidenceID])
				clonedByID[reference.EvidenceID] = cloned
			}
			resolved[candidateIndex].IndexEvidence[evidenceIndex] = cloned
		}
	}
	return resolved, nil
}

// HydrateCandidate resolves one candidate without revalidating or rehashing the
// complete receipt.
func (resolver *PrivateEvidenceResolver) HydrateCandidate(candidate CandidateIdentity) (lyricssource.Candidate, error) {
	resolved, err := resolver.HydrateCandidates([]CandidateIdentity{candidate})
	if err != nil {
		return lyricssource.Candidate{}, err
	}
	return resolved[0], nil
}

// HydrateCandidates resolves a candidate subset in time linear in that subset's
// compact evidence references. Receipt-wide validation happened once when the
// resolver was constructed. One batch-local defensive clone is retained per
// canonical evidence envelope, even when many candidates share it.
func (resolver *PrivateEvidenceResolver) HydrateCandidates(candidates []CandidateIdentity) ([]lyricssource.Candidate, error) {
	if resolver == nil || resolver.byID == nil {
		return nil, errors.New("private evidence resolver is required")
	}
	if resolver.sekaipediaEvidenceByID != nil {
		return resolver.hydrateSekaipediaCandidates(candidates)
	}
	if resolver.candidateResolver == nil {
		return nil, errors.New("private evidence resolver is required")
	}
	sourceCandidates := make([]lyricssource.Candidate, len(candidates))
	for candidateIndex, candidate := range candidates {
		if err := resolver.validateCandidateReferences(candidate); err != nil {
			return nil, err
		}
		sourceCandidates[candidateIndex] = candidate.SourceCandidate()
	}
	return resolver.candidateResolver.ResolveCandidates(sourceCandidates)
}

// ValidateCandidates requires the supplied complete candidate set to resolve
// every receipt item exactly once by evidence identity, while allowing one
// immutable acquisition to be referenced by multiple candidates. It validates
// compact references directly and never accumulates hydrated raw copies.
func (resolver *PrivateEvidenceResolver) ValidateCandidates(candidates []CandidateIdentity) error {
	if resolver == nil || resolver.byID == nil {
		return errors.New("private evidence resolver is required")
	}
	used := make(map[string]struct{}, len(resolver.byID))
	for _, candidate := range candidates {
		if err := resolver.validateCandidateReferences(candidate); err != nil {
			return err
		}
		if resolver.sekaipediaEvidenceByID != nil {
			if err := resolver.validateSekaipediaCandidate(candidate); err != nil {
				return err
			}
		} else {
			if resolver.candidateResolver == nil {
				return errors.New("private evidence resolver is required")
			}
			if err := resolver.candidateResolver.ValidateCandidate(candidate.SourceCandidate()); err != nil {
				return err
			}
		}
		for _, reference := range candidate.IndexEvidenceRefs {
			used[reference.EvidenceID] = struct{}{}
		}
	}
	if len(used) != len(resolver.byID) {
		return errors.New("private evidence receipt contains orphan evidence")
	}
	return nil
}

// ProjectReceipt returns the deterministic exact union of already-validated
// evidence reachable from the supplied candidate set. Each selected envelope
// is defensively cloned once, shared references deduplicate only when their
// complete exact resolutions agree, and the canonical receipt digest is
// recomputed without reparsing or revalidating the source receipt per candidate.
func (resolver *PrivateEvidenceResolver) ProjectReceipt(candidates []CandidateIdentity) (PrivateEvidenceReceipt, error) {
	if resolver == nil || resolver.byID == nil {
		return PrivateEvidenceReceipt{}, errors.New("private evidence resolver is required")
	}
	if len(candidates) == 0 {
		return PrivateEvidenceReceipt{}, errors.New("private evidence receipt projection requires candidates")
	}

	projectedCapacity := 0
	for _, candidate := range candidates {
		if len(candidate.IndexEvidenceRefs) >= len(resolver.byID)-projectedCapacity {
			projectedCapacity = len(resolver.byID)
			break
		}
		projectedCapacity += len(candidate.IndexEvidenceRefs)
	}
	projectedByID := make(map[string]lyricssource.IndexEvidence, projectedCapacity)
	addProjected := func(evidence lyricssource.IndexEvidence, clone bool) error {
		if existing, found := projectedByID[evidence.EvidenceID]; found {
			if !samePrivateIndexEvidence(existing, evidence) {
				return errors.New("private evidence ID has conflicting exact resolutions")
			}
			return nil
		}
		if clone {
			evidence = clonePrivateIndexEvidence(evidence)
		}
		projectedByID[evidence.EvidenceID] = evidence
		return nil
	}

	if resolver.sekaipediaEvidenceByID != nil {
		for _, candidate := range candidates {
			if err := resolver.validateCandidateReferences(candidate); err != nil {
				return PrivateEvidenceReceipt{}, err
			}
			if err := resolver.validateSekaipediaCandidate(candidate); err != nil {
				return PrivateEvidenceReceipt{}, err
			}
			for _, reference := range candidate.IndexEvidenceRefs {
				if err := addProjected(resolver.sekaipediaEvidenceByID[reference.EvidenceID], true); err != nil {
					return PrivateEvidenceReceipt{}, err
				}
			}
		}
	} else {
		if resolver.candidateResolver == nil {
			return PrivateEvidenceReceipt{}, errors.New("private evidence resolver is required")
		}
		sourceCandidates := make([]lyricssource.Candidate, len(candidates))
		for index, candidate := range candidates {
			if err := resolver.validateCandidateReferences(candidate); err != nil {
				return PrivateEvidenceReceipt{}, err
			}
			sourceCandidates[index] = candidate.SourceCandidate()
		}
		resolved, err := resolver.candidateResolver.ResolveCandidates(sourceCandidates)
		if err != nil {
			return PrivateEvidenceReceipt{}, err
		}
		for _, candidate := range resolved {
			for _, evidence := range candidate.IndexEvidence {
				// ResolveCandidates already created one batch-local defensive clone
				// per selected EvidenceID, including overlapping references.
				if err := addProjected(evidence, false); err != nil {
					return PrivateEvidenceReceipt{}, err
				}
			}
		}
	}

	ids := make([]string, 0, len(projectedByID))
	for evidenceID := range projectedByID {
		ids = append(ids, evidenceID)
	}
	sort.Strings(ids)
	projected := make([]lyricssource.IndexEvidence, len(ids))
	for index, evidenceID := range ids {
		projected[index] = projectedByID[evidenceID]
	}
	return newPrivateEvidenceReceiptFromValidatedProjection(projected)
}

func newPrivateEvidenceReceiptFromValidatedProjection(
	evidence []lyricssource.IndexEvidence,
) (PrivateEvidenceReceipt, error) {
	if len(evidence) == 0 {
		return PrivateEvidenceReceipt{}, errors.New("private evidence receipt projection resolved no evidence")
	}
	if err := validatePrivateEvidenceReceiptCapacity(evidence); err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	for index, item := range evidence {
		if item.EvidenceID == "" || index > 0 && evidence[index-1].EvidenceID >= item.EvidenceID {
			return PrivateEvidenceReceipt{}, errors.New("private evidence receipt projection is not uniquely ordered by evidence ID")
		}
	}
	receipt := PrivateEvidenceReceipt{
		SchemaVersion: PrivateEvidenceReceiptSchemaVersion,
		IndexEvidence: append([]lyricssource.IndexEvidence(nil), evidence...),
	}
	probe := receipt
	probe.ReceiptSHA256 = strings.Repeat("0", 64)
	if _, err := privateEvidenceReceiptJSONSize(probe, true, true); err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	digest, err := privateEvidenceReceiptDigest(receipt)
	if err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest
	if err := validatePrivateEvidenceReceipt(receipt, true, true); err != nil {
		return PrivateEvidenceReceipt{}, err
	}
	return receipt, nil
}

func (resolver *PrivateEvidenceResolver) validateSekaipediaFixedArtifact(artifact FixedArtifact) error {
	if err := resolver.validateCandidateReferences(artifact.Candidate); err != nil {
		return err
	}
	if err := resolver.validateSekaipediaCandidate(artifact.Candidate); err != nil {
		return err
	}
	if len(artifact.Fixed.IndexEvidence) != len(artifact.Candidate.IndexEvidenceRefs) {
		return errors.New("candidate index evidence is incomplete")
	}
	resolved := make(map[string]lyricssource.IndexEvidence, len(artifact.Fixed.IndexEvidence))
	for _, evidence := range artifact.Fixed.IndexEvidence {
		if _, duplicate := resolved[evidence.EvidenceID]; duplicate {
			return errors.New("candidate index evidence ID resolves more than once")
		}
		canonical, found := resolver.sekaipediaEvidenceByID[evidence.EvidenceID]
		if !found || !samePrivateIndexEvidence(canonical, evidence) {
			return errors.New("candidate index evidence drifted from its validated resolution")
		}
		resolved[evidence.EvidenceID] = evidence
	}
	for _, reference := range artifact.Candidate.IndexEvidenceRefs {
		if _, found := resolved[reference.EvidenceID]; !found {
			return errors.New("candidate index evidence reference digest does not resolve")
		}
	}
	return nil
}

// ValidateFixedArtifacts binds fixed-fetch evidence to the already-validated
// complete receipt index. Unlike an item-local receipt rebuild, this performs no
// receipt-wide digest pass and makes no additional raw-byte copies.
func (resolver *PrivateEvidenceResolver) ValidateFixedArtifacts(artifacts []FixedArtifact) error {
	if resolver == nil || resolver.byID == nil {
		return errors.New("private evidence resolver is required")
	}
	for _, artifact := range artifacts {
		if resolver.sekaipediaEvidenceByID != nil {
			if err := resolver.validateSekaipediaFixedArtifact(artifact); err != nil {
				return err
			}
			continue
		}
		if resolver.candidateResolver == nil {
			return errors.New("private evidence resolver is required")
		}
		candidate := artifact.Candidate.SourceCandidate()
		candidate.IndexEvidence = artifact.Fixed.IndexEvidence
		if err := resolver.candidateResolver.ValidateResolvedCandidate(candidate); err != nil {
			return err
		}
	}
	return nil
}

// HydrateCandidate is the compatibility helper for a one-off resolution. Code
// processing multiple candidates or items should construct one
// PrivateEvidenceResolver and reuse it.
func (receipt PrivateEvidenceReceipt) HydrateCandidate(candidate CandidateIdentity) (lyricssource.Candidate, error) {
	resolver, err := NewPrivateEvidenceResolver(receipt)
	if err != nil {
		return lyricssource.Candidate{}, err
	}
	return resolver.HydrateCandidate(candidate)
}

func validatePrivateEvidenceReceiptForFixedArtifacts(
	receipt PrivateEvidenceReceipt,
	artifacts []FixedArtifact,
) error {
	// Fixed-artifact bundles already contain exact evidence retained by each
	// provider fetch. Validate the canonical receipt directly and bind shallow
	// envelope views to candidates instead of constructing another receipt-wide
	// resolver and hydrating another complete set of raw-byte copies.
	if err := ValidatePrivateEvidenceReceipt(receipt); err != nil {
		return err
	}
	resolved := make([][]lyricssource.IndexEvidence, len(artifacts))
	used := make(map[string]struct{}, len(receipt.IndexEvidence))
	for artifactIndex, artifact := range artifacts {
		candidate := artifact.Candidate.SourceCandidate()
		candidate.IndexEvidence = make([]lyricssource.IndexEvidence, len(candidate.IndexEvidenceRefs))
		for evidenceIndex, reference := range candidate.IndexEvidenceRefs {
			receiptIndex, found := privateEvidenceReceiptIndex(receipt.IndexEvidence, reference.EvidenceID)
			if !found {
				return errors.New("candidate reference is unresolved by the private evidence receipt")
			}
			evidence := receipt.IndexEvidence[receiptIndex]
			if evidence.SHA256 != reference.SHA256 || evidence.RawSHA256 != reference.SHA256 {
				return errors.New("candidate reference is unresolved by the private evidence receipt")
			}
			candidate.IndexEvidence[evidenceIndex] = evidence
			used[reference.EvidenceID] = struct{}{}
		}
		if err := lyricssource.ValidateCandidateIndexEvidence(candidate); err != nil {
			return err
		}
		resolved[artifactIndex] = candidate.IndexEvidence
	}
	if len(used) != len(receipt.IndexEvidence) {
		return errors.New("private evidence receipt contains orphan evidence")
	}
	for index, evidence := range resolved {
		if !samePrivateIndexEvidenceSlice(evidence, artifacts[index].Fixed.IndexEvidence) {
			return errors.New("fixed artifact evidence drifted from the preflight receipt")
		}
	}
	return nil
}

func privateEvidenceReceiptIndex(evidence []lyricssource.IndexEvidence, evidenceID string) (int, bool) {
	index := sort.Search(len(evidence), func(index int) bool {
		return evidence[index].EvidenceID >= evidenceID
	})
	return index, index < len(evidence) && evidence[index].EvidenceID == evidenceID
}

func ValidatePrivateEvidenceReceiptForCandidates(receipt PrivateEvidenceReceipt, candidates []CandidateIdentity) error {
	if privateEvidenceIsSekaipediaOnly(receipt.IndexEvidence) {
		// Validate and index the shared fixed authority once. The borrowed index is
		// confined to this call, so no receipt-sized defensive raw clone is needed.
		if err := ValidatePrivateEvidenceReceipt(receipt); err != nil {
			return err
		}
		resolver, err := newPrivateSekaipediaEvidenceResolver(receipt.IndexEvidence, false)
		if err != nil {
			return err
		}
		return resolver.ValidateCandidates(candidates)
	}

	// Validate the receipt envelope and digest here, then let lyricssource
	// validate each exact raw envelope once through a borrowed resolver. This
	// one-shot path never retains a receipt-sized defensive clone.
	if err := validatePrivateEvidenceReceipt(receipt, false, true); err != nil {
		return err
	}
	used := make(map[string]struct{}, len(receipt.IndexEvidence))
	sourceCandidates := make([]lyricssource.Candidate, len(candidates))
	for candidateIndex, candidate := range candidates {
		for _, reference := range candidate.IndexEvidenceRefs {
			receiptIndex, found := privateEvidenceReceiptIndex(receipt.IndexEvidence, reference.EvidenceID)
			if !found {
				return errors.New("candidate reference is unresolved by the private evidence receipt")
			}
			evidence := receipt.IndexEvidence[receiptIndex]
			if evidence.SHA256 != reference.SHA256 || evidence.RawSHA256 != reference.SHA256 {
				return errors.New("candidate reference is unresolved by the private evidence receipt")
			}
			used[reference.EvidenceID] = struct{}{}
		}
		sourceCandidates[candidateIndex] = candidate.SourceCandidate()
	}
	if len(used) != len(receipt.IndexEvidence) {
		return errors.New("private evidence receipt contains orphan evidence")
	}
	return lyricssource.ValidateCandidatesAgainstIndexEvidence(receipt.IndexEvidence, sourceCandidates)
}

// BuildDraftFromFixedArtifacts builds one manifest draft from a strict private
// plural-artifact handoff. Every documented fixed identity must resolve to one
// provider-specific candidate and one exact raw revision.
func BuildDraftFromFixedArtifacts(item PreflightItem, identity lyricscontract.CatalogIdentity, bundle FixedArtifactBundle) (Draft, error) {
	if item.PostFetchState == PostFetchStateVersionConflict || bundle.PostFetchState == PostFetchStateVersionConflict {
		return Draft{}, fmt.Errorf("%w: plural post-fetch version conflict cannot produce a manifest draft", ErrManifestRebuildRequired)
	}
	if bundle.PostFetchState != "" && bundle.PostFetchState != PostFetchStateComplete {
		return Draft{}, fmt.Errorf("%w: invalid fixed artifact post-fetch state", ErrManifestRebuildRequired)
	}
	if len(bundle.Artifacts) == 0 || len(bundle.Artifacts) > MaxFixedArtifactBundleArtifacts {
		return Draft{}, fmt.Errorf("%w: fixed artifact bundle has an invalid artifact count", ErrManifestRebuildRequired)
	}
	candidates := make([]CandidateIdentity, len(bundle.Artifacts))
	primaryIndex := -1
	totalRaw := 0
	for index, artifact := range bundle.Artifacts {
		if err := validateCandidate(artifact.Candidate); err != nil {
			return Draft{}, err
		}
		if err := validateFixedArtifact(artifact); err != nil {
			return Draft{}, err
		}
		if len(artifact.Fixed.Wikitext) > MaxFixedArtifactBundleRawBytes-totalRaw {
			return Draft{}, fmt.Errorf("%w: fixed artifact bundle exceeds its aggregate raw-byte limit", ErrManifestRebuildRequired)
		}
		totalRaw += len(artifact.Fixed.Wikitext)
		candidates[index] = artifact.Candidate
		if item.Candidate != nil && reflect.DeepEqual(artifact.Candidate, *item.Candidate) {
			if primaryIndex >= 0 {
				return Draft{}, fmt.Errorf("%w: preflight candidate resolves more than once", ErrManifestRebuildRequired)
			}
			primaryIndex = index
		}
	}
	artifactKeys, err := ResolveArtifactRenditionKeys(candidates)
	if err != nil {
		return Draft{}, err
	}
	byArtifactKey := make(map[string]FixedArtifact, len(bundle.Artifacts))
	byLogicalKey := make(map[string][]string, len(bundle.Artifacts))
	for index, artifact := range bundle.Artifacts {
		key := artifactKeys[index]
		byArtifactKey[key] = artifact
		byLogicalKey[artifact.Candidate.RenditionKey] = append(byLogicalKey[artifact.Candidate.RenditionKey], key)
	}
	if item.Candidate == nil || primaryIndex < 0 {
		return Draft{}, fmt.Errorf("%w: fixed artifact bundle does not contain the preflight candidate", ErrManifestRebuildRequired)
	}
	if len(item.FixedArtifactCandidates) > 0 && !reflect.DeepEqual(candidates, item.FixedArtifactCandidates) {
		return Draft{}, fmt.Errorf("%w: fixed artifact bundle drifted from the preflight artifact candidates", ErrManifestRebuildRequired)
	}
	var evidenceErr error
	if bundle.EvidenceResolver != nil {
		if bundle.EvidenceReceipt.SchemaVersion != 0 || bundle.EvidenceReceipt.IndexEvidence != nil ||
			bundle.EvidenceReceipt.ReceiptSHA256 != "" {
			evidenceErr = errors.New("fixed artifact bundle has ambiguous private evidence inputs")
		} else {
			evidenceErr = bundle.EvidenceResolver.ValidateFixedArtifacts(bundle.Artifacts)
		}
	} else {
		evidenceErr = validatePrivateEvidenceReceiptForFixedArtifacts(bundle.EvidenceReceipt, bundle.Artifacts)
	}
	if evidenceErr != nil {
		return Draft{}, fmt.Errorf("%w: %v", ErrManifestRebuildRequired, evidenceErr)
	}

	primary := bundle.Artifacts[primaryIndex]
	primaryArtifactKey := artifactKeys[primaryIndex]
	if err := lyricscontract.ValidateFixedPerformerSegmentationPolicy(identity, primary.Fixed); err != nil {
		return Draft{}, catalogPerformerPolicyError(item.MusicID, err)
	}
	if primary.Fixed.Document == nil {
		if len(bundle.Artifacts) != 1 {
			return Draft{}, fmt.Errorf("%w: plural fixed artifacts require an authoritative source document", ErrManifestRebuildRequired)
		}
		item.Candidate.ArtifactRenditionKey = primaryArtifactKey
		item.CompositionReason = bundleCompositionReason(item, bundle, primary.Candidate.VersionReason)
		return BuildDraft(item, identity, primary.Fixed)
	}
	template := *primary.Fixed.Document
	if err := model.ValidateLyricsSourceDocument(template); err != nil {
		return Draft{}, fmt.Errorf("music %d fixed source document: %w", item.MusicID, err)
	}
	if !reflect.DeepEqual(primary.Fixed.FixedIdentities, template.FixedIdentities) {
		return Draft{}, fmt.Errorf("%w: fixed source identities drifted from the source document", ErrManifestRebuildRequired)
	}
	compositionReason := bundleCompositionReason(item, bundle, template.ReasonCode)
	if !model.IsValidLyricsSourceVersionReasonCode(compositionReason) || compositionReason == model.LyricsSourceVersionReasonVersionConflict {
		return Draft{}, fmt.Errorf("%w: fixed artifact bundle has no acceptable final composition reason", ErrManifestRebuildRequired)
	}

	components, err := resolveFixedArtifactComponents(template, bundle.Components, byArtifactKey, byLogicalKey)
	if err != nil {
		return Draft{}, err
	}
	if components.FullText != primaryArtifactKey {
		return Draft{}, fmt.Errorf("%w: preflight candidate is not the authoritative Full artifact", ErrManifestRebuildRequired)
	}
	finalDocument := template
	finalDocument.ReasonCode = compositionReason
	finalDocument.FixedIdentities = make([]model.LyricsSourceFixedIdentity, len(bundle.Artifacts))
	for index, artifact := range bundle.Artifacts {
		finalDocument.FixedIdentities[index] = fixedIdentityFromArtifactWithKey(artifact, artifactKeys[index])
	}
	sort.Slice(finalDocument.FixedIdentities, func(left, right int) bool {
		return finalDocument.FixedIdentities[left].RenditionKey < finalDocument.FixedIdentities[right].RenditionKey
	})
	finalDocument.Provenance.FullText = model.LyricsSourceComponentRef{RenditionKey: components.FullText}
	finalDocument.Provenance.GameText = optionalComponentRef(components.GameText)
	finalDocument.Provenance.VersionEvidence = model.LyricsSourceComponentRef{RenditionKey: components.VersionEvidence}
	finalDocument.Provenance.PerformerSegmentation = optionalComponentRef(components.PerformerSegmentation)
	finalDocument.Provenance.GameProjection = optionalComponentRef(components.GameProjection)
	finalDocument.Provenance.Ruby = optionalComponentRef(components.Ruby)
	if err := model.ValidateLyricsSourceDocument(finalDocument); err != nil {
		return Draft{}, fmt.Errorf("%w: final composed source document: %v", ErrManifestRebuildRequired, err)
	}
	if err := requireEveryFixedArtifactContributes(finalDocument); err != nil {
		return Draft{}, err
	}
	fullIdentity, found := fixedIdentityByRendition(finalDocument.FixedIdentities, components.FullText)
	if !found {
		return Draft{}, fmt.Errorf("%w: fixed source document has no Full identity", ErrManifestRebuildRequired)
	}
	itemForPrimary := item
	primaryCandidate := *item.Candidate
	primaryCandidate.ArtifactRenditionKey = primaryArtifactKey
	itemForPrimary.Candidate = &primaryCandidate
	itemForPrimary.FixedArtifactCandidates = append([]CandidateIdentity{}, item.FixedArtifactCandidates...)
	if len(itemForPrimary.FixedArtifactCandidates) > 0 {
		itemForPrimary.FixedArtifactCandidates[primaryIndex].ArtifactRenditionKey = primaryArtifactKey
	}
	itemForPrimary.CompositionReason = compositionReason
	draft, err := BuildDraftWithProvenance(itemForPrimary, identity, primary.Fixed, fullIdentity, compositionReason, finalDocument.GameProjection)
	if err != nil {
		return Draft{}, err
	}
	return rebindDraftDocumentFromFixedArtifacts(draft, finalDocument, byArtifactKey)
}

func bundleCompositionReason(item PreflightItem, bundle FixedArtifactBundle, fallback model.LyricsSourceVersionReasonCode) model.LyricsSourceVersionReasonCode {
	if bundle.CompositionReason != "" {
		if item.CompositionReason != "" && item.CompositionReason != bundle.CompositionReason {
			return model.LyricsSourceVersionReasonVersionConflict
		}
		return bundle.CompositionReason
	}
	if item.CompositionReason != "" {
		return item.CompositionReason
	}
	return fallback
}

func resolveFixedArtifactComponents(
	document model.LyricsSourceDocument,
	explicit FixedArtifactComponentSelection,
	byArtifactKey map[string]FixedArtifact,
	byLogicalKey map[string][]string,
) (FixedArtifactComponentSelection, error) {
	resolve := func(component, templateKey, selected string, required bool) (string, error) {
		if !required {
			if selected != "" {
				return "", fmt.Errorf("%w: component %s selects an artifact without component data", ErrManifestRebuildRequired, component)
			}
			return "", nil
		}
		if selected != "" {
			if _, exists := byArtifactKey[selected]; !exists {
				return "", fmt.Errorf("%w: component %s references unknown artifact %q", ErrManifestRebuildRequired, component, selected)
			}
			return selected, nil
		}
		if _, exists := byArtifactKey[templateKey]; exists {
			return templateKey, nil
		}
		matches := byLogicalKey[templateKey]
		if len(matches) != 1 {
			return "", fmt.Errorf("%w: component %s requires an explicit artifact selection for logical rendition %q", ErrManifestRebuildRequired, component, templateKey)
		}
		return matches[0], nil
	}
	var result FixedArtifactComponentSelection
	var err error
	if result.FullText, err = resolve("full_text", document.Provenance.FullText.RenditionKey, explicit.FullText, true); err != nil {
		return result, err
	}
	if document.Provenance.GameText != nil {
		result.GameText, err = resolve("game_text", document.Provenance.GameText.RenditionKey, explicit.GameText, true)
	} else {
		_, err = resolve("game_text", "", explicit.GameText, false)
	}
	if err != nil {
		return result, err
	}
	if result.VersionEvidence, err = resolve("version_evidence", document.Provenance.VersionEvidence.RenditionKey, explicit.VersionEvidence, true); err != nil {
		return result, err
	}
	if document.Provenance.PerformerSegmentation != nil {
		result.PerformerSegmentation, err = resolve("performer_segmentation", document.Provenance.PerformerSegmentation.RenditionKey, explicit.PerformerSegmentation, true)
	} else {
		_, err = resolve("performer_segmentation", "", explicit.PerformerSegmentation, false)
	}
	if err != nil {
		return result, err
	}
	if document.Provenance.GameProjection != nil {
		result.GameProjection, err = resolve("game_projection", document.Provenance.GameProjection.RenditionKey, explicit.GameProjection, true)
	} else {
		_, err = resolve("game_projection", "", explicit.GameProjection, false)
	}
	if err != nil {
		return result, err
	}
	if document.Provenance.Ruby != nil {
		result.Ruby, err = resolve("ruby", document.Provenance.Ruby.RenditionKey, explicit.Ruby, true)
	} else {
		_, err = resolve("ruby", "", explicit.Ruby, false)
	}
	return result, err
}

func optionalComponentRef(renditionKey string) *model.LyricsSourceComponentRef {
	if renditionKey == "" {
		return nil
	}
	return &model.LyricsSourceComponentRef{RenditionKey: renditionKey}
}

func requireEveryFixedArtifactContributes(document model.LyricsSourceDocument) error {
	referenced := map[string]struct{}{
		document.Provenance.FullText.RenditionKey:        {},
		document.Provenance.VersionEvidence.RenditionKey: {},
	}
	for _, reference := range []*model.LyricsSourceComponentRef{
		document.Provenance.GameText, document.Provenance.PerformerSegmentation,
		document.Provenance.GameProjection, document.Provenance.Ruby,
	} {
		if reference != nil {
			referenced[reference.RenditionKey] = struct{}{}
		}
	}
	for _, alternate := range document.AlternateVocals {
		referenced[alternate.Provenance.VersionEvidence.RenditionKey] = struct{}{}
		for _, reference := range []*model.LyricsSourceComponentRef{
			alternate.Provenance.FullText, alternate.Provenance.GameText, alternate.Provenance.GameProjection,
		} {
			if reference != nil {
				referenced[reference.RenditionKey] = struct{}{}
			}
		}
	}
	for _, identity := range document.FixedIdentities {
		if _, exists := referenced[identity.RenditionKey]; !exists {
			return fmt.Errorf("%w: artifact %q has no component contribution", ErrManifestRebuildRequired, identity.RenditionKey)
		}
	}
	return nil
}

func validateFixedArtifact(artifact FixedArtifact) error {
	candidate := artifact.Candidate
	fixed := artifact.Fixed
	if fixed.Provider != candidate.Provider || fixed.Origin != candidate.Origin || fixed.PageID != candidate.PageID ||
		fixed.RevisionID != candidate.RevisionID || canonicalFixedRevisionTimestamp(fixed) != candidate.RevisionTimestamp ||
		fixed.SHA1 != candidate.SHA1 || fixed.PageTitle != candidate.Title || fixed.CanonicalURL != candidate.CanonicalURL ||
		!equalStrings(fixed.Categories, candidate.Categories) ||
		fixed.Section != candidate.Section || fixed.RenditionKey != candidate.RenditionKey ||
		fixed.VersionReason != candidate.VersionReason || !equalIndexEvidenceRefs(fixed.IndexEvidenceRefs, candidate.IndexEvidenceRefs) {
		return fmt.Errorf("%w: exact fixed artifact identity drifted from its candidate", ErrManifestRebuildRequired)
	}
	if len(fixed.Wikitext) == 0 || len(fixed.Wikitext) > 2<<20 || !utf8.Valid(fixed.Wikitext) {
		return fmt.Errorf("%w: exact fixed artifact raw bytes are invalid", ErrManifestRebuildRequired)
	}
	if err := validateFixedRevisionContentBinding(candidate, fixed); err != nil {
		return fmt.Errorf("%w: exact fixed artifact %v", ErrManifestRebuildRequired, err)
	}
	fetchedAt := fixed.FetchedAt.UTC().Format(time.RFC3339Nano)
	if fixed.FetchedAt.IsZero() || fetchedAt == "0001-01-01T00:00:00Z" {
		return fmt.Errorf("%w: exact fixed artifact fetchedAt is invalid", ErrManifestRebuildRequired)
	}
	return nil
}

func fixedIdentityFromArtifact(artifact FixedArtifact) model.LyricsSourceFixedIdentity {
	artifactKeys, err := ResolveArtifactRenditionKeys([]CandidateIdentity{artifact.Candidate})
	if err != nil {
		return model.LyricsSourceFixedIdentity{}
	}
	return fixedIdentityFromArtifactWithKey(artifact, artifactKeys[0])
}

func fixedIdentityFromArtifactWithKey(artifact FixedArtifact, artifactRenditionKey string) model.LyricsSourceFixedIdentity {
	candidate := artifact.Candidate
	fixed := artifact.Fixed
	return model.LyricsSourceFixedIdentity{
		Provider: candidate.Provider, Origin: candidate.Origin, PageID: fixed.PageID, RevisionID: fixed.RevisionID,
		SHA1: fixed.SHA1, Title: fixed.PageTitle, CanonicalURL: fixed.CanonicalURL,
		RevisionTimestamp: candidate.RevisionTimestamp,
		FetchedAt:         fixed.FetchedAt.UTC().Format(time.RFC3339Nano), Categories: append([]string{}, fixed.Categories...),
		Section: candidate.Section, RenditionKey: artifactRenditionKey,
		CompositionRenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
	}
}

func fixedIdentityByRendition(identities []model.LyricsSourceFixedIdentity, renditionKey string) (model.LyricsSourceFixedIdentity, bool) {
	for _, identity := range identities {
		if identity.RenditionKey == renditionKey {
			return identity, true
		}
	}
	return model.LyricsSourceFixedIdentity{}, false
}

func rebindDraftDocumentFromFixedArtifacts(draft Draft, document model.LyricsSourceDocument, byRendition map[string]FixedArtifact) (Draft, error) {
	var err error
	document, err = canonicalizeStagedDocument(document)
	if err != nil {
		return Draft{}, err
	}
	draft = rebindDraftExtractionProjection(draft, document)
	artifacts := make([]Artifact, len(document.FixedIdentities))
	for index, identity := range document.FixedIdentities {
		fixedArtifact, found := byRendition[identity.RenditionKey]
		if !found {
			return Draft{}, fmt.Errorf("%w: missing exact raw bytes for rendition %q", ErrManifestRebuildRequired, identity.RenditionKey)
		}
		expectedIdentity := fixedIdentityFromArtifactWithKey(fixedArtifact, identity.RenditionKey)
		if !reflect.DeepEqual(identity, expectedIdentity) {
			return Draft{}, fmt.Errorf("%w: rebound identity drifted from exact fixed artifact %q", ErrManifestRebuildRequired, identity.RenditionKey)
		}
		rawDigest := sha256.Sum256(fixedArtifact.Fixed.Wikitext)
		artifact := Artifact{
			Identity: identity, RawWikitextByteCount: len(fixedArtifact.Fixed.Wikitext),
			RawWikitextSHA256: hex.EncodeToString(rawDigest[:]),
		}
		artifactSHA, err := stagedArtifactDigest(artifact)
		if err != nil {
			return Draft{}, err
		}
		artifact.ArtifactSHA256 = artifactSHA
		artifacts[index] = artifact
	}
	documentSHA, err := lyricsSourceDocumentDigest(document)
	if err != nil {
		return Draft{}, err
	}
	draft.Document = document
	draft.DocumentSHA256 = documentSHA
	draft.Artifacts = artifacts
	draft.DraftSHA256 = ""
	draftSHA, err := draftDigest(draft)
	if err != nil {
		return Draft{}, err
	}
	draft.DraftSHA256 = draftSHA
	if err := ValidateDraft(draft); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

func samePrivateIndexEvidenceSlice(left, right []lyricssource.IndexEvidence) bool {
	if len(left) != len(right) || left == nil != (right == nil) {
		return false
	}
	for index := range left {
		if !samePrivateIndexEvidence(left[index], right[index]) {
			return false
		}
	}
	return true
}
