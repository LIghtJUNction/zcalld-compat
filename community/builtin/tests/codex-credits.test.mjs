import { test, expect } from 'bun:test'
import { createCodex } from '../codex-impl.mjs'
import { codexQuotaWindows, createCodexCreditGuard } from '../codex-credits.mjs'

const WHEN = 1900000000000
const account = (id = 'fixture-account') => ({
  type: 'oauth', refreshOwner: 'host', accountId: id, expires: WHEN + 3600000,
  access: 'fixture.' + Buffer.from(JSON.stringify({ exp: WHEN / 1000 + 3600,
    'https://api.openai.com/auth': { chatgpt_account_id: id } })).toString('base64url') + '.fixture',
})
const policy = (restricted = true, assertCurrent = () => {}) =>
  ({ codex_no_credits: restricted, assertCurrent })
const usage = (used = 10, reset = WHEN / 1000 + 300, secondary = null) => ({
  rate_limit: { allowed: used < 100, limit_reached: used >= 100,
    primary_window: { used_percent: used, limit_window_seconds: 300, reset_at: reset },
    secondary_window: secondary },
  credits: { has_credits: true, unlimited: true, balance: '1000000' },
})
const reply = data => new Response(JSON.stringify(data), { headers: { 'content-type': 'application/json' } })
const generated = () => new Response('event: response.completed\ndata: ' + JSON.stringify({
  type: 'response.completed', response: { id: 'fixture-response', object: 'response', status: 'completed', output: [] },
}) + '\n\n', { headers: { 'content-type': 'text/event-stream' } })
const request = (p = policy(), signal) => ({ method: 'POST', headers: {},
  body: JSON.stringify({ model: 'fixture-model', input: 'hi', stream: false }), signal,
  ...(p === undefined ? {} : { zcalldAccountPolicy: p }) })
const URL = 'https://chatgpt.com/backend-api/codex/responses'
const USAGE = 'https://chatgpt.com/backend-api/wham/usage'
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
async function drain(response) { await response.text(); return response.status }

test('unrestricted accounts do not check usage and private policy never reaches fetch', async () => {
  const calls = []
  const adapter = createCodex({ now: () => WHEN, fetcher: async (url, init) => {
    calls.push({ url: String(url), init })
    expect(Object.hasOwn(init, 'zcalldAccountPolicy')).toBe(false)
    return generated()
  } })
  const loader = await adapter.auth.loader(async () => account())
  expect(await drain(await loader.fetch(URL, request(policy(false))))).toBe(200)
  expect(calls.map(c => c.url)).toEqual([URL])
})

test('restricted requests check the fixed official usage endpoint once per short cache', async () => {
  const calls = []
  const adapter = createCodex({ now: () => WHEN, fetcher: async (url, init) => {
    calls.push(String(url))
    expect(Object.hasOwn(init, 'zcalldAccountPolicy')).toBe(false)
    expect(init.redirect).toBe('error')
    expect(init.credentials).toBe('omit')
    if (String(url) === USAGE) {
      expect(init.method).toBe('GET')
      expect(init.body).toBeUndefined()
      expect(init.headers.get('chatgpt-account-id')).toBe('fixture-account')
      return reply(usage())
    }
    expect(String(url)).toBe(URL)
    return generated()
  } })
  const loader = await adapter.auth.loader(async () => account())
  expect(await drain(await loader.fetch(URL, request()))).toBe(200)
  expect(await drain(await loader.fetch(URL, request()))).toBe(200)
  expect(calls).toEqual([USAGE, URL, URL])
})

test('exhausted windows reject before generation even when credits are unlimited', async () => {
  let posts = 0
  const adapter = createCodex({ now: () => WHEN, fetcher: async (url) => {
    if (String(url) === USAGE) return reply(usage(100, WHEN / 1000 + 300,
      { used_percent: 105, limit_window_seconds: 604800, reset_at: WHEN / 1000 + 900 }))
    posts++; return generated()
  } })
  const loader = await adapter.auth.loader(async () => account())
  const response = await loader.fetch(URL, request())
  expect(response.status).toBe(429)
  expect(response.headers.get('retry-after')).toBe('900')
  expect((await response.json()).error.code).toBe('codex_subscription_exhausted')
  expect(posts).toBe(0)
})

