package agentbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type Message struct {
	ID          string    `json:"id"`
	ChannelName string    `json:"channel_name"`
	Author      string    `json:"author"`
	ParentID    string    `json:"parent_id"`
	RootID      string    `json:"root_id"`
	Text        string    `json:"text"`
	CreatedAt   time.Time `json:"created_at"`
	Cursor      string    `json:"cursor"`
}

type MessagePage struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor"`
	HasMore    bool      `json:"has_more"`
}

type TintwireClient struct {
	endpoint string
	token    string
	client   *http.Client
	nextID   atomic.Int64
}

func NewTintwireClient(baseURL, token string) (*TintwireClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("TINTWIRE_URL must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("TINTWIRE_URL must use HTTPS unless it points to loopback")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("TINTWIRE_AGENT_TOKEN is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/mcp"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return &TintwireClient{endpoint: parsed.String(), token: token, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func (c *TintwireClient) Initialize(ctx context.Context) error {
	var result json.RawMessage
	return c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2026-07-28",
		"clientInfo":      map[string]string{"name": "tintwire-codex-bridge", "version": "0.1.0"},
		"capabilities":    map[string]any{},
	}, &result)
}

func (c *TintwireClient) ListMessages(ctx context.Context, channel, after string, limit int) (MessagePage, error) {
	arguments := map[string]any{"channel": channel, "limit": limit}
	if after != "" {
		arguments["after"] = after
	}
	var page MessagePage
	if err := c.tool(ctx, "messages.list.v1", arguments, &page); err != nil {
		return MessagePage{}, err
	}
	return page, nil
}

func (c *TintwireClient) PublishReply(ctx context.Context, channel, parentID, text, key string) error {
	arguments := map[string]any{
		"channel": channel, "parent_id": parentID, "text": text, "idempotency_key": key,
	}
	var result struct {
		Message Message `json:"message"`
	}
	return c.tool(ctx, "messages.publish.v1", arguments, &result)
}

func (c *TintwireClient) tool(ctx context.Context, name string, arguments any, target any) error {
	var result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result); err != nil {
		return err
	}
	if result.IsError {
		message := "Tintwire tool call failed"
		if len(result.Content) > 0 && strings.TrimSpace(result.Content[0].Text) != "" {
			message = result.Content[0].Text
		}
		return errors.New(message)
	}
	if len(result.StructuredContent) == 0 {
		return errors.New("tintwire tool returned no structured content")
	}
	if err := json.Unmarshal(result.StructuredContent, target); err != nil {
		return fmt.Errorf("decode Tintwire tool result: %w", err)
	}
	return nil
}

func (c *TintwireClient) rpc(ctx context.Context, method string, params any, target any) error {
	id := c.nextID.Add(1)
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("call Tintwire: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("tintwire returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode Tintwire response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("tintwire RPC error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		return fmt.Errorf("decode Tintwire result: %w", err)
	}
	return nil
}

func (c *TintwireClient) ReportPresence(ctx context.Context, channel, state string) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	var result json.RawMessage
	return c.tool(ctx, "agents.heartbeat.v1", map[string]any{"channel": channel, "state": state, "idempotency_key": "heartbeat-" + hex.EncodeToString(nonce[:])}, &result)
}
