// Package lyricsacquisition stores immutable provider acquisitions and a
// descriptor-pinned SQLite index inside a private ledger root.
//
// Immutable leaves are published create-exclusively from an already verified
// source descriptor. Darwin requires fclonefileat on APFS; Linux requires the
// reviewed descriptor-link and O_TMPFILE preflight. A raced source pathname is
// never passed to the final publication syscall, and a raced destination is
// never replaced. Publication sources and completed markers are retained under
// explicit entry and byte bounds because Darwin and Linux provide no portable
// unlink or rename operation conditional on the verified inode.
//
// Metadata uses a bounded reusable state machine of descriptor-pinned slots.
// A commit preserves complete active and standby generations while overwriting
// only a distinct inactive, path-and-inode-verified private slot, fsyncs that
// snapshot, then durably rewrites one of three reusable selector descriptors
// before coherently rebinding SQLite. Crash-torn inactive slots are retained
// and may be reused only after descriptor, owner, mode, link, pathname, and
// inode verification; detected replacements are never touched. Historical v2
// ledgers with only metadata.db and append-only selectors remain readable, but
// new transitions do not grow the selector namespace without bound. The
// highest complete selector is the authoritative current-generation record.
//
// No writable-root-only design can authenticate arbitrary same-effective-UID
// substitution that occurs before a historical reopen. The integrity contract
// therefore excludes a hostile peer with the same effective UID that can
// rewrite the private mode-0700 namespace and fabricate every reviewed owner,
// mode, size, content, SQLite, and selector invariant. Detectable races while a
// ledger is open fail closed without moving, deleting, or writing the raced
// replacement.
package lyricsacquisition

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// AcquisitionID is the SHA-256 content address of one canonical acquisition
// manifest. Exact offline replay accepts only this identity.
type AcquisitionID string

// RequestKind identifies the exact provider operation whose response was
// acquired.
type RequestKind string

const (
	RequestKindSearch     RequestKind = "search"
	RequestKindRevision   RequestKind = "revision"
	RequestKindFixedIndex RequestKind = "fixed_index"
)

// Request preserves the exact canonical request identity and revision selector
// used for an acquisition.
type Request struct {
	Provider                 string
	CanonicalRequestIdentity string
	Kind                     RequestKind
	RevisionSelector         string
}

// ObservedRevision preserves bounded revision metadata observed in the exact
// provider response.
type ObservedRevision struct {
	Selector   string
	RevisionID int64
	Timestamp  string
	SHA1       string
}

// EvidenceProjection binds one exact evidence identity to its immutable raw
// projection bytes.
type EvidenceProjection struct {
	EvidenceID string
	Raw        []byte
	RawSHA256  string
}

// RecordInput is the complete immutable acquisition submitted to the ledger.
// EvidenceEnvelope must be the canonical JSON envelope for Evidence.
type RecordInput struct {
	Request                Request
	FetchedAt              string
	RawResponse            []byte
	RawResponseSHA256      string
	Evidence               EvidenceProjection
	EvidenceEnvelope       []byte
	EvidenceEnvelopeSHA256 string
	ObservedRevisions      []ObservedRevision
}

// Acquisition is an immutable acquisition returned after commit or exact-ID
// replay. Byte slices are defensive copies owned by the caller.
type Acquisition struct {
	AcquisitionID          AcquisitionID
	Request                Request
	FetchedAt              string
	RawResponse            []byte
	RawResponseSHA256      string
	Evidence               EvidenceProjection
	EvidenceEnvelope       []byte
	EvidenceEnvelopeSHA256 string
	ObservedRevisions      []ObservedRevision
	ReplayOnly             bool
}

// ErrAcquisitionNotFound reports that an exact AcquisitionID is absent.
var ErrAcquisitionNotFound = errors.New("lyrics acquisition ID not found")

// Ledger is the controlled acquisition-ledger handle. It intentionally exposes
// no request-key or latest-acquisition offline replay operation.
type Ledger struct {
	mu    sync.Mutex
	spool *spool
}

// CreateLedger creates and opens a new private mode-0700 ledger root. It fails
// if the root already exists.
func CreateLedger(ctx context.Context, rootPath string) (*Ledger, error) {
	opened, err := createSpool(ctx, rootPath)
	if err != nil {
		return nil, err
	}
	return &Ledger{spool: opened}, nil
}

// OpenLedger opens and fully validates an existing private ledger root.
func OpenLedger(ctx context.Context, rootPath string) (*Ledger, error) {
	opened, err := openExistingSpool(ctx, rootPath)
	if err != nil {
		return nil, err
	}
	return &Ledger{spool: opened}, nil
}

