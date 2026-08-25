package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

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

func TestNormalizeVAPIDContact(t *testing.T) {
	tests := map[string]string{
		"admin@example.com":        "admin@example.com",
		"mailto:admin@example.com": "admin@example.com",
		"https://example.com/push": "https://example.com/push",
	}
	for input, want := range tests {
		got, ok := normalizeVAPIDContact(input)
		if !ok || got != want {
			t.Errorf("normalizeVAPIDContact(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	for _, input := range []string{"http://example.com", "admin", "mailto:invalid"} {
		if _, ok := normalizeVAPIDContact(input); ok {
			t.Errorf("normalizeVAPIDContact(%q) accepted invalid contact", input)
		}
	}
}
