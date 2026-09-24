package translator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// ProgressFn receives progress notes during long-running operations (wired to
// SSE by the caller). stage is a short label; detail is human-readable.
type ProgressFn func(stage, detail string, current, total int)

// Translator runs CN sync + AI translation against the SQLite store. Config
// (LLM keys, models, batch size) is read live from the config store so admin
// changes take effect without restart.
type Translator struct {
	store      *store.Store
	eventStore *store.EventStore
	cfg        *config.Config
	dataClient *http.Client
	llmClient  *http.Client
	hedgeDelay time.Duration

	mu             sync.Mutex
	status         Status
	progress       ProgressFn
	releaseContent func()
	editorGate     *editorgate.Gate
	releaseGate    func()
	runCtx         context.Context
	runCancel      context.CancelFunc
	runWG          sync.WaitGroup

	llmUnavailableUntil time.Time
	llmLastError        string

	eventAssociationMu        sync.Mutex
	eventAssociationCached    EventAssociationIndex
	eventAssociationExpiresAt time.Time
}

// Status reports the translator's current run state.
type Status struct {
	Running   bool   `json:"running"`
	LastRun   string `json:"lastRun,omitempty"`
	LastMode  string `json:"lastMode,omitempty"`
	LastError string `json:"lastError,omitempty"`
	LastNote  string `json:"lastNote,omitempty"`
}

// llmConfig is a snapshot of LLM settings for one operation.
type llmConfig struct {
	LLMType        string
	GeminiAPIKey   string
	GeminiModel    string
	OpenAIAPIKey   string
	OpenAIBaseURL  string
	OpenAIModel    string
	RequestTimeout time.Duration
	MaxRetries     int
	BatchSize      int
	RateDelay      time.Duration
}

const (
	defaultDataRequestTimeout = 3 * time.Minute
	defaultLLMRequestTimeout  = 45 * time.Second
	defaultLLMMaxRetries      = 2
	automaticLLMTimeout       = 30 * time.Second
	llmFailureCooldown        = 10 * time.Minute
)

var ErrRunning = errors.New("a translate job is already running")

func New(s *store.Store, es *store.EventStore, cfg *config.Config, gates ...*editorgate.Gate) *Translator {
	t := &Translator{
		store:      s,
		eventStore: es,
		cfg:        cfg,
		dataClient: httpx.NewClientWithTimeouts(defaultDataRequestTimeout, 15*time.Second, 25*time.Second, 45*time.Second),
		// LLM calls use a live, per-request context timeout from config. Keeping
		// the client timeout at zero prevents it from fighting that deadline,
		// especially while an OpenAI-compatible SSE response is streaming.
		llmClient:  httpx.NewHTTPSCredentialClient(0, 10*time.Second, 15*time.Second, 0),
		hedgeDelay: defaultSourceHedgeDelay,
	}
	if len(gates) > 0 {
		t.editorGate = gates[0]
	}
	return t
}

func (t *Translator) SetEditorGate(gate *editorgate.Gate) {
	t.mu.Lock()
	t.editorGate = gate
	t.mu.Unlock()
}

// SetProgress installs a progress callback (e.g. SSE broadcast).
func (t *Translator) SetProgress(fn ProgressFn) { t.progress = fn }

func (t *Translator) emit(stage, detail string, cur, total int) {
	if t.progress != nil {
		t.progress(stage, detail, cur, total)
	}
}

// snapshotConfig reads current LLM settings from the config store.
func (t *Translator) snapshotConfig() llmConfig {
	requestTimeout := time.Duration(t.cfg.GetInt(config.KeyLLMRequestTimeoutMS, int(defaultLLMRequestTimeout/time.Millisecond))) * time.Millisecond
	if requestTimeout <= 0 {
		requestTimeout = defaultLLMRequestTimeout
	}
	maxRetries := t.cfg.GetInt(config.KeyLLMMaxRetries, defaultLLMMaxRetries)
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > 5 {
		maxRetries = 5
	}
	return llmConfig{
		LLMType:        t.cfg.GetOr(config.KeyLLMType, "openai"),
		GeminiAPIKey:   t.cfg.Get(config.KeyGeminiAPIKey),
		GeminiModel:    t.cfg.GetOr(config.KeyGeminiModel, "gemini-2.0-flash"),
		OpenAIAPIKey:   t.cfg.Get(config.KeyOpenAIAPIKey),
		OpenAIBaseURL:  t.cfg.GetOr(config.KeyOpenAIBaseURL, "https://api.openai.com/v1"),
		OpenAIModel:    t.cfg.GetOr(config.KeyOpenAIModel, "gpt-4o-mini"),
		RequestTimeout: requestTimeout,
		MaxRetries:     maxRetries,
		BatchSize:      t.cfg.GetInt(config.KeyBatchSize, 20),
		RateDelay:      time.Duration(t.cfg.GetInt(config.KeyRateDelayMS, 800)) * time.Millisecond,
	}
}

