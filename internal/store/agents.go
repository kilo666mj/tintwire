package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Agent credentials are a distinct credential class from reader sessions,
// channel publishing tokens, and bot tokens. They are only ever returned once,
// at registration, and are stored as SHA-256 hashes.
const agentTokenPrefix = "twa_"

var (
	ErrAgentNotFound = errors.New("agent not found")
	ErrRunNotFound   = errors.New("agent run not found")

	agentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,48}$`)
)

type Agent struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	DisplayName  string     `json:"display_name"`
	Description  string     `json:"description,omitempty"`
	Owner        string     `json:"owner"`
	Username     string     `json:"username"`
	Enabled      bool       `json:"enabled"`
	IsAdmin      bool       `json:"is_admin"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	Channels     []string   `json:"channels"`
	UserID       string     `json:"-"`
	OAuthSubject string     `json:"oauth_subject,omitempty"`
}

type CreateAgentInput struct {
	Name         string
	DisplayName  string
	Description  string
	OwnerUserID  string
	IsAdmin      bool
	OAuthSubject string
}

type AgentRun struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"-"`
	Agent     string    `json:"agent"`
	Initiator string    `json:"initiator,omitempty"`
	Purpose   string    `json:"purpose"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Effects   int       `json:"effects"`
}

type AgentRunEvent struct {
	ID             string    `json:"id"`
	Tool           string    `json:"tool"`
	Summary        string    `json:"summary"`
	NotificationID string    `json:"notification_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateAgent registers an agent together with its own principal user, so that
// channel membership, authorization, and activity attribution reuse exactly one
// mechanism. The principal cannot log in: it is stored without a password hash.
func (s *Store) CreateAgent(ctx context.Context, input CreateAgentInput) (Agent, string, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Description = strings.TrimSpace(input.Description)
	input.OAuthSubject = strings.TrimSpace(input.OAuthSubject)
	if !agentNamePattern.MatchString(input.Name) {
		return Agent{}, "", errors.New("agent name must be URL-safe lowercase text")
	}
	if input.DisplayName == "" {
		input.DisplayName = input.Name
	}
	if len(input.DisplayName) > 100 || len(input.Description) > 500 || len(input.OAuthSubject) > 255 {
		return Agent{}, "", errors.New("agent metadata is too long")
	}
	token, tokenHash, err := newAgentToken()
	if err != nil {
		return Agent{}, "", err
	}
	now := time.Now().UTC()
	agentID, err := newID("agt_", now)
	if err != nil {
		return Agent{}, "", err
	}
	userID, err := newID("usr_", now)
	if err != nil {
		return Agent{}, "", err
	}
	credentialID, err := newID("acr_", now)
	if err != nil {
		return Agent{}, "", err
	}
	username := "agent-" + input.Name
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, "", err
	}
	defer tx.Rollback()
	adminValue := 0
	if input.IsAdmin {
		adminValue = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at, is_admin) VALUES (?, ?, X'', ?, ?)`, userID, username, now.UnixMilli(), adminValue); err != nil {
		return Agent{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO agents(id, name, display_name, description, owner_user_id, user_id, enabled, created_at, oauth_subject)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, NULLIF(?,''))`, agentID, input.Name, input.DisplayName, input.Description, input.OwnerUserID, userID, now.UnixMilli(), input.OAuthSubject); err != nil {
		return Agent{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_credentials(id, agent_id, token_hash, created_at) VALUES (?, ?, ?, ?)`, credentialID, agentID, tokenHash, now.UnixMilli()); err != nil {
		return Agent{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, "", err
	}
	return Agent{
		ID: agentID, Name: input.Name, DisplayName: input.DisplayName, Description: input.Description,
		Username: username, Enabled: true, IsAdmin: input.IsAdmin, CreatedAt: now, OAuthSubject: strings.TrimSpace(input.OAuthSubject),
		UserID: userID, Channels: []string{},
	}, token, nil
}

