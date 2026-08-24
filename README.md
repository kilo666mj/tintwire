# Tintwire

Tintwire is a self-hosted rich-notification inbox for structured, interactive
cards. It is a focused alternative to routing operational notifications through
a general-purpose chat system.

> **Work in progress:** Tintwire is under active development. Interfaces,
> configuration, deployment procedures, and database schemas may change without
> notice. It is not yet recommended for production use without careful review
> and backups.

Tintwire combines:

- Rich cards with semantic color, fields, tables, images, and authenticated
  actions.
- An installable PWA with mobile and desktop Web Push, unread badges, and deep
  links.
- Mattermost and Slack webhook compatibility, including existing
  `/hooks/{id}` URLs.
- Bounded bridges for Mattermost bots and custom slash commands.
- Native typed commands, approvals, durable action events, and agent principals
  with an authenticated MCP surface.
- SQLite for local or single-node use and PostgreSQL for shared multi-node
  deployments.

## Screenshots

[![Tintwire signal inbox populated with synthetic operational notifications](docs/screenshots/inbox.png)](docs/screenshots/inbox.png)

The inbox combines compatibility webhooks and native structured cards with
channel, severity, lifecycle, search, and unread controls.

[![Tintwire release-summary channel displaying a synthetic structured card](docs/screenshots/release-summary.png)](docs/screenshots/release-summary.png)

Channel timelines present filterable card rows alongside ordinary conversation
and the channel composer.

## Quick start

Tintwire is a single Go service that serves its own web client. Start a local
SQLite-backed instance with one development webhook:

```sh
export TINTWIRE_HOOK_TOKEN=local-development-hook
go run ./cmd/tintwire -hook-id "$TINTWIRE_HOOK_TOKEN"
```

Publish a compatible notification:

```sh
curl -i \
  -H 'Content-Type: application/json' \
  -d '{"text":"Hello from Mattermost","username":"example-bot"}' \
  http://127.0.0.1:8080/hooks/local-development-hook
```

Open `http://127.0.0.1:8080/`. The default database is `tintwire.db`; the
service listens only on loopback unless `-listen` or `TINTWIRE_LISTEN` is
set. The webhook token is a secret URL credential and is stored only as a
SHA-256 hash.

A native version 1 card uses the same channel-scoped token:

```sh
curl -i \
  -H "Authorization: Bearer $TINTWIRE_HOOK_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"version":1,"channel":"#release-lists","title":"Daily release summary","summary":"3 unique releases","severity":"info","source":"release_watcher"}' \
  http://127.0.0.1:8080/api/v1/notifications
```

This unauthenticated reader mode is for loopback development only. Configure a
reader password or OIDC before exposing Tintwire beyond a trusted access
boundary; production authentication also requires the exact browser origin in
`TINTWIRE_PUBLIC_URL`.

## Documentation

- [Getting started and administration](docs/GETTING_STARTED.md)
- [Mattermost compatibility](docs/MATTERMOST_COMPATIBILITY.md)
- [Agents and MCP](docs/AGENTS_AND_MCP.md)
- [Client behavior](docs/CLIENTS.md)
- [Client validation checklist](docs/CLIENT_VALIDATION.md)
- [Desktop release policy](docs/DESKTOP_RELEASES.md)
- [Mattermost channel parity](docs/MATTERMOST_CHANNEL_PARITY.md)
- [Security policy](SECURITY.md)
- [Contributing](CONTRIBUTING.md)

Synthetic compatibility contracts live in `testdata/compat`; the interactive
card reference lives in `docs/mockups`.

## Project status

Tintwire includes channels and scoped publishing tokens, structured cards,
history, search, filters, unread state, realtime delivery, Web Push, Mattermost
and Slack compatibility, bot and command bridges, notification lifecycle,
authenticated actions, agents, MCP, and a Tauri desktop client.

Deployment topology, ingress, backups, and database failover are intentionally
left to the operator.

## License

MIT. See [LICENSE](LICENSE).
