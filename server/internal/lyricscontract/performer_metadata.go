package lyricscontract

import (
	"errors"
	"strings"

	"golang.org/x/text/unicode/norm"

	"moesekai/server/internal/lyricsperformers"
	"moesekai/server/internal/model"
)

// ErrUnsafePerformerMetadata closes the persisted performer-value boundary
// without including source-supplied performer values in an error or log chain.
var ErrUnsafePerformerMetadata = errors.New("unsafe persisted lyrics performer metadata")

type AuditedPersistedPerformer struct {
	ID      string
	Name    string
	Aliases []string
}

// These Project SEKAI performer-wide identities are stable across songs and
// providers. The persisted ID is deliberately non-Latin; Name is the Japanese
// display name, except for the two official Latin-script VIRTUAL SINGER brand
// names. Audited external lyrics-only singers are resolved through the shared
// lyricsperformers registry below.
var auditedPersistedPerformers = []AuditedPersistedPerformer{
	{ID: "歌唱者-01", Name: "星乃一歌", Aliases: []string{"ichika", "Hoshino Ichika", "Ichika Hoshino", "星乃一歌"}},
	{ID: "歌唱者-02", Name: "天馬咲希", Aliases: []string{"saki", "Tenma Saki", "Saki Tenma", "天馬咲希", "天马咲希"}},
	{ID: "歌唱者-03", Name: "望月穂波", Aliases: []string{"honami", "Mochizuki Honami", "Honami Mochizuki", "望月穂波", "望月穗波"}},
	{ID: "歌唱者-04", Name: "日野森志歩", Aliases: []string{"shiho", "Hinomori Shiho", "Shiho Hinomori", "日野森志歩", "日野森志步"}},
	{ID: "歌唱者-05", Name: "花里みのり", Aliases: []string{"minori", "Hanasato Minori", "Minori Hanasato", "花里みのり", "花里实乃理"}},
	{ID: "歌唱者-06", Name: "桐谷遥", Aliases: []string{"haruka", "Kiritani Haruka", "Haruka Kiritani", "桐谷遥", "桐谷遙"}},
	{ID: "歌唱者-07", Name: "桃井愛莉", Aliases: []string{"airi", "Momoi Airi", "Airi Momoi", "桃井愛莉", "桃井爱莉"}},
	{ID: "歌唱者-08", Name: "日野森雫", Aliases: []string{"shizuku", "Hinomori Shizuku", "Shizuku Hinomori", "日野森雫"}},
	{ID: "歌唱者-09", Name: "小豆沢こはね", Aliases: []string{"kohane", "Azusawa Kohane", "Kohane Azusawa", "小豆沢こはね", "小豆泽心羽"}},
	{ID: "歌唱者-10", Name: "白石杏", Aliases: []string{"an", "Shiraishi An", "An Shiraishi", "白石杏"}},
	{ID: "歌唱者-11", Name: "東雲彰人", Aliases: []string{"akito", "Shinonome Akito", "Akito Shinonome", "東雲彰人", "东云彰人"}},
	{ID: "歌唱者-12", Name: "青柳冬弥", Aliases: []string{"toya", "Aoyagi Toya", "Toya Aoyagi", "青柳冬弥"}},
	{ID: "歌唱者-13", Name: "天馬司", Aliases: []string{"tsukasa", "Tenma Tsukasa", "Tsukasa Tenma", "天馬司", "天马司"}},
	{ID: "歌唱者-14", Name: "鳳えむ", Aliases: []string{"emu", "Otori Emu", "Emu Otori", "鳳えむ", "凤笑梦"}},
	{ID: "歌唱者-15", Name: "草薙寧々", Aliases: []string{"nene", "Kusanagi Nene", "Nene Kusanagi", "草薙寧々", "草薙宁宁"}},
	{ID: "歌唱者-16", Name: "神代類", Aliases: []string{"rui", "Kamishiro Rui", "Rui Kamishiro", "神代類", "神代类"}},
	{ID: "歌唱者-17", Name: "宵崎奏", Aliases: []string{"kanade", "Yoisaki Kanade", "Kanade Yoisaki", "宵崎奏"}},
	{ID: "歌唱者-18", Name: "朝比奈まふゆ", Aliases: []string{"mafuyu", "Asahina Mafuyu", "Mafuyu Asahina", "朝比奈まふゆ", "朝比奈真冬"}},
	{ID: "歌唱者-19", Name: "東雲絵名", Aliases: []string{"ena", "Shinonome Ena", "Ena Shinonome", "東雲絵名", "东云绘名"}},
	{ID: "歌唱者-20", Name: "暁山瑞希", Aliases: []string{"mizuki", "Akiyama Mizuki", "Mizuki Akiyama", "暁山瑞希", "晓山瑞希"}},
	{ID: "歌唱者-21", Name: "初音ミク", Aliases: []string{"miku", "Hatsune Miku", "Miku Hatsune", "初音ミク", "初音未来", "初音未來"}},
	{ID: "歌唱者-22", Name: "鏡音リン", Aliases: []string{"rin", "Kagamine Rin", "Rin Kagamine", "鏡音リン", "镜音铃", "鏡音鈴"}},
	{ID: "歌唱者-23", Name: "鏡音レン", Aliases: []string{"len", "Kagamine Len", "Len Kagamine", "鏡音レン", "镜音连", "鏡音連"}},
	{ID: "歌唱者-24", Name: "巡音ルカ", Aliases: []string{"luka", "Megurine Luka", "Luka Megurine", "巡音ルカ", "巡音流歌"}},
	{ID: "歌唱者-25", Name: "MEIKO", Aliases: []string{"meiko", "MEIKO"}},
	{ID: "歌唱者-26", Name: "KAITO", Aliases: []string{"kaito", "KAITO"}},
}

