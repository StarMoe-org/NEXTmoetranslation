package store

import "database/sql"

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
