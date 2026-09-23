package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"net/url"
	"os"
	"path/filepath"

	"sort"
	"strings"
	"syscall"

	"moesekai/server/internal/model"
)

// openCheckpointPrivatePath establishes the local threat model used by the
// checkpoint VFS containment: root and the current effective UID are trusted;
// other local UIDs without an independent ACL grant are not. Every resolved
// ancestor must be owned by one of those trusted UIDs, and any group/other-
// writable ancestor must be sticky. The immediate run directory must be
// current-EUID-owned and not writable by group or other. This makes its entries
// non-replaceable by a principal outside that set while allowing normal
// /tmp-style sticky ancestors. Root, same-EUID code, and any principal granted
// directory mutation rights by a platform ACL are inside the trust boundary;
// the operator must therefore use an ACL-reviewed private run directory.
func openCheckpointPrivatePath(path string) (*checkpointPrivatePath, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoint path: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoint private run directory: %w", err)
	}
	resolvedParent, err = filepath.Abs(resolvedParent)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoint private run directory absolutely: %w", err)
	}
	if err := validateCheckpointAncestorChain(resolvedParent); err != nil {
		return nil, err
	}
	parentFile, err := os.Open(resolvedParent)
	if err != nil {
		return nil, fmt.Errorf("open checkpoint private run directory: %w", err)
	}
	fail := func(err error) (*checkpointPrivatePath, error) {
		_ = parentFile.Close()
		return nil, err
	}
	parentInfo, err := parentFile.Stat()
	if err != nil {
		return fail(fmt.Errorf("inspect opened checkpoint private run directory: %w", err))
	}
	pathInfo, err := os.Stat(resolvedParent)
	if err != nil || !os.SameFile(parentInfo, pathInfo) || !parentInfo.IsDir() ||
		parentInfo.Mode().Perm()&0o022 != 0 {
		return fail(errors.New("checkpoint private run directory must be stable and not group/other-writable"))
	}
	owner, ok := checkpointOwnerID(parentInfo)
	if !ok || int(owner) != os.Geteuid() {
		return fail(errors.New("checkpoint private run directory must be owned by the effective UID"))
	}
	return &checkpointPrivatePath{
		path:       filepath.Join(resolvedParent, filepath.Base(absolute)),
		parentPath: resolvedParent,
		parentFile: parentFile,
		parentInfo: parentInfo,
	}, nil
}

func validateCheckpointAncestorChain(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	root := volume + string(os.PathSeparator)
	relative := strings.TrimPrefix(clean, root)
	current := root
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect checkpoint private path ancestry: %w", err)
		}
		owner, ok := checkpointOwnerID(info)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			owner != 0 && int(owner) != os.Geteuid() {
			return errors.New("checkpoint private path ancestry is not owned by root or the effective UID")
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return errors.New("checkpoint private path ancestry is writable by an untrusted local UID")
		}
	}
	return nil
}

func (private *checkpointPrivatePath) Close() error {
	if private == nil || private.parentFile == nil {
		return nil
	}
	err := private.parentFile.Close()
	private.parentFile = nil
	return err
}

func checkpointAnchorPath(path string) string {
	digest := sha256.Sum256([]byte(filepath.Base(path)))
	return filepath.Join(filepath.Dir(path), ".lyrics-preflight-"+hex.EncodeToString(digest[:16])+".anchor")
}

func checkpointPathFamily(path string) []string {
	anchorPath := checkpointAnchorPath(path)
	paths := []string{path, anchorPath}
	for _, basePath := range []string{path, anchorPath} {
		for _, suffix := range sqliteSidecarSuffixes {
			paths = append(paths, basePath+suffix)
		}
	}
	return paths
}

func resolvedCheckpointEntryPath(path, label string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", label, err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve %s parent: %w", label, err)
	}
	return filepath.Clean(filepath.Join(resolvedParent, filepath.Base(absolute))), nil
}

