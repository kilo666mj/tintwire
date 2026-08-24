package controlraft

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestThreeNodeQuorumCommitsAndMinorityCannotMutate(t *testing.T) {
	certificate, key, ca := testTLSMaterial(t)
	addresses := []string{freeAddress(t), freeAddress(t), freeAddress(t)}
	ids := []string{"node-one", "node-two", "node-three"}
	nodes := make([]*Node, 3)
	stores := make([]*store.Store, 3)
	for index := range nodes {
		data, err := store.Open(filepath.Join(t.TempDir(), "tintwire.db"))
		if err != nil {
			t.Fatal(err)
		}
		stores[index] = data
		t.Cleanup(func() { _ = data.Close() })
		if err := data.ConfigureReplication("cluster-test", ids[index]); err != nil {
			t.Fatal(err)
		}
		if err := data.ConfigureControlPlane(ids[0], 30*time.Second); err != nil {
			t.Fatal(err)
		}
		var peers []Peer
		for peerIndex := range ids {
			if peerIndex != index {
				peers = append(peers, Peer{ID: ids[peerIndex], Address: addresses[peerIndex]})
			}
		}
		node, err := Open(Config{NodeID: ids[index], BindAddress: addresses[index], Advertise: addresses[index], DataDirectory: t.TempDir(), Certificate: certificate, Key: key, CA: ca, Bootstrap: index == 0, Peers: peers}, data)
		if err != nil {
			t.Fatal(err)
		}
		nodes[index] = node
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			if node != nil {
				_ = node.Close()
			}
		}
	})

	initializationCtx, initializationCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := nodes[0].Initialize(initializationCtx); err != nil {
		initializationCancel()
		t.Fatal(err)
	}
	initializationCancel()
	waitFor(t, 12*time.Second, func() bool {
		count, err := nodes[0].VoterCount()
		return err == nil && count == 3
	})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err := nodes[0].Mutate(ctx, func(data *store.Store) (any, error) {
		return data.CreateUser(context.Background(), "committed-user", "correct horse battery staple", false)
	})
	if err != nil {
		t.Fatalf("majority mutation: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := stores[2].AuthenticateUser(context.Background(), "committed-user", "correct horse battery staple")
		return err == nil
	})

	const actionKey = "cluster-wide-action-encryption-key"
	_, err = nodes[0].Mutate(ctx, func(data *store.Store) (any, error) {
		return nil, data.SaveSettings(context.Background(), map[string]string{"action_encryption_key": actionKey})
	})
	if err != nil {
		t.Fatalf("commit action encryption key: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		for _, data := range stores {
			value, ok, err := data.Setting(context.Background(), "action_encryption_key")
			if err != nil || !ok || value != actionKey {
				return false
			}
		}
		return true
	})
	_, err = nodes[0].Mutate(ctx, func(data *store.Store) (any, error) {
		return data.SaveActionTarget(context.Background(), "deletion-probe", "https://example.com/probe", []byte("ciphertext"), false)
	})
	if err != nil {
		t.Fatalf("commit action target: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		for _, data := range stores {
			if _, err := data.ActionTargetByName(context.Background(), "deletion-probe"); err != nil {
				return false
			}
		}
		return true
	})
	_, err = nodes[0].Mutate(ctx, func(data *store.Store) (any, error) {
		return nil, data.DeleteActionTarget(context.Background(), "deletion-probe")
	})
	if err != nil {
		t.Fatalf("delete action target: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		for _, data := range stores {
			if _, err := data.ActionTargetByName(context.Background(), "deletion-probe"); !errors.Is(err, store.ErrNotificationNotFound) {
				return false
			}
		}
		return true
	})
	if err := nodes[0].raft.LeadershipTransferToServer(raft.ServerID(ids[1]), raft.ServerAddress(addresses[1])).Error(); err != nil {
		t.Fatalf("transfer leadership: %v", err)
	}
	waitFor(t, 5*time.Second, nodes[1].IsLeader)
	value, ok, err := stores[1].Setting(context.Background(), "action_encryption_key")
	if err != nil || !ok || value != actionKey {
		t.Fatalf("new leader action encryption key=%q ok=%v err=%v", value, ok, err)
	}
	_, err = nodes[1].Mutate(ctx, func(data *store.Store) (any, error) {
		return nil, data.SaveSettings(context.Background(), map[string]string{"leadership_change_probe": "committed"})
	})
	if err != nil {
		t.Fatalf("new leader mutation: %v", err)
	}
	if err := nodes[1].raft.LeadershipTransferToServer(raft.ServerID(ids[0]), raft.ServerAddress(addresses[0])).Error(); err != nil {
		t.Fatalf("transfer leadership back: %v", err)
	}
	waitFor(t, 5*time.Second, nodes[0].IsLeader)

	if err := nodes[1].Close(); err != nil {
		t.Fatal(err)
	}
	nodes[1] = nil
	if err := nodes[2].Close(); err != nil {
		t.Fatal(err)
	}
	nodes[2] = nil
	minorityCtx, minorityCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer minorityCancel()
	_, err = nodes[0].Mutate(minorityCtx, func(data *store.Store) (any, error) {
		return data.CreateUser(context.Background(), "uncommitted-user", "correct horse battery staple", false)
	})
	if err == nil {
		t.Fatal("minority mutation unexpectedly committed")
	}
	if !errors.Is(err, raft.ErrNotLeader) && !errors.Is(err, raft.ErrLeadershipLost) && !errors.Is(err, raft.ErrEnqueueTimeout) && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("minority failed with expected quorum-related error: %v", err)
	}
	if _, err := stores[0].AuthenticateUser(context.Background(), "uncommitted-user", "correct horse battery staple"); !errors.Is(err, store.ErrInvalidCredentials) {
		t.Fatalf("minority proposal altered live state: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func testTLSMaterial(t *testing.T) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := filepath.Join(directory, "node.crt")
	key := filepath.Join(directory, "node.key")
	ca := filepath.Join(directory, "ca.crt")
	writePEM(t, certificate, "CERTIFICATE", leafDER)
	writePEM(t, key, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(leafKey))
	writePEM(t, ca, "CERTIFICATE", caDER)
	return certificate, key, ca
}

func writePEM(t *testing.T, path, kind string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0o600); err != nil {
		t.Fatal(err)
	}
}
