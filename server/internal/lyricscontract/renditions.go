package lyricscontract

type RenditionPeerTranslation struct {
	Side         string   `json:"side"`
	Locale       string   `json:"locale"`
	Translations []string `json:"translations"`
}

type RenditionTranslation struct {
	RenditionKey       string                     `json:"renditionKey"`
	Translations       []string                   `json:"translations,omitempty"`
	PeerTranslations   []RenditionPeerTranslation `json:"peerTranslations,omitempty"`
	TranslationCredit  string                     `json:"translationCredit,omitempty"`
	ProofreadingCredit string                     `json:"proofreadingCredit,omitempty"`
}
