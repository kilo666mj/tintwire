package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var (
	ErrWebhookNotFound      = errors.New("webhook not found")
	ErrWebhookChannelDenied = errors.New("webhook channel override not allowed")
	ErrChannelNotFound      = errors.New("channel not found")
	ErrNotificationNotFound = errors.New("notification not found")
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrImportConflict       = errors.New("import conflict")
	ErrForbidden            = errors.New("forbidden")
	ErrInvalidTransition    = errors.New("invalid state transition")
	ErrAlreadyExists        = errors.New("record already exists")
)

func IsAlreadyExists(err error) bool {
	if errors.Is(err, ErrAlreadyExists) || (err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")) {
		return true
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

const localInboxUserID = "usr_tintwire_local_inbox"

const webhookChannelPolicyPrefix = "webhook_channel_policy:"

func LocalInboxUser() User {
	return User{ID: localInboxUserID, Username: "tintwire-local-inbox", IsAdmin: true}
}

// Generated once at startup so unknown-user authentication performs the same
// expensive password check as a wrong password for an existing user.
var dummyPasswordHash, _ = bcrypt.GenerateFromPassword([]byte("tintwire-invalid-login-placeholder"), bcrypt.DefaultCost)

type Store struct {
	db                   *sql.DB
	postgres             bool
	replicationClusterID string
	replicationNodeID    string
	controlAuthority     string
	controlLease         time.Duration
}

type IncomingNotification struct {
	Channel     string
	Text        string
	Username    string
	IconURL     string
	Attachments json.RawMessage
	RawPayload  json.RawMessage
	ExternalKey string
	State       string
	Card        json.RawMessage
}

type Notification struct {
	ID          string          `json:"id"`
	ChannelID   string          `json:"channel_id"`
	ChannelName string          `json:"channel_name"`
	Text        string          `json:"text"`
	Username    string          `json:"username"`
	IconURL     string          `json:"icon_url,omitempty"`
	Attachments json.RawMessage `json:"attachments"`
	State       string          `json:"state"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	EventCount  int             `json:"event_count"`
	Card        json.RawMessage `json:"card,omitempty"`
	Agent       string          `json:"agent,omitempty"`
	Unread      bool            `json:"unread,omitempty"`
	CanOperate  bool            `json:"can_operate,omitempty"`
	CanApprove  bool            `json:"can_approve,omitempty"`
	CompatRoot  bool            `json:"-"`
}

type MattermostActionResult struct {
	Status       string    `json:"status"`
	ResponseText string    `json:"response"`
	Actor        string    `json:"actor,omitempty"`
	ActionIndex  int       `json:"action_index"`
	CompletedAt  time.Time `json:"completed_at"`
}

type NotificationEvent struct {
	ID         string
	State      string
	RawPayload json.RawMessage
	CreatedAt  time.Time
	Actor      string
}

type PushSubscription struct {
	UserID    string
	Endpoint  string
	P256DH    string
	Auth      string
	CreatedAt time.Time
}

type User struct {
	ID       string
	Username string
	IsAdmin  bool
}

type OIDCLoginState struct {
	Verifier string
	Nonce    string
}

type ChannelSummary struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	Description       string `json:"description,omitempty"`
	AccentColor       string `json:"accent_color,omitempty"`
	Visibility        string `json:"visibility"`
	NotificationLevel string `json:"notification_level"`
	UnreadCount       int    `json:"unread_count"`
	FiringCount       int    `json:"firing_count"`
	TotalCount        int    `json:"total_count"`
	TotalMessages     int    `json:"total_messages,omitempty"`
}

type CreateChannelInput struct {
	Name, DisplayName, Description, AccentColor, Visibility string
}

type WebhookImport struct {
	Token         string
	Channel       string
	ChannelLocked bool
}

type WebhookImportResult struct {
	Created  int
	Existing int
}

type Webhook struct {
	ID            string     `json:"id"`
	ChannelID     string     `json:"channel_id"`
	Channel       string     `json:"channel"`
	Kind          string     `json:"kind"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	ChannelLocked bool       `json:"channel_locked"`
}

type ActionTarget struct {
	ID           string
	Name         string
	URL          string
	AuthCipher   []byte
	AllowPrivate bool
}

type ActionExecution struct {
	Key            string
	Status         string
	ResponseText   string
	NotificationID string
	ActionIndex    int
	UserID         string
}

type MattermostBot struct {
	User        User
	WebhookID   string
	ChannelID   string
	ChannelName string
	TeamName    string
}

type MattermostPost struct {
	ID        string          `json:"id"`
	ChannelID string          `json:"channel_id"`
	UserID    string          `json:"user_id"`
	Message   string          `json:"message"`
	RootID    string          `json:"root_id"`
	CreateAt  int64           `json:"create_at"`
	Props     json.RawMessage `json:"props"`
}

type MattermostReaction struct {
	UserID    string `json:"user_id"`
	PostID    string `json:"post_id"`
	EmojiName string `json:"emoji_name"`
	CreateAt  int64  `json:"create_at"`
}

type MattermostActionSource struct {
	Attachments       json.RawMessage
	PostID, ChannelID string
}

type SlashCommand struct {
	ID, Team, Trigger, DisplayName, Description, Creator, Method, URL string
	TokenCipher, TokenHash                                            []byte
	Autocomplete, AllowPrivate                                        bool
	AutocompleteHint, AutocompleteDescription, Username, IconURL      string
}

type SlashCommandExecution struct {
	ID, CommandID, ChannelID, UserID, Text, ResponseTokenHash, RequestKey string
	ExpiresAt                                                             time.Time
}

type SlashCommandResponse struct {
	ID           string          `json:"id"`
	ExecutionID  string          `json:"execution_id"`
	UserID       string          `json:"-"`
	ResponseType string          `json:"response_type"`
	Text         string          `json:"text"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
}

type NotificationQuery struct {
	ID             string
	Limit          int
	Search         string
	Channel        string
	State          string
	ShowDismissed  bool
	DismissedOnly  bool
	Severity       string
	UnreadOnly     bool
	ExcludeMuted   bool
	OrderByUpdated bool
	BeforeAt       int64
	BeforeID       string
	UserID         string
	UserAdmin      bool
}

type NotificationInboxAction string

const (
	InboxMarkRead   NotificationInboxAction = "read"
	InboxMarkUnread NotificationInboxAction = "unread"
	InboxDismiss    NotificationInboxAction = "dismiss"
	InboxRestore    NotificationInboxAction = "restore"
)

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS channels (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS webhooks (
    id         TEXT PRIMARY KEY,
    token_hash BLOB NOT NULL UNIQUE,
    channel_id TEXT NOT NULL REFERENCES channels(id),
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS notifications (
    id               TEXT PRIMARY KEY,
    channel_id       TEXT NOT NULL REFERENCES channels(id),
    webhook_id       TEXT NOT NULL REFERENCES webhooks(id),
    text             TEXT NOT NULL,
    username         TEXT NOT NULL,
    icon_url         TEXT NOT NULL,
    attachments_json BLOB NOT NULL,
    raw_payload_json BLOB NOT NULL,
    created_at       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS notifications_created_at_idx
    ON notifications(created_at DESC, id DESC);
`); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return &Store{db: db}, nil
}

func OpenBackend(driverName, source string) (*Store, error) {
	switch strings.ToLower(strings.TrimSpace(driverName)) {
	case "", "sqlite":
		return Open(source)
	case "postgres", "postgresql", "pgx":
		return OpenPostgres(source)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driverName)
	}
}

func (s *Store) IsPostgres() bool { return s.postgres }

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version < 1 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE notifications ADD COLUMN external_key TEXT NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN state TEXT NOT NULL DEFAULT 'received';
ALTER TABLE notifications ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE notifications SET updated_at = created_at WHERE updated_at = 0;

CREATE UNIQUE INDEX notification_external_key_idx
    ON notifications(webhook_id, external_key)
    WHERE external_key <> '';

CREATE TABLE notification_events (
    id               TEXT PRIMARY KEY,
    notification_id  TEXT NOT NULL REFERENCES notifications(id),
    state             TEXT NOT NULL,
    raw_payload_json  BLOB NOT NULL,
    created_at        INTEGER NOT NULL
);

CREATE INDEX notification_events_notification_idx
    ON notification_events(notification_id, created_at, id);

PRAGMA user_version = 1;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 1
	}
	if version < 2 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE app_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE push_subscriptions (
    endpoint   TEXT PRIMARY KEY,
    p256dh     TEXT NOT NULL,
    auth       TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

PRAGMA user_version = 2;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 2
	}
	if version < 3 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE notifications ADD COLUMN card_json BLOB NOT NULL DEFAULT X'';
PRAGMA user_version = 3;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 3
	}
	if version < 4 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash BLOB NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE sessions (
    token_hash BLOB PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions(expires_at);
PRAGMA user_version = 4;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 4
	}
	if version < 5 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE push_subscriptions ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX push_subscriptions_user_idx ON push_subscriptions(user_id);
PRAGMA user_version = 5;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 5
	}
	if version < 6 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE channel_read_state (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    read_at INTEGER NOT NULL,
    PRIMARY KEY(user_id, channel_id)
);
PRAGMA user_version = 6;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 6
	}
	if version < 7 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE channels ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN accent_color TEXT NOT NULL DEFAULT '';
UPDATE channels SET display_name = name WHERE display_name = '';
PRAGMA user_version = 7;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 7
	}
	if version < 8 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;
ALTER TABLE channels ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public';
CREATE TABLE channel_memberships (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK(role IN ('viewer', 'operator', 'channel_admin')),
    created_at INTEGER NOT NULL,
    PRIMARY KEY(user_id, channel_id)
);
PRAGMA user_version = 8;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 8
	}
	if version < 9 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE notification_events ADD COLUMN actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
