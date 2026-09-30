package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	_ "time/tzdata"
)

// Additive tables use SQL common to SQLite and PostgreSQL. Live workflow state
// belongs to the authoritative database, not legacy replication snapshots.
const workflowSchema = `
CREATE INDEX IF NOT EXISTS notifications_updated_idx ON notifications(updated_at,id);
CREATE INDEX IF NOT EXISTS channel_messages_created_idx ON channel_messages(created_at,id);

CREATE TABLE IF NOT EXISTS attention_alerts (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 target TEXT NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL, created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS attention_preferences (
 user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 timezone TEXT NOT NULL, quiet_start TEXT NOT NULL, quiet_end TEXT NOT NULL,
 critical_bypass INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS notification_snoozes (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
 until_at BIGINT NOT NULL, PRIMARY KEY(user_id,notification_id)
);
CREATE TABLE IF NOT EXISTS workflow_schedules (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, target TEXT NOT NULL, config TEXT NOT NULL,
 due_at BIGINT NOT NULL, lease_until BIGINT NOT NULL DEFAULT 0, lease_token TEXT NOT NULL DEFAULT '',
 UNIQUE(user_id,kind,target)
);
CREATE INDEX IF NOT EXISTS workflow_schedules_due_idx ON workflow_schedules(due_at,lease_until);
CREATE TABLE IF NOT EXISTS delivery_events (
 id TEXT PRIMARY KEY, notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL DEFAULT '', device TEXT NOT NULL DEFAULT '',
 outcome TEXT NOT NULL, attempt INTEGER NOT NULL DEFAULT 0, created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS delivery_events_notification_idx ON delivery_events(notification_id,user_id,created_at);
CREATE TABLE IF NOT EXISTS digest_history (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 view_id TEXT NOT NULL REFERENCES saved_views(id) ON DELETE CASCADE,
 title TEXT NOT NULL, count INTEGER NOT NULL, created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS producer_monitors (
 id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
 name TEXT NOT NULL, source TEXT NOT NULL, period_seconds BIGINT NOT NULL,
 grace_seconds BIGINT NOT NULL, daily_json TEXT NOT NULL DEFAULT '{}', last_seen BIGINT NOT NULL, state TEXT NOT NULL,
 notification_id TEXT NOT NULL DEFAULT '', UNIQUE(channel_id,source)
);
CREATE TABLE IF NOT EXISTS agent_bindings (
 channel_id TEXT PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
 agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE, created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_commands (
 id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
 agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL UNIQUE REFERENCES channel_messages(id) ON DELETE CASCADE,
 author_id TEXT NOT NULL REFERENCES users(id), state TEXT NOT NULL,
 lease_owner TEXT NOT NULL DEFAULT '', lease_until BIGINT NOT NULL DEFAULT 0,
 turn_id TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '', cancel_requested INTEGER NOT NULL DEFAULT 0,
 created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_commands_queue_idx ON agent_commands(channel_id,state,created_at,id);
`

type AttentionAlert struct {
	ID     string
	Target string
	Kind   string
	Title  string
	Body   string
}

