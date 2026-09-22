package collab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

type CheckpointConflict struct {
	Code    string
	Details []string
	Current any
}

func (e *CheckpointConflict) Error() string { return e.Code }

func (s *Service) Checkpoint(ctx context.Context, musicID int, user string) (any, bool, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	baseline, err := s.persistence.baseline(ctx, musicID)
	if err != nil {
		return nil, false, err
	}
	authority, err := s.persistence.authoritativeDocument(musicID)
	if err != nil {
		return nil, false, err
	}
	_, authoritySHA, authorityRevision, authorityKind, err := canonicalDocument(authority)
	if err != nil {
		return nil, false, err
	}
	if authorityKind != baseline.kind || authoritySHA != baseline.authoritySHA256 || authorityRevision != baseline.baseRevision {
		return nil, false, &CheckpointConflict{
			Code: "source_drift", Details: []string{"the authoritative lyrics changed outside this collaboration room"}, Current: authority,
		}
	}
	room := roomName(musicID, baseline.epoch)
	if baseline.baseRevision == 0 {
		// A first save establishes the immutable source envelope. Freeze the
		// entire epoch before materializing it: invalidate unused tickets, reject
		// in-flight authorizations, disconnect peers, and wait for ygo's
		// persistence worker to drain. Reloading afterwards includes every update
		// the old room accepted before the freeze and no update can enter later.
		s.beginRetiring(room, musicID)
		defer s.endRetiring(room)
		if err := s.closeRetiredRoom(room); err != nil {
			return nil, false, err
		}
		baseline, err = s.persistence.baseline(ctx, musicID)
		if err != nil {
			return nil, false, err
		}
		if baseline.baseRevision != 0 || roomName(musicID, baseline.epoch) != room {
			return nil, false, ErrRetiredRoom
		}
	}
	doc := s.server.GetDoc(room)
	if doc == nil {
		// No peer currently owns the room. Decode the persisted snapshot so a
		// checkpoint immediately after reconnect remains deterministic.
		doc = crdt.New()
		if err := crdt.ApplyUpdateV1(doc, baseline.update, nil); err != nil {
			return nil, false, err
		}
	}
	root := doc.GetMap("lyrics")
	draft, err := materializeDocument(root, baseline.kind, musicID)
	if err != nil {
		return nil, false, err
	}
	_, _, draftRevision, _, err := canonicalDocument(draft)
	if err != nil {
		return nil, false, err
	}
	if draftRevision != authorityRevision {
		return nil, false, &CheckpointConflict{
			Code: "revision_conflict", Details: []string{"the collaborative draft was based on a different revision"}, Current: authority,
		}
	}
	if err := validateImmutableDraft(authority, draft); err != nil {
		return nil, false, err
	}
	var saved any
	var changed bool
	var newRevision int
	var authorityUpdate []byte
	commitCheckpoint := func(tx *sql.Tx, final any, didChange bool) error {
		_, newSHA, revision, kind, canonicalErr := canonicalDocument(final)
		if canonicalErr != nil {
			return canonicalErr
		}
		if kind != baseline.kind {
			return ErrDocumentMismatch
		}
		newRevision = revision
		if baseline.baseRevision == 0 && didChange {
			_, _, reseedErr := s.persistence.reseedCheckpointTx(ctx, tx, baseline, final, user, true)
			return reseedErr
		}
		checkpointUpdate, authority, updateErr := checkpointDocumentUpdate(doc, final)
		if updateErr != nil {
			return updateErr
		}
		authorityUpdate = authority
		return s.persistence.commitCheckpointTx(ctx, tx, baseline, checkpointUpdate, revision, newSHA, user, didChange)
	}
	switch draft := draft.(type) {
	case model.SongLyrics:
		result, didChange, saveErr := s.store.SaveLyricsMutationWithBeforeCommit(
			draft, user,
			func(tx *sql.Tx, final model.SongLyrics, changed bool) error {
				return commitCheckpoint(tx, final, changed)
			},
		)
		if saveErr != nil {
			return nil, false, saveErr
		}
		saved, changed = result, didChange
	case store.LyricsRenditionDocument:
		result, didChange, saveErr := s.store.SaveLyricsRenditionMutationWithBeforeCommit(
			draft, user,
			func(tx *sql.Tx, final store.LyricsRenditionDocument, changed bool) error {
				return commitCheckpoint(tx, final, changed)
			},
		)
		if saveErr != nil {
			return nil, false, saveErr
		}
		saved, changed = result, didChange
	default:
		return nil, false, ErrDocumentMismatch
	}
	if changed && baseline.baseRevision != 0 && s.server.GetDoc(room) != nil {
		// The live doc adopts the very update the durable checkpoint stores, so the
		// envelope keys keep a single writer identity. Editable fields changed
		// concurrently during the DB save stay intact and will be checkpointed by
		// the next call; if this resident copy disappeared meanwhile, closing it
		// simply forces the next peer to reload that durable state.
		if err := s.applyAuthorityUpdate(ctx, room, authorityUpdate); err != nil {
			log.Printf("[collab] advance live checkpoint envelope musicId=%d room=%s: %v", musicID, room, err)
			if closeErr := s.closeRetiredRoom(room); closeErr != nil {
				log.Printf("[collab] close stale live checkpoint room musicId=%d room=%s: %v", musicID, room, closeErr)
			}
		}
	}
	if err := s.store.RecordAudit(user, "lyrics.collab.checkpoint", fmt.Sprintf("musicId=%d revision=%d", musicID, newRevision)); err != nil {
		log.Printf("[collab] checkpoint audit failed musicId=%d revision=%d: %v", musicID, newRevision, err)
	}
	return saved, changed, nil
}

