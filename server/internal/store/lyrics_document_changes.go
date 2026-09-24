package store

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// LyricsDocumentChanges compares the detail a document request produces with
// the song as it stands: what the public site serves, else the editable
// database state, else nothing. Both sides are read in the request format, so
// fields the route regenerates (line IDs, sourceTabPaths, provenance metadata,
// revision, updatedAt), boundaries that render identically and what the
// request cannot carry, which GET reports as warnings, are never changes.
// Listed lines are capped across the document; the counts are always complete
// and truncated reports that some entries are not listed. translationEditions
// lists both sides' editions, the default first, when the list or its order
// differs; a song without translationEditions has the edition main labelled
// 默认译本, and a song that had nothing has none.
type LyricsDocumentChanges struct {
	Against             string                           `json:"against"`
	Changed             bool                             `json:"changed"`
	Truncated           bool                             `json:"truncated,omitempty"`
	Source              *LyricsDocumentSourceChange      `json:"source,omitempty"`
	TranslationEditions *LyricsDocumentEditionsChange    `json:"translationEditions,omitempty"`
	RenditionsAdded     []LyricsDocumentRenditionSummary `json:"renditionsAdded,omitempty"`
	RenditionsRemoved   []LyricsDocumentRenditionSummary `json:"renditionsRemoved,omitempty"`
	Renditions          []LyricsDocumentRenditionChange  `json:"renditions,omitempty"`
}

type LyricsDocumentEditionsChange struct {
	Before []LyricsTranslationEditionSummary `json:"before"`
	After  []LyricsTranslationEditionSummary `json:"after"`
}

const (
	LyricsDocumentChangesAgainstServed   = "served"
	LyricsDocumentChangesAgainstDatabase = "database"
	LyricsDocumentChangesAgainstNothing  = "nothing"

	maxLyricsDocumentChangeEntries = 200
	// maxLyricsDocumentChangeCells bounds the line alignment table; a larger
	// differing middle is compared by position.
	maxLyricsDocumentChangeCells = 1 << 20
)

// LyricsDocumentSourceChange names the source revision of both sides as the
// request carries it, with URLs in the canonical form PUT stores. Before is
// null when the song had nothing.
type LyricsDocumentSourceChange struct {
	Before *LyricsDocumentSource `json:"before"`
	After  LyricsDocumentSource  `json:"after"`
}

type LyricsDocumentRenditionSummary struct {
	Key       string `json:"key"`
	Game      string `json:"game"`
	Lines     int    `json:"lines"`
	GameLines int    `json:"gameLines"`
}

type LyricsDocumentTextChange struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

type LyricsDocumentCreditsChange struct {
	Before LyricsDocumentCredits `json:"before"`
	After  LyricsDocumentCredits `json:"after"`
}

// LyricsDocumentRenditionChange names what changed in a rendition present on
// both sides. translationCredits compares the credits the rendition is served
// with in the default edition, whether they come from the document or from the
// rendition; editionCredits compares the other editions' credits by key.
type LyricsDocumentRenditionChange struct {
	Key                string                                 `json:"key"`
	Kind               *LyricsDocumentTextChange              `json:"kind,omitempty"`
	Label              *LyricsDocumentTextChange              `json:"label,omitempty"`
	Game               *LyricsDocumentTextChange              `json:"game,omitempty"`
	TranslationCredits *LyricsDocumentCreditsChange           `json:"translationCredits,omitempty"`
	EditionCredits     map[string]LyricsDocumentCreditsChange `json:"editionCredits,omitempty"`
	Sides              []LyricsDocumentSideChange             `json:"sides,omitempty"`
}

// LyricsDocumentSideChange compares the request lines of one side: full is
// lines and game is gameLines, so a same or cut Game shows as inGame and game
// changes. Added lines carry their position after the request, removed lines
// their position before it, and changed lines both.
type LyricsDocumentSideChange struct {
	Side         string                      `json:"side"`
	LinesBefore  int                         `json:"linesBefore"`
	LinesAfter   int                         `json:"linesAfter"`
	AddedCount   int                         `json:"addedCount"`
	RemovedCount int                         `json:"removedCount"`
	ChangedCount int                         `json:"changedCount"`
	Added        []LyricsDocumentLineSummary `json:"added,omitempty"`
	Removed      []LyricsDocumentLineSummary `json:"removed,omitempty"`
	Changed      []LyricsDocumentLineChange  `json:"changed,omitempty"`
}

