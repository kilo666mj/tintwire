# Agent conversations and remote control

The local Codex bridge connects a dedicated Tintwire channel to an existing
Codex thread. It starts `codex app-server --stdio`, resumes the configured thread,
and posts final responses as attributed threaded replies. Runtime credentials
and the thread configuration remain on the bridge host.

## Durable queue (schema 30)

The server now provides a durable command queue and exclusive, renewable
60-second command leases. In **Notification tools → Agent queue**, an
installation administrator binds a dedicated channel to an enabled agent that
has operator access. Bindings are fixed to that identity. Existing messages are
not imported; new top-level human messages from channel operators are enqueued
atomically with message creation. Readers with only viewer access can converse
but cannot submit agent commands. Replies, bot messages, and generated content
are excluded.

The bridge uses this queue by default. Create the binding before upgrading the
bridge. `-durable-queue=false` retains the older cursor-based relay temporarily;
that mode still has the crash/replay limitation described below. `-replay-existing`
applies only to that legacy mode. Keep one runtime session per dedicated bridge
identity/channel, and retain its existing thread configuration.

The bridge records the runtime turn ID, renews its lease every ten seconds, and
atomically saves completion together with its threaded response. Expired commands
become interrupted and block newer work. A replacement bridge reads the recorded
runtime turn to recover an existing result or wait for that turn; it never starts
the command again. If the runtime turn is unknown (including a crash before its
ID was recorded), inspect the runtime, stop any remaining work, then use
**Reconcile stopped runtime** to close the command without replay. Submit a new
human message only when another attempt is intended.

**Cancel** cancels queued commands or requests interruption of active work.
Cancellation intent survives lease expiry. **Refresh queue** shows queued,
running, cancelling, interrupted, completed, failed, and cancelled states.
Channel operators may cancel and reconcile; binding/control operations record
the human actor in the administrative audit history. The runtime's sandbox and
approval boundary remain unchanged: this bridge never grants approvals or
answers interactive questions automatically.

The MCP additions are read-only `commands.pending.v1`, `commands.claim.v1`,
`commands.renew.v1`, and
`commands.complete.v1`. Mutations require idempotency keys, current agent/channel
authority, and the current bridge lease owner. Queue HTTP routes are
`PUT /api/v1/channels/{id}/agent-binding`,
`GET /api/v1/channels/{id}/agent-commands`, and
`POST /api/v1/agent-commands/{id}/control` (`cancel` or `reconciled`). Runtime
turn IDs are excluded from the reader-facing queue response.

Cancellation uses `turn/interrupt`; reconciliation uses `thread/read` with
`includeTurns`, verified against generated local app-server schemas and the
[official app-server documentation](https://learn.chatgpt.com/docs/app-server).
This remains an experimental protocol; verify it when upgrading the Codex CLI.

### Run it

Create a dedicated channel, register a non-admin agent, and grant that agent
`operator` membership in only that channel. Bind that channel to the agent in **Notification tools → Agent queue**.
Then identify the existing Codex thread UUID and run:

```sh
export TINTWIRE_URL=https://tintwire.example.com
export TINTWIRE_AGENT_TOKEN='the-token-returned-at-agent-registration'
export TINTWIRE_CHANNEL=agent-chat
export CODEX_THREAD_ID=0199...

go run ./cmd/tintwire-codex-bridge \
  -state /var/lib/tintwire-codex-bridge/agent-chat.json
```

Only new operator messages created after binding enter the queue.
`-replay-existing` applies only with the legacy `-durable-queue=false` option.
The token is accepted only through the environment so it does not appear in the
process argument list. Non-loopback HTTP URLs are rejected; use HTTPS in normal
operation.

### Availability and discovery

The **Agents** sidebar button lists conversation channels advertised by bridges,
filtered by the reader's current channel access. It shows each agent's description,
availability, and last heartbeat, with **Open conversation** linking to its channel.
The channel also shows the agent's availability:

- **Ready**: the bridge can reach an idle runtime and accepts messages.
- **Busy**: the runtime is active or a bridge turn is in progress; follow-up
  messages remain in the channel and are processed in order.
- **Offline**: the bridge reports unavailable, shuts down, or its heartbeat expires.

The Codex bridge calls `agents.heartbeat.v1` every 20 seconds and on bridge state
changes, after resuming the configured thread. Heartbeats expire after 60 seconds;
the browser refreshes every 10 seconds while visible. Runtime status is checked
independently of turn completion. A registered bot or recently used credential
alone does not advertise availability. Older bridges do not appear until upgraded.

Deploy the updated Tintwire server first (schema 30), create the channel binding,
then rebuild and restart the bridge. Each bridge must use a dedicated agent
identity, conversation channel, and configured runtime thread.

Presence is stored in the shared PostgreSQL database so every application node sees
the same state. SQLite standalone instances are supported; legacy SQLite control
snapshots intentionally do not replicate live presence. The directory API is
`GET /api/v1/agent-conversations`; administration stays under **Integrations → Bots**.

### Persistent private deployment

The repository includes a user service and launcher for deployments where the
Tintwire MCP endpoint is deliberately reachable only on the server's loopback
interface. The launcher maintains an SSH local forward, waits for Tintwire's
health endpoint, loads the agent token from a systemd encrypted credential, and
then starts the bridge:

- `systemd/tintwire-codex-bridge.service`
- `scripts/run-tintwire-codex-bridge`

Install the bridge binary in `~/.local/bin`, then use `systemd-creds encrypt
--user` to create the two encrypted credentials referenced by the unit. The
agent-token credential contains only the token. The configuration credential
is a shell environment file defining `TINTWIRE_URL`, `TINTWIRE_CHANNEL`,
`CODEX_THREAD_ID`, and `TINTWIRE_SSH_TARGET`; it may also override the
local-forward specification, binary path, or state path. Encrypting the
configuration protects the Codex thread reference, which is an opaque runtime
handle even though it is not an authentication credential.

The public reverse proxy can continue denying `/mcp`: the default live setup
uses `http://127.0.0.1:18092` and forwards it to the application's loopback
listener on the Tintwire host. The service requires non-interactive SSH access
and a user manager with lingering enabled.

Link and start the checked-in unit, then inspect its status and recent output:

```sh
systemctl --user link "$PWD/systemd/tintwire-codex-bridge.service"
systemctl --user enable --now tintwire-codex-bridge.service
systemctl --user status tintwire-codex-bridge.service
journalctl --user-unit tintwire-codex-bridge.service -n 100
```

Stopping or disabling the bridge does not expose `/mcp`; it only disconnects
the private relay. Re-enable it with the command above after updating the binary
or encrypted configuration.
