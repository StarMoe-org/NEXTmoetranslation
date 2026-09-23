package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"moesekai/server/offline/internal/lyricsextractionplan"
)

func TestRunChecksSyntheticPlanAndDeclaredFilesOffline(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan, body, digest := writeSyntheticCommandPlan(t, root)
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run([]string{
		"-plan", planPath,
		"-expected-sha256", digest,
		"-root", root,
	}, &stdout); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "valid extraction-plan v1 "+digest+"\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(plan.Outputs[0].Path))); !os.IsNotExist(err) {
		t.Fatalf("offline checker created an extraction output: %v", err)
	}
}

func TestRunRejectsDigestOrDeclaredFileDrift(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan, body, digest := writeSyntheticCommandPlan(t, root)
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"-plan", planPath, "-expected-sha256", strings.Repeat("0", 64), "-root", root,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "expected digest") {
		t.Fatalf("digest mismatch error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(plan.Inputs[0].Path)), []byte("changed catalog identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"-plan", planPath, "-expected-sha256", digest, "-root", root,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("declared file drift error=%v", err)
	}
}

func TestRunRequiresOnlyExplicitFlags(t *testing.T) {
	for name, arguments := range map[string][]string{
		"missing":    {},
		"positional": {"unexpected"},
		"whitespace": {"-plan", " plan.json", "-expected-sha256", strings.Repeat("0", 64), "-root", "."},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(arguments, &bytes.Buffer{}); err == nil {
				t.Fatal("invalid command arguments were accepted")
			}
		})
	}
}

func TestCheckerPackageHasNoNetworkOrSubprocessImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == "net" || strings.HasPrefix(path, "net/") || path == "os/exec" || path == "plugin" {
				t.Fatalf("offline checker imports execution dependency %q in %s", path, file)
			}
		}
	}
}

func writeSyntheticCommandPlan(t *testing.T, root string) (lyricsextractionplan.Plan, []byte, string) {
	t.Helper()
	catalogBody := []byte("synthetic command catalog")
	sourceBody := []byte("package commandfixture\n")
	writeCommandFixture(t, root, "inputs/catalog.db", catalogBody)
	writeCommandFixture(t, root, "source/parser.go", sourceBody)
	files := []lyricsextractionplan.SourceFileIdentity{{
		Path: "source/parser.go", SizeBytes: int64(len(sourceBody)), SHA256: commandSHA256(sourceBody),
	}}
	snapshotSHA, err := lyricsextractionplan.SourceSnapshotSHA256(files)
	if err != nil {
		t.Fatal(err)
	}
	versions := lyricsextractionplan.CompiledEffectiveVersions()
	plan := lyricsextractionplan.Plan{
		SchemaVersion:     lyricsextractionplan.SchemaVersionV1,
		CanonicalEncoding: lyricsextractionplan.CanonicalEncodingV1,
		DigestAlgorithm:   lyricsextractionplan.PlanDigestAlgorithm,
		PlanID:            "synthetic-command-plan-001", CreatedAt: "2026-08-01T00:00:01Z",
		Catalog: lyricsextractionplan.CatalogIdentity{
			InputID: "catalog", SchemaVersion: lyricsextractionplan.CatalogSchemaVersion,
			RuntimeSchemaVersion: lyricsextractionplan.MaximumCatalogRuntimeSchema,
			RecordCount:          2, IdentityPolicyVersion: versions.Policies.CatalogIdentity,
		},
		Inputs: []lyricsextractionplan.InputIdentity{{
			ID: "catalog", Kind: lyricsextractionplan.InputCatalogDatabase,
			Path: "inputs/catalog.db", SizeBytes: int64(len(catalogBody)), SHA256: commandSHA256(catalogBody),
		}},
		SourceSnapshot: lyricsextractionplan.SourceSnapshot{
			Algorithm:  lyricsextractionplan.SnapshotAlgorithmV1,
			CapturedAt: "2026-08-01T00:00:00Z", Files: files, SHA256: snapshotSHA,
		},
		Providers:         commandProviderConfiguration(t),
		EffectiveVersions: versions,
		Execution: lyricsextractionplan.ExecutionSettings{
			Concurrency: 4, MaxAttempts: 3, RequestTimeoutMillis: 8 * 60 * 1000,
			RetryDelayMillis: 250, Ceilings: lyricsextractionplan.CompiledHardCeilings(),
			SafetyFloors: lyricsextractionplan.CompiledSafetyFloors(),
		},
		Resume: lyricsextractionplan.ResumePolicy{
			Mode: lyricsextractionplan.ResumeFresh, InputID: "",
			RetryErrorCodes: []string{}, RetryMissingReasons: []string{}, RetryIncompleteCodes: []string{},
		},
		Outputs: lyricsextractionplan.RequiredOutputs([3]string{
			"outputs/preflight.json", "outputs/staging.json", "outputs/evidence-receipt.json",
		}),
		Deployment: lyricsextractionplan.RequiredDeploymentPolicy(),
	}
	body, err := lyricsextractionplan.MarshalCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := lyricsextractionplan.CanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan, body, digest
}

