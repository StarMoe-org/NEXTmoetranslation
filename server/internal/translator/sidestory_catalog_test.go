package translator

import (
	"reflect"
	"testing"

	"moesekai/server/internal/store"
)

func boolRef(value bool) *bool { return &value }

// Every branch of getAreaCategory in the main site's
// story/area/areaCategory.ts, in its order of precedence.
func TestSideStoryAreaCategoryMatchesTheMainSite(t *testing.T) {
	for _, test := range []struct {
		name   string
		action sideStoryActionSet
		want   string
	}{
		{"event from a six-digit condition", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_ev_test_01", ReleaseConditionID: 114501}, "event_146"},
		{"event 1", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_ev_test_01", ReleaseConditionID: 100001, ActionSetType: "limited"}, "event_1"},
		{"event condition needs a scenario", sideStoryActionSet{ID: 10, ReleaseConditionID: 114501}, ""},
		{"event condition must start with 1", sideStoryActionSet{ID: 10, ScenarioID: "x", ReleaseConditionID: 214501}, ""},
		{"seven digits are not an event", sideStoryActionSet{ID: 10, ScenarioID: "x", ReleaseConditionID: 1145010, ActionSetType: "limited", AreaID: 3}, "limited_3"},
		{"negative condition is not an event", sideStoryActionSet{ID: 10, ScenarioID: "x", ReleaseConditionID: -12345}, ""},
		{"2373 is event 145", sideStoryActionSet{ID: 2373, ScenarioID: "areatalk_mzk5", ReleaseConditionID: 7, ActionSetType: "limited", AreaID: 9}, "event_145"},
		{"2373 without scenario", sideStoryActionSet{ID: 2373}, "event_145"},
		{"event condition wins over 2373", sideStoryActionSet{ID: 2373, ScenarioID: "x", ReleaseConditionID: 100051}, "event_1"},
		{"aprilfool before limited", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_aprilfool2022_01", ActionSetType: "limited", AreaID: 4}, "aprilfool2022"},
		{"aprilfool without a second part", sideStoryActionSet{ID: 10, ScenarioID: "aprilfool2023", ActionSetType: "limited", AreaID: 4}, ""},
		{"limited", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_limited_01", ActionSetType: "limited", AreaID: 12}, "limited_12"},
		{"grade1", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_g1", ActionSetType: "normal", IsNextGrade: boolRef(false), ReleaseConditionID: 1}, "grade1"},
		{"grade2", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_g2", ActionSetType: "normal", IsNextGrade: boolRef(true), ReleaseConditionID: 1}, "grade2"},
		{"grade needs isNextGrade", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_g", ActionSetType: "normal", ReleaseConditionID: 1}, ""},
		{"grade needs condition 1", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_g", ActionSetType: "normal", IsNextGrade: boolRef(false), ReleaseConditionID: 2}, ""},
		{"grade needs normal", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_g", ActionSetType: "special", IsNextGrade: boolRef(true), ReleaseConditionID: 1}, ""},
		{"theater lower bound", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_th", ReleaseConditionID: 2000000}, "theater"},
		{"theater upper bound", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_th", ReleaseConditionID: 2000036}, "theater"},
		{"above theater", sideStoryActionSet{ID: 10, ScenarioID: "areatalk_th", ReleaseConditionID: 2000037}, ""},
		{"theater needs a scenario", sideStoryActionSet{ID: 10, ReleaseConditionID: 2000001}, ""},
		{"empty", sideStoryActionSet{ID: 10, ScenarioID: "op_02area", ActionSetType: "normal", ReleaseConditionID: 5}, ""},
	} {
		if got := sideStoryAreaCategory(test.action); got != test.want {
			t.Errorf("%s: category = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestBuildSideStoryCardCatalogUsesEachServersBundleAndPath(t *testing.T) {
	jpCards := []sideStoryCard{
		{ID: 7, CharacterID: 3, Prefix: "テストカード", AssetbundleName: "res_jp007", ReleaseAt: 1700000000000},
		{ID: 8, Prefix: "片方", AssetbundleName: "res_jp008"},
		{ID: 9, Prefix: "三話", AssetbundleName: "res_jp009"},
	}
	jpEpisodes := []sideStoryCardEpisode{
		{CardID: 7, Seq: 2, Title: "後編", ScenarioID: "test_007_02"},
		{CardID: 7, Seq: 1, Title: "前編", ScenarioID: "test_007_01"},
		{CardID: 8, Seq: 1, Title: "前編", ScenarioID: "test_008_01"},
		{CardID: 9, Seq: 1, ScenarioID: "test_009_01"}, {CardID: 9, Seq: 1, ScenarioID: "test_009_02"},
	}
	cnCards := []sideStoryCard{{ID: 7, AssetbundleName: "res_cn007"}}
	cnEpisodes := []sideStoryCardEpisode{{CardID: 7, Seq: 1, Title: "上篇", ScenarioID: "test_007_01"}}
	enEpisodes := []sideStoryCardEpisode{
		{CardID: 7, Seq: 1, Title: "Part 1", ScenarioID: "test_007_01", AssetbundleName: "res_en007"},
		{CardID: 7, Seq: 2, Title: "Part 2", ScenarioID: "test_007_02", AssetbundleName: "res_en007"},
	}
	got := buildSideStoryCardCatalog(jpCards, jpEpisodes, cnCards, cnEpisodes, enEpisodes)
	want := []store.SideStoryCatalogStory{{
		Kind: store.SideStoryKindCard, StoryID: "7", Title: "テストカード", CharacterID: 3, ReleasedAt: 1700000000000,
		Episodes: []store.SideStoryCatalogEpisode{
			{Key: "1", ScenarioID: "test_007_01", TitleJP: "前編", Position: 1,
				JPAssetPath: "character/member/res_jp007/test_007_01", CNAssetPath: "character/member/res_cn007/test_007_01",
				ENAssetPath: "character/member_scenario/res_en007/test_007_01", CNTitle: "上篇", ENTitle: "Part 1"},
			{Key: "2", ScenarioID: "test_007_02", TitleJP: "後編", Position: 2,
				JPAssetPath: "character/member/res_jp007/test_007_02",
				ENAssetPath: "character/member_scenario/res_en007/test_007_02", ENTitle: "Part 2"},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("card catalog =\n%+v\nwant\n%+v", got, want)
	}
}

func TestBuildSideStoryAreaCatalogComputesEachServersGroupFromItsOwnID(t *testing.T) {
	jpActionSets := []sideStoryActionSet{
		{ID: 1300, AreaID: 5, ScenarioID: "areatalk_ev_shuffle_test", ReleaseConditionID: 100001, ArchivePublishedAt: 1700000000000},
		{ID: 1220, AreaID: 6, ScenarioID: "areatalk_limited_test", ActionSetType: "limited"},
		{ID: 1221, AreaID: 6, ScenarioID: "op_02area_test", ActionSetType: "normal", ReleaseConditionID: 5},
		{ID: 1222, AreaID: 6, ScenarioID: "bad id!", ActionSetType: "limited"},
		{ID: 1223, AreaID: 6, ScenarioID: "areatalk_limited_test", ActionSetType: "limited"},
	}
	areas := []sideStoryArea{{ID: 5, Name: "テスト広場", SubName: "北"}, {ID: 6, Name: "テスト教室"}}
	cnActionSets := []sideStoryActionSet{{ID: 1299, ScenarioID: "areatalk_ev_shuffle_test"}, {ID: 99902, ScenarioID: "areatalk_limited_test"}}
	enActionSets := []sideStoryActionSet{{ID: 1455, ScenarioID: "areatalk_ev_shuffle_test"}}
	got := buildSideStoryAreaCatalog(jpActionSets, areas, cnActionSets, enActionSets)
	want := []store.SideStoryCatalogStory{
		{Kind: store.SideStoryKindArea, StoryID: "areatalk_limited_test", Title: "テスト教室", AreaID: 6,
			AreaCategory: "limited_6", ActionSetID: 1220, Episodes: []store.SideStoryCatalogEpisode{{
				Key: "1", ScenarioID: "areatalk_limited_test",
				JPAssetPath: "scenario/actionset/group12/areatalk_limited_test",
				CNAssetPath: "scenario/actionset/group999/areatalk_limited_test",
			}}},
		{Kind: store.SideStoryKindArea, StoryID: "areatalk_ev_shuffle_test", Title: "テスト広場 - 北", AreaID: 5,
			AreaCategory: "event_1", ActionSetID: 1300, ReleasedAt: 1700000000000, Episodes: []store.SideStoryCatalogEpisode{{
				Key: "1", ScenarioID: "areatalk_ev_shuffle_test",
				JPAssetPath: "scenario/actionset/group13/areatalk_ev_shuffle_test",
				CNAssetPath: "scenario/actionset/group12/areatalk_ev_shuffle_test",
				ENAssetPath: "scenario/actionset/group14/areatalk_ev_shuffle_test",
			}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("area catalog =\n%+v\nwant\n%+v", got, want)
	}
}
