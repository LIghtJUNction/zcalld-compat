import { describe, test, expect } from 'bun:test'
import { createCodexMaintenance } from '../codex-maintenance.mjs'

const ACCOUNT = 'builtin:codex#0123456789abcdefabcd'
const NOW = Date.parse('2026-11-20T10:00:00Z')
const END = new Date(NOW + 20 * 60000).toISOString()
const ID = '12345678-abcd-4abc-8abc-0123456789ab'
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } })
function setup(options = {}) {
  const requests = [], generations = []
  const quota = options.quota ?? { rate_limit: { allowed: true, primary_window: { used_percent: 5, limit_window_seconds: 18000, reset_at: NOW / 1000 + 1800 }, secondary_window: { used_percent: 0, limit_window_seconds: 604800, reset_at: NOW / 1000 + 604800 } }, rate_limit_reset_credits: { available_count: 1 } }
  const cards = options.cards ?? { available_count: 1, credits: [{ id: 'card-1', status: 'available', expires_at: END }] }
  const models = options.models ?? { models: [{ slug: 'standard' }, { slug: 'small-mini', supported_reasoning_levels: [{ effort: 'high' }, { effort: 'low' }] }] }
  const hook = createCodexMaintenance({
    now: () => NOW,
    accountID: a => a.accountId,
    externalAuth: async a => a,
    clientHeaders: h => h.set('originator', 'codex_cli_rs'),
    fetcher: async (url, init) => {
      requests.push({ url: String(url), init })
      expect(new URL(url).origin).toBe('https://chatgpt.com')
      expect(init.headers.get('chatgpt-account-id')).toBe('synthetic-account')
      if (init.method === 'POST') return options.post ? options.post(url, init) : json({ code: 'reset', windows_reset: ['primary'] })
      if (String(url).includes('/wham/usage')) return json(quota)
      if (String(url).includes('/codex/models')) return json(models)
      if (String(url).endsWith('/rate-limit-reset-credits')) return json(cards)
      throw new Error('unexpected fixed endpoint')
    },
  })
  const stored = { type: 'oauth', access: 'synthetic-token', expires: NOW + 3600000, accountId: 'synthetic-account', refreshOwner: 'host' }
  const trusted = {
    assertCurrent: options.assertCurrent || (() => {}), noCredits: true,
    generation: async (input, signal) => {
      generations.push(input)
      expect(signal.aborted).toBe(false)
      return options.generation ? options.generation(input, signal) : new Response('event: response.completed\ndata: {"type":"response.completed","response":{"status":"completed"}}\n\n', { headers: { 'content-type': 'text/event-stream' } })
    },
  }
  const call = (operation, fields = {}) => hook(stored, { account: ACCOUNT, operation, deadline_ms: NOW + 25000, ...fields }, trusted)
  return { call, requests, generations, stored, trusted, hook }
}

