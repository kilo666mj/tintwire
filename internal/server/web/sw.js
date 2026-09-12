"use strict";
importScripts("/pwa-kit/worker.js");
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));
PWAKitWorker.installPushHandlers({
  title:"Tintwire",icon:"/assets/icon-192.png",badge:"/assets/icon-192.png",tag:"tintwire-notification",
  notificationOptions:data=>({renotify:true,requireInteraction:data.state==="firing",timestamp:data.timestamp||Date.now()}),
  badgeCount:()=>"dot"
});
