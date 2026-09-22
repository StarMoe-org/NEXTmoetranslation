package db

var migrationsV35ToV40 = []migration{
	{
		version: 35,
		name:    "song_lyrics_public_withdrawals",
		// Deleting a song_lyrics_publications row cannot remove a song the
		// embedded reviewed bundle also contains, so unpublishing records an
		// explicit withdrawal the public projection applies last.
		sql: `
CREATE TABLE song_lyrics_public_withdrawals (
    music_id      INTEGER PRIMARY KEY,
    withdrawn_at  INTEGER NOT NULL CHECK (withdrawn_at>0),
    withdrawn_by  TEXT NOT NULL DEFAULT ''
);
`,
	},
}
