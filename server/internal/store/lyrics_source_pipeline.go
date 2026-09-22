package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsdiscovery"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

const (
	maxLyricsSourceRawBytes       = 2 << 20
	maxLyricsSourceCategories     = 256
	maxLyricsSourceCategoryBytes  = 512
	maxLyricsSourceEvidence       = 64
	maxLyricsSourceEvidenceBytes  = 2048
	maxLyricsReviewNoteBytes      = 2000
	maxLyricsReviewEvidenceBytes  = 1 << 20
	maxLyricsReviewActorBytes     = 128
	maxLyricsReviewIdempotencyKey = 128
)

var (
	ErrLyricsSourceArtifactNotFound = errors.New("lyrics source artifact not found")
	ErrLyricsSourceArtifactConflict = errors.New("lyrics source artifact immutable conflict")
	ErrLyricsSourceReviewNotFound   = errors.New("lyrics source review not found")
	ErrLyricsSourceReviewConflict   = errors.New("lyrics source review conflict")
	ErrLyricsSourceIdempotency      = errors.New("lyrics source review idempotency conflict")
	ErrLyricsSourceInvalidRequest   = errors.New("invalid lyrics source review request")
)

type lyricsSourceArtifactEnvelope struct {
	Version       int      `json:"version"`
	SourceType    string   `json:"sourceType"`
	SourceOrigin  string   `json:"sourceOrigin"`
	PageID        int      `json:"pageId"`
	RevisionID    int      `json:"revisionId"`
	PageTitle     string   `json:"pageTitle"`
	CanonicalURL  string   `json:"canonicalUrl"`
	MediaWikiSHA1 string   `json:"mediaWikiSha1"`
	Categories    []string `json:"categories"`
	RawSHA256     string   `json:"rawWikitextSha256"`
}

type lyricsSourceAnalysisEnvelope struct {
	Version                  int                               `json:"version"`
	ArtifactSHA256           string                            `json:"artifactSha256"`
	MusicID                  int                               `json:"musicId"`
	CatalogFingerprint       string                            `json:"catalogFingerprint"`
	MatchingPolicyVersion    string                            `json:"matchingPolicyVersion"`
	RestrictionPolicyVersion string                            `json:"restrictionPolicyVersion"`
	ExtractorVersion         string                            `json:"extractorVersion"`
	MatchOutcome             string                            `json:"matchOutcome"`
	RestrictionOutcome       string                            `json:"restrictionOutcome"`
	ExtractionOutcome        string                            `json:"extractionOutcome"`
	MatchingEvidence         []model.LyricsSourceEvidence      `json:"matchingEvidence"`
	RestrictionRuleIDs       []string                          `json:"restrictionRuleIds"`
	SelectedVersion          model.LyricsSourceVersion         `json:"selectedVersion"`
	Performers               []model.LyricsSourcePerformer     `json:"performers"`
	RubyGeneratorVersion     string                            `json:"rubyGeneratorVersion"`
	ExtractedLines           []model.LyricsSourceExtractedLine `json:"extractedLines"`
	ExtractedLinesSHA256     string                            `json:"extractedLinesSha256"`
}

type CompleteLyricsDiscoveryResultParams struct {
	JobID           int64
	LeaseOwner      string
	ExpectedVersion int64
	CompletedAt     time.Time
	ShadowResult    lyricsdiscovery.Result
	Candidates      []lyricssource.Candidate
	IndexEvidence   []lyricssource.IndexEvidence
}

