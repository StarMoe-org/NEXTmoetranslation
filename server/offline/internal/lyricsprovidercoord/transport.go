package lyricsprovidercoord

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

// Wrap returns a provider transport whose request admission and completion are
// durably recorded under this retained owner. The returned transport cannot be
// used after Owner.Close.
func (owner *Owner) Wrap(provider lyricsproviderpolicy.Provider, next http.RoundTripper) (http.RoundTripper, error) {
	if owner == nil || next == nil {
		return nil, holdV1("coordinated provider transport requires an owner and underlying transport")
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.failure != nil {
		return nil, owner.failure
	}
	if err := owner.verifyRootLocked(); err != nil {
		return nil, owner.failLocked(err)
	}
	if _, supported := owner.records[provider]; !supported {
		return nil, holdV1("coordinated transport provider %q is not provisioned", provider)
	}
	endpointValue, supported := lyricsproviderpolicy.CanonicalEndpointV1(provider)
	endpoint, err := url.Parse(endpointValue)
	if !supported || err != nil || endpoint == nil {
		return nil, holdV1("coordinated transport provider %q has no canonical endpoint", provider)
	}
	return &coordinatedTransportV1{owner: owner, provider: provider, endpoint: endpoint, next: next}, nil
}

type coordinatedTransportV1 struct {
	owner    *Owner
	provider lyricsproviderpolicy.Provider
	endpoint *url.URL
	next     http.RoundTripper
}

func (transport *coordinatedTransportV1) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.Context() == nil {
		return nil, holdV1("coordinated live request is invalid")
	}
	if request.Method != http.MethodGet || request.Body != nil {
		return nil, holdV1("coordinated live requests must be bodyless GET requests")
	}
	if transport.endpoint == nil || request.URL.Scheme != transport.endpoint.Scheme || request.URL.Host != transport.endpoint.Host ||
		request.URL.EscapedPath() != transport.endpoint.EscapedPath() || request.URL.User != nil || request.URL.Fragment != "" ||
		request.URL.ForceQuery || request.URL.RawQuery == "" {
		return nil, holdV1("coordinated live request does not use the provider's exact canonical endpoint")
	}
	query := request.URL.Query()
	if request.URL.RawQuery != query.Encode() {
		return nil, holdV1("coordinated live request query is not canonical")
	}
	maxlagValues := query["maxlag"]
	if len(maxlagValues) != 1 || maxlagValues[0] != strconv.Itoa(lyricsproviderpolicy.MediaWikiMaxlagV1) {
		return nil, holdV1("coordinated MediaWiki request must contain exactly maxlag=5")
	}
	return coordinatedRoundTripV1(
		transport.owner, transport.provider, transport.next, request, true,
	)
}

type coordinatedExactPublicPageTransportV1 struct {
	owner    *Owner
	provider lyricsproviderpolicy.Provider
	exactURL string
	next     http.RoundTripper
}

func (transport *coordinatedExactPublicPageTransportV1) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	if request == nil || request.URL == nil || request.Context() == nil ||
		request.Method != http.MethodGet || request.Body != nil || request.URL.String() != transport.exactURL {
		return nil, holdV1("coordinated exact public page request does not match the reviewed URL")
	}
	return coordinatedRoundTripV1(
		transport.owner, transport.provider, transport.next, request, false,
	)
}

