# Security and privacy boundaries

The repository [security policy](../SECURITY.md) explains private vulnerability
reporting. This document describes the runtime boundaries operators and
integrators must preserve.

## Authentication and authorization

- Loopback development without reader authentication is not a public mode.
- Browser authentication uses a local password or OIDC. Exact public origin,
  state, nonce, PKCE, issuer, and audience checks are security controls.
- Desktop OIDC handoffs remain pending until the authenticated system browser
  approves them using an independent HttpOnly secret. Compare the short code in
  the desktop app and browser; cancel any sign-in you did not initiate.
- Private channels require explicit membership or installation-administrator
  access. The same check applies to history, activity, unread state, images,
  actions, and push recipients.
- MCP agents are separate principals with bounded roles. Delegation through
  Switchboard must preserve the existing agent subject and configured trust.
- Publishing tokens are channel-scoped secret credentials. Store the URL or
  bearer value only where the producer needs it; Tintwire cannot recover it
  from the stored hash.

## Stored and transmitted data

Notification text, cards, images, action context, user identities, read state,
push endpoints, and audit events can all be sensitive. TLS protects network
transport; database access controls and backup encryption protect durable
copies.

Action credentials use AES-GCM before storage, but the encryption key is
committed to the same database for multi-node consistency. A complete database
read or dump therefore defeats that separation. The encryption primarily
limits accidental API exposure, not a database compromise.

Web Push sends the minimum payload needed by the client policy, but provider
metadata and subscription endpoints remain sensitive. Keep application-specific
recipient selection, urgency, TTL, and caching decisions in Tintwire.

## External content and callbacks

Remote card images reveal view timing and the reader network address to the
image host unless the authenticated image proxy is configured. The proxy
requires an exact source mapping, checks reader visibility, rejects traversal
and redirects, limits type/size/time/concurrency, and never persists a copy.

Registered actions can affect other systems. Destinations are exact,
re-resolved at dispatch, and denied for loopback, link-local, or private
addresses unless the registration explicitly allows a private target. Keep
egress policy outside the application as a second boundary.

## Logging and diagnostics

Logs and support bundles must not contain:

- webhook or channel publishing URLs;
- session cookies, OAuth tokens, push endpoints, or VAPID private keys;
- database connection strings or action credentials;
- private notification bodies, image URLs, channel membership, or identity
  subjects unless explicitly required and redacted.

Use synthetic channels and `.example` identities when reproducing a problem.
Share suspected vulnerabilities only through GitHub private vulnerability
reporting.

## Operator review checklist

- TLS and the exact public origin are enforced.
- Every human and agent has the minimum role and channel access.
- Incoming webhooks and sessions can be revoked without deleting history.
- Database users, dumps, and replicas are treated as credential-bearing.
- Remote image sources and private action destinations are explicitly trusted.
- Push behavior is tested on real devices without exposing private payloads.
- Backups have bounded retention and a verified isolated restore procedure.
