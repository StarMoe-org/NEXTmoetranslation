package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/config"
	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/searchindex"
	"moesekai/server/internal/store"
)

func rejectIncompleteSeed(databasePath string) error {
	marker := databasePath + ".seed-incomplete"
	if _, err := os.Stat(marker); err == nil {
		return fmt.Errorf("incomplete seed publication marker exists: %s", marker)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect incomplete seed publication marker: %w", err)
	}
	return nil
}

// applyEmbeddedLyricsEditorSeed installs the compiled-in editor seed, which only
// the standalone production binary carries.
func applyEmbeddedLyricsEditorSeed(st *store.Store) {
	if runtimeProfile != runtimeProfileNextProduction {
		return
	}
	editorSeed, err := embeddedlyricsseed.Load()
	if err != nil {
		fatal("load embedded lyrics editor seed", err)
	}
	seedResult, err := st.ApplyEmbeddedLyricsEditorSeed(context.Background(), editorSeed)
	if errors.Is(err, store.ErrEmbeddedLyricsEditorSeedCatalogNotReady) {
		log.Printf("embedded lyrics editor seed: deferred because catalog is not initialized")
	} else if err != nil {
		fatal("apply embedded lyrics editor seed", err)
	} else {
		log.Printf("embedded lyrics editor seed: seed=%s inserted=%d preserved=%d replayed=%d sourceV3=%d legacy=%d availability=%d",
			seedResult.SeedSHA256, seedResult.Inserted, seedResult.PreservedExisting, seedResult.Replayed,
			seedResult.SourceV3, seedResult.Legacy, seedResult.Availability)
	}
}

// seedConfigFromEnv writes settings from env vars on first run only, leaving the
// admin UI authoritative thereafter.
func seedConfigFromEnv(cfg *config.Config) error {
	seed := map[string]string{
		config.KeyLLMType:                         os.Getenv("LLM_TYPE"),
		config.KeyGeminiAPIKey:                    os.Getenv("GEMINI_API_KEY"),
		config.KeyGeminiModel:                     os.Getenv("GEMINI_MODEL"),
		config.KeyOpenAIAPIKey:                    os.Getenv("OPENAI_API_KEY"),
		config.KeyOpenAIBaseURL:                   os.Getenv("OPENAI_BASE_URL"),
		config.KeyOpenAIModel:                     os.Getenv("OPENAI_MODEL"),
		config.KeyLLMRequestTimeoutMS:             envOr("LLM_REQUEST_TIMEOUT_MS", "45000"),
		config.KeyLLMMaxRetries:                   envOr("LLM_MAX_RETRIES", "2"),
		config.KeyBatchSize:                       envOr("TRANSLATE_BATCH_SIZE", "20"),
		config.KeyRateDelayMS:                     envOr("TRANSLATE_RATE_DELAY_MS", "800"),
		config.KeyUpstreamRepo:                    envOr("UPSTREAM_REPO", "Team-Haruki/haruki-sekai-master"),
		config.KeyUpstreamBranch:                  envOr("UPSTREAM_BRANCH", "main"),
		config.KeyUpstreamVersionURL:              os.Getenv("UPSTREAM_VERSION_URL"),
		config.KeyUpstreamVersionFallbackURL:      os.Getenv("UPSTREAM_VERSION_FALLBACK_URL"),
		config.KeyUpstreamJPMasterdataURL:         os.Getenv("UPSTREAM_JP_MASTERDATA_URL"),
		config.KeyUpstreamJPMasterdataFallbackURL: os.Getenv("UPSTREAM_JP_MASTERDATA_FALLBACK_URL"),
		config.KeyUpstreamCNMasterdataURL:         os.Getenv("UPSTREAM_CN_MASTERDATA_URL"),
		config.KeyUpstreamCNMasterdataFallbackURL: os.Getenv("UPSTREAM_CN_MASTERDATA_FALLBACK_URL"),
		config.KeyUpstreamJPAssetsURL:             os.Getenv("UPSTREAM_JP_ASSETS_URL"),
		config.KeyUpstreamJPAssetsFallbackURL:     os.Getenv("UPSTREAM_JP_ASSETS_FALLBACK_URL"),
		config.KeyUpstreamCNAssetsURL:             os.Getenv("UPSTREAM_CN_ASSETS_URL"),
		config.KeyUpstreamCNAssetsFallbackURL:     os.Getenv("UPSTREAM_CN_ASSETS_FALLBACK_URL"),
		config.KeyUpstreamENMasterdataURL:         os.Getenv("UPSTREAM_EN_MASTERDATA_URL"),
		config.KeyUpstreamENMasterdataFallbackURL: os.Getenv("UPSTREAM_EN_MASTERDATA_FALLBACK_URL"),
		config.KeyUpstreamJPScriptsURL:            os.Getenv("UPSTREAM_JP_SCRIPTS_URL"),
		config.KeyUpstreamJPScriptsFallbackURL:    os.Getenv("UPSTREAM_JP_SCRIPTS_FALLBACK_URL"),
		config.KeyUpstreamCNScriptsURL:            os.Getenv("UPSTREAM_CN_SCRIPTS_URL"),
		config.KeyUpstreamENScriptsURL:            os.Getenv("UPSTREAM_EN_SCRIPTS_URL"),
		config.KeyUpstreamFetchConcurrency:        os.Getenv("UPSTREAM_FETCH_CONCURRENCY"),
		config.KeyMusicAliasesURL:                 envOr("MUSIC_ALIASES_URL", searchindex.DefaultMusicAliasesURL),
		config.KeySchedulerOn:                     envOr("TRANSLATE_SCHEDULER_ENABLED", "false"),
		config.KeyLyricsDiscoveryOn:               envOr("LYRICS_DISCOVERY_ENABLED", "false"),
		config.KeyLyricsFetchRevisionOn:           envOr("LYRICS_FETCH_REVISION_ENABLED", "false"),
		config.KeyBackupGitRepoURL:                os.Getenv("GIT_PUSH_REPO_URL"),
		config.KeyBackupGitBranch:                 envOr("GIT_PUSH_BRANCH", "backup-translations"),
		config.KeyBackupS3Bucket:                  os.Getenv("BACKUP_S3_BUCKET"),
		config.KeyBackupS3Region:                  os.Getenv("BACKUP_S3_REGION"),
		config.KeyBackupS3Endpoint:                os.Getenv("BACKUP_S3_ENDPOINT"),
		config.KeyBackupS3AccessKey:               os.Getenv("BACKUP_S3_ACCESS_KEY"),
		config.KeyBackupS3SecretKey:               os.Getenv("BACKUP_S3_SECRET_KEY"),
	}
	for key, value := range seed {
		if config.IsSecret(key) && !cfg.HasMasterKey() {
			delete(seed, key)
			continue
		}
		seed[key] = value
	}
	seeded, err := cfg.SetManyIfAbsent(seed)
	if err != nil {
		return err
	}
	if seeded > 0 {
		log.Printf("[config] seeded %d settings from environment", seeded)
	}
	return nil
}

