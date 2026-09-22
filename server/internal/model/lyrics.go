package model

const (
	PublicLyricsMaxArtifactBytes = 4 << 20
	PublicLyricsMaxIndexEntries  = 100_000
	PublicLyricsMaxTitleBytes    = 64 << 10
)

type LocalizedTitle struct {
	Japanese string `json:"ja-JP"`
	Chinese  string `json:"zh-CN,omitempty"`
	English  string `json:"en-US,omitempty"`
}

type RuntimeLyricsMetadata struct {
	ReleaseID         string   `json:"releaseId"`
	ImmutableOverlay  bool     `json:"immutableOverlay"`
	Source            string   `json:"source,omitempty"`
	State             string   `json:"state"`
	HasDetail         bool     `json:"hasDetail"`
	AvailableVersions []string `json:"availableVersions"`
	Revision          int      `json:"revision"`
	UpdatedAt         string   `json:"updatedAt"`
	BatchSHA256       string   `json:"batchSha256,omitempty"`
	RootSHA256        string   `json:"rootSha256,omitempty"`
}

type CatalogMusicItem struct {
	MusicID                 int                    `json:"musicId"`
	Title                   LocalizedTitle         `json:"title"`
	JacketURL               string                 `json:"jacketUrl,omitempty"`
	IsNewlyWrittenMusic     bool                   `json:"isNewlyWrittenMusic"`
	LyricsStatus            string                 `json:"lyricsStatus,omitempty"`
	LyricsAvailabilityState string                 `json:"lyricsAvailabilityState,omitempty"`
	RuntimeLyrics           *RuntimeLyricsMetadata `json:"runtimeLyrics,omitempty"`
}

type CatalogMusicResponse struct {
	Items      []CatalogMusicItem `json:"items"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

type CatalogPerformerItem struct {
	PerformerID int            `json:"performerId"`
	Name        LocalizedTitle `json:"name"`
}

type CatalogPerformerResponse struct {
	Items []CatalogPerformerItem `json:"items"`
}

// LyricRubySpan is an editable furigana span. Concatenating Text across a
// segment's Ruby array must reproduce the segment text exactly; Reading may be
// empty for punctuation, kana, Latin text, or a deliberately unannotated span.
type LyricRubySpan struct {
	Text    string `json:"text"`
	Reading string `json:"reading,omitempty"`
}

type LyricSegment struct {
	Text         string          `json:"text"`
	PerformerIDs []int           `json:"performerIds"`
	Ruby         []LyricRubySpan `json:"ruby,omitempty"`
}

type LyricLine struct {
	ID                string         `json:"id"`
	Order             int            `json:"order"`
	Japanese          string         `json:"japanese"`
	Chinese           string         `json:"zh-CN"`
	English           string         `json:"en-US"`
	StanzaBreakBefore bool           `json:"stanzaBreakBefore,omitempty"`
	Segments          []LyricSegment `json:"segments"`
}

type SongLyrics struct {
	MusicID            int         `json:"musicId"`
	Status             string      `json:"status"`
	PublishedRevision  int         `json:"publishedRevision,omitempty"`
	Revision           int         `json:"revision"`
	UpdatedAt          string      `json:"updatedAt"`
	Attribution        string      `json:"attribution"`
	TranslationCredit  string      `json:"translationCredit"`
	ProofreadingCredit string      `json:"proofreadingCredit"`
	SourceNote         string      `json:"sourceNote,omitempty"`
	SourceURL          string      `json:"sourceUrl,omitempty"`
	LicenseNote        string      `json:"licenseNote,omitempty"`
	SourcePageID       int         `json:"sourcePageId,omitempty"`
	SourceRevisionID   int         `json:"sourceRevisionId,omitempty"`
	SourceSHA1         string      `json:"sourceSha1,omitempty"`
	SourceFetchedAt    string      `json:"sourceFetchedAt,omitempty"`
	Lines              []LyricLine `json:"lines"`
}

type LyricsListItem struct {
	MusicID           int    `json:"musicId"`
	Status            string `json:"status"`
	Revision          int    `json:"revision"`
	PublishedRevision int    `json:"publishedRevision,omitempty"`
	UpdatedAt         string `json:"updatedAt"`
}

type LyricsListResponse struct {
	Items      []LyricsListItem `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type PublicLyricsIndex struct {
	Version int                     `json:"version"`
	Songs   []PublicLyricsIndexItem `json:"songs"`
}

type PublicLyricsIndexItem struct {
	MusicID   int            `json:"musicId"`
	Revision  int            `json:"revision"`
	UpdatedAt string         `json:"updatedAt"`
	Title     LocalizedTitle `json:"title"`
}

type PublicSongLyrics struct {
	Version          int         `json:"version"`
	MusicID          int         `json:"musicId"`
	Revision         int         `json:"revision"`
	UpdatedAt        string      `json:"updatedAt"`
	Attribution      string      `json:"attribution"`
	SourceURL        string      `json:"sourceUrl,omitempty"`
	SourcePageID     int         `json:"sourcePageId,omitempty"`
	SourceRevisionID int         `json:"sourceRevisionId,omitempty"`
	SourceSHA1       string      `json:"sourceSha1,omitempty"`
	SourceFetchedAt  string      `json:"sourceFetchedAt,omitempty"`
	LicenseName      string      `json:"licenseName,omitempty"`
	LicenseURL       string      `json:"licenseUrl,omitempty"`
	Lines            []LyricLine `json:"lines"`
}
