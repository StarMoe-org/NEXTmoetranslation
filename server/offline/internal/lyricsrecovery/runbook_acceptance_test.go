package lyricsrecovery

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsextractionplan"
	"moesekai/server/offline/internal/lyricsrootmanifest"
)

const (
	acceptanceRootEnv         = "MOESEKAI_RECOVERY_V2_ACCEPTANCE_ROOT"
	acceptancePlanEnv         = "MOESEKAI_RECOVERY_V2_ACCEPTANCE_PLAN"
	acceptancePlanSHAEnv      = "MOESEKAI_RECOVERY_V2_ACCEPTANCE_PLAN_SHA256"
	acceptanceSourceRootEnv   = "MOESEKAI_RECOVERY_V2_ACCEPTANCE_SOURCE_ROOT"
	acceptanceRunbookRootPath = "/private/tmp/moesekai-704-recovery-v2/runs/"
)

func TestPrepareRecoveryRunbookOfflineAcceptanceInputs(t *testing.T) {
	root := os.Getenv(acceptanceRootEnv)
	if root == "" {
		t.Skip("operator-only recovery-v2 acceptance input preparation")
	}
	sourceRoot := os.Getenv(acceptanceSourceRootEnv)
	requireAcceptanceDirectory(t, root)
	if sourceRoot == "" || !filepath.IsAbs(sourceRoot) || filepath.Clean(sourceRoot) != sourceRoot {
		t.Fatal("acceptance source root is not an explicit canonical absolute path")
	}
	inputs := filepath.Join(root, "inputs")
	outputs := filepath.Join(root, "outputs")
	for _, path := range []string{inputs, outputs} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	binding := fixtureCatalogBinding()
	catalog, verification, err := OpenCatalogAgainstPlan(ctx, fixtureCatalogPath, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	parentLedger, err := lyricsacquisition.CreateLedger(ctx, filepath.Join(inputs, "parent-ledger"))
	if err != nil {
		t.Fatal(err)
	}
	parent := fixtureParentRoot(t, ctx, inputs, parentLedger, binding, catalog.MusicIDs())
	if err := parentLedger.Close(); err != nil {
		t.Fatal(err)
	}
	parentBody, err := lyricsrootmanifest.MarshalCanonical(parent)
	if err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(inputs, "parent-root.json")
	if err := lyricsrootmanifest.PublishCreateExclusive(parentPath, parentBody); err != nil {
		t.Fatal(err)
	}

	plan := fixtureRecoveryPlan(t, outputs, binding, parent)
	plan.SourceSnapshot, err = lyricsextractionplan.PrepareRecoverySourceSnapshot(sourceRoot, "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := lyricsextractionplan.VerifyRecoverySourceSnapshot(sourceRoot, plan); err != nil {
		t.Fatal(err)
	}
	planBody, err := lyricsextractionplan.MarshalRecoveryCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	planSHA, err := lyricsextractionplan.RecoveryCanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(inputs, "recovery-plan-v2.json")
	writePrivateExclusive(t, planPath, planBody)
	t.Logf("PASS prepared plan=%s planSha256=%s sourceSnapshotSha256=%s sourceFiles=%d parent=%s catalogCount=%d",
		planPath, planSHA, plan.SourceSnapshot.SHA256, len(plan.SourceSnapshot.Files), parentPath, verification.RecordCount)
}

func TestSeedRecoveryRunbookOfflineFixtureLedger(t *testing.T) {
	planPath := os.Getenv(acceptancePlanEnv)
	if planPath == "" {
		t.Skip("operator-only recovery-v2 offline fixture seeding")
	}
	expectedSHA := os.Getenv(acceptancePlanSHAEnv)
	sourceRoot := os.Getenv(acceptanceSourceRootEnv)
	plan := readAcceptancePlan(t, planPath, expectedSHA)
	if err := lyricsextractionplan.VerifyRecoverySourceSnapshot(sourceRoot, plan); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		plan.Outputs.Ledger, plan.Outputs.AcquisitionSet, plan.Outputs.ProviderOutcomes,
		plan.Outputs.SongResults, plan.Outputs.EvidencePack, plan.Outputs.RootManifest,
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("acceptance output is not create-exclusive: %s err=%v", path, err)
		}
	}

	counter := &rejectingAcceptanceDefaultTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = counter
	defer func() { http.DefaultTransport = previous }()

	ctx := context.Background()
	catalog, verification, err := OpenCatalogAgainstPlan(ctx, plan.Catalog.Path, plan.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	ledger, err := lyricsacquisition.CreateLedger(ctx, plan.Outputs.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	transports := fixtureProviderTransports(t)
	session, err := NewAcquisitionSession(runtime, ledger, transports)
	if err != nil {
		t.Fatal(err)
	}
	songs := make([]SongAcquisitionSet, 0, len(plan.Scope.MusicIDs))
	for _, musicID := range plan.Scope.MusicIDs {
		identity, err := catalog.MusicIdentity(ctx, musicID)
		if err != nil {
			t.Fatal(err)
		}
		providers, progress, err := session.AcquireSong(ctx, musicID, identity)
		if err != nil {
			t.Fatal(err)
		}
		if len(providers) != 1 || len(progress) != 1 {
			t.Fatalf("music %d fixture evaluated provider prefix is not length one", musicID)
		}
		if providers[0].Provider != model.LyricsSourceProviderSekaipedia ||
			providers[0].Status != lyricsprovideroutcome.StatusCandidate {
			t.Fatalf("music %d Sekaipedia fixture terminal=%+v", musicID, providers[0])
		}
		songs = append(songs, SongAcquisitionSet{MusicID: musicID, Providers: providers})
	}
	sort.Slice(songs, func(left, right int) bool { return songs[left].MusicID < songs[right].MusicID })
	set, err := NewAcquisitionSet(plan.PlanID, expectedSHA, runtime.Order, songs)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAcquisitionSetAuthorization(
		set, plan.PlanID, expectedSHA, plan.Scope.MusicIDs, runtime.Order, runtime.ProviderMusicIDs,
	); err != nil {
		t.Fatal(err)
	}
	if err := PublishAcquisitionSet(plan.Outputs.AcquisitionSet, set); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if counter.Count() != 0 {
		t.Fatalf("offline fixture seed touched the default network transport %d times", counter.Count())
	}
	if transports[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper).requestCount() == 0 ||
		transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatal("offline fixture crossed the complete Sekaipedia stopping point")
	}
	assertNoRomanizationTokens(t, plan.Outputs.Ledger, plan.Outputs.AcquisitionSet)
	t.Logf("PASS seeded exact fixture ledger songs=%d catalogCount=%d setSha256=%s defaultNetworkRequests=0",
		len(songs), verification.RecordCount, set.SetSHA256)
}

