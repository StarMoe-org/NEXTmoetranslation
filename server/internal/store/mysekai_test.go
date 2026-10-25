package store

import (
	"context"
	"encoding/json"
	"errors"
	"moesekai/server/internal/db"
	"path/filepath"
	"testing"
)

func TestMysekaiSourceEditsPublicationAndRestore(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "mysekai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := New(database)
	step := 0
	hash := mysekaiDigest([]byte("original"))
	line := MysekaiLine{ID: "synthetic-a", Kind: "text", Step: &step, Text: "Original", SourceHash: hash}
	catalog := MysekaiCatalog{Schema: "moly-text-source/1", Region: "jp", Revision: mysekaiDigest([]byte("catalog-1")), Scripts: []MysekaiScript{{Key: "synthetic-script", Lines: []MysekaiLine{line}}, {Key: "other-script", Lines: []MysekaiLine{{ID: "synthetic-b", Kind: "text", Step: &step, Text: "Other", SourceHash: mysekaiDigest([]byte("other"))}}}}}
	raw, _ := json.Marshal(catalog)
	if _, err = s.ImportMysekaiSource(ctx, raw, "", "admin"); err != nil {
		t.Fatal(err)
	}
	// Diagnostics are allowed to change without pretending the source changed.
	var diagnostic map[string]any
	_ = json.Unmarshal(raw, &diagnostic)
	diagnostic["retiredScripts"] = []any{}
	diagRaw, _ := json.Marshal(diagnostic)
	if _, err = s.ImportMysekaiSource(ctx, diagRaw, catalog.Revision, "admin"); err != nil {
		t.Fatal(err)
	}
	save := MysekaiSave{Region: "jp", Locale: "zh-CN", ScriptKey: "synthetic-script", SourceRevision: catalog.Revision, Edits: []MysekaiEdit{{ID: line.ID, SourceHash: hash, Text: "译文"}}}
	if _, err = s.SaveMysekai(ctx, save, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveMysekai(ctx, save, "editor"); !errors.Is(err, ErrMysekaiConflict) {
		t.Fatalf("stale write: %v", err)
	}
	request := MysekaiPublicationRequest{Region: "jp", Locale: "zh-CN", SourceRevision: catalog.Revision}
	page, _ := s.MysekaiList(ctx, "jp", "zh-CN", "", "", "", 40)
	request.DraftRevision = page.DraftRevision
	channel, err := s.SetMysekaiPublication(ctx, request, "publish", "admin")
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.MysekaiPublicFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var root MysekaiPublication
	root = MysekaiPublication{}
	if err = json.Unmarshal(files["mysekai/jp/zh-CN/latest.json"], &root); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range root.Buckets {
		if mysekaiDigest(files["mysekai/jp/zh-CN/"+bucket.Path]) != bucket.SHA256 {
			t.Fatal("corrupt bucket")
		}
	}
	// A revision containing unrelated source changes preserves script bytes.
	oldRevision := catalog.Revision
	catalog.Revision = mysekaiDigest([]byte("catalog-2"))
	catalog.Scripts[1].Lines[0].Text = "Other changed"
	catalog.Scripts[1].Lines[0].SourceHash = mysekaiDigest([]byte("other changed"))
	raw, _ = json.Marshal(catalog)
	if _, err = s.ImportMysekaiSource(ctx, raw, oldRevision, "admin"); err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = channel.Revision
	request.SourceRevision = catalog.Revision
	channel2, err := s.SetMysekaiPublication(ctx, request, "publish", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if channel2.Publication != channel.Publication {
		t.Fatal("unrelated source revision changed translated script publication")
	}
	var beforeObjects, beforeBytes int
	if err = database.QueryRow(`SELECT COUNT(*),SUM(LENGTH(CAST(body AS BLOB))) FROM mysekai_objects`).Scan(&beforeObjects, &beforeBytes); err != nil {
		t.Fatal(err)
	}
	// Publishing one edited line appends its leaf, route and search bucket, not another
	// copy of all immutable objects from previous revisions.
	save.SourceRevision = catalog.Revision
	save.Edits[0].ExpectedRevision = 1
	save.Edits[0].Text = "译文改一个字"
	if _, err = s.SaveMysekai(ctx, save, "editor"); err != nil {
		t.Fatal(err)
	}
	page, _ = s.MysekaiList(ctx, "jp", "zh-CN", "", "", "", 40)
	request.ExpectedRevision = channel2.Revision
	request.DraftRevision = page.DraftRevision
	channel2, err = s.SetMysekaiPublication(ctx, request, "publish", "admin")
	if err != nil {
		t.Fatal(err)
	}
	var afterObjects, afterBytes int
	if err = database.QueryRow(`SELECT COUNT(*),SUM(LENGTH(CAST(body AS BLOB))) FROM mysekai_objects`).Scan(&afterObjects, &afterBytes); err != nil {
		t.Fatal(err)
	}
	if afterObjects-beforeObjects != 3 {
		t.Fatalf("single-script edit appended %d objects, want leaf, route and search bucket", afterObjects-beforeObjects)
	}
	t.Logf("single-line publication appended %d bytes in %d objects; previous immutable bytes=%d", afterBytes-beforeBytes, afterObjects-beforeObjects, beforeBytes)
	// Changed source preserves draft text but cannot publish it without review.
	oldRevision = catalog.Revision
	catalog.Revision = mysekaiDigest([]byte("catalog-3"))
	catalog.Scripts[0].Lines[0].SourceHash = mysekaiDigest([]byte("changed"))
	raw, _ = json.Marshal(catalog)
	if _, err = s.ImportMysekaiSource(ctx, raw, oldRevision, "admin"); err != nil {
		t.Fatal(err)
	}
	detail, err := s.MysekaiDetail(ctx, "jp", "zh-CN", "synthetic-script")
	if err != nil || detail.Drafts[line.ID].Text != "译文改一个字" {
		t.Fatalf("draft lost: %v", err)
	}
	request.ExpectedRevision = channel2.Revision
	request.SourceRevision = catalog.Revision
	channel3, err := s.SetMysekaiPublication(ctx, request, "publish", "admin")
	if err != nil {
		t.Fatal(err)
	}
	files, err = s.MysekaiPublicFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root = MysekaiPublication{}
	if err = json.Unmarshal(files["mysekai/jp/zh-CN/latest.json"], &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Buckets) != 0 {
		t.Fatal("outdated draft published")
	}
	request.ExpectedRevision = channel3.Revision
	request.Publication = channel.Publication
	if _, err = s.SetMysekaiPublication(ctx, request, "rollback", "admin"); err != nil {
		t.Fatal(err)
	}
	exported, err := s.ExportMysekaiContentContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = importMysekaiContentTx(ctx, tx, exported); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after, err := s.MysekaiPublicFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(after["mysekai/jp/zh-CN/latest.json"]) != string(files["mysekai/jp/zh-CN/revisions/"+channel.Publication+".json"]) {
		t.Fatal("backup lost rollback or immutable history")
	}
}
