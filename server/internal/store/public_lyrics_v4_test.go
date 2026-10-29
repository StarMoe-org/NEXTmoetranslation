package store

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestPublicLyricsV4CanonicalFixturesStrictRoundTrip(t *testing.T) {
	root := "../../../contracts/public-lyrics/v4"
	for _, name := range []string{
		"detail-default-only.fixture.json",
		"detail-multi-edition.fixture.json",
		"detail-exact-projection.fixture.json",
	} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(root + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			detail, err := DecodePublicLyricsV4Detail(body)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodePublicLyricsV4Detail(detail)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodePublicLyricsV4Detail(encoded)
			if err != nil || !reflect.DeepEqual(decoded, detail) {
				t.Fatalf("strict v4 round trip err=%v decoded=%+v", err, decoded)
			}
		})
	}
	indexBody, err := os.ReadFile(root + "/index.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	index, err := DecodePublicLyricsV4Index(indexBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodePublicLyricsV4Index(index); err != nil {
		t.Fatal(err)
	}
}

func TestPublicLyricsV4StrictDecoderRejectsSourceTranslationsAndEditionDrift(t *testing.T) {
	body, err := os.ReadFile("../../../contracts/public-lyrics/v4/detail-exact-projection.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(change func(map[string]any)) []byte {
		t.Helper()
		var value map[string]any
		if err := json.Unmarshal(body, &value); err != nil {
			t.Fatal(err)
		}
		change(value)
		result, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for name, invalid := range map[string][]byte{
		"source zh-CN": mutate(func(value map[string]any) {
			value["renditions"].([]any)[0].(map[string]any)["full"].(map[string]any)["lines"].([]any)[0].(map[string]any)["zh-CN"] = "禁止"
		}),
		"source translation credits": mutate(func(value map[string]any) {
			value["renditions"].([]any)[0].(map[string]any)["translationCredits"] = map[string]any{"translation": "禁止"}
		}),
		"exact projection explicit Game": mutate(func(value map[string]any) {
			value["translationEditions"].([]any)[0].(map[string]any)["renditions"].([]any)[0].(map[string]any)["game"] = map[string]any{"translations": []any{"歌唱", "前进吧"}}
		}),
		"missing default": mutate(func(value map[string]any) {
			value["defaultTranslationEditionKey"] = "missing"
		}),
		"noncanonical editions": mutate(func(value map[string]any) {
			first := value["translationEditions"].([]any)[0]
			value["translationEditions"] = []any{first, first}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePublicLyricsV4Detail(invalid); err == nil {
				t.Fatal("tampered Public v4 detail was accepted")
			}
		})
	}
}
