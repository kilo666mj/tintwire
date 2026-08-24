package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kilo666mj/tintwire/internal/controlraft"
	"github.com/kilo666mj/tintwire/internal/replicationhttp"
	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("tintwire stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", envOr("TINTWIRE_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	databaseDriver := flag.String("db-driver", envOr("TINTWIRE_DB_DRIVER", "sqlite"), "database driver: sqlite or postgres")
	database := flag.String("db", envOr("TINTWIRE_DB", "tintwire.db"), "SQLite path or PostgreSQL connection string")
	hookID := flag.String("hook-id", os.Getenv("TINTWIRE_HOOK_ID"), "Mattermost webhook token to bootstrap")
	channel := flag.String("channel", envOr("TINTWIRE_CHANNEL", "general"), "channel for the bootstrapped webhook")
	vapidContact := flag.String("vapid-contact", os.Getenv("TINTWIRE_VAPID_CONTACT"), "public email, mailto:, or HTTPS contact for Web Push")
	readerUsername := flag.String("reader-username", envOr("TINTWIRE_READER_USERNAME", "admin"), "bootstrap reader username")
	readerPassword := flag.String("reader-password", os.Getenv("TINTWIRE_READER_PASSWORD"), "bootstrap reader password; enables reader authentication")
	actionKey := flag.String("action-key", os.Getenv("TINTWIRE_ACTION_KEY"), "base64-encoded 32-byte key for encrypting action credentials")
	publicURL := flag.String("public-url", os.Getenv("TINTWIRE_PUBLIC_URL"), "canonical external HTTP(S) origin used for secure cookies and origin validation")
	oauthIssuer := flag.String("oauth-issuer", os.Getenv("TINTWIRE_OAUTH_ISSUER"), "Pocket ID issuer for MCP OAuth access tokens")
	oauthResource := flag.String("oauth-resource", os.Getenv("TINTWIRE_OAUTH_RESOURCE"), "required OAuth audience; defaults to TINTWIRE_PUBLIC_URL/mcp")
	oauthScope := flag.String("oauth-scope", envOr("TINTWIRE_OAUTH_SCOPE", "tintwire:mcp"), "required Pocket ID API permission for MCP")
	oidcClientID := flag.String("oidc-client-id", os.Getenv("TINTWIRE_OIDC_CLIENT_ID"), "Pocket ID client ID for interactive browser sign-in")
	oidcRedirectURL := flag.String("oidc-redirect-url", os.Getenv("TINTWIRE_OIDC_REDIRECT_URL"), "Pocket ID callback URL; defaults to TINTWIRE_PUBLIC_URL/api/v1/auth/oidc/callback")
	clusterID := flag.String("cluster-id", os.Getenv("TINTWIRE_CLUSTER_ID"), "replication cluster identity; requires node-id")
	nodeID := flag.String("node-id", os.Getenv("TINTWIRE_NODE_ID"), "immutable replication node identity; requires cluster-id")
	replicationListen := flag.String("replication-listen", os.Getenv("TINTWIRE_REPLICATION_LISTEN"), "mTLS replication listen address")
	replicationCert := flag.String("replication-cert", os.Getenv("TINTWIRE_REPLICATION_CERT"), "replication TLS certificate")
	replicationKey := flag.String("replication-key", os.Getenv("TINTWIRE_REPLICATION_KEY"), "replication TLS private key")
	replicationCA := flag.String("replication-ca", os.Getenv("TINTWIRE_REPLICATION_CA"), "replication client/root CA")
	replicationPeers := flag.String("replication-peers", os.Getenv("TINTWIRE_REPLICATION_PEERS"), "comma-separated HTTPS peer origins")
	controlAuthority := flag.String("control-authority", os.Getenv("TINTWIRE_CONTROL_AUTHORITY"), "node ID authorized to publish security control snapshots")
	controlLeaseRaw := flag.String("control-lease", envOr("TINTWIRE_CONTROL_LEASE", "30s"), "maximum age of a replica security control snapshot")
	controlRaftListen := flag.String("control-raft-listen", os.Getenv("TINTWIRE_CONTROL_RAFT_LISTEN"), "mTLS control Raft listen address")
	controlRaftAdvertise := flag.String("control-raft-advertise", os.Getenv("TINTWIRE_CONTROL_RAFT_ADVERTISE"), "control Raft address advertised to peers")
	controlRaftDirectory := flag.String("control-raft-dir", os.Getenv("TINTWIRE_CONTROL_RAFT_DIR"), "durable control Raft data directory")
	controlRaftPeers := flag.String("control-raft-peers", os.Getenv("TINTWIRE_CONTROL_RAFT_PEERS"), "comma-separated node=host:port control voters")
	controlRaftBootstrap := flag.Bool("control-raft-bootstrap", envBool("TINTWIRE_CONTROL_RAFT_BOOTSTRAP"), "bootstrap a new control Raft cluster if no state exists")
	controlProxyPort := flag.String("control-proxy-port", envOr("TINTWIRE_CONTROL_PROXY_PORT", "18088"), "internal HTTP port used to forward control writes to the Raft leader")
	flag.Parse()
	controlLease, err := time.ParseDuration(*controlLeaseRaw)
	if err != nil {
		return fmt.Errorf("parse control lease: %w", err)
	}

	if *hookID == "" {
		return fmt.Errorf("a webhook token is required; set -hook-id or TINTWIRE_HOOK_ID")
	}
	authRequired := *readerPassword != ""
	if err := validateAuthConfiguration(authRequired, *publicURL); err != nil {
		return err
	}

	db, err := store.OpenBackend(*databaseDriver, *database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.ConfigureReplication(*clusterID, *nodeID); err != nil {
		return fmt.Errorf("configure replication identity: %w", err)
	}
	if err := db.ConfigureControlPlane(*controlAuthority, controlLease); err != nil {
		return fmt.Errorf("configure control plane: %w", err)
	}

	if err := db.BootstrapWebhook(context.Background(), *hookID, *channel); err != nil {
		return fmt.Errorf("bootstrap webhook: %w", err)
	}
	if authRequired {
		if err := db.BootstrapUser(context.Background(), *readerUsername, *readerPassword); err != nil {
			return fmt.Errorf("bootstrap reader: %w", err)
		}
	} else {
		slog.Warn("reader authentication is disabled; set TINTWIRE_READER_PASSWORD before exposing Tintwire")
	}

	var consensus *controlraft.Node
	if *controlRaftListen != "" || *controlRaftAdvertise != "" || *controlRaftDirectory != "" || *controlRaftPeers != "" || *controlRaftBootstrap {
		if *clusterID == "" || *nodeID == "" || *controlRaftListen == "" || *controlRaftAdvertise == "" || *controlRaftDirectory == "" || *replicationCert == "" || *replicationKey == "" || *replicationCA == "" {
			return fmt.Errorf("control Raft requires cluster ID, node ID, listen, advertise, data directory, and replication TLS material")
		}
		peers, err := controlraft.ParsePeers(*controlRaftPeers)
		if err != nil {
			return err
		}
		consensus, err = controlraft.Open(controlraft.Config{NodeID: *nodeID, BindAddress: *controlRaftListen, Advertise: *controlRaftAdvertise, DataDirectory: *controlRaftDirectory, Certificate: *replicationCert, Key: *replicationKey, CA: *replicationCA, Bootstrap: *controlRaftBootstrap, Peers: peers}, db)
		if err != nil {
			return fmt.Errorf("initialize control Raft: %w", err)
		}
		defer consensus.Close()
	}

	var serverConsensus server.ControlConsensus
	if consensus != nil {
		serverConsensus = consensus
	}
	handler, err := server.NewWithOptions(db, server.Options{VAPIDContact: *vapidContact, AuthRequired: authRequired, ActionKey: *actionKey, PublicURL: *publicURL, OAuthIssuer: *oauthIssuer, OAuthResource: *oauthResource, OAuthScope: *oauthScope, OIDCClientID: *oidcClientID, OIDCRedirectURL: *oidcRedirectURL, Consensus: serverConsensus, ControlProxyPort: *controlProxyPort})
	if err != nil {
		return fmt.Errorf("initialize HTTP server: %w", err)
	}
	if consensus != nil {
		initializeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = consensus.Initialize(initializeCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("seed control Raft: %w", err)
		}
	}
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Streaming notification events intentionally keep a response open.
		// Production ingress must enforce client-facing write timeouts.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var replicationServer *http.Server
	if *replicationListen != "" || *replicationCert != "" || *replicationKey != "" || *replicationCA != "" || *replicationPeers != "" {
		if *replicationListen == "" || *replicationCert == "" || *replicationKey == "" || *replicationCA == "" {
			return fmt.Errorf("replication listen, cert, key, and CA must be configured together")
		}
		certificate, err := tls.LoadX509KeyPair(*replicationCert, *replicationKey)
		if err != nil {
			return fmt.Errorf("load replication certificate: %w", err)
		}
		caPEM, err := os.ReadFile(*replicationCA)
		if err != nil {
			return fmt.Errorf("read replication CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return fmt.Errorf("replication CA contains no certificates")
		}
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
		replicationServer = &http.Server{Addr: *replicationListen, Handler: replicationhttp.Handler(db), TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
		go func() {
			if err := replicationServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				slog.Error("replication listener stopped", "error", err)
				stop()
			}
		}()
		peers, err := replicationhttp.ParsePeers(*replicationPeers)
		if err != nil {
			return err
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: pool}}, Timeout: 15 * time.Second}
		go (&replicationhttp.Syncer{Data: db, Client: client, Peers: peers, DisableControlSnapshots: consensus != nil}).Run(ctx)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP shutdown failed", "error", err)
		}
		if replicationServer != nil {
			_ = replicationServer.Shutdown(shutdownCtx)
		}
	}()

	slog.Info("Tintwire listening", "address", *listen, "channel", *channel)
	err = httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func validateAuthConfiguration(authRequired bool, publicURL string) error {
	if authRequired && strings.TrimSpace(publicURL) == "" {
		return fmt.Errorf("TINTWIRE_PUBLIC_URL is required when reader authentication is enabled")
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
