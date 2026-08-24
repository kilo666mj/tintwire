package store

import (
	"context"
	"database/sql"
	"errors"
)

// TeamForChannel resolves the Mattermost team alias that a channel is mapped
// to. A channel must be aliased to a team for team-scoped slash commands and
// bot compatibility. It returns ErrNotificationNotFound when the channel has no
// team alias.
func (s *Store) TeamForChannel(ctx context.Context, channelID string) (string, error) {
	var team string
	err := s.db.QueryRowContext(ctx, `SELECT team_name FROM mattermost_channel_aliases WHERE channel_id=? LIMIT 1`, channelID).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotificationNotFound
	}
	return team, err
}

// SlashCommandForChannel resolves a team-scoped command for a selected channel.
// An explicit compatibility alias selects that channel's team. Channels without
// an alias may use a trigger when it is defined by exactly one imported team;
// this matches Mattermost's team-wide command availability without guessing
// when multiple teams define the same trigger. Channel read authorization is
// enforced separately by the caller.
func (s *Store) SlashCommandForChannel(ctx context.Context, channelID, trigger string, _ User) (SlashCommand, error) {
	team, err := s.TeamForChannel(ctx, channelID)
	if err != nil && !errors.Is(err, ErrNotificationNotFound) {
		return SlashCommand{}, err
	}
	var command SlashCommand
	if err == nil {
		err = s.db.QueryRowContext(ctx, `
SELECT sc.id, sc.team_name, sc.trigger_word, sc.display_name, sc.description, sc.creator, sc.method, sc.url, sc.token_cipher, sc.token_hash, sc.allow_private, sc.autocomplete, sc.autocomplete_hint, sc.autocomplete_description, sc.username, sc.icon_url
FROM slash_commands sc
WHERE sc.team_name = ? AND sc.trigger_word = ?`, team, trigger).Scan(
			&command.ID, &command.Team, &command.Trigger, &command.DisplayName, &command.Description,
			&command.Creator, &command.Method, &command.URL, &command.TokenCipher, &command.TokenHash,
			&command.AllowPrivate, &command.Autocomplete, &command.AutocompleteHint,
			&command.AutocompleteDescription, &command.Username, &command.IconURL)
	} else {
		var matches int
		err = s.db.QueryRowContext(ctx, `
SELECT sc.id, sc.team_name, sc.trigger_word, sc.display_name, sc.description, sc.creator, sc.method, sc.url, sc.token_cipher, sc.token_hash, sc.allow_private, sc.autocomplete, sc.autocomplete_hint, sc.autocomplete_description, sc.username, sc.icon_url, COUNT(*) OVER()
FROM slash_commands sc
WHERE sc.trigger_word = ?
LIMIT 1`, trigger).Scan(
			&command.ID, &command.Team, &command.Trigger, &command.DisplayName, &command.Description,
			&command.Creator, &command.Method, &command.URL, &command.TokenCipher, &command.TokenHash,
			&command.AllowPrivate, &command.Autocomplete, &command.AutocompleteHint,
			&command.AutocompleteDescription, &command.Username, &command.IconURL, &matches)
		if err == nil && matches != 1 {
			return SlashCommand{}, ErrNotificationNotFound
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return command, ErrNotificationNotFound
	}
	return command, err
}

// BotChannelAuthorized reports whether a compatibility bot may access the given
// Tintwire channel. Administrators and the bot's default channel are always
// authorized; other channels require an explicit membership grant.
func (s *Store) BotChannelAuthorized(ctx context.Context, bot MattermostBot, channelID string) (bool, error) {
	if bot.User.IsAdmin {
		return true, nil
	}
	if channelID == bot.ChannelID {
		return true, nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_memberships WHERE user_id=? AND channel_id=?`, bot.User.ID, channelID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
