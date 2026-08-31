package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// ErrMessageNotFound is returned when a channel message or reply does not exist.
var ErrMessageNotFound = errors.New("message not found")

const maxMessageLength = 4000

// ChannelMessage is a human-authored message or reply in a channel timeline.
// Top-level messages have an empty ParentID and a root_id equal to their own id.
// Replies carry the ParentID of the message they reply to and inherit its
// root_id, so a thread is always addressable by its root message id.
type ChannelMessage struct {
	ID           string    `json:"id"`
	ChannelID    string    `json:"channel_id"`
	ChannelName  string    `json:"channel_name,omitempty"`
	AuthorUserID string    `json:"author_user_id"`
	Author       string    `json:"author"`
	ParentID     string    `json:"parent_id,omitempty"`
	RootID       string    `json:"root_id,omitempty"`
	Text         string    `json:"text"`
	CreatedAt    time.Time `json:"created_at"`
	ReplyCount   int       `json:"reply_count,omitempty"`
}

// TimelineItem is one entry in the ordered merged channel timeline. It is
// either a human message or a notification card. The CreatedAtMilli and ID
// fields define the shared (created_at DESC, id DESC) ordering used across
// both source tables and by the opaque pagination cursor.
type TimelineItem struct {
	Kind           string               `json:"kind"` // "message" | "notification" | "command"
	Message        *ChannelMessage      `json:"message,omitempty"`
	Notification   *Notification        `json:"notification,omitempty"`
	Command        *CommandTimelineItem `json:"command,omitempty"`
	CreatedAtMilli int64                `json:"created_at"`
	ID             string               `json:"id"`
}

// CommandTimelineItem is a slash-command response rendered in the channel
// timeline. In-channel responses are visible to every channel reader, while
// ephemeral responses are visible only to the invoker (InvokerUserID). The
// response payload carries the command output and any attachments.
type CommandTimelineItem struct {
	ID            string          `json:"id"`
	ExecutionID   string          `json:"execution_id"`
	ChannelID     string          `json:"channel_id"`
	ChannelName   string          `json:"channel_name,omitempty"`
	ResponseType  string          `json:"response_type"`
	Text          string          `json:"text"`
	Username      string          `json:"username,omitempty"`
	AuthorIcon    string          `json:"icon_url,omitempty"`
	Props         json.RawMessage `json:"props,omitempty"`
	Attachments   json.RawMessage `json:"attachments,omitempty"`
	Author        string          `json:"author"`
	InvokerUserID string          `json:"invoker_user_id"`
	Invoker       string          `json:"invoker"`
	CreatedAt     time.Time       `json:"created_at"`
	// Payload is the raw command response and is never emitted to the client;
	// the server parses it into the safe fields above.
	Payload json.RawMessage `json:"-"`
}

type CreateMessageInput struct {
	ChannelID      string
	Text           string
	ParentID       string
	IdempotencyKey string
}

// channelReadable reports whether the user may read a channel. Administrators
// can read every channel. Non-administrators read public channels and any
// private channel they are explicitly a member of. An empty user (anonymous)
// may only read public channels.
func (s *Store) channelReadable(ctx context.Context, user User, channelID string) (bool, error) {
	var visibility string
	err := s.db.QueryRowContext(ctx, `SELECT visibility FROM channels WHERE id=?`, channelID).Scan(&visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotificationNotFound
	}
	if err != nil {
		return false, err
	}
	if user.IsAdmin {
		return true, nil
	}
	if user.ID == "" {
		return visibility == "public", nil
	}
	if visibility == "public" {
		return true, nil
	}
	var membership int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_memberships WHERE user_id=? AND channel_id=?`, user.ID, channelID).Scan(&membership); err != nil {
		return false, err
	}
	return membership > 0, nil
}

// ChannelIDByName resolves a channel id from its URL-safe name.
func (s *Store) ChannelIDByName(ctx context.Context, name string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM channels WHERE name=?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotificationNotFound
	}
	return id, err
}

// ChannelNameByID resolves a channel's URL-safe name from its id.
func (s *Store) ChannelNameByID(ctx context.Context, channelID string) (string, error) {
	return s.channelNameByID(ctx, channelID)
}

// ChannelReadable reports whether the user may read a channel (public, member,
// or administrator).
func (s *Store) ChannelReadable(ctx context.Context, user User, channelID string) (bool, error) {
	return s.channelReadable(ctx, user, channelID)
}

func (s *Store) channelNameByID(ctx context.Context, channelID string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM channels WHERE id=?`, channelID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotificationNotFound
	}
	return name, err
}

