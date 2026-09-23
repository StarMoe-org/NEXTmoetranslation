package main

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsstaging"
)

func TestExecuteParentCancellationDuringFetchReturnsCanceledWithoutManifest(t *testing.T) {
	opts, _, _ := writeLocalInputs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	releaseFetch := make(chan struct{})
	done := make(chan executeCancellationResult, 1)
	go func() {
		manifest, err := execute(ctx, opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			close(started)
			<-releaseFetch
			return lyricssource.FixedRevision{}, errors.New("fetch completed after parent cancellation")
		}})
		done <- executeCancellationResult{manifest: manifest, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fixed-revision fetch did not start")
	}
	cancel()
	close(releaseFetch)
	requireCanceledWithoutManifest(t, done)
}

func TestExecuteParentCancellationInterruptsClientWaitWithoutManifest(t *testing.T) {
	opts, _, _ := writeLocalInputs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan executeCancellationResult, 1)
	go func() {
		manifest, err := execute(ctx, opts, fakeSourceClient{fetchCandidate: func(attemptCtx context.Context, _ lyricssource.MusicIdentity, _ lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			close(started)
			<-attemptCtx.Done()
			return lyricssource.FixedRevision{}, attemptCtx.Err()
		}})
		done <- executeCancellationResult{manifest: manifest, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("client wait did not start")
	}
	cancel()
	requireCanceledWithoutManifest(t, done)
}

func TestExecuteParentCancellationDuringRetryDelayReturnsCanceledWithoutManifest(t *testing.T) {
	opts, _, _ := writeLocalInputs(t)
	opts.MaxAttempts = 2
	opts.RetryDelay = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstAttemptFailed := make(chan struct{})
	releaseFirstAttempt := make(chan struct{})
	done := make(chan executeCancellationResult, 1)
	var calls atomic.Int32
	go func() {
		manifest, err := execute(ctx, opts, fakeSourceClient{fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			calls.Add(1)
			close(firstAttemptFailed)
			<-releaseFirstAttempt
			return lyricssource.FixedRevision{}, &lyricssource.HTTPError{StatusCode: 503}
		}})
		done <- executeCancellationResult{manifest: manifest, err: err}
	}()

	select {
	case <-firstAttemptFailed:
	case <-time.After(time.Second):
		t.Fatal("first fixed-revision fetch did not start")
	}
	cancel()
	close(releaseFirstAttempt)
	requireCanceledWithoutManifest(t, done)
	if got := calls.Load(); got != 1 {
		t.Fatalf("fixed-revision fetch calls=%d, want 1", got)
	}
}

type executeCancellationResult struct {
	manifest lyricsstaging.Manifest
	err      error
}

func requireCanceledWithoutManifest(t *testing.T, done <-chan executeCancellationResult) {
	t.Helper()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("execute error=%v, want context.Canceled", result.err)
		}
		if !reflect.DeepEqual(result.manifest, lyricsstaging.Manifest{}) {
			t.Fatalf("execute returned manifest on cancellation: %+v", result.manifest)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled execute did not return")
	}
}