PRAGMA user_version = 9;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 9
	}
	if version < 10 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE action_targets (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    url TEXT NOT NULL,
    auth_cipher BLOB NOT NULL,
    allow_private INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE TABLE action_executions (
    operation_key TEXT PRIMARY KEY,
    notification_id TEXT NOT NULL REFERENCES notifications(id),
    action_index INTEGER NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id),
    status TEXT NOT NULL,
    response_text TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    completed_at INTEGER
);
PRAGMA user_version = 10;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 10
	}
	if version < 11 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE mattermost_bot_tokens (
    token_hash BLOB PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    team_name TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE mattermost_channel_aliases (
    team_name TEXT NOT NULL,
    channel_name TEXT NOT NULL,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    PRIMARY KEY(team_name, channel_name)
);
CREATE TABLE mattermost_posts (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    message TEXT NOT NULL,
    root_id TEXT NOT NULL,
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    props_json BLOB NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX mattermost_posts_channel_time_idx ON mattermost_posts(channel_id,created_at,id);
PRAGMA user_version = 11;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 11
	}
	if version < 12 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE mattermost_reactions (
    post_id TEXT NOT NULL REFERENCES mattermost_posts(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    emoji_name TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(post_id,user_id,emoji_name)
);
PRAGMA user_version = 12;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 13 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE slash_commands (
    id TEXT PRIMARY KEY, team_name TEXT NOT NULL, trigger_word TEXT NOT NULL,
    display_name TEXT NOT NULL, description TEXT NOT NULL, creator TEXT NOT NULL,
    method TEXT NOT NULL CHECK(method IN ('GET','POST')), url TEXT NOT NULL,
    token_cipher BLOB NOT NULL, token_hash BLOB NOT NULL, allow_private INTEGER NOT NULL DEFAULT 0,
    autocomplete INTEGER NOT NULL DEFAULT 0, autocomplete_hint TEXT NOT NULL DEFAULT '',
    autocomplete_description TEXT NOT NULL DEFAULT '', username TEXT NOT NULL DEFAULT '',
    icon_url TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
    UNIQUE(team_name,trigger_word)
);
CREATE TABLE slash_command_executions (
    id TEXT PRIMARY KEY, command_id TEXT NOT NULL REFERENCES slash_commands(id),
    channel_id TEXT NOT NULL REFERENCES channels(id), user_id TEXT NOT NULL REFERENCES users(id),
	text TEXT NOT NULL, response_token_hash BLOB NOT NULL UNIQUE, request_key TEXT NOT NULL UNIQUE, expires_at INTEGER NOT NULL,
    response_count INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL
);
CREATE TABLE slash_command_responses (
    id TEXT PRIMARY KEY, execution_id TEXT NOT NULL REFERENCES slash_command_executions(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id), response_type TEXT NOT NULL CHECK(response_type IN ('ephemeral','in_channel')),
    text TEXT NOT NULL, payload_json BLOB NOT NULL, created_at INTEGER NOT NULL
);
CREATE INDEX slash_command_responses_execution_idx ON slash_command_responses(execution_id,created_at,id);
PRAGMA user_version = 13;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 14 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE notification_user_state (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    read_at INTEGER NOT NULL DEFAULT 0,
    unread INTEGER NOT NULL DEFAULT 0,
    dismissed_at INTEGER,
    PRIMARY KEY(user_id, notification_id)
);
CREATE INDEX notification_user_state_dismissed_idx
    ON notification_user_state(user_id, dismissed_at);
INSERT OR IGNORE INTO users(id,username,password_hash,created_at,is_admin)
VALUES('usr_tintwire_local_inbox','tintwire-local-inbox',X'',0,1);
PRAGMA user_version = 14;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 15 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    owner_user_id TEXT NOT NULL REFERENCES users(id),
    user_id TEXT NOT NULL UNIQUE REFERENCES users(id),
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);
CREATE TABLE agent_credentials (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at INTEGER
);
CREATE TABLE agent_runs (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    initiator_user_id TEXT REFERENCES users(id),
    purpose TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('running','completed','failed','cancelled')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX agent_runs_agent_idx ON agent_runs(agent_id, created_at DESC, id DESC);
CREATE TABLE agent_run_events (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    tool TEXT NOT NULL,
    summary TEXT NOT NULL,
    notification_id TEXT REFERENCES notifications(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX agent_run_events_run_idx ON agent_run_events(run_id, created_at, id);
ALTER TABLE webhooks ADD COLUMN kind TEXT NOT NULL DEFAULT 'incoming';
ALTER TABLE notifications ADD COLUMN agent_id TEXT REFERENCES agents(id);
ALTER TABLE notifications ADD COLUMN agent_run_id TEXT REFERENCES agent_runs(id);
PRAGMA user_version = 15;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 16 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE agent_tool_invocations (
    agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    tool TEXT NOT NULL,
    request_fingerprint BLOB NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('running','completed')),
    result_json BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(agent_id, idempotency_key)
);
PRAGMA user_version = 16;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 17 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE channel_notification_preferences (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    level TEXT NOT NULL CHECK(level IN ('all','critical','muted')),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(user_id, channel_id)
);
PRAGMA user_version = 17;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 18 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE agents ADD COLUMN oauth_subject TEXT;
CREATE UNIQUE INDEX agents_oauth_subject_idx ON agents(oauth_subject) WHERE oauth_subject IS NOT NULL;
PRAGMA user_version = 18;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 19 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE replication_operations (
    cluster_id TEXT NOT NULL,
    origin TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK(sequence > 0),
    physical_ms INTEGER NOT NULL CHECK(physical_ms > 0),
    logical INTEGER NOT NULL CHECK(logical >= 0),
    kind TEXT NOT NULL,
    channel_id TEXT NOT NULL DEFAULT '',
    actor_type TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    payload_json BLOB NOT NULL CHECK(json_valid(CAST(payload_json AS TEXT))),
    created_at INTEGER NOT NULL,
    PRIMARY KEY(origin, sequence)
);
CREATE INDEX replication_operations_created_idx ON replication_operations(created_at, origin, sequence);
PRAGMA user_version = 19;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 20 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE replication_cursors (origin TEXT PRIMARY KEY, sequence INTEGER NOT NULL);
CREATE TABLE replication_quarantine (
    origin TEXT NOT NULL, sequence INTEGER NOT NULL, reason TEXT NOT NULL,
    envelope_json BLOB NOT NULL, created_at INTEGER NOT NULL,
    PRIMARY KEY(origin, sequence)
);
PRAGMA user_version = 20;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 21 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE replication_peer_status (
    peer TEXT PRIMARY KEY,
    node_id TEXT NOT NULL DEFAULT '',
    last_attempt_at INTEGER NOT NULL,
    last_success_at INTEGER,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
PRAGMA user_version = 21;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 22 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE replication_snapshot_status (
    source TEXT PRIMARY KEY,
    last_applied_at INTEGER NOT NULL,
    application_count INTEGER NOT NULL,
    notification_count INTEGER NOT NULL
);
PRAGMA user_version = 22;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 23 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE users ADD COLUMN oidc_subject TEXT;
CREATE UNIQUE INDEX users_oidc_subject_idx ON users(oidc_subject) WHERE oidc_subject IS NOT NULL;
CREATE TABLE oidc_login_states (
    state_hash BLOB PRIMARY KEY,
    verifier TEXT NOT NULL,
    nonce TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX oidc_login_states_expiry_idx ON oidc_login_states(expires_at);
PRAGMA user_version = 23;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 24 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE webhooks ADD COLUMN revoked_at INTEGER;
PRAGMA user_version = 24;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 25 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
ALTER TABLE users ADD COLUMN disabled_at INTEGER;
CREATE TABLE admin_audit_events (
    id TEXT PRIMARY KEY,
    actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    target_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX admin_audit_events_created_idx ON admin_audit_events(created_at DESC,id DESC);
PRAGMA user_version = 25;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 26 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
CREATE TABLE channel_messages (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id TEXT REFERENCES channel_messages(id) ON DELETE CASCADE,
    root_id TEXT NOT NULL,
    text TEXT NOT NULL,
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    deleted_at INTEGER
);
CREATE INDEX channel_messages_channel_idx ON channel_messages(channel_id, created_at, id);
CREATE INDEX channel_messages_root_idx ON channel_messages(root_id, created_at, id);
CREATE UNIQUE INDEX channel_messages_idempotency_idx ON channel_messages(channel_id, author_user_id, idempotency_key) WHERE idempotency_key <> '';
PRAGMA user_version = 26;
`); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) BootstrapUser(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 100 || len(password) < 12 {
		return errors.New("reader username is required and password must contain at least 12 characters")
	}
	var existingHash []byte
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE username = ?`, username).Scan(&existingHash)
	if err == nil {
		if bcrypt.CompareHashAndPassword(existingHash, []byte(password)) != nil {
			// A shared database is authoritative for bootstrap credentials. HA
			// nodes can retain different historical environment values after a
			// SQLite migration; they must not prevent an otherwise healthy node
			// from joining or overwrite the migrated password.
			if s.postgres {
				return nil
			}
			return errors.New("reader already exists with a different password")
		}
		_, err = s.db.ExecContext(ctx, `UPDATE users SET is_admin = 1 WHERE username = ?`, username)
		if err != nil {
			return err
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	id, err := newID("usr_", now)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at, is_admin) VALUES (?, ?, ?, ?, 1)`, id, username, hash, now.UnixMilli())
	return err
}

func (s *Store) CreateUser(ctx context.Context, username, password string, isAdmin bool) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 100 || len(password) < 12 {
		return User{}, errors.New("username is required and password must contain at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	id, err := newID("usr_", now)
	if err != nil {
		return User{}, err
	}
	adminValue := 0
	if isAdmin {
		adminValue = 1
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at, is_admin) VALUES (?, ?, ?, ?, ?)`, id, username, hash, now.UnixMilli(), adminValue)
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Username: username, IsAdmin: isAdmin}, nil
}

func (s *Store) CreateOIDCLoginState(ctx context.Context, state, verifier, nonce string, lifetime time.Duration) error {
	if state == "" || verifier == "" || nonce == "" || lifetime <= 0 {
		return errors.New("invalid OIDC login state")
	}
	hash := sha256.Sum256([]byte(state))
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oidc_login_states WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_login_states(state_hash,verifier,nonce,expires_at) VALUES(?,?,?,?)`, hash[:], verifier, nonce, now.Add(lifetime).UnixMilli())
	return err
}

func (s *Store) ConsumeOIDCLoginState(ctx context.Context, state string) (OIDCLoginState, error) {
	hash := sha256.Sum256([]byte(state))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OIDCLoginState{}, err
	}
	defer tx.Rollback()
	var result OIDCLoginState
	err = tx.QueryRowContext(ctx, `SELECT verifier,nonce FROM oidc_login_states WHERE state_hash=? AND expires_at>?`, hash[:], time.Now().UTC().UnixMilli()).Scan(&result.Verifier, &result.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return OIDCLoginState{}, ErrInvalidCredentials
	}
	if err != nil {
		return OIDCLoginState{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM oidc_login_states WHERE state_hash=?`, hash[:]); err != nil {
		return OIDCLoginState{}, err
	}
	return result, tx.Commit()
}

func (s *Store) OIDCLoginState(ctx context.Context, state string) (OIDCLoginState, error) {
	hash := sha256.Sum256([]byte(state))
	var result OIDCLoginState
	err := s.db.QueryRowContext(ctx, `SELECT verifier,nonce FROM oidc_login_states WHERE state_hash=? AND expires_at>?`, hash[:], time.Now().UTC().UnixMilli()).Scan(&result.Verifier, &result.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return OIDCLoginState{}, ErrInvalidCredentials
	}
	return result, err
}

func (s *Store) FindOrCreateOIDCUser(ctx context.Context, subject, preferredUsername string) (User, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" || len(subject) > 255 {
		return User{}, errors.New("invalid OIDC subject")
	}
	var user User
	var disabled sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id,username,is_admin,disabled_at FROM users WHERE oidc_subject=?`, subject).Scan(&user.ID, &user.Username, &user.IsAdmin, &disabled)
	if err == nil {
		if disabled.Valid {
			return User{}, ErrInvalidCredentials
		}
		return user, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	username := normalizeOIDCUsername(preferredUsername)
	if username == "" {
		username = "pocket-user"
	}
	base := username
	for suffix := 0; ; suffix++ {
		if suffix > 0 {
			username = fmt.Sprintf("%s-%d", base, suffix+1)
		}
		var exists int
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE username=?`, username).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return User{}, err
		}
	}
	randomPassword := make([]byte, 32)
	if _, err := rand.Read(randomPassword); err != nil {
		return User{}, err
	}
	hash, err := bcrypt.GenerateFromPassword(randomPassword, bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	id, err := newID("usr_", now)
	if err != nil {
		return User{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at,is_admin,oidc_subject) VALUES(?,?,?,?,0,?)`, id, username, hash, now.UnixMilli(), subject)
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Username: username}, nil
}

func normalizeOIDCUsername(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
		if b.Len() >= 80 {
			break
		}
	}
	return strings.Trim(b.String(), "-_")
}

func (s *Store) SetChannelMember(ctx context.Context, channelID, username, role string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO channel_memberships(user_id, channel_id, role, created_at)
SELECT id, ?, ?, ? FROM users WHERE username = ?
ON CONFLICT(user_id, channel_id) DO UPDATE SET role = excluded.role`, channelID, role, now.UnixMilli(), username)
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
	return nil
}

