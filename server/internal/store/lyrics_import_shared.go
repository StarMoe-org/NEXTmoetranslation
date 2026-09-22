package store

import (
	"errors"
	"fmt"

	"moesekai/server/internal/lyricscompose"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

// Persisted lyrics-source helpers shared by the serving store (backup, restore,
// source import) and the offline importers under server/cmd.

var ErrLyricsRecoveryImportConflict = errors.New("lyrics recovery import conflicts with existing private lyrics state")

func recoveryNullablePositiveInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

// MapDeclaredLyricsSourcePerformerIDs projects source performer labels onto the
// closed catalog performer namespace.
func MapDeclaredLyricsSourcePerformerIDs(
	sourceIDs []string,
	aliases map[string]int,
	declaredUnmapped map[string]bool,
) ([]int, error) {
	seen := map[int]bool{}
	result := make([]int, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		normalized := normalizeLyricsSourcePerformerAlias(sourceID)
		if normalized == "" || normalized == "chorus" || normalized == "ensemble" ||
			normalized == "all" || normalized == "everyone" {
			continue
		}
		performerID := aliases[normalized]
		if performerID <= 0 {
			// Fixed Wiki revisions can name human or external singers that do
			// not exist in the runtime's closed catalog_performers table. The
			// staging manifest preserves those source labels for auditability;
			// projection may omit them only when the same normalized label is
			// explicitly declared in that immutable source legend. The caller
			// then uses the selected catalog-vocal fallback. Undeclared labels
			// still fail closed as possible manifest corruption.
			if declaredUnmapped[normalized] {
				continue
			}
			return nil, ErrLyricsSourcePerformerMapping
		}
		if !seen[performerID] {
			seen[performerID] = true
			result = append(result, performerID)
		}
	}
	return result, nil
}

// ValidatePersistedLyricsSourceDocument is the contract every source document
// must satisfy before it is stored or restored.
func ValidatePersistedLyricsSourceDocument(document model.LyricsSourceDocument) error {
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		return err
	}
	if err := validateStoreV3DocumentGraph(document); err != nil {
		return err
	}
	validateFull := func(full model.LyricsSourceFull) error {
		if err := lyricscompose.ValidatePersistedPerformerMetadata(full); err != nil {
			return errors.New("unsafe persisted lyrics performer metadata")
		}
		canonicalRubyVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(full.RubyGeneratorVersion)
		if err != nil || canonicalRubyVersion != full.RubyGeneratorVersion {
			return errors.New("unsafe persisted lyrics ruby generator metadata")
		}
		return nil
	}
	if document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		for _, rendition := range document.Renditions {
			for _, full := range []*model.LyricsSourceFull{rendition.Full, rendition.Game} {
				if full != nil {
					if err := validateFull(*full); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := validateFull(document.Full); err != nil {
		return err
	}
	for _, alternate := range document.AlternateVocals {
		for _, full := range []*model.LyricsSourceFull{alternate.Full, alternate.Game} {
			if full != nil {
				if err := validateFull(*full); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// LyricsSourceComponentRefs maps every provenance component of a document to
// the rendition key that contributed it.
func LyricsSourceComponentRefs(document model.LyricsSourceDocument) map[string]string {
	if document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		refs, err := storeV3DocumentComponentRefs(document)
		if err != nil {
			return map[string]string{}
		}
		return refs
	}
	refs := map[string]string{
		"full_text":        document.Provenance.FullText.RenditionKey,
		"version_evidence": document.Provenance.VersionEvidence.RenditionKey,
	}
	if document.Provenance.PerformerSegmentation != nil {
		refs["performer_segmentation"] = document.Provenance.PerformerSegmentation.RenditionKey
	}
	if document.Provenance.GameProjection != nil {
		refs["game_projection"] = document.Provenance.GameProjection.RenditionKey
	}
	if document.Provenance.Ruby != nil {
		refs["ruby"] = document.Provenance.Ruby.RenditionKey
	}
	for index, alternate := range document.AlternateVocals {
		prefix := fmt.Sprintf("alternate_vocal_%06d_", index+1)
		refs[prefix+"version_evidence"] = alternate.Provenance.VersionEvidence.RenditionKey
		if alternate.Provenance.FullText != nil {
			refs[prefix+"full_text"] = alternate.Provenance.FullText.RenditionKey
		}
		if alternate.Provenance.GameText != nil {
			refs[prefix+"game_text"] = alternate.Provenance.GameText.RenditionKey
		}
		if alternate.Provenance.GameProjection != nil {
			refs[prefix+"game_projection"] = alternate.Provenance.GameProjection.RenditionKey
		}
	}
	return refs
}
