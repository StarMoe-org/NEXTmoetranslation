package model

// Source priority: pinned > human > cn > llm > unknown
const (
	SourceCN      = "cn"
	SourceHuman   = "human"
	SourcePinned  = "pinned"
	SourceLLM     = "llm"
	SourceUnknown = "unknown"
)

func IsValidSource(source string) bool {
	switch source {
	case SourceCN, SourceHuman, SourcePinned, SourceLLM, SourceUnknown:
		return true
	default:
		return false
	}
}

const (
	LocaleJapanese = "ja-JP"
	LocaleChinese  = "zh-CN"
	LocaleEnglish  = "en-US"
)

var SupportedLocales = []string{LocaleJapanese, LocaleChinese, LocaleEnglish}

func IsValidLocale(locale string) bool {
	for _, supported := range SupportedLocales {
		if locale == supported {
			return true
		}
	}
	return false
}

// SupportedCategories are the flat translation categories (event stories
// are handled separately). Order is preserved for stable category listing.
// gachaInfo holds the multi-KB gacha texts apart from gacha (names) so the
// main site does not load them on every page.
var SupportedCategories = []string{
	"cards", "skills", "events", "information", "music", "gacha", "gachaInfo", "virtualLive",
	"sticker", "comic", "mysekai", "costumes", "characters", "units",
}

func IsValidCategory(category string) bool {
	for _, c := range SupportedCategories {
		if c == category {
			return true
		}
	}
	return false
}

// IsRestoreOptionalCategory reports categories added after translation exports
// and legacy seeds already existed. An import that predates one, or was taken
// before its first sync, restores it as empty.
func IsRestoreOptionalCategory(category string) bool {
	return category == "gachaInfo"
}

// Entry is a single translation row in the .full.json format:
//
//	{ "text": ..., "source": ..., "ids": [...] }
type Entry struct {
	Text   string   `json:"text"`
	Source string   `json:"source"`
	Ids    []string `json:"ids,omitempty"`
}

// Category is field -> { jpKey -> Entry }, matching X.full.json on disk.
type Category map[string]map[string]Entry

// EntryWithKey is an entry returned to the console API with its jp key.
type EntryWithKey struct {
	Key       string   `json:"key"`
	Text      string   `json:"text"`
	Source    string   `json:"source"`
	Ids       []string `json:"ids,omitempty"`
	UpdatedAt int64    `json:"updatedAt,omitempty"`
}

// CategoryLocaleSnapshot is an authenticated, point-in-time editing view of a
// complete category. Revision is opaque to clients and must be echoed as the
// baseRevision of a batch mutation.
type CategoryLocaleSnapshot struct {
	Category string                    `json:"category"`
	Locale   string                    `json:"locale"`
	Revision string                    `json:"revision"`
	Fields   map[string][]EntryWithKey `json:"fields"`
}

type CategoryEntryUpdate struct {
	Field  string `json:"field"`
	Key    string `json:"key"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

// FieldInfo holds per-field counts for the sidebar.
type FieldInfo struct {
	Name         string `json:"name"`
	Total        int    `json:"total"`
	CnCount      int    `json:"cnCount"`
	HumanCount   int    `json:"humanCount"`
	PinnedCount  int    `json:"pinnedCount"`
	LlmCount     int    `json:"llmCount"`
	UnknownCount int    `json:"unknownCount"`
}

type CategoryInfo struct {
	Name   string      `json:"name"`
	Fields []FieldInfo `json:"fields"`
}

// ---- Event story formats (compatible with eventStory/event_N.json) ----

type EventStoryMeta struct {
	Source      string `json:"source"`
	Version     string `json:"version"`
	LastUpdated int64  `json:"last_updated"`
}

type EventStoryEpisode struct {
	ScenarioID   string              `json:"scenarioId"`
	Title        string              `json:"title"`
	TitleSource  string              `json:"titleSource,omitempty"`
	TalkData     map[string]string   `json:"talkData"`
	TalkSources  map[string]string   `json:"talkSources,omitempty"`
	TalkOrder    []string            `json:"talkOrder,omitempty"`
	SpeakerNames map[string]string   `json:"speakerNames,omitempty"`
	Segments     []EventStorySegment `json:"segments,omitempty"`
}

type EventStorySegment struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Position   int    `json:"position"`
	Japanese   string `json:"japanese"`
	SourceHash string `json:"sourceHash"`
	Text       string `json:"text"`
	Source     string `json:"source"`
	Revision   int    `json:"revision,omitempty"`
}

type EventStoryDetail struct {
	Meta     EventStoryMeta               `json:"meta"`
	Episodes map[string]EventStoryEpisode `json:"episodes"`
}

type EventStorySummary struct {
	EventID           int    `json:"eventId"`
	EventName         string `json:"eventName,omitempty"`
	EventNameJapanese string `json:"eventNameJapanese,omitempty"`
	Source            string `json:"source"`
	EpisodeCount      int    `json:"episodeCount"`
	UntranslatedCount int    `json:"untranslatedCount"`
	LastUpdated       int64  `json:"lastUpdated"`
	AllOfficialTagged bool   `json:"allOfficialTagged,omitempty"`
}
