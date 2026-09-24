package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func validProviderFixedIdentity(provider LyricsSourceProvider, renditionKey string) LyricsSourceFixedIdentity {
	identity := LyricsSourceFixedIdentity{
		Provider:     provider,
		Origin:       LyricsSourceOriginVocaloidFandom,
		PageID:       12,
		RevisionID:   34,
		SHA1:         strings.Repeat("a", 40),
		Title:        "合成試験曲",
		CanonicalURL: "https://vocaloid.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=34",
		FetchedAt:    "2026-07-31T12:34:56.123456789Z",
		Categories:   []string{"Lyrics", "Songs"},
		Section:      "Lyrics/Project SEKAI Version",
		RenditionKey: renditionKey,
		IndexEvidenceRefs: []LyricsSourceIndexEvidenceRef{{
			EvidenceID: "search:vocaloid-fandom:12", SHA256: strings.Repeat("b", 64),
		}},
	}
	switch provider {
	case LyricsSourceProviderMoegirl:
		identity.Origin = LyricsSourceOriginMoegirl
		identity.PageID = 56
		identity.RevisionID = 78
		identity.SHA1 = strings.Repeat("c", 40)
		identity.CanonicalURL = "https://moegirl.icu/index.php?oldid=78&title=%E5%90%88%E6%88%90%E8%AF%95%E9%AA%8C%E6%9B%B2"
		identity.Categories = []string{"歌曲", "游戏音乐"}
		identity.Section = "歌词/游戏版"
		identity.IndexEvidenceRefs = []LyricsSourceIndexEvidenceRef{{
			EvidenceID: "search:moegirl:56", SHA256: strings.Repeat("d", 64),
		}}
	case LyricsSourceProviderSekaipedia:
		identity.Origin = LyricsSourceOriginSekaipedia
		identity.PageID = 268
		identity.RevisionID = 335193
		identity.SHA1 = "b216a827f88c59f5e954a120027832fe9cd74413"
		identity.Title = "List of songs"
		identity.CanonicalURL = "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193"
		identity.RevisionTimestamp = "2026-07-27T16:29:13Z"
		identity.Categories = []string{"Lists", "Project SEKAI"}
		identity.Section = "Song index"
		identity.IndexEvidenceRefs = []LyricsSourceIndexEvidenceRef{{
			EvidenceID: "authority:sekaipedia:list-of-songs:335193",
			SHA256:     "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
		}}
	}
	return identity
}

func validLyricsSourceFull() LyricsSourceFull {
	return LyricsSourceFull{
		Version:              LyricsSourceVersion{Kind: "sekai", Label: "Project SEKAI Version"},
		Performers:           []LyricsSourcePerformer{{PerformerID: "miku", Name: "初音ミク", Color: "#33CCBB"}},
		RubyGeneratorVersion: "wiki-ruby-v1",
		Lines: []LyricsSourceFullLine{
			{
				ID:   "full-000001",
				Text: "初音歌う",
				Segments: []LyricsSourceSegment{{
					Text: "初音歌う", PerformerIDs: []string{"miku"},
					Ruby: []LyricsSourceRubySpan{{Text: "初音", Reading: "はつね"}, {Text: "歌", Reading: "うた"}, {Text: "う"}},
				}},
				TrailingPerformerIDs: []string{"miku"},
			},
			{
				ID: "full-000002", Text: "未来へ", StanzaBreakBefore: true,
				Segments: []LyricsSourceSegment{{
					Text: "未来へ", PerformerIDs: []string{"miku"},
					Ruby: []LyricsSourceRubySpan{{Text: "未来", Reading: "みらい"}, {Text: "へ"}},
				}},
				TrailingPerformerIDs: []string{},
			},
			{
				ID: "full-000003", Text: "進もう",
				Segments: []LyricsSourceSegment{{
					Text: "進もう", PerformerIDs: []string{"miku"},
					Ruby: []LyricsSourceRubySpan{{Text: "進", Reading: "すす"}, {Text: "もう"}},
				}},
				TrailingPerformerIDs: []string{},
			},
		},
	}
}