func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (User, error) {
	var user User
	var hash []byte
	err := s.db.QueryRowContext(ctx, `SELECT id, username, password_hash, is_admin FROM users WHERE username = ? AND disabled_at IS NULL`, strings.TrimSpace(username)).Scan(&user.ID, &user.Username, &hash, &user.IsAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
		return User{}, ErrInvalidCredentials
	}
	if err == nil && bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return User{}, ErrInvalidCredentials
	}
	return user, err
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	var user User
	err := s.db.QueryRowContext(ctx, `SELECT id,username,is_admin FROM users WHERE username=? AND disabled_at IS NULL`, username).Scan(&user.ID, &user.Username, &user.IsAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	return user, err
}

func (s *Store) CreateSession(ctx context.Context, userID string, lifetime time.Duration) (string, time.Time, error) {
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return "", time.Time{}, err
	}
	token := hex.EncodeToString(tokenBytes[:])
	expires, err := s.CreateSessionWithToken(ctx, userID, token, lifetime)
	return token, expires, err
}

func (s *Store) CreateSessionWithToken(ctx context.Context, userID, token string, lifetime time.Duration) (time.Time, error) {
	hash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	expires := now.Add(lifetime)
	result, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash, user_id, created_at, expires_at) SELECT ?,id,?,? FROM users WHERE id=? AND disabled_at IS NULL`, hash[:], now.UnixMilli(), expires.UnixMilli(), userID)
	if err == nil {
		if changed, countErr := result.RowsAffected(); countErr != nil {
			err = countErr
		} else if changed == 0 {
			err = ErrInvalidCredentials
		}
	}
	return expires, err
}

func (s *Store) UserForSession(ctx context.Context, token string) (User, error) {
	hash := sha256.Sum256([]byte(token))
	var user User
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.username, u.is_admin FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ? AND s.expires_at > ? AND u.disabled_at IS NULL`, hash[:], time.Now().UTC().UnixMilli()).Scan(&user.ID, &user.Username, &user.IsAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	return user, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash[:])
	return err
}

func (s *Store) RotateSession(ctx context.Context, token string, lifetime time.Duration) (string, time.Time, error) {
	oldHash := sha256.Sum256([]byte(token))
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return "", time.Time{}, err
	}
	newToken := hex.EncodeToString(tokenBytes[:])
	newHash := sha256.Sum256([]byte(newToken))
	now := time.Now().UTC()
	expires := now.Add(lifetime)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	var userID string
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE token_hash = ? AND expires_at > ?`, oldHash[:], now.UnixMilli()).Scan(&userID); errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrInvalidCredentials
	} else if err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, oldHash[:]); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`, newHash[:], userID, now.UnixMilli(), expires.UnixMilli()); err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, err
	}
	return newToken, expires, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// CheckWritable verifies that the database can acquire a write transaction
// without changing durable state. It is intended for readiness probes.
func (s *Store) CheckWritable(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES ('readiness_probe','1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		return err
	}
	return tx.Rollback()
}

