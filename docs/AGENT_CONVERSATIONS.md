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
- MCP tools for listing channels, reading human messages, publishing agent
  replies, and reading or publishing notifications;
- durable agent runs and externally visible effect records; and
- browser SSE events for new channel messages.

The MCP surface now includes cursor-based human message reads, individual
message reads, and idempotent agent-attributed message/reply publishing. It does
not yet subscribe to messages or expose a durable command queue. A Tintwire
agent run is an audit record; it is not a live model or runtime session.

MCP also runs in the opposite direction from inbound control: an agent calls
Tintwire's MCP server. Tintwire cannot use that connection to inject a new turn
into an idle or running agent. No currently available Switchboard capability
exposes general agent-session resume or steering.

## Local Codex bridge MVP

`cmd/tintwire-codex-bridge` provides the first local end-to-end path. It polls a
dedicated Tintwire channel through MCP, sends each new human-authored message to
one configured existing Codex thread, waits for the final agent message, and
posts that output as a threaded Tintwire reply.

The bridge starts `codex app-server --stdio` itself, resumes the supplied thread
with `thread/resume`, starts turns with `turn/start`, and reads
`turn/completed`. This protocol was verified against the locally installed
Codex CLI 0.154.0 and its generated JSON schema on 2026-09-15. It is an
experimental Codex CLI surface, so upgrades should regenerate and compare the
schema before rollout. A managed app-server daemon is not required for this
local bridge.

The relay state file records the last accepted cursor and any completed reply
that has not yet been published. Reply publication is idempotent. A process
crash while a model turn is in progress can still cause that turn to be retried;
the durable server-side command queue and lease described below remain the next
reliability milestone.

The bridge never approves runtime requests and never answers interactive
questions on the user's behalf. If Codex asks this headless client for approval
or input, the bridge rejects the request and the resulting turn reports the
failure in Tintwire. Commands that fit the thread's existing sandbox and
approval policy can proceed normally.

### Run it

Create a dedicated channel, register a non-admin agent, and grant that agent
`operator` membership in only that channel. Then identify the existing Codex
thread UUID and run:

```sh
export TINTWIRE_URL=https://tintwire.example.com
export TINTWIRE_AGENT_TOKEN='the-token-returned-at-agent-registration'
export TINTWIRE_CHANNEL=agent-chat
export CODEX_THREAD_ID=0199...

go run ./cmd/tintwire-codex-bridge \
  -state /var/lib/tintwire-codex-bridge/agent-chat.json
```

On first startup, existing channel messages are skipped. Pass
`-replay-existing` only when those messages should deliberately be submitted.
The token is accepted only through the environment so it does not appear in the
process argument list. Non-loopback HTTP URLs are rejected; use HTTPS in normal
operation.

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

The first three tools are implemented; the remaining durable queue tools are
the next step:

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
4. Add command claiming/completion through MCP.
5. Move local bridge configuration into an authorized channel binding.
6. Add channel binding/status UI and explicit control commands.
7. Test leases, reconnects, concurrent bridge claims, revocation,
   prompt-injection boundaries, and agent feedback-loop prevention.