func validLyricsSourceDocument() LyricsSourceDocument {
	fullRef := LyricsSourceComponentRef{RenditionKey: "full-sekai"}
	gameRef := LyricsSourceComponentRef{RenditionKey: "game-sekai"}
	return LyricsSourceDocument{
		SchemaVersion: LyricsSourceDocumentSchemaVersion,
		ReasonCode:    LyricsSourceVersionReasonTaggedFullAndGame,
		FixedIdentities: []LyricsSourceFixedIdentity{
			validProviderFixedIdentity(LyricsSourceProviderVocaloidFandom, fullRef.RenditionKey),
			validProviderFixedIdentity(LyricsSourceProviderMoegirl, gameRef.RenditionKey),
		},
		Provenance: LyricsSourceComponentProvenance{
			FullText: fullRef, PerformerSegmentation: &fullRef, GameProjection: &gameRef,
			Ruby: &fullRef, VersionEvidence: fullRef,
		},
		Full: validLyricsSourceFull(),
		GameProjection: &LyricsSourceGameProjection{
			LineIDs: []string{"full-000001", "full-000003"},
		},
	}
}

func validVocaloidOnlyLyricsSourceDocument() LyricsSourceDocument {
	document := validLyricsSourceDocument()
	document.ReasonCode = LyricsSourceVersionReasonUntaggedFullOnly
	document.FixedIdentities = document.FixedIdentities[:1]
	document.Full.Version = LyricsSourceVersion{Kind: "vocaloid", Label: "Vocaloid Version"}
	document.Full.Performers = []LyricsSourcePerformer{}
	for index := range document.Full.Lines {
		line := &document.Full.Lines[index]
		line.Segments = []LyricsSourceSegment{{
			Text: line.Text, PerformerIDs: []string{}, Ruby: append([]LyricsSourceRubySpan{}, line.Segments[0].Ruby...),
		}}
		line.TrailingPerformerIDs = []string{}
	}
	document.Provenance.PerformerSegmentation = nil
	document.Provenance.GameProjection = nil
	document.GameProjection = nil
	return document
}

func validAuthoritativeVirtualSingerLyricsSourceDocument() LyricsSourceDocument {
	document := validVocaloidOnlyLyricsSourceDocument()
	fullIdentity := validProviderFixedIdentity(LyricsSourceProviderSekaipedia, "full-vs-sekaipedia")
	fullIdentity.CompositionRenditionKey = "full-vocaloid"
	segmentationIdentity := validProviderFixedIdentity(LyricsSourceProviderVocaloidFandom, "segments-vs-fandom")
	segmentationIdentity.CompositionRenditionKey = "full-vocaloid"
	document.FixedIdentities = []LyricsSourceFixedIdentity{fullIdentity, segmentationIdentity}
	document.Full.Version = LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"}
	document.Full.Performers = []LyricsSourcePerformer{
		{PerformerID: "miku", Name: "初音ミク", Color: "#33CCBB"},
		{PerformerID: "rin", Name: "鏡音リン", Color: "#FFCC11"},
	}
	document.Full.Lines[0].Segments = []LyricsSourceSegment{
		{
			Text: "初音", PerformerIDs: []string{"miku"},
			Ruby: []LyricsSourceRubySpan{{Text: "初音", Reading: "はつね"}},
		},
		{
			Text: "歌う", PerformerIDs: []string{"rin"},
			Ruby: []LyricsSourceRubySpan{{Text: "歌", Reading: "うた"}, {Text: "う"}},
		},
	}
	document.Full.Lines[1].Segments[0].PerformerIDs = []string{"miku"}
	document.Full.Lines[2].Segments[0].PerformerIDs = []string{"rin"}
	fullRef := LyricsSourceComponentRef{RenditionKey: fullIdentity.RenditionKey}
	segmentationRef := LyricsSourceComponentRef{RenditionKey: segmentationIdentity.RenditionKey}
	document.Provenance.FullText = fullRef
	document.Provenance.PerformerSegmentation = &segmentationRef
	document.Provenance.Ruby = &fullRef
	document.Provenance.VersionEvidence = fullRef
	document.PrivateReview = &LyricsSourcePrivateReview{
		PerformerSegmentationEvidence: LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
	}
	return document
}

