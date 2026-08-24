package replicationhttp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestSyncPeerPullsContiguousOperations(t *testing.T) {
	ctx := context.Background()
	source, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.ConfigureReplication("cluster-test-01", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := source.ConfigureControlPlane("127.0.0.1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := source.CreateUser(ctx, "replicated-viewer", "a sufficiently long password", false); err != nil {
		t.Fatal(err)
	}
	if err := source.BootstrapWebhook(ctx, "hook-source", "operations"); err != nil {
		t.Fatal(err)
	}
	created, err := source.CreateFromWebhook(ctx, "hook-source", store.IncomingNotification{Text: "replicate me", State: "firing", RawPayload: json.RawMessage(`{"ok":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(Handler(source))
	defer server.Close()
	peer, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	replica, err := store.Open(filepath.Join(t.TempDir(), "replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.ConfigureReplication("cluster-test-01", "node-replica-01"); err != nil {
		t.Fatal(err)
	}
	if err := replica.ConfigureControlPlane("127.0.0.1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	syncer := Syncer{Data: replica, Client: server.Client()}
	if _, err := syncer.syncPeer(ctx, peer); err != nil {
		t.Fatal(err)
	}
	got, err := replica.ListNotifications(ctx, 10)
	if err != nil || len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("notifications=%#v err=%v", got, err)
	}
	if _, err := replica.AuthenticateUser(ctx, "replicated-viewer", "a sufficiently long password"); err != nil {
		t.Fatalf("authenticate control-snapshot user: %v", err)
	}
	if _, err := syncer.syncPeer(ctx, peer); err != nil {
		t.Fatalf("idempotent sync: %v", err)
	}
}

func TestParsePeersRejectsNonHTTPS(t *testing.T) {
	if _, err := ParsePeers("http://node.example:18090"); err == nil {
		t.Fatal("accepted non-HTTPS peer")
	}
}

func TestSyncPeerFallsBackToMergedSnapshotAcrossRetainedGap(t *testing.T) {
	ctx := context.Background()
	source, err := store.Open(filepath.Join(t.TempDir(), "source-gap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.ConfigureReplication("cluster-test-01", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := source.ConfigureControlPlane("127.0.0.1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := source.BootstrapWebhook(ctx, "gap-hook", "operations"); err != nil {
		t.Fatal(err)
	}
	remote, err := source.CreateFromWebhook(ctx, "gap-hook", store.IncomingNotification{Text: "remote gap", State: "firing", RawPayload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetNotificationState(ctx, remote.ID, store.LocalInboxUser(), "resolved"); err != nil {
		t.Fatal(err)
	}
	control, err := source.BuildControlSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.PruneReplicationOperations(ctx, "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}

	replica, err := store.Open(filepath.Join(t.TempDir(), "replica-gap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.ConfigureReplication("cluster-test-01", "node-replica-01"); err != nil {
		t.Fatal(err)
	}
	if err := replica.ConfigureControlPlane("127.0.0.1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyControlSnapshot(ctx, control); err != nil {
		t.Fatal(err)
	}
	local, err := replica.CreateFromWebhook(ctx, "gap-hook", store.IncomingNotification{Text: "preserve local", RawPayload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewTLSServer(Handler(source))
	defer server.Close()
	peer, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	syncer := Syncer{Data: replica, Client: server.Client(), DisableControlSnapshots: true}
	if _, err := syncer.syncPeer(ctx, peer); err != nil {
		t.Fatal(err)
	}
	values, err := replica.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]store.Notification)
	for _, value := range values {
		found[value.ID] = value
	}
	if len(found) != 2 || found[remote.ID].State != "resolved" || found[local.ID].Text != "preserve local" {
		t.Fatalf("snapshot fallback notifications = %#v", values)
	}
}
