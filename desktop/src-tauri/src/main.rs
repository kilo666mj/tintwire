// Tintwire desktop client.
//
// The shell is packaging, not a second client: it loads the configured Tintwire
// origin in a webview and adds only what a browser tab cannot provide — a
// tray-resident background process, native notifications, deep links, launch at
// login, and persisted window state. It has no privileged API surface of its
// own and reuses the reader session held by the webview.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use serde::{Deserialize, Serialize};
use std::process::Command;
use tauri::{
    image::Image,
    menu::{Menu, MenuItem},
    tray::{TrayIconBuilder, TrayIconEvent},
    AppHandle, Emitter, Manager, RunEvent, WebviewWindow, WindowEvent,
};
use tauri_plugin_deep_link::DeepLinkExt;
use tauri_plugin_notification::NotificationExt;
use tauri_plugin_store::StoreExt;

const STORE_FILE: &str = "tintwire.json";
const ORIGIN_KEY: &str = "origin";
const MAIN_WINDOW: &str = "main";
const TRAY_ICON: &[u8] = include_bytes!("../icons/icon-tray.png");
const TRAY_ICON_UNREAD: &[u8] = include_bytes!("../icons/icon-tray-unread.png");

#[derive(Debug, Serialize, Deserialize)]
struct AlertPayload {
    id: String,
    title: String,
    body: String,
    urgent: bool,
}

/// A configured origin is an HTTP(S) origin with no path, query, fragment, or
/// user information, matching the server's own canonical public URL rule.
fn normalize_origin(origin: &str) -> Result<String, String> {
    let parsed = url::Url::parse(origin.trim()).map_err(|_| "not a valid URL".to_string())?;
    if parsed.scheme() != "http" && parsed.scheme() != "https" {
        return Err("origin must use HTTP or HTTPS".into());
    }
    if parsed.host_str().is_none() || !parsed.username().is_empty() || parsed.password().is_some() {
        return Err("origin must be a plain host without credentials".into());
    }
    if parsed.query().is_some() || parsed.fragment().is_some() || parsed.path() != "/" {
        return Err("origin must not contain a path, query, or fragment".into());
    }
    Ok(parsed.origin().ascii_serialization())
}

fn stored_origin(app: &AppHandle) -> Option<String> {
    let store = app.store(STORE_FILE).ok()?;
    let value = store.get(ORIGIN_KEY)?;
    value.as_str().map(str::to_string)
}

/// Grants the configured origin — and only that origin — permission to call this
/// shell's commands. The capability is added at runtime because the origin is
/// installation-specific and unknown when the client is built.
fn allow_origin(app: &AppHandle, origin: &str) -> Result<(), String> {
    let capability = serde_json::json!({
        "identifier": format!("tintwire-remote-{}", origin.replace([':', '/', '.'], "-")),
        "windows": [MAIN_WINDOW],
        "remote": { "urls": [format!("{origin}/*")] },
        // The web client may raise alerts, update the tray, listen for tray
        // commands, and focus its window. It cannot change the configured
        // server origin or reach any plugin directly.
        "permissions": [
            "allow-set-unread",
            "allow-alert",
            "allow-begin-oidc-login",
            "core:event:default",
            "core:window:allow-set-focus"
        ]
    });
    app.add_capability(capability.to_string())
        .map_err(|error| error.to_string())
}

fn show_main_window(app: &AppHandle) {
    if let Some(window) = app.get_webview_window(MAIN_WINDOW) {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
    }
}

fn navigate(window: &WebviewWindow, target: &str) -> Result<(), String> {
    let parsed = url::Url::parse(target).map_err(|error| error.to_string())?;
    window.navigate(parsed).map_err(|error| error.to_string())
}

/// Routes supported Tintwire resource links to the running instance rather
/// than a browser tab. Deep-link input is untrusted: only explicit resource
/// forms are honored, and the destination is always the configured origin.
fn handle_deep_link(app: &AppHandle, link: &str) {
    let Some(origin) = stored_origin(app) else {
        return;
    };
    let target = match url::Url::parse(link) {
        Ok(parsed) if parsed.scheme() == "tintwire" => {
            let id = parsed
                .path()
                .trim_start_matches('/')
                .rsplit('/')
                .next()
                .unwrap_or("")
                .to_string();
            let host = parsed.host_str().unwrap_or_default().to_string();
            if host == "auth" {
                let code = parsed.query_pairs().find_map(|(key, value)| {
                    (key == "code"
                        && value.len() == 64
                        && value.chars().all(|character| character.is_ascii_hexdigit()))
                    .then(|| value.into_owned())
                });
                match code {
                    Some(code) => format!("{origin}/#desktop-auth={code}"),
                    None => origin.clone(),
                }
            } else if host == "notification" && !id.is_empty() {
                format!("{origin}/?notification={}", urlencoding_minimal(&id))
            } else if host == "message" && !id.is_empty() {
                format!("{origin}/?message={}", urlencoding_minimal(&id))
            } else {
                origin.clone()
            }
        }
        _ => origin.clone(),
    };
    if let Some(window) = app.get_webview_window(MAIN_WINDOW) {
        let _ = navigate(&window, &target);
    }
    show_main_window(app);
}

