// Package upstream watches the JP masterdata source for new game data and
// triggers a CN sync when it changes. Instead of polling the GitHub commits API
// (which is rate-limited — see the project notes), it fetches the raw
// versions/current_version.json and compares dataVersion. Optionally it keeps a
// local git clone of the masterdata repo refreshed (git pull) so future work can
// read masterdata from disk without hammering the API.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/httpx"
)

const (
	defaultPollInterval       = time.Hour
	defaultRateLimitCooldown  = time.Hour
	maxRetryAfterCooldown     = 24 * time.Hour
	maxFallbackCooldown       = 6 * time.Hour
	maxVersionResponseBytes   = 1 << 20
	defaultVersionURL         = "https://metadata.pjsk.moe/jp/versions/current_version.json"
	defaultVersionFallbackURL = "https://raw.githubusercontent.com/{repo}/{branch}/versions/current_version.json"
)

var builtInVersionFallbackURLs = []string{
	"https://raw.githubusercontent.com/{repo}/{branch}/versions/current_version.json",
	"https://fastly.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json",
	"https://gcore.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json",
	"https://cdn.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json",
}

// VersionInfo is the subset of current_version.json we care about.
type VersionInfo struct {
	AppVersion   string `json:"appVersion"`
	DataVersion  string `json:"dataVersion"`
	AssetVersion string `json:"assetVersion"`
}

// SyncFn runs a CN sync. Returning an error keeps the change pending for retry.
type SyncFn func() error

type ContextSyncFn func(context.Context) error

// Status reports the watcher's state for the admin UI.
type Status struct {
	Enabled             bool     `json:"enabled"`
	Repo                string   `json:"repo"`
	Branch              string   `json:"branch"`
	VersionURL          string   `json:"versionURL,omitempty"`
	VersionFallbackURL  string   `json:"versionFallbackURL,omitempty"`
	VersionFallbackURLs []string `json:"versionFallbackURLs,omitempty"`
	LastSource          string   `json:"lastSource,omitempty"`
	LastCheck           string   `json:"lastCheck,omitempty"`
	LastSuccess         string   `json:"lastSuccess,omitempty"`
	LastDataVersion     string   `json:"lastDataVersion,omitempty"`
	PendingDataVersion  string   `json:"pendingDataVersion,omitempty"`
	ChangeDetectedAt    string   `json:"changeDetectedAt,omitempty"`
	LastSync            string   `json:"lastSync,omitempty"`
	LastError           string   `json:"lastError,omitempty"`
	LastErrorAt         string   `json:"lastErrorAt,omitempty"`
	ConsecutiveFailures int      `json:"consecutiveFailures,omitempty"`
	RateLimitedUntil    string   `json:"rateLimitedUntil,omitempty"`
	GitMirrorReady      bool     `json:"gitMirrorReady"`
}

// Watcher polls current_version.json and triggers sync on dataVersion change.
type Watcher struct {
	cfg      *config.Config
	syncFn   ContextSyncFn
	client   *http.Client
	interval time.Duration
	gitDir   string // local clone path; empty disables the git mirror
	useGit   bool

	checkMu            sync.Mutex
	mu                 sync.Mutex
	status             Status
	etag               string
	lastModified       string
	validatorURL       string
	cachedVersion      VersionInfo
	pendingDataVersion string
	rateLimitedUntil   time.Time

	ctx       context.Context
	cancel    context.CancelFunc
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// Options configures the watcher.
type Options struct {
	Interval time.Duration // poll interval (default 1h)
	GitDir   string        // local masterdata clone dir; "" disables git mirror
	UseGit   bool          // whether to maintain the git mirror
}

func New(cfg *config.Config, syncFn SyncFn, opts Options) *Watcher {
	var contextual ContextSyncFn
	if syncFn != nil {
		contextual = func(context.Context) error { return syncFn() }
	}
	return newWatcher(cfg, contextual, opts)
}

func NewWithContext(cfg *config.Config, syncFn ContextSyncFn, opts Options) *Watcher {
	return newWatcher(cfg, syncFn, opts)
}

func newWatcher(cfg *config.Config, syncFn ContextSyncFn, opts Options) *Watcher {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	lastDataVersion := cfg.Get(config.KeyUpstreamLastDataVersion)
	pendingDataVersion := cfg.Get(config.KeyUpstreamPendingDataVersion)
	ctx, cancel := context.WithCancel(context.Background())
	return &Watcher{
		cfg:      cfg,
		syncFn:   syncFn,
		client:   httpx.NewClient(30 * time.Second),
		interval: interval,
		gitDir:   opts.GitDir,
		useGit:   opts.UseGit && opts.GitDir != "",
		status: Status{
			LastDataVersion: lastDataVersion, PendingDataVersion: pendingDataVersion,
		},
		pendingDataVersion: pendingDataVersion,
		ctx:                ctx,
		cancel:             cancel,
	}
}

// versionURL builds the current_version.json URL for the configured repo.
func (w *Watcher) versionURL() string {
	repo := w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	return expandVersionURL(w.cfg.Get(config.KeyUpstreamVersionURL), repo, branch)
}

func (w *Watcher) versionURLs() []string {
	repo := w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	primary := expandVersionURL(w.cfg.Get(config.KeyUpstreamVersionURL), repo, branch)
	fallbackSetting := w.cfg.GetOr(config.KeyUpstreamVersionFallbackURL, defaultVersionFallbackURL)
	templates := append(splitVersionTemplates(fallbackSetting), builtInVersionFallbackURLs...)
	urls := []string{primary}
	seen := map[string]bool{primary: true}
	for _, tmpl := range templates {
		url := expandVersionTemplate(tmpl, repo, branch)
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		urls = append(urls, url)
	}
	return urls
}

func splitVersionTemplates(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
}

func expandVersionURL(tmpl, repo, branch string) string {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		tmpl = defaultVersionURL
	}
	return expandVersionTemplate(tmpl, repo, branch)
}

