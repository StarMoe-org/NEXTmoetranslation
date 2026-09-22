package filesvc

import (
	"fmt"
	"log"

	"moesekai/server/internal/files"
	"moesekai/server/internal/store"
)

// applyLyricsWithdrawals removes explicitly unpublished songs from the public
// lyrics assets. The bundle overlay only ever adds database publications on top
// of the reviewed embedded release, so without this pass unpublishing a song
// the bundle contains would leave it public. It runs after every merge, so a
// withdrawal also wins over a fail-open bundle-only projection.
func (svc *Service) applyLyricsWithdrawals(assets map[string][]byte, summary LyricsProjectionSummary,
	provenance map[int]SongProvenance) (map[string][]byte, LyricsProjectionSummary, map[int]SongProvenance) {
	withdrawn, err := svc.store.PublicLyricsWithdrawals()
	if err != nil {
		log.Printf("[projection] lyrics withdrawals unavailable; serving the merged projection: %v", err)
		return assets, degradeLyricsSummary(summary, "withdrawals: query failed"), provenance
	}
	if len(withdrawn) == 0 {
		return assets, summary, provenance
	}
	index, err := decodePublicLyricsIndex(assets[publicLyricsIndexKey])
	if err != nil {
		log.Printf("[projection] merged lyrics index undecodable; withdrawals not applied: %v", err)
		return assets, degradeLyricsSummary(summary, "withdrawals: merged index decode failed"), provenance
	}
	songs := make([]store.PublicLyricsIndexSong, 0, len(index.Songs))
	for _, song := range index.Songs {
		if withdrawn[song.MusicID] {
			continue
		}
		songs = append(songs, song)
	}
	body, err := files.MarshalIndentCompat(store.PublicLyricsIndexDocument{Version: index.Version, Songs: songs})
	if err != nil {
		log.Printf("[projection] withdrawn lyrics index unmarshalable; serving the merged projection: %v", err)
		return assets, degradeLyricsSummary(summary, "withdrawals: index marshal failed"), provenance
	}
	// A fail-open merge returns the embedded bundle map itself, so the
	// withdrawal pass must never write into the map it was handed.
	next := make(map[string][]byte, len(assets))
	for key, value := range assets {
		next[key] = value
	}
	next[publicLyricsIndexKey] = append(body, '\n')
	for musicID := range withdrawn {
		delete(next, fmt.Sprintf("translation/lyrics/music_%d.json", musicID))
	}

	summary.TotalSongs = len(songs)
	if provenance == nil {
		return next, summary, nil
	}
	remaining := make(map[int]SongProvenance, len(provenance))
	for musicID, item := range provenance {
		if withdrawn[musicID] {
			continue
		}
		remaining[musicID] = item
	}
	summary.BundleSongs = 0
	summary.DBPublicationSongs = 0
	summary.LocalizationSongs = 0
	for _, song := range songs {
		switch remaining[song.MusicID].Source {
		case string(sourceBundle):
			summary.BundleSongs++
		case string(sourceDBPublication):
			summary.DBPublicationSongs++
		case string(sourceLocalizationProjection):
			summary.LocalizationSongs++
		}
	}
	return next, summary, remaining
}

func degradeLyricsSummary(summary LyricsProjectionSummary, reason string) LyricsProjectionSummary {
	summary.Degraded = true
	if summary.DegradedReason == "" {
		summary.DegradedReason = reason
	}
	return summary
}
