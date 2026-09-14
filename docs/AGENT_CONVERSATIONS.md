# Agent conversations and remote control

Design note for continuing an existing agent conversation from a Tintwire
channel and issuing commands to that agent.

## Desired outcome

A person can bind a Tintwire channel to an existing agent session, send that
agent messages or explicit control commands from Tintwire, and receive its
responses in the same channel. The first intended runtime is an existing Codex
session, subject to a supported API for resuming or steering that session.

The interaction should feel like a normal conversation rather than a series of
unrelated notifications. If the agent is already working, new messages should
queue by default. Interrupting active work must be explicit.

## Current state

Tintwire already has most of the presentation and authorization primitives:

- authenticated human-authored channel messages and threaded replies;
- a merged timeline containing messages, notifications, and command output;
- first-class agent principals with explicit channel grants;
- an authenticated, sessionless MCP endpoint;
- MCP tools for listing channels and reading or publishing notifications;
- durable agent runs and externally visible effect records; and
- browser SSE events for new channel messages.

The current MCP surface is notification-oriented. It cannot list or read human
channel messages, publish a channel message or reply, or subscribe to new
messages. A Tintwire agent run is an audit record; it is not a live model or
runtime session.

MCP also runs in the opposite direction from inbound control: an agent calls
Tintwire's MCP server. Tintwire cannot use that connection to inject a new turn
into an idle or running agent. No currently available Switchboard capability
exposes general agent-session resume or steering.

## Proposed architecture

```text
Human in Tintwire
       |
       | addressed message or control command
       v
Durable Tintwire command queue
       |
       | claim/acknowledge
       v
Agent-runtime bridge
       |
       | resume or steer a bound runtime session
       v
Existing agent session
       |
       | conversational reply through Tintwire MCP
       v
Tintwire channel timeline
```

Tintwire should own channel bindings, authorization, command persistence, and
delivery state. A separate runtime bridge should own runtime credentials and
the opaque external session handle. This keeps Tintwire independent of Codex or
any other particular agent implementation.

### Channel binding

A binding associates a Tintwire channel, a Tintwire agent principal, a runtime
type, and an opaque external session reference. The external session reference
must not grant authority by itself and should not be exposed to ordinary channel
readers.

The owner or an installation administrator can attach or detach a session.
Channel membership alone should not allow changing the binding.

### Inbound commands

Only deliberate input should become an agent instruction. Possible initial
rules are:

- in a dedicated agent channel, every human-authored top-level message is input;
- elsewhere, only an explicit agent mention or `/agent` command is input; and
- notification content, command output, webhook content, bot messages, and the
  agent's own messages are never reinterpreted as commands.

Each accepted input becomes a durable queue item with its author, channel,
message ID, binding ID, creation time, delivery state, and idempotency key.
The bridge claims inputs in order and records delivery and completion without
executing an item twice.

### Runtime bridge

The bridge needs a runtime-specific adapter with operations equivalent to:

- resolve or validate a session reference;
- report whether the session is idle, busy, unavailable, or finished;
- enqueue a user turn;
- explicitly interrupt active work;
- start a replacement session when requested; and
- receive the agent's progress and final response.

An ordinary message queues while the session is busy. Only an authorized,
explicit interrupt command may steer or cancel active work. If the runtime does
not support resuming an existing session, the adapter must report that clearly
rather than silently create a new one.

### MCP additions

The exact schemas still need review, but the minimal conversational MCP surface
is likely:

- `messages.list.v1`: read authorized channel messages using a stable cursor;
- `messages.get.v1`: resolve one message and its thread context;
- `messages.publish.v1`: publish an agent-authored message or threaded reply;
- `commands.claim.v1`: atomically claim pending inputs for this agent binding;
- `commands.complete.v1`: record delivery/result state; and
- optionally, a read-only binding/status resource.

Every mutation must use Tintwire's existing durable idempotency policy. Agent
responses should retain both the Tintwire agent identity and the associated run
or command ID for auditability.

Polling the command queue is sufficient for an initial implementation. A later
webhook or event stream may reduce latency, but the bridge remains the component
that wakes or resumes the agent runtime.

## User experience

The channel header should show the bound agent and its state, for example
**Idle**, **Working**, **Waiting for approval**, **Offline**, or **Detached**.
Agent output should be posted as ordinary attributed timeline messages, with
replies threaded under the human input where practical.

Suggested control commands:

- `/agent status`
- `/agent interrupt`
- `/agent new`
- `/agent detach`

High-impact operations still follow the agent runtime's normal approval model.
A command delivered through Tintwire must not imply approval, widen filesystem
or network access, or bypass tool safety checks.

## Security requirements

- Authorize command submission separately from merely reading a channel.
- Preserve the authenticated human author as structured provenance.
- Treat only the selected human message as instruction; surrounding timeline
  and producer content remain untrusted context.
- Keep runtime credentials outside Tintwire and never store them in messages,
  runs, or logs.
- Do not expose opaque runtime session references to ordinary readers.
- Serialize turns per binding and use leases so two bridges cannot drive the
  same session concurrently.
- Make interrupt, detach, and new-session operations explicit and auditable.
- Avoid agent feedback loops by excluding agent-authored and generated timeline
  entries from the inbound queue.

## Open questions

1. Which supported Codex API or local control surface can resume and steer an
   existing session?
2. Should the first version use dedicated agent channels, explicit mentions, or
   both?
3. Which channel roles may submit ordinary turns, and which may interrupt,
   detach, or replace a session?
4. Should progress updates be persisted as messages, represented as transient
   status, or limited to the final answer?
5. How should approvals requested by the runtime be represented and answered in
   Tintwire without weakening the runtime's approval boundary?

## Suggested implementation sequence

1. Confirm the Codex runtime's supported resume, input, interrupt, status, and
   response interfaces.
2. Define channel-to-agent-session bindings and their authorization policy.
3. Add the durable command queue and lease/state transitions.
4. Expose message publishing and command claiming/completion through MCP.
5. Implement a Codex bridge that polls, resumes the bound session, and publishes
   replies.
6. Add channel binding/status UI and explicit control commands.
7. Test ordering, retries, reconnects, concurrent bridge claims, revocation,
   prompt-injection boundaries, and agent feedback-loop prevention.