func (s *Store) BootstrapWebhook(ctx context.Context, token, channelName string) error {
	now := time.Now().UTC()
	tokenHash := sha256.Sum256([]byte(token))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var channelID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, channelName).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		channelID, err = newID("chn_", now)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO channels(id, name, created_at, display_name) VALUES (?, ?, ?, ?)`,
			channelID, channelName, now.UnixMilli(), channelName); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	var existingChannelID string
	var revoked sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT channel_id,revoked_at FROM webhooks WHERE token_hash = ?`, tokenHash[:]).Scan(&existingChannelID, &revoked)
	if err == nil {
		if revoked.Valid {
			return errors.New("webhook token has been revoked")
		}
		if existingChannelID != channelID {
			return fmt.Errorf("webhook token is already mapped to another channel")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES(?, 'public') ON CONFLICT(key) DO NOTHING`, webhookChannelPolicyPrefix+hex.EncodeToString(tokenHash[:])); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	webhookID, err := newID("whk_", now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webhooks(id, token_hash, channel_id, created_at) VALUES (?, ?, ?, ?)`,
		webhookID, tokenHash[:], channelID, now.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES(?, 'public')`, webhookChannelPolicyPrefix+hex.EncodeToString(tokenHash[:])); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateChannel(ctx context.Context, input CreateChannelInput) (ChannelSummary, string, error) {
	now := time.Now().UTC()
	channelID, err := newID("chn_", now)
	if err != nil {
		return ChannelSummary{}, "", err
	}
	webhookID, err := newID("whk_", now)
	if err != nil {
		return ChannelSummary{}, "", err
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return ChannelSummary{}, "", err
	}
	token := hex.EncodeToString(tokenBytes[:])
	tokenHash := sha256.Sum256([]byte(token))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChannelSummary{}, "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO channels(id, name, display_name, description, accent_color, visibility, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, channelID, input.Name, input.DisplayName, input.Description, input.AccentColor, input.Visibility, now.UnixMilli())
	if err != nil {
		return ChannelSummary{}, "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO webhooks(id, token_hash, channel_id, created_at) VALUES (?, ?, ?, ?)`, webhookID, tokenHash[:], channelID, now.UnixMilli())
	if err != nil {
		return ChannelSummary{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES(?, 'public')`, webhookChannelPolicyPrefix+hex.EncodeToString(tokenHash[:])); err != nil {
		return ChannelSummary{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return ChannelSummary{}, "", err
	}
	return ChannelSummary{ID: channelID, Name: input.Name, DisplayName: input.DisplayName, Description: input.Description, AccentColor: input.AccentColor, Visibility: input.Visibility}, token, nil
}

func (s *Store) UpdateChannel(ctx context.Context, channelID string, input CreateChannelInput) (ChannelSummary, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE channels SET display_name=?,description=?,accent_color=?,visibility=? WHERE id=?`, input.DisplayName, input.Description, input.AccentColor, input.Visibility, channelID)
	if err != nil {
		return ChannelSummary{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ChannelSummary{}, err
	}
	if changed == 0 {
		return ChannelSummary{}, ErrChannelNotFound
	}
	var channel ChannelSummary
	err = s.db.QueryRowContext(ctx, `SELECT id,name,display_name,description,accent_color,visibility FROM channels WHERE id=?`, channelID).Scan(
		&channel.ID, &channel.Name, &channel.DisplayName, &channel.Description, &channel.AccentColor, &channel.Visibility,
	)
	return channel, err
}

func (s *Store) CreateWebhook(ctx context.Context, channelID string, channelLocked bool) (Webhook, string, error) {
	now := time.Now().UTC()
	var channelName string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM channels WHERE id=?`, channelID).Scan(&channelName); errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, "", ErrForbidden
	} else if err != nil {
		return Webhook{}, "", err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Webhook{}, "", err
	}
	token := "twh_" + hex.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(token))
	id, err := newID("whk_", now)
	if err != nil {
		return Webhook{}, "", err
	}
	policy := "locked"
	if !channelLocked {
		policy = "public"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Webhook{}, "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO webhooks(id,token_hash,channel_id,created_at,kind) VALUES(?,?,?,?,'incoming')`, id, hash[:], channelID, now.UnixMilli()); err != nil {
		return Webhook{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, webhookChannelPolicyPrefix+hex.EncodeToString(hash[:]), policy); err != nil {
		return Webhook{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Webhook{}, "", err
	}
	return Webhook{ID: id, ChannelID: channelID, Channel: channelName, Kind: "incoming", CreatedAt: now, ChannelLocked: channelLocked}, token, nil
}

// DuplicateWebhook creates an additional incoming webhook with the same channel
// and channel-override policy. The new secret is returned once and is never
// persisted in plaintext; the existing webhook remains active.
func (s *Store) DuplicateWebhook(ctx context.Context, id string) (Webhook, string, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Webhook{}, "", err
	}
	defer tx.Rollback()

	var channelID, channelName string
	var oldHash []byte
	err = tx.QueryRowContext(ctx, `SELECT w.channel_id,c.name,w.token_hash FROM webhooks w JOIN channels c ON c.id=w.channel_id WHERE w.id=? AND w.kind='incoming' AND w.revoked_at IS NULL`, id).Scan(&channelID, &channelName, &oldHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, "", ErrWebhookNotFound
	}
	if err != nil {
		return Webhook{}, "", err
	}
	policy := "locked"
	if err := tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key=?`, webhookChannelPolicyPrefix+hex.EncodeToString(oldHash)).Scan(&policy); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, "", err
	}

	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Webhook{}, "", err
	}
	token := "twh_" + hex.EncodeToString(secret[:])
	tokenHash := sha256.Sum256([]byte(token))
	newID, err := newID("whk_", now)
	if err != nil {
		return Webhook{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO webhooks(id,token_hash,channel_id,created_at,kind) VALUES(?,?,?,?,'incoming')`, newID, tokenHash[:], channelID, now.UnixMilli()); err != nil {
		return Webhook{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, webhookChannelPolicyPrefix+hex.EncodeToString(tokenHash[:]), policy); err != nil {
		return Webhook{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Webhook{}, "", err
	}
	return Webhook{ID: newID, ChannelID: channelID, Channel: channelName, Kind: "incoming", CreatedAt: now, ChannelLocked: policy != "public"}, token, nil
}

func (s *Store) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT w.id,w.channel_id,c.name,w.kind,w.created_at,w.revoked_at,COALESCE(p.value,'locked')
FROM webhooks w
JOIN channels c ON c.id=w.channel_id
LEFT JOIN app_settings p ON p.key='webhook_channel_policy:' || LOWER(HEX(w.token_hash))
WHERE w.kind='incoming'
ORDER BY w.created_at DESC,w.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Webhook, 0)
	for rows.Next() {
		var item Webhook
		var created int64
		var revoked sql.NullInt64
		var policy string
		if err := rows.Scan(&item.ID, &item.ChannelID, &item.Channel, &item.Kind, &created, &revoked, &policy); err != nil {
			return nil, err
		}
		item.CreatedAt = time.UnixMilli(created).UTC()
		if revoked.Valid {
			at := time.UnixMilli(revoked.Int64).UTC()
			item.RevokedAt = &at
		}
		item.ChannelLocked = policy != "public"
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SetWebhookChannelLocked(ctx context.Context, id string, channelLocked bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tokenHash []byte
	err = tx.QueryRowContext(ctx, `SELECT token_hash FROM webhooks WHERE id=? AND kind='incoming' AND revoked_at IS NULL`, id).Scan(&tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWebhookNotFound
	}
	if err != nil {
		return err
	}
	policy := "public"
	if channelLocked {
		policy = "locked"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, webhookChannelPolicyPrefix+hex.EncodeToString(tokenHash), policy); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RevokeWebhook(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE webhooks SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND kind='incoming'`, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrWebhookNotFound
	}
	return nil
}

func (s *Store) ImportWebhooks(ctx context.Context, imports []WebhookImport, apply bool) (WebhookImportResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebhookImportResult{}, err
	}
	defer tx.Rollback()
	result := WebhookImportResult{}
	seen := make(map[[32]byte]struct{}, len(imports))
	for _, item := range imports {
		hash := sha256.Sum256([]byte(item.Token))
		policyKey := webhookChannelPolicyPrefix + hex.EncodeToString(hash[:])
		policy := "locked"
		if !item.ChannelLocked {
			policy = "public"
		}
		if _, duplicate := seen[hash]; duplicate {
			return WebhookImportResult{}, fmt.Errorf("%w: duplicate webhook in import", ErrImportConflict)
		}
		seen[hash] = struct{}{}
		var channelID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, item.Channel).Scan(&channelID)
		if errors.Is(err, sql.ErrNoRows) {
			return WebhookImportResult{}, fmt.Errorf("%w: destination channel does not exist", ErrImportConflict)
		}
		if err != nil {
			return WebhookImportResult{}, err
		}
		var existingChannelID string
		var revoked sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT channel_id,revoked_at FROM webhooks WHERE token_hash = ?`, hash[:]).Scan(&existingChannelID, &revoked)
		if err == nil {
			if revoked.Valid {
				return WebhookImportResult{}, fmt.Errorf("%w: webhook token has been revoked", ErrImportConflict)
			}
			if existingChannelID != channelID {
				return WebhookImportResult{}, fmt.Errorf("%w: webhook is already mapped to another channel", ErrImportConflict)
			}
			result.Existing++
			if apply {
				if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, policyKey, policy); err != nil {
					return WebhookImportResult{}, err
				}
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return WebhookImportResult{}, err
		}
		result.Created++
		if apply {
			now := time.Now().UTC()
			id, err := newID("whk_", now)
			if err != nil {
				return WebhookImportResult{}, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO webhooks(id, token_hash, channel_id, created_at) VALUES (?, ?, ?, ?)`, id, hash[:], channelID, now.UnixMilli()); err != nil {
				return WebhookImportResult{}, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, policyKey, policy); err != nil {
				return WebhookImportResult{}, err
			}
		}
	}
	if !apply {
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return WebhookImportResult{}, err
	}
	return result, nil
}

func (s *Store) ImportMattermostBot(ctx context.Context, token, username, teamName, channelName string) error {
	if err := s.BootstrapWebhook(ctx, token, channelName); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, channelID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username=?`, username).Scan(&userID); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: bot user does not exist", ErrImportConflict)
	} else if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name=?`, channelName).Scan(&channelID); err != nil {
		return err
	}
	var aliasChannel string
	err = tx.QueryRowContext(ctx, `SELECT channel_id FROM mattermost_channel_aliases WHERE team_name=? AND channel_name=?`, teamName, channelName).Scan(&aliasChannel)
	if err == nil && aliasChannel != channelID {
		return fmt.Errorf("%w: Mattermost alias is mapped to another channel", ErrImportConflict)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mattermost_channel_aliases(team_name,channel_name,channel_id) VALUES(?,?,?)`, teamName, channelName, channelID); err != nil {
			return err
		}
	}
	var existingUser, existingChannel, existingTeam string
	err = tx.QueryRowContext(ctx, `SELECT user_id,channel_id,team_name FROM mattermost_bot_tokens WHERE token_hash=?`, hash[:]).Scan(&existingUser, &existingChannel, &existingTeam)
	if err == nil {
		if existingUser != userID || existingChannel != channelID || existingTeam != teamName {
			return fmt.Errorf("%w: bot token mapping differs", ErrImportConflict)
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mattermost_bot_tokens(token_hash,user_id,channel_id,team_name,created_at) VALUES(?,?,?,?,?)`, hash[:], userID, channelID, teamName, now.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AuthenticateMattermostBot(ctx context.Context, token string) (MattermostBot, error) {
	hash := sha256.Sum256([]byte(token))
	var bot MattermostBot
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.is_admin,c.id,c.name,b.team_name,(SELECT id FROM webhooks w WHERE w.token_hash=b.token_hash LIMIT 1) FROM mattermost_bot_tokens b JOIN users u ON u.id=b.user_id JOIN channels c ON c.id=b.channel_id WHERE b.token_hash=?`, hash[:]).Scan(&bot.User.ID, &bot.User.Username, &bot.User.IsAdmin, &bot.ChannelID, &bot.ChannelName, &bot.TeamName, &bot.WebhookID)
	if errors.Is(err, sql.ErrNoRows) {
		return MattermostBot{}, ErrInvalidCredentials
	}
	return bot, err
}

func (s *Store) RecordMattermostPost(ctx context.Context, bot MattermostBot, channelID, message, rootID string, props json.RawMessage, notificationID string) (MattermostPost, error) {
	if rootID != "" {
		root, err := s.messageByID(ctx, rootID)
		if err == nil {
			if root.ChannelID != channelID {
				return MattermostPost{}, ErrNotificationNotFound
			}
			reply, err := s.createChannelMessage(ctx, bot.User, CreateMessageInput{ChannelID: channelID, ParentID: rootID, Text: message})
			if err != nil {
				return MattermostPost{}, err
			}
			if len(props) == 0 {
				props = json.RawMessage(`{}`)
			}
			return MattermostPost{ID: reply.ID, ChannelID: channelID, UserID: bot.User.ID, Message: reply.Text, RootID: reply.RootID, CreateAt: reply.CreatedAt.UnixMilli(), Props: props}, nil
		}
		if !errors.Is(err, ErrMessageNotFound) {
			return MattermostPost{}, err
		}
	}
	now := time.Now().UTC()
	var lastCreated int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at),0) FROM mattermost_posts WHERE channel_id=?`, channelID).Scan(&lastCreated); err != nil {
		return MattermostPost{}, err
	}
	if now.UnixMilli() <= lastCreated {
		now = time.UnixMilli(lastCreated + 1).UTC()
	}
	id, err := newID("pst_", now)
	if err != nil {
		return MattermostPost{}, err
	}
	if len(props) == 0 {
		props = json.RawMessage(`{}`)
	}
	if rootID != "" {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return MattermostPost{}, err
		}
		defer tx.Rollback()
		var rootNotification, state string
		if err := tx.QueryRowContext(ctx, `SELECT notification_id FROM mattermost_posts WHERE id=? AND channel_id=?`, rootID, channelID).Scan(&rootNotification); errors.Is(err, sql.ErrNoRows) {
			return MattermostPost{}, ErrNotificationNotFound
		} else if err != nil {
			return MattermostPost{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT state FROM notifications WHERE id=?`, rootNotification).Scan(&state); err != nil {
			return MattermostPost{}, err
		}
		raw, _ := json.Marshal(map[string]any{"text": message, "props": json.RawMessage(props)})
		eventID, err := newID("evt_", now)
		if err != nil {
			return MattermostPost{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(id,notification_id,state,raw_payload_json,created_at,actor_user_id) VALUES(?,?,?,?,?,?)`, eventID, rootNotification, state, raw, now.UnixMilli(), bot.User.ID); err != nil {
			return MattermostPost{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notifications SET updated_at=? WHERE id=?`, now.UnixMilli(), rootNotification); err != nil {
			return MattermostPost{}, err
		}
		notificationID = rootNotification
		if _, err := tx.ExecContext(ctx, `INSERT INTO mattermost_posts(id,channel_id,user_id,message,root_id,notification_id,props_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, channelID, bot.User.ID, message, rootID, notificationID, props, now.UnixMilli()); err != nil {
			return MattermostPost{}, err
		}
		if err := tx.Commit(); err != nil {
			return MattermostPost{}, err
		}
	} else {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO mattermost_posts(id,channel_id,user_id,message,root_id,notification_id,props_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, channelID, bot.User.ID, message, "", notificationID, props, now.UnixMilli()); err != nil {
			return MattermostPost{}, err
		}
	}
	return MattermostPost{ID: id, ChannelID: channelID, UserID: bot.User.ID, Message: message, RootID: rootID, CreateAt: now.UnixMilli(), Props: props}, nil
}

func (s *Store) ListMattermostPosts(ctx context.Context, channelID string, since int64) ([]MattermostPost, error) {
	// Native Tintwire channel messages are posts from the perspective of a
	// Mattermost-compatible bot. Include them here so polling integrations keep
	// working when a human uses Tintwire's composer instead of Mattermost's UI.
	rows, err := s.db.QueryContext(ctx, `SELECT id,channel_id,user_id,message,root_id,props_json,created_at FROM mattermost_posts WHERE channel_id=? AND created_at>? ORDER BY created_at,id`, channelID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posts := make([]MattermostPost, 0)
	for rows.Next() {
		var post MattermostPost
		if err := rows.Scan(&post.ID, &post.ChannelID, &post.UserID, &post.Message, &post.RootID, &post.Props, &post.CreateAt); err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nativeRows, err := s.db.QueryContext(ctx, `SELECT id,channel_id,author_user_id,text,CASE WHEN parent_id IS NULL THEN '' ELSE root_id END,created_at FROM channel_messages WHERE channel_id=? AND created_at>? AND deleted_at IS NULL ORDER BY created_at,id`, channelID, since)
	if err != nil {
		return nil, err
	}
	defer nativeRows.Close()
	for nativeRows.Next() {
		var post MattermostPost
		if err := nativeRows.Scan(&post.ID, &post.ChannelID, &post.UserID, &post.Message, &post.RootID, &post.CreateAt); err != nil {
			return nil, err
		}
		post.Props = json.RawMessage(`{}`)
		posts = append(posts, post)
	}
	if err := nativeRows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(posts, func(i, j int) bool {
		if posts[i].CreateAt == posts[j].CreateAt {
			return posts[i].ID < posts[j].ID
		}
		return posts[i].CreateAt < posts[j].CreateAt
	})
	return posts, nil
}

func (s *Store) RecordMattermostApproval(ctx context.Context, notificationID string, actor User, emoji string) error {
	if emoji != "white_check_mark" && emoji != "x" {
		return ErrInvalidTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var postID, channelID, state string
	if err := tx.QueryRowContext(ctx, `SELECT mp.id,mp.channel_id,n.state FROM mattermost_posts mp JOIN notifications n ON n.id=mp.notification_id WHERE mp.notification_id=? AND mp.root_id=''`, notificationID).Scan(&postID, &channelID, &state); errors.Is(err, sql.ErrNoRows) {
		return ErrNotificationNotFound
	} else if err != nil {
		return err
	}
	if !actor.IsAdmin {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channel_memberships WHERE user_id=? AND channel_id=? AND role IN('operator','channel_admin'))`, actor.ID, channelID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrForbidden
		}
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `DELETE FROM mattermost_reactions WHERE post_id=? AND user_id=? AND emoji_name IN('white_check_mark','x')`, postID, actor.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mattermost_reactions(post_id,user_id,emoji_name,created_at) VALUES(?,?,?,?)`, postID, actor.ID, emoji, now.UnixMilli()); err != nil {
		return err
	}
	label := "Approved"
	if emoji == "x" {
		label = "Rejected"
	}
	raw, _ := json.Marshal(map[string]string{"text": label})
	eventID, err := newID("evt_", now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(id,notification_id,state,raw_payload_json,created_at,actor_user_id) VALUES(?,?,?,?,?,?)`, eventID, notificationID, state, raw, now.UnixMilli(), actor.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notifications SET updated_at=? WHERE id=?`, now.UnixMilli(), notificationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListMattermostReactions(ctx context.Context, postID, channelID string) ([]MattermostReaction, error) {
	var belongs bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mattermost_posts WHERE id=? AND channel_id=?)`, postID, channelID).Scan(&belongs); err != nil {
		return nil, err
	}
	if !belongs {
		return nil, ErrNotificationNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,post_id,emoji_name,created_at FROM mattermost_reactions WHERE post_id=? ORDER BY created_at,user_id,emoji_name`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MattermostReaction, 0)
	for rows.Next() {
		var reaction MattermostReaction
		if err := rows.Scan(&reaction.UserID, &reaction.PostID, &reaction.EmojiName, &reaction.CreateAt); err != nil {
			return nil, err
		}
		result = append(result, reaction)
	}
	return result, rows.Err()
}

func (s *Store) CreateFromWebhook(ctx context.Context, token string, input IncomingNotification) (Notification, error) {
	if input.State == "" {
		input.State = "received"
	}
	if input.State != "received" && input.State != "firing" && input.State != "acknowledged" && input.State != "resolved" {
		return Notification{}, fmt.Errorf("unsupported notification state %q", input.State)
	}
	if len(input.Attachments) == 0 {
		input.Attachments = json.RawMessage("[]")
	}
	cardData := []byte(input.Card)
	if cardData == nil {
		cardData = []byte{}
	}
	username := input.Username
	if username == "" {
		username = "webhook"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, err
	}
	defer tx.Rollback()

	tokenHash := sha256.Sum256([]byte(token))
	var webhookID, channelID, channelName string
	err = tx.QueryRowContext(ctx, `
SELECT w.id, c.id, c.name
FROM webhooks w
JOIN channels c ON c.id = w.channel_id
WHERE w.token_hash = ? AND w.revoked_at IS NULL`, tokenHash[:]).Scan(&webhookID, &channelID, &channelName)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrWebhookNotFound
	}
	if err != nil {
		return Notification{}, err
	}
	if override := strings.TrimPrefix(strings.TrimSpace(input.Channel), "#"); override != "" {
		var policy string
		policyKey := webhookChannelPolicyPrefix + hex.EncodeToString(tokenHash[:])
		err = tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, policyKey).Scan(&policy)
		if errors.Is(err, sql.ErrNoRows) {
			policy = "locked"
		} else if err != nil {
			return Notification{}, err
		}
		if policy == "public" {
			err = tx.QueryRowContext(ctx, `SELECT id, name FROM channels WHERE name = ? AND visibility = 'public'`, override).Scan(&channelID, &channelName)
			if errors.Is(err, sql.ErrNoRows) {
				return Notification{}, ErrWebhookChannelDenied
			}
			if err != nil {
				return Notification{}, err
			}
		}
	}

	now := time.Now().UTC()
	if input.ExternalKey != "" {
		var existingID string
		var createdAt int64
		var existingState string
		err = tx.QueryRowContext(ctx, `
SELECT id, created_at, state
FROM notifications
WHERE webhook_id = ? AND external_key = ?`, webhookID, input.ExternalKey).Scan(&existingID, &createdAt, &existingState)
		if err == nil {
			reopened := existingState == "resolved" && input.State == "firing"
			if !reopened && stateRank(existingState) > stateRank(input.State) {
				input.State = existingState
			}
			if now.UnixMilli() <= createdAt {
				now = time.UnixMilli(createdAt + 1).UTC()
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE notifications
SET channel_id = ?, text = ?, username = ?, icon_url = ?, attachments_json = ?,
    raw_payload_json = ?, state = ?, updated_at = ?, card_json = ?
WHERE id = ?`, channelID, input.Text, username, input.IconURL, []byte(input.Attachments),
				[]byte(input.RawPayload), input.State, now.UnixMilli(), cardData, existingID); err != nil {
				return Notification{}, err
			}
			if err := insertNotificationEvent(ctx, tx, existingID, input.State, input.RawPayload, now); err != nil {
				return Notification{}, err
			}
			if reopened {
				// A recurring incident is new work for every reader. Do not let the
				// previous occurrence's read or archive decisions hide it.
				if _, err := tx.ExecContext(ctx, `DELETE FROM notification_user_state WHERE notification_id = ?`, existingID); err != nil {
					return Notification{}, err
				}
			}
			if err := s.appendReplicationOperation(ctx, tx, "notification.updated", channelID, "webhook", webhookID, map[string]any{
				"notification_id": existingID, "state": input.State, "text": input.Text,
				"username": username, "icon_url": input.IconURL, "card_json": string(cardData),
				"attachments_json": string(input.Attachments), "raw_payload_json": string(input.RawPayload),
				"updated_at": now.UnixMilli(),
			}, now); err != nil {
				return Notification{}, err
			}
			if err := tx.Commit(); err != nil {
				return Notification{}, err
			}
			return Notification{
				ID: existingID, ChannelID: channelID, ChannelName: channelName,
				Text: input.Text, Username: username, IconURL: input.IconURL,
				Attachments: input.Attachments, State: input.State,
				CreatedAt: time.UnixMilli(createdAt).UTC(), UpdatedAt: now, Card: input.Card,
			}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Notification{}, err
		}
	}

	var id string
	if input.ExternalKey != "" {
		id = notificationIDForExternalKey(webhookID, input.ExternalKey)
	} else {
		id, err = newID("ntf_", now)
		if err != nil {
			return Notification{}, err
		}
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO notifications(
    id, channel_id, webhook_id, text, username, icon_url,
    attachments_json, raw_payload_json, created_at, external_key, state, updated_at, card_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, channelID, webhookID, input.Text, username, input.IconURL,
		[]byte(input.Attachments), []byte(input.RawPayload), now.UnixMilli(),
		input.ExternalKey, input.State, now.UnixMilli(), cardData)
	if err != nil {
		return Notification{}, err
	}
	if err := insertNotificationEvent(ctx, tx, id, input.State, input.RawPayload, now); err != nil {
		return Notification{}, err
	}
	if err := s.appendReplicationOperation(ctx, tx, "notification.created", channelID, "webhook", webhookID, map[string]any{
		"notification_id": id, "state": input.State, "text": input.Text,
		"username": username, "icon_url": input.IconURL, "card_json": string(cardData),
		"attachments_json": string(input.Attachments), "raw_payload_json": string(input.RawPayload),
		"external_key": input.ExternalKey, "channel_name": channelName,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(),
	}, now); err != nil {
		return Notification{}, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, err
	}

	return Notification{
		ID: id, ChannelID: channelID, ChannelName: channelName,
		Text: input.Text, Username: username, IconURL: input.IconURL,
		Attachments: input.Attachments, State: input.State, CreatedAt: now, UpdatedAt: now, Card: input.Card,
	}, nil
}

