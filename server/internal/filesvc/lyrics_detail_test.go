package filesvc

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/store"
)

func TestPublicLyricsDetailReturnsACopyOfTheServedBytes(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "lyrics-detail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	es := store.NewEventStore(database)
	svc := New(s, es, files.NewGenerator(s, es, ""))
	svc.Rebuild()

	detail, ok := svc.PublicLyricsDetail(307)
	response := httptest.NewRecorder()
	svc.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/files/translation/lyrics/music_307.json", nil))
	if !ok || response.Code != http.StatusOK || !bytes.Equal(detail, response.Body.Bytes()) {
		t.Fatalf("detail ok=%t served=%d equal=%t", ok, response.Code, bytes.Equal(detail, response.Body.Bytes()))
	}
	detail[0] = 'x'
	if again, _ := svc.PublicLyricsDetail(307); again[0] == 'x' {
		t.Fatal("the accessor exposed the served buffer")
	}
	if _, ok := svc.PublicLyricsDetail(999999); ok {
		t.Fatal("an unserved song returned a detail")
	}
}
