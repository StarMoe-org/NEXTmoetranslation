package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"moesekai/server/internal/jsonstrict"
	"moesekai/server/internal/model"
)

var ErrPublicLyricsV4Unavailable = errors.New("public lyrics v4 is unavailable for this recovery batch")

type PublicLyricsV4Line struct {
	ID                   string                  `json:"id"`
	Order                int                     `json:"order"`
	Japanese             string                  `json:"japanese"`
	StanzaBreakBefore    bool                    `json:"stanzaBreakBefore,omitempty"`
	Segments             []PublicLyricsV3Segment `json:"segments"`
	TrailingPerformerIDs []string                `json:"trailingPerformerIds"`
}

type PublicLyricsV4Side struct {
	Version model.LyricsSourceVersion `json:"version"`
	Lines   []PublicLyricsV4Line      `json:"lines"`
}

type PublicLyricsV4Rendition struct {
	Key               string                               `json:"key"`
	Kind              model.LyricsSourceRenditionKind      `json:"kind"`
	Label             string                               `json:"label"`
	AvailableVersions []string                             `json:"availableVersions"`
	Performers        []model.LyricsSourcePerformer        `json:"performers"`
	Full              *PublicLyricsV4Side                  `json:"full,omitempty"`
	Game              *PublicLyricsV4Side                  `json:"game,omitempty"`
	Relation          PublicLyricsV3Relation               `json:"relation"`
	SourceTabPaths    []model.LyricsSourceTabPath          `json:"sourceTabPaths"`
	Provenance        []PublicLyricsV3ComponentAttribution `json:"provenance"`
}

type PublicLyricsV4TranslationSide struct {
	Translations []string `json:"translations"`
}

type PublicLyricsV4EditionRendition struct {
	RenditionKey       string                            `json:"renditionKey"`
	TranslationCredits *PublicLyricsV3TranslationCredits `json:"translationCredits,omitempty"`
	Full               *PublicLyricsV4TranslationSide    `json:"full,omitempty"`
	Game               *PublicLyricsV4TranslationSide    `json:"game,omitempty"`
}

type PublicLyricsV4TranslationEdition struct {
	Key        string                           `json:"key"`
	Label      string                           `json:"label"`
	Renditions []PublicLyricsV4EditionRendition `json:"renditions"`
}

type PublicLyricsV4DetailDocument struct {
	Version                      int                                `json:"version"`
	MusicID                      int                                `json:"musicId"`
	Revision                     int                                `json:"revision"`
	UpdatedAt                    string                             `json:"updatedAt"`
	State                        PublicLyricsAvailabilityState      `json:"state"`
	DefaultTranslationEditionKey string                             `json:"defaultTranslationEditionKey"`
	TranslationEditions          []PublicLyricsV4TranslationEdition `json:"translationEditions"`
	Renditions                   []PublicLyricsV4Rendition          `json:"renditions"`
}

type PublicLyricsV4IndexDocument struct {
	Version int                     `json:"version"`
	Songs   []PublicLyricsIndexSong `json:"songs"`
}

