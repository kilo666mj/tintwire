# Desktop release policy

Tintwire Desktop uses semantic versions. Release tags should be named
`desktop-vMAJOR.MINOR.PATCH`, and the tag version must match both
`desktop/src-tauri/Cargo.toml` and `desktop/src-tauri/tauri.conf.json`.

## Current distribution

The supported path is a local source build with `desktop/install.sh`. The
project does not currently publish trusted prebuilt binaries.

Before publishing binaries, the project must establish a dedicated release
identity that is not a maintainer's personal key. Its public key or certificate
chain, full fingerprint, rotation policy, and revocation process should be
documented here. Private signing material must never be committed or placed in
CI logs or artifacts.

## Required release controls

A binary release workflow should:

1. Build only from a protected version tag.
2. Verify that the tag and both desktop version files agree.
3. Run the Go, browser, Rust, and packaging test suites.
4. Produce a checksum manifest for every artifact.
5. Sign that manifest with the project release identity.
6. Verify the signature and checksums before publishing.
7. Publish artifacts to a durable HTTPS release page.
8. Download the published files and repeat verification from a clean machine.

Linux AppImages should be tested on current Debian/Ubuntu- and Fedora-family
systems. macOS artifacts must be signed and notarized with an Apple Developer
identity before distribution.

## Updates and rollback

Until a signed update manifest with anti-rollback rules exists, updates should
be explicit and user-approved. Keep the previous verified binary until the new
version passes the client validation checklist. Do not add a background updater
that introduces a second, weaker trust path.

See [`CLIENT_VALIDATION.md`](CLIENT_VALIDATION.md) for the reusable validation
matrix.
