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
const login = fs.readFileSync(path.join(root, 'internal/proxy/static/login.html'), 'utf8');

const open = html.indexOf('<script>');
const close = html.lastIndexOf('</script>');
if (open < 0 || close < 0 || close < open) {
  console.error('check-admin-ui: no <script> block found in ' + page);
  process.exit(2);
}
let script = html.slice(open + '<script>'.length, close);

for (const legacy of ['<th>Boosted</th>', 'boosted — raw teacher-account forwarding', "setPerm(u,f,v)", "card('Boosted'"]) {
  if (html.indexOf(legacy) >= 0) {
    console.error('check-admin-ui: legacy permission control is still visible: ' + legacy);
    process.exit(1);
  }
}
for (const expected of ['Create permission groups', 'Default access by account type', 'Assign groups to people', 'simpleGroupTemplate', 'Advanced permission settings', 'Manage access']) {
  if (html.indexOf(expected) < 0) {
    console.error('check-admin-ui: new permissions workflow is missing: ' + expected);
    process.exit(1);
  }
}
for (const expected of ['autocomplete="one-time-code"', 'type="password"', 'Continue securely', '/admin/setup', '/admin/logout']) {
  if (login.indexOf(expected) < 0) {
    console.error('check-admin-ui: safer first-run login is missing: ' + expected);
    process.exit(1);
  }
}
if (login.indexOf('localStorage') >= 0 || !html.includes('function logoutAdmin()')) {
  console.error('check-admin-ui: login must not persist credentials and dashboard sign-out must be wired');
  process.exit(1);
}

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
var window = { addEventListener: function(){} };
var setTimeout = function(){ return 0; };
var clearTimeout = function(){};
`;

// Stub the two endpoints the pickers read and the events POST, so the check is
// offline and deterministic. The recorded POST body is what the driver asserts.
const stub = `
var posted=null, roomPosted=null, logs=[];
function jsonResponse(data){ return {ok:true,status:200,json:async()=>data,text:async()=>JSON.stringify(data)}; }
var fetch = async function(url, opts){
  if(url.indexOf('/admin/rooms')>=0){
    if(opts && opts.method==='POST'){ roomPosted=JSON.parse(opts.body); return jsonResponse({ok:true,school:'schuldorf',rooms:roomPosted.rooms,texts:[]}); }
    return jsonResponse({school:'schuldorf',rooms:{},texts:['Aula','Turnhalle']});
  }
  if(opts && opts.method==='POST'){ posted=JSON.parse(opts.body); return jsonResponse({created:[{eventId:1,username:'dee'},{eventId:2,username:'sam'}]}); }
  if(url.indexOf('/admin/users')>=0) return jsonResponse({users:[
    {username:'dee',displayName:'Dee',personType:5,school:'schuldorf',className:'10a'},
    {username:'sam',displayName:'',personType:5,school:'schuldorf',className:'10a'},
    {username:'wils',displayName:'W',personType:2,school:'schuldorf',className:''},
    {username:'other',personType:5,school:'elsewhere',className:'9z'}]});
  if(url.indexOf('type=ROOM')>=0) return jsonResponse({results:[
    {type:'ROOM',id:1,name:'R12'},{type:'ROOM',id:2,name:'R13'},{type:'ROOM',id:3,name:'Turnhalle'}]});
  return jsonResponse({events:[],results:[]});
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

  // Room map: the event-side room text is free text (suggested from the import's
  // texts), the id comes from the room picker, and add / update saves the pair. An
  // earlier UI keyed the map on the picker's room name, so an imported text like
  // "Aula" could not be mapped at all.
  await loadRoomMap();
  must($('rmSrcList').options.length===2, 'event room texts not offered as suggestions: '+$('rmSrcList').options.length);
  $('rmSrcIn').value='Aula';
  $('rmElVal').value='169';
  await addRoomMap();
  must(roomPosted && roomPosted.rooms && roomPosted.rooms['Aula']===169,
    'the room map did not post the import text -> room id: '+JSON.stringify(roomPosted));
  must($('rmSrcIn').value==='', 'the source field was not cleared after add');

  // Escaping: a webhook URL is chosen by whoever created the subscription, so it
  // must reach the admin page as text, never as HTML. Interpolating it raw let a
  // signed-in student run script in the admin's session when the dashboard
  // listed the webhook. The same applies to usernames in the users table.
  fetch = async function(url){
    if(url.indexOf('/admin/webhooks')>=0) return jsonResponse({webhooks:[
      {id:1,school:'schuldorf',elementType:'CLASS',elementId:1,name:'10a',
       url:'"><img src=x onerror=alert(1)>',createdBy:'<b>evil</b>'}]});
    return jsonResponse({});
  };
  await loadWebhooks();
  const cell = _sel('__q').children[0]._html;
  must(cell.indexOf('<img')<0, 'a webhook URL reached the page as raw HTML: '+cell);
  must(cell.indexOf('&lt;img')>=0, 'the webhook URL was not escaped: '+cell);
  must(cell.indexOf('<b>evil</b>')<0, 'createdBy reached the page as raw HTML: '+cell);
  must(jsq("a'b" + String.fromCharCode(34) + "</td>").indexOf('<')<0
    && jsq("a'b" + String.fromCharCode(34)).indexOf("'")<0
    && jsq("a'b" + String.fromCharCode(34)).indexOf(String.fromCharCode(34))<0,
    'jsq does not neutralise quotes or tags for an inline handler');

  // Anonymous access matrix is deployment-wide: four global capability rows,
  // each rendered from the flags the API returned. Pooled classes come first so
  // the fallback that most affects what users see is not buried.
  anonFeatures={classes:false,ntfy:false,webhooks:false,changes:false};
  renderAnonymous();
  let anonRows=_sel('__q').children;
  must(anonRows.length===4, 'expected 4 anonymous capability rows, got '+anonRows.length);
  must(anonRows[0]._html.indexOf('setAnonFeature')>=0, 'anonymous classes toggle missing');
  must(anonRows[0]._html.indexOf('classes')>=0, 'the first anonymous row is not pooled classes');
  must(anonRows[1]._html.indexOf('setAnonFeature')>=0, 'anonymous ntfy toggle missing');
  must(anonRows[1]._html.indexOf('>off<')>=0, 'disabled capability did not render off');
  anonFeatures={classes:true,ntfy:true,webhooks:false,changes:true};
  renderAnonymous();
  anonRows=_sel('__q').children;
  must(anonRows[0]._html.indexOf('>on<')>=0, 'enabled pooled classes did not render on');
  must(anonRows[1]._html.indexOf('>on<')>=0, 'enabled capability did not render on');
  must(anonRows[3]._html.indexOf('>on<')>=0, 'enabled live-changes capability did not render on');
  must(anonRows[0]._html.indexOf('setAnonFeature')>=0, 'anonymous toggles disappeared after reload');

  // The former one-toggle-per-feature UI has been replaced by named groups and
  // account-type defaults. Confirm internal capability names render in plain
  // language in the UI summary.
  must(simpleGroupNames['v2.boosted']==='Extended timetable access', 'extended timetable group has a legacy-facing label');
  must(simpleCapabilityLabel('timetable.teacher.boosted_raw.view')==='View teacher timetables through a teacher account',
    'raw staff timetable access has no plain-language label');

  // Registration warnings describe saved evidence, not guessed authentication
  // failures. Parent-like accounts without a class need review, not deletion.
  must(accountAuthenticationLabel({credentialStatusKnown:true,hasSharedSecret:true,hasPassword:true}).startsWith('Both'),'dual credential label missing');
  must(accountAuthenticationLabel({credentialStatusKnown:true,hasSharedSecret:true})==='Token/shared secret only','token-only label incorrect');
  must(accountAuthenticationLabel({credentialStatusKnown:true,hasPassword:true})==='Password only','password-only label incorrect');
  must(accountAuthenticationLabel({credentialStatusKnown:true})==='None saved','missing credentials incorrectly labelled');
  must(accountAuthenticationLabel({})==='Not checked','unknown credential state incorrectly labelled');
  const ready={username:'ready',school:'schuldorf',personId:5,personType:5,classId:10,replayable:true};
  must(accountRegistrationWarnings(ready).length===0,'complete student incorrectly flagged');
  must(accountRegistrationWarnings({...ready,replayable:false}).some(i=>i.code==='credential'),'missing reusable credential not flagged');
  const parentIssues=accountRegistrationWarnings({...ready,classId:-1});
  must(parentIssues.length===1&&parentIssues[0].code==='class','parent-like class warning incorrect');
  const guardian={...ready,classId:-1,personType:0,accountLinks:{guardian:true,persons:[{id:10,type:5,displayName:'<child>'},{id:20,type:5},{id:30,type:5}]}};
  must(accountRegistrationWarnings(guardian).length===0,'recognised guardian incorrectly flagged as classless student');
  must(accountTypeLabel(guardian).includes('3 linked children'),'linked child count missing');
  must(accountRegistrationWarnings({...ready,personType:0}).some(i=>i.code==='type'),'unknown account type not flagged');
  allUsers=[ready,{...ready,username:'<img src=x>',replayable:false,password:'never-render-this-secret'}];
  renderAccountWarnings();
  must($('accountWarningCount').textContent==='1','warning count incorrect');
  must($('accountWarningRows').innerHTML.includes('&lt;img'),'warning account name not escaped');
  must(!$('accountWarningRows').innerHTML.includes('never-render-this-secret'),'credential leaked into warning page');
  $('accountWarningSearch').value='notfound';renderAccountWarnings();
  must($('accountWarningRows').innerHTML.includes('No warnings match'),'warning search did not filter accounts');
  $('accountWarningSearch').value='';

  // Every supported capability appears exactly once in the individual editor.
  const keys=permissionSections.flatMap(([,items])=>items.map(([key])=>key));
  must(new Set(keys).size===keys.length,'permission editor contains duplicate rules');
  must(keys.includes('notify.ntfy.create')&&keys.includes('calendar.student.personal.mint'),'notification/calendar choices missing');
  const canonical=${JSON.stringify(fs.readFileSync(path.join(root,'internal/access/access.go'),'utf8'))};
  const capBlock=canonical.slice(canonical.indexOf('// Timetable viewing'),canonical.indexOf('// AllCapabilities'));
  const backendKeys=[...capBlock.matchAll(/Capability = "([^"]+)"/g)].map(m=>m[1]);
  must(backendKeys.length===keys.length&&backendKeys.every(key=>keys.includes(key)),'editor does not cover the backend capability list');
  let savedGroup=null;
  fetch=async function(url,opts={}){
    if(url.endsWith('/access/group-editor')){savedGroup=JSON.parse(opts.body);return jsonResponse({ok:true,id:77});}
    if(url.indexOf('/access/role-defaults')>=0)return jsonResponse({roleDefaults:{}});
    if(url.indexOf('/access/groups?')>=0)return jsonResponse({groups:[]});
    return jsonResponse({});
  };
  allUsers=[];
  $('simpleAccessSchool').value='schuldorf';$('simpleGroupName').value='Calendar only';$('permissionGroupId').value='0';$('permissionGroupAudience').value='';
  $('permission-notify.ntfy.create').value='-1';$('permission-calendar.student.personal.mint').value='1';
  await createSimpleGroup();
  must(savedGroup&&savedGroup.name==='Calendar only','editor did not save group');
  must(savedGroup.grants['notify.ntfy.create']===-1&&savedGroup.grants['calendar.student.personal.mint']===1,'individual permission choices were not preserved');
  must($('permissionGroupMessage').textContent.includes('Saved'),'save feedback missing');

  // Removing a school's group must never delete the inherited global row.
  const defaultRequests=[];
  fetch=async function(url,opts={}){
    defaultRequests.push({url,method:opts.method||'GET'});
    if((opts.method||'GET')==='DELETE')return jsonResponse({ok:true});
    return jsonResponse({roleDefaults:{student:[1]}});
  };
  $('simpleAccessSchool').value='schuldorf';
  window.simpleAccessGroups=[{ID:1,Name:'v2.student_base'}];
  await toggleSimpleRoleGroup('student',77,false);
  const deletes=defaultRequests.filter(r=>r.method==='DELETE');
  must(deletes.length===1&&deletes[0].url.includes('school=schuldorf&'),'school default removal touched global scope');
  must($('simpleRoleDefaults').innerHTML.includes('disabled'),'inherited global default was rendered editable');
  must($('simpleRoleDefaults').innerHTML.includes('Shared default'),'inherited default has no explanation');
  must($('roleDefaultMessage').textContent.includes('schuldorf'),'default save feedback missing school context');

  let userLoads=0,accessLoads=0;
  fetch=async function(url){if(url.includes('/admin/users'))userLoads++;if(url.includes('/access/'))accessLoads++;return jsonResponse({users:[]});};
  await loadUsers(false);
  must(userLoads===1&&accessLoads===0,'user refresh performed a redundant access reload');

  // Repeated clicks share the same refresh; a failure must not lock later refreshes.
  const originalRefresh = refreshDashboard;
  let releaseRefresh, refreshCalls=0;
  refreshDashboard = () => { refreshCalls++; return new Promise(resolve => { releaseRefresh=resolve; }); };
  const firstRefresh=refreshAll(), secondRefresh=refreshAll();
  must(firstRefresh===secondRefresh && refreshCalls===1, 'overlapping refresh duplicated requests');
  releaseRefresh(); await firstRefresh;
  refreshDashboard = async () => { refreshCalls++; throw new Error('offline'); };
  try { await refreshAll(); } catch(e) {}
  refreshDashboard = async () => { refreshCalls++; };
  await refreshAll();
  must(refreshCalls===3, 'failed refresh blocked retry');
  refreshDashboard = originalRefresh;


  // Preview sends unsaved group rules to the read-only endpoint, never to save.
  $('permissionGroupId').value='0';$('permissionGroupAudience').value='student';
  const previewCalls=[];
  api=async (method,path,body)=>{previewCalls.push({method,path,body});return {users:[]};};
  await previewPermissionGroup();
  must(previewCalls.length===1&&previewCalls[0].path==='access/preview','preview used a mutation endpoint');
  must(previewCalls[0].body.assignTo==='student','preview lost audience');
  const capKeys=permissionSections.flatMap(([,rows])=>rows.map(([key])=>key));
  for(const key of ['analytics.personal.view','analytics.personal.history','analytics.teacher.view','analytics.class_history.view','analytics.export','events.technik.create','events.technik.attendance.write'])must(capKeys.includes(key),'new permission missing '+key);

  const originalUserReload=loadSimpleUserAccess;
  loadSimpleUserAccess=async()=>{};
  window.personInheritance={school:'testschool',user:'dee',inherit:true,excludedGroupIds:[12]};
  const inheritanceCalls=[];
  api=async(method,path,body)=>{inheritanceCalls.push({method,path,body});return {ok:true};};
  await savePersonInheritance(null,12,true);
  must(inheritanceCalls[0].method==='PUT'&&inheritanceCalls[0].path.startsWith('access/default-group-policy?'),'inheritance control used wrong endpoint');
  must(inheritanceCalls[0].body.inherit===true&&inheritanceCalls[0].body.excludedGroupIds.length===0,'restoring inherited group failed');
  await savePersonInheritance(false,null,null);
  must(inheritanceCalls[1].body.inherit===false,'automatic inheritance off not sent');
  loadSimpleUserAccess=originalUserReload;
  must(formatPersonCompatibility({storedRole:'teacher',upstreamAdmin:null}).includes('Not reported by upstream'),'unknown upstream administrator mislabelled');
  console.log('check-admin-ui: all admin UI checks passed');
})().catch(function(e){ console.error('FAIL: ' + (e && e.stack || e)); process.exit(1); });
`;

// One script, one scope: the page's top-level let bindings stay visible to the
// driver. Separating these into different evals would hide them, which is how
// the first version of this check reported a false failure.
const combined = shim + stub + script + driver;
vm.runInThisContext(combined, { filename: 'admin-ui-check.js' });
