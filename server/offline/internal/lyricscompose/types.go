// Package lyricscompose provides deterministic, side-effect-free composition
// primitives for fixed lyrics source artifacts. It reuses the model's closed
// reason-code constants but owns no database, store, extractor, or web logic.
package lyricscompose

import "errors"

var (
	ErrInvalidSource        = errors.New("invalid lyrics composition source")
	ErrIdentityMismatch     = errors.New("lyrics composition identity mismatch")
	ErrSequenceKindMismatch = errors.New("lyrics composition sequence kind mismatch")
	ErrVisibleTextMismatch  = errors.New("visible Japanese sequence mismatch")
	ErrInvalidSegmentation  = errors.New("invalid performer segmentation")
	ErrInvalidRuby          = errors.New("invalid ruby component")
	ErrComponentConflict    = errors.New("lyrics component conflict")
	ErrProjectionMissing    = errors.New("Game sequence is not a Full subsequence")
	ErrProjectionAmbiguous  = errors.New("Game sequence has multiple Full subsequence mappings")
	ErrVersionConflict      = errors.New("lyrics version conflict")
	ErrComponentsIncomplete = errors.New("lyrics Full/version components are incomplete")
)

// SequenceKind prevents component composition across Full and Game texts even
// when their visible text happens to be identical. Untagged artifacts may only
// compose with other untagged artifacts.
type SequenceKind string

const (
	SequenceFull     SequenceKind = "full"
	SequenceGame     SequenceKind = "game"
	SequenceUntagged SequenceKind = "untagged"
)

// Identity is adapter-supplied source identity. CatalogSongKey and RenditionKey
// are exact composition gates that keep original/cover (and any other rendition
// distinction) separate. FixedIdentityKey identifies this source's immutable
// fetched artifact and is preserved independently in component provenance.
type Identity struct {
	CatalogSongKey   string
	RenditionKey     string
	FixedIdentityKey string
}

// VisibleLine is authoritative visible text. StanzaBreakBefore is carried from
// the Full-text owner but is not used to make fuzzy text matches.
type VisibleLine struct {
	Text              string
	StanzaBreakBefore bool
}

type Performer struct {
	ID    string
	Name  string
	Color string
}

type Segment struct {
	Text         string
	PerformerIDs []string
}

type SegmentedLine struct {
	Segments             []Segment
	TrailingPerformerIDs []string
}

// Segmentation is a complete component: it must cover every visible line and
// reconstruct each line exactly at grapheme boundaries.
type Segmentation struct {
	Performers []Performer
	Lines      []SegmentedLine
}

type RubySpan struct {
	Text    string
	Reading string
}

// Ruby is represented independently from performer segmentation so compatible
// components from separate fixed-source views can be composed without changing
// authoritative text. Each line's spans must reconstruct that line exactly.
type Ruby struct {
	GeneratorVersion string
	Lines            [][]RubySpan
}

// Source is one pure composition input. SourceKey and FixedIdentityKey are
// provenance fields; catalog song, rendition, sequence kind, and the complete
// visible sequence are the cross-source composition gates.
type Source struct {
	SourceKey       string
	Identity        Identity
	SequenceKind    SequenceKind
	VisibleJapanese []VisibleLine
	Segmentation    *Segmentation
	Ruby            *Ruby
}

type ComponentRef struct {
	SourceKey        string
	FixedIdentityKey string
}

type Provenance struct {
	FullText     ComponentRef
	Segmentation *ComponentRef
	Ruby         *ComponentRef
}

type ComposedSegment struct {
	Text         string
	PerformerIDs []string
	Ruby         []RubySpan
}

type ComposedLine struct {
	Text                 string
	StanzaBreakBefore    bool
	Segments             []ComposedSegment
	TrailingPerformerIDs []string
}

// Result owns authoritative text from the base source and records the exact
// source selected for each optional component. Missing optional components are
// represented losslessly with one unassigned segment and unannotated ruby spans.
type Result struct {
	Identity             Identity
	SequenceKind         SequenceKind
	Performers           []Performer
	RubyGeneratorVersion string
	Lines                []ComposedLine
	Provenance           Provenance
}
