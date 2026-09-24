// Package config manages runtime settings stored in SQLite. Non-secret values
// are stored in plaintext; secrets (API keys, backup credentials) are encrypted
// with AES-GCM using a key derived from the MOESEKAI_MASTER_KEY env var.
//
// Settings are seeded from environment variables on first run only, so the
// admin UI becomes the source of truth thereafter.
package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"moesekai/server/internal/db"
	"moesekai/server/internal/httpx"
)

// Setting keys. Secret keys must be listed in secretKeys below.
const (
	KeyLLMType             = "llm.type"       // "gemini" | "openai"
	KeyGeminiAPIKey        = "llm.gemini.key" // secret
	KeyGeminiModel         = "llm.gemini.model"
	KeyOpenAIAPIKey        = "llm.openai.key" // secret
	KeyOpenAIBaseURL       = "llm.openai.base_url"
	KeyOpenAIModel         = "llm.openai.model"
	KeyLLMRequestTimeoutMS = "llm.request_timeout_ms"
	KeyLLMMaxRetries       = "llm.max_retries"
	KeyBatchSize           = "translate.batch_size"
	KeyRateDelayMS         = "translate.rate_delay_ms"

	KeyUpstreamRepo                    = "upstream.repo"   // e.g. Team-Haruki/haruki-sekai-master
	KeyUpstreamBranch                  = "upstream.branch" // e.g. main
	KeyUpstreamVersionURL              = "upstream.version_url"
	KeyUpstreamVersionFallbackURL      = "upstream.version_fallback_url"
	KeyUpstreamJPMasterdataURL         = "upstream.jp_masterdata_url"
	KeyUpstreamJPMasterdataFallbackURL = "upstream.jp_masterdata_fallback_url"
	KeyUpstreamCNMasterdataURL         = "upstream.cn_masterdata_url"
	KeyUpstreamCNMasterdataFallbackURL = "upstream.cn_masterdata_fallback_url"
	KeyUpstreamJPAssetsURL             = "upstream.jp_assets_url"
	KeyUpstreamJPAssetsFallbackURL     = "upstream.jp_assets_fallback_url"
	KeyUpstreamCNAssetsURL             = "upstream.cn_assets_url"
	KeyUpstreamCNAssetsFallbackURL     = "upstream.cn_assets_fallback_url"
	KeyUpstreamENMasterdataURL         = "upstream.en_masterdata_url"
	KeyUpstreamENMasterdataFallbackURL = "upstream.en_masterdata_fallback_url"
	KeyUpstreamJPScriptsURL            = "upstream.jp_scripts_url"
	KeyUpstreamJPScriptsFallbackURL    = "upstream.jp_scripts_fallback_url"
	KeyUpstreamCNScriptsURL            = "upstream.cn_scripts_url"
	KeyUpstreamENScriptsURL            = "upstream.en_scripts_url"
	KeyUpstreamFetchConcurrency        = "upstream.fetch_concurrency"
	KeyMusicAliasesURL                 = "upstream.music_aliases_url"
	KeySchedulerOn                     = "scheduler.enabled"
	KeyLyricsDiscoveryOn               = "lyrics_discovery.enabled"
	KeyLyricsFetchRevisionOn           = "lyrics_discovery.fetch_revision.enabled"
	KeyUpstreamLastDataVersion         = "upstream.state.last_data_version"
	KeyUpstreamPendingDataVersion      = "upstream.state.pending_data_version"

	KeyBackupS3Enabled   = "backup.s3.enabled"
	KeyBackupS3Endpoint  = "backup.s3.endpoint"
	KeyBackupS3Region    = "backup.s3.region"
	KeyBackupS3Bucket    = "backup.s3.bucket"
	KeyBackupS3Prefix    = "backup.s3.prefix"
	KeyBackupS3AccessKey = "backup.s3.access_key" // secret
	KeyBackupS3SecretKey = "backup.s3.secret_key" // secret

	KeyBackupGitEnabled = "backup.git.enabled"
	KeyBackupGitRepoURL = "backup.git.repo_url" // secret (may embed token)
	KeyBackupGitBranch  = "backup.git.branch"

	KeyBackupDailyHour = "backup.daily_hour" // UTC hour 0-23
)

// secretKeys are stored encrypted at rest.
var secretKeys = map[string]bool{
	KeyGeminiAPIKey:      true,
	KeyOpenAIAPIKey:      true,
	KeyBackupS3AccessKey: true,
	KeyBackupS3SecretKey: true,
	KeyBackupGitRepoURL:  true,
}

