package db

var migrationsMysekai = []migration{{version: 40, name: "mysekai_text_workflow", sql: `
CREATE TABLE mysekai_sources (
 region TEXT NOT NULL, revision TEXT NOT NULL, catalog TEXT NOT NULL,
 imported_at INTEGER NOT NULL, imported_by TEXT NOT NULL,
 PRIMARY KEY(region, revision)
);
CREATE TABLE mysekai_current_sources (
 region TEXT PRIMARY KEY, revision TEXT NOT NULL,
 FOREIGN KEY(region,revision) REFERENCES mysekai_sources(region,revision)
);
CREATE TABLE mysekai_scripts (
 region TEXT NOT NULL, script_key TEXT NOT NULL, document TEXT NOT NULL,
 active INTEGER NOT NULL CHECK(active IN (0,1)), PRIMARY KEY(region,script_key)
);
CREATE TABLE mysekai_drafts (
 region TEXT NOT NULL, line_id TEXT NOT NULL, locale TEXT NOT NULL,
 source_hash TEXT NOT NULL, text TEXT NOT NULL, revision INTEGER NOT NULL,
 updated_at INTEGER NOT NULL, updated_by TEXT NOT NULL,
 PRIMARY KEY(region,line_id,locale)
);
CREATE TABLE mysekai_draft_history (
 region TEXT NOT NULL, line_id TEXT NOT NULL, locale TEXT NOT NULL,
 source_hash TEXT NOT NULL, text TEXT NOT NULL, revision INTEGER NOT NULL,
 updated_at INTEGER NOT NULL, updated_by TEXT NOT NULL,
 PRIMARY KEY(region,line_id,locale,revision)
);
CREATE TABLE mysekai_channels (
 region TEXT NOT NULL, locale TEXT NOT NULL, revision INTEGER NOT NULL,
 publication TEXT NOT NULL DEFAULT '', PRIMARY KEY(region,locale)
);
CREATE TABLE mysekai_publications (
 region TEXT NOT NULL, locale TEXT NOT NULL, revision TEXT NOT NULL,
 assets TEXT NOT NULL, created_at INTEGER NOT NULL, created_by TEXT NOT NULL,
 PRIMARY KEY(region,locale,revision)
);
CREATE TABLE mysekai_objects (
 sha256 TEXT PRIMARY KEY, body TEXT NOT NULL
);
CREATE TABLE mysekai_publication_history (
 region TEXT NOT NULL, locale TEXT NOT NULL, revision INTEGER NOT NULL,
 publication TEXT NOT NULL, action TEXT NOT NULL, updated_at INTEGER NOT NULL,
 updated_by TEXT NOT NULL, PRIMARY KEY(region,locale,revision)
);
`}}
