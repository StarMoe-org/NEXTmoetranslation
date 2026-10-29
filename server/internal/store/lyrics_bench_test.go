package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/model"
)

func BenchmarkDecodeLyricsSourceDocument(b *testing.B) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil || len(bundle.Documents) == 0 {
		b.Fatalf("load seed: %v", err)
	}
	best := bundle.Documents[0].DocumentJSON
	for _, doc := range bundle.Documents {
		if len(doc.DocumentJSON) > len(best) {
			best = doc.DocumentJSON
		}
	}
	raw := []byte(best)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := model.DecodeLyricsSourceDocument(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodePublicLyricsV3Detail(b *testing.B) {
	raw, err := os.ReadFile("../../../contracts/public-lyrics/v3/detail-rem-two-families.fixture.json")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodePublicLyricsV3Detail(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodePublicLyricsV3Detail(b *testing.B) {
	raw, err := os.ReadFile("../../../contracts/public-lyrics/v3/detail-rem-two-families.fixture.json")
	if err != nil {
		b.Fatal(err)
	}
	detail, err := DecodePublicLyricsV3Detail(raw)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EncodePublicLyricsV3Detail(detail); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSeeded700SongHotPaths(b *testing.B) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		b.Fatal(err)
	}
	database, err := db.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()

	tx, err := database.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for _, item := range bundle.Manifest.Items {
		if _, err := tx.Exec(`INSERT INTO catalog_music
			(music_id,title_ja,lyrics_catalog_fingerprint,lyrics_catalog_policy_version) VALUES (?,?,?,?)`,
			item.MusicID, item.JapaneseTitle, item.CatalogFingerprint, bundle.Manifest.CatalogPolicyVersion); err != nil {
			b.Fatal(err)
		}
	}
	performers := map[int]struct{}{}
	for _, segment := range bundle.LegacySegments {
		var ids []int
		if err := json.Unmarshal([]byte(segment.PerformerIDsJSON), &ids); err != nil {
			b.Fatal(err)
		}
		for _, id := range ids {
			performers[id] = struct{}{}
		}
	}
	for id := range performers {
		if _, err := tx.Exec(`INSERT INTO catalog_performers(performer_id,name_ja,updated_at) VALUES (?,?,1)`, id, "P"); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}

	s := New(database)
	if _, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle); err != nil {
		b.Fatal(err)
	}

	targetMusicID := bundle.Documents[0].MusicID
	bestLen := 0
	for _, doc := range bundle.Documents {
		if len(doc.DocumentJSON) > bestLen {
			bestLen = len(doc.DocumentJSON)
			targetMusicID = doc.MusicID
		}
	}

	publishedCount := 0
	for _, doc := range bundle.Documents {
		rendDoc, err := s.GetLyricsRenditionDocument(doc.MusicID)
		if err != nil || len(rendDoc.Renditions) == 0 || rendDoc.Renditions[0].Full == nil || rendDoc.Renditions[0].Game != nil {
			continue
		}
		mut := rendDoc
		mut.Renditions[0].TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: "Bench"}
		mut.Renditions[0].Full.Lines[0].Chinese = "基准测试翻译"
		if _, _, err := s.SaveLyricsRenditionMutation(mut, "bench"); err != nil {
			b.Fatal(err)
		}
		publishedCount++
		if publishedCount >= 10 {
			break
		}
	}

	b.Run("GetLyricsRenditionDocument", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := s.GetLyricsRenditionDocument(targetMusicID); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("PublishedLyricsLocalizationProjection700", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, _, err := s.PublishedLyricsLocalizationProjection(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("PublishedLyricsLocalizationProjectionCold700", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.invalidateLocalizationProjectionCache()
			index, details, _, err := s.PublishedLyricsLocalizationProjection()
			if err != nil {
				b.Fatal(err)
			}
			if len(index) != 10 || len(details) != 10 {
				b.Fatalf("projected=%d/%d want 10", len(index), len(details))
			}
		}
	})

	b.Run("ListLyricsPage", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := s.ListLyrics(50, 500); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("CatalogMusicPage", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := s.CatalogMusic("", false, 50, 500); err != nil {
				b.Fatal(err)
			}
		}
	})
}