describe('fixed optional Codex maintenance', () => {
  test('fresh inspection proves windows and available credit expiry without exposing auth', async () => {
    const s = setup(), value = await s.call('inspect')
    expect(value.usage_known).toBe(true)
    expect(value.models_known).toBe(true)
    expect(value.reset_credits).toEqual({ count: 1, expires_at: END })
    expect(value.reset_cards).toEqual([{ id: 'card-1', expires_at: END }])
    expect(value.windows[1].resets_at).toBe(new Date(NOW + 604800000).toISOString())
    expect(JSON.stringify(value)).not.toContain('synthetic-token')
    expect(JSON.stringify(value)).not.toContain('synthetic-account')
    expect(s.requests.every(r => r.init.method === 'GET')).toBe(true)
  })
  test('count mismatch and unproved expiry omit reset-credit notification', async () => {
    const s = setup({ cards: { available_count: 2, credits: [{ id: 'card-1', status: 'available', expires_at: END }] } })
    const value = await s.call('inspect')
    expect(value.cards_complete).toBe(false)
    expect(value.reset_credits).toBeUndefined()
    const noExpiry = await setup({ cards: { available_count: 1, credits: [{ id: 'card-1', status: 'available', expires_at: null }] } }).call('inspect')
    expect(noExpiry.cards_complete).toBe(true)
    expect(noExpiry.reset_credits).toBeUndefined()
  })
  test('warmup uses only actual small catalog model and fixed prompt through host callback', async () => {
    const s = setup(), result = await s.call('warmup', { windows: ['secondary'] })
    expect(result).toEqual({ status: 'completed', code: 'warmed' })
    expect(s.generations).toEqual([{ model: 'small-mini', input: 'hi', stream: true, reasoning: { effort: 'low' } }])
    expect(s.requests.every(r => r.init.method === 'GET')).toBe(true)
  })
  test('unknown, exhausted and missing-model observations cannot generate', async () => {
    for (const options of [
      { quota: {} },
      { quota: { rate_limit: { limit_reached: true, secondary_window: { used_percent: 100, limit_window_seconds: 604800, reset_at: NOW / 1000 + 1000 } } } },
      { models: { models: [] } },
    ]) {
      const s = setup(options)
      expect((await s.call('warmup', { windows: ['secondary'] })).status).toBe('not_sent')
      expect(s.generations).toHaveLength(0)
    }
  })
  test('normal builtin NoCredits denial is proven not-sent; failed SSE remains unknown', async () => {
    const blocked = setup({ generation: () => json({ error: { type: 'codex_account_policy', code: 'codex_subscription_exhausted' } }, 429) })
    expect((await blocked.call('warmup', { windows: ['secondary'] })).status).toBe('not_sent')
    const failed = setup({ generation: () => new Response('data: {"type":"response.failed"}\n\n') })
    expect((await failed.call('warmup', { windows: ['secondary'] })).status).toBe('unknown')
  })
  test('consume pins exact card, expiry, idempotency id and fixed official POST body', async () => {
    const s = setup()
    const result = await s.call('consume', { credit_id: 'card-1', redeem_request_id: ID, expected_expires_at: END, lead_minutes: 30 })
    expect(result).toEqual({ status: 'completed', code: 'reset' })
    const posts = s.requests.filter(r => r.init.method === 'POST')
    expect(posts).toHaveLength(1)
    expect(posts[0].url).toBe('https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume')
    expect(JSON.parse(posts[0].init.body)).toEqual({ credit_id: 'card-1', redeem_request_id: ID })
    expect(posts[0].init.headers.get('content-type')).toBe('application/json')
  })
  test('a replaced or not-yet-due card cannot be silently substituted', async () => {
    for (const cards of [
      { available_count: 1, credits: [{ id: 'card-2', status: 'available', expires_at: END }] },
      { available_count: 1, credits: [{ id: 'card-1', status: 'available', expires_at: new Date(NOW + 86400000).toISOString() }] },
    ]) {
      const s = setup({ cards })
      expect((await s.call('consume', { credit_id: 'card-1', redeem_request_id: ID, expected_expires_at: END, lead_minutes: 30 })).status).toBe('not_sent')
      expect(s.requests.filter(r => r.init.method === 'POST')).toHaveLength(0)
    }
  })
  test('after-send failure is unknown and a caller cannot inject endpoints or prompts', async () => {
    const s = setup({ post: async () => { throw new Error('credential-canary') } })
    const result = await s.call('consume', { credit_id: 'card-1', redeem_request_id: ID, expected_expires_at: END, lead_minutes: 30 })
    expect(result).toEqual({ status: 'unknown', code: 'interrupted' })
    expect(JSON.stringify(result)).not.toContain('credential-canary')
    for (const field of ['url', 'prompt', 'model', 'policy']) {
      const clean = setup()
      await expect(clean.call('warmup', { windows: ['secondary'], [field]: 'injected' })).rejects.toThrow()
      expect(clean.requests).toHaveLength(0)
    }
  })
  test('cancelled or invalid authentication is rejected before metadata or generation', async () => {
    const s = setup(), ctl = new AbortController(); ctl.abort()
    await expect(s.call('warmup', { signal: ctl.signal, windows: ['secondary'] })).rejects.toThrow()
    expect(s.generations).toHaveLength(0)
    s.stored.expires = undefined
    await expect(s.call('inspect')).rejects.toThrow()
    expect(s.requests).toHaveLength(0)
  })
})
