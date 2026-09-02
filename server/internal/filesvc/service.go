// Package filesvc serves the public, CDN-cacheable translation files under
// /files/*. Content is generated from the DB and held in memory with strong
// ETags; regeneration is debounced and triggered by DB changes. Responses carry
// long max-age + stale-while-revalidate so a CDN can cache aggressively while
// the ETag lets clients revalidate cheaply.
package filesvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/files"
	"moesekai/server/internal/model"
	"moesekai/server/internal/publiclyricsbundle"
	"moesekai/server/internal/store"
)

type assetSource string

const (
	sourceBundle                 assetSource = "bundle"
	sourceDBPublication          assetSource = "db_publication"
	sourceLocalizationProjection assetSource = "localization_projection"
	sourceGenerated              assetSource = "generated"
)

type asset struct {
	body        []byte
	etag        string
	contentType string
	modTime     time.Time
	source      assetSource
	revision    int
}

type SongProvenance struct {
	MusicID           int      `json:"musicId"`
	Source            string   `json:"source"`
	Revision          int      `json:"revision"`
	State             string   `json:"state"`
	AvailableVersions []string `json:"availableVersions"`
	UpdatedAt         string   `json:"updatedAt"`
	HasDetail         bool     `json:"hasDetail"`
}

type LyricsProjectionSummary struct {
	TotalSongs         int    `json:"totalSongs"`
	BundleSongs        int    `json:"bundleSongs"`
	DBPublicationSongs int    `json:"dbPublicationSongs"`
	LocalizationSongs  int    `json:"localizationSongs"`
	Degraded           bool   `json:"degraded"`
	DegradedReason     string `json:"degradedReason,omitempty"`
	BundleReleaseID    string `json:"bundleReleaseId,omitempty"`
}

// ProjectionStatus distinguishes durable database writes from publication of a
// complete regenerated public asset set.
type ProjectionStatus struct {
	Generation    uint64                  `json:"generation"`
	Pending       bool                    `json:"pending"`
	LastSuccessAt string                  `json:"lastSuccessAt,omitempty"`
	LastError     string                  `json:"lastError,omitempty"`
	LyricsSummary LyricsProjectionSummary `json:"lyricsSummary"`
}

// Service holds generated assets in memory and serves them.
type Service struct {
	gen             *files.Generator
	store           *store.Store
	events          *store.EventStore
	publicLyrics    map[string][]byte
	publicLyricsErr error
	maxAge          time.Duration
	lyricsMaxAge    time.Duration
	swr             time.Duration
	debounce        time.Duration

	mu         sync.RWMutex
	assets     map[string]asset // path key e.g. "translation/cards.json"
	provenance map[int]SongProvenance
	rebuildMu  sync.Mutex
	statusMu   sync.RWMutex
	status     ProjectionStatus
	requested  uint64
	published  uint64
	running    bool

	rebuildCh       chan struct{}
	immediateCh     chan struct{}
	rebuildAssetsFn func() error
	retryMin        time.Duration
	retryMax        time.Duration
	ctx             context.Context
	cancel          context.CancelFunc
	startOnce       sync.Once
	stopOnce        sync.Once
	wg              sync.WaitGroup
}

func New(s *store.Store, es *store.EventStore, gen *files.Generator) *Service {
	publicLyrics, publicLyricsErr := publiclyricsbundle.Load()
	ctx, cancel := context.WithCancel(context.Background())

	var initialSummary LyricsProjectionSummary
	var initialProvenance map[int]SongProvenance
	if publicLyrics != nil {
		if bundleIndex, err := decodePublicLyricsIndex(publicLyrics[publicLyricsIndexKey]); err == nil {
			initialProvenance = make(map[int]SongProvenance, len(bundleIndex.Songs))
			for _, song := range bundleIndex.Songs {
				initialProvenance[song.MusicID] = SongProvenance{
					MusicID:           song.MusicID,
					Source:            string(sourceBundle),
					Revision:          song.Revision,
					State:             string(song.State),
					AvailableVersions: append([]string(nil), song.AvailableVersions...),
					UpdatedAt:         song.UpdatedAt,
					HasDetail:         song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly,
				}
			}
			initialSummary = LyricsProjectionSummary{
				TotalSongs:         len(bundleIndex.Songs),
				BundleSongs:        len(bundleIndex.Songs),
				DBPublicationSongs: 0,
				LocalizationSongs:  0,
				Degraded:           false,
				BundleReleaseID:    publiclyricsbundle.ReleaseID,
			}
		}
	}

	svc := &Service{
		gen:             gen,
		store:           s,
		events:          es,
		publicLyrics:    publicLyrics,
		publicLyricsErr: publicLyricsErr,
		maxAge:          5 * time.Minute,
		lyricsMaxAge:    15 * time.Second,
		swr:             time.Hour,
		debounce:        5 * time.Minute,
		assets:          map[string]asset{},
		provenance:      initialProvenance,
		status: ProjectionStatus{
			LyricsSummary: initialSummary,
		},
		rebuildCh:   make(chan struct{}, 1),
		immediateCh: make(chan struct{}, 1),
		retryMin:    time.Second,
		retryMax:    30 * time.Second,
		ctx:         ctx,
		cancel:      cancel,
	}
	svc.rebuildAssetsFn = svc.rebuildAssets
	return svc
}