func rejectCheckpointPathAliases(checkpointPath string, opts options) error {
	protected := checkpointPathFamily(checkpointPath)
	protectedResolved := make([]string, len(protected))
	for index, path := range protected {
		resolved, err := resolvedCheckpointEntryPath(path, "checkpoint path family")
		if err != nil {
			return err
		}
		protectedResolved[index] = resolved
	}
	others := []struct {
		label string
		path  string
	}{
		{label: "database", path: opts.DatabasePath},
		{label: "final output", path: opts.OutputPath},
	}
	for _, other := range others {
		if other.path == "" {
			continue
		}
		resolved, err := resolvedCheckpointEntryPath(other.path, other.label)
		if err != nil {
			return err
		}
		for _, protectedPath := range protectedResolved {
			if resolved == protectedPath {
				return fmt.Errorf("%s path must not alias the checkpoint, operational anchor, or any SQLite sidecar", other.label)
			}
		}
		otherInfo, err := os.Stat(other.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s path for checkpoint aliasing: %w", other.label, err)
		}
		if err != nil {
			continue
		}
		for _, protectedPath := range protected {
			protectedInfo, protectedErr := os.Stat(protectedPath)
			if protectedErr != nil && !errors.Is(protectedErr, os.ErrNotExist) {
				return fmt.Errorf("inspect checkpoint path family for %s aliasing: %w", other.label, protectedErr)
			}
			if protectedErr == nil && os.SameFile(otherInfo, protectedInfo) {
				return fmt.Errorf("%s path must not resolve to the checkpoint, operational anchor, or any SQLite sidecar", other.label)
			}
		}
	}
	return nil
}

func validateCheckpointOptions(opts options) error {
	if opts.CheckpointPath != "" && opts.ResumeCheckpointPath != "" {
		return errors.New("-checkpoint and -resume-checkpoint are mutually exclusive")
	}
	checkpointPath := opts.CheckpointPath
	flagName := "-checkpoint"
	resume := false
	if opts.ResumeCheckpointPath != "" {
		checkpointPath = opts.ResumeCheckpointPath
		flagName = "-resume-checkpoint"
		resume = true
	}
	if checkpointPath == "" {
		return nil
	}
	if checkpointPath != strings.TrimSpace(checkpointPath) {
		return fmt.Errorf("%s must not have surrounding whitespace", flagName)
	}
	if checkpointPath == "-" {
		return fmt.Errorf("%s requires a private SQLite file path", flagName)
	}
	if opts.ResumeReportPath != "" {
		return errors.New("checkpoint execution must not be combined with -resume-report")
	}
	private, err := openCheckpointPrivatePath(checkpointPath)
	if err != nil {
		return err
	}
	defer private.Close()
	if err := rejectCheckpointPathAliases(private.path, opts); err != nil {
		return err
	}
	if !resume {
		if _, err := os.Lstat(private.path); err == nil {
			return errors.New("create new private checkpoint: path already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect checkpoint path: %w", err)
		}
		anchorPath := checkpointAnchorPath(private.path)
		if _, err := os.Lstat(anchorPath); err == nil {
			return errors.New("create new private checkpoint: stale operational anchor already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect checkpoint operational anchor: %w", err)
		}
		if err := rejectCheckpointSidecars(private.path); err != nil {
			return err
		}
		return rejectCheckpointSidecars(anchorPath)
	}
	file, info, anchorExists, err := openPrivateCheckpointFile(private.path, true)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := rejectCheckpointSidecars(private.path); err != nil {
		return err
	}
	if anchorExists {
		if err := validateCheckpointOperationalSidecars(checkpointAnchorPath(private.path), true); err != nil {
			return err
		}
	}
	databaseInfo, err := os.Stat(opts.DatabasePath)
	if err == nil && os.SameFile(info, databaseInfo) {
		return errors.New("checkpoint path must not resolve to the database path")
	}
	return nil
}

func checkpointExecutionBindingFor(opts options) checkpointExecutionBinding {
	return checkpointExecutionBinding{
		SchemaVersion: checkpointExecutionBindingVersion,
		Concurrency:   opts.Concurrency, MaxAttempts: opts.MaxAttempts,
		RequestTimeoutNanoseconds: int64(opts.RequestTimeout), RetryDelayNanoseconds: int64(opts.RetryDelay),
	}
}

func checkpointTargetKind(target model.CatalogLyricsTarget) (string, error) {
	switch target.Disposition {
	case model.LyricsCatalogTargetReview:
		return checkpointTargetCatalogReview, nil
	case model.LyricsCatalogTargetGameSizeEvidence:
		return checkpointTargetGameSizeEvidence, nil
	case model.LyricsCatalogTargetFullTarget:
		return checkpointTargetProviderWork, nil
	default:
		return "", fmt.Errorf("unsupported catalog target disposition %q", target.Disposition)
	}
}

