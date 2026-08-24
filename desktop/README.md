# Tintwire desktop client

A [Tauri 2](https://tauri.app) shell around the same web client the browser
loads. It exists for the four things a browser tab cannot do: stay resident in
the background, own a tray icon, raise notifications without a browser's push
permission, and restore window state between sessions.

The shell is packaging, not a second client. It holds no credentials, has no API
of its own, and never talks to the Tintwire API directly — the webview carries
the ordinary reader session, and every authorization decision stays on the
server.

## What it adds

- **Tray icon** with a visible unread badge and unread count in its tooltip, plus
  open, mark-all-read, and quit. Closing the window hides it; the client keeps
  running.
- **Native notifications** raised from the existing event stream. Firing alerts
  make noise, everything else arrives silently. Because the window process stays
  alive, desktop alerts do not depend on browser-vendor Web Push at all, and the
  web client hides its Web Push enrollment when running inside the shell.
- **Deep links**: `tintwire://notification/{id}` and `tintwire://message/{id}` open the running instance and
  scrolls to that card instead of launching a browser tab.
- **Launch at login**, single-instance enforcement, and persisted window
  geometry.

## Installing

`./install.sh` builds a release binary and installs it, replacing any existing
copy. It stops a running client first and relaunches it afterwards, so an
upgrade is one command.

```sh
cd desktop && ./install.sh
```

On Linux it installs the executable to `~/.local/bin`, an icon and a desktop
entry to `~/.local/share`, and refreshes the desktop and icon caches. The
desktop entry claims `x-scheme-handler/tintwire`, which is what makes
`tintwire://notification/{id}` and `tintwire://message/{id}` links open the client. Missing GTK/WebKit build
dependencies are installed first on Fedora and Debian derivatives, which needs
`sudo`; nothing else in the install requires root.

On macOS it builds the `.app` bundle and replaces `/Applications/Tintwire.app`.
That path needs the Tauri CLI (`cargo install tauri-cli --version '^2'`); the
Linux path does not, because a bare executable is installable as-is.

## Building

Requires a Rust toolchain and the platform's usual Tauri prerequisites (Xcode
command line tools on macOS; `libwebkit2gtk-4.1-dev` and friends on Linux).

```sh
cd desktop/src-tauri
cargo build            # development binary
cargo build --release  # optimized binary
```

Bundling distributable installers uses the Tauri CLI (`cargo tauri build`).
The Linux release bundle is an AppImage. Build it with:

```sh
cd desktop
./release-linux.sh
```

The script validates that the Cargo and Tauri versions match, writes stable
artifact names under `desktop/dist/linux-x86_64`, and produces `SHA256SUMS`.
Set `RELEASE_GPG_KEY` to a local GPG signing-key fingerprint to embed an
AppImage signature and create the detached `SHA256SUMS.asc` signature. The
private signing key is deliberately external to the repository and must be
backed up offline.

Local developer builds are unsigned. Public binary releases are not yet
configured; see [`../docs/DESKTOP_RELEASES.md`](../docs/DESKTOP_RELEASES.md)
for the controls required before publishing them.

## First run

The client asks for the address of your Tintwire server, stores it, and points
the window at it. Sign in there as you would in a browser. The stored origin
lives in the platform application-configuration directory, for example
`~/Library/Application Support/com.tintwire.desktop/tintwire.json` on macOS;
delete that file to reconnect to a different server.

## How the shell and web client talk

The configured origin — and only that origin — is granted a runtime capability
allowing exactly three commands: `set_unread`, which drives the tray count and
window badge; `alert`, which raises a native notification; and
`begin_oidc_login`, which opens Pocket ID login in the system browser. The
setup-only `configure` and `configured_origin` commands are registered by the
shell but are not granted to remote content. The remote page cannot change the
configured server or reach any plugin directly. The capability is added at
runtime because the origin is installation-specific and unknown when the client
is built.

The server's `Content-Security-Policy` names Tauri's IPC transport in
`connect-src` (`ipc:` on macOS and Linux, `http://ipc.localhost` on Windows).
Browsers never resolve either, so the browser surface is unchanged; without it
the desktop bridge is silently blocked.

## Rendering performance

The shared desktop UI uses a CSS-only fixed decorative background. Feed cards,
console panels, and the sticky sidebar use opaque or near-opaque graphite
surfaces without backdrop blur, which avoids repainting blurred background
pixels during WebKitGTK scrolling. Modal and login blur remains because it is
outside normal feed scrolling. Mobile viewports continue to omit the decorative
background entirely.

## Known gaps

- The notification plugin exposes no desktop API for replacing a visible alert,
  so a resolution appears alongside its firing alert rather than over it.
- Public binary signing, macOS notarization, and Windows signing are not
  configured.
- Only the tray count reflects unread state while the window is hidden; there is
  no separate background poller, so alerts stop if the webview loses its session
  and nobody signs in again.
