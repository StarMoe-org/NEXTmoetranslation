package collab

import (
	"context"
	"fmt"
	"testing"

	"github.com/reearth/ygo/crdt"
	"moesekai/server/internal/model"
)

func seedDraftLine(t *testing.T, service *Service, room, chinese string) {
	t.Helper()
	if err := service.server.Apply(context.Background(), room, func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) {
			root := txn.GetMap("lyrics")
			lines := crdt.NewArrayPrelim()
			line := crdt.NewMapPrelim()
			line.Set(txn, "id", "line-1")
			line.Set(txn, "order", 0)
			line.Set(txn, "japanese", newText(txn, "歌"))
			line.Set(txn, "zh-CN", newText(txn, chinese))
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
	}); err != nil {
		t.Fatal(err)
	}
}

func draftChinese(doc *crdt.Doc) (*crdt.YText, error) {
	value, ok := doc.GetMap("lyrics").Get("lines")
	if !ok {
		return nil, fmt.Errorf("draft has no lines")
	}
	lines, ok := value.(*crdt.YArray)
	if !ok {
		return nil, fmt.Errorf("lines type = %T", value)
	}
	line, ok := lines.Get(0).(*crdt.YMap)
	if !ok {
		return nil, fmt.Errorf("line type = %T", lines.Get(0))
	}
	value, _ = line.Get("zh-CN")
	text, ok := value.(*crdt.YText)
	if !ok {
		return nil, fmt.Errorf("zh-CN type = %T", value)
	}
	return text, nil
}

func editDraftLine(t *testing.T, service *Service, room, chinese string) {
	t.Helper()
	var editErr error
	if err := service.server.Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		// Shared types are resolved outside transact: reads take the doc lock the
		// transaction already holds.
		text, lookupErr := draftChinese(doc)
		if lookupErr != nil {
			editErr = lookupErr
			return
		}
		transact(func(txn *crdt.Transaction) {
			if text.Len() > 0 {
				text.Delete(txn, 0, text.Len())
			}
			text.Insert(txn, 0, chinese, nil)
		})
	}); err != nil {
		t.Fatal(err)
	}
	if editErr != nil {
		t.Fatal(editErr)
	}
}

func durableDraft(t *testing.T, service *Service, room string) model.SongLyrics {
	t.Helper()
	identity, err := parseRoom(room)
	if err != nil {
		t.Fatal(err)
	}
	update, err := service.persistence.LoadDoc(room)
	if err != nil {
		t.Fatal(err)
	}
	document := crdt.New()
	if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
		t.Fatal(err)
	}
	draft, err := materializeDocument(document.GetMap("lyrics"), documentLegacy, identity.musicID)
	if err != nil {
		t.Fatal(err)
	}
	lyrics, ok := draft.(model.SongLyrics)
	if !ok {
		t.Fatalf("durable draft type = %T", draft)
	}
	return lyrics
}

func TestRepeatedCheckpointsKeepDurableRevisionAuthoritative(t *testing.T) {
	fixture := setupContractService(t)
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	seedDraftLine(t, fixture.service, ticket.Room, "初稿")
	saved, changed, err := fixture.service.Checkpoint(t.Context(), 42, fixture.claims.Username)
	if err != nil {
		t.Fatal(err)
	}
	if lyrics, ok := saved.(model.SongLyrics); !ok || !changed || lyrics.Revision != 1 {
		t.Fatalf("first checkpoint saved=%#v changed=%v", saved, changed)
	}
	epoch, err := fixture.service.persistence.currentEpoch(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	room := roomName(42, epoch)

	// Each save writes the envelope twice under the old code: once into the
	// detached checkpoint copy and once into the live doc. Those writes are
	// concurrent, so the durable winner is a client-id coin flip per iteration.
	for revision := 2; revision <= 13; revision++ {
		editDraftLine(t, fixture.service, room, fmt.Sprintf("第%d稿", revision))
		saved, changed, err := fixture.service.Checkpoint(t.Context(), 42, fixture.claims.Username)
		if err != nil {
			t.Fatalf("checkpoint %d: %v", revision, err)
		}
		lyrics, ok := saved.(model.SongLyrics)
		if !ok || !changed || lyrics.Revision != revision {
			t.Fatalf("checkpoint %d saved=%#v changed=%v", revision, saved, changed)
		}
		if durable := durableDraft(t, fixture.service, room); durable.Revision != revision {
			t.Fatalf("durable revision=%d authority=%d after checkpoint %d", durable.Revision, revision, revision)
		}
	}

	if err := fixture.service.closeRetiredRoom(room); err != nil {
		t.Fatal(err)
	}
	editDraftLine(t, fixture.service, room, "重载稿")
	saved, changed, err = fixture.service.Checkpoint(t.Context(), 42, fixture.claims.Username)
	if err != nil {
		t.Fatalf("checkpoint after room reload: %v", err)
	}
	if lyrics, ok := saved.(model.SongLyrics); !ok || !changed || lyrics.Revision != 14 || lyrics.Lines[0].Chinese != "重载稿" {
		t.Fatalf("reloaded checkpoint saved=%#v changed=%v", saved, changed)
	}
}
