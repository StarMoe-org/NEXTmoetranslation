package main

import (
	"testing"

	"moesekai/server/internal/lyricssource"
)

func TestUnpublishedLyricsIsExplicitDeterministicIncompleteCode(t *testing.T) {
	err := lyricssource.ErrLyricsUnpublished
	if retryableSourceError(err) {
		t.Fatal("explicit unpublished evidence must not be retried at the same fixed revision")
	}
	if !incompleteSourceError(err) {
		t.Fatal("explicit unpublished evidence must remain an explainable incomplete result")
	}
	if got := safeErrorCode(err); got != "lyrics_unpublished" {
		t.Fatalf("safe error code = %q", got)
	}
	if _, resumable := safeResumeIncompleteCodes["lyrics_unpublished"]; resumable {
		t.Fatal("a fixed unpublished revision must not be parser-resumed")
	}
}
