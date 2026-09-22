package db

import (
	"database/sql"
	"errors"
	"fmt"

	"strings"
	"testing"
)

func TestV23MigrationPreservesPopulatedV22RowsAndAdmitsSekaipediaGraph(t *testing.T) {
	path := legacyFixtureCopy(t, "v23-populated-provider-graph.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:22]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	for ordinal, provider := range []string{"vocaloid_fandom", "moegirl"} {
		insertV23ProviderGraphFixture(t, raw, newV23ProviderGraphFixture(provider, ordinal+1), 22)
	}
	sequenceValues := map[string]int{
		"lyrics_discovery_jobs": 91001, "lyrics_discovery_shadow_results": 91002,
		"lyrics_source_artifacts": 91003, "lyrics_source_analyses": 91004,
		"lyrics_source_review_items": 91005, "lyrics_source_review_decisions": 91006,
		"lyrics_source_renditions": 91007,
	}
	for name, sequence := range sequenceValues {
		result, err := raw.Exec(`UPDATE sqlite_sequence SET seq=? WHERE name=?`, sequence, name)
		if err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			raw.Close()
			t.Fatalf("seed sqlite_sequence %s changed=%d err=%v", name, changed, err)
		}
	}
	columns := v23SnapshotColumns(t, raw)
	beforeRows := snapshotV23Rows(t, raw, columns)
	beforeObjects := v23RebuiltSchemaObjects(t, raw)
	if err := database.applyMigrations(migrations[22:23]); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	for name, want := range sequenceValues {
		var got int
		if err := raw.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name=?`, name).Scan(&got); err != nil || got != want {
			raw.Close()
			t.Fatalf("v23 sqlite_sequence %s=%d want=%d err=%v", name, got, want, err)
		}
	}
	afterRows := snapshotV23Rows(t, raw, columns)
	for table, before := range beforeRows {
		if afterRows[table] != before {
			raw.Close()
			t.Fatalf("v23 changed existing %s rows\nbefore=%s\nafter=%s", table, before, afterRows[table])
		}
	}
	afterObjects := v23RebuiltSchemaObjects(t, raw)
	for object := range beforeObjects {
		if !afterObjects[object] {
			raw.Close()
			t.Fatalf("v23 lost schema object %s", object)
		}
	}
	var oldTimestampRows int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM lyrics_source_index_evidence
		WHERE provider IN ('vocaloid_fandom','moegirl') AND revision_timestamp=''`).Scan(&oldTimestampRows); err != nil || oldTimestampRows != 2 {
		raw.Close()
		t.Fatalf("legacy evidence timestamp rows=%d err=%v", oldTimestampRows, err)
	}
	sekaipedia := newV23ProviderGraphFixture("sekaipedia", 3)
	insertV23ProviderGraphFixture(t, raw, sekaipedia, 23)
	var evidenceTimestamp, artifactTimestamp, identityTimestamp, documentTimestamp string
	if err := raw.QueryRow(`SELECT evidence.revision_timestamp,artifact.revision_timestamp,
		json_extract(artifact.fixed_identity_json,'$.revisionTimestamp'),
		json_extract(document.document_json,'$.fixedIdentities[0].revisionTimestamp')
		FROM lyrics_source_index_evidence evidence
		JOIN song_lyrics_source_artifact_index_evidence link
		  ON link.provider=evidence.provider AND link.evidence_id=evidence.evidence_id AND link.sha256=evidence.sha256
		JOIN song_lyrics_source_artifacts artifact
		  ON artifact.document_id=link.document_id AND artifact.rendition_key=link.rendition_key
		JOIN song_lyrics_source_documents document ON document.document_id=artifact.document_id
		WHERE evidence.provider='sekaipedia'`).Scan(
		&evidenceTimestamp, &artifactTimestamp, &identityTimestamp, &documentTimestamp,
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if evidenceTimestamp != sekaipedia.revisionTimestamp || artifactTimestamp != sekaipedia.revisionTimestamp ||
		identityTimestamp != sekaipedia.revisionTimestamp || documentTimestamp != sekaipedia.revisionTimestamp {
		raw.Close()
		t.Fatalf("Sekaipedia timestamps evidence=%q artifact=%q identity=%q document=%q",
			evidenceTimestamp, artifactTimestamp, identityTimestamp, documentTimestamp)
	}
	fandom := newV23ProviderGraphFixture("vocaloid_fandom", 1)
	for name, attempt := range map[string]func() error{
		"fetch job": func() error {
			_, err := raw.Exec(`INSERT INTO lyrics_discovery_job_index_evidence
				(job_id,position,provider,evidence_id,sha256,created_at) VALUES (?,1,?,?,?,12)`, sekaipedia.jobID,
				fandom.provider, fandom.evidenceID, fandom.evidenceSHA256)
			return err
		},
		"artifact review": func() error {
			_, err := raw.Exec(`INSERT INTO lyrics_source_review_index_evidence
				(review_id,position,provider,evidence_id,sha256) VALUES (?,1,?,?,?)`, sekaipedia.reviewID,
				fandom.provider, fandom.evidenceID, fandom.evidenceSHA256)
			return err
		},
		"rendition": func() error {
			_, err := raw.Exec(`INSERT INTO lyrics_source_rendition_index_evidence
				(rendition_id,position,provider,evidence_id,sha256) VALUES (?,1,?,?,?)`, sekaipedia.renditionID,
				fandom.provider, fandom.evidenceID, fandom.evidenceSHA256)
			return err
		},
		"final song source": func() error {
			_, err := raw.Exec(`INSERT INTO song_lyrics_source_artifact_index_evidence
				(document_id,rendition_key,position,provider,evidence_id,sha256) VALUES (?, ?,1,?,?,?)`,
				sekaipedia.documentID, "full-sekai-3", fandom.provider, fandom.evidenceID, fandom.evidenceSHA256)
			return err
		},
	} {
		t.Run("provider mismatch "+name, func(t *testing.T) {
			if err := attempt(); err == nil || !strings.Contains(err.Error(), "provider mismatch") {
				t.Fatalf("provider-mismatched %s evidence link error=%v", name, err)
			}
		})
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_discovery_result_index_evidence
		(result_id,position,provider,evidence_id,sha256) VALUES (?,1,?,?,?)`, fandom.resultID,
		sekaipedia.provider, sekaipedia.evidenceID, sekaipedia.evidenceSHA256); err != nil {
		raw.Close()
		t.Fatalf("mixed-provider discovery evidence: %v", err)
	}
	const candidateReviewID = 39005
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_items
		(review_id,domain_key,kind,analysis_id,music_id,catalog_fingerprint,review_policy_version,reason_code,
		 evidence_json,state,identity_gate,source_use_gate,parse_gate,version,priority,created_at,updated_at,provider)
		VALUES (?,?, 'candidate_selection',NULL,?,?,'review-v1','ambiguous_candidates','{}','pending',
		 'not_applicable','not_applicable','not_applicable',1,0,13,13,?)`, candidateReviewID, v23Hex(64, 39005),
		fandom.musicID, v23Hex(64, 1102), fandom.provider); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_index_evidence
		(review_id,position,provider,evidence_id,sha256) VALUES (?,0,?,?,?)`, candidateReviewID,
		sekaipedia.provider, sekaipedia.evidenceID, sekaipedia.evidenceSHA256); err != nil {
		raw.Close()
		t.Fatalf("mixed-provider candidate review evidence: %v", err)
	}
	foreignRows, err := raw.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if foreignRows.Next() {
		foreignRows.Close()
		raw.Close()
		t.Fatal("v23 populated graph has a foreign-key violation")
	}
	if err := foreignRows.Close(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	var foreignKeys int
	if err := raw.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		raw.Close()
		t.Fatalf("foreign_keys=%d err=%v", foreignKeys, err)
	}
	if _, err := raw.Exec(`UPDATE lyrics_source_index_evidence SET revision_timestamp='' WHERE provider='sekaipedia'`); err == nil {
		raw.Close()
		t.Fatal("v23 lost index-evidence immutability")
	}
	if _, err := raw.Exec(`DELETE FROM lyrics_source_renditions WHERE provider='sekaipedia'`); err == nil {
		raw.Close()
		t.Fatal("v23 lost rendition immutability")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var v23Records, sekaipediaEvidence int
	if err := reopened.QueryRow(`SELECT
		(SELECT COUNT(*) FROM schema_migrations WHERE version=23),
		(SELECT COUNT(*) FROM lyrics_source_index_evidence WHERE provider='sekaipedia')`).Scan(&v23Records, &sekaipediaEvidence); err != nil || v23Records != 1 || sekaipediaEvidence != 1 {
		t.Fatalf("idempotent reopen v23 records=%d evidence=%d err=%v", v23Records, sekaipediaEvidence, err)
	}
}

func TestV23MigrationRejectsExistingProviderMismatchedArtifactEvidenceLinks(t *testing.T) {
	path := legacyFixtureCopy(t, "v23-provider-mismatch.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	database := &DB{DB: raw, path: path}
	if err := database.applyMigrations(migrations[:22]); err != nil {
		t.Fatal(err)
	}
	fandom := newV23ProviderGraphFixture("vocaloid_fandom", 1)
	moegirl := newV23ProviderGraphFixture("moegirl", 2)
	insertV23ProviderGraphFixture(t, raw, fandom, 22)
	insertV23ProviderGraphFixture(t, raw, moegirl, 22)
	if _, err := raw.Exec(`INSERT INTO lyrics_source_review_index_evidence
		(review_id,position,provider,evidence_id,sha256) VALUES (?,1,?,?,?)`, fandom.reviewID,
		moegirl.provider, moegirl.evidenceID, moegirl.evidenceSHA256); err != nil {
		t.Fatal(err)
	}
	if err := database.applyMigrations(migrations[22:23]); err == nil {
		t.Fatal("v23 accepted a provider-mismatched artifact-review evidence link")
	}
	var version, v23Views int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 22 {
		t.Fatalf("rolled-back v23 version=%d err=%v", version, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='view' AND name='lyrics_source_fixed_identity_rows'`).Scan(&v23Views); err != nil || v23Views != 0 {
		t.Fatalf("rolled-back v23 views=%d err=%v", v23Views, err)
	}
	var foreignKeys int
	if err := raw.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("rolled-back v23 foreign_keys=%d err=%v", foreignKeys, err)
	}
}

