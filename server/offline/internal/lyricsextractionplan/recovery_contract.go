package lyricsextractionplan

import "fmt"

const (
	RecoverySchemaVersionV2     = 2
	RecoveryCanonicalEncodingV2 = "moesekai-lyrics-recovery-plan-ordered-json-v2"
	RecoveryDigestAlgorithmV2   = "sha256-moesekai-lyrics-recovery-plan-ordered-json-v2"

	RecoveryProviderPolicyVersionV1  = "lyrics-provider-policy/v1"
	RecoveryFallbackPolicyVersionV1  = "closed-provider-fallback-v1"
	RecoveryCompositionVersionV1     = "provider-component-gated-composition-v1"
	RecoveryCompositionVersionV2     = "provider-component-gated-rendition-composition-v2"
	RecoveryLineIDVersionV1          = "full-ordered-line-ids-v1"
	RecoveryLineIDVersionV2          = "rendition-ordered-line-ids-v2"
	RecoveryOutcomeArtifactVersionV1 = "lyrics-provider-outcome-artifact-v1"
	RecoverySongResultVersionV1      = "lyrics-recovery-song-result-v1"
	RecoverySongResultVersionV2      = "lyrics-recovery-song-result-v2"

	RecoveryScopeFinal   RecoveryScopeKind = "final"
	RecoveryScopePartial RecoveryScopeKind = "partial"
	RecoveryScopeRetry   RecoveryScopeKind = "retry"

	RecoveryOutputCreateExclusive RecoveryOutputPublication     = "create_exclusive"
	RecoveryOutputPrivate         RecoveryOutputConfidentiality = "private"

	MaxRecoveryAliases            = 64
	MaxRecoveryAliasBytes         = 256
	MaxRecoverySekaipediaTargets  = 10_000
	MaxRecoveryExactPublicTargets = 16
	MaxRecoveryPageTitleBytes     = 512
	MaxRecoveryScopeMusicIDs      = 10_000
	MaxRecoveryLiveCanarySongs    = 16
	MaxRecoveryOutputPathBytes    = 4096
	RecoveryMaxActualInFlight     = 1
	RecoveryRequiredMaxlag        = 5

	HistoricalSekaipediaListAcquisitionID = "5fe933acc356773e4fd82e7c5068c7b97a0ccf71b42cb0e8269f49657f2e5a96"
)

type RecoveryScopeKind string
type RecoveryOutputPublication string
type RecoveryOutputConfidentiality string

type RecoveryPlan struct {
	SchemaVersion     int                           `json:"schemaVersion"`
	CanonicalEncoding string                        `json:"canonicalEncoding"`
	DigestAlgorithm   string                        `json:"digestAlgorithm"`
	PlanID            string                        `json:"planId"`
	CreatedAt         string                        `json:"createdAt"`
	Catalog           RecoveryCatalogBinding        `json:"catalog"`
	SourceSnapshot    SourceSnapshot                `json:"sourceSnapshot"`
	Scope             RecoveryScopeBinding          `json:"scope"`
	Providers         RecoveryProviderConfiguration `json:"providers"`
	Versions          RecoveryVersions              `json:"versions"`
	Execution         RecoveryExecutionSettings     `json:"execution"`
	SekaipediaCanary  *RecoverySekaipediaCanaryPlan `json:"sekaipediaCanary,omitempty"`
	Outputs           RecoveryOutputs               `json:"outputs"`
	Deployment        DeploymentPolicy              `json:"deployment"`
}

type RecoveryCatalogBinding struct {
	Path                  string `json:"path"`
	SizeBytes             int64  `json:"sizeBytes"`
	SourceSHA256          string `json:"sourceSha256"`
	SchemaVersion         int    `json:"schemaVersion"`
	RuntimeSchemaVersion  int    `json:"runtimeSchemaVersion"`
	RecordCount           int    `json:"recordCount"`
	IdentityPolicyVersion string `json:"identityPolicyVersion"`
	IdentitySHA256        string `json:"identitySha256"`
	MusicIDsSHA256        string `json:"musicIdsSha256"`
}

type RecoveryScopeBinding struct {
	Kind                 RecoveryScopeKind `json:"kind"`
	ScopeID              string            `json:"scopeId"`
	MusicIDs             []int             `json:"musicIds"`
	SupersedesRootID     string            `json:"supersedesRootId"`
	SupersedesRootSHA256 string            `json:"supersedesRootSha256"`
}

type RecoveryProviderConfiguration struct {
	Order          []Provider             `json:"order"`
	Configurations []RecoveryProviderPlan `json:"configurations"`
}

type RecoveryProviderPlan struct {
	Provider           Provider                        `json:"provider"`
	Mode               ProviderMode                    `json:"mode"`
	CrawlDelayMillis   int64                           `json:"crawlDelayMillis"`
	CacheTTLMillis     int64                           `json:"cacheTtlMillis"`
	MusicIDs           []int                           `json:"musicIds,omitempty"`
	Authorities        []FixedAuthority                `json:"authorities"`
	ContributorAliases []RecoveryContributorAlias      `json:"contributorAliases"`
	SekaipediaTargets  []RecoverySekaipediaPageTarget  `json:"sekaipediaTargets,omitempty"`
	ExactPublicTargets []RecoveryExactPublicPageTarget `json:"exactPublicTargets,omitempty"`
}

