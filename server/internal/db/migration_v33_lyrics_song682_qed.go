package db

const migrationV33Song682TranslationQEDCorrectionSQL = `
UPDATE song_lyrics_translation_edition_lines
SET text = '故证毕'
WHERE edition_key = 'main'
  AND position = 9
  AND document_id IN (SELECT document_id FROM song_lyrics_source_documents WHERE music_id = 682);

UPDATE song_lyrics_rendition_translation_lines
SET text = '故证毕'
WHERE rendition_key = 'sekai'
  AND position = 9
  AND document_id IN (SELECT document_id FROM song_lyrics_source_documents WHERE music_id = 682);

UPDATE song_lyrics_translation_edition_state
SET revision = revision + 1, updated_at = 1724544000
WHERE document_id IN (SELECT document_id FROM song_lyrics_source_documents WHERE music_id = 682);
`

const MigrationV33Song682TranslationQEDCorrectionSQL = migrationV33Song682TranslationQEDCorrectionSQL
