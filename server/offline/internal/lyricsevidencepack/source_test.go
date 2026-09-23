package lyricsevidencepack

import (
	"context"
	"strings"
	"testing"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

func TestResolveExactAcquisitionRejectsReferenceDrift(t *testing.T) {
	item := testEvidence(t, 1, 0)
	ref := evidenceRef(item)
	base := testAcquisition(item)
	for name, mutate := range map[string]func(*lyricsacquisition.Acquisition){
		"not replay": func(acquired *lyricsacquisition.Acquisition) {
			acquired.ReplayOnly = false
		},
		"acquisition ID": func(acquired *lyricsacquisition.Acquisition) {
			acquired.AcquisitionID = lyricsacquisition.AcquisitionID(strings.Repeat("f", 64))
		},
		"provider": func(acquired *lyricsacquisition.Acquisition) {
			acquired.Request.Provider = string(model.LyricsSourceProviderMoegirl)
		},
		"evidence ID": func(acquired *lyricsacquisition.Acquisition) {
			acquired.Evidence.EvidenceID = "revision:sekaipedia:1:1:" + strings.Repeat("a", 64)
		},
		"evidence digest": func(acquired *lyricsacquisition.Acquisition) {
			acquired.Evidence.RawSHA256 = strings.Repeat("a", 64)
		},
		"envelope digest": func(acquired *lyricsacquisition.Acquisition) {
			acquired.EvidenceEnvelopeSHA256 = strings.Repeat("a", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			acquired := base
			acquired.Evidence.Raw = append([]byte(nil), base.Evidence.Raw...)
			acquired.EvidenceEnvelope = append([]byte(nil), base.EvidenceEnvelope...)
			mutate(&acquired)
			if _, err := resolveExactAcquisition(context.Background(), fixedExactSource{acquired: acquired}, ref); err == nil {
				t.Fatalf("exact acquisition %s drift was accepted", name)
			}
		})
	}
}

func TestResolveExactAcquisitionReturnsCanonicalEnvelope(t *testing.T) {
	item := testEvidence(t, 1, 0)
	ref := evidenceRef(item)
	resolved, err := resolveExactAcquisition(context.Background(), sliceExactSource{items: []lyricssource.IndexEvidence{item}}, ref)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EvidenceID != ref.EvidenceID || resolved.Provider != ref.Provider || resolved.SHA256 != ref.SHA256 {
		t.Fatalf("resolved exact envelope=%+v ref=%+v", resolved, ref)
	}
}
