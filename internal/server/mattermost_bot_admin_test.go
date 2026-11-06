package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestAdminManagesMattermostBots(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mm-bots.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.BootstrapUser(ctx, "admin", "secure bot admin password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(ctx, "alice", "secure human password", false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ops", "support"} {
		if _, _, err := db.CreateChannel(ctx, store.CreateChannelInput{Name: name, Visibility: "public"}); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := NewWithOptions(db, Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	admin := func(method, path, body string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = bytes.NewBufferString(body)
		}
		request := httptest.NewRequest(method, path, reader)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		if cookie != nil {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	login := admin(http.MethodPost, "/api/v1/session", `{"username":"admin","password":"secure bot admin password"}`)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatalf("login status = %d", login.Code)
	}
	cookie = login.Result().Cookies()[0]
	bot := func(token string) int {
		request := httptest.NewRequest(http.MethodGet, "/api/v4/users/me", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}

	// A bot must never take over an existing human account.
	if got := admin(http.MethodPost, "/api/v1/admin/mattermost-bots", `{"username":"alice","team":"main","channel":"ops"}`); got.Code != http.StatusConflict {
		t.Fatalf("human takeover status = %d, body = %q", got.Code, got.Body.String())
	}

	created := admin(http.MethodPost, "/api/v1/admin/mattermost-bots", `{"username":"hermes","team":"main","channel":"ops"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %q", created.Code, created.Body.String())
	}
	var result struct {
		Bot   store.MattermostBotSummary `json:"bot"`
		Token string                     `json:"token"`
	}
	if err := json.NewDecoder(created.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Token == "" || result.Bot.ID == "" || result.Bot.Username != "hermes" || result.Bot.Team != "main" || result.Bot.Channel != "ops" {
		t.Fatalf("created = %+v", result)
	}
	if code := bot(result.Token); code != http.StatusOK {
		t.Fatalf("bot auth status = %d", code)
	}
	// The bot user has no password, so it cannot log in to the web UI.
	if got := admin(http.MethodPost, "/api/v1/session", `{"username":"hermes","password":"anything at all here"}`); got.Code == http.StatusOK {
		t.Fatal("credential-less bot user logged in")
	}

	if got := admin(http.MethodPost, "/api/v1/admin/mattermost-bots/"+result.Bot.ID+"/channels", `{"channel":"support"}`); got.Code != http.StatusNoContent {
		t.Fatalf("grant status = %d, body = %q", got.Code, got.Body.String())
	}
	listed := admin(http.MethodGet, "/api/v1/admin/mattermost-bots", "")
	var list struct {
		Bots []store.MattermostBotSummary `json:"bots"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Bots) != 1 || len(list.Bots[0].Channels) != 1 || list.Bots[0].Channels[0] != "support" {
		t.Fatalf("list = %+v", list.Bots)
	}

	if got := admin(http.MethodPost, "/api/v1/admin/mattermost-bots/"+result.Bot.ID+"/revoke", ""); got.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %q", got.Code, got.Body.String())
	}
	if code := bot(result.Token); code != http.StatusUnauthorized {
		t.Fatalf("revoked bot auth status = %d", code)
	}
	if got := admin(http.MethodPost, "/api/v1/admin/mattermost-bots/"+result.Bot.ID+"/revoke", ""); got.Code != http.StatusNotFound {
		t.Fatalf("second revoke status = %d", got.Code)
	}

	// The revoked bot's identity can be reissued with a fresh token.
	again := admin(http.MethodPost, "/api/v1/admin/mattermost-bots", `{"username":"hermes","team":"main","channel":"ops"}`)
	if again.Code != http.StatusCreated {
		t.Fatalf("recreate status = %d, body = %q", again.Code, again.Body.String())
	}
}