func insertV23EvidenceRow(database *sql.DB, fixture v23ProviderGraphFixture, provider, origin, kind, canonicalURL, revisionTimestamp string) error {
	_, err := database.Exec(`INSERT INTO lyrics_source_index_evidence
		(provider,evidence_id,sha256,kind,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
		 canonical_revision_url,categories_json,canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,'["Lyrics","Songs"]','','2026-07-30T12:34:57.123456789Z',?,?,?,1)`,
		provider, fixture.evidenceID, fixture.evidenceSHA256, kind, origin, fixture.pageID, fixture.revisionID,
		revisionTimestamp, fixture.sha1, "Provider Invalid", canonicalURL, fixture.evidenceRaw, len(fixture.evidenceRaw), fixture.evidenceSHA256)
	return err
}

func TestV23SekaipediaEvidenceRejectsInvalidProviderOriginURLTimestampAndSearchRows(t *testing.T) {
	database, err := Open(t.TempDir() + "/v23-evidence-validation.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	valid := newV23ProviderGraphFixture("sekaipedia", 4)
	for name, mutate := range map[string]func(*v23ProviderGraphFixture, *string, *string, *string, *string, *string){
		"unsupported provider": func(_ *v23ProviderGraphFixture, provider, _, _, _, _ *string) { *provider = "unknown" },
		"wrong origin":         func(_ *v23ProviderGraphFixture, _, origin, _, _, _ *string) { *origin = "https://sekaipedia.org" },
		"api endpoint": func(f *v23ProviderGraphFixture, _, _, _, canonicalURL, _ *string) {
			*canonicalURL = fmt.Sprintf("%s/w/api.php?oldid=%d", f.origin, f.revisionID)
		},
		"bare URL": func(f *v23ProviderGraphFixture, _, _, _, canonicalURL, _ *string) {
			*canonicalURL = f.origin + "/wiki/Provider_4"
		},
		"empty wiki path": func(f *v23ProviderGraphFixture, _, _, _, canonicalURL, _ *string) {
			*canonicalURL = fmt.Sprintf("%s/wiki/?oldid=%d", f.origin, f.revisionID)
		},
		"wrong oldid": func(f *v23ProviderGraphFixture, _, _, _, canonicalURL, _ *string) {
			*canonicalURL = fmt.Sprintf("%s/wiki/Provider_4?oldid=%d", f.origin, f.revisionID+1)
		},
		"extra query":       func(_ *v23ProviderGraphFixture, _, _, _, canonicalURL, _ *string) { *canonicalURL += "&x=1" },
		"fragment":          func(_ *v23ProviderGraphFixture, _, _, _, canonicalURL, _ *string) { *canonicalURL += "#Lyrics" },
		"missing timestamp": func(_ *v23ProviderGraphFixture, _, _, _, _, timestamp *string) { *timestamp = "" },
		"noncanonical timestamp": func(_ *v23ProviderGraphFixture, _, _, _, _, timestamp *string) {
			*timestamp = "2026-07-30T12:34:56.000Z"
		},
		"timestamp after fetch": func(_ *v23ProviderGraphFixture, _, _, _, _, timestamp *string) {
			*timestamp = "2026-07-30T12:34:58Z"
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := valid
			fixture.evidenceID += ":" + strings.ReplaceAll(name, " ", "-")
			provider, origin, kind := fixture.provider, fixture.origin, "mediawiki_revision"
			canonicalURL, timestamp := fixture.canonicalURL, fixture.revisionTimestamp
			mutate(&fixture, &provider, &origin, &kind, &canonicalURL, &timestamp)
			if err := insertV23EvidenceRow(database.DB, fixture, provider, origin, kind, canonicalURL, timestamp); err == nil {
				t.Fatal("invalid Sekaipedia evidence was accepted")
			}
		})
	}
	searchFixture := valid
	searchFixture.evidenceID += ":search"
	if _, err := database.Exec(`INSERT INTO lyrics_source_index_evidence
		(provider,evidence_id,sha256,kind,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
		 canonical_revision_url,categories_json,canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at)
		VALUES ('sekaipedia',?,?,'mediawiki_search_response','https://www.sekaipedia.org',NULL,NULL,'','','','',
		 '[]','https://www.sekaipedia.org/w/api.php?action=query','2026-07-30T12:34:57Z',?,?,?,1)`,
		searchFixture.evidenceID, searchFixture.evidenceSHA256, searchFixture.evidenceRaw, len(searchFixture.evidenceRaw),
		searchFixture.evidenceSHA256); err == nil {
		t.Fatal("Sekaipedia search-response evidence was accepted")
	}
	fandom := newV23ProviderGraphFixture("vocaloid_fandom", 5)
	if err := insertV23EvidenceRow(database.DB, fandom, fandom.provider, fandom.origin, "mediawiki_revision",
		fandom.canonicalURL, "2026-07-30T12:34:56Z"); err == nil {
		t.Fatal("legacy Fandom revision evidence accepted a Sekaipedia revision timestamp")
	}
	if _, err := database.Exec(`INSERT INTO lyrics_source_index_evidence
		(provider,evidence_id,sha256,kind,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
		 canonical_revision_url,categories_json,canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at)
		VALUES ('vocaloid_fandom',?,?,'mediawiki_search_response','https://vocaloid.fandom.com',NULL,NULL,'','','','',
		 '[]','https://vocaloid.fandom.com/api.php?action=query','2026-07-30T12:34:57Z',?,?,?,1)`,
		fandom.evidenceID+":search", fandom.evidenceSHA256, fandom.evidenceRaw, len(fandom.evidenceRaw),
		fandom.evidenceSHA256); err != nil {
		t.Fatalf("valid Fandom search-response evidence: %v", err)
	}
}