type RecoverySekaipediaPageTarget struct {
	MusicID           int                                `json:"musicId"`
	PageTitle         string                             `json:"pageTitle"`
	ResolvedPageTitle string                             `json:"resolvedPageTitle,omitempty"`
	FixedRevision     *RecoverySekaipediaRevisionBinding `json:"fixedRevision,omitempty"`
}

// RecoverySekaipediaRevisionBinding pins a song target to one exact immutable
// MediaWiki response and semantic revision tuple. Historical targets omit it;
// newly reviewed fixed targets carry every field.
type RecoverySekaipediaRevisionBinding struct {
	PageID            int    `json:"pageId"`
	RevisionID        int    `json:"revisionId"`
	RevisionTimestamp string `json:"revisionTimestamp"`
	SHA1              string `json:"sha1"`
	ContentSHA256     string `json:"contentSha256"`
	RawResponseSHA256 string `json:"rawResponseSha256"`
}

type RecoveryExactPublicPageTarget struct {
	MusicID          int                 `json:"musicId"`
	PageURL          string              `json:"pageUrl"`
	PageTitle        string              `json:"pageTitle"`
	JapaneseTitle    string              `json:"japaneseTitle"`
	PageID           int                 `json:"pageId"`
	RevisionID       int                 `json:"revisionId"`
	FetchedAt        string              `json:"fetchedAt"`
	RawHTML          RecoveryFileBinding `json:"rawHtml"`
	ExtractionReport RecoveryFileBinding `json:"extractionReport"`
}

type RecoveryFileBinding struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type RecoveryContributorAlias struct {
	MusicID             int    `json:"musicId"`
	CatalogContributor  string `json:"catalogContributor"`
	ProviderContributor string `json:"providerContributor"`
}

type RecoveryParserVersion struct {
	Provider             Provider `json:"provider"`
	ParserVersion        string   `json:"parserVersion"`
	RubyGeneratorVersion string   `json:"rubyGeneratorVersion,omitempty"`
}

type RecoveryVersions struct {
	SourceSelection string                  `json:"sourceSelection"`
	SourceSnapshot  string                  `json:"sourceSnapshot"`
	ProviderPolicy  string                  `json:"providerPolicy"`
	FallbackPolicy  string                  `json:"fallbackPolicy"`
	Composition     string                  `json:"composition"`
	LineIDs         string                  `json:"lineIds"`
	OutcomeArtifact string                  `json:"outcomeArtifact"`
	SongResult      string                  `json:"songResult"`
	Parsers         []RecoveryParserVersion `json:"parsers"`
}

type RecoveryExecutionSettings struct {
	MaxAttempts              int   `json:"maxAttempts"`
	RequestTimeoutMillis     int64 `json:"requestTimeoutMillis"`
	RetryDelayMillis         int64 `json:"retryDelayMillis"`
	ProviderResponseBytes    int   `json:"providerResponseBytes"`
	MaxActualNetworkInFlight int   `json:"maxActualNetworkInFlight"`
	MediaWikiMaxlag          int   `json:"mediaWikiMaxlag"`
	LiveCanaryMusicIDs       []int `json:"liveCanaryMusicIds"`
}

type RecoverySekaipediaCanaryPlan struct {
	List  RecoverySekaipediaCanaryRevision `json:"list"`
	Songs []RecoverySekaipediaCanarySong   `json:"songs"`
}

// The two omitempty tags preserve the immutable byte shape of historical plans
// at the inspection-only decoder. ValidateRecovery and every new encoder still
// require both exact lowercase SHA-256 identities.
type RecoverySekaipediaCanaryRevision struct {
	AcquisitionID     string `json:"acquisitionId,omitempty"`
	PageID            int    `json:"pageId"`
	RevisionID        int    `json:"revisionId"`
	RevisionTimestamp string `json:"revisionTimestamp"`
	SHA1              string `json:"sha1"`
	ContentSHA256     string `json:"contentSha256,omitempty"`
	RawResponseSHA256 string `json:"rawResponseSha256"`
}

type RecoverySekaipediaCanarySong struct {
	MusicID           int    `json:"musicId"`
	CatalogTitle      string `json:"catalogTitle"`
	ProviderTitle     string `json:"providerTitle"`
	PageID            int    `json:"pageId"`
	RevisionID        int    `json:"revisionId"`
	RevisionTimestamp string `json:"revisionTimestamp"`
	SHA1              string `json:"sha1"`
	ContentSHA256     string `json:"contentSha256"`
	RawResponseSHA256 string `json:"rawResponseSha256"`
}