// checkpointDocumentUpdate encodes the state committed to the collaboration
// ledger. The authoritative envelope comes from final while editable shared
// fields come from the room snapshot captured before the store transaction.
// This keeps the SQLite checkpoint internally consistent without mutating the
// live room until the authoritative and collaboration writes have committed.
// The second return value is the envelope write on its own: the live room must
// integrate that exact update rather than set the keys again, because a second
// writer would be concurrent with this one and the map winner would then be
// decided by client id.
func checkpointDocumentUpdate(doc *crdt.Doc, final any) ([]byte, []byte, error) {
	if doc == nil {
		return nil, nil, ErrRoomUnavailable
	}
	update := crdt.EncodeStateAsUpdateV1(doc, nil)
	if len(update) == 0 || len(update) > maxDocumentUpdateBytes {
		return nil, nil, ErrUpdateTooLarge
	}
	checkpoint := crdt.New()
	if err := crdt.ApplyUpdateV1(checkpoint, update, nil); err != nil {
		return nil, nil, err
	}
	var authority []byte
	unsubscribe := checkpoint.OnUpdate(func(delta []byte, _ any) { authority = delta })
	checkpoint.Transact(func(txn *crdt.Transaction) {
		root := txn.GetMap("lyrics")
		switch document := final.(type) {
		case model.SongLyrics:
			setAuthorityScalar(txn, root, "status", document.Status, true)
			setAuthorityScalar(txn, root, "publishedRevision", document.PublishedRevision, document.PublishedRevision != 0)
			setAuthorityScalar(txn, root, "revision", document.Revision, true)
			setAuthorityScalar(txn, root, "updatedAt", document.UpdatedAt, true)
		case store.LyricsRenditionDocument:
			setAuthorityScalar(txn, root, "status", document.Status, true)
			setAuthorityScalar(txn, root, "publishedRevision", document.PublishedRevision, document.PublishedRevision != 0)
			setAuthorityScalar(txn, root, "revision", document.Revision, true)
			setAuthorityScalar(txn, root, "updatedAt", document.UpdatedAt, true)
		}
	})
	unsubscribe()
	update = crdt.EncodeStateAsUpdateV1(checkpoint, nil)
	if len(update) == 0 || len(update) > maxDocumentUpdateBytes {
		return nil, nil, ErrUpdateTooLarge
	}
	return update, authority, nil
}

