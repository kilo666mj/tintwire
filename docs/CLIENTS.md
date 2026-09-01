# Client behavior

Inbox navigation, desktop behavior, and mobile notification delivery.

## Reading a large inbox

The inbox has keyboard control in every client. `j` and `k` move the selection
between cards, `Enter` expands or collapses the selected card, and `a`, `r`, `u`,
and `e` acknowledge, resolve, toggle read state, and archive it. `m` marks
everything read, `/` jumps to search, `c` toggles compact view, and `?` lists the
shortcuts. Keys are ignored while typing in a field, and the selection follows a
notification rather than a position, so a background refresh does not move it.

Compact view is a stored preference rather than a viewport rule: it tightens card
padding and type, and on displays wider than 1500 pixels arranges the feed in two
columns. Roomy view stays available on the same display.

Named views combine up to 20 channels into one chronological notification feed.
They are stored per user, retain the current search/lifecycle/severity filters,
open with both read and unread items by default, and are represented in the URL
for bookmarking and browser navigation. Explicitly selected channels remain in a
view even when their alert delivery is muted; views never change alert settings.

## Desktop client

A Tauri desktop client lives in [`desktop/`](../desktop/README.md). It loads the
same web client in a native window and adds what a browser tab cannot: a
tray-resident background process, a visibly badged tray unread count, native
notifications for cards and other users' channel messages that do not depend on
browser-vendor Web Push, `tintwire://notification/{id}` and
`tintwire://message/{id}` deep links, launch at login, and persisted window
state. The CSS-only background and opaque scrolling surfaces avoid live
backdrop blur to reduce WebKitGTK compositing work.

```sh
cd desktop && ./install.sh
```

That builds the client and installs it for the current user, replacing and
relaunching any running copy.

On first run the client asks for the address of your Tintwire server and stores
it; authentication is the ordinary reader session, established in the window.
The shell holds no credentials. The configured origin is granted exactly three
commands: update the tray count, raise a notification, and open Pocket ID login
in the system browser. Setup-only commands for reading and changing the origin
are not granted to remote content.

Serving Tintwire to the desktop client requires no server configuration. The
`Content-Security-Policy` already names Tauri's IPC transport in `connect-src`,
which browsers never resolve.

Source builds are supported today. Requirements for publishing signed Linux or
macOS binaries are documented in
[`docs/DESKTOP_RELEASES.md`](DESKTOP_RELEASES.md).

## Mobile alerts

Tintwire includes an installable PWA and background Web Push delivery for mobile
and desktop devices. Enable it by giving the server a public VAPID contact
address:

```sh
TINTWIRE_VAPID_CONTACT=mailto:admin@example.com \
  go run ./cmd/tintwire -hook-id local-development-hook
```

Open Tintwire and select **Mobile alerts**. Tintwire shows the correct enrollment
steps for the current device and lets supported browsers install the app directly.
The browser creates one subscription for that installation and stores it in
Tintwire's configured database. VAPID keys are
generated once and retained in the same database, so the database must be backed
up and preserved across upgrades. Firing alerts request high-urgency delivery;
resolved alerts use the same notification tag and replace their firing alert on
platforms that support replacement. Permanently expired subscriptions are
removed automatically. If a browser later discards its subscription while
notification permission remains granted, Tintwire creates and saves a replacement
the next time the app starts. If the browser requires another user gesture,
Tintwire leaves the enable control available for a manual retry.

Except for browser-defined localhost exemptions, service workers and Web Push
require HTTPS. On iPhone and iPad, the in-app setup explains how to add Tintwire
to the Home Screen; launch that installed app, open **Mobile alerts**, and enable
delivery. On supported Chromium browsers the setup sheet can invoke the native
PWA installation prompt. The VAPID contact must be a
real email address, `mailto:` address, or public HTTPS URL; placeholder/internal
domains may be rejected by browser push services.

Subscription writes are restricted to the same origin and, when reader
authentication is enabled, require a valid reader session. Delivery always
applies channel visibility and membership checks. Each reader can additionally
choose all alerts, critical native-card alerts only, or muted delivery for each
visible channel from the Mobile alerts dialog; the preference applies to all of
that reader's subscribed devices.