func buildCheckpointBindings(opts options, catalog []catalogItem, targets []model.CatalogLyricsTarget) (
	map[int]checkpointTargetBinding, string, []byte, string, error,
) {
	if len(catalog) != len(targets) {
		return nil, "", nil, "", errors.New("checkpoint catalog and target counts differ")
	}
	itemsByID := make(map[int]catalogItem, len(catalog))
	for _, item := range catalog {
		itemsByID[item.MusicID] = item
	}
	targetsByID := make(map[int]checkpointTargetBinding, len(targets))
	records := make([]checkpointCatalogRecord, 0, len(targets))
	for _, target := range targets {
		item, found := itemsByID[target.MusicID]
		if !found {
			return nil, "", nil, "", errors.New("checkpoint target is absent from the catalog")
		}
		kind, err := checkpointTargetKind(target)
		if err != nil {
			return nil, "", nil, "", err
		}
		associations := append([]int(nil), target.AssociationMusicIDs...)
		sort.Ints(associations)
		record := checkpointCatalogRecord{
			MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle, ProducerMetadata: item.ProducerMetadata,
			Lyricist: item.Lyricist, Composer: item.Composer, Arranger: item.Arranger,
			CatalogFingerprint: item.CatalogFingerprint, Disposition: target.Disposition,
			TargetMusicID: target.TargetMusicID, AssociationMusicIDs: associations, ReasonCode: target.ReasonCode,
		}
		body, err := json.Marshal(record)
		if err != nil {
			return nil, "", nil, "", fmt.Errorf("encode checkpoint catalog binding: %w", err)
		}
		digest := sha256.Sum256(body)
		binding := checkpointTargetBinding{
			CatalogItem: item, Target: target, Kind: kind, Body: body, SHA256: hex.EncodeToString(digest[:]),
		}
		if _, duplicate := targetsByID[target.MusicID]; duplicate {
			return nil, "", nil, "", errors.New("checkpoint catalog contains duplicate music IDs")
		}
		targetsByID[target.MusicID] = binding
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool { return records[left].MusicID < records[right].MusicID })
	catalogEnvelope := struct {
		SchemaVersion        int                       `json:"schemaVersion"`
		CatalogSchemaVersion int                       `json:"catalogSchemaVersion"`
		CatalogCount         int                       `json:"catalogCount"`
		Records              []checkpointCatalogRecord `json:"records"`
	}{
		SchemaVersion: checkpointSchemaVersion, CatalogSchemaVersion: catalogSchemaVersion,
		CatalogCount: len(catalog), Records: records,
	}
	catalogBody, err := json.Marshal(catalogEnvelope)
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("encode checkpoint catalog fingerprint: %w", err)
	}
	catalogDigest := sha256.Sum256(catalogBody)

	executionBody, err := json.Marshal(checkpointExecutionBindingFor(opts))
	if err != nil {
		return nil, "", nil, "", fmt.Errorf("encode checkpoint execution options: %w", err)
	}
	executionDigest := sha256.Sum256(executionBody)
	return targetsByID, hex.EncodeToString(catalogDigest[:]), executionBody, hex.EncodeToString(executionDigest[:]), nil
}

func syncCheckpointParentDirectory(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open checkpoint parent for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(
			func() error {
				if syncErr != nil {
					return fmt.Errorf("sync checkpoint parent: %w", syncErr)
				}
				return nil
			}(),
			func() error {
				if closeErr != nil {
					return fmt.Errorf("close checkpoint parent after sync: %w", closeErr)
				}
				return nil
			}(),
		)
	}
	return nil
}

