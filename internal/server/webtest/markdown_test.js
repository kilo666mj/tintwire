"use strict";

const assert = require("node:assert/strict");
const markdown = require("../web/markdown.js");

assert.deepEqual(
  markdown.parse("*Alert:* Disk nearly full - `critical`"),
  [
    {type: "strong", value: "Alert:"},
    {type: "text", value: " Disk nearly full - "},
    {type: "code", value: "critical"},
  ],
);

assert.deepEqual(
  markdown.parse("See <https://alerts.example/graph|source>\n**Resolved**"),
  [
    {type: "text", value: "See "},
    {type: "link", value: "source", href: "https://alerts.example/graph"},
    {type: "text", value: "\n"},
    {type: "strong", value: "Resolved"},
  ],
);

assert.deepEqual(
  markdown.parse("<script>alert(1)</script>"),
  [{type: "text", value: "<script>alert(1)</script>"}],
);

assert.deepEqual(
  markdown.parse("Open https://example.com/releases?id=42, then visit (https://example.com/docs)."),
  [
    {type: "text", value: "Open "},
    {type: "link", value: "https://example.com/releases?id=42", href: "https://example.com/releases?id=42"},
    {type: "text", value: ", then visit ("},
    {type: "link", value: "https://example.com/docs", href: "https://example.com/docs"},
    {type: "text", value: ")."},
  ],
);

assert.deepEqual(
  markdown.parseBlocks("Status\n\n| Host | Used | State |\n| :--- | ---: | :---: |\n| web01 | `91%` | **critical** |"),
  [
    {type: "text", value: "Status\n"},
    {
      type: "table",
      headers: ["Host", "Used", "State"],
      alignments: ["left", "right", "center"],
      rows: [["web01", "`91%`", "**critical**"]],
    },
  ],
);

assert.deepEqual(
  markdown.parseBlocks("one | two\nnot a separator"),
  [{type: "text", value: "one | two\nnot a separator"}],
);
