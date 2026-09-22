// Package collab hosts the ephemeral Yjs collaboration layer for lyrics.
// SQLite remains the authoritative publish/backup/audit model; collaborative
// documents are fenced drafts which only enter that model through Checkpoint.
package collab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	"moesekai/server/internal/auth"
	"moesekai/server/internal/db"
	"moesekai/server/internal/editorgate"
	"moesekai/server/internal/store"
)

const (
	ticketTTL              = 45 * time.Second
	maxTickets             = 1024
	maxDocumentUpdateBytes = 8 << 20
	maxUpdateBytes         = maxDocumentUpdateBytes
	maxMessageBytes        = maxUpdateBytes + (64 << 10)
)

var (
	ErrInvalidRoom      = errors.New("invalid collaboration room")
	ErrRoomUnavailable  = errors.New("collaboration room unavailable")
	ErrRetiredRoom      = errors.New("collaboration room is retired")
	ErrAuthorityDrift   = errors.New("authoritative lyrics changed")
	ErrSchemaMismatch   = errors.New("collaboration schema mismatch")
	ErrDocumentMismatch = errors.New("collaboration document mismatch")
	ErrTicketInvalid    = errors.New("collaboration ticket invalid")
	ErrTicketCapacity   = errors.New("collaboration ticket capacity exhausted")
	ErrUpdateTooLarge   = errors.New("collaboration update too large")
)

