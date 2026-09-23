package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsevidencepack"
)

type options struct {
	LedgerRoot      string
	SelectionPath   string
	OutputDirectory string
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("lyrics-evidence-pack", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.LedgerRoot, "ledger-root", "", "validated private acquisition ledger root")
	flags.StringVar(&opts.SelectionPath, "selection", "", "private canonical exact acquisition/evidence selection JSON")
	flags.StringVar(&opts.OutputDirectory, "output-dir", "", "new or recoverable private evidence pack directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || invalidPathOption(opts.LedgerRoot) || invalidPathOption(opts.SelectionPath) || invalidPathOption(opts.OutputDirectory) {
		return errors.New("-ledger-root, -selection, and -output-dir are required without positional arguments")
	}
	selectionBody, err := readPrivateInput(opts.SelectionPath, lyricsevidencepack.MaxManifestBytes)
	if err != nil {
		return fmt.Errorf("read evidence selection: %w", err)
	}
	selection, err := lyricsevidencepack.DecodeSelection(selectionBody)
	if err != nil {
		return err
	}
	ledger, err := lyricsacquisition.OpenLedger(context.Background(), opts.LedgerRoot)
	if err != nil {
		return err
	}
	manifest, buildErr := lyricsevidencepack.Build(context.Background(), opts.OutputDirectory, selection.Evidence, ledger)
	closeErr := ledger.Close()
	if buildErr != nil {
		return buildErr
	}
	if closeErr != nil {
		return fmt.Errorf("close acquisition ledger: %w", closeErr)
	}
	_, err = fmt.Fprintf(stdout, "published evidence pack %s with %d items in %d shards\n",
		manifest.PackSHA256, manifest.Totals.ItemCount, manifest.Totals.ShardCount)
	return err
}

func invalidPathOption(value string) bool {
	return value == "" || strings.TrimSpace(value) != value
}

func readPrivateInput(path string, maximum int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !validPrivateInputInfo(info) || info.Size() <= 0 || info.Size() > int64(maximum) {
		return nil, errors.New("private input must be a direct effective-UID-owned single-link mode-0600 regular file within its byte bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(info, opened) || !os.SameFile(info, pathInfo) {
		return nil, errors.New("private input changed while being opened")
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	after, statErr := file.Stat()
	afterPath, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !os.SameFile(info, after) || !os.SameFile(info, afterPath) ||
		!validPrivateInputInfo(after) || len(body) > maximum || int64(len(body)) != info.Size() {
		return nil, errors.New("private input changed or exceeded its byte bound while being read")
	}
	return body, nil
}

func validPrivateInputInfo(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && stat.Nlink == 1
}
