package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	pwakit "go.michaelspost.com/pwa-kit"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

type pushRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f pushRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPushHTTPClientBlocksPrivateDestinationsAndRedirects(t *testing.T) {
	client := newPushHTTPClient()
	response, err := client.Post("https://127.0.0.1/push", "application/octet-stream", strings.NewReader("payload"))
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("private push destination error = %v", err)
	}

	requests := 0
	client.Transport = pushRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://127.0.0.1/internal"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})
	response, err = client.Post("https://push.example/device", "application/octet-stream", strings.NewReader("payload"))
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || requests != 1 {
		t.Fatalf("redirect error/requests = %v/%d", err, requests)
	}
}

func TestNotificationPushPayloadUsesStableTagAndLifecycle(t *testing.T) {
	notification := store.Notification{
		ID: "ntf_example", ChannelName: "prometheus", State: "firing",
		Text:        "fallback text",
		Attachments: json.RawMessage(`[{"title":"[FIRING:1] Disk full"}]`),
	}
	payload := notificationPushPayload(notification)
	if payload.Title != "Firing · Disk full · prometheus" || payload.Body != "fallback text" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Tag != "tintwire-ntf_example" || payload.URL != "/?channel=prometheus&notification=ntf_example" {
		t.Fatalf("payload identity = %#v", payload)
	}
	notification.State = "resolved"
	resolved := notificationPushPayload(notification)
	if resolved.Tag != payload.Tag || resolved.Title != "Resolved · Disk full · prometheus" {
		t.Fatalf("resolved payload = %#v", resolved)
	}
}

