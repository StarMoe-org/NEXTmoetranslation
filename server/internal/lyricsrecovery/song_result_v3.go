package lyricsrecovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"moesekai/server/internal/lyricscompose"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

const maxSongResultRenditions = 16

type SongResultPeerTranslation struct {
	Side         string   `json:"side"`
	Locale       string   `json:"locale"`
	Translations []string `json:"translations"`
}

type SongResultRendition struct {
	RenditionKey          string                                   `json:"renditionKey"`
	SourceKind            model.LyricsSourceRenditionKind          `json:"sourceKind"`
	SourceTabPaths        []model.LyricsSourceTabPath              `json:"sourceTabPaths"`
	ReasonCode            model.LyricsSourceVersionReasonCode      `json:"reasonCode"`
	SourcePerformerIDs    []string                                 `json:"sourcePerformerIds,omitempty"`
	FullPerformerEvidence model.LyricsSourcePerformerEvidenceState `json:"fullPerformerEvidence"`
	GamePerformerEvidence model.LyricsSourcePerformerEvidenceState `json:"gamePerformerEvidence"`
	Full                  *model.LyricsSourceFull                  `json:"full,omitempty"`
	Game                  *model.LyricsSourceFull                  `json:"game,omitempty"`
	Relation              model.LyricsSourceRenditionRelation      `json:"relation"`
	PrivateReview         *model.LyricsSourcePrivateReview         `json:"privateReview,omitempty"`
	Components            []RenditionComponentEvidenceRef          `json:"components"`
	Translations          []string                                 `json:"translations,omitempty"`
	PeerTranslations      []SongResultPeerTranslation              `json:"peerTranslations,omitempty"`
}

func (result SongResult) MarshalJSON() ([]byte, error) {
	if result.SchemaVersion != SongResultSchemaVersionV3 {
		type legacy SongResult
		return json.Marshal(legacy(result))
	}
	type wire struct {
		SchemaVersion     int                                     `json:"schemaVersion"`
		CanonicalEncoding string                                  `json:"canonicalEncoding"`
		DigestAlgorithm   string                                  `json:"digestAlgorithm"`
		MusicID           int                                     `json:"musicId"`
		State             lyricscontract.CoverageState            `json:"state"`
		NoLyricsReason    string                                  `json:"noLyricsReason,omitempty"`
		ProviderOutcomes  []lyricsrootmanifest.ProviderOutcomeRef `json:"providerOutcomes"`
		SelectedEvidence  []lyricscontract.EvidenceRef            `json:"selectedEvidence"`
		Renditions        []SongResultRendition                   `json:"renditions"`
		ResultSHA256      string                                  `json:"resultSha256"`
	}
	return json.Marshal(wire{
		SchemaVersion: result.SchemaVersion, CanonicalEncoding: result.CanonicalEncoding,
		DigestAlgorithm: result.DigestAlgorithm, MusicID: result.MusicID, State: result.State,
		NoLyricsReason: result.NoLyricsReason, ProviderOutcomes: result.ProviderOutcomes,
		SelectedEvidence: result.SelectedEvidence, Renditions: result.Renditions,
		ResultSHA256: result.ResultSHA256,
	})
}

