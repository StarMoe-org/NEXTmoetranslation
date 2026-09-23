package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	LyricsCatalogIdentityPolicyVersion = "catalog-identity-v2"
	LyricsMatchingPolicyVersion        = "mandatory-gates-v1"
	LyricsRestrictionPolicyVersion     = "wiki-restrictions-v1"
	LyricsExtractorVersion             = "wiki-lyrics-v2-sekai-ruby-colors"
	LyricsReviewPolicyVersion          = "private-review-v1"

	// LyricsCatalogTargetFullTarget names the catalog anchor used to fetch and
	// analyze a complete external source artifact. It does not prove that the
	// catalog chart represented by that record is itself full length.
	LyricsCatalogTargetFullTarget       = "full_target"
	LyricsCatalogTargetGameSizeEvidence = "game_size_evidence"
	LyricsCatalogTargetReview           = "review"
)

type CatalogVocalSignal struct {
	VocalID           int    `json:"vocalId"`
	VocalType         string `json:"vocalType,omitempty"`
	Caption           string `json:"caption,omitempty"`
	AssetbundleName   string `json:"assetbundleName,omitempty"`
	CharacterType     string `json:"characterType,omitempty"`
	CharacterID       int    `json:"characterId,omitempty"`
	CharacterSequence int    `json:"characterSequence,omitempty"`
}

// CatalogVocalSignalsAreInstrumental returns true only when the immutable
// catalog supplies a non-empty vocal set and every exact vocalType is the
// canonical instrumental marker. Mixed, missing, or loosely normalized input
// remains non-instrumental and must fail through the ordinary lyrics path.
func CatalogVocalSignalsAreInstrumental(vocals []CatalogVocalSignal) bool {
	if len(vocals) == 0 {
		return false
	}
	for _, vocal := range vocals {
		if vocal.VocalType != "instrumental" {
			return false
		}
	}
	return true
}

// CatalogLyricsAreInstrumental returns true only when exact instrumental vocal
// signals are paired with the absence of a lyricist. A non-empty role-bound
// lyricist is authoritative evidence that the song must continue through the
// ordinary lyrics path even when the game asset is labelled instrumental.
func CatalogLyricsAreInstrumental(vocals []CatalogVocalSignal, lyricist string) bool {
	return strings.TrimSpace(lyricist) == "" && CatalogVocalSignalsAreInstrumental(vocals)
}

type CatalogLyricsEvidence struct {
	PolicyVersion string                  `json:"policyVersion"`
	Title         string                  `json:"title"`
	Lyricist      string                  `json:"lyricist,omitempty"`
	Composer      string                  `json:"composer,omitempty"`
	Arranger      string                  `json:"arranger,omitempty"`
	Assetbundle   string                  `json:"assetbundle,omitempty"`
	VersionHint   string                  `json:"versionHint,omitempty"`
	LyricsVersion string                  `json:"lyricsVersion"`
	Vocals        []CatalogVocalSignal    `json:"vocals"`
	Presence      CatalogEvidencePresence `json:"presence"`
}

type CatalogEvidencePresence struct {
	Lyricist      bool `json:"lyricist"`
	Composer      bool `json:"composer"`
	Arranger      bool `json:"arranger"`
	Assetbundle   bool `json:"assetbundle"`
	VersionHint   bool `json:"versionHint"`
	LyricsVersion bool `json:"lyricsVersion"`
	Vocals        bool `json:"vocals"`
}

type CatalogLyricsGroupingRecord struct {
	MusicID     int
	Fingerprint string
	Evidence    CatalogLyricsEvidence
}

type CatalogLyricsTarget struct {
	MusicID             int
	CatalogFingerprint  string
	Disposition         string
	TargetMusicID       int
	AssociationMusicIDs []int
	ReasonCode          string
}

