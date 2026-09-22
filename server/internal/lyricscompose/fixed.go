package lyricscompose

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

// FixedArtifactInput is one exact fetched provider artifact adapted to the
// provider-independent composition boundary. SourceKey is the unique artifact
// key; LogicalRenditionKey remains the cross-provider rendition gate.
type FixedArtifactInput struct {
	SourceKey           string
	LogicalRenditionKey string
	Fixed               lyricssource.FixedRevision
}

// FixedArtifactComponents records the unique artifact selected for every
// composed component. Empty optional fields mean that component is absent.
type FixedArtifactComponents struct {
	FullText              string
	GameText              string
	AlternateVocals       string
	PerformerSegmentation string
	GameProjection        string
	Ruby                  string
	VersionEvidence       string
}

// FixedArtifactComposition is the deterministic result of resolving version
// evidence and composing compatible Full-text component views.
type FixedArtifactComposition struct {
	ReasonCode         model.LyricsSourceVersionReasonCode
	Full               model.LyricsSourceFull
	Game               *model.LyricsSourceFull
	GameProjection     *model.LyricsSourceGameProjection
	AlternateVocals    []model.LyricsSourceAlternateVocal
	PrivateReview      *model.LyricsSourcePrivateReview
	Renditions         []model.LyricsSourceRendition
	Components         FixedArtifactComponents
	SelectedSourceKeys []string
}

type fixedComponentMetadata struct {
	provider             model.LyricsSourceProvider
	revisionTimestamp    time.Time
	hasRevisionTimestamp bool
}

type fixedArtifactView struct {
	input                FixedArtifactInput
	full                 *model.LyricsSourceFull
	gameFull             *model.LyricsSourceFull
	game                 []VisibleLine
	alternateVocals      []model.LyricsSourceAlternateVocal
	source               *Source
	explicitResolution   *VersionResolution
	privateReview        *model.LyricsSourcePrivateReview
	fullMetadata         fixedComponentMetadata
	segmentationMetadata fixedComponentMetadata
	gameMetadata         fixedComponentMetadata
	rubyMetadata         fixedComponentMetadata
	versionMetadata      fixedComponentMetadata
}

type fixedVersionPlan struct {
	resolution   VersionResolution
	fullOwner    fixedArtifactView
	versionOwner fixedArtifactView
	gameOwner    fixedArtifactView
}

type fixedComponentKind uint8

const (
	fixedComponentFull fixedComponentKind = iota
	fixedComponentSegmentation
	fixedComponentGame
	fixedComponentRuby
	fixedComponentVersion
)

// ComposeFixedArtifacts resolves each component over exact fetched artifacts.
// Sekaipedia outranks fallback providers only when that component is complete,
// aligned to the authoritative Full sequence, and not canonically stale.
// Artifacts that do not supply a final component are omitted from
// SelectedSourceKeys so manifests retain no provenance-only decorations.
func ComposeFixedArtifacts(inputs []FixedArtifactInput) (FixedArtifactComposition, error) {
	if fixedArtifactInputsContainV3(inputs) {
		return composeFixedArtifactRenditions(inputs)
	}
	return composeFixedArtifactsV2(inputs)
}

func composeFixedArtifactsV2(inputs []FixedArtifactInput) (FixedArtifactComposition, error) {
	if len(inputs) == 0 {
		return FixedArtifactComposition{}, fmt.Errorf("%w: no fixed artifacts", ErrInvalidSource)
	}
	views := make([]fixedArtifactView, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for index, input := range inputs {
		if strings.TrimSpace(input.SourceKey) == "" || input.SourceKey != strings.TrimSpace(input.SourceKey) ||
			strings.TrimSpace(input.LogicalRenditionKey) == "" || input.LogicalRenditionKey != strings.TrimSpace(input.LogicalRenditionKey) {
			return FixedArtifactComposition{}, fmt.Errorf("%w: fixed artifact identity is incomplete", ErrInvalidSource)
		}
		if _, duplicate := seen[input.SourceKey]; duplicate {
			return FixedArtifactComposition{}, fmt.Errorf("%w: duplicate fixed artifact key %q", ErrInvalidSource, input.SourceKey)
		}
		seen[input.SourceKey] = struct{}{}
		view, err := fixedView(input)
		if err != nil {
			if errors.Is(err, ErrInvalidSegmentation) {
				return FixedArtifactComposition{}, fmt.Errorf("%w: invalid fixed performer segmentation", ErrInvalidSegmentation)
			}
			return FixedArtifactComposition{}, err
		}
		views[index] = view
	}
	sort.Slice(views, func(left, right int) bool { return views[left].input.SourceKey < views[right].input.SourceKey })
	if gameOnly, found, err := composeGameOnlyFixedArtifacts(views); err != nil {
		return FixedArtifactComposition{}, err
	} else if found {
		return gameOnly, nil
	}

	authorityViews, fallbackViews := splitFixedAuthorityViews(views)
	fallbackPlan, hasFallbackPlan, err := resolveFixedVersionPlan(fallbackViews)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), err)
	}
	authorityPlan, hasAuthorityPlan, err := resolveFixedVersionPlan(authorityViews)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), err)
	}
	if !hasFallbackPlan && !hasAuthorityPlan {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), ErrVersionConflict)
	}

	plan := authorityPlan
	if hasFallbackPlan {
		plan = fallbackPlan
		if hasAuthorityPlan && fixedVersionPlanAligns(authorityPlan, fallbackPlan.fullOwner) &&
			!fixedVersionPlanIsStale(authorityPlan, fallbackPlan.fullOwner) {
			plan = fixedVersionPlan{
				resolution: authorityPlan.resolution, fullOwner: fallbackPlan.fullOwner,
				versionOwner: authorityPlan.versionOwner, gameOwner: authorityPlan.gameOwner,
			}
		}
	}
	fullOwner := plan.fullOwner
	if fullOwner.source == nil || fullOwner.full == nil || !equalVisibleTexts(plan.resolution.Full, fullOwner.source.VisibleJapanese) {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), ErrVisibleTextMismatch)
	}

	version, _, err := selectFixedRenditionStructure(views, fullOwner)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), err)
	}
	segmentationView, hasSegmentation, err := selectFixedSegmentation(views, fullOwner, version.Kind)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), err)
	}
	rubyView, hasRuby, err := selectFixedRuby(views, fullOwner)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), err)
	}

	base := *fullOwner.source
	base.Segmentation = nil
	base.Ruby = nil
	supplements := selectedFixedSupplements(base, segmentationView, hasSegmentation, rubyView, hasRuby)
	composed, err := Compose(base, supplements...)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), err)
	}
	if !equalComposedTexts(plan.resolution.Full, composed.Lines) {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), ErrVisibleTextMismatch)
	}

	full := composedResultFull(composed, *fullOwner.full, version)
	full, err = lyricscontract.NormalizePersistedPerformerMetadata(full)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), lyricscontract.ErrUnsafePerformerMetadata)
	}
	var projection *model.LyricsSourceGameProjection
	if plan.resolution.GameToFull != nil {
		lineIDs := make([]string, len(plan.resolution.GameToFull))
		for index, position := range plan.resolution.GameToFull {
			if position < 0 || position >= len(full.Lines) {
				return FixedArtifactComposition{}, fixedCompositionConflict(len(views), ErrProjectionMissing)
			}
			lineIDs[index] = full.Lines[position].ID
		}
		projection = &model.LyricsSourceGameProjection{LineIDs: lineIDs}
	}
	var game *model.LyricsSourceFull
	if plan.gameOwner.gameFull != nil {
		gameValue := *plan.gameOwner.gameFull
		for index := range gameValue.Lines {
			gameValue.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
		}
		gameValue, err = lyricscontract.NormalizePersistedPerformerMetadata(gameValue)
		if err != nil {
			return FixedArtifactComposition{}, fixedCompositionConflict(len(views), lyricscontract.ErrUnsafePerformerMetadata)
		}
		if plan.resolution.Game != nil && !lyricscontract.StringsEqual(plan.resolution.Game, visibleTextsFromFull(gameValue)) {
			return FixedArtifactComposition{}, fixedCompositionConflict(len(views), ErrVisibleTextMismatch)
		}
		if err := validateComposedGameFull(gameValue, plan.gameOwner); err != nil {
			return FixedArtifactComposition{}, fixedCompositionConflict(len(views), fmt.Errorf("%w: game Full validation: %v", ErrInvalidSource, err))
		}
		game = &gameValue
	}
	alternateVocals, alternateOwner, err := selectFixedAlternateVocals(views, fullOwner)
	if err != nil {
		return FixedArtifactComposition{}, fixedCompositionConflict(len(views), err)
	}

	components := FixedArtifactComponents{
		FullText: composed.Provenance.FullText.SourceKey, VersionEvidence: plan.versionOwner.input.SourceKey,
	}
	if len(alternateVocals) != 0 {
		components.AlternateVocals = alternateOwner.input.SourceKey
	}
	if game != nil {
		components.GameText = plan.gameOwner.input.SourceKey
	}
	var privateReview *model.LyricsSourcePrivateReview
	hasPersistedPerformerSegmentation := persistedFullHasPerformerSegmentation(full)
	if composed.Provenance.Segmentation != nil && hasPersistedPerformerSegmentation {
		components.PerformerSegmentation = composed.Provenance.Segmentation.SourceKey
		privateReview = clonePrivateReview(segmentationView.privateReview)
	}
	if composed.Provenance.Ruby != nil {
		components.Ruby = composed.Provenance.Ruby.SourceKey
	}
	if projection != nil {
		components.GameProjection = plan.gameOwner.input.SourceKey
	}
	selected := selectedComponentSourceKeys(components)
	return FixedArtifactComposition{
		ReasonCode: plan.resolution.ReasonCode, Full: full, Game: game, GameProjection: projection,
		AlternateVocals: alternateVocals, PrivateReview: privateReview, Components: components,
		SelectedSourceKeys: selected,
	}, nil
}

