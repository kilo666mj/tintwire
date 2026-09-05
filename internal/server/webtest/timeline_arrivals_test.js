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
