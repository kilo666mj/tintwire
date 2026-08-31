package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMattermostApprovalRequested(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "approve action", raw: `[{"actions":[{"id":"approve","name":"Approve"}]}]`, want: true},
		{name: "reject action", raw: `[{"actions":[{"id":"deny","name":"Reject request"}]}]`, want: true},
		{name: "informational card", raw: `[{"title":"Decision recorded","text":"No action required"}]`},
		{name: "unrelated action", raw: `[{"actions":[{"id":"details","name":"View details"}]}]`},
		{name: "invalid payload", raw: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mattermostApprovalRequested(json.RawMessage(test.raw)); got != test.want {
				t.Fatalf("mattermostApprovalRequested() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestOpenMigratesLegacyNotification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL);
CREATE TABLE webhooks (id TEXT PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE, channel_id TEXT NOT NULL REFERENCES channels(id), created_at INTEGER NOT NULL);
CREATE TABLE notifications (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES channels(id),
    webhook_id TEXT NOT NULL REFERENCES webhooks(id),
    text TEXT NOT NULL,
    username TEXT NOT NULL,
    icon_url TEXT NOT NULL,
    attachments_json BLOB NOT NULL,
    raw_payload_json BLOB NOT NULL,
    created_at INTEGER NOT NULL
);
INSERT INTO channels VALUES ('channel', 'general', 1000);
INSERT INTO webhooks VALUES ('webhook', X'01', 'channel', 1000);
INSERT INTO notifications VALUES ('notification', 'channel', 'webhook', 'legacy', 'webhook', '', X'5B5D', X'7B7D', 1000);
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	notifications, err := store.ListNotifications(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 {
		t.Fatalf("notification count = %d, want 1", len(notifications))
	}
	got := notifications[0]
	if got.State != "received" || !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Fatalf("migrated notification = %#v", got)
	}
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 26 {
		t.Fatalf("schema version = %d, want 26", version)
	}
}

func TestMarkChannelReadLeavesOtherChannelsUnread(t *testing.T) {
	ctx := context.Background()
	data, err := Open(filepath.Join(t.TempDir(), "channel-read.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.BootstrapUser(ctx, "reader", "a secure test password"); err != nil {
		t.Fatal(err)
	}
	reader, err := data.AuthenticateUser(ctx, "reader", "a secure test password")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ token, channel string }{{"first-hook", "first"}, {"second-hook", "second"}} {
		if err := data.BootstrapWebhook(ctx, item.token, item.channel); err != nil {
			t.Fatal(err)
		}
		if _, err := data.CreateFromWebhook(ctx, item.token, IncomingNotification{Text: item.channel, RawPayload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	channels, err := data.ListChannels(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || channels[0].UnreadCount != 1 || channels[1].UnreadCount != 1 {
		t.Fatalf("initial channels = %#v", channels)
	}
	if err := data.MarkChannelRead(ctx, reader, channels[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	channels, err = data.ListChannels(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if channels[0].UnreadCount != 0 || channels[1].UnreadCount != 1 {
		t.Fatalf("channels after marking one read = %#v", channels)
	}
	if count, err := data.UnreadCount(ctx, reader); err != nil || count != 1 {
		t.Fatalf("unread count = %d, %v", count, err)
	}
	if err := data.SetChannelNotificationPreference(ctx, reader, channels[1].ID, "muted"); err != nil {
		t.Fatal(err)
	}
	if count, err := data.UnreadCount(ctx, reader); err != nil || count != 0 {
		t.Fatalf("unread count with remaining channel muted = %d, %v", count, err)
	}
	channels, err = data.ListChannels(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if channels[1].UnreadCount != 1 {
		t.Fatalf("muted channel unread count = %d, want channel-local count preserved", channels[1].UnreadCount)
	}
}

func TestUserAuthenticationAndSessions(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	ctx := context.Background()
	if err := data.BootstrapUser(ctx, "alice", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := data.BootstrapUser(ctx, "alice", "correct horse battery staple"); err != nil {
		t.Fatalf("idempotent bootstrap: %v", err)
	}
	if err := data.BootstrapUser(ctx, "alice", "different password"); err == nil {
		t.Fatal("changed bootstrap password unexpectedly succeeded")
	}
	if _, err := data.AuthenticateUser(ctx, "alice", "wrong password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v", err)
	}
	user, err := data.AuthenticateUser(ctx, "ALICE", "correct horse battery staple")
	if err != nil || user.ID == "" {
		t.Fatalf("user = %#v, error = %v", user, err)
	}
	token, expires, err := data.CreateSession(ctx, user.ID, time.Hour)
	if err != nil || token == "" || !expires.After(time.Now()) {
		t.Fatalf("session token = %q, expires = %v, error = %v", token, expires, err)
	}
	if authenticated, err := data.UserForSession(ctx, token); err != nil || authenticated.ID != user.ID {
		t.Fatalf("authenticated = %#v, error = %v", authenticated, err)
	}
	if err := data.DeleteSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := data.UserForSession(ctx, token); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("deleted session error = %v", err)
	}
	if err := data.SavePushSubscription(ctx, PushSubscription{UserID: user.ID, Endpoint: "https://push.example/authenticated", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	subscriptions, err := data.ListPushSubscriptions(ctx)
	if err != nil || len(subscriptions) != 1 || subscriptions[0].UserID != user.ID {
		t.Fatalf("subscriptions = %#v, error = %v", subscriptions, err)
	}
	if err := data.BootstrapUser(ctx, "other", "another secure password"); err != nil {
		t.Fatal(err)
	}
	other, err := data.AuthenticateUser(ctx, "other", "another secure password")
	if err != nil {
		t.Fatal(err)
	}
	if err := data.RemoveUserPushSubscription(ctx, other.ID, "https://push.example/authenticated"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("other user removal error = %v", err)
	}
	if err := data.RemoveUserPushSubscription(ctx, user.ID, "https://push.example/authenticated"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-one.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL);
CREATE TABLE webhooks (id TEXT PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE, channel_id TEXT NOT NULL REFERENCES channels(id), created_at INTEGER NOT NULL);
CREATE TABLE notifications (
    id TEXT PRIMARY KEY, channel_id TEXT NOT NULL, webhook_id TEXT NOT NULL,
    text TEXT NOT NULL, username TEXT NOT NULL, icon_url TEXT NOT NULL,
    attachments_json BLOB NOT NULL, raw_payload_json BLOB NOT NULL,
    created_at INTEGER NOT NULL, external_key TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'received', updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE notification_events (
    id TEXT PRIMARY KEY, notification_id TEXT NOT NULL, state TEXT NOT NULL,
    raw_payload_json BLOB NOT NULL, created_at INTEGER NOT NULL
);
PRAGMA user_version = 1;
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SavePushSubscription(context.Background(), PushSubscription{
		Endpoint: "https://push.example/device", P256DH: "key", Auth: "auth",
	}); err != nil {
		t.Fatalf("version-one push migration failed: %v", err)
	}
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 26 {
		t.Fatalf("schema version = %d, want 26", version)
	}
}

func TestOIDCLoginStateAndUserProvisioning(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "oidc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { data.Close() })
	ctx := context.Background()
	if err := data.CreateOIDCLoginState(ctx, "state", "verifier", "nonce", time.Minute); err != nil {
		t.Fatal(err)
	}
	state, err := data.OIDCLoginState(ctx, "state")
	if err != nil || state.Verifier != "verifier" || state.Nonce != "nonce" {
		t.Fatalf("state = %#v, error = %v", state, err)
	}
	if _, err := data.ConsumeOIDCLoginState(ctx, "state"); err != nil {
		t.Fatal(err)
	}
	if _, err := data.ConsumeOIDCLoginState(ctx, "state"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("second consume error = %v", err)
	}
	first, err := data.FindOrCreateOIDCUser(ctx, "subject-1", "Alice Example")
	if err != nil {
		t.Fatal(err)
	}
	second, err := data.FindOrCreateOIDCUser(ctx, "subject-1", "changed")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Username != "alice-example" {
		t.Fatalf("users = %#v and %#v", first, second)
	}
	if _, err := data.AuthenticateUser(ctx, first.Username, "not-an-oidc-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("OIDC-only password authentication error = %v", err)
	}
}

func TestPushSubscriptionsPersistAndUpdate(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first := PushSubscription{Endpoint: "https://push.example/one", P256DH: "key-one", Auth: "auth-one"}
	if err := store.SavePushSubscription(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.P256DH = "key-two"
	if err := store.SavePushSubscription(ctx, first); err != nil {
		t.Fatal(err)
	}
	subscriptions, err := store.ListPushSubscriptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 1 || subscriptions[0].P256DH != "key-two" || subscriptions[0].CreatedAt.IsZero() {
		t.Fatalf("subscriptions = %#v", subscriptions)
	}
	if err := store.RemoveUserPushSubscription(ctx, "", first.Endpoint); err != nil {
		t.Fatal(err)
	}
	subscriptions, err = store.ListPushSubscriptions(ctx)
	if err != nil || len(subscriptions) != 0 {
		t.Fatalf("subscriptions after removal = %#v, error = %v", subscriptions, err)
	}
}

func TestAuthenticatedUserCanClaimAnonymousPushSubscription(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "push-claim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	ctx := context.Background()
	user, err := data.CreateUser(ctx, "member", "secure member password", false)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "https://push.example/existing-phone"
	if err := data.SavePushSubscription(ctx, PushSubscription{Endpoint: endpoint, P256DH: "anonymous-key", Auth: "anonymous-auth"}); err != nil {
		t.Fatal(err)
	}
	if err := data.SavePushSubscription(ctx, PushSubscription{UserID: user.ID, Endpoint: endpoint, P256DH: "member-key", Auth: "member-auth"}); err != nil {
		t.Fatalf("claim anonymous subscription: %v", err)
	}
	subscriptions, err := data.ListPushSubscriptions(ctx)
	if err != nil || len(subscriptions) != 1 {
		t.Fatalf("subscriptions = %#v, error = %v", subscriptions, err)
	}
	if subscriptions[0].UserID != user.ID || subscriptions[0].P256DH != "member-key" {
		t.Fatalf("claimed subscription = %#v", subscriptions[0])
	}
	if err := data.SavePushSubscription(ctx, PushSubscription{Endpoint: endpoint, P256DH: "anonymous-again", Auth: "anonymous-again"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("anonymous takeover error = %v", err)
	}
	other, err := data.CreateUser(ctx, "other", "secure other password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SavePushSubscription(ctx, PushSubscription{UserID: other.ID, Endpoint: endpoint, P256DH: "wrong-key", Auth: "wrong-auth"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("different-device takeover error = %v", err)
	}
	if err := data.SavePushSubscription(ctx, PushSubscription{UserID: other.ID, Endpoint: endpoint, P256DH: "member-key", Auth: "member-auth"}); err != nil {
		t.Fatalf("same-device identity rebind: %v", err)
	}
	subscriptions, err = data.ListPushSubscriptions(ctx)
	if err != nil || len(subscriptions) != 1 || subscriptions[0].UserID != other.ID {
		t.Fatalf("rebound subscription = %#v, error = %v", subscriptions, err)
	}
}

func TestPushSubscriptionOwnershipAndNotificationVisibility(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "push-authorization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	ctx := context.Background()
	member, err := data.CreateUser(ctx, "member", "secure member password", false)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := data.CreateUser(ctx, "outsider", "secure outsider password", false)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := data.CreateUser(ctx, "admin", "secure admin password", true)
	if err != nil {
		t.Fatal(err)
	}
	channel, token, err := data.CreateChannel(ctx, CreateChannelInput{Name: "private", DisplayName: "Private", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SetChannelMember(ctx, channel.ID, member.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	notification, err := data.CreateFromWebhook(ctx, token, IncomingNotification{Text: "private message", RawPayload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, subscription := range []PushSubscription{
		{UserID: member.ID, Endpoint: "https://push.example/member", P256DH: "member-key", Auth: "member-auth"},
		{UserID: outsider.ID, Endpoint: "https://push.example/outsider", P256DH: "outsider-key", Auth: "outsider-auth"},
		{UserID: admin.ID, Endpoint: "https://push.example/admin", P256DH: "admin-key", Auth: "admin-auth"},
		{Endpoint: "https://push.example/anonymous", P256DH: "anonymous-key", Auth: "anonymous-auth"},
	} {
		if err := data.SavePushSubscription(ctx, subscription); err != nil {
			t.Fatal(err)
		}
	}
	if err := data.SavePushSubscription(ctx, PushSubscription{UserID: outsider.ID, Endpoint: "https://push.example/member", P256DH: "stolen", Auth: "stolen"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("cross-user endpoint takeover error = %v", err)
	}
	allowed, err := data.ListPushSubscriptionsForNotification(ctx, notification.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	allowedUsers := map[string]bool{}
	for _, subscription := range allowed {
		allowedUsers[subscription.UserID] = true
	}
	if len(allowed) != 2 || !allowedUsers[member.ID] || !allowedUsers[admin.ID] {
		t.Fatalf("authorized subscriptions = %#v", allowed)
	}
	if err := data.SetChannelNotificationPreference(ctx, member, channel.ID, "muted"); err != nil {
		t.Fatal(err)
	}
	muted, err := data.ListPushSubscriptionsForNotification(ctx, notification.ID, false)
	if err != nil || len(muted) != 1 || muted[0].UserID != admin.ID {
		t.Fatalf("muted subscriptions = %#v, error = %v", muted, err)
	}
	if err := data.SetChannelNotificationPreference(ctx, member, channel.ID, "critical"); err != nil {
		t.Fatal(err)
	}
	critical, err := data.CreateFromWebhook(ctx, token, IncomingNotification{Text: "critical", RawPayload: []byte(`{}`), Card: []byte(`{"version":1,"severity":"critical"}`)})
	if err != nil {
		t.Fatal(err)
	}
	criticalAllowed, err := data.ListPushSubscriptionsForNotification(ctx, critical.ID, false)
	if err != nil || len(criticalAllowed) != 2 {
		t.Fatalf("critical subscriptions = %#v, error = %v", criticalAllowed, err)
	}
	channels, err := data.ListChannels(ctx, member)
	if err != nil || len(channels) != 1 || channels[0].NotificationLevel != "critical" {
		t.Fatalf("channel notification level = %#v, error = %v", channels, err)
	}
	withAnonymous, err := data.ListPushSubscriptionsForNotification(ctx, notification.ID, true)
	withAnonymousUsers := map[string]bool{}
	for _, subscription := range withAnonymous {
		withAnonymousUsers[subscription.UserID] = true
	}
	if err != nil || len(withAnonymous) != 2 || !withAnonymousUsers[admin.ID] || !withAnonymousUsers[""] {
		t.Fatalf("unauthenticated-mode subscriptions = %#v, error = %v", withAnonymous, err)
	}
	if err := data.RemoveUserPushSubscription(ctx, outsider.ID, "https://push.example/member"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("cross-user endpoint deletion error = %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.SaveSettings(ctx, map[string]string{"example": "value"}); err != nil {
		t.Fatal(err)
	}
	value, ok, err := store.Setting(ctx, "example")
	if err != nil || !ok || value != "value" {
		t.Fatalf("Setting = %q, %v, %v", value, ok, err)
	}
	_, ok, err = store.Setting(ctx, "missing")
	if err != nil || ok {
		t.Fatalf("missing Setting ok = %v, error = %v", ok, err)
	}
}

func TestChannelSummariesIncludeFiringCounts(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "channel-counts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	ctx := context.Background()
	if err := data.BootstrapWebhook(ctx, "prometheus-token", "prometheus"); err != nil {
		t.Fatal(err)
	}
	if err := data.BootstrapWebhook(ctx, "general-token", "general"); err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateFromWebhook(ctx, "prometheus-token", IncomingNotification{Text: "disk full", State: "firing", RawPayload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateFromWebhook(ctx, "prometheus-token", IncomingNotification{Text: "service restored", State: "resolved", RawPayload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	channels, err := data.ListChannels(ctx, User{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ChannelSummary{}
	for _, channel := range channels {
		byName[channel.Name] = channel
	}
	if got := byName["prometheus"]; got.TotalCount != 2 || got.FiringCount != 1 {
		t.Fatalf("prometheus summary = %#v", got)
	}
	if got := byName["general"]; got.TotalCount != 0 || got.FiringCount != 0 {
		t.Fatalf("general summary = %#v", got)
	}
}
