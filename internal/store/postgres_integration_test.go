package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPostgresCoreLifecycle(t *testing.T) {
	dsn := os.Getenv("TINTWIRE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TINTWIRE_TEST_POSTGRES_DSN is not set")
	}
	data, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	ctx := context.Background()
	if err := data.BootstrapWebhook(ctx, "postgres-test-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	if err := data.BootstrapUser(ctx, "admin", "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	user, err := data.AuthenticateUser(ctx, "ADMIN", "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	created, existing, err := data.ImportSlashCommands(ctx, []SlashCommand{{
		Team: "postgres-test", Trigger: "status", DisplayName: "Status", Method: "POST",
		URL: "https://example.invalid/slash", TokenCipher: []byte("ciphertext"), TokenHash: []byte("hash"),
		AllowPrivate: false, Autocomplete: true, AutocompleteHint: "[status]",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 || existing != 0 {
		t.Fatalf("slash command import created=%d existing=%d", created, existing)
	}
	notification, err := data.CreateFromWebhook(ctx, "postgres-test-hook", IncomingNotification{
		Text: "database smoke test", State: "firing", RawPayload: json.RawMessage(`{"ok":true}`),
		Card: json.RawMessage(`{"version":1,"severity":"critical"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := data.QueryNotifications(ctx, NotificationQuery{Limit: 20, ID: notification.ID, Search: "SMOKE", Severity: "critical", UserID: user.ID, UserAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != notification.ID {
		t.Fatalf("notifications = %#v", listed)
	}
	if err := data.SetNotificationInboxState(ctx, user.ID, notification.ID, InboxMarkRead, time.Now()); err != nil {
		t.Fatal(err)
	}
	listed, err = data.QueryNotifications(ctx, NotificationQuery{Limit: 1, ID: notification.ID, UserID: user.ID, UserAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Unread {
		t.Fatalf("marked notification = %#v", listed)
	}
	channels, err := data.ListChannels(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 {
		t.Fatalf("channels = %#v", channels)
	}
	reader, err := data.CreateUser(ctx, "postgres-push-reader", "a third sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SavePushSubscription(ctx, PushSubscription{UserID: reader.ID, Endpoint: "https://push.example/postgres-reader", P256DH: "reader-key", Auth: "reader-auth"}); err != nil {
		t.Fatal(err)
	}
	message, err := data.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channels[0].ID, Text: "postgres message push"})
	if err != nil {
		t.Fatal(err)
	}
	messageSubscriptions, err := data.ListPushSubscriptionsForChannelMessage(ctx, message.ID)
	if err != nil || len(messageSubscriptions) != 1 || messageSubscriptions[0].UserID != reader.ID {
		t.Fatalf("message push subscriptions = %#v, error = %v", messageSubscriptions, err)
	}
	webhook, _, err := data.CreateWebhook(ctx, channels[0].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	webhooks, err := data.ListWebhooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundWebhook := false
	for _, candidate := range webhooks {
		if candidate.ID == webhook.ID && !candidate.ChannelLocked {
			foundWebhook = true
		}
	}
	if !foundWebhook {
		t.Fatalf("managed webhook missing from %#v", webhooks)
	}
	managed, err := data.CreateUser(ctx, "postgres-managed-user", "a different long password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SetManagedUserAdmin(ctx, user.ID, managed.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := data.SetManagedUserDisabled(ctx, user.ID, managed.ID, true); err != nil {
		t.Fatal(err)
	}
	users, err := data.ListManagedUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range users {
		if candidate.ID == managed.ID && candidate.IsAdmin && candidate.DisabledAt != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("managed user missing from %#v", users)
	}
}
