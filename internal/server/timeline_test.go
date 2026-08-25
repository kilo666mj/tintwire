package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

func timelineServer(t *testing.T) (http.Handler, *http.Cookie, store.ChannelSummary, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "timeline-server.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BootstrapUser(context.Background(), "admin", "secure timeline password"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure timeline password"}`))
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	if loginRecorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %q", loginRecorder.Code, loginRecorder.Body.String())
	}
	cookie := loginRecorder.Result().Cookies()[0]

	create := httptest.NewRequest(http.MethodPost, "/api/v1/channels", bytes.NewBufferString(`{"name":"chat","display_name":"Chat","visibility":"public"}`))
	create.Header.Set("Origin", "http://example.com")
	create.Host = "example.com"
	create.AddCookie(cookie)
	createRecorder := httptest.NewRecorder()
	handler.ServeHTTP(createRecorder, create)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create channel status = %d, body = %q", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		Channel         store.ChannelSummary `json:"channel"`
		PublishingToken string               `json:"publishing_token"`
	}
	if err := json.NewDecoder(createRecorder.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return handler, cookie, created.Channel, created.PublishingToken
}

func TestChannelTimelineRoundTrip(t *testing.T) {
	handler, cookie, channel, token := timelineServer(t)

	// Publish a notification card into the channel.
	card := httptest.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewBufferString("incident resolved"))
	card.Header.Set("Content-Type", "text/plain")
	card.Header.Set("Authorization", "Bearer "+token)
	cardRecorder := httptest.NewRecorder()
	handler.ServeHTTP(cardRecorder, card)
	if cardRecorder.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, body = %q", cardRecorder.Code, cardRecorder.Body.String())
	}

	postMessage := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+channel.ID+"/messages", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	messageResponse := postMessage(`{"text":"hello from the composer"}`)
	if messageResponse.Code != http.StatusCreated {
		t.Fatalf("message status = %d, body = %q", messageResponse.Code, messageResponse.Body.String())
	}
	var message store.ChannelMessage
	if err := json.NewDecoder(messageResponse.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	if message.Author != "admin" || message.ChannelID != channel.ID || message.RootID != message.ID {
		t.Fatalf("created message = %#v", message)
	}

	// Reply to that message; the reply must inherit the root id.
	replyResponse := postMessage(`{"text":"agreed","parent_id":"` + message.ID + `"}`)
	if replyResponse.Code != http.StatusCreated {
		t.Fatalf("reply status = %d, body = %q", replyResponse.Code, replyResponse.Body.String())
	}
	var reply store.ChannelMessage
	if err := json.NewDecoder(replyResponse.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.ParentID != message.ID || reply.RootID != message.ID {
		t.Fatalf("reply thread identity = %#v", reply)
	}

	// Idempotency: resending the same key must not create a second message.
	firstKeyed := postMessage(`{"text":"keyed message","idempotency_key":"msg-key-1"}`)
	secondKeyed := postMessage(`{"text":"keyed message","idempotency_key":"msg-key-1"}`)
	var firstKeyedMessage store.ChannelMessage
	if err := json.NewDecoder(firstKeyed.Body).Decode(&firstKeyedMessage); err != nil {
		t.Fatal(err)
	}
	var secondKeyedMessage store.ChannelMessage
	if err := json.NewDecoder(secondKeyed.Body).Decode(&secondKeyedMessage); err != nil {
		t.Fatal(err)
	}
	if firstKeyedMessage.ID != secondKeyedMessage.ID {
		t.Fatalf("idempotency produced duplicates: %s != %s", firstKeyedMessage.ID, secondKeyedMessage.ID)
	}

	// The merged timeline must interleave the notification card and messages.
	timeline := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID+"/timeline", nil)
	timeline.AddCookie(cookie)
	timelineRecorder := httptest.NewRecorder()
	handler.ServeHTTP(timelineRecorder, timeline)
	if timelineRecorder.Code != http.StatusOK {
		t.Fatalf("timeline status = %d, body = %q", timelineRecorder.Code, timelineRecorder.Body.String())
	}
	var timelineBody struct {
		Items      []store.TimelineItem `json:"items"`
		NextCursor string               `json:"next_cursor"`
	}
	if err := json.NewDecoder(timelineRecorder.Body).Decode(&timelineBody); err != nil {
		t.Fatal(err)
	}
	var messageCount, notificationCount int
	for _, item := range timelineBody.Items {
		switch item.Kind {
		case "message":
			messageCount++
		case "notification":
			notificationCount++
		}
	}
	// hello composer, agreed reply, keyed message, incident card = 3 messages + 1 card.
	if messageCount != 3 || notificationCount != 1 {
		t.Fatalf("timeline composition: %d messages, %d notifications in %#v", messageCount, notificationCount, timelineBody.Items)
	}

	// Unread-only excludes the signed-in user's own messages while retaining
	// unread notifications from the channel.
	unread := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID+"/timeline?unread=1", nil)
	unread.AddCookie(cookie)
	unreadRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unreadRecorder, unread)
	var unreadBody struct {
		Items []store.TimelineItem `json:"items"`
	}
	if err := json.NewDecoder(unreadRecorder.Body).Decode(&unreadBody); err != nil {
		t.Fatal(err)
	}
	if unreadRecorder.Code != http.StatusOK || len(unreadBody.Items) != 1 || unreadBody.Items[0].Kind != "notification" {
		t.Fatalf("unread timeline status=%d items=%#v", unreadRecorder.Code, unreadBody.Items)
	}

	// Newest messages appear before the card: both keyed + keyed(dup) collapse to
	// one, leaving replies/messages above the notification.
	foundReplyBacklink := false
	for _, item := range timelineBody.Items {
		if item.Kind == "message" && item.Message.RootID == message.ID && item.Message.ID != message.ID {
			foundReplyBacklink = true
		}
	}
	if !foundReplyBacklink {
		t.Fatalf("reply did not appear grouped in timeline: %#v", timelineBody.Items)
	}

	// Search is shared across notification cards and human messages on the
	// selected-channel surface.
	search := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID+"/timeline?q=composer", nil)
	search.AddCookie(cookie)
	searchRecorder := httptest.NewRecorder()
	handler.ServeHTTP(searchRecorder, search)
	var searchBody struct {
		Items []store.TimelineItem `json:"items"`
	}
	if err := json.NewDecoder(searchRecorder.Body).Decode(&searchBody); err != nil {
		t.Fatal(err)
	}
	if searchRecorder.Code != http.StatusOK || len(searchBody.Items) != 1 || searchBody.Items[0].Kind != "message" || searchBody.Items[0].Message.ID != message.ID {
		t.Fatalf("timeline search status=%d items=%#v", searchRecorder.Code, searchBody.Items)
	}
	deepLink := httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+reply.ID, nil)
	deepLink.AddCookie(cookie)
	deepLinkRecorder := httptest.NewRecorder()
	handler.ServeHTTP(deepLinkRecorder, deepLink)
	var deepLinked store.ChannelMessage
	if err := json.NewDecoder(deepLinkRecorder.Body).Decode(&deepLinked); err != nil {
		t.Fatal(err)
	}
	if deepLinkRecorder.Code != http.StatusOK || deepLinked.ID != reply.ID || deepLinked.ChannelName != channel.Name {
		t.Fatalf("message deep link status=%d message=%#v", deepLinkRecorder.Code, deepLinked)
	}

	// The thread endpoint must return the reply correlated to the parent.
	thread := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID+"/threads/"+message.ID, nil)
	thread.AddCookie(cookie)
	threadRecorder := httptest.NewRecorder()
	handler.ServeHTTP(threadRecorder, thread)
	var threadBody struct {
		Replies []store.ChannelMessage `json:"replies"`
	}
	if err := json.NewDecoder(threadRecorder.Body).Decode(&threadBody); err != nil {
		t.Fatal(err)
	}
	if len(threadBody.Replies) != 1 || threadBody.Replies[0].ID != reply.ID {
		t.Fatalf("thread replies = %#v", threadBody.Replies)
	}
}

func TestChannelTimelineDismissedFilter(t *testing.T) {
	handler, cookie, channel, token := timelineServer(t)
	publish := httptest.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewBufferString("dismiss me"))
	publish.Header.Set("Content-Type", "text/plain")
	publish.Header.Set("Authorization", "Bearer "+token)
	publishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(publishRecorder, publish)
	var notification store.Notification
	if err := json.NewDecoder(publishRecorder.Body).Decode(&notification); err != nil {
		t.Fatal(err)
	}

	dismiss := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+notification.ID+"/inbox", bytes.NewBufferString(`{"action":"dismiss"}`))
	dismiss.Header.Set("Content-Type", "application/json")
	dismiss.Header.Set("Origin", "http://example.com")
	dismiss.Host = "example.com"
	dismiss.AddCookie(cookie)
	dismissRecorder := httptest.NewRecorder()
	handler.ServeHTTP(dismissRecorder, dismiss)
	if dismissRecorder.Code != http.StatusNoContent {
		t.Fatalf("dismiss status=%d body=%q", dismissRecorder.Code, dismissRecorder.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channel.ID+"/timeline?state=dismissed", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var body struct {
		Items []store.TimelineItem `json:"items"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(body.Items) != 1 || body.Items[0].Kind != "notification" || body.Items[0].Notification.ID != notification.ID {
		t.Fatalf("dismissed timeline status=%d items=%#v", recorder.Code, body.Items)
	}
}