func validNonVocaloidUnsegmentedLyricsSourceDocument() LyricsSourceDocument {
	document := validLyricsSourceDocument()
	document.Full.Performers = []LyricsSourcePerformer{}
	for index := range document.Full.Lines {
		line := &document.Full.Lines[index]
		line.Segments[0].PerformerIDs = []string{}
		line.TrailingPerformerIDs = []string{}
	}
	document.Provenance.PerformerSegmentation = nil
	return document
}

func splitFirstLyricsSourceLineWithoutPerformerIDs(document *LyricsSourceDocument) {
	document.Full.Lines[0].Segments = []LyricsSourceSegment{
		{
			Text: "初音", PerformerIDs: []string{},
			Ruby: []LyricsSourceRubySpan{{Text: "初音", Reading: "はつね"}},
		},
		{
			Text: "歌う", PerformerIDs: []string{},
			Ruby: []LyricsSourceRubySpan{{Text: "歌", Reading: "うた"}, {Text: "う"}},
		},
	}
}

func TestValidateLyricsSourceDocumentAcceptsProviderAwareProjection(t *testing.T) {
	if LyricsSourceOriginMoegirl != "https://moegirl.icu" {
		t.Fatalf("moegirl origin = %q", LyricsSourceOriginMoegirl)
	}
	document := validLyricsSourceDocument()
	if err := ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("valid provider-aware document: %v", err)
	}
	for _, identity := range document.FixedIdentities {
		if err := ValidateLyricsSourceFixedIdentity(identity); err != nil {
			t.Fatalf("valid %s identity: %v", identity.Provider, err)
		}
	}
}

func TestValidateLyricsSourceFixedIdentityAcceptsSekaipediaAuthority(t *testing.T) {
	if LyricsSourceOriginSekaipedia != "https://www.sekaipedia.org" {
		t.Fatalf("sekaipedia origin = %q", LyricsSourceOriginSekaipedia)
	}
	identity := validProviderFixedIdentity(LyricsSourceProviderSekaipedia, "song-index")
	if !IsValidLyricsSourceProvider(identity.Provider) {
		t.Fatalf("sekaipedia provider %q was not recognized", identity.Provider)
	}
	if err := ValidateLyricsSourceFixedIdentity(identity); err != nil {
		t.Fatalf("valid sekaipedia identity: %v", err)
	}
	body, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLyricsSourceFixedIdentity(body)
	if err != nil {
		t.Fatalf("closed sekaipedia identity: %v", err)
	}
	if decoded.RevisionTimestamp != "2026-07-27T16:29:13Z" {
		t.Fatalf("revision timestamp = %q", decoded.RevisionTimestamp)
	}

	for _, provider := range []LyricsSourceProvider{LyricsSourceProviderVocaloidFandom, LyricsSourceProviderMoegirl} {
		legacy := validProviderFixedIdentity(provider, "legacy-full")
		if legacy.RevisionTimestamp != "" {
			t.Fatalf("legacy %s fixture unexpectedly has a revision timestamp", provider)
		}
		if err := ValidateLyricsSourceFixedIdentity(legacy); err != nil {
			t.Fatalf("legacy %s identity without revisionTimestamp: %v", provider, err)
		}
	}
}

