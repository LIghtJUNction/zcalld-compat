// MIT compatibility implementation; wire behavior reviewed against Magpie's
// internal/provider/{account,codex_request}.go at 1e174f8f9f613a920203a4658d259456fcab5d0d.
import { authJSON, expiry, fetchSafe, headersFor, jwtClaims, jsonResponse, object,
  requestBody, safeBase, secret, sha256, sseEvents } from './common.mjs'

export const PROVIDER = 'builtin:codex'
export const CODEX_BASE = 'https://chatgpt.com/backend-api/codex'
export const TOKEN_URL = 'https://auth.openai.com/oauth/token'
export const CLIENT_ID = 'app_EMoamEEZ73f0CkXaXp7hrann'
const DEFAULT_VERSION = '0.169.1'

export function accountID(auth) {
  return auth.accountId || auth.account_id || jwtClaims(auth.idToken ?? auth.id_token)['https://api.openai.com/auth']?.chatgpt_account_id ||
    jwtClaims(auth.access)['https://api.openai.com/auth']?.chatgpt_account_id || ''
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
    if (!notification) delete item.id
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
  return {
    auth: {
      provider: PROVIDER,
      methods: [],
      // Refresh only when an operation needs the account. The host owns a
      // per-account, cross-process renewal lock and persists before releasing it.
      keepAlive: false,
      refreshLead: 120000,
      async refresh(auth) {
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
            const asked = new URL(url)
            if (!/\/responses\/?$/u.test(asked.pathname)) throw new Error('Codex supports the Responses endpoint')
            const original = requestBody(init)
            const stream = original.stream === true
            const body = prepare(original)
            const auth = await getAuth()
            const headers = headersFor(init)
            headers.set('authorization', `Bearer ${secret(auth?.access, 'Codex access token')}`)
            const account = accountID(auth)
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
            const response = await fetchSafe(fetcher, `${base}/responses${asked.search}`, { ...init, headers, body: JSON.stringify(body) })
            if (!response.ok || stream) return response
            if (!(response.headers.get('content-type') ?? '').toLowerCase().startsWith('text/event-stream')) {
              throw new Error('Codex returned a non-SSE success response')
            }
            return await collect(response)
          },
        }
      },
    },
  }
}

export default async function () { return createCodex() }
