package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

func (s *Store) ListSavedViews(ctx context.Context, userID string) ([]SavedView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,definition_json FROM saved_views WHERE user_id=? ORDER BY name COLLATE NOCASE,id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := []SavedView{}
	for rows.Next() {
		var view SavedView
		var raw []byte
		if err := rows.Scan(&view.ID, &view.Name, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (s *Store) SaveSavedView(ctx context.Context, userID string, view SavedView) (SavedView, error) {
	view.Name = strings.TrimSpace(view.Name)
	if view.ID == "" {
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM saved_views WHERE user_id=? AND name=?`, userID, view.Name).Scan(&view.ID); err != nil && err != sql.ErrNoRows {
			return SavedView{}, err
		}
		if view.ID == "" {
			id, err := newID("viw_", time.Now().UTC())
			if err != nil {
				return SavedView{}, err
			}
			view.ID = id
		}
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return SavedView{}, err
	}
	now := time.Now().UTC().UnixMilli()
	_, err = s.db.ExecContext(ctx, `INSERT INTO saved_views(id,user_id,name,definition_json,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(user_id,name) DO UPDATE SET definition_json=excluded.definition_json,updated_at=excluded.updated_at`, view.ID, userID, view.Name, raw, now, now)
	if IsAlreadyExists(err) {
		return SavedView{}, ErrAlreadyExists
	}
	return view, err
}

func (s *Store) DeleteSavedView(ctx context.Context, userID, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM saved_views WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return sql.ErrNoRows
	}
	return err
}