func (s *Store) SaveAttentionAlert(ctx context.Context, job WorkflowSchedule, title, body string, now time.Time) (bool, error) {
	id := fmt.Sprintf("attention:%s:%d", job.ID, job.DueAt)
	result, err := s.db.ExecContext(ctx, `INSERT INTO attention_alerts(id,user_id,target,kind,title,body,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, id, job.UserID, job.Target, job.Kind, title, body, now.UnixMilli())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) AttentionAlert(ctx context.Context, userID, id string) (AttentionAlert, error) {
	var a AttentionAlert
	err := s.db.QueryRowContext(ctx, `SELECT id,target,kind,title,body FROM attention_alerts WHERE id=? AND user_id=?`, id, userID).Scan(&a.ID, &a.Target, &a.Kind, &a.Title, &a.Body)
	return a, err
}

type AttentionPreferences struct {
	Timezone       string `json:"timezone"`
	QuietStart     string `json:"quiet_start"`
	QuietEnd       string `json:"quiet_end"`
	CriticalBypass bool   `json:"critical_bypass"`
}

func (p AttentionPreferences) Validate() error {
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return errors.New("use an IANA timezone such as Europe/Berlin")
	}
	if p.QuietStart == "" && p.QuietEnd == "" {
		return nil
	}
	for _, v := range []string{p.QuietStart, p.QuietEnd} {
		if _, err := time.Parse("15:04", v); err != nil {
			return errors.New("quiet hours must use HH:MM")
		}
	}
	if p.QuietStart == p.QuietEnd {
		return errors.New("quiet hours must have different start and end times")
	}
	return nil
}

func (p AttentionPreferences) Quiet(now time.Time, critical bool) bool {
	if p.QuietStart == "" || (critical && p.CriticalBypass) {
		return false
	}
	zone, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return false
	}
	v := now.In(zone).Format("15:04")
	if p.QuietStart < p.QuietEnd {
		return v >= p.QuietStart && v < p.QuietEnd
	}
	return v >= p.QuietStart || v < p.QuietEnd
}

func (s *Store) AttentionPreferences(ctx context.Context, userID string) (AttentionPreferences, error) {
	p := AttentionPreferences{Timezone: "UTC"}
	err := s.db.QueryRowContext(ctx, `SELECT timezone,quiet_start,quiet_end,critical_bypass FROM attention_preferences WHERE user_id=?`, userID).Scan(&p.Timezone, &p.QuietStart, &p.QuietEnd, &p.CriticalBypass)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return p, err
}

func (s *Store) SaveAttentionPreferences(ctx context.Context, userID string, p AttentionPreferences) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO attention_preferences(user_id,timezone,quiet_start,quiet_end,critical_bypass) VALUES(?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET timezone=excluded.timezone,quiet_start=excluded.quiet_start,quiet_end=excluded.quiet_end,critical_bypass=excluded.critical_bypass`, userID, p.Timezone, p.QuietStart, p.QuietEnd, boolInt(p.CriticalBypass))
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) Snooze(ctx context.Context, user User, id string, until time.Time) error {
	allowed, err := s.CanReadNotification(ctx, id, user)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	if !until.IsZero() && (!until.After(time.Now()) || until.After(time.Now().Add(30*24*time.Hour))) {
		return errors.New("snooze must end within the next 30 days")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if until.IsZero() {
		_, err = tx.ExecContext(ctx, `DELETE FROM notification_snoozes WHERE user_id=? AND notification_id=?`, user.ID, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE user_id=? AND kind='snooze' AND target=?`, user.ID, id)
		}
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO notification_snoozes(user_id,notification_id,until_at) VALUES(?,?,?) ON CONFLICT(user_id,notification_id) DO UPDATE SET until_at=excluded.until_at`, user.ID, id, until.UnixMilli())
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO workflow_schedules(id,user_id,kind,target,config,due_at) VALUES(?,?,'snooze',?,'{}',?) ON CONFLICT(user_id,kind,target) DO UPDATE SET due_at=excluded.due_at,lease_until=0,lease_token=''`, "snooze:"+user.ID+":"+id, user.ID, id, until.UnixMilli())
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DeliveryReason is also used for desktop notifications, so all clients apply
// the same preference and time-zone decisions.
func (s *Store) DeliveryReason(ctx context.Context, user User, n Notification, now time.Time) (string, error) {
	level, err := s.ChannelNotificationPreference(ctx, user, n.ChannelID)
	if err != nil {
		return "access_revoked", err
	}
	var card struct {
		Severity string `json:"severity"`
	}
	_ = json.Unmarshal(n.Card, &card)
	if level == "muted" {
		return "channel_muted", nil
	}
	if level == "critical" && card.Severity != "critical" {
		return "critical_only", nil
	}
	var snoozed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_snoozes WHERE user_id=? AND notification_id=? AND until_at>?)`, user.ID, n.ID, now.UnixMilli()).Scan(&snoozed); err != nil {
		return "", err
	}
	if snoozed {
		return "snoozed", nil
	}
	p, err := s.AttentionPreferences(ctx, user.ID)
	if err != nil {
		return "", err
	}
	if p.Quiet(now, card.Severity == "critical") {
		return "quiet_hours", nil
	}
	return "eligible", nil
}

type WorkflowSchedule struct {
	ID         string `json:"id"`
	UserID     string `json:"-"`
	Kind       string `json:"kind"`
	Target     string `json:"target"`
	Config     string `json:"config"`
	DueAt      int64  `json:"due_at"`
	LeaseToken string `json:"-"`
}

func (s *Store) ClaimSchedules(ctx context.Context, now time.Time) ([]WorkflowSchedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,kind,target,config,due_at FROM workflow_schedules WHERE due_at<=? AND lease_until<? ORDER BY due_at LIMIT 50`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	var candidates []WorkflowSchedule
	for rows.Next() {
		var v WorkflowSchedule
		if err = rows.Scan(&v.ID, &v.UserID, &v.Kind, &v.Target, &v.Config, &v.DueAt); err != nil {
			break
		}
		candidates = append(candidates, v)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	claimed := []WorkflowSchedule{}
	for _, v := range candidates {
		v.LeaseToken, err = newID("lease_", now)
		if err != nil {
			return nil, err
		}
		res, err := s.db.ExecContext(ctx, `UPDATE workflow_schedules SET lease_until=?,lease_token=? WHERE id=? AND due_at=? AND lease_until<?`, now.Add(5*time.Minute).UnixMilli(), v.LeaseToken, v.ID, v.DueAt, now.UnixMilli())
		if err != nil {
			return nil, err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count == 1 {
			claimed = append(claimed, v)
		}
	}
	return claimed, nil
}

func (s *Store) FinishSchedule(ctx context.Context, v WorkflowSchedule, next time.Time) error {
	if next.IsZero() {
		_, err := s.db.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE id=? AND lease_token=?`, v.ID, v.LeaseToken)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE workflow_schedules SET due_at=?,lease_until=0,lease_token='' WHERE id=? AND lease_token=?`, next.UnixMilli(), v.ID, v.LeaseToken)
	return err
}

func (s *Store) WorkflowUser(ctx context.Context, id string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id,username,is_admin FROM users WHERE id=? AND disabled_at IS NULL`, id).Scan(&u.ID, &u.Username, &u.IsAdmin)
	return u, err
}

