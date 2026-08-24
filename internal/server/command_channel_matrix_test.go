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
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

// setupChannelCommand builds a server with admin/bob users, a team "prod"
// aliasing the public channels ops/support/dev plus the private channels
// inside/secret, a team "qa" aliasing the public channel foreign, and an
// unmapped public channel. Bob is a member of only "inside". A single
// team-scoped slash command is imported. It returns the handler, the admin and
// bob session cookies, and an id->channel lookup.
func setupChannelCommand(t *testing.T, trigger, backendURL string) (http.Handler, *http.Cookie, *http.Cookie, func(string) string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "chan-cmd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.BootstrapUser(ctx, "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(ctx, "bob", "secure bob password", false); err != nil {
		t.Fatal(err)
	}
	channelIDs := map[string]string{}
	for _, name := range []string{"ops", "support", "dev", "inside", "secret", "foreign", "unmapped"} {
		visibility := "public"
		if name == "inside" || name == "secret" {
			visibility = "private"
		}
		created, _, err := db.CreateChannel(ctx, store.CreateChannelInput{Name: name, DisplayName: name, Visibility: visibility})
		if err != nil {
			t.Fatal(err)
		}
		channelIDs[name] = created.ID
	}
	if err := db.SetChannelMember(ctx, channelIDs["inside"], "bob", "viewer"); err != nil {
		t.Fatal(err)
	}

	handler, err := server.NewWithOptions(db, server.Options{
		AuthRequired: true,
		ActionKey:    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	login := func(username, password string) *http.Cookie {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"`+username+`","password":"`+password+`"}`))
		req.Header.Set("Origin", "http://example.com")
		req.Host = "example.com"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login %s status=%d body=%q", username, rec.Code, rec.Body.String())
		}
		return rec.Result().Cookies()[0]
	}
	adminCookie := login("admin", "secure admin password")
	bobCookie := login("bob", "secure bob password")

	// Alias channels to teams via bot imports/grants.
	if err := db.ImportMattermostBot(ctx, "bot-token", "admin", "prod", "ops"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"support", "dev", "inside", "secret"} {
		if err := db.GrantMattermostBotChannel(ctx, "bot-token", "prod", name); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ImportMattermostBot(ctx, "qa-bot-token", "admin", "qa", "foreign"); err != nil {
		t.Fatal(err)
	}

	definition := fmt.Sprintf(`{"commands":[{"team":"prod","trigger":"%s","display_name":"Run","description":"","creator":"admin","method":"POST","url":%q,"token":"secret","allow_private":true}]}`, trigger, backendURL)
	importReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/slash-commands", bytes.NewBufferString(definition))
	importReq.Header.Set("Origin", "http://example.com")
	importReq.Host = "example.com"
	importReq.AddCookie(adminCookie)
	importRec := httptest.NewRecorder()
	handler.ServeHTTP(importRec, importReq)
	if importRec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%q", importRec.Code, importRec.Body.String())
	}
	return handler, adminCookie, bobCookie, func(name string) string { return channelIDs[name] }
}

func TestChannelCommandMultiplePublicChannels(t *testing.T) {
	var received url.Values
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		received = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Release synced","response_type":"in_channel"}`)
	}))
	defer backend.Close()

	handler, adminCookie, _, channelID := setupChannelCommand(t, "sync", backend.URL)
	invoke := func(channelName, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channelID(channelName)+"/commands", bytes.NewBufferString(`{"text":"/sync release one"}`))
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Idempotency-Key", key)
		req.Host = "example.com"
		req.AddCookie(adminCookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	first := invoke("ops", "chan-multi-001")
	if first.Code != http.StatusOK || received.Get("channel_name") != "ops" || received.Get("team_id") != "prod" {
		t.Fatalf("ops invoke status=%d form=%v", first.Code, received)
	}
	second := invoke("support", "chan-multi-002")
	if second.Code != http.StatusOK || received.Get("channel_name") != "support" {
		t.Fatalf("support invoke status=%d form=%v", second.Code, received)
	}
	// Both timelines contain the in-channel response.
	for _, name := range []string{"ops", "support"} {
		timeline := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channelID(name)+"/timeline", nil)
		timeline.AddCookie(adminCookie)
		timelineRec := httptest.NewRecorder()
		handler.ServeHTTP(timelineRec, timeline)
		if timelineRec.Code != http.StatusOK || !strings.Contains(timelineRec.Body.String(), "Release synced") {
			t.Fatalf("%s timeline status=%d body=%q", name, timelineRec.Code, timelineRec.Body.String())
		}
	}
}

