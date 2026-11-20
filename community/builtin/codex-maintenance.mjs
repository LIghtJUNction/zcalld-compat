// Optional one-shot maintenance. URLs, prompt and account selection are not RPC
// inputs. The trusted host supplies the existing fetchAs path for generation.
import { object, secret, expiry, fetchSafe } from './common.mjs'
import { codexQuotaIdentity, codexQuotaWindows } from './codex-credits.mjs'

const ORIGIN = 'https://chatgpt.com'
const PATHS = Object.freeze({
  usage: '/backend-api/wham/usage',
  models: '/backend-api/codex/models?client_version=0.170.0',
  credits: '/backend-api/wham/rate-limit-reset-credits',
  consume: '/backend-api/wham/rate-limit-reset-credits/consume',
})
const ACCOUNT = /^builtin:codex#[a-f0-9]{20}$/u
const UUID = /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/u
const METHODS = new Set(['inspect', 'warmup', 'consume'])
const LIMIT = 64 << 10

function failure() { return new Error('Codex maintenance could not verify the selected account') }
function abort(signal) { return signal.reason instanceof Error ? signal.reason : new DOMException('Cancelled', 'AbortError') }
async function bounded(promise, signal, late = () => {}) {
  if (signal.aborted) { promise.then(late, () => {}); throw abort(signal) }
  let stop
  const cancelled = new Promise((_, reject) => {
    stop = () => reject(abort(signal))
    signal.addEventListener('abort', stop, { once: true })
  })
  try { return await Promise.race([promise, cancelled]) }
  catch (error) { if (signal.aborted) promise.then(late, () => {}); throw error }
  finally { signal.removeEventListener('abort', stop) }
}
async function body(response, signal, maximum = LIMIT) {
  const reader = response.body?.getReader()
  if (!reader) throw failure()
  const parts = []
  let size = 0
  try {
    while (true) {
      const { value, done } = await bounded(reader.read(), signal)
      if (done) break
      size += value.byteLength
      if (size > maximum) throw failure()
      parts.push(value)
    }
    const bytes = new Uint8Array(size)
    let offset = 0
    for (const part of parts) { bytes.set(part, offset); offset += part.byteLength }
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (error) { void reader.cancel().catch(() => {}); throw error }
  finally { reader.releaseLock() }
}
async function jsonBody(response, signal, maximum = LIMIT) {
  if (!response.ok || !/^application\/(?:json|[a-z0-9.+-]+\+json)(?:\s*;|$)/iu.test(response.headers.get('content-type') || '')) {
    void response.body?.cancel().catch(() => {})
    throw failure()
  }
  return object(JSON.parse(await body(response, signal, maximum)))
}
function iso(seconds) {
  return Number.isSafeInteger(seconds) && seconds > 0 && seconds < 8640000000000
    ? new Date(seconds * 1000).toISOString() : null
}
function expiryDate(value) {
  if (value === null || value === undefined || value === '') return null
  if (typeof value !== 'string' || !/^\d{4}-\d\d-\d\dT/iu.test(value)) throw failure()
  const date = Date.parse(value)
  if (!Number.isFinite(date) || date <= 0) throw failure()
  return new Date(date).toISOString()
}
function resetCards(value, usageCount) {
  if (!Number.isSafeInteger(usageCount) || usageCount < 0 || usageCount > 128) return null
  if (!Number.isSafeInteger(value.available_count) || value.available_count !== usageCount || !Array.isArray(value.credits) || value.credits.length > 1024) return null
  const cards = [], seen = new Set()
  for (const card of value.credits) {
    if (card?.status !== 'available') continue
    if (typeof card.id !== 'string' || !card.id || card.id.length > 512 || /[\u0000-\u001f\u007f]/u.test(card.id) || seen.has(card.id)) return null
    seen.add(card.id)
    cards.push({ id: card.id, expires_at: expiryDate(card.expires_at) })
  }
  if (cards.length !== usageCount) return null
  return cards
}
function chooseModel(value) {
  if (!Array.isArray(value.models) || !value.models.length || value.models.length > 4096) return null
  const models = value.models.filter(m => m && typeof m.slug === 'string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$/u.test(m.slug))
  if (models.length !== value.models.length || new Set(models.map(m => m.slug)).size !== models.length) return null
  // Prefer the small family only when this account actually advertised it.
  // No inferred price or model name is introduced into the request.
  const model = models.find(m => /(?:mini|luna)/iu.test(m.slug)) || models[0]
  const levels = Array.isArray(model.supported_reasoning_levels)
    ? model.supported_reasoning_levels.map(x => x?.effort).filter(x => typeof x === 'string' && /^[a-z]{1,16}$/u.test(x)) : []
  return { name: model.slug, effort: levels.includes('low') ? 'low' : levels[0] }
}
function eligible(snapshot, card, lead, nowMs) {
  if (!snapshot.usage_known || !snapshot.cards_complete || !card?.expires_at) return false
  const expires = Date.parse(card.expires_at)
  if (!Number.isFinite(expires) || expires <= nowMs) return false
  const windows = snapshot.windows.filter(w => !w.aside)
  if (!windows.some(w => w.used > 0)) return false
  const threshold = expires - lead * 60000
  if (threshold <= nowMs) return true
  return windows.some(w => w.used >= 100 && !w.model && (!w.resets_at || Date.parse(w.resets_at) >= threshold))
}

function onCredits(usage) {
  const credits = usage?.credits
  if (!credits || typeof credits !== 'object' || Array.isArray(credits)) return false
  if (credits.unlimited === true) return true
  if (credits.has_credits !== true) return false
  if (credits.balance === undefined || credits.balance === null) return true
  if (typeof credits.balance !== 'string') return false
  const value = credits.balance.trim()
  if (!value) return true
  if (/^nan$/iu.test(value)) return false
  if (/^\+?(?:inf|infinity)$/iu.test(value)) return true
  if (/^-(?:inf|infinity)$/iu.test(value)) return false
  const number = Number(value)
  // The source's CodexGoesOnCredits treats a balance it cannot parse as
  // available when has_credits is explicit; a numeric zero/debt is not.
  return Number.isNaN(number) || number > 0
}

function weeklyEnd(snapshot, trigger, nowMs) {
  if (!snapshot.usage_known ||
      !['before_credits', 'routes_exhausted'].includes(trigger) ||
      (trigger === 'before_credits' && (snapshot.no_credits || !snapshot.on_credits))) return null
  const ends = snapshot.windows.filter(w => !w.aside && !w.model && w.span_seconds >= 86400 &&
    w.used >= 100 && w.resets_at && Date.parse(w.resets_at) > nowMs).map(w => Date.parse(w.resets_at))
  return ends.length ? new Date(Math.max(...ends)).toISOString() : null
}

export function createCodexMaintenance({ fetcher, externalAuth, accountID, clientHeaders, now = Date.now }) {
  return async function maintenance(stored, request, trusted) {
    object(request)
    if (!METHODS.has(request.operation) || !ACCOUNT.test(request.account || '') ||
        ['url', 'prompt', 'model', 'policy', 'no_credits'].some(k => Object.hasOwn(request, k)) ||
        typeof trusted?.assertCurrent !== 'function') throw failure()
    const remaining = request.deadline_ms - now()
    if (!Number.isFinite(remaining) || remaining <= 0 || remaining > 60000) throw failure()
    const signals = [AbortSignal.timeout(Math.ceil(remaining))]
    if (request.signal) signals.push(request.signal)
    const signal = AbortSignal.any(signals)
    if (signal.aborted) throw abort(signal)
    let auth = object(stored)
    const ownership = value => JSON.stringify([value.type, value.refreshOwner ?? null, value.sourceAuthFile ?? null])
    const initialOwnership = ownership(auth)
    trusted.assertCurrent()
    if (auth.type !== 'oauth' || !['external', 'host', undefined].includes(auth.refreshOwner)) throw failure()
    if (auth.refreshOwner === 'external') auth = await bounded(externalAuth(auth, now()), signal)
    const expires = expiry(auth.access, 0, now()) || Number(auth.expires)
    if (!Number.isFinite(expires) || expires <= now()) throw failure()
    const actualAccount = secret(accountID(auth), 'bound Codex account')
    const initialIdentity = codexQuotaIdentity(auth, actualAccount)
    const headers = new Headers({ accept: 'application/json', authorization: `Bearer ${secret(auth.access, 'Codex access token')}`,
      'chatgpt-account-id': actualAccount })
    clientHeaders(headers, '0.170.0')
    async function read(operation, maximum = LIMIT) {
      trusted.assertCurrent()
      if (signal.aborted) throw abort(signal)
      const one = AbortSignal.any([signal, AbortSignal.timeout(8000)])
      const response = await bounded(fetchSafe(fetcher, ORIGIN + PATHS[operation], { method: 'GET', headers, signal: one }), one,
        late => { void late?.body?.cancel().catch(() => {}) })
      return await jsonBody(response, one, maximum)
    }
    async function inspect() {
      const [usageResult, modelsResult] = await Promise.allSettled([read('usage'), read('models', 4 << 20)])
      const usage = usageResult.status === 'fulfilled' ? usageResult.value : null
      const parsed = codexQuotaWindows(usage, now())
      const model = modelsResult.status === 'fulfilled' ? chooseModel(modelsResult.value) : null
      let cards = null
      const count = usage?.rate_limit_reset_credits?.available_count
      if (count === 0) cards = []
      else if (Number.isSafeInteger(count) && count > 0 && count <= 128) {
        try { cards = resetCards(await read('credits'), count) } catch { /* Unknown is never treated as no cards. */ }
      }
      let resetCredits
      if (cards?.length) {
        const dates = cards.map(c => c.expires_at).filter(Boolean).sort()
        if (dates.length) resetCredits = { count, expires_at: dates[0] }
      }
      const snapshot = {
        account: request.account, enabled: true, fresh: true, observed_at: new Date(now()).toISOString(),
        no_credits: trusted.noCredits === true, on_credits: onCredits(usage), usage_known: parsed.known, models_known: !!model,
        held: parsed.held, windows: parsed.windows.map(w => ({ ...w, resets_at: iso(w.resets_at) })),
        cards_complete: cards !== null, reset_cards: cards || [],
        ...(resetCredits ? { reset_credits: resetCredits } : {}),
      }
      return { snapshot, model }
    }
    if (request.operation === 'inspect') return (await inspect()).snapshot
    if (request.operation === 'warmup') {
      if (!Array.isArray(request.windows) || !request.windows.length || request.windows.length > 2 ||
          request.windows.some(n => !['primary', 'secondary'].includes(n)) || typeof trusted.generation !== 'function') throw failure()
      const { snapshot, model } = await inspect()
      if (!snapshot.usage_known || (snapshot.no_credits && snapshot.held) || !model || signal.aborted) return { status: 'not_sent', code: 'unavailable' }
      if (request.windows.some(name => !snapshot.windows.some(w => w.name === name && w.span_seconds > 0 && !w.aside && !w.model)))
        return { status: 'not_sent', code: 'window_changed' }
      trusted.assertCurrent()
      const input = { model: model.name, input: 'hi', stream: true, ...(model.effort ? { reasoning: { effort: model.effort } } : {}) }
      let response
      try {
        // Only the host can implement generation. It must call its normal
        // fetchAs path, including fresh credentials and the NoCredits guard.
        response = await bounded(trusted.generation(input, signal), signal, late => { void late?.body?.cancel().catch(() => {}) })
        const text = await body(response, signal)
        if (!response.ok) {
          let error
          try { error = JSON.parse(text).error } catch { /* No trusted no-send proof. */ }
          if (error?.type === 'codex_account_policy' && ['codex_quota_unavailable', 'codex_subscription_exhausted'].includes(error.code))
            return { status: 'not_sent', code: 'policy_blocked' }
          return { status: 'unknown', code: 'upstream_rejected' }
        }
        const complete = text.split(/\r?\n/u).some(line => {
          if (!line.startsWith('data:')) return false
          try { return JSON.parse(line.slice(5).trim()).type === 'response.completed' } catch { return false }
        })
        return { status: complete ? 'completed' : 'unknown', code: complete ? 'warmed' : 'incomplete' }
      } catch { void response?.body?.cancel().catch(() => {}); return { status: 'unknown', code: 'interrupted' } }
    }
    if (typeof request.credit_id !== 'string' || !request.credit_id || request.credit_id.length > 512 ||
        !UUID.test(request.redeem_request_id || '') || !Number.isInteger(request.lead_minutes) ||
        request.lead_minutes < 3 || request.lead_minutes > 1440) throw failure()
    const expected = expiryDate(request.expected_expires_at)
    const purpose = request.purpose ?? 'expiring'
    if (!['week', 'expiring'].includes(purpose) || (purpose === 'expiring' && !expected)) throw failure()
    const expectedWeek = purpose === 'week' ? expiryDate(request.expected_week_until) : null
    if (purpose === 'week' && (!expectedWeek || !['before_credits', 'routes_exhausted'].includes(request.trigger))) throw failure()
    // A final fresh check at the actual send boundary is in addition to the
    // Go engine's second look. Never exchange another card or request ID here.
    const { snapshot } = await inspect()
    const candidates = snapshot.reset_cards.filter(c => c.expires_at ? Date.parse(c.expires_at) > now() : purpose === 'week').sort((a, b) => {
      if (!a.expires_at && b.expires_at) return 1
      if (a.expires_at && !b.expires_at) return -1
      if (a.expires_at !== b.expires_at) return a.expires_at.localeCompare(b.expires_at)
      return Buffer.compare(Buffer.from(a.id), Buffer.from(b.id))
    })
    const card = candidates[0]
    const due = purpose === 'week' ? weeklyEnd(snapshot, request.trigger, now()) === expectedWeek
      : eligible(snapshot, card, request.lead_minutes, now())
    if (!card || card.id !== request.credit_id || card.expires_at !== expected || !due || signal.aborted)
      return { status: 'not_sent', code: 'card_changed' }
    // Inspection can overlap a native sign-in change, access-token renewal or
    // an explicit credential-owner handoff. Re-read local authority at the last
    // boundary before spending this exact card; never fall back to the copy
    // captured before the usage/cards requests.
    try {
      trusted.assertCurrent()
      if (typeof trusted.readAuth !== 'function') throw failure()
      const latestStored = object(await bounded(Promise.resolve().then(() => trusted.readAuth()), signal))
      if (ownership(latestStored) !== initialOwnership) throw failure()
      const latest = latestStored.refreshOwner === 'external'
        ? await bounded(externalAuth(latestStored, now()), signal) : latestStored
      const latestAccount = secret(accountID(latest), 'bound Codex account')
      const latestExpires = expiry(latest.access, 0, now()) || Number(latest.expires)
      if (!Number.isFinite(latestExpires) || latestExpires <= now() ||
          codexQuotaIdentity(latest, latestAccount) !== initialIdentity) throw failure()
      trusted.assertCurrent()
      if (signal.aborted) throw abort(signal)
    } catch { return { status: 'not_sent', code: 'credentials_changed' } }
    let response
    try {
      const postHeaders = new Headers(headers); postHeaders.set('content-type', 'application/json')
      response = await bounded(fetchSafe(fetcher, ORIGIN + PATHS.consume, { method: 'POST', headers: postHeaders, signal,
        body: JSON.stringify({ credit_id: card.id, redeem_request_id: request.redeem_request_id }) }), signal,
      late => { void late?.body?.cancel().catch(() => {}) })
      const value = await jsonBody(response, signal)
      if (!['reset', 'nothing_to_reset', 'no_credit', 'already_redeemed'].includes(value.code)) return { status: 'unknown', code: 'invalid_response' }
      return { status: 'completed', code: value.code }
    } catch { void response?.body?.cancel().catch(() => {}); return { status: 'unknown', code: 'interrupted' } }
  }
}