type DigestConfig struct {
	Time     string `json:"time"`
	Timezone string `json:"timezone"`
}

func (d DigestConfig) Next(now time.Time) (time.Time, error) {
	zone, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return time.Time{}, errors.New("invalid timezone")
	}
	clock, err := time.Parse("15:04", d.Time)
	if err != nil {
		return time.Time{}, errors.New("digest time must use HH:MM")
	}
	local := now.In(zone)
	next := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, zone)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

func (s *Store) SaveDigest(ctx context.Context, userID, viewID string, d DigestConfig) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM saved_views WHERE id=? AND user_id=?)`, viewID, userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrForbidden
	}
	if d.Time == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE user_id=? AND kind='digest' AND target=?`, userID, viewID)
		return err
	}
	next, err := d.Next(time.Now())
	if err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO workflow_schedules(id,user_id,kind,target,config,due_at) VALUES(?,?,'digest',?,?,?) ON CONFLICT(user_id,kind,target) DO UPDATE SET config=excluded.config,due_at=excluded.due_at,lease_until=0,lease_token=''`, "digest:"+userID+":"+viewID, userID, viewID, string(raw), next.UnixMilli())
	return err
}

func (s *Store) UserSchedules(ctx context.Context, userID string) ([]WorkflowSchedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,target,config,due_at FROM workflow_schedules WHERE user_id=? ORDER BY due_at`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []WorkflowSchedule{}
	for rows.Next() {
		v := WorkflowSchedule{}
		if err := rows.Scan(&v.ID, &v.Kind, &v.Target, &v.Config, &v.DueAt); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

type DigestEntry struct {
	ID        string `json:"id"`
	ViewID    string `json:"view_id"`
	Title     string `json:"title"`
	Count     int    `json:"count"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) CreateDigest(ctx context.Context, v WorkflowSchedule, now time.Time) (DigestEntry, error) {
	u, err := s.WorkflowUser(ctx, v.UserID)
	if err != nil {
		return DigestEntry{}, err
	}
	views, err := s.ListSavedViews(ctx, u.ID)
	if err != nil {
		return DigestEntry{}, err
	}
	var view *SavedView
	for i := range views {
		if views[i].ID == v.Target {
			view = &views[i]
			break
		}
	}
	if view == nil {
		return DigestEntry{}, sql.ErrNoRows
	}
	q := NotificationQuery{UserID: u.ID, UserAdmin: u.IsAdmin, Channels: view.Channels, Search: view.Search, State: view.State, Severity: view.Severity, UnreadOnly: view.Unread, Limit: 201, OrderByUpdated: true}
	if q.State == "dismissed" {
		q.State = ""
		q.DismissedOnly = true
	}
	count := 0
	for {
		values, err := s.QueryNotifications(ctx, q)
		if err != nil {
			return DigestEntry{}, err
		}
		stop := false
		for _, n := range values {
			if n.UpdatedAt.Before(now.Add(-24 * time.Hour)) {
				stop = true
				break
			}
			count++
		}
		if stop || len(values) < 201 {
			break
		}
		last := values[len(values)-1]
		q.BeforeAt = last.UpdatedAt.UnixMilli()
		q.BeforeID = last.ID
	}
	d := DigestEntry{ID: fmt.Sprintf("%s:%d", v.ID, v.DueAt), ViewID: view.ID, Title: view.Name, Count: count, CreatedAt: now.UnixMilli()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO digest_history(id,user_id,view_id,title,count,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, d.ID, u.ID, d.ViewID, d.Title, d.Count, d.CreatedAt)
	return d, err
}

func (s *Store) Digests(ctx context.Context, userID string) ([]DigestEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,view_id,title,count,created_at FROM digest_history WHERE user_id=? ORDER BY created_at DESC LIMIT 30`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []DigestEntry{}
	for rows.Next() {
		var v DigestEntry
		if err := rows.Scan(&v.ID, &v.ViewID, &v.Title, &v.Count, &v.CreatedAt); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}
