package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/lyricssource"
)

const (
	lyricsImportTokenTTL  = 10 * time.Minute
	maxLyricsImportTokens = 256
)

var (
	errLyricsImportGrantInvalid = errors.New("lyrics import grant is invalid")
	errLyricsImportGrantBusy    = errors.New("lyrics import grant is already claimed")
	errLyricsImportCapacity     = errors.New("lyrics import grant capacity exhausted")
)

type lyricsImportGrant struct {
	user            string
	musicID         int
	preview         lyricssource.Preview
	catalogIdentity lyricssource.MusicIdentity
	producerStatus  editorgate.Status
	expiresAt       time.Time
	claimID         string
}

type lyricsImportClaim struct {
	token   string
	claimID string
	preview lyricssource.Preview
}

func randomCapabilityID() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

func producerStatusStopped(status editorgate.Status) bool {
	return status.InstanceID != "" && !status.Running && status.Generation == status.CompletedGeneration
}

func sameProducerStatus(left, right editorgate.Status) bool {
	return left == right
}

func (s *Server) issueLyricsImportGrant(user string, musicID int, preview lyricssource.Preview, producerStatus editorgate.Status, identities ...lyricssource.MusicIdentity) (string, error) {
	var catalogIdentity lyricssource.MusicIdentity
	if len(identities) > 0 {
		catalogIdentity = identities[0]
	}
	if musicID <= 0 || preview.PageID <= 0 || preview.RevisionID <= 0 || !lyricssource.HasCanonicalSHA1(preview.SHA1) || !producerStatusStopped(producerStatus) ||
		(catalogIdentity.MusicID != 0 && catalogIdentity.MusicID != musicID) {
		return "", lyricssource.ErrMalformedResponse
	}
	token, err := randomCapabilityID()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.lyricsImportMu.Lock()
	defer s.lyricsImportMu.Unlock()
	for existing, grant := range s.lyricsImports {
		if grant.claimID == "" && !grant.expiresAt.After(now) {
			delete(s.lyricsImports, existing)
		}
	}
	if len(s.lyricsImports) >= maxLyricsImportTokens {
		var oldestToken string
		var oldestExpiry time.Time
		for existing, grant := range s.lyricsImports {
			if grant.claimID != "" {
				continue
			}
			if oldestExpiry.IsZero() || grant.expiresAt.Before(oldestExpiry) {
				oldestToken, oldestExpiry = existing, grant.expiresAt
			}
		}
		if oldestToken == "" {
			return "", errLyricsImportCapacity
		}
		delete(s.lyricsImports, oldestToken)
	}
	s.lyricsImports[token] = lyricsImportGrant{
		user: user, musicID: musicID, preview: preview, catalogIdentity: catalogIdentity, producerStatus: producerStatus,
		expiresAt: now.Add(lyricsImportTokenTTL),
	}
	return token, nil
}

// claimLyricsImportGrant atomically marks a capability in flight. Terminal
// identity, expiry, or producer-state failures consume the capability; a busy
// claim remains present so exactly one concurrent request can own it.
func (s *Server) claimLyricsImportGrant(token, user string, musicID int, producerStatus editorgate.Status) (lyricsImportClaim, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return lyricsImportClaim{}, errLyricsImportGrantInvalid
	}
	s.lyricsImportMu.Lock()
	defer s.lyricsImportMu.Unlock()
	grant, ok := s.lyricsImports[token]
	if !ok {
		return lyricsImportClaim{}, errLyricsImportGrantInvalid
	}
	if grant.claimID != "" {
		return lyricsImportClaim{}, errLyricsImportGrantBusy
	}
	if !grant.expiresAt.After(time.Now()) || grant.user != user || grant.musicID != musicID ||
		!sameProducerStatus(grant.producerStatus, producerStatus) {
		delete(s.lyricsImports, token)
		return lyricsImportClaim{}, errLyricsImportGrantInvalid
	}
	claimID, err := randomCapabilityID()
	if err != nil {
		return lyricsImportClaim{}, err
	}
	grant.claimID = claimID
	s.lyricsImports[token] = grant
	return lyricsImportClaim{token: token, claimID: claimID, preview: grant.preview}, nil
}

func (s *Server) lyricsImportGrantCatalogCurrent(claim lyricsImportClaim) (bool, bool, error) {
	s.lyricsImportMu.Lock()
	grant, ok := s.lyricsImports[claim.token]
	s.lyricsImportMu.Unlock()
	if !ok || grant.claimID != claim.claimID || claim.claimID == "" {
		return false, false, errLyricsImportGrantInvalid
	}
	if grant.catalogIdentity.MusicID == 0 {
		return true, false, nil
	}
	current, err := s.store.CatalogMusicIdentity(grant.musicID)
	if err == sql.ErrNoRows {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	// Compare against the same full identity shape the preview captured; a
	// partial struct would treat every credited song as permanently drifted.
	return grant.catalogIdentity == (lyricssource.MusicIdentity{
		MusicID: current.MusicID, JapaneseTitle: current.JapaneseTitle, ProducerMetadata: current.ProducerMetadata,
		Lyricist: current.Lyricist, Composer: current.Composer, Arranger: current.Arranger,
		PerformerSegmentationPolicy: lyricssource.PerformerSegmentationPolicyFromCatalogVocals(current.Vocals),
	}), true, nil
}

func (s *Server) commitLyricsImportGrant(claim lyricsImportClaim) bool {
	s.lyricsImportMu.Lock()
	defer s.lyricsImportMu.Unlock()
	grant, ok := s.lyricsImports[claim.token]
	if !ok || grant.claimID != claim.claimID || claim.claimID == "" {
		return false
	}
	delete(s.lyricsImports, claim.token)
	return true
}

// invalidateLyricsImportGrant is a fail-closed cleanup for an internal claim
// ownership invariant failure after a deterministic terminal outcome or durable
// save. Capability tokens are never reused, so deleting by token cannot target
// a later grant and prevents replay of an authorization whose DB write may have
// already committed.
func (s *Server) invalidateLyricsImportGrant(token string) {
	s.lyricsImportMu.Lock()
	delete(s.lyricsImports, token)
	s.lyricsImportMu.Unlock()
}

func (s *Server) releaseLyricsImportGrant(claim lyricsImportClaim) bool {
	s.lyricsImportMu.Lock()
	defer s.lyricsImportMu.Unlock()
	grant, ok := s.lyricsImports[claim.token]
	if !ok || grant.claimID != claim.claimID || claim.claimID == "" {
		return false
	}
	grant.claimID = ""
	s.lyricsImports[claim.token] = grant
	return true
}