// LyricsDocumentLineSummary is an added or removed line: its ja markup, zh,
// stanza break, resolved performers, resolved segments when it has several or
// one sung by other performers than the line, inGame on the Full lines of a
// cut rendition, and its zh in the other editions.
type LyricsDocumentLineSummary struct {
	Line              int                     `json:"line"`
	Japanese          string                  `json:"ja"`
	Chinese           string                  `json:"zh,omitempty"`
	StanzaBreakBefore bool                    `json:"stanzaBreakBefore,omitempty"`
	PerformerIDs      []int                   `json:"performerIds"`
	Segments          []LyricsDocumentSegment `json:"segments,omitempty"`
	InGame            *bool                   `json:"inGame,omitempty"`
	ChineseEditions   map[string]string       `json:"zhEditions,omitempty"`
}

// LyricsDocumentLineChange lists the changed fields of one line: ja (text),
// ruby (readings or how they split the text), segments (segment boundaries),
// performers, zh, zhEditions, stanzaBreakBefore and inGame. ja carries the
// markup when the text or ruby changed, segments the resolved segments when
// boundaries or performers changed, and zhEditions the changed editions.
type LyricsDocumentLineChange struct {
	Before          int                                 `json:"before"`
	After           int                                 `json:"after"`
	Fields          []string                            `json:"fields"`
	Japanese        *LyricsDocumentTextChange           `json:"ja,omitempty"`
	Chinese         *LyricsDocumentTextChange           `json:"zh,omitempty"`
	ChineseEditions map[string]LyricsDocumentTextChange `json:"zhEditions,omitempty"`
	Segments        *LyricsDocumentSegmentsChange       `json:"segments,omitempty"`
}

type LyricsDocumentSegmentsChange struct {
	Before []LyricsDocumentSegment `json:"before"`
	After  []LyricsDocumentSegment `json:"after"`
}

// lyricsDocumentChangeBaseline is the side a publish is compared with, read
// as a request: the served detail when the caller has it, else the editable
// database state.
func (s *Store) lyricsDocumentChangeBaseline(q queryRower, musicID int, served []byte) (*LyricsDocumentRequest, string, error) {
	exporter := &lyricsDocumentExporter{}
	if served != nil {
		detail, _, err := exporter.servedDetail(musicID, served)
		if err != nil {
			return nil, "", err
		}
		request := exporter.request(detail)
		if err := s.withStoredEditionList(q, musicID, &request); err != nil {
			return nil, "", err
		}
		return &request, LyricsDocumentChangesAgainstServed, nil
	}
	detail, found, err := s.lyricsDocumentDatabaseDetail(q, exporter, musicID)
	if err != nil || !found {
		return nil, LyricsDocumentChangesAgainstNothing, err
	}
	request := exporter.request(detail)
	return &request, LyricsDocumentChangesAgainstDatabase, nil
}

