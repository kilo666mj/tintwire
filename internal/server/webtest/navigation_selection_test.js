"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");
const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const ranges = [
  ["function channelButton(", "function applyChannelSort("],
  ["function writeFilterURL(", "async function loadChannels("],
  ["function applySavedView(", "async function loadSavedViews("],
  ['window.addEventListener("popstate",', 'desktopShell?.listen?.("tintwire://mark-all-read"'],
];
const code = ranges.map(([start, end]) => source.slice(source.indexOf(start), source.indexOf(end))).join("\n");

function element(tag, classes = "", text = "") {
  const names = new Set(classes.split(" ").filter(Boolean));
  return {
    textContent: text, children: [], dataset: {}, attributes: {}, style: {setProperty() {}},
    classList: {add: name => names.add(name), contains: name => names.has(name)},
    setAttribute(name, value) { this.attributes[name] = value; },
    append(...children) { this.children.push(...children); },
    replaceChildren(...children) { this.children = children; },
    addEventListener(name, handler) { this[name] = handler; },
  };
}

function setup() {
  const context = {
    URLSearchParams, element, location: {search: "", pathname: "/"},
    selectedChannel: "", selectedChannels: [],
    channelCache: [{name: "logw", display_name: "logw", unread_count: 53}],
    savedViewCache: [{id: "news", name: "News", channels: ["social"], unread: true}],
    unreadChannelsFirst: false, inboxStateEnabled: false, isAdmin: false,
    channelList: element(), mobileChannelList: element(), savedViewList: element(),
    channelEditButton: {}, mobileChannelEditButton: {}, feedTitle: {}, mobileChannelToggle: {},
    inboxFilters: {}, inboxSearch: {value: ""}, channelFilter: {value: ""},
    stateFilter: {value: ""}, severityFilter: {value: ""}, readFilter: {value: "1"},
    setViewForChannel() {}, loadNotifications() {},
    matchMedia: () => ({matches: false}), channelDialog: {open: false},
    window: {addEventListener(name, callback) { context[name] = callback; }},
  };
  context.FormData = function () {
    return Object.entries({q: context.inboxSearch.value, channel: context.channelFilter.value,
      state: context.stateFilter.value, severity: context.severityFilter.value, unread: context.readFilter.value});
  };
  const navigate = (_state, _title, url) => { context.location.search = new URL(url, "https://example.invalid").search; };
  context.history = {pushState: navigate, replaceState: navigate};
  vm.createContext(context);
  vm.runInContext(code, context);
  context.renderChannelNavigation(context.channelCache);
  return context;
}

function assertSelection(c, expected, title) {
  const current = list => list.children.flatMap(row => row.children)
    .filter(button => button.classList.contains("active"))
    .map(button => {
      assert.equal(button.attributes["aria-current"], "page");
      return button.dataset.channel ?? button.textContent;
    });
  assert.deepEqual([...current(c.channelList), ...current(c.savedViewList)], [expected]);
  assert.deepEqual(current(c.mobileChannelList), current(c.channelList));
  assert.equal(c.feedTitle.textContent, title);
}

test("switching between saved views and channels clears the previous selection immediately", () => {
  const c = setup();
  c.selectChannel("logw");
  assertSelection(c, "logw", "logw");
  c.applySavedView(c.savedViewCache[0]);
  assertSelection(c, "News", "News");
  c.selectChannel("logw");
  assertSelection(c, "logw", "logw");
  assert.equal(new URLSearchParams(c.location.search).has("view"), false);
});

test("leaving a saved view for the primary or firing feed clears its scope and highlight", () => {
  for (const leave of [c => c.showPrimaryFeed(true), c => c.selectChannel(""),
    c => c.openFiringView(""), c => c.openFiringView("logw")]) {
    const c = setup();
    c.applySavedView(c.savedViewCache[0]);
    leave(c);
    assertSelection(c, c.selectedChannel, c.selectedChannel || "All notifications");
    assert.equal(c.selectedChannels.length, 0);
    const params = new URLSearchParams(c.location.search);
    assert.equal(params.has("view"), false);
    assert.equal(params.has("channels"), false);
  }
});

test("browser back and forward restore both navigation lists", () => {
  const c = setup();
  c.applySavedView(c.savedViewCache[0]);
  const savedURL = c.location.search;
  c.selectChannel("logw");
  const channelURL = c.location.search;
  c.location.search = savedURL;
  c.popstate();
  assertSelection(c, "News", "News");
  c.location.search = channelURL;
  c.popstate();
  assertSelection(c, "logw", "logw");
});
