package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

func TestMattermostWebhookRoundTrip(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "tintwire.db")
	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(context.Background(), "test-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateChannel(context.Background(), store.CreateChannelInput{Name: "ignored-override", Visibility: "public"}); err != nil {
		t.Fatal(err)
	}

	handler := server.New(db)
	payload := []byte(`{
        "text":"build completed",
        "channel":"ignored-override",
        "username":"deploy-bot",
		"icon_url":"https://social.example/deploy-bot.png",
		"attachments":[{"color":"danger","title":"Release","title_link":"https://social.example/releases/1.2.3","text":"v1.2.3","image_url":"https://social.example/releases/1.2.3.png","fields":[{"title":"Status","value":"healthy","short":true}]}]
    }`)
	response := postJSON(t, handler, "/hooks/test-hook", payload)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, body = %q", response.StatusCode, readBody(t, response))
	}
	if body := readBody(t, response); body != "ok" {
		t.Fatalf("POST body = %q, want ok", body)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	listResponse := listRecorder.Result()
	defer listResponse.Body.Close()
	var result struct {
		Notifications []store.Notification `json:"notifications"`
		UnreadCount   int                  `json:"unread_count"`
	}
	if err := json.NewDecoder(listResponse.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Notifications) != 1 {
		t.Fatalf("notification count = %d, want 1", len(result.Notifications))
	}
	if result.UnreadCount != 1 || !result.Notifications[0].Unread {
		t.Fatalf("local inbox unread state = %#v", result)
	}
	got := result.Notifications[0]
	if got.Text != "build completed" || got.Username != "deploy-bot" || got.IconURL != "https://social.example/deploy-bot.png" || got.ChannelName != "ignored-override" {
		t.Fatalf("unexpected notification: %+v", got)
	}
	var attachments []map[string]any
	if err := json.Unmarshal(got.Attachments, &attachments); err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0]["title"] != "Release" {
		t.Fatalf("unexpected attachments: %#v", attachments)
	}
	if attachments[0]["color"] != "danger" {
		t.Fatalf("attachment color = %#v, want danger", attachments[0]["color"])
	}
	if attachments[0]["title_link"] != "https://social.example/releases/1.2.3" || attachments[0]["image_url"] != "https://social.example/releases/1.2.3.png" {
		t.Fatalf("attachment preview = %#v", attachments[0])
	}

	localInboxAction := func(action string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+got.ID+"/inbox", bytes.NewBufferString(`{"action":"`+action+`"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := localInboxAction("dismiss"); recorder.Code != http.StatusNoContent {
		t.Fatalf("local dismiss status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	dismissed := httptest.NewRecorder()
	handler.ServeHTTP(dismissed, httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil))
	if err := json.NewDecoder(dismissed.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Notifications) != 0 || result.UnreadCount != 0 {
		t.Fatalf("local dismissed inbox = %#v", result)
	}
	if recorder := localInboxAction("restore"); recorder.Code != http.StatusNoContent {
		t.Fatalf("local restore status = %d, body = %q", recorder.Code, recorder.Body.String())
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Text != "build completed" {
		t.Fatalf("notification did not persist: %#v", persisted)
	}
}

func TestPasswordLoginRateLimit(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "login-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateUser(ctx, "limited-user", "a sufficiently long password", false); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	login := func(password string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{"username":"limited-user","password":"`+password+`"}`))
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		request.RemoteAddr = "192.0.2.10:4321"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	for attempt := 0; attempt < 8; attempt++ {
		if response := login("wrong password"); response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d", attempt+1, response.Code)
		}
	}
	response := login("wrong password")
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "60" {
		t.Fatalf("limited response status=%d headers=%v", response.Code, response.Header())
	}
}

func TestOperationalEndpoints(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "operations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.ConfigureReplication("cluster-test-01", "node-test-01"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordReplicationPeerResult(context.Background(), "https://peer.example:18090", "node-peer-01", nil); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)

	for _, path := range []string{"/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %q", path, recorder.Code, recorder.Body.String())
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "tintwire_http_requests_total") ||
		!strings.Contains(recorder.Body.String(), `tintwire_replication_peer_up{peer="https://peer.example:18090",node_id="node-peer-01"} 1`) ||
		!strings.Contains(recorder.Body.String(), "tintwire_replication_quarantined 0") {
		t.Fatalf("GET /metrics = %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestReplicaReadinessRequiresControlLease(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "replica-readiness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.ConfigureReplication("cluster-test-01", "node-replica-01"); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureControlPlane("node-authority-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.New(db).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "security control lease") {
		t.Fatalf("GET /readyz = %d, body %q", recorder.Code, recorder.Body.String())
	}
}

func TestOAuthProtectedResourceMetadata(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "oauth-metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, PublicURL: "https://tintwire.example"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"resource":"https://tintwire.example/mcp"`) {
		t.Fatalf("metadata status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)))
	if !strings.Contains(unauthorized.Header().Get("WWW-Authenticate"), `resource_metadata="https://tintwire.example/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("challenge = %q", unauthorized.Header().Get("WWW-Authenticate"))
	}
}

func TestOAuthProtectedResourceMetadataAdvertisesPocketID(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "pocket-id-metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	handler, err := server.NewWithOptions(db, server.Options{PublicURL: "https://tintwire.example", OAuthIssuer: "https://idp.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	body := recorder.Body.String()
	for _, expected := range []string{`"authorization_servers":["https://idp.example.com"]`, `"scopes_supported":["tintwire:mcp"]`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metadata missing %s: %s", expected, body)
		}
	}
}

func TestMattermostWebhookValidation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "known-hook", "general"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)

	tests := []struct {
		name    string
		path    string
		payload string
		status  int
	}{
		{name: "unknown hook", path: "/hooks/unknown", payload: `{"text":"hello"}`, status: http.StatusNotFound},
		{name: "malformed JSON", path: "/hooks/known-hook", payload: `{`, status: http.StatusBadRequest},
		{name: "missing content", path: "/hooks/known-hook", payload: `{}`, status: http.StatusBadRequest},
		{name: "attachments is object", path: "/hooks/known-hook", payload: `{"attachments":{}}`, status: http.StatusBadRequest},
		{name: "attachment only", path: "/hooks/known-hook", payload: `{"attachments":[{"fallback":"summary"}]}`, status: http.StatusOK},
		{name: "blocks is object", path: "/hooks/known-hook", payload: `{"blocks":{}}`, status: http.StatusBadRequest},
		{name: "block only", path: "/hooks/known-hook", payload: `{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"hello"}}]}`, status: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := postJSON(t, handler, test.path, []byte(test.payload))
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d, body = %q", response.StatusCode, test.status, readBody(t, response))
			}
		})
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), `tintwire_webhook_rejections_total{reason="unknown"} 1`) {
		t.Fatalf("unknown webhook rejection metric missing: %s", metrics.Body.String())
	}
}

func TestMattermostWebhookChannelOverridePolicy(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "webhook-channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.BootstrapWebhook(ctx, "unlocked-hook", "default"); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(ctx, "other-hook", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportWebhooks(ctx, []store.WebhookImport{{Token: "other-hook", Channel: "other", ChannelLocked: true}}, true); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)

	overridden := postJSON(t, handler, "/hooks/unlocked-hook", []byte(`{"text":"redirected","channel":"#other"}`))
	if overridden.StatusCode != http.StatusOK {
		t.Fatalf("public override status = %d, body = %q", overridden.StatusCode, readBody(t, overridden))
	}
	overridden.Body.Close()
	notifications, err := db.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].ChannelName != "other" {
		t.Fatalf("public override notifications = %#v", notifications)
	}

	denied := postJSON(t, handler, "/hooks/unlocked-hook", []byte(`{"text":"denied","channel":"missing"}`))
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown override status = %d, body = %q", denied.StatusCode, readBody(t, denied))
	}
	denied.Body.Close()

	locked := postJSON(t, handler, "/hooks/other-hook", []byte(`{"text":"fixed","channel":"default"}`))
	if locked.StatusCode != http.StatusOK {
		t.Fatalf("locked override status = %d, body = %q", locked.StatusCode, readBody(t, locked))
	}
	locked.Body.Close()
	notifications, err = db.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 2 || notifications[0].ChannelName != "other" {
		t.Fatalf("locked hook notifications = %#v", notifications)
	}
}

func TestSlackBlockKitNormalization(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "blocks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "slack-hook", "slack"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)
	payload := []byte(`{"blocks":[
      {"type":"header","text":{"type":"plain_text","text":"Deploy complete"}},
      {"type":"section","text":{"type":"mrkdwn","text":"Version *1.2.3*"},"fields":[{"type":"mrkdwn","text":"*Region:* eu"}]},
      {"type":"divider"},
      {"type":"actions","elements":[{"type":"button","text":"Open build","url":"https://example.com/build"},{"type":"button","text":"Unsafe","url":"javascript:alert(1)"}]},
      {"type":"future_widget"}
    ]}`)
	response := postJSON(t, handler, "/hooks/slack-hook", payload)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()
	notifications, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 {
		t.Fatalf("notifications = %#v", notifications)
	}
	text := notifications[0].Text
	for _, expected := range []string{"**Deploy complete**", "Version *1.2.3*", "*Region:* eu", "<https://example.com/build|Open build>", "Unsafe", "[Unsupported Slack block: future_widget]"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("normalized text %q does not contain %q", text, expected)
		}
	}
	if strings.Contains(text, "javascript:") {
		t.Fatalf("unsafe URL survived normalization: %q", text)
	}
}

func TestMattermostWebhookFormPayloads(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "form-hook", "compatibility"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)

	form := url.Values{"payload": {`{"text":"URL encoded","username":"legacy"}`}}
	request := httptest.NewRequest(http.MethodPost, "/hooks/form-hook", bytes.NewBufferString(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("form status = %d, body = %q", recorder.Code, recorder.Body.String())
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("payload", `{"text":"Multipart","username":"legacy"}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/hooks/form-hook", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("multipart status = %d, body = %q", recorder.Code, recorder.Body.String())
	}

	notifications, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]bool{}
	for _, notification := range notifications {
		texts[notification.Text] = true
	}
	if len(notifications) != 2 || !texts["Multipart"] || !texts["URL encoded"] {
		t.Fatalf("notifications = %#v", notifications)
	}
}