// BindFixedArtifactComposition updates the authoritative Full artifact's
// extraction/document projection for the staging builder. Its immutable raw
// bytes and provider identity remain unchanged; final cross-artifact component
// provenance is supplied separately through FixedArtifactComponents.
func BindFixedArtifactComposition(primary FixedArtifactInput, composition FixedArtifactComposition) (lyricssource.FixedRevision, error) {
	if primary.SourceKey == "" || primary.SourceKey != composition.Components.FullText {
		return lyricssource.FixedRevision{}, fmt.Errorf("%w: authoritative Full artifact does not match composition", ErrIdentityMismatch)
	}
	if !model.IsValidLyricsSourceVersionReasonCode(composition.ReasonCode) ||
		composition.ReasonCode == model.LyricsSourceVersionReasonVersionConflict {
		return lyricssource.FixedRevision{}, fmt.Errorf("%w: invalid final composition reason", ErrVersionConflict)
	}
	if err := lyricscontract.ValidatePersistedPerformerMetadata(composition.Full); err != nil {
		return lyricssource.FixedRevision{}, fmt.Errorf("%w: unsafe composed performer metadata", ErrInvalidSource)
	}
	fixed := primary.Fixed
	identity := model.LyricsSourceFixedIdentity{
		Provider: fixed.Provider, Origin: fixed.Origin, PageID: fixed.PageID, RevisionID: fixed.RevisionID,
		SHA1: fixed.SHA1, Title: fixed.PageTitle, CanonicalURL: fixed.CanonicalURL,
		RevisionTimestamp: fixedArtifactRevisionTimestamp(primary),
		FetchedAt:         fixed.FetchedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Categories:        append([]string{}, fixed.Categories...), Section: fixed.Section,
		RenditionKey: primary.SourceKey, CompositionRenditionKey: primary.LogicalRenditionKey,
		VersionReason:     fixed.VersionReason,
		IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, fixed.IndexEvidenceRefs...),
	}
	if err := model.ValidateLyricsSourceFixedIdentity(identity); err != nil {
		return lyricssource.FixedRevision{}, fmt.Errorf("%w: authoritative Full identity: %v", ErrInvalidSource, err)
	}
	component := model.LyricsSourceComponentRef{RenditionKey: primary.SourceKey}
	document := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersion,
		ReasonCode:      composition.ReasonCode,
		FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
		Provenance: model.LyricsSourceComponentProvenance{
			FullText: component, VersionEvidence: component,
		},
		Full:            composition.Full,
		Game:            cloneModelFull(composition.Game),
		GameProjection:  cloneModelGameProjection(composition.GameProjection),
		AlternateVocals: model.CloneLyricsSourceAlternateVocals(composition.AlternateVocals),
		PrivateReview:   clonePrivateReview(composition.PrivateReview),
	}
	bindIdentity := func(renditionKey, componentName string) error {
		if renditionKey == "" || renditionKey == primary.SourceKey {
			return nil
		}
		for _, existing := range document.FixedIdentities {
			if existing.RenditionKey == renditionKey {
				return nil
			}
		}
		for _, identities := range [][]model.LyricsSourceFixedIdentity{
			primary.Fixed.FixedIdentities,
			func() []model.LyricsSourceFixedIdentity {
				if primary.Fixed.Document == nil {
					return nil
				}
				return primary.Fixed.Document.FixedIdentities
			}(),
		} {
			for _, documented := range identities {
				if documented.RenditionKey == renditionKey {
					document.FixedIdentities = append(document.FixedIdentities, documented)
					return nil
				}
			}
		}
		return fmt.Errorf("%w: %s identity is not bound to the primary fixed revision", ErrIdentityMismatch, componentName)
	}
	if composition.Components.GameText != "" {
		gameRef := model.LyricsSourceComponentRef{RenditionKey: composition.Components.GameText}
		if err := bindIdentity(composition.Components.GameText, "Game artifact"); err != nil {
			return lyricssource.FixedRevision{}, err
		}
		document.Provenance.GameText = &gameRef
	}
	if len(composition.AlternateVocals) != 0 {
		if composition.Components.AlternateVocals != primary.SourceKey || primary.Fixed.Document == nil ||
			!reflect.DeepEqual(primary.Fixed.Document.AlternateVocals, composition.AlternateVocals) {
			return lyricssource.FixedRevision{}, fmt.Errorf("%w: alternate vocal artifacts are not bound to the primary fixed revision", ErrIdentityMismatch)
		}
		for _, alternate := range composition.AlternateVocals {
			for name, reference := range map[string]*model.LyricsSourceComponentRef{
				"alternate Full artifact":    alternate.Provenance.FullText,
				"alternate Game artifact":    alternate.Provenance.GameText,
				"alternate projection":       alternate.Provenance.GameProjection,
				"alternate version evidence": &alternate.Provenance.VersionEvidence,
			} {
				if reference != nil {
					if err := bindIdentity(reference.RenditionKey, name); err != nil {
						return lyricssource.FixedRevision{}, err
					}
				}
			}
		}
	} else if primary.Fixed.Document != nil && len(primary.Fixed.Document.AlternateVocals) != 0 {
		return lyricssource.FixedRevision{}, fmt.Errorf("%w: composition dropped alternate vocal artifacts", ErrIdentityMismatch)
	}
	if composition.Components.PerformerSegmentation != "" {
		ref := component
		document.Provenance.PerformerSegmentation = &ref
	}
	if composition.Components.Ruby != "" {
		ref := component
		document.Provenance.Ruby = &ref
	}
	if composition.GameProjection != nil {
		ref := component
		document.Provenance.GameProjection = &ref
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		return lyricssource.FixedRevision{}, fmt.Errorf("%w: composed source document: %v", ErrInvalidSource, err)
	}
	fixed.Lines, fixed.Extraction = legacyExtractionFromFull(composition.Full)
	fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)
	fixed.Document = &document
	return fixed, nil
}

