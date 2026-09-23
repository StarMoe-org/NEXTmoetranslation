package lyricsrecovery

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
)

type exactPublicExtractionReportCatalog struct {
	MusicID          int    `json:"musicId"`
	JapaneseTitle    string `json:"japaneseTitle"`
	ProducerMetadata string `json:"producerMetadata"`
	Lyricist         string `json:"lyricist"`
	Composer         string `json:"composer"`
	Arranger         string `json:"arranger"`
	LyricsVersion    string `json:"lyricsVersion"`
}

type exactPublicExtractionReport struct {
	SchemaVersion   int                                  `json:"schemaVersion"`
	Provider        model.LyricsSourceProvider           `json:"provider"`
	URLReportSHA256 string                               `json:"urlReportSha256"`
	RawHTMLSHA256   string                               `json:"rawHtmlSha256"`
	CatalogSHA256   string                               `json:"catalogSha256"`
	Catalog         exactPublicExtractionReportCatalog   `json:"catalog"`
	PageURL         string                               `json:"pageUrl"`
	PageTitle       string                               `json:"pageTitle"`
	JapaneseTitle   string                               `json:"japaneseTitle"`
	PageID          int                                  `json:"pageId"`
	RevisionID      int                                  `json:"revisionId"`
	FetchedAt       string                               `json:"fetchedAt"`
	RightsNotice    string                               `json:"rightsNotice"`
	LineCount       int                                  `json:"lineCount"`
	StanzaCount     int                                  `json:"stanzaCount"`
	Lines           []lyricssource.MoegirlPublicHTMLLine `json:"lines"`
}

func acquireExactPublicArtifact(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
	runtime RuntimeConfig,
	ledger *lyricsacquisition.Ledger,
) (ProviderAcquisitionSet, ProviderReplay, error) {
	if ctx == nil || ledger == nil || musicID <= 0 || identity.MusicID != musicID {
		return ProviderAcquisitionSet{}, ProviderReplay{}, errors.New("exact public artifact acquisition input is invalid")
	}
	target, found, err := runtime.ExactPublicTarget(musicID)
	if err != nil || !found {
		if err != nil {
			return ProviderAcquisitionSet{}, ProviderReplay{}, err
		}
		return ProviderAcquisitionSet{}, ProviderReplay{}, errors.New("exact public artifact target is missing")
	}
	if identity.JapaneseTitle != target.JapaneseTitle {
		return ProviderAcquisitionSet{}, ProviderReplay{}, errors.New("exact public artifact catalog title does not match the immutable target")
	}

	raw, err := readExactPublicFile(target.RawHTML, 2<<20)
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, fmt.Errorf("read exact public HTML: %w", err)
	}
	report, err := readExactPublicFile(target.ExtractionReport, 1<<20)
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, fmt.Errorf("read exact public extraction report: %w", err)
	}
	extracted, err := lyricssource.ParseMoegirlPublicPageHTML(raw, target.PageURL)
	if err != nil || extracted.PageID != target.PageID || extracted.RevisionID != target.RevisionID ||
		extracted.PageTitle != target.PageTitle || extracted.JapaneseTitle != target.JapaneseTitle {
		return ProviderAcquisitionSet{}, ProviderReplay{}, errors.New("exact public HTML conflicts with its immutable page identity")
	}
	if err := validateExactPublicExtractionReport(report, target, extracted); err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, err
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, target.FetchedAt)
	if err != nil || fetchedAt.Location() != time.UTC || fetchedAt.UTC().Format(time.RFC3339Nano) != target.FetchedAt {
		return ProviderAcquisitionSet{}, ProviderReplay{}, errors.New("exact public artifact fetchedAt is invalid")
	}

	fixed, candidate, evidence, err := buildExactPublicFixed(target, extracted, raw, fetchedAt)
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, err
	}
	envelope, err := json.Marshal(evidence)
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, err
	}
	envelopeDigest := sha256.Sum256(envelope)
	rawDigest := sha256.Sum256(raw)
	rawSHA1 := sha1.Sum(raw)
	selector := fmt.Sprintf("public-revision:%d", target.RevisionID)
	committed, err := ledger.Commit(ctx, lyricsacquisition.RecordInput{
		Request: lyricsacquisition.Request{
			Provider:                 string(lyricssource.ProviderMoegirlPublicExact),
			CanonicalRequestIdentity: target.PageURL,
			Kind:                     lyricsacquisition.RequestKindRevision,
			RevisionSelector:         selector,
		},
		FetchedAt:   target.FetchedAt,
		RawResponse: append([]byte(nil), raw...), RawResponseSHA256: hex.EncodeToString(rawDigest[:]),
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: evidence.EvidenceID, Raw: append([]byte(nil), evidence.Raw...),
			RawSHA256: evidence.RawSHA256,
		},
		EvidenceEnvelope:       append([]byte(nil), envelope...),
		EvidenceEnvelopeSHA256: hex.EncodeToString(envelopeDigest[:]),
		ObservedRevisions: []lyricsacquisition.ObservedRevision{{
			Selector: selector, RevisionID: int64(target.RevisionID), SHA1: hex.EncodeToString(rawSHA1[:]),
		}},
	})
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, err
	}
	replayed, err := ledger.ReplayByAcquisitionID(ctx, committed.AcquisitionID)
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, err
	}

	counts := lyricsprovideroutcome.Counts{Acquisitions: 1, Targets: 1, Evaluated: 1, Candidates: 1}
	outcome, err := lyricsprovideroutcome.New(
		lyricssource.ProviderMoegirlPublicExact,
		lyricsprovideroutcome.StatusCandidate,
		[]lyricssource.Candidate{candidate},
		lyricsprovideroutcome.Diagnostic{
			Provider:   lyricssource.ProviderMoegirlPublicExact,
			Phase:      lyricsprovideroutcome.PhaseFinalize,
			ReasonCode: lyricsprovideroutcome.ReasonCandidate,
			Counts:     counts,
			AcquisitionRefs: []model.LyricsSourceIndexEvidenceRef{{
				EvidenceID: evidence.EvidenceID, SHA256: evidence.SHA256,
			}},
		},
	)
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, err
	}
	replay, err := buildExactPublicProviderReplay(musicID, outcome, candidate, fixed, replayed, runtime)
	if err != nil {
		return ProviderAcquisitionSet{}, ProviderReplay{}, err
	}
	terminal := ProviderAcquisitionSet{
		Provider:       lyricssource.ProviderMoegirlPublicExact,
		AcquisitionIDs: []lyricsacquisition.AcquisitionID{committed.AcquisitionID},
		Status:         outcome.Status, ReasonCode: outcome.Diagnostic.ReasonCode,
		Phase: outcome.Diagnostic.Phase, Counts: outcome.Diagnostic.Counts,
	}
	return terminal, replay, nil
}