func NormalizeCatalogLyricsEvidence(input CatalogLyricsEvidence) CatalogLyricsEvidence {
	input.PolicyVersion = LyricsCatalogIdentityPolicyVersion
	input.Title = normalizeLyricsIdentityText(input.Title)
	input.Lyricist = normalizeLyricsIdentityText(input.Lyricist)
	input.Composer = normalizeLyricsIdentityText(input.Composer)
	input.Arranger = normalizeLyricsIdentityText(input.Arranger)
	input.Assetbundle = normalizeLyricsIdentityText(input.Assetbundle)
	input.VersionHint = normalizeLyricsIdentityText(input.VersionHint)
	input.LyricsVersion = strings.ToLower(normalizeLyricsIdentityText(input.LyricsVersion))
	if !input.Presence.LyricsVersion {
		input.LyricsVersion = "unknown"
	}
	vocals := make([]CatalogVocalSignal, 0, len(input.Vocals))
	seen := map[CatalogVocalSignal]struct{}{}
	for _, vocal := range input.Vocals {
		vocal.VocalType = normalizeLyricsIdentityText(vocal.VocalType)
		vocal.Caption = normalizeLyricsIdentityText(vocal.Caption)
		vocal.AssetbundleName = normalizeLyricsIdentityText(vocal.AssetbundleName)
		vocal.CharacterType = normalizeLyricsIdentityText(vocal.CharacterType)
		if vocal.VocalID <= 0 && vocal.VocalType == "" && vocal.Caption == "" && vocal.AssetbundleName == "" &&
			vocal.CharacterType == "" && vocal.CharacterID <= 0 && vocal.CharacterSequence <= 0 {
			continue
		}
		if _, exists := seen[vocal]; exists {
			continue
		}
		seen[vocal] = struct{}{}
		vocals = append(vocals, vocal)
	}
	sort.Slice(vocals, func(i, j int) bool {
		left, right := vocals[i], vocals[j]
		if left.VocalID != right.VocalID {
			return left.VocalID < right.VocalID
		}
		if left.CharacterSequence != right.CharacterSequence {
			return left.CharacterSequence < right.CharacterSequence
		}
		if left.CharacterType != right.CharacterType {
			return left.CharacterType < right.CharacterType
		}
		if left.CharacterID != right.CharacterID {
			return left.CharacterID < right.CharacterID
		}
		if left.VocalType != right.VocalType {
			return left.VocalType < right.VocalType
		}
		if left.Caption != right.Caption {
			return left.Caption < right.Caption
		}
		return left.AssetbundleName < right.AssetbundleName
	})
	input.Vocals = vocals
	return input
}

func CatalogLyricsEvidenceFingerprint(input CatalogLyricsEvidence) (string, error) {
	canonical := NormalizeCatalogLyricsEvidence(input)
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

// ClassifyCatalogLyricsTargets applies mandatory, lexicographic gates. A group
// is automatic only when every record has complete, consistent role-bound
// credits and every version is explicit and supported. One explicit full
// record remains the anchor; otherwise an all-game-size group elects its
// lowest music ID as the deterministic anchor for a complete external source.
func ClassifyCatalogLyricsTargets(records []CatalogLyricsGroupingRecord) []CatalogLyricsTarget {
	groups := map[string][]CatalogLyricsGroupingRecord{}
	for _, record := range records {
		if record.MusicID <= 0 {
			continue
		}
		record.Evidence = NormalizeCatalogLyricsEvidence(record.Evidence)
		workKey := normalizeLyricsWorkText(record.Evidence.Title)
		groups[workKey] = append(groups[workKey], record)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]CatalogLyricsTarget, 0, len(records))
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool { return group[i].MusicID < group[j].MusicID })
		if reason, explicit := catalogExplicitNoLyricsReason(group); explicit {
			for _, record := range group {
				result = append(result, CatalogLyricsTarget{
					MusicID: record.MusicID, CatalogFingerprint: record.Fingerprint,
					Disposition: LyricsCatalogTargetReview, ReasonCode: reason,
				})
			}
			continue
		}
		fullIndex := -1
		fullCount := 0
		invalid := false
		reason := ""
		baselineRequiredCredits := ""
		baselineArranger := ""
		for index, record := range group {
			presence := record.Evidence.Presence
			// Title + role-bound lyricist + role-bound composer are sufficient
			// catalog identity for a vocal song. Arrangement is useful evidence
			// when the catalog supplies it, but many authoritative music records
			// omit that optional credit; its absence must not block lyrics work.
			creditsComplete := presence.Lyricist && presence.Composer &&
				record.Evidence.Lyricist != "" && record.Evidence.Composer != ""
			if !creditsComplete {
				invalid = true
				if reason == "" {
					reason = "missing_role_bound_credits"
				}
			} else {
				requiredCredits := normalizeLyricsWorkText(record.Evidence.Lyricist + "\x1f" + record.Evidence.Composer)
				if baselineRequiredCredits == "" {
					baselineRequiredCredits = requiredCredits
				} else if requiredCredits != baselineRequiredCredits {
					invalid = true
					reason = "role_bound_credits_conflict"
				}
				if presence.Arranger && record.Evidence.Arranger != "" {
					arranger := normalizeLyricsWorkText(record.Evidence.Arranger)
					if baselineArranger == "" {
						baselineArranger = arranger
					} else if arranger != baselineArranger {
						invalid = true
						reason = "role_bound_credits_conflict"
					}
				}
			}
			if !presence.LyricsVersion {
				invalid = true
				if reason == "" {
					reason = "missing_explicit_version"
				}
				continue
			}
			switch record.Evidence.LyricsVersion {
			case "full":
				fullCount++
				fullIndex = index
				if fullCount > 1 {
					invalid = true
					reason = "multiple_explicit_full_targets"
				}
			case "game_size":
			default:
				invalid = true
				if reason == "" {
					reason = "unsupported_or_unknown_version"
				}
			}
		}
		if invalid {
			for _, record := range group {
				result = append(result, CatalogLyricsTarget{MusicID: record.MusicID, CatalogFingerprint: record.Fingerprint, Disposition: LyricsCatalogTargetReview, ReasonCode: reason})
			}
			continue
		}
		anchorIndex := fullIndex
		if fullCount == 0 {
			anchorIndex = 0
		}
		anchor := group[anchorIndex]
		associations := make([]int, 0, len(group)-1)
		for _, record := range group {
			if record.MusicID != anchor.MusicID {
				associations = append(associations, record.MusicID)
			}
		}
		for _, record := range group {
			disposition := LyricsCatalogTargetGameSizeEvidence
			if record.MusicID == anchor.MusicID {
				disposition = LyricsCatalogTargetFullTarget
			}
			result = append(result, CatalogLyricsTarget{
				MusicID: record.MusicID, CatalogFingerprint: record.Fingerprint, Disposition: disposition,
				TargetMusicID: anchor.MusicID, AssociationMusicIDs: append([]int(nil), associations...),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].MusicID < result[j].MusicID })
	return result
}

