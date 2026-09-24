package translator

import (
	"sort"
	"strings"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

const jpInformationURL = "https://baijing.exmeaning.com/jp/information"

// extractResult is the per-category output: field -> {pairs, trace}.
type extractResult map[string]store.CNApplyField

type cnExtractedCategory struct {
	fields           map[string]store.CNApplyField
	musicCatalog     []store.MusicCatalogRecord
	performerCatalog []store.PerformerCatalogRecord
	err              error
}

func extractCNFields(fn func() (map[string]store.CNApplyField, error)) func() cnExtractedCategory {
	return func() cnExtractedCategory {
		fields, err := fn()
		return cnExtractedCategory{fields: fields, err: err}
	}
}

func (t *Translator) extractMusicCategory() cnExtractedCategory {
	fields, catalog, err := t.extractMusic()
	return cnExtractedCategory{fields: fields, musicCatalog: catalog, err: err}
}

func (t *Translator) extractCharactersCategory() cnExtractedCategory {
	fields, catalog, err := t.extractCharacters()
	return cnExtractedCategory{fields: fields, performerCatalog: catalog, err: err}
}

func newExtractResult(fields ...string) extractResult {
	r := make(extractResult, len(fields))
	for _, f := range fields {
		r[f] = store.CNApplyField{Pairs: map[string]string{}, Trace: map[string][]string{}}
	}
	return r
}

// toCNApply converts extractResult + traceMap into the store apply input.
func (r extractResult) withTrace(tm traceMap) map[string]store.CNApplyField {
	out := make(map[string]store.CNApplyField, len(r))
	for field, f := range r {
		f.Trace = tm[field]
		if f.Trace == nil {
			f.Trace = map[string][]string{}
		}
		out[field] = f
	}
	return out
}

func (t *Translator) extractCards() (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata("cards.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("cards.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, "id")
	out := newExtractResult("prefix", "skillName", "gachaPhrase")
	tm := newTraceMap("prefix", "skillName", "gachaPhrase")
	for _, item := range jp {
		id := getInt(item, "id")
		cnItem := cnByID[id]
		jpPrefix := getString(item, "prefix")
		tm.add("prefix", jpPrefix, id)
		collectPair(out["prefix"].Pairs, jpPrefix, getString(cnItem, "prefix"))
		jpSkill := getString(item, "cardSkillName")
		tm.add("skillName", jpSkill, id)
		collectPair(out["skillName"].Pairs, jpSkill, getString(cnItem, "cardSkillName"))
		phrase := getString(item, "gachaPhrase")
		if phrase != "" && phrase != "-" {
			tm.add("gachaPhrase", phrase, id)
			collectPair(out["gachaPhrase"].Pairs, phrase, getString(cnItem, "gachaPhrase"))
		}
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractSkills() (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata("skills.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("skills.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, "id")
	out := newExtractResult("description")
	tm := newTraceMap("description")
	for _, item := range jp {
		id := getInt(item, "id")
		cnItem := cnByID[id]
		jpDesc := getString(item, "description")
		tm.add("description", jpDesc, id)
		collectPair(out["description"].Pairs, jpDesc, getString(cnItem, "description"))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractInformation() (map[string]store.CNApplyField, error) {
	raw, err := t.fetchJSONURL(jpInformationURL)
	if err != nil {
		return nil, err
	}
	items := toMapSlice(asMap(raw)["informations"])
	out := newExtractResult("title")
	tm := newTraceMap("title")
	for _, item := range items {
		id := getInt(item, "id")
		title := safeText(getString(item, "title"))
		tm.add("title", title, id)
		collectPair(out["title"].Pairs, title, "")
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractSimpleNameByID(file, idField, nameField string) (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata(file, "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata(file, "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, idField)
	out := newExtractResult("name")
	tm := newTraceMap("name")
	for _, item := range jp {
		id := getInt(item, idField)
		jpName := getString(item, nameField)
		tm.add("name", jpName, id)
		collectPair(out["name"].Pairs, jpName, getString(cnByID[id], nameField))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractEvents() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("events.json", "id", "name")
}
func (t *Translator) extractGacha() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("gachas.json", "id", "name")
}

var gachaInfoFields = []string{"summary", "bubbleText", "description"}

// extractGachaInfo registers the JP gachaInformation texts untranslated: the
// CN server's gachaInformation is its own announcement (CN dates, anniversary
// numbering, rules), not a translation of the JP text. Keys keep the exact
// masterdata text, line breaks and surrounding whitespace included.
func (t *Translator) extractGachaInfo() (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata("gachas.json", "jp")
	if err != nil {
		return nil, err
	}
	out := newExtractResult(gachaInfoFields...)
	tm := newTraceMap(gachaInfoFields...)
	for _, item := range jp {
		id := getInt(item, "id")
		jpInfo := asMap(item["gachaInformation"])
		for _, field := range gachaInfoFields {
			jpText := getString(jpInfo, field)
			if strings.TrimSpace(jpText) == "" {
				continue
			}
			tm.addExact(field, jpText, id)
			out[field].Pairs[jpText] = ""
		}
	}
	return out.withTrace(tm), nil
}
func (t *Translator) extractVirtualLive() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("virtualLives.json", "id", "name")
}
func (t *Translator) extractStickers() (map[string]store.CNApplyField, error) {
	return t.extractSimpleNameByID("stamps.json", "id", "name")
}

func (t *Translator) extractComics() (map[string]store.CNApplyField, error) {
	jp, err := t.fetchMasterdata("tips.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("tips.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByID := byIntID(cn, "id")
	out := newExtractResult("title")
	tm := newTraceMap("title")
	for _, item := range jp {
		if getString(item, "assetbundleName") == "" {
			continue
		}
		id := getInt(item, "id")
		jpTitle := getString(item, "title")
		tm.add("title", jpTitle, id)
		collectPair(out["title"].Pairs, jpTitle, getString(cnByID[id], "title"))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractMusic() (map[string]store.CNApplyField, []store.MusicCatalogRecord, error) {
	musics, err := t.fetchMasterdata("musics.json", "jp")
	if err != nil {
		return nil, nil, err
	}
	vocals, err := t.fetchMasterdata("musicVocals.json", "jp")
	if err != nil {
		return nil, nil, err
	}
	out := newExtractResult("title", "artist", "vocalCaption")
	tm := newTraceMap("title", "artist", "vocalCaption")
	vocalSignals := musicVocalSignals(vocals)
	catalog := make([]store.MusicCatalogRecord, 0, len(musics))
	for _, m := range musics {
		musicID := getInt(m, "id")
		if title := getString(m, "title"); title != "" {
			out["title"].Pairs[title] = ""
			tm.add("title", title, musicID)
			newlyWritten, _ := m["isNewlyWrittenMusic"].(bool)
			lyricsVersion, lyricsVersionKnown := catalogLyricsVersion(m)
			catalog = append(catalog, store.MusicCatalogRecord{
				MusicID: musicID, JapaneseTitle: title, IsNewlyWrittenMusic: newlyWritten,
				ProducerMetadata: musicProducerMetadata(m), Lyricist: getString(m, "lyricist"),
				Composer: getString(m, "composer"), Arranger: getString(m, "arranger"),
				AssetbundleName: getString(m, "assetbundleName"), VersionHint: catalogVersionHint(m),
				LyricsVersion: lyricsVersion, LyricsVersionKnown: lyricsVersionKnown, Vocals: vocalSignals[musicID],
			})
		}
		for _, key := range []string{"lyricist", "composer", "arranger"} {
			if v := getString(m, key); v != "" && v != "-" {
				out["artist"].Pairs[v] = ""
				tm.add("artist", v, musicID)
			}
		}
	}
	for _, v := range vocals {
		vocalID := getInt(v, "id")
		if vocalID == 0 {
			vocalID = getInt(v, "musicId")
		}
		if caption := getString(v, "caption"); caption != "" {
			out["vocalCaption"].Pairs[caption] = ""
			tm.add("vocalCaption", caption, vocalID)
		}
	}
	return out.withTrace(tm), catalog, nil
}

func musicVocalSignals(vocals []map[string]any) map[int][]model.CatalogVocalSignal {
	result := map[int][]model.CatalogVocalSignal{}
	for _, vocal := range vocals {
		musicID := getInt(vocal, "musicId")
		if musicID <= 0 {
			continue
		}
		base := model.CatalogVocalSignal{
			VocalID: getInt(vocal, "id"), VocalType: getString(vocal, "musicVocalType"),
			Caption: getString(vocal, "caption"), AssetbundleName: getString(vocal, "assetbundleName"),
		}
		characters := toMapSlice(vocal["characters"])
		if len(characters) == 0 {
			result[musicID] = append(result[musicID], base)
			continue
		}
		for _, character := range characters {
			signal := base
			signal.CharacterType = getString(character, "characterType")
			signal.CharacterID = getInt(character, "characterId")
			signal.CharacterSequence = getInt(character, "seq")
			result[musicID] = append(result[musicID], signal)
		}
	}
	for musicID := range result {
		sort.Slice(result[musicID], func(i, j int) bool {
			left, right := result[musicID][i], result[musicID][j]
			if left.VocalID != right.VocalID {
				return left.VocalID < right.VocalID
			}
			return left.CharacterSequence < right.CharacterSequence
		})
	}
	return result
}

func catalogLyricsVersion(music map[string]any) (string, bool) {
	signals := make([]string, 0, 4)
	if _, exists := music["isFullLength"]; exists {
		full, ok := getBool(music, "isFullLength")
		if !ok {
			signals = append(signals, "unknown")
		} else if full {
			signals = append(signals, "full")
		} else {
			signals = append(signals, "game_size")
		}
	}
	for _, key := range []string{"musicVersion", "lyricsVersion", "versionType"} {
		if _, exists := music[key]; !exists {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(getString(music, key)))
		switch value {
		case "full", "full_length", "full-length", "long":
			signals = append(signals, "full")
		case "game", "game_size", "game-size":
			signals = append(signals, "game_size")
		default:
			signals = append(signals, "unknown")
		}
	}
	if len(signals) == 0 {
		return "unknown", false
	}
	version := signals[0]
	if version == "unknown" {
		return "unknown", true
	}
	for _, signal := range signals[1:] {
		if signal == "unknown" || signal != version {
			return "unknown", true
		}
	}
	return version, true
}

func catalogVersionHint(music map[string]any) string {
	for _, key := range []string{"assetbundleName", "musicAssetbundleName", "versionHint"} {
		if value := strings.TrimSpace(getString(music, key)); value != "" {
			return value
		}
	}
	return ""
}

func musicProducerMetadata(music map[string]any) string {
	values := make([]string, 0, 3)
	for _, key := range []string{"lyricist", "composer", "arranger"} {
		if value := strings.TrimSpace(getString(music, key)); value != "" && value != "-" {
			values = append(values, value)
		}
	}
	return strings.Join(values, " | ")
}

func (t *Translator) extractMysekai() (map[string]store.CNApplyField, error) {
	out := newExtractResult("fixtureName", "flavorText", "genre", "tag")
	tm := newTraceMap("fixtureName", "flavorText", "genre", "tag")

	jpFix, err := t.fetchMasterdata("mysekaiFixtures.json", "jp")
	if err != nil {
		return nil, err
	}
	cnFix, err := t.fetchMasterdata("mysekaiFixtures.json", "cn")
	if err != nil {
		return nil, err
	}
	cnFixByID := byIntID(cnFix, "id")
	for _, f := range jpFix {
		id := getInt(f, "id")
		cnf := cnFixByID[id]
		jpName := getString(f, "name")
		tm.add("fixtureName", jpName, id)
		collectPair(out["fixtureName"].Pairs, jpName, getString(cnf, "name"))
		jpFlavor := getString(f, "flavorText")
		tm.add("flavorText", jpFlavor, id)
		collectPair(out["flavorText"].Pairs, jpFlavor, getString(cnf, "flavorText"))
	}

	jpGenre, err := t.fetchMasterdata("mysekaiFixtureMainGenres.json", "jp")
	if err != nil {
		return nil, err
	}
	cnGenre, err := t.fetchMasterdata("mysekaiFixtureMainGenres.json", "cn")
	if err != nil {
		return nil, err
	}
	cnGenreByID := byIntID(cnGenre, "id")
	for _, g := range jpGenre {
		id := getInt(g, "id")
		jpName := getString(g, "name")
		tm.add("genre", jpName, id)
		collectPair(out["genre"].Pairs, jpName, getString(cnGenreByID[id], "name"))
	}

	jpTag, err := t.fetchMasterdata("mysekaiFixtureTags.json", "jp")
	if err != nil {
		return nil, err
	}
	cnTag, err := t.fetchMasterdata("mysekaiFixtureTags.json", "cn")
	if err != nil {
		return nil, err
	}
	cnTagByID := byIntID(cnTag, "id")
	for _, g := range jpTag {
		id := getInt(g, "id")
		jpName := getString(g, "name")
		tm.add("tag", jpName, id)
		collectPair(out["tag"].Pairs, jpName, getString(cnTagByID[id], "name"))
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractCostumes() (map[string]store.CNApplyField, error) {
	out := newExtractResult("name", "colorName", "designer")
	tm := newTraceMap("name", "colorName", "designer")
	jpRaw, err := t.fetchMasterdataDocument("snowy_costumes.json", "jp")
	if err != nil {
		return nil, err
	}
	cnRaw, err := t.fetchMasterdataDocument("snowy_costumes.json", "cn")
	if err != nil {
		return nil, err
	}
	jpList := toMapSlice(asMap(jpRaw)["costumes"])
	cnList := toMapSlice(asMap(cnRaw)["costumes"])
	cnByID := byIntID(cnList, "id")
	for _, costume := range jpList {
		id := getInt(costume, "id")
		cnCostume := cnByID[id]
		jpName := safeText(getString(costume, "name"))
		tm.add("name", jpName, id)
		collectPair(out["name"].Pairs, jpName, safeText(getString(cnCostume, "name")))
		jpDesigner := safeText(getString(costume, "designer"))
		tm.add("designer", jpDesigner, id)
		collectPair(out["designer"].Pairs, jpDesigner, safeText(getString(cnCostume, "designer")))

		jpParts := toParts(costume["parts"])
		cnParts := toParts(cnCostume["parts"])
		for partType, partList := range jpParts {
			cnPartByAsset := map[string]map[string]any{}
			for _, p := range cnParts[partType] {
				cnPartByAsset[getString(p, "assetbundleName")] = p
			}
			for _, p := range partList {
				jpColor := safeText(getString(p, "colorName"))
				if jpColor == "" {
					continue
				}
				tm.add("colorName", jpColor, id)
				cnColor := safeText(getString(cnPartByAsset[getString(p, "assetbundleName")], "colorName"))
				collectPair(out["colorName"].Pairs, jpColor, cnColor)
			}
		}
	}
	return out.withTrace(tm), nil
}

func (t *Translator) extractCharacters() (map[string]store.CNApplyField, []store.PerformerCatalogRecord, error) {
	fields := []string{"hobby", "specialSkill", "favoriteFood", "hatedFood", "weak", "introduction"}
	out := newExtractResult(fields...)
	tm := newTraceMap(fields...)
	jp, err := t.fetchMasterdata("characterProfiles.json", "jp")
	if err != nil {
		return nil, nil, err
	}
	cn, err := t.fetchMasterdata("characterProfiles.json", "cn")
	if err != nil {
		return nil, nil, err
	}
	cnByID := byIntID(cn, "characterId")
	for _, profile := range jp {
		id := getInt(profile, "characterId")
		cnProfile := cnByID[id]
		for _, field := range fields {
			jpText := safeText(getString(profile, field))
			tm.add(field, jpText, id)
			collectPair(out[field].Pairs, jpText, safeText(getString(cnProfile, field)))
		}
	}
	jpCharacters, err := t.fetchMasterdata("gameCharacters.json", "jp")
	if err != nil {
		return nil, nil, err
	}
	cnCharacters, err := t.fetchMasterdata("gameCharacters.json", "cn")
	if err != nil {
		return nil, nil, err
	}
	cnCharactersByID := byIntID(cnCharacters, "id")
	records := make([]store.PerformerCatalogRecord, 0, len(jpCharacters))
	for _, character := range jpCharacters {
		id := getInt(character, "id")
		records = append(records, store.PerformerCatalogRecord{
			PerformerID:  id,
			JapaneseName: characterName(character),
			ChineseName:  characterName(cnCharactersByID[id]),
		})
	}
	return out.withTrace(tm), records, nil
}

func characterName(character map[string]any) string {
	if name := strings.TrimSpace(getString(character, "name")); name != "" {
		return name
	}
	return strings.TrimSpace(strings.Join([]string{getString(character, "firstName"), getString(character, "givenName")}, " "))
}

func (t *Translator) extractUnits() (map[string]store.CNApplyField, error) {
	out := newExtractResult("unitName", "profileSentence")
	tm := newTraceMap("unitName", "profileSentence")
	jp, err := t.fetchMasterdata("unitProfiles.json", "jp")
	if err != nil {
		return nil, err
	}
	cn, err := t.fetchMasterdata("unitProfiles.json", "cn")
	if err != nil {
		return nil, err
	}
	cnByUnit := map[string]map[string]any{}
	for _, unit := range cn {
		cnByUnit[getString(unit, "unit")] = unit
	}
	for _, unit := range jp {
		id := getString(unit, "unit")
		cnUnit := cnByUnit[id]
		jpUnitName := getString(unit, "unitName")
		tm.addStr("unitName", jpUnitName, id)
		collectPair(out["unitName"].Pairs, jpUnitName, getString(cnUnit, "unitName"))
		jpSentence := getString(unit, "profileSentence")
		tm.addStr("profileSentence", jpSentence, id)
		collectPair(out["profileSentence"].Pairs, jpSentence, getString(cnUnit, "profileSentence"))
	}
	return out.withTrace(tm), nil
}
