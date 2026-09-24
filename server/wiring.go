package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/api"
	"moesekai/server/internal/auth"
	"moesekai/server/internal/backup"
	"moesekai/server/internal/collab"
	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/files"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lifecycle"
	"moesekai/server/internal/lyricsdiscovery"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/searchindex"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
	"moesekai/server/internal/upstream"
)

// services are the long-lived components the process serves requests with and
// shuts down again. The lyrics workers are nil unless their feature is enabled.
type services struct {
	database        *db.DB
	auth            *auth.Auth
	cfg             *config.Config
	hub             *sse.Hub
	lifecycle       *lifecycle.State
	editorGate      *editorgate.Gate
	translator      *translator.Translator
	sideStory       *translator.SideStoryBackfill
	upstream        *upstream.Watcher
	backup          *backup.Manager
	collab          *collab.Service
	files           *filesvc.Service
	search          *searchindex.Builder
	api             *api.Server
	lyricsDiscovery *lyricsdiscovery.Worker
	lyricsFetch     *lyricsdiscovery.FetchWorker
}

func buildServices(env startupEnv, settings runtimeSettings, database *db.DB) *services {
	st := store.New(database)
	applyEmbeddedLyricsEditorSeed(st)

	tokenTTL, err := parseTTL(envOr("TOKEN_TTL_HOURS", "168"))
	if err != nil {
		fatal("TOKEN_TTL_HOURS", err)
	}
	authSvc := auth.New(database, settings.jwtSecret, tokenTTL)
	if err := authSvc.ValidatePersistedRoles(); err != nil {
		fatal("validate persisted user roles", err)
	}

	cfg, err := config.New(database, settings.masterKey)
	if err != nil {
		fatal("init config", err)
	}
	if err := seedConfigFromEnv(cfg); err != nil {
		fatal("seed configuration", err)
	}

	if err := seedAdminFromEnv(authSvc); err != nil {
		fatal("seed initial admin", err)
	}
	if err := validateProductionAdmin(env.production, authSvc); err != nil {
		fatal("production startup validation", err)
	}

	es := store.NewEventStore(database)
	lyricsDiscoveryWorker, lyricsFetchRevisionWorker := newLyricsWorkers(st, cfg)
	gen := files.NewGenerator(st, es, env.dataDir)
	fileService := newFileService(st, es, gen)
	idx := newSearchIndex(st, fileService, cfg, env)

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
	sideStoryOptions, err := sideStoryBackfillOptionsFromEnv()
	if err != nil {
		fatal("side story backfill configuration", err)
	}
	sideStory := translator.NewSideStoryBackfill(tr, sideStoryOptions)

	watcher := newUpstreamWatcher(cfg, tr, env.dataDir)
	// Backup manager: daily + manual backup/restore to S3 and/or GitHub.
	backupMgr := backup.NewManager(cfg, gen, st, es, filepath.Join(env.dataDir, "backup-work"))
	collabService, err := collab.New(database, st, authSvc, editorGate, settings.allowOrigin)
	if err != nil {
		fatal("init lyrics collaboration", err)
	}
	backupMgr.SetAfterRestore(afterContentRestore(collabService, sideStory))

	apiServer := api.NewServer(st, es, authSvc, cfg, hub, tr, watcher, backupMgr, editorGate)
	apiServer.SetCollab(collabService)
	apiServer.SetFileService(fileService)
	apiServer.SetSearchStatus(idx)
	apiServer.SetSideStoryRunner(sideStory)

	return &services{
		database: database, auth: authSvc, cfg: cfg, hub: hub, lifecycle: appLifecycle,
		editorGate: editorGate, translator: tr, sideStory: sideStory, upstream: watcher, backup: backupMgr, collab: collabService,
		files: fileService, search: idx, api: apiServer,
		lyricsDiscovery: lyricsDiscoveryWorker, lyricsFetch: lyricsFetchRevisionWorker,
	}
}

// afterContentRestore retires the realtime sessions that predate a restore and
// asks the side-story backfill for a catalog refresh, which it otherwise
// makes only every 6 h, so restored side-story tables are resynced promptly.
func afterContentRestore(collabService *collab.Service, sideStory *translator.SideStoryBackfill) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := collabService.RetireAll(ctx); err != nil {
			log.Printf("[collab] post-restore retirement failed: %v", err)
		}
		sideStory.TriggerSideStoryBackfill(true)
	}
}

