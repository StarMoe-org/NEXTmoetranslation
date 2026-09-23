package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsstaging"
)

type cancelOnCheckpointProgress struct {
	cancel context.CancelFunc
	buffer bytes.Buffer
	once   sync.Once
}

func (writer *cancelOnCheckpointProgress) Write(body []byte) (int, error) {
	count, err := writer.buffer.Write(body)
	if bytes.Contains(body, []byte("phase=stored completed=1 ")) {
		writer.once.Do(writer.cancel)
	}
	return count, err
}

func loadCheckpointTestCatalog(t *testing.T, path string) ([]catalogItem, []model.CatalogLyricsTarget) {
	t.Helper()
	catalog, err := loadCatalog(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]model.CatalogLyricsGroupingRecord, len(catalog))
	for index, item := range catalog {
		records[index] = model.CatalogLyricsGroupingRecord{
			MusicID: item.MusicID, Fingerprint: item.CatalogFingerprint, Evidence: item.Evidence,
		}
	}
	return catalog, model.ClassifyCatalogLyricsTargets(records)
}

func TestCheckpointFlagsHaveExplicitNonReportHelp(t *testing.T) {
	var help bytes.Buffer
	if err := run([]string{"-h"}, &help); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error=%v", err)
	}
	for _, text := range []string{
		"-checkpoint", "create a new exclusive mode-0600 private SQLite checkpoint",
		"not a final report or stage/import input", "-resume-checkpoint",
		"processing only missing work", "this is not a report",
	} {
		if !strings.Contains(help.String(), text) {
			t.Fatalf("checkpoint help missing %q: %s", text, help.String())
		}
	}
}

func TestCheckpointRejectsPriorReportResumeModes(t *testing.T) {
	for _, opts := range []options{
		{CheckpointPath: "fresh.sqlite", ResumeReportPath: "prior-701.json"},
		{ResumeCheckpointPath: "resume.sqlite", ResumeReportPath: "prior-701.json"},
	} {
		if err := validateCheckpointOptions(opts); err == nil ||
			err.Error() != "checkpoint execution must not be combined with -resume-report" {
			t.Fatalf("checkpoint/prior-report combination error=%v", err)
		}
	}
}