func createPreflightCheckpoint(ctx context.Context, checkpointPath string, opts options, catalog []catalogItem,
	targets []model.CatalogLyricsTarget, generatedAt string,
) (created *preflightCheckpoint, returnErr error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	targetBindings, catalogFingerprint, executionBody, executionSHA, err := buildCheckpointBindings(opts, catalog, targets)
	if err != nil {
		return nil, err
	}
	private, err := openCheckpointPrivatePath(checkpointPath)
	if err != nil {
		return nil, err
	}
	defer private.Close()
	if err := rejectCheckpointPathAliases(private.path, opts); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(private.path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.New("create new private checkpoint: path already exists")
		}
		return nil, fmt.Errorf("create new private checkpoint: %w", err)
	}
	cleanupReservation := true
	var createdInfo os.FileInfo
	defer func() {
		// Keep the created descriptor open while comparing and unlinking pathname
		// entries so its device/inode cannot be recycled into an unrelated file.
		if cleanupReservation && createdInfo != nil {
			returnErr = errors.Join(returnErr, cleanupFreshCheckpointReservation(
				private.path, createdInfo, private.parentFile, private.parentInfo,
			))
		}
		if file != nil {
			returnErr = errors.Join(returnErr, file.Close())
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("set private checkpoint permissions: %w", err)
	}
	createdInfo, err = file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect new private checkpoint: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync new private checkpoint: %w", err)
	}
	if err := private.parentFile.Sync(); err != nil {
		return nil, fmt.Errorf("sync new private checkpoint directory entry: %w", err)
	}

	checkpoint, err := openCheckpointWritable(private.path, targetBindings, catalogFingerprint, executionBody, executionSHA)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		cleanupErr := checkpoint.discardFreshInitialization()
		file = nil
		return nil, errors.Join(fmt.Errorf("close new private checkpoint reservation: %w", err), cleanupErr)
	}
	file = nil
	cleanupReservation = false
	if err := checkpoint.initialize(ctx, generatedAt); err != nil {
		return nil, errors.Join(err, checkpoint.discardFreshInitialization())
	}
	if err := checkpoint.validateState(ctx, false); err != nil {
		return nil, errors.Join(fmt.Errorf("validate new checkpoint: %w", err), checkpoint.discardFreshInitialization())
	}
	return checkpoint, nil
}

func validateFreshCheckpointCleanupParent(path string, parentFile *os.File, parentInfo os.FileInfo) error {
	if parentFile == nil || parentInfo == nil {
		return errors.New("fresh checkpoint cleanup requires its pinned private directory")
	}
	openedInfo, openedErr := parentFile.Stat()
	pathInfo, pathErr := os.Stat(filepath.Dir(path))
	if openedErr != nil || pathErr != nil {
		return errors.New("fresh checkpoint private directory changed before cleanup")
	}
	owner, ownerOK := checkpointOwnerID(openedInfo)
	if !openedInfo.IsDir() || !pathInfo.IsDir() || !os.SameFile(parentInfo, openedInfo) ||
		!os.SameFile(parentInfo, pathInfo) || openedInfo.Mode().Perm()&0o022 != 0 ||
		!ownerOK || int(owner) != os.Geteuid() {
		return errors.New("fresh checkpoint private directory changed before cleanup")
	}
	return nil
}

func cleanupFreshCheckpointReservation(path string, createdInfo os.FileInfo, parentFile *os.File, parentInfo os.FileInfo) error {
	if createdInfo == nil {
		return nil
	}
	if err := validateFreshCheckpointCleanupParent(path, parentFile, parentInfo); err != nil {
		return err
	}
	anchorPath := checkpointAnchorPath(path)
	if err := rejectCheckpointSidecars(path); err != nil {
		return fmt.Errorf("preserve fresh checkpoint with an unexpected visible sidecar: %w", err)
	}
	if err := rejectCheckpointSidecars(anchorPath); err != nil {
		return fmt.Errorf("preserve fresh checkpoint with an unexpected operational sidecar: %w", err)
	}
	removed := false
	for _, candidate := range []string{anchorPath, path} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect fresh checkpoint cleanup path: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(createdInfo, info) {
			// A racing replacement or stale anchor is unrelated and must survive.
			continue
		}
		if err := os.Remove(candidate); err != nil {
			return fmt.Errorf("remove owned incomplete checkpoint path: %w", err)
		}
		removed = true
	}
	if removed {
		if err := parentFile.Sync(); err != nil {
			return fmt.Errorf("sync private directory after incomplete checkpoint cleanup: %w", err)
		}
	}
	return nil
}

