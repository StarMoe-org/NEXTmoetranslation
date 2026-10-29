package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"moesekai/server/internal/jsonstrict"
	"moesekai/server/internal/model"
)

var ErrPublicLyricsV3Unavailable = errors.New("public lyrics v3 is unavailable for this recovery batch")

var errPublicLyricsV2CompatibilityUnrepresentable = errors.New("public lyrics v3 document is not losslessly representable as v2")

func isPublicV3RenditionKey(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	c0 := s[0]
	if !((c0 >= 'a' && c0 <= 'z') || (c0 >= '0' && c0 <= '9')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func isPublicV3Color(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// validateCompactPublicArtifactSize computes the exact 2-space indented JSON
// byte length from an already-marshaled compact JSON payload so callers do not
// have to re-encode the struct a second time through json.Encoder.
func validateCompactPublicArtifactSize(compact []byte) error {
	n := int64(len(compact)) + 1 // trailing newline emitted by json.Encoder.Encode
	depth := 0
	inString := false
	for i := 0; i < len(compact); i++ {
		c := compact[i]
		if inString {
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ':':
			n++
		case '{':
			if i+1 < len(compact) && compact[i+1] == '}' {
				i++
				continue
			}
			depth++
			n += 1 + int64(2*depth)
		case '[':
			if i+1 < len(compact) && compact[i+1] == ']' {
				i++
				continue
			}
			depth++
			n += 1 + int64(2*depth)
		case ',':
			n += 1 + int64(2*depth)
		case '}', ']':
			depth--
			n += 1 + int64(2*depth)
		}
	}
	if n > int64(model.PublicLyricsMaxArtifactBytes) {
		return errors.New("encoded public lyrics document exceeds the public artifact size limit")
	}
	return nil
}

func validateDecodedPublicArtifactSize(raw []byte, decoded any) error {
	// Even maximal 2-space indentation at bounded public lyrics depth (< 12)
	// expands compact JSON by well under 16x; skip re-encoding normal payloads.
	if len(raw) <= model.PublicLyricsMaxArtifactBytes/16 {
		return nil
	}
	return validatePublicLyricsArtifactSize(decoded)
}

type PublicLyricsV3TranslationCredits struct {
	Translation  string `json:"translation,omitempty"`
	Proofreading string `json:"proofreading,omitempty"`
}

type PublicLyricsV3RubySpan struct {
	Text    string `json:"text"`
	Reading string `json:"reading,omitempty"`
}

type PublicLyricsV3Segment struct {
	Text         string                   `json:"text"`
	PerformerIDs []string                 `json:"performerIds"`
	Ruby         []PublicLyricsV3RubySpan `json:"ruby"`
}

type PublicLyricsV3Line struct {
	ID                   string                  `json:"id"`
	Order                int                     `json:"order"`
	Japanese             string                  `json:"japanese"`
	Chinese              string                  `json:"zh-CN,omitempty"`
	English              string                  `json:"en-US,omitempty"`
	StanzaBreakBefore    bool                    `json:"stanzaBreakBefore,omitempty"`
	Segments             []PublicLyricsV3Segment `json:"segments"`
	TrailingPerformerIDs []string                `json:"trailingPerformerIds"`
}

type PublicLyricsV3Side struct {
	Version model.LyricsSourceVersion `json:"version"`
	Lines   []PublicLyricsV3Line      `json:"lines"`
}

type PublicLyricsV3Relation struct {
	Kind             model.LyricsSourceRenditionRelationKind `json:"kind"`
	FullRenditionKey string                                  `json:"fullRenditionKey,omitempty"`
	LineIDs          []string                                `json:"lineIds,omitempty"`
}

type PublicLyricsV3ComponentAttribution struct {
	Component   string                     `json:"component"`
	Provider    model.LyricsSourceProvider `json:"provider"`
	Title       string                     `json:"title"`
	RevisionID  int                        `json:"revisionId"`
	RevisionURL string                     `json:"revisionUrl"`
	LicenseName string                     `json:"licenseName"`
	LicenseURL  string                     `json:"licenseUrl"`
}

type PublicLyricsV3Rendition struct {
	Key                string                               `json:"key"`
	Kind               model.LyricsSourceRenditionKind      `json:"kind"`
	Label              string                               `json:"label"`
	AvailableVersions  []string                             `json:"availableVersions"`
	Performers         []model.LyricsSourcePerformer        `json:"performers"`
	Full               *PublicLyricsV3Side                  `json:"full,omitempty"`
	Game               *PublicLyricsV3Side                  `json:"game,omitempty"`
	Relation           PublicLyricsV3Relation               `json:"relation"`
	SourceTabPaths     []model.LyricsSourceTabPath          `json:"sourceTabPaths"`
	Provenance         []PublicLyricsV3ComponentAttribution `json:"provenance"`
	TranslationCredits *PublicLyricsV3TranslationCredits    `json:"translationCredits,omitempty"`
}

type PublicLyricsV3DetailDocument struct {
	Version    int                           `json:"version"`
	MusicID    int                           `json:"musicId"`
	Revision   int                           `json:"revision"`
	UpdatedAt  string                        `json:"updatedAt"`
	State      PublicLyricsAvailabilityState `json:"state,omitempty"`
	Renditions []PublicLyricsV3Rendition     `json:"renditions"`
}

type PublicLyricsV3IndexDocument struct {
	Version int                     `json:"version"`
	Songs   []PublicLyricsIndexSong `json:"songs"`
}

func EncodePublicLyricsV3Index(index PublicLyricsV3IndexDocument) ([]byte, error) {
	if err := validatePublicLyricsV3Index(index); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if err := validateCompactPublicArtifactSize(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func DecodePublicLyricsV3Index(body []byte) (PublicLyricsV3IndexDocument, error) {
	var index PublicLyricsV3IndexDocument
	if err := decodePublicLyricsV3JSON(body, &index); err != nil {
		return PublicLyricsV3IndexDocument{}, fmt.Errorf("decode public lyrics v3 index: %w", err)
	}
	if err := validatePublicLyricsV3Index(index); err != nil {
		return PublicLyricsV3IndexDocument{}, err
	}
	if err := validateDecodedPublicArtifactSize(body, index); err != nil {
		return PublicLyricsV3IndexDocument{}, err
	}
	return index, nil
}

func EncodePublicLyricsV3Detail(detail PublicLyricsV3DetailDocument) ([]byte, error) {
	if err := validatePublicLyricsV3Detail(detail); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	if err := validateCompactPublicArtifactSize(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func DecodePublicLyricsV3Detail(body []byte) (PublicLyricsV3DetailDocument, error) {
	var detail PublicLyricsV3DetailDocument
	if err := decodePublicLyricsV3JSON(body, &detail); err != nil {
		return PublicLyricsV3DetailDocument{}, fmt.Errorf("decode public lyrics v3 detail: %w", err)
	}
	if err := validatePublicLyricsV3Detail(detail); err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	if err := validateDecodedPublicArtifactSize(body, detail); err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	return detail, nil
}

func decodePublicLyricsV3JSON(body []byte, target any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("public lyrics v3 JSON is empty")
	}
	if err := jsonstrict.Scan(body, jsonstrict.Options{ForbidNull: true}); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("public lyrics v3 contains trailing JSON")
		}
		return err
	}
	return nil
}

func buildPublicLyricsV3Rendition(rendition model.LyricsSourceRendition, translations []string, sideTranslations map[string][]string, localization *LyricsRenditionLocalizationBackupRecord, bindings []model.LyricsSourceRenditionComponentBinding, identities map[string]model.LyricsSourceFixedIdentity, contributions map[string]string) (PublicLyricsV3Rendition, error) {
	return buildLyricsV3Rendition(rendition, translations, sideTranslations, localization, bindings, identities, contributions, true)
}

func buildLyricsV3Rendition(rendition model.LyricsSourceRendition, translations []string, sideTranslations map[string][]string, localization *LyricsRenditionLocalizationBackupRecord, bindings []model.LyricsSourceRenditionComponentBinding, identities map[string]model.LyricsSourceFixedIdentity, contributions map[string]string, validatePublicCredits bool) (PublicLyricsV3Rendition, error) {
	result := PublicLyricsV3Rendition{Key: rendition.RenditionKey, Kind: rendition.SourceKind, SourceTabPaths: clonePublicV3TabPaths(rendition.SourceTabPaths), Relation: PublicLyricsV3Relation{Kind: rendition.Relation.Kind, FullRenditionKey: rendition.Relation.FullRenditionKey, LineIDs: append([]string(nil), rendition.Relation.LineIDs...)}, Performers: publicV3Performers(rendition.Full, rendition.Game)}
	if rendition.Full != nil {
		result.AvailableVersions = append(result.AvailableVersions, "full")
		result.Label = rendition.Full.Version.Label
		result.Full = publicV3Side(*rendition.Full, translations)
	}
	if rendition.Game != nil {
		result.AvailableVersions = append(result.AvailableVersions, "game")
		if result.Label == "" {
			result.Label = rendition.Game.Version.Label
		}
		gameTranslations := append([]string(nil), sideTranslations["game"]...)
		if rendition.Full == nil {
			gameTranslations = append([]string(nil), translations...)
		}
		if rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
			var err error
			gameTranslations, err = publicV3ProjectionTranslations(
				*rendition.Full, rendition.Relation.LineIDs, translations,
			)
			if err != nil {
				return PublicLyricsV3Rendition{}, err
			}
		}
		result.Game = publicV3Side(*rendition.Game, gameTranslations)
	} else if rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
		if rendition.Full == nil {
			return PublicLyricsV3Rendition{}, errors.New("public v3 exact projection has no Full side")
		}
		projected, err := publicV3ProjectedSide(
			*rendition.Full, rendition.Relation.LineIDs, translations,
		)
		if err != nil {
			return PublicLyricsV3Rendition{}, err
		}
		result.AvailableVersions = append(result.AvailableVersions, "game")
		result.Game = projected
	}
	if localization != nil && (localization.TranslationCredit != "" || localization.ProofreadingCredit != "") {
		result.TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: localization.TranslationCredit, Proofreading: localization.ProofreadingCredit}
	}
	for _, binding := range bindings {
		if binding.RenditionKey != rendition.RenditionKey {
			continue
		}
		fixedIdentityKey, found := contributions[binding.ComponentKey]
		if !found || fixedIdentityKey != binding.FixedIdentityKey {
			return PublicLyricsV3Rendition{}, fmt.Errorf("public v3 component %s contribution identity is not canonical", binding.ComponentKey)
		}
		identity, ok := identities[fixedIdentityKey]
		if !ok {
			return PublicLyricsV3Rendition{}, fmt.Errorf("public v3 component %s identity is missing", binding.ComponentKey)
		}
		licenseName, licenseURL := publicLyricsProviderLicense(identity.Provider)
		if licenseName == "" || licenseURL == "" {
			return PublicLyricsV3Rendition{}, fmt.Errorf("public v3 component %s has no provider license", binding.ComponentKey)
		}
		result.Provenance = append(result.Provenance, PublicLyricsV3ComponentAttribution{Component: binding.ComponentKey, Provider: identity.Provider, Title: identity.Title, RevisionID: identity.RevisionID, RevisionURL: identity.CanonicalURL, LicenseName: licenseName, LicenseURL: licenseURL})
	}
	if validatePublicCredits {
		return result, validatePublicLyricsV3Rendition(result)
	}
	credits := result.TranslationCredits
	result.TranslationCredits = nil
	if err := validatePublicLyricsV3Rendition(result); err != nil {
		return PublicLyricsV3Rendition{}, err
	}
	result.TranslationCredits = credits
	return result, nil
}

func publicV3ProjectionTranslations(
	full model.LyricsSourceFull,
	lineIDs, translations []string,
) ([]string, error) {
	if len(translations) == 0 {
		return nil, nil
	}
	byLineID := make(map[string]string, len(full.Lines))
	for index, line := range full.Lines {
		if index < len(translations) {
			byLineID[line.ID] = translations[index]
		}
	}
	result := make([]string, len(lineIDs))
	for index, lineID := range lineIDs {
		value, found := byLineID[lineID]
		if !found {
			return nil, fmt.Errorf(
				"public v3 exact projection line %q has no Full translation", lineID,
			)
		}
		result[index] = value
	}
	return result, nil
}

var emptyPublicV3StringBacking [0]string

func formatProjectedGameLineID(n int) string {
	var buf [16]byte
	copy(buf[:5], "game-")
	if n >= 0 && n <= 999999 {
		for pos := 10; pos >= 5; pos-- {
			buf[pos] = byte('0' + n%10)
			n /= 10
		}
		return string(buf[:11])
	}
	return fmt.Sprintf("game-%06d", n)
}

func publicV3ProjectedSide(
	full model.LyricsSourceFull,
	lineIDs, translations []string,
) (*PublicLyricsV3Side, error) {
	byLineID := make(map[string]int, len(full.Lines))
	for idx, line := range full.Lines {
		byLineID[line.ID] = idx
	}
	projLines := make([]model.LyricsSourceFullLine, len(lineIDs))
	var gameTranslations []string
	if len(translations) > 0 {
		gameTranslations = make([]string, len(lineIDs))
	}
	for index, lineID := range lineIDs {
		fullIdx, found := byLineID[lineID]
		if !found {
			return nil, fmt.Errorf(
				"public v3 exact projection references unknown Full line %q", lineID,
			)
		}
		line := full.Lines[fullIdx]
		line.ID = formatProjectedGameLineID(index + 1)
		projLines[index] = line
		if len(translations) > 0 {
			if fullIdx >= len(translations) {
				return nil, fmt.Errorf(
					"public v3 exact projection line %q has no Full translation", lineID,
				)
			}
			gameTranslations[index] = translations[fullIdx]
		}
	}
	projected := model.LyricsSourceFull{
		Version:              full.Version,
		Performers:           full.Performers,
		Lines:                projLines,
		RubyGeneratorVersion: full.RubyGeneratorVersion,
	}
	return publicV3Side(projected, gameTranslations), nil
}

func publicV3Side(source model.LyricsSourceFull, translations []string) *PublicLyricsV3Side {
	totalSegs := 0
	totalRuby := 0
	totalPerfIDs := 0
	for _, line := range source.Lines {
		totalPerfIDs += len(line.TrailingPerformerIDs)
		totalSegs += len(line.Segments)
		for _, seg := range line.Segments {
			totalPerfIDs += len(seg.PerformerIDs)
			totalRuby += len(seg.Ruby)
		}
	}
	linesSlab := make([]PublicLyricsV3Line, len(source.Lines))
	segsSlab := make([]PublicLyricsV3Segment, totalSegs)
	rubySlab := make([]PublicLyricsV3RubySpan, totalRuby)
	var perfSlab []string
	if totalPerfIDs > 0 {
		perfSlab = make([]string, totalPerfIDs)
	}
	segPos := 0
	rubyPos := 0
	perfPos := 0
	slicePerf := func(src []string) []string {
		if src == nil {
			return nil
		}
		n := len(src)
		if n == 0 {
			return emptyPublicV3StringBacking[:0:0]
		}
		dst := perfSlab[perfPos : perfPos+n : perfPos+n]
		copy(dst, src)
		perfPos += n
		return dst
	}
	for index, line := range source.Lines {
		nSeg := len(line.Segments)
		lineSegs := segsSlab[segPos : segPos+nSeg : segPos+nSeg]
		segPos += nSeg
		for segmentIndex, segment := range line.Segments {
			nRuby := len(segment.Ruby)
			segRuby := rubySlab[rubyPos : rubyPos+nRuby : rubyPos+nRuby]
			rubyPos += nRuby
			for rubyIndex, span := range segment.Ruby {
				segRuby[rubyIndex] = PublicLyricsV3RubySpan{Text: span.Text, Reading: span.Reading}
			}
			lineSegs[segmentIndex] = PublicLyricsV3Segment{
				Text:         segment.Text,
				PerformerIDs: slicePerf(segment.PerformerIDs),
				Ruby:         segRuby,
			}
		}
		publicLine := PublicLyricsV3Line{
			ID:                   line.ID,
			Order:                index,
			Japanese:             line.Text,
			StanzaBreakBefore:    line.StanzaBreakBefore,
			TrailingPerformerIDs: slicePerf(line.TrailingPerformerIDs),
			Segments:             lineSegs,
		}
		if index < len(translations) {
			publicLine.Chinese = translations[index]
		}
		linesSlab[index] = publicLine
	}
	return &PublicLyricsV3Side{Version: source.Version, Lines: linesSlab}
}

func publicV3Performers(full, game *model.LyricsSourceFull) []model.LyricsSourcePerformer {
	seen := map[string]model.LyricsSourcePerformer{}
	for _, source := range []*model.LyricsSourceFull{full, game} {
		if source == nil {
			continue
		}
		for _, performer := range source.Performers {
			seen[performer.PerformerID] = performer
		}
	}
	result := make([]model.LyricsSourcePerformer, 0, len(seen))
	for _, performer := range seen {
		result = append(result, performer)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].PerformerID < result[right].PerformerID })
	return result
}