func fixedView(input FixedArtifactInput) (fixedArtifactView, error) {
	fixed := input.Fixed
	metadata := fixedInputMetadata(input)
	view := fixedArtifactView{
		input: input, fullMetadata: metadata, segmentationMetadata: metadata,
		gameMetadata: metadata, rubyMetadata: metadata, versionMetadata: metadata,
	}
	if fixed.Document != nil {
		if fixed.VersionReason != "" && !model.IsValidLyricsSourceCandidateVersionReasonCode(fixed.VersionReason) {
			return fixedArtifactView{}, fmt.Errorf("%w: invalid candidate version reason %q", ErrInvalidSource, fixed.VersionReason)
		}
		if err := model.ValidateLyricsSourceDocument(*fixed.Document); err != nil {
			return fixedArtifactView{}, fmt.Errorf("%w: source document: %v", ErrInvalidSource, err)
		}
		identities := fixedDocumentIdentities(*fixed.Document)
		view.alternateVocals = model.CloneLyricsSourceAlternateVocals(fixed.Document.AlternateVocals)
		fullIdentity, hasDocumentFull := identities[fixed.Document.Provenance.FullText.RenditionKey]
		gameTextRef := fixed.Document.Provenance.GameText
		if !hasDocumentFull {
			if fixed.Document.Game == nil || gameTextRef == nil {
				return view, nil
			}
			gameIdentity, found := identities[gameTextRef.RenditionKey]
			if !found || model.LyricsSourceCompositionRenditionKey(gameIdentity) != input.LogicalRenditionKey {
				return view, nil
			}
			game := *fixed.Document.Game
			view.gameFull = &game
			view.game = visibleLinesFromModelFull(game)
			view.gameMetadata = fixedIdentityMetadata(gameIdentity, metadata)
			view.versionMetadata = fixedReferenceMetadata(
				fixed.Document.Provenance.VersionEvidence, identities, metadata,
			)
			view.privateReview = clonePrivateReview(fixed.Document.PrivateReview)
		} else {
			if model.LyricsSourceCompositionRenditionKey(fullIdentity) != input.LogicalRenditionKey {
				return view, nil
			}
			full := fixed.Document.Full
			view.full = &full
			view.fullMetadata = fixedIdentityMetadata(fullIdentity, metadata)
			view.versionMetadata = fixedReferenceMetadata(
				fixed.Document.Provenance.VersionEvidence, identities, metadata,
			)
			if fixed.Document.Provenance.PerformerSegmentation != nil {
				view.segmentationMetadata = fixedReferenceMetadata(
					*fixed.Document.Provenance.PerformerSegmentation, identities, metadata,
				)
			}
			if fixed.Document.Provenance.Ruby != nil {
				view.rubyMetadata = fixedReferenceMetadata(*fixed.Document.Provenance.Ruby, identities, metadata)
			}
			if gameTextRef != nil {
				view.gameMetadata = fixedReferenceMetadata(*gameTextRef, identities, metadata)
			} else if fixed.Document.Provenance.GameProjection != nil {
				view.gameMetadata = fixedReferenceMetadata(
					*fixed.Document.Provenance.GameProjection, identities, metadata,
				)
			}
			view.privateReview = clonePrivateReview(fixed.Document.PrivateReview)
			if fixed.Document.Game != nil {
				game := *fixed.Document.Game
				view.gameFull = &game
				view.game = visibleLinesFromModelFull(game)
			} else if fixed.Document.GameProjection != nil {
				game, err := projectedVisibleLines(full, *fixed.Document.GameProjection)
				if err != nil {
					return fixedArtifactView{}, err
				}
				view.game = game
			}
		}
		resolution, err := fixedDocumentVersionResolution(*fixed.Document)
		if err != nil {
			return fixedArtifactView{}, err
		}
		view.explicitResolution = &resolution
	} else {
		if strings.HasPrefix(input.LogicalRenditionKey, "full-") {
			full, err := modelFullFromExtraction(fixed.Extraction)
			if err != nil {
				if fixed.Provider == model.LyricsSourceProviderSekaipedia && len(fixed.Extraction.Lines) == 0 {
					return view, nil
				}
				return fixedArtifactView{}, err
			}
			view.full = &full
		}
		if strings.HasPrefix(input.LogicalRenditionKey, "game-") {
			game, err := modelFullFromExtraction(fixed.Extraction)
			if err != nil {
				return fixedArtifactView{}, err
			}
			view.gameFull = &game
			view.game = visibleLinesFromExtraction(fixed.Extraction)
		}
		if view.full == nil && len(view.game) == 0 && fixed.Provider == model.LyricsSourceProviderSekaipedia {
			return view, nil
		}
		if !model.IsValidLyricsSourceCandidateVersionReasonCode(fixed.VersionReason) {
			return fixedArtifactView{}, fmt.Errorf("%w: invalid candidate version reason %q", ErrInvalidSource, fixed.VersionReason)
		}
	}
	if view.full != nil {
		authoritativeVocaloidSegmentation := view.privateReview != nil &&
			view.privateReview.PerformerSegmentationEvidence ==
				model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured
		source := sourceFromModelFull(
			input.SourceKey, input.LogicalRenditionKey, *view.full, authoritativeVocaloidSegmentation,
		)
		if err := ValidateSource(source); err != nil {
			return fixedArtifactView{}, fmt.Errorf("fixed artifact source validation: %w", err)
		}
		canonicalSource, err := canonicalizeCompositionSource(source)
		if err != nil {
			return fixedArtifactView{}, lyricscontract.ErrUnsafePerformerMetadata
		}
		view.source = &canonicalSource
	}
	return view, nil
}

func validateComposedGameFull(game model.LyricsSourceFull, owner fixedArtifactView) error {
	if owner.privateReview != nil &&
		owner.privateReview.PerformerSegmentationEvidence ==
			model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured &&
		game.Version.Kind == "vocaloid" {
		return model.ValidateLyricsSourceFullWithAuthoritativeVocaloidSegmentation(game)
	}
	return model.ValidateLyricsSourceFull(game)
}

func composeGameOnlyFixedArtifacts(views []fixedArtifactView) (FixedArtifactComposition, bool, error) {
	gameViews := make([]fixedArtifactView, 0, len(views))
	for _, view := range views {
		if view.full != nil {
			return FixedArtifactComposition{}, false, nil
		}
		if view.gameFull == nil {
			continue
		}
		if view.input.Fixed.VersionReason != model.LyricsSourceVersionReasonTaggedGameOnly &&
			view.input.Fixed.VersionReason != model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid {
			return FixedArtifactComposition{}, false, nil
		}
		gameViews = append(gameViews, view)
	}
	if len(gameViews) == 0 {
		return FixedArtifactComposition{}, false, nil
	}
	if len(gameViews) != 1 {
		return FixedArtifactComposition{}, false, fixedCompositionConflict(len(gameViews), ErrVersionConflict)
	}
	selected := gameViews[0]
	game, err := lyricscontract.NormalizePersistedPerformerMetadata(*selected.gameFull)
	if err != nil {
		return FixedArtifactComposition{}, false, fixedCompositionConflict(len(views), lyricscontract.ErrUnsafePerformerMetadata)
	}
	for index := range game.Lines {
		game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
	}
	if err := validateComposedGameFull(game, selected); err != nil {
		return FixedArtifactComposition{}, false, fixedCompositionConflict(len(views), fmt.Errorf("%w: game-only Full validation: %v", ErrInvalidSource, err))
	}
	components := FixedArtifactComponents{
		GameText: selected.input.SourceKey, VersionEvidence: selected.input.SourceKey,
	}
	if persistedFullHasPerformerSegmentation(game) {
		components.PerformerSegmentation = selected.input.SourceKey
	}
	if fullHasRuby(game) {
		components.Ruby = selected.input.SourceKey
	}
	alternateVocals, alternateOwner, err := selectFixedAlternateVocals(views, selected)
	if err != nil {
		return FixedArtifactComposition{}, false, fixedCompositionConflict(len(views), err)
	}
	if len(alternateVocals) != 0 {
		components.AlternateVocals = alternateOwner.input.SourceKey
	}
	reason := selected.input.Fixed.VersionReason
	return FixedArtifactComposition{
		ReasonCode: reason, Game: &game, AlternateVocals: alternateVocals,
		PrivateReview: clonePrivateReview(selected.privateReview), Components: components,
		SelectedSourceKeys: selectedComponentSourceKeys(components),
	}, true, nil
}

func selectFixedAlternateVocals(views []fixedArtifactView, preferred fixedArtifactView) ([]model.LyricsSourceAlternateVocal, fixedArtifactView, error) {
	var selected []model.LyricsSourceAlternateVocal
	var owner fixedArtifactView
	for _, view := range views {
		if len(view.alternateVocals) == 0 {
			continue
		}
		if selected == nil {
			selected = model.CloneLyricsSourceAlternateVocals(view.alternateVocals)
			owner = view
			continue
		}
		if !reflect.DeepEqual(selected, view.alternateVocals) {
			return nil, fixedArtifactView{}, ErrVersionConflict
		}
		owner = preferredFixedView(owner, view, fixedComponentVersion)
	}
	if len(selected) == 0 {
		return nil, fixedArtifactView{}, nil
	}
	if preferred.input.SourceKey != "" && len(preferred.alternateVocals) != 0 &&
		reflect.DeepEqual(selected, preferred.alternateVocals) {
		owner = preferred
	}
	return selected, owner, nil
}

func splitFixedAuthorityViews(views []fixedArtifactView) (authority, fallback []fixedArtifactView) {
	for _, view := range views {
		if view.versionMetadata.provider == model.LyricsSourceProviderSekaipedia {
			authority = append(authority, view)
		} else {
			fallback = append(fallback, view)
		}
	}
	return authority, fallback
}

func resolveFixedVersionPlan(views []fixedArtifactView) (fixedVersionPlan, bool, error) {
	explicit := make([]fixedArtifactView, 0, len(views))
	raw := make([]fixedArtifactView, 0, len(views))
	for _, view := range views {
		switch {
		case view.explicitResolution != nil:
			explicit = append(explicit, view)
		case view.full != nil || len(view.game) > 0:
			raw = append(raw, view)
		}
	}
	var selected fixedVersionPlan
	hasSelected := false
	if len(explicit) > 0 {
		plan, err := resolveExplicitFixedVersionPlan(explicit)
		if err != nil {
			return fixedVersionPlan{}, false, err
		}
		selected, hasSelected = plan, true
	}
	if len(raw) > 0 {
		fullOwner, versionOwner, gameOwner, evidence, err := resolveFixedVersionEvidence(raw)
		if errors.Is(err, ErrComponentsIncomplete) && len(explicit) > 0 {
			combined := append(append([]fixedArtifactView{}, raw...), explicit...)
			fullOwner, versionOwner, gameOwner, evidence, err = resolveFixedVersionEvidence(combined)
		}
		if err != nil {
			return fixedVersionPlan{}, false, err
		}
		resolution, err := ResolveVersion(evidence)
		if err != nil {
			return fixedVersionPlan{}, false, err
		}
		plan := fixedVersionPlan{
			resolution: resolution, fullOwner: fullOwner, versionOwner: versionOwner, gameOwner: gameOwner,
		}
		if hasSelected {
			if selected.fullOwner.input.LogicalRenditionKey != plan.fullOwner.input.LogicalRenditionKey {
				return fixedVersionPlan{}, false, fmt.Errorf("%w: explicit and raw Full rendition keys differ", ErrVersionConflict)
			}
			switch {
			case fixedVersionResolutionEqual(selected.resolution, plan.resolution):
				selected.fullOwner = preferredFixedView(selected.fullOwner, plan.fullOwner, fixedComponentFull)
				selected.versionOwner = preferredFixedView(selected.versionOwner, plan.versionOwner, fixedComponentVersion)
				selected.gameOwner = preferredFixedView(selected.gameOwner, plan.gameOwner, fixedComponentGame)
			case fixedVersionResolutionEnriches(selected.resolution, plan.resolution):
				plan.fullOwner = preferredFixedView(selected.fullOwner, plan.fullOwner, fixedComponentFull)
				selected = plan
			case fixedVersionResolutionEnriches(plan.resolution, selected.resolution):
				selected.fullOwner = preferredFixedView(selected.fullOwner, plan.fullOwner, fixedComponentFull)
			default:
				return fixedVersionPlan{}, false, fmt.Errorf("%w: explicit and raw version resolutions conflict", ErrVersionConflict)
			}
		} else {
			selected, hasSelected = plan, true
		}
	}
	return selected, hasSelected, nil
}

func resolveExplicitFixedVersionPlan(views []fixedArtifactView) (fixedVersionPlan, error) {
	selected := views[0]
	for _, view := range views[1:] {
		if view.input.LogicalRenditionKey != selected.input.LogicalRenditionKey ||
			!fixedVersionResolutionEqual(*view.explicitResolution, *selected.explicitResolution) {
			return fixedVersionPlan{}, ErrVersionConflict
		}
	}
	fullOwner, versionOwner, gameOwner := selected, selected, selected
	for _, view := range views[1:] {
		fullOwner = preferredFixedView(fullOwner, view, fixedComponentFull)
		versionOwner = preferredFixedView(versionOwner, view, fixedComponentVersion)
		gameOwner = preferredFixedView(gameOwner, view, fixedComponentGame)
	}
	return fixedVersionPlan{
		resolution: cloneVersionResolution(*selected.explicitResolution),
		fullOwner:  fullOwner, versionOwner: versionOwner, gameOwner: gameOwner,
	}, nil
}