func (t *Translator) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *Translator) recordLLMSuccess() {
	t.mu.Lock()
	t.llmUnavailableUntil = time.Time{}
	t.llmLastError = ""
	t.mu.Unlock()
}

func (t *Translator) recordLLMFailure(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	t.llmUnavailableUntil = time.Now().Add(llmFailureCooldown)
	t.llmLastError = err.Error()
	t.mu.Unlock()
}

func (t *Translator) automaticLLMUnavailable() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.llmUnavailableUntil.IsZero() || !time.Now().Before(t.llmUnavailableUntil) {
		t.llmUnavailableUntil = time.Time{}
		t.llmLastError = ""
		return "", false
	}
	return fmt.Sprintf("LLM 暂时不可用（冷却至 %s）：%s", t.llmUnavailableUntil.UTC().Format(time.RFC3339), t.llmLastError), true
}

// markStart claims the single-run lock, returning an error if already running.
func (t *Translator) markStart(ctx context.Context, mode string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	if t.status.Running {
		t.mu.Unlock()
		log.Printf("[translate] %s rejected: a job is already running", mode)
		return ErrRunning
	}
	previous := t.status
	runCtx, runCancel := context.WithCancel(ctx)
	t.status.Running = true
	t.status.LastMode = mode
	t.status.LastError = ""
	gate := t.editorGate
	t.runCtx = runCtx
	t.runCancel = runCancel
	t.runWG.Add(1)
	t.mu.Unlock()

	var releaseGate func()
	if gate != nil {
		var err error
		releaseGate, err = gate.BeginProducerContext(runCtx)
		if err != nil {
			t.mu.Lock()
			t.status = previous
			t.runCtx = nil
			t.runCancel = nil
			t.mu.Unlock()
			runCancel()
			t.runWG.Done()
			log.Printf("[translate] %s rejected: %v", mode, err)
			return err
		}
	}
	if err := runCtx.Err(); err != nil {
		if releaseGate != nil {
			releaseGate()
		}
		t.mu.Lock()
		t.status = previous
		t.runCtx = nil
		t.runCancel = nil
		t.mu.Unlock()
		runCancel()
		t.runWG.Done()
		return err
	}
	releaseContent, err := t.store.LockContentSharedContext(runCtx)
	if err != nil {
		if releaseGate != nil {
			releaseGate()
		}
		t.mu.Lock()
		t.status = previous
		t.runCtx = nil
		t.runCancel = nil
		t.mu.Unlock()
		runCancel()
		t.runWG.Done()
		return err
	}
	if err := runCtx.Err(); err != nil {
		releaseContent()
		if releaseGate != nil {
			releaseGate()
		}
		t.mu.Lock()
		t.status = previous
		t.runCtx = nil
		t.runCancel = nil
		t.mu.Unlock()
		runCancel()
		t.runWG.Done()
		return err
	}
	t.mu.Lock()
	t.releaseContent = releaseContent
	t.releaseGate = releaseGate
	t.mu.Unlock()
	log.Printf("[translate] %s started", mode)
	return nil
}

func (t *Translator) setNote(note string) {
	t.mu.Lock()
	t.status.LastNote = note
	t.mu.Unlock()
}

func (t *Translator) markEnd(note string, err error) {
	t.mu.Lock()
	mode := t.status.LastMode
	releaseContent := t.releaseContent
	releaseGate := t.releaseGate
	runCancel := t.runCancel
	t.releaseContent = nil
	t.releaseGate = nil
	t.runCtx = nil
	t.runCancel = nil
	t.mu.Unlock()
	if releaseContent != nil {
		releaseContent()
	}
	if releaseGate != nil {
		releaseGate()
	}
	if runCancel != nil {
		runCancel()
	}

	t.mu.Lock()
	t.status.Running = false
	t.status.LastRun = time.Now().UTC().Format(time.RFC3339)
	t.status.LastNote = note
	if err != nil {
		t.status.LastError = err.Error()
	}
	t.mu.Unlock()
	if err != nil {
		log.Printf("[translate] %s FAILED: %v", mode, err)
	} else {
		log.Printf("[translate] %s done: %s", mode, note)
	}
	t.runWG.Done()
}

