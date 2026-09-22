package db

const migrationV34Song682TranslationMirrorSyncSQL = `
-- Synchronize legacy rendition localization mirror revision with authoritative edition state
UPDATE song_lyrics_rendition_localizations
SET revision = (
  SELECT revision FROM song_lyrics_translation_edition_state
  WHERE song_lyrics_translation_edition_state.document_id = song_lyrics_rendition_localizations.document_id
)
WHERE document_id IN (
  SELECT document_id FROM song_lyrics_source_documents WHERE music_id = 682
)
-- A mirror row without authoritative edition state keeps its own revision;
-- the subquery above would otherwise write NULL into a NOT NULL column.
AND EXISTS (
  SELECT 1 FROM song_lyrics_translation_edition_state
  WHERE song_lyrics_translation_edition_state.document_id = song_lyrics_rendition_localizations.document_id
);
`

const MigrationV34Song682TranslationMirrorSyncSQL = migrationV34Song682TranslationMirrorSyncSQL
