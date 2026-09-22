package db

const migrationV32Song682TranslationEditionsSQL = `
-- Migration v32: Promote Song 682 to source-v3 with multi-edition translations (雪莹ちゃん & 爱死天流)
DELETE FROM song_lyrics_publications WHERE music_id=682;
DELETE FROM song_lyric_segments WHERE line_id IN (SELECT line_id FROM song_lyric_lines WHERE music_id=682);
DELETE FROM song_lyric_lines WHERE music_id=682;
DELETE FROM song_lyrics WHERE music_id=682;

-- A staged or recovery import between v27 and v31 can already own this exact
-- document, and document_sha256 is UNIQUE, so the seed below runs only when
-- song 682 has no source document yet.
CREATE TEMP TABLE song_682_existing_source AS
SELECT document_id FROM song_lyrics_source_documents
WHERE music_id=682 OR document_sha256='a70872e510d9c168d183a83eacdf7ac634e6b0702785925e940698fd8cdb1886';

INSERT INTO song_lyrics_source_documents
	(music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
SELECT
	682,3,'','{"schemaVersion":3,"fixedIdentities":[{"provider":"sekaipedia","origin":"https://www.sekaipedia.org","pageId":105029,"revisionId":339560,"sha1":"468e3e8342967330ac4a4bfbb3d77d6dcf0bef20","title":"Anata Shika Mienai no","canonicalUrl":"https://www.sekaipedia.org/wiki/Anata_Shika_Mienai_no?oldid=339560","revisionTimestamp":"2026-08-10T15:50:02Z","fetchedAt":"2026-08-25T00:00:00Z","categories":["Incomplete lyrics","MORE MORE JUMP! songs","Pages using Tabber parser tag","Pre-existing songs","Songs","Songs unlocked from the Music Shop","Songs with 3D MV"],"section":"Lyrics/Full Version","renditionKey":"fixed-682-sekai-full","compositionRenditionKey":"sekai","versionReason":"untagged_full_only","indexEvidenceRefs":[{"evidenceId":"authority:sekaipedia:list-of-songs:340860:500b992c242cbf27fb7f84099272cb4aee5f3a872d77dc7ae0b711dac3673493","sha256":"b381f24fa9d584d1aa58ab9a33030e7a557293beb77e145823505ab14a86cc88"},{"evidenceId":"revision:sekaipedia:105029:339560:98a13e9877b4a9e56214c23290dc60a45ed8e49c3e8e17a10a8aafea37936ab9","sha256":"a89053572d8f41787e5160076d8556794cd4bc31d398b65a6c39577b053cfd85"}]}],"renditions":[{"renditionKey":"sekai","sourceKind":"sekai","sourceTabPaths":[["Full Version"]],"reasonCode":"untagged_full_only","sourcePerformerIds":["歌唱者-05","歌唱者-06","歌唱者-07","歌唱者-08","歌唱者-24"],"fullPerformerEvidence":"source_complete_structured","gamePerformerEvidence":"none","full":{"version":{"kind":"sekai","label":"SEKAI Version"},"performers":[{"performerId":"歌唱者-05","name":"花里みのり"},{"performerId":"歌唱者-06","name":"桐谷遥"},{"performerId":"歌唱者-07","name":"桃井愛莉"},{"performerId":"歌唱者-08","name":"日野森雫"},{"performerId":"歌唱者-24","name":"巡音ルカ"}],"rubyGeneratorVersion":"sekaipedia-ruby-kana-v2","lines":[{"id":"full-000001","text":"きっと運命の寵児","segments":[{"text":"きっと運命の寵児","performerIds":["歌唱者-08"],"ruby":[{"text":"きっと"},{"text":"運命","reading":"うんめい","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"の"},{"text":"寵児","reading":"ちょうじ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}}]}],"trailingPerformerIds":[]},{"id":"full-000002","text":"まるで魔性の物語","segments":[{"text":"まるで魔性の物語","performerIds":["歌唱者-08"],"ruby":[{"text":"まるで"},{"text":"魔性","reading":"ましょう","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"の"},{"text":"物語","reading":"ものがたり","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}}]}],"trailingPerformerIds":[]},{"id":"full-000003","text":"誰もが釘付けの一等星","segments":[{"text":"誰もが釘付けの一等星","performerIds":["歌唱者-07"],"ruby":[{"text":"誰","reading":"だれ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"もが"},{"text":"釘付","reading":"くぎづ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"けの"},{"text":"一等星","reading":"いっとうせい","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}}]}],"trailingPerformerIds":[]},{"id":"full-000004","text":"でもね","stanzaBreakBefore":true,"segments":[{"text":"でもね","performerIds":["歌唱者-06"],"ruby":[{"text":"でもね"}]}],"trailingPerformerIds":[]},{"id":"full-000005","text":"あなたも知らないあなたを","stanzaBreakBefore":true,"segments":[{"text":"あなたも知らないあなたを","performerIds":["歌唱者-06"],"ruby":[{"text":"あなたも"},{"text":"知","reading":"し","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"らないあなたを"}]}],"trailingPerformerIds":[]},{"id":"full-000006","text":"わたしは知っている","segments":[{"text":"わたしは知っている","performerIds":["歌唱者-05"],"ruby":[{"text":"わたしは"},{"text":"知","reading":"し","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"っている"}]}],"trailingPerformerIds":[]},{"id":"full-000007","text":"なのに","stanzaBreakBefore":true,"segments":[{"text":"なのに","performerIds":["歌唱者-24"],"ruby":[{"text":"なのに"}]}],"trailingPerformerIds":[]},{"id":"full-000008","text":"なのに","segments":[{"text":"なのに","performerIds":["歌唱者-05"],"ruby":[{"text":"なのに"}]}],"trailingPerformerIds":[]},{"id":"full-000009","text":"みんな好きだから","stanzaBreakBefore":true,"segments":[{"text":"みんな好きだから","performerIds":["歌唱者-24"],"ruby":[{"text":"みんな"},{"text":"好","reading":"す","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"きだから"}]}],"trailingPerformerIds":[]},{"id":"full-000010","text":"Q.E.D","segments":[{"text":"Q.E.D","performerIds":["歌唱者-24"],"ruby":[{"text":"Q.E.D"}]}],"trailingPerformerIds":[]},{"id":"full-000011","text":"あけらかん","stanzaBreakBefore":true,"segments":[{"text":"あけらかん","performerIds":["歌唱者-24"],"ruby":[{"text":"あけらかん"}]}],"trailingPerformerIds":[]},{"id":"full-000012","text":"あなたは手をひらひら","segments":[{"text":"あなたは手をひらひら","performerIds":["歌唱者-24"],"ruby":[{"text":"あなたは"},{"text":"手","reading":"て","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"をひらひら"}]}],"trailingPerformerIds":[]},{"id":"full-000013","text":"見えない壁","stanzaBreakBefore":true,"segments":[{"text":"見えない壁","performerIds":["歌唱者-06"],"ruby":[{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えない"},{"text":"壁","reading":"かべ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}}]}],"trailingPerformerIds":[]},{"id":"full-000014","text":"冷たい汗","segments":[{"text":"冷たい汗","performerIds":["歌唱者-06"],"ruby":[{"text":"冷","reading":"つめ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"たい"},{"text":"汗","reading":"あせ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}}]}],"trailingPerformerIds":[]},{"id":"full-000015","text":"首筋伝う","segments":[{"text":"首筋伝う","performerIds":["歌唱者-06"],"ruby":[{"text":"首筋","reading":"くびすじ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"伝","reading":"つた","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"う"}]}],"trailingPerformerIds":[]},{"id":"full-000016","text":"あなたしか見えないの（見えないの）","stanzaBreakBefore":true,"segments":[{"text":"あなたしか見えないの（見えないの）","performerIds":["歌唱者-07"],"ruby":[{"text":"あなたしか"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの（"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの）"}]}],"trailingPerformerIds":[]},{"id":"full-000017","text":"有象無象の世迷言？","segments":[{"text":"有象無象の世迷言？","performerIds":["歌唱者-08"],"ruby":[{"text":"有象無象","reading":"うぞうむぞう","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"の"},{"text":"世迷言","reading":"よまいごと","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"？"}]}],"trailingPerformerIds":[]},{"id":"full-000018","text":"一人、また一人蕩かして","segments":[{"text":"一人、また一人蕩かして","performerIds":["歌唱者-05"],"ruby":[{"text":"一人","reading":"ひとり","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"、また"},{"text":"一人","reading":"ひとり","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"蕩","reading":"とろ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"かして"}]}],"trailingPerformerIds":[]},{"id":"full-000019","text":"嗚呼、茹っていく","segments":[{"text":"嗚呼、茹っていく","performerIds":["歌唱者-05"],"ruby":[{"text":"嗚呼","reading":"ああ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"、"},{"text":"茹","reading":"うだ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"っていく"}]}],"trailingPerformerIds":[]},{"id":"full-000020","text":"クセになっちゃうわ","stanzaBreakBefore":true,"segments":[{"text":"クセになっちゃうわ","performerIds":["歌唱者-06"],"ruby":[{"text":"クセになっちゃうわ"}]}],"trailingPerformerIds":[]},{"id":"full-000021","text":"あなたしか見えないの","stanzaBreakBefore":true,"segments":[{"text":"あなたしか見えないの","performerIds":["歌唱者-05","歌唱者-06","歌唱者-07","歌唱者-08","歌唱者-24"],"ruby":[{"text":"あなたしか"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの"}]}],"trailingPerformerIds":[]},{"id":"full-000022","text":"あなたしか見えないの","segments":[{"text":"あなたしか見えないの","performerIds":["歌唱者-05"],"ruby":[{"text":"あなたしか"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの"}]}],"trailingPerformerIds":[]},{"id":"full-000023","text":"なんであなたしか見えないの？","segments":[{"text":"なんであなたしか見えないの？","performerIds":["歌唱者-08"],"ruby":[{"text":"なんであなたしか"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの？"}]}],"trailingPerformerIds":[]},{"id":"full-000024","text":"あなたしか見えないの","stanzaBreakBefore":true,"segments":[{"text":"あなたしか見えないの","performerIds":["歌唱者-05","歌唱者-06","歌唱者-07","歌唱者-08","歌唱者-24"],"ruby":[{"text":"あなたしか"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの"}]}],"trailingPerformerIds":[]},{"id":"full-000025","text":"蠱毒巣食った呪ひ言","segments":[{"text":"蠱毒巣食った呪ひ言","performerIds":["歌唱者-05","歌唱者-06","歌唱者-07","歌唱者-08","歌唱者-24"],"ruby":[{"text":"蠱毒","reading":"こどく","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"巣食","reading":"すく","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"った"},{"text":"呪","reading":"のろ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"ひ"},{"text":"言","reading":"ごと","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}}]}],"trailingPerformerIds":[]},{"id":"full-000026","text":"此方は気にせずお幸せに","segments":[{"text":"此方は気にせずお幸せに","performerIds":["歌唱者-05","歌唱者-06","歌唱者-07","歌唱者-08","歌唱者-24"],"ruby":[{"text":"此方","reading":"こちら","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"は"},{"text":"気","reading":"き","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"にせずお"},{"text":"幸","reading":"しあわ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"せに"}]}],"trailingPerformerIds":[]},{"id":"full-000027","text":"嗚呼、堕ちていく","segments":[{"text":"嗚呼、堕ちていく","performerIds":["歌唱者-05","歌唱者-06","歌唱者-07","歌唱者-08","歌唱者-24"],"ruby":[{"text":"嗚呼","reading":"ああ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"、"},{"text":"堕","reading":"お","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"ちていく"}]}],"trailingPerformerIds":[]},{"id":"full-000028","text":"あなたしか見えないの（見えないの）","stanzaBreakBefore":true,"segments":[{"text":"あなたしか見えないの（見えないの）","performerIds":["歌唱者-06"],"ruby":[{"text":"あなたしか"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの（"},{"text":"見","reading":"み","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"えないの）"}]}],"trailingPerformerIds":[]},{"id":"full-000029","text":"有象無象のひとりなら","segments":[{"text":"有象無象のひとりなら","performerIds":["歌唱者-07"],"ruby":[{"text":"有象無象","reading":"うぞうむぞう","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"のひとりなら"}]}],"trailingPerformerIds":[]},{"id":"full-000030","text":"あなたの「特別」に成れたのね！","segments":[{"text":"あなたの「特別」に成れたのね！","performerIds":["歌唱者-24"],"ruby":[{"text":"あなたの「"},{"text":"特別","reading":"とくべつ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"」に"},{"text":"成","reading":"な","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"れたのね！"}]}],"trailingPerformerIds":[]},{"id":"full-000031","text":"嗚呼、素敵だわ","segments":[{"text":"嗚呼、素敵だわ","performerIds":["歌唱者-24"],"ruby":[{"text":"嗚呼","reading":"ああ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"、"},{"text":"素敵","reading":"すてき","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"だわ"}]}],"trailingPerformerIds":[]},{"id":"full-000032","text":"クセになっちゃうわ","stanzaBreakBefore":true,"segments":[{"text":"クセになっちゃうわ","performerIds":["歌唱者-05","歌唱者-06"],"ruby":[{"text":"クセになっちゃうわ"}]}],"trailingPerformerIds":[]},{"id":"full-000033","text":"目も当てられないわ","stanzaBreakBefore":true,"segments":[{"text":"目も当てられないわ","performerIds":["歌唱者-07","歌唱者-08"],"ruby":[{"text":"目","reading":"め","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"も"},{"text":"当","reading":"あ","readingEvidence":{"kind":"deterministic_dictionary","generatorVersion":"kagome-ipadic-han-kana-v2"}},{"text":"てられないわ"}]}],"trailingPerformerIds":[]}]},"relation":{"kind":"none"},"provenance":{"fullText":{"renditionKey":"fixed-682-sekai-full"},"fullPerformerSegmentation":{"renditionKey":"fixed-682-sekai-full"},"relationEvidence":{"renditionKey":"fixed-682-sekai-full"},"versionEvidence":{"renditionKey":"fixed-682-sekai-full"}}}]}','a70872e510d9c168d183a83eacdf7ac634e6b0702785925e940698fd8cdb1886','0000000000000000000000000000000000000000000000000000000000000000',1724544000
WHERE EXISTS (SELECT 1 FROM catalog_music WHERE music_id=682)
  AND NOT EXISTS (SELECT 1 FROM song_682_existing_source);

CREATE TEMP VIEW song_682_seeded_source AS
SELECT document_id FROM song_lyrics_source_documents
WHERE music_id=682 AND NOT EXISTS (SELECT 1 FROM song_682_existing_source);

INSERT INTO song_lyrics_source_artifacts
	(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
	 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
	 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
SELECT
	document_id,'sekaipedia','fixed-682-sekai-full','https://www.sekaipedia.org',105029,339560,'2026-08-10T15:50:02Z',
	'468e3e8342967330ac4a4bfbb3d77d6dcf0bef20','Anata Shika Mienai no','https://www.sekaipedia.org/wiki/Anata_Shika_Mienai_no?oldid=339560',
	'2026-08-25T00:00:00Z','["Incomplete lyrics","MORE MORE JUMP! songs","Pages using Tabber parser tag","Pre-existing songs","Songs","Songs unlocked from the Music Shop","Songs with 3D MV"]','Lyrics/Full Version','sekai','untagged_full_only',
	'[{"evidenceId":"authority:sekaipedia:list-of-songs:340860:500b992c242cbf27fb7f84099272cb4aee5f3a872d77dc7ae0b711dac3673493","sha256":"b381f24fa9d584d1aa58ab9a33030e7a557293beb77e145823505ab14a86cc88"},{"evidenceId":"revision:sekaipedia:105029:339560:98a13e9877b4a9e56214c23290dc60a45ed8e49c3e8e17a10a8aafea37936ab9","sha256":"a89053572d8f41787e5160076d8556794cd4bc31d398b65a6c39577b053cfd85"}]','{"provider":"sekaipedia","origin":"https://www.sekaipedia.org","pageId":105029,"revisionId":339560,"sha1":"468e3e8342967330ac4a4bfbb3d77d6dcf0bef20","title":"Anata Shika Mienai no","canonicalUrl":"https://www.sekaipedia.org/wiki/Anata_Shika_Mienai_no?oldid=339560","revisionTimestamp":"2026-08-10T15:50:02Z","fetchedAt":"2026-08-25T00:00:00Z","categories":["Incomplete lyrics","MORE MORE JUMP! songs","Pages using Tabber parser tag","Pre-existing songs","Songs","Songs unlocked from the Music Shop","Songs with 3D MV"],"section":"Lyrics/Full Version","renditionKey":"fixed-682-sekai-full","compositionRenditionKey":"sekai","versionReason":"untagged_full_only","indexEvidenceRefs":[{"evidenceId":"authority:sekaipedia:list-of-songs:340860:500b992c242cbf27fb7f84099272cb4aee5f3a872d77dc7ae0b711dac3673493","sha256":"b381f24fa9d584d1aa58ab9a33030e7a557293beb77e145823505ab14a86cc88"},{"evidenceId":"revision:sekaipedia:105029:339560:98a13e9877b4a9e56214c23290dc60a45ed8e49c3e8e17a10a8aafea37936ab9","sha256":"a89053572d8f41787e5160076d8556794cd4bc31d398b65a6c39577b053cfd85"}]}','b7c3386e43f6dc10b54e83a2e2e8ee7a43028f45d127e905ba1cace545275675',1,'0000000000000000000000000000000000000000000000000000000000000000','0000000000000000000000000000000000000000000000000000000000000000'
FROM song_682_seeded_source;

INSERT INTO song_lyrics_component_contributions
	(document_id,component,rendition_key,contribution_sha256)
SELECT document_id,'renditions/sekai/full_text','fixed-682-sekai-full','a9d5cf0167994a08836b7521f208395ee339866f4a63a1927642da99dc1abcc6'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_component_contributions
	(document_id,component,rendition_key,contribution_sha256)
SELECT document_id,'renditions/sekai/full_performer_segmentation','fixed-682-sekai-full','fbc75fb2def36b4abb13c54d63427e7f9ff4a7cefe34a8e2eec785474048ceaf'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_component_contributions
	(document_id,component,rendition_key,contribution_sha256)
SELECT document_id,'renditions/sekai/relation','fixed-682-sekai-full','db30e2d7481554e8e1930ca6d6039450454dcc64b50d5d8568629bf6cfa5643d'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_component_contributions
	(document_id,component,rendition_key,contribution_sha256)
SELECT document_id,'renditions/sekai/version','fixed-682-sekai-full','ccff6237ecbcb250a7e951f428835f8b3e979bbe5416e6fc2accbe4fc1f07f2e'
FROM song_682_seeded_source;

INSERT INTO song_lyrics_rendition_localizations
	(document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision)
SELECT
	document_id,'sekai','zh-CN','@雪莹ちゃん','',1724544000,'system',9
FROM song_682_seeded_source;

INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',0,'一定是命中注定的天之骄子'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',1,'宛如充满魔性魅力的故事篇章'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',2,'令任谁都目不转睛的耀眼一等星'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',3,'可是啊'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',4,'连你自己都不曾知晓的那个你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',5,'唯独我心知肚明'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',6,'明明如此'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',7,'明明如此'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',8,'因为大家全都喜欢你啊'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',9,'证明完毕 Q.E.D'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',10,'若无其事神色自若'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',11,'你轻轻挥动着双手'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',12,'看不见的厚重壁垒'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',13,'冰冷刺骨的冷汗'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',14,'顺着后颈缓缓滑落'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',15,'我的眼里唯独只有你（唯独只有你）'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',16,'难道不过是乌合之众的胡言乱语？'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',17,'将一人、又一人彻底俘获融化'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',18,'啊啊，逐渐滚烫燥热起来'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',19,'简直要让人彻底上瘾了呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',20,'我的眼里唯独只有你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',21,'我的眼里唯独只有你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',22,'究竟为何我的眼里唯独只有你？'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',23,'我的眼里唯独只有你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',24,'盘踞蛊毒的恶毒咒语'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',25,'无需顾虑我这边 请务必幸福哦'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',26,'啊啊，逐渐沉沦堕落'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',27,'我的眼里唯独只有你（唯独只有你）'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',28,'若沦为乌合之众中的一员'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',29,'是不是就能成为你的「特别存在」了呢！'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',30,'啊啊，真是美妙绝伦呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',31,'简直要让人彻底上瘾了呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_rendition_translation_lines
	(document_id,rendition_key,locale,position,text)
SELECT document_id,'sekai','zh-CN',32,'真是让人不忍直视惨不忍睹呢'
FROM song_682_seeded_source;

INSERT INTO song_lyrics_translation_editions
	(document_id,edition_key,label,created_at,created_by)
SELECT document_id,'main','雪莹ちゃん',1724544000,'system'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_editions
	(document_id,edition_key,label,created_at,created_by)
SELECT document_id,'aishitenryu','爱死天流',1724544000,'system'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_state
	(document_id,default_edition_key,revision,updated_at,updated_by)
SELECT document_id,'main',9,1724544000,'system'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_localizations
	(document_id,edition_key,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by)
SELECT document_id,'main','sekai','zh-CN','@雪莹ちゃん','',1724544000,'system'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_localizations
	(document_id,edition_key,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by)
SELECT document_id,'aishitenryu','sekai','zh-CN','@爱死天流','',1724544000,'system'
FROM song_682_seeded_source;

INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',0,'一定是命中注定的天之骄子'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',1,'宛如充满魔性魅力的故事篇章'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',2,'令任谁都目不转睛的耀眼一等星'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',3,'可是啊'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',4,'连你自己都不曾知晓的那个你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',5,'唯独我心知肚明'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',6,'明明如此'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',7,'明明如此'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',8,'因为大家全都喜欢你啊'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',9,'证明完毕 Q.E.D'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',10,'若无其事神色自若'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',11,'你轻轻挥动着双手'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',12,'看不见的厚重壁垒'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',13,'冰冷刺骨的冷汗'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',14,'顺着后颈缓缓滑落'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',15,'我的眼里唯独只有你（唯独只有你）'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',16,'难道不过是乌合之众的胡言乱语？'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',17,'将一人、又一人彻底俘获融化'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',18,'啊啊，逐渐滚烫燥热起来'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',19,'简直要让人彻底上瘾了呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',20,'我的眼里唯独只有你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',21,'我的眼里唯独只有你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',22,'究竟为何我的眼里唯独只有你？'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',23,'我的眼里唯独只有你'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',24,'盘踞蛊毒的恶毒咒语'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',25,'无需顾虑我这边 请务必幸福哦'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',26,'啊啊，逐渐沉沦堕落'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',27,'我的眼里唯独只有你（唯独只有你）'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',28,'若沦为乌合之众中的一员'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',29,'是不是就能成为你的「特别存在」了呢！'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',30,'啊啊，真是美妙绝伦呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',31,'简直要让人彻底上瘾了呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'main','sekai','full','zh-CN',32,'真是让人不忍直视惨不忍睹呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',0,'定是命运的宠儿'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',1,'恍如一则魔性的故事（story）'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',2,'人人都深深着迷的一等星'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',3,'可是啊'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',4,'那个连你都不了解的你自己'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',5,'我却了然于胸呀'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',6,'然而'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',7,'然而'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',8,'毕竟你人见人爱'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',9,'故就此证毕.'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',10,'若无其事状'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',11,'你向我轻轻挥了挥手'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',12,'看不见的一道墙'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',13,'脖子上的冷汗'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',14,'刷刷往下淌'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',15,'在我眼中仅有你一人'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',16,'可是泛泛之辈的痴人说梦？'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',17,'令人一个接一个神魂颠倒'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',18,'啊啊，要闷到发晕了'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',19,'如痴如醉欲罢不能呢'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',20,'在我眼中仅有你一人'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',21,'在我眼中仅有你一人'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',22,'为何我眼中仅有你一人？'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',23,'在我眼中仅有你一人'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',24,'是久经蛊毒洗练的咒骂之辞（是救赎孤独的咒骂之辞）'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',25,'不必介怀我这一隅 祝你幸福'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',26,'啊啊，逐渐为此沉沦'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',27,'在我眼中仅有你一人'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',28,'若是芸芸众生里平凡的一员'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',29,'那我也是成为属于你的“特别”了呢！'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',30,'啊啊，太美妙了'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',31,'如痴如醉欲罢不能啊'
FROM song_682_seeded_source;
INSERT INTO song_lyrics_translation_edition_lines
	(document_id,edition_key,rendition_key,side,locale,position,text)
SELECT document_id,'aishitenryu','sekai','full','zh-CN',32,'光彩夺目美不胜收啊'
FROM song_682_seeded_source;

DROP VIEW song_682_seeded_source;
DROP TABLE song_682_existing_source;
`

const MigrationV32Song682TranslationEditionsSQL = migrationV32Song682TranslationEditionsSQL
