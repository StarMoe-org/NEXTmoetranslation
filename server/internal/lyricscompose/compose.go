package lyricscompose

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
)

// ValidateSource validates one complete, immutable composition view. Optional
// components are validated against the complete visible sequence before any
// cross-source selection is attempted.
func ValidateSource(source Source) error {
	if strings.TrimSpace(source.SourceKey) == "" || source.SourceKey != strings.TrimSpace(source.SourceKey) ||
		strings.TrimSpace(source.Identity.CatalogSongKey) == "" ||
		strings.TrimSpace(source.Identity.RenditionKey) == "" ||
		strings.TrimSpace(source.Identity.FixedIdentityKey) == "" {
		return fmt.Errorf("%w: missing exact identity", ErrInvalidSource)
	}
	switch source.SequenceKind {
	case SequenceFull, SequenceGame, SequenceUntagged:
	default:
		return fmt.Errorf("%w: unsupported sequence kind %q", ErrInvalidSource, source.SequenceKind)
	}
	if len(source.VisibleJapanese) == 0 {
		return fmt.Errorf("%w: visible Japanese sequence is empty", ErrInvalidSource)
	}
	for index, line := range source.VisibleJapanese {
		if !validVisibleText(line.Text) {
			return fmt.Errorf("%w: visible line %d is invalid", ErrInvalidSource, index+1)
		}
	}
	if source.Segmentation != nil {
		if err := validateSegmentation(*source.Segmentation, source.VisibleJapanese); err != nil {
			return err
		}
	}
	if source.Ruby != nil {
		if err := validateRuby(*source.Ruby, source.VisibleJapanese); err != nil {
			return err
		}
	}
	return nil
}

// Compose supplements only missing components. Every provided source must have
// the same catalog song, rendition, sequence kind, and complete visible Japanese
// sequence as base. Fixed identities may differ across providers and remain
// attached to their selected ComponentRefs. There is no positional or
// normalized-text fallback, and input order never breaks a disagreement.
func Compose(base Source, supplements ...Source) (Result, error) {
	if err := ValidateSource(base); err != nil {
		return Result{}, fmt.Errorf("base source: %w", err)
	}
	canonicalBase, err := canonicalizeCompositionSource(base)
	if err != nil {
		return Result{}, lyricscontract.ErrUnsafePerformerMetadata
	}
	base = canonicalBase
	all := make([]Source, 0, len(supplements)+1)
	all = append(all, base)
	for index, supplement := range supplements {
		if err := ValidateSource(supplement); err != nil {
			return Result{}, fmt.Errorf("supplement %d: %w", index+1, err)
		}
		if supplement.Identity.CatalogSongKey != base.Identity.CatalogSongKey ||
			supplement.Identity.RenditionKey != base.Identity.RenditionKey {
			return Result{}, fmt.Errorf("%w: supplement %q", ErrIdentityMismatch, supplement.SourceKey)
		}
		if supplement.SequenceKind != base.SequenceKind {
			return Result{}, fmt.Errorf("%w: %q is %q, base is %q", ErrSequenceKindMismatch, supplement.SourceKey, supplement.SequenceKind, base.SequenceKind)
		}
		if !visibleSequenceEqual(base.VisibleJapanese, supplement.VisibleJapanese) {
			return Result{}, fmt.Errorf("%w: supplement %q", ErrVisibleTextMismatch, supplement.SourceKey)
		}
		canonicalSupplement, err := canonicalizeCompositionSource(supplement)
		if err != nil {
			return Result{}, lyricscontract.ErrUnsafePerformerMetadata
		}
		all = append(all, canonicalSupplement)
	}

	segmentation, segmentationRef, err := selectSegmentation(all)
	if err != nil {
		return Result{}, err
	}
	ruby, rubyRef, err := selectRuby(all)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Identity:     base.Identity,
		SequenceKind: base.SequenceKind,
		Performers:   []Performer{},
		Lines:        make([]ComposedLine, len(base.VisibleJapanese)),
		Provenance: Provenance{
			FullText: componentRef(base), Segmentation: segmentationRef, Ruby: rubyRef,
		},
	}
	if segmentation != nil {
		result.Performers = clonePerformers(segmentation.Performers)
	}
	if ruby != nil {
		result.RubyGeneratorVersion = ruby.GeneratorVersion
	}

	for lineIndex, visible := range base.VisibleJapanese {
		segments := []Segment{{Text: visible.Text, PerformerIDs: []string{}}}
		trailing := []string{}
		if segmentation != nil {
			segments = segmentation.Lines[lineIndex].Segments
			trailing = segmentation.Lines[lineIndex].TrailingPerformerIDs
		}
		spans := []RubySpan{{Text: visible.Text}}
		if ruby != nil {
			spans = ruby.Lines[lineIndex]
		}
		partitioned, partitionErr := partitionRuby(segments, spans)
		if partitionErr != nil {
			return Result{}, fmt.Errorf("line %d: %w", lineIndex+1, partitionErr)
		}
		line := ComposedLine{
			Text:                 visible.Text,
			StanzaBreakBefore:    visible.StanzaBreakBefore,
			Segments:             make([]ComposedSegment, len(segments)),
			TrailingPerformerIDs: cloneStrings(trailing),
		}
		for segmentIndex, segment := range segments {
			line.Segments[segmentIndex] = ComposedSegment{
				Text: segment.Text, PerformerIDs: cloneStrings(segment.PerformerIDs), Ruby: partitioned[segmentIndex],
			}
		}
		result.Lines[lineIndex] = line
	}
	if err := ValidateResult(result); err != nil {
		return Result{}, err
	}
	return result, nil
}