func resolveFixedVersionEvidence(views []fixedArtifactView) (
	fullOwner, versionOwner, gameOwner fixedArtifactView,
	evidence VersionEvidence,
	err error,
) {
	var taggedFull, taggedGame, untagged []fixedArtifactView
	for _, view := range views {
		switch view.input.Fixed.VersionReason {
		case model.LyricsSourceVersionReasonTaggedFullAndGame:
			if view.full == nil || len(view.game) == 0 {
				return fullOwner, versionOwner, gameOwner, evidence, ErrProjectionMissing
			}
			taggedFull = append(taggedFull, view)
		case model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid:
			if len(view.game) == 0 {
				return fullOwner, versionOwner, gameOwner, evidence, ErrProjectionMissing
			}
			taggedGame = append(taggedGame, view)
		case model.LyricsSourceVersionReasonUntaggedUncutIdentity,
			model.LyricsSourceVersionReasonUntaggedGameSubset,
			model.LyricsSourceVersionReasonUntaggedFullOnly:
			if view.full == nil && len(view.game) == 0 {
				return fullOwner, versionOwner, gameOwner, evidence, ErrProjectionMissing
			}
			untagged = append(untagged, view)
		default:
			return fullOwner, versionOwner, gameOwner, evidence, ErrVersionConflict
		}
	}

	if len(taggedFull) > 0 {
		if len(taggedGame) != 0 {
			return fullOwner, versionOwner, gameOwner, evidence, ErrVersionConflict
		}
		owner, err := selectEquivalentFixedViews(taggedFull, fixedComponentVersion, func(left, right fixedArtifactView) bool {
			return left.input.LogicalRenditionKey == right.input.LogicalRenditionKey &&
				lyricscontract.StringsEqual(visibleTextsFromFull(*left.full), visibleTextsFromFull(*right.full)) &&
				lyricscontract.StringsEqual(visibleTexts(left.game), visibleTexts(right.game))
		})
		if err != nil {
			return fullOwner, versionOwner, gameOwner, evidence, err
		}
		return owner, owner, owner, VersionEvidence{
			TaggedFull: visibleTextsFromFull(*owner.full), TaggedGame: visibleTexts(owner.game),
		}, nil
	}
	if len(taggedGame) > 0 {
		game, err := selectEquivalentFixedViews(taggedGame, fixedComponentVersion, func(left, right fixedArtifactView) bool {
			return left.input.LogicalRenditionKey == right.input.LogicalRenditionKey &&
				lyricscontract.StringsEqual(visibleTexts(left.game), visibleTexts(right.game))
		})
		if err != nil {
			return fullOwner, versionOwner, gameOwner, evidence, err
		}
		vocaloidViews := vocaloidFullViews(views)
		if len(vocaloidViews) == 0 {
			return fullOwner, versionOwner, gameOwner, evidence, fmt.Errorf("%w: %w", ErrVersionConflict, ErrComponentsIncomplete)
		}
		vocaloid, err := selectEquivalentFixedViews(vocaloidViews, fixedComponentFull, equalFixedFullViews)
		if err != nil {
			return fullOwner, versionOwner, gameOwner, evidence, err
		}
		return vocaloid, game, game, VersionEvidence{
			TaggedGame: visibleTexts(game.game), VocaloidFull: visibleTextsFromFull(*vocaloid.full),
		}, nil
	}

	vocaloidViews := vocaloidFullViews(untagged)
	nonVocaloid := make([]fixedArtifactView, 0, len(untagged))
	for _, view := range untagged {
		if !isVocaloidFull(view) {
			nonVocaloid = append(nonVocaloid, view)
		}
	}
	if len(vocaloidViews) > 0 && len(nonVocaloid) > 0 {
		vocaloid, err := selectEquivalentFixedViews(vocaloidViews, fixedComponentFull, equalFixedFullViews)
		if err != nil {
			return fullOwner, versionOwner, gameOwner, evidence, err
		}
		game, err := selectEquivalentFixedViews(nonVocaloid, fixedComponentVersion, func(left, right fixedArtifactView) bool {
			return left.input.LogicalRenditionKey == right.input.LogicalRenditionKey &&
				lyricscontract.StringsEqual(untaggedVisible(left), untaggedVisible(right))
		})
		if err != nil {
			return fullOwner, versionOwner, gameOwner, evidence, err
		}
		return vocaloid, game, game, VersionEvidence{
			VocaloidFull: visibleTextsFromFull(*vocaloid.full), Untagged: untaggedVisible(game),
		}, nil
	}

	fullCandidates := make([]fixedArtifactView, 0, len(untagged))
	for _, view := range untagged {
		if view.full != nil {
			fullCandidates = append(fullCandidates, view)
		}
	}
	if len(fullCandidates) == 0 {
		return fullOwner, versionOwner, gameOwner, evidence, fmt.Errorf("%w: %w", ErrVersionConflict, ErrComponentsIncomplete)
	}
	owner, err := selectEquivalentFixedViews(fullCandidates, fixedComponentFull, equalFixedFullViews)
	if err != nil {
		return fullOwner, versionOwner, gameOwner, evidence, err
	}
	return owner, owner, owner, VersionEvidence{Untagged: visibleTextsFromFull(*owner.full)}, nil
}

func selectEquivalentFixedViews(
	views []fixedArtifactView,
	component fixedComponentKind,
	equal func(fixedArtifactView, fixedArtifactView) bool,
) (fixedArtifactView, error) {
	if len(views) == 0 {
		return fixedArtifactView{}, ErrVersionConflict
	}
	selected := views[0]
	for _, view := range views[1:] {
		if !equal(selected, view) {
			return fixedArtifactView{}, ErrVersionConflict
		}
		selected = preferredFixedView(selected, view, component)
	}
	return selected, nil
}

func equalFixedFullViews(left, right fixedArtifactView) bool {
	return left.full != nil && right.full != nil && left.input.LogicalRenditionKey == right.input.LogicalRenditionKey &&
		lyricscontract.StringsEqual(visibleTextsFromFull(*left.full), visibleTextsFromFull(*right.full))
}

func fixedCompositionConflict(artifactCount int, cause error) error {
	if artifactCount > 1 {
		return fmt.Errorf("%w: %w", ErrVersionConflict, cause)
	}
	return cause
}

func vocaloidFullViews(views []fixedArtifactView) []fixedArtifactView {
	result := make([]fixedArtifactView, 0, len(views))
	for _, view := range views {
		if isVocaloidFull(view) {
			result = append(result, view)
		}
	}
	return result
}

func isVocaloidFull(view fixedArtifactView) bool {
	return view.full != nil && (view.full.Version.Kind == "vocaloid" || view.input.LogicalRenditionKey == "full-vocaloid")
}

func untaggedVisible(view fixedArtifactView) []string {
	if view.full != nil {
		return visibleTextsFromFull(*view.full)
	}
	return visibleTexts(view.game)
}

func fixedDocumentVersionResolution(document model.LyricsSourceDocument) (VersionResolution, error) {
	resolution := VersionResolution{ReasonCode: document.ReasonCode}
	if len(document.Full.Lines) > 0 {
		resolution.Full = visibleTextsFromFull(document.Full)
	}
	if document.Game != nil {
		resolution.Game = visibleTextsFromFull(*document.Game)
	}
	if document.GameProjection == nil {
		return resolution, nil
	}
	if len(document.Full.Lines) == 0 {
		return VersionResolution{}, ErrProjectionMissing
	}
	positions := make(map[string]int, len(document.Full.Lines))
	for index, line := range document.Full.Lines {
		positions[line.ID] = index
	}
	resolution.GameToFull = make([]int, len(document.GameProjection.LineIDs))
	if resolution.Game == nil {
		resolution.Game = make([]string, len(document.GameProjection.LineIDs))
	}
	if len(resolution.Game) != len(document.GameProjection.LineIDs) {
		return VersionResolution{}, ErrProjectionMissing
	}
	for index, lineID := range document.GameProjection.LineIDs {
		position, found := positions[lineID]
		if !found {
			return VersionResolution{}, ErrProjectionMissing
		}
		if document.Game == nil {
			resolution.Game[index] = document.Full.Lines[position].Text
		} else if document.Game.Lines[index].Text != document.Full.Lines[position].Text {
			return VersionResolution{}, ErrVisibleTextMismatch
		}
		resolution.GameToFull[index] = position
	}
	return resolution, nil
}

func fixedDocumentIdentities(document model.LyricsSourceDocument) map[string]model.LyricsSourceFixedIdentity {
	result := make(map[string]model.LyricsSourceFixedIdentity, len(document.FixedIdentities))
	for _, identity := range document.FixedIdentities {
		result[identity.RenditionKey] = identity
	}
	return result
}

func fixedInputMetadata(input FixedArtifactInput) fixedComponentMetadata {
	metadata := fixedComponentMetadata{provider: input.Fixed.Provider}
	if !input.Fixed.RevisionTimestamp.IsZero() {
		metadata.revisionTimestamp = input.Fixed.RevisionTimestamp.UTC()
		metadata.hasRevisionTimestamp = true
	}
	for _, identities := range [][]model.LyricsSourceFixedIdentity{
		input.Fixed.FixedIdentities,
		func() []model.LyricsSourceFixedIdentity {
			if input.Fixed.Document == nil {
				return nil
			}
			return input.Fixed.Document.FixedIdentities
		}(),
	} {
		for _, identity := range identities {
			if identity.RenditionKey == input.SourceKey {
				return fixedIdentityMetadata(identity, metadata)
			}
		}
	}
	return metadata
}

func fixedReferenceMetadata(
	reference model.LyricsSourceComponentRef,
	identities map[string]model.LyricsSourceFixedIdentity,
	fallback fixedComponentMetadata,
) fixedComponentMetadata {
	identity, found := identities[reference.RenditionKey]
	if !found {
		return fallback
	}
	return fixedIdentityMetadata(identity, fallback)
}

func fixedIdentityMetadata(
	identity model.LyricsSourceFixedIdentity,
	fallback fixedComponentMetadata,
) fixedComponentMetadata {
	result := fallback
	if identity.Provider != "" {
		result.provider = identity.Provider
	}
	if parsed, ok := parseFixedRevisionTimestamp(identity.RevisionTimestamp); ok {
		result.revisionTimestamp = parsed
		result.hasRevisionTimestamp = true
	}
	return result
}

