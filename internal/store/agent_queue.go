package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type AgentCommand struct {
	CancelRequested bool   `json:"cancel_requested"`
	ID              string `json:"id"`
	ChannelID       string `json:"channel_id"`
	MessageID       string `json:"message_id"`
	AuthorID        string `json:"author_id"`
	Author          string `json:"author"`
	Text            string `json:"text"`
	State           string `json:"state"`
	TurnID          string `json:"turn_id,omitempty"`
	Result          string `json:"result,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

func (s *Store) AgentCommandPending(ctx context.Context, agent Agent, channel string) (bool, error) {
	channelID, err := s.ChannelIDByName(ctx, channel)
	if err != nil {
		return false, err
	}
	if err := s.RequireChannelOperator(ctx, User{ID: agent.UserID}, channelID); err != nil {
		return false, err
	}
	var bound bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_bindings WHERE channel_id=? AND agent_id=?)`, channelID, agent.ID).Scan(&bound); err != nil {
		return false, err
	}
	if !bound {
		return false, errors.New("an administrator must bind this channel to the agent first")
	}
	var pending bool
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT lease_until<=? FROM agent_commands WHERE channel_id=? AND agent_id=? AND state IN ('queued','running','cancelling','interrupted') ORDER BY created_at,id LIMIT 1),FALSE)`, time.Now().UnixMilli(), channelID, agent.ID).Scan(&pending)
	return pending, err
}

func (s *Store) BindAgent(ctx context.Context, user User, channelID, agentName string) error {
	if !user.IsAdmin {
		return ErrForbidden
	}
	a, err := s.AgentByName(ctx, agentName)
	if err != nil {
		return err
	}
	if !a.Enabled {
		return ErrForbidden
	}
	if err := s.RequireChannelOperator(ctx, User{ID: a.UserID}, channelID); err != nil {
		return err
	}
	// Bindings are deliberately immutable while installed. Rebinding an active
	// runtime could send pending human commands to a different session.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO agent_bindings(channel_id,agent_id,created_at) VALUES(?,?,?) ON CONFLICT(channel_id) DO NOTHING`, channelID, a.ID, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	var bound string
	if err := tx.QueryRowContext(ctx, `SELECT agent_id FROM agent_bindings WHERE channel_id=?`, channelID).Scan(&bound); err != nil {
		return err
	}
	if bound != a.ID {
		return ErrAlreadyExists
	}
	if count == 1 {
		if err := recordQueueControl(ctx, tx, user, "agent.binding.create", channelID+":"+a.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Enqueue a deliberate human message in the same transaction as its creation.
// Binding never imports historical messages, including equal-timestamp arrivals.
func enqueueCommand(ctx context.Context, tx *sql.Tx, messageID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_commands(id,channel_id,agent_id,message_id,author_id,state,created_at,updated_at)
 SELECT 'cmd_' || m.id,m.channel_id,b.agent_id,m.id,m.author_user_id,'queued',m.created_at,m.created_at
 FROM channel_messages m JOIN agent_bindings b ON b.channel_id=m.channel_id JOIN users u ON u.id=m.author_user_id
 WHERE m.id=? AND m.parent_id IS NULL AND m.deleted_at IS NULL AND u.disabled_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM agents a WHERE a.user_id=u.id)
 AND NOT EXISTS(SELECT 1 FROM mattermost_bot_tokens t WHERE t.user_id=u.id)
 AND (u.is_admin=1 OR EXISTS(SELECT 1 FROM channel_memberships cm WHERE cm.user_id=u.id AND cm.channel_id=m.channel_id AND cm.role IN ('operator','channel_admin')))
 ON CONFLICT(message_id) DO NOTHING`, messageID)
	return err
}

func (s *Store) ClaimAgentCommand(ctx context.Context, agent Agent, channel, owner string) (*AgentCommand, error) {
	if len(owner) < 8 || len(owner) > 128 {
		return nil, errors.New("invalid bridge lease owner")
	}
	channelID, err := s.ChannelIDByName(ctx, channel)
	if err != nil {
		return nil, err
	}
	if err := s.RequireChannelOperator(ctx, User{ID: agent.UserID}, channelID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Row lock serializes competing bridges for this binding on PostgreSQL.
	res, err := tx.ExecContext(ctx, `UPDATE agent_bindings SET created_at=created_at WHERE channel_id=? AND agent_id=?`, channelID, agent.ID)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, errors.New("an administrator must bind this channel to the agent first")
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE agent_commands SET state='cancelled',result='Author no longer has command permission.',updated_at=? WHERE channel_id=? AND state='queued' AND NOT EXISTS(SELECT 1 FROM users u WHERE u.id=agent_commands.author_id AND u.disabled_at IS NULL AND (u.is_admin=1 OR EXISTS(SELECT 1 FROM channel_memberships m WHERE m.user_id=u.id AND m.channel_id=agent_commands.channel_id AND m.role IN ('operator','channel_admin'))))`, now, channelID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_commands SET state='interrupted',updated_at=? WHERE channel_id=? AND state IN ('running','cancelling') AND lease_until<?`, now, channelID, now); err != nil {
		return nil, err
	}
	var id, state string
	var lease int64
	err = tx.QueryRowContext(ctx, `SELECT id,state,lease_until FROM agent_commands WHERE channel_id=? AND state IN ('queued','running','cancelling','interrupted') ORDER BY created_at,id LIMIT 1`, channelID).Scan(&id, &state, &lease)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if lease > now {
		return nil, tx.Commit()
	}
	if state == "queued" {
		state = "running"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_commands SET state=?,lease_owner=?,lease_until=?,updated_at=? WHERE id=?`, state, owner, now+60000, now, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	command, err := s.agentCommand(ctx, id)
	return &command, err
}

func (s *Store) agentCommand(ctx context.Context, id string) (AgentCommand, error) {
	var c AgentCommand
	err := s.db.QueryRowContext(ctx, `SELECT q.id,q.channel_id,q.message_id,q.author_id,u.username,m.text,q.state,q.turn_id,q.result,q.cancel_requested,q.created_at,q.updated_at FROM agent_commands q JOIN channel_messages m ON m.id=q.message_id JOIN users u ON u.id=q.author_id WHERE q.id=?`, id).Scan(&c.ID, &c.ChannelID, &c.MessageID, &c.AuthorID, &c.Author, &c.Text, &c.State, &c.TurnID, &c.Result, &c.CancelRequested, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (s *Store) RenewAgentCommand(ctx context.Context, agent Agent, id, owner, turnID string) (AgentCommand, error) {
	c, err := s.agentCommand(ctx, id)
	if err != nil {
		return c, err
	}
	if err := s.RequireChannelOperator(ctx, User{ID: agent.UserID}, c.ChannelID); err != nil {
		return c, err
	}
	if err := s.RequireChannelOperator(ctx, User{ID: c.AuthorID}, c.ChannelID); err != nil {
		return c, err
	}
	if len(turnID) > 200 {
		return c, errors.New("invalid turn id")
	}
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE agent_commands SET lease_until=?,turn_id=CASE WHEN ?='' THEN turn_id ELSE ? END,updated_at=? WHERE id=? AND agent_id=? AND lease_owner=? AND lease_until>? AND state IN ('running','cancelling','interrupted') AND (turn_id='' OR ?='' OR turn_id=?)`, now+60000, turnID, turnID, now, id, agent.ID, owner, now, turnID, turnID)
	if err != nil {
		return c, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return c, err
	}
	if n != 1 {
		return c, ErrForbidden
	}
	return s.agentCommand(ctx, id)
}

func (s *Store) CompleteAgentCommand(ctx context.Context, agent Agent, id, owner, state, result string) (string, error) {
	if state != "completed" && state != "failed" && state != "cancelled" {
		return "", ErrInvalidTransition
	}
	result = strings.TrimSpace(result)
	if len(result) > 4000 {
		return "", errors.New("result too long")
	}
	c, err := s.agentCommand(ctx, id)
	if err != nil {
		return "", err
	}
	if err := s.RequireChannelOperator(ctx, User{ID: agent.UserID}, c.ChannelID); err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE agent_commands SET state=?,result=?,updated_at=?,lease_until=0 WHERE id=? AND agent_id=? AND lease_owner=? AND lease_until>? AND state IN ('running','cancelling','interrupted')`, state, result, now, id, agent.ID, owner, now)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrForbidden
	}
	if result == "" {
		result = "Agent command " + state + "."
	}
	replyID := "msg_reply_" + id
	if _, err := tx.ExecContext(ctx, `INSERT INTO channel_messages(id,channel_id,author_user_id,parent_id,root_id,text,idempotency_key,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, replyID, c.ChannelID, agent.UserID, c.MessageID, c.MessageID, result, "agent-command:"+id, now, now); err != nil {
		return "", err
	}
	return replyID, tx.Commit()
}

func (s *Store) AgentCommands(ctx context.Context, user User, channelID string) ([]AgentCommand, error) {
	allowed, err := s.channelReadable(ctx, user, channelID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM agent_commands WHERE channel_id=? ORDER BY created_at DESC,id DESC LIMIT 100`, channelID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	values := []AgentCommand{}
	for _, id := range ids {
		c, err := s.agentCommand(ctx, id)
		if err != nil {
			return nil, err
		}
		c.TurnID = ""
		values = append(values, c)
	}
	return values, nil
}

func (s *Store) ControlAgentCommand(ctx context.Context, user User, id, action string) error {
	c, err := s.agentCommand(ctx, id)
	if err != nil {
		return err
	}
	if err := s.RequireChannelOperator(ctx, user, c.ChannelID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var res sql.Result
	switch action {
	case "cancel":
		res, err = tx.ExecContext(ctx, `UPDATE agent_commands SET state=CASE WHEN state='queued' THEN 'cancelled' ELSE 'cancelling' END,cancel_requested=1,updated_at=? WHERE id=? AND state IN ('queued','running','interrupted')`, time.Now().UnixMilli(), id)
	case "reconciled":
		res, err = tx.ExecContext(ctx, `UPDATE agent_commands SET state='cancelled',result='Human verified runtime stopped; command closed without replay.',updated_at=? WHERE id=? AND (state='interrupted' OR (state IN ('running','cancelling') AND lease_until<?))`, time.Now().UnixMilli(), id, time.Now().UnixMilli())
	default:
		return ErrInvalidTransition
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrInvalidTransition
	}
	if err := recordQueueControl(ctx, tx, user, "agent.command."+action, id); err != nil {
		return err
	}
	return tx.Commit()
}

func recordQueueControl(ctx context.Context, tx *sql.Tx, user User, action, detail string) error {
	now := time.Now()
	id, err := newID("audit_", now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_audit_events(id,actor_user_id,action,detail,created_at) VALUES(?,?,?,?,?)`, id, user.ID, action, detail, now.UnixMilli())
	return err
}