// ValidateResult proves that composed segmentation and ruby still reconstruct
// authoritative text at grapheme boundaries.
func ValidateResult(result Result) error {
	if strings.TrimSpace(result.Identity.CatalogSongKey) == "" || strings.TrimSpace(result.Identity.RenditionKey) == "" ||
		strings.TrimSpace(result.Identity.FixedIdentityKey) == "" || len(result.Lines) == 0 {
		return fmt.Errorf("%w: incomplete result identity or text", ErrInvalidSource)
	}
	switch result.SequenceKind {
	case SequenceFull, SequenceGame, SequenceUntagged:
	default:
		return fmt.Errorf("%w: unsupported result sequence kind %q", ErrInvalidSource, result.SequenceKind)
	}
	if result.Provenance.FullText.SourceKey == "" ||
		result.Provenance.FullText.FixedIdentityKey != result.Identity.FixedIdentityKey {
		return fmt.Errorf("%w: FullText provenance does not match the base fixed identity", ErrIdentityMismatch)
	}
	for _, item := range []struct {
		component string
		reference *ComponentRef
	}{
		{component: "segmentation", reference: result.Provenance.Segmentation},
		{component: "ruby", reference: result.Provenance.Ruby},
	} {
		if item.reference != nil && (item.reference.SourceKey == "" || item.reference.FixedIdentityKey == "") {
			return fmt.Errorf("%w: %s provenance is incomplete", ErrIdentityMismatch, item.component)
		}
	}
	performers := make(map[string]struct{}, len(result.Performers))
	for _, performer := range result.Performers {
		if performer.ID == "" {
			return fmt.Errorf("%w: empty performer ID", ErrInvalidSegmentation)
		}
		if _, duplicate := performers[performer.ID]; duplicate {
			return fmt.Errorf("%w: duplicate performer", ErrInvalidSegmentation)
		}
		performers[performer.ID] = struct{}{}
	}
	for _, performer := range result.Performers {
		persisted, known, err := lyricscontract.NormalizeAuditedPerformerValues(performer.ID, performer.Name)
		if err != nil || !known || performer.ID != persisted.ID || performer.Name != persisted.Name {
			return lyricscontract.ErrUnsafePerformerMetadata
		}
	}
	hasReading := false
	for lineIndex, line := range result.Lines {
		if !validVisibleText(line.Text) || len(line.Segments) == 0 {
			return fmt.Errorf("%w: result line %d is invalid", ErrInvalidSource, lineIndex+1)
		}
		segmentTexts := make([]string, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			segmentTexts[segmentIndex] = segment.Text
			if err := validatePerformerRefs(segment.PerformerIDs, performers); err != nil {
				return fmt.Errorf("%w: result line %d segment %d: %v", ErrInvalidSegmentation, lineIndex+1, segmentIndex+1, err)
			}
			if len(segment.Ruby) == 0 {
				return fmt.Errorf("%w: result line %d segment %d has no spans", ErrInvalidRuby, lineIndex+1, segmentIndex+1)
			}
			spanTexts := make([]string, len(segment.Ruby))
			for spanIndex, span := range segment.Ruby {
				spanTexts[spanIndex] = span.Text
				if !validRubyReading(span.Reading) {
					return fmt.Errorf("%w: result line %d segment %d span %d has a non-kana reading", ErrInvalidRuby, lineIndex+1, segmentIndex+1, spanIndex+1)
				}
				hasReading = hasReading || span.Reading != ""
			}
			if !reconstructsByGrapheme(segment.Text, spanTexts) {
				return fmt.Errorf("%w: result line %d segment %d spans do not reconstruct text", ErrInvalidRuby, lineIndex+1, segmentIndex+1)
			}
		}
		if !reconstructsByGrapheme(line.Text, segmentTexts) {
			return fmt.Errorf("%w: result line %d segments do not reconstruct text", ErrInvalidSegmentation, lineIndex+1)
		}
		if err := validatePerformerRefs(line.TrailingPerformerIDs, performers); err != nil {
			return fmt.Errorf("%w: result line %d trailing performers: %v", ErrInvalidSegmentation, lineIndex+1, err)
		}
	}
	if hasReading && strings.TrimSpace(result.RubyGeneratorVersion) == "" {
		return fmt.Errorf("%w: readings require a generator version", ErrInvalidRuby)
	}
	return nil
}