func readExactPublicFile(binding lyricssource.ExactPublicFileBinding, maximum int) ([]byte, error) {
	body, err := readPrivateFile(binding.Path, maximum, 1)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != binding.SizeBytes || digestHex(body) != binding.SHA256 {
		return nil, errors.New("exact public file does not match its immutable size and SHA-256")
	}
	return body, nil
}

func validateExactPublicExtractionReport(
	body []byte,
	target lyricssource.ExactPublicPageTarget,
	extracted lyricssource.MoegirlPublicHTMLExtraction,
) error {
	if len(body) == 0 || len(body) > 1<<20 {
		return errors.New("exact public extraction report exceeds its byte boundary")
	}
	var report exactPublicExtractionReport
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return fmt.Errorf("decode exact public extraction report: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("exact public extraction report contains trailing JSON")
	}
	if report.SchemaVersion != 1 || report.Provider != lyricssource.ProviderMoegirlPublicExact ||
		!canonicalLowerSHA256(report.URLReportSHA256) || !canonicalLowerSHA256(report.CatalogSHA256) ||
		report.RawHTMLSHA256 != target.RawHTML.SHA256 || report.Catalog.MusicID != target.MusicID ||
		report.Catalog.JapaneseTitle != target.JapaneseTitle ||
		(report.Catalog.LyricsVersion != "full" && report.Catalog.LyricsVersion != "game_size") ||
		report.PageURL != target.PageURL || report.PageTitle != target.PageTitle ||
		report.JapaneseTitle != target.JapaneseTitle || report.PageID != target.PageID ||
		report.RevisionID != target.RevisionID || report.FetchedAt != target.FetchedAt ||
		report.RightsNotice != extracted.RightsNotice || report.LineCount != len(extracted.Lines) ||
		len(report.Lines) != len(extracted.Lines) {
		return errors.New("exact public extraction report conflicts with its immutable target")
	}
	stanzaCount := 0
	if len(extracted.Lines) > 0 {
		stanzaCount = 1
	}
	for index, line := range extracted.Lines {
		if line.StanzaBreakBefore {
			stanzaCount++
		}
		if report.Lines[index] != line {
			return errors.New("exact public extraction report lyrics conflict with the retained HTML")
		}
	}
	if report.StanzaCount != stanzaCount {
		return errors.New("exact public extraction report stanza count conflicts with the retained HTML")
	}
	return nil
}