func clonePublicV3Strings(input []string) []string {
	if input == nil {
		return nil
	}
	return append([]string{}, input...)
}

func clonePublicV3TabPaths(input []model.LyricsSourceTabPath) []model.LyricsSourceTabPath {
	result := make([]model.LyricsSourceTabPath, len(input))
	for index, path := range input {
		result[index] = append(model.LyricsSourceTabPath(nil), path...)
	}
	return result
}

func publicV3AvailableVersions(detail PublicLyricsV3DetailDocument) []string {
	seen := map[string]bool{}
	for _, rendition := range detail.Renditions {
		for _, version := range rendition.AvailableVersions {
			seen[version] = true
		}
	}
	result := []string{}
	for _, version := range []string{"full", "game"} {
		if seen[version] {
			result = append(result, version)
		}
	}
	return result
}

func validatePublicLyricsV3Index(index PublicLyricsV3IndexDocument) error {
	if index.Version != 3 || len(index.Songs) == 0 || len(index.Songs) > model.PublicLyricsMaxIndexEntries {
		return errors.New("public v3 index envelope is invalid")
	}
	last := 0
	for _, song := range index.Songs {
		if song.MusicID <= last || song.Revision <= 0 || !canonicalPublicV3Timestamp(song.UpdatedAt) ||
			song.Title.Japanese == "" || song.Title.Japanese != strings.TrimSpace(song.Title.Japanese) ||
			len(song.Title.Japanese) > model.PublicLyricsMaxTitleBytes || len(song.Title.Chinese) > model.PublicLyricsMaxTitleBytes ||
			len(song.Title.English) > model.PublicLyricsMaxTitleBytes || !utf8.ValidString(song.Title.Japanese) ||
			!utf8.ValidString(song.Title.Chinese) || !utf8.ValidString(song.Title.English) {
			return errors.New("public v3 index is not ordered or bounded")
		}
		last = song.MusicID
		switch song.State {
		case PublicLyricsStateComplete:
			if !samePublicV3Strings(song.AvailableVersions, []string{"full"}) &&
				!samePublicV3Strings(song.AvailableVersions, []string{"full", "game"}) || song.NoLyricsReason != "" {
				return fmt.Errorf("public v3 index music %d complete state is not lossless", song.MusicID)
			}
		case PublicLyricsStateGameOnly:
			if !samePublicV3Strings(song.AvailableVersions, []string{"game"}) || song.NoLyricsReason != "" {
				return fmt.Errorf("public v3 index music %d Game-only state is not lossless", song.MusicID)
			}
		case PublicLyricsStateSatisfiedNoLyrics:
			if song.AvailableVersions != nil || song.NoLyricsReason != model.LyricsAvailabilityNoLyricsCatalogInstrumental {
				return fmt.Errorf("public v3 index music %d no-lyrics state is invalid", song.MusicID)
			}
		case PublicLyricsStateAmbiguous, PublicLyricsStateMissing, PublicLyricsStateIncomplete, PublicLyricsStateFailed:
			if song.AvailableVersions != nil || song.NoLyricsReason != "" {
				return fmt.Errorf("public v3 index music %d unresolved state owns text", song.MusicID)
			}
		default:
			return fmt.Errorf("public v3 index music %d has unsupported state %q", song.MusicID, song.State)
		}
	}
	return nil
}

