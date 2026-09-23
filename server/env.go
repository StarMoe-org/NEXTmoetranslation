package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/lifecycle"
	"moesekai/server/internal/lyricsdiscovery"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseTTL(hours string) (time.Duration, error) {
	const maxTokenTTLHours = 24 * 30
	hours = strings.TrimSpace(hours)
	n, err := strconv.Atoi(hours)
	if err != nil || n <= 0 || n > maxTokenTTLHours {
		return 0, fmt.Errorf("must be a canonical integer from 1 to %d hours", maxTokenTTLHours)
	}
	if strconv.Itoa(n) != hours {
		return 0, fmt.Errorf("must be a canonical integer from 1 to %d hours", maxTokenTTLHours)
	}
	return time.Duration(n) * time.Hour, nil
}

func durationEnvMs(name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw, configured := os.LookupEnv(name)
	if !configured || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("must be a positive integer number of milliseconds")
	}
	if n > int64(^uint64(0)>>1)/int64(time.Millisecond) {
		return 0, fmt.Errorf("duration overflows")
	}
	duration := time.Duration(n) * time.Millisecond
	if duration < minimum || duration > maximum {
		return 0, fmt.Errorf("must be between %d and %d milliseconds", minimum.Milliseconds(), maximum.Milliseconds())
	}
	return duration, nil
}

func boundedIntEnv(name string, fallback, minimum, maximum int) (int, error) {
	raw, configured := os.LookupEnv(name)
	if !configured || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	n, err := strconv.Atoi(value)
	if err != nil || strconv.Itoa(n) != value || n < minimum || n > maximum {
		return 0, fmt.Errorf("%s must be a canonical integer from %d to %d", name, minimum, maximum)
	}
	return n, nil
}

func resolveProductionMode(value string) (bool, error) {
	if runtimeProfile == runtimeProfileNextProduction && value != "true" {
		return false, fmt.Errorf(`standalone production binary requires MOESEKAI_PRODUCTION to remain exactly "true"`)
	}
	return parseProductionMode(value)
}

func parseProductionMode(value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	production, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("must be true or false")
	}
	return production, nil
}

func validateRuntimeTimezone(production bool, value string, configured bool) error {
	if production && (!configured || value != "UTC") {
		return errors.New(`standalone production binary requires TZ to remain exactly "UTC"`)
	}
	return nil
}

func resolveWebDir(production bool, value string, configured bool) (string, error) {
	if production {
		if !configured || value != "/app/web" {
			return "", errors.New(`standalone production binary requires WEB_DIR to remain exactly "/app/web"`)
		}
		return value, nil
	}
	if value == "" {
		return "./web", nil
	}
	return value, nil
}

func resolveDBPath(production bool, value string, configured bool) (string, error) {
	if production {
		if !configured || value != "/data/moesekai.db" {
			return "", errors.New(`standalone production binary requires DB_PATH to remain exactly "/data/moesekai.db"`)
		}
		return value, nil
	}
	if value == "" {
		return "./data/moesekai.db", nil
	}
	return value, nil
}

func resolveDataDir(production bool, value string, configured bool) (string, error) {
	if production {
		if !configured || value != "/data" {
			return "", errors.New(`standalone production binary requires DATA_DIR to remain exactly "/data"`)
		}
		return value, nil
	}
	if value == "" {
		return "./data", nil
	}
	return value, nil
}

func validateProductionAdmin(production bool, a *auth.Auth) error {
	if !production {
		return nil
	}
	admins, err := a.CountAdmins()
	if err != nil {
		return fmt.Errorf("count administrators: %w", err)
	}
	if admins == 0 {
		return fmt.Errorf("an initialized administrator is required; provide ADMIN_PASSWORD or TRANSLATOR_ACCOUNTS for first boot")
	}
	return nil
}

func validateProductionMasterKey(production bool, masterKey string) error {
	masterKey = strings.TrimSpace(masterKey)
	if production && (len([]byte(masterKey)) < 32 || masterKey == "replace-with-at-least-32-random-bytes") {
		return fmt.Errorf("MOESEKAI_MASTER_KEY must contain at least 32 bytes of non-template secret material")
	}
	return nil
}

func validateConsoleOrigin(production bool, origin string) error {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return fmt.Errorf("must be * or one absolute http(s) origin")
	}
	if origin == "*" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return fmt.Errorf("must be * or one absolute http(s) origin without credentials, path, query, fragment, or trailing slash")
	}
	if production && parsed.Scheme == "http" && !isLoopbackOriginHost(parsed.Hostname()) {
		return fmt.Errorf("production console origin must use https unless it is a loopback host")
	}
	return nil
}

func isLoopbackOriginHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	address := net.ParseIP(strings.TrimSpace(host))
	return address != nil && address.IsLoopback()
}

func lyricsDiscoveryOptionsFromEnv() (lyricsdiscovery.Options, error) {
	scanInterval, err := durationEnvMs("LYRICS_DISCOVERY_SCAN_MS", 6*time.Hour, time.Minute, 7*24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	leaseDuration, err := durationEnvMs("LYRICS_DISCOVERY_LEASE_MS", 2*time.Minute, 10*time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	idleWait, err := durationEnvMs("LYRICS_DISCOVERY_IDLE_MS", 2*time.Second, 100*time.Millisecond, time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMin, err := durationEnvMs("LYRICS_DISCOVERY_RETRY_MIN_MS", 30*time.Second, time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMax, err := durationEnvMs("LYRICS_DISCOVERY_RETRY_MAX_MS", 30*time.Minute, time.Second, 30*24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	jobTimeout, err := durationEnvMs("LYRICS_DISCOVERY_JOB_TIMEOUT_MS", 30*time.Second, time.Second, 10*time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	concurrency, err := boundedIntEnv("LYRICS_DISCOVERY_CONCURRENCY", 4, 1, 16)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	if retryMax < retryMin {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_DISCOVERY_RETRY_MAX_MS must be greater than or equal to LYRICS_DISCOVERY_RETRY_MIN_MS")
	}
	if jobTimeout >= leaseDuration {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_DISCOVERY_JOB_TIMEOUT_MS must be shorter than LYRICS_DISCOVERY_LEASE_MS")
	}
	return lyricsdiscovery.Options{
		ScanInterval: scanInterval, LeaseDuration: leaseDuration, IdleWait: idleWait,
		RetryMin: retryMin, RetryMax: retryMax, JobTimeout: jobTimeout, Concurrency: concurrency,
	}, nil
}

func lyricsFetchRevisionOptionsFromEnv() (lyricsdiscovery.Options, error) {
	leaseDuration, err := durationEnvMs("LYRICS_FETCH_REVISION_LEASE_MS", 2*time.Minute, 10*time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	idleWait, err := durationEnvMs("LYRICS_FETCH_REVISION_IDLE_MS", 2*time.Second, 100*time.Millisecond, time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMin, err := durationEnvMs("LYRICS_FETCH_REVISION_RETRY_MIN_MS", 30*time.Second, time.Second, 24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	retryMax, err := durationEnvMs("LYRICS_FETCH_REVISION_RETRY_MAX_MS", 30*time.Minute, time.Second, 30*24*time.Hour)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	jobTimeout, err := durationEnvMs("LYRICS_FETCH_REVISION_JOB_TIMEOUT_MS", 30*time.Second, time.Second, 10*time.Minute)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	concurrency, err := boundedIntEnv("LYRICS_FETCH_REVISION_CONCURRENCY", 4, 1, 16)
	if err != nil {
		return lyricsdiscovery.Options{}, err
	}
	if retryMax < retryMin {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_FETCH_REVISION_RETRY_MAX_MS must be greater than or equal to LYRICS_FETCH_REVISION_RETRY_MIN_MS")
	}
	if jobTimeout >= leaseDuration {
		return lyricsdiscovery.Options{}, errors.New("LYRICS_FETCH_REVISION_JOB_TIMEOUT_MS must be shorter than LYRICS_FETCH_REVISION_LEASE_MS")
	}
	return lyricsdiscovery.Options{ScanInterval: time.Hour, LeaseDuration: leaseDuration, IdleWait: idleWait,
		RetryMin: retryMin, RetryMax: retryMax, JobTimeout: jobTimeout, Concurrency: concurrency}, nil
}

func shutdownConfigFromEnv() (lifecycle.ShutdownConfig, error) {
	parse := func(name string, fallback int) (time.Duration, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			value = strconv.Itoa(fallback)
		}
		milliseconds, err := strconv.Atoi(value)
		if err != nil || milliseconds <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer number of milliseconds", name)
		}
		return time.Duration(milliseconds) * time.Millisecond, nil
	}
	budget, err := parse("SHUTDOWN_BUDGET_MS", 25000)
	if err != nil {
		return lifecycle.ShutdownConfig{}, err
	}
	drain, err := parse("SHUTDOWN_DRAIN_MS", 2000)
	if err != nil {
		return lifecycle.ShutdownConfig{}, err
	}
	if drain >= budget {
		return lifecycle.ShutdownConfig{}, fmt.Errorf("SHUTDOWN_DRAIN_MS must be shorter than SHUTDOWN_BUDGET_MS")
	}
	return lifecycle.ShutdownConfig{Budget: budget, Drain: drain}, nil
}