// lyricsDocumentChangesBetween compares two songs read as requests by the
// exporter; before is nil when the song had nothing.
func lyricsDocumentChangesBetween(against string, before *LyricsDocumentRequest, afterRequest LyricsDocumentRequest) LyricsDocumentChanges {
	changes := LyricsDocumentChanges{Against: against}
	budget := &lyricsDocumentChangeBudget{remaining: maxLyricsDocumentChangeEntries}
	afterSource := lyricsDocumentCanonicalSource(afterRequest.Source)
	beforeRequest := LyricsDocumentRequest{}
	var beforeSource *LyricsDocumentSource
	if before != nil {
		beforeRequest = *before
		source := lyricsDocumentCanonicalSource(beforeRequest.Source)
		beforeSource = &source
	}
	if beforeSource == nil || *beforeSource != afterSource {
		changes.Source = &LyricsDocumentSourceChange{Before: beforeSource, After: afterSource}
	}
	beforeEditions := []LyricsTranslationEditionSummary{}
	if before != nil {
		beforeEditions = lyricsDocumentEffectiveEditions(beforeRequest)
	}
	if afterEditions := lyricsDocumentEffectiveEditions(afterRequest); !reflect.DeepEqual(beforeEditions, afterEditions) &&
		(before != nil || !lyricsDocumentImplicitEditions(afterRequest.TranslationEditions)) {
		changes.TranslationEditions = &LyricsDocumentEditionsChange{Before: beforeEditions, After: afterEditions}
	}
	beforeByKey := make(map[string]int, len(beforeRequest.Renditions))
	for index, rendition := range beforeRequest.Renditions {
		beforeByKey[rendition.Key] = index
	}
	afterKeys := make(map[string]bool, len(afterRequest.Renditions))
	for _, rendition := range afterRequest.Renditions {
		afterKeys[rendition.Key] = true
		index, found := beforeByKey[rendition.Key]
		if !found {
			changes.RenditionsAdded = append(changes.RenditionsAdded, lyricsDocumentRenditionSummaryOf(rendition))
			continue
		}
		if change, changed := lyricsDocumentRenditionChanges(beforeRequest, beforeRequest.Renditions[index], afterRequest, rendition, budget); changed {
			changes.Renditions = append(changes.Renditions, change)
		}
	}
	for _, rendition := range beforeRequest.Renditions {
		if !afterKeys[rendition.Key] {
			changes.RenditionsRemoved = append(changes.RenditionsRemoved, lyricsDocumentRenditionSummaryOf(rendition))
		}
	}
	sort.Slice(changes.RenditionsAdded, func(i, j int) bool { return changes.RenditionsAdded[i].Key < changes.RenditionsAdded[j].Key })
	sort.Slice(changes.RenditionsRemoved, func(i, j int) bool { return changes.RenditionsRemoved[i].Key < changes.RenditionsRemoved[j].Key })
	sort.Slice(changes.Renditions, func(i, j int) bool { return changes.Renditions[i].Key < changes.Renditions[j].Key })
	changes.Truncated = budget.truncated
	changes.Changed = changes.Source != nil || changes.TranslationEditions != nil || len(changes.RenditionsAdded) > 0 ||
		len(changes.RenditionsRemoved) > 0 || len(changes.Renditions) > 0
	return changes
}

// lyricsDocumentEffectiveEditions is the request's edition list, the implicit
// main edition when it has none.
func lyricsDocumentEffectiveEditions(request LyricsDocumentRequest) []LyricsTranslationEditionSummary {
	if len(request.TranslationEditions) == 0 {
		return []LyricsTranslationEditionSummary{{Key: MainLyricsTranslationEditionKey, Label: MainLyricsTranslationEditionLabel}}
	}
	result := make([]LyricsTranslationEditionSummary, len(request.TranslationEditions))
	for index, edition := range request.TranslationEditions {
		result[index] = LyricsTranslationEditionSummary{Key: edition.Key, Label: strings.TrimSpace(edition.Label)}
	}
	return result
}

// lyricsDocumentEditionCredits are a rendition's trimmed editionCredits
// without empty entries, which PUT stores as no credits.
func lyricsDocumentEditionCredits(rendition LyricsDocumentRendition) map[string]LyricsDocumentCredits {
	result := map[string]LyricsDocumentCredits{}
	for key, credits := range rendition.EditionCredits {
		credits = LyricsDocumentCredits{Translation: strings.TrimSpace(credits.Translation), Proofreading: strings.TrimSpace(credits.Proofreading)}
		if credits != (LyricsDocumentCredits{}) {
			result[key] = credits
		}
	}
	return result
}

// lyricsDocumentLineEditions are a line's zhEditions without empty lines,
// which PUT stores like a missing edition; nil when none is left.
func lyricsDocumentLineEditions(line LyricsDocumentLine) map[string]string {
	var result map[string]string
	for key, text := range line.ChineseEditions {
		if text == "" {
			continue
		}
		if result == nil {
			result = map[string]string{}
		}
		result[key] = text
	}
	return result
}

