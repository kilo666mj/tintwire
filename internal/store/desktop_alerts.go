package store

import "context"

type DesktopArrival struct {
	ID   string
	Kind string
	At   int64
}

// DesktopArrivals returns a feed independent of the open channel, saved view and read filters.
// It also works across app nodes because it reads the authoritative database.
func (s *Store) DesktopArrivals(ctx context.Context, user User, after int64, afterID string) ([]DesktopArrival, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,at FROM (
 SELECT n.id AS id,'notification' AS kind,n.updated_at AS at,c.id AS channel_id,c.visibility AS visibility FROM notifications n JOIN channels c ON c.id=n.channel_id
 UNION ALL
 SELECT m.id,'message',m.created_at,c.id,c.visibility FROM channel_messages m JOIN channels c ON c.id=m.channel_id WHERE m.deleted_at IS NULL AND m.author_user_id<>?
 UNION ALL
 SELECT a.id,'attention',a.created_at,'','public' FROM attention_alerts a WHERE a.user_id=?
 ) arrivals WHERE (at>? OR (at=? AND id>?)) AND (? OR visibility='public' OR EXISTS(SELECT 1 FROM channel_memberships cm WHERE cm.channel_id=arrivals.channel_id AND cm.user_id=?)) ORDER BY at,id LIMIT 200`, user.ID, user.ID, after, after, afterID, user.IsAdmin, user.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []DesktopArrival{}
	for rows.Next() {
		var v DesktopArrival
		if err := rows.Scan(&v.ID, &v.Kind, &v.At); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}
