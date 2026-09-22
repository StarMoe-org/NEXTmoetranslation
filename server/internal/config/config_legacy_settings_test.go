package config

import "testing"

func TestNewNormalizesLegacyBooleanAndIntegerSettings(t *testing.T) {
	database := openTestDB(t)
	legacy := map[string]string{
		KeySchedulerOn:           "on",
		KeyLyricsDiscoveryOn:     "1",
		KeyLyricsFetchRevisionOn: "yes",
		KeyBackupS3Enabled:       "0",
		KeyBackupGitEnabled:      "off",
		KeyBackupDailyHour:       "07",
	}
	for key, value := range legacy {
		if _, err := database.Exec(`INSERT INTO settings(key, value, encrypted) VALUES (?, ?, 0)`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	configuration, err := New(database, "")
	if err != nil {
		t.Fatalf("legacy persisted settings rejected: %v", err)
	}
	for key, want := range map[string]string{
		KeySchedulerOn: "true", KeyLyricsDiscoveryOn: "true", KeyLyricsFetchRevisionOn: "true",
		KeyBackupS3Enabled: "false", KeyBackupGitEnabled: "false", KeyBackupDailyHour: "7",
	} {
		if got := configuration.Get(key); got != want {
			t.Fatalf("setting %s = %q want %q", key, got, want)
		}
		var stored string
		if err := database.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != want {
			t.Fatalf("persisted setting %s = %q want %q", key, stored, want)
		}
	}
	if !configuration.GetBool(KeySchedulerOn, false) || configuration.GetBool(KeyBackupGitEnabled, true) {
		t.Fatalf("normalized booleans scheduler=%v git=%v",
			configuration.GetBool(KeySchedulerOn, false), configuration.GetBool(KeyBackupGitEnabled, true))
	}
	if _, err := New(database, ""); err != nil {
		t.Fatalf("reopen after normalization: %v", err)
	}
}

func TestNewRejectsOutOfRangePersistedIntegerSetting(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.Exec(`INSERT INTO settings(key, value, encrypted) VALUES (?, ?, 0)`, KeyBackupDailyHour, "099"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(database, ""); err == nil {
		t.Fatal("out-of-range persisted integer setting was accepted")
	}
}
