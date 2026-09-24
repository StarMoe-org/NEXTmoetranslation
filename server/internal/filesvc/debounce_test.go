package filesvc

import (
	"testing"
	"time"
)

const testDebounceWindow = 100 * time.Millisecond

// startRecordingRebuilds starts a worker whose generations only record when
// they ran, and returns once the initial generation has.
func startRecordingRebuilds(t *testing.T) (*Service, <-chan time.Time) {
	t.Helper()
	svc := New(nil, nil, nil)
	svc.SetDebounce(testDebounceWindow)
	rebuilds := make(chan time.Time, 64)
	svc.rebuildAssetsFn = func() error {
		rebuilds <- time.Now()
		return nil
	}
	svc.Start()
	t.Cleanup(func() {
		svc.Stop()
		svc.Wait()
	})
	select {
	case <-rebuilds:
	case <-time.After(10 * time.Second):
		t.Fatal("the initial generation did not run")
	}
	return svc, rebuilds
}

func TestDebouncedWriteWaitsOneWindow(t *testing.T) {
	svc, rebuilds := startRecordingRebuilds(t)
	written := time.Now()
	svc.Trigger()
	svc.Trigger()
	select {
	case at := <-rebuilds:
		if waited := at.Sub(written); waited < testDebounceWindow {
			t.Fatalf("a write was published after %s, inside its %s window", waited, testDebounceWindow)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the debounced write was not published")
	}
	select {
	case <-rebuilds:
		t.Fatal("two writes inside one window ran two generations")
	case <-time.After(3 * testDebounceWindow):
	}
}

func TestSteadyWritesStillPublishWithinTwoDebounceWindows(t *testing.T) {
	svc, rebuilds := startRecordingRebuilds(t)
	stop := make(chan struct{})
	writing := make(chan struct{})
	go func() {
		defer close(writing)
		ticker := time.NewTicker(testDebounceWindow / 5)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				svc.Trigger()
			}
		}
	}()
	first := time.Now()
	svc.Trigger()
	var at time.Time
	select {
	case at = <-rebuilds:
	case <-time.After(10 * testDebounceWindow):
	}
	close(stop)
	<-writing
	if at.IsZero() {
		t.Fatalf("no generation ran in %s of writes every %s", 10*testDebounceWindow, testDebounceWindow/5)
	}
	if waited := at.Sub(first); waited > 3*testDebounceWindow {
		t.Fatalf("writes every %s held the generation back for %s", testDebounceWindow/5, waited)
	}
}
