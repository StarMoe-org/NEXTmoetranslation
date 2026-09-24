package db

// Migration v39 adds card-story and area-talk translation storage. It only
// creates new tables. Lines are keyed by their Japanese text within an
// episode; the title line has position -1. No raw script is stored:
// script_sha256 is the digest of the canonical JP script (empty until fetched)
// and jp_refetch marks a changed JP asset path. Times are unix seconds except
// released_at (unix milliseconds).
const migrationV39SideStoriesSQL = `
CREATE TABLE side_stories (
kind          TEXT NOT NULL,
story_id      TEXT NOT NULL,
title         TEXT NOT NULL DEFAULT '',
character_id  INTEGER NOT NULL DEFAULT 0,
area_id       INTEGER NOT NULL DEFAULT 0,
area_category TEXT NOT NULL DEFAULT '',
action_set_id INTEGER NOT NULL DEFAULT 0,
released_at   INTEGER NOT NULL DEFAULT 0,
updated_at    INTEGER NOT NULL DEFAULT 0,
PRIMARY KEY (kind, story_id),
CHECK ((kind='card' AND length(story_id) BETWEEN 1 AND 9 AND story_id GLOB '[1-9]*' AND story_id NOT GLOB '*[^0-9]*' AND
        action_set_id=0) OR
       (kind='area' AND length(story_id) BETWEEN 1 AND 128 AND story_id GLOB '[A-Za-z0-9]*' AND
        story_id NOT GLOB '*[^A-Za-z0-9_.-]*' AND action_set_id>0)),
CHECK (typeof(title)='text' AND typeof(area_category)='text'),
CHECK (typeof(character_id)='integer' AND character_id>=0 AND typeof(area_id)='integer' AND area_id>=0),
CHECK (typeof(action_set_id)='integer' AND typeof(released_at)='integer' AND released_at>=0),
CHECK (typeof(updated_at)='integer' AND updated_at>=0)
);
CREATE TABLE side_story_episodes (
kind            TEXT NOT NULL,
story_id        TEXT NOT NULL,
episode_key     TEXT NOT NULL,
scenario_id     TEXT NOT NULL,
title_jp        TEXT NOT NULL DEFAULT '',
position        INTEGER NOT NULL DEFAULT 0,
jp_asset_path   TEXT NOT NULL,
cn_asset_path   TEXT NOT NULL DEFAULT '',
en_asset_path   TEXT NOT NULL DEFAULT '',
script_sha256   TEXT NOT NULL DEFAULT '',
jp_fetched_at   INTEGER NOT NULL DEFAULT 0,
jp_refetch      INTEGER NOT NULL DEFAULT 0,
cn_state        TEXT NOT NULL DEFAULT 'pending',
en_state        TEXT NOT NULL DEFAULT 'pending',
attempts        INTEGER NOT NULL DEFAULT 0,
next_attempt_at INTEGER NOT NULL DEFAULT 0,
last_error      TEXT NOT NULL DEFAULT '',
updated_at      INTEGER NOT NULL DEFAULT 0,
PRIMARY KEY (kind, story_id, episode_key),
CHECK ((kind='card' AND episode_key IN ('1','2')) OR (kind='area' AND episode_key='1')),
CHECK (length(scenario_id) BETWEEN 1 AND 128 AND scenario_id GLOB '[A-Za-z0-9]*' AND scenario_id NOT GLOB '*[^A-Za-z0-9_.-]*'),
CHECK (typeof(title_jp)='text' AND typeof(position)='integer' AND position>=0),
CHECK (typeof(jp_asset_path)='text' AND jp_asset_path<>'' AND typeof(cn_asset_path)='text' AND typeof(en_asset_path)='text'),
CHECK (script_sha256='' OR (length(script_sha256)=64 AND script_sha256 NOT GLOB '*[^0-9a-f]*')),
CHECK (typeof(jp_fetched_at)='integer' AND jp_fetched_at>=0 AND jp_refetch IN (0,1)),
CHECK (cn_state IN ('pending','imported','absent','mismatch','error') AND en_state IN ('pending','imported','absent','mismatch','error')),
CHECK (typeof(attempts)='integer' AND attempts>=0 AND typeof(next_attempt_at)='integer' AND next_attempt_at>=0),
CHECK (typeof(last_error)='text' AND typeof(updated_at)='integer' AND updated_at>=0),
FOREIGN KEY (kind, story_id) REFERENCES side_stories(kind, story_id) ON DELETE CASCADE
);
CREATE TABLE side_story_lines (
kind        TEXT NOT NULL,
story_id    TEXT NOT NULL,
episode_key TEXT NOT NULL,
jp_key      TEXT NOT NULL,
role        TEXT NOT NULL,
speaker     TEXT NOT NULL DEFAULT '',
position    INTEGER NOT NULL,
PRIMARY KEY (kind, story_id, episode_key, jp_key),
CHECK (typeof(jp_key)='text' AND jp_key<>'' AND jp_key=trim(jp_key)),
CHECK (role IN ('title','talk','speaker') AND typeof(speaker)='text'),
CHECK (typeof(position)='integer' AND ((role='title' AND position=-1) OR (role<>'title' AND position>=0))),
FOREIGN KEY (kind, story_id, episode_key) REFERENCES side_story_episodes(kind, story_id, episode_key) ON DELETE CASCADE
);
CREATE TABLE side_story_line_localizations (
kind        TEXT NOT NULL,
story_id    TEXT NOT NULL,
episode_key TEXT NOT NULL,
jp_key      TEXT NOT NULL,
locale      TEXT NOT NULL,
text        TEXT NOT NULL DEFAULT '',
source      TEXT NOT NULL,
revision    INTEGER NOT NULL,
updated_by  TEXT NOT NULL DEFAULT '',
updated_at  INTEGER NOT NULL DEFAULT 0,
PRIMARY KEY (kind, story_id, episode_key, jp_key, locale),
CHECK (locale IN ('zh-CN','en-US')),
CHECK (typeof(text)='text' AND source IN ('official','llm','human')),
CHECK (typeof(revision)='integer' AND revision>0),
CHECK (typeof(updated_by)='text' AND typeof(updated_at)='integer' AND updated_at>=0),
FOREIGN KEY (kind, story_id, episode_key, jp_key) REFERENCES side_story_lines(kind, story_id, episode_key, jp_key) ON DELETE CASCADE
);
CREATE INDEX idx_side_story_line_localizations_locale ON side_story_line_localizations(locale, kind, story_id, episode_key);
`
