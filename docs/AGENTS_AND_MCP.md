# Agents and MCP

Agent principals, permissions, and the authenticated Model Context Protocol surface.

## Agents

Agents are first-class principals rather than shared API tokens. An installation
administrator can register, inspect, and revoke them in the **Automation**
panel or register one with `POST /api/v1/agents`:

```sh
curl -H 'Content-Type: application/json' \
  -d '{"name":"triage","display_name":"Triage","description":"Investigates alerts"}' \
  https://tintwire.example.com/api/v1/agents
```

Agent administration requires an authenticated installation administrator, so
reader authentication must be enabled. Registration returns the agent's access
token exactly once; only its SHA-256
hash is retained, and later reads never expose it. Each agent gets its own
principal user named `agent-<name>`, so channel access is granted with the same
`PUT /api/v1/channels/{id}/members/{username}` endpoint used for people. An
agent has no implicit access to any channel, including public ones: publishing
requires an explicit `operator` or `channel_admin` membership, or an agent
registered with `"is_admin": true`.

Agent access tokens are a separate credential class from reader sessions,
channel publishing tokens, and Mattermost bot tokens, and are never accepted as
browser session credentials. `GET /api/v1/agents` lists the directory with
ownership, channel grants, and last credential use.
`POST /api/v1/agents/{name}/revoke` disables the agent, revokes its credentials,
and cancels its open runs without affecting its owner's session or any other
agent.

Work is recorded as durable runs. A run holds its initiator, stated purpose,
lifecycle state, and the externally visible effects it produced; model
reasoning, prompts, and hidden context are not stored. Administrators read run
history with `GET /api/v1/agents/{name}/runs` and per-run effects with
`GET /api/v1/agents/runs/{id}/events`. Notifications an agent publishes are
attributed to both the agent and its run, and the inbox API returns the agent
name in the notification's `agent` field.

## Model Context Protocol

Agents reach Tintwire through a remote MCP endpoint at `POST /mcp`, using the
sessionless Streamable HTTP transport. The deprecated HTTP+SSE transport is not
offered, and batched JSON-RPC requests are rejected. The endpoint speaks
protocol version `2026-07-28` and also accepts `2025-11-25` and `2025-06-18`;
an unsupported `MCP-Protocol-Version` header is refused.

Authentication is the agent's own access token as a bearer credential. Reader
session cookies are never accepted, and the endpoint calls the same store
authorization used by the HTTP API rather than a second privileged path:

```sh
curl -H "Authorization: Bearer $TINTWIRE_AGENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  https://tintwire.example.com/mcp
```

Tool names are versioned: `channels.list.v1`, `notifications.search.v1`,
`notifications.get.v1`, `notifications.publish.v1`,
`notifications.set_state.v1`, `notifications.invoke_action.v1`, `runs.start.v1`, `runs.record.v1`,
`runs.finish.v1`, and, for installation-administrator agents only,
`channels.create.v1`. Every mutating tool requires a stable `idempotency_key`:
a repeat of the same call replays the first result without repeating the effect,
and reusing a key with different arguments is rejected as a conflict. A replayed
`channels.create.v1` result never repeats the publishing token. Tool traffic is
rate limited per agent and per agent and tool.

Read tools return canonical IDs, state, and sanitized presentation text so
agents do not scrape rendered markup; raw compatibility payloads and stored
action credentials are never exposed. Read-only resources are published at
`tintwire://channels`, `tintwire://channels/{name}`,
`tintwire://notifications/{id}`, and `tintwire://notifications/{id}/activity`.

Notification content, attachment text, and activity history are untrusted
producer data. The server states this in its MCP instructions, and enforces
authorization, idempotency, and state-transition policy itself: tool
descriptions and client annotations are never treated as a security boundary.

MCP action invocation uses the same registered-target lookup, channel-operator
authorization, encrypted context, SSRF protection, and durable idempotency path
as the web client. RFC 9728 protected-resource metadata is published at the
standard well-known location and linked from bearer challenges when
`TINTWIRE_PUBLIC_URL` is configured. Tintwire can validate Pocket ID API access
tokens through OIDC discovery and its rotating JWKS. Configure
`TINTWIRE_OAUTH_ISSUER`; the default resource is
`$TINTWIRE_PUBLIC_URL/mcp` and the default required permission is
`tintwire:mcp`. Validation requires a valid signature, exact issuer, expiry,
that resource in `aud`, the permission in `scope`, and a subject mapped to an
enabled Tintwire agent. Existing `twa_` agent tokens remain supported.

In your OIDC provider, create an API whose immutable resource is
`https://tintwire.example.com/mcp`, add the `tintwire:mcp` permission, and grant
the relevant client user-delegated or client-credentials access. Add the
resulting token subject when registering the corresponding agent:

```json
{
  "name": "automation",
  "display_name": "Automation",
  "oauth_subject": "client-tintwire-automation"
}
```

Pocket ID client-credentials tokens use `client-<client-id>` as their subject;
user-delegated tokens use the user's subject. OAuth changes only how the agent
authenticates—the mapped agent principal still supplies all channel membership,
operator, and administrator authorization. ID tokens are never accepted at
`/mcp`.

The verifier uses `coreos/go-oidc` for discovery, signature verification, and
key rotation. Tintwire's MCP endpoint is a resource server and validates access
token audience and permissions.

For interactive browser sign-in, create a separate public PKCE client with
`https://tintwire.example.com/api/v1/auth/oidc/callback` as its callback and
`https://tintwire.example.com/` as its launch URL. Set
`TINTWIRE_OIDC_CLIENT_ID` to that client's ID; no client secret is used.