func TestNativeCardRoundTrip(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "publisher-token", "release-lists"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateChannel(context.Background(), store.CreateChannelInput{Name: "alerts", DisplayName: "System alerts", Visibility: "public"}); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)
	payload := []byte(`{
        "version":1,
		"channel":"#alerts",
        "title":"Daily release summary",
        "summary":"3 unique from 4 list entries",
        "severity":"info",
        "source":"release_watcher",
        "metrics":[{"label":"Unique","value":3},{"label":"Duplicates removed","value":1}],
        "fields":[{"label":"Window","value":"Last 24 hours"}],
        "badges":[{"label":"Automated","tone":"info"}],
        "images":[{"url":"https://example.com/chart.png","alt":"Release activity chart"}],
        "links":[{"label":"Runbook","url":"https://example.com/runbook"}],
        "rows":[{"primary":"Example.Show.S01E01","tags":["Source A","Source B"],"emphasis":"strong"}],
        "actions":[{"label":"Open report","type":"link","url":"https://example.com/report"}]
    }`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer publisher-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	var created map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created["id"] == "" {
		t.Fatal("native endpoint returned no notification ID")
	}

	notifications, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 {
		t.Fatalf("notification count = %d", len(notifications))
	}
	if notifications[0].ChannelName != "alerts" || notifications[0].Username != "release_watcher" {
		t.Fatalf("notification = %#v", notifications[0])
	}
	var card map[string]any
	if err := json.Unmarshal(notifications[0].Card, &card); err != nil {
		t.Fatal(err)
	}
	if card["title"] != "Daily release summary" {
		t.Fatalf("card = %#v", card)
	}
	if len(card["fields"].([]any)) != 1 || len(card["badges"].([]any)) != 1 || len(card["images"].([]any)) != 1 || len(card["links"].([]any)) != 1 {
		t.Fatalf("rich components = %#v", card)
	}
}

func TestCompatibilityFixtures(t *testing.T) {
	fixturePath := func(name string) string { return filepath.Join("..", "..", "testdata", "compat", name) }
	for _, name := range []string{"release-list-native.json", "approval-service-post.json", "slash-command-import.json"} {
		body, err := os.ReadFile(fixturePath(name))
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal(body, &value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(body), "callback_secret\": \"secret") || strings.Contains(string(body), "production") {
			t.Fatalf("fixture is not redacted: %s", name)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "fixtures.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "fixture-token", "release-lists"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)
	body, _ := os.ReadFile(fixturePath("release-list-native.json"))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer fixture-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("native fixture status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestNativeCardValidationAndAuthentication(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "known-token", "general"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)
	tests := []struct {
		name, token, payload string
		status               int
	}{
		{name: "missing token", payload: `{"version":1,"title":"hello"}`, status: http.StatusUnauthorized},
		{name: "unknown token", token: "missing", payload: `{"version":1,"title":"hello"}`, status: http.StatusUnauthorized},
		{name: "unknown field", token: "known-token", payload: `{"version":1,"title":"hello","html":"<b>bad</b>"}`, status: http.StatusBadRequest},
		{name: "unknown channel", token: "known-token", payload: `{"version":1,"channel":"#missing","title":"hello"}`, status: http.StatusForbidden},
		{name: "multiple objects", token: "known-token", payload: `{"version":1,"title":"hello"}{"version":1,"title":"again"}`, status: http.StatusBadRequest},
		{name: "structured metric value", token: "known-token", payload: `{"version":1,"title":"hello","metrics":[{"label":"bad","value":{"html":"no"}}]}`, status: http.StatusBadRequest},
		{name: "javascript action", token: "known-token", payload: `{"version":1,"title":"hello","actions":[{"label":"bad","type":"link","url":"javascript:alert(1)"}]}`, status: http.StatusBadRequest},
		{name: "javascript image", token: "known-token", payload: `{"version":1,"title":"hello","images":[{"url":"javascript:alert(1)","alt":"bad"}]}`, status: http.StatusBadRequest},
		{name: "unsupported badge tone", token: "known-token", payload: `{"version":1,"title":"hello","badges":[{"label":"bad","tone":"rainbow"}]}`, status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", bytes.NewBufferString(test.payload))
			request.Header.Set("Content-Type", "application/json")
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %q", recorder.Code, test.status, recorder.Body.String())
			}
		})
	}
}

func TestNotificationSearchAndFilters(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "ops-token", "operations"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)
	webhook := postJSON(t, handler, "/hooks/ops-token", []byte(`{"text":"database backup completed","username":"backup-bot"}`))
	if webhook.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d", webhook.StatusCode)
	}
	webhook.Body.Close()
	native := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", bytes.NewBufferString(`{"version":1,"title":"Disk almost full","summary":"web01 needs attention","severity":"critical","source":"monitor","rows":[{"primary":"web01","tags":["storage"]}]}`))
	native.Header.Set("Content-Type", "application/json")
	native.Header.Set("Authorization", "Bearer ops-token")
	nativeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(nativeRecorder, native)
	if nativeRecorder.Code != http.StatusCreated {
		t.Fatalf("native status = %d", nativeRecorder.Code)
	}

	tests := []struct {
		name, path string
		count      int
	}{
		{name: "text search", path: "/api/v1/notifications?q=backup", count: 1},
		{name: "native row search", path: "/api/v1/notifications?q=storage", count: 1},
		{name: "channel", path: "/api/v1/notifications?channel=operations", count: 2},
		{name: "state received", path: "/api/v1/notifications?state=received", count: 2},
		{name: "state firing", path: "/api/v1/notifications?state=firing", count: 0},
		{name: "state acknowledged", path: "/api/v1/notifications?state=acknowledged", count: 0},
		{name: "state dismissed", path: "/api/v1/notifications?state=dismissed", count: 0},
		{name: "severity", path: "/api/v1/notifications?severity=critical", count: 1},
		{name: "combined no match", path: "/api/v1/notifications?q=backup&severity=critical", count: 0},
		{name: "escaped wildcard", path: "/api/v1/notifications?q=%25", count: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
			}
			var result struct {
				Notifications []store.Notification `json:"notifications"`
			}
			if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if len(result.Notifications) != test.count {
				t.Fatalf("count = %d, want %d", len(result.Notifications), test.count)
			}
		})
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/notifications?limit=201", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad limit status = %d", bad.Code)
	}
	badState := httptest.NewRecorder()
	handler.ServeHTTP(badState, httptest.NewRequest(http.MethodGet, "/api/v1/notifications?state=invalid", nil))
	if badState.Code != http.StatusBadRequest {
		t.Fatalf("bad state status = %d", badState.Code)
	}
}

func TestSimpleMessagePublishing(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "script-token", "scripts"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)
	tests := []struct {
		name, contentType, payload string
		status                     int
	}{
		{name: "plain text", contentType: "text/plain; charset=utf-8", payload: " nightly job completed\n", status: http.StatusCreated},
		{name: "generic JSON", contentType: "application/json", payload: `{"text":"cache warmed","source":"warmup"}`, status: http.StatusCreated},
		{name: "empty", contentType: "text/plain", payload: "  ", status: http.StatusBadRequest},
		{name: "unknown JSON field", contentType: "application/json", payload: `{"text":"hello","html":"bad"}`, status: http.StatusBadRequest},
		{name: "unsupported type", contentType: "application/xml", payload: "<text>bad</text>", status: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewBufferString(test.payload))
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Authorization", "Bearer script-token")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %q", recorder.Code, test.status, recorder.Body.String())
			}
		})
	}
	notifications, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 2 {
		t.Fatalf("notification count = %d", len(notifications))
	}
	byText := map[string]store.Notification{}
	for _, notification := range notifications {
		byText[notification.Text] = notification
	}
	if byText["cache warmed"].Username != "warmup" {
		t.Fatalf("JSON notification = %#v", byText["cache warmed"])
	}
	if byText["nightly job completed"].Username != "publisher" {
		t.Fatalf("plain notification = %#v", byText["nightly job completed"])
	}
}

func TestNotificationHistoryCursor(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "history-hook", "history"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)
	for _, text := range []string{"one", "two", "three"} {
		response := postJSON(t, handler, "/hooks/history-hook", []byte(`{"text":"`+text+`"}`))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("post %q status = %d", text, response.StatusCode)
		}
		response.Body.Close()
	}
	type page struct {
		Notifications []store.Notification `json:"notifications"`
		NextCursor    string               `json:"next_cursor"`
	}
	readPage := func(path string) page {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("page status = %d, body = %q", recorder.Code, recorder.Body.String())
		}
		var result page
		if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := readPage("/api/v1/notifications?limit=2")
	if len(first.Notifications) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	second := readPage("/api/v1/notifications?limit=2&before=" + url.QueryEscape(first.NextCursor))
	if len(second.Notifications) != 1 || second.NextCursor != "" {
		t.Fatalf("second page = %#v", second)
	}
	seen := map[string]bool{}
	for _, notification := range append(first.Notifications, second.Notifications...) {
		if seen[notification.ID] {
			t.Fatalf("duplicate notification %q across pages", notification.ID)
		}
		seen[notification.ID] = true
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/notifications?before=invalid!", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d", bad.Code)
	}
}

