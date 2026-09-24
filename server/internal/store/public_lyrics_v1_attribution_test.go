package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

// pjskV1DetailRejection ports the pjsk.moe validateDocument rules for a
// version-1 detail header (web/src/lib/lyrics.ts: validateDocument,
// isAttributions, isAttribution, isCanonicalAttributionRevisionUrl). The
// empty string means the main site accepts the header.
func pjskV1DetailRejection(body []byte) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return err.Error()
	}
	allowed := map[string]bool{"version": true, "musicId": true, "revision": true, "updatedAt": true,
		"attribution": true, "attributions": true, "lines": true}
	for key := range top {
		if !allowed[key] {
			return "unknown top-level key " + key
		}
	}
	rawAttribution, hasAttribution := top["attribution"]
	rawAttributions, hasAttributions := top["attributions"]
	if !hasAttribution && !hasAttributions {
		return "neither attribution nor attributions"
	}
	if hasAttribution {
		var attribution string
		if err := json.Unmarshal(rawAttribution, &attribution); err != nil || strings.TrimSpace(attribution) == "" ||
			utf8.RuneCountInString(attribution) > 16*1024 {
			return "invalid attribution"
		}
	}
	if !hasAttributions {
		return ""
	}
	var attributions []map[string]json.RawMessage
	if err := json.Unmarshal(rawAttributions, &attributions); err != nil || len(attributions) == 0 || len(attributions) > 16 {
		return "attributions must be a non-empty array of at most 16"
	}
	licenses := map[string][2]string{
		"vocaloid_fandom":      {"CC BY-SA 3.0", "https://creativecommons.org/licenses/by-sa/3.0/"},
		"moegirl":              {"CC BY-NC-SA 3.0", "https://creativecommons.org/licenses/by-nc-sa/3.0/"},
		"moegirl_public_exact": {"CC BY-NC-SA 3.0", "https://creativecommons.org/licenses/by-nc-sa/3.0/"},
		"sekaipedia":           {"CC BY-SA 4.0", "https://creativecommons.org/licenses/by-sa/4.0/"},
	}
	identities := map[string]bool{}
	for index, raw := range attributions {
		for key := range raw {
			switch key {
			case "provider", "title", "revisionId", "revisionUrl", "licenseName", "licenseUrl":
			default:
				return fmt.Sprintf("attributions[%d] has unknown key %s", index, key)
			}
		}
		var attribution struct {
			Provider    string  `json:"provider"`
			Title       string  `json:"title"`
			RevisionID  float64 `json:"revisionId"`
			RevisionURL string  `json:"revisionUrl"`
			LicenseName string  `json:"licenseName"`
			LicenseURL  string  `json:"licenseUrl"`
		}
		encoded, _ := json.Marshal(raw)
		if err := json.Unmarshal(encoded, &attribution); err != nil {
			return fmt.Sprintf("attributions[%d]: %v", index, err)
		}
		license, known := licenses[attribution.Provider]
		revisionID := int(attribution.RevisionID)
		if !known || float64(revisionID) != attribution.RevisionID || revisionID < 1 {
			return fmt.Sprintf("attributions[%d] has an invalid provider or revisionId", index)
		}
		if attribution.Title == "" || utf8.RuneCountInString(attribution.Title) > 2048 ||
			attribution.LicenseName != license[0] || attribution.LicenseURL != license[1] {
			return fmt.Sprintf("attributions[%d] has an invalid title or license", index)
		}
		if !pjskCanonicalAttributionRevisionURL(attribution.RevisionURL, attribution.Provider, revisionID) {
			return fmt.Sprintf("attributions[%d] revisionUrl %q is not canonical", index, attribution.RevisionURL)
		}
		identity := attribution.Provider + "\x00" + strconv.Itoa(revisionID)
		if identities[identity] {
			return "attributions repeat a provider revision"
		}
		identities[identity] = true
	}
	return ""
}

