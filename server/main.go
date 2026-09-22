package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"moesekai/server/internal/api"
	"moesekai/server/internal/auth"
	"moesekai/server/internal/backup"
	"moesekai/server/internal/collab"
	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/files"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/lifecycle"
	"moesekai/server/internal/lyricsdiscovery"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/searchindex"
	"moesekai/server/internal/singleinstance"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
	"moesekai/server/internal/upstream"
	"moesekai/server/internal/workspaceverify"
)

// runtimeProfile is overridden only in the standalone production binary via
// ldflags. Keeping the selector in the executable prevents container-level
// environment overrides from turning that binary back into development mode.
var runtimeProfile = "development"

const (
	runtimeProfileNextProduction   = "next-production"
	publishedAdminPasswordTemplate = "replace-with-12-or-more-characters"
)

func main() {
	// Timestamped logs (UTC) on stdout so `docker logs` shows operational activity.
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("")
	verifyWorkspaceOnly := len(os.Args) == 2 && os.Args[1] == "--verify-workspace"
	verifyRuntimeOnly := len(os.Args) == 2 && os.Args[1] == "--verify-runtime"
	if len(os.Args) > 1 && !verifyWorkspaceOnly && !verifyRuntimeOnly {
		fatal("arguments", errors.New("usage: moesekai-server [--verify-workspace|--verify-runtime]"))
	}

	production, err := resolveProductionMode(os.Getenv("MOESEKAI_PRODUCTION"))
	if err != nil {
		fatal("MOESEKAI_PRODUCTION", err)
	}
	timezoneRaw, timezoneConfigured := os.LookupEnv("TZ")
	if err := validateRuntimeTimezone(production, timezoneRaw, timezoneConfigured); err != nil {
		fatal("TZ", err)
	}
	if production {
		// Enforce UTC even if the approved base image's /etc/localtime differs.
		time.Local = time.UTC
	}
	workspaceModeRaw, workspaceModeConfigured := os.LookupEnv("WORKSPACE_MODE")
	workspaceWebDirRaw, workspaceConfigured := os.LookupEnv("WORKSPACE_WEB_DIR")
	workspaceWebDir := strings.TrimSpace(workspaceWebDirRaw)
	workspaceManifestSHARaw, workspaceManifestSHAConfigured := os.LookupEnv("WORKSPACE_MANIFEST_SHA256")
	workspaceManifestSHA := strings.TrimSpace(workspaceManifestSHARaw)
	workspaceConfig := workspaceverify.Config{
		Mode: workspaceModeRaw, Root: workspaceWebDir, ManifestSHA256: workspaceManifestSHA, Production: production,
		ModeConfigured: workspaceModeConfigured, RootConfigured: workspaceConfigured, ManifestSHA256Configured: workspaceManifestSHAConfigured,
	}
	var verifiedWorkspace *workspaceverify.Manifest
	if verifyWorkspaceOnly && !production {
		verifiedWorkspace, err = workspaceverify.Verify(workspaceConfig)
	} else {
		verifiedWorkspace, err = workspaceverify.VerifyRuntime(workspaceConfig)
	}
	if err != nil {
		fatal("workspace verification", err)
	}
	webDirRaw, webDirConfigured := os.LookupEnv("WEB_DIR")
	webDir, err := resolveWebDir(production, webDirRaw, webDirConfigured)
	if err != nil {
		fatal("WEB_DIR", err)
	}
	dbPathRaw, dbPathConfigured := os.LookupEnv("DB_PATH")
	dbPath, err := resolveDBPath(production, dbPathRaw, dbPathConfigured)
	if err != nil {
		fatal("DB_PATH", err)
	}
	dataDirRaw, dataDirConfigured := os.LookupEnv("DATA_DIR")
	dataDir, err := resolveDataDir(production, dataDirRaw, dataDirConfigured)
	if err != nil {
		fatal("DATA_DIR", err)
	}
	if verifyWorkspaceOnly || verifyRuntimeOnly {
		if workspaceModeRaw == workspaceverify.ModeDisabled {
			log.Println("workspace verified: disabled")
		} else if verifiedWorkspace == nil {
			log.Println("workspace verification skipped: optional workspace is not configured")
		} else {
			log.Printf("workspace verified: %s at %s (%s)", verifiedWorkspace.Artifact.AppVersion, workspaceWebDir, verifiedWorkspace.Producer.SourceRevision)
		}
		return
	}

	port := envOr("PORT", "8080")
	masterKey := os.Getenv("MOESEKAI_MASTER_KEY")
	jwtSecret := envOr("JWT_SECRET", "")
	allowOrigin := envOr("CONSOLE_ORIGIN", "*")
	shutdownConfig, err := shutdownConfigFromEnv()
	if err != nil {
		fatal("shutdown configuration", err)
	}
	if err := httpx.ValidateUpstreamEnvironment(production); err != nil {
		fatal("upstream network configuration", err)
	}
	if err := validateConsoleOrigin(production, allowOrigin); err != nil {
		fatal("CONSOLE_ORIGIN", err)
	}
	if err := validateProductionMasterKey(production, masterKey); err != nil {
		fatal("production startup validation", err)
	}

	if err := auth.ValidateJWTSecret(jwtSecret); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal: JWT_SECRET: %v\n", err)
		os.Exit(1)
	}

	instanceOwner, err := singleinstance.Acquire(dbPath)
	if err != nil {
		fatal("acquire database ownership", err)
	}
	defer func() {
		if err := instanceOwner.Close(); err != nil {
			log.Printf("release database ownership: %v", err)
		}
	}()
	if err := rejectIncompleteSeed(dbPath); err != nil {
		fatal("validate seed publication", err)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		fatal("open db", err)
	}
	defer database.Close()

	st := store.New(database)
	if runtimeProfile == runtimeProfileNextProduction {
		editorSeed, err := embeddedlyricsseed.Load()
		if err != nil {
			fatal("load embedded lyrics editor seed", err)
		}
		seedResult, err := st.ApplyEmbeddedLyricsEditorSeed(context.Background(), editorSeed)
		if errors.Is(err, store.ErrEmbeddedLyricsEditorSeedCatalogNotReady) {
			log.Printf("embedded lyrics editor seed: deferred because catalog is not initialized")
		} else if err != nil {
			fatal("apply embedded lyrics editor seed", err)
		} else {
			log.Printf("embedded lyrics editor seed: seed=%s inserted=%d preserved=%d replayed=%d sourceV3=%d legacy=%d availability=%d",
				seedResult.SeedSHA256, seedResult.Inserted, seedResult.PreservedExisting, seedResult.Replayed,
				seedResult.SourceV3, seedResult.Legacy, seedResult.Availability)
		}
	}

	tokenTTL, err := parseTTL(envOr("TOKEN_TTL_HOURS", "168"))
	if err != nil {
		fatal("TOKEN_TTL_HOURS", err)
	}
	authSvc := auth.New(database, jwtSecret, tokenTTL)
	if err := authSvc.ValidatePersistedRoles(); err != nil {
		fatal("validate persisted user roles", err)
	}

	cfg, err := config.New(database, masterKey)
	if err != nil {
		fatal("init config", err)
	}
	if err := seedConfigFromEnv(cfg); err != nil {
		fatal("seed configuration", err)
	}

	if err := seedAdminFromEnv(authSvc); err != nil {
		fatal("seed initial admin", err)
	}
	if err := validateProductionAdmin(production, authSvc); err != nil {
		fatal("production startup validation", err)
	}

	es := store.NewEventStore(database)
	var lyricsDiscoveryWorker *lyricsdiscovery.Worker
	var lyricsFetchRevisionWorker *lyricsdiscovery.FetchWorker
	discoveryEnabled := cfg.GetBool(config.KeyLyricsDiscoveryOn, false)
	fetchEnabled := cfg.GetBool(config.KeyLyricsFetchRevisionOn, false)
	var lyricsSourceClient *lyricssource.Client
	if discoveryEnabled || fetchEnabled {
		lyricsSourceClient = lyricssource.New()
	}
	if discoveryEnabled {
		adapter, err := store.NewLyricsDiscoveryAdapter(st, store.LyricsDiscoveryShadowPolicyVersion, store.DefaultLyricsDiscoveryJobMaxAttempts)
		if err != nil {
			fatal("init lyrics discovery store", err)
		}
		executor, err := lyricsdiscovery.NewSourceExecutor(lyricsSourceClient)
		if err != nil {
			fatal("init lyrics discovery source", err)
		}
		workerOptions, err := lyricsDiscoveryOptionsFromEnv()
		if err != nil {
			fatal("lyrics discovery configuration", err)
		}
		lyricsDiscoveryWorker, err = lyricsdiscovery.New(adapter, executor, workerOptions)
		if err != nil {
			fatal("init lyrics discovery worker", err)
		}
	}
	if fetchEnabled {
		adapter, err := store.NewLyricsSourceFetchAdapter(st)
		if err != nil {
			fatal("init lyrics source fetch store", err)
		}
		executor, err := lyricsdiscovery.NewFetchExecutor(lyricsSourceClient)
		if err != nil {
			fatal("init lyrics source fetch executor", err)
		}
		workerOptions, err := lyricsFetchRevisionOptionsFromEnv()
		if err != nil {
			fatal("lyrics source fetch configuration", err)
		}
		lyricsFetchRevisionWorker, err = lyricsdiscovery.NewFetchWorker(adapter, executor, workerOptions)
		if err != nil {
			fatal("init lyrics source fetch worker", err)
		}
	}
	gen := files.NewGenerator(st, es, dataDir)

	fileService := filesvc.New(st, es, gen)
	filesDebounce, err := durationEnvMs("FILES_REBUILD_DEBOUNCE_MS", 5*time.Minute, 100*time.Millisecond, 24*time.Hour)
	if err != nil {
		fatal("FILES_REBUILD_DEBOUNCE_MS", err)
	}
	fileService.SetDebounce(filesDebounce)
	// Regenerate public files whenever the DB changes (debounced inside).
	st.OnChange(fileService.Trigger)

	searchDebounce, err := durationEnvMs("SEARCH_INDEX_DEBOUNCE_MS", time.Hour, time.Millisecond, 24*time.Hour)
	if err != nil {
		fatal("SEARCH_INDEX_DEBOUNCE_MS", err)
	}
	searchRefresh, err := durationEnvMs("SEARCH_INDEX_REFRESH_MS", time.Hour, time.Second, 24*time.Hour)
	if err != nil {
		fatal("SEARCH_INDEX_REFRESH_MS", err)
	}
	searchRetryMin, err := durationEnvMs("SEARCH_INDEX_RETRY_MIN_MS", 5*time.Second, time.Millisecond, 5*time.Minute)
	if err != nil {
		fatal("SEARCH_INDEX_RETRY_MIN_MS", err)
	}
	searchRetryMax, err := durationEnvMs("SEARCH_INDEX_RETRY_MAX_MS", 5*time.Minute, time.Millisecond, time.Hour)
	if err != nil {
		fatal("SEARCH_INDEX_RETRY_MAX_MS", err)
	}
	if searchRetryMax < searchRetryMin {
		fatal("search index retry configuration", errors.New("SEARCH_INDEX_RETRY_MAX_MS must be greater than or equal to SEARCH_INDEX_RETRY_MIN_MS"))
	}
	idx := searchindex.New(st, fileService, cfg, searchDebounce, searchRefresh)
	idx.SetRetryBounds(searchRetryMin, searchRetryMax)
	if production {
		idx.UseProductionCoverageFloors()
	}
	idx.SetCachePath(filepath.Join(dataDir, "search-index-cache.json"))
	st.OnChange(idx.Trigger)

	hub := sse.NewHub()
	appLifecycle := &lifecycle.State{}

	editorGate, err := editorgate.New()
	if err != nil {
		fatal("init editor gate", err)
	}
	tr := translator.New(st, es, cfg, editorGate)
	tr.SetProgress(func(stage, detail string, cur, total int) {
		hub.Broadcast(stage, map[string]any{"detail": detail, "current": cur, "total": total})
	})

	// Upstream watcher: polls current_version.json directly (not GitHub REST API),
	// backs off on raw-content 429s, and triggers CN sync on change.
	useGit := envOr("UPSTREAM_USE_GIT", "false") == "true"
	upstreamPoll, err := durationEnvMs("UPSTREAM_POLL_MS", time.Hour, time.Second, 24*time.Hour)
	if err != nil {
		fatal("UPSTREAM_POLL_MS", err)
	}
	watcher := upstream.NewWithContext(cfg, func(ctx context.Context) error {
		result, err := tr.SyncCNOnlyContext(ctx)
		if err != nil {
			return err
		}
		if warning := result.SkippedError(); warning != nil {
			return warning
		}
		return nil
	}, upstream.Options{
		Interval: upstreamPoll,
		GitDir:   filepath.Join(dataDir, "masterdata-mirror"),
		UseGit:   useGit,
	})
	// Backup manager: daily + manual backup/restore to S3 and/or GitHub.
	backupMgr := backup.NewManager(cfg, gen, st, es, filepath.Join(dataDir, "backup-work"))
	collabService, err := collab.New(database, st, authSvc, editorGate, allowOrigin)
	if err != nil {
		fatal("init lyrics collaboration", err)
	}
	backupMgr.SetAfterRestore(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := collabService.RetireAll(ctx); err != nil {
			log.Printf("[collab] post-restore retirement failed: %v", err)
		}
	})

	apiServer := api.NewServer(st, es, authSvc, cfg, hub, tr, watcher, backupMgr, editorGate)
	apiServer.SetCollab(collabService)
	apiServer.SetFileService(fileService)
	apiServer.SetSearchStatus(idx)

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	registerPublicFileRoutes(mux, fileService)

	registerOperationalRoutesWithSearch(mux, database, authSvc, appLifecycle, fileService, idx)
	registerWorkspaceRoutes(mux)

	// Catch-all: serve the statically-exported console SPA. More specific routes
	// above (/api/, /files/, /healthz, /sse) take precedence in ServeMux, so "/"
	// only receives page and asset requests. This makes Go the single process —
	// no nginx, no Node.js.
	serveWeb := false
	if st, err := os.Stat(webDir); err == nil && st.IsDir() {
		mux.HandleFunc("/", staticHandler(webDir))
		serveWeb = true
	}

	handler := preflightMiddleware(mux)
	handler = lifecycleMiddleware(appLifecycle, handler)
	handler = workspaceTombstoneMiddleware(handler)
	handler = corsMiddleware(handler, allowOrigin)
	handler = loggingMiddleware(handler)
	httpServer := newHTTPServer(":"+port, handler)
	listener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		fatal("listen", err)
	}
	defer listener.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	// Do not launch work that can mutate remote or local state until the process
	// has successfully claimed its TCP address.
	fileService.Start()
	idx.Start()
	watcher.Start()
	backupMgr.StartScheduler()
	if lyricsFetchRevisionWorker != nil {
		if err := lyricsFetchRevisionWorker.Start(context.Background()); err != nil {
			fatal("start lyrics source fetch worker", err)
		}
	}
	if lyricsDiscoveryWorker != nil {
		if err := lyricsDiscoveryWorker.Start(context.Background()); err != nil {
			fatal("start lyrics discovery worker", err)
		}
	}

	log.Printf("moesekai server starting on :%s", port)
	log.Printf("  db:        %s", dbPath)
	log.Printf("  data dir:  %s", dataDir)
	log.Printf("  files:     /files/* (public, cacheable)")
	log.Printf("  compat:    /translation/* (alias for /files/translation/*)")
	log.Printf("  api:       /api/*   (JWT, no-store)")
	if serveWeb {
		log.Printf("  console:   /       (static SPA from %s)", webDir)
	} else {
		log.Printf("  console:   not served (%s not found) — API-only mode", webDir)
	}
	log.Printf("  workspace: disabled (retired external workspace is verifier-only)")
	if !cfg.HasMasterKey() {
		log.Println("  WARNING: MOESEKAI_MASTER_KEY not set — secrets cannot be stored")
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- httpServer.Serve(listener) }()
	var serveErr error
	serveResultConsumed := false
	select {
	case err := <-serverErr:
		serveErr = err
		serveResultConsumed = true
	case sig := <-signals:
		log.Printf("shutdown requested by %s", sig)
	}

	shutdownErr := lifecycle.RunShutdown(shutdownConfig, log.Printf, os.Exit,
		func(ctx context.Context) error {
			// Close every admission gate first. The HTTP listener then closes and
			// already-admitted handlers receive only the short drain phase.
			appLifecycle.Drain()
			editorGate.Drain()
			backupMgr.Drain()
			if lyricsFetchRevisionWorker != nil {
				lyricsFetchRevisionWorker.Drain()
			}
			if lyricsDiscoveryWorker != nil {
				lyricsDiscoveryWorker.Drain()
			}
			if err := collabService.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("shutdown lyrics collaboration: %v", err)
			}
			return httpServer.Shutdown(ctx)
		},
		func() {
			// Hard cancellation starts together so no worker loses the remaining
			// total budget behind another component's Wait.
			if lyricsFetchRevisionWorker != nil {
				lyricsFetchRevisionWorker.Cancel()
			}
			if lyricsDiscoveryWorker != nil {
				lyricsDiscoveryWorker.Cancel()
			}
			tr.Cancel()
			backupMgr.Cancel()
			watcher.Stop()
			idx.Stop()
			fileService.Stop()
			hub.Close()
			appLifecycle.StopProbes()
			if err := httpServer.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("force-close HTTP: %v", err)
			}
		},
		func() error {
			if !serveResultConsumed {
				serveErr = <-serverErr
			}
			if lyricsDiscoveryWorker != nil {
				lyricsDiscoveryWorker.Wait()
			}
			if lyricsFetchRevisionWorker != nil {
				lyricsFetchRevisionWorker.Wait()
			}
			fileService.Wait()
			idx.Wait()
			watcher.Wait()
			backupMgr.Wait()
			tr.Wait()
			appLifecycle.Wait()
			return nil
		})
	if shutdownErr != nil {
		log.Printf("shutdown completed after forced cancellation: %v", shutdownErr)
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		_ = database.Close()
		_ = instanceOwner.Close()
		fatal("listen", serveErr)
	}
}

