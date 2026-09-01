"use strict";

const webAssetVersion = new URL(document.currentScript.src).searchParams.get("v") || "";
const list = document.querySelector("#list");

// Desktop shell bridge. The Tauri client injects window.__TAURI__ only for the
// configured origin; in a browser or installed PWA every call below is skipped
// and Web Push remains the alert path.
const desktopShell = window.__TAURI__?.core ? {
  invoke: window.__TAURI__.core.invoke,
  listen: window.__TAURI__.event?.listen,
  setUnread(count) {
    this.invoke("set_unread", {count: Number(count) || 0}).catch(() => {});
  },
  alert(payload) {
    this.invoke("alert", {payload}).catch(() => {});
  },
  beginOIDCLogin(handoff) {
    return this.invoke("begin_oidc_login", {handoff});
  },
  openExternal(url) {
    return this.invoke("open_external", {url});
  },
} : null;

// Webviews do not consistently create a new native window for target=_blank.
// Hand those explicit external-link clicks to the desktop shell instead; the
// browser/PWA path and ordinary in-app navigation remain unchanged.
document.addEventListener("click", event => {
  if (!desktopShell || !(event.target instanceof Element)) return;
  const link = event.target.closest("a[target='_blank'][href]");
  if (!link || link.hasAttribute("download")) return;
  let target;
  try { target = new URL(link.href, location.href); } catch (_) { return; }
  if (target.protocol !== "http:" && target.protocol !== "https:") return;
  event.preventDefault();
  desktopShell.openExternal(target.href).catch(error => console.error("Unable to open external link", error));
});
const alertButton = document.querySelector("#alert-button");
const alertStatus = document.querySelector("#alert-status");
const alertDialog = document.querySelector("#alert-dialog");
const alertDialogClose = document.querySelector("#alert-dialog-close");
const alertSetupCopy = document.querySelector("#alert-setup-copy");
const alertSetupStatus = document.querySelector("#alert-setup-status");
const alertSetupButton = document.querySelector("#alert-setup-button");
const alertInstallButton = document.querySelector("#alert-install-button");
const alertIOSGuide = document.querySelector("#alert-ios-guide");
const alertChannel = document.querySelector("#alert-channel");
const alertLevel = document.querySelector("#alert-level");
const alertPreferenceSave = document.querySelector("#alert-preference-save");
const alertPreferenceStatus = document.querySelector("#alert-preference-status");
const inboxFilters = document.querySelector("#inbox-filters");
const inboxSearch = document.querySelector("#inbox-search");
const channelFilter = document.querySelector("#channel-filter");
const stateFilter = document.querySelector("#state-filter");
const severityFilter = document.querySelector("#severity-filter");
const readFilter = document.querySelector("#read-filter");
const loadMoreButton = document.querySelector("#load-more");
const densityButton = document.querySelector("#density-button");
const loginOverlay = document.querySelector("#login-overlay");
const loginForm = document.querySelector("#login-form");
const loginError = document.querySelector("#login-error");
const oidcLoginButton = document.querySelector("#oidc-login-button");
const loginDivider = document.querySelector("#login-divider");
const logoutButton = document.querySelector("#logout-button");
const sessionIdentity = document.querySelector("#session-identity");
const readButton = document.querySelector("#read-button");
const topbarActions = document.querySelector(".topbar-actions");
const channelCreateButton = document.querySelector("#channel-create-button");
const channelEditButton = document.querySelector("#channel-edit-button");
const mobileChannelEditButton = document.querySelector("#mobile-channel-edit-button");
const createChannelDialog = document.querySelector("#create-channel-dialog");
const createChannelDialogClose = document.querySelector("#create-channel-dialog-close");
const createChannelForm = document.querySelector("#create-channel-form");
const createChannelName = document.querySelector("#create-channel-name");
const createChannelDisplay = document.querySelector("#create-channel-display");
const createChannelDescription = document.querySelector("#create-channel-description");
const createChannelAccent = document.querySelector("#create-channel-accent");
const createChannelVisibility = document.querySelector("#create-channel-visibility");
const createChannelStatus = document.querySelector("#create-channel-status");
const createChannelSubmit = document.querySelector("#create-channel-submit");
const automationOpen = document.querySelector("#automation-open");
const automationDialog = document.querySelector("#automation-dialog");
const automationClose = document.querySelector("#automation-close");
const usersOpen = document.querySelector("#users-open");
const usersDialog = document.querySelector("#users-dialog");
const usersClose = document.querySelector("#users-close");
const usersStatus = document.querySelector("#users-status");
const usersList = document.querySelector("#users-list");
const agentForm = document.querySelector("#agent-form");
const agentName = document.querySelector("#agent-name");
const agentDisplayName = document.querySelector("#agent-display-name");
const agentDescription = document.querySelector("#agent-description");
const agentOAuthSubject = document.querySelector("#agent-oauth-subject");
const agentAdmin = document.querySelector("#agent-admin");
const agentStatus = document.querySelector("#agent-status");
const agentList = document.querySelector("#agent-list");
const webhookForm = document.querySelector("#webhook-form");
const webhookChannel = document.querySelector("#webhook-channel");
const webhookChannelLocked = document.querySelector("#webhook-channel-locked");
const webhookStatus = document.querySelector("#webhook-status");
const webhookList = document.querySelector("#webhook-list");
const credentialPanel = document.querySelector("#credential-panel");
const credentialTitle = document.querySelector("#credential-title");
const credentialValue = document.querySelector("#credential-value");
const credentialCopy = document.querySelector("#credential-copy");
const filterConsole = document.querySelector("#filter-console");
const inboxNav = document.querySelector("#inbox-nav");
const filtersNav = document.querySelector("#filters-nav");
const activityNav = document.querySelector("#activity-nav");
const inbox = document.querySelector("#inbox");
const channelList = document.querySelector("#channel-list");
const mobileChannelList = document.querySelector("#mobile-channel-list");
const mobileChannelToggle = document.querySelector("#mobile-channel-toggle");
const channelDialog = document.querySelector("#channel-dialog");
const channelDialogClose = document.querySelector("#channel-dialog-close");
const inboxToast = document.querySelector("#inbox-toast");
const feedTitle = document.querySelector("#feed-title");
const savedViewList = document.querySelector("#saved-view-list");
const savedViewAdd = document.querySelector("#saved-view-add");
const savedViewDialog = document.querySelector("#saved-view-dialog");
const savedViewForm = document.querySelector("#saved-view-form");
const savedViewClose = document.querySelector("#saved-view-close");
const savedViewName = document.querySelector("#saved-view-name");
const savedViewChannels = document.querySelector("#saved-view-channels");
const savedViewStatus = document.querySelector("#saved-view-status");
const composer = document.querySelector("#composer");
const composerInput = document.querySelector("#composer-input");
const composerSubmit = document.querySelector("#composer-submit");
const composerStatus = document.querySelector("#composer-status");
const composerReply = document.querySelector("#composer-reply");
const composerReplyLabel = document.querySelector("#composer-reply-label");
const composerCancelReply = document.querySelector("#composer-cancel-reply");
let currentChannelID = "";
let replyingTo = null;
let timelineNextCursor = "";
let inboxStateEnabled = false;
let inboxToastTimer;
let isAdmin = false;
let currentUserID = "";
let latestUnreadCount = 0;
const sentMessageHoldMilliseconds = 10000;
const heldSentMessages = new Map();

function updateReadControl() {
  readButton.hidden = !inboxStateEnabled || selectedChannels.length > 0;
  const selected = channelCache.find(channel => channel.name === selectedChannel);
  const visibleUnreadCount = selected ? Number(selected.unread_count || 0) : latestUnreadCount;
  readButton.textContent = visibleUnreadCount
    ? `${selected ? "Mark channel read" : "Mark all read"} · ${visibleUnreadCount}`
    : "All caught up";
  readButton.disabled = visibleUnreadCount === 0;
}

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function showCredential(title, value) {
  credentialTitle.textContent = title;
  credentialValue.textContent = value;
  credentialPanel.hidden = false;
}

function automationItem(title, metadata, revoked, revoke, extraActions = []) {
  const item = element("div", "automation-item");
  const main = element("div", "automation-item-main");
  main.append(element("strong", "automation-item-title", title));
  main.append(element("div", "automation-item-meta", metadata));
  const actions = element("div", "automation-item-actions");
  const revokeButton = element("button", "automation-revoke-button", revoked ? "Revoked" : "Revoke");
  revokeButton.type = "button";
  revokeButton.disabled = revoked;
  if (!revoked) revokeButton.addEventListener("click", async () => {
    if (!confirm(`Revoke ${title}? Existing credentials will stop working.`)) return;
    revokeButton.disabled = true;
    try { await revoke(); } catch (error) { revokeButton.disabled = false; alert(`Unable to revoke: ${error.message}`); }
  });
  actions.append(...extraActions, revokeButton);
  item.append(main, actions);
  return item;
}

async function loadAgents() {
  const response = await fetch("/api/v1/agents");
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const data = await response.json();
  const agents = data.agents || [];
  if (!agents.length) {
    agentList.replaceChildren(element("div", "channel-loading", "No bots registered."));
    return;
  }
  agentList.replaceChildren(...agents.map(agent => {
    const grants = (agent.channels || []).join(", ") || "no channel grants";
    const authority = agent.is_admin ? "administrator" : "delegated";
    const used = agent.last_used_at ? ` · used ${new Date(agent.last_used_at).toLocaleString()}` : " · never used";
    return automationItem(agent.display_name || agent.name, `${agent.name} · ${authority} · ${grants}${used}`, !agent.enabled, async () => {
      const response = await fetch(`/api/v1/agents/${encodeURIComponent(agent.name)}/revoke`, {method:"POST"});
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      await loadAgents();
    });
  }));
}

async function loadWebhooks() {
  const response = await fetch("/api/v1/webhooks");
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const data = await response.json();
  const webhooks = data.webhooks || [];
  if (!webhooks.length) {
    webhookList.replaceChildren(element("div", "channel-loading", "No incoming webhooks."));
    return;
  }
  const groups = new Map();
  for (const webhook of webhooks) {
    const key = webhook.channel_id || webhook.channel;
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(webhook);
  }
  webhookList.replaceChildren(...[...groups.values()].map(group => {
    const card = element("section", "automation-item automation-webhook-group");
    const activeCount = group.filter(webhook => !webhook.revoked_at).length;
    const heading = element("div", "automation-webhook-heading");
    const headingCopy = element("div", "automation-item-main");
    headingCopy.append(element("strong", "automation-item-title", group[0].channel));
    headingCopy.append(element("span", "automation-item-meta", `${activeCount} active · ${group.length} total URL${group.length === 1 ? "" : "s"}`));
    const sourceWebhook = group.find(webhook => !webhook.revoked_at);
    const newURLButton = element("button", "automation-new-url-button", "New URL");
    newURLButton.type = "button";
    newURLButton.disabled = !sourceWebhook;
    newURLButton.addEventListener("click", async () => {
      newURLButton.disabled = true;
      try {
        const response = await fetch(`/api/v1/webhooks/${encodeURIComponent(sourceWebhook.id)}/new-url`, {method:"POST"});
        if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
        const data = await response.json();
        showCredential(`${data.webhook.channel} additional webhook URL`, `${location.origin}${data.path}`);
        webhookStatus.textContent = "Additional webhook URL created. Existing URLs remain active.";
        await loadWebhooks();
      } catch (error) {
        newURLButton.disabled = false;
        alert(`Unable to create webhook URL: ${error.message}`);
      }
    });
    heading.append(headingCopy, newURLButton);
    const urls = element("div", "automation-webhook-urls");
    for (const webhook of group) {
      const routing = webhook.channel_locked ? "channel locked" : "public override allowed";
      const revoked = Boolean(webhook.revoked_at);
      const row = element("div", `automation-webhook-url${revoked ? " automation-webhook-url-revoked" : ""}`);
      const identity = element("div", "automation-item-main");
      identity.append(element("code", "automation-webhook-id", webhook.id));
      identity.append(element("div", "automation-item-meta", `${routing} · ${revoked ? "revoked" : `created ${new Date(webhook.created_at).toLocaleString()}`}`));
      const actions = element("div", "automation-item-actions");
      const lockButton = element("button", "automation-lock-button", webhook.channel_locked ? "Allow overrides" : "Lock to channel");
      lockButton.type = "button";
      lockButton.disabled = revoked;
      lockButton.setAttribute("aria-pressed", String(webhook.channel_locked));
      lockButton.addEventListener("click", async () => {
        lockButton.disabled = true;
        try {
          const response = await fetch(`/api/v1/webhooks/${encodeURIComponent(webhook.id)}`, {method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({channel_locked:!webhook.channel_locked})});
          if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
          await loadWebhooks();
        } catch (error) {
          lockButton.disabled = false;
          alert(`Unable to update webhook routing: ${error.message}`);
        }
      });
      const revokeButton = element("button", "automation-revoke-button", revoked ? "Revoked" : "Revoke");
      revokeButton.type = "button";
      revokeButton.disabled = revoked;
      revokeButton.addEventListener("click", async () => {
        if (!confirm(`Revoke ${webhook.id}? This URL will stop working.`)) return;
        revokeButton.disabled = true;
        try {
          const response = await fetch(`/api/v1/webhooks/${encodeURIComponent(webhook.id)}/revoke`, {method:"POST"});
          if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
          await loadWebhooks();
        } catch (error) {
          revokeButton.disabled = false;
          alert(`Unable to revoke webhook: ${error.message}`);
        }
      });
      actions.append(lockButton, revokeButton);
      row.append(identity, actions);
      urls.append(row);
    }
    card.append(heading, urls);
    return card;
  }));
}

