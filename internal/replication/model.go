// Package replication contains the deterministic convergence model used to
// validate protocol rules before production transport and persistence exist.
package replication

import (
	"errors"
	"fmt"
	"sort"
)

type State uint8

const (
	Received State = iota
	Firing
	Acknowledged
	Resolved
)

type Operation struct {
	Origin         string
	Sequence       uint64
	NotificationID string
	State          State
}

func (o Operation) ID() string { return fmt.Sprintf("%s:%d", o.Origin, o.Sequence) }

type Node struct {
	states  map[string]State
	applied map[string]Operation
}

func NewNode() *Node {
	return &Node{states: make(map[string]State), applied: make(map[string]Operation)}
}

func (n *Node) Apply(operation Operation) error {
	if operation.Origin == "" || operation.Sequence == 0 || operation.NotificationID == "" || operation.State > Resolved {
		return errors.New("invalid operation")
	}
	if existing, ok := n.applied[operation.ID()]; ok {
		if existing != operation {
			return errors.New("operation ID collision")
		}
		return nil
	}
	n.applied[operation.ID()] = operation
	if operation.State > n.states[operation.NotificationID] {
		n.states[operation.NotificationID] = operation.State
	}
	return nil
}

func (n *Node) State(notificationID string) State { return n.states[notificationID] }

func (n *Node) Cursor(origin string) uint64 {
	var cursor uint64
	for {
		next := cursor + 1
		if _, ok := n.applied[fmt.Sprintf("%s:%d", origin, next)]; !ok {
			return cursor
		}
		cursor = next
	}
}

func (n *Node) Snapshot() []Operation {
	operations := make([]Operation, 0, len(n.applied))
	for _, operation := range n.applied {
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].Origin == operations[j].Origin {
			return operations[i].Sequence < operations[j].Sequence
		}
		return operations[i].Origin < operations[j].Origin
	})
	return operations
}

func (n *Node) Merge(operations []Operation) error {
	for _, operation := range operations {
		if err := n.Apply(operation); err != nil {
			return err
		}
	}
	return nil
}