func (checkpoint *preflightCheckpoint) discardFreshInitialization() error {
	if checkpoint == nil {
		return nil
	}
	var result error
	if checkpoint.database != nil {
		// A context-bound database/sql transaction may already have scheduled its
		// rollback on an internal goroutine. Acquire the sole connection once so
		// that rollback and journal cleanup are complete before unlinking the
		// owned fresh inode and its operational anchor.
		idleCtx, cancel := context.WithTimeout(context.Background(), checkpointValidationTimeout)
		result = errors.Join(result, checkpoint.database.PingContext(idleCtx))
		cancel()
		result = errors.Join(result, checkpoint.database.Close())
		checkpoint.database = nil
	}
	result = errors.Join(result, cleanupFreshCheckpointReservation(
		checkpoint.path, checkpoint.pinnedInfo, checkpoint.parentFile, checkpoint.parentInfo,
	))
	if checkpoint.pinnedFile != nil {
		result = errors.Join(result, checkpoint.pinnedFile.Close())
		checkpoint.pinnedFile = nil
	}
	if checkpoint.parentFile != nil {
		result = errors.Join(result, checkpoint.parentFile.Close())
		checkpoint.parentFile = nil
	}
	return result
}

func openPreflightCheckpoint(ctx context.Context, checkpointPath string, opts options, catalog []catalogItem,
	targets []model.CatalogLyricsTarget,
) (*preflightCheckpoint, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	private, err := openCheckpointPrivatePath(checkpointPath)
	if err != nil {
		return nil, err
	}
	if err := rejectCheckpointPathAliases(private.path, opts); err != nil {
		_ = private.Close()
		return nil, err
	}
	if err := private.Close(); err != nil {
		return nil, err
	}
	targetBindings, catalogFingerprint, executionBody, executionSHA, err := buildCheckpointBindings(opts, catalog, targets)
	if err != nil {
		return nil, err
	}
	// A writable open is intentional: if a prior process crashed with a hot
	// rollback journal beside the operational anchor, SQLite must recover it
	// before any schema or content validation can be meaningful. The anchor and
	// journal are already contained by the private-directory ownership boundary.
	checkpoint, err := openCheckpointWritable(checkpointPath, targetBindings, catalogFingerprint, executionBody, executionSHA)
	if err != nil {
		return nil, err
	}
	if err := checkpoint.validateState(ctx, false); err != nil {
		_ = checkpoint.Close()
		return nil, fmt.Errorf("validate resume checkpoint after contained recovery: %w", err)
	}
	return checkpoint, nil
}

func openCheckpointReadOnly(path string, targets map[int]checkpointTargetBinding, catalogFingerprint string,
	executionBody []byte, executionSHA string,
) (*preflightCheckpoint, error) {
	return openCheckpointDatabase(path, true, targets, catalogFingerprint, executionBody, executionSHA)
}

func openCheckpointWritable(path string, targets map[int]checkpointTargetBinding, catalogFingerprint string,
	executionBody []byte, executionSHA string,
) (*preflightCheckpoint, error) {
	return openCheckpointDatabase(path, false, targets, catalogFingerprint, executionBody, executionSHA)
}

