package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/httpx"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsextractionplan"
	"moesekai/server/internal/lyricsprovidercoord"
	"moesekai/server/internal/lyricsproviderpolicy"
	"moesekai/server/internal/lyricsrecovery"
	"moesekai/server/internal/model"
)

const (
	protectedRecoveryTestCatalogPath    = "/private/tmp/moesekai-lyrics-catalog-v18-20260731-704.db"
	protectedRecoveryTestCatalogPathEnv = "MOESEKAI_RECOVERY_TEST_CATALOG"
)

var recoveryCommandTestRoot string
var recoveryCommandTestCatalogPath string
var recoveryCommandTestCatalogBinding lyricsextractionplan.RecoveryCatalogBinding

func TestMain(m *testing.M) {
	os.Exit(runRecoveryCommandTestMain(m))
}

func TestOutputStateRejectsPreexistingForensicResponseStore(t *testing.T) {
	root, err := os.MkdirTemp(recoveryCommandTestRoot, "forensic-output-state-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	parsed := options{
		ledgerPath:           filepath.Join(root, "ledger"),
		acquisitionSetPath:   filepath.Join(root, "acquisition-set.json"),
		providerOutcomesPath: filepath.Join(root, "provider-outcomes"),
		songResultsPath:      filepath.Join(root, "song-results"),
		evidencePackPath:     filepath.Join(root, "evidence-pack"),
		rootManifestPath:     filepath.Join(root, "root.json"),
	}
	forensicPath, err := lyricsrecovery.ForensicResponseStorePath(parsed.ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(forensicPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkOutputState(parsed, false, false); err == nil {
		t.Fatal("output-state preflight accepted a preexisting private forensic response store")
	}
}

func runRecoveryCommandTestMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "lyrics-recovery-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create stable lyrics-recovery test root: %v\n", err)
		return 1
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || canonicalRoot != root {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "canonicalize stable lyrics-recovery test root: path=%q resolved=%q err=%v\n", root, canonicalRoot, err)
		return 1
	}
	if err := os.Chmod(canonicalRoot, 0o700); err != nil {
		_ = os.RemoveAll(canonicalRoot)
		fmt.Fprintf(os.Stderr, "secure stable lyrics-recovery test root: %v\n", err)
		return 1
	}
	rootInfo, statErr := os.Lstat(canonicalRoot)
	privateErr := validateDirectoryInfo(rootInfo, directoryPolicy{
		label: "lyrics-recovery test root", private: true, effectiveUID: true,
	})
	if statErr != nil || privateErr != nil {
		_ = os.RemoveAll(canonicalRoot)
		fmt.Fprintf(os.Stderr, "validate stable lyrics-recovery test root: stat=%v private=%v\n", statErr, privateErr)
		return 1
	}
	recoveryCommandTestCatalogPath, recoveryCommandTestCatalogBinding, err = prepareRecoveryCommandTestCatalog(canonicalRoot)
	if err != nil {
		_ = os.RemoveAll(canonicalRoot)
		fmt.Fprintf(os.Stderr, "prepare protected lyrics-recovery test catalog: %v\n", err)
		return 1
	}

	recoveryCommandTestRoot = canonicalRoot
	setPrivateInputAncestryRootForTest(canonicalRoot)
	setRecoveryLiveStateProvisionParentForTest(canonicalRoot)
	code := m.Run()
	setPrivateInputAncestryRootForTest(string(os.PathSeparator))
	setRecoveryLiveStateProvisionParentForTest("/private/tmp")
	if err := os.RemoveAll(canonicalRoot); err != nil {
		fmt.Fprintf(os.Stderr, "remove stable lyrics-recovery test root: %v\n", err)
		code = 1
	}
	return code
}