func (s *Store) CompleteLyricsDiscoveryResult(ctx context.Context, params CompleteLyricsDiscoveryResultParams) error {
	if ctx == nil {
		return errors.New("lyrics discovery completion requires context")
	}
	if _, err := validateLyricsDiscoveryShadowResult(params.ShadowResult); err != nil {
		return err
	}
	artifactCandidates, artifactEvidence, err := decodeLyricsDiscoveryArtifact(
		params.ShadowResult.Artifact, params.ShadowResult.CandidateCount,
	)
	if err != nil {
		return lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, err)
	}
	if !sameCompactLyricsDiscoveryCandidates(artifactCandidates, params.Candidates) ||
		!sameLyricsIndexEvidenceCollection(artifactEvidence, params.IndexEvidence) {
		return lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult,
			errors.New("discovery completion arguments drifted from the evidence artifact"))
	}
	artifact, err := canonicalStoredLyricsDiscoveryArtifact(artifactCandidates)
	if err != nil {
		return lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, err)
	}
	params.Candidates = artifactCandidates
	params.IndexEvidence = artifactEvidence
	completedAt := canonicalLyricsDiscoveryTime(params.CompletedAt)
	if completedAt.IsZero() || completedAt.After(time.Now().UTC().Add(maxLyricsDiscoveryClockSkew)) {
		return lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, errors.New("invalid completion time"))
	}
	owner := strings.TrimSpace(params.LeaseOwner)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLyricsDiscoveryJobContext(ctx, tx, `WHERE job_id=?`, params.JobID)
	if err != nil {
		return err
	}
	leaseCheckedAt := canonicalLyricsDiscoveryTime(time.Now().UTC())
	var provider model.LyricsSourceProvider
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM lyrics_discovery_jobs WHERE job_id=?`, job.ID).Scan(&provider); err != nil {
		return err
	}
	if provider != model.LyricsSourceProviderVocaloidFandom {
		return errors.New("legacy discovery completion cannot write a non-Fandom provider job")
	}
	if job.Kind != model.LyricsDiscoveryJobDiscover || job.State != model.LyricsDiscoveryJobLeased ||
		job.LeaseOwner != owner || job.Version != params.ExpectedVersion || !job.LeaseExpiresAt.After(leaseCheckedAt) {
		if model.IsTerminalLyricsDiscoveryJobState(job.State) {
			return fmt.Errorf("%w: %s", ErrLyricsDiscoveryJobTerminal, job.State)
		}
		return ErrLyricsDiscoveryLeaseNotOwned
	}
	if err := insertOrVerifyLyricsIndexEvidenceCollectionTx(ctx, tx, params.IndexEvidence, completedAt); err != nil {
		return err
	}
	insertedResult, err := tx.ExecContext(ctx, `INSERT INTO lyrics_discovery_shadow_results
		(job_id, music_id, catalog_fingerprint, policy_version, outcome, candidate_count, result_json, created_at, provider)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.ID, job.Target.MusicID, job.Target.CatalogFingerprint,
		job.Target.PolicyVersion, params.ShadowResult.Outcome, params.ShadowResult.CandidateCount,
		string(artifact), completedAt.UnixMilli(), provider)
	if err != nil {
		return err
	}
	resultID, err := insertedResult.LastInsertId()
	if err != nil {
		return err
	}
	if err := linkLyricsDiscoveryResultEvidenceTx(ctx, tx, resultID, params.IndexEvidence); err != nil {
		return err
	}
	if len(params.Candidates) == 1 {
		candidate := cloneLyricsDiscoveryCandidate(params.Candidates[0])
		candidateProvider := candidate.Provider
		if candidateProvider == "" {
			candidateProvider = model.LyricsSourceProviderVocaloidFandom
		}
		if err := validateProviderAwareLyricsDiscoveryCandidate(candidateProvider, candidate); err != nil {
			return lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, errors.New("invalid provider-aware fixed candidate identity"))
		}
		fixedCandidate := legacyLyricsDiscoveryCandidateIdentity(&candidate)
		_, _, err = enqueueLyricsDiscoveryJobTx(ctx, tx, EnqueueLyricsDiscoveryJobParams{
			Provider: candidateProvider, Kind: model.LyricsDiscoveryJobFetchRevision,
			Target: model.LyricsDiscoveryJobTarget{
				MusicID: job.Target.MusicID, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
				CatalogFingerprint: job.Target.CatalogFingerprint, PolicyVersion: model.LyricsMatchingPolicyVersion,
				ExpectedSHA1: candidate.SHA1, FixedCandidate: fixedCandidate,
			},
			FixedCandidate: &candidate,
			MaxAttempts:    job.MaxAttempts,
		}, completedAt)
		if err != nil {
			return err
		}
	} else if len(params.Candidates) > 1 {
		evidence, err := canonicalCandidateReviewEvidence(params.Candidates)
		if err != nil {
			return err
		}
		review, _, err := createLyricsSourceReviewTx(ctx, tx, createLyricsSourceReviewParams{
			Provider: provider, Kind: "candidate_selection", MusicID: job.Target.MusicID, CatalogFingerprint: job.Target.CatalogFingerprint,
			ReasonCode: "ambiguous_candidates", EvidenceJSON: evidence, Priority: 0, CreatedAt: completedAt,
		})
		if err != nil {
			return err
		}
		if err := linkLyricsSourceReviewEvidenceTx(ctx, tx, review.ReviewID, params.IndexEvidence); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE lyrics_discovery_jobs SET state='succeeded', next_attempt_at=?,
		lease_owner=NULL, lease_expires_at=NULL, last_error_code=NULL, updated_at=?, completed_at=?, version=version+1
		WHERE job_id=? AND state='leased' AND lease_owner=? AND version=? AND lease_expires_at>?`,
		completedAt.UnixMilli(), completedAt.UnixMilli(), completedAt.UnixMilli(), job.ID, owner,
		params.ExpectedVersion, leaseCheckedAt.UnixMilli())
	if err != nil {
		return err
	}
	if err := requireLyricsDiscoveryJobTransition(ctx, tx, result, job.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func canonicalCandidateReviewEvidence(candidates []lyricssource.Candidate) ([]byte, error) {
	if len(candidates) < 2 || len(candidates) > 8 {
		return nil, errors.New("candidate review requires 2-8 candidates")
	}
	for _, candidate := range candidates {
		provider := candidate.Provider
		if provider == "" {
			provider = model.LyricsSourceProviderVocaloidFandom
		}
		if err := validateProviderAwareLyricsDiscoveryCandidate(provider, candidate); err != nil {
			return nil, errors.New("candidate review contains invalid provider-aware identity")
		}
	}
	canonical := make([]lyricssource.Candidate, len(candidates))
	for index := range candidates {
		canonical[index] = cloneLyricsDiscoveryCandidate(candidates[index])
	}
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Provider != canonical[j].Provider {
			return canonical[i].Provider < canonical[j].Provider
		}
		if canonical[i].PageID != canonical[j].PageID {
			return canonical[i].PageID < canonical[j].PageID
		}
		if canonical[i].RevisionID != canonical[j].RevisionID {
			return canonical[i].RevisionID < canonical[j].RevisionID
		}
		if canonical[i].Section != canonical[j].Section {
			return canonical[i].Section < canonical[j].Section
		}
		return canonical[i].RenditionKey < canonical[j].RenditionKey
	})
	payload, err := json.Marshal(struct {
		Candidates []lyricssource.Candidate `json:"candidates"`
	}{Candidates: canonical})
	if err != nil || len(payload) > maxLyricsReviewEvidenceBytes {
		return nil, errors.New("candidate evidence exceeds safe limit")
	}
	return payload, legacy.ValidateUniqueJSON(payload)
}

type CompleteLyricsFetchParams struct {
	JobID           int64
	LeaseOwner      string
	ExpectedVersion int64
	CompletedAt     time.Time
	Fixed           lyricssource.FixedRevision
	Evidence        []model.LyricsSourceEvidence
	Associations    []model.LyricsSourceAssociation
}

func (s *Store) CompleteLyricsFetch(ctx context.Context, params CompleteLyricsFetchParams) (model.LyricsSourceReviewItem, error) {
	if ctx == nil {
		return model.LyricsSourceReviewItem{}, errors.New("lyrics fetch completion requires context")
	}
	owner := strings.TrimSpace(params.LeaseOwner)
	completedAt := canonicalLyricsDiscoveryTime(params.CompletedAt)
	if owner == "" || completedAt.IsZero() || completedAt.After(time.Now().UTC().Add(maxLyricsDiscoveryClockSkew)) {
		return model.LyricsSourceReviewItem{}, errors.New("invalid lyrics fetch completion")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	defer tx.Rollback()
	job, err := loadLyricsDiscoveryJobContext(ctx, tx, `WHERE job_id=?`, params.JobID)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	leaseCheckedAt := canonicalLyricsDiscoveryTime(time.Now().UTC())
	var provider model.LyricsSourceProvider
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM lyrics_discovery_jobs WHERE job_id=?`, job.ID).Scan(&provider); err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	fixedCandidate, err := loadLyricsDiscoveryFixedCandidateWithEvidenceContext(ctx, tx, job.ID)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	if job.Kind != model.LyricsDiscoveryJobFetchRevision || job.State != model.LyricsDiscoveryJobLeased ||
		job.LeaseOwner != owner || job.Version != params.ExpectedVersion || !job.LeaseExpiresAt.After(leaseCheckedAt) {
		return model.LyricsSourceReviewItem{}, ErrLyricsDiscoveryLeaseNotOwned
	}
	if fixedCandidate == nil || !fixedRevisionMatchesCandidate(params.Fixed, *fixedCandidate, provider) ||
		params.Fixed.PageID != job.Target.PageID || params.Fixed.RevisionID != job.Target.RevisionID ||
		params.Fixed.SHA1 != job.Target.ExpectedSHA1 {
		return model.LyricsSourceReviewItem{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeSourceDrift, nil)
	}
	identity, err := loadCatalogMusicIdentityContext(ctx, tx, job.Target.MusicID)
	if err != nil || identity.CatalogFingerprint != job.Target.CatalogFingerprint {
		return model.LyricsSourceReviewItem{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeSourceDrift, err)
	}
	if err := lyricscontract.ValidateFixedPerformerSegmentationPolicy(lyricscontract.CatalogIdentity{
		MusicID: identity.MusicID, JapaneseTitle: identity.JapaneseTitle, ProducerMetadata: identity.ProducerMetadata,
		Lyricist: identity.Lyricist, Composer: identity.Composer, Arranger: identity.Arranger,
		Vocals: append([]model.CatalogVocalSignal{}, identity.Vocals...), CatalogFingerprint: identity.CatalogFingerprint,
	}, params.Fixed); err != nil {
		return model.LyricsSourceReviewItem{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, err)
	}
	targets, err := catalogLyricsTargetsContext(ctx, tx)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	currentTarget, ok := lyricsSourceTargetForMusic(targets, job.Target.MusicID)
	if !ok || currentTarget.Disposition != model.LyricsCatalogTargetFullTarget {
		return model.LyricsSourceReviewItem{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeSourceDrift, nil)
	}
	artifact, err := canonicalLyricsSourceArtifact(job, params.Fixed, completedAt)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	artifactProvenanceStatus, err := validateFixedRevisionProvenance(params.Fixed, provider)
	if err != nil {
		return model.LyricsSourceReviewItem{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, err)
	}
	artifact, _, err = insertOrVerifyLyricsSourceArtifactTx(ctx, tx, artifact, provider, artifactProvenanceStatus)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	if artifactProvenanceStatus == "complete" {
		if err := insertOrVerifyLyricsSourceRenditionsTx(ctx, tx, artifact.ArtifactID, params.Fixed.FixedIdentities, completedAt); err != nil {
			return model.LyricsSourceReviewItem{}, err
		}
	}
	analysis, err := canonicalLyricsSourceAnalysis(job, artifact, params.Fixed, params.Evidence, completedAt)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	analysis, _, err = insertOrVerifyLyricsSourceAnalysisTx(ctx, tx, analysis, provider)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	associations, err := completeLyricsSourceAssociations(ctx, tx, analysis, params.Associations)
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	if err := insertOrVerifyLyricsSourceAssociationsTx(ctx, tx, analysis, associations, completedAt); err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	reviewEvidence, err := json.Marshal(map[string]any{
		"analysisId": analysis.AnalysisID, "artifactId": artifact.ArtifactID,
		"matchingEvidence": analysis.MatchingEvidence,
	})
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	review, _, err := createLyricsSourceReviewTx(ctx, tx, createLyricsSourceReviewParams{
		Provider: provider, Kind: "artifact_review", AnalysisID: analysis.AnalysisID, MusicID: job.Target.MusicID,
		CatalogFingerprint: job.Target.CatalogFingerprint, ReasonCode: "machine_analysis_ready",
		EvidenceJSON: reviewEvidence, Priority: 0, CreatedAt: completedAt,
	})
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_discovery_job_outputs(job_id, artifact_id, analysis_id, review_id, created_at, provider)
		VALUES (?, ?, ?, ?, ?, ?)`, job.ID, artifact.ArtifactID, analysis.AnalysisID, review.ReviewID, completedAt.UnixMilli(), provider); err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE lyrics_discovery_jobs SET state='succeeded', next_attempt_at=?,
		lease_owner=NULL, lease_expires_at=NULL, last_error_code=NULL, updated_at=?, completed_at=?, version=version+1
		WHERE job_id=? AND state='leased' AND lease_owner=? AND version=? AND lease_expires_at>?`,
		completedAt.UnixMilli(), completedAt.UnixMilli(), completedAt.UnixMilli(), job.ID, owner,
		params.ExpectedVersion, leaseCheckedAt.UnixMilli())
	if err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	if err := requireLyricsDiscoveryJobTransition(ctx, tx, result, job.ID); err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.LyricsSourceReviewItem{}, err
	}
	return review, nil
}

