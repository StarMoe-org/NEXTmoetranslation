package store

import (
	"context"
	"testing"
	"time"
)

// TestSideStoryProjectionAndListScale seeds more than the production volume
// (3000 stories × 2 episodes × 40 lines plus titles, both locales) and bounds
// the public projection and the list.
func TestSideStoryProjectionAndListScale(t *testing.T) {
	s := newSideStoryTestStore(t)
	started := time.Now()
	for _, statement := range []string{
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<3000)
		INSERT INTO side_stories(kind,story_id,title,character_id,released_at,updated_at)
		SELECT 'card',CAST(i AS TEXT),'テスト称号'||i,1+i%26,i*1000,1 FROM n`,
		`INSERT INTO side_story_episodes(kind,story_id,episode_key,scenario_id,title_jp,position,jp_asset_path,cn_asset_path,
			script_sha256,cn_state,en_state)
		SELECT s.kind,s.story_id,k.episode_key,'test_card_'||s.story_id||'_0'||k.episode_key,'テスト話'||s.story_id||'-'||k.episode_key,
			CAST(k.episode_key AS INTEGER),'character/member/test/'||s.story_id,'cn/'||s.story_id,
			'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','imported','absent'
		FROM side_stories s CROSS JOIN (SELECT '1' AS episode_key UNION ALL SELECT '2') k`,
		`INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,speaker,position)
		SELECT kind,story_id,episode_key,title_jp,'title','',-1 FROM side_story_episodes`,
		`WITH RECURSIVE m(j) AS (SELECT 0 UNION ALL SELECT j+1 FROM m WHERE j<39)
		INSERT INTO side_story_lines(kind,story_id,episode_key,jp_key,role,speaker,position)
		SELECT e.kind,e.story_id,e.episode_key,'テスト台詞'||e.story_id||'-'||e.episode_key||'-'||m.j,
			CASE WHEN m.j%5=0 THEN 'speaker' ELSE 'talk' END,CASE WHEN m.j%5=0 THEN '' ELSE 'テスト話者' END,m.j*2
		FROM side_story_episodes e CROSS JOIN m`,
		`INSERT INTO side_story_line_localizations(kind,story_id,episode_key,jp_key,locale,text,source,revision,updated_by,updated_at)
		SELECT l.kind,l.story_id,l.episode_key,l.jp_key,loc.locale,'测试译文'||l.position,
			CASE l.position%3 WHEN 0 THEN 'official' WHEN 1 THEN 'llm' ELSE 'human' END,1,'seed',1+l.position
		FROM side_story_lines l CROSS JOIN (SELECT 'zh-CN' AS locale UNION ALL SELECT 'en-US') loc`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM side_story_line_localizations`).Scan(&rows); err != nil || rows != 2*3000*2*41 {
		t.Fatalf("seeded translation rows=%d err=%v", rows, err)
	}
	t.Logf("seeded %d translation rows in %s", rows, time.Since(started))

	ctx := context.Background()
	started = time.Now()
	files, err := s.SideStoryPublicFilesContext(ctx, "zh-CN")
	projection := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3000 || len(files["cardStory/card_1.json"].Episodes) != 2 || len(files["cardStory/card_1.json"].Episodes[1].Talk) != 40 {
		t.Fatalf("projection files=%d card_1=%+v", len(files), files["cardStory/card_1.json"].Episodes)
	}
	started = time.Now()
	list, err := s.ListSideStoriesContext(ctx, "card", "en-US")
	listing := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3000 || list[0].ID != "3000" || list[0].LineCount != 82 || list[0].TranslatedCount != 82 || list[0].Status != "translated" {
		t.Fatalf("list size=%d first=%+v", len(list), list[0])
	}
	t.Logf("SideStoryPublicFilesContext %s, ListSideStoriesContext %s", projection, listing)
	if projection > 5*time.Second || listing > 5*time.Second {
		t.Fatalf("projection %s or list %s exceeds 5s", projection, listing)
	}
}