// openCheckpointDatabase never gives SQLite the operator-visible checkpoint
// pathname. It pins that reviewed inode, atomically creates or reuses a
// no-overwrite hard-link anchor inside the trusted private run directory, and
// gives SQLite only the anchor pathname. Consequently, replacing the visible
// pathname cannot redirect SQLite to a different inode. A detected swap fails
// closed and leaves the anchor in place so the reviewed durable state is not
// destroyed. The only remaining pathname mutators are principals inside
// openCheckpointPrivatePath's explicit root/same-EUID/ACL trust boundary.
func openCheckpointDatabase(path string, readOnly bool, targets map[int]checkpointTargetBinding, catalogFingerprint string,
	executionBody []byte, executionSHA string,
) (*preflightCheckpoint, error) {
	private, err := openCheckpointPrivatePath(path)
	if err != nil {
		return nil, err
	}
	pinned, info, anchorPreexisting, err := openPrivateCheckpointFile(private.path, readOnly)
	if err != nil {
		_ = private.Close()
		return nil, err
	}
	fail := func(err error) (*preflightCheckpoint, error) {
		_ = pinned.Close()
		_ = private.Close()
		return nil, err
	}
	operationalPath, err := ensureCheckpointOperationalAnchor(private, info, anchorPreexisting)
	if err != nil {
		return fail(err)
	}
	if checkpointBeforeSQLiteOpenHook != nil {
		checkpointBeforeSQLiteOpenHook(private.path, operationalPath)
	}
	databaseURL := &url.URL{Scheme: "file", Path: operationalPath}
	query := databaseURL.Query()
	if readOnly {
		query.Set("mode", "ro")
		query.Add("_pragma", "query_only(1)")
	} else {
		query.Set("mode", "rw")
	}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "trusted_schema(0)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "temp_store(MEMORY)")
	databaseURL.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return fail(fmt.Errorf("open private checkpoint SQLite database: %w", err))
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(0)
	checkpoint := &preflightCheckpoint{
		path: private.path, operationalPath: operationalPath, readOnly: readOnly,
		database: database, pinnedFile: pinned, pinnedInfo: info,
		parentFile: private.parentFile, parentInfo: private.parentInfo,
		catalogCount: len(targets), catalogFingerprint: catalogFingerprint,
		executionBody: append([]byte(nil), executionBody...), executionSHA256: executionSHA, targets: targets,
	}
	private.parentFile = nil
	closeOnError := func(err error) (*preflightCheckpoint, error) {
		_ = database.Close()
		checkpoint.database = nil
		_ = pinned.Close()
		checkpoint.pinnedFile = nil
		_ = checkpoint.parentFile.Close()
		checkpoint.parentFile = nil
		return nil, err
	}
	var schemaVersion int64
	if err := database.QueryRow(`PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
		return closeOnError(fmt.Errorf("open or recover private checkpoint SQLite database: %w", err))
	}
	if !readOnly {
		if err := checkpoint.configureWritableDatabase(); err != nil {
			return closeOnError(err)
		}
	}
	if err := rejectCheckpointSidecars(operationalPath); err != nil {
		return closeOnError(fmt.Errorf("checkpoint operational sidecar was not cleanly recovered: %w", err))
	}
	if err := checkpoint.verifyFile("after SQLite open"); err != nil {
		return closeOnError(err)
	}
	if !readOnly {
		// SQLite may have just rolled back a contained hot journal through the
		// operational anchor. Pin that recovered inode and the journal deletion
		// durably before schema/content validation or any new work proceeds.
		if err := checkpoint.syncDurableState("SQLite open or contained rollback recovery"); err != nil {
			return closeOnError(err)
		}
	}
	return checkpoint, nil
}

func openPrivateCheckpointFile(path string, readOnly bool) (*os.File, os.FileInfo, bool, error) {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, false, fmt.Errorf("inspect private checkpoint: %w", err)
	}
	if !linkInfo.Mode().IsRegular() || linkInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, false, errors.New("checkpoint path must identify a regular non-symlink file")
	}
	owner, ownerOK := checkpointOwnerID(linkInfo)
	if linkInfo.Mode().Perm() != 0o600 || !ownerOK || int(owner) != os.Geteuid() {
		return nil, nil, false, errors.New("checkpoint file must be effective-UID-owned with permissions exactly 0600")
	}
	if linkInfo.Size() < 0 || linkInfo.Size() > maxCheckpointBytes {
		return nil, nil, false, fmt.Errorf("checkpoint file exceeds %d bytes", maxCheckpointBytes)
	}
	anchorPath := checkpointAnchorPath(path)
	anchorInfo, anchorErr := os.Lstat(anchorPath)
	anchorExists := anchorErr == nil
	if anchorErr != nil && !errors.Is(anchorErr, os.ErrNotExist) {
		return nil, nil, false, fmt.Errorf("inspect checkpoint operational anchor: %w", anchorErr)
	}
	links := checkpointLinkCount(linkInfo)
	if links != 1 && links != 2 || links == 1 && anchorExists || links == 2 &&
		(!anchorExists || !anchorInfo.Mode().IsRegular() || !os.SameFile(linkInfo, anchorInfo)) {
		return nil, nil, false, errors.New("checkpoint file has an unrecognized hard-link graph")
	}
	flags := os.O_RDONLY
	if !readOnly {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open private checkpoint file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, false, fmt.Errorf("inspect opened private checkpoint: %w", err)
	}
	pathInfo, pathErr := os.Stat(path)
	if pathErr != nil || !os.SameFile(linkInfo, info) || !os.SameFile(linkInfo, pathInfo) ||
		info.Mode().Perm() != 0o600 || checkpointLinkCount(info) != links {
		_ = file.Close()
		return nil, nil, false, errors.New("checkpoint path or inode changed while being opened")
	}
	return file, info, anchorExists, nil
}

func ensureCheckpointOperationalAnchor(private *checkpointPrivatePath, pinnedInfo os.FileInfo, preexisting bool) (string, error) {
	if err := rejectCheckpointSidecars(private.path); err != nil {
		return "", err
	}
	anchorPath := checkpointAnchorPath(private.path)
	if !preexisting {
		if err := rejectCheckpointSidecars(anchorPath); err != nil {
			return "", fmt.Errorf("checkpoint operational anchor has a pre-existing sidecar: %w", err)
		}
		if err := os.Link(private.path, anchorPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				return "", errors.New("checkpoint operational anchor appeared concurrently")
			}
			return "", fmt.Errorf("create checkpoint operational inode anchor: %w", err)
		}
		if err := private.parentFile.Sync(); err != nil {
			return "", fmt.Errorf("sync checkpoint operational inode anchor: %w", err)
		}
	}
	anchorInfo, err := os.Lstat(anchorPath)
	pathInfo, pathErr := os.Lstat(private.path)
	if err != nil || pathErr != nil || !anchorInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(pinnedInfo, anchorInfo) || !os.SameFile(pinnedInfo, pathInfo) ||
		checkpointLinkCount(anchorInfo) != 2 || checkpointLinkCount(pathInfo) != 2 ||
		anchorInfo.Mode().Perm() != 0o600 || pathInfo.Mode().Perm() != 0o600 {
		return "", errors.New("checkpoint operational anchor does not bind the reviewed inode exactly")
	}
	if err := validateCheckpointOperationalSidecars(anchorPath, preexisting); err != nil {
		return "", err
	}
	return anchorPath, nil
}

func validateCheckpointOperationalSidecars(anchorPath string, allowJournal bool) error {
	for _, suffix := range sqliteSidecarSuffixes {
		sidecarPath := anchorPath + suffix
		info, err := os.Lstat(sidecarPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect checkpoint operational sidecar: %w", err)
		}
		if suffix != "-journal" || !allowJournal {
			return fmt.Errorf("checkpoint has unsupported operational sidecar %s", suffix)
		}
		owner, ownerOK := checkpointOwnerID(info)
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
			checkpointLinkCount(info) != 1 || !ownerOK || int(owner) != os.Geteuid() ||
			info.Size() < 0 || info.Size() > maxCheckpointBytes+checkpointPageSize {
			return errors.New("checkpoint rollback journal is outside its private bounded ownership contract")
		}
	}
	return nil
}

func checkpointLinkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 0
}

func checkpointOwnerID(info os.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid, true
	}
	return 0, false
}

func (checkpoint *preflightCheckpoint) configureWritableDatabase() error {
	if checkpoint == nil || checkpoint.database == nil {
		return errors.New("checkpoint database is required")
	}
	var journalMode string
	if err := checkpoint.database.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
		return fmt.Errorf("configure checkpoint journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "delete") {
		return errors.New("checkpoint journal mode must be DELETE")
	}
	for _, statement := range []string{
		`PRAGMA synchronous=FULL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA trusted_schema=OFF`,
		`PRAGMA temp_store=MEMORY`,
		`PRAGMA locking_mode=NORMAL`,
		fmt.Sprintf(`PRAGMA max_page_count=%d`, maxCheckpointPages),
	} {
		if _, err := checkpoint.database.Exec(statement); err != nil {
			return fmt.Errorf("configure private checkpoint SQLite database: %w", err)
		}
	}
	return nil
}

func (checkpoint *preflightCheckpoint) syncDurableState(stage string) error {
	if checkpoint == nil || checkpoint.readOnly || checkpoint.pinnedFile == nil || checkpoint.parentFile == nil {
		return errors.New("writable pinned checkpoint file and private directory are required")
	}
	if err := checkpoint.verifyFile("before " + stage + " sync"); err != nil {
		return err
	}
	if err := checkpoint.pinnedFile.Sync(); err != nil {
		return fmt.Errorf("sync checkpoint %s: %w", stage, err)
	}
	if err := checkpoint.parentFile.Sync(); err != nil {
		return fmt.Errorf("sync checkpoint private directory after %s: %w", stage, err)
	}
	return checkpoint.verifyFile("after " + stage + " sync")
}