func fixedRevisionMatchesCandidate(fixed lyricssource.FixedRevision, candidate lyricssource.Candidate, provider model.LyricsSourceProvider) bool {
	fixedProvider := fixed.Provider
	if fixedProvider == "" {
		fixedProvider = model.LyricsSourceProviderVocaloidFandom
	}
	fixedOrigin := fixed.Origin
	if fixedOrigin == "" && fixedProvider == model.LyricsSourceProviderVocaloidFandom {
		fixedOrigin = model.LyricsSourceOriginVocaloidFandom
	}
	candidateProvider := candidate.Provider
	if candidateProvider == "" {
		candidateProvider = model.LyricsSourceProviderVocaloidFandom
	}
	candidateOrigin := candidate.Origin
	if candidateOrigin == "" && candidateProvider == model.LyricsSourceProviderVocaloidFandom {
		candidateOrigin = model.LyricsSourceOriginVocaloidFandom
	}
	if provider != candidateProvider || fixedProvider != candidateProvider || fixedOrigin != candidateOrigin ||
		fixed.PageID != candidate.PageID || fixed.RevisionID != candidate.RevisionID || fixed.SHA1 != candidate.SHA1 ||
		fixed.PageTitle != candidate.Title || fixed.CanonicalURL != candidate.CanonicalURL ||
		!sameStringSlice(fixed.Categories, candidate.Categories) {
		return false
	}
	if !lyricsDiscoveryCandidateIsProviderAware(candidate) {
		return true
	}
	if fixed.Section != candidate.Section || fixed.RenditionKey != candidate.RenditionKey ||
		fixed.VersionReason != candidate.VersionReason || !sameIndexEvidenceRefs(fixed.IndexEvidenceRefs, candidate.IndexEvidenceRefs) ||
		!sameLyricsIndexEvidenceCollection(fixed.IndexEvidence, candidate.IndexEvidence) {
		return false
	}
	resolved := stripLyricsCandidateIndexEvidence(candidate)
	resolved.IndexEvidence = cloneLyricsIndexEvidenceSlice(fixed.IndexEvidence)
	return lyricssource.ValidateCandidateIndexEvidence(resolved) == nil
}

