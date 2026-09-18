package store

import (
	"context"
	"errors"
	"time"
)

// Presence expires independently of credential use. It is shared by PostgreSQL
// nodes; legacy SQLite replication intentionally does not replay live presence.
const AgentPresenceTTL = 60 * time.Second
const agentPresenceSchema = `CREATE TABLE IF NOT EXISTS agent_presence (
 agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
 channel_id TEXT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
 state TEXT NOT NULL CHECK(state IN ('ready','busy','offline')),
 updated_at BIGINT NOT NULL,
 PRIMARY KEY(agent_id,channel_id)
);`

type AgentConversation struct {
	Name               string    `json:"name"`
	DisplayName        string    `json:"display_name"`
	Description        string    `json:"description"`
	Channel            string    `json:"channel"`
	ChannelDisplayName string    `json:"channel_display_name"`
	State              string    `json:"state"`
	AcceptsMessages    bool      `json:"accepts_messages"`
	LastSeenAt         time.Time `json:"last_seen_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

func (s *Store) ReportAgentPresence(ctx context.Context, agent Agent, channel, state string) error {
	if state != "ready" && state != "busy" && state != "offline" {
		return errors.New("invalid agent availability state")
	}
	channelID, err := s.ChannelIDByName(ctx, channel)
	if err != nil {
		return err
	}
	// Recheck current authority rather than trusting a cached principal or a prior heartbeat.
	var allowed bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents a JOIN users u ON u.id=a.user_id WHERE a.id=? AND a.enabled=1 AND u.disabled_at IS NULL AND (u.is_admin=1 OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.user_id=a.user_id AND m.channel_id=? AND m.role IN ('operator','channel_admin'))))`, agent.ID, channelID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_presence(agent_id,channel_id,state,updated_at) VALUES(?,?,?,?) ON CONFLICT(agent_id,channel_id) DO UPDATE SET state=excluded.state,updated_at=excluded.updated_at`, agent.ID, channelID, state, time.Now().UnixMilli())
	return err
}

// ListAgentConversations exposes no credentials, owners, or runtime session IDs.
// Only explicitly advertised conversation channels are returned, never every grant.
func (s *Store) ListAgentConversations(ctx context.Context, user User) ([]AgentConversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.name,a.display_name,a.description,c.name,c.display_name,p.state,p.updated_at
 FROM agent_presence p JOIN agents a ON a.id=p.agent_id JOIN users u ON u.id=a.user_id JOIN channels c ON c.id=p.channel_id
 WHERE a.enabled=1 AND u.disabled_at IS NULL
 AND (u.is_admin=1 OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.user_id=a.user_id AND m.channel_id=c.id AND m.role IN ('operator','channel_admin')))
 AND (? OR c.visibility='public' OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.user_id=? AND m.channel_id=c.id))
 ORDER BY a.display_name,a.name,c.name`, user.IsAdmin, user.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]AgentConversation, 0)
	now := time.Now()
	for rows.Next() {
		var entry AgentConversation
		var updated int64
		if err := rows.Scan(&entry.Name, &entry.DisplayName, &entry.Description, &entry.Channel, &entry.ChannelDisplayName, &entry.State, &updated); err != nil {
			return nil, err
		}
		entry.LastSeenAt = time.UnixMilli(updated).UTC()
		entry.ExpiresAt = entry.LastSeenAt.Add(AgentPresenceTTL)
		if !now.Before(entry.ExpiresAt) {
			entry.State = "offline"
		}
		entry.AcceptsMessages = entry.State != "offline"
		result = append(result, entry)
	}
	return result, rows.Err()
}
