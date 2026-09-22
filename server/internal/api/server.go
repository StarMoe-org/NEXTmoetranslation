// Package api wires the HTTP routes for the console (JWT-authenticated, no-cache)
// API. Public, cacheable file serving lives in package filesvc and is mounted
// separately under /files/*.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/auth"
	"moesekai/server/internal/backup"
	"moesekai/server/internal/collab"
	"moesekai/server/internal/config"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/searchindex"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
	"moesekai/server/internal/upstream"
)

const maxJSONBodyBytes = 8 << 20

type lyricsSourceClient interface {
	Search(context.Context, lyricssource.MusicIdentity) ([]lyricssource.Candidate, error)
	Preview(context.Context, lyricssource.MusicIdentity, int, int) (lyricssource.Preview, error)
}

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

// Server holds the dependencies shared by all console handlers.
type Server struct {
	store                   *store.Store
	eventStore              *store.EventStore
	auth                    *auth.Auth
	cfg                     *config.Config
	hub                     *sse.Hub
	translator              *translator.Translator
	upstream                *upstream.Watcher
	backup                  *backup.Manager
	lyricsSrc               lyricsSourceClient
	authAttempts            *authAttemptLimiter
	editorGate              *editorgate.Gate
	lyricsImportMu          sync.Mutex
	lyricsImports           map[string]lyricsImportGrant
	lyricsInvariantReporter func(string, ...any)
	projection              interface {
		Status() filesvc.ProjectionStatus
	}
	fileService interface {
		RebuildEvent(eventID int) error
		RebuildCategory(category string) error
		PublishNow()
		Status() filesvc.ProjectionStatus
		SongProvenance(musicID int) (filesvc.SongProvenance, bool)
	}
	search interface {
		Status() searchindex.Status
	}
	wsHub interface {
		Broadcast(event string, data any)
		BroadcastGateStatus()
		RevokeUser(user string)
	}
	collab *collab.Service
}

func (s *Server) SetWsHub(w interface {
	Broadcast(event string, data any)
	BroadcastGateStatus()
	RevokeUser(user string)
}) {
	s.wsHub = w
}

func (s *Server) SetCollab(service *collab.Service) {
	s.collab = service
}

func (s *Server) SetSearchStatus(provider interface {
	Status() searchindex.Status
}) {
	s.search = provider
}

// SetProjectionStatus connects the authenticated status API to the public file
// service without changing existing Server construction contracts.
func (s *Server) SetProjectionStatus(provider interface {
	Status() filesvc.ProjectionStatus
}) {
	s.projection = provider
	if fs, ok := provider.(interface {
		RebuildEvent(eventID int) error
		RebuildCategory(category string) error
		PublishNow()
		Status() filesvc.ProjectionStatus
		SongProvenance(musicID int) (filesvc.SongProvenance, bool)
	}); ok {
		s.fileService = fs
	}
}

func (s *Server) SetFileService(fs interface {
	RebuildEvent(eventID int) error
	RebuildCategory(category string) error
	PublishNow()
	Status() filesvc.ProjectionStatus
	SongProvenance(musicID int) (filesvc.SongProvenance, bool)
}) {
	s.fileService = fs
	s.projection = fs
}

func (s *Server) rebuildEventAsset(eventID int) {
	if s.fileService != nil {
		if err := s.fileService.RebuildEvent(eventID); err != nil {
			log.Printf("[filesvc] rebuild event %d failed: %v", eventID, err)
		}
	}
}

func (s *Server) rebuildCategoryAsset(category string) {
	if s.fileService != nil {
		if err := s.fileService.RebuildCategory(category); err != nil {
			log.Printf("[filesvc] rebuild category %s failed: %v", category, err)
		}
	}
}

