package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type ProducerMonitor struct {
	ID             string       `json:"id"`
	ChannelID      string       `json:"channel_id"`
	Name           string       `json:"name"`
	Source         string       `json:"source"`
	PeriodSeconds  int64        `json:"period_seconds"`
	GraceSeconds   int64        `json:"grace_seconds"`
	Daily          DigestConfig `json:"daily"`
	LastSeen       int64        `json:"last_seen"`
	State          string       `json:"state"`
	NotificationID string       `json:"notification_id"`
}

func (m ProducerMonitor) Deadline() time.Time {
	last := time.UnixMilli(m.LastSeen)
	if m.Daily.Time != "" {
		if next, err := m.Daily.Next(last); err == nil {
			return next.Add(time.Duration(m.GraceSeconds) * time.Second)
		}
	}
	return last.Add(time.Duration(m.PeriodSeconds+m.GraceSeconds) * time.Second)
}

func (s *Store) SaveMonitor(ctx context.Context, user User, m ProducerMonitor) (ProducerMonitor, error) {
	if !user.IsAdmin {
		return m, ErrForbidden
	}
	if err := s.RequireChannelOperator(ctx, user, m.ChannelID); err != nil {
		return m, err
	}
	m.Name = strings.TrimSpace(m.Name)
	m.Source = strings.TrimSpace(m.Source)
	if m.Name == "" || m.Source == "" || len(m.Name) > 100 || len(m.Source) > 100 || m.PeriodSeconds < 60 || m.PeriodSeconds > 31*86400 || m.GraceSeconds < 0 || m.GraceSeconds > 7*86400 {
		return m, errors.New("name, source, interval of 60 seconds to 31 days and grace of 0 to 7 days required")
	}
	if m.Daily.Time != "" {
		if _, err := m.Daily.Next(time.Now()); err != nil {
			return m, err
		}
	}
	raw, err := json.Marshal(m.Daily)
	if err != nil {
		return m, err
	}
	m.ID, err = newID("mon_", time.Now())
	if err != nil {
		return m, err
	}
	m.LastSeen = time.Now().UnixMilli()
	m.State = "healthy"
	_, err = s.db.ExecContext(ctx, `INSERT INTO producer_monitors(id,channel_id,name,source,period_seconds,grace_seconds,daily_json,last_seen,state) VALUES(?,?,?,?,?,?,?,?,?)`, m.ID, m.ChannelID, m.Name, m.Source, m.PeriodSeconds, m.GraceSeconds, string(raw), m.LastSeen, m.State)
	return m, err
}

func (s *Store) Monitors(ctx context.Context, user User) ([]ProducerMonitor, error) {
	if !user.IsAdmin {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,channel_id,name,source,period_seconds,grace_seconds,daily_json,last_seen,state,notification_id FROM producer_monitors ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []ProducerMonitor{}
	for rows.Next() {
		var m ProducerMonitor
		var raw string
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.Name, &m.Source, &m.PeriodSeconds, &m.GraceSeconds, &raw, &m.LastSeen, &m.State, &m.NotificationID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &m.Daily); err != nil {
			return nil, err
		}
		values = append(values, m)
	}
	return values, rows.Err()
}

func (s *Store) DeleteMonitor(ctx context.Context, user User, id string) error {
	if !user.IsAdmin {
		return ErrForbidden
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM producer_monitors WHERE id=?`, id)
	return err
}

// CheckMonitors uses a compare-and-swap inside the same transaction as the
// alert. Concurrent nodes can neither duplicate an alert nor lose a heartbeat.
func (s *Store) CheckMonitors(ctx context.Context, now time.Time) ([]string, error) {
	monitors, err := s.Monitors(ctx, User{IsAdmin: true})
	if err != nil {
		return nil, err
	}
	changed := []string{}
	for _, m := range monitors {
		state := "healthy"
		if now.After(m.Deadline()) {
			state = "missing"
		}
		if state == m.State {
			continue
		}
		id, err := s.transitionMonitor(ctx, m, state, now)
		if err != nil {
			return changed, err
		}
		if id != "" {
			changed = append(changed, id)
		}
	}
	return changed, nil
}

func (s *Store) transitionMonitor(ctx context.Context, m ProducerMonitor, state string, now time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	nid := "ntf_" + m.ID
	res, err := tx.ExecContext(ctx, `UPDATE producer_monitors SET state=?,notification_id=? WHERE id=? AND state=? AND last_seen=?`, state, nid, m.ID, m.State, m.LastSeen)
	if err != nil {
		return "", err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", nil
	}
	digest := sha256.Sum256([]byte("internal-monitor:" + m.ID))
	hook := "whk_" + m.ID
	if _, err := tx.ExecContext(ctx, `INSERT INTO webhooks(id,token_hash,channel_id,created_at,kind,revoked_at) VALUES(?,?,?,?,'monitor',?) ON CONFLICT(id) DO NOTHING`, hook, digest[:], m.ChannelID, now.UnixMilli(), now.UnixMilli()); err != nil {
		return "", err
	}
	lifecycle, severity, summary := "firing", "warning", "Expected notification from "+m.Source+" did not arrive before its deadline."
	if state == "healthy" {
		lifecycle, severity, summary = "resolved", "success", "Notifications from "+m.Source+" have resumed."
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "title": m.Name, "summary": summary, "severity": severity, "source": "Tintwire monitor"})
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,channel_id,webhook_id,text,username,icon_url,attachments_json,raw_payload_json,created_at,updated_at,state,card_json) VALUES(?,?,?,?,'Tintwire monitor','',?,?,?, ?,?,?) ON CONFLICT(id) DO UPDATE SET text=excluded.text,raw_payload_json=excluded.raw_payload_json,updated_at=excluded.updated_at,state=excluded.state,card_json=excluded.card_json`, nid, m.ChannelID, hook, summary, []byte(`[]`), raw, now.UnixMilli(), now.UnixMilli(), lifecycle, raw); err != nil {
		return "", err
	}
	if lifecycle == "firing" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notification_user_state WHERE notification_id=?`, nid); err != nil {
			return "", err
		}
	}
	if err := insertNotificationEvent(ctx, tx, nid, lifecycle, raw, now); err != nil {
		return "", err
	}
	return nid, tx.Commit()
}
