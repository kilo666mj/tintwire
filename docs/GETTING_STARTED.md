# Getting started and administration

Development setup, authentication, channel administration, native cards, actions, and incoming webhooks.

## Development quick start

Tintwire is a single Go service that serves the web client itself and supports
SQLite for local development or PostgreSQL for multi-node deployments. The
quickest way in is one webhook-backed
channel with no authentication.

Start it with an explicit development webhook token:

```sh
export TINTWIRE_HOOK_TOKEN=local-development-hook
go run ./cmd/tintwire -hook-id "$TINTWIRE_HOOK_TOKEN"
```

Then publish a notification:

```sh
curl -i \
  -H 'Content-Type: application/json' \
  -d '{"text":"Hello from Mattermost","username":"example-bot"}' \
  http://127.0.0.1:8080/hooks/local-development-hook
```

Open `http://127.0.0.1:8080/` to see the notification. The default database is
`tintwire.db`; the service listens only on loopback unless `-listen` or
`TINTWIRE_LISTEN` is set. The webhook token is a secret URL credential and is
stored only as a SHA-256 hash.

The same channel-scoped token can publish a version 1 native card:

```sh
curl -i \
  -H "Authorization: Bearer $TINTWIRE_HOOK_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"version":1,"channel":"#release-lists","title":"Daily release summary","summary":"3 unique from 4 entries","severity":"info","source":"release_watcher","metrics":[{"label":"Unique","value":3}],"rows":[{"primary":"Example.Show.S01E01","tags":["Source A","Source B"],"emphasis":"strong"}]}' \
  http://127.0.0.1:8080/api/v1/notifications
```

Native cards support a title, summary, severity, source, metrics, labeled
fields, tone-limited badges, lazy-loaded images with required alternative text,
safe HTTP(S) links, filterable tagged rows, source and cross-list row views, and
registered or link actions. An optional `channel` selects a public destination
when the publishing webhook is unlocked; locked webhooks remain scoped to their
configured channel. Component counts and text lengths are bounded;
unknown fields, unsafe URL schemes, and unsupported component types are rejected
so producer content remains declarative and script-free. Synthetic redacted
contracts for a release summary, `approval-service`, and slash commands live in
`testdata/compat`; the interactive rendering reference is in `docs/mockups`.
Remote images use a no-referrer policy, but loading a producer-selected image
URL still reveals the approximate view time and reader network address to that
image host. Disable or proxy remote images at the deployment boundary when
producers are not trusted with that privacy signal.

The inbox search bar searches compatibility text, attachment content, native
card content, sources, and channel names. Channel, lifecycle state, and native
severity filters can be combined; the corresponding API query parameters are
`q`, `channel`, `state`, `severity`, and `limit` (maximum 200). History uses the
opaque `next_cursor` returned by the API as the subsequent `before` parameter;
the inbox exposes this as **Load more**.

Simple scripts can publish either plain text or a small generic JSON message to
`POST /api/v1/messages` with the same bearer token:

```sh
curl -H "Authorization: Bearer $TINTWIRE_HOOK_TOKEN" \
  -H 'Content-Type: text/plain' \
  --data 'nightly job completed' \
  http://127.0.0.1:8080/api/v1/messages
```

The JSON form accepts `text` and an optional `source`; additional fields are
rejected instead of being interpreted as markup.

Set a reader password to protect inbox, activity, realtime, and push APIs with a
server-side login session:

```sh
TINTWIRE_READER_USERNAME=admin \
TINTWIRE_READER_PASSWORD='use-a-long-unique-password' \
TINTWIRE_PUBLIC_URL='http://127.0.0.1:8080' \
go run ./cmd/tintwire -hook-id local-development-hook
```

Reader passwords must contain at least 12 characters. Passwords are stored as
bcrypt hashes; random session credentials are stored only as SHA-256 hashes and
sent in HttpOnly, SameSite=Strict cookies. Omitting the reader password retains
the loopback development mode and logs a warning. Do not expose that mode beyond
a trusted access boundary.

Authentication requires `TINTWIRE_PUBLIC_URL`; set it to the exact browser
origin, for example `https://tintwire.example.com` or the local origin shown
above. Tintwire fails closed at startup without it, requires that origin and host
on browser mutations, and marks session cookies `Secure` for HTTPS origins even
when the loopback proxy connection is plain HTTP. It deliberately does not trust
arbitrary `X-Forwarded-*` headers.

Interactive Pocket ID sign-in is available when `TINTWIRE_OAUTH_ISSUER` and
`TINTWIRE_OIDC_CLIENT_ID` are set. Register the callback
`$TINTWIRE_PUBLIC_URL/api/v1/auth/oidc/callback` as a public client with PKCE,
then use **Continue with Pocket ID** on the login screen. Tintwire verifies the
authorization code, PKCE challenge, issuer, audience, nonce, and one-time state.
The first successful login provisions a non-administrator local reader keyed by
the immutable OIDC subject; it never links an existing local account by display
name or email. Promote or grant channel membership through the normal Tintwire
administrator controls.

