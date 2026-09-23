package lyricsextractionplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

func TestSyntheticPlanCanonicalRoundTripAndDigest(t *testing.T) {
	plan := syntheticPlan(t)
	body, err := MarshalCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || body[len(body)-1] == '\n' {
		t.Fatalf("canonical body boundary is invalid: %q", body)
	}
	digest, err := CanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	const wantDigest = "7c005d85e1ef611e1e18283e64e988a52eedd908c9dfa1bb33ed45c2cee93b62"
	if digest != wantDigest {
		t.Fatalf("synthetic canonical fixture digest=%s want=%s", digest, wantDigest)
	}
	decoded, actual, err := Check(body, digest)
	if err != nil {
		t.Fatal(err)
	}
	if actual != digest || decoded.PlanID != plan.PlanID || decoded.Catalog.RecordCount != 2 {
		t.Fatalf("decoded plan=%+v digest=%s", decoded, actual)
	}
}

func TestHistoricalRubyGeneratorAliasIsInboundOnlyAndCanonicalizesBeforeUse(t *testing.T) {
	current := mustCanonicalPlan(t, syntheticPlan(t))
	if bytes.Contains(current, []byte(historicalSekaipediaRubyGeneratorAlias)) ||
		!bytes.Contains(current, []byte(registeredSekaipediaRuby)) ||
		!bytes.Contains(current, []byte(registeredStructuredRuby)) {
		t.Fatalf("new canonical plan uses retired ruby vocabulary: %s", current)
	}

	historicalPlan := syntheticPlan(t)
	historicalPlan.EffectiveVersions = historicalEffectiveVersionsV1()
	historicalCanonical := mustCanonicalPlan(t, historicalPlan)
	historicalAlias := bytes.Replace(
		historicalCanonical,
		[]byte(historicalRegisteredSekaipediaRuby),
		[]byte(historicalSekaipediaRubyGeneratorAlias),
		1,
	)
	if bytes.Equal(historicalAlias, historicalCanonical) {
		t.Fatal("historical compatibility fixture did not change")
	}
	digest := sha256.Sum256(historicalAlias)
	decoded, actual, err := Check(historicalAlias, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if actual != hex.EncodeToString(digest[:]) ||
		decoded.EffectiveVersions.Parsers[0].RubyGeneratorVersion != historicalRegisteredSekaipediaRuby ||
		decoded.EffectiveVersions.Parsers[1].RubyGeneratorVersion != historicalRegisteredStructuredRuby {
		t.Fatalf("historical alias digest=%q decoded=%+v", actual, decoded.EffectiveVersions.Parsers)
	}
	remarshaled, err := MarshalCanonical(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(remarshaled, []byte(historicalSekaipediaRubyGeneratorAlias)) ||
		!bytes.Contains(remarshaled, []byte(historicalRegisteredSekaipediaRuby)) ||
		!bytes.Contains(remarshaled, []byte(historicalRegisteredStructuredRuby)) {
		t.Fatalf("retired ruby vocabulary escaped compatibility boundary: %s", remarshaled)
	}
	oldPlan := syntheticPlan(t)
	oldPlan.EffectiveVersions.Parsers[0].RubyGeneratorVersion = historicalSekaipediaRubyGeneratorAlias
	if err := Validate(oldPlan); err == nil {
		t.Fatal("new in-memory plan accepted retired ruby vocabulary")
	}
}

func TestDecodeCanonicalRejectsHostileJSONBoundaries(t *testing.T) {
	body := mustCanonicalPlan(t, syntheticPlan(t))
	deep := []byte(`{"adversarial":` + strings.Repeat("[", MaxPlanJSONDepth+1) + "0" + strings.Repeat("]", MaxPlanJSONDepth+1) + `}`)
	for name, mutated := range map[string][]byte{
		"unknown command": bytes.Replace(body, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"command":"sh -c rm"`), 1),
		"duplicate":       bytes.Replace(body, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1),
		"trailing value":  append(append([]byte{}, body...), []byte(`{}`)...),
		"trailing space":  append(append([]byte{}, body...), ' '),
		"invalid UTF-8":   append([]byte{0xff}, body...),
		"lone surrogate":  []byte(`{"schemaVersion":1,"planId":"\uD800"}`),
		"excessive depth": deep,
		"oversized":       bytes.Repeat([]byte{' '}, MaxPlanBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCanonical(mutated); err == nil {
				t.Fatal("invalid extraction plan JSON was accepted")
			}
		})
	}
}

func TestValidateRejectsIdentityPolicyAndSafetyDrift(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"noncanonical input hash": func(plan *Plan) {
			plan.Inputs[0].SHA256 = strings.ToUpper(plan.Inputs[0].SHA256)
		},
		"noncanonical timestamp": func(plan *Plan) {
			plan.CreatedAt = "2026-08-01T00:00:00+00:00"
		},
		"duplicate input identity": func(plan *Plan) {
			duplicate := plan.Inputs[0]
			plan.Inputs = append(plan.Inputs, duplicate)
		},
		"duplicate source identity": func(plan *Plan) {
			plan.SourceSnapshot.Files = append(plan.SourceSnapshot.Files, plan.SourceSnapshot.Files[0])
			plan.SourceSnapshot.SHA256 = strings.Repeat("0", 64)
		},
		"shell fragment path": func(plan *Plan) {
			plan.Outputs[0].Path = "out/report.json;rm"
		},
		"unapproved origin": func(plan *Plan) {
			plan.Providers.Configurations[0].Origin = "https://sekaipedia.org"
		},
		"rewritten authority evidence ID": func(plan *Plan) {
			plan.Providers.Configurations[0].Authorities[0].EvidenceID = "authority:sekaipedia:rewritten:1"
		},
		"noncanonical authority hash": func(plan *Plan) {
			plan.Providers.Configurations[0].Authorities[0].SHA1 = strings.ToUpper(plan.Providers.Configurations[0].Authorities[0].SHA1)
		},
		"noncanonical authority timestamp": func(plan *Plan) {
			authority := &plan.Providers.Configurations[0].Authorities[0]
			authority.RevisionTimestamp = strings.TrimSuffix(authority.RevisionTimestamp, "Z") + "+00:00"
		},
		"wrong authority role": func(plan *Plan) {
			plan.Providers.Configurations[0].Authorities[0].Role = "other"
		},
		"wrong authority capture profile": func(plan *Plan) {
			plan.Providers.Configurations[0].Authorities[0].CaptureProfile = CaptureProfileMediaWikiRevisionContentV1
		},
		"duplicate provider identity": func(plan *Plan) {
			plan.Providers.Configurations[1] = plan.Providers.Configurations[0]
		},
		"unbounded authority identity": func(plan *Plan) {
			plan.Providers.Configurations[0].Authorities[0].RevisionID = MaxMediaWikiIdentity + 1
		},
		"catalog exceeds plan ceiling": func(plan *Plan) {
			plan.Execution.Ceilings.CatalogRecords = plan.Catalog.RecordCount - 1
		},
		"authority exceeds plan title ceiling": func(plan *Plan) {
			plan.Execution.Ceilings.CandidateTitleBytes = 1
		},
		"ceiling drift": func(plan *Plan) {
			plan.Execution.Ceilings.Concurrency++
		},
		"floor drift": func(plan *Plan) {
			plan.Execution.SafetyFloors.ProviderCrawlDelayMillis--
		},
		"snapshot mismatch": func(plan *Plan) {
			plan.SourceSnapshot.SHA256 = strings.Repeat("0", 64)
		},
		"output aliases input": func(plan *Plan) {
			plan.Outputs[0].Path = plan.Inputs[0].Path
		},
		"unsafe resume code": func(plan *Plan) {
			plan.Inputs = append(plan.Inputs, InputIdentity{
				ID: "resume", Kind: InputResumeReport, Path: "inputs/resume.json", SizeBytes: 2,
				SHA256: sha256Hex([]byte("{}")),
			})
			plan.Resume = ResumePolicy{
				Mode: ResumeReport, InputID: "resume", RetryErrorCodes: []string{"arbitrary_error"},
				RetryMissingReasons: []string{}, RetryIncompleteCodes: []string{},
			}
		},
		"released deployment": func(plan *Plan) {
			plan.Deployment.State = "RELEASE"
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticPlan(t)
			mutate(&plan)
			if err := Validate(plan); err == nil {
				t.Fatal("invalid extraction plan was accepted")
			}
		})
	}
}

func TestFixedIndexProvidersRequireExactlyOneActiveSongIndex(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *Plan){
		"Sekaipedia omitted": func(_ *testing.T, plan *Plan) {
			plan.Providers.Configurations[0].Authorities = []FixedAuthority{}
		},
		"Moegirl omitted": func(_ *testing.T, plan *Plan) {
			plan.Providers.Configurations[1].Authorities = []FixedAuthority{}
		},
		"Sekaipedia no active": func(_ *testing.T, plan *Plan) {
			plan.Providers.Configurations[0].Authorities[0].Disposition = AuthorityRetained
		},
		"Moegirl no active": func(_ *testing.T, plan *Plan) {
			plan.Providers.Configurations[1].Authorities[0].Disposition = AuthorityRetained
		},
		"Sekaipedia multiple active": func(t *testing.T, plan *Plan) {
			second := futureProviderTestPlanData(t).Configurations[0].Authorities[0]
			plan.Providers.Configurations[0].Authorities = append(plan.Providers.Configurations[0].Authorities, second)
		},
		"Moegirl multiple active": func(t *testing.T, plan *Plan) {
			second := futureProviderTestPlanData(t).Configurations[1].Authorities[0]
			plan.Providers.Configurations[1].Authorities = append(plan.Providers.Configurations[1].Authorities, second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticPlan(t)
			mutate(t, &plan)
			if err := Validate(plan); err == nil {
				t.Fatal("invalid active song-index authority selection was accepted")
			}
		})
	}
}

func TestDifferentReviewedFutureAuthoritiesValidateWithoutSourceEdits(t *testing.T) {
	plan := syntheticPlan(t)
	plan.Providers = futureProviderTestPlanData(t)
	if err := Validate(plan); err != nil {
		t.Fatalf("reviewed future authority data was rejected: %v", err)
	}
	body := mustCanonicalPlan(t, plan)
	digest, err := CanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, actual, err := Check(body, digest); err != nil || actual != digest {
		t.Fatalf("future plan digest binding actual=%q err=%v", actual, err)
	}
}

func TestOrderedRetainedAuthoritiesPreservePlanDerivedEvidenceIDs(t *testing.T) {
	plan := syntheticPlan(t)
	future := futureProviderTestPlanData(t)
	for providerIndex := 0; providerIndex < 2; providerIndex++ {
		retained := plan.Providers.Configurations[providerIndex].Authorities[0]
		retained.Disposition = AuthorityRetained
		active := future.Configurations[providerIndex].Authorities[0]
		plan.Providers.Configurations[providerIndex].Authorities = []FixedAuthority{active, retained}
	}
	additionalRetained := newFutureAuthority(
		t, ProviderSekaipedia, 8001, 8002, "Earlier reviewed index", strings.Repeat("d", 40), strings.Repeat("e", 64),
	)
	additionalRetained.Disposition = AuthorityRetained
	plan.Providers.Configurations[0].Authorities = append(
		plan.Providers.Configurations[0].Authorities, additionalRetained,
	)
	before := cloneProviderConfiguration(plan.Providers)
	if err := Validate(plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Providers, before) {
		t.Fatal("validation rewrote retained authority identities")
	}
	for _, configured := range plan.Providers.Configurations[:2] {
		for _, authority := range configured.Authorities {
			derived, err := FixedAuthorityEvidenceID(
				configured.Provider, authority.Role, authority.PageID, authority.RevisionID, authority.Title,
			)
			if err != nil || derived != authority.EvidenceID {
				t.Fatalf("derived evidence ID=%q authority=%+v err=%v", derived, authority, err)
			}
		}
	}
	plan.Providers.Configurations[0].Authorities[0], plan.Providers.Configurations[0].Authorities[1] =
		plan.Providers.Configurations[0].Authorities[1], plan.Providers.Configurations[0].Authorities[0]
	if err := Validate(plan); err == nil || !strings.Contains(err.Error(), "order") {
		t.Fatalf("noncanonical authority order error=%v", err)
	}
}

func TestUnregisteredImplementationVersionsFail(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"policy": func(plan *Plan) {
			plan.EffectiveVersions.Policies.Matching = "mandatory-gates-v2"
		},
		"parser": func(plan *Plan) {
			plan.EffectiveVersions.Parsers[0].ParserVersion = "sekaipedia-list-song-parser-v2"
		},
		"ruby": func(plan *Plan) {
			plan.EffectiveVersions.Parsers[1].RubyGeneratorVersion = "kagome-ipadic-v2"
		},
		"algorithm": func(plan *Plan) {
			plan.EffectiveVersions.Algorithms.ProviderSelection = "sekaipedia-moegirl-vocaloid-fandom-v2"
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticPlan(t)
			mutate(&plan)
			if err := Validate(plan); err == nil || !strings.Contains(err.Error(), "registered") {
				t.Fatalf("unregistered implementation error=%v", err)
			}
		})
	}
}

func TestCompiledProviderConfigurationContainsNoMutableAuthorityData(t *testing.T) {
	providers := CompiledProviderConfiguration()
	if len(providers.Order) != 3 || len(providers.Configurations) != 3 {
		t.Fatalf("compiled provider union=%+v", providers)
	}
	for _, configured := range providers.Configurations {
		if configured.Authorities == nil || len(configured.Authorities) != 0 {
			t.Fatalf("compiled provider %q leaked mutable authority data: %+v", configured.Provider, configured.Authorities)
		}
	}
}

func TestCentralSchemaAndCeilingConstantsRemainPinned(t *testing.T) {
	versions := CompiledEffectiveVersions()
	if versions.Schemas.Catalog != CatalogSchemaVersion ||
		versions.Schemas.MaximumCatalogRuntime != MaximumCatalogRuntimeSchema ||
		versions.Schemas.PreflightReport != PreflightSchemaVersion ||
		versions.Schemas.EvidenceReceipt != EvidenceReceiptSchemaVersion ||
		versions.Schemas.StagingManifest != StagingManifestSchemaVersion ||
		versions.Schemas.LyricsSourceDocument != model.LyricsSourceDocumentSchemaVersion {
		t.Fatalf("central schema versions drifted: %+v", versions.Schemas)
	}
	if CatalogSchemaVersion != 18 || MaximumCatalogRuntimeSchema != 23 || PreflightSchemaVersion != 1 ||
		EvidenceReceiptSchemaVersion != 1 || StagingManifestSchemaVersion != 3 {
		t.Fatal("legacy schema pin changed")
	}
	ceilings := CompiledHardCeilings()
	if ceilings.ProviderResponseBytes != lyricsproviderpolicy.ResponseSizeCeilingBytesV1 ||
		ceilings.FixedArtifacts != 16 || ceilings.PreflightReportBytes != 96<<20 ||
		ceilings.EvidenceReceiptBytes != 64<<20 || ceilings.EvidenceReceiptRawBytes != 32<<20 ||
		ceilings.LyricsSourceDocumentBytes != model.MaxLyricsSourceDocumentBytes ||
		ceilings.LyricsSourceJSONDepth != model.MaxLyricsSourceJSONDepth {
		t.Fatalf("central hard ceilings drifted: %+v", ceilings)
	}
}

func TestScopedProductionSourcesContainNoCurrentAuthorityLiterals(t *testing.T) {
	current := currentAuthorityTestPlanData()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directories := []string{
		workingDirectory,
		filepath.Join(workingDirectory, "..", "..", "cmd", "lyrics-extraction-plan-check"),
	}
	for _, directory := range directories {
		files, err := filepath.Glob(filepath.Join(directory, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, configured := range current.Configurations[:2] {
				for _, authority := range configured.Authorities {
					for _, value := range []string{
						authority.EvidenceID, authority.RevisionTimestamp, authority.SHA1,
						authority.RawSHA256, authority.Title, authority.CanonicalURL,
					} {
						if value != "" && bytes.Contains(body, []byte(value)) {
							t.Fatalf("current authority literal %q is compiled into %s", value, file)
						}
					}
					for _, identity := range []int{authority.PageID, authority.RevisionID} {
						pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(strconv.Itoa(identity)) + `\b`)
						if pattern.Match(body) {
							t.Fatalf("current authority identity %d is compiled into %s", identity, file)
						}
					}
				}
			}
		}
	}
}

func TestCheckRejectsPlanExpectedDigestMismatch(t *testing.T) {
	body := mustCanonicalPlan(t, syntheticPlan(t))
	if _, actual, err := Check(body, strings.Repeat("0", 64)); err == nil || actual == "" {
		t.Fatalf("digest mismatch actual=%q err=%v", actual, err)
	}
	if _, _, err := Check(body, strings.Repeat("A", 64)); err == nil {
		t.Fatal("noncanonical expected digest was accepted")
	}
}

func TestValidReportAndCheckpointResumePolicies(t *testing.T) {
	for name, inputKind := range map[string]InputKind{
		"report":     InputResumeReport,
		"checkpoint": InputResumeCheckpoint,
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticPlan(t)
			resumeBody := []byte("synthetic resume")
			plan.Inputs = append(plan.Inputs, InputIdentity{
				ID: "resume", Kind: inputKind, Path: "inputs/resume.bin",
				SizeBytes: int64(len(resumeBody)), SHA256: sha256Hex(resumeBody),
			})
			plan.Resume.InputID = "resume"
			if inputKind == InputResumeReport {
				plan.Resume.Mode = ResumeReport
				plan.Resume.RetryErrorCodes = []string{"rate_limited", "timeout"}
				plan.Resume.RetryMissingReasons = []string{"no_search_hits"}
				plan.Resume.RetryIncompleteCodes = []string{"ambiguous_source", "unsupported_format"}
				plan.Resume.RevalidateUniqueComplete = true
			} else {
				plan.Resume.Mode = ResumeCheckpoint
			}
			if err := Validate(plan); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifyDeclaredFilesIsOfflineReadOnlyAndExact(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalogFile := writeSyntheticFile(t, root, "inputs/catalog.db", []byte("synthetic catalog snapshot"))
	catalog := InputIdentity{
		ID: "catalog", Kind: InputCatalogDatabase, Path: catalogFile.Path,
		SizeBytes: catalogFile.SizeBytes, SHA256: catalogFile.SHA256,
	}
	source := writeSyntheticFile(t, root, "source/parser.go", []byte("package synthetic\n"))
	plan := syntheticPlanWithFiles(t, catalog, []SourceFileIdentity{source})
	if err := VerifyDeclaredFiles(root, plan); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(source.Path)), []byte("package changed!!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDeclaredFiles(root, plan); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("source drift error=%v", err)
	}
}

func TestVerifyDeclaredFilesRejectsInvalidVerificationRootForms(t *testing.T) {
	canonicalParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(canonicalParent, "real-root")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkRoot := filepath.Join(canonicalParent, "symlink-root")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name          string
		root          string
		errorContains string
	}{
		{name: "non-absolute", root: ".", errorContains: "verification root must be an explicit canonical absolute path"},
		{name: "unclean", root: realRoot + string(filepath.Separator) + ".", errorContains: "verification root must be an explicit canonical absolute path"},
		{name: "root symlink", root: symlinkRoot, errorContains: "verification root must be a real directory, not a symlink"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := VerifyDeclaredFiles(test.root, syntheticPlan(t)); err == nil || !strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("verification root error=%v, want substring %q", err, test.errorContains)
			}
		})
	}
}

func TestVerifyDeclaredFilesRejectsAliasedVerificationRoot(t *testing.T) {
	aliasRoot := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(aliasRoot)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalRoot == aliasRoot {
		t.Skip("temporary directory does not expose a distinct filesystem alias")
	}
	catalogFile := writeSyntheticFile(t, canonicalRoot, "inputs/catalog.db", []byte("synthetic catalog snapshot"))
	catalog := InputIdentity{
		ID: "catalog", Kind: InputCatalogDatabase, Path: catalogFile.Path,
		SizeBytes: catalogFile.SizeBytes, SHA256: catalogFile.SHA256,
	}
	source := writeSyntheticFile(t, canonicalRoot, "source/parser.go", []byte("package synthetic\n"))
	plan := syntheticPlanWithFiles(t, catalog, []SourceFileIdentity{source})
	if err := VerifyDeclaredFiles(canonicalRoot, plan); err != nil {
		t.Fatalf("canonical verification root error=%v", err)
	}
	if err := VerifyDeclaredFiles(aliasRoot, plan); err == nil ||
		!strings.Contains(err.Error(), "verification root must not traverse a symlink or filesystem alias") {
		t.Fatalf("aliased verification root error=%v", err)
	}
}

func TestVerifyDeclaredFilesRejectsCatalogSidecar(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalogFile := writeSyntheticFile(t, root, "inputs/catalog.db", []byte("synthetic catalog snapshot"))
	catalog := InputIdentity{
		ID: "catalog", Kind: InputCatalogDatabase, Path: catalogFile.Path,
		SizeBytes: catalogFile.SizeBytes, SHA256: catalogFile.SHA256,
	}
	source := writeSyntheticFile(t, root, "source/parser.go", []byte("package synthetic\n"))
	plan := syntheticPlanWithFiles(t, catalog, []SourceFileIdentity{source})
	if err := os.WriteFile(filepath.Join(root, "inputs", "catalog.db-wal"), []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDeclaredFiles(root, plan); err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("catalog sidecar error=%v", err)
	}
}

func TestVerifyDeclaredFilesRejectsExistingOrSymlinkedCreateExclusiveOutput(t *testing.T) {
	tests := map[string]struct {
		prepare       func(*testing.T, string, string)
		errorContains string
	}{
		"existing": {
			prepare: func(t *testing.T, root, output string) {
				writeSyntheticFile(t, root, output, []byte("occupied"))
			},
			errorContains: "create-exclusive output path already exists",
		},
		"symlinked parent": {
			prepare: func(t *testing.T, root, output string) {
				target := filepath.Join(root, "private-output")
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, "outputs")); err != nil {
					t.Fatal(err)
				}
			},
			errorContains: "create-exclusive output path must not traverse a symlink",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			catalogFile := writeSyntheticFile(t, root, "inputs/catalog.db", []byte("synthetic catalog snapshot"))
			catalog := InputIdentity{
				ID: "catalog", Kind: InputCatalogDatabase, Path: catalogFile.Path,
				SizeBytes: catalogFile.SizeBytes, SHA256: catalogFile.SHA256,
			}
			source := writeSyntheticFile(t, root, "source/parser.go", []byte("package synthetic\n"))
			plan := syntheticPlanWithFiles(t, catalog, []SourceFileIdentity{source})
			test.prepare(t, root, plan.Outputs[0].Path)
			if err := VerifyDeclaredFiles(root, plan); err == nil || !strings.Contains(err.Error(), test.errorContains) {
				t.Fatalf("create-exclusive output error=%v, want substring %q", err, test.errorContains)
			}
		})
	}
}

func TestVerifyDeclaredFilesRejectsSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalogFile := writeSyntheticFile(t, root, "inputs/catalog.db", []byte("synthetic catalog snapshot"))
	catalog := InputIdentity{
		ID: "catalog", Kind: InputCatalogDatabase, Path: catalogFile.Path,
		SizeBytes: catalogFile.SizeBytes, SHA256: catalogFile.SHA256,
	}
	target := writeSyntheticFile(t, root, "private/parser.go", []byte("package synthetic\n"))
	linkPath := filepath.Join(root, "source", "parser.go")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, filepath.FromSlash(target.Path)), linkPath); err != nil {
		t.Fatal(err)
	}
	source := target
	source.Path = "source/parser.go"
	plan := syntheticPlanWithFiles(t, catalog, []SourceFileIdentity{source})
	if err := VerifyDeclaredFiles(root, plan); err == nil ||
		!strings.Contains(err.Error(), "declared file path must not traverse a symlink") {
		t.Fatalf("declared-file symlink error=%v", err)
	}
}

func TestPlanPackageHasNoNetworkOrSubprocessImports(t *testing.T) {
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
			if path == "net" || path == "net/http" || path == "os/exec" || path == "plugin" ||
				strings.HasPrefix(path, "net/rpc") {
				t.Fatalf("data-only plan package imports execution dependency %q in %s", path, file)
			}
		}
	}
}

func TestDataPathsCannotCarryShellFragments(t *testing.T) {
	for _, value := range []string{
		"out/report.json;rm", "out/$(touch-x)", "out/a b", "../outside", "/absolute", "out\\windows",
	} {
		if validDataPath(value) || !containsShellFragment(value) && strings.ContainsAny(value, ";$ \\") {
			t.Fatalf("unsafe data path was accepted: %q", value)
		}
	}
}

func syntheticPlan(t *testing.T) Plan {
	t.Helper()
	catalogBytes := []byte("synthetic catalog snapshot")
	sourceBytes := []byte("package synthetic\n")
	return syntheticPlanWithFiles(t,
		InputIdentity{
			ID: "catalog", Kind: InputCatalogDatabase, Path: "inputs/catalog.db",
			SizeBytes: int64(len(catalogBytes)), SHA256: sha256Hex(catalogBytes),
		},
		[]SourceFileIdentity{{
			Path: "source/parser.go", SizeBytes: int64(len(sourceBytes)), SHA256: sha256Hex(sourceBytes),
		}},
	)
}

func syntheticPlanWithFiles(t *testing.T, catalog InputIdentity, sourceFiles []SourceFileIdentity) Plan {
	t.Helper()
	snapshotSHA, err := SourceSnapshotSHA256(sourceFiles)
	if err != nil {
		t.Fatal(err)
	}
	return Plan{
		SchemaVersion: SchemaVersionV1, CanonicalEncoding: CanonicalEncodingV1,
		DigestAlgorithm: PlanDigestAlgorithm, PlanID: "synthetic-plan-001",
		CreatedAt: "2026-08-01T00:00:01Z",
		Catalog: CatalogIdentity{
			InputID: catalog.ID, SchemaVersion: CatalogSchemaVersion,
			RuntimeSchemaVersion: MaximumCatalogRuntimeSchema, RecordCount: 2,
			IdentityPolicyVersion: CompiledEffectiveVersions().Policies.CatalogIdentity,
		},
		Inputs: []InputIdentity{catalog},
		SourceSnapshot: SourceSnapshot{
			Algorithm: SnapshotAlgorithmV1, CapturedAt: "2026-08-01T00:00:00Z",
			Files: sourceFiles, SHA256: snapshotSHA,
		},
		Providers: currentAuthorityTestPlanData(), EffectiveVersions: CompiledEffectiveVersions(),
		Execution: ExecutionSettings{
			Concurrency: 4, MaxAttempts: 3, RequestTimeoutMillis: 8 * 60 * 1000,
			RetryDelayMillis: 250, Ceilings: CompiledHardCeilings(), SafetyFloors: CompiledSafetyFloors(),
		},
		Resume: ResumePolicy{
			Mode: ResumeFresh, InputID: "", RetryErrorCodes: []string{},
			RetryMissingReasons: []string{}, RetryIncompleteCodes: []string{},
		},
		Outputs: RequiredOutputs([3]string{
			"outputs/preflight.json", "outputs/staging.json", "outputs/evidence-receipt.json",
		}),
		Deployment: RequiredDeploymentPolicy(),
	}
}

// currentAuthorityTestPlanData is the sole test-plan fixture containing the
// presently reviewed authority values. Production code must derive and validate
// these fields from plan data without compiling any of them as defaults.
func currentAuthorityTestPlanData() ProviderConfiguration {
	providers := CompiledProviderConfiguration()
	providers.Configurations[0].Authorities = []FixedAuthority{{
		EvidenceID:        "authority:sekaipedia:list-of-songs:335193",
		Disposition:       AuthorityActive,
		Role:              AuthorityRoleSongIndex,
		CaptureProfile:    CaptureProfileMediaWikiAPIRevisionResponseV1,
		PageID:            268,
		RevisionID:        335193,
		RevisionTimestamp: "2026-07-27T16:29:13Z",
		SHA1:              "b216a827f88c59f5e954a120027832fe9cd74413",
		RawSHA256:         "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
		Title:             "List of songs",
		CanonicalURL:      "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193",
	}}
	providers.Configurations[1].Authorities = []FixedAuthority{{
		EvidenceID:     "search:moegirl:488279",
		Disposition:    AuthorityActive,
		Role:           AuthorityRoleSongIndex,
		CaptureProfile: CaptureProfileMediaWikiRevisionContentV1,
		PageID:         488279,
		RevisionID:     8073049,
		SHA1:           "d15e3eae65f3516d9b93b7644315574648379a3b",
		Title:          "世界计划 彩色舞台 feat. 初音未来/歌曲",
		CanonicalURL:   "https://moegirl.icu/index.php?oldid=8073049&title=%E4%B8%96%E7%95%8C%E8%AE%A1%E5%88%92+%E5%BD%A9%E8%89%B2%E8%88%9E%E5%8F%B0+feat.+%E5%88%9D%E9%9F%B3%E6%9C%AA%E6%9D%A5%2F%E6%AD%8C%E6%9B%B2",
	}}
	return providers
}

func futureProviderTestPlanData(t *testing.T) ProviderConfiguration {
	t.Helper()
	providers := CompiledProviderConfiguration()
	providers.Configurations[0].Authorities = []FixedAuthority{
		newFutureAuthority(t, ProviderSekaipedia, 9001, 9002, "Future song index", strings.Repeat("a", 40), strings.Repeat("b", 64)),
	}
	providers.Configurations[1].Authorities = []FixedAuthority{
		newFutureAuthority(t, ProviderMoegirl, 9003, 9004, "未来歌曲索引", strings.Repeat("c", 40), ""),
	}
	return providers
}

func newFutureAuthority(
	t *testing.T,
	provider Provider,
	pageID, revisionID int,
	title, sha1, rawSHA256 string,
) FixedAuthority {
	t.Helper()
	authority := FixedAuthority{
		Disposition: AuthorityActive, Role: AuthorityRoleSongIndex,
		PageID: pageID, RevisionID: revisionID, SHA1: sha1, RawSHA256: rawSHA256, Title: title,
		CanonicalURL: FixedAuthorityCanonicalURL(provider, title, revisionID),
	}
	switch provider {
	case ProviderSekaipedia:
		authority.CaptureProfile = CaptureProfileMediaWikiAPIRevisionResponseV1
		authority.RevisionTimestamp = "2026-07-31T23:59:59Z"
	case ProviderMoegirl:
		authority.CaptureProfile = CaptureProfileMediaWikiRevisionContentV1
	default:
		t.Fatalf("unsupported future authority provider %q", provider)
	}
	evidenceID, err := FixedAuthorityEvidenceID(provider, authority.Role, pageID, revisionID, title)
	if err != nil {
		t.Fatal(err)
	}
	authority.EvidenceID = evidenceID
	return authority
}

func cloneProviderConfiguration(input ProviderConfiguration) ProviderConfiguration {
	result := input
	result.Order = append([]Provider(nil), input.Order...)
	result.Configurations = append([]ProviderPlan(nil), input.Configurations...)
	for index := range result.Configurations {
		if input.Configurations[index].Authorities != nil {
			result.Configurations[index].Authorities = append([]FixedAuthority{}, input.Configurations[index].Authorities...)
		}
	}
	return result
}

func mustCanonicalPlan(t *testing.T, plan Plan) []byte {
	t.Helper()
	body, err := MarshalCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func writeSyntheticFile(t *testing.T, root, relativePath string, body []byte) SourceFileIdentity {
	t.Helper()
	absolutePath := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolutePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return SourceFileIdentity{Path: relativePath, SizeBytes: int64(len(body)), SHA256: sha256Hex(body)}
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
