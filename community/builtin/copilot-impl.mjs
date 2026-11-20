// Copilot subscription transport. Protocol choices come from the imported model
// catalog, never from an assumed price, subscription tier, or model-name prefix.
import { authJSON, fetchSafe, headersFor, requestBody, safeBase, secret, protocolFromPath } from './common.mjs'

export const PROVIDER = 'builtin:copilot'
const BASE = 'https://api.githubcopilot.com'
const EDITOR_HEADERS = {
  'editor-version': 'vscode/1.140.0', 'editor-plugin-version': 'copilot-chat/0.68.0',
  'copilot-integration-id': 'vscode-chat', 'user-agent': 'GitHubCopilotChat/0.68.0',
  'x-github-api-version': '2026-01-09',
}
const CLI_HEADERS = {
  'editor-version': 'copilot/1.0.88', 'copilot-integration-id': 'copilot-developer-cli',
  'x-github-api-version': '2026-07-01', 'user-agent': 'copilot/1.0.88',
}

export function lastRole(body) {
  if (Array.isArray(body.messages) && body.messages.length) {
    const last = body.messages.at(-1)
    if (last?.role === 'user' && Array.isArray(last.content) && last.content.length &&
      last.content.every((part) => part?.type === 'tool_result')) return 'tool'
    return last?.role ?? ''
  }
  if (typeof body.input === 'string') return 'user'
  return Array.isArray(body.input) ? body.input.at(-1)?.role ?? '' : ''
}

function githubBase(host) {
  if (!host || host === 'github.com') return 'https://api.github.com'
  if (!/^[a-z0-9][a-z0-9.-]*\.ghe\.com$/iu.test(host)) throw new Error('unsupported Copilot enterprise host')
  return `https://api.${host}`
}

function nativeProtocols(info, model) {
  const data = info?.models?.[model]
  const values = data?.native_endpoints ?? data?.supported_endpoints ?? data?.api?.native_endpoints
  if (!Array.isArray(values)) return undefined
  return values.map((path) => {
    if (['chat', 'responses', 'anthropic'].includes(path)) return path
    try { return protocolFromPath(`https://copilot.invalid${path.startsWith('/') ? '' : '/'}${path}`) }
    catch { return '' }
  })
}

export function createCopilot({ fetcher = globalThis.fetch, tokenURL, userURL, baseURL = BASE,
  now = Date.now, testMode = false } = {}) {
  const fallback = safeBase(baseURL, 'Copilot endpoint', testMode)
  return {
    auth: {
      provider: PROVIDER, methods: [], keepAlive: false,
      async loader(getAuth, info) {
        let cached, pending
        async function session() {
          const auth = await getAuth()
          const github = secret(auth?.access ?? auth?.key, 'Copilot GitHub token')
          const direct = auth?.tokenKind === 'cli'
          if (cached?.github === github && cached.expires > now() + 120000) return cached
          if (pending?.github === github) return await pending.promise
          const promise = (async () => {
            const root = githubBase(auth?.host)
            const endpoint = direct ? userURL ?? `${root}/copilot_internal/user` : tokenURL ?? `${root}/copilot_internal/v2/token`
            const headers = new Headers(direct ? {} : EDITOR_HEADERS)
            headers.set('authorization', `token ${github}`); headers.set('accept', 'application/json')
            const response = await fetchSafe(fetcher, safeBase(endpoint, 'Copilot session endpoint', testMode),
              { headers, signal: AbortSignal.timeout(30000) })
            const data = await authJSON(response, 'Copilot session')
            const token = direct ? github : secret(data.token, 'Copilot session token')
            const expires = direct ? now() + 30 * 60000 : Number(data.expires_at) * 1000
            if (!Number.isFinite(expires) || expires <= now()) throw new Error('Copilot returned an expired session')
            const base = safeBase(data.endpoints?.api || auth?.apiBase || fallback, 'Copilot inference endpoint', testMode)
            cached = { github, token, expires, base, direct }
            return cached
          })()
          const job = { github, promise }; pending = job
          try { return await promise } finally { if (pending === job) pending = undefined }
        }
        return {
          baseURL: fallback,
          async fetch(url, init) {
            const protocol = protocolFromPath(url)
            const body = requestBody(init)
            const allowed = nativeProtocols(info, body.model)
            if (allowed && !allowed.includes(protocol)) throw new Error('the imported Copilot catalog does not support this model on the selected protocol')
            const current = await session()
            const headers = headersFor(init)
            for (const [name, value] of Object.entries(current.direct ? CLI_HEADERS : EDITOR_HEADERS)) headers.set(name, value)
            headers.set('authorization', `Bearer ${current.token}`)
            headers.set('openai-intent', 'conversation-panel')
            headers.set('x-initiator', lastRole(body) === 'user' ? 'user' : 'agent')
            if (/"(?:image_url|input_image)"|"type"\s*:\s*"image"/u.test(init.body)) headers.set('copilot-vision-request', 'true')
            else headers.delete('copilot-vision-request')
            const path = { chat: '/chat/completions', responses: '/responses', anthropic: '/v1/messages' }[protocol]
            if (!path) throw new Error('Copilot supports Chat, Responses, and Anthropic Messages')
            const response = await fetchSafe(fetcher, `${current.base}${path}${new URL(url).search}`, { ...init, headers })
            // A failed session is invalidated for the next call; never replay a
            // generation whose upstream may already have consumed it.
            if (response.status === 401 && cached === current) cached = undefined
            return response
          },
        }
      },
    },
  }
}

