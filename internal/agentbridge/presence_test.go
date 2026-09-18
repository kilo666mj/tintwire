package agentbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type presenceRuntime struct {
	client *CodexClient
	state  atomic.Value
}

func (r *presenceRuntime) Close() error { return nil }
func (r *presenceRuntime) Write(data []byte) (int, error) {
	var request struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	r.client.mu.Lock()
	response := r.client.pending[request.ID]
	delete(r.client.pending, request.ID)
	r.client.mu.Unlock()
	response <- rpcEnvelope{Result: json.RawMessage(`{"thread":{"status":{"type":"` + r.state.Load().(string) + `"}}}`)}
	return len(data), nil
}

func TestBridgePresenceTracksRuntimeAndShutdown(t *testing.T) {
	runtime := &presenceRuntime{}
	runtime.state.Store("idle")
	codex := &CodexClient{pending: make(map[int64]chan rpcEnvelope), done: make(chan struct{}), stdin: runtime}
	runtime.client = codex
	reports := make(chan string, 20)
	keys := map[string]bool{}
	client, err := NewTintwireClient("http://127.0.0.1:8080", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Channel string `json:"channel"`
					State   string `json:"state"`
					Key     string `json:"idempotency_key"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Params.Name != "agents.heartbeat.v1" || request.Params.Arguments.Channel != "agent-chat" {
			t.Errorf("unexpected heartbeat: %+v", request)
		}
		if keys[request.Params.Arguments.Key] {
			t.Error("heartbeat key reused")
		}
		keys[request.Params.Arguments.Key] = true
		reports <- request.Params.Arguments.State
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"result":{"structuredContent":{"ttl_seconds":60}}}`))}, nil
	})
	bridge := &Bridge{Tintwire: client, Codex: codex, Config: Config{Channel: "agent-chat", ThreadID: "private-thread"}}
	stop := bridge.startPresence(context.Background())
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	expect := func(state string) {
		t.Helper()
		select {
		case got := <-reports:
			if got != state {
				t.Fatalf("state=%s want=%s", got, state)
			}
		case <-time.After(time.Second):
			t.Fatal("no heartbeat")
		}
	}
	expect("ready")
	bridge.busy.Store(true)
	bridge.notifyPresence()
	expect("busy")
	bridge.busy.Store(false)
	runtime.state.Store("active")
	bridge.notifyPresence()
	expect("busy")
	runtime.state.Store("idle")
	bridge.unavailable.Store(true)
	bridge.notifyPresence()
	expect("offline")
	bridge.unavailable.Store(false)
	bridge.notifyPresence()
	expect("ready")
	runtime.state.Store("notLoaded")
	bridge.notifyPresence()
	expect("offline")
	stop()
	stopped = true
	expect("offline")
}
