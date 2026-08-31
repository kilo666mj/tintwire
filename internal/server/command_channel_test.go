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
	"sync/atomic"
	"testing"
	"time"

	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

func TestChannelSlashCommandTimelineAndVisibility(t *testing.T) {
	var received url.Values
	var backendCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		_ = r.ParseForm()
		received = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Immediate channel result","response_type":"ephemeral"}`)
	}))
	defer backend.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "channel-slash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.BootstrapUser(ctx, "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(ctx, "bob", "secure bob password", false); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, PublicURL: "https://tintwire.example", ActionKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	login := func(username, password string) *http.Cookie {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"`+username+`","password":"`+password+`"}`))
		request.Header.Set("Origin", "https://tintwire.example")
		request.Host = "tintwire.example"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("login %s status=%d body=%q", username, recorder.Code, recorder.Body.String())
		}
		return recorder.Result().Cookies()[0]
	}
	adminCookie := login("admin", "secure admin password")
	bobCookie := login("bob", "secure bob password")

	// Create the channel and a bot import to establish the team alias.
	if err := db.BootstrapWebhook(ctx, "channel-publisher", "ops"); err != nil {
		t.Fatal(err)
	}
	channel, err := db.ChannelIDByName(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ImportMattermostBot(ctx, "bot-token", "admin", "ops-team", "ops"); err != nil {
		t.Fatal(err)
	}
	definition := fmt.Sprintf(`{"commands":[{"team":"ops-team","trigger":"lookup","display_name":"Lookup","description":"Find a thing","creator":"admin","method":"POST","url":%q,"token":"command-secret","allow_private":true}]}`, backend.URL)
	importCommand := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/slash-commands", bytes.NewBufferString(definition))
	importCommand.Header.Set("Origin", "https://tintwire.example")
	importCommand.Host = "tintwire.example"
	importCommand.AddCookie(adminCookie)
	importRecorder := httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importCommand)
	if importRecorder.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%q", importRecorder.Code, importRecorder.Body.String())
	}

	runCommandIn := func(channelID string, cookie *http.Cookie, key, text string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channelID+"/commands", bytes.NewBufferString(`{"text":"`+text+`"}`))
		request.Header.Set("Origin", "https://tintwire.example")
		request.Header.Set("Idempotency-Key", key)
		request.Host = "tintwire.example"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	runCommand := func(cookie *http.Cookie, key, text string) *httptest.ResponseRecorder {
		return runCommandIn(channel, cookie, key, text)
	}

	created := runCommand(adminCookie, "channel-op-0001", "/lookup release one")
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), "Immediate channel result") {
		t.Fatalf("execute status=%d body=%q", created.Code, created.Body.String())
	}
	if received.Get("token") != "command-secret" || received.Get("user_name") != "admin" || received.Get("command") != "/lookup" || received.Get("text") != "release one" || received.Get("team_id") != "ops-team" || received.Get("channel_name") != "ops" {
		t.Fatalf("command form=%v", received)
	}
	var execution struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &execution); err != nil {
		t.Fatal(err)
	}

	// Idempotency: retry with the same key replays without a second backend call.
	retry := runCommand(adminCookie, "channel-op-0001", "/lookup release one")
	if retry.Code != http.StatusOK || backendCalls.Load() != 1 || retry.Body.String() != created.Body.String() {
		t.Fatalf("retry status=%d calls=%d body=%q", retry.Code, backendCalls.Load(), retry.Body.String())
	}
	// Different input with the same key must conflict.
	conflict := runCommand(adminCookie, "channel-op-0001", "/lookup different")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%q", conflict.Code, conflict.Body.String())
	}

	// Mattermost commands are available throughout their team. A Tintwire
	// channel without a compatibility alias can therefore use a trigger that
	// belongs to exactly one imported team.
	if err := db.BootstrapWebhook(ctx, "general-publisher", "general"); err != nil {
		t.Fatal(err)
	}
	general, err := db.ChannelIDByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	unmapped := runCommandIn(general, adminCookie, "channel-op-unmapped-0001", "/lookup release two")
	if unmapped.Code != http.StatusOK || received.Get("team_id") != "ops-team" || received.Get("channel_name") != "general" {
		t.Fatalf("unmapped execute status=%d body=%q form=%v", unmapped.Code, unmapped.Body.String(), received)
	}

	// Do not guess when the same trigger exists in more than one team.
	ambiguousDefinition := fmt.Sprintf(`{"commands":[{"team":"other-team","trigger":"lookup","display_name":"Other lookup","description":"Find another thing","creator":"admin","method":"POST","url":%q,"token":"other-secret","allow_private":true}]}`, backend.URL)
	ambiguousImport := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/slash-commands", bytes.NewBufferString(ambiguousDefinition))
	ambiguousImport.Header.Set("Origin", "https://tintwire.example")
	ambiguousImport.Host = "tintwire.example"
	ambiguousImport.AddCookie(adminCookie)
	ambiguousRecorder := httptest.NewRecorder()
	handler.ServeHTTP(ambiguousRecorder, ambiguousImport)
	if ambiguousRecorder.Code != http.StatusOK {
		t.Fatalf("ambiguous import status=%d body=%q", ambiguousRecorder.Code, ambiguousRecorder.Body.String())
	}
	ambiguous := runCommandIn(general, adminCookie, "channel-op-unmapped-0002", "/lookup release three")
	if ambiguous.Code != http.StatusNotFound {
		t.Fatalf("ambiguous execute status=%d body=%q", ambiguous.Code, ambiguous.Body.String())
	}

	timeline := func(cookie *http.Cookie) []store.TimelineItem {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel+"/timeline", nil)
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("timeline status=%d body=%q", recorder.Code, recorder.Body.String())
		}
		var body struct {
			Items []store.TimelineItem `json:"items"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Items
	}

	// The invoker sees the ephemeral command response in the timeline.
	adminItems := timeline(adminCookie)
	if len(adminItems) != 1 || adminItems[0].Kind != "command" || adminItems[0].Command == nil || !strings.Contains(adminItems[0].Command.Text, "Immediate channel result") || adminItems[0].Command.ResponseType != "ephemeral" {
		t.Fatalf("admin timeline=%#v", adminItems)
	}
	// A different user must not see the invoker's ephemeral response.
	bobItems := timeline(bobCookie)
	if len(bobItems) != 0 {
		t.Fatalf("bob saw admin ephemeral response: %#v", bobItems)
	}
	bob, err := db.AuthenticateUser(ctx, "bob", "secure bob password")
	if err != nil {
		t.Fatal(err)
	}
	if unread, err := db.UnreadCount(ctx, bob); err != nil || unread != 0 {
		t.Fatalf("bob ephemeral unread = %d, error = %v", unread, err)
	}
}

