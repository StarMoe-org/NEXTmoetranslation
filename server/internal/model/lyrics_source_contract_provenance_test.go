package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLyricsSourceLegacyFullBridgePreservesRubySpans(t *testing.T) {
	legacy := []LyricsSourceExtractedLine{{
		Japanese: "初音歌う", StanzaBreakBefore: true,
		Segments: []LyricsSourceSegment{{
			Text: "初音歌う", PerformerIDs: []string{"miku"},
			Ruby: []LyricsSourceRubySpan{{Text: "初音", Reading: "はつね"}, {Text: "歌", Reading: "うた"}, {Text: "う"}},
		}},
		TrailingPerformerIDs: []string{"miku"},
	}}
	beforeDigest := LyricsSourceExtractedLinesSHA256(legacy)
	full := NewLyricsSourceFullFromLegacy(
		LyricsSourceVersion{Kind: "sekai", Label: "Project SEKAI Version"},
		[]LyricsSourcePerformer{{PerformerID: "miku", Name: "初音ミク", Color: "#33CCBB"}},
		"wiki-ruby-v1",
		legacy,
	)
	if full.Lines[0].ID != "full-000001" {
		t.Fatalf("legacy line ID = %q", full.Lines[0].ID)
	}
	if err := ValidateLyricsSourceFull(full); err != nil {
		t.Fatalf("bridged Full: %v", err)
	}
	roundTrip := full.LegacyExtractedLines()
	if !reflect.DeepEqual(roundTrip, legacy) {
		t.Fatalf("legacy bridge changed extraction\n got: %#v\nwant: %#v", roundTrip, legacy)
	}
	if digest := LyricsSourceExtractedLinesSHA256(roundTrip); digest != beforeDigest {
		t.Fatalf("legacy extraction digest changed: %s != %s", digest, beforeDigest)
	}

	full.Lines[0].Segments[0].Ruby[0].Reading = "changed"
	if legacy[0].Segments[0].Ruby[0].Reading != "はつね" {
		t.Fatal("legacy bridge aliased ruby spans")
	}
}

func TestValidateLyricsSourceFixedIdentityFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*LyricsSourceFixedIdentity){
		"unknown provider": func(identity *LyricsSourceFixedIdentity) {
			identity.Provider = "other"
		},
		"provider origin mismatch": func(identity *LyricsSourceFixedIdentity) {
			identity.Origin = LyricsSourceOriginMoegirl
		},
		"noncanonical URL origin": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://evil.example/wiki/Song?oldid=34"
		},
		"revision mismatch": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://vocaloid.fandom.com/wiki/Song?oldid=35"
		},
		"noncanonical query": func(identity *LyricsSourceFixedIdentity) {
			identity.CanonicalURL = "https://vocaloid.fandom.com/wiki/Song?oldid=34&title=Song"
		},
		"missing fetchedAt": func(identity *LyricsSourceFixedIdentity) {
			identity.FetchedAt = ""
		},
		"malformed fetchedAt": func(identity *LyricsSourceFixedIdentity) {
			identity.FetchedAt = "not-a-timestamp"
		},
		"noncanonical fetchedAt": func(identity *LyricsSourceFixedIdentity) {
			identity.FetchedAt = "2026-07-31T12:34:56.000Z"
		},
		"missing section": func(identity *LyricsSourceFixedIdentity) {
			identity.Section = ""
		},
		"malformed rendition key": func(identity *LyricsSourceFixedIdentity) {
			identity.RenditionKey = "Full SEKAI"
		},
		"missing categories": func(identity *LyricsSourceFixedIdentity) {
			identity.Categories = nil
		},
		"duplicate categories": func(identity *LyricsSourceFixedIdentity) {
			identity.Categories = append(identity.Categories, identity.Categories[0])
		},
		"missing index evidence": func(identity *LyricsSourceFixedIdentity) {
			identity.IndexEvidenceRefs = nil
		},
		"duplicate index evidence": func(identity *LyricsSourceFixedIdentity) {
			identity.IndexEvidenceRefs = append(identity.IndexEvidenceRefs, identity.IndexEvidenceRefs[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			identity := validProviderFixedIdentity(LyricsSourceProviderVocaloidFandom, "full-sekai")
			mutate(&identity)
			if err := ValidateLyricsSourceFixedIdentity(identity); err == nil {
				t.Fatal("invalid fixed identity was accepted")
			}
		})
	}
}

