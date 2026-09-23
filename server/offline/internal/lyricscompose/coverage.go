package lyricscompose

import (
	"fmt"

	"moesekai/server/internal/model"
)

// VersionCoverage keeps exactly the seven model-defined version reason categories.
type VersionCoverage struct {
	TaggedFullAndGame              int `json:"tagged_full_and_game"`
	TaggedGameOnly                 int `json:"tagged_game_only"`
	TaggedGameOnlyFullFromVocaloid int `json:"tagged_game_only_full_from_vocaloid"`
	UntaggedUncutIdentity          int `json:"untagged_uncut_identity"`
	UntaggedGameSubset             int `json:"untagged_game_subset"`
	UntaggedFullOnly               int `json:"untagged_full_only"`
	VersionConflict                int `json:"version_conflict"`
}

// Coverage remains an alias for callers that adopted the original name before
// source-provider coverage became a separate matrix.
type Coverage = VersionCoverage

type CoverageCount struct {
	Category model.LyricsSourceVersionReasonCode
	Count    int
}

func (coverage *VersionCoverage) Add(reason model.LyricsSourceVersionReasonCode) error {
	if coverage == nil {
		return fmt.Errorf("nil lyrics composition coverage")
	}
	switch reason {
	case model.LyricsSourceVersionReasonTaggedFullAndGame:
		coverage.TaggedFullAndGame++
	case model.LyricsSourceVersionReasonTaggedGameOnly:
		coverage.TaggedGameOnly++
	case model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid:
		coverage.TaggedGameOnlyFullFromVocaloid++
	case model.LyricsSourceVersionReasonUntaggedUncutIdentity:
		coverage.UntaggedUncutIdentity++
	case model.LyricsSourceVersionReasonUntaggedGameSubset:
		coverage.UntaggedGameSubset++
	case model.LyricsSourceVersionReasonUntaggedFullOnly:
		coverage.UntaggedFullOnly++
	case model.LyricsSourceVersionReasonVersionConflict:
		coverage.VersionConflict++
	default:
		return fmt.Errorf("unknown lyrics version reason %q", reason)
	}
	return nil
}

func (coverage VersionCoverage) Counts() []CoverageCount {
	return []CoverageCount{
		{Category: model.LyricsSourceVersionReasonTaggedFullAndGame, Count: coverage.TaggedFullAndGame},
		{Category: model.LyricsSourceVersionReasonTaggedGameOnly, Count: coverage.TaggedGameOnly},
		{Category: model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid, Count: coverage.TaggedGameOnlyFullFromVocaloid},
		{Category: model.LyricsSourceVersionReasonUntaggedUncutIdentity, Count: coverage.UntaggedUncutIdentity},
		{Category: model.LyricsSourceVersionReasonUntaggedGameSubset, Count: coverage.UntaggedGameSubset},
		{Category: model.LyricsSourceVersionReasonUntaggedFullOnly, Count: coverage.UntaggedFullOnly},
		{Category: model.LyricsSourceVersionReasonVersionConflict, Count: coverage.VersionConflict},
	}
}

func (coverage VersionCoverage) Total() int {
	return coverage.TaggedFullAndGame + coverage.TaggedGameOnly + coverage.TaggedGameOnlyFullFromVocaloid +
		coverage.UntaggedUncutIdentity + coverage.UntaggedGameSubset +
		coverage.UntaggedFullOnly + coverage.VersionConflict
}