test('full unknown-reset windows hold; unknown availability and stale resets never allow', () => {
  const unknownReset = usage(100)
  delete unknownReset.rate_limit.primary_window.reset_at
  expect(codexQuotaWindows(unknownReset, WHEN)).toMatchObject({ held: true, reset_at: null })
  const partial = usage(10)
  delete partial.rate_limit.primary_window.reset_at
  expect(codexQuotaWindows(partial, WHEN).known).toBe(false)
  expect(codexQuotaWindows(usage(10, WHEN / 1000 - 1), WHEN).known).toBe(false)
  expect(codexQuotaWindows(usage(10, WHEN / 1000 + 3), WHEN).windows[0].resets_at).toBe(WHEN / 1000 + 3)
})

test('missing, malformed, expired, over-limit and failed usage fail closed', async () => {
  const cases = [
    () => reply({ credits: { unlimited: true } }),
    () => reply({ rate_limit: { primary_window: { used_percent: '0', limit_window_seconds: 300 } } }),
    () => reply(usage(0, WHEN / 1000 - 1)),
    () => new Response('x'.repeat(65537), { headers: { 'content-type': 'application/json' } }),
    () => new Response('{"error":"PRIVATE_CANARY"}', { status: 401, headers: { 'content-type': 'application/json' } }),
    () => new Response('{}', { status: 302, headers: { location: 'https://unrelated.invalid/' } }),
  ]
  for (const make of cases) {
    let posts = 0
    const adapter = createCodex({ now: () => WHEN, fetcher: async (url) => {
      if (String(url) === USAGE) return make()
      posts++; return generated()
    } })
    const loader = await adapter.auth.loader(async () => account())
    const response = await loader.fetch(URL, request())
    expect(response.status).toBe(503)
    expect(await response.text()).not.toContain('PRIVATE_CANARY')
    expect(posts).toBe(0)
  }
})

test('expired cache and elapsed reset are rechecked without a poller or a generation retry', async () => {
  let clock = WHEN, gets = 0, posts = 0
  const adapter = createCodex({ now: () => clock, fetcher: async (url) => {
    if (String(url) === USAGE) return reply(usage(++gets === 1 ? 100 : 0, Math.floor(clock / 1000) + 2))
    posts++; return generated()
  } })
  const loader = await adapter.auth.loader(async () => account())
  expect(await drain(await loader.fetch(URL, request()))).toBe(429)
  clock += 500
  expect(await drain(await loader.fetch(URL, request()))).toBe(429)
  expect(gets).toBe(1)
  clock += 2000
  expect(await drain(await loader.fetch(URL, request()))).toBe(200)
  expect(gets).toBe(2)
  expect(posts).toBe(1)
})

test('actual official account identities isolate quota caches', async () => {
  const seen = []
  const adapter = createCodex({ now: () => WHEN, fetcher: async (url, init) => {
    if (String(url) !== USAGE) return generated()
    const id = init.headers.get('chatgpt-account-id')
    seen.push(id)
    return reply(usage(id === 'account-a' ? 100 : 10))
  } })
  const a = await adapter.auth.loader(async () => account('account-a'))
  const b = await adapter.auth.loader(async () => account('account-b'))
  expect(await drain(await a.fetch(URL, request()))).toBe(429)
  expect(await drain(await b.fetch(URL, request()))).toBe(200)
  expect(seen).toEqual(['account-a', 'account-b'])
})

test('one cancelled waiter does not cancel another request sharing the quota check', async () => {
  const ready = deferred()
  let gets = 0, sharedSignal
  const guard = createCodexCreditGuard({ now: () => WHEN, readUsage: async (_, opts) => {
    gets++; sharedSignal = opts.signal
    await ready.promise
    return reply(usage())
  } })
  const a = new AbortController(), b = new AbortController()
  const first = guard.check(account(), 'fixture-account', a.signal)
  const second = guard.check(account(), 'fixture-account', b.signal)
  a.abort()
  await expect(first).rejects.toThrow()
  expect(sharedSignal.aborted).toBe(false)
  ready.resolve()
  expect(await second).toBe(null)
  expect(gets).toBe(1)
})

