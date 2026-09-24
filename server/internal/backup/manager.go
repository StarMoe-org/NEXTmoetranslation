// Package backup provides daily and manual content backup/restore to an
// S3-compatible bucket and a Git repository. These exports deliberately omit
// users, settings, and audit state and are not full database backups.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/files"
	"moesekai/server/internal/importer"
	"moesekai/server/internal/store"
)

var (
	ErrBusy                 = errors.New("backup or restore is already running")
	ErrDraining             = errors.New("backup manager is draining")
	ErrInvalidRestoreTarget = errors.New("invalid restore target")
)

// Status reports the last backup/restore outcome per target.
type Status struct {
	Running       bool   `json:"running"`
	S3Enabled     bool   `json:"s3Enabled"`
	GitEnabled    bool   `json:"gitEnabled"`
	LastBackup    string `json:"lastBackup,omitempty"`
	LastS3Backup  string `json:"lastS3Backup,omitempty"`
	LastGitBackup string `json:"lastGitBackup,omitempty"`
	LastRestore   string `json:"lastRestore,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	LastOperation string `json:"lastOperation,omitempty"`
	LastFinished  string `json:"lastFinished,omitempty"`
	DailyHourUTC  int    `json:"dailyHourUtc"`
}

type restoreCandidate struct {
	payload        importer.Payload
	result         importer.Result
	content        translationContent
	contentPresent bool
}

type backupPayload struct {
	translationsDir string
	contentDir      string
}

// Manager coordinates backup targets and the daily schedule.
type Manager struct {
	cfg      *config.Config
	gen      *files.Generator
	store    *store.Store
	eventStr *store.EventStore
	workDir  string // scratch space for tarballs / git clones

	mu              sync.Mutex
	status          Status
	draining        bool
	ctx             context.Context
	cancel          context.CancelFunc
	schedulerCtx    context.Context
	schedulerCancel context.CancelFunc
	startOnce       sync.Once
	stopOnce        sync.Once
	wg              sync.WaitGroup
	now             func() time.Time
	editorGate      *editorgate.Gate
	afterRestore    func()
}

func (m *Manager) SetEditorGate(gate *editorgate.Gate) {
	m.mu.Lock()
	m.editorGate = gate
	m.mu.Unlock()
}

// SetAfterRestore registers a fast callback that runs only after a restore has
// committed successfully. Realtime document services use it to retire any
// pre-restore in-memory sessions before stale clients can write again.
func (m *Manager) SetAfterRestore(afterRestore func()) {
	m.mu.Lock()
	m.afterRestore = afterRestore
	m.mu.Unlock()
}

func NewManager(cfg *config.Config, gen *files.Generator, s *store.Store, es *store.EventStore, workDir string) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	schedulerCtx, schedulerCancel := context.WithCancel(context.Background())
	return &Manager{
		cfg:             cfg,
		gen:             gen,
		store:           s,
		eventStr:        es,
		workDir:         workDir,
		ctx:             ctx,
		cancel:          cancel,
		schedulerCtx:    schedulerCtx,
		schedulerCancel: schedulerCancel,
		now:             time.Now,
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.S3Enabled = m.cfg.GetBool(config.KeyBackupS3Enabled, false)
	s.GitEnabled = m.cfg.GetBool(config.KeyBackupGitEnabled, false)
	s.DailyHourUTC = m.cfg.GetInt(config.KeyBackupDailyHour, 19) // 19 UTC = 03:00 UTC+8
	return s
}

// StartScheduler runs a daily backup at the configured UTC hour.
func (m *Manager) StartScheduler() {
	m.startOnce.Do(func() {
		m.mu.Lock()
		if m.draining {
			m.mu.Unlock()
			return
		}
		m.wg.Add(1)
		m.mu.Unlock()
		go func() {
			defer m.wg.Done()
			m.scheduleLoop()
		}()
	})
}

// Drain rejects new operations and stops the scheduler without canceling an
// already admitted HTTP operation. Cancel provides the hard-deadline path.
func (m *Manager) Drain() {
	m.mu.Lock()
	m.draining = true
	m.mu.Unlock()
	m.schedulerCancel()
}

func (m *Manager) Cancel() {
	m.stopOnce.Do(func() {
		m.Drain()
		m.cancel()
	})
}

func (m *Manager) Stop() { m.Cancel() }

func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) scheduleLoop() {
	// Retry throughout the day until one complete backup succeeds. Running the
	// check immediately also covers processes started after the configured hour.
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	lastSuccessDay := ""
	m.runScheduledIfDueContext(m.schedulerCtx, &lastSuccessDay)
	for {
		select {
		case <-m.schedulerCtx.Done():
			return
		case <-ticker.C:
			m.runScheduledIfDueContext(m.schedulerCtx, &lastSuccessDay)
		}
	}
}

func (m *Manager) runScheduledIfDue(lastSuccessDay *string) {
	m.runScheduledIfDueContext(context.Background(), lastSuccessDay)
}

func (m *Manager) runScheduledIfDueContext(ctx context.Context, lastSuccessDay *string) {
	now := m.now().UTC()
	hour := m.cfg.GetInt(config.KeyBackupDailyHour, 19)
	day := now.Format("2006-01-02")
	if now.Hour() < hour || day == *lastSuccessDay {
		return
	}
	log.Printf("[backup] daily scheduled backup starting (hour=%d UTC)", hour)
	if _, err := m.BackupAllContext(ctx); err != nil {
		log.Printf("[backup] daily backup failed; will retry: %v", err)
		return
	}
	*lastSuccessDay = day
}

// BackupAll runs every enabled backup target. Returns a per-target summary.
func (m *Manager) BackupAll() (map[string]string, error) {
	return m.BackupAllContext(context.Background())
}

func (m *Manager) BackupAllContext(parent context.Context) (results map[string]string, resultErr error) {
	ctx, finish, err := m.beginOperation(parent, "backup")
	if err != nil {
		return nil, err
	}
	defer func() { finish(resultErr) }()

	s3Enabled := m.cfg.GetBool(config.KeyBackupS3Enabled, false)
	gitEnabled := m.cfg.GetBool(config.KeyBackupGitEnabled, false)
	results = map[string]string{}
	if !s3Enabled && !gitEnabled {
		results["s3"] = "disabled"
		results["git"] = "disabled"
		err := errors.New("no backup targets enabled")
		return results, err
	}
	var firstErr error
	setTargetError := func(target string, targetErr error) {
		log.Printf("[backup] %s failed: %v", target, targetErr)
		results[target] = "error: " + targetErr.Error()
		if firstErr == nil {
			firstErr = targetErr
		}
	}

	log.Printf("[backup] backup starting (s3=%v git=%v)",
		s3Enabled, gitEnabled)

	var s3Cfg s3Settings
	var s3ConfigErr error
	if s3Enabled {
		s3Cfg, s3ConfigErr = m.s3Config()
		if s3ConfigErr != nil {
			setTargetError("s3", s3ConfigErr)
		}
	} else {
		results["s3"] = "disabled"
	}
	var gitRepoURL, gitBranch string
	var gitConfigErr error
	if gitEnabled {
		gitRepoURL, gitBranch, gitConfigErr = m.gitConfig()
		if gitConfigErr != nil {
			setTargetError("git", gitConfigErr)
		}
	} else {
		results["git"] = "disabled"
	}

	validS3 := s3Enabled && s3ConfigErr == nil
	validGit := gitEnabled && gitConfigErr == nil
	var encryptionKey []byte
	if (validS3 || validGit) && ctx.Err() == nil {
		encryptionKey, err = loadBackupEncryptionKey()
		if err != nil {
			if validS3 {
				setTargetError("s3", err)
				validS3 = false
			}
			if validGit {
				setTargetError("git", err)
				validGit = false
			}
		}
	}
	defer clear(encryptionKey)
	if (validS3 || validGit) && ctx.Err() == nil {
		work := filepath.Join(m.workDir, "backup-all")
		_ = os.RemoveAll(work)
		defer os.RemoveAll(work)
		translationsDir, contentDir, materializeErr := m.materializeBackupPayloadContext(ctx, filepath.Join(work, "materialized"))
		if materializeErr != nil {
			if validS3 {
				setTargetError("s3", materializeErr)
				validS3 = false
			}
			if validGit {
				setTargetError("git", materializeErr)
				validGit = false
			}
		}
		var artifact []byte
		if validS3 || validGit {
			artifact, err = encryptBackupPayloadContext(ctx, filepath.Join(work, "artifact"), backupPayload{
				translationsDir: translationsDir,
				contentDir:      contentDir,
			}, encryptionKey)
			if err != nil {
				if validS3 {
					setTargetError("s3", err)
					validS3 = false
				}
				if validGit {
					setTargetError("git", err)
					validGit = false
				}
			}
		}
		defer clear(artifact)

		if validS3 {
			if targetErr := m.publishS3BackupArtifactContext(ctx, s3Cfg, artifact); targetErr != nil {
				setTargetError("s3", targetErr)
			} else {
				log.Printf("[backup] s3 ok")
				results["s3"] = "ok"
				m.mu.Lock()
				m.status.LastS3Backup = nowRFC3339()
				m.mu.Unlock()
			}
		}

		if validGit && ctx.Err() == nil {
			repoDir, targetErr := m.prepareGitBackupRepoContext(ctx, filepath.Join(work, "git"), gitRepoURL, gitBranch)
			if targetErr == nil {
				targetErr = m.publishGitBackupArtifactContext(ctx, repoDir, gitRepoURL, gitBranch, artifact)
			}
			if targetErr != nil {
				setTargetError("git", targetErr)
			} else {
				log.Printf("[backup] git ok")
				results["git"] = "ok"
				m.mu.Lock()
				m.status.LastGitBackup = nowRFC3339()
				m.mu.Unlock()
			}
		}
	}
	if gitEnabled && results["git"] == "" && ctx.Err() != nil {
		setTargetError("git", ctx.Err())
	}
	if s3Enabled && results["s3"] == "" && ctx.Err() != nil {
		setTargetError("s3", ctx.Err())
	}

	if firstErr == nil {
		m.mu.Lock()
		m.status.LastBackup = nowRFC3339()
		m.mu.Unlock()
	}
	return results, firstErr
}

func (m *Manager) beginOperation(parent context.Context, operation string) (context.Context, func(error), error) {
	if parent == nil {
		parent = context.Background()
	}
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		return nil, nil, ErrDraining
	}
	if m.status.Running {
		m.mu.Unlock()
		return nil, nil, ErrBusy
	}
	m.status.Running = true
	m.status.LastOperation = operation
	m.status.LastError = ""
	m.wg.Add(1)
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stopManagerCancel := context.AfterFunc(m.ctx, cancel)
	var once sync.Once
	finish := func(result error) {
		once.Do(func() {
			stopManagerCancel()
			cancel()
			m.mu.Lock()
			m.status.Running = false
			m.status.LastFinished = nowRFC3339()
			m.status.LastError = sanitizeStatusError(result)
			m.mu.Unlock()
			m.wg.Done()
		})
	}
	return ctx, finish, nil
}

// RestoreFrom restores translations from the named target ("s3" or "git") and
// imports them into the stores, replacing current data.
func (m *Manager) RestoreFrom(target string) (importer.Result, error) {
	return m.RestoreFromAsContext(context.Background(), target, "")
}

func (m *Manager) RestoreFromAs(target, actor string) (importer.Result, error) {
	return m.RestoreFromAsContext(context.Background(), target, actor)
}

func (m *Manager) RestoreFromAsContext(parent context.Context, target, actor string) (result importer.Result, resultErr error) {
	if target != "s3" && target != "git" {
		return importer.Result{}, fmt.Errorf("%w: %s", ErrInvalidRestoreTarget, target)
	}
	ctx, finish, err := m.beginOperation(parent, "restore:"+target)
	if err != nil {
		return importer.Result{}, err
	}
	defer func() { finish(resultErr) }()
	m.mu.Lock()
	gate := m.editorGate
	m.mu.Unlock()
	var releaseGate func()
	if gate != nil {
		releaseGate, err = gate.BeginProducerContext(ctx)
		if err != nil {
			return importer.Result{}, err
		}
		defer releaseGate()
	}
	log.Printf("[backup] restore starting (target=%s)", target)
	var candidate restoreCandidate
	switch target {
	case "s3":
		candidate, err = m.prepareS3RestoreContext(ctx)
	case "git":
		candidate, err = m.prepareGitRestoreContext(ctx)
	}
	if err != nil {
		log.Printf("[backup] restore from %s failed: %v", target, err)
		return result, err
	}
	result = candidate.result
	if err := m.applyRestoreCandidate(ctx, candidate, actor); err != nil {
		log.Printf("[backup] restore from %s failed: %v", target, err)
		return result, err
	}
	m.mu.Lock()
	afterRestore := m.afterRestore
	m.mu.Unlock()
	if afterRestore != nil {
		afterRestore()
	}
	log.Printf("[backup] restore from %s ok: %d categories, %d entries, %d event stories",
		target, result.Categories, result.Entries, result.EventStories)
	m.mu.Lock()
	m.status.LastRestore = nowRFC3339()
	m.mu.Unlock()
	return result, nil
}

func (m *Manager) applyRestoreCandidate(ctx context.Context, candidate restoreCandidate, actor string) error {
	releaseContent, err := m.store.LockContentExclusiveContext(ctx)
	if err != nil {
		return err
	}
	defer releaseContent()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.store.RestoreBackupWithSideStoriesContext(ctx, candidate.payload.Categories, candidate.payload.Events,
		candidate.content.Entries, candidate.content.Events, candidate.content.Lyrics, candidate.content.SideStories,
		candidate.contentPresent, actor); err != nil {
		return err
	}
	// The restore replaces event stories behind the event store, whose cached
	// summaries list the events the public rebuild writes. Clearing them while
	// the content lock is still held makes that rebuild list the restored events.
	m.eventStr.InvalidateSummaryCache()
	return nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func sanitizeStatusError(err error) string {
	if err == nil {
		return ""
	}
	message := sanitizeGit(strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " "))
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}