// Cancel interrupts the active producer's network and retry waits. Wait must be
// called before SQLite is closed.
func (t *Translator) Cancel() {
	t.mu.Lock()
	cancel := t.runCancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *Translator) Wait() { t.runWG.Wait() }

func (t *Translator) runContext() context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runCtx != nil {
		return t.runCtx
	}
	return context.Background()
}

func (t *Translator) wait(delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	ctx := t.runContext()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// IsAlreadyRunning reports whether an error is the "already running" sentinel.
func IsAlreadyRunning(err error) bool {
	return errors.Is(err, ErrRunning) || errors.Is(err, editorgate.ErrProducerRunning) ||
		(err != nil && strings.Contains(strings.ToLower(err.Error()), "already running"))
}

func IsDraining(err error) bool { return errors.Is(err, editorgate.ErrDraining) }

// ---- CN sync ----

// CNSyncResult summarizes a CN-sync run.
type CNSyncResult struct {
	Mode                 string            `json:"mode"`
	Categories           int               `json:"categories"`
	UpdatedEntries       int               `json:"updatedEntries"`
	EventStoryFiles      int               `json:"eventStoryFiles"`
	AITranslationSkipped int               `json:"aiTranslationSkipped,omitempty"`
	AITranslationNote    string            `json:"aiTranslationNote,omitempty"`
	Skipped              []string          `json:"skipped,omitempty"`
	SkippedDetails       map[string]string `json:"skippedDetails,omitempty"`
}

func (r *CNSyncResult) addSkipped(category string, err error) {
	seen := false
	for _, existing := range r.Skipped {
		if existing == category {
			seen = true
			break
		}
	}
	if !seen {
		r.Skipped = append(r.Skipped, category)
	}
	if err == nil {
		return
	}
	if r.SkippedDetails == nil {
		r.SkippedDetails = map[string]string{}
	}
	r.SkippedDetails[category] = err.Error()
	log.Printf("[translate] cn-sync skipped %s: %v", category, err)
}

// SkippedError returns a concise, actionable status error suitable for the
// upstream watcher and management UI. Full details remain in SkippedDetails.
func (r CNSyncResult) SkippedError() error {
	if len(r.Skipped) == 0 {
		return nil
	}
	parts := make([]string, 0, len(r.Skipped))
	for _, category := range r.Skipped {
		part := category
		if detail := strings.TrimSpace(r.SkippedDetails[category]); detail != "" {
			part += ": " + truncateStatusDetail(detail, 280)
		}
		parts = append(parts, part)
	}
	return fmt.Errorf("data sync skipped: %s", strings.Join(parts, "; "))
}

func truncateStatusDetail(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func summarizeErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	limit := len(errs)
	if limit > 3 {
		limit = 3
	}
	parts := make([]string, 0, limit)
	for _, err := range errs[:limit] {
		parts = append(parts, err.Error())
	}
	if len(errs) > limit {
		parts = append(parts, fmt.Sprintf("另有 %d 个错误", len(errs)-limit))
	}
	return fmt.Errorf("%d 个剧情子任务失败: %s", len(errs), strings.Join(parts, "; "))
}

// SyncCNOnly fetches masterdata and applies official CN translations to all
// categories plus event stories. It is the scheduled / manual "数据更新" action.
func (t *Translator) SyncCNOnly() (CNSyncResult, error) {
	return t.SyncCNOnlyContext(context.Background())
}

type cnSyncStep struct {
	category string
	fn       func() cnExtractedCategory
}

func (t *Translator) cnSyncSteps() []cnSyncStep {
	return []cnSyncStep{
		{"cards", extractCNFields(t.extractCards)},
		{"skills", extractCNFields(t.extractSkills)},
		{"events", extractCNFields(t.extractEvents)},
		{"information", extractCNFields(t.extractInformation)},
		{"gacha", extractCNFields(t.extractGacha)},
		{"gachaInfo", extractCNFields(t.extractGachaInfo)},
		{"virtualLive", extractCNFields(t.extractVirtualLive)},
		{"sticker", extractCNFields(t.extractStickers)},
		{"comic", extractCNFields(t.extractComics)},
		{"mysekai", extractCNFields(t.extractMysekai)},
		{"costumes", extractCNFields(t.extractCostumes)},
		{"characters", t.extractCharactersCategory},
		{"units", extractCNFields(t.extractUnits)},
		{"music", t.extractMusicCategory},
	}
}

func (t *Translator) SyncCNOnlyContext(ctx context.Context) (CNSyncResult, error) {
	if err := t.markStart(ctx, "cn-sync"); err != nil {
		return CNSyncResult{}, err
	}
	result := CNSyncResult{Mode: "cn-sync"}
	var runErr error
	defer func() {
		note := "cn sync complete"
		if runErr != nil {
			note = "cn sync failed"
		} else if result.AITranslationSkipped > 0 {
			note = fmt.Sprintf("cn sync complete; skipped AI translation for %d event stories", result.AITranslationSkipped)
		}
		t.markEnd(note, runErr)
	}()

	steps := t.cnSyncSteps()

	// Remote extraction is read-only and independent per category. Fetch a
	// bounded number in parallel, then apply translations and their corresponding
	// catalog snapshots to SQLite in the stable category order below. This keeps
	// all database writes serialized while avoiding dozens of latency-bound HTTP
	// requests running one after another.
	fetched := make([]cnExtractedCategory, len(steps))
	jobs := make(chan int)
	done := make(chan int, len(steps))
	workers := t.fetchConcurrency()
	if workers > len(steps) {
		workers = len(steps)
	}
	var fetchWG sync.WaitGroup
	for range workers {
		fetchWG.Add(1)
		go func() {
			defer fetchWG.Done()
			for i := range jobs {
				fetched[i] = steps[i].fn()
				done <- i
			}
		}()
	}
	go func() {
		for i := range steps {
			jobs <- i
		}
		close(jobs)
		fetchWG.Wait()
		close(done)
	}()

	progressTotal := len(steps)*2 + 2
	t.setNote("cn-sync fetching masterdata")
	fetchedCount := 0
	for i := range done {
		fetchedCount++
		t.emit("sync.progress", "已拉取 "+steps[i].category, fetchedCount, progressTotal)
	}

	for i, step := range steps {
		if err := t.runContext().Err(); err != nil {
			runErr = err
			return result, runErr
		}
		t.setNote(fmt.Sprintf("cn-sync %d/%d: %s", i+1, len(steps), step.category))
		t.emit("sync.progress", "正在写入 "+step.category, len(steps)+i+1, progressTotal)
		extracted := fetched[i]
		if extracted.err != nil {
			if isTransientErr(extracted.err) {
				result.addSkipped(step.category, extracted.err)
				continue
			}
			runErr = fmt.Errorf("%s: %w", step.category, extracted.err)
			return result, runErr
		}
		updated, err := t.store.ApplyCNCategoryWithCatalog(
			step.category, extracted.fields, extracted.musicCatalog, extracted.performerCatalog,
		)
		if err != nil {
			runErr = fmt.Errorf("apply %s: %w", step.category, err)
			return result, runErr
		}
		result.Categories++
		result.UpdatedEntries += updated
	}

	t.setNote("cn-sync event stories")
	storyProgress := progressTotal - 1
	t.emit("sync.progress", "正在更新活动剧情", storyProgress, progressTotal)
	storyOutcome, err := t.syncEventStoriesCNOnly(storyProgress, progressTotal)
	if err != nil {
		if isTransientErr(err) {
			result.addSkipped("eventStories", err)
		} else {
			runErr = err
			return result, runErr
		}
	} else {
		result.EventStoryFiles = storyOutcome.Processed
		result.AITranslationSkipped = storyOutcome.AITranslationSkipped
		result.AITranslationNote = storyOutcome.AITranslationNote
		if len(storyOutcome.PartialErrors) > 0 {
			result.addSkipped("eventStories", summarizeErrors(storyOutcome.PartialErrors))
		}
	}
	t.emit("sync.progress", "数据更新完成", progressTotal, progressTotal)
	return result, nil
}

// ---- AI translation ----

// AITranslateRequest targets one category/field for LLM gap-filling.
type AITranslateRequest struct {
	Category string `json:"category"`
	Field    string `json:"field"`
	Provider string `json:"provider"`
	Limit    int    `json:"limit"`
}

// AITranslateResult summarizes an AI translation run for one field.
type AITranslateResult struct {
	Category        string `json:"category"`
	Field           string `json:"field"`
	Provider        string `json:"provider"`
	Candidates      int    `json:"candidates"`
	Translated      int    `json:"translated"`
	SkippedExisting int    `json:"skippedExisting"`
}

// ManualAITranslate fills empty entries in one field via the LLM.
func (t *Translator) ManualAITranslate(req AITranslateRequest) (AITranslateResult, error) {
	return t.ManualAITranslateContext(context.Background(), req)
}

func (t *Translator) ManualAITranslateContext(ctx context.Context, req AITranslateRequest) (AITranslateResult, error) {
	if err := t.markStart(ctx, "manual-ai"); err != nil {
		return AITranslateResult{}, err
	}
	var runErr error
	defer func() { t.markEnd("manual ai complete", runErr) }()

	provider := normalizeProvider(req.Provider, t.cfg.GetOr(config.KeyLLMType, "openai"))
	result := AITranslateResult{Category: req.Category, Field: req.Field, Provider: provider}

	if req.Category == "" || req.Field == "" {
		runErr = fmt.Errorf("category and field are required")
		return result, runErr
	}
	if !model.IsValidCategory(req.Category) {
		runErr = fmt.Errorf("unsupported category: %s", req.Category)
		return result, runErr
	}
	if provider != "gemini" && provider != "openai" {
		runErr = fmt.Errorf("unsupported provider: %s", provider)
		return result, runErr
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	candidates, skipped, err := t.store.AICandidates(req.Category, req.Field, limit)
	if err != nil {
		runErr = err
		return result, runErr
	}
	result.SkippedExisting = skipped
	result.Candidates = len(candidates)
	if len(candidates) == 0 {
		return result, nil
	}
	sort.Strings(candidates)

	updates, translateErr := t.translateBatch(provider, candidates)
	if len(updates) > 0 {
		if err := t.runContext().Err(); err != nil {
			runErr = err
			return result, runErr
		}
		translated, moreSkipped, err := t.store.ApplyAITranslations(req.Category, req.Field, updates)
		if err != nil {
			runErr = err
			return result, runErr
		}
		result.Translated = translated
		result.SkippedExisting += moreSkipped
	}
	if translateErr != nil {
		runErr = translateErr
		return result, runErr
	}
	return result, nil
}

// maxLLMBatchTextBytes caps the Japanese text per request so a batch of
// multi-KB texts (gachaInfo descriptions) stays within provider output limits
// (Gemini maxOutputTokens=8192). Short entries still batch by BatchSize.
const maxLLMBatchTextBytes = 8 << 10

// llmBatches splits keys into consecutive batches of at most batchSize keys and
// maxBytes of text; a single key larger than maxBytes forms its own batch.
func llmBatches(keys []string, batchSize, maxBytes int) [][]string {
	var batches [][]string
	start, size := 0, 0
	for i, key := range keys {
		if i > start && (i-start >= batchSize || size+len(key) > maxBytes) {
			batches = append(batches, keys[start:i])
			start, size = i, 0
		}
		size += len(key)
	}
	if start < len(keys) {
		batches = append(batches, keys[start:])
	}
	return batches
}

// translateBatch runs LLM translation over keys in BatchSize chunks (bounded
// by maxLLMBatchTextBytes), honoring the rate-limit delay. Returns jp -> cn for
// non-empty results.
func (t *Translator) translateBatch(provider string, keys []string) (map[string]string, error) {
	cfg := t.snapshotConfig()
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}
	updates := make(map[string]string, len(keys))
	i := 0
	for _, batch := range llmBatches(keys, batchSize, maxLLMBatchTextBytes) {
		end := i + len(batch)
		log.Printf("[translate] batch %d-%d/%d (provider=%s)", i+1, end, len(keys), provider)
		translated, err := t.callLLMWithAttempts(provider, batch, func(attempt, attempts int) {
			t.emit("translate.progress", fmt.Sprintf("AI 翻译中 %d/%d · 请求 %d/%d", i, len(keys), attempt, attempts), i, len(keys))
		})
		if err != nil {
			log.Printf("[translate] batch %d-%d failed: %v", i+1, end, err)
			return updates, err
		}
		for idx, jp := range batch {
			if idx < len(translated) {
				if cn := strings.TrimSpace(translated[idx]); cn != "" {
					updates[jp] = cn
				}
			}
		}
		t.emit("translate.progress", fmt.Sprintf("AI 翻译已完成 %d/%d", end, len(keys)), end, len(keys))
		if end < len(keys) {
			if err := t.wait(cfg.RateDelay); err != nil {
				return updates, err
			}
		}
		i = end
	}
	return updates, nil
}

func normalizeProvider(provider, fallback string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = strings.ToLower(strings.TrimSpace(fallback))
	}
	return p
}
