package lyricsrecoveryimport

import (
	"testing"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func TestCompareDeterministicPublicRuby(t *testing.T) {
	expected, err := lyricssource.GenerateDeterministicRubySpans("本当")
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]model.LyricsSourceRubySpan, len(expected))
	for index, span := range expected {
		actual[index] = model.LyricsSourceRubySpan{Text: span.Text, Reading: span.Reading}
	}
	if err := compareDeterministicPublicRuby(actual, "本当"); err != nil {
		t.Fatalf("deterministic public ruby was rejected: %v", err)
	}
	if len(actual) == 0 {
		t.Fatal("deterministic ruby fixture was empty")
	}
	actual[0].Reading += "ず"
	if err := compareDeterministicPublicRuby(actual, "本当"); err == nil {
		t.Fatal("ruby drift was accepted")
	}
	actual[0].Reading = expected[0].Reading
	actual[0].ReadingEvidence = &model.LyricsSourceReadingEvidence{}
	if err := compareDeterministicPublicRuby(actual, "本当"); err == nil {
		t.Fatal("provider reading evidence was accepted")
	}
}