func TestDecodeLyricsSourceFixedIdentityRequiresCanonicalFetchedAt(t *testing.T) {
	identity := validProviderFixedIdentity(LyricsSourceProviderVocaloidFandom, "full-sekai")
	body, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLyricsSourceFixedIdentity(body); err != nil {
		t.Fatalf("valid fixed identity: %v", err)
	}

	canonicalField := `"fetchedAt":"2026-07-31T12:34:56.123456789Z",`
	for name, replacement := range map[string]string{
		"missing":      "",
		"malformed":    `"fetchedAt":"not-a-timestamp",`,
		"noncanonical": `"fetchedAt":"2026-07-31T12:34:56.000Z",`,
	} {
		t.Run(name, func(t *testing.T) {
			mutated := []byte(strings.Replace(string(body), canonicalField, replacement, 1))
			if _, err := DecodeLyricsSourceFixedIdentity(mutated); err == nil {
				t.Fatal("invalid fetchedAt was accepted by the closed decoder")
			}
		})
	}
}

func TestValidateLyricsSourceDocumentRequiresExactComponentProvenance(t *testing.T) {
	for name, mutate := range map[string]func(*LyricsSourceDocument){
		"unknown Full source": func(document *LyricsSourceDocument) {
			document.Provenance.FullText.RenditionKey = "missing"
		},
		"duplicate rendition key": func(document *LyricsSourceDocument) {
			document.FixedIdentities[1].RenditionKey = document.FixedIdentities[0].RenditionKey
		},
		"missing performer provenance": func(document *LyricsSourceDocument) {
			document.Provenance.PerformerSegmentation = nil
		},
		"missing ruby provenance": func(document *LyricsSourceDocument) {
			document.Provenance.Ruby = nil
		},
		"missing projection provenance": func(document *LyricsSourceDocument) {
			document.Provenance.GameProjection = nil
		},
		"extraneous projection provenance": func(document *LyricsSourceDocument) {
			document.GameProjection = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			document := validLyricsSourceDocument()
			mutate(&document)
			if err := ValidateLyricsSourceDocument(document); err == nil {
				t.Fatal("invalid component provenance was accepted")
			}
		})
	}
}

func TestValidateLyricsSourceGameProjectionRequiresOrderedFullLineIDs(t *testing.T) {
	for name, projection := range map[string]LyricsSourceGameProjection{
		"empty lines":    {LineIDs: []string{}},
		"unknown line":   {LineIDs: []string{"missing"}},
		"duplicate line": {LineIDs: []string{"full-000001", "full-000001"}},
		"out of order":   {LineIDs: []string{"full-000003", "full-000001"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateLyricsSourceGameProjection(projection, validLyricsSourceFull()); err == nil {
				t.Fatal("invalid GameProjection was accepted")
			}
		})
	}
}

func TestDecodeLyricsSourceDocumentRejectsRomanizationUnknownDuplicateAndTrailingFields(t *testing.T) {
	body, err := json.Marshal(validLyricsSourceDocument())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLyricsSourceDocument(body); err != nil {
		t.Fatalf("valid closed document: %v", err)
	}

	for name, mutated := range map[string][]byte{
		"romanization":   []byte(strings.Replace(string(body), `"text":"初音歌う"`, `"text":"初音歌う","romanizedText":"Hatsune utau"`, 1)),
		"romaji":         []byte(strings.Replace(string(body), `"text":"初音歌う"`, `"text":"初音歌う","romaji":"Hatsune utau"`, 1)),
		"missing reason": []byte(strings.Replace(string(body), `"reasonCode":"tagged_full_and_game",`, "", 1)),
		"unknown":        []byte(strings.Replace(string(body), `"schemaVersion":2`, `"schemaVersion":2,"futureField":true`, 1)),
		"duplicate":      []byte(strings.Replace(string(body), `"schemaVersion":2`, `"schemaVersion":2,"schemaVersion":2`, 1)),
		"trailing":       append(append([]byte{}, body...), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeLyricsSourceDocument(mutated); err == nil {
				t.Fatal("invalid source-boundary JSON was accepted")
			} else if (name == "romanization" || name == "romaji") && !strings.Contains(err.Error(), "romanization field") {
				t.Fatalf("romanization error = %v", err)
			}
		})
	}
}

