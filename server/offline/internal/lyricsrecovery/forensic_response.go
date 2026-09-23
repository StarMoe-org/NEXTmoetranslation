package lyricsrecovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
)

const (
	ForensicResponseStoreSuffix     = ".forensic-responses"
	forensicResponseBlobDirectory   = "blobs"
	forensicResponseRecordDirectory = "manifests"

	forensicResponseManifestSchema = "lyrics-recovery-forensic-response/v1"
	forensicResponseRawBlobSchema  = "lyrics-recovery-forensic-raw/v1"

	MaxForensicResponseManifestBytes = 64 << 10
	MaxForensicResponseRawBlobBytes  = 3 << 20
	maxForensicResponseHeaders       = 1024
	maxForensicResponseHeaderBytes   = 256 << 10
	maxForensicResponseStatusBytes   = 256
)

type ForensicResponseRef struct {
	ResponseID        string `json:"responseId"`
	RawResponseSHA256 string `json:"rawResponseSha256"`
	StatusCode        int    `json:"statusCode"`
}

type ForensicResponse struct {
	ForensicResponseRef
	Provider            model.LyricsSourceProvider
	Action              string
	CanonicalRequestURL string
	FetchedAt           string
	Status              string
	Header              http.Header
	Raw                 []byte
}

type ForensicResponseStore struct {
	root string
	mu   sync.Mutex
}

type forensicResponseHeader struct {
	Name   []byte   `json:"name"`
	Values [][]byte `json:"values"`
}

type forensicResponseRawBinding struct {
	SHA256    string `json:"sha256"`
	ByteCount int    `json:"byteCount"`
}

type forensicResponseManifest struct {
	SchemaVersion       string                     `json:"schemaVersion"`
	Provider            model.LyricsSourceProvider `json:"provider"`
	Action              string                     `json:"action"`
	CanonicalRequestURL string                     `json:"canonicalRequestUrl"`
	FetchedAt           string                     `json:"fetchedAt"`
	StatusCode          int                        `json:"statusCode"`
	Status              []byte                     `json:"status"`
	Headers             []forensicResponseHeader   `json:"headers"`
	RawResponse         forensicResponseRawBinding `json:"rawResponse"`
}

type forensicResponseRawBlob struct {
	SchemaVersion string `json:"schemaVersion"`
	SHA256        string `json:"sha256"`
	ByteCount     int    `json:"byteCount"`
	Body          []byte `json:"body"`
}

func ForensicResponseStorePath(ledgerRoot string) (string, error) {
	if ledgerRoot == "" || strings.TrimSpace(ledgerRoot) != ledgerRoot || !filepath.IsAbs(ledgerRoot) ||
		filepath.Clean(ledgerRoot) != ledgerRoot || ledgerRoot == string(os.PathSeparator) {
		return "", errors.New("forensic response ledger root is invalid")
	}
	return ledgerRoot + ForensicResponseStoreSuffix, nil
}

func OpenForensicResponseStore(path string) (*ForensicResponseStore, error) {
	if err := validateForensicResponseStorePath(path); err != nil {
		return nil, err
	}
	for _, directory := range []string{path, filepath.Join(path, forensicResponseBlobDirectory), filepath.Join(path, forensicResponseRecordDirectory)} {
		opened, _, err := openStablePrivateDirectory(directory)
		if err != nil {
			return nil, err
		}
		if err := opened.Close(); err != nil {
			return nil, err
		}
	}
	return &ForensicResponseStore{root: path}, nil
}

func openOrCreateForensicResponseStore(path string) (*ForensicResponseStore, error) {
	if err := validateForensicResponseStorePath(path); err != nil {
		return nil, err
	}
	for _, directory := range []string{path, filepath.Join(path, forensicResponseBlobDirectory), filepath.Join(path, forensicResponseRecordDirectory)} {
		if err := ensureForensicResponseDirectory(directory); err != nil {
			return nil, err
		}
	}
	return OpenForensicResponseStore(path)
}

func validateForensicResponseStorePath(path string) error {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		path == string(os.PathSeparator) || !strings.HasSuffix(path, ForensicResponseStoreSuffix) {
		return errors.New("forensic response store path is invalid")
	}
	return nil
}

func ensureForensicResponseDirectory(path string) error {
	err := lyricsoutcomeartifact.CreatePrivateDirectory(path)
	if err == nil {
		return nil
	}
	if !errors.Is(err, lyricsoutcomeartifact.ErrAlreadyPublished) {
		return err
	}
	opened, _, openErr := openStablePrivateDirectory(path)
	if openErr != nil {
		return openErr
	}
	return opened.Close()
}

