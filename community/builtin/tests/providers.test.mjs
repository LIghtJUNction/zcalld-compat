import { test, expect } from 'bun:test'
import { createCodex, collect as collectCodex } from '../codex-impl.mjs'
import { createCopilot } from '../copilot-impl.mjs'
import { createCodeAssist } from '../codeassist-impl.mjs'
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { spawn } from 'node:child_process'
import readline from 'node:readline'

const encoder = new TextEncoder()
function fragmentedSSE(events, newline = '\r\n') {
  const text = events.map((value) => `data: ${JSON.stringify(value)}${newline}${newline}`).join('')
  const bytes = encoder.encode(text)
  let offset = 0
  return new Response(new ReadableStream({ pull(controller) {
    if (offset === bytes.length) return controller.close()
    const end = Math.min(bytes.length, offset + [1, 3, 2, 11][offset % 4])
    controller.enqueue(bytes.slice(offset, end)); offset = end
  } }), { headers: { 'content-type': 'text/event-stream', 'x-fixture-metadata': 'kept' } })
}

test('Codex forces streaming and preserves the complete nonstream final object and account/session headers', async () => {
  let sent
  const final = { id: 'fixture-response', object: 'response', status: 'completed', model: 'fixture', service_tier: 'server-defined',
    output: [{ type: 'reasoning', encrypted_content: 'fixture-encrypted', summary: [{ type: 'summary_text', text: '想法' }] },
      { type: 'function_call', call_id: 'fixture-call', name: 'lookup', arguments: '{"q":"你好"}' }], usage: { input_tokens: 11, output_tokens: 7 }, fixture_extension: true }
  const plugin = createCodex({ fetcher: async (url, init) => { sent = { url, init, body: JSON.parse(init.body) }; return fragmentedSSE([
    { type: 'response.created', response: { id: 'fixture-response' } }, { type: 'response.output_text.delta', delta: 'ignored text reconstruction' },
    { type: 'response.completed', response: final }]) } })
  const loader = await plugin.auth.loader(async () => ({ access: 'fixture-access', accountId: 'fixture-account' }))
  const response = await loader.fetch('https://chatgpt.com/backend-api/codex/responses', {
    headers: { authorization: 'Bearer local-gateway-token', 'content-length': '1', session_id: 'wrong' },
    body: JSON.stringify({ model: 'fixture', input: 'hello', stream: false, store: true, prompt_cache_key: 'fixture-session',
      service_tier: 'requested-custom-tier', tool_choice: 'none', max_output_tokens: 12 }),
  })
  expect(await response.json()).toEqual(final)
  expect(response.headers.get('x-fixture-metadata')).toBe('kept')
  expect(sent.body.stream).toBe(true); expect(sent.body.store).toBe(false)
  expect(sent.body.service_tier).toBe('requested-custom-tier'); expect(sent.body.tool_choice).toBe('none')
  expect(sent.body.max_output_tokens).toBeUndefined()
  expect(sent.init.headers.get('authorization')).toBe('Bearer fixture-access')
  expect(sent.init.headers.get('chatgpt-account-id')).toBe('fixture-account')
  expect(sent.init.headers.get('session-id')).toBe('fixture-session'); expect(sent.init.headers.get('thread-id')).toBe('fixture-session')
  expect(sent.init.headers.get('session_id')).toBeNull(); expect(sent.init.headers.get('content-length')).toBeNull()
  expect(sent.init.redirect).toBe('error')
})

test('Codex streaming is byte preserving and incomplete streams never look like valid JSON replies', async () => {
  const original = 'event: response.output_text.delta\ndata: {"type":"response.output_text.delta","delta":"ok"}\n\n'
  const plugin = createCodex({ fetcher: async () => new Response(original, { headers: { 'content-type': 'text/event-stream' } }) })
  const loader = await plugin.auth.loader(async () => ({ access: 'fixture-access' }))
  const response = await loader.fetch('https://chatgpt.com/backend-api/codex/responses', { body: JSON.stringify({ model: 'fixture', input: [], stream: true }) })
  expect(await response.text()).toBe(original)
  await expect(collectCodex(fragmentedSSE([{ type: 'response.output_text.delta', delta: 'partial' }]))).rejects.toThrow('without a final response')
  await expect(loader.fetch('https://chatgpt.com/backend-api/codex/responses', { body: JSON.stringify({ model: 'fixture', input: [], previous_response_id: 'missing-state' }) })).rejects.toThrow('complete input history')
})

