package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"runtime/debug"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/db"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/searchindex"
)

func init() {
	// net/http/pprof's init registers handlers on http.DefaultServeMux. Clear it
	// so pprof is reachable only through our explicit admin-authenticated mux.
	http.DefaultServeMux = http.NewServeMux()
}

// operationalDeps are the probe dependencies. draining and search are optional;
// without a projection /readyz never reports ready.
type operationalDeps struct {
	database   *db.DB
	auth       *auth.Auth
	draining   interface{ IsDraining() bool }
	projection interface {
		Status() filesvc.ProjectionStatus
	}
	search interface {
		Status() searchindex.Status
	}
}

func registerOperationalRoutes(mux *http.ServeMux, deps operationalDeps) {
	mux.HandleFunc("/healthz", operationalGetOnly(func(w http.ResponseWriter, r *http.Request) {
		setOperationalHeaders(w.Header())
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	mux.HandleFunc("/readyz", operationalGetOnly(func(w http.ResponseWriter, r *http.Request) {
		setOperationalHeaders(w.Header())
		if deps.draining != nil && deps.draining.IsDraining() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := deps.database.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		if deps.projection == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		// Ready once a projection has been published. A pending or failed rebuild
		// keeps serving the last published files, so it must not take the only
		// replica out of the gateway: every saved edit queues a rebuild.
		projectionStatus := deps.projection.Status()
		if projectionStatus.Generation == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		if deps.search != nil && !deps.search.Status().Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		// Status can change while the database probe and provider checks run.
		// Recheck every volatile readiness dependency immediately before 200.
		projectionStatus = deps.projection.Status()
		if (deps.draining != nil && deps.draining.IsDraining()) || projectionStatus.Generation == 0 ||
			(deps.search != nil && !deps.search.Status().Ready) {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ready"}`)
	}))
	details := deps.auth.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		var lastGCUnix int64
		if mem.LastGC > 0 {
			lastGCUnix = int64(mem.LastGC / uint64(time.Second))
		}
		detail := map[string]any{
			"status": "ok",
			"requests": map[string]uint64{
				"total": httpRequestTotal.Load(), "clientErrors": httpClientErrors.Load(),
				"serverErrors": httpServerErrors.Load(),
			},
			"memory": map[string]any{
				"heapAllocBytes":    mem.HeapAlloc,
				"heapInuseBytes":    mem.HeapInuse,
				"heapIdleBytes":     mem.HeapIdle,
				"heapReleasedBytes": mem.HeapReleased,
				"heapSysBytes":      mem.HeapSys,
				"heapObjects":       mem.HeapObjects,
				"sysBytes":          mem.Sys,
				"gomemlimitBytes":   debug.SetMemoryLimit(-1),
				"numGC":             mem.NumGC,
				"lastGCUnix":        lastGCUnix,
				"goroutines":        runtime.NumGoroutine(),
			},
		}
		if deps.search != nil {
			detail["search"] = deps.search.Status()
		}
		_ = json.NewEncoder(w).Encode(detail)
	})
	mux.HandleFunc("/healthz/details", operationalGetOnly(func(w http.ResponseWriter, r *http.Request) {
		setOperationalHeaders(w.Header())
		details(w, r)
	}))
	registerAdminPprofRoutes(mux, deps.auth)
}

func registerAdminPprofRoutes(mux *http.ServeMux, authService *auth.Auth) {
	if authService == nil {
		return
	}
	adminGet := func(handler http.HandlerFunc) http.HandlerFunc {
		guarded := authService.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			handler(w, r)
		})
		return operationalGetOnly(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			guarded(w, r)
		})
	}
	mux.HandleFunc("/debug/pprof/", adminGet(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", adminGet(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", adminGet(pprof.Profile))
	mux.HandleFunc("/debug/pprof/trace", adminGet(pprof.Trace))
	symbolGuarded := authService.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		pprof.Symbol(w, r)
	})
	mux.HandleFunc("/debug/pprof/symbol", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
			setOperationalHeaders(w.Header())
			w.Header().Set("Allow", "GET, HEAD, POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprint(w, `{"error":"method not allowed"}`)
			return
		}
		symbolGuarded(w, r)
	})
}

// operationalGetOnly mirrors the console API's getOnly for the lifecycle probes,
// which main cannot import from package api.
func operationalGetOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			setOperationalHeaders(w.Header())
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprint(w, `{"error":"method not allowed"}`)
			return
		}
		next(w, r)
	}
}

func setOperationalHeaders(headers http.Header) {
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
}
