package translator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"moesekai/server/internal/store"
)

// Masterdata records reduced to the fields the side-story catalog reads.
type sideStoryCard struct {
	ID              int    `json:"id"`
	CharacterID     int    `json:"characterId"`
	Prefix          string `json:"prefix"`
	AssetbundleName string `json:"assetbundleName"`
	ReleaseAt       int64  `json:"releaseAt"`
}

type sideStoryCardEpisode struct {
	CardID          int    `json:"cardId"`
	Seq             int    `json:"seq"`
	Title           string `json:"title"`
	ScenarioID      string `json:"scenarioId"`
	AssetbundleName string `json:"assetbundleName"`
}

type sideStoryActionSet struct {
	ID                 int    `json:"id"`
	AreaID             int    `json:"areaId"`
	ScenarioID         string `json:"scenarioId"`
	ReleaseConditionID int    `json:"releaseConditionId"`
	ActionSetType      string `json:"actionSetType"`
	IsNextGrade        *bool  `json:"isNextGrade"`
	ArchivePublishedAt int64  `json:"archivePublishedAt"`
}

type sideStoryArea struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	SubName string `json:"subName"`
}

// sideStoryAreaCategory ports getAreaCategory and categoryToUrlParam from the
// main site's story/area/areaCategory.ts; "" means the talk is not listed.
func sideStoryAreaCategory(action sideStoryActionSet) string {
	cond := strconv.Itoa(action.ReleaseConditionID)
	hasScenario := action.ScenarioID != ""
	if hasScenario && len(cond) == 6 && cond[0] == '1' {
		eventIndex, _ := strconv.Atoi(cond[1:4])
		return "event_" + strconv.Itoa(eventIndex+1)
	}
	if action.ID == 2373 {
		return "event_145"
	}
	if hasScenario && strings.Contains(action.ScenarioID, "aprilfool") {
		parts := strings.Split(action.ScenarioID, "_")
		if len(parts) < 2 {
			return ""
		}
		return parts[1]
	}
	if hasScenario && action.ActionSetType == "limited" {
		return "limited_" + strconv.Itoa(action.AreaID)
	}
	if hasScenario && action.ActionSetType == "normal" && action.IsNextGrade != nil && action.ReleaseConditionID == 1 {
		if *action.IsNextGrade {
			return "grade2"
		}
		return "grade1"
	}
	if hasScenario && action.ReleaseConditionID >= 2000000 && action.ReleaseConditionID <= 2000036 {
		return "theater"
	}
	return ""
}

func sideStoryAreaGroupPath(actionSetID int, scenarioID string) string {
	return fmt.Sprintf("scenario/actionset/group%d/%s", actionSetID/100, scenarioID)
}

// buildSideStoryCardCatalog lists one story per JP card with exactly the
// cardEpisodes seq 1 and 2. CN/EN paths and titles are set only when that
// server's cardEpisodes has the scenarioId.
func buildSideStoryCardCatalog(jpCards []sideStoryCard, jpEpisodes []sideStoryCardEpisode,
	cnCards []sideStoryCard, cnEpisodes, enEpisodes []sideStoryCardEpisode) []store.SideStoryCatalogStory {
	cnBundles := map[int]string{}
	for _, card := range cnCards {
		if _, seen := cnBundles[card.ID]; !seen {
			cnBundles[card.ID] = card.AssetbundleName
		}
	}
	cnByScenario, enByScenario := sideStoryEpisodesByScenario(cnEpisodes), sideStoryEpisodesByScenario(enEpisodes)
	episodesByCard := map[int][]sideStoryCardEpisode{}
	for _, episode := range jpEpisodes {
		episodesByCard[episode.CardID] = append(episodesByCard[episode.CardID], episode)
	}
	seen := map[int]bool{}
	stories := []store.SideStoryCatalogStory{}
	for _, card := range jpCards {
		cardID := strconv.Itoa(card.ID)
		episodes := episodesByCard[card.ID]
		if seen[card.ID] || !store.ValidSideStoryID(store.SideStoryKindCard, cardID) || len(episodes) != 2 {
			continue
		}
		seen[card.ID] = true
		sort.Slice(episodes, func(i, j int) bool { return episodes[i].Seq < episodes[j].Seq })
		story := store.SideStoryCatalogStory{
			Kind: store.SideStoryKindCard, StoryID: cardID, Title: card.Prefix,
			CharacterID: max(card.CharacterID, 0), ReleasedAt: max(card.ReleaseAt, 0),
		}
		for index, episode := range episodes {
			bundle := card.AssetbundleName
			if bundle == "" {
				bundle = episode.AssetbundleName
			}
			if episode.Seq != index+1 || bundle == "" || !store.ValidSideStoryID(store.SideStoryKindArea, episode.ScenarioID) {
				story.Episodes = nil
				break
			}
			entry := store.SideStoryCatalogEpisode{
				Key: strconv.Itoa(episode.Seq), ScenarioID: episode.ScenarioID, TitleJP: episode.Title, Position: episode.Seq,
				JPAssetPath: "character/member/" + bundle + "/" + episode.ScenarioID,
			}
			if cn, ok := cnByScenario[episode.ScenarioID]; ok {
				entry.CNTitle = cn.Title
				if cnBundle := cnBundles[cn.CardID]; cnBundle != "" {
					entry.CNAssetPath = "character/member/" + cnBundle + "/" + episode.ScenarioID
				}
			}
			if en, ok := enByScenario[episode.ScenarioID]; ok {
				entry.ENTitle = en.Title
				if en.AssetbundleName != "" {
					entry.ENAssetPath = "character/member_scenario/" + en.AssetbundleName + "/" + episode.ScenarioID
				}
			}
			story.Episodes = append(story.Episodes, entry)
		}
		if len(story.Episodes) == 2 {
			stories = append(stories, story)
		}
	}
	return stories
}