func (store *ForensicResponseStore) Commit(
	ctx context.Context,
	provider model.LyricsSourceProvider,
	response lyricssource.RecoveryHTTPResponse,
) (ForensicResponseRef, error) {
	if ctx == nil || store == nil {
		return ForensicResponseRef{}, errors.New("forensic response commit input is invalid")
	}
	if err := ctx.Err(); err != nil {
		return ForensicResponseRef{}, err
	}
	manifest, manifestBody, rawBlob, rawBlobBody, ref, err := forensicResponseArtifacts(provider, response)
	if err != nil {
		return ForensicResponseRef{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := OpenForensicResponseStore(store.root); err != nil {
		return ForensicResponseRef{}, err
	}
	if err := publishForensicResponseFile(
		filepath.Join(store.root, forensicResponseBlobDirectory, manifest.RawResponse.SHA256+".json"),
		rawBlobBody,
		func(body []byte) error {
			decoded, decodeErr := decodeForensicResponseRawBlob(body)
			if decodeErr != nil || !equalForensicResponseRawBlob(decoded, rawBlob) {
				return errors.New("forensic raw response blob is invalid")
			}
			return nil
		},
		MaxForensicResponseRawBlobBytes,
	); err != nil {
		return ForensicResponseRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return ForensicResponseRef{}, err
	}
	if err := publishForensicResponseFile(
		filepath.Join(store.root, forensicResponseRecordDirectory, ref.ResponseID+".json"),
		manifestBody,
		func(body []byte) error {
			decoded, decodeErr := decodeForensicResponseManifest(body)
			if decodeErr != nil || !equalForensicResponseManifest(decoded, manifest) {
				return errors.New("forensic response manifest is invalid")
			}
			return nil
		},
		MaxForensicResponseManifestBytes,
	); err != nil {
		return ForensicResponseRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return ForensicResponseRef{}, err
	}
	return ref, nil
}

func (store *ForensicResponseStore) Replay(
	ctx context.Context,
	responseID string,
) (ForensicResponse, error) {
	if ctx == nil || store == nil || !canonicalLowerSHA256(responseID) {
		return ForensicResponse{}, errors.New("forensic response replay identity is invalid")
	}
	if err := ctx.Err(); err != nil {
		return ForensicResponse{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := OpenForensicResponseStore(store.root); err != nil {
		return ForensicResponse{}, err
	}
	manifestBody, err := readPrivateFile(
		filepath.Join(store.root, forensicResponseRecordDirectory, responseID+".json"),
		MaxForensicResponseManifestBytes,
		1,
	)
	if err != nil {
		return ForensicResponse{}, err
	}
	if digestHex(manifestBody) != responseID {
		return ForensicResponse{}, errors.New("forensic response manifest does not match its content address")
	}
	manifest, err := decodeForensicResponseManifest(manifestBody)
	if err != nil {
		return ForensicResponse{}, err
	}
	rawBlobBody, err := readPrivateFile(
		filepath.Join(store.root, forensicResponseBlobDirectory, manifest.RawResponse.SHA256+".json"),
		MaxForensicResponseRawBlobBytes,
		1,
	)
	if err != nil {
		return ForensicResponse{}, err
	}
	rawBlob, err := decodeForensicResponseRawBlob(rawBlobBody)
	if err != nil || rawBlob.SHA256 != manifest.RawResponse.SHA256 || rawBlob.ByteCount != manifest.RawResponse.ByteCount {
		return ForensicResponse{}, errors.New("forensic response raw binding is invalid")
	}
	if err := ctx.Err(); err != nil {
		return ForensicResponse{}, err
	}
	return forensicResponseFromArtifacts(responseID, manifest, rawBlob), nil
}

func forensicResponseArtifacts(
	provider model.LyricsSourceProvider,
	response lyricssource.RecoveryHTTPResponse,
) (
	forensicResponseManifest,
	[]byte,
	forensicResponseRawBlob,
	[]byte,
	ForensicResponseRef,
	error,
) {
	if !model.IsValidLyricsSourceProvider(provider) || response.StatusCode < 100 || response.StatusCode > 599 ||
		response.FetchedAt.IsZero() || response.FetchedAt.Location() != time.UTC || len(response.Raw) > lyricssource.MaxIndexEvidenceRawBytes ||
		len(response.Status) > maxForensicResponseStatusBytes ||
		lyricssource.ValidateRecoveryHTTPRequestBoundary(provider, response.Action, response.CanonicalRequestURL) != nil {
		return forensicResponseManifest{}, nil, forensicResponseRawBlob{}, nil, ForensicResponseRef{},
			errors.New("forensic response boundary is invalid")
	}
	fetchedAt := response.FetchedAt.UTC().Format(time.RFC3339Nano)
	if fetchedAt != response.FetchedAt.Format(time.RFC3339Nano) {
		return forensicResponseManifest{}, nil, forensicResponseRawBlob{}, nil, ForensicResponseRef{},
			errors.New("forensic response fetchedAt is not canonical UTC")
	}
	headers, err := forensicResponseHeaders(response.Header)
	if err != nil {
		return forensicResponseManifest{}, nil, forensicResponseRawBlob{}, nil, ForensicResponseRef{}, err
	}
	raw := append([]byte{}, response.Raw...)
	rawSHA256 := digestHex(raw)
	rawBlob := forensicResponseRawBlob{
		SchemaVersion: forensicResponseRawBlobSchema,
		SHA256:        rawSHA256, ByteCount: len(raw), Body: raw,
	}
	rawBlobBody, err := json.Marshal(rawBlob)
	if err != nil || len(rawBlobBody) == 0 || len(rawBlobBody) > MaxForensicResponseRawBlobBytes {
		return forensicResponseManifest{}, nil, forensicResponseRawBlob{}, nil, ForensicResponseRef{},
			errors.New("forensic raw response blob exceeds its boundary")
	}
	manifest := forensicResponseManifest{
		SchemaVersion: forensicResponseManifestSchema,
		Provider:      provider, Action: response.Action, CanonicalRequestURL: response.CanonicalRequestURL,
		FetchedAt: fetchedAt, StatusCode: response.StatusCode, Status: []byte(response.Status),
		Headers: headers, RawResponse: forensicResponseRawBinding{SHA256: rawSHA256, ByteCount: len(raw)},
	}
	if err := validateForensicResponseManifest(manifest); err != nil {
		return forensicResponseManifest{}, nil, forensicResponseRawBlob{}, nil, ForensicResponseRef{}, err
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil || len(manifestBody) == 0 || len(manifestBody) > MaxForensicResponseManifestBytes {
		return forensicResponseManifest{}, nil, forensicResponseRawBlob{}, nil, ForensicResponseRef{},
			errors.New("forensic response manifest exceeds its boundary")
	}
	ref := ForensicResponseRef{
		ResponseID: digestHex(manifestBody), RawResponseSHA256: rawSHA256, StatusCode: response.StatusCode,
	}
	return manifest, manifestBody, rawBlob, rawBlobBody, ref, nil
}

func forensicResponseReference(
	provider model.LyricsSourceProvider,
	response lyricssource.RecoveryHTTPResponse,
) (ForensicResponseRef, error) {
	_, _, _, _, ref, err := forensicResponseArtifacts(provider, response)
	return ref, err
}

func forensicResponseHeaders(header http.Header) ([]forensicResponseHeader, error) {
	if header == nil {
		return nil, nil
	}
	result := make([]forensicResponseHeader, 0, len(header))
	totalBytes := 0
	for name, values := range header {
		if name == "" || strings.ContainsAny(name, "\x00\r\n") {
			return nil, errors.New("forensic response header name is invalid")
		}
		item := forensicResponseHeader{Name: []byte(name)}
		totalBytes += len(name)
		if values != nil {
			item.Values = make([][]byte, len(values))
			for index, value := range values {
				if strings.ContainsAny(value, "\x00\r\n") {
					return nil, errors.New("forensic response header value is invalid")
				}
				item.Values[index] = []byte(value)
				totalBytes += len(value)
			}
		}
		result = append(result, item)
	}
	if len(result) > maxForensicResponseHeaders || totalBytes > maxForensicResponseHeaderBytes {
		return nil, errors.New("forensic response headers exceed their boundary")
	}
	sort.Slice(result, func(left, right int) bool { return bytes.Compare(result[left].Name, result[right].Name) < 0 })
	return result, nil
}

func validateForensicResponseManifest(manifest forensicResponseManifest) error {
	if manifest.SchemaVersion != forensicResponseManifestSchema || !model.IsValidLyricsSourceProvider(manifest.Provider) ||
		manifest.StatusCode < 100 || manifest.StatusCode > 599 || len(manifest.Status) > maxForensicResponseStatusBytes ||
		!canonicalLowerSHA256(manifest.RawResponse.SHA256) || manifest.RawResponse.ByteCount < 0 ||
		manifest.RawResponse.ByteCount > lyricssource.MaxIndexEvidenceRawBytes ||
		lyricssource.ValidateRecoveryHTTPRequestBoundary(manifest.Provider, manifest.Action, manifest.CanonicalRequestURL) != nil {
		return errors.New("forensic response manifest identity is invalid")
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, manifest.FetchedAt)
	if err != nil || fetchedAt.Location() != time.UTC || fetchedAt.UTC().Format(time.RFC3339Nano) != manifest.FetchedAt {
		return errors.New("forensic response manifest fetchedAt is invalid")
	}
	totalBytes := 0
	for index, header := range manifest.Headers {
		if len(header.Name) == 0 || bytes.ContainsAny(header.Name, "\x00\r\n") ||
			index > 0 && bytes.Compare(manifest.Headers[index-1].Name, header.Name) >= 0 {
			return errors.New("forensic response manifest headers are invalid")
		}
		totalBytes += len(header.Name)
		for _, value := range header.Values {
			if bytes.ContainsAny(value, "\x00\r\n") {
				return errors.New("forensic response manifest header value is invalid")
			}
			totalBytes += len(value)
		}
	}
	if len(manifest.Headers) > maxForensicResponseHeaders || totalBytes > maxForensicResponseHeaderBytes {
		return errors.New("forensic response manifest headers exceed their boundary")
	}
	return nil
}

func decodeForensicResponseManifest(body []byte) (forensicResponseManifest, error) {
	var manifest forensicResponseManifest
	if len(body) == 0 || len(body) > MaxForensicResponseManifestBytes || inspectSongResultJSON(body) != nil {
		return manifest, errors.New("forensic response manifest bytes are invalid")
	}
	if err := decodeForensicCanonicalJSON(body, &manifest); err != nil {
		return manifest, err
	}
	if err := validateForensicResponseManifest(manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func decodeForensicResponseRawBlob(body []byte) (forensicResponseRawBlob, error) {
	var blob forensicResponseRawBlob
	if len(body) == 0 || len(body) > MaxForensicResponseRawBlobBytes || inspectSongResultJSON(body) != nil {
		return blob, errors.New("forensic raw response blob bytes are invalid")
	}
	if err := decodeForensicCanonicalJSON(body, &blob); err != nil {
		return blob, err
	}
	if blob.SchemaVersion != forensicResponseRawBlobSchema || !canonicalLowerSHA256(blob.SHA256) ||
		blob.ByteCount < 0 || blob.ByteCount > lyricssource.MaxIndexEvidenceRawBytes || len(blob.Body) != blob.ByteCount ||
		digestHex(blob.Body) != blob.SHA256 {
		return blob, errors.New("forensic raw response blob binding is invalid")
	}
	return blob, nil
}

func decodeForensicCanonicalJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("forensic response JSON contains trailing data")
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(body, canonical) {
		return errors.New("forensic response JSON is not canonical")
	}
	return nil
}

func publishForensicResponseFile(
	path string,
	body []byte,
	validate func([]byte) error,
	maximum int,
) error {
	err := publishPrivateFile(path, body, validate)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrAlreadyPublished) {
		return err
	}
	existing, readErr := readPrivateFile(path, maximum, 1)
	if readErr != nil || !bytes.Equal(existing, body) || validate(existing) != nil {
		return errors.New("existing forensic response artifact conflicts with its content address")
	}
	return nil
}

func forensicResponseFromArtifacts(
	responseID string,
	manifest forensicResponseManifest,
	blob forensicResponseRawBlob,
) ForensicResponse {
	var header http.Header
	if manifest.Headers != nil {
		header = make(http.Header, len(manifest.Headers))
		for _, item := range manifest.Headers {
			name := string(item.Name)
			if item.Values == nil {
				header[name] = nil
				continue
			}
			values := make([]string, len(item.Values))
			for index, value := range item.Values {
				values[index] = string(value)
			}
			header[name] = values
		}
	}
	return ForensicResponse{
		ForensicResponseRef: ForensicResponseRef{
			ResponseID: responseID, RawResponseSHA256: manifest.RawResponse.SHA256, StatusCode: manifest.StatusCode,
		},
		Provider: manifest.Provider, Action: manifest.Action, CanonicalRequestURL: manifest.CanonicalRequestURL,
		FetchedAt: manifest.FetchedAt, Status: string(manifest.Status), Header: header,
		Raw: append([]byte{}, blob.Body...),
	}
}

func equalForensicResponseManifest(left, right forensicResponseManifest) bool {
	leftBody, leftErr := json.Marshal(left)
	rightBody, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBody, rightBody)
}

func equalForensicResponseRawBlob(left, right forensicResponseRawBlob) bool {
	leftBody, leftErr := json.Marshal(left)
	rightBody, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBody, rightBody)
}

func digestHex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