func buildExactPublicFixed(
	target lyricssource.ExactPublicPageTarget,
	extracted lyricssource.MoegirlPublicHTMLExtraction,
	raw []byte,
	fetchedAt time.Time,
) (lyricssource.FixedRevision, lyricssource.Candidate, lyricssource.IndexEvidence, error) {
	if len(extracted.Lines) == 0 {
		return lyricssource.FixedRevision{}, lyricssource.Candidate{}, lyricssource.IndexEvidence{},
			errors.New("exact public artifact contains no lyrics")
	}
	rawSHA256 := digestHex(raw)
	rawSHA1Digest := sha1.Sum(raw)
	rawSHA1 := hex.EncodeToString(rawSHA1Digest[:])
	evidenceID := lyricssource.ExactPublicPageEvidenceID(
		target.PageID, target.RevisionID, target.PageURL, target.FetchedAt, rawSHA256,
	)
	if evidenceID == "" {
		return lyricssource.FixedRevision{}, lyricssource.Candidate{}, lyricssource.IndexEvidence{},
			errors.New("exact public evidence identity is invalid")
	}
	evidence := lyricssource.IndexEvidence{
		EvidenceID: evidenceID, SHA256: rawSHA256,
		Kind:     lyricssource.IndexEvidenceKindExactPublicHTML,
		Provider: lyricssource.ProviderMoegirlPublicExact, Origin: lyricssource.OriginMoegirlPublicExact,
		PageID: target.PageID, RevisionID: target.RevisionID, Title: target.PageTitle,
		CanonicalURL: target.PageURL, Categories: []string{}, CanonicalRequestURL: target.PageURL,
		FetchedAt: target.FetchedAt, Raw: append([]byte(nil), raw...), RawSHA256: rawSHA256,
	}
	ref := model.LyricsSourceIndexEvidenceRef{EvidenceID: evidenceID, SHA256: rawSHA256}
	structured := make([]lyricssource.StructuredLine, len(extracted.Lines))
	legacy := make([]lyricssource.ExtractedLine, len(extracted.Lines))
	translations := make([]string, len(extracted.Lines))
	for index, line := range extracted.Lines {
		ruby, rubyErr := lyricssource.GenerateDeterministicRubySpans(line.Japanese)
		if rubyErr != nil {
			return lyricssource.FixedRevision{}, lyricssource.Candidate{}, lyricssource.IndexEvidence{},
				fmt.Errorf("exact public artifact ruby line %d: %w", index+1, rubyErr)
		}
		structured[index] = lyricssource.StructuredLine{
			Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments: []lyricssource.LyricsSegment{{
				Text: line.Japanese, PerformerIDs: []string{}, Ruby: ruby,
			}},
			TrailingPerformerIDs: []string{},
		}
		legacy[index] = lyricssource.ExtractedLine{
			Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore,
		}
		translations[index] = line.Translation
	}
	extraction := lyricssource.Extraction{
		Version:              lyricssource.LyricsVersion{Kind: "vocaloid", Label: "Virtual Singer Version"},
		Performers:           []lyricssource.Performer{},
		RubyGeneratorVersion: lyricssource.DeterministicRubyGeneratorVersion(), Lines: structured,
	}
	candidate := lyricssource.Candidate{
		Provider: lyricssource.ProviderMoegirlPublicExact, Origin: lyricssource.OriginMoegirlPublicExact,
		PageID: target.PageID, Title: target.PageTitle, CanonicalURL: target.PageURL,
		RevisionID: target.RevisionID, SHA1: rawSHA1, RawSHA256: rawSHA256,
		Categories: []string{}, FetchedAt: target.FetchedAt, Section: "Lyrics",
		RenditionKey: "full-vocaloid", VersionReason: model.LyricsSourceVersionReasonUntaggedFullOnly,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{ref},
		IndexEvidence:     []lyricssource.IndexEvidence{evidence},
	}
	fixed := lyricssource.FixedRevision{
		Provider: lyricssource.ProviderMoegirlPublicExact, Origin: lyricssource.OriginMoegirlPublicExact,
		CanonicalURL: target.PageURL, PageID: target.PageID, PageTitle: target.PageTitle,
		RevisionID: target.RevisionID, SHA1: rawSHA1, RawSHA256: rawSHA256,
		Categories: []string{}, FetchedAt: fetchedAt, Wikitext: append([]byte(nil), raw...),
		Lines: legacy, Translations: translations, Extraction: extraction,
		Section: candidate.Section, RenditionKey: candidate.RenditionKey,
		VersionReason:     candidate.VersionReason,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{ref},
		IndexEvidence:     []lyricssource.IndexEvidence{evidence},
	}
	return fixed, candidate, evidence, nil
}

