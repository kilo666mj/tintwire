"use strict";
const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");
const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const code = source.slice(source.indexOf("const nativeActionDrafts"), source.indexOf("function activityItem("));
function element(tag, classes="", text="") {
  return {tag,classes,textContent:text,children:[],value:"",disabled:false,
    append(...children){this.children.push(...children)},setAttribute(){},focus(){this.focused=true},
    addEventListener(name, fn){this[name]=fn}};
}
function setup() {
 const calls=[];
 const c={element,TextEncoder,crypto:{randomUUID:()=>`operation-${calls.length}`},loads:0,
   loadNotifications(){c.loads++},async fetch(url, request){calls.push({url,...request});return {ok:true,json:async()=>({status:"succeeded",response:"Queued"})}}};
 vm.createContext(c);vm.runInContext(code,c);
 const value={title:"Question",actions:[{id:"viewed-version-1",type:"http",label:"Send answer",input:{label:"Which resolver?",required:true}}]};
 const render=()=>{
  const card=c.nativeCard(value,{id:"notification-1",can_operate:true});
  const group=card.children[1].children[0];
  return {card,answer:group.children[0].children[0],button:group.children[1],feedback:group.children[2]};
 };
 return {c,calls,value,render};
}
test("answer is bound to the viewed action and blank input is not dispatched",async()=>{
 const {c,calls,render}=setup();const ui=render();
 await ui.button.click();assert.equal(calls.length,0);assert.equal(ui.answer.focused,true);
 ui.answer.value="Keep existing resolver";await ui.button.click();
 assert.deepEqual(JSON.parse(calls[0].body),{action_id:"viewed-version-1",input:"Keep existing resolver"});
 assert.equal(c.loads,1);assert.equal(ui.answer.value,"");
});
test("draft survives card refresh and network retry keeps the operation identity",async()=>{
 const {c,calls,render}=setup();let ui=render();ui.answer.value="resolver A";ui.answer.input();
 ui=render();assert.equal(ui.answer.value,"resolver A");
 c.fetch=async(url,request)=>{calls.push(request);throw Error("offline")};
 await ui.button.click();assert.equal(ui.button.disabled,false);assert.match(ui.feedback.textContent,/offline/);
 await ui.button.click();assert.equal(calls[0].headers["Idempotency-Key"],calls[1].headers["Idempotency-Key"]);
 ui.answer.value="resolver B";await ui.button.click();assert.notEqual(calls[1].headers["Idempotency-Key"],calls[2].headers["Idempotency-Key"]);
});
test("stale action errors remain visible and do not clear the answer",async()=>{
 const {c,render}=setup();const ui=render();ui.answer.value="answer";
 c.fetch=async()=>({ok:false,text:async()=>"This action has changed. Refresh the card."});
 await ui.button.click();assert.match(ui.feedback.textContent,/changed/);assert.equal(ui.answer.value,"answer");assert.equal(c.loads,0);
});