func (s *Store) AgentForOAuthSubject(ctx context.Context, subject string) (Agent, User, error) {
	var agent Agent
	var user User
	var enabled int
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT a.id,a.name,a.display_name,a.description,a.user_id,a.oauth_subject,a.enabled,a.created_at,
       u.id,u.username,u.is_admin,COALESCE(owner.username,'')
FROM agents a JOIN users u ON u.id=a.user_id LEFT JOIN users owner ON owner.id=a.owner_user_id
WHERE a.oauth_subject=? AND a.revoked_at IS NULL`, subject).Scan(&agent.ID, &agent.Name, &agent.DisplayName, &agent.Description, &agent.UserID, &agent.OAuthSubject, &enabled, &createdAt, &user.ID, &user.Username, &user.IsAdmin, &agent.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, User{}, ErrInvalidCredentials
	}
	if err != nil || enabled == 0 {
		if err == nil {
			err = ErrInvalidCredentials
		}
		return Agent{}, User{}, err
	}
	agent.Enabled, agent.Username, agent.IsAdmin = true, user.Username, user.IsAdmin
	agent.CreatedAt = time.UnixMilli(createdAt).UTC()
	agent.Channels, err = s.agentChannels(ctx, agent.ID)
	return agent, user, err
}

func newAgentToken() (string, []byte, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", nil, err
	}
	token := agentTokenPrefix + hex.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(token))
	return token, hash[:], nil
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id, a.name, a.display_name, a.description, owner.username, principal.username,
       a.enabled, principal.is_admin, a.created_at, COALESCE(a.oauth_subject,''),
       (SELECT MAX(c.last_used_at) FROM agent_credentials c WHERE c.agent_id = a.id AND c.revoked_at IS NULL)
FROM agents a
JOIN users owner ON owner.id = a.owner_user_id
JOIN users principal ON principal.id = a.user_id
ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := make([]Agent, 0)
	for rows.Next() {
		agent := Agent{Channels: []string{}}
		var createdAt int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&agent.ID, &agent.Name, &agent.DisplayName, &agent.Description, &agent.Owner, &agent.Username, &agent.Enabled, &agent.IsAdmin, &createdAt, &agent.OAuthSubject, &lastUsed); err != nil {
			return nil, err
		}
		agent.CreatedAt = time.UnixMilli(createdAt).UTC()
		if lastUsed.Valid {
			at := time.UnixMilli(lastUsed.Int64).UTC()
			agent.LastUsedAt = &at
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range agents {
		channels, err := s.agentChannels(ctx, agents[index].ID)
		if err != nil {
			return nil, err
		}
		agents[index].Channels = channels
	}
	return agents, nil
}

func (s *Store) agentChannels(ctx context.Context, agentID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.name || ':' || m.role
FROM agents a
JOIN channel_memberships m ON m.user_id = a.user_id
JOIN channels c ON c.id = m.channel_id
WHERE a.id = ?
ORDER BY c.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	channels := make([]string, 0)
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			return nil, err
		}
		channels = append(channels, entry)
	}
	return channels, rows.Err()
}

func (s *Store) AgentByName(ctx context.Context, name string) (Agent, error) {
	agent := Agent{Channels: []string{}}
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT a.id, a.name, a.display_name, a.description, owner.username, principal.username,
       a.enabled, principal.is_admin, a.created_at, a.user_id, COALESCE(a.oauth_subject,'')
FROM agents a
JOIN users owner ON owner.id = a.owner_user_id
JOIN users principal ON principal.id = a.user_id
WHERE a.name = ?`, strings.TrimSpace(name)).Scan(&agent.ID, &agent.Name, &agent.DisplayName, &agent.Description,
		&agent.Owner, &agent.Username, &agent.Enabled, &agent.IsAdmin, &createdAt, &agent.UserID, &agent.OAuthSubject)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrAgentNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	agent.CreatedAt = time.UnixMilli(createdAt).UTC()
	agent.Channels, err = s.agentChannels(ctx, agent.ID)
	return agent, err
}