type lyricsDocumentChangeBudget struct {
	remaining int
	truncated bool
}

// take reserves one listed entry and reports whether it may be listed.
func (b *lyricsDocumentChangeBudget) take() bool {
	if b.remaining == 0 {
		b.truncated = true
		return false
	}
	b.remaining--
	return true
}

func lyricsDocumentRenditionSummaryOf(rendition LyricsDocumentRendition) LyricsDocumentRenditionSummary {
	return LyricsDocumentRenditionSummary{
		Key: rendition.Key, Game: rendition.Game, Lines: len(rendition.Lines), GameLines: len(rendition.GameLines),
	}
}

// lyricsDocumentCanonicalSource writes a source URL PUT accepts in the form
// PUT stores.
func lyricsDocumentCanonicalSource(source LyricsDocumentSource) LyricsDocumentSource {
	source.Title = strings.TrimSpace(source.Title)
	if parsed, err := parseLyricsDocumentSourceURL(source.URL, source.Title); err == nil {
		source.URL = parsed.canonicalURL
	}
	return source
}

// lyricsDocumentEffectiveCredits are the credits PUT publishes the rendition
// with: its own, else the document credits when it has a zh line.
func lyricsDocumentEffectiveCredits(document LyricsDocumentRequest, rendition LyricsDocumentRendition) LyricsDocumentCredits {
	if rendition.TranslationCredits != nil {
		return LyricsDocumentCredits{
			Translation:  strings.TrimSpace(rendition.TranslationCredits.Translation),
			Proofreading: strings.TrimSpace(rendition.TranslationCredits.Proofreading),
		}
	}
	if lyricsDocumentRenditionTranslated(rendition) {
		return LyricsDocumentCredits{
			Translation: strings.TrimSpace(document.TranslationCredit), Proofreading: strings.TrimSpace(document.ProofreadingCredit),
		}
	}
	return LyricsDocumentCredits{}
}

func lyricsDocumentRenditionTranslated(rendition LyricsDocumentRendition) bool {
	for _, lines := range [][]LyricsDocumentLine{rendition.Lines, rendition.GameLines} {
		for _, line := range lines {
			if line.Chinese != "" {
				return true
			}
		}
	}
	return false
}

func lyricsDocumentTextChangeOf(before, after string) *LyricsDocumentTextChange {
	if before == after {
		return nil
	}
	return &LyricsDocumentTextChange{Before: before, After: after}
}

func lyricsDocumentRenditionChanges(beforeDocument LyricsDocumentRequest, before LyricsDocumentRendition,
	afterDocument LyricsDocumentRequest, after LyricsDocumentRendition, budget *lyricsDocumentChangeBudget,
) (LyricsDocumentRenditionChange, bool) {
	change := LyricsDocumentRenditionChange{
		Key:   after.Key,
		Kind:  lyricsDocumentTextChangeOf(before.Kind, after.Kind),
		Label: lyricsDocumentTextChangeOf(before.Label, after.Label),
		Game:  lyricsDocumentTextChangeOf(before.Game, after.Game),
	}
	if beforeCredits, afterCredits := lyricsDocumentEffectiveCredits(beforeDocument, before),
		lyricsDocumentEffectiveCredits(afterDocument, after); beforeCredits != afterCredits {
		change.TranslationCredits = &LyricsDocumentCreditsChange{Before: beforeCredits, After: afterCredits}
	}
	beforeEditionCredits, afterEditionCredits := lyricsDocumentEditionCredits(before), lyricsDocumentEditionCredits(after)
	for _, key := range sortedLyricsDocumentKeys(lyricsDocumentKeyUnion(beforeEditionCredits, afterEditionCredits)) {
		if beforeEditionCredits[key] != afterEditionCredits[key] {
			if change.EditionCredits == nil {
				change.EditionCredits = map[string]LyricsDocumentCreditsChange{}
			}
			change.EditionCredits[key] = LyricsDocumentCreditsChange{Before: beforeEditionCredits[key], After: afterEditionCredits[key]}
		}
	}
	for _, side := range []struct {
		name          string
		before, after []LyricsDocumentLine
	}{{"full", before.Lines, after.Lines}, {"game", before.GameLines, after.GameLines}} {
		full := side.name == "full"
		sideChange, changed := lyricsDocumentSideChanges(side.name,
			lyricsDocumentLineViews(before, side.before, full && before.Game == "cut"),
			lyricsDocumentLineViews(after, side.after, full && after.Game == "cut"), budget)
		if changed {
			change.Sides = append(change.Sides, sideChange)
		}
	}
	changed := change.Kind != nil || change.Label != nil || change.Game != nil || change.TranslationCredits != nil ||
		len(change.EditionCredits) > 0 || len(change.Sides) > 0
	return change, changed
}