// Start launches the tracked publication worker and returns without waiting for
// the initial generation. Readiness remains false until that worker succeeds.
func (svc *Service) Start() {
	svc.startOnce.Do(func() {
		if svc.ctx.Err() != nil {
			return
		}
		svc.statusMu.Lock()
		if svc.requested <= svc.published {
			svc.requested = svc.published + 1
		}
		svc.statusMu.Unlock()
		svc.wg.Add(1)
		go func() {
			defer svc.wg.Done()
			svc.loop()
		}()
	})
}

// Stop cancels pending retries and debounced work. Wait must be called before
// closing SQLite to ensure an active generation has returned.
func (svc *Service) Stop() { svc.stopOnce.Do(svc.cancel) }

func (svc *Service) Wait() { svc.wg.Wait() }

// SetDebounce updates the debounce duration for publication runs.
func (svc *Service) SetDebounce(d time.Duration) {
	svc.statusMu.Lock()
	svc.debounce = d
	svc.statusMu.Unlock()
}

// Trigger schedules a debounced rebuild (safe to call from DB change hooks).
func (svc *Service) Trigger() {
	if svc.ctx.Err() != nil {
		return
	}
	svc.statusMu.Lock()
	svc.requested++
	svc.statusMu.Unlock()
	select {
	case svc.rebuildCh <- struct{}{}:
	default:
	}
}

// PublishNow bypasses the debounce window and immediately requests a full rebuild.
func (svc *Service) PublishNow() {
	if svc.ctx.Err() != nil {
		return
	}
	svc.statusMu.Lock()
	svc.requested++
	svc.statusMu.Unlock()
	select {
	case svc.immediateCh <- struct{}{}:
	default:
	}
	select {
	case svc.rebuildCh <- struct{}{}:
	default:
	}
}

// Status returns a race-safe projection publication snapshot.
func (svc *Service) Status() ProjectionStatus {
	svc.statusMu.RLock()
	defer svc.statusMu.RUnlock()
	status := svc.status
	status.Generation = svc.published
	status.Pending = svc.running || svc.requested > svc.published
	return status
}

// SongProvenance returns the runtime source and revision metadata for a song.
func (svc *Service) SongProvenance(musicID int) (SongProvenance, bool) {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if svc.provenance == nil {
		return SongProvenance{}, false
	}
	p, ok := svc.provenance[musicID]
	if !ok {
		return SongProvenance{}, false
	}
	p.AvailableVersions = append([]string(nil), p.AvailableVersions...)
	return p, true
}

func (svc *Service) loop() {
	retryMin, retryMax := svc.retryBounds()
	retryDelay := retryMin
	err := svc.rebuild(svc.ctx)
	svc.logRebuildError(err)
	for {
		if svc.ctx.Err() != nil {
			return
		}
		immediate := svc.drainRebuildNotifications()
		pending := svc.hasPendingPublication()
		switch {
		case err != nil && pending:
			if !svc.waitForRetry(retryDelay) {
				return
			}
			if retryDelay < retryMax-retryDelay {
				retryDelay *= 2
			} else {
				retryDelay = retryMax
			}
		case pending:
			retryDelay = retryMin
			if !immediate && !svc.waitForDebounce() {
				return
			}
		default:
			retryDelay = retryMin
			select {
			case <-svc.ctx.Done():
				return
			case <-svc.immediateCh:
				immediate = true
			case <-svc.rebuildCh:
			}
			if !immediate && !svc.waitForDebounce() {
				return
			}
		}
		err = svc.rebuild(svc.ctx)
		svc.logRebuildError(err)
	}
}

