package lyricsproviderpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxJSONDepthV1 = 32

var (
	ErrInvalidPolicyV1 = errors.New("invalid lyrics provider policy v1")

	profileIDPatternV1 = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reviewRefPatternV1 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,255}$`)
	userAgentProductV1 = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._+-]{0,31}(?: |$)`)
)

// ValidationError identifies the closed-contract path that failed.
type ValidationError struct {
	Path   string
	Reason string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ErrInvalidPolicyV1.Error()
	}
	return fmt.Sprintf("%s: %s: %s", ErrInvalidPolicyV1, e.Path, e.Reason)
}

func (e *ValidationError) Unwrap() error {
	return ErrInvalidPolicyV1
}

func invalidPolicyV1(path, format string, args ...any) error {
	return &ValidationError{Path: path, Reason: fmt.Sprintf(format, args...)}
}

// DecodeV1 performs closed JSON decoding and validates with no exception
// profiles. Unknown, duplicate, trailing, null, and inline-credential fields
// fail closed.
func DecodeV1(data []byte) (PolicyV1, error) {
	return DecodeV1WithApprovals(data, ApprovalRegistryV1{})
}

// DecodeV1WithApprovals performs closed JSON decoding against a trusted
// external approval registry.
func DecodeV1WithApprovals(data []byte, approvals ApprovalRegistryV1) (PolicyV1, error) {
	if err := inspectClosedJSONV1(data); err != nil {
		return PolicyV1{}, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy PolicyV1
	if err := decoder.Decode(&policy); err != nil {
		return PolicyV1{}, invalidPolicyV1("$", "closed JSON decode failed: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return PolicyV1{}, invalidPolicyV1("$", "trailing JSON is prohibited")
	}
	if err := ValidateV1WithApprovals(policy, approvals); err != nil {
		return PolicyV1{}, err
	}
	return policy, nil
}

func inspectClosedJSONV1(data []byte) error {
	if len(data) == 0 {
		return invalidPolicyV1("$", "empty JSON is prohibited")
	}
	if !utf8.Valid(data) {
		return invalidPolicyV1("$", "invalid UTF-8 is prohibited")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSONValueV1(decoder, "$", 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return invalidPolicyV1("$", "trailing JSON is prohibited")
		}
		return invalidPolicyV1("$", "malformed or trailing JSON: %v", err)
	}
	return nil
}

func inspectJSONValueV1(decoder *json.Decoder, path string, depth int) error {
	if depth > maxJSONDepthV1 {
		return invalidPolicyV1(path, "JSON nesting exceeds %d", maxJSONDepthV1)
	}
	token, err := decoder.Token()
	if err != nil {
		return invalidPolicyV1(path, "malformed JSON: %v", err)
	}
	if token == nil {
		return invalidPolicyV1(path, "null is prohibited; omit optional fields")
	}
	delim, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return invalidPolicyV1(path, "malformed object key: %v", keyErr)
			}
			key, ok := keyToken.(string)
			if !ok {
				return invalidPolicyV1(path, "object key is not a string")
			}
			childPath := path + "." + key
			if _, duplicate := seen[key]; duplicate {
				return invalidPolicyV1(childPath, "duplicate object field is prohibited")
			}
			seen[key] = struct{}{}
			if isInlineCredentialFieldV1(key) {
				return invalidPolicyV1(childPath, "inline credential fields are prohibited")
			}
			if err := inspectJSONValueV1(decoder, childPath, depth+1); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim('}') {
			return invalidPolicyV1(path, "malformed object closing delimiter")
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := inspectJSONValueV1(decoder, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
			index++
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim(']') {
			return invalidPolicyV1(path, "malformed array closing delimiter")
		}
	default:
		return invalidPolicyV1(path, "unexpected JSON delimiter %q", delim)
	}
	return nil
}

func isInlineCredentialFieldV1(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	switch normalized {
	case "authorization", "proxyauthorization", "password", "passwd", "token", "accesstoken",
		"refreshtoken", "apikey", "secret", "clientsecret", "cookie", "setcookie", "username",
		"credential", "credentials":
		return true
	default:
		return false
	}
}

// ValidateV1 validates with no permission or credential exceptions.
func ValidateV1(policy PolicyV1) error {
	return ValidateV1WithApprovals(policy, ApprovalRegistryV1{})
}

// ValidateV1WithApprovals validates the contract and every trusted approval
// entry before using any profile reference.
func ValidateV1WithApprovals(policy PolicyV1, approvals ApprovalRegistryV1) error {
	permissionProfiles, credentialProfiles, err := validateApprovalRegistryV1(approvals)
	if err != nil {
		return err
	}
	if policy.Version != ContractVersionV1 {
		return invalidPolicyV1("$.version", "must equal %q", ContractVersionV1)
	}
	if err := validateIdentityV1(policy.Identity); err != nil {
		return err
	}
	if policy.Acquirer.MaxProcesses != MaxAcquirerProcessesV1 {
		return invalidPolicyV1("$.acquirer.maxProcesses", "must remain exactly %d", MaxAcquirerProcessesV1)
	}
	if policy.Acquirer.CrossProcessCoordination != CrossProcessCoordinationRetainedGlobalLock {
		return invalidPolicyV1("$.acquirer.crossProcessCoordination", "must equal %q", CrossProcessCoordinationRetainedGlobalLock)
	}
	if policy.Acquirer.LiveStateRoot != FixedLiveStateRootV1 {
		return invalidPolicyV1("$.acquirer.liveStateRoot", "must equal the fixed private local state root %q", FixedLiveStateRootV1)
	}
	if policy.Acquirer.UnusableStateDisposition != UnusableLiveStateDispositionHold {
		return invalidPolicyV1("$.acquirer.unusableStateDisposition", "missing, unprovisioned, or corrupt state must HOLD")
	}
	if policy.Acquirer.UnresolvedAdmissionDisposition != UnresolvedAdmissionDispositionHold {
		return invalidPolicyV1("$.acquirer.unresolvedAdmissionDisposition", "unresolved admitted requests must HOLD")
	}
	if len(policy.Providers) != len(compiledProviderSpecsV1) {
		return invalidPolicyV1("$.providers", "must contain exactly all %d compiled providers", len(compiledProviderSpecsV1))
	}

	seenProviders := make(map[Provider]struct{}, len(policy.Providers))
	for index, providerPolicy := range policy.Providers {
		path := fmt.Sprintf("$.providers[%d]", index)
		spec, supported := compiledProviderSpecV1(providerPolicy.Provider)
		if !supported {
			return invalidPolicyV1(path+".provider", "unsupported provider %q", providerPolicy.Provider)
		}
		if _, duplicate := seenProviders[providerPolicy.Provider]; duplicate {
			return invalidPolicyV1(path+".provider", "duplicate provider %q", providerPolicy.Provider)
		}
		seenProviders[providerPolicy.Provider] = struct{}{}

		var permission *ApprovedPermissionProfileV1
		if providerPolicy.PermissionProfile != nil {
			if err := validateProfileRefV1(*providerPolicy.PermissionProfile, path+".permissionProfile"); err != nil {
				return err
			}
			approved, found := permissionProfiles[profileKeyV1(*providerPolicy.PermissionProfile)]
			if !found {
				return invalidPolicyV1(path+".permissionProfile", "profile is not present in the trusted reviewed registry")
			}
			if approved.Provider != providerPolicy.Provider {
				return invalidPolicyV1(path+".permissionProfile", "profile is approved for provider %q, not %q", approved.Provider, providerPolicy.Provider)
			}
			copy := approved
			permission = &copy
		}
		if providerPolicy.CredentialProfile != nil {
			if err := validateProfileRefV1(*providerPolicy.CredentialProfile, path+".credentialProfile"); err != nil {
				return err
			}
			approved, found := credentialProfiles[profileKeyV1(*providerPolicy.CredentialProfile)]
			if !found {
				return invalidPolicyV1(path+".credentialProfile", "profile is not present in the trusted reviewed registry")
			}
			if approved.Provider != providerPolicy.Provider {
				return invalidPolicyV1(path+".credentialProfile", "profile is approved for provider %q, not %q", approved.Provider, providerPolicy.Provider)
			}
		}
		if err := validateProviderPolicyV1(providerPolicy, spec, permission, path); err != nil {
			return err
		}
	}
	for _, spec := range compiledProviderSpecsV1 {
		if _, found := seenProviders[spec.Provider]; !found {
			return invalidPolicyV1("$.providers", "compiled provider %q is missing", spec.Provider)
		}
	}
	return nil
}

func validateIdentityV1(identity IdentityV1) error {
	if !identity.Truthful {
		return invalidPolicyV1("$.identity.truthful", "must explicitly attest a truthful identity")
	}
	if !identity.Stable {
		return invalidPolicyV1("$.identity.stable", "must explicitly attest a stable non-rotating identity")
	}
	if identity.UserAgent == "" || identity.UserAgent != strings.TrimSpace(identity.UserAgent) || len(identity.UserAgent) > 512 || containsControlV1(identity.UserAgent) {
		return invalidPolicyV1("$.identity.userAgent", "must be a trimmed control-free value of at most 512 bytes")
	}
	if !userAgentProductV1.MatchString(identity.UserAgent) || strings.HasPrefix(strings.ToLower(identity.UserAgent), "mozilla/") {
		return invalidPolicyV1("$.identity.userAgent", "must begin with a truthful product/version token, not a generic browser identity")
	}
	if err := validateContactV1(identity.Contact); err != nil {
		return err
	}
	if strings.Count(identity.UserAgent, identity.Contact) != 1 {
		return invalidPolicyV1("$.identity.userAgent", "must contain the exact stable contact identity once")
	}
	return nil
}

func validateContactV1(contact string) error {
	if contact == "" || contact != strings.TrimSpace(contact) || len(contact) > 512 || containsControlV1(contact) {
		return invalidPolicyV1("$.identity.contact", "must be a trimmed control-free value of at most 512 bytes")
	}
	parsed, err := url.Parse(contact)
	if err != nil || parsed == nil || parsed.String() != contact || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return invalidPolicyV1("$.identity.contact", "must be a canonical mailto or HTTPS contact without query or fragment")
	}
	switch parsed.Scheme {
	case "mailto":
		if parsed.Opaque == "" || parsed.Host != "" || parsed.Path != "" || parsed.User != nil {
			return invalidPolicyV1("$.identity.contact", "malformed mailto contact")
		}
		address, addressErr := mail.ParseAddress(parsed.Opaque)
		separator := strings.LastIndexByte(parsed.Opaque, '@')
		if addressErr != nil || address.Name != "" || address.Address != parsed.Opaque || separator <= 0 || separator == len(parsed.Opaque)-1 {
			return invalidPolicyV1("$.identity.contact", "mailto contact must contain one canonical address without a display name")
		}
		domain := parsed.Opaque[separator+1:]
		if domain != strings.ToLower(domain) || !validContactHostnameV1(domain) {
			return invalidPolicyV1("$.identity.contact", "mailto contact requires a canonical non-local domain")
		}
	case "https":
		if parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Port() != "" {
			return invalidPolicyV1("$.identity.contact", "HTTPS contact must not contain credentials or an explicit port")
		}
		hostname := parsed.Hostname()
		if parsed.Host != hostname || hostname != strings.ToLower(hostname) || !validContactHostnameV1(hostname) {
			return invalidPolicyV1("$.identity.contact", "HTTPS contact requires a canonical non-local hostname")
		}
	default:
		return invalidPolicyV1("$.identity.contact", "only canonical mailto and HTTPS contacts are supported")
	}
	return nil
}

func validContactHostnameV1(hostname string) bool {
	return hostname != "" && hostname != "localhost" && strings.Contains(hostname, ".") &&
		!strings.HasPrefix(hostname, ".") && !strings.HasSuffix(hostname, ".") && net.ParseIP(hostname) == nil
}

func containsControlV1(value string) bool {
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return true
		}
	}
	return false
}

