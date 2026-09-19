// Package searchindex regenerates the public search-index.json consumed by the
// site, combining upstream masterdata with the current translations. It is a
// port of the legacy backend/search_index.go, adapted to the SQLite store and
// the in-memory file service.
package searchindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

const (
	defaultJPMasterdataURL         = "https://metadata.pjsk.moe/jp/master"
	defaultJPMasterdataFallbackURL = "https://raw.githubusercontent.com/{repo}/{branch}/master"
	DefaultMusicAliasesURL         = "https://moe.exmeaning.com/data/music_alias/music_aliases.json"
	maxMasterdataWireBytes         = 64 << 20
	maxMasterdataDecodedBytes      = 128 << 20
	maxMasterdataRecords           = 500_000
	maxMusicAliasRecords           = 100_000
	maxMusicAliasesPerRecord       = 512
	maxMusicAliasLength            = 4096
	maxSearchCacheBytes            = 256 << 20
	searchCacheVersion             = 2
)

var errBuildSuperseded = errors.New("search index build superseded")

var expectedSearchGroups = []string{"events", "music", "cards", "gacha", "mysekai", "live", "costumes"}

var productionMinimumSourceRecords = map[string]int{
	"events.json":          100,
	"musics.json":          300,
	"cards.json":           1000,
	"gachas.json":          100,
	"mysekaiFixtures.json": 500,
	"virtualLives.json":    100,
	"snowy_costumes.json":  500,
}

type Entry struct {
	ID      int      `json:"id"`
	N       string   `json:"n"`
	G       string   `json:"g"`
	C       int      `json:"c,omitempty"`
	CN      string   `json:"cn,omitempty"`
	Aliases []string `json:"a,omitempty"`
}

type MultilingualEntry struct {
	ID      int      `json:"id"`
	N       string   `json:"n"`
	G       string   `json:"g"`
	C       int      `json:"c,omitempty"`
	CN      string   `json:"cn,omitempty"`
	EN      string   `json:"en,omitempty"`
	Aliases []string `json:"a,omitempty"`
}