func TestReaderAuthentication(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "operator", "a secure test password"); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(context.Background(), "auth-hook", "private"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}

	protected := httptest.NewRecorder()
	handler.ServeHTTP(protected, httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil))
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", protected.Code)
	}

	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"operator","password":"a secure test password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	if loginRecorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %q", loginRecorder.Code, loginRecorder.Body.String())
	}
	cookies := loginRecorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value == "" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookies = %#v", cookies)
	}
	posted := postJSON(t, handler, "/hooks/auth-hook", []byte(`{"text":"new alert"}`))
	if posted.StatusCode != http.StatusOK {
		t.Fatalf("authenticated setup webhook status = %d", posted.StatusCode)
	}
	posted.Body.Close()

	authenticated := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	authenticated.AddCookie(cookies[0])
	authenticatedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(authenticatedRecorder, authenticated)
	if authenticatedRecorder.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d", authenticatedRecorder.Code)
	}
	var inbox struct {
		Notifications []store.Notification `json:"notifications"`
		UnreadCount   int                  `json:"unread_count"`
	}
	if err := json.NewDecoder(authenticatedRecorder.Body).Decode(&inbox); err != nil {
		t.Fatal(err)
	}
	if inbox.UnreadCount != 1 || len(inbox.Notifications) != 1 || !inbox.Notifications[0].Unread {
		t.Fatalf("unread inbox = %#v", inbox)
	}
	operator, err := db.AuthenticateUser(context.Background(), "operator", "a secure test password")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetNotificationState(context.Background(), inbox.Notifications[0].ID, operator, "acknowledged"); err != nil {
		t.Fatal(err)
	}
	listByQuery := func(query string) []store.Notification {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/notifications"+query, nil)
		request.AddCookie(cookies[0])
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("query %q status = %d", query, recorder.Code)
		}
		var filtered struct {
			Notifications []store.Notification `json:"notifications"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&filtered); err != nil {
			t.Fatal(err)
		}
		return filtered.Notifications
	}
	listByState := func(state string) []store.Notification {
		return listByQuery("?state=" + url.QueryEscape(state))
	}
	acknowledgedNotifications := listByState("acknowledged")
	if len(acknowledgedNotifications) != 1 || acknowledgedNotifications[0].State != "acknowledged" {
		t.Fatalf("state acknowledged = %#v", acknowledgedNotifications)
	}
	if err := db.SetNotificationState(context.Background(), inbox.Notifications[0].ID, operator, "resolved"); err != nil {
		t.Fatal(err)
	}
	resolvedNotifications := listByState("resolved")
	if len(resolvedNotifications) != 1 || resolvedNotifications[0].State != "resolved" {
		t.Fatalf("state resolved = %#v", resolvedNotifications)
	}
	channelsRequest := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	channelsRequest.AddCookie(cookies[0])
	channelsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(channelsRecorder, channelsRequest)
	var channelResult struct {
		Channels []store.ChannelSummary `json:"channels"`
	}
	if err := json.NewDecoder(channelsRecorder.Body).Decode(&channelResult); err != nil {
		t.Fatal(err)
	}
	if len(channelResult.Channels) != 1 || channelResult.Channels[0].Name != "private" || channelResult.Channels[0].DisplayName != "private" || channelResult.Channels[0].UnreadCount != 1 || channelResult.Channels[0].TotalCount != 1 {
		t.Fatalf("channels = %#v", channelResult.Channels)
	}
	markRead := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/read", nil)
	markRead.Header.Set("Origin", "http://example.com")
	markRead.Host = "example.com"
	markRead.AddCookie(cookies[0])
	markRecorder := httptest.NewRecorder()
	handler.ServeHTTP(markRecorder, markRead)
	if markRecorder.Code != http.StatusNoContent {
		t.Fatalf("mark read status = %d, body = %q", markRecorder.Code, markRecorder.Body.String())
	}
	readRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	readRequest.AddCookie(cookies[0])
	readRecorder := httptest.NewRecorder()
	handler.ServeHTTP(readRecorder, readRequest)
	var readInbox struct {
		Notifications []store.Notification `json:"notifications"`
		UnreadCount   int                  `json:"unread_count"`
	}
	if err := json.NewDecoder(readRecorder.Body).Decode(&readInbox); err != nil {
		t.Fatal(err)
	}
	if readInbox.UnreadCount != 0 || readInbox.Notifications[0].Unread {
		t.Fatalf("read inbox = %#v", readInbox)
	}
	unreadOnlyRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications?unread=1", nil)
	unreadOnlyRequest.AddCookie(cookies[0])
	unreadOnlyRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unreadOnlyRecorder, unreadOnlyRequest)
	if err := json.NewDecoder(unreadOnlyRecorder.Body).Decode(&readInbox); err != nil {
		t.Fatal(err)
	}
	if len(readInbox.Notifications) != 0 || readInbox.UnreadCount != 0 {
		t.Fatalf("unread-only inbox after mark read = %#v", readInbox)
	}
	updateInbox := func(action string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+inbox.Notifications[0].ID+"/inbox", bytes.NewBufferString(`{"action":"`+action+`"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		request.AddCookie(cookies[0])
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := updateInbox("unread"); recorder.Code != http.StatusNoContent {
		t.Fatalf("mark unread status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	unreadRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	unreadRequest.AddCookie(cookies[0])
	unreadRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unreadRecorder, unreadRequest)
	if err := json.NewDecoder(unreadRecorder.Body).Decode(&readInbox); err != nil {
		t.Fatal(err)
	}
	if readInbox.UnreadCount != 1 || !readInbox.Notifications[0].Unread {
		t.Fatalf("marked-unread inbox = %#v", readInbox)
	}
	unreadOnlyRequest = httptest.NewRequest(http.MethodGet, "/api/v1/notifications?unread=1", nil)
	unreadOnlyRequest.AddCookie(cookies[0])
	unreadOnlyRecorder = httptest.NewRecorder()
	handler.ServeHTTP(unreadOnlyRecorder, unreadOnlyRequest)
	if err := json.NewDecoder(unreadOnlyRecorder.Body).Decode(&readInbox); err != nil {
		t.Fatal(err)
	}
	if len(readInbox.Notifications) != 1 || !readInbox.Notifications[0].Unread {
		t.Fatalf("unread-only inbox after mark unread = %#v", readInbox)
	}
	if recorder := updateInbox("dismiss"); recorder.Code != http.StatusNoContent {
		t.Fatalf("dismiss status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	dismissedByState := listByState("dismissed")
	if len(dismissedByState) != 1 || dismissedByState[0].ID != inbox.Notifications[0].ID {
		t.Fatalf("state dismissed = %#v", dismissedByState)
	}
	included := listByQuery("?include_dismissed=true")
	if len(included) != 1 || included[0].ID != inbox.Notifications[0].ID {
		t.Fatalf("query include_dismissed = %#v", included)
	}
	dismissedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	dismissedRequest.AddCookie(cookies[0])
	dismissedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(dismissedRecorder, dismissedRequest)
	var dismissedInbox struct {
		Notifications []store.Notification `json:"notifications"`
		UnreadCount   int                  `json:"unread_count"`
	}
	if err := json.NewDecoder(dismissedRecorder.Body).Decode(&dismissedInbox); err != nil {
		t.Fatal(err)
	}
	if dismissedInbox.UnreadCount != 0 || len(dismissedInbox.Notifications) != 0 {
		t.Fatalf("dismissed inbox = %#v", dismissedInbox)
	}
	if recorder := updateInbox("restore"); recorder.Code != http.StatusNoContent {
		t.Fatalf("restore status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	restoredRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	restoredRequest.AddCookie(cookies[0])
	restoredRecorder := httptest.NewRecorder()
	handler.ServeHTTP(restoredRecorder, restoredRequest)
	if err := json.NewDecoder(restoredRecorder.Body).Decode(&dismissedInbox); err != nil {
		t.Fatal(err)
	}
	if dismissedInbox.UnreadCount != 0 || len(dismissedInbox.Notifications) != 1 || dismissedInbox.Notifications[0].Unread {
		t.Fatalf("restored inbox = %#v", dismissedInbox)
	}

	logout := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	logout.Header.Set("Origin", "http://example.com")
	logout.Host = "example.com"
	logout.AddCookie(cookies[0])
	logoutRecorder := httptest.NewRecorder()
	handler.ServeHTTP(logoutRecorder, logout)
	if logoutRecorder.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d", logoutRecorder.Code)
	}
	after := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	after.AddCookie(cookies[0])
	afterRecorder := httptest.NewRecorder()
	handler.ServeHTTP(afterRecorder, after)
	if afterRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out status = %d", afterRecorder.Code)
	}
}

func TestCanonicalPublicURLSecuresProxySessionsAndOrigin(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "public-url.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "operator", "a secure test password"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, PublicURL: "https://tintwire.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	login := func(origin, host string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"operator","password":"a secure test password"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		request.Host = host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	wrongOrigin := login("https://evil.example", "tintwire.example.com")
	if wrongOrigin.Code != http.StatusForbidden {
		t.Fatalf("wrong origin status=%d", wrongOrigin.Code)
	}
	wrongHost := login("https://tintwire.example.com", "internal-proxy")
	if wrongHost.Code != http.StatusForbidden {
		t.Fatalf("wrong host status=%d", wrongHost.Code)
	}
	valid := login("https://tintwire.example.com", "tintwire.example.com")
	if valid.Code != http.StatusOK {
		t.Fatalf("valid login status=%d body=%q", valid.Code, valid.Body.String())
	}
	cookies := valid.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("public cookie=%#v", cookies)
	}
	if _, err := server.NewWithOptions(db, server.Options{PublicURL: "https://example.com/path"}); err == nil {
		t.Fatal("public URL with path was accepted")
	}
}

func TestChannelAdministration(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure channel password"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure channel password"}`))
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	cookie := loginRecorder.Result().Cookies()[0]

	create := httptest.NewRequest(http.MethodPost, "/api/v1/channels", bytes.NewBufferString(`{"name":"security","display_name":"Security alerts","description":"Private security feed","accent_color":"#e5484d","visibility":"private"}`))
	create.Header.Set("Origin", "http://example.com")
	create.Host = "example.com"
	create.AddCookie(cookie)
	createRecorder := httptest.NewRecorder()
	handler.ServeHTTP(createRecorder, create)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %q", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		Channel         store.ChannelSummary `json:"channel"`
		PublishingToken string               `json:"publishing_token"`
	}
	if err := json.NewDecoder(createRecorder.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Channel.Name != "security" || created.Channel.Visibility != "private" || len(created.PublishingToken) != 64 {
		t.Fatalf("created = %#v", created)
	}

	message := httptest.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewBufferString("credential rotated"))
	message.Header.Set("Content-Type", "text/plain")
	message.Header.Set("Authorization", "Bearer "+created.PublishingToken)
	messageRecorder := httptest.NewRecorder()
	handler.ServeHTTP(messageRecorder, message)
	if messageRecorder.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, body = %q", messageRecorder.Code, messageRecorder.Body.String())
	}
	var published map[string]string
	if err := json.NewDecoder(messageRecorder.Body).Decode(&published); err != nil {
		t.Fatal(err)
	}

	duplicate := httptest.NewRequest(http.MethodPost, "/api/v1/channels", bytes.NewBufferString(`{"name":"security"}`))
	duplicate.Header.Set("Origin", "http://example.com")
	duplicate.Host = "example.com"
	duplicate.AddCookie(cookie)
	duplicateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(duplicateRecorder, duplicate)
	if duplicateRecorder.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, body = %q", duplicateRecorder.Code, duplicateRecorder.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	list.AddCookie(cookie)
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, list)
	var channels struct {
		Channels []store.ChannelSummary `json:"channels"`
	}
	if err := json.NewDecoder(listRecorder.Body).Decode(&channels); err != nil {
		t.Fatal(err)
	}
	if len(channels.Channels) != 1 || channels.Channels[0].TotalCount != 1 {
		t.Fatalf("channels = %#v", channels.Channels)
	}

	createUser := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{"username":"viewer","password":"secure viewer password"}`))
	createUser.Header.Set("Origin", "http://example.com")
	createUser.Host = "example.com"
	createUser.AddCookie(cookie)
	createUserRecorder := httptest.NewRecorder()
	handler.ServeHTTP(createUserRecorder, createUser)
	if createUserRecorder.Code != http.StatusCreated {
		t.Fatalf("create user status = %d, body = %q", createUserRecorder.Code, createUserRecorder.Body.String())
	}
	viewerLogin := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"viewer","password":"secure viewer password"}`))
	viewerLogin.Header.Set("Origin", "http://example.com")
	viewerLogin.Host = "example.com"
	viewerLoginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(viewerLoginRecorder, viewerLogin)
	viewerCookie := viewerLoginRecorder.Result().Cookies()[0]
	viewerList := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	viewerList.AddCookie(viewerCookie)
	viewerListRecorder := httptest.NewRecorder()
	handler.ServeHTTP(viewerListRecorder, viewerList)
	var beforeMembership struct {
		Channels []store.ChannelSummary `json:"channels"`
	}
	if err := json.NewDecoder(viewerListRecorder.Body).Decode(&beforeMembership); err != nil {
		t.Fatal(err)
	}
	if len(beforeMembership.Channels) != 0 {
		t.Fatalf("private channels leaked to non-member: %#v", beforeMembership.Channels)
	}
	viewerInboxBeforeMembership := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	viewerInboxBeforeMembership.AddCookie(viewerCookie)
	viewerInboxBeforeMembershipRecorder := httptest.NewRecorder()
	handler.ServeHTTP(viewerInboxBeforeMembershipRecorder, viewerInboxBeforeMembership)
	var hiddenInbox struct {
		Notifications []store.Notification `json:"notifications"`
		UnreadCount   int                  `json:"unread_count"`
	}
	if err := json.NewDecoder(viewerInboxBeforeMembershipRecorder.Body).Decode(&hiddenInbox); err != nil {
		t.Fatal(err)
	}
	if len(hiddenInbox.Notifications) != 0 || hiddenInbox.UnreadCount != 0 {
		t.Fatalf("private unread data leaked to non-member: %#v", hiddenInbox)
	}
	leakedEvents := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/"+published["id"]+"/events", nil)
	leakedEvents.AddCookie(viewerCookie)
	leakedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(leakedRecorder, leakedEvents)
	if leakedRecorder.Code != http.StatusNotFound {
		t.Fatalf("private activity leak status = %d", leakedRecorder.Code)
	}

	membership := httptest.NewRequest(http.MethodPut, "/api/v1/channels/"+created.Channel.ID+"/members/viewer", bytes.NewBufferString(`{"role":"viewer"}`))
	membership.Header.Set("Origin", "http://example.com")
	membership.Host = "example.com"
	membership.AddCookie(cookie)
	membershipRecorder := httptest.NewRecorder()
	handler.ServeHTTP(membershipRecorder, membership)
	if membershipRecorder.Code != http.StatusNoContent {
		t.Fatalf("membership status = %d, body = %q", membershipRecorder.Code, membershipRecorder.Body.String())
	}
	viewerList = httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	viewerList.AddCookie(viewerCookie)
	viewerListRecorder = httptest.NewRecorder()
	handler.ServeHTTP(viewerListRecorder, viewerList)
	var afterMembership struct {
		Channels []store.ChannelSummary `json:"channels"`
	}
	if err := json.NewDecoder(viewerListRecorder.Body).Decode(&afterMembership); err != nil {
		t.Fatal(err)
	}
	if len(afterMembership.Channels) != 1 || afterMembership.Channels[0].Name != "security" {
		t.Fatalf("member channels = %#v", afterMembership.Channels)
	}
	viewerInbox := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	viewerInbox.AddCookie(viewerCookie)
	viewerInboxRecorder := httptest.NewRecorder()
	handler.ServeHTTP(viewerInboxRecorder, viewerInbox)
	var viewerNotifications struct {
		Notifications []store.Notification `json:"notifications"`
	}
	if err := json.NewDecoder(viewerInboxRecorder.Body).Decode(&viewerNotifications); err != nil {
		t.Fatal(err)
	}
	if len(viewerNotifications.Notifications) != 1 || viewerNotifications.Notifications[0].CanOperate {
		t.Fatalf("viewer notifications = %#v", viewerNotifications.Notifications)
	}
	membership = httptest.NewRequest(http.MethodPut, "/api/v1/channels/"+created.Channel.ID+"/members/viewer", bytes.NewBufferString(`{"role":"operator"}`))
	membership.Header.Set("Origin", "http://example.com")
	membership.Host = "example.com"
	membership.AddCookie(cookie)
	membershipRecorder = httptest.NewRecorder()
	handler.ServeHTTP(membershipRecorder, membership)
	if membershipRecorder.Code != http.StatusNoContent {
		t.Fatalf("operator membership status = %d", membershipRecorder.Code)
	}
	viewerInbox = httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	viewerInbox.AddCookie(viewerCookie)
	viewerInboxRecorder = httptest.NewRecorder()
	handler.ServeHTTP(viewerInboxRecorder, viewerInbox)
	if err := json.NewDecoder(viewerInboxRecorder.Body).Decode(&viewerNotifications); err != nil {
		t.Fatal(err)
	}
	notification := viewerNotifications.Notifications[0]
	if !notification.CanOperate {
		t.Fatalf("operator cannot operate notification: %#v", notification)
	}
	ack := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notification.ID+"/state", bytes.NewBufferString(`{"state":"acknowledged"}`))
	ack.Header.Set("Origin", "http://example.com")
	ack.Host = "example.com"
	ack.AddCookie(viewerCookie)
	ackRecorder := httptest.NewRecorder()
	handler.ServeHTTP(ackRecorder, ack)
	if ackRecorder.Code != http.StatusNoContent {
		t.Fatalf("acknowledge status = %d, body = %q", ackRecorder.Code, ackRecorder.Body.String())
	}
	resolve := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notification.ID+"/state", bytes.NewBufferString(`{"state":"resolved"}`))
	resolve.Header.Set("Origin", "http://example.com")
	resolve.Host = "example.com"
	resolve.AddCookie(viewerCookie)
	resolveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(resolveRecorder, resolve)
	if resolveRecorder.Code != http.StatusNoContent {
		t.Fatalf("resolve status = %d", resolveRecorder.Code)
	}
	invalid := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notification.ID+"/state", bytes.NewBufferString(`{"state":"acknowledged"}`))
	invalid.Header.Set("Origin", "http://example.com")
	invalid.Host = "example.com"
	invalid.AddCookie(viewerCookie)
	invalidRecorder := httptest.NewRecorder()
	handler.ServeHTTP(invalidRecorder, invalid)
	if invalidRecorder.Code != http.StatusConflict {
		t.Fatalf("resolved-to-acknowledged status = %d", invalidRecorder.Code)
	}
	eventsRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/"+notification.ID+"/events", nil)
	eventsRequest.AddCookie(viewerCookie)
	eventsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(eventsRecorder, eventsRequest)
	var lifecycle struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(eventsRecorder.Body).Decode(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.Events) != 3 || lifecycle.Events[1]["actor"] != "viewer" || lifecycle.Events[1]["state"] != "acknowledged" || lifecycle.Events[2]["state"] != "resolved" {
		t.Fatalf("lifecycle events = %#v", lifecycle.Events)
	}

	const importedSecret = "mattermost-secret-hook-id"
	importPayload := `{"dry_run":true,"webhooks":[{"id":"` + importedSecret + `","channel":"security"}]}`
	importRequest := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/webhooks", bytes.NewBufferString(importPayload))
	importRequest.Header.Set("Origin", "http://example.com")
	importRequest.Host = "example.com"
	importRequest.AddCookie(cookie)
	importRecorder := httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importRequest)
	if importRecorder.Code != http.StatusOK || bytes.Contains(importRecorder.Body.Bytes(), []byte(importedSecret)) {
		t.Fatalf("dry-run import status = %d, body = %q", importRecorder.Code, importRecorder.Body.String())
	}
	notImported := postJSON(t, handler, "/hooks/"+importedSecret, []byte(`{"text":"not yet"}`))
	if notImported.StatusCode != http.StatusNotFound {
		t.Fatalf("dry-run created webhook, status = %d", notImported.StatusCode)
	}
	notImported.Body.Close()
	importPayload = `{"webhooks":[{"id":"` + importedSecret + `","channel":"security"}]}`
	importRequest = httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/webhooks", bytes.NewBufferString(importPayload))
	importRequest.Header.Set("Origin", "http://example.com")
	importRequest.Host = "example.com"
	importRequest.AddCookie(cookie)
	importRecorder = httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importRequest)
	if importRecorder.Code != http.StatusOK || bytes.Contains(importRecorder.Body.Bytes(), []byte(importedSecret)) {
		t.Fatalf("import status = %d, body = %q", importRecorder.Code, importRecorder.Body.String())
	}
	webhooks, err := db.ListWebhooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, webhook := range webhooks {
		if webhook.Channel == "security" && webhook.ChannelLocked {
			t.Fatalf("imported webhook should allow channel overrides by default: %#v", webhook)
		}
	}
	imported := postJSON(t, handler, "/hooks/"+importedSecret, []byte(`{"text":"import worked"}`))
	if imported.StatusCode != http.StatusOK {
		t.Fatalf("imported webhook status = %d", imported.StatusCode)
	}
	imported.Body.Close()
	importRequest = httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/webhooks", bytes.NewBufferString(importPayload))
	importRequest.Header.Set("Origin", "http://example.com")
	importRequest.Host = "example.com"
	importRequest.AddCookie(cookie)
	importRecorder = httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importRequest)
	if importRecorder.Code != http.StatusOK || !strings.Contains(importRecorder.Body.String(), `"existing":1`) {
		t.Fatalf("idempotent import status = %d, body = %q", importRecorder.Code, importRecorder.Body.String())
	}
	if err := db.BootstrapWebhook(context.Background(), "other-token", "other"); err != nil {
		t.Fatal(err)
	}
	conflictPayload := `{"webhooks":[{"id":"` + importedSecret + `","channel":"other"}]}`
	conflictRequest := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/webhooks", bytes.NewBufferString(conflictPayload))
	conflictRequest.Header.Set("Origin", "http://example.com")
	conflictRequest.Host = "example.com"
	conflictRequest.AddCookie(cookie)
	conflictRecorder := httptest.NewRecorder()
	handler.ServeHTTP(conflictRecorder, conflictRequest)
	if conflictRecorder.Code != http.StatusConflict || bytes.Contains(conflictRecorder.Body.Bytes(), []byte(importedSecret)) {
		t.Fatalf("conflict status = %d, body = %q", conflictRecorder.Code, conflictRecorder.Body.String())
	}
}

func TestRegisteredHTTPActionIsRedactedAuthorizedAndIdempotent(t *testing.T) {
	var calls atomic.Int32
	var redirectedCalls atomic.Int32
	redirectDestination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectedCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer redirectDestination.Close()
	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectDestination.URL, http.StatusFound)
	}))
	defer redirectSource.Close()
	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxTestActionResponse+1))
	}))
	defer oversized.Close()
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer callback-secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		contextValue, _ := body["context"].(map[string]any)
		if contextValue["decision_id"] != "dec-123" {
			t.Errorf("callback body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Approval recorded"}`)
	}))
	defer callback.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure action password"); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(context.Background(), "action-hook", "approvals"); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, ActionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure action password"}`))
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	cookie := loginRecorder.Result().Cookies()[0]
	register := httptest.NewRequest(http.MethodPut, "/api/v1/action-targets/approval-service", bytes.NewBufferString(fmt.Sprintf(`{"url":%q,"bearer_token":"callback-secret","allow_private":true}`, callback.URL)))
	register.Header.Set("Origin", "http://example.com")
	register.Host = "example.com"
	register.AddCookie(cookie)
	registerRecorder := httptest.NewRecorder()
	handler.ServeHTTP(registerRecorder, register)
	if registerRecorder.Code != http.StatusOK || strings.Contains(registerRecorder.Body.String(), "callback-secret") {
		t.Fatalf("register status=%d body=%q", registerRecorder.Code, registerRecorder.Body.String())
	}
	blocked := httptest.NewRequest(http.MethodPut, "/api/v1/action-targets/blocked", bytes.NewBufferString(fmt.Sprintf(`{"url":%q}`, callback.URL)))
	blocked.Header.Set("Origin", "http://example.com")
	blocked.Host = "example.com"
	blocked.AddCookie(cookie)
	blockedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(blockedRecorder, blocked)
	if blockedRecorder.Code != http.StatusBadRequest {
		t.Fatalf("blocked target status=%d", blockedRecorder.Code)
	}
	registerTarget := func(name, targetURL string) {
		request := httptest.NewRequest(http.MethodPut, "/api/v1/action-targets/"+name, bytes.NewBufferString(fmt.Sprintf(`{"url":%q,"allow_private":true}`, targetURL)))
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("register %s status=%d body=%q", name, recorder.Code, recorder.Body.String())
		}
	}
	registerTarget("redirect", redirectSource.URL)
	registerTarget("oversized", oversized.URL)
	card := []byte(`{"version":1,"title":"Approve remediation","summary":"Restart web01","severity":"warning","source":"approval-service","actions":[{"label":"Approve","type":"http","target":"approval-service","context":{"decision_id":"dec-123","private":"must-not-leak"}},{"label":"Redirect","type":"http","target":"redirect","context":{}},{"label":"Oversized","type":"http","target":"oversized","context":{}}]}`)
	publish := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", bytes.NewReader(card))
	publish.Header.Set("Content-Type", "application/json")
	publish.Header.Set("Authorization", "Bearer action-hook")
	publishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(publishRecorder, publish)
	if publishRecorder.Code != http.StatusCreated {
		t.Fatalf("publish status=%d body=%q", publishRecorder.Code, publishRecorder.Body.String())
	}
	var published map[string]string
	if err := json.NewDecoder(publishRecorder.Body).Decode(&published); err != nil {
		t.Fatal(err)
	}
	storedNotifications, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedNotifications[0].Card, []byte("dec-123")) || bytes.Contains(storedNotifications[0].Card, []byte("must-not-leak")) {
		t.Fatalf("stored card context is plaintext: %s", storedNotifications[0].Card)
	}
	storedEvents, err := db.ListNotificationEvents(context.Background(), published["id"])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedEvents[0].RawPayload, []byte("dec-123")) {
		t.Fatalf("stored event context is plaintext: %s", storedEvents[0].RawPayload)
	}
	list := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	list.AddCookie(cookie)
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, list)
	if strings.Contains(listRecorder.Body.String(), "dec-123") || strings.Contains(listRecorder.Body.String(), "must-not-leak") || strings.Contains(listRecorder.Body.String(), `"target"`) {
		t.Fatalf("browser API leaked server-held action data: %s", listRecorder.Body.String())
	}
	execute := func(key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+published["id"]+"/actions/0", nil)
		request.Header.Set("Origin", "http://example.com")
		request.Header.Set("Idempotency-Key", key)
		request.Host = "example.com"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	first := execute("approval-op-0001")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "Approval recorded") {
		t.Fatalf("first action status=%d body=%q", first.Code, first.Body.String())
	}
	second := execute("approval-op-0001")
	if second.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%q", second.Code, second.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("callback calls=%d want 1", calls.Load())
	}
	redirectRequest := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+published["id"]+"/actions/1", nil)
	redirectRequest.Header.Set("Origin", "http://example.com")
	redirectRequest.Header.Set("Idempotency-Key", "redirect-op-0001")
	redirectRequest.Host = "example.com"
	redirectRequest.AddCookie(cookie)
	redirectRecorder := httptest.NewRecorder()
	handler.ServeHTTP(redirectRecorder, redirectRequest)
	if redirectRecorder.Code != http.StatusBadGateway || redirectedCalls.Load() != 0 {
		t.Fatalf("redirect status=%d destination calls=%d body=%q", redirectRecorder.Code, redirectedCalls.Load(), redirectRecorder.Body.String())
	}
	oversizedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+published["id"]+"/actions/2", nil)
	oversizedRequest.Header.Set("Origin", "http://example.com")
	oversizedRequest.Header.Set("Idempotency-Key", "oversized-op-0001")
	oversizedRequest.Host = "example.com"
	oversizedRequest.AddCookie(cookie)
	oversizedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(oversizedRecorder, oversizedRequest)
	if oversizedRecorder.Code != http.StatusBadGateway || !strings.Contains(oversizedRecorder.Body.String(), "too large") {
		t.Fatalf("oversized status=%d body=%q", oversizedRecorder.Code, oversizedRecorder.Body.String())
	}
	events := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/"+published["id"]+"/events", nil)
	events.AddCookie(cookie)
	eventsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(eventsRecorder, events)
	if !strings.Contains(eventsRecorder.Body.String(), "Approval recorded") || !strings.Contains(eventsRecorder.Body.String(), `"actor":"admin"`) {
		t.Fatalf("action audit=%s", eventsRecorder.Body.String())
	}
	deleteTarget := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/action-targets/approval-service", nil)
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	deleted := deleteTarget()
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete target status=%d body=%q", deleted.Code, deleted.Body.String())
	}
	if _, err := db.ActionTargetByName(context.Background(), "approval-service"); !errors.Is(err, store.ErrNotificationNotFound) {
		t.Fatalf("deleted target lookup: %v", err)
	}
	missing := deleteTarget()
	if missing.Code != http.StatusNotFound {
		t.Fatalf("delete missing target status=%d body=%q", missing.Code, missing.Body.String())
	}
	revoked := execute("approval-op-after-target-delete")
	if revoked.Code != http.StatusBadGateway || !strings.Contains(revoked.Body.String(), "not registered") {
		t.Fatalf("deleted target action status=%d body=%q", revoked.Code, revoked.Body.String())
	}
}

const maxTestActionResponse = 64 << 10

func TestMattermostBotCompatibilityBridge(t *testing.T) {
	var decisionCalls atomic.Int32
	decisionCallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		contextValue, _ := body["context"].(map[string]any)
		if body["user_name"] != "admin" || contextValue["callback_secret"] != "embedded-secret" || contextValue["decision_id"] != "decision-42" {
			t.Errorf("decision callback body=%#v", body)
		}
		_, _ = io.WriteString(w, `{"ephemeral_text":"Decision approved"}`)
	}))
	defer decisionCallback.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "mattermost-bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(context.Background(), "release-bot", "secure robot password", false); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(context.Background(), "channel-publisher", "release-lists"); err != nil {
		t.Fatal(err)
	}
	actionKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, ActionKey: actionKey})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure admin password"}`))
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	adminCookie := loginRecorder.Result().Cookies()[0]
	registerTarget := httptest.NewRequest(http.MethodPut, "/api/v1/action-targets/approval-service", bytes.NewBufferString(fmt.Sprintf(`{"url":%q,"allow_private":true}`, decisionCallback.URL)))
	registerTarget.Header.Set("Origin", "http://example.com")
	registerTarget.Host = "example.com"
	registerTarget.AddCookie(adminCookie)
	registerRecorder := httptest.NewRecorder()
	handler.ServeHTTP(registerRecorder, registerTarget)
	if registerRecorder.Code != http.StatusOK {
		t.Fatalf("register decision target status=%d body=%q", registerRecorder.Code, registerRecorder.Body.String())
	}
	const botToken = "existing-mattermost-bot-token"
	importRequest := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/mattermost-bot", bytes.NewBufferString(`{"token":"`+botToken+`","username":"release-bot","team":"operations","channel":"release-lists"}`))
	importRequest.Header.Set("Origin", "http://example.com")
	importRequest.Host = "example.com"
	importRequest.AddCookie(adminCookie)
	importRecorder := httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importRequest)
	if importRecorder.Code != http.StatusOK || strings.Contains(importRecorder.Body.String(), botToken) {
		t.Fatalf("import status=%d body=%q", importRecorder.Code, importRecorder.Body.String())
	}
	botRequest := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, body)
		request.Header.Set("Authorization", "Bearer "+botToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v4/users/me", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
	me := botRequest(http.MethodGet, "/api/v4/users/me", nil)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"username":"release-bot"`) {
		t.Fatalf("me status=%d body=%q", me.Code, me.Body.String())
	}
	foreignUser := botRequest(http.MethodGet, "/api/v4/users/username/admin", nil)
	if foreignUser.Code != http.StatusNotFound {
		t.Fatalf("foreign user lookup status=%d body=%q", foreignUser.Code, foreignUser.Body.String())
	}
	user := botRequest(http.MethodGet, "/api/v4/users/username/release-bot", nil)
	if user.Code != http.StatusOK || !strings.Contains(user.Body.String(), `"username":"release-bot"`) {
		t.Fatalf("user lookup status=%d body=%q", user.Code, user.Body.String())
	}
	channel := botRequest(http.MethodGet, "/api/v4/teams/name/operations/channels/name/release-lists", nil)
	if channel.Code != http.StatusOK {
		t.Fatalf("channel status=%d body=%q", channel.Code, channel.Body.String())
	}
	var channelValue map[string]any
	if err := json.NewDecoder(channel.Body).Decode(&channelValue); err != nil {
		t.Fatal(err)
	}
	channelID := channelValue["id"].(string)
	topPayload := fmt.Sprintf(`{"channel_id":%q,"message":"Search result ready","props":{"attachments":[{"color":"good","title":"Candidate","text":"Release.One","actions":[{"id":"approve","name":"Approve","type":"button","integration":{"url":%q,"context":{"decision_id":"decision-42","callback_secret":"embedded-secret"}}}]}]}}`, channelID, decisionCallback.URL)
	top := botRequest(http.MethodPost, "/api/v4/posts", bytes.NewBufferString(topPayload))
	if top.Code != http.StatusCreated {
		t.Fatalf("top post status=%d body=%q", top.Code, top.Body.String())
	}
	var topPost store.MattermostPost
	if err := json.NewDecoder(top.Body).Decode(&topPost); err != nil {
		t.Fatal(err)
	}
	reply := botRequest(http.MethodPost, "/api/v4/posts", bytes.NewBufferString(fmt.Sprintf(`{"channel_id":%q,"message":"Download queued","root_id":%q}`, channelID, topPost.ID)))
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply status=%d body=%q", reply.Code, reply.Body.String())
	}
	var replyPost store.MattermostPost
	if err := json.NewDecoder(reply.Body).Decode(&replyPost); err != nil {
		t.Fatal(err)
	}
	if replyPost.RootID != topPost.ID || replyPost.CreateAt <= topPost.CreateAt {
		t.Fatalf("reply=%#v top=%#v", replyPost, topPost)
	}
	posts := botRequest(http.MethodGet, "/api/v4/channels/"+channelID+"/posts?since="+strconv.FormatInt(topPost.CreateAt, 10), nil)
	if posts.Code != http.StatusOK {
		t.Fatalf("posts status=%d", posts.Code)
	}
	var postList struct {
		Order []string                        `json:"order"`
		Posts map[string]store.MattermostPost `json:"posts"`
	}
	if err := json.NewDecoder(posts.Body).Decode(&postList); err != nil {
		t.Fatal(err)
	}
	if len(postList.Order) != 1 || postList.Order[0] != replyPost.ID {
		t.Fatalf("posts=%#v", postList)
	}
	notifications, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].Text != "Search result ready" || notifications[0].EventCount != 2 {
		t.Fatalf("notifications=%#v", notifications)
	}
	if bytes.Contains(notifications[0].Attachments, []byte("embedded-secret")) || bytes.Contains(notifications[0].Attachments, []byte(decisionCallback.URL)) {
		t.Fatalf("stored attachment leaked callback data: %s", notifications[0].Attachments)
	}
	inboxRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	inboxRequest.AddCookie(adminCookie)
	inboxRecorder := httptest.NewRecorder()
	handler.ServeHTTP(inboxRecorder, inboxRequest)
	if strings.Contains(inboxRecorder.Body.String(), "embedded-secret") || strings.Contains(inboxRecorder.Body.String(), "context_cipher") || strings.Contains(inboxRecorder.Body.String(), decisionCallback.URL) {
		t.Fatalf("inbox leaked callback data: %s", inboxRecorder.Body.String())
	}
	executeDecision := func(key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notifications[0].ID+"/mattermost-actions/0/0", nil)
		request.Header.Set("Origin", "http://example.com")
		request.Header.Set("Idempotency-Key", key)
		request.Host = "example.com"
		request.AddCookie(adminCookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	decisionResult := executeDecision("decision-op-0001")
	if decisionResult.Code != http.StatusOK || !strings.Contains(decisionResult.Body.String(), "Decision approved") {
		t.Fatalf("decision action status=%d body=%q", decisionResult.Code, decisionResult.Body.String())
	}
	decisionRetry := executeDecision("decision-op-0001")
	if decisionRetry.Code != http.StatusOK || decisionCalls.Load() != 1 {
		t.Fatalf("decision retry status=%d calls=%d", decisionRetry.Code, decisionCalls.Load())
	}
	decisionDuplicate := executeDecision("decision-op-0002")
	if decisionDuplicate.Code != http.StatusOK || !strings.Contains(decisionDuplicate.Body.String(), `"action_index":0`) || decisionCalls.Load() != 1 {
		t.Fatalf("decision duplicate status=%d calls=%d body=%q", decisionDuplicate.Code, decisionCalls.Load(), decisionDuplicate.Body.String())
	}
	completedInboxRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	completedInboxRequest.AddCookie(adminCookie)
	completedInbox := httptest.NewRecorder()
	handler.ServeHTTP(completedInbox, completedInboxRequest)
	if completedInbox.Code != http.StatusOK {
		t.Fatalf("completed inbox status=%d body=%q", completedInbox.Code, completedInbox.Body.String())
	}
	var completedPayload struct {
		Notifications []struct {
			Attachments json.RawMessage `json:"attachments"`
		} `json:"notifications"`
	}
	if err := json.NewDecoder(completedInbox.Body).Decode(&completedPayload); err != nil {
		t.Fatal(err)
	}
	var completedAttachments []struct {
		Actions []struct {
			Executable bool `json:"executable"`
			Selected   bool `json:"selected"`
		} `json:"actions"`
		ActionResult store.MattermostActionResult `json:"action_result"`
	}
	if len(completedPayload.Notifications) != 1 || json.Unmarshal(completedPayload.Notifications[0].Attachments, &completedAttachments) != nil || len(completedAttachments) != 1 {
		t.Fatalf("completed inbox payload=%s", completedInbox.Body.String())
	}
	completedAttachment := completedAttachments[0]
	if len(completedAttachment.Actions) != 1 || completedAttachment.Actions[0].Executable || !completedAttachment.Actions[0].Selected || completedAttachment.ActionResult.Status != "succeeded" || completedAttachment.ActionResult.ResponseText != "Decision approved" || completedAttachment.ActionResult.Actor != "admin" || completedAttachment.ActionResult.ActionIndex != 0 {
		t.Fatalf("completed attachment=%#v", completedAttachment)
	}
	events, err := db.ListNotificationEvents(context.Background(), notifications[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	actors := map[string]bool{}
	for _, event := range events {
		actors[event.Actor] = true
	}
	if len(events) != 3 || !actors["admin"] || !actors["release-bot"] {
		t.Fatalf("events=%#v", events)
	}
	approval := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notifications[0].ID+"/approval", bytes.NewBufferString(`{"decision":"approve"}`))
	approval.Header.Set("Origin", "http://example.com")
	approval.Host = "example.com"
	approval.AddCookie(adminCookie)
	approvalRecorder := httptest.NewRecorder()
	handler.ServeHTTP(approvalRecorder, approval)
	if approvalRecorder.Code != http.StatusNoContent {
		t.Fatalf("approval status=%d body=%q", approvalRecorder.Code, approvalRecorder.Body.String())
	}
	reactions := botRequest(http.MethodGet, "/api/v4/posts/"+topPost.ID+"/reactions", nil)
	if reactions.Code != http.StatusOK {
		t.Fatalf("reactions status=%d body=%q", reactions.Code, reactions.Body.String())
	}
	var reactionValues []store.MattermostReaction
	if err := json.NewDecoder(reactions.Body).Decode(&reactionValues); err != nil {
		t.Fatal(err)
	}
	if len(reactionValues) != 1 || reactionValues[0].EmojiName != "white_check_mark" {
		t.Fatalf("reactions=%#v", reactionValues)
	}
	rejection := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notifications[0].ID+"/approval", bytes.NewBufferString(`{"decision":"reject"}`))
	rejection.Header.Set("Origin", "http://example.com")
	rejection.Host = "example.com"
	rejection.AddCookie(adminCookie)
	rejectionRecorder := httptest.NewRecorder()
	handler.ServeHTTP(rejectionRecorder, rejection)
	if rejectionRecorder.Code != http.StatusNoContent {
		t.Fatalf("rejection status=%d", rejectionRecorder.Code)
	}
	reactions = botRequest(http.MethodGet, "/api/v4/posts/"+topPost.ID+"/reactions", nil)
	if err := json.NewDecoder(reactions.Body).Decode(&reactionValues); err != nil {
		t.Fatal(err)
	}
	if len(reactionValues) != 1 || reactionValues[0].EmojiName != "x" {
		t.Fatalf("replaced reactions=%#v", reactionValues)
	}
	for attempt := 2; attempt < 240; attempt++ {
		response := botRequest(http.MethodPost, "/api/v4/posts", bytes.NewBufferString(fmt.Sprintf(`{"channel_id":%q,"message":"bounded reply","root_id":%q}`, channelID, topPost.ID)))
		if response.Code != http.StatusCreated {
			t.Fatalf("rate-limit setup post %d status=%d body=%q", attempt+1, response.Code, response.Body.String())
		}
	}
	limited := botRequest(http.MethodPost, "/api/v4/posts", bytes.NewBufferString(fmt.Sprintf(`{"channel_id":%q,"message":"one too many","root_id":%q}`, channelID, topPost.ID)))
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "60" {
		t.Fatalf("mattermost rate limit status=%d headers=%v", limited.Code, limited.Header())
	}
}

func TestSlashCommandCompatibility(t *testing.T) {
	var received url.Values
	var backendCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		received = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Immediate result","response_type":"ephemeral","goto_location":"javascript:alert(1)"}`)
	}))
	defer backend.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "slash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(context.Background(), "publisher", "operations"); err != nil {
		t.Fatal(err)
	}
	actionKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, ActionKey: actionKey, PublicURL: "https://tintwire.example"})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure admin password"}`))
	login.Header.Set("Origin", "https://tintwire.example")
	login.Host = "tintwire.example"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	cookie := loginRecorder.Result().Cookies()[0]
	if err := db.ImportMattermostBot(context.Background(), "bot-token", "admin", "ops", "operations"); err != nil {
		t.Fatal(err)
	}
	definition := fmt.Sprintf(`{"commands":[{"team":"ops","trigger":"lookup","display_name":"Lookup","description":"Find a thing","creator":"admin","method":"POST","url":%q,"token":"command-secret","allow_private":true,"autocomplete":true,"autocomplete_hint":"[query]"}]}`, backend.URL)
	importCommand := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/slash-commands", bytes.NewBufferString(body))
		r.Header.Set("Origin", "https://tintwire.example")
		r.Host = "tintwire.example"
		r.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder
	}
	first := importCommand(definition)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"created":1`) {
		t.Fatalf("import status=%d body=%q", first.Code, first.Body.String())
	}
	second := importCommand(definition)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"existing":1`) {
		t.Fatalf("reimport status=%d body=%q", second.Code, second.Body.String())
	}
	changed := importCommand(strings.Replace(definition, "command-secret", "changed-secret", 1))
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed token status=%d body=%q", changed.Code, changed.Body.String())
	}
	list := httptest.NewRequest(http.MethodGet, "/api/v1/commands?team=ops", nil)
	list.AddCookie(cookie)
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, list)
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), `"trigger":"/lookup"`) || strings.Contains(listRecorder.Body.String(), "command-secret") || strings.Contains(listRecorder.Body.String(), backend.URL) {
		t.Fatalf("list status=%d body=%q", listRecorder.Code, listRecorder.Body.String())
	}
	execute := httptest.NewRequest(http.MethodPost, "/api/v1/commands", bytes.NewBufferString(`{"team":"ops","channel":"operations","command":"/lookup","text":"release one"}`))
	execute.Header.Set("Origin", "https://tintwire.example")
	requestIdentity := "slash-operation-0001"
	execute.Header.Set("Idempotency-Key", requestIdentity)
	execute.Host = "tintwire.example"
	execute.AddCookie(cookie)
	executeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(executeRecorder, execute)
	if executeRecorder.Code != http.StatusOK || !strings.Contains(executeRecorder.Body.String(), "Immediate result") || strings.Contains(executeRecorder.Body.String(), "javascript:") {
		t.Fatalf("execute status=%d body=%q", executeRecorder.Code, executeRecorder.Body.String())
	}
	if received.Get("token") != "command-secret" || received.Get("user_name") != "admin" || received.Get("command") != "/lookup" || received.Get("text") != "release one" {
		t.Fatalf("command form=%v", received)
	}
	retry := httptest.NewRequest(http.MethodPost, "/api/v1/commands", bytes.NewBufferString(`{"team":"ops","channel":"operations","command":"/lookup","text":"release one"}`))
	retry.Header.Set("Origin", "https://tintwire.example")
	retry.Header.Set("Idempotency-Key", requestIdentity)
	retry.Host = "tintwire.example"
	retry.AddCookie(cookie)
	retryRecorder := httptest.NewRecorder()
	handler.ServeHTTP(retryRecorder, retry)
	if retryRecorder.Code != http.StatusOK || backendCalls.Load() != 1 || retryRecorder.Body.String() != executeRecorder.Body.String() {
		t.Fatalf("retry status=%d calls=%d body=%q", retryRecorder.Code, backendCalls.Load(), retryRecorder.Body.String())
	}
	responseURL, err := url.Parse(received.Get("response_url"))
	if err != nil {
		t.Fatal(err)
	}
	if responseURL.Scheme != "https" || responseURL.Host != "tintwire.example" {
		t.Fatalf("response URL did not use canonical public origin: %s", responseURL)
	}
	delayed := httptest.NewRequest(http.MethodPost, responseURL.RequestURI(), bytes.NewBufferString(`{"text":"Delayed result","response_type":"in_channel"}`))
	delayedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(delayedRecorder, delayed)
	if delayedRecorder.Code != http.StatusOK {
		t.Fatalf("delayed status=%d body=%q", delayedRecorder.Code, delayedRecorder.Body.String())
	}
	var execution struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(executeRecorder.Body.Bytes(), &execution); err != nil {
		t.Fatal(err)
	}
	responses := httptest.NewRequest(http.MethodGet, "/api/v1/commands/"+execution.ID+"/responses", nil)
	responses.AddCookie(cookie)
	responsesRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responsesRecorder, responses)
	if responsesRecorder.Code != http.StatusOK || !strings.Contains(responsesRecorder.Body.String(), "Immediate result") || !strings.Contains(responsesRecorder.Body.String(), "Delayed result") || strings.Contains(responsesRecorder.Body.String(), "command-secret") {
		t.Fatalf("responses status=%d body=%q", responsesRecorder.Code, responsesRecorder.Body.String())
	}
	for i := 0; i < 4; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, responseURL.RequestURI(), bytes.NewBufferString("ok")))
		if recorder.Code != http.StatusOK {
			t.Fatalf("delayed use %d status=%d", i, recorder.Code)
		}
	}
	exhausted := httptest.NewRecorder()
	handler.ServeHTTP(exhausted, httptest.NewRequest(http.MethodPost, responseURL.RequestURI(), bytes.NewBufferString("too late")))
	if exhausted.Code != http.StatusGone {
		t.Fatalf("exhausted status=%d", exhausted.Code)
	}
}

