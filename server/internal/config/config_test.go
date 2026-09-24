package config

import (
	"os"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/httpx"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("MOESEKAI_PRODUCTION", "false")
	_ = os.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "true")
	os.Exit(m.Run())
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/config.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestSecretEncryption(t *testing.T) {
	database := openTestDB(t)
	c, err := New(database, "master-key-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(KeyOpenAIAPIKey, "sk-secret-value"); err != nil {
		t.Fatal(err)
	}
	// Stored value in DB must NOT be the plaintext.
	var stored string
	var enc int
	if err := database.QueryRow(`SELECT value, encrypted FROM settings WHERE key = ?`,
		KeyOpenAIAPIKey).Scan(&stored, &enc); err != nil {
		t.Fatal(err)
	}
	if enc != 1 {
		t.Errorf("expected encrypted=1, got %d", enc)
	}
	if stored == "sk-secret-value" {
		t.Error("secret stored in plaintext")
	}
	// A fresh Config with the same key must decrypt it.
	c2, err := New(database, "master-key-123")
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Get(KeyOpenAIAPIKey); got != "sk-secret-value" {
		t.Errorf("decrypt mismatch: got %q", got)
	}
}

func TestNonSecretPlaintext(t *testing.T) {
	database := openTestDB(t)
	c, _ := New(database, "k")
	if err := c.Set(KeyLLMType, "openai"); err != nil {
		t.Fatal(err)
	}
	var stored string
	var enc int
	database.QueryRow(`SELECT value, encrypted FROM settings WHERE key = ?`, KeyLLMType).Scan(&stored, &enc)
	if enc != 0 || stored != "openai" {
		t.Errorf("non-secret should be plaintext: enc=%d val=%q", enc, stored)
	}
}

func TestSeedIfAbsent(t *testing.T) {
	database := openTestDB(t)
	c, _ := New(database, "k")
	ok, err := c.SetIfAbsent(KeyUpstreamRepo, "owner/repo")
	if err != nil || !ok {
		t.Fatalf("first seed should write: ok=%v err=%v", ok, err)
	}
	// Simulate admin override, then re-seed: must not overwrite.
	c.Set(KeyUpstreamRepo, "admin/changed")
	ok, _ = c.SetIfAbsent(KeyUpstreamRepo, "owner/repo")
	if ok {
		t.Error("second seed should not overwrite existing value")
	}
	if c.Get(KeyUpstreamRepo) != "admin/changed" {
		t.Errorf("admin value lost: %q", c.Get(KeyUpstreamRepo))
	}
}

func TestSetManyIfAbsentIsAtomicAndPreservesExistingValues(t *testing.T) {
	database := openTestDB(t)
	configuration, err := New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configuration.SetManyIfAbsent(map[string]string{
		KeyUpstreamRepo: "owner/repo", KeySchedulerOn: "ture",
	}); err == nil {
		t.Fatal("invalid seed group unexpectedly succeeded")
	}
	if got := configuration.Get(KeyUpstreamRepo); got != "" {
		t.Fatalf("failed seed group partially changed cache: %q", got)
	}
	if err := configuration.Set(KeyUpstreamRepo, "admin/override"); err != nil {
		t.Fatal(err)
	}
	changed, err := configuration.SetManyIfAbsent(map[string]string{
		KeyUpstreamRepo: "seed/value", KeySchedulerOn: "false",
	})
	if err != nil || changed != 1 {
		t.Fatalf("seed group changed=%d err=%v", changed, err)
	}
	if configuration.Get(KeyUpstreamRepo) != "admin/override" || configuration.Get(KeySchedulerOn) != "false" {
		t.Fatalf("seed group values repo=%q scheduler=%q", configuration.Get(KeyUpstreamRepo), configuration.Get(KeySchedulerOn))
	}
}

func TestSecretWithoutMasterKey(t *testing.T) {
	database := openTestDB(t)
	c, _ := New(database, "") // no master key
	if err := c.Set(KeyOpenAIAPIKey, "x"); err == nil {
		t.Error("expected error storing secret without master key")
	}
}