func prepareRecoveryCommandTestCatalog(root string) (string, lyricsextractionplan.RecoveryCatalogBinding, error) {
	sourcePath := os.Getenv(protectedRecoveryTestCatalogPathEnv)
	explicitSource := sourcePath != ""
	if sourcePath == "" {
		sourcePath = protectedRecoveryTestCatalogPath
	}
	if body, err := os.ReadFile(sourcePath); err == nil {
		path := filepath.Join(root, filepath.Base(protectedRecoveryTestCatalogPath))
		if err := os.WriteFile(path, body, 0o444); err != nil {
			return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
		}
		if err := os.Chmod(path, 0o444); err != nil {
			return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
		}
		return path, lyricsextractionplan.RecoveryCatalogBinding{
			Path: path, SizeBytes: 1_150_976,
			SourceSHA256:          "58626dcd03a8bc06ffa1e1c8fba3cfa6dea0560fb471abd802829b4a7d6dd7f4",
			SchemaVersion:         lyricsextractionplan.CatalogSchemaVersion,
			RuntimeSchemaVersion:  lyricsextractionplan.MaximumCatalogRuntimeSchema,
			RecordCount:           704,
			IdentityPolicyVersion: lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity,
			IdentitySHA256:        "a17efa8a7c5e6c533d2502f01fccd7c5ddf9cd68bb28a489b7f7f6552e127fe2",
			MusicIDsSHA256:        "510da78c96ff21ac6f200dbfc3054be326c081d3fd0876d12ae3557d49188fa1",
		}, nil
	} else if explicitSource {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, fmt.Errorf("read %s: %w", protectedRecoveryTestCatalogPathEnv, err)
	}
	return writeSyntheticRecoveryCommandTestCatalog(root)
}

func writeSyntheticRecoveryCommandTestCatalog(root string) (string, lyricsextractionplan.RecoveryCatalogBinding, error) {
	path := filepath.Join(root, "synthetic-catalog-v18-704.db")
	database, err := sql.Open("sqlite", "file:"+path+"?mode=rwc")
	if err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	database.SetMaxOpenConns(1)
	fail := func(cause error) (string, lyricsextractionplan.RecoveryCatalogBinding, error) {
		_ = database.Close()
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, cause
	}
	if _, err := database.Exec(`PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL;
		CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);
		CREATE TABLE catalog_music(
			music_id INTEGER PRIMARY KEY,
			title_ja TEXT NOT NULL,
			producer_metadata TEXT NOT NULL,
			lyricist TEXT NOT NULL,
			composer TEXT NOT NULL,
			arranger TEXT NOT NULL,
			vocal_signals_json TEXT NOT NULL,
			lyrics_catalog_fingerprint TEXT NOT NULL,
			lyrics_catalog_policy_version TEXT NOT NULL
		)`); err != nil {
		return fail(err)
	}
	transaction, err := database.Begin()
	if err != nil {
		return fail(err)
	}
	for version := 1; version <= lyricsextractionplan.CatalogSchemaVersion; version++ {
		if _, err := transaction.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, version); err != nil {
			_ = transaction.Rollback()
			return fail(err)
		}
	}
	policy := lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity
	musicIDs := make([]int, 704)
	identityDigest := sha256.New()
	_, _ = identityDigest.Write([]byte("moesekai-lyrics-recovery-catalog-fingerprints-v1\x00"))
	var encoded [8]byte
	for index := range musicIDs {
		musicID := index + 1
		musicIDs[index] = musicID
		title := fmt.Sprintf("試験曲%03d", musicID)
		producer := "試験制作者"
		lyricist := "試験作詞"
		composer := "試験作曲"
		arranger := "試験編曲"
		vocalsJSON := "[]"
		switch musicID {
		case 2:
			title = "ロキ"
			producer = "みきとP | みきとP | みきとP"
			lyricist, composer, arranger = "みきとP", "みきとP", "みきとP"
			vocalsJSON = `[{"vocalType":"sekai"}]`
		case 235:
			title = "Journey"
			producer = "DECO*27 | DECO*27 | Rockwell"
			lyricist, composer, arranger = "DECO*27", "DECO*27", "Rockwell"
			vocalsJSON = `[{"vocalType":"sekai"}]`
		}
		fingerprintBytes := sha256.Sum256([]byte(fmt.Sprintf("synthetic-recovery-catalog-v1:%d", musicID)))
		fingerprint := hex.EncodeToString(fingerprintBytes[:])
		if _, err := transaction.Exec(`INSERT INTO catalog_music
			(music_id,title_ja,producer_metadata,lyricist,composer,arranger,vocal_signals_json,
			 lyrics_catalog_fingerprint,lyrics_catalog_policy_version) VALUES (?,?,?,?,?,?,?,?,?)`,
			musicID, title, producer, lyricist, composer, arranger, vocalsJSON, fingerprint, policy); err != nil {
			_ = transaction.Rollback()
			return fail(err)
		}
		binary.BigEndian.PutUint64(encoded[:], uint64(musicID))
		_, _ = identityDigest.Write(encoded[:])
		_, _ = identityDigest.Write(fingerprintBytes[:])
	}
	if err := transaction.Commit(); err != nil {
		return fail(err)
	}
	if _, err := database.Exec(`PRAGMA optimize`); err != nil {
		return fail(err)
	}
	if err := database.Close(); err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	if err := os.Chmod(path, 0o444); err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			return "", lyricsextractionplan.RecoveryCatalogBinding{}, fmt.Errorf("synthetic recovery catalog retained %s sidecar", suffix)
		}
	}
	sourceDigest := sha256.Sum256(body)
	musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil {
		return "", lyricsextractionplan.RecoveryCatalogBinding{}, err
	}
	return path, lyricsextractionplan.RecoveryCatalogBinding{
		Path: path, SizeBytes: int64(len(body)), SourceSHA256: hex.EncodeToString(sourceDigest[:]),
		SchemaVersion:        lyricsextractionplan.CatalogSchemaVersion,
		RuntimeSchemaVersion: lyricsextractionplan.MaximumCatalogRuntimeSchema,
		RecordCount:          704, IdentityPolicyVersion: policy,
		IdentitySHA256: hex.EncodeToString(identityDigest.Sum(nil)), MusicIDsSHA256: musicIDsSHA256,
	}, nil
}

