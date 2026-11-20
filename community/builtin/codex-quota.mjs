// Explicit account usage reads. This path never invokes a loader, auth refresh,
// generation, reset-card operation, or provider-controlled URL.
import { codexQuotaIdentity, codexQuotaWindows } from './codex-credits.mjs'

const MAX_BYTES = 65536
const failure = (error) => ({ ok: false, error })

export function createCodexQuota({ readUsage, externalAuth, accountID, now = Date.now }) {
  return async function codexQuota(stored, request) {
    let response
    try {
      if (!stored || stored.type !== 'oauth' ||
          ![undefined, 'host', 'external'].includes(stored.refreshOwner))
        return failure('native_auth_unavailable')
      let auth
      try { auth = stored.refreshOwner === 'external' ? await externalAuth(stored, now()) : stored }
      catch { return failure('native_auth_unavailable') }
      const account = accountID(auth)
      if (!account) return failure('native_auth_unavailable')
      const identity = codexQuotaIdentity(auth, account)
      // Bind one GET to the already resolved credential. The original native
      // file is read again only to reject a concurrent identity/token change.
      const snapshot = Object.freeze({ type: 'oauth', access: auth.access,
        accountId: account, expires: auth.expires })
      response = await readUsage(snapshot, {
        operation: 'usage', account_hint: account, max_response_bytes: MAX_BYTES,
        timeout_ms: request.timeout_ms, signal: request.signal,
      })
      if (!response.ok) {
        const status = response.status
        await response.body?.cancel().catch(() => {})
        return failure(status === 401 || status === 403 ? 'native_auth_unavailable'
          : status === 504 ? 'timeout' : 'provider_error')
      }
      if (request.signal?.aborted) { await response.body?.cancel().catch(() => {}); return failure('timeout') }
      // readUsage already enforces this bound. Recheck the returned response so
      // a replacement fixture cannot accidentally hide a missing transport cap.
      const reader = response.body?.getReader()
      if (!reader) return failure('provider_error')
      const stop = () => { void reader.cancel().catch(() => {}) }
      request.signal?.addEventListener('abort', stop, { once: true })
      if (request.signal?.aborted) stop()
      const chunks = []
      let size = 0
      try {
        while (true) {
          const { value, done } = await reader.read()
          if (done) break
          size += value.byteLength
          if (size > MAX_BYTES || request.signal?.aborted) throw new Error('bounded quota read ended')
          chunks.push(value)
        }
      } catch {
        await reader.cancel().catch(() => {})
        return failure(request.signal?.aborted ? 'timeout' : 'provider_error')
      } finally { request.signal?.removeEventListener('abort', stop); reader.releaseLock() }
      const bytes = new Uint8Array(size)
      let offset = 0
      for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength }
      const at = now()
      const parsed = codexQuotaWindows(JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes)), at)
      if (!parsed.known || parsed.windows.some(w => w.used > 100 ||
          w.span_seconds > 366 * 86400 ||
          (w.resets_at !== null && !Number.isSafeInteger(w.resets_at * 1000))))
        return failure('provider_error')
      if (stored.refreshOwner === 'external') {
        let current
        try { current = await externalAuth(stored, now()) }
        catch { return failure('native_auth_unavailable') }
        if (codexQuotaIdentity(current, accountID(current)) !== identity)
          return failure('account_or_plugin_changed')
      }
      if (request.signal?.aborted) return failure('timeout')
      return { ok: true, at: new Date(at).toISOString(), windows: parsed.windows.map(w => ({
        name: w.name, used: w.used, span_seconds: w.span_seconds,
        ...(w.resets_at === null ? {} : { resetsAt: new Date(w.resets_at * 1000).toISOString() }),
      })) }
    } catch {
      await response?.body?.cancel().catch(() => {})
      return failure(request.signal?.aborted ? 'timeout' : 'provider_error')
    }
  }
}