func validateSegmentation(segmentation Segmentation, visible []VisibleLine) error {
	if len(segmentation.Lines) != len(visible) {
		return fmt.Errorf("%w: line count %d, want %d", ErrInvalidSegmentation, len(segmentation.Lines), len(visible))
	}
	performers := make(map[string]struct{}, len(segmentation.Performers))
	for _, performer := range segmentation.Performers {
		if performer.ID == "" || strings.TrimSpace(performer.ID) != performer.ID {
			return fmt.Errorf("%w: invalid performer ID", ErrInvalidSegmentation)
		}
		if _, duplicate := performers[performer.ID]; duplicate {
			return fmt.Errorf("%w: duplicate performer", ErrInvalidSegmentation)
		}
		performers[performer.ID] = struct{}{}
	}
	for lineIndex, line := range segmentation.Lines {
		if len(line.Segments) == 0 {
			return fmt.Errorf("%w: line %d has no segments", ErrInvalidSegmentation, lineIndex+1)
		}
		parts := make([]string, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			parts[segmentIndex] = segment.Text
			if err := validatePerformerRefs(segment.PerformerIDs, performers); err != nil {
				return fmt.Errorf("%w: line %d segment %d: %v", ErrInvalidSegmentation, lineIndex+1, segmentIndex+1, err)
			}
		}
		if !reconstructsByGrapheme(visible[lineIndex].Text, parts) {
			return fmt.Errorf("%w: line %d does not reconstruct visible text at grapheme boundaries", ErrInvalidSegmentation, lineIndex+1)
		}
		if err := validatePerformerRefs(line.TrailingPerformerIDs, performers); err != nil {
			return fmt.Errorf("%w: line %d trailing performers: %v", ErrInvalidSegmentation, lineIndex+1, err)
		}
	}
	return nil
}