func TestActionKeyCanRecoverFromReplicatedSetting(t *testing.T) {
	var actionCalls atomic.Int32
	var actionAuth atomic.Value
	actionAuth.Store("")
	var slashCalls atomic.Int32
	var slashToken atomic.Value
	slashToken.Store("")

	db, err := store.Open(filepath.Join(t.TempDir(), "action-key-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure action password"); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(context.Background(), "action-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	actionTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actionCalls.Add(1)
		actionAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Action completed"}`)
	}))
	defer actionTarget.Close()
	slashCallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slashCalls.Add(1)
		_ = r.ParseForm()
		if token := r.Form.Get("token"); token != "" {
			slashToken.Store(token)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Slash completed","response_type":"ephemeral"}`)
	}))
	defer slashCallback.Close()

	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure action password"}`))
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	if loginRecorder.Code != http.StatusOK {
		t.Fatalf("login status=%d", loginRecorder.Code)
	}
	cookie := loginRecorder.Result().Cookies()[0]

	runRegister := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/api/v1/action-targets/approval-service", bytes.NewBufferString(`{"url":"`+actionTarget.URL+`","bearer_token":"callback-secret","allow_private":true}`))
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	registerRecorder := runRegister()
	if registerRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("register without key status=%d body=%q", registerRecorder.Code, registerRecorder.Body.String())
	}

	commandDefinition := fmt.Sprintf(`{"commands":[{"team":"ops","trigger":"recover","display_name":"Recover","description":"Test recovery","creator":"admin","method":"POST","url":%q,"token":"command-secret","allow_private":true,"autocomplete":true,"autocomplete_hint":"[query]"}]}`, slashCallback.URL)
	importCommand := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/slash-commands", bytes.NewBufferString(body))
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	beforeImport := importCommand(commandDefinition)
	if beforeImport.Code != http.StatusServiceUnavailable {
		t.Fatalf("import without key status=%d body=%q", beforeImport.Code, beforeImport.Body.String())
	}

	material := make([]byte, 32)
	for i := range material {
		material[i] = 7
	}
	actionKey := base64.StdEncoding.EncodeToString(material)
	if err := db.SaveSettings(context.Background(), map[string]string{"action_encryption_key": actionKey}); err != nil {
		t.Fatal(err)
	}

	registerRecorder = runRegister()
	if registerRecorder.Code != http.StatusOK || strings.Contains(registerRecorder.Body.String(), "callback-secret") {
		t.Fatalf("register with setting key status=%d body=%q", registerRecorder.Code, registerRecorder.Body.String())
	}

	if err := db.ImportMattermostBot(context.Background(), "slash-bot-token", "admin", "ops", "operations"); err != nil {
		t.Fatal(err)
	}
	afterImport := importCommand(commandDefinition)
	if afterImport.Code != http.StatusOK || !strings.Contains(afterImport.Body.String(), `"created":1`) {
		t.Fatalf("import with setting key status=%d body=%q", afterImport.Code, afterImport.Body.String())
	}

	card := []byte(`{"version":1,"title":"Decision","summary":"Recovery check","severity":"info","source":"approval-service","actions":[{"label":"Approve","type":"http","target":"approval-service","context":{"decision_id":"dec-1"}}]}`)
	publish := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", bytes.NewReader(card))
	publish.Header.Set("Content-Type", "application/json")
	publish.Header.Set("Authorization", "Bearer action-hook")
	publishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(publishRecorder, publish)
	if publishRecorder.Code != http.StatusCreated {
		t.Fatalf("publish status=%d body=%q", publishRecorder.Code, publishRecorder.Body.String())
	}
	var published map[string]string
	if err := json.NewDecoder(publishRecorder.Body).Decode(&published); err != nil {
		t.Fatal(err)
	}
	notificationID, ok := published["id"]
	if !ok || notificationID == "" {
		t.Fatalf("publish response = %#v", published)
	}

	executeAction := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notificationID+"/actions/0", nil)
	executeAction.Header.Set("Origin", "http://example.com")
	executeAction.Header.Set("Idempotency-Key", "recover-action-0001")
	executeAction.Host = "example.com"
	executeAction.AddCookie(cookie)
	executeActionRecorder := httptest.NewRecorder()
	handler.ServeHTTP(executeActionRecorder, executeAction)
	if executeActionRecorder.Code != http.StatusOK {
		t.Fatalf("action execute status=%d body=%q", executeActionRecorder.Code, executeActionRecorder.Body.String())
	}
	if actionCalls.Load() != 1 {
		t.Fatalf("action callback calls=%d", actionCalls.Load())
	}
	if actionAuth.Load().(string) != "Bearer callback-secret" {
		t.Fatalf("action callback auth=%q", actionAuth.Load())
	}

	executeSlash := httptest.NewRequest(http.MethodPost, "/api/v1/commands", bytes.NewBufferString(`{"team":"ops","channel":"operations","command":"/recover","text":"check"}`))
	executeSlash.Header.Set("Origin", "http://example.com")
	executeSlash.Header.Set("Idempotency-Key", "recover-slash-0001")
	executeSlash.Host = "example.com"
	executeSlash.AddCookie(cookie)
	executeSlashRecorder := httptest.NewRecorder()
	handler.ServeHTTP(executeSlashRecorder, executeSlash)
	if executeSlashRecorder.Code != http.StatusOK {
		t.Fatalf("slash execute status=%d body=%q", executeSlashRecorder.Code, executeSlashRecorder.Body.String())
	}
	if slashCalls.Load() != 1 {
		t.Fatalf("slash callback calls=%d", slashCalls.Load())
	}
	if slashToken.Load().(string) != "command-secret" {
		t.Fatalf("slash callback token=%q", slashToken.Load())
	}
}