func NewServer(s *store.Store, es *store.EventStore, a *auth.Auth, cfg *config.Config, hub *sse.Hub, tr *translator.Translator, up *upstream.Watcher, bk *backup.Manager, gates ...*editorgate.Gate) *Server {
	var gate *editorgate.Gate
	if len(gates) > 0 {
		gate = gates[0]
	}
	if gate == nil {
		gate = editorgate.MustNew()
	}
	if tr != nil {
		tr.SetEditorGate(gate)
	}
	if bk != nil {
		bk.SetEditorGate(gate)
	}
	var lyricsSrc lyricsSourceClient = lyricssource.New()
	// The online editor tries Sekaipedia first, then the legacy fallback
	// providers. The reviewed Sekaipedia authority is compiled in, together with
	// the reviewed music-ID-to-page-title and contributor-alias maps. Music 728
	// has a Sekaipedia page, but that revision's lyrics table is empty, so it
	// stays out of the compiled editor target-map.
	sekaipediaConfigs := []lyricssource.ProviderConfig{
		lyricssource.ReviewedSekaipediaProviderConfig(
			[]lyricssource.SekaipediaPageTarget{
				{MusicID: 50, PageTitle: "Blessing"},
				{MusicID: 69, PageTitle: "Fragile"},
				{MusicID: 95, PageTitle: "Ifuudoudou"},
				{MusicID: 131, PageTitle: "Hatsune Miku no Gekishou"},
				{MusicID: 138, PageTitle: "KING"},
				{MusicID: 141, PageTitle: "Gunjou Sanka"},
				{MusicID: 148, PageTitle: "ray", ResolvedPageTitle: "Ray"},
				{MusicID: 186, PageTitle: "Hatsune Tenchikaibyaku Shinwa"},
				{MusicID: 222, PageTitle: "Piano×Forte×Scandal"},
				{MusicID: 245, PageTitle: "Aun no Beats"},
				{MusicID: 295, PageTitle: "Float Planner"},
				{MusicID: 334, PageTitle: "Vampire's ∞ pathoS"},
				{MusicID: 353, PageTitle: "Kitty"},
				{MusicID: 386, PageTitle: "Kirapipi★Kirapika"},
				{MusicID: 402, PageTitle: "Envy Baby"},
				{MusicID: 475, PageTitle: "Chigau!!!"},
				{MusicID: 499, PageTitle: "Konton Boogie"},
				{MusicID: 515, PageTitle: "Igaku"},
				{MusicID: 555, PageTitle: "Fusion"},
				{MusicID: 560, PageTitle: "Eyelid"},
				{MusicID: 562, PageTitle: "Ángel"},
				{MusicID: 583, PageTitle: "Accelerate"},
				{MusicID: 592, PageTitle: "Queen of Hearts (song)"},
				{MusicID: 608, PageTitle: "Hoshizora Melancholia"},
				{MusicID: 621, PageTitle: "Tokyo Summer Session"},
				{MusicID: 635, PageTitle: "Ari no Mama no Story o"},
				{MusicID: 647, PageTitle: "SANchi Chokusou"},
				{MusicID: 649, PageTitle: "Sayonara Tengoku Mata Kite Jigoku"},
				{MusicID: 682, PageTitle: "Anata Shika Mienai no"},
				{MusicID: 692, PageTitle: "Vocalo-Colosseum"},
				{MusicID: 750, PageTitle: "Losstime Memory"},
				{MusicID: 751, PageTitle: "Additional Memory"},
				{MusicID: 752, PageTitle: "Ayano no Koufuku Riron"},
				{MusicID: 753, PageTitle: "Kuusou Forest"},
				{MusicID: 756, PageTitle: "Gimme more!"},
				{MusicID: 764, PageTitle: "Otsukimi Recital"},
				{MusicID: 789, PageTitle: "Tenbin, Yubisaki de Furete"},
			},
			[]lyricssource.ProviderContributorAlias{
				{MusicID: 69, CatalogContributor: "ぬゆり", ProviderContributor: "nulut"},
				{MusicID: 95, CatalogContributor: "梅とら", ProviderContributor: "Umetora"},
				{MusicID: 131, CatalogContributor: "cosMo@暴走P", ProviderContributor: "cosMo@BousouP"},
				{MusicID: 148, CatalogContributor: "藤原 基央", ProviderContributor: "Motoo Fujiwara"},
				{MusicID: 186, CatalogContributor: "cosMo@暴走P", ProviderContributor: "cosMo@BousouP"},
				{MusicID: 245, CatalogContributor: "羽生まゐご", ProviderContributor: "Hanyuu Maigo"},
				{MusicID: 334, CatalogContributor: "ひとしずく", ProviderContributor: "Hitoshizuku"},
				{MusicID: 334, CatalogContributor: "やま△", ProviderContributor: "Yama△"},
				{MusicID: 353, CatalogContributor: "ツミキ", ProviderContributor: "Tsumiki"},
				{MusicID: 386, CatalogContributor: "nyanyannya(大天才P)", ProviderContributor: "nyanyannya"},
				{MusicID: 475, CatalogContributor: "カルロス袴田(サイゼP)", ProviderContributor: "Carlos Hakamada"},
				{MusicID: 515, CatalogContributor: "原口沙輔", ProviderContributor: "Haraguchi Sasuke"},
				{MusicID: 555, CatalogContributor: "DECO*27 (OTOIRO)", ProviderContributor: "DECO*27"},
				{MusicID: 555, CatalogContributor: "tepe (OTOIRO)", ProviderContributor: "tepe"},
				{MusicID: 560, CatalogContributor: "ぬゆり", ProviderContributor: "nulut"},
				{MusicID: 562, CatalogContributor: "かいりきベア", ProviderContributor: "Kairiki Bear"},
				{MusicID: 583, CatalogContributor: "吉田夜世", ProviderContributor: "Yoshida Yasei"},
				{MusicID: 592, CatalogContributor: "奏音69", ProviderContributor: "Kanon69"},
				{MusicID: 635, CatalogContributor: "のぼる↑", ProviderContributor: "Noboru↑"},
				{MusicID: 750, CatalogContributor: "じん", ProviderContributor: "JIN"},
				{MusicID: 751, CatalogContributor: "じん", ProviderContributor: "JIN"},
				{MusicID: 752, CatalogContributor: "じん", ProviderContributor: "JIN"},
				{MusicID: 753, CatalogContributor: "じん", ProviderContributor: "JIN"},
				{MusicID: 756, CatalogContributor: "めろくる", ProviderContributor: "Mellowcle"},
				{MusicID: 764, CatalogContributor: "じん", ProviderContributor: "JIN"},
				{MusicID: 789, CatalogContributor: "卯花ロク", ProviderContributor: "Uka Roku"},
			},
		),
	}
	configs := append(sekaipediaConfigs, lyricssource.DefaultProviderConfigs()...)
	if registry, err := lyricssource.NewRegistry(configs...); err == nil {
		lyricsSrc = registry
	} else {
		log.Printf("[lyrics] source registry unavailable; falling back to vocaloid_fandom: %v", err)
	}
	return &Server{
		store: s, eventStore: es, auth: a, cfg: cfg, hub: hub, translator: tr,
		upstream: up, backup: bk, lyricsSrc: lyricsSrc,
		authAttempts:            newAuthAttemptLimiter(10, 5*time.Minute, 8192),
		editorGate:              gate,
		lyricsImports:           map[string]lyricsImportGrant{},
		lyricsInvariantReporter: log.Printf,
	}
}

