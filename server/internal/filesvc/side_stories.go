package filesvc

import (
	"context"
	"fmt"
	"time"

	"moesekai/server/internal/model"
)

// sideStoryRoots lists the only locales side-story files are published in and
// the prefix each is served under; there is no v2/zh-CN or v2/ja-JP mirror.
var sideStoryRoots = []struct{ locale, prefix string }{
	{model.LocaleChinese, "translation/"},
	{model.LocaleEnglish, "v2/en-US/translation/"},
}

// addSideStoryAssets projects every side-story file into next, one pass per
// locale.
func (svc *Service) addSideStoryAssets(ctx context.Context, next map[string]asset, now time.Time) error {
	for _, root := range sideStoryRoots {
		if err := ctx.Err(); err != nil {
			return err
		}
		bodies, err := svc.gen.SideStoryFilesJSON(ctx, root.locale)
		if err != nil {
			return fmt.Errorf("side stories %s: %w", root.locale, err)
		}
		for key, body := range bodies {
			next[root.prefix+key] = makeAsset(body, "application/json; charset=utf-8", now)
		}
	}
	return nil
}

// RebuildSideStory incrementally republishes the file holding one side story.
func (svc *Service) RebuildSideStory(kind, storyID string) error {
	return svc.RebuildSideStoryContext(svc.ctx, kind, storyID)
}

// RebuildSideStoryContext republishes, in both locales, the file holding one
// side story, and withdraws it where it no longer has a translated line.
func (svc *Service) RebuildSideStoryContext(ctx context.Context, kind, storyID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	releaseContent, err := svc.store.LockContentSharedContext(ctx)
	if err != nil {
		return err
	}
	defer releaseContent()
	svc.sideStoryPublishMu.Lock()
	defer svc.sideStoryPublishMu.Unlock()

	now := time.Now()
	updates := make(map[string]asset, len(sideStoryRoots))
	var removed []string
	for _, root := range sideStoryRoots {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, body, ok, err := svc.gen.SideStoryFileForStoryJSON(ctx, kind, storyID, root.locale)
		if err != nil {
			return fmt.Errorf("side story %s/%s %s: %w", kind, storyID, root.locale, err)
		}
		if ok {
			updates[root.prefix+key] = makeAsset(body, "application/json; charset=utf-8", now)
		} else {
			removed = append(removed, root.prefix+key)
		}
	}
	svc.applyIncremental(updates, removed...)
	return nil
}