var settingKeys = map[string]bool{
	KeyLLMType: true, KeyGeminiAPIKey: true, KeyGeminiModel: true,
	KeyOpenAIAPIKey: true, KeyOpenAIBaseURL: true, KeyOpenAIModel: true,
	KeyLLMRequestTimeoutMS: true, KeyLLMMaxRetries: true, KeyBatchSize: true, KeyRateDelayMS: true,
	KeyUpstreamRepo: true, KeyUpstreamBranch: true, KeyUpstreamVersionURL: true,
	KeyUpstreamVersionFallbackURL: true, KeyUpstreamJPMasterdataURL: true,
	KeyUpstreamJPMasterdataFallbackURL: true, KeyUpstreamCNMasterdataURL: true,
	KeyUpstreamCNMasterdataFallbackURL: true, KeyUpstreamJPAssetsURL: true,
	KeyUpstreamJPAssetsFallbackURL: true, KeyUpstreamCNAssetsURL: true,
	KeyUpstreamCNAssetsFallbackURL: true, KeyUpstreamENMasterdataURL: true, KeyUpstreamENMasterdataFallbackURL: true,
	KeyUpstreamJPScriptsURL: true, KeyUpstreamJPScriptsFallbackURL: true, KeyUpstreamCNScriptsURL: true,
	KeyUpstreamENScriptsURL: true, KeyUpstreamFetchConcurrency: true, KeyMusicAliasesURL: true,
	KeySchedulerOn: true, KeyLyricsDiscoveryOn: true, KeyLyricsFetchRevisionOn: true, KeyUpstreamLastDataVersion: true, KeyUpstreamPendingDataVersion: true,
	KeyBackupS3Enabled: true, KeyBackupS3Endpoint: true, KeyBackupS3Region: true,
	KeyBackupS3Bucket: true, KeyBackupS3Prefix: true, KeyBackupS3AccessKey: true,
	KeyBackupS3SecretKey: true, KeyBackupGitEnabled: true, KeyBackupGitRepoURL: true,
	KeyBackupGitBranch: true, KeyBackupDailyHour: true,
}

// IsSecret reports whether a setting key holds a secret value.
func IsSecret(key string) bool { return secretKeys[key] }

// Config provides typed, cached access to settings backed by SQLite.
type Config struct {
	db     *db.DB
	aesKey []byte

	mu      sync.RWMutex
	cache   map[string]string // decrypted values
	writeMu sync.Mutex
}