func canonicalLyricsSourceArtifact(job model.LyricsDiscoveryJob, fixed lyricssource.FixedRevision, completedAt time.Time) (model.LyricsSourceArtifact, error) {
	provider, origin, err := canonicalFixedRevisionProvider(fixed)
	if err != nil || fixed.PageID <= 0 || fixed.RevisionID <= 0 || !lyricssource.HasCanonicalSHA1(fixed.SHA1) ||
		len(fixed.Wikitext) == 0 || len(fixed.Wikitext) > maxLyricsSourceRawBytes || !utf8.Valid(fixed.Wikitext) {
		return model.LyricsSourceArtifact{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, errors.New("invalid fixed revision"))
	}
	if err := validateProviderCanonicalLyricsSourceURL(provider, origin, fixed.CanonicalURL, fixed.PageTitle, fixed.RevisionID); err != nil {
		return model.LyricsSourceArtifact{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, errors.New("invalid canonical source URL"))
	}
	categories, err := canonicalLyricsSourceStringSet(fixed.Categories, maxLyricsSourceCategories, maxLyricsSourceCategoryBytes)
	if err != nil {
		return model.LyricsSourceArtifact{}, err
	}
	rawDigest := sha256.Sum256(fixed.Wikitext)
	rawSHA := hex.EncodeToString(rawDigest[:])
	envelope := lyricsSourceArtifactEnvelope{
		Version: 1, SourceType: "mediawiki", SourceOrigin: origin,
		PageID: fixed.PageID, RevisionID: fixed.RevisionID, PageTitle: strings.TrimSpace(fixed.PageTitle),
		CanonicalURL: fixed.CanonicalURL, MediaWikiSHA1: fixed.SHA1, Categories: categories, RawSHA256: rawSHA,
	}
	payload, _ := json.Marshal(envelope)
	digest := sha256.Sum256(payload)
	return model.LyricsSourceArtifact{
		SourceType: "mediawiki", SourceOrigin: origin, PageID: fixed.PageID,
		RevisionID: fixed.RevisionID, PageTitle: envelope.PageTitle, CanonicalRevisionURL: fixed.CanonicalURL,
		MediaWikiSHA1: fixed.SHA1, Categories: categories, RawWikitext: append([]byte(nil), fixed.Wikitext...),
		RawWikitextSHA256: rawSHA, ArtifactSHA256: hex.EncodeToString(digest[:]), FirstFetchedAt: fixed.FetchedAt.UTC(),
		FirstCreatingJobID: job.ID, CreatedAt: completedAt,
	}, nil
}

func canonicalFixedRevisionProvider(fixed lyricssource.FixedRevision) (model.LyricsSourceProvider, string, error) {
	provider := fixed.Provider
	if provider == "" {
		provider = model.LyricsSourceProviderVocaloidFandom
	}
	origin := fixed.Origin
	if origin == "" && provider == model.LyricsSourceProviderVocaloidFandom {
		origin = model.LyricsSourceOriginVocaloidFandom
	}
	wantOrigin := model.LyricsSourceOriginVocaloidFandom
	if provider == model.LyricsSourceProviderMoegirl {
		wantOrigin = model.LyricsSourceOriginMoegirl
	}
	if !model.IsValidLyricsSourceProvider(provider) || origin != wantOrigin {
		return "", "", errors.New("fixed revision provider identity is invalid")
	}
	return provider, origin, nil
}

