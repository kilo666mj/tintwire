package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type ManagedMembership struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Role        string `json:"role"`
}

type ManagedUser struct {
	ID           string              `json:"id"`
	Username     string              `json:"username"`
	AuthType     string              `json:"auth_type"`
	IsAdmin      bool                `json:"is_admin"`
	DisabledAt   *time.Time          `json:"disabled_at,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	SessionCount int                 `json:"session_count"`
	Protected    bool                `json:"protected"`
	Memberships  []ManagedMembership `json:"memberships"`
}

func (s *Store) ListManagedUsers(ctx context.Context) ([]ManagedUser, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT u.id,u.username,u.is_admin,u.created_at,u.disabled_at,
CASE WHEN u.id=? THEN 'system' WHEN EXISTS(SELECT 1 FROM agents a WHERE a.user_id=u.id) THEN 'agent' WHEN u.oidc_subject IS NOT NULL THEN 'oidc' ELSE 'password' END,
(SELECT COUNT(*) FROM sessions s WHERE s.user_id=u.id AND s.expires_at>?)
FROM users u ORDER BY u.username COLLATE NOCASE`, localInboxUserID, time.Now().UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]ManagedUser, 0)
	for rows.Next() {
		var user ManagedUser
		var created int64
		var disabled sql.NullInt64
		if err := rows.Scan(&user.ID, &user.Username, &user.IsAdmin, &created, &disabled, &user.AuthType, &user.SessionCount); err != nil {
			return nil, err
		}
		user.CreatedAt = time.UnixMilli(created).UTC()
		if disabled.Valid {
			value := time.UnixMilli(disabled.Int64).UTC()
			user.DisabledAt = &value
		}
		user.Protected = user.AuthType == "system" || user.AuthType == "agent"
		user.Memberships = []ManagedMembership{}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range users {
		membershipRows, err := s.db.QueryContext(ctx, `SELECT c.id,c.display_name,m.role FROM channel_memberships m JOIN channels c ON c.id=m.channel_id WHERE m.user_id=? ORDER BY c.display_name COLLATE NOCASE`, users[i].ID)
		if err != nil {
			return nil, err
		}
		for membershipRows.Next() {
			var membership ManagedMembership
			if err := membershipRows.Scan(&membership.ChannelID, &membership.ChannelName, &membership.Role); err != nil {
				membershipRows.Close()
				return nil, err
			}
			users[i].Memberships = append(users[i].Memberships, membership)
		}
		if err := membershipRows.Close(); err != nil {
			return nil, err
		}
	}
	return users, nil
}

func (s *Store) SetManagedUserAdmin(ctx context.Context, actorID, userID string, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	protected, currentAdmin, disabled, err := managedUserState(ctx, tx, userID)
	if err != nil {
		return err
	}
	if protected {
		return ErrForbidden
	}
	if currentAdmin && !enabled {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u WHERE u.is_admin=1 AND u.disabled_at IS NULL AND u.id<>? AND u.id<>? AND NOT EXISTS(SELECT 1 FROM agents a WHERE a.user_id=u.id)`, userID, localInboxUserID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return errors.New("cannot remove the final enabled administrator")
		}
	}
	value := 0
	if enabled {
		value = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET is_admin=? WHERE id=?`, value, userID); err != nil {
		return err
	}
	if currentAdmin != enabled {
		if err := recordAdminAudit(ctx, tx, actorID, userID, "user.admin", map[bool]string{true: "enabled", false: "disabled"}[enabled]); err != nil {
			return err
		}
	}
	_ = disabled
	return tx.Commit()
}

func (s *Store) SetManagedUserDisabled(ctx context.Context, actorID, userID string, disabled bool) error {
	if actorID == userID && disabled {
		return errors.New("cannot disable your own account")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	protected, admin, currentlyDisabled, err := managedUserState(ctx, tx, userID)
	if err != nil {
		return err
	}
	if protected {
		return ErrForbidden
	}
	if admin && disabled && !currentlyDisabled {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u WHERE u.is_admin=1 AND u.disabled_at IS NULL AND u.id<>? AND u.id<>? AND NOT EXISTS(SELECT 1 FROM agents a WHERE a.user_id=u.id)`, userID, localInboxUserID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return errors.New("cannot disable the final enabled administrator")
		}
	}
	var value any
	if disabled {
		value = time.Now().UTC().UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at=? WHERE id=?`, value, userID); err != nil {
		return err
	}
	if disabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
			return err
		}
	}
	if currentlyDisabled != disabled {
		if err := recordAdminAudit(ctx, tx, actorID, userID, "user.access", map[bool]string{true: "disabled", false: "enabled"}[disabled]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RevokeManagedUserSessions(ctx context.Context, actorID, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	protected, _, _, err := managedUserState(ctx, tx, userID)
	if err != nil {
		return err
	}
	if protected {
		return ErrForbidden
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	if err := recordAdminAudit(ctx, tx, actorID, userID, "user.sessions", "revoked"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResetManagedUserPassword(ctx context.Context, actorID, userID, password string) error {
	if len(password) < 12 {
		return errors.New("password must contain at least 12 characters")
	}
	var authType string
	err := s.db.QueryRowContext(ctx, `SELECT CASE WHEN id=? THEN 'system' WHEN EXISTS(SELECT 1 FROM agents a WHERE a.user_id=users.id) THEN 'agent' WHEN oidc_subject IS NOT NULL THEN 'oidc' ELSE 'password' END FROM users WHERE id=?`, localInboxUserID, userID).Scan(&authType)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return err
	}
	if authType != "password" {
		return errors.New("only local-user passwords can be reset")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	if err := recordAdminAudit(ctx, tx, actorID, userID, "user.password", "reset"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveChannelMember(ctx context.Context, actorID, channelID, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_memberships WHERE channel_id=? AND user_id=?`, channelID, userID); err != nil {
		return err
	}
	if err := recordAdminAudit(ctx, tx, actorID, userID, "user.membership", channelID+":none"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetChannelMemberByID(ctx context.Context, actorID, channelID, userID, role string) error {
	if role != "viewer" && role != "operator" && role != "channel_admin" {
		return errors.New("invalid channel role")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO channel_memberships(user_id,channel_id,role,created_at) SELECT u.id,c.id,?,? FROM users u CROSS JOIN channels c WHERE u.id=? AND c.id=? ON CONFLICT(user_id,channel_id) DO UPDATE SET role=excluded.role`, role, time.Now().UTC().UnixMilli(), userID, channelID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrInvalidCredentials
	}
	if err := recordAdminAudit(ctx, tx, actorID, userID, "user.membership", channelID+":"+role); err != nil {
		return err
	}
	return tx.Commit()
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordAdminAudit(ctx context.Context, db sqlExecutor, actorID, targetID, action, detail string) error {
	now := time.Now().UTC()
	id, err := newID("aud_", now)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO admin_audit_events(id,actor_user_id,target_user_id,action,detail,created_at) VALUES(?,?,?,?,?,?)`, id, actorID, targetID, action, detail, now.UnixMilli())
	return err
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func managedUserState(ctx context.Context, db rowQuerier, userID string) (bool, bool, bool, error) {
	var admin int
	var disabled sql.NullInt64
	var protected int
	err := db.QueryRowContext(ctx, `SELECT is_admin,disabled_at,CASE WHEN id=? OR EXISTS(SELECT 1 FROM agents a WHERE a.user_id=users.id) THEN 1 ELSE 0 END FROM users WHERE id=?`, localInboxUserID, userID).Scan(&admin, &disabled, &protected)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false, ErrInvalidCredentials
	}
	return protected != 0, admin != 0, disabled.Valid, err
}