func validateProviderPolicyV1(policy ProviderPolicyV1, spec ProviderSpecV1, permission *ApprovedPermissionProfileV1, path string) error {
	if policy.Origin != spec.Origin {
		return invalidPolicyV1(path+".origin", "provider %q requires canonical origin %q", spec.Provider, spec.Origin)
	}
	if policy.APIPath != spec.APIPath {
		return invalidPolicyV1(path+".apiPath", "provider %q requires canonical API path %q", spec.Provider, spec.APIPath)
	}
	if policy.Transport.Proxy != RuleProhibited {
		return invalidPolicyV1(path+".transport.proxy", "proxies are prohibited")
	}
	if policy.Transport.IPRotation != RuleProhibited {
		return invalidPolicyV1(path+".transport.ipRotation", "IP rotation is prohibited")
	}
	if policy.Transport.HeaderRotation != RuleProhibited {
		return invalidPolicyV1(path+".transport.headerRotation", "header rotation is prohibited")
	}
	if policy.Response.MaxBytes != ResponseSizeCeilingBytesV1 {
		return invalidPolicyV1(path+".response.maxBytes", "compiled response ceiling must remain exactly %d", ResponseSizeCeilingBytesV1)
	}
	if policy.Scheduling.MinimumStartIntervalSeconds < MinimumStartIntervalSecondsV1 {
		return invalidPolicyV1(path+".scheduling.minimumStartIntervalSeconds", "must be at least %d", MinimumStartIntervalSecondsV1)
	}
	if policy.Scheduling.ConcurrencyAccounting != ConcurrencyAccountingActualNetworkRequests {
		return invalidPolicyV1(path+".scheduling.concurrencyAccounting", "must count actual network requests")
	}
	if policy.Scheduling.HiddenConcurrency != RuleProhibited {
		return invalidPolicyV1(path+".scheduling.hiddenConcurrency", "hidden concurrency is prohibited")
	}
	if policy.Scheduling.MaxActualNetworkInFlight != DefaultMaxActualNetworkInFlightV1 {
		return invalidPolicyV1(path+".scheduling.maxActualNetworkInFlight", "must remain at the mandatory ceiling %d", DefaultMaxActualNetworkInFlightV1)
	}
	if policy.MediaWiki.Maxlag != MediaWikiMaxlagV1 {
		return invalidPolicyV1(path+".mediaWiki.maxlag", "must remain exactly %d", MediaWikiMaxlagV1)
	}
	if err := validateCooldownV1(policy.Cooldown, path+".cooldown"); err != nil {
		return err
	}
	return validateActionsV1(policy.Actions, spec, permission, path+".actions")
}

