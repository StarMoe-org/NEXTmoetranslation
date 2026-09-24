package db

// Migration v38 records the recovery ledger item a whole-song document publish
// supersedes. The ledger itself stays immutable: the takeover row carries the
// superseded recovery source document verbatim (all document columns NULL when
// the item owned only an availability document), so backup validation keeps
// checking the item's artifacts and contributions against it. The publish
// deletes that document's localization and translation-edition rows with it;
// localizations_json keeps them (NULL when it owned none; source v3 only), so
// recovery candidates still carry the ledger's translations. Rows cascade
// only with their ledger item, which content restore removes by deleting the
// catalog and then the recovery batches.
const migrationV38LyricsRecoveryTakeoversSQL = `
CREATE TABLE lyrics_recovery_takeovers (
music_id            INTEGER PRIMARY KEY,
batch_sha256        TEXT NOT NULL,
item_state          TEXT NOT NULL,
schema_version      INTEGER,
reason_code         TEXT,
document_json       TEXT,
document_sha256     TEXT,
document_created_at INTEGER,
localizations_json  TEXT,
taken_over_at       INTEGER NOT NULL,
taken_over_by       TEXT NOT NULL,
CHECK (typeof(music_id)='integer' AND music_id>0),
CHECK (length(batch_sha256)=64 AND batch_sha256=lower(batch_sha256) AND batch_sha256 NOT GLOB '*[^0-9a-f]*'),
CHECK (item_state IN ('complete','game_only','satisfied_no_lyrics','ambiguous','missing','incomplete','failed')),
CHECK ((schema_version IS NULL AND reason_code IS NULL AND document_json IS NULL AND document_sha256 IS NULL AND
        document_created_at IS NULL) OR
       (typeof(schema_version)='integer' AND schema_version IN (1,2,3) AND typeof(reason_code)='text' AND
        length(document_json) BETWEEN 2 AND 16777216 AND json_valid(document_json) AND json_type(document_json)='object' AND
        length(document_sha256)=64 AND document_sha256=lower(document_sha256) AND document_sha256 NOT GLOB '*[^0-9a-f]*' AND
        typeof(document_created_at)='integer' AND document_created_at>0)),
CHECK (localizations_json IS NULL OR
       (schema_version IS 3 AND typeof(localizations_json)='text' AND
        length(localizations_json) BETWEEN 2 AND 16777216 AND json_valid(localizations_json) AND
        json_type(localizations_json)='object' AND json_type(localizations_json,'$.localizations') IS 'array' AND
        json_array_length(localizations_json,'$.localizations')>0)),
CHECK (typeof(taken_over_at)='integer' AND taken_over_at>0),
CHECK (typeof(taken_over_by)='text'),
FOREIGN KEY (batch_sha256,music_id) REFERENCES lyrics_recovery_import_items(batch_sha256,music_id) ON DELETE CASCADE
);
CREATE INDEX idx_lyrics_recovery_takeovers_item ON lyrics_recovery_takeovers(batch_sha256,music_id);
CREATE TRIGGER lyrics_recovery_takeovers_item_insert
BEFORE INSERT ON lyrics_recovery_takeovers
WHEN NOT EXISTS (
SELECT 1 FROM lyrics_recovery_import_items AS item
WHERE item.batch_sha256=NEW.batch_sha256 AND item.music_id=NEW.music_id AND item.state=NEW.item_state AND
      item.document_sha256=COALESCE(NEW.document_sha256,'')
)
BEGIN SELECT RAISE(ABORT, 'lyrics recovery takeover does not match its recovery item'); END;
CREATE TRIGGER lyrics_recovery_takeovers_immutable_update
BEFORE UPDATE ON lyrics_recovery_takeovers
BEGIN SELECT RAISE(ABORT, 'lyrics recovery takeovers are immutable'); END;
CREATE TRIGGER lyrics_recovery_takeovers_immutable_delete
BEFORE DELETE ON lyrics_recovery_takeovers
WHEN EXISTS (
SELECT 1 FROM lyrics_recovery_import_items
WHERE batch_sha256=OLD.batch_sha256 AND music_id=OLD.music_id
)
BEGIN SELECT RAISE(ABORT, 'lyrics recovery takeovers are immutable'); END;
`