// sideStoryBackfillOptionsFromEnv reads the card and area story backfill env;
// the worker still runs only while the translate scheduler is enabled.
func sideStoryBackfillOptionsFromEnv() (translator.SideStoryBackfillOptions, error) {
	enabled := true
	if raw := strings.TrimSpace(os.Getenv("SIDE_STORY_BACKFILL_ENABLED")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return translator.SideStoryBackfillOptions{}, errors.New("SIDE_STORY_BACKFILL_ENABLED must be true or false")
		}
		enabled = value
	}
	interval, err := durationEnvMs("SIDE_STORY_BACKFILL_INTERVAL_MS", time.Minute, time.Second, 24*time.Hour)
	if err != nil {
		return translator.SideStoryBackfillOptions{}, fmt.Errorf("SIDE_STORY_BACKFILL_INTERVAL_MS %w", err)
	}
	batch, err := boundedIntEnv("SIDE_STORY_BACKFILL_BATCH", 30, 1, 500)
	if err != nil {
		return translator.SideStoryBackfillOptions{}, err
	}
	delay, err := durationEnvMs("SIDE_STORY_BACKFILL_REQUEST_DELAY_MS", time.Second, 100*time.Millisecond, time.Minute)
	if err != nil {
		return translator.SideStoryBackfillOptions{}, fmt.Errorf("SIDE_STORY_BACKFILL_REQUEST_DELAY_MS %w", err)
	}
	return translator.SideStoryBackfillOptions{Enabled: enabled, Interval: interval, Batch: batch, RequestDelay: delay}, nil
}

func newLyricsWorkers(st *store.Store, cfg *config.Config) (*lyricsdiscovery.Worker, *lyricsdiscovery.FetchWorker) {
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
	return lyricsDiscoveryWorker, lyricsFetchRevisionWorker
}

func newFileService(st *store.Store, es *store.EventStore, gen *files.Generator) *filesvc.Service {
	fileService := filesvc.New(st, es, gen)
	filesDebounce, err := durationEnvMs("FILES_REBUILD_DEBOUNCE_MS", 5*time.Minute, 100*time.Millisecond, 24*time.Hour)
	if err != nil {
		fatal("FILES_REBUILD_DEBOUNCE_MS", err)
	}
	fileService.SetDebounce(filesDebounce)
	// Regenerate public files whenever the DB changes (debounced inside).
	st.OnChange(fileService.Trigger)
	return fileService
}

func newSearchIndex(st *store.Store, fileService *filesvc.Service, cfg *config.Config, env startupEnv) *searchindex.Builder {
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
	if env.production {
		idx.UseProductionCoverageFloors()
	}
	idx.SetCachePath(filepath.Join(env.dataDir, "search-index-cache.json"))
	st.OnChange(idx.Trigger)
	return idx
}

// newUpstreamWatcher polls current_version.json directly (not the GitHub REST
// API), backs off on raw-content 429s, and triggers CN sync on change.
func newUpstreamWatcher(cfg *config.Config, tr *translator.Translator, dataDir string) *upstream.Watcher {
	useGit := envOr("UPSTREAM_USE_GIT", "false") == "true"
	upstreamPoll, err := durationEnvMs("UPSTREAM_POLL_MS", time.Hour, time.Second, 24*time.Hour)
	if err != nil {
		fatal("UPSTREAM_POLL_MS", err)
	}
	return upstream.NewWithContext(cfg, func(ctx context.Context) error {
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
}

func (s *services) startWorkers() {
	s.files.Start()
	s.search.Start()
	s.upstream.Start()
	s.backup.StartScheduler()
	s.sideStory.Start()
	if s.lyricsFetch != nil {
		if err := s.lyricsFetch.Start(context.Background()); err != nil {
			fatal("start lyrics source fetch worker", err)
		}
	}
	if s.lyricsDiscovery != nil {
		if err := s.lyricsDiscovery.Start(context.Background()); err != nil {
			fatal("start lyrics discovery worker", err)
		}
	}
}
