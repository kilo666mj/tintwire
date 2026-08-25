(function (root) {
  "use strict";

  const tokenPattern = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\*[^*\n]+\*|<https?:\/\/[^<>\s|]+(?:\|[^<>\n]+)?>|https?:\/\/[^<>\s]+|:[a-z0-9_+\-]+:)/gi;

  function bareLink(token) {
    let href = token;
    let trailing = "";
    while (/[.,;:!]$/.test(href)) {
      trailing = href.slice(-1) + trailing;
      href = href.slice(0, -1);
    }
    while (href.endsWith(")") && (href.match(/\(/g) || []).length < (href.match(/\)/g) || []).length) {
      trailing = ")" + trailing;
      href = href.slice(0, -1);
    }
    return {href, trailing};
  }

  function parse(text) {
    const value = String(text ?? "");
    const tokens = [];
    let offset = 0;

    for (const match of value.matchAll(tokenPattern)) {
      if (match.index > offset) {
        tokens.push({type: "text", value: value.slice(offset, match.index)});
      }

      const token = match[0];
      if (token.startsWith("`")) {
        tokens.push({type: "code", value: token.slice(1, -1)});
      } else if (token.startsWith("**")) {
        tokens.push({type: "strong", value: token.slice(2, -2)});
      } else if (token.startsWith("*")) {
        tokens.push({type: "strong", value: token.slice(1, -1)});
      } else if (token.startsWith("<")) {
        const link = token.slice(1, -1);
        const separator = link.indexOf("|");
        const href = separator === -1 ? link : link.slice(0, separator);
        const label = separator === -1 ? link : link.slice(separator + 1);
        tokens.push({type: "link", value: label, href});
      } else if (token.startsWith(":")) {
        const name = token.slice(1, -1).toLowerCase();
        const emoji = root.TintwireEmoji?.[name];
        tokens.push(emoji ? {type: "emoji", value: emoji, name} : {type: "text", value: token});
      } else {
        const link = bareLink(token);
        tokens.push({type: "link", value: link.href, href: link.href});
        if (link.trailing) tokens.push({type: "text", value: link.trailing});
      }
      offset = match.index + token.length;
    }

    if (offset < value.length) {
      tokens.push({type: "text", value: value.slice(offset)});
    }
    return tokens.reduce((merged, token) => {
      const previous = merged[merged.length - 1];
      if (token.type === "text" && previous?.type === "text") {
        previous.value += token.value;
      } else {
        merged.push(token);
      }
      return merged;
    }, []);
  }

  function splitTableRow(line) {
    let value = line.trim();
    if (value.startsWith("|")) value = value.slice(1);
    if (value.endsWith("|")) value = value.slice(0, -1);

    const cells = [];
    let cell = "";
    for (let index = 0; index < value.length; index += 1) {
      if (value[index] === "\\" && value[index + 1] === "|") {
        cell += "|";
        index += 1;
      } else if (value[index] === "|") {
        cells.push(cell.trim());
        cell = "";
      } else {
        cell += value[index];
      }
    }
    cells.push(cell.trim());
    return cells;
  }

  function tableAlignment(cell) {
    const value = cell.trim();
    if (!/^:?-{3,}:?$/.test(value)) return null;
    if (value.startsWith(":") && value.endsWith(":")) return "center";
    if (value.endsWith(":")) return "right";
    return "left";
  }

  function parseBlocks(text) {
    const lines = String(text ?? "").split("\n");
    const blocks = [];
    let plain = [];

    function flushPlain() {
      if (plain.length) {
        blocks.push({type: "text", value: plain.join("\n")});
        plain = [];
      }
    }

    for (let index = 0; index < lines.length; index += 1) {
      const header = splitTableRow(lines[index]);
      const separator = index + 1 < lines.length ? splitTableRow(lines[index + 1]) : [];
      const alignments = separator.map(tableAlignment);
      const isTable = lines[index].includes("|") &&
        header.length > 1 &&
        separator.length === header.length &&
        alignments.every((alignment) => alignment !== null);

      if (!isTable) {
        plain.push(lines[index]);
        continue;
      }

      flushPlain();
      const rows = [];
      index += 2;
      while (index < lines.length && lines[index].includes("|")) {
        const row = splitTableRow(lines[index]);
        if (row.length !== header.length) break;
        rows.push(row);
        index += 1;
      }
      index -= 1;
      blocks.push({type: "table", headers: header, alignments, rows});
    }

    flushPlain();
    return blocks;
  }

  const api = {parse, parseBlocks};
  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  } else {
    root.TintwireMarkdown = api;
  }
})(globalThis);