func validateRuby(ruby Ruby, visible []VisibleLine) error {
	if len(ruby.Lines) != len(visible) {
		return fmt.Errorf("%w: line count %d, want %d", ErrInvalidRuby, len(ruby.Lines), len(visible))
	}
	hasReading := false
	for lineIndex, spans := range ruby.Lines {
		if len(spans) == 0 {
			return fmt.Errorf("%w: line %d has no spans", ErrInvalidRuby, lineIndex+1)
		}
		parts := make([]string, len(spans))
		for spanIndex, span := range spans {
			parts[spanIndex] = span.Text
			if !validRubyReading(span.Reading) {
				return fmt.Errorf("%w: line %d span %d has a non-kana reading", ErrInvalidRuby, lineIndex+1, spanIndex+1)
			}
			hasReading = hasReading || span.Reading != ""
		}
		if !reconstructsByGrapheme(visible[lineIndex].Text, parts) {
			return fmt.Errorf("%w: line %d does not reconstruct visible text at grapheme boundaries", ErrInvalidRuby, lineIndex+1)
		}
	}
	if hasReading && strings.TrimSpace(ruby.GeneratorVersion) == "" {
		return fmt.Errorf("%w: readings require a generator version", ErrInvalidRuby)
	}
	return nil
}

func selectSegmentation(sources []Source) (*Segmentation, *ComponentRef, error) {
	var selected *Segmentation
	var ref *ComponentRef
	baseOwnsComponent := len(sources) > 0 && sources[0].Segmentation != nil
	for _, source := range sources {
		if source.Segmentation == nil {
			continue
		}
		if selected == nil {
			copy := cloneSegmentation(*source.Segmentation)
			selected = &copy
			value := componentRef(source)
			ref = &value
			continue
		}
		if !segmentationEqual(*selected, *source.Segmentation) {
			return nil, nil, fmt.Errorf("%w: performer segmentation differs between %q and %q", ErrComponentConflict, ref.SourceKey, source.SourceKey)
		}
		if !baseOwnsComponent && source.SourceKey < ref.SourceKey {
			value := componentRef(source)
			ref = &value
		}
	}
	return selected, ref, nil
}

func selectRuby(sources []Source) (*Ruby, *ComponentRef, error) {
	var selected *Ruby
	var ref *ComponentRef
	baseOwnsComponent := len(sources) > 0 && sources[0].Ruby != nil
	for _, source := range sources {
		if source.Ruby == nil {
			continue
		}
		if selected == nil {
			copy := cloneRuby(*source.Ruby)
			selected = &copy
			value := componentRef(source)
			ref = &value
			continue
		}
		if !rubyEqual(*selected, *source.Ruby) {
			return nil, nil, fmt.Errorf("%w: ruby differs between %q and %q", ErrComponentConflict, ref.SourceKey, source.SourceKey)
		}
		if !baseOwnsComponent && source.SourceKey < ref.SourceKey {
			value := componentRef(source)
			ref = &value
		}
	}
	return selected, ref, nil
}

func partitionRuby(segments []Segment, spans []RubySpan) ([][]RubySpan, error) {
	result := make([][]RubySpan, len(segments))
	spanClusters := make([][]string, len(spans))
	for index, span := range spans {
		spanClusters[index] = splitGraphemes(span.Text)
	}
	spanIndex, spanOffset := 0, 0
	for segmentIndex, segment := range segments {
		remaining := len(splitGraphemes(segment.Text))
		for remaining > 0 {
			if spanIndex >= len(spans) {
				return nil, fmt.Errorf("%w: ruby ended before segmentation", ErrComponentConflict)
			}
			available := len(spanClusters[spanIndex]) - spanOffset
			take := available
			if take > remaining {
				take = remaining
			}
			span := spans[spanIndex]
			if span.Reading != "" && (spanOffset != 0 || take != available) {
				return nil, fmt.Errorf("%w: annotated ruby span crosses a performer segment boundary", ErrComponentConflict)
			}
			result[segmentIndex] = append(result[segmentIndex], RubySpan{
				Text: joinClusters(spanClusters[spanIndex][spanOffset : spanOffset+take]), Reading: span.Reading,
			})
			remaining -= take
			spanOffset += take
			if spanOffset == len(spanClusters[spanIndex]) {
				spanIndex++
				spanOffset = 0
			}
		}
	}
	if spanIndex != len(spans) || spanOffset != 0 {
		return nil, fmt.Errorf("%w: ruby continues after segmentation", ErrComponentConflict)
	}
	return result, nil
}

