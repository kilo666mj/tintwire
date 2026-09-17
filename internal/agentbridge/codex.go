package agentbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type CodexClient struct {
	command *exec.Cmd
	stdin   io.WriteCloser

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcEnvelope
	events  chan rpcEnvelope
	done    chan struct{}
	readErr error
}

func StartCodexClient(ctx context.Context, binary string, stderr io.Writer) (*CodexClient, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "codex"
	}
	command := exec.CommandContext(ctx, binary, "app-server", "--stdio")
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	client := &CodexClient{
		command: command, stdin: stdin, pending: make(map[int64]chan rpcEnvelope),
		events: make(chan rpcEnvelope, 256), done: make(chan struct{}),
	}
	go client.read(stdout)
	if err := client.initialize(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (c *CodexClient) initialize(ctx context.Context) error {
	var initialized json.RawMessage
	if err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "tintwire-codex-bridge", "version": "0.1.0"},
		"capabilities": map[string]any{},
	}, &initialized); err != nil {
		return fmt.Errorf("initialize Codex app-server: %w", err)
	}
	return c.write(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
}

func (c *CodexClient) Resume(ctx context.Context, threadID string) error {
	var response json.RawMessage
	if err := c.call(ctx, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &response); err != nil {
		return fmt.Errorf("resume Codex thread %s: %w", threadID, err)
	}
	return nil
}

func (c *CodexClient) RunTurn(ctx context.Context, threadID, messageID, text string) (string, error) {
	if err := c.waitUntilIdle(ctx, threadID); err != nil {
		return "", err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := c.call(ctx, "turn/start", map[string]any{
		"threadId":            threadID,
		"input":               []map[string]string{{"type": "text", "text": text}},
		"clientUserMessageId": "tintwire:" + messageID,
		"turnTrigger":         "tintwire",
	}, &started); err != nil {
		return "", fmt.Errorf("start Codex turn: %w", err)
	}
	if started.Turn.ID == "" {
		return "", errors.New("codex returned no turn id")
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.done:
			return "", c.readerError()
		case event := <-c.events:
			if event.Method != "turn/completed" {
				continue
			}
			reply, matched, err := completedReply(event.Params, threadID, started.Turn.ID)
			if !matched {
				continue
			}
			return reply, err
		}
	}
}

func (c *CodexClient) waitUntilIdle(ctx context.Context, threadID string) error {
	var read struct {
		Thread struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := c.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &read); err != nil {
		return fmt.Errorf("read Codex thread status: %w", err)
	}
	if read.Thread.Status.Type == "idle" {
		return nil
	}
	if read.Thread.Status.Type != "active" {
		return fmt.Errorf("codex thread is %s", read.Thread.Status.Type)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.readerError()
		case event := <-c.events:
			if event.Method != "thread/status/changed" {
				continue
			}
			var changed struct {
				ThreadID string `json:"threadId"`
				Status   struct {
					Type string `json:"type"`
				} `json:"status"`
			}
			if json.Unmarshal(event.Params, &changed) != nil || changed.ThreadID != threadID {
				continue
			}
			if changed.Status.Type == "idle" {
				return nil
			}
			if changed.Status.Type != "active" {
				return fmt.Errorf("codex thread became %s", changed.Status.Type)
			}
		}
	}
}

func completedReply(params json.RawMessage, threadID, turnID string) (string, bool, error) {
	var completed struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Items []struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Phase string `json:"phase"`
			} `json:"items"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &completed) != nil || completed.ThreadID != threadID || completed.Turn.ID != turnID {
		return "", false, nil
	}
	if completed.Turn.Status != "completed" {
		if completed.Turn.Error != nil && completed.Turn.Error.Message != "" {
			return "", true, errors.New(completed.Turn.Error.Message)
		}
		return "", true, fmt.Errorf("codex turn ended with status %s", completed.Turn.Status)
	}
	var fallback, final string
	for _, item := range completed.Turn.Items {
		if item.Type != "agentMessage" || strings.TrimSpace(item.Text) == "" {
			continue
		}
		fallback = strings.TrimSpace(item.Text)
		if item.Phase == "final_answer" {
			final = fallback
		}
	}
	if final != "" {
		return final, true, nil
	}
	if fallback == "" {
		return "", true, errors.New("codex completed without an agent message")
	}
	return fallback, true, nil
}

func (c *CodexClient) call(ctx context.Context, method string, params any, target any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	response := make(chan rpcEnvelope, 1)
	c.pending[id] = response
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		return c.readerError()
	case envelope := <-response:
		if envelope.Error != nil {
			return fmt.Errorf("RPC error %d: %s", envelope.Error.Code, envelope.Error.Message)
		}
		if err := json.Unmarshal(envelope.Result, target); err != nil {
			return err
		}
		return nil
	}
}

func (c *CodexClient) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		var envelope rpcEnvelope
		if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
			continue
		}
		if envelope.Method != "" && len(envelope.ID) > 0 {
			// The bridge never grants approvals or supplies interactive answers.
			// Rejecting server requests preserves the runtime's safety boundary and
			// lets the turn report that an interactive client is required.
			_ = c.write(map[string]any{
				"jsonrpc": "2.0", "id": json.RawMessage(envelope.ID),
				"error": map[string]any{"code": -32001, "message": "interactive request unavailable through Tintwire bridge"},
			})
			continue
		}
		if envelope.Method == "turn/completed" || envelope.Method == "thread/status/changed" {
			c.events <- envelope
			continue
		}
		var id int64
		if json.Unmarshal(envelope.ID, &id) != nil {
			continue
		}
		c.mu.Lock()
		response := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if response != nil {
			response <- envelope
		}
	}
	err := scanner.Err()
	if err == nil {
		err = errors.New("codex app-server stopped")
	}
	c.mu.Lock()
	c.readErr = err
	c.mu.Unlock()
	close(c.done)
}

func (c *CodexClient) readerError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr == nil {
		return errors.New("codex app-server stopped")
	}
	return c.readErr
}

func (c *CodexClient) write(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(encoded, '\n'))
	return err
}

func (c *CodexClient) Close() error {
	_ = c.stdin.Close()
	if c.command.Process != nil {
		_ = c.command.Process.Kill()
	}
	return c.command.Wait()
}
