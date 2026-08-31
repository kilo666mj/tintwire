package controlraft

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/kilo666mj/tintwire/internal/store"
)

type Peer struct {
	ID      string
	Address string
}

type Config struct {
	NodeID        string
	BindAddress   string
	Advertise     string
	DataDirectory string
	Certificate   string
	Key           string
	CA            string
	Bootstrap     bool
	Peers         []Peer
}

type Node struct {
	raft       *raft.Raft
	transport  *raft.NetworkTransport
	store      *raftboltdb.BoltStore
	data       *store.Store
	localID    string
	peers      []Peer
	mutateMu   sync.Mutex
	stop       chan struct{}
	newCluster bool
}

func Open(config Config, data *store.Store) (*Node, error) {
	if config.NodeID == "" || config.BindAddress == "" || config.Advertise == "" || config.DataDirectory == "" {
		return nil, errors.New("control raft identity, bind, advertise, and data directory are required")
	}
	if err := os.MkdirAll(config.DataDirectory, 0o700); err != nil {
		return nil, err
	}
	stream, err := newTLSStream(config)
	if err != nil {
		return nil, err
	}
	transport := raft.NewNetworkTransport(stream, 3, 5*time.Second, os.Stderr)
	boltStore, err := raftboltdb.NewBoltStore(filepath.Join(config.DataDirectory, "raft.db"))
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	snapshots, err := raft.NewFileSnapshotStore(config.DataDirectory, 3, os.Stderr)
	if err != nil {
		_ = boltStore.Close()
		_ = transport.Close()
		return nil, err
	}
	hasState, err := raft.HasExistingState(boltStore, boltStore, snapshots)
	if err != nil {
		_ = boltStore.Close()
		_ = transport.Close()
		return nil, err
	}
	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(config.NodeID)
	raftConfig.SnapshotThreshold = 64
	raftConfig.SnapshotInterval = 30 * time.Second
	fsm := &FSM{Data: data}
	instance, err := raft.NewRaft(raftConfig, fsm, boltStore, boltStore, snapshots, transport)
	if err != nil {
		_ = boltStore.Close()
		_ = transport.Close()
		return nil, err
	}
	node := &Node{raft: instance, transport: transport, store: boltStore, data: data, localID: config.NodeID, peers: config.Peers, stop: make(chan struct{}), newCluster: config.Bootstrap && !hasState}
	if config.Bootstrap && !hasState {
		future := instance.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
			ID:       raft.ServerID(config.NodeID),
			Address:  raft.ServerAddress(config.Advertise),
			Suffrage: raft.Voter,
		}}})
		if err := future.Error(); err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
			return nil, errors.Join(err, node.Close())
		}
	}
	go node.reconcileLoop()
	return node, nil
}

// Initialize commits the existing SQLite control state as the first log entry
// when creating a brand-new cluster. Existing Raft data is never re-seeded.
func (n *Node) Initialize(ctx context.Context) error {
	if !n.newCluster {
		return nil
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for !n.IsLeader() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	snapshot, err := n.data.ExportControlSnapshot(ctx, n.localID)
	if err != nil {
		return err
	}
	if err := n.ApplySnapshot(ctx, snapshot); err != nil {
		return err
	}
	n.newCluster = false
	return nil
}

func ParsePeers(raw string) ([]Peer, error) {
	var peers []Peer
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, address, ok := strings.Cut(part, "=")
		id, address = strings.TrimSpace(id), strings.TrimSpace(address)
		if !ok || id == "" || address == "" || seen[id] {
			return nil, fmt.Errorf("invalid or duplicate control raft peer %q", part)
		}
		if _, _, err := net.SplitHostPort(address); err != nil {
			return nil, fmt.Errorf("invalid control raft peer %q: %w", part, err)
		}
		seen[id] = true
		peers = append(peers, Peer{ID: id, Address: address})
	}
	return peers, nil
}

func (n *Node) Close() error {
	select {
	case <-n.stop:
	default:
		close(n.stop)
	}
	var joined error
	if n.raft != nil {
		joined = errors.Join(joined, n.raft.Shutdown().Error())
	}
	if n.transport != nil {
		joined = errors.Join(joined, n.transport.Close())
	}
	if n.store != nil {
		joined = errors.Join(joined, n.store.Close())
	}
	return joined
}

func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

func (n *Node) NodeID() string { return n.localID }

func (n *Node) Leader() (string, string) {
	address, id := n.raft.LeaderWithID()
	return string(id), string(address)
}

func (n *Node) VoterCount() (int, error) {
	future := n.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return 0, err
	}
	count := 0
	for _, server := range future.Configuration().Servers {
		if server.Suffrage == raft.Voter {
			count++
		}
	}
	return count, nil
}

