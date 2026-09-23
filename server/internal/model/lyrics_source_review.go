package model

import "time"

type LyricsSourceArtifact struct {
	ArtifactID           int64
	SourceType           string
	SourceOrigin         string
	PageID               int
	RevisionID           int
	PageTitle            string
	CanonicalRevisionURL string
	MediaWikiSHA1        string
	Categories           []string
	RawWikitext          []byte
	RawWikitextSHA256    string
	ArtifactSHA256       string
	FirstFetchedAt       time.Time
	FirstCreatingJobID   int64
	CreatedAt            time.Time
}

type LyricsSourceAnalysis struct {
	AnalysisID               int64
	AnalysisKey              string
	ArtifactID               int64
	MusicID                  int
	CatalogFingerprint       string
	MatchingPolicyVersion    string
	RestrictionPolicyVersion string
	ExtractorVersion         string
	MatchOutcome             string
	RestrictionOutcome       string
	ExtractionOutcome        string
	MatchingEvidence         []LyricsSourceEvidence
	RestrictionRuleIDs       []string
	SelectedVersion          LyricsSourceVersion
	Performers               []LyricsSourcePerformer
	RubyGeneratorVersion     string
	ExtractedLines           []LyricsSourceExtractedLine
	ExtractedLinesSHA256     string
	AnalysisSHA256           string
	CreatingJobID            int64
	CreatedAt                time.Time
}

type LyricsSourceAssociation struct {
	MusicID            int
	CatalogFingerprint string
	Kind               string
}

type LyricsSourceCandidateIdentity struct {
	PageID       int      `json:"pageId"`
	RevisionID   int      `json:"revisionId"`
	SHA1         string   `json:"sha1"`
	Title        string   `json:"title"`
	CanonicalURL string   `json:"canonicalUrl"`
	Categories   []string `json:"categories"`
}

type LyricsSourceReviewItem struct {
	ReviewID            int64
	DomainKey           string
	Kind                string
	AnalysisID          int64
	MusicID             int
	CatalogFingerprint  string
	ReviewPolicyVersion string
	ReasonCode          string
	EvidenceJSON        []byte
	State               string
	IdentityGate        string
	SourceUseGate       string
	ParseGate           string
	Version             int64
	Priority            int
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         time.Time
}
