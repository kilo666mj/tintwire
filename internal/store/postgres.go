package store

import (
	"database/sql"
	"fmt"
)

const postgresSchemaVersion = 27

func init() {
	sql.Register("tintwire-postgres", newPostgresDriver())
}

func OpenPostgres(dsn string) (*Store, error) {
	db, err := sql.Open("tintwire-postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL database: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect PostgreSQL database: %w", err)
	}
	if _, err := db.Exec(postgresSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize PostgreSQL database: %w", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version WHERE singleton=1`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read PostgreSQL schema version: %w", err)
	}
	if version == 24 {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled_at BIGINT;
CREATE TABLE IF NOT EXISTS admin_audit_events (id TEXT PRIMARY KEY, actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL, target_user_id TEXT REFERENCES users(id) ON DELETE SET NULL, action TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS admin_audit_events_created_idx ON admin_audit_events(created_at DESC,id DESC);
UPDATE schema_version SET version=25 WHERE singleton=1`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate PostgreSQL schema to version 25: %w", err)
		}
		version = 25
	}
	if version == 25 {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS channel_messages (
  id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  parent_id TEXT REFERENCES channel_messages(id) ON DELETE CASCADE, root_id TEXT NOT NULL,
  text TEXT NOT NULL, idempotency_key TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL, deleted_at BIGINT
);
CREATE INDEX IF NOT EXISTS channel_messages_channel_idx ON channel_messages(channel_id,created_at,id);
CREATE INDEX IF NOT EXISTS channel_messages_root_idx ON channel_messages(root_id,created_at,id);
CREATE UNIQUE INDEX IF NOT EXISTS channel_messages_idempotency_idx ON channel_messages(channel_id,author_user_id,idempotency_key) WHERE idempotency_key <> '';
UPDATE schema_version SET version=26 WHERE singleton=1`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate PostgreSQL schema to version 26: %w", err)
		}
		version = 26
	}
	if version == 26 {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS saved_views (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, name TEXT NOT NULL, definition_json BYTEA NOT NULL, created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL, UNIQUE(user_id,name)); UPDATE schema_version SET version=27 WHERE singleton=1`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate PostgreSQL schema to version 27: %w", err)
		}
		version = 27
	}
	if version != postgresSchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("unsupported PostgreSQL schema version %d", version)
	}
	return &Store{db: db, postgres: true}, nil
}

const postgresSchema = `
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE IF NOT EXISTS schema_version (
    singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
    version INTEGER NOT NULL
);
INSERT INTO schema_version(singleton,version) VALUES(1,27)
ON CONFLICT(singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY, username CITEXT NOT NULL UNIQUE, password_hash BYTEA NOT NULL,
    created_at BIGINT NOT NULL, is_admin INTEGER NOT NULL DEFAULT 0, oidc_subject TEXT, disabled_at BIGINT
);
CREATE TABLE IF NOT EXISTS saved_views (
    id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL, definition_json BYTEA NOT NULL, created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL, UNIQUE(user_id,name)
);
CREATE UNIQUE INDEX IF NOT EXISTS users_oidc_subject_idx ON users(oidc_subject) WHERE oidc_subject IS NOT NULL;
CREATE TABLE IF NOT EXISTS admin_audit_events (
    id TEXT PRIMARY KEY, actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    target_user_id TEXT REFERENCES users(id) ON DELETE SET NULL, action TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS admin_audit_events_created_idx ON admin_audit_events(created_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS channels (
    id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at BIGINT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
    accent_color TEXT NOT NULL DEFAULT '', visibility TEXT NOT NULL DEFAULT 'public'
);
CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '', owner_user_id TEXT NOT NULL REFERENCES users(id),
    user_id TEXT NOT NULL UNIQUE REFERENCES users(id), enabled INTEGER NOT NULL DEFAULT 1,
    created_at BIGINT NOT NULL, revoked_at BIGINT, oauth_subject TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS agents_oauth_subject_idx ON agents(oauth_subject) WHERE oauth_subject IS NOT NULL;
CREATE TABLE IF NOT EXISTS agent_runs (
    id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    initiator_user_id TEXT REFERENCES users(id), purpose TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('running','completed','failed','cancelled')),
    created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_runs_agent_idx ON agent_runs(agent_id,created_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS webhooks (
    id TEXT PRIMARY KEY, token_hash BYTEA NOT NULL UNIQUE, channel_id TEXT NOT NULL REFERENCES channels(id),
    created_at BIGINT NOT NULL, kind TEXT NOT NULL DEFAULT 'incoming', revoked_at BIGINT
);
CREATE TABLE IF NOT EXISTS notifications (
    id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id),
    webhook_id TEXT NOT NULL REFERENCES webhooks(id), text TEXT NOT NULL, username TEXT NOT NULL,
    icon_url TEXT NOT NULL, attachments_json BYTEA NOT NULL, raw_payload_json BYTEA NOT NULL,
    created_at BIGINT NOT NULL, external_key TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'received',
    updated_at BIGINT NOT NULL DEFAULT 0, card_json BYTEA NOT NULL DEFAULT decode('', 'hex'),
    agent_id TEXT REFERENCES agents(id), agent_run_id TEXT REFERENCES agent_runs(id)
);
CREATE INDEX IF NOT EXISTS notifications_created_at_idx ON notifications(created_at DESC,id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS notification_external_key_idx ON notifications(webhook_id,external_key) WHERE external_key <> '';
CREATE TABLE IF NOT EXISTS notification_events (
    id TEXT PRIMARY KEY, notification_id TEXT NOT NULL REFERENCES notifications(id), state TEXT NOT NULL,
    raw_payload_json BYTEA NOT NULL, created_at BIGINT NOT NULL,
    actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS notification_events_notification_idx ON notification_events(notification_id,created_at,id);
CREATE TABLE IF NOT EXISTS push_subscriptions (
    endpoint TEXT PRIMARY KEY, p256dh TEXT NOT NULL, auth TEXT NOT NULL, created_at BIGINT NOT NULL,
    user_id TEXT REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS push_subscriptions_user_idx ON push_subscriptions(user_id);
CREATE TABLE IF NOT EXISTS sessions (
    token_hash BYTEA PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS oidc_login_states (
    state_hash BYTEA PRIMARY KEY, verifier TEXT NOT NULL, nonce TEXT NOT NULL, expires_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS oidc_login_states_expiry_idx ON oidc_login_states(expires_at);
CREATE TABLE IF NOT EXISTS channel_read_state (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE, read_at BIGINT NOT NULL,
    PRIMARY KEY(user_id,channel_id)
);
CREATE TABLE IF NOT EXISTS channel_memberships (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK(role IN ('viewer','operator','channel_admin')), created_at BIGINT NOT NULL,
    PRIMARY KEY(user_id,channel_id)
);
CREATE TABLE IF NOT EXISTS action_targets (
    id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, url TEXT NOT NULL, auth_cipher BYTEA NOT NULL,
    allow_private INTEGER NOT NULL DEFAULT 0, created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS action_executions (
    operation_key TEXT PRIMARY KEY, notification_id TEXT NOT NULL REFERENCES notifications(id),
    action_index INTEGER NOT NULL, user_id TEXT NOT NULL REFERENCES users(id), status TEXT NOT NULL,
    response_text TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL, completed_at BIGINT
);
CREATE TABLE IF NOT EXISTS mattermost_bot_tokens (
    token_hash BYTEA PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE, team_name TEXT NOT NULL,
    created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS mattermost_channel_aliases (
    team_name TEXT NOT NULL, channel_name TEXT NOT NULL,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    PRIMARY KEY(team_name,channel_name)
);
CREATE TABLE IF NOT EXISTS mattermost_posts (
    id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, message TEXT NOT NULL, root_id TEXT NOT NULL,
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    props_json BYTEA NOT NULL, created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS mattermost_posts_channel_time_idx ON mattermost_posts(channel_id,created_at,id);
CREATE TABLE IF NOT EXISTS mattermost_reactions (
    post_id TEXT NOT NULL REFERENCES mattermost_posts(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, emoji_name TEXT NOT NULL,
    created_at BIGINT NOT NULL, PRIMARY KEY(post_id,user_id,emoji_name)
);
CREATE TABLE IF NOT EXISTS slash_commands (
    id TEXT PRIMARY KEY, team_name TEXT NOT NULL, trigger_word TEXT NOT NULL,
    display_name TEXT NOT NULL, description TEXT NOT NULL, creator TEXT NOT NULL,
    method TEXT NOT NULL CHECK(method IN ('GET','POST')), url TEXT NOT NULL,
    token_cipher BYTEA NOT NULL, token_hash BYTEA NOT NULL, allow_private INTEGER NOT NULL DEFAULT 0,
    autocomplete INTEGER NOT NULL DEFAULT 0, autocomplete_hint TEXT NOT NULL DEFAULT '',
    autocomplete_description TEXT NOT NULL DEFAULT '', username TEXT NOT NULL DEFAULT '',
    icon_url TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL, UNIQUE(team_name,trigger_word)
);
CREATE TABLE IF NOT EXISTS slash_command_executions (
    id TEXT PRIMARY KEY, command_id TEXT NOT NULL REFERENCES slash_commands(id),
    channel_id TEXT NOT NULL REFERENCES channels(id), user_id TEXT NOT NULL REFERENCES users(id),
    text TEXT NOT NULL, response_token_hash BYTEA NOT NULL UNIQUE, request_key TEXT NOT NULL UNIQUE,
    expires_at BIGINT NOT NULL, response_count INTEGER NOT NULL DEFAULT 0, created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS slash_command_responses (
    id TEXT PRIMARY KEY, execution_id TEXT NOT NULL REFERENCES slash_command_executions(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id),
    response_type TEXT NOT NULL CHECK(response_type IN ('ephemeral','in_channel')),
    text TEXT NOT NULL, payload_json BYTEA NOT NULL, created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS slash_command_responses_execution_idx ON slash_command_responses(execution_id,created_at,id);
CREATE TABLE IF NOT EXISTS notification_user_state (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    read_at BIGINT NOT NULL DEFAULT 0, unread INTEGER NOT NULL DEFAULT 0, dismissed_at BIGINT,
    PRIMARY KEY(user_id,notification_id)
);
CREATE INDEX IF NOT EXISTS notification_user_state_dismissed_idx ON notification_user_state(user_id,dismissed_at);
CREATE TABLE IF NOT EXISTS agent_credentials (
    id TEXT PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE, created_at BIGINT NOT NULL, last_used_at BIGINT, revoked_at BIGINT
);
CREATE TABLE IF NOT EXISTS agent_run_events (
    id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    tool TEXT NOT NULL, summary TEXT NOT NULL,
    notification_id TEXT REFERENCES notifications(id) ON DELETE SET NULL, created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_run_events_run_idx ON agent_run_events(run_id,created_at,id);
CREATE TABLE IF NOT EXISTS agent_tool_invocations (
    agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE, idempotency_key TEXT NOT NULL,
    tool TEXT NOT NULL, request_fingerprint BYTEA NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('running','completed')), result_json BYTEA NOT NULL,
    created_at BIGINT NOT NULL, PRIMARY KEY(agent_id,idempotency_key)
);
CREATE TABLE IF NOT EXISTS channel_notification_preferences (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    level TEXT NOT NULL CHECK(level IN ('all','critical','muted')), updated_at BIGINT NOT NULL,
    PRIMARY KEY(user_id,channel_id)
);
CREATE TABLE IF NOT EXISTS replication_operations (
    cluster_id TEXT NOT NULL, origin TEXT NOT NULL, sequence BIGINT NOT NULL CHECK(sequence > 0),
    physical_ms BIGINT NOT NULL CHECK(physical_ms > 0), logical BIGINT NOT NULL CHECK(logical >= 0),
    kind TEXT NOT NULL, channel_id TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL,
    actor_id TEXT NOT NULL, payload_json BYTEA NOT NULL, created_at BIGINT NOT NULL,
    PRIMARY KEY(origin,sequence)
);
CREATE INDEX IF NOT EXISTS replication_operations_created_idx ON replication_operations(created_at,origin,sequence);
CREATE TABLE IF NOT EXISTS replication_cursors (origin TEXT PRIMARY KEY, sequence BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS replication_quarantine (
    origin TEXT NOT NULL, sequence BIGINT NOT NULL, reason TEXT NOT NULL,
    envelope_json BYTEA NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY(origin,sequence)
);
CREATE TABLE IF NOT EXISTS replication_peer_status (
    peer TEXT PRIMARY KEY, node_id TEXT NOT NULL DEFAULT '', last_attempt_at BIGINT NOT NULL,
    last_success_at BIGINT, consecutive_failures INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS replication_snapshot_status (
    source TEXT PRIMARY KEY, last_applied_at BIGINT NOT NULL, application_count BIGINT NOT NULL,
    notification_count BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_messages (
    id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id TEXT REFERENCES channel_messages(id) ON DELETE CASCADE, root_id TEXT NOT NULL,
    text TEXT NOT NULL, idempotency_key TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL, deleted_at BIGINT
);
CREATE INDEX IF NOT EXISTS channel_messages_channel_idx ON channel_messages(channel_id,created_at,id);
CREATE INDEX IF NOT EXISTS channel_messages_root_idx ON channel_messages(root_id,created_at,id);
CREATE UNIQUE INDEX IF NOT EXISTS channel_messages_idempotency_idx ON channel_messages(channel_id,author_user_id,idempotency_key) WHERE idempotency_key <> '';
INSERT INTO users(id,username,password_hash,created_at,is_admin)
VALUES('usr_tintwire_local_inbox','tintwire-local-inbox',decode('', 'hex'),0,1)
ON CONFLICT(id) DO NOTHING;
`