// AgentForToken resolves an agent credential to the agent and its principal
// user. Disabled agents and revoked credentials never authenticate.
func (s *Store) AgentForToken(ctx context.Context, token string) (Agent, User, error) {
	if !strings.HasPrefix(token, agentTokenPrefix) {
		return Agent{}, User{}, ErrInvalidCredentials
	}
	hash := sha256.Sum256([]byte(token))
	agent := Agent{Channels: []string{}}
	user := User{}
	var createdAt int64
	var credentialID string
	err := s.db.QueryRowContext(ctx, `
SELECT c.id, a.id, a.name, a.display_name, a.description, owner.username, principal.username,
       a.enabled, principal.is_admin, a.created_at, principal.id
FROM agent_credentials c
JOIN agents a ON a.id = c.agent_id
JOIN users owner ON owner.id = a.owner_user_id
JOIN users principal ON principal.id = a.user_id
WHERE c.token_hash = ? AND c.revoked_at IS NULL AND a.enabled = 1 AND a.revoked_at IS NULL`, hash[:]).
		Scan(&credentialID, &agent.ID, &agent.Name, &agent.DisplayName, &agent.Description, &agent.Owner,
			&agent.Username, &agent.Enabled, &agent.IsAdmin, &createdAt, &agent.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, User{}, ErrInvalidCredentials
	}
	if err != nil {
		return Agent{}, User{}, err
	}
	agent.CreatedAt = time.UnixMilli(createdAt).UTC()
	user = User{ID: agent.UserID, Username: agent.Username, IsAdmin: agent.IsAdmin}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_credentials SET last_used_at = ? WHERE id = ?`, time.Now().UTC().UnixMilli(), credentialID); err != nil {
		return Agent{}, User{}, err
	}
	return agent, user, nil
}

// RevokeAgent disables an agent and all of its credentials without touching its
// owner's session or any other agent's credentials.
func (s *Store) RevokeAgent(ctx context.Context, name string) error {
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var agentID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM agents WHERE name = ?`, strings.TrimSpace(name)).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAgentNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET enabled = 0, revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, now, agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_credentials SET revoked_at = COALESCE(revoked_at, ?) WHERE agent_id = ?`, now, agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runs SET state = 'cancelled', updated_at = ? WHERE agent_id = ? AND state = 'running'`, now, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// StartAgentRun opens a durable run. The initiator is derived from the
// authenticated caller; clients never supply an actor identity.
func (s *Store) StartAgentRun(ctx context.Context, agentID, initiatorUserID, purpose string) (AgentRun, error) {
	purpose = strings.TrimSpace(purpose)
	if purpose == "" || len(purpose) > 500 {
		return AgentRun{}, errors.New("run purpose is required and must be at most 500 characters")
	}
	now := time.Now().UTC()
	id, err := newID("run_", now)
	if err != nil {
		return AgentRun{}, err
	}
	var initiator any
	if initiatorUserID != "" {
		initiator = initiatorUserID
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO agent_runs(id, agent_id, initiator_user_id, purpose, state, created_at, updated_at)
VALUES (?, ?, ?, ?, 'running', ?, ?)`, id, agentID, initiator, purpose, now.UnixMilli(), now.UnixMilli()); err != nil {
		return AgentRun{}, err
	}
	return AgentRun{ID: id, AgentID: agentID, Purpose: purpose, State: "running", CreatedAt: now, UpdatedAt: now}, nil
}

// FinishAgentRun closes a run owned by the given agent. Terminal runs stay
// terminal so that a late tool call cannot reopen recorded history.
func (s *Store) FinishAgentRun(ctx context.Context, agentID, runID, state string) error {
	if state != "completed" && state != "failed" && state != "cancelled" {
		return ErrInvalidTransition
	}
	now := time.Now().UTC().UnixMilli()
	result, err := s.db.ExecContext(ctx, `UPDATE agent_runs SET state = ?, updated_at = ? WHERE id = ? AND agent_id = ? AND state = 'running'`, state, now, runID, agentID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runs WHERE id = ? AND agent_id = ?)`, runID, agentID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrRunNotFound
		}
		return ErrInvalidTransition
	}
	return nil
}