func validatePublicLyricsV3Detail(detail PublicLyricsV3DetailDocument) error {
	if detail.Version != 3 || detail.MusicID <= 0 || detail.Revision <= 0 || !canonicalPublicV3Timestamp(detail.UpdatedAt) ||
		(detail.State != PublicLyricsStateComplete && detail.State != PublicLyricsStateGameOnly) || detail.Renditions == nil || len(detail.Renditions) == 0 || len(detail.Renditions) > 16 {
		return errors.New("public v3 detail envelope is invalid")
	}
	lastKey := ""
	seenPaths := map[string]string{}
	hasFull, hasGame := false, false
	for _, rendition := range detail.Renditions {
		if rendition.Key <= lastKey {
			return errors.New("public v3 renditions are not strictly ordered")
		}
		lastKey = rendition.Key
		if err := validatePublicLyricsV3Rendition(rendition); err != nil {
			return err
		}
		for _, path := range rendition.SourceTabPaths {
			key := strings.Join(path, "\x00")
			if owner, duplicate := seenPaths[key]; duplicate {
				return fmt.Errorf("public v3 source tab path is shared by renditions %q and %q", owner, rendition.Key)
			}
			seenPaths[key] = rendition.Key
		}
		hasFull = hasFull || rendition.Full != nil
		hasGame = hasGame || rendition.Game != nil
	}
	if !hasFull && !hasGame || detail.State == PublicLyricsStateComplete && !hasFull ||
		detail.State == PublicLyricsStateGameOnly && (hasFull || !hasGame) {
		return errors.New("public v3 detail state does not match rendition sides")
	}
	return nil
}

