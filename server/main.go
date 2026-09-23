package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lifecycle"
	"moesekai/server/internal/singleinstance"
)

// runtimeProfile is overridden only in the standalone production binary via
// ldflags. Keeping the selector in the executable prevents container-level
// environment overrides from turning that binary back into development mode.
var runtimeProfile = "development"

const (
	runtimeProfileNextProduction   = "next-production"
	publishedAdminPasswordTemplate = "replace-with-12-or-more-characters"
)

func main() {
	// Timestamped logs (UTC) on stdout so `docker logs` shows operational activity.
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("")

	env := resolveEnvironment()
	if env.verifyOnly {
		logWorkspaceVerification(env)
		return
	}
	settings := resolveRuntimeSettings(env.production)

	instanceOwner, err := singleinstance.Acquire(env.dbPath)
	if err != nil {
		fatal("acquire database ownership", err)
	}
	defer func() {
		if err := instanceOwner.Close(); err != nil {
			log.Printf("release database ownership: %v", err)
		}
	}()
	if err := rejectIncompleteSeed(env.dbPath); err != nil {
		fatal("validate seed publication", err)
	}

	database, err := db.Open(env.dbPath)
	if err != nil {
		fatal("open db", err)
	}
	defer database.Close()

	svc := buildServices(env, settings, database)
	handler, serveWeb := buildHandler(env, settings, svc)
	httpServer := newHTTPServer(":"+settings.port, handler)
	listener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		fatal("listen", err)
	}
	defer listener.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	// Do not launch work that can mutate remote or local state until the process
	// has successfully claimed its TCP address.
	svc.startWorkers()
	logStartupBanner(env, settings, svc.cfg, serveWeb)

	serve := serveUntilSignal(httpServer, listener, signals)
	shutdownErr := lifecycle.RunShutdown(settings.shutdown, log.Printf, os.Exit,
		func(ctx context.Context) error { return svc.drain(ctx, httpServer) },
		func() { svc.cancel(httpServer) },
		func() error {
			serve.wait()
			svc.wait()
			return nil
		})
	if shutdownErr != nil {
		log.Printf("shutdown completed after forced cancellation: %v", shutdownErr)
	}
	if serve.err != nil && serve.err != http.ErrServerClosed {
		_ = database.Close()
		_ = instanceOwner.Close()
		fatal("listen", serve.err)
	}
}

func fatal(ctx string, err error) {
	fmt.Fprintf(os.Stderr, "Fatal: %s: %v\n", ctx, err)
	os.Exit(1)
}
