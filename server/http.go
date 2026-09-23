package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lifecycle"
)

// buildHandler mounts every route on one mux and wraps it in the middleware
// chain. It reports whether the console SPA is served from disk.
func buildHandler(env startupEnv, settings runtimeSettings, svc *services) (http.Handler, bool) {
	mux := http.NewServeMux()
	svc.api.RegisterRoutes(mux)
	registerPublicFileRoutes(mux, svc.files)
	registerOperationalRoutes(mux, operationalDeps{
		database: svc.database, auth: svc.auth, draining: svc.lifecycle,
		projection: svc.files, search: svc.search,
	})
	registerWorkspaceRoutes(mux)

	// Catch-all: serve the statically-exported console SPA. More specific routes
	// above (/api/, /files/, /healthz, /sse) take precedence in ServeMux, so "/"
	// only receives page and asset requests. This makes Go the single process —
	// no nginx, no Node.js.
	serveWeb := false
	if info, err := os.Stat(env.webDir); err == nil && info.IsDir() {
		mux.HandleFunc("/", staticHandler(env.webDir))
		serveWeb = true
	}

	handler := preflightMiddleware(mux)
	handler = lifecycleMiddleware(svc.lifecycle, handler)
	handler = workspaceTombstoneMiddleware(handler)
	handler = corsMiddleware(handler, settings.allowOrigin)
	return loggingMiddleware(handler), serveWeb
}

func registerPublicFileRoutes(mux *http.ServeMux, fileService *filesvc.Service) {
	mux.Handle("/files/", fileService.Handler())
	// /translation/* is a backward-compatible alias for /files/translation/*.
	// External sites (e.g. pjsk.moe) fetch translation JSON from this path.
	mux.HandleFunc("/translation/", func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, "/translation/")
		if suffix == "lyrics" || suffix == "lyrics/" {
			suffix = "lyrics/index.json"
		}
		r.URL.Path = "/files/translation/" + suffix
		fileService.Handler().ServeHTTP(w, r)
	})
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
}

func lifecycleMiddleware(state *lifecycle.State, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLifecycleProbe(r) {
			done, admitted := state.BeginProbe()
			if !admitted {
				writeDrainingResponse(w, r)
				return
			}
			defer done()
			next.ServeHTTP(w, r)
			return
		}
		done, admitted := state.BeginRequest()
		if !admitted {
			writeDrainingResponse(w, r)
			return
		}
		defer done()
		next.ServeHTTP(w, r)
	})
}

func writeDrainingResponse(w http.ResponseWriter, r *http.Request) {
	setOperationalHeaders(w.Header())
	// corsMiddleware scopes its header to the console origin and skips the public
	// file paths because their handlers set a permissive one. Those handlers never
	// run while draining, so repeat it here or the caller sees an opaque failure.
	if isPublicFilePath(r.URL.Path) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `{"status":"draining"}`)
}

func isPublicFilePath(path string) bool {
	return strings.HasPrefix(path, "/files/") || strings.HasPrefix(path, "/translation/")
}

func isLifecycleProbe(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.URL.Path == "/healthz" || r.URL.Path == "/readyz")
}

func corsMiddleware(next http.Handler, origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /files/* and /translation/* set their own permissive CORS; here we scope console API.
		if !isPublicFilePath(r.URL.Path) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Moe-Loaded-Producer-State, X-SSE-Presence")
		}
		next.ServeHTTP(w, r)
	})
}

func preflightMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			// corsMiddleware skips the public file paths because their handlers
			// set a permissive header, but no handler runs for a preflight.
			if isPublicFilePath(r.URL.Path) {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