async function openAutomation() {
  credentialPanel.hidden = true;
  webhookChannel.replaceChildren(element("option", "", "Choose channel…"));
  webhookChannel.firstChild.value = "";
  for (const channel of channelCache) {
    const option = element("option", "", channel.display_name || channel.name);
    option.value = channel.id;
    webhookChannel.append(option);
  }
  automationDialog.showModal();
  const results = await Promise.allSettled([loadAgents(), loadWebhooks()]);
  if (results[0].status === "rejected") agentStatus.textContent = `Unable to load bots: ${results[0].reason.message}`;
  if (results[1].status === "rejected") webhookStatus.textContent = `Unable to load webhooks: ${results[1].reason.message}`;
}

async function updateManagedUser(userID, change) {
  const response = await fetch(`/api/v1/admin/users/${encodeURIComponent(userID)}`, {method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify(change)});
  if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
}

async function loadManagedUsers() {
  usersStatus.textContent = "Loading users…";
  const response = await fetch("/api/v1/admin/users");
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const data = await response.json();
  const cards = (data.users || []).map(user => {
    const card = element("section", "managed-user");
    const heading = element("div", "managed-user-heading");
    const identity = element("div", "managed-user-identity");
    identity.append(element("strong", "", user.username));
    identity.append(element("span", "managed-user-meta", `${user.auth_type} · ${user.session_count} active session${user.session_count === 1 ? "" : "s"}${user.disabled_at ? " · disabled" : ""}`));
    heading.append(identity);
    const actions = element("div", "managed-user-actions");
    const action = (label, handler, dangerous = false) => { const button=element("button",dangerous?"danger":"",label);button.type="button";button.disabled=user.protected;button.addEventListener("click",async()=>{button.disabled=true;try{await handler();await loadManagedUsers()}catch(error){usersStatus.textContent=error.message;button.disabled=user.protected}});actions.append(button); };
    action(user.is_admin ? "Demote" : "Make admin", () => updateManagedUser(user.id,{is_admin:!user.is_admin}), user.is_admin);
    action(user.disabled_at ? "Enable" : "Disable", () => updateManagedUser(user.id,{disabled:!user.disabled_at}), !user.disabled_at);
    action("Revoke sessions", async()=>{const result=await fetch(`/api/v1/admin/users/${encodeURIComponent(user.id)}/sessions`,{method:"DELETE"});if(!result.ok)throw new Error((await result.text()).trim()||`HTTP ${result.status}`)}, true);
    if (user.auth_type === "password") action("Reset password", async()=>{const password=prompt(`New password for ${user.username} (minimum 12 characters)`);if(password===null)return;await updateManagedUser(user.id,{password})});
    heading.append(actions); card.append(heading);
    if (!user.protected) {
      const memberships = element("div", "managed-memberships");
      for (const channel of channelCache) {
        const row = element("label", "managed-membership"); row.append(element("span","",channel.display_name||channel.name));
        const select = document.createElement("select"); select.setAttribute("aria-label",`${user.username} role in ${channel.display_name||channel.name}`);
        for (const [value,label] of [["","No explicit role"],["viewer","Viewer"],["operator","Operator"],["channel_admin","Channel admin"]]) {const option=element("option","",label);option.value=value;select.append(option)}
        select.value=(user.memberships||[]).find(value=>value.channel_id===channel.id)?.role||"";
        select.addEventListener("change",async()=>{select.disabled=true;const result=await fetch(`/api/v1/admin/users/${encodeURIComponent(user.id)}/memberships/${encodeURIComponent(channel.id)}`,{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({role:select.value})});if(!result.ok)usersStatus.textContent=(await result.text()).trim()||`HTTP ${result.status}`;select.disabled=false});
        row.append(select); memberships.append(row);
      }
      const details=document.createElement("details");details.className="managed-membership-details";details.append(element("summary","",`Channel access · ${(user.memberships||[]).length} explicit`),memberships);card.append(details);
    }
    return card;
  });
  usersList.replaceChildren(...cards);
  usersStatus.textContent = `${cards.length} identities. Protected system and agent identities are read-only.`;
}

function appendInlineMarkup(node, text) {
  for (const token of TintwireMarkdown.parse(text)) {
    if (token.type === "strong") {
      node.append(element("strong", "", token.value));
    } else if (token.type === "code") {
      node.append(element("code", "", token.value));
    } else if (token.type === "link") {
      const href = safeHTTPURL(token.href);
      if (href) {
        const link = element("a", "", token.value);
        link.href = href;
        link.target = "_blank";
        link.rel = "noopener noreferrer";
        node.append(link);
      } else {
        node.append(document.createTextNode(token.value));
      }
    } else if (token.type === "emoji") {
      const emoji = element("span", "emoji", token.value);
      emoji.setAttribute("role", "img");
      emoji.setAttribute("aria-label", `:${token.name}:`);
      node.append(emoji);
    } else {
      node.append(document.createTextNode(token.value));
    }
  }
}

function richText(className, text) {
  const node = element("div", className);
  appendInlineMarkup(node, text);
  return node;
}

function tableBlock(block) {
  const wrapper = element("div", "table-wrap");
  const table = element("table", "mattermost-table");
  const head = document.createElement("thead");
  const headRow = document.createElement("tr");
  block.headers.forEach((value, index) => {
    const cell = document.createElement("th");
    cell.style.textAlign = block.alignments[index];
    appendInlineMarkup(cell, value);
    headRow.append(cell);
  });
  head.append(headRow);
  table.append(head);

  const body = document.createElement("tbody");
  for (const row of block.rows) {
    const tableRow = document.createElement("tr");
    row.forEach((value, index) => {
      const cell = document.createElement("td");
      cell.style.textAlign = block.alignments[index];
      appendInlineMarkup(cell, value);
      tableRow.append(cell);
    });
    body.append(tableRow);
  }
  table.append(body);
  wrapper.append(table);
  return wrapper;
}

function richContent(className, text) {
  const container = element("div", className);
  for (const block of TintwireMarkdown.parseBlocks(text)) {
    if (block.type === "table") {
      container.append(tableBlock(block));
    } else {
      container.append(richText("formatted-text", block.value));
    }
  }
  return container;
}

function safeHTTPURL(value) {
  if (typeof value !== "string" || !value.trim()) return "";
  try {
    const parsed = new URL(value, location.href);
    return parsed.protocol === "http:" || parsed.protocol === "https:" ? parsed.href : "";
  } catch (_) {
    return "";
  }
}

function remoteImage(className, source, alt) {
  const url = safeHTTPURL(source);
  if (!url) return null;
  const image = element("img", className);
  image.src = url;
  image.alt = alt || "";
  image.loading = "lazy";
  image.referrerPolicy = "no-referrer";
  return image;
}

function applyAttachmentColor(card, value) {
  if (typeof value.color !== "string") return;
  const color = value.color.trim().toLowerCase();
  if (["danger", "warning", "good"].includes(color)) {
    card.classList.add("attachment-colored");
    card.classList.add(`attachment-${color}`);
    return;
  }
  if (/^#[0-9a-f]{6}$/i.test(color) || /^#[0-9a-f]{3}$/i.test(color)) {
    card.classList.add("attachment-colored");
    card.style.borderLeftColor = color;
  }
}

function attachmentCard(value, notification, attachmentIndex) {
  const card = element("div", "attachment");
  applyAttachmentColor(card, value);
  if (value.pretext) card.append(richContent("attachment-pretext", value.pretext));
  if (value.author_name) card.append(element("div", "attachment-author", value.author_name));
  if (value.title) {
    const titleURL = safeHTTPURL(value.title_link);
    const title = titleURL ? element("a", "attachment-title", value.title) : element("div", "attachment-title", value.title);
    if (titleURL) { title.href = titleURL; title.target = "_blank"; title.rel = "noopener noreferrer"; }
    card.append(title);
  }
  if (value.text) card.append(richContent("attachment-text", value.text));
  if (Array.isArray(value.fields)) {
    const fields = element("div", "field-grid");
    for (const field of value.fields) {
      const item = element("div", "field");
      item.append(element("b", "", field.title || ""));
      item.append(richContent("field-value", field.value || ""));
      fields.append(item);
    }
    card.append(fields);
  }
  const image = remoteImage("attachment-image", value.image_url, value.title || value.fallback || "Attachment image");
  if (image) card.append(image);
  const thumbnail = value.thumb_url !== value.image_url
    ? remoteImage("attachment-thumbnail", value.thumb_url, value.title || value.fallback || "Attachment thumbnail")
    : null;
  if (thumbnail) card.append(thumbnail);
  if (value.footer) card.append(element("div", "attachment-footer", value.footer));
  const actionResult = value.action_result;
  if (Array.isArray(value.actions) && (notification.can_operate || actionResult)) {
    const actions = element("div", "native-actions");
    const feedback = element("div", `action-feedback${actionResult?.status === "succeeded" ? " action-feedback-success" : actionResult ? " action-feedback-failed" : ""}`);
    const resultText = result => {
      const actor = result.actor ? ` · ${result.actor}` : "";
      return `${result.response || (result.status === "succeeded" ? "Action completed." : "Action failed.")}${actor}`;
    };
    if (actionResult) feedback.textContent = resultText(actionResult);
    value.actions.forEach((action, actionIndex) => {
      if (!action.executable && !actionResult) return;
      const selected = Boolean(action.selected || actionResult?.action_index === actionIndex);
      const button = element("button", `native-action${selected ? " action-selected" : ""}`, `${action.name || action.id || "Run action"}${selected ? " ✓" : ""}`);
      button.type = "button";
      button.disabled = !notification.can_operate || action.executable === false || actionResult?.status === "succeeded";
      button.addEventListener("click", async () => {
        for (const item of actions.querySelectorAll("button")) item.disabled = true;
        feedback.className = "action-feedback";
        feedback.textContent = "Running action…";
        const key = crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random().toString(16).slice(2)}`;
        const response = await fetch(`/api/v1/notifications/${encodeURIComponent(notification.id)}/mattermost-actions/${attachmentIndex}/${actionIndex}`, {method:"POST", headers:{"Idempotency-Key":key}});
        let result = {};
        try { result = await response.json(); } catch (_) { result = {}; }
        if (response.ok) {
          button.classList.add("action-selected");
          button.textContent = `${action.name || action.id || "Run action"} ✓`;
          feedback.classList.add("action-feedback-success");
          feedback.textContent = resultText(result);
          await loadNotifications(false);
        } else {
          feedback.classList.add("action-feedback-failed");
          feedback.textContent = result.response || `Action failed (HTTP ${response.status}).`;
          for (const item of actions.querySelectorAll("button")) item.disabled = false;
        }
      });
      actions.append(button);
    });
    card.append(actions, feedback);
  }
  return card;
}

function nativeCard(value, notification) {
  const card = element("section", `native-card severity-${value.severity || "info"}`);
  const heading = element("div", "native-heading");
  heading.append(element("h2", "native-title", value.title));
  if (value.severity) heading.append(element("span", "severity-badge", value.severity));
  card.append(heading);
  if (value.summary) card.append(richContent("native-summary", value.summary));
  if (Array.isArray(value.badges) && value.badges.length) {
    const badges = element("div", "card-badges");
    for (const badge of value.badges) badges.append(element("span", `card-badge badge-${badge.tone || "neutral"}`, badge.label));
    card.append(badges);
  }
  if (Array.isArray(value.fields) && value.fields.length) {
    const fields = element("div", "field-grid");
    for (const field of value.fields) { const item=element("div","field");item.append(element("b","",field.label));item.append(richContent("field-value",field.value));fields.append(item); }
    card.append(fields);
  }
  if (Array.isArray(value.images)) {
    for (const image of value.images) { const node=element("img","card-image");node.src=image.url;node.alt=image.alt;node.loading="lazy";node.referrerPolicy="no-referrer";card.append(node); }
  }
  if (Array.isArray(value.links) && value.links.length) {
    const links=element("div","card-links");for(const valueLink of value.links){const link=element("a","card-link",valueLink.label);link.href=valueLink.url;link.target="_blank";link.rel="noopener noreferrer";links.append(link)}card.append(links);
  }
  if (Array.isArray(value.metrics) && value.metrics.length) {
    const metrics = element("div", "metric-grid");
    for (const metric of value.metrics) {
      const item = element("div", "metric");
      item.append(element("div", "metric-value", String(metric.value)));
      item.append(element("div", "metric-label", metric.label));
      metrics.append(item);
    }
    card.append(metrics);
  }
  if (Array.isArray(value.rows) && value.rows.length) {
    const filterPanel = element("div", "row-filter-panel");
    const controls = element("div", "row-controls");
    const search = document.createElement("input");
    search.type = "search";
    search.placeholder = "Filter rows…";
    search.setAttribute("aria-label", "Filter card rows");
    controls.append(search);
    const count = element("span", "row-count");
    controls.append(count);
    filterPanel.append(controls);
    const filterButtons = element("div", "row-filter-buttons");
    const tags = [...new Set(value.rows.flatMap(row => row.tags || []))].sort((left, right) => left.localeCompare(right));
    let selectedTag = "";
    let crossOnly = false;
    const allButton = element("button", "row-filter active", "All");
    allButton.type = "button";
    filterButtons.append(allButton);
    const tagButtons = tags.map(tag => {
      const button = element("button", "row-filter", tag);
      button.type = "button";
      button.dataset.tag = tag;
      filterButtons.append(button);
      return button;
    });
    const crossButton = element("button", "row-filter", "Cross-list");
    crossButton.type = "button";
    filterButtons.append(crossButton);
    filterPanel.append(filterButtons);
    card.append(filterPanel);
    const rows = element("div", "native-rows");
    const moreButton = element("button", "row-show-more", "");
    moreButton.type = "button";
    const mobilePageSize = 25;
    let visibleLimit = matchMedia("(max-width: 700px)").matches ? mobilePageSize : Number.POSITIVE_INFINITY;
    const renderRows = () => {
      const query = search.value.trim().toLocaleLowerCase();
      const matches = value.rows.filter(row =>
        (!query || row.primary.toLocaleLowerCase().includes(query) || (row.tags || []).some(tag => tag.toLocaleLowerCase().includes(query))) &&
        (!selectedTag || (row.tags || []).includes(selectedTag)) &&
        (!crossOnly || (row.tags || []).length > 1)
      );
      const visible = matches.slice(0, visibleLimit);
      count.textContent = `${visible.length} shown · ${matches.length} matching · ${value.rows.length} total`;
      rows.replaceChildren(...visible.map(row => {
        const item = element("div", `native-row${row.emphasis === "strong" ? " row-strong" : ""}`);
        item.append(element("code", "row-primary", row.primary));
        const tags = element("span", "row-tags");
        for (const tag of row.tags || []) tags.append(element("span", "row-tag", tag));
        item.append(tags);
        return item;
      }));
      moreButton.hidden = visible.length === matches.length;
      moreButton.textContent = `Show ${Math.min(mobilePageSize, matches.length - visible.length)} more`;
    };
    const resetVisibleRows = () => {
      visibleLimit = matchMedia("(max-width: 700px)").matches ? mobilePageSize : Number.POSITIVE_INFINITY;
    };
    allButton.addEventListener("click", () => {
      selectedTag = ""; crossOnly = false;
      resetVisibleRows();
      [...tagButtons, crossButton].forEach(button => button.classList.remove("active"));
      allButton.classList.add("active"); renderRows();
    });
    for (const button of tagButtons) button.addEventListener("click", () => {
      selectedTag = button.dataset.tag; crossOnly = false;
      resetVisibleRows();
      [allButton, ...tagButtons, crossButton].forEach(item => item.classList.toggle("active", item === button));
      renderRows();
    });
    crossButton.addEventListener("click", () => {
      selectedTag = ""; crossOnly = true;
      resetVisibleRows();
      [allButton, ...tagButtons, crossButton].forEach(item => item.classList.toggle("active", item === crossButton));
      renderRows();
    });
    search.addEventListener("input", () => { resetVisibleRows(); renderRows(); });
    moreButton.addEventListener("click", () => { visibleLimit += mobilePageSize; renderRows(); });
    renderRows();
    card.append(rows, moreButton);
  }
  if (Array.isArray(value.actions) && value.actions.length) {
    const actions = element("div", "native-actions");
    value.actions.forEach((action, index) => {
      if (action.type === "http" && notification.can_operate) {
        const button = element("button", "native-action", action.label); button.type="button";
        button.addEventListener("click", async () => {
          button.disabled=true;
          const operationKey = crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random().toString(16).slice(2)}`;
          const response = await fetch(`/api/v1/notifications/${encodeURIComponent(notification.id)}/actions/${index}`, {method:"POST",headers:{"Idempotency-Key":operationKey}});
          if (response.ok) loadNotifications(false); else button.disabled=false;
        });
        actions.append(button);
      } else if (action.type === "link") {
        const link = element("a", "native-action", action.label); link.href=action.url; link.target="_blank"; link.rel="noopener noreferrer"; actions.append(link);
      }
    });
    card.append(actions);
  }
  return card;
}