func TestAllMasksSecrets(t *testing.T) {
	database := openTestDB(t)
	c, _ := New(database, "k")
	c.Set(KeyOpenAIAPIKey, "sk-xyz")
	c.Set(KeyLLMType, "openai")
	masked := c.All(false)
	if masked[KeyOpenAIAPIKey] != "********" {
		t.Errorf("secret not masked: %q", masked[KeyOpenAIAPIKey])
	}
	if masked[KeyLLMType] != "openai" {
		t.Errorf("non-secret should not be masked: %q", masked[KeyLLMType])
	}
	revealed := c.All(true)
	if revealed[KeyOpenAIAPIKey] != "sk-xyz" {
		t.Errorf("reveal failed: %q", revealed[KeyOpenAIAPIKey])
	}
}

func TestSetManyRejectsPatchAtomically(t *testing.T) {
	database := openTestDB(t)
	config, _ := New(database, "")
	if _, err := config.SetMany(map[string]string{
		KeyLLMType: "openai", KeyOpenAIAPIKey: "must-not-persist",
	}); err == nil {
		t.Fatal("secret patch without master key unexpectedly succeeded")
	}
	if got := config.Get(KeyLLMType); got != "" {
		t.Fatalf("failed patch changed cache: %q", got)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed patch changed database: count=%d err=%v", count, err)
	}
}

