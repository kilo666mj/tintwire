# Client validation checklist

These checks require real browsers, devices, or signing identities and are
release gates rather than unit tests. Record the device, OS/browser version,
date, tester, and result outside the repository when results include private
deployment details. Never record push endpoints, credentials, or screenshots
containing real notifications.

## Web Push matrix

Run against an HTTPS test deployment using a synthetic channel and card.

| Client | Install/enroll | Background alert | Tap deep link | Update replacement | Badge | Revoke |
|---|---|---|---|---|---|---|
| iPhone/iPad Home Screen app | | | | | | |
| Android Chromium installed PWA | | | | | | |
| Desktop Chromium PWA | | | | | | |
| Desktop Firefox | | | | | | |

For each client:

1. Enroll, close the app completely, and publish a critical firing card.
2. Confirm the lock-screen preview contains the subject and a bounded,
   notification-safe body excerpt.
3. Tap it and confirm the correct channel and notification open.
4. Update the same external notification and record whether the platform
   replaces or adds to the original alert.
5. Set the channel to `critical only`; confirm an informational card is silent
   and a critical card arrives.
6. Set it to `muted`; confirm no card arrives on that user's devices.
7. Revoke the subscription and confirm subsequent delivery is absent.
8. Remove private-channel membership and confirm delivery stops.

## Desktop distribution matrix

Before distributing desktop binaries, verify these behaviors on every supported
operating system:

- Clean installation and first-run server configuration.
- Upgrade and downgrade/rollback.
- Notification and message deep links.
- Launch at login and single-instance behavior.
- Native alerts, unread tray badge, and Focus/Do Not Disturb behavior.
- Session expiry and reauthentication.
- Uninstall without leaving credentials or background processes.

For Linux, test the packaged GUI on at least one current Fedora-family system
and one current Debian/Ubuntu-family system. Artifact extraction and checksum
tests are useful, but do not replace desktop-integration testing.