func TestValidateLyricsSourceFixedIdentityAcceptsProjectSekaiFandomForVocaloidFandom(t *testing.T) {
	projectSekai := func() LyricsSourceFixedIdentity {
		identity := validProviderFixedIdentity(LyricsSourceProviderVocaloidFandom, "full-sekai")
		identity.Origin = LyricsSourceOriginProjectSekaiFandom
		identity.RevisionID = 375274
		identity.CanonicalURL = "https://projectsekai.fandom.com/wiki/Synthetic_(Song)?oldid=375274"
		return identity
	}
	if err := ValidateLyricsSourceFixedIdentity(projectSekai()); err != nil {
		t.Fatalf("projectsekai canonical identity: %v", err)
	}
	for name, mutate := range map[string]func(*LyricsSourceFixedIdentity){
		"other fandom origin": func(identity *LyricsSourceFixedIdentity) {
			identity.Origin = "https://evil.fandom.com"
			identity.CanonicalURL = "https://evil.fandom.com/wiki/Synthetic_(Song)?oldid=375274"
		},
		"URL host differs from origin": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://vocaloid.fandom.com/wiki/Synthetic_(Song)?oldid=375274"
		},
		"uppercase host": func(identity *LyricsSourceFixedIdentity) {
			identity.Origin = "https://PROJECTSEKAI.fandom.com"
			identity.CanonicalURL = "https://PROJECTSEKAI.fandom.com/wiki/Synthetic_(Song)?oldid=375274"
		},
		"http origin": func(identity *LyricsSourceFixedIdentity) {
			identity.Origin = "http://projectsekai.fandom.com"
			identity.CanonicalURL = "http://projectsekai.fandom.com/wiki/Synthetic_(Song)?oldid=375274"
		},
		"index.php revision URL": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://projectsekai.fandom.com/index.php?oldid=375274&title=Synthetic_(Song)"
		},
		"oldid differs from revision": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://projectsekai.fandom.com/wiki/Synthetic_(Song)?oldid=375275"
		},
		"sekaipedia provider": func(identity *LyricsSourceFixedIdentity) {
			identity.Provider = LyricsSourceProviderSekaipedia
			identity.RevisionTimestamp = "2026-07-27T16:29:13Z"
		},
		"moegirl provider": func(identity *LyricsSourceFixedIdentity) {
			identity.Provider = LyricsSourceProviderMoegirl
		},
	} {
		t.Run(name, func(t *testing.T) {
			identity := projectSekai()
			mutate(&identity)
			if err := ValidateLyricsSourceFixedIdentity(identity); err == nil {
				t.Fatalf("identity with origin %q and URL %q was accepted", identity.Origin, identity.CanonicalURL)
			}
		})
	}
}

func TestValidateLyricsSourceFixedIdentityRequiresSekaipediaRevisionTimestampAndWikiURL(t *testing.T) {
	for name, mutate := range map[string]func(*LyricsSourceFixedIdentity){
		"missing revision timestamp": func(identity *LyricsSourceFixedIdentity) {
			identity.RevisionTimestamp = ""
		},
		"malformed revision timestamp": func(identity *LyricsSourceFixedIdentity) {
			identity.RevisionTimestamp = "not-a-timestamp"
		},
		"noncanonical revision timestamp": func(identity *LyricsSourceFixedIdentity) {
			identity.RevisionTimestamp = "2026-07-27T16:29:13.000Z"
		},
		"revision timestamp after fetch": func(identity *LyricsSourceFixedIdentity) {
			identity.RevisionTimestamp = "2026-08-01T00:00:00Z"
			identity.FetchedAt = "2026-07-31T23:59:59Z"
		},
		"index revision URL": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://www.sekaipedia.org/index.php?oldid=335193&title=List_of_songs"
		},
		"bare wiki URL": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://www.sekaipedia.org/wiki/List_of_songs"
		},
	} {
		t.Run(name, func(t *testing.T) {
			identity := validProviderFixedIdentity(LyricsSourceProviderSekaipedia, "song-index")
			mutate(&identity)
			if err := ValidateLyricsSourceFixedIdentity(identity); err == nil {
				t.Fatal("invalid sekaipedia fixed identity was accepted")
			}
		})
	}
}

func TestValidateLyricsSourceDocumentKeepsFullOnlyCompatibility(t *testing.T) {
	document := validLyricsSourceDocument()
	document.FixedIdentities = document.FixedIdentities[:1]
	document.ReasonCode = LyricsSourceVersionReasonUntaggedFullOnly
	document.GameProjection = nil
	document.Provenance.GameProjection = nil
	if err := ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("Full-only document: %v", err)
	}
}