function activityItem(value) {
  const item = element("div", `activity-item activity-item-${value.state}`);
  const head = element("div", "activity-head");
  if (["firing", "acknowledged", "resolved"].includes(value.state)) {
    head.append(element("span", `state-badge state-${value.state}`, value.state));
  }
  head.append(document.createTextNode(`${new Date(value.created_at).toLocaleString()}${value.actor ? ` · ${value.actor}` : ""}`));
  item.append(head);
  if (value.title) item.append(element("div", "activity-title", value.title));
  if (value.text) item.append(richContent("activity-text", value.text));
  return item;
}

function operationButtons(notification) {
  const disclosure = element("details", "incident-state-actions");
  const summary = element("summary", "", "Incident state");
  summary.title = "Shared with everyone in this channel";
  const actions = element("div", "operation-buttons");
  actions.append(element("div", "incident-state-copy", "These changes are shared with everyone in this channel."));
  const feedback = element("div", "action-feedback");
  const transition = async state => {
    for (const button of actions.querySelectorAll("button")) button.disabled = true;
    feedback.className = "action-feedback";
    feedback.textContent = "";
    try {
      const response = await fetch(`/api/v1/notifications/${encodeURIComponent(notification.id)}/state`, {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({state})});
      if (!response.ok) {
        const detail = (await response.text()).trim();
        throw new Error(detail || `HTTP ${response.status}`);
      }
      await loadNotifications(false);
    } catch (error) {
      feedback.className = "action-feedback action-feedback-failed";
      feedback.textContent = `Unable to ${state === "resolved" ? "resolve" : "acknowledge"}: ${error.message}`;
      for (const button of actions.querySelectorAll("button")) button.disabled = false;
    }
  };
  if (notification.state === "received" || notification.state === "firing") {
    const acknowledge = element("button", "operation-button", "Acknowledge"); acknowledge.type="button"; acknowledge.addEventListener("click", () => transition("acknowledged")); actions.append(acknowledge);
  }
  if (notification.state !== "resolved") {
    const resolve = element("button", "operation-button operation-resolve", "Resolve"); resolve.type="button"; resolve.addEventListener("click", () => transition("resolved")); actions.append(resolve);
  }
  actions.append(feedback);
  disclosure.append(summary, actions);
  return disclosure;
}

function approvalButtons(notification) {
  const actions=element("div","operation-buttons");
  for (const [decision,label] of [["approve","Approve"],["reject","Reject"]]) {
    const button=element("button",`operation-button${decision==="reject"?" operation-reject":""}`,label);button.type="button";
    button.addEventListener("click",async()=>{for(const item of actions.querySelectorAll("button"))item.disabled=true;const response=await fetch(`/api/v1/notifications/${encodeURIComponent(notification.id)}/approval`,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({decision})});if(response.ok)loadNotifications(false);else for(const item of actions.querySelectorAll("button"))item.disabled=false;});actions.append(button);
  }
  return actions;
}

function activityHistory(notification) {
  const details = element("details", "activity");
  details.append(element("summary", "", `Activity history · ${notification.event_count} events`));
  const body = element("div", "activity-list");
  details.append(body);
  let loaded = false;
  details.addEventListener("toggle", async () => {
    if (!details.open || loaded) return;
    loaded = true;
    body.append(element("div", "activity-loading", "Loading activity…"));
    try {
      const response = await fetch(`/api/v1/notifications/${encodeURIComponent(notification.id)}/events`);
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const data = await response.json();
      body.replaceChildren(...(data.events || []).map(activityItem));
    } catch (error) {
      loaded = false;
      body.replaceChildren(element("div", "activity-error", `Unable to load activity: ${error.message}`));
    }
  });
  return details;
}

const collapsedNotificationIDs = new Set();

function notificationLabel(value) {
  let label = value.card?.title || value.attachments?.[0]?.title || value.attachments?.[0]?.fallback || value.text || "Notification";
  label = String(label).replace(/\s+/g, " ").trim();
  return label.length > 120 ? `${label.slice(0, 119).trim()}…` : label;
}

function compactAlertText(value, limit = 180) {
  const normalized = String(value ?? "").replace(/\s+/g, " ").trim();
  const characters = Array.from(normalized);
  return characters.length > limit ? `${characters.slice(0, limit - 1).join("").trim()}…` : normalized;
}

// Desktop alerts keep the structured subject in the native notification title
// and choose the first useful piece of card content for its body. Some producers
// intentionally omit summary and put all detail in fields, metrics, or rows.
function notificationAlertPresentation(value) {
  const card = value.card || null;
  const attachment = value.attachments?.[0] || null;
  const subject = compactAlertText(card?.title || attachment?.title || "", 100);
  const candidates = [card?.summary, value.text, attachment?.text, attachment?.fallback];
  for (const field of card?.fields || attachment?.fields || []) candidates.push(`${field.label || field.title}: ${field.value}`);
  for (const metric of card?.metrics || []) candidates.push(`${metric.label}: ${metric.value}`);
  if (card?.rows?.length) candidates.push(`${card.rows.length} items · ${card.rows[0].primary}`);
  const body = candidates.map(candidate => compactAlertText(candidate)).find(candidate => candidate && candidate.toLocaleLowerCase() !== subject.toLocaleLowerCase()) || "";
  return {subject, body};
}

function showInboxToast(message, actionLabel, action) {
  clearTimeout(inboxToastTimer);
  const copy = element("span", "", message);
  const children = [copy];
  if (actionLabel && action) {
    const button = element("button", "", actionLabel);
    button.type = "button";
    button.addEventListener("click", async () => {
      inboxToast.hidden = true;
      await action();
    });
    children.push(button);
  }
  inboxToast.replaceChildren(...children);
  inboxToast.hidden = false;
  inboxToastTimer = setTimeout(() => { inboxToast.hidden = true; }, 6000);
}

async function updateInboxState(notification, action) {
  // Capture this before the request: removing the focused bottom card can
  // change WebKit's scroll position before the refreshed timeline is rendered.
  const keepTimelineAtBottom = Boolean(selectedChannel) && isTimelineNearBottom();
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(notification.id)}/inbox`, {
    method: "POST",
    headers: {"Content-Type":"application/json"},
    body: JSON.stringify({action})
  });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  if (action === "dismiss") {
    if (selectedChannel) {
      loadedTimelineItems = loadedTimelineItems.filter(item =>
        item.kind !== "notification" || item.notification?.id !== notification.id
      );
      renderChannelTimeline(loadedTimelineItems);
      if (keepTimelineAtBottom) anchorTimelineToBottom();
    } else {
      loadedNotifications = loadedNotifications.filter(value => value.id !== notification.id);
      render(loadedNotifications);
    }
    collapsedNotificationIDs.delete(notification.id);
    showInboxToast("Notification archived", "Undo", async () => {
      try {
        await updateInboxState(notification, "restore");
      } catch (error) {
        showInboxToast(`Unable to restore notification: ${error.message}`);
      }
    });
  } else if (action === "read" || action === "unread") {
    notification.unread = action === "unread";
    await loadNotifications(false, false, keepTimelineAtBottom);
    showInboxToast(action === "read" ? "Marked as read" : "Marked as unread");
  }
  await loadChannels();
  if (action === "dismiss" || action === "restore") {
    await loadNotifications(false, false, keepTimelineAtBottom);
  }
}

function inboxButtons(notification) {
  const inDismissedMode = stateFilter.value === "dismissed";
  const actions = element("div", "card-inbox-actions");
  if (inDismissedMode) {
    const restore = element("button", "card-inbox-button", "Restore");
    restore.type = "button";
    restore.addEventListener("click", async () => {
      restore.disabled = true;
      try {
        await updateInboxState(notification, "restore");
      } catch (error) {
        restore.disabled = false;
        showInboxToast(`Unable to restore notification: ${error.message}`);
      }
    });
    actions.append(restore);
    return actions;
  }
  const read = element("button", "card-inbox-button", notification.unread ? "Mark read" : "Mark unread");
  const archive = element("button", "card-inbox-button card-dismiss-button", "Archive");
  read.title = notification.unread ? "Remove from the unread view; keep in history" : "Return to the unread view";
  archive.title = "Hide from normal history; restore from the Archived filter";
  read.type = archive.type = "button";
  read.addEventListener("click", async () => {
    read.disabled = true;
    try {
      await updateInboxState(notification, notification.unread ? "read" : "unread");
    } catch (error) {
      read.disabled = false;
      showInboxToast(`Unable to update notification: ${error.message}`);
    }
  });
  archive.addEventListener("click", async () => {
    archive.disabled = true;
    try {
      await updateInboxState(notification, "dismiss");
    } catch (error) {
      archive.disabled = false;
      showInboxToast(`Unable to archive notification: ${error.message}`);
    }
  });
  actions.append(read);
  if (readFilter.value !== "1") actions.append(archive);
  return actions;
}

function swipeCard(card, notification) {
  if (stateFilter.value === "dismissed") return card;
  const shell = element("div", "swipe-shell");
  const readAction = element("button", "swipe-action swipe-action-read", notification.unread ? "Mark read" : "Mark unread");
  const dismissAction = element("button", "swipe-action swipe-action-dismiss", "Archive");
  const canArchive = readFilter.value !== "1";
  dismissAction.hidden = !canArchive;
  const setSwipeState = async (action, button) => {
    button.disabled = true;
    dismissAction.disabled = true;
    readAction.disabled = true;
    try {
      await updateInboxState(notification, action);
    } catch (error) {
      showInboxToast(`Unable to ${action} notification: ${error.message}`);
    } finally {
      if (!card.isConnected) return;
      button.disabled = false;
      dismissAction.disabled = false;
      readAction.disabled = false;
      card.style.transform = "";
    }
  };
  dismissAction.type = "button";
  readAction.type = "button";
  readAction.addEventListener("click", async () => {
    await setSwipeState(notification.unread ? "read" : "unread", readAction);
  });
  dismissAction.addEventListener("click", async () => {
    await setSwipeState("dismiss", dismissAction);
  });
  shell.append(readAction, dismissAction, card);

  let startX = 0;
  let startY = 0;
  let offset = 0;
  let tracking = false;
  let horizontal = false;
  card.addEventListener("pointerdown", event => {
    if (event.pointerType !== "touch" || event.target.closest("button,a,input,select,textarea,summary,.table-wrap,.native-rows")) return;
    startX = event.clientX;
    startY = event.clientY;
    offset = 0;
    tracking = true;
    horizontal = false;
    shell.classList.add("swipe-shell-active");
    card.classList.add("card-swiping");
    try { card.setPointerCapture(event.pointerId); } catch (_) {}
  });
  card.addEventListener("pointermove", event => {
    if (!tracking) return;
    const dx = event.clientX - startX;
    const dy = event.clientY - startY;
    if (!horizontal && Math.abs(dx) < 8 && Math.abs(dy) < 8) return;
    if (!horizontal && Math.abs(dy) >= Math.abs(dx)) {
      tracking = false;
      shell.classList.remove("swipe-shell-active");
      card.classList.remove("card-swiping");
      return;
    }
    horizontal = true;
    offset = Math.max(canArchive ? -96 : 0, Math.min(110, dx));
    card.style.transform = `translateX(${offset}px)`;
    if (event.cancelable) event.preventDefault();
  });
  const finish = async () => {
    if (!tracking) return;
    tracking = false;
    shell.classList.remove("swipe-shell-active");
    card.classList.remove("card-swiping");
    if (horizontal && offset > 72) {
      card.style.transform = "";
      await setSwipeState(notification.unread ? "read" : "unread", readAction);
    } else if (canArchive && horizontal && offset < -72) {
      await setSwipeState("dismiss", dismissAction);
    } else {
      card.style.transform = "";
    }
  };
  card.addEventListener("pointerup", finish);
  card.addEventListener("pointercancel", () => {
    tracking = false;
    shell.classList.remove("swipe-shell-active");
    card.classList.remove("card-swiping");
    card.style.transform = "";
  });
  return shell;
}

let selectedNotificationID = "";

function notificationCards() {
  return [...list.querySelectorAll(".card")];
}

function selectedCard() {
  return selectedNotificationID
    ? list.querySelector(`#notification-${CSS.escape(selectedNotificationID)}`)
    : null;
}

