// Package lyricsproviderpolicy defines the closed, versioned safety contract for
// acquiring lyrics from the supported MediaWiki providers.
package lyricsproviderpolicy

const (
	// ContractVersionV1 is the only contract version accepted by this package.
	ContractVersionV1 = "lyrics-provider-policy/v1"

	// ResponseSizeCeilingBytesV1 is the compiled maximum response body size.
	ResponseSizeCeilingBytesV1 = 2 << 20
	// MinimumStartIntervalSecondsV1 is the hard provider-wide request-start floor.
	MinimumStartIntervalSecondsV1 = 10
	// DefaultMaxActualNetworkInFlightV1 applies without a reviewed permission profile.
	DefaultMaxActualNetworkInFlightV1 = 1
	// MediaWikiMaxlagV1 is the required maxlag query parameter value.
	MediaWikiMaxlagV1 = 5
	// DefaultActionBatchCeilingV1 applies to every supported action without permission.
	DefaultActionBatchCeilingV1 = 1
	// MaxAcquirerProcessesV1 is the maximum number of concurrent live owners.
	MaxAcquirerProcessesV1 = 1
	// FixedLiveStateRootV1 is the only live coordination root used by the recovery binary.
	FixedLiveStateRootV1 = "/private/tmp/moesekai-lyrics-live-acquisition-state-v1"
	// MinimumFallbackCooldownSecondsV1 is the minimum delay used without usable Retry-After.
	MinimumFallbackCooldownSecondsV1 = 60
	// MinimumBackoffMultiplierV1 is the conservative exponential-backoff floor.
	MinimumBackoffMultiplierV1 = 2
)

type Provider string

const (
	ProviderVocaloidFandom Provider = "vocaloid_fandom"
	ProviderMoegirl        Provider = "moegirl"
	ProviderSekaipedia     Provider = "sekaipedia"
)

type Action string

const (
	// ActionSearch represents one MediaWiki search expression, regardless of the
	// number of results returned by that expression.
	ActionSearch Action = "search"
	// ActionPageByID represents one page ID in one actual network request.
	ActionPageByID Action = "page_by_id"
	// ActionPageByTitle represents one title in one actual network request.
	ActionPageByTitle Action = "page_by_title"
	// ActionRevisionByID represents one revision ID in one actual network request.
	ActionRevisionByID Action = "revision_by_id"
)

const (
	RuleProhibited = "prohibited"

	ConcurrencyAccountingActualNetworkRequests = "actual_network_requests"
	CrossProcessCoordinationRetainedGlobalLock = "retained_global_live_acquisition_lock"
	UnusableLiveStateDispositionHold           = "hold"
	UnresolvedAdmissionDispositionHold         = "hold"

	CooldownScopeProviderWide            = "provider_wide"
	CooldownPersistencePersistent        = "persistent"
	RetryAfterRuleNeverShorten           = "at_least_header_never_shorten"
	MaxlagClassificationProviderOverload = "retryable_provider_overload"
	BackoffModeExponentialExtendOnly     = "exponential_extend_only"
)

// ProviderSpecV1 is compiled provider identity and action capability. Returned
// specs are copies and cannot alter validation behavior.
type ProviderSpecV1 struct {
	Provider Provider
	Origin   string
	APIPath  string
	Actions  []Action
}

var compiledProviderSpecsV1 = []ProviderSpecV1{
	{
		Provider: ProviderVocaloidFandom,
		Origin:   "https://vocaloid.fandom.com",
		APIPath:  "/api.php",
		Actions:  []Action{ActionSearch, ActionPageByID, ActionRevisionByID},
	},
	{
		Provider: ProviderMoegirl,
		Origin:   "https://moegirl.icu",
		APIPath:  "/api.php",
		Actions:  []Action{ActionPageByTitle, ActionRevisionByID},
	},
	{
		Provider: ProviderSekaipedia,
		Origin:   "https://www.sekaipedia.org",
		APIPath:  "/w/api.php",
		Actions:  []Action{ActionPageByTitle, ActionRevisionByID},
	},
}

// CompiledProviderSpecsV1 returns the complete provider enum, canonical HTTPS
// origins/API paths, and supported actions for v1.
func CompiledProviderSpecsV1() []ProviderSpecV1 {
	result := make([]ProviderSpecV1, len(compiledProviderSpecsV1))
	for index, spec := range compiledProviderSpecsV1 {
		result[index] = spec
		result[index].Actions = append([]Action(nil), spec.Actions...)
	}
	return result
}

