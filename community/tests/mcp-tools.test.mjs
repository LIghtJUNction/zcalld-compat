import { test, expect } from "bun:test"
import { answerTools, namedTools, toolPrefix } from "../vendor/mcp-tools.mjs"

const plugin = {spec:"npm:@fixture/opencode-weather-auth@1.2.3",tools:[{name:"echo",description:"Repeat input",schema:{type:"object",properties:{text:{type:"string"}}}},{name:"other"}]}
const signal = () => new AbortController().signal
const row = (method,name="weather_echo",id="1") => ({id_json:id,method,params:{name,arguments:{text:"你好"}}})
const request = (rows, extra={}) => ({requests:rows,batch:false,max_response_bytes:1<<20,acl:{grants:[],concurrency:[]},...extra})

test("stable imported names, truncation and collisions",() => {
  expect(toolPrefix(plugin.spec)).toBe("weather")
  expect(toolPrefix("file:///tmp/tools-天气")).toBe("plugin")
  const long={spec:"opencode-"+"a".repeat(40),tools:[{name:"b".repeat(48)}]}
  const tools=namedTools([long,long])
  expect(tools[0].name.length).toBe(64)
  expect(tools[1].name.length).toBe(64)
  expect(tools[1].name.endsWith("_2")).toBe(true)
  expect(namedTools([{...plugin,error:"not enabled"}])).toEqual([])
})

test("per-plugin endpoint uses original names and first matching plugin",() => {
  expect(namedTools([plugin,plugin],"weather").map(x=>x.name)).toEqual(["echo","other"])
  expect(namedTools([plugin],"missing")).toEqual([])
})

test("scope intersection filters list and refuses call without invoking",async() => {
  let calls=0
  const invoke=async()=>{calls++;return {text:"wrong"}}
  const acl={grants:[["magpie-tools"],["magpie-tools/weather_echo"]],concurrency:[]}
  const list=JSON.parse((await answerTools(request([row("tools/list")],{acl}),[plugin],invoke,signal())).toString())
  expect(list.result.tools.map(t=>t.name)).toEqual(["weather_echo"])
  const denied=JSON.parse((await answerTools(request([row("tools/call","weather_other")],{acl}),[plugin],invoke,signal())).toString())
  expect(denied.error.code).toBe(-32602);expect(calls).toBe(0)
})

test("tool execution preserves raw id, arguments and exact returned text",async()=>{
  let received
  const body=(await answerTools(request([row("tools/call","weather_echo","9007199254740993")]),[plugin],async(p)=>{received=p;return {text:"你好\nλ"}},signal())).toString()
  expect(body.includes('"id":9007199254740993')).toBe(true)
  expect(JSON.parse(body).result.content[0].text).toBe("你好\nλ")
  expect(received.spec).toBe(plugin.spec);expect(received.name).toBe("echo");expect(received.args.text).toBe("你好")
})

test("unknown tool is a protocol error and thrown tool is a tool error",async()=>{
  let calls=0;const invoke=async()=>{calls++;throw new Error("fixture failure")}
  const a=JSON.parse((await answerTools(request([row("tools/call","missing")]),[plugin],invoke,signal())).toString())
  expect(a.error.code).toBe(-32602);expect(calls).toBe(0)
  const b=JSON.parse((await answerTools(request([row("tools/call")]),[plugin],invoke,signal())).toString())
  expect(b.result.isError).toBe(true);expect(b.result.content[0].text).toBe("fixture failure");expect(calls).toBe(1)
})

test("batch preserves precomputed reply and order",async()=>{
  const first='{"jsonrpc":"2.0","id":12345678901234567890,"result":{}}'
  const b=(await answerTools(request([{reply_json:first},row("tools/list")],{batch:true}),[plugin],async()=>({text:""}),signal())).toString()
  expect(b.startsWith(`[${first},`)).toBe(true);expect(JSON.parse(b).length).toBe(2)
})

test("response bound refuses oversized output and never retries a tool",async()=>{
  let calls=0
  await expect(answerTools(request([row("tools/call")],{max_response_bytes:512}),[plugin],async()=>{calls++;return {text:"x".repeat(1024)}},signal())).rejects.toThrow("configured limit")
  expect(calls).toBe(1)
})

test("caller concurrency queue abort does not execute or starve next request",async()=>{
  let release,started;const ready=new Promise(r=>started=r), hold=new Promise(r=>release=r)
  const acl={grants:[],concurrency:[{id:"fixture-queue",max:1}]}
  let calls=0
  const invoke=async()=>{calls++;if(calls===1){started();await hold}return {text:"ok"}}
  const first=answerTools(request([row("tools/call")],{acl}),[plugin],invoke,signal());await ready
  const ctl=new AbortController()
  const cancelled=answerTools(request([row("tools/call")],{acl}),[plugin],invoke,ctl.signal)
  ctl.abort();await expect(cancelled).rejects.toThrow("cancelled")
  release();await first
  await answerTools(request([row("tools/call")],{acl}),[plugin],invoke,signal())
  expect(calls).toBe(2)
})
