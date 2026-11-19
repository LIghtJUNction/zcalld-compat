// Google's Code Assist envelope is confined to this optional plugin. The core
// continues to see ordinary Gemini requests/responses and never knows a project,
// subscription tier, token, or client application identifier.
import { randomUUID } from 'node:crypto'
import { authJSON, fetchSafe, headersFor, jsonResponse, object, requestBody, safeBase,
  secret, sha256, sseEvents } from './common.mjs'

const APPS = {
  gemini: {
    base: 'https://cloudcode-pa.googleapis.com', version: '0.61.0',
  },
  antigravity: {
    base: 'https://daily-cloudcode-pa.googleapis.com', version: '1.22.2',
  },
}

function userAgent(agent, app, model) {
  return agent === 'antigravity' ? `antigravity/hub/${app.version} ${process.platform}/${process.arch}` :
    `GeminiCLI/${app.version}/${model || 'gemini-2.5-pro'} (${process.platform}; ${process.arch}; terminal)`
}

export function envelope(agent, model, project, body) {
  const request = structuredClone(body)
  const result = { project, model, request }
  if (agent === 'antigravity') {
    result.userAgent = 'antigravity'; result.requestType = 'agent'; result.requestId = `agent-${randomUUID()}`
    const first = request.contents?.find((item) => item.role === 'user')?.parts?.find((part) => typeof part.text === 'string')?.text || randomUUID()
    request.sessionId = `-${BigInt(`0x${sha256(first).slice(0, 16)}`) & 0x7fffffffffffffffn}`
  } else result.user_prompt_id = randomUUID()
  return result
}

function unpack(event) {
  let data
  try { data = object(JSON.parse(event.data)) } catch { throw new Error('Code Assist returned malformed SSE JSON') }
  return data.response ? object(data.response, 'Code Assist response') : data
}

export async function collect(response) {
  const result = {}
  const candidates = new Map()
  let received = false
  for await (const event of sseEvents(response)) {
    if (event.data === '[DONE]') break
    const data = unpack(event); received = true
    if (data.error) return jsonResponse(data, response, Number(data.error.code) >= 400 && Number(data.error.code) <= 599 ? Number(data.error.code) : 502)
    for (const [key, value] of Object.entries(data)) if (key !== 'candidates') result[key] = value
    for (const candidate of data.candidates ?? []) {
      const index = candidate.index ?? 0
      let merged = candidates.get(index)
      if (!merged) { merged = { index, content: { role: 'model', parts: [] } }; candidates.set(index, merged) }
      for (const [key, value] of Object.entries(candidate)) if (key !== 'content') merged[key] = value
      if (candidate.content?.role) merged.content.role = candidate.content.role
      for (const part of candidate.content?.parts ?? []) {
        const last = merged.content.parts.at(-1)
        const textOnly = (p) => p && typeof p.text === 'string' && Object.keys(p).every((key) => ['text', 'thought'].includes(key))
        if (textOnly(last) && textOnly(part) && !!last.thought === !!part.thought) last.text += part.text
        else merged.content.parts.push(part)
      }
    }
  }
  if (!received) throw new Error('Code Assist stream ended without a response')
  if (candidates.size) result.candidates = [...candidates.values()].sort((a, b) => a.index - b.index)
  return jsonResponse(result, response)
}

export function unwrapStream(response) {
  const events = sseEvents(response)[Symbol.asyncIterator]()
  const encoder = new TextEncoder()
  const stream = new ReadableStream({
    async pull(controller) {
      try {
        while (true) {
          const next = await events.next()
          if (next.done || next.value.data === '[DONE]') { await events.return(); controller.close(); return }
          controller.enqueue(encoder.encode(`data: ${JSON.stringify(unpack(next.value))}\n\n`))
          return
        }
      } catch (error) { controller.error(error); await events.return().catch(() => {}) }
    },
    async cancel() { await events.return().catch(() => {}) },
  })
  const headers = new Headers(response.headers)
  for (const key of ['content-length', 'content-encoding', 'transfer-encoding', 'connection']) headers.delete(key)
  headers.set('content-type', 'text/event-stream')
  return new Response(stream, { status: response.status, headers })
}

