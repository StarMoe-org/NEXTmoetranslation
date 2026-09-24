package translator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/store"
)

const (
	sideStoryCatalogMaxAge     = 6 * time.Hour
	sideStoryCatalogRetryDelay = 10 * time.Minute
	sideStoryDeferredDelay     = 5 * time.Second // at most, after a round deferred to a producer
	sideStoryApplyBatch        = 5
)

var errSideStoryDeferred = errors.New("a producer job is running; retrying next round")

// SideStoryBackfillOptions configures the card and area story backfill.
type SideStoryBackfillOptions struct {
	Enabled      bool          // SIDE_STORY_BACKFILL_ENABLED is not "false"
	Interval     time.Duration // between rounds
	Batch        int           // episodes per round
	RequestDelay time.Duration // between upstream requests
}

// SideStoryBackfill imports card and area scripts and their official CN/EN
// text in rate-limited background rounds. It runs while enabled() allows it,
// independent of the translate scheduler, never claims the translator's job
// lock and never calls an LLM. The embedded Translator serves the on-demand
// runner methods.
type SideStoryBackfill struct {
	*Translator
	opts SideStoryBackfillOptions
	now  func() time.Time

	ctx       context.Context
	cancel    context.CancelFunc
	wake      chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once

	mu             sync.Mutex
	running        bool
	forceCatalog   bool
	nextRoundAt    time.Time
	lastRoundAt    time.Time
	lastRoundError string
	lastRound      store.SideStoryRoundSummary
	catalogs       map[string]sideStoryCatalogState // by kind

	// lastRequestAt carries the request delay from one round into the next;
	// only the round goroutine touches it.
	lastRequestAt time.Time
	publish       func(ctx context.Context, kind, storyID string) error // set before Start
}

// sideStoryRoundChanges lists what a round wrote.
type sideStoryRoundChanges struct {
	catalog bool             // stories, episodes or titles changed
	stories []sideStoryStory // episode lines or translations changed
}

type sideStoryStory struct{ kind, id string }

func NewSideStoryBackfill(t *Translator, opts SideStoryBackfillOptions) *SideStoryBackfill {
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	if opts.Batch < 1 {
		opts.Batch = 30
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SideStoryBackfill{
		Translator: t, opts: opts, now: time.Now,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1),
		catalogs: map[string]sideStoryCatalogState{},
	}
}

// sideStoryCatalogState tracks the catalog refresh of one kind.
type sideStoryCatalogState struct {
	refreshedAt time.Time
	version     string // upstream data version at refreshedAt
	failedAt    time.Time
}

var sideStoryCatalogKinds = []string{store.SideStoryKindCard, store.SideStoryKindArea}

// Start launches the round loop unless the backfill is disabled by env. The
// first round runs immediately; the side_story_backfill.enabled setting is
// checked every round.
func (w *SideStoryBackfill) Start() {
	w.startOnce.Do(func() {
		if !w.opts.Enabled || w.ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		w.nextRoundAt = w.now()
		w.mu.Unlock()
		w.wg.Add(1)
		go w.loop()
	})
}

// SetPublisher makes rounds republish each story whose episodes they wrote
// through publish, after the write is released, instead of scheduling a full
// public rebuild; catalog changes still schedule one. Call it before Start.
func (w *SideStoryBackfill) SetPublisher(publish func(ctx context.Context, kind, storyID string) error) {
	w.publish = publish
}

// Stop cancels the running round's requests and writes; Wait joins the loop.
func (w *SideStoryBackfill) Stop() { w.cancel() }

func (w *SideStoryBackfill) Wait() { w.wg.Wait() }

// enabled needs SIDE_STORY_BACKFILL_ENABLED not false and the
// side_story_backfill.enabled setting not false (unset means on); the legacy
// scheduler.enabled switch does not gate it.
func (w *SideStoryBackfill) enabled() bool {
	return w.opts.Enabled && w.ctx.Err() == nil && w.cfg.GetBool(config.KeySideStoryBackfillOn, true)
}