func (svc *Service) retryBounds() (time.Duration, time.Duration) {
	minimum := svc.retryMin
	if minimum <= 0 {
		minimum = time.Second
	}
	maximum := svc.retryMax
	if maximum < minimum {
		maximum = minimum
	}
	return minimum, maximum
}

func (svc *Service) waitForRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-svc.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (svc *Service) waitForDebounce() bool {
	if svc.debounce <= 0 {
		return svc.ctx.Err() == nil
	}
	timer := time.NewTimer(svc.debounce)
	defer timer.Stop()
	for {
		select {
		case <-svc.ctx.Done():
			return false
		case <-svc.immediateCh:
			return true
		case <-svc.rebuildCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(svc.debounce)
		case <-timer.C:
			return true
		}
	}
}

func (svc *Service) drainRebuildNotifications() bool {
	immediate := false
	for {
		select {
		case <-svc.immediateCh:
			immediate = true
		case <-svc.rebuildCh:
		default:
			return immediate
		}
	}
}

func (svc *Service) hasPendingPublication() bool {
	svc.statusMu.RLock()
	defer svc.statusMu.RUnlock()
	return svc.requested > svc.published
}

func (svc *Service) logRebuildError(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[projection] generation failed; retrying: %v", err)
	}
}

// Rebuild regenerates all in-memory assets from the DB.
func (svc *Service) Rebuild() {
	if err := svc.rebuild(svc.ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[projection] generation failed: %v", err)
		select {
		case svc.rebuildCh <- struct{}{}:
		default:
		}
	}
}

// RebuildEvent incrementally updates in-memory public assets for a single event story.
func (svc *Service) RebuildEvent(eventID int) error {
	return svc.RebuildEventContext(svc.ctx, eventID)
}

// RebuildEventContext incrementally updates in-memory public assets for a single event story with context.
func (svc *Service) RebuildEventContext(ctx context.Context, eventID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	releaseContent, err := svc.store.LockContentSharedContext(ctx)
	if err != nil {
		return err
	}
	defer releaseContent()

	now := time.Now()
	b, err := svc.gen.EventStoryJSON(eventID)
	if err != nil {
		return fmt.Errorf("event %d: %w", eventID, err)
	}
	updates := make(map[string]asset, 1+len(model.SupportedLocales))
	updates[fmt.Sprintf("translation/eventStory/event_%d.json", eventID)] = makeAsset(b, "application/json; charset=utf-8", now)
	for _, locale := range model.SupportedLocales {
		lb, err := svc.gen.EventStoryLocaleJSON(eventID, locale)
		if err != nil {
			return fmt.Errorf("locale event %s/%d: %w", locale, eventID, err)
		}
		updates[fmt.Sprintf("v2/%s/translation/eventStory/event_%d.json", locale, eventID)] = makeAsset(lb, "application/json; charset=utf-8", now)
	}

	svc.mu.Lock()
	for k, v := range updates {
		svc.assets[k] = v
	}
	svc.mu.Unlock()
	return nil
}

// RebuildCategory incrementally updates in-memory public assets for a single category.
func (svc *Service) RebuildCategory(category string) error {
	return svc.RebuildCategoryContext(svc.ctx, category)
}

// RebuildCategoryContext incrementally updates in-memory public assets for a single category with context.
func (svc *Service) RebuildCategoryContext(ctx context.Context, category string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	releaseContent, err := svc.store.LockContentSharedContext(ctx)
	if err != nil {
		return err
	}
	defer releaseContent()

	now := time.Now()
	updates := make(map[string]asset, 2+2*len(model.SupportedLocales))
	flat, err := svc.gen.CategoryFlatJSON(category)
	if err != nil {
		return fmt.Errorf("flat %s: %w", category, err)
	}
	updates["translation/"+category+".json"] = makeAsset(flat, "application/json; charset=utf-8", now)
	full, err := svc.gen.CategoryFullJSON(category)
	if err != nil {
		return fmt.Errorf("full %s: %w", category, err)
	}
	updates["translation/"+category+".full.json"] = makeAsset(full, "application/json; charset=utf-8", now)

	for _, locale := range model.SupportedLocales {
		lflat, err := svc.gen.CategoryLocaleFlatJSON(category, locale)
		if err != nil {
			return fmt.Errorf("locale flat %s/%s: %w", locale, category, err)
		}
		updates[fmt.Sprintf("v2/%s/translation/%s.json", locale, category)] = makeAsset(lflat, "application/json; charset=utf-8", now)
		lfull, err := svc.gen.CategoryLocaleFullJSON(category, locale)
		if err != nil {
			return fmt.Errorf("locale full %s/%s: %w", locale, category, err)
		}
		updates[fmt.Sprintf("v2/%s/translation/%s.full.json", locale, category)] = makeAsset(lfull, "application/json; charset=utf-8", now)
	}

	svc.mu.Lock()
	for k, v := range updates {
		svc.assets[k] = v
	}
	svc.mu.Unlock()
	return nil
}

