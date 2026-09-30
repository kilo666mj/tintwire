package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kilo666mj/tintwire/internal/store"
)

func queueTools() []mcpTool {
	tools := []mcpTool{{Name: "commands.pending.v1", Title: "Check for queued commands", Description: "Read-only wakeup check for a bound agent channel. Idle polling does not create idempotency records.", InputSchema: json.RawMessage(`{"type":"object","properties":{"channel":{"type":"string","maxLength":64}},"required":["channel"],"additionalProperties":false}`), Annotations: map[string]any{"readOnlyHint": true}}}
	for _, name := range []string{"commands.claim.v1", "commands.renew.v1", "commands.complete.v1"} {
		tools = append(tools, mcpTool{Name: name, Title: name, Description: "Durable agent command delivery. Claim serializes a bound channel; renew every 20 seconds. Interrupted commands must be reconciled with the runtime, never replayed. Complete stores the result and threaded reply atomically.", InputSchema: json.RawMessage(`{"type":"object","properties":{"channel":{"type":"string"},"id":{"type":"string"},"owner":{"type":"string","minLength":8,"maxLength":128},"turn_id":{"type":"string","maxLength":200},"state":{"type":"string","enum":["completed","failed","cancelled"]},"result":{"type":"string","maxLength":4000},"idempotency_key":{"type":"string","minLength":8,"maxLength":128}},"required":["owner","idempotency_key"],"additionalProperties":false}`), Annotations: map[string]any{"readOnlyHint": false, "idempotentHint": true, "destructiveHint": false}})
	}
	return tools
}

func (s *Server) queueTool(r *http.Request, agent store.Agent, name string, args json.RawMessage) (any, *rpcError) {
	var input struct {
		Channel string `json:"channel"`
		ID      string `json:"id"`
		Owner   string `json:"owner"`
		TurnID  string `json:"turn_id"`
		State   string `json:"state"`
		Result  string `json:"result"`
		Key     string `json:"idempotency_key"`
	}
	if err := decodeToolArguments(args, &input); err != nil {
		return toolFailure(err.Error()), nil
	}
	if name == "commands.pending.v1" {
		pending, err := s.store.AgentCommandPending(r.Context(), agent, input.Channel)
		if err != nil {
			return toolFailure(err.Error()), nil
		}
		return toolSuccess(map[string]bool{"pending": pending}), nil
	}
	return s.mcpMutate(r, agent, name, input.Key, args, func() (any, string, error) {
		if s.consensus != nil {
			return nil, "", errors.New("durable queues require standalone SQLite or shared PostgreSQL")
		}
		switch name {
		case "commands.claim.v1":
			c, err := s.store.ClaimAgentCommand(r.Context(), agent, input.Channel, input.Owner)
			return map[string]any{"command": c}, "", err
		case "commands.renew.v1":
			c, err := s.store.RenewAgentCommand(r.Context(), agent, input.ID, input.Owner, input.TurnID)
			return map[string]any{"command": c}, "", err
		case "commands.complete.v1":
			id, err := s.store.CompleteAgentCommand(r.Context(), agent, input.ID, input.Owner, input.State, input.Result)
			if err == nil {
				s.publishMessage(id)
			}
			return map[string]any{"message_id": id}, "completed an agent command", err
		}
		return nil, "", errors.New("unknown queue operation")
	})
}

func (s *Server) bindAgent(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Agent string `json:"agent"`
	}
	if !workflowDecode(w, r, &input) {
		return
	}
	if err := s.store.BindAgent(r.Context(), u, r.PathValue("id"), input.Agent); err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listAgentCommands(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	values, err := s.store.AgentCommands(r.Context(), u, r.PathValue("id"))
	if err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"commands": values})
}

func (s *Server) controlAgentCommand(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	var input struct {
		Action string `json:"action"`
	}
	if !workflowDecode(w, r, &input) {
		return
	}
	if err := s.store.ControlAgentCommand(r.Context(), u, r.PathValue("id"), input.Action); err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
