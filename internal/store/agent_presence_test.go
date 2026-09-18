package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAgentConversationVisibilityAndExpiry(t *testing.T) {
	db := agentTestStore(t)
	ctx := context.Background()
	owner, err := db.CreateUser(ctx, "owner", "secure owner password", true)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := db.CreateUser(ctx, "reader", "secure reader password", false)
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "helper", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	channel, _, err := db.CreateChannel(ctx, CreateChannelInput{Name: "agent-chat", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReportAgentPresence(ctx, agent, channel.Name, "ready"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ungranted heartbeat: %v", err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReportAgentPresence(ctx, agent, channel.Name, "ready"); err != nil {
		t.Fatal(err)
	}
	entries, err := db.ListAgentConversations(ctx, reader)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private channel leak: %+v %v", entries, err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, reader.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	check := func(state string, count int) {
		t.Helper()
		entries, err := db.ListAgentConversations(ctx, reader)
		if err != nil || len(entries) != count {
			t.Fatalf("entries: %+v %v", entries, err)
		}
		if count > 0 && (entries[0].State != state || entries[0].AcceptsMessages != (state != "offline")) {
			t.Fatalf("availability: %+v", entries[0])
		}
	}
	check("ready", 1)
	if err := db.ReportAgentPresence(ctx, agent, channel.Name, "busy"); err != nil {
		t.Fatal(err)
	}
	check("busy", 1)
	if _, err := db.db.Exec(`UPDATE agent_presence SET updated_at=?`, time.Now().Add(-AgentPresenceTTL-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	check("offline", 1)
	if err := db.ReportAgentPresence(ctx, agent, channel.Name, "offline"); err != nil {
		t.Fatal(err)
	}
	check("offline", 1)
	if err := db.ReportAgentPresence(ctx, agent, channel.Name, "invented"); err == nil {
		t.Fatal("accepted invalid state")
	}
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	check("", 0)
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeAgent(ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	check("", 0)
	if err := db.ReportAgentPresence(ctx, agent, channel.Name, "ready"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked heartbeat: %v", err)
	}
}
