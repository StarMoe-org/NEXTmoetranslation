package lyricsproviderpolicy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func testIdentityV1() IdentityV1 {
	const contact = "mailto:lyrics-policy-tests@unit.test"
	return IdentityV1{
		UserAgent: "LyricsPolicyTests/1.0 (" + contact + ")",
		Contact:   contact,
		Truthful:  true,
		Stable:    true,
	}
}

func mustBaselineV1(t *testing.T) PolicyV1 {
	t.Helper()
	policy, err := NewBaselineV1(testIdentityV1())
	if err != nil {
		t.Fatalf("new baseline: %v", err)
	}
	return policy
}

func providerPolicyIndexV1(t *testing.T, policy PolicyV1, provider Provider) int {
	t.Helper()
	for index := range policy.Providers {
		if policy.Providers[index].Provider == provider {
			return index
		}
	}
	t.Fatalf("provider %q not found", provider)
	return -1
}

func actionPolicyIndexV1(t *testing.T, provider ProviderPolicyV1, action Action) int {
	t.Helper()
	for index := range provider.Actions {
		if provider.Actions[index].Action == action {
			return index
		}
	}
	t.Fatalf("action %q not found for provider %q", action, provider.Provider)
	return -1
}

func TestBaselineV1CompilesCanonicalProviderSafetyContract(t *testing.T) {
	policy := mustBaselineV1(t)
	if err := ValidateV1(policy); err != nil {
		t.Fatalf("validate baseline: %v", err)
	}

	wantEndpoints := map[Provider]string{
		ProviderVocaloidFandom: "https://vocaloid.fandom.com/api.php",
		ProviderMoegirl:        "https://moegirl.icu/api.php",
		ProviderSekaipedia:     "https://www.sekaipedia.org/w/api.php",
	}
	for provider, want := range wantEndpoints {
		got, found := CanonicalEndpointV1(provider)
		if !found || got != want {
			t.Fatalf("endpoint %q = %q, %v; want %q, true", provider, got, found, want)
		}
	}
	if _, found := CanonicalEndpointV1("unsupported"); found {
		t.Fatal("unsupported provider returned an endpoint")
	}

	if policy.Acquirer.CrossProcessCoordination != CrossProcessCoordinationRetainedGlobalLock ||
		policy.Acquirer.LiveStateRoot != FixedLiveStateRootV1 ||
		policy.Acquirer.UnusableStateDisposition != UnusableLiveStateDispositionHold ||
		policy.Acquirer.UnresolvedAdmissionDisposition != UnresolvedAdmissionDispositionHold {
		t.Fatalf("baseline acquirer coordination is incomplete: %+v", policy.Acquirer)
	}

	for _, provider := range policy.Providers {
		if provider.Response.MaxBytes != ResponseSizeCeilingBytesV1 ||
			provider.Scheduling.MinimumStartIntervalSeconds != MinimumStartIntervalSecondsV1 ||
			provider.Scheduling.MaxActualNetworkInFlight != DefaultMaxActualNetworkInFlightV1 ||
			provider.MediaWiki.Maxlag != MediaWikiMaxlagV1 ||
			provider.Transport.Proxy != RuleProhibited || provider.Transport.IPRotation != RuleProhibited ||
			provider.Transport.HeaderRotation != RuleProhibited || provider.Scheduling.HiddenConcurrency != RuleProhibited {
			t.Fatalf("provider %q baseline weakened: %+v", provider.Provider, provider)
		}
		for _, action := range provider.Actions {
			if action.MaxBatchSize != DefaultActionBatchCeilingV1 {
				t.Fatalf("provider %q action %q batch=%d", provider.Provider, action.Action, action.MaxBatchSize)
			}
		}
	}

	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeV1(body)
	if err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	if decoded.Version != ContractVersionV1 || len(decoded.Providers) != 3 {
		t.Fatalf("decoded baseline = %+v", decoded)
	}

	specs := CompiledProviderSpecsV1()
	specs[0].Origin = "https://attacker.invalid"
	specs[0].Actions[0] = "unsupported"
	endpoint, _ := CanonicalEndpointV1(ProviderVocaloidFandom)
	if endpoint != wantEndpoints[ProviderVocaloidFandom] {
		t.Fatalf("returned provider specs mutated compiled endpoint: %q", endpoint)
	}
}

