package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/model"
)

const (
	v36VocaloidFandomOriginCheck = "CHECK ((provider='vocaloid_fandom' AND origin='https://vocaloid.fandom.com') OR"
	v37VocaloidFandomOriginCheck = "CHECK ((provider='vocaloid_fandom' AND origin IN ('https://vocaloid.fandom.com','https://projectsekai.fandom.com')) OR"
)

type v37SQLExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

func TestMigrationV37RebuildsSongLyricsSourceArtifactsVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v36.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applySchema(); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[:36]); err != nil {
		t.Fatal(err)
	}
	artifacts := seedV36LyricsSourceGraph(t, raw)
	// A v36 production database has already had its catalog identities
	// repaired and its runtime guards installed by earlier starts.
	if err := database.ensureRuntimeInvariants(context.Background()); err != nil {
		t.Fatal(err)
	}

	// v36 refuses the Project SEKAI Fandom origin.
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertV37SourceFixture(tx, 9001, 9001, v37FixedIdentity(model.LyricsSourceProviderVocaloidFandom,
		model.LyricsSourceOriginProjectSekaiFandom, "Synthetic_(Song)", 375274, "full-sekai-9001", "revision:vocaloid_fandom:375274")); err == nil ||
		!strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("v36 projectsekai artifact error=%v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	beforeSchema := v37SchemaObjects(t, raw)
	beforeRows := v37TableRowDigests(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	t.Logf("Open applied v37 to %d song_lyrics_source_artifacts rows in %s (pre-migration backup and integrity check included)",
		artifacts, time.Since(started))

	var version, migrationCount int
	if err := migrated.QueryRow(`SELECT MAX(version),COUNT(*) FROM schema_migrations`).Scan(&version, &migrationCount); err != nil {
		t.Fatal(err)
	}
	if version != 39 || migrationCount != 39 {
		t.Fatalf("schema_migrations max=%d count=%d", version, migrationCount)
	}

	// Open also applies v38 and v39, which only add the takeover and side-story tables.
	afterSchema := v37SchemaObjects(t, migrated.DB)
	for key, definition := range afterSchema {
		if strings.HasPrefix(definition, "on lyrics_recovery_takeovers: ") || isV39SideStoryObject(definition) {
			delete(afterSchema, key)
		}
	}
	if len(afterSchema) != len(beforeSchema) {
		t.Fatalf("schema object count before=%d after=%d", len(beforeSchema), len(afterSchema))
	}
	for key, before := range beforeSchema {
		want := before
		if key == "table/song_lyrics_source_artifacts" {
			if strings.Count(before, v36VocaloidFandomOriginCheck) != 1 {
				t.Fatalf("v36 artifact DDL lacks the pinned origin CHECK:\n%s", before)
			}
			want = strings.Replace(before, v36VocaloidFandomOriginCheck, v37VocaloidFandomOriginCheck, 1)
		}
		if after, ok := afterSchema[key]; !ok || after != want {
			t.Fatalf("schema object %s changed\nbefore=%s\nafter=%s", key, before, after)
		}
	}
	for _, name := range []string{
		"trigger/song_lyrics_source_artifacts_identity_validate_insert",
		"trigger/song_lyrics_source_artifacts_immutable_update",
		"trigger/song_lyrics_source_artifacts_immutable_delete",
		"trigger/song_lyrics_source_artifact_index_evidence_provider_insert",
		"trigger/song_lyrics_source_v3_reject_delete",
		"index/idx_song_lyrics_source_artifacts_provider",
		"view/lyrics_source_fixed_identity_rows",
	} {
		if _, ok := afterSchema[name]; !ok {
			t.Fatalf("v37 lost %s", name)
		}
	}

	afterRows := v37TableRowDigests(t, migrated.DB)
	delete(afterRows, "lyrics_recovery_takeovers")
	for _, table := range v39SideStoryTables {
		delete(afterRows, table)
	}
	if len(afterRows) != len(beforeRows) {
		t.Fatalf("table count before=%d after=%d", len(beforeRows), len(afterRows))
	}
	for table, before := range beforeRows {
		if afterRows[table] != before {
			t.Fatalf("table %s rows changed: before=%s after=%s", table, before, afterRows[table])
		}
	}
	assertNoForeignKeyViolations(t, migrated.DB)

	if _, err := migrated.Exec(`UPDATE song_lyrics_source_artifacts SET section='changed'`); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("artifact update error=%v", err)
	}
	if _, err := migrated.Exec(`DELETE FROM song_lyrics_source_artifacts`); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("artifact delete error=%v", err)
	}

	projectSekai := v37FixedIdentity(model.LyricsSourceProviderVocaloidFandom, model.LyricsSourceOriginProjectSekaiFandom,
		"Synthetic_(Song)", 375274, "full-sekai-9001", "revision:vocaloid_fandom:375274")
	if err := insertV37SourceFixture(migrated.DB, 9001, 9001, projectSekai); err != nil {
		t.Fatalf("v37 projectsekai artifact: %v", err)
	}
	undeclared := projectSekai
	undeclared.RenditionKey = "full-sekai-9002"
	if err := insertV37SourceArtifact(migrated.DB, 9001, undeclared); err == nil ||
		!strings.Contains(err.Error(), "invalid song lyrics source artifact fixed identity") {
		t.Fatalf("identity trigger after v37 error=%v", err)
	}
	for name, identity := range map[string]model.LyricsSourceFixedIdentity{
		"evil fandom origin": v37FixedIdentity(model.LyricsSourceProviderVocaloidFandom, "https://evil.fandom.com",
			"Synthetic_(Song)", 375275, "full-sekai-9003", "revision:vocaloid_fandom:375275"),
		"projectsekai origin for sekaipedia": v37FixedIdentity(model.LyricsSourceProviderSekaipedia,
			model.LyricsSourceOriginProjectSekaiFandom, "Synthetic_(Song)", 375276, "full-sekai-9004", "revision:sekaipedia:375276"),
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := migrated.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := insertV37SourceFixture(tx, 9100+identity.RevisionID%10, 9100+identity.RevisionID%10, identity); err == nil ||
				!strings.Contains(err.Error(), "CHECK constraint failed") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

// seedV36LyricsSourceGraph stores every embedded seed source-v3 document with
// its artifacts and contributions, plus Fandom and Moegirl artifacts with
// index-evidence links, legacy lyrics and a rendition localization.
func seedV36LyricsSourceGraph(t *testing.T, database *sql.DB) int {
	t.Helper()
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, item := range bundle.Manifest.Items {
		if _, err := tx.Exec(`INSERT INTO catalog_music(music_id,title_ja,lyrics_catalog_fingerprint,lyrics_catalog_policy_version)
			VALUES (?,?,?,?)`, item.MusicID, item.JapaneseTitle, item.CatalogFingerprint, model.LyricsCatalogIdentityPolicyVersion); err != nil {
			t.Fatal(err)
		}
	}
	artifactsByMusic := map[int][]embeddedlyricsseed.SourceArtifactRecord{}
	for _, artifact := range bundle.Artifacts {
		artifactsByMusic[artifact.MusicID] = append(artifactsByMusic[artifact.MusicID], artifact)
	}
	contributionsByMusic := map[int][]embeddedlyricsseed.SourceContributionRecord{}
	for _, contribution := range bundle.Contributions {
		contributionsByMusic[contribution.MusicID] = append(contributionsByMusic[contribution.MusicID], contribution)
	}
	artifacts := 0
	for index, document := range bundle.Documents {
		documentID := 1000 + index
		if _, err := tx.Exec(`INSERT INTO song_lyrics_source_documents
			(document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
			VALUES (?,?,?,?,?,?,?,?)`, documentID, document.MusicID, document.SchemaVersion, document.ReasonCode,
			document.DocumentJSON, document.DocumentSHA256, document.ManifestBatchSHA256, document.CreatedAt); err != nil {
			t.Fatal(err)
		}
		for _, artifact := range artifactsByMusic[document.MusicID] {
			if _, err := tx.Exec(`INSERT INTO song_lyrics_source_artifacts
				(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,
				 page_title,canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,
				 version_reason,index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,
				 raw_wikitext_sha256,artifact_sha256)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, documentID, artifact.Provider, artifact.RenditionKey,
				artifact.Origin, artifact.PageID, artifact.RevisionID, artifact.RevisionTimestamp, artifact.MediaWikiSHA1,
				artifact.PageTitle, artifact.CanonicalRevisionURL, artifact.FetchedAt, artifact.CategoriesJSON, artifact.Section,
				artifact.CompositionRenditionKey, artifact.VersionReason, artifact.IndexEvidenceRefsJSON,
				artifact.FixedIdentityJSON, artifact.FixedIdentitySHA256, artifact.RawByteCount,
				artifact.RawWikitextSHA256, artifact.ArtifactSHA256); err != nil {
				t.Fatal(err)
			}
			artifacts++
		}
		for _, contribution := range contributionsByMusic[document.MusicID] {
			if _, err := tx.Exec(`INSERT INTO song_lyrics_component_contributions
				(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, documentID,
				contribution.Component, contribution.RenditionKey, contribution.ContributionSHA256); err != nil {
				t.Fatal(err)
			}
		}
	}
	if artifacts != len(bundle.Artifacts) || artifacts == 0 {
		t.Fatalf("seeded %d of %d embedded artifacts", artifacts, len(bundle.Artifacts))
	}

	for _, music := range []struct {
		musicID  int
		identity model.LyricsSourceFixedIdentity
	}{
		{8001, v37FixedIdentity(model.LyricsSourceProviderVocaloidFandom, model.LyricsSourceOriginVocaloidFandom,
			"Synthetic_Song", 4101, "full-vocaloid-8001", "fixed-v36-vocaloid")},
		{8002, v37FixedIdentity(model.LyricsSourceProviderMoegirl, model.LyricsSourceOriginMoegirl,
			"合成の歌", 4102, "full-moegirl-8002", "fixed-v36-moegirl")},
	} {
		if err := insertV37SourceFixture(tx, 8000+music.musicID, music.musicID, music.identity); err != nil {
			t.Fatal(err)
		}
		reference := music.identity.IndexEvidenceRefs[0]
		if _, err := tx.Exec(`INSERT INTO lyrics_source_index_evidence
			(provider,evidence_id,sha256,kind,origin,page_id,revision_id,mediawiki_sha1,page_title,canonical_revision_url,
			 categories_json,canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at,revision_timestamp)
			VALUES (?,?,?,'mediawiki_revision',?,?,?,?,?,?,'[]','',?,?,?,?,1,'')`, music.identity.Provider, reference.EvidenceID,
			reference.SHA256, music.identity.Origin, music.identity.PageID, music.identity.RevisionID, music.identity.SHA1,
			music.identity.Title, music.identity.CanonicalURL, music.identity.FetchedAt, []byte("{}"), 2, reference.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO song_lyrics_source_artifact_index_evidence
			(document_id,rendition_key,position,provider,evidence_id,sha256) VALUES (?,?,0,?,?,?)`, 8000+music.musicID,
			music.identity.RenditionKey, music.identity.Provider, reference.EvidenceID, reference.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO song_lyrics_component_contributions
			(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, 8000+music.musicID,
			"renditions/"+music.identity.RenditionKey+"/full_text", music.identity.RenditionKey, hex64("5")); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_localizations
			(document_id,rendition_key,locale,translation_credit,updated_at,updated_by,revision)
			VALUES (?,?,'zh-CN','合成译者',7,'fixture',2)`, 8000+music.musicID, music.identity.RenditionKey); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_translation_lines
			(document_id,rendition_key,locale,position,text) VALUES (?,?,'zh-CN',0,'合成的一行')`,
			8000+music.musicID, music.identity.RenditionKey); err != nil {
			t.Fatal(err)
		}
		artifacts++
	}
	if _, err := tx.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (8003,'合成の旧曲')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO song_lyrics(music_id,revision,updated_at,source_url) VALUES
		(8003,3,9,'https://vocaloid.fandom.com/wiki/Synthetic_Legacy?oldid=4103')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO song_lyric_lines(music_id,line_id,position,japanese,zh_cn) VALUES
		(8003,'line-0001',0,'合成のうた','合成之歌')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func v37FixedIdentity(provider model.LyricsSourceProvider, origin, page string, revisionID int, renditionKey, evidenceID string) model.LyricsSourceFixedIdentity {
	canonicalURL := origin + "/wiki/" + page + "?oldid=" + strconv.Itoa(revisionID)
	digest := sha256.Sum256([]byte(canonicalURL))
	identity := model.LyricsSourceFixedIdentity{
		Provider: provider, Origin: origin, PageID: revisionID, RevisionID: revisionID,
		SHA1: hex.EncodeToString(digest[:20]), Title: strings.ReplaceAll(page, "_", " "), CanonicalURL: canonicalURL,
		FetchedAt: "2026-09-20T08:00:00Z", Categories: []string{}, Section: "SEKAI Version", RenditionKey: renditionKey,
		CompositionRenditionKey: "sekai", VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{EvidenceID: evidenceID, SHA256: hex.EncodeToString(digest[:])}},
	}
	if provider == model.LyricsSourceProviderSekaipedia {
		identity.RevisionTimestamp = identity.FetchedAt
	}
	return identity
}

// insertV37SourceFixture stores a source-v3 document whose only fixed
// identity is identity, and its artifact, for a new catalog song.
func insertV37SourceFixture(execer v37SQLExecer, documentID, musicID int, identity model.LyricsSourceFixedIdentity) error {
	if _, err := execer.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (?,?)`, musicID, "合成試験曲"); err != nil {
		return err
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	documentJSON := fmt.Sprintf(`{"schemaVersion":3,"fixedIdentities":[%s]}`, identityJSON)
	documentDigest := sha256.Sum256([]byte(documentJSON))
	if _, err := execer.Exec(`INSERT INTO song_lyrics_source_documents
		(document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,3,'',?,?,?,11)`, documentID, musicID, documentJSON, hex.EncodeToString(documentDigest[:]), hex64("9")); err != nil {
		return err
	}
	return insertV37SourceArtifact(execer, documentID, identity)
}

func insertV37SourceArtifact(execer v37SQLExecer, documentID int, identity model.LyricsSourceFixedIdentity) error {
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	evidenceJSON, err := json.Marshal(identity.IndexEvidenceRefs)
	if err != nil {
		return err
	}
	identityDigest := sha256.Sum256(identityJSON)
	_, err = execer.Exec(`INSERT INTO song_lyrics_source_artifacts
		(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
		 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
		 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,'[]',?,?,?,?,?,?,1,?,?)`, documentID, identity.Provider, identity.RenditionKey,
		identity.Origin, identity.PageID, identity.RevisionID, identity.RevisionTimestamp, identity.SHA1, identity.Title,
		identity.CanonicalURL, identity.FetchedAt, identity.Section, identity.CompositionRenditionKey, identity.VersionReason,
		string(evidenceJSON), string(identityJSON), hex.EncodeToString(identityDigest[:]), hex64("7"), hex64("8"))
	return err
}

// v37SchemaObjects maps type/name to the owning table and exact stored SQL of
// every schema object.
func v37SchemaObjects(t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	rows, err := database.Query(`SELECT type,name,tbl_name,COALESCE(sql,'<no sql>') FROM sqlite_master`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	objects := map[string]string{}
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			t.Fatal(err)
		}
		objects[kind+"/"+name] = "on " + table + ": " + statement
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

// v37TableRowDigests hashes the sorted, type-tagged rows of every table except
// the migration ledger.
func v37TableRowDigests(t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	rows, err := database.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name<>'schema_migrations' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	digests := make(map[string]string, len(tables))
	for _, table := range tables {
		tableRows, err := database.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := tableRows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var encoded []string
		for tableRows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := tableRows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			fields := make([]string, len(values))
			for index, value := range values {
				switch typed := value.(type) {
				case []byte:
					fields[index] = "blob:" + hex.EncodeToString(typed)
				default:
					fields[index] = fmt.Sprintf("%T:%q", typed, fmt.Sprint(typed))
				}
			}
			encoded = append(encoded, strings.Join(fields, "\x1f"))
		}
		if err := tableRows.Err(); err != nil {
			t.Fatal(err)
		}
		tableRows.Close()
		sort.Strings(encoded)
		digest := sha256.Sum256([]byte(strings.Join(encoded, "\x1e")))
		digests[table] = fmt.Sprintf("%d rows %x", len(encoded), digest)
	}
	return digests
}