func TestNotificationPushPayloadDoesNotRepeatApplicationName(t *testing.T) {
	notification := store.Notification{
		ID: "ntf_mastodon", ChannelName: "mastodon", Text: "New post",
	}
	payload := notificationPushPayload(notification)
	if payload.Title != "mastodon" || payload.Body != "New post" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestPushSummaryRemovesRedundantAlertmanagerLifecyclePrefix(t *testing.T) {
	for input, want := range map[string]string{
		"[FIRING:12] Disk full": "Disk full",
		"[RESOLVED] Disk full":  "Disk full",
		"[warning] Disk full":   "[warning] Disk full",
	} {
		if got := withoutLifecyclePrefix(input); got != want {
			t.Errorf("withoutLifecyclePrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPushPresentationUsesNativeCardTitleAndSummary(t *testing.T) {
	notification := store.Notification{
		ID: "ntf_native", ChannelName: "alerts", Text: "web01 needs attention",
		Card: json.RawMessage(`{
			"version": 1,
			"title": "Disk almost full",
			"summary": "web01 needs attention",
			"source": "monitor"
		}`),
	}
	payload := notificationPushPayload(notification)
	if payload.Title != "Disk almost full · alerts" || payload.Body != "web01 needs attention" {
		t.Fatalf("payload = %#v, want separate native card title and summary", payload)
	}
}

func TestPushPresentationFallsBackToNativeCardContent(t *testing.T) {
	notification := store.Notification{
		ID: "ntf_native_fields", ChannelName: "logw",
		Card: json.RawMessage(`{
			"version": 1,
			"title": "Disk almost full",
			"fields": [{"label": "Host", "value": "web01"}]
		}`),
	}
	payload := notificationPushPayload(notification)
	if payload.Title != "Disk almost full · logw" || payload.Body != "Host: web01" {
		t.Fatalf("payload = %#v, want native card field as body fallback", payload)
	}
}

func TestPushPresentationUsesAttachmentTitleAndText(t *testing.T) {
	notification := store.Notification{
		ID: "ntf_attachment", ChannelName: "logs", Text: "top-level fallback",
		Attachments: json.RawMessage(`[{"title":"System alerts alert","text":"Three matching lines from auth.log"}]`),
	}
	payload := notificationPushPayload(notification)
	if payload.Title != "System alerts alert · logs" || payload.Body != "Three matching lines from auth.log" {
		t.Fatalf("payload = %#v, want separate attachment title and text", payload)
	}
}

func TestPushPresentationDoesNotRepeatIdenticalSubjectAndBody(t *testing.T) {
	subject, body := normalizePushSubjectAndBody("Disk full", "Disk full")
	if subject != "Disk full" || body != "" {
		t.Fatalf("normalizePushSubjectAndBody() = %q, %q", subject, body)
	}
}

func TestPushPresentationIsWhitespaceNormalizedAndBounded(t *testing.T) {
	notification := store.Notification{Text: strings.Repeat("word\n", 100)}
	_, summary := pushPresentation(notification)
	if strings.Contains(summary, "\n") || len([]rune(summary)) > 180 || !strings.HasSuffix(summary, "…") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestDeclarativePayloadPreservesOldWorkerAndNativePresentation(t *testing.T) {
	origin, _ := url.Parse("https://app.example.com")
	p := &pushService{origin: origin}
	for _, state := range []string{"received", "firing", "resolved", "message", "command", "digest", "test"} {
		payload := pushPayload{Title: "Backup", Body: "Complete", URL: "/?notification=123", Tag: "tintwire-123", State: state, Timestamp: 1234}
		raw, err := p.marshalPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			pushPayload
			pwakit.DeclarativePush
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.pushPayload != payload {
			t.Fatal("legacy payload changed")
		}
		n := decoded.Notification
		if decoded.WebPush != 8030 || n.Title != payload.Title || n.Body != payload.Body || n.Tag != payload.Tag || n.Navigate != "https://app.example.com/?notification=123" || n.Timestamp != 1234 || n.RequireInteraction != (state == "firing") || !n.Renotify {
			t.Fatalf("declarative payload mismatch: %s", raw)
		}
	}
	for _, target := range []string{"https://evil.example/", "//evil.example/", "javascript:alert(1)"} {
		if _, err := p.marshalPayload(pushPayload{Title: "Test", URL: target, Tag: "test"}); err == nil {
			t.Fatalf("accepted cross-origin navigation %q", target)
		}
	}
	raw, err := (&pushService{}).marshalPayload(pushPayload{Title: "Local", URL: "/"})
	if err != nil || strings.Contains(string(raw), "web_push") {
		t.Fatalf("unconfigured development origin: %s %v", raw, err)
	}
}

func TestDevicePushIsOwnedBoundedAndNeverClaimsReceipt(t *testing.T) {
	s, _, user, _, _ := workflowServer(t)
	ctx := context.Background()
	p, err := newPushService(s.store, "mailto:tests@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	s.push = p
	s.limiter = newToolLimiter()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub := store.PushSubscription{UserID: user.ID, Endpoint: "https://push.example.com/one", P256DH: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))}
	for _, endpoint := range []string{sub.Endpoint, "https://push.example.com/two"} {
		copy := sub
		copy.Endpoint = endpoint
		if err := s.store.SavePushSubscription(ctx, copy); err != nil {
			t.Fatal(err)
		}
	}
	requests := 0
	providerStatus := 201
	p.client = &http.Client{Transport: pushRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.String() != sub.Endpoint || r.Header.Get("TTL") != "60" || r.Header.Get("Urgency") != "high" {
			t.Errorf("wrong test destination/options")
		}
		return &http.Response{StatusCode: providerStatus, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("secret provider detail")), Request: r}, nil
	})}
	request := func(user store.User, body, origin string) *httptest.ResponseRecorder {
		return workflowRequest(http.HandlerFunc(s.testPush), user, "POST", "/api/v1/push/test", body, origin)
	}
	body := `{"endpoint":"` + sub.Endpoint + `"}`
	other, err := s.store.CreateUser(ctx, "other", "another long password", false)
	if err != nil {
		t.Fatal(err)
	}
	if r := request(other, body, "http://example.com"); r.Code != 404 {
		t.Fatalf("other user's endpoint: %d", r.Code)
	}
	if r := request(user, body, "https://evil.example"); r.Code != 403 {
		t.Fatalf("cross origin: %d", r.Code)
	}
	if r := request(user, `{"endpoint":"https://127.0.0.1/arbitrary"}`, "http://example.com"); r.Code != 404 {
		t.Fatalf("arbitrary endpoint: %d", r.Code)
	}
	if r := request(user, body+` {}`, "http://example.com"); r.Code != 400 {
		t.Fatalf("trailing data: %d", r.Code)
	}
	if requests != 0 {
		t.Fatal("unauthorized request sent a push")
	}
	unauth := httptest.NewRecorder()
	s.requireReader(s.testPush)(unauth, httptest.NewRequest("POST", "http://example.com/api/v1/push/test", strings.NewReader(body)))
	if unauth.Code != 401 {
		t.Fatalf("unauthenticated: %d", unauth.Code)
	}
	r := request(user, body, "http://example.com")
	if r.Code != 200 || strings.TrimSpace(r.Body.String()) != `{"outcome":"provider_accepted"}` || requests != 1 {
		t.Fatalf("test: %d %s requests=%d", r.Code, r.Body, requests)
	}
	providerStatus = 503
	r = request(user, body, "http://example.com")
	if r.Code != 502 || !strings.Contains(r.Body.String(), "provider_rejected") || strings.Contains(r.Body.String(), "secret") || requests != 2 {
		t.Fatalf("rejection: %d %s requests=%d", r.Code, r.Body, requests)
	}
	providerStatus = 410
	r = request(user, body, "http://example.com")
	if r.Code != 410 {
		t.Fatalf("expired: %d %s", r.Code, r.Body)
	}
	if _, err := s.store.UserPushSubscription(ctx, user.ID, sub.Endpoint); !errors.Is(err, store.ErrInvalidCredentials) {
		t.Fatalf("expired subscription retained: %v", err)
	}
	if _, err := s.store.UserPushSubscription(ctx, user.ID, "https://push.example.com/two"); err != nil {
		t.Fatalf("other device removed: %v", err)
	}
	if err := s.store.SavePushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	providerStatus = 201
	for range 2 {
		if r := request(user, body, "http://example.com"); r.Code != 200 {
			t.Fatalf("allowed test: %d", r.Code)
		}
	}
	if r := request(user, body, "http://example.com"); r.Code != 429 || r.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", r.Code)
	}
	if requests != 5 {
		t.Fatalf("unexpected sends: %d", requests)
	}
	notifications, err := s.store.ListNotifications(ctx, 10)
	if err != nil || len(notifications) != 0 {
		t.Fatal("test created an inbox notification", err)
	}
}