func TestChannelSlashCommandInChannelSharedVisibility(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Shared result","response_type":"in_channel"}`)
	}))
	defer backend.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "channel-slash-shared.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.BootstrapUser(ctx, "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(ctx, "bob", "secure bob password", false); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, ActionKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	login := func(username, password string) *http.Cookie {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"`+username+`","password":"`+password+`"}`))
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result().Cookies()[0]
	}
	adminCookie := login("admin", "secure admin password")
	bobCookie := login("bob", "secure bob password")

	if err := db.BootstrapWebhook(ctx, "publisher", "ops"); err != nil {
		t.Fatal(err)
	}
	channel, err := db.ChannelIDByName(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ImportMattermostBot(ctx, "bot-token", "admin", "ops-team", "ops"); err != nil {
		t.Fatal(err)
	}
	definition := fmt.Sprintf(`{"commands":[{"team":"ops-team","trigger":"announce","display_name":"Announce","description":"Post a shared result","creator":"admin","method":"POST","url":%q,"token":"secret","allow_private":true}]}`, backend.URL)
	importCommand := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/slash-commands", bytes.NewBufferString(definition))
	importCommand.Header.Set("Origin", "http://example.com")
	importCommand.Host = "example.com"
	importCommand.AddCookie(adminCookie)
	importRecorder := httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importCommand)
	if importRecorder.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%q", importRecorder.Code, importRecorder.Body.String())
	}
	run := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel+"/commands", bytes.NewBufferString(`{"text":"/announce release two"}`))
	run.Header.Set("Origin", "http://example.com")
	run.Header.Set("Idempotency-Key", "channel-op-shared-0001")
	run.Host = "example.com"
	run.AddCookie(adminCookie)
	runRecorder := httptest.NewRecorder()
	handler.ServeHTTP(runRecorder, run)
	if runRecorder.Code != http.StatusOK {
		t.Fatalf("run status=%d body=%q", runRecorder.Code, runRecorder.Body.String())
	}

	timeline := func(cookie *http.Cookie) []store.TimelineItem {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel+"/timeline", nil)
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var body struct {
			Items []store.TimelineItem `json:"items"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Items
	}
	// In-channel output is shared, so both users see it.
	var sharedResponseID string
	for name, cookie := range map[string]*http.Cookie{"admin": adminCookie, "bob": bobCookie} {
		items := timeline(cookie)
		if len(items) != 1 || items[0].Kind != "command" || !strings.Contains(items[0].Command.Text, "Shared result") {
			t.Fatalf("%s timeline=%#v", name, items)
		}
		sharedResponseID = items[0].Command.ID
	}
	bob, err := db.AuthenticateUser(ctx, "bob", "secure bob password")
	if err != nil {
		t.Fatal(err)
	}
	if unread, err := db.UnreadCount(ctx, bob); err != nil || unread != 1 {
		t.Fatalf("bob shared-command unread = %d, error = %v", unread, err)
	}
	if err := db.MarkChannelRead(ctx, bob, channel, time.Now()); err != nil {
		t.Fatal(err)
	}
	if unread, err := db.UnreadCount(ctx, bob); err != nil || unread != 0 {
		t.Fatalf("bob shared-command unread after mark = %d, error = %v", unread, err)
	}
	admin, err := db.AuthenticateUser(ctx, "admin", "secure admin password")
	if err != nil {
		t.Fatal(err)
	}
	for _, subscription := range []store.PushSubscription{
		{UserID: admin.ID, Endpoint: "https://push.example/admin-command", P256DH: "admin-key", Auth: "admin-auth"},
		{UserID: bob.ID, Endpoint: "https://push.example/bob-command", P256DH: "bob-key", Auth: "bob-auth"},
	} {
		if err := db.SavePushSubscription(ctx, subscription); err != nil {
			t.Fatal(err)
		}
	}
	commandPush, err := db.ListPushSubscriptionsForCommandResponse(ctx, sharedResponseID)
	if err != nil || len(commandPush) != 1 || commandPush[0].UserID != bob.ID {
		t.Fatalf("shared command push subscriptions = %#v, error = %v", commandPush, err)
	}
	if err := db.SetChannelNotificationPreference(ctx, bob, channel, "critical"); err != nil {
		t.Fatal(err)
	}
	commandPush, err = db.ListPushSubscriptionsForCommandResponse(ctx, sharedResponseID)
	if err != nil || len(commandPush) != 0 {
		t.Fatalf("critical-only command push subscriptions = %#v, error = %v", commandPush, err)
	}
}