/// Notification IDs are opaque server-generated identifiers; anything outside
/// that alphabet is dropped rather than escaped into the URL.
fn urlencoding_minimal(value: &str) -> String {
    value
        .chars()
        .filter(|character| {
            character.is_ascii_alphanumeric() || *character == '_' || *character == '-'
        })
        .collect()
}

#[tauri::command]
fn configured_origin(app: AppHandle) -> Option<String> {
    stored_origin(&app)
}

#[tauri::command]
fn begin_oidc_login(app: AppHandle, handoff: String) -> Result<(), String> {
    if handoff.len() != 64
        || !handoff
            .chars()
            .all(|character| character.is_ascii_hexdigit())
    {
        return Err("desktop sign-in handoff is invalid".into());
    }
    let origin =
        stored_origin(&app).ok_or_else(|| "server origin is not configured".to_string())?;
    let target = format!("{origin}/api/v1/auth/oidc/start?desktop={handoff}");
    #[cfg(target_os = "macos")]
    let mut command = Command::new("open");
    #[cfg(target_os = "linux")]
    let mut command = Command::new("xdg-open");
    #[cfg(target_os = "windows")]
    let mut command = {
        let mut command = Command::new("rundll32");
        command.arg("url.dll,FileProtocolHandler");
        command
    };
    command
        .arg(target)
        .spawn()
        .map_err(|error| error.to_string())?;
    Ok(())
}

/// Stores the server origin on first run and points the window at it.
#[tauri::command]
fn configure(app: AppHandle, origin: String) -> Result<String, String> {
    let origin = normalize_origin(&origin)?;
    let store = app.store(STORE_FILE).map_err(|error| error.to_string())?;
    store.set(ORIGIN_KEY, serde_json::Value::String(origin.clone()));
    store.save().map_err(|error| error.to_string())?;
    allow_origin(&app, &origin)?;
    let window = app
        .get_webview_window(MAIN_WINDOW)
        .ok_or_else(|| "main window is unavailable".to_string())?;
    navigate(&window, &origin)?;
    Ok(origin)
}

/// Called by the web client when its unread total changes, so the tray reflects
/// the inbox without the shell needing its own credential or API access.
#[tauri::command]
fn set_unread(app: AppHandle, count: u32) -> Result<(), String> {
    if let Some(tray) = app.tray_by_id("tintwire") {
        let tooltip = if count > 0 {
            format!("Tintwire — {count} unread")
        } else {
            "Tintwire".to_string()
        };
        tray.set_tooltip(Some(&tooltip))
            .map_err(|error| error.to_string())?;
        let icon = if count > 0 {
            TRAY_ICON_UNREAD
        } else {
            TRAY_ICON
        };
        tray.set_icon(Some(
            Image::from_bytes(icon).map_err(|error| error.to_string())?,
        ))
        .map_err(|error| error.to_string())?;
    }
    if let Some(window) = app.get_webview_window(MAIN_WINDOW) {
        let badge = if count > 0 { Some(count as i64) } else { None };
        let _ = window.set_badge_count(badge);
    }
    Ok(())
}

/// Raises a native notification for a stream event. The desktop client keeps its
/// window process alive, so alerts do not depend on browser-vendor Web Push.
#[tauri::command]
fn alert(app: AppHandle, payload: AlertPayload) -> Result<(), String> {
    let mut builder = app
        .notification()
        .builder()
        .title(payload.title)
        .body(payload.body)
        // Grouping is per notification so a platform that supports coalescing
        // keeps one thread per card.
        .group(format!("tintwire-{}", payload.id));
    // Resolutions and ordinary arrivals land quietly; only firing alerts make
    // noise. The plugin exposes no desktop API for replacing a visible alert,
    // so a resolution appears alongside its firing alert rather than over it.
    if !payload.urgent {
        builder = builder.silent();
    }
    builder.show().map_err(|error| error.to_string())
}