func validVisibleText(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}

func validRubyReading(value string) bool {
	if value == "" {
		return true
	}
	if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	hasKana := false
	for _, current := range value {
		switch {
		case unicode.In(current, unicode.Hiragana, unicode.Katakana):
			hasKana = true
		case current == 'ー' || current == '・':
			if !hasKana {
				return false
			}
		case unicode.Is(unicode.Mn, current) || unicode.Is(unicode.Mc, current):
			if !hasKana {
				return false
			}
		default:
			return false
		}
	}
	return hasKana
}

func validatePerformerRefs(references []string, performers map[string]struct{}) error {
	seen := make(map[string]struct{}, len(references))
	for _, performerID := range references {
		if performerID == "" || strings.TrimSpace(performerID) != performerID {
			return errors.New("invalid performer ID")
		}
		if _, duplicate := seen[performerID]; duplicate {
			return errors.New("duplicate performer ID")
		}
		seen[performerID] = struct{}{}
		if _, exists := performers[performerID]; !exists {
			return errors.New("unknown performer ID")
		}
	}
	return nil
}

func visibleSequenceEqual(left, right []VisibleLine) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Text != right[index].Text {
			return false
		}
	}
	return true
}

func segmentationEqual(left, right Segmentation) bool {
	if len(left.Performers) != len(right.Performers) || len(left.Lines) != len(right.Lines) {
		return false
	}
	for index := range left.Performers {
		if left.Performers[index] != right.Performers[index] {
			return false
		}
	}
	for index := range left.Lines {
		if !segmentsEqual(left.Lines[index].Segments, right.Lines[index].Segments) ||
			!lyricscontract.StringsEqual(left.Lines[index].TrailingPerformerIDs, right.Lines[index].TrailingPerformerIDs) {
			return false
		}
	}
	return true
}

func segmentsEqual(left, right []Segment) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Text != right[index].Text || !lyricscontract.StringsEqual(left[index].PerformerIDs, right[index].PerformerIDs) {
			return false
		}
	}
	return true
}

func rubyEqual(left, right Ruby) bool {
	if left.GeneratorVersion != right.GeneratorVersion || len(left.Lines) != len(right.Lines) {
		return false
	}
	for lineIndex := range left.Lines {
		if len(left.Lines[lineIndex]) != len(right.Lines[lineIndex]) {
			return false
		}
		for spanIndex := range left.Lines[lineIndex] {
			if left.Lines[lineIndex][spanIndex] != right.Lines[lineIndex][spanIndex] {
				return false
			}
		}
	}
	return true
}

func componentRef(source Source) ComponentRef {
	return ComponentRef{SourceKey: source.SourceKey, FixedIdentityKey: source.Identity.FixedIdentityKey}
}

func cloneSegmentation(input Segmentation) Segmentation {
	result := Segmentation{Performers: clonePerformers(input.Performers), Lines: make([]SegmentedLine, len(input.Lines))}
	for lineIndex, line := range input.Lines {
		result.Lines[lineIndex] = SegmentedLine{
			Segments: make([]Segment, len(line.Segments)), TrailingPerformerIDs: cloneStrings(line.TrailingPerformerIDs),
		}
		for segmentIndex, segment := range line.Segments {
			result.Lines[lineIndex].Segments[segmentIndex] = Segment{Text: segment.Text, PerformerIDs: cloneStrings(segment.PerformerIDs)}
		}
	}
	return result
}

func cloneRuby(input Ruby) Ruby {
	result := Ruby{GeneratorVersion: input.GeneratorVersion, Lines: make([][]RubySpan, len(input.Lines))}
	for index, line := range input.Lines {
		result.Lines[index] = append([]RubySpan{}, line...)
	}
	return result
}

func clonePerformers(input []Performer) []Performer {
	return append([]Performer{}, input...)
}

func cloneStrings(input []string) []string {
	return append([]string{}, input...)
}