func expandVersionTemplate(tmpl, repo, branch string) string {
	tmpl = strings.TrimSpace(tmpl)
	tmpl = strings.ReplaceAll(tmpl, "{repo}", repo)
	tmpl = strings.ReplaceAll(tmpl, "{branch}", branch)
	return tmpl
}

// Start launches the polling loop unless disabled in config.
func (w *Watcher) Start() {
	w.startOnce.Do(func() {
		if w.ctx.Err() != nil {
			return
		}
		if !w.cfg.GetBool(config.KeySchedulerOn, false) {
			fmt.Println("[upstream] scheduler disabled by config")
			w.setStatus(func(s *Status) { s.Enabled = false })
			return
		}
		w.setStatus(func(s *Status) {
			s.Enabled = true
			s.Repo = w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
			s.Branch = w.cfg.GetOr(config.KeyUpstreamBranch, "main")
			w.updateStatusSources(s)
		})
		if w.useGit {
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				w.ensureGitMirrorContext(w.ctx)
			}()
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.loop()
		}()
	})
}

func (w *Watcher) Stop() { w.stopOnce.Do(w.cancel) }

func (w *Watcher) Wait() { w.wg.Wait() }

func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *Watcher) setStatus(fn func(*Status)) {
	w.mu.Lock()
	fn(&w.status)
	w.mu.Unlock()
}

func (w *Watcher) updateStatusSources(s *Status) {
	urls := w.versionURLs()
	s.VersionURL = ""
	s.VersionFallbackURL = ""
	s.VersionFallbackURLs = nil
	if len(urls) == 0 {
		return
	}
	s.VersionURL = urls[0]
	if len(urls) > 1 {
		s.VersionFallbackURL = urls[1]
		s.VersionFallbackURLs = append([]string(nil), urls[1:]...)
	}
}

func (w *Watcher) loop() {
	// Record a first-run baseline without syncing. A pending version restored
	// from SQLite remains a change and is retried immediately after restart.
	w.checkContext(w.ctx, true)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.checkContext(w.ctx, false)
		}
	}
}

// CheckNow runs an immediate check (admin "check now" button). When force is
// true it triggers a sync even if the version is unchanged.
func (w *Watcher) CheckNow(force bool) (Status, error) {
	return w.CheckNowContext(context.Background(), force)
}

func (w *Watcher) CheckNowContext(ctx context.Context, force bool) (Status, error) {
	changed, err := w.fetchAndCompareContext(ctx)
	if err != nil {
		return w.Status(), err
	}
	if changed || force {
		if err := w.runSyncContext(ctx); err != nil {
			return w.Status(), err
		}
	}
	return w.Status(), nil
}

func (w *Watcher) check(baseline bool) {
	w.checkContext(context.Background(), baseline)
}

