package collab

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func TestAuthorizeConsumesTicketOnCrossRoomAttempt(t *testing.T) {
	service := &Service{tickets: map[string]ticketGrant{
		"once": {musicID: 12, room: "lyrics-12-e1", expiresAt: time.Now().Add(time.Minute)},
	}}
	wrong := httptest.NewRequest("GET", "http://example.test/yjs/lyrics/13?ticket=once", nil)
	wrong.SetPathValue("musicId", "13")
	if _, ok := service.authorize(wrong); ok {
		t.Fatal("cross-room ticket was accepted")
	}
	right := httptest.NewRequest("GET", "http://example.test/yjs/lyrics/12?ticket=once", nil)
	right.SetPathValue("musicId", "12")
	if _, ok := service.authorize(right); ok {
		t.Fatal("replayed ticket was accepted")
	}
}

func TestIssueTicketRepairsLedgerAfterCatalogMusicIDIsRecreated(t *testing.T) {
	fixture := setupContractService(t)
	first, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.persistence.db.Exec(`DELETE FROM catalog_music WHERE music_id=42`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.persistence.db.Exec(`UPDATE lyrics_collab_documents
		SET authority_sha256=? WHERE music_id=42`, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.store.UpsertMusicCatalog([]store.MusicCatalogRecord{{MusicID: 42, JapaneseTitle: "recreated"}}); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	if first.Room != "lyrics-42-e1" || second.Room != "lyrics-42-e2" {
		t.Fatalf("rooms before/after recreation = %q/%q", first.Room, second.Room)
	}
	request := httptest.NewRequest("GET", "http://example.test/yjs/lyrics/42?ticket="+first.Ticket, nil)
	request.SetPathValue("musicId", "42")
	if _, accepted := fixture.service.authorize(request); accepted {
		t.Fatal("ticket from the retired catalog generation remained usable")
	}
}

func TestRetireAllEvictsResidentRoomAfterTransactionalEpochFence(t *testing.T) {
	fixture := setupContractService(t)
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.server.Apply(t.Context(), ticket.Room, func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) {
			txn.GetText("restore-probe").Insert(txn, 0, "resident", nil)
		})
	}); err != nil {
		t.Fatal(err)
	}
	if fixture.service.server.GetDoc(ticket.Room) == nil {
		t.Fatal("test room did not become resident")
	}
	// RestoreBackupContext performs this fence before RetireAll is invoked.
	if _, err := fixture.service.persistence.db.Exec(`UPDATE lyrics_collab_documents SET epoch=epoch+1 WHERE music_id=42`); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RetireAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fixture.service.server.GetDoc(ticket.Room) != nil {
		t.Fatal("pre-restore room remained resident after RetireAll")
	}
	epoch, err := fixture.service.persistence.currentEpoch(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 3 {
		t.Fatalf("reseeded epoch=%d want 3", epoch)
	}
	fixture.service.activeMu.Lock()
	_, resident := fixture.service.resident[ticket.Room]
	fixture.service.activeMu.Unlock()
	if resident {
		t.Fatal("resident room ledger retained an unloaded epoch")
	}
}

func TestFirstCheckpointFreezesRoomBeforeMaterializingFinalPersistedUpdate(t *testing.T) {
	fixture := setupContractService(t)
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := fixture.service.persistence.baseline(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	client := crdt.New()
	if err := crdt.ApplyUpdateV1(client, baseline.update, nil); err != nil {
		t.Fatal(err)
	}
	state := client.StateVector()
	client.Transact(func(txn *crdt.Transaction) {
		root := txn.GetMap("lyrics")
		lines := crdt.NewArrayPrelim()
		line := crdt.NewMapPrelim()
		line.Set(txn, "id", "line-1")
		line.Set(txn, "order", 0)
		line.Set(txn, "japanese", newText(txn, "歌"))
		line.Set(txn, "zh-CN", newText(txn, "最终"))
		line.Set(txn, "en-US", newText(txn, ""))
		segments := crdt.NewArrayPrelim()
		segment := crdt.NewMapPrelim()
		segment.Set(txn, "text", newText(txn, "歌"))
		segment.Set(txn, "performerIds", crdt.NewArrayPrelim())
		ruby := crdt.NewArrayPrelim()
		span := crdt.NewMapPrelim()
		span.Set(txn, "text", newText(txn, "歌"))
		ruby.PushType(txn, span)
		segment.Set(txn, "ruby", ruby)
		segments.PushType(txn, segment)
		line.Set(txn, "segments", segments)
		lines.PushType(txn, line)
		root.Set(txn, "lines", lines)
	})
	finalUpdate := crdt.EncodeStateAsUpdateV1(client, state)
	fixture.service.closeRoom = func(room string, force bool) error {
		if room != ticket.Room || !force {
			t.Fatalf("close room=%q force=%v", room, force)
		}
		// Deterministically model the last update accepted before ygo finishes
		// closing peers and draining its persistence worker.
		return fixture.service.persistence.StoreUpdateContext(context.Background(), room, finalUpdate)
	}
	saved, changed, err := fixture.service.Checkpoint(t.Context(), 42, fixture.claims.Username)
	if err != nil {
		t.Fatal(err)
	}
	lyrics, ok := saved.(model.SongLyrics)
	if !ok || !changed || lyrics.Revision != 1 || len(lyrics.Lines) != 1 || lyrics.Lines[0].Chinese != "最终" {
		t.Fatalf("saved checkpoint %#v changed=%v", saved, changed)
	}
	if err := fixture.service.persistence.StoreUpdateContext(context.Background(), ticket.Room, finalUpdate); !errors.Is(err, ErrRetiredRoom) {
		t.Fatalf("old epoch accepted post-checkpoint update: %v", err)
	}
}

func TestFirstCheckpointClosesActiveWebSocketAndCompletes(t *testing.T) {
	fixture := setupContractService(t)
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/yjs/lyrics/{musicId}", fixture.service)
	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/yjs/lyrics/42?ticket=" + url.QueryEscape(ticket.Ticket)
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()

	deadline := time.Now().Add(2 * time.Second)
	for fixture.service.server.GetDoc(ticket.Room) == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fixture.service.server.GetDoc(ticket.Room) == nil {
		t.Fatal("active WebSocket did not register its collaboration room")
	}

	result := make(chan error, 1)
	go func() {
		_, _, checkpointErr := fixture.service.Checkpoint(context.Background(), 42, fixture.claims.Username)
		result <- checkpointErr
	}()
	select {
	case checkpointErr := <-result:
		if checkpointErr == nil {
			t.Fatal("blank first checkpoint unexpectedly succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first checkpoint deadlocked behind an active WebSocket")
	}

	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
}

func TestIssueTicketRequiresAtomicallyAcceptedProducerStatus(t *testing.T) {
	fixture := setupContractService(t)
	if _, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("missing producer proof error=%v", err)
	}
	stale := fixture.service.gate.Status()
	releaseProducer, err := fixture.service.gate.BeginProducer()
	if err != nil {
		t.Fatal(err)
	}
	releaseProducer()
	if _, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, stale); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("stale producer proof error=%v", err)
	}
}

