"use strict";
const assert = require("node:assert/strict");
const {test} = require("node:test");
const {DesktopAlertPoller} = require("../web/desktop-alerts.js");

test("desktop polling follows all pages without depending on inbox selection", async () => {
 const calls=[], notifications=[];
 const poller=new DesktopAlertPoller({
  fetchPage: async cursor => { calls.push(cursor); if (!cursor) return {alerts:[],cursor:"start",has_more:false}; if(cursor==="start") return {alerts:[{id:"other-channel",version:1,title:"Alert"}],cursor:"page2",has_more:true}; return {alerts:[{id:"new-message",version:2,title:"Message"}],cursor:"end",has_more:false}; },
  notify: async value=>notifications.push(value), reportError:error=>{throw error},
 });
 await poller.poll(); assert.equal(notifications.length,0,"first launch must not replay backlog");
 await poller.poll(); assert.deepEqual(calls,["","start","page2"]); assert.deepEqual(notifications.map(n=>n.id),["other-channel","new-message"]);
});

test("failed OS notifications remain retryable and do not advance the cursor",async()=>{
 let fail=true, successes=0, errors=0;
 const poller=new DesktopAlertPoller({fetchPage:async()=>({alerts:[{id:"one",version:1}],cursor:"done",has_more:false}),notify:async()=>{if(fail)throw Error("notification service unavailable");successes++},reportError:()=>errors++});
 await poller.poll(); assert.equal(poller.cursor,""); assert.equal(errors,1);
 fail=false;await poller.poll();await poller.poll();assert.equal(successes,1);assert.equal(poller.cursor,"done");
});

test("concurrent stream and timer wakeups share one poll and logout resets state",async()=>{
 let release,calls=0;
 const poller=new DesktopAlertPoller({fetchPage:async()=>{calls++;await new Promise(r=>{release=r});return {alerts:[],cursor:"next"}},notify:async()=>{},reportError:error=>{throw error}});
 const first=poller.poll(); await poller.poll(); assert.equal(calls,1);release();await first;
 poller.fetchPage=async()=>{const e=Error("signed out");e.status=401;throw e};await poller.poll();assert.equal(poller.cursor,"");
});