func validateCooldownV1(policy CooldownPolicyV1, path string) error {
	if policy.Scope != CooldownScopeProviderWide {
		return invalidPolicyV1(path+".scope", "must be provider-wide")
	}
	if policy.Persistence != CooldownPersistencePersistent {
		return invalidPolicyV1(path+".persistence", "must be persistent across process restarts")
	}
	if !policy.SharedAcrossActions {
		return invalidPolicyV1(path+".sharedAcrossActions", "all provider actions must share one cooldown")
	}
	if len(policy.RetryAfterStatuses) != 2 {
		return invalidPolicyV1(path+".retryAfterStatuses", "must contain exactly 429 and 503")
	}
	seen := make(map[int]struct{}, len(policy.RetryAfterStatuses))
	for _, status := range policy.RetryAfterStatuses {
		if _, duplicate := seen[status]; duplicate {
			return invalidPolicyV1(path+".retryAfterStatuses", "duplicate status %d", status)
		}
		seen[status] = struct{}{}
	}
	if _, found := seen[429]; !found {
		return invalidPolicyV1(path+".retryAfterStatuses", "429 is required")
	}
	if _, found := seen[503]; !found {
		return invalidPolicyV1(path+".retryAfterStatuses", "503 is required")
	}
	if policy.RetryAfterRule != RetryAfterRuleNeverShorten {
		return invalidPolicyV1(path+".retryAfterRule", "Retry-After must be a not-before floor that is never shortened")
	}
	if policy.MaxlagClassification != MaxlagClassificationProviderOverload {
		return invalidPolicyV1(path+".maxlagClassification", "maxlag must be classified as retryable provider overload")
	}
	if !policy.MaxlagUsesSameCooldown {
		return invalidPolicyV1(path+".maxlagUsesSameCooldown", "maxlag must extend the same provider-wide cooldown")
	}
	if policy.FallbackSeconds < MinimumFallbackCooldownSecondsV1 {
		return invalidPolicyV1(path+".fallbackSeconds", "must be at least %d", MinimumFallbackCooldownSecondsV1)
	}
	if policy.BackoffMultiplier < MinimumBackoffMultiplierV1 {
		return invalidPolicyV1(path+".backoffMultiplier", "must be at least %d", MinimumBackoffMultiplierV1)
	}
	if policy.BackoffMode != BackoffModeExponentialExtendOnly {
		return invalidPolicyV1(path+".backoffMode", "must use conservative exponential extend-only backoff")
	}
	return nil
}

