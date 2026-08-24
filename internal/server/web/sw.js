"use strict";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

self.addEventListener("push", (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (_) {}
  const options = {
    body: data.body || "",
    icon: "/assets/icon-192.png",
    badge: "/assets/icon-192.png",
    tag: data.tag || "tintwire-notification",
    renotify: true,
    requireInteraction: data.state === "firing",
    timestamp: data.timestamp || Date.now(),
    data: {url: data.url || "/"}
  };
  const work = [self.registration.showNotification(data.title || "Tintwire", options)];
  if ("setAppBadge" in self.registration) work.push(self.registration.setAppBadge());
  event.waitUntil(Promise.all(work));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = new URL(event.notification.data?.url || "/", self.location.origin).href;
  event.waitUntil(self.clients.matchAll({type: "window", includeUncontrolled: true}).then((clients) => {
    for (const client of clients) {
      if ("navigate" in client) {
        return client.navigate(target).then((navigated) => (navigated || client).focus());
      }
    }
    return self.clients.openWindow(target);
  }));
});