Authenticated readers have durable per-channel read cursors. New and updated
cards are highlighted, the PWA badge reflects the unread total where supported,
and the clickable channel navigation shows total, unread, and actively firing
counts. Channel selection is retained in the URL and rendered as a desktop
sidebar or a compact mobile channel picker. **Mark channel read** advances the
selected channel cursor; **Mark all read** advances all channel cursors
atomically. Channel timelines open in **Unread only** mode; the read-state
selector switches to **Read & unread** for complete history. **Mark read**
removes an item from the unread view but keeps it in history. From complete
history, **Archive** hides a notification from normal history; the **Archived**
lifecycle filter finds and restores it. Web Push
subscriptions are attached to the authenticated reader and can only be revoked
by their owner.

Installation administrators can create additional readers with
`POST /api/v1/users`, create public or private channels with
`POST /api/v1/channels`, and assign `viewer`, `operator`, or `channel_admin`
membership with `PUT /api/v1/channels/{id}/members/{username}`. Channel creation
returns a random channel-scoped publishing token exactly once; only its SHA-256
hash is retained. Private channels are excluded from notification queries and
channel navigation unless the reader is an installation administrator or an
explicit member. The selected-channel **Edit** action manages the display name,
description, accent color, and visibility; the URL-safe channel name remains
immutable.

The admin-only **Users** panel lists human and system identities and their
authentication type. It can promote or demote administrators, enable or disable
human access, revoke sessions, reset local passwords, and assign explicit
channel roles. It prevents self-disablement and removal of the final enabled
human administrator; changes are recorded in the administrative audit log.

Operators and channel administrators can acknowledge or resolve notifications
from the inbox. Transitions are validated (`received`/`firing` to
`acknowledged` or `resolved`, then `acknowledged` to `resolved`), repeated
requests are idempotent, and each accepted transition becomes an immutable
activity event attributed to the authenticated reader. Private-channel activity
history applies the same membership check as the main feed.

Native cards may also contain registered HTTP actions. Configure a separate
installation encryption key before publishing or registering them:

```sh
export TINTWIRE_ACTION_KEY="$(openssl rand -base64 32)"
```

`TINTWIRE_ACTION_KEY` is bootstrap material only with respect to the environment
file. The first action-target save
or slash-command import commits it to cluster application settings as
`action_encryption_key` before storing the encrypted credential. Runtime
encryption and decryption use only that stored setting, so
application nodes cannot silently fall back to different node-local keys. If
neither the setting nor valid bootstrap material is
available, the server still starts but credential operations return `503`.

The committed key is stored in the same database as the ciphertext. Database
read access, a database dump, or a backup containing `app_settings` therefore
provides the material needed to decrypt stored action-target and slash-command
credentials. Protect database accounts, dumps, and every backup copy as secret
credential material; database encryption here protects accidental API exposure,
not an attacker who can read the complete database.

For legacy SQLite installations, back up the bootstrap key separately from the
database until the committed setting has been verified. When upgrading an
installation that already has encrypted credentials but no
`action_encryption_key` setting, use the legacy key that encrypted those
credentials as the bootstrap value and re-save each action target and imported
slash command before relying on failover. Do not seed the setting from an
arbitrary replica's environment. Credentials previously written with different
node-local keys must be re-entered under one chosen canonical key.

Administrators register an exact callback destination with
`PUT /api/v1/action-targets/{name}`; HTTPS is required unless the request
explicitly opts into a private target. `DELETE /api/v1/action-targets/{name}`
revokes a target cluster-wide; existing cards remain in history but can no
longer invoke it. Optional bearer credentials and per-card callback context are
AES-GCM encrypted before storage and are never returned by inbox APIs. The
dispatcher resolves and revalidates destination addresses,
blocks private/link-local/loopback addresses unless explicitly allowed, refuses
redirects, limits callback time and response size, forwards an idempotency key,
and records an immutable success or failure event. Clients must send a unique
`Idempotency-Key` when invoking
`POST /api/v1/notifications/{id}/actions/{index}`.

Existing Mattermost hook credentials can be planned and imported through
`POST /api/v1/admin/import/webhooks`. The strict JSON request contains
`dry_run` and a `webhooks` array of `{id, channel, channel_locked}` mappings.
`channel_locked` defaults to `false` when omitted. An unlocked imported hook may
override its destination only to an existing public Tintwire channel. Imports are atomic
and idempotent; duplicate IDs, unknown channels, and attempts to remap an ID
fail closed. Responses report only created/existing counts and never echo hook
IDs.

Authenticated installation administrators can also manage incoming webhooks in
the **Automation** panel. `POST /api/v1/webhooks` creates a random
channel-scoped hook and returns its `/hooks/{token}` path exactly once;
`GET /api/v1/webhooks` returns only non-secret metadata; and
`POST /api/v1/webhooks/{id}/revoke` immediately disables future deliveries
without deleting notifications previously received through that hook. Payload
channel overrides are enabled by default and remain limited to existing public
channels; administrators can lock or unlock each hook in the **Automation** panel.
Because stored secrets are hashed and cannot be copied later, each active row
also offers **New URL**. It creates another active URL with the same channel and
override policy, then shows it once with a copy control. Existing URLs remain
active until explicitly revoked. The Automation panel groups all URLs for a
channel into one card while retaining per-URL policy and revocation controls.
