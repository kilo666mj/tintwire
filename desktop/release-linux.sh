#!/usr/bin/env bash
# Build a reproducible-named Linux AppImage and its checksum manifest.
# When RELEASE_GPG_KEY is set, the AppImage and manifest are signed by that key.
set -euo pipefail

cd "$(dirname "$0")"

version="$(sed -n 's/^version = "\([^"]*\)"/\1/p' src-tauri/Cargo.toml | head -n1)"
config_version="$(sed -n 's/^[[:space:]]*"version": "\([^"]*\)",/\1/p' src-tauri/tauri.conf.json | head -n1)"
if [ -z "$version" ] || [ "$version" != "$config_version" ]; then
    echo "error: Cargo.toml and tauri.conf.json versions must match" >&2
    exit 1
fi
if [ -n "${RELEASE_VERSION:-}" ] && [ "$RELEASE_VERSION" != "$version" ]; then
    echo "error: release version $RELEASE_VERSION does not match desktop version $version" >&2
    exit 1
fi
command -v cargo-tauri >/dev/null 2>&1 || {
    echo "error: cargo-tauri is required (cargo install tauri-cli --version '^2' --locked)" >&2
    exit 1
}

# The release profile already strips Tintwire itself. Prevent linuxdeploy from
# rewriting every bundled system library; that fails on NFSv4 ACL xattrs and is
# unnecessary for this webview shell.
export NO_STRIP=true

if [ -n "${RELEASE_GPG_KEY:-}" ]; then
    command -v gpg >/dev/null 2>&1 || { echo "error: gpg is required for signed releases" >&2; exit 1; }
    export SIGN=1
    export SIGN_KEY="$RELEASE_GPG_KEY"
    export APPIMAGETOOL_FORCE_SIGN=1
fi

cargo tauri build --bundles appimage

release_dir="dist/linux-x86_64"
mkdir -p "$release_dir"
find "$release_dir" -maxdepth 1 -type f -delete

appimage_path="$(find src-tauri/target/release/bundle/appimage -maxdepth 1 -type f -name '*.AppImage' -print -quit)"
if [ -z "$appimage_path" ]; then
    echo "error: Tauri did not produce an AppImage bundle" >&2
    exit 1
fi

install -m 0755 "$appimage_path" "$release_dir/Tintwire-${version}-x86_64.AppImage"
(
    cd "$release_dir"
    sha256sum ./*.AppImage >SHA256SUMS
    if [ -n "${RELEASE_GPG_KEY:-}" ]; then
        gpg --batch --yes --local-user "$RELEASE_GPG_KEY" --armor --detach-sign SHA256SUMS
    fi
)

echo "Linux release artifacts are in desktop/$release_dir"