func notificationIDForExternalKey(webhookID, externalKey string) string {
	digest := sha256.Sum256([]byte(webhookID + "\x00" + externalKey))
	return "ntf_" + hex.EncodeToString(digest[:16])
}

func (s *Store) ListNotificationEvents(ctx context.Context, notificationID string) ([]NotificationEvent, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notifications WHERE id = ?)`, notificationID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotificationNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT e.id, e.state, e.raw_payload_json, e.created_at, COALESCE(u.username, '')
FROM notification_events e
LEFT JOIN users u ON u.id = e.actor_user_id
WHERE e.notification_id = ?
ORDER BY e.created_at, e.id`, notificationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]NotificationEvent, 0)
	for rows.Next() {
		var event NotificationEvent
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.State, &event.RawPayload, &createdAt, &event.Actor); err != nil {
			return nil, err
		}
		event.CreatedAt = time.UnixMilli(createdAt).UTC()
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) CanReadNotification(ctx context.Context, notificationID string, user User) (bool, error) {
	if user.ID == "" || user.IsAdmin {
		return true, nil
	}
	var allowed bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(
  SELECT 1 FROM notifications n JOIN channels c ON c.id = n.channel_id
  WHERE n.id = ? AND (c.visibility = 'public' OR EXISTS (
    SELECT 1 FROM channel_memberships m WHERE m.channel_id = c.id AND m.user_id = ?
  ))
)`, notificationID, user.ID).Scan(&allowed)
	return allowed, err
}

func insertNotificationEvent(ctx context.Context, tx *sql.Tx, notificationID, state string, rawPayload json.RawMessage, now time.Time) error {
	id, err := newID("evt_", now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO notification_events(id, notification_id, state, raw_payload_json, created_at)
VALUES (?, ?, ?, ?, ?)`, id, notificationID, state, []byte(rawPayload), now.UnixMilli())
	return err
}