func buildExactPublicProviderReplay(
	musicID int,
	outcome lyricsprovideroutcome.Outcome[lyricssource.Candidate],
	candidate lyricssource.Candidate,
	fixed lyricssource.FixedRevision,
	acquired lyricsacquisition.Acquisition,
	runtime RuntimeConfig,
) (ProviderReplay, error) {
	if outcome.Validate() != nil || outcome.Provider != lyricssource.ProviderMoegirlPublicExact ||
		candidate.Provider != outcome.Provider || runtime.PolicyVersion == "" {
		return ProviderReplay{}, errors.New("exact public provider replay input is invalid")
	}
	evidenceRefs, artifactRefs, err := replayReferences([]lyricsacquisition.Acquisition{acquired})
	if err != nil {
		return ProviderReplay{}, err
	}
	if err := outcomeReferencesResolved(outcome, evidenceRefs); err != nil {
		return ProviderReplay{}, err
	}
	parserVersion := runtime.Parsers[outcome.Provider]
	if parserVersion == "" {
		return ProviderReplay{}, errors.New("exact public provider parser version is missing")
	}
	artifact, err := lyricsoutcomeartifact.New(
		musicID, outcome.Provider, outcome.Status, outcome.Diagnostic.ReasonCode, outcome.Diagnostic.Phase,
		outcome.Diagnostic.Counts, parserVersion, runtime.PolicyVersion,
		&lyricsoutcomeartifact.CandidateIdentity{
			PageID: candidate.PageID, RevisionID: candidate.RevisionID, SHA1: candidate.SHA1,
			RawSHA256: candidate.RawSHA256, RenditionKey: candidate.RenditionKey,
			VersionReason: candidate.VersionReason, LineCount: len(fixed.Extraction.Lines),
		},
		artifactRefs,
	)
	if err != nil {
		return ProviderReplay{}, err
	}
	fixedCopy := fixed
	return ProviderReplay{
		Outcome: outcome, Artifact: artifact, Fixed: &fixedCopy, EvidenceRefs: evidenceRefs,
	}, nil
}

