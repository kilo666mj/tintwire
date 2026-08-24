package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

type mcpClient struct {
	t       *testing.T
	handler http.Handler
	token   string
}

func (c *mcpClient) call(method, params string) (json.RawMessage, *struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}, int) {
	c.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"`
	if params != "" {
		body += `,"params":` + params
	}
	body += `}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	recorder := httptest.NewRecorder()
	c.handler.ServeHTTP(recorder, request)
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			c.t.Fatalf("decode %s response: %v (%q)", method, err, recorder.Body.String())
		}
	}
	return response.Result, response.Error, recorder.Code
}

type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (c *mcpClient) tool(name, arguments string) toolResult {
	c.t.Helper()
	result, rpcError, status := c.call("tools/call", `{"name":"`+name+`","arguments":`+arguments+`}`)
	if status != http.StatusOK || rpcError != nil {
		c.t.Fatalf("%s status = %d error = %+v", name, status, rpcError)
	}
	var parsed toolResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		c.t.Fatalf("decode %s result: %v", name, err)
	}
	return parsed
}

func (r toolResult) text() string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

func mcpFixture(t *testing.T, admin bool) (http.Handler, *store.Store, *mcpClient, store.ChannelSummary) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	owner, err := db.CreateUser(ctx, "admin", "secure admin password", true)
	if err != nil {
		t.Fatal(err)
	}
	channel, _, err := db.CreateChannel(ctx, store.CreateChannelInput{Name: "operations", DisplayName: "Operations", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := db.CreateAgent(ctx, store.CreateAgentInput{Name: "triage", OwnerUserID: owner.ID, IsAdmin: admin})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	return handler, db, &mcpClient{t: t, handler: handler, token: token}, channel
}

func TestMCPRequiresAgentCredential(t *testing.T) {
	handler, db, client, _ := mcpFixture(t, false)
	anonymous := &mcpClient{t: t, handler: handler}
	if _, _, status := anonymous.call("initialize", `{"protocolVersion":"2026-07-28"}`); status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", status)
	}
	// A reader session cookie is a different credential class and must not work.
	if err := db.BootstrapUser(context.Background(), "reader", "secure reader password"); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{"username":"reader","password":"secure reader password"}`))
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	cookie := loginRecorder.Result().Cookies()[0]
	withCookie := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	withCookie.AddCookie(cookie)
	cookieRecorder := httptest.NewRecorder()
	handler.ServeHTTP(cookieRecorder, withCookie)
	if cookieRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("session cookie status = %d, want 401", cookieRecorder.Code)
	}

	result, rpcError, status := client.call("initialize", `{"protocolVersion":"2026-07-28"}`)
	if status != http.StatusOK || rpcError != nil {
		t.Fatalf("initialize status = %d error = %+v", status, rpcError)
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.ProtocolVersion != "2026-07-28" || initialized.ServerInfo.Name != "tintwire" {
		t.Fatalf("initialize result = %+v", initialized)
	}
	if _, rpcError, _ := client.call("unknown/method", ""); rpcError == nil || rpcError.Code != -32601 {
		t.Fatalf("unknown method error = %+v", rpcError)
	}

	batch := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`))
	batch.Header.Set("Authorization", "Bearer "+client.token)
	batchRecorder := httptest.NewRecorder()
	handler.ServeHTTP(batchRecorder, batch)
	if !strings.Contains(batchRecorder.Body.String(), "batched requests are not supported") {
		t.Fatalf("batch body = %q", batchRecorder.Body.String())
	}

	notification := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	notification.Header.Set("Authorization", "Bearer "+client.token)
	notificationRecorder := httptest.NewRecorder()
	handler.ServeHTTP(notificationRecorder, notification)
	if notificationRecorder.Code != http.StatusAccepted || notificationRecorder.Body.Len() != 0 {
		t.Fatalf("initialized status = %d body = %q", notificationRecorder.Code, notificationRecorder.Body.String())
	}

	version := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	version.Header.Set("Authorization", "Bearer "+client.token)
	version.Header.Set("MCP-Protocol-Version", "1999-01-01")
	versionRecorder := httptest.NewRecorder()
	handler.ServeHTTP(versionRecorder, version)
	if versionRecorder.Code != http.StatusBadRequest {
		t.Fatalf("unsupported protocol version status = %d", versionRecorder.Code)
	}
}

func TestMCPInvokesRegisteredActionThroughSharedPath(t *testing.T) {
	var calls atomic.Int32
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"text":"action complete"}`)
	}))
	defer callback.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "mcp-action.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	owner, err := db.CreateUser(ctx, "admin", "secure admin password", true)
	if err != nil {
		t.Fatal(err)
	}
	channel, publishingToken, err := db.CreateChannel(ctx, store.CreateChannelInput{Name: "actions", DisplayName: "Actions", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	agent, agentToken, err := db.CreateAgent(ctx, store.CreateAgentInput{Name: "operator", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "operator"); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	if err := db.SaveSettings(ctx, map[string]string{"action_encryption_key": key}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveActionTarget(ctx, "callback", callback.URL, []byte{}, true); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true, ActionKey: key})
	if err != nil {
		t.Fatal(err)
	}

	card := `{"version":1,"title":"Run action","actions":[{"label":"Run","type":"http","target":"callback","context":{"job":"42"}}]}`
	publish := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(card))
	publish.Header.Set("Authorization", "Bearer "+publishingToken)
	publish.Header.Set("Content-Type", "application/json")
	publishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(publishRecorder, publish)
	if publishRecorder.Code != http.StatusCreated {
		t.Fatalf("publish=%d body=%q", publishRecorder.Code, publishRecorder.Body.String())
	}
	var published map[string]string
	if err := json.Unmarshal(publishRecorder.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}

	client := &mcpClient{t: t, handler: handler, token: agentToken}
	arguments := `{"id":"` + published["id"] + `","action_index":0,"idempotency_key":"mcp-action-0001"}`
	first := client.tool("notifications.invoke_action.v1", arguments)
	if first.IsError || !strings.Contains(first.text(), "action complete") {
		t.Fatalf("first result = %+v", first)
	}
	second := client.tool("notifications.invoke_action.v1", arguments)
	if second.IsError {
		t.Fatalf("retry result = %+v", second)
	}
	if calls.Load() != 1 {
		t.Fatalf("callback calls=%d, want 1", calls.Load())
	}
}

