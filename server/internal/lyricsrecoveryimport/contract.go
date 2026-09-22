// Package lyricsrecoveryimport defines the additive all-root import manifest
// that bridges recovery v2 into the existing Full draft store plus explicit
// non-Full availability documents.
package lyricsrecoveryimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsrecovery"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

const (
	ManifestSchemaVersion = 1
	MaxManifestBytes      = 256 << 20
	MaxManifestItems      = 100_000
)

var canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type RootBinding struct {
	SchemaVersion  int                     `json:"schemaVersion"`
	RootID         string                  `json:"rootId"`
	RootSHA256     string                  `json:"rootSha256"`
	CatalogCount   int                     `json:"catalogCount"`
	MusicIDsSHA256 string                  `json:"musicIdsSha256"`
	Coverage       lyricscontract.Coverage `json:"coverage"`
}

type Item struct {
	MusicID                    int                               `json:"musicId"`
	JapaneseTitle              string                            `json:"japaneseTitle"`
	CatalogFingerprint         string                            `json:"catalogFingerprint"`
	TargetMusicID              int                               `json:"targetMusicId"`
	AssociationMusicIDs        []int                             `json:"associationMusicIds"`
	State                      lyricscontract.CoverageState      `json:"state"`
	ResultSHA256               string                            `json:"resultSha256"`
	Draft                      *lyricsstaging.Draft              `json:"draft,omitempty"`
	Availability               *model.LyricsAvailabilityDocument `json:"availability,omitempty"`
	AvailabilityDocumentSHA256 string                            `json:"availabilityDocumentSha256,omitempty"`
	Artifacts                  []lyricsstaging.Artifact          `json:"artifacts,omitempty"`
	Translations               []string                          `json:"translations,omitempty"`
}

type Manifest struct {
	SchemaVersion int         `json:"schemaVersion"`
	Root          RootBinding `json:"root"`
	Items         []Item      `json:"items"`
	BatchSHA256   string      `json:"batchSha256"`
}

func NewManifest(root lyricsrootmanifest.Manifest, items []Item) (Manifest, error) {
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Root: RootBinding{
			SchemaVersion: root.SchemaVersion, RootID: root.RootID, RootSHA256: root.RootSHA256,
			CatalogCount: root.Catalog.RecordCount, MusicIDsSHA256: root.Catalog.MusicIDsSHA256,
			Coverage: root.Coverage,
		},
		Items: cloneItems(items),
	}
	sort.Slice(manifest.Items, func(left, right int) bool {
		return manifest.Items[left].MusicID < manifest.Items[right].MusicID
	})
	if err := validateManifest(manifest, false); err != nil {
		return Manifest{}, err
	}
	digest, err := manifestDigest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.BatchSHA256 = digest
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return cloneManifest(manifest), nil
}

func ValidateManifest(manifest Manifest) error {
	if err := validateManifest(manifest, true); err != nil {
		return err
	}
	digest, err := manifestDigest(manifest)
	if err != nil || digest != manifest.BatchSHA256 {
		return errors.New("recovery import manifest batch digest does not match")
	}
	body, err := json.Marshal(manifest)
	if err != nil || len(body) == 0 || len(body) > MaxManifestBytes || !utf8.Valid(body) {
		return errors.New("recovery import manifest exceeds its byte boundary")
	}
	return nil
}

func validateManifest(manifest Manifest, requireDigest bool) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.Root.SchemaVersion != lyricscontract.SchemaVersionV2 ||
		manifest.Root.RootID == "" || strings.TrimSpace(manifest.Root.RootID) != manifest.Root.RootID ||
		!canonicalSHA256.MatchString(manifest.Root.RootSHA256) || manifest.Root.CatalogCount <= 0 ||
		manifest.Root.CatalogCount > MaxManifestItems || !canonicalSHA256.MatchString(manifest.Root.MusicIDsSHA256) ||
		manifest.Items == nil || len(manifest.Items) != manifest.Root.CatalogCount || len(manifest.Items) > MaxManifestItems {
		return errors.New("recovery import manifest envelope is invalid")
	}
	if requireDigest {
		if !canonicalSHA256.MatchString(manifest.BatchSHA256) {
			return errors.New("recovery import manifest digest is invalid")
		}
	} else if manifest.BatchSHA256 != "" {
		return errors.New("new recovery import manifest contains a premature digest")
	}
	stateCounts := map[lyricscontract.CoverageState]int{}
	musicIDs := make([]int, len(manifest.Items))
	lastMusicID := 0
	for index, item := range manifest.Items {
		if item.MusicID <= lastMusicID {
			return errors.New("recovery import manifest items are not strictly ordered")
		}
		lastMusicID = item.MusicID
		musicIDs[index] = item.MusicID
		if err := validateItem(item); err != nil {
			return fmt.Errorf("recovery import music %d: %w", item.MusicID, err)
		}
		stateCounts[item.State]++
	}
	musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil || musicIDsSHA256 != manifest.Root.MusicIDsSHA256 {
		return errors.New("recovery import manifest music IDs do not match the compact-root binding")
	}
	if !coverageCountsMatch(manifest.Root.Coverage, stateCounts, len(manifest.Items)) {
		return errors.New("recovery import manifest state counts do not match the compact-root coverage")
	}
	return nil
}