func (s *Store) SetNotificationState(ctx context.Context, notificationID string, actor User, nextState string) error {
	if nextState != "acknowledged" && nextState != "resolved" {
		return ErrInvalidTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentState, channelID string
	var updatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT state, channel_id, updated_at FROM notifications WHERE id = ?`, notificationID).Scan(&currentState, &channelID, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotificationNotFound
	}
	if err != nil {
		return err
	}
	if !actor.IsAdmin {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channel_memberships WHERE user_id = ? AND channel_id = ? AND role IN ('operator','channel_admin'))`, actor.ID, channelID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrForbidden
		}
	}
	if currentState == nextState {
		return tx.Commit()
	}
	valid := (currentState == "received" || currentState == "firing") && (nextState == "acknowledged" || nextState == "resolved") || currentState == "acknowledged" && nextState == "resolved"
	if !valid {
		return ErrInvalidTransition
	}
	now := time.Now().UTC()
	if now.UnixMilli() <= updatedAt {
		now = time.UnixMilli(updatedAt + 1).UTC()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notifications SET state = ?, updated_at = ? WHERE id = ?`, nextState, now.UnixMilli(), notificationID); err != nil {
		return err
	}
	eventID, err := newID("evt_", now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(id, notification_id, state, raw_payload_json, created_at, actor_user_id) VALUES (?, ?, ?, ?, ?, ?)`, eventID, notificationID, nextState, []byte("{}"), now.UnixMilli(), actor.ID); err != nil {
		return err
	}
	if err := s.appendReplicationOperation(ctx, tx, "notification.state", channelID, "user", actor.ID, map[string]any{
		"notification_id": notificationID, "state": nextState, "event_id": eventID,
		"updated_at": now.UnixMilli(),
	}, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListNotifications(ctx context.Context, limit int) ([]Notification, error) {
	return s.QueryNotifications(ctx, NotificationQuery{Limit: limit})
}

func (s *Store) QueryNotifications(ctx context.Context, query NotificationQuery) ([]Notification, error) {
	if query.Limit <= 0 || query.Limit > 201 {
		query.Limit = 100
	}
	readExpression := "0"
	operateExpression := "0"
	joinReadState := ""
	args := make([]any, 0, 10)
	if query.UserAdmin {
		operateExpression = "1"
	} else if query.UserID != "" {
		operateExpression = "EXISTS (SELECT 1 FROM channel_memberships operation_membership WHERE operation_membership.channel_id = n.channel_id AND operation_membership.user_id = ? AND operation_membership.role IN ('operator', 'channel_admin'))"
		args = append(args, query.UserID)
	}
	if query.UserID != "" {
		readExpression = "CASE WHEN COALESCE(us.unread, 0) = 1 THEN 1 WHEN n.updated_at > MAX(COALESCE(rs.read_at, 0), COALESCE(us.read_at, 0)) THEN 1 ELSE 0 END"
		joinReadState = " LEFT JOIN channel_read_state rs ON rs.channel_id = n.channel_id AND rs.user_id = ? LEFT JOIN notification_user_state us ON us.notification_id = n.id AND us.user_id = ?"
		args = append(args, query.UserID, query.UserID)
	}
	statement := `
SELECT n.id, c.id, c.name, n.text, n.username, n.icon_url,
       n.attachments_json, n.state, n.created_at, n.updated_at, CAST(n.card_json AS BLOB),
       (SELECT COUNT(*) FROM notification_events e WHERE e.notification_id = n.id), ` + readExpression + `, ` + operateExpression + `,
       EXISTS(SELECT 1 FROM mattermost_posts mp WHERE mp.notification_id=n.id AND mp.root_id=''),
       COALESCE((SELECT a.name FROM agents a WHERE a.id = n.agent_id), '')
FROM notifications n
JOIN channels c ON c.id = n.channel_id` + joinReadState + `
WHERE 1 = 1`
	if query.UserID != "" {
		if query.DismissedOnly {
			statement += ` AND us.dismissed_at IS NOT NULL`
		} else if !query.ShowDismissed {
			statement += ` AND us.dismissed_at IS NULL`
		}
	}
	if query.UserID != "" && !query.UserAdmin {
		statement += ` AND (c.visibility = 'public' OR EXISTS (SELECT 1 FROM channel_memberships m WHERE m.channel_id = c.id AND m.user_id = ?))`
		args = append(args, query.UserID)
	}
	if query.ExcludeMuted && query.UserID != "" {
		statement += ` AND COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id = ? AND p.channel_id = c.id), 'all') <> 'muted'`
		args = append(args, query.UserID)
	}
	if search := strings.TrimSpace(query.Search); search != "" {
		statement += ` AND (n.text LIKE ? ESCAPE '\' COLLATE NOCASE OR n.username LIKE ? ESCAPE '\' COLLATE NOCASE OR c.name LIKE ? ESCAPE '\' COLLATE NOCASE OR CAST(n.card_json AS TEXT) LIKE ? ESCAPE '\' COLLATE NOCASE OR CAST(n.attachments_json AS TEXT) LIKE ? ESCAPE '\' COLLATE NOCASE)`
		pattern := "%" + escapeLike(search) + "%"
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	if query.ID != "" {
		statement += ` AND n.id = ?`
		args = append(args, query.ID)
	}
	if query.Channel != "" {
		statement += ` AND c.name = ?`
		args = append(args, query.Channel)
	}
	if query.State != "" {
		statement += ` AND n.state = ?`
		args = append(args, query.State)
	}
	if query.Severity != "" {
		statement += ` AND json_extract(CASE WHEN json_valid(CAST(n.card_json AS TEXT)) THEN CAST(n.card_json AS TEXT) ELSE '{}' END, '$.severity') = ?`
		args = append(args, query.Severity)
	}
	if query.UnreadOnly && query.UserID != "" {
		statement += ` AND (` + readExpression + `) = 1`
	}
	orderColumn := "n.created_at"
	if query.OrderByUpdated {
		orderColumn = "n.updated_at"
	}
	if query.BeforeAt > 0 && query.BeforeID != "" {
		statement += ` AND (` + orderColumn + ` < ? OR (` + orderColumn + ` = ? AND n.id < ?))`
		args = append(args, query.BeforeAt, query.BeforeAt, query.BeforeID)
	}
	statement += ` ORDER BY ` + orderColumn + ` DESC, n.id DESC LIMIT ?`
	args = append(args, query.Limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	notifications := make([]Notification, 0)
	for rows.Next() {
		var notification Notification
		var createdAt, updatedAt int64
		if err := rows.Scan(
			&notification.ID, &notification.ChannelID, &notification.ChannelName,
			&notification.Text, &notification.Username, &notification.IconURL,
			&notification.Attachments, &notification.State, &createdAt, &updatedAt, &notification.Card,
			&notification.EventCount,
			&notification.Unread,
			&notification.CanOperate,
			&notification.CompatRoot,
			&notification.Agent,
		); err != nil {
			return nil, err
		}
		notification.CreatedAt = time.UnixMilli(createdAt).UTC()
		notification.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		notification.CanApprove = notification.CanOperate && notification.CompatRoot && mattermostApprovalRequested(notification.Attachments)
		notifications = append(notifications, notification)
	}
	return notifications, rows.Err()
}

func mattermostApprovalRequested(raw json.RawMessage) bool {
	var attachments []struct {
		Actions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"actions"`
	}
	if json.Unmarshal(raw, &attachments) != nil {
		return false
	}
	for _, attachment := range attachments {
		for _, action := range attachment.Actions {
			label := strings.ToLower(strings.TrimSpace(action.ID + " " + action.Name))
			if strings.Contains(label, "approv") || strings.Contains(label, "reject") {
				return true
			}
		}
	}
	return false
}

func (s *Store) MarkAllRead(ctx context.Context, userID string, readAt time.Time) error {
	if userID == "" {
		return errors.New("user is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO channel_read_state(user_id, channel_id, read_at)
SELECT ?, id, MAX(?, COALESCE((SELECT MAX(updated_at) FROM notifications WHERE channel_id=channels.id), 0), COALESCE((SELECT MAX(created_at) FROM channel_messages WHERE channel_id=channels.id AND deleted_at IS NULL), 0), COALESCE((SELECT MAX(r.created_at) FROM slash_command_responses r JOIN slash_command_executions e ON e.id=r.execution_id WHERE e.channel_id=channels.id AND r.response_type='in_channel'), 0)) FROM channels WHERE true
ON CONFLICT(user_id, channel_id) DO UPDATE SET read_at = MAX(read_at, excluded.read_at)`, userID, readAt.UTC().UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_user_state SET unread = 0 WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkChannelRead(ctx context.Context, user User, channelID string, readAt time.Time) error {
	if user.ID == "" || channelID == "" {
		return errors.New("user and channel are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO channel_read_state(user_id, channel_id, read_at)
SELECT ?, c.id, MAX(?, COALESCE((SELECT MAX(updated_at) FROM notifications WHERE channel_id=c.id), 0), COALESCE((SELECT MAX(created_at) FROM channel_messages WHERE channel_id=c.id AND deleted_at IS NULL), 0), COALESCE((SELECT MAX(r.created_at) FROM slash_command_responses r JOIN slash_command_executions e ON e.id=r.execution_id WHERE e.channel_id=c.id AND r.response_type='in_channel'), 0))
FROM channels c
WHERE c.id=? AND (? OR c.visibility='public' OR EXISTS (
  SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?
))
ON CONFLICT(user_id, channel_id) DO UPDATE SET read_at=MAX(read_at, excluded.read_at)`,
		user.ID, readAt.UTC().UnixMilli(), channelID, user.IsAdmin, user.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrForbidden
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE notification_user_state SET unread=0
WHERE user_id=? AND notification_id IN (SELECT id FROM notifications WHERE channel_id=?)`, user.ID, channelID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetNotificationInboxState(ctx context.Context, userID, notificationID string, action NotificationInboxAction, at time.Time) error {
	if userID == "" {
		return errors.New("user is required")
	}
	var statement string
	switch action {
	case InboxMarkRead:
		statement = `INSERT INTO notification_user_state(user_id,notification_id,read_at,unread) VALUES(?,?,?,0)
ON CONFLICT(user_id,notification_id) DO UPDATE SET read_at=MAX(read_at,excluded.read_at),unread=0`
	case InboxMarkUnread:
		statement = `INSERT INTO notification_user_state(user_id,notification_id,unread) VALUES(?,?,1)
ON CONFLICT(user_id,notification_id) DO UPDATE SET unread=1`
	case InboxDismiss:
		statement = `INSERT INTO notification_user_state(user_id,notification_id,read_at,unread,dismissed_at) VALUES(?,?,?,0,?)
ON CONFLICT(user_id,notification_id) DO UPDATE SET read_at=MAX(read_at,excluded.read_at),unread=0,dismissed_at=excluded.dismissed_at`
	case InboxRestore:
		statement = `INSERT INTO notification_user_state(user_id,notification_id,dismissed_at) VALUES(?,?,NULL)
ON CONFLICT(user_id,notification_id) DO UPDATE SET dismissed_at=NULL`
	default:
		return errors.New("invalid inbox action")
	}
	arguments := []any{userID, notificationID}
	if action == InboxMarkRead {
		arguments = append(arguments, at.UTC().UnixMilli())
	} else if action == InboxDismiss {
		arguments = append(arguments, at.UTC().UnixMilli(), at.UTC().UnixMilli())
	}
	result, err := s.db.ExecContext(ctx, statement, arguments...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotificationNotFound
	}
	return nil
}

func (s *Store) UnreadCount(ctx context.Context, user User) (int, error) {
	if user.ID == "" {
		return 0, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM notifications n
JOIN channels c ON c.id = n.channel_id
LEFT JOIN channel_read_state rs ON rs.channel_id = n.channel_id AND rs.user_id = ?
LEFT JOIN notification_user_state us ON us.notification_id = n.id AND us.user_id = ?
WHERE (COALESCE(us.unread, 0) = 1 OR n.updated_at > MAX(COALESCE(rs.read_at, 0), COALESCE(us.read_at, 0)))
  AND us.dismissed_at IS NULL
  AND COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id = ? AND p.channel_id = c.id), 'all') <> 'muted'
  AND (? OR c.visibility = 'public' OR EXISTS (
    SELECT 1 FROM channel_memberships m WHERE m.channel_id = c.id AND m.user_id = ?
  ))`, user.ID, user.ID, user.ID, user.IsAdmin, user.ID).Scan(&count)
	if err != nil {
		return 0, err
	}
	messageUnread, err := s.messageUnreadCount(ctx, user)
	if err != nil {
		return 0, err
	}
	commandUnread, err := s.commandUnreadCount(ctx, user)
	if err != nil {
		return 0, err
	}
	return count + messageUnread + commandUnread, nil
}

func (s *Store) ListChannels(ctx context.Context, user User) ([]ChannelSummary, error) {
	statement := `
SELECT c.id, c.name, c.display_name, c.description, c.accent_color, c.visibility,
	   COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id = NULLIF(?, '') AND p.channel_id = c.id), 'all'),
	   COUNT(CASE WHEN us.dismissed_at IS NULL THEN n.id END),
	   COALESCE(SUM(CASE WHEN ? <> '' AND us.dismissed_at IS NULL AND (COALESCE(us.unread, 0) = 1 OR n.updated_at > MAX(COALESCE(rs.read_at, 0), COALESCE(us.read_at, 0))) THEN 1 ELSE 0 END), 0),
	   COALESCE(SUM(CASE WHEN us.dismissed_at IS NULL AND n.state = 'firing' THEN 1 ELSE 0 END), 0)
FROM channels c
LEFT JOIN notifications n ON n.channel_id = c.id
LEFT JOIN channel_read_state rs ON rs.channel_id = c.id AND rs.user_id = NULLIF(?, '')
LEFT JOIN notification_user_state us ON us.notification_id = n.id AND us.user_id = NULLIF(?, '')
WHERE 1 = 1`
	args := []any{user.ID, user.ID, user.ID, user.ID}
	if user.ID != "" && !user.IsAdmin {
		statement += ` AND (c.visibility = 'public' OR EXISTS (SELECT 1 FROM channel_memberships m WHERE m.channel_id = c.id AND m.user_id = ?))`
		args = append(args, user.ID)
	}
	statement += ` GROUP BY c.id, c.name, c.display_name, c.description, c.accent_color, c.visibility ORDER BY c.display_name COLLATE NOCASE, c.name COLLATE NOCASE`
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	channels := make([]ChannelSummary, 0)
	for rows.Next() {
		var channel ChannelSummary
		if err := rows.Scan(&channel.ID, &channel.Name, &channel.DisplayName, &channel.Description, &channel.AccentColor, &channel.Visibility, &channel.NotificationLevel, &channel.TotalCount, &channel.UnreadCount, &channel.FiringCount); err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	messageCounts, err := s.channelMessageCounts(ctx, user)
	if err != nil {
		return nil, err
	}
	for index := range channels {
		if count, ok := messageCounts[channels[index].ID]; ok {
			channels[index].TotalMessages = count.Total
			channels[index].TotalCount += count.Total
			channels[index].UnreadCount += count.Unread
		}
	}
	commandCounts, err := s.channelCommandCounts(ctx, user)
	if err != nil {
		return nil, err
	}
	for index := range channels {
		if count, ok := commandCounts[channels[index].ID]; ok {
			channels[index].TotalCount += count.Total
			channels[index].UnreadCount += count.Unread
		}
	}
	return channels, nil
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (s *Store) SaveSettings(ctx context.Context, values map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO app_settings(key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SavePushSubscription(ctx context.Context, subscription PushSubscription) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO push_subscriptions(endpoint, p256dh, auth, created_at, user_id)
VALUES (?, ?, ?, ?, NULLIF(?, ''))
ON CONFLICT(endpoint) DO UPDATE SET
  p256dh = excluded.p256dh,
  auth = excluded.auth,
  user_id = excluded.user_id
WHERE COALESCE(push_subscriptions.user_id, '') = COALESCE(excluded.user_id, '')
   OR (excluded.user_id IS NOT NULL AND (
        push_subscriptions.user_id IS NULL
        OR (push_subscriptions.p256dh = excluded.p256dh AND push_subscriptions.auth = excluded.auth)
      ))`,
		subscription.Endpoint, subscription.P256DH, subscription.Auth, now.UnixMilli(), subscription.UserID)
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
	return nil
}

func (s *Store) RemoveUserPushSubscription(ctx context.Context, userID, endpoint string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ? AND COALESCE(user_id, '') = ?`, endpoint, userID)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrInvalidCredentials
	}
	return nil
}

func (s *Store) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(user_id, ''), endpoint, p256dh, auth, created_at
FROM push_subscriptions
ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PushSubscription, 0)
	for rows.Next() {
		var subscription PushSubscription
		var createdAt int64
		if err := rows.Scan(&subscription.UserID, &subscription.Endpoint, &subscription.P256DH, &subscription.Auth, &createdAt); err != nil {
			return nil, err
		}
		subscription.CreatedAt = time.UnixMilli(createdAt).UTC()
		result = append(result, subscription)
	}
	return result, rows.Err()
}

func (s *Store) ListPushSubscriptionsForNotification(ctx context.Context, notificationID string, includeAnonymous bool) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(ps.user_id, ''), ps.endpoint, ps.p256dh, ps.auth, ps.created_at
FROM push_subscriptions ps
WHERE (? AND ps.user_id IS NULL) OR EXISTS (
  SELECT 1
  FROM users u
  JOIN notifications n ON n.id = ?
  JOIN channels c ON c.id = n.channel_id
  WHERE u.id = ps.user_id AND
  COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id=u.id AND p.channel_id=c.id), 'all') <> 'muted' AND
  (COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id=u.id AND p.channel_id=c.id), 'all') <> 'critical' OR (json_valid(n.card_json) AND json_extract(n.card_json, '$.severity') = 'critical')) AND (
    u.is_admin <> 0 OR c.visibility = 'public' OR EXISTS (
      SELECT 1 FROM channel_memberships m WHERE m.channel_id = c.id AND m.user_id = u.id
    )
  )
)
ORDER BY ps.created_at`, includeAnonymous, notificationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PushSubscription, 0)
	for rows.Next() {
		var subscription PushSubscription
		var createdAt int64
		if err := rows.Scan(&subscription.UserID, &subscription.Endpoint, &subscription.P256DH, &subscription.Auth, &createdAt); err != nil {
			return nil, err
		}
		subscription.CreatedAt = time.UnixMilli(createdAt).UTC()
		result = append(result, subscription)
	}
	return result, rows.Err()
}

// ListPushSubscriptionsForChannelMessage returns subscriptions for readers who
// currently have channel access and selected the "all" notification level.
// Critical-only is notification-card specific because ordinary messages carry
// no severity. The author is excluded from alerts for their own message.
func (s *Store) ListPushSubscriptionsForChannelMessage(ctx context.Context, messageID string) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ps.user_id, ps.endpoint, ps.p256dh, ps.auth, ps.created_at
FROM push_subscriptions ps
JOIN users u ON u.id = ps.user_id
JOIN channel_messages cm ON cm.id = ? AND cm.deleted_at IS NULL
JOIN channels c ON c.id = cm.channel_id
WHERE ps.user_id <> cm.author_user_id AND
  COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id=u.id AND p.channel_id=c.id), 'all') = 'all' AND
  (u.is_admin <> 0 OR c.visibility = 'public' OR EXISTS (
    SELECT 1 FROM channel_memberships m WHERE m.channel_id = c.id AND m.user_id = u.id
  ))
ORDER BY ps.created_at`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PushSubscription, 0)
	for rows.Next() {
		var subscription PushSubscription
		var createdAt int64
		if err := rows.Scan(&subscription.UserID, &subscription.Endpoint, &subscription.P256DH, &subscription.Auth, &createdAt); err != nil {
			return nil, err
		}
		subscription.CreatedAt = time.UnixMilli(createdAt).UTC()
		result = append(result, subscription)
	}
	return result, rows.Err()
}

// ListPushSubscriptionsForCommandResponse applies the ordinary-message push
// policy to shared command output. Ephemeral responses intentionally produce no
// push because their only reader is the invoker, who is also excluded from
// self-notifications.
func (s *Store) ListPushSubscriptionsForCommandResponse(ctx context.Context, responseID string) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ps.user_id, ps.endpoint, ps.p256dh, ps.auth, ps.created_at
FROM push_subscriptions ps
JOIN users u ON u.id = ps.user_id
JOIN slash_command_responses r ON r.id = ? AND r.response_type = 'in_channel'
JOIN slash_command_executions e ON e.id = r.execution_id
JOIN channels c ON c.id = e.channel_id
WHERE ps.user_id <> e.user_id AND
  COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id=u.id AND p.channel_id=c.id), 'all') = 'all' AND
  (u.is_admin <> 0 OR c.visibility = 'public' OR EXISTS (
    SELECT 1 FROM channel_memberships m WHERE m.channel_id = c.id AND m.user_id = u.id
  ))
ORDER BY ps.created_at`, responseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PushSubscription, 0)
	for rows.Next() {
		var subscription PushSubscription
		var createdAt int64
		if err := rows.Scan(&subscription.UserID, &subscription.Endpoint, &subscription.P256DH, &subscription.Auth, &createdAt); err != nil {
			return nil, err
		}
		subscription.CreatedAt = time.UnixMilli(createdAt).UTC()
		result = append(result, subscription)
	}
	return result, rows.Err()
}

