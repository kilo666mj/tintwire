package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func timelineTestStore(t *testing.T) (*Store, context.Context, User) {
	t.Helper()
	data, err := Open(filepath.Join(t.TempDir(), "timeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := data.BootstrapUser(ctx, "alice", "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	user, err := data.AuthenticateUser(ctx, "alice", "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	return data, ctx, user
}

func TestChannelMessageCreateAndTimelineOrder(t *testing.T) {
	data, ctx, user := timelineTestStore(t)
	defer data.Close()

	if err := data.BootstrapWebhook(ctx, "hook", "general"); err != nil {
		t.Fatal(err)
	}
	channel, err := data.ChannelIDByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	// Interleave a notification card to verify merged ordering.
	if _, err := data.CreateFromWebhook(ctx, "hook", IncomingNotification{Text: "service restored", RawPayload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	message, err := data.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel, Text: "hello timeline"})
	if err != nil {
		t.Fatal(err)
	}
	if message.RootID != message.ID || message.ParentID != "" || message.Author != "alice" {
		t.Fatalf("top-level message = %#v", message)
	}
	time.Sleep(2 * time.Millisecond)
	reply, err := data.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel, Text: "a reply", ParentID: message.ID})
	if err != nil {
		t.Fatal(err)
	}
	if reply.RootID != message.ID || reply.ParentID != message.ID {
		t.Fatalf("reply thread identity = %#v", reply)
	}

	items, _, err := data.ListChannelTimeline(ctx, user, channel, "", 100, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 3 {
		t.Fatalf("timeline items = %#v", items)
	}
	// Newest first: reply, message, notification card.
	if items[0].Kind != "message" || items[0].Message == nil || items[0].Message.Text != "a reply" {
		t.Fatalf("first item = %#v", items[0])
	}
	if items[1].Kind != "message" || items[1].Message.Text != "hello timeline" {
		t.Fatalf("second item = %#v", items[1])
	}
	if items[2].Kind != "notification" || items[2].Notification.Text != "service restored" {
		t.Fatalf("third item = %#v", items[2])
	}
}

func TestChannelMessageIdempotency(t *testing.T) {
	data, ctx, user := timelineTestStore(t)
	defer data.Close()

	if err := data.BootstrapWebhook(ctx, "hook", "general"); err != nil {
		t.Fatal(err)
	}
	channel, err := data.ChannelIDByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	input := CreateMessageInput{ChannelID: channel, Text: "idempotent message", IdempotencyKey: "request-key-1"}
	first, err := data.CreateChannelMessage(ctx, user, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := data.CreateChannelMessage(ctx, user, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotency produced duplicates: %s != %s", first.ID, second.ID)
	}
	items, _, err := data.ListChannelTimeline(ctx, user, channel, "", 100, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range items {
		if item.Kind == "message" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate submission created %d messages, want 1", count)
	}
}

func TestChannelMessagePrivateAccess(t *testing.T) {
	data, ctx, _ := timelineTestStore(t)
	defer data.Close()

	channel, _, err := data.CreateChannel(ctx, CreateChannelInput{Name: "private", DisplayName: "Private", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := data.CreateUser(ctx, "bob", "another sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	// Bob is not a member: reading and writing must be rejected.
	if _, err := data.CreateChannelMessage(ctx, other, CreateMessageInput{ChannelID: channel.ID, Text: "intrusion"}); err != ErrForbidden {
		t.Fatalf("non-member create error = %v", err)
	}
	if _, _, err := data.ListChannelTimeline(ctx, other, channel.ID, "", 100, 0, ""); err != ErrForbidden {
		t.Fatalf("non-member list error = %v", err)
	}
	// Grant Bob membership; he may now post and read.
	if err := data.SetChannelMember(ctx, channel.ID, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	message, err := data.CreateChannelMessage(ctx, other, CreateMessageInput{ChannelID: channel.ID, Text: "member hello"})
	if err != nil {
		t.Fatal(err)
	}
	if message.ChannelID != channel.ID {
		t.Fatalf("member message = %#v", message)
	}
	items, _, err := data.ListChannelTimeline(ctx, other, channel.ID, "", 100, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Message == nil {
		t.Fatalf("member timeline = %#v", items)
	}
}

func TestChannelTimelinePagination(t *testing.T) {
	data, ctx, user := timelineTestStore(t)
	defer data.Close()

	if err := data.BootstrapWebhook(ctx, "hook", "general"); err != nil {
		t.Fatal(err)
	}
	channel, err := data.ChannelIDByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		if _, err := data.CreateFromWebhook(ctx, "hook", IncomingNotification{Text: fmt.Sprintf("card %d", index), RawPayload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 5; index++ {
		if _, err := data.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel, Text: fmt.Sprintf("message %d", index), IdempotencyKey: fmt.Sprintf("page-key-%d", index)}); err != nil {
			t.Fatal(err)
		}
	}

	const pageSize = 3
	seen := map[string]bool{}
	var beforeAt int64
	var beforeID string
	for page := 0; page < 10; page++ {
		items, hasMore, err := data.ListChannelTimeline(ctx, user, channel, "", pageSize, beforeAt, beforeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		if len(items) > pageSize {
			t.Fatalf("page %d returned %d items, want at most %d", page, len(items), pageSize)
		}
		for _, item := range items {
			key := item.Kind + ":" + item.ID
			if seen[key] {
				t.Fatalf("page %d returned duplicate item %s", page, key)
			}
			seen[key] = true
		}
		if !hasMore {
			break // last page
		}
		last := items[len(items)-1]
		beforeAt, beforeID = last.CreatedAtMilli, last.ID
	}
	if len(seen) != 10 {
		t.Fatalf("pagination covered %d unique items, want 10", len(seen))
	}
}

func TestChannelTimelineSearchesMessagesAndNotifications(t *testing.T) {
	data, ctx, user := timelineTestStore(t)
	defer data.Close()

	if err := data.BootstrapWebhook(ctx, "hook", "general"); err != nil {
		t.Fatal(err)
	}
	channel, err := data.ChannelIDByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateFromWebhook(ctx, "hook", IncomingNotification{Text: "database recovered", RawPayload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel, Text: "review the Phoenix rollout"}); err != nil {
		t.Fatal(err)
	}

	items, _, err := data.ListChannelTimeline(ctx, user, channel, "phoenix", 100, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != "message" || items[0].Message.Text != "review the Phoenix rollout" {
		t.Fatalf("message search = %#v", items)
	}
	items, _, err = data.ListChannelTimeline(ctx, user, channel, "RECOVERED", 100, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != "notification" {
		t.Fatalf("notification search = %#v", items)
	}
}

func TestChannelMessageUnreadCounts(t *testing.T) {
	data, ctx, user := timelineTestStore(t)
	defer data.Close()
	other, err := data.CreateUser(ctx, "bob", "another sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}

	if err := data.BootstrapWebhook(ctx, "hook", "general"); err != nil {
		t.Fatal(err)
	}
	channel, err := data.ChannelIDByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel, Text: "unread me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel, Text: "also unread"}); err != nil {
		t.Fatal(err)
	}
	count, err := data.UnreadCount(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("own messages produced unread count %d", count)
	}
	otherCount, err := data.UnreadCount(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if otherCount != 2 {
		t.Fatalf("other user's unread count = %d, want 2", otherCount)
	}
	if _, err := data.CreateChannelMessage(ctx, other, CreateMessageInput{ChannelID: channel, Text: "message from bob"}); err != nil {
		t.Fatal(err)
	}
	count, err = data.UnreadCount(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("incoming unread count = %d, want 1", count)
	}
	channels, err := data.ListChannels(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].UnreadCount != 1 {
		t.Fatalf("channel unread counts = %#v", channels)
	}
	if err := data.MarkChannelRead(ctx, user, channel, time.Now()); err != nil {
		t.Fatal(err)
	}
	count, err = data.UnreadCount(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unread count after mark read = %d, want 0", count)
	}
}

func TestChannelMessagePushPreferencesAndAuthorization(t *testing.T) {
	data, ctx, author := timelineTestStore(t)
	defer data.Close()
	if err := data.BootstrapWebhook(ctx, "hook", "general"); err != nil {
		t.Fatal(err)
	}
	channelID, err := data.ChannelIDByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := data.CreateUser(ctx, "bob", "another sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, subscription := range []PushSubscription{
		{UserID: author.ID, Endpoint: "https://push.example/author", P256DH: "author-key", Auth: "author-auth"},
		{UserID: reader.ID, Endpoint: "https://push.example/reader", P256DH: "reader-key", Auth: "reader-auth"},
	} {
		if err := data.SavePushSubscription(ctx, subscription); err != nil {
			t.Fatal(err)
		}
	}
	message, err := data.CreateChannelMessage(ctx, author, CreateMessageInput{ChannelID: channelID, Text: "message push"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := data.ListPushSubscriptionsForChannelMessage(ctx, message.ID)
	if err != nil || len(allowed) != 1 || allowed[0].UserID != reader.ID {
		t.Fatalf("message push subscriptions = %#v, error = %v", allowed, err)
	}
	if err := data.SetChannelNotificationPreference(ctx, reader, channelID, "critical"); err != nil {
		t.Fatal(err)
	}
	criticalOnly, err := data.ListPushSubscriptionsForChannelMessage(ctx, message.ID)
	if err != nil || len(criticalOnly) != 0 {
		t.Fatalf("critical-only message subscriptions = %#v, error = %v", criticalOnly, err)
	}
	if err := data.SetChannelNotificationPreference(ctx, reader, channelID, "muted"); err != nil {
		t.Fatal(err)
	}
	muted, err := data.ListPushSubscriptionsForChannelMessage(ctx, message.ID)
	if err != nil || len(muted) != 0 {
		t.Fatalf("muted message subscriptions = %#v, error = %v", muted, err)
	}
}