func coordinatedRoundTripV1(
	owner *Owner,
	provider lyricsproviderpolicy.Provider,
	next http.RoundTripper,
	request *http.Request,
	classifyMediaWikiMaxlag bool,
) (*http.Response, error) {
	admission, err := owner.admit(request.Context(), provider, request)
	if err != nil {
		return nil, err
	}

	response, roundTripErr := next.RoundTrip(request)
	if roundTripErr != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cause := roundTripErr
		if cause == nil {
			cause = errors.New("underlying transport returned an incomplete response")
		}
		return nil, owner.leaveUnresolved(admission, cause)
	}

	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		if err := owner.complete(admission, true, response.Header.Values("Retry-After")); err != nil {
			_ = response.Body.Close()
			return nil, err
		}
		response.Body = owner.observedBody(admission, response.Body)
		return response, nil
	}

	body, readErr := io.ReadAll(io.LimitReader(response.Body, int64(lyricsproviderpolicy.ResponseSizeCeilingBytesV1)+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(body) > lyricsproviderpolicy.ResponseSizeCeilingBytesV1 {
		cause := errors.Join(readErr, closeErr)
		if len(body) > lyricsproviderpolicy.ResponseSizeCeilingBytesV1 {
			cause = errors.Join(cause, errors.New("response exceeded the compiled byte ceiling"))
		}
		return nil, owner.leaveUnresolved(admission, cause)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))

	overload := false
	if classifyMediaWikiMaxlag {
		overload = mediaWikiMaxlagV1(body)
	}
	if err := owner.complete(admission, overload, response.Header.Values("Retry-After")); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	response.Body = owner.observedBody(admission, response.Body)
	return response, nil
}

func (owner *Owner) admit(
	ctx context.Context,
	provider lyricsproviderpolicy.Provider,
	request *http.Request,
) (admissionV1, error) {
	if ctx == nil {
		return admissionV1{}, holdV1("live request context is required")
	}
	requestDigest := sha256.Sum256([]byte(request.Method + "\n" + request.URL.String()))
	for {
		if err := ctx.Err(); err != nil {
			return admissionV1{}, holdV1("live request context ended before durable admission: %v", err)
		}
		owner.mu.Lock()
		if owner.failure != nil {
			err := owner.failure
			owner.mu.Unlock()
			return admissionV1{}, err
		}
		if err := owner.verifyRootLocked(); err != nil {
			err = owner.failLocked(err)
			owner.mu.Unlock()
			return admissionV1{}, err
		}
		expected, supported := owner.records[provider]
		if !supported {
			owner.mu.Unlock()
			return admissionV1{}, holdV1("provider %q is not provisioned", provider)
		}
		current, err := owner.readRecordLocked(provider)
		if err != nil || !reflect.DeepEqual(current, expected) {
			if err == nil {
				err = holdV1("provider %q live state changed outside the retained owner", provider)
			}
			err = owner.failLocked(err)
			owner.mu.Unlock()
			return admissionV1{}, err
		}
		observedID, responseObserved := owner.observed[provider]
		canSupersedeObserved := expected.State == stateAdmittedV1 && expected.Admission != nil &&
			responseObserved && observedID == expected.Admission.ID
		if (expected.State != stateIdleV1 || expected.Admission != nil) && !canSupersedeObserved {
			owner.mu.Unlock()
			return admissionV1{}, holdV1("provider %q already has an unresolved admitted request", provider)
		}
		notBefore, err := parseCanonicalTimeV1(expected.NotBefore)
		if err != nil {
			err = owner.failLocked(err)
			owner.mu.Unlock()
			return admissionV1{}, err
		}
		now := time.Now().UTC()
		if now.Before(notBefore) {
			owner.mu.Unlock()
			timer := time.NewTimer(notBefore.Sub(now))
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return admissionV1{}, holdV1("provider %q durable cooldown remains in force through %s: %v", provider, expected.NotBefore, ctx.Err())
			case <-timer.C:
			}
			continue
		}
		id, err := newAdmissionIDV1()
		if err != nil {
			err = owner.failLocked(err)
			owner.mu.Unlock()
			return admissionV1{}, err
		}
		replacement := cloneRecordV1(expected)
		if replacement.Generation == math.MaxUint64 {
			err = owner.failLocked(holdV1("provider %q live state generation is exhausted", provider))
			owner.mu.Unlock()
			return admissionV1{}, err
		}
		replacement.Generation++
		replacement.State = stateAdmittedV1
		minimumNotBefore := now.Add(time.Duration(lyricsproviderpolicy.MinimumStartIntervalSecondsV1) * time.Second)
		if minimumNotBefore.After(notBefore) {
			replacement.NotBefore = canonicalTimeV1(minimumNotBefore)
		}
		replacement.Admission = &admissionRecordV1{
			ID: id, AdmittedAt: canonicalTimeV1(now), RequestSHA256: hex.EncodeToString(requestDigest[:]),
		}
		if err := owner.persistRecordLocked(provider, expected, replacement); err != nil {
			owner.mu.Unlock()
			return admissionV1{}, err
		}
		if canSupersedeObserved {
			delete(owner.observed, provider)
		}
		owner.mu.Unlock()
		return admissionV1{provider: provider, id: id}, nil
	}
}