func validatePublicLyricsV3Rendition(rendition PublicLyricsV3Rendition) error {
	if !isPublicV3RenditionKey(rendition.Key) || rendition.Label == "" || rendition.Label != strings.TrimSpace(rendition.Label) || len(rendition.Label) > 2048 || !utf8.ValidString(rendition.Label) ||
		!model.IsValidLyricsSourceRenditionKind(rendition.Kind) || rendition.SourceTabPaths == nil || len(rendition.SourceTabPaths) == 0 || len(rendition.SourceTabPaths) > 32 ||
		rendition.AvailableVersions == nil || len(rendition.AvailableVersions) == 0 || rendition.Performers == nil || len(rendition.Performers) > 256 || rendition.Provenance == nil || len(rendition.Provenance) == 0 || len(rendition.Provenance) > 128 {
		return errors.New("public v3 rendition header is invalid")
	}
	expectedVersions := []string{}
	if rendition.Full != nil {
		expectedVersions = append(expectedVersions, "full")
	}
	if rendition.Game != nil {
		expectedVersions = append(expectedVersions, "game")
	}
	if !samePublicV3Strings(rendition.AvailableVersions, expectedVersions) {
		return errors.New("public v3 rendition available versions are not lossless")
	}
	if !model.IsValidLyricsSourceRenditionRelationKind(rendition.Relation.Kind) {
		return errors.New("public v3 rendition relation is invalid")
	}
	if err := validatePublicV3TabPaths(rendition.SourceTabPaths); err != nil {
		return err
	}
	performers := make(map[string]struct{}, len(rendition.Performers))
	lastPerformer := ""
	for _, performer := range rendition.Performers {
		if performer.PerformerID == "" || performer.PerformerID <= lastPerformer || performer.PerformerID != strings.TrimSpace(performer.PerformerID) ||
			len(performer.PerformerID) > 128 || len(performer.Name) == 0 || len(performer.Name) > 2048 ||
			performer.Name != strings.TrimSpace(performer.Name) || !utf8.ValidString(performer.PerformerID) ||
			!utf8.ValidString(performer.Name) || strings.ContainsAny(performer.PerformerID, "\r\n\x00") || strings.ContainsAny(performer.Name, "\r\n\x00") ||
			performer.Color != "" && !isPublicV3Color(performer.Color) {
			return errors.New("public v3 rendition performers are invalid")
		}
		lastPerformer = performer.PerformerID
		performers[performer.PerformerID] = struct{}{}
	}
	if err := validatePublicV3Side(rendition.Full, rendition.Kind, performers); err != nil {
		return err
	}
	if err := validatePublicV3Side(rendition.Game, rendition.Kind, performers); err != nil {
		return err
	}
	if rendition.Relation.Kind == model.LyricsSourceRenditionRelationNone {
		if rendition.Relation.FullRenditionKey != "" || rendition.Relation.LineIDs != nil {
			return errors.New("public v3 none relation contains projection data")
		}
	} else {
		if rendition.Full == nil || rendition.Game == nil || rendition.Relation.FullRenditionKey != rendition.Key || rendition.Relation.LineIDs == nil || len(rendition.Relation.LineIDs) != len(rendition.Game.Lines) {
			return errors.New("public v3 exact projection relation is incomplete")
		}
		fullLines := make(map[string]PublicLyricsV3Line, len(rendition.Full.Lines))
		for _, line := range rendition.Full.Lines {
			fullLines[line.ID] = line
		}
		seen := make(map[string]struct{}, len(rendition.Relation.LineIDs))
		previousOrder := -1
		for index, lineID := range rendition.Relation.LineIDs {
			fullLine, found := fullLines[lineID]
			if !found {
				return errors.New("public v3 exact projection references an unknown Full line")
			}
			if _, duplicate := seen[lineID]; duplicate {
				return errors.New("public v3 exact projection repeats a Full line")
			}
			if fullLine.Order <= previousOrder {
				return errors.New("public v3 exact projection Full references are out of order")
			}
			gameLine := rendition.Game.Lines[index]
			if fullLine.Japanese != gameLine.Japanese {
				return errors.New("public v3 exact projection Game text differs from Full")
			}
			if fullLine.Chinese != gameLine.Chinese || fullLine.English != gameLine.English {
				return errors.New("public v3 exact projection Game translations differ from Full")
			}
			seen[lineID] = struct{}{}
			previousOrder = fullLine.Order
		}
	}
	if rendition.TranslationCredits != nil {
		credits := rendition.TranslationCredits
		if credits.Translation != strings.TrimSpace(credits.Translation) || credits.Proofreading != strings.TrimSpace(credits.Proofreading) ||
			len(credits.Translation) > 2048 || len(credits.Proofreading) > 2048 || !utf8.ValidString(credits.Translation) || !utf8.ValidString(credits.Proofreading) || credits.Translation == "" && credits.Proofreading == "" {
			return errors.New("public v3 rendition translation credits are invalid")
		}
	}
	lastComponentRank := -1
	seenComponents := make(map[string]struct{}, len(rendition.Provenance))
	componentPrefix := "renditions/" + rendition.Key + "/"
	for _, attribution := range rendition.Provenance {
		if !strings.HasPrefix(attribution.Component, componentPrefix) || attribution.Component == componentPrefix ||
			len(attribution.Component) > 512 || !model.IsValidLyricsSourceProvider(attribution.Provider) ||
			attribution.Title == "" || attribution.Title != strings.TrimSpace(attribution.Title) || len(attribution.Title) > 2048 ||
			!validPublicV3RevisionURL(attribution) ||
			attribution.LicenseName == "" || len(attribution.LicenseName) > 512 ||
			!strings.HasPrefix(attribution.LicenseURL, "https://") || len(attribution.LicenseURL) > 4096 ||
			!utf8.ValidString(attribution.Component) || !utf8.ValidString(attribution.Title) ||
			!utf8.ValidString(attribution.RevisionURL) || !utf8.ValidString(attribution.LicenseName) ||
			!utf8.ValidString(attribution.LicenseURL) ||
			containsPublicV3Control(attribution.Component) || containsPublicV3Control(attribution.Title) ||
			containsPublicV3Control(attribution.RevisionURL) || containsPublicV3Control(attribution.LicenseName) ||
			containsPublicV3Control(attribution.LicenseURL) {
			return errors.New("public v3 rendition provenance is invalid")
		}
		licenseName, licenseURL := publicLyricsProviderLicense(attribution.Provider)
		if attribution.LicenseName != licenseName || attribution.LicenseURL != licenseURL {
			return errors.New("public v3 rendition source license does not match its provider")
		}
		if _, duplicate := seenComponents[attribution.Component]; duplicate {
			return errors.New("public v3 rendition provenance repeats a component")
		}
		seenComponents[attribution.Component] = struct{}{}
		component := model.LyricsSourceRenditionComponentKind(strings.TrimPrefix(attribution.Component, componentPrefix))
		rank := model.LyricsSourceRenditionComponentRank(component)
		if rank < 0 || rank <= lastComponentRank {
			return errors.New("public v3 rendition provenance is not in canonical component order")
		}
		lastComponentRank = rank
	}
	hasComponent := func(component string) bool {
		_, ok := seenComponents[componentPrefix+component]
		return ok
	}
	if !hasComponent("relation") || !hasComponent("version") {
		return errors.New("public v3 rendition provenance is missing relation or version evidence")
	}
	if rendition.Full != nil && !hasComponent("full_text") {
		return errors.New("public v3 rendition Full side is missing full_text provenance")
	}
	if rendition.Full == nil && (hasComponent("full_text") || hasComponent("full_performer_segmentation") || hasComponent("full_ruby")) {
		return errors.New("public v3 rendition has Full provenance without a Full side")
	}
	if rendition.Game != nil && rendition.Relation.Kind == model.LyricsSourceRenditionRelationNone && !hasComponent("game_text") {
		return errors.New("public v3 independent Game side is missing game_text provenance")
	}
	if rendition.Game == nil && (hasComponent("game_text") || hasComponent("game_performer_segmentation") || hasComponent("game_ruby")) {
		return errors.New("public v3 rendition has Game provenance without a Game side")
	}
	return nil
}

