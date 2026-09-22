package store

import (
	"path/filepath"
	"testing"

	"moesekai/server/internal/db"
)

func setupProviderTargetStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "provider-targets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

func TestLyricsProviderTargetsReturnTheSeededMapInMusicIDOrder(t *testing.T) {
	s := setupProviderTargetStore(t)
	targets, aliases, err := s.LyricsProviderTargets("sekaipedia")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 37 || len(aliases) != 26 {
		t.Fatalf("seeded targets=%d aliases=%d want=37/26", len(targets), len(aliases))
	}
	previous := 0
	for _, target := range targets {
		if target.MusicID <= previous {
			t.Fatalf("targets are not ordered by music ID: %d after %d", target.MusicID, previous)
		}
		previous = target.MusicID
	}
	if targets[6].MusicID != 148 || targets[6].PageTitle != "ray" || targets[6].ResolvedPageTitle != "Ray" ||
		targets[6].UpdatedBy != "migration-v36" {
		t.Fatalf("resolved-title target=%+v", targets[6])
	}
	if aliases[6].MusicID != 334 || aliases[6].CatalogContributor != "ひとしずく" || aliases[6].ProviderContributor != "Hitoshizuku" ||
		aliases[7].MusicID != 334 || aliases[7].CatalogContributor != "やま△" {
		t.Fatalf("song-scoped aliases=%+v %+v", aliases[6], aliases[7])
	}
	if _, _, err := s.LyricsProviderTargets("moegirl"); err != nil {
		t.Fatalf("unknown provider read: %v", err)
	}
}

func TestUpsertLyricsProviderTargetReplacesTheSongAliasSet(t *testing.T) {
	s := setupProviderTargetStore(t)
	if err := s.UpsertLyricsProviderTarget("sekaipedia", LyricsProviderPageTarget{
		MusicID: 800, PageTitle: "New Song",
	}, []LyricsProviderContributorAlias{
		{CatalogContributor: "作者", ProviderContributor: "Sakusha"},
		{CatalogContributor: "作曲", ProviderContributor: "Sakkyoku"},
	}, "alice"); err != nil {
		t.Fatal(err)
	}
	targets, aliases, err := s.LyricsProviderTargets("sekaipedia")
	if err != nil {
		t.Fatal(err)
	}
	added := targets[len(targets)-1]
	if len(targets) != 38 || added.MusicID != 800 || added.PageTitle != "New Song" || added.UpdatedBy != "alice" ||
		added.UpdatedAt <= 0 {
		t.Fatalf("added target=%+v count=%d", added, len(targets))
	}
	if len(aliases) != 28 || aliases[26].MusicID != 800 || aliases[26].CatalogContributor != "作曲" ||
		aliases[27].CatalogContributor != "作者" {
		t.Fatalf("added aliases=%d tail=%+v", len(aliases), aliases[26:])
	}

	if err := s.UpsertLyricsProviderTarget("sekaipedia", LyricsProviderPageTarget{
		MusicID: 800, PageTitle: "new song", ResolvedPageTitle: "New Song (song)",
	}, []LyricsProviderContributorAlias{
		{CatalogContributor: "作者", ProviderContributor: "Author"},
	}, "bob"); err != nil {
		t.Fatal(err)
	}
	targets, aliases, err = s.LyricsProviderTargets("sekaipedia")
	if err != nil {
		t.Fatal(err)
	}
	replaced := targets[len(targets)-1]
	if len(targets) != 38 || replaced.PageTitle != "new song" || replaced.ResolvedPageTitle != "New Song (song)" ||
		replaced.UpdatedBy != "bob" {
		t.Fatalf("replaced target=%+v count=%d", replaced, len(targets))
	}
	if len(aliases) != 27 || aliases[26].CatalogContributor != "作者" || aliases[26].ProviderContributor != "Author" {
		t.Fatalf("replaced aliases=%d tail=%+v", len(aliases), aliases[26:])
	}

	var upserts int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.provider-target.upsert'`).
		Scan(&upserts); err != nil || upserts != 2 {
		t.Fatalf("upsert audits=%d err=%v", upserts, err)
	}
}

func TestDeleteLyricsProviderTargetRemovesTheTargetAndItsAliases(t *testing.T) {
	s := setupProviderTargetStore(t)
	removed, err := s.DeleteLyricsProviderTarget("sekaipedia", 334, "alice")
	if err != nil || !removed {
		t.Fatalf("delete removed=%t err=%v", removed, err)
	}
	targets, aliases, err := s.LyricsProviderTargets("sekaipedia")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 36 || len(aliases) != 24 {
		t.Fatalf("after delete targets=%d aliases=%d want=36/24", len(targets), len(aliases))
	}
	for _, target := range targets {
		if target.MusicID == 334 {
			t.Fatal("deleted target survived")
		}
	}
	for _, alias := range aliases {
		if alias.MusicID == 334 {
			t.Fatal("deleted aliases survived")
		}
	}
	missing, err := s.DeleteLyricsProviderTarget("sekaipedia", 334, "alice")
	if err != nil || missing {
		t.Fatalf("second delete removed=%t err=%v", missing, err)
	}
	var deletes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.provider-target.delete'`).
		Scan(&deletes); err != nil || deletes != 1 {
		t.Fatalf("delete audits=%d err=%v", deletes, err)
	}
}
