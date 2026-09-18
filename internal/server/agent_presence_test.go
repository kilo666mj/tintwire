package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestAgentHeartbeatAndReaderDirectory(t *testing.T) {
	handler, db, client, channel := mcpFixture(t, false)
	ctx := context.Background()
	ready := `{"channel":"operations","state":"ready","idempotency_key":"heartbeat-ready"}`
	if result := client.tool("agents.heartbeat.v1", ready); !result.IsError {
		t.Fatal("heartbeat without channel authority succeeded")
	}
	if err := db.SetChannelMember(ctx, channel.ID, "agent-triage", "operator"); err != nil {
		t.Fatal(err)
	}
	if result := client.tool("agents.heartbeat.v1", ready); result.IsError {
		t.Fatal(result.text())
	}
	if result := client.tool("agents.heartbeat.v1", `{"channel":"operations","state":"busy","idempotency_key":"heartbeat-busy"}`); result.IsError {
		t.Fatal(result.text())
	}
	// An old retry must not reset busy to ready or renew the old heartbeat.
	if result := client.tool("agents.heartbeat.v1", ready); result.IsError {
		t.Fatal(result.text())
	}
	if result := client.tool("agents.heartbeat.v1", `{"channel":"operations","state":"offline","idempotency_key":"heartbeat-ready"}`); !result.IsError {
		t.Fatal("conflicting heartbeat key accepted")
	}
	request := httptest.NewRequest("GET", "/api/v1/agent-conversations", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous directory: %d", response.Code)
	}
	if _, err := db.CreateUser(ctx, "viewer", "secure viewer password", false); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(`{"username":"viewer","password":"secure viewer password"}`))
	login.Header.Set("Origin", "http://example.com")
	logged := httptest.NewRecorder()
	handler.ServeHTTP(logged, login)
	if logged.Code != http.StatusOK {
		t.Fatal(logged.Body.String())
	}
	request.AddCookie(logged.Result().Cookies()[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var result struct {
		Agents []store.AgentConversation `json:"agents"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Agents) != 1 || result.Agents[0].State != "busy" {
		t.Fatalf("directory: %d %s", response.Code, response.Body.String())
	}
	for _, private := range []string{"oauth_subject", "owner", "user_id", client.token, "thread_id"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("directory leaks %s", private)
		}
	}
}