func rejectIncompleteSeed(databasePath string) error {
	marker := databasePath + ".seed-incomplete"
	if _, err := os.Stat(marker); err == nil {
		return fmt.Errorf("incomplete seed publication marker exists: %s", marker)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect incomplete seed publication marker: %w", err)
	}
	return nil
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

func registerOperationalRoutes(mux *http.ServeMux, database *db.DB, authSvc *auth.Auth, projections ...interface {
	Status() filesvc.ProjectionStatus
}) {
	registerOperationalRoutesWithLifecycle(mux, database, authSvc, nil, projections...)
}

func registerOperationalRoutesWithLifecycle(mux *http.ServeMux, database *db.DB, authSvc *auth.Auth, draining interface {
	IsDraining() bool
}, projections ...interface {
	Status() filesvc.ProjectionStatus
}) {
	var projection interface {
		Status() filesvc.ProjectionStatus
	}
	if len(projections) > 0 {
		projection = projections[0]
	}
	registerOperationalRoutesWithProviders(mux, database, authSvc, draining, projection, nil)
}

func registerOperationalRoutesWithSearch(mux *http.ServeMux, database *db.DB, authSvc *auth.Auth, draining interface {
	IsDraining() bool
}, projection interface {
	Status() filesvc.ProjectionStatus
}, search interface {
	Status() searchindex.Status
}) {
	registerOperationalRoutesWithProviders(mux, database, authSvc, draining, projection, search)
}

func registerOperationalRoutesWithProviders(mux *http.ServeMux, database *db.DB, authSvc *auth.Auth, draining interface {
	IsDraining() bool
}, projection interface {
	Status() filesvc.ProjectionStatus
}, search interface {
	Status() searchindex.Status
}) {
	mux.HandleFunc("/healthz", operationalGetOnly(func(w http.ResponseWriter, r *http.Request) {
		setOperationalHeaders(w.Header())
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	mux.HandleFunc("/readyz", operationalGetOnly(func(w http.ResponseWriter, r *http.Request) {
		setOperationalHeaders(w.Header())
		if draining != nil && draining.IsDraining() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := database.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		if projection == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		projectionStatus := projection.Status()
		if projectionStatus.Generation == 0 || projectionStatus.Pending || projectionStatus.LastError != "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		if search != nil && !search.Status().Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		// Status can change while the database probe and provider checks run.
		// Recheck every volatile readiness dependency immediately before 200.
		projectionStatus = projection.Status()
		if (draining != nil && draining.IsDraining()) || projectionStatus.Generation == 0 ||
			projectionStatus.Pending || projectionStatus.LastError != "" ||
			(search != nil && !search.Status().Ready) {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ready"}`)
	}))
	details := authSvc.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		detail := map[string]any{
			"status": "ok",
			"requests": map[string]uint64{
				"total": httpRequestTotal.Load(), "clientErrors": httpClientErrors.Load(),
				"serverErrors": httpServerErrors.Load(),
			},
		}
		if search != nil {
			detail["search"] = search.Status()
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

func setOperationalHeaders(headers http.Header) {
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
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

// seedConfigFromEnv writes settings from env vars on first run only, leaving the
// admin UI authoritative thereafter.
func seedConfigFromEnv(cfg *config.Config) error {
	seed := map[string]string{
		config.KeyLLMType:                         os.Getenv("LLM_TYPE"),
		config.KeyGeminiAPIKey:                    os.Getenv("GEMINI_API_KEY"),
		config.KeyGeminiModel:                     os.Getenv("GEMINI_MODEL"),
		config.KeyOpenAIAPIKey:                    os.Getenv("OPENAI_API_KEY"),
		config.KeyOpenAIBaseURL:                   os.Getenv("OPENAI_BASE_URL"),
		config.KeyOpenAIModel:                     os.Getenv("OPENAI_MODEL"),
		config.KeyLLMRequestTimeoutMS:             envOr("LLM_REQUEST_TIMEOUT_MS", "45000"),
		config.KeyLLMMaxRetries:                   envOr("LLM_MAX_RETRIES", "2"),
		config.KeyBatchSize:                       envOr("TRANSLATE_BATCH_SIZE", "20"),
		config.KeyRateDelayMS:                     envOr("TRANSLATE_RATE_DELAY_MS", "800"),
		config.KeyUpstreamRepo:                    envOr("UPSTREAM_REPO", "Team-Haruki/haruki-sekai-master"),
		config.KeyUpstreamBranch:                  envOr("UPSTREAM_BRANCH", "main"),
		config.KeyUpstreamVersionURL:              os.Getenv("UPSTREAM_VERSION_URL"),
		config.KeyUpstreamVersionFallbackURL:      os.Getenv("UPSTREAM_VERSION_FALLBACK_URL"),
		config.KeyUpstreamJPMasterdataURL:         os.Getenv("UPSTREAM_JP_MASTERDATA_URL"),
		config.KeyUpstreamJPMasterdataFallbackURL: os.Getenv("UPSTREAM_JP_MASTERDATA_FALLBACK_URL"),
		config.KeyUpstreamCNMasterdataURL:         os.Getenv("UPSTREAM_CN_MASTERDATA_URL"),
		config.KeyUpstreamCNMasterdataFallbackURL: os.Getenv("UPSTREAM_CN_MASTERDATA_FALLBACK_URL"),
		config.KeyUpstreamJPAssetsURL:             os.Getenv("UPSTREAM_JP_ASSETS_URL"),
		config.KeyUpstreamJPAssetsFallbackURL:     os.Getenv("UPSTREAM_JP_ASSETS_FALLBACK_URL"),
		config.KeyUpstreamCNAssetsURL:             os.Getenv("UPSTREAM_CN_ASSETS_URL"),
		config.KeyUpstreamCNAssetsFallbackURL:     os.Getenv("UPSTREAM_CN_ASSETS_FALLBACK_URL"),
		config.KeyUpstreamFetchConcurrency:        os.Getenv("UPSTREAM_FETCH_CONCURRENCY"),
		config.KeyMusicAliasesURL:                 envOr("MUSIC_ALIASES_URL", searchindex.DefaultMusicAliasesURL),
		config.KeySchedulerOn:                     envOr("TRANSLATE_SCHEDULER_ENABLED", "false"),
		config.KeyLyricsDiscoveryOn:               envOr("LYRICS_DISCOVERY_ENABLED", "false"),
		config.KeyLyricsFetchRevisionOn:           envOr("LYRICS_FETCH_REVISION_ENABLED", "false"),
		config.KeyBackupGitRepoURL:                os.Getenv("GIT_PUSH_REPO_URL"),
		config.KeyBackupGitBranch:                 envOr("GIT_PUSH_BRANCH", "backup-translations"),
		config.KeyBackupS3Bucket:                  os.Getenv("BACKUP_S3_BUCKET"),
		config.KeyBackupS3Region:                  os.Getenv("BACKUP_S3_REGION"),
		config.KeyBackupS3Endpoint:                os.Getenv("BACKUP_S3_ENDPOINT"),
		config.KeyBackupS3AccessKey:               os.Getenv("BACKUP_S3_ACCESS_KEY"),
		config.KeyBackupS3SecretKey:               os.Getenv("BACKUP_S3_SECRET_KEY"),
	}
	for key, value := range seed {
		if config.IsSecret(key) && !cfg.HasMasterKey() {
			delete(seed, key)
			continue
		}
		seed[key] = value
	}
	seeded, err := cfg.SetManyIfAbsent(seed)
	if err != nil {
		return err
	}
	if seeded > 0 {
		log.Printf("[config] seeded %d settings from environment", seeded)
	}
	return nil
}

// seedAdminFromEnv guarantees an administrator from TRANSLATOR_ACCOUNTS (legacy
// "user:pass,user2:pass2") or ADMIN_USER/ADMIN_PASSWORD when none exists.
func seedAdminFromEnv(a *auth.Auth) error {
	adminCount, err := a.CountAdmins()
	if err != nil || adminCount > 0 {
		return err
	}
	userCount, err := a.CountUsers()
	if err != nil {
		return err
	}
	created := 0
	adminCreated := false
	if accts := os.Getenv("TRANSLATOR_ACCOUNTS"); accts != "" {
		for _, pair := range strings.Split(accts, ",") {
			parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
			if len(parts) != 2 {
				continue
			}
			if err := validateBootstrapAdminPassword(parts[1]); err != nil {
				return err
			}
			role := auth.RoleEditor
			if !adminCreated {
				role = auth.RoleAdmin
			}
			if _, err := a.CreateUser(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), role); err == nil {
				created++
				adminCreated = adminCreated || role == auth.RoleAdmin
			}
		}
	}
	if !adminCreated {
		user := envOr("ADMIN_USER", "admin")
		pass := envOr("ADMIN_PASSWORD", "")
		if pass != "" {
			if err := validateBootstrapAdminPassword(pass); err != nil {
				return err
			}
			if _, err := a.CreateUser(user, pass, auth.RoleAdmin); err == nil {
				created++
				adminCreated = true
				log.Printf("[auth] created initial admin %q from env", user)
			}
		}
	}
	if created > 0 {
		log.Printf("[auth] seeded %d account(s) from environment", created)
	}
	if !adminCreated && userCount > 0 {
		return fmt.Errorf("database contains users but no administrator; provide a unique strong ADMIN_USER/ADMIN_PASSWORD")
	}
	return nil
}

func validateBootstrapAdminPassword(value string) error {
	if strings.TrimSpace(value) == publishedAdminPasswordTemplate {
		return errors.New("published ADMIN_PASSWORD template must be replaced with unique secret material")
	}
	return nil
}

func resolveProductionMode(value string) (bool, error) {
	if runtimeProfile == runtimeProfileNextProduction && value != "true" {
		return false, fmt.Errorf(`standalone production binary requires MOESEKAI_PRODUCTION to remain exactly "true"`)
	}
	return parseProductionMode(value)
}

func validateRuntimeTimezone(production bool, value string, configured bool) error {
	if production && (!configured || value != "UTC") {
		return errors.New(`standalone production binary requires TZ to remain exactly "UTC"`)
	}
	return nil
}

func resolveWebDir(production bool, value string, configured bool) (string, error) {
	if production {
		if !configured || value != "/app/web" {
			return "", errors.New(`standalone production binary requires WEB_DIR to remain exactly "/app/web"`)
		}
		return value, nil
	}
	if value == "" {
		return "./web", nil
	}
	return value, nil
}

func resolveDBPath(production bool, value string, configured bool) (string, error) {
	if production {
		if !configured || value != "/data/moesekai.db" {
			return "", errors.New(`standalone production binary requires DB_PATH to remain exactly "/data/moesekai.db"`)
		}
		return value, nil
	}
	if value == "" {
		return "./data/moesekai.db", nil
	}
	return value, nil
}

func resolveDataDir(production bool, value string, configured bool) (string, error) {
	if production {
		if !configured || value != "/data" {
			return "", errors.New(`standalone production binary requires DATA_DIR to remain exactly "/data"`)
		}
		return value, nil
	}
	if value == "" {
		return "./data", nil
	}
	return value, nil
}

func parseProductionMode(value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	production, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("must be true or false")
	}
	return production, nil
}

func validateProductionAdmin(production bool, a *auth.Auth) error {
	if !production {
		return nil
	}
	admins, err := a.CountAdmins()
	if err != nil {
		return fmt.Errorf("count administrators: %w", err)
	}
	if admins == 0 {
		return fmt.Errorf("an initialized administrator is required; provide ADMIN_PASSWORD or TRANSLATOR_ACCOUNTS for first boot")
	}
	return nil
}

func validateProductionMasterKey(production bool, masterKey string) error {
	masterKey = strings.TrimSpace(masterKey)
	if production && (len([]byte(masterKey)) < 32 || masterKey == "replace-with-at-least-32-random-bytes") {
		return fmt.Errorf("MOESEKAI_MASTER_KEY must contain at least 32 bytes of non-template secret material")
	}
	return nil
}

func validateConsoleOrigin(production bool, origin string) error {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return fmt.Errorf("must be * or one absolute http(s) origin")
	}
	if origin == "*" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return fmt.Errorf("must be * or one absolute http(s) origin without credentials, path, query, fragment, or trailing slash")
	}
	if production && parsed.Scheme == "http" && !isLoopbackOriginHost(parsed.Hostname()) {
		return fmt.Errorf("production console origin must use https unless it is a loopback host")
	}
	return nil
}

func isLoopbackOriginHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	address := net.ParseIP(strings.TrimSpace(host))
	return address != nil && address.IsLoopback()
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseTTL(hours string) (time.Duration, error) {
	const maxTokenTTLHours = 24 * 30
	hours = strings.TrimSpace(hours)
	n, err := strconv.Atoi(hours)
	if err != nil || n <= 0 || n > maxTokenTTLHours {
		return 0, fmt.Errorf("must be a canonical integer from 1 to %d hours", maxTokenTTLHours)
	}
	if strconv.Itoa(n) != hours {
		return 0, fmt.Errorf("must be a canonical integer from 1 to %d hours", maxTokenTTLHours)
	}
	return time.Duration(n) * time.Hour, nil
}

func durationEnvMs(name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw, configured := os.LookupEnv(name)
	if !configured || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("must be a positive integer number of milliseconds")
	}
	if n > int64(^uint64(0)>>1)/int64(time.Millisecond) {
		return 0, fmt.Errorf("duration overflows")
	}
	duration := time.Duration(n) * time.Millisecond
	if duration < minimum || duration > maximum {
		return 0, fmt.Errorf("must be between %d and %d milliseconds", minimum.Milliseconds(), maximum.Milliseconds())
	}
	return duration, nil
}

func boundedIntEnv(name string, fallback, minimum, maximum int) (int, error) {
	raw, configured := os.LookupEnv(name)
	if !configured || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	n, err := strconv.Atoi(value)
	if err != nil || strconv.Itoa(n) != value || n < minimum || n > maximum {
		return 0, fmt.Errorf("%s must be a canonical integer from %d to %d", name, minimum, maximum)
	}
	return n, nil
}

func lyricsDiscoveryOptionsFromEnv() (lyricsdiscovery.Options, error) {
	scanInterval, err := durationEnvMs("LYRICS_DISCOVERY_SCAN_MS", 6*time.Hour, time.Minute, 7*24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	leaseDuration, err := durationEnvMs("LYRICS_DISCOVERY_LEASE_MS", 2*time.Minute, 10*time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	idleWait, err := durationEnvMs("LYRICS_DISCOVERY_IDLE_MS", 2*time.Second, 100*time.Millisecond, time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMin, err := durationEnvMs("LYRICS_DISCOVERY_RETRY_MIN_MS", 30*time.Second, time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMax, err := durationEnvMs("LYRICS_DISCOVERY_RETRY_MAX_MS", 30*time.Minute, time.Second, 30*24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	jobTimeout, err := durationEnvMs("LYRICS_DISCOVERY_JOB_TIMEOUT_MS", 30*time.Second, time.Second, 10*time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	concurrency, err := boundedIntEnv("LYRICS_DISCOVERY_CONCURRENCY", 4, 1, 16)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	if retryMax < retryMin {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_DISCOVERY_RETRY_MAX_MS must be greater than or equal to LYRICS_DISCOVERY_RETRY_MIN_MS")
	}
	if jobTimeout >= leaseDuration {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_DISCOVERY_JOB_TIMEOUT_MS must be shorter than LYRICS_DISCOVERY_LEASE_MS")
	}
	return lyricsdiscovery.Options{
		ScanInterval: scanInterval, LeaseDuration: leaseDuration, IdleWait: idleWait,
		RetryMin: retryMin, RetryMax: retryMax, JobTimeout: jobTimeout, Concurrency: concurrency,
	}, nil
}

func lyricsFetchRevisionOptionsFromEnv() (lyricsdiscovery.Options, error) {
	leaseDuration, err := durationEnvMs("LYRICS_FETCH_REVISION_LEASE_MS", 2*time.Minute, 10*time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	idleWait, err := durationEnvMs("LYRICS_FETCH_REVISION_IDLE_MS", 2*time.Second, 100*time.Millisecond, time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMin, err := durationEnvMs("LYRICS_FETCH_REVISION_RETRY_MIN_MS", 30*time.Second, time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMax, err := durationEnvMs("LYRICS_FETCH_REVISION_RETRY_MAX_MS", 30*time.Minute, time.Second, 30*24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	jobTimeout, err := durationEnvMs("LYRICS_FETCH_REVISION_JOB_TIMEOUT_MS", 30*time.Second, time.Second, 10*time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	concurrency, err := boundedIntEnv("LYRICS_FETCH_REVISION_CONCURRENCY", 4, 1, 16)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	if retryMax < retryMin {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_FETCH_REVISION_RETRY_MAX_MS must be greater than or equal to LYRICS_FETCH_REVISION_RETRY_MIN_MS")
	}
	if jobTimeout >= leaseDuration {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_FETCH_REVISION_JOB_TIMEOUT_MS must be shorter than LYRICS_FETCH_REVISION_LEASE_MS")
	}
	return lyricsdiscovery.Options{ScanInterval: time.Hour, LeaseDuration: leaseDuration, IdleWait: idleWait,
		RetryMin: retryMin, RetryMax: retryMax, JobTimeout: jobTimeout, Concurrency: concurrency}, nil
}

func shutdownConfigFromEnv() (lifecycle.ShutdownConfig, error) {
	parse := func(name string, fallback int) (time.Duration, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			value = strconv.Itoa(fallback)
		}
		milliseconds, err := strconv.Atoi(value)
		if err != nil || milliseconds <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer number of milliseconds", name)
		}
		return time.Duration(milliseconds) * time.Millisecond, nil
	}
	budget, err := parse("SHUTDOWN_BUDGET_MS", 25000)
	if err != nil {
		return lifecycle.ShutdownConfig{}, err
	}
	drain, err := parse("SHUTDOWN_DRAIN_MS", 2000)
	if err != nil {
		return lifecycle.ShutdownConfig{}, err
	}
	if drain >= budget {
		return lifecycle.ShutdownConfig{}, fmt.Errorf("SHUTDOWN_DRAIN_MS must be shorter than SHUTDOWN_BUDGET_MS")
	}
	return lifecycle.ShutdownConfig{Budget: budget, Drain: drain}, nil
}

func fatal(ctx string, err error) {
	fmt.Fprintf(os.Stderr, "Fatal: %s: %v\n", ctx, err)
	os.Exit(1)
}
