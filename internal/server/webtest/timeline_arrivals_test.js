"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");

const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const tracking = source.slice(source.indexOf('const timelineNewMessages ='), source.indexOf('function captureTimelineScrollAnchor('));
const load = source.slice(source.indexOf('async function loadChannelTimeline('), source.indexOf('// Toggles between the global notification feed'));
const item = (id, created_at, kind = "message") => ({id, created_at, kind});

function setup() {
  const notice = {hidden: true};
  const jump = {};
  const listeners = {};
  let responseItems = [];
  let bottom = 1500;
  const list = {
    scrollHeight: 1500, scrollTop: 0, clientHeight: 500,
    addEventListener: (name, handler) => { listeners[name] = handler; },
    getBoundingClientRect: () => ({bottom}),
    setAttribute: () => {}, focus: () => {},
  };
  const context = {
    document: {querySelector: selector => selector === "#timeline-new-messages" ? notice : jump},
    window: {innerHeight: 500, addEventListener: () => {}},
    matchMedia: () => ({matches: false}),
    list, selectedChannel: "test", channelCache: [{name: "test", id: "channel-1"}],
    currentChannelID: "", timelineNextCursor: "", loadedTimelineItems: [],
    inboxSearch: {value: ""}, stateFilter: {value: ""}, readFilter: {value: "1"},
    heldSentMessages: new Map(), loadMoreButton: {}, URLSearchParams,
    fetch: async () => ({ok: true, json: async () => ({items: responseItems})}),
    captureTimelineScrollAnchor: () => ({}), anchorTimelineToEntry: () => {},
    renderChannelTimeline: () => {},
    anchorTimelineToBottom: () => { list.scrollTop = 1000; bottom = 500; },
  };
  jump.addEventListener = (name, handler) => { jump.click = handler; };
  vm.createContext(context);
  vm.runInContext(tracking + load, context);
  return {context, notice, jump, list, listeners,
    refresh: async (items, append = false) => {
      responseItems = items;
      await context.loadChannelTimeline(append);
    },
  };
}

test("arrivals show while reading earlier entries and persist through unchanged polls", async () => {
  const s = setup();
  await s.refresh([item("first", 100)]);
  assert.equal(s.notice.hidden, true);
  s.list.scrollTop = 0;
  const items = [item("new", 200, "notification"), item("first", 100)];
  await s.refresh(items);
  assert.equal(s.notice.hidden, false);
  await s.refresh(items);
  assert.equal(s.notice.hidden, false);
  s.jump.click();
  assert.equal(s.list.scrollTop, 1000);
  assert.equal(s.notice.hidden, true);
});

test("scrolling to the bottom clears the notice; arrivals at the bottom follow automatically", async () => {
  const s = setup();
  await s.refresh([item("first", 100)]);
  await s.refresh([item("second", 200)]);
  assert.equal(s.notice.hidden, true);
  s.list.scrollTop = 0;
  await s.refresh([item("third", 300, "command")]);
  assert.equal(s.notice.hidden, false);
  s.list.scrollTop = 990;
  s.listeners.scroll();
  assert.equal(s.notice.hidden, true);
});

test("history pagination, read-state backfill, and existing-item edits are not arrivals", async () => {
  const s = setup();
  await s.refresh([item("first", 100)]);
  s.list.scrollTop = 0;
  await s.refresh([item("older", 50)], true);
  assert.equal(s.notice.hidden, true);
  await s.refresh([item("first", 150), item("backfill", 25)]);
  assert.equal(s.notice.hidden, true);
});

test("filter changes establish a new baseline and channel exits clear pending arrivals", async () => {
  const s = setup();
  await s.refresh([item("first", 100)]);
  s.list.scrollTop = 0;
  await s.refresh([item("new", 200)]);
  s.context.inboxSearch.value = "filtered";
  await s.refresh([item("different", 300)]);
  assert.equal(s.notice.hidden, true);
  await s.refresh([item("arrival", 400)]);
  assert.equal(s.notice.hidden, false);
  s.context.resetTimelineArrivals();
  assert.equal(s.notice.hidden, true);
});

