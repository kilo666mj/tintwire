# Production operations and recovery

Tintwire deliberately leaves infrastructure policy to the operator. This
runbook defines the application evidence a production deployment should
collect without prescribing one cloud, proxy, or PostgreSQL distribution.

## Production baseline

- Run a tagged or reviewed commit consistently on every application node.
- Use PostgreSQL for a multi-node deployment:

  ```sh
  TINTWIRE_DB_DRIVER=postgres
  TINTWIRE_DB='postgres://tintwire:REDACTED@db.example:5432/tintwire?sslmode=verify-full'
  TINTWIRE_LISTEN=127.0.0.1:8080
  TINTWIRE_PUBLIC_URL=https://tintwire.example.com
  ```

- Terminate TLS at a reverse proxy that forwards only to healthy loopback or
  private listeners.
- Enable a reader password or OIDC before exposing the service. Use exact
  origins and callback URLs.
- Keep database, webhook, VAPID, OIDC, and action-encryption material in
  root-owned files or the deployment secret store.
- Monitor each app node, the public route, PostgreSQL, Web Push results, and
  backup freshness independently.

`db.example` and `tintwire.example.com` are reserved examples. Do not copy the
placeholder credential into a deployment.

## Routine checks

Daily:

- verify every application target and the public readiness route;
- publish one synthetic card to a dedicated test channel and confirm it can be
  read, marked read, and found in history;
- inspect failed action events, OIDC errors, Web Push provider rejections, and
  database connection failures;
- confirm PostgreSQL replication and backup freshness using the database
  platform runbook.

Weekly:

- compare every node's binary/source version and environment template;
- restore the newest database dump into an isolated database and run the
  application migration/startup path against it without public ingress;
- verify a local-reader login and OIDC login where both are configured;
- review administrators, agents, channel membership, active sessions, incoming
  webhooks, and action targets.

For Web Push changes, provider acceptance is not proof of delivery. Exercise
Chrome/Firefox as applicable and verify physical iPhone receipt separately.
Use [client validation](CLIENT_VALIDATION.md) as the evidence checklist.

## Backup scope

The PostgreSQL database contains notifications, messages, channels, user and
agent identities, sessions, Web Push subscriptions, audit events, action
targets, encrypted action credentials, and the committed action-encryption
key. A database reader or dump therefore has enough material to decrypt those
stored credentials. Treat every dump as secret data.

Also preserve, through the deployment secret store:

- database credentials and trust roots;
- OIDC client configuration;
- VAPID private material;
- any bootstrap action key still required during migration;
- reverse-proxy and TLS recovery configuration.

Do not assume app-node disks contain a recoverable multi-node installation.
SQLite deployments must back up the database atomically while writes are
quiesced or through a SQLite-aware backup procedure.

## Restore rehearsal

1. Restore the database to an isolated endpoint.
2. Start exactly one application node with public ingress and outbound action
   dispatch disabled;
3. Allow migrations to complete and inspect errors;
4. Verify users, channels, notification history, unread cursors, audit events,
   action targets, and stored settings;
5. Verify encrypted credentials can be read only through their intended use,
   never by printing them;
6. Record the backup timestamp, application commit, schema version, and restore
   duration, then destroy the rehearsal environment securely.

## Upgrade and rollback

1. Read release notes and run `go test ./...` plus client checks.
2. Take and verify a fresh database backup.
3. Remove one application node from the pool, upgrade it, and verify startup,
   migrations, login, history, and a synthetic publish.
4. Roll through the remaining nodes while retaining enough healthy capacity.
5. Verify public health, node versions, push delivery, and database health.

Database migrations may make an old binary unsafe. Rollback means restoring a
known compatible application/database pair according to the release notes, not
blindly starting an old executable against a newer schema.

## Troubleshooting

| Symptom | Check first |
| --- | --- |
| Public route unhealthy | Node readiness, reverse-proxy upstream selection, exact `TINTWIRE_PUBLIC_URL`, and database reachability |
| Login loop or callback error | Issuer, client ID, registered callback, exact origin, clock, and proxy scheme/host handling |
| One node behaves differently | Commit, environment template, embedded web assets, and connection string |
| Notifications publish but do not push | Reader preference, channel authorization, subscription ownership, VAPID configuration, provider response, and physical-device receipt |
| Actions return `503` | Stored `action_encryption_key`, valid bootstrap material during migration, and destination registration |
| Historical content missing | Selected channel, read-state/lifecycle filters, pagination cursor, and authorization before assuming data loss |

Never repair an outage by exposing unauthenticated reader mode, disabling
origin checks, printing stored credentials, or bypassing channel authorization.