// TriggerSideStoryBackfill wakes the worker for an immediate round, after the
// running one if any. It reports false when the backfill is disabled; a
// catalog refresh request is kept for the first round after it is re-enabled.
func (w *SideStoryBackfill) TriggerSideStoryBackfill(refreshCatalog bool) bool {
	if refreshCatalog {
		w.mu.Lock()
		w.forceCatalog = true
		w.mu.Unlock()
	}
	if !w.enabled() {
		return false
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return true
}

func (w *SideStoryBackfill) SideStoryBackfillState() store.SideStoryBackfillState {
	enabled := w.enabled()
	w.mu.Lock()
	defer w.mu.Unlock()
	// The catalog counts as refreshed once every kind is, as of the oldest.
	var catalogRefreshedAt time.Time
	for index, kind := range sideStoryCatalogKinds {
		refreshedAt := w.catalogs[kind].refreshedAt
		if refreshedAt.IsZero() {
			catalogRefreshedAt = time.Time{}
			break
		}
		if index == 0 || refreshedAt.Before(catalogRefreshedAt) {
			catalogRefreshedAt = refreshedAt
		}
	}
	state := store.SideStoryBackfillState{
		Enabled: enabled, Running: w.running,
		LastRoundAt: sideStoryTime(w.lastRoundAt), CatalogRefreshedAt: sideStoryTime(catalogRefreshedAt),
		LastRoundError: w.lastRoundError, LastRound: w.lastRound,
	}
	if state.Enabled {
		state.NextRoundAt = sideStoryTime(w.nextRoundAt)
	}
	return state
}

func sideStoryTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func (w *SideStoryBackfill) loop() {
	defer w.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-timer.C:
		case <-w.wake:
		}
		delay := w.opts.Interval
		if w.enabled() && w.runRound(w.ctx) {
			delay = min(delay, sideStoryDeferredDelay)
		}
		if w.ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		w.nextRoundAt = w.now().Add(delay)
		w.mu.Unlock()
		timer.Reset(delay)
	}
}

// runRound runs one round and reports whether it was deferred to a producer
// job, which keeps any requested catalog refresh for the retry.
func (w *SideStoryBackfill) runRound(ctx context.Context) (deferred bool) {
	started := w.now()
	w.mu.Lock()
	w.running = true
	force := w.forceCatalog
	w.forceCatalog = false
	w.mu.Unlock()

	summary, changes, err := w.round(ctx, force)
	message := ""
	if err != nil {
		message = truncateStatusDetail(err.Error(), 600)
		// A round that deferred before any work retries every few seconds.
		if err != errSideStoryDeferred {
			log.Printf("[side-story] round: %s", message)
		}
	}
	w.mu.Lock()
	w.running = false
	w.lastRoundAt = started
	w.lastRound = summary
	w.lastRoundError = message
	w.mu.Unlock()
	w.publishRound(ctx, changes)
	if changes.catalog || len(changes.stories) > 0 {
		w.emit("sidestory.sync", fmt.Sprintf("卡牌剧情与区域对话已同步：获取 %d 话，写入 %d 条官方译文", summary.Fetched, summary.OfficialWritten),
			summary.Fetched, summary.Episodes)
	}
	return errors.Is(err, errSideStoryDeferred)
}

// publishRound republishes the stories a round's episode writes changed. A
// catalog change, a failed publish or no publisher schedules the full rebuild.
func (w *SideStoryBackfill) publishRound(ctx context.Context, changes sideStoryRoundChanges) {
	rebuild := changes.catalog
	for _, story := range changes.stories {
		if w.publish == nil || ctx.Err() != nil {
			rebuild = true
			break
		}
		if err := w.publish(ctx, story.kind, story.id); err != nil {
			log.Printf("[side-story] publish %s %s: %v", story.kind, story.id, err)
			rebuild = true
		}
	}
	if rebuild {
		w.store.NotifyChange()
	}
}

func (w *SideStoryBackfill) producerRunning() bool {
	w.Translator.mu.Lock()
	gate := w.editorGate
	w.Translator.mu.Unlock()
	return gate != nil && gate.Status().Running
}

