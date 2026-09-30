# Notification workflows

Open **Notification tools** in the sidebar for schedules, grouped incidents,
agent queues, producer monitors, and the integration playground. Monitors and
the playground require an authenticated installation administrator. The web,
PWA, and desktop clients use the same controls.

## Quiet hours, reminders, and digests

**Schedules** stores quiet hours per reader, using an IANA timezone such as
`Europe/Berlin`. Overnight intervals are supported. Critical native cards bypass
quiet hours only when the reader explicitly enables that exception. Channel
muting and critical-only channel preferences still apply. Ordinary messages and
shared command output also respect quiet hours.

Each notification has **Snooze** and **Delivery details** controls. Snoozing
suppresses that reader's alerts for the card until the selected time (up to 30
days). It does not hide the card or change its shared lifecycle. The reminder
marks the card unread and can send Web Push or a native desktop alert. Quiet
hours defer the reminder; archiving the card or losing channel access prevents
its delivery. Cancel reminders from **Schedules**.

Schedule a daily digest for a saved view and a local delivery time. Digests count
notifications updated in the preceding 24 hours, applying the view's channels,
search, lifecycle, severity, and read-state defaults and the reader's current
channel access. They appear in the schedules dialog; nonempty digests also send
Web Push. Desktop clients receive nonempty digest arrivals through their native alert
feed. A digest intentionally covers the explicitly selected view, including
channels whose individual alerts are muted. Quiet hours defer the digest.
Deleting a view stops its schedule on the next worker pass.

Background workers run every ten seconds. Database leases coordinate schedules
across PostgreSQL app nodes; the worker stops with the server process. A crashed
worker's lease expires after five minutes. Reminders and digest history have
stable occurrence IDs. Alert sending remains best effort: a crash between
recording a scheduled occurrence and contacting a provider can lose that push;
the unread reminder or digest history remains available. This is not a guarantee
of device receipt or exactly-once external delivery.

## Incident groups

Native cards may include an `incident_key` of up to 200 bytes:

```json
{
  "version": 1,
  "title": "API error rate increased",
  "source": "metrics",
  "severity": "warning",
  "state": "firing",
  "incident_key": "api-service"
}
```

Compatible webhooks can use `props.incident_key`. The same key from different
producers groups cards **within one channel**. Grouping never merges private
channels or overwrites the underlying cards. `lifecycle_key` continues to update
one producer's individual card; `incident_key` relates several cards.

**Incidents** displays the latest 200 groups, their card and firing counts, and
an expandable chronological list of the latest 200 cards in each group.
**Acknowledge incident** atomically acknowledges all received/firing cards in
that group, including cards outside the displayed page. It requires channel
operator authority, preserves resolved cards, and records the actor on each
notification's activity history.

## Delivery inspection and desktop notifications

**Delivery details** shows your current delivery policy, enrolled Web Push device
count, and up to 100 recorded outcomes. Device identifiers are opaque fingerprints;
provider response bodies, push endpoints, and subscription keys are not retained
in the history. Readers see only their own history and must still have access to
the card. History begins with this feature; older outcomes cannot be reconstructed.

Web Push retries transient transport errors, HTTP 429, and HTTP 5xx at most three
times within the delivery deadline, waiting one then two seconds. Permanent
errors are not retried; HTTP 404/410 removes the expired subscription. Subscription
ownership, reader access, and preferences are checked again before retries.
`provider_accepted` means provider acceptance, not proof of device receipt.
Desktop acknowledgements are client reports, not proof a banner was displayed.

Desktop alerts have a dedicated cursor-based database feed. They do not depend
on the selected channel, search, read filter, saved view, or the app node serving
an event stream. The client polls every 15 seconds, with immediate event-stream
wakeups when available. First connection establishes a baseline without replaying
old notifications. Failed native calls leave the cursor retryable and surface
an error. Closing the window to the tray retains delivery; quitting the app does
not. **Schedules → Test desktop notification** checks the native call. OS Do Not
Disturb settings can still suppress banners.

## Missing producer notifications

An administrator can create a monitor for an exact native `source` or compatible
webhook `username` in a channel. Choose an interval (one minute to 31 days), or a
daily local deadline, plus a grace period. Monitoring starts when the monitor is
created; it does not infer historical completion from old cards.

Publication refreshes `last_seen` in the ingestion transaction. A missed deadline
creates a warning card. The next publication resolves that same card. Subsequent
failures reopen it and clear old reader dismissal/read state. Simultaneous worker
checks use a database compare-and-swap and transaction to avoid duplicate alerts.
A source name is an explicit matching rule, not a separate authentication boundary:
any publisher already authorized for the channel can publish under that name.

A monitor checks successful publication, not whether the underlying job actually
succeeded. Producers should publish their own failure status. Tintwire's own
availability still needs an independent external watchdog.

## Integration playground

Administrators can paste native cards or Mattermost/Slack JSON, select a template,
and preview validation and rendering. Templates cover deployments, backup
results, a review request, and a camera image. The review template links to a
review page; actionable approvals still require the existing action integration.

Preview does not persist or publish anything and action buttons are disabled.
**Send to test channel** creates a real notification in the explicitly selected
channel and uses normal alert delivery. Payload channel overrides are ignored in
the playground. Testing lifecycle updates or live actions should use the normal
publishing API with a dedicated test-channel credential.

## HTTP API

Reader routes use the normal session cookie. Mutations require a matching Origin.

| Method and path | Purpose |
| --- | --- |
| `GET /api/v1/attention` | Preferences, schedules, recent digest history |
| `PUT /api/v1/attention` | `timezone`, `quiet_start`, `quiet_end`, `critical_bypass` |
| `PUT /api/v1/notifications/{id}/snooze` | RFC3339 `until`; `{}` cancels |
| `PUT /api/v1/saved-views/{id}/digest` | Local `time` (`HH:MM`) and `timezone`; empty time stops |
| `GET /api/v1/incidents` | Visible incident groups |
| `GET /api/v1/incidents?channel_id=…&key=…` | Group cards |
| `POST /api/v1/incidents/acknowledge` | `channel_id` and `key` |
| `GET /api/v1/notifications/{id}/delivery` | Reader's delivery history and current policy |
| `POST /api/v1/notifications/{id}/desktop-delivery` | Client-reported `desktop_accepted` or `desktop_failed` outcome |
| `GET /api/v1/desktop/alerts?after=…` | Native alert feed; omit cursor to establish baseline |
| `GET`, `POST /api/v1/monitors` | List/create producer monitors (admin) |
| `DELETE /api/v1/monitors/{id}` | Stop a monitor (admin); existing cards remain |
| `POST /api/v1/playground` | `format`, `payload`, optional `publish` and `channel_id` (admin) |

## Storage and rollout

These features add schema 30 tables, automatically initialized on SQLite and
PostgreSQL startup. Back up the database before upgrading. SQLite-to-PostgreSQL
migration includes the new tables; open older SQLite databases with the current
binary before migrating. The changes are additive but older binaries reject a
newer schema version: reverting the executable is not a tested rollback procedure.

Workflow writes and workers require standalone SQLite or shared PostgreSQL.
Legacy control-Raft deployments do not replicate this workflow state and reject
workflow mutations. Histories follow the repository's existing indefinite
retention policy; no automatic hard deletion is introduced.
