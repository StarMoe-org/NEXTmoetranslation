package main

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsextractionplan"
	"moesekai/server/offline/internal/lyricsrecovery"
)

func parseAcquisitionMusicIDs(value string) ([]int, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return nil, errors.New("acquisition music IDs are empty or noncanonical")
	}
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > lyricsextractionplan.MaxRecoveryScopeMusicIDs {
		return nil, errors.New("acquisition music-ID subset is outside its bound")
	}
	musicIDs := make([]int, len(parts))
	lastMusicID := 0
	for index, part := range parts {
		musicID, err := strconv.Atoi(part)
		if err != nil || musicID <= lastMusicID || strconv.Itoa(musicID) != part {
			return nil, errors.New("acquisition music IDs must be strictly ordered canonical positive integers")
		}
		musicIDs[index] = musicID
		lastMusicID = musicID
	}
	return musicIDs, nil
}

func validateAcquisitionSubset(
	plan lyricsextractionplan.RecoveryPlan,
	runtime lyricsrecovery.RuntimeConfig,
	musicIDs []int,
) error {
	if len(musicIDs) == 0 || len(musicIDs) > len(plan.Scope.MusicIDs) {
		return errors.New("acquisition subset is empty or larger than the immutable plan scope")
	}
	sekaipediaTargets := make(map[int]struct{})
	for _, configured := range plan.Providers.Configurations {
		if configured.Provider != lyricsextractionplan.ProviderSekaipedia {
			continue
		}
		for _, target := range configured.SekaipediaTargets {
			sekaipediaTargets[target.MusicID] = struct{}{}
		}
	}
	lastMusicID := 0
	for _, musicID := range musicIDs {
		if musicID <= lastMusicID {
			return errors.New("acquisition subset music IDs are not strictly ordered")
		}
		index := sort.SearchInts(plan.Scope.MusicIDs, musicID)
		if index == len(plan.Scope.MusicIDs) || plan.Scope.MusicIDs[index] != musicID {
			return errors.New("acquisition subset contains a music ID outside the immutable plan")
		}
		providerOrder, err := runtime.ProviderOrderForMusicID(musicID)
		if err != nil || len(providerOrder) != 1 || providerOrder[0] != lyricssource.ProviderSekaipedia {
			return errors.New("acquisition subset must be exclusively assigned to Sekaipedia without fallback")
		}
		if _, exact := sekaipediaTargets[musicID]; !exact {
			return errors.New("acquisition subset lacks an exact plan-bound Sekaipedia page target")
		}
		lastMusicID = musicID
	}
	return nil
}