func TestCheckpointCancellationLeavesValidatedStrictlyResumableMissingWork(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "checkpoint-secret-one"),
		completeCatalogFixture(2, "checkpoint-secret-two"),
		completeCatalogFixture(3, "checkpoint-secret-three"),
	})
	checkpointPath := filepath.Join(t.TempDir(), "private-checkpoint.sqlite")
	opts := testOptions(path)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	opts.CheckpointPath = checkpointPath

	ctx, cancel := context.WithCancel(context.Background())
	progress := &cancelOnCheckpointProgress{cancel: cancel}
	var initialCalls atomic.Int32
	_, err := executeWithCatalogLoaderAndProgress(ctx, opts, fakeSourceClient{
		search: func(ctx context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			initialCalls.Add(1)
			if identity.MusicID == 1 {
				return nil, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}, func() time.Time { return time.Unix(500, 0).UTC() }, loadCatalog, progress)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled checkpoint error=%v", err)
	}
	if initialCalls.Load() != 2 {
		t.Fatalf("initial source calls=%d, want first completed and second canceled", initialCalls.Load())
	}
	info, err := os.Lstat(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || checkpointLinkCount(info) != 1 || info.Size() > maxCheckpointBytes {
		t.Fatalf("checkpoint file info=%+v links=%d", info, checkpointLinkCount(info))
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Lstat(checkpointPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canceled checkpoint sidecar %s: %v", suffix, err)
		}
	}
	body, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	var notReport report
	if json.Unmarshal(body, &notReport) == nil || !bytes.HasPrefix(body, []byte("SQLite format 3\x00")) {
		t.Fatalf("checkpoint was usable as JSON or lacked SQLite header")
	}

	resumeOpts := opts
	resumeOpts.CheckpointPath = ""
	resumeOpts.ResumeCheckpointPath = checkpointPath
	var resumedMu sync.Mutex
	resumedIDs := []int{}
	var resumeProgress bytes.Buffer
	resumed, err := executeWithCatalogLoaderAndProgress(context.Background(), resumeOpts, fakeSourceClient{
		search: func(_ context.Context, identity lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			resumedMu.Lock()
			resumedIDs = append(resumedIDs, identity.MusicID)
			resumedMu.Unlock()
			return nil, nil
		},
	}, func() time.Time { return time.Unix(900, 0).UTC() }, loadCatalog, &resumeProgress)
	if err != nil {
		t.Fatal(err)
	}
	resumedMu.Lock()
	if !equalMusicIDs(resumedIDs, []int{2, 3}) {
		t.Fatalf("strict resume processed music IDs %v, want only missing [2 3]", resumedIDs)
	}
	resumedMu.Unlock()
	if resumed.GeneratedAt != time.Unix(500, 0).UTC().Format(time.RFC3339Nano) ||
		len(resumed.Missing) != 3 || reportItemCount(resumed) != 3 {
		t.Fatalf("resumed report generatedAt=%q summary=%+v", resumed.GeneratedAt, resumed.Summary)
	}
	for _, secret := range []string{"checkpoint-secret-one", "checkpoint-secret-two", "checkpoint-secret-three"} {
		if strings.Contains(progress.buffer.String(), secret) || strings.Contains(resumeProgress.String(), secret) {
			t.Fatalf("checkpoint progress leaked catalog content %q", secret)
		}
	}
	progressLine := regexp.MustCompile(`^checkpoint phase=(created|resumed|stored|complete) completed=[0-9]+ missing=[0-9]+ catalog_review=[0-9]+ game_size_evidence=[0-9]+ unique_complete=[0-9]+ ambiguous=[0-9]+ missing_class=[0-9]+ incomplete=[0-9]+ error=[0-9]+ evidence_items=[0-9]+ evidence_raw_bytes=[0-9]+$`)
	for _, output := range []string{progress.buffer.String(), resumeProgress.String()} {
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if !progressLine.MatchString(line) {
				t.Fatalf("non-statistical checkpoint progress line %q", line)
			}
		}
	}

	var unexpectedCalls atomic.Int32
	reconstructedAgain, err := execute(context.Background(), resumeOpts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			unexpectedCalls.Add(1)
			return nil, errors.New("complete checkpoint must not search")
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			unexpectedCalls.Add(1)
			return lyricssource.FixedRevision{}, errors.New("complete checkpoint must not fetch")
		},
	}, func() time.Time { return time.Unix(1200, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if unexpectedCalls.Load() != 0 || !reportJSONEqual(reconstructedAgain, resumed) {
		t.Fatalf("complete reconstruction calls=%d equal=%t", unexpectedCalls.Load(), reportJSONEqual(reconstructedAgain, resumed))
	}
}

func TestCheckpointResultAndExactEvidenceTransactionRejectsSameIDConflict(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "transaction-one"),
		completeCatalogFixture(2, "transaction-two"),
	})
	catalog, targets := loadCheckpointTestCatalog(t, path)
	opts := testOptions(path)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	checkpointPath := filepath.Join(t.TempDir(), "transaction.sqlite")
	checkpoint, err := createPreflightCheckpoint(context.Background(), checkpointPath, opts, catalog, targets,
		time.Unix(700, 0).UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}

	candidateOne := candidateFor(1, "one")
	itemOne := baseReportItem(catalog[0], targets[0])
	itemOne.SearchAttempts = 1
	first := classifyFixedRevision(itemOne, candidateOne, attemptResult[lyricssource.FixedRevision]{
		value: fixedFor(candidateOne, "transaction lyric"), attempts: 1,
	})
	if first.class != "unique_complete" {
		t.Fatalf("first result=%+v", first)
	}
	if err := checkpoint.storeResult(first); err != nil {
		t.Fatal(err)
	}

	candidateTwo := candidateOne
	candidateTwo.Categories = []string{"Lyrics", "Other"}
	candidateTwo.IndexEvidence = []lyricssource.IndexEvidence{clonePreflightIndexEvidence(candidateOne.IndexEvidence[0])}
	candidateTwo.IndexEvidence[0].Categories = append([]string(nil), candidateTwo.Categories...)
	if err := lyricssource.ValidateCandidateIndexEvidence(candidateTwo); err != nil {
		t.Fatalf("conflicting but individually valid candidate: %v", err)
	}
	itemTwo := baseReportItem(catalog[1], targets[1])
	itemTwo.SearchAttempts = 1
	second := classifyFixedRevision(itemTwo, candidateTwo, attemptResult[lyricssource.FixedRevision]{
		value: fixedFor(candidateTwo, "transaction lyric"), attempts: 1,
	})
	if second.class != "unique_complete" {
		t.Fatalf("second result=%+v", second)
	}
	if err := checkpoint.storeResult(second); !errors.Is(err, errPreflightEvidenceConflict) {
		t.Fatalf("same-ID conflict error=%v", err)
	}
	var resultCount, evidenceCount, linkCount int
	if err := checkpoint.database.QueryRow(`SELECT COUNT(*) FROM results`).Scan(&resultCount); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.database.QueryRow(`SELECT COUNT(*) FROM evidence`).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.database.QueryRow(`SELECT COUNT(*) FROM result_evidence`).Scan(&linkCount); err != nil {
		t.Fatal(err)
	}
	if resultCount != 1 || evidenceCount != 1 || linkCount != 1 {
		t.Fatalf("conflict transaction counts results=%d evidence=%d links=%d", resultCount, evidenceCount, linkCount)
	}
	if err := checkpoint.validateState(context.Background(), false); err != nil {
		t.Fatalf("checkpoint invalid after rolled-back conflict: %v", err)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointSharedEvidenceReuseChargesCanonicalReceiptOnce(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "shared-evidence-one"),
		completeCatalogFixture(2, "shared-evidence-two"),
	})
	catalog, targets := loadCheckpointTestCatalog(t, catalogPath)
	opts := testOptions(catalogPath)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	checkpoint, err := createPreflightCheckpoint(context.Background(), filepath.Join(t.TempDir(), "shared.sqlite"),
		opts, catalog, targets, time.Unix(750, 0).UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	candidate := candidateFor(1, "shared-evidence")
	for index := range catalog {
		item := baseReportItem(catalog[index], targets[index])
		item.SearchAttempts = 1
		result := classifyFixedRevision(item, candidate, attemptResult[lyricssource.FixedRevision]{
			value: fixedFor(candidate, "shared evidence lyric"), attempts: 1,
		})
		if err := checkpoint.storeResult(result); err != nil {
			t.Fatalf("store shared result %d: %v", index, err)
		}
	}

	var evidenceRows, linkRows int
	if err := checkpoint.database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM evidence),(SELECT COUNT(*) FROM result_evidence)`).Scan(&evidenceRows, &linkRows); err != nil {
		t.Fatal(err)
	}
	counters, err := scanCheckpointCounters(checkpoint.database.QueryRow(checkpointCountersQuery()))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(candidate.IndexEvidence)
	if err != nil {
		t.Fatal(err)
	}
	encodedReceipt, err := lyricsstaging.MarshalPrivateEvidenceReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if evidenceRows != 1 || linkRows != 2 || counters.Stats.EvidenceItems != 1 ||
		counters.Stats.EvidenceRawBytes != int64(len(candidate.IndexEvidence[0].Raw)) ||
		counters.EvidenceReceiptBytes != int64(len(encodedReceipt)) {
		t.Fatalf("shared evidence rows=%d links=%d counters=%+v receiptBytes=%d want=%d",
			evidenceRows, linkRows, counters, counters.EvidenceReceiptBytes, len(encodedReceipt))
	}
	if err := checkpoint.validateState(context.Background(), true); err != nil {
		t.Fatalf("shared-evidence checkpoint invalid: %v", err)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointResumeBindsCatalogSchemaCountFingerprintAndExecutionOptions(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "binding")})
	checkpointPath := filepath.Join(t.TempDir(), "binding.sqlite")
	opts := testOptions(path)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	opts.CheckpointPath = checkpointPath
	if _, err := execute(context.Background(), opts, fakeSourceClient{}, func() time.Time { return time.Unix(800, 0).UTC() }); err != nil {
		t.Fatal(err)
	}

	assertNoSource := func(t *testing.T, resume options, want string) {
		t.Helper()
		var sourceCalls atomic.Int32
		_, err := execute(context.Background(), resume, fakeSourceClient{
			search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				sourceCalls.Add(1)
				return nil, nil
			},
		}, time.Now)
		if err == nil || !strings.Contains(err.Error(), want) || sourceCalls.Load() != 0 {
			t.Fatalf("resume binding err=%v want=%q sourceCalls=%d", err, want, sourceCalls.Load())
		}
	}

	changedOptions := opts
	changedOptions.CheckpointPath = ""
	changedOptions.ResumeCheckpointPath = checkpointPath
	changedOptions.Concurrency = 2
	assertNoSource(t, changedOptions, "execution options")

	changedCatalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "binding-changed")})
	changedCatalog := opts
	changedCatalog.DatabasePath = changedCatalogPath
	changedCatalog.CheckpointPath = ""
	changedCatalog.ResumeCheckpointPath = checkpointPath
	assertNoSource(t, changedCatalog, "current catalog")

	largerCatalogPath := writeV18Catalog(t, []catalogFixture{
		completeCatalogFixture(1, "binding"), completeCatalogFixture(2, "binding-two"),
	})
	changedCount := opts
	changedCount.DatabasePath = largerCatalogPath
	changedCount.CheckpointPath = ""
	changedCount.ResumeCheckpointPath = checkpointPath
	assertNoSource(t, changedCount, "current catalog")

	database, err := sql.Open("sqlite", "file:"+checkpointPath+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA user_version=3`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	validResume := opts
	validResume.CheckpointPath = ""
	validResume.ResumeCheckpointPath = checkpointPath
	assertNoSource(t, validResume, "version")
}