func validateFixedRevisionProvenance(fixed lyricssource.FixedRevision, provider model.LyricsSourceProvider) (string, error) {
	if len(fixed.FixedIdentities) == 0 {
		if provider != model.LyricsSourceProviderVocaloidFandom || fixed.Document != nil {
			return "", errors.New("provider-aware fixed revision is missing fixed identities")
		}
		return "rebuild_required", nil
	}
	fetchedAt := fixed.FetchedAt.UTC().Format(time.RFC3339Nano)
	matchedTopLevel := false
	for _, identity := range fixed.FixedIdentities {
		if err := model.ValidateLyricsSourceFixedIdentity(identity); err != nil || identity.Provider != provider ||
			identity.Origin != fixed.Origin || identity.PageID != fixed.PageID || identity.RevisionID != fixed.RevisionID ||
			identity.SHA1 != fixed.SHA1 || identity.Title != fixed.PageTitle || identity.CanonicalURL != fixed.CanonicalURL ||
			identity.FetchedAt != fetchedAt || !sameStringSlice(identity.Categories, fixed.Categories) {
			return "", errors.New("fixed revision contains inconsistent provider identities")
		}
		if identity.Section == fixed.Section && identity.RenditionKey == fixed.RenditionKey &&
			sameIndexEvidenceRefs(identity.IndexEvidenceRefs, fixed.IndexEvidenceRefs) {
			matchedTopLevel = true
		}
	}
	if !matchedTopLevel {
		return "", errors.New("fixed revision top-level rendition is not represented by its fixed identities")
	}
	if fixed.Document != nil {
		if err := model.ValidateLyricsSourceDocument(*fixed.Document); err != nil {
			return "", err
		}
		fixedJSON, _ := json.Marshal(fixed.FixedIdentities)
		documentJSON, _ := json.Marshal(fixed.Document.FixedIdentities)
		if !bytes.Equal(fixedJSON, documentJSON) {
			return "", errors.New("fixed revision document identities drifted from fetched identities")
		}
	}
	return "complete", nil
}

func insertOrVerifyLyricsSourceRenditionsTx(ctx context.Context, tx *sql.Tx, artifactID int64, identities []model.LyricsSourceFixedIdentity, createdAt time.Time) error {
	for _, identity := range identities {
		identityJSON, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		identityDigest := sha256.Sum256(identityJSON)
		identitySHA := hex.EncodeToString(identityDigest[:])
		categoriesJSON, _ := json.Marshal(identity.Categories)
		evidenceJSON, _ := json.Marshal(identity.IndexEvidenceRefs)
		if _, err := tx.ExecContext(ctx, `INSERT INTO lyrics_source_renditions
			(provider,artifact_id,origin,page_id,revision_id,mediawiki_sha1,page_title,canonical_revision_url,
			 fetched_at,categories_json,section,rendition_key,index_evidence_refs_json,fixed_identity_json,
			 fixed_identity_sha256,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(fixed_identity_sha256) DO NOTHING`, identity.Provider, artifactID, identity.Origin,
			identity.PageID, identity.RevisionID, identity.SHA1, identity.Title, identity.CanonicalURL,
			identity.FetchedAt, string(categoriesJSON), identity.Section, identity.RenditionKey, string(evidenceJSON),
			string(identityJSON), identitySHA, createdAt.UnixMilli()); err != nil {
			return err
		}
		var renditionID, storedArtifactID int64
		var storedJSON string
		if err := tx.QueryRowContext(ctx, `SELECT rendition_id,artifact_id,fixed_identity_json FROM lyrics_source_renditions
			WHERE fixed_identity_sha256=?`, identitySHA).Scan(&renditionID, &storedArtifactID, &storedJSON); err != nil {
			return err
		}
		if storedArtifactID != artifactID || storedJSON != string(identityJSON) {
			return ErrLyricsSourceArtifactConflict
		}
		if err := insertLyricsSourceRenditionEvidenceLinksTx(ctx, tx, renditionID, identity); err != nil {
			return err
		}
	}
	return nil
}

