// Command migrate atomically imports a complete legacy translations seed into
// a new SQLite database. It never mutates or replaces an existing database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"moesekai/server/internal/db"
	"moesekai/server/internal/importer"
	"moesekai/server/internal/model"
	"moesekai/server/internal/singleinstance"
	"moesekai/server/internal/store"
)

var errTargetExists = errors.New("target database already exists")

// migrationOwnershipAcquiredHook is used by command-package tests to pause a
// migration immediately after it owns the target database path.
var migrationOwnershipAcquiredHook func()

// migrationObjectImportedHook is used by command-package tests to inject a
// failure between staging imports. The staging database remains unpublished.
var migrationObjectImportedHook func(int) error

// migrationPostRenameDirSyncHook injects the durability uncertainty that can
// occur after rename but before the containing directory is confirmed.
var migrationPostRenameDirSyncHook func(string) error

func main() {
	src := flag.String("src", "../../translations", "legacy translations directory")
	dbPath := flag.String("db", "./data/moesekai.db", "target SQLite path")
	verify := flag.Bool("verify", true, "required lossless DB round-trip verification")
	flag.Parse()

	if err := run(*src, *dbPath, *verify); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(src, dbPath string, verify bool) error {
	return runContext(context.Background(), src, dbPath, verify)
}

func runContext(ctx context.Context, src, dbPath string, verify bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !verify {
		return errors.New("seed verification cannot be disabled")
	}
	owner, err := singleinstance.Acquire(dbPath)
	if err != nil {
		return fmt.Errorf("acquire database ownership: %w", err)
	}
	defer owner.Close()
	if migrationOwnershipAcquiredHook != nil {
		migrationOwnershipAcquiredHook()
	}
	parent := filepath.Dir(dbPath)
	incompletePath := dbPath + ".seed-incomplete"
	if _, markerErr := os.Stat(incompletePath); markerErr == nil {
		if _, targetErr := os.Stat(dbPath); targetErr == nil {
			return fmt.Errorf("incomplete seed marker exists for target %s", dbPath)
		} else if !errors.Is(targetErr, os.ErrNotExist) {
			return fmt.Errorf("inspect incomplete seed target: %w", targetErr)
		}
		if err := os.Remove(incompletePath); err != nil {
			return fmt.Errorf("remove recoverable incomplete seed marker: %w", err)
		}
		if err := fsyncDir(parent); err != nil {
			return fmt.Errorf("sync recovered seed marker removal: %w", err)
		}
	} else if !errors.Is(markerErr, os.ErrNotExist) {
		return fmt.Errorf("inspect incomplete seed marker: %w", markerErr)
	}

	if _, err := os.Stat(dbPath); err == nil {
		return fmt.Errorf("%w: %s", errTargetExists, dbPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect target database: %w", err)
	}

	payload, result, err := importer.ReadSeedDir(ctx, src)
	if err != nil {
		return fmt.Errorf("validate complete seed: %w", err)
	}
	sort.Slice(payload.Events, func(i, j int) bool { return payload.Events[i].EventID < payload.Events[j].EventID })

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	stagingFile, err := os.CreateTemp(parent, "."+filepath.Base(dbPath)+".seed-*.db")
	if err != nil {
		return fmt.Errorf("create staging database: %w", err)
	}
	stagingPath := stagingFile.Name()
	if err := stagingFile.Close(); err != nil {
		_ = os.Remove(stagingPath)
		return fmt.Errorf("close staging placeholder: %w", err)
	}
	published := false
	defer func() {
		if !published {
			removeSQLiteFiles(stagingPath)
		}
	}()

	if err := os.Remove(stagingPath); err != nil {
		return fmt.Errorf("prepare staging database: %w", err)
	}
	database, err := db.Open(stagingPath)
	if err != nil {
		return fmt.Errorf("open staging database: %w", err)
	}
	databaseOpen := true
	defer func() {
		if databaseOpen {
			_ = database.Close()
		}
	}()
	translationStore := store.New(database)
	eventStore := store.NewEventStore(database)

	objects := 0
	importedEntries := 0
	importedEvents := 0
	for _, categoryName := range model.SupportedCategories {
		count, err := translationStore.ImportCategoryContext(ctx, categoryName, payload.Categories[categoryName])
		if err != nil {
			return fmt.Errorf("import %s: %w", categoryName, err)
		}
		if count == 0 && !model.IsRestoreOptionalCategory(categoryName) {
			return fmt.Errorf("import %s produced zero rows", categoryName)
		}
		importedEntries += count
		objects++
		if err := afterImportedObject(objects); err != nil {
			return err
		}
		fmt.Printf("[migrate] %-12s %d entries\n", categoryName, count)
	}
	for _, event := range payload.Events {
		if err := eventStore.ImportOrderedContext(ctx, event.EventID, event.Meta, event.Episodes); err != nil {
			return fmt.Errorf("import event %d: %w", event.EventID, err)
		}
		objects++
		importedEvents++
		if err := afterImportedObject(objects); err != nil {
			return err
		}
	}
	if objects != len(model.SupportedCategories)+len(payload.Events) ||
		importedEntries == 0 || importedEntries != result.Entries ||
		importedEvents == 0 || importedEvents != result.EventStories {
		return fmt.Errorf("seed import produced an incomplete database")
	}
	if err := verifyDBRoundTrip(translationStore, eventStore, payload); err != nil {
		return err
	}
	if err := database.IntegrityCheck(ctx); err != nil {
		return fmt.Errorf("verify staging integrity: %w", err)
	}
	if err := database.Checkpoint(ctx); err != nil {
		return fmt.Errorf("checkpoint staging database: %w", err)
	}
	if err := database.Close(); err != nil {
		return fmt.Errorf("close staging database: %w", err)
	}
	databaseOpen = false
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("publish canceled seed: %w", err)
	}
	if err := fsyncFile(stagingPath); err != nil {
		return fmt.Errorf("sync staging database: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("publish canceled seed: %w", err)
	}
	if err := writeIncompleteMarker(incompletePath); err != nil {
		return fmt.Errorf("write incomplete seed marker: %w", err)
	}
	if err := fsyncDir(parent); err != nil {
		return fmt.Errorf("sync incomplete seed marker: %w", err)
	}
	if err := renameNoReplace(stagingPath, dbPath); err != nil {
		_ = os.Remove(incompletePath)
		_ = fsyncDir(parent)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w before publish: %s", errTargetExists, dbPath)
		}
		return fmt.Errorf("publish staging database: %w", err)
	}
	syncDirectory := fsyncDir
	if migrationPostRenameDirSyncHook != nil {
		syncDirectory = migrationPostRenameDirSyncHook
	}
	if err := syncDirectory(parent); err != nil {
		removeErr := os.Remove(dbPath)
		rollbackSyncErr := fsyncDir(parent)
		return fmt.Errorf("sync database directory: %w", errors.Join(err,
			wrapError("remove unconfirmed target", removeErr), wrapError("sync target rollback", rollbackSyncErr)))
	}
	if err := os.Remove(incompletePath); err != nil {
		return fmt.Errorf("remove incomplete seed marker: %w", err)
	}
	if err := fsyncDir(parent); err != nil {
		markerErr := writeIncompleteMarker(incompletePath)
		markerSyncErr := fsyncDir(parent)
		return fmt.Errorf("sync incomplete seed marker removal: %w", errors.Join(err,
			wrapError("restore incomplete seed marker", markerErr), wrapError("sync restored seed marker", markerSyncErr)))
	}
	published = true
	removeSQLiteFiles(stagingPath)
	fmt.Printf("[verify] OK: %d categories, %d entries, %d event stories; integrity_check=ok\n",
		result.Categories, result.Entries, result.EventStories)
	return nil
}

func writeIncompleteMarker(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString("seed publication is not yet directory-durable\n"); err != nil {
		_ = file.Close()
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func afterImportedObject(count int) error {
	if migrationObjectImportedHook != nil {
		if err := migrationObjectImportedHook(count); err != nil {
			return fmt.Errorf("injected failure after object %d: %w", count, err)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("MOESEKAI_MIGRATE_FAIL_AFTER_OBJECTS")); raw != "" {
		failAfter, err := strconv.Atoi(raw)
		if err != nil || failAfter <= 0 {
			return fmt.Errorf("MOESEKAI_MIGRATE_FAIL_AFTER_OBJECTS must be a positive integer")
		}
		if count == failAfter {
			return fmt.Errorf("injected failure after object %d", count)
		}
	}
	return nil
}

func verifyDBRoundTrip(s *store.Store, es *store.EventStore, payload importer.Payload) error {
	for _, categoryName := range model.SupportedCategories {
		got, err := s.CategoryData(categoryName)
		if err != nil {
			return fmt.Errorf("read back category %s: %w", categoryName, err)
		}
		if !reflect.DeepEqual(normalizeCategory(payload.Categories[categoryName]), normalizeCategory(got)) {
			return fmt.Errorf("category %s did not round-trip through staging database", categoryName)
		}
	}
	for _, expected := range payload.Events {
		got, err := es.OrderedDetail(expected.EventID)
		if err != nil {
			return fmt.Errorf("read back event %d: %w", expected.EventID, err)
		}
		if diff := eventDiff(expected, got); diff != "" {
			return fmt.Errorf("event %d did not round-trip: %s", expected.EventID, diff)
		}
	}
	return nil
}

func normalizeCategory(category model.Category) model.Category {
	out := make(model.Category, len(category))
	for field, entries := range category {
		if len(entries) == 0 {
			continue
		}
		normalized := make(map[string]model.Entry, len(entries))
		for key, entry := range entries {
			if entry.Source == "" {
				entry.Source = model.SourceUnknown
			}
			if len(entry.Ids) == 0 {
				entry.Ids = nil
			}
			normalized[key] = entry
		}
		out[field] = normalized
	}
	return out
}

func eventDiff(expected store.LegacyEventRestore, got store.OrderedDetail) string {
	if !reflect.DeepEqual(expected.Meta, got.Meta) {
		return fmt.Sprintf("metadata mismatch: expected=%+v got=%+v", expected.Meta, got.Meta)
	}
	if len(expected.Episodes) != len(got.Episodes) {
		return fmt.Sprintf("episode count %d != %d", len(expected.Episodes), len(got.Episodes))
	}
	for index, episode := range expected.Episodes {
		actual := got.Episodes[index]
		if episode.EpisodeNo != actual.EpisodeNo || episode.ScenarioID != actual.ScenarioID ||
			episode.Title != actual.Title || episode.TitleSource != actual.TitleSource {
			return fmt.Sprintf("episode %d identity/metadata mismatch", index)
		}
		if !reflect.DeepEqual(episode.TalkKeys, actual.TalkKeys) {
			return fmt.Sprintf("episode %s talk order mismatch", episode.EpisodeNo)
		}
		if !reflect.DeepEqual(episode.TalkData, actual.TalkData) {
			return fmt.Sprintf("episode %s talk text mismatch", episode.EpisodeNo)
		}
		expectedSources := make(map[string]string, len(episode.TalkKeys))
		for _, key := range episode.TalkKeys {
			expectedSources[key] = expected.Meta.Source
			if episode.TalkSources[key] != "" {
				expectedSources[key] = episode.TalkSources[key]
			}
		}
		if !reflect.DeepEqual(normalizeStringMap(expectedSources), normalizeStringMap(actual.TalkSources)) {
			return fmt.Sprintf("episode %s talk source mismatch", episode.EpisodeNo)
		}
		if !reflect.DeepEqual(normalizeStringMap(episode.SpeakerNames), normalizeStringMap(actual.SpeakerNames)) {
			return fmt.Sprintf("episode %s speaker mismatch", episode.EpisodeNo)
		}
	}
	return ""
}

func normalizeStringMap(value map[string]string) map[string]string {
	if len(value) == 0 {
		return nil
	}
	return value
}

func removeSQLiteFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	backups, _ := filepath.Glob(path + ".pre-migration-v*.bak")
	for _, backup := range backups {
		_ = os.Remove(backup)
	}
}

func fsyncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	return errors.Join(syncErr, file.Close())
}

func fsyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}
