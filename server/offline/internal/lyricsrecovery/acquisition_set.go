package lyricsrecovery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"unicode/utf8"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

const (
	AcquisitionSetSchemaVersionV2     = 2
	AcquisitionSetCanonicalEncodingV2 = "moesekai-lyrics-recovery-acquisition-set-ordered-json-v2"
	AcquisitionSetDigestAlgorithmV2   = "sha256-moesekai-lyrics-recovery-acquisition-set-v2"
	MaxAcquisitionSetBytes            = 16 << 20
	MaxSongAcquisitionIDs             = 1024
)

var canonicalAcquisitionSetSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ProviderAcquisitionSet struct {
	Provider       model.LyricsSourceProvider        `json:"provider"`
	AcquisitionIDs []lyricsacquisition.AcquisitionID `json:"acquisitionIds"`
	Status         lyricsprovideroutcome.Status      `json:"status"`
	ReasonCode     lyricsprovideroutcome.ReasonCode  `json:"reasonCode"`
	Phase          lyricsprovideroutcome.Phase       `json:"phase"`
	Counts         lyricsprovideroutcome.Counts      `json:"counts"`
}

type SongAcquisitionSet struct {
	MusicID   int                      `json:"musicId"`
	Providers []ProviderAcquisitionSet `json:"providers"`
}

type AcquisitionSet struct {
	SchemaVersion     int                          `json:"schemaVersion"`
	CanonicalEncoding string                       `json:"canonicalEncoding"`
	DigestAlgorithm   string                       `json:"digestAlgorithm"`
	PlanID            string                       `json:"planId"`
	PlanSHA256        string                       `json:"planSha256"`
	ProviderOrder     []model.LyricsSourceProvider `json:"providerOrder"`
	Songs             []SongAcquisitionSet         `json:"songs"`
	SetSHA256         string                       `json:"setSha256"`
}

func NewAcquisitionSet(
	planID string,
	planSHA256 string,
	providerOrder []model.LyricsSourceProvider,
	songs []SongAcquisitionSet,
) (AcquisitionSet, error) {
	set := AcquisitionSet{
		SchemaVersion: AcquisitionSetSchemaVersionV2, CanonicalEncoding: AcquisitionSetCanonicalEncodingV2,
		DigestAlgorithm: AcquisitionSetDigestAlgorithmV2, PlanID: planID, PlanSHA256: planSHA256,
		ProviderOrder: append([]model.LyricsSourceProvider(nil), providerOrder...),
		Songs:         cloneSongAcquisitionSets(songs),
	}
	if err := validateAcquisitionSet(set, false); err != nil {
		return AcquisitionSet{}, err
	}
	digest, err := acquisitionSetDigest(set)
	if err != nil {
		return AcquisitionSet{}, err
	}
	set.SetSHA256 = digest
	if err := ValidateAcquisitionSet(set); err != nil {
		return AcquisitionSet{}, err
	}
	return cloneAcquisitionSet(set), nil
}

func ValidateAcquisitionSet(set AcquisitionSet) error {
	if err := validateAcquisitionSet(set, true); err != nil {
		return err
	}
	digestInput := cloneAcquisitionSet(set)
	digestInput.SetSHA256 = ""
	digest, err := acquisitionSetDigest(digestInput)
	if err != nil || digest != set.SetSHA256 {
		return errors.New("lyrics recovery acquisition set digest does not match")
	}
	return nil
}

// ValidateAcquisitionSetAuthorization binds every declared song, provider
// prefix, and acquisition to one exact immutable plan invocation.
func ValidateAcquisitionSetAuthorization(
	set AcquisitionSet,
	planID string,
	planSHA256 string,
	authorizedMusicIDs []int,
	authorizedProviderOrder []model.LyricsSourceProvider,
	authorizedProviderMusicIDs map[model.LyricsSourceProvider][]int,
) error {
	if err := ValidateAcquisitionSet(set); err != nil {
		return err
	}
	if set.PlanID != planID || set.PlanSHA256 != planSHA256 ||
		!canonicalAcquisitionSetSHA256.MatchString(planSHA256) || authorizedMusicIDs == nil ||
		len(set.Songs) != len(authorizedMusicIDs) || !equalProviderOrder(set.ProviderOrder, authorizedProviderOrder) {
		return errors.New("lyrics recovery acquisition set does not exactly bind the authorized immutable plan scope")
	}
	providerOrders, err := authorizedProviderOrdersByMusicID(
		authorizedMusicIDs, authorizedProviderOrder, authorizedProviderMusicIDs,
	)
	if err != nil {
		return err
	}
	lastMusicID := 0
	for index, musicID := range authorizedMusicIDs {
		if musicID <= lastMusicID || set.Songs[index].MusicID != musicID ||
			validateOrderedProviderPrefix(providerOrders[musicID], set.Songs[index].Providers) != nil {
			return errors.New("lyrics recovery acquisition set songs or provider prefixes do not exactly match the authorized plan")
		}
		lastMusicID = musicID
	}
	return nil
}