// RecordAgentRunEvent appends an externally visible effect to a run. Summaries
// explain the effect; model reasoning and full prompts are never stored.
func (s *Store) RecordAgentRunEvent(ctx context.Context, agentID, runID, tool, summary, notificationID string) error {
	if runID == "" {
		return nil
	}
	summary = strings.TrimSpace(summary)
	if len(summary) > 1000 {
		summary = summary[:1000]
	}
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM agent_runs WHERE id = ? AND agent_id = ?`, runID, agentID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRunNotFound
	}
	if err != nil {
		return err
	}
	if state != "running" {
		return ErrInvalidTransition
	}
	now := time.Now().UTC()
	id, err := newID("rev_", now)
	if err != nil {
		return err
	}
	var notification any
	if notificationID != "" {
		notification = notificationID
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_run_events(id, run_id, tool, summary, notification_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`, id, runID, tool, summary, notification, now.UnixMilli()); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE agent_runs SET updated_at = ? WHERE id = ?`, now.UnixMilli(), runID)
	return err
}

func (s *Store) ListAgentRuns(ctx context.Context, agentID string, limit int) ([]AgentRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, a.name, COALESCE(u.username, ''), r.purpose, r.state, r.created_at, r.updated_at,
       (SELECT COUNT(*) FROM agent_run_events e WHERE e.run_id = r.id)
FROM agent_runs r
JOIN agents a ON a.id = r.agent_id
LEFT JOIN users u ON u.id = r.initiator_user_id
WHERE r.agent_id = ?
ORDER BY r.created_at DESC, r.id DESC
LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]AgentRun, 0)
	for rows.Next() {
		var run AgentRun
		var createdAt, updatedAt int64
		if err := rows.Scan(&run.ID, &run.Agent, &run.Initiator, &run.Purpose, &run.State, &createdAt, &updatedAt, &run.Effects); err != nil {
			return nil, err
		}
		run.AgentID = agentID
		run.CreatedAt = time.UnixMilli(createdAt).UTC()
		run.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) ListAgentRunEvents(ctx context.Context, runID string) ([]AgentRunEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, tool, summary, COALESCE(notification_id, ''), created_at
FROM agent_run_events WHERE run_id = ? ORDER BY created_at, id LIMIT 500`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]AgentRunEvent, 0)
	for rows.Next() {
		var event AgentRunEvent
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.Tool, &event.Summary, &event.NotificationID, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = time.UnixMilli(createdAt).UTC()
		events = append(events, event)
	}
	return events, rows.Err()
}

// CreateFromAgent publishes into a channel the agent is explicitly allowed to
// publish to. Agents have no implicit access to any channel; installation
// administrators may publish anywhere.
func (s *Store) CreateFromAgent(ctx context.Context, agent Agent, channelName, runID string, input IncomingNotification) (Notification, error) {
	if input.State == "" {
		input.State = "received"
	}
	if input.State != "received" && input.State != "firing" {
		return Notification{}, errors.New("agents may publish received or firing notifications")
	}
	if len(input.Attachments) == 0 {
		input.Attachments = json.RawMessage("[]")
	}
	cardData := []byte(input.Card)
	if cardData == nil {
		cardData = []byte{}
	}
	if len(input.RawPayload) == 0 {
		input.RawPayload = json.RawMessage("{}")
	}
	username := input.Username
	if username == "" {
		username = agent.Name
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, err
	}
	defer tx.Rollback()

	var channelID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, strings.TrimSpace(channelName)).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrForbidden
	}
	if err != nil {
		return Notification{}, err
	}
	if !agent.IsAdmin {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channel_memberships WHERE user_id = ? AND channel_id = ? AND role IN ('operator','channel_admin'))`, agent.UserID, channelID).Scan(&allowed); err != nil {
			return Notification{}, err
		}
		if !allowed {
			return Notification{}, ErrForbidden
		}
	}
	if runID != "" {
		var runState string
		err := tx.QueryRowContext(ctx, `SELECT state FROM agent_runs WHERE id = ? AND agent_id = ?`, runID, agent.ID).Scan(&runState)
		if errors.Is(err, sql.ErrNoRows) {
			return Notification{}, ErrRunNotFound
		}
		if err != nil {
			return Notification{}, err
		}
		if runState != "running" {
			return Notification{}, ErrInvalidTransition
		}
	}

	now := time.Now().UTC()
	webhookID, err := agentWebhook(ctx, tx, channelID, now)
	if err != nil {
		return Notification{}, err
	}
	id, err := newID("ntf_", now)
	if err != nil {
		return Notification{}, err
	}
	var run any
	if runID != "" {
		run = runID
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO notifications(
    id, channel_id, webhook_id, text, username, icon_url,
    attachments_json, raw_payload_json, created_at, external_key, state, updated_at, card_json,
    agent_id, agent_run_id
) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, '', ?, ?, ?, ?, ?)`,
		id, channelID, webhookID, input.Text, username,
		[]byte(input.Attachments), []byte(input.RawPayload), now.UnixMilli(),
		input.State, now.UnixMilli(), cardData, agent.ID, run); err != nil {
		return Notification{}, err
	}
	if err := insertNotificationEvent(ctx, tx, id, input.State, input.RawPayload, now); err != nil {
		return Notification{}, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, err
	}
	return Notification{
		ID: id, ChannelID: channelID, ChannelName: strings.TrimSpace(channelName),
		Text: input.Text, Username: username, Attachments: input.Attachments,
		State: input.State, CreatedAt: now, UpdatedAt: now, Card: input.Card, Agent: agent.Name,
	}, nil
}