func TestChannelCommandOutputSanitization(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Search result","response_type":"in_channel","username":"lookup-bot","icon_url":"https://social.example/lookup.png","attachments":[{"title":"Candidate","text":"Release.One","actions":[{"name":"Approve","type":"button","integration":{"url":"https://callback.example/act","context":{"secret":"top-secret"}}}]}]}`)
	}))
	defer backend.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "channel-attach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.BootstrapUser(ctx, "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, PublicURL: "https://tintwire.example", ActionKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure admin password"}`))
	login.Header.Set("Origin", "https://tintwire.example")
	login.Host = "tintwire.example"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	cookie := loginRecorder.Result().Cookies()[0]

	if err := db.BootstrapWebhook(ctx, "publisher", "ops"); err != nil {
		t.Fatal(err)
	}
	channel, err := db.ChannelIDByName(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ImportMattermostBot(ctx, "bot-token", "admin", "ops-team", "ops"); err != nil {
		t.Fatal(err)
	}
	definition := fmt.Sprintf(`{"commands":[{"team":"ops-team","trigger":"scan","display_name":"Scan","description":"Scan","creator":"admin","method":"POST","url":%q,"token":"secret","allow_private":true}]}`, backend.URL)
	importCommand := httptest.NewRequest(http.MethodPost, "/api/v1/admin/import/slash-commands", bytes.NewBufferString(definition))
	importCommand.Header.Set("Origin", "https://tintwire.example")
	importCommand.Host = "tintwire.example"
	importCommand.AddCookie(cookie)
	importRecorder := httptest.NewRecorder()
	handler.ServeHTTP(importRecorder, importCommand)
	if importRecorder.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%q", importRecorder.Code, importRecorder.Body.String())
	}

	run := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel+"/commands", bytes.NewBufferString(`{"text":"/scan release one"}`))
	run.Header.Set("Origin", "https://tintwire.example")
	run.Header.Set("Idempotency-Key", "channel-attach-0001")
	run.Host = "tintwire.example"
	run.AddCookie(cookie)
	runRecorder := httptest.NewRecorder()
	handler.ServeHTTP(runRecorder, run)
	if runRecorder.Code != http.StatusOK {
		t.Fatalf("run status=%d body=%q", runRecorder.Code, runRecorder.Body.String())
	}

	timeline := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel+"/timeline", nil)
	timeline.AddCookie(cookie)
	timelineRecorder := httptest.NewRecorder()
	handler.ServeHTTP(timelineRecorder, timeline)
	body := timelineRecorder.Body.String()
	if timelineRecorder.Code != http.StatusOK {
		t.Fatalf("timeline status=%d body=%q", timelineRecorder.Code, body)
	}
	if !strings.Contains(body, `"username":"lookup-bot"`) || !strings.Contains(body, `"icon_url":"https://social.example/lookup.png"`) {
		t.Fatalf("command override missing from timeline: %s", body)
	}
	if strings.Contains(body, "callback.example") || strings.Contains(body, "top-secret") || strings.Contains(body, "integration") || strings.Contains(body, "\"context\"") {
		t.Fatalf("command output leaked callback secrets: %s", body)
	}
}

func TestChannelCommandRejectsSlashTextAndChannelAccess(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "channel-reject.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.BootstrapUser(ctx, "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, ActionKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure admin password"}`))
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	cookie := loginRecorder.Result().Cookies()[0]

	if err := db.BootstrapWebhook(ctx, "publisher", "ops"); err != nil {
		t.Fatal(err)
	}
	channel, err := db.ChannelIDByName(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	// A message that does not begin with '/' is not a command.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel+"/commands", bytes.NewBufferString(`{"text":"not a command"}`))
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("Idempotency-Key", "channel-op-reject-0001")
	request.Host = "example.com"
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("non-slash status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
