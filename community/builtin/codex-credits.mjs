// Client-side subscription-window guard. There is no server "never bill credits"
// switch: a separate client or a single large request can cross a window between
// this check and generation. No background poller or paid retry is introduced.
import { secret, sha256 } from './common.mjs'

const MAX_IDENTITIES = 64
const CACHE_MS = 1000
const USAGE_TIMEOUT_MS = 8000
const MAX_USAGE_BYTES = 65536

function record(value) {
  return value && typeof value === 'object' && !Array.isArray(value) ? value : null
}
function finite(value) {
  return typeof value === 'number' && Number.isFinite(value)
}
function abortReason(signal) {
  return signal?.reason instanceof Error ? signal.reason : new DOMException('Request cancelled', 'AbortError')
}
function failure(status, code, message, reset, nowMs) {
  const headers = new Headers({ 'content-type': 'application/json', 'cache-control': 'no-store' })
  if (finite(reset) && reset * 1000 > nowMs)
    headers.set('retry-after', String(Math.min(2147483647, Math.ceil(reset - nowMs / 1000))))
  return new Response(JSON.stringify({ error: { code, message, type: 'codex_account_policy' } }), { status, headers })
}
function unknown() {
  return failure(503, 'codex_quota_unavailable',
    'This account forbids credit spending, but its subscription windows could not be verified; no generation was sent')
}
function held(reset, nowMs) {
  return failure(429, 'codex_subscription_exhausted',
    'This account forbids credit spending and a subscription window is exhausted; no generation was sent', reset, nowMs)
}

// nowMs is milliseconds. All returned resets_at/reset_at values are Unix SECONDS;
// span_seconds is seconds. Only the documented account-wide rate_limit windows
// apply here. Named response-header families are not model-to-window mappings.
export function codexQuotaWindows(value, nowMs = Date.now()) {
  const data = record(value)
  const rate = record(data?.rate_limit)
  const windows = []
  let known = !!rate
  if (!rate) return { known: false, windows, held: false, reset_at: null }
  for (const flag of ['allowed', 'limit_reached'])
    if (rate[flag] !== undefined && typeof rate[flag] !== 'boolean') known = false
  for (const name of ['primary', 'secondary']) {
    const raw = rate[name + '_window']
    if (raw === undefined || raw === null) continue
    const window = record(raw)
    if (!window || !finite(window.used_percent) || window.used_percent < 0 ||
        !Number.isSafeInteger(window.limit_window_seconds) || window.limit_window_seconds <= 0) {
      known = false
      continue
    }
    let reset = null
    if (window.reset_at !== undefined && window.reset_at !== null) {
      if (!Number.isSafeInteger(window.reset_at) || window.reset_at <= 0) known = false
      else reset = window.reset_at
    } else if (window.reset_after_seconds !== undefined && window.reset_after_seconds !== null) {
      if (!Number.isSafeInteger(window.reset_after_seconds) || window.reset_after_seconds < 0) known = false
      else reset = Math.floor(nowMs / 1000) + window.reset_after_seconds
    }
    if (reset !== null && reset * 1000 <= nowMs) known = false
    // A missing reset can prove a full window but cannot establish a safe
    // available interval or make stale data look current.
    if (reset === null && window.used_percent < 100) known = false
    windows.push({ name, used: window.used_percent, span_seconds: window.limit_window_seconds,
      resets_at: reset, aside: false, model: '' })
  }
  if (!windows.length) known = false
  const full = windows.filter(w => w.used >= 100 && (w.resets_at === null || w.resets_at * 1000 > nowMs))
  const denied = rate.allowed === false || rate.limit_reached === true
  const isHeld = full.length > 0 || denied
  const reset = full.length && full.every(w => w.resets_at !== null)
    ? Math.max(...full.map(w => w.resets_at)) : null
  return { known, windows, held: isHeld, reset_at: reset }
}

function stillCurrent(parsed, nowMs) {
  if (parsed.held) {
    if (parsed.reset_at !== null && parsed.reset_at * 1000 <= nowMs) return false
    return true
  }
  return parsed.known && parsed.windows.length > 0 &&
    parsed.windows.every(w => w.resets_at !== null && w.resets_at * 1000 > nowMs)
}
function policyResult(parsed, nowMs) {
  if (parsed.held && stillCurrent(parsed, nowMs)) return held(parsed.reset_at, nowMs)
  return stillCurrent(parsed, nowMs) ? null : unknown()
}

// A workspace can contain multiple users. Key local quota state by the exact
// credential used for its GET; renewal starts a fresh short cache. Keep tokens
// out of cache keys and do not rely on optional JWT user claims.
export function codexQuotaIdentity(auth, account) {
  return sha256(JSON.stringify([account, secret(auth?.access, 'Codex access token')]))
}

