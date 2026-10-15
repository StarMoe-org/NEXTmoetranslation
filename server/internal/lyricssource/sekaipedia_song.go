package lyricssource

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

type sekaipediaPrimarySourceSide struct {
	side         string
	topLabel     string
	nestedLabel  string
	body         string
	explicitKind string
}

type sekaipediaParsedPrimarySide struct {
	source sekaipediaPrimarySourceSide
	parsed sekaipediaRenditionExtraction
	kind   string
}

// sekaipediaLyricsSection returns the Lyrics section when it contains a real
// lyrics layout, and otherwise falls back to the first following top-level
// section before Versions that does. Some pages (for example Tenbin, Yubisaki
// de Furete) keep a translation stub under Lyrics and place the actual
// Game/Full tabber under a later song-title section.
func sekaipediaLyricsSection(content string) (string, error) {
	content = strings.ReplaceAll(content, "\r", "")
	matches, err := sekaipediaActiveTopLevelHeadings(content)
	if err != nil {
		return "", err
	}
	lyricsIndex := -1
	for index, match := range matches {
		if strings.TrimSpace(content[match[2]:match[3]]) == "Lyrics" {
			if lyricsIndex >= 0 {
				return "", ErrAmbiguous
			}
			lyricsIndex = index
		}
	}
	if lyricsIndex < 0 {
		return "", ErrMissingLyrics
	}
	sectionAt := func(index int) string {
		end := len(content)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		return content[matches[index][1]:end]
	}
	for index := lyricsIndex; index < len(matches); index++ {
		if index > lyricsIndex && strings.TrimSpace(content[matches[index][2]:matches[index][3]]) == "Versions" {
			break
		}
		section := sectionAt(index)
		if _, _, parseErr := parseSekaipediaLyricsLayout(section); parseErr == nil {
			return section, nil
		}
	}
	return sectionAt(lyricsIndex), nil
}

// parseSekaipediaSong first closes the fixed Versions rows and exact tab tree.
// Catalog policy is consulted only for a singular compatibility view after all
// source-evidenced peer renditions have been retained.
func parseSekaipediaSong(content string, policy PerformerSegmentationPolicy) (sekaipediaSongExtraction, error) {
	if !utf8ValidBounded(content, maxResponseBytes) {
		return sekaipediaSongExtraction{}, ErrMalformedResponse
	}
	if policy != PerformerSegmentationDisabled && policy != PerformerSegmentationSekaiEligible {
		return sekaipediaSongExtraction{}, ErrMalformedResponse
	}
	lyricsSection, err := sekaipediaLyricsSection(content)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	versionsSection, err := sekaipediaTopLevelSection(content, "Versions")
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	records, err := parseSekaipediaVersions(versionsSection)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	tabs, sameLyrics, err := parseSekaipediaLyricsTabs(lyricsSection)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	primarySides, err := sekaipediaPrimarySourceSides(tabs)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	if sekaipediaPrimarySidesArePlural(primarySides) {
		return parseSekaipediaPluralSong(records, tabs, primarySides, sameLyrics, policy)
	}

	// Existing one-rendition layouts retain their exact v2 compatibility view,
	// while the native v3 rendition is populated from the already parsed source
	// paths rather than reconstructed from version labels or identity sections.
	legacy, err := parseSekaipediaSongLegacy(content, policy)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	peer, err := sekaipediaPeerFromLegacy(legacy, tabs)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	legacy.Renditions = []sekaipediaPeerRenditionExtraction{peer}
	return legacy, nil
}

func sekaipediaPrimarySourceSides(tabs map[string]string) ([]sekaipediaPrimarySourceSide, error) {
	sides := make([]sekaipediaPrimarySourceSide, 0, 4)
	appendTop := func(side, label, body string) error {
		body = strings.TrimSpace(body)
		if body == "" {
			return nil
		}
		nested, err := parseSekaipediaNestedRenditionTabs(body)
		if err != nil {
			return err
		}
		labels := make([]string, 0, len(nested))
		for nestedLabel := range nested {
			labels = append(labels, nestedLabel)
		}
		sort.Strings(labels)
		added := 0
		for _, nestedLabel := range labels {
			explicitKind := sekaipediaPrimaryLabelKind(nestedLabel)
			if nestedLabel != "" && explicitKind == "" {
				continue
			}
			sides = append(sides, sekaipediaPrimarySourceSide{
				side: side, topLabel: label, nestedLabel: nestedLabel,
				body: nested[nestedLabel], explicitKind: explicitKind,
			})
			added++
		}
		if added == 0 {
			// A sole non-rendition nested template may be formatting internal to
			// one source side. Keep the exact top-level body as the primary path;
			// the legacy bounded parser still validates its internal structure.
			sides = append(sides, sekaipediaPrimarySourceSide{
				side: side, topLabel: label, body: body,
			})
		}
		return nil
	}
	if tabs["Full Version"] != "" && tabs["APPEND/Full Version"] != "" {
		return nil, ErrAmbiguous
	}
	for _, top := range []struct {
		side  string
		label string
	}{
		{side: "full", label: "Full Version"},
		{side: "full", label: "APPEND/Full Version"},
		{side: "game", label: "Game Version"},
	} {
		if err := appendTop(top.side, top.label, tabs[top.label]); err != nil {
			return nil, err
		}
	}
	for _, top := range []struct {
		label string
		kind  string
	}{
		{label: "SEKAI", kind: "sekai"},
		{label: "VIRTUAL SINGER", kind: "vocaloid"},
	} {
		if strings.TrimSpace(tabs[top.label]) == "" {
			continue
		}
		sides = append(sides, sekaipediaPrimarySourceSide{
			side: "full", topLabel: top.label, body: tabs[top.label], explicitKind: top.kind,
		})
	}
	if len(sides) == 0 {
		return nil, ErrMissingLyrics
	}
	return sides, nil
}

// sekaipediaPrimaryNestedLabelKey maps a nested rendition tab label to the
// closed primary-label key. The source pages contain a small number of
// misspelled or pluralized vocaloid labels (VRITUAL SINGER, VIRTUAL SINGERS,
// VIRUTAL SINGER); those still select the same VIRTUAL SINGER rendition rather
// than being treated as unknown alternate tabs.
func sekaipediaPrimaryNestedLabelKey(label string) string {
	label = strings.ToLower(strings.Join(strings.Fields(label), " "))
	switch label {
	case "sekai":
		return "sekai"
	case "virtual singer", "virtual singers", "vritual singer", "virutal singer":
		return "virtual singer"
	default:
		return label
	}
}

func sekaipediaPrimaryLabelKind(label string) string {
	switch sekaipediaPrimaryNestedLabelKey(label) {
	case "sekai":
		return "sekai"
	case "virtual singer":
		return "vocaloid"
	default:
		return ""
	}
}

func sekaipediaPrimarySidesArePlural(sides []sekaipediaPrimarySourceSide) bool {
	kinds := map[string]struct{}{}
	for _, side := range sides {
		if side.explicitKind != "" {
			kinds[side.explicitKind] = struct{}{}
		}
	}
	return len(kinds) > 1
}