// NormalizePersistedPerformerMetadata returns a copy whose performer values are
// safe to serialize. Audited Project SEKAI identities receive Japanese display
// names and stable non-Latin IDs; the closed external singer registry receives
// stable 外部歌唱者-NN IDs and official display names. If a source legend or
// reference cannot be tied to either audited set, performer segmentation is
// omitted while lyric text and ruby are retained. Conflicting audited
// identities fail closed without echoing source values.
func NormalizePersistedPerformerMetadata(full model.LyricsSourceFull) (model.LyricsSourceFull, error) {
	result := full
	result.Performers = make([]model.LyricsSourcePerformer, len(full.Performers))
	remapped := make(map[string]string, len(full.Performers))
	seenPersisted := make(map[string]struct{}, len(full.Performers))
	omitSegmentation := false
	for index, performer := range full.Performers {
		persisted, known, err := normalizePersistedPerformer(performer)
		if err != nil {
			return model.LyricsSourceFull{}, ErrUnsafePerformerMetadata
		}
		if _, duplicate := remapped[performer.PerformerID]; duplicate {
			return model.LyricsSourceFull{}, ErrUnsafePerformerMetadata
		}
		if !known {
			omitSegmentation = true
			continue
		}
		if _, duplicate := seenPersisted[persisted.PerformerID]; duplicate {
			return model.LyricsSourceFull{}, ErrUnsafePerformerMetadata
		}
		remapped[performer.PerformerID] = persisted.PerformerID
		seenPersisted[persisted.PerformerID] = struct{}{}
		result.Performers[index] = persisted
	}
	if omitSegmentation {
		return lyricsSourceFullWithoutPerformerSegmentation(full)
	}

	result.Lines = make([]model.LyricsSourceFullLine, len(full.Lines))
	for lineIndex, line := range full.Lines {
		result.Lines[lineIndex] = line
		result.Lines[lineIndex].Segments = make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			result.Lines[lineIndex].Segments[segmentIndex] = segment
			result.Lines[lineIndex].Segments[segmentIndex].Ruby = append([]model.LyricsSourceRubySpan{}, segment.Ruby...)
			for spanIndex, span := range segment.Ruby {
				if span.ReadingEvidence != nil {
					evidence := *span.ReadingEvidence
					result.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = &evidence
				}
			}
			ids, found := RemapPersistedPerformerIDs(segment.PerformerIDs, remapped)
			if !found {
				return lyricsSourceFullWithoutPerformerSegmentation(full)
			}
			result.Lines[lineIndex].Segments[segmentIndex].PerformerIDs = ids
		}
		ids, found := RemapPersistedPerformerIDs(line.TrailingPerformerIDs, remapped)
		if !found {
			return lyricsSourceFullWithoutPerformerSegmentation(full)
		}
		result.Lines[lineIndex].TrailingPerformerIDs = ids
	}
	if err := validatePerformerNormalizationInput(result); err != nil {
		return model.LyricsSourceFull{}, ErrUnsafePerformerMetadata
	}
	return result, nil
}

// ValidatePersistedPerformerMetadata rejects serialized source-local performer
// values. It intentionally reports only the closed boundary error, never the
// prohibited performer ID or display name.
func ValidatePersistedPerformerMetadata(full model.LyricsSourceFull) error {
	normalized, err := NormalizePersistedPerformerMetadata(full)
	if err != nil || len(normalized.Performers) != len(full.Performers) || len(normalized.Lines) != len(full.Lines) {
		return ErrUnsafePerformerMetadata
	}
	for index := range full.Performers {
		if normalized.Performers[index] != full.Performers[index] {
			return ErrUnsafePerformerMetadata
		}
	}
	for lineIndex := range full.Lines {
		if len(normalized.Lines[lineIndex].Segments) != len(full.Lines[lineIndex].Segments) ||
			!StringsEqual(normalized.Lines[lineIndex].TrailingPerformerIDs, full.Lines[lineIndex].TrailingPerformerIDs) {
			return ErrUnsafePerformerMetadata
		}
		for segmentIndex := range full.Lines[lineIndex].Segments {
			if !StringsEqual(
				normalized.Lines[lineIndex].Segments[segmentIndex].PerformerIDs,
				full.Lines[lineIndex].Segments[segmentIndex].PerformerIDs,
			) {
				return ErrUnsafePerformerMetadata
			}
		}
	}
	return nil
}