func canonicalLyricsSourceAnalysis(job model.LyricsDiscoveryJob, artifact model.LyricsSourceArtifact, fixed lyricssource.FixedRevision, evidence []model.LyricsSourceEvidence, completedAt time.Time) (model.LyricsSourceAnalysis, error) {
	var structuredErr error
	fixed, structuredErr = lyricssource.EnsureStructuredExtraction(fixed)
	if structuredErr != nil {
		return model.LyricsSourceAnalysis{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, structuredErr)
	}
	if len(evidence) == 0 || len(evidence) > maxLyricsSourceEvidence {
		return model.LyricsSourceAnalysis{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, errors.New("invalid matching evidence"))
	}
	for _, item := range evidence {
		if strings.TrimSpace(item.RuleID) == "" || strings.TrimSpace(item.Gate) == "" ||
			strings.TrimSpace(item.Outcome) == "" || len(item.Summary) > maxLyricsSourceEvidenceBytes {
			return model.LyricsSourceAnalysis{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, errors.New("invalid matching evidence item"))
		}
	}
	if len(fixed.Extraction.Lines) == 0 || fixed.Extraction.Version.Kind == "" || strings.TrimSpace(fixed.Extraction.Version.Label) == "" ||
		strings.TrimSpace(fixed.Extraction.RubyGeneratorVersion) == "" {
		return model.LyricsSourceAnalysis{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, errors.New("fixed revision is missing structured extraction evidence"))
	}
	lines := make([]model.LyricsSourceExtractedLine, len(fixed.Extraction.Lines))
	for index, line := range fixed.Extraction.Lines {
		segments := make([]model.LyricsSourceSegment, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			ruby := make([]model.LyricsSourceRubySpan, len(segment.Ruby))
			for rubyIndex, span := range segment.Ruby {
				ruby[rubyIndex] = model.LyricsSourceRubySpan{Text: span.Text, Reading: span.Reading}
			}
			segments[segmentIndex] = model.LyricsSourceSegment{Text: segment.Text,
				PerformerIDs: append([]string{}, segment.PerformerIDs...), Ruby: ruby}
		}
		lines[index] = model.LyricsSourceExtractedLine{Japanese: line.Japanese, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments: segments, TrailingPerformerIDs: append([]string{}, line.TrailingPerformerIDs...)}
	}
	performers := make([]model.LyricsSourcePerformer, len(fixed.Extraction.Performers))
	for index, performer := range fixed.Extraction.Performers {
		performers[index] = model.LyricsSourcePerformer{PerformerID: performer.PerformerID, Name: performer.Name, Color: performer.Color}
	}
	selectedVersion := model.LyricsSourceVersion{Kind: fixed.Extraction.Version.Kind, Label: fixed.Extraction.Version.Label}
	performers, rubyGeneratorVersion, lines, err := canonicalizeLyricsSourceAnalysisMetadata(
		selectedVersion, performers, fixed.Extraction.RubyGeneratorVersion, lines,
	)
	if err != nil {
		return model.LyricsSourceAnalysis{}, lyricsdiscovery.NewError(lyricsdiscovery.CodeInvalidResult, err)
	}
	linesSHA := model.LyricsSourceExtractedLinesSHA256(lines)
	envelope := lyricsSourceAnalysisEnvelope{
		Version: 2, ArtifactSHA256: artifact.ArtifactSHA256, MusicID: job.Target.MusicID,
		CatalogFingerprint: job.Target.CatalogFingerprint, MatchingPolicyVersion: model.LyricsMatchingPolicyVersion,
		RestrictionPolicyVersion: model.LyricsRestrictionPolicyVersion, ExtractorVersion: model.LyricsExtractorVersion,
		MatchOutcome: "matched", RestrictionOutcome: "clear", ExtractionOutcome: "extracted",
		MatchingEvidence: evidence, RestrictionRuleIDs: []string{}, SelectedVersion: selectedVersion,
		Performers: performers, RubyGeneratorVersion: rubyGeneratorVersion,
		ExtractedLines: lines, ExtractedLinesSHA256: linesSHA,
	}
	payload, _ := json.Marshal(envelope)
	digest := sha256.Sum256(payload)
	keyPayload := fmt.Sprintf("v1\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s", artifact.ArtifactID, job.Target.MusicID,
		job.Target.CatalogFingerprint, model.LyricsMatchingPolicyVersion, model.LyricsRestrictionPolicyVersion, model.LyricsExtractorVersion)
	keyDigest := sha256.Sum256([]byte(keyPayload))
	return model.LyricsSourceAnalysis{
		AnalysisKey: hex.EncodeToString(keyDigest[:]), ArtifactID: artifact.ArtifactID, MusicID: job.Target.MusicID,
		CatalogFingerprint: job.Target.CatalogFingerprint, MatchingPolicyVersion: model.LyricsMatchingPolicyVersion,
		RestrictionPolicyVersion: model.LyricsRestrictionPolicyVersion, ExtractorVersion: model.LyricsExtractorVersion,
		MatchOutcome: "matched", RestrictionOutcome: "clear", ExtractionOutcome: "extracted",
		MatchingEvidence: append([]model.LyricsSourceEvidence(nil), evidence...), RestrictionRuleIDs: []string{},
		SelectedVersion: selectedVersion, Performers: performers, RubyGeneratorVersion: rubyGeneratorVersion,
		ExtractedLines: lines, ExtractedLinesSHA256: linesSHA, AnalysisSHA256: hex.EncodeToString(digest[:]),
		CreatingJobID: job.ID, CreatedAt: completedAt,
	}, nil
}

func canonicalizeLyricsSourceAnalysisMetadata(
	version model.LyricsSourceVersion,
	performers []model.LyricsSourcePerformer,
	rubyGeneratorVersion string,
	lines []model.LyricsSourceExtractedLine,
) ([]model.LyricsSourcePerformer, string, []model.LyricsSourceExtractedLine, error) {
	full := model.NewLyricsSourceFullFromLegacy(version, performers, rubyGeneratorVersion, lines)
	canonicalFull, err := lyricscontract.NormalizePersistedPerformerMetadata(full)
	if err != nil {
		return nil, "", nil, errors.New("unsafe persisted lyrics source performer metadata")
	}
	canonicalRubyVersion, err := lyricssource.RecoveryPersistedRubyGeneratorVersion(canonicalFull.RubyGeneratorVersion)
	if err != nil {
		return nil, "", nil, errors.New("unsafe persisted lyrics source ruby generator metadata")
	}
	canonicalFull.RubyGeneratorVersion = canonicalRubyVersion
	return append([]model.LyricsSourcePerformer{}, canonicalFull.Performers...), canonicalRubyVersion,
		canonicalFull.LegacyExtractedLines(), nil
}