func lyricsDocumentKeyUnion[V any](left, right map[string]V) map[string]bool {
	result := make(map[string]bool, len(left)+len(right))
	for key := range left {
		result[key] = true
	}
	for key := range right {
		result[key] = true
	}
	return result
}

// lyricsDocumentLineView is a request line with its performers resolved
// against the rendition default and its markup read into text and ruby.
// inGame is set on the Full lines of a cut rendition.
type lyricsDocumentLineView struct {
	line       LyricsDocumentLine
	text       string
	ruby       []string
	performers []int
	segments   []LyricsDocumentSegment
	lengths    []int
	editions   map[string]string
	inGame     *bool
	key        string
}

func lyricsDocumentLineViews(rendition LyricsDocumentRendition, lines []LyricsDocumentLine, cut bool) []lyricsDocumentLineView {
	defaults := rendition.PerformerIDs
	if defaults == nil {
		defaults = []int{}
	}
	views := make([]lyricsDocumentLineView, len(lines))
	for index, line := range lines {
		performers := line.PerformerIDs
		if performers == nil {
			performers = defaults
		}
		view := lyricsDocumentLineView{line: line, performers: performers, editions: lyricsDocumentLineEditions(line)}
		if cut {
			inGame := line.InGame
			view.inGame = &inGame
		}
		view.text, view.ruby = lyricsDocumentMarkupReading(line.Japanese)
		if line.Segments == nil {
			view.segments = []LyricsDocumentSegment{{Japanese: line.Japanese, PerformerIDs: performers}}
		} else {
			view.segments = make([]LyricsDocumentSegment, len(line.Segments))
			for segmentIndex, segment := range line.Segments {
				view.segments[segmentIndex] = segment
				if segment.PerformerIDs == nil {
					view.segments[segmentIndex].PerformerIDs = performers
				}
			}
		}
		view.lengths = make([]int, len(view.segments))
		for segmentIndex, segment := range view.segments {
			text, _ := lyricsDocumentMarkupReading(segment.Japanese)
			view.lengths[segmentIndex] = len([]rune(text))
		}
		key, _ := json.Marshal([]any{line.Japanese, view.segments, line.Chinese, view.editions, line.StanzaBreakBefore, line.InGame})
		view.key = string(key)
		views[index] = view
	}
	return views
}

// lyricsDocumentMarkupReading reads markup into its text and its ruby as
// base|reading pairs. Unlike the validating parser it accepts any reading, so
// a served ruby the markup cannot express still compares.
func lyricsDocumentMarkupReading(markup string) (string, []string) {
	var text strings.Builder
	var ruby []string
	runes := []rune(markup)
	for index := 0; index < len(runes); index++ {
		current := runes[index]
		if (current == '{' || current == '}') && index+1 < len(runes) && runes[index+1] == current {
			text.WriteRune(current)
			index++
			continue
		}
		if current == '{' {
			end := -1
			for cursor := index + 1; cursor < len(runes) && end < 0; cursor++ {
				if runes[cursor] == '}' {
					end = cursor
				}
			}
			if end > 0 {
				inner := string(runes[index+1 : end])
				if base, _, found := strings.Cut(inner, "|"); found {
					ruby = append(ruby, inner)
					text.WriteString(base)
					index = end
					continue
				}
			}
		}
		text.WriteRune(current)
	}
	return text.String(), ruby
}