func (w *Watcher) checkContext(ctx context.Context, baseline bool) {
	changed, err := w.fetchAndCompareContext(ctx)
	if err != nil {
		fmt.Printf("[upstream] check failed: %v\n", err)
		return
	}
	if baseline && !changed {
		fmt.Printf("[upstream] baseline dataVersion=%s\n", w.Status().LastDataVersion)
		return
	}
	if changed {
		if err := w.runSyncContext(ctx); err != nil {
			fmt.Printf("[upstream] sync failed: %v\n", err)
		}
	}
}

// fetchAndCompare fetches the version file and updates status, returning whether
// dataVersion changed since the last observed value.
func (w *Watcher) fetchAndCompare() (bool, error) {
	return w.fetchAndCompareContext(context.Background())
}

func (w *Watcher) fetchAndCompareContext(ctx context.Context) (bool, error) {
	w.checkMu.Lock()
	defer w.checkMu.Unlock()

	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, sourceURL, err := w.fetchVersionContext(ctx)
	checkedAt := nowRFC3339()
	if err != nil {
		w.setStatus(func(s *Status) {
			s.LastCheck = checkedAt
			s.LastError = err.Error()
			s.LastErrorAt = checkedAt
			s.ConsecutiveFailures++
			w.updateStatusSources(s)
		})
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	status := w.Status()
	changed := status.LastDataVersion != "" && status.LastDataVersion != info.DataVersion
	if status.LastDataVersion == "" {
		if err := w.cfg.Set(config.KeyUpstreamLastDataVersion, info.DataVersion); err != nil {
			return false, fmt.Errorf("persist upstream baseline: %w", err)
		}
	} else if changed {
		if err := w.cfg.Set(config.KeyUpstreamPendingDataVersion, info.DataVersion); err != nil {
			return false, fmt.Errorf("persist pending upstream version: %w", err)
		}
	}
	w.setStatus(func(s *Status) {
		s.LastCheck = checkedAt
		s.LastSuccess = checkedAt
		s.LastError = ""
		s.LastErrorAt = ""
		s.ConsecutiveFailures = 0
		w.updateStatusSources(s)
		s.LastSource = sourceURL
		if changed {
			s.ChangeDetectedAt = checkedAt
			w.pendingDataVersion = info.DataVersion
			s.PendingDataVersion = info.DataVersion
		} else if s.LastDataVersion == "" {
			s.LastDataVersion = info.DataVersion
		}
	})
	if changed {
		fmt.Printf("[upstream] dataVersion changed -> %s\n", info.DataVersion)
	}
	return changed, nil
}

func (w *Watcher) fetchVersion() (VersionInfo, string, error) {
	return w.fetchVersionContext(context.Background())
}

func (w *Watcher) fetchVersionContext(ctx context.Context) (VersionInfo, string, error) {
	var info VersionInfo
	now := time.Now()
	_, _, _, rateLimitedUntil := w.fetchState("")
	if !rateLimitedUntil.IsZero() {
		if now.Before(rateLimitedUntil) {
			return info, "", fmt.Errorf("version fetch paused after HTTP 429; retry after %s", rateLimitedUntil.UTC().Format(time.RFC3339))
		}
		w.clearRateLimit()
	}

	type failedSource struct {
		sourceURL  string
		err        error
		retryAfter string
	}
	type fetchResult struct {
		info       VersionInfo
		sourceURL  string
		retryAfter string
		err        error
	}
	runRound := func(urls []string) (VersionInfo, string, []failedSource, bool) {
		roundCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		results := make(chan fetchResult, len(urls))
		var wg sync.WaitGroup
		for _, sourceURL := range urls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fetched, retryAfter, err := w.fetchVersionURL(roundCtx, sourceURL)
				results <- fetchResult{info: fetched, sourceURL: sourceURL, retryAfter: retryAfter, err: err}
			}()
		}
		defer wg.Wait()
		failures := make([]failedSource, 0, len(urls))
		for range urls {
			result := <-results
			if result.err == nil {
				cancel()
				return result.info, result.sourceURL, failures, true
			}
			failures = append(failures, failedSource{
				sourceURL: result.sourceURL,
				err:       result.err, retryAfter: result.retryAfter,
			})
		}
		return VersionInfo{}, "", failures, false
	}

	urls := w.versionURLs()
	fetched, sourceURL, failures, ok := runRound(urls)
	if ok {
		return fetched, sourceURL, nil
	}

	var retryable []string
	for _, failure := range failures {
		if isRetryableVersionError(failure.err) {
			retryable = append(retryable, failure.sourceURL)
		}
	}
	if len(retryable) > 0 {
		if err := waitContext(ctx, 500*time.Millisecond); err != nil {
			return info, "", err
		}
		fetched, sourceURL, retryFailures, ok := runRound(retryable)
		if ok {
			return fetched, sourceURL, nil
		}
		for _, failure := range retryFailures {
			failure.err = fmt.Errorf("retry: %w", failure.err)
			failures = append(failures, failure)
		}
	}

	for _, failure := range failures {
		if failure.retryAfter == "" && !strings.Contains(failure.err.Error(), "http 429") {
			continue
		}
		until := now.Add(rateLimitCooldown(failure.retryAfter, now, w.interval))
		w.setRateLimit(until)
		break
	}
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, failure.err.Error())
	}
	return info, "", fmt.Errorf("all version sources failed: %s", strings.Join(parts, "; "))
}

