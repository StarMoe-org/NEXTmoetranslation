package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"moesekai/server/offline/internal/lyricsextractionplan"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lyrics extraction plan check: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("lyrics-extraction-plan-check", flag.ContinueOnError)
	if stdout == nil {
		flags.SetOutput(io.Discard)
	} else {
		flags.SetOutput(stdout)
	}
	planPath := flags.String("plan", "", "canonical extraction-plan v1 JSON file")
	expectedSHA256 := flags.String("expected-sha256", "", "independently supplied lowercase SHA-256 of the exact canonical plan bytes")
	root := flags.String("root", "", "local directory containing every declared catalog/resume and source-snapshot file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{name: "-plan", value: *planPath},
		{name: "-expected-sha256", value: *expectedSHA256},
		{name: "-root", value: *root},
	} {
		if required.value == "" {
			return fmt.Errorf("%s is required", required.name)
		}
		if required.value != strings.TrimSpace(required.value) {
			return fmt.Errorf("%s must not have surrounding whitespace", required.name)
		}
	}
	body, err := readBoundedPlan(*planPath)
	if err != nil {
		return err
	}
	plan, digest, err := lyricsextractionplan.Check(body, *expectedSHA256)
	if err != nil {
		return err
	}
	if err := lyricsextractionplan.VerifyDeclaredFiles(*root, plan); err != nil {
		return err
	}
	if stdout != nil {
		_, err = fmt.Fprintf(stdout, "valid extraction-plan v1 %s\n", digest)
	}
	return err
}

func readBoundedPlan(planPath string) ([]byte, error) {
	info, err := os.Lstat(planPath)
	if err != nil {
		return nil, fmt.Errorf("inspect plan: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("plan must be a regular file, not a symlink")
	}
	if info.Size() <= 0 || info.Size() > lyricsextractionplan.MaxPlanBytes {
		return nil, fmt.Errorf("plan must contain between 1 and %d bytes", lyricsextractionplan.MaxPlanBytes)
	}
	file, err := os.Open(planPath)
	if err != nil {
		return nil, fmt.Errorf("open plan: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened plan: %w", err)
	}
	if !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() {
		return nil, errors.New("plan path identity or size changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, lyricsextractionplan.MaxPlanBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	if len(body) != int(info.Size()) || len(body) > lyricsextractionplan.MaxPlanBytes {
		return nil, errors.New("plan size changed while reading")
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect read plan: %w", err)
	}
	if !os.SameFile(openedInfo, finalInfo) || finalInfo.Size() != openedInfo.Size() ||
		!finalInfo.ModTime().Equal(openedInfo.ModTime()) {
		return nil, errors.New("plan identity, size, or modification time changed while reading")
	}
	return body, nil
}