// Selection is keyed by notification ID rather than position, so a background
// refresh or a newly arrived card does not move the highlight out from under
// whoever is reading.
function selectCard(card, {scroll = true} = {}) {
  for (const other of notificationCards()) {
    other.classList.toggle("card-selected", other === card);
  }
  selectedNotificationID = card ? card.id.replace(/^notification-/, "") : "";
  if (card && scroll) card.scrollIntoView({block: "nearest"});
}

function notificationCardNode(value) {
  const inDismissedMode = stateFilter.value === "dismissed";
  const card = element("article", "card");
  card.id = `notification-${value.id}`;
  const meta = element("div", "meta");
  const avatar = remoteImage("message-avatar", value.icon_url, `${value.username || "Message"} avatar`);
  if (avatar) meta.append(avatar);
  if (value.unread) {
    meta.append(element("span", "unread-dot"));
    card.classList.add("card-unread");
  }
  if (["firing", "acknowledged", "resolved"].includes(value.state)) {
    meta.append(element("span", `state-badge state-${value.state}`, value.state));
    card.classList.add(`card-${value.state}`);
  }
  const changed = value.updated_at && value.updated_at !== value.created_at;
  const timestamp = new Date(changed ? value.updated_at : value.created_at).toLocaleString();
  meta.append(element("span", "meta-channel", value.channel_name));
  meta.append(element("span", "meta-source", value.username));
  if (value.agent) meta.append(element("span", "state-badge agent-badge", `agent ${value.agent}`));
  meta.append(element("time", "meta-time", `${changed ? "updated " : ""}${timestamp}`));
  const collapseButton = element("button", "card-collapse");
  collapseButton.type = "button";
  const body = element("div", "card-body");
  const collapsedTitle = element("div", "card-collapsed-title", notificationLabel(value));
  const setCollapsed = collapsed => {
    card.classList.toggle("card-collapsed", collapsed);
    body.hidden = collapsed;
    collapseButton.textContent = collapsed ? "⌄" : "⌃";
    collapseButton.setAttribute("aria-expanded", String(!collapsed));
    collapseButton.setAttribute("aria-label", `${collapsed ? "Expand" : "Collapse"} notification: ${notificationLabel(value)}`);
    if (collapsed) collapsedNotificationIDs.add(value.id);
    else collapsedNotificationIDs.delete(value.id);
  };
  collapseButton.addEventListener("click", () => setCollapsed(!body.hidden));
  meta.append(collapseButton);
  card.append(meta, collapsedTitle, body);
  if (value.card) {
    body.append(nativeCard(value.card, value));
  } else if (value.text) {
    body.append(richContent("text", value.text));
  }
  if (!value.card && Array.isArray(value.attachments)) {
    value.attachments.forEach((attachment,index)=>body.append(attachmentCard(attachment,value,index)));
  }
  // Plain webhook posts default to "received" for storage and filtering, but
  // that does not make them incidents. Only notifications that entered an
  // actionable lifecycle expose shared Acknowledge/Resolve controls.
  if (value.can_operate && ["firing", "acknowledged"].includes(value.state)) body.append(operationButtons(value));
  if (value.can_approve) body.append(approvalButtons(value));
  if (value.event_count > 1) body.append(activityHistory(value));
  if (inboxStateEnabled) body.append(inboxButtons(value));
  setCollapsed(collapsedNotificationIDs.has(value.id));
  return mailboxEnabled(inDismissedMode) ? swipeCard(card, value) : card;
}

function mailboxEnabled(inDismissedMode) { return inboxStateEnabled && !inDismissedMode; }

function stableRenderedNode(cache, next, key, value, build) {
  const signature = JSON.stringify(value);
  const cached = cache.get(key);
  const node = cached?.signature === signature ? cached.node : build();
  next.set(key, {signature, node});
  return node;
}

// Keep matching nodes mounted so refreshes do not flash cards, restart their
// images, or discard focus and transient interaction state.
function reconcileListChildren(nodes) {
  for (let index = 0; index < nodes.length; index++) {
    const node = nodes[index];
    const current = list.children[index] || null;
    if (current !== node) list.insertBefore(node, current);
  }
  while (list.children.length > nodes.length) list.lastElementChild.remove();
}

let renderedNotificationNodes = new Map();

function render(values) {
  if (!values.length) {
    renderedNotificationNodes = new Map();
    reconcileListChildren([element("div", "empty", "No notifications yet. Publish a notification to get started.")]);
    return;
  }
  const requestedID = new URLSearchParams(location.search).get("notification");
  if (requestedID) collapsedNotificationIDs.delete(requestedID);
  const next = new Map();
  const nodes = values.map(value => {
    const node = stableRenderedNode(
      renderedNotificationNodes,
      next,
      `notification:${value.id}`,
      value,
      () => notificationCardNode(value),
    );
    const card = node.matches(".card") ? node : node.querySelector(".card");
    card?.classList.toggle("card-target", value.id === requestedID);
    return node;
  });
  reconcileListChildren(nodes);
  renderedNotificationNodes = next;

  if (requestedID) {
    const target = document.getElementById(`notification-${requestedID}`);
    if (target) {
      target.classList.add("card-target");
      target.scrollIntoView({block: "center"});
    }
  }
  const stillPresent = selectedCard();
  if (stillPresent) selectCard(stillPresent, {scroll: false});
  else selectedNotificationID = "";
}

let loading = false;
let reloadQueued = false;
let loadedNotifications = [];
let nextCursor = "";
let desktopAlertsPrimed = false;
let desktopAlertsPrimedAt = 0;
const desktopAlertVersions = new Map();

// Raises one native alert per newly arrived or newly firing notification. The
// first load after launch only primes the seen set, so opening the client does
// not replay the backlog as alerts.
function announceDesktopAlerts(next) {
  if (!desktopAlertsPrimed) {
    desktopAlertsPrimed = true;
    desktopAlertsPrimedAt = Date.now();
    for (const value of next) desktopAlertVersions.set(value.id, value.updated_at);
    return;
  }
  for (const value of next.slice(0, 12)) {
    const previousVersion = desktopAlertVersions.get(value.id);
    desktopAlertVersions.set(value.id, value.updated_at);
    if (previousVersion === value.updated_at) continue;
    const updatedAt = Date.parse(value.updated_at || value.created_at || "");
    if (previousVersion === undefined && (!Number.isFinite(updatedAt) || updatedAt < desktopAlertsPrimedAt)) continue;
    if (previousVersion !== undefined && value.state !== "firing" && value.state !== "resolved") continue;
    const preference = channelCache.find(channel => channel.id === value.channel_id)?.notification_level || "all";
    if (preference === "muted") continue;
    if (preference === "critical" && value.card?.severity !== "critical") continue;
    const presentation = notificationAlertPresentation(value);
    desktopShell.alert({
      id: value.id,
      title: presentation.subject ? compactAlertText(`${presentation.subject} · ${value.channel_name || "Tintwire"}`, 100) : (value.channel_name || "Tintwire"),
      body: presentation.body || notificationLabel(value),
      urgent: value.state === "firing",
    });
  }
}

function syncDesktopAlertVersions(next) {
  if (!desktopAlertsPrimed) {
    desktopAlertsPrimed = true;
    desktopAlertsPrimedAt = Date.now();
  }
  for (const value of next) desktopAlertVersions.set(value.id, value.updated_at);
}

async function loadNotifications(append = false, announce = false, pinTimelineToBottom = false) {
  if (selectedChannel) {
    try {
      await loadChannelTimeline(append, pinTimelineToBottom);
    } catch (error) {
      list.replaceChildren(element("div", "error", `Unable to load timeline: ${error.message}`));
    }
    await loadChannels();
    return;
  }
  if (loading) {
    reloadQueued = true;
    return;
  }
  loading = true;
  try {
    const parameters = new URLSearchParams(new FormData(inboxFilters));
    for (const [key, value] of [...parameters]) if (!value) parameters.delete(key);
    if (selectedChannels.length) { parameters.delete("channel"); parameters.set("channels", selectedChannels.join(",")); }
    if (append && nextCursor) parameters.set("before", nextCursor);
    const response = await fetch(`/api/v1/notifications?${parameters}`);
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const data = await response.json();
    if (desktopShell && !append) {
      if (announce) announceDesktopAlerts(data.notifications || []);
      else syncDesktopAlertVersions(data.notifications || []);
    }
    loadedNotifications = append ? loadedNotifications.concat(data.notifications || []) : (data.notifications || []);
    nextCursor = data.next_cursor || "";
    latestUnreadCount = Number(data.unread_count || 0);
    updateReadControl();
    desktopShell?.setUnread(latestUnreadCount);
    if ("setAppBadge" in navigator) {
      if (latestUnreadCount) navigator.setAppBadge(latestUnreadCount).catch(() => {});
      else if ("clearAppBadge" in navigator) navigator.clearAppBadge().catch(() => {});
    }
    loadMoreButton.hidden = !nextCursor;
    loadMoreButton.disabled = false;
    render(loadedNotifications);
  } catch (error) {
    list.replaceChildren(element("div", "error", `Unable to load notifications: ${error.message}`));
  } finally {
    loading = false;
    if (reloadQueued) {
      reloadQueued = false;
      loadNotifications();
    }
  }
}

// Renders a single channel message or reply as a timeline entry. Replies are
// visually indented and carry a reply affordance so the composer can be primed
// to attach to the threaded parent.
function messageNode(message, isReply = false, heldUntil = 0) {
  const node = element("article", isReply ? "timeline-message timeline-reply" : "timeline-message");
  node.id = `message-${message.id}`;
  const meta = element("div", "meta");
  meta.append(element("span", "meta-source", message.author || message.author_user_id));
  if (Number(message.reply_count || 0) > 0) {
    meta.append(element("span", "state-badge agent-badge", `${message.reply_count} reply${message.reply_count === 1 ? "" : "s"}`));
  }
  meta.append(element("time", "meta-time", new Date(message.created_at).toLocaleString()));
  const replyButton = element("button", "reply-button", "Reply");
  replyButton.type = "button";
  replyButton.setAttribute("aria-label", `Reply to ${message.author}`);
  replyButton.addEventListener("click", () => beginReply(message));
  const linkButton = element("button", "message-link-button", "Copy link");
  linkButton.type = "button";
  linkButton.setAttribute("aria-label", `Copy link to message from ${message.author}`);
  linkButton.addEventListener("click", async () => {
    const link = new URL(location.pathname, location.origin);
    link.searchParams.set("message", message.id);
    try {
      await navigator.clipboard.writeText(link.href);
      linkButton.textContent = "Copied";
      setTimeout(() => { linkButton.textContent = "Copy link"; }, 1600);
    } catch (_) {
      window.prompt("Copy message link", link.href);
    }
  });
  meta.append(replyButton, linkButton);
  node.append(meta, richContent("text", message.text));
  if (heldUntil) {
    const notice = element("div", "sent-message-hold");
    const history = element("button", "sent-message-history", "View history");
    history.type = "button";
    history.addEventListener("click", async () => {
      readFilter.value = "";
      await loadChannelTimeline(false);
    });
    notice.append(element("span", "sent-message-hold-copy"), history);
    notice.dataset.heldUntil = String(heldUntil);
    node.classList.add("timeline-message-held");
    node.append(notice);
  }
  return node;
}