func validateActionsV1(actions []ActionPolicyV1, spec ProviderSpecV1, permission *ApprovedPermissionProfileV1, path string) error {
	if len(actions) != len(spec.Actions) {
		return invalidPolicyV1(path, "provider %q requires exactly %d supported actions", spec.Provider, len(spec.Actions))
	}
	supported := make(map[Action]struct{}, len(spec.Actions))
	for _, action := range spec.Actions {
		supported[action] = struct{}{}
	}
	approvedCeilings := make(map[Action]int)
	if permission != nil {
		for _, action := range permission.ActionBatchCeilings {
			approvedCeilings[action.Action] = action.MaxBatchSize
		}
	}
	seen := make(map[Action]struct{}, len(actions))
	for index, actionPolicy := range actions {
		actionPath := fmt.Sprintf("%s[%d]", path, index)
		if _, found := supported[actionPolicy.Action]; !found {
			return invalidPolicyV1(actionPath+".action", "unsupported action %q for provider %q", actionPolicy.Action, spec.Provider)
		}
		if _, duplicate := seen[actionPolicy.Action]; duplicate {
			return invalidPolicyV1(actionPath+".action", "duplicate action %q", actionPolicy.Action)
		}
		seen[actionPolicy.Action] = struct{}{}
		if actionPolicy.MaxBatchSize < 1 {
			return invalidPolicyV1(actionPath+".maxBatchSize", "must be positive")
		}
		allowed := DefaultActionBatchCeilingV1
		if approved, found := approvedCeilings[actionPolicy.Action]; found {
			allowed = approved
		}
		if actionPolicy.MaxBatchSize > allowed {
			return invalidPolicyV1(actionPath+".maxBatchSize", "exceeds reviewed ceiling %d", allowed)
		}
	}
	for _, action := range spec.Actions {
		if _, found := seen[action]; !found {
			return invalidPolicyV1(path, "required action %q is missing", action)
		}
	}
	return nil
}