func newAdmissionIDV1() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", holdV1("generate durable request admission identity: %v", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (owner *Owner) complete(admission admissionV1, overload bool, retryAfterValues []string) error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.failure != nil {
		return owner.failure
	}
	expected, supported := owner.records[admission.provider]
	if !supported || expected.State != stateAdmittedV1 || expected.Admission == nil || expected.Admission.ID != admission.id {
		return owner.failLocked(holdV1("provider %q request completion does not match the durable admission", admission.provider))
	}
	current, err := owner.readRecordLocked(admission.provider)
	if err != nil || !reflect.DeepEqual(current, expected) {
		if err == nil {
			err = holdV1("provider %q live state changed before request completion", admission.provider)
		}
		return owner.failLocked(err)
	}

	replacement := cloneRecordV1(expected)
	if replacement.Generation == math.MaxUint64 {
		return owner.failLocked(holdV1("provider %q live state generation is exhausted", admission.provider))
	}
	replacement.Generation++
	if overload {
		if replacement.FailureCount >= maximumFailureCountV1 {
			return owner.failLocked(holdV1("provider %q overload backoff is exhausted", admission.provider))
		}
		now := time.Now().UTC()
		notBefore, err := parseCanonicalTimeV1(replacement.NotBefore)
		if err != nil {
			return owner.failLocked(err)
		}
		retryNotBefore, usable, err := retryAfterNotBeforeV1(retryAfterValues, now)
		if err != nil {
			return owner.failLocked(err)
		}
		if !usable {
			retryNotBefore, err = fallbackNotBeforeV1(now, replacement.FailureCount)
			if err != nil {
				return owner.failLocked(err)
			}
		}
		if retryNotBefore.After(notBefore) {
			replacement.NotBefore = canonicalTimeV1(retryNotBefore)
		}
		replacement.FailureCount++
	} else {
		replacement.FailureCount = 0
	}
	return owner.persistRecordLocked(admission.provider, expected, replacement)
}

func (owner *Owner) observedBody(admission admissionV1, body io.ReadCloser) io.ReadCloser {
	return &admissionResponseBodyV1{body: body, observe: func() error { return owner.observe(admission) }}
}

func (owner *Owner) observe(admission admissionV1) error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.failure != nil {
		return owner.failure
	}
	expected, supported := owner.records[admission.provider]
	if !supported || expected.State != stateAdmittedV1 || expected.Admission == nil || expected.Admission.ID != admission.id {
		return owner.failLocked(holdV1("provider %q response observation does not match the durable admission", admission.provider))
	}
	current, err := owner.readRecordLocked(admission.provider)
	if err != nil || !reflect.DeepEqual(current, expected) {
		if err == nil {
			err = holdV1("provider %q live state changed before response observation", admission.provider)
		}
		return owner.failLocked(err)
	}
	owner.observed[admission.provider] = admission.id
	return nil
}

type admissionResponseBodyV1 struct {
	body    io.ReadCloser
	observe func() error
	once    sync.Once
	err     error
}

func (body *admissionResponseBodyV1) Read(target []byte) (int, error) {
	return body.body.Read(target)
}