func TestCheckpointPermissionsFreshExclusivityAndPublicationRecovery(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "publication")})
	checkpointPath := filepath.Join(t.TempDir(), "publication.sqlite")
	opts := testOptions(path)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	opts.CheckpointPath = checkpointPath
	candidate := candidateFor(1, "one")
	var exactProgress bytes.Buffer
	generated, err := executeWithCatalogLoaderAndProgress(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return fixedFor(candidate, "private exact evidence reconstruction"), nil
		},
	}, func() time.Time { return time.Unix(900, 0).UTC() }, loadCatalog, &exactProgress)
	if err != nil {
		t.Fatal(err)
	}
	if generated.EvidenceReceipt == nil || len(generated.EvidenceReceipt.IndexEvidence) != 1 || len(generated.UniqueComplete) != 1 {
		t.Fatalf("checkpointed exact-evidence report=%+v", generated.Summary)
	}
	for _, private := range []string{candidate.Title, candidate.IndexEvidence[0].EvidenceID, string(candidate.IndexEvidence[0].Raw)} {
		if strings.Contains(exactProgress.String(), private) {
			t.Fatalf("checkpoint progress leaked private content %q", private)
		}
	}
	if !strings.Contains(exactProgress.String(), "evidence_items=1") ||
		!strings.Contains(exactProgress.String(), "evidence_raw_bytes="+strconv.Itoa(len(candidate.IndexEvidence[0].Raw))) {
		t.Fatalf("checkpoint progress omitted exact-evidence statistics: %s", exactProgress.String())
	}
	if _, err := execute(context.Background(), opts, fakeSourceClient{}, time.Now); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("fresh checkpoint overwrite error=%v", err)
	}

	failedOutput := filepath.Join(t.TempDir(), "failed-report.json")
	publishFailure := errors.New("synthetic publication failure")
	if err := writeReportWithPublisherContext(context.Background(), failedOutput, path, generated,
		func(string, string) error { return publishFailure }, nil); !errors.Is(err, publishFailure) {
		t.Fatalf("publication failure=%v", err)
	}
	if _, err := os.Lstat(failedOutput); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed final report was visible: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(failedOutput), "."+filepath.Base(failedOutput)+".tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("failed publication temporaries=%v err=%v", matches, err)
	}

	resume := opts
	resume.CheckpointPath = ""
	resume.ResumeCheckpointPath = checkpointPath
	var sourceCalls atomic.Int32
	reconstructed, err := execute(context.Background(), resume, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, errors.New("complete checkpoint must not contact a provider")
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCalls.Load() != 0 || !reportJSONEqual(reconstructed, generated) {
		t.Fatalf("publication recovery calls=%d reportEqual=%t", sourceCalls.Load(), reportJSONEqual(reconstructed, generated))
	}
	output := filepath.Join(t.TempDir(), "final-report.json")
	if err := writeReport(output, path, reconstructed, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("final report mode=%#o", info.Mode().Perm())
	}

	if err := os.Chmod(checkpointPath, 0o640); err != nil {
		t.Fatal(err)
	}
	badModeOutput := filepath.Join(t.TempDir(), "bad-mode.json")
	resume.OutputPath = badModeOutput
	if err := validateCheckpointOptions(resume); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("bad checkpoint mode error=%v", err)
	}
}

func TestAtomicReportPublicationRemovesOwnedOutputOnCancellation(t *testing.T) {
	path := writeV18Catalog(t, []catalogFixture{})
	generated, err := execute(context.Background(), testOptions(path), fakeSourceClient{}, func() time.Time { return time.Unix(1000, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}

	raceOutput := filepath.Join(t.TempDir(), "raced-report.json")
	err = writeReportWithPublisherContext(context.Background(), raceOutput, path, generated, os.Link, func() {
		if err := os.WriteFile(raceOutput, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("atomic report publication overwrote a path created before publish")
	}
	body, readErr := os.ReadFile(raceOutput)
	if readErr != nil || string(body) != "keep" {
		t.Fatalf("raced output=%q err=%v", body, readErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := filepath.Join(t.TempDir(), "canceled-report.json")
	err = writeReportWithPublisherContext(ctx, output, path, generated, os.Link, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publication error=%v", err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled final report was visible: %v", err)
	}
}

func createCheckpointFixture(t *testing.T, exactEvidence bool) (string, string, options, report) {
	t.Helper()
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "checkpoint-fixture")})
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	opts := testOptions(catalogPath)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	opts.CheckpointPath = checkpointPath
	source := fakeSourceClient{}
	if exactEvidence {
		candidate := candidateFor(1, "checkpoint-fixture")
		source = fakeSourceClient{
			search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
				return []lyricssource.Candidate{candidate}, nil
			},
			fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
				return fixedFor(candidate, "checkpoint fixture lyric"), nil
			},
		}
	}
	generated, err := execute(context.Background(), opts, source, func() time.Time { return time.Unix(1100, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	return catalogPath, checkpointPath, opts, generated
}

func TestCheckpointRejectsDatabaseAndFinalOutputAliasesForEntireSQLitePathFamily(t *testing.T) {
	_, checkpointPath, opts, _ := createCheckpointFixture(t, false)
	resume := opts
	resume.CheckpointPath = ""
	resume.ResumeCheckpointPath = checkpointPath
	family := checkpointPathFamily(checkpointPath)
	for _, label := range []string{"database", "final output"} {
		for _, protectedPath := range family {
			name := label + " " + filepath.Base(protectedPath)
			t.Run(name, func(t *testing.T) {
				aliased := resume
				if label == "database" {
					aliased.DatabasePath = protectedPath
				} else {
					aliased.OutputPath = protectedPath
				}
				err := validateCheckpointOptions(aliased)
				if err == nil || !strings.Contains(err.Error(), "must not alias the checkpoint") {
					t.Fatalf("%s alias %s error=%v", label, protectedPath, err)
				}
			})
		}
	}

	t.Run("hard-link alias", func(t *testing.T) {
		aliasPath := filepath.Join(t.TempDir(), "database-alias.db")
		if err := os.Link(checkpointPath, aliasPath); err != nil {
			t.Fatal(err)
		}
		aliased := resume
		aliased.DatabasePath = aliasPath
		if err := validateCheckpointOptions(aliased); err == nil || !strings.Contains(err.Error(), "must not resolve") {
			t.Fatalf("hard-link database alias error=%v", err)
		}
	})

	t.Run("sidecar hard-link alias", func(t *testing.T) {
		sidecarPath := checkpointAnchorPath(checkpointPath) + "-journal"
		if err := os.WriteFile(sidecarPath, []byte("preserve sidecar alias"), 0o600); err != nil {
			t.Fatal(err)
		}
		aliasPath := filepath.Join(t.TempDir(), "sidecar-alias")
		if err := os.Link(sidecarPath, aliasPath); err != nil {
			t.Fatal(err)
		}
		aliased := resume
		aliased.OutputPath = aliasPath
		if err := validateCheckpointOptions(aliased); err == nil || !strings.Contains(err.Error(), "must not resolve") {
			t.Fatalf("sidecar hard-link output alias error=%v", err)
		}
		body, err := os.ReadFile(sidecarPath)
		if err != nil || string(body) != "preserve sidecar alias" {
			t.Fatalf("sidecar alias was modified: body=%q err=%v", body, err)
		}
	})
}

func mutateClosedCheckpoint(t *testing.T, path string, mutation func(*sql.DB)) {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	mutation(database)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed mutation left visible sidecar %s: %v", suffix, err)
		}
	}
}

func rewriteCheckpointTableSQL(t *testing.T, database *sql.DB, table, oldFragment, newFragment string) string {
	t.Helper()
	var original string
	if err := database.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&original); err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(original, oldFragment, newFragment, 1)
	if drifted == original {
		t.Fatalf("checkpoint table %s did not contain %q", table, oldFragment)
	}
	var schemaVersion int
	if err := database.QueryRow(`PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA writable_schema=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE sqlite_schema SET sql=? WHERE type='table' AND name=?`, drifted, table); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(fmt.Sprintf(`PRAGMA schema_version=%d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA writable_schema=OFF`); err != nil {
		t.Fatal(err)
	}
	return drifted
}

func resumeCheckpointError(t *testing.T, opts options, checkpointPath, want string) error {
	t.Helper()
	resume := opts
	resume.CheckpointPath = ""
	resume.ResumeCheckpointPath = checkpointPath
	var sourceCalls atomic.Int32
	_, err := execute(context.Background(), resume, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, errors.New("corrupt checkpoint must fail before source search")
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			sourceCalls.Add(1)
			return lyricssource.FixedRevision{}, errors.New("corrupt checkpoint must fail before source fetch")
		},
	}, time.Now)
	if err == nil || !strings.Contains(err.Error(), want) || sourceCalls.Load() != 0 {
		t.Fatalf("resume corruption err=%v want=%q sourceCalls=%d", err, want, sourceCalls.Load())
	}
	return err
}