type rejectingAcceptanceDefaultTransport struct {
	mu       sync.Mutex
	requests int
}

func (transport *rejectingAcceptanceDefaultTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.requests++
	transport.mu.Unlock()
	return nil, errors.New("default network transport is forbidden during offline recovery acceptance")
}

func (transport *rejectingAcceptanceDefaultTransport) Count() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.requests
}

func readAcceptancePlan(t *testing.T, path, expectedSHA string) lyricsextractionplan.RecoveryPlan {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
		info.Size() <= 0 || info.Size() > lyricsextractionplan.MaxPlanBytes {
		t.Fatal("acceptance recovery plan is not a direct mode-0600 bounded file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, actual, err := lyricsextractionplan.CheckRecovery(body, expectedSHA)
	if err != nil {
		t.Fatalf("acceptance recovery plan digest=%s err=%v", actual, err)
	}
	return plan
}

func requireAcceptanceDirectory(t *testing.T, path string) {
	t.Helper()
	if !strings.HasPrefix(path, acceptanceRunbookRootPath) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("acceptance root is outside the recovery-v2 runs root")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		t.Fatal("acceptance root must be a direct mode-0700 directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		t.Fatal("acceptance root must not traverse a symlink or filesystem alias")
	}
}

func writePrivateExclusive(t *testing.T, path string, body []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertNoRomanizationTokens(t *testing.T, roots ...string) {
	t.Helper()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lower := bytes.ToLower(body)
			for _, forbidden := range [][]byte{[]byte("romaji"), []byte("romanization"), []byte("romanized")} {
				if bytes.Contains(lower, forbidden) {
					return errors.New("persisted offline fixture contains a forbidden romanization token")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
