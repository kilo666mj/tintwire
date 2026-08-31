package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const replicationSnapshotVersion = 1

type ReplicationSnapshot struct {
	Version         int                               `json:"version"`
	ProtocolVersion int                               `json:"protocol_version"`
	ClusterID       string                            `json:"cluster_id"`
	Source          string                            `json:"source"`
	CreatedAt       time.Time                         `json:"created_at"`
	Cursors         map[string]uint64                 `json:"cursors"`
	Notifications   []ReplicationSnapshotNotification `json:"notifications"`
	Digest          string                            `json:"digest"`
}

type ReplicationSnapshotNotification struct {
	ID          string          `json:"id"`
	ChannelID   string          `json:"channel_id"`
	WebhookID   string          `json:"webhook_id"`
	Text        string          `json:"text"`
	Username    string          `json:"username"`
	IconURL     string          `json:"icon_url"`
	Attachments json.RawMessage `json:"attachments"`
	RawPayload  json.RawMessage `json:"raw_payload"`
	Card        json.RawMessage `json:"card"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
	ExternalKey string          `json:"external_key"`
	State       string          `json:"state"`
}

// BuildReplicationSnapshot exports only the multi-writer notification domain.
// Raft-controlled tables and node-local operational state are deliberately
// excluded so importing a snapshot cannot roll back credentials or local work.
func (s *Store) BuildReplicationSnapshot(ctx context.Context) (ReplicationSnapshot, error) {
	if s.replicationClusterID == "" || s.replicationNodeID == "" {
		return ReplicationSnapshot{}, errors.New("replication is not configured")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReplicationSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := ReplicationSnapshot{Version: replicationSnapshotVersion, ProtocolVersion: replicationProtocolVersion, ClusterID: s.replicationClusterID, Source: s.replicationNodeID, CreatedAt: time.Now().UTC(), Cursors: make(map[string]uint64), Notifications: make([]ReplicationSnapshotNotification, 0)}
	rows, err := tx.QueryContext(ctx, `SELECT origin,MAX(sequence) FROM (SELECT origin,sequence FROM replication_operations UNION ALL SELECT origin,sequence FROM replication_cursors) GROUP BY origin`)
	if err != nil {
		return ReplicationSnapshot{}, err
	}
	for rows.Next() {
		var origin string
		var sequence uint64
		if err := rows.Scan(&origin, &sequence); err != nil {
			_ = rows.Close()
			return ReplicationSnapshot{}, err
		}
		snapshot.Cursors[origin] = sequence
	}
	if err := rows.Close(); err != nil {
		return ReplicationSnapshot{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,channel_id,webhook_id,text,username,icon_url,attachments_json,raw_payload_json,card_json,created_at,updated_at,external_key,state FROM notifications ORDER BY id`)
	if err != nil {
		return ReplicationSnapshot{}, err
	}
	for rows.Next() {
		var notification ReplicationSnapshotNotification
		if err := rows.Scan(&notification.ID, &notification.ChannelID, &notification.WebhookID, &notification.Text, &notification.Username, &notification.IconURL, &notification.Attachments, &notification.RawPayload, &notification.Card, &notification.CreatedAt, &notification.UpdatedAt, &notification.ExternalKey, &notification.State); err != nil {
			_ = rows.Close()
			return ReplicationSnapshot{}, err
		}
		snapshot.Notifications = append(snapshot.Notifications, notification)
	}
	if err := rows.Close(); err != nil {
		return ReplicationSnapshot{}, err
	}
	snapshot.Digest, err = replicationSnapshotDigest(snapshot)
	return snapshot, err
}