func TestDecodeV1RejectsUnknownDuplicateTrailingMissingNullAndInlineCredentialJSON(t *testing.T) {
	policy := mustBaselineV1(t)
	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(body)
	versionField := fmt.Sprintf(`"version":%q`, ContractVersionV1)
	contactField := fmt.Sprintf(`"contact":%q,`, policy.Identity.Contact)

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown top-level field",
			body: strings.Replace(encoded, "{", `{"unknown":true,`, 1),
			want: "unknown field",
		},
		{
			name: "unknown nested field",
			body: strings.Replace(encoded, `"maxlag":5`, `"maxlag":5,"lagAlias":5`, 1),
			want: "unknown field",
		},
		{
			name: "duplicate top-level field",
			body: strings.Replace(encoded, versionField, versionField+","+versionField, 1),
			want: "duplicate object field",
		},
		{
			name: "duplicate nested field",
			body: strings.Replace(encoded, `"maxlag":5`, `"maxlag":5,"maxlag":5`, 1),
			want: "duplicate object field",
		},
		{
			name: "trailing object",
			body: encoded + `{}`,
			want: "trailing JSON",
		},
		{
			name: "missing contact",
			body: strings.Replace(encoded, contactField, "", 1),
			want: "identity.contact",
		},
		{
			name: "null optional profile",
			body: strings.Replace(encoded, `"actions":[`, `"permissionProfile":null,"actions":[`, 1),
			want: "null is prohibited",
		},
		{
			name: "inline password",
			body: strings.Replace(encoded, `"actions":[`, `"credentialProfile":{"id":"test","version":1,"password":"secret"},"actions":[`, 1),
			want: "inline credential fields",
		},
		{
			name: "inline authorization",
			body: strings.Replace(encoded, `"actions":[`, `"authorization":"Bearer secret","actions":[`, 1),
			want: "inline credential fields",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeV1([]byte(test.body)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateV1RejectsWeakerOverridesUnsupportedValuesAndDuplicates(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*PolicyV1)
	}{
		{name: "unsupported version", want: "version", mutate: func(policy *PolicyV1) { policy.Version = "lyrics-provider-policy/v2" }},
		{name: "false identity", want: "truthful", mutate: func(policy *PolicyV1) { policy.Identity.Truthful = false }},
		{name: "unstable identity", want: "stable", mutate: func(policy *PolicyV1) { policy.Identity.Stable = false }},
		{name: "missing contact", want: "identity.contact", mutate: func(policy *PolicyV1) { policy.Identity.Contact = "" }},
		{name: "contact absent from user agent", want: "exact stable contact", mutate: func(policy *PolicyV1) { policy.Identity.UserAgent = "LyricsPolicyTests/1.0" }},
		{name: "insecure contact", want: "only canonical mailto and HTTPS", mutate: func(policy *PolicyV1) {
			policy.Identity.Contact = "http://operator.test/contact"
			policy.Identity.UserAgent = "LyricsPolicyTests/1.0 (" + policy.Identity.Contact + ")"
		}},
		{name: "generic browser user agent", want: "generic browser", mutate: func(policy *PolicyV1) {
			policy.Identity.UserAgent = "Mozilla/5.0 (" + policy.Identity.Contact + ")"
		}},
		{name: "multiple acquirer processes", want: "maxProcesses", mutate: func(policy *PolicyV1) { policy.Acquirer.MaxProcesses = 2 }},
		{name: "missing retained coordination", want: "crossProcessCoordination", mutate: func(policy *PolicyV1) { policy.Acquirer.CrossProcessCoordination = "absent" }},
		{name: "alternate live state root", want: "liveStateRoot", mutate: func(policy *PolicyV1) { policy.Acquirer.LiveStateRoot = "/private/tmp/other" }},
		{name: "auto-zero unusable state", want: "unusableStateDisposition", mutate: func(policy *PolicyV1) { policy.Acquirer.UnusableStateDisposition = "initialize" }},
		{name: "auto-clear unresolved admission", want: "unresolvedAdmissionDisposition", mutate: func(policy *PolicyV1) { policy.Acquirer.UnresolvedAdmissionDisposition = "clear" }},
		{name: "missing provider", want: "exactly all", mutate: func(policy *PolicyV1) { policy.Providers = policy.Providers[:2] }},
		{name: "duplicate provider", want: "duplicate provider", mutate: func(policy *PolicyV1) { policy.Providers[2] = policy.Providers[0] }},
		{name: "unsupported provider", want: "unsupported provider", mutate: func(policy *PolicyV1) { policy.Providers[0].Provider = "other" }},
		{name: "noncanonical origin", want: "canonical origin", mutate: func(policy *PolicyV1) { policy.Providers[0].Origin = "http://vocaloid.fandom.com" }},
		{name: "noncanonical api path", want: "canonical API path", mutate: func(policy *PolicyV1) { policy.Providers[0].APIPath = "/w/api.php" }},
		{name: "proxy enabled", want: "proxies are prohibited", mutate: func(policy *PolicyV1) { policy.Providers[0].Transport.Proxy = "allowed" }},
		{name: "ip rotation enabled", want: "IP rotation is prohibited", mutate: func(policy *PolicyV1) { policy.Providers[0].Transport.IPRotation = "allowed" }},
		{name: "header rotation enabled", want: "header rotation is prohibited", mutate: func(policy *PolicyV1) { policy.Providers[0].Transport.HeaderRotation = "allowed" }},
		{name: "larger response", want: "response ceiling", mutate: func(policy *PolicyV1) { policy.Providers[0].Response.MaxBytes++ }},
		{name: "response override", want: "response ceiling", mutate: func(policy *PolicyV1) { policy.Providers[0].Response.MaxBytes-- }},
		{name: "faster starts", want: "must be at least 10", mutate: func(policy *PolicyV1) { policy.Providers[0].Scheduling.MinimumStartIntervalSeconds = 9 }},
		{name: "raised actual concurrency", want: "mandatory ceiling 1", mutate: func(policy *PolicyV1) { policy.Providers[0].Scheduling.MaxActualNetworkInFlight = 2 }},
		{name: "nonpositive concurrency", want: "mandatory ceiling 1", mutate: func(policy *PolicyV1) { policy.Providers[0].Scheduling.MaxActualNetworkInFlight = 0 }},
		{name: "logical concurrency accounting", want: "actual network requests", mutate: func(policy *PolicyV1) { policy.Providers[0].Scheduling.ConcurrencyAccounting = "logical_workers" }},
		{name: "hidden concurrency", want: "hidden concurrency is prohibited", mutate: func(policy *PolicyV1) { policy.Providers[0].Scheduling.HiddenConcurrency = "allowed" }},
		{name: "weaker maxlag", want: "exactly 5", mutate: func(policy *PolicyV1) { policy.Providers[0].MediaWiki.Maxlag = 4 }},
		{name: "process local cooldown", want: "provider-wide", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.Scope = "process" }},
		{name: "volatile cooldown", want: "persistent", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.Persistence = "memory" }},
		{name: "action-local cooldown", want: "share one cooldown", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.SharedAcrossActions = false }},
		{name: "missing 503 retry after", want: "exactly 429 and 503", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.RetryAfterStatuses = []int{429} }},
		{name: "duplicate retry status", want: "duplicate status", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.RetryAfterStatuses = []int{429, 429} }},
		{name: "retry after cap", want: "never shortened", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.RetryAfterRule = "cap_at_5m" }},
		{name: "wrong maxlag class", want: "provider overload", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.MaxlagClassification = "generic_error" }},
		{name: "separate maxlag cooldown", want: "same provider-wide cooldown", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.MaxlagUsesSameCooldown = false }},
		{name: "short fallback", want: "at least 60", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.FallbackSeconds = 59 }},
		{name: "weak backoff", want: "at least 2", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.BackoffMultiplier = 1 }},
		{name: "shortening backoff", want: "extend-only", mutate: func(policy *PolicyV1) { policy.Providers[0].Cooldown.BackoffMode = "jitter_below" }},
		{name: "unsupported action", want: "unsupported action", mutate: func(policy *PolicyV1) { policy.Providers[0].Actions[0].Action = "bulk_export" }},
		{name: "duplicate action", want: "duplicate action", mutate: func(policy *PolicyV1) { policy.Providers[0].Actions[1] = policy.Providers[0].Actions[0] }},
		{name: "unapproved batch inflation", want: "reviewed ceiling 1", mutate: func(policy *PolicyV1) { policy.Providers[0].Actions[0].MaxBatchSize = 2 }},
		{name: "missing action", want: "requires exactly", mutate: func(policy *PolicyV1) { policy.Providers[0].Actions = policy.Providers[0].Actions[:2] }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := mustBaselineV1(t)
			test.mutate(&policy)
			if err := ValidateV1(policy); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateV1PermissionProfilesAreExternalVersionedScopedAndExact(t *testing.T) {
	permissionRef := ProfileRefV1{ID: "test-fandom-throughput", Version: 1}
	approval := ApprovalMetadataV1{ReviewReference: "review/test-fandom-throughput-v1", ReviewedAt: "2026-08-01T00:00:00Z"}
	approved := ApprovedPermissionProfileV1{
		Ref: permissionRef, Provider: ProviderVocaloidFandom,
		MaxActualNetworkInFlight: 1,
		ActionBatchCeilings:      []ActionPolicyV1{{Action: ActionSearch, MaxBatchSize: 3}},
		Approval:                 approval,
	}
	registry := ApprovalRegistryV1{PermissionProfiles: []ApprovedPermissionProfileV1{approved}}

	policy := mustBaselineV1(t)
	providerIndex := providerPolicyIndexV1(t, policy, ProviderVocaloidFandom)
	searchIndex := actionPolicyIndexV1(t, policy.Providers[providerIndex], ActionSearch)
	policy.Providers[providerIndex].PermissionProfile = &permissionRef
	policy.Providers[providerIndex].Scheduling.MaxActualNetworkInFlight = 1
	policy.Providers[providerIndex].Actions[searchIndex].MaxBatchSize = 3
	if err := ValidateV1WithApprovals(policy, registry); err != nil {
		t.Fatalf("approved permission profile rejected: %v", err)
	}

	tests := []struct {
		name           string
		want           string
		mutatePolicy   func(*PolicyV1)
		mutateRegistry func(*ApprovalRegistryV1)
	}{
		{
			name: "unapproved profile reference", want: "not present",
			mutateRegistry: func(registry *ApprovalRegistryV1) { registry.PermissionProfiles = nil },
		},
		{
			name: "registry cannot raise actual concurrency", want: "remain exactly 1",
			mutateRegistry: func(registry *ApprovalRegistryV1) { registry.PermissionProfiles[0].MaxActualNetworkInFlight = 2 },
		},
		{
			name: "actual concurrency cannot be approved", want: "mandatory ceiling 1",
			mutatePolicy: func(policy *PolicyV1) { policy.Providers[providerIndex].Scheduling.MaxActualNetworkInFlight = 2 },
		},
		{
			name: "batch exceeds approval", want: "reviewed ceiling 3",
			mutatePolicy: func(policy *PolicyV1) { policy.Providers[providerIndex].Actions[searchIndex].MaxBatchSize = 4 },
		},
		{
			name: "batch action not approved", want: "reviewed ceiling 1",
			mutatePolicy: func(policy *PolicyV1) { policy.Providers[providerIndex].Actions[1].MaxBatchSize = 2 },
		},
		{
			name: "profile scoped to another provider", want: "approved for provider",
			mutateRegistry: func(registry *ApprovalRegistryV1) {
				registry.PermissionProfiles[0].Provider = ProviderMoegirl
				registry.PermissionProfiles[0].ActionBatchCeilings = []ActionPolicyV1{{Action: ActionPageByTitle, MaxBatchSize: 2}}
			},
		},
		{
			name: "duplicate approval profile", want: "duplicate permission profile",
			mutateRegistry: func(registry *ApprovalRegistryV1) {
				registry.PermissionProfiles = append(registry.PermissionProfiles, registry.PermissionProfiles[0])
			},
		},
		{
			name: "unsupported approved action", want: "unsupported action",
			mutateRegistry: func(registry *ApprovalRegistryV1) {
				registry.PermissionProfiles[0].ActionBatchCeilings[0].Action = "bulk_export"
			},
		},
		{
			name: "unversioned profile", want: "positive immutable profile version",
			mutateRegistry: func(registry *ApprovalRegistryV1) { registry.PermissionProfiles[0].Ref.Version = 0 },
		},
		{
			name: "missing review metadata", want: "reviewReference",
			mutateRegistry: func(registry *ApprovalRegistryV1) { registry.PermissionProfiles[0].Approval.ReviewReference = "" },
		},
		{
			name: "noncanonical review time", want: "canonical UTC RFC3339",
			mutateRegistry: func(registry *ApprovalRegistryV1) {
				registry.PermissionProfiles[0].Approval.ReviewedAt = "2026-08-01T00:00:00+00:00"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidatePolicy := policy
			candidatePolicy.Providers = append([]ProviderPolicyV1(nil), policy.Providers...)
			candidatePolicy.Providers[providerIndex].Actions = append([]ActionPolicyV1(nil), policy.Providers[providerIndex].Actions...)
			candidateRegistry := registry
			candidateRegistry.PermissionProfiles = append([]ApprovedPermissionProfileV1(nil), registry.PermissionProfiles...)
			candidateRegistry.PermissionProfiles[0].ActionBatchCeilings = append([]ActionPolicyV1(nil), registry.PermissionProfiles[0].ActionBatchCeilings...)
			if test.mutatePolicy != nil {
				test.mutatePolicy(&candidatePolicy)
			}
			if test.mutateRegistry != nil {
				test.mutateRegistry(&candidateRegistry)
			}
			if err := ValidateV1WithApprovals(candidatePolicy, candidateRegistry); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestReviewedPermissionProfilesCannotWeakenCompiledOriginsFloorsOrRetryAfter(t *testing.T) {
	permissionRef := ProfileRefV1{ID: "test-reviewed-throughput", Version: 1}
	registry := ApprovalRegistryV1{PermissionProfiles: []ApprovedPermissionProfileV1{{
		Ref: permissionRef, Provider: ProviderVocaloidFandom,
		MaxActualNetworkInFlight: 1,
		ActionBatchCeilings:      []ActionPolicyV1{{Action: ActionSearch, MaxBatchSize: 2}},
		Approval: ApprovalMetadataV1{
			ReviewReference: "review/test-reviewed-throughput-v1", ReviewedAt: "2026-08-01T00:00:00Z",
		},
	}}}
	baseline := mustBaselineV1(t)
	providerIndex := providerPolicyIndexV1(t, baseline, ProviderVocaloidFandom)
	searchIndex := actionPolicyIndexV1(t, baseline.Providers[providerIndex], ActionSearch)
	baseline.Providers[providerIndex].PermissionProfile = &permissionRef
	baseline.Providers[providerIndex].Scheduling.MaxActualNetworkInFlight = 1
	baseline.Providers[providerIndex].Actions[searchIndex].MaxBatchSize = 2
	if err := ValidateV1WithApprovals(baseline, registry); err != nil {
		t.Fatalf("reviewed throughput policy rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ProviderPolicyV1){
		"origin": func(provider *ProviderPolicyV1) { provider.Origin = "https://attacker.invalid" },
		"start floor": func(provider *ProviderPolicyV1) {
			provider.Scheduling.MinimumStartIntervalSeconds = MinimumStartIntervalSecondsV1 - 1
		},
		"retry after": func(provider *ProviderPolicyV1) { provider.Cooldown.RetryAfterRule = "cap_at_5m" },
		"fallback floor": func(provider *ProviderPolicyV1) {
			provider.Cooldown.FallbackSeconds = MinimumFallbackCooldownSecondsV1 - 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := baseline
			candidate.Providers = append([]ProviderPolicyV1(nil), baseline.Providers...)
			candidate.Providers[providerIndex].Actions = append([]ActionPolicyV1(nil), baseline.Providers[providerIndex].Actions...)
			mutate(&candidate.Providers[providerIndex])
			if err := ValidateV1WithApprovals(candidate, registry); err == nil {
				t.Fatalf("reviewed throughput metadata weakened %s", name)
			}
		})
	}
}

func TestValidateV1CredentialProfilesAreApprovedMetadataOnly(t *testing.T) {
	credentialRef := ProfileRefV1{ID: "test-sekaipedia-credential", Version: 1}
	registry := ApprovalRegistryV1{CredentialProfiles: []ApprovedCredentialProfileV1{{
		Ref: credentialRef, Provider: ProviderSekaipedia,
		Approval: ApprovalMetadataV1{ReviewReference: "review/test-sekaipedia-credential-v1", ReviewedAt: "2026-08-01T00:00:00Z"},
	}}}
	policy := mustBaselineV1(t)
	providerIndex := providerPolicyIndexV1(t, policy, ProviderSekaipedia)
	policy.Providers[providerIndex].CredentialProfile = &credentialRef
	if err := ValidateV1WithApprovals(policy, registry); err != nil {
		t.Fatalf("approved credential metadata rejected: %v", err)
	}

	tests := []struct {
		name   string
		want   string
		mutate func(*ApprovalRegistryV1)
	}{
		{name: "unapproved", want: "not present", mutate: func(registry *ApprovalRegistryV1) { registry.CredentialProfiles = nil }},
		{name: "wrong provider", want: "approved for provider", mutate: func(registry *ApprovalRegistryV1) { registry.CredentialProfiles[0].Provider = ProviderMoegirl }},
		{name: "duplicate metadata", want: "duplicate credential profile", mutate: func(registry *ApprovalRegistryV1) {
			registry.CredentialProfiles = append(registry.CredentialProfiles, registry.CredentialProfiles[0])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := registry
			candidate.CredentialProfiles = append([]ApprovedCredentialProfileV1(nil), registry.CredentialProfiles...)
			test.mutate(&candidate)
			if err := ValidateV1WithApprovals(policy, candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want substring %q", err, test.want)
			}
		})
	}
}
