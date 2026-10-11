"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const fs = require("node:fs");
const {webcrypto} = require("node:crypto");
const source = fs.readFileSync(require.resolve("../web/push-health.js"), "utf8");
function fixture(config = {}) {
  const elements = Object.fromEntries(["testButton", "repairButton", "confirmButton", "status", "confirmation"].map(key => [key, {addEventListener(event, fn) { this[event] = fn; }}]));
  const storage = config.storage || new Map();
  const requests = [];
  let time = 100000, userID = "user-1", repairCalls = 0;
  let subscription = {endpoint: "https://push.example/device-1", toJSON() { return {endpoint: this.endpoint, keys: {auth:"generated-auth",p256dh:"generated-key"}}; }};
  const context = {crypto: webcrypto, TextEncoder, Uint8Array, Date, JSON};
  vm.runInNewContext(source, context);
  const health = context.TintwirePushHealth.create({
    ...elements, now: () => time, userID: () => userID,
    storage: {getItem: key => storage.get(key), setItem: (key, value) => { if (config.failStorage) throw Error("blocked"); storage.set(key, value); }, removeItem: key => storage.delete(key)},
    fetch: async (url, options) => { requests.push({url, ...options});return config.response || {ok:true,json:async()=>({outcome:"provider_accepted"})}; },
    client: () => ({repair: async () => {repairCalls++;return config.repairResult || {status:"on",subscription};}})
  });
  return {health, elements, storage, requests, get repairCalls() {return repairCalls;}, get subscription() {return subscription;},
    state: async (status="on", endpoint) => { if(endpoint) subscription={...subscription,endpoint}; await health.setState({status,subscription},userID); },
    setUser: value => {userID=value;}, advance: value => {time+=value;}
  };
}
test("provider acceptance never confirms device receipt; explicit confirmation does", async () => {
 const f=fixture();await f.state();assert.equal(f.elements.testButton.disabled,false);
 await f.elements.testButton.click();
 assert.equal(f.requests.length,1);assert.equal(JSON.parse(f.requests[0].body).endpoint,f.subscription.endpoint);
 assert.match(f.elements.status.textContent,/accepted/);assert.match(f.elements.confirmation.textContent,/No confirmed/);
 assert.equal(f.elements.confirmButton.hidden,false);
 f.elements.confirmButton.click();assert.match(f.elements.confirmation.textContent,/confirmed by you/);
 assert.equal(f.elements.confirmButton.hidden,true);
 const saved=JSON.stringify([...f.storage]);assert.doesNotMatch(saved,/push.example|generated-auth|generated-key/);
});
test("confirmation survives reload only for the same account and subscription",async()=>{
 const f=fixture();await f.state();await f.elements.testButton.click();f.elements.confirmButton.click();
 const reload=fixture({storage:f.storage});await reload.state();assert.match(reload.elements.confirmation.textContent,/confirmed by you/);
 await reload.state("on","https://push.example/device-2");assert.match(reload.elements.confirmation.textContent,/No confirmed/);
 reload.setUser("other");await reload.state();assert.match(reload.elements.confirmation.textContent,/No confirmed/);
});
test("repair clears confirmation even when browser returns the same endpoint",async()=>{
 const f=fixture();await f.state();await f.elements.testButton.click();f.elements.confirmButton.click();
 await f.elements.repairButton.click();assert.equal(f.repairCalls,1);
 assert.match(f.elements.confirmation.textContent,/No confirmed/);assert.match(f.elements.status.textContent,/renewed/);
 await f.elements.testButton.click();assert.equal(f.elements.confirmButton.hidden,false);f.elements.confirmButton.click();
 assert.match(f.elements.confirmation.textContent,/confirmed by you/);
});
test("provider errors cannot be confirmed and expired endpoint offers repair",async()=>{
 const f=fixture({response:{ok:false,status:410,json:async()=>({message:"Subscription expired. Repair alerts."})}});
 await f.state();await f.elements.testButton.click();assert.equal(f.elements.confirmButton.hidden,true);
 f.elements.confirmButton.click();assert.match(f.elements.confirmation.textContent,/No confirmed/);assert.match(f.elements.status.textContent,/Repair/);
});
test("failed repair stays an error and cannot be mistaken for success",async()=>{
 const f=fixture({repairResult:{status:"error",message:"Unable to save subscription"}});
 await f.state();await f.elements.repairButton.click();assert.match(f.elements.status.textContent,/Unable to save/);
 assert.equal(f.elements.testButton.disabled,true);assert.equal(f.elements.repairButton.disabled,false);
});
test("expired tests and disabling cannot reuse an old confirmation",async()=>{
 const f=fixture();await f.state();await f.elements.testButton.click();f.advance(600001);f.elements.confirmButton.click();
 assert.match(f.elements.confirmation.textContent,/No confirmed/);assert.equal(f.elements.confirmButton.hidden,true);
 await f.elements.testButton.click();f.elements.confirmButton.click();await f.state("off");await f.state();
 assert.match(f.elements.confirmation.textContent,/No confirmed/);
});
test("confirmation remains usable when local storage is unavailable",async()=>{
 const f=fixture({failStorage:true});await f.state();await f.elements.testButton.click();f.elements.confirmButton.click();
 assert.match(f.elements.confirmation.textContent,/confirmed by you/);
});
test("focus refresh cannot turn provider acceptance into confirmed delivery",async()=>{
 const f=fixture();await f.state();await f.elements.testButton.click();await f.state("checking");await f.state();
 assert.equal(f.elements.confirmButton.hidden,false);assert.match(f.elements.confirmation.textContent,/No confirmed/);
});
test("a late test response cannot attach to a different signed-in user",async()=>{
 let resolve;const f=fixture({response:{ok:true,json:()=>new Promise(r=>{resolve=r;})}});
 await f.state();const work=f.elements.testButton.click();await new Promise(r=>setImmediate(r));
 f.setUser("user-2");await f.state();resolve({outcome:"provider_accepted"});await work;
 assert.equal(f.elements.confirmButton.hidden,true);assert.match(f.elements.confirmation.textContent,/No confirmed/);
});

test("repair adapter treats an already removed endpoint as success but keeps server errors",async()=>{
 const app=fs.readFileSync(require.resolve("../web/app.js"),"utf8");
 const code=app.slice(app.indexOf("async function removeSubscription("),app.indexOf("function showPushState("));
 for(const status of [200,404,403,500]) {
  const context={fetch:async()=>({ok:status===200,status}),JSON,Error};vm.runInNewContext(code,context);
  const result=context.removeSubscription({endpoint:"https://push.example/device"});
  if(status===200||status===404) await result;else await assert.rejects(result,/unsubscribe rejected/);
 }
});