func TestCheckpointPrivateDirectoryRejectsUntrustedWritableAncestry(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "private-directory")})
	writableAncestor := filepath.Join(t.TempDir(), "writable-ancestor")
	if err := os.Mkdir(writableAncestor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writableAncestor, 0o777); err != nil {
		t.Fatal(err)
	}
	privateRun := filepath.Join(writableAncestor, "private-run")
	if err := os.Mkdir(privateRun, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(catalogPath)
	opts.CheckpointPath = filepath.Join(privateRun, "checkpoint.sqlite")
	if err := validateCheckpointOptions(opts); err == nil || !strings.Contains(err.Error(), "writable by an untrusted local UID") {
		t.Fatalf("writable ancestry error=%v", err)
	}
}

func TestCheckpointInitializationCancellationRemovesOnlyOwnedIncompleteInode(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "initialization-cancel")})
	catalog, targets := loadCheckpointTestCatalog(t, catalogPath)
	opts := testOptions(catalogPath)
	for _, test := range []struct {
		name        string
		replacePath bool
	}{
		{name: "owned reservation is removed"},
		{name: "unrelated visible replacement survives", replacePath: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkpointPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const replacementBody = "unrelated visible inode"
			replacementPath := checkpointPath + ".replacement"
			if test.replacePath {
				if err := os.WriteFile(replacementPath, []byte(replacementBody), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var hookErr error
			checkpointBeforeInitializationCommitHook = func(visiblePath, _ string) {
				if test.replacePath {
					hookErr = os.Rename(replacementPath, visiblePath)
				}
				cancel()
			}
			t.Cleanup(func() { checkpointBeforeInitializationCommitHook = nil })
			checkpoint, err := createPreflightCheckpoint(ctx, checkpointPath, opts, catalog, targets,
				time.Unix(1190, 0).UTC().Format(time.RFC3339Nano))
			checkpointBeforeInitializationCommitHook = nil
			if checkpoint != nil {
				_ = checkpoint.Close()
				t.Fatal("canceled initialization returned a checkpoint")
			}
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("initialization cancellation error=%v", err)
			}
			if test.replacePath {
				body, readErr := os.ReadFile(checkpointPath)
				if readErr != nil || string(body) != replacementBody {
					t.Fatalf("unrelated replacement body=%q err=%v", body, readErr)
				}
			} else if _, statErr := os.Lstat(checkpointPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("canceled initialization stranded visible checkpoint: %v", statErr)
			}
			anchorPath := checkpointAnchorPath(checkpointPath)
			if _, statErr := os.Lstat(anchorPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("canceled initialization stranded operational anchor: %v", statErr)
			}
			for _, basePath := range []string{checkpointPath, anchorPath} {
				for _, suffix := range sqliteSidecarSuffixes {
					if _, statErr := os.Lstat(basePath + suffix); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("canceled initialization stranded sidecar %s: %v", basePath+suffix, statErr)
					}
				}
			}
		})
	}
}

func TestCheckpointFreshCreationPreservesUnrelatedAnchorWithoutStrandingReservation(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "stale-anchor")})
	catalog, targets := loadCheckpointTestCatalog(t, catalogPath)
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	anchorPath := checkpointAnchorPath(checkpointPath)
	const anchorBody = "preexisting operational state"
	if err := os.WriteFile(anchorPath, []byte(anchorBody), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(catalogPath)
	if _, err := createPreflightCheckpoint(context.Background(), checkpointPath, opts, catalog, targets,
		time.Unix(1200, 0).UTC().Format(time.RFC3339Nano)); err == nil {
		t.Fatal("fresh checkpoint overwrote or accepted an unrelated operational anchor")
	}
	if _, err := os.Lstat(checkpointPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed fresh creation stranded visible reservation: %v", err)
	}
	body, err := os.ReadFile(anchorPath)
	if err != nil || string(body) != anchorBody {
		t.Fatalf("unrelated anchor body=%q err=%v", body, err)
	}
}