func (svc *Service) rebuild(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	svc.rebuildMu.Lock()
	defer svc.rebuildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	svc.statusMu.Lock()
	if svc.requested <= svc.published {
		svc.requested = svc.published + 1
	}
	targetGeneration := svc.requested
	svc.running = true
	svc.statusMu.Unlock()

	err := svc.rebuildAssetsFn()
	svc.statusMu.Lock()
	svc.running = false
	if err != nil {
		svc.status.LastError = "projection_generation_failed"
	} else {
		svc.published = targetGeneration
		svc.status.LastSuccessAt = time.Now().UTC().Format(time.RFC3339Nano)
		svc.status.LastError = ""
	}
	svc.statusMu.Unlock()
	return err
}

func (svc *Service) rebuildAssets() error {
	return svc.rebuildAssetsContext(svc.ctx)
}

func (svc *Service) databaseOnlyLyricsSummary(projected map[string][]byte) (LyricsProjectionSummary, map[int]SongProvenance) {
	index, err := decodePublicLyricsIndex(projected[publicLyricsIndexKey])
	if err != nil {
		return LyricsProjectionSummary{
			Degraded:       true,
			DegradedReason: "db_only: database lyrics index decode failed",
		}, nil
	}
	provenance := make(map[int]SongProvenance, len(index.Songs))
	for _, song := range index.Songs {
		provenance[song.MusicID] = SongProvenance{
			MusicID:           song.MusicID,
			Source:            string(sourceDBPublication),
			Revision:          song.Revision,
			State:             string(song.State),
			AvailableVersions: append([]string(nil), song.AvailableVersions...),
			UpdatedAt:         song.UpdatedAt,
			HasDetail:         song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly,
		}
	}
	return LyricsProjectionSummary{
		TotalSongs:         len(index.Songs),
		BundleSongs:        0,
		DBPublicationSongs: len(index.Songs),
		LocalizationSongs:  0,
		Degraded:           false,
	}, provenance
}