// pjskCanonicalAttributionRevisionURL mirrors isCanonicalAttributionRevisionUrl.
// Go's URL serialization stands in for WHATWG url.toString(); both escape
// spaces and non-ASCII path bytes.
func pjskCanonicalAttributionRevisionURL(value, provider string, revisionID int) bool {
	if value == "" || len(value) > 4096 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.String() != value || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Port() != "" || parsed.Fragment != "" {
		return false
	}
	revision := strconv.Itoa(revisionID)
	base := parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
	path := parsed.EscapedPath()
	switch provider {
	case "moegirl_public_exact":
		return value == "https://zh.moegirl.org.cn/%E4%BA%BF%E5%B9%B4%E7%88%B1%E6%81%8B" && parsed.RawQuery == ""
	case "moegirl":
		if parsed.Hostname() != "moegirl.icu" {
			return false
		}
		if strings.HasPrefix(path, "/wiki/") && len(path) > len("/wiki/") {
			return value == base+"?oldid="+revision
		}
		title := parsed.Query().Get("title")
		return path == "/index.php" && title != "" &&
			value == base+"?"+url.Values{"oldid": {revision}, "title": {title}}.Encode()
	case "vocaloid_fandom", "sekaipedia":
		host := parsed.Hostname()
		validHost := host == "www.sekaipedia.org"
		if provider == "vocaloid_fandom" {
			validHost = host == "vocaloid.fandom.com" || host == "projectsekai.fandom.com"
		}
		return validHost && strings.HasPrefix(path, "/wiki/") && len(path) > len("/wiki/") &&
			value == base+"?oldid="+revision
	default:
		return false
	}
}

func TestServedLegacyV1DetailAttributionsPassTheMainSiteRules(t *testing.T) {
	s := setupLyricsStore(t)
	cases := []struct {
		name       string
		sourceURL  string
		revisionID int
		// provider is the attribution the detail must carry; empty means
		// attributions must be omitted. "*" accepts either outcome.
		provider model.LyricsSourceProvider
	}{
		{"wikia host", "https://vocaloid.wikia.com/wiki/Song?oldid=123", 123, ""},
		{"other fandom wiki", "https://utaite.fandom.com/wiki/Song?oldid=3", 3, ""},
		{"projectsekai oldid differs from revision", "https://projectsekai.fandom.com/wiki/Song?oldid=5", 6, ""},
		{"unescaped page title", "https://projectsekai.fandom.com/wiki/Tell Your World?oldid=5", 5, "*"},
		{"projectsekai canonical revision", "https://projectsekai.fandom.com/wiki/Song?oldid=5", 5, model.LyricsSourceProviderVocaloidFandom},
		{"vocaloid fandom canonical revision", "https://vocaloid.fandom.com/wiki/Song?oldid=20", 20, model.LyricsSourceProviderVocaloidFandom},
		{"sekaipedia canonical revision", "https://www.sekaipedia.org/wiki/Test_Song?oldid=77", 77, model.LyricsSourceProviderSekaipedia},
	}
	records := make([]MusicCatalogRecord, len(cases))
	for index := range cases {
		records[index] = MusicCatalogRecord{MusicID: 100 + index, JapaneseTitle: fmt.Sprintf("合成曲%d", index)}
	}
	if err := s.UpsertMusicCatalog(records); err != nil {
		t.Fatal(err)
	}
	for index, tc := range cases {
		input := validLyrics()
		input.MusicID = 100 + index
		input.SourceURL = tc.sourceURL
		input.SourcePageID = 10
		input.SourceRevisionID = tc.revisionID
		input.SourceSHA1 = validSourceSHA1
		input.SourceFetchedAt = "2026-07-22T12:00:00Z"
		saved, _, err := s.SaveLyricsMutation(input, "agent")
		if err != nil {
			t.Fatalf("%s: save: %v", tc.name, err)
		}
		if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
			t.Fatalf("%s: publish: %v", tc.name, err)
		}
	}
	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	for index, tc := range cases {
		detail, ok := details[100+index]
		if !ok {
			t.Fatalf("%s: no served detail", tc.name)
		}
		body, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		if rejection := pjskV1DetailRejection(body); rejection != "" {
			t.Errorf("%s: main site rejects the served detail: %s\n%s", tc.name, rejection, body)
			continue
		}
		if detail.Attribution == "" {
			t.Errorf("%s: served detail lost its attribution", tc.name)
		}
		switch tc.provider {
		case "*":
		case "":
			if detail.Attributions != nil {
				t.Errorf("%s: non-canonical source kept attributions %+v", tc.name, detail.Attributions)
			}
		default:
			if len(detail.Attributions) != 1 || detail.Attributions[0].Provider != tc.provider ||
				detail.Attributions[0].RevisionURL != tc.sourceURL || detail.Attributions[0].RevisionID != tc.revisionID {
				t.Errorf("%s: attributions=%+v", tc.name, detail.Attributions)
			}
		}
	}
}