func EncodePublicLyricsV4Index(index PublicLyricsV4IndexDocument) ([]byte, error) {
	if err := validatePublicLyricsV4Index(index); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if err := validateCompactPublicArtifactSize(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func DecodePublicLyricsV4Index(body []byte) (PublicLyricsV4IndexDocument, error) {
	var index PublicLyricsV4IndexDocument
	if err := decodePublicLyricsV4JSON(body, &index); err != nil {
		return PublicLyricsV4IndexDocument{}, fmt.Errorf("decode public lyrics v4 index: %w", err)
	}
	if err := validatePublicLyricsV4Index(index); err != nil {
		return PublicLyricsV4IndexDocument{}, err
	}
	if err := validateDecodedPublicArtifactSize(body, index); err != nil {
		return PublicLyricsV4IndexDocument{}, err
	}
	return index, nil
}

func EncodePublicLyricsV4Detail(detail PublicLyricsV4DetailDocument) ([]byte, error) {
	if err := validatePublicLyricsV4Detail(detail); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	if err := validateCompactPublicArtifactSize(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func DecodePublicLyricsV4Detail(body []byte) (PublicLyricsV4DetailDocument, error) {
	var detail PublicLyricsV4DetailDocument
	if err := decodePublicLyricsV4JSON(body, &detail); err != nil {
		return PublicLyricsV4DetailDocument{}, fmt.Errorf("decode public lyrics v4 detail: %w", err)
	}
	if err := validatePublicLyricsV4Detail(detail); err != nil {
		return PublicLyricsV4DetailDocument{}, err
	}
	if err := validateDecodedPublicArtifactSize(body, detail); err != nil {
		return PublicLyricsV4DetailDocument{}, err
	}
	return detail, nil
}

func decodePublicLyricsV4JSON(body []byte, target any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("public lyrics v4 JSON is empty")
	}
	if err := jsonstrict.Scan(body, jsonstrict.Options{ForbidNull: true}); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("public lyrics v4 contains trailing JSON")
		}
		return err
	}
	return nil
}

func publicLyricsV4SourceRendition(source PublicLyricsV3Rendition) PublicLyricsV4Rendition {
	result := PublicLyricsV4Rendition{
		Key: source.Key, Kind: source.Kind, Label: source.Label,
		AvailableVersions: clonePublicV3Strings(source.AvailableVersions),
		Performers:        append([]model.LyricsSourcePerformer{}, source.Performers...),
		Relation:          source.Relation,
		SourceTabPaths:    append([]model.LyricsSourceTabPath(nil), source.SourceTabPaths...),
		Provenance:        append([]PublicLyricsV3ComponentAttribution(nil), source.Provenance...),
	}
	result.Relation.LineIDs = clonePublicV3Strings(source.Relation.LineIDs)
	if source.Full != nil {
		result.Full = publicLyricsV4SourceSide(source.Full)
	}
	if source.Game != nil {
		result.Game = publicLyricsV4SourceSide(source.Game)
	}
	return result
}

func publicLyricsV4SourceSide(source *PublicLyricsV3Side) *PublicLyricsV4Side {
	result := &PublicLyricsV4Side{Version: source.Version, Lines: make([]PublicLyricsV4Line, len(source.Lines))}
	for index, line := range source.Lines {
		result.Lines[index] = PublicLyricsV4Line{
			ID: line.ID, Order: line.Order, Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments:             append([]PublicLyricsV3Segment(nil), line.Segments...),
			TrailingPerformerIDs: clonePublicV3Strings(line.TrailingPerformerIDs),
		}
	}
	return result
}

func publicLyricsV4VirtualMainRenditions(detail PublicLyricsV3DetailDocument) []PublicLyricsV4EditionRendition {
	result := make([]PublicLyricsV4EditionRendition, len(detail.Renditions))
	for index, rendition := range detail.Renditions {
		item := PublicLyricsV4EditionRendition{RenditionKey: rendition.Key}
		if rendition.TranslationCredits != nil {
			copy := *rendition.TranslationCredits
			item.TranslationCredits = &copy
		}
		if rendition.Full != nil {
			item.Full = &PublicLyricsV4TranslationSide{Translations: publicLyricsV4SideTranslations(rendition.Full)}
		}
		if rendition.Game != nil && rendition.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection {
			item.Game = &PublicLyricsV4TranslationSide{Translations: publicLyricsV4SideTranslations(rendition.Game)}
		}
		result[index] = item
	}
	return result
}

func publicLyricsV4SideTranslations(side *PublicLyricsV3Side) []string {
	result := make([]string, len(side.Lines))
	for index, line := range side.Lines {
		result[index] = line.Chinese
	}
	return result
}

func validatePublicLyricsV4Index(index PublicLyricsV4IndexDocument) error {
	if index.Version != 4 {
		return errors.New("public v4 index envelope is invalid")
	}
	return validatePublicLyricsV3Index(PublicLyricsV3IndexDocument{Version: 3, Songs: index.Songs})
}

func validatePublicLyricsV4Detail(detail PublicLyricsV4DetailDocument) error {
	if detail.Version != 4 || !validLyricsTranslationEditionKey(detail.DefaultTranslationEditionKey) ||
		len(detail.TranslationEditions) == 0 || len(detail.TranslationEditions) > maxLyricsTranslationEditions {
		return errors.New("public v4 detail envelope is invalid")
	}
	v3 := PublicLyricsV3DetailDocument{Version: 3, MusicID: detail.MusicID, Revision: detail.Revision, UpdatedAt: detail.UpdatedAt, State: detail.State, Renditions: make([]PublicLyricsV3Rendition, len(detail.Renditions))}
	for index, rendition := range detail.Renditions {
		v3.Renditions[index] = publicLyricsV4ValidationRendition(rendition)
	}
	if err := validatePublicLyricsV3Detail(v3); err != nil {
		return fmt.Errorf("public v4 source renditions: %w", err)
	}
	lastEditionKey := ""
	foundDefault := false
	for _, edition := range detail.TranslationEditions {
		if !validLyricsTranslationEditionKey(edition.Key) || edition.Key <= lastEditionKey ||
			validateLyricsTranslationEditionLabel(edition.Label) != nil || len(edition.Renditions) != len(detail.Renditions) {
			return errors.New("public v4 translation editions are not canonical")
		}
		lastEditionKey = edition.Key
		foundDefault = foundDefault || edition.Key == detail.DefaultTranslationEditionKey
		for index, materialized := range edition.Renditions {
			source := detail.Renditions[index]
			if materialized.RenditionKey != source.Key || !validPublicLyricsV4Credits(materialized.TranslationCredits) {
				return fmt.Errorf("public v4 edition %s rendition coverage is invalid", edition.Key)
			}
			if err := validatePublicLyricsV4TranslationSide(materialized.Full, source.Full); err != nil {
				return fmt.Errorf("public v4 edition %s rendition %s Full: %w", edition.Key, source.Key, err)
			}
			if source.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
				if materialized.Full == nil || materialized.Game != nil {
					return fmt.Errorf("public v4 edition %s rendition %s exact projection is not Full-only", edition.Key, source.Key)
				}
			} else if err := validatePublicLyricsV4TranslationSide(materialized.Game, source.Game); err != nil {
				return fmt.Errorf("public v4 edition %s rendition %s Game: %w", edition.Key, source.Key, err)
			}
		}
	}
	if !foundDefault {
		return errors.New("public v4 default translation edition is missing")
	}
	return nil
}

