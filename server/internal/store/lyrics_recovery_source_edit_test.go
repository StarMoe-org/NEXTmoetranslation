package store

import (
	"errors"
	"strings"
	"testing"
)

func TestRecoveryImportedV3EditorRefusesSourceEditAndStaysReadable(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	current, err := fixture.store.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("recovery v3 editor GET: %v", err)
	}

	translationOnly := cloneLyricsRenditionEditorDocument(t, current)
	translationOnly.Renditions[0].Full.Lines[0].Chinese = "恢复后的简中"
	translationOnly.Renditions[0].TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: "恢复译者"}
	if translationOnly.Renditions[0].Relation.Kind == "exact_projection" {
		for index, lineID := range translationOnly.Renditions[0].Relation.LineIDs {
			if lineID == translationOnly.Renditions[0].Full.Lines[0].ID {
				translationOnly.Renditions[0].Game.Lines[index].Chinese = "恢复后的简中"
			}
		}
	}
	translated, changed, err := fixture.store.SaveLyricsRenditionMutation(translationOnly, "recovery-editor")
	if err != nil || !changed || translated.Renditions[0].Full.Lines[0].Chinese != "恢复后的简中" {
		t.Fatalf("recovery translation save revision=%d changed=%v err=%v", translated.Revision, changed, err)
	}

	requested := cloneLyricsRenditionEditorDocument(t, translated)
	line := &requested.Renditions[0].Full.Lines[1]
	line.StanzaBreakBefore = !line.StanzaBreakBefore
	saved, changed, err := fixture.store.SaveLyricsRenditionMutation(requested, "recovery-editor")
	var contractErr *LyricsRenditionContractError
	if err == nil || !errors.As(err, &contractErr) || contractErr.Code != "source_drift" || changed {
		t.Fatalf("recovery source edit revision=%d changed=%v err=%v", saved.Revision, changed, err)
	}
	if len(contractErr.Details) != 1 || !strings.Contains(contractErr.Details[0], "recovery-imported") {
		t.Fatalf("recovery source edit details=%v", contractErr.Details)
	}

	reloaded, err := fixture.store.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatalf("recovery document is unreadable after a refused source edit: %v", err)
	}
	if reloaded.Revision != translated.Revision ||
		reloaded.Renditions[0].Full.Lines[0].Chinese != "恢复后的简中" ||
		reloaded.Renditions[0].Full.Lines[1].StanzaBreakBefore != translated.Renditions[0].Full.Lines[1].StanzaBreakBefore {
		t.Fatalf("refused source edit changed the stored document=%+v", reloaded.Renditions[0].Full.Lines[1])
	}
}
