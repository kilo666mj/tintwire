// Generate the browser-side shortcode map from Emoji Mart's pinned native data.
// Usage: node scripts/generate-emoji-data.mjs input.json output.js
import fs from "node:fs";

const [input, output] = process.argv.slice(2);
if (!input || !output) throw new Error("input and output paths are required");

const data = JSON.parse(fs.readFileSync(input, "utf8"));
const emojis = {};
for (const [name, value] of Object.entries(data.emojis)) {
  const native = value.skins?.[0]?.native;
  if (native) emojis[name] = native;
}
for (const [alias, name] of Object.entries(data.aliases || {})) {
  if (emojis[name]) emojis[alias] = emojis[name];
}
Object.assign(emojis, {
  "skin-tone-2": "🏻",
  "skin-tone-3": "🏼",
  "skin-tone-4": "🏽",
  "skin-tone-5": "🏾",
  "skin-tone-6": "🏿",
});

const source = `// Generated from @emoji-mart/data 1.2.1 (MIT), Emoji 15 native set.\n` +
  `(function(root){"use strict";root.TintwireEmoji=Object.freeze(${JSON.stringify(emojis)});})(globalThis);\n`;
fs.writeFileSync(output, source);