type RecoveryOutputs struct {
	Ledger           string                        `json:"ledger"`
	AcquisitionSet   string                        `json:"acquisitionSet"`
	ProviderOutcomes string                        `json:"providerOutcomes"`
	SongResults      string                        `json:"songResults"`
	EvidencePack     string                        `json:"evidencePack"`
	RootManifest     string                        `json:"rootManifest"`
	Publication      RecoveryOutputPublication     `json:"publication"`
	Confidentiality  RecoveryOutputConfidentiality `json:"confidentiality"`
	FileMode         uint32                        `json:"fileMode"`
	DirectoryMode    uint32                        `json:"directoryMode"`
}

func CompiledRecoveryVersions() RecoveryVersions {
	return RecoveryVersions{
		SourceSelection: RecoverySourceSelectionPolicyV3,
		SourceSnapshot:  RecoverySourceSnapshotAlgorithmV2,
		ProviderPolicy:  RecoveryProviderPolicyVersionV1,
		FallbackPolicy:  RecoveryFallbackPolicyVersionV1,
		Composition:     RecoveryCompositionVersionV2,
		LineIDs:         RecoveryLineIDVersionV2,
		OutcomeArtifact: RecoveryOutcomeArtifactVersionV1,
		SongResult:      RecoverySongResultVersionV2,
		Parsers: []RecoveryParserVersion{
			{Provider: ProviderSekaipedia, ParserVersion: registeredSekaipediaParser, RubyGeneratorVersion: registeredSekaipediaRuby},
			{Provider: ProviderMoegirl, ParserVersion: registeredMoegirlParser, RubyGeneratorVersion: registeredStructuredRuby},
			{Provider: ProviderVocaloidFandom, ParserVersion: registeredFandomParser, RubyGeneratorVersion: registeredStructuredRuby},
		},
	}
}

// CompiledScopedRecoveryVersions derives parser bindings only for the selected
// scoped provider order. Historical unscoped plans continue using the exact
// three-parser CompiledRecoveryVersions byte shape.
func CompiledScopedRecoveryVersions(order []Provider) (RecoveryVersions, error) {
	versions := CompiledRecoveryVersions()
	versions.Parsers = make([]RecoveryParserVersion, len(order))
	for index, provider := range order {
		parserVersion, ok := RegisteredRecoveryParserVersion(provider)
		if !ok {
			return RecoveryVersions{}, fmt.Errorf("recovery provider %q has no registered parser", provider)
		}
		rubyVersion, ok := RegisteredRecoveryRubyGeneratorVersion(provider)
		if !ok {
			return RecoveryVersions{}, fmt.Errorf("recovery provider %q has no registered ruby generator", provider)
		}
		versions.Parsers[index] = RecoveryParserVersion{
			Provider: provider, ParserVersion: parserVersion, RubyGeneratorVersion: rubyVersion,
		}
	}
	return versions, nil
}

func RegisteredRecoveryParserVersion(provider Provider) (string, bool) {
	switch provider {
	case ProviderSekaipedia:
		return registeredSekaipediaParser, true
	case ProviderMoegirl:
		return registeredMoegirlParser, true
	case ProviderMoegirlPublicExact:
		return registeredMoegirlPublicParser, true
	case ProviderVocaloidFandom:
		return registeredFandomParser, true
	default:
		return "", false
	}
}

func RegisteredRecoveryRubyGeneratorVersion(provider Provider) (string, bool) {
	switch provider {
	case ProviderSekaipedia:
		return registeredSekaipediaRuby, true
	case ProviderMoegirl, ProviderMoegirlPublicExact, ProviderVocaloidFandom:
		return registeredStructuredRuby, true
	default:
		return "", false
	}
}

func historicalRecoveryVersionsV1(order []Provider) (RecoveryVersions, error) {
	versions := CompiledRecoveryVersions()
	if order != nil {
		versions.Parsers = make([]RecoveryParserVersion, len(order))
	}
	providers := []Provider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom}
	if order != nil {
		providers = order
	}
	for index, provider := range providers {
		parserVersion, ok := RegisteredRecoveryParserVersion(provider)
		if !ok {
			return RecoveryVersions{}, fmt.Errorf("recovery provider %q has no historical parser", provider)
		}
		rubyVersion := historicalRegisteredStructuredRuby
		if provider == ProviderSekaipedia {
			rubyVersion = historicalRegisteredSekaipediaRuby
		}
		versions.Parsers[index] = RecoveryParserVersion{
			Provider: provider, ParserVersion: parserVersion, RubyGeneratorVersion: rubyVersion,
		}
	}
	return versions, nil
}

func RequiredRecoveryOutputs(paths [6]string) RecoveryOutputs {
	return RecoveryOutputs{
		Ledger: paths[0], AcquisitionSet: paths[1], ProviderOutcomes: paths[2],
		SongResults: paths[3], EvidencePack: paths[4], RootManifest: paths[5],
		Publication: RecoveryOutputCreateExclusive, Confidentiality: RecoveryOutputPrivate,
		FileMode: PrivateOutputFileMode, DirectoryMode: 0o700,
	}
}
