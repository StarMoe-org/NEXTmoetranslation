package lyricscompose

import (
	"errors"
	"fmt"

	"moesekai/server/internal/model"
)

// MapGameToFull returns the sole strictly increasing exact-text mapping from
// Game lines to Full lines. Earliest and latest greedy embeddings are equal if
// and only if the subsequence embedding is unique; differing embeddings fail
// closed instead of selecting a repeated line by position.
func MapGameToFull(full, game []string) ([]int, error) {
	if len(full) == 0 || len(game) == 0 || len(game) > len(full) {
		return nil, ErrProjectionMissing
	}
	for _, line := range full {
		if !validVisibleText(line) {
			return nil, fmt.Errorf("%w: invalid Full line", ErrProjectionMissing)
		}
	}
	for _, line := range game {
		if !validVisibleText(line) {
			return nil, fmt.Errorf("%w: invalid Game line", ErrProjectionMissing)
		}
	}

	earliest := make([]int, len(game))
	fullIndex := 0
	for gameIndex, line := range game {
		for fullIndex < len(full) && full[fullIndex] != line {
			fullIndex++
		}
		if fullIndex == len(full) {
			return nil, ErrProjectionMissing
		}
		earliest[gameIndex] = fullIndex
		fullIndex++
	}

	latest := make([]int, len(game))
	fullIndex = len(full) - 1
	for gameIndex := len(game) - 1; gameIndex >= 0; gameIndex-- {
		for fullIndex >= 0 && full[fullIndex] != game[gameIndex] {
			fullIndex--
		}
		if fullIndex < 0 {
			return nil, ErrProjectionMissing
		}
		latest[gameIndex] = fullIndex
		fullIndex--
	}
	for index := range earliest {
		if earliest[index] != latest[index] {
			return nil, ErrProjectionAmbiguous
		}
	}
	return earliest, nil
}

// VersionEvidence is one already identity-gated version evidence set. Nil means
// absent; a present empty sequence is a conflict. VocaloidFull is the explicit
// complete reference used by the two conservative fallback classifications.
type VersionEvidence struct {
	TaggedFull   []string
	TaggedGame   []string
	VocaloidFull []string
	Untagged     []string
}

type VersionResolution struct {
	ReasonCode model.LyricsSourceVersionReasonCode
	Full       []string
	Game       []string
	GameToFull []int
}

// ResolveVersion implements the closed six-reason matrix. Tagged Full+Game and
// untagged subset evidence require a unique exact mapping. Tagged Game-only
// evidence is validated but ignored in favor of fixed Vocaloid Full text.
// Unsupported mixtures and mapping conflicts fail closed as version_conflict.
func ResolveVersion(evidence VersionEvidence) (VersionResolution, error) {
	taggedFull := evidence.TaggedFull != nil
	taggedGame := evidence.TaggedGame != nil
	vocaloidFull := evidence.VocaloidFull != nil
	untagged := evidence.Untagged != nil

	switch {
	case taggedFull && taggedGame && !vocaloidFull && !untagged:
		return resolveTaggedFullAndGame(evidence.TaggedFull, evidence.TaggedGame)
	case !taggedFull && taggedGame && vocaloidFull && !untagged:
		return resolveTaggedGameOnlyFullFromVocaloid(evidence.VocaloidFull, evidence.TaggedGame)
	case !taggedFull && !taggedGame && vocaloidFull && untagged:
		mapping, err := MapGameToFull(evidence.VocaloidFull, evidence.Untagged)
		if err != nil {
			return versionConflict(err)
		}
		resolution := VersionResolution{
			Full: cloneStrings(evidence.VocaloidFull), Game: cloneStrings(evidence.Untagged),
		}
		if len(evidence.VocaloidFull) == len(evidence.Untagged) {
			for index, position := range mapping {
				if position != index {
					return versionConflict(ErrProjectionAmbiguous)
				}
			}
			resolution.ReasonCode = model.LyricsSourceVersionReasonUntaggedUncutIdentity
			resolution.GameToFull = append([]int{}, mapping...)
			return resolution, nil
		}
		resolution.ReasonCode = model.LyricsSourceVersionReasonUntaggedGameSubset
		return resolution, nil
	case !taggedFull && !taggedGame && !vocaloidFull && untagged:
		if err := validateVersionSequence(evidence.Untagged); err != nil {
			return versionConflict(err)
		}
		return VersionResolution{ReasonCode: model.LyricsSourceVersionReasonUntaggedFullOnly, Full: cloneStrings(evidence.Untagged)}, nil
	default:
		return versionConflict(fmt.Errorf("unsupported evidence combination"))
	}
}

func resolveTaggedFullAndGame(full, game []string) (VersionResolution, error) {
	if err := validateVersionSequence(full); err != nil {
		return versionConflict(err)
	}
	if err := validateVersionSequence(game); err != nil {
		return versionConflict(err)
	}
	resolution := VersionResolution{
		ReasonCode: model.LyricsSourceVersionReasonTaggedFullAndGame,
		Full:       cloneStrings(full), Game: cloneStrings(game),
	}
	mapping, err := MapGameToFull(full, game)
	if err == nil {
		resolution.GameToFull = mapping
		return resolution, nil
	}
	if errors.Is(err, ErrProjectionMissing) || errors.Is(err, ErrProjectionAmbiguous) {
		// The exact Game artifact is still authoritative evidence even when a
		// strict relationship cannot be proven. Projection is optional and must
		// never erase either version.
		return resolution, nil
	}
	return versionConflict(err)
}

func resolveMappedReason(
	reason model.LyricsSourceVersionReasonCode,
	full, game []string,
	includeProjection bool,
) (VersionResolution, error) {
	mapping, err := MapGameToFull(full, game)
	if err != nil {
		return versionConflict(err)
	}
	resolution := VersionResolution{
		ReasonCode: reason,
		Full:       cloneStrings(full),
		Game:       cloneStrings(game),
	}
	if includeProjection {
		resolution.GameToFull = append([]int{}, mapping...)
	}
	return resolution, nil
}

func resolveTaggedGameOnlyFullFromVocaloid(full, ignoredGame []string) (VersionResolution, error) {
	if err := validateVersionSequence(full); err != nil {
		return versionConflict(err)
	}
	if err := validateVersionSequence(ignoredGame); err != nil {
		return versionConflict(err)
	}
	return VersionResolution{
		ReasonCode: model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid,
		Full:       cloneStrings(full),
	}, nil
}

func validateVersionSequence(sequence []string) error {
	if len(sequence) == 0 {
		return ErrProjectionMissing
	}
	for _, line := range sequence {
		if !validVisibleText(line) {
			return ErrProjectionMissing
		}
	}
	return nil
}

func versionConflict(cause error) (VersionResolution, error) {
	return VersionResolution{ReasonCode: model.LyricsSourceVersionReasonVersionConflict}, fmt.Errorf("%w: %v", ErrVersionConflict, cause)
}
