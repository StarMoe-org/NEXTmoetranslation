package main

import (
	"context"

	"errors"
	"fmt"

	"os"
	"path/filepath"

	"strings"

	"golang.org/x/sys/unix"

	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsextractionplan"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsrecovery"
)

func readPinnedPrivateFile(path string, maximum int) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("private input byte boundary is invalid")
	}
	pinned, err := openPinnedRegularFile(path, regularFilePolicy{
		label: "pinned private input", exactPermissions: 0o600, requireExactMode: true,
		requirePrivate: true, maximum: int64(maximum),
	})
	if err != nil {
		return nil, err
	}
	defer pinned.Close()
	return pinned.readAll()
}

func verifyPinnedRecoverySourceSnapshot(
	rootPath string,
	plan lyricsextractionplan.RecoveryPlan,
	inspection bool,
) error {
	verify := lyricsextractionplan.VerifyRecoverySourceSnapshot
	if inspection {
		verify = lyricsextractionplan.VerifyRecoverySourceSnapshotForInspection
	}
	if err := verify(rootPath, plan); err != nil {
		return err
	}
	root, err := openCanonicalDirectory(rootPath, directoryPolicy{
		label: "recovery source root", effectiveUID: true,
	})
	if err != nil {
		return err
	}
	defer root.Close()
	for _, source := range plan.SourceSnapshot.Files {
		if err := verifyPinnedSourceFile(root, source); err != nil {
			return fmt.Errorf("verify recovery source snapshot file %q: %w", source.Path, err)
		}
	}
	if err := verify(rootPath, plan); err != nil {
		return err
	}
	return root.verify()
}

func verifyPinnedSourceFile(root *pinnedDirectory, source lyricsextractionplan.SourceFileIdentity) error {
	relative := filepath.FromSlash(source.Path)
	if source.Path == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == "." {
		return errors.New("source snapshot path is not canonical and relative")
	}
	components := strings.Split(relative, string(os.PathSeparator))
	current := root
	ownedCurrent := false
	defer func() {
		if ownedCurrent {
			_ = current.Close()
		}
	}()
	for _, component := range components[:len(components)-1] {
		nextPath := filepath.Join(current.path, component)
		next, err := openDirectoryAt(current, component, nextPath, directoryPolicy{
			label: "recovery source directory", effectiveUID: true,
		})
		if err != nil {
			return err
		}
		if ownedCurrent {
			_ = current.Close()
		}
		current = next
		ownedCurrent = true
	}
	leaf := components[len(components)-1]
	path := filepath.Join(current.path, leaf)
	pinned, err := openPinnedRegularFileAt(current, leaf, path, regularFilePolicy{
		label: "recovery source file", maximum: lyricsextractionplan.MaxSourceFileBytes,
	})
	if err != nil {
		return err
	}
	defer pinned.file.Close()
	digest, err := pinned.sha256(source.SizeBytes)
	if err != nil {
		return err
	}
	if digest != source.SHA256 {
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", digest, source.SHA256)
	}
	return nil
}

func openCheckedRecoveryLedger(ctx context.Context, path string) (*checkedRecoveryLedger, error) {
	root, err := openCanonicalDirectory(path, directoryPolicy{
		label: "acquisition ledger root", private: true, effectiveUID: true,
	})
	if err != nil {
		return nil, err
	}
	ledger, err := lyricsacquisition.OpenLedger(ctx, path)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	checked := &checkedRecoveryLedger{ledger: ledger, root: root}
	if err := checked.verify(); err != nil {
		_ = checked.Close()
		return nil, err
	}
	return checked, nil
}

func (ledger *checkedRecoveryLedger) verify() error {
	if ledger == nil || ledger.ledger == nil || ledger.root == nil {
		return errors.New("checked acquisition ledger is not open")
	}
	return ledger.root.verify()
}

func (ledger *checkedRecoveryLedger) Close() error {
	if ledger == nil {
		return nil
	}
	var result error
	if ledger.ledger != nil {
		result = errors.Join(result, ledger.ledger.Close())
		ledger.ledger = nil
	}
	if ledger.root != nil {
		result = errors.Join(result, ledger.root.Close())
		ledger.root = nil
	}
	return result
}

func openCheckedRecoveryCatalog(
	ctx context.Context,
	path string,
	binding lyricsextractionplan.RecoveryCatalogBinding,
) (*checkedRecoveryCatalog, error) {
	pinned, err := openPinnedRegularFile(path, regularFilePolicy{
		label: "immutable recovery catalog", exactPermissions: 0o444, requireExactMode: true,
		maximum: lyricsextractionplan.MaxCatalogDatabaseBytes,
	})
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*checkedRecoveryCatalog, error) {
		_ = pinned.Close()
		return nil, cause
	}
	digest, err := pinned.sha256(binding.SizeBytes)
	if err != nil {
		return fail(err)
	}
	if digest != binding.SourceSHA256 {
		return fail(errors.New("immutable recovery catalog SHA-256 does not match the plan"))
	}
	catalog, _, err := lyricsrecovery.OpenCatalogAgainstPinnedFile(ctx, path, binding, pinned.file)
	if err != nil {
		return fail(err)
	}
	if err := pinned.verify(); err != nil {
		_ = catalog.Close()
		return fail(err)
	}
	return &checkedRecoveryCatalog{catalog: catalog, pinned: pinned}, nil
}

func (catalog *checkedRecoveryCatalog) MusicIdentity(ctx context.Context, musicID int) (lyricssource.MusicIdentity, error) {
	if catalog == nil || catalog.catalog == nil || catalog.pinned == nil {
		return lyricssource.MusicIdentity{}, errors.New("checked recovery catalog is not open")
	}
	if err := catalog.pinned.verify(); err != nil {
		return lyricssource.MusicIdentity{}, err
	}
	identity, err := catalog.catalog.MusicIdentity(ctx, musicID)
	if err != nil {
		return lyricssource.MusicIdentity{}, err
	}
	if err := catalog.pinned.verify(); err != nil {
		return lyricssource.MusicIdentity{}, err
	}
	return identity, nil
}

func (catalog *checkedRecoveryCatalog) Close() error {
	if catalog == nil {
		return nil
	}
	var result error
	if catalog.catalog != nil {
		result = errors.Join(result, catalog.catalog.Close())
		catalog.catalog = nil
	}
	if catalog.pinned != nil {
		result = errors.Join(result, catalog.pinned.Close())
		catalog.pinned = nil
	}
	return result
}

func requireAbsentWithPrivateParent(path string) error {
	parent, err := openCanonicalDirectory(filepath.Dir(path), directoryPolicy{
		label: "lyrics recovery output parent", private: true, effectiveUID: true,
	})
	if err != nil {
		return err
	}
	defer parent.Close()
	var stat unix.Stat_t
	err = unix.Fstatat(int(parent.file.Fd()), filepath.Base(path), &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return errors.New("lyrics recovery refuses an existing output")
	}
	if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if _, pathErr := os.Lstat(path); !errors.Is(pathErr, os.ErrNotExist) {
		if pathErr == nil {
			return errors.New("lyrics recovery refuses an existing output")
		}
		return pathErr
	}
	return parent.verify()
}