func validateItem(item Item) error {
	if item.MusicID <= 0 || item.TargetMusicID != item.MusicID {
		return errors.New("music target identity is invalid")
	}
	if strings.TrimSpace(item.JapaneseTitle) == "" || item.JapaneseTitle != strings.TrimSpace(item.JapaneseTitle) ||
		strings.ContainsAny(item.JapaneseTitle, "\r\n") {
		return errors.New("Japanese title is invalid")
	}
	if !canonicalSHA256.MatchString(item.CatalogFingerprint) || !canonicalSHA256.MatchString(item.ResultSHA256) {
		return errors.New("catalog fingerprint or result digest is invalid")
	}
	if item.AssociationMusicIDs == nil || !strictlyIncreasingPositiveInts(item.AssociationMusicIDs) {
		return errors.New("catalog associations are invalid")
	}
	for _, association := range item.AssociationMusicIDs {
		if association == item.MusicID {
			return errors.New("catalog association contains the target music ID")
		}
	}
	switch item.State {
	case lyricscontract.CoverageComplete, lyricscontract.CoverageGameOnly:
		if item.Draft != nil {
			if item.Availability != nil || item.AvailabilityDocumentSHA256 != "" || item.Artifacts != nil || item.Translations != nil {
				return errors.New("rendition item mixes Draft and availability ownership")
			}
			if err := lyricsstaging.ValidateDraft(*item.Draft); err != nil {
				return err
			}
			if item.Draft.MusicID != item.MusicID || item.Draft.JapaneseTitle != item.JapaneseTitle ||
				item.Draft.CatalogFingerprint != item.CatalogFingerprint || item.Draft.TargetMusicID != item.TargetMusicID ||
				!reflect.DeepEqual(item.Draft.AssociationMusicIDs, item.AssociationMusicIDs) {
				return errors.New("rendition staging draft drifted from the recovery item identity")
			}
			if item.State == lyricscontract.CoverageGameOnly && item.Draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
				return errors.New("Game-only Draft ownership requires source v3")
			}
			break
		}
		if item.State == lyricscontract.CoverageComplete {
			return errors.New("complete item must own a staging draft")
		}
		if item.Availability == nil || item.Artifacts == nil || len(item.Artifacts) == 0 ||
			item.Translations == nil || item.Availability.State != model.LyricsAvailabilityStateGameOnly {
			return errors.New("Game-only item is incomplete")
		}
		if err := validateAvailabilityItem(item); err != nil {
			return err
		}
		if item.Availability.Game == nil || len(item.Translations) != len(item.Availability.Game.Lines) {
			return errors.New("Game-only translations do not align with Game text")
		}
	case lyricscontract.CoverageSatisfiedNoLyrics,
		lyricscontract.CoverageAmbiguous, lyricscontract.CoverageMissing,
		lyricscontract.CoverageIncomplete, lyricscontract.CoverageFailed:
		if item.Draft != nil || item.Availability == nil || item.Artifacts != nil || item.Translations != nil {
			return errors.New("text-free recovery item leaked source artifacts or translations")
		}
		if err := validateAvailabilityItem(item); err != nil {
			return err
		}
		if !availabilityStateMatchesCoverage(item.Availability.State, item.State) {
			return errors.New("availability state does not match the recovery state")
		}
	default:
		return errors.New("unsupported recovery import state")
	}
	return nil
}

func validateAvailabilityItem(item Item) error {
	if err := model.ValidateLyricsAvailabilityDocument(*item.Availability); err != nil {
		return err
	}
	digest, err := availabilityDocumentDigest(*item.Availability)
	if err != nil || !canonicalSHA256.MatchString(item.AvailabilityDocumentSHA256) ||
		digest != item.AvailabilityDocumentSHA256 {
		return errors.New("availability document digest does not match")
	}
	if item.Availability.State == model.LyricsAvailabilityStateGameOnly {
		if len(item.Artifacts) != len(item.Availability.FixedIdentities) {
			return errors.New("Game-only artifacts do not match fixed identities")
		}
		lastKey := ""
		for index, artifact := range item.Artifacts {
			if err := lyricsstaging.ValidateRecoveryArtifact(artifact); err != nil {
				return err
			}
			if artifact.Identity.RenditionKey <= lastKey ||
				!reflect.DeepEqual(artifact.Identity, item.Availability.FixedIdentities[index]) {
				return errors.New("Game-only artifacts are unordered or drifted")
			}
			lastKey = artifact.Identity.RenditionKey
		}
	}
	return nil
}

