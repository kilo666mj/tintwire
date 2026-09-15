package agentbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type Config struct {
	Channel        string
	ThreadID       string
	StatePath      string
	PollInterval   time.Duration
	ReplayExisting bool
	Once           bool
}

type Bridge struct {
	Tintwire *TintwireClient
	Codex    *CodexClient
	Config   Config
}

type bridgeState struct {
	Initialized bool          `json:"initialized"`
	Channel     string        `json:"channel"`
	ThreadID    string        `json:"thread_id"`
	Cursor      string        `json:"cursor"`
	Pending     *pendingReply `json:"pending,omitempty"`
}

type pendingReply struct {
	MessageID string `json:"message_id"`
	Cursor    string `json:"cursor"`
	Text      string `json:"text"`
}

func (b *Bridge) Run(ctx context.Context) error {
	if b.Tintwire == nil || b.Codex == nil {
		return errors.New("bridge clients are required")
	}
	if strings.TrimSpace(b.Config.Channel) == "" || strings.TrimSpace(b.Config.ThreadID) == "" || strings.TrimSpace(b.Config.StatePath) == "" {
		return errors.New("channel, thread id, and state path are required")
	}
	if b.Config.PollInterval <= 0 {
		b.Config.PollInterval = 2 * time.Second
	}
	state, err := b.loadState()
	if err != nil {
		return err
	}
	if state.Initialized && (state.Channel != b.Config.Channel || state.ThreadID != b.Config.ThreadID) {
		return errors.New("state file belongs to a different channel or Codex thread")
	}
	if err := b.Tintwire.Initialize(ctx); err != nil {
		return err
	}
	if err := b.Codex.Resume(ctx, b.Config.ThreadID); err != nil {
		return err
	}
	state.Channel = b.Config.Channel
	state.ThreadID = b.Config.ThreadID
	if state.Pending != nil {
		if err := b.publishPending(ctx, &state); err != nil {
			return err
		}
	}
	if !state.Initialized {
		page, err := b.Tintwire.ListMessages(ctx, b.Config.Channel, "", 100)
		if err != nil {
			return err
		}
		state.Initialized = true
		if !b.Config.ReplayExisting {
			state.Cursor = page.NextCursor
			if err := b.saveState(state); err != nil {
				return err
			}
		} else if err := b.processPage(ctx, &state, page); err != nil {
			return err
		}
		if b.Config.Once {
			return nil
		}
	}

	ticker := time.NewTicker(b.Config.PollInterval)
	defer ticker.Stop()
	for {
		err := b.poll(ctx, &state)
		if err != nil {
			slog.Error("bridge poll failed", "error", err)
		}
		if b.Config.Once {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *Bridge) poll(ctx context.Context, state *bridgeState) error {
	for {
		page, err := b.Tintwire.ListMessages(ctx, b.Config.Channel, state.Cursor, 50)
		if err != nil {
			return err
		}
		if err := b.processPage(ctx, state, page); err != nil {
			return err
		}
		if !page.HasMore {
			return nil
		}
	}
}

func (b *Bridge) processPage(ctx context.Context, state *bridgeState, page MessagePage) error {
	for _, message := range page.Messages {
		prompt := fmt.Sprintf("Tintwire message from @%s in #%s (message %s):\n\n%s", message.Author, b.Config.Channel, message.ID, message.Text)
		reply, err := b.Codex.RunTurn(ctx, b.Config.ThreadID, message.ID, prompt)
		if err != nil {
			reply = "I couldn't complete that turn: " + err.Error()
		}
		reply = limitMessage(reply, 4000)
		if message.Cursor == "" {
			return errors.New("Tintwire message feed returned no cursor")
		}
		state.Pending = &pendingReply{MessageID: message.ID, Cursor: message.Cursor, Text: reply}
		if err := b.saveState(*state); err != nil {
			return err
		}
		if err := b.publishPending(ctx, state); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) publishPending(ctx context.Context, state *bridgeState) error {
	pending := state.Pending
	if pending == nil {
		return nil
	}
	if err := b.Tintwire.PublishReply(ctx, b.Config.Channel, pending.MessageID, pending.Text, "codex-reply-"+pending.MessageID); err != nil {
		return err
	}
	state.Cursor = pending.Cursor
	state.Pending = nil
	return b.saveState(*state)
}

func limitMessage(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maximum {
		return value
	}
	const suffix = "\n\n[response truncated by Tintwire bridge]"
	cut := maximum - len(suffix)
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut]) + suffix
}

func (b *Bridge) loadState() (bridgeState, error) {
	data, err := os.ReadFile(b.Config.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return bridgeState{}, nil
	}
	if err != nil {
		return bridgeState{}, err
	}
	var state bridgeState
	if err := json.Unmarshal(data, &state); err != nil {
		return bridgeState{}, fmt.Errorf("decode bridge state: %w", err)
	}
	return state, nil
}

func (b *Bridge) saveState(state bridgeState) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(b.Config.StatePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".tintwire-bridge-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, b.Config.StatePath); err != nil {
		return err
	}
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}