func commandProviderConfiguration(t *testing.T) lyricsextractionplan.ProviderConfiguration {
	t.Helper()
	providers := lyricsextractionplan.CompiledProviderConfiguration()
	sekaipedia := lyricsextractionplan.FixedAuthority{
		Disposition:    lyricsextractionplan.AuthorityActive,
		Role:           lyricsextractionplan.AuthorityRoleSongIndex,
		CaptureProfile: lyricsextractionplan.CaptureProfileMediaWikiAPIRevisionResponseV1,
		PageID:         7001, RevisionID: 7002, RevisionTimestamp: "2026-07-31T23:59:59Z",
		SHA1: strings.Repeat("a", 40), RawSHA256: strings.Repeat("b", 64), Title: "Command song index",
	}
	sekaipedia.CanonicalURL = lyricsextractionplan.FixedAuthorityCanonicalURL(
		lyricsextractionplan.ProviderSekaipedia, sekaipedia.Title, sekaipedia.RevisionID,
	)
	var err error
	sekaipedia.EvidenceID, err = lyricsextractionplan.FixedAuthorityEvidenceID(
		lyricsextractionplan.ProviderSekaipedia, sekaipedia.Role,
		sekaipedia.PageID, sekaipedia.RevisionID, sekaipedia.Title,
	)
	if err != nil {
		t.Fatal(err)
	}
	moegirl := lyricsextractionplan.FixedAuthority{
		Disposition:    lyricsextractionplan.AuthorityActive,
		Role:           lyricsextractionplan.AuthorityRoleSongIndex,
		CaptureProfile: lyricsextractionplan.CaptureProfileMediaWikiRevisionContentV1,
		PageID:         7003, RevisionID: 7004, SHA1: strings.Repeat("c", 40), Title: "命令测试歌曲索引",
	}
	moegirl.CanonicalURL = lyricsextractionplan.FixedAuthorityCanonicalURL(
		lyricsextractionplan.ProviderMoegirl, moegirl.Title, moegirl.RevisionID,
	)
	moegirl.EvidenceID, err = lyricsextractionplan.FixedAuthorityEvidenceID(
		lyricsextractionplan.ProviderMoegirl, moegirl.Role,
		moegirl.PageID, moegirl.RevisionID, moegirl.Title,
	)
	if err != nil {
		t.Fatal(err)
	}
	providers.Configurations[0].Authorities = []lyricsextractionplan.FixedAuthority{sekaipedia}
	providers.Configurations[1].Authorities = []lyricsextractionplan.FixedAuthority{moegirl}
	return providers
}

func writeCommandFixture(t *testing.T, root, relativePath string, body []byte) {
	t.Helper()
	absolutePath := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolutePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func commandSHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