func (s *Store) SetChannelNotificationPreference(ctx context.Context, actor User, channelID, level string) error {
	if level != "all" && level != "critical" && level != "muted" {
		return errors.New("invalid notification preference")
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO channel_notification_preferences(user_id,channel_id,level,updated_at)
SELECT ?,c.id,?,? FROM channels c
WHERE c.id=? AND (c.visibility='public' OR ? OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?))
ON CONFLICT(user_id,channel_id) DO UPDATE SET level=excluded.level,updated_at=excluded.updated_at`, actor.ID, level, time.Now().UTC().UnixMilli(), channelID, actor.IsAdmin, actor.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrForbidden
	}
	return nil
}

func (s *Store) ChannelNotificationPreference(ctx context.Context, actor User, channelID string) (string, error) {
	var level string
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id=? AND p.channel_id=c.id),'all')
FROM channels c WHERE c.id=? AND (c.visibility='public' OR ? OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?))`, actor.ID, channelID, actor.IsAdmin, actor.ID).Scan(&level)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrForbidden
	}
	return level, err
}

func (s *Store) SaveActionTarget(ctx context.Context, name, targetURL string, authCipher []byte, allowPrivate bool) (ActionTarget, error) {
	now := time.Now().UTC()
	id, err := newID("act_", now)
	if err != nil {
		return ActionTarget{}, err
	}
	allowPrivateValue := 0
	if allowPrivate {
		allowPrivateValue = 1
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO action_targets(id, name, url, auth_cipher, allow_private, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    url=?,
    auth_cipher=?,
    allow_private=?`,
		id, name, targetURL, authCipher, allowPrivateValue, now.UnixMilli(),
		targetURL, authCipher, allowPrivateValue)
	if err != nil {
		return ActionTarget{}, err
	}
	var target ActionTarget
	err = s.db.QueryRowContext(ctx, `SELECT id, name, url, auth_cipher, allow_private FROM action_targets WHERE name = ?`, name).Scan(&target.ID, &target.Name, &target.URL, &target.AuthCipher, &target.AllowPrivate)
	return target, err
}

func (s *Store) DeleteActionTarget(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM action_targets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNotificationNotFound
	}
	return nil
}

func (s *Store) ActionTargetByName(ctx context.Context, name string) (ActionTarget, error) {
	var target ActionTarget
	err := s.db.QueryRowContext(ctx, `SELECT id, name, url, auth_cipher, allow_private FROM action_targets WHERE name = ?`, name).Scan(&target.ID, &target.Name, &target.URL, &target.AuthCipher, &target.AllowPrivate)
	if errors.Is(err, sql.ErrNoRows) {
		return ActionTarget{}, ErrNotificationNotFound
	}
	return target, err
}

func (s *Store) ActionTargetByURL(ctx context.Context, targetURL string) (ActionTarget, error) {
	var target ActionTarget
	err := s.db.QueryRowContext(ctx, `SELECT id,name,url,auth_cipher,allow_private FROM action_targets WHERE url=?`, targetURL).Scan(&target.ID, &target.Name, &target.URL, &target.AuthCipher, &target.AllowPrivate)
	if errors.Is(err, sql.ErrNoRows) {
		return ActionTarget{}, ErrNotificationNotFound
	}
	return target, err
}

func (s *Store) NotificationCardForAction(ctx context.Context, notificationID string, actor User) (json.RawMessage, string, error) {
	var card json.RawMessage
	var state, channelID string
	err := s.db.QueryRowContext(ctx, `SELECT CAST(card_json AS BLOB), state, channel_id FROM notifications WHERE id = ?`, notificationID).Scan(&card, &state, &channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotificationNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if !actor.IsAdmin {
		var allowed bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channel_memberships WHERE user_id=? AND channel_id=? AND role IN ('operator','channel_admin'))`, actor.ID, channelID).Scan(&allowed); err != nil {
			return nil, "", err
		}
		if !allowed {
			return nil, "", ErrForbidden
		}
	}
	return card, state, nil
}

func (s *Store) MattermostActionForNotification(ctx context.Context, notificationID string, actor User) (MattermostActionSource, error) {
	var source MattermostActionSource
	err := s.db.QueryRowContext(ctx, `SELECT CAST(n.attachments_json AS BLOB),mp.id,n.channel_id FROM notifications n JOIN mattermost_posts mp ON mp.notification_id=n.id AND mp.root_id='' WHERE n.id=?`, notificationID).Scan(&source.Attachments, &source.PostID, &source.ChannelID)
	if errors.Is(err, sql.ErrNoRows) {
		return source, ErrNotificationNotFound
	}
	if err != nil {
		return source, err
	}
	if !actor.IsAdmin {
		var allowed bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channel_memberships WHERE user_id=? AND channel_id=? AND role IN('operator','channel_admin'))`, actor.ID, source.ChannelID).Scan(&allowed); err != nil {
			return source, err
		}
		if !allowed {
			return source, ErrForbidden
		}
	}
	return source, nil
}

func (s *Store) ImportSlashCommands(ctx context.Context, commands []SlashCommand) (created, existing int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	seen := map[string]bool{}
	for _, command := range commands {
		key := command.Team + "\x00" + command.Trigger
		if seen[key] {
			return 0, 0, fmt.Errorf("%w: duplicate slash command", ErrImportConflict)
		}
		seen[key] = true
		var current SlashCommand
		err := tx.QueryRowContext(ctx, `SELECT id,method,url,token_hash,allow_private,display_name,description,creator,autocomplete,autocomplete_hint,autocomplete_description,username,icon_url FROM slash_commands WHERE team_name=? AND trigger_word=?`, command.Team, command.Trigger).Scan(&current.ID, &current.Method, &current.URL, &current.TokenHash, &current.AllowPrivate, &current.DisplayName, &current.Description, &current.Creator, &current.Autocomplete, &current.AutocompleteHint, &current.AutocompleteDescription, &current.Username, &current.IconURL)
		if err == nil {
			if current.Method != command.Method || current.URL != command.URL || !equalBytes(current.TokenHash, command.TokenHash) || current.AllowPrivate != command.AllowPrivate || current.DisplayName != command.DisplayName || current.Description != command.Description || current.Creator != command.Creator || current.Autocomplete != command.Autocomplete || current.AutocompleteHint != command.AutocompleteHint || current.AutocompleteDescription != command.AutocompleteDescription || current.Username != command.Username || current.IconURL != command.IconURL {
				return 0, 0, fmt.Errorf("%w: slash command definition differs", ErrImportConflict)
			}
			existing++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, err
		}
		id, err := newID("cmd_", time.Now().UTC())
		if err != nil {
			return 0, 0, err
		}
		allowPrivate, autocomplete := 0, 0
		if command.AllowPrivate {
			allowPrivate = 1
		}
		if command.Autocomplete {
			autocomplete = 1
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO slash_commands(id,team_name,trigger_word,display_name,description,creator,method,url,token_cipher,token_hash,allow_private,autocomplete,autocomplete_hint,autocomplete_description,username,icon_url,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, command.Team, command.Trigger, command.DisplayName, command.Description, command.Creator, command.Method, command.URL, command.TokenCipher, command.TokenHash, allowPrivate, autocomplete, command.AutocompleteHint, command.AutocompleteDescription, command.Username, command.IconURL, time.Now().UTC().UnixMilli())
		if err != nil {
			return 0, 0, err
		}
		created++
	}
	err = tx.Commit()
	return
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

func (s *Store) SlashCommands(ctx context.Context, team string, actor User) ([]SlashCommand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT sc.id,sc.team_name,sc.trigger_word,sc.display_name,sc.description,sc.creator,sc.method,sc.url,sc.token_cipher,sc.token_hash,sc.allow_private,sc.autocomplete,sc.autocomplete_hint,sc.autocomplete_description,sc.username,sc.icon_url FROM slash_commands sc JOIN mattermost_channel_aliases a ON a.team_name=sc.team_name JOIN channels c ON c.id=a.channel_id WHERE sc.team_name=? AND (c.visibility='public' OR ? OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?)) ORDER BY sc.trigger_word`, team, actor.IsAdmin, actor.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SlashCommand
	for rows.Next() {
		var command SlashCommand
		if err := rows.Scan(&command.ID, &command.Team, &command.Trigger, &command.DisplayName, &command.Description, &command.Creator, &command.Method, &command.URL, &command.TokenCipher, &command.TokenHash, &command.AllowPrivate, &command.Autocomplete, &command.AutocompleteHint, &command.AutocompleteDescription, &command.Username, &command.IconURL); err != nil {
			return nil, err
		}
		result = append(result, command)
	}
	return result, rows.Err()
}

