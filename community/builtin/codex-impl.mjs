// MIT compatibility implementation; wire behavior reviewed against Magpie's
// internal/provider/{account,codex_request}.go at 1e174f8f9f613a920203a4658d259456fcab5d0d.
import { authJSON, expiry, fetchSafe, headersFor, jwtClaims, jsonResponse, object,
  requestBody, safeBase, secret, sha256, sseEvents } from './common.mjs'
import { createCodexMetadata } from './codex-metadata.mjs'
import { createCodexMaintenance } from './codex-maintenance.mjs'
import { createCodexCreditGuard, codexQuotaIdentity } from './codex-credits.mjs'
import { createCodexQuota } from './codex-quota.mjs'
import { open } from 'node:fs/promises'
import { constants } from 'node:fs'
import path from 'node:path'

export const PROVIDER = 'builtin:codex'
export const CODEX_BASE = 'https://chatgpt.com/backend-api/codex'
export const TOKEN_URL = 'https://auth.openai.com/oauth/token'
export const CLIENT_ID = 'app_EMoamEEZ73f0CkXaXp7hrann'
const DEFAULT_VERSION = '0.169.1'

export function accountID(auth) {
  return auth.accountId || auth.account_id || jwtClaims(auth.idToken ?? auth.id_token)['https://api.openai.com/auth']?.chatgpt_account_id ||
    jwtClaims(auth.access)['https://api.openai.com/auth']?.chatgpt_account_id || ''
}

// Native Codex only serializes writes to auth.json; its refresh exchange is not
// covered by the cross-process file lock. Until an explicit owner handoff, this
// adapter borrows the current access token and never spends its refresh token.
export async function externalAuth(auth, now = Date.now()) {
  const file = auth?.sourceAuthFile
  const expected = accountID(auth)
  if (typeof file !== 'string' || !path.isAbsolute(file) || !expected) {
    throw new Error('Externally managed Codex account has no bound credential source')
  }
  let handle, raw
  try {
    handle = await open(file, constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0))
    const st = await handle.stat()
    if (!st.isFile() || st.size <= 0 || st.size > 1024 * 1024 ||
      (process.platform !== 'win32' && (st.mode & 0o077) !== 0)) {
      throw new Error('invalid external credential file')
    }
    // A fixed buffer also bounds a file that grows after stat. Codex saves with
    // an atomic rename, so an opened old generation remains a consistent read.
    const bytes = Buffer.alloc(st.size + 1)
    let length = 0
    while (length < bytes.length) {
      const read = await handle.read(bytes, length, bytes.length - length, length)
      if (!read.bytesRead) break
      length += read.bytesRead
    }
    if (length !== st.size) throw new Error('external credential file changed')
    raw = object(JSON.parse(bytes.subarray(0, length).toString('utf8')))
  } catch {
    throw new Error('Cannot read the private externally managed Codex credential file')
  } finally {
    if (handle) await handle.close()
  }
  const tokens = object(raw.tokens, 'external Codex tokens')
  const access = secret(tokens.access_token, 'external Codex access token')
  const next = { access, refresh: tokens.refresh_token ?? '', idToken: tokens.id_token ?? '',
    accountId: tokens.account_id ?? '', expires: expiry(access, 0, now) }
  next.accountId = accountID(next)
  if (next.accountId !== expected) throw new Error('Externally managed Codex source account changed; explicit reconfiguration is required')
  if (!next.expires || next.expires <= now) {
    throw new Error('Externally managed Codex access token has expired; its native owner must renew it or complete an explicit handoff')
  }
  if (next.refresh) secret(next.refresh, 'external Codex refresh token')
  if (next.idToken) secret(next.idToken, 'external Codex ID token')
  return next
}

function checkOwner(auth) {
  if (auth?.refreshOwner !== undefined && !['external', 'host'].includes(auth.refreshOwner)) {
    throw new Error('Codex refresh owner is invalid')
  }
}

export function clientHeaders(headers, version = DEFAULT_VERSION) {
  headers.set('originator', 'codex_cli_rs')
  headers.set('version', version)
  headers.set('user-agent', `codex_cli_rs/${version} (${process.platform}; ${process.arch}) zcalld`)
}

