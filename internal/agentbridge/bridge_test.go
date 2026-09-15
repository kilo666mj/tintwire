package agentbridge

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompletedReplyPrefersFinalAnswer(t *testing.T) {
	params := json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[{"type":"agentMessage","text":"working","phase":"commentary"},{"type":"agentMessage","text":"done","phase":"final_answer"}]}}`)
	reply, matched, err := completedReply(params, "thread-1", "turn-1")
	if err != nil || !matched || reply != "done" {
		t.Fatalf("completed reply = %q, %v, %v", reply, matched, err)
	}
	if _, matched, _ := completedReply(params, "thread-2", "turn-1"); matched {
		t.Fatal("completion from another thread matched")
	}
}

func TestCompletedReplyReportsFailure(t *testing.T) {
	params := json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed","error":{"message":"approval unavailable"},"items":[]}}`)
	_, matched, err := completedReply(params, "thread-1", "turn-1")
	if !matched || err == nil || !strings.Contains(err.Error(), "approval unavailable") {
		t.Fatalf("failure = matched %v, error %v", matched, err)
	}
}

func TestLimitMessagePreservesUTF8AndLimit(t *testing.T) {
	result := limitMessage(strings.Repeat("é", 3000), 4000)
	if len(result) > 4000 || !utf8.ValidString(result) || !strings.Contains(result, "response truncated") {
		t.Fatalf("invalid truncated response: bytes=%d utf8=%v", len(result), utf8.ValidString(result))
	}
}
