"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");

const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const handler = source.slice(source.indexOf("function handleReadShortcut("), source.indexOf("function selectedCard("));

function setup({editable = false, field = false, dialog = false, authenticated = true, cards = [{unread: true}]} = {}) {
  let listener;
  const context = {
    inboxStateEnabled: authenticated,
    document: {
      activeElement: {isContentEditable: editable, closest: () => field},
      querySelector: () => dialog,
      addEventListener: (name, callback) => { assert.equal(name, "keydown"); listener = callback; },
    },
    list: {
      querySelector: selector => {
        assert.equal(selector, ".card-unread");
        const card = cards.find(card => card.unread);
        return card ? {querySelector: () => card.restoreOnly ? null : {
          disabled: card.pending,
          click: () => { card.clicks = (card.clicks || 0) + 1; card.unread = false; },
        }} : null;
      },
    },
  };
  vm.runInNewContext(handler, context);
  return (overrides = {}) => {
    let prevented = false;
    listener({key: "r", preventDefault: () => { prevented = true; }, ...overrides});
    return prevented;
  };
}

test("r marks unread cards in feed order, skipping read cards", () => {
  const cards = [{unread: false}, {unread: true}, {unread: true}];
  const press = setup({cards});
  assert.equal(press(), true);
  assert.equal(cards[1].clicks, 1);
  assert.equal(cards[2].clicks, undefined);
  assert.equal(press(), true);
  assert.equal(cards[2].clicks, 1);
  assert.equal(press(), false);
  assert.equal(cards[0].clicks, undefined);
});

test("typing, dialogs, unauthenticated sessions, and pending or archived cards are protected", () => {
  for (const options of [{editable: true}, {field: true}, {dialog: true}, {authenticated: false},
    {cards: [{unread: true, pending: true}]}, {cards: [{unread: true, restoreOnly: true}]}, {cards: []}]) {
    assert.equal(setup(options)(), false, JSON.stringify(options));
  }
});

test("modified shortcuts, held keys, composition, and handled events are ignored", () => {
  for (const event of [{key: "x"}, {key: "R"}, {ctrlKey: true}, {metaKey: true}, {altKey: true},
    {shiftKey: true}, {repeat: true}, {isComposing: true}, {defaultPrevented: true}]) {
    assert.equal(setup()(event), false, JSON.stringify(event));
  }
});
