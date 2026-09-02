package filesvc

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

//go:embed legacy_game_projection_550.json
var legacyGameProjection550 []byte

var (
	legacyGameDocsOnce sync.Once
	legacyGameDocs     map[int]model.LyricsSourceDocument
	legacyGameDocsErr  error
)

func loadLegacyGameDocuments() (map[int]model.LyricsSourceDocument, error) {
	legacyGameDocsOnce.Do(func() {
		legacyGameDocs = map[int]model.LyricsSourceDocument{}
		sources := map[int][]byte{550: legacyGameProjection550}
		for musicID, body := range sources {
			document, err := model.DecodeLyricsSourceDocument(body)
			if err != nil {
				legacyGameDocsErr = fmt.Errorf("legacy game projection %d: %w", musicID, err)
				legacyGameDocs = nil
				return
			}
			legacyGameDocs[musicID] = document
		}
	})
	return legacyGameDocs, legacyGameDocsErr
}

func applyLegacyGameProjections(
	projected map[string][]byte,
	songs []store.PublicLyricsIndexSong,
	provenance map[int]SongProvenance,
	dbOwned map[int]bool,
) {
	if len(dbOwned) == 0 {
		return
	}
	for musicID := range dbOwned {
		key := fmt.Sprintf("translation/lyrics/music_%d.json", musicID)
		body, ok := projected[key]
		if !ok {
			continue
		}
		converted, versions, err := upconvertLegacyPublication(musicID, body)
		if err != nil {
			log.Printf("[projection] lyrics %d legacy game projection skipped: %v", musicID, err)
			continue
		}
		if converted == nil {
			continue
		}
		projected[key] = converted
		for index := range songs {
			if songs[index].MusicID != musicID {
				continue
			}
			songs[index].State = store.PublicLyricsStateComplete
			songs[index].AvailableVersions = append([]string(nil), versions...)
			break
		}
		entry := provenance[musicID]
		entry.State = string(store.PublicLyricsStateComplete)
		entry.AvailableVersions = append([]string(nil), versions...)
		entry.HasDetail = true
		provenance[musicID] = entry
	}
}

func upconvertLegacyPublication(musicID int, body []byte) ([]byte, []string, error) {
	documents, err := loadLegacyGameDocuments()
	if err != nil {
		return nil, nil, err
	}
	document, ok := documents[musicID]
	if !ok {
		return nil, nil, nil
	}
	var legacy store.PublicLyricsDetailDocument
	if err := json.Unmarshal(body, &legacy); err != nil {
		return nil, nil, err
	}
	if legacy.Version != 1 || legacy.MusicID != musicID || len(legacy.Lines) == 0 {
		return nil, nil, nil
	}
	detail, err := publicV3DetailFromLegacyPublication(document, legacy)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := store.EncodePublicLyricsV3Detail(detail)
	if err != nil {
		return nil, nil, err
	}
	return append(encoded, '\n'), []string{"full", "game"}, nil
}

func publicV3DetailFromLegacyPublication(
	document model.LyricsSourceDocument,
	legacy store.PublicLyricsDetailDocument,
) (store.PublicLyricsV3DetailDocument, error) {
	if len(document.Renditions) != 1 || len(document.FixedIdentities) == 0 {
		return store.PublicLyricsV3DetailDocument{}, fmt.Errorf("legacy game projection document is incomplete")
	}
	rendition := document.Renditions[0]
	if rendition.Full == nil || rendition.Game == nil {
		return store.PublicLyricsV3DetailDocument{}, fmt.Errorf("legacy game projection missing Full or Game")
	}
	if len(legacy.Lines) != len(rendition.Full.Lines) {
		return store.PublicLyricsV3DetailDocument{}, fmt.Errorf("legacy publication line count %d does not match Full %d", len(legacy.Lines), len(rendition.Full.Lines))
	}
	translations := make([]string, len(rendition.Full.Lines))
	for index, line := range rendition.Full.Lines {
		if legacy.Lines[index].Japanese != line.Text {
			return store.PublicLyricsV3DetailDocument{}, fmt.Errorf("legacy publication line %d japanese changed", index+1)
		}
		translations[index] = legacy.Lines[index].Chinese
	}
	full := publicV3SideFromSource(*rendition.Full, translations)
	gameTranslations, err := projectLegacyTranslations(*rendition.Full, rendition.Relation.LineIDs, translations)
	if err != nil {
		return store.PublicLyricsV3DetailDocument{}, err
	}
	game := publicV3SideFromSource(*rendition.Game, gameTranslations)
	identity := document.FixedIdentities[0]
	publicRendition := store.PublicLyricsV3Rendition{
		Key:               rendition.RenditionKey,
		Kind:              rendition.SourceKind,
		Label:             rendition.Full.Version.Label,
		AvailableVersions: []string{"full", "game"},
		Performers:        publicV3PerformersFromSource(rendition.Full, rendition.Game),
		Full:              full,
		Game:              game,
		Relation: store.PublicLyricsV3Relation{
			Kind:             rendition.Relation.Kind,
			FullRenditionKey: rendition.Relation.FullRenditionKey,
			LineIDs:          append([]string(nil), rendition.Relation.LineIDs...),
		},
		SourceTabPaths: cloneTabPaths(rendition.SourceTabPaths),
		Provenance:     sekaipediaComponentProvenance(rendition.RenditionKey, identity),
	}
	if credit := translationCreditFromLegacy(legacy); credit != "" {
		publicRendition.TranslationCredits = &store.PublicLyricsV3TranslationCredits{Translation: credit}
	}
	return store.PublicLyricsV3DetailDocument{
		Version:    3,
		MusicID:    legacy.MusicID,
		Revision:   legacy.Revision,
		UpdatedAt:  legacy.UpdatedAt,
		State:      store.PublicLyricsStateComplete,
		Renditions: []store.PublicLyricsV3Rendition{publicRendition},
	}, nil
}