func TestAuthorizedYjsConnectionHoldsEditorGateUntilRequestEnds(t *testing.T) {
	fixture := setupContractService(t)
	status := fixture.service.gate.Status()
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, status)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://example.test/yjs/lyrics/42?ticket="+ticket.Ticket, nil)
	request.SetPathValue("musicId", "42")
	capture := &authCapture{}
	request = request.WithContext(context.WithValue(request.Context(), authCaptureKey{}, capture))
	if _, accepted := fixture.service.authorize(request); !accepted || capture.release == nil {
		t.Fatalf("authorize accepted=%v release=%v", accepted, capture.release != nil)
	}
	producerAcquired := make(chan func(), 1)
	go func() {
		release, err := fixture.service.gate.BeginProducer()
		if err != nil {
			producerAcquired <- nil
			return
		}
		producerAcquired <- release
	}()
	select {
	case <-producerAcquired:
		t.Fatal("producer crossed an active Yjs editor")
	case <-time.After(100 * time.Millisecond):
	}
	capture.release()
	select {
	case release := <-producerAcquired:
		if release == nil {
			t.Fatal("producer acquisition failed")
		}
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not continue after Yjs request release")
	}
}

func TestRetiringRoomRejectsAuthorizedLateLoad(t *testing.T) {
	fixture := setupContractService(t)
	status := fixture.service.gate.Status()
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, status)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://example.test/yjs/lyrics/42?ticket="+ticket.Ticket, nil)
	request.SetPathValue("musicId", "42")
	capture := &authCapture{}
	request = request.WithContext(context.WithValue(request.Context(), authCaptureKey{}, capture))
	if _, accepted := fixture.service.authorize(request); !accepted || capture.release == nil {
		t.Fatalf("authorize accepted=%v release=%v", accepted, capture.release != nil)
	}
	fixture.service.ticketsMu.Lock()
	fixture.service.retiring[ticket.Room] = struct{}{}
	fixture.service.ticketsMu.Unlock()
	if fixture.service.markResident(ticket.Room) {
		t.Fatal("authorized request loaded a room after retirement began")
	}
	if err := fixture.service.server.Apply(t.Context(), ticket.Room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); !errors.Is(err, ErrRetiredRoom) {
		t.Fatalf("late room load error=%v want ErrRetiredRoom", err)
	}
	if fixture.service.server.GetDoc(ticket.Room) != nil {
		t.Fatal("failed retired-room load left a resident placeholder")
	}
	capture.release()
	fixture.service.untrack(ticket.Room, fixture.claims.Username)
}

func TestRevokeUserClosesLiveConnectionAndReleasesEditorGate(t *testing.T) {
	fixture := setupContractService(t)
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/yjs/lyrics/{musicId}", fixture.service)
	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/yjs/lyrics/42?ticket=" + url.QueryEscape(ticket.Ticket)
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()
	deadline := time.Now().Add(2 * time.Second)
	for fixture.service.server.GetDoc(ticket.Room) == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fixture.service.server.GetDoc(ticket.Room) == nil {
		t.Fatal("active WebSocket did not register its collaboration room")
	}

	fixture.service.RevokeUser(fixture.claims.Username)

	// The revalidation ticker only fires every 20 seconds; revocation must not
	// wait for it.
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			// gorilla replaces the deadline error with its own net.Error, so the
			// still-open case is recognised by Timeout rather than by identity.
			var readErr net.Error
			if errors.As(err, &readErr) && readErr.Timeout() {
				t.Fatal("revoked account kept its collaboration connection open")
			}
			break
		}
	}
	producerAcquired := make(chan func(), 1)
	go func() {
		release, beginErr := fixture.service.gate.BeginProducer()
		if beginErr != nil {
			producerAcquired <- nil
			return
		}
		producerAcquired <- release
	}()
	select {
	case release := <-producerAcquired:
		if release == nil {
			t.Fatal("producer acquisition failed")
		}
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("revoked collaboration connection kept the editor gate slot")
	}
}