test('the last cancelled waiter aborts its own flight and a later request can succeed', async () => {
  let gets = 0, aborted = 0
  const guard = createCodexCreditGuard({ now: () => WHEN, readUsage: async (_, opts) => {
    gets++
    if (gets === 1) await new Promise((_, reject) => opts.signal.addEventListener('abort', () => {
      aborted++; reject(new DOMException('cancelled', 'AbortError'))
    }, { once: true }))
    return reply(usage())
  } })
  const ctl = new AbortController()
  const first = guard.check(account(), 'fixture-account', ctl.signal)
  ctl.abort()
  await expect(first).rejects.toThrow()
  expect(await guard.check(account(), 'fixture-account')).toBe(null)
  expect(gets).toBe(2)
  expect(aborted).toBe(1)
})

test('a quota read cannot authorize a different account after an in-flight account change', async () => {
  let current = account('account-a'), posts = 0
  const adapter = createCodex({ now: () => WHEN, fetcher: async (url) => {
    if (String(url) === USAGE) { current = account('account-b'); return reply(usage()) }
    posts++; return generated()
  } })
  const loader = await adapter.auth.loader(async () => current)
  await expect(loader.fetch(URL, request())).rejects.toThrow('account changed')
  expect(posts).toBe(0)
})

test('a policy changed during quota lookup is rejected immediately before POST', async () => {
  let changed = false, posts = 0
  const adapter = createCodex({ now: () => WHEN, fetcher: async (url) => {
    if (String(url) === USAGE) { changed = true; return reply(usage()) }
    posts++; return generated()
  } })
  const loader = await adapter.auth.loader(async () => account())
  await expect(loader.fetch(URL, request(policy(true, () => {
    if (changed) throw new Error('policy changed')
  })))).rejects.toThrow('policy changed')
  expect(posts).toBe(0)
})

test('new account-wide exhausted headers win over an older in-flight available usage response', async () => {
  const ready = deferred()
  const guard = createCodexCreditGuard({ now: () => WHEN, readUsage: async () => {
    await ready.promise; return reply(usage())
  } })
  const check = guard.check(account(), 'fixture-account')
  guard.observe('fixture-account', new Headers({
    'x-codex-primary-used-percent': '100',
    'x-codex-primary-window-minutes': '5',
    'x-codex-primary-reset-at': String(WHEN / 1000 + 100),
  }))
  ready.resolve()
  expect((await check).status).toBe(429)
})

test('unknown named families and incomplete non-full headers cannot authorize or hold another model', async () => {
  let gets = 0
  const guard = createCodexCreditGuard({ now: () => WHEN, readUsage: async () => { gets++; return reply(usage()) } })
  guard.observe('fixture-account', new Headers({
    'x-codex-unrelated-v2-primary-used-percent': '100',
    'x-codex-unrelated-v2-primary-window-minutes': '5',
    'x-codex-unrelated-v2-primary-reset-at': String(WHEN / 1000 + 100),
    'x-codex-primary-used-percent': '10',
  }))
  expect(await guard.check(account(), 'fixture-account')).toBe(null)
  expect(gets).toBe(1)
})

test('metadata remains an explicit read-only operation outside the generation guard', async () => {
  let calls = 0
  const adapter = createCodex({ now: () => WHEN, fetcher: async url => {
    expect(String(url)).toBe(USAGE); calls++; return reply(usage(100))
  } })
  const loader = await adapter.auth.loader(async () => account())
  expect(await drain(await loader.fetch(URL, request()))).toBe(429)
  const metadata = await adapter.auth.codexMetadata(account(), { operation: 'usage', account_hint: 'fixture-account' })
  expect(metadata.status).toBe(200)
  expect((await metadata.json()).rate_limit.primary_window.used_percent).toBe(100)
  expect(calls).toBe(2)
})

test('malformed trusted policy is rejected before any fetch', async () => {
  let calls = 0
  const adapter = createCodex({ now: () => WHEN, fetcher: async () => { calls++; return generated() } })
  const loader = await adapter.auth.loader(async () => account())
  for (const value of [null, {}, { codex_no_credits: 'false', assertCurrent() {} }])
    await expect(loader.fetch(URL, request(value))).rejects.toThrow('trusted Codex account policy')
  expect(calls).toBe(0)
})