function updateHeldMessageNotices() {
  const now = Date.now();
  let expired = false;
  for (const [id, held] of heldSentMessages) {
    if (held.expiresAt <= now) {
      heldSentMessages.delete(id);
      expired = true;
    }
  }
  if (expired) {
    loadedTimelineItems = loadedTimelineItems.filter(item => !item.held_until || item.held_until > now);
    if (selectedChannel) renderChannelTimeline(loadedTimelineItems);
  }
  for (const notice of document.querySelectorAll(".sent-message-hold[data-held-until]")) {
    const seconds = Math.max(0, Math.ceil((Number(notice.dataset.heldUntil) - now) / 1000));
    const copy = notice.querySelector(".sent-message-hold-copy");
    if (copy) copy.textContent = `Sent · Leaving Unread only in ${seconds}s · `;
  }
}

setInterval(updateHeldMessageNotices, 250);

let renderedTimelineNodes = new Map();

function renderChannelTimeline(items) {
  if (!items.length) {
    const copy = readFilter.value === "1"
      ? "No unread messages or notifications in this channel."
      : "No messages or notifications in this channel yet. Send the first message below.";
    renderedTimelineNodes = new Map();
    reconcileListChildren([element("div", "empty", copy)]);
    return;
  }
  const topLevel = [];
  const repliesByRoot = new Map();
  const loadedRoots = new Set(items
    .filter(item => item.kind === "message" && item.message && !item.message.parent_id)
    .map(item => item.message.root_id || item.message.id));
  // The API is newest-first for stable cursor pagination. Reverse the loaded
  // page so channels read like Mattermost, with the newest entry at the bottom.
  for (const item of [...items].reverse()) {
    if (item.kind === "notification" && item.notification) {
      topLevel.push({type: "notification", value: item.notification});
    } else if (item.kind === "command" && item.command) {
      topLevel.push({type: "command", value: item.command});
    } else if (item.kind === "message" && item.message) {
      const message = item.message;
      if (message.parent_id) {
        if (loadedRoots.has(message.root_id)) {
          const list = repliesByRoot.get(message.root_id) || [];
          list.push({message, heldUntil: item.held_until || 0});
          repliesByRoot.set(message.root_id, list);
        } else {
          // Keep a reply in chronological position when its root is outside
          // this page. Appending all such replies after the loop made old
          // messages appear newer than every notification in the channel.
          topLevel.push({type: "reply", value: message, heldUntil: item.held_until || 0});
        }
      } else {
        topLevel.push({type: "message", value: message, root: message.root_id || message.id, heldUntil: item.held_until || 0});
      }
    }
  }
  const next = new Map();
  const nodes = [];
  const add = (key, value, build) => {
    nodes.push(stableRenderedNode(renderedTimelineNodes, next, key, value, build));
  };
  for (const entry of topLevel) {
    if (entry.type === "notification") {
      add(`notification:${entry.value.id}`, entry.value, () => notificationCardNode(entry.value));
      continue;
    }
    if (entry.type === "command") {
      add(`command:${entry.value.id}`, entry.value, () => commandNode(entry.value));
      continue;
    }
    if (entry.type === "reply") {
      add(`reply:${entry.value.id}`, {value: entry.value, heldUntil: entry.heldUntil}, () =>
        messageNode(entry.value, true, entry.heldUntil));
      continue;
    }
    add(`message:${entry.value.id}`, {value: entry.value, heldUntil: entry.heldUntil}, () =>
      messageNode(entry.value, false, entry.heldUntil));
    const replies = repliesByRoot.get(entry.root) || [];
    if (replies.length) {
      add(`thread:${entry.root}`, replies, () => {
        const thread = element("div", "thread");
        for (const reply of replies) thread.append(messageNode(reply.message, true, reply.heldUntil));
        return thread;
      });
    }
  }
  reconcileListChildren(nodes);
  renderedTimelineNodes = next;
}

// Renders a slash-command response as a timeline entry with command attribution
// and, for ephemeral output, an invoker-only marker.
function commandNode(command) {
  const node = element("article", "timeline-message timeline-command");
  node.id = `command-${command.id}`;
  const meta = element("div", "meta");
  const displayName = command.username || command.author || "command";
  const avatar = remoteImage("message-avatar", command.icon_url, `${displayName} avatar`);
  if (avatar) meta.append(avatar);
  meta.append(element("span", "meta-source", displayName));
  if (command.response_type) meta.append(element("span", "state-badge agent-badge", command.response_type));
  if (command.invoker) meta.append(element("span", "meta-time", `by ${command.invoker}`));
  meta.append(element("time", "meta-time", new Date(command.created_at).toLocaleString()));
  node.append(meta);
  if (command.text) node.append(richContent("text", command.text));
  if (Array.isArray(command.attachments)) {
    for (const attachment of command.attachments) node.append(commandAttachment(attachment));
  }
  return node;
}

// Renders a slash-command response attachment read-only: content, fields, and
// images are preserved, but interactive actions are not offered because the
// command-output path has no server-side action callback binding.
function commandAttachment(attachment) {
  const card = element("div", "attachment");
  applyAttachmentColor(card, attachment);
  if (attachment.pretext) card.append(richContent("attachment-pretext", attachment.pretext));
  if (attachment.author_name) card.append(element("div", "attachment-author", attachment.author_name));
  if (attachment.title) {
    const titleURL = safeHTTPURL(attachment.title_link);
    const title = titleURL ? element("a", "attachment-title", attachment.title) : element("div", "attachment-title", attachment.title);
    if (titleURL) { title.href = titleURL; title.target = "_blank"; title.rel = "noopener noreferrer"; }
    card.append(title);
  }
  if (attachment.text) card.append(richContent("attachment-text", attachment.text));
  if (Array.isArray(attachment.fields)) {
    const fields = element("div", "field-grid");
    for (const field of attachment.fields) {
      const item = element("div", "field");
      item.append(element("b", "", field.title || ""));
      item.append(richContent("field-value", field.value || ""));
      fields.append(item);
    }
    card.append(fields);
  }
  const image = remoteImage("attachment-image", attachment.image_url, attachment.title || attachment.fallback || "Attachment image");
  if (image) card.append(image);
  const thumbnail = attachment.thumb_url !== attachment.image_url
    ? remoteImage("attachment-thumbnail", attachment.thumb_url, attachment.title || attachment.fallback || "Attachment thumbnail")
    : null;
  if (thumbnail) card.append(thumbnail);
  if (attachment.footer) card.append(element("div", "attachment-footer", attachment.footer));
  return card;
}

let loadedTimelineItems = [];
let stopTimelineScrollAnchor = () => {};

function isTimelineNearBottom() {
  return list.scrollHeight - list.scrollTop - list.clientHeight < 180;
}

function captureTimelineScrollAnchor() {
  const listTop = list.getBoundingClientRect().top;
  const entries = list.querySelectorAll('[id^="notification-"], [id^="message-"], [id^="command-"]');
  for (const entry of entries) {
    const bounds = entry.getBoundingClientRect();
    if (bounds.bottom > listTop) return {id: entry.id, offset: bounds.top - listTop};
  }
  return null;
}

function restoreTimelineScrollAnchor(anchor) {
  if (!anchor) return;
  const entry = document.getElementById(anchor.id);
  if (!entry || !list.contains(entry)) return;
  const offset = entry.getBoundingClientRect().top - list.getBoundingClientRect().top;
  list.scrollTop += offset - anchor.offset;
}

// Rich previews can change the timeline height after render as remote images
// load. Temporarily repeat a requested scroll position while that media
// settles, but stop immediately if the reader starts navigating the list.
function maintainTimelineScrollPosition(scroll) {
  stopTimelineScrollAnchor();
  let active = true;
  let timeout = 0;
  let expectedScrollTop = null;
  const stop = () => {
    if (!active) return;
    active = false;
    clearTimeout(timeout);
    observer?.disconnect();
    list.removeEventListener("scroll", handleScroll);
    for (const event of ["wheel", "touchstart", "pointerdown", "keydown"]) {
      list.removeEventListener(event, stop);
    }
  };
  stopTimelineScrollAnchor = stop;
  const handleScroll = () => {
    if (!active) return;
    if (expectedScrollTop !== null && Math.abs(list.scrollTop - expectedScrollTop) < 1) {
      expectedScrollTop = null;
      return;
    }
    stop();
  };
  const restore = () => {
    if (!active) return;
    scroll();
    expectedScrollTop = list.scrollTop;
  };
  const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(restore);
  for (const child of list.children) observer?.observe(child);
  list.addEventListener("scroll", handleScroll, {passive: true});
  for (const event of ["wheel", "touchstart", "pointerdown", "keydown"]) {
    list.addEventListener(event, stop, {once: true});
  }
  for (const image of list.querySelectorAll("img")) {
    if (!image.complete) {
      image.addEventListener("load", restore, {once: true});
      image.addEventListener("error", restore, {once: true});
    }
  }
  restore();
  requestAnimationFrame(restore);
  timeout = setTimeout(stop, 10000);
}

function anchorTimelineToBottom() {
  maintainTimelineScrollPosition(() => { list.scrollTop = list.scrollHeight; });
}

function anchorTimelineToEntry(anchor) {
  maintainTimelineScrollPosition(() => restoreTimelineScrollAnchor(anchor));
}

async function loadChannelTimeline(append = false, pinToBottom = false) {
  const channel = channelCache.find(candidate => candidate.name === selectedChannel);
  if (!channel) {
    setViewForChannel("");
    return;
  }
  currentChannelID = channel.id;
  const parameters = new URLSearchParams();
  const search = inboxSearch.value.trim();
  if (search) parameters.set("q", search);
  if (stateFilter.value) parameters.set("state", stateFilter.value);
  if (readFilter.value === "1") parameters.set("unread", "1");
  if (append && timelineNextCursor) parameters.set("before", timelineNextCursor);
  const suffix = parameters.size ? `?${parameters}` : "";
  const initialLoad = !append && loadedTimelineItems.length === 0;
  const response = await fetch(`/api/v1/channels/${encodeURIComponent(channel.id)}/timeline${suffix}`);
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const data = await response.json();
  timelineNextCursor = data.next_cursor || "";
  const responseItems = data.items || [];
  if (!append && readFilter.value === "1") {
    const now = Date.now();
    const responseIDs = new Set(responseItems.map(item => item.id));
    for (const [id, held] of heldSentMessages) {
      if (held.channelID !== channel.id || held.expiresAt <= now || responseIDs.has(id)) continue;
      responseItems.unshift(held.item);
    }
  }
  // Polling usually returns byte-for-byte equivalent timeline data. Keep the
  // existing DOM in that case so a no-op refresh cannot disturb scrolling,
  // focus, selection, or in-progress interaction with a card.
  const timelineUnchanged = !append && !initialLoad &&
    JSON.stringify(responseItems) === JSON.stringify(loadedTimelineItems);
  if (timelineUnchanged) {
    loadMoreButton.hidden = !timelineNextCursor;
    loadMoreButton.disabled = false;
    return;
  }
  loadedTimelineItems = append ? loadedTimelineItems.concat(responseItems) : responseItems;
  loadMoreButton.hidden = !timelineNextCursor;
  loadMoreButton.disabled = false;
  // Measure immediately before replacing the entries. A reader can move while
  // the request is in flight, and that latest position determines whether a
  // refresh should remain pinned.
  const wasNearBottom = pinToBottom || isTimelineNearBottom();
  const scrollAnchor = !append && !initialLoad && !wasNearBottom
    ? captureTimelineScrollAnchor()
    : null;
  const previousHeight = list.scrollHeight;
  renderChannelTimeline(loadedTimelineItems);
  if (append) {
    // Reversing the expanded API page inserts older entries above the current
    // conversation. Preserve the reader's position after that prepend.
    list.scrollTop += list.scrollHeight - previousHeight;
  } else if (initialLoad) {
    // Open the bounded list at the newest entry. This moves only the timeline,
    // so the channel sidebar remains visible. Keep anchoring briefly while
    // lazy-loaded rich-preview images establish their final dimensions.
    anchorTimelineToBottom();
  } else if (wasNearBottom) {
    // A refresh recreates rich-preview images. Their later load events can
    // otherwise increase scrollHeight and leave the reader above the newest
    // entry even though they were at the bottom before the refresh.
    anchorTimelineToBottom();
  } else if (scrollAnchor) {
    // Re-rendering replaces every card. Preserve the first visible entry and
    // its viewport offset so periodic refreshes do not move the reader.
    anchorTimelineToEntry(scrollAnchor);
  }
}

// Toggles between the global notification feed (all channels) and the selected
// channel's merged timeline with its composer.
function setViewForChannel(name) {
  if (name) {
    inbox.classList.add("channel-timeline-view");
    filterConsole.hidden = false;
    filterConsole.open = true;
    inboxFilters.classList.add("timeline-search");
    inboxSearch.placeholder = `Search ${name} timeline…`;
    composer.hidden = false;
    composerInput.placeholder = `Message ${name}`;
    composerSubmit.before(readButton);
    list.before(loadMoreButton);
    loadMoreButton.hidden = !timelineNextCursor;
  } else {
    inbox.classList.remove("channel-timeline-view");
    inboxFilters.classList.remove("timeline-search");
    inboxSearch.placeholder = "Search notifications…";
    filterConsole.hidden = false;
    composer.hidden = true;
    topbarActions.prepend(readButton);
    list.after(loadMoreButton);
    loadMoreButton.hidden = !nextCursor;
  }
}

// Refreshes the correct view for the current selection and channel counts.
async function refreshInboxView(announce = false) {
  try {
    if (selectedChannel) await loadChannelTimeline(false);
    else await loadNotifications(false, announce);
  } catch (error) {
    list.replaceChildren(element("div", "error", `Unable to load view: ${error.message}`));
  }
  await loadChannels();
}

function beginReply(message) {
  replyingTo = message;
  composerReplyLabel.textContent = `Replying to ${message.author}: ${message.text.slice(0, 60)}`;
  composerReply.hidden = false;
  composerInput.focus();
}

