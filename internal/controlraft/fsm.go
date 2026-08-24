package controlraft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/hashicorp/raft"

	"github.com/kilo666mj/tintwire/internal/store"
)

const commandVersion = 1

type command struct {
	Version  int                   `json:"version"`
	Snapshot store.ControlSnapshot `json:"snapshot"`
}

type applyResult struct {
	Err error
}

// FSM applies only complete normalized snapshots. A proposed mutation is first
// evaluated against an isolated SQLite copy; no live node changes until Raft
// has durably committed this command to a majority.
type FSM struct {
	Data *store.Store

	mu     sync.RWMutex
	latest store.ControlSnapshot
}

func Encode(snapshot store.ControlSnapshot) ([]byte, error) {
	return json.Marshal(command{Version: commandVersion, Snapshot: snapshot})
}

func (f *FSM) Apply(log *raft.Log) any {
	var cmd command
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		return applyResult{Err: err}
	}
	if cmd.Version != commandVersion {
		return applyResult{Err: errors.New("unsupported control raft command")}
	}
	if err := f.Data.ApplyCommittedControlSnapshot(context.Background(), cmd.Snapshot); err != nil {
		return applyResult{Err: err}
	}
	f.mu.Lock()
	f.latest = cmd.Snapshot
	f.mu.Unlock()
	return applyResult{}
}

func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	snapshot := f.latest
	f.mu.RUnlock()
	if snapshot.Version == 0 {
		var err error
		snapshot, err = f.Data.ExportControlSnapshot(context.Background(), f.Data.ControlAuthority())
		if err != nil {
			return nil, err
		}
	}
	encoded, err := Encode(snapshot)
	if err != nil {
		return nil, err
	}
	return encodedSnapshot(encoded), nil
}

func (f *FSM) Restore(reader io.ReadCloser) error {
	defer reader.Close()
	var cmd command
	if err := json.NewDecoder(reader).Decode(&cmd); err != nil {
		return err
	}
	if cmd.Version != commandVersion {
		return errors.New("unsupported control raft snapshot")
	}
	if err := f.Data.ApplyCommittedControlSnapshot(context.Background(), cmd.Snapshot); err != nil {
		return err
	}
	f.mu.Lock()
	f.latest = cmd.Snapshot
	f.mu.Unlock()
	return nil
}

type encodedSnapshot []byte

func (snapshot encodedSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(snapshot); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (encodedSnapshot) Release() {}

func AppliedError(value any) error {
	result, ok := value.(applyResult)
	if !ok {
		return errors.New("invalid control raft apply response")
	}
	return result.Err
}
