package replication

import "testing"

func TestPartitionRejoinConvergesAcrossOrderAndDuplicates(t *testing.T) {
	a, b, c := NewNode(), NewNode(), NewNode()
	created := Operation{Origin: "a", Sequence: 1, NotificationID: "n1", State: Firing}
	acknowledged := Operation{Origin: "b", Sequence: 1, NotificationID: "n1", State: Acknowledged}
	resolved := Operation{Origin: "c", Sequence: 1, NotificationID: "n1", State: Resolved}
	for _, item := range []struct {
		node *Node
		ops  []Operation
	}{{a, []Operation{created, acknowledged}}, {b, []Operation{acknowledged, created, created}}, {c, []Operation{resolved, created}}} {
		if err := item.node.Merge(item.ops); err != nil {
			t.Fatal(err)
		}
	}
	// Rejoin by exchanging snapshots in deliberately different orders.
	if err := a.Merge(c.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err := c.Merge(b.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(a.Snapshot()); err != nil {
		t.Fatal(err)
	}
	for name, node := range map[string]*Node{"a": a, "b": b, "c": c} {
		if got := node.State("n1"); got != Resolved {
			t.Fatalf("%s state = %d, want resolved", name, got)
		}
	}
}

func TestCursorDoesNotAdvanceAcrossGap(t *testing.T) {
	node := NewNode()
	if err := node.Apply(Operation{Origin: "a", Sequence: 2, NotificationID: "n2", State: Firing}); err != nil {
		t.Fatal(err)
	}
	if got := node.Cursor("a"); got != 0 {
		t.Fatalf("cursor with gap = %d", got)
	}
	if err := node.Apply(Operation{Origin: "a", Sequence: 1, NotificationID: "n1", State: Firing}); err != nil {
		t.Fatal(err)
	}
	if got := node.Cursor("a"); got != 2 {
		t.Fatalf("filled cursor = %d", got)
	}
}

func TestOperationIDCollisionIsQuarantined(t *testing.T) {
	node := NewNode()
	if err := node.Apply(Operation{Origin: "a", Sequence: 1, NotificationID: "n1", State: Firing}); err != nil {
		t.Fatal(err)
	}
	if err := node.Apply(Operation{Origin: "a", Sequence: 1, NotificationID: "n2", State: Firing}); err == nil {
		t.Fatal("mismatched duplicate was accepted")
	}
}
