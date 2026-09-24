package searchindex

import (
	"testing"
	"time"
)

func TestSteadyTriggersStillRebuildWithinTwoDebounceWindows(t *testing.T) {
	const window = 100 * time.Millisecond
	b := New(nil, nil, nil, window, time.Hour)
	t.Cleanup(b.Stop)
	stop := make(chan struct{})
	triggering := make(chan struct{})
	go func() {
		defer close(triggering)
		ticker := time.NewTicker(window / 5)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				b.Trigger()
			}
		}
	}()
	first := time.Now()
	done := make(chan bool, 1)
	go func() { done <- b.waitForDebounce() }()
	var waited time.Duration
	select {
	case ok := <-done:
		waited = time.Since(first)
		if !ok {
			t.Fatal("the debounce wait was cancelled")
		}
	case <-time.After(10 * window):
	}
	close(stop)
	<-triggering
	if waited == 0 {
		t.Fatalf("no rebuild in %s of triggers every %s", 10*window, window/5)
	}
	if waited > 3*window {
		t.Fatalf("triggers every %s held the rebuild back for %s", window/5, waited)
	}
}
