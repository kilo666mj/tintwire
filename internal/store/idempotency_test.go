package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleAgentToolReservationCanRecover(t *testing.T) {
	ctx := context.Background()
	data, err := Open(filepath.Join(t.TempDir(), "agent-reservation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	owner, err := data.CreateUser(ctx, "owner", "a sufficiently long password", true)
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := data.CreateAgent(ctx, CreateAgentInput{Name: "retry-agent", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := []byte("request")
	if _, err := data.db.ExecContext(ctx, `INSERT INTO agent_tool_invocations(agent_id,idempotency_key,tool,request_fingerprint,status,result_json,created_at) VALUES(?,?,?,?, 'running',X'',?)`, agent.ID, "retry-key", "notify", fingerprint, time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := data.ReserveAgentToolInvocation(ctx, agent.ID, "retry-key", "notify", fingerprint); err != nil || !fresh {
		t.Fatalf("reserve stale invocation: fresh=%t err=%v", fresh, err)
	}
}

func TestStaleActionReservationCanRecover(t *testing.T) {
	ctx := context.Background()
	data, err := Open(filepath.Join(t.TempDir(), "action-reservation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.Close() }()
	if err := data.BootstrapWebhook(ctx, "action-hook", "actions"); err != nil {
		t.Fatal(err)
	}
	user, err := data.CreateUser(ctx, "operator", "a sufficiently long password", true)
	if err != nil {
		t.Fatal(err)
	}
	notification, err := data.CreateFromWebhook(ctx, "action-hook", IncomingNotification{Text: "act", RawPayload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.db.ExecContext(ctx, `INSERT INTO action_executions(operation_key,notification_id,action_index,user_id,status,created_at) VALUES(?,?,?,?, 'running',?)`, "retry-action", notification.ID, 0, user.ID, time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_, fresh, err := data.ReserveActionExecution(ctx, ActionExecution{Key: "retry-action", NotificationID: notification.ID, ActionIndex: 0, UserID: user.ID})
	if err != nil || !fresh {
		t.Fatalf("reserve stale action: fresh=%t err=%v", fresh, err)
	}
}
