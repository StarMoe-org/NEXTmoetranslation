package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/api"
	"moesekai/server/internal/auth"
	"moesekai/server/internal/backup"
	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/model"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
)

func TestSekaiTextCategorySaveToPublicFilesAndExistingBackups(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	t.Setenv("MOESEKAI_BACKUP_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, "compatibility.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	translations := store.New(database)
	events := store.NewEventStore(database)
	if _, err := translations.ImportCategory("cards", model.Category{
		"prefix": {"source-key": {Text: "before", Source: model.SourceCN, Ids: []string{"42"}}},
	}); err != nil {
		t.Fatal(err)
	}
	configStore, err := config.New(database, "compatibility-master-key")
	if err != nil {
		t.Fatal(err)
	}
	authService := auth.New(database, "compatibility-jwt-secret-at-least-32-bytes", time.Hour)
	if _, err := authService.CreateUser("editor", "compatibility-password", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	var s3Mu sync.Mutex
	var latestArtifact []byte
	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/latest.enc") {
			s3Mu.Lock()
			latestArtifact = append([]byte(nil), body...)
			s3Mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Server.Close()
	remote := filepath.Join(root, "backup.git")
	command := exec.Command("git", "init", "--bare", "--initial-branch=compatibility", remote)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("init bare git: %v: %s", err, output)
	}
	settings := map[string]string{
		config.KeyBackupS3Enabled:   "true",
		config.KeyBackupS3Endpoint:  s3Server.URL,
		config.KeyBackupS3Region:    "local-1",
		config.KeyBackupS3Bucket:    "compatibility",
		config.KeyBackupS3Prefix:    "snapshots",
		config.KeyBackupS3AccessKey: "local-access",
		config.KeyBackupS3SecretKey: "local-secret",
		config.KeyBackupGitEnabled:  "true",
		config.KeyBackupGitRepoURL:  "file://" + remote,
		config.KeyBackupGitBranch:   "compatibility",
	}
	for key, value := range settings {
		if err := configStore.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}

	generator := files.NewGenerator(translations, events, root)
	publicFiles := filesvc.New(translations, events, generator)
	publicFiles.SetDebounce(time.Millisecond)
	publicFiles.Start()
	t.Cleanup(func() {
		publicFiles.Stop()
		publicFiles.Wait()
	})
	translations.OnChange(publicFiles.Trigger)
	hub := sse.NewHub()
	backupManager := backup.NewManager(configStore, generator, translations, events, filepath.Join(root, "backup-work"))
	apiServer := api.NewServer(translations, events, authService, configStore, hub,
		translator.New(translations, events, configStore), nil, backupManager)
	apiServer.SetProjectionStatus(publicFiles)
	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	registerPublicFileRoutes(mux, publicFiles)
	server := httptest.NewServer(mux)
	defer server.Close()

	token := compatibilityLogin(t, server.URL)
	streamRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/sse", nil)
	streamRequest.Header.Set("Authorization", "Bearer "+token)
	stream, err := http.DefaultClient.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	eventReceived := make(chan map[string]any, 1)
	go func() {
		reader := bufio.NewReader(stream.Body)
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
			if event == sse.EventEntryUpdated && strings.HasPrefix(line, "data: ") {
				var payload map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload)
				eventReceived <- payload
				return
			}
		}
	}()

	projectionBeforeResponse := compatibilityJSON(t, token, http.MethodGet, server.URL+"/api/projection/status", nil)
	var projectionBefore filesvc.ProjectionStatus
	if err := json.NewDecoder(projectionBeforeResponse.Body).Decode(&projectionBefore); err != nil {
		t.Fatal(err)
	}
	projectionBeforeResponse.Body.Close()
	snapshotResponse := compatibilityJSON(t, token, http.MethodGet, server.URL+"/api/category/snapshot?category=cards&locale=zh-CN", nil)
	var snapshot model.CategoryLocaleSnapshot
	if err := json.NewDecoder(snapshotResponse.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	snapshotResponse.Body.Close()
	batchResponse := compatibilityStrictJSON(t, token, http.MethodPut, server.URL, "/api/editor/v1/category/batch", map[string]any{
		"category": "cards", "locale": model.LocaleChinese, "baseRevision": snapshot.Revision,
		"clientId": "sekaitext-e2e", "updates": []map[string]string{{
			"field": "prefix", "key": "source-key", "text": "saved through API", "source": "human",
		}},
	})
	var saved struct {
		model.CategoryLocaleSnapshot
		Updated int `json:"updated"`
	}
	if err := json.NewDecoder(batchResponse.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	batchResponse.Body.Close()
	if saved.Updated != 1 || saved.Revision == snapshot.Revision || saved.Fields["prefix"][0].Text != "saved through API" {
		t.Fatalf("save response = %+v", saved)
	}
	select {
	case payload := <-eventReceived:
		if payload["key"] != "source-key" || payload["text"] != "saved through API" {
			t.Fatalf("SSE = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("save did not emit backward-compatible per-entry SSE")
	}
	var dbText string
	if err := database.QueryRow(`SELECT cn_text FROM entries WHERE category='cards' AND field='prefix' AND jp_key='source-key'`).Scan(&dbText); err != nil || dbText != "saved through API" {
		t.Fatalf("database text=%q err=%v", dbText, err)
	}
	var auditCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='entry.locale.update' AND user='editor' AND detail LIKE '%batch=true'`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		statusResponse := compatibilityJSON(t, token, http.MethodGet, server.URL+"/api/projection/status", nil)
		var status filesvc.ProjectionStatus
		if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
			t.Fatal(err)
		}
		statusResponse.Body.Close()
		if !status.Pending && status.Generation > projectionBefore.Generation {
			if status.LastError != "" {
				t.Fatalf("projection status = %+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("projection did not publish after generation %d: %+v", projectionBefore.Generation, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	filesBody := compatibilityPublicGET(t, server.URL+"/files/translation/cards.json")
	aliasBody := compatibilityPublicGET(t, server.URL+"/translation/cards.json")
	if !bytes.Equal(filesBody, aliasBody) || !bytes.Contains(filesBody, []byte("saved through API")) {
		t.Fatalf("public aliases differ or omit save\n/files: %s\n/translation: %s", filesBody, aliasBody)
	}
	lyricsIndex := compatibilityPublicGET(t, server.URL+"/translation/lyrics/index.json")
	for _, path := range []string{"/translation/lyrics", "/translation/lyrics/"} {
		if got := compatibilityPublicGET(t, server.URL+path); !bytes.Equal(got, lyricsIndex) {
			t.Fatalf("lyrics directory alias %s differs from index", path)
		}
	}
	var indexPayload struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(lyricsIndex, &indexPayload); err != nil || indexPayload.Version != 3 {
		t.Fatalf("lyrics index is not the accepted public v3 contract: %s", lyricsIndex)
	}

	backupResponse := compatibilityStrictJSON(t, token, http.MethodPost, server.URL, "/api/editor/v1/backup/push", map[string]any{})
	defer backupResponse.Body.Close()
	if backupResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(backupResponse.Body)
		t.Fatalf("backup push status=%d body=%s", backupResponse.StatusCode, body)
	}
	gitShow := exec.Command("git", "--git-dir", remote, "show", "refs/heads/compatibility:backup.enc")
	gitArtifact, err := gitShow.Output()
	if err != nil {
		t.Fatal(err)
	}
	s3Mu.Lock()
	s3Artifact := append([]byte(nil), latestArtifact...)
	s3Mu.Unlock()
	if len(gitArtifact) == 0 || !bytes.Equal(gitArtifact, s3Artifact) {
		t.Fatalf("Git and S3 must contain the same non-empty encrypted artifact: git=%d s3=%d", len(gitArtifact), len(s3Artifact))
	}
	if bytes.Contains(gitArtifact, filesBody) || bytes.Contains(gitArtifact, []byte("saved through API")) ||
		bytes.HasPrefix(gitArtifact, []byte{0x1f, 0x8b}) || json.Valid(gitArtifact) {
		t.Fatal("backup targets exposed plaintext or directly parseable backup content")
	}
}

func compatibilityLogin(t *testing.T, baseURL string) string {
	t.Helper()
	response := compatibilityJSON(t, "", http.MethodPost, baseURL+"/api/auth/login", map[string]string{
		"username": "editor", "password": "compatibility-password",
	})
	defer response.Body.Close()
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&login); err != nil || login.Token == "" {
		t.Fatalf("login=%+v err=%v", login, err)
	}
	return login.Token
}

// compatibilityStrictJSON adds the producer-state proof the v1 write routes
// require, taken from the gate status a freshly loaded client would have seen.
func compatibilityStrictJSON(t *testing.T, token, method, baseURL, path string, value any) *http.Response {
	t.Helper()
	gate := compatibilityJSON(t, token, http.MethodGet, baseURL+"/api/editor-gate/status", nil)
	var status struct {
		InstanceID          string `json:"instanceId"`
		Revision            uint64 `json:"revision"`
		CompletedGeneration uint64 `json:"completedGeneration"`
	}
	if err := json.NewDecoder(gate.Body).Decode(&status); err != nil {
		gate.Body.Close()
		t.Fatal(err)
	}
	gate.Body.Close()
	return compatibilityJSON(t, token, method, baseURL+path, value,
		fmt.Sprintf("%s:%d:%d", status.InstanceID, status.Revision, status.CompletedGeneration))
}

func compatibilityJSON(t *testing.T, token, method, url string, value any, producerState ...string) *http.Response {
	t.Helper()
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, _ := http.NewRequest(method, url, body)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, state := range producerState {
		request.Header.Set("X-Moe-Loaded-Producer-State", state)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("%s %s status=%d body=%s", method, url, response.StatusCode, payload)
	}
	return response
}

func compatibilityPublicGET(t *testing.T, url string) []byte {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d err=%v", url, response.StatusCode, err)
	}
	return body
}