func TestRecoveryCommandOfflineDefaultsAndAuthorizationGates(t *testing.T) {
	arguments := commandTestArguments(t)
	counter := &rejectingDefaultTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = counter
	t.Cleanup(func() { http.DefaultTransport = previous })
	var ownershipCalls atomic.Int64
	previousAcquire := acquireRecoveryLiveOwnership
	acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) {
		ownershipCalls.Add(1)
		return nil, errors.New("offline path touched live ownership")
	}
	t.Cleanup(func() { acquireRecoveryLiveOwnership = previousAcquire })

	tests := []struct {
		name       string
		extra      []string
		wantOutput string
	}{
		{name: "default check is offline", wantOutput: "PASS mode=check"},
		{name: "live canary defaults HOLD", extra: []string{
			"-mode", "live-canary", "-sekaipedia-list-replay-ledger", filepath.Join(recoveryCommandTestRoot, "unauthorized-list-ledger"),
		}, wantOutput: "HOLD mode=live-canary"},
		{name: "acquisition rejects live token alone", extra: []string{
			"-mode", "acquisition", "-live-canary-authorization", liveCanaryAuthorization,
			"-sekaipedia-list-replay-ledger", filepath.Join(recoveryCommandTestRoot, "unauthorized-acquisition-list-ledger"),
		}, wantOutput: "HOLD mode=acquisition"},
		{name: "acquisition rejects acquisition token alone", extra: []string{
			"-mode", "acquisition", "-acquisition-authorization", acquisitionAuthorization,
			"-sekaipedia-list-replay-ledger", filepath.Join(recoveryCommandTestRoot, "unauthorized-acquisition-list-ledger"),
		}, wantOutput: "HOLD mode=acquisition"},
		{name: "subset acquisition rejects live token alone", extra: []string{
			"-mode", "acquisition-subset", "-live-canary-authorization", liveCanaryAuthorization,
			"-sekaipedia-list-replay-ledger", filepath.Join(recoveryCommandTestRoot, "unauthorized-subset-list-ledger"),
			"-acquisition-music-ids", "2",
		}, wantOutput: "HOLD mode=acquisition-subset"},
		{name: "subset acquisition rejects acquisition token alone", extra: []string{
			"-mode", "acquisition-subset", "-acquisition-authorization", acquisitionAuthorization,
			"-sekaipedia-list-replay-ledger", filepath.Join(recoveryCommandTestRoot, "unauthorized-subset-list-ledger"),
			"-acquisition-music-ids", "2",
		}, wantOutput: "HOLD mode=acquisition-subset"},
		{name: "migration defaults HOLD", extra: []string{"-mode", "migration"}, wantOutput: "HOLD mode=migration"},
		{name: "migration remains unexposed with token", extra: []string{
			"-mode", "migration", "-migration-authorization", migrationAuthorization,
		}, wantOutput: "implementation=not-exposed realMigration=HOLD"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := run(context.Background(), append(append([]string{}, arguments...), test.extra...), &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), test.wantOutput) {
				t.Fatalf("command output %q does not contain %q", output.String(), test.wantOutput)
			}
			if counter.requests.Load() != 0 {
				t.Fatalf("offline or unauthorized command touched the default network transport %d times", counter.requests.Load())
			}
		})
	}
	if ownershipCalls.Load() != 0 {
		t.Fatalf("offline or unauthorized modes touched live ownership %d times", ownershipCalls.Load())
	}
}