export function prepare(body) {
  const result = structuredClone(object(body))
  // The subscription endpoint has no stored-response lookup. Reject references
  // whose contents cannot be reconstructed, rather than silently losing turns.
  if (result.previous_response_id) throw new Error('Codex requires complete input history; previous_response_id is unsupported')
  for (const key of ['max_output_tokens', 'max_completion_tokens', 'temperature', 'top_p', 'previous_response_id',
    'user', 'safety_identifier', 'prompt_cache_options']) delete result[key]
  if (typeof result.input === 'string') result.input = [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: result.input }] }]
  if (result.input === undefined) result.input = []
  if (!Array.isArray(result.input)) throw new Error('Codex input must be a string or an array')
  for (const item of result.input) {
    if (!item || typeof item !== 'object') continue
    if (item.type === 'item_reference') throw new Error('Codex requires inline history; item_reference is unsupported')
    const notification = ['function_call_output', 'custom_tool_call_output'].includes(item.type) && !item.call_id
    const compaction = ['compaction', 'compaction_summary', 'context_compaction'].includes(item.type)
    if (!notification && !compaction) delete item.id
    if (!item.type || ['message', 'reasoning'].includes(item.type)) delete item.status
    if ((!item.type || item.type === 'message') && item.role === 'system') item.role = 'developer'
    if (typeof item.call_id === 'string' && item.call_id.length > 64) item.call_id = sha256(item.call_id.trim())
  }
  result.store = false
  result.stream = true
  // Keep tools, tool_choice, encrypted reasoning, effort and service-tier fields
  // as the caller supplied them. The server decides account entitlements.
  if (result.instructions === undefined) result.instructions = ''
  if (result.parallel_tool_calls === undefined) result.parallel_tool_calls = true
  if (result.reasoning?.effort === 'ultra') result.reasoning.effort = 'max'
  if (result.reasoning?.effort !== 'none') {
    const include = result.include ?? []
    if (!Array.isArray(include)) throw new Error('Codex include must be an array')
    if (!include.includes('reasoning.encrypted_content')) include.push('reasoning.encrypted_content')
    result.include = include
  }
  if (!result.prompt_cache_key) {
    const firstUser = result.input.findIndex((item) => item?.role === 'user')
    if (firstUser >= 0) {
      const key = sha256(String(result.instructions) + JSON.stringify(result.input.slice(0, firstUser + 1)))
      result.prompt_cache_key = `${key.slice(0, 8)}-${key.slice(8, 12)}-${key.slice(12, 16)}-${key.slice(16, 20)}-${key.slice(20, 32)}`
    }
  }
  return result
}