type approvalKeyV1 struct {
	id      string
	version int
}

func profileKeyV1(ref ProfileRefV1) approvalKeyV1 {
	return approvalKeyV1{id: ref.ID, version: ref.Version}
}

func validateApprovalRegistryV1(registry ApprovalRegistryV1) (map[approvalKeyV1]ApprovedPermissionProfileV1, map[approvalKeyV1]ApprovedCredentialProfileV1, error) {
	permissions := make(map[approvalKeyV1]ApprovedPermissionProfileV1, len(registry.PermissionProfiles))
	for index, profile := range registry.PermissionProfiles {
		path := fmt.Sprintf("approvalRegistry.permissionProfiles[%d]", index)
		if err := validateProfileRefV1(profile.Ref, path+".ref"); err != nil {
			return nil, nil, err
		}
		spec, supported := compiledProviderSpecV1(profile.Provider)
		if !supported {
			return nil, nil, invalidPolicyV1(path+".provider", "unsupported provider %q", profile.Provider)
		}
		if err := validateApprovalMetadataV1(profile.Approval, path+".approval"); err != nil {
			return nil, nil, err
		}
		if profile.MaxActualNetworkInFlight != DefaultMaxActualNetworkInFlightV1 {
			return nil, nil, invalidPolicyV1(path+".maxActualNetworkInFlight", "must remain exactly %d", DefaultMaxActualNetworkInFlightV1)
		}
		supportedActions := make(map[Action]struct{}, len(spec.Actions))
		for _, action := range spec.Actions {
			supportedActions[action] = struct{}{}
		}
		raisesPermission := false
		seenActions := make(map[Action]struct{}, len(profile.ActionBatchCeilings))
		for actionIndex, ceiling := range profile.ActionBatchCeilings {
			actionPath := fmt.Sprintf("%s.actionBatchCeilings[%d]", path, actionIndex)
			if _, found := supportedActions[ceiling.Action]; !found {
				return nil, nil, invalidPolicyV1(actionPath+".action", "unsupported action %q for provider %q", ceiling.Action, profile.Provider)
			}
			if _, duplicate := seenActions[ceiling.Action]; duplicate {
				return nil, nil, invalidPolicyV1(actionPath+".action", "duplicate action %q", ceiling.Action)
			}
			seenActions[ceiling.Action] = struct{}{}
			if ceiling.MaxBatchSize <= DefaultActionBatchCeilingV1 {
				return nil, nil, invalidPolicyV1(actionPath+".maxBatchSize", "permission profiles may contain only reviewed increases above %d", DefaultActionBatchCeilingV1)
			}
			raisesPermission = true
		}
		if !raisesPermission {
			return nil, nil, invalidPolicyV1(path, "permission profile does not raise a reviewed ceiling")
		}
		key := profileKeyV1(profile.Ref)
		if _, duplicate := permissions[key]; duplicate {
			return nil, nil, invalidPolicyV1(path+".ref", "duplicate permission profile id/version")
		}
		copy := profile
		copy.ActionBatchCeilings = append([]ActionPolicyV1(nil), profile.ActionBatchCeilings...)
		permissions[key] = copy
	}

	credentials := make(map[approvalKeyV1]ApprovedCredentialProfileV1, len(registry.CredentialProfiles))
	for index, profile := range registry.CredentialProfiles {
		path := fmt.Sprintf("approvalRegistry.credentialProfiles[%d]", index)
		if err := validateProfileRefV1(profile.Ref, path+".ref"); err != nil {
			return nil, nil, err
		}
		if _, supported := compiledProviderSpecV1(profile.Provider); !supported {
			return nil, nil, invalidPolicyV1(path+".provider", "unsupported provider %q", profile.Provider)
		}
		if err := validateApprovalMetadataV1(profile.Approval, path+".approval"); err != nil {
			return nil, nil, err
		}
		key := profileKeyV1(profile.Ref)
		if _, duplicate := credentials[key]; duplicate {
			return nil, nil, invalidPolicyV1(path+".ref", "duplicate credential profile id/version")
		}
		credentials[key] = profile
	}
	return permissions, credentials, nil
}

func validateProfileRefV1(ref ProfileRefV1, path string) error {
	if !profileIDPatternV1.MatchString(ref.ID) {
		return invalidPolicyV1(path+".id", "must be a canonical lowercase metadata identifier")
	}
	if ref.Version <= 0 {
		return invalidPolicyV1(path+".version", "must be a positive immutable profile version")
	}
	return nil
}

func validateApprovalMetadataV1(approval ApprovalMetadataV1, path string) error {
	if !reviewRefPatternV1.MatchString(approval.ReviewReference) {
		return invalidPolicyV1(path+".reviewReference", "must be a non-secret stable review reference")
	}
	reviewedAt, err := time.Parse(time.RFC3339Nano, approval.ReviewedAt)
	if err != nil || reviewedAt.UTC().Format(time.RFC3339Nano) != approval.ReviewedAt || !strings.HasSuffix(approval.ReviewedAt, "Z") {
		return invalidPolicyV1(path+".reviewedAt", "must be canonical UTC RFC3339")
	}
	return nil
}
