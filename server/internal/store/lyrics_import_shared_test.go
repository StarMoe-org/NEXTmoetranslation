package store

import (
	"errors"
	"testing"
)

func TestMapDeclaredLyricsSourcePerformerIDsRejectsUndeclaredLabel(t *testing.T) {
	if _, err := MapDeclaredLyricsSourcePerformerIDs(
		[]string{"undeclared_singer"},
		map[string]int{"miku": 21},
		map[string]bool{"external_singer": true},
	); !errors.Is(err, ErrLyricsSourcePerformerMapping) {
		t.Fatalf("undeclared performer error=%v", err)
	}
}
