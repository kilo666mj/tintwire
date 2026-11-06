package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ResolveBotChannel resolves a Mattermost compatibility channel reference for a
// bot. The reference may be a Tintwire channel id or a channel name within the
// bot's team. The resolved channel must be one the bot is explicitly permitted
// to use (administrator, its default channel, or a membership grant); otherwise
// it returns ErrForbidden or ErrNotificationNotFound.
func (s *Store) ResolveBotChannel(ctx context.Context, bot MattermostBot, channelRef string) (channelID, channelName string, err error) {
	if channelRef == "" {
		return "", "", ErrForbidden
	}
	if name, err := s.botChannelByID(ctx, bot, channelRef); err == nil {
		return channelRef, name, nil
	} else if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotificationNotFound) {
		return "", "", err
	}
	return s.botChannelByTeamAlias(ctx, bot, channelRef)
}

func (s *Store) botChannelByID(ctx context.Context, bot MattermostBot, channelID string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM channels WHERE id=?`, channelID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotificationNotFound
	}
	if err != nil {
		return "", err
	}
	if ok, err := s.BotChannelAuthorized(ctx, bot, channelID); err != nil {
		return "", err
	} else if !ok {
		return "", ErrForbidden
	}
	return name, nil
}

func (s *Store) botChannelByTeamAlias(ctx context.Context, bot MattermostBot, channelName string) (string, string, error) {
	var channelID, name string
	err := s.db.QueryRowContext(ctx, `SELECT a.channel_id, c.name FROM mattermost_channel_aliases a JOIN channels c ON c.id=a.channel_id WHERE a.team_name=? AND a.channel_name=?`, bot.TeamName, channelName).Scan(&channelID, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotificationNotFound
	}
	if err != nil {
		return "", "", err
	}
	if ok, err := s.BotChannelAuthorized(ctx, bot, channelID); err != nil {
		return "", "", err
	} else if !ok {
		return "", "", ErrForbidden
	}
	return channelID, name, nil
}

// CreateBotNotification records a new notification in a specific channel on
// behalf of a compatibility bot, without relying on the bot token's default
// channel binding. The bot's token-linked webhook id is used for provenance.
func (s *Store) CreateBotNotification(ctx context.Context, bot MattermostBot, channelID, channelName string, input IncomingNotification) (Notification, error) {
	if input.State == "" {
		input.State = "received"
	}
	if input.State != "received" && input.State != "firing" && input.State != "acknowledged" && input.State != "resolved" {
		return Notification{}, errors.New("unsupported notification state")
	}
	if input.Username == "" {
		input.Username = bot.User.Username
	}
	if len(input.Attachments) == 0 {
		input.Attachments = json.RawMessage("[]")
	}
	if bot.WebhookID == "" {
		return Notification{}, ErrWebhookNotFound
	}
	cardData := []byte(input.Card)
	if cardData == nil {
		cardData = []byte{}
	}
	username := input.Username
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Notification{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	id, err := newID("ntf_", now)
	if err != nil {
		return Notification{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO notifications(id, channel_id, webhook_id, text, username, icon_url, attachments_json, raw_payload_json, created_at, external_key, state, updated_at, card_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		id, channelID, bot.WebhookID, input.Text, username, input.IconURL,
		[]byte(input.Attachments), []byte(input.RawPayload), now.UnixMilli(),
		input.State, now.UnixMilli(), cardData); err != nil {
		return Notification{}, err
	}
	if err := insertNotificationEvent(ctx, tx, id, input.State, input.RawPayload, now); err != nil {
		return Notification{}, err
	}
	if err := s.appendReplicationOperation(ctx, tx, "notification.created", channelID, "bot", bot.WebhookID, map[string]any{
		"notification_id": id, "state": input.State, "text": input.Text,
		"username": username, "icon_url": input.IconURL, "card_json": string(cardData),
		"attachments_json": string(input.Attachments), "raw_payload_json": string(input.RawPayload),
		"channel_name": channelName, "created_at": now.UnixMilli(), "updated_at": now.UnixMilli(),
	}, now); err != nil {
		return Notification{}, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, err
	}
	return Notification{
		ID: id, ChannelID: channelID, ChannelName: channelName, Text: input.Text,
		Username: username, IconURL: input.IconURL, Attachments: input.Attachments,
		State: input.State, CreatedAt: now, UpdatedAt: now, Card: input.Card,
	}, nil
}

// MattermostPostChannel reports the Tintwire channel a Mattermost compatibility
// post belongs to.
func (s *Store) MattermostPostChannel(ctx context.Context, postID string) (string, error) {
	var channelID string
	err := s.db.QueryRowContext(ctx, `SELECT channel_id FROM mattermost_posts WHERE id=?`, postID).Scan(&channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotificationNotFound
	}
	return channelID, err
}

// GrantMattermostBotChannel adds an explicit bot-to-channel grant using the
// principal membership model, so a single bot identity may operate in multiple
// permitted channels within its team.
func (s *Store) GrantMattermostBotChannel(ctx context.Context, token, teamName, channelName string) error {
	hash := sha256.Sum256([]byte(token))
	return s.grantMattermostBotChannel(ctx, hash[:], teamName, channelName)
}

func (s *Store) grantMattermostBotChannel(ctx context.Context, hash []byte, teamName, channelName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var userID, botTeam string
	if err := tx.QueryRowContext(ctx, `SELECT user_id, team_name FROM mattermost_bot_tokens WHERE token_hash=?`, hash).Scan(&userID, &botTeam); errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidCredentials
	} else if err != nil {
		return err
	}
	if botTeam != teamName {
		return ErrForbidden
	}
	var channelID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name=?`, channelName).Scan(&channelID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotificationNotFound
	} else if err != nil {
		return err
	}
	var existingChannel string
	err = tx.QueryRowContext(ctx, `SELECT channel_id FROM mattermost_channel_aliases WHERE team_name=? AND channel_name=?`, teamName, channelName).Scan(&existingChannel)
	if err == nil && existingChannel != channelID {
		return ErrImportConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mattermost_channel_aliases(team_name, channel_name, channel_id) VALUES(?,?,?)`, teamName, channelName, channelID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO channel_memberships(user_id, channel_id, role, created_at) VALUES(?,?,'viewer',?) ON CONFLICT(user_id, channel_id) DO NOTHING`, userID, channelID, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// MattermostBotSummary describes a bot without its token. ID is the bot's
// linked incoming webhook, which shares the token hash.
type MattermostBotSummary struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Team      string    `json:"team"`
	Channel   string    `json:"channel"`
	Channels  []string  `json:"channels"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateMattermostBot creates a credential-less bot user (or reuses one left
// by a revoked bot), maps a newly generated token to it, and returns the token
// once. Existing human or agent users are never taken over.
func (s *Store) CreateMattermostBot(ctx context.Context, username, teamName, channelName string) (MattermostBotSummary, string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `SELECT u.id FROM users u WHERE u.username=?`, username).Scan(&userID)
	if err == nil {
		var reusable bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=? AND length(u.password_hash)=0 AND u.oidc_subject IS NULL
AND NOT EXISTS(SELECT 1 FROM agents a WHERE a.user_id=u.id) AND NOT EXISTS(SELECT 1 FROM mattermost_bot_tokens b WHERE b.user_id=u.id))`, userID).Scan(&reusable); err != nil {
			return MattermostBotSummary{}, "", err
		}
		if !reusable {
			return MattermostBotSummary{}, "", fmt.Errorf("%w: username is already in use", ErrImportConflict)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		now := time.Now().UTC()
		if userID, err = newID("usr_", now); err != nil {
			return MattermostBotSummary{}, "", err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at, is_admin) VALUES (?, ?, ?, ?, 0)`, userID, username, []byte{}, now.UnixMilli()); err != nil {
			return MattermostBotSummary{}, "", err
		}
	} else {
		return MattermostBotSummary{}, "", err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return MattermostBotSummary{}, "", err
	}
	token := "tmb_" + hex.EncodeToString(secret[:])
	if err := s.ImportMattermostBot(ctx, token, username, teamName, channelName); err != nil {
		return MattermostBotSummary{}, "", err
	}
	hash := sha256.Sum256([]byte(token))
	bots, err := s.listMattermostBots(ctx, `WHERE b.token_hash=?`, hash[:])
	if err != nil || len(bots) != 1 {
		return MattermostBotSummary{}, "", errors.Join(errors.New("created bot not found"), err)
	}
	return bots[0], token, nil
}

func (s *Store) ListMattermostBots(ctx context.Context) ([]MattermostBotSummary, error) {
	return s.listMattermostBots(ctx, ``)
}

func (s *Store) listMattermostBots(ctx context.Context, where string, args ...any) ([]MattermostBotSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,u.id,u.username,b.team_name,c.id,c.name,b.created_at FROM mattermost_bot_tokens b
JOIN users u ON u.id=b.user_id JOIN channels c ON c.id=b.channel_id JOIN webhooks w ON w.token_hash=b.token_hash `+where+` ORDER BY u.username, b.created_at`, args...)
	if err != nil {
		return nil, err
	}
	type botRow struct {
		summary           MattermostBotSummary
		userID, channelID string
	}
	var found []botRow
	for rows.Next() {
		var row botRow
		var created int64
		if err := rows.Scan(&row.summary.ID, &row.userID, &row.summary.Username, &row.summary.Team, &row.channelID, &row.summary.Channel, &created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		row.summary.CreatedAt = time.UnixMilli(created).UTC()
		found = append(found, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	bots := make([]MattermostBotSummary, 0, len(found))
	for _, row := range found {
		bot := row.summary
		bot.Channels = []string{}
		grants, err := s.db.QueryContext(ctx, `SELECT c.name FROM channel_memberships m JOIN channels c ON c.id=m.channel_id WHERE m.user_id=? AND c.id<>? ORDER BY c.name`, row.userID, row.channelID)
		if err != nil {
			return nil, err
		}
		for grants.Next() {
			var name string
			if err := grants.Scan(&name); err != nil {
				_ = grants.Close()
				return nil, err
			}
			bot.Channels = append(bot.Channels, name)
		}
		if err := errors.Join(grants.Err(), grants.Close()); err != nil {
			return nil, err
		}
		bots = append(bots, bot)
	}
	return bots, nil
}

// GrantMattermostBotChannelByID grants a channel within the bot's own team.
func (s *Store) GrantMattermostBotChannelByID(ctx context.Context, id, channelName string) error {
	var hash []byte
	var teamName string
	err := s.db.QueryRowContext(ctx, `SELECT b.token_hash,b.team_name FROM webhooks w JOIN mattermost_bot_tokens b ON b.token_hash=w.token_hash WHERE w.id=?`, id).Scan(&hash, &teamName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidCredentials
	} else if err != nil {
		return err
	}
	return s.grantMattermostBotChannel(ctx, hash, teamName, channelName)
}

// RevokeMattermostBot removes the bot token mapping and revokes its linked
// webhook. The user and its post history remain.
func (s *Store) RevokeMattermostBot(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var hash []byte
	if err := tx.QueryRowContext(ctx, `SELECT token_hash FROM webhooks WHERE id=?`, id).Scan(&hash); errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidCredentials
	} else if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM mattermost_bot_tokens WHERE token_hash=?`, hash)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return ErrInvalidCredentials
	}
	if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET revoked_at=COALESCE(revoked_at,?) WHERE id=?`, time.Now().UTC().UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}
