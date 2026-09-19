package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/model"
)

func TestLocaleEntryIsolationAndValidation(t *testing.T) {
	h := setupLegacyAPI(t)

	update := authorizedRequest(t, h, http.MethodPut, "/api/entry?response=correlated-v1", map[string]string{
		"category": "cards", "field": "prefix", "key": "human-key",
		"text": "English editorial", "source": "human", "locale": model.LocaleEnglish,
	})
	defer update.Body.Close()
	if update.StatusCode != http.StatusOK {
		t.Fatalf("English update status = %d", update.StatusCode)
	}
	var updateResult map[string]string
	if err := json.NewDecoder(update.Body).Decode(&updateResult); err != nil {
		t.Fatal(err)
	}
	if updateResult["locale"] != model.LocaleEnglish || updateResult["category"] != "cards" ||
		updateResult["field"] != "prefix" || updateResult["key"] != "human-key" || updateResult["text"] != "English editorial" ||
		updateResult["source"] != "human" || updateResult["status"] != "ok" || len(updateResult) != 7 {
		t.Fatalf("English correlated response = %#v", updateResult)
	}

	english := authorizedRequest(t, h, http.MethodGet, "/api/entries?category=cards&field=prefix&locale=en-US", nil)
	defer english.Body.Close()
	var englishEntries []model.EntryWithKey
	if err := json.NewDecoder(english.Body).Decode(&englishEntries); err != nil {
		t.Fatal(err)
	}
	if got := findEntry(englishEntries, "human-key"); got.Text != "English editorial" || got.Source != model.SourceHuman {
		t.Fatalf("English entry = %+v", got)
	}
	var localeAudits int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='entry.locale.update' AND user='alice'`).Scan(&localeAudits); err != nil || localeAudits != 1 {
		t.Fatalf("locale audit count=%d err=%v", localeAudits, err)
	}
	if got := findEntry(englishEntries, "cn-key"); got.Text != "" || got.Source != model.SourceUnknown {
		t.Fatalf("missing English entry = %+v", got)
	}

	legacy := authorizedRequest(t, h, http.MethodGet, "/api/entries?category=cards&field=prefix&source=human", nil)
	defer legacy.Body.Close()
	var legacyEntries []model.EntryWithKey
	if err := json.NewDecoder(legacy.Body).Decode(&legacyEntries); err != nil {
		t.Fatal(err)
	}
	if got := findEntry(legacyEntries, "human-key"); got.Text != "人工" {
		t.Fatalf("English edit changed legacy zh-CN text: %+v", got)
	}

	japanese := authorizedRequest(t, h, http.MethodGet, "/api/entries?category=cards&field=prefix&locale=ja-JP", nil)
	defer japanese.Body.Close()
	var japaneseEntries []model.EntryWithKey
	if err := json.NewDecoder(japanese.Body).Decode(&japaneseEntries); err != nil {
		t.Fatal(err)
	}
	if got := findEntry(japaneseEntries, "human-key"); got.Text != "human-key" || got.Source != model.SourceUnknown {
		t.Fatalf("Japanese source entry = %+v", got)
	}

	readOnly := authorizedRequest(t, h, http.MethodPut, "/api/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "human-key",
		"text": "拒绝", "source": "human", "locale": model.LocaleJapanese,
	})
	defer readOnly.Body.Close()
	if readOnly.StatusCode != http.StatusBadRequest {
		t.Fatalf("ja-JP write status = %d", readOnly.StatusCode)
	}

	invalid := authorizedRequest(t, h, http.MethodGet, "/api/categories?locale=fr-FR", nil)
	defer invalid.Body.Close()
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid locale status = %d", invalid.StatusCode)
	}
}

func TestLocaleEventStoryUsesStableSegmentsAndKeepsLegacyProjection(t *testing.T) {
	h := setupLegacyAPI(t)
	initial := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=en-US", nil)
	defer initial.Body.Close()
	var detail model.EventStoryDetail
	if err := json.NewDecoder(initial.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	episode := detail.Episodes["1"]
	if len(episode.Segments) != 3 {
		t.Fatalf("stable segments = %d, want title plus two talks", len(episode.Segments))
	}
	segmentID := ""
	sourceHash := ""
	for _, segment := range episode.Segments {
		if segment.Kind == "talk" && segment.Japanese == "二" {
			segmentID = segment.ID
			sourceHash = segment.SourceHash
		}
	}
	if segmentID == "" {
		t.Fatal("talk segment ID not found")
	}
	missingIdentity := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "segmentId": segmentID,
		"cnText": "Missing hash", "source": "human", "entryType": "talk", "locale": model.LocaleEnglish,
	})
	missingIdentity.Body.Close()
	if missingIdentity.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing source identity status = %d", missingIdentity.StatusCode)
	}
	staleIdentity := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "segmentId": segmentID, "sourceHash": "stale",
		"cnText": "Stale write", "source": "human", "entryType": "talk", "locale": model.LocaleEnglish,
	})
	staleIdentity.Body.Close()
	if staleIdentity.StatusCode != http.StatusConflict {
		t.Fatalf("stale source identity status = %d", staleIdentity.StatusCode)
	}

	update := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "segmentId": segmentID,
		"sourceHash": sourceHash, "cnText": "Second line", "source": "human", "entryType": "talk", "locale": model.LocaleEnglish,
	})
	defer update.Body.Close()
	if update.StatusCode != http.StatusOK {
		t.Fatalf("English event update status = %d", update.StatusCode)
	}
	var eventAudits int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='event.locale.update' AND user='alice'`).Scan(&eventAudits); err != nil || eventAudits != 1 {
		t.Fatalf("event locale audit count=%d err=%v", eventAudits, err)
	}

	localized := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=en-US", nil)
	defer localized.Body.Close()
	if err := json.NewDecoder(localized.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if got := detail.Episodes["1"].TalkData["二"]; got != "Second line" {
		t.Fatalf("English talk text = %q", got)
	}
	legacy := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42", nil)
	defer legacy.Body.Close()
	if err := json.NewDecoder(legacy.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if got := detail.Episodes["1"].TalkData["二"]; got != "第二句" {
		t.Fatalf("English edit changed legacy talk text = %q", got)
	}
}

func TestExplicitChineseEventWriteRequiresIdentityWhileOmittedLocaleStaysLegacy(t *testing.T) {
	h := setupLegacyAPI(t)
	initial := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=zh-CN", nil)
	defer initial.Body.Close()
	var detail model.EventStoryDetail
	if err := json.NewDecoder(initial.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	var segmentID, sourceHash string
	for _, segment := range detail.Episodes["1"].Segments {
		if segment.Kind == "talk" && segment.Japanese == "二" {
			segmentID, sourceHash = segment.ID, segment.SourceHash
			if segment.Text != "第二句" || segment.Source != model.SourceHuman {
				t.Fatalf("explicit zh-CN segment = %+v", segment)
			}
		}
	}
	if segmentID == "" {
		t.Fatal("talk segment identity not found")
	}
	base := map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "segmentId": segmentID,
		"sourceHash": sourceHash, "cnText": "显式中文", "source": "human",
		"entryType": "talk", "locale": model.LocaleChinese,
	}
	for _, field := range []string{"episodeNo", "jpKey", "segmentId", "sourceHash", "entryType"} {
		request := make(map[string]any, len(base))
		for key, value := range base {
			request[key] = value
		}
		delete(request, field)
		response := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", request)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("explicit zh-CN missing %s status = %d", field, response.StatusCode)
		}
	}
	stale := make(map[string]any, len(base))
	for key, value := range base {
		stale[key] = value
	}
	stale["sourceHash"] = "stale"
	response := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", stale)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("explicit zh-CN stale identity status = %d", response.StatusCode)
	}
	response = authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", base)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("explicit zh-CN update status = %d", response.StatusCode)
	}
	roundTrip := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=zh-CN", nil)
	defer roundTrip.Body.Close()
	if err := json.NewDecoder(roundTrip.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	foundUpdatedSegment := false
	for _, segment := range detail.Episodes["1"].Segments {
		if segment.ID == segmentID && segment.SourceHash == sourceHash && segment.Text == "显式中文" {
			foundUpdatedSegment = true
		}
	}
	if !foundUpdatedSegment {
		t.Fatalf("explicit zh-CN GET/PUT round trip = %+v", detail.Episodes["1"].Segments)
	}
	omittedRead := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42", nil)
	defer omittedRead.Body.Close()
	var omittedDetail model.EventStoryDetail
	if err := json.NewDecoder(omittedRead.Body).Decode(&omittedDetail); err != nil {
		t.Fatal(err)
	}
	if len(omittedDetail.Episodes["1"].Segments) != 0 || omittedDetail.Episodes["1"].TalkData["二"] != "显式中文" {
		t.Fatalf("omitted-locale legacy GET shape = %+v", omittedDetail.Episodes["1"])
	}
	legacy := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "cnText": "省略语言",
		"source": "human", "entryType": "talk",
	})
	legacy.Body.Close()
	if legacy.StatusCode != http.StatusOK {
		t.Fatalf("omitted-locale legacy update status = %d", legacy.StatusCode)
	}
}

