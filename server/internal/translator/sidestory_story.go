package translator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"moesekai/server/internal/store"
)

// beginSideStoryWrite takes editor access and the shared content lock for one
// side-story write. It fails with editorgate.ErrProducerRunning while a
// producer job or restore runs, so a write is never interleaved with one.
func (t *Translator) beginSideStoryWrite(ctx context.Context) (func(), error) {
	t.mu.Lock()
	gate := t.editorGate
	t.mu.Unlock()
	releaseEditor := func() {}
	if gate != nil {
		release, err := gate.BeginEditorContext(ctx)
		if err != nil {
			return nil, err
		}
		releaseEditor = release
	}
	releaseContent, err := t.store.LockContentSharedContext(ctx)
	if err != nil {
		releaseEditor()
		return nil, err
	}
	return func() {
		releaseContent()
		releaseEditor()
	}, nil
}

func (t *Translator) applySideStoryFetchesContext(ctx context.Context, fetches []store.SideStoryEpisodeFetch, now time.Time) (store.SideStoryApplyResult, error) {
	release, err := t.beginSideStoryWrite(ctx)
	if err != nil {
		return store.SideStoryApplyResult{}, err
	}
	defer release()
	return t.store.ApplySideStoryFetchesContext(ctx, fetches, now)
}

// RefreshSideStoryContext fetches the JP, CN and EN scripts of every episode
// of one story now and applies them. It returns sql.ErrNoRows for an unknown
// story and ErrSideStoryUpstreamUnavailable when no JP script could be fetched
// because the sources failed. A JP fetch failure is stored only on an episode
// the backfill still queues; on any other it is only returned.
func (t *Translator) RefreshSideStoryContext(ctx context.Context, kind, storyID string) ([]store.SideStoryEpisodeApply, error) {
	items, err := t.store.SideStoryEpisodesContext(ctx, kind, storyID)
	if err != nil {
		return nil, err
	}
	pacer := &sideStoryPacer{}
	fetches := make([]store.SideStoryEpisodeFetch, 0, len(items))
	fetched, failures := 0, []string{}
	for _, item := range items {
		fetch, err := t.fetchSideStoryEpisodeContext(ctx, pacer, item, true, true)
		if err != nil {
			return nil, err
		}
		if fetch.JP.Script != nil {
			fetched++
		} else if fetch.JP.Err != "" {
			failures = append(failures, fmt.Sprintf("episode %s: %s", item.EpisodeKey, fetch.JP.Err))
		}
		fetches = append(fetches, fetch)
	}
	result, err := t.applySideStoryFetchesContext(ctx, fetches, time.Now())
	if err != nil {
		return nil, err
	}
	if fetched == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrSideStoryUpstreamUnavailable, strings.Join(failures, "; "))
	}
	return result.Episodes, nil
}

// FetchSideStoryJPScriptContext fetches and canonicalises the JP script of one
// episode. It returns sql.ErrNoRows for an unknown episode.
func (t *Translator) FetchSideStoryJPScriptContext(ctx context.Context, kind, storyID, episodeKey string) (string, string, error) {
	item, err := t.store.SideStoryEpisodeContext(ctx, kind, storyID, episodeKey)
	if err != nil {
		return "", "", err
	}
	outcome, err := t.fetchSideStoryScriptContext(ctx, &sideStoryPacer{}, "jp", kind, item.JPAssetPath)
	switch {
	case err != nil:
		return "", "", err
	case outcome.Missing:
		return "", "", fmt.Errorf("%w: %s not found", ErrSideStoryUpstreamUnavailable, item.JPAssetPath)
	case outcome.Script == nil:
		return "", "", fmt.Errorf("%w: %s", ErrSideStoryUpstreamUnavailable, outcome.Err)
	}
	return outcome.Script.CanonicalJSON, outcome.Script.SHA256, nil
}
