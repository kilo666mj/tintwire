package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// NotificationCardForReader applies the same visibility rules as the inbox.
func (s *Store) NotificationCardForReader(ctx context.Context, id string, user User) (json.RawMessage, error) {
	if user.ID == "" {
		return nil, ErrNotificationNotFound
	}
	var card json.RawMessage
	err := s.db.QueryRowContext(ctx, `SELECT CAST(n.card_json AS BLOB)
		FROM notifications n JOIN channels c ON c.id=n.channel_id
		WHERE n.id=? AND (? OR c.visibility='public' OR EXISTS (
			SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?
		))`, id, user.IsAdmin, user.ID).Scan(&card)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotificationNotFound
	}
	return card, err
}