func (svc *Service) rebuildAssetsContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if svc.publicLyricsErr != nil {
		return fmt.Errorf("public lyrics bundle: %w", svc.publicLyricsErr)
	}
	releaseContent, err := svc.store.LockContentSharedContext(ctx)
	if err != nil {
		return err
	}
	defer releaseContent()

	next := map[string]asset{}
	now := time.Now()

	for _, cat := range model.SupportedCategories {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := svc.gen.CategoryFlatJSON(cat)
		if err != nil {
			return fmt.Errorf("flat %s: %w", cat, err)
		}
		next["translation/"+cat+".json"] = makeAsset(b, "application/json; charset=utf-8", now)
		b, err = svc.gen.CategoryFullJSON(cat)
		if err != nil {
			return fmt.Errorf("full %s: %w", cat, err)
		}
		next["translation/"+cat+".full.json"] = makeAsset(b, "application/json; charset=utf-8", now)
	}
	for _, locale := range model.SupportedLocales {
		for _, cat := range model.SupportedCategories {
			if err := ctx.Err(); err != nil {
				return err
			}
			b, err := svc.gen.CategoryLocaleFlatJSON(cat, locale)
			if err != nil {
				return fmt.Errorf("locale flat %s/%s: %w", locale, cat, err)
			}
			key := fmt.Sprintf("v2/%s/translation/%s.json", locale, cat)
			next[key] = makeAsset(b, "application/json; charset=utf-8", now)
			b, err = svc.gen.CategoryLocaleFullJSON(cat, locale)
			if err != nil {
				return fmt.Errorf("locale full %s/%s: %w", locale, cat, err)
			}
			key = fmt.Sprintf("v2/%s/translation/%s.full.json", locale, cat)
			next[key] = makeAsset(b, "application/json; charset=utf-8", now)
		}
	}
	summaries, err := svc.events.List()
	if err != nil {
		return fmt.Errorf("event list: %w", err)
	}
	for _, sum := range summaries {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := svc.gen.EventStoryJSON(sum.EventID)
		if err != nil {
			return fmt.Errorf("event %d: %w", sum.EventID, err)
		}
		key := fmt.Sprintf("translation/eventStory/event_%d.json", sum.EventID)
		next[key] = makeAsset(b, "application/json; charset=utf-8", now)
		for _, locale := range model.SupportedLocales {
			b, err := svc.gen.EventStoryLocaleJSON(sum.EventID, locale)
			if err != nil {
				return fmt.Errorf("locale event %s/%d: %w", locale, sum.EventID, err)
			}
			key = fmt.Sprintf("v2/%s/translation/eventStory/event_%d.json", locale, sum.EventID)
			next[key] = makeAsset(b, "application/json; charset=utf-8", now)
		}
	}
	// The accepted recovery-v3 projection is public, immutable release content
	// and remains the reviewed base for every rebuild. Database publications
	// overlay it: newer or bundle-absent database songs replace/add their index
	// entry and their detail bytes, so newly published lyrics reach the public
	// site while the reviewed bundle stays the safety base. A failing
	// database projection keeps the pure bundle bytes (fail-open), and a
	// missing bundle keeps the database-only fallback exactly. Canonical and
	// locale-mirror paths publish in one asset generation.
	var lyrics map[string][]byte
	var lyricsSummary LyricsProjectionSummary
	var lyricsProvenance map[int]SongProvenance

	if svc.publicLyrics == nil {
		lyrics, err = svc.gen.PublishedLyricsJSON()
		if err != nil {
			return fmt.Errorf("lyrics: %w", err)
		}
		lyricsSummary, lyricsProvenance = svc.databaseOnlyLyricsSummary(lyrics)
	} else {
		lyrics, lyricsSummary, lyricsProvenance = svc.overlayPublishedLyrics(svc.publicLyrics)
	}
	for key, body := range lyrics {
		source := sourceGenerated
		rev := 0
		if key == publicLyricsIndexKey {
			source = sourceGenerated
		} else if strings.HasPrefix(key, "translation/lyrics/music_") {
			var musicID int
			if n, _ := fmt.Sscanf(key, "translation/lyrics/music_%d.json", &musicID); n == 1 {
				if p, ok := lyricsProvenance[musicID]; ok {
					source = assetSource(p.Source)
					rev = p.Revision
				}
			}
		}
		lyricsAsset := makeAssetWithSource(body, "application/json; charset=utf-8", now, source, rev)
		next[key] = lyricsAsset
		for _, locale := range model.SupportedLocales {
			localizedKey := fmt.Sprintf("v2/%s/%s", locale, key)
			next[localizedKey] = lyricsAsset
		}
	}

	svc.mu.Lock()
	// Preserve only externally-set assets. Locale mirrors are generated above and
	// must disappear when their source event or lyrics publication disappears.
	for k, v := range svc.assets {
		if _, ok := next[k]; !ok && (k == "data/search-index.json" || k == "v2/data/search-index.json" || strings.HasSuffix(k, "/data/search-index.json")) {
			next[k] = v
		}
	}
	svc.assets = next
	svc.provenance = lyricsProvenance
	svc.mu.Unlock()

	svc.statusMu.Lock()
	svc.status.LyricsSummary = lyricsSummary
	svc.statusMu.Unlock()

	return nil
}

// publicLyricsIndexKey is the canonical lyrics index path shared by the
// reviewed runtime bundle and the database-backed projection.
const publicLyricsIndexKey = "translation/lyrics/index.json"

