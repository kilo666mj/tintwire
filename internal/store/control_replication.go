package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const controlSnapshotVersion = 1

type controlTableDefinition struct {
	Name          string
	Columns       []string
	LegacyColumns int
	Replace       bool
}

var controlTableDefinitions = []controlTableDefinition{
	// Tables without Replace must use soft deletion. A future hard-delete path
	// must first add safe snapshot deletion semantics; upserts alone cannot infer
	// which rows disappeared. Production PostgreSQL disables this legacy path.
	{Name: "app_settings", Columns: []string{"key", "value"}},
	{Name: "users", Columns: []string{"id", "username", "password_hash", "created_at", "is_admin", "oidc_subject", "disabled_at"}, LegacyColumns: 6},
	{Name: "admin_audit_events", Columns: []string{"id", "actor_user_id", "target_user_id", "action", "detail", "created_at"}},
	{Name: "channels", Columns: []string{"id", "name", "created_at", "display_name", "description", "accent_color", "visibility"}},
	{Name: "webhooks", Columns: []string{"id", "token_hash", "channel_id", "created_at", "kind", "revoked_at"}, LegacyColumns: 5},
	{Name: "agents", Columns: []string{"id", "name", "display_name", "description", "owner_user_id", "user_id", "enabled", "created_at", "revoked_at", "oauth_subject"}},
	{Name: "agent_credentials", Columns: []string{"id", "agent_id", "token_hash", "created_at", "last_used_at", "revoked_at"}},
	{Name: "action_targets", Columns: []string{"id", "name", "url", "auth_cipher", "allow_private", "created_at"}, Replace: true},
	{Name: "mattermost_channel_aliases", Columns: []string{"team_name", "channel_name", "channel_id"}},
	{Name: "mattermost_bot_tokens", Columns: []string{"token_hash", "user_id", "channel_id", "team_name", "created_at"}},
	{Name: "slash_commands", Columns: []string{"id", "team_name", "trigger_word", "display_name", "description", "creator", "method", "url", "token_cipher", "token_hash", "allow_private", "autocomplete", "autocomplete_hint", "autocomplete_description", "username", "icon_url", "created_at"}},
	{Name: "sessions", Columns: []string{"token_hash", "user_id", "created_at", "expires_at"}, Replace: true},
	{Name: "oidc_login_states", Columns: []string{"state_hash", "verifier", "nonce", "expires_at"}, Replace: true},
	{Name: "push_subscriptions", Columns: []string{"endpoint", "p256dh", "auth", "created_at", "user_id"}, Replace: true},
	{Name: "channel_memberships", Columns: []string{"user_id", "channel_id", "role", "created_at"}, Replace: true},
	{Name: "channel_read_state", Columns: []string{"user_id", "channel_id", "read_at"}, Replace: true},
	{Name: "notification_user_state", Columns: []string{"user_id", "notification_id", "read_at", "unread", "dismissed_at"}, Replace: true},
	{Name: "channel_notification_preferences", Columns: []string{"user_id", "channel_id", "level", "updated_at"}, Replace: true},
}

type ControlSnapshotValue struct {
	Text    *string `json:"text,omitempty"`
	Integer *int64  `json:"integer,omitempty"`
	Blob    string  `json:"blob,omitempty"`
	Null    bool    `json:"null,omitempty"`
}

type ControlSnapshotTable struct {
	Name string                   `json:"name"`
	Rows [][]ControlSnapshotValue `json:"rows"`
}

type ControlSnapshot struct {
	Version   int                    `json:"version"`
	ClusterID string                 `json:"cluster_id"`
	Authority string                 `json:"authority"`
	CreatedAt time.Time              `json:"created_at"`
	Tables    []ControlSnapshotTable `json:"tables"`
	Digest    string                 `json:"digest"`
}

func (s *Store) ConfigureControlPlane(authority string, lease time.Duration) error {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return nil
	}
	if s.replicationClusterID == "" || !replicationIdentityPattern.MatchString(authority) {
		return errors.New("control authority requires valid replication and authority node IDs")
	}
	if lease < 10*time.Second || lease > 10*time.Minute {
		return errors.New("control lease must be between 10 seconds and 10 minutes")
	}
	s.controlAuthority = authority
	s.controlLease = lease
	return nil
}

func (s *Store) IsControlAuthority() bool {
	return s.controlAuthority != "" && s.replicationNodeID == s.controlAuthority
}

func (s *Store) ControlAuthority() string { return s.controlAuthority }

func (s *Store) CanMutateControlPlane() bool {
	return s.controlAuthority == "" || s.IsControlAuthority()
}

func (s *Store) ControlLeaseValid(ctx context.Context) (bool, error) {
	if s.controlAuthority == "" || s.IsControlAuthority() {
		return true, nil
	}
	value, ok, err := s.Setting(ctx, "replication_control_last_success_ms")
	if err != nil || !ok {
		return false, err
	}
	var millis int64
	if _, err := fmt.Sscan(value, &millis); err != nil {
		return false, err
	}
	return time.Since(time.UnixMilli(millis)) <= s.controlLease, nil
}

func (s *Store) BuildControlSnapshot(ctx context.Context) (ControlSnapshot, error) {
	if !s.IsControlAuthority() {
		return ControlSnapshot{}, ErrForbidden
	}
	return s.ExportControlSnapshot(ctx, s.replicationNodeID)
}