func parseFixedRevisionTimestamp(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || value == "" || !strings.HasSuffix(value, "Z") || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func fixedArtifactRevisionTimestamp(input FixedArtifactInput) string {
	if !input.Fixed.RevisionTimestamp.IsZero() {
		return input.Fixed.RevisionTimestamp.UTC().Format(time.RFC3339Nano)
	}
	for _, identities := range [][]model.LyricsSourceFixedIdentity{
		input.Fixed.FixedIdentities,
		func() []model.LyricsSourceFixedIdentity {
			if input.Fixed.Document == nil {
				return nil
			}
			return input.Fixed.Document.FixedIdentities
		}(),
	} {
		for _, identity := range identities {
			if identity.RenditionKey == input.SourceKey {
				if _, ok := parseFixedRevisionTimestamp(identity.RevisionTimestamp); ok {
					return identity.RevisionTimestamp
				}
				return ""
			}
		}
	}
	return ""
}

func fixedVersionResolutionEqual(left, right VersionResolution) bool {
	return left.ReasonCode == right.ReasonCode && lyricscontract.StringsEqual(left.Full, right.Full) &&
		lyricscontract.StringsEqual(left.Game, right.Game) && intsEqual(left.GameToFull, right.GameToFull)
}

func fixedVersionResolutionEnriches(base, enriched VersionResolution) bool {
	if base.Game != nil || base.GameToFull != nil || !lyricscontract.StringsEqual(base.Full, enriched.Full) {
		return false
	}
	if len(enriched.Game) > 0 && len(enriched.GameToFull) > 0 {
		return true
	}
	return base.ReasonCode == model.LyricsSourceVersionReasonUntaggedFullOnly &&
		enriched.ReasonCode == model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid &&
		enriched.Game == nil && enriched.GameToFull == nil
}

func cloneVersionResolution(input VersionResolution) VersionResolution {
	result := VersionResolution{ReasonCode: input.ReasonCode}
	if input.Full != nil {
		result.Full = cloneStrings(input.Full)
	}
	if input.Game != nil {
		result.Game = cloneStrings(input.Game)
	}
	if input.GameToFull != nil {
		result.GameToFull = append([]int{}, input.GameToFull...)
	}
	return result
}

func intsEqual(left, right []int) bool {
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

func fixedVersionPlanAligns(plan fixedVersionPlan, fullOwner fixedArtifactView) bool {
	return plan.fullOwner.input.LogicalRenditionKey == fullOwner.input.LogicalRenditionKey &&
		fullOwner.source != nil && equalVisibleTexts(plan.resolution.Full, fullOwner.source.VisibleJapanese)
}

func fixedVersionPlanIsStale(plan fixedVersionPlan, fullOwner fixedArtifactView) bool {
	if fixedComponentIsStale(plan.versionOwner, fixedComponentVersion, fullOwner) {
		return true
	}
	return plan.resolution.GameToFull != nil && fixedComponentIsStale(plan.gameOwner, fixedComponentGame, fullOwner)
}

func fixedAuthorityTier(provider model.LyricsSourceProvider) int {
	if provider == model.LyricsSourceProviderSekaipedia {
		return 2
	}
	return 1
}

func fixedViewMetadata(view fixedArtifactView, component fixedComponentKind) fixedComponentMetadata {
	switch component {
	case fixedComponentFull:
		return view.fullMetadata
	case fixedComponentSegmentation:
		return view.segmentationMetadata
	case fixedComponentGame:
		return view.gameMetadata
	case fixedComponentRuby:
		return view.rubyMetadata
	case fixedComponentVersion:
		return view.versionMetadata
	default:
		return fixedComponentMetadata{}
	}
}

func preferredFixedView(left, right fixedArtifactView, component fixedComponentKind) fixedArtifactView {
	leftMetadata := fixedViewMetadata(left, component)
	rightMetadata := fixedViewMetadata(right, component)
	if leftMetadata.hasRevisionTimestamp && rightMetadata.hasRevisionTimestamp &&
		!leftMetadata.revisionTimestamp.Equal(rightMetadata.revisionTimestamp) {
		if rightMetadata.revisionTimestamp.After(leftMetadata.revisionTimestamp) {
			return right
		}
		return left
	}
	if right.input.SourceKey < left.input.SourceKey {
		return right
	}
	return left
}

func fixedComponentIsStale(candidate fixedArtifactView, component fixedComponentKind, fullOwner fixedArtifactView) bool {
	metadata := fixedViewMetadata(candidate, component)
	fullMetadata := fixedViewMetadata(fullOwner, fixedComponentFull)
	if metadata.provider != model.LyricsSourceProviderSekaipedia ||
		!metadata.hasRevisionTimestamp || !fullMetadata.hasRevisionTimestamp {
		return false
	}
	return metadata.revisionTimestamp.Before(fullMetadata.revisionTimestamp)
}

func fixedViewAligns(candidate, fullOwner fixedArtifactView) bool {
	return candidate.source != nil && fullOwner.source != nil &&
		candidate.input.LogicalRenditionKey == fullOwner.input.LogicalRenditionKey &&
		visibleSequenceEqual(candidate.source.VisibleJapanese, fullOwner.source.VisibleJapanese)
}

func selectFixedRenditionStructure(
	views []fixedArtifactView,
	fullOwner fixedArtifactView,
) (model.LyricsSourceVersion, fixedArtifactView, error) {
	candidates := make([]fixedArtifactView, 0, len(views))
	for _, view := range views {
		if fixedViewAligns(view, fullOwner) && !fixedComponentIsStale(view, fixedComponentFull, fullOwner) {
			candidates = append(candidates, view)
		}
	}
	selected, found, err := selectFixedComponentCandidate(candidates, fixedComponentFull, func(left, right fixedArtifactView) bool {
		return left.full.Version == right.full.Version
	})
	if err != nil {
		return model.LyricsSourceVersion{}, fixedArtifactView{}, err
	}
	if !found {
		return fullOwner.full.Version, fullOwner, nil
	}
	return selected.full.Version, selected, nil
}

func selectFixedSegmentation(
	views []fixedArtifactView,
	fullOwner fixedArtifactView,
	versionKind string,
) (fixedArtifactView, bool, error) {
	candidates := make([]fixedArtifactView, 0, len(views))
	for _, view := range views {
		if view.source == nil || view.source.Segmentation == nil || !fixedViewAligns(view, fullOwner) ||
			fixedComponentIsStale(view, fixedComponentSegmentation, fullOwner) {
			continue
		}
		if versionKind == "vocaloid" && view.privateReview == nil {
			continue
		}
		candidates = append(candidates, view)
	}
	return selectFixedComponentCandidate(candidates, fixedComponentSegmentation, func(left, right fixedArtifactView) bool {
		return segmentationEqual(*left.source.Segmentation, *right.source.Segmentation)
	})
}

func selectFixedRuby(views []fixedArtifactView, fullOwner fixedArtifactView) (fixedArtifactView, bool, error) {
	candidates := make([]fixedArtifactView, 0, len(views))
	for _, view := range views {
		if view.source == nil || view.source.Ruby == nil || !fixedViewAligns(view, fullOwner) ||
			fixedComponentIsStale(view, fixedComponentRuby, fullOwner) {
			continue
		}
		candidates = append(candidates, view)
	}
	return selectFixedComponentCandidate(candidates, fixedComponentRuby, func(left, right fixedArtifactView) bool {
		return rubyEqual(*left.source.Ruby, *right.source.Ruby)
	})
}

func selectFixedComponentCandidate(
	candidates []fixedArtifactView,
	component fixedComponentKind,
	equal func(fixedArtifactView, fixedArtifactView) bool,
) (fixedArtifactView, bool, error) {
	if len(candidates) == 0 {
		return fixedArtifactView{}, false, nil
	}
	highestTier := 0
	for _, candidate := range candidates {
		tier := fixedAuthorityTier(fixedViewMetadata(candidate, component).provider)
		if tier > highestTier {
			highestTier = tier
		}
	}
	var selected fixedArtifactView
	found := false
	for _, candidate := range candidates {
		if fixedAuthorityTier(fixedViewMetadata(candidate, component).provider) != highestTier {
			continue
		}
		if !found {
			selected, found = candidate, true
			continue
		}
		if !equal(selected, candidate) {
			return fixedArtifactView{}, false, ErrComponentConflict
		}
		selected = preferredFixedView(selected, candidate, component)
	}
	return selected, found, nil
}

func selectedFixedSupplements(
	base Source,
	segmentation fixedArtifactView,
	hasSegmentation bool,
	ruby fixedArtifactView,
	hasRuby bool,
) []Source {
	selected := make(map[string]Source, 2)
	if hasSegmentation {
		source := *segmentation.source
		source.Ruby = nil
		selected[source.SourceKey] = source
	}
	if hasRuby {
		source, found := selected[ruby.source.SourceKey]
		if !found {
			source = *ruby.source
			source.Segmentation = nil
		}
		source.Ruby = ruby.source.Ruby
		selected[source.SourceKey] = source
	}
	result := make([]Source, 0, len(selected))
	for _, source := range selected {
		source.Identity.CatalogSongKey = base.Identity.CatalogSongKey
		source.Identity.RenditionKey = base.Identity.RenditionKey
		result = append(result, source)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].SourceKey < result[right].SourceKey })
	return result
}