func TestExplicitChineseEntryAuditFailureRollsBackMutation(t *testing.T) {
	h := setupLegacyAPI(t)
	if _, err := h.db.Exec(`CREATE TRIGGER fail_entry_locale_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='entry.locale.update' BEGIN SELECT RAISE(ABORT, 'audit failed'); END`); err != nil {
		t.Fatal(err)
	}
	response := authorizedRequest(t, h, http.MethodPut, "/api/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "human-key", "text": "不应保存",
		"source": "human", "locale": model.LocaleChinese,
	})
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("audit failure status = %d", response.StatusCode)
	}
	var text string
	if err := h.db.QueryRow(`SELECT cn_text FROM entries WHERE category='cards' AND field='prefix' AND jp_key='human-key'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "人工" {
		t.Fatalf("entry mutation survived failed audit: %q", text)
	}
}

func TestExplicitChineseEventAuditFailureRollsBackMutation(t *testing.T) {
	h := setupLegacyAPI(t)
	initial := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=zh-CN", nil)
	defer initial.Body.Close()
	var detail model.EventStoryDetail
	if err := json.NewDecoder(initial.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	var segmentID, sourceHash string
	for _, segment := range detail.Episodes["1"].Segments {
		if segment.Kind == "talk" && segment.Japanese == "二" {
			segmentID, sourceHash = segment.ID, segment.SourceHash
		}
	}
	if _, err := h.db.Exec(`CREATE TRIGGER fail_event_locale_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='event.locale.update' BEGIN SELECT RAISE(ABORT, 'audit failed'); END`); err != nil {
		t.Fatal(err)
	}
	response := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "segmentId": segmentID,
		"sourceHash": sourceHash, "cnText": "不应保存", "source": "human",
		"entryType": "talk", "locale": model.LocaleChinese,
	})
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("audit failure status = %d", response.StatusCode)
	}
	var text string
	if err := h.db.QueryRow(`SELECT cn_text FROM event_story_lines WHERE event_id=42 AND episode_no='1' AND jp_key='二'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "第二句" {
		t.Fatalf("event mutation survived failed audit: %q", text)
	}
}