func TestAlertmanagerFiringAndResolvedUpdateOneNotification(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "alert-hook", "prometheus"); err != nil {
		t.Fatal(err)
	}
	handler := server.New(db)

	firing := `{
        "channel":"prometheus",
        "attachments":[{
            "color":"danger",
            "title":"[FIRING:2] DiskSpaceLow for node-exporter (cluster=\"prod\")",
            "text":"two filesystems are full"
        }]
    }`
	resolved := `{
        "channel":"prometheus",
        "attachments":[{
            "color":"good",
            "title":"[RESOLVED] DiskSpaceLow for node-exporter (cluster=\"prod\")",
            "text":"filesystems recovered"
        }]
    }`

	response := postJSON(t, handler, "/hooks/alert-hook", []byte(firing))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("firing status = %d, body = %q", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()
	first, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].State != "firing" {
		t.Fatalf("firing notifications = %#v", first)
	}
	if first[0].EventCount != 1 {
		t.Fatalf("firing event count = %d, want 1", first[0].EventCount)
	}

	response = postJSON(t, handler, "/hooks/alert-hook", []byte(resolved))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolved status = %d, body = %q", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()
	latest, err := db.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 {
		t.Fatalf("notification count = %d, want 1", len(latest))
	}
	if latest[0].ID != first[0].ID || latest[0].State != "resolved" {
		t.Fatalf("resolved notification = %#v, first = %#v", latest[0], first[0])
	}
	if latest[0].EventCount != 2 {
		t.Fatalf("resolved event count = %d, want 2", latest[0].EventCount)
	}
	if !latest[0].UpdatedAt.After(latest[0].CreatedAt) {
		t.Fatalf("updated_at = %s, created_at = %s", latest[0].UpdatedAt, latest[0].CreatedAt)
	}
	events, err := db.ListNotificationEvents(context.Background(), latest[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].State != "firing" || events[1].State != "resolved" {
		t.Fatalf("events = %#v", events)
	}

	activityRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/"+latest[0].ID+"/events", nil)
	activityRecorder := httptest.NewRecorder()
	handler.ServeHTTP(activityRecorder, activityRequest)
	if activityRecorder.Code != http.StatusOK {
		t.Fatalf("activity status = %d, body = %q", activityRecorder.Code, activityRecorder.Body.String())
	}
	var activity struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(activityRecorder.Body).Decode(&activity); err != nil {
		t.Fatal(err)
	}
	if len(activity.Events) != 2 {
		t.Fatalf("activity events = %#v", activity.Events)
	}
	if activity.Events[0]["state"] != "firing" || activity.Events[1]["state"] != "resolved" {
		t.Fatalf("activity states = %#v", activity.Events)
	}
	if activity.Events[0]["title"] != "[FIRING:2] DiskSpaceLow for node-exporter (cluster=\"prod\")" {
		t.Fatalf("activity title = %#v", activity.Events[0]["title"])
	}
	if _, exposed := activity.Events[0]["raw_payload"]; exposed {
		t.Fatal("activity API exposed raw_payload")
	}

	missingRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/missing/events", nil)
	missingRecorder := httptest.NewRecorder()
	handler.ServeHTTP(missingRecorder, missingRequest)
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("missing activity status = %d, want 404", missingRecorder.Code)
	}
}