func validPublicV3RevisionURL(attribution PublicLyricsV3ComponentAttribution) bool {
	if attribution.RevisionID <= 0 || attribution.RevisionURL == "" ||
		len(attribution.RevisionURL) > 4096 || attribution.RevisionURL != strings.TrimSpace(attribution.RevisionURL) {
		return false
	}
	parsed, err := url.Parse(attribution.RevisionURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Opaque != "" || parsed.Host == "" || parsed.Path == "" || parsed.ForceQuery {
		return false
	}
	if !model.LyricsSourceProviderAcceptsOrigin(attribution.Provider, parsed.Scheme+"://"+parsed.Host) {
		return false
	}
	if attribution.Provider == model.LyricsSourceProviderMoegirlPublicExact {
		return parsed.RawQuery == "" && parsed.EscapedPath() != "/" &&
			strings.HasPrefix(parsed.EscapedPath(), "/") && parsed.String() == attribution.RevisionURL
	}
	query := parsed.Query()
	if len(query["oldid"]) != 1 || query.Get("oldid") != strconv.Itoa(attribution.RevisionID) ||
		parsed.RawQuery != query.Encode() {
		return false
	}
	switch attribution.Provider {
	case model.LyricsSourceProviderVocaloidFandom, model.LyricsSourceProviderSekaipedia:
		return strings.HasPrefix(parsed.EscapedPath(), "/wiki/") && parsed.EscapedPath() != "/wiki/" && len(query) == 1
	case model.LyricsSourceProviderMoegirl:
		if strings.HasPrefix(parsed.EscapedPath(), "/wiki/") && parsed.EscapedPath() != "/wiki/" {
			return len(query) == 1
		}
		return parsed.EscapedPath() == "/index.php" && len(query) == 2 &&
			len(query["title"]) == 1 && strings.TrimSpace(query.Get("title")) != ""
	default:
		return false
	}
}

func containsPublicV3Control(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' || c == '\n' || c == 0 {
			return true
		}
	}
	return false
}