func (w *Watcher) fetchVersionURL(ctx context.Context, sourceURL string) (VersionInfo, string, error) {
	var info VersionInfo
	etag, lastModified, cached, _ := w.fetchState(sourceURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return info, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "moesekai-upstream-watcher")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return info, "", fmt.Errorf("GET %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		if cached.DataVersion == "" {
			return info, "", fmt.Errorf("GET %s: http 304 but no cached dataVersion", sourceURL)
		}
		w.updateValidators(resp, cached, sourceURL)
		return cached, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			return info, resp.Header.Get("Retry-After"), fmt.Errorf("GET %s: http 429: version source rate limited", sourceURL)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return info, "", fmt.Errorf("GET %s: http %d: %s", sourceURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := httpx.ReadBody(resp, maxVersionResponseBytes, maxVersionResponseBytes)
	if err != nil {
		return info, "", fmt.Errorf("GET %s: read: %w", sourceURL, err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, "", fmt.Errorf("GET %s: decode: %w", sourceURL, err)
	}
	if info.DataVersion == "" {
		return info, "", fmt.Errorf("GET %s: current_version.json missing dataVersion", sourceURL)
	}
	w.updateValidators(resp, info, sourceURL)
	return info, "", nil
}

func (w *Watcher) fetchState(sourceURL string) (etag, lastModified string, cached VersionInfo, rateLimitedUntil time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if sourceURL == "" || sourceURL == w.validatorURL {
		etag, lastModified = w.etag, w.lastModified
	}
	return etag, lastModified, w.cachedVersion, w.rateLimitedUntil
}

func (w *Watcher) updateValidators(resp *http.Response, info VersionInfo, sourceURL string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cachedVersion = info
	w.validatorURL = sourceURL
	if etag := resp.Header.Get("ETag"); etag != "" || resp.StatusCode == http.StatusOK {
		w.etag = etag
	}
	if lastModified := resp.Header.Get("Last-Modified"); lastModified != "" || resp.StatusCode == http.StatusOK {
		w.lastModified = lastModified
	}
	w.rateLimitedUntil = time.Time{}
	w.status.RateLimitedUntil = ""
}

func isRetryableVersionError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "http 500") ||
		strings.Contains(s, "http 502") ||
		strings.Contains(s, "http 503") ||
		strings.Contains(s, "http 504") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF")
}

func (w *Watcher) setRateLimit(until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rateLimitedUntil = until
	w.status.RateLimitedUntil = until.UTC().Format(time.RFC3339)
}

func (w *Watcher) clearRateLimit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rateLimitedUntil = time.Time{}
	w.status.RateLimitedUntil = ""
}

func rateLimitCooldown(retryAfter string, now time.Time, interval time.Duration) time.Duration {
	if d := parseRetryAfter(retryAfter, now); d > 0 {
		if d > maxRetryAfterCooldown {
			return maxRetryAfterCooldown
		}
		return d
	}
	d := interval * 2
	if d < defaultRateLimitCooldown {
		d = defaultRateLimitCooldown
	}
	if d > maxFallbackCooldown {
		d = maxFallbackCooldown
	}
	return d
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	t, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	if d := t.Sub(now); d > 0 {
		return d
	}
	return 0
}

// SyncStartVersion returns the upstream version a sync starting now processes.
// Callers pass it back to RecordSyncResult so a version published while the sync
// runs is not recorded as done.
func (w *Watcher) SyncStartVersion() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pendingDataVersion
}