func parseSekaipediaPluralSong(
	records []sekaipediaVersionRecord,
	tabs map[string]string,
	sides []sekaipediaPrimarySourceSide,
	sameLyrics bool,
	policy PerformerSegmentationPolicy,
) (sekaipediaSongExtraction, error) {
	parsedSides := make([]sekaipediaParsedPrimarySide, 0, len(sides))
	for _, side := range sides {
		parsed, kind, err := parseSekaipediaPrimarySide(records, side, policy)
		if err != nil {
			return sekaipediaSongExtraction{}, fmt.Errorf("primary %s tab parse: %w", side.side, err)
		}
		parsedSides = append(parsedSides, sekaipediaParsedPrimarySide{source: side, parsed: parsed, kind: kind})
	}

	_, singersByID, err := buildSekaipediaSingerAliases(sekaipediaSingers)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	peersByKind := map[string]*sekaipediaPeerRenditionExtraction{}
	for _, side := range parsedSides {
		sourcePerformerIDs, ok := sekaipediaPersistedPerformerIDs(side.parsed.set.ids, singersByID)
		if !ok || len(sourcePerformerIDs) == 0 {
			return sekaipediaSongExtraction{}, ErrUnsupportedTable
		}
		sort.Strings(sourcePerformerIDs)
		peer := peersByKind[side.kind]
		if peer == nil {
			peer = &sekaipediaPeerRenditionExtraction{
				RenditionKey: side.kind, Kind: side.kind,
				ReasonCode:         model.LyricsSourceVersionReasonUntaggedFullOnly,
				SourcePerformerIDs: sourcePerformerIDs,
			}
			peersByKind[side.kind] = peer
		} else if !stringSlicesEqual(peer.SourcePerformerIDs, sourcePerformerIDs) {
			return sekaipediaSongExtraction{}, ErrAmbiguous
		}
		path := model.LyricsSourceTabPath{side.source.topLabel}
		if side.source.nestedLabel != "" {
			path = append(path, side.source.nestedLabel)
		}
		peer.SourceTabPaths = append(peer.SourceTabPaths, path)
		section := "Lyrics/" + strings.Join([]string(path), "/")
		extraction := side.parsed.extraction
		switch side.source.side {
		case "full":
			if peer.Full != nil {
				return sekaipediaSongExtraction{}, ErrAmbiguous
			}
			peer.Full = &extraction
			peer.FullSection = section
			peer.FullStructuredEvidence = sekaipediaStructuredEvidenceState(side.parsed)
			peer.fullProjectionLines = append([]sekaipediaColumnLine(nil), side.parsed.projectionLines...)
		case "game":
			if peer.Game != nil {
				return sekaipediaSongExtraction{}, ErrAmbiguous
			}
			peer.Game = &extraction
			peer.GameSection = section
			peer.GameStructuredEvidence = sekaipediaStructuredEvidenceState(side.parsed)
			peer.gameProjectionLines = append([]sekaipediaColumnLine(nil), side.parsed.projectionLines...)
		default:
			return sekaipediaSongExtraction{}, ErrUnsupportedTable
		}
	}

	keys := make([]string, 0, len(peersByKind))
	for key := range peersByKind {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	peers := make([]sekaipediaPeerRenditionExtraction, 0, len(keys))
	for _, key := range keys {
		peer := *peersByKind[key]
		switch {
		case peer.Full != nil && peer.Game != nil:
			peer.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
			if mapping, projectionErr := sekaipediaResolveProjection(peer.fullProjectionLines, peer.gameProjectionLines); projectionErr == nil {
				fullSelection := sekaipediaRenditionExtraction{extraction: *peer.Full, projectionLines: peer.fullProjectionLines}
				gameSelection := sekaipediaRenditionExtraction{extraction: *peer.Game, projectionLines: peer.gameProjectionLines}
				if sekaipediaExactGameProjectionCompatible(fullSelection, gameSelection, mapping) {
					peer.GameLineIndexes = mapping
				}
			}
		case peer.Full == nil && peer.Game != nil:
			if sameLyrics {
				full := *peer.Game
				full.Version.Label = sekaipediaVersionLabel(peer.Kind)
				peer.Full = &full
				peer.FullSection = peer.GameSection
				peer.FullStructuredEvidence = peer.GameStructuredEvidence
				peer.Game = nil
				peer.GameSection = ""
				peer.GameStructuredEvidence = sekaipediaPerformerEvidenceNone
				peer.GameLineIndexes = make([]int, len(full.Lines))
				for index := range peer.GameLineIndexes {
					peer.GameLineIndexes[index] = index
				}
				peer.ReasonCode = model.LyricsSourceVersionReasonUntaggedUncutIdentity
			} else {
				peer.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnly
			}
		case peer.Full != nil:
			if sameLyrics {
				peer.GameLineIndexes = make([]int, len(peer.Full.Lines))
				for index := range peer.GameLineIndexes {
					peer.GameLineIndexes[index] = index
				}
				peer.ReasonCode = model.LyricsSourceVersionReasonUntaggedUncutIdentity
			}
		default:
			return sekaipediaSongExtraction{}, ErrMissingLyrics
		}
		peers = append(peers, peer)
	}

	alternates, err := parseSekaipediaAlternateVocals(tabs, records)
	if err != nil {
		return sekaipediaSongExtraction{}, fmt.Errorf("alternate vocal extraction: %w", err)
	}
	alternates, err = sekaipediaScopeAlternateVocalRenditions(alternates)
	if err != nil {
		return sekaipediaSongExtraction{}, fmt.Errorf("alternate vocal singer scope: %w", err)
	}
	result, err := sekaipediaCompatibilityView(peers, policy)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	result.Renditions = peers
	result.AlternateVocals = alternates
	return result, nil
}

func parseSekaipediaPrimarySide(
	records []sekaipediaVersionRecord,
	side sekaipediaPrimarySourceSide,
	policy PerformerSegmentationPolicy,
) (sekaipediaRenditionExtraction, string, error) {
	kinds := []string{side.explicitKind}
	if side.explicitKind == "" {
		kinds = kinds[:0]
		for _, kind := range []string{"original", "sekai", "vocaloid"} {
			if sekaipediaHasVersionKind(records, kind) {
				kinds = append(kinds, kind)
			}
		}
	}
	type match struct {
		kind   string
		parsed sekaipediaRenditionExtraction
	}
	matches := make([]match, 0, len(kinds))
	var firstErr error
	for _, kind := range kinds {
		sets, err := sekaipediaVersionSets(records, kind)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		parsed, err := parseSekaipediaRenditionAgainstSets(side.body, kind, sets, side.side == "full")
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		matches = append(matches, match{kind: kind, parsed: parsed})
	}
	if len(matches) == 0 {
		if firstErr != nil {
			return sekaipediaRenditionExtraction{}, "", firstErr
		}
		return sekaipediaRenditionExtraction{}, "", ErrMissingLyrics
	}
	if len(matches) == 1 {
		return matches[0].parsed, matches[0].kind, nil
	}
	// Catalog assertions resolve only an otherwise source-equivalent unlabeled
	// side; explicit source-labelled peers have already been retained.
	preferred := []string{"sekai", "vocaloid", "original"}
	if policy == PerformerSegmentationDisabled {
		preferred = []string{"vocaloid", "original", "sekai"}
	}
	for _, kind := range preferred {
		for _, match := range matches {
			if match.kind == kind {
				return match.parsed, match.kind, nil
			}
		}
	}
	return sekaipediaRenditionExtraction{}, "", ErrAmbiguous
}

func sekaipediaStructuredEvidenceState(parsed sekaipediaRenditionExtraction) sekaipediaPerformerEvidenceState {
	if !parsed.sourceTagged || !extractionHasPerformerSegmentation(parsed.extraction) {
		return sekaipediaPerformerEvidenceNone
	}
	if extractionHasCompletePerformerSegmentation(parsed.extraction) &&
		sekaipediaSingerSetExactlyWitnessed(parsed.set, parsed.usedIDs) {
		return sekaipediaPerformerEvidenceComplete
	}
	return sekaipediaPerformerEvidencePartial
}

func sekaipediaExtractionPerformerRosterMatchesSingerSet(
	extraction Extraction,
	set sekaipediaSingerSet,
) bool {
	return sekaipediaExtractionPerformerRosterMatchesIDs(&extraction, set.ids)
}

func sekaipediaExtractionPerformerRosterMatchesIDs(
	extraction *Extraction,
	performerIDs []string,
) bool {
	if extraction == nil || len(extraction.Performers) != len(performerIDs) || len(performerIDs) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(extraction.Performers))
	for _, performer := range extraction.Performers {
		seen[performer.PerformerID] = struct{}{}
	}
	if len(seen) != len(performerIDs) {
		return false
	}
	for _, id := range performerIDs {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

func sekaipediaSingerSetExactlyWitnessed(set sekaipediaSingerSet, used map[string]struct{}) bool {
	if len(set.ids) == 0 || len(set.ids) != len(used) {
		return false
	}
	for _, id := range set.ids {
		if _, ok := used[id]; !ok {
			return false
		}
	}
	return true
}

func sekaipediaVersionLabel(kind string) string {
	switch kind {
	case "sekai":
		return "SEKAI Version"
	case "vocaloid":
		return "VIRTUAL SINGER Version"
	case "original":
		return "Original Version"
	default:
		return ""
	}
}

func sekaipediaCompatibilityView(
	peers []sekaipediaPeerRenditionExtraction,
	policy PerformerSegmentationPolicy,
) (sekaipediaSongExtraction, error) {
	if len(peers) == 0 {
		return sekaipediaSongExtraction{}, ErrMissingLyrics
	}
	preferredKinds := []string{"sekai", "vocaloid", "original"}
	if policy == PerformerSegmentationDisabled {
		preferredKinds = []string{"vocaloid", "original", "sekai"}
	}
	var selected *sekaipediaPeerRenditionExtraction
	for _, requireFull := range []bool{true, false} {
		for _, kind := range preferredKinds {
			for index := range peers {
				if peers[index].Kind == kind && (peers[index].Full != nil) == requireFull {
					selected = &peers[index]
					break
				}
			}
			if selected != nil {
				break
			}
		}
		if selected != nil {
			break
		}
	}
	if selected == nil {
		return sekaipediaSongExtraction{}, ErrMissingLyrics
	}
	result := sekaipediaSongExtraction{
		Section: selected.FullSection, GameSection: selected.GameSection,
		RenditionKey: fullRenditionKey(selected.Kind), ReasonCode: selected.ReasonCode,
		GameLineIndexes: append([]int(nil), selected.GameLineIndexes...),
	}
	if selected.Full != nil {
		result.Full = *selected.Full
		result.Game = selected.Game
		result.AuthoritativeStructured = selected.FullStructuredEvidence == sekaipediaPerformerEvidenceComplete
		return result, nil
	}
	if selected.Game == nil {
		return sekaipediaSongExtraction{}, ErrMissingLyrics
	}
	result.Full = *selected.Game // legacy fixed-wikitext compatibility only
	result.Game = selected.Game
	result.Section = selected.GameSection
	result.GameSection = selected.GameSection
	result.RenditionKey = "game-" + selected.Kind
	result.AuthoritativeStructured = selected.GameStructuredEvidence == sekaipediaPerformerEvidenceComplete
	return result, nil
}

func sekaipediaPeerFromLegacy(
	legacy sekaipediaSongExtraction,
	tabs map[string]string,
) (sekaipediaPeerRenditionExtraction, error) {
	gameOnly := strings.HasPrefix(legacy.RenditionKey, "game-")
	kind := strings.TrimPrefix(strings.TrimPrefix(legacy.RenditionKey, "full-"), "game-")
	if kind == "" {
		return sekaipediaPeerRenditionExtraction{}, ErrUnsupportedTable
	}
	peer := sekaipediaPeerRenditionExtraction{
		RenditionKey: kind, Kind: kind, ReasonCode: legacy.ReasonCode,
		GameLineIndexes: append([]int(nil), legacy.GameLineIndexes...),
	}
	var fullPath, gamePath model.LyricsSourceTabPath
	var err error
	if !gameOnly && legacy.Game == nil &&
		legacy.ReasonCode == model.LyricsSourceVersionReasonUntaggedUncutIdentity &&
		strings.TrimSpace(tabs["Game Version"]) != "" {
		// A fixed same-lyrics note makes the visible Game tab the authoritative
		// complete Full text plus an identity projection. Retain that exact source
		// path instead of requiring a nonexistent Full tab.
		_, fullPath, err = sekaipediaLegacyPrimaryPaths(tabs, kind, true, false)
	} else {
		fullPath, gamePath, err = sekaipediaLegacyPrimaryPaths(tabs, kind, gameOnly, legacy.Game != nil)
	}
	if err != nil {
		return sekaipediaPeerRenditionExtraction{}, err
	}
	if gameOnly {
		game := *legacy.Game
		peer.Game = &game
		peer.GameSection = "Lyrics/" + strings.Join([]string(gamePath), "/")
		peer.SourceTabPaths = append(peer.SourceTabPaths, gamePath)
		peer.GameStructuredEvidence = sekaipediaExtractionEvidenceState(game, legacy.AuthoritativeStructured)
		if peer.GameStructuredEvidence == sekaipediaPerformerEvidenceComplete {
			peer.SourcePerformerIDs = sekaipediaExtractionSourcePerformerIDs(nil, &game)
		}
		return peer, nil
	}
	full := legacy.Full
	peer.Full = &full
	peer.FullSection = "Lyrics/" + strings.Join([]string(fullPath), "/")
	peer.SourceTabPaths = append(peer.SourceTabPaths, fullPath)
	peer.FullStructuredEvidence = sekaipediaExtractionEvidenceState(full, legacy.AuthoritativeStructured)
	if legacy.Game != nil {
		game := *legacy.Game
		peer.Game = &game
		peer.GameSection = "Lyrics/" + strings.Join([]string(gamePath), "/")
		peer.SourceTabPaths = append(peer.SourceTabPaths, gamePath)
		peer.GameStructuredEvidence = sekaipediaExtractionEvidenceState(game, legacy.AuthoritativeStructured)
	}
	if peer.FullStructuredEvidence == sekaipediaPerformerEvidenceComplete ||
		peer.GameStructuredEvidence == sekaipediaPerformerEvidenceComplete {
		peer.SourcePerformerIDs = sekaipediaExtractionSourcePerformerIDs(peer.Full, peer.Game)
	}
	return peer, nil
}

func sekaipediaExtractionSourcePerformerIDs(full, game *Extraction) []string {
	seen := map[string]struct{}{}
	for _, extraction := range []*Extraction{full, game} {
		if extraction == nil {
			continue
		}
		for _, performer := range extraction.Performers {
			seen[performer.PerformerID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func sekaipediaExtractionEvidenceState(extraction Extraction, complete bool) sekaipediaPerformerEvidenceState {
	if !extractionHasPerformerSegmentation(extraction) {
		return sekaipediaPerformerEvidenceNone
	}
	if complete && extractionHasCompletePerformerSegmentation(extraction) {
		return sekaipediaPerformerEvidenceComplete
	}
	return sekaipediaPerformerEvidencePartial
}

func sekaipediaLegacyPrimaryPaths(
	tabs map[string]string,
	kind string,
	gameOnly bool,
	hasGame bool,
) (model.LyricsSourceTabPath, model.LyricsSourceTabPath, error) {
	kindLabel := "SEKAI"
	if kind == "vocaloid" {
		kindLabel = "VIRTUAL SINGER"
	}
	if body := strings.TrimSpace(tabs[kindLabel]); body != "" && !gameOnly {
		return model.LyricsSourceTabPath{kindLabel}, nil, nil
	}
	fullLabel := ""
	for _, label := range []string{"Full Version", "APPEND/Full Version"} {
		if strings.TrimSpace(tabs[label]) != "" {
			fullLabel = label
			break
		}
	}
	pathFor := func(label string) (model.LyricsSourceTabPath, error) {
		if label == "" || strings.TrimSpace(tabs[label]) == "" {
			return nil, ErrMissingLyrics
		}
		nested, err := parseSekaipediaNestedRenditionTabs(tabs[label])
		if err != nil {
			return nil, err
		}
		if nested[kindLabel] != "" {
			return model.LyricsSourceTabPath{label, kindLabel}, nil
		}
		if nested[""] != "" {
			return model.LyricsSourceTabPath{label}, nil
		}
		for nestedLabel := range nested {
			if sekaipediaPrimaryLabelKind(nestedLabel) != "" {
				return nil, ErrMissingLyrics
			}
		}
		return model.LyricsSourceTabPath{label}, nil
	}
	var fullPath model.LyricsSourceTabPath
	var gamePath model.LyricsSourceTabPath
	var err error
	if !gameOnly {
		fullPath, err = pathFor(fullLabel)
		if err != nil {
			return nil, nil, err
		}
	}
	if gameOnly || hasGame {
		gamePath, err = pathFor("Game Version")
		if err != nil {
			return nil, nil, err
		}
	}
	return fullPath, gamePath, nil
}

func parseSekaipediaSongLegacy(content string, policy PerformerSegmentationPolicy) (sekaipediaSongExtraction, error) {
	if !utf8ValidBounded(content, maxResponseBytes) {
		return sekaipediaSongExtraction{}, ErrMalformedResponse
	}
	lyricsSection, err := sekaipediaLyricsSection(content)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	versionsSection, err := sekaipediaTopLevelSection(content, "Versions")
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	records, err := parseSekaipediaVersions(versionsSection)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	tabs, sameLyrics, err := parseSekaipediaLyricsTabs(lyricsSection)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	alternateVocals := []sekaipediaAlternateVocalExtraction{}
	if parsedAlternates, alternateErr := parseSekaipediaAlternateVocals(tabs, records); alternateErr != nil {
		return sekaipediaSongExtraction{}, fmt.Errorf("alternate vocal extraction: %w", alternateErr)
	} else if scopedAlternates, scopeErr := sekaipediaScopeAlternateVocalRenditions(parsedAlternates); scopeErr != nil {
		return sekaipediaSongExtraction{}, fmt.Errorf("alternate vocal singer scope: %w", scopeErr)
	} else {
		alternateVocals = scopedAlternates
	}

	fullBody, hasFull := sekaipediaPrimaryFullLyricsBody(tabs)
	gameBody := tabs["Game Version"]
	_, hasGame := tabs["Game Version"]
	_, hasSekai := tabs["SEKAI"]
	_, hasVirtualSinger := tabs["VIRTUAL SINGER"]
	versionLayout := hasFull || hasGame
	renditionLayout := hasSekai || hasVirtualSinger
	if versionLayout == renditionLayout {
		return sekaipediaSongExtraction{}, ErrUnsupportedTable
	}
	gameOnly := hasGame && !hasFull
	gameOnlySameLyrics := sameLyrics && gameOnly

	if renditionLayout {
		var label, kind string
		switch policy {
		case PerformerSegmentationSekaiEligible:
			if hasSekai {
				label, kind = "SEKAI", "sekai"
			} else if hasVirtualSinger {
				label, kind = "VIRTUAL SINGER", "vocaloid"
			} else {
				return sekaipediaSongExtraction{}, ErrMissingLyrics
			}
		case PerformerSegmentationDisabled:
			if !hasVirtualSinger {
				return sekaipediaSongExtraction{}, ErrCatalogRenditionConflict
			}
			label, kind = "VIRTUAL SINGER", "vocaloid"
		default:
			return sekaipediaSongExtraction{}, ErrMalformedResponse
		}
		sets, err := sekaipediaVersionSets(records, kind)
		if err != nil {
			return sekaipediaSongExtraction{}, err
		}
		selected, err := parseSekaipediaRenditionAgainstSets(tabs[label], kind, sets, true)
		if err != nil {
			return sekaipediaSongExtraction{}, err
		}
		selected.extraction, err = applyPerformerSegmentationPolicy(
			MusicIdentity{PerformerSegmentationPolicy: policy}, selected.extraction,
		)
		if err != nil {
			return sekaipediaSongExtraction{}, err
		}
		result := sekaipediaSongExtraction{
			Full:                    selected.extraction,
			Section:                 "Lyrics/" + label,
			GameSection:             "Lyrics/" + label,
			RenditionKey:            fullRenditionKey(kind),
			ReasonCode:              model.LyricsSourceVersionReasonUntaggedFullOnly,
			AuthoritativeStructured: sekaipediaRenditionHasAuthoritativeStructuredSegmentation(selected),
			AlternateVocals:         alternateVocals,
		}
		if sameLyrics {
			result.ReasonCode = model.LyricsSourceVersionReasonUntaggedUncutIdentity
			result.GameLineIndexes = make([]int, len(result.Full.Lines))
			for index := range result.GameLineIndexes {
				result.GameLineIndexes[index] = index
			}
		}
		return result, nil
	}

	if !hasFull && !hasGame {
		return sekaipediaSongExtraction{}, ErrMissingLyrics
	}
	var kind string
	switch policy {
	case PerformerSegmentationSekaiEligible:
		if sekaipediaHasVersionKind(records, "sekai") {
			kind = "sekai"
		} else if sekaipediaHasVersionKind(records, "vocaloid") {
			kind = "vocaloid"
		} else if !hasGame && sekaipediaHasSoleOriginalVersion(records) {
			kind = "original"
		} else {
			return sekaipediaSongExtraction{}, ErrMissingLyrics
		}
	case PerformerSegmentationDisabled:
		if sekaipediaHasVersionKind(records, "vocaloid") {
			kind = "vocaloid"
		} else if !hasGame && sekaipediaHasSoleOriginalVersion(records) {
			kind = "original"
		} else {
			return sekaipediaSongExtraction{}, ErrCatalogRenditionConflict
		}
	default:
		return sekaipediaSongExtraction{}, ErrMalformedResponse
	}
	sets, err := sekaipediaVersionSets(records, kind)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	var selected sekaipediaRenditionExtraction
	var selectedGame *Extraction
	var gameProjection []int
	switch {
	case gameOnly:
		selected, err = parseSekaipediaRenditionAgainstSets(gameBody, kind, sets, false)
		if err == nil {
			game := selected.extraction
			game.Version.Label = "Game Version"
			if gameOnlySameLyrics {
				// The fixed-revision note is an explicit exact-identity claim: the
				// visible Game text is also the complete Full text. Persist one Full
				// authority plus an identity GameProjection instead of misclassifying
				// the song as Game-only or inventing a second independent text.
				selectedGame = nil
			} else {
				selected.extraction = game
				selectedGame = &game
			}
		}
	case hasGame:
		joint, jointErr := parseSekaipediaFullGameAgainstSets(
			fullBody, gameBody, kind, sets,
		)
		if jointErr == nil {
			selected = joint.full
			gameProjection = joint.projection
			if !sekaipediaExactGameProjectionCompatible(joint.full, joint.game, joint.projection) {
				// Semantic alignment can identify the corresponding rows while
				// still changing the exact Japanese text (for example, spacing or
				// punctuation). The persisted GameProjection contract is stricter:
				// it may only claim an exact Full-line identity. Keep the independent
				// Game artifact and fail closed on the relation instead of emitting a
				// projection the model validator cannot prove.
				gameProjection = nil
			}
			game := joint.game.extraction
			selectedGame = &game
			err = nil
		} else {
			// Projection failure is not extraction failure. Parse each tagged table
			// independently so exact Full and Game artifacts survive even when no
			// strict relation can be proven between them.
			independentFull, fullErr := parseSekaipediaRenditionAgainstSets(fullBody, kind, sets, true)
			independentGame, gameErr := parseSekaipediaRenditionAgainstSets(gameBody, kind, sets, false)
			if fullErr == nil && gameErr == nil {
				selected = independentFull
				game := independentGame.extraction
				selectedGame = &game
				err = nil
			} else {
				err = jointErr
			}
		}
	default:
		selected, err = parseSekaipediaRenditionAgainstSets(fullBody, kind, sets, true)
	}
	if err != nil {
		if policy == PerformerSegmentationDisabled {
			// A line that failed the table is named in the text only, so the
			// outcome is still classified as a policy conflict.
			var lineErr *sekaipediaLineError
			if errors.As(err, &lineErr) {
				return sekaipediaSongExtraction{}, fmt.Errorf("%w: %v", ErrCatalogRenditionConflict, lineErr)
			}
			return sekaipediaSongExtraction{}, ErrCatalogRenditionConflict
		}
		return sekaipediaSongExtraction{}, err
	}
	selected.extraction, err = applyPerformerSegmentationPolicy(
		MusicIdentity{PerformerSegmentationPolicy: policy}, selected.extraction,
	)
	if err != nil {
		return sekaipediaSongExtraction{}, err
	}
	if selectedGame != nil {
		updatedGame, gameErr := applyPerformerSegmentationPolicy(
			MusicIdentity{PerformerSegmentationPolicy: policy}, *selectedGame,
		)
		if gameErr != nil {
			return sekaipediaSongExtraction{}, gameErr
		}
		selectedGame = &updatedGame
	}
	result := sekaipediaSongExtraction{
		Full:                    selected.extraction,
		Game:                    selectedGame,
		Section:                 "Lyrics/Full Version",
		GameSection:             "Lyrics/Game Version",
		RenditionKey:            fullRenditionKey(kind),
		ReasonCode:              model.LyricsSourceVersionReasonUntaggedFullOnly,
		AuthoritativeStructured: sekaipediaRenditionHasAuthoritativeStructuredSegmentation(selected),
		AlternateVocals:         alternateVocals,
	}
	switch {
	case gameOnlySameLyrics:
		result.Section = result.GameSection
		result.RenditionKey = fullRenditionKey(kind)
		result.ReasonCode = model.LyricsSourceVersionReasonUntaggedUncutIdentity
		result.GameLineIndexes = make([]int, len(result.Full.Lines))
		for index := range result.GameLineIndexes {
			result.GameLineIndexes[index] = index
		}
	case gameOnly:
		result.Section = result.GameSection
		result.RenditionKey = "game-" + kind
		result.ReasonCode = model.LyricsSourceVersionReasonTaggedGameOnly
	case hasGame && len(gameProjection) > 0:
		result.GameLineIndexes = gameProjection
		result.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
	case hasGame && selectedGame != nil:
		result.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
	case sameLyrics:
		result.GameSection = result.Section
		result.GameLineIndexes = make([]int, len(result.Full.Lines))
		for index := range result.GameLineIndexes {
			result.GameLineIndexes[index] = index
		}
		result.ReasonCode = model.LyricsSourceVersionReasonUntaggedUncutIdentity
	}
	return result, nil
}

func sekaipediaExtractionWithinPerformerScope(extraction *Extraction, performerIDs []string) bool {
	if extraction == nil {
		return true
	}
	allowed := make(map[string]bool, len(performerIDs))
	for _, performerID := range performerIDs {
		allowed[performerID] = true
	}
	for _, performer := range extraction.Performers {
		if !allowed[performer.PerformerID] {
			return false
		}
	}
	for _, line := range extraction.Lines {
		for _, performerID := range line.TrailingPerformerIDs {
			if !allowed[performerID] {
				return false
			}
		}
		for _, segment := range line.Segments {
			for _, performerID := range segment.PerformerIDs {
				if !allowed[performerID] {
					return false
				}
			}
		}
	}
	return true
}

func sekaipediaScopeAlternateVocalRenditions(
	alternates []sekaipediaAlternateVocalExtraction,
) ([]sekaipediaAlternateVocalExtraction, error) {
	result := make([]sekaipediaAlternateVocalExtraction, len(alternates))
	for index, alternate := range alternates {
		if alternate.DeclaredFull != (alternate.Full != nil) ||
			alternate.DeclaredGame != (alternate.Game != nil) ||
			alternate.Full == nil && alternate.Game == nil ||
			!sekaipediaExtractionWithinPerformerScope(alternate.Full, alternate.SingerIDs) ||
			!sekaipediaExtractionWithinPerformerScope(alternate.Game, alternate.SingerIDs) {
			return nil, ErrUnsupportedTable
		}
		result[index] = alternate
	}
	return result, nil
}

type sekaipediaRomajiEvidence struct {
	value     string
	ambiguous bool
}

type sekaipediaLyricsLineTemplateLocation struct {
	start  int
	end    int
	fields []string
}

func applySekaipediaExactMissingRomajiFallback(tabs map[string]string) map[string]string {
	evidence := map[string]sekaipediaRomajiEvidence{}
	characterEvidence := map[rune]sekaipediaRomajiEvidence{}
	recordEvidence := func(key, value string) {
		if key == "" || value == "" {
			return
		}
		current, exists := evidence[key]
		if !exists {
			evidence[key] = sekaipediaRomajiEvidence{value: value}
			return
		}
		if current.value != value {
			current.ambiguous = true
			evidence[key] = current
		}
	}
	recordCharacterEvidence := func(character rune, reading string) {
		if !validGeneratedRubyReading(reading) {
			return
		}
		current, exists := characterEvidence[character]
		if !exists {
			characterEvidence[character] = sekaipediaRomajiEvidence{value: reading}
			return
		}
		if current.value != reading {
			current.ambiguous = true
			characterEvidence[character] = current
		}
	}
	for _, body := range tabs {
		for _, location := range sekaipediaLyricsLineTemplateLocations(body) {
			params, err := sekaipediaNamedParameters(location.fields, map[string]bool{
				"japanese": true, "romaji": true, "english": true, "english 2": true,
			})
			if err != nil || params["japanese"] == "" || params["romaji"] == "" {
				continue
			}
			japaneseLines, _, japaneseErr := parseSekaipediaLyricColumnVariant(
				params["japanese"], sekaipediaAllSingerSet(),
			)
			romajiLines, romajiErr := parseSekaipediaReadingColumn(params["romaji"], sekaipediaAllSingerSet())
			if japaneseErr == nil && romajiErr == nil && len(japaneseLines) == len(romajiLines) {
				recordEvidence("raw\x00"+params["japanese"], params["romaji"])
				if visible, ok := sekaipediaRenderedColumnEvidenceKey(params["japanese"]); ok {
					recordEvidence("visible\x00"+visible, params["romaji"])
				}
				for lineIndex := range japaneseLines {
					// Evidence that other lines reuse never comes from the lenient
					// last resort, which may keep a guessed split.
					spans, ok := deriveSekaipediaRubyMode(
						sekaipediaColumnLineText(japaneseLines[lineIndex]),
						sekaipediaColumnLineText(romajiLines[lineIndex]),
						false,
					)
					if !ok {
						continue
					}
					for _, span := range spans {
						runes := []rune(span.Text)
						if len(runes) == 1 && model.LyricsSourceRubyBaseRune(runes[0]) && span.Reading != "" {
							recordCharacterEvidence(runes[0], span.Reading)
						}
					}
					for character, reading := range sekaipediaSourceOOVCharacterReadings(spans) {
						recordCharacterEvidence(character, reading)
					}
				}
			}
		}
	}
	result := make(map[string]string, len(tabs))
	for label, body := range tabs {
		locations := sekaipediaLyricsLineTemplateLocations(body)
		for index := len(locations) - 1; index >= 0; index-- {
			location := locations[index]
			params, err := sekaipediaNamedParameters(location.fields, map[string]bool{
				"japanese": true, "romaji": true, "english": true, "english 2": true,
			})
			if err != nil || params["japanese"] == "" {
				continue
			}
			if params["romaji"] != "" {
				if _, readingErr := parseSekaipediaReadingColumn(params["romaji"], sekaipediaAllSingerSet()); readingErr == nil || !isSekaipediaRecoverableReadingError(readingErr) {
					continue
				}
			}
			matched, exists := evidence["raw\x00"+params["japanese"]]
			visible := ""
			if params["japanese"] != "" {
				visible, _ = sekaipediaRenderedColumnEvidenceKey(params["japanese"])
			}
			if (!exists || matched.ambiguous || matched.value == "") && visible != "" {
				matched, exists = evidence["visible\x00"+visible]
			}
			explicitReading := ""
			if !exists || matched.ambiguous || matched.value == "" {
				if reading, ok := sekaipediaReadingFromCharacterEvidence(visible, characterEvidence); ok {
					explicitReading = string(reading)
				}
				if explicitReading == "" {
					continue
				}
			}
			fields := append([]string(nil), location.fields...)
			replaced := false
			if explicitReading != "" {
				for fieldIndex := 1; fieldIndex < len(fields); fieldIndex++ {
					separator := strings.IndexByte(fields[fieldIndex], '=')
					if separator < 0 || !strings.EqualFold(strings.TrimSpace(fields[fieldIndex][:separator]), "japanese") {
						continue
					}
					fields[fieldIndex] = fields[fieldIndex][:separator+1] + "{{ruby|" + params["japanese"] + "|" + explicitReading + "}}"
					replaced = true
					break
				}
			}
			if explicitReading == "" {
				for fieldIndex := 1; fieldIndex < len(fields); fieldIndex++ {
					separator := strings.IndexByte(fields[fieldIndex], '=')
					if separator < 0 || !strings.EqualFold(strings.TrimSpace(fields[fieldIndex][:separator]), "romaji") {
						continue
					}
					fields[fieldIndex] = fields[fieldIndex][:separator+1] + matched.value
					replaced = true
					break
				}
				if !replaced {
					fields = append(fields, "romaji="+matched.value)
				}
			}
			replacement := "{{" + strings.Join(fields, "|") + "}}"
			body = body[:location.start] + replacement + body[location.end:]
		}
		result[label] = body
	}
	return result
}

func sekaipediaSourceOOVCharacterReadings(spans []RubySpan) map[rune]string {
	result := map[rune]string{}
	ambiguous := map[rune]bool{}
	if initializeFuriganaTokenizer() != nil {
		return result
	}
	for _, span := range spans {
		if span.Reading == "" || !containsKanji(span.Text) {
			continue
		}
		type tokenReading struct {
			surface string
			reading []rune
			oov     bool
		}
		tokens := []tokenReading{}
		oovIndex := -1
		valid := true
		for _, token := range furiganaTokenizer.Tokenize(span.Text) {
			if token.Surface == "" {
				continue
			}
			features := token.Features()
			candidate := ""
			if len(features) >= 8 && features[7] != "*" {
				candidate = katakanaToHiragana(features[7])
			}
			if validGeneratedRubyReading(candidate) {
				if _, ok := rubySpansFromKanaReading(token.Surface, []rune(candidate)); ok {
					tokens = append(tokens, tokenReading{surface: token.Surface, reading: []rune(candidate)})
					continue
				}
			}
			runes := []rune(token.Surface)
			if oovIndex >= 0 || len(runes) != 1 || !model.LyricsSourceRubyBaseRune(runes[0]) {
				valid = false
				break
			}
			oovIndex = len(tokens)
			tokens = append(tokens, tokenReading{surface: token.Surface, oov: true})
		}
		if !valid || oovIndex < 0 {
			continue
		}
		prefix := []rune{}
		for _, token := range tokens[:oovIndex] {
			prefix = append(prefix, token.reading...)
		}
		suffix := []rune{}
		for _, token := range tokens[oovIndex+1:] {
			suffix = append(suffix, token.reading...)
		}
		source := canonicalizeSekaipediaKana([]rune(span.Reading))
		prefixEnd, prefixOK := sekaipediaKanaPrefix(source, 0, prefix)
		if !prefixOK || len(source)-prefixEnd < len(suffix) {
			continue
		}
		suffixStart := len(source) - len(suffix)
		suffixOK := true
		for index := range suffix {
			if !sekaipediaKanaEquivalent(source[suffixStart+index], suffix[index]) {
				suffixOK = false
				break
			}
		}
		if !suffixOK || suffixStart <= prefixEnd {
			continue
		}
		reading := string(source[prefixEnd:suffixStart])
		if !validGeneratedRubyReading(reading) {
			continue
		}
		character := []rune(tokens[oovIndex].surface)[0]
		if existing, exists := result[character]; exists && existing != reading {
			ambiguous[character] = true
			delete(result, character)
			continue
		}
		if !ambiguous[character] {
			result[character] = reading
		}
	}
	return result
}

func sekaipediaReadingFromCharacterEvidence(
	text string, evidence map[rune]sekaipediaRomajiEvidence,
) ([]rune, bool) {
	if text == "" || initializeFuriganaTokenizer() != nil {
		return nil, false
	}
	var reading strings.Builder
	usedSourceEvidence := false
	for _, token := range furiganaTokenizer.Tokenize(text) {
		if token.Surface == "" {
			continue
		}
		if containsKanji(token.Surface) {
			features := token.Features()
			if len(features) >= 8 && features[7] != "*" {
				candidate := katakanaToHiragana(features[7])
				if validGeneratedRubyReading(candidate) {
					if _, ok := rubySpansFromKanaReading(token.Surface, []rune(candidate)); ok {
						reading.WriteString(candidate)
						continue
					}
				}
			}
			runes := []rune(token.Surface)
			if len(runes) != 1 {
				return nil, false
			}
			matched, exists := evidence[runes[0]]
			if !exists || matched.ambiguous || !validGeneratedRubyReading(matched.value) {
				return nil, false
			}
			reading.WriteString(matched.value)
			usedSourceEvidence = true
			continue
		}
		for _, current := range token.Surface {
			if sekaipediaIsKana(current) {
				reading.WriteString(katakanaToHiragana(string(current)))
			}
		}
	}
	result := canonicalizeSekaipediaKana([]rune(reading.String()))
	return result, usedSourceEvidence && len(result) > 0
}

func sekaipediaRenderedColumnEvidenceKey(value string) (string, bool) {
	lines, _, err := parseSekaipediaLyricColumnVariant(value, sekaipediaAllSingerSet())
	if err != nil || len(lines) == 0 {
		return "", false
	}
	parts := make([]string, len(lines))
	for lineIndex, line := range lines {
		parts[lineIndex] = sekaipediaColumnLineText(line)
		if parts[lineIndex] == "" {
			return "", false
		}
	}
	return strings.Join(parts, "\n"), true
}

func sekaipediaLyricsLineTemplateLocations(value string) []sekaipediaLyricsLineTemplateLocation {
	locations := []sekaipediaLyricsLineTemplateLocation{}
	for cursor := 0; cursor < len(value); {
		relative := strings.Index(value[cursor:], "{{")
		if relative < 0 {
			break
		}
		start := cursor + relative
		_, end, inner, ok := balancedSekaipediaTemplateAt(value, start)
		if !ok {
			cursor = start + 2
			continue
		}
		fields, fieldsOK := splitTopLevelSekaipediaFields(inner, "|")
		if fieldsOK && len(fields) > 0 && strings.EqualFold(strings.TrimSpace(fields[0]), "Lyrics line") {
			locations = append(locations, sekaipediaLyricsLineTemplateLocation{start: start, end: end, fields: fields})
		}
		cursor = start + 2
	}
	return locations
}

func sekaipediaExactGameProjectionCompatible(
	full, game sekaipediaRenditionExtraction, mapping []int,
) bool {
	if len(mapping) == 0 || len(game.extraction.Lines) != len(mapping) {
		return false
	}
	for gameIndex, fullIndex := range mapping {
		if fullIndex < 0 || fullIndex >= len(full.extraction.Lines) ||
			game.extraction.Lines[gameIndex].Japanese != full.extraction.Lines[fullIndex].Japanese {
			return false
		}
	}
	return true
}

func sekaipediaRenditionHasAuthoritativeStructuredSegmentation(
	selected sekaipediaRenditionExtraction,
) bool {
	return selected.sourceTagged && extractionHasCompletePerformerSegmentation(selected.extraction)
}

func sekaipediaPrimaryFullLyricsBody(tabs map[string]string) (string, bool) {
	for _, label := range []string{"Full Version", "APPEND/Full Version"} {
		if body := strings.TrimSpace(tabs[label]); body != "" {
			return tabs[label], true
		}
	}
	return "", false
}

func parseSekaipediaVersions(section string) ([]sekaipediaVersionRecord, error) {
	value := strings.TrimSpace(section)
	if len(value) >= len("<tabber>") && strings.EqualFold(value[:len("<tabber>")], "<tabber>") {
		if len(value) <= len("<tabber>")+len("</tabber>") ||
			!strings.EqualFold(value[len(value)-len("</tabber>"):], "</tabber>") {
			return nil, ErrUnsupportedTable
		}
		inner := strings.TrimSpace(value[len("<tabber>") : len(value)-len("</tabber>")])
		tabs, err := parseSekaipediaTabberEntries(inner)
		if err != nil || len(tabs) != 3 || tabs["VIRTUAL SINGER"] == "" || tabs["SEKAI"] == "" || tabs["Another Vocal"] == "" {
			return nil, ErrUnsupportedTable
		}
		labels := []struct {
			name string
			kind string
		}{
			{name: "VIRTUAL SINGER", kind: "vocaloid"},
			{name: "SEKAI", kind: "sekai"},
			{name: "Another Vocal", kind: "another"},
		}
		records := []sekaipediaVersionRecord{}
		seen := map[string]struct{}{}
		for _, label := range labels {
			parsed, err := parseSekaipediaVersionsTable(tabs[label.name])
			if err != nil {
				return nil, err
			}
			for _, record := range parsed {
				if record.kind != label.kind && record.kind != "alternate" {
					return nil, ErrUnsupportedTable
				}
				key := record.kind + "\x00" + record.label + "\x00" + record.singers
				if _, duplicate := seen[key]; duplicate {
					return nil, ErrAmbiguous
				}
				seen[key] = struct{}{}
				records = append(records, record)
			}
		}
		if len(records) == 0 || len(records) > 64 {
			return nil, ErrUnsupportedTable
		}
		return records, nil
	}
	return parseSekaipediaVersionsTable(value)
}

func parseSekaipediaVersionsTable(section string) ([]sekaipediaVersionRecord, error) {
	templates, err := parseSekaipediaTemplateSequence(section)
	if err != nil || len(templates) < 3 || !strings.EqualFold(templates[0].name, "Song versions head") ||
		!strings.EqualFold(templates[len(templates)-1].name, "Song versions tail") || len(templates[0].fields) != 1 ||
		len(templates[len(templates)-1].fields) != 1 {
		return nil, ErrUnsupportedTable
	}
	records := make([]sekaipediaVersionRecord, 0, len(templates)-2)
	seen := map[string]struct{}{}
	for _, template := range templates[1 : len(templates)-1] {
		if !strings.EqualFold(template.name, "Song versions line") {
			return nil, ErrUnsupportedTable
		}
		params, err := sekaipediaNamedParameters(template.fields, map[string]bool{
			"version": true, "singers": true, "audio": true, "date": true,
		})
		if err != nil || params["version"] == "" {
			return nil, ErrUnsupportedTable
		}
		if params["version"] == "Instrumental" && len(templates) == 3 {
			_, hasSingers := params["singers"]
			if !hasSingers || params["singers"] != "" || params["audio"] == "" ||
				(len(params) != 3 && len(params) != 4) {
				return nil, ErrUnsupportedTable
			}
			records = append(records, sekaipediaVersionRecord{kind: "original"})
			continue
		}
		if isSekaipediaAuxiliaryVersionLabel(params["version"]) {
			if len(params) < 2 || len(params) > 4 {
				return nil, ErrUnsupportedTable
			}
			// Auxiliary rows are non-primary rendition evidence, not ignorable
			// decoration. Preserve every singer-bearing row so Archive, live, and
			// April Fools tabs can be resolved against their exact fixed roster.
			if params["singers"] != "" {
				key := "alternate\x00" + params["version"] + "\x00" + params["singers"]
				if _, duplicate := seen[key]; duplicate {
					return nil, ErrAmbiguous
				}
				seen[key] = struct{}{}
				records = append(records, sekaipediaVersionRecord{
					kind: "alternate", label: params["version"], singers: params["singers"],
				})
			}
			continue
		}
		if params["singers"] == "" || params["audio"] == "" || (len(params) != 3 && len(params) != 4) {
			return nil, ErrUnsupportedTable
		}
		var kind string
		switch params["version"] {
		case "SEKAI":
			kind = "sekai"
		case "VIRTUAL SINGER":
			kind = "vocaloid"
		case "Another Vocal":
			kind = "another"
		default:
			return nil, ErrUnsupportedTable
		}
		key := kind + "\x00" + params["singers"]
		if _, duplicate := seen[key]; duplicate {
			return nil, ErrAmbiguous
		}
		seen[key] = struct{}{}
		records = append(records, sekaipediaVersionRecord{kind: kind, label: params["version"], singers: params["singers"]})
	}
	if len(records) == 0 || len(records) > 64 {
		return nil, ErrUnsupportedTable
	}
	return records, nil
}

func isSekaipediaAuxiliaryVersionLabel(value string) bool {
	switch value {
	case "Instrumental",
		"Connect Live", "Connect Live (DAY1 first)", "Connect Live (DAY1 second)",
		"Connect Live (DAY2 first)", "Connect Live (DAY2 second)",
		"COLORFUL LIVE", "Project SEKAI the Movie",
		"April Fools' 2022", "April Fools' 2024", "April Fools' 2025", "April Fools' 2026",
		"[[Project SEKAI×Ensemble Stars!! | Ensemble Stars!! Collab]]",
		"Ensemble Stars!! Collab", "Ensemble Stars!!", "Ensemble Stars":
		return true
	default:
		return false
	}
}

func parseSekaipediaLyricsTabs(section string) (map[string]string, bool, error) {
	return parseSekaipediaLyricsLayout(section)
}

func bindSekaipediaImplicitSourceLocations(lines []sekaipediaColumnLine, sourceRowOrdinal int) {
	if sourceRowOrdinal <= 0 {
		return
	}
	segmentOrdinal := 0
	for lineIndex := range lines {
		for segmentIndex := range lines[lineIndex].segments {
			segment := &lines[lineIndex].segments[segmentIndex]
			if segment.sourceGroup > 0 || segment.sourceSegmentOrdinal > 0 {
				continue
			}
			segmentOrdinal++
			segment.sourceGroup = sourceRowOrdinal
			segment.sourceSegmentOrdinal = segmentOrdinal
		}
	}
}

func parseSekaipediaSourceColumn(
	value string, set sekaipediaSingerSet,
) ([]sekaipediaColumnLine, bool, error) {
	return parseSekaipediaLyricColumnVariant(value, set)
}

func parseSekaipediaReadingColumn(
	value string, set sekaipediaSingerSet,
) ([]sekaipediaColumnLine, error) {
	lines, _, err := parseSekaipediaLyricColumnVariant(value, set)
	return lines, err
}

// parseSekaipediaReadingColumnPrefix retains only a fully parsed leading
// sequence of top-level source templates. It is used after the complete reading
// column has already failed, so an invalid later sibling cannot erase exact
// row-local transliteration for earlier source groups. The caller must still
// prove that the prefix aligns one-to-one with the corresponding source lines.
func parseSekaipediaReadingColumnPrefix(
	value string, set sekaipediaSingerSet,
) ([]sekaipediaColumnLine, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxExtractedTextBytes || !utf8.ValidString(value) {
		return nil, false
	}
	var retained []sekaipediaColumnLine
	cursor := 0
	for cursor < len(value) {
		start := nextSekaipediaTemplateStart(value, cursor)
		if start < 0 {
			break
		}
		_, end, inner, balanced := balancedSekaipediaTemplateAt(value, start)
		if !balanced {
			break
		}
		candidate, err := parseSekaipediaReadingColumn(value[:end], set)
		if err != nil || len(candidate) == 0 {
			fields, fieldsOK := splitTopLevelSekaipediaFields(inner, "|")
			if !fieldsOK || len(fields) != 4 || !strings.EqualFold(strings.TrimSpace(fields[0]), "Lyric") ||
				!strings.Contains(fields[2], "\n") || strings.TrimSpace(fields[3]) == "" {
				break
			}
			if _, _, primaryErr := parseSekaipediaLyricTemplateFields(
				[]string{fields[0], fields[1], fields[2]}, set,
			); primaryErr != nil {
				break
			}
			// Some fixed rows encode two sequential reading segments as four
			// positional fields. Retain only the independently valid leading
			// singer/text pair; the unparsed final field remains explicitly
			// missing and must be generated from its own source segment.
			bounded := value[:start] + "{{" + strings.Join([]string{fields[0], fields[1], fields[2]}, "|") + "}}"
			candidate, err = parseSekaipediaReadingColumn(bounded, set)
			if err != nil || len(candidate) == 0 {
				break
			}
			retained = candidate
			break
		}
		retained = candidate
		cursor = end
	}
	if len(retained) == 0 || cursor >= len(value) {
		return nil, false
	}
	return retained, true
}

// trimSekaipediaLeadingLyricsProse skips editorial prose lines that appear
// before the first Lyrics head template inside one rendition tab. Some pages
// leave short notes about ambiguous singers directly above the table.
func trimSekaipediaLeadingLyricsProse(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r", ""))
	for value != "" {
		newline := strings.IndexByte(value, '\n')
		line := value
		rest := ""
		if newline >= 0 {
			line, rest = value[:newline], value[newline+1:]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			value = strings.TrimSpace(rest)
			continue
		}
		if strings.HasPrefix(line, "{{") {
			return value
		}
		value = strings.TrimSpace(rest)
	}
	return value
}

func sekaipediaRowContentFullyDeclared(params map[string]string, head sekaipediaLyricsHead) bool {
	if len(head.declared) == 0 {
		return false
	}
	for name, value := range params {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if !head.declared[name] {
			return false
		}
	}
	return true
}

func parseSekaipediaRenditionWithSet(
	body, kind string,
	set sekaipediaSingerSet,
	requireFullSet bool,
) (sekaipediaRenditionExtraction, error) {
	body, err := unwrapSekaipediaNestedRendition(body, kind)
	if err != nil {
		return sekaipediaRenditionExtraction{}, err
	}
	body = trimSekaipediaLeadingLyricsProse(body)
	templates, err := parseSekaipediaTemplateSequence(body)
	if err == nil {
		templates, err = stripSekaipediaLeadingLyricStubs(templates)
	}
	if err != nil || len(templates) < 2 || !strings.EqualFold(templates[0].name, "Lyrics head") {
		return sekaipediaRenditionExtraction{}, ErrUnsupportedTable
	}
	lineEnd := len(templates)
	if strings.EqualFold(templates[len(templates)-1].name, "Lyrics tail") {
		tailFields := templates[len(templates)-1].fields
		if len(tailFields) < 1 || len(tailFields) > 2 ||
			(len(tailFields) == 2 && len(tailFields[1]) > maxExtractedTextBytes) {
			return sekaipediaRenditionExtraction{}, ErrUnsupportedTable
		}
		lineEnd--
	} else if requireFullSet {
		return sekaipediaRenditionExtraction{}, ErrUnsupportedTable
	}
	if lineEnd < 2 {
		return sekaipediaRenditionExtraction{}, ErrMissingLyrics
	}
	head, err := parseSekaipediaLyricsHead(templates[0])
	if err != nil {
		return sekaipediaRenditionExtraction{}, err
	}

	japaneseLines := []sekaipediaColumnLine{}
	romajiLines := []sekaipediaColumnLine{}
	usedIDs := map[string]struct{}{}
	romajiComplete := head.hasRomaji
	japaneseHasTagged := false
	japaneseHasPlain := false
	for lineIndex, template := range templates[1:lineEnd] {
		if !strings.EqualFold(template.name, "Lyrics line") {
			return sekaipediaRenditionExtraction{}, ErrUnsupportedTable
		}
		params, err := sekaipediaNamedParameters(template.fields, map[string]bool{
			"japanese": true, "romaji": true, "english": true, "english 2": true,
		})
		if err != nil {
			return sekaipediaRenditionExtraction{}, ErrMissingLyrics
		}
		sourceValue, hasSource := params[head.sourceColumn]
		if strings.TrimSpace(sourceValue) == "" {
			allEmpty := true
			for _, value := range params {
				allEmpty = allEmpty && strings.TrimSpace(value) == ""
			}
			if hasSource && allEmpty {
				continue
			}
			// Rows whose source column is empty but whose other declared
			// columns carry content (e.g. repeated romaji-only interjections)
			// act as stanza separators; the Japanese import loses nothing and
			// the following line keeps its stanza break. Content in a column
			// the Lyrics head never declared still fails closed.
			if hasSource && sekaipediaRowContentFullyDeclared(params, head) {
				continue
			}
			return sekaipediaRenditionExtraction{}, ErrMissingLyrics
		}
		parsedJapanese, taggedJapanese, err := parseSekaipediaSourceColumn(sourceValue, set)
		if err != nil {
			return sekaipediaRenditionExtraction{}, err
		}
		bindSekaipediaImplicitSourceLocations(parsedJapanese, lineIndex+1)
		if taggedJapanese {
			japaneseHasTagged = true
		}
		if !taggedJapanese || sekaipediaLinesHaveUnperformedSegments(parsedJapanese) {
			japaneseHasPlain = true
		}
		for index := range parsedJapanese {
			parsedJapanese[index].stanzaBreakBefore = parsedJapanese[index].stanzaBreakBefore || lineIndex > 0 && index == 0
			for _, segment := range parsedJapanese[index].segments {
				for _, performerID := range segment.performerIDs {
					usedIDs[performerID] = struct{}{}
				}
			}
		}
		japaneseStart := len(japaneseLines)
		japaneseLines = append(japaneseLines, parsedJapanese...)

		if params["romaji"] == "" {
			romajiComplete = false
			romajiLines = appendSekaipediaMissingReadingLines(romajiLines, len(parsedJapanese))
			continue
		}
		if !head.hasRomaji {
			return sekaipediaRenditionExtraction{}, ErrUnsupportedTable
		}
		parsedRomaji, err := parseSekaipediaReadingColumn(params["romaji"], set)
		if err != nil {
			if isSekaipediaRecoverableReadingError(err) {
				romajiComplete = false
				if prefix, ok := parseSekaipediaReadingColumnPrefix(params["romaji"], set); ok &&
					len(prefix) < len(parsedJapanese) && sekaipediaColumnsAligned(parsedJapanese[:len(prefix)], prefix) {
					bindSekaipediaImplicitSourceLocations(prefix, lineIndex+1)
					romajiLines = append(romajiLines, prefix...)
					romajiLines = appendSekaipediaMissingReadingLines(romajiLines, len(parsedJapanese)-len(prefix))
					continue
				}
				romajiLines = appendSekaipediaMissingReadingLines(romajiLines, len(parsedJapanese))
				continue
			}
			return sekaipediaRenditionExtraction{}, err
		}
		bindSekaipediaImplicitSourceLocations(parsedRomaji, lineIndex+1)
		if len(parsedRomaji) != len(parsedJapanese) {
			romajiComplete = false
			for lineIndex := range parsedJapanese {
				parsedJapanese[lineIndex].allowUniqueDictionaryProbe = true
				japaneseLines[japaneseStart+lineIndex].allowUniqueDictionaryProbe = true
			}
			mapped := make([]sekaipediaColumnLine, len(parsedJapanese))
			for lineIndex := range parsedJapanese {
				mapped[lineIndex] = parsedJapanese[lineIndex]
				mapped[lineIndex].rubyFallback = append([]RubySpan(nil), parsedJapanese[lineIndex].rubyFallback...)
				mapped[lineIndex].segments = append([]sekaipediaColumnSegment(nil), parsedJapanese[lineIndex].segments...)
				for segmentIndex := range mapped[lineIndex].segments {
					segment := &mapped[lineIndex].segments[segmentIndex]
					segment.performerIDs = append([]string(nil), segment.performerIDs...)
					segment.ruby = append([]RubySpan(nil), segment.ruby...)
				}
			}
			if applySekaipediaExactSourceGroupRuby(mapped, parsedRomaji) {
				copy(japaneseLines[japaneseStart:], mapped)
			} else if fallback, ok := deriveSekaipediaColumnRubyFallback(parsedJapanese, parsedRomaji); ok {
				for lineIndex := range fallback {
					japaneseLines[japaneseStart+lineIndex].rubyFallback = fallback[lineIndex]
				}
			}
			romajiLines = appendSekaipediaMissingReadingLines(romajiLines, len(parsedJapanese))
			continue
		}
		romajiLines = append(romajiLines, parsedRomaji...)
	}
	if len(japaneseLines) == 0 || len(japaneseLines) > maxExtractedLines {
		return sekaipediaRenditionExtraction{}, ErrMissingLyrics
	}
	japaneseTagged := japaneseHasTagged && !japaneseHasPlain
	if japaneseTagged && (len(usedIDs) == 0 || len(set.ids) == 0) {
		return sekaipediaRenditionExtraction{}, ErrUnsupportedTable
	}

	// Reuse a complete reading only for an exact repeated visible line within
	// this one source side. The helper rejects conflicting readings, so tied or
	// ambiguous source evidence remains fail-closed and never crosses renditions.
	applySekaipediaRepeatedLineRubyFallback(japaneseLines, romajiLines)
	aligned := romajiComplete && sekaipediaColumnsAligned(japaneseLines, romajiLines)
	// Performer tags in the authoritative Japanese column remain valid even when
	// an optional romaji column is missing or cannot be aligned globally. Keep
	// those source segments and deterministically generate kana ruby for the same
	// exact Japanese text; segmentation and reading evidence are independent.
	japaneseOnly := japaneseTagged && !aligned
	generatedRuby := !head.englishSource && !aligned
	lines, segmentCount, err := buildSekaipediaStructuredLines(
		japaneseLines, romajiLines, aligned, japaneseOnly, generatedRuby,
	)
	if err != nil {
		return sekaipediaRenditionExtraction{}, err
	}
	version := LyricsVersion{Kind: kind, Label: "SEKAI Version"}
	switch kind {
	case "vocaloid":
		version.Label = "VIRTUAL SINGER Version"
	case "original":
		version.Label = "Original Version"
	case "alternate":
		version.Label = "Alternate Vocal"
	}
	performers := []Performer{}
	rubyVersion := ""
	if japaneseHasTagged && (aligned || japaneseOnly || generatedRuby) {
		performerIDs := make([]string, 0, len(usedIDs))
		for _, singer := range sekaipediaSingers {
			if _, witnessed := usedIDs[singer.id]; witnessed {
				performerIDs = append(performerIDs, singer.id)
			}
		}
		performers = sekaipediaPerformers(performerIDs)
	}
	switch {
	case head.hasRomaji && !head.englishSource:
		rubyVersion = sekaipediaRubyGeneratorVersion
	case generatedRuby:
		rubyVersion = rubyGeneratorVersion
	}
	return sekaipediaRenditionExtraction{
		extraction: Extraction{
			Version: version, Performers: performers, RubyGeneratorVersion: rubyVersion, Lines: lines,
		},
		projectionLines: japaneseLines,
		usedIDs:         usedIDs,
		aligned:         aligned && japaneseTagged,
		japaneseOnly:    japaneseOnly,
		sourceTagged:    japaneseHasTagged,
		segments:        segmentCount,
		set:             set,
	}, nil
}

func equalFoldSekaipediaHeader(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}
