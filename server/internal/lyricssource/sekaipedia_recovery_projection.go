package lyricssource

import (
	"errors"
	"fmt"
	"strings"

	"moesekai/server/internal/model"
)

// SekaipediaRecoveryProjection is the independently reparsed, Japanese-only
// projection of one exact Sekaipedia revision evidence envelope. It contains
// no translation or romanization columns.
type SekaipediaRecoveryProjection struct {
	Section                 string
	RenditionKey            string
	ReasonCode              model.LyricsSourceVersionReasonCode
	Full                    model.LyricsSourceFull
	Game                    *model.LyricsSourceFull
	GameProjection          *model.LyricsSourceGameProjection
	AlternateVocals         []model.LyricsSourceAlternateVocal
	FixedJapaneseWikitext   []byte
	AuthoritativeStructured bool
}

// RecoverSekaipediaProjection reparses one exact MediaWiki revision envelope
// against its immutable semantic tuple. This is the recovery-to-import trust
// boundary: callers can compare the returned model directly with a SongResult
// without trusting a reconstructed digest in that result.
func RecoverSekaipediaProjection(
	raw []byte,
	expected FixedIndex,
	policy PerformerSegmentationPolicy,
) (SekaipediaRecoveryProjection, error) {
	if err := VerifySekaipediaRevisionContent(raw, expected); err != nil {
		return SekaipediaRecoveryProjection{}, err
	}
	page, err := parsePageResponse(raw)
	if err != nil || page.title != expected.Title {
		return SekaipediaRecoveryProjection{}, ErrRevisionChanged
	}
	parsed, err := parseSekaipediaSong(page.content, policy)
	if err != nil {
		return SekaipediaRecoveryProjection{}, err
	}
	parsedFull := extractionToModelFull(parsed.Full)
	fixed := sekaipediaFixedJapaneseWikitext(parsed.Full.Lines)
	if len(fixed) == 0 {
		return SekaipediaRecoveryProjection{}, ErrMalformedResponse
	}
	var full model.LyricsSourceFull
	if !strings.HasPrefix(parsed.RenditionKey, "game-") {
		full = parsedFull
	}
	var game *model.LyricsSourceFull
	if parsed.Game != nil {
		gameValue := extractionToModelFull(*parsed.Game)
		for index := range gameValue.Lines {
			gameValue.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
		}
		game = &gameValue
	}
	var gameProjection *model.LyricsSourceGameProjection
	if len(parsed.GameLineIndexes) != 0 {
		lineIDs := make([]string, len(parsed.GameLineIndexes))
		last := -1
		for index, position := range parsed.GameLineIndexes {
			if position < 0 || position >= len(full.Lines) || position <= last {
				return SekaipediaRecoveryProjection{}, errors.New("Sekaipedia Game projection is invalid")
			}
			lineIDs[index] = full.Lines[position].ID
			last = position
		}
		gameProjection = &model.LyricsSourceGameProjection{LineIDs: lineIDs}
	}
	alternates := recoveryAlternateVocals(parsed.AlternateVocals)
	projection := SekaipediaRecoveryProjection{
		Section: parsed.Section, RenditionKey: parsed.RenditionKey, ReasonCode: parsed.ReasonCode,
		Full: full, Game: game, GameProjection: gameProjection, AlternateVocals: alternates,
		FixedJapaneseWikitext: fixed, AuthoritativeStructured: parsed.AuthoritativeStructured,
	}
	return projection, nil
}

func recoveryAlternateVocals(input []sekaipediaAlternateVocalExtraction) []model.LyricsSourceAlternateVocal {
	if input == nil {
		return nil
	}
	result := make([]model.LyricsSourceAlternateVocal, 0, len(input))
	for _, alternate := range input {
		converted := model.LyricsSourceAlternateVocal{
			TabLabel: alternate.TabLabel, SingerLabel: alternate.SingerLabel,
			SingerIDs: append([]string{}, alternate.SingerIDs...),
		}
		if alternate.Full != nil {
			converted.Full = cloneRecoveryAlternateFull(extractionToModelFull(*alternate.Full))
		}
		if alternate.Game != nil {
			converted.Game = cloneRecoveryAlternateFull(extractionToModelFull(*alternate.Game))
		}
		if alternate.Full != nil && alternate.Game != nil {
			mapping, err := sekaipediaResolveProjection(alternate.fullProjectionLines, alternate.gameProjectionLines)
			if err == nil && len(mapping) == len(converted.Game.Lines) {
				lineIDs := make([]string, len(mapping))
				valid := true
				for index, position := range mapping {
					if position < 0 || position >= len(converted.Full.Lines) {
						valid = false
						break
					}
					lineIDs[index] = converted.Full.Lines[position].ID
				}
				if valid {
					converted.GameProjection = &model.LyricsSourceGameProjection{LineIDs: lineIDs}
				}
			}
		}
		result = append(result, converted)
	}
	return result
}

func cloneRecoveryAlternateFull(input model.LyricsSourceFull) *model.LyricsSourceFull {
	return model.CloneLyricsSourceFull(&input)
}
