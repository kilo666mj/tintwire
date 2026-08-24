package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
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
	defer tx.Rollback()
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, botTeam string
	if err := tx.QueryRowContext(ctx, `SELECT user_id, team_name FROM mattermost_bot_tokens WHERE token_hash=?`, hash[:]).Scan(&userID, &botTeam); errors.Is(err, sql.ErrNoRows) {
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