func TestEmbeddedWebAssets(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := server.New(db)

	for _, path := range []string{"/", "/assets/markdown.js", "/assets/app.js", "/assets/icon.svg", "/assets/icon-192.png", "/assets/icon-512.png", "/assets/apple-touch-icon.png", "/manifest.webmanifest", "/sw.js"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			if path == "/manifest.webmanifest" && response.Header.Get("Content-Type") != "application/manifest+json" {
				t.Fatalf("manifest content type = %q", response.Header.Get("Content-Type"))
			}
			if path == "/" && response.Header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("inbox cache control = %q", response.Header.Get("Cache-Control"))
			}
			if path == "/sw.js" && response.Header.Get("Service-Worker-Allowed") != "/" {
				t.Fatalf("service worker scope = %q", response.Header.Get("Service-Worker-Allowed"))
			}
			if path == "/sw.js" {
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range []string{"showNotification", "setAppBadge", "notificationclick"} {
					if !strings.Contains(string(body), marker) {
						t.Fatalf("service worker does not contain %q", marker)
					}
				}
			}
			if path == "/assets/app.js" {
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range []string{`"Incident state"`, `"Archive"`, `"Notification archived"`, `"Allow overrides"`, `"Lock to channel"`, `"channel-timeline-view"`, `list.scrollTop = list.scrollHeight`} {
					if !strings.Contains(string(body), marker) {
						t.Fatalf("inbox JavaScript does not contain %s", marker)
					}
				}
			}
			if path == "/" {
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range []string{`id="channel-list"`, `id="mobile-channel-list"`, `id="feed-title"`, `id="alert-dialog"`, `id="alert-setup-button"`, `<option value="dismissed">Archived</option>`} {
					if !strings.Contains(string(body), marker) {
						t.Fatalf("inbox HTML does not contain %s", marker)
					}
				}
				for _, asset := range []string{"/manifest.webmanifest", "/assets/sentinel.css", "/assets/markdown.js", "/assets/app.js"} {
					if !strings.Contains(string(body), asset+"?v=") {
						t.Fatalf("inbox HTML does not fingerprint %s", asset)
					}
				}
				if composer, list := strings.Index(string(body), `id="composer"`), strings.Index(string(body), `id="list"`); composer < 0 || list < 0 || list > composer {
					t.Fatal("channel composer must appear after the message history")
				}
			}
		})
	}
}