func TestRecoveryCommandHistoricalPlanIsInspectionOnlyAndCannotAuthorizeLiveCanary(t *testing.T) {
	arguments := rewriteCommandPlanForHistoricalInspection(t, commandTestArguments(t))
	var ownershipCalls atomic.Int32
	previousAcquire := acquireRecoveryLiveOwnership
	acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) {
		ownershipCalls.Add(1)
		return nil, errors.New("historical inspection plan reached live ownership")
	}
	t.Cleanup(func() { acquireRecoveryLiveOwnership = previousAcquire })
	counter := &rejectingDefaultTransport{}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = counter
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	var output bytes.Buffer
	if err := run(context.Background(), arguments, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "PASS mode=check") ||
		!strings.Contains(output.String(), "legacyListInspection=true") ||
		!strings.Contains(output.String(), "liveCanaryAuthority=HOLD") {
		t.Fatalf("historical inspection output=%q", output.String())
	}

	live := append(append([]string(nil), arguments...),
		"-mode", "live-canary",
		"-live-canary-authorization", liveCanaryAuthorization,
		"-sekaipedia-list-replay-ledger", filepath.Join(recoveryCommandTestRoot, "historical-inspection-ledger"),
	)
	if err := run(context.Background(), live, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "exact replay acquisition ID is invalid") {
		t.Fatalf("historical live-canary authorization error=%v", err)
	}
	if ownershipCalls.Load() != 0 || counter.requests.Load() != 0 {
		t.Fatalf("historical inspection touched live authority: ownership=%d requests=%d",
			ownershipCalls.Load(), counter.requests.Load())
	}
	for _, flagName := range []string{"-ledger", "-acquisition-set", "-provider-outcomes"} {
		if _, err := os.Lstat(commandFlagValue(t, arguments, flagName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("historical inspection touched %s: %v", flagName, err)
		}
	}
}

func TestRecoveryCommandRejectsExtraBuildInputBeforeCatalogActivity(t *testing.T) {
	arguments := commandTestArguments(t)
	sourceRoot := commandFlagValue(t, arguments, "-source-root")
	writeCommandSourceFile(t, sourceRoot, "server/cmd/lyrics-recovery/late-extra.s", []byte("TEXT ·late(SB),$0-0\nRET\n"))
	missingCatalog := filepath.Join(sourceRoot, "catalog-must-not-open.db")
	parsed, err := parseOptions(append(arguments, "-catalog", missingCatalog))
	if err != nil {
		t.Fatal(err)
	}
	_, _, catalog, err := checkInputs(context.Background(), parsed)
	if catalog != nil {
		_ = catalog.Close()
		t.Fatal("catalog opened before closed-world source rejection")
	}
	if err == nil || !strings.Contains(err.Error(), "omits current eligible file") {
		t.Fatalf("extra build input error=%v", err)
	}
	if _, statErr := os.Lstat(missingCatalog); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("source rejection touched the catalog path: %v", statErr)
	}
}

func TestRecoveryCommandReplayCannotFallThroughToDefaultNetwork(t *testing.T) {
	arguments := append(commandTestArguments(t), "-mode", "replay")
	counter := &rejectingDefaultTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = counter
	defer func() { http.DefaultTransport = previous }()
	if err := run(context.Background(), arguments, &bytes.Buffer{}); err == nil {
		t.Fatal("replay without an explicit existing ledger and acquisition set was accepted")
	}
	if counter.requests.Load() != 0 {
		t.Fatal("offline replay attempted default network access")
	}
}

