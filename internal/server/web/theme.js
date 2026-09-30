"use strict";
// Applies the stored visual theme before the first paint. It loads
// synchronously from <head> because the CSP forbids inline scripts, and it
// must run before the stylesheets render to avoid a flash of the other theme.
(function () {
  let skin = "sentinel";
  try {
    if (localStorage.getItem("tintwire-skin") === "wire") skin = "wire";
  } catch {}
  document.documentElement.dataset.skin = skin;
})();