// Status is safe for readiness and authenticated operational APIs. LastError
// is a stable code; detailed upstream URLs and responses remain log-only.
type Status struct {
	Ready         bool   `json:"ready"`
	Degraded      bool   `json:"degraded"`
	Running       bool   `json:"running"`
	Generation    uint64 `json:"generation"`
	LastSuccessAt string `json:"lastSuccessAt,omitempty"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	Source        string `json:"source,omitempty"`
}

type cacheEnvelope struct {
	Version      int             `json:"version"`
	Legacy       json.RawMessage `json:"legacy"`
	Multilingual json.RawMessage `json:"multilingual"`
	Coverage     map[string]int  `json:"coverage,omitempty"`
}

// Builder produces search-index.json and publishes it to the file service.
type Builder struct {
	store                *store.Store
	files                *filesvc.Service
	cfg                  *config.Config
	client               *http.Client
	debounce             time.Duration
	refresh              time.Duration
	retryMin             time.Duration
	retryMax             time.Duration
	cachePath            string
	minimumSourceRecords int
	minimumSourceByFile  map[string]int

	triggerCh chan struct{}

	buildMu      sync.Mutex
	generationMu sync.Mutex
	generation   uint64
	mu           sync.Mutex
	lastBuilt    time.Time
	lastResult   string
	status       Status
	lastCoverage map[string]int
	activeBuilds int
	ctx          context.Context
	cancel       context.CancelFunc
	startOnce    sync.Once
	stopOnce     sync.Once
	wg           sync.WaitGroup
}

func New(s *store.Store, fsvc *filesvc.Service, cfg *config.Config, debounce, refresh time.Duration) *Builder {
	if debounce <= 0 {
		debounce = time.Hour
	}
	if refresh <= 0 {
		refresh = time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Builder{
		store:                s,
		files:                fsvc,
		cfg:                  cfg,
		client:               httpx.NewClient(60 * time.Second),
		debounce:             debounce,
		refresh:              refresh,
		retryMin:             5 * time.Second,
		retryMax:             5 * time.Minute,
		minimumSourceRecords: 2,
		triggerCh:            make(chan struct{}, 1),
		ctx:                  ctx,
		cancel:               cancel,
	}
}

// SetCachePath enables restart persistence for the complete related asset set.
// It must be called before Start.
func (b *Builder) SetCachePath(path string) { b.cachePath = strings.TrimSpace(path) }

// UseProductionCoverageFloors rejects tiny but syntactically valid cold-start
// responses using conservative lower bounds far below current JP source sizes.
func (b *Builder) UseProductionCoverageFloors() {
	b.minimumSourceByFile = cloneCoverage(productionMinimumSourceRecords)
}

// SetRetryBounds configures bounded exponential retry after initial or transient
// failures. Non-positive values retain safe defaults.
func (b *Builder) SetRetryBounds(minimum, maximum time.Duration) {
	if minimum > 0 {
		b.retryMin = minimum
	}
	if maximum >= b.retryMin {
		b.retryMax = maximum
	}
}

func (b *Builder) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

// Start builds once immediately (in the background) and launches the debounce +
// periodic-refresh loop. Building on startup makes search-index.json available
// without waiting for the debounce window or the first refresh tick.
func (b *Builder) Start() {
	b.startOnce.Do(func() {
		if b.ctx.Err() != nil {
			return
		}
		if err := b.loadCache(b.ctx); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("[search-index] cached index rejected: %v", err)
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.loop()
		}()
	})
}

func (b *Builder) Stop() { b.stopOnce.Do(b.cancel) }

func (b *Builder) Wait() { b.wg.Wait() }

// Trigger schedules a debounced rebuild.
func (b *Builder) Trigger() {
	if b.ctx.Err() != nil {
		return
	}
	// Invalidate an active candidate immediately. Waiting until the queued build
	// starts would let a pre-change translation snapshot publish in the gap.
	b.generationMu.Lock()
	b.generation++
	b.generationMu.Unlock()
	select {
	case b.triggerCh <- struct{}{}:
	default:
	}
}

func (b *Builder) loop() {
	retryDelay := b.retryMin
	reason := "startup"
	for {
		err := b.buildContext(b.ctx, reason)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil && !errors.Is(err, errBuildSuperseded) {
			log.Printf("[search-index] complete build failed; retrying in %s: %v", retryDelay, err)
			if !b.wait(retryDelay) {
				return
			}
			if retryDelay < b.retryMax/2 {
				retryDelay *= 2
			} else {
				retryDelay = b.retryMax
			}
			reason = "retry"
			continue
		}
		retryDelay = b.retryMin
		reason = b.waitForWork()
		if reason == "" {
			return
		}
	}
}

// wait sleeps out the retry backoff after a failed build.
//
// It deliberately does not consume triggerCh. Handing the wait over to the
// debounce window would let steady editing postpone recovery indefinitely,
// because every further trigger resets that window. The token stays in the
// channel (capacity 1, so nothing is lost) for the next waitForWork, and
// Trigger() has already bumped the generation, so no in-flight candidate can
// publish a pre-change snapshot.
func (b *Builder) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-b.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (b *Builder) waitForWork() string {
	timer := time.NewTimer(b.refresh)
	defer timer.Stop()
	select {
	case <-b.ctx.Done():
		return ""
	case <-b.triggerCh:
		if !b.waitForDebounce() {
			return ""
		}
		return "debounce"
	case <-timer.C:
		return "refresh"
	}
}

func (b *Builder) waitForDebounce() bool {
	if b.debounce <= 0 {
		return b.ctx.Err() == nil
	}
	timer := time.NewTimer(b.debounce)
	defer timer.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return false
		case <-b.triggerCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(b.debounce)
		case <-timer.C:
			return true
		}
	}
}

// catText looks up a translated cn text for a given field+jpText, returning ""
// if absent or unchanged from the source.
func (b *Builder) catText(cat model.Category, field, jp string) string {
	if cat == nil {
		return ""
	}
	if fm, ok := cat[field]; ok {
		if e, ok := fm[jp]; ok && e.Text != "" && e.Text != jp {
			return e.Text
		}
	}
	return ""
}

func (b *Builder) build(reason string) {
	_ = b.buildContext(b.ctx, reason)
}

func (b *Builder) buildContext(ctx context.Context, reason string) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.generationMu.Lock()
	b.generation++
	generation := b.generation
	b.generationMu.Unlock()
	b.beginAttempt()
	defer func() { b.finishAttempt(generation, resultErr) }()

	b.buildMu.Lock()
	defer b.buildMu.Unlock()
	b.generationMu.Lock()
	stale := generation != b.generation
	b.generationMu.Unlock()
	if stale {
		return errBuildSuperseded
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	releaseContent, err := b.store.LockContentSharedContext(ctx)
	if err != nil {
		return fmt.Errorf("lock content snapshot: %w", err)
	}
	translationCategories := []string{"events", "music", "cards", "gacha", "mysekai", "costumes", "virtualLive"}
	translations, englishTranslations, err := b.store.SearchTranslationSnapshotContext(ctx, translationCategories)
	releaseContent()
	if err != nil {
		return fmt.Errorf("load translation snapshot: %w", err)
	}
	musicAliases, err := b.fetchMusicAliases(ctx)
	if err != nil {
		return fmt.Errorf("load music aliases: %w", err)
	}

	index := make([]Entry, 0, 4096)
	multilingual := make([]MultilingualEntry, 0, 4096)
	successes := 0
	coverage := make(map[string]int, 7)

	transEN := make(map[string]model.Category, 7)
	for group, category := range map[string]string{
		"events": "events", "music": "music", "cards": "cards", "gacha": "gacha",
		"mysekai": "mysekai", "live": "virtualLive", "costumes": "costumes",
	} {
		transEN[group] = englishTranslations[category]
	}

	type src struct {
		file, group, nameField, transField string
		extraCharID                        bool
	}
	simple := []src{
		{"events.json", "events", "name", "name", false},
		{"musics.json", "music", "title", "title", false},
		{"cards.json", "cards", "prefix", "prefix", true},
		{"gachas.json", "gacha", "name", "name", false},
		{"mysekaiFixtures.json", "mysekai", "name", "fixtureName", false},
		{"virtualLives.json", "live", "name", "name", false},
	}
	transFor := map[string]model.Category{
		"events": translations["events"], "music": translations["music"], "cards": translations["cards"],
		"gacha": translations["gacha"], "mysekai": translations["mysekai"], "live": translations["virtualLive"],
	}

	var failures []string
	for _, sdef := range simple {
		arr, err := b.fetchArray(ctx, sdef.file, sdef.nameField)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", sdef.file, err))
			continue
		}
		successes++
		coverage[sdef.file] = len(arr)
		for _, item := range arr {
			name, _ := item[sdef.nameField].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			id := asInt(item["id"])
			e := Entry{ID: id, N: name, G: sdef.group}
			if sdef.extraCharID {
				e.C = asInt(item["characterId"])
			}
			if cn := b.catText(transFor[sdef.group], sdef.transField, name); cn != "" {
				e.CN = cn
			}
			if sdef.group == "music" {
				e.Aliases = cloneStrings(musicAliases[id])
			}
			index = append(index, e)
			multi := MultilingualEntry{ID: e.ID, N: e.N, G: e.G, C: e.C, CN: e.CN, Aliases: cloneStrings(e.Aliases)}
			if en := b.catText(transEN[sdef.group], sdef.transField, name); en != "" {
				multi.EN = en
			}
			multilingual = append(multilingual, multi)
		}
	}

	// Costumes have a nested shape (snowy_costumes.json -> {costumes:[...]}).
	if costumes, err := b.fetchCostumes(ctx); err != nil {
		failures = append(failures, fmt.Sprintf("snowy_costumes.json: %v", err))
	} else {
		successes++
		representedCostumes := 0
		for _, c := range costumes {
			name, _ := c["name"].(string)
			if strings.TrimSpace(name) == "" || name == "-" {
				continue
			}
			e := Entry{ID: asInt(c["id"]), N: name, G: "costumes"}
			if cn := b.catText(translations["costumes"], "name", name); cn != "" {
				e.CN = cn
			}
			index = append(index, e)
			multi := MultilingualEntry{ID: e.ID, N: e.N, G: e.G, CN: e.CN}
			if en := b.catText(transEN["costumes"], "name", name); en != "" {
				multi.EN = en
			}
			multilingual = append(multilingual, multi)
			representedCostumes++
		}
		coverage["snowy_costumes.json"] = representedCostumes
	}

	expectedSuccesses := len(simple) + 1
	if successes != expectedSuccesses {
		return fmt.Errorf("rebuilt %d/%d source sets: %s", successes, expectedSuccesses, strings.Join(failures, "; "))
	}
	if err := validateSearchRepresentations(index, multilingual); err != nil {
		return fmt.Errorf("validate complete index: %w", err)
	}
	if err := b.validateCoverage(coverage); err != nil {
		return fmt.Errorf("validate source coverage: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	buf, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshal legacy index: %w", err)
	}
	multilingualJSON, err := json.Marshal(multilingual)
	if err != nil {
		return fmt.Errorf("marshal multilingual index: %w", err)
	}
	b.generationMu.Lock()
	if generation != b.generation || ctx.Err() != nil {
		b.generationMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		return errBuildSuperseded
	}
	b.files.SetAssets(searchAssets(buf, multilingualJSON), "application/json; charset=utf-8")
	b.mu.Lock()
	b.lastBuilt = time.Now()
	b.lastResult = fmt.Sprintf("%d entries (%s)", len(index), reason)
	b.lastCoverage = cloneCoverage(coverage)
	b.mu.Unlock()
	b.generationMu.Unlock()
	log.Printf("[search-index] published %d entries (reason=%s)", len(index), reason)

	// The cache is a restart shortcut, not a publication precondition: a full
	// or read-only /data must not withhold an index that already validated.
	// persistCache keeps its own generation guard, so a superseded candidate
	// still cannot overwrite the persisted last-known-good copy.
	if err := b.persistCache(ctx, generation, buf, multilingualJSON, coverage); err != nil {
		if !errors.Is(err, errBuildSuperseded) && ctx.Err() == nil {
			log.Printf("[search-index] persisting the index cache failed; serving from memory: %v", err)
		}
	}
	return nil
}

func (b *Builder) beginAttempt() {
	b.mu.Lock()
	b.activeBuilds++
	b.status.Running = true
	b.status.LastAttemptAt = time.Now().UTC().Format(time.RFC3339Nano)
	b.mu.Unlock()
}

func (b *Builder) finishAttempt(generation uint64, err error) {
	b.generationMu.Lock()
	latest := generation == b.generation
	b.generationMu.Unlock()
	b.mu.Lock()
	b.activeBuilds--
	b.status.Running = b.activeBuilds > 0
	if latest && !errors.Is(err, context.Canceled) && !errors.Is(err, errBuildSuperseded) {
		if err != nil {
			b.status.Degraded = true
			b.status.LastError = "search_index_build_failed"
		} else {
			b.status.Ready = true
			b.status.Degraded = false
			b.status.Generation++
			b.status.LastSuccessAt = time.Now().UTC().Format(time.RFC3339Nano)
			b.status.LastError = ""
			b.status.Source = "live"
		}
	}
	b.mu.Unlock()
}

func (b *Builder) loadCache(ctx context.Context) error {
	if b.cachePath == "" {
		return os.ErrNotExist
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := readCacheFile(ctx, b.cachePath)
	if err != nil {
		return err
	}
	var cached cacheEnvelope
	if err := json.Unmarshal(body, &cached); err != nil {
		return fmt.Errorf("decode cache envelope: %w", err)
	}
	if cached.Version != searchCacheVersion {
		return fmt.Errorf("unsupported cache version %d", cached.Version)
	}
	var legacy []Entry
	if !strings.HasPrefix(strings.TrimSpace(string(cached.Legacy)), "[") {
		return fmt.Errorf("invalid legacy cache payload")
	}
	if err := json.Unmarshal(cached.Legacy, &legacy); err != nil {
		return fmt.Errorf("invalid legacy cache payload")
	}
	var multilingual []MultilingualEntry
	if !strings.HasPrefix(strings.TrimSpace(string(cached.Multilingual)), "[") {
		return fmt.Errorf("invalid multilingual cache payload")
	}
	if err := json.Unmarshal(cached.Multilingual, &multilingual); err != nil || len(multilingual) != len(legacy) {
		return fmt.Errorf("invalid multilingual cache payload")
	}
	if err := validateSearchRepresentations(legacy, multilingual); err != nil {
		return fmt.Errorf("invalid cached search index: %w", err)
	}
	representedCoverage := coverageFromLegacy(legacy)
	coverage := representedCoverage
	if len(cached.Coverage) > 0 {
		coverage = cloneCoverage(cached.Coverage)
		for source, represented := range representedCoverage {
			if coverage[source] < represented {
				return fmt.Errorf("cached source coverage is smaller than represented %s entries", source)
			}
		}
	}
	if err := b.validateCoverage(coverage); err != nil {
		return fmt.Errorf("invalid cached source coverage: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.files.SetAssets(searchAssets(cached.Legacy, cached.Multilingual), "application/json; charset=utf-8")
	b.mu.Lock()
	b.status.Ready = true
	b.status.Degraded = true
	b.status.Generation++
	b.status.LastError = "search_index_refresh_pending"
	b.status.Source = "cache"
	b.lastCoverage = cloneCoverage(coverage)
	b.mu.Unlock()
	log.Printf("[search-index] loaded validated cached index (%d entries)", len(legacy))
	return nil
}

func (b *Builder) persistCache(ctx context.Context, generation uint64, legacy, multilingual []byte, coverage map[string]int) error {
	if b.cachePath == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(cacheEnvelope{
		Version: searchCacheVersion, Legacy: legacy, Multilingual: multilingual, Coverage: cloneCoverage(coverage),
	})
	if err != nil {
		return err
	}
	if len(body) > maxSearchCacheBytes {
		return fmt.Errorf("search cache exceeds %d bytes", maxSearchCacheBytes)
	}
	directory := filepath.Dir(b.cachePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".search-index-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	b.generationMu.Lock()
	if generation != b.generation || ctx.Err() != nil {
		b.generationMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		return errBuildSuperseded
	}
	if err := os.Rename(temporaryPath, b.cachePath); err != nil {
		b.generationMu.Unlock()
		return err
	}
	b.generationMu.Unlock()
	removeTemporary = false
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func readCacheFile(ctx context.Context, path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 0 || info.Size() > maxSearchCacheBytes {
		return nil, fmt.Errorf("search cache exceeds %d bytes", maxSearchCacheBytes)
	}
	body, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: file}, maxSearchCacheBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSearchCacheBytes {
		return nil, fmt.Errorf("search cache exceeds %d bytes", maxSearchCacheBytes)
	}
	return body, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func validateSearchRepresentations(legacy []Entry, multilingual []MultilingualEntry) error {
	if len(legacy) == 0 || len(multilingual) != len(legacy) {
		return fmt.Errorf("asset lengths do not match or are empty")
	}
	allowed := make(map[string]bool, len(expectedSearchGroups))
	for _, group := range expectedSearchGroups {
		allowed[group] = true
	}
	type identity struct {
		group string
		id    int
	}
	seen := make(map[identity]bool, len(legacy))
	represented := make(map[string]bool, len(expectedSearchGroups))
	for index, entry := range legacy {
		multi := multilingual[index]
		if entry.ID <= 0 || strings.TrimSpace(entry.N) == "" || !allowed[entry.G] {
			return fmt.Errorf("entry %d has invalid identity", index)
		}
		key := identity{group: entry.G, id: entry.ID}
		if seen[key] {
			return fmt.Errorf("entry %s/%d appears more than once", entry.G, entry.ID)
		}
		seen[key] = true
		represented[entry.G] = true
		if entry.ID != multi.ID || entry.N != multi.N || entry.G != multi.G || entry.C != multi.C || entry.CN != multi.CN || !sameStrings(entry.Aliases, multi.Aliases) {
			return fmt.Errorf("asset representations do not match at entry %d", index)
		}
		if err := validateMusicAliases(entry.G, entry.ID, entry.Aliases); err != nil {
			return fmt.Errorf("entry %d aliases: %w", index, err)
		}
	}
	for _, group := range expectedSearchGroups {
		if !represented[group] {
			return fmt.Errorf("asset set is missing group %s", group)
		}
	}
	return nil
}

func (b *Builder) validateCoverage(candidate map[string]int) error {
	for _, source := range []string{
		"events.json", "musics.json", "cards.json", "gachas.json", "mysekaiFixtures.json", "virtualLives.json", "snowy_costumes.json",
	} {
		minimum := b.minimumRecords(source)
		if candidate[source] < minimum {
			return fmt.Errorf("%s has %d records; require at least %d", source, candidate[source], minimum)
		}
	}
	b.mu.Lock()
	previous := cloneCoverage(b.lastCoverage)
	b.mu.Unlock()
	for source, previousCount := range previous {
		if candidate[source] < previousCount {
			return fmt.Errorf("%s suspiciously reduced from %d to %d records", source, previousCount, candidate[source])
		}
	}
	return nil
}

func (b *Builder) minimumRecords(source string) int {
	if minimum := b.minimumSourceByFile[source]; minimum > 0 {
		return minimum
	}
	return b.minimumSourceRecords
}

func cloneCoverage(value map[string]int) map[string]int {
	if len(value) == 0 {
		return nil
	}
	cloned := make(map[string]int, len(value))
	for source, count := range value {
		cloned[source] = count
	}
	return cloned
}

func coverageFromLegacy(entries []Entry) map[string]int {
	sources := map[string]string{
		"events": "events.json", "music": "musics.json", "cards": "cards.json", "gacha": "gachas.json",
		"mysekai": "mysekaiFixtures.json", "live": "virtualLives.json", "costumes": "snowy_costumes.json",
	}
	coverage := make(map[string]int, len(sources))
	for _, entry := range entries {
		coverage[sources[entry.G]]++
	}
	return coverage
}

func searchAssets(legacy, multilingual []byte) map[string][]byte {
	return map[string][]byte{
		"data/search-index.json":          legacy,
		"v2/data/search-index.json":       multilingual,
		"v2/en-US/data/search-index.json": multilingual,
	}
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateMusicAliases(group string, musicID int, aliases []string) error {
	if group != "music" {
		if len(aliases) != 0 {
			return errors.New("aliases are only valid for music entries")
		}
		return nil
	}
	if musicID <= 0 || len(aliases) > maxMusicAliasesPerRecord {
		return errors.New("alias identity or count is invalid")
	}
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		if alias == "" || alias != strings.TrimSpace(alias) || len(alias) > maxMusicAliasLength {
			return errors.New("alias is empty, padded, or too long")
		}
		if strings.ContainsAny(alias, "\r\n\x00") {
			return errors.New("alias contains forbidden control characters")
		}
		if _, duplicate := seen[alias]; duplicate {
			return errors.New("alias is duplicated")
		}
		seen[alias] = struct{}{}
	}
	return nil
}

func (b *Builder) fetchMusicAliases(ctx context.Context) (map[int][]string, error) {
	if b.cfg == nil {
		return nil, nil
	}
	url := strings.TrimSpace(b.cfg.Get(config.KeyMusicAliasesURL))
	if url == "" {
		return nil, nil
	}
	data, err := b.fetchJSON(ctx, url)
	if err != nil {
		return nil, err
	}
	root, ok := data.(map[string]any)
	if !ok {
		return nil, errors.New("music alias source must be an object")
	}
	rawMusics, ok := root["musics"].([]any)
	if !ok || len(rawMusics) == 0 || len(rawMusics) > maxMusicAliasRecords {
		return nil, errors.New("music alias source has an invalid musics array")
	}
	aliases := make(map[int][]string, len(rawMusics))
	for index, raw := range rawMusics {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("music alias record %d is not an object", index)
		}
		musicID := asInt(entry["music_id"])
		rawAliases, ok := entry["aliases"].([]any)
		if !ok {
			return nil, fmt.Errorf("music alias record %d has an invalid aliases array", index)
		}
		if _, duplicate := aliases[musicID]; duplicate {
			return nil, fmt.Errorf("music alias source contains duplicate music id %d", musicID)
		}
		values := make([]string, 0, len(rawAliases))
		for aliasIndex, rawAlias := range rawAliases {
			alias, ok := rawAlias.(string)
			if !ok {
				return nil, fmt.Errorf("music alias record %d alias %d is not a string", index, aliasIndex)
			}
			values = append(values, alias)
		}
		if err := validateMusicAliases("music", musicID, values); err != nil {
			return nil, fmt.Errorf("music alias record %d: %w", index, err)
		}
		aliases[musicID] = values
	}
	return aliases, nil
}

func (b *Builder) fetchArray(ctx context.Context, filename, nameField string) ([]map[string]any, error) {
	data, err := b.fetchMasterdata(ctx, filename)
	if err != nil {
		return nil, err
	}
	arr, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected json type for %s", filename)
	}
	if len(arr) > maxMasterdataRecords {
		return nil, fmt.Errorf("too many records in %s: %d", filename, len(arr))
	}
	out := make([]map[string]any, 0, len(arr))
	for index, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s record %d is not an object", filename, index)
		}
		out = append(out, m)
	}
	if err := validateMasterdataRecords(filename, nameField, out, b.minimumRecords(filename)); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *Builder) fetchCostumes(ctx context.Context) ([]map[string]any, error) {
	data, err := b.fetchMasterdata(ctx, "snowy_costumes.json")
	if err != nil {
		return nil, err
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected json type for snowy_costumes.json")
	}
	arr, ok := obj["costumes"].([]any)
	if !ok {
		return nil, fmt.Errorf("missing costumes array")
	}
	if len(arr) > maxMasterdataRecords {
		return nil, fmt.Errorf("too many costume records: %d", len(arr))
	}
	out := make([]map[string]any, 0, len(arr))
	for index, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("snowy_costumes.json record %d is not an object", index)
		}
		out = append(out, m)
	}
	if err := validateMasterdataRecords("snowy_costumes.json", "name", out, b.minimumRecords("snowy_costumes.json")); err != nil {
		return nil, err
	}
	return out, nil
}

func validateMasterdataRecords(filename, nameField string, records []map[string]any, minimum int) error {
	if len(records) < minimum {
		return fmt.Errorf("%s has %d records; require at least %d", filename, len(records), minimum)
	}
	seen := make(map[int]bool, len(records))
	for index, record := range records {
		id := asInt(record["id"])
		name, _ := record[nameField].(string)
		if id <= 0 || strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s record %d has incomplete identity", filename, index)
		}
		if seen[id] {
			return fmt.Errorf("%s contains duplicate id %d", filename, id)
		}
		seen[id] = true
	}
	return nil
}

func (b *Builder) fetchMasterdata(ctx context.Context, filename string) (any, error) {
	bases := []string{defaultJPMasterdataURL, defaultJPMasterdataFallbackURL}
	repo, branch := "Team-Haruki/haruki-sekai-master", "main"
	if b.cfg != nil {
		bases[0] = b.cfg.GetOr(config.KeyUpstreamJPMasterdataURL, bases[0])
		bases[1] = b.cfg.GetOr(config.KeyUpstreamJPMasterdataFallbackURL, bases[1])
		repo = b.cfg.GetOr(config.KeyUpstreamRepo, repo)
		branch = b.cfg.GetOr(config.KeyUpstreamBranch, branch)
	}
	seen := map[string]bool{}
	var failures []string
	for _, base := range bases {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		base = strings.ReplaceAll(base, "{repo}", repo)
		base = strings.ReplaceAll(base, "{branch}", branch)
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		url := base + "/" + strings.TrimLeft(filename, "/")
		data, err := b.fetchJSON(ctx, url)
		if err == nil {
			return data, nil
		}
		failures = append(failures, err.Error())
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no JP masterdata source configured")
	}
	return nil, fmt.Errorf("all JP masterdata sources failed: %s", strings.Join(failures, "; "))
}

func (b *Builder) fetchJSON(ctx context.Context, url string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "moesekai-search-index")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return nil, fmt.Errorf("GET %s: http %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := httpx.ReadBody(resp, maxMasterdataWireBytes, maxMasterdataDecodedBytes)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func asInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case string:
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	default:
		return 0
	}
}
