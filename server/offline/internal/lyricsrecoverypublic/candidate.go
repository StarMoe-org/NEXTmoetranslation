package lyricsrecoverypublic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

const (
	ManifestSchemaVersion = 1
	ManifestKind          = "lyrics-recovery-public-candidate-manifest-v1"
	ReceiptSchemaVersion  = 1
	ReceiptKind           = "lyrics-recovery-public-candidate-receipt-v1"
	IndexPath             = "index.json"
	ManifestPath          = "manifest.json"
	ReceiptPath           = "receipt.json"
	maxCandidateBytes     = 256 << 20
	maxManifestBytes      = 4 << 20
	maxReceiptBytes       = 64 << 10
)

var canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

var publishBundleBeforeRenameHook func(string) error

type StateCount struct {
	State store.PublicLyricsAvailabilityState `json:"state"`
	Count int                                 `json:"count"`
}

type Asset struct {
	Path    string `json:"path"`
	MusicID int    `json:"musicId,omitempty"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}

type Manifest struct {
	SchemaVersion       int          `json:"schemaVersion"`
	Kind                string       `json:"kind"`
	PublicLyricsVersion int          `json:"publicLyricsVersion"`
	BatchSHA256         string       `json:"batchSha256"`
	RootSHA256          string       `json:"rootSha256"`
	CatalogCount        int          `json:"catalogCount"`
	DetailCount         int          `json:"detailCount"`
	States              []StateCount `json:"states"`
	Index               Asset        `json:"index"`
	Details             []Asset      `json:"details"`
	ContentSHA256       string       `json:"contentSha256"`
}

type Receipt struct {
	SchemaVersion  int    `json:"schemaVersion"`
	Kind           string `json:"kind"`
	BatchSHA256    string `json:"batchSha256"`
	RootSHA256     string `json:"rootSha256"`
	DatabaseSHA256 string `json:"databaseSha256"`
	DatabaseBytes  int64  `json:"databaseBytes"`
	ManifestSHA256 string `json:"manifestSha256"`
	ManifestBytes  int64  `json:"manifestBytes"`
	ContentSHA256  string `json:"contentSha256"`
	CatalogCount   int    `json:"catalogCount"`
	DetailCount    int    `json:"detailCount"`
	AssetCount     int    `json:"assetCount"`
	ReceiptSHA256  string `json:"receiptSha256"`
}

type Bundle struct {
	IndexBody    []byte
	DetailBodies map[int][]byte
	Manifest     Manifest
	ManifestBody []byte
	Receipt      Receipt
	ReceiptBody  []byte
}

func BuildBundle(candidate store.RecoveryPublicLyricsCandidate, databaseSHA256 string, databaseBytes int64) (Bundle, error) {
	if !canonicalSHA256.MatchString(candidate.BatchSHA256) || !canonicalSHA256.MatchString(candidate.RootSHA256) ||
		!canonicalSHA256.MatchString(databaseSHA256) || databaseBytes <= 0 {
		return Bundle{}, errors.New("public candidate exact source identities are invalid")
	}
	if candidate.Index.Version != 2 || len(candidate.Index.Songs) == 0 ||
		len(candidate.Index.Songs) > model.PublicLyricsMaxIndexEntries || candidate.Details == nil {
		return Bundle{}, errors.New("public candidate index envelope is invalid")
	}

	indexBody, err := marshalCanonical(candidate.Index)
	if err != nil {
		return Bundle{}, err
	}
	indexAsset := newAsset(IndexPath, 0, indexBody)
	detailBodies := make(map[int][]byte, len(candidate.Details))
	detailAssets := make([]Asset, 0, len(candidate.Details))
	stateCounts := make(map[store.PublicLyricsAvailabilityState]int)
	lastMusicID := 0
	for _, song := range candidate.Index.Songs {
		if song.MusicID <= lastMusicID || song.Revision <= 0 || song.UpdatedAt == "" || song.State == "" ||
			song.Title.Japanese == "" {
			return Bundle{}, errors.New("public candidate index songs are invalid or not strictly ordered")
		}
		lastMusicID = song.MusicID
		stateCounts[song.State]++
		detail, hasDetail := candidate.Details[song.MusicID]
		expectsDetail := song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly
		if hasDetail != expectsDetail {
			return Bundle{}, fmt.Errorf("public candidate music %d detail presence does not match state", song.MusicID)
		}
		if !hasDetail {
			continue
		}
		if detail.Version != 2 || detail.MusicID != song.MusicID || detail.Revision != song.Revision ||
			detail.UpdatedAt != song.UpdatedAt || detail.State != song.State {
			return Bundle{}, fmt.Errorf("public candidate music %d index and detail identities differ", song.MusicID)
		}
		body, err := marshalCanonical(detail)
		if err != nil {
			return Bundle{}, fmt.Errorf("marshal public candidate music %d: %w", song.MusicID, err)
		}
		path := detailPath(song.MusicID)
		detailBodies[song.MusicID] = body
		detailAssets = append(detailAssets, newAsset(path, song.MusicID, body))
	}
	if len(detailBodies) != len(candidate.Details) {
		return Bundle{}, errors.New("public candidate contains details outside its exact index")
	}

	states, err := orderedStateCounts(stateCounts, len(candidate.Index.Songs))
	if err != nil {
		return Bundle{}, err
	}
	assets := make([]Asset, 0, len(detailAssets)+1)
	assets = append(assets, indexAsset)
	assets = append(assets, detailAssets...)
	contentSHA256 := contentDigest(assets)
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion, Kind: ManifestKind, PublicLyricsVersion: 2,
		BatchSHA256: candidate.BatchSHA256, RootSHA256: candidate.RootSHA256,
		CatalogCount: len(candidate.Index.Songs), DetailCount: len(detailAssets), States: states,
		Index: indexAsset, Details: detailAssets, ContentSHA256: contentSHA256,
	}
	if err := ValidateManifest(manifest); err != nil {
		return Bundle{}, err
	}
	manifestBody, err := marshalCanonical(manifest)
	if err != nil || len(manifestBody) > maxManifestBytes {
		return Bundle{}, errors.New("public candidate manifest exceeds its canonical boundary")
	}
	receipt := Receipt{
		SchemaVersion: ReceiptSchemaVersion, Kind: ReceiptKind,
		BatchSHA256: candidate.BatchSHA256, RootSHA256: candidate.RootSHA256,
		DatabaseSHA256: databaseSHA256, DatabaseBytes: databaseBytes,
		ManifestSHA256: digestBytes(manifestBody), ManifestBytes: int64(len(manifestBody)),
		ContentSHA256: contentSHA256, CatalogCount: len(candidate.Index.Songs),
		DetailCount: len(detailAssets), AssetCount: len(assets),
	}
	receipt.ReceiptSHA256, err = receiptDigest(receipt)
	if err != nil {
		return Bundle{}, err
	}
	if err := ValidateReceipt(receipt, manifest, manifestBody); err != nil {
		return Bundle{}, err
	}
	receiptBody, err := marshalCanonical(receipt)
	if err != nil || len(receiptBody) > maxReceiptBytes {
		return Bundle{}, errors.New("public candidate receipt exceeds its canonical boundary")
	}

	totalBytes := int64(len(indexBody) + len(manifestBody) + len(receiptBody))
	for _, body := range detailBodies {
		totalBytes += int64(len(body))
	}
	if totalBytes > maxCandidateBytes {
		return Bundle{}, errors.New("public candidate bundle exceeds its byte boundary")
	}
	return Bundle{
		IndexBody: indexBody, DetailBodies: detailBodies,
		Manifest: manifest, ManifestBody: manifestBody, Receipt: receipt, ReceiptBody: receiptBody,
	}, nil
}

func BuildV3Bundle(candidate store.RecoveryPublicLyricsV3Candidate, databaseSHA256 string, databaseBytes int64) (Bundle, error) {
	if !canonicalSHA256.MatchString(candidate.BatchSHA256) || !canonicalSHA256.MatchString(candidate.RootSHA256) ||
		!canonicalSHA256.MatchString(databaseSHA256) || databaseBytes <= 0 || candidate.Index.Version != 3 ||
		len(candidate.Index.Songs) == 0 || len(candidate.Index.Songs) > model.PublicLyricsMaxIndexEntries || candidate.Details == nil {
		return Bundle{}, errors.New("public v3 candidate envelope is invalid")
	}
	indexBody, err := marshalCanonical(candidate.Index)
	if err != nil {
		return Bundle{}, err
	}
	indexAsset := newAsset(IndexPath, 0, indexBody)
	detailBodies := make(map[int][]byte, len(candidate.Details))
	detailAssets := make([]Asset, 0, len(candidate.Details))
	stateCounts := make(map[store.PublicLyricsAvailabilityState]int)
	lastMusicID := 0
	for _, song := range candidate.Index.Songs {
		if song.MusicID <= lastMusicID || song.Revision <= 0 || song.UpdatedAt == "" || song.Title.Japanese == "" {
			return Bundle{}, errors.New("public v3 candidate index songs are invalid")
		}
		lastMusicID = song.MusicID
		stateCounts[song.State]++
		detail, exists := candidate.Details[song.MusicID]
		expects := song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly
		if exists != expects {
			return Bundle{}, fmt.Errorf("public v3 candidate music %d detail presence does not match state", song.MusicID)
		}
		if !exists {
			continue
		}
		if detail.Version != 3 || detail.MusicID != song.MusicID || detail.Revision != song.Revision || detail.UpdatedAt != song.UpdatedAt || detail.State != song.State {
			return Bundle{}, fmt.Errorf("public v3 candidate music %d identity drifted", song.MusicID)
		}
		body, err := marshalCanonical(detail)
		if err != nil {
			return Bundle{}, err
		}
		detailBodies[song.MusicID] = body
		detailAssets = append(detailAssets, newAsset(detailPath(song.MusicID), song.MusicID, body))
	}
	if len(detailBodies) != len(candidate.Details) {
		return Bundle{}, errors.New("public v3 candidate contains details outside its index")
	}
	states, err := orderedStateCounts(stateCounts, len(candidate.Index.Songs))
	if err != nil {
		return Bundle{}, err
	}
	assets := append([]Asset{indexAsset}, detailAssets...)
	contentSHA := contentDigest(assets)
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, Kind: ManifestKind, PublicLyricsVersion: 3, BatchSHA256: candidate.BatchSHA256, RootSHA256: candidate.RootSHA256, CatalogCount: len(candidate.Index.Songs), DetailCount: len(detailAssets), States: states, Index: indexAsset, Details: detailAssets, ContentSHA256: contentSHA}
	if err := ValidateManifestV3(manifest); err != nil {
		return Bundle{}, err
	}
	manifestBody, err := marshalCanonical(manifest)
	if err != nil || len(manifestBody) > maxManifestBytes {
		return Bundle{}, errors.New("public v3 candidate manifest exceeds its canonical boundary")
	}
	receipt := Receipt{SchemaVersion: ReceiptSchemaVersion, Kind: ReceiptKind, BatchSHA256: candidate.BatchSHA256, RootSHA256: candidate.RootSHA256, DatabaseSHA256: databaseSHA256, DatabaseBytes: databaseBytes, ManifestSHA256: digestBytes(manifestBody), ManifestBytes: int64(len(manifestBody)), ContentSHA256: contentSHA, CatalogCount: len(candidate.Index.Songs), DetailCount: len(detailAssets), AssetCount: len(assets)}
	receipt.ReceiptSHA256, err = receiptDigest(receipt)
	if err != nil {
		return Bundle{}, err
	}
	if err := ValidateReceiptV3(receipt, manifest, manifestBody); err != nil {
		return Bundle{}, err
	}
	receiptBody, err := marshalCanonical(receipt)
	if err != nil || len(receiptBody) > maxReceiptBytes {
		return Bundle{}, errors.New("public v3 candidate receipt exceeds its canonical boundary")
	}
	totalBytes := int64(len(indexBody) + len(manifestBody) + len(receiptBody))
	for _, body := range detailBodies {
		totalBytes += int64(len(body))
	}
	if totalBytes > maxCandidateBytes {
		return Bundle{}, errors.New("public v3 candidate bundle exceeds its byte boundary")
	}
	return Bundle{IndexBody: indexBody, DetailBodies: detailBodies, Manifest: manifest, ManifestBody: manifestBody, Receipt: receipt, ReceiptBody: receiptBody}, nil
}

func ValidateManifestV3(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.Kind != ManifestKind || manifest.PublicLyricsVersion != 3 ||
		!canonicalSHA256.MatchString(manifest.BatchSHA256) || !canonicalSHA256.MatchString(manifest.RootSHA256) || manifest.CatalogCount <= 0 ||
		manifest.CatalogCount > model.PublicLyricsMaxIndexEntries || manifest.DetailCount < 0 || manifest.DetailCount > manifest.CatalogCount || !canonicalSHA256.MatchString(manifest.ContentSHA256) ||
		manifest.Index.Path != IndexPath || manifest.Index.MusicID != 0 || !validAsset(manifest.Index) || manifest.Details == nil || len(manifest.Details) != manifest.DetailCount {
		return errors.New("public v3 candidate manifest envelope is invalid")
	}
	if len(manifest.States) != len(publicStateOrder()) {
		return errors.New("public v3 candidate manifest states are incomplete")
	}
	total, text := 0, 0
	for index, state := range manifest.States {
		if state.State != publicStateOrder()[index] || state.Count < 0 {
			return errors.New("public v3 candidate manifest states are invalid")
		}
		total += state.Count
		if state.State == store.PublicLyricsStateComplete || state.State == store.PublicLyricsStateGameOnly {
			text += state.Count
		}
	}
	if total != manifest.CatalogCount || text != manifest.DetailCount {
		return errors.New("public v3 candidate manifest counts do not close")
	}
	last := 0
	assets := []Asset{manifest.Index}
	for _, asset := range manifest.Details {
		if asset.MusicID <= last || asset.Path != detailPath(asset.MusicID) || !validAsset(asset) {
			return errors.New("public v3 candidate manifest details are invalid")
		}
		last = asset.MusicID
		assets = append(assets, asset)
	}
	if contentDigest(assets) != manifest.ContentSHA256 {
		return errors.New("public v3 candidate manifest content digest does not match")
	}
	return nil
}

func ValidateReceiptV3(receipt Receipt, manifest Manifest, manifestBody []byte) error {
	if receipt.SchemaVersion != ReceiptSchemaVersion || receipt.Kind != ReceiptKind || receipt.BatchSHA256 != manifest.BatchSHA256 || receipt.RootSHA256 != manifest.RootSHA256 ||
		!canonicalSHA256.MatchString(receipt.DatabaseSHA256) || receipt.DatabaseBytes <= 0 || receipt.ManifestSHA256 != digestBytes(manifestBody) || receipt.ManifestBytes != int64(len(manifestBody)) ||
		receipt.ContentSHA256 != manifest.ContentSHA256 || receipt.CatalogCount != manifest.CatalogCount || receipt.DetailCount != manifest.DetailCount || receipt.AssetCount != manifest.DetailCount+1 || !canonicalSHA256.MatchString(receipt.ReceiptSHA256) {
		return errors.New("public v3 candidate receipt envelope is invalid")
	}
	digest, err := receiptDigest(receipt)
	if err != nil || digest != receipt.ReceiptSHA256 {
		return errors.New("public v3 candidate receipt digest does not match")
	}
	return nil
}

func PublishExactV3(outputDirectory string, bundle Bundle) error {
	if !filepath.IsAbs(outputDirectory) || filepath.Clean(outputDirectory) != outputDirectory {
		return errors.New("public v3 candidate output directory must be canonical absolute")
	}
	if err := ValidateManifestV3(bundle.Manifest); err != nil {
		return err
	}
	if err := ValidateReceiptV3(bundle.Receipt, bundle.Manifest, bundle.ManifestBody); err != nil {
		return err
	}
	if !bytes.Equal(bundle.ManifestBody, mustCanonical(bundle.Manifest)) || !bytes.Equal(bundle.ReceiptBody, mustCanonical(bundle.Receipt)) {
		return errors.New("public v3 candidate manifest or receipt bytes are not canonical")
	}
	if err := validateV3BundleBodies(bundle); err != nil {
		return err
	}
	parent := filepath.Dir(outputDirectory)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("public v3 candidate output parent is invalid")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return errors.New("public v3 candidate output parent must not traverse a filesystem alias")
	}
	if _, err := os.Lstat(outputDirectory); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("public v3 candidate output directory already exists")
		}
		return err
	}
	return publishBundleDirectory(outputDirectory, bundle, "public v3 candidate")
}

func validateV3BundleBodies(bundle Bundle) error {
	if newAsset(IndexPath, 0, bundle.IndexBody) != bundle.Manifest.Index {
		return errors.New("public v3 candidate index bytes do not match the manifest")
	}
	index, err := store.DecodePublicLyricsV3Index(bundle.IndexBody)
	if err != nil || !bytes.Equal(bundle.IndexBody, mustCanonical(index)) ||
		len(index.Songs) != bundle.Manifest.CatalogCount {
		return errors.New("public v3 candidate index bytes are not the exact canonical v3 index")
	}
	indexed := make(map[int]store.PublicLyricsIndexSong, len(index.Songs))
	stateCounts := make(map[store.PublicLyricsAvailabilityState]int)
	expectedDetails := 0
	for _, song := range index.Songs {
		indexed[song.MusicID] = song
		stateCounts[song.State]++
		if song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly {
			expectedDetails++
		}
	}
	states, err := orderedStateCounts(stateCounts, len(index.Songs))
	if err != nil || !equalStateCounts(states, bundle.Manifest.States) {
		return errors.New("public v3 candidate index states do not match the manifest")
	}
	if expectedDetails != bundle.Manifest.DetailCount || len(bundle.DetailBodies) != bundle.Manifest.DetailCount {
		return errors.New("public v3 candidate detail bodies do not close against the index")
	}
	totalBytes := int64(len(bundle.IndexBody) + len(bundle.ManifestBody) + len(bundle.ReceiptBody))
	for _, asset := range bundle.Manifest.Details {
		body, exists := bundle.DetailBodies[asset.MusicID]
		if !exists || newAsset(detailPath(asset.MusicID), asset.MusicID, body) != asset {
			return fmt.Errorf("public v3 candidate music %d bytes do not match the manifest", asset.MusicID)
		}
		detail, err := store.DecodePublicLyricsV3Detail(body)
		if err != nil || !bytes.Equal(body, mustCanonical(detail)) {
			return fmt.Errorf("public v3 candidate music %d bytes are not the exact strict v3 detail", asset.MusicID)
		}
		song, exists := indexed[asset.MusicID]
		if !exists || detail.Version != 3 || detail.MusicID != song.MusicID || detail.Revision != song.Revision ||
			detail.UpdatedAt != song.UpdatedAt || detail.State != song.State ||
			!equalStrings(publicV3DetailAvailableVersions(detail), song.AvailableVersions) ||
			(detail.State != store.PublicLyricsStateComplete && detail.State != store.PublicLyricsStateGameOnly) {
			return fmt.Errorf("public v3 candidate music %d bytes do not match the exact index identity", asset.MusicID)
		}
		totalBytes += int64(len(body))
	}
	if totalBytes > maxCandidateBytes {
		return errors.New("public v3 candidate bundle exceeds its byte boundary")
	}
	return nil
}

func equalStateCounts(left, right []StateCount) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func publicV3DetailAvailableVersions(detail store.PublicLyricsV3DetailDocument) []string {
	full, game := false, false
	for _, rendition := range detail.Renditions {
		full = full || rendition.Full != nil
		game = game || rendition.Game != nil
	}
	versions := make([]string, 0, 2)
	if full {
		versions = append(versions, "full")
	}
	if game {
		versions = append(versions, "game")
	}
	return versions
}

func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.Kind != ManifestKind ||
		manifest.PublicLyricsVersion != 2 || !canonicalSHA256.MatchString(manifest.BatchSHA256) ||
		!canonicalSHA256.MatchString(manifest.RootSHA256) || manifest.CatalogCount <= 0 ||
		manifest.CatalogCount > model.PublicLyricsMaxIndexEntries || manifest.DetailCount < 0 ||
		manifest.DetailCount > manifest.CatalogCount || !canonicalSHA256.MatchString(manifest.ContentSHA256) ||
		manifest.Index.Path != IndexPath || manifest.Index.MusicID != 0 || !validAsset(manifest.Index) ||
		manifest.Details == nil || len(manifest.Details) != manifest.DetailCount {
		return errors.New("public candidate manifest envelope is invalid")
	}
	if len(manifest.States) != len(publicStateOrder()) {
		return errors.New("public candidate manifest state counts are incomplete")
	}
	totalStates := 0
	textStates := 0
	for index, state := range manifest.States {
		if state.State != publicStateOrder()[index] || state.Count < 0 {
			return errors.New("public candidate manifest state counts are invalid")
		}
		totalStates += state.Count
		if state.State == store.PublicLyricsStateComplete || state.State == store.PublicLyricsStateGameOnly {
			textStates += state.Count
		}
	}
	if totalStates != manifest.CatalogCount || textStates != manifest.DetailCount {
		return errors.New("public candidate manifest counts do not close")
	}
	lastMusicID := 0
	assets := make([]Asset, 0, len(manifest.Details)+1)
	assets = append(assets, manifest.Index)
	for _, asset := range manifest.Details {
		if asset.MusicID <= lastMusicID || asset.Path != detailPath(asset.MusicID) || !validAsset(asset) {
			return errors.New("public candidate manifest details are invalid or not strictly ordered")
		}
		lastMusicID = asset.MusicID
		assets = append(assets, asset)
	}
	if contentDigest(assets) != manifest.ContentSHA256 {
		return errors.New("public candidate manifest content digest does not match")
	}
	return nil
}

func ValidateReceipt(receipt Receipt, manifest Manifest, manifestBody []byte) error {
	if receipt.SchemaVersion != ReceiptSchemaVersion || receipt.Kind != ReceiptKind ||
		receipt.BatchSHA256 != manifest.BatchSHA256 || receipt.RootSHA256 != manifest.RootSHA256 ||
		!canonicalSHA256.MatchString(receipt.DatabaseSHA256) || receipt.DatabaseBytes <= 0 ||
		receipt.ManifestSHA256 != digestBytes(manifestBody) || receipt.ManifestBytes != int64(len(manifestBody)) ||
		receipt.ContentSHA256 != manifest.ContentSHA256 || receipt.CatalogCount != manifest.CatalogCount ||
		receipt.DetailCount != manifest.DetailCount || receipt.AssetCount != manifest.DetailCount+1 ||
		!canonicalSHA256.MatchString(receipt.ReceiptSHA256) {
		return errors.New("public candidate receipt envelope is invalid")
	}
	digest, err := receiptDigest(receipt)
	if err != nil || digest != receipt.ReceiptSHA256 {
		return errors.New("public candidate receipt digest does not match")
	}
	return nil
}

func PublishExact(outputDirectory string, bundle Bundle) error {
	if !filepath.IsAbs(outputDirectory) || filepath.Clean(outputDirectory) != outputDirectory {
		return errors.New("public candidate output directory must be a canonical absolute path")
	}
	parent := filepath.Dir(outputDirectory)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("public candidate output parent must be an existing direct directory")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return errors.New("public candidate output parent must not traverse a filesystem alias")
	}
	if _, err := os.Lstat(outputDirectory); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("public candidate output directory already exists")
		}
		return fmt.Errorf("inspect public candidate output directory: %w", err)
	}
	if err := ValidateManifest(bundle.Manifest); err != nil {
		return err
	}
	if err := ValidateReceipt(bundle.Receipt, bundle.Manifest, bundle.ManifestBody); err != nil {
		return err
	}
	if !bytes.Equal(bundle.ManifestBody, mustCanonical(bundle.Manifest)) ||
		!bytes.Equal(bundle.ReceiptBody, mustCanonical(bundle.Receipt)) {
		return errors.New("public candidate manifest or receipt bytes are not canonical")
	}
	if err := validateBundleBodies(bundle); err != nil {
		return err
	}
	return publishBundleDirectory(outputDirectory, bundle, "public candidate")
}

func publishBundleDirectory(outputDirectory string, bundle Bundle, label string) (err error) {
	parent := filepath.Dir(outputDirectory)
	staging, err := os.MkdirTemp(parent, ".public-candidate-staging-")
	if err != nil {
		return fmt.Errorf("create %s staging directory: %w", label, err)
	}
	defer func() {
		if staging != "" {
			if removeErr := os.RemoveAll(staging); err == nil && removeErr != nil {
				err = fmt.Errorf("clean %s staging directory: %w", label, removeErr)
			}
		}
	}()
	if err := writeExclusive(filepath.Join(staging, IndexPath), bundle.IndexBody); err != nil {
		return fmt.Errorf("write %s index: %w", label, err)
	}
	musicIDs := make([]int, 0, len(bundle.DetailBodies))
	for musicID := range bundle.DetailBodies {
		musicIDs = append(musicIDs, musicID)
	}
	sort.Ints(musicIDs)
	for _, musicID := range musicIDs {
		if err := writeExclusive(filepath.Join(staging, detailPath(musicID)), bundle.DetailBodies[musicID]); err != nil {
			return fmt.Errorf("write %s detail %d: %w", label, musicID, err)
		}
	}
	if err := writeExclusive(filepath.Join(staging, ManifestPath), bundle.ManifestBody); err != nil {
		return fmt.Errorf("write %s manifest: %w", label, err)
	}
	// The receipt is deliberately last. Its presence means every referenced
	// asset and the manifest were written into the fully staged directory.
	if err := writeExclusive(filepath.Join(staging, ReceiptPath), bundle.ReceiptBody); err != nil {
		return fmt.Errorf("write %s receipt: %w", label, err)
	}
	if err := syncDirectory(staging); err != nil {
		return fmt.Errorf("sync %s staging directory: %w", label, err)
	}
	if publishBundleBeforeRenameHook != nil {
		if err := publishBundleBeforeRenameHook(staging); err != nil {
			return err
		}
	}
	if _, statErr := os.Lstat(outputDirectory); !errors.Is(statErr, os.ErrNotExist) {
		if statErr == nil {
			return fmt.Errorf("%s output directory appeared during publication", label)
		}
		return fmt.Errorf("inspect %s output directory during publication: %w", label, statErr)
	}
	if err := os.Rename(staging, outputDirectory); err != nil {
		return fmt.Errorf("publish %s output directory: %w", label, err)
	}
	staging = ""
	return syncDirectory(parent)
}

func validateBundleBodies(bundle Bundle) error {
	if newAsset(IndexPath, 0, bundle.IndexBody) != bundle.Manifest.Index {
		return errors.New("public candidate index bytes do not match the manifest")
	}
	var index store.PublicLyricsIndexDocument
	if err := json.Unmarshal(bundle.IndexBody, &index); err != nil || !bytes.Equal(bundle.IndexBody, mustCanonical(index)) ||
		index.Version != 2 || len(index.Songs) != bundle.Manifest.CatalogCount {
		return errors.New("public candidate index bytes are not the exact canonical v2 index")
	}
	indexed := make(map[int]store.PublicLyricsIndexSong, len(index.Songs))
	expectedDetails := 0
	lastMusicID := 0
	for _, song := range index.Songs {
		if song.MusicID <= lastMusicID {
			return errors.New("public candidate index bytes are not strictly ordered")
		}
		lastMusicID = song.MusicID
		indexed[song.MusicID] = song
		if song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly {
			expectedDetails++
		}
	}
	if expectedDetails != bundle.Manifest.DetailCount || len(bundle.DetailBodies) != bundle.Manifest.DetailCount {
		return errors.New("public candidate detail bodies do not close against the index")
	}
	totalBytes := int64(len(bundle.IndexBody) + len(bundle.ManifestBody) + len(bundle.ReceiptBody))
	for _, asset := range bundle.Manifest.Details {
		body, exists := bundle.DetailBodies[asset.MusicID]
		if !exists || newAsset(detailPath(asset.MusicID), asset.MusicID, body) != asset {
			return fmt.Errorf("public candidate music %d bytes do not match the manifest", asset.MusicID)
		}
		var detail store.PublicLyricsDetailDocument
		if err := json.Unmarshal(body, &detail); err != nil || !bytes.Equal(body, mustCanonical(detail)) {
			return fmt.Errorf("public candidate music %d bytes are not canonical", asset.MusicID)
		}
		song, exists := indexed[asset.MusicID]
		if !exists || detail.Version != 2 || detail.MusicID != song.MusicID || detail.Revision != song.Revision ||
			detail.UpdatedAt != song.UpdatedAt || detail.State != song.State ||
			!equalStrings(detail.AvailableVersions, song.AvailableVersions) ||
			(detail.State != store.PublicLyricsStateComplete && detail.State != store.PublicLyricsStateGameOnly) {
			return fmt.Errorf("public candidate music %d bytes do not match the exact index identity", asset.MusicID)
		}
		totalBytes += int64(len(body))
	}
	if totalBytes > maxCandidateBytes {
		return errors.New("public candidate bundle exceeds its byte boundary")
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open public candidate directory %s: %w", filepath.Base(path), err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync public candidate directory %s: %w", filepath.Base(path), err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close public candidate directory %s: %w", filepath.Base(path), err)
	}
	return nil
}

func orderedStateCounts(counts map[store.PublicLyricsAvailabilityState]int, total int) ([]StateCount, error) {
	result := make([]StateCount, 0, len(publicStateOrder()))
	seen := 0
	for _, state := range publicStateOrder() {
		count := counts[state]
		result = append(result, StateCount{State: state, Count: count})
		seen += count
		delete(counts, state)
	}
	if seen != total || len(counts) != 0 {
		return nil, errors.New("public candidate contains unsupported or incomplete state counts")
	}
	return result, nil
}

func publicStateOrder() []store.PublicLyricsAvailabilityState {
	return []store.PublicLyricsAvailabilityState{
		store.PublicLyricsStateComplete,
		store.PublicLyricsStateGameOnly,
		store.PublicLyricsStateSatisfiedNoLyrics,
		store.PublicLyricsStateAmbiguous,
		store.PublicLyricsStateMissing,
		store.PublicLyricsStateIncomplete,
		store.PublicLyricsStateFailed,
	}
}

func detailPath(musicID int) string {
	return "music_" + strconv.Itoa(musicID) + ".json"
}

func newAsset(path string, musicID int, body []byte) Asset {
	return Asset{Path: path, MusicID: musicID, SHA256: digestBytes(body), Bytes: int64(len(body))}
}

func validAsset(asset Asset) bool {
	return asset.Path != "" && asset.Path == filepath.Base(asset.Path) && utf8.ValidString(asset.Path) &&
		!strings.ContainsAny(asset.Path, "\x00\r\n") && canonicalSHA256.MatchString(asset.SHA256) &&
		asset.Bytes > 0 && asset.Bytes <= model.PublicLyricsMaxArtifactBytes
}

func contentDigest(assets []Asset) string {
	hasher := sha256.New()
	for _, asset := range assets {
		_, _ = io.WriteString(hasher, asset.Path)
		_, _ = hasher.Write([]byte{0})
		_, _ = io.WriteString(hasher, strconv.FormatInt(asset.Bytes, 10))
		_, _ = hasher.Write([]byte{0})
		_, _ = io.WriteString(hasher, asset.SHA256)
		_, _ = hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func receiptDigest(receipt Receipt) (string, error) {
	receipt.ReceiptSHA256 = ""
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	return digestBytes(body), nil
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func marshalCanonical(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func mustCanonical(value any) []byte {
	body, _ := marshalCanonical(value)
	return body
}

func writeExclusive(path string, body []byte) error {
	if len(body) == 0 {
		return errors.New("public candidate asset body is empty")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create public candidate asset %s: %w", filepath.Base(path), err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	return nil
}