func TestValidateLyricsSourceDocumentVersionReasonMatrix(t *testing.T) {
	codes := []struct {
		code LyricsSourceVersionReasonCode
		want string
	}{
		{LyricsSourceVersionReasonTaggedFullAndGame, "tagged_full_and_game"},
		{LyricsSourceVersionReasonTaggedGameOnly, "tagged_game_only"},
		{LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid, "tagged_game_only_full_from_vocaloid"},
		{LyricsSourceVersionReasonUntaggedUncutIdentity, "untagged_uncut_identity"},
		{LyricsSourceVersionReasonUntaggedGameSubset, "untagged_game_subset"},
		{LyricsSourceVersionReasonUntaggedFullOnly, "untagged_full_only"},
		{LyricsSourceVersionReasonVersionConflict, "version_conflict"},
	}
	for _, code := range codes {
		if string(code.code) != code.want || !IsValidLyricsSourceVersionReasonCode(code.code) {
			t.Fatalf("version reason code = %q want=%q valid=%t", code.code, code.want, IsValidLyricsSourceVersionReasonCode(code.code))
		}
		wantCandidateReason := code.code != LyricsSourceVersionReasonVersionConflict
		if IsValidLyricsSourceCandidateVersionReasonCode(code.code) != wantCandidateReason {
			t.Fatalf("candidate version reason %q valid=%t want=%t", code.code, IsValidLyricsSourceCandidateVersionReasonCode(code.code), wantCandidateReason)
		}
	}
	if IsValidLyricsSourceVersionReasonCode("other") || IsValidLyricsSourceCandidateVersionReasonCode("other") {
		t.Fatal("unknown version reason code was recognized")
	}

	for _, test := range []struct {
		name       string
		reasonCode LyricsSourceVersionReasonCode
		projection string
		wantErr    bool
	}{
		{name: "tagged full and game subset", reasonCode: LyricsSourceVersionReasonTaggedFullAndGame, projection: "subset"},
		{name: "tagged full and game missing projection", reasonCode: LyricsSourceVersionReasonTaggedFullAndGame, projection: "none"},
		{name: "untagged uncut identity", reasonCode: LyricsSourceVersionReasonUntaggedUncutIdentity, projection: "identity"},
		{name: "untagged uncut subset", reasonCode: LyricsSourceVersionReasonUntaggedUncutIdentity, projection: "subset", wantErr: true},
		{name: "untagged uncut missing projection", reasonCode: LyricsSourceVersionReasonUntaggedUncutIdentity, projection: "none", wantErr: true},
		{name: "tagged game only no projection", reasonCode: LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid, projection: "none"},
		{name: "tagged game only with projection", reasonCode: LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid, projection: "subset", wantErr: true},
		{name: "untagged game subset no projection", reasonCode: LyricsSourceVersionReasonUntaggedGameSubset, projection: "none"},
		{name: "untagged game subset with projection", reasonCode: LyricsSourceVersionReasonUntaggedGameSubset, projection: "subset", wantErr: true},
		{name: "untagged full only no projection", reasonCode: LyricsSourceVersionReasonUntaggedFullOnly, projection: "none"},
		{name: "untagged full only with projection", reasonCode: LyricsSourceVersionReasonUntaggedFullOnly, projection: "subset", wantErr: true},
		{name: "version conflict", reasonCode: LyricsSourceVersionReasonVersionConflict, projection: "none", wantErr: true},
		{name: "unknown reason", reasonCode: "other", projection: "none", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := validLyricsSourceDocument()
			document.ReasonCode = test.reasonCode
			if test.reasonCode == LyricsSourceVersionReasonTaggedGameOnly ||
				test.reasonCode == LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid {
				game := document.Full
				for index := range game.Lines {
					game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
				}
				document.Full = LyricsSourceFull{}
				document.Game = &game
				document.FixedIdentities = document.FixedIdentities[1:]
				gameRef := LyricsSourceComponentRef{RenditionKey: document.FixedIdentities[0].RenditionKey}
				document.Provenance = LyricsSourceComponentProvenance{
					GameText: &gameRef, PerformerSegmentation: &gameRef, Ruby: &gameRef, VersionEvidence: gameRef,
				}
			}
			switch test.projection {
			case "none":
				document.GameProjection = nil
				document.Provenance.GameProjection = nil
			case "identity":
				document.GameProjection.LineIDs = make([]string, len(document.Full.Lines))
				for index, line := range document.Full.Lines {
					document.GameProjection.LineIDs[index] = line.ID
				}
			case "subset":
			default:
				t.Fatalf("unsupported projection fixture %q", test.projection)
			}
			err := ValidateLyricsSourceDocument(document)
			if test.wantErr && err == nil {
				t.Fatal("invalid version reason/projection combination was accepted")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("valid version reason/projection combination: %v", err)
			}
		})
	}
}