func newSongResultV3(replay ReplayResult) (SongResult, error) {
	if replay.Instrumental || replay.Composition == nil || len(replay.Composition.Renditions) == 0 {
		return SongResult{}, errors.New("lyrics recovery v3 requires a vocal peer rendition composition")
	}
	composition := replay.Composition
	if composition.ReasonCode != "" || len(composition.Full.Lines) != 0 || composition.Game != nil ||
		composition.GameProjection != nil || len(composition.AlternateVocals) != 0 || composition.PrivateReview != nil ||
		composition.Components != (lyricscompose.FixedArtifactComponents{}) || !componentEvidenceZero(replay.Components) {
		return SongResult{}, errors.New("lyrics recovery v3 composition contains legacy singular fields")
	}
	result := SongResult{
		SchemaVersion: SongResultSchemaVersionV3, CanonicalEncoding: SongResultCanonicalEncodingV3,
		DigestAlgorithm: SongResultDigestAlgorithmV3, MusicID: replay.MusicID,
		ProviderOutcomes: make([]lyricsrootmanifest.ProviderOutcomeRef, len(replay.Providers)),
		SelectedEvidence: append([]lyricscontract.EvidenceRef(nil), replay.Selected...),
	}
	for index, provider := range replay.Providers {
		result.ProviderOutcomes[index] = lyricsrootmanifest.ProviderOutcomeRef{
			Provider: provider.Artifact.Provider, OutcomeID: provider.Artifact.OutcomeID,
			SHA256: provider.Artifact.ArtifactSHA256,
		}
	}
	sort.Slice(result.SelectedEvidence, func(left, right int) bool {
		return result.SelectedEvidence[left].EvidenceID < result.SelectedEvidence[right].EvidenceID
	})
	evidenceByKey := make(map[string]RenditionComponentEvidence, len(replay.RenditionComponents))
	for _, rendition := range replay.RenditionComponents {
		if _, duplicate := evidenceByKey[rendition.RenditionKey]; duplicate {
			return SongResult{}, errors.New("lyrics recovery v3 repeats rendition component evidence")
		}
		evidenceByKey[rendition.RenditionKey] = rendition
	}
	ordered := model.CloneLyricsSourceRenditions(composition.Renditions)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].RenditionKey < ordered[right].RenditionKey })
	result.Renditions = make([]SongResultRendition, len(ordered))
	hasFull := false
	for index, rendition := range ordered {
		evidence, found := evidenceByKey[rendition.RenditionKey]
		if !found {
			return SongResult{}, fmt.Errorf("lyrics recovery v3 rendition %q has no component evidence", rendition.RenditionKey)
		}
		full, err := normalizedSongResultV3Full(rendition.Full)
		if err != nil {
			return SongResult{}, err
		}
		game, err := normalizedSongResultV3Full(rendition.Game)
		if err != nil {
			return SongResult{}, err
		}
		hasFull = hasFull || full != nil
		translations, peerTranslations, err := songResultRenditionTranslations(rendition, replay.Providers)
		if err != nil {
			return SongResult{}, err
		}
		result.Renditions[index] = SongResultRendition{
			RenditionKey: rendition.RenditionKey, SourceKind: rendition.SourceKind,
			SourceTabPaths: cloneSongResultTabPaths(rendition.SourceTabPaths), ReasonCode: rendition.ReasonCode,
			SourcePerformerIDs:    append([]string(nil), rendition.SourcePerformerIDs...),
			FullPerformerEvidence: rendition.FullPerformerEvidence,
			GamePerformerEvidence: rendition.GamePerformerEvidence,
			Full:                  full, Game: game, Relation: cloneSongResultRelation(rendition.Relation),
			PrivateReview:    cloneSongResultPrivateReview(rendition.PrivateReview),
			Components:       cloneRenditionComponentEvidence([]RenditionComponentEvidence{evidence})[0].Components,
			Translations:     translations,
			PeerTranslations: peerTranslations,
		}
	}
	if len(evidenceByKey) != len(result.Renditions) {
		return SongResult{}, errors.New("lyrics recovery v3 component evidence contains an unknown rendition")
	}
	if hasFull {
		result.State = lyricscontract.CoverageComplete
	} else {
		result.State = lyricscontract.CoverageGameOnly
	}
	if err := validateSongResult(result, false); err != nil {
		return SongResult{}, err
	}
	digest, err := songResultDigest(result)
	if err != nil {
		return SongResult{}, err
	}
	result.ResultSHA256 = digest
	if err := ValidateSongResult(result); err != nil {
		return SongResult{}, err
	}
	return cloneSongResult(result), nil
}

func normalizedSongResultV3Full(input *model.LyricsSourceFull) (*model.LyricsSourceFull, error) {
	if input == nil {
		return nil, nil
	}
	hadSegmentation := songResultFullHasPerformerSegmentation(*input)
	full, err := lyricscontract.NormalizePersistedPerformerMetadata(*input)
	if err != nil || hadSegmentation != songResultFullHasPerformerSegmentation(full) {
		return nil, fmt.Errorf("lyrics recovery v3 exact performer registry: %w", lyricscontract.ErrUnsafePerformerMetadata)
	}
	persistedRubyVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(full.RubyGeneratorVersion)
	if err != nil {
		return nil, err
	}
	full.RubyGeneratorVersion = persistedRubyVersion
	return &full, nil
}

