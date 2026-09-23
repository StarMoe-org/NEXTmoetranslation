package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"moesekai/server/internal/httpx"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsprovidercoord"
	"moesekai/server/offline/internal/lyricsproviderpolicy"
)

const authorization = "MOESEKAI_SEKAIPEDIA_URL_PREFLIGHT_AUTHORIZED"

var canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type options struct {
	listResponsePath    string
	expectedListSHA256  string
	expectedTargetCount int
	outputRoot          string
	authorization       string
	crawlDelay          time.Duration
	requestTimeout      time.Duration
	maxAttempts         int
	retryDelay          time.Duration
}

type report struct {
	SchemaVersion      int                                        `json:"schemaVersion"`
	Provider           string                                     `json:"provider"`
	ListResponseSHA256 string                                     `json:"listResponseSha256"`
	TargetCount        int                                        `json:"targetCount"`
	CheckedAt          string                                     `json:"checkedAt"`
	AllURLsExist       bool                                       `json:"allUrlsExist"`
	Failure            string                                     `json:"failure,omitempty"`
	Statuses           []lyricssource.SekaipediaPageURLStatus     `json:"statuses"`
	Batches            []lyricssource.SekaipediaURLPreflightBatch `json:"batches"`
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) (resultErr error) {
	parsed, err := parseOptions(arguments)
	if err != nil {
		return err
	}
	if parsed.authorization != authorization {
		_, writeErr := fmt.Fprintln(output, "HOLD mode=sekaipedia-url-preflight authorization=missing network=HOLD")
		return writeErr
	}
	listRaw, err := readPinnedInput(parsed.listResponsePath, parsed.expectedListSHA256)
	if err != nil {
		return err
	}
	targets, err := lyricssource.SekaipediaListPageURLTargets(listRaw)
	if err != nil {
		return err
	}
	if len(targets) != parsed.expectedTargetCount {
		return fmt.Errorf("Sekaipedia List linked target count=%d, want %d", len(targets), parsed.expectedTargetCount)
	}
	if err := createPrivateOutputRoot(parsed.outputRoot); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(parsed.outputRoot)
		}
	}()

	owner, err := lyricsprovidercoord.AcquireDefault()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Close()) }()
	client := httpx.NewUpstreamClientWithOptions(httpx.UpstreamClientOptions{
		Timeout: parsed.requestTimeout, DialTimeout: 10 * time.Second,
		TLSHandshakeTimeout: 12 * time.Second, ResponseHeaderTimeout: 20 * time.Second,
		Policy: httpx.UpstreamPolicyFromEnvironment(), AllowQuery: true,
	})
	wrapped, err := owner.Wrap(lyricsproviderpolicy.ProviderSekaipedia, client.Transport)
	if err != nil {
		return err
	}
	client.Transport = wrapped
	statuses, batches, preflightErr := lyricssource.PreflightSekaipediaPageURLs(
		ctx, targets, client, parsed.crawlDelay, 10*time.Second,
		parsed.maxAttempts, parsed.retryDelay,
	)
	resolveErr := owner.ResolveProvider(lyricsproviderpolicy.ProviderSekaipedia)
	responsesRoot := filepath.Join(parsed.outputRoot, "responses")
	if err := os.Mkdir(responsesRoot, 0o700); err != nil {
		return err
	}
	for index := range batches {
		path := filepath.Join(responsesRoot, fmt.Sprintf("batch-%04d.json", index+1))
		if err := writePrivateExclusive(path, batches[index].Raw); err != nil {
			return err
		}
		batches[index].Raw = nil
	}
	combinedErr := errors.Join(preflightErr, resolveErr)
	result := report{
		SchemaVersion: 1, Provider: "sekaipedia",
		ListResponseSHA256: parsed.expectedListSHA256,
		TargetCount:        len(statuses), CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
		AllURLsExist: combinedErr == nil, Statuses: statuses, Batches: batches,
	}
	if combinedErr != nil {
		result.Failure = combinedErr.Error()
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := writePrivateExclusive(filepath.Join(parsed.outputRoot, "report.json"), append(body, '\n')); err != nil {
		return err
	}
	root, err := os.Open(parsed.outputRoot)
	if err != nil {
		return err
	}
	if err := root.Sync(); err != nil {
		_ = root.Close()
		return err
	}
	if err := root.Close(); err != nil {
		return err
	}
	cleanup = false
	if combinedErr != nil {
		return combinedErr
	}
	_, err = fmt.Fprintf(output, "PASS mode=sekaipedia-url-preflight targets=%d batches=%d allURLsExist=true output=%s\n",
		len(statuses), len(batches), parsed.outputRoot)
	return err
}

func parseOptions(arguments []string) (options, error) {
	var parsed options
	flags := flag.NewFlagSet("sekaipedia-url-preflight", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&parsed.listResponsePath, "list-response", "", "exact Sekaipedia List API response")
	flags.StringVar(&parsed.expectedListSHA256, "expected-list-sha256", "", "exact List response SHA-256")
	flags.IntVar(&parsed.expectedTargetCount, "expected-target-count", 0, "exact linked target count")
	flags.StringVar(&parsed.outputRoot, "output", "", "create-exclusive private output root")
	flags.StringVar(&parsed.authorization, "authorization", "", "explicit live URL-preflight authorization")
	flags.DurationVar(&parsed.crawlDelay, "crawl-delay", 10*time.Second, "minimum start interval per provider request")
	flags.DurationVar(&parsed.requestTimeout, "request-timeout", 2*time.Minute, "per-request timeout")
	flags.IntVar(&parsed.maxAttempts, "max-attempts", 5, "bounded retry attempts")
	flags.DurationVar(&parsed.retryDelay, "retry-delay", 30*time.Second, "additional retry delay")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return options{}, errors.New("Sekaipedia URL preflight accepts only explicit named flags")
	}
	for _, path := range []string{parsed.listResponsePath, parsed.outputRoot} {
		if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return options{}, errors.New("Sekaipedia URL preflight paths must be canonical and absolute")
		}
	}
	if !canonicalSHA256.MatchString(parsed.expectedListSHA256) || parsed.expectedTargetCount < 1 ||
		parsed.expectedTargetCount > 10_000 || parsed.crawlDelay < 10*time.Second ||
		parsed.requestTimeout <= 0 || parsed.maxAttempts < 1 || parsed.maxAttempts > 5 || parsed.retryDelay < 0 {
		return options{}, errors.New("Sekaipedia URL preflight bounds are invalid")
	}
	return parsed, nil
}

func readPinnedInput(path, expectedSHA256 string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Type() != 0 || info.Size() <= 0 || info.Size() > lyricssource.MaxIndexEvidenceRawBytes {
		return nil, errors.New("Sekaipedia List response input is not a bounded regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	actual := hex.EncodeToString(digest[:])
	if actual != expectedSHA256 {
		return nil, fmt.Errorf("Sekaipedia List response SHA-256=%s, want %s", actual, expectedSHA256)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return nil, errors.New("Sekaipedia List response changed while being read")
	}
	return body, nil
}

func createPrivateOutputRoot(path string) error {
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0o700 {
		return errors.New("Sekaipedia URL preflight output parent must exist with mode 0700")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return fmt.Errorf("create URL preflight output root: %w", err)
	}
	return nil
}

func writePrivateExclusive(path string, body []byte) error {
	if len(body) == 0 {
		return errors.New("refusing to publish an empty URL preflight artifact")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(body)
	if writeErr == nil && written != len(body) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	return errors.Join(writeErr, file.Close())
}
