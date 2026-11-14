package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPublicLyricsV3ContractFixturesValidateStrictSchemas(t *testing.T) {
	root := filepath.Join("..", "..", "..", "contracts", "public-lyrics", "v3")
	indexSchema := compilePublicLyricsV3Schema(t, "index.schema.json", root)
	detailSchema := compilePublicLyricsV3Schema(t, "detail.schema.json", root)

	indexFixture := readPublicLyricsV3Contract(t, filepath.Join(root, "index.fixture.json"))
	validatePublicLyricsV3JSON(t, indexSchema, indexFixture)
	for _, name := range []string{
		"detail-legacy-one-rendition.fixture.json",
		"detail-full-only.fixture.json",
		"detail-full-game-projection.fixture.json",
		"detail-game-only.fixture.json",
		"detail-rem-two-families.fixture.json",
	} {
		validatePublicLyricsV3JSON(t, detailSchema, readPublicLyricsV3Contract(t, filepath.Join(root, name)))
	}
}

func TestPublicLyricsV3ContractRejectsUnknownAndLossyShapes(t *testing.T) {
	root := filepath.Join("..", "..", "..", "contracts", "public-lyrics", "v3")
	indexSchema := compilePublicLyricsV3Schema(t, "index.schema.json", root)
	detailSchema := compilePublicLyricsV3Schema(t, "detail.schema.json", root)

	readDetail := func(name string) map[string]any {
		t.Helper()
		var document map[string]any
		if err := json.Unmarshal(readPublicLyricsV3Contract(t, filepath.Join(root, name)), &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	firstRendition := func(document map[string]any) map[string]any {
		t.Helper()
		return document["renditions"].([]any)[0].(map[string]any)
	}
	firstRuby := func(document map[string]any) []any {
		t.Helper()
		rendition := firstRendition(document)
		side := rendition["full"]
		if side == nil {
			side = rendition["game"]
		}
		line := side.(map[string]any)["lines"].([]any)[0].(map[string]any)
		segment := line["segments"].([]any)[0].(map[string]any)
		return segment["ruby"].([]any)
	}
	validateRejected := func(schema *jsonschema.Schema, document any, reason string) {
		t.Helper()
		body, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(decodePublicLyricsV3JSONForSchema(t, body)); err == nil {
			t.Fatal(reason)
		}
	}

	unknownRoot := readDetail("detail-full-only.fixture.json")
	unknownRoot["unexpected"] = true
	validateRejected(detailSchema, unknownRoot, "detail schema accepted an unknown root property")

	unknownNested := readDetail("detail-full-only.fixture.json")
	firstRendition(unknownNested)["unexpected"] = true
	validateRejected(detailSchema, unknownNested, "detail schema accepted an unknown nested property")

	missingHanReading := readDetail("detail-legacy-one-rendition.fixture.json")
	delete(firstRuby(missingHanReading)[0].(map[string]any), "reading")
	validateRejected(detailSchema, missingHanReading, "detail schema accepted a Han span without a reading")

	blankBase := readDetail("detail-legacy-one-rendition.fixture.json")
	firstRuby(blankBase)[1].(map[string]any)["text"] = " "
	firstRuby(blankBase)[1].(map[string]any)["reading"] = "う"
	validateRejected(detailSchema, blankBase, "detail schema accepted a reading over a blank base")

	readingWithoutKana := readDetail("detail-legacy-one-rendition.fixture.json")
	firstRuby(readingWithoutKana)[0].(map[string]any)["reading"] = "ー"
	validateRejected(detailSchema, readingWithoutKana, "detail schema accepted a reading without kana")

	wrongRevisionHost := readDetail("detail-full-only.fixture.json")
	provenance := firstRendition(wrongRevisionHost)["provenance"].([]any)
	provenance[0].(map[string]any)["revisionUrl"] = "https://example.invalid/wiki/Test?oldid=1202"
	validateRejected(detailSchema, wrongRevisionHost, "detail schema accepted a provider-mismatched revision URL")

	mutableMoegirlURL := readDetail("detail-full-only.fixture.json")
	attribution := firstRendition(mutableMoegirlURL)["provenance"].([]any)[0].(map[string]any)
	attribution["provider"] = "moegirl"
	attribution["revisionUrl"] = "https://moegirl.icu/wiki/Public_Test"
	attribution["licenseName"] = "CC BY-NC-SA 3.0"
	attribution["licenseUrl"] = "https://creativecommons.org/licenses/by-nc-sa/3.0/"
	validateRejected(detailSchema, mutableMoegirlURL, "detail schema accepted a mutable Moegirl URL without an oldid")

	gameOnly := readDetail("detail-game-only.fixture.json")
	rendition := firstRendition(gameOnly)
	rendition["full"] = rendition["game"]
	validateRejected(detailSchema, gameOnly, "detail schema accepted full lyrics in a game_only document")

	var index map[string]any
	if err := json.Unmarshal(readPublicLyricsV3Contract(t, filepath.Join(root, "index.fixture.json")), &index); err != nil {
		t.Fatal(err)
	}
	emptyIndex := map[string]any{"version": float64(3), "songs": []any{}}
	validateRejected(indexSchema, emptyIndex, "index schema accepted an empty catalog")
	unresolvedWithText := index["songs"].([]any)[0].(map[string]any)
	unresolvedWithText["state"] = "incomplete"
	validateRejected(indexSchema, index, "index schema accepted available versions on an unresolved entry")
}

func TestPublicLyricsV3ContractTreatsIdeographicZeroAsNumericPlainText(t *testing.T) {
	root := filepath.Join("..", "..", "..", "contracts", "public-lyrics", "v3")
	detailSchema := compilePublicLyricsV3Schema(t, "detail.schema.json", root)
	var document map[string]any
	if err := json.Unmarshal(readPublicLyricsV3Contract(t, filepath.Join(root, "detail-legacy-one-rendition.fixture.json")), &document); err != nil {
		t.Fatal(err)
	}
	rendition := document["renditions"].([]any)[0].(map[string]any)
	line := rendition["full"].(map[string]any)["lines"].([]any)[0].(map[string]any)
	segment := line["segments"].([]any)[0].(map[string]any)
	line["japanese"] = "〇"
	segment["text"] = "〇"
	segment["ruby"] = []any{map[string]any{"text": "〇"}}
	plain, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	validatePublicLyricsV3JSON(t, detailSchema, plain)

	// It needs no reading, but may carry a gloss like any other text.
	segment["ruby"].([]any)[0].(map[string]any)["reading"] = "れい"
	annotated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	validatePublicLyricsV3JSON(t, detailSchema, annotated)
}

func compilePublicLyricsV3Schema(t *testing.T, name, root string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	body := readPublicLyricsV3Contract(t, filepath.Join(root, name))
	var resource any
	if err := json.Unmarshal(body, &resource); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource(name, resource); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func validatePublicLyricsV3JSON(t *testing.T, schema *jsonschema.Schema, body []byte) {
	t.Helper()
	if err := schema.Validate(decodePublicLyricsV3JSONForSchema(t, body)); err != nil {
		t.Fatal(err)
	}
}

func decodePublicLyricsV3JSONForSchema(t *testing.T, body []byte) any {
	t.Helper()
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func readPublicLyricsV3Contract(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || bytes.TrimSpace(body) == nil {
		t.Fatal("empty public v3 contract file")
	}
	return body
}
