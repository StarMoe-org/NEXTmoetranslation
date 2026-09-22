package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsextractionplan"
	"moesekai/server/internal/lyricsimportreceipt"
	"moesekai/server/internal/lyricsrecovery"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func TestCompiledProviderSafetyContractRetainsMandatoryRules(t *testing.T) {
	if err := validateCompiledProviderSafetyContract(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseProviderScopesRequire697SekaipediaAndExactPublic795(t *testing.T) {
	musicIDs := make([]int, 0, releaseCatalogTargetCount)
	sekaipediaIDs := make([]int, 0, releaseCatalogTargetCount-1)
	targets := make([]lyricsextractionplan.RecoverySekaipediaPageTarget, 0, releaseCatalogTargetCount-1)
	for musicID := 1; musicID <= releaseCatalogTargetCount-1; musicID++ {
		musicIDs = append(musicIDs, musicID)
		sekaipediaIDs = append(sekaipediaIDs, musicID)
		targets = append(targets, lyricsextractionplan.RecoverySekaipediaPageTarget{
			MusicID: musicID, PageTitle: "Reviewed_page",
		})
	}
	musicIDs = append(musicIDs, 795)
	plan := lyricsextractionplan.RecoveryPlan{
		Scope: lyricsextractionplan.RecoveryScopeBinding{MusicIDs: musicIDs},
		Providers: lyricsextractionplan.RecoveryProviderConfiguration{
			Order: []lyricsextractionplan.Provider{
				lyricsextractionplan.ProviderSekaipedia,
				lyricsextractionplan.ProviderMoegirlPublicExact,
			},
			Configurations: []lyricsextractionplan.RecoveryProviderPlan{
				{
					Provider: lyricsextractionplan.ProviderSekaipedia,
					MusicIDs: sekaipediaIDs, SekaipediaTargets: targets,
				},
				{
					Provider: lyricsextractionplan.ProviderMoegirlPublicExact,
					MusicIDs: []int{795}, Authorities: []lyricsextractionplan.FixedAuthority{},
					ContributorAliases: []lyricsextractionplan.RecoveryContributorAlias{},
					ExactPublicTargets: []lyricsextractionplan.RecoveryExactPublicPageTarget{{
						MusicID: 795, PageURL: releaseExactMoegirlURL, PageTitle: "亿年爱恋",
						JapaneseTitle: "一億年恋してる", PageID: 649688, RevisionID: 8500224,
						FetchedAt: "2026-08-03T14:58:50.501307Z",
						RawHTML: lyricsextractionplan.RecoveryFileBinding{
							Path: releaseExactMoegirlRawPath, SizeBytes: 128236, SHA256: releaseExactMoegirlRawSHA256,
						},
						ExtractionReport: lyricsextractionplan.RecoveryFileBinding{
							Path: releaseExactMoegirlReportPath, SizeBytes: 6344, SHA256: releaseExactMoegirlReportSHA256,
						},
					}},
				},
			},
		},
	}
	if err := validateReleaseProviderScopes(plan); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*lyricsextractionplan.RecoveryPlan){
		"ICU URL": func(candidate *lyricsextractionplan.RecoveryPlan) {
			candidate.Providers.Configurations[1].ExactPublicTargets[0].PageURL = "https://moegirl.icu/api.php"
		},
		"API authority": func(candidate *lyricsextractionplan.RecoveryPlan) {
			candidate.Providers.Configurations[1].Authorities = []lyricsextractionplan.FixedAuthority{{PageID: 1}}
		},
		"795 in Sekaipedia": func(candidate *lyricsextractionplan.RecoveryPlan) {
			candidate.Providers.Configurations[0].MusicIDs = append(candidate.Providers.Configurations[0].MusicIDs, 795)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := plan
			candidate.Providers.Configurations = append(
				[]lyricsextractionplan.RecoveryProviderPlan(nil), plan.Providers.Configurations...,
			)
			candidate.Providers.Configurations[0].MusicIDs = append([]int(nil), plan.Providers.Configurations[0].MusicIDs...)
			candidate.Providers.Configurations[1].Authorities = append([]lyricsextractionplan.FixedAuthority(nil), plan.Providers.Configurations[1].Authorities...)
			candidate.Providers.Configurations[1].ExactPublicTargets = append(
				[]lyricsextractionplan.RecoveryExactPublicPageTarget(nil), plan.Providers.Configurations[1].ExactPublicTargets...,
			)
			mutate(&candidate)
			if err := validateReleaseProviderScopes(candidate); err == nil {
				t.Fatal("invalid release provider scope was accepted")
			}
		})
	}
}

func TestRunRequiresClosedSubcommandAndHelpIsReadOnly(t *testing.T) {
	if err := run(context.Background(), nil, ioDiscard{}); err == nil {
		t.Fatal("missing subcommand was accepted")
	}
	var output bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "No command in this retired CLI performs provider acquisition") ||
		!strings.Contains(output.String(), "lyrics-recovery-acceptance-launcher") ||
		!strings.Contains(output.String(), "lyrics-recovery-public-candidate") {
		t.Fatalf("help omitted the retirement and read-only guarantees: %s", output.String())
	}
	for _, command := range []string{"validate-fresh", "check-backup", "check-post-import", "check-deploy", "check-public"} {
		if err := run(context.Background(), []string{command}, ioDiscard{}); err == nil ||
			!strings.Contains(err.Error(), "retired historical 698-song Public v2 release gate") {
			t.Fatalf("historical command %q was not retired: %v", command, err)
		}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) { return len(data), nil }

func TestEncryptedBackupReceiptDigestAndEnvelopeChecks(t *testing.T) {
	bundle := validatedReleaseBundle{
		Validation: releaseValidationReceipt{ReceiptSHA256: strings.Repeat("f", 64)},
		Bindings: releaseBindings{
			Root:     lyricsrootmanifest.Manifest{RootSHA256: strings.Repeat("a", 64)},
			Manifest: lyricsstaging.Manifest{BatchSHA256: strings.Repeat("b", 64)},
		},
	}
	receipt := encryptedBackupReceipt{
		SchemaVersion: encryptedBackupReceiptSchemaVersion,
		Kind:          encryptedBackupReceiptKind, ValidationReceiptSHA256: bundle.Validation.ReceiptSHA256,
		RootSHA256:              bundle.Bindings.Root.RootSHA256,
		ImportBatchSHA256:       bundle.Bindings.Manifest.BatchSHA256,
		PlaintextDatabaseSHA256: strings.Repeat("c", 64), PlaintextDatabaseStateSHA256: strings.Repeat("d", 64),
		CiphertextSHA256: strings.Repeat("e", 64), CiphertextByteCount: 123,
		EncryptionFormat: "age-v1", CreatedAt: "2026-08-02T01:02:03Z", VerifiedAt: "2026-08-02T01:03:03Z",
		IntegrityCheck: "ok", RestoreCheck: "ok", Offline: true, OffsiteCopyCount: 1,
	}
	digest, err := encryptedBackupReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ReceiptSHA256 = digest
	if err := validateEncryptedBackupReceipt(receipt, bundle); err != nil {
		t.Fatalf("valid encrypted backup receipt: %v", err)
	}
	if err := validateCiphertextPrefix("age-v1", []byte("age-encryption.org/v1\nsynthetic")); err != nil {
		t.Fatal(err)
	}
	if err := validateCiphertextPrefix("age-v1", []byte("SQLite format 3\x00synthetic")); err == nil {
		t.Fatal("plaintext SQLite backup was accepted as ciphertext")
	}
	receipt.RestoreCheck = "skipped"
	if err := validateEncryptedBackupReceipt(receipt, bundle); err == nil {
		t.Fatal("unverified restore receipt was accepted")
	}
}