func TestNewRejectsInvalidPersistedTypedSetting(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.Exec(`INSERT INTO settings(key, value, encrypted) VALUES (?, ?, 0)`, KeySchedulerOn, "ture"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(database, ""); err == nil {
		t.Fatal("invalid persisted typed setting was accepted")
	}
}

func TestSetManyValidatesTypedSettingsAtomically(t *testing.T) {
	database := openTestDB(t)
	configuration, err := New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	invalid := map[string]string{
		KeySchedulerOn: "ture", KeyLyricsDiscoveryOn: "1", KeyLyricsFetchRevisionOn: "yes", KeyBackupS3Enabled: "1", KeyLLMType: "custom",
		KeyLLMRequestTimeoutMS: "0", KeyLLMMaxRetries: "6", KeyBatchSize: "0",
		KeyRateDelayMS: "-1", KeyUpstreamFetchConcurrency: "13",
	}
	for key, value := range invalid {
		if _, err := configuration.SetMany(map[string]string{KeyUpstreamRepo: "owner/repo", key: value}); err == nil {
			t.Fatalf("invalid setting %s=%q accepted", key, value)
		}
		if got := configuration.Get(KeyUpstreamRepo); got != "" {
			t.Fatalf("invalid setting %s partially changed config: %q", key, got)
		}
	}
	for key, value := range map[string]string{
		KeySchedulerOn: "false", KeyLyricsDiscoveryOn: "true", KeyLyricsFetchRevisionOn: "false", KeyBackupS3Enabled: "true", KeyLLMType: "openai",
		KeyLLMRequestTimeoutMS: "1", KeyLLMMaxRetries: "5", KeyBatchSize: "200",
		KeyRateDelayMS: "0", KeyUpstreamFetchConcurrency: "12",
	} {
		if err := configuration.Set(key, value); err != nil {
			t.Fatalf("valid setting %s=%q rejected: %v", key, value, err)
		}
	}
}

func TestSecretBearingServiceEndpointsRequireAbsoluteHTTPS(t *testing.T) {
	database := openTestDB(t)
	configuration, err := New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{KeyOpenAIBaseURL, KeyBackupS3Endpoint} {
		for _, value := range []string{
			"http://service.example", "http://192.0.2.10", "https://user:secret@service.example", "https://service.example?token=x",
			"https://service.example/#fragment", "//service.example", "https://service.example/\nheader",
		} {
			if err := configuration.Set(key, value); err == nil {
				t.Fatalf("%s accepted unsafe endpoint %q", key, value)
			}
		}
		for _, value := range []string{"", "https://service.example", "https://service.example/v1", "http://127.0.0.1:8080", "http://[::1]:8080"} {
			if err := configuration.Set(key, value); err != nil {
				t.Fatalf("%s rejected safe endpoint %q: %v", key, value, err)
			}
		}
	}
}

func TestNewRejectsUnsafePersistedServiceEndpoint(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.Exec(`INSERT INTO settings(key, value, encrypted) VALUES (?, ?, 0)`, KeyOpenAIBaseURL, "http://service.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(database, ""); err == nil {
		t.Fatal("unsafe persisted service endpoint was accepted")
	}
}

func TestUpstreamURLSettingsFailClosedInProduction(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "true")
	t.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "true")
	database := openTestDB(t)
	configuration, err := New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		KeyUpstreamVersionURL, KeyUpstreamVersionFallbackURL,
		KeyUpstreamJPMasterdataURL, KeyUpstreamJPMasterdataFallbackURL,
		KeyUpstreamCNMasterdataURL, KeyUpstreamCNMasterdataFallbackURL,
		KeyUpstreamJPAssetsURL, KeyUpstreamJPAssetsFallbackURL,
		KeyUpstreamCNAssetsURL, KeyUpstreamCNAssetsFallbackURL,
		KeyUpstreamENMasterdataURL, KeyUpstreamENMasterdataFallbackURL,
		KeyUpstreamJPScriptsURL, KeyUpstreamJPScriptsFallbackURL,
		KeyUpstreamCNScriptsURL, KeyUpstreamENScriptsURL, KeyMusicAliasesURL,
	}
	unsafe := []string{
		"http://127.0.0.1:8080/data",
		"https://10.0.0.1/data",
		"https://[fd00::1]/data",
		"https://user:secret@example.com/data",
		"https://example.com/data?token=x",
		"https://example.com/data#fragment",
		"https://example.com:8443/data",
		"https://example.com/data\nheader",
	}
	for _, key := range keys {
		for _, value := range unsafe {
			if err := configuration.Set(key, value); err == nil {
				t.Fatalf("%s accepted unsafe production URL %q", key, value)
			}
		}
		if err := configuration.Set(key, "https://example.com:443/path"); err != nil {
			t.Fatalf("%s rejected public HTTPS URL: %v", key, err)
		}
	}
	if err := configuration.Set(KeyUpstreamVersionFallbackURL, "https://one.example/path, https://two.example/path"); err != nil {
		t.Fatalf("safe fallback list rejected: %v", err)
	}
}

func TestUpstreamURLSettingsAllowExplicitLocalDevelopmentOnly(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "false")
	t.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "true")
	database := openTestDB(t)
	configuration, err := New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := configuration.Set(KeyUpstreamVersionURL, "http://127.0.0.1:43210/version.json"); err != nil {
		t.Fatalf("explicit local development URL rejected: %v", err)
	}
	if err := configuration.Set(KeyUpstreamVersionFallbackURL, "http://10.0.0.1:8080/version.json"); err == nil {
		t.Fatal("development override accepted non-loopback private HTTP")
	}
}

func TestNewRejectsPersistedPrivateUpstreamURLInProduction(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "true")
	t.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "false")
	database := openTestDB(t)
	if _, err := database.Exec(`INSERT INTO settings(key, value, encrypted) VALUES (?, ?, 0)`, KeyUpstreamVersionURL, "https://127.0.0.1/version.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(database, ""); err == nil {
		t.Fatal("persisted private upstream URL was accepted in production")
	}
}

