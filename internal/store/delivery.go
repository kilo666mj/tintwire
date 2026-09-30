package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type DeliveryEvent struct {
	Device    string    `json:"device"`
	Outcome   string    `json:"outcome"`
	Attempt   int       `json:"attempt"`
	CreatedAt time.Time `json:"created_at"`
}

// RecordDelivery keeps an opaque device fingerprint and fixed outcome.
// Endpoints and provider response bodies can contain credentials. Only an
// opaque device fingerprint and a fixed outcome vocabulary are persisted.
func (s *Store) RecordDelivery(ctx context.Context, id, userID, endpoint, outcome string, attempt int) error {
	now := time.Now()
	eventID, err := newID("dlv_", now)
	if err != nil {
		return err
	}
	device := ""
	if endpoint != "" {
		sum := sha256.Sum256([]byte(endpoint))
		device = hex.EncodeToString(sum[:6])
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO delivery_events(id,notification_id,user_id,device,outcome,attempt,created_at) VALUES(?,?,?,?,?,?,?)`, eventID, id, userID, device, outcome, attempt, now.UnixMilli())
	return err
}

func (s *Store) DeliveryEvents(ctx context.Context, id string, user User) ([]DeliveryEvent, error) {
	allowed, err := s.CanReadNotification(ctx, id, user)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT device,outcome,attempt,created_at FROM delivery_events WHERE notification_id=? AND user_id=? ORDER BY created_at DESC,id DESC LIMIT 100`, id, user.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []DeliveryEvent{}
	for rows.Next() {
		var v DeliveryEvent
		var at int64
		if err := rows.Scan(&v.Device, &v.Outcome, &v.Attempt, &at); err != nil {
			return nil, err
		}
		v.CreatedAt = time.UnixMilli(at).UTC()
		events = append(events, v)
	}
	return events, rows.Err()
}

func (s *Store) SubscribedUsers(ctx context.Context, n Notification) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT u.id,u.username,u.is_admin FROM users u JOIN push_subscriptions p ON p.user_id=u.id JOIN channels c ON c.id=? WHERE u.disabled_at IS NULL AND (u.is_admin=1 OR c.visibility='public' OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.user_id=u.id AND m.channel_id=c.id))`, n.ChannelID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	users := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) UserSubscriptions(ctx context.Context, userID string) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.endpoint,p.p256dh,p.auth FROM push_subscriptions p JOIN users u ON u.id=p.user_id WHERE p.user_id=? AND u.disabled_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []PushSubscription{}
	for rows.Next() {
		v := PushSubscription{UserID: userID}
		if err := rows.Scan(&v.Endpoint, &v.P256DH, &v.Auth); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}
