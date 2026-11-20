import { test, expect } from 'bun:test'
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { createCodex } from '../codex-impl.mjs'

const now = () => 1_900_000_000_000
const jwt = (account='fixture-account', seconds=900) =>
  `fixture.${Buffer.from(JSON.stringify({ exp: now()/1000+seconds,
    'https://api.openai.com/auth': { chatgpt_account_id: account } })).toString('base64url')}.fixture`
const auth = () => ({type:'oauth',refreshOwner:'host',access:jwt(),refresh:'must-never-be-used',accountId:'fixture-account',expires:now()+900000})
const request = (operation='usage', extra={}) => ({operation,client_version:'0.170.0',models:[{route:'public-alias',model:'actual-slug'}],timeout_ms:1000,max_response_bytes:1<<20,...extra})
const json = (value, status=200, headers={}) => new Response(JSON.stringify(value),{status,headers:{'content-type':'application/json',...headers}})
const hook = (fetcher) => createCodex({fetcher,now}).auth.codexMetadata
const model = {slug:'actual-slug',display_name:'Official model',base_instructions:'Official instructions with {{personality}}',
  model_messages:{instructions_template:'upstream template',instructions_variables:{personality_default:'official'}},
  supported_reasoning_levels:[{effort:'high',description:'Official'}],shell_type:'shell_command',
  visibility:'list',supported_in_api:true,priority:7,support_verbosity:true,
  truncation_policy:{mode:'bytes',limit:100000},experimental_supported_tools:['web'],
  opaque_extension:{nested:['untouched',12]}}

test('four metadata operations are exact fixed-origin GETs without renewal', async()=>{
  const seen=[]
  const run=hook(async(url,init)=>{
    seen.push(String(url))
    expect(init.method).toBe('GET')
    expect(init.body).toBeUndefined()
    expect(init.redirect).toBe('error')
    expect(init.credentials).toBe('omit')
    expect(init.headers.get('authorization')).toBe(`Bearer ${auth().access}`)
    expect(init.headers.get('chatgpt-account-id')).toBe('fixture-account')
    expect(init.headers.get('cookie')).toBeNull()
    return json(String(url).includes('/codex/models')?{models:[model]}:{actual:true})
  })
  for(const operation of ['models','usage','accounts','config']) {
    const response=await run(auth(),request(operation))
    expect(response.status).toBe(200)
  }
  expect(seen).toEqual([
    'https://chatgpt.com/backend-api/codex/models?client_version=0.170.0',
    'https://chatgpt.com/backend-api/wham/usage',
    'https://chatgpt.com/backend-api/wham/accounts/check',
    'https://chatgpt.com/backend-api/wham/config/bundle',
  ])
})
test('model mapping preserves official instructions/capabilities and advertises no unknown model', async()=>{
  const run=hook(async()=>json({models:[model,{...model,slug:'unconfigured'}],extension:'retained'},200,{'etag':'upstream','set-cookie':'private'}))
  const response=await run(auth(),request('models',{models:[{route:'public-alias',model:'actual-slug'},{route:'unknown',model:'missing'}]}))
  expect(response.status).toBe(200)
  const data=await response.json()
  expect(data).toEqual({models:[{...model,slug:'public-alias'}],extension:'retained'})
  expect(response.headers.get('etag')).toMatch(/^"zcalld-[a-f0-9]{64}"$/)
  expect(response.headers.get('set-cookie')).toBeNull()
  expect(response.headers.get('authorization')).toBeNull()
})
test('model upgrades use an advertised local alias and preserve upstream guidance', async()=>{
  const upgrade={model:'next-upstream',migration_config:{high:'high'},message:'Official upgrade guidance'}
  const source={...model,upgrade}
  const target={...model,slug:'next-upstream',base_instructions:'Next official instructions'}
  const response=await hook(async()=>json({models:[source,target]}))(auth(),request('models',{
    models:[{route:'local-old',model:'actual-slug'},{route:'local-next',model:'next-upstream'}]
  }))
  expect(response.status).toBe(200)
  const data=await response.json()
  expect(data.models.map(entry=>entry.slug)).toEqual(['local-old','local-next'])
  expect(data.models[0].upgrade).toEqual({...upgrade,model:'local-next'})
  expect(data.models[0].base_instructions).toBe(model.base_instructions)
  expect(data.models[1].base_instructions).toBe('Next official instructions')
  expect(source.upgrade).toEqual(upgrade)
})
test('model upgrades require an unambiguous advertised target', async()=>{
  const source={...model,upgrade:{model:'next-upstream',message:'Official'}}
  const target={...model,slug:'next-upstream'}
  const cases=[
    {extra:[],official:[source,target],expected:null},
    {extra:[{route:'missing-official',model:'next-upstream'}],official:[source],expected:null},
    {extra:[{route:'next-a',model:'next-upstream'},{route:'next-b',model:'next-upstream'}],official:[source,target],expected:null},
    {extra:[{route:'next-a',model:'next-upstream'},{route:'next-upstream',model:'next-upstream'}],official:[source,target],expected:{...source.upgrade}}
  ]
  for(const {extra,official,expected} of cases){
    const response=await hook(async()=>json({models:official}))(auth(),request('models',{
      models:[{route:'local-old',model:'actual-slug'},...extra]
    }))
    expect(response.status).toBe(200)
    const data=await response.json()
    expect(data.models[0].upgrade).toEqual(expected)
    if(expected)expect(data.models.some(entry=>entry.slug===expected.model)).toBe(true)
  }
})