func (body *admissionResponseBodyV1) Close() error {
	closeErr := body.body.Close()
	body.once.Do(func() {
		if closeErr == nil {
			body.err = body.observe()
		}
	})
	return errors.Join(closeErr, body.err)
}

// ResolveProvider clears only the latest response observed by this retained
// owner. Callers must invoke it only after the higher-level acquisition path
// has durably committed or terminally classified every request for a provider.
func (owner *Owner) ResolveProvider(provider lyricsproviderpolicy.Provider) error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.failure != nil {
		return owner.failure
	}
	expected, supported := owner.records[provider]
	if !supported {
		return holdV1("provider %q is not provisioned", provider)
	}
	if expected.State == stateIdleV1 && expected.Admission == nil {
		return nil
	}
	observedID, observed := owner.observed[provider]
	if !observed || expected.State != stateAdmittedV1 || expected.Admission == nil || expected.Admission.ID != observedID {
		return owner.failLocked(holdV1("provider %q has no safely resolvable observed response", provider))
	}
	current, err := owner.readRecordLocked(provider)
	if err != nil || !reflect.DeepEqual(current, expected) {
		if err == nil {
			err = holdV1("provider %q live state changed before resolution", provider)
		}
		return owner.failLocked(err)
	}
	replacement := cloneRecordV1(expected)
	if replacement.Generation == math.MaxUint64 {
		return owner.failLocked(holdV1("provider %q live state generation is exhausted", provider))
	}
	replacement.Generation++
	replacement.State = stateIdleV1
	replacement.Admission = nil
	if err := owner.persistRecordLocked(provider, expected, replacement); err != nil {
		return err
	}
	delete(owner.observed, provider)
	return nil
}

func (owner *Owner) leaveUnresolved(admission admissionV1, cause error) error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.failure != nil {
		return owner.failure
	}
	record, supported := owner.records[admission.provider]
	if !supported || record.State != stateAdmittedV1 || record.Admission == nil || record.Admission.ID != admission.id {
		return owner.failLocked(holdV1("provider request became unresolved without a matching durable admission"))
	}
	return owner.failLocked(holdV1("provider %q admitted request remains unresolved: %v", admission.provider, cause))
}

func mediaWikiMaxlagV1(body []byte) bool {
	var response struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &response) == nil && response.Error != nil && strings.EqualFold(strings.TrimSpace(response.Error.Code), "maxlag")
}

func retryAfterNotBeforeV1(values []string, now time.Time) (time.Time, bool, error) {
	if len(values) == 0 {
		return time.Time{}, false, nil
	}
	if len(values) != 1 {
		return time.Time{}, false, holdV1("multiple Retry-After header values are ambiguous")
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return time.Time{}, false, nil
	}
	allDigits := true
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Time{}, false, holdV1("Retry-After delay cannot be represented without shortening")
		}
		return now.Add(time.Duration(seconds) * time.Second), true, nil
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, false, nil
	}
	retryAt = retryAt.UTC()
	if !retryAt.After(now) {
		return now, true, nil
	}
	return retryAt, true, nil
}

func fallbackNotBeforeV1(now time.Time, failureCount uint32) (time.Time, error) {
	if failureCount >= maximumFailureCountV1 {
		return time.Time{}, holdV1("provider overload fallback backoff cannot be extended safely")
	}
	base := time.Duration(lyricsproviderpolicy.MinimumFallbackCooldownSecondsV1) * time.Second
	multiplier := time.Duration(1) << failureCount
	if multiplier > 0 && base > time.Duration(math.MaxInt64)/multiplier {
		return time.Time{}, holdV1("provider overload fallback backoff cannot be represented without shortening")
	}
	return now.Add(base * multiplier), nil
}

func (transport *coordinatedTransportV1) String() string {
	return fmt.Sprintf("coordinated-live-transport(%s)", transport.provider)
}
