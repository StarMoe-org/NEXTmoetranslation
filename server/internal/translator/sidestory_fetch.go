package translator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"moesekai/server/internal/httpx"
	"moesekai/server/internal/store"
)

// ErrSideStoryUpstreamUnavailable reports that no configured source served a
// card or area script.
var ErrSideStoryUpstreamUnavailable = errors.New("side story upstream unavailable")

// sideStoryPacer spaces consecutive upstream requests by delay, measured from
// the end of the previous request, and counts them.
type sideStoryPacer struct {
	delay    time.Duration
	last     time.Time
	requests int
}

func (p *sideStoryPacer) begin(ctx context.Context) error {
	if p.requests > 0 && p.delay > 0 {
		if err := waitContext(ctx, p.delay-time.Since(p.last)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.requests++
	return nil
}

func (p *sideStoryPacer) end() { p.last = time.Now() }

// getSideStoryUpstream GETs url under the masterdata size limits and returns
// the decoded body and the HTTP status.
func (t *Translator) getSideStoryUpstream(ctx context.Context, pacer *sideStoryPacer, url string) ([]byte, int, error) {
	if err := pacer.begin(ctx); err != nil {
		return nil, 0, err
	}
	defer pacer.end()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "moesekai-data-sync")
	resp, err := t.dataClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return nil, resp.StatusCode, fmt.Errorf("GET %s: http %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := httpx.ReadBody(resp, maxMasterdataWireBytes, maxMasterdataDecodedBytes)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("GET %s: read: %w", url, err)
	}
	return raw, resp.StatusCode, nil
}

// fetchSideStoryScriptContext fetches one script from the server's bases in
// order. A response without TalkData or with an invalid script moves on to the
// next base; every base answering 404 makes the script Missing. The script is
// parsed under its own ScenarioId so the store can judge the identity. Only
// context errors are returned as errors.
func (t *Translator) fetchSideStoryScriptContext(ctx context.Context, pacer *sideStoryPacer, server, kind, assetPath string) (store.SideStoryFetchOutcome, error) {
	outcome := store.SideStoryFetchOutcome{Attempted: true}
	bases := t.sideStoryScriptBases(server, kind)
	missing := len(bases) > 0
	failures := make([]string, 0, len(bases))
	for _, base := range bases {
		url := joinSourceURL(base, assetPath+".json")
		raw, status, err := t.getSideStoryUpstream(ctx, pacer, url)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return outcome, ctxErr
		}
		if err == nil {
			var value any
			if err = json.Unmarshal(raw, &value); err != nil {
				err = fmt.Errorf("GET %s: decode: %w", url, err)
			} else if !scenarioHasTalkData(value) {
				err = fmt.Errorf("GET %s: missing TalkData", url)
			} else {
				scenarioID, _ := asMap(value)["ScenarioId"].(string)
				script, parseErr := store.ParseSideStoryScript(value, scenarioID)
				if parseErr == nil {
					return store.SideStoryFetchOutcome{Attempted: true, Script: &script}, nil
				}
				err = fmt.Errorf("GET %s: %w", url, parseErr)
			}
		}
		failures = append(failures, err.Error())
		if status == http.StatusNotFound {
			continue
		}
		missing = false
		outcome.Transient = outcome.Transient || isTransientErr(err)
	}
	if missing {
		outcome.Missing = true
		return outcome, nil
	}
	outcome.Err = "no upstream source configured"
	if len(failures) > 0 {
		outcome.Err = truncateStatusDetail(strings.Join(failures, "; "), 400)
	}
	return outcome, nil
}

// fetchSideStoryEpisodeContext fetches an episode's JP script and, when it is
// present, the CN/EN scripts selected by fetchCN/fetchEN. Only context errors
// are returned as errors.
func (t *Translator) fetchSideStoryEpisodeContext(ctx context.Context, pacer *sideStoryPacer, item store.SideStoryWorkItem, fetchCN, fetchEN bool) (store.SideStoryEpisodeFetch, error) {
	fetch := store.SideStoryEpisodeFetch{Kind: item.Kind, StoryID: item.StoryID, EpisodeKey: item.EpisodeKey}
	var err error
	if fetch.JP, err = t.fetchSideStoryScriptContext(ctx, pacer, "jp", item.Kind, item.JPAssetPath); err != nil || fetch.JP.Script == nil {
		return fetch, err
	}
	if fetchCN && item.CNAssetPath != "" {
		if fetch.CN, err = t.fetchSideStoryScriptContext(ctx, pacer, "cn", item.Kind, item.CNAssetPath); err != nil {
			return fetch, err
		}
	}
	if fetchEN && item.ENAssetPath != "" {
		if fetch.EN, err = t.fetchSideStoryScriptContext(ctx, pacer, "en", item.Kind, item.ENAssetPath); err != nil {
			return fetch, err
		}
	}
	return fetch, nil
}

// fetchSideStoryMasterdata decodes one masterdata array from the server's
// bases in order, keeping only the fields of T.
func fetchSideStoryMasterdata[T any](ctx context.Context, t *Translator, pacer *sideStoryPacer, server, filename string) ([]T, error) {
	failures := []sourceFailure{}
	for _, base := range t.masterdataBases(server) {
		url := joinSourceURL(base, filename)
		raw, _, err := t.getSideStoryUpstream(ctx, pacer, url)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err == nil {
			var records []T
			if err = json.Unmarshal(raw, &records); err == nil && len(records) > maxMasterdataRecords {
				err = fmt.Errorf("too many records: %d", len(records))
			}
			if err == nil {
				return records, nil
			}
			err = fmt.Errorf("GET %s: decode: %w", url, err)
		}
		failures = append(failures, sourceFailure{url: url, err: err})
	}
	return nil, fmt.Errorf("%s %s: %w", server, filename, joinSourceFailures(failures))
}