func catalogExplicitNoLyricsReason(group []CatalogLyricsGroupingRecord) (string, bool) {
	if len(group) == 0 {
		return "", false
	}
	reason := ""
	for _, record := range group {
		current := ""
		switch {
		case isCatalogMedleyTitle(record.Evidence.Title) && !catalogHasRoleBoundLyricsCredits(record.Evidence):
			current = "medley_composite_source"
		case isCatalogInstrumentalEvidence(record.Evidence):
			current = "instrumental_no_vocals"
		default:
			return "", false
		}
		if reason != "" && reason != current {
			return "", false
		}
		reason = current
	}
	return reason, reason != ""
}

func catalogHasRoleBoundLyricsCredits(evidence CatalogLyricsEvidence) bool {
	canonical := NormalizeCatalogLyricsEvidence(evidence)
	return canonical.Presence.Lyricist && canonical.Presence.Composer &&
		canonical.Lyricist != "" && canonical.Composer != ""
}

func isCatalogInstrumentalEvidence(evidence CatalogLyricsEvidence) bool {
	if !evidence.Presence.Vocals {
		return false
	}
	canonical := NormalizeCatalogLyricsEvidence(evidence)
	return CatalogLyricsAreInstrumental(canonical.Vocals, canonical.Lyricist)
}

func isCatalogMedleyTitle(title string) bool {
	value := strings.ToLower(norm.NFKC.String(title))
	if strings.Contains(value, "メドレー") {
		return true
	}
	for _, field := range strings.FieldsFunc(value, func(current rune) bool {
		return !unicode.IsLetter(current) && !unicode.IsNumber(current)
	}) {
		if field == "medley" {
			return true
		}
	}
	return false
}

func normalizeLyricsIdentityText(value string) string {
	value = norm.NFKC.String(strings.TrimSpace(value))
	if value == "" || value == "-" {
		return ""
	}
	return strings.Join(strings.Fields(value), " ")
}

func normalizeLyricsWorkText(value string) string {
	value = strings.ToLower(norm.NFKC.String(value))
	var builder strings.Builder
	for _, current := range value {
		if unicode.IsLetter(current) || unicode.IsNumber(current) || current == '=' || current == '?' {
			builder.WriteRune(current)
		}
	}
	return builder.String()
}

type LyricsSourceVersion struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type LyricsSourcePerformer struct {
	PerformerID string `json:"performerId"`
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
}

type LyricsSourceRubySpan struct {
	Text            string                       `json:"text"`
	Reading         string                       `json:"reading,omitempty"`
	ReadingEvidence *LyricsSourceReadingEvidence `json:"readingEvidence,omitempty"`
}

type LyricsSourceSegment struct {
	Text         string                 `json:"text"`
	PerformerIDs []string               `json:"performerIds"`
	Ruby         []LyricsSourceRubySpan `json:"ruby"`
}

type LyricsSourceExtractedLine struct {
	Japanese             string                `json:"japanese"`
	StanzaBreakBefore    bool                  `json:"stanzaBreakBefore,omitempty"`
	Segments             []LyricsSourceSegment `json:"segments"`
	TrailingPerformerIDs []string              `json:"trailingPerformerIds"`
}

type LyricsSourceEvidence struct {
	RuleID  string `json:"ruleId"`
	Gate    string `json:"gate"`
	Outcome string `json:"outcome"`
	Summary string `json:"summary"`
}

func LyricsSourceExtractedLinesSHA256(lines []LyricsSourceExtractedLine) string {
	payload, _ := json.Marshal(struct {
		Version int                         `json:"version"`
		Lines   []LyricsSourceExtractedLine `json:"lines"`
	}{Version: 2, Lines: lines})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