func TestClosedLyricsSourceJSONRejectsLoneSurrogatesAndExcessiveDepth(t *testing.T) {
	fixedBody, err := json.Marshal(validProviderFixedIdentity(LyricsSourceProviderVocaloidFandom, "full-sekai"))
	if err != nil {
		t.Fatal(err)
	}
	documentBody, err := json.Marshal(validLyricsSourceDocument())
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		body          []byte
		loneSurrogate []byte
		decode        func([]byte) error
	}{
		{
			name: "fixed identity", body: fixedBody,
			loneSurrogate: []byte(strings.Replace(string(fixedBody), `"title":"合成試験曲"`, `"title":"\uD800"`, 1)),
			decode: func(body []byte) error {
				_, err := DecodeLyricsSourceFixedIdentity(body)
				return err
			},
		},
		{
			name: "document", body: documentBody,
			loneSurrogate: []byte(strings.Replace(string(documentBody), `"text":"初音歌う"`, `"text":"\uD800"`, 1)),
			decode: func(body []byte) error {
				_, err := DecodeLyricsSourceDocument(body)
				return err
			},
		},
	}
	deepValue := strings.Repeat("[", MaxLyricsSourceJSONDepth+1) + "0" +
		strings.Repeat("]", MaxLyricsSourceJSONDepth+1)
	for _, test := range cases {
		t.Run(test.name+" lone surrogate", func(t *testing.T) {
			if err := test.decode(test.loneSurrogate); err == nil || !strings.Contains(err.Error(), "surrogate") {
				t.Fatalf("lone-surrogate error = %v", err)
			}
		})
		t.Run(test.name+" excessive depth", func(t *testing.T) {
			deepBody := []byte(string(test.body[:len(test.body)-1]) + `,"adversarial":` + deepValue + `}`)
			if err := test.decode(deepBody); err == nil || !strings.Contains(err.Error(), "nesting depth") {
				t.Fatalf("nesting-depth error = %v", err)
			}
		})
	}
}

func TestValidateLyricsSourceFullRejectsRubyOrSegmentDrift(t *testing.T) {
	for name, mutate := range map[string]func(*LyricsSourceFull){
		"line drift": func(full *LyricsSourceFull) {
			full.Lines[0].Text = "別の歌詞"
		},
		"ruby drift": func(full *LyricsSourceFull) {
			full.Lines[0].Segments[0].Ruby[0].Text = "初"
		},
		"Latin ruby reading": func(full *LyricsSourceFull) {
			full.Lines[0].Segments[0].Ruby[0].Reading = "Hatsune utau"
		},
		"unknown performer": func(full *LyricsSourceFull) {
			full.Lines[0].Segments[0].PerformerIDs = []string{"unknown"}
		},
		"duplicate line ID": func(full *LyricsSourceFull) {
			full.Lines[1].ID = full.Lines[0].ID
		},
		"reading without generator": func(full *LyricsSourceFull) {
			full.RubyGeneratorVersion = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			full := validLyricsSourceFull()
			mutate(&full)
			if err := ValidateLyricsSourceFull(full); err == nil {
				t.Fatal("invalid authoritative Full was accepted")
			}
		})
	}
}

func TestLyricsSourceProviderOriginsAcceptProjectSekaiFandomForVocaloidFandom(t *testing.T) {
	tests := []struct {
		provider LyricsSourceProvider
		origin   string
		want     bool
	}{
		{LyricsSourceProviderVocaloidFandom, "https://vocaloid.fandom.com", true},
		{LyricsSourceProviderVocaloidFandom, "https://projectsekai.fandom.com", true},
		{LyricsSourceProviderVocaloidFandom, "https://www.sekaipedia.org", false},
		{LyricsSourceProviderSekaipedia, "https://www.sekaipedia.org", true},
		{LyricsSourceProviderSekaipedia, "https://projectsekai.fandom.com", false},
		{LyricsSourceProviderMoegirl, "https://moegirl.icu", true},
		{LyricsSourceProviderMoegirlPublicExact, "https://zh.moegirl.org.cn", true},
		{LyricsSourceProvider("unknown"), "https://vocaloid.fandom.com", false},
	}
	for _, test := range tests {
		if got := LyricsSourceProviderAcceptsOrigin(test.provider, test.origin); got != test.want {
			t.Errorf("LyricsSourceProviderAcceptsOrigin(%q, %q) = %t, want %t", test.provider, test.origin, got, test.want)
		}
	}
	if origins := LyricsSourceProviderOrigins(LyricsSourceProviderVocaloidFandom); len(origins) == 0 || origins[0] != LyricsSourceOriginVocaloidFandom {
		t.Fatalf("vocaloid_fandom primary origin = %v", origins)
	}
}
