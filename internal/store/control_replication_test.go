package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestControlSnapshotReplicatesAuthenticationAndPrivateChannelState(t *testing.T) {
	ctx := context.Background()
	authority, err := Open(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = authority.Close() }()
	if err := authority.ConfigureReplication("cluster-test-01", "node-authority-01"); err != nil {
		t.Fatal(err)
	}
	if err := authority.ConfigureControlPlane("node-authority-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := authority.BootstrapUser(ctx, "admin", "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	viewer, err := authority.CreateUser(ctx, "viewer", "another sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	channel, _, err := authority.CreateChannel(ctx, CreateChannelInput{Name: "private", DisplayName: "Private", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.SetChannelMember(ctx, channel.ID, viewer.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := authority.CreateSession(ctx, viewer.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := authority.SavePushSubscription(ctx, PushSubscription{UserID: viewer.ID, Endpoint: "https://push.example/subscription", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	if err := authority.SaveSettings(ctx, map[string]string{"vapid_public_key": "public", "vapid_private_key": "private"}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := authority.BuildControlSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := Open(filepath.Join(t.TempDir(), "replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replica.Close() }()
	if err := replica.ConfigureReplication("cluster-test-01", "node-replica-01"); err != nil {
		t.Fatal(err)
	}
	if err := replica.ConfigureControlPlane("node-authority-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyControlSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := replica.AuthenticateUser(ctx, "viewer", "another sufficiently long password")
	if err != nil || got.ID != viewer.ID {
		t.Fatalf("authenticate replicated user = %#v, %v", got, err)
	}
	channels, err := replica.ListChannels(ctx, got)
	if err != nil || len(channels) != 1 || channels[0].ID != channel.ID {
		t.Fatalf("replicated channels = %#v, %v", channels, err)
	}
	subscriptions, err := replica.ListPushSubscriptions(ctx)
	if err != nil || len(subscriptions) != 1 || subscriptions[0].UserID != viewer.ID {
		t.Fatalf("replicated push subscriptions = %#v, %v", subscriptions, err)
	}
	if valid, err := replica.ControlLeaseValid(ctx); err != nil || !valid {
		t.Fatalf("control lease valid = %v, %v", valid, err)
	}
}

func TestControlSnapshotRejectsWrongAuthorityAndDigest(t *testing.T) {
	ctx := context.Background()
	authority, err := Open(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = authority.Close() }()
	if err := authority.ConfigureReplication("cluster-test-01", "node-authority-01"); err != nil {
		t.Fatal(err)
	}
	if err := authority.ConfigureControlPlane("node-authority-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	snapshot, err := authority.BuildControlSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := Open(filepath.Join(t.TempDir(), "replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replica.Close() }()
	if err := replica.ConfigureReplication("cluster-test-01", "node-replica-01"); err != nil {
		t.Fatal(err)
	}
	if err := replica.ConfigureControlPlane("node-other-0001", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyControlSnapshot(ctx, snapshot); err == nil {
		t.Fatal("accepted snapshot from wrong authority")
	}
	if err := replica.ConfigureControlPlane("node-authority-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	snapshot.Digest = "bad"
	if err := replica.ApplyControlSnapshot(ctx, snapshot); err == nil {
		t.Fatal("accepted snapshot with invalid digest")
	}
}

func TestControlSnapshotAcceptsLegacyWebhookRows(t *testing.T) {
	ctx := context.Background()
	authority, err := Open(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = authority.Close() }()
	if err := authority.ConfigureReplication("cluster-test-01", "node-authority-01"); err != nil {
		t.Fatal(err)
	}
	if err := authority.ConfigureControlPlane("node-authority-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := authority.BootstrapWebhook(ctx, "legacy-hook", "operations"); err != nil {
		t.Fatal(err)
	}

	snapshot, err := authority.BuildControlSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundWebhook := false
	for tableIndex := range snapshot.Tables {
		if snapshot.Tables[tableIndex].Name != "webhooks" {
			continue
		}
		if len(snapshot.Tables[tableIndex].Rows) != 1 || len(snapshot.Tables[tableIndex].Rows[0]) != 6 {
			t.Fatalf("unexpected webhook snapshot rows: %#v", snapshot.Tables[tableIndex].Rows)
		}
		snapshot.Tables[tableIndex].Rows[0] = snapshot.Tables[tableIndex].Rows[0][:5]
		foundWebhook = true
	}
	if !foundWebhook {
		t.Fatal("webhooks table missing from snapshot")
	}
	snapshot.Digest, err = controlSnapshotDigest(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	replica, err := Open(filepath.Join(t.TempDir(), "replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replica.Close() }()
	if err := replica.ConfigureReplication("cluster-test-01", "node-replica-01"); err != nil {
		t.Fatal(err)
	}
	if err := replica.ConfigureControlPlane("node-authority-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyControlSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.CreateFromWebhook(ctx, "legacy-hook", IncomingNotification{Text: "restored", RawPayload: []byte(`{}`)}); err != nil {
		t.Fatalf("legacy webhook was not restored as active: %v", err)
	}
}
