"use strict";
const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");
const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const code = source.slice(source.indexOf("const nativeActionDrafts"), source.indexOf("function activityItem("));
function element(tag, classes="", text="") {
  return {tag,classes,textContent:text,children:[],value:"",disabled:false,
    append(...children){this.children.push(...children);for(const c of children)c.parentNode=this},
    insertBefore(child,before){const i=this.children.indexOf(before);if(i<0)this.append(child);else{this.children.splice(i,0,child);child.parentNode=this}},
    replaceChildren(...children){this.children=[];this.append(...children)},setAttribute(){},focus(){this.focused=true},
    addEventListener(name, fn){this[name]=fn}};
}
function setup() {
 const calls=[];
 const c={element,richContent:(classes,text)=>element("div",classes,text),currentUserID:"user-1",TextEncoder,crypto:{randomUUID:()=>`operation-${calls.length}`},loads:0,
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

function approvalUI(context, value) {
 const card=context.nativeCard(value,{id:"notification-1",can_operate:true});
 const actions=card.children.find(n=>n.classes==="native-actions");
 return {card,title:card.children[0].children[0],approve:actions.children[0].children[0],reject:actions.children[1].children[0],pause:actions.children[2].children[0]};
}
function approvalCard() {
 return {source:"agent-scheduler",title:"Investigation · awaiting approval",summary:"Review the proposed fix",badges:[{label:"awaiting approval",tone:"info"}],actions:[
  {id:"approve-v1",type:"http",label:"Approve fix"},
  {id:"reject-v1",type:"http",label:"Reject"},
  {id:"pause-v1",type:"http",label:"Stand down · this episode"}]};
}
test("accepted approval stays visible through stale refreshes and yields to published state",async()=>{
 const {c,calls}=setup();const value=approvalCard();let ui=approvalUI(c,value);
 await ui.approve.click();assert.match(ui.title.textContent,/approved · queued/);
 assert.equal(ui.approve.textContent,"Approved ✓");assert.equal(ui.reject.disabled,true);assert.equal(ui.pause.disabled,false);
 await ui.approve.click();assert.equal(calls.length,1);
 ui=approvalUI(c,value);assert.match(ui.title.textContent,/approved · queued/);assert.equal(ui.approve.disabled,true);
 const running={...value,title:"Investigation · running",actions:[value.actions[2]]};
 const card=c.nativeCard(running,{id:"notification-1",can_operate:true});assert.equal(card.children[0].children[0].textContent,"Investigation · running");
 const revised={...value,actions:value.actions.map(a=>({...a,id:a.id+"-new"}))};
 ui=approvalUI(c,revised);assert.equal(ui.approve.disabled,false);assert.match(ui.title.textContent,/awaiting approval/);
});
test("failed approval never shows approved and remains retryable",async()=>{
 const {c}=setup();c.fetch=async()=>({ok:true,json:async()=>({status:"failed",response:"Approval expired"})});
 const value=approvalCard();let ui=approvalUI(c,value);await ui.approve.click();
 assert.match(ui.title.textContent,/awaiting approval/);assert.equal(ui.approve.disabled,false);
 ui=approvalUI(c,value);assert.equal(ui.approve.disabled,false);
});
test("successful approval survives refresh failure but does not leak into another session",async()=>{
 const {c}=setup();const value=approvalCard();const ui=approvalUI(c,value);
 c.loadNotifications=()=>{throw Error("refresh unavailable")};await ui.approve.click();
 assert.match(ui.title.textContent,/approved · queued/);assert.equal(ui.approve.disabled,true);
 vm.runInContext('nativeApprovalReceipts.clear()',c);
 c.fetch=async()=>{c.currentUserID="user-2";return {ok:true,json:async()=>({status:"succeeded"})}};
 const other=approvalUI(c,value);await other.approve.click();
 assert.equal(approvalUI(c,value).approve.disabled,false);
});
