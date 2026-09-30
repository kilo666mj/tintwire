package agentbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type queueRuntime struct {
	client  *CodexClient
	methods []string
	mu      sync.Mutex
	history string
}

func (r *queueRuntime) Close() error { return nil }
func (r *queueRuntime) Write(data []byte) (int, error) {
	var input struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.methods = append(r.methods, input.Method)
	r.mu.Unlock()
	result := json.RawMessage(`{}`)
	if input.Method == "thread/read" {
		result = json.RawMessage(r.history)
	}
	r.client.mu.Lock()
	reply := r.client.pending[input.ID]
	delete(r.client.pending, input.ID)
	r.client.mu.Unlock()
	if reply != nil {
		reply <- rpcEnvelope{Result: result}
	}
	return len(data), nil
}

func TestQueueRecoveryPublishesExistingResultWithoutNewTurn(t *testing.T) {
	runtime := &queueRuntime{history: `{"thread":{"turns":[{"id":"turn-1","status":"completed","items":[{"type":"agentMessage","text":"Already finished","phase":"final_answer"}]}]}}`}
	codex := &CodexClient{pending: make(map[int64]chan rpcEnvelope), done: make(chan struct{}), events: make(chan rpcEnvelope, 8), stdin: runtime}
	runtime.client = codex
	client, err := NewTintwireClient("http://127.0.0.1:8080", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var input struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		structured := `{"command":{"id":"command-1","state":"interrupted","turn_id":"turn-1"}}`
		if input.Params.Name == "commands.complete.v1" {
			completed = true
			if input.Params.Arguments["state"] != "completed" || input.Params.Arguments["result"] != "Already finished" {
				t.Errorf("completion: %+v", input.Params.Arguments)
			}
			structured = `{"message_id":"reply-1"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"result":{"structuredContent":` + structured + `}}`))}, nil
	})
	bridge := &Bridge{Tintwire: client, Codex: codex, Config: Config{ThreadID: "thread-1", Channel: "agent-chat"}}
	if err := bridge.runQueuedCommand(context.Background(), "bridge-one", queuedCommand{ID: "command-1"}); err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("existing result was not published")
	}
	for _, method := range runtime.methods {
		if method == "turn/start" {
			t.Fatal("recovery started a duplicate turn")
		}
	}
}

func TestUnknownRuntimeTurnRemainsUnreconciled(t *testing.T) {
	runtime := &queueRuntime{history: `{"thread":{"turns":[]}}`}
	codex := &CodexClient{pending: make(map[int64]chan rpcEnvelope), done: make(chan struct{}), stdin: runtime}
	runtime.client = codex
	_, err := codex.recoverTurn(context.Background(), "thread-1", "missing-turn")
	if !errors.Is(err, errTurnUnknown) {
		t.Fatalf("unknown turn: %v", err)
	}
	if len(runtime.methods) != 1 || runtime.methods[0] != "thread/read" {
		t.Fatalf("unexpected calls: %+v", runtime.methods)
	}
}
