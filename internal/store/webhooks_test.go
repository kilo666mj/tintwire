package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestManagedWebhookLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "webhooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	channel, _, err := db.CreateChannel(ctx, CreateChannelInput{Name: "alerts", DisplayName: "Alerts", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	webhook, token, err := db.CreateWebhook(ctx, channel.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || webhook.Channel != "alerts" || webhook.ChannelLocked {
		t.Fatalf("created webhook = %#v, token present = %t", webhook, token != "")
	}
	listed, err := db.ListWebhooks(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed webhooks = %#v, err = %v", listed, err)
	}
	for _, item := range listed {
		if item.Channel == "alerts" && item.ChannelLocked {
			t.Fatalf("webhooks should allow channel overrides by default: %#v", item)
		}
	}
	managed := func() Webhook {
		for _, item := range listed {
			if item.ID == webhook.ID {
				return item
			}
		}
		return Webhook{}
	}()
	if managed.ID == "" || managed.RevokedAt != nil || managed.ChannelLocked {
		t.Fatalf("managed webhook missing from %#v", listed)
	}
	if err := db.SetWebhookChannelLocked(ctx, webhook.ID, true); err != nil {
		t.Fatal(err)
	}
	listed, err = db.ListWebhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.ID == webhook.ID && !item.ChannelLocked {
			t.Fatalf("webhook was not locked: %#v", item)
		}
	}
	if err := db.SetWebhookChannelLocked(ctx, webhook.ID, false); err != nil {
		t.Fatal(err)
	}
	additional, additionalToken, err := db.DuplicateWebhook(ctx, webhook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if additional.ID == webhook.ID || additionalToken == "" || additional.ChannelID != channel.ID || additional.ChannelLocked {
		t.Fatalf("additional webhook = %#v, token present = %t", additional, additionalToken != "")
	}
	if _, err := db.CreateFromWebhook(ctx, token, IncomingNotification{Text: "existing URL", RawPayload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("publish through existing URL: %v", err)
	}
	webhook, token = additional, additionalToken
	other, _, err := db.CreateChannel(ctx, CreateChannelInput{Name: "other", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	firing, err := db.CreateFromWebhook(ctx, token, IncomingNotification{Text: "disk full", State: "firing", ExternalKey: "alert-1", RawPayload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := db.CreateFromWebhook(ctx, token, IncomingNotification{Channel: other.Name, Text: "disk recovered", State: "resolved", ExternalKey: "alert-1", RawPayload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != firing.ID || resolved.ChannelID != other.ID || resolved.ChannelName != other.Name {
		t.Fatalf("rerouted lifecycle notification = %#v, original = %#v", resolved, firing)
	}
	notifications, err := db.ListNotifications(ctx, 10)
	if err != nil || len(notifications) == 0 || notifications[0].ID != firing.ID || notifications[0].ChannelID != other.ID {
		t.Fatalf("stored rerouted notification = %#v, err = %v", notifications, err)
	}
	if _, err := db.CreateFromWebhook(ctx, token, IncomingNotification{Text: "before revoke", RawPayload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeWebhook(ctx, webhook.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFromWebhook(ctx, token, IncomingNotification{Text: "after revoke", RawPayload: json.RawMessage(`{}`)}); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("publish after revoke error = %v", err)
	}
	listed, err = db.ListWebhooks(ctx)
	managed = Webhook{}
	for _, item := range listed {
		if item.ID == webhook.ID {
			managed = item
		}
	}
	if err != nil || managed.RevokedAt == nil {
		t.Fatalf("revoked webhooks = %#v, err = %v", listed, err)
	}
	if err := db.SetWebhookChannelLocked(ctx, webhook.ID, true); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("lock revoked webhook error = %v", err)
	}
}
