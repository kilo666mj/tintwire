package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestReplicationShadowLogIsAtomicAndOrdered(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "replication.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	if err := data.ConfigureReplication("cluster-test-01", "node-test-01"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := data.BootstrapWebhook(ctx, "replication-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	notification, err := data.CreateFromWebhook(ctx, "replication-hook", IncomingNotification{
		Text: "disk pressure", State: "firing", RawPayload: json.RawMessage(`{"text":"disk pressure"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SetNotificationState(ctx, notification.ID, LocalInboxUser(), "resolved"); err != nil {
		t.Fatal(err)
	}
	operations, err := data.ReplicationOperations(ctx, "node-test-01", 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 {
		t.Fatalf("operations = %#v", operations)
	}
	if operations[0].Sequence != 1 || operations[0].Kind != "notification.created" || operations[1].Sequence != 2 || operations[1].Kind != "notification.state" {
		t.Fatalf("operation order = %#v", operations)
	}
	if operations[0].ClusterID != "cluster-test-01" || operations[0].PhysicalMS <= 0 || operations[1].PhysicalMS < operations[0].PhysicalMS {
		t.Fatalf("operation clocks = %#v", operations)
	}
	var statePayload struct {
		NotificationID string `json:"notification_id"`
		State          string `json:"state"`
	}
	if err := json.Unmarshal(operations[1].Payload, &statePayload); err != nil {
		t.Fatal(err)
	}
	if statePayload.NotificationID != notification.ID || statePayload.State != "resolved" {
		t.Fatalf("state payload = %#v", statePayload)
	}
	remaining, err := data.ReplicationOperations(ctx, "node-test-01", 1, 1)
	if err != nil || len(remaining) != 1 || remaining[0].Sequence != 2 {
		t.Fatalf("bounded cursor read = %#v, %v", remaining, err)
	}
}

func TestReplicationReplayEquivalentIdempotentAndContiguous(t *testing.T) {
	ctx := context.Background()
	source, err := Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if err := source.ConfigureReplication("cluster-test-01", "node-source-01"); err != nil {
		t.Fatal(err)
	}
	if err := source.BootstrapWebhook(ctx, "source-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	n, err := source.CreateFromWebhook(ctx, "source-hook", IncomingNotification{Text: "disk pressure", Username: "monitor", State: "firing", Attachments: json.RawMessage(`[{"text":"details"}]`), RawPayload: json.RawMessage(`{"source":"test"}`), ExternalKey: "alert-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetNotificationState(ctx, n.ID, LocalInboxUser(), "resolved"); err != nil {
		t.Fatal(err)
	}
	ops, err := source.ReplicationOperations(ctx, "node-source-01", 0, 500)
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
	if err := replica.ApplyReplicationOperations(ctx, ops[1:]); !errors.Is(err, ErrReplicationGap) {
		t.Fatalf("gap error = %v", err)
	}
	if err := replica.ApplyReplicationOperations(ctx, ops); err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyReplicationOperations(ctx, ops); err != nil {
		t.Fatalf("duplicate replay: %v", err)
	}
	got, err := replica.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != n.ID || got[0].Text != n.Text || got[0].State != "resolved" {
		t.Fatalf("replayed notification = %#v", got)
	}
	var externalKey string
	if err := replica.db.QueryRow(`SELECT external_key FROM notifications WHERE id=?`, n.ID).Scan(&externalKey); err != nil || externalKey != "alert-1" {
		t.Fatalf("external key = %q, %v", externalKey, err)
	}
	bad := append([]ReplicationOperation(nil), ops...)
	bad[0].Payload = json.RawMessage(`{"notification_id":"different"}`)
	if err := replica.ApplyReplicationOperations(ctx, bad); err == nil {
		t.Fatal("operation ID collision accepted")
	}
}

func TestReplicationIdentityRequiresBothValidIDs(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	for _, identity := range [][2]string{{"cluster-test-01", ""}, {"short", "node-test-01"}, {"cluster test", "node-test-01"}} {
		if err := data.ConfigureReplication(identity[0], identity[1]); err == nil {
			t.Fatalf("accepted identity %#v", identity)
		}
	}
	if err := data.ConfigureReplication("", ""); err != nil {
		t.Fatalf("disabled replication = %v", err)
	}
}

func TestExternalKeyConvergesAcrossWriters(t *testing.T) {
	ctx := context.Background()
	first, err := Open(filepath.Join(t.TempDir(), "first.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Open(filepath.Join(t.TempDir(), "second.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err := first.ConfigureReplication("cluster-test-01", "node-first-01"); err != nil {
		t.Fatal(err)
	}
	if err := first.ConfigureControlPlane("node-first-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := first.BootstrapWebhook(ctx, "shared-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	control, err := first.BuildControlSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.ConfigureReplication("cluster-test-01", "node-second-01"); err != nil {
		t.Fatal(err)
	}
	if err := second.ConfigureControlPlane("node-first-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := second.ApplyControlSnapshot(ctx, control); err != nil {
		t.Fatal(err)
	}
	a, err := first.CreateFromWebhook(ctx, "shared-hook", IncomingNotification{Text: "first writer", State: "resolved", ExternalKey: "alert-42", RawPayload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	b, err := second.CreateFromWebhook(ctx, "shared-hook", IncomingNotification{Text: "second writer", State: "firing", ExternalKey: "alert-42", RawPayload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("external key IDs differ: %q != %q", a.ID, b.ID)
	}
	firstOps, err := first.ReplicationOperations(ctx, "node-first-01", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	secondOps, err := second.ReplicationOperations(ctx, "node-second-01", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyReplicationOperations(ctx, secondOps); err != nil {
		t.Fatal(err)
	}
	if err := second.ApplyReplicationOperations(ctx, firstOps); err != nil {
		t.Fatal(err)
	}
	firstValues, err := first.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	secondValues, err := second.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstValues) != 1 || len(secondValues) != 1 || firstValues[0].ID != secondValues[0].ID || firstValues[0].Text != secondValues[0].Text || firstValues[0].State != "resolved" || secondValues[0].State != "resolved" {
		t.Fatalf("external-key convergence: first=%#v second=%#v", firstValues, secondValues)
	}
}
