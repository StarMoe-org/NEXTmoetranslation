package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"moesekai/server/offline/internal/lyricsstaging"
)

const maxManifestBytes = 256 << 20

type manifestInput interface {
	io.Reader
	io.Seeker
	Stat() (os.FileInfo, error)
	Close() error
}

type manifestOpener func(string) (manifestInput, error)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lyrics validate: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("lyrics-validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inputPath := flags.String("input", "", "local lyrics staging manifest to check for self-consistency and integrity")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*inputPath) == "" {
		return errors.New("-input is required")
	}
	if *inputPath != strings.TrimSpace(*inputPath) || *inputPath == "-" {
		return errors.New("-input must identify an existing regular file without surrounding whitespace")
	}
	manifest, err := loadManifest(*inputPath)
	if err != nil {
		return err
	}
	if stdout == nil {
		return errors.New("stdout writer is required")
	}
	draftNoun := "drafts"
	if len(manifest.Items) == 1 {
		draftNoun = "draft"
	}
	_, err = fmt.Fprintf(stdout, "verified staging manifest self-consistency and integrity for %d staged %s; did not independently verify fixed-revision provenance; batch sha256:%s\n", len(manifest.Items), draftNoun, manifest.BatchSHA256)
	return err
}

// loadManifest checks the manifest's closed schema, internal relationships, and
// embedded digests. It does not independently fetch or attest the fixed-revision
// provenance described by the manifest.
func loadManifest(path string) (lyricsstaging.Manifest, error) {
	return loadManifestWithOpener(path, func(path string) (manifestInput, error) {
		return os.Open(path)
	})
}

func loadManifestWithOpener(path string, openFile manifestOpener) (lyricsstaging.Manifest, error) {
	if openFile == nil {
		return lyricsstaging.Manifest{}, errors.New("staging manifest opener is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return lyricsstaging.Manifest{}, fmt.Errorf("resolve staging manifest path: %w", err)
	}
	inspectedInfo, err := os.Stat(absolute)
	if err != nil {
		return lyricsstaging.Manifest{}, fmt.Errorf("inspect staging manifest: %w", err)
	}
	if !inspectedInfo.Mode().IsRegular() || inspectedInfo.Size() <= 0 || inspectedInfo.Size() > maxManifestBytes {
		return lyricsstaging.Manifest{}, fmt.Errorf("staging manifest must be a regular file between 1 and %d bytes", maxManifestBytes)
	}

	file, err := openFile(absolute)
	if err != nil {
		return lyricsstaging.Manifest{}, fmt.Errorf("open staging manifest: %w", err)
	}
	if file == nil {
		return lyricsstaging.Manifest{}, errors.New("open staging manifest: opener returned no file")
	}
	defer file.Close()

	if err := revalidateManifestInput(absolute, file, inspectedInfo, "between inspection and open"); err != nil {
		return lyricsstaging.Manifest{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return lyricsstaging.Manifest{}, fmt.Errorf("rewind staging manifest before read: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return lyricsstaging.Manifest{}, fmt.Errorf("read staging manifest: %w", err)
	}
	if len(body) > maxManifestBytes {
		return lyricsstaging.Manifest{}, fmt.Errorf("staging manifest exceeds %d bytes", maxManifestBytes)
	}
	if int64(len(body)) != inspectedInfo.Size() {
		return lyricsstaging.Manifest{}, errors.New("staging manifest changed while being read")
	}
	bodyDigest := sha256.Sum256(body)

	if err := revalidateManifestInput(absolute, file, inspectedInfo, "while being read"); err != nil {
		return lyricsstaging.Manifest{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return lyricsstaging.Manifest{}, fmt.Errorf("rewind staging manifest for verification: %w", err)
	}
	verificationDigest, verificationSize, err := digestManifestInput(file)
	if err != nil {
		return lyricsstaging.Manifest{}, fmt.Errorf("re-read staging manifest for verification: %w", err)
	}
	if verificationSize > maxManifestBytes {
		return lyricsstaging.Manifest{}, fmt.Errorf("staging manifest exceeds %d bytes", maxManifestBytes)
	}
	if verificationSize != inspectedInfo.Size() {
		return lyricsstaging.Manifest{}, errors.New("staging manifest changed while being read")
	}
	if bodyDigest != verificationDigest {
		return lyricsstaging.Manifest{}, errors.New("staging manifest contents changed while being read")
	}
	if err := revalidateManifestInput(absolute, file, inspectedInfo, "while being read"); err != nil {
		return lyricsstaging.Manifest{}, err
	}

	return lyricsstaging.DecodeManifest(body)
}

func digestManifestInput(file manifestInput) ([sha256.Size]byte, int64, error) {
	hasher := sha256.New()
	readBytes, err := io.Copy(hasher, io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return [sha256.Size]byte{}, readBytes, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, readBytes, nil
}

func revalidateManifestInput(path string, file manifestInput, inspectedInfo os.FileInfo, stage string) error {
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened staging manifest: %w", err)
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("reinspect staging manifest path: %w", err)
	}
	if !openedInfo.Mode().IsRegular() ||
		!pathInfo.Mode().IsRegular() ||
		!os.SameFile(inspectedInfo, openedInfo) ||
		!os.SameFile(openedInfo, pathInfo) ||
		openedInfo.Size() != inspectedInfo.Size() ||
		pathInfo.Size() != inspectedInfo.Size() ||
		!openedInfo.ModTime().Equal(inspectedInfo.ModTime()) ||
		!pathInfo.ModTime().Equal(inspectedInfo.ModTime()) {
		return fmt.Errorf("staging manifest changed %s", stage)
	}
	return nil
}
