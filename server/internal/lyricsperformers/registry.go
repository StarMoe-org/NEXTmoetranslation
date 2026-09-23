package lyricsperformers

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// External is one audited, lyrics-only singer identity. NumericID is reserved
// outside the Project SEKAI game-character range and is shared by the producer
// and the Public lyrics consumer. SourceID is the stable persisted identity
// used in immutable source documents.
type External struct {
	NumericID int
	SourceID  string
	Name      string
	Color     string
	Aliases   []string
}

// The fixed 700-song v14 replay currently encounters exactly these audited
// external Sekaipedia singers in successfully built source documents. Keep the
// sparse numeric IDs aligned with their fixed 外部歌唱者-NN source identities so
// later additions do not renumber published payloads. A color remains empty
// when the fixed source proves the identity and name but supplies no audited
// display color; persistence validators intentionally allow that omission.
var auditedExternal = []External{
	{NumericID: 1001, SourceID: "外部歌唱者-01", Name: "GUMI", Color: "#70B85A", Aliases: []string{"GUMI", "Megpoid"}},
	{NumericID: 1002, SourceID: "外部歌唱者-02", Name: "Kasane Teto", Aliases: []string{"Kasane Teto", "Teto"}},
	{NumericID: 1003, SourceID: "外部歌唱者-03", Name: "flower", Aliases: []string{"flower"}},
	{NumericID: 1004, SourceID: "外部歌唱者-04", Name: "Nenerobo", Aliases: []string{"Nenerobo"}},
	{NumericID: 1006, SourceID: "外部歌唱者-06", Name: "Kamui Gakupo", Aliases: []string{"Kamui Gakupo", "Gakupo", "Gackpo"}},
	{NumericID: 1007, SourceID: "外部歌唱者-07", Name: "KAFU", Color: "#8A8A91", Aliases: []string{"KAFU", "可不"}},
	{NumericID: 1008, SourceID: "外部歌唱者-08", Name: "Gekiyaku", Aliases: []string{"Gekiyaku"}},
	{NumericID: 1009, SourceID: "外部歌唱者-09", Name: "SEKAI", Color: "#4A89A8", Aliases: []string{"SEKAI", "星界"}},
	{NumericID: 1011, SourceID: "外部歌唱者-11", Name: "Zundamon", Color: "#78AF54", Aliases: []string{"Zundamon", "ずんだもん"}},
	{NumericID: 1012, SourceID: "外部歌唱者-12", Name: "Kaai Yuki", Aliases: []string{"Kaai Yuki", "Yuki"}},
	{NumericID: 1013, SourceID: "外部歌唱者-13", Name: "Adachi Rei", Aliases: []string{"Adachi Rei", "Rei"}},
	{NumericID: 1014, SourceID: "外部歌唱者-14", Name: "RIME", Aliases: []string{"RIME", "Rime"}},
	{NumericID: 1015, SourceID: "外部歌唱者-15", Name: "Hanakuma Chifuyu", Aliases: []string{"Hanakuma Chifuyu"}},
	{NumericID: 1016, SourceID: "外部歌唱者-16", Name: "VY1", Aliases: []string{"VY1"}},
	{NumericID: 1017, SourceID: "外部歌唱者-17", Name: "SOLARIA", Aliases: []string{"SOLARIA"}},
	{NumericID: 1018, SourceID: "外部歌唱者-18", Name: "Kotonoha Aoi", Color: "#4D8FCC", Aliases: []string{"Kotonoha Aoi", "Aoi Kotonoha", "琴葉葵"}},
	{NumericID: 1019, SourceID: "外部歌唱者-19", Name: "Kotonoha Akane", Color: "#D75C58", Aliases: []string{"Kotonoha Akane", "Akane Kotonoha", "琴葉茜"}},
	{NumericID: 1028, SourceID: "外部歌唱者-28", Name: "IA", Color: "#FFB1C4", Aliases: []string{"IA", "Ia"}},
	{NumericID: 1030, SourceID: "外部歌唱者-30", Name: "HARU", Color: "#8EC31F", Aliases: []string{"HARU", "Haru", "羽累"}},
	{NumericID: 1031, SourceID: "外部歌唱者-31", Name: "COKO", Color: "#00A3E0", Aliases: []string{"COKO", "Coko", "狐子"}},
}

// All returns a detached copy of the closed audited registry.
func All() []External {
	result := make([]External, len(auditedExternal))
	for index, performer := range auditedExternal {
		result[index] = performer
		result[index].Aliases = append([]string{}, performer.Aliases...)
	}
	return result
}

// ByAlias resolves a persisted source ID, display name, or audited source
// alias without exposing provider-local arbitrary labels.
func ByAlias(value string) (External, bool) {
	key := aliasKey(value)
	if key == "" {
		return External{}, false
	}
	for _, performer := range auditedExternal {
		if key == aliasKey(performer.SourceID) || key == aliasKey(performer.Name) {
			return performer, true
		}
		for _, alias := range performer.Aliases {
			if key == aliasKey(alias) {
				return performer, true
			}
		}
	}
	return External{}, false
}
func aliasKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(norm.NFKC.String(value)), " "))
}