func validatePerformerNormalizationInput(full model.LyricsSourceFull) error {
	contract := full
	if contract.Version.Kind == "vocaloid" {
		contract.Version.Kind = "sekai"
	}
	if err := model.ValidateLyricsSourceFull(contract); err != nil {
		return ErrUnsafePerformerMetadata
	}
	return nil
}

func normalizePersistedPerformer(performer model.LyricsSourcePerformer) (model.LyricsSourcePerformer, bool, error) {
	persisted, known, err := NormalizeAuditedPerformerValues(performer.PerformerID, performer.Name)
	if err != nil || !known {
		return model.LyricsSourcePerformer{}, known, err
	}
	return model.LyricsSourcePerformer{
		PerformerID: persisted.ID, Name: persisted.Name, Color: performer.Color,
	}, true, nil
}

func NormalizeAuditedPerformerValues(id, name string) (AuditedPersistedPerformer, bool, error) {
	byID, idKnown := auditedPersistedPerformerForAlias(id)
	byName, nameKnown := auditedPersistedPerformerForAlias(name)
	if idKnown && nameKnown && byID.ID != byName.ID {
		return AuditedPersistedPerformer{}, false, ErrUnsafePerformerMetadata
	}
	// A source-local ID may be remapped when the displayed performer identity is
	// audited. The inverse is intentionally forbidden: a recognized ID must not
	// turn an arbitrary source label into an allowed persisted brand.
	if !nameKnown {
		return AuditedPersistedPerformer{}, false, nil
	}
	return byName, true, nil
}

func auditedPersistedPerformerForAlias(value string) (AuditedPersistedPerformer, bool) {
	key := persistedPerformerAliasKey(value)
	if key == "" {
		return AuditedPersistedPerformer{}, false
	}
	for _, performer := range auditedPersistedPerformers {
		if key == persistedPerformerAliasKey(performer.ID) || key == persistedPerformerAliasKey(performer.Name) {
			return performer, true
		}
		for _, alias := range performer.Aliases {
			if key == persistedPerformerAliasKey(alias) {
				return performer, true
			}
		}
	}
	if performer, found := lyricsperformers.ByAlias(value); found {
		return AuditedPersistedPerformer{
			ID: performer.SourceID, Name: performer.Name, Aliases: append([]string{}, performer.Aliases...),
		}, true
	}
	return AuditedPersistedPerformer{}, false
}

func persistedPerformerAliasKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(norm.NFKC.String(value)), " "))
}

func RemapPersistedPerformerIDs(ids []string, remapped map[string]string) ([]string, bool) {
	if ids == nil {
		return nil, true
	}
	result := make([]string, len(ids))
	for index, id := range ids {
		persisted, found := remapped[id]
		if !found {
			return nil, false
		}
		result[index] = persisted
	}
	return result, true
}

func validateLyricsForPerformerOmission(full model.LyricsSourceFull) error {
	contract := full
	if contract.Version.Kind == "vocaloid" {
		contract.Version.Kind = "sekai"
	}
	contract.Performers = []model.LyricsSourcePerformer{}
	contract.Lines = make([]model.LyricsSourceFullLine, len(full.Lines))
	for lineIndex, line := range full.Lines {
		contract.Lines[lineIndex] = line
		contract.Lines[lineIndex].TrailingPerformerIDs = []string{}
		if line.Segments == nil {
			contract.Lines[lineIndex].Segments = nil
			continue
		}
		contract.Lines[lineIndex].Segments = make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			contract.Lines[lineIndex].Segments[segmentIndex] = segment
			contract.Lines[lineIndex].Segments[segmentIndex].PerformerIDs = []string{}
			contract.Lines[lineIndex].Segments[segmentIndex].Ruby = append([]model.LyricsSourceRubySpan{}, segment.Ruby...)
			for spanIndex, span := range segment.Ruby {
				if span.ReadingEvidence != nil {
					evidence := *span.ReadingEvidence
					contract.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = &evidence
				}
			}
		}
	}
	if err := model.ValidateLyricsSourceFull(contract); err != nil {
		return ErrUnsafePerformerMetadata
	}
	return nil
}

func lyricsSourceFullWithoutPerformerSegmentation(full model.LyricsSourceFull) (model.LyricsSourceFull, error) {
	if err := validateLyricsForPerformerOmission(full); err != nil {
		return model.LyricsSourceFull{}, ErrUnsafePerformerMetadata
	}
	result := full
	result.Performers = []model.LyricsSourcePerformer{}
	result.Lines = make([]model.LyricsSourceFullLine, len(full.Lines))
	for lineIndex, line := range full.Lines {
		ruby := []model.LyricsSourceRubySpan{}
		for _, segment := range line.Segments {
			ruby = append(ruby, segment.Ruby...)
		}
		result.Lines[lineIndex] = line
		result.Lines[lineIndex].Segments = []model.LyricsSourceSegment{{
			Text: line.Text, PerformerIDs: []string{}, Ruby: ruby,
		}}
		result.Lines[lineIndex].TrailingPerformerIDs = []string{}
	}
	if err := model.ValidateLyricsSourceFull(result); err != nil {
		return model.LyricsSourceFull{}, ErrUnsafePerformerMetadata
	}
	return result, nil
}

func StringsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