func validatePublicV3Side(side *PublicLyricsV3Side, kind model.LyricsSourceRenditionKind, performers map[string]struct{}) error {
	if side == nil {
		return nil
	}
	if side.Version.Kind != string(kind) || side.Version.Label == "" || side.Version.Label != strings.TrimSpace(side.Version.Label) || len(side.Version.Label) > 2048 || side.Lines == nil || len(side.Lines) == 0 || len(side.Lines) > maxLyricsLines {
		return errors.New("public v3 rendition side is invalid")
	}
	seenLines := make(map[string]struct{}, len(side.Lines))
	for index, line := range side.Lines {
		if line.ID == "" || line.ID != strings.TrimSpace(line.ID) || len(line.ID) > 128 || line.Order != index ||
			line.Japanese == "" || len(line.Japanese) > maxLyricsLineTextBytes ||
			!utf8.ValidString(line.ID) || !utf8.ValidString(line.Japanese) ||
			containsPublicV3Control(line.ID) || containsPublicV3Control(line.Japanese) ||
			line.Segments == nil || len(line.Segments) == 0 || len(line.Segments) > maxLyricsSegmentsPerLine || line.TrailingPerformerIDs == nil {
			return fmt.Errorf("public v3 side line %d is invalid", index+1)
		}
		if _, duplicate := seenLines[line.ID]; duplicate {
			return errors.New("public v3 side repeats a line ID")
		}
		seenLines[line.ID] = struct{}{}
		if len(line.Chinese) > maxLyricsLineTextBytes || len(line.English) > maxLyricsLineTextBytes || !utf8.ValidString(line.Chinese) || !utf8.ValidString(line.English) {
			return fmt.Errorf("public v3 side line %d translation is invalid", index+1)
		}
		if err := validatePublicV3PerformerIDs(line.TrailingPerformerIDs, performers); err != nil {
			return fmt.Errorf("public v3 side line %d trailing performers: %w", index+1, err)
		}
		lineOffset := 0
		for segmentIndex, segment := range line.Segments {
			if segment.Text == "" || len(segment.Text) > maxLyricsLineTextBytes || !utf8.ValidString(segment.Text) ||
				containsPublicV3Control(segment.Text) || segment.PerformerIDs == nil ||
				len(segment.PerformerIDs) > maxLyricsPerformers || segment.Ruby == nil || len(segment.Ruby) == 0 || len(segment.Ruby) > 8192 {
				return fmt.Errorf("public v3 side line %d segment %d is invalid", index+1, segmentIndex+1)
			}
			if err := validatePublicV3PerformerIDs(segment.PerformerIDs, performers); err != nil {
				return fmt.Errorf("public v3 side line %d segment %d performers: %w", index+1, segmentIndex+1, err)
			}
			rubyOffset := 0
			for rubyIndex, span := range segment.Ruby {
				if span.Text == "" || len(span.Text) > maxLyricsLineTextBytes || len(span.Reading) > maxLyricsLineTextBytes ||
					!utf8.ValidString(span.Text) || !utf8.ValidString(span.Reading) ||
					containsPublicV3Control(span.Text) || containsPublicV3Control(span.Reading) {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d is invalid", index+1, segmentIndex+1, rubyIndex+1)
				}
				if publicV3TextContainsHan(span.Text) && span.Reading == "" {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d is missing a Han reading", index+1, segmentIndex+1, rubyIndex+1)
				}
				if span.Reading != "" && !publicV3ReadingBaseSpan(span.Text) {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d reading base contains a non-Han character", index+1, segmentIndex+1, rubyIndex+1)
				}
				if span.Reading != "" && !publicV3KanaReading(span.Reading) {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d reading is not kana", index+1, segmentIndex+1, rubyIndex+1)
				}
				if !strings.HasPrefix(segment.Text[rubyOffset:], span.Text) {
					return fmt.Errorf("public v3 side line %d segment %d ruby does not concatenate", index+1, segmentIndex+1)
				}
				rubyOffset += len(span.Text)
			}
			if rubyOffset != len(segment.Text) {
				return fmt.Errorf("public v3 side line %d segment %d ruby does not concatenate", index+1, segmentIndex+1)
			}
			if !strings.HasPrefix(line.Japanese[lineOffset:], segment.Text) {
				return fmt.Errorf("public v3 side line %d segments do not concatenate", index+1)
			}
			lineOffset += len(segment.Text)
		}
		if lineOffset != len(line.Japanese) {
			return fmt.Errorf("public v3 side line %d segments do not concatenate", index+1)
		}
	}
	return nil
}

