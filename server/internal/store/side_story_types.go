package store

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

const (
	SideStoryKindCard, SideStoryKindArea                                                                             = "card", "area"
	SideStorySourceOfficial, SideStorySourceLLM, SideStorySourceHuman                                                = "official", "llm", "human"
	SideStoryRoleTitle, SideStoryRoleTalk, SideStoryRoleSpeaker                                                      = "title", "talk", "speaker"
	SideStoryStatePending, SideStoryStateImported, SideStoryStateAbsent, SideStoryStateMismatch, SideStoryStateError = "pending", "imported", "absent", "mismatch", "error"
)

const (
	sideStoryMaxEdits      = 2000
	sideStoryMaxTextBytes  = 16384
	sideStoryMaxQueueLimit = 500
)

var sideStoryAreaIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func ValidSideStoryKind(kind string) bool {
	return kind == SideStoryKindCard || kind == SideStoryKindArea
}

// ValidSideStoryID accepts a card id as canonical positive decimal of at most
// nine digits and an area id as a scenarioId.
func ValidSideStoryID(kind, id string) bool {
	switch kind {
	case SideStoryKindCard:
		if len(id) == 0 || len(id) > 9 || id[0] == '0' {
			return false
		}
		for index := 0; index < len(id); index++ {
			if id[index] < '0' || id[index] > '9' {
				return false
			}
		}
		return true
	case SideStoryKindArea:
		return sideStoryAreaIDPattern.MatchString(id)
	}
	return false
}

// ValidSideStoryEpisodeKey accepts "1" and "2" for a card and "1" for an area.
func ValidSideStoryEpisodeKey(kind, key string) bool {
	switch kind {
	case SideStoryKindCard:
		return key == "1" || key == "2"
	case SideStoryKindArea:
		return key == "1"
	}
	return false
}

func ValidSideStoryLocale(locale string) bool {
	return locale == model.LocaleChinese || locale == model.LocaleEnglish
}

// SideStoryPublicSourceLabel maps a stored source to its public label.
func SideStoryPublicSourceLabel(locale, source string) string {
	if source != SideStorySourceOfficial {
		return source
	}
	if locale == model.LocaleEnglish {
		return "official_en"
	}
	return "official_cn"
}

// Catalog from masterdata (built by the sync writer).
type SideStoryCatalogStory struct {
	Kind, StoryID, Title             string
	CharacterID, AreaID, ActionSetID int
	AreaCategory                     string
	ReleasedAt                       int64 // unix ms (card releaseAt, actionSet archivePublishedAt); 0 unknown
	Episodes                         []SideStoryCatalogEpisode
}

type SideStoryCatalogEpisode struct {
	Key, ScenarioID, TitleJP              string
	Position                              int
	JPAssetPath, CNAssetPath, ENAssetPath string // path below the server's asset base, without ".json"; "" = not on that server
	CNTitle, ENTitle                      string // official episode titles, "" = none
}

type SideStoryCatalogResult struct {
	Stories               int `json:"stories"`
	NewStories            int `json:"newStories"`
	NewEpisodes           int `json:"newEpisodes"`
	OfficialRequeued      int `json:"officialRequeued"`
	OfficialTitlesWritten int `json:"officialTitlesWritten"` // official title rows inserted or replaced
	TitlesReplaced        int `json:"titlesReplaced"`        // title lines replaced after a JP title change
	DroppedHumanTitles    int `json:"droppedHumanTitles"`    // human rows deleted with a replaced JP title
}

type SideStoryWorkItem struct {
	Kind, StoryID, EpisodeKey, ScenarioID string
	JPAssetPath, CNAssetPath, ENAssetPath string
	ScriptSHA256, CNState, ENState        string
	Attempts                              int
}

type SideStoryTalk struct {
	Index         int
	Body, Speaker string
}

type SideStoryScript struct {
	ScenarioID, CanonicalJSON, SHA256 string
	Talks                             []SideStoryTalk
}

type SideStoryFetchOutcome struct {
	Attempted bool             // false: this locale was not fetched now; its state is unchanged
	Script    *SideStoryScript // fetched and parsed
	Missing   bool             // HTTP 404, or not published on that server
	Err       string           // any other failure
	Transient bool             // Err is worth retrying soon
}

type SideStoryEpisodeFetch struct {
	Kind, StoryID, EpisodeKey string
	JP, CN, EN                SideStoryFetchOutcome
}

