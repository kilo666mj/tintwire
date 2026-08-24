// First-run configuration. The shell stores one thing: which server to open.
// Credentials are never handled here; the reader session is established by the
// Tintwire web client itself once the window navigates to the origin.
const { invoke } = window.__TAURI__.core;
const autostart = window.__TAURI__.autostart;

const form = document.querySelector("#setup-form");
const originField = document.querySelector("#origin");
const autostartField = document.querySelector("#autostart");
const status = document.querySelector("#status");

autostart
  ?.isEnabled()
  .then(enabled => {
    autostartField.checked = enabled;
  })
  .catch(() => {});

invoke("configured_origin")
  .then(origin => {
    if (origin) originField.value = origin;
  })
  .catch(() => {});

form.addEventListener("submit", async event => {
  event.preventDefault();
  status.textContent = "";
  const origin = originField.value.trim().replace(/\/+$/, "");
  if (!origin) return;
  try {
    if (autostart) {
      if (autostartField.checked) await autostart.enable();
      else await autostart.disable();
    }
  } catch {
    // Launch at login is a convenience; failing to set it must not block setup.
  }
  try {
    await invoke("configure", { origin });
  } catch (error) {
    status.textContent = `${error}`.replace(/^Error:\s*/, "");
  }
});