func TestDeploymentReceiptBindsReleaseAndImportReceipt(t *testing.T) {
	bundle := validatedReleaseBundle{
		Validation: releaseValidationReceipt{ReceiptSHA256: strings.Repeat("0", 64)},
		Bindings: releaseBindings{
			Root:     lyricsrootmanifest.Manifest{RootSHA256: strings.Repeat("1", 64)},
			Manifest: lyricsstaging.Manifest{BatchSHA256: strings.Repeat("2", 64)},
		},
	}
	importSHA := strings.Repeat("3", 64)
	receipt := deploymentReceipt{
		SchemaVersion: deploymentReceiptSchemaVersion, Kind: deploymentReceiptKind, Environment: "production",
		ValidationReceiptSHA256: bundle.Validation.ReceiptSHA256,
		RootSHA256:              bundle.Bindings.Root.RootSHA256, ImportBatchSHA256: bundle.Bindings.Manifest.BatchSHA256,
		ImportReceiptSHA256: importSHA, ArtifactDigest: "sha256:" + strings.Repeat("4", 64),
		BaseURL: "https://moesekai.example", DeployedAt: "2026-08-02T02:00:00Z", VerifiedAt: "2026-08-02T02:01:00Z",
	}
	digest, err := deploymentReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ReceiptSHA256 = digest
	if err := validateDeploymentReceipt(receipt, bundle, importSHA, receipt.BaseURL); err != nil {
		t.Fatalf("valid deployment receipt: %v", err)
	}
	receipt.ImportReceiptSHA256 = strings.Repeat("5", 64)
	if err := validateDeploymentReceipt(receipt, bundle, importSHA, receipt.BaseURL); err == nil {
		t.Fatal("deployment receipt with the wrong import receipt was accepted")
	}
}