export function createCodexCreditGuard({ readUsage, now = Date.now }) {
  const cache = new Map()
  const pending = new Map()
  let revision = 0
  function remember(identity, parsed) {
    cache.delete(identity)
    while (cache.size >= MAX_IDENTITIES) cache.delete(cache.keys().next().value)
    const entry = { parsed, at: now(), revision: ++revision }
    cache.set(identity, entry)
    return entry
  }
  function acquire(identity, auth, account) {
    const existing = pending.get(identity)
    if (existing && !existing.controller.signal.aborted) return existing
    if (!existing && pending.size >= MAX_IDENTITIES) return null
    const flight = { controller: new AbortController(), waiters: new Set(), settled: false, started: revision }
    pending.set(identity, flight)
    flight.promise = (async () => {
      try {
        const response = await readUsage(auth, {
          operation: 'usage', account_hint: account, timeout_ms: USAGE_TIMEOUT_MS,
          max_response_bytes: MAX_USAGE_BYTES, signal: flight.controller.signal,
        })
        if (flight.controller.signal.aborted) throw abortReason(flight.controller.signal)
        if (!response.ok) {
          void response.body?.cancel().catch(() => {})
          throw new Error('usage unavailable')
        }
        // readUsage is the fixed-origin bounded metadata implementation. Read a
        // defensive second bound as well; a replacement test hook cannot grow it.
        const reader = response.body?.getReader()
        if (!reader) throw new Error('usage missing')
        const chunks = []
        let length = 0
        try {
          while (true) {
            const { value, done } = await reader.read()
            if (done) break
            length += value.byteLength
            if (length > MAX_USAGE_BYTES) throw new Error('usage over limit')
            chunks.push(value)
          }
        } catch (error) {
          await reader.cancel().catch(() => {})
          throw error
        } finally { reader.releaseLock() }
        const bytes = new Uint8Array(length)
        let offset = 0
        for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength }
        const parsed = codexQuotaWindows(JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes)), now())
        const newer = cache.get(identity)
        // A generation that finished while this GET was pending can supply a
        // newer full-window header. Do not overwrite that hold with the old GET.
        if (newer && newer.revision > flight.started) return newer.parsed
        if (parsed.known || parsed.held) remember(identity, parsed)
        return parsed
      } finally {
        flight.settled = true
        if (pending.get(identity) === flight) pending.delete(identity)
      }
    })()
    // A caller may abort in the same turn before attaching its waiter.
    void flight.promise.catch(() => {})
    return flight
  }
  function wait(flight, signal) {
    if (signal?.aborted) {
      if (!flight.waiters.size && !flight.settled) flight.controller.abort()
      return Promise.reject(abortReason(signal))
    }
    return new Promise((resolve, reject) => {
      const waiter = {}
      let finished = false
      flight.waiters.add(waiter)
      const finish = (fn, value) => {
        if (finished) return
        finished = true
        signal?.removeEventListener('abort', cancelled)
        flight.waiters.delete(waiter)
        if (!flight.waiters.size && !flight.settled) flight.controller.abort()
        fn(value)
      }
      const cancelled = () => finish(reject, abortReason(signal))
      signal?.addEventListener('abort', cancelled, { once: true })
      flight.promise.then(value => finish(resolve, value), error => finish(reject, error))
    })
  }
  return {
    async check(auth, account, signal) {
      if (signal?.aborted) throw abortReason(signal)
      if (typeof account !== 'string' || !account || account.length > 256) return unknown()
      const identity = codexQuotaIdentity(auth, account)
      const current = cache.get(identity)
      if (current && now() - current.at >= 0 && now() - current.at < CACHE_MS &&
          stillCurrent(current.parsed, now())) return policyResult(current.parsed, now())
      const flight = acquire(identity, auth, account)
      if (!flight) return unknown()
      try {
        const parsed = await wait(flight, signal)
        if (signal?.aborted) throw abortReason(signal)
        return policyResult(parsed, now())
      } catch (error) {
        if (signal?.aborted) throw abortReason(signal)
        return unknown()
      }
    },
    observe(auth, account, headers) {
      const identity = codexQuotaIdentity(auth, account)
      // These fixed headers describe the account-wide subscription windows.
      // A new named family is not assumed to correspond to the current model.
      const full = []
      for (const name of ['primary', 'secondary']) {
        const prefix = 'x-codex-' + name + '-'
        const used = headers.get(prefix + 'used-percent')
        const span = headers.get(prefix + 'window-minutes')
        if (used === null || span === null || !/^\d+(?:\.\d+)?$/u.test(used) || !/^\d+$/u.test(span)) continue
        const percent = Number(used), seconds = Number(span) * 60
        if (!Number.isFinite(percent) || percent < 100 || !Number.isSafeInteger(seconds) || seconds <= 0) continue
        const rawReset = headers.get(prefix + 'reset-at')
        const reset = rawReset && /^\d+$/u.test(rawReset) && Number.isSafeInteger(Number(rawReset))
          && Number(rawReset) > 0 ? Number(rawReset) : null
        if (reset !== null && reset * 1000 <= now()) continue
        full.push({ name, used: percent, span_seconds: seconds, resets_at: reset, aside: false, model: '' })
      }
      if (!full.length) return
      // Headers can prove a hold. Partial headers never create permission to
      // generate or extend the lifetime of an older available usage snapshot.
      remember(identity, { known: true, windows: full, held: true,
        reset_at: full.every(w => w.resets_at !== null) ? Math.max(...full.map(w => w.resets_at)) : null })
    },
  }
}