// agentWebhook returns the channel's internal publishing row used for
// credential-free agent writes. Its token hash is a random value that is never
// issued, so it cannot be used as a bearer publishing token.
func agentWebhook(ctx context.Context, tx *sql.Tx, channelID string, now time.Time) (string, error) {
	var webhookID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM webhooks WHERE channel_id = ? AND kind = 'agent'`, channelID).Scan(&webhookID)
	if err == nil {
		return webhookID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	webhookID, err = newID("whk_", now)
	if err != nil {
		return "", err
	}
	var unusable [32]byte
	if _, err := rand.Read(unusable[:]); err != nil {
		return "", err
	}
	hash := sha256.Sum256(unusable[:])
	if _, err := tx.ExecContext(ctx, `INSERT INTO webhooks(id, token_hash, channel_id, created_at, kind) VALUES (?, ?, ?, ?, 'agent')`, webhookID, hash[:], channelID, now.UnixMilli()); err != nil {
		return "", err
	}
	return webhookID, nil
}

// ReserveAgentToolInvocation makes a mutating tool call idempotent per agent.
// A repeated key with different arguments is a conflict rather than a silent
// replay of an unrelated result.
func (s *Store) ReserveAgentToolInvocation(ctx context.Context, agentID, key, tool string, fingerprint []byte) (result []byte, fresh bool, err error) {
	now := time.Now().UTC().UnixMilli()
	if _, err = s.db.ExecContext(ctx, `DELETE FROM agent_tool_invocations WHERE agent_id=? AND idempotency_key=? AND status='running' AND created_at<?`, agentID, key, now-int64((15*time.Minute)/time.Millisecond)); err != nil {
		return nil, false, err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO agent_tool_invocations(agent_id, idempotency_key, tool, request_fingerprint, status, result_json, created_at)
VALUES (?, ?, ?, ?, 'running', X'', ?)`, agentID, key, tool, fingerprint, now)
	if err == nil {
		return nil, true, nil
	}
	var storedTool, status string
	var storedFingerprint, storedResult []byte
	queryErr := s.db.QueryRowContext(ctx, `SELECT tool, request_fingerprint, status, result_json FROM agent_tool_invocations WHERE agent_id = ? AND idempotency_key = ?`, agentID, key).
		Scan(&storedTool, &storedFingerprint, &status, &storedResult)
	if queryErr != nil {
		return nil, false, err
	}
	if storedTool != tool || !equalBytes(storedFingerprint, fingerprint) {
		return nil, false, ErrImportConflict
	}
	if status != "completed" {
		return nil, false, ErrInvalidTransition
	}
	return storedResult, false, nil
}

func (s *Store) CompleteAgentToolInvocation(ctx context.Context, agentID, key string, result []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_tool_invocations SET status = 'completed', result_json = ? WHERE agent_id = ? AND idempotency_key = ?`, result, agentID, key)
	return err
}

// ReleaseAgentToolInvocation frees a reservation whose operation failed, so a
// retry with the same key can run again instead of being permanently stuck.
func (s *Store) ReleaseAgentToolInvocation(ctx context.Context, agentID, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_tool_invocations WHERE agent_id = ? AND idempotency_key = ? AND status = 'running'`, agentID, key)
	return err
}