func TestAuthorizedDirectLivePathCannotBypassOwnershipOrMutateOutputsFirst(t *testing.T) {
	arguments, _ := bindLiveCanaryCommandListReplay(t, commandTestArguments(t))
	arguments = append(arguments,
		"-mode", "acquisition",
		"-live-canary-authorization", liveCanaryAuthorization,
		"-acquisition-authorization", acquisitionAuthorization,
	)
	previousAcquire := acquireRecoveryLiveOwnership
	acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) {
		return nil, fmt.Errorf("%w: test root is unprovisioned", lyricsprovidercoord.ErrHold)
	}
	t.Cleanup(func() { acquireRecoveryLiveOwnership = previousAcquire })

	var output bytes.Buffer
	if err := run(context.Background(), arguments, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "HOLD mode=acquisition liveOwnership=HOLD network=HOLD") {
		t.Fatalf("ownership HOLD output=%q", output.String())
	}
	for _, flagName := range []string{"-ledger", "-acquisition-set", "-provider-outcomes", "-song-results", "-evidence-pack", "-root-manifest"} {
		path := commandFlagValue(t, arguments, flagName)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("live output %s was touched before ownership: %v", flagName, err)
		}
	}
	if _, err := parseOptions(append(arguments, "-live-state-root", filepath.Join(recoveryCommandTestRoot, "bypass"))); err == nil {
		t.Fatal("direct invocation accepted a live state root override")
	}
}

func TestSekaipediaListReplayLedgerIsRequiredAndCompatibilityIDIsOptionalCanonical(t *testing.T) {
	arguments := commandTestArguments(t)
	sourceLedger := filepath.Join(recoveryCommandTestRoot, "existing-ledger")
	const acquisitionID = lyricsextractionplan.HistoricalSekaipediaListAcquisitionID
	for name, extra := range map[string][]string{
		"missing ledger": {
			"-mode", "live-canary",
		},
		"acquisition only": {
			"-mode", "live-canary", "-sekaipedia-list-replay-acquisition-id", acquisitionID,
		},
		"outside live canary": {
			"-sekaipedia-list-replay-ledger", sourceLedger,
			"-sekaipedia-list-replay-acquisition-id", acquisitionID,
		},
		"invalid acquisition ID": {
			"-mode", "live-canary", "-sekaipedia-list-replay-ledger", sourceLedger,
			"-sekaipedia-list-replay-acquisition-id", strings.Repeat("A", 64),
		},
		"source aliases output": {
			"-mode", "live-canary", "-sekaipedia-list-replay-ledger", commandFlagValue(t, arguments, "-ledger"),
			"-sekaipedia-list-replay-acquisition-id", acquisitionID,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOptions(append(append([]string{}, arguments...), extra...)); err == nil {
				t.Fatal("invalid exact-acquisition replay flags were accepted")
			}
		})
	}
	ledgerOnly, err := parseOptions(append(append([]string{}, arguments...),
		"-mode", "live-canary",
		"-sekaipedia-list-replay-ledger", sourceLedger,
	))
	if err != nil || ledgerOnly.sekaipediaListReplayLedgerPath != sourceLedger ||
		ledgerOnly.sekaipediaListReplayAcquisitionID != "" {
		t.Fatalf("valid plan-authoritative replay options=%+v err=%v", ledgerOnly, err)
	}
	parsed, err := parseOptions(append(append([]string{}, arguments...),
		"-mode", "live-canary",
		"-sekaipedia-list-replay-ledger", sourceLedger,
		"-sekaipedia-list-replay-acquisition-id", acquisitionID,
	))
	if err != nil || parsed.sekaipediaListReplayLedgerPath != sourceLedger ||
		parsed.sekaipediaListReplayAcquisitionID != acquisitionID {
		t.Fatalf("valid compatible exact-acquisition replay options=%+v err=%v", parsed, err)
	}
}