func authorizedProviderOrdersByMusicID(
	musicIDs []int,
	providerOrder []model.LyricsSourceProvider,
	providerMusicIDs map[model.LyricsSourceProvider][]int,
) (map[int][]model.LyricsSourceProvider, error) {
	if len(musicIDs) == 0 || len(providerOrder) == 0 {
		return nil, errors.New("lyrics recovery provider authorization is empty")
	}
	result := make(map[int][]model.LyricsSourceProvider, len(musicIDs))
	allowedMusicIDs := make(map[int]struct{}, len(musicIDs))
	lastMusicID := 0
	for _, musicID := range musicIDs {
		if musicID <= lastMusicID {
			return nil, errors.New("lyrics recovery authorized music IDs are not strictly ordered")
		}
		allowedMusicIDs[musicID] = struct{}{}
		lastMusicID = musicID
	}
	if len(providerMusicIDs) == 0 {
		for _, musicID := range musicIDs {
			result[musicID] = append([]model.LyricsSourceProvider(nil), providerOrder...)
		}
		return result, nil
	}
	seenProviders := make(map[model.LyricsSourceProvider]struct{}, len(providerOrder))
	assigned := make(map[int]model.LyricsSourceProvider, len(musicIDs))
	for _, provider := range providerOrder {
		if !model.IsValidLyricsSourceProvider(provider) {
			return nil, errors.New("lyrics recovery authorized provider order is invalid")
		}
		if _, duplicate := seenProviders[provider]; duplicate {
			return nil, errors.New("lyrics recovery authorized provider order contains a duplicate")
		}
		seenProviders[provider] = struct{}{}
		providerScope, configured := providerMusicIDs[provider]
		if !configured || len(providerScope) == 0 {
			return nil, errors.New("lyrics recovery authorized provider scopes are incomplete")
		}
		lastProviderMusicID := 0
		for _, musicID := range providerScope {
			if musicID <= lastProviderMusicID {
				return nil, errors.New("lyrics recovery authorized provider scope is not strictly ordered")
			}
			lastProviderMusicID = musicID
			if _, allowed := allowedMusicIDs[musicID]; !allowed {
				continue
			}
			if _, duplicate := assigned[musicID]; duplicate {
				return nil, errors.New("lyrics recovery authorized provider scopes overlap")
			}
			assigned[musicID] = provider
			result[musicID] = []model.LyricsSourceProvider{provider}
		}
	}
	if len(providerMusicIDs) != len(seenProviders) || len(assigned) != len(musicIDs) {
		return nil, errors.New("lyrics recovery authorized provider scopes do not exactly partition the song set")
	}
	return result, nil
}

func validateAcquisitionSet(set AcquisitionSet, requireDigest bool) error {
	if set.SchemaVersion != AcquisitionSetSchemaVersionV2 || set.CanonicalEncoding != AcquisitionSetCanonicalEncodingV2 ||
		set.DigestAlgorithm != AcquisitionSetDigestAlgorithmV2 || set.PlanID == "" || len(set.PlanID) > 128 ||
		!canonicalAcquisitionSetSHA256.MatchString(set.PlanSHA256) || len(set.ProviderOrder) == 0 ||
		len(set.ProviderOrder) > 16 || len(set.Songs) == 0 || len(set.Songs) > 10_000 {
		return errors.New("lyrics recovery acquisition set identity is invalid")
	}
	if requireDigest {
		if !canonicalAcquisitionSetSHA256.MatchString(set.SetSHA256) {
			return errors.New("lyrics recovery acquisition set digest is invalid")
		}
	} else if set.SetSHA256 != "" {
		return errors.New("new lyrics recovery acquisition set contains a premature digest")
	}
	orderSeen := make(map[model.LyricsSourceProvider]struct{}, len(set.ProviderOrder))
	for _, provider := range set.ProviderOrder {
		if !model.IsValidLyricsSourceProvider(provider) {
			return errors.New("lyrics recovery acquisition provider order is invalid")
		}
		if _, duplicate := orderSeen[provider]; duplicate {
			return errors.New("lyrics recovery acquisition provider order contains a duplicate")
		}
		orderSeen[provider] = struct{}{}
	}

	seenAcquisitions := make(map[lyricsacquisition.AcquisitionID]struct{})
	lastMusicID := 0
	for _, song := range set.Songs {
		if song.MusicID <= lastMusicID || validateOrderedProviderSelection(set.ProviderOrder, song.Providers) != nil {
			return errors.New("lyrics recovery acquisition songs are not canonically ordered non-empty provider selections")
		}
		lastMusicID = song.MusicID
		for _, provider := range song.Providers {
			if len(provider.AcquisitionIDs) > MaxSongAcquisitionIDs {
				return errors.New("lyrics recovery provider acquisition identity is invalid")
			}
			if err := validateProviderTerminal(provider); err != nil {
				return errors.New("lyrics recovery provider acquisition terminal is invalid")
			}
			seen := make(map[lyricsacquisition.AcquisitionID]struct{}, len(provider.AcquisitionIDs))
			for _, acquisitionID := range provider.AcquisitionIDs {
				if !canonicalAcquisitionSetSHA256.MatchString(string(acquisitionID)) {
					return errors.New("lyrics recovery acquisition ID is invalid")
				}
				if _, duplicate := seen[acquisitionID]; duplicate {
					return errors.New("lyrics recovery provider acquisition IDs contain a duplicate")
				}
				if _, duplicate := seenAcquisitions[acquisitionID]; duplicate {
					return errors.New("lyrics recovery acquisition ID is declared more than once in the exact set")
				}
				seen[acquisitionID] = struct{}{}
				seenAcquisitions[acquisitionID] = struct{}{}
			}
		}
	}
	return nil
}