// ValidateAgainstRoot proves that every manifest item is the exact ordered
// projection of one immutable compact-root SongResult.
func ValidateAgainstRoot(manifest Manifest, root lyricsrootmanifest.Manifest, results map[int]lyricsrecovery.SongResult) error {
	if err := ValidateManifest(manifest); err != nil {
		return err
	}
	if err := lyricsrootmanifest.Validate(root); err != nil {
		return err
	}
	if manifest.Root.SchemaVersion != root.SchemaVersion || manifest.Root.RootID != root.RootID ||
		manifest.Root.RootSHA256 != root.RootSHA256 || manifest.Root.CatalogCount != root.Catalog.RecordCount ||
		manifest.Root.MusicIDsSHA256 != root.Catalog.MusicIDsSHA256 || !reflect.DeepEqual(manifest.Root.Coverage, root.Coverage) ||
		len(manifest.Items) != len(root.Songs) || len(results) != len(root.Songs) {
		return errors.New("recovery import manifest does not match the compact root")
	}
	for index, item := range manifest.Items {
		rootSong := root.Songs[index]
		result, found := results[item.MusicID]
		if !found || item.MusicID != rootSong.MusicID || item.State != rootSong.State ||
			item.ResultSHA256 != rootSong.ResultSHA256 || result.MusicID != item.MusicID || result.State != item.State ||
			result.ResultSHA256 != item.ResultSHA256 {
			return fmt.Errorf("recovery import music %d does not exactly follow the compact root", item.MusicID)
		}
		rootRef, err := lyricsrecovery.RootSongRef(result)
		if err != nil || !reflect.DeepEqual(rootRef, rootSong) {
			return fmt.Errorf("recovery import music %d result drifted from its compact-root ref", item.MusicID)
		}
		if err := validateItemAgainstResult(item, result); err != nil {
			return fmt.Errorf("recovery import music %d: %w", item.MusicID, err)
		}
	}
	return nil
}

func validateItemAgainstResult(item Item, result lyricsrecovery.SongResult) error {
	if result.SchemaVersion == lyricsrecovery.SongResultSchemaVersionV3 {
		if item.Draft == nil || item.Draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
			return errors.New("v3 recovery result requires a source v3 Draft")
		}
		payloads := make([]model.LyricsSourceRendition, len(result.Renditions))
		for index, rendition := range result.Renditions {
			payload, err := sourceRenditionFromResult(rendition)
			if err != nil {
				return err
			}
			payloads[index] = payload
		}
		if !reflect.DeepEqual(item.Draft.Document.Renditions, payloads) {
			return errors.New("v3 staging draft drifted from the complete recovery rendition array")
		}
		if !reflect.DeepEqual(item.Draft.RenditionTranslations, renditionTranslationsFromResult(result)) {
			return errors.New("v3 staging translations drifted from the complete recovery rendition array")
		}
		return nil
	}
	switch item.State {
	case lyricscontract.CoverageComplete:
		document := item.Draft.Document
		if result.Full == nil || document.ReasonCode != result.ReasonCode ||
			!reflect.DeepEqual(document.Full, *result.Full) ||
			!reflect.DeepEqual(document.GameProjection, result.GameProjection) ||
			!reflect.DeepEqual(item.Draft.Translations, result.Translations) {
			return errors.New("Full draft drifted from the authoritative recovery composition")
		}
	case lyricscontract.CoverageGameOnly:
		if result.Game == nil || item.Availability.ReasonCode != result.ReasonCode ||
			!reflect.DeepEqual(item.Availability.Game, result.Game) ||
			!reflect.DeepEqual(item.Translations, result.Translations) {
			return errors.New("Game-only availability drifted from the authoritative recovery composition")
		}
	case lyricscontract.CoverageSatisfiedNoLyrics:
		if result.NoLyricsReason != lyricsrecovery.NoLyricsReasonCatalogInstrumental ||
			item.Availability.NoLyricsReason != model.LyricsAvailabilityNoLyricsCatalogInstrumental {
			return errors.New("satisfied no-lyrics reason drifted from recovery")
		}
	default:
		if result.ReasonCode != model.LyricsSourceVersionReasonVersionConflict {
			return errors.New("unresolved recovery result is not fail closed")
		}
	}
	return nil
}

func availabilityStateMatchesCoverage(state model.LyricsAvailabilityState, coverage lyricscontract.CoverageState) bool {
	return string(state) == string(coverage)
}