test('Codex refresh returns a rotated token without exposing provider response text', async () => {
  let body, signal
  const plugin = createCodex({ now: () => 1000000, fetcher: async (_url, init) => {
    body = JSON.parse(init.body); signal = init.signal
    return Response.json({ access_token: 'fixture-access-new', refresh_token: 'fixture-refresh-new', expires_in: 3600 })
  } })
  const result = await plugin.auth.refresh({ refresh: 'fixture-refresh-old', accountId: 'fixture-account' })
  expect(body.grant_type).toBe('refresh_token'); expect(result.refresh).toBe('fixture-refresh-new')
  expect(result.expires).toBe(4600000); expect(signal.aborted).toBe(false)
  const bad = createCodex({ fetcher: async () => Response.json({ error: 'invalid_grant', detail: 'do-not-log-fixture-token' }, { status: 400 }) })
  try { await bad.auth.refresh({ refresh: 'fixture-refresh-old' }); throw new Error('unexpected success') }
  catch (error) { expect(error.signIn).toBe('expired'); expect(error.message).not.toContain('do-not-log-fixture-token') }
})

test('externally managed Codex rereads native rotations and never calls the refresh endpoint', async () => {
  const dir = await mkdtemp(path.join(tmpdir(), 'zcalld-codex-external-'))
  const source = path.join(dir, 'auth.json')
  const now = 1000000
  const jwt = (label, exp = 4000) => `header.${Buffer.from(JSON.stringify({ exp, label })).toString('base64url')}.signature`
  const original = { tokens: { access_token: jwt('first'), refresh_token: 'fixture-native-refresh-first', account_id: 'fixture-native-account' } }
  await writeFile(source, JSON.stringify(original), { mode: 0o600 })
  const stored = { type: 'oauth', refreshOwner: 'external', sourceAuthFile: source,
    accountId: 'fixture-native-account', access: 'fixture-stale-access', refresh: 'fixture-never-use-this-refresh', expires: 1 }
  const seen = []
  const plugin = createCodex({ now: () => now, fetcher: async (url, init) => {
    expect(new URL(url).pathname).toBe('/backend-api/codex/responses')
    seen.push(init.headers.get('authorization'))
    return fragmentedSSE([{ type: 'response.completed', response: { id: 'fixture', output: [], usage: {} } }])
  } })
  try {
    const before = await readFile(source, 'utf8')
    const copied = await plugin.auth.refresh(stored)
    expect(copied.refresh).toBe('fixture-native-refresh-first')
    expect(await readFile(source, 'utf8')).toBe(before)
    expect(seen.length).toBe(0)
    const loader = await plugin.auth.loader(async () => stored)
    const call = () => loader.fetch('https://chatgpt.com/backend-api/codex/responses', { body: JSON.stringify({ model: 'fixture', input: 'hello', stream: false }) })
    await call()
    original.tokens.access_token = jwt('second'); original.tokens.refresh_token = 'fixture-native-refresh-second'
    await writeFile(source, JSON.stringify(original), { mode: 0o600 })
    await call()
    expect(seen).toEqual([`Bearer ${jwt('first')}`, `Bearer ${jwt('second')}`])
    original.tokens.account_id = 'fixture-different-account'
    await writeFile(source, JSON.stringify(original), { mode: 0o600 })
    await expect(call()).rejects.toThrow('source account changed')
    original.tokens.account_id = 'fixture-native-account'; original.tokens.access_token = jwt('expired', 1)
    await writeFile(source, JSON.stringify(original), { mode: 0o600 })
    await expect(call()).rejects.toThrow('native owner must renew')
    await expect(plugin.auth.refresh(stored)).rejects.toThrow('native owner must renew')
    expect(seen.length).toBe(2)
    expect(await readFile(source, 'utf8')).toBe(JSON.stringify(original))
  } finally { await rm(dir, { recursive: true, force: true }) }
})

