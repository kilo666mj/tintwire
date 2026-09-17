# Architecture and failure behavior

Tintwire is one Go service with an embedded web client. It accepts structured
notifications and compatibility webhooks, stores durable application state,
serves authenticated readers, dispatches registered actions, and exposes MCP
tools for approved agent principals.

## Deployment shapes

SQLite is for local development and deliberate single-node installations.
PostgreSQL is the shared state layer for multi-node deployments. Application
nodes are stateless with respect to durable inbox data: they serve the same
database behind one TLS reverse proxy or load balancer.

```text
publishers and agents
        │
        ▼
TLS reverse proxy / load balancer
        │
  ┌─────┼─────┐
  ▼     ▼     ▼
app-1 app-2 app-3
  └─────┼─────┘
        ▼
PostgreSQL HA
```

The application does not provide PostgreSQL failover, ingress, TLS
termination, or backup scheduling. Those are operator responsibilities. Legacy
SQLite replication and control-Raft settings are retained for compatibility
and development history; they are not part of the recommended PostgreSQL
deployment.

## Ownership boundaries

| Layer | Owns |
| --- | --- |
| Tintwire | Channels, notifications, messages, read state, users, sessions, Web Push subscriptions, action targets, audit events, MCP authorization |
| PostgreSQL | Durable shared application state and transaction ordering |
| Reverse proxy | Public TLS, request limits, trusted routing, and node health selection |
| Identity provider | Browser and MCP identity assertions |
| pwa-kit | Shared permission, subscription, VAPID, and notification-click browser behavior |
| Operator | Database HA, backups, restore tests, deployment rollout, monitoring, secret distribution, and retention policy |

Application-owned adapters keep subscription ownership, notification policy,
recipient selection, service-worker caching, and app-specific TTL/urgency in
Tintwire even though the common browser transport comes from pwa-kit.

## Write and delivery paths

Publishers use channel-scoped webhook tokens. The token is returned once and
only its SHA-256 hash is stored. Native cards and compatibility payloads are
validated and committed before realtime, unread, badge, or Web Push delivery
is attempted.

Interactive action destinations are registered separately. Optional
credentials and per-card context are encrypted before storage, destinations
are resolved and revalidated, private networks require an explicit opt-in,
redirects are refused, and every invocation requires an idempotency key.

## Failure behavior

| Failure | Expected behavior | Operator response |
| --- | --- | --- |
| One app node stops | Healthy peers continue against shared PostgreSQL | Remove the node, inspect logs, and replace it without restoring local app data |
| PostgreSQL is unavailable | Durable reads and writes fail; app-node restarts do not repair it | Follow the database HA runbook and verify consistency before restoring traffic |
| Web Push provider rejects delivery | The durable inbox remains authoritative | Inspect subscription/provider results and verify receipt on a physical device |
| Action target times out | Invocation records a bounded failure event; no redirect or unlimited response is accepted | Repair the target and retry only with a new deliberate idempotency decision |
| OIDC is unavailable | Existing valid sessions may continue until expiry; new OIDC logins fail | Restore the provider; do not enable unauthenticated reader mode publicly |
| Remote image source disappears | The authenticated image proxy returns an error; the card remains | Restore the source or accept the missing non-durable image |
| One node has stale configuration | Startup or behavior may differ despite shared data | Reconcile the node from the same release and environment template before returning it to the pool |

## Related references

- [Getting started and administration](GETTING_STARTED.md)
- [Production operations and recovery](OPERATIONS.md)
- [Security and privacy boundaries](SECURITY_AND_PRIVACY.md)
- [Agents and MCP](AGENTS_AND_MCP.md)
- [Client validation](CLIENT_VALIDATION.md)

Examples use `.example` names, loopback, and documentation-only addresses.
Keep real notification content, channel names, identity subjects, database
addresses, and webhook tokens out of the public repository.