type SideStoryEpisodeApply struct {
	Kind              string `json:"kind"`
	StoryID           string `json:"id"`
	EpisodeKey        string `json:"key"`
	Fetched           bool   `json:"fetched"`
	ScriptChanged     bool   `json:"scriptChanged"`
	CNState           string `json:"cnState"`
	ENState           string `json:"enState"`
	OfficialWritten   int    `json:"officialWritten"`
	DroppedHumanLines int    `json:"droppedHumanLines"`
	Error             string `json:"error,omitempty"`
}

type SideStoryApplyResult struct {
	Episodes []SideStoryEpisodeApply
	Changed  bool // any line or translation row changed
}

type SideStoryLineState struct {
	JP        string `json:"jp"`
	Role      string `json:"role"`
	Speaker   string `json:"speaker,omitempty"`
	Position  int    `json:"position"`
	Text      string `json:"text"`
	Source    string `json:"source"` // "" when no row
	Revision  int    `json:"revision"`
	UpdatedBy string `json:"updatedBy,omitempty"`
	UpdatedAt int64  `json:"updatedAt,omitempty"` // unix seconds
}

type SideStoryLineEdit struct {
	JP               string `json:"jp"`
	Text             string `json:"text"`
	Source           string `json:"source,omitempty"` // "" = human; allowed human | llm
	ExpectedRevision *int   `json:"expectedRevision,omitempty"`
}

type SideStoryLineConflict struct {
	JP               string `json:"jp"`
	ExpectedRevision int    `json:"expectedRevision"`
	CurrentRevision  int    `json:"currentRevision"`
	CurrentText      string `json:"currentText"`
	CurrentSource    string `json:"currentSource"`
}

var (
	ErrSideStoryRevisionConflict = errors.New("side story line revision conflict")
	ErrSideStoryUnknownLines     = errors.New("side story lines are unknown")
	ErrSideStoryInvalid          = errors.New("side story request is invalid")
)

type SideStoryRevisionConflictError struct{ Conflicts []SideStoryLineConflict }

func (e *SideStoryRevisionConflictError) Error() string {
	return fmt.Sprintf("%v: %d line(s)", ErrSideStoryRevisionConflict, len(e.Conflicts))
}

func (e *SideStoryRevisionConflictError) Is(target error) bool {
	return target == ErrSideStoryRevisionConflict
}

type SideStoryUnknownLinesError struct{ Lines []string }

func (e *SideStoryUnknownLinesError) Error() string {
	return fmt.Sprintf("%v: %d line(s)", ErrSideStoryUnknownLines, len(e.Lines))
}

func (e *SideStoryUnknownLinesError) Is(target error) bool {
	return target == ErrSideStoryUnknownLines
}

type SideStoryUpdateResult struct {
	Updated   int                  `json:"updated"`
	Unchanged int                  `json:"unchanged"`
	Lines     []SideStoryLineState `json:"lines"` // every edited line, current state
}

type SideStorySourceCounts struct {
	Official int `json:"official"`
	LLM      int `json:"llm"`
	Human    int `json:"human"`
}

type SideStorySummary struct {
	Kind                string                `json:"kind"`
	ID                  string                `json:"id"`
	Title               string                `json:"title"`
	CharacterID         int                   `json:"characterId"`
	AreaID              int                   `json:"areaId"`
	AreaCategory        string                `json:"areaCategory"`
	ActionSetID         int                   `json:"actionSetId"`
	ReleasedAt          int64                 `json:"releasedAt"` // unix ms
	EpisodeCount        int                   `json:"episodeCount"`
	FetchedEpisodeCount int                   `json:"fetchedEpisodeCount"`
	LineCount           int                   `json:"lineCount"`
	TranslatedCount     int                   `json:"translatedCount"`
	UntranslatedCount   int                   `json:"untranslatedCount"`
	SourceCounts        SideStorySourceCounts `json:"sourceCounts"`
	PrimarySource       string                `json:"primarySource"` // most frequent source among translated lines; ties human > official > llm; "" until a body or speaker line is translated
	Status              string                `json:"status"`        // pending (no episode fetched) | untranslated | partial | translated
	UpdatedAt           int64                 `json:"updatedAt"`     // unix seconds, latest translation write, else story update
}