func lyricsDocumentLineFields(before, after lyricsDocumentLineView) []string {
	var fields []string
	if before.text != after.text {
		fields = append(fields, "ja")
	}
	if !reflect.DeepEqual(before.ruby, after.ruby) {
		fields = append(fields, "ruby")
	}
	if len(before.segments) != len(after.segments) ||
		before.text == after.text && !reflect.DeepEqual(before.lengths, after.lengths) {
		fields = append(fields, "segments")
	}
	if len(before.segments) == len(after.segments) {
		for index := range before.segments {
			if !sameLyricsDocumentExportIDs(before.segments[index].PerformerIDs, after.segments[index].PerformerIDs) {
				fields = append(fields, "performers")
				break
			}
		}
	}
	if before.line.Chinese != after.line.Chinese {
		fields = append(fields, "zh")
	}
	if !reflect.DeepEqual(before.editions, after.editions) {
		fields = append(fields, "zhEditions")
	}
	if before.line.StanzaBreakBefore != after.line.StanzaBreakBefore {
		fields = append(fields, "stanzaBreakBefore")
	}
	if before.line.InGame != after.line.InGame {
		fields = append(fields, "inGame")
	}
	return fields
}

func lyricsDocumentLineSummaryOf(position int, view lyricsDocumentLineView) LyricsDocumentLineSummary {
	summary := LyricsDocumentLineSummary{
		Line: position, Japanese: view.line.Japanese, Chinese: view.line.Chinese, StanzaBreakBefore: view.line.StanzaBreakBefore,
		PerformerIDs: view.performers, InGame: view.inGame, ChineseEditions: view.editions,
	}
	if len(view.segments) > 1 || len(view.segments) == 1 && !sameLyricsDocumentExportIDs(view.segments[0].PerformerIDs, view.performers) {
		summary.Segments = view.segments
	}
	return summary
}

func lyricsDocumentSideChanges(side string, before, after []lyricsDocumentLineView, budget *lyricsDocumentChangeBudget) (LyricsDocumentSideChange, bool) {
	change := LyricsDocumentSideChange{Side: side, LinesBefore: len(before), LinesAfter: len(after)}
	for _, operation := range lyricsDocumentAlignLines(before, after) {
		switch {
		case operation.before < 0:
			change.AddedCount++
			if budget.take() {
				change.Added = append(change.Added, lyricsDocumentLineSummaryOf(operation.after, after[operation.after]))
			}
		case operation.after < 0:
			change.RemovedCount++
			if budget.take() {
				change.Removed = append(change.Removed, lyricsDocumentLineSummaryOf(operation.before, before[operation.before]))
			}
		default:
			beforeLine, afterLine := before[operation.before], after[operation.after]
			if beforeLine.key == afterLine.key {
				continue
			}
			change.ChangedCount++
			if !budget.take() {
				continue
			}
			fields := lyricsDocumentLineFields(beforeLine, afterLine)
			lineChange := LyricsDocumentLineChange{Before: operation.before, After: operation.after, Fields: fields}
			for _, field := range fields {
				switch field {
				case "ja", "ruby":
					lineChange.Japanese = lyricsDocumentTextChangeOf(beforeLine.line.Japanese, afterLine.line.Japanese)
				case "zh":
					lineChange.Chinese = lyricsDocumentTextChangeOf(beforeLine.line.Chinese, afterLine.line.Chinese)
				case "zhEditions":
					lineChange.ChineseEditions = map[string]LyricsDocumentTextChange{}
					for _, key := range sortedLyricsDocumentKeys(lyricsDocumentKeyUnion(beforeLine.editions, afterLine.editions)) {
						if beforeLine.editions[key] != afterLine.editions[key] {
							lineChange.ChineseEditions[key] = LyricsDocumentTextChange{Before: beforeLine.editions[key], After: afterLine.editions[key]}
						}
					}
				case "segments", "performers":
					lineChange.Segments = &LyricsDocumentSegmentsChange{Before: beforeLine.segments, After: afterLine.segments}
				}
			}
			change.Changed = append(change.Changed, lineChange)
		}
	}
	return change, change.AddedCount+change.RemovedCount+change.ChangedCount > 0
}

