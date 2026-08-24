package server_test

import (
	"bytes"
	"context"
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

func TestMattermostBotCrossChannel(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "bot-cross.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.BootstrapUser(ctx, "admin", "secure admin password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(ctx, "release-bot", "secure bot password", false); err != nil {
		t.Fatal(err)
	}
	supportChannel, _, err := db.CreateChannel(ctx, store.CreateChannelInput{Name: "support", DisplayName: "Support", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	secretChannel, _, err := db.CreateChannel(ctx, store.CreateChannelInput{Name: "secret", DisplayName: "Secret", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	const botToken = "cross-channel-bot-token"
	if err := db.ImportMattermostBot(ctx, botToken, "release-bot", "prod", "ops"); err != nil {
		t.Fatal(err)
	}
	// Grant the bot a second channel within the same team.
	if err := db.GrantMattermostBotChannel(ctx, botToken, "prod", "support"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	bot := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, body)
		request.Header.Set("Authorization", "Bearer "+botToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	// The bot can resolve the granted second channel via the team alias.
	resolved := bot(http.MethodGet, "/api/v4/teams/name/prod/channels/name/support", nil)
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve granted channel status=%d body=%q", resolved.Code, resolved.Body.String())
	}
	var supportResolved map[string]any
	if err := json.NewDecoder(resolved.Body).Decode(&supportResolved); err != nil {
		t.Fatal(err)
	}
	supportID, _ := supportResolved["id"].(string)
	if supportID != supportChannel.ID {
		t.Fatalf("support channel id=%q want %q", supportID, supportChannel.ID)
	}

	// Post into the second permitted channel; the notification must land there.
	supportPost := fmt.Sprintf(`{"channel_id":%q,"message":"support update"}`, supportID)
	created := bot(http.MethodPost, "/api/v4/posts", bytes.NewBufferString(supportPost))
	if created.Code != http.StatusCreated {
		t.Fatalf("post to granted channel status=%d body=%q", created.Code, created.Body.String())
	}
	posts := bot(http.MethodGet, "/api/v4/channels/"+supportID+"/posts", nil)
	if posts.Code != http.StatusOK || !strings.Contains(posts.Body.String(), "support update") {
		t.Fatalf("list posts status=%d body=%q", posts.Code, posts.Body.String())
	}

	// Messages sent from Tintwire's native composer must also be visible to a
	// legacy bot polling the Mattermost posts endpoint.
	admin, err := db.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	nativeMessage, err := db.CreateChannelMessage(ctx, admin, store.CreateMessageInput{ChannelID: supportID, Text: "backfill Ride.or.Die 1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ListMattermostPosts(ctx, supportID, nativeMessage.CreatedAt.UnixMilli()-1); err != nil {
		t.Fatalf("list native compatibility posts: %v", err)
	}
	nativePosts := bot(http.MethodGet, "/api/v4/channels/"+supportID+"/posts?since="+fmt.Sprint(nativeMessage.CreatedAt.UnixMilli()-1), nil)
	if nativePosts.Code != http.StatusOK {
		t.Fatalf("list native posts status=%d body=%q", nativePosts.Code, nativePosts.Body.String())
	}
	var nativePostList struct {
		Order []string                        `json:"order"`
		Posts map[string]store.MattermostPost `json:"posts"`
	}
	if err := json.NewDecoder(nativePosts.Body).Decode(&nativePostList); err != nil {
		t.Fatal(err)
	}
	gotNative, ok := nativePostList.Posts[nativeMessage.ID]
	if !ok || gotNative.Message != nativeMessage.Text || gotNative.UserID != admin.ID || gotNative.RootID != "" {
		t.Fatalf("native composer post missing from compatibility poll: %#v", nativePostList)
	}
	nativeReply := bot(http.MethodPost, "/api/v4/posts", bytes.NewBufferString(fmt.Sprintf(`{"channel_id":%q,"message":"Backfill help","root_id":%q}`, supportID, nativeMessage.ID)))
	if nativeReply.Code != http.StatusCreated {
		t.Fatalf("reply to native composer post status=%d body=%q", nativeReply.Code, nativeReply.Body.String())
	}
	var nativeReplyPost store.MattermostPost
	if err := json.NewDecoder(nativeReply.Body).Decode(&nativeReplyPost); err != nil {
		t.Fatal(err)
	}
	if nativeReplyPost.RootID != nativeMessage.ID || nativeReplyPost.UserID == admin.ID {
		t.Fatalf("native compatibility reply=%#v", nativeReplyPost)
	}

	// The bot must not be rerouted to its default channel.
	opsID, err := db.ChannelIDByName(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	opsPosts := bot(http.MethodGet, "/api/v4/channels/"+opsID+"/posts", nil)
	if opsPosts.Code != http.StatusOK || strings.Contains(opsPosts.Body.String(), "support update") {
		t.Fatalf("bot was silently rerouted to default channel: %q", opsPosts.Body.String())
	}

	// An unauthorized private channel is rejected (404 hides its existence).
	intrusion := fmt.Sprintf(`{"channel_id":%q,"message":"intrusion"}`, secretChannel.ID)
	rejected := bot(http.MethodPost, "/api/v4/posts", bytes.NewBufferString(intrusion))
	if rejected.Code == http.StatusCreated {
		t.Fatalf("unauthorized post unexpectedly succeeded: body=%q", rejected.Body.String())
	}

	// Unmapped and cross-team channels are not found.
	unmapped := bot(http.MethodGet, "/api/v4/teams/name/prod/channels/name/unknown", nil)
	if unmapped.Code != http.StatusNotFound {
		t.Fatalf("unmapped channel status=%d body=%q", unmapped.Code, unmapped.Body.String())
	}
	crossTeam := bot(http.MethodGet, "/api/v4/teams/name/other/channels/name/ops", nil)
	if crossTeam.Code != http.StatusNotFound {
		t.Fatalf("cross-team channel status=%d body=%q", crossTeam.Code, crossTeam.Body.String())
	}

	// The granted-channel notification is visible in the channel's timeline.
	adminLogin := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure admin password"}`))
	adminLogin.Header.Set("Origin", "http://example.com")
	adminLogin.Host = "example.com"
	adminLoginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(adminLoginRecorder, adminLogin)
	adminCookie := adminLoginRecorder.Result().Cookies()[0]
	timeline := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+supportID+"/timeline", nil)
	timeline.AddCookie(adminCookie)
	timelineRecorder := httptest.NewRecorder()
	handler.ServeHTTP(timelineRecorder, timeline)
	if timelineRecorder.Code != http.StatusOK || !strings.Contains(timelineRecorder.Body.String(), "support update") || !strings.Contains(timelineRecorder.Body.String(), "Backfill help") {
		t.Fatalf("support timeline status=%d body=%q", timelineRecorder.Code, timelineRecorder.Body.String())
	}
}
