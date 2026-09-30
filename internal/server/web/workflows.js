"use strict";

(function () {
  const dialog = document.querySelector("#workflow-dialog");
  const content = document.querySelector("#workflow-content");
  const heading = document.querySelector("#workflow-title");
  const status = document.querySelector("#workflow-status");
  const localZone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  let pageVersion = 0;

  async function request(path, method = "GET", body) {
    const response = await fetch(path, {method, headers: body === undefined ? {} : {"Content-Type": "application/json"}, body: body === undefined ? undefined : JSON.stringify(body)});
    if (!response.ok) { const error = new Error((await response.text()).trim() || `HTTP ${response.status}`); error.status = response.status; throw error; }
    return response.status === 204 ? null : response.json();
  }
  function button(label, action, className = "card-inbox-button") {
    const node = element("button", className, label);
    node.type = "button";
    node.addEventListener("click", async () => {
      node.disabled = true;
      try { await action(); } catch (error) { status.textContent = error.message; showInboxToast(error.message); }
      finally { node.disabled = false; }
    });
    return node;
  }
  function field(label, type, value = "") {
    const wrapper = element("label", "workflow-field");
    const input = element("input"); input.type = type; input.value = value;
    wrapper.append(element("span", "", label), input);
    return {wrapper, input};
  }
  function select(label, values) {
    const wrapper = element("label", "workflow-field");
    const input = element("select");
    for (const [value, text] of values) { const option = element("option", "", text); option.value = value; input.append(option); }
    wrapper.append(element("span", "", label), input);
    return {wrapper, input};
  }
  function section(title, copy = "") {
    const node = element("section", "workflow-section"); node.append(element("h3", "", title));
    if (copy) node.append(element("p", "automation-copy", copy));
    content.append(node); return node;
  }
  function open(title) {
    pageVersion++;
    const admin = document.querySelector("#automation-open").hidden === false;
    document.querySelector("#workflow-monitors").hidden = !admin;
    document.querySelector("#workflow-playground").hidden = !admin;
    heading.textContent = title; status.textContent = ""; content.replaceChildren();
    if (!dialog.open) dialog.showModal();
    return pageVersion;
  }
  document.querySelector("#workflow-close").addEventListener("click", () => dialog.close());
  dialog.addEventListener("click", event => { if (event.target === dialog) dialog.close(); });

  async function attention() {
    const version = open("Notification schedules");
    const [data, views] = await Promise.all([request("/api/v1/attention"), request("/api/v1/saved-views")]);
    if (version !== pageVersion) return;
    const quiet = section("Quiet hours", "Schedules use your selected timezone. Critical alerts bypass quiet hours only when you enable the exception.");
    const zone = field("Timezone", "text", data.preferences.timezone === "UTC" && !data.preferences.quiet_start ? localZone : data.preferences.timezone);
    const start = field("Quiet from", "time", data.preferences.quiet_start);
    const end = field("Until", "time", data.preferences.quiet_end);
    const critical = field("Allow critical alerts during quiet hours", "checkbox"); critical.input.checked = data.preferences.critical_bypass;
    quiet.append(zone.wrapper, start.wrapper, end.wrapper, critical.wrapper, button("Save quiet hours", async () => {
      await request("/api/v1/attention", "PUT", {timezone: zone.input.value, quiet_start: start.input.value, quiet_end: end.input.value, critical_bypass: critical.input.checked});
      status.textContent = "Quiet hours saved.";
    }), button("Disable quiet hours", async () => {
      await request("/api/v1/attention", "PUT", {timezone: zone.input.value, quiet_start: "", quiet_end: "", critical_bypass: critical.input.checked}); await attention();
    }));
    if (desktopShell) {
      quiet.append(button("Test desktop notification", async () => {
        await desktopShell.invoke("alert", {payload: {id: "desktop-test", title: "Tintwire desktop test", body: "Native notifications are working.", urgent: true}});
        status.textContent = "The operating system accepted the notification. Check your notification center and Do Not Disturb setting if no banner appeared.";
      }));
    }
    const digests = section("Daily digests", "A digest counts notifications matching the saved view that were updated in the past 24 hours. It appears here and sends an alert when the count is nonzero.");
    if (views.views.length) {
      const view = select("Saved view", views.views.map(v => [v.id, v.name]));
      const time = field("Digest time", "time", "08:00");
      const showSchedule = () => { const schedule = data.schedules.find(s => s.kind === "digest" && s.target === view.input.value); time.input.value = schedule ? JSON.parse(schedule.config).time : "08:00"; };
      view.input.addEventListener("change", showSchedule); showSchedule();
      digests.append(view.wrapper, time.wrapper, button("Schedule digest", async () => {
        await request(`/api/v1/saved-views/${encodeURIComponent(view.input.value)}/digest`, "PUT", {time: time.input.value, timezone: zone.input.value}); await attention();
      }), button("Stop digest", async () => {
        await request(`/api/v1/saved-views/${encodeURIComponent(view.input.value)}/digest`, "PUT", {time: "", timezone: zone.input.value}); await attention();
      }));
    } else digests.append(element("p", "", "Save a view first to schedule its digest."));
    for (const schedule of data.schedules.filter(s => s.kind === "digest")) {
      const view = views.views.find(v => v.id === schedule.target);
      digests.append(element("p", "", `${view?.name || "Deleted view"} · next ${new Date(schedule.due_at).toLocaleString()}`));
    }
    for (const entry of data.digests) {
      digests.append(button(`${entry.title}: ${entry.count} notifications · ${new Date(entry.created_at).toLocaleString()}`, async () => {
        const view = views.views.find(v => v.id === entry.view_id); if (view) { dialog.close(); await applySavedView(view); }
      }));
    }
    const reminders = section("Snoozed notifications");
    const snoozes = data.schedules.filter(s => s.kind === "snooze");
    if (!snoozes.length) reminders.append(element("p", "", "No reminders scheduled."));
    for (const snooze of snoozes) {
      const row = element("div", "workflow-row");
      const link = element("a", "", `Reminder · ${new Date(snooze.due_at).toLocaleString()}`); link.href = `/?notification=${encodeURIComponent(snooze.target)}`;
      row.append(link, button("Cancel reminder", async () => { await request(`/api/v1/notifications/${encodeURIComponent(snooze.target)}/snooze`, "PUT", {}); await attention(); })); reminders.append(row);
    }
  }

  async function snooze(notification) {
    open("Remind me later");
    const body = section(notificationLabel(notification), "Snoozing suppresses alerts for this card until your reminder. The card stays available in history.");
    async function save(until) { await request(`/api/v1/notifications/${encodeURIComponent(notification.id)}/snooze`, "PUT", {until: until.toISOString()}); dialog.close(); showInboxToast(`Reminder set for ${until.toLocaleString()}`); }
    for (const [label, minutes] of [["15 minutes", 15], ["1 hour", 60], ["2 hours", 120], ["Tomorrow", 1440]]) body.append(button(label, () => save(new Date(Date.now() + minutes * 60000))));
    const custom = field("Choose a time", "datetime-local");
    body.append(custom.wrapper, button("Set reminder", () => { const until = new Date(custom.input.value); if (!Number.isFinite(until.getTime())) throw new Error("Choose a valid reminder time."); return save(until); }));
  }

  async function delivery(notification) {
    const version = open("Notification delivery");
    const data = await request(`/api/v1/notifications/${encodeURIComponent(notification.id)}/delivery`);
    if (version !== pageVersion) return;
    section(notificationLabel(notification), `Current policy: ${data.current_policy.replaceAll("_", " ")}. Web Push ${data.push_configured ? "configured" : "not configured"}; ${data.device_count} enrolled devices.`);
    const history = section("Delivery history", data.receipt_note);
    if (!data.events.length) history.append(element("p", "", "No recorded attempts for your account. Older deliveries are not available."));
    for (const event of data.events) history.append(element("p", "", `${new Date(event.created_at).toLocaleString()} · ${event.outcome.replaceAll("_", " ")}${event.device ? ` · device ${event.device}` : ""}${event.attempt ? ` · attempt ${event.attempt}` : ""}`));
  }

  async function incidents() {
    const version = open("Incidents"); const data = await request("/api/v1/incidents"); if (version !== pageVersion) return;
    if (!data.incidents.length) section("No grouped incidents", "Publish native cards with incident_key, or compatibility webhooks with props.incident_key. Matching keys group producers within the same channel.");
    for (const incident of data.incidents) {
      const item = element("details", "workflow-section");
      item.append(element("summary", "", `${incident.key} · #${incident.channel} · ${incident.count} cards · ${incident.firing} firing`));
      const cards = element("div", "workflow-cards");
      item.append(button("Acknowledge incident", async () => { await request("/api/v1/incidents/acknowledge", "POST", {channel_id: incident.channel_id, key: incident.key}); await incidents(); await refreshInboxState(); }), cards);
      item.addEventListener("toggle", async () => {
        if (!item.open || cards.childNodes.length) return;
        try {
          const data = await request(`/api/v1/incidents?channel_id=${encodeURIComponent(incident.channel_id)}&key=${encodeURIComponent(incident.key)}`);
          for (const n of data.notifications) {
            const card = element("div", "workflow-incident-card");
            const link = element("a", "", `${notificationLabel(n)} · ${n.state} · ${new Date(n.updated_at).toLocaleString()}`); link.href = `/?notification=${encodeURIComponent(n.id)}`;
            card.append(link, element("p", "", n.text)); cards.append(card);
          }
          if (incident.count > data.notifications.length) cards.append(element("p", "", "Showing the latest 200 cards."));
        } catch (error) { status.textContent = error.message; }
      });
      content.append(item);
    }
  }

  async function monitors() {
    const version = open("Producer monitors");
    const [data, channels] = await Promise.all([request("/api/v1/monitors"), request("/api/v1/channels")]); if (version !== pageVersion) return;
    const form = section("Expect a notification", "A native card source or webhook username refreshes its monitor when published in this channel. Missing notifications create a warning card; the next publication resolves it.");
    const name = field("Monitor name", "text"); const source = field("Exact source / webhook username", "text");
    const channel = select("Channel", channels.channels.map(c => [c.id, c.display_name || c.name]));
    const minutes = field("Expected interval (minutes)", "number", "1440"); minutes.input.min = "1";
    const grace = field("Grace period (minutes)", "number", "30"); grace.input.min = "0";
    const daily = field("Or daily deadline (optional)", "time"); const zone = field("Timezone for daily deadline", "text", localZone);
    form.append(name.wrapper, source.wrapper, channel.wrapper, minutes.wrapper, grace.wrapper, daily.wrapper, zone.wrapper, button("Create monitor", async () => {
      await request("/api/v1/monitors", "POST", {name: name.input.value, source: source.input.value, channel_id: channel.input.value, period_seconds: Number(minutes.input.value) * 60, grace_seconds: Number(grace.input.value) * 60, daily: {time: daily.input.value, timezone: zone.input.value}}); await monitors();
    }));
    for (const monitor of data.monitors) {
      const item = section(monitor.name, `${monitor.source} · ${monitor.state} · last signal ${new Date(monitor.last_seen).toLocaleString()}`);
      item.append(button("Remove monitor", async () => { if (!confirm(`Stop monitoring ${monitor.name}?`)) return; await request(`/api/v1/monitors/${encodeURIComponent(monitor.id)}`, "DELETE"); await monitors(); }));
    }
  }

  const templates = {
    Deployment: {version: 1, title: "Deployment complete", summary: "Example service is now running release v1.2.3", source: "deploy", severity: "success", incident_key: "example-service", fields: [{label: "Version", value: "v1.2.3"}]},
    Backup: {version: 1, title: "Backup complete", summary: "Daily backup verified", source: "backup", severity: "success", metrics: [{label: "Size", value: "2.4 GB"}]},
    Approval: {version: 1, title: "Release approval requested", summary: "Review the change before approving", source: "release", severity: "warning", links: [{label: "Review change", url: "https://example.com/change/123"}]},
    Camera: {version: 1, title: "Motion detected", summary: "Front entrance", source: "camera", severity: "info", images: [{url: "https://example.com/snapshot.jpg", alt: "Example camera snapshot"}]},
  };
  async function playground() {
    const version = open("Integration playground"); const channels = await request("/api/v1/channels"); if (version !== pageVersion) return;
    const form = section("Preview a payload", "Preview validates and renders without publishing. Send to test channel creates a real notification in the channel you choose.");
    const format = select("Format", [["native", "Native card"], ["webhook", "Mattermost / Slack webhook"]]);
    const template = select("Template", Object.keys(templates).map(k => [k, k]));
    const channel = select("Test channel", [["", "Choose a test channel…"], ...channels.channels.map(c => [c.id, c.display_name || c.name])]);
    const label = element("label", "workflow-field"); label.append(element("span", "", "JSON payload")); const payload = element("textarea", "workflow-payload"); payload.rows = 16; payload.spellcheck = false; label.append(payload);
    const setTemplate = () => { payload.value = JSON.stringify(format.input.value === "native" ? templates[template.input.value] : {text: templates[template.input.value].summary, username: templates[template.input.value].source, props: {incident_key: "example-service"}}, null, 2); };
    template.input.addEventListener("change", setTemplate); format.input.addEventListener("change", setTemplate); setTemplate();
    const preview = element("div", "workflow-preview");
    async function validate(publish) {
      if (publish && !channel.input.value) throw new Error("Choose a test channel first.");
      const data = await request("/api/v1/playground", "POST", {format: format.input.value, payload: JSON.parse(payload.value), channel_id: channel.input.value, publish});
      const n = data.notification; preview.replaceChildren();
      if (n.card) preview.append(nativeCard(n.card, {...n, can_operate: false}));
      else { preview.append(richContent("text", n.text)); (n.attachments || []).forEach((a, i) => preview.append(attachmentCard(a, {...n, can_operate: false}, i))); }
      // Preview controls never invoke an action against a real target.
      preview.querySelectorAll("button").forEach(node => { node.disabled = true; });
      status.textContent = publish ? "Published to the selected test channel." : "Valid payload. Nothing published.";
    }
    form.append(format.wrapper, template.wrapper, channel.wrapper, label, button("Preview", () => validate(false)), button("Send to test channel", () => validate(true)), preview);
  }

  async function queue() {
    const version = open("Agent command queue"); const channels = await request("/api/v1/channels"); if (version !== pageVersion) return;
    const form = section("Conversation channel", "Top-level human messages from channel operators become commands after an administrator binds an agent. Replies and generated messages are never treated as instructions.");
    const channel = select("Channel", channels.channels.map(c => [c.id, c.display_name || c.name]));
    const selected = channelCache.find(c => c.name === selectedChannel); if (selected) channel.input.value = selected.id;
    const list = element("div", "workflow-queue");
    async function refresh() {
      const data = await request(`/api/v1/channels/${encodeURIComponent(channel.input.value)}/agent-commands`); list.replaceChildren();
      if (!data.commands.length) list.append(element("p", "", "No queued commands in this channel."));
      for (const command of data.commands) {
        const item = element("div", "workflow-section"); item.append(element("strong", "", `${command.author} · ${command.state}`), element("p", "", command.text));
        if (command.result) item.append(element("p", "", command.result));
        if (["queued", "running", "interrupted"].includes(command.state)) item.append(button("Cancel", async () => { await request(`/api/v1/agent-commands/${encodeURIComponent(command.id)}/control`, "POST", {action: "cancel"}); await refresh(); }));
        if (["running", "interrupted", "cancelling"].includes(command.state)) item.append(button("Reconcile stopped runtime", async () => {
          if (!confirm("Have you checked the runtime and verified this turn is stopped? This closes the command without replaying it. Any completed work remains in the runtime.")) return;
          await request(`/api/v1/agent-commands/${encodeURIComponent(command.id)}/control`, "POST", {action: "reconciled"}); await refresh();
        }));
        list.append(item);
      }
    }
    channel.input.addEventListener("change", () => refresh().catch(error => { status.textContent = error.message; }));
    form.append(channel.wrapper, button("Refresh queue", refresh));
    if (document.querySelector("#automation-open").hidden === false) {
      const agents = await request("/api/v1/agents");
      const agent = select("Agent to bind", agents.agents.filter(a => a.enabled).map(a => [a.name, a.display_name || a.name]));
      form.append(agent.wrapper, button("Bind agent to channel", async () => { await request(`/api/v1/channels/${encodeURIComponent(channel.input.value)}/agent-binding`, "PUT", {agent: agent.input.value}); status.textContent = "Channel bound. Only new messages enter the queue."; await refresh(); }));
    }
    content.append(list); await refresh();
  }

  document.querySelector("#workflow-open").addEventListener("click", () => attention().catch(error => { status.textContent = error.message; }));
  for (const [id, action] of [["workflow-schedules", attention], ["workflow-incidents", incidents], ["workflow-monitors", monitors], ["workflow-playground", playground], ["workflow-queue", queue]]) {
    document.querySelector(`#${id}`).addEventListener("click", () => action().catch(error => { status.textContent = error.message; }));
  }
  window.TintwireWorkflows = {
    cardButtons(notification) {
      const actions = element("div", "card-inbox-actions");
      actions.append(button("Snooze", () => snooze(notification)), button("Delivery details", () => delivery(notification))); return actions;
    },
  };

  if (desktopShell) {
    let lastErrorAt = 0;
    const poller = new DesktopAlertPoller({
      fetchPage: cursor => request(`/api/v1/desktop/alerts${cursor ? `?after=${encodeURIComponent(cursor)}` : ""}`),
      notify: async payload => {
        let outcome = "desktop_accepted";
        try { await desktopShell.invoke("alert", {payload}); }
        catch (error) { outcome = "desktop_failed"; throw error; }
        finally { if (payload.id.startsWith("ntf_")) request(`/api/v1/notifications/${encodeURIComponent(payload.id)}/desktop-delivery`, "POST", {outcome}).catch(error => console.error("Unable to record desktop delivery", error)); }
      },
      reportError: error => {
        console.error("Desktop notification delivery failed", error);
        if (Date.now() - lastErrorAt > 60000) { lastErrorAt = Date.now(); showInboxToast(`Desktop notifications: ${error.message}`); }
      },
    });
    window.TintwireWorkflows.pollDesktop = () => poller.poll();
    setInterval(() => poller.poll(), 15000);
    window.addEventListener("focus", () => poller.poll());
    poller.poll();
  }
})();
