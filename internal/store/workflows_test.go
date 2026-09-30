package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

func workflowFixture(t *testing.T) (*Store, User, ChannelSummary, string) {
	t.Helper()
	db := workflowTestStore(t)
	ctx := context.Background()
	user, err := db.CreateUser(ctx, "workflow-owner", "long workflow password", true)
	if err != nil {
		t.Fatal(err)
	}
	channel, token, err := db.CreateChannel(ctx, CreateChannelInput{Name: "workflow", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	return db, user, channel, token
}

func TestAttentionTimezoneAndSnoozeIsolation(t *testing.T) {
	db, user, channel, token := workflowFixture(t)
	ctx := context.Background()
	prefs := AttentionPreferences{Timezone: "Europe/Berlin", QuietStart: "22:00", QuietEnd: "07:00", CriticalBypass: true}
	if err := db.SaveAttentionPreferences(ctx, user.ID, prefs); err != nil {
		t.Fatal(err)
	}
	evening := time.Date(2026, 9, 30, 21, 0, 0, 0, time.UTC)
	if !prefs.Quiet(evening, false) || prefs.Quiet(evening, true) || prefs.Quiet(evening.Add(10*time.Hour), false) {
		t.Fatal("overnight timezone/bypass policy")
	}
	if (AttentionPreferences{Timezone: "invalid"}).Validate() == nil {
		t.Fatal("invalid timezone accepted")
	}
	n, err := db.CreateFromWebhook(ctx, token, IncomingNotification{RawPayload: []byte(`{}`), Text: "test", State: "firing", Card: []byte(`{"version":1,"severity":"critical"}`)})
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := db.CreateUser(ctx, "outsider", "outsider password long", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Snooze(ctx, outsider, n.ID, time.Now().Add(time.Hour)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("private snooze: %v", err)
	}
	if err := db.Snooze(ctx, user, n.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	reason, err := db.DeliveryReason(ctx, user, n, time.Now())
	if err != nil || reason != "snoozed" {
		t.Fatalf("snooze: %s %v", reason, err)
	}
	if err := db.Snooze(ctx, user, n.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelNotificationPreference(ctx, user, channel.ID, "muted"); err != nil {
		t.Fatal(err)
	}
	reason, err = db.DeliveryReason(ctx, user, n, evening)
	if err != nil || reason != "channel_muted" {
		t.Fatalf("critical bypass overrode mute: %s %v", reason, err)
	}
}

func TestScheduleClaimsAndRevokedViewDigest(t *testing.T) {
	db, user, channel, token := workflowFixture(t)
	ctx := context.Background()
	n, err := db.CreateFromWebhook(ctx, token, IncomingNotification{RawPayload: []byte(`{}`), Text: "release", State: "received"})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	if err := db.Snooze(ctx, user, n.ID, until); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs, err := db.ClaimSchedules(ctx, until.Add(time.Second))
			if err != nil {
				t.Error(err)
			}
			counts <- len(jobs)
		}()
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	if total != 1 {
		t.Fatalf("duplicate schedule claims: %d", total)
	}
	reader, err := db.CreateUser(ctx, "digest-reader", "long digest reader password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, reader.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	view, err := db.SaveSavedView(ctx, reader.ID, SavedView{Name: "Releases", Channels: []string{channel.Name}})
	if err != nil {
		t.Fatal(err)
	}
	config := DigestConfig{Time: "08:00", Timezone: "Europe/Berlin"}
	if err := db.SaveDigest(ctx, reader.ID, view.ID, config); err != nil {
		t.Fatal(err)
	}
	job := WorkflowSchedule{ID: "daily-test", UserID: reader.ID, Target: view.ID, DueAt: 1}
	digest, err := db.CreateDigest(ctx, job, time.Now())
	if err != nil || digest.Count != 1 {
		t.Fatalf("digest count: %+v %v", digest, err)
	}
	if _, err := db.db.Exec(`DELETE FROM channel_memberships WHERE user_id=?`, reader.ID); err != nil {
		t.Fatal(err)
	}
	job.DueAt = 2
	digest, err = db.CreateDigest(ctx, job, time.Now())
	if err != nil || digest.Count != 0 {
		t.Fatalf("revoked channel in digest: %+v %v", digest, err)
	}
	if err := db.SaveDigest(ctx, user.ID, view.ID, config); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign digest: %v", err)
	}
	// Calendar scheduling follows local daylight saving, not a fixed 24h offset.
	next, err := config.Next(time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC))
	if err != nil || next.Hour() != 8 || next.UTC().Hour() != 7 {
		t.Fatalf("DST: %v %v", next, err)
	}
}

func TestIncidentGroupingPermissionsAndAtomicAcknowledgement(t *testing.T) {
	db, user, channel, token := workflowFixture(t)
	ctx := context.Background()
	_, token2, err := db.CreateWebhook(ctx, channel.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	// Two native producers and a compatibility producer share one explicit key.
	for index, input := range []IncomingNotification{
		{RawPayload: []byte(`{}`), Username: "metrics", State: "firing", Card: []byte(`{"incident_key":"service-a"}`)},
		{RawPayload: []byte(`{}`), Username: "logs", State: "resolved", Card: []byte(`{"incident_key":"service-a"}`)},
		{Username: "probe", State: "firing", RawPayload: []byte(`{"props":{"incident_key":"service-a"}}`)},
	} {
		publishingToken := token
		if index == 1 {
			publishingToken = token2
		}
		if _, err := db.CreateFromWebhook(ctx, publishingToken, input); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := db.Incidents(ctx, user)
	if err != nil || len(groups) != 1 || groups[0].Count != 3 {
		t.Fatalf("groups: %+v %v", groups, err)
	}
	outsider, err := db.CreateUser(ctx, "incident-reader", "long incident reader password", false)
	if err != nil {
		t.Fatal(err)
	}
	groups, err = db.Incidents(ctx, outsider)
	if err != nil || len(groups) != 0 {
		t.Fatalf("private groups: %+v %v", groups, err)
	}
	if err := db.AcknowledgeIncident(ctx, outsider, channel.ID, "service-a"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthorized ack: %v", err)
	}
	if err := db.AcknowledgeIncident(ctx, user, channel.ID, "service-a"); err != nil {
		t.Fatal(err)
	}
	values, err := db.IncidentNotifications(ctx, user, channel.ID, "service-a")
	if err != nil {
		t.Fatal(err)
	}
	ack, resolved := 0, 0
	for _, n := range values {
		if n.State == "acknowledged" {
			ack++
		}
		if n.State == "resolved" {
			resolved++
		}
	}
	if ack != 2 || resolved != 1 {
		t.Fatalf("state counts %d/%d", ack, resolved)
	}
}

func TestProducerMonitorFailureAndRecovery(t *testing.T) {
	db, user, channel, token := workflowFixture(t)
	ctx := context.Background()
	m, err := db.SaveMonitor(ctx, user, ProducerMonitor{Name: "Backup", Source: "backup", ChannelID: channel.ID, PeriodSeconds: 60, GraceSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Minute).UnixMilli()
	if _, err := db.db.Exec(`UPDATE producer_monitors SET last_seen=? WHERE id=?`, past, m.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := db.CheckMonitors(ctx, time.Now())
	if err != nil || len(ids) != 1 {
		t.Fatalf("missing: %v %v", ids, err)
	}
	again, err := db.CheckMonitors(ctx, time.Now())
	if err != nil || len(again) != 0 {
		t.Fatalf("duplicate alert: %v %v", again, err)
	}
	if _, err := db.CreateFromWebhook(ctx, token, IncomingNotification{RawPayload: []byte(`{}`), Username: "backup", Text: "finished"}); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.CheckMonitors(ctx, time.Now())
	if err != nil || len(recovered) != 1 || recovered[0] != ids[0] {
		t.Fatalf("recovery: %v %v", recovered, err)
	}
	values, err := db.QueryNotifications(ctx, NotificationQuery{ID: ids[0]})
	if err != nil || len(values) != 1 || values[0].State != "resolved" {
		t.Fatalf("recovered card: %+v %v", values, err)
	}
}

func TestDurableAgentQueueLeasesRecoveryAndReply(t *testing.T) {
	db, user, channel, _ := workflowFixture(t)
	ctx := context.Background()
	agent, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "queue-agent", OwnerUserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := db.BindAgent(ctx, user, channel.ID, agent.Name); err != nil {
		t.Fatal(err)
	}
	assertPending := func(want bool) {
		t.Helper()
		got, err := db.AgentCommandPending(ctx, agent, channel.Name)
		if err != nil || got != want {
			t.Fatalf("pending: got %v, want %v: %v", got, want, err)
		}
	}
	assertPending(false)
	// Ensure the boundary test is independent of the wall clock's precision.
	if _, err := db.db.Exec(`UPDATE agent_bindings SET created_at=?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	first, err := db.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel.ID, Text: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateChannelMessageFromAgent(ctx, agent, channel.Name, "", CreateMessageInput{Text: "Never execute this"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateChannelMessage(ctx, user, CreateMessageInput{ChannelID: channel.ID, Text: "A reply", ParentID: first.ID}); err != nil {
		t.Fatal(err)
	}
	assertPending(true)
	one, err := db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-one")
	if err != nil || one == nil || one.MessageID != first.ID {
		t.Fatalf("claim: %+v %v", one, err)
	}
	assertPending(false)
	two, err := db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-two")
	if err != nil || two != nil {
		t.Fatalf("duplicate claim: %+v %v", two, err)
	}
	if _, err := db.RenewAgentCommand(ctx, agent, one.ID, "wrong-owner", "turn-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign lease: %v", err)
	}
	if _, err := db.RenewAgentCommand(ctx, agent, one.ID, "bridge-one", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE agent_commands SET lease_until=0 WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	assertPending(true)
	recovered, err := db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-two")
	if err != nil || recovered == nil || recovered.State != "interrupted" || recovered.TurnID != "turn-1" {
		t.Fatalf("recovery must not replay: %+v %v", recovered, err)
	}
	reply, err := db.CompleteAgentCommand(ctx, agent, one.ID, "bridge-two", "completed", "Investigation complete")
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.ChannelMessageByID(ctx, user, reply)
	if err != nil || message.ParentID != first.ID || message.AuthorUserID != agent.UserID {
		t.Fatalf("reply: %+v %v", message, err)
	}
	assertPending(false)
	empty, err := db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-two")
	if err != nil || empty != nil {
		t.Fatalf("agent/reply feedback loop: %+v %v", empty, err)
	}
}

func workflowTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TINTWIRE_TEST_WORKFLOW_POSTGRES_DSN")
	if dsn == "" {
		return agentTestStore(t)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("tintwire-postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name, err := newID("workflow_", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	db, err := OpenPostgres(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if _, err := admin.Exec(`DROP DATABASE "` + name + `"`); err != nil {
			t.Error(err)
		}
		_ = admin.Close()
	})
	return db
}

func TestQueueCancellationSurvivesLeaseExpiryAndRevokedAuthors(t *testing.T) {
	db, owner, channel, _ := workflowFixture(t)
	ctx := context.Background()
	agent, _, err := db.CreateAgent(ctx, CreateAgentInput{Name: "cancel-agent", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, agent.Username, "operator"); err != nil {
		t.Fatal(err)
	}
	// Messages submitted before binding are never backfilled.
	if _, err := db.CreateChannelMessage(ctx, owner, CreateMessageInput{ChannelID: channel.ID, Text: "Historical"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindAgent(ctx, owner, channel.ID, agent.Name); err != nil {
		t.Fatal(err)
	}
	if command, err := db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-one"); err != nil || command != nil {
		t.Fatalf("historical command: %+v %v", command, err)
	}
	if _, err := db.CreateChannelMessage(ctx, owner, CreateMessageInput{ChannelID: channel.ID, Text: "Start"}); err != nil {
		t.Fatal(err)
	}
	command, err := db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-one")
	if err != nil || command == nil {
		t.Fatalf("claim: %+v %v", command, err)
	}
	if _, err := db.RenewAgentCommand(ctx, agent, command.ID, "bridge-one", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.ControlAgentCommand(ctx, owner, command.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE agent_commands SET lease_until=0 WHERE id=?`, command.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-two")
	if err != nil || recovered == nil || !recovered.CancelRequested || recovered.State != "interrupted" {
		t.Fatalf("lost cancellation: %+v %v", recovered, err)
	}
	if _, err := db.CompleteAgentCommand(ctx, agent, command.ID, "bridge-two", "cancelled", "Stopped"); err != nil {
		t.Fatal(err)
	}
	human, err := db.CreateUser(ctx, "operator", "long operator password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, human.Username, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateChannelMessage(ctx, human, CreateMessageInput{ChannelID: channel.ID, Text: "Queued before demotion"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelMember(ctx, channel.ID, human.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	command, err = db.ClaimAgentCommand(ctx, agent, channel.Name, "bridge-two")
	if err != nil || command != nil {
		t.Fatalf("revoked author executed: %+v %v", command, err)
	}
}
