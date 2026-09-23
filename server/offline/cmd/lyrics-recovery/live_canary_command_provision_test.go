package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/offline/internal/lyricsprovidercoord"
)

func TestLiveStateProvisionCommandIsExplicitNoNetworkAndPlanFree(t *testing.T) {
	counter := &rejectingDefaultTransport{}
	previousDefault := http.DefaultTransport
	http.DefaultTransport = counter
	t.Cleanup(func() { http.DefaultTransport = previousDefault })

	var provisionCalls atomic.Int32
	previousProvision := provisionRecoveryLiveState
	provisionRecoveryLiveState = func() error {
		provisionCalls.Add(1)
		return nil
	}
	t.Cleanup(func() { provisionRecoveryLiveState = previousProvision })
	previousAcquire := acquireRecoveryLiveOwnership
	acquireRecoveryLiveOwnership = func() (recoveryLiveOwnership, error) {
		return nil, errors.New("provisioning touched live acquisition ownership")
	}
	t.Cleanup(func() { acquireRecoveryLiveOwnership = previousAcquire })

	var unauthorized bytes.Buffer
	if err := run(context.Background(), []string{"-mode", "provision-live-state"}, &unauthorized); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unauthorized.String(), "HOLD mode=provision-live-state") || provisionCalls.Load() != 0 {
		t.Fatalf("unauthorized provisioning output=%q calls=%d", unauthorized.String(), provisionCalls.Load())
	}

	var authorized bytes.Buffer
	if err := run(context.Background(), []string{
		"-mode", "provision-live-state",
		"-live-state-provision-authorization", liveStateProvisionAuthorization,
	}, &authorized); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authorized.String(), "PASS mode=provision-live-state") ||
		!strings.Contains(authorized.String(), "publication=create-exclusive network=HOLD cooldown=preserved") ||
		provisionCalls.Load() != 1 || counter.requests.Load() != 0 {
		t.Fatalf("authorized provisioning output=%q calls=%d network=%d",
			authorized.String(), provisionCalls.Load(), counter.requests.Load())
	}
	if _, err := parseOptions([]string{
		"-mode", "provision-live-state",
		"-live-state-provision-authorization", liveStateProvisionAuthorization,
		"-live-state-root", filepath.Join("/private/tmp", "bypass"),
	}); err == nil {
		t.Fatal("provisioning accepted a live-state root override")
	}
	if _, err := parseOptions([]string{
		"-mode", "provision-live-state",
		"-live-state-provision-authorization", liveStateProvisionAuthorization,
		"-plan", "/private/tmp/unrelated-plan.json",
	}); err == nil {
		t.Fatal("provisioning accepted unrelated recovery-plan inputs")
	}
}

func TestProvisionRecoveryLiveStateRootIsCreateExclusiveAndNeverResetsCooldown(t *testing.T) {
	created, err := os.MkdirTemp(recoveryCommandTestRoot, "live-state-provision-path-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(created); err != nil {
		t.Fatal(err)
	}
	root := created
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	if err := provisionRecoveryLiveStateRoot(root); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("provisioned root info=%v err=%v", rootInfo, err)
	}
	for _, name := range []string{"global-live.lock", "vocaloid_fandom.json", "moegirl.json", "sekaipedia.json"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || info.Mode().Type() != 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("provisioned entry %s info=%v err=%v", name, info, err)
		}
	}
	owner, err := lyricsprovidercoord.Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	sekaipediaPath := filepath.Join(root, "sekaipedia.json")
	body, err := os.ReadFile(sekaipediaPath)
	if err != nil {
		t.Fatal(err)
	}
	var record provisionedLiveStateRecordV1
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	record.Generation += 7
	record.NotBefore = time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339Nano)
	record.FailureCount = 7
	body, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(sekaipediaPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), body...)
	if err := provisionRecoveryLiveStateRoot(root); err == nil || !strings.Contains(err.Error(), "existing root") {
		t.Fatalf("existing provisioned root error=%v", err)
	}
	after, err := os.ReadFile(sekaipediaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("create-exclusive provisioning reset future cooldown: before=%s after=%s", before, after)
	}
}

func TestProvisionRecoveryLiveStateRootRefusesAmbiguousLeaf(t *testing.T) {
	target, err := os.MkdirTemp(recoveryCommandTestRoot, "live-state-provision-target-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(target) })
	alias := target + "-alias"
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	if err := provisionRecoveryLiveStateRoot(alias); err == nil ||
		(!strings.Contains(err.Error(), "existing root") && !strings.Contains(err.Error(), "ambiguous")) {
		t.Fatalf("ambiguous live-state root error=%v", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ambiguous target was modified entries=%v err=%v", entries, err)
	}
}