func publicLyricsV4ValidationRendition(source PublicLyricsV4Rendition) PublicLyricsV3Rendition {
	result := PublicLyricsV3Rendition{
		Key: source.Key, Kind: source.Kind, Label: source.Label,
		AvailableVersions: clonePublicV3Strings(source.AvailableVersions),
		Performers:        append([]model.LyricsSourcePerformer{}, source.Performers...),
		Relation:          source.Relation,
		SourceTabPaths:    append([]model.LyricsSourceTabPath(nil), source.SourceTabPaths...),
		Provenance:        append([]PublicLyricsV3ComponentAttribution(nil), source.Provenance...),
	}
	result.Relation.LineIDs = clonePublicV3Strings(source.Relation.LineIDs)
	if source.Full != nil {
		result.Full = publicLyricsV4ValidationSide(source.Full)
	}
	if source.Game != nil {
		result.Game = publicLyricsV4ValidationSide(source.Game)
	}
	return result
}

func publicLyricsV4ValidationSide(source *PublicLyricsV4Side) *PublicLyricsV3Side {
	result := &PublicLyricsV3Side{Version: source.Version, Lines: make([]PublicLyricsV3Line, len(source.Lines))}
	for index, line := range source.Lines {
		result.Lines[index] = PublicLyricsV3Line{
			ID: line.ID, Order: line.Order, Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments:             append([]PublicLyricsV3Segment(nil), line.Segments...),
			TrailingPerformerIDs: clonePublicV3Strings(line.TrailingPerformerIDs),
		}
	}
	return result
}

func validPublicLyricsV4Credits(credits *PublicLyricsV3TranslationCredits) bool {
	if credits == nil {
		return true
	}
	if credits.Translation == "" && credits.Proofreading == "" {
		return false
	}
	for _, value := range []string{credits.Translation, credits.Proofreading} {
		if value != strings.TrimSpace(value) || len(value) > 2048 || !utf8.ValidString(value) {
			return false
		}
	}
	return true
}

func validatePublicLyricsV4TranslationSide(materialized *PublicLyricsV4TranslationSide, source *PublicLyricsV4Side) error {
	if source == nil {
		if materialized != nil {
			return errors.New("translation exists without source side")
		}
		return nil
	}
	if materialized == nil || materialized.Translations == nil || len(materialized.Translations) != len(source.Lines) {
		return errors.New("translation does not fully cover source side")
	}
	for _, text := range materialized.Translations {
		if len(text) > maxLyricsLineTextBytes || !utf8.ValidString(text) {
			return errors.New("translation line is invalid")
		}
	}
	return nil
}