function clearReply() {
  replyingTo = null;
  composerReply.hidden = true;
  composerReplyLabel.textContent = "";
}

composerCancelReply.addEventListener("click", clearReply);

composerInput.addEventListener("keydown", event => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    composer.requestSubmit();
  }
});

function commandIdempotencyKey() {
  return "cmd-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 12);
}

composer.addEventListener("submit", async event => {
  event.preventDefault();
  const text = composerInput.value.trim();
  if (!text || !currentChannelID) return;
  composerStatus.textContent = "";
  composerSubmit.disabled = true;
  const isCommand = text.startsWith("/");
  try {
    const endpoint = isCommand
      ? `/api/v1/channels/${encodeURIComponent(currentChannelID)}/commands`
      : `/api/v1/channels/${encodeURIComponent(currentChannelID)}/messages`;
    const headers = {"Content-Type": "application/json"};
    if (isCommand) headers["Idempotency-Key"] = commandIdempotencyKey();
    const body = isCommand
      ? JSON.stringify({text})
      : JSON.stringify({text, parent_id: replyingTo?.id || ""});
    const response = await fetch(endpoint, {method: "POST", headers, body});
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const created = await response.json();
    composerInput.value = "";
    clearReply();
    if (!isCommand) {
      const heldUntil = readFilter.value === "1" ? Date.now() + sentMessageHoldMilliseconds : 0;
      const item = {kind: "message", message: created, id: created.id, created_at: new Date(created.created_at).getTime()};
      if (heldUntil) {
        item.held_until = heldUntil;
        heldSentMessages.set(created.id, {item, channelID: currentChannelID, expiresAt: heldUntil});
      }
      loadedTimelineItems = [
        item,
        ...loadedTimelineItems.filter(item => item.id !== created.id),
      ];
      renderChannelTimeline(loadedTimelineItems);
      updateHeldMessageNotices();
      list.scrollTo({top: list.scrollHeight, behavior: "smooth"});
      await loadChannels();
    } else {
      await refreshInboxView();
    }
  } catch (error) {
    composerStatus.textContent = `Unable to send: ${error.message}`;
  } finally {
    composerSubmit.disabled = false;
  }
});

let channelCache = [];
let selectedChannel = new URLSearchParams(location.search).get("channel") || "";
let selectedChannels = (new URLSearchParams(location.search).get("channels") || "").split(",").filter(Boolean);
let savedViewCache = [];
let editingChannelID = "";

function channelButton(channel) {
  const row = element("div", "channel-row");
  if (!channel.name) row.classList.add("channel-row-all");
  const button = element("button", "channel-item");
  button.type = "button";
  button.dataset.channel = channel.name;
  button.title = `${channel.display_name}: ${channel.total_count} notifications, ${channel.unread_count} unread, ${channel.firing_count} firing`;
  if (channel.name === selectedChannel) {
    button.classList.add("active");
    button.setAttribute("aria-current", "page");
  }
  const dot = element("span", "channel-dot");
  if (!Number(channel.unread_count || 0)) {
    dot.classList.add("channel-dot-idle");
  }
  if (/^#[0-9a-f]{6}$/i.test(channel.accent_color || "")) {
    dot.style.setProperty("--channel-accent", channel.accent_color);
  }
  button.append(dot, element("span", "channel-name", channel.display_name));
  const counts = element("span", "channel-counts");
  if (channel.unread_count) counts.append(element("span", "channel-unread", `${channel.unread_count} unread`));
  else counts.append(element("span", "channel-count", `${channel.total_count} total`));
  button.append(counts);
  button.addEventListener("click", () => selectChannel(channel.name));
  row.append(button);
  if (channel.firing_count) {
    const firingButton = element("button", "channel-firing", `${channel.firing_count} firing`);
    firingButton.type = "button";
    firingButton.title = `View ${channel.firing_count} firing notifications in ${channel.display_name}`;
    firingButton.setAttribute("aria-label", firingButton.title);
    firingButton.addEventListener("click", () => openFiringView(channel.name));
    row.append(firingButton);
  }
  if (channel.name && inboxStateEnabled) {
    const muted = channel.notification_level === "muted";
    const muteButton = element("button", "channel-notification-button", muted ? "Muted" : "Mute");
    muteButton.type = "button";
    muteButton.title = `${muted ? "Enable" : "Mute"} notifications for ${channel.display_name || channel.name}`;
    muteButton.setAttribute("aria-label", muteButton.title);
    muteButton.setAttribute("aria-pressed", String(muted));
    muteButton.addEventListener("click", async () => {
      muteButton.disabled = true;
      try {
        await setChannelNotificationLevel(channel.id, muted ? "all" : "muted", false);
        showInboxToast(`${channel.display_name || channel.name} notifications ${muted ? "enabled" : "muted"}.`);
      } catch (error) {
        muteButton.disabled = false;
        showInboxToast(`Unable to update channel notifications: ${error.message}`);
      }
    });
    row.append(muteButton);
  }
  return row;
}

function renderChannelNavigation(channels) {
  const all = channels.filter(channel => channel.notification_level !== "muted").reduce((summary, channel) => ({
    name: "", display_name: "All notifications", accent_color: "#67e8f9",
    total_count: summary.total_count + Number(channel.total_count || 0),
    unread_count: summary.unread_count + Number(channel.unread_count || 0),
    firing_count: summary.firing_count + Number(channel.firing_count || 0)
  }), {name: "", display_name: "All notifications", accent_color: "#67e8f9", total_count: 0, unread_count: 0, firing_count: 0});
  const entries = [all, ...channels];
  channelList.replaceChildren(...entries.map(channelButton));
  mobileChannelList.replaceChildren(...entries.map(channelButton));
  const selected = entries.find(channel => channel.name === selectedChannel) || all;
  channelEditButton.hidden = !isAdmin || !selected.name;
  mobileChannelEditButton.hidden = !isAdmin || !selected.name;
  const activeView = savedViewCache.find(view => view.id === new URLSearchParams(location.search).get("view"));
  feedTitle.textContent = activeView ? activeView.name : selected.display_name;
  const unread = all.unread_count ? ` · ${all.unread_count} unread` : "";
  mobileChannelToggle.textContent = `${selected.name ? selected.display_name : "Channels"}${unread}`;
}

function writeFilterURL(method) {
  const parameters = new URLSearchParams(new FormData(inboxFilters));
  for (const [key, value] of [...parameters]) if (!value) parameters.delete(key);
  history[method](null, "", parameters.size ? `?${parameters}` : location.pathname);
}

function updatePrimaryNavigation(preferred = "") {
  const filtered = Boolean(selectedChannel || selectedChannels.length || inboxSearch.value.trim() || stateFilter.value || severityFilter.value);
  const mode = preferred || (filtered ? "filters" : (readFilter.value === "1" ? "inbox" : "activity"));
  inboxNav.classList.toggle("active", mode === "inbox");
  filtersNav.classList.toggle("active", mode === "filters");
  activityNav.classList.toggle("active", mode === "activity");
}

function showPrimaryFeed(unreadOnly) {
  selectedChannel = "";
  selectedChannels = [];
  inboxSearch.value = "";
  channelFilter.value = "";
  stateFilter.value = "";
  severityFilter.value = "";
  readFilter.value = unreadOnly ? "1" : "";
  timelineNextCursor = "";
  renderChannelNavigation(channelCache);
  setViewForChannel("");
  updatePrimaryNavigation(unreadOnly ? "inbox" : "activity");
  writeFilterURL("pushState");
  loadNotifications(false);
  document.querySelector("#inbox").scrollIntoView({block: "start"});
}

function selectChannel(name) {
  if (name === selectedChannel) return;
  selectedChannel = name;
  selectedChannels = [];
  if (name) readFilter.value = "1";
  timelineNextCursor = "";
  loadedTimelineItems = [];
  channelFilter.value = name;
  renderChannelNavigation(channelCache);
  updatePrimaryNavigation(name ? "filters" : "");
  setViewForChannel(name);
  writeFilterURL("pushState");
  loadNotifications(false);
  if (channelDialog.open) channelDialog.close();
  document.querySelector("#inbox").scrollIntoView({block: "start"});
}

function openFiringView(name) {
  selectedChannel = name;
  inboxSearch.value = "";
  channelFilter.value = name;
  stateFilter.value = "firing";
  severityFilter.value = "";
  readFilter.value = "";
  timelineNextCursor = "";
  loadedTimelineItems = [];
  renderChannelNavigation(channelCache);
  setViewForChannel(name);
  updatePrimaryNavigation("filters");
  writeFilterURL("pushState");
  loadNotifications(false);
  if (channelDialog.open) channelDialog.close();
  document.querySelector("#inbox").scrollIntoView({block: "start"});
}

async function loadChannels() {
  try {
    const response = await fetch("/api/v1/channels");
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const data = await response.json();
    channelCache = data.channels || [];
    latestUnreadCount = channelCache.reduce((total, channel) => (
      total + (channel.notification_level === "muted" ? 0 : Number(channel.unread_count || 0))
    ), 0);
    desktopShell?.setUnread(latestUnreadCount);
    if ("setAppBadge" in navigator) {
      if (latestUnreadCount) navigator.setAppBadge(latestUnreadCount).catch(() => {});
      else if ("clearAppBadge" in navigator) navigator.clearAppBadge().catch(() => {});
    }
    if (selectedChannel && !channelCache.some(channel => channel.name === selectedChannel)) selectedChannel = "";
    const options = [element("option", "", "All channels")];
    options[0].value = "";
    for (const channel of channelCache) {
      const unread = channel.unread_count ? ` · ${channel.unread_count} unread` : "";
      const option = element("option", "", `${channel.display_name}${unread}`);
      option.value = channel.name;
      options.push(option);
    }
    channelFilter.replaceChildren(...options);
    channelFilter.value = selectedChannel;
    const alertOptions = channelCache.map(channel => {
      const option = element("option", "", channel.display_name || channel.name);
      option.value = channel.id;
      return option;
    });
    alertChannel.replaceChildren(...alertOptions);
    const preferred = channelCache.find(channel => channel.name === selectedChannel) || channelCache[0];
    if (preferred) alertChannel.value = preferred.id;
    renderChannelNavigation(channelCache);
    setViewForChannel(selectedChannel);
    updatePrimaryNavigation();
    updateReadControl();
  } catch (error) {
    const message = element("span", "channel-loading channel-error", `Channels unavailable · ${error.message}`);
    channelList.replaceChildren(message);
    mobileChannelList.replaceChildren(element("span", "channel-loading channel-error", "Channels unavailable"));
  }
}

function applySavedView(view) {
  selectedChannel = "";
  selectedChannels = [...view.channels];
  inboxSearch.value = view.q || "";
  channelFilter.value = "";
  stateFilter.value = view.state || "";
  severityFilter.value = view.severity || "";
  readFilter.value = view.unread ? "1" : "";
  feedTitle.textContent = view.name;
  const parameters = new URLSearchParams(new FormData(inboxFilters));
  parameters.delete("channel");
  parameters.set("channels", selectedChannels.join(","));
  parameters.set("view", view.id);
  for (const [key, value] of [...parameters]) if (!value) parameters.delete(key);
  history.pushState(null, "", `?${parameters}`);
  renderSavedViews();
  updatePrimaryNavigation("filters");
  loadNotifications(false);
}

function renderSavedViews() {
  const activeID = new URLSearchParams(location.search).get("view") || "";
  if (!savedViewCache.length) { savedViewList.replaceChildren(element("span", "channel-loading", "No saved views")); return; }
  savedViewList.replaceChildren(...savedViewCache.map(view => {
    const row = element("div", "channel-row");
    const button = element("button", `channel-item${view.id === activeID ? " active" : ""}`, view.name);
    button.type = "button";
    button.title = view.channels.join(", ");
    button.addEventListener("click", () => applySavedView(view));
    const remove = element("button", "channel-mute-button", "×");
    remove.type = "button";
    remove.title = `Delete ${view.name}`;
    remove.addEventListener("click", async () => {
      if (!confirm(`Delete the saved view “${view.name}”?`)) return;
      const response = await fetch(`/api/v1/saved-views/${encodeURIComponent(view.id)}`, {method:"DELETE"});
      if (response.ok) await loadSavedViews();
    });
    row.append(button, remove);
    return row;
  }));
}

async function loadSavedViews() {
  const response = await fetch("/api/v1/saved-views");
  if (!response.ok) { savedViewList.replaceChildren(element("span", "channel-loading", "Views unavailable")); return; }
  savedViewCache = (await response.json()).views || [];
  renderSavedViews();
  const requested = new URLSearchParams(location.search).get("view");
  const view = savedViewCache.find(candidate => candidate.id === requested);
  if (view) { selectedChannels = [...view.channels]; feedTitle.textContent = view.name; }
}

savedViewAdd.addEventListener("click", () => {
  const suggested = selectedChannels.length ? selectedChannels : channelCache.filter(channel => /mastodon|bluesky/i.test(`${channel.name} ${channel.display_name}`)).map(channel => channel.name);
  savedViewName.value = "News";
  savedViewStatus.textContent = "";
  savedViewChannels.replaceChildren(...channelCache.map(channel => {
    const option = element("option", "", channel.display_name || channel.name);
    option.value = channel.name;
    option.selected = suggested.includes(channel.name);
    return option;
  }));
  savedViewDialog.showModal();
  savedViewName.focus();
  savedViewName.select();
});
savedViewClose.addEventListener("click", () => savedViewDialog.close());
savedViewDialog.addEventListener("click", event => { if (event.target === savedViewDialog) savedViewDialog.close(); });
savedViewForm.addEventListener("submit", async event => {
  event.preventDefault();
  const name = savedViewName.value.trim();
  const channels = [...savedViewChannels.selectedOptions].map(option => option.value);
  if (!channels.length) { savedViewStatus.textContent = "Choose at least one channel."; return; }
  const response = await fetch("/api/v1/saved-views", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({name, channels, q:inboxSearch.value.trim(), state:stateFilter.value, severity:severityFilter.value, unread:false})});
  if (!response.ok) { savedViewStatus.textContent = (await response.text()).trim() || "Unable to save view"; return; }
  const view = await response.json();
  savedViewDialog.close();
  await loadSavedViews();
  applySavedView(view);
});