func insertOrVerifyLyricsSourceArtifactTx(ctx context.Context, tx *sql.Tx, artifact model.LyricsSourceArtifact, provider model.LyricsSourceProvider, provenanceStatus string) (model.LyricsSourceArtifact, bool, error) {
	if !model.IsValidLyricsSourceProvider(provider) || (provenanceStatus != "complete" && provenanceStatus != "rebuild_required") {
		return model.LyricsSourceArtifact{}, false, errors.New("invalid lyrics source artifact provenance")
	}
	categoriesJSON, _ := json.Marshal(artifact.Categories)
	result, err := tx.ExecContext(ctx, `INSERT INTO lyrics_source_artifacts
		(source_type, source_origin, page_id, revision_id, page_title, canonical_revision_url, mediawiki_sha1,
		 categories_json, raw_wikitext, raw_byte_count, raw_wikitext_sha256, artifact_sha256,
		 first_fetched_at, first_creating_job_id, created_at, provider, provenance_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_type, source_origin, page_id, revision_id) DO NOTHING`,
		artifact.SourceType, artifact.SourceOrigin, artifact.PageID, artifact.RevisionID, artifact.PageTitle,
		artifact.CanonicalRevisionURL, artifact.MediaWikiSHA1, string(categoriesJSON), artifact.RawWikitext,
		len(artifact.RawWikitext), artifact.RawWikitextSHA256, artifact.ArtifactSHA256,
		artifact.FirstFetchedAt.UnixMilli(), artifact.FirstCreatingJobID, artifact.CreatedAt.UnixMilli(), provider, provenanceStatus)
	if err != nil {
		return model.LyricsSourceArtifact{}, false, err
	}
	created, _ := result.RowsAffected()
	var stored model.LyricsSourceArtifact
	var raw []byte
	var categories string
	var storedProvider model.LyricsSourceProvider
	var storedProvenanceStatus string
	var fetchedAt, createdAt int64
	err = tx.QueryRowContext(ctx, `SELECT artifact_id, source_type, source_origin, page_id, revision_id, page_title,
		canonical_revision_url, mediawiki_sha1, categories_json, raw_wikitext, raw_wikitext_sha256,
		artifact_sha256, first_fetched_at, first_creating_job_id, created_at, provider, provenance_status FROM lyrics_source_artifacts
		WHERE source_type=? AND source_origin=? AND page_id=? AND revision_id=?`, artifact.SourceType,
		artifact.SourceOrigin, artifact.PageID, artifact.RevisionID).Scan(&stored.ArtifactID, &stored.SourceType,
		&stored.SourceOrigin, &stored.PageID, &stored.RevisionID, &stored.PageTitle, &stored.CanonicalRevisionURL,
		&stored.MediaWikiSHA1, &categories, &raw, &stored.RawWikitextSHA256, &stored.ArtifactSHA256,
		&fetchedAt, &stored.FirstCreatingJobID, &createdAt, &storedProvider, &storedProvenanceStatus)
	if err != nil {
		return model.LyricsSourceArtifact{}, false, err
	}
	if err := json.Unmarshal([]byte(categories), &stored.Categories); err != nil {
		return model.LyricsSourceArtifact{}, false, err
	}
	stored.RawWikitext = raw
	stored.FirstFetchedAt = time.UnixMilli(fetchedAt).UTC()
	stored.CreatedAt = time.UnixMilli(createdAt).UTC()
	if storedProvider != provider || storedProvenanceStatus != provenanceStatus ||
		stored.ArtifactSHA256 != artifact.ArtifactSHA256 || stored.RawWikitextSHA256 != artifact.RawWikitextSHA256 ||
		!bytes.Equal(stored.RawWikitext, artifact.RawWikitext) {
		return model.LyricsSourceArtifact{}, false, ErrLyricsSourceArtifactConflict
	}
	return stored, created == 1, nil
}

func insertOrVerifyLyricsSourceAnalysisTx(ctx context.Context, tx *sql.Tx, analysis model.LyricsSourceAnalysis, provider model.LyricsSourceProvider) (model.LyricsSourceAnalysis, bool, error) {
	if !model.IsValidLyricsSourceProvider(provider) {
		return model.LyricsSourceAnalysis{}, false, errors.New("invalid lyrics source analysis provider")
	}
	canonicalPerformers, canonicalRubyVersion, canonicalLines, err := canonicalizeLyricsSourceAnalysisMetadata(
		analysis.SelectedVersion, analysis.Performers, analysis.RubyGeneratorVersion, analysis.ExtractedLines,
	)
	if err != nil || !reflect.DeepEqual(canonicalPerformers, analysis.Performers) ||
		canonicalRubyVersion != analysis.RubyGeneratorVersion || !reflect.DeepEqual(canonicalLines, analysis.ExtractedLines) ||
		model.LyricsSourceExtractedLinesSHA256(analysis.ExtractedLines) != analysis.ExtractedLinesSHA256 {
		return model.LyricsSourceAnalysis{}, false, errors.New("unsafe persisted lyrics source analysis metadata")
	}
	evidenceJSON, _ := json.Marshal(analysis.MatchingEvidence)
	restrictionJSON, _ := json.Marshal(analysis.RestrictionRuleIDs)
	selectedVersionJSON, _ := json.Marshal(analysis.SelectedVersion)
	performersJSON, _ := json.Marshal(analysis.Performers)
	linesJSON, _ := json.Marshal(analysis.ExtractedLines)
	result, err := tx.ExecContext(ctx, `INSERT INTO lyrics_source_analyses
		(analysis_key, artifact_id, music_id, catalog_fingerprint, matching_policy_version,
		 restriction_policy_version, extractor_version, match_outcome, restriction_outcome,
		 extraction_outcome, matching_evidence_json, restriction_rule_ids_json, selected_version_json,
		 performers_json, ruby_generator_version, extracted_lines_json, extracted_line_count,
		 extracted_lines_sha256, analysis_sha256, creating_job_id, created_at, provider)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(analysis_key) DO NOTHING`, analysis.AnalysisKey, analysis.ArtifactID, analysis.MusicID,
		analysis.CatalogFingerprint, analysis.MatchingPolicyVersion, analysis.RestrictionPolicyVersion,
		analysis.ExtractorVersion, analysis.MatchOutcome, analysis.RestrictionOutcome, analysis.ExtractionOutcome,
		string(evidenceJSON), string(restrictionJSON), string(selectedVersionJSON), string(performersJSON),
		analysis.RubyGeneratorVersion, string(linesJSON), len(analysis.ExtractedLines),
		analysis.ExtractedLinesSHA256, analysis.AnalysisSHA256, analysis.CreatingJobID, analysis.CreatedAt.UnixMilli(), provider)
	if err != nil {
		return model.LyricsSourceAnalysis{}, false, err
	}
	created, _ := result.RowsAffected()
	var storedID int64
	var storedHash string
	var storedProvider model.LyricsSourceProvider
	if err := tx.QueryRowContext(ctx, `SELECT analysis_id, analysis_sha256, provider FROM lyrics_source_analyses WHERE analysis_key=?`, analysis.AnalysisKey).Scan(&storedID, &storedHash, &storedProvider); err != nil {
		return model.LyricsSourceAnalysis{}, false, err
	}
	if storedProvider != provider || storedHash != analysis.AnalysisSHA256 {
		return model.LyricsSourceAnalysis{}, false, ErrLyricsSourceArtifactConflict
	}
	analysis.AnalysisID = storedID
	return analysis, created == 1, nil
}

type catalogLyricsTargetsQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func catalogLyricsTargetsContext(ctx context.Context, query catalogLyricsTargetsQuery) ([]model.CatalogLyricsTarget, error) {
	rows, err := query.QueryContext(ctx, `SELECT music_id, title_ja, lyricist, composer, arranger, assetbundle_name,
		version_hint, lyrics_version, lyrics_evidence_presence_json, vocal_signals_json, lyrics_catalog_fingerprint
		FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []model.CatalogLyricsGroupingRecord{}
	for rows.Next() {
		var record model.CatalogLyricsGroupingRecord
		var title, presenceJSON, vocalsJSON string
		if err := rows.Scan(&record.MusicID, &title, &record.Evidence.Lyricist, &record.Evidence.Composer,
			&record.Evidence.Arranger, &record.Evidence.Assetbundle, &record.Evidence.VersionHint,
			&record.Evidence.LyricsVersion, &presenceJSON, &vocalsJSON, &record.Fingerprint); err != nil {
			return nil, err
		}
		record.Evidence.Title = title
		if err := json.Unmarshal([]byte(presenceJSON), &record.Evidence.Presence); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(vocalsJSON), &record.Evidence.Vocals); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return model.ClassifyCatalogLyricsTargets(records), nil
}

func lyricsSourceTargetForMusic(targets []model.CatalogLyricsTarget, musicID int) (model.CatalogLyricsTarget, bool) {
	for _, target := range targets {
		if target.MusicID == musicID {
			return target, true
		}
	}
	return model.CatalogLyricsTarget{}, false
}

func completeLyricsSourceAssociations(ctx context.Context, tx *sql.Tx, analysis model.LyricsSourceAnalysis, supplied []model.LyricsSourceAssociation) ([]model.LyricsSourceAssociation, error) {
	if len(supplied) > 0 {
		return supplied, nil
	}
	targets, err := catalogLyricsTargetsContext(ctx, tx)
	if err != nil {
		return nil, err
	}
	associations := []model.LyricsSourceAssociation{}
	for _, target := range targets {
		if target.TargetMusicID != analysis.MusicID {
			continue
		}
		kind := "game_size_evidence"
		if target.MusicID == analysis.MusicID {
			kind = "full_target"
		}
		associations = append(associations, model.LyricsSourceAssociation{MusicID: target.MusicID,
			CatalogFingerprint: target.CatalogFingerprint, Kind: kind})
	}
	if len(associations) == 0 {
		return nil, lyricsdiscovery.NewError(lyricsdiscovery.CodeSourceDrift, errors.New("catalog grouping no longer has an explicit full target"))
	}
	return associations, nil
}

func insertOrVerifyLyricsSourceAssociationsTx(ctx context.Context, tx *sql.Tx, analysis model.LyricsSourceAnalysis, associations []model.LyricsSourceAssociation, createdAt time.Time) error {
	seen := map[string]struct{}{}
	fullTargets := 0
	for _, association := range associations {
		key := fmt.Sprintf("%d/%s", association.MusicID, association.Kind)
		if _, exists := seen[key]; exists || association.MusicID <= 0 || !lyricsDiscoveryFingerprintPattern.MatchString(association.CatalogFingerprint) {
			return errors.New("invalid lyrics source association")
		}
		seen[key] = struct{}{}
		if association.Kind == "full_target" {
			fullTargets++
		} else if association.Kind != "game_size_evidence" {
			return errors.New("invalid lyrics source association kind")
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO lyrics_source_associations
			(analysis_id, music_id, catalog_fingerprint, kind, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(analysis_id, music_id, kind) DO NOTHING`, analysis.AnalysisID, association.MusicID,
			association.CatalogFingerprint, association.Kind, createdAt.UnixMilli())
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			var storedFingerprint string
			if err := tx.QueryRowContext(ctx, `SELECT catalog_fingerprint FROM lyrics_source_associations
				WHERE analysis_id=? AND music_id=? AND kind=?`, analysis.AnalysisID, association.MusicID,
				association.Kind).Scan(&storedFingerprint); err != nil || storedFingerprint != association.CatalogFingerprint {
				return ErrLyricsSourceArtifactConflict
			}
		}
	}
	if fullTargets != 1 {
		return errors.New("lyrics source associations require exactly one full target")
	}
	return nil
}

func canonicalLyricsSourceStringSet(values []string, maximum, maxBytes int) ([]string, error) {
	if len(values) > maximum {
		return nil, errors.New("source string set exceeds safe limit")
	}
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
			return nil, errors.New("source string set contains invalid value")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}
