#!/usr/bin/env node
// Runtime check for the admin page's plain-JS behaviour.
//
// The Go tests cover the events API; they cannot execute the page's JavaScript,
// so a TypeError or a wrong-typed list in admin.html only shows up in a browser.
// One such bug shipped: the student picker pruned its selections against an
// array of user objects while holding usernames, so every pick vanished on the
// next render. `node --check` passes that code happily.
//
// This extracts the <script> from admin.html, runs it against a tiny DOM shim,
// and drives the event pickers directly. It needs no test framework and no
// browser. Run it from the repo root:
//
//   node scripts/check-admin-ui.js
//
// Exits non-zero and names the failing check on the first problem.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.resolve(__dirname, '..');
const page = path.join(root, 'internal/proxy/static/admin.html');
const html = fs.readFileSync(page, 'utf8');

const open = html.indexOf('<script>');
const close = html.lastIndexOf('</script>');
if (open < 0 || close < 0 || close < open) {
  console.error('check-admin-ui: no <script> block found in ' + page);
  process.exit(2);
}
let script = html.slice(open + '<script>'.length, close);

// Drop the page's own init IIFE: it wires listeners onto the real DOM and fires
// async loads on page open, which is noise here. The driver calls the functions
// under test directly.
script = script.split('\n(function init(){')[0];

// The shim is deliberately small: only what fillSchoolSelect, the pickers and
// addEvent touch. `$` and `log` are declared by admin.js itself, so they must
// not appear here or the two declarations collide.
const shim = `
var _els = {};
function _sel(id){
  if(!_els[id]){
    _els[id] = {
      value:'', textContent:'', className:'', style:{}, dataset:{}, children:[], options:[], listeners:{}, _html:'',
      get innerHTML(){ return this._html; },
      set innerHTML(v){ this._html=v; if(v===''){ this.children=[]; this.options=[]; this.value=''; } },
      appendChild(c){ this.children.push(c); if(c && c.tagName==='OPTION'){ this.options.push(c); if(!this.value) this.value=c.value; } return c; },
      addEventListener(k,f){ this.listeners[k]=f; },
      insertAdjacentHTML(){}, querySelector(){ return _sel('__q'); },
    };
  }
  return _els[id];
}
var document = {
  getElementById: _sel,
  createElement: function(t){ var e=_sel('__el'+Object.keys(_els).length); e.tagName=t.toUpperCase(); return e; },
  querySelector: function(){ return _sel('__q'); },
  querySelectorAll: function(){ return []; },
  addEventListener: function(){},
};
var setTimeout = function(){ return 0; };
var clearTimeout = function(){};
`;

// Stub the two endpoints the pickers read and the events POST, so the check is
// offline and deterministic. The recorded POST body is what the driver asserts.
const stub = `
var posted=null, logs=[];
var fetch = async function(url, opts){
  if(opts && opts.method==='POST'){ posted=JSON.parse(opts.body); return {ok:true,status:200,json:async()=>({created:[{eventId:1,username:'dee'},{eventId:2,username:'sam'}]})}; }
  if(url.indexOf('/admin/users')>=0) return {ok:true,status:200,json:async()=>({users:[
    {username:'dee',displayName:'Dee',personType:5,school:'schuldorf',className:'10a'},
    {username:'sam',displayName:'',personType:5,school:'schuldorf',className:'10a'},
    {username:'wils',displayName:'W',personType:2,school:'schuldorf',className:''},
    {username:'other',personType:5,school:'elsewhere',className:'9z'}]})};
  if(url.indexOf('type=ROOM')>=0) return {ok:true,status:200,json:async()=>({results:[
    {type:'ROOM',id:1,name:'R12'},{type:'ROOM',id:2,name:'R13'},{type:'ROOM',id:3,name:'Turnhalle'}]})};
  return {ok:true,status:200,json:async()=>({events:[],results:[]})};
};
`;

// The driver runs after admin.js in the same scope, so it sees the page's own
// let-bound state (status, schoolList, evStudentPicks, evRoomPicks, ...).
const driver = `
log = function(m){ logs.push(String(m)); };
function must(cond, msg){ if(!cond){ console.error('FAIL: '+msg); process.exit(1); } }

// A school selector must default to the school the proxy runs as, not an empty
// string and not the old "testschool" fixture.
status = {school:'schuldorf', schools:1, users:4, admins:1, pool:6, tokens:0, webhooks:0, ntfyTopics:1, recon:0, boosted:0, editors:0, outbox:{pending:0,dead:0}};
schoolList = [{name:'schuldorf'}];
fillSchoolSelect('poolSchool'); fillSchoolSelect('evSchool');
must($('poolSchool').value==='schuldorf', 'school selector did not default to the real school');
must(defaultSchool()==='schuldorf', 'defaultSchool() is not the running school');

(async function(){
  await loadEventPickers();

  // The student picker offers students of the selected school only: no teacher
  // accounts (personType 2) and nobody from another school.
  const kids = $('evStudentBox').children.map(c=>c.textContent);
  must(!kids.some(t=>t.indexOf('wils')>=0), 'a teacher account is offered as a student');
  must(!kids.some(t=>t.indexOf('other')>=0), 'a student of another school is offered');
  must(kids.length===2, 'expected 2 students, got '+kids.length);
  must($('evRoomBox').children.length===3, 'expected 3 rooms');

  // Multi-select: two students and two rooms.
  $('evStudentBox').children[0].onclick();
  $('evStudentBox').children[1].onclick();
  must(evStudentPicks.length===2, 'multi-select did not take two students: '+JSON.stringify(evStudentPicks));
  $('evRoomBox').children[0].onclick();
  $('evRoomBox').children[1].onclick();
  must(evRoomPicks.length===2, 'multi-select did not take two rooms: '+JSON.stringify(evRoomPicks));

  // Regression: narrowing the filter must not drop picks.
  evStudentPicks=['dee','sam'];
  $('evUser').value='sam'; renderStudentPicker();
  must(evStudentPicks.length===2, 'the filter dropped a selection');
  $('evUser').value='nomatch'; renderStudentPicker();
  must(evStudentPicks.length===2, 'an empty filter dropped a selection');
  $('evUser').value='';

  // addEvent posts one usernames[] batch and joins the rooms, leaving times the
  // admin typed exactly as typed (9:00 must not be coerced to 09:00).
  $('evTitle').value='Technik'; $('evDate').value='2026-10-01';
  $('evStart').value='9:00'; $('evEnd').value='10:00';
  await addEvent();
  must(Array.isArray(posted.usernames) && posted.usernames.length===2, 'batch usernames not sent: '+JSON.stringify(posted.usernames));
  must(posted.room==='R12, R13', 'rooms not joined: '+JSON.stringify(posted.room));
  must(posted.startTime==='9:00' && posted.endTime==='10:00', 'times mangled: '+posted.startTime+'..'+posted.endTime);
  must(logs.length && logs[logs.length-1].indexOf('2 students')>=0, 'success not logged as a batch');

  console.log('check-admin-ui: all admin UI checks passed');
})().catch(function(e){ console.error('FAIL: ' + (e && e.stack || e)); process.exit(1); });
`;

// One script, one scope: the page's top-level let bindings stay visible to the
// driver. Separating these into different evals would hide them, which is how
// the first version of this check reported a false failure.
const combined = shim + stub + script + driver;
vm.runInThisContext(combined, { filename: 'admin-ui-check.js' });
