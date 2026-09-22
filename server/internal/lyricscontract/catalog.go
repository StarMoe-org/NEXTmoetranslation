// Package lyricscontract holds the lyrics source contract shared by the
// runtime store and the offline lyrics tooling.
package lyricscontract

import (
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

type CatalogIdentity struct {
	MusicID            int
	JapaneseTitle      string
	ProducerMetadata   string
	Lyricist           string
	Composer           string
	Arranger           string
	Vocals             []model.CatalogVocalSignal
	CatalogFingerprint string
}

func (identity CatalogIdentity) SourceIdentity() lyricssource.MusicIdentity {
	return lyricssource.MusicIdentity{
		MusicID: identity.MusicID, JapaneseTitle: identity.JapaneseTitle,
		ProducerMetadata: identity.ProducerMetadata, Lyricist: identity.Lyricist,
		Composer: identity.Composer, Arranger: identity.Arranger,
		PerformerSegmentationPolicy: lyricssource.PerformerSegmentationPolicyFromCatalogVocals(identity.Vocals),
		Instrumental:                model.CatalogVocalSignalsAreInstrumental(identity.Vocals),
	}
}