mobileChannelToggle.addEventListener("click", () => channelDialog.showModal());
channelDialogClose.addEventListener("click", () => channelDialog.close());
channelDialog.addEventListener("click", event => {
  if (event.target === channelDialog) channelDialog.close();
});
channelCreateButton.addEventListener("click", () => {
  editingChannelID = "";
  createChannelForm.reset();
  createChannelName.disabled = false;
  createChannelAccent.value = "#2f80ff";
  createChannelSubmit.textContent = "Create";
  document.querySelector("#create-channel-dialog-title").textContent = "Create channel";
  createChannelStatus.textContent = "";
  createChannelDialog.showModal();
  createChannelName.focus();
});
function openChannelEditor() {
  const channel = channelCache.find(candidate => candidate.name === selectedChannel);
  if (!channel) return;
  editingChannelID = channel.id;
  createChannelName.value = channel.name;
  createChannelName.disabled = true;
  createChannelDisplay.value = channel.display_name || channel.name;
  createChannelDescription.value = channel.description || "";
  createChannelAccent.value = channel.accent_color || "#2f80ff";
  createChannelVisibility.value = channel.visibility || "public";
  createChannelSubmit.textContent = "Save";
  document.querySelector("#create-channel-dialog-title").textContent = "Edit channel";
  createChannelStatus.textContent = "";
  createChannelDialog.showModal();
  createChannelDescription.focus();
}
channelEditButton.addEventListener("click", openChannelEditor);
mobileChannelEditButton.addEventListener("click", () => {
  channelDialog.close();
  openChannelEditor();
});
createChannelDialogClose.addEventListener("click", () => createChannelDialog.close());
createChannelDialog.addEventListener("click", event => {
  if (event.target === createChannelDialog) createChannelDialog.close();
});
createChannelForm.addEventListener("submit", async event => {
  event.preventDefault();
  const submit = createChannelForm.querySelector("button[type=submit]");
  submit.disabled = true;
  createChannelStatus.textContent = editingChannelID ? "Saving channel…" : "Creating channel…";
  try {
    const endpoint = editingChannelID ? `/api/v1/channels/${encodeURIComponent(editingChannelID)}` : "/api/v1/channels";
    const response = await fetch(endpoint, {
      method: editingChannelID ? "PUT" : "POST",
      headers: {"Content-Type":"application/json"},
      body: JSON.stringify({
        ...(editingChannelID ? {} : {name: createChannelName.value.trim()}),
        display_name: createChannelDisplay.value.trim() || "",
        description: createChannelDescription.value.trim() || "",
        accent_color: createChannelAccent.value,
        visibility: createChannelVisibility.value
      })
    });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const data = await response.json();
    await loadChannels();
    if (!editingChannelID && data?.channel?.name) selectChannel(data.channel.name);
    editingChannelID = "";
    createChannelForm.reset();
    createChannelName.disabled = false;
    createChannelDialog.close();
    createChannelStatus.textContent = "";
  } catch (error) {
    createChannelStatus.textContent = `Unable to ${editingChannelID ? "save" : "create"} channel: ${error.message}`;
    submit.disabled = false;
    return;
  }
  submit.disabled = false;
});

automationOpen.addEventListener("click", event => {
  event.preventDefault();
  agentStatus.textContent = "";
  webhookStatus.textContent = "";
  openAutomation();
});
automationClose.addEventListener("click", () => automationDialog.close());
automationDialog.addEventListener("click", event => {
  if (event.target === automationDialog) automationDialog.close();
});
usersOpen.addEventListener("click", event => { event.preventDefault(); usersDialog.showModal(); loadManagedUsers().catch(error => { usersStatus.textContent=error.message; }); });
usersClose.addEventListener("click", () => usersDialog.close());
usersDialog.addEventListener("click", event => { if (event.target === usersDialog) usersDialog.close(); });
credentialCopy.addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(credentialValue.textContent);
    credentialCopy.textContent = "Copied";
    setTimeout(() => { credentialCopy.textContent = "Copy"; }, 1800);
  } catch (_) {
    credentialValue.focus?.();
  }
});
agentForm.addEventListener("submit", async event => {
  event.preventDefault();
  const submit = agentForm.querySelector("button[type=submit]");
  submit.disabled = true;
  agentStatus.textContent = "Creating bot…";
  try {
    const response = await fetch("/api/v1/agents", {
      method: "POST",
      headers: {"Content-Type":"application/json"},
      body: JSON.stringify({
        name: agentName.value.trim(),
        display_name: agentDisplayName.value.trim(),
        description: agentDescription.value.trim(),
        oauth_subject: agentOAuthSubject.value.trim(),
        is_admin: agentAdmin.checked
      })
    });
    if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
    const data = await response.json();
    showCredential(`${data.agent.display_name || data.agent.name} access token`, data.access_token);
    agentForm.reset();
    agentStatus.textContent = "Bot created.";
    await loadAgents();
  } catch (error) {
    agentStatus.textContent = `Unable to create bot: ${error.message}`;
  } finally {
    submit.disabled = false;
  }
});
webhookForm.addEventListener("submit", async event => {
  event.preventDefault();
  const submit = webhookForm.querySelector("button[type=submit]");
  submit.disabled = true;
  webhookStatus.textContent = "Creating webhook…";
  try {
    const response = await fetch("/api/v1/webhooks", {
      method: "POST",
      headers: {"Content-Type":"application/json"},
      body: JSON.stringify({channel_id: webhookChannel.value, channel_locked: webhookChannelLocked.checked})
    });
    if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
    const data = await response.json();
    showCredential(`${data.webhook.channel} webhook URL`, `${location.origin}${data.path}`);
    webhookStatus.textContent = "Webhook created.";
    await loadWebhooks();
  } catch (error) {
    webhookStatus.textContent = `Unable to create webhook: ${error.message}`;
  } finally {
    submit.disabled = false;
  }
});

loadMoreButton.addEventListener("click", () => {
  loadMoreButton.disabled = true;
  loadNotifications(true);
});

const initialFilters = new URLSearchParams(location.search);
inboxSearch.value = initialFilters.get("q") || "";
stateFilter.value = initialFilters.get("state") || "";
severityFilter.value = initialFilters.get("severity") || "";
readFilter.value = initialFilters.has("notification") ? "" : (initialFilters.get("unread") || "1");
let filterTimer;
function filtersChanged() {
  clearTimeout(filterTimer);
  filterTimer = setTimeout(() => {
    if (stateFilter.value === "dismissed") readFilter.value = "";
    selectedChannel = channelFilter.value;
    selectedChannels = [];
    renderChannelNavigation(channelCache);
    updatePrimaryNavigation();
    writeFilterURL("replaceState");
    loadNotifications(false);
  }, 180);
}
inboxFilters.addEventListener("input", filtersChanged);
inboxFilters.addEventListener("change", filtersChanged);
inboxFilters.addEventListener("submit", event => { event.preventDefault(); filtersChanged(); });
window.addEventListener("popstate", () => {
  const parameters = new URLSearchParams(location.search);
  inboxSearch.value = parameters.get("q") || "";
  selectedChannel = parameters.get("channel") || "";
  selectedChannels = (parameters.get("channels") || "").split(",").filter(Boolean);
  channelFilter.value = selectedChannel;
  stateFilter.value = parameters.get("state") || "";
  severityFilter.value = parameters.get("severity") || "";
  readFilter.value = parameters.has("notification") ? "" : (parameters.get("unread") || "1");
  renderChannelNavigation(channelCache);
  setViewForChannel(selectedChannel);
  updatePrimaryNavigation();
  loadNotifications(false);
});

desktopShell?.listen?.("tintwire://mark-all-read", () => {
  if (!readButton.disabled) readButton.click();
});

// A tintwire://notification/{id} deep link lands here as ?notification={id}.
async function focusDeepLinkedNotification() {
  const requested = new URLSearchParams(location.search).get("notification");
  if (!requested) return;
  history.replaceState(null, "", location.pathname + location.hash);
  const card = document.querySelector(`#notification-${CSS.escape(requested)}`);
  if (!card) return;
  card.scrollIntoView({block: "center", behavior: "smooth"});
  card.classList.add("card-deep-linked");
  setTimeout(() => card.classList.remove("card-deep-linked"), 2400);
}

// A tintwire://message/{id} deep link lands here as ?message={id}. Resolve the
// server-authoritative channel after authentication, then load the normal
// timeline and append an older target when it is outside the newest page.
async function focusDeepLinkedMessage() {
  const requested = new URLSearchParams(location.search).get("message");
  if (!requested) return;
  const response = await fetch(`/api/v1/messages/${encodeURIComponent(requested)}`);
  if (!response.ok) return;
  const message = await response.json();
  const channel = channelCache.find(candidate => candidate.id === message.channel_id);
  if (!channel) return;
  selectedChannel = channel.name;
  channelFilter.value = channel.name;
  timelineNextCursor = "";
  loadedTimelineItems = [];
  renderChannelNavigation(channelCache);
  setViewForChannel(channel.name);
  await loadChannelTimeline(false);
  if (!loadedTimelineItems.some(item => item.kind === "message" && item.message?.id === message.id)) {
    loadedTimelineItems.push({kind: "message", message, id: message.id, created_at: new Date(message.created_at).getTime()});
    renderChannelTimeline(loadedTimelineItems);
  }
  history.replaceState(null, "", `?channel=${encodeURIComponent(channel.name)}`);
  const target = document.querySelector(`#message-${CSS.escape(requested)}`);
  if (!target) return;
  target.scrollIntoView({block: "center", behavior: "smooth"});
  target.classList.add("card-deep-linked");
  setTimeout(() => target.classList.remove("card-deep-linked"), 2400);
}

let events;
const seenChannelMessageEvents = new Set();

async function handleChannelMessageEvent(event) {
  if (!desktopShell) return;
  let id = "";
  try { id = JSON.parse(event.data).id || ""; } catch (_) { return; }
  if (!id || seenChannelMessageEvents.has(id)) return;
  seenChannelMessageEvents.add(id);
  const response = await fetch(`/api/v1/messages/${encodeURIComponent(id)}`);
  if (!response.ok) return;
  const message = await response.json();
  refreshInboxState();
  if (message.author_user_id === currentUserID) return;
  const preference = channelCache.find(channel => channel.id === message.channel_id)?.notification_level || "all";
  if (preference !== "all") return;
  const prefix = message.parent_id ? "Reply" : "Message";
  const text = `${message.author}: ${String(message.text || "").trim()}`;
  desktopShell.alert({
    id: message.id,
    title: `${prefix} · ${message.channel_name || "Tintwire"}`,
    body: text.length > 180 ? `${text.slice(0, 177)}…` : text,
    urgent: false,
  });
}

function connectEvents() {
  if (events) events.close();
  events = new EventSource("/api/v1/events");
  events.addEventListener("notification", () => refreshInboxState(true));
  events.addEventListener("channel-message", event => {
    handleChannelMessageEvent(event).catch(() => {});
  });
}

// Read and dismissed state is durable per-user server state. A control write
// can land on a different HA origin than this EventSource connection, so
// refresh visible clients periodically and whenever the user returns to them.
// The event stream still provides the immediate path when both requests share
// an origin.
let inboxRefreshPending = false;
async function refreshInboxState(announce = false) {
  if (!inboxStateEnabled || inboxRefreshPending) return;
  inboxRefreshPending = true;
  try {
    await refreshInboxView(announce === true);
  } finally {
    inboxRefreshPending = false;
  }
}

window.addEventListener("focus", refreshInboxState);
document.addEventListener("visibilitychange", refreshInboxState);
setInterval(refreshInboxState, 15000);

let pushRegistration;
let pushConfig;
let deferredInstallPrompt;

function isAppleMobile() {
  return /iPhone|iPad|iPod/i.test(navigator.userAgent) ||
    (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
}

function isStandaloneApp() {
  return matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
}

function base64URLBytes(value) {
  const padding = "=".repeat((4 - value.length % 4) % 4);
  const raw = atob((value + padding).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}

async function saveSubscription(subscription) {
  const response = await fetch("/api/v1/push/subscriptions", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify(subscription)
  });
  if (!response.ok) throw new Error(`subscription rejected (HTTP ${response.status})`);
}

async function removeSubscription(subscription) {
  const response = await fetch("/api/v1/push/subscriptions", {
    method: "DELETE",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({endpoint: subscription.endpoint})
  });
  if (!response.ok) throw new Error(`unsubscribe rejected (HTTP ${response.status})`);
}

async function createPushSubscription() {
  const subscription = await pushRegistration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: base64URLBytes(pushConfig.public_key)
  });
  await saveSubscription(subscription);
  return subscription;
}

function showPushState(enabled, status) {
  alertButton.disabled = false;
  alertButton.textContent = enabled ? "Alerts on" : "Mobile alerts";
  alertButton.dataset.enabled = enabled ? "true" : "false";
  alertSetupButton.textContent = enabled ? "Disable alerts" : "Enable alerts";
  alertSetupButton.disabled = false;
  alertSetupStatus.textContent = status;
  alertStatus.textContent = status;
}