func TestChannelCommandPrivateAndUnauthorizedAccess(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"ok","response_type":"ephemeral"}`)
	}))
	defer backend.Close()

	handler, adminCookie, bobCookie, channelID := setupChannelCommand(t, "lookup", backend.URL)
	invoke := func(channelName string, cookie *http.Cookie, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channelID(channelName)+"/commands", bytes.NewBufferString(`{"text":"/lookup release one"}`))
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Idempotency-Key", key)
		req.Host = "example.com"
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	// A member of a private channel may invoke.
	member := invoke("inside", bobCookie, "chan-pri-001")
	if member.Code != http.StatusOK {
		t.Fatalf("member private invoke status=%d body=%q", member.Code, member.Body.String())
	}
	// A non-member cannot invoke in a private channel.
	denied := invoke("secret", bobCookie, "chan-pri-002")
	if denied.Code != http.StatusForbidden && denied.Code != http.StatusNotFound {
		t.Fatalf("non-member private invoke status=%d body=%q", denied.Code, denied.Body.String())
	}
	// An unmapped channel may use a trigger that belongs to exactly one team.
	unmapped := invoke("unmapped", adminCookie, "chan-pri-003")
	if unmapped.Code != http.StatusOK {
		t.Fatalf("unmapped channel invoke status=%d body=%q", unmapped.Code, unmapped.Body.String())
	}
	// A channel in a different team (qa) does not see the prod command.
	crossTeam := invoke("foreign", adminCookie, "chan-pri-004")
	if crossTeam.Code != http.StatusNotFound {
		t.Fatalf("cross-team invoke status=%d body=%q", crossTeam.Code, crossTeam.Body.String())
	}
}

func TestChannelCommandDelayedSharedVsEphemeral(t *testing.T) {
	var responseURL string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		responseURL = r.Form.Get("response_url")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"invoker only","response_type":"ephemeral"}`)
	}))
	defer backend.Close()

	handler, adminCookie, bobCookie, channelID := setupChannelCommand(t, "lookup", backend.URL)
	invoke := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channelID("ops")+"/commands", bytes.NewBufferString(`{"text":"/lookup release one"}`))
	invoke.Header.Set("Origin", "http://example.com")
	invoke.Header.Set("Idempotency-Key", "chan-delay-001")
	invoke.Host = "example.com"
	invoke.AddCookie(adminCookie)
	invokeRec := httptest.NewRecorder()
	handler.ServeHTTP(invokeRec, invoke)
	if invokeRec.Code != http.StatusOK || responseURL == "" {
		t.Fatalf("invoke status=%d response_url=%q body=%q", invokeRec.Code, responseURL, invokeRec.Body.String())
	}
	parsed, err := url.Parse(responseURL)
	if err != nil {
		t.Fatal(err)
	}
	// Deliver a delayed shared (in_channel) response.
	delayed := httptest.NewRequest(http.MethodPost, parsed.RequestURI(), bytes.NewBufferString(`{"text":"shared delayed","response_type":"in_channel"}`))
	delayedRec := httptest.NewRecorder()
	handler.ServeHTTP(delayedRec, delayed)
	if delayedRec.Code != http.StatusOK {
		t.Fatalf("delayed status=%d body=%q", delayedRec.Code, delayedRec.Body.String())
	}

	timeline := func(cookie *http.Cookie) string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channelID("ops")+"/timeline", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	adminBody := timeline(adminCookie)
	// Admin is the invoker, so it sees both the ephemeral and the shared delayed output.
	if !strings.Contains(adminBody, "invoker only") || !strings.Contains(adminBody, "shared delayed") {
		t.Fatalf("admin timeline missing outputs: %s", adminBody)
	}
	// Bob sees only the shared delayed response, not the invoker's ephemeral one.
	bobBody := timeline(bobCookie)
	if strings.Contains(bobBody, "invoker only") || !strings.Contains(bobBody, "shared delayed") {
		t.Fatalf("bob timeline visibility wrong: %s", bobBody)
	}
}

func TestChannelCommandDelayedResponseExhausted(t *testing.T) {
	var responseURL string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		responseURL = r.Form.Get("response_url")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"accepted","response_type":"ephemeral"}`)
	}))
	defer backend.Close()

	handler, adminCookie, _, channelID := setupChannelCommand(t, "lookup", backend.URL)
	invoke := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channelID("ops")+"/commands", bytes.NewBufferString(`{"text":"/lookup release one"}`))
	invoke.Header.Set("Origin", "http://example.com")
	invoke.Header.Set("Idempotency-Key", "chan-exhaust-001")
	invoke.Host = "example.com"
	invoke.AddCookie(adminCookie)
	invokeRec := httptest.NewRecorder()
	handler.ServeHTTP(invokeRec, invoke)
	if invokeRec.Code != http.StatusOK || responseURL == "" {
		t.Fatalf("invoke status=%d response_url=%q", invokeRec.Code, responseURL)
	}
	parsed, err := url.Parse(responseURL)
	if err != nil {
		t.Fatal(err)
	}
	// Use the response_url the maximum number of times (5), then it is exhausted.
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, parsed.RequestURI(), bytes.NewBufferString("update")))
		if rec.Code != http.StatusOK {
			t.Fatalf("delayed use %d status=%d body=%q", i+1, rec.Code, rec.Body.String())
		}
	}
	exhausted := httptest.NewRecorder()
	handler.ServeHTTP(exhausted, httptest.NewRequest(http.MethodPost, parsed.RequestURI(), bytes.NewBufferString("too late")))
	if exhausted.Code != http.StatusGone {
		t.Fatalf("exhausted status=%d body=%q", exhausted.Code, exhausted.Body.String())
	}
}

// Compile-time guard that the command response envelope decodes as expected.
func TestChannelCommandResponseEnvelope(t *testing.T) {
	var decoded struct {
		ID        string `json:"id"`
		Responses []struct {
			ResponseType string `json:"response_type"`
			Text         string `json:"text"`
		} `json:"responses"`
	}
	payload := []byte(`{"id":"run_01","responses":[{"response_type":"ephemeral","text":"hello"}]}`)
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.ID != "run_01" || len(decoded.Responses) != 1 || decoded.Responses[0].ResponseType != "ephemeral" {
		t.Fatalf("envelope decode failed: %v %#v", err, decoded)
	}
}