export function createCodeAssist({ agent = 'gemini', fetcher = globalThis.fetch, baseURL,
  loadURL = 'https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist', tokenURL = 'https://oauth2.googleapis.com/token',
  now = Date.now, testMode = false } = {}) {
  const app = APPS[agent]
  if (!app) throw new Error('unsupported Code Assist application')
  const base = safeBase(baseURL ?? app.base, 'Code Assist endpoint', testMode)
  const token = safeBase(tokenURL, 'Google token endpoint', testMode)
  const load = safeBase(loadURL, 'Code Assist project endpoint', testMode)
  return {
    auth: {
      provider: `builtin:${agent}`, methods: [], keepAlive: false, refreshLead: 120000,
      async refresh(auth) {
        // A refresh token belongs to the OAuth application that issued it.
        // Import that application's identity from private local configuration;
        // the distributed adapter does not embed another application's secrets.
        if (!auth.clientId || !auth.clientSecret) throw new Error('Google OAuth client identity is missing; import the matching original application clientId/clientSecret into private builtin-oauth-clients.json')
        const body = new URLSearchParams({ client_id: secret(auth.clientId, 'Google OAuth client ID'), client_secret: secret(auth.clientSecret, 'Google OAuth client secret'),
          grant_type: 'refresh_token', refresh_token: secret(auth.refresh, 'Google refresh token') })
        const response = await fetchSafe(fetcher, token, { method: 'POST', body, signal: AbortSignal.timeout(30000),
          headers: { 'content-type': 'application/x-www-form-urlencoded' } })
        const data = await authJSON(response, 'Google token refresh')
        const seconds = Number(data.expires_in)
        if (!Number.isFinite(seconds) || seconds <= 0) throw new Error('Google refresh did not provide an expiry')
        return { access: secret(data.access_token, 'renewed Google access token'),
          refresh: data.refresh_token ? secret(data.refresh_token, 'renewed Google refresh token') : auth.refresh,
          expires: now() + seconds * 1000 }
      },
      async loader(getAuth) {
        let found, pending
        async function project(auth) {
          if (auth.project) return secret(auth.project, 'Google Cloud project')
          if (found) return found
          if (!pending) pending = (async () => {
            const response = await fetchSafe(fetcher, load, { method: 'POST', signal: AbortSignal.timeout(30000),
              headers: { authorization: `Bearer ${secret(auth.access, 'Google access token')}`, 'content-type': 'application/json', 'user-agent': userAgent(agent, app, '') },
              body: JSON.stringify({ metadata: { ideType: agent === 'antigravity' ? 'ANTIGRAVITY' : 'GEMINI_CLI' } }) })
            const data = await authJSON(response, 'Code Assist project discovery')
            const id = typeof data.cloudaicompanionProject === 'string' ? data.cloudaicompanionProject : data.cloudaicompanionProject?.id
            if (!id) throw new Error('Code Assist has no project for this sign-in; complete setup in the original application or configure a project')
            // No onboarding, automatic tier selection, or entitlement changes.
            found = secret(id, 'Google Cloud project')
            return found
          })().finally(() => { pending = undefined })
          return await pending
        }
        return {
          baseURL: base,
          async fetch(url, init) {
            const asked = new URL(url)
            const match = /\/models\/(.+):(streamGenerateContent|generateContent)$/u.exec(asked.pathname)
            if (!match) throw new Error('Code Assist requires a Gemini generateContent endpoint')
            const model = decodeURIComponent(match[1])
            const body = requestBody(init)
            const auth = await getAuth()
            const headers = headersFor(init)
            headers.set('authorization', `Bearer ${secret(auth?.access, 'Google access token')}`)
            headers.set('user-agent', userAgent(agent, app, model)); headers.set('accept', 'text/event-stream')
            const response = await fetchSafe(fetcher, `${base}/v1internal:streamGenerateContent?alt=sse`, { ...init, headers,
              body: JSON.stringify(envelope(agent, model, await project(auth), body)) })
            if (!response.ok) return response
            if (!(response.headers.get('content-type') ?? '').toLowerCase().startsWith('text/event-stream')) throw new Error('Code Assist returned a non-SSE success response')
            return match[2] === 'streamGenerateContent' ? unwrapStream(response) : await collect(response)
          },
        }
      },
    },
  }
}

export default async function () { return createCodeAssist() }