function showPushUnavailable(status, copy) {
  alertButton.disabled = false;
  alertButton.textContent = "Mobile alerts";
  alertButton.dataset.enabled = "false";
  alertSetupButton.textContent = "Enable alerts";
  alertSetupButton.disabled = true;
  alertSetupStatus.textContent = status;
  alertStatus.textContent = status;
  alertSetupCopy.textContent = copy;
}

function updateInstallUI() {
  const needsAppleInstall = isAppleMobile() && !isStandaloneApp();
  alertIOSGuide.hidden = !needsAppleInstall;
  alertInstallButton.hidden = isAppleMobile() || !deferredInstallPrompt || isStandaloneApp();
  if (needsAppleInstall) {
    showPushUnavailable(
      "Install Tintwire before enabling alerts on this device.",
      "iPhone and iPad deliver Web Push only to Home Screen web apps."
    );
  }
}

async function initializePush() {
  try {
    const response = await fetch("/api/v1/push/config");
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    pushConfig = await response.json();
    if (!pushConfig.enabled) {
      showPushUnavailable(
        "Server setup is required before devices can subscribe.",
        "Set TINTWIRE_VAPID_CONTACT on the Tintwire server, then restart it."
      );
      updateInstallUI();
      return;
    }
    if (isAppleMobile() && !isStandaloneApp()) {
      updateInstallUI();
      return;
    }
    if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
      showPushUnavailable(
        "This browser does not support Web Push.",
        "Install Tintwire or open it in a current browser with notification support."
      );
      updateInstallUI();
      return;
    }
    const workerURL = webAssetVersion ? `/sw.js?v=${encodeURIComponent(webAssetVersion)}` : "/sw.js";
    pushRegistration = await navigator.serviceWorker.register(workerURL);
    let subscription = await pushRegistration.pushManager.getSubscription();
    let renewed = false;
    if (!subscription && Notification.permission === "granted") {
      try {
        subscription = await createPushSubscription();
        renewed = true;
      } catch (error) {
        alertSetupCopy.textContent = "Tintwire could not renew this device's expired alert subscription automatically. Try enabling alerts again.";
        showPushState(false, `Alerts need renewal: ${error.message}`);
        updateInstallUI();
        return;
      }
    }
    if (subscription) {
      if (!renewed) await saveSubscription(subscription);
      alertSetupCopy.textContent = "This device will receive background alerts and open the matching Tintwire channel when tapped.";
      showPushState(true, "Alerts are enabled on this device.");
    } else if (Notification.permission === "denied") {
      showPushUnavailable(
        "Notifications are blocked in browser settings.",
        "Allow notifications for Tintwire in this device's settings, then return here."
      );
    } else {
      alertSetupCopy.textContent = "Receive firing and resolved alerts even while Tintwire is closed. Tapping one opens the matching alert.";
      showPushState(false, "Alerts are ready to enable on this device.");
    }
    updateInstallUI();
  } catch (error) {
    showPushUnavailable(`Alerts unavailable: ${error.message}`, "Tintwire could not initialize notifications on this device.");
  }
}

async function loadChannelNotificationPreference() {
  const channel = channelCache.find(value => value.id === alertChannel.value);
  if (!channel) return;
  alertPreferenceStatus.textContent = "Loading preference…";
  const response = await fetch(`/api/v1/channels/${encodeURIComponent(channel.id)}/notification-preference`);
  if (!response.ok) {
    alertPreferenceStatus.textContent = `Unable to load preference (HTTP ${response.status}).`;
    return;
  }
  const value = await response.json();
  alertLevel.value = value.level;
  alertPreferenceStatus.textContent = `${channel.display_name || channel.name}: ${alertLevel.options[alertLevel.selectedIndex].text}.`;
}

async function saveChannelNotificationPreference() {
  const channel = channelCache.find(value => value.id === alertChannel.value);
  if (!channel) return;
  alertPreferenceSave.disabled = true;
  try {
    await setChannelNotificationLevel(channel.id, alertLevel.value, false);
    alertPreferenceStatus.textContent = `${channel.display_name || channel.name}: ${alertLevel.options[alertLevel.selectedIndex].text}.`;
  } catch (error) {
    alertPreferenceStatus.textContent = `Unable to save preference: ${error.message}`;
  } finally {
    alertPreferenceSave.disabled = false;
  }
}

async function setChannelNotificationLevel(channelID, level, refreshNotifications = true) {
  const response = await fetch(`/api/v1/channels/${encodeURIComponent(channelID)}/notification-preference`, {
    method: "PUT",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({level}),
  });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const channel = channelCache.find(value => value.id === channelID);
  if (channel) channel.notification_level = level;
  if (alertChannel.value === channelID) alertLevel.value = level;
  renderChannelNavigation(channelCache);
  if (refreshNotifications) await loadNotifications(false);
}

async function togglePush() {
  alertSetupButton.disabled = true;
  try {
    if (!pushRegistration || !pushConfig?.enabled) {
      showPushUnavailable("Alerts are not ready on this device.", "Close this setup and try again after Tintwire finishes loading.");
      return;
    }
    const current = await pushRegistration.pushManager.getSubscription();
    if (current && alertButton.dataset.enabled === "true") {
      await removeSubscription(current);
      await current.unsubscribe();
      alertSetupCopy.textContent = "Receive firing and resolved alerts even while Tintwire is closed. Tapping one opens the matching alert.";
      showPushState(false, "Alerts are disabled on this device.");
      return;
    }
    const permission = await Notification.requestPermission();
    if (permission !== "granted") {
      showPushUnavailable(
        "Notification permission was not granted.",
        "Allow notifications for Tintwire in this device's settings, then return here."
      );
      return;
    }
    if (current) await saveSubscription(current);
    else await createPushSubscription();
    alertSetupCopy.textContent = "This device will receive background alerts and open the matching Tintwire channel when tapped.";
    showPushState(true, "Alerts are enabled on this device.");
  } catch (error) {
    showPushState(false, `Unable to change alerts: ${error.message}`);
  }
}

window.addEventListener("beforeinstallprompt", event => {
  event.preventDefault();
  deferredInstallPrompt = event;
  updateInstallUI();
});

window.addEventListener("appinstalled", () => {
  deferredInstallPrompt = undefined;
  updateInstallUI();
  initializePush();
});

alertButton.addEventListener("click", () => {
  updateInstallUI();
  alertDialog.showModal();
  loadChannelNotificationPreference().catch(error => { alertPreferenceStatus.textContent = error.message; });
});
alertDialogClose.addEventListener("click", () => alertDialog.close());
alertDialog.addEventListener("click", event => {
  if (event.target === alertDialog) alertDialog.close();
});
alertSetupButton.addEventListener("click", togglePush);
alertChannel.addEventListener("change", loadChannelNotificationPreference);
alertPreferenceSave.addEventListener("click", saveChannelNotificationPreference);
alertInstallButton.addEventListener("click", async () => {
  if (!deferredInstallPrompt) return;
  await deferredInstallPrompt.prompt();
  await deferredInstallPrompt.userChoice;
  deferredInstallPrompt = undefined;
  updateInstallUI();
});

async function initializeSession(desktopAuthExchanged = false) {
  try {
    if (desktopShell && window.location.hash.startsWith("#desktop-auth=")) {
      const code = window.location.hash.slice("#desktop-auth=".length);
      history.replaceState(null, "", window.location.pathname + window.location.search);
      const exchange = await fetch("/api/v1/auth/desktop/session", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({code})});
      if (!exchange.ok) throw new Error(`Desktop sign-in failed (HTTP ${exchange.status}).`);
      desktopAuthExchanged = true;
    }
    let response = await fetch("/api/v1/session");
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    let session = await response.json();
    for (let attempt = 0; desktopAuthExchanged && session.auth_required && !session.authenticated && attempt < 20; attempt += 1) {
      await new Promise(resolve => setTimeout(resolve, 250));
      response = await fetch("/api/v1/session");
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      session = await response.json();
    }
    inboxStateEnabled = Boolean(session.authenticated);
    isAdmin = Boolean(session.is_admin);
    currentUserID = session.user_id || "";
    sessionIdentity.textContent = session.username || "";
    sessionIdentity.hidden = !session.authenticated || !session.username;
    oidcLoginButton.hidden = !session.oidc_enabled;
    loginDivider.hidden = !session.oidc_enabled;
    if (session.auth_required && !session.authenticated) {
      loginOverlay.hidden = false;
      logoutButton.hidden = true;
      sessionIdentity.hidden = true;
      channelCreateButton.hidden = true;
      automationOpen.hidden = true;
      usersOpen.hidden = true;
      document.querySelector("#login-username").focus();
      return;
    }
    loginOverlay.hidden = true;
    channelCreateButton.hidden = !(session.auth_required && isAdmin);
    automationOpen.hidden = !isAdmin;
    usersOpen.hidden = !isAdmin;
    logoutButton.hidden = !session.auth_required;
    readButton.hidden = !inboxStateEnabled;
    await loadChannels();
    await loadSavedViews();
    await loadNotifications(false);
    connectEvents();
    // The desktop shell delivers native alerts from its resident window, so the
    // Web Push enrollment path is not offered there.
    if (desktopShell) {
      alertButton.hidden = true;
      alertStatus.textContent = "Desktop alerts use system notifications.";
    } else initializePush();
    await focusDeepLinkedMessage();
    focusDeepLinkedNotification();
  } catch (error) {
    list.replaceChildren(element("div", "error", `Unable to check session: ${error.message}`));
  }
}

loginForm.addEventListener("submit", async event => {
  event.preventDefault();
  loginError.textContent = "";
  const submit = loginForm.querySelector("button");
  submit.disabled = true;
  try {
    const data = Object.fromEntries(new FormData(loginForm));
    const response = await fetch("/api/v1/session", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(data)});
    if (!response.ok) throw new Error(response.status === 401 ? "Invalid username or password." : `Sign-in failed (HTTP ${response.status}).`);
    loginForm.reset();
    await initializeSession();
  } catch (error) {
    loginError.textContent = error.message;
  } finally {
    submit.disabled = false;
  }
});

oidcLoginButton.addEventListener("click", async () => {
  loginError.textContent = "";
  if (!desktopShell) {
    window.location.assign("/api/v1/auth/oidc/start");
    return;
  }
  oidcLoginButton.disabled = true;
  try {
    const handoff = Array.from(crypto.getRandomValues(new Uint8Array(32)), value => value.toString(16).padStart(2, "0")).join("");
    await desktopShell.beginOIDCLogin(handoff);
    loginError.textContent = "Complete sign-in in your browser.";
    for (let attempt = 0; attempt < 600; attempt += 1) {
      await new Promise(resolve => setTimeout(resolve, 1000));
      const exchange = await fetch("/api/v1/auth/desktop/session", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({code:handoff})});
      if (exchange.status === 401) continue;
      if (!exchange.ok) throw new Error(`Desktop sign-in failed (HTTP ${exchange.status}).`);
      await initializeSession(true);
      return;
    }
    throw new Error("Desktop sign-in timed out.");
  } catch (error) {
    loginError.textContent = `Unable to open browser sign-in: ${error}`;
  } finally {
    oidcLoginButton.disabled = false;
  }
});

logoutButton.addEventListener("click", async () => {
  await fetch("/api/v1/session", {method:"DELETE"});
  if (events) events.close();
  await initializeSession();
});

readButton.addEventListener("click", async () => {
  readButton.disabled = true;
  const selected = channelCache.find(channel => channel.name === selectedChannel);
  const endpoint = selected ? `/api/v1/channels/${encodeURIComponent(selected.id)}/read` : "/api/v1/notifications/read";
  const response = await fetch(endpoint, {method:"POST"});
  if (response.ok) {
    await loadNotifications(false);
    await loadChannels();
  }
  else readButton.disabled = false;
});

if (matchMedia("(max-width: 700px)").matches && ![inboxSearch.value, stateFilter.value, severityFilter.value].some(Boolean)) {
  filterConsole.open = false;
}
inboxNav.addEventListener("click", event => {
  event.preventDefault();
  showPrimaryFeed(true);
});
activityNav.addEventListener("click", event => {
  event.preventDefault();
  showPrimaryFeed(false);
});
filtersNav.addEventListener("click", event => {
  event.preventDefault();
  filterConsole.open = true;
  filterConsole.hidden = false;
  updatePrimaryNavigation("filters");
  filterConsole.scrollIntoView({block: "start"});
  inboxSearch.focus({preventScroll: true});
});

// Compact view trades card padding and type scale for more cards on screen, and
// at very wide viewports splits the feed into two columns. It is a stored
// preference rather than a viewport rule so a large display can still show the
// roomy layout.
function applyDensity(compact) {
  document.body.classList.toggle("density-compact", compact);
  densityButton.setAttribute("aria-pressed", String(compact));
  densityButton.textContent = compact ? "Roomy view" : "Compact view";
}

applyDensity(localStorage.getItem("tintwire-density") === "compact");

densityButton.addEventListener("click", () => {
  const compact = !document.body.classList.contains("density-compact");
  localStorage.setItem("tintwire-density", compact ? "compact" : "roomy");
  applyDensity(compact);
});

function typingTarget(target) {
  return target instanceof HTMLElement &&
    (target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName));
}

// Moving keyboard focus into an editable control (search box, composer, command
// field) must not leave a card highlighted by the j/k navigator. Otherwise the
// caret sits in one place while a card elsewhere still shows the selection
// highlight.
document.addEventListener("focusin", event => {
  if (typingTarget(event.target)) selectCard(null);
});

initializeSession();