// round refreshes the catalog when due, then fetches the due episodes with the
// request delay between upstream requests and applies them in small batches.
func (w *SideStoryBackfill) round(ctx context.Context, forceCatalog bool) (summary store.SideStoryRoundSummary, changes sideStoryRoundChanges, err error) {
	if w.producerRunning() {
		if forceCatalog {
			w.mu.Lock()
			w.forceCatalog = true
			w.mu.Unlock()
		}
		return summary, changes, errSideStoryDeferred
	}
	pacer := &sideStoryPacer{delay: w.opts.RequestDelay, last: w.lastRequestAt}
	defer func() {
		summary.Requests = pacer.requests
		w.lastRequestAt = pacer.last
	}()
	var problems []error
	if kinds := w.catalogDue(forceCatalog); len(kinds) > 0 {
		changes.catalog, err = w.refreshCatalog(ctx, pacer, kinds)
		if errors.Is(err, errSideStoryDeferred) || ctx.Err() != nil {
			return summary, changes, err
		}
		if err != nil {
			problems = append(problems, err)
		}
	}
	items, err := w.store.SideStoryWorkQueueContext(ctx, w.opts.Batch, w.now())
	if err != nil {
		return summary, changes, errors.Join(append(problems, err)...)
	}
	summary.Episodes = len(items)
	batch := make([]store.SideStoryEpisodeFetch, 0, sideStoryApplyBatch)
	apply := func() error {
		if len(batch) == 0 {
			return nil
		}
		result, err := w.applySideStoryFetchesContext(ctx, batch, w.now())
		batch = batch[:0]
		if errors.Is(err, editorgate.ErrProducerRunning) {
			return errSideStoryDeferred
		}
		if err != nil {
			return err
		}
		for _, episode := range result.Episodes {
			if episode.Fetched {
				summary.Fetched++
				story := sideStoryStory{episode.Kind, episode.StoryID}
				if result.Changed && !slices.Contains(changes.stories, story) {
					changes.stories = append(changes.stories, story)
				}
			}
			if episode.Error != "" {
				if sideStoryEpisodeFailed(episode) {
					summary.Errors++
				} else {
					summary.Retrying++
				}
			}
			summary.OfficialWritten += episode.OfficialWritten
			if episode.DroppedHumanLines > 0 {
				log.Printf("[side-story] %s %s episode %s: changed JP script deleted %d human line translation(s)",
					episode.Kind, episode.StoryID, episode.EpisodeKey, episode.DroppedHumanLines)
			}
		}
		return nil
	}
	for _, item := range items {
		fetch, err := w.fetchSideStoryEpisodeContext(ctx, pacer, item,
			item.CNState == store.SideStoryStatePending, item.ENState == store.SideStoryStatePending)
		if err != nil {
			return summary, changes, err
		}
		batch = append(batch, fetch)
		if len(batch) < sideStoryApplyBatch {
			continue
		}
		if err := apply(); err != nil {
			return summary, changes, errors.Join(append(problems, err)...)
		}
	}
	if err := apply(); err != nil {
		problems = append(problems, err)
	}
	return summary, changes, errors.Join(problems...)
}

// sideStoryEpisodeFailed separates an episode that needs attention from one
// whose lastError only notes a locale waiting for its automatic retry.
func sideStoryEpisodeFailed(episode store.SideStoryEpisodeApply) bool {
	if !episode.Fetched {
		return true
	}
	for _, state := range []string{episode.CNState, episode.ENState} {
		if state == store.SideStoryStateError || state == store.SideStoryStateMismatch {
			return true
		}
	}
	return false
}

// catalogDue lists the kinds whose catalog is due: at the first round, after
// sideStoryCatalogMaxAge, when the upstream watcher recorded a new data version
// (only while the watcher runs, which needs scheduler.enabled), or on request.
// A kind whose refresh failed is retried after sideStoryCatalogRetryDelay
// unless requested.
func (w *SideStoryBackfill) catalogDue(force bool) []string {
	version := w.cfg.Get(config.KeyUpstreamLastDataVersion)
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	var due []string
	for _, kind := range sideStoryCatalogKinds {
		state := w.catalogs[kind]
		if !force {
			if !state.failedAt.IsZero() && now.Sub(state.failedAt) < sideStoryCatalogRetryDelay {
				continue
			}
			if !state.refreshedAt.IsZero() && now.Sub(state.refreshedAt) < sideStoryCatalogMaxAge && version == state.version {
				continue
			}
		}
		due = append(due, kind)
	}
	return due
}

// refreshCatalog fetches and writes the catalogs of kinds. It reports whether
// stories or episodes were added, official text requeued or title
// translations written or deleted.
func (w *SideStoryBackfill) refreshCatalog(ctx context.Context, pacer *sideStoryPacer, kinds []string) (bool, error) {
	version := w.cfg.Get(config.KeyUpstreamLastDataVersion)
	attemptAt := w.now()
	changed := false
	var errs []error
	for _, kind := range kinds {
		stories, err := w.fetchSideStoryCatalogContext(ctx, pacer, kind)
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}
		if err == nil {
			var release func()
			if release, err = w.beginSideStoryWrite(ctx); err == nil {
				var result store.SideStoryCatalogResult
				result, err = w.store.SyncSideStoryCatalogContext(ctx, kind, stories, w.now())
				release()
				changed = changed || result.NewStories > 0 || result.NewEpisodes > 0 || result.OfficialRequeued > 0 ||
					result.OfficialTitlesWritten > 0 || result.TitlesReplaced > 0
				if result.DroppedHumanTitles > 0 {
					log.Printf("[side-story] %s catalog: changed JP episode titles deleted %d human title translation(s)", kind, result.DroppedHumanTitles)
				}
			}
		}
		if errors.Is(err, editorgate.ErrProducerRunning) {
			w.mu.Lock()
			w.forceCatalog = true
			w.mu.Unlock()
			return changed, errSideStoryDeferred
		}
		w.mu.Lock()
		state := w.catalogs[kind]
		if err != nil {
			errs = append(errs, fmt.Errorf("%s catalog: %w", kind, err))
			state.failedAt = attemptAt
		} else {
			state = sideStoryCatalogState{refreshedAt: attemptAt, version: version}
		}
		w.catalogs[kind] = state
		w.mu.Unlock()
	}
	return changed, errors.Join(errs...)
}