// The subscription endpoint sometimes omits Content-Type while returning SSE.
// Only its successful, untyped replies are probed. Validate a complete first
// Responses event within 64 KiB, then replay every byte and retain backpressure.
async function codexEventStream(response) {
  const declared = (response.headers.get('content-type') ?? '').trim()
  const media = declared.split(';', 1)[0].trim().toLowerCase()
  if (media === 'text/event-stream') return response
  if (declared || !response.body) {
    await response.body?.cancel().catch(() => {})
    throw new Error('Codex returned a non-SSE success response')
  }
  const reader = response.body.getReader()
  const prefix = new Uint8Array(64 << 10)
  const decoder = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })
  let used = 0, scanned = 0, lineStart = 0, skipLF = false, firstLine = true
  let event = '', data = '', hasData = false, recognized = false, tail
  const line = (bytes) => {
    let text = decoder.decode(bytes)
    if (firstLine) { firstLine = false; if (text.startsWith('\uFEFF')) text = text.slice(1) }
    if (text === '') {
      if (!hasData) { event = ''; return }
      let value
      try { value = object(JSON.parse(data)) } catch { throw new Error('Codex returned malformed SSE JSON') }
      if (typeof value.type !== 'string' || !/^(?:response\.[a-z0-9_.]+|error)$/u.test(value.type) ||
        (event && event !== value.type)) throw new Error('Codex returned a non-Responses SSE event')
      recognized = true
      return
    }
    if (text.startsWith(':')) return
    const colon = text.indexOf(':')
    const field = colon < 0 ? text : text.slice(0, colon)
    let value = colon < 0 ? '' : text.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') event = value
    else if (field === 'data') { data += (hasData ? '\n' : '') + value; hasData = true }
  }
  try {
    while (!recognized) {
      const { value, done } = await reader.read()
      if (done) throw new Error('Codex stream ended before a complete SSE event')
      if (!(value instanceof Uint8Array)) throw new Error('Codex stream returned non-byte data')
      const keep = Math.min(value.byteLength, prefix.length - used)
      prefix.set(value.subarray(0, keep), used)
      used += keep
      while (scanned < used && !recognized) {
        const at = scanned++, byte = prefix[at]
        if (skipLF && byte === 10) { skipLF = false; lineStart = scanned; continue }
        skipLF = false
        if (byte === 10 || byte === 13) {
          line(prefix.subarray(lineStart, at))
          lineStart = scanned
          skipLF = byte === 13
        }
      }
      if (recognized) {
        if (keep < value.byteLength) tail = value.subarray(keep)
        break
      }
      if (used === prefix.length) throw new Error('Codex SSE prefix exceeds 64 KiB')
    }
    let first = prefix.subarray(0, used), ended = false
    const replay = new ReadableStream({
      async pull(controller) {
        if (first) { controller.enqueue(first); first = undefined; return }
        if (tail) { controller.enqueue(tail); tail = undefined; return }
        try {
          const { value, done } = await reader.read()
          if (done) { ended = true; reader.releaseLock(); controller.close() }
          else controller.enqueue(value)
        } catch (error) {
          ended = true; reader.releaseLock(); controller.error(error)
        }
      },
      async cancel(reason) {
        if (!ended) { ended = true; await reader.cancel(reason).catch(() => {}); reader.releaseLock() }
      },
    }, { highWaterMark: 0 })
    const headers = new Headers(response.headers)
    headers.set('content-type', 'text/event-stream')
    return new Response(replay, { status: response.status, statusText: response.statusText, headers })
  } catch (error) {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
    throw error
  }
}

export async function collect(response) {
  for await (const event of sseEvents(response)) {
    if (event.data === '[DONE]') break
    let data
    try { data = object(JSON.parse(event.data)) }
    catch { throw new Error('Codex returned malformed SSE JSON') }
    const type = data.type || event.event
    if (['response.completed', 'response.incomplete', 'response.failed'].includes(type)) {
      const final = object(data.response, 'final Codex response')
      // The final object includes output items, tool arguments, usage, encrypted
      // reasoning and vendor extensions. No text-only reconstruction is used.
      return jsonResponse(final, response)
    }
    if (type === 'error') return jsonResponse({ error: data.error ?? data }, response, 502)
  }
  throw new Error('Codex stream ended without a final response')
}

