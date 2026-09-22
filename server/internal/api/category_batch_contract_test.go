package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/model"
	"moesekai/server/internal/searchindex"
)

type staticProjectionStatus struct{ value filesvc.ProjectionStatus }

func (status staticProjectionStatus) Status() filesvc.ProjectionStatus { return status.value }

type staticSearchStatus struct{ value searchindex.Status }

func (status staticSearchStatus) Status() searchindex.Status { return status.value }

type fakeProjectionWithProvenance struct {
	status filesvc.ProjectionStatus
	prov   map[int]filesvc.SongProvenance
}

func (f fakeProjectionWithProvenance) Status() filesvc.ProjectionStatus { return f.status }
func (f fakeProjectionWithProvenance) SongProvenance(musicID int) (filesvc.SongProvenance, bool) {
	p, ok := f.prov[musicID]
	return p, ok
}

func TestProjectionStatusMusicIDQueryContract(t *testing.T) {
	h := setupLegacyAPI(t)
	fake := fakeProjectionWithProvenance{
		status: filesvc.ProjectionStatus{
			Generation: 10,
			LyricsSummary: filesvc.LyricsProjectionSummary{
				TotalSongs:         706,
				BundleSongs:        680,
				DBPublicationSongs: 25,
				LocalizationSongs:  1,
				Degraded:           false,
				BundleReleaseID:    "test-release",
			},
		},
		prov: map[int]filesvc.SongProvenance{
			682: {
				MusicID:           682,
				Source:            "localization_projection",
				Revision:          2,
				State:             "complete",
				AvailableVersions: []string{"full", "game"},
				UpdatedAt:         "2026-08-25T14:15:00Z",
				HasDetail:         true,
			},
		},
	}
	h.api.SetProjectionStatus(fake)

	t.Run("no musicId param returns status without song field", func(t *testing.T) {
		resp := authorizedRequest(t, h, http.MethodGet, "/api/projection/status", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["song"]; ok {
			t.Fatalf("unexpected song field in response: %v", raw)
		}
		if _, ok := raw["lyricsSummary"]; !ok {
			t.Fatalf("expected lyricsSummary in response: %v", raw)
		}
	})

	t.Run("invalid musicId returns 400", func(t *testing.T) {
		for _, invalid := range []string{"0", "-1", "abc", "null"} {
			resp := authorizedRequest(t, h, http.MethodGet, "/api/projection/status?musicId="+invalid, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("musicId=%s status = %d, want 400", invalid, resp.StatusCode)
			}
		}
	})

	t.Run("existing musicId returns song provenance", func(t *testing.T) {
		resp := authorizedRequest(t, h, http.MethodGet, "/api/projection/status?musicId=682", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var res struct {
			filesvc.ProjectionStatus
			Song *filesvc.SongProvenance `json:"song"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatal(err)
		}
		if res.Song == nil || res.Song.MusicID != 682 || res.Song.Source != "localization_projection" || res.Song.Revision != 2 {
			t.Fatalf("song provenance mismatch: %+v", res.Song)
		}
	})

	t.Run("nonexistent musicId returns song null", func(t *testing.T) {
		resp := authorizedRequest(t, h, http.MethodGet, "/api/projection/status?musicId=999999", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		val, exists := raw["song"]
		if !exists || val != nil {
			t.Fatalf("expected song: null, got exists=%t val=%v", exists, val)
		}
	})
}

func TestCategorySnapshotBatchAuditSSEAndProjectionContract(t *testing.T) {
	h := setupLegacyAPI(t)
	h.api.SetProjectionStatus(staticProjectionStatus{value: filesvc.ProjectionStatus{
		Generation: 6, Pending: true,
	}})
	h.api.SetSearchStatus(staticSearchStatus{value: searchindex.Status{
		Ready: true, Degraded: true, Generation: 2, LastError: "search_index_build_failed", Source: "cache",
	}})

	snapshotResponse := authorizedRequest(t, h, http.MethodGet, "/api/category/snapshot?category=cards&locale=en-US", nil)
	defer snapshotResponse.Body.Close()
	if snapshotResponse.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d", snapshotResponse.StatusCode)
	}
	var snapshot model.CategoryLocaleSnapshot
	if err := json.NewDecoder(snapshotResponse.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Category != "cards" || snapshot.Locale != model.LocaleEnglish || snapshot.Revision == "" || len(snapshot.Fields["prefix"]) != 6 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	streamRequest := bearerSSERequest(t, h.server.URL, h.token)
	stream, err := http.DefaultClient.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	events := make(chan map[string]any, 2)
	go func() {
		reader := bufio.NewReader(stream.Body)
		var event string
		for len(events) < 2 {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "entry.locale.updated" && strings.HasPrefix(line, "data: ") {
				var payload map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload)
				events <- payload
			}
		}
	}()

	batch := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/category/batch", map[string]any{
		"category": "cards", "locale": model.LocaleEnglish, "baseRevision": snapshot.Revision,
		"clientId": "sekaitext-window", "updates": []map[string]string{
			{"field": "prefix", "key": "cn-key", "text": "Official", "source": "human"},
			{"field": "prefix", "key": "human-key", "text": "Human", "source": "human"},
		},
	})
	defer batch.Body.Close()
	if batch.StatusCode != http.StatusOK {
		t.Fatalf("batch status = %d", batch.StatusCode)
	}
	var result struct {
		model.CategoryLocaleSnapshot
		Updated int `json:"updated"`
	}
	if err := json.NewDecoder(batch.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Updated != 2 || result.Revision == snapshot.Revision || len(result.Fields["prefix"]) != 6 {
		t.Fatalf("batch result = %+v", result)
	}
	for range 2 {
		select {
		case payload := <-events:
			if payload["locale"] != model.LocaleEnglish || payload["clientId"] != "sekaitext-window" || payload["category"] != "cards" {
				t.Fatalf("SSE payload = %#v", payload)
			}
			var audits int
			if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE detail LIKE '%batch=true'`).Scan(&audits); err != nil || audits != 2 {
				t.Fatalf("SSE escaped before commit: audits=%d err=%v", audits, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for batch per-entry SSE")
		}
	}

	stale := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/category/batch", map[string]any{
		"category": "cards", "locale": model.LocaleEnglish, "baseRevision": snapshot.Revision,
		"updates": []map[string]string{{"field": "prefix", "key": "pinned-key", "text": "stale", "source": "human"}},
	})
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale batch status = %d", stale.StatusCode)
	}
	var staleBody struct {
		Error   string                       `json:"error"`
		Current model.CategoryLocaleSnapshot `json:"current"`
	}
	if err := json.NewDecoder(stale.Body).Decode(&staleBody); err != nil {
		t.Fatal(err)
	}
	stale.Body.Close()
	if staleBody.Error != "revision_conflict" || staleBody.Current.Revision != result.Revision {
		t.Fatalf("stale batch body = %+v", staleBody)
	}

	projection := authorizedRequest(t, h, http.MethodGet, "/api/projection/status", nil)
	defer projection.Body.Close()
	if projection.StatusCode != http.StatusOK {
		t.Fatalf("projection status = %d", projection.StatusCode)
	}
	var projectionStatus filesvc.ProjectionStatus
	if err := json.NewDecoder(projection.Body).Decode(&projectionStatus); err != nil || projectionStatus.Generation != 6 || !projectionStatus.Pending {
		t.Fatalf("projection body=%+v err=%v", projectionStatus, err)
	}
	searchResponse := authorizedRequest(t, h, http.MethodGet, "/api/search/status", nil)
	defer searchResponse.Body.Close()
	if searchResponse.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", searchResponse.StatusCode)
	}
	var searchStatus searchindex.Status
	if err := json.NewDecoder(searchResponse.Body).Decode(&searchStatus); err != nil || !searchStatus.Ready || !searchStatus.Degraded || searchStatus.Source != "cache" {
		t.Fatalf("search body=%+v err=%v", searchStatus, err)
	}
}