func songResultRenditionTranslations(
	rendition model.LyricsSourceRendition,
	providers []ProviderReplay,
) ([]string, []SongResultPeerTranslation, error) {
	sourceKey := ""
	authoritative := rendition.Full
	if authoritative != nil && rendition.Provenance.FullText != nil {
		sourceKey = rendition.Provenance.FullText.RenditionKey
	} else if rendition.Game != nil && rendition.Provenance.GameText != nil {
		authoritative = rendition.Game
		sourceKey = rendition.Provenance.GameText.RenditionKey
	}
	primarySide := "game"
	if rendition.Full != nil {
		primarySide = "full"
	}
	translations, err := songResultTranslationsForSide(
		sourceKey, primarySide, string(rendition.SourceKind), authoritative, providers, true,
	)
	if err != nil {
		return nil, nil, err
	}
	if rendition.Full == nil || rendition.Game == nil ||
		rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
		return translations, nil, nil
	}
	if rendition.Provenance.GameText == nil {
		return nil, nil, errors.New("lyrics recovery v3 independent Game translation source is incomplete")
	}
	peer, err := songResultTranslationsForSide(
		rendition.Provenance.GameText.RenditionKey, "game", string(rendition.SourceKind), rendition.Game, providers, false,
	)
	if err != nil {
		return nil, nil, err
	}
	if peer == nil {
		return translations, nil, nil
	}
	return translations, []SongResultPeerTranslation{{
		Side: "game", Locale: "zh-CN", Translations: peer,
	}}, nil
}

func songResultTranslationsForSide(
	sourceKey string,
	side string,
	renditionKind string,
	authoritative *model.LyricsSourceFull,
	providers []ProviderReplay,
	required bool,
) ([]string, error) {
	if sourceKey == "" || authoritative == nil || len(authoritative.Lines) == 0 {
		return nil, errors.New("lyrics recovery v3 translation source is incomplete")
	}
	for _, provider := range providers {
		if provider.Artifact.OutcomeID != sourceKey {
			continue
		}
		if provider.Fixed == nil || len(provider.Fixed.Translations) == 0 {
			return nil, nil
		}
		if provider.Fixed.RenditionKey != side+"-"+renditionKind {
			if required {
				return nil, errors.New("lyrics recovery v3 translation owner differs from the authoritative side")
			}
			continue
		}
		if len(provider.Fixed.Translations) != len(authoritative.Lines) ||
			len(provider.Fixed.Extraction.Lines) != len(authoritative.Lines) {
			return nil, errors.New("lyrics recovery v3 translations do not align with the rendition")
		}
		for index, line := range authoritative.Lines {
			if provider.Fixed.Extraction.Lines[index].Japanese != line.Text {
				return nil, errors.New("lyrics recovery v3 translation source text drifted")
			}
		}
		translations := append([]string(nil), provider.Fixed.Translations...)
		if err := validateSongResultTranslations(translations, len(authoritative.Lines)); err != nil {
			return nil, err
		}
		return translations, nil
	}
	if required {
		return nil, errors.New("lyrics recovery v3 translation source is outside the provider prefix")
	}
	return nil, nil
}