test('Copilot shares one token exchange and preserves each model native endpoint', async () => {
  let exchanges = 0
  const requests = []
  const now = Date.now()
  const plugin = createCopilot({ now: () => now, fetcher: async (url, init) => {
    if (url.endsWith('/copilot_internal/v2/token')) {
      exchanges++; await new Promise((resolve) => setTimeout(resolve, 15))
      expect(init.headers.get('authorization')).toBe('token fixture-github')
      return Response.json({ token: 'fixture-session', expires_at: Math.floor(now / 1000) + 3600, endpoints: { api: 'https://api.githubcopilot.com' } })
    }
    requests.push({ url, headers: init.headers, body: JSON.parse(init.body) })
    return Response.json({ fixture: true })
  } })
  const loader = await plugin.auth.loader(async () => ({ access: 'fixture-github', tokenKind: 'editor' }), { models: {
    chat: { native_endpoints: ['/v1/chat/completions', '/v1/messages'] }, responses: { native_endpoints: ['/v1/responses'] },
  } })
  await Promise.all([
    loader.fetch('https://api.githubcopilot.com/v1/messages', { headers: { authorization: 'local-token' }, body: JSON.stringify({ model: 'chat', messages: [{ role: 'user', content: [{ type: 'tool_result', tool_use_id: 'a', content: 'ok' }] }] }) }),
    loader.fetch('https://api.githubcopilot.com/responses', { body: JSON.stringify({ model: 'responses', input: 'hello' }) }),
  ])
  expect(exchanges).toBe(1)
  expect(requests.map((r) => new URL(r.url).pathname).sort()).toEqual(['/responses', '/v1/messages'])
  for (const request of requests) expect(request.headers.get('authorization')).toBe('Bearer fixture-session')
  expect(requests.find((r) => r.body.model === 'chat').headers.get('x-initiator')).toBe('agent')
  expect(requests.find((r) => r.body.model === 'responses').headers.get('x-initiator')).toBe('user')
  await expect(loader.fetch('https://api.githubcopilot.com/v1/messages', { body: JSON.stringify({ model: 'responses', messages: [] }) })).rejects.toThrow('catalog does not support')
  expect(requests.length).toBe(2)
})

test('Code Assist lazily discovers a project and retains Gemini thought/tool/usage fields', async () => {
  const requests = []
  const plugin = createCodeAssist({ agent: 'antigravity', fetcher: async (url, init) => {
    requests.push({ url, body: JSON.parse(init.body) })
    if (url.endsWith(':loadCodeAssist')) return Response.json({ cloudaicompanionProject: { id: 'fixture-project' } })
    return fragmentedSSE([
      { response: { candidates: [{ index: 0, content: { role: 'model', parts: [{ thought: true, text: 'think', thoughtSignature: 'fixture-signature' }] } }] } },
      { response: { candidates: [{ index: 0, content: { role: 'model', parts: [{ functionCall: { name: 'lookup', args: { q: 'hello' } } }] }, finishReason: 'STOP' }], usageMetadata: { promptTokenCount: 5, candidatesTokenCount: 3 } } },
    ])
  } })
  expect(requests.length).toBe(0)
  const loader = await plugin.auth.loader(async () => ({ access: 'fixture-google' }))
  expect(requests.length).toBe(0)
  const response = await loader.fetch('https://daily-cloudcode-pa.googleapis.com/v1beta/models/fixture:generateContent', {
    body: JSON.stringify({ contents: [{ role: 'user', parts: [{ text: 'preserve this request' }] }], systemInstruction: { parts: [{ text: 'unaltered instructions' }] } }),
  })
  const result = await response.json()
  expect(requests.length).toBe(2); expect(requests[1].body.project).toBe('fixture-project')
  expect(requests[1].body.request.systemInstruction.parts[0].text).toBe('unaltered instructions')
  expect(result.candidates[0].content.parts[0].thoughtSignature).toBe('fixture-signature')
  expect(result.candidates[0].content.parts[1].functionCall.args).toEqual({ q: 'hello' })
  expect(result.usageMetadata.promptTokenCount).toBe(5)
})