type Ticket struct {
	Ticket    string    `json:"ticket"`
	Room      string    `json:"room"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type ticketGrant struct {
	username     string
	role         string
	tokenVersion int
	bearer       string
	musicID      int
	epoch        int64
	room         string
	producer     editorgate.Status
	expiresAt    time.Time
}

type authCapture struct {
	room     string
	username string
	bearer   string
	release  func()
}

type authCaptureKey struct{}

type Service struct {
	auth        *auth.Auth
	gate        *editorgate.Gate
	store       *store.Store
	persistence *sqlitePersistence
	server      *ygws.Server

	ticketsMu sync.Mutex
	tickets   map[string]ticketGrant
	retiring  map[string]struct{}

	activeMu  sync.Mutex
	active    map[string]int
	userRooms map[string]map[string]int
	resident  map[string]struct{}
	opMu      sync.Mutex
	closeRoom func(string, bool) error
}

func New(database *db.DB, lyricsStore *store.Store, authService *auth.Auth, gate *editorgate.Gate, allowedOrigins ...string) (*Service, error) {
	if database == nil || lyricsStore == nil || authService == nil || gate == nil {
		return nil, errors.New("collaboration dependencies are required")
	}
	persistence := &sqlitePersistence{db: database, store: lyricsStore}
	service := &Service{
		auth: authService, gate: gate, store: lyricsStore, persistence: persistence,
		tickets: make(map[string]ticketGrant), retiring: make(map[string]struct{}),
		active: make(map[string]int), userRooms: make(map[string]map[string]int),
		resident: make(map[string]struct{}),
	}
	server := ygws.NewServerWithPersistence(persistence)
	server.Authorize = service.authorize
	server.MaxConnections = 50
	server.MaxPeersPerRoom = 10
	server.MaxRooms = 256
	server.MaxUpdateBytes = maxUpdateBytes
	server.MaxMessageBytes = maxMessageBytes
	server.MessageRateLimit = 20
	server.MessageRateBurst = 40
	server.MaxAwarenessBytesPerRoom = 256 << 10
	server.MaxAwarenessClientsPerRoom = 256
	server.AwarenessExpiry = 90 * time.Second
	server.RoomIdleTimeout = 5 * time.Minute
	server.MaxResidentRooms = 128
	server.PersistCoalesceWindow = 100 * time.Millisecond
	server.PersistCoalesceMaxWait = time.Second
	server.CompactEvery = 100
	server.MaxPendingItems = 100_000
	server.HandshakeTimeout = 10 * time.Second
	server.PeerWriteQueueSize = 256
	server.OnLoadDocument = func(_ context.Context, room string, _ *crdt.Doc) error {
		if !service.markResident(room) {
			return ErrRetiredRoom
		}
		return nil
	}
	server.OnUnloadDocument = func(_ context.Context, room string) {
		service.unmarkResident(room)
	}
	if len(allowedOrigins) > 0 && strings.TrimSpace(allowedOrigins[0]) != "" {
		server.AllowedOrigins = []string{strings.TrimSpace(allowedOrigins[0])}
	}
	service.server = server
	service.closeRoom = server.CloseRoom
	return service, nil
}

func (s *Service) IssueTicket(
	ctx context.Context,
	claims *auth.Claims,
	bearer string,
	musicID int,
	acceptedProducer ...editorgate.Status,
) (Ticket, error) {
	if claims == nil || strings.TrimSpace(bearer) == "" || musicID <= 0 {
		return Ticket{}, ErrTicketInvalid
	}
	verified, err := s.auth.VerifyToken(bearer)
	if err != nil || verified.Username != claims.Username || verified.Role != claims.Role || verified.TokenVersion != claims.TokenVersion {
		return Ticket{}, ErrTicketInvalid
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if len(acceptedProducer) != 1 || acceptedProducer[0].Running {
		return Ticket{}, ErrTicketInvalid
	}
	producer := acceptedProducer[0]
	if current := s.gate.Status(); current != producer {
		return Ticket{}, ErrTicketInvalid
	}
	baseline, err := s.persistence.ensureRoom(ctx, musicID)
	if errors.Is(err, ErrAuthorityDrift) {
		// A catalog row may be deleted and later recreated with the same musicId,
		// or an authoritative save may win a race with ticket issuance. The
		// generation ledger intentionally survives deletion, so repair it by
		// reseeding a fresh epoch instead of leaving this ID permanently blocked.
		if err := s.replaceFromAuthoritativeLocked(ctx, musicID); err != nil {
			return Ticket{}, err
		}
		baseline, err = s.persistence.ensureRoom(ctx, musicID)
	}
	if err != nil {
		return Ticket{}, err
	}
	ticketValue, err := randomTicket()
	if err != nil {
		return Ticket{}, err
	}
	expiresAt := time.Now().UTC().Add(ticketTTL)
	grant := ticketGrant{
		username: claims.Username, role: claims.Role, tokenVersion: claims.TokenVersion,
		bearer: bearer, musicID: musicID, epoch: baseline.epoch,
		room: roomName(musicID, baseline.epoch), producer: producer, expiresAt: expiresAt,
	}
	s.ticketsMu.Lock()
	now := time.Now()
	for key, existing := range s.tickets {
		if !existing.expiresAt.After(now) {
			delete(s.tickets, key)
		}
	}
	if len(s.tickets) >= maxTickets {
		s.ticketsMu.Unlock()
		return Ticket{}, ErrTicketCapacity
	}
	s.tickets[ticketValue] = grant
	s.ticketsMu.Unlock()
	return Ticket{Ticket: ticketValue, Room: grant.room, ExpiresAt: expiresAt}, nil
}

func randomTicket() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Service) authorize(r *http.Request) (ygws.ConnectionConfig, bool) {
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	values, ok := query["ticket"]
	ticket := ""
	if len(values) == 1 {
		ticket = values[0]
	}
	var grant ticketGrant
	var exists bool
	s.ticketsMu.Lock()
	for _, candidate := range values {
		if candidate == "" {
			continue
		}
		candidateGrant, found := s.tickets[candidate]
		if !found {
			continue
		}
		delete(s.tickets, candidate)
		if len(values) == 1 {
			grant, exists = candidateGrant, true
		}
	}
	s.ticketsMu.Unlock()
	if queryErr != nil || len(query) != 1 || !ok || len(values) != 1 || ticket == "" || strings.ContainsAny(ticket, " \t\r\n") {
		return ygws.ConnectionConfig{}, false
	}
	pathMusicID, pathErr := parsePositiveInt(r.PathValue("musicId"))
	if !exists || !grant.expiresAt.After(time.Now()) || pathErr != nil || pathMusicID != grant.musicID {
		return ygws.ConnectionConfig{}, false
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.ticketsMu.Lock()
	_, roomRetiring := s.retiring[grant.room]
	s.ticketsMu.Unlock()
	if roomRetiring {
		return ygws.ConnectionConfig{}, false
	}
	claims, err := s.auth.VerifyToken(grant.bearer)
	if err != nil || claims.Username != grant.username || claims.Role != grant.role || claims.TokenVersion != grant.tokenVersion {
		return ygws.ConnectionConfig{}, false
	}
	if s.gate.Status() != grant.producer {
		return ygws.ConnectionConfig{}, false
	}
	releaseEditor, acceptedStatus, current := s.gate.BeginStrictEditor(
		grant.producer.InstanceID, grant.producer.Revision, grant.producer.CompletedGeneration,
	)
	if !current || acceptedStatus != grant.producer {
		return ygws.ConnectionConfig{}, false
	}
	releaseNeeded := true
	defer func() {
		if releaseNeeded {
			releaseEditor()
		}
	}()
	baseline, err := s.persistence.ensureRoom(r.Context(), grant.musicID)
	if err != nil || baseline.epoch != grant.epoch || roomName(grant.musicID, baseline.epoch) != grant.room {
		return ygws.ConnectionConfig{}, false
	}
	// Never trust a path-derived room. The ticket is the authority and rewrites
	// the value consumed by ygo after the strict equality check above.
	r.SetPathValue("room", grant.room)
	if capture, ok := r.Context().Value(authCaptureKey{}).(*authCapture); ok {
		capture.room, capture.username, capture.bearer, capture.release = grant.room, grant.username, grant.bearer, releaseEditor
		releaseNeeded = false
	}
	s.track(grant.room, grant.username)
	go s.revalidateConnection(r.Context(), grant)
	return ygws.ConnectionConfig{}, true
}

func parsePositiveInt(value string) (int, error) {
	var result int
	if value == "" || strings.ContainsAny(value, "+- \t\r\n") {
		return 0, ErrInvalidRoom
	}
	if _, err := fmt.Sscanf(value, "%d", &result); err != nil || result <= 0 || fmt.Sprintf("%d", result) != value {
		return 0, ErrInvalidRoom
	}
	return result, nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	capture := &authCapture{}
	r = r.WithContext(context.WithValue(r.Context(), authCaptureKey{}, capture))
	s.server.ServeHTTP(w, r)
	if capture.release != nil {
		capture.release()
	}
	if capture.room != "" {
		s.untrack(capture.room, capture.username)
	}
}

func (s *Service) revalidateConnection(ctx context.Context, grant ticketGrant) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			claims, err := s.auth.VerifyToken(grant.bearer)
			currentEpoch, epochErr := s.persistence.currentEpoch(ctx, grant.musicID)
			if err != nil || claims.Username != grant.username || claims.Role != grant.role || claims.TokenVersion != grant.tokenVersion ||
				s.gate.Status() != grant.producer || epochErr != nil || currentEpoch != grant.epoch {
				_ = s.server.CloseRoom(grant.room, true)
				return
			}
		}
	}
}

func (s *Service) track(room, username string) {
	s.activeMu.Lock()
	s.active[room]++
	rooms := s.userRooms[username]
	if rooms == nil {
		rooms = make(map[string]int)
		s.userRooms[username] = rooms
	}
	rooms[room]++
	s.activeMu.Unlock()
}

func (s *Service) untrack(room, username string) {
	s.activeMu.Lock()
	if s.active[room] <= 1 {
		delete(s.active, room)
	} else {
		s.active[room]--
	}
	if rooms := s.userRooms[username]; rooms != nil {
		if rooms[room] <= 1 {
			delete(rooms, room)
		} else {
			rooms[room]--
		}
		if len(rooms) == 0 {
			delete(s.userRooms, username)
		}
	}
	s.activeMu.Unlock()
}

// RevokeUser closes every collaboration room the account is connected to, so a
// deleted, demoted, or re-tokenized account stops writing immediately instead of
// at the next revalidation tick. ygo closes rooms, not individual peers, so
// co-editors of those rooms reconnect too. The forced close ends the peer's
// request, which releases its editor gate slot. Unused tickets need no cleanup:
// authorize re-verifies the bearer against the users table.
func (s *Service) RevokeUser(username string) {
	if strings.TrimSpace(username) == "" {
		return
	}
	s.activeMu.Lock()
	rooms := make([]string, 0, len(s.userRooms[username]))
	for room := range s.userRooms[username] {
		rooms = append(rooms, room)
	}
	s.activeMu.Unlock()
	for _, room := range rooms {
		if err := s.closeRetiredRoom(room); err != nil {
			log.Printf("[collab] close revoked room %s: %v", room, err)
		}
	}
}

func (s *Service) markResident(room string) bool {
	s.ticketsMu.Lock()
	defer s.ticketsMu.Unlock()
	_, retiring := s.retiring[room]
	if retiring {
		return false
	}
	s.activeMu.Lock()
	s.resident[room] = struct{}{}
	s.activeMu.Unlock()
	return true
}

func (s *Service) unmarkResident(room string) {
	s.activeMu.Lock()
	delete(s.resident, room)
	s.activeMu.Unlock()
}

func (s *Service) closeRetiredRoom(room string) error {
	if room == "" {
		return nil
	}
	closeRoom := s.closeRoom
	if closeRoom == nil {
		closeRoom = s.server.CloseRoom
	}
	err := closeRoom(room, true)
	if err == nil || errors.Is(err, ygws.ErrRoomNotFound) {
		s.unmarkResident(room)
		return nil
	}
	return err
}

func (s *Service) ReplaceFromAuthoritative(ctx context.Context, musicID int) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.replaceFromAuthoritativeLocked(ctx, musicID)
}

func (s *Service) replaceFromAuthoritativeLocked(ctx context.Context, musicID int) error {
	oldRoom, _, err := s.persistence.replaceFromAuthoritative(ctx, musicID)
	if err != nil {
		return err
	}
	s.invalidateTickets(musicID)
	if oldRoom != "" {
		if closeErr := s.closeRetiredRoom(oldRoom); closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (s *Service) invalidateTickets(musicID int) {
	s.ticketsMu.Lock()
	for ticket, grant := range s.tickets {
		if grant.musicID == musicID {
			delete(s.tickets, ticket)
		}
	}
	s.ticketsMu.Unlock()
}

func (s *Service) beginRetiring(room string, musicID int) {
	s.ticketsMu.Lock()
	s.retiring[room] = struct{}{}
	for ticket, grant := range s.tickets {
		if grant.musicID == musicID {
			delete(s.tickets, ticket)
		}
	}
	s.ticketsMu.Unlock()
}

func (s *Service) endRetiring(room string) {
	s.ticketsMu.Lock()
	delete(s.retiring, room)
	s.ticketsMu.Unlock()
}

// RetireAll is used after an authoritative restore. RestoreBackupContext bumps
// every epoch inside its own transaction; this callback reseeds snapshots and
// closes any pre-restore in-memory rooms which were tracked at authorization.
func (s *Service) RetireAll(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.ticketsMu.Lock()
	s.tickets = make(map[string]ticketGrant)
	s.ticketsMu.Unlock()
	s.activeMu.Lock()
	roomSet := make(map[string]struct{}, len(s.active)+len(s.resident))
	for room := range s.active {
		roomSet[room] = struct{}{}
	}
	for room := range s.resident {
		roomSet[room] = struct{}{}
	}
	s.activeMu.Unlock()
	for room := range roomSet {
		if err := s.closeRetiredRoom(room); err != nil {
			log.Printf("[collab] close retired room %s: %v", room, err)
		}
	}
	rows, err := s.persistence.db.QueryContext(ctx, `SELECT collab.music_id
		FROM lyrics_collab_documents collab JOIN catalog_music music ON music.music_id=collab.music_id
		ORDER BY collab.music_id`)
	if err != nil {
		return err
	}
	var musicIDs []int
	for rows.Next() {
		var musicID int
		if err := rows.Scan(&musicID); err != nil {
			rows.Close()
			return err
		}
		musicIDs = append(musicIDs, musicID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, musicID := range musicIDs {
		oldRoom, _, err := s.persistence.replaceFromAuthoritative(ctx, musicID)
		if err != nil {
			return fmt.Errorf("reseed collaboration for music %d: %w", musicID, err)
		}
		if err := s.closeRetiredRoom(oldRoom); err != nil {
			log.Printf("[collab] close reseeded room %s: %v", oldRoom, err)
		}
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