fn build_tray(app: &AppHandle, icon: Image<'_>) -> tauri::Result<()> {
    let open = MenuItem::with_id(app, "open", "Open Tintwire", true, None::<&str>)?;
    let mark_read = MenuItem::with_id(app, "mark-read", "Mark all read", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&open, &mark_read, &quit])?;
    TrayIconBuilder::with_id("tintwire")
        .icon(icon)
        .tooltip("Tintwire")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id.as_ref() {
            "open" => show_main_window(app),
            "mark-read" => {
                // The shell asks the web client to perform the operation so that
                // authorization stays server-side and attributable.
                let _ = app.emit("tintwire://mark-all-read", ());
                show_main_window(app);
            }
            "quit" => app.exit(0),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click { .. } = event {
                show_main_window(tray.app_handle());
            }
        })
        .build(app)?;
    Ok(())
}

fn main() {
    // Fedora/KWin can currently terminate GTK clients with an explicit-sync
    // Wayland protocol error (wp_linux_drm_syncobj_surface_v1). Prefer the
    // stable XWayland path unless the caller explicitly selects a backend.
    // Setting this here rather than only in the desktop entry keeps the client
    // working when it is launched from a shell, a deep link, or autostart.
    #[cfg(target_os = "linux")]
    if std::env::var_os("GDK_BACKEND").is_none() {
        std::env::set_var("GDK_BACKEND", "x11");
    }
    #[cfg(target_os = "linux")]
    if std::env::var_os("WEBKIT_DISABLE_DMABUF_RENDERER").is_none() {
        std::env::set_var("WEBKIT_DISABLE_DMABUF_RENDERER", "1");
    }

    let window_state = tauri_plugin_window_state::Builder::default();
    // Restoring a maximized physical position/geometry can leave WKWebView's
    // hit-testing offset from the rendered page on Retina displays. Retain the
    // user's normal window size on macOS, but let AppKit place the window and
    // handle maximization afresh on each launch.
    #[cfg(target_os = "macos")]
    let window_state = window_state.with_state_flags(tauri_plugin_window_state::StateFlags::SIZE);

    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, argv, _cwd| {
            show_main_window(app);
            if let Some(link) = argv
                .iter()
                .find(|argument| argument.starts_with("tintwire://"))
            {
                handle_deep_link(app, link);
            }
        }))
        .plugin(tauri_plugin_store::Builder::default().build())
        .plugin(window_state.build())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_deep_link::init())
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .invoke_handler(tauri::generate_handler![
            configured_origin,
            configure,
            set_unread,
            alert,
            begin_oidc_login
        ])
        .setup(|app| {
            let handle = app.handle().clone();
            // Set the icon explicitly as well as through the Tauri bundle
            // metadata. Direct executable launches do not give Linux desktops
            // a .desktop entry from which to resolve a taskbar/window icon.
            let icon = handle
                .default_window_icon()
                .expect("Tintwire bundle icon is configured")
                .clone();
            if let Some(window) = handle.get_webview_window(MAIN_WINDOW) {
                window.set_icon(icon.clone())?;
            }
            build_tray(&handle, Image::from_bytes(TRAY_ICON)?)?;
            // With no stored origin the window keeps the bundled first-run
            // setup page it was created with.
            if let Some(origin) = stored_origin(&handle) {
                match allow_origin(&handle, &origin) {
                    Ok(()) => {
                        if let Some(window) = handle.get_webview_window(MAIN_WINDOW) {
                            let _ = navigate(&window, &origin);
                        }
                    }
                    // Without the capability the web client cannot reach the
                    // tray or native alerts, so this must not fail silently.
                    Err(error) => eprintln!("tintwire: cannot trust {origin}: {error}"),
                }
            }
            if let Ok(Some(urls)) = app.deep_link().get_current() {
                if let Some(url) = urls.first() {
                    handle_deep_link(&handle, url.as_str());
                }
            }

            let deep_link_handle = handle.clone();
            app.deep_link().on_open_url(move |event| {
                if let Some(url) = event.urls().first() {
                    handle_deep_link(&deep_link_handle, url.as_str());
                }
            });

            Ok(())
        })
        .on_window_event(|window, event| {
            // Closing the window hides it: the client stays resident so the
            // event stream, tray count, and alerts keep working.
            if let WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .build(tauri::generate_context!())
        .expect("build Tintwire desktop client")
        .run(|_app, event| {
            if let RunEvent::ExitRequested { api, code, .. } = event {
                if code.is_none() {
                    api.prevent_exit();
                }
            }
        });
}