test("mobile bottom detection uses the viewport instead of the unbounded list", () => {
  const s = setup();
  s.context.matchMedia = () => ({matches: true});
  s.list.scrollHeight = s.list.clientHeight;
  assert.equal(s.context.isTimelineNearBottom(), false);
  s.context.anchorTimelineToBottom();
  assert.equal(s.context.isTimelineNearBottom(), true);
});

function setupInboxMutation() {
  const s = setup();
  const c = s.context;
  c.loadChannels = async () => {};
  c.loadNotifications = (append, announce, pin) => c.loadChannelTimeline(append, pin);
  c.showInboxToast = () => {};
  s.renders = [];
  c.renderChannelTimeline = items => { s.renders.push(JSON.parse(JSON.stringify(items))); };
  vm.runInContext(source.slice(source.indexOf('async function updateInboxState('), source.indexOf('function inboxButtons(')), c);
  return s;
}

function notificationItem(id) {
  return {...item(id, 100, "notification"), notification: {id, unread: true}};
}

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return {promise, resolve};
}

test("a successful read removes the unread timeline card before the refresh returns", async () => {
  const s = setupInboxMutation();
  await s.refresh([notificationItem("read-me"), notificationItem("keep-me")]);
  const refresh = deferred();
  let refreshStarted;
  const started = new Promise(resolve => { refreshStarted = resolve; });
  s.context.fetch = async (url, options) => {
    if (options?.method === "POST") return {ok: true};
    refreshStarted();
    return refresh.promise;
  };
  const mutation = s.context.updateInboxState(s.context.loadedTimelineItems[0].notification, "read");
  await started;
  const visible = s.renders.at(-1).map(entry => entry.id);
  refresh.resolve({ok: true, json: async () => ({items: [notificationItem("keep-me")]})});
  await mutation;
  assert.deepEqual(visible, ["keep-me"]);
});

test("an older timeline response cannot restore a card after marking it read", async () => {
  const s = setupInboxMutation();
  await s.refresh([notificationItem("read-me")]);
  const old = deferred();
  let reads = 0;
  s.context.fetch = async (url, options) => {
    if (options?.method === "POST") return {ok: true};
    if (++reads === 1) return old.promise;
    return {ok: true, json: async () => ({items: []})};
  };
  const poll = s.context.loadChannelTimeline();
  await s.context.updateInboxState(s.context.loadedTimelineItems[0].notification, "read");
  old.resolve({ok: true, json: async () => ({items: [notificationItem("read-me")]})});
  await poll;
  assert.deepEqual(s.renders.at(-1), []);
  assert.equal(s.context.loadedTimelineItems.length, 0);
});

test("mark read in history keeps the card and updates its rendered unread state", async () => {
  const s = setupInboxMutation();
  s.context.readFilter.value = "";
  await s.refresh([notificationItem("read-me")]);
  s.context.fetch = async (url, options) => options?.method === "POST" ? {ok: true} : {
    ok: true, json: async () => ({items: [{...notificationItem("read-me"), notification: {id: "read-me", unread: false}}]})
  };
  await s.context.updateInboxState(s.context.loadedTimelineItems[0].notification, "read");
  assert.equal(s.renders.at(-1).length, 1);
  assert.equal(s.renders.at(-1)[0].notification.unread, false);
});

test("a failed read keeps the unread card available for retry", async () => {
  const s = setupInboxMutation();
  await s.refresh([notificationItem("read-me")]);
  s.context.fetch = async () => ({ok: false, status: 500});
  await assert.rejects(s.context.updateInboxState(s.context.loadedTimelineItems[0].notification, "read"), /HTTP 500/);
  assert.equal(s.renders.at(-1)[0].notification.unread, true);
  assert.equal(s.context.loadedTimelineItems[0].notification.unread, true);
});