func (s *Store) SlashCommandForActor(ctx context.Context, team, trigger, channel string, actor User) (SlashCommand, string, error) {
	var command SlashCommand
	var channelID string
	err := s.db.QueryRowContext(ctx, `SELECT sc.id,sc.team_name,sc.trigger_word,sc.display_name,sc.description,sc.creator,sc.method,sc.url,sc.token_cipher,sc.token_hash,sc.allow_private,sc.autocomplete,sc.autocomplete_hint,sc.autocomplete_description,sc.username,sc.icon_url,c.id FROM slash_commands sc JOIN mattermost_channel_aliases a ON a.team_name=sc.team_name JOIN channels c ON c.id=a.channel_id WHERE sc.team_name=? AND sc.trigger_word=? AND a.channel_name=? AND (c.visibility='public' OR ? OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?))`, team, trigger, channel, actor.IsAdmin, actor.ID).Scan(&command.ID, &command.Team, &command.Trigger, &command.DisplayName, &command.Description, &command.Creator, &command.Method, &command.URL, &command.TokenCipher, &command.TokenHash, &command.AllowPrivate, &command.Autocomplete, &command.AutocompleteHint, &command.AutocompleteDescription, &command.Username, &command.IconURL, &channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return command, "", ErrNotificationNotFound
	}
	return command, channelID, err
}

func (s *Store) CreateSlashCommandExecution(ctx context.Context, execution SlashCommandExecution) (SlashCommandExecution, bool, error) {
	now := time.Now().UTC()
	id, err := newID("run_", now)
	if err != nil {
		return execution, false, err
	}
	execution.ID = id
	_, err = s.db.ExecContext(ctx, `INSERT INTO slash_command_executions(id,command_id,channel_id,user_id,text,response_token_hash,request_key,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, execution.ID, execution.CommandID, execution.ChannelID, execution.UserID, execution.Text, execution.ResponseTokenHash, execution.RequestKey, execution.ExpiresAt.UnixMilli(), now.UnixMilli())
	if err == nil {
		return execution, true, nil
	}
	var existing SlashCommandExecution
	var expiry int64
	queryErr := s.db.QueryRowContext(ctx, `SELECT id,command_id,channel_id,user_id,text,response_token_hash,request_key,expires_at FROM slash_command_executions WHERE request_key=?`, execution.RequestKey).Scan(&existing.ID, &existing.CommandID, &existing.ChannelID, &existing.UserID, &existing.Text, &existing.ResponseTokenHash, &existing.RequestKey, &expiry)
	if queryErr != nil {
		return execution, false, err
	}
	existing.ExpiresAt = time.UnixMilli(expiry).UTC()
	if existing.CommandID != execution.CommandID || existing.ChannelID != execution.ChannelID || existing.UserID != execution.UserID || existing.Text != execution.Text {
		return execution, false, ErrImportConflict
	}
	return existing, false, nil
}

func (s *Store) AddSlashCommandResponse(ctx context.Context, executionID, responseType, text string, payload json.RawMessage) (SlashCommandResponse, error) {
	now := time.UnixMilli(time.Now().UTC().UnixMilli()).UTC()
	id, err := newID("rsp_", now)
	if err != nil {
		return SlashCommandResponse{}, err
	}
	var userID string
	if err := s.db.QueryRowContext(ctx, `SELECT user_id FROM slash_command_executions WHERE id=?`, executionID).Scan(&userID); err != nil {
		return SlashCommandResponse{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO slash_command_responses(id,execution_id,user_id,response_type,text,payload_json,created_at) VALUES(?,?,?,?,?,?,?)`, id, executionID, userID, responseType, text, payload, now.UnixMilli())
	return SlashCommandResponse{ID: id, ExecutionID: executionID, UserID: userID, ResponseType: responseType, Text: text, Payload: payload, CreatedAt: now}, err
}

func (s *Store) UseSlashResponseToken(ctx context.Context, tokenHash string) (SlashCommandExecution, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SlashCommandExecution{}, err
	}
	defer tx.Rollback()
	var execution SlashCommandExecution
	var expiry int64
	var count int
	err = tx.QueryRowContext(ctx, `SELECT id,command_id,channel_id,user_id,text,expires_at,response_count FROM slash_command_executions WHERE response_token_hash=?`, tokenHash).Scan(&execution.ID, &execution.CommandID, &execution.ChannelID, &execution.UserID, &execution.Text, &expiry, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return execution, ErrNotificationNotFound
	}
	if err != nil {
		return execution, err
	}
	if time.Now().UTC().UnixMilli() > expiry || count >= 5 {
		return execution, ErrForbidden
	}
	if _, err = tx.ExecContext(ctx, `UPDATE slash_command_executions SET response_count=response_count+1 WHERE id=?`, execution.ID); err != nil {
		return execution, err
	}
	execution.ExpiresAt = time.UnixMilli(expiry).UTC()
	err = tx.Commit()
	return execution, err
}

func (s *Store) SlashCommandResponses(ctx context.Context, executionID string, actor User) ([]SlashCommandResponse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.execution_id,r.user_id,r.response_type,r.text,CAST(r.payload_json AS BLOB),r.created_at FROM slash_command_responses r JOIN slash_command_executions e ON e.id=r.execution_id JOIN channels c ON c.id=e.channel_id WHERE e.id=? AND (r.user_id=? OR (r.response_type='in_channel' AND (c.visibility='public' OR ? OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?)))) ORDER BY r.created_at,r.id`, executionID, actor.ID, actor.IsAdmin, actor.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SlashCommandResponse
	for rows.Next() {
		var value SlashCommandResponse
		var created int64
		if err := rows.Scan(&value.ID, &value.ExecutionID, &value.UserID, &value.ResponseType, &value.Text, &value.Payload, &created); err != nil {
			return nil, err
		}
		value.CreatedAt = time.UnixMilli(created).UTC()
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) ReserveActionExecution(ctx context.Context, execution ActionExecution) (ActionExecution, bool, error) {
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM action_executions WHERE operation_key=? AND status='running' AND created_at<?`, execution.Key, now.Add(-15*time.Minute).UnixMilli()); err != nil {
		return ActionExecution{}, false, err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO action_executions(operation_key,notification_id,action_index,user_id,status,created_at) VALUES (?,?,?,?, 'running', ?)`, execution.Key, execution.NotificationID, execution.ActionIndex, execution.UserID, now.UnixMilli())
	if err == nil {
		execution.Status = "running"
		return execution, true, nil
	}
	var existing ActionExecution
	queryErr := s.db.QueryRowContext(ctx, `SELECT operation_key,notification_id,action_index,user_id,status,response_text FROM action_executions WHERE operation_key=?`, execution.Key).Scan(&existing.Key, &existing.NotificationID, &existing.ActionIndex, &existing.UserID, &existing.Status, &existing.ResponseText)
	if queryErr != nil {
		return ActionExecution{}, false, err
	}
	if existing.NotificationID != execution.NotificationID || existing.ActionIndex != execution.ActionIndex || existing.UserID != execution.UserID {
		return ActionExecution{}, false, ErrImportConflict
	}
	return existing, false, nil
}

func (s *Store) CompleteActionExecution(ctx context.Context, key, status, responseText string, actor User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var notificationID string
	var actionIndex int
	if err := tx.QueryRowContext(ctx, `SELECT notification_id,action_index FROM action_executions WHERE operation_key=?`, key).Scan(&notificationID, &actionIndex); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE action_executions SET status=?,response_text=?,completed_at=? WHERE operation_key=?`, status, responseText, now.UnixMilli(), key); err != nil {
		return err
	}
	var currentState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM notifications WHERE id=?`, notificationID).Scan(&currentState); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"text": responseText})
	eventID, err := newID("evt_", now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(id,notification_id,state,raw_payload_json,created_at,actor_user_id) VALUES(?,?,?,?,?,?)`, eventID, notificationID, currentState, payload, now.UnixMilli(), actor.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notifications SET updated_at=? WHERE id=?`, now.UnixMilli(), notificationID); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestMattermostActionResults returns the latest completed result for each
// Mattermost attachment. A successful result wins over later failures so an
// already-completed approval cannot become executable again.
func (s *Store) LatestMattermostActionResults(ctx context.Context, notificationIDs []string) (map[string]map[int]MattermostActionResult, error) {
	results := make(map[string]map[int]MattermostActionResult)
	if len(notificationIDs) == 0 {
		return results, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(notificationIDs)), ",")
	args := make([]any, len(notificationIDs))
	for index, id := range notificationIDs {
		args[index] = id
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT e.notification_id,e.action_index,e.status,e.response_text,COALESCE(u.username,''),e.completed_at
FROM action_executions e LEFT JOIN users u ON u.id=e.user_id
WHERE e.notification_id IN (`+placeholders+`) AND e.action_index>=10000
  AND e.status IN ('succeeded','failed') AND e.completed_at IS NOT NULL
ORDER BY CASE WHEN e.status='succeeded' THEN 0 ELSE 1 END,e.completed_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var notificationID, status, responseText, actor string
		var combinedIndex int
		var completedAt int64
		if err := rows.Scan(&notificationID, &combinedIndex, &status, &responseText, &actor, &completedAt); err != nil {
			return nil, err
		}
		attachmentIndex := (combinedIndex - 10000) / 101
		actionIndex := (combinedIndex - 10000) % 101
		if results[notificationID] == nil {
			results[notificationID] = make(map[int]MattermostActionResult)
		}
		if _, exists := results[notificationID][attachmentIndex]; exists {
			continue
		}
		results[notificationID][attachmentIndex] = MattermostActionResult{
			Status: status, ResponseText: responseText, Actor: actor, ActionIndex: actionIndex,
			CompletedAt: time.UnixMilli(completedAt).UTC(),
		}
	}
	return results, rows.Err()
}

func newID(prefix string, now time.Time) (string, error) {
	var value [16]byte
	millis := uint64(now.UnixMilli())
	binary.BigEndian.PutUint64(value[:8], millis)
	if _, err := rand.Read(value[8:]); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return prefix + hex.EncodeToString(value[:]), nil
}
