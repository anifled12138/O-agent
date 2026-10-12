import assert from 'node:assert/strict';
import {mkdir} from 'node:fs/promises';
import {pathToFileURL,fileURLToPath} from 'node:url';
const {chromium}=await import(process.env.O_PLAYWRIGHT_MODULE?pathToFileURL(process.env.O_PLAYWRIGHT_MODULE).href:'playwright');
const browser=await chromium.launch({headless:true,...(process.env.O_CHROME_PATH?{executablePath:process.env.O_CHROME_PATH}:{})});
const origin=process.env.O_UI_TEST_URL||'http://127.0.0.1:5188',output=process.env.O_UI_SCREENSHOTS||fileURLToPath(new URL('../../.tmp/account-ui/',import.meta.url));
await mkdir(output,{recursive:true}); const errors=[]; let latestPage;
async function fixture({cloud=false,remoteOnly=false}={}) {
 const context=await browser.newContext({viewport:{width:1280,height:900}}),page=await context.newPage();
 latestPage=page; page.on('pageerror',e=>errors.push(e.message));
 const s={account:{state:'signed_out',serverUrl:'',deviceName:'My PC',pendingRevocations:0},loginFails:false,logoutFails:false,approval:false,approvePersists:false,mutations:[],conversation:null,task:null};
 if(!cloud) await page.addInitScript(()=>{window.oDesktop={apiOrigin:location.origin,platform:'win32',version:'test',openExternal:async url=>{window.openedLoginURLs??=[];window.openedLoginURLs.push(url);}};});
 await page.route('**/api/**',async route=>{
  const req=route.request(),path=new URL(req.url()).pathname.replace(/^\/api\/v[12]/,'');
  if(req.method()!=='GET') s.mutations.push({path,body:req.postDataJSON()});
  let body=[];
  if(path==='/auth/status') body={authenticationRequired:cloud,setupRequired:false,registrationAvailable:false,turnstileSiteKey:''};
  else if(path==='/auth/session') body={user:{id:'owner',email:'owner@example.test'}};
  else if(path==='/account') body=s.account;
  else if(path==='/account/login'){
   if(s.loginFails)return route.fulfill({status:503,json:{error:'login temporarily unavailable'}});
   s.account={...s.account,state:'awaiting_login',...req.postDataJSON(),verificationUrl:'https://account.example.test/?device_login=public-code'};body=s.account;
  }else if(path==='/account/logout'){
   if(s.logoutFails)return route.fulfill({status:503,json:{error:'logout persistence failed'}});
   s.account={state:'signed_out',serverUrl:s.account.serverUrl,deviceName:s.account.deviceName,pendingRevocations:1};body=s.account;
  }else if(path==='/auth/device/approval')body={name:'My PC',platform:'windows',approved:s.approval};
  else if(path==='/auth/device/approve'){s.approval=s.approvePersists;body={status:'approved'};}
  else if(path==='/providers')body=remoteOnly?[]:[{id:'p1',name:'Local model',model:'local-model',hasApiKey:true,contextWindow:100000}];
  else if(path==='/plugins')body=[{id:'core:conversation_title',type:'core',status:'disabled'}];
  else if(path==='/nodes')body=[{id:'node1',name:'My PC',platform:'windows',connectivity:'connected',resources:{maxConcurrentTasks:1},catalog:{models:[{id:'local1',name:'PC provider',model:'pc-model'}],permissionProfiles:['read_only','workspace_autonomous']}}];
  else if(path==='/tasks')body=s.task?[s.task]:[];
  else if(path==='/conversations'&&req.method()==='POST'){s.conversation={id:'new',title:'New',providerId:'device-reference',permissionProfile:'workspace_autonomous',messages:[]};body=s.conversation;}
  else if(path==='/conversations')body=s.conversation?[s.conversation]:[];
  else if(path==='/conversations/new')body=s.conversation;
  else if(path==='/conversations/new/tasks/nodes'){s.task={id:'task1',nodeId:'node1',status:'queued',payload:{sourceConversationId:'new',...req.postDataJSON()}};body={task:s.task,created:true};}
  else if(path==='/tasks/task1')body=s.task;
  return route.fulfill({json:body});
 });return{context,page,s};
}
try{
 const{context,page,s}=await fixture();await page.goto(origin);
 const composer=page.getByRole('textbox',{name:'输入消息内容'});await composer.waitFor();assert(await composer.isEnabled());
 await page.getByRole('button',{name:'账户与远程连接',exact:true}).click();
 const dialog=page.getByRole('dialog',{name:'账户与远程连接'});
 await dialog.getByLabel('服务器地址').fill('https://account.example.test');await dialog.getByLabel('这台电脑的名称').fill('My PC');
 s.loginFails=true;await dialog.getByRole('button',{name:'登录账户',exact:true}).click();
 await dialog.getByRole('alert').filter({hasText:'login temporarily unavailable'}).waitFor();
 assert.equal(await dialog.getByLabel('服务器地址').inputValue(),'https://account.example.test');
 await dialog.getByLabel('这台电脑的名称').focus();await page.waitForTimeout(2200);
 assert(await dialog.getByLabel('这台电脑的名称').evaluate(el=>el===document.activeElement),'poll must preserve focus');
 s.loginFails=false;await dialog.getByRole('button',{name:'登录账户',exact:true}).click();
 await page.waitForFunction(()=>window.openedLoginURLs?.length===1);
 assert.equal(await page.evaluate(()=>window.openedLoginURLs[0]),'https://account.example.test/?device_login=public-code');
 assert(!JSON.stringify(s.mutations).includes('password'));
 s.account={state:'connected',serverUrl:'https://account.example.test',deviceName:'My PC',user:{email:'owner@example.test',displayName:'Owner'},pendingRevocations:0};
 await dialog.getByText('Owner · owner@example.test',{exact:true}).waitFor();await page.screenshot({path:output+'/account-desktop.png'});
 s.logoutFails=true;await dialog.getByRole('button',{name:'退出账户并断开远程连接'}).click();
 await dialog.getByRole('alert').filter({hasText:'logout persistence failed'}).waitFor();
 assert(await dialog.getByText('Owner · owner@example.test',{exact:true}).isVisible());
 s.logoutFails=false;await dialog.getByRole('button',{name:'退出账户并断开远程连接'}).click();
 await dialog.getByText('本机已断开。服务器上的旧设备凭据正在撤销，网络恢复后会继续处理。').waitFor();
 await page.setViewportSize({width:375,height:812});await page.screenshot({path:output+'/account-mobile.png'});
 assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
 await dialog.getByRole('button',{name:'关闭',exact:true}).click();assert(await composer.isEnabled());await context.close();
 const a=await fixture({cloud:true});await a.page.goto(origin+'/?device_login=public-code');
 await a.page.getByRole('button',{name:'确认连接',exact:true}).click();
 await a.page.getByRole('alert').filter({hasText:'服务器未确认设备授权'}).waitFor();
 assert.equal(await a.page.getByRole('heading',{name:'设备已授权'}).count(),0);
 a.s.approvePersists=true;await a.page.getByRole('button',{name:'确认连接',exact:true}).click();
 await a.page.getByRole('heading',{name:'设备已授权'}).waitFor();await a.page.screenshot({path:output+'/device-approval.png'});await a.context.close();
 const r=await fixture({cloud:true,remoteOnly:true});await r.page.goto(origin);
 await r.page.getByRole('button',{name:'执行位置 当前服务节点'}).click();await r.page.getByRole('button',{name:'My PC windows',exact:true}).click();
 await r.page.getByRole('button',{name:'选择模型，当前 pc-model'}).waitFor();await r.page.getByRole('textbox',{name:'输入消息内容'}).fill('Use the PC model');
 await r.page.getByRole('button',{name:'发送',exact:true}).click();await r.page.getByRole('button',{name:'停止会话',exact:true}).waitFor();
 const c=r.s.mutations.find(x=>x.path==='/conversations'),d=r.s.mutations.find(x=>x.path==='/conversations/new/tasks/nodes');
 assert.equal(c.body.targetNodeId,'node1');assert.equal(c.body.localProviderId,'local1');assert(!c.body.providerId);
 assert.equal(d.body.targetNodeId,'node1');assert.equal(d.body.localProviderId,'local1');
 assert(!r.s.mutations.some(x=>x.path.includes('/turns')||x.path.includes('/tasks/cloud')));
 await r.page.screenshot({path:output+'/remote-only-model.png'});await r.context.close();assert.deepEqual(errors,[]);
 console.log('PASS: optional use, login failure/draft/focus, browser login, logout failure/read-back/offline, phone layout, approval read-back, device model without cloud model.');
}catch(error){if(latestPage){await latestPage.screenshot({path:output+'/failure.png'});console.log(await latestPage.locator('body').innerText());console.log(errors);}throw error;}finally{await browser.close();}