func validateSongResultV3(result SongResult, requireDigest bool) error {
	if result.CanonicalEncoding != SongResultCanonicalEncodingV3 ||
		result.DigestAlgorithm != SongResultDigestAlgorithmV3 || result.MusicID <= 0 ||
		result.ProviderOutcomes == nil || result.SelectedEvidence == nil || result.Renditions == nil ||
		len(result.Renditions) == 0 || len(result.Renditions) > maxSongResultRenditions ||
		len(result.ProviderOutcomes) > lyricsrootmanifest.MaxProviderOutcomes ||
		len(result.SelectedEvidence) > lyricsrootmanifest.MaxSelectionsPerSong {
		return errors.New("lyrics recovery song result v3 identity is invalid")
	}
	if result.ReasonCode != "" || result.NoLyricsReason != "" || result.Full != nil || result.Game != nil ||
		result.GameProjection != nil || result.AlternateVocals != nil || result.Translations != nil ||
		!componentEvidenceZero(result.Components) {
		return errors.New("lyrics recovery song result v3 contains legacy singular fields")
	}
	if requireDigest {
		if !canonicalSongResultSHA256.MatchString(result.ResultSHA256) {
			return errors.New("lyrics recovery song result v3 digest is invalid")
		}
	} else if result.ResultSHA256 != "" {
		return errors.New("new lyrics recovery song result v3 contains a premature digest")
	}
	if err := validateSongOutcomeRefs(result.ProviderOutcomes); err != nil {
		return err
	}
	if err := validateSongEvidenceRefs(result.SelectedEvidence); err != nil {
		return err
	}
	outcomes := make(map[string]lyricsrootmanifest.ProviderOutcomeRef, len(result.ProviderOutcomes))
	for _, outcome := range result.ProviderOutcomes {
		outcomes[outcome.OutcomeID] = outcome
	}
	available := make(map[lyricscontract.EvidenceRef]struct{}, len(result.SelectedEvidence))
	for _, evidence := range result.SelectedEvidence {
		available[evidence] = struct{}{}
	}
	used := make(map[lyricscontract.EvidenceRef]struct{}, len(result.SelectedEvidence))
	payloads := make([]model.LyricsSourceRendition, len(result.Renditions))
	lastKey := ""
	hasFull := false
	for index, rendition := range result.Renditions {
		if lastKey != "" && lastKey >= rendition.RenditionKey {
			return errors.New("lyrics recovery song result v3 renditions are not canonically ordered")
		}
		lastKey = rendition.RenditionKey
		payload, err := modelRenditionFromSongResult(rendition)
		if err != nil {
			return err
		}
		payloads[index] = payload
		if rendition.Full != nil {
			hasFull = true
			if err := lyricscontract.ValidatePersistedPerformerMetadata(*rendition.Full); err != nil {
				return fmt.Errorf("lyrics recovery song result v3: %w", lyricscontract.ErrUnsafePerformerMetadata)
			}
		}
		if rendition.Game != nil {
			if err := lyricscontract.ValidatePersistedPerformerMetadata(*rendition.Game); err != nil {
				return fmt.Errorf("lyrics recovery song result v3 Game: %w", lyricscontract.ErrUnsafePerformerMetadata)
			}
		}
		lineCount := 0
		if rendition.Full != nil {
			lineCount = len(rendition.Full.Lines)
		} else if rendition.Game != nil {
			lineCount = len(rendition.Game.Lines)
		}
		if err := validateSongResultTranslations(rendition.Translations, lineCount); err != nil {
			return err
		}
		if err := validateSongResultPeerTranslations(rendition); err != nil {
			return err
		}
		for _, component := range rendition.Components {
			outcome, found := outcomes[component.OutcomeID]
			if !found || component.Evidence == nil || len(component.Evidence) == 0 {
				return errors.New("lyrics recovery song result v3 component has no exact provider artifact")
			}
			if err := validateSongEvidenceRefs(component.Evidence); err != nil {
				return err
			}
			for _, evidence := range component.Evidence {
				if evidence.Provider != outcome.Provider {
					return errors.New("lyrics recovery song result v3 component evidence provider differs from its outcome")
				}
				if _, found := available[evidence]; !found {
					return errors.New("lyrics recovery song result v3 component evidence is outside the selected union")
				}
				used[evidence] = struct{}{}
			}
		}
	}
	if err := model.ValidateLyricsSourceRenditionSetPayload(payloads); err != nil {
		return err
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(payloads)
	if err != nil {
		return err
	}
	actual := make([]RenditionComponentEvidenceRef, 0, len(bindings))
	for _, rendition := range result.Renditions {
		actual = append(actual, rendition.Components...)
	}
	if len(actual) != len(bindings) {
		return errors.New("lyrics recovery song result v3 component enumeration is incomplete")
	}
	for index, binding := range bindings {
		if actual[index].Component != binding.Component || actual[index].OutcomeID != binding.FixedIdentityKey {
			return errors.New("lyrics recovery song result v3 components are not canonical")
		}
	}
	if len(used) != len(available) {
		return errors.New("lyrics recovery song result v3 selected evidence contains an unreferenced artifact")
	}
	switch result.State {
	case lyricscontract.CoverageComplete:
		if !hasFull || len(result.ProviderOutcomes) == 0 || len(result.SelectedEvidence) == 0 {
			return errors.New("complete lyrics recovery song result v3 is incomplete")
		}
	case lyricscontract.CoverageGameOnly:
		if hasFull || len(result.ProviderOutcomes) == 0 || len(result.SelectedEvidence) == 0 {
			return errors.New("Game-only lyrics recovery song result v3 is incomplete")
		}
	default:
		return errors.New("lyrics recovery song result v3 has an unsupported state")
	}
	return nil
}

func validateSongResultPeerTranslations(rendition SongResultRendition) error {
	if rendition.PeerTranslations == nil {
		return nil
	}
	if len(rendition.PeerTranslations) == 0 || rendition.Full == nil || rendition.Game == nil ||
		rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
		return errors.New("lyrics recovery song result v3 peer translations have no independently persisted side")
	}
	lastKey := ""
	for _, peer := range rendition.PeerTranslations {
		key := peer.Side + "\x00" + peer.Locale
		if peer.Side != "game" || peer.Locale != "zh-CN" || key <= lastKey {
			return errors.New("lyrics recovery song result v3 peer translations are not canonical")
		}
		lastKey = key
		if err := validateSongResultTranslations(peer.Translations, len(rendition.Game.Lines)); err != nil {
			return err
		}
	}
	return nil
}

