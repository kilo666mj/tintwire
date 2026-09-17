"use strict";

const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");

const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const helper = source.slice(source.indexOf("function desktopOIDCVerificationCode("), source.indexOf("oidcLoginButton.addEventListener("));
const context = {};
vm.runInNewContext(helper, context);

test("desktop OIDC code matches oidcrp's handoff-derived browser code", () => {
  assert.equal(context.desktopOIDCVerificationCode("0123456789abcdef"), "0123-4567");
  assert.equal(context.desktopOIDCVerificationCode("abcdef0123456789"), "ABCD-EF01");
});