func TestProbeAndPublicAssetUseGETAndExactBoundedResponses(t *testing.T) {
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method+" "+request.URL.Path)
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		if request.URL.Path == "/healthz" {
			response.Header().Set("Cache-Control", "no-store")
			_, _ = response.Write([]byte(`{"status":"ok"}`))
			return
		}
		_, _ = response.Write([]byte(`{"version":2,"songs":[]}`))
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	if err := checkProbe(context.Background(), client, base, "/healthz", []byte(`{"status":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := getPublicAsset(context.Background(), client, base, "/files/translation/lyrics/index.json"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(methods, []string{"GET /healthz", "GET /files/translation/lyrics/index.json"}) {
		t.Fatalf("network methods=%v", methods)
	}
}

func TestReadOnlySQLiteEnforcesIntegrityAndQueryOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "post-import.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE exact_release(id INTEGER PRIMARY KEY, value TEXT NOT NULL); INSERT INTO exact_release VALUES(1,'ok')`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	readonly, err := openReadOnlySQLite(context.Background(), path, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.close()
	if err := verifySQLiteIntegrity(context.Background(), readonly.db); err != nil {
		t.Fatal(err)
	}
	if _, err := readonly.db.Exec(`UPDATE exact_release SET value='changed' WHERE id=1`); err == nil {
		t.Fatal("query-only SQLite connection accepted a write")
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		t.Fatalf("read SQLite mutation fixture: %v", err)
	}
	body[len(body)-1] ^= 0xff
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readonly.verifyUnchanged(); err == nil {
		t.Fatal("same-size in-place SQLite mutation after pinned reads was accepted")
	}
}

func TestImportInputsRequireExact698FullGameAndEvidenceUnion(t *testing.T) {
	full := model.LyricsSourceFull{
		Version:    model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"},
		Performers: []model.LyricsSourcePerformer{},
		Lines: []model.LyricsSourceFullLine{{
			ID: "full-000001", Text: "歌う",
			Segments:             []model.LyricsSourceSegment{{Text: "歌う", PerformerIDs: []string{}, Ruby: []model.LyricsSourceRubySpan{{Text: "歌う"}}}},
			TrailingPerformerIDs: []string{},
		}},
	}
	evidenceRef := lyricscontract.EvidenceRef{
		Provider: model.LyricsSourceProviderSekaipedia, AcquisitionID: strings.Repeat("a", 64),
		EvidenceID: "revision:sekaipedia:1:2:" + strings.Repeat("b", 64), SHA256: strings.Repeat("b", 64),
		EnvelopeSHA256: strings.Repeat("c", 64),
	}
	evidenceRef2 := lyricscontract.EvidenceRef{
		Provider: model.LyricsSourceProviderMoegirl, AcquisitionID: strings.Repeat("d", 64),
		EvidenceID: "revision:moegirl:3:4:" + strings.Repeat("e", 64), SHA256: strings.Repeat("e", 64),
		EnvelopeSHA256: strings.Repeat("f", 64),
	}
	manifest := lyricsstaging.Manifest{
		Preflight:        lyricsstaging.PreflightReference{CatalogCount: releaseCatalogTargetCount, UniqueCompleteCount: releaseCatalogTargetCount},
		CatalogReference: make([]lyricsstaging.CatalogReference, releaseCatalogTargetCount),
		Items:            make([]lyricsstaging.Draft, releaseCatalogTargetCount),
	}
	root := lyricsrootmanifest.Manifest{
		Songs: make([]lyricsrootmanifest.SongResultRef, releaseCatalogTargetCount),
		Coverage: lyricscontract.Coverage{
			UniqueEvidenceCount: 2, UniqueAcquisitionCount: 2,
		},
	}
	results := make(map[int]lyricsrecovery.SongResult, releaseCatalogTargetCount)
	for index := 0; index < releaseCatalogTargetCount; index++ {
		musicID := index + 1
		document := model.LyricsSourceDocument{
			ReasonCode: model.LyricsSourceVersionReasonUntaggedFullOnly,
			FixedIdentities: []model.LyricsSourceFixedIdentity{{
				Provider:          model.LyricsSourceProviderSekaipedia,
				IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{EvidenceID: evidenceRef.EvidenceID, SHA256: evidenceRef.SHA256}},
			}},
			Full: full,
		}
		manifest.Items[index] = lyricsstaging.Draft{MusicID: musicID, Document: document}
		root.Songs[index] = lyricsrootmanifest.SongResultRef{MusicID: musicID, SelectedEvidence: []lyricscontract.EvidenceRef{evidenceRef}}
		copyFull := full
		results[musicID] = lyricsrecovery.SongResult{MusicID: musicID, ReasonCode: document.ReasonCode, Full: &copyFull}
	}
	manifest.Items[releaseCatalogTargetCount-1].Artifacts = []lyricsstaging.Artifact{{
		Identity: model.LyricsSourceFixedIdentity{
			Provider: model.LyricsSourceProviderMoegirl,
			IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
				EvidenceID: evidenceRef2.EvidenceID, SHA256: evidenceRef2.SHA256,
			}},
		},
	}}
	root.Songs[releaseCatalogTargetCount-1].SelectedEvidence = append(
		root.Songs[releaseCatalogTargetCount-1].SelectedEvidence, evidenceRef2,
	)
	receipt := lyricsstaging.PrivateEvidenceReceipt{IndexEvidence: []lyricssource.IndexEvidence{
		{
			Provider: model.LyricsSourceProviderSekaipedia, EvidenceID: evidenceRef.EvidenceID,
			SHA256: evidenceRef.SHA256, RawSHA256: evidenceRef.SHA256,
		},
		{
			Provider: model.LyricsSourceProviderMoegirl, EvidenceID: evidenceRef2.EvidenceID,
			SHA256: evidenceRef2.SHA256, RawSHA256: evidenceRef2.SHA256,
		},
	}}
	if err := validateImportInputsAgainstFreshRoot(manifest, receipt, root, results); err != nil {
		t.Fatalf("exact 698 import binding: %v", err)
	}
	orphan := receipt
	orphan.IndexEvidence = append([]lyricssource.IndexEvidence{}, receipt.IndexEvidence...)
	extra := receipt.IndexEvidence[0]
	extra.EvidenceID = "revision:sekaipedia:9:9:" + strings.Repeat("d", 64)
	extra.SHA256 = strings.Repeat("d", 64)
	extra.RawSHA256 = extra.SHA256
	orphan.IndexEvidence = append(orphan.IndexEvidence, extra)
	if err := validateImportInputsAgainstFreshRoot(manifest, orphan, root, results); err == nil {
		t.Fatal("unrelated import evidence receipt row was accepted")
	}
	missing := receipt
	missing.IndexEvidence = []lyricssource.IndexEvidence{}
	if err := validateImportInputsAgainstFreshRoot(manifest, missing, root, results); err == nil {
		t.Fatal("incomplete import evidence receipt union was accepted")
	}
	duplicate := receipt
	duplicate.IndexEvidence = []lyricssource.IndexEvidence{receipt.IndexEvidence[0], receipt.IndexEvidence[0]}
	if err := validateImportInputsAgainstFreshRoot(manifest, duplicate, root, results); err == nil {
		t.Fatal("duplicate evidence row masking a missing root identity was accepted")
	}
	conflictingRoot := root
	conflictingRoot.Songs = append([]lyricsrootmanifest.SongResultRef{}, root.Songs...)
	conflicting := evidenceRef
	conflicting.AcquisitionID = strings.Repeat("e", 64)
	conflictingRoot.Songs[1].SelectedEvidence = []lyricscontract.EvidenceRef{conflicting}
	if _, err := orderedEvidenceUnion(conflictingRoot); err == nil {
		t.Fatal("conflicting root evidence identity was accepted")
	}
	manifest.Items[releaseCatalogTargetCount-1].Document.ReasonCode = model.LyricsSourceVersionReasonTaggedFullAndGame
	if err := validateImportInputsAgainstFreshRoot(manifest, receipt, root, results); err == nil {
		t.Fatal("Full/Game authority drift in one of 698 items was accepted")
	}
}

func TestPublicDetailRequiresAuthoritativeFullGameAndAttributions(t *testing.T) {
	document := testPublicDocument()
	draft := lyricsstaging.Draft{MusicID: 7, JapaneseTitle: "試験曲", Document: document}
	index := store.PublicLyricsIndexSong{
		MusicID: 7, Revision: 3, UpdatedAt: "2026-08-02T03:00:00Z",
		AvailableVersions: []string{"full", "game"},
	}
	detail := store.PublicLyricsDetailDocument{
		Version: 2, MusicID: 7, Revision: 3, UpdatedAt: index.UpdatedAt,
		Attributions: publicAttributions(document), AvailableVersions: []string{"full", "game"},
		Lines: []store.PublicLyricsLine{{
			ID: "full-000001", Order: 0, Japanese: "初音歌う", Chinese: "", English: "",
			Segments: []model.LyricSegment{{
				Text: "初音歌う", PerformerIDs: []int{},
				Ruby: []model.LyricRubySpan{{Text: "初音", Reading: "はつね"}, {Text: "歌う"}},
			}},
		}},
		GameProjection: &store.PublicLyricsGameProjection{
			ReasonCode: model.LyricsSourceVersionReasonUntaggedUncutIdentity,
			LineIDs:    []string{"full-000001"},
		},
	}
	vocals := []model.CatalogVocalSignal{{VocalType: "sekai"}}
	if err := validatePublicDetail(detail, index, draft, vocals); err != nil {
		t.Fatalf("valid public detail: %v", err)
	}
	detail.Lines[0].Segments[0].Ruby[0].Reading = "ミク"
	if err := validatePublicDetail(detail, index, draft, vocals); err == nil {
		t.Fatal("public ruby drift was accepted")
	}
}

