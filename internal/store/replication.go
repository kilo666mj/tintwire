package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var ErrReplicationGap = errors.New("replication sequence gap")
var ErrReplicationQuarantined = errors.New("replication operation quarantined")

const (
	replicationProtocolVersion = 1
	maxReplicationPayloadBytes = 1 << 20
)

var replicationIdentityPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{7,127}$`)

type ReplicationOperation struct {
	Version    int             `json:"version"`
	ClusterID  string          `json:"cluster_id"`
	Origin     string          `json:"origin"`
	Sequence   uint64          `json:"sequence"`
	PhysicalMS int64           `json:"physical_ms"`
	Logical    uint32          `json:"logical"`
	Kind       string          `json:"kind"`
	ChannelID  string          `json:"channel_id,omitempty"`
	ActorType  string          `json:"actor_type"`
	ActorID    string          `json:"actor_id"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

type ReplicationStatus struct {
	ClusterID        string            `json:"cluster_id"`
	NodeID           string            `json:"node_id"`
	ControlAuthority bool              `json:"control_authority"`
	Origins          map[string]uint64 `json:"origins"`
}

type ReplicationPeerStatus struct {
	Peer                string
	NodeID              string
	LastAttemptAt       time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures uint64
}

type ReplicationMetrics struct {
	Operations           map[string]uint64
	Quarantined          uint64
	Peers                []ReplicationPeerStatus
	SnapshotApplications uint64
	LastSnapshotAt       *time.Time
}

func (s *Store) ReplicationMetrics(ctx context.Context) (ReplicationMetrics, error) {
	metrics := ReplicationMetrics{Operations: make(map[string]uint64)}
	rows, err := s.db.QueryContext(ctx, `SELECT origin,MAX(sequence) FROM (SELECT origin,sequence FROM replication_operations UNION ALL SELECT origin,sequence FROM replication_cursors) GROUP BY origin`)
	if err != nil {
		return metrics, err
	}
	for rows.Next() {
		var origin string
		var count uint64
		if err := rows.Scan(&origin, &count); err != nil {
			rows.Close()
			return metrics, err
		}
		metrics.Operations[origin] = count
	}
	if err := rows.Close(); err != nil {
		return metrics, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM replication_quarantine`).Scan(&metrics.Quarantined); err != nil {
		return metrics, err
	}
	var snapshotAt sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(application_count),0),MAX(last_applied_at) FROM replication_snapshot_status`).Scan(&metrics.SnapshotApplications, &snapshotAt); err != nil {
		return metrics, err
	}
	if snapshotAt.Valid {
		value := time.UnixMilli(snapshotAt.Int64).UTC()
		metrics.LastSnapshotAt = &value
	}
	rows, err = s.db.QueryContext(ctx, `SELECT peer,node_id,last_attempt_at,last_success_at,consecutive_failures FROM replication_peer_status ORDER BY peer`)
	if err != nil {
		return metrics, err
	}
	defer rows.Close()
	for rows.Next() {
		var peer ReplicationPeerStatus
		var success sql.NullInt64
		var attempt int64
		if err := rows.Scan(&peer.Peer, &peer.NodeID, &attempt, &success, &peer.ConsecutiveFailures); err != nil {
			return metrics, err
		}
		peer.LastAttemptAt = time.UnixMilli(attempt).UTC()
		if success.Valid {
			value := time.UnixMilli(success.Int64).UTC()
			peer.LastSuccessAt = &value
		}
		metrics.Peers = append(metrics.Peers, peer)
	}
	return metrics, rows.Err()
}

func (s *Store) RecordReplicationPeerResult(ctx context.Context, peer, nodeID string, syncErr error) error {
	now := time.Now().UTC().UnixMilli()
	if syncErr == nil {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO replication_peer_status(peer,node_id,last_attempt_at,last_success_at,consecutive_failures,last_error)
VALUES(?,?,?,?,0,'')
ON CONFLICT(peer) DO UPDATE SET node_id=excluded.node_id,last_attempt_at=excluded.last_attempt_at,
last_success_at=excluded.last_success_at,consecutive_failures=0,last_error=''`, peer, nodeID, now, now)
		return err
	}
	message := syncErr.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO replication_peer_status(peer,node_id,last_attempt_at,consecutive_failures,last_error)
VALUES(?,?,?,1,?)
ON CONFLICT(peer) DO UPDATE SET node_id=CASE WHEN excluded.node_id='' THEN node_id ELSE excluded.node_id END,
last_attempt_at=excluded.last_attempt_at,consecutive_failures=consecutive_failures+1,last_error=excluded.last_error`, peer, nodeID, now, message)
	return err
}