func validateImmutableDraft(authority, draft any) error {
	switch current := authority.(type) {
	case model.SongLyrics:
		requested, ok := draft.(model.SongLyrics)
		if !ok || requested.MusicID != current.MusicID {
			return &CheckpointConflict{Code: "source_drift", Details: []string{"lyrics document kind or musicId changed"}, Current: authority}
		}
		if current.Revision == 0 {
			return nil
		}
		if requested.SourcePageID != current.SourcePageID || requested.SourceRevisionID != current.SourceRevisionID ||
			requested.SourceSHA1 != current.SourceSHA1 || requested.SourceFetchedAt != current.SourceFetchedAt || requested.SourceURL != current.SourceURL ||
			len(requested.Lines) != len(current.Lines) {
			return &CheckpointConflict{Code: "source_drift", Details: []string{"source provenance or ordered source lines changed"}, Current: authority}
		}
		for index := range current.Lines {
			left, right := requested.Lines[index], current.Lines[index]
			if left.ID != right.ID || left.Order != right.Order || left.Japanese != right.Japanese {
				return &CheckpointConflict{Code: "source_drift", Details: []string{"line IDs, order, and Japanese source text are immutable"}, Current: authority}
			}
		}
		return nil
	case store.LyricsRenditionDocument:
		requested, ok := draft.(store.LyricsRenditionDocument)
		if !ok || requested.MusicID != current.MusicID || requested.Status != "draft" || requested.PublishedRevision != 0 || requested.UpdatedAt != current.UpdatedAt {
			return &CheckpointConflict{Code: "source_drift", Details: []string{"plural rendition source envelope changed"}, Current: authority}
		}
		if len(requested.Renditions) != len(current.Renditions) {
			return &CheckpointConflict{Code: "source_drift", Details: []string{"plural rendition count changed"}, Current: authority}
		}
		for i := range requested.Renditions {
			reqRend := requested.Renditions[i]
			curRend := current.Renditions[i]
			if reqRend.Key != curRend.Key || reqRend.Kind != curRend.Kind {
				return &CheckpointConflict{Code: "source_drift", Details: []string{"rendition key or kind changed"}, Current: authority}
			}
			if (reqRend.Full == nil) != (curRend.Full == nil) || (reqRend.Game == nil) != (curRend.Game == nil) {
				return &CheckpointConflict{Code: "source_drift", Details: []string{"rendition sides changed"}, Current: authority}
			}
			if reqRend.Full != nil && curRend.Full != nil {
				if len(reqRend.Full.Lines) != len(curRend.Full.Lines) {
					return &CheckpointConflict{Code: "source_drift", Details: []string{"rendition line count changed"}, Current: authority}
				}
				for lineIdx := range reqRend.Full.Lines {
					reqLine := reqRend.Full.Lines[lineIdx]
					curLine := curRend.Full.Lines[lineIdx]
					if reqLine.ID != curLine.ID || reqLine.Order != curLine.Order || reqLine.Japanese != curLine.Japanese {
						return &CheckpointConflict{Code: "source_drift", Details: []string{"line IDs, order, and Japanese source text are immutable"}, Current: authority}
					}
				}
			}
		}
		return nil
	default:
		return ErrDocumentMismatch
	}
}

// applyAuthorityUpdate integrates the checkpointed envelope write into the live
// room and fans it out. Apply reports ErrNoChanges because the update is
// integrated directly instead of through its transaction, which is what keeps
// the live item identical to the durable one.
func (s *Service) applyAuthorityUpdate(ctx context.Context, room string, update []byte) error {
	if len(update) == 0 {
		return nil
	}
	var integrateErr error
	err := s.server.Apply(ctx, room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		integrateErr = crdt.ApplyUpdateV1(doc, update, nil)
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		return err
	}
	if integrateErr != nil {
		return integrateErr
	}
	return s.server.BroadcastUpdate(ctx, room, update)
}

func setAuthorityScalar(txn *crdt.Transaction, root *crdt.YMap, key string, value any, present bool) {
	if present {
		root.Set(txn, key, value)
	} else {
		root.Delete(txn, key)
	}
}