func testPublicDocument() model.LyricsSourceDocument {
	identity := model.LyricsSourceFixedIdentity{
		Provider: model.LyricsSourceProviderSekaipedia, Title: "試験曲", RevisionID: 2,
		CanonicalURL: "https://www.sekaipedia.org/wiki/Test?oldid=2", RenditionKey: "full-sekai",
	}
	component := model.LyricsSourceComponentRef{RenditionKey: identity.RenditionKey}
	return model.LyricsSourceDocument{
		ReasonCode:      model.LyricsSourceVersionReasonUntaggedUncutIdentity,
		FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
		Provenance: model.LyricsSourceComponentProvenance{
			FullText: component, PerformerSegmentation: &component, GameProjection: &component,
			Ruby: &component, VersionEvidence: component,
		},
		Full: model.LyricsSourceFull{
			Version:              model.LyricsSourceVersion{Kind: "sekai", Label: "Project SEKAI Version"},
			Performers:           []model.LyricsSourcePerformer{},
			RubyGeneratorVersion: "kagome-ipadic-v1",
			Lines: []model.LyricsSourceFullLine{{
				ID: "full-000001", Text: "初音歌う",
				Segments: []model.LyricsSourceSegment{{
					Text: "初音歌う", PerformerIDs: []string{},
					Ruby: []model.LyricsSourceRubySpan{{Text: "初音", Reading: "はつね"}, {Text: "歌う"}},
				}},
				TrailingPerformerIDs: []string{},
			}},
		},
		GameProjection: &model.LyricsSourceGameProjection{LineIDs: []string{"full-000001"}},
	}
}

func TestStrictJSONRejectsDuplicatesAndPublicRootsAreClosed(t *testing.T) {
	var target struct {
		Value int `json:"value"`
	}
	if err := decodeStrictJSON([]byte(`{"value":1,"value":2}`), &target, "duplicate fixture"); err == nil {
		t.Fatal("duplicate JSON keys were accepted")
	}
	for _, value := range []string{
		"/files/translation/lyrics", "/files/v2/ja-JP/translation/lyrics",
		"/files/v2/zh-CN/translation/lyrics", "/files/v2/en-US/translation/lyrics",
	} {
		if !validPublicRoot(value) {
			t.Fatalf("closed public root rejected: %s", value)
		}
	}
	if validPublicRoot("/files/v2/other/translation/lyrics") {
		t.Fatal("unknown public locale root was accepted")
	}
}

func TestReceiptJSONContractsRemainClosed(t *testing.T) {
	receipt := encryptedBackupReceipt{}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mutated := append(body[:len(body)-1], []byte(`,"lyrics":"forbidden"}`)...)
	if err := decodeStrictJSON(mutated, &receipt, "backup receipt"); err == nil {
		t.Fatal("unknown content field was accepted in backup receipt")
	}
}

func TestPinnedReadRejectsSameSizeInPlaceMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pinned.json")
	if err := os.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	priorHook := testHookBeforePinnedRehash
	testHookBeforePinnedRehash = func(candidate, _ string) error {
		if candidate == path {
			return os.WriteFile(path, []byte("bbbb"), 0o600)
		}
		return nil
	}
	t.Cleanup(func() { testHookBeforePinnedRehash = priorHook })
	if _, _, err := readPinnedRegular(path, "same-size mutation fixture", 16, 0o600); err == nil {
		t.Fatal("same-size in-place mutation after the pinned read was accepted")
	}
}

