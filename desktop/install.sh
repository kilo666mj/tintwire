#!/usr/bin/env bash
# Build and install the Tintwire desktop client on macOS or Linux, replacing
# any existing copy.
set -euo pipefail

cd "$(dirname "$0")"

if [ -d "${HOME}/.cargo/bin" ]; then
    PATH="${HOME}/.cargo/bin:${PATH}"
    export PATH
fi

APP_NAME="Tintwire"
APP_ID="com.tintwire.desktop"
PROC_NAME="tintwire-desktop" # Tauri binary name, from src-tauri/Cargo.toml [package] name
LINUX_ICON_NAME="$APP_ID"
URL_SCHEME="tintwire"

ensure_build_tools() {
    command -v cargo >/dev/null 2>&1 || {
        echo "error: Rust and Cargo are required to build ${APP_NAME}" >&2
        exit 1
    }
}

run_as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    elif command -v sudo >/dev/null 2>&1; then
        sudo "$@"
    else
        echo "error: sudo is required to install Linux build dependencies" >&2
        exit 1
    fi
}

ensure_linux_dependencies() {
    if command -v pkg-config >/dev/null 2>&1 &&
       pkg-config --exists glib-2.0 webkit2gtk-4.1; then
        return
    fi

    # Tauri links against the system GTK/WebKit stack. Install its documented
    # prerequisites before Cargo starts, rather than failing late in a build.
    if command -v dnf >/dev/null 2>&1; then
        echo "Installing Fedora Tauri build dependencies..."
        run_as_root dnf install -y \
            webkit2gtk4.1-devel \
            openssl-devel \
            curl \
            wget \
            file \
            libappindicator-gtk3-devel \
            librsvg2-devel \
            libxdo-devel \
            gcc \
            gcc-c++ \
            make \
            pkgconf-pkg-config
    elif command -v apt-get >/dev/null 2>&1; then
        echo "Installing Debian/Ubuntu Tauri build dependencies..."
        run_as_root apt-get update
        run_as_root apt-get install -y \
            libwebkit2gtk-4.1-dev \
            build-essential \
            curl \
            wget \
            file \
            libxdo-dev \
            libssl-dev \
            libayatana-appindicator3-dev \
            librsvg2-dev \
            patchelf \
            pkg-config
    else
        echo "error: missing the GTK/WebKit development libraries required by Tauri" >&2
        echo "See https://v2.tauri.app/start/prerequisites/#linux" >&2
        exit 1
    fi

    command -v pkg-config >/dev/null 2>&1 &&
        pkg-config --exists glib-2.0 webkit2gtk-4.1 || {
            echo "error: GTK/WebKit development libraries are still unavailable after installation" >&2
            exit 1
        }
}

is_running() {
    pgrep -x "$PROC_NAME" >/dev/null 2>&1
}

# Linux exposes the process comm field used by `pgrep -x` with a 15-character
# limit. The Tintwire binary name is 16 characters, so match the complete
# executable command line instead or an old process survives reinstalls.
is_running_linux() {
    local executable="$1"
    pgrep -f "^${executable}([[:space:]]|$)" >/dev/null 2>&1
}

wait_for_linux_exit() {
    local executable="$1"
    for _ in $(seq 1 20); do
        is_running_linux "$executable" || return 0
        sleep 0.5
    done
    echo "error: ${APP_NAME} did not quit" >&2
    return 1
}

wait_for_exit() {
    for _ in $(seq 1 20); do
        is_running || return 0
        sleep 0.5
    done
    echo "error: ${APP_NAME} did not quit" >&2
    return 1
}

install_macos() {
    local build_app="src-tauri/target/release/bundle/macos/${APP_NAME}.app"
    local dest_app="/Applications/${APP_NAME}.app"
    local relaunch=false

    # A macOS install needs a real .app bundle, which only the Tauri CLI
    # produces. Linux avoids this because a bare executable is installable.
    command -v cargo-tauri >/dev/null 2>&1 || {
        echo "error: the Tauri CLI is required to bundle ${APP_NAME} for macOS" >&2
        echo "Install it with: cargo install tauri-cli --version '^2'" >&2
        exit 1
    }

    # Build only the .app bundle (skip the DMG for speed).
    (cd src-tauri && cargo tauri build --bundles app)
    [ -d "$build_app" ] || { echo "error: build output not found at $build_app" >&2; exit 1; }

    if is_running; then
        relaunch=true
        echo "Quitting running ${APP_NAME}..."
        osascript -e "quit app \"${APP_NAME}\""
        wait_for_exit
    fi

    # Replace the whole bundle so stale files from the old version cannot linger.
    rm -rf "$dest_app"
    ditto "$build_app" "$dest_app"
    echo "Installed $(defaults read "$dest_app/Contents/Info" CFBundleShortVersionString 2>/dev/null || echo "$APP_NAME") to $dest_app"

    if $relaunch; then
        echo "Relaunching ${APP_NAME}..."
        open "$dest_app"
    fi
}