func (s *Store) ReplicationStatus(ctx context.Context) (ReplicationStatus, error) {
	status := ReplicationStatus{ClusterID: s.replicationClusterID, NodeID: s.replicationNodeID, ControlAuthority: s.IsControlAuthority(), Origins: map[string]uint64{}}
	rows, err := s.db.QueryContext(ctx, `SELECT origin,MAX(sequence) FROM (SELECT origin,sequence FROM replication_operations UNION ALL SELECT origin,sequence FROM replication_cursors) GROUP BY origin`)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	for rows.Next() {
		var origin string
		var sequence uint64
		if err := rows.Scan(&origin, &sequence); err != nil {
			return status, err
		}
		status.Origins[origin] = sequence
	}
	return status, rows.Err()
}

func (s *Store) ReplicationCursor(ctx context.Context, origin string) (uint64, error) {
	var v uint64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM (
SELECT sequence FROM replication_cursors WHERE origin=?
UNION ALL SELECT sequence FROM replication_operations WHERE origin=?
)`, origin, origin).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func (s *Store) ConfigureReplication(clusterID, nodeID string) error {
	clusterID = strings.TrimSpace(clusterID)
	nodeID = strings.TrimSpace(nodeID)
	if clusterID == "" && nodeID == "" {
		return nil
	}
	if !replicationIdentityPattern.MatchString(clusterID) || !replicationIdentityPattern.MatchString(nodeID) {
		return errors.New("cluster-id and node-id must both be 8-128 URL-safe characters")
	}
	s.replicationClusterID = clusterID
	s.replicationNodeID = nodeID
	return nil
}

func (s *Store) appendReplicationOperation(ctx context.Context, tx *sql.Tx, kind, channelID, actorType, actorID string, payload any, now time.Time) error {
	if s.replicationClusterID == "" {
		return nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode replication operation: %w", err)
	}
	if len(encoded) > maxReplicationPayloadBytes {
		return errors.New("replication operation exceeds 1 MiB")
	}
	var previousSequence uint64
	var previousPhysical int64
	var previousLogical uint32
	err = tx.QueryRowContext(ctx, `
SELECT sequence,physical_ms,logical FROM (
  SELECT sequence,physical_ms,logical FROM replication_operations WHERE origin=?
  UNION ALL
  SELECT sequence,0,0 FROM replication_cursors WHERE origin=?
) ORDER BY sequence DESC,physical_ms DESC LIMIT 1`, s.replicationNodeID, s.replicationNodeID).Scan(&previousSequence, &previousPhysical, &previousLogical)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	physical := now.UTC().UnixMilli()
	logical := uint32(0)
	if previousPhysical >= physical {
		physical = previousPhysical
		logical = previousLogical + 1
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO replication_operations(
  cluster_id,origin,sequence,physical_ms,logical,kind,channel_id,actor_type,actor_id,payload_json,created_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, s.replicationClusterID, s.replicationNodeID, previousSequence+1,
		physical, logical, kind, channelID, actorType, actorID, encoded, now.UTC().UnixMilli())
	return err
}

// PruneReplicationOperations removes an origin's retained envelopes only after
// durably recording the covered cursor. Normalized effects remain available to
// snapshot bootstrap, and future local sequence allocation continues above the
// pruned range.
func (s *Store) PruneReplicationOperations(ctx context.Context, origin string, through uint64) error {
	if !replicationIdentityPattern.MatchString(origin) || through == 0 {
		return errors.New("invalid replication prune range")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var high uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM replication_operations WHERE origin=?`, origin).Scan(&high); err != nil {
		return err
	}
	if through > high {
		return errors.New("replication prune range exceeds retained high-water mark")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO replication_cursors(origin,sequence) VALUES(?,?) ON CONFLICT(origin) DO UPDATE SET sequence=MAX(sequence,excluded.sequence)`, origin, through); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM replication_operations WHERE origin=? AND sequence<=?`, origin, through); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReplicationOperations(ctx context.Context, origin string, after uint64, limit int) ([]ReplicationOperation, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT cluster_id,origin,sequence,physical_ms,logical,kind,channel_id,actor_type,actor_id,payload_json,created_at
FROM replication_operations WHERE origin=? AND sequence>? ORDER BY sequence LIMIT ?`, origin, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]ReplicationOperation, 0)
	for rows.Next() {
		operation := ReplicationOperation{Version: replicationProtocolVersion}
		var createdAt int64
		if err := rows.Scan(&operation.ClusterID, &operation.Origin, &operation.Sequence, &operation.PhysicalMS,
			&operation.Logical, &operation.Kind, &operation.ChannelID, &operation.ActorType, &operation.ActorID,
			&operation.Payload, &createdAt); err != nil {
			return nil, err
		}
		operation.CreatedAt = time.UnixMilli(createdAt).UTC()
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// ApplyReplicationOperations durably applies one contiguous origin range. The
// visible effect, received envelope, and cursor advance share one transaction.
func (s *Store) ApplyReplicationOperations(ctx context.Context, operations []ReplicationOperation) error {
	if len(operations) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	origin := operations[0].Origin
	var cursor uint64
	err = tx.QueryRowContext(ctx, `SELECT sequence FROM replication_cursors WHERE origin=?`, origin).Scan(&cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, op := range operations {
		if op.Origin != origin || op.Sequence == 0 {
			return errors.New("mixed or invalid replication range")
		}
		if op.Sequence <= cursor {
			var payload []byte
			var kind, cluster string
			if err := tx.QueryRowContext(ctx, `SELECT cluster_id,kind,payload_json FROM replication_operations WHERE origin=? AND sequence=?`, origin, op.Sequence).Scan(&cluster, &kind, &payload); err != nil {
				return err
			}
			if cluster != op.ClusterID || kind != op.Kind || !bytes.Equal(payload, op.Payload) {
				return errors.New("replication operation ID collision")
			}
			continue
		}
		if op.Sequence != cursor+1 {
			return ErrReplicationGap
		}
		if op.Version != replicationProtocolVersion || op.ClusterID != s.replicationClusterID {
			return s.quarantineReplication(ctx, tx, op, "unsupported version or cluster")
		}
		if len(op.Payload) > maxReplicationPayloadBytes || !json.Valid(op.Payload) {
			return s.quarantineReplication(ctx, tx, op, "invalid payload")
		}
		if err := applyReplicationEffect(ctx, tx, op); err != nil {
			return s.quarantineReplication(ctx, tx, op, err.Error())
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO replication_operations(cluster_id,origin,sequence,physical_ms,logical,kind,channel_id,actor_type,actor_id,payload_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, op.ClusterID, op.Origin, op.Sequence, op.PhysicalMS, op.Logical, op.Kind, op.ChannelID, op.ActorType, op.ActorID, []byte(op.Payload), op.CreatedAt.UnixMilli())
		if err != nil {
			return err
		}
		cursor = op.Sequence
		_, err = tx.ExecContext(ctx, `INSERT INTO replication_cursors(origin,sequence) VALUES(?,?) ON CONFLICT(origin) DO UPDATE SET sequence=excluded.sequence`, origin, cursor)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) quarantineReplication(ctx context.Context, tx *sql.Tx, op ReplicationOperation, reason string) error {
	b, _ := json.Marshal(op)
	_, err := tx.ExecContext(ctx, `INSERT INTO replication_quarantine(origin,sequence,reason,envelope_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(origin,sequence) DO NOTHING`, op.Origin, op.Sequence, reason, b, time.Now().UTC().UnixMilli())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return ErrReplicationQuarantined
}

type replicatedNotification struct {
	NotificationID  string `json:"notification_id"`
	State           string `json:"state"`
	Text            string `json:"text"`
	Username        string `json:"username"`
	IconURL         string `json:"icon_url"`
	CardJSON        string `json:"card_json"`
	AttachmentsJSON string `json:"attachments_json"`
	RawPayloadJSON  string `json:"raw_payload_json"`
	ExternalKey     string `json:"external_key"`
	ChannelName     string `json:"channel_name"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	EventID         string `json:"event_id"`
}

func applyReplicationEffect(ctx context.Context, tx *sql.Tx, op ReplicationOperation) error {
	var p replicatedNotification
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return err
	}
	if p.NotificationID == "" {
		return errors.New("missing notification ID")
	}
	switch op.Kind {
	case "notification.created":
		if p.ChannelName == "" {
			return errors.New("missing channel name")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO channels(id,name,created_at,display_name) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, op.ChannelID, p.ChannelName, p.CreatedAt, p.ChannelName)
		if err != nil {
			return err
		}
		h := sha256.Sum256([]byte(op.ActorID))
		_, err = tx.ExecContext(ctx, `INSERT INTO webhooks(id,token_hash,channel_id,created_at) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, op.ActorID, h[:], op.ChannelID, p.CreatedAt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,channel_id,webhook_id,text,username,icon_url,attachments_json,raw_payload_json,created_at,external_key,state,updated_at,card_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.NotificationID, op.ChannelID, op.ActorID, p.Text, p.Username, p.IconURL, []byte(p.AttachmentsJSON), []byte(p.RawPayloadJSON), p.CreatedAt, p.ExternalKey, p.State, p.UpdatedAt, []byte(p.CardJSON))
		if err == nil {
			return nil
		}
		if p.ExternalKey == "" {
			return err
		}
		var local ReplicationSnapshotNotification
		if queryErr := tx.QueryRowContext(ctx, `SELECT id,channel_id,webhook_id,text,username,icon_url,attachments_json,raw_payload_json,card_json,created_at,updated_at,external_key,state FROM notifications WHERE id=?`, p.NotificationID).Scan(&local.ID, &local.ChannelID, &local.WebhookID, &local.Text, &local.Username, &local.IconURL, &local.Attachments, &local.RawPayload, &local.Card, &local.CreatedAt, &local.UpdatedAt, &local.ExternalKey, &local.State); queryErr != nil {
			return err
		}
		if local.WebhookID != op.ActorID || local.ExternalKey != p.ExternalKey || p.NotificationID != notificationIDForExternalKey(op.ActorID, p.ExternalKey) {
			return errors.New("notification creation identity collision")
		}
		incoming := ReplicationSnapshotNotification{ID: p.NotificationID, ChannelID: op.ChannelID, WebhookID: op.ActorID, Text: p.Text, Username: p.Username, IconURL: p.IconURL, Attachments: json.RawMessage(p.AttachmentsJSON), RawPayload: json.RawMessage(p.RawPayloadJSON), Card: json.RawMessage(p.CardJSON), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, ExternalKey: p.ExternalKey, State: p.State}
		state := local.State
		if stateRank(incoming.State) > stateRank(state) {
			state = incoming.State
		}
		incomingWins := incoming.UpdatedAt > local.UpdatedAt || (incoming.UpdatedAt == local.UpdatedAt && bytes.Compare(snapshotNotificationContentKey(incoming), snapshotNotificationContentKey(local)) > 0)
		if incomingWins {
			_, err = tx.ExecContext(ctx, `UPDATE notifications SET channel_id=?,text=?,username=?,icon_url=?,attachments_json=?,raw_payload_json=?,card_json=?,created_at=MIN(created_at,?),updated_at=?,state=? WHERE id=?`, incoming.ChannelID, incoming.Text, incoming.Username, incoming.IconURL, snapshotBytes(incoming.Attachments), snapshotBytes(incoming.RawPayload), snapshotBytes(incoming.Card), incoming.CreatedAt, incoming.UpdatedAt, state, incoming.ID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE notifications SET created_at=MIN(created_at,?),state=? WHERE id=?`, incoming.CreatedAt, state, incoming.ID)
		}
		return err
	case "notification.updated":
		res, err := tx.ExecContext(ctx, `UPDATE notifications SET channel_id=?,text=?,username=?,icon_url=?,attachments_json=?,raw_payload_json=?,state=?,updated_at=?,card_json=? WHERE id=?`, op.ChannelID, p.Text, p.Username, p.IconURL, []byte(p.AttachmentsJSON), []byte(p.RawPayloadJSON), p.State, p.UpdatedAt, []byte(p.CardJSON), p.NotificationID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("notification update before creation")
		}
		return nil
	case "notification.state":
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM notifications WHERE id=?`, p.NotificationID).Scan(&current); err != nil {
			return err
		}
		if stateRank(p.State) > stateRank(current) {
			_, err := tx.ExecContext(ctx, `UPDATE notifications SET state=?,updated_at=MAX(updated_at,?) WHERE id=?`, p.State, p.UpdatedAt, p.NotificationID)
			return err
		}
		return nil
	default:
		return errors.New("unsupported operation kind")
	}
}

func stateRank(s string) int {
	switch s {
	case "received":
		return 0
	case "firing":
		return 1
	case "acknowledged":
		return 2
	case "resolved":
		return 3
	}
	return -1
}