func TestPushSubscriptionEnrollmentRequiresSameOrigin(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler, err := server.NewWithOptions(db, server.Options{VAPIDContact: "mailto:admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	configRequest := httptest.NewRequest(http.MethodGet, "/api/v1/push/config", nil)
	configRecorder := httptest.NewRecorder()
	handler.ServeHTTP(configRecorder, configRequest)
	var config struct {
		Enabled   bool   `json:"enabled"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(configRecorder.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.PublicKey == "" {
		t.Fatalf("push config = %#v", config)
	}

	payload := []byte(`{"endpoint":"https://push.example/device","expirationTime":null,"keys":{"p256dh":"public","auth":"secret"}}`)
	crossOrigin := httptest.NewRequest(http.MethodPost, "/api/v1/push/subscriptions", bytes.NewReader(payload))
	crossOrigin.Header.Set("Content-Type", "application/json")
	crossOrigin.Header.Set("Origin", "https://attacker.example")
	crossRecorder := httptest.NewRecorder()
	handler.ServeHTTP(crossRecorder, crossOrigin)
	if crossRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", crossRecorder.Code)
	}
	privatePayload := []byte(`{"endpoint":"https://127.0.0.1/push","expirationTime":null,"keys":{"p256dh":"public","auth":"secret"}}`)
	privateRequest := httptest.NewRequest(http.MethodPost, "/api/v1/push/subscriptions", bytes.NewReader(privatePayload))
	privateRequest.Header.Set("Content-Type", "application/json")
	privateRequest.Header.Set("Origin", "http://example.com")
	privateRequest.Host = "example.com"
	privateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(privateRecorder, privateRequest)
	if privateRecorder.Code != http.StatusBadRequest {
		t.Fatalf("private endpoint status = %d, want 400", privateRecorder.Code)
	}

	sameOrigin := httptest.NewRequest(http.MethodPost, "/api/v1/push/subscriptions", bytes.NewReader(payload))
	sameOrigin.Header.Set("Content-Type", "application/json")
	sameOrigin.Header.Set("Origin", "http://example.com")
	sameOrigin.Host = "example.com"
	sameRecorder := httptest.NewRecorder()
	handler.ServeHTTP(sameRecorder, sameOrigin)
	if sameRecorder.Code != http.StatusOK {
		t.Fatalf("same-origin status = %d, body = %q", sameRecorder.Code, sameRecorder.Body.String())
	}
	subscriptions, err := db.ListPushSubscriptions(context.Background())
	if err != nil || len(subscriptions) != 1 {
		t.Fatalf("subscriptions = %#v, error = %v", subscriptions, err)
	}
}

func TestEventStreamConnects(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := server.New(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}
	if body := readBody(t, response); body != ": connected\n\n" {
		t.Fatalf("body = %q", body)
	}
}

func postJSON(t *testing.T, handler http.Handler, path string, payload []byte) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
