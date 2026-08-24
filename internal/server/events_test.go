package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestWebhookPublishesEvent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapWebhook(context.Background(), "test-hook", "prometheus"); err != nil {
		t.Fatal(err)
	}

	s := &Server{store: db, subscribers: make(map[chan liveUpdate]struct{})}
	stream := newStreamRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.streamEvents(stream, request)
	}()
	stream.waitFor(t, ": connected\n\n")

	hookRequest := httptest.NewRequest(http.MethodPost, "/hooks/test-hook", strings.NewReader(`{"text":"firing"}`))
	hookRequest.SetPathValue("id", "test-hook")
	hookRequest.Header.Set("Content-Type", "application/json")
	hookResponse := httptest.NewRecorder()
	s.receiveWebhook(hookResponse, hookRequest)
	if hookResponse.Code != http.StatusOK {
		t.Fatalf("hook status = %d, body = %q", hookResponse.Code, hookResponse.Body.String())
	}
	stream.waitFor(t, "event: notification\n")
	s.publishMessage("msg_test")
	stream.waitFor(t, "event: channel-message\ndata: {\"id\":\"msg_test\"}\n\n")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event stream did not stop after cancellation")
	}
}

type streamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	written chan struct{}
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header), written: make(chan struct{}, 1)}
}

func (r *streamRecorder) Header() http.Header {
	return r.header
}

func (r *streamRecorder) WriteHeader(_ int) {}

func (r *streamRecorder) Write(value []byte) (int, error) {
	r.mu.Lock()
	n, err := r.body.Write(value)
	r.mu.Unlock()
	select {
	case r.written <- struct{}{}:
	default:
	}
	return n, err
}

func (r *streamRecorder) Flush() {}

func (r *streamRecorder) waitFor(t *testing.T, expected string) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		r.mu.Lock()
		found := strings.Contains(r.body.String(), expected)
		r.mu.Unlock()
		if found {
			return
		}
		select {
		case <-r.written:
		case <-timer.C:
			t.Fatalf("stream did not contain %q", expected)
		}
	}
}