func (set AcquisitionSet) OrderedProviders(musicID int) ([]ProviderAcquisitionSet, error) {
	if err := ValidateAcquisitionSet(set); err != nil {
		return nil, err
	}
	index := sort.Search(len(set.Songs), func(index int) bool { return set.Songs[index].MusicID >= musicID })
	if index == len(set.Songs) || set.Songs[index].MusicID != musicID {
		return nil, errors.New("lyrics recovery acquisition set is missing the required song")
	}
	return cloneProviderAcquisitionSets(set.Songs[index].Providers), nil
}
func validateProviderTerminal(provider ProviderAcquisitionSet) error {
	candidates := []struct{}{}
	if provider.Status == lyricsprovideroutcome.StatusCandidate {
		candidates = []struct{}{{}}
	}
	_, err := lyricsprovideroutcome.New(
		provider.Provider, provider.Status, candidates,
		lyricsprovideroutcome.Diagnostic{
			Provider: provider.Provider, Phase: provider.Phase,
			ReasonCode: provider.ReasonCode, Counts: provider.Counts,
			AcquisitionRefs: []model.LyricsSourceIndexEvidenceRef{},
		},
	)
	return err
}

func MarshalAcquisitionSet(set AcquisitionSet) ([]byte, error) {
	if err := ValidateAcquisitionSet(set); err != nil {
		return nil, err
	}
	body, err := json.Marshal(set)
	if err != nil || len(body) == 0 || len(body) > MaxAcquisitionSetBytes || !utf8.Valid(body) {
		return nil, errors.New("lyrics recovery acquisition set exceeds its byte boundary")
	}
	return body, nil
}

func DecodeAcquisitionSet(body []byte) (AcquisitionSet, error) {
	if len(body) == 0 || len(body) > MaxAcquisitionSetBytes || !utf8.Valid(body) {
		return AcquisitionSet{}, errors.New("lyrics recovery acquisition set bytes are invalid")
	}
	if err := inspectSongResultJSON(body); err != nil {
		return AcquisitionSet{}, err
	}
	var set AcquisitionSet
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&set); err != nil {
		return AcquisitionSet{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return AcquisitionSet{}, errors.New("lyrics recovery acquisition set contains trailing JSON")
	}
	if err := ValidateAcquisitionSet(set); err != nil {
		return AcquisitionSet{}, err
	}
	canonical, _ := json.Marshal(set)
	if !bytes.Equal(body, canonical) {
		return AcquisitionSet{}, errors.New("lyrics recovery acquisition set is not canonical JSON")
	}
	return cloneAcquisitionSet(set), nil
}

func PublishAcquisitionSet(path string, set AcquisitionSet) error {
	body, err := MarshalAcquisitionSet(set)
	if err != nil {
		return err
	}
	return publishPrivateFile(path, body, func(candidate []byte) error {
		_, err := DecodeAcquisitionSet(candidate)
		return err
	})
}

func OpenAcquisitionSet(path string) (AcquisitionSet, error) {
	body, err := readPrivateFile(path, MaxAcquisitionSetBytes, 1)
	if err != nil {
		return AcquisitionSet{}, err
	}
	return DecodeAcquisitionSet(body)
}

func acquisitionSetDigest(set AcquisitionSet) (string, error) {
	body, err := json.Marshal(set)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-recovery-acquisition-set-v2\x00"))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func cloneProviderAcquisitionSets(input []ProviderAcquisitionSet) []ProviderAcquisitionSet {
	result := append([]ProviderAcquisitionSet(nil), input...)
	for index := range result {
		if input[index].AcquisitionIDs == nil {
			result[index].AcquisitionIDs = nil
		} else {
			result[index].AcquisitionIDs = append([]lyricsacquisition.AcquisitionID{}, input[index].AcquisitionIDs...)
		}
	}
	return result
}

func cloneSongAcquisitionSets(input []SongAcquisitionSet) []SongAcquisitionSet {
	result := append([]SongAcquisitionSet(nil), input...)
	for songIndex := range result {
		result[songIndex].Providers = cloneProviderAcquisitionSets(input[songIndex].Providers)
	}
	return result
}

func cloneAcquisitionSet(set AcquisitionSet) AcquisitionSet {
	set.ProviderOrder = append([]model.LyricsSourceProvider(nil), set.ProviderOrder...)
	set.Songs = cloneSongAcquisitionSets(set.Songs)
	return set
}

func equalProviderOrder(left, right []model.LyricsSourceProvider) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