// lyricsDocumentLineOperation pairs a line before the request with one after
// it; -1 on one side marks an added or removed line.
type lyricsDocumentLineOperation struct {
	before, after int
}

// lyricsDocumentAlignLines aligns identical lines first, then, between them,
// lines with the same text or the same zh as one changed line, and pairs the
// rest by position, so a split, merge or insertion does not mark every later
// line changed.
func lyricsDocumentAlignLines(before, after []lyricsDocumentLineView) []lyricsDocumentLineOperation {
	var operations []lyricsDocumentLineOperation
	identical := lyricsDocumentCommonLines(len(before), len(after), func(i, j int) bool { return before[i].key == after[j].key })
	similar := func(i, j int) bool {
		return before[i].text == after[j].text || before[i].line.Chinese != "" && before[i].line.Chinese == after[j].line.Chinese
	}
	gap := func(beforeStart, beforeEnd, afterStart, afterEnd int) {
		pairs := lyricsDocumentCommonLines(beforeEnd-beforeStart, afterEnd-afterStart, func(i, j int) bool {
			return similar(beforeStart+i, afterStart+j)
		})
		pairs = append(pairs, lyricsDocumentLineOperation{beforeEnd - beforeStart, afterEnd - afterStart})
		i, j := 0, 0
		for _, pair := range pairs {
			for i < pair.before && j < pair.after {
				operations = append(operations, lyricsDocumentLineOperation{beforeStart + i, afterStart + j})
				i, j = i+1, j+1
			}
			for ; i < pair.before; i++ {
				operations = append(operations, lyricsDocumentLineOperation{beforeStart + i, -1})
			}
			for ; j < pair.after; j++ {
				operations = append(operations, lyricsDocumentLineOperation{-1, afterStart + j})
			}
			if pair.before < beforeEnd-beforeStart {
				operations = append(operations, lyricsDocumentLineOperation{beforeStart + pair.before, afterStart + pair.after})
				i, j = pair.before+1, pair.after+1
			}
		}
	}
	i, j := 0, 0
	for _, pair := range identical {
		gap(i, pair.before, j, pair.after)
		operations = append(operations, pair)
		i, j = pair.before+1, pair.after+1
	}
	gap(i, len(before), j, len(after))
	return operations
}

// lyricsDocumentCommonLines returns a longest common subsequence of index
// pairs under same, after trimming the common prefix and suffix.
func lyricsDocumentCommonLines(n, m int, same func(i, j int) bool) []lyricsDocumentLineOperation {
	var pairs []lyricsDocumentLineOperation
	start := 0
	for start < n && start < m && same(start, start) {
		pairs = append(pairs, lyricsDocumentLineOperation{start, start})
		start++
	}
	endBefore, endAfter := n, m
	for endBefore > start && endAfter > start && same(endBefore-1, endAfter-1) {
		endBefore, endAfter = endBefore-1, endAfter-1
	}
	rows, columns := endBefore-start, endAfter-start
	if rows > 0 && columns > 0 && rows*columns <= maxLyricsDocumentChangeCells {
		width := columns + 1
		table := make([]uint16, (rows+1)*width)
		for i := rows - 1; i >= 0; i-- {
			for j := columns - 1; j >= 0; j-- {
				if same(start+i, start+j) {
					table[i*width+j] = table[(i+1)*width+j+1] + 1
				} else {
					table[i*width+j] = max(table[(i+1)*width+j], table[i*width+j+1])
				}
			}
		}
		for i, j := 0, 0; i < rows && j < columns; {
			switch {
			case same(start+i, start+j):
				pairs = append(pairs, lyricsDocumentLineOperation{start + i, start + j})
				i, j = i+1, j+1
			case table[(i+1)*width+j] >= table[i*width+j+1]:
				i++
			default:
				j++
			}
		}
	}
	for offset := 0; endBefore+offset < n; offset++ {
		pairs = append(pairs, lyricsDocumentLineOperation{endBefore + offset, endAfter + offset})
	}
	return pairs
}