// seedAdminFromEnv guarantees an administrator from TRANSLATOR_ACCOUNTS (legacy
// "user:pass,user2:pass2") or ADMIN_USER/ADMIN_PASSWORD when none exists.
func seedAdminFromEnv(a *auth.Auth) error {
	adminCount, err := a.CountAdmins()
	if err != nil || adminCount > 0 {
		return err
	}
	userCount, err := a.CountUsers()
	if err != nil {
		return err
	}
	created := 0
	adminCreated := false
	if accts := os.Getenv("TRANSLATOR_ACCOUNTS"); accts != "" {
		for _, pair := range strings.Split(accts, ",") {
			parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
			if len(parts) != 2 {
				continue
			}
			if err := validateBootstrapAdminPassword(parts[1]); err != nil {
				return err
			}
			role := auth.RoleEditor
			if !adminCreated {
				role = auth.RoleAdmin
			}
			if _, err := a.CreateUser(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), role); err == nil {
				created++
				adminCreated = adminCreated || role == auth.RoleAdmin
			}
		}
	}
	if !adminCreated {
		user := envOr("ADMIN_USER", "admin")
		pass := envOr("ADMIN_PASSWORD", "")
		if pass != "" {
			if err := validateBootstrapAdminPassword(pass); err != nil {
				return err
			}
			if _, err := a.CreateUser(user, pass, auth.RoleAdmin); err == nil {
				created++
				adminCreated = true
				log.Printf("[auth] created initial admin %q from env", user)
			}
		}
	}
	if created > 0 {
		log.Printf("[auth] seeded %d account(s) from environment", created)
	}
	if !adminCreated && userCount > 0 {
		return fmt.Errorf("database contains users but no administrator; provide a unique strong ADMIN_USER/ADMIN_PASSWORD")
	}
	return nil
}

func validateBootstrapAdminPassword(value string) error {
	if strings.TrimSpace(value) == publishedAdminPasswordTemplate {
		return errors.New("published ADMIN_PASSWORD template must be replaced with unique secret material")
	}
	return nil
}
