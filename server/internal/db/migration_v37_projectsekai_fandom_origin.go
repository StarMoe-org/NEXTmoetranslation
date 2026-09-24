package db

// Migration v37 widens the vocaloid_fandom origin CHECK of
// song_lyrics_source_artifacts to the Project SEKAI Fandom wiki, which
// editor-published documents and their backups cite. The table, index and
// trigger texts are otherwise the v27 definitions, byte for byte, and every
// row is copied unchanged.
const migrationV37ProjectSekaiFandomOriginSQL = `
-- migration:foreign_keys_off
-- legacy_alter_table keeps the child foreign keys, the identity view and the
-- evidence-link triggers pointing at song_lyrics_source_artifacts by name.
PRAGMA legacy_alter_table=ON;
DROP TRIGGER song_lyrics_source_artifacts_identity_validate_insert;
DROP TRIGGER song_lyrics_source_artifacts_immutable_update;
DROP TRIGGER song_lyrics_source_artifacts_immutable_delete;
DROP INDEX idx_song_lyrics_source_artifacts_provider;
ALTER TABLE song_lyrics_source_artifacts RENAME TO song_lyrics_source_artifacts_v36;
CREATE TABLE song_lyrics_source_artifacts (
	document_id              INTEGER NOT NULL,
	provider                 TEXT NOT NULL,
	rendition_key            TEXT NOT NULL,
	origin                   TEXT NOT NULL,
	page_id                  INTEGER NOT NULL,
	revision_id              INTEGER NOT NULL,
	revision_timestamp       TEXT NOT NULL DEFAULT '',
	mediawiki_sha1           TEXT NOT NULL,
	page_title               TEXT NOT NULL,
	canonical_revision_url   TEXT NOT NULL,
	fetched_at               TEXT NOT NULL,
	categories_json          TEXT NOT NULL,
	section                  TEXT NOT NULL,
	composition_rendition_key TEXT NOT NULL DEFAULT '',
	version_reason           TEXT NOT NULL DEFAULT '',
	index_evidence_refs_json TEXT NOT NULL,
	fixed_identity_json      TEXT NOT NULL,
	fixed_identity_sha256    TEXT NOT NULL,
	raw_byte_count           INTEGER NOT NULL,
	raw_wikitext_sha256      TEXT NOT NULL,
	artifact_sha256          TEXT NOT NULL,
	PRIMARY KEY (document_id,rendition_key),
	UNIQUE (document_id,fixed_identity_sha256),
	CHECK (provider IN ('vocaloid_fandom','moegirl','sekaipedia')),
	CHECK ((provider='vocaloid_fandom' AND origin IN ('https://vocaloid.fandom.com','https://projectsekai.fandom.com')) OR
	       (provider='moegirl' AND origin='https://moegirl.icu') OR
	       (provider='sekaipedia' AND origin='https://www.sekaipedia.org')),
	CHECK (length(rendition_key) BETWEEN 1 AND 128 AND rendition_key=lower(rendition_key) AND rendition_key NOT GLOB '*[^a-z0-9._-]*'),
	CHECK (typeof(page_id)='integer' AND page_id>0 AND typeof(revision_id)='integer' AND revision_id>0),
	CHECK (length(mediawiki_sha1)=40 AND mediawiki_sha1=lower(mediawiki_sha1) AND mediawiki_sha1 NOT GLOB '*[^0-9a-f]*'),
	CHECK (length(page_title) BETWEEN 1 AND 2048 AND page_title=trim(page_title)),
	CHECK (length(canonical_revision_url) BETWEEN 1 AND 4096 AND canonical_revision_url=trim(canonical_revision_url)),
	CHECK (provider<>'sekaipedia' OR
	       (instr(canonical_revision_url,'#')=0 AND
	        substr(canonical_revision_url,1,length(origin||'/wiki/'))=origin||'/wiki/' AND
	        instr(substr(canonical_revision_url,length(origin||'/wiki/')+1),'?')>1 AND
	        substr(canonical_revision_url,instr(canonical_revision_url,'?'))='?oldid='||revision_id)),
	CHECK (length(fetched_at) BETWEEN 20 AND 35 AND fetched_at=trim(fetched_at) AND substr(fetched_at,-1)='Z'),
	CHECK (json_valid(categories_json) AND json_type(categories_json)='array'),
	CHECK (length(section) BETWEEN 1 AND 512 AND section=trim(section)),
	CHECK ((provider='sekaipedia' AND length(revision_timestamp) BETWEEN 20 AND 30 AND
	        revision_timestamp=trim(revision_timestamp) AND substr(revision_timestamp,-1)='Z' AND
	        strftime('%s',revision_timestamp) IS NOT NULL AND julianday(revision_timestamp)<=julianday(fetched_at) AND
	        (length(revision_timestamp)=20 OR
	         (length(revision_timestamp) BETWEEN 22 AND 30 AND substr(revision_timestamp,20,1)='.' AND
	          substr(revision_timestamp,21,length(revision_timestamp)-21) NOT GLOB '*[^0-9]*' AND
	          substr(revision_timestamp,-2,1)<>'0'))) OR
	       (provider<>'sekaipedia' AND revision_timestamp='')),
	CHECK (composition_rendition_key='' OR
	       (length(composition_rendition_key) BETWEEN 1 AND 128 AND
	        substr(composition_rendition_key,1,1) GLOB '[a-z0-9]' AND
	        composition_rendition_key=lower(composition_rendition_key) AND
	        composition_rendition_key NOT GLOB '*[^a-z0-9._-]*')),
	CHECK (version_reason='' OR version_reason IN
	       ('tagged_full_and_game','tagged_game_only','tagged_game_only_full_from_vocaloid','untagged_uncut_identity',
	        'untagged_game_subset','untagged_full_only','version_conflict')),
	CHECK (json_valid(index_evidence_refs_json) AND json_type(index_evidence_refs_json)='array' AND json_array_length(index_evidence_refs_json) BETWEEN 1 AND 64),
	CHECK (length(fixed_identity_json) BETWEEN 2 AND 1048576 AND json_valid(fixed_identity_json) AND json_type(fixed_identity_json)='object'),
	CHECK (length(fixed_identity_sha256)=64 AND fixed_identity_sha256=lower(fixed_identity_sha256) AND fixed_identity_sha256 NOT GLOB '*[^0-9a-f]*'),
	CHECK (typeof(raw_byte_count)='integer' AND raw_byte_count BETWEEN 1 AND 2097152),
	CHECK (length(raw_wikitext_sha256)=64 AND raw_wikitext_sha256=lower(raw_wikitext_sha256) AND raw_wikitext_sha256 NOT GLOB '*[^0-9a-f]*'),
	CHECK (length(artifact_sha256)=64 AND artifact_sha256=lower(artifact_sha256) AND artifact_sha256 NOT GLOB '*[^0-9a-f]*'),
	FOREIGN KEY (document_id) REFERENCES song_lyrics_source_documents(document_id) ON DELETE CASCADE
);
INSERT INTO song_lyrics_source_artifacts
	(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
	 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
	 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
SELECT document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
	canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
	index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256
FROM song_lyrics_source_artifacts_v36 ORDER BY document_id,rendition_key;
DROP TABLE song_lyrics_source_artifacts_v36;
CREATE INDEX idx_song_lyrics_source_artifacts_provider
	ON song_lyrics_source_artifacts(provider,page_id,revision_id,rendition_key);
CREATE TRIGGER song_lyrics_source_artifacts_immutable_update BEFORE UPDATE ON song_lyrics_source_artifacts
BEGIN SELECT RAISE(ABORT, 'song lyrics source artifacts are immutable'); END;
CREATE TRIGGER song_lyrics_source_artifacts_immutable_delete BEFORE DELETE ON song_lyrics_source_artifacts
WHEN EXISTS (SELECT 1 FROM song_lyrics_source_documents WHERE document_id=OLD.document_id)
BEGIN SELECT RAISE(ABORT, 'song lyrics source artifacts are immutable'); END;
CREATE TRIGGER song_lyrics_source_artifacts_identity_validate_insert
AFTER INSERT ON song_lyrics_source_artifacts
WHEN EXISTS (SELECT 1 FROM lyrics_source_fixed_identity_violations
             WHERE scope='song' AND owner_id=NEW.document_id AND rendition_key=NEW.rendition_key)
BEGIN SELECT RAISE(ABORT, 'invalid song lyrics source artifact fixed identity'); END;
PRAGMA legacy_alter_table=OFF;
`
