package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const incidentSchema = `CREATE TABLE IF NOT EXISTS notification_incidents (
 notification_id TEXT PRIMARY KEY REFERENCES notifications(id) ON DELETE CASCADE,
 incident_key TEXT NOT NULL
); CREATE INDEX IF NOT EXISTS notification_incidents_key_idx ON notification_incidents(incident_key);`

func saveIncidentKey(ctx context.Context, tx *sql.Tx, id string, input IncomingNotification) error {
	var card struct {
		Key string `json:"incident_key"`
	}
	_ = json.Unmarshal(input.Card, &card)
	key := strings.TrimSpace(card.Key)
	if key == "" {
		var payload struct {
			Props struct {
				Key string `json:"incident_key"`
			} `json:"props"`
		}
		_ = json.Unmarshal(input.RawPayload, &payload)
		key = strings.TrimSpace(payload.Props.Key)
	}
	if len(key) > 200 {
		return errors.New("incident_key must be at most 200 bytes")
	}
	if key == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM notification_incidents WHERE notification_id=?`, id)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO notification_incidents(notification_id,incident_key) VALUES(?,?) ON CONFLICT(notification_id) DO UPDATE SET incident_key=excluded.incident_key`, id, key)
	return err
}

type Incident struct {
	Key       string `json:"key"`
	ChannelID string `json:"channel_id"`
	Channel   string `json:"channel"`
	Count     int    `json:"count"`
	Firing    int    `json:"firing"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Store) Incidents(ctx context.Context, user User) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.incident_key,c.id,c.name,COUNT(*),SUM(CASE WHEN n.state='firing' THEN 1 ELSE 0 END),MAX(n.updated_at) FROM notification_incidents i JOIN notifications n ON n.id=i.notification_id JOIN channels c ON c.id=n.channel_id WHERE (? OR c.visibility='public' OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.user_id=? AND m.channel_id=c.id)) GROUP BY i.incident_key,c.id,c.name ORDER BY MAX(n.updated_at) DESC LIMIT 200`, user.IsAdmin, user.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []Incident{}
	for rows.Next() {
		var v Incident
		if err := rows.Scan(&v.Key, &v.ChannelID, &v.Channel, &v.Count, &v.Firing, &v.UpdatedAt); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s *Store) IncidentNotifications(ctx context.Context, user User, channelID, key string) ([]Notification, error) {
	allowed, err := s.channelReadable(ctx, user, channelID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT n.id FROM notification_incidents i JOIN notifications n ON n.id=i.notification_id WHERE n.channel_id=? AND i.incident_key=? ORDER BY n.updated_at DESC,n.id DESC LIMIT 200`, channelID, key)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	values := []Notification{}
	for _, id := range ids {
		v, err := s.QueryNotifications(ctx, NotificationQuery{ID: id, UserID: user.ID, UserAdmin: user.IsAdmin, ShowDismissed: true})
		if err != nil {
			return nil, err
		}
		values = append(values, v...)
	}
	return values, nil
}

// AcknowledgeIncident atomically acknowledges the whole channel-scoped group. It never
// changes resolved notifications and records the human actor on every card.
func (s *Store) AcknowledgeIncident(ctx context.Context, user User, channelID, key string) error {
	if err := s.RequireChannelOperator(ctx, user, channelID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `UPDATE notifications SET state='acknowledged',updated_at=? WHERE channel_id=? AND state IN ('received','firing') AND id IN (SELECT notification_id FROM notification_incidents WHERE incident_key=?) RETURNING id`, time.Now().UnixMilli(), channelID, key)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		now := time.Now()
		eventID, err := newID("evt_", now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(id,notification_id,state,raw_payload_json,created_at,actor_user_id) VALUES(?,?,'acknowledged',?,?,?)`, eventID, id, []byte(`{}`), now.UnixMilli(), user.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RequireChannelOperator(ctx context.Context, user User, channelID string) error {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN channels c ON c.id=? WHERE u.id=? AND u.disabled_at IS NULL AND (u.is_admin=1 OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.user_id=u.id AND m.channel_id=c.id AND m.role IN ('operator','channel_admin'))))`, channelID, user.ID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
