"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");

const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const initialization = source.slice(source.indexOf("function initializeMobileSidebarSections("), source.indexOf("// Compact view trades"));

function setup(storage = new Map(), blocked = false) {
  const sections = new Map();
  const buttons = ["saved-view-section", "sidebar-controls"].map(id => {
    sections.set(id, {id, dataset: {}});
    return {
      attributes: {"aria-controls": id},
      getAttribute(name) { return this.attributes[name]; },
      setAttribute(name, value) { this.attributes[name] = value; },
      addEventListener(name, callback) { assert.equal(name, "click"); this.click = callback; },
    };
  });
  vm.runInNewContext(initialization, {
    document: {querySelectorAll: () => buttons, getElementById: id => sections.get(id)},
    localStorage: {
      getItem(key) { if (blocked) throw new Error("Storage blocked"); return storage.get(key); },
      setItem(key, value) { if (blocked) throw new Error("Storage blocked"); storage.set(key, value); },
    },
  });
  return {buttons, sections};
}

function assertExpanded(state, index, expanded) {
  const button = state.buttons[index];
  assert.equal(button.getAttribute("aria-expanded"), String(expanded));
  assert.equal(state.sections.get(button.getAttribute("aria-controls")).dataset.mobileCollapsed, String(!expanded));
}

test("mobile sidebar sections default closed and open independently", () => {
  const state = setup();
  assertExpanded(state, 0, false);
  assertExpanded(state, 1, false);
  state.buttons[0].click();
  assertExpanded(state, 0, true);
  assertExpanded(state, 1, false);
  state.buttons[1].click();
  state.buttons[0].click();
  assertExpanded(state, 0, false);
  assertExpanded(state, 1, true);
});

test("mobile sidebar choices survive initialization again", () => {
  const storage = new Map();
  const state = setup(storage);
  state.buttons[1].click();
  const restored = setup(storage);
  assertExpanded(restored, 0, false);
  assertExpanded(restored, 1, true);
  restored.buttons[1].click();
  assertExpanded(setup(storage), 1, false);
});

test("mobile sidebar remains usable when preference storage is blocked", () => {
  const state = setup(new Map(), true);
  assertExpanded(state, 0, false);
  state.buttons[0].click();
  assertExpanded(state, 0, true);
  state.buttons[0].click();
  assertExpanded(state, 0, false);
});