func TestEventLocaleRevisionRejectsCompetingTranslationEdit(t *testing.T) {
	h := setupLegacyAPI(t)
	detailResponse := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=en-US", nil)
	defer detailResponse.Body.Close()
	var detail model.EventStoryDetail
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	var target model.EventStorySegment
	for _, segment := range detail.Episodes["1"].Segments {
		if segment.Kind == "talk" && segment.Japanese == "二" {
			target = segment
		}
	}
	if target.ID == "" || target.Revision != 0 {
		t.Fatalf("initial target = %+v", target)
	}
	request := map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "segmentId": target.ID,
		"sourceHash": target.SourceHash, "cnText": "First English", "source": "human",
		"entryType": "talk", "locale": model.LocaleEnglish, "revision": target.Revision,
	}
	first := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/event-story/update", request)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first revision update status = %d", first.StatusCode)
	}
	var saved struct {
		Status   string `json:"status"`
		Revision int    `json:"revision"`
	}
	if err := json.NewDecoder(first.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if saved.Status != "ok" || saved.Revision != 1 {
		t.Fatalf("revision update response = %+v", saved)
	}
	request["cnText"] = "Stale overwrite"
	stale := authorizedRequest(t, h, http.MethodPut, "/api/editor/v1/event-story/update", request)
	defer stale.Body.Close()
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale revision status = %d", stale.StatusCode)
	}
	var contractErr struct {
		Code string `json:"error"`
	}
	_ = json.NewDecoder(stale.Body).Decode(&contractErr)
	if contractErr.Code != "revision_conflict" {
		t.Fatalf("stale revision error = %+v", contractErr)
	}
	roundTrip := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=en-US", nil)
	defer roundTrip.Body.Close()
	if err := json.NewDecoder(roundTrip.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	for _, segment := range detail.Episodes["1"].Segments {
		if segment.ID == target.ID && (segment.Text != "First English" || segment.Revision != 1) {
			t.Fatalf("revision round trip = %+v", segment)
		}
	}
}
