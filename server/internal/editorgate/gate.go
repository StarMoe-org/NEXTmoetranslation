// Package editorgate coordinates process-local producer jobs and editor writes.
package editorgate

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

const MaxSafeCounter uint64 = 1<<53 - 1

var (
	ErrProducerRunning = errors.New("a producer job is already running")
	ErrDraining        = errors.New("application is draining")
)

// Status is the exact version 1 producer state exposed to editor clients.
type Status struct {
	Version             int    `json:"version"`
	InstanceID          string `json:"instanceId"`
	Revision            uint64 `json:"revision"`
	Generation          uint64 `json:"generation"`
	CompletedGeneration uint64 `json:"completedGeneration"`
	Running             bool   `json:"running"`
	LastRun             string `json:"lastRun"`
}

// Gate is a writer-preferred RW gate. A producer publishes Running before it
// waits for current editors, preventing new editors from barging ahead of it.
type Gate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	status   Status
	editors  uint64
	draining bool
	hooks    []func(Status)
}

func New() (*Gate, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate editor gate instance id: %w", err)
	}
	g := &Gate{status: Status{
		Version:    1,
		InstanceID: base64.RawURLEncoding.EncodeToString(random),
	}}
	g.cond = sync.NewCond(&g.mu)
	return g, nil
}

func MustNew() *Gate {
	g, err := New()
	if err != nil {
		panic(err)
	}
	return g
}

func (g *Gate) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

// OnChange registers an observer of producer transitions. Hooks run outside the
// gate lock, so a slow observer cannot block editors or producers.
func (g *Gate) OnChange(hook func(Status)) {
	if hook == nil {
		return
	}
	g.mu.Lock()
	g.hooks = append(g.hooks, hook)
	g.mu.Unlock()
}

func (g *Gate) notify(status Status) {
	g.mu.Lock()
	hooks := g.hooks
	g.mu.Unlock()
	for _, hook := range hooks {
		hook(status)
	}
}

// Drain permanently rejects new producer jobs. An already admitted producer is
// allowed to finish so its generation and transaction boundaries remain intact.
func (g *Gate) Drain() {
	g.mu.Lock()
	g.draining = true
	g.cond.Broadcast()
	g.mu.Unlock()
}

// BeginEditor acquires shared access while no producer is active.
func (g *Gate) BeginEditor() func() {
	release, err := g.BeginEditorContext(context.Background())
	if err != nil {
		panic(err)
	}
	return release
}

// BeginEditorContext rejects requests that arrive after a producer publishes
// its running state. Waiting and applying their pre-producer payload afterward
// could overwrite a restore or translation run with stale content.
func (g *Gate) BeginEditorContext(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.status.Running {
		g.mu.Unlock()
		return nil, ErrProducerRunning
	}
	g.editors++
	g.mu.Unlock()
	return g.editorRelease(), nil
}

// BeginStrictEditor atomically checks the state loaded by a strict client and
// acquires shared access. A false return includes the status that rejected it.
func (g *Gate) BeginStrictEditor(instanceID string, revision, completedGeneration uint64) (func(), Status, bool) {
	g.mu.Lock()
	if g.status.Running || instanceID != g.status.InstanceID || revision != g.status.Revision ||
		completedGeneration != g.status.CompletedGeneration {
		status := g.status
		g.mu.Unlock()
		return nil, status, false
	}
	g.editors++
	status := g.status
	g.mu.Unlock()
	return g.editorRelease(), status, true
}

func (g *Gate) editorRelease() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.editors == 0 {
				g.mu.Unlock()
				panic("editorgate: editor release without acquisition")
			}
			g.editors--
			if g.editors == 0 {
				g.cond.Broadcast()
			}
			g.mu.Unlock()
		})
	}
}

// BeginProducer marks the next generation running before waiting for exclusive
// editor access. A duplicate producer is rejected immediately rather than
// waiting behind the current job.
func (g *Gate) BeginProducer() (func(), error) {
	return g.BeginProducerContext(context.Background())
}

// BeginProducerContext is BeginProducer with cancellation while waiting for
// current editors. Cancellation completes the published generation before
// returning so clients are never left observing a permanently running gate.
func (g *Gate) BeginProducerContext(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.draining {
		g.mu.Unlock()
		return nil, ErrDraining
	}
	if g.status.Running {
		g.mu.Unlock()
		return nil, ErrProducerRunning
	}
	if g.status.Generation >= MaxSafeCounter || g.status.Revision > MaxSafeCounter-2 {
		g.mu.Unlock()
		return nil, errors.New("producer gate counters exhausted")
	}
	g.status.Generation++
	g.status.Revision++
	g.status.Running = true
	started := g.status
	g.cond.Broadcast()
	// New editors are already rejected by the published running state, so the
	// lock can be released to announce the transition before draining editors.
	g.mu.Unlock()
	g.notify(started)

	g.mu.Lock()
	stopWake := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	for g.editors > 0 {
		g.cond.Wait()
		if err := ctx.Err(); err != nil {
			stopWake()
			g.finishProducerLocked()
			canceled := g.status
			g.mu.Unlock()
			g.notify(canceled)
			return nil, err
		}
	}
	stopWake()
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.finishProducerLocked()
			finished := g.status
			g.mu.Unlock()
			g.notify(finished)
		})
	}, nil
}

func (g *Gate) finishProducerLocked() {
	g.status.CompletedGeneration = g.status.Generation
	g.status.Revision++
	g.status.Running = false
	g.status.LastRun = time.Now().UTC().Format(time.RFC3339Nano)
	g.cond.Broadcast()
}