func replayExactPublicArtifact(
	ctx context.Context,
	musicID int,
	identity lyricssource.MusicIdentity,
	runtime RuntimeConfig,
	ledger *lyricsacquisition.Ledger,
	terminal ProviderAcquisitionSet,
) (ProviderReplay, error) {
	if ctx == nil || ledger == nil || terminal.Provider != lyricssource.ProviderMoegirlPublicExact ||
		len(terminal.AcquisitionIDs) != 1 || identity.MusicID != musicID {
		return ProviderReplay{}, errors.New("exact public artifact replay input is invalid")
	}
	target, found, err := runtime.ExactPublicTarget(musicID)
	if err != nil || !found || identity.JapaneseTitle != target.JapaneseTitle {
		if err != nil {
			return ProviderReplay{}, err
		}
		return ProviderReplay{}, errors.New("exact public artifact replay target is missing")
	}
	acquired, err := ledger.ReplayByAcquisitionID(ctx, terminal.AcquisitionIDs[0])
	if err != nil {
		return ProviderReplay{}, err
	}
	if err := validateExactPublicReplayAcquisition(acquired, target); err != nil {
		return ProviderReplay{}, err
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, target.FetchedAt)
	if err != nil {
		return ProviderReplay{}, err
	}
	extracted, err := lyricssource.ParseMoegirlPublicPageHTML(acquired.RawResponse, target.PageURL)
	if err != nil {
		return ProviderReplay{}, err
	}
	fixed, candidate, evidence, err := buildExactPublicFixed(target, extracted, acquired.RawResponse, fetchedAt)
	if err != nil {
		return ProviderReplay{}, err
	}
	counts := lyricsprovideroutcome.Counts{Acquisitions: 1, Targets: 1, Evaluated: 1, Candidates: 1}
	outcome, err := lyricsprovideroutcome.New(
		lyricssource.ProviderMoegirlPublicExact,
		lyricsprovideroutcome.StatusCandidate,
		[]lyricssource.Candidate{candidate},
		lyricsprovideroutcome.Diagnostic{
			Provider:   lyricssource.ProviderMoegirlPublicExact,
			Phase:      lyricsprovideroutcome.PhaseFinalize,
			ReasonCode: lyricsprovideroutcome.ReasonCandidate,
			Counts:     counts,
			AcquisitionRefs: []model.LyricsSourceIndexEvidenceRef{{
				EvidenceID: evidence.EvidenceID, SHA256: evidence.SHA256,
			}},
		},
	)
	if err != nil {
		return ProviderReplay{}, err
	}
	if terminal.Status != outcome.Status || terminal.ReasonCode != outcome.Diagnostic.ReasonCode ||
		terminal.Phase != outcome.Diagnostic.Phase || terminal.Counts != outcome.Diagnostic.Counts {
		return ProviderReplay{}, errors.New("exact public replay terminal conflicts with the immutable artifact")
	}
	return buildExactPublicProviderReplay(musicID, outcome, candidate, fixed, acquired, runtime)
}

func validateExactPublicReplayAcquisition(
	acquired lyricsacquisition.Acquisition,
	target lyricssource.ExactPublicPageTarget,
) error {
	if !acquired.ReplayOnly || acquired.Request.Provider != string(lyricssource.ProviderMoegirlPublicExact) ||
		acquired.Request.CanonicalRequestIdentity != target.PageURL ||
		acquired.Request.Kind != lyricsacquisition.RequestKindRevision ||
		acquired.Request.RevisionSelector != fmt.Sprintf("public-revision:%d", target.RevisionID) ||
		acquired.FetchedAt != target.FetchedAt || acquired.RawResponseSHA256 != target.RawHTML.SHA256 ||
		int64(len(acquired.RawResponse)) != target.RawHTML.SizeBytes || digestHex(acquired.RawResponse) != target.RawHTML.SHA256 ||
		len(acquired.ObservedRevisions) != 1 {
		return errors.New("exact public replay acquisition conflicts with the immutable target")
	}
	observed := acquired.ObservedRevisions[0]
	rawSHA1 := sha1.Sum(acquired.RawResponse)
	if observed.Selector != acquired.Request.RevisionSelector || observed.RevisionID != int64(target.RevisionID) ||
		observed.Timestamp != "" || observed.SHA1 != hex.EncodeToString(rawSHA1[:]) {
		return errors.New("exact public replay observed revision conflicts with the immutable target")
	}
	extracted, err := lyricssource.ParseMoegirlPublicPageHTML(acquired.RawResponse, target.PageURL)
	if err != nil || extracted.PageID != target.PageID || extracted.RevisionID != target.RevisionID ||
		extracted.PageTitle != target.PageTitle || extracted.JapaneseTitle != target.JapaneseTitle {
		return errors.New("exact public replay HTML conflicts with the immutable target")
	}
	envelopeDigest := sha256.Sum256(acquired.EvidenceEnvelope)
	if acquired.EvidenceEnvelopeSHA256 != hex.EncodeToString(envelopeDigest[:]) ||
		!bytes.Equal(acquired.Evidence.Raw, acquired.RawResponse) || acquired.Evidence.RawSHA256 != target.RawHTML.SHA256 {
		return errors.New("exact public replay evidence envelope is invalid")
	}
	return nil
}