func publicV3KanaReading(value string) bool {
	hasKana := false
	for _, current := range value {
		switch {
		case unicode.In(current, unicode.Hiragana, unicode.Katakana):
			hasKana = true
		case current == 'ー' || current == '・' || unicode.Is(unicode.Mn, current) || unicode.Is(unicode.Mc, current):
			if !hasKana {
				return false
			}
		default:
			return false
		}
	}
	return hasKana
}

func publicV3TextContainsHan(text string) bool {
	for _, current := range text {
		if model.LyricsSourceRubyBaseRune(current) {
			return true
		}
	}
	return false
}

func publicV3ReadingBaseSpan(text string) bool {
	for _, current := range text {
		if model.LyricsSourceRubyBaseRune(current) {
			continue
		}
		return false
	}
	return text != ""
}

func validatePublicV3PerformerIDs(ids []string, performers map[string]struct{}) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) <= 8 {
		for i, id := range ids {
			if id == "" || id != strings.TrimSpace(id) || len(id) > 128 || !utf8.ValidString(id) || containsPublicV3Control(id) {
				return errors.New("performer ID is invalid")
			}
			if _, found := performers[id]; !found {
				return errors.New("performer ID is outside the rendition roster")
			}
			for j := 0; j < i; j++ {
				if ids[j] == id {
					return errors.New("performer IDs contain a duplicate")
				}
			}
		}
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 128 || !utf8.ValidString(id) || containsPublicV3Control(id) {
			return errors.New("performer ID is invalid")
		}
		if _, found := performers[id]; !found {
			return errors.New("performer ID is outside the rendition roster")
		}
		if _, duplicate := seen[id]; duplicate {
			return errors.New("performer IDs contain a duplicate")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validatePublicV3TabPaths(paths []model.LyricsSourceTabPath) error {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == nil || len(path) == 0 || len(path) > 8 {
			return errors.New("public v3 source tab path is invalid")
		}
		key := strings.Join(path, "\x00")
		if _, duplicate := seen[key]; duplicate {
			return errors.New("public v3 source tab paths contain a duplicate")
		}
		seen[key] = struct{}{}
		for _, label := range path {
			if label == "" || label != strings.TrimSpace(label) || len(label) > 512 || !utf8.ValidString(label) || strings.ContainsAny(label, "\r\n\x00") {
				return errors.New("public v3 source tab path label is invalid")
			}
		}
	}
	return nil
}

func canonicalPublicV3Timestamp(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Unix() > 0 && strings.HasSuffix(value, "Z") && parsed.UTC().Format(time.RFC3339Nano) == value
}

func samePublicV3Strings(left, right []string) bool {
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

func publicV3LineIDs(side *PublicLyricsV3Side) []string {
	if side == nil {
		return nil
	}
	result := make([]string, len(side.Lines))
	for index, line := range side.Lines {
		result[index] = line.ID
	}
	return result
}