func clonePrivateReview(input *model.LyricsSourcePrivateReview) *model.LyricsSourcePrivateReview {
	if input == nil {
		return nil
	}
	copy := *input
	return &copy
}

func canonicalizeCompositionSource(source Source) (Source, error) {
	if source.Segmentation == nil {
		return source, nil
	}
	segmentation, known, err := canonicalizeCompositionSegmentation(*source.Segmentation)
	if err != nil {
		return Source{}, lyricscontract.ErrUnsafePerformerMetadata
	}
	if !known {
		source.Segmentation = nil
		return source, nil
	}
	source.Segmentation = &segmentation
	return source, nil
}

func canonicalizeCompositionSegmentation(segmentation Segmentation) (Segmentation, bool, error) {
	if len(segmentation.Performers) == 0 {
		return cloneSegmentation(segmentation), true, nil
	}
	result := Segmentation{
		Performers: make([]Performer, len(segmentation.Performers)),
		Lines:      make([]SegmentedLine, len(segmentation.Lines)),
	}
	remapped := make(map[string]string, len(segmentation.Performers))
	seenPersisted := make(map[string]struct{}, len(segmentation.Performers))
	for index, performer := range segmentation.Performers {
		persisted, known, err := lyricscontract.NormalizeAuditedPerformerValues(performer.ID, performer.Name)
		if err != nil {
			return Segmentation{}, false, lyricscontract.ErrUnsafePerformerMetadata
		}
		if !known {
			return Segmentation{}, false, nil
		}
		if _, duplicate := seenPersisted[persisted.ID]; duplicate {
			return Segmentation{}, false, lyricscontract.ErrUnsafePerformerMetadata
		}
		remapped[performer.ID] = persisted.ID
		seenPersisted[persisted.ID] = struct{}{}
		result.Performers[index] = Performer{ID: persisted.ID, Name: persisted.Name, Color: performer.Color}
	}
	for lineIndex, line := range segmentation.Lines {
		result.Lines[lineIndex] = SegmentedLine{
			Segments: make([]Segment, len(line.Segments)),
		}
		for segmentIndex, segment := range line.Segments {
			ids, found := lyricscontract.RemapPersistedPerformerIDs(segment.PerformerIDs, remapped)
			if !found {
				return Segmentation{}, false, lyricscontract.ErrUnsafePerformerMetadata
			}
			result.Lines[lineIndex].Segments[segmentIndex] = Segment{Text: segment.Text, PerformerIDs: ids}
		}
		ids, found := lyricscontract.RemapPersistedPerformerIDs(line.TrailingPerformerIDs, remapped)
		if !found {
			return Segmentation{}, false, lyricscontract.ErrUnsafePerformerMetadata
		}
		result.Lines[lineIndex].TrailingPerformerIDs = ids
	}
	return result, true, nil
}

func persistedFullHasPerformerSegmentation(full model.LyricsSourceFull) bool {
	if len(full.Performers) != 0 {
		return true
	}
	for _, line := range full.Lines {
		if len(line.TrailingPerformerIDs) != 0 || len(line.Segments) != 1 ||
			len(line.Segments) == 1 && line.Segments[0].Text != line.Text {
			return true
		}
		for _, segment := range line.Segments {
			if len(segment.PerformerIDs) != 0 {
				return true
			}
		}
	}
	return false
}

func sourceFromModelFull(
	sourceKey, logicalRenditionKey string,
	full model.LyricsSourceFull,
	authoritativeVocaloidSegmentation bool,
) Source {
	visible := make([]VisibleLine, len(full.Lines))
	for index, line := range full.Lines {
		visible[index] = VisibleLine{Text: line.Text, StanzaBreakBefore: line.StanzaBreakBefore}
	}
	source := Source{
		SourceKey: sourceKey,
		Identity: Identity{
			CatalogSongKey: "fixed-artifact-composition", RenditionKey: logicalRenditionKey,
			FixedIdentityKey: sourceKey,
		},
		SequenceKind: SequenceFull, VisibleJapanese: visible,
	}
	if fullHasSegmentation(full) && (full.Version.Kind != "vocaloid" || authoritativeVocaloidSegmentation) {
		segmentation := Segmentation{Performers: make([]Performer, len(full.Performers)), Lines: make([]SegmentedLine, len(full.Lines))}
		for index, performer := range full.Performers {
			segmentation.Performers[index] = Performer{ID: performer.PerformerID, Name: performer.Name, Color: performer.Color}
		}
		for lineIndex, line := range full.Lines {
			segmentation.Lines[lineIndex] = SegmentedLine{
				Segments:             make([]Segment, len(line.Segments)),
				TrailingPerformerIDs: append([]string{}, line.TrailingPerformerIDs...),
			}
			for segmentIndex, segment := range line.Segments {
				segmentation.Lines[lineIndex].Segments[segmentIndex] = Segment{
					Text: segment.Text, PerformerIDs: append([]string{}, segment.PerformerIDs...),
				}
			}
		}
		source.Segmentation = &segmentation
	}
	if fullHasRuby(full) {
		ruby := Ruby{GeneratorVersion: full.RubyGeneratorVersion, Lines: make([][]RubySpan, len(full.Lines))}
		for lineIndex, line := range full.Lines {
			for _, segment := range line.Segments {
				for _, span := range segment.Ruby {
					ruby.Lines[lineIndex] = append(ruby.Lines[lineIndex], RubySpan{Text: span.Text, Reading: span.Reading})
				}
			}
		}
		source.Ruby = &ruby
	}
	return source
}

func composedResultFull(
	result Result,
	authoritative model.LyricsSourceFull,
	version model.LyricsSourceVersion,
) model.LyricsSourceFull {
	performers := make([]model.LyricsSourcePerformer, len(result.Performers))
	for index, performer := range result.Performers {
		performers[index] = model.LyricsSourcePerformer{PerformerID: performer.ID, Name: performer.Name, Color: performer.Color}
	}
	lines := make([]model.LyricsSourceExtractedLine, len(result.Lines))
	for lineIndex, line := range result.Lines {
		segments := make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			ruby := make([]model.LyricsSourceRubySpan, len(segment.Ruby))
			for spanIndex, span := range segment.Ruby {
				ruby[spanIndex] = model.LyricsSourceRubySpan{Text: span.Text, Reading: span.Reading}
			}
			segments[segmentIndex] = model.LyricsSourceSegment{
				Text: segment.Text, PerformerIDs: append([]string{}, segment.PerformerIDs...), Ruby: ruby,
			}
		}
		lines[lineIndex] = model.LyricsSourceExtractedLine{
			Japanese: line.Text, StanzaBreakBefore: line.StanzaBreakBefore, Segments: segments,
			TrailingPerformerIDs: append([]string{}, line.TrailingPerformerIDs...),
		}
	}
	full := model.NewLyricsSourceFullFromLegacy(version, performers, result.RubyGeneratorVersion, lines)
	if len(authoritative.Lines) == len(full.Lines) {
		for index := range full.Lines {
			full.Lines[index].ID = authoritative.Lines[index].ID
		}
	}
	return full
}