// RootPath returns the exact validated private ledger root used to derive
// adjacent recovery-only forensic storage. It never creates or opens another
// path and fails after the ledger is closed.
func (ledger *Ledger) RootPath() (string, error) {
	if ledger == nil {
		return "", errSpoolClosed
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.spool == nil || ledger.spool.root == nil {
		return "", errSpoolClosed
	}
	if err := ledger.spool.root.verify(); err != nil {
		return "", err
	}
	return ledger.spool.root.path, nil
}

// Close durably closes the ledger. It is idempotent.
func (ledger *Ledger) Close() error {
	if ledger == nil {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.spool == nil {
		return nil
	}
	err := ledger.spool.Close()
	ledger.spool = nil
	return err
}

// Commit durably records one immutable acquisition and returns its exact
// content-addressed identity. Recommitting identical input is idempotent.
func (ledger *Ledger) Commit(ctx context.Context, input RecordInput) (Acquisition, error) {
	if ledger == nil {
		return Acquisition{}, errSpoolClosed
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.spool == nil {
		return Acquisition{}, errSpoolClosed
	}
	committed, err := ledger.spool.commit(ctx, internalRecord(input))
	if err != nil {
		return Acquisition{}, err
	}
	return exportedAcquisition(committed), nil
}

// ReplayByAcquisitionID performs read-only offline replay of exactly one
// content-addressed acquisition. It never selects by request key or recency.
func (ledger *Ledger) ReplayByAcquisitionID(ctx context.Context, acquisitionID AcquisitionID) (Acquisition, error) {
	if ledger == nil {
		return Acquisition{}, errSpoolClosed
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.spool == nil {
		return Acquisition{}, errSpoolClosed
	}
	replayed, err := ledger.spool.replayByAcquisitionID(ctx, string(acquisitionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Acquisition{}, ErrAcquisitionNotFound
	}
	if err != nil {
		return Acquisition{}, err
	}
	return exportedAcquisition(replayed), nil
}

func internalRecord(input RecordInput) validatedProviderResponse {
	observed := make([]observedRevision, len(input.ObservedRevisions))
	for index, revision := range input.ObservedRevisions {
		observed[index] = observedRevision{
			selector: revision.Selector, revisionID: revision.RevisionID,
			timestamp: revision.Timestamp, sha1: revision.SHA1,
		}
	}
	return validatedProviderResponse{
		request: acquisitionRequest{
			provider: input.Request.Provider, canonicalRequestIdentity: input.Request.CanonicalRequestIdentity,
			kind: acquisitionRequestKind(input.Request.Kind), revisionSelector: input.Request.RevisionSelector,
		},
		fetchedAt:   input.FetchedAt,
		rawResponse: append([]byte(nil), input.RawResponse...), rawResponseSHA256: input.RawResponseSHA256,
		evidence: evidenceProjection{
			evidenceID: input.Evidence.EvidenceID, raw: append([]byte(nil), input.Evidence.Raw...),
			rawSHA256: input.Evidence.RawSHA256,
		},
		envelope: evidenceEnvelope{
			raw: append([]byte(nil), input.EvidenceEnvelope...), sha256: input.EvidenceEnvelopeSHA256,
		},
		observedRevisions: observed,
	}
}

func exportedAcquisition(input acquiredProviderResponse) Acquisition {
	observed := make([]ObservedRevision, len(input.observedRevisions))
	for index, revision := range input.observedRevisions {
		observed[index] = ObservedRevision{
			Selector: revision.selector, RevisionID: revision.revisionID,
			Timestamp: revision.timestamp, SHA1: revision.sha1,
		}
	}
	return Acquisition{
		AcquisitionID: AcquisitionID(input.acquisitionID),
		Request: Request{
			Provider: input.request.provider, CanonicalRequestIdentity: input.request.canonicalRequestIdentity,
			Kind: RequestKind(input.request.kind), RevisionSelector: input.request.revisionSelector,
		},
		FetchedAt:         input.fetchedAt,
		RawResponse:       append([]byte(nil), input.rawResponse...),
		RawResponseSHA256: input.rawResponseSHA256,
		Evidence: EvidenceProjection{
			EvidenceID: input.evidence.evidenceID, Raw: append([]byte(nil), input.evidence.raw...),
			RawSHA256: input.evidence.rawSHA256,
		},
		EvidenceEnvelope:       append([]byte(nil), input.envelope.raw...),
		EvidenceEnvelopeSHA256: input.envelope.sha256,
		ObservedRevisions:      observed,
		ReplayOnly:             input.replayOnly,
	}
}
