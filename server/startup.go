package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/config"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/lifecycle"
	"moesekai/server/internal/workspaceverify"
)

// startupEnv is the environment resolved before the process touches persistent
// state, so an invalid production shape fails before the database is opened.
type startupEnv struct {
	production        bool
	verifyOnly        bool
	webDir            string
	dbPath            string
	dataDir           string
	workspaceMode     string
	workspaceWebDir   string
	verifiedWorkspace *workspaceverify.Manifest
}

// runtimeSettings are the process settings read after a verify-only run has
// already exited.
type runtimeSettings struct {
	port        string
	masterKey   string
	jwtSecret   string
	allowOrigin string
	shutdown    lifecycle.ShutdownConfig
}

func resolveEnvironment() startupEnv {
	verifyWorkspaceOnly := len(os.Args) == 2 && os.Args[1] == "--verify-workspace"
	verifyRuntimeOnly := len(os.Args) == 2 && os.Args[1] == "--verify-runtime"
	if len(os.Args) > 1 && !verifyWorkspaceOnly && !verifyRuntimeOnly {
		fatal("arguments", errors.New("usage: moesekai-server [--verify-workspace|--verify-runtime]"))
	}

	production, err := resolveProductionMode(os.Getenv("MOESEKAI_PRODUCTION"))
	if err != nil {
		fatal("MOESEKAI_PRODUCTION", err)
	}
	timezoneRaw, timezoneConfigured := os.LookupEnv("TZ")
	if err := validateRuntimeTimezone(production, timezoneRaw, timezoneConfigured); err != nil {
		fatal("TZ", err)
	}
	if production {
		// Enforce UTC even if the approved base image's /etc/localtime differs.
		time.Local = time.UTC
	}
	workspaceModeRaw, workspaceModeConfigured := os.LookupEnv("WORKSPACE_MODE")
	workspaceWebDirRaw, workspaceConfigured := os.LookupEnv("WORKSPACE_WEB_DIR")
	workspaceWebDir := strings.TrimSpace(workspaceWebDirRaw)
	workspaceManifestSHARaw, workspaceManifestSHAConfigured := os.LookupEnv("WORKSPACE_MANIFEST_SHA256")
	workspaceManifestSHA := strings.TrimSpace(workspaceManifestSHARaw)
	workspaceConfig := workspaceverify.Config{
		Mode: workspaceModeRaw, Root: workspaceWebDir, ManifestSHA256: workspaceManifestSHA, Production: production,
		ModeConfigured: workspaceModeConfigured, RootConfigured: workspaceConfigured, ManifestSHA256Configured: workspaceManifestSHAConfigured,
	}
	var verifiedWorkspace *workspaceverify.Manifest
	if verifyWorkspaceOnly && !production {
		verifiedWorkspace, err = workspaceverify.Verify(workspaceConfig)
	} else {
		verifiedWorkspace, err = workspaceverify.VerifyRuntime(workspaceConfig)
	}
	if err != nil {
		fatal("workspace verification", err)
	}
	webDirRaw, webDirConfigured := os.LookupEnv("WEB_DIR")
	webDir, err := resolveWebDir(production, webDirRaw, webDirConfigured)
	if err != nil {
		fatal("WEB_DIR", err)
	}
	dbPathRaw, dbPathConfigured := os.LookupEnv("DB_PATH")
	dbPath, err := resolveDBPath(production, dbPathRaw, dbPathConfigured)
	if err != nil {
		fatal("DB_PATH", err)
	}
	dataDirRaw, dataDirConfigured := os.LookupEnv("DATA_DIR")
	dataDir, err := resolveDataDir(production, dataDirRaw, dataDirConfigured)
	if err != nil {
		fatal("DATA_DIR", err)
	}
	return startupEnv{
		production: production, verifyOnly: verifyWorkspaceOnly || verifyRuntimeOnly,
		webDir: webDir, dbPath: dbPath, dataDir: dataDir,
		workspaceMode: workspaceModeRaw, workspaceWebDir: workspaceWebDir, verifiedWorkspace: verifiedWorkspace,
	}
}

func logWorkspaceVerification(env startupEnv) {
	if env.workspaceMode == workspaceverify.ModeDisabled {
		log.Println("workspace verified: disabled")
		return
	}
	if env.verifiedWorkspace == nil {
		log.Println("workspace verification skipped: optional workspace is not configured")
		return
	}
	log.Printf("workspace verified: %s at %s (%s)", env.verifiedWorkspace.Artifact.AppVersion, env.workspaceWebDir, env.verifiedWorkspace.Producer.SourceRevision)
}

func resolveRuntimeSettings(production bool) runtimeSettings {
	settings := runtimeSettings{
		port:        envOr("PORT", "8080"),
		masterKey:   os.Getenv("MOESEKAI_MASTER_KEY"),
		jwtSecret:   envOr("JWT_SECRET", ""),
		allowOrigin: envOr("CONSOLE_ORIGIN", "*"),
	}
	shutdownConfig, err := shutdownConfigFromEnv()
	if err != nil {
		fatal("shutdown configuration", err)
	}
	settings.shutdown = shutdownConfig
	if err := httpx.ValidateUpstreamEnvironment(production); err != nil {
		fatal("upstream network configuration", err)
	}
	if err := validateConsoleOrigin(production, settings.allowOrigin); err != nil {
		fatal("CONSOLE_ORIGIN", err)
	}
	if err := validateProductionMasterKey(production, settings.masterKey); err != nil {
		fatal("production startup validation", err)
	}
	if err := auth.ValidateJWTSecret(settings.jwtSecret); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal: JWT_SECRET: %v\n", err)
		os.Exit(1)
	}
	return settings
}

func logStartupBanner(env startupEnv, settings runtimeSettings, cfg *config.Config, serveWeb bool) {
	log.Printf("moesekai server starting on :%s", settings.port)
	log.Printf("  db:        %s", env.dbPath)
	log.Printf("  data dir:  %s", env.dataDir)
	log.Printf("  files:     /files/* (public, cacheable)")
	log.Printf("  compat:    /translation/* (alias for /files/translation/*)")
	log.Printf("  api:       /api/*   (JWT, no-store)")
	if serveWeb {
		log.Printf("  console:   /       (static SPA from %s)", env.webDir)
	} else {
		log.Printf("  console:   not served (%s not found) — API-only mode", env.webDir)
	}
	log.Printf("  workspace: disabled (retired external workspace is verifier-only)")
	if !cfg.HasMasterKey() {
		log.Println("  WARNING: MOESEKAI_MASTER_KEY not set — secrets cannot be stored")
	}
}