func sideStoryEpisodesByScenario(episodes []sideStoryCardEpisode) map[string]sideStoryCardEpisode {
	out := make(map[string]sideStoryCardEpisode, len(episodes))
	for _, episode := range episodes {
		if _, seen := out[episode.ScenarioID]; !seen && episode.ScenarioID != "" {
			out[episode.ScenarioID] = episode
		}
	}
	return out
}

// buildSideStoryAreaCatalog lists one story per JP actionSet with a listed
// category. The script group of each server comes from that server's
// actionSet id, which differs from the JP id for some talks.
func buildSideStoryAreaCatalog(jpActionSets []sideStoryActionSet, areas []sideStoryArea,
	cnActionSets, enActionSets []sideStoryActionSet) []store.SideStoryCatalogStory {
	areaTitles := map[int]string{}
	for _, area := range areas {
		if _, seen := areaTitles[area.ID]; seen {
			continue
		}
		title := area.Name
		if area.SubName != "" {
			title += " - " + area.SubName
		}
		areaTitles[area.ID] = title
	}
	cnIDs, enIDs := sideStoryActionSetIDs(cnActionSets), sideStoryActionSetIDs(enActionSets)
	sorted := append([]sideStoryActionSet(nil), jpActionSets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	seen := map[string]bool{}
	stories := []store.SideStoryCatalogStory{}
	for _, action := range sorted {
		scenarioID := action.ScenarioID
		category := sideStoryAreaCategory(action)
		if action.ID <= 0 || category == "" || seen[scenarioID] || !store.ValidSideStoryID(store.SideStoryKindArea, scenarioID) {
			continue
		}
		seen[scenarioID] = true
		episode := store.SideStoryCatalogEpisode{
			Key: "1", ScenarioID: scenarioID, JPAssetPath: sideStoryAreaGroupPath(action.ID, scenarioID),
		}
		if id, ok := cnIDs[scenarioID]; ok {
			episode.CNAssetPath = sideStoryAreaGroupPath(id, scenarioID)
		}
		if id, ok := enIDs[scenarioID]; ok {
			episode.ENAssetPath = sideStoryAreaGroupPath(id, scenarioID)
		}
		stories = append(stories, store.SideStoryCatalogStory{
			Kind: store.SideStoryKindArea, StoryID: scenarioID, Title: areaTitles[action.AreaID],
			AreaID: max(action.AreaID, 0), AreaCategory: category, ActionSetID: action.ID,
			ReleasedAt: max(action.ArchivePublishedAt, 0), Episodes: []store.SideStoryCatalogEpisode{episode},
		})
	}
	return stories
}

// sideStoryActionSetIDs maps each scenarioId to its lowest positive actionSet id.
func sideStoryActionSetIDs(actionSets []sideStoryActionSet) map[string]int {
	out := make(map[string]int, len(actionSets))
	for _, action := range actionSets {
		if action.ScenarioID == "" || action.ID <= 0 {
			continue
		}
		if current, seen := out[action.ScenarioID]; !seen || action.ID < current {
			out[action.ScenarioID] = action.ID
		}
	}
	return out
}

// fetchSideStoryCatalogContext fetches the masterdata of one kind, one file
// after another, and builds its catalog.
func (t *Translator) fetchSideStoryCatalogContext(ctx context.Context, pacer *sideStoryPacer, kind string) ([]store.SideStoryCatalogStory, error) {
	if kind == store.SideStoryKindCard {
		jpCards, err := fetchSideStoryMasterdata[sideStoryCard](ctx, t, pacer, "jp", "cards.json")
		if err != nil {
			return nil, err
		}
		jpEpisodes, err := fetchSideStoryMasterdata[sideStoryCardEpisode](ctx, t, pacer, "jp", "cardEpisodes.json")
		if err != nil {
			return nil, err
		}
		cnCards, err := fetchSideStoryMasterdata[sideStoryCard](ctx, t, pacer, "cn", "cards.json")
		if err != nil {
			return nil, err
		}
		cnEpisodes, err := fetchSideStoryMasterdata[sideStoryCardEpisode](ctx, t, pacer, "cn", "cardEpisodes.json")
		if err != nil {
			return nil, err
		}
		enEpisodes, err := fetchSideStoryMasterdata[sideStoryCardEpisode](ctx, t, pacer, "en", "cardEpisodes.json")
		if err != nil {
			return nil, err
		}
		return buildSideStoryCardCatalog(jpCards, jpEpisodes, cnCards, cnEpisodes, enEpisodes), nil
	}
	jpActionSets, err := fetchSideStoryMasterdata[sideStoryActionSet](ctx, t, pacer, "jp", "actionSets.json")
	if err != nil {
		return nil, err
	}
	areas, err := fetchSideStoryMasterdata[sideStoryArea](ctx, t, pacer, "jp", "areas.json")
	if err != nil {
		return nil, err
	}
	cnActionSets, err := fetchSideStoryMasterdata[sideStoryActionSet](ctx, t, pacer, "cn", "actionSets.json")
	if err != nil {
		return nil, err
	}
	enActionSets, err := fetchSideStoryMasterdata[sideStoryActionSet](ctx, t, pacer, "en", "actionSets.json")
	if err != nil {
		return nil, err
	}
	return buildSideStoryAreaCatalog(jpActionSets, areas, cnActionSets, enActionSets), nil
}