test('metadata never fabricates missing instructions or malformed catalog', async()=>{
  for(const value of [{models:[{slug:'actual-slug'}]},{models:[model,model]},{models:null}]) {
    const result=await hook(async()=>json(value))(auth(),request('models'))
    expect(result.status).toBe(502)
    expect(await result.text()).not.toContain('must-never-be-used')
  }
})
test('bootstrap and usage retain exact upstream JSON bytes and optional ETag', async()=>{
  const raw=' { "actual": 9007199254740993, "unknown": [true, null] } \n'
  const result=await hook(async()=>new Response(raw,{headers:{'content-type':'application/json; charset=utf-8','etag':'"official"','set-cookie':'secret'}}))(auth(),request('accounts'))
  expect(await result.text()).toBe(raw)
  expect(result.headers.get('etag')).toBe('"official"')
  expect(result.headers.get('set-cookie')).toBeNull()
})
test('unsupported URL/operation/query never reaches the network', async()=>{
  let calls=0
  const run=hook(async()=>{calls++;return json({})})
  for(const req of [request('responses'),request('__proto__'),request('toString'),
      request('usage',{url:'https://evil.invalid/'}),request('models',{client_version:'x&account=other'})]) {
    expect((await run(auth(),req)).status).toBe(400)
  }
  expect(calls).toBe(0)
})
test('redirect and upstream authentication failure do not mutate credentials', async()=>{
  const stored=auth(), before=JSON.stringify(stored)
  let cancelled=0
  const redirect=hook(async()=>new Response(new ReadableStream({cancel(){cancelled++}}),{status:302,headers:{location:'https://evil.invalid/'}}))
  expect((await redirect(stored,request())).status).toBe(502)
  const rejected=await hook(async()=>json({secret:'must-not-be-echoed'},401))(stored,request())
  expect(rejected.status).toBe(401)
  expect(await rejected.text()).not.toContain('must-not-be-echoed')
  expect(JSON.stringify(stored)).toBe(before)
  expect(cancelled).toBe(1)
})
test('external credentials are re-read, remain read-only and reject account changes/expiry', async()=>{
  const root=await mkdtemp(path.join(tmpdir(),'zcalld-codex-metadata-'))
  const source=path.join(root,'auth.json')
  let calls=0
  const run=hook(async()=>{calls++;return json({ok:true})})
  const stored={...auth(),refreshOwner:'external',sourceAuthFile:source}
  try {
    const save=async(account,seconds)=>writeFile(source,JSON.stringify({tokens:{access_token:jwt(account,seconds),refresh_token:'owner-only',account_id:account}}),{mode:0o600})
    await save('fixture-account',900)
    const before=await readFile(source)
    expect((await run(stored,request())).status).toBe(200)
    expect(await readFile(source)).toEqual(before)
    await save('another-account',900)
    expect((await run(stored,request())).status).toBe(401)
    await save('fixture-account',-1)
    expect((await run(stored,request())).status).toBe(401)
    expect(calls).toBe(1)
    expect(stored.refresh).toBe('must-never-be-used')
  } finally { await rm(root,{recursive:true,force:true}) }
})
test('expired host-owned access does not renew or make an upstream request', async()=>{
  let calls=0
  const stored={...auth(),access:jwt('fixture-account',-1),expires:now()-1000}
  expect((await hook(async()=>{calls++;return json({})})(stored,request())).status).toBe(401)
  expect(calls).toBe(0)
})
test('oversized or non-JSON successful metadata is cancelled and rejected', async()=>{
  let cancelled=0
  const run=hook(async()=>new Response(new ReadableStream({
    pull(c){c.enqueue(new TextEncoder().encode(' '.repeat(100)))},
    cancel(){cancelled++}
  }),{headers:{'content-type':'application/json'}}))
  expect((await run(auth(),request('usage',{max_response_bytes:50}))).status).toBe(502)
  expect(cancelled).toBe(1)
  expect((await hook(async()=>new Response('data: {}\n\n',{headers:{'content-type':'text/event-stream'}}))(auth(),request())).status).toBe(502)
})
test('overall deadline and explicit cancel release delayed fetch/body without retry', async()=>{
  let calls=0,cancelled=0
  const run=hook(async()=>{
    calls++
    await Bun.sleep(40)
    return new Response(new ReadableStream({cancel(){cancelled++}}),{headers:{'content-type':'application/json'}})
  })
  expect((await run(auth(),request('usage',{timeout_ms:10}))).status).toBe(504)
  await Bun.sleep(50)
  expect(calls).toBe(1);expect(cancelled).toBe(1)
  const controller=new AbortController()
  const readerRun=hook(async()=>new Response(new ReadableStream({
    pull(){return new Promise(()=>{})},cancel(){cancelled++}
  }),{headers:{'content-type':'application/json'}}))
  const waiting=readerRun(auth(),request('usage',{signal:controller.signal}))
  setTimeout(()=>controller.abort(),10)
  expect((await waiting).status).toBe(504)
  expect(cancelled).toBe(2)
})

test('account hint only rejects mismatch; it never selects another credential', async()=>{
  let calls=0
  const run=hook(async()=>{calls++;return json({ok:true})})
  expect((await run(auth(),request('usage',{account_hint:'fixture-account'}))).status).toBe(200)
  const wrong=await run(auth(),request('usage',{account_hint:'different-account'}))
  expect(wrong.status).toBe(403)
  expect(await wrong.text()).not.toContain('different-account')
  for(const hint of ['',null,[],['fixture-account'], 'bad\nvalue',' fixture-account ']) {
    expect((await run(auth(),request('usage',{account_hint:hint}))).status).toBe(400)
  }
  expect(calls).toBe(1)
})
