"use strict";

// Kept independent of inbox rendering: changing a filter or opening a channel
// must never change which desktop notifications are delivered.
(function (root) {
  class DesktopAlertPoller {
    constructor({fetchPage, notify, reportError}) {
      this.fetchPage = fetchPage;
      this.notify = notify;
      this.reportError = reportError;
      this.cursor = "";
      this.running = false;
      this.seen = new Map();
    }
    async poll() {
      if (this.running) return;
      this.running = true;
      try {
        let more;
        do {
          const page = await this.fetchPage(this.cursor);
          for (const alert of page.alerts || []) {
            if (this.seen.get(alert.id) === alert.version) continue;
            await this.notify({id: alert.id, title: alert.title, body: alert.body, urgent: alert.urgent});
            this.seen.set(alert.id, alert.version);
            if (this.seen.size > 2000) this.seen.delete(this.seen.keys().next().value);
          }
          this.cursor = page.cursor;
          more = page.has_more;
        } while (more);
      } catch (error) {
        if (error.status === 401) { this.cursor = ""; this.seen.clear(); }
        else this.reportError(error);
      } finally { this.running = false; }
    }
  }
  if (typeof module !== "undefined" && module.exports) module.exports = {DesktopAlertPoller};
  else root.DesktopAlertPoller = DesktopAlertPoller;
})(globalThis);