// A legacy song published without credits is attributed only by the source
// card its v1 detail derives from the source, so a source that yields no card
// cannot be published source-only.
func TestSourceOnlyLegacyPublicationRequiresAServedSourceAttribution(t *testing.T) {
	s := setupLyricsStore(t)
	cases := []struct {
		name       string
		sourceURL  string
		revisionID int
		publishes  bool
	}{
		{"other fandom wiki", "https://utaite.fandom.com/wiki/Song?oldid=3", 3, false},
		{"wikia host", "https://vocaloid.wikia.com/wiki/Song?oldid=123", 123, false},
		{"projectsekai oldid differs from revision", "https://projectsekai.fandom.com/wiki/Song?oldid=5", 6, false},
		{"projectsekai canonical revision", "https://projectsekai.fandom.com/wiki/Song?oldid=5", 5, true},
		{"sekaipedia canonical revision", "https://www.sekaipedia.org/wiki/Test_Song?oldid=77", 77, true},
	}
	records := make([]MusicCatalogRecord, len(cases))
	for index := range cases {
		records[index] = MusicCatalogRecord{MusicID: 200 + index, JapaneseTitle: fmt.Sprintf("合成曲%d", index)}
	}
	if err := s.UpsertMusicCatalog(records); err != nil {
		t.Fatal(err)
	}
	for index, tc := range cases {
		input := validLyrics()
		input.MusicID = 200 + index
		input.Attribution, input.TranslationCredit, input.ProofreadingCredit = "", "", ""
		input.SourceURL, input.SourcePageID, input.SourceRevisionID = tc.sourceURL, 10, tc.revisionID
		input.SourceSHA1, input.SourceFetchedAt = validSourceSHA1, "2026-07-22T12:00:00Z"
		saved, _, err := s.SaveLyricsMutation(input, "agent")
		if err != nil {
			t.Fatalf("%s: save: %v", tc.name, err)
		}
		_, err = s.PublishLyrics(saved.MusicID, saved.Revision)
		var contractErr *LyricsContractError
		if tc.publishes != (err == nil) || !tc.publishes && (!errors.As(err, &contractErr) || contractErr.Code != "incomplete_publication") {
			t.Fatalf("%s: publish err=%v, want published=%t", tc.name, err, tc.publishes)
		}
	}
	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	for index, tc := range cases {
		detail, served := details[200+index]
		if served != tc.publishes {
			t.Fatalf("%s: served=%t", tc.name, served)
		}
		if !served {
			continue
		}
		body, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		if rejection := pjskV1DetailRejection(body); rejection != "" || detail.Attribution != "" || len(detail.Attributions) != 1 {
			t.Fatalf("%s: served source-only detail rejection=%q\n%s", tc.name, rejection, body)
		}
	}
}