func TestChannelMessagePrivateAuthorization(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "private.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure private password"); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	login := func(username, password string) *http.Cookie {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"`+username+`","password":"`+password+`"}`))
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("login %s status = %d, body = %q", username, recorder.Code, recorder.Body.String())
		}
		return recorder.Result().Cookies()[0]
	}
	adminCookie := login("admin", "secure private password")

	create := httptest.NewRequest(http.MethodPost, "/api/v1/channels", bytes.NewBufferString(`{"name":"classified","visibility":"private"}`))
	create.Header.Set("Origin", "http://example.com")
	create.Host = "example.com"
	create.AddCookie(adminCookie)
	createRecorder := httptest.NewRecorder()
	handler.ServeHTTP(createRecorder, create)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %q", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		Channel store.ChannelSummary `json:"channel"`
	}
	if err := json.NewDecoder(createRecorder.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	// Create a non-member user.
	createUser := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{"username":"bob","password":"secure bob password"}`))
	createUser.Header.Set("Origin", "http://example.com")
	createUser.Host = "example.com"
	createUser.AddCookie(adminCookie)
	userRecorder := httptest.NewRecorder()
	handler.ServeHTTP(userRecorder, createUser)
	if userRecorder.Code != http.StatusCreated {
		t.Fatalf("create user status = %d, body = %q", userRecorder.Code, userRecorder.Body.String())
	}
	bobCookie := login("bob", "secure bob password")

	// Bob cannot post to or read the private channel.
	postMessage := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+created.Channel.ID+"/messages", bytes.NewBufferString(`{"text":"intrusion"}`))
	postMessage.Header.Set("Content-Type", "application/json")
	postMessage.AddCookie(bobCookie)
	postRecorder := httptest.NewRecorder()
	handler.ServeHTTP(postRecorder, postMessage)
	if postRecorder.Code != http.StatusForbidden {
		t.Fatalf("non-member post status = %d, body = %q", postRecorder.Code, postRecorder.Body.String())
	}
	listTimeline := httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+created.Channel.ID+"/timeline", nil)
	listTimeline.AddCookie(bobCookie)
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listTimeline)
	if listRecorder.Code != http.StatusForbidden {
		t.Fatalf("non-member list status = %d, body = %q", listRecorder.Code, listRecorder.Body.String())
	}
	adminMessage := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+created.Channel.ID+"/messages", bytes.NewBufferString(`{"text":"private history"}`))
	adminMessage.Header.Set("Content-Type", "application/json")
	adminMessage.AddCookie(adminCookie)
	adminMessageRecorder := httptest.NewRecorder()
	handler.ServeHTTP(adminMessageRecorder, adminMessage)
	var privateMessage store.ChannelMessage
	if err := json.NewDecoder(adminMessageRecorder.Body).Decode(&privateMessage); err != nil {
		t.Fatal(err)
	}
	privateDeepLink := httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+privateMessage.ID, nil)
	privateDeepLink.AddCookie(bobCookie)
	privateDeepLinkRecorder := httptest.NewRecorder()
	handler.ServeHTTP(privateDeepLinkRecorder, privateDeepLink)
	if privateDeepLinkRecorder.Code != http.StatusForbidden {
		t.Fatalf("private deep-link status = %d, want 403", privateDeepLinkRecorder.Code)
	}

	// Grant membership; Bob may now post and read.
	membership := httptest.NewRequest(http.MethodPut, "/api/v1/channels/"+created.Channel.ID+"/members/bob", bytes.NewBufferString(`{"role":"viewer"}`))
	membership.Header.Set("Origin", "http://example.com")
	membership.Host = "example.com"
	membership.AddCookie(adminCookie)
	membershipRecorder := httptest.NewRecorder()
	handler.ServeHTTP(membershipRecorder, membership)
	if membershipRecorder.Code != http.StatusNoContent {
		t.Fatalf("membership status = %d, body = %q", membershipRecorder.Code, membershipRecorder.Body.String())
	}
	allowed := httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+created.Channel.ID+"/messages", bytes.NewBufferString(`{"text":"member hello"}`))
	allowed.Header.Set("Content-Type", "application/json")
	allowed.AddCookie(bobCookie)
	allowedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(allowedRecorder, allowed)
	if allowedRecorder.Code != http.StatusCreated {
		t.Fatalf("member post status = %d, body = %q", allowedRecorder.Code, allowedRecorder.Body.String())
	}

	// A second server sharing the authoritative database must observe membership
	// removal immediately; no node-local authorization cache may extend access.
	secondHandler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := db.AuthenticateUser(context.Background(), "bob", "secure bob password")
	if err != nil {
		t.Fatal(err)
	}
	remove := httptest.NewRequest(http.MethodPut, "/api/v1/admin/users/"+bob.ID+"/memberships/"+created.Channel.ID, bytes.NewBufferString(`{"role":""}`))
	remove.Header.Set("Origin", "http://example.com")
	remove.Host = "example.com"
	remove.AddCookie(adminCookie)
	removeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(removeRecorder, remove)
	if removeRecorder.Code != http.StatusOK {
		t.Fatalf("remove membership status=%d body=%q", removeRecorder.Code, removeRecorder.Body.String())
	}
	for name, request := range map[string]*http.Request{
		"timeline":  httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+created.Channel.ID+"/timeline", nil),
		"deep-link": httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+privateMessage.ID, nil),
		"post":      httptest.NewRequest(http.MethodPost, "/api/v1/channels/"+created.Channel.ID+"/messages", bytes.NewBufferString(`{"text":"after removal"}`)),
	} {
		request.AddCookie(bobCookie)
		if name == "post" {
			request.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		secondHandler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s after cross-node membership removal status=%d body=%q", name, recorder.Code, recorder.Body.String())
		}
	}
	revoke := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/users/"+bob.ID+"/sessions", nil)
	revoke.Header.Set("Origin", "http://example.com")
	revoke.Host = "example.com"
	revoke.AddCookie(adminCookie)
	revokeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(revokeRecorder, revoke)
	if revokeRecorder.Code != http.StatusNoContent {
		t.Fatalf("revoke sessions status=%d body=%q", revokeRecorder.Code, revokeRecorder.Body.String())
	}
	session := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	session.AddCookie(bobCookie)
	sessionRecorder := httptest.NewRecorder()
	secondHandler.ServeHTTP(sessionRecorder, session)
	var sessionState struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(sessionRecorder.Body).Decode(&sessionState); err != nil {
		t.Fatal(err)
	}
	if sessionRecorder.Code != http.StatusOK || sessionState.Authenticated {
		t.Fatalf("session after cross-node revocation status=%d authenticated=%v", sessionRecorder.Code, sessionState.Authenticated)
	}
}