func TestLocaleSSEPayloadIsIgnorableAndScoped(t *testing.T) {
	h := setupLegacyAPI(t)
	req := bearerSSERequest(t, h.server.URL, h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	eventData := make(chan map[string]any, 1)
	legacyEvent := make(chan struct{}, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		var event string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if event == "entry.locale.updated" && strings.HasPrefix(line, "data: ") {
				var data map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data)
				eventData <- data
				return
			}
			if event == "entry.updated" && strings.HasPrefix(line, "data: ") {
				legacyEvent <- struct{}{}
				return
			}
		}
	}()
	update := authorizedRequest(t, h, http.MethodPut, "/api/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "cn-key",
		"text": "English", "source": "human", "locale": model.LocaleEnglish,
	})
	update.Body.Close()
	select {
	case <-legacyEvent:
		t.Fatal("English edit was broadcast on the legacy Chinese SSE event")
	case data := <-eventData:
		if data["locale"] != model.LocaleEnglish || data["key"] != "cn-key" || data["text"] != "English" {
			t.Fatalf("localized SSE payload = %#v", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for localized SSE")
	}
}

func TestExplicitChineseMutationsAuditWithoutChangingOmittedLegacyPath(t *testing.T) {
	h := setupLegacyAPI(t)
	omitted := authorizedRequest(t, h, http.MethodPut, "/api/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "human-key", "text": "Omitted Chinese", "source": "human",
	})
	omitted.Body.Close()
	var count int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='entry.locale.update'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("omitted locale entry audit count = %d", count)
	}
	explicit := authorizedRequest(t, h, http.MethodPut, "/api/entry", map[string]string{
		"category": "cards", "field": "prefix", "key": "human-key", "text": "Explicit Chinese", "source": "human", "locale": model.LocaleChinese,
	})
	explicit.Body.Close()
	if explicit.StatusCode != http.StatusOK {
		t.Fatalf("explicit Chinese entry status = %d", explicit.StatusCode)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='entry.locale.update' AND detail LIKE 'locale=zh-CN %'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("explicit Chinese entry audit count=%d err=%v", count, err)
	}

	detailResponse := authorizedRequest(t, h, http.MethodGet, "/api/event-story?eventId=42&locale=zh-CN", nil)
	defer detailResponse.Body.Close()
	var detail model.EventStoryDetail
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	var segmentID, sourceHash string
	for _, segment := range detail.Episodes["1"].Segments {
		if segment.Kind == "talk" && segment.Japanese == "二" {
			segmentID, sourceHash = segment.ID, segment.SourceHash
		}
	}
	event := authorizedRequest(t, h, http.MethodPut, "/api/event-story/update", map[string]any{
		"eventId": 42, "episodeNo": "1", "jpKey": "二", "cnText": "显式中文剧情",
		"segmentId": segmentID, "sourceHash": sourceHash, "source": "human",
		"entryType": "talk", "locale": model.LocaleChinese,
	})
	event.Body.Close()
	if event.StatusCode != http.StatusOK {
		t.Fatalf("explicit Chinese event status = %d", event.StatusCode)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='event.locale.update' AND detail LIKE 'locale=zh-CN %'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("explicit Chinese event audit count=%d err=%v", count, err)
	}
}

func TestOmittedLocaleInternalFailureIsSanitized(t *testing.T) {
	h := setupLegacyAPI(t)
	if _, err := h.db.Exec(`DROP TABLE entries`); err != nil {
		t.Fatal(err)
	}
	response := authorizedRequest(t, h, http.MethodGet, "/api/categories", nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError || strings.Contains(string(body), "no such table") ||
		!strings.Contains(string(body), `"error":"internal error"`) {
		t.Fatalf("omitted-locale failure status=%d body=%s", response.StatusCode, body)
	}
}

func findEntry(entries []model.EntryWithKey, key string) model.EntryWithKey {
	for _, entry := range entries {
		if entry.Key == key {
			return entry
		}
	}
	return model.EntryWithKey{}
}
