package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kilo666mj/tintwire/internal/agentbridge"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("Tintwire Codex bridge stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	tintwireURL := flag.String("tintwire-url", os.Getenv("TINTWIRE_URL"), "Tintwire base URL (or TINTWIRE_URL)")
	channel := flag.String("channel", os.Getenv("TINTWIRE_CHANNEL"), "dedicated Tintwire channel name (or TINTWIRE_CHANNEL)")
	threadID := flag.String("thread", os.Getenv("CODEX_THREAD_ID"), "existing Codex thread UUID (or CODEX_THREAD_ID)")
	statePath := flag.String("state", ".tintwire-codex-bridge.json", "durable relay cursor and pending-reply state")
	codexBinary := flag.String("codex", "codex", "Codex CLI binary")
	poll := flag.Duration("poll", 2*time.Second, "Tintwire polling interval")
	replayExisting := flag.Bool("replay-existing", false, "send the newest existing messages on first startup")
	once := flag.Bool("once", false, "poll once and exit")
	flag.Parse()

	if strings.TrimSpace(*channel) == "" || strings.TrimSpace(*threadID) == "" {
		return errors.New("channel and existing Codex thread UUID are required")
	}
	tintwire, err := agentbridge.NewTintwireClient(*tintwireURL, os.Getenv("TINTWIRE_AGENT_TOKEN"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	codex, err := agentbridge.StartCodexClient(ctx, *codexBinary, os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = codex.Close() }()

	slog.Info("starting Tintwire Codex bridge", "channel", *channel, "thread", *threadID)
	return (&agentbridge.Bridge{Tintwire: tintwire, Codex: codex, Config: agentbridge.Config{
		Channel: *channel, ThreadID: *threadID, StatePath: *statePath, PollInterval: *poll,
		ReplayExisting: *replayExisting, Once: *once,
	}}).Run(ctx)
}