// CanonicalEndpointV1 returns a provider's compiled HTTPS API endpoint.
func CanonicalEndpointV1(provider Provider) (string, bool) {
	spec, found := compiledProviderSpecV1(provider)
	if !found {
		return "", false
	}
	return spec.Origin + spec.APIPath, true
}
func compiledProviderSpecV1(provider Provider) (ProviderSpecV1, bool) {
	for _, spec := range compiledProviderSpecsV1 {
		if spec.Provider == provider {
			spec.Actions = append([]Action(nil), spec.Actions...)
			return spec, true
		}
	}
	return ProviderSpecV1{}, false
}

// PolicyV1 is a closed deployment contract. ApprovalRegistryV1 is deliberately
// separate so an untrusted policy document cannot approve its own exceptions.
type PolicyV1 struct {
	Version   string             `json:"version"`
	Identity  IdentityV1         `json:"identity"`
	Acquirer  AcquirerPolicyV1   `json:"acquirer"`
	Providers []ProviderPolicyV1 `json:"providers"`
}

// IdentityV1 is an explicit operator attestation. Validation can prove syntax,
// stability requirements, and User-Agent/contact consistency; deployment
// review remains responsible for confirming the asserted real-world identity.
type IdentityV1 struct {
	UserAgent string `json:"userAgent"`
	Contact   string `json:"contact"`
	Truthful  bool   `json:"truthful"`
	Stable    bool   `json:"stable"`
}

type AcquirerPolicyV1 struct {
	MaxProcesses                   int    `json:"maxProcesses"`
	CrossProcessCoordination       string `json:"crossProcessCoordination"`
	LiveStateRoot                  string `json:"liveStateRoot"`
	UnusableStateDisposition       string `json:"unusableStateDisposition"`
	UnresolvedAdmissionDisposition string `json:"unresolvedAdmissionDisposition"`
}

type ProviderPolicyV1 struct {
	Provider          Provider           `json:"provider"`
	Origin            string             `json:"origin"`
	APIPath           string             `json:"apiPath"`
	Transport         TransportPolicyV1  `json:"transport"`
	Response          ResponsePolicyV1   `json:"response"`
	Scheduling        SchedulingPolicyV1 `json:"scheduling"`
	MediaWiki         MediaWikiPolicyV1  `json:"mediaWiki"`
	Cooldown          CooldownPolicyV1   `json:"cooldown"`
	Actions           []ActionPolicyV1   `json:"actions"`
	PermissionProfile *ProfileRefV1      `json:"permissionProfile,omitempty"`
	CredentialProfile *ProfileRefV1      `json:"credentialProfile,omitempty"`
}

type TransportPolicyV1 struct {
	Proxy          string `json:"proxy"`
	IPRotation     string `json:"ipRotation"`
	HeaderRotation string `json:"headerRotation"`
}

type ResponsePolicyV1 struct {
	MaxBytes int `json:"maxBytes"`
}

type SchedulingPolicyV1 struct {
	MinimumStartIntervalSeconds int    `json:"minimumStartIntervalSeconds"`
	MaxActualNetworkInFlight    int    `json:"maxActualNetworkInFlight"`
	ConcurrencyAccounting       string `json:"concurrencyAccounting"`
	HiddenConcurrency           string `json:"hiddenConcurrency"`
}

type MediaWikiPolicyV1 struct {
	Maxlag int `json:"maxlag"`
}

// CooldownPolicyV1 requires one persistent provider-wide not-before value.
// Retry-After, maxlag, and fallback/backoff updates may only extend it.
type CooldownPolicyV1 struct {
	Scope                  string `json:"scope"`
	Persistence            string `json:"persistence"`
	SharedAcrossActions    bool   `json:"sharedAcrossActions"`
	RetryAfterStatuses     []int  `json:"retryAfterStatuses"`
	RetryAfterRule         string `json:"retryAfterRule"`
	MaxlagClassification   string `json:"maxlagClassification"`
	MaxlagUsesSameCooldown bool   `json:"maxlagUsesSameCooldown"`
	FallbackSeconds        int    `json:"fallbackSeconds"`
	BackoffMultiplier      int    `json:"backoffMultiplier"`
	BackoffMode            string `json:"backoffMode"`
}