// CreateChannelMessage stores a human-authored message or reply. It enforces
// channel read authorization, validates the message, resolves the thread root
// for replies, and honors idempotency keys so a retried submission does not
// duplicate a message. The server derives channel and actor from authenticated
// state, so a client cannot post into an unreadable channel by supplying an
// arbitrary channel id.
func (s *Store) CreateChannelMessage(ctx context.Context, actor User, input CreateMessageInput) (ChannelMessage, error) {
	readable, err := s.channelReadable(ctx, actor, input.ChannelID)
	if err != nil {
		return ChannelMessage{}, err
	}
	if !readable {
		return ChannelMessage{}, ErrForbidden
	}
	return s.createChannelMessage(ctx, actor, input)
}

// createChannelMessage stores a message after its caller has established that
// the actor may write to the channel. Compatibility bots use this path after
// ResolveBotChannel has applied their separate channel grants.
func (s *Store) createChannelMessage(ctx context.Context, actor User, input CreateMessageInput) (ChannelMessage, error) {
	text := strings.TrimSpace(input.Text)
	if text == "" {
		return ChannelMessage{}, errors.New("message text is required")
	}
	if len(text) > maxMessageLength {
		return ChannelMessage{}, errors.New("message text is too long")
	}

	if input.IdempotencyKey != "" {
		if existing, err := s.messageByIdempotencyKey(ctx, actor.ID, input.ChannelID, input.IdempotencyKey); err == nil {
			return existing, nil
		}
	}

	now := time.Now().UTC()
	id, err := newID("msg_", now)
	if err != nil {
		return ChannelMessage{}, err
	}
	rootID := id
	if input.ParentID != "" {
		parent, err := s.messageByID(ctx, input.ParentID)
		if errors.Is(err, ErrMessageNotFound) {
			return ChannelMessage{}, ErrMessageNotFound
		}
		if err != nil {
			return ChannelMessage{}, err
		}
		if parent.ChannelID != input.ChannelID {
			return ChannelMessage{}, ErrForbidden
		}
		rootID = parent.RootID
		if rootID == "" {
			rootID = parent.ID
		}
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO channel_messages(id, channel_id, author_user_id, parent_id, root_id, text, idempotency_key, created_at, updated_at)
VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
		id, input.ChannelID, actor.ID, input.ParentID, rootID, text, input.IdempotencyKey, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		if input.IdempotencyKey != "" && IsAlreadyExists(err) {
			return s.messageByIdempotencyKey(ctx, actor.ID, input.ChannelID, input.IdempotencyKey)
		}
		return ChannelMessage{}, err
	}
	return s.messageByID(ctx, id)
}

func (s *Store) messageByID(ctx context.Context, id string) (ChannelMessage, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT m.id, m.channel_id, c.name, m.author_user_id, u.username, COALESCE(m.parent_id, ''), m.root_id, m.text, m.created_at,
       (SELECT COUNT(*) FROM channel_messages r WHERE r.root_id = m.id AND r.id <> m.id AND r.deleted_at IS NULL)
FROM channel_messages m
JOIN channels c ON c.id = m.channel_id
JOIN users u ON u.id = m.author_user_id
WHERE m.id = ? AND m.deleted_at IS NULL`, id)
	return scanChannelMessage(row)
}

func (s *Store) messageByIdempotencyKey(ctx context.Context, userID, channelID, key string) (ChannelMessage, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT m.id, m.channel_id, c.name, m.author_user_id, u.username, COALESCE(m.parent_id, ''), m.root_id, m.text, m.created_at,
       (SELECT COUNT(*) FROM channel_messages r WHERE r.root_id = m.id AND r.id <> m.id AND r.deleted_at IS NULL)
FROM channel_messages m
JOIN channels c ON c.id = m.channel_id
JOIN users u ON u.id = m.author_user_id
WHERE m.channel_id = ? AND m.author_user_id = ? AND m.idempotency_key = ? AND m.deleted_at IS NULL`, channelID, userID, key)
	return scanChannelMessage(row)
}