func (n *Node) ApplySnapshot(ctx context.Context, snapshot store.ControlSnapshot) error {
	if !n.IsLeader() {
		return raft.ErrNotLeader
	}
	if err := n.raft.VerifyLeader().Error(); err != nil {
		return err
	}
	encoded, err := Encode(snapshot)
	if err != nil {
		return err
	}
	timeout, err := raftTimeout(ctx, 10*time.Second)
	if err != nil {
		return err
	}
	future := n.raft.Apply(encoded, timeout)
	if err := future.Error(); err != nil {
		return err
	}
	return AppliedError(future.Response())
}

// Mutate evaluates a control change against an isolated SQLite database and
// commits its complete resulting state through Raft. The live leader and all
// followers are changed only by FSM.Apply after majority commit.
func (n *Node) Mutate(ctx context.Context, mutation func(*store.Store) (any, error)) (any, error) {
	n.mutateMu.Lock()
	defer n.mutateMu.Unlock()
	if !n.IsLeader() {
		return nil, raft.ErrNotLeader
	}
	timeout, err := raftTimeout(ctx, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err := n.raft.Barrier(timeout).Error(); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "tintwire-control-proposal-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	stagedPath := filepath.Join(directory, "control.db")
	if err := n.data.Backup(ctx, stagedPath); err != nil {
		return nil, err
	}
	staged, err := store.Open(stagedPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = staged.Close() }()
	status, err := n.data.ReplicationStatus(ctx)
	if err != nil {
		return nil, err
	}
	if err := staged.ConfigureReplication(status.ClusterID, n.localID); err != nil {
		return nil, err
	}
	if err := staged.ConfigureControlPlane(n.localID, 30*time.Second); err != nil {
		return nil, err
	}
	result, err := mutation(staged)
	if err != nil {
		return nil, err
	}
	proposed, err := staged.ExportControlSnapshot(ctx, n.localID)
	if err != nil {
		return nil, err
	}
	if err := n.ApplySnapshot(ctx, proposed); err != nil {
		return nil, err
	}
	return result, nil
}

func raftTimeout(ctx context.Context, maximum time.Duration) (time.Duration, error) {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		if remaining < maximum {
			return remaining, nil
		}
	}
	return maximum, nil
}

func (n *Node) Healthy(lease time.Duration) bool {
	if n.IsLeader() {
		return n.raft.VerifyLeader().Error() == nil
	}
	_, leader := n.raft.LeaderWithID()
	lastContact := n.raft.LastContact()
	return leader != "" && !lastContact.IsZero() && time.Since(lastContact) <= lease
}

func (n *Node) reconcileLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-ticker.C:
		}
		if !n.IsLeader() {
			continue
		}
		future := n.raft.GetConfiguration()
		if future.Error() != nil {
			continue
		}
		current := make(map[raft.ServerID]raft.ServerAddress)
		for _, server := range future.Configuration().Servers {
			current[server.ID] = server.Address
		}
		for _, peer := range n.peers {
			id, address := raft.ServerID(peer.ID), raft.ServerAddress(peer.Address)
			if existing, ok := current[id]; ok && existing == address {
				continue
			}
			_ = n.raft.AddVoter(id, address, 0, 10*time.Second).Error()
		}
	}
}

type tlsStream struct {
	net.Listener
	clientCertificate tls.Certificate
	roots             *x509.CertPool
}

func newTLSStream(config Config) (*tlsStream, error) {
	certificate, err := tls.LoadX509KeyPair(config.Certificate, config.Key)
	if err != nil {
		return nil, err
	}
	caBytes, err := os.ReadFile(config.CA)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("control raft CA contains no certificates")
	}
	listener, err := net.Listen("tcp", config.BindAddress)
	if err != nil {
		return nil, err
	}
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots,
		MinVersion:   tls.VersionTLS13,
	}
	return &tlsStream{Listener: tls.NewListener(listener, serverTLS), clientCertificate: certificate, roots: roots}, nil
}

func (s *tlsStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	host, _, err := net.SplitHostPort(string(address))
	if err != nil {
		return nil, fmt.Errorf("invalid control raft address: %w", err)
	}
	dialer := &net.Dialer{Timeout: timeout}
	return tls.DialWithDialer(dialer, "tcp", string(address), &tls.Config{
		Certificates: []tls.Certificate{s.clientCertificate},
		RootCAs:      s.roots,
		ServerName:   strings.Trim(host, "[]"),
		MinVersion:   tls.VersionTLS13,
	})
}
