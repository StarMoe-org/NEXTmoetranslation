package lyricsevidencepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"moesekai/server/internal/lyricsacquisition"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

// ExactAcquisitionSource replays only an explicitly supplied content-addressed
// acquisition ID. *lyricsacquisition.Ledger implements this interface.
type ExactAcquisitionSource interface {
	ReplayByAcquisitionID(context.Context, lyricsacquisition.AcquisitionID) (lyricsacquisition.Acquisition, error)
}

// EvidenceRefFromAcquisition derives the sole compact pack reference from one
// exact offline replay and validates its canonical evidence envelope.
func EvidenceRefFromAcquisition(acquired lyricsacquisition.Acquisition) (lyricscontract.EvidenceRef, error) {
	ref, _, err := exactEvidenceFromAcquisition(acquired)
	return ref, err
}

func exactEvidenceFromAcquisition(
	acquired lyricsacquisition.Acquisition,
) (lyricscontract.EvidenceRef, lyricssource.IndexEvidence, error) {
	ref := lyricscontract.EvidenceRef{
		Provider:       model.LyricsSourceProvider(acquired.Request.Provider),
		AcquisitionID:  string(acquired.AcquisitionID),
		EvidenceID:     acquired.Evidence.EvidenceID,
		SHA256:         acquired.Evidence.RawSHA256,
		EnvelopeSHA256: acquired.EvidenceEnvelopeSHA256,
	}
	if !acquired.ReplayOnly {
		return lyricscontract.EvidenceRef{}, lyricssource.IndexEvidence{}, errors.New("evidence pack requires an exact offline acquisition replay")
	}
	if err := lyricscontract.ValidateEvidenceRef(ref); err != nil || sha256Hex(acquired.Evidence.Raw) != ref.SHA256 ||
		sha256Hex(acquired.EvidenceEnvelope) != ref.EnvelopeSHA256 {
		return lyricscontract.EvidenceRef{}, lyricssource.IndexEvidence{}, errors.New("exact acquisition does not match its compact evidence reference")
	}
	envelope, err := DecodeCanonicalEnvelope(acquired.EvidenceEnvelope)
	if err != nil {
		return lyricscontract.EvidenceRef{}, lyricssource.IndexEvidence{}, err
	}
	if envelope.Provider != ref.Provider || envelope.EvidenceID != ref.EvidenceID || envelope.SHA256 != ref.SHA256 ||
		envelope.RawSHA256 != ref.SHA256 || !bytes.Equal(envelope.Raw, acquired.Evidence.Raw) {
		return lyricscontract.EvidenceRef{}, lyricssource.IndexEvidence{}, errors.New("exact acquisition does not bind its canonical evidence envelope")
	}
	return ref, envelope, nil
}

func resolveExactAcquisition(
	ctx context.Context,
	source ExactAcquisitionSource,
	ref lyricscontract.EvidenceRef,
) (lyricssource.IndexEvidence, error) {
	if ctx == nil || source == nil {
		return lyricssource.IndexEvidence{}, errors.New("context and exact acquisition source are required")
	}
	acquired, err := source.ReplayByAcquisitionID(ctx, lyricsacquisition.AcquisitionID(ref.AcquisitionID))
	if err != nil {
		return lyricssource.IndexEvidence{}, fmt.Errorf("replay exact selected acquisition: %w", err)
	}
	actualRef, envelope, err := exactEvidenceFromAcquisition(acquired)
	if err != nil {
		return lyricssource.IndexEvidence{}, err
	}
	if actualRef != ref {
		return lyricssource.IndexEvidence{}, errors.New("exact selected acquisition does not match its compact reference")
	}
	return envelope, nil
}
