// Optional provider compatibility, based on Magpie's MIT-licensed wire adapters.
// Upstream reference: yetone/magpie 1e174f8f9f613a920203a4658d259456fcab5d0d.
// No account discovery or network work runs when a module is imported.
import { createHash } from 'node:crypto'

export const MAX_REQUEST = 16 << 20
export const MAX_REPLY = 64 << 20
export const MAX_AUTH_REPLY = 1 << 20

export function object(value, label = 'JSON') {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${label} must be an object`)
  return value
}

export function requestBody(init) {
  if (typeof init?.body !== 'string' || Buffer.byteLength(init.body) > MAX_REQUEST) {
    throw new Error('provider request must be a JSON string of at most 16 MiB')
  }
  try { return object(JSON.parse(init.body), 'provider request') }
  catch { throw new Error('provider request is not a JSON object') }
}

export function secret(value, label) {
  if (typeof value !== 'string' || !value || value.length > 32768 || /[\x00-\x20\x7f]/u.test(value)) {
    throw new Error(`${label} is missing or malformed`)
  }
  return value
}

export function sha256(value) { return createHash('sha256').update(value).digest('hex') }

export function jwtClaims(token) {
  try {
    if (typeof token !== 'string' || token.length > 32768) return {}
    return object(JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString('utf8')))
  } catch { return {} }
}

export function expiry(token, seconds, now = Date.now()) {
  const exp = Number(jwtClaims(token).exp)
  if (Number.isFinite(exp) && exp > 0) return exp * 1000
  const lifetime = Number(seconds)
  return Number.isFinite(lifetime) && lifetime > 0 ? now + lifetime * 1000 : 0
}

// The plugin itself sets upstream credentials. A local gateway token, a stale
// loader header, or a client Content-Length must never survive this boundary.
export function headersFor(init) {
  const headers = new Headers(init?.headers)
  for (const name of ['authorization', 'proxy-authorization', 'x-api-key', 'x-goog-api-key',
    'host', 'connection', 'transfer-encoding', 'content-length', 'cookie']) headers.delete(name)
  headers.set('content-type', 'application/json')
  return headers
}

export function safeBase(raw, label = 'provider endpoint', testMode = false) {
  let url
  try { url = new URL(raw) } catch { throw new Error(`${label} is not a URL`) }
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname)
  if ((url.protocol !== 'https:' && !(testMode && local && url.protocol === 'http:')) || url.username || url.password || url.hash || url.search) {
    throw new Error(`${label} must be a credential-free HTTPS URL`)
  }
  return url.href.replace(/\/$/, '')
}

export async function fetchSafe(fetcher, url, init) {
  // Redirects could send the rebuilt upstream credentials to a different host.
  return await fetcher(url, { ...init, redirect: 'error', credentials: 'omit' })
}

export async function boundedBytes(response, limit = MAX_AUTH_REPLY) {
  if (!response.body) return new Uint8Array()
  const reader = response.body.getReader()
  const chunks = []
  let total = 0
  try {
    while (true) {
      const { value, done } = await reader.read()
      if (done) break
      total += value.byteLength
      if (total > limit) throw new Error('provider response exceeds the configured compatibility limit')
      chunks.push(value)
    }
    const all = new Uint8Array(total)
    let offset = 0
    for (const chunk of chunks) { all.set(chunk, offset); offset += chunk.byteLength }
    return all
  } catch (error) {
    await reader.cancel().catch(() => {})
    throw error
  } finally { reader.releaseLock() }
}

export async function authJSON(response, label) {
  const bytes = await boundedBytes(response)
  let data
  try { data = object(JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes))) }
  catch { throw new Error(`${label} returned invalid JSON (HTTP ${response.status})`) }
  if (!response.ok) {
    // Do not place a token endpoint's arbitrary response text into logs.
    const error = new Error(`${label} failed (HTTP ${response.status})`)
    if (response.status === 401 || data.error === 'invalid_grant') error.signIn = 'expired'
    throw error
  }
  return data
}

// SSE framing is independent of network chunk boundaries, including CRLF split
// across reads. Limits apply before JSON parsing and include unknown events.
export async function* sseEvents(response, maxBytes = MAX_REPLY) {
  if (!response.body) throw new Error('streaming provider returned no body')
  const reader = response.body.getReader()
  const decoder = new TextDecoder('utf-8', { fatal: true })
  let pending = '', event = '', lines = [], eventBytes = 0, total = 0, complete = false
  const accept = (line) => {
    if (line === '') {
      const result = lines.length ? { event, data: lines.join('\n') } : undefined
      lines = []; event = ''; eventBytes = 0
      return result
    }
    eventBytes += Buffer.byteLength(line) + 1
    if (eventBytes > MAX_REPLY) throw new Error('provider SSE event exceeds 64 MiB')
    const colon = line.indexOf(':')
    const field = colon < 0 ? line : line.slice(0, colon)
    let value = colon < 0 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'data') lines.push(value)
    else if (field === 'event') event = value
  }
  try {
    while (true) {
      const { value, done } = await reader.read()
      if (done) pending += decoder.decode()
      else {
        total += value.byteLength
        if (total > maxBytes) throw new Error('provider SSE response exceeds 64 MiB')
        pending += decoder.decode(value, { stream: true })
      }
      while (true) {
        const index = pending.search(/[\r\n]/u)
        if (index < 0 || (pending[index] === '\r' && index === pending.length - 1 && !done)) break
        const line = pending.slice(0, index)
        const width = pending[index] === '\r' && pending[index + 1] === '\n' ? 2 : 1
        pending = pending.slice(index + width)
        const result = accept(line)
        if (result) yield result
      }
      if (Buffer.byteLength(pending) + eventBytes > MAX_REPLY) throw new Error('provider SSE event exceeds 64 MiB')
      if (done) {
        if (pending) accept(pending)
        const result = accept('')
        if (result) yield result
        complete = true
        break
      }
    }
  } finally {
    if (!complete) await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}

export function jsonResponse(data, response, status = response.status) {
  const headers = new Headers(response.headers)
  for (const name of ['content-length', 'content-encoding', 'transfer-encoding', 'connection']) headers.delete(name)
  headers.set('content-type', 'application/json')
  const body = JSON.stringify(data)
  if (Buffer.byteLength(body) > MAX_REPLY) throw new Error('provider JSON response exceeds 64 MiB')
  return new Response(body, { status, headers })
}

export function protocolFromPath(raw) {
  const url = new URL(raw)
  if (/\/chat\/completions\/?$/u.test(url.pathname)) return 'chat'
  if (/\/responses\/?$/u.test(url.pathname)) return 'responses'
  if (/\/messages\/?$/u.test(url.pathname)) return 'anthropic'
  if (/:streamGenerateContent$|:generateContent$/u.test(url.pathname)) return 'gemini'
  throw new Error('provider endpoint does not name a supported protocol')
}
