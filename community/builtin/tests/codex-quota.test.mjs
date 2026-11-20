import { afterEach, expect, test } from 'bun:test'
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { createCodex } from '../codex-impl.mjs'

const NOW = 1900000000000
const dirs = []
afterEach(async () => { for (const dir of dirs.splice(0)) await rm(dir, { recursive: true, force: true }) })
function token(account = 'fixture-account', expires = NOW / 1000 + 3600, user = 'fixture-user') {
  return 'fixture.' + Buffer.from(JSON.stringify({ exp: expires, sub: user,
    'https://api.openai.com/auth': { chatgpt_account_id: account } })).toString('base64url') + '.signature'
}
function usage() {
  return { rate_limit: { allowed: true, limit_reached: false,
    primary_window: { used_percent: 37.5, limit_window_seconds: 18000, reset_at: NOW / 1000 + 3600 },
    secondary_window: { used_percent: 12, limit_window_seconds: 604800, reset_at: NOW / 1000 + 500000 } },
    credits: { balance: 'PRIVATE-NOT-A-CURRENCY', has_credits: true }, account: 'PRIVATE-ACCOUNT' }
}
async function fixture(mode = 'ok') {
  const dir = await mkdtemp(path.join(tmpdir(), 'zcalld-quota-')); dirs.push(dir)
  const file = path.join(dir, 'native-auth.json')
  const native = { tokens: { access_token: token(mode === 'wrong-account' ? 'other-account' : 'fixture-account',
    mode === 'expired' ? NOW / 1000 - 1 : NOW / 1000 + 3600),
    account_id: mode === 'wrong-account' ? 'other-account' : 'fixture-account', refresh_token: 'PRIVATE-NATIVE-REFRESH' } }
  await writeFile(file, JSON.stringify(native), { mode: 0o600 })
  const before = await readFile(file)
  const calls = []
  let aborted = false
  const adapter = createCodex({ now: () => NOW, fetcher: async (url, init) => {
    calls.push({ url, method: init.method })
    expect(url).toBe('https://chatgpt.com/backend-api/wham/usage')
    expect(init.method).toBe('GET')
    expect(init.redirect).toBe('error')
    expect(init.credentials).toBe('omit')
    expect(new Headers(init.headers).get('authorization')).toBe('Bearer ' + token())
    if (mode === 'wait') {
      await new Promise((resolve) => { const end = () => { aborted = true; resolve() }; if (init.signal.aborted) end(); else init.signal.addEventListener('abort', end, { once: true }) })
      throw new DOMException('cancelled', 'AbortError')
    }
    if (mode === 'changed') await writeFile(file, JSON.stringify({ tokens: { ...native.tokens, access_token: token('fixture-account', NOW / 1000 + 3600, 'other-user') } }), { mode: 0o600 })
    if (mode === 'redirect') return new Response('', { status: 302, headers: { location: 'https://never.invalid/' } })
    const body = mode === 'large' ? JSON.stringify({ ...usage(), padding: 'x'.repeat(65536) })
      : mode === 'malformed' ? JSON.stringify({ rate_limit: { primary_window: { used_percent: 'unknown' } } })
      : JSON.stringify(usage())
    return new Response(body, { headers: { 'content-type': 'application/json' } })
  } })
  return { file, before, calls, get aborted() { return aborted }, quota: adapter.auth.codexQuota,
    auth: { type: 'oauth', accountId: 'fixture-account', refreshOwner: 'external', sourceAuthFile: file,
      access: 'PRIVATE-STAGED-ACCESS', refresh: 'PRIVATE-STAGED-REFRESH', expires: 1 } }
}

test('explicit native usage borrows source without refresh or leaking account fields', async () => {
  const f = await fixture()
  const result = await f.quota(f.auth, { timeout_ms: 1000 })
  expect(result.ok).toBe(true)
  expect(result.windows.map(w => [w.name, w.used, w.span_seconds])).toEqual([['primary', 37.5, 18000], ['secondary', 12, 604800]])
  expect(f.calls).toHaveLength(1)
  expect(await readFile(f.file)).toEqual(f.before)
  expect(JSON.stringify(result)).not.toContain('PRIVATE')
  expect(JSON.stringify(result)).not.toContain('account')
})
for (const mode of ['expired', 'wrong-account', 'redirect', 'large', 'malformed', 'changed']) {
  test('native usage rejects ' + mode + ' without refresh or retry', async () => {
    const f = await fixture(mode)
    const result = await f.quota(f.auth, { timeout_ms: 1000 })
    expect(result.ok).toBe(false)
    expect(f.calls).toHaveLength(['expired', 'wrong-account'].includes(mode) ? 0 : 1)
    if (mode !== 'changed') expect(await readFile(f.file)).toEqual(f.before)
    expect(JSON.stringify(result)).not.toContain('PRIVATE')
    if (mode === 'changed') expect(result.error).toBe('account_or_plugin_changed')
  })
}
test('native usage cancellation cancels its one GET', async () => {
  const f = await fixture('wait')
  const controller = new AbortController()
  const result = f.quota(f.auth, { timeout_ms: 1000, signal: controller.signal })
  while (!f.calls.length) await new Promise(resolve => setTimeout(resolve, 1))
  controller.abort()
  expect((await result).ok).toBe(false)
  expect(f.calls).toHaveLength(1)
  expect(f.aborted).toBe(true)
  expect(await readFile(f.file)).toEqual(f.before)
})