export function createCodex({ fetcher = globalThis.fetch, baseURL = CODEX_BASE, tokenURL = TOKEN_URL,
  version = DEFAULT_VERSION, now = Date.now, testMode = false } = {}) {
  const base = safeBase(baseURL, 'Codex endpoint', testMode)
  const token = safeBase(tokenURL, 'Codex token endpoint', testMode)
  const codexMetadata = createCodexMetadata({ fetcher, externalAuth, accountID, clientHeaders, now })
  const creditGuard = createCodexCreditGuard({ readUsage: codexMetadata, now })
  return {
    auth: {
      provider: PROVIDER,
      methods: [],
      codexMetadata,
      codexQuota: createCodexQuota({ readUsage: codexMetadata, externalAuth, accountID, now }),
      codexMaintenance: createCodexMaintenance({ fetcher, externalAuth, accountID, clientHeaders, now }),
      // Refresh only when an operation needs the account. The host owns a
      // per-account, cross-process renewal lock and persists before releasing it.
      keepAlive: false,
      refreshLead: 120000,
      async refresh(auth) {
        checkOwner(auth)
        if (auth.refreshOwner === 'external') return await externalAuth(auth, now())
        const refresh = secret(auth.refresh, 'Codex refresh token')
        const headers = new Headers({ 'content-type': 'application/json' })
        clientHeaders(headers, version)
        // Do not bind rotation to a generation's cancellation signal: losing a
        // successful rotated token would also break rollback to the old gateway.
        const response = await fetchSafe(fetcher, token, { method: 'POST', headers,
          body: JSON.stringify({ client_id: CLIENT_ID, grant_type: 'refresh_token', refresh_token: refresh, refresh_reason: 'proactive_expiry' }),
          signal: AbortSignal.timeout(30000) })
        const data = await authJSON(response, 'Codex token refresh')
        const access = secret(data.access_token, 'renewed Codex access token')
        const next = { access, refresh: data.refresh_token ? secret(data.refresh_token, 'renewed Codex refresh token') : refresh,
          expires: expiry(access, data.expires_in, now()) }
        if (data.id_token) next.idToken = secret(data.id_token, 'renewed Codex ID token')
        const account = accountID({ ...auth, ...next })
        if (account) next.accountId = account
        if (!next.expires) throw new Error('Codex refresh did not provide an expiry')
        return next
      },
      async loader(getAuth) {
        return {
          baseURL: base,
          async fetch(url, init) {
            const { zcalldAccountPolicy: policy, ...wireInit } = init ?? {}
            if (policy !== undefined && (!policy || typeof policy.codex_no_credits !== 'boolean' ||
                typeof policy.assertCurrent !== 'function')) throw new Error('Invalid trusted Codex account policy')
            const asked = new URL(url)
            if (!/\/responses\/?$/u.test(asked.pathname)) throw new Error('Codex supports the Responses endpoint')
            const original = requestBody(wireInit)
            const stream = original.stream === true
            const body = prepare(original)
            let stored = await getAuth()
            checkOwner(stored)
            let auth = stored?.refreshOwner === 'external' ? await externalAuth(stored, now()) : stored
            const account = accountID(auth)
            policy?.assertCurrent()
            if (policy?.codex_no_credits) {
              // Borrow only the resolved credential. A second external-file read
              // inside metadata could check another user under this snapshot's key.
              const snapshot = Object.freeze({ type: stored?.type, access: auth?.access,
                accountId: account, expires: auth?.expires })
              const identity = codexQuotaIdentity(snapshot, account)
              const blocked = await creditGuard.check(snapshot, account, wireInit.signal)
              if (blocked) return blocked
              // Workspace IDs are shared by members. Check the actual credential
              // again before POST, including when an external owner renews it.
              stored = await getAuth()
              checkOwner(stored)
              auth = stored?.refreshOwner === 'external' ? await externalAuth(stored, now()) : stored
              if (codexQuotaIdentity(auth, accountID(auth)) !== identity)
                throw new Error('Codex account changed during the subscription check')
              auth = snapshot
            }
            const headers = headersFor(wireInit)
            headers.set('authorization', `Bearer ${secret(auth?.access, 'Codex access token')}`)
            if (account) headers.set('chatgpt-account-id', secret(account, 'Codex account ID'))
            else headers.delete('chatgpt-account-id')
            clientHeaders(headers, version)
            headers.set('openai-beta', 'responses=experimental')
            headers.set('accept', 'text/event-stream')
            for (const name of ['session_id', 'conversation_id', 'session-id', 'thread-id']) headers.delete(name)
            if (body.prompt_cache_key) {
              const key = secret(body.prompt_cache_key, 'Codex prompt cache key')
              headers.set('session-id', key); headers.set('thread-id', key)
            }
            policy?.assertCurrent()
            if (wireInit.signal?.aborted) throw wireInit.signal.reason
            const response = await fetchSafe(fetcher, `${base}/responses${asked.search}`, { ...wireInit, headers, body: JSON.stringify(body) })
            if (policy?.codex_no_credits) creditGuard.observe(auth, account, response.headers)
            if (!response.ok) return response
            const events = await codexEventStream(response)
            return stream ? events : await collect(events)
          },
        }
      },
    },
  }
}