func TestMCPPublishRequiresChannelGrantAndIsIdempotent(t *testing.T) {
	_, db, client, channel := mcpFixture(t, false)
	ctx := context.Background()

	listed := client.tool("channels.list.v1", `{}`)
	if listed.IsError || !strings.Contains(listed.text(), `"operations"`) {
		t.Fatalf("channels.list = %+v", listed)
	}

	denied := client.tool("notifications.publish.v1", `{"channel":"operations","text":"hello","idempotency_key":"publish-0001"}`)
	if !denied.IsError || !strings.Contains(denied.text(), "not allowed") {
		t.Fatalf("ungranted publish = %+v", denied)
	}

	if err := db.SetChannelMember(ctx, channel.ID, "agent-triage", "operator"); err != nil {
		t.Fatal(err)
	}

	run := client.tool("runs.start.v1", `{"purpose":"investigate the nightly alert","idempotency_key":"run-0001"}`)
	if run.IsError {
		t.Fatalf("runs.start = %+v", run)
	}
	var startedRun struct {
		RunID string `json:"run_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(run.StructuredContent, &startedRun); err != nil {
		t.Fatal(err)
	}
	if startedRun.State != "running" {
		t.Fatalf("run = %+v", startedRun)
	}

	arguments := `{"channel":"operations","card":{"version":1,"title":"Nightly job failed","summary":"exit status 1","severity":"critical","source":"triage"},"run_id":"` + startedRun.RunID + `","idempotency_key":"publish-0002"}`
	published := client.tool("notifications.publish.v1", arguments)
	if published.IsError {
		t.Fatalf("publish = %s", published.text())
	}
	var publishedValue struct {
		ID      string `json:"id"`
		Channel string `json:"channel"`
		Agent   string `json:"agent"`
	}
	if err := json.Unmarshal(published.StructuredContent, &publishedValue); err != nil {
		t.Fatal(err)
	}
	if publishedValue.Channel != "operations" || publishedValue.Agent != "triage" {
		t.Fatalf("published = %+v", publishedValue)
	}

	replay := client.tool("notifications.publish.v1", arguments)
	if replay.IsError || !strings.Contains(replay.text(), publishedValue.ID) {
		t.Fatalf("replay = %s", replay.text())
	}
	conflict := client.tool("notifications.publish.v1", `{"channel":"operations","text":"different","idempotency_key":"publish-0002"}`)
	if !conflict.IsError || !strings.Contains(conflict.text(), "different arguments") {
		t.Fatalf("conflict = %s", conflict.text())
	}
	missingKey := client.tool("notifications.publish.v1", `{"channel":"operations","text":"hello","idempotency_key":"short"}`)
	if !missingKey.IsError {
		t.Fatalf("short idempotency key = %+v", missingKey)
	}
	stored, err := db.QueryNotifications(ctx, store.NotificationQuery{Limit: 10, UserAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored notifications = %d, want one after replay", len(stored))
	}

	acknowledged := client.tool("notifications.set_state.v1", `{"id":"`+publishedValue.ID+`","state":"acknowledged","run_id":"`+startedRun.RunID+`","idempotency_key":"state-0001"}`)
	if acknowledged.IsError {
		t.Fatalf("set_state = %s", acknowledged.text())
	}
	read := client.tool("notifications.get.v1", `{"id":"`+publishedValue.ID+`"}`)
	if read.IsError || !strings.Contains(read.text(), `"state":"acknowledged"`) {
		t.Fatalf("get = %s", read.text())
	}
	if strings.Contains(read.text(), "raw_payload") {
		t.Fatal("agent notification reads must not expose raw compatibility payloads")
	}

	finished := client.tool("runs.finish.v1", `{"run_id":"`+startedRun.RunID+`","state":"completed","idempotency_key":"run-9999"}`)
	if finished.IsError {
		t.Fatalf("runs.finish = %s", finished.text())
	}
	late := client.tool("runs.record.v1", `{"run_id":"`+startedRun.RunID+`","summary":"late","idempotency_key":"record-0001"}`)
	if !late.IsError {
		t.Fatalf("recording against a finished run = %+v", late)
	}

	agent, err := db.AgentByName(ctx, "triage")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := db.ListAgentRuns(ctx, agent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != "completed" || runs[0].Effects != 2 {
		t.Fatalf("runs = %+v, want one completed run with the publish and acknowledgement recorded", runs)
	}
}

func TestMCPAdministratorToolsAndResources(t *testing.T) {
	_, _, client, _ := mcpFixture(t, true)

	result, rpcError, status := client.call("tools/list", "")
	if status != http.StatusOK || rpcError != nil {
		t.Fatalf("tools/list status = %d error = %+v", status, rpcError)
	}
	if !strings.Contains(string(result), "channels.create.v1") {
		t.Fatalf("administrator tools = %s", result)
	}

	created := client.tool("channels.create.v1", `{"name":"agent-notes","display_name":"Agent notes","idempotency_key":"channel-0001"}`)
	if created.IsError {
		t.Fatalf("channels.create = %s", created.text())
	}
	var creation struct {
		PublishingToken string `json:"publishing_token"`
	}
	if err := json.Unmarshal(created.StructuredContent, &creation); err != nil {
		t.Fatal(err)
	}
	if len(creation.PublishingToken) != 64 {
		t.Fatalf("publishing token = %q", creation.PublishingToken)
	}
	replay := client.tool("channels.create.v1", `{"name":"agent-notes","display_name":"Agent notes","idempotency_key":"channel-0001"}`)
	if replay.IsError || strings.Contains(replay.text(), creation.PublishingToken) {
		t.Fatalf("replayed channel creation must not repeat the publishing token: %s", replay.text())
	}

	resources, rpcError, status := client.call("resources/read", `{"uri":"tintwire://channels"}`)
	if status != http.StatusOK || rpcError != nil {
		t.Fatalf("resources/read status = %d error = %+v", status, rpcError)
	}
	if !strings.Contains(string(resources), "agent-notes") {
		t.Fatalf("channel resource = %s", resources)
	}
	if _, rpcError, _ := client.call("resources/read", `{"uri":"https://example.com/secrets"}`); rpcError == nil {
		t.Fatal("unsupported resource URI was accepted")
	}
	if _, rpcError, _ := client.call("resources/read", `{"uri":"tintwire://notifications/ntf_missing"}`); rpcError == nil {
		t.Fatal("unknown notification resource was accepted")
	}
}

func TestMCPRejectsRevokedAgent(t *testing.T) {
	handler, db, client, _ := mcpFixture(t, false)
	if err := db.RevokeAgent(context.Background(), "triage"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	request.Header.Set("Authorization", "Bearer "+client.token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked agent status = %d, want 401", recorder.Code)
	}
}
