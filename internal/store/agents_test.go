package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func agentTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "tintwire.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAgentForOAuthSubject(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oauth-agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	owner, err := db.CreateUser(ctx, "oauth-owner", "secure owner password", true)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "oauth-agent", OwnerUserID: owner.ID, OAuthSubject: "client-pocket-id-client"})
	if err != nil {
		t.Fatal(err)
	}
	agent, principal, err := db.AgentForOAuthSubject(ctx, "client-pocket-id-client")
	if err != nil {
		t.Fatal(err)
	}
	if agent.ID != created.ID || agent.OAuthSubject != "client-pocket-id-client" || principal.ID != created.UserID {
		t.Fatalf("resolved agent=%+v principal=%+v", agent, principal)
	}
	if _, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "duplicate-subject", OwnerUserID: owner.ID, OAuthSubject: "client-pocket-id-client"}); err == nil {
		t.Fatal("duplicate OAuth subject was accepted")
	}
}

func TestAgentRegistrationAuthenticatesOnlyItsOwnToken(t *testing.T) {
	ctx := context.Background()
	db := agentTestStore(t)
	owner, err := db.CreateUser(ctx, "operator", "operator-password", true)
	if err != nil {
		t.Fatal(err)
	}
	agent, token, err := db.CreateAgent(ctx, CreateAgentInput{Name: "triage", DisplayName: "Triage", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Username != "agent-triage" || agent.IsAdmin {
		t.Fatalf("agent = %+v, want non-admin principal agent-triage", agent)
	}
	resolved, user, err := db.AgentForToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != agent.ID || user.ID != agent.UserID || user.Username != "agent-triage" {
		t.Fatalf("resolved = %+v user = %+v", resolved, user)
	}
	if _, _, err := db.AgentForToken(ctx, "twa_deadbeef"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown token error = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := db.AgentForToken(ctx, "not-an-agent-token"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("foreign credential class error = %v, want ErrInvalidCredentials", err)
	}
	if _, err := db.AuthenticateUser(ctx, "agent-triage", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("agent principal password login error = %v, want ErrInvalidCredentials", err)
	}
}

func TestRevokeAgentStopsCredentialAndCancelsRuns(t *testing.T) {
	ctx := context.Background()
	db := agentTestStore(t)
	owner, err := db.CreateUser(ctx, "operator", "operator-password", true)
	if err != nil {
		t.Fatal(err)
	}
	agent, token, err := db.CreateAgent(ctx, CreateAgentInput{Name: "triage", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.StartAgentRun(ctx, agent.ID, owner.ID, "investigate alert")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeAgent(ctx, "triage"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AgentForToken(ctx, token); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("revoked token error = %v, want ErrInvalidCredentials", err)
	}
	runs, err := db.ListAgentRuns(ctx, agent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].State != "cancelled" {
		t.Fatalf("runs = %+v, want the open run cancelled", runs)
	}
	if err := db.RevokeAgent(ctx, "missing"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("revoke missing agent error = %v, want ErrAgentNotFound", err)
	}
}

func TestAgentPublishRequiresExplicitChannelGrant(t *testing.T) {
	ctx := context.Background()
	db := agentTestStore(t)
	owner, err := db.CreateUser(ctx, "operator", "operator-password", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateChannel(ctx, CreateChannelInput{Name: "operations", DisplayName: "Operations", Visibility: "public"}); err != nil {
		t.Fatal(err)
	}
	channel, _, err := db.CreateChannel(ctx, CreateChannelInput{Name: "secrets", DisplayName: "Secrets", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "triage", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	// A public channel is not implicitly writable by an agent.
	if _, err := db.CreateFromAgent(ctx, agent, "operations", "", IncomingNotification{Text: "hello"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("public channel publish error = %v, want ErrForbidden", err)
	}
	if _, err := db.CreateFromAgent(ctx, agent, "missing", "", IncomingNotification{Text: "hello"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unknown channel publish error = %v, want ErrForbidden", err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFromAgent(ctx, agent, "secrets", "", IncomingNotification{Text: "hello"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer publish error = %v, want ErrForbidden", err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "operator"); err != nil {
		t.Fatal(err)
	}
	run, err := db.StartAgentRun(ctx, agent.ID, owner.ID, "investigate alert")
	if err != nil {
		t.Fatal(err)
	}
	notification, err := db.CreateFromAgent(ctx, agent, "secrets", run.ID, IncomingNotification{Text: "hello", State: "firing"})
	if err != nil {
		t.Fatal(err)
	}
	if notification.Agent != "triage" || notification.Username != "triage" {
		t.Fatalf("notification = %+v, want triage attribution", notification)
	}
	stored, err := db.QueryNotifications(ctx, NotificationQuery{UserID: owner.ID, UserAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Agent != "triage" {
		t.Fatalf("stored = %+v, want agent attribution retained", stored)
	}
	if _, err := db.CreateFromAgent(ctx, agent, "secrets", run.ID, IncomingNotification{Text: "resolved", State: "resolved"}); err == nil {
		t.Fatal("agents must not publish terminal lifecycle states directly")
	}
}

func TestAgentRunLifecycleAndEffects(t *testing.T) {
	ctx := context.Background()
	db := agentTestStore(t)
	owner, err := db.CreateUser(ctx, "operator", "operator-password", true)
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "triage", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "summarizer", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.StartAgentRun(ctx, agent.ID, owner.ID, "investigate alert")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAgentRunEvent(ctx, agent.ID, run.ID, "notifications.publish", "published a card", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAgentRunEvent(ctx, other.ID, run.ID, "notifications.publish", "cross-agent write", ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-agent run event error = %v, want ErrRunNotFound", err)
	}
	if err := db.FinishAgentRun(ctx, other.ID, run.ID, "completed"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-agent finish error = %v, want ErrRunNotFound", err)
	}
	if err := db.FinishAgentRun(ctx, agent.ID, run.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishAgentRun(ctx, agent.ID, run.ID, "failed"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reopening a terminal run error = %v, want ErrInvalidTransition", err)
	}
	if err := db.RecordAgentRunEvent(ctx, agent.ID, run.ID, "notifications.publish", "late effect", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("late run event error = %v, want ErrInvalidTransition", err)
	}
	runs, err := db.ListAgentRuns(ctx, agent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State != "completed" || runs[0].Effects != 1 || runs[0].Initiator != "operator" {
		t.Fatalf("runs = %+v", runs)
	}
	events, err := db.ListAgentRunEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Tool != "notifications.publish" {
		t.Fatalf("events = %+v", events)
	}
}
