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
	{
		version: 36,
		name:    "lyrics_provider_page_targets",
		// The reviewed Sekaipedia music-ID-to-page-title and contributor-alias
		// maps move out of the binary so a new song can be bound without a
		// redeploy. The seed is the map v35 deployments compiled in; music 728
		// is absent from it because that Sekaipedia revision's lyrics table is
		// empty.
		sql: `
CREATE TABLE lyrics_provider_page_targets (
    provider            TEXT NOT NULL CHECK (provider<>''),
    music_id            INTEGER NOT NULL CHECK (music_id>0),
    page_title          TEXT NOT NULL CHECK (page_title<>''),
    resolved_page_title TEXT NOT NULL DEFAULT '',
    updated_at          INTEGER NOT NULL CHECK (updated_at>0),
    updated_by          TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider, music_id)
);
CREATE TABLE lyrics_provider_contributor_aliases (
    provider             TEXT NOT NULL CHECK (provider<>''),
    music_id             INTEGER NOT NULL CHECK (music_id>0),
    catalog_contributor  TEXT NOT NULL CHECK (catalog_contributor<>''),
    provider_contributor TEXT NOT NULL CHECK (provider_contributor<>''),
    updated_at           INTEGER NOT NULL CHECK (updated_at>0),
    updated_by           TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider, music_id, catalog_contributor)
);
INSERT INTO lyrics_provider_page_targets(provider,music_id,page_title,resolved_page_title,updated_at,updated_by) VALUES
    ('sekaipedia',50,'Blessing','',1790000000,'migration-v36'),
    ('sekaipedia',69,'Fragile','',1790000000,'migration-v36'),
    ('sekaipedia',95,'Ifuudoudou','',1790000000,'migration-v36'),
    ('sekaipedia',131,'Hatsune Miku no Gekishou','',1790000000,'migration-v36'),
    ('sekaipedia',138,'KING','',1790000000,'migration-v36'),
    ('sekaipedia',141,'Gunjou Sanka','',1790000000,'migration-v36'),
    ('sekaipedia',148,'ray','Ray',1790000000,'migration-v36'),
    ('sekaipedia',186,'Hatsune Tenchikaibyaku Shinwa','',1790000000,'migration-v36'),
    ('sekaipedia',222,'Piano×Forte×Scandal','',1790000000,'migration-v36'),
    ('sekaipedia',245,'Aun no Beats','',1790000000,'migration-v36'),
    ('sekaipedia',295,'Float Planner','',1790000000,'migration-v36'),
    ('sekaipedia',334,'Vampire''s ∞ pathoS','',1790000000,'migration-v36'),
    ('sekaipedia',353,'Kitty','',1790000000,'migration-v36'),
    ('sekaipedia',386,'Kirapipi★Kirapika','',1790000000,'migration-v36'),
    ('sekaipedia',402,'Envy Baby','',1790000000,'migration-v36'),
    ('sekaipedia',475,'Chigau!!!','',1790000000,'migration-v36'),
    ('sekaipedia',499,'Konton Boogie','',1790000000,'migration-v36'),
    ('sekaipedia',515,'Igaku','',1790000000,'migration-v36'),
    ('sekaipedia',555,'Fusion','',1790000000,'migration-v36'),
    ('sekaipedia',560,'Eyelid','',1790000000,'migration-v36'),
    ('sekaipedia',562,'Ángel','',1790000000,'migration-v36'),
    ('sekaipedia',583,'Accelerate','',1790000000,'migration-v36'),
    ('sekaipedia',592,'Queen of Hearts (song)','',1790000000,'migration-v36'),
    ('sekaipedia',608,'Hoshizora Melancholia','',1790000000,'migration-v36'),
    ('sekaipedia',621,'Tokyo Summer Session','',1790000000,'migration-v36'),
    ('sekaipedia',635,'Ari no Mama no Story o','',1790000000,'migration-v36'),
    ('sekaipedia',647,'SANchi Chokusou','',1790000000,'migration-v36'),
    ('sekaipedia',649,'Sayonara Tengoku Mata Kite Jigoku','',1790000000,'migration-v36'),
    ('sekaipedia',682,'Anata Shika Mienai no','',1790000000,'migration-v36'),
    ('sekaipedia',692,'Vocalo-Colosseum','',1790000000,'migration-v36'),
    ('sekaipedia',750,'Losstime Memory','',1790000000,'migration-v36'),
    ('sekaipedia',751,'Additional Memory','',1790000000,'migration-v36'),
    ('sekaipedia',752,'Ayano no Koufuku Riron','',1790000000,'migration-v36'),
    ('sekaipedia',753,'Kuusou Forest','',1790000000,'migration-v36'),
    ('sekaipedia',756,'Gimme more!','',1790000000,'migration-v36'),
    ('sekaipedia',764,'Otsukimi Recital','',1790000000,'migration-v36'),
    ('sekaipedia',789,'Tenbin, Yubisaki de Furete','',1790000000,'migration-v36');
INSERT INTO lyrics_provider_contributor_aliases(provider,music_id,catalog_contributor,provider_contributor,updated_at,updated_by) VALUES
    ('sekaipedia',69,'ぬゆり','nulut',1790000000,'migration-v36'),
    ('sekaipedia',95,'梅とら','Umetora',1790000000,'migration-v36'),
    ('sekaipedia',131,'cosMo@暴走P','cosMo@BousouP',1790000000,'migration-v36'),
    ('sekaipedia',148,'藤原 基央','Motoo Fujiwara',1790000000,'migration-v36'),
    ('sekaipedia',186,'cosMo@暴走P','cosMo@BousouP',1790000000,'migration-v36'),
    ('sekaipedia',245,'羽生まゐご','Hanyuu Maigo',1790000000,'migration-v36'),
    ('sekaipedia',334,'ひとしずく','Hitoshizuku',1790000000,'migration-v36'),
    ('sekaipedia',334,'やま△','Yama△',1790000000,'migration-v36'),
    ('sekaipedia',353,'ツミキ','Tsumiki',1790000000,'migration-v36'),
    ('sekaipedia',386,'nyanyannya(大天才P)','nyanyannya',1790000000,'migration-v36'),
    ('sekaipedia',475,'カルロス袴田(サイゼP)','Carlos Hakamada',1790000000,'migration-v36'),
    ('sekaipedia',515,'原口沙輔','Haraguchi Sasuke',1790000000,'migration-v36'),
    ('sekaipedia',555,'DECO*27 (OTOIRO)','DECO*27',1790000000,'migration-v36'),
    ('sekaipedia',555,'tepe (OTOIRO)','tepe',1790000000,'migration-v36'),
    ('sekaipedia',560,'ぬゆり','nulut',1790000000,'migration-v36'),
    ('sekaipedia',562,'かいりきベア','Kairiki Bear',1790000000,'migration-v36'),
    ('sekaipedia',583,'吉田夜世','Yoshida Yasei',1790000000,'migration-v36'),
    ('sekaipedia',592,'奏音69','Kanon69',1790000000,'migration-v36'),
    ('sekaipedia',635,'のぼる↑','Noboru↑',1790000000,'migration-v36'),
    ('sekaipedia',750,'じん','JIN',1790000000,'migration-v36'),
    ('sekaipedia',751,'じん','JIN',1790000000,'migration-v36'),
    ('sekaipedia',752,'じん','JIN',1790000000,'migration-v36'),
    ('sekaipedia',753,'じん','JIN',1790000000,'migration-v36'),
    ('sekaipedia',756,'めろくる','Mellowcle',1790000000,'migration-v36'),
    ('sekaipedia',764,'じん','JIN',1790000000,'migration-v36'),
    ('sekaipedia',789,'卯花ロク','Uka Roku',1790000000,'migration-v36');
`,
	},
	{
		version: 37,
		name:    "lyrics_source_artifacts_projectsekai_fandom_origin",
		sql:     migrationV37ProjectSekaiFandomOriginSQL,
	},
	{
		version: 38,
		name:    "lyrics_recovery_takeovers",
		sql:     migrationV38LyricsRecoveryTakeoversSQL,
	},
}