func modelFullFromExtraction(extraction lyricssource.Extraction) (model.LyricsSourceFull, error) {
	if len(extraction.Lines) == 0 {
		return model.LyricsSourceFull{}, ErrProjectionMissing
	}
	performers := make([]model.LyricsSourcePerformer, len(extraction.Performers))
	for index, performer := range extraction.Performers {
		performers[index] = model.LyricsSourcePerformer{PerformerID: performer.PerformerID, Name: performer.Name, Color: performer.Color}
	}
	lines := make([]model.LyricsSourceExtractedLine, len(extraction.Lines))
	for lineIndex, line := range extraction.Lines {
		segments := make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			ruby := make([]model.LyricsSourceRubySpan, len(segment.Ruby))
			for rubyIndex, span := range segment.Ruby {
				ruby[rubyIndex] = model.LyricsSourceRubySpan{Text: span.Text, Reading: span.Reading}
			}
			segments[segmentIndex] = model.LyricsSourceSegment{Text: segment.Text, PerformerIDs: append([]string{}, segment.PerformerIDs...), Ruby: ruby}
		}
		lines[lineIndex] = model.LyricsSourceExtractedLine{
			Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore, Segments: segments,
			TrailingPerformerIDs: append([]string{}, line.TrailingPerformerIDs...),
		}
	}
	full := model.NewLyricsSourceFullFromLegacy(
		model.LyricsSourceVersion{Kind: extraction.Version.Kind, Label: extraction.Version.Label},
		performers, extraction.RubyGeneratorVersion, lines,
	)
	return full, nil
}

func legacyExtractionFromFull(full model.LyricsSourceFull) ([]lyricssource.ExtractedLine, lyricssource.Extraction) {
	lines := make([]lyricssource.ExtractedLine, len(full.Lines))
	extraction := lyricssource.Extraction{
		Version:              lyricssource.LyricsVersion{Kind: full.Version.Kind, Label: full.Version.Label},
		Performers:           make([]lyricssource.Performer, len(full.Performers)),
		RubyGeneratorVersion: full.RubyGeneratorVersion,
		Lines:                make([]lyricssource.StructuredLine, len(full.Lines)),
	}
	for index, performer := range full.Performers {
		extraction.Performers[index] = lyricssource.Performer{PerformerID: performer.PerformerID, Name: performer.Name, Color: performer.Color}
	}
	for lineIndex, line := range full.Lines {
		lines[lineIndex] = lyricssource.ExtractedLine{Japanese: line.Text, StanzaBreakBefore: line.StanzaBreakBefore}
		structured := lyricssource.StructuredLine{
			Japanese: line.Text, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments:             make([]lyricssource.LyricsSegment, len(line.Segments)),
			TrailingPerformerIDs: append([]string{}, line.TrailingPerformerIDs...),
		}
		for segmentIndex, segment := range line.Segments {
			ruby := make([]lyricssource.RubySpan, len(segment.Ruby))
			for spanIndex, span := range segment.Ruby {
				ruby[spanIndex] = lyricssource.RubySpan{Text: span.Text, Reading: span.Reading}
			}
			structured.Segments[segmentIndex] = lyricssource.LyricsSegment{
				Text: segment.Text, PerformerIDs: append([]string{}, segment.PerformerIDs...), Ruby: ruby,
			}
		}
		extraction.Lines[lineIndex] = structured
	}
	return lines, extraction
}

func projectedVisibleLines(full model.LyricsSourceFull, projection model.LyricsSourceGameProjection) ([]VisibleLine, error) {
	byID := make(map[string]model.LyricsSourceFullLine, len(full.Lines))
	for _, line := range full.Lines {
		byID[line.ID] = line
	}
	result := make([]VisibleLine, len(projection.LineIDs))
	for index, lineID := range projection.LineIDs {
		line, found := byID[lineID]
		if !found {
			return nil, ErrProjectionMissing
		}
		result[index] = VisibleLine{Text: line.Text, StanzaBreakBefore: line.StanzaBreakBefore}
	}
	return result, nil
}

func visibleLinesFromExtraction(extraction lyricssource.Extraction) []VisibleLine {
	result := make([]VisibleLine, len(extraction.Lines))
	for index, line := range extraction.Lines {
		result[index] = VisibleLine{Text: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore}
	}
	return result
}

func visibleLinesFromModelFull(full model.LyricsSourceFull) []VisibleLine {
	result := make([]VisibleLine, len(full.Lines))
	for index, line := range full.Lines {
		result[index] = VisibleLine{Text: line.Text, StanzaBreakBefore: line.StanzaBreakBefore}
	}
	return result
}

func visibleTexts(lines []VisibleLine) []string {
	result := make([]string, len(lines))
	for index, line := range lines {
		result[index] = line.Text
	}
	return result
}

func visibleTextsFromFull(full model.LyricsSourceFull) []string {
	result := make([]string, len(full.Lines))
	for index, line := range full.Lines {
		result[index] = line.Text
	}
	return result
}

func equalVisibleTexts(expected []string, actual []VisibleLine) bool {
	if len(expected) != len(actual) {
		return false
	}
	for index := range expected {
		if expected[index] != actual[index].Text {
			return false
		}
	}
	return true
}

func equalComposedTexts(expected []string, actual []ComposedLine) bool {
	if len(expected) != len(actual) {
		return false
	}
	for index := range expected {
		if expected[index] != actual[index].Text {
			return false
		}
	}
	return true
}

func fullHasSegmentation(full model.LyricsSourceFull) bool {
	if len(full.Performers) > 0 {
		return true
	}
	for _, line := range full.Lines {
		if len(line.TrailingPerformerIDs) > 0 {
			return true
		}
		for _, segment := range line.Segments {
			if len(segment.PerformerIDs) > 0 {
				return true
			}
		}
	}
	return false
}

func fullHasRuby(full model.LyricsSourceFull) bool {
	for _, line := range full.Lines {
		for _, segment := range line.Segments {
			for _, span := range segment.Ruby {
				if span.Reading != "" {
					return true
				}
			}
		}
	}
	return false
}

func selectedComponentSourceKeys(components FixedArtifactComponents) []string {
	seen := map[string]struct{}{}
	for _, key := range []string{
		components.FullText, components.GameText, components.AlternateVocals,
		components.PerformerSegmentation, components.GameProjection, components.Ruby, components.VersionEvidence,
	} {
		if key != "" {
			seen[key] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for key := range seen {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func cloneModelFull(full *model.LyricsSourceFull) *model.LyricsSourceFull {
	if full == nil {
		return nil
	}
	copy := *full
	copy.Performers = append([]model.LyricsSourcePerformer{}, full.Performers...)
	copy.Lines = make([]model.LyricsSourceFullLine, len(full.Lines))
	for index, line := range full.Lines {
		copy.Lines[index] = line
		copy.Lines[index].Segments = make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			copy.Lines[index].Segments[segmentIndex] = segment
			copy.Lines[index].Segments[segmentIndex].PerformerIDs = append([]string{}, segment.PerformerIDs...)
			copy.Lines[index].Segments[segmentIndex].Ruby = append([]model.LyricsSourceRubySpan{}, segment.Ruby...)
			for spanIndex, span := range segment.Ruby {
				if span.ReadingEvidence != nil {
					evidence := *span.ReadingEvidence
					copy.Lines[index].Segments[segmentIndex].Ruby[spanIndex].ReadingEvidence = &evidence
				}
			}
		}
		copy.Lines[index].TrailingPerformerIDs = append([]string{}, line.TrailingPerformerIDs...)
	}
	return &copy
}

func cloneModelGameProjection(projection *model.LyricsSourceGameProjection) *model.LyricsSourceGameProjection {
	if projection == nil {
		return nil
	}
	return &model.LyricsSourceGameProjection{LineIDs: append([]string{}, projection.LineIDs...)}
}
