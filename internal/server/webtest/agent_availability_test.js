"use strict";
const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {test} = require("node:test");
const vm = require("node:vm");
const source = readFileSync(require.resolve("../web/app.js"), "utf8");
const code = source.slice(source.indexOf("function availabilityState("), source.indexOf('agentsOpen.addEventListener("click"'));
function element(_tag, classes, text) {
 return {classes,textContent:text,children:[],append(...nodes){this.children.push(...nodes)},replaceChildren(...nodes){this.children=nodes},addEventListener(name,fn){this[name]=fn}};
}
function setup() {
 const c = {Date, element, selectedChannels:[], selectedChannel:"chat", agentConversations:[], agentDirectoryError:"", agentDirectoryLoading:false,
  agentsDirectory:element(),agentsDirectoryStatus:element(),conversationAgents:element(),agentsOpen:{hidden:false},
  agentsDialog:{close(){c.closed=true}},composerInput:{focus(){c.focused=true}},selectChannel(name){c.selectedChannel=name}};
 vm.createContext(c);vm.runInContext(code,c);return c;
}
const agent = (state,expires=Date.now()+60000) => ({name:"helper",display_name:"Helper",description:"Helps with work",channel:"chat",state,expires_at:new Date(expires).toISOString(),last_seen_at:new Date().toISOString()});
test("expired ready status becomes offline in directory and conversation",()=>{
 const c=setup();c.agentConversations=[agent("ready",Date.now()-1)];c.renderAgentDirectory();
 assert.match(c.agentsDirectory.children[0].children[0].children[2].textContent,/Offline/);
 assert.match(c.conversationAgents.children[0].textContent,/Offline/);
 assert.equal(c.conversationAgents.hidden,false);
});
test("busy agents explain queuing and open the correct conversation",()=>{
 const c=setup();c.agentConversations=[agent("busy")];c.renderAgentDirectory();
 assert.match(c.conversationAgents.children[0].textContent,/Busy · messages queue/);
 c.agentsDirectory.children[0].children[1].click();assert.equal(c.selectedChannel,"chat");assert.equal(c.closed,true);assert.equal(c.focused,true);
 c.selectedChannel="other";c.renderConversationAgents();assert.equal(c.conversationAgents.hidden,true);
});
test("failed refresh clears stale availability and successful refresh recovers",async()=>{
 const c=setup();c.agentConversations=[agent("ready")];c.fetch=async()=>({ok:false,status:403});await c.loadAgentDirectory();
 assert.equal(c.agentsDirectory.children.length,0);assert.match(c.agentsDirectoryStatus.textContent,/Unable to check/);
 c.fetch=async()=>({ok:true,json:async()=>({agents:[agent("ready")]})});await c.loadAgentDirectory();
 assert.equal(c.agentsDirectory.children.length,1);assert.equal(c.agentsDirectoryStatus.textContent,"");
});