// ChannelMessageByID returns one live message after applying the same channel
// authorization as timeline and thread reads. This supports stable deep links
// without requiring the target to remain in the newest timeline page.
func (s *Store) ChannelMessageByID(ctx context.Context, actor User, id string) (ChannelMessage, error) {
	message, err := s.messageByID(ctx, id)
	if err != nil {
		return ChannelMessage{}, err
	}
	readable, err := s.channelReadable(ctx, actor, message.ChannelID)
	if err != nil {
		return ChannelMessage{}, err
	}
	if !readable {
		return ChannelMessage{}, ErrForbidden
	}
	return message, nil
}

func scanChannelMessage(row *sql.Row) (ChannelMessage, error) {
	var message ChannelMessage
	var createdAt int64
	if err := row.Scan(&message.ID, &message.ChannelID, &message.ChannelName, &message.AuthorUserID, &message.Author, &message.ParentID, &message.RootID, &message.Text, &createdAt, &message.ReplyCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelMessage{}, ErrMessageNotFound
		}
		return ChannelMessage{}, err
	}
	message.CreatedAt = time.UnixMilli(createdAt).UTC()
	return message, nil
}

func (s *Store) listChannelMessages(ctx context.Context, actor User, channelID, search string, unreadOnly bool, limit int, beforeAt int64, beforeID string) ([]ChannelMessage, error) {
	statement := `
SELECT m.id, m.channel_id, c.name, m.author_user_id, u.username, COALESCE(m.parent_id, ''), m.root_id, m.text, m.created_at,
       (SELECT COUNT(*) FROM channel_messages r WHERE r.root_id = m.id AND r.id <> m.id AND r.deleted_at IS NULL)
FROM channel_messages m
JOIN channels c ON c.id = m.channel_id
JOIN users u ON u.id = m.author_user_id
WHERE m.channel_id = ? AND m.deleted_at IS NULL`
	args := []any{channelID}
	if unreadOnly && actor.ID != "" {
		statement += ` AND m.author_user_id <> ? AND m.created_at > COALESCE((SELECT read_at FROM channel_read_state WHERE channel_id=m.channel_id AND user_id=?), 0)`
		args = append(args, actor.ID, actor.ID)
	}
	if search = strings.TrimSpace(search); search != "" {
		statement += ` AND (LOWER(m.text) LIKE LOWER(?) ESCAPE '\' OR LOWER(u.username) LIKE LOWER(?) ESCAPE '\')`
		pattern := "%" + escapeLike(search) + "%"
		args = append(args, pattern, pattern)
	}
	if beforeAt > 0 && beforeID != "" {
		statement += ` AND (m.created_at < ? OR (m.created_at = ? AND m.id < ?))`
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	statement += ` ORDER BY m.created_at DESC, m.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]ChannelMessage, 0)
	for rows.Next() {
		var message ChannelMessage
		var createdAt int64
		if err := rows.Scan(&message.ID, &message.ChannelID, &message.ChannelName, &message.AuthorUserID, &message.Author, &message.ParentID, &message.RootID, &message.Text, &createdAt, &message.ReplyCount); err != nil {
			return nil, err
		}
		message.CreatedAt = time.UnixMilli(createdAt).UTC()
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// ListChannelThread returns the replies, in chronological order, attached to a
// thread root. It enforces channel read authorization and verifies the root
// message belongs to the channel.
func (s *Store) ListChannelThread(ctx context.Context, actor User, channelID, rootID string) ([]ChannelMessage, error) {
	readable, err := s.channelReadable(ctx, actor, channelID)
	if err != nil {
		return nil, err
	}
	if !readable {
		return nil, ErrForbidden
	}
	if rootID != "" {
		root, err := s.messageByID(ctx, rootID)
		if err != nil {
			return nil, err
		}
		if root.ChannelID != channelID {
			return nil, ErrForbidden
		}
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.channel_id, c.name, m.author_user_id, u.username, COALESCE(m.parent_id, ''), m.root_id, m.text, m.created_at,
       (SELECT COUNT(*) FROM channel_messages r WHERE r.root_id = m.id AND r.id <> m.id AND r.deleted_at IS NULL)
FROM channel_messages m
JOIN channels c ON c.id = m.channel_id
JOIN users u ON u.id = m.author_user_id
WHERE m.channel_id = ? AND m.root_id = ? AND m.id <> ? AND m.deleted_at IS NULL
ORDER BY m.created_at ASC, m.id ASC`, channelID, rootID, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := make([]ChannelMessage, 0)
	for rows.Next() {
		var message ChannelMessage
		var createdAt int64
		if err := rows.Scan(&message.ID, &message.ChannelID, &message.ChannelName, &message.AuthorUserID, &message.Author, &message.ParentID, &message.RootID, &message.Text, &createdAt, &message.ReplyCount); err != nil {
			return nil, err
		}
		message.CreatedAt = time.UnixMilli(createdAt).UTC()
		replies = append(replies, message)
	}
	return replies, rows.Err()
}

// ListChannelTimeline returns a stable, ordered, merged page of a channel's
// notification cards and human messages. It enforces channel read authorization
// and applies keyset pagination over (created_at DESC, id DESC). It returns the
// first `limit` items plus a flag indicating whether a further page exists. The
// caller builds the opaque cursor from the last returned item when hasMore is
// true.
func (s *Store) ListChannelTimeline(ctx context.Context, actor User, channelID, search string, limit int, beforeAt int64, beforeID string) ([]TimelineItem, bool, error) {
	return s.ListChannelTimelineFiltered(ctx, actor, channelID, search, "", false, limit, beforeAt, beforeID)
}

// ListChannelTimelineFiltered applies an optional notification lifecycle filter
// to a channel timeline. When a lifecycle is selected, messages and command
// output are omitted because those entries do not have notification state.
func (s *Store) ListChannelTimelineFiltered(ctx context.Context, actor User, channelID, search, state string, unreadOnly bool, limit int, beforeAt int64, beforeID string) ([]TimelineItem, bool, error) {
	readable, err := s.channelReadable(ctx, actor, channelID)
	if err != nil {
		return nil, false, err
	}
	if !readable {
		return nil, false, ErrForbidden
	}
	channelName, err := s.channelNameByID(ctx, channelID)
	if err != nil {
		return nil, false, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	pageSize := limit + 1
	query := NotificationQuery{
		Channel:        channelName,
		Search:         search,
		Limit:          pageSize,
		BeforeAt:       beforeAt,
		BeforeID:       beforeID,
		UserID:         actor.ID,
		UserAdmin:      actor.IsAdmin,
		OrderByUpdated: true,
		UnreadOnly:     unreadOnly,
	}
	if state == "dismissed" {
		query.DismissedOnly = true
	} else {
		query.State = state
	}
	notifications, err := s.QueryNotifications(ctx, query)
	if err != nil {
		return nil, false, err
	}
	var messages []ChannelMessage
	var commands []CommandTimelineItem
	if state != "" {
		return mergeTimelinePage(notifications, messages, commands, limit), len(notifications) > limit, nil
	}
	messages, err = s.listChannelMessages(ctx, actor, channelID, search, unreadOnly, pageSize, beforeAt, beforeID)
	if err != nil {
		return nil, false, err
	}
	commands, err = s.listChannelCommandResponses(ctx, channelID, actor, search, unreadOnly, pageSize, beforeAt, beforeID)
	if err != nil {
		return nil, false, err
	}
	merged := mergeTimeline(notifications, messages, commands)
	hasMore := len(merged) > limit
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, hasMore, nil
}

func mergeTimelinePage(notifications []Notification, messages []ChannelMessage, commands []CommandTimelineItem, limit int) []TimelineItem {
	merged := mergeTimeline(notifications, messages, commands)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func mergeTimeline(notifications []Notification, messages []ChannelMessage, commands []CommandTimelineItem) []TimelineItem {
	items := make([]TimelineItem, 0, len(notifications)+len(messages)+len(commands))
	for i := range notifications {
		notification := notifications[i]
		items = append(items, TimelineItem{Kind: "notification", Notification: &notification, CreatedAtMilli: notification.UpdatedAt.UnixMilli(), ID: notification.ID})
	}
	for i := range messages {
		message := messages[i]
		items = append(items, TimelineItem{Kind: "message", Message: &message, CreatedAtMilli: message.CreatedAt.UnixMilli(), ID: message.ID})
	}
	for i := range commands {
		command := commands[i]
		items = append(items, TimelineItem{Kind: "command", Command: &command, CreatedAtMilli: command.CreatedAt.UnixMilli(), ID: command.ID})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAtMilli != items[j].CreatedAtMilli {
			return items[i].CreatedAtMilli > items[j].CreatedAtMilli
		}
		return items[i].ID > items[j].ID
	})
	return items
}

// listChannelCommandResponses returns the slash-command responses visible to the
// actor in a channel. In-channel responses are shared; ephemeral responses are
// limited to the user who invoked the command.
func (s *Store) listChannelCommandResponses(ctx context.Context, channelID string, actor User, search string, unreadOnly bool, limit int, beforeAt int64, beforeID string) ([]CommandTimelineItem, error) {
	statement := `
SELECT r.id, e.id, e.channel_id, c.name, r.response_type, r.text, r.payload_json,
       COALESCE(NULLIF(sc.username, ''), sc.display_name), e.user_id, inv.username, r.created_at
FROM slash_command_responses r
JOIN slash_command_executions e ON e.id = r.execution_id
JOIN slash_commands sc ON sc.id = e.command_id
JOIN channels c ON c.id = e.channel_id
JOIN users inv ON inv.id = e.user_id
WHERE e.channel_id = ? AND (r.response_type = 'in_channel' OR e.user_id = ?)`
	args := []any{channelID, actor.ID}
	if unreadOnly && actor.ID != "" {
		statement += ` AND r.response_type='in_channel' AND r.created_at > COALESCE((SELECT read_at FROM channel_read_state WHERE channel_id=e.channel_id AND user_id=?), 0)`
		args = append(args, actor.ID)
	}
	if search = strings.TrimSpace(search); search != "" {
		statement += ` AND (LOWER(r.text) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(NULLIF(sc.username, ''), sc.display_name)) LIKE LOWER(?) ESCAPE '\' OR LOWER(inv.username) LIKE LOWER(?) ESCAPE '\')`
		pattern := "%" + escapeLike(search) + "%"
		args = append(args, pattern, pattern, pattern)
	}
	if beforeAt > 0 && beforeID != "" {
		statement += ` AND (r.created_at < ? OR (r.created_at = ? AND r.id < ?))`
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	statement += ` ORDER BY r.created_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	commands := make([]CommandTimelineItem, 0)
	for rows.Next() {
		var item CommandTimelineItem
		var createdAt int64
		if err := rows.Scan(&item.ID, &item.ExecutionID, &item.ChannelID, &item.ChannelName, &item.ResponseType, &item.Text, &item.Payload, &item.Author, &item.InvokerUserID, &item.Invoker, &createdAt); err != nil {
			return nil, err
		}
		item.CreatedAt = time.UnixMilli(createdAt).UTC()
		commands = append(commands, item)
	}
	return commands, rows.Err()
}

// messageUnreadCount counts messages newer than each channel's read cursor for
// the channels the user can see. It mirrors the notification unread rule so a
// mixed timeline produces a single coherent unread number.
func (s *Store) messageUnreadCount(ctx context.Context, user User) (int, error) {
	var count int
	statement := `
SELECT COUNT(*) FROM channel_messages m
JOIN channels c ON c.id = m.channel_id
LEFT JOIN channel_read_state rs ON rs.channel_id = m.channel_id AND rs.user_id = NULLIF(?, '')
WHERE m.deleted_at IS NULL AND ? <> '' AND m.created_at > COALESCE(rs.read_at, 0)`
	statement += ` AND m.author_user_id <> ?`
	args := []any{user.ID, user.ID, user.ID}
	statement += ` AND COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id=? AND p.channel_id=c.id), 'all') <> 'muted'`
	args = append(args, user.ID)
	if user.ID != "" && !user.IsAdmin {
		statement += ` AND (c.visibility = 'public' OR EXISTS (SELECT 1 FROM channel_memberships mm WHERE mm.channel_id = c.id AND mm.user_id = ?))`
		args = append(args, user.ID)
	}
	if err := s.db.QueryRowContext(ctx, statement, args...).Scan(&count); err != nil {
		return 0, nil
	}
	return count, nil
}

type channelMessageCount struct {
	Total  int
	Unread int
}

func (s *Store) channelMessageCounts(ctx context.Context, user User) (map[string]channelMessageCount, error) {
	statement := `
SELECT c.id,
       COUNT(CASE WHEN m.id IS NOT NULL AND m.deleted_at IS NULL THEN 1 END),
       COUNT(CASE WHEN ? <> '' AND m.id IS NOT NULL AND m.deleted_at IS NULL AND m.author_user_id <> ? AND m.created_at > COALESCE(rs.read_at, 0) THEN 1 END)
FROM channels c
LEFT JOIN channel_messages m ON m.channel_id = c.id
LEFT JOIN channel_read_state rs ON rs.channel_id = c.id AND rs.user_id = NULLIF(?, '')
WHERE 1 = 1`
	args := []any{user.ID, user.ID, user.ID}
	if user.ID != "" && !user.IsAdmin {
		statement += ` AND (c.visibility = 'public' OR EXISTS (SELECT 1 FROM channel_memberships mm WHERE mm.channel_id = c.id AND mm.user_id = ?))`
		args = append(args, user.ID)
	}
	statement += ` GROUP BY c.id`
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]channelMessageCount)
	for rows.Next() {
		var id string
		var count channelMessageCount
		if err := rows.Scan(&id, &count.Total, &count.Unread); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

func (s *Store) commandUnreadCount(ctx context.Context, user User) (int, error) {
	if user.ID == "" {
		return 0, nil
	}
	statement := `
SELECT COUNT(*) FROM slash_command_responses r
JOIN slash_command_executions e ON e.id = r.execution_id
JOIN channels c ON c.id = e.channel_id
LEFT JOIN channel_read_state rs ON rs.channel_id = e.channel_id AND rs.user_id = ?
WHERE r.response_type = 'in_channel' AND r.created_at > COALESCE(rs.read_at, 0)`
	args := []any{user.ID}
	statement += ` AND COALESCE((SELECT p.level FROM channel_notification_preferences p WHERE p.user_id=? AND p.channel_id=c.id), 'all') <> 'muted'`
	args = append(args, user.ID)
	if !user.IsAdmin {
		statement += ` AND (c.visibility = 'public' OR EXISTS (SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?))`
		args = append(args, user.ID)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, statement, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) channelCommandCounts(ctx context.Context, user User) (map[string]channelMessageCount, error) {
	statement := `
SELECT c.id,
       COUNT(CASE WHEN r.id IS NOT NULL AND (r.response_type='in_channel' OR e.user_id=NULLIF(?, '')) THEN 1 END),
       COUNT(CASE WHEN r.id IS NOT NULL AND r.response_type='in_channel' AND ? <> '' AND r.created_at > COALESCE(rs.read_at, 0) THEN 1 END)
FROM channels c
LEFT JOIN slash_command_executions e ON e.channel_id=c.id
LEFT JOIN slash_command_responses r ON r.execution_id=e.id
LEFT JOIN channel_read_state rs ON rs.channel_id=c.id AND rs.user_id=NULLIF(?, '')
WHERE 1=1`
	args := []any{user.ID, user.ID, user.ID}
	if user.ID != "" && !user.IsAdmin {
		statement += ` AND (c.visibility='public' OR EXISTS (SELECT 1 FROM channel_memberships m WHERE m.channel_id=c.id AND m.user_id=?))`
		args = append(args, user.ID)
	}
	statement += ` GROUP BY c.id`
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]channelMessageCount)
	for rows.Next() {
		var channelID string
		var count channelMessageCount
		if err := rows.Scan(&channelID, &count.Total, &count.Unread); err != nil {
			return nil, err
		}
		counts[channelID] = count
	}
	return counts, rows.Err()
}