type SideStoryEpisodeDetail struct {
	Key               string               `json:"key"`
	ScenarioID        string               `json:"scenarioId"`
	Title             string               `json:"title"` // JP
	Fetched           bool                 `json:"fetched"`
	ScriptSHA256      string               `json:"scriptSha256"`
	CNState           string               `json:"cnState"`
	ENState           string               `json:"enState"`
	LastError         string               `json:"lastError,omitempty"`
	Lines             []SideStoryLineState `json:"lines"` // ordered by position
	TranslatedCount   int                  `json:"translatedCount"`
	UntranslatedCount int                  `json:"untranslatedCount"`
}

type SideStoryDetail struct {
	Kind         string                   `json:"kind"`
	ID           string                   `json:"id"`
	Title        string                   `json:"title"`
	CharacterID  int                      `json:"characterId"`
	AreaID       int                      `json:"areaId"`
	AreaCategory string                   `json:"areaCategory"`
	ActionSetID  int                      `json:"actionSetId"`
	Locale       string                   `json:"locale"`
	Episodes     []SideStoryEpisodeDetail `json:"episodes"` // ordered by position
}

type SideStoryAITarget struct {
	EpisodeKey string `json:"episodeKey"`
	JP         string `json:"jp"`
	Revision   int    `json:"revision"`
}

type SideStoryKindProgress struct {
	Stories      int `json:"stories"`
	Episodes     int `json:"episodes"`
	Fetched      int `json:"fetched"`
	PendingFetch int `json:"pendingFetch"`
	CNImported   int `json:"cnImported"`
	CNPending    int `json:"cnPending"`
	CNAbsent     int `json:"cnAbsent"`
	CNMismatch   int `json:"cnMismatch"`
	CNError      int `json:"cnError"`
	ENImported   int `json:"enImported"`
	ENPending    int `json:"enPending"`
	ENAbsent     int `json:"enAbsent"`
	ENMismatch   int `json:"enMismatch"`
	ENError      int `json:"enError"`
	Errors       int `json:"errors"` // episodes with a JP fetch error recorded
}

// Public projection (ordered; the publish writer turns it into JSON).
type SideStoryPublicTalk struct{ JP, Text string }

type SideStoryPublicEpisode struct {
	Key, ScenarioID, Title, Source string // Title = translated title or ""; Source = public label
	Talk                           []SideStoryPublicTalk
}

type SideStoryPublicFile struct {
	Source      string // aggregate public label
	LastUpdated int64  // unix seconds, newest translation write in the file
	Episodes    []SideStoryPublicEpisode
}

// Shared value types used between the translator and the API.
type SideStoryRoundSummary struct {
	Episodes        int `json:"episodes"`
	Requests        int `json:"requests"`
	Fetched         int `json:"fetched"`
	OfficialWritten int `json:"officialWritten"`
	// Errors counts episodes whose JP fetch failed or whose CN/EN state is
	// error or mismatch; Retrying counts the other episodes that carry a
	// lastError, a locale left pending for a later retry (404, a mirror
	// serving Japanese, a transient failure).
	Errors   int `json:"errors"`
	Retrying int `json:"retrying"`
}

type SideStoryBackfillState struct {
	Enabled            bool                  `json:"enabled"`
	Running            bool                  `json:"running"`
	LastRoundAt        string                `json:"lastRoundAt,omitempty"` // RFC 3339
	NextRoundAt        string                `json:"nextRoundAt,omitempty"`
	CatalogRefreshedAt string                `json:"catalogRefreshedAt,omitempty"`
	LastRoundError     string                `json:"lastRoundError,omitempty"`
	LastRound          SideStoryRoundSummary `json:"lastRound"`
}

type SideStoryAIResult struct {
	Translated int `json:"translated"`
	Remaining  int `json:"remaining"`
}

func sideStoryInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSideStoryInvalid, fmt.Sprintf(format, args...))
}

// validSideStoryText accepts valid UTF-8 without NUL of at most 16384 bytes.
func validSideStoryText(text string) bool {
	return len(text) <= sideStoryMaxTextBytes && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

// sideStoryHasKana reports whether text contains hiragana or katakana
// (U+3040–U+30FF).
func sideStoryHasKana(text string) bool {
	for _, r := range text {
		if r >= 0x3040 && r <= 0x30FF {
			return true
		}
	}
	return false
}

func validSideStoryRequest(kind, storyID, locale string) error {
	if !ValidSideStoryKind(kind) {
		return sideStoryInvalid("kind %q", kind)
	}
	if !ValidSideStoryID(kind, storyID) {
		return sideStoryInvalid("%s id %q", kind, storyID)
	}
	if !ValidSideStoryLocale(locale) {
		return sideStoryInvalid("locale %q", locale)
	}
	return nil
}
