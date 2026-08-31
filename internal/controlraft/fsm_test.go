package controlraft

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestFSMApplyAndRestoreCommittedSnapshot(t *testing.T) {
	ctx := context.Background()
	source, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if err := source.ConfigureReplication("cluster-test-01", "node-leader-01"); err != nil {
		t.Fatal(err)
	}
	if err := source.ConfigureControlPlane("node-leader-01", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := source.BootstrapUser(ctx, "admin", "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.ExportControlSnapshot(ctx, "node-leader-01")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	newReplica := func(name string) (*store.Store, *FSM) {
		replica, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := replica.ConfigureReplication("cluster-test-01", name); err != nil {
			t.Fatal(err)
		}
		if err := replica.ConfigureControlPlane("node-leader-01", 30*time.Second); err != nil {
			t.Fatal(err)
		}
		return replica, &FSM{Data: replica}
	}

	replica, fsm := newReplica("node-replica-01")
	defer func() { _ = replica.Close() }()
	if err := AppliedError(fsm.Apply(&raft.Log{Data: encoded})); err != nil {
		t.Fatal(err)
	}
	if _, err := replica.AuthenticateUser(ctx, "admin", "a sufficiently long password"); err != nil {
		t.Fatalf("committed state not applied: %v", err)
	}

	restored, restoredFSM := newReplica("node-replica-02")
	defer func() { _ = restored.Close() }()
	if err := restoredFSM.Restore(io.NopCloser(bytes.NewReader(encoded))); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.AuthenticateUser(ctx, "admin", "a sufficiently long password"); err != nil {
		t.Fatalf("snapshot state not restored: %v", err)
	}
}