// overlayPublishedLyrics merges the database-backed lyrics projection onto the
// reviewed runtime bundle. Database songs own the merged index entry when they
// are complete or game_only and newer than the bundle entry, when the bundle
// has no entry, or when their revision is newer than the bundle entry and they
// are not a draft state. Draft and incomplete database states therefore never
// downgrade a reviewed complete or game_only bundle entry. DB-owned detail
// bytes replace the bundle detail exactly. Any database projection or index
// decode failure serves the pure bundle bytes (fail-open to the reviewed
// release) and marks the summary as degraded.
func (svc *Service) overlayPublishedLyrics(bundle map[string][]byte) (map[string][]byte, LyricsProjectionSummary, map[int]SongProvenance) {
	bundleIndex, err := decodePublicLyricsIndex(bundle[publicLyricsIndexKey])
	if err != nil {
		log.Printf("[projection] reviewed bundle index undecodable; serving reviewed bundle: %v", err)
		return bundle, LyricsProjectionSummary{
			Degraded:        true,
			DegradedReason:  "layer1: bundle index decode failed",
			BundleReleaseID: publiclyricsbundle.ReleaseID,
		}, nil
	}

	bundleByID := make(map[int]store.PublicLyricsIndexSong, len(bundleIndex.Songs))
	bundlePosition := make(map[int]int, len(bundleIndex.Songs))
	bundleProvenance := make(map[int]SongProvenance, len(bundleIndex.Songs))
	for position, song := range bundleIndex.Songs {
		bundleByID[song.MusicID] = song
		bundlePosition[song.MusicID] = position
		bundleProvenance[song.MusicID] = SongProvenance{
			MusicID:           song.MusicID,
			Source:            string(sourceBundle),
			Revision:          song.Revision,
			State:             string(song.State),
			AvailableVersions: append([]string(nil), song.AvailableVersions...),
			UpdatedAt:         song.UpdatedAt,
			HasDetail:         song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly,
		}
	}

	makeBundleFallbackSummary := func(reason string) LyricsProjectionSummary {
		return LyricsProjectionSummary{
			TotalSongs:         len(bundleIndex.Songs),
			BundleSongs:        len(bundleIndex.Songs),
			DBPublicationSongs: 0,
			LocalizationSongs:  0,
			Degraded:           true,
			DegradedReason:     reason,
			BundleReleaseID:    publiclyricsbundle.ReleaseID,
		}
	}

	projected, err := svc.gen.PublishedLyricsJSON()
	if err != nil {
		log.Printf("[projection] database lyrics publications unavailable; serving reviewed bundle: %v", err)
		return bundle, makeBundleFallbackSummary("layer2: published lyrics query failed"), bundleProvenance
	}

	databaseIndex, err := decodePublicLyricsIndex(projected[publicLyricsIndexKey])
	if err != nil {
		log.Printf("[projection] database lyrics index undecodable; serving reviewed bundle: %v", err)
		return bundle, makeBundleFallbackSummary("layer2: database lyrics index decode failed"), bundleProvenance
	}

	provenance := make(map[int]SongProvenance, len(bundleProvenance))
	for k, v := range bundleProvenance {
		provenance[k] = v
	}

	merged := make(map[string][]byte, len(bundle)+len(projected))
	for key, body := range bundle {
		merged[key] = body
	}

	songs := append([]store.PublicLyricsIndexSong(nil), bundleIndex.Songs...)
	dbOwned := make(map[int]bool)
	for _, databaseSong := range databaseIndex.Songs {
		bundleSong, inBundle := bundleByID[databaseSong.MusicID]
		if !databasePublicationOwnsEntry(databaseSong, bundleSong, inBundle) {
			continue
		}
		dbOwned[databaseSong.MusicID] = true
		normalized := normalizeDatabaseIndexSong(databaseSong, projected)
		if position, ok := bundlePosition[databaseSong.MusicID]; ok {
			songs[position] = normalized
		} else {
			songs = append(songs, normalized)
		}
		provenance[normalized.MusicID] = SongProvenance{
			MusicID:           normalized.MusicID,
			Source:            string(sourceDBPublication),
			Revision:          normalized.Revision,
			State:             string(normalized.State),
			AvailableVersions: append([]string(nil), normalized.AvailableVersions...),
			UpdatedAt:         normalized.UpdatedAt,
			HasDetail:         normalized.State == store.PublicLyricsStateComplete || normalized.State == store.PublicLyricsStateGameOnly,
		}
	}
	applyLegacyGameProjections(projected, songs, provenance, dbOwned)

	// Edited source-v3 rendition localizations overlay exactly like legacy
	// database publications: newer complete entries own their index slot and
	// serve their validated v3 detail. Legacy publication rows keep precedence
	// over localization projection for the same song. A failing localization
	// projection keeps the reviewed bundle and legacy publications (fail-closed
	// for the additive layer only, marking degraded).
	var degraded bool
	var degradedReason string
	localizationBytes := map[int][]byte{}
	localizationIndex, localizationDetails, localizationV4Details, localizationErr := svc.gen.PublishedLyricsLocalizationProjection()
	if localizationErr != nil {
		log.Printf("[projection] lyrics localization projection unavailable; serving bundle plus legacy publications: %v", localizationErr)
		degraded = true
		degradedReason = "layer3: localization projection query failed"
	} else {
		for _, localizationSong := range localizationIndex {
			if dbOwned[localizationSong.MusicID] {
				continue
			}
			bundleSong, inBundle := bundleByID[localizationSong.MusicID]
			if !databasePublicationOwnsEntry(localizationSong, bundleSong, inBundle) {
				continue
			}
			var body []byte
			var encodeErr error
			if v4Detail, hasV4 := localizationV4Details[localizationSong.MusicID]; hasV4 {
				body, encodeErr = store.EncodePublicLyricsV4Detail(v4Detail)
			} else {
				detail, ok := localizationDetails[localizationSong.MusicID]
				if !ok {
					continue
				}
				body, encodeErr = store.EncodePublicLyricsV3Detail(detail)
			}
			if encodeErr != nil {
				log.Printf("[projection] lyrics localization %d undecodable; skipping: %v", localizationSong.MusicID, encodeErr)
				continue
			}
			dbOwned[localizationSong.MusicID] = true
			if position, ok := bundlePosition[localizationSong.MusicID]; ok {
				songs[position] = localizationSong
			} else {
				songs = append(songs, localizationSong)
			}
			localizationBytes[localizationSong.MusicID] = append(body, '\n')
			provenance[localizationSong.MusicID] = SongProvenance{
				MusicID:           localizationSong.MusicID,
				Source:            string(sourceLocalizationProjection),
				Revision:          localizationSong.Revision,
				State:             string(localizationSong.State),
				AvailableVersions: append([]string(nil), localizationSong.AvailableVersions...),
				UpdatedAt:         localizationSong.UpdatedAt,
				HasDetail:         localizationSong.State == store.PublicLyricsStateComplete || localizationSong.State == store.PublicLyricsStateGameOnly,
			}
		}
	}
	sort.Slice(songs, func(left, right int) bool {
		return songs[left].MusicID < songs[right].MusicID
	})
	if len(dbOwned) > 0 {
		body, err := files.MarshalIndentCompat(store.PublicLyricsIndexDocument{
			Version: bundleIndex.Version,
			Songs:   songs,
		})
		if err != nil {
			log.Printf("[projection] merged lyrics index unmarshalable; serving reviewed bundle: %v", err)
			return bundle, makeBundleFallbackSummary("merge: lyrics index marshal failed"), bundleProvenance
		}
		body = append(body, '\n')
		merged[publicLyricsIndexKey] = body
	}
	for musicID := range dbOwned {
		key := fmt.Sprintf("translation/lyrics/music_%d.json", musicID)
		if body, ok := projected[key]; ok {
			merged[key] = body
		} else if body, ok := localizationBytes[musicID]; ok {
			merged[key] = body
		} else {
			delete(merged, key)
		}
	}

	bundleCount := 0
	dbCount := 0
	locCount := 0
	for _, song := range songs {
		p, ok := provenance[song.MusicID]
		if !ok {
			continue
		}
		switch p.Source {
		case string(sourceBundle):
			bundleCount++
		case string(sourceDBPublication):
			dbCount++
		case string(sourceLocalizationProjection):
			locCount++
		}
	}

	summary := LyricsProjectionSummary{
		TotalSongs:         len(songs),
		BundleSongs:        bundleCount,
		DBPublicationSongs: dbCount,
		LocalizationSongs:  locCount,
		Degraded:           degraded,
		DegradedReason:     degradedReason,
		BundleReleaseID:    publiclyricsbundle.ReleaseID,
	}

	return merged, summary, provenance
}