// RecordSyncResult updates the shared upstream status after a scheduled or
// manual data sync. A complete manual sync clears a stale watcher error because
// it has just verified the configured upstream data path end to end.
// processedVersion is the version the finished run started from (see
// SyncStartVersion); pending is only cleared while it still matches, so a newer
// version detected mid-sync stays pending for the next tick.
func (w *Watcher) RecordSyncResult(processedVersion string, err error) {
	stamp := nowRFC3339()
	consumed := false
	if err == nil && processedVersion != "" {
		w.mu.Lock()
		consumed = w.pendingDataVersion == processedVersion
		w.mu.Unlock()
		updates := map[string]string{config.KeyUpstreamLastDataVersion: processedVersion}
		if consumed {
			updates[config.KeyUpstreamPendingDataVersion] = ""
		}
		if _, persistErr := w.cfg.SetMany(updates); persistErr != nil {
			err = fmt.Errorf("persist completed upstream version: %w", persistErr)
		}
	}
	w.setStatus(func(s *Status) {
		if err != nil {
			s.LastError = err.Error()
			s.LastErrorAt = stamp
			return
		}
		s.LastSync = stamp
		if processedVersion != "" {
			s.LastDataVersion = processedVersion
		}
		if consumed {
			w.pendingDataVersion = ""
			s.PendingDataVersion = ""
		}
		s.LastSuccess = stamp
		s.LastError = ""
		s.LastErrorAt = ""
		s.ConsecutiveFailures = 0
	})
}

// runSync refreshes the git mirror (if enabled) then runs the CN sync.
func (w *Watcher) runSync() error {
	return w.runSyncContext(context.Background())
}

func (w *Watcher) runSyncContext(ctx context.Context) error {
	if w.useGit {
		if err := w.pullGitMirrorContext(ctx); err != nil {
			fmt.Printf("[upstream] git mirror pull failed (continuing with HTTP sync): %v\n", err)
		}
	}
	if w.syncFn == nil {
		return nil
	}
	processedVersion := w.SyncStartVersion()
	if err := w.syncFn(ctx); err != nil {
		w.RecordSyncResult(processedVersion, err)
		return err
	}
	w.RecordSyncResult(processedVersion, nil)
	fmt.Println("[upstream] sync completed after upstream change")
	return nil
}

// ---- git mirror (optional) ----

func (w *Watcher) repoURL() string {
	repo := w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
	return fmt.Sprintf("https://github.com/%s.git", repo)
}

// ensureGitMirror clones the masterdata repo on first run (shallow), or marks
// the mirror ready if it already exists.
func (w *Watcher) ensureGitMirror() {
	w.ensureGitMirrorContext(context.Background())
}

func (w *Watcher) ensureGitMirrorContext(ctx context.Context) {
	if w.gitDir == "" {
		return
	}
	gitPath := filepath.Join(w.gitDir, ".git")
	if _, err := os.Stat(gitPath); err == nil {
		w.setStatus(func(s *Status) { s.GitMirrorReady = true })
		return
	}
	if err := os.MkdirAll(filepath.Dir(w.gitDir), 0o755); err != nil {
		fmt.Printf("[upstream] git mirror mkdir failed: %v\n", err)
		return
	}
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	fmt.Printf("[upstream] cloning masterdata mirror (shallow) -> %s\n", w.gitDir)
	if err := runGitContext(ctx, w.gitDir, true, "clone", "--depth", "1", "--branch", branch, w.repoURL(), w.gitDir); err != nil {
		fmt.Printf("[upstream] git clone failed: %v\n", err)
		return
	}
	w.setStatus(func(s *Status) { s.GitMirrorReady = true })
	fmt.Println("[upstream] git mirror ready")
}

// pullGitMirror fast-forwards the local mirror. Clones first if absent.
func (w *Watcher) pullGitMirror() error {
	return w.pullGitMirrorContext(context.Background())
}

func (w *Watcher) pullGitMirrorContext(ctx context.Context) error {
	if w.gitDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(w.gitDir, ".git")); err != nil {
		w.ensureGitMirrorContext(ctx)
		return nil
	}
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	return runGitContext(ctx, w.gitDir, false, "pull", "--ff-only", "origin", branch)
}

// runGit runs a git command. When isClone is true, dir is the parent (the clone
// target is in args); otherwise dir is the repo working directory.
func runGit(dir string, isClone bool, args ...string) error {
	return runGitContext(context.Background(), dir, isClone, args...)
}

func runGitContext(parent context.Context, dir string, isClone bool, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if isClone {
		cmd.Dir = filepath.Dir(dir)
	} else {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