// New opens the config over the given DB. masterKey may be empty, in which case
// secret settings cannot be stored (an error is returned on write attempts).
func New(database *db.DB, masterKey string) (*Config, error) {
	c := &Config{db: database, cache: map[string]string{}}
	if masterKey != "" {
		sum := sha256.Sum256([]byte(masterKey))
		c.aesKey = sum[:]
	}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// HasMasterKey reports whether secret encryption is available.
func (c *Config) HasMasterKey() bool { return len(c.aesKey) == 32 }

func (c *Config) reload() error {
	rows, err := c.db.Query(`SELECT key, value, encrypted FROM settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cache := map[string]string{}
	canonical := map[string]string{}
	for rows.Next() {
		var key, value string
		var enc int
		if err := rows.Scan(&key, &value, &enc); err != nil {
			return err
		}
		if enc == 1 {
			if !c.HasMasterKey() {
				// Cannot decrypt without the key; skip (treated as unset).
				continue
			}
			dec, err := c.decrypt(value)
			if err != nil {
				return fmt.Errorf("decrypt %s: %w", key, err)
			}
			value = dec
		}
		if normalized, legacy := canonicalSettingValue(key, value); legacy {
			canonical[key] = normalized
			value = normalized
		}
		if err := validateSettingValue(key, value); err != nil {
			return fmt.Errorf("invalid persisted setting: %w", err)
		}
		cache[key] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := c.persistCanonicalSettings(canonical); err != nil {
		return err
	}
	c.mu.Lock()
	c.cache = cache
	c.mu.Unlock()
	return nil
}

// canonicalSettingValue rewrites the boolean and integer spellings that older
// binaries accepted into the single form validateSettingValue allows. Values
// that are not a legacy spelling are returned unchanged so they still fail
// validation.
func canonicalSettingValue(key, value string) (string, bool) {
	switch key {
	case KeySchedulerOn, KeyLyricsDiscoveryOn, KeyLyricsFetchRevisionOn, KeyBackupS3Enabled, KeyBackupGitEnabled:
		switch value {
		case "1", "yes", "on":
			return "true", true
		case "0", "no", "off":
			return "false", true
		}
	case KeyLLMRequestTimeoutMS, KeyLLMMaxRetries, KeyBatchSize, KeyRateDelayMS,
		KeyUpstreamFetchConcurrency, KeyBackupDailyHour:
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return value, false
		}
		if normalized := strconv.Itoa(number); normalized != value {
			return normalized, true
		}
	}
	return value, false
}

// persistCanonicalSettings stores the rewritten values so later readers and
// writers see the same canonical form. Only plaintext rows are rewritten;
// no boolean or integer setting is a secret.
func (c *Config) persistCanonicalSettings(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.Exec(`UPDATE settings SET value=? WHERE key=? AND encrypted=0`, value, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Get returns a setting value, or the empty string if unset.
func (c *Config) Get(key string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache[key]
}

// GetOr returns the setting value or fallback if empty.
func (c *Config) GetOr(key, fallback string) string {
	if v := c.Get(key); v != "" {
		return v
	}
	return fallback
}

// GetBool parses a boolean setting (true/1/yes). fallback used if unset.
func (c *Config) GetBool(key string, fallback bool) bool {
	v := c.Get(key)
	if v == "" {
		return fallback
	}
	switch v {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return fallback
}

// GetInt parses an integer setting. fallback used if unset or invalid.
func (c *Config) GetInt(key string, fallback int) int {
	v := c.Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// Set writes a setting, encrypting it if the key is a secret.
func (c *Config) Set(key, value string) error {
	_, err := c.SetMany(map[string]string{key: value})
	return err
}

// SetMany validates, encrypts, and commits a settings patch atomically. The
// in-memory cache is published only after the database transaction commits.
func (c *Config) SetMany(values map[string]string) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	type encodedSetting struct {
		key, value, stored string
		encrypted          int
	}
	encoded := make([]encodedSetting, 0, len(values))
	for key, value := range values {
		// Some validators trim internally, so the submitted value is validated
		// as given but stored trimmed; a whitespace-only value then clears the
		// setting instead of shadowing its default.
		if err := validateSettingValue(key, value); err != nil {
			return 0, err
		}
		value = strings.TrimSpace(value)
		stored, encrypted, err := c.encodeSetting(key, value)
		if err != nil {
			return 0, err
		}
		encoded = append(encoded, encodedSetting{key: key, value: value, stored: stored, encrypted: encrypted})
	}
	tx, err := c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, setting := range encoded {
		if _, err := tx.Exec(`INSERT INTO settings (key, value, encrypted) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, encrypted = excluded.encrypted`,
			setting.key, setting.stored, setting.encrypted); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	for _, setting := range encoded {
		c.cache[setting.key] = setting.value
	}
	c.mu.Unlock()
	return len(encoded), nil
}

func (c *Config) encodeSetting(key, value string) (string, int, error) {
	if !settingKeys[key] {
		return "", 0, fmt.Errorf("unknown setting %q", key)
	}
	if err := validateSettingValue(key, value); err != nil {
		return "", 0, err
	}
	enc := 0
	stored := value
	if IsSecret(key) {
		if !c.HasMasterKey() {
			return "", 0, errors.New("cannot store secret: MOESEKAI_MASTER_KEY not configured")
		}
		e, err := c.encrypt(value)
		if err != nil {
			return "", 0, err
		}
		stored = e
		enc = 1
	}
	return stored, enc, nil
}

func validateSettingValue(key, value string) error {
	canonicalBool := func() error {
		if value != "true" && value != "false" {
			return fmt.Errorf("%s must be true or false", key)
		}
		return nil
	}
	canonicalInt := func(minimum, maximum int) error {
		number, err := strconv.Atoi(value)
		if err != nil || number < minimum || number > maximum || strconv.Itoa(number) != value {
			return fmt.Errorf("%s must be an integer from %d through %d", key, minimum, maximum)
		}
		return nil
	}
	switch key {
	case KeySchedulerOn, KeyLyricsDiscoveryOn, KeyLyricsFetchRevisionOn, KeyBackupS3Enabled, KeyBackupGitEnabled:
		return canonicalBool()
	case KeyLLMType:
		if value != "gemini" && value != "openai" {
			return fmt.Errorf("%s must be gemini or openai", key)
		}
	case KeyOpenAIBaseURL, KeyBackupS3Endpoint:
		return validateSecretServiceURL(key, value)
	case KeyUpstreamVersionURL, KeyUpstreamVersionFallbackURL,
		KeyUpstreamJPMasterdataURL, KeyUpstreamJPMasterdataFallbackURL,
		KeyUpstreamCNMasterdataURL, KeyUpstreamCNMasterdataFallbackURL,
		KeyUpstreamJPAssetsURL, KeyUpstreamJPAssetsFallbackURL,
		KeyUpstreamCNAssetsURL, KeyUpstreamCNAssetsFallbackURL,
		KeyUpstreamENMasterdataURL, KeyUpstreamENMasterdataFallbackURL,
		KeyUpstreamJPScriptsURL, KeyUpstreamJPScriptsFallbackURL,
		KeyUpstreamCNScriptsURL, KeyUpstreamENScriptsURL, KeyMusicAliasesURL:
		return validateUpstreamURLSetting(key, value)
	case KeyLLMRequestTimeoutMS:
		return canonicalInt(1, 300000)
	case KeyLLMMaxRetries:
		return canonicalInt(0, 5)
	case KeyBatchSize:
		return canonicalInt(1, 200)
	case KeyRateDelayMS:
		return canonicalInt(0, 60000)
	case KeyUpstreamFetchConcurrency:
		return canonicalInt(1, 12)
	case KeyBackupDailyHour:
		return canonicalInt(0, 23)
	}
	return nil
}

func validateSecretServiceURL(key, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > 2048 {
		return fmt.Errorf("%s exceeds the 2048-byte URL limit", key)
	}
	parsed, err := url.Parse(value)
	loopbackHTTP := parsed != nil && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())
	if err != nil || (parsed.Scheme != "https" && !loopbackHTTP) || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("%s must be an absolute HTTPS URL (or loopback HTTP for local testing) without credentials, query, or fragment", key)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("%s must not contain control characters", key)
		}
	}
	return nil
}

func validateUpstreamURLSetting(key, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 8192 {
		return fmt.Errorf("%s exceeds the 8192-byte URL setting limit", key)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("%s must not contain control characters", key)
		}
	}
	templates := []string{value}
	if key == KeyUpstreamVersionFallbackURL {
		templates = strings.FieldsFunc(value, func(char rune) bool { return char == ',' || char == ';' })
		if len(templates) == 0 {
			return fmt.Errorf("%s must contain at least one URL", key)
		}
	}
	policy := httpx.UpstreamPolicyFromEnvironment()
	for _, template := range templates {
		template = strings.TrimSpace(template)
		expanded := strings.ReplaceAll(template, "{repo}", "owner/repo")
		expanded = strings.ReplaceAll(expanded, "{branch}", "main")
		if err := httpx.ValidateUpstreamURL(expanded, policy); err != nil {
			return fmt.Errorf("%s contains an unsafe upstream URL: %w", key, err)
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	address := net.ParseIP(strings.TrimSpace(host))
	return address != nil && address.IsLoopback()
}

// SetIfAbsent writes a setting only if it is not already present. Used for
// env-based seeding so the admin UI remains authoritative after first run.
func (c *Config) SetIfAbsent(key, value string) (bool, error) {
	changed, err := c.SetManyIfAbsent(map[string]string{key: value})
	return changed == 1, err
}

// SetManyIfAbsent validates and seeds a group atomically. Existing settings are
// left untouched, and no value is inserted if any supplied value is invalid.
func (c *Config) SetManyIfAbsent(values map[string]string) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	type encodedSetting struct {
		key, value, stored string
		encrypted          int
	}
	encoded := make([]encodedSetting, 0, len(values))
	for key, value := range values {
		// Validated as given and stored trimmed, as in SetMany; a value that
		// trims to empty seeds nothing, so a later seed can still fill it.
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if err := validateSettingValue(key, value); err != nil {
			return 0, err
		}
		stored, encrypted, err := c.encodeSetting(key, trimmed)
		if err != nil {
			return 0, err
		}
		encoded = append(encoded, encodedSetting{key: key, value: trimmed, stored: stored, encrypted: encrypted})
	}
	tx, err := c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	inserted := make([]encodedSetting, 0, len(encoded))
	for _, setting := range encoded {
		result, err := tx.Exec(`INSERT OR IGNORE INTO settings (key, value, encrypted) VALUES (?, ?, ?)`,
			setting.key, setting.stored, setting.encrypted)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed == 1 {
			inserted = append(inserted, setting)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	for _, setting := range inserted {
		c.cache[setting.key] = setting.value
	}
	c.mu.Unlock()
	return len(inserted), nil
}

// All returns a snapshot of all settings, with secrets masked unless reveal is
// true. Used by the admin settings API.
func (c *Config) All(reveal bool) map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string, len(c.cache))
	for k, v := range c.cache {
		if IsSecret(k) && !reveal && v != "" {
			out[k] = "********"
			continue
		}
		out[k] = v
	}
	return out
}

// ---- AES-GCM helpers ----

func (c *Config) encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (c *Config) decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