// decodePublicLyricsIndex decodes an index.json document into the exported
// store read model shared by the legacy generator and the reviewed v3 bundle.
func decodePublicLyricsIndex(body []byte) (store.PublicLyricsIndexDocument, error) {
	var index store.PublicLyricsIndexDocument
	if err := json.Unmarshal(body, &index); err != nil {
		return store.PublicLyricsIndexDocument{}, err
	}
	return index, nil
}

// normalizeDatabaseIndexSong upgrades a database-backed index entry to the
// reviewed bundle schema. Legacy v1 publications omit the state and
// availableVersions fields, which strict v3 consumers require, so the merged
// index emits complete/full (plus game when the v1 payload carries a game
// projection) instead of mixing v1-shaped entries into a v3 index.
func normalizeDatabaseIndexSong(song store.PublicLyricsIndexSong, projected map[string][]byte) store.PublicLyricsIndexSong {
	if song.State != "" && len(song.AvailableVersions) > 0 {
		return song
	}
	song.State = store.PublicLyricsStateComplete
	if len(song.AvailableVersions) == 0 {
		versions := []string{"full"}
		key := fmt.Sprintf("translation/lyrics/music_%d.json", song.MusicID)
		if body, ok := projected[key]; ok {
			var detail store.PublicLyricsDetailDocument
			if json.Unmarshal(body, &detail) == nil && detail.GameProjection != nil {
				versions = []string{"full", "game"}
			}
		}
		song.AvailableVersions = versions
	}
	return song
}

