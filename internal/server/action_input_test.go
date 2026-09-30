package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

func TestNativeActionInputAndViewedIdentity(t *testing.T) {
	var callbacks []map[string]any
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		callbacks = append(callbacks, body)
		_, _ = io.WriteString(w, `{"text":"Answer received"}`)
	}))
	defer callback.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "input.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.BootstrapUser(context.Background(), "admin", "input-test-password"); err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapWebhook(context.Background(), "input-hook", "test"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, ActionKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	request := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "example.com"
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if path == "/api/v1/notifications" {
			req.Header.Set("Authorization", "Bearer input-hook")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	login := request("POST", "/api/v1/session", `{"username":"admin","password":"input-test-password"}`, "")
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	cookie = login.Result().Cookies()[0]
	reg := request("PUT", "/api/v1/action-targets/answer", fmt.Sprintf(`{"url":%q,"allow_private":true}`, callback.URL), "")
	if reg.Code != 200 {
		t.Fatal(reg.Body.String())
	}
	publish := func(id string) string {
		body := fmt.Sprintf(`{"version":1,"title":"A question","summary":"Which resolver?","lifecycle_key":"question-1","actions":[{"id":%q,"label":"Send answer","type":"http","target":"answer","input":{"label":"Which resolver?","required":true},"context":{"trusted":"stored"}}]}`, id)
		rec := request("POST", "/api/v1/notifications", body, "")
		if rec.Code != 201 {
			t.Fatal(rec.Body.String())
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out["id"]
	}
	id := publish("question-generation-1")
	path := "/api/v1/notifications/" + id + "/actions/0"
	for i, body := range []string{`{}`, `{"action_id":"question-generation-0","input":"answer"}`, `{"action_id":"question-generation-1","input":" "}`, `{"action_id":"question-generation-1","input":"answer","actor":{"id":"spoof"}}`, fmt.Sprintf(`{"action_id":"question-generation-1","input":%q}`, strings.Repeat("é", 1001))} {
		rec := request("POST", path, body, fmt.Sprintf("invalid-input-%d", i))
		if rec.Code != 400 && rec.Code != 409 {
			t.Fatalf("invalid input: %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(callbacks) != 0 {
		t.Fatalf("invalid inputs dispatched: %d", len(callbacks))
	}
	body := `{"action_id":"question-generation-1","input":"Resolver café\nKeep current port."}`
	for i := 0; i < 2; i++ {
		rec := request("POST", path, body, "answer-operation-1")
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
	}
	if len(callbacks) != 1 {
		t.Fatalf("callback count=%d", len(callbacks))
	}
	if callbacks[0]["input"] != "Resolver café\nKeep current port." || callbacks[0]["actor"].(map[string]any)["username"] != "admin" || callbacks[0]["context"].(map[string]any)["trusted"] != "stored" {
		t.Fatalf("callback=%#v", callbacks[0])
	}
	if next := publish("question-generation-2"); next != id {
		t.Fatalf("card identity changed")
	}
	stale := request("POST", path, body, "stale-view-operation")
	if stale.Code != 409 || len(callbacks) != 1 {
		t.Fatalf("stale view dispatched: %d", stale.Code)
	}
	fresh := request("POST", path, `{"action_id":"question-generation-2","input":"New answer"}`, "answer-operation-2")
	if fresh.Code != 200 || len(callbacks) != 2 {
		t.Fatalf("fresh view: %d %s", fresh.Code, fresh.Body.String())
	}
}