type ActionPolicyV1 struct {
	Action       Action `json:"action"`
	MaxBatchSize int    `json:"maxBatchSize"`
}

// ProfileRefV1 is metadata only. It never contains credentials, tokens,
// passwords, cookies, authorization headers, or permission values.
type ProfileRefV1 struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// ApprovalRegistryV1 must be supplied by trusted reviewed configuration. This
// package intentionally ships no real permission or credential approvals.
type ApprovalRegistryV1 struct {
	PermissionProfiles []ApprovedPermissionProfileV1
	CredentialProfiles []ApprovedCredentialProfileV1
}

// ApprovalMetadataV1 fixes the review record for one versioned profile.
type ApprovalMetadataV1 struct {
	ReviewReference string
	ReviewedAt      string
}

// ApprovedPermissionProfileV1 may raise only exact per-action batch ceilings.
// Actual network concurrency remains fixed at one across all profiles.
type ApprovedPermissionProfileV1 struct {
	Ref                      ProfileRefV1
	Provider                 Provider
	MaxActualNetworkInFlight int
	ActionBatchCeilings      []ActionPolicyV1
	Approval                 ApprovalMetadataV1
}

// ApprovedCredentialProfileV1 records approved external credential metadata.
// Secret material and secret locators are intentionally not representable.
type ApprovedCredentialProfileV1 struct {
	Ref      ProfileRefV1
	Provider Provider
	Approval ApprovalMetadataV1
}

// NewBaselineV1 creates the no-exception contract. The caller must supply its
// own truthful stable identity; no contact or permission value is invented.
func NewBaselineV1(identity IdentityV1) (PolicyV1, error) {
	policy := PolicyV1{
		Version:  ContractVersionV1,
		Identity: identity,
		Acquirer: AcquirerPolicyV1{
			MaxProcesses:                   MaxAcquirerProcessesV1,
			CrossProcessCoordination:       CrossProcessCoordinationRetainedGlobalLock,
			LiveStateRoot:                  FixedLiveStateRootV1,
			UnusableStateDisposition:       UnusableLiveStateDispositionHold,
			UnresolvedAdmissionDisposition: UnresolvedAdmissionDispositionHold,
		},
		Providers: make([]ProviderPolicyV1, 0, len(compiledProviderSpecsV1)),
	}
	for _, spec := range CompiledProviderSpecsV1() {
		actions := make([]ActionPolicyV1, len(spec.Actions))
		for index, action := range spec.Actions {
			actions[index] = ActionPolicyV1{Action: action, MaxBatchSize: DefaultActionBatchCeilingV1}
		}
		policy.Providers = append(policy.Providers, ProviderPolicyV1{
			Provider: spec.Provider,
			Origin:   spec.Origin,
			APIPath:  spec.APIPath,
			Transport: TransportPolicyV1{
				Proxy: RuleProhibited, IPRotation: RuleProhibited, HeaderRotation: RuleProhibited,
			},
			Response: ResponsePolicyV1{MaxBytes: ResponseSizeCeilingBytesV1},
			Scheduling: SchedulingPolicyV1{
				MinimumStartIntervalSeconds: MinimumStartIntervalSecondsV1,
				MaxActualNetworkInFlight:    DefaultMaxActualNetworkInFlightV1,
				ConcurrencyAccounting:       ConcurrencyAccountingActualNetworkRequests,
				HiddenConcurrency:           RuleProhibited,
			},
			MediaWiki: MediaWikiPolicyV1{Maxlag: MediaWikiMaxlagV1},
			Cooldown: CooldownPolicyV1{
				Scope: CooldownScopeProviderWide, Persistence: CooldownPersistencePersistent,
				SharedAcrossActions: true, RetryAfterStatuses: []int{429, 503},
				RetryAfterRule:         RetryAfterRuleNeverShorten,
				MaxlagClassification:   MaxlagClassificationProviderOverload,
				MaxlagUsesSameCooldown: true,
				FallbackSeconds:        MinimumFallbackCooldownSecondsV1,
				BackoffMultiplier:      MinimumBackoffMultiplierV1,
				BackoffMode:            BackoffModeExponentialExtendOnly,
			},
			Actions: actions,
		})
	}
	if err := ValidateV1(policy); err != nil {
		return PolicyV1{}, err
	}
	return policy, nil
}
