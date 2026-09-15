package agentbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestTintwireClientCallsConversationTools(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request path/auth = %q/%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		var body string
		switch request.Method {
		case "initialize":
			body = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-07-28"}}`
		case "tools/call":
			if request.Params.Name == "messages.list.v1" {
				body = `{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"messages":[{"id":"msg_1","author":"michael","text":"hello","cursor":"cursor-1"}],"next_cursor":"cursor-1","has_more":false}}}`
			} else if request.Params.Name == "messages.publish.v1" {
				body = `{"jsonrpc":"2.0","id":3,"result":{"structuredContent":{"message":{"id":"msg_2"}}}}`
			} else {
				t.Fatalf("unexpected tool %q", request.Params.Name)
			}
		default:
			t.Fatalf("unexpected method %q", request.Method)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})

	client, err := NewTintwireClient("http://127.0.0.1:8080", "secret")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = transport
	ctx := context.Background()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := client.ListMessages(ctx, "agent-chat", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Text != "hello" || page.Messages[0].Cursor != "cursor-1" || page.NextCursor != "cursor-1" {
		t.Fatalf("page = %+v", page)
	}
	if err := client.PublishReply(ctx, "agent-chat", "msg_1", "done", "reply-msg_1"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestTintwireClientRequiresSecureRemoteURL(t *testing.T) {
	if _, err := NewTintwireClient("http://example.com", "secret"); err == nil {
		t.Fatal("insecure remote URL accepted")
	}
	if _, err := NewTintwireClient("http://127.0.0.1:8080", "secret"); err != nil {
		t.Fatalf("loopback URL rejected: %v", err)
	}
}
