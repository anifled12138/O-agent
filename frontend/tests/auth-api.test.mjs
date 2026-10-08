import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';

const clientURL = new URL('../app/api.ts', import.meta.url).href;
function failure(path, status, challengeRequired) {
 const code = `
  const events=[];
  globalThis.window={location:{origin:'https://o.example'},dispatchEvent:(event)=>events.push(event.type)};
  delete process.env.NEXT_PUBLIC_API_URL;
  globalThis.fetch=async()=>new Response(JSON.stringify({error:'test auth error',challengeRequired:${challengeRequired}}),{status:${status},headers:{'Content-Type':'application/json'}});
  const {request,APIError}=await import(${JSON.stringify(clientURL)});
  try{await request(${JSON.stringify(path)});}catch(error){
   process.stdout.write(JSON.stringify({typed:error instanceof APIError,status:error.status,challengeRequired:error.challengeRequired,events}));
  }
 `;
 return JSON.parse(execFileSync(process.execPath,['--experimental-strip-types','--no-warnings','--input-type=module','-e',code],{encoding:'utf8'}));
}
test('expired protected session notifies the account gate rather than retaining a private workspace',()=>{
 const result=failure('/projects',401,false);
 assert.deepEqual(result,{typed:true,status:401,challengeRequired:false,events:['o:authentication-required']});
});
test('login failure carries human challenge metadata without triggering session-expiry recursion',()=>{
 const result=failure('/auth/login',403,true);
 assert.deepEqual(result,{typed:true,status:403,challengeRequired:true,events:[]});
 const session=failure('/auth/session',401,false);
 assert.deepEqual(session.events,[]);
});
