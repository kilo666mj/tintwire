package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

func workflowServer(t *testing.T) (*Server, http.Handler, store.User, string, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(context.Background(), "owner", "workflow password long", true)
	if err != nil {
		t.Fatal(err)
	}
	channel, token, err := db.CreateChannel(context.Background(), store.CreateChannelInput{Name: "tests", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: db, authRequired: true, subscribers: make(map[chan liveUpdate]struct{})}
	mux := http.NewServeMux()
	s.workflowRoutes(mux)
	return s, mux, user, channel.ID, token
}

func workflowRequest(handler http.Handler, user store.User, method, path, body, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	// Exercise normal reader middleware with an explicit authenticated principal.
	req = req.WithContext(context.WithValue(req.Context(), userContextKey{}, user))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestWorkflowAuthorizationPreviewAndPublication(t *testing.T) {
	s, _, user, channelID, _ := workflowServer(t)
	// Handlers are tested with the user middleware boundary explicitly supplied.
	route := func(fn http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
		return workflowRequest(fn, user, method, path, body, "http://example.com")
	}
	invalid := route(s.playground, "POST", "/api/v1/playground", `{"format":"native","payload":{"version":1}}`)
	if invalid.Code != 400 {
		t.Fatalf("invalid card: %d %s", invalid.Code, invalid.Body)
	}
	payload := `{"format":"native","payload":{"version":1,"title":"Build","source":"deploy","severity":"success"},"channel_id":"` + channelID + `"}`
	preview := route(s.playground, "POST", "/api/v1/playground", payload)
	if preview.Code != 200 {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body)
	}
	values, err := s.store.ListNotifications(context.Background(), 100)
	if err != nil || len(values) != 0 {
		t.Fatal("preview published a notification", err)
	}
	payload = strings.TrimSuffix(payload, "}") + `,"publish":true}`
	published := route(s.playground, "POST", "/api/v1/playground", payload)
	if published.Code != 200 {
		t.Fatalf("publish: %d %s", published.Code, published.Body)
	}
	values, err = s.store.ListNotifications(context.Background(), 100)
	if err != nil || len(values) != 1 {
		t.Fatalf("published values: %+v %v", values, err)
	}
	nonadmin := user
	nonadmin.IsAdmin = false
	denied := workflowRequest(http.HandlerFunc(s.playground), nonadmin, "POST", "/api/v1/playground", payload, "http://example.com")
	if denied.Code != 403 {
		t.Fatalf("nonadmin playground: %d", denied.Code)
	}
	cross := workflowRequest(http.HandlerFunc(s.playground), user, "POST", "/api/v1/playground", payload, "https://attacker.example")
	if cross.Code != 403 {
		t.Fatalf("cross-origin publish: %d", cross.Code)
	}
}

func TestDesktopFeedIgnoresInboxFiltersAndRespectsPolicy(t *testing.T) {
	s, _, user, channelID, token := workflowServer(t)
	ctx := context.Background()
	initial := workflowRequest(http.HandlerFunc(s.desktopAlerts), user, "GET", "/api/v1/desktop/alerts", "", "")
	var first struct {
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(initial.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	n, err := s.store.CreateFromWebhook(ctx, token, store.IncomingNotification{Text: "new event", RawPayload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetNotificationInboxState(ctx, user.ID, n.ID, store.InboxMarkRead, time.Now()); err != nil {
		t.Fatal(err)
	}
	page := workflowRequest(http.HandlerFunc(s.desktopAlerts), user, "GET", "/api/v1/desktop/alerts?after="+first.Cursor+"&channel=unrelated&q=nonmatching&unread=1", "", "")
	var response struct {
		Alerts []struct {
			ID string `json:"id"`
		} `json:"alerts"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &response); err != nil {
		t.Fatalf("page: %s %v", page.Body, err)
	}
	if len(response.Alerts) != 1 || response.Alerts[0].ID != n.ID {
		t.Fatalf("filtered desktop arrivals: %s", page.Body)
	}
	if err := s.store.SetChannelNotificationPreference(ctx, user, channelID, "muted"); err != nil {
		t.Fatal(err)
	}
	page = workflowRequest(http.HandlerFunc(s.desktopAlerts), user, "GET", "/api/v1/desktop/alerts?after="+first.Cursor, "", "")
	if err := json.Unmarshal(page.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Alerts) != 0 {
		t.Fatalf("muted desktop arrival: %s", page.Body)
	}
	outsider, err := s.store.CreateUser(ctx, "outsider", "outsider password long", false)
	if err != nil {
		t.Fatal(err)
	}
	page = workflowRequest(http.HandlerFunc(s.desktopAlerts), outsider, "GET", "/api/v1/desktop/alerts?after="+first.Cursor, "", "")
	if err := json.Unmarshal(page.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Alerts) != 0 {
		t.Fatalf("private arrival leaked: %s", page.Body)
	}
}

func TestSnoozeWorkerRestoresUnreadAndCreatesDesktopReminder(t *testing.T) {
	s, _, user, _, token := workflowServer(t)
	ctx := context.Background()
	n, err := s.store.CreateFromWebhook(ctx, token, store.IncomingNotification{Text: "remember", RawPayload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetNotificationInboxState(ctx, user.ID, n.ID, store.InboxMarkRead, time.Now()); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Second)
	if err := s.store.Snooze(ctx, user, n.ID, until); err != nil {
		t.Fatal(err)
	}
	// Run with a supplied clock; no sleeps are required for scheduling tests.
	s.workflowTick(ctx, until.Add(time.Second))
	jobs, err := s.store.UserSchedules(ctx, user.ID)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("remaining jobs: %+v %v", jobs, err)
	}
	values, err := s.store.QueryNotifications(ctx, store.NotificationQuery{ID: n.ID, UserID: user.ID, UserAdmin: true})
	if err != nil || len(values) != 1 || !values[0].Unread {
		t.Fatalf("reminder unread: %+v %v", values, err)
	}
	arrivals, err := s.store.DesktopArrivals(ctx, user, until.UnixMilli(), "!")
	if err != nil || len(arrivals) != 1 || arrivals[0].Kind != "attention" {
		t.Fatalf("desktop reminder: %+v %v", arrivals, err)
	}
}

func TestPushRetryHistoryAndPrivacy(t *testing.T) {
	s, _, user, _, token := workflowServer(t)
	ctx := context.Background()
	n, err := s.store.CreateFromWebhook(ctx, token, store.IncomingNotification{Text: "retry", RawPayload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := newPushService(s.store, "mailto:tests@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub := store.PushSubscription{UserID: user.ID, Endpoint: "https://push.example.com/private-endpoint-token", P256DH: base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))}
	if err := s.store.SavePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	p.client = &http.Client{Transport: pushRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		status := 503
		if attempts == 2 {
			status = 201
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("provider secret must not be retained")), Request: r}, nil
	})}
	p.deliverToUser(ctx, n, user)
	events, err := s.store.DeliveryEvents(ctx, n.ID, user)
	if err != nil || len(events) != 2 || attempts != 2 {
		t.Fatalf("retry history: %+v %v attempts=%d", events, err, attempts)
	}
	if events[0].Outcome != "provider_accepted" || events[0].Attempt != 2 || events[1].Outcome != "provider_rejected" {
		t.Fatalf("outcomes: %+v", events)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-endpoint-token") || strings.Contains(string(raw), "provider secret") {
		t.Fatal("delivery history leaked a secret")
	}
	attempts = 0
	p.client.Transport = pushRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: 410, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	p.deliverToUser(ctx, n, user)
	if attempts != 1 {
		t.Fatalf("expired subscription retried %d times", attempts)
	}
	subscriptions, err := s.store.UserSubscriptions(ctx, user.ID)
	if err != nil || len(subscriptions) != 0 {
		t.Fatalf("expired subscription retained: %+v %v", subscriptions, err)
	}
}