func publicV3SideFromSource(source model.LyricsSourceFull, translations []string) *store.PublicLyricsV3Side {
	lines := make([]store.PublicLyricsV3Line, len(source.Lines))
	for index, line := range source.Lines {
		publicLine := store.PublicLyricsV3Line{
			ID:                   line.ID,
			Order:                index,
			Japanese:             line.Text,
			StanzaBreakBefore:    line.StanzaBreakBefore,
			TrailingPerformerIDs: cloneNonNilStrings(line.TrailingPerformerIDs),
			Segments:             make([]store.PublicLyricsV3Segment, len(line.Segments)),
		}
		if index < len(translations) {
			publicLine.Chinese = translations[index]
		}
		for segmentIndex, segment := range line.Segments {
			ruby := make([]store.PublicLyricsV3RubySpan, len(segment.Ruby))
			for rubyIndex, span := range segment.Ruby {
				ruby[rubyIndex] = store.PublicLyricsV3RubySpan{Text: span.Text, Reading: span.Reading}
			}
			publicLine.Segments[segmentIndex] = store.PublicLyricsV3Segment{
				Text:         segment.Text,
				PerformerIDs: cloneNonNilStrings(segment.PerformerIDs),
				Ruby:         ruby,
			}
		}
		lines[index] = publicLine
	}
	return &store.PublicLyricsV3Side{Version: source.Version, Lines: lines}
}

func projectLegacyTranslations(full model.LyricsSourceFull, lineIDs, translations []string) ([]string, error) {
	if len(translations) != len(full.Lines) {
		return nil, fmt.Errorf("translation count %d does not match Full %d", len(translations), len(full.Lines))
	}
	byID := make(map[string]string, len(full.Lines))
	for index, line := range full.Lines {
		byID[line.ID] = translations[index]
	}
	projected := make([]string, len(lineIDs))
	for index, lineID := range lineIDs {
		text, ok := byID[lineID]
		if !ok {
			return nil, fmt.Errorf("game projection references unknown Full line %q", lineID)
		}
		projected[index] = text
	}
	return projected, nil
}

func publicV3PerformersFromSource(full, game *model.LyricsSourceFull) []model.LyricsSourcePerformer {
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
	sort.Slice(result, func(left, right int) bool {
		return result[left].PerformerID < result[right].PerformerID
	})
	return result
}

func sekaipediaComponentProvenance(renditionKey string, identity model.LyricsSourceFixedIdentity) []store.PublicLyricsV3ComponentAttribution {
	components := []string{
		"full_text",
		"full_performer_segmentation",
		"full_ruby",
		"game_text",
		"game_performer_segmentation",
		"game_ruby",
		"relation",
		"version",
	}
	result := make([]store.PublicLyricsV3ComponentAttribution, len(components))
	for index, component := range components {
		result[index] = store.PublicLyricsV3ComponentAttribution{
			Component:   "renditions/" + renditionKey + "/" + component,
			Provider:    identity.Provider,
			Title:       identity.Title,
			RevisionID:  identity.RevisionID,
			RevisionURL: identity.CanonicalURL,
			LicenseName: "CC BY-SA 4.0",
			LicenseURL:  "https://creativecommons.org/licenses/by-sa/4.0/",
		}
	}
	return result
}

func translationCreditFromLegacy(legacy store.PublicLyricsDetailDocument) string {
	if legacy.TranslationCredits != nil && legacy.TranslationCredits.Translation != "" {
		return legacy.TranslationCredits.Translation
	}
	return legacy.Attribution
}

func cloneTabPaths(input []model.LyricsSourceTabPath) []model.LyricsSourceTabPath {
	result := make([]model.LyricsSourceTabPath, len(input))
	for index, path := range input {
		result[index] = append(model.LyricsSourceTabPath(nil), path...)
	}
	return result
}

func cloneNonNilStrings(input []string) []string {
	if len(input) == 0 {
		return []string{}
	}
	return append([]string{}, input...)
}
