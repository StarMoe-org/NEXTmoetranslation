package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Public lyrics withdrawals are the positive record that an operator took a
// song off the public site. Deleting the song_lyrics_publications row is not
// enough on its own: the reviewed runtime bundle embedded in the binary still
// contains the song, and the projection overlay would keep serving the bundle
// entry. Publish and unpublish maintain the marker inside their own
// transaction so a publication row and a withdrawal never coexist.

func recordPublicLyricsWithdrawalTx(tx *sql.Tx, musicID int, actor string, now int64) (bool, error) {
	result, err := tx.Exec(`INSERT OR IGNORE INTO song_lyrics_public_withdrawals(music_id, withdrawn_at, withdrawn_by)
		VALUES (?, ?, ?)`, musicID, now, actor)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

func clearPublicLyricsWithdrawalTx(tx *sql.Tx, musicID int) (bool, error) {
	result, err := tx.Exec(`DELETE FROM song_lyrics_public_withdrawals WHERE music_id=?`, musicID)
	if err != nil {
		return false, err
	}
	removed, err := result.RowsAffected()
	return removed == 1, err
}

// SetSourceV3LyricsWithdrawn withdraws a source-v3 song from the public site or
// lifts its withdrawal. Source-v3 songs have no publication row, so the marker
// alone decides whether the projection serves them. revision must match the
// default translation edition document the editor loaded.
func (s *Store) SetSourceV3LyricsWithdrawn(musicID, revision int, withdrawn bool, users ...string) (LyricsRenditionDocument, bool, error) {
	unlock := s.lockLyrics(musicID)
	defer unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return LyricsRenditionDocument{}, false, err
	}
	defer tx.Rollback()
	_, _, current, err := loadLyricsRenditionMutationTx(tx, LyricsRenditionDocument{MusicID: musicID})
	if err != nil {
		return LyricsRenditionDocument{}, false, err
	}
	if revision != current.Revision {
		copy := current
		return LyricsRenditionDocument{}, false, &LyricsRenditionContractError{Code: "revision_conflict", Current: &copy}
	}
	now := time.Now().Unix()
	action := "lyrics.publish"
	var changed bool
	if withdrawn {
		action = "lyrics.unpublish"
		changed, err = recordPublicLyricsWithdrawalTx(tx, musicID, optionalActor(users), now)
	} else {
		changed, err = clearPublicLyricsWithdrawalTx(tx, musicID)
	}
	if err != nil {
		return LyricsRenditionDocument{}, false, err
	}
	if !changed {
		return current, false, nil
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts, user, action, detail) VALUES (?, ?, ?, ?)`,
		now, optionalActor(users), action, fmt.Sprintf("musicId=%d revision=%d sourceV3=true", musicID, revision)); err != nil {
		return LyricsRenditionDocument{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return LyricsRenditionDocument{}, false, err
	}
	s.NotifyChange()
	return current, true, nil
}

// PublicLyricsWithdrawals returns the music IDs the public projection must omit
// from the index and from per-song detail routes.
func (s *Store) PublicLyricsWithdrawals() (map[int]bool, error) {
	rows, err := s.db.Query(`SELECT music_id FROM song_lyrics_public_withdrawals ORDER BY music_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	withdrawn := map[int]bool{}
	for rows.Next() {
		var musicID int
		if err := rows.Scan(&musicID); err != nil {
			return nil, err
		}
		withdrawn[musicID] = true
	}
	return withdrawn, rows.Err()
}