// databasePublicationOwnsEntry decides whether a database publication replaces
// or adds the merged index entry for a music ID. The reviewed bundle keeps its
// entry when the database publication is not strictly newer, and draft database
// states never replace a reviewed complete or game_only bundle entry.
func databasePublicationOwnsEntry(databaseSong, bundleSong store.PublicLyricsIndexSong, inBundle bool) bool {
	if !inBundle {
		return true
	}
	if databaseSong.State == store.PublicLyricsStateComplete || databaseSong.State == store.PublicLyricsStateGameOnly {
		return databaseSong.Revision > bundleSong.Revision
	}
	if databaseLyricsStateIsDraft(databaseSong.State) &&
		(bundleSong.State == store.PublicLyricsStateComplete || bundleSong.State == store.PublicLyricsStateGameOnly) {
		return false
	}
	return databaseSong.Revision > bundleSong.Revision
}

// databaseLyricsStateIsDraft reports whether an availability state represents
// a draft or degraded publication that must never replace reviewed content.
func databaseLyricsStateIsDraft(state store.PublicLyricsAvailabilityState) bool {
	switch state {
	case store.PublicLyricsStateIncomplete, store.PublicLyricsStateFailed,
		store.PublicLyricsStateAmbiguous, store.PublicLyricsStateMissing:
		return true
	default:
		return false
	}
}

// SetAsset stores a pre-rendered asset (e.g. data/search-index.json) under key.
func (svc *Service) SetAsset(key string, body []byte, contentType string) {
	svc.SetAssets(map[string][]byte{key: body}, contentType)
}

// SetAssets publishes a related asset set under one lock so readers cannot
// observe mixed generations.
func (svc *Service) SetAssets(bodies map[string][]byte, contentType string) {
	now := time.Now()
	prepared := make(map[string]asset, len(bodies))
	for key, body := range bodies {
		if svc.publicLyrics != nil && isPublicLyricsAssetKey(key) {
			continue
		}
		prepared[key] = makeAsset(bytes.Clone(body), contentType, now)
	}
	svc.mu.Lock()
	for key, value := range prepared {
		svc.assets[key] = value
	}
	svc.mu.Unlock()
}

func isPublicLyricsAssetKey(key string) bool {
	if strings.HasPrefix(key, "translation/lyrics/") {
		return true
	}
	for _, locale := range model.SupportedLocales {
		if strings.HasPrefix(key, fmt.Sprintf("v2/%s/translation/lyrics/", locale)) {
			return true
		}
	}
	return false
}

// cacheControlFor keeps the long CDN TTL for every asset except the lyrics
// index and details. Those two are read as one consistent pair, and each is
// evicted on its own timer, so a long TTL lets an edge node answer with a
// detail that predates the index publication it is validated against. A short
// TTL bounds that skew; stale-while-revalidate is dropped for them so an edge
// node cannot keep serving a superseded revision after it expires.
func (svc *Service) cacheControlFor(key string) string {
	if isPublicLyricsAssetKey(key) {
		return fmt.Sprintf("public, max-age=%d, must-revalidate", int(svc.lyricsMaxAge.Seconds()))
	}
	return fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d",
		int(svc.maxAge.Seconds()), int(svc.swr.Seconds()))
}

func makeAsset(body []byte, contentType string, t time.Time) asset {
	return makeAssetWithSource(body, contentType, t, sourceGenerated, 0)
}

func makeAssetWithSource(body []byte, contentType string, t time.Time, source assetSource, revision int) asset {
	sum := sha256.Sum256(body)
	return asset{
		body:        body,
		etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
		contentType: contentType,
		modTime:     t,
		source:      source,
		revision:    revision,
	}
}

// Handler serves GET /files/<path>. Path traversal is impossible because lookup
// is a map key match, not a filesystem path.
func (svc *Service) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/files/")
		key = strings.TrimPrefix(key, "/")

		svc.mu.RLock()
		a, ok := svc.assets[key]
		svc.mu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}

		h := w.Header()
		h.Set("Content-Type", a.contentType)
		h.Set("ETag", a.etag)
		h.Set("Cache-Control", svc.cacheControlFor(key))
		h.Set("Access-Control-Allow-Origin", "*")

		if match := r.Header.Get("If-None-Match"); match != "" && etagMatch(match, a.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.ServeContent(w, r, key, a.modTime, newReadSeeker(a.body))
	}
}

func etagMatch(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}
