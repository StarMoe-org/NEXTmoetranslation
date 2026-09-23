package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/db"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/searchindex"
)

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
		projectionStatus := deps.projection.Status()
		if projectionStatus.Generation == 0 || projectionStatus.Pending || projectionStatus.LastError != "" {
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
			projectionStatus.Pending || projectionStatus.LastError != "" ||
			(deps.search != nil && !deps.search.Status().Ready) {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ready"}`)
	}))
	details := deps.auth.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		detail := map[string]any{
			"status": "ok",
			"requests": map[string]uint64{
				"total": httpRequestTotal.Load(), "clientErrors": httpClientErrors.Load(),
				"serverErrors": httpServerErrors.Load(),
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
