// Package lyricsprovidercoord owns cross-process live lyrics-provider request
// admission. Offline acquisition and replay paths must not open this package's
// fixed state root.
package lyricsprovidercoord

import (
	"errors"
	"time"

	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

const (
	stateSchemaVersionV1 = "lyrics-provider-live-state/v1"
	stateIdleV1          = "idle"
	stateAdmittedV1      = "admitted"

	globalLockFileV1      = "global-live.lock"
	maximumRecordBytesV1  = 16 << 10
	maximumFailureCountV1 = 27
)

var (
	// ErrHold means live acquisition must not proceed. Missing, unprovisioned,
	// corrupt, busy, cooled-down beyond the request context, or unresolved state
	// all use this disposition and are never initialized or cleared implicitly.
	ErrHold = errors.New("live lyrics-provider acquisition is HOLD")

	providerRecordFilesV1 = map[lyricsproviderpolicy.Provider]string{
		lyricsproviderpolicy.ProviderVocaloidFandom: "vocaloid_fandom.json",
		lyricsproviderpolicy.ProviderMoegirl:        "moegirl.json",
		lyricsproviderpolicy.ProviderSekaipedia:     "sekaipedia.json",
	}
)

type providerRecordV1 struct {
	SchemaVersion string                        `json:"schemaVersion"`
	Provider      lyricsproviderpolicy.Provider `json:"provider"`
	Generation    uint64                        `json:"generation"`
	State         string                        `json:"state"`
	NotBefore     string                        `json:"notBefore"`
	FailureCount  uint32                        `json:"failureCount"`
	Admission     *admissionRecordV1            `json:"admission,omitempty"`
}

type admissionRecordV1 struct {
	ID            string `json:"id"`
	AdmittedAt    string `json:"admittedAt"`
	RequestSHA256 string `json:"requestSha256"`
}

type admissionV1 struct {
	provider lyricsproviderpolicy.Provider
	id       string
}

func canonicalTimeV1(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
