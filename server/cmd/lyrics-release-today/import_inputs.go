package main

import (
	"errors"
	"fmt"
	"reflect"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsrecovery"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

type importEvidenceKey struct {
	Provider   model.LyricsSourceProvider
	EvidenceID string
	SHA256     string
}

func validateImportInputsAgainstFreshRoot(
	manifest lyricsstaging.Manifest,
	receipt lyricsstaging.PrivateEvidenceReceipt,
	root lyricsrootmanifest.Manifest,
	results map[int]lyricsrecovery.SongResult,
) error {
	if manifest.Preflight.CatalogCount != releaseCatalogTargetCount ||
		manifest.Preflight.UniqueCompleteCount != releaseCatalogTargetCount ||
		len(manifest.CatalogReference) != releaseCatalogTargetCount || len(manifest.Items) != releaseCatalogTargetCount {
		return errors.New("release import manifest is not the exact complete 698 staging batch")
	}
	if len(root.Songs) != len(manifest.Items) || len(results) != len(root.Songs) {
		return errors.New("release import manifest does not have one item for every fresh root song")
	}

	rootUnion, err := orderedEvidenceUnion(root)
	if err != nil {
		return err
	}
	rootEvidence := make(map[string]lyricscontract.EvidenceRef, len(rootUnion))
	for _, reference := range rootUnion {
		rootEvidence[reference.EvidenceID] = reference
	}
	requiredEvidence := make(map[string]importEvidenceKey, len(rootUnion))
	addRequiredEvidence := func(provider model.LyricsSourceProvider, references []model.LyricsSourceIndexEvidenceRef) error {
		for _, reference := range references {
			rootReference, found := rootEvidence[reference.EvidenceID]
			if !found || rootReference.Provider != provider || rootReference.SHA256 != reference.SHA256 {
				return errors.New("release import inputs reference evidence outside the exact fresh root union")
			}
			key := importEvidenceKey{Provider: provider, EvidenceID: reference.EvidenceID, SHA256: reference.SHA256}
			if prior, duplicate := requiredEvidence[reference.EvidenceID]; duplicate && prior != key {
				return errors.New("release import inputs contain a conflicting evidence identity")
			}
			requiredEvidence[reference.EvidenceID] = key
		}
		return nil
	}

	for index, draft := range manifest.Items {
		rootSong := root.Songs[index]
		result, found := results[draft.MusicID]
		if !found || draft.MusicID != rootSong.MusicID || result.MusicID != draft.MusicID || result.Full == nil {
			return fmt.Errorf("staged music %d does not exactly follow the fresh root order", draft.MusicID)
		}
		if draft.Document.ReasonCode != result.ReasonCode ||
			!reflect.DeepEqual(draft.Document.Full, *result.Full) ||
			!reflect.DeepEqual(draft.Document.GameProjection, result.GameProjection) ||
			!reflect.DeepEqual(draft.Translations, result.Translations) {
			return fmt.Errorf("staged music %d drifted from authoritative Full/Game recovery output", draft.MusicID)
		}
		if err := lyricscontract.ValidatePersistedPerformerMetadata(draft.Document.Full); err != nil {
			return fmt.Errorf("staged music %d contains unsafe persisted performer metadata", draft.MusicID)
		}
		for _, identity := range draft.Document.FixedIdentities {
			if err := addRequiredEvidence(identity.Provider, identity.IndexEvidenceRefs); err != nil {
				return fmt.Errorf("staged music %d: %w", draft.MusicID, err)
			}
		}
		for _, artifact := range draft.Artifacts {
			if err := addRequiredEvidence(artifact.Identity.Provider, artifact.Identity.IndexEvidenceRefs); err != nil {
				return fmt.Errorf("staged music %d: %w", draft.MusicID, err)
			}
		}
	}

	if len(requiredEvidence) != len(rootUnion) {
		return errors.New("release import inputs do not re-prove the complete fresh root evidence union")
	}
	if len(receipt.IndexEvidence) != len(rootUnion) {
		return errors.New("release import evidence receipt does not equal the complete fresh root evidence union")
	}
	receiptEvidence := make(map[string]struct{}, len(receipt.IndexEvidence))
	for _, evidence := range receipt.IndexEvidence {
		required, found := requiredEvidence[evidence.EvidenceID]
		if _, duplicate := receiptEvidence[evidence.EvidenceID]; duplicate || !found ||
			required.Provider != evidence.Provider || required.SHA256 != evidence.SHA256 ||
			evidence.RawSHA256 != evidence.SHA256 {
			return errors.New("release import evidence receipt contains missing, duplicate, orphan, or mismatched evidence")
		}
		receiptEvidence[evidence.EvidenceID] = struct{}{}
	}
	if len(receiptEvidence) != len(rootUnion) {
		return errors.New("release import evidence receipt does not re-prove every fresh root evidence identity")
	}
	return nil
}
