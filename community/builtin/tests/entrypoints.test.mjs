import { test, expect } from 'bun:test'
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { pathToFileURL } from 'node:url'
import { spawn } from 'node:child_process'
import readline from 'node:readline'

function environment(dir) {
  const env = {}
  for (const key of ['PATH', 'SystemRoot', 'SYSTEMROOT', 'WINDIR', 'COMSPEC', 'PATHEXT']) {
    if (process.env[key]) env[key] = process.env[key]
  }
  for (const key of ['HOME', 'USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'XDG_CONFIG_HOME',
    'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'XDG_STATE_HOME', 'CODEX_HOME', 'TMP', 'TEMP', 'TMPDIR']) env[key] = dir
  return env
}

class Host {
  constructor(script, dir) {
    this.child = spawn(process.execPath, [script], { cwd: dir, env: environment(dir), stdio: ['pipe', 'pipe', 'pipe'] })
    this.pending = new Map(); this.sequence = 0
    this.child.stderr.resume()
    this.lines = readline.createInterface({ input: this.child.stdout })
    this.lines.on('line', (line) => {
      let data; try { data = JSON.parse(line) } catch { return }
      const pending = this.pending.get(data.id)
      if (!pending) return
      if (data.event) { pending.events.push(data); return }
      this.pending.delete(data.id); clearTimeout(pending.timer)
      data.error ? pending.reject(new Error(data.error.message)) : pending.resolve({ result: data.result, events: pending.events })
    })
    this.child.on('exit', () => {
      for (const pending of this.pending.values()) { clearTimeout(pending.timer); pending.reject(new Error('fixture host exited')) }
      this.pending.clear()
    })
  }
  call(method, params) {
    const id = ++this.sequence
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error('fixture RPC deadline exceeded')) }, 5000)
      this.pending.set(id, { resolve, reject, events: [], timer })
      this.child.stdin.write(JSON.stringify({ id, method, params }) + '\n')
    })
  }
  async close() {
    if (this.child.exitCode === null) {
      this.child.stdin.end()
      await Promise.race([new Promise(resolve => this.child.once('exit', resolve)), new Promise(resolve => setTimeout(resolve, 1000))])
      if (this.child.exitCode === null) this.child.kill()
    }
    this.lines.close()
  }
}

async function fixture(run, entries = []) {
  const dir = await mkdtemp(path.join(tmpdir(), 'zcalld-builtin-entry-'))
  const authPath = path.join(dir, 'plugin-auth.json')
  await writeFile(authPath, '{}', { mode: 0o600 })
  const hostPath = path.resolve(import.meta.dir, '../../vendor/host.js')
  const guard = path.join(dir, 'guard.mjs')
  // Builtins use fetch for vendor traffic. Also block native socket creation,
  // so this initialization regression cannot contact a provider by accident.
  await writeFile(guard, `
import fs from 'node:fs'
import net from 'node:net'
import tls from 'node:tls'
const blocked = () => { fs.appendFileSync(${JSON.stringify(path.join(dir, 'network.log'))}, 'attempt\\n'); throw new Error('fixture forbids network') }
globalThis.fetch = blocked
globalThis.WebSocket = class { constructor() { blocked() } }
net.Socket.prototype.connect = blocked
net.connect = net.createConnection = tls.connect = blocked
Bun.connect = Bun.listen = Bun.serve = blocked
for (const file of ${JSON.stringify(entries)}) {
  const mod = await import(file)
  const functions = Object.entries(mod).filter(([,value]) => typeof value === 'function')
  if (functions.length !== 1 || functions[0][0] !== 'default') throw new Error('builtin entry must expose only its initializer')
}
await import(${JSON.stringify(pathToFileURL(hostPath).href)})
`)
  const host = new Host(guard, dir)
  try { await run({ host, dir, authPath }) }
  finally { await host.close(); await rm(dir, { recursive: true, force: true }) }
}

test('real builtin entrypoints initialize once and list empty MCP tools without credentials or network', async () => {
  const names = ['codex', 'copilot', 'codeassist', 'antigravity']
  const files = names.map(name => path.resolve(import.meta.dir, '../' + name + '.mjs'))
  await fixture(async ({ host, dir, authPath }) => {
    const { result } = await host.call('init', { authPath, directory: dir,
      plugins: files.map((target, i) => ({ spec: names[i], target })), config: {} })
    expect(result.plugins.length).toBe(4)
    for (const item of result.plugins) expect(item.error).toBeUndefined()
    expect(result.plugins.map(item => item.provides)).toEqual([
      ['builtin:codex'], ['builtin:copilot'], ['builtin:gemini'], ['builtin:antigravity'],
    ])
    const { events } = await host.call('mcpHttp', { requests: [{ id_json: '1', method: 'tools/list', params: {} }],
      batch: false, acl: { grants: [] }, window: 524288, timeout_ms: 3000, max_response_bytes: 65536 })
    expect(events.filter(item => item.event === 'head').map(item => item.status)).toEqual([200])
    const body = Buffer.concat(events.filter(item => item.event === 'chunk').map(item => Buffer.from(item.data, 'base64')))
    expect(JSON.parse(body.toString())).toEqual({ jsonrpc: '2.0', id: 1, result: { tools: [] } })
    expect(JSON.parse(await readFile(authPath, 'utf8'))).toEqual({})
    expect(await Bun.file(path.join(dir, 'network.log')).exists()).toBe(false)
  }, files.map(file => pathToFileURL(file).href))
}, 10000)

test('legacy community default and named initializers still load, with aliases deduplicated', async () => {
  await fixture(async ({ host, dir, authPath }) => {
    const target = path.join(dir, 'legacy.mjs')
    await writeFile(target, `
import { appendFileSync } from 'node:fs'
import { join } from 'node:path'
async function first({directory}) {
  appendFileSync(join(directory, 'calls.log'), 'first\\n')
  return { auth: { provider: 'fixture:first' } }
}
export { first as default, first as alias }
export async function second({directory}) {
  appendFileSync(join(directory, 'calls.log'), 'second\\n')
  return { auth: { provider: 'fixture:second' } }
}
`)
    const { result } = await host.call('init', { authPath, directory: dir, plugins: [{ spec: 'legacy', target }], config: {} })
    expect(result.plugins[0].error).toBeUndefined()
    expect(result.plugins[0].provides.sort()).toEqual(['fixture:first', 'fixture:second'])
    expect((await readFile(path.join(dir, 'calls.log'), 'utf8')).trim().split('\n').sort()).toEqual(['first', 'second'])
  })
}, 10000)
