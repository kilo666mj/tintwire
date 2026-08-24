package replicationhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

const maxResponse = 4 << 20
const maxControlSnapshot = 16 << 20
const maxReplicationSnapshot = 64 << 20

func Handler(data *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		status, err := data.ReplicationStatus(r.Context())
		if err != nil {
			http.Error(w, "status unavailable", 500)
			return
		}
		writeJSON(w, status)
	})
	mux.HandleFunc("GET /v1/operations", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, err := strconv.ParseUint(q.Get("after"), 10, 64)
		if err != nil && q.Get("after") != "" {
			http.Error(w, "invalid cursor", 400)
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		ops, err := data.ReplicationOperations(r.Context(), q.Get("origin"), after, limit)
		if err != nil {
			http.Error(w, "operations unavailable", 500)
			return
		}
		for len(ops) > 0 {
			encoded, _ := json.Marshal(map[string]any{"operations": ops})
			if len(encoded) <= maxResponse {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(encoded)
				return
			}
			ops = ops[:len(ops)-1]
		}
		writeJSON(w, map[string]any{"operations": ops})
	})
	mux.HandleFunc("GET /v1/control-snapshot", func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := data.BuildControlSnapshot(r.Context())
		if errors.Is(err, store.ErrForbidden) {
			http.Error(w, "not control authority", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, "control snapshot unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, snapshot)
	})
	mux.HandleFunc("GET /v1/replication-snapshot", func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := data.BuildReplicationSnapshot(r.Context())
		if err != nil {
			http.Error(w, "replication snapshot unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, snapshot)
	})
	return http.MaxBytesHandler(mux, maxResponse)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type Syncer struct {
	Data                    *store.Store
	Client                  *http.Client
	Peers                   []*url.URL
	DisableControlSnapshots bool
}

func ParsePeers(raw string) ([]*url.URL, error) {
	var out []*url.URL
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
			return nil, fmt.Errorf("invalid replication peer %q", part)
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *Syncer) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		for _, peer := range s.Peers {
			nodeID, err := s.syncPeer(ctx, peer)
			if recordErr := s.Data.RecordReplicationPeerResult(ctx, peer.Hostname(), nodeID, err); recordErr != nil && ctx.Err() == nil {
				slog.Warn("record replication peer status", "peer", peer.Redacted(), "error", recordErr)
			}
			if err != nil && ctx.Err() == nil {
				slog.Warn("replication peer sync failed", "peer", peer.Redacted(), "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Syncer) syncPeer(ctx context.Context, peer *url.URL) (string, error) {
	var remote store.ReplicationStatus
	if err := s.get(ctx, peer, "/v1/status", &remote); err != nil {
		return "", err
	}
	local, err := s.Data.ReplicationStatus(ctx)
	if err != nil {
		return remote.NodeID, err
	}
	if remote.ClusterID != local.ClusterID || remote.NodeID == local.NodeID {
		return remote.NodeID, errors.New("peer identity mismatch")
	}
	if remote.NodeID != peer.Hostname() {
		return remote.NodeID, errors.New("peer node ID does not match its certificate hostname")
	}
	snapshotApplied := false
	for origin, high := range remote.Origins {
		cursor, err := s.Data.ReplicationCursor(ctx, origin)
		if err != nil {
			return remote.NodeID, err
		}
		for cursor < high {
			var body struct {
				Operations []store.ReplicationOperation `json:"operations"`
			}
			path := "/v1/operations?origin=" + url.QueryEscape(origin) + "&after=" + strconv.FormatUint(cursor, 10) + "&limit=500"
			if err := s.get(ctx, peer, path, &body); err != nil {
				return remote.NodeID, err
			}
			if len(body.Operations) == 0 {
				if snapshotApplied {
					return remote.NodeID, errors.New("peer returned an incomplete range after snapshot")
				}
				if err := s.applyReplicationSnapshot(ctx, peer); err != nil {
					return remote.NodeID, err
				}
				snapshotApplied = true
				cursor, err = s.Data.ReplicationCursor(ctx, origin)
				if err != nil {
					return remote.NodeID, err
				}
				continue
			}
			if body.Operations[0].Sequence != cursor+1 {
				if snapshotApplied {
					return remote.NodeID, errors.New("peer retained range still has a gap after snapshot")
				}
				if err := s.applyReplicationSnapshot(ctx, peer); err != nil {
					return remote.NodeID, err
				}
				snapshotApplied = true
				cursor, err = s.Data.ReplicationCursor(ctx, origin)
				if err != nil {
					return remote.NodeID, err
				}
				continue
			}
			if err := s.Data.ApplyReplicationOperations(ctx, body.Operations); err != nil {
				if !errors.Is(err, store.ErrReplicationGap) || snapshotApplied {
					return remote.NodeID, err
				}
				if err := s.applyReplicationSnapshot(ctx, peer); err != nil {
					return remote.NodeID, err
				}
				snapshotApplied = true
				cursor, err = s.Data.ReplicationCursor(ctx, origin)
				if err != nil {
					return remote.NodeID, err
				}
				continue
			}
			cursor = body.Operations[len(body.Operations)-1].Sequence
		}
	}
	if remote.ControlAuthority && !s.DisableControlSnapshots {
		if remote.NodeID != s.Data.ControlAuthority() {
			return remote.NodeID, errors.New("unexpected control authority")
		}
		var snapshot store.ControlSnapshot
		if err := s.getLimit(ctx, peer, "/v1/control-snapshot", maxControlSnapshot, &snapshot); err != nil {
			return remote.NodeID, err
		}
		if err := s.Data.ApplyControlSnapshot(ctx, snapshot); err != nil {
			return remote.NodeID, err
		}
	}
	return remote.NodeID, nil
}

func (s *Syncer) applyReplicationSnapshot(ctx context.Context, peer *url.URL) error {
	var snapshot store.ReplicationSnapshot
	if err := s.getLimit(ctx, peer, "/v1/replication-snapshot", maxReplicationSnapshot, &snapshot); err != nil {
		return fmt.Errorf("fetch replication snapshot: %w", err)
	}
	if err := s.Data.ApplyReplicationSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("apply replication snapshot: %w", err)
	}
	return nil
}

func (s *Syncer) get(ctx context.Context, peer *url.URL, path string, out any) error {
	return s.getLimit(ctx, peer, path, maxResponse, out)
}

func (s *Syncer) getLimit(ctx context.Context, peer *url.URL, path string, limit int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.String()+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("peer returned %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(out)
}