test('Google renewal requires the matching private OAuth client identity', async () => {
  let requests = 0
  const plugin = createCodeAssist({ fetcher: async (_url, init) => {
    requests++
    expect(init.body.get('client_id')).toBe('fixture-client-id')
    expect(init.body.get('client_secret')).toBe('fixture-client-secret')
    return Response.json({ access_token: 'fixture-google-new', expires_in: 3600 })
  } })
  await expect(plugin.auth.refresh({ refresh: 'fixture-google-refresh' })).rejects.toThrow('client identity is missing')
  expect(requests).toBe(0)
  const updated = await plugin.auth.refresh({ refresh: 'fixture-google-refresh', clientId: 'fixture-client-id', clientSecret: 'fixture-client-secret' })
  expect(updated.access).toBe('fixture-google-new'); expect(requests).toBe(1)
})

class Host {
  constructor(script) {
    this.child = spawn(process.execPath, [script], { stdio: ['pipe', 'pipe', 'pipe'] })
    this.pending = new Map(); this.sequence = 0
    this.child.stderr.resume()
    this.lines = readline.createInterface({ input: this.child.stdout })
    this.lines.on('line', (line) => {
      let data; try { data = JSON.parse(line) } catch { return }
      if (!data.id || data.event) return
      const pending = this.pending.get(data.id)
      if (pending) { this.pending.delete(data.id); data.error ? pending.reject(new Error(data.error.message)) : pending.resolve(data.result) }
    })
    this.child.on('exit', () => { for (const pending of this.pending.values()) pending.reject(new Error('fixture host exited')); this.pending.clear() })
  }
  call(method, params) {
    const id = ++this.sequence
    return new Promise((resolve, reject) => { this.pending.set(id, { resolve, reject }); this.child.stdin.write(`${JSON.stringify({ id, method, params })}\n`) })
  }
  async close() {
    this.child.stdin.end()
    await Promise.race([new Promise((resolve) => this.child.once('exit', resolve)), new Promise((resolve) => setTimeout(resolve, 1000))])
    if (this.child.exitCode === null) this.child.kill()
    this.lines.close()
  }
}

test('two real Bun hosts serialize one rotating Codex refresh and persist its result', async () => {
  const dir = await mkdtemp(path.join(tmpdir(), 'zcalld-builtin-test-'))
  let exchanges = 0
  const server = Bun.serve({ hostname: '127.0.0.1', port: 0, async fetch(request) {
    expect(new URL(request.url).pathname).toBe('/token'); exchanges++
    await new Promise((resolve) => setTimeout(resolve, 80))
    return Response.json({ access_token: 'fixture-rotated-access', refresh_token: 'fixture-rotated-refresh', expires_in: 3600 })
  } })
  const module = path.resolve(import.meta.dir, '../codex-impl.mjs')
  const fixture = path.join(dir, 'provider.mjs')
  await writeFile(fixture, `import {createCodex} from ${JSON.stringify(module)}; export default async()=>createCodex({tokenURL:${JSON.stringify(`http://127.0.0.1:${server.port}/token`)},testMode:true});`)
  const authPath = path.join(dir, 'plugin-auth.json')
  await writeFile(authPath, JSON.stringify({ 'builtin:codex#fixture': { type: 'oauth', access: 'fixture-expired-access', refresh: 'fixture-old-refresh', expires: 1 } }), { mode: 0o600 })
  const hosts = [new Host(path.resolve(import.meta.dir, '../../vendor/host.js')), new Host(path.resolve(import.meta.dir, '../../vendor/host.js'))]
  try {
    const params = { authPath, directory: dir, plugins: [{ spec: 'fixture-builtin', target: fixture }], config: { provider: { 'builtin:codex': { npm: '@ai-sdk/openai', models: { fixture: { name: 'Fixture' } } } } } }
    await Promise.all(hosts.map((host) => host.call('init', params)))
    await Promise.all(hosts.map((host) => host.call('load', { provider: 'builtin:codex', account: 'builtin:codex#fixture' })))
    expect(exchanges).toBe(1)
    const saved = JSON.parse(await readFile(authPath, 'utf8'))['builtin:codex#fixture']
    expect(saved.refresh).toBe('fixture-rotated-refresh'); expect(saved.expires).toBeGreaterThan(Date.now())
  } finally {
    await Promise.all(hosts.map((host) => host.close())); server.stop(true); await rm(dir, { recursive: true, force: true })
  }
}, 10000)