func TestV23SekaipediaURLValidationCoversDiscoveryArtifactRenditionAndFinalRows(t *testing.T) {
	database, err := Open(t.TempDir() + "/v23-durable-url-validation.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	valid := newV23ProviderGraphFixture("sekaipedia", 6)
	insertV23ProviderGraphFixture(t, database.DB, valid, 23)
	invalid := newV23ProviderGraphFixture("sekaipedia", 7)
	invalid.canonicalURL = fmt.Sprintf("%s/w/api.php?oldid=%d", invalid.origin, invalid.revisionID)
	invalid.fixedCandidateJSON = strings.Replace(invalid.fixedCandidateJSON,
		fmt.Sprintf("%s/wiki/Provider_7?oldid=%d", invalid.origin, invalid.revisionID), invalid.canonicalURL, 1)
	invalid.fixedIdentityJSON = strings.Replace(invalid.fixedIdentityJSON,
		fmt.Sprintf("%s/wiki/Provider_7?oldid=%d", invalid.origin, invalid.revisionID), invalid.canonicalURL, 1)
	if _, err := database.Exec(`INSERT INTO lyrics_discovery_jobs
		(job_id,idempotency_key,kind,state,music_id,page_id,revision_id,attempts,max_attempts,next_attempt_at,
		 created_at,updated_at,version,catalog_fingerprint,policy_version,expected_sha1,fixed_candidate_json,
		 provider,fixed_identity_json,provenance_status)
		VALUES (?,?, 'fetch_revision','queued',?,?,?,0,3,1,1,1,1,?,'matching-v2',?,?,'sekaipedia',?,'complete')`,
		invalid.jobID, v23Hex(64, 7001), invalid.musicID, invalid.pageID, invalid.revisionID, v23Hex(64, 7002),
		invalid.sha1, invalid.fixedCandidateJSON, invalid.fixedIdentityJSON); err == nil {
		t.Fatal("discovery job accepted a Sekaipedia API URL")
	}
	if _, err := database.Exec(`INSERT INTO lyrics_source_artifacts
		(artifact_id,source_type,source_origin,page_id,revision_id,page_title,canonical_revision_url,mediawiki_sha1,
		 categories_json,raw_wikitext,raw_byte_count,raw_wikitext_sha256,artifact_sha256,first_fetched_at,
		 first_creating_job_id,created_at,provider,provenance_status)
		VALUES (?,'mediawiki','https://www.sekaipedia.org',?,?,?, ?,?,'[]',?, ?,?,?,1,1,1,'sekaipedia','complete')`,
		invalid.artifactID, invalid.pageID, invalid.revisionID, "Invalid Provider", invalid.canonicalURL, invalid.sha1,
		invalid.rawWikitext, len(invalid.rawWikitext), v23Hex(64, 7003), v23Hex(64, 7004)); err == nil {
		t.Fatal("source artifact accepted a Sekaipedia API URL")
	}
	if _, err := database.Exec(`INSERT INTO lyrics_source_renditions
		(provider,artifact_id,origin,page_id,revision_id,mediawiki_sha1,page_title,canonical_revision_url,fetched_at,
		 categories_json,section,rendition_key,index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,created_at)
		VALUES ('sekaipedia',?,'https://www.sekaipedia.org',?,?,?,?,?,'2026-07-30T12:34:57Z','[]','Lyrics',
		 'invalid-rendition',?, ?,?,1)`, valid.artifactID, invalid.pageID, invalid.revisionID, invalid.sha1,
		"Invalid Provider", invalid.canonicalURL,
		fmt.Sprintf(`[{"evidenceId":%q,"sha256":%q}]`, valid.evidenceID, valid.evidenceSHA256),
		invalid.fixedIdentityJSON, v23Hex(64, 7005)); err == nil {
		t.Fatal("rendition accepted a Sekaipedia API URL")
	}
	if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (?,?)`, invalid.musicID, "Invalid Provider"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics(music_id) VALUES (?)`, invalid.musicID); err != nil {
		t.Fatal(err)
	}
	invalidDocumentJSON := `{"fixedIdentities":[` + invalid.fixedIdentityJSON + `]}`
	if _, err := database.Exec(`INSERT INTO song_lyrics_source_documents
		(document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
		VALUES (?,?,1,'untagged_full_only',?,?,?,1)`, invalid.documentID, invalid.musicID, invalidDocumentJSON,
		v23Hex(64, 7006), v23Hex(64, 7007)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO song_lyrics_source_artifacts
		(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
		 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
		 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
		VALUES (?,'sekaipedia','invalid-final','https://www.sekaipedia.org',?,?,?,?,?,?,'2026-07-30T12:34:57Z',
		 '[]','Lyrics','','',?,?,?,1,?,?)`, invalid.documentID, invalid.pageID, invalid.revisionID,
		invalid.revisionTimestamp, invalid.sha1, "Invalid Provider", invalid.canonicalURL,
		fmt.Sprintf(`[{"evidenceId":%q,"sha256":%q}]`, valid.evidenceID, valid.evidenceSHA256),
		invalid.fixedIdentityJSON, v23Hex(64, 7008), v23Hex(64, 7009), v23Hex(64, 7010)); err == nil {
		t.Fatal("final song-source artifact accepted a Sekaipedia API URL")
	}
}

func TestV23FinalArtifactRejectsDivergentScalarAndDocumentIdentityValues(t *testing.T) {
	for name, mutate := range map[string]func(*string, *string, *string, *string){
		"revision timestamp": func(revisionTimestamp, _, _, _ *string) {
			*revisionTimestamp = "2026-07-30T12:34:56.123456788Z"
		},
		"composition rendition key": func(_, compositionRenditionKey, _, _ *string) {
			*compositionRenditionKey = "full-sekai-other"
		},
		"version reason": func(_, _, versionReason, _ *string) {
			*versionReason = "version_conflict"
		},
		"duplicate document identity": func(_, _, _, documentIdentities *string) {
			divergent := strings.Replace(*documentIdentities, "2026-07-30T12:34:56.123456789Z",
				"2026-07-30T12:34:56.123456788Z", 1)
			*documentIdentities += "," + divergent
		},
	} {
		t.Run(name, func(t *testing.T) {
			database, err := Open(t.TempDir() + "/v23-divergent-final-artifact.db")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			fixture := newV23ProviderGraphFixture("sekaipedia", 8)
			renditionKey := "full-sekai-8"
			fixedIdentityJSON := fixture.fixedIdentityJSON
			revisionTimestamp := fixture.revisionTimestamp
			compositionRenditionKey := fixture.compositionRenditionKey
			versionReason := fixture.versionReason
			documentIdentities := fixedIdentityJSON
			mutate(&revisionTimestamp, &compositionRenditionKey, &versionReason, &documentIdentities)
			if _, err := database.Exec(`INSERT INTO catalog_music(music_id,title_ja) VALUES (?,?)`,
				fixture.musicID, "Provider 8"); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`INSERT INTO song_lyrics(music_id) VALUES (?)`, fixture.musicID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`INSERT INTO song_lyrics_source_documents
				(document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
				VALUES (?,?,1,'untagged_full_only',?,?,?,1)`, fixture.documentID, fixture.musicID,
				`{"fixedIdentities":[`+documentIdentities+`]}`, v23Hex(64, 8101), v23Hex(64, 8102)); err != nil {
				t.Fatal(err)
			}
			refsJSON := fmt.Sprintf(`[{"evidenceId":%q,"sha256":%q}]`, fixture.evidenceID, fixture.evidenceSHA256)
			_, err = database.Exec(`INSERT INTO song_lyrics_source_artifacts
				(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
				 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
				 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
				VALUES (?,?,?,?,?,?,?,?,?,?,'2026-07-30T12:34:57.123456789Z',?,'Lyrics',?,?,?,?,?,?,?,?)`,
				fixture.documentID, fixture.provider, renditionKey, fixture.origin, fixture.pageID, fixture.revisionID,
				revisionTimestamp, fixture.sha1, "Provider 8", fixture.canonicalURL, `["Lyrics","Songs"]`,
				compositionRenditionKey, versionReason, refsJSON, fixedIdentityJSON, v23Hex(64, 8103),
				len(fixture.rawWikitext), v23Hex(64, 8104), v23Hex(64, 8105))
			if err == nil || !strings.Contains(err.Error(), "invalid song lyrics source artifact fixed identity") {
				t.Fatalf("divergent %s error=%v", name, err)
			}
		})
	}
}

func TestMigrationHistoryNewerThanLatestRefusesStartup(t *testing.T) {
	path := t.TempDir() + "/newer-than-latest.db"
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO schema_migrations(version,name,checksum,applied_at)
		VALUES (?,'future_migration',?,1)`, len(migrations)+1, strings.Repeat("f", 64)); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("Open newer history error=%v", err)
	}
}

func TestLyricsSourceMigrationFailureRollsBackEachVersion(t *testing.T) {
	for _, version := range []int{13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25} {
		t.Run(migrations[version-1].name, func(t *testing.T) {
			path := legacyFixtureCopy(t, "lyrics-source-predecessor.db")
			raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			predecessor := &DB{DB: raw, path: path}
			if err := predecessor.applyMigrations(migrations[:version-1]); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected lyrics source migration failure")
			migrationBeforeCommitHook = func(current int) error {
				if current == version {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { migrationBeforeCommitHook = nil })
			err = predecessor.applyMigrations(migrations[version-1 : version])
			migrationBeforeCommitHook = nil
			if err == nil || !strings.Contains(err.Error(), injected.Error()) {
				t.Fatalf("migration v%d error=%v", version, err)
			}
			var recorded int
			if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&recorded); err != nil || recorded != 0 {
				t.Fatalf("migration v%d record=%d err=%v", version, recorded, err)
			}
			var objectCount int
			objectName := map[int]string{13: "lyrics_source_artifacts", 14: "lyrics_source_analyses", 15: "lyrics_source_review_items", 16: "lyrics_source_review_decisions_v15", 17: "lyrics_source_review_batch_idempotency", 18: "lyrics_source_analyses_structured_v2_insert", 19: "song_lyric_segments_ruby_insert", 20: "lyrics_discovery_fixed_candidate_validate_insert", 21: "lyrics_source_renditions", 22: "song_lyrics_source_documents", 23: "lyrics_source_fixed_identity_rows", 24: "lyrics_recovery_import_batches", 25: "song_lyrics_source_documents_v24"}[version]
			if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, objectName).Scan(&objectCount); err != nil || objectCount != 0 {
				t.Fatalf("migration v%d object %s count=%d err=%v", version, objectName, objectCount, err)
			}
		})
	}
}
