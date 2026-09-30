package agentbridge

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type queuedCommand struct {
	CancelRequested bool   `json:"cancel_requested"`
	ID              string `json:"id"`
	MessageID       string `json:"message_id"`
	Author          string `json:"author"`
	Text            string `json:"text"`
	State           string `json:"state"`
	TurnID          string `json:"turn_id"`
}

func (c *TintwireClient) queueCall(ctx context.Context, name string, args map[string]any) (*queuedCommand, error) {
	if _, ok := args["idempotency_key"]; !ok {
		args["idempotency_key"] = randomKey()
	}
	var result struct {
		Command *queuedCommand `json:"command"`
	}
	err := c.tool(ctx, name, args, &result)
	return result.Command, err
}

func (b *Bridge) runQueue(ctx context.Context) error {
	owner := randomKey()
	for {
		var pending struct {
			Pending bool `json:"pending"`
		}
		if err := b.Tintwire.tool(ctx, "commands.pending.v1", map[string]string{"channel": b.Config.Channel}, &pending); err != nil {
			return err
		}
		var command *queuedCommand
		var err error
		if pending.Pending {
			command, err = b.Tintwire.queueCall(ctx, "commands.claim.v1", map[string]any{"channel": b.Config.Channel, "owner": owner})
		}
		if err != nil {
			return err
		}
		if command != nil {
			if err := b.runQueuedCommand(ctx, owner, *command); err != nil {
				return err
			}
		}
		if b.Config.Once {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.Config.PollInterval):
		}
	}
}

func (b *Bridge) runQueuedCommand(ctx context.Context, owner string, command queuedCommand) error {
	b.busy.Store(true)
	b.notifyPresence()
	defer func() { b.busy.Store(false); b.notifyPresence() }()
	// Renew before starting even when the claim response was replayed.
	current, err := b.Tintwire.queueCall(ctx, "commands.renew.v1", map[string]any{"id": command.ID, "owner": owner})
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("queue returned no command")
	}
	command = *current
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		reply string
		err   error
	}
	finished := make(chan outcome, 1)
	started := make(chan string, 1)
	initialCancel := command.CancelRequested || command.State == "cancelling"
	if command.State == "interrupted" || command.TurnID != "" {
		if command.TurnID == "" {
			return errors.New("interrupted command has no recorded turn; inspect the runtime and reconcile it in Tintwire before continuing")
		}
		if initialCancel {
			if err := b.Codex.interrupt(ctx, b.Config.ThreadID, command.TurnID); err != nil {
				return err
			}
		}
		go func() {
			reply, err := b.Codex.recoverTurn(turnCtx, b.Config.ThreadID, command.TurnID)
			finished <- outcome{reply, err}
		}()
	} else if command.State == "cancelling" && command.TurnID == "" {
		finished <- outcome{"Cancelled before the runtime turn started.", context.Canceled}
	} else {
		prompt := fmt.Sprintf("Tintwire message from @%s in #%s (message %s):\n\n%s", command.Author, b.Config.Channel, command.MessageID, command.Text)
		go func() {
			reply, err := b.Codex.runTurn(turnCtx, b.Config.ThreadID, command.MessageID, prompt, func(id string) error {
				_, err := b.Tintwire.queueCall(turnCtx, "commands.renew.v1", map[string]any{"id": command.ID, "owner": owner, "turn_id": id})
				if err == nil {
					started <- id
				}
				return err
			})
			finished <- outcome{reply, err}
		}()
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	cancelled := initialCancel
	turnID := command.TurnID
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-started:
			turnID = id
		case <-ticker.C:
			c, err := b.Tintwire.queueCall(ctx, "commands.renew.v1", map[string]any{"id": command.ID, "owner": owner})
			if err != nil {
				cancel()
				return fmt.Errorf("command lease lost; runtime reconciliation required: %w", err)
			}
			if c != nil && c.State == "cancelling" && !cancelled {
				if turnID != "" {
					if err := b.Codex.interrupt(ctx, b.Config.ThreadID, turnID); err != nil {
						return err
					}
				}
				cancelled = true
				cancel()
			}
		case result := <-finished:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Unknown runtime state is not failure evidence: retain the command
			// for reconciliation instead of allowing a second turn to start.
			if errors.Is(result.err, errTurnUnknown) {
				return result.err
			}
			state := "completed"
			if result.err != nil {
				state = "failed"
				result.reply = "I couldn't complete that turn: " + result.err.Error()
			}
			if cancelled || errors.Is(result.err, context.Canceled) {
				state = "cancelled"
				result.reply = "Agent command cancelled."
			}
			key := randomKey()
			args := map[string]any{"id": command.ID, "owner": owner, "state": state, "result": limitMessage(result.reply, 4000), "idempotency_key": key}
			// Retry only result publication, never the model turn.
			for attempt := 0; attempt < 3; attempt++ {
				if _, err := b.Tintwire.queueCall(ctx, "commands.complete.v1", args); err == nil {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
			return errors.New("result publication failed; reconcile the recorded runtime turn before continuing")
		}
	}
}

var errTurnUnknown = errors.New("runtime turn could not be reconciled; inspect the runtime before closing the interrupted command")

func (c *CodexClient) interrupt(ctx context.Context, threadID, turnID string) error {
	var result json.RawMessage
	return c.call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID}, &result)
}

func (c *CodexClient) recoverTurn(ctx context.Context, threadID, turnID string) (string, error) {
	for {
		var result struct {
			Thread struct {
				Turns []json.RawMessage `json:"turns"`
			} `json:"thread"`
		}
		if err := c.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &result); err != nil {
			return "", fmt.Errorf("%w: %v", errTurnUnknown, err)
		}
		found := false
		for _, raw := range result.Thread.Turns {
			var turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			if json.Unmarshal(raw, &turn) != nil || turn.ID != turnID {
				continue
			}
			found = true
			if turn.Status == "inProgress" {
				break
			}
			wrapped, err := json.Marshal(map[string]any{"threadId": threadID, "turn": raw})
			if err != nil {
				return "", err
			}
			reply, _, err := completedReply(wrapped, threadID, turnID)
			return reply, err
		}
		if !found {
			return "", errTurnUnknown
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func randomKey() string { return rand.Text() }
