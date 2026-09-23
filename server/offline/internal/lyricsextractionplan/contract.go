// Package lyricsextractionplan defines the closed, data-only extraction-plan v1
// boundary. It deliberately contains no extraction or network execution hooks.
package lyricsextractionplan

import (
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

const (
	SchemaVersionV1     = 1
	CanonicalEncodingV1 = "moesekai-extraction-plan-ordered-json-v1"
	PlanDigestAlgorithm = "sha256-moesekai-extraction-plan-ordered-json-v1"
	SnapshotAlgorithmV1 = "sha256-ordered-source-file-identities-v1"

	MaxPlanBytes                    = 1 << 20
	MaxPlanJSONDepth                = 32
	MaxInputs                       = 2
	MaxSourceSnapshotFiles          = 4096
	MaxSourceFileBytes              = 16 << 20
	MaxSourceSnapshotBytes          = 128 << 20
	MaxCatalogDatabaseBytes   int64 = 4 << 30
	MaxResumeCheckpointBytes  int64 = 4 << 30
	MaxIdentityBytes                = 128
	MaxEvidenceIDBytes              = 256
	MaxAuthorityTitleBytes          = 2048
	MaxAuthorityURLBytes            = 4096
	MaxPathBytes                    = 4096
	MaxAuthoritiesPerProvider       = 16
	MaxAuthorities                  = 32
	MaxMediaWikiIdentity            = 2_147_483_647

	// These plan-v1 wire values are intentionally duplicated from the legacy
	// staging boundary and guarded by contract tests. Keeping the data-only plan
	// package independent prevents recovery-v2 from linking the legacy private
	// evidence-receipt implementation.
	CatalogSchemaVersion              = 18
	MaximumCatalogRuntimeSchema       = 23
	PreflightSchemaVersion            = 1
	EvidenceReceiptSchemaVersion      = 1
	StagingManifestSchemaVersion      = 3
	LyricsSourceDocumentSchemaVersion = model.LyricsSourceDocumentSchemaVersion

	ProviderSekaipedia         Provider = Provider(model.LyricsSourceProviderSekaipedia)
	ProviderMoegirl            Provider = Provider(model.LyricsSourceProviderMoegirl)
	ProviderMoegirlPublicExact Provider = Provider(model.LyricsSourceProviderMoegirlPublicExact)
	ProviderVocaloidFandom     Provider = Provider(model.LyricsSourceProviderVocaloidFandom)

	OriginSekaipedia         = model.LyricsSourceOriginSekaipedia
	OriginMoegirl            = model.LyricsSourceOriginMoegirl
	OriginMoegirlPublicExact = model.LyricsSourceOriginMoegirlPublicExact
	OriginVocaloidFandom     = model.LyricsSourceOriginVocaloidFandom

	ProviderModeActive ProviderMode = "active"

	AuthorityActive   AuthorityDisposition = "active"
	AuthorityRetained AuthorityDisposition = "retained"

	AuthorityRoleSongIndex AuthorityRole = "song_index"

	// CaptureProfileMediaWikiAPIRevisionResponseV1 fixes an authority to the
	// exact API response envelope, including its revision timestamp and raw hash.
	CaptureProfileMediaWikiAPIRevisionResponseV1 AuthorityCaptureProfile = "mediawiki_api_revision_response_v1"
	// CaptureProfileMediaWikiRevisionContentV1 fixes an authority to canonical
	// revision content identified by MediaWiki SHA-1 without an API envelope hash.
	CaptureProfileMediaWikiRevisionContentV1 AuthorityCaptureProfile = "mediawiki_revision_content_v1"

	InputCatalogDatabase  InputKind = "catalog_database"
	InputResumeReport     InputKind = "resume_report"
	InputResumeCheckpoint InputKind = "resume_checkpoint"

	ResumeFresh      ResumeMode = "fresh"
	ResumeReport     ResumeMode = "report"
	ResumeCheckpoint ResumeMode = "checkpoint"

	OutputPreflightReport OutputKind = "preflight_report"
	OutputStagingManifest OutputKind = "staging_manifest"
	OutputEvidenceReceipt OutputKind = "evidence_receipt"

	OutputPublicationCreateExclusive OutputPublication     = "create_exclusive"
	OutputConfidentialityPrivate     OutputConfidentiality = "private"
	PrivateOutputFileMode                                  = 0o600

	DeploymentStateHold DeploymentState = "HOLD"

	HoldSourceSnapshot       HoldCode = "source_snapshot"
	HoldCatalogInputs        HoldCode = "catalog_inputs"
	HoldExtractionExecution  HoldCode = "extraction_execution"
	HoldProductionDeployment HoldCode = "production_deployment"
)

const (
	registeredPerformerSegmentationPolicy = "catalog-vocals-sekai-eligibility-v1"
	registeredResumePolicy                = "safe-fixed-revision-resume-v1"

	registeredSekaipediaParser         = "sekaipedia-list-song-parser-v1"
	registeredMoegirlParser            = "moegirl-fixed-index-section-parser-v1"
	registeredMoegirlPublicParser      = "moegirl-exact-public-html-parser-v1"
	registeredFandomParser             = "fandom-category-aware-structured-parser-v2"
	historicalRegisteredSekaipediaRuby = "sekaipedia-ruby-kana-v1"
	historicalRegisteredStructuredRuby = "kagome-ipadic-v1"
	registeredSekaipediaRuby           = "sekaipedia-ruby-kana-v2"
	registeredStructuredRuby           = "kagome-ipadic-han-kana-v2"

	registeredProviderSelection = "sekaipedia-moegirl-vocaloid-fandom-v1"
	registeredVersionResolution = "closed-six-reason-version-matrix-v1"
	registeredProjection        = "unique-exact-subsequence-v1"
	registeredComposition       = "provider-component-gated-composition-v1"
)

type Provider string
type ProviderMode string
type AuthorityDisposition string
type AuthorityRole string
type AuthorityCaptureProfile string
type InputKind string
type ResumeMode string
type OutputKind string
type OutputPublication string
type OutputConfidentiality string
type DeploymentState string
type HoldCode string

type Plan struct {
	SchemaVersion     int                   `json:"schemaVersion"`
	CanonicalEncoding string                `json:"canonicalEncoding"`
	DigestAlgorithm   string                `json:"digestAlgorithm"`
	PlanID            string                `json:"planId"`
	CreatedAt         string                `json:"createdAt"`
	Catalog           CatalogIdentity       `json:"catalog"`
	Inputs            []InputIdentity       `json:"inputs"`
	SourceSnapshot    SourceSnapshot        `json:"sourceSnapshot"`
	Providers         ProviderConfiguration `json:"providers"`
	EffectiveVersions EffectiveVersions     `json:"effectiveVersions"`
	Execution         ExecutionSettings     `json:"execution"`
	Resume            ResumePolicy          `json:"resume"`
	Outputs           []OutputIdentity      `json:"outputs"`
	Deployment        DeploymentPolicy      `json:"deployment"`
}

type CatalogIdentity struct {
	InputID               string `json:"inputId"`
	SchemaVersion         int    `json:"schemaVersion"`
	RuntimeSchemaVersion  int    `json:"runtimeSchemaVersion"`
	RecordCount           int    `json:"recordCount"`
	IdentityPolicyVersion string `json:"identityPolicyVersion"`
}

type InputIdentity struct {
	ID        string    `json:"id"`
	Kind      InputKind `json:"kind"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"sizeBytes"`
	SHA256    string    `json:"sha256"`
}

type SourceSnapshot struct {
	Algorithm  string               `json:"algorithm"`
	CapturedAt string               `json:"capturedAt"`
	Files      []SourceFileIdentity `json:"files"`
	SHA256     string               `json:"sha256"`
}

type SourceFileIdentity struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type ProviderConfiguration struct {
	Order          []Provider     `json:"order"`
	Configurations []ProviderPlan `json:"configurations"`
}

type ProviderPlan struct {
	Provider         Provider         `json:"provider"`
	Origin           string           `json:"origin"`
	Mode             ProviderMode     `json:"mode"`
	CrawlDelayMillis int64            `json:"crawlDelayMillis"`
	CacheTTLMillis   int64            `json:"cacheTtlMillis"`
	Authorities      []FixedAuthority `json:"authorities"`
}

type FixedAuthority struct {
	EvidenceID        string                  `json:"evidenceId"`
	Disposition       AuthorityDisposition    `json:"disposition"`
	Role              AuthorityRole           `json:"role"`
	CaptureProfile    AuthorityCaptureProfile `json:"captureProfile"`
	PageID            int                     `json:"pageId"`
	RevisionID        int                     `json:"revisionId"`
	RevisionTimestamp string                  `json:"revisionTimestamp"`
	SHA1              string                  `json:"sha1"`
	ContentSHA256     string                  `json:"contentSha256,omitempty"`
	RawSHA256         string                  `json:"rawSha256"`
	Title             string                  `json:"title"`
	CanonicalURL      string                  `json:"canonicalUrl"`
}

type EffectiveVersions struct {
	Schemas    SchemaVersions    `json:"schemas"`
	Policies   PolicyVersions    `json:"policies"`
	Parsers    []ParserVersion   `json:"parsers"`
	Algorithms AlgorithmVersions `json:"algorithms"`
}

type SchemaVersions struct {
	ExtractionPlan        int `json:"extractionPlan"`
	Catalog               int `json:"catalog"`
	MaximumCatalogRuntime int `json:"maximumCatalogRuntime"`
	PreflightReport       int `json:"preflightReport"`
	EvidenceReceipt       int `json:"evidenceReceipt"`
	StagingManifest       int `json:"stagingManifest"`
	LyricsSourceDocument  int `json:"lyricsSourceDocument"`
}

type PolicyVersions struct {
	CatalogIdentity       string `json:"catalogIdentity"`
	Matching              string `json:"matching"`
	Restriction           string `json:"restriction"`
	Extractor             string `json:"extractor"`
	Review                string `json:"review"`
	PerformerSegmentation string `json:"performerSegmentation"`
	Resume                string `json:"resume"`
}

type ParserVersion struct {
	Provider             Provider `json:"provider"`
	ParserVersion        string   `json:"parserVersion"`
	RubyGeneratorVersion string   `json:"rubyGeneratorVersion"`
}

type AlgorithmVersions struct {
	ProviderSelection string `json:"providerSelection"`
	VersionResolution string `json:"versionResolution"`
	Projection        string `json:"projection"`
	Composition       string `json:"composition"`
	SnapshotDigest    string `json:"snapshotDigest"`
	PlanDigest        string `json:"planDigest"`
}

type ExecutionSettings struct {
	Concurrency          int          `json:"concurrency"`
	MaxAttempts          int          `json:"maxAttempts"`
	RequestTimeoutMillis int64        `json:"requestTimeoutMillis"`
	RetryDelayMillis     int64        `json:"retryDelayMillis"`
	Ceilings             HardCeilings `json:"ceilings"`
	SafetyFloors         SafetyFloors `json:"safetyFloors"`
}

type HardCeilings struct {
	Concurrency               int   `json:"concurrency"`
	Attempts                  int   `json:"attempts"`
	RequestTimeoutMillis      int64 `json:"requestTimeoutMillis"`
	RetryDelayMillis          int64 `json:"retryDelayMillis"`
	ProviderCrawlDelayMillis  int64 `json:"providerCrawlDelayMillis"`
	ProviderCacheTTLMillis    int64 `json:"providerCacheTtlMillis"`
	CatalogRecords            int   `json:"catalogRecords"`
	CatalogJSONBytes          int   `json:"catalogJsonBytes"`
	ProviderResponseBytes     int   `json:"providerResponseBytes"`
	SearchPages               int   `json:"searchPages"`
	ReportCandidates          int   `json:"reportCandidates"`
	CandidateTitleBytes       int   `json:"candidateTitleBytes"`
	CandidateURLBytes         int   `json:"candidateUrlBytes"`
	CandidateCategoryBytes    int   `json:"candidateCategoryBytes"`
	ExtractedLines            int   `json:"extractedLines"`
	ExtractedLineBytes        int   `json:"extractedLineBytes"`
	ExtractedTextBytes        int   `json:"extractedTextBytes"`
	IndexEvidenceRawBytes     int   `json:"indexEvidenceRawBytes"`
	FixedArtifacts            int   `json:"fixedArtifacts"`
	PreflightReportBytes      int   `json:"preflightReportBytes"`
	EvidenceReceiptBytes      int   `json:"evidenceReceiptBytes"`
	EvidenceReceiptRawBytes   int   `json:"evidenceReceiptRawBytes"`
	LyricsSourceDocumentBytes int   `json:"lyricsSourceDocumentBytes"`
	LyricsSourceJSONDepth     int   `json:"lyricsSourceJsonDepth"`
}

type SafetyFloors struct {
	RequestTimeoutMillis     int64 `json:"requestTimeoutMillis"`
	RetryDelayMillis         int64 `json:"retryDelayMillis"`
	ProviderCrawlDelayMillis int64 `json:"providerCrawlDelayMillis"`
	ProviderCacheTTLMillis   int64 `json:"providerCacheTtlMillis"`
}

type ResumePolicy struct {
	Mode                     ResumeMode `json:"mode"`
	InputID                  string     `json:"inputId"`
	RetryErrorCodes          []string   `json:"retryErrorCodes"`
	RetryMissingReasons      []string   `json:"retryMissingReasons"`
	RetryIncompleteCodes     []string   `json:"retryIncompleteCodes"`
	RevalidateUniqueComplete bool       `json:"revalidateUniqueComplete"`
}

type OutputIdentity struct {
	Kind            OutputKind            `json:"kind"`
	Path            string                `json:"path"`
	Publication     OutputPublication     `json:"publication"`
	Confidentiality OutputConfidentiality `json:"confidentiality"`
	FileMode        uint32                `json:"fileMode"`
}

type DeploymentPolicy struct {
	State DeploymentState `json:"state"`
	Holds []HoldCode      `json:"holds"`
}

// CompiledEffectiveVersions returns the only policy, parser, ruby, and
// algorithm implementations registered by this binary. Adding a selectable
// version requires code to implement and register it here first.
func CompiledEffectiveVersions() EffectiveVersions {
	return EffectiveVersions{
		Schemas: SchemaVersions{
			ExtractionPlan: SchemaVersionV1, Catalog: CatalogSchemaVersion,
			MaximumCatalogRuntime: MaximumCatalogRuntimeSchema,
			PreflightReport:       PreflightSchemaVersion, EvidenceReceipt: EvidenceReceiptSchemaVersion,
			StagingManifest:      StagingManifestSchemaVersion,
			LyricsSourceDocument: LyricsSourceDocumentSchemaVersion,
		},
		Policies: PolicyVersions{
			CatalogIdentity:       model.LyricsCatalogIdentityPolicyVersion,
			Matching:              model.LyricsMatchingPolicyVersion,
			Restriction:           model.LyricsRestrictionPolicyVersion,
			Extractor:             model.LyricsExtractorVersion,
			Review:                model.LyricsReviewPolicyVersion,
			PerformerSegmentation: registeredPerformerSegmentationPolicy,
			Resume:                registeredResumePolicy,
		},
		Parsers: []ParserVersion{
			{Provider: ProviderSekaipedia, ParserVersion: registeredSekaipediaParser, RubyGeneratorVersion: registeredSekaipediaRuby},
			{Provider: ProviderMoegirl, ParserVersion: registeredMoegirlParser, RubyGeneratorVersion: registeredStructuredRuby},
			{Provider: ProviderVocaloidFandom, ParserVersion: registeredFandomParser, RubyGeneratorVersion: registeredStructuredRuby},
		},
		Algorithms: AlgorithmVersions{
			ProviderSelection: registeredProviderSelection,
			VersionResolution: registeredVersionResolution,
			Projection:        registeredProjection,
			Composition:       registeredComposition,
			SnapshotDigest:    SnapshotAlgorithmV1,
			PlanDigest:        PlanDigestAlgorithm,
		},
	}
}

func historicalEffectiveVersionsV1() EffectiveVersions {
	versions := CompiledEffectiveVersions()
	for index := range versions.Parsers {
		switch versions.Parsers[index].Provider {
		case ProviderSekaipedia:
			versions.Parsers[index].RubyGeneratorVersion = historicalRegisteredSekaipediaRuby
		case ProviderMoegirl, ProviderVocaloidFandom:
			versions.Parsers[index].RubyGeneratorVersion = historicalRegisteredStructuredRuby
		}
	}
	return versions
}

func CompiledHardCeilings() HardCeilings {
	return HardCeilings{
		Concurrency: 16, Attempts: 5, RequestTimeoutMillis: 10 * 60 * 1000,
		RetryDelayMillis: 30 * 1000, ProviderCrawlDelayMillis: 10 * 60 * 1000,
		ProviderCacheTTLMillis: 24 * 60 * 60 * 1000,
		CatalogRecords:         100_000, CatalogJSONBytes: 1 << 20,
		ProviderResponseBytes: lyricsproviderpolicy.ResponseSizeCeilingBytesV1,
		SearchPages:           32, ReportCandidates: 16, CandidateTitleBytes: MaxAuthorityTitleBytes,
		CandidateURLBytes: MaxAuthorityURLBytes, CandidateCategoryBytes: 1024,
		ExtractedLines: 1000, ExtractedLineBytes: 8 << 10, ExtractedTextBytes: 1 << 20,
		IndexEvidenceRawBytes:     lyricsproviderpolicy.ResponseSizeCeilingBytesV1,
		FixedArtifacts:            16,
		PreflightReportBytes:      96 << 20,
		EvidenceReceiptBytes:      64 << 20,
		EvidenceReceiptRawBytes:   32 << 20,
		LyricsSourceDocumentBytes: model.MaxLyricsSourceDocumentBytes,
		LyricsSourceJSONDepth:     model.MaxLyricsSourceJSONDepth,
	}
}

func CompiledSafetyFloors() SafetyFloors {
	return SafetyFloors{
		RequestTimeoutMillis: 1, RetryDelayMillis: 0,
		ProviderCrawlDelayMillis: int64(lyricsproviderpolicy.MinimumStartIntervalSecondsV1) * 1000,
		ProviderCacheTTLMillis:   10 * 1000,
	}
}

// CompiledProviderConfiguration returns only the compiled provider union,
// canonical trust roots, active mode, safety-floor scheduling, and explicit
// empty authority arrays. A valid plan must supply its reviewed authority data.
func CompiledProviderConfiguration() ProviderConfiguration {
	floors := CompiledSafetyFloors()
	return ProviderConfiguration{
		Order: []Provider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom},
		Configurations: []ProviderPlan{
			{
				Provider: ProviderSekaipedia, Origin: OriginSekaipedia, Mode: ProviderModeActive,
				CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
				Authorities: []FixedAuthority{},
			},
			{
				Provider: ProviderMoegirl, Origin: OriginMoegirl, Mode: ProviderModeActive,
				CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
				Authorities: []FixedAuthority{},
			},
			{
				Provider: ProviderVocaloidFandom, Origin: OriginVocaloidFandom, Mode: ProviderModeActive,
				CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
				Authorities: []FixedAuthority{},
			},
		},
	}
}

func RequiredOutputs(paths [3]string) []OutputIdentity {
	return []OutputIdentity{
		{Kind: OutputPreflightReport, Path: paths[0], Publication: OutputPublicationCreateExclusive, Confidentiality: OutputConfidentialityPrivate, FileMode: PrivateOutputFileMode},
		{Kind: OutputStagingManifest, Path: paths[1], Publication: OutputPublicationCreateExclusive, Confidentiality: OutputConfidentialityPrivate, FileMode: PrivateOutputFileMode},
		{Kind: OutputEvidenceReceipt, Path: paths[2], Publication: OutputPublicationCreateExclusive, Confidentiality: OutputConfidentialityPrivate, FileMode: PrivateOutputFileMode},
	}
}

func RequiredDeploymentPolicy() DeploymentPolicy {
	return DeploymentPolicy{
		State: DeploymentStateHold,
		Holds: []HoldCode{HoldSourceSnapshot, HoldCatalogInputs, HoldExtractionExecution, HoldProductionDeployment},
	}
}