// broadcast sends an SSE/WS event if a hub is configured (it may be nil in tests).
func (s *Server) broadcast(event string, data any) {
	if s.hub != nil {
		s.hub.Broadcast(event, data)
	}
	if s.wsHub != nil {
		s.wsHub.Broadcast(event, data)
		s.wsHub.BroadcastGateStatus()
	}
}

// revokeUser closes an account's live streams on both hubs and its
// collaboration rooms after its token generation changed. Either hub and the
// collaboration service may be nil in tests.
func (s *Server) revokeUser(user string) {
	if s.hub != nil {
		s.hub.RevokeUser(user)
	}
	if s.wsHub != nil {
		s.wsHub.RevokeUser(user)
	}
	if s.collab != nil {
		s.collab.RevokeUser(user)
	}
}

func (s *Server) reportLyricsInvariant(format string, args ...any) {
	if s.lyricsInvariantReporter != nil {
		s.lyricsInvariantReporter(format, args...)
	}
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

func (s *Server) contentMutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		releaseEditor, err := s.editorGate.BeginEditorContext(r.Context())
		if err != nil {
			writeEditorAdmissionError(w, err)
			return
		}
		defer releaseEditor()
		release, err := s.store.LockContentSharedContext(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "request canceled")
			return
		}
		defer release()
		next(w, r)
	}
}

func (s *Server) editorMutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, err := s.editorGate.BeginEditorContext(r.Context())
		if err != nil {
			writeEditorAdmissionError(w, err)
			return
		}
		defer release()
		next(w, r)
	}
}

func writeEditorAdmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, editorgate.ErrProducerRunning) {
		writeErr(w, http.StatusConflict, "producer is running; reload before saving")
		return
	}
	writeErr(w, http.StatusServiceUnavailable, "request canceled")
}

// writeJSON sends v as JSON with no-store caching (console data is live).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(v)
}

// writeErr sends a JSON error. msg comes from internal code, not user input.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeBody decodes a JSON request body into dst, returning false (and writing
// a 400) on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	data, ok := readJSONBody(w, r)
	return ok && decodeJSONBody(w, data, dst)
}

func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil || len(bytes.TrimSpace(data)) == 0 || validateUniqueJSON(data) != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return nil, false
	}
	return data, true
}

func decodeJSONBody(w http.ResponseWriter, data []byte, dst any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return false
	}
	return true
}

func validateUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid object")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid array")
		}
	default:
		return errors.New("unexpected delimiter")
	}
	return nil
}

// decodeOptional decodes a JSON body into dst, tolerating an empty body.
func decodeOptional(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxJSONBodyBytes {
		return errors.New("body too large")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := validateUniqueJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("invalid trailing JSON")
	}
	return nil
}

// currentUser returns the authenticated username, or "" if unauthenticated.
func currentUser(r *http.Request) string {
	if claims, ok := auth.FromContext(r.Context()); ok {
		return claims.Username
	}
	return ""
}