func TestSetManyValidatesDailyHourAndKnownKeysAtomically(t *testing.T) {
	database := openTestDB(t)
	configuration, err := New(database, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "-1", "24", "1.5", "01", "+1", " 1"} {
		if _, err := configuration.SetMany(map[string]string{
			KeyLLMType: "openai", KeyBackupDailyHour: value,
		}); err == nil {
			t.Fatalf("invalid daily hour %q accepted", value)
		}
		if got := configuration.Get(KeyLLMType); got != "" {
			t.Fatalf("invalid daily hour %q partially changed config: %q", value, got)
		}
	}
	if _, err := configuration.SetMany(map[string]string{
		KeyBackupDailyHour: "7", "backup.unrecognized": "value",
	}); err == nil {
		t.Fatal("unknown setting accepted")
	}
	if got := configuration.Get(KeyBackupDailyHour); got != "" {
		t.Fatalf("unknown-key patch partially changed daily hour: %q", got)
	}
	for _, value := range []string{"0", "7", "23"} {
		if _, err := configuration.SetMany(map[string]string{KeyBackupDailyHour: value}); err != nil {
			t.Fatalf("valid daily hour %q rejected: %v", value, err)
		}
		if got := configuration.Get(KeyBackupDailyHour); got != value {
			t.Fatalf("daily hour = %q, want %q", got, value)
		}
	}
}

func TestSetManyStoresTrimmedValues(t *testing.T) {
	database := openTestDB(t)
	configuration, err := New(database, "trim-master-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configuration.SetMany(map[string]string{
		KeyOpenAIBaseURL:    "   ",
		KeyBackupS3Endpoint: " https://storage.example ",
		KeyGeminiModel:      "\tgemini-model\n",
		KeyOpenAIAPIKey:     " sk-trimmed\n",
		KeyBackupGitRepoURL: " https://token@git.example/owner/repo.git ",
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		KeyOpenAIBaseURL:    "",
		KeyBackupS3Endpoint: "https://storage.example",
		KeyGeminiModel:      "gemini-model",
		KeyOpenAIAPIKey:     "sk-trimmed",
		KeyBackupGitRepoURL: "https://token@git.example/owner/repo.git",
	}
	reopened, err := New(database, "trim-master-key")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range want {
		if got := configuration.Get(key); got != value {
			t.Fatalf("cached %s = %q, want %q", key, got, value)
		}
		if got := reopened.Get(key); got != value {
			t.Fatalf("persisted %s = %q, want %q", key, got, value)
		}
	}
	if got := configuration.GetOr(KeyOpenAIBaseURL, "https://default.example/v1"); got != "https://default.example/v1" {
		t.Fatalf("whitespace-only base URL shadowed the default: %q", got)
	}
}

func TestSetManyIfAbsentStoresTrimmedValues(t *testing.T) {
	database := openTestDB(t)
	configuration, err := New(database, "trim-master-key")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := configuration.SetManyIfAbsent(map[string]string{
		KeyOpenAIBaseURL:    " https://llm.example/v1 ",
		KeyGeminiModel:      "\tgemini-model\n",
		KeyOpenAIAPIKey:     " sk-seeded\n",
		KeyUpstreamRepo:     "owner/repo ",
		KeyBackupS3Endpoint: "   ",
	})
	if err != nil || changed != 4 {
		t.Fatalf("seed changed=%d err=%v", changed, err)
	}
	want := map[string]string{
		KeyOpenAIBaseURL: "https://llm.example/v1",
		KeyGeminiModel:   "gemini-model",
		KeyOpenAIAPIKey:  "sk-seeded",
		KeyUpstreamRepo:  "owner/repo",
	}
	reopened, err := New(database, "trim-master-key")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range want {
		if got := configuration.Get(key); got != value {
			t.Fatalf("cached %s = %q, want %q", key, got, value)
		}
		if got := reopened.Get(key); got != value {
			t.Fatalf("persisted %s = %q, want %q", key, got, value)
		}
	}
	seeded, err := configuration.SetIfAbsent(KeyBackupS3Endpoint, "https://storage.example")
	if err != nil || !seeded {
		t.Fatalf("whitespace-only seed blocked a later seed: seeded=%v err=%v", seeded, err)
	}
}
