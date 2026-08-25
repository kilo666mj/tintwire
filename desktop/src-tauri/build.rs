fn main() {
    // App commands must be declared for the access-control list, otherwise they
    // are callable only from the bundled local page and never from the
    // configured remote origin.
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "configured_origin",
            "configure",
            "set_unread",
            "alert",
            "begin_oidc_login",
            "open_external",
        ]),
    ))
    .expect("failed to run tauri-build");
}