// ExportControlSnapshot builds a normalized control state for a consensus
// proposal or snapshot. The caller is responsible for proving leadership;
// ordinary replication callers must use BuildControlSnapshot instead.
func (s *Store) ExportControlSnapshot(ctx context.Context, authority string) (ControlSnapshot, error) {
	if !replicationIdentityPattern.MatchString(authority) {
		return ControlSnapshot{}, errors.New("invalid control snapshot authority")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ControlSnapshot{}, err
	}
	defer tx.Rollback()
	snapshot := ControlSnapshot{Version: controlSnapshotVersion, ClusterID: s.replicationClusterID, Authority: authority, CreatedAt: time.Now().UTC()}
	for _, definition := range controlTableDefinitions {
		rows, err := tx.QueryContext(ctx, `SELECT `+strings.Join(definition.Columns, ",")+` FROM `+definition.Name)
		if err != nil {
			return ControlSnapshot{}, err
		}
		table := ControlSnapshotTable{Name: definition.Name, Rows: make([][]ControlSnapshotValue, 0)}
		for rows.Next() {
			values := make([]any, len(definition.Columns))
			destinations := make([]any, len(values))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				rows.Close()
				return ControlSnapshot{}, err
			}
			encoded := make([]ControlSnapshotValue, len(values))
			for i, value := range values {
				switch value := value.(type) {
				case nil:
					encoded[i].Null = true
				case int64:
					encoded[i].Integer = &value
				case string:
					encoded[i].Text = &value
				case []byte:
					encoded[i].Blob = hex.EncodeToString(value)
				default:
					rows.Close()
					return ControlSnapshot{}, fmt.Errorf("unsupported %s snapshot value %T", definition.Name, value)
				}
			}
			table.Rows = append(table.Rows, encoded)
		}
		if err := rows.Close(); err != nil {
			return ControlSnapshot{}, err
		}
		snapshot.Tables = append(snapshot.Tables, table)
	}
	if err := tx.Commit(); err != nil {
		return ControlSnapshot{}, err
	}
	digest, err := controlSnapshotDigest(snapshot)
	if err != nil {
		return ControlSnapshot{}, err
	}
	snapshot.Digest = digest
	return snapshot, nil
}

func (s *Store) ApplyControlSnapshot(ctx context.Context, snapshot ControlSnapshot) error {
	if s.controlAuthority == "" || s.IsControlAuthority() || snapshot.Version != controlSnapshotVersion || snapshot.ClusterID != s.replicationClusterID || snapshot.Authority != s.controlAuthority {
		return errors.New("invalid control snapshot authority")
	}
	return s.applyControlSnapshot(ctx, snapshot, false)
}

// ApplyCommittedControlSnapshot applies state that has already been committed
// by the Raft FSM. Raft authenticates and orders the leader; this method still
// validates the cluster, encoding, digest, and timestamp before touching SQLite.
func (s *Store) ApplyCommittedControlSnapshot(ctx context.Context, snapshot ControlSnapshot) error {
	if snapshot.Version != controlSnapshotVersion || snapshot.ClusterID != s.replicationClusterID || !replicationIdentityPattern.MatchString(snapshot.Authority) {
		return errors.New("invalid committed control snapshot")
	}
	return s.applyControlSnapshot(ctx, snapshot, true)
}

func (s *Store) applyControlSnapshot(ctx context.Context, snapshot ControlSnapshot, committed bool) error {
	digest := snapshot.Digest
	snapshot.Digest = ""
	want, err := controlSnapshotDigest(snapshot)
	if err != nil || digest == "" || digest != want {
		return errors.New("invalid control snapshot digest")
	}
	if snapshot.CreatedAt.After(time.Now().UTC().Add(2 * time.Minute)) {
		return errors.New("control snapshot timestamp is in the future")
	}
	byName := make(map[string]ControlSnapshotTable, len(snapshot.Tables))
	for _, table := range snapshot.Tables {
		if _, exists := byName[table.Name]; exists {
			return errors.New("duplicate control snapshot table")
		}
		byName[table.Name] = table
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := len(controlTableDefinitions) - 1; i >= 0; i-- {
		definition := controlTableDefinitions[i]
		if definition.Replace {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+definition.Name); err != nil {
				return err
			}
		}
	}
	for _, definition := range controlTableDefinitions {
		table, ok := byName[definition.Name]
		if !ok {
			return fmt.Errorf("control snapshot missing table %s", definition.Name)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(definition.Columns)), ",")
		assignments := make([]string, len(definition.Columns))
		for i, column := range definition.Columns {
			assignments[i] = column + "=excluded." + column
		}
		query := `INSERT INTO ` + definition.Name + `(` + strings.Join(definition.Columns, ",") + `) VALUES(` + placeholders + `) ON CONFLICT DO UPDATE SET ` + strings.Join(assignments, ",")
		for _, row := range table.Rows {
			if len(row) != len(definition.Columns) && (definition.LegacyColumns == 0 || len(row) != definition.LegacyColumns) {
				return fmt.Errorf("invalid %s snapshot row", definition.Name)
			}
			values := make([]any, len(definition.Columns))
			for i := len(row); i < len(values); i++ {
				values[i] = nil
			}
			for i, value := range row {
				switch {
				case value.Null:
					values[i] = nil
				case value.Integer != nil:
					values[i] = *value.Integer
				case value.Text != nil:
					values[i] = *value.Text
				case value.Blob != "":
					decoded, err := hex.DecodeString(value.Blob)
					if err != nil {
						return err
					}
					values[i] = decoded
				default:
					values[i] = []byte{}
				}
			}
			if _, err := tx.ExecContext(ctx, query, values...); err != nil {
				return fmt.Errorf("apply %s control snapshot: %w", definition.Name, err)
			}
		}
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES('replication_control_last_success_ms',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(now)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if committed {
		s.controlAuthority = snapshot.Authority
	}
	return nil
}

func controlSnapshotDigest(snapshot ControlSnapshot) (string, error) {
	snapshot.Digest = ""
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