func TestDecodeLyricsSourceDocumentEnforcesVocaloidOnlyFullShape(t *testing.T) {
	valid := validVocaloidOnlyLyricsSourceDocument()
	body, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLyricsSourceDocument(body); err != nil {
		t.Fatalf("valid vocaloid-only document: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*LyricsSourceDocument)
	}{
		{
			name: "performer metadata",
			mutate: func(document *LyricsSourceDocument) {
				document.Full.Performers = []LyricsSourcePerformer{{PerformerID: "miku", Name: "初音ミク"}}
			},
		},
		{
			name: "color metadata",
			mutate: func(document *LyricsSourceDocument) {
				document.Full.Performers = []LyricsSourcePerformer{{PerformerID: "miku", Name: "初音ミク", Color: "#33CCBB"}}
			},
		},
		{
			name: "multiple segments",
			mutate: func(document *LyricsSourceDocument) {
				document.Full.Lines[0].Segments = []LyricsSourceSegment{
					{Text: "初音", PerformerIDs: []string{}, Ruby: []LyricsSourceRubySpan{{Text: "初音", Reading: "はつね"}}},
					{Text: "歌う", PerformerIDs: []string{}, Ruby: []LyricsSourceRubySpan{{Text: "歌", Reading: "うた"}, {Text: "う"}}},
				}
			},
		},
		{
			name: "nil segment performer IDs",
			mutate: func(document *LyricsSourceDocument) {
				document.Full.Lines[0].Segments[0].PerformerIDs = nil
			},
		},
		{
			name: "segment performer ID",
			mutate: func(document *LyricsSourceDocument) {
				document.Full.Lines[0].Segments[0].PerformerIDs = []string{"miku"}
			},
		},
		{
			name: "nil trailing performer IDs",
			mutate: func(document *LyricsSourceDocument) {
				document.Full.Lines[0].TrailingPerformerIDs = nil
			},
		},
		{
			name: "trailing performer ID",
			mutate: func(document *LyricsSourceDocument) {
				document.Full.Lines[0].TrailingPerformerIDs = []string{"miku"}
			},
		},
		{
			name: "raw performer annotation",
			mutate: func(document *LyricsSourceDocument) {
				line := &document.Full.Lines[0]
				line.Text = "@1初音歌う"
				line.Segments[0].Text = line.Text
				line.Segments[0].Ruby = []LyricsSourceRubySpan{{Text: line.Text}}
			},
		},
		{
			name: "raw color annotation",
			mutate: func(document *LyricsSourceDocument) {
				line := &document.Full.Lines[0]
				line.Text = "#33CCBB初音歌う"
				line.Segments[0].Text = line.Text
				line.Segments[0].Ruby = []LyricsSourceRubySpan{{Text: line.Text}}
			},
		},
		{
			name: "performer segmentation provenance",
			mutate: func(document *LyricsSourceDocument) {
				reference := document.Provenance.FullText
				document.Provenance.PerformerSegmentation = &reference
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := validVocaloidOnlyLyricsSourceDocument()
			test.mutate(&document)
			body, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeLyricsSourceDocument(body); err == nil {
				t.Fatal("invalid vocaloid-only document was accepted")
			}
		})
	}
}