func modelRenditionFromSongResult(result SongResultRendition) (model.LyricsSourceRendition, error) {
	provenance := model.LyricsSourceRenditionProvenance{}
	seen := make(map[model.LyricsSourceRenditionComponentKind]struct{}, len(result.Components))
	for _, component := range result.Components {
		if model.LyricsSourceRenditionComponentRank(component.Component) < 0 || component.OutcomeID == "" {
			return model.LyricsSourceRendition{}, errors.New("lyrics recovery song result v3 has an unknown component")
		}
		if _, duplicate := seen[component.Component]; duplicate {
			return model.LyricsSourceRendition{}, errors.New("lyrics recovery song result v3 repeats a component")
		}
		seen[component.Component] = struct{}{}
		ref := model.LyricsSourceComponentRef{RenditionKey: component.OutcomeID}
		switch component.Component {
		case model.LyricsSourceRenditionComponentFullText:
			provenance.FullText = &ref
		case model.LyricsSourceRenditionComponentFullPerformerSegmentation:
			provenance.FullPerformerSegmentation = &ref
		case model.LyricsSourceRenditionComponentFullRuby:
			provenance.FullRuby = &ref
		case model.LyricsSourceRenditionComponentGameText:
			provenance.GameText = &ref
		case model.LyricsSourceRenditionComponentGamePerformerSegmentation:
			provenance.GamePerformerSegmentation = &ref
		case model.LyricsSourceRenditionComponentGameRuby:
			provenance.GameRuby = &ref
		case model.LyricsSourceRenditionComponentRelation:
			provenance.RelationEvidence = ref
		case model.LyricsSourceRenditionComponentVersion:
			provenance.VersionEvidence = ref
		}
	}
	return model.LyricsSourceRendition{
		RenditionKey: result.RenditionKey, SourceKind: result.SourceKind,
		SourceTabPaths: cloneSongResultTabPaths(result.SourceTabPaths), ReasonCode: result.ReasonCode,
		SourcePerformerIDs:    append([]string(nil), result.SourcePerformerIDs...),
		FullPerformerEvidence: result.FullPerformerEvidence,
		GamePerformerEvidence: result.GamePerformerEvidence,
		Full:                  cloneSongResultFull(result.Full), Game: cloneSongResultFull(result.Game),
		Relation: cloneSongResultRelation(result.Relation), Provenance: provenance,
		PrivateReview: cloneSongResultPrivateReview(result.PrivateReview),
	}, nil
}

