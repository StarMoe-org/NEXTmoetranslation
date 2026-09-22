package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/db"
)

// The lifecycle probes answer any method otherwise, because their handlers never
// inspect r.Method.
func TestLifecycleProbesRejectWriteMethods(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "probes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mux := http.NewServeMux()
	registerOperationalRoutes(mux, database, auth.New(database, "operations-secret-at-least-32-bytes", time.Hour))

	for _, path := range []string{"/healthz", "/readyz", "/healthz/details"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(method, path, nil))
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status = %d, want 405", method, path, response.Code)
			}
			if got := response.Header().Get("Allow"); got != "GET, HEAD" {
				t.Fatalf("%s %s Allow = %q", method, path, got)
			}
		}
		read := httptest.NewRecorder()
		mux.ServeHTTP(read, httptest.NewRequest(http.MethodGet, path, nil))
		if read.Code == http.StatusMethodNotAllowed {
			t.Fatalf("GET %s was rejected as 405", path)
		}
	}
}

// corsMiddleware skips the public file paths, so without an explicit answer here
// a cross-origin preflight for /files/* or /translation/* carries no
// Access-Control-Allow-Origin and the browser blocks the real GET.
func TestPreflightAnswersPublicFilePathsWithPermissiveCORS(t *testing.T) {
	const origin = "https://console.example.com"
	handler := corsMiddleware(preflightMiddleware(http.NewServeMux()), origin)

	for _, path := range []string{"/files/v2/index.json", "/translation/lyrics/index.json"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodOptions, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s preflight status = %d", path, response.Code)
		}
		if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("%s preflight Access-Control-Allow-Origin = %q", path, got)
		}
		if got := response.Header().Get("Access-Control-Allow-Methods"); got != "GET, HEAD, OPTIONS" {
			t.Fatalf("%s preflight Access-Control-Allow-Methods = %q", path, got)
		}
		if response.Header().Get("Access-Control-Max-Age") == "" {
			t.Fatalf("%s preflight has no Access-Control-Max-Age", path)
		}
	}

	console := httptest.NewRecorder()
	handler.ServeHTTP(console, httptest.NewRequest(http.MethodOptions, "/api/entries", nil))
	if console.Code != http.StatusNoContent || console.Header().Get("Access-Control-Allow-Origin") != origin ||
		console.Header().Get("Access-Control-Allow-Methods") != "GET, POST, PUT, DELETE, OPTIONS" ||
		console.Header().Get("Access-Control-Max-Age") != "" {
		t.Fatalf("console preflight status=%d headers=%v", console.Code, console.Header())
	}
}