func coverageCountsMatch(coverage lyricscontract.Coverage, counts map[lyricscontract.CoverageState]int, total int) bool {
	return coverage.Total == total && coverage.Complete == counts[lyricscontract.CoverageComplete] &&
		coverage.GameOnly == counts[lyricscontract.CoverageGameOnly] &&
		coverage.SatisfiedNoLyrics == counts[lyricscontract.CoverageSatisfiedNoLyrics] &&
		coverage.CatalogReview == counts[lyricscontract.CoverageCatalogReview] &&
		coverage.GameSizeEvidence == counts[lyricscontract.CoverageGameSizeEvidence] &&
		coverage.Ambiguous == counts[lyricscontract.CoverageAmbiguous] &&
		coverage.Missing == counts[lyricscontract.CoverageMissing] &&
		coverage.Incomplete == counts[lyricscontract.CoverageIncomplete] &&
		coverage.Failed == counts[lyricscontract.CoverageFailed]
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

func availabilityDocumentDigest(document model.LyricsAvailabilityDocument) (string, error) {
	body, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func AvailabilityDocumentSHA256(document model.LyricsAvailabilityDocument) (string, error) {
	if err := model.ValidateLyricsAvailabilityDocument(document); err != nil {
		return "", err
	}
	return availabilityDocumentDigest(document)
}

func manifestDigest(manifest Manifest) (string, error) {
	manifest.BatchSHA256 = ""
	body, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func MarshalCanonical(manifest Manifest) ([]byte, error) {
	if err := ValidateManifest(manifest); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if len(body) > MaxManifestBytes {
		return nil, errors.New("recovery import manifest exceeds its encoded byte boundary")
	}
	return body, nil
}

func DecodeCanonical(body []byte) (Manifest, error) {
	if len(body) == 0 || len(body) > MaxManifestBytes || !utf8.Valid(body) {
		return Manifest{}, errors.New("recovery import manifest bytes are invalid")
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("recovery import manifest contains trailing JSON")
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	canonical, err := MarshalCanonical(manifest)
	if err != nil || !bytes.Equal(canonical, body) {
		return Manifest{}, errors.New("recovery import manifest is not canonical JSON")
	}
	return cloneManifest(manifest), nil
}

func cloneManifest(manifest Manifest) Manifest {
	manifest.Items = cloneItems(manifest.Items)
	return manifest
}

func cloneStagingRenditionTranslations(input []lyricscontract.RenditionTranslation) []lyricscontract.RenditionTranslation {
	if input == nil {
		return nil
	}
	result := make([]lyricscontract.RenditionTranslation, len(input))
	for index, item := range input {
		result[index] = item
		result[index].Translations = append([]string(nil), item.Translations...)
		result[index].PeerTranslations = make([]lyricscontract.RenditionPeerTranslation, len(item.PeerTranslations))
		for peerIndex, peer := range item.PeerTranslations {
			result[index].PeerTranslations[peerIndex] = peer
			result[index].PeerTranslations[peerIndex].Translations = append([]string(nil), peer.Translations...)
		}
		if item.PeerTranslations == nil {
			result[index].PeerTranslations = nil
		}
	}
	return result
}

func cloneItems(items []Item) []Item {
	if items == nil {
		return nil
	}
	result := make([]Item, len(items))
	for index, item := range items {
		result[index] = item
		if item.AssociationMusicIDs == nil {
			result[index].AssociationMusicIDs = nil
		} else {
			result[index].AssociationMusicIDs = append([]int{}, item.AssociationMusicIDs...)
		}
		if item.Draft != nil {
			draft := *item.Draft
			draft.AssociationMusicIDs = append([]int(nil), item.Draft.AssociationMusicIDs...)
			draft.Source.Categories = append([]string(nil), item.Draft.Source.Categories...)
			draft.Artifacts = append([]lyricsstaging.Artifact(nil), item.Draft.Artifacts...)
			draft.Document.FixedIdentities = append([]model.LyricsSourceFixedIdentity(nil), item.Draft.Document.FixedIdentities...)
			draft.Document.Renditions = model.CloneLyricsSourceRenditions(item.Draft.Document.Renditions)
			draft.RenditionTranslations = cloneStagingRenditionTranslations(item.Draft.RenditionTranslations)
			result[index].Draft = &draft
		}
		if item.Availability != nil {
			document := *item.Availability
			result[index].Availability = &document
		}
		result[index].Artifacts = append([]lyricsstaging.Artifact(nil), item.Artifacts...)
		result[index].Translations = append([]string(nil), item.Translations...)
		if item.Artifacts == nil {
			result[index].Artifacts = nil
		}
		if item.Translations == nil {
			result[index].Translations = nil
		}
	}
	return result
}