func UpconvertSongResultV2(result SongResult) (SongResult, error) {
	if result.SchemaVersion != SongResultSchemaVersionV2 {
		return SongResult{}, errors.New("lyrics recovery song result up-conversion requires v2")
	}
	if err := ValidateSongResult(result); err != nil {
		return SongResult{}, err
	}
	if len(result.AlternateVocals) != 0 {
		return SongResult{}, errors.New("lyrics recovery song result v2 with alternates is not one rendition")
	}
	if result.State != lyricscontract.CoverageComplete && result.State != lyricscontract.CoverageGameOnly {
		return SongResult{}, errors.New("lyrics recovery song result v2 has no lyric rendition to up-convert")
	}
	kind := ""
	if result.Full != nil {
		kind = result.Full.Version.Kind
	}
	if result.Game != nil {
		if kind != "" && result.Game.Version.Kind != kind {
			return SongResult{}, errors.New("lyrics recovery song result v2 Full/Game kinds do not form one rendition")
		}
		kind = result.Game.Version.Kind
	}
	if !model.IsValidLyricsSourceRenditionKind(model.LyricsSourceRenditionKind(kind)) {
		return SongResult{}, errors.New("lyrics recovery song result v2 has no closed rendition kind")
	}
	paths := make([]model.LyricsSourceTabPath, 0, 2)
	seenPaths := map[string]struct{}{}
	appendPath := func(label string) {
		if label == "" {
			return
		}
		if _, duplicate := seenPaths[label]; duplicate {
			return
		}
		seenPaths[label] = struct{}{}
		paths = append(paths, model.LyricsSourceTabPath{label})
	}
	if result.Full != nil {
		appendPath(result.Full.Version.Label)
	}
	if result.Game != nil {
		appendPath(result.Game.Version.Label)
	}
	relation := model.LyricsSourceRenditionRelation{Kind: model.LyricsSourceRenditionRelationNone}
	if result.GameProjection != nil {
		relation = model.LyricsSourceRenditionRelation{
			Kind: model.LyricsSourceRenditionRelationExactProjection, FullRenditionKey: kind,
			LineIDs: cloneStringsPreservingNil(result.GameProjection.LineIDs),
		}
	}
	legacyEvidence := legacySongResultV2RenditionEvidence(result)
	provenance := model.LyricsSourceRenditionProvenance{}
	for component, refs := range legacyEvidence {
		outcomeID, err := inferSongResultV2ComponentOutcome(result, refs)
		if err != nil {
			return SongResult{}, err
		}
		ref := model.LyricsSourceComponentRef{RenditionKey: outcomeID}
		switch component {
		case model.LyricsSourceRenditionComponentFullText:
			provenance.FullText = &ref
		case model.LyricsSourceRenditionComponentFullPerformerSegmentation:
			provenance.FullPerformerSegmentation = &ref
		case model.LyricsSourceRenditionComponentFullRuby:
			provenance.FullRuby = &ref
		case model.LyricsSourceRenditionComponentGameText:
			provenance.GameText = &ref
		case model.LyricsSourceRenditionComponentGamePerformerSegmentation:
			provenance.GamePerformerSegmentation = &ref
		case model.LyricsSourceRenditionComponentGameRuby:
			provenance.GameRuby = &ref
		case model.LyricsSourceRenditionComponentRelation:
			provenance.RelationEvidence = ref
		case model.LyricsSourceRenditionComponentVersion:
			provenance.VersionEvidence = ref
		}
	}
	full := cloneSongResultFull(result.Full)
	game := cloneSongResultFull(result.Game)
	populateSongResultLegacyReadingEvidence(
		full, kind, model.LyricsSourceRenditionSideFull, provenance.FullRuby,
	)
	populateSongResultLegacyReadingEvidence(
		game, kind, model.LyricsSourceRenditionSideGame, provenance.GameRuby,
	)
	fullEvidence := songResultLegacyPerformerEvidence(full)
	gameEvidence := songResultLegacyPerformerEvidence(game)
	payload := model.LyricsSourceRendition{
		RenditionKey: kind, SourceKind: model.LyricsSourceRenditionKind(kind), SourceTabPaths: paths,
		ReasonCode:            result.ReasonCode,
		FullPerformerEvidence: fullEvidence, GamePerformerEvidence: gameEvidence,
		Full: full, Game: game, Relation: relation, Provenance: provenance,
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents([]model.LyricsSourceRendition{payload})
	if err != nil {
		return SongResult{}, fmt.Errorf("up-convert lyrics recovery song result v2: %w", err)
	}
	components := make([]RenditionComponentEvidenceRef, len(bindings))
	for index, binding := range bindings {
		refs := legacyEvidence[binding.Component]
		components[index] = RenditionComponentEvidenceRef{
			Component: binding.Component, OutcomeID: binding.FixedIdentityKey,
			Evidence: cloneEvidenceRefs(refs),
		}
	}
	upgraded := SongResult{
		SchemaVersion: SongResultSchemaVersionV3, CanonicalEncoding: SongResultCanonicalEncodingV3,
		DigestAlgorithm: SongResultDigestAlgorithmV3, MusicID: result.MusicID, State: result.State,
		ProviderOutcomes: append([]lyricsrootmanifest.ProviderOutcomeRef{}, result.ProviderOutcomes...),
		SelectedEvidence: cloneEvidenceRefs(result.SelectedEvidence),
		Renditions: []SongResultRendition{{
			RenditionKey: kind, SourceKind: model.LyricsSourceRenditionKind(kind), SourceTabPaths: paths,
			ReasonCode:            result.ReasonCode,
			FullPerformerEvidence: fullEvidence, GamePerformerEvidence: gameEvidence,
			Full: cloneSongResultFull(full), Game: cloneSongResultFull(game),
			Relation: relation, Components: components, Translations: cloneStringsPreservingNil(result.Translations),
		}},
	}
	if err := validateSongResult(upgraded, false); err != nil {
		return SongResult{}, err
	}
	digest, err := songResultDigest(upgraded)
	if err != nil {
		return SongResult{}, err
	}
	upgraded.ResultSHA256 = digest
	if err := ValidateSongResult(upgraded); err != nil {
		return SongResult{}, err
	}
	return cloneSongResult(upgraded), nil
}

func songResultLegacyPerformerEvidence(
	full *model.LyricsSourceFull,
) model.LyricsSourcePerformerEvidenceState {
	if full == nil || !songResultFullHasPerformerSegmentation(*full) {
		return model.LyricsSourcePerformerEvidenceNone
	}
	return model.LyricsSourcePerformerEvidenceSourcePartial
}

func populateSongResultLegacyReadingEvidence(
	full *model.LyricsSourceFull,
	renditionKey string,
	side model.LyricsSourceRenditionSide,
	rubyReference *model.LyricsSourceComponentRef,
) {
	if full == nil || rubyReference == nil {
		return
	}
	for lineIndex := range full.Lines {
		for segmentIndex := range full.Lines[lineIndex].Segments {
			for spanIndex := range full.Lines[lineIndex].Segments[segmentIndex].Ruby {
				span := &full.Lines[lineIndex].Segments[segmentIndex].Ruby[spanIndex]
				if span.Reading == "" {
					continue
				}
				span.ReadingEvidence = &model.LyricsSourceReadingEvidence{
					Kind:             model.LyricsSourceReadingEvidenceLegacyV2Component,
					FixedIdentityKey: rubyReference.RenditionKey,
					RenditionKey:     renditionKey, Side: side,
					SourceRowOrdinal: lineIndex + 1, SourceSegmentOrdinal: segmentIndex + 1,
				}
			}
		}
	}
}

func legacySongResultV2RenditionEvidence(
	result SongResult,
) map[model.LyricsSourceRenditionComponentKind][]lyricscontract.EvidenceRef {
	components := make(map[model.LyricsSourceRenditionComponentKind][]lyricscontract.EvidenceRef)
	if result.Full != nil {
		components[model.LyricsSourceRenditionComponentFullText] = result.Components.FullText
		if songResultFullHasPerformerSegmentation(*result.Full) {
			components[model.LyricsSourceRenditionComponentFullPerformerSegmentation] = result.Components.PerformerSegmentation
		}
		if songResultFullHasRuby(*result.Full) {
			components[model.LyricsSourceRenditionComponentFullRuby] = result.Components.Ruby
		}
	}
	if result.Game != nil {
		components[model.LyricsSourceRenditionComponentGameText] = result.Components.GameText
		if result.Full == nil {
			if songResultFullHasPerformerSegmentation(*result.Game) {
				components[model.LyricsSourceRenditionComponentGamePerformerSegmentation] = result.Components.PerformerSegmentation
			}
			if songResultFullHasRuby(*result.Game) {
				components[model.LyricsSourceRenditionComponentGameRuby] = result.Components.Ruby
			}
		} else {
			if songResultFullHasPerformerSegmentation(*result.Game) {
				components[model.LyricsSourceRenditionComponentGamePerformerSegmentation] = result.Components.GameText
			}
			if songResultFullHasRuby(*result.Game) {
				components[model.LyricsSourceRenditionComponentGameRuby] = result.Components.GameText
			}
		}
	}
	if result.GameProjection != nil {
		components[model.LyricsSourceRenditionComponentRelation] = result.Components.GameProjection
	} else if len(result.Components.GameText) != 0 {
		components[model.LyricsSourceRenditionComponentRelation] = result.Components.GameText
	} else {
		components[model.LyricsSourceRenditionComponentRelation] = result.Components.VersionEvidence
	}
	components[model.LyricsSourceRenditionComponentVersion] = result.Components.VersionEvidence
	return components
}

func inferSongResultV2ComponentOutcome(
	result SongResult,
	refs []lyricscontract.EvidenceRef,
) (string, error) {
	if len(refs) == 0 {
		return "", errors.New("lyrics recovery song result v2 component has no exact evidence")
	}
	provider := refs[0].Provider
	for _, ref := range refs[1:] {
		if ref.Provider != provider {
			return "", errors.New("lyrics recovery song result v2 component spans provider artifacts")
		}
	}
	outcomeID := ""
	for _, outcome := range result.ProviderOutcomes {
		if outcome.Provider != provider {
			continue
		}
		if outcomeID != "" {
			return "", errors.New("lyrics recovery song result v2 component provider is ambiguous")
		}
		outcomeID = outcome.OutcomeID
	}
	if outcomeID == "" {
		return "", errors.New("lyrics recovery song result v2 component has no provider outcome")
	}
	return outcomeID, nil
}

func songResultFullHasRuby(full model.LyricsSourceFull) bool {
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

func cloneSongResultRenditions(input []SongResultRendition) []SongResultRendition {
	if input == nil {
		return nil
	}
	result := make([]SongResultRendition, len(input))
	for index, rendition := range input {
		result[index] = rendition
		result[index].SourceTabPaths = cloneSongResultTabPaths(rendition.SourceTabPaths)
		result[index].SourcePerformerIDs = append([]string(nil), rendition.SourcePerformerIDs...)
		result[index].Full = cloneSongResultFull(rendition.Full)
		result[index].Game = cloneSongResultFull(rendition.Game)
		result[index].Relation = cloneSongResultRelation(rendition.Relation)
		result[index].PrivateReview = cloneSongResultPrivateReview(rendition.PrivateReview)
		result[index].Components = make([]RenditionComponentEvidenceRef, len(rendition.Components))
		for componentIndex, component := range rendition.Components {
			result[index].Components[componentIndex] = component
			result[index].Components[componentIndex].Evidence = cloneEvidenceRefs(component.Evidence)
		}
		result[index].Translations = cloneStringsPreservingNil(rendition.Translations)
		result[index].PeerTranslations = make([]SongResultPeerTranslation, len(rendition.PeerTranslations))
		for peerIndex, peer := range rendition.PeerTranslations {
			result[index].PeerTranslations[peerIndex] = peer
			result[index].PeerTranslations[peerIndex].Translations = cloneStringsPreservingNil(peer.Translations)
		}
		if rendition.PeerTranslations == nil {
			result[index].PeerTranslations = nil
		}
	}
	return result
}

func cloneSongResultPrivateReview(
	input *model.LyricsSourcePrivateReview,
) *model.LyricsSourcePrivateReview {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}

func cloneSongResultFull(input *model.LyricsSourceFull) *model.LyricsSourceFull {
	if input == nil {
		return nil
	}
	full := cloneLyricsSourceFull(*input)
	return &full
}

func cloneSongResultTabPaths(input []model.LyricsSourceTabPath) []model.LyricsSourceTabPath {
	if input == nil {
		return nil
	}
	result := make([]model.LyricsSourceTabPath, len(input))
	for index, path := range input {
		result[index] = append(model.LyricsSourceTabPath{}, path...)
	}
	return result
}

func cloneSongResultRelation(input model.LyricsSourceRenditionRelation) model.LyricsSourceRenditionRelation {
	input.LineIDs = cloneStringsPreservingNil(input.LineIDs)
	return input
}
