package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSavedViewsAreUserScopedAndUpdatedByName(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "views.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	first, err := db.SaveSavedView(ctx, LocalInboxUser().ID, SavedView{Name: "News", Channels: []string{"mastodon", "bluesky"}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.SaveSavedView(ctx, LocalInboxUser().ID, SavedView{Name: "News", Channels: []string{"mastodon"}, Unread: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != first.ID {
		t.Fatalf("updated id = %q, want %q", updated.ID, first.ID)
	}
	views, err := db.ListSavedViews(ctx, LocalInboxUser().ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ID != first.ID || len(views[0].Channels) != 1 || !views[0].Unread {
		t.Fatalf("views = %#v", views)
	}
	if err := db.DeleteSavedView(ctx, LocalInboxUser().ID, first.ID); err != nil {
		t.Fatal(err)
	}
	views, err = db.ListSavedViews(ctx, LocalInboxUser().ID)
	if err != nil || len(views) != 0 {
		t.Fatalf("views after delete = %#v, err = %v", views, err)
	}
}

func TestNotificationQueryCombinesNamedChannels(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "combined.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for _, name := range []string{"mastodon", "bluesky", "operations"} {
		channel, _, err := db.CreateChannel(ctx, CreateChannelInput{Name: name, Visibility: "public"})
		if err != nil {
			t.Fatal(err)
		}
		_, token, err := db.CreateWebhook(ctx, channel.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateFromWebhook(ctx, token, IncomingNotification{Text: name, RawPayload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	notifications, err := db.QueryNotifications(ctx, NotificationQuery{Channels: []string{"mastodon", "bluesky"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 2 {
		t.Fatalf("combined notifications = %#v", notifications)
	}
	for _, notification := range notifications {
		if notification.ChannelName == "operations" {
			t.Fatalf("unselected channel returned: %#v", notification)
		}
	}
}