install_linux() {
    local build_bin="src-tauri/target/release/${PROC_NAME}"
    local bin_dir="${XDG_BIN_HOME:-${HOME}/.local/bin}"
    local data_dir="${XDG_DATA_HOME:-${HOME}/.local/share}"
    local dest_bin="${bin_dir}/${PROC_NAME}"
    local applications_dir="${data_dir}/applications"
    local icon_dir="${data_dir}/icons/hicolor/512x512/apps"
    # Plasma matches a native Wayland window's application ID to the desktop
    # filename. Tauri derives that ID from tauri.conf.json's identifier.
    local desktop_file="${applications_dir}/${APP_ID}.desktop"
    local legacy_desktop_file="${applications_dir}/${PROC_NAME}.desktop"
    local icon_file="${icon_dir}/${LINUX_ICON_NAME}.png"
    local legacy_icon_file="${icon_dir}/${PROC_NAME}.png"
    local relaunch=false

    ensure_linux_dependencies

    # There is no frontend bundler to run: the shell loads the server's own web
    # client, and `frontendDist` is only the small local first-run page. A bare
    # Tauri executable is enough for a user-local install and needs neither root
    # nor the Tauri CLI.
    (cd src-tauri && cargo build --release)
    [ -x "$build_bin" ] || { echo "error: build output not found at $build_bin" >&2; exit 1; }

    if is_running_linux "$dest_bin"; then
        relaunch=true
        echo "Stopping running ${APP_NAME}..."
        pkill -TERM -f "^${dest_bin}([[:space:]]|$)"
        wait_for_linux_exit "$dest_bin"
    fi

    install -Dm755 "$build_bin" "$dest_bin"
    install -Dm644 "src-tauri/icons/icon-tray.png" "$icon_file"
    mkdir -p "$applications_dir"
    rm -f "$legacy_desktop_file"
    rm -f "$legacy_icon_file"
    # MimeType claims the tintwire:// scheme. Without an installed desktop entry
    # claiming it, deep links have nothing to open on Linux.
    printf '%s\n' \
        '[Desktop Entry]' \
        'Type=Application' \
        "Name=${APP_NAME}" \
        'Comment=Rich notification inbox' \
        "Exec=/usr/bin/env GDK_BACKEND=x11 WEBKIT_DISABLE_DMABUF_RENDERER=1 \"${dest_bin}\" %u" \
        "Icon=${LINUX_ICON_NAME}" \
        "StartupWMClass=${APP_ID}" \
        "MimeType=x-scheme-handler/${URL_SCHEME};" \
        'Terminal=false' \
        'Categories=Network;Utility;' \
        'StartupNotify=true' \
        >"$desktop_file"
    chmod 644 "$desktop_file"

    if command -v update-desktop-database >/dev/null 2>&1; then
        update-desktop-database "$applications_dir" >/dev/null 2>&1 || true
    fi
    if command -v gtk-update-icon-cache >/dev/null 2>&1; then
        gtk-update-icon-cache -f -t "${data_dir}/icons/hicolor" >/dev/null 2>&1 || true
    fi
    if command -v kbuildsycoca6 >/dev/null 2>&1; then
        kbuildsycoca6 --noincremental >/dev/null 2>&1 || true
    fi
    echo "Installed ${APP_NAME} to $dest_bin"

    case ":${PATH}:" in
        *":${bin_dir}:"*) ;;
        *) echo "note: ${bin_dir} is not on your PATH" ;;
    esac

    if $relaunch; then
        echo "Relaunching ${APP_NAME}..."
        GDK_BACKEND=x11 WEBKIT_DISABLE_DMABUF_RENDERER=1 nohup "$dest_bin" >/dev/null 2>&1 &
    fi
}

ensure_build_tools

case "$(uname -s)" in
    Darwin) install_macos ;;
    Linux) install_linux ;;
    *) echo "error: unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac
