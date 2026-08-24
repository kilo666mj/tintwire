package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestReplicationSnapshotMergesGapAndPreservesLocalAndControlState(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.ConfigureReplication("cluster-test-01", "node-source-01"); err != nil {
		t.Fatal(err)
	}
	if err := source.ConfigureControlPlane("node-source-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := source.BootstrapWebhook(ctx, "source-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	remote, err := source.CreateFromWebhook(ctx, "source-hook", IncomingNotification{Text: "remote", State: "firing", RawPayload: json.RawMessage(`{"source":"remote"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetNotificationState(ctx, remote.ID, LocalInboxUser(), "resolved"); err != nil {
		t.Fatal(err)
	}
	control, err := source.BuildControlSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	replica, err := Open(filepath.Join(t.TempDir(), "replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.ConfigureReplication("cluster-test-01", "node-replica-01"); err != nil {
		t.Fatal(err)
	}
	if err := replica.ConfigureControlPlane("node-source-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyControlSnapshot(ctx, control); err != nil {
		t.Fatal(err)
	}
	local, err := replica.CreateFromWebhook(ctx, "source-hook", IncomingNotification{Text: "local", State: "received", RawPayload: json.RawMessage(`{"source":"local"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.CreateUser(ctx, "local-user", "correct horse battery staple", false); err != nil {
		t.Fatal(err)
	}

	if err := source.PruneReplicationOperations(ctx, "node-source-01", 1); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.BuildReplicationSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyReplicationSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	values, err := replica.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Notification{}
	for _, value := range values {
		found[value.ID] = value
	}
	if len(found) != 2 || found[remote.ID].State != "resolved" || found[local.ID].Text != "local" {
		t.Fatalf("merged notifications = %#v", values)
	}
	if cursor, err := replica.ReplicationCursor(ctx, "node-source-01"); err != nil || cursor != 2 {
		t.Fatalf("source cursor = %d, %v", cursor, err)
	}
	if operations, err := replica.ReplicationOperations(ctx, "node-replica-01", 0, 10); err != nil || len(operations) != 1 {
		t.Fatalf("local operations = %#v, %v", operations, err)
	}
	if _, err := replica.AuthenticateUser(ctx, "local-user", "correct horse battery staple"); err != nil {
		t.Fatalf("snapshot changed Raft-controlled user state: %v", err)
	}

	tampered := snapshot
	tampered.Notifications = append([]ReplicationSnapshotNotification(nil), snapshot.Notifications...)
	tampered.Notifications[0].Text = "tampered"
	if err := replica.ApplyReplicationSnapshot(ctx, tampered); err == nil {
		t.Fatal("accepted snapshot with invalid digest")
	}
}

func TestPrunedLocalOriginContinuesSequence(t *testing.T) {
	ctx := context.Background()
	data, err := Open(filepath.Join(t.TempDir(), "prune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	if err := data.ConfigureReplication("cluster-test-01", "node-source-01"); err != nil {
		t.Fatal(err)
	}
	if err := data.BootstrapWebhook(ctx, "source-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateFromWebhook(ctx, "source-hook", IncomingNotification{Text: "first", RawPayload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := data.PruneReplicationOperations(ctx, "node-source-01", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := data.CreateFromWebhook(ctx, "source-hook", IncomingNotification{Text: "second", RawPayload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	operations, err := data.ReplicationOperations(ctx, "node-source-01", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Sequence != 2 {
		t.Fatalf("post-prune operations = %#v", operations)
	}
	if err := data.PruneReplicationOperations(ctx, "node-source-01", 3); err == nil {
		t.Fatal("accepted prune beyond retained high-water mark")
	}
}
