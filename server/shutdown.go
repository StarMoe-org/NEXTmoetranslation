package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
)

// serveResult owns the HTTP serve error until the shutdown sequence joins the
// serve goroutine.
type serveResult struct {
	errs     <-chan error
	err      error
	consumed bool
}

// serveUntilSignal returns once the server stops on its own or a termination
// signal arrives.
func serveUntilSignal(httpServer *http.Server, listener net.Listener, signals <-chan os.Signal) *serveResult {
	errs := make(chan error, 1)
	go func() { errs <- httpServer.Serve(listener) }()
	result := &serveResult{errs: errs}
	select {
	case err := <-errs:
		result.err = err
		result.consumed = true
	case sig := <-signals:
		log.Printf("shutdown requested by %s", sig)
	}
	return result
}

func (r *serveResult) wait() {
	if !r.consumed {
		r.err = <-r.errs
		r.consumed = true
	}
}

// drain closes every admission gate first. The HTTP listener then closes and
// already-admitted handlers receive only the short drain phase.
func (s *services) drain(ctx context.Context, httpServer *http.Server) error {
	s.lifecycle.Drain()
	s.editorGate.Drain()
	s.backup.Drain()
	if s.lyricsFetch != nil {
		s.lyricsFetch.Drain()
	}
	if s.lyricsDiscovery != nil {
		s.lyricsDiscovery.Drain()
	}
	if err := s.collab.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("shutdown lyrics collaboration: %v", err)
	}
	return httpServer.Shutdown(ctx)
}

// cancel starts hard cancellation for everything at once so no worker loses the
// remaining total budget behind another component's Wait.
func (s *services) cancel(httpServer *http.Server) {
	if s.lyricsFetch != nil {
		s.lyricsFetch.Cancel()
	}
	if s.lyricsDiscovery != nil {
		s.lyricsDiscovery.Cancel()
	}
	s.translator.Cancel()
	s.backup.Cancel()
	s.upstream.Stop()
	s.search.Stop()
	s.files.Stop()
	s.hub.Close()
	s.lifecycle.StopProbes()
	if err := httpServer.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("force-close HTTP: %v", err)
	}
}

func (s *services) wait() {
	if s.lyricsDiscovery != nil {
		s.lyricsDiscovery.Wait()
	}
	if s.lyricsFetch != nil {
		s.lyricsFetch.Wait()
	}
	s.files.Wait()
	s.search.Wait()
	s.upstream.Wait()
	s.backup.Wait()
	s.translator.Wait()
	s.lifecycle.Wait()
}