func TestValidationReceiptIsTamperEvidentAndNoOverwrite(t *testing.T) {
	t.Run("tamper", func(t *testing.T) {
		receipt := validValidationReceiptFixture(t)
		body, err := marshalReleaseValidationReceipt(receipt)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.Replace(body, []byte(`"acquisitionCount": 1`), []byte(`"acquisitionCount": 2`), 1)
		if err := os.WriteFile(receipt.ReceiptPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadReleaseValidationReceipt(receipt.ReceiptPath); err == nil {
			t.Fatal("tampered validation receipt was accepted")
		}
	})

	t.Run("replay overwrite", func(t *testing.T) {
		receipt := validValidationReceiptFixture(t)
		if err := publishReleaseValidationReceipt(receipt); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(receipt.ReceiptPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := publishReleaseValidationReceipt(receipt); err == nil {
			t.Fatal("validation receipt replay overwrote a historical receipt")
		}
		after, err := os.ReadFile(receipt.ReceiptPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("failed validation receipt replay changed the historical receipt")
		}
	})
}

func TestDownstreamReceiptsRejectCrossBundleMixing(t *testing.T) {
	bundleA := validatedReleaseBundle{
		Validation: releaseValidationReceipt{ReceiptSHA256: strings.Repeat("a", 64)},
		Bindings: releaseBindings{
			Root:     lyricsrootmanifest.Manifest{RootSHA256: strings.Repeat("b", 64)},
			Manifest: lyricsstaging.Manifest{BatchSHA256: strings.Repeat("c", 64)},
		},
	}
	bundleB := bundleA
	bundleB.Validation.ReceiptSHA256 = strings.Repeat("d", 64)
	backup := encryptedBackupReceipt{
		SchemaVersion: encryptedBackupReceiptSchemaVersion, Kind: encryptedBackupReceiptKind,
		ValidationReceiptSHA256: bundleA.Validation.ReceiptSHA256,
		RootSHA256:              bundleA.Bindings.Root.RootSHA256, ImportBatchSHA256: bundleA.Bindings.Manifest.BatchSHA256,
		PlaintextDatabaseSHA256: strings.Repeat("e", 64), PlaintextDatabaseStateSHA256: strings.Repeat("f", 64),
		CiphertextSHA256: strings.Repeat("1", 64), CiphertextByteCount: 64, EncryptionFormat: "age-v1",
		CreatedAt: "2026-08-02T04:00:00Z", VerifiedAt: "2026-08-02T04:01:00Z",
		IntegrityCheck: "ok", RestoreCheck: "ok", Offline: true, OffsiteCopyCount: 1,
	}
	backup.ReceiptSHA256, _ = encryptedBackupReceiptDigest(backup)
	if err := validateEncryptedBackupReceipt(backup, bundleA); err != nil {
		t.Fatalf("bundle A backup: %v", err)
	}
	if err := validateEncryptedBackupReceipt(backup, bundleB); err == nil {
		t.Fatal("bundle A backup receipt was accepted for bundle B")
	}

	deployment := deploymentReceipt{
		SchemaVersion: deploymentReceiptSchemaVersion, Kind: deploymentReceiptKind, Environment: "production",
		ValidationReceiptSHA256: bundleA.Validation.ReceiptSHA256,
		RootSHA256:              bundleA.Bindings.Root.RootSHA256, ImportBatchSHA256: bundleA.Bindings.Manifest.BatchSHA256,
		ImportReceiptSHA256: strings.Repeat("2", 64), ArtifactDigest: "sha256:" + strings.Repeat("3", 64),
		BaseURL: "https://moesekai.example", DeployedAt: "2026-08-02T05:00:00Z", VerifiedAt: "2026-08-02T05:01:00Z",
	}
	deployment.ReceiptSHA256, _ = deploymentReceiptDigest(deployment)
	if err := validateDeploymentReceipt(deployment, bundleA, deployment.ImportReceiptSHA256, deployment.BaseURL); err != nil {
		t.Fatalf("bundle A deployment: %v", err)
	}
	if err := validateDeploymentReceipt(deployment, bundleB, deployment.ImportReceiptSHA256, deployment.BaseURL); err == nil {
		t.Fatal("bundle A deployment receipt was accepted for bundle B")
	}

	importBundle, importReceipt := validBoundImportReceiptFixture()
	if err := validateReleaseImportReceiptBundleBinding(importReceipt, importReceipt.ReceiptPath, importBundle); err != nil {
		t.Fatalf("bundle A import receipt: %v", err)
	}
	mixedImportBundle := importBundle
	mixedImportBundle.Bindings.ManifestFileSHA256 = strings.Repeat("6", 64)
	if err := validateReleaseImportReceiptBundleBinding(importReceipt, importReceipt.ReceiptPath, mixedImportBundle); err == nil {
		t.Fatal("bundle A import receipt was accepted with bundle B manifest bytes")
	}
	mixedImportBundle = importBundle
	mixedImportBundle.Validation.ImportEvidence.ReceiptSHA256 = strings.Repeat("7", 64)
	if err := validateReleaseImportReceiptBundleBinding(importReceipt, importReceipt.ReceiptPath, mixedImportBundle); err == nil {
		t.Fatal("bundle A import receipt was accepted with bundle B evidence receipt")
	}
}

func TestOperationalProducerReceiptBytesAreAcceptedOnlyForExactReleaseBundle(t *testing.T) {
	bundleA, template := validBoundImportReceiptFixture()
	produce := func(t *testing.T, path string, bundle validatedReleaseBundle) (releaseImportReceipt, []byte) {
		t.Helper()
		binding := importReceiptBindingForBundle(path, bundle)
		receipt, err := lyricsimportreceipt.New(binding, lyricsimportreceipt.Metadata{
			BackupSHA256: template.BackupSHA256, BackupStateSHA256: template.BackupStateSHA256,
			PreImportDatabaseSHA256:      template.PreImportDatabaseSHA256,
			PreImportDatabaseStateSHA256: template.PreImportDatabaseStateSHA256,
			DatabasePath:                 template.DatabasePath, RecoveryDatabasePath: template.RecoveryDatabasePath,
			Operator: template.Operator, Items: template.Items, PreparedAt: template.PreparedAt,
		})
		if err != nil {
			t.Fatalf("operational producer constructor: %v", err)
		}
		body, err := lyricsimportreceipt.MarshalBound(receipt, binding)
		if err != nil {
			t.Fatalf("operational producer encoder: %v", err)
		}
		return receipt, body
	}
	writeReceipt := func(t *testing.T, path string, body []byte) {
		t.Helper()
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	directory := t.TempDir()
	receiptPath := filepath.Join(directory, "bundle-a-import-receipt.json")
	receiptA, bodyA := produce(t, receiptPath, bundleA)
	writeReceipt(t, receiptPath, bodyA)
	if loaded, loadedBody, _, err := loadBoundReleaseImportReceipt(receiptPath, bundleA); err != nil {
		t.Fatalf("real release loader rejected operational bundle A bytes: %v", err)
	} else if !reflect.DeepEqual(loaded, receiptA) || !bytes.Equal(loadedBody, bodyA) {
		t.Fatal("real release loader changed operational bundle A receipt bytes")
	}

	t.Run("missing binding", func(t *testing.T) {
		path := filepath.Join(directory, "missing-binding.json")
		_, body := produce(t, path, bundleA)
		body = bytes.Replace(body, []byte("  \"validationReceiptSha256\": \""+bundleA.Validation.ReceiptSHA256+"\",\n"), nil, 1)
		writeReceipt(t, path, body)
		if _, _, _, err := loadBoundReleaseImportReceipt(path, bundleA); err == nil {
			t.Fatal("real release loader accepted a missing producer binding")
		}
	})

	t.Run("unknown binding", func(t *testing.T) {
		path := filepath.Join(directory, "unknown-binding.json")
		_, body := produce(t, path, bundleA)
		body = bytes.Replace(body, []byte(`"schemaVersion": 5,`), []byte("\"schemaVersion\": 5,\n  \"unknownBinding\": \"forbidden\","), 1)
		writeReceipt(t, path, body)
		if _, _, _, err := loadBoundReleaseImportReceipt(path, bundleA); err == nil {
			t.Fatal("real release loader accepted an unknown producer binding")
		}
	})

	t.Run("mismatched manifest input", func(t *testing.T) {
		path := filepath.Join(directory, "mismatched-manifest.json")
		receipt, _ := produce(t, path, bundleA)
		receipt.ManifestSHA256 = strings.Repeat("6", 64)
		body, err := lyricsimportreceipt.MarshalCanonical(receipt)
		if err != nil {
			t.Fatal(err)
		}
		writeReceipt(t, path, body)
		if _, _, _, err := loadBoundReleaseImportReceipt(path, bundleA); err == nil {
			t.Fatal("real release loader accepted a mismatched producer manifest binding")
		}
	})

	t.Run("same manifest different validation bundle B", func(t *testing.T) {
		bundleB := bundleA
		bundleB.Validation.ReceiptSHA256 = strings.Repeat("b", 64)
		if _, _, _, err := loadBoundReleaseImportReceipt(receiptPath, bundleB); err == nil {
			t.Fatal("bundle A receipt was authorized for bundle B with the same manifest and a different validation receipt")
		}
	})

	t.Run("same evidence different compact root bundle B", func(t *testing.T) {
		bundleB := bundleA
		bundleB.Validation.ReceiptSHA256 = strings.Repeat("b", 64)
		bundleB.Validation.RootManifest = validationRootBinding{
			File: validationFileBinding{SHA256: strings.Repeat("c", 64)}, RootID: "root-bundle-b",
			RootSHA256: strings.Repeat("d", 64),
		}
		bundleB.Bindings.RootFileSHA256 = bundleB.Validation.RootManifest.File.SHA256
		bundleB.Bindings.Root.RootID = bundleB.Validation.RootManifest.RootID
		bundleB.Bindings.Root.RootSHA256 = bundleB.Validation.RootManifest.RootSHA256
		if _, _, _, err := loadBoundReleaseImportReceipt(receiptPath, bundleB); err == nil {
			t.Fatal("bundle A receipt was authorized for bundle B with the same evidence and a different compact root")
		}
	})

	t.Run("crossed bundle", func(t *testing.T) {
		bundleB := bundleA
		bundleB.Validation.ReceiptSHA256 = strings.Repeat("b", 64)
		bundleB.Validation.RootManifest = validationRootBinding{
			File: validationFileBinding{SHA256: strings.Repeat("c", 64)}, RootID: "root-bundle-b",
			RootSHA256: strings.Repeat("d", 64),
		}
		bundleB.Validation.ImportEvidence.ReceiptSHA256 = strings.Repeat("e", 64)
		bundleB.Bindings.RootFileSHA256 = bundleB.Validation.RootManifest.File.SHA256
		bundleB.Bindings.Root.RootID = bundleB.Validation.RootManifest.RootID
		bundleB.Bindings.Root.RootSHA256 = bundleB.Validation.RootManifest.RootSHA256
		bundleB.Bindings.ManifestFileSHA256 = strings.Repeat("f", 64)
		bundleB.Bindings.Manifest.BatchSHA256 = strings.Repeat("0", 64)
		if _, _, _, err := loadBoundReleaseImportReceipt(receiptPath, bundleB); err == nil {
			t.Fatal("bundle A operational receipt was accepted for a fully crossed bundle B")
		}
	})
}

func TestImportReceiptRejectsMissingUnknownAndMismatchedReleaseBindings(t *testing.T) {
	bundle, valid := validBoundImportReceiptFixture()
	mutations := map[string]func(*releaseImportReceipt){
		"missing validation receipt":   func(receipt *releaseImportReceipt) { receipt.ValidationReceiptSHA256 = "" },
		"wrong validation receipt":     func(receipt *releaseImportReceipt) { receipt.ValidationReceiptSHA256 = strings.Repeat("b", 64) },
		"missing root manifest digest": func(receipt *releaseImportReceipt) { receipt.RootManifestSHA256 = "" },
		"wrong root manifest digest":   func(receipt *releaseImportReceipt) { receipt.RootManifestSHA256 = strings.Repeat("c", 64) },
		"missing root identity":        func(receipt *releaseImportReceipt) { receipt.RootID = "" },
		"wrong root identity":          func(receipt *releaseImportReceipt) { receipt.RootID = "root-bundle-b" },
		"missing root digest":          func(receipt *releaseImportReceipt) { receipt.RootSHA256 = "" },
		"wrong root digest":            func(receipt *releaseImportReceipt) { receipt.RootSHA256 = strings.Repeat("d", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			receipt := valid
			mutate(&receipt)
			if err := validateReleaseImportReceiptBundleBinding(receipt, receipt.ReceiptPath, bundle); err == nil {
				t.Fatal("invalid import receipt release binding was accepted")
			}
		})
	}

	body, err := marshalReleaseImportReceipt(valid)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("unknown field", func(t *testing.T) {
		mutated := bytes.Replace(body, []byte(`"schemaVersion": 5,`), []byte("\"schemaVersion\": 5,\n  \"unknownBinding\": \"forbidden\","), 1)
		var decoded releaseImportReceipt
		if err := decodeStrictJSON(mutated, &decoded, "durable import receipt"); err == nil {
			t.Fatal("unknown import receipt binding was accepted")
		}
	})
	t.Run("content field", func(t *testing.T) {
		mutated := bytes.Replace(body, []byte(`"schemaVersion": 5,`), []byte("\"schemaVersion\": 5,\n  \"lyrics\": \"forbidden\","), 1)
		if err := rejectJSONKeys(mutated, importReceiptForbiddenFields, "durable import receipt"); err == nil {
			t.Fatal("content-bearing import receipt field was accepted")
		}
	})
	t.Run("missing field in JSON", func(t *testing.T) {
		missing := bytes.Replace(body, []byte("  \"validationReceiptSha256\": \""+valid.ValidationReceiptSHA256+"\",\n"), nil, 1)
		var decoded releaseImportReceipt
		if err := decodeStrictJSON(missing, &decoded, "durable import receipt"); err != nil {
			t.Fatalf("decode structurally valid missing-field receipt: %v", err)
		}
		if err := validateReleaseImportReceiptBundleBinding(decoded, decoded.ReceiptPath, bundle); err == nil {
			t.Fatal("import receipt missing validationReceiptSha256 was accepted")
		}
	})
	t.Run("excessive depth", func(t *testing.T) {
		deep := []byte(`{"schemaVersion":5,"unknown":` + strings.Repeat("[", lyricsimportreceipt.MaxJSONDepth+1) + `0` + strings.Repeat("]", lyricsimportreceipt.MaxJSONDepth+1) + `}`)
		if _, err := lyricsimportreceipt.DecodeCanonical(deep); err == nil {
			t.Fatal("excessively deep import receipt JSON was accepted")
		}
	})
	t.Run("invalid UTF-8", func(t *testing.T) {
		invalid := append([]byte(nil), body...)
		invalid[len(invalid)/2] = 0xff
		if _, err := lyricsimportreceipt.DecodeCanonical(invalid); err == nil {
			t.Fatal("non-UTF-8 import receipt JSON was accepted")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		if _, err := lyricsimportreceipt.DecodeCanonical(bytes.Repeat([]byte{'x'}, lyricsimportreceipt.MaxReceiptBytes+1)); err == nil {
			t.Fatal("oversized import receipt JSON was accepted")
		}
	})
	t.Run("noncanonical producer encoding", func(t *testing.T) {
		receipt := valid
		receipt.ReceiptPath = filepath.Join(t.TempDir(), "noncanonical-import-receipt.json")
		canonical, err := marshalBoundReleaseImportReceipt(receipt, receipt.ReceiptPath, bundle)
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, canonical); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receipt.ReceiptPath, compact.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := loadBoundReleaseImportReceipt(receipt.ReceiptPath, bundle); err == nil {
			t.Fatal("noncanonical durable import receipt was accepted")
		}
	})
}

func TestPublicDetailRequiresExactMappedPerformerValues(t *testing.T) {
	document := testPublicDocument()
	document.Full.Performers = []model.LyricsSourcePerformer{
		{PerformerID: "歌唱者-21", Name: "初音ミク"},
		{PerformerID: "歌唱者-22", Name: "鏡音リン"},
	}
	document.Full.Lines[0].Segments[0].PerformerIDs = []string{"歌唱者-22", "歌唱者-21"}
	document.Full.Lines[0].TrailingPerformerIDs = []string{"歌唱者-22", "歌唱者-21"}
	draft := lyricsstaging.Draft{MusicID: 7, JapaneseTitle: "試験曲", Document: document}
	index := store.PublicLyricsIndexSong{
		MusicID: 7, Revision: 3, UpdatedAt: "2026-08-02T06:00:00Z",
		AvailableVersions: []string{"full", "game"},
	}
	detail := store.PublicLyricsDetailDocument{
		Version: 2, MusicID: 7, Revision: 3, UpdatedAt: index.UpdatedAt,
		Attributions: publicAttributions(document), AvailableVersions: index.AvailableVersions,
		Lines: []store.PublicLyricsLine{{
			ID: "full-000001", Order: 0, Japanese: "初音歌う",
			Segments: []model.LyricSegment{{
				Text: "初音歌う", PerformerIDs: []int{22, 21},
				Ruby: []model.LyricRubySpan{{Text: "初音", Reading: "はつね"}, {Text: "歌う"}},
			}},
		}},
		GameProjection: &store.PublicLyricsGameProjection{
			ReasonCode: document.ReasonCode, LineIDs: []string{"full-000001"},
		},
	}
	vocals := []model.CatalogVocalSignal{{VocalType: "sekai"}}
	if err := validatePublicDetail(detail, index, draft, vocals); err != nil {
		t.Fatalf("exact mapped performer order: %v", err)
	}
	detail.Lines[0].Segments[0].PerformerIDs = []int{21, 22}
	if err := validatePublicDetail(detail, index, draft, vocals); err == nil {
		t.Fatal("same-length Public performer substitution/order change was accepted")
	}
}

func TestPublicPerformerAuthorityHandlesVocaloidAndEnglishOriginalWithoutWeakeningNoRomaji(t *testing.T) {
	t.Run("ordinary Vocaloid remains performer-free", func(t *testing.T) {
		document := testPublicDocument()
		document.Full.Version = model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"}
		document.Full.Performers = []model.LyricsSourcePerformer{}
		document.Full.Lines[0].Segments[0].PerformerIDs = []string{}
		document.Full.Lines[0].TrailingPerformerIDs = []string{}
		document.Provenance.PerformerSegmentation = nil
		expected, err := authoritativePublicPerformerIDs(document, nil)
		if err != nil || expected[0][0] == nil || len(expected[0][0]) != 0 {
			t.Fatalf("ordinary Vocaloid performer projection=%v err=%v", expected, err)
		}
	})

	t.Run("reviewed Vocaloid segmentation maps exact IDs", func(t *testing.T) {
		document := testPublicDocument()
		document.Full.Version = model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"}
		document.PrivateReview = &model.LyricsSourcePrivateReview{
			PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
		}
		document.Full.Performers = []model.LyricsSourcePerformer{{PerformerID: "歌唱者-21", Name: "初音ミク"}}
		document.Full.Lines[0].Segments[0].PerformerIDs = []string{"歌唱者-21"}
		expected, err := authoritativePublicPerformerIDs(document, nil)
		if err != nil || !reflect.DeepEqual(expected[0][0], []int{21}) {
			t.Fatalf("reviewed Vocaloid performer projection=%v err=%v", expected, err)
		}
	})

	t.Run("reviewed external performer uses reserved lyrics-only ID", func(t *testing.T) {
		document := testPublicDocument()
		document.Full.Version = model.LyricsSourceVersion{Kind: "vocaloid", Label: "VIRTUAL SINGER Version"}
		document.PrivateReview = &model.LyricsSourcePrivateReview{
			PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
		}
		document.Full.Performers = []model.LyricsSourcePerformer{{
			PerformerID: "外部歌唱者-01", Name: "GUMI", Color: "#70B85A",
		}}
		document.Full.Lines[0].Segments[0].PerformerIDs = []string{"外部歌唱者-01"}
		document.Full.Lines[0].TrailingPerformerIDs = []string{"外部歌唱者-01"}
		expected, err := authoritativePublicPerformerIDs(document, nil)
		if err != nil || !reflect.DeepEqual(expected[0][0], []int{1001}) {
			t.Fatalf("reviewed external performer projection=%v err=%v", expected, err)
		}
	})

	t.Run("English Full original", func(t *testing.T) {
		document := testPublicDocument()
		document.ReasonCode = model.LyricsSourceVersionReasonUntaggedFullOnly
		document.GameProjection = nil
		document.Provenance.GameProjection = nil
		document.Provenance.PerformerSegmentation = nil
		document.Full.Version = model.LyricsSourceVersion{Kind: "original", Label: "Original Version"}
		document.Full.Performers = []model.LyricsSourcePerformer{}
		const english = "I sing the complete original song"
		document.Full.Lines[0].Text = english
		document.Full.Lines[0].Segments = []model.LyricsSourceSegment{{
			Text: english, PerformerIDs: []string{}, Ruby: []model.LyricsSourceRubySpan{{Text: english}},
		}}
		document.Full.Lines[0].TrailingPerformerIDs = []string{}
		draft := lyricsstaging.Draft{MusicID: 7, JapaneseTitle: "English original", Document: document}
		index := store.PublicLyricsIndexSong{
			MusicID: 7, Revision: 4, UpdatedAt: "2026-08-02T07:00:00Z", AvailableVersions: []string{"full"},
		}
		detail := store.PublicLyricsDetailDocument{
			Version: 2, MusicID: 7, Revision: 4, UpdatedAt: index.UpdatedAt,
			Attributions: publicAttributions(document), AvailableVersions: []string{"full"},
			Lines: []store.PublicLyricsLine{{
				ID: "full-000001", Order: 0, Japanese: english,
				Segments: []model.LyricSegment{{
					Text: english, PerformerIDs: []int{}, Ruby: []model.LyricRubySpan{{Text: english}},
				}},
			}},
		}
		if err := validatePublicDetail(detail, index, draft, []model.CatalogVocalSignal{{VocalType: "sekai"}}); err != nil {
			t.Fatalf("legitimate English Full original was rejected: %v", err)
		}
	})

	t.Run("romaji performer metadata remains rejected", func(t *testing.T) {
		document := testPublicDocument()
		document.Full.Performers = []model.LyricsSourcePerformer{{PerformerID: "miku", Name: "Miku"}}
		document.Full.Lines[0].Segments[0].PerformerIDs = []string{"miku"}
		if _, err := authoritativePublicPerformerIDs(document, []model.CatalogVocalSignal{{VocalType: "sekai"}}); err == nil {
			t.Fatal("romanized performer metadata was accepted")
		}
	})
}

func validBoundImportReceiptFixture() (validatedReleaseBundle, releaseImportReceipt) {
	const receiptPath = "/private/tmp/moesekai-import-receipt-fixture.json"
	const evidencePath = "/private/tmp/moesekai-import-evidence-fixture.json"
	manifest := lyricsstaging.Manifest{
		SchemaVersion: 1,
		BatchSHA256:   strings.Repeat("1", 64),
		Items:         make([]lyricsstaging.Draft, releaseCatalogTargetCount),
	}
	receipt := releaseImportReceipt{
		SchemaVersion:                importReceiptSchemaVersion,
		CommitProtocol:               importReceiptProtocol,
		DatabaseAuditAction:          importReceiptAuditAction,
		ValidationReceiptSHA256:      strings.Repeat("5", 64),
		RootManifestSHA256:           strings.Repeat("9", 64),
		RootID:                       "root-bundle-a",
		RootSHA256:                   strings.Repeat("a", 64),
		ManifestSchemaVersion:        manifest.SchemaVersion,
		ManifestSHA256:               strings.Repeat("2", 64),
		BatchSHA256:                  manifest.BatchSHA256,
		EvidenceReceiptPath:          evidencePath,
		EvidenceReceiptSHA256:        strings.Repeat("3", 64),
		BackupSHA256:                 strings.Repeat("6", 64),
		StateDigestVersion:           importStateDigestVersion,
		BackupStateSHA256:            strings.Repeat("7", 64),
		PreImportDatabaseSHA256:      strings.Repeat("8", 64),
		PreImportDatabaseStateSHA256: strings.Repeat("7", 64),
		DatabasePath:                 "/private/tmp/moesekai-import-database-fixture.sqlite",
		RecoveryDatabasePath:         "/private/tmp/moesekai-import-recovery-fixture.sqlite",
		ReceiptPath:                  receiptPath,
		Operator:                     "offline-operator",
		ImportedCount:                0,
		ReplayedCount:                releaseCatalogTargetCount,
		Items:                        make([]releaseImportReceiptItem, releaseCatalogTargetCount),
		PreparedAt:                   "2026-08-02T08:00:00Z",
	}
	for index := 0; index < releaseCatalogTargetCount; index++ {
		musicID := index + 1
		documentSHA256 := strings.Repeat("4", 64)
		manifest.Items[index] = lyricsstaging.Draft{
			MusicID: musicID, DocumentSHA256: documentSHA256,
			Document: model.LyricsSourceDocument{
				FixedIdentities: []model.LyricsSourceFixedIdentity{{
					RenditionKey: "full-vocaloid", FetchedAt: "2026-08-02T08:00:00Z",
				}},
				Provenance: model.LyricsSourceComponentProvenance{
					FullText: model.LyricsSourceComponentRef{RenditionKey: "full-vocaloid"},
				},
			},
		}
		receipt.Items[index] = releaseImportReceiptItem{
			MusicID: musicID, Revision: 1, DocumentSHA256: documentSHA256,
			FullTextRenditionKey: "full-vocaloid", SourceFetchedAt: "2026-08-02T08:00:00Z",
			Artifacts: []releaseImportReceiptArtifact{},
		}
	}
	bundle := validatedReleaseBundle{
		Validation: releaseValidationReceipt{
			ReceiptSHA256: receipt.ValidationReceiptSHA256,
			RootManifest: validationRootBinding{
				File:   validationFileBinding{SHA256: receipt.RootManifestSHA256},
				RootID: receipt.RootID, RootSHA256: receipt.RootSHA256,
			},
			ImportEvidence: validationImportEvidenceBinding{
				File: validationFileBinding{Path: evidencePath}, ReceiptSHA256: receipt.EvidenceReceiptSHA256,
			},
		},
		Bindings: releaseBindings{
			Root: lyricsrootmanifest.Manifest{
				RootID: receipt.RootID, RootSHA256: receipt.RootSHA256,
			},
			RootFileSHA256: receipt.RootManifestSHA256,
			Manifest:       manifest, ManifestFileSHA256: receipt.ManifestSHA256,
		},
	}
	return bundle, receipt
}

func validValidationReceiptFixture(t *testing.T) releaseValidationReceipt {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	path := func(name string) string { return filepath.Join(base, name) }
	receipt := releaseValidationReceipt{
		SchemaVersion: releaseValidationReceiptSchemaVersion,
		Kind:          releaseValidationReceiptKind,
		Plan: validationPlanBinding{
			File:   validationFileBinding{Path: path("plan.json"), SHA256: strings.Repeat("a", 64), ByteCount: 10},
			PlanID: "plan-698", PlanSHA256: strings.Repeat("a", 64), SourceRoot: path("source"),
			SourceSnapshotSHA256: strings.Repeat("b", 64), SourceFileCount: 3,
		},
		Catalog: validationCatalogBinding{
			File:        validationFileBinding{Path: releaseCatalogPath, SHA256: releaseCatalogSHA256, ByteCount: 20},
			RecordCount: releaseCatalogTargetCount, IdentitySHA256: strings.Repeat("c", 64),
			MusicIDsSHA256: releaseCatalogMusicIDsSHA256, IdentityPolicyVersion: "catalog-identity-v2",
		},
		Ledger: validationTreeBinding{
			Path: path("ledger"), SHA256: strings.Repeat("d", 64), FileCount: 2, ByteCount: 30,
		},
		AcquisitionSet: validationAcquisitionSetBinding{
			File:      validationFileBinding{Path: path("acquisition-set.json"), SHA256: strings.Repeat("e", 64), ByteCount: 40},
			SetSHA256: strings.Repeat("f", 64),
		},
		ProviderOutcomes: validationTreeBinding{
			Path: path("provider-outcomes"), SHA256: strings.Repeat("1", 64), FileCount: 2, ByteCount: 50,
		},
		SongResults: validationTreeBinding{
			Path: path("song-results"), SHA256: strings.Repeat("2", 64), FileCount: releaseCatalogTargetCount, ByteCount: 60,
		},
		EvidencePack: validationEvidencePackBinding{
			Tree: validationTreeBinding{
				Path: path("evidence-pack"), SHA256: strings.Repeat("3", 64), FileCount: 2, ByteCount: 70,
			},
			PackSHA256: strings.Repeat("4", 64), SelectionSHA256: strings.Repeat("5", 64),
			EvidenceCount: 1, ShardCount: 1,
		},
		RootManifest: validationRootBinding{
			File:   validationFileBinding{Path: path("root.json"), SHA256: strings.Repeat("6", 64), ByteCount: 80},
			RootID: "root-698", RootSHA256: strings.Repeat("7", 64),
		},
		ImportManifest: validationImportManifestBinding{
			File:        validationFileBinding{Path: path("import.json"), SHA256: strings.Repeat("8", 64), ByteCount: 90},
			BatchSHA256: strings.Repeat("9", 64), ItemCount: releaseCatalogTargetCount,
		},
		ImportEvidence: validationImportEvidenceBinding{
			File:          validationFileBinding{Path: path("import-evidence.json"), SHA256: strings.Repeat("a", 64), ByteCount: 100},
			ReceiptSHA256: strings.Repeat("b", 64), EvidenceCount: 1,
		},
		AcquisitionCount: 1, ProviderOutcomeCount: 2, ReceiptPath: path("validation.json"),
	}
	digest, err := releaseValidationReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ReceiptSHA256 = digest
	if err := validateReleaseValidationReceipt(receipt, receipt.ReceiptPath); err != nil {
		t.Fatalf("validation receipt fixture: %v", err)
	}
	return receipt
}