func TestRecoveryLiveTransportsAreExplicitFailClosedAndProviderLocal(t *testing.T) {
	t.Setenv("MOESEKAI_PRODUCTION", "true")
	t.Setenv(httpx.UpstreamAllowInsecureLocalEnv, "false")
	runtime := lyricsrecovery.RuntimeConfig{
		Order: []model.LyricsSourceProvider{
			model.LyricsSourceProviderSekaipedia,
			model.LyricsSourceProviderMoegirl,
			model.LyricsSourceProviderVocaloidFandom,
		},
		RequestTimeout: 30 * time.Second,
	}
	transports, err := newRecoveryLiveTransports(runtime, passthroughLiveOwnership{})
	if err != nil {
		t.Fatal(err)
	}
	if len(transports) != len(runtime.Order) {
		t.Fatalf("explicit live transport count=%d, want %d", len(transports), len(runtime.Order))
	}
	seen := make(map[http.RoundTripper]struct{}, len(transports))
	for _, provider := range runtime.Order {
		transport := transports[provider]
		if transport == nil || transport == http.DefaultTransport {
			t.Fatalf("provider %s did not receive an explicit non-default transport", provider)
		}
		if _, duplicate := seen[transport]; duplicate {
			t.Fatalf("provider %s shared its underlying live transport", provider)
		}
		seen[transport] = struct{}{}
		request, requestErr := http.NewRequest(http.MethodGet, "https://127.0.0.1/w/api.php?action=query", nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response, roundTripErr := transport.RoundTrip(request)
		if response != nil {
			_ = response.Body.Close()
		}
		if roundTripErr == nil || !strings.Contains(roundTripErr.Error(), "unsafe upstream URL") {
			t.Fatalf("provider %s explicit transport did not reject a private literal before dial: %v", provider, roundTripErr)
		}
	}
}

type passthroughLiveOwnership struct{}

func (passthroughLiveOwnership) Wrap(_ lyricsproviderpolicy.Provider, transport http.RoundTripper) (http.RoundTripper, error) {
	return transport, nil
}

func (passthroughLiveOwnership) ResolveProvider(lyricsproviderpolicy.Provider) error { return nil }
func (passthroughLiveOwnership) Close() error                                        { return nil }

type rejectingDefaultTransport struct {
	requests atomic.Int64
}

func (transport *rejectingDefaultTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.requests.Add(1)
	return nil, errors.New("default network transport must remain unreachable")
}

func commandTestArguments(t *testing.T) []string {
	t.Helper()
	root, err := os.MkdirTemp(recoveryCommandTestRoot, "command-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	outputs := filepath.Join(root, "outputs")
	if err := os.Mkdir(outputs, 0o700); err != nil {
		t.Fatal(err)
	}

	catalog := recoveryCommandTestCatalogBinding
	catalog.Path = recoveryCommandTestCatalogPath
	floors := lyricsextractionplan.CompiledSafetyFloors()
	moegirlContent := []byte("* [[Other#Other|Other]]\n")
	moegirlSHA1 := sha1.Sum(moegirlContent)
	sekaipediaAuthority := commandTestAuthority(t, lyricsextractionplan.ProviderSekaipedia, 268, 335193,
		"List of songs", "b216a827f88c59f5e954a120027832fe9cd74413",
		"aaddff2922548aab7e522124ff2bad86427501930d549c9d94c9b4e473c35f92",
		"c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd", "2026-07-27T16:29:13Z")
	moegirlAuthority := commandTestAuthority(t, lyricsextractionplan.ProviderMoegirl, 488279, 8073049,
		"世界计划 彩色舞台 feat. 初音未来/歌曲", hex.EncodeToString(moegirlSHA1[:]), "", "", "")
	paths := [6]string{
		filepath.Join(outputs, "ledger"), filepath.Join(outputs, "acquisition-set.json"),
		filepath.Join(outputs, "provider-outcomes"), filepath.Join(outputs, "song-results"),
		filepath.Join(outputs, "evidence-pack"), filepath.Join(outputs, "root.json"),
	}
	const sourceFixturePath = "server/cmd/lyrics-recovery/testdata/fixture.json"
	writeCommandSourceFile(t, root, "server/go.mod", []byte("module example.invalid/recovery-command\n\ngo 1.25\n"))
	writeCommandSourceFile(t, root, "server/go.sum", []byte("example.invalid/module v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"))
	writeCommandSourceFile(t, root, "server/cmd/lyrics-recovery/main.go", []byte("package main\n"))
	fixtureBody := []byte(`{"reviewed":"offline fixture"}`)
	writeCommandSourceFile(t, root, sourceFixturePath, fixtureBody)
	fixtureRawSHA256 := sha256.Sum256(fixtureBody)
	fixtureContentSHA1 := sha1.Sum(fixtureBody)
	fixtureContentSHA256 := sha256.Sum256(fixtureBody)
	fixtureManifest, err := json.Marshal(lyricsextractionplan.RecoveryFixtureManifestV2{
		SchemaVersion:     lyricsextractionplan.RecoveryFixtureManifestSchemaVersionV2,
		SelectionPolicy:   lyricsextractionplan.RecoverySourceSelectionPolicyV2,
		SnapshotAlgorithm: lyricsextractionplan.RecoverySourceSnapshotAlgorithmV2,
		Fixtures: []lyricsextractionplan.RecoveryFixtureIdentityV2{{
			Path: sourceFixturePath, Format: lyricsextractionplan.RecoveryFixtureFormatRawFileV1,
			RawSizeBytes: int64(len(fixtureBody)), RawSHA256: hex.EncodeToString(fixtureRawSHA256[:]),
			ContentSHA1: hex.EncodeToString(fixtureContentSHA1[:]), ContentSHA256: hex.EncodeToString(fixtureContentSHA256[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeCommandSourceFile(t, root, "server/internal/lyricsextractionplan/recovery_source_fixtures_v2.json", fixtureManifest)
	sourceSnapshot, err := lyricsextractionplan.PrepareRecoverySourceSnapshot(root, "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	plan := lyricsextractionplan.RecoveryPlan{
		SchemaVersion:     lyricsextractionplan.RecoverySchemaVersionV2,
		CanonicalEncoding: lyricsextractionplan.RecoveryCanonicalEncodingV2,
		DigestAlgorithm:   lyricsextractionplan.RecoveryDigestAlgorithmV2,
		PlanID:            "command-test-recovery-v2", CreatedAt: "2026-08-02T00:00:00Z", Catalog: catalog,
		SourceSnapshot: sourceSnapshot,
		Scope: lyricsextractionplan.RecoveryScopeBinding{
			Kind: lyricsextractionplan.RecoveryScopePartial, ScopeID: "catalog-704", MusicIDs: []int{2, 235},
			SupersedesRootID: "command-test-parent", SupersedesRootSHA256: strings.Repeat("e", 64),
		},
		Providers: lyricsextractionplan.RecoveryProviderConfiguration{
			Order: []lyricsextractionplan.Provider{
				lyricsextractionplan.ProviderSekaipedia, lyricsextractionplan.ProviderMoegirl,
				lyricsextractionplan.ProviderVocaloidFandom,
			},
			Configurations: []lyricsextractionplan.RecoveryProviderPlan{
				{Provider: lyricsextractionplan.ProviderSekaipedia, Mode: lyricsextractionplan.ProviderModeActive,
					CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
					Authorities: []lyricsextractionplan.FixedAuthority{sekaipediaAuthority},
					ContributorAliases: []lyricsextractionplan.RecoveryContributorAlias{{
						MusicID: 2, CatalogContributor: "みきとP", ProviderContributor: "MikitoP",
					}}},
				{Provider: lyricsextractionplan.ProviderMoegirl, Mode: lyricsextractionplan.ProviderModeActive,
					CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
					Authorities:        []lyricsextractionplan.FixedAuthority{moegirlAuthority},
					ContributorAliases: []lyricsextractionplan.RecoveryContributorAlias{}},
				{Provider: lyricsextractionplan.ProviderVocaloidFandom, Mode: lyricsextractionplan.ProviderModeActive,
					CrawlDelayMillis: floors.ProviderCrawlDelayMillis, CacheTTLMillis: floors.ProviderCacheTTLMillis,
					Authorities:        []lyricsextractionplan.FixedAuthority{},
					ContributorAliases: []lyricsextractionplan.RecoveryContributorAlias{}},
			},
		},
		Versions: lyricsextractionplan.CompiledRecoveryVersions(),
		Execution: lyricsextractionplan.RecoveryExecutionSettings{
			MaxAttempts: 1, RequestTimeoutMillis: 30_000, RetryDelayMillis: floors.RetryDelayMillis,
			ProviderResponseBytes:    lyricsextractionplan.CompiledHardCeilings().ProviderResponseBytes,
			MaxActualNetworkInFlight: 1, MediaWikiMaxlag: 5, LiveCanaryMusicIDs: []int{2},
		},
		SekaipediaCanary: &lyricsextractionplan.RecoverySekaipediaCanaryPlan{
			List: lyricsextractionplan.RecoverySekaipediaCanaryRevision{
				AcquisitionID: lyricsextractionplan.HistoricalSekaipediaListAcquisitionID,
				PageID:        sekaipediaAuthority.PageID, RevisionID: sekaipediaAuthority.RevisionID,
				RevisionTimestamp: sekaipediaAuthority.RevisionTimestamp, SHA1: sekaipediaAuthority.SHA1,
				ContentSHA256: sekaipediaAuthority.ContentSHA256, RawResponseSHA256: sekaipediaAuthority.RawSHA256,
			},
			Songs: []lyricsextractionplan.RecoverySekaipediaCanarySong{{
				MusicID: 2, CatalogTitle: "ロキ", ProviderTitle: "Roki", PageID: 398, RevisionID: 330574,
				RevisionTimestamp: "2026-07-15T07:59:12Z", SHA1: "29198603574701b81b34198e63343930abd3d9a2",
				ContentSHA256:     "3f57e7a5cfabf6d9997a2392d8f52fe40b13b95af1312c3f8857e13f405c3ebd",
				RawResponseSHA256: "cc44c089e8704019f390c15084a4882c366c0c0ba30c082496f8f6387e662360",
			}},
		},
		Outputs:    lyricsextractionplan.RequiredRecoveryOutputs(paths),
		Deployment: lyricsextractionplan.RequiredDeploymentPolicy(),
	}
	body, err := lyricsextractionplan.MarshalRecoveryCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	planSHA, err := lyricsextractionplan.RecoveryCanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"-plan", planPath,
		"-expected-plan-sha256", planSHA,
		"-source-root", root,
		"-catalog", catalog.Path,
		"-expected-catalog-sha256", catalog.SourceSHA256,
		"-expected-catalog-count", "704",
		"-expected-catalog-music-ids-sha256", catalog.MusicIDsSHA256,
		"-ledger", paths[0], "-acquisition-set", paths[1],
		"-provider-outcomes", paths[2], "-song-results", paths[3],
		"-evidence-pack", paths[4], "-root-manifest", paths[5],
	}
}

func rewriteCommandPlanForHistoricalInspection(t *testing.T, arguments []string) []string {
	t.Helper()
	path := commandFlagValue(t, arguments, "-plan")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lyricsextractionplan.DecodeRecoveryCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	plan.SekaipediaCanary.List.AcquisitionID = ""
	body, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lyricsextractionplan.DecodeRecoveryCanonicalForInspection(body); err != nil {
		t.Fatal(err)
	}
	if _, err := lyricsextractionplan.DecodeRecoveryCanonical(body); err == nil {
		t.Fatal("historical command fixture remained operationally decodable")
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	result := append([]string(nil), arguments...)
	for index := 0; index+1 < len(result); index++ {
		if result[index] == "-expected-plan-sha256" {
			result[index+1] = hex.EncodeToString(digest[:])
			return result
		}
	}
	t.Fatal("command arguments omit expected plan SHA-256")
	return nil
}

func commandFlagValue(t *testing.T, arguments []string, name string) string {
	t.Helper()
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	t.Fatalf("command test flag %q is absent", name)
	return ""
}

func writeCommandSourceFile(t *testing.T, root, relativePath string, body []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

func commandTestAuthority(t *testing.T, provider lyricsextractionplan.Provider, pageID, revisionID int,
	title, sha1Value, contentSHA256, rawSHA256, revisionTimestamp string) lyricsextractionplan.FixedAuthority {
	t.Helper()
	authority := lyricsextractionplan.FixedAuthority{
		Disposition: lyricsextractionplan.AuthorityActive, Role: lyricsextractionplan.AuthorityRoleSongIndex,
		PageID: pageID, RevisionID: revisionID, RevisionTimestamp: revisionTimestamp,
		SHA1: sha1Value, ContentSHA256: contentSHA256, RawSHA256: rawSHA256, Title: title,
		CanonicalURL:   lyricsextractionplan.FixedAuthorityCanonicalURL(provider, title, revisionID),
		CaptureProfile: lyricsextractionplan.CaptureProfileMediaWikiRevisionContentV1,
	}
	if provider == lyricsextractionplan.ProviderSekaipedia {
		authority.CaptureProfile = lyricsextractionplan.CaptureProfileMediaWikiAPIRevisionResponseV1
	}
	var err error
	authority.EvidenceID, err = lyricsextractionplan.FixedAuthorityEvidenceID(provider, authority.Role, pageID, revisionID, title)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}
