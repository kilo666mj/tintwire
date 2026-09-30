package store

import (
	"context"
	"time"
)

func (s *Store) CreatePlaygroundNotification(ctx context.Context, user User, channelID string, n IncomingNotification) (Notification, error) {
	if !user.IsAdmin {
		return Notification{}, ErrForbidden
	}
	if err := s.RequireChannelOperator(ctx, user, channelID); err != nil {
		return Notification{}, err
	}
	channel, err := s.ChannelNameByID(ctx, channelID)
	if err != nil {
		return Notification{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	hook, err := agentWebhook(ctx, tx, channelID, now)
	if err != nil {
		return Notification{}, err
	}
	id, err := newID("ntf_", now)
	if err != nil {
		return Notification{}, err
	}
	if len(n.Attachments) == 0 {
		n.Attachments = []byte(`[]`)
	}
	if len(n.RawPayload) == 0 {
		n.RawPayload = []byte(`{}`)
	}
	if n.State == "" {
		n.State = "received"
	}
	card := []byte(n.Card)
	if card == nil {
		card = []byte{}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,channel_id,webhook_id,text,username,icon_url,attachments_json,raw_payload_json,created_at,updated_at,state,card_json) VALUES(?,?,?,?,?,'',?,?,?,?,?,?)`, id, channelID, hook, n.Text, n.Username, []byte(n.Attachments), []byte(n.RawPayload), now.UnixMilli(), now.UnixMilli(), n.State, card); err != nil {
		return Notification{}, err
	}
	if err := insertNotificationEvent(ctx, tx, id, n.State, n.RawPayload, now); err != nil {
		return Notification{}, err
	}
	if err := saveIncidentKey(ctx, tx, id, n); err != nil {
		return Notification{}, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, err
	}
	return Notification{ID: id, ChannelID: channelID, ChannelName: channel, Text: n.Text, Username: n.Username, Attachments: n.Attachments, Card: n.Card, State: n.State, CreatedAt: now, UpdatedAt: now}, nil
}
