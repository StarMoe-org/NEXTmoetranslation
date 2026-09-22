package store

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

// Exported store surface for the offline lyrics importers under server/cmd.
// The serving binary never calls it; it exists so those import transactions can
// live outside package store without exposing the private lyrics schema.

// MaxLyricsImportActorBytes bounds the actor recorded by an offline import.
const MaxLyricsImportActorBytes = maxLyricsReviewActorBytes

// OfflineLyricsImport owns one import transaction and the lyrics stripes it
// writes. Close releases both and is safe after Commit.
type OfflineLyricsImport struct {
	store  *Store
	unlock func()
	tx     *sql.Tx
}

// BeginOfflineLyricsImport locks every affected lyrics stripe, then opens the
// transaction. The stripes stay locked until Close.
func (s *Store) BeginOfflineLyricsImport(ctx context.Context, musicIDs []int) (*OfflineLyricsImport, error) {
	unlock := s.lockLyricsStripes(musicIDs)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		unlock()
		return nil, err
	}
	return &OfflineLyricsImport{store: s, unlock: unlock, tx: tx}, nil
}

// Tx is the open transaction every import statement runs in.
func (i *OfflineLyricsImport) Tx() *sql.Tx { return i.tx }

func (i *OfflineLyricsImport) Commit() error { return i.tx.Commit() }

// Close rolls back an uncommitted transaction and unlocks the stripes.
func (i *OfflineLyricsImport) Close() {
	_ = i.tx.Rollback()
	i.unlock()
}

// NotifyChange wakes the change subscribers after a committed import.
func (i *OfflineLyricsImport) NotifyChange() { i.store.NotifyChange() }

func (s *Store) lockLyricsStripes(musicIDs []int) func() {
	seen := make(map[int]struct{}, len(musicIDs))
	stripes := make([]int, 0, len(musicIDs))
	for _, musicID := range musicIDs {
		stripe := lyricsMutexStripe(musicID)
		if _, exists := seen[stripe]; exists {
			continue
		}
		seen[stripe] = struct{}{}
		stripes = append(stripes, stripe)
	}
	sort.Ints(stripes)
	for _, stripe := range stripes {
		s.lyricsMutexes[stripe].Lock()
	}
	return func() {
		for index := len(stripes) - 1; index >= 0; index-- {
			s.lyricsMutexes[stripes[index]].Unlock()
		}
	}
}

// CatalogPerformerAliases is the closed catalog performer alias table an
// offline import projects source performer labels against.
type CatalogPerformerAliases struct {
	catalog catalogPerformerAliases
}

func LoadCatalogPerformerAliases(tx *sql.Tx) (CatalogPerformerAliases, error) {
	catalog, err := loadCatalogPerformerAliases(tx)
	if err != nil {
		return CatalogPerformerAliases{}, err
	}
	return CatalogPerformerAliases{catalog: catalog}, nil
}

// Resolve returns the alias table extended with the document's own legend and
// the set of normalized labels that legend declares as unmapped.
func (a CatalogPerformerAliases) Resolve(performers []model.LyricsSourcePerformer) (map[string]int, map[string]bool) {
	return resolveLyricsSourcePerformerAliases(a.catalog, performers)
}

// ValidateImportedLyrics runs the provenance and private-draft lyrics contracts
// an imported draft must satisfy before it is written.
func ValidateImportedLyrics(lyrics model.SongLyrics, performers CatalogPerformerAliases) error {
	if _, err := validateLyricsProvenance(lyrics); err != nil {
		return err
	}
	if code, details, _ := validateLyrics(lyrics, performers.catalog.validIDs, false); code != "" {
		return &LyricsContractError{Code: code, Details: details}
	}
	return nil
}

// LoadLyricsTx returns the editable lyrics of one song, or ErrLyricsNotFound.
func LoadLyricsTx(tx *sql.Tx, musicID int) (model.SongLyrics, error) {
	stored, err := (&Store{}).loadLyrics(tx, musicID)
	if err != nil {
		return model.SongLyrics{}, err
	}
	return stored.lyrics, nil
}

func SameLyricsContent(left, right model.SongLyrics) bool { return sameLyricsContent(left, right) }

// LyricsSourceHash is the song_lyrics.source_hash value for the given lines.
func LyricsSourceHash(lines []model.LyricLine) string { return lyricsSourceHash(lines) }

// FormatLyricsTimestamp renders a stored unix timestamp the way the serving
// store renders song_lyrics.updated_at.
func FormatLyricsTimestamp(unix int64) string { return formatTimestamp(unix) }

func CloneLyricsSourceDocument(document model.LyricsSourceDocument) *model.LyricsSourceDocument {
	return cloneSourceDocumentPtr(document)
}

func InsertLyricsRenditionLocalizationsTx(ctx context.Context, tx *sql.Tx, documentID int64,
	document model.LyricsSourceDocument, translations []lyricsstaging.RenditionTranslation, actor string, now int64,
) error {
	return insertLyricsRenditionLocalizationsTx(ctx, tx, documentID, document, translations, actor, now)
}

func ExportLyricsRenditionLocalizationsTx(ctx context.Context, tx *sql.Tx, documentID int64,
	document model.LyricsSourceDocument,
) ([]lyricsstaging.RenditionTranslation, error) {
	return exportLyricsRenditionLocalizationsTx(ctx, tx, documentID, document)
}

func LyricsRenditionTranslationsDigest(translations []lyricsstaging.RenditionTranslation) (string, error) {
	return v3TranslationsDigest(translations)
}

func InsertOrVerifyLyricsIndexEvidenceCollectionTx(ctx context.Context, tx *sql.Tx,
	evidence []lyricssource.IndexEvidence, createdAt time.Time,
) error {
	return insertOrVerifyLyricsIndexEvidenceCollectionTx(ctx, tx, evidence, createdAt)
}