func TestCheckpointSQLiteOpenUsesPinnedOperationalInodeAndFailsClosedOnVisibleSwap(t *testing.T) {
	catalogPath, checkpointPath, opts, original := createCheckpointFixture(t, false)
	catalog, targets := loadCheckpointTestCatalog(t, catalogPath)
	replacementPath := checkpointPath + ".replacement"
	const replacementBody = "not a sqlite database and must never be opened"
	if err := os.WriteFile(replacementPath, []byte(replacementBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var hookErr error
	checkpointBeforeSQLiteOpenHook = func(visiblePath, operationalPath string) {
		hookErr = os.Rename(replacementPath, visiblePath)
	}
	t.Cleanup(func() { checkpointBeforeSQLiteOpenHook = nil })
	checkpoint, err := openPreflightCheckpoint(context.Background(), checkpointPath, opts, catalog, targets)
	if checkpoint != nil {
		_ = checkpoint.Close()
		t.Fatal("visible pathname swap unexpectedly opened a checkpoint")
	}
	checkpointBeforeSQLiteOpenHook = nil
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "changed after SQLite open") {
		t.Fatalf("visible swap error=%v", err)
	}
	body, readErr := os.ReadFile(checkpointPath)
	if readErr != nil || string(body) != replacementBody {
		t.Fatalf("replacement visible inode was modified: body=%q err=%v", body, readErr)
	}
	anchorPath := checkpointAnchorPath(checkpointPath)
	anchorInfo, anchorErr := os.Lstat(anchorPath)
	if anchorErr != nil || !anchorInfo.Mode().IsRegular() || anchorInfo.Mode().Perm() != 0o600 || checkpointLinkCount(anchorInfo) != 1 {
		t.Fatalf("recoverable pinned anchor info=%+v links=%d err=%v", anchorInfo, checkpointLinkCount(anchorInfo), anchorErr)
	}
	if err := os.Remove(checkpointPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(anchorPath, checkpointPath); err != nil {
		t.Fatal(err)
	}
	resume := opts
	resume.CheckpointPath = ""
	resume.ResumeCheckpointPath = checkpointPath
	var sourceCalls atomic.Int32
	recovered, err := execute(context.Background(), resume, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, errors.New("complete recovered checkpoint must not search")
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCalls.Load() != 0 || !reportJSONEqual(recovered, original) {
		t.Fatalf("recovered visible-swap checkpoint calls=%d equal=%t", sourceCalls.Load(), reportJSONEqual(recovered, original))
	}
}

func TestCheckpointCommittedTransactionSurvivesVisiblePathRace(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "commit-race")})
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	replacementPath := checkpointPath + ".replacement"
	const replacementBody = "preserve raced replacement"
	if err := os.WriteFile(replacementPath, []byte(replacementBody), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(catalogPath)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	opts.CheckpointPath = checkpointPath
	candidate := candidateFor(1, "commit-race")
	var hookErr error
	checkpointAfterResultCommitHook = func(visiblePath, operationalPath string) {
		hookErr = os.Rename(replacementPath, visiblePath)
	}
	t.Cleanup(func() { checkpointAfterResultCommitHook = nil })
	_, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return fixedFor(candidate, "committed before pathname race"), nil
		},
	}, func() time.Time { return time.Unix(1300, 0).UTC() })
	checkpointAfterResultCommitHook = nil
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "checkpoint visible path") {
		t.Fatalf("post-commit visible race error=%v", err)
	}
	body, readErr := os.ReadFile(checkpointPath)
	if readErr != nil || string(body) != replacementBody {
		t.Fatalf("raced replacement body=%q err=%v", body, readErr)
	}
	anchorPath := checkpointAnchorPath(checkpointPath)
	if err := os.Remove(checkpointPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(anchorPath, checkpointPath); err != nil {
		t.Fatal(err)
	}
	resume := opts
	resume.CheckpointPath = ""
	resume.ResumeCheckpointPath = checkpointPath
	var sourceCalls atomic.Int32
	recovered, err := execute(context.Background(), resume, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, errors.New("committed result must be recovered without search")
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCalls.Load() != 0 || len(recovered.UniqueComplete) != 1 || recovered.EvidenceReceipt == nil {
		t.Fatalf("committed race recovery calls=%d summary=%+v receipt=%v", sourceCalls.Load(), recovered.Summary, recovered.EvidenceReceipt != nil)
	}
}

func TestCheckpointExactCreateSQLAndForeignKeyActionsAreVersionBound(t *testing.T) {
	t.Run("weakened check constraint", func(t *testing.T) {
		_, checkpointPath, opts, _ := createCheckpointFixture(t, false)
		mutateClosedCheckpoint(t, checkpointPath, func(database *sql.DB) {
			rewriteCheckpointTableSQL(t, database, "evidence", "raw_byte_count <= 2097152", "raw_byte_count <= 2097153")
		})
		resumeCheckpointError(t, opts, checkpointPath, "CREATE SQL does not match the exact versioned constraints")
	})

	t.Run("foreign key delete action", func(t *testing.T) {
		_, checkpointPath, opts, _ := createCheckpointFixture(t, false)
		var drifted string
		mutateClosedCheckpoint(t, checkpointPath, func(database *sql.DB) {
			drifted = rewriteCheckpointTableSQL(t, database, "results", "ON DELETE RESTRICT", "ON DELETE CASCADE")
		})
		definitionIndex := -1
		for index := range checkpointSchemaDefinitions {
			if checkpointSchemaDefinitions[index].name == "results" {
				definitionIndex = index
				break
			}
		}
		if definitionIndex < 0 {
			t.Fatal("results schema definition is missing")
		}
		original := checkpointSchemaDefinitions[definitionIndex].sql
		checkpointSchemaDefinitions[definitionIndex].sql = drifted
		t.Cleanup(func() { checkpointSchemaDefinitions[definitionIndex].sql = original })
		resumeCheckpointError(t, opts, checkpointPath, "foreign-key graph or actions do not match the exact version")
	})
}

func TestCheckpointRejectsCanonicalEvidenceAndLinkStatisticCorruption(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*testing.T, *sql.DB)
	}{
		{
			name: "evidence table key versus embedded ID", want: "table key does not match its embedded evidence ID",
			mutate: func(t *testing.T, database *sql.DB) {
				var evidenceID string
				if err := database.QueryRow(`SELECT evidence_id FROM evidence`).Scan(&evidenceID); err != nil {
					t.Fatal(err)
				}
				changed := evidenceID + "-table-key-drift"
				if _, err := database.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`UPDATE result_evidence SET evidence_id=?`, changed); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`UPDATE evidence SET evidence_id=?`, changed); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "evidence canonical JSON", want: "checkpoint JSON is not canonical",
			mutate: func(t *testing.T, database *sql.DB) {
				var body []byte
				if err := database.QueryRow(`SELECT evidence_json FROM evidence`).Scan(&body); err != nil {
					t.Fatal(err)
				}
				body = append(body, ' ')
				if _, err := database.Exec(`UPDATE evidence SET evidence_json=?`, body); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "evidence canonical JSON SHA", want: "evidence SHA does not bind its canonical JSON",
			mutate: func(t *testing.T, database *sql.DB) {
				if _, err := database.Exec(`UPDATE evidence SET evidence_sha256=?`, strings.Repeat("0", 64)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "decoded raw byte count", want: "raw-byte count does not match decoded raw bytes",
			mutate: func(t *testing.T, database *sql.DB) {
				if _, err := database.Exec(`UPDATE evidence SET raw_byte_count=raw_byte_count+1`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "result evidence item count", want: "statistics do not match their exact links",
			mutate: func(t *testing.T, database *sql.DB) {
				if _, err := database.Exec(`UPDATE results SET evidence_item_count=0`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "result evidence raw byte count", want: "statistics do not match their exact links",
			mutate: func(t *testing.T, database *sql.DB) {
				if _, err := database.Exec(`UPDATE results SET evidence_raw_bytes=evidence_raw_bytes+1`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing exact link", want: "statistics do not match their exact links",
			mutate: func(t *testing.T, database *sql.DB) {
				if _, err := database.Exec(`DELETE FROM result_evidence`); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, checkpointPath, opts, _ := createCheckpointFixture(t, true)
			mutateClosedCheckpoint(t, checkpointPath, func(database *sql.DB) { test.mutate(t, database) })
			resumeCheckpointError(t, opts, checkpointPath, test.want)
		})
	}
}

func TestCheckpointRejectsTransactionalCounterCorruption(t *testing.T) {
	_, checkpointPath, opts, _ := createCheckpointFixture(t, true)
	mutateClosedCheckpoint(t, checkpointPath, func(database *sql.DB) {
		if _, err := database.Exec(`UPDATE checkpoint_counters SET evidence_raw_bytes=evidence_raw_bytes+1`); err != nil {
			t.Fatal(err)
		}
	})
	resumeCheckpointError(t, opts, checkpointPath, "transactional counters do not bind")
}

func TestCheckpointEncodedEvidenceAccountingMatchesReceiptAndDatabaseContracts(t *testing.T) {
	evidence := make([]lyricssource.IndexEvidence, 0, 3)
	for index := 0; index < 3; index++ {
		candidate := candidateFor(index+1, fmt.Sprintf("encoded-capacity-%d", index))
		item := clonePreflightIndexEvidence(candidate.IndexEvidence[0])
		evidence = append(evidence, item)
	}
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(evidence)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := lyricsstaging.MarshalPrivateEvidenceReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	contributions := make([]int64, 0, len(receipt.IndexEvidence))
	for _, item := range receipt.IndexEvidence {
		body, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		contribution, err := checkpointEvidenceReceiptItemBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		contributions = append(contributions, contribution)
	}
	accounted, err := checkpointEvidenceReceiptBytesAfterInsert(0, 0, contributions)
	if err != nil {
		t.Fatal(err)
	}
	if accounted != int64(len(encoded)) {
		t.Fatalf("checkpoint receipt accounting=%d, shared receipt encoder=%d", accounted, len(encoded))
	}
	first, err := checkpointEvidenceReceiptBytesAfterInsert(0, 0, contributions[:1])
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := checkpointEvidenceReceiptBytesAfterInsert(1, first, contributions[1:])
	if err != nil {
		t.Fatal(err)
	}
	if incremental != accounted {
		t.Fatalf("incremental receipt accounting=%d, one-shot=%d", incremental, accounted)
	}

	remainingContribution := int64(lyricsstaging.MaxPrivateEvidenceReceiptBytes) - accounted - 1
	atLimit, err := checkpointEvidenceReceiptBytesAfterInsert(int64(len(contributions)), accounted, []int64{remainingContribution})
	if err != nil {
		t.Fatal(err)
	}
	overLimit, err := checkpointEvidenceReceiptBytesAfterInsert(int64(len(contributions)), accounted, []int64{remainingContribution + 1})
	if err != nil {
		t.Fatal(err)
	}
	if atLimit != lyricsstaging.MaxPrivateEvidenceReceiptBytes || overLimit != atLimit+1 {
		t.Fatalf("receipt boundary at=%d over=%d limit=%d", atLimit, overLimit, lyricsstaging.MaxPrivateEvidenceReceiptBytes)
	}
	if maxCheckpointPages*checkpointPageSize != maxCheckpointBytes || maxCheckpointBytes != 128<<20 ||
		maxCheckpointEvidenceJSONBytes != int64(lyricsstaging.MaxPrivateEvidenceReceiptBytes) {
		t.Fatalf("checkpoint capacity db=%d evidence=%d receipt=%d", maxCheckpointBytes,
			maxCheckpointEvidenceJSONBytes, lyricsstaging.MaxPrivateEvidenceReceiptBytes)
	}

	_, checkpointPath, _, _ := createCheckpointFixture(t, false)
	database, err := sql.Open("sqlite", "file:"+checkpointPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, table := range []string{"evidence", "result_evidence"} {
		var withoutRowID int
		if err := database.QueryRow(`SELECT wr FROM pragma_table_list WHERE schema='main' AND name=?`, table).Scan(&withoutRowID); err != nil {
			t.Fatal(err)
		}
		if withoutRowID != 1 {
			t.Fatalf("capacity-sensitive table %s retains duplicate rowid index storage", table)
		}
	}
}

func TestCheckpointEnforcesPerEvidenceAndAggregatePrivateReceiptLimitsBeforeCommit(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "capacity")})
	catalog, targets := loadCheckpointTestCatalog(t, catalogPath)
	opts := testOptions(catalogPath)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	checkpoint, err := createPreflightCheckpoint(context.Background(), filepath.Join(t.TempDir(), "capacity.sqlite"),
		opts, catalog, targets, time.Unix(1400, 0).UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	validCandidate := candidateFor(1, "capacity")
	validItem := baseReportItem(catalog[0], targets[0])
	validItem.SearchAttempts = 1
	perEvidence := classifyFixedRevision(validItem, validCandidate, attemptResult[lyricssource.FixedRevision]{
		value: fixedFor(validCandidate, "capacity lyric"), attempts: 1,
	})
	perEvidence.evidence = append([]lyricssource.IndexEvidence(nil), perEvidence.evidence...)
	perEvidence.evidence[0] = clonePreflightIndexEvidence(perEvidence.evidence[0])
	perEvidence.evidence[0].Raw = bytes.Repeat([]byte("x"), lyricssource.MaxIndexEvidenceRawBytes+1)
	if err := checkpoint.storeResult(perEvidence); err == nil {
		t.Fatal("checkpoint accepted evidence larger than 2 MiB")
	}

	var aggregateCandidate lyricssource.Candidate
	for index := 0; index < 17; index++ {
		component := candidateFor(1000+index, fmt.Sprintf("capacity-%d", index))
		rawBytes := lyricssource.MaxIndexEvidenceRawBytes
		if index == 16 {
			rawBytes = 1
		}
		component = attachCandidateRevisionEvidence(component, bytes.Repeat([]byte{byte('a' + index%26)}, rawBytes))
		if index == 0 {
			aggregateCandidate = component
			aggregateCandidate.IndexEvidenceRefs = nil
			aggregateCandidate.IndexEvidence = nil
		}
		aggregateCandidate.IndexEvidenceRefs = append(aggregateCandidate.IndexEvidenceRefs, component.IndexEvidenceRefs[0])
		aggregateCandidate.IndexEvidence = append(aggregateCandidate.IndexEvidence, component.IndexEvidence[0])
	}
	aggregateItem := baseReportItem(catalog[0], targets[0])
	aggregateItem.SearchAttempts = 1
	aggregate := classifyFixedRevision(aggregateItem, aggregateCandidate, attemptResult[lyricssource.FixedRevision]{
		value: fixedFor(aggregateCandidate, "aggregate capacity lyric"), attempts: 1,
	})
	if err := checkpoint.storeResult(aggregate); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("aggregate private evidence bound error=%v", err)
	}
	var results, evidence, links int
	if err := checkpoint.database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM results),(SELECT COUNT(*) FROM evidence),(SELECT COUNT(*) FROM result_evidence)`).
		Scan(&results, &evidence, &links); err != nil {
		t.Fatal(err)
	}
	if results != 0 || evidence != 0 || links != 0 {
		t.Fatalf("capacity failures committed results=%d evidence=%d links=%d", results, evidence, links)
	}
	if err := checkpoint.validateState(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRejectsUnsupportedOrNonprivateOperationalSidecarsWithoutDeletingThem(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
		mode   os.FileMode
		want   string
	}{
		{name: "wal", suffix: "-wal", mode: 0o600, want: "unsupported operational sidecar -wal"},
		{name: "shared memory", suffix: "-shm", mode: 0o600, want: "unsupported operational sidecar -shm"},
		{name: "nonprivate rollback journal", suffix: "-journal", mode: 0o640, want: "outside its private bounded ownership contract"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, checkpointPath, opts, _ := createCheckpointFixture(t, false)
			anchorPath := checkpointAnchorPath(checkpointPath)
			if err := os.Link(checkpointPath, anchorPath); err != nil {
				t.Fatal(err)
			}
			sidecarPath := anchorPath + test.suffix
			const sidecarBody = "preserve potentially recoverable state"
			if err := os.WriteFile(sidecarPath, []byte(sidecarBody), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(sidecarPath, test.mode); err != nil {
				t.Fatal(err)
			}
			resumeCheckpointError(t, opts, checkpointPath, test.want)
			body, err := os.ReadFile(sidecarPath)
			if err != nil || string(body) != sidecarBody {
				t.Fatalf("rejected sidecar body=%q err=%v", body, err)
			}
			anchorInfo, err := os.Lstat(anchorPath)
			if err != nil || !anchorInfo.Mode().IsRegular() {
				t.Fatalf("rejected sidecar lost operational anchor info=%+v err=%v", anchorInfo, err)
			}
		})
	}
}

func TestCheckpointCleansContainedColdRollbackJournalWithoutSourceWork(t *testing.T) {
	_, checkpointPath, opts, original := createCheckpointFixture(t, false)
	anchorPath := checkpointAnchorPath(checkpointPath)
	if err := os.Link(checkpointPath, anchorPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchorPath+"-journal", []byte("cold rollback journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	resume := opts
	resume.CheckpointPath = ""
	resume.ResumeCheckpointPath = checkpointPath
	var sourceCalls atomic.Int32
	recovered, err := execute(context.Background(), resume, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, errors.New("cold-journal cleanup must not search")
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCalls.Load() != 0 || !reportJSONEqual(recovered, original) {
		t.Fatalf("cold-journal cleanup calls=%d equal=%t", sourceCalls.Load(), reportJSONEqual(recovered, original))
	}
	if _, err := os.Lstat(anchorPath + "-journal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cold rollback journal remains: %v", err)
	}
}

const checkpointPostCommitCrashHelperEnvironment = "LYRICS_PREFLIGHT_CHECKPOINT_POST_COMMIT_CRASH_HELPER"

func TestCheckpointPostCommitCrashHelper(t *testing.T) {
	if os.Getenv(checkpointPostCommitCrashHelperEnvironment) != "1" {
		return
	}
	catalogPath := os.Getenv(checkpointPostCommitCrashHelperEnvironment + "_CATALOG")
	checkpointPath := os.Getenv(checkpointPostCommitCrashHelperEnvironment + "_PATH")
	opts := testOptions(catalogPath)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	opts.CheckpointPath = checkpointPath
	candidate := candidateFor(1, "post-commit-crash")
	checkpointAfterResultCommitHook = func(_, _ string) { os.Exit(0) }
	_, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return fixedFor(candidate, "post-commit crash lyric"), nil
		},
	}, func() time.Time { return time.Unix(1450, 0).UTC() })
	if err != nil {
		os.Exit(21)
	}
	os.Exit(22)
}

func TestCheckpointRecoversTransactionCommittedImmediatelyBeforeProcessCrash(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "post-commit-crash")})
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	command := exec.Command(os.Args[0], "-test.run=^TestCheckpointPostCommitCrashHelper$")
	command.Env = append(os.Environ(),
		checkpointPostCommitCrashHelperEnvironment+"=1",
		checkpointPostCommitCrashHelperEnvironment+"_CATALOG="+catalogPath,
		checkpointPostCommitCrashHelperEnvironment+"_PATH="+checkpointPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("post-commit crash helper error=%v output=%s", err, output)
	}
	anchorPath := checkpointAnchorPath(checkpointPath)
	visibleInfo, visibleErr := os.Lstat(checkpointPath)
	anchorInfo, anchorErr := os.Lstat(anchorPath)
	if visibleErr != nil || anchorErr != nil || !os.SameFile(visibleInfo, anchorInfo) ||
		checkpointLinkCount(visibleInfo) != 2 || visibleInfo.Mode().Perm() != 0o600 {
		t.Fatalf("post-commit crash checkpoint visible=%+v anchor=%+v errors=%v/%v",
			visibleInfo, anchorInfo, visibleErr, anchorErr)
	}
	for _, basePath := range []string{checkpointPath, anchorPath} {
		for _, suffix := range sqliteSidecarSuffixes {
			if _, err := os.Lstat(basePath + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("post-commit crash sidecar %s: %v", basePath+suffix, err)
			}
		}
	}

	resume := testOptions(catalogPath)
	resume.Concurrency = 1
	resume.MaxAttempts = 1
	resume.ResumeCheckpointPath = checkpointPath
	var sourceCalls atomic.Int32
	recovered, err := execute(context.Background(), resume, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, errors.New("post-commit crash recovery must not repeat provider work")
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCalls.Load() != 0 || len(recovered.UniqueComplete) != 1 || recovered.EvidenceReceipt == nil {
		t.Fatalf("post-commit crash recovery calls=%d summary=%+v receipt=%v",
			sourceCalls.Load(), recovered.Summary, recovered.EvidenceReceipt != nil)
	}
	if _, err := os.Lstat(anchorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("post-commit crash recovery left anchor: %v", err)
	}
	finalInfo, err := os.Lstat(checkpointPath)
	if err != nil || finalInfo.Mode().Perm() != 0o600 || checkpointLinkCount(finalInfo) != 1 || finalInfo.Size() > maxCheckpointBytes {
		t.Fatalf("post-commit recovered checkpoint info=%+v links=%d err=%v", finalInfo, checkpointLinkCount(finalInfo), err)
	}
}

const checkpointCrashHelperEnvironment = "LYRICS_PREFLIGHT_CHECKPOINT_CRASH_HELPER"

func TestCheckpointHotJournalCrashHelper(t *testing.T) {
	if os.Getenv(checkpointCrashHelperEnvironment) != "1" {
		return
	}
	catalogPath := os.Getenv(checkpointCrashHelperEnvironment + "_CATALOG")
	checkpointPath := os.Getenv(checkpointCrashHelperEnvironment + "_PATH")
	catalog, err := loadCatalog(context.Background(), catalogPath)
	if err != nil {
		os.Exit(10)
	}
	records := make([]model.CatalogLyricsGroupingRecord, len(catalog))
	for index, item := range catalog {
		records[index] = model.CatalogLyricsGroupingRecord{
			MusicID: item.MusicID, Fingerprint: item.CatalogFingerprint, Evidence: item.Evidence,
		}
	}
	opts := testOptions(catalogPath)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	checkpoint, err := openPreflightCheckpoint(context.Background(), checkpointPath, opts, catalog,
		model.ClassifyCatalogLyricsTargets(records))
	if err != nil {
		os.Exit(11)
	}
	if _, err := checkpoint.database.Exec(`PRAGMA cache_size=1`); err != nil {
		os.Exit(12)
	}
	if _, err := checkpoint.database.Exec(`PRAGMA cache_spill=ON`); err != nil {
		os.Exit(13)
	}
	transaction, err := checkpoint.database.BeginTx(context.Background(), nil)
	if err != nil {
		os.Exit(14)
	}
	if _, err := transaction.Exec(`UPDATE evidence SET evidence_json=zeroblob(length(evidence_json))`); err != nil {
		os.Exit(15)
	}
	if _, err := transaction.Exec(`UPDATE results SET result_json=zeroblob(length(result_json))`); err != nil {
		os.Exit(16)
	}
	journalPath := checkpoint.operationalPath + "-journal"
	journalInfo, err := os.Lstat(journalPath)
	if err != nil || !journalInfo.Mode().IsRegular() || journalInfo.Size() <= 512 {
		os.Exit(17)
	}
	header := make([]byte, 8)
	journal, err := os.Open(journalPath)
	if err != nil {
		os.Exit(18)
	}
	read, readErr := io.ReadFull(journal, header)
	_ = journal.Close()
	if readErr != nil || read != len(header) || bytes.Equal(header, make([]byte, len(header))) {
		os.Exit(19)
	}
	_ = transaction
	os.Exit(0)
}

func TestCheckpointRecoversContainedHotJournalAfterProcessCrash(t *testing.T) {
	catalogPath := writeV18Catalog(t, []catalogFixture{completeCatalogFixture(1, "hot-journal-crash")})
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	opts := testOptions(catalogPath)
	opts.Concurrency = 1
	opts.MaxAttempts = 1
	opts.CheckpointPath = checkpointPath
	candidate := attachCandidateRevisionEvidence(candidateFor(1, "hot-journal-crash"), bytes.Repeat([]byte("h"), 1<<20))
	original, err := execute(context.Background(), opts, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			return []lyricssource.Candidate{candidate}, nil
		},
		fetchCandidate: func(context.Context, lyricssource.MusicIdentity, lyricssource.Candidate) (lyricssource.FixedRevision, error) {
			return fixedFor(candidate, "hot journal recovery lyric"), nil
		},
	}, func() time.Time { return time.Unix(1500, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCheckpointHotJournalCrashHelper$")
	command.Env = append(os.Environ(),
		checkpointCrashHelperEnvironment+"=1",
		checkpointCrashHelperEnvironment+"_CATALOG="+catalogPath,
		checkpointCrashHelperEnvironment+"_PATH="+checkpointPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("crash helper error=%v output=%s", err, output)
	}
	anchorPath := checkpointAnchorPath(checkpointPath)
	visibleInfo, visibleErr := os.Lstat(checkpointPath)
	anchorInfo, anchorErr := os.Lstat(anchorPath)
	journalInfo, journalErr := os.Lstat(anchorPath + "-journal")
	if visibleErr != nil || anchorErr != nil || journalErr != nil ||
		!os.SameFile(visibleInfo, anchorInfo) || checkpointLinkCount(visibleInfo) != 2 ||
		!journalInfo.Mode().IsRegular() || journalInfo.Mode().Perm() != 0o600 || journalInfo.Size() <= 512 {
		t.Fatalf("crashed checkpoint visible=%+v anchor=%+v journal=%+v errors=%v/%v/%v",
			visibleInfo, anchorInfo, journalInfo, visibleErr, anchorErr, journalErr)
	}
	resume := opts
	resume.CheckpointPath = ""
	resume.ResumeCheckpointPath = checkpointPath
	var sourceCalls atomic.Int32
	recovered, err := execute(context.Background(), resume, fakeSourceClient{
		search: func(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error) {
			sourceCalls.Add(1)
			return nil, errors.New("hot-journal recovery must restore the committed result")
		},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if sourceCalls.Load() != 0 || !reportJSONEqual(recovered, original) {
		t.Fatalf("hot-journal recovery calls=%d equal=%t", sourceCalls.Load(), reportJSONEqual(recovered, original))
	}
	if _, err := os.Lstat(anchorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered operational anchor remains: %v", err)
	}
	if _, err := os.Lstat(anchorPath + "-journal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered hot journal remains: %v", err)
	}
	info, err := os.Lstat(checkpointPath)
	if err != nil || info.Mode().Perm() != 0o600 || checkpointLinkCount(info) != 1 {
		t.Fatalf("recovered standalone checkpoint info=%+v links=%d err=%v", info, checkpointLinkCount(info), err)
	}
}