// ApplyReplicationSnapshot merges normalized notification state and advances
// covered cursors atomically. It never deletes local records or replication
// envelopes and never changes control-plane tables.
func (s *Store) ApplyReplicationSnapshot(ctx context.Context, snapshot ReplicationSnapshot) error {
	if snapshot.Version != replicationSnapshotVersion || snapshot.ProtocolVersion != replicationProtocolVersion || snapshot.ClusterID != s.replicationClusterID || !replicationIdentityPattern.MatchString(snapshot.Source) {
		return errors.New("invalid replication snapshot identity")
	}
	digest := snapshot.Digest
	snapshot.Digest = ""
	want, err := replicationSnapshotDigest(snapshot)
	if err != nil || digest == "" || digest != want {
		return errors.New("invalid replication snapshot digest")
	}
	if snapshot.CreatedAt.After(time.Now().UTC().Add(2 * time.Minute)) {
		return errors.New("replication snapshot timestamp is in the future")
	}
	for origin, sequence := range snapshot.Cursors {
		if !replicationIdentityPattern.MatchString(origin) || sequence == 0 {
			return errors.New("invalid replication snapshot cursor")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	seen := make(map[string]bool, len(snapshot.Notifications))
	for _, incoming := range snapshot.Notifications {
		if incoming.ID == "" || incoming.ChannelID == "" || incoming.WebhookID == "" || incoming.CreatedAt <= 0 || incoming.UpdatedAt < incoming.CreatedAt || stateRank(incoming.State) < 0 || !validSnapshotJSON(incoming.Attachments) || !validSnapshotJSON(incoming.RawPayload) || !validSnapshotJSON(incoming.Card) || seen[incoming.ID] {
			return errors.New("invalid replication snapshot notification")
		}
		seen[incoming.ID] = true
		var local ReplicationSnapshotNotification
		err := tx.QueryRowContext(ctx, `SELECT id,channel_id,webhook_id,text,username,icon_url,attachments_json,raw_payload_json,card_json,created_at,updated_at,external_key,state FROM notifications WHERE id=?`, incoming.ID).Scan(&local.ID, &local.ChannelID, &local.WebhookID, &local.Text, &local.Username, &local.IconURL, &local.Attachments, &local.RawPayload, &local.Card, &local.CreatedAt, &local.UpdatedAt, &local.ExternalKey, &local.State)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,channel_id,webhook_id,text,username,icon_url,attachments_json,raw_payload_json,card_json,created_at,updated_at,external_key,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, incoming.ID, incoming.ChannelID, incoming.WebhookID, incoming.Text, incoming.Username, incoming.IconURL, snapshotBytes(incoming.Attachments), snapshotBytes(incoming.RawPayload), snapshotBytes(incoming.Card), incoming.CreatedAt, incoming.UpdatedAt, incoming.ExternalKey, incoming.State)
			if err != nil {
				return fmt.Errorf("insert replication snapshot notification: %w", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		if local.WebhookID != incoming.WebhookID || local.ExternalKey != incoming.ExternalKey {
			return errors.New("replication snapshot notification identity collision")
		}
		state := local.State
		if stateRank(incoming.State) > stateRank(state) {
			state = incoming.State
		}
		incomingWins := incoming.UpdatedAt > local.UpdatedAt
		if incoming.UpdatedAt == local.UpdatedAt {
			incomingWins = bytes.Compare(snapshotNotificationContentKey(incoming), snapshotNotificationContentKey(local)) > 0
		}
		if incomingWins {
			_, err = tx.ExecContext(ctx, `UPDATE notifications SET channel_id=?,text=?,username=?,icon_url=?,attachments_json=?,raw_payload_json=?,card_json=?,created_at=MIN(created_at,?),updated_at=?,state=? WHERE id=?`, incoming.ChannelID, incoming.Text, incoming.Username, incoming.IconURL, snapshotBytes(incoming.Attachments), snapshotBytes(incoming.RawPayload), snapshotBytes(incoming.Card), incoming.CreatedAt, incoming.UpdatedAt, state, incoming.ID)
		} else if state != local.State {
			_, err = tx.ExecContext(ctx, `UPDATE notifications SET created_at=MIN(created_at,?),state=? WHERE id=?`, incoming.CreatedAt, state, incoming.ID)
		} else if incoming.CreatedAt < local.CreatedAt {
			_, err = tx.ExecContext(ctx, `UPDATE notifications SET created_at=? WHERE id=?`, incoming.CreatedAt, incoming.ID)
		}
		if err != nil {
			return err
		}
	}
	for origin, sequence := range snapshot.Cursors {
		_, err := tx.ExecContext(ctx, `INSERT INTO replication_cursors(origin,sequence) VALUES(?,?) ON CONFLICT(origin) DO UPDATE SET sequence=MAX(sequence,excluded.sequence)`, origin, sequence)
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO replication_snapshot_status(source,last_applied_at,application_count,notification_count) VALUES(?,?,1,?) ON CONFLICT(source) DO UPDATE SET last_applied_at=excluded.last_applied_at,application_count=application_count+1,notification_count=excluded.notification_count`, snapshot.Source, time.Now().UTC().UnixMilli(), len(snapshot.Notifications)); err != nil {
		return err
	}
	return tx.Commit()
}

func snapshotNotificationContentKey(value ReplicationSnapshotNotification) []byte {
	encoded, _ := json.Marshal([]any{value.ChannelID, value.Text, value.Username, value.IconURL, string(value.Attachments), string(value.RawPayload), string(value.Card)})
	return encoded
}

func validSnapshotJSON(value json.RawMessage) bool {
	return len(value) == 0 || json.Valid(value)
}

func snapshotBytes(value json.RawMessage) []byte {
	if value == nil {
		return []byte{}
	}
	return []byte(value)
}

func replicationSnapshotDigest(snapshot ReplicationSnapshot) (string, error) {
	snapshot.Digest = ""
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
