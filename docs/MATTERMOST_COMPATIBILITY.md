# Mattermost compatibility

Compatibility behavior for Mattermost bots, slash commands, webhooks, and interactive actions.

## Mattermost bot bridge

Administrators can map an existing bot bearer token, Tintwire user, Mattermost
team alias, and channel with `POST /api/v1/admin/import/mattermost-bot`. The
token is stored only as a SHA-256 hash and is restricted to that channel. The
bounded bridge implements:

```text
GET  /api/v4/users/me
GET  /api/v4/users/username/{username}
GET  /api/v4/teams/name/{team}/channels/name/{channel}
GET  /api/v4/channels/{channel_id}/posts?since={timestamp}
POST /api/v4/posts
GET  /api/v4/posts/{post_id}/reactions
```

Top-level bot posts become inbox notifications; `root_id` replies become
immutable activity entries. Post timestamps are monotonic within a channel so
polling cannot miss same-millisecond writes. When a compatibility post contains
an Approve or Reject attachment action, operators see explicit approval controls
that are exposed to legacy bots as the authenticated user's `white_check_mark`
or `x` reaction. Informational posts do not show approval controls.

Mattermost attachment actions are also supported for bounded integrations such
as `approval-service`. An administrator must first register the action's exact
callback URL as an action target. Callback URLs and opaque action context are
replaced with an encrypted target reference before storage and are never exposed
by inbox APIs. A click is authorized against the stored card, reconstructs the
actor, post, and channel identity on the server, and uses the same SSRF-safe,
bounded, no-redirect dispatcher as native HTTP actions. Clients invoke
`POST /api/v1/notifications/{id}/mattermost-actions/{attachment}/{action}` with
a unique `Idempotency-Key`; the compatible response and immutable audit entry
are reused for retries.

## Mattermost custom slash commands

Administrators can atomically import existing command definitions with
`POST /api/v1/admin/import/slash-commands`. Each definition supplies its
Mattermost team, trigger, display and autocomplete metadata, `GET` or `POST`
request URL, command token, and optional `allow_private` flag. The action key is
required: authorization tokens are AES-GCM encrypted, fingerprinted only for
idempotent conflict detection, and never returned by command metadata or result
APIs.

Prefer `POST` command targets. Mattermost-compatible `GET` targets remain
available for legacy integrations, but necessarily place the decrypted command
token and other form fields in the query string, where the target's access logs
may retain them.

Authenticated users run imported commands from the keyboard command box or
`POST /api/v1/commands` with `{team, channel, command, text}`. Tintwire resolves
the selected channel and actor on the server and sends the Mattermost-compatible
form fields, including a fresh `trigger_id` and scoped `response_url`. Outbound
requests use the same DNS/IP validation, explicit private-target opt-in,
no-redirect policy, timeout, and response-size bound as interactive actions.
Command submissions require a unique `Idempotency-Key`; retries replay the
stored result without contacting the integration again, and reuse with different
command data is rejected.
Immediate plain-text and JSON responses are stored in the durable command
history; unsafe `goto_location` schemes are removed. Opaque delayed response
URLs expire after 30 minutes and accept at most five deliveries. Ephemeral
responses remain scoped to the invoking user, while `in_channel` results are
visible only to users allowed to read that channel. Results are available at
`GET /api/v1/commands/{id}/responses`.

The web client runs imported commands and ordinary messages from the selected
channel's composer. Human messages, notification cards, shared or ephemeral
command responses, bot output, and replies render in one authorized timeline;
shared timeline items participate in search, unread state, notification
preferences, and safe deep links.

Mattermost-compatible ingestion accepts JSON, URL-encoded `payload`, and
multipart `payload` request forms containing `text`, `username`,
`icon_url`, and top-level Mattermost `attachments`. A payload-level `channel`
override is honored for unlocked hooks, but only when it names an existing public
channel. New and bootstrapped hooks allow overrides by default; administrators can
lock individual hooks to their configured channel. Attachment colors support Slack/Mattermost `danger`,
`warning`, and `good` values plus validated hex colors. Text renders a restricted,
safe subset of Slack/Mattermost markup: bold text, inline code, line breaks, and
HTTP(S) links. Mattermost pipe tables, including column alignment, render as
responsive HTML tables. The inbox uses a server-sent event stream to refresh
when new notifications arrive. Alertmanager Slack notifications whose titles
begin with `[FIRING:n]` and `[RESOLVED]` are correlated by their normalized title:
the resolved payload updates the existing card while both deliveries remain in
the immutable notification event history. Cards with multiple lifecycle events
show a lazy-loaded activity timeline; its API returns only sanitized presentation
fields and never exposes the stored raw compatibility payload.

Slack Block Kit `header`, `section`, `fields`, `context`, `divider`, `image`,
and link-button `actions` are normalized into the restricted safe renderer.
Unknown block types become visible inert fallback text, and non-HTTP(S) links
are never made clickable.
