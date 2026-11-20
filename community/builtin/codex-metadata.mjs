// Optional native Codex metadata: fixed-origin GETs, explicit account, no renewal.
import { object, secret, expiry, fetchSafe, sha256 } from './common.mjs'

const ORIGIN = 'https://chatgpt.com'
const PATHS = Object.freeze({
  models: '/backend-api/codex/models',
  usage: '/backend-api/wham/usage',
  accounts: '/backend-api/wham/accounts/check',
  config: '/backend-api/wham/config/bundle',
})
const MAX_BYTES = 4 << 20
const VERSION = /^[A-Za-z0-9.+_-]{1,64}$/u

function failure(status, code, message) {
  return new Response(JSON.stringify({ error: { code, message } }), {
    status, headers: { 'content-type': 'application/json', 'cache-control': 'no-store' },
  })
}
function aborted() { return new DOMException('Codex metadata operation cancelled', 'AbortError') }
async function abortable(promise, signal, late) {
  if (signal.aborted) { promise.then(late, () => {}); throw aborted() }
  let stop
  const cancelled = new Promise((_, reject) => {
    stop = () => reject(aborted())
    signal.addEventListener('abort', stop, { once: true })
  })
  try { return await Promise.race([promise, cancelled]) }
  catch (error) {
    if (signal.aborted) promise.then(late, () => {})
    throw error
  } finally { signal.removeEventListener('abort', stop) }
}
async function bytes(response, maximum, signal) {
  if (!response.body) throw new Error('invalid body')
  const reader = response.body.getReader()
  const chunks = []
  let length = 0
  try {
    while (true) {
      const { value, done } = await abortable(reader.read(), signal, () => {})
      if (done) break
      length += value.byteLength
      if (length > maximum) throw new Error('over limit')
      chunks.push(value)
    }
    const result = new Uint8Array(length)
    let offset = 0
    for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength }
    return result
  } catch (error) {
    // Abort/cancel must release the live body even when the host gives up before head.
    void reader.cancel().catch(() => {})
    throw error
  } finally { reader.releaseLock() }
}
function modelsFor(data, bindings) {
  if (!Array.isArray(data.models) || !Array.isArray(bindings) || !bindings.length || bindings.length > 4096)
    throw new Error('invalid model catalog')
  const upstream = new Map()
  for (const model of data.models) {
    if (!model || typeof model !== 'object' || Array.isArray(model) || typeof model.slug !== 'string' ||
        upstream.has(model.slug)) throw new Error('invalid model catalog')
    upstream.set(model.slug, model)
  }
  const seen = new Set()
  const models = []
  for (const binding of bindings) {
    const { route, model } = object(binding)
    if (typeof route !== 'string' || !route || route.length > 512 || /[\u0000-\u001f\u007f]/u.test(route) ||
        typeof model !== 'string' || !model || model.length > 512 || seen.has(route))
      throw new Error('invalid model binding')
    seen.add(route)
    const entry = upstream.get(model)
    if (!entry) continue // The account has not advertised this model; invent nothing.
    // Native 0.170 requires real instructions or its upstream template.
    if (typeof entry.base_instructions !== 'string' &&
        typeof entry.model_messages?.instructions_template !== 'string')
      throw new Error('upstream model omitted instructions')
    // Preserve every upstream capability and instruction; only expose the proven
    // local route name as slug. Dynamic/cross-provider mappings are rejected by host.
    models.push({ ...entry, slug: route })
  }
  return { ...data, models }
}
export function createCodexMetadata({ fetcher, externalAuth, accountID, clientHeaders, now }) {
  return async function codexMetadata(stored, request) {
    let response
    try {
      object(request)
      const path = Object.hasOwn(PATHS, request.operation) ? PATHS[request.operation] : undefined
      if (!path || Object.prototype.hasOwnProperty.call(request, 'url'))
        return failure(400, 'codex_metadata_request_invalid', 'Unsupported Codex metadata operation')
      const version = request.client_version || '0.170.0'
      if (!VERSION.test(version)) return failure(400, 'codex_metadata_request_invalid', 'Invalid Codex client version')
      const maximum = Math.min(MAX_BYTES, Math.max(1, Number(request.max_response_bytes) || MAX_BYTES))
      const timeout = Math.min(30000, Math.max(1, Number(request.timeout_ms) || 30000))
      const signals = [AbortSignal.timeout(timeout)]
      if (request.signal) signals.push(request.signal)
      const signal = AbortSignal.any(signals)
      let auth = object(stored)
      if (auth.type !== 'oauth' || !['external', 'host', undefined].includes(auth.refreshOwner))
        return failure(401, 'codex_account_unavailable', 'The explicitly bound Codex account is unavailable')
      if (auth.refreshOwner === 'external') {
        try { auth = await externalAuth(auth, now()) }
        catch {
          return failure(401, 'codex_external_auth_unavailable',
            'The native Codex credential source is unavailable, expired, or belongs to another account; renew it in its owner or explicitly rebind it')
        }
      }
      const expires = expiry(auth.access, 0, now()) || Number(auth.expires)
      if (!Number.isFinite(expires) || expires <= now())
        return failure(401, 'codex_auth_expired', 'Codex access has expired; its credential owner must renew it')
      const account = secret(accountID(auth), 'bound Codex account')
      if (request.account_hint !== undefined) {
        const hint = request.account_hint
        if (typeof hint !== 'string' || !hint || hint.length > 256 || hint.trim() !== hint || /[\u0000-\u001f\u007f]/u.test(hint))
          return failure(400, 'codex_account_hint_invalid', 'Empty or invalid Codex account hint')
        if (hint !== account)
          return failure(403, 'codex_account_mismatch', 'The client account does not match the explicitly bound provider account')
      }
      const headers = new Headers({
        accept: 'application/json',
        authorization: `Bearer ${secret(auth.access, 'Codex access token')}`,
        'chatgpt-account-id': account,
      })
      clientHeaders(headers, version)
      const url = new URL(path, ORIGIN)
      if (request.operation === 'models') url.searchParams.set('client_version', version)
      const pending = fetchSafe(fetcher, url.href, { method: 'GET', headers, signal })
      response = await abortable(pending, signal, (late) => { void late?.body?.cancel().catch(() => {}) })
      if (!response.ok) {
        void response.body?.cancel().catch(() => {})
        if (response.status >= 300 && response.status < 400)
          return failure(502, 'codex_metadata_redirect_rejected', 'Codex metadata redirects are not followed')
        return failure(response.status >= 400 && response.status <= 599 ? response.status : 502,
          'codex_metadata_upstream_rejected', 'The bound Codex account metadata request was rejected')
      }
      const media = (response.headers.get('content-type') || '').split(';', 1)[0].trim().toLowerCase()
      if (media !== 'application/json' && !/^application\/[a-z0-9.+-]+\+json$/u.test(media))
        throw new Error('invalid content type')
      let raw = await bytes(response, maximum, signal)
      const value = object(JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(raw)))
      const outputHeaders = new Headers({ 'content-type': 'application/json', 'cache-control': 'no-store' })
      if (request.operation === 'models') {
        raw = new TextEncoder().encode(JSON.stringify(modelsFor(value, request.models)))
        if (raw.byteLength > maximum) throw new Error('mapped catalog over limit')
        outputHeaders.set('etag', `"zcalld-${sha256(raw)}"`)
      } else {
        const tag = response.headers.get('etag')
        if (tag && tag.length <= 1024 && !/[\u0000-\u001f\u007f]/u.test(tag)) outputHeaders.set('etag', tag)
      }
      return new Response(raw, { status: response.status, headers: outputHeaders })
    } catch (error) {
      void response?.body?.cancel().catch(() => {})
      return failure(error?.name === 'AbortError' || error?.name === 'TimeoutError' ? 504 : 502,
        error?.name === 'AbortError' || error?.name === 'TimeoutError' ? 'codex_metadata_timeout' : 'codex_metadata_invalid_response',
        error?.name === 'AbortError' || error?.name === 'TimeoutError'
          ? 'Codex metadata request was cancelled or timed out'
          : 'Codex metadata could not be read; no credentials were renewed')
    }
  }
}
