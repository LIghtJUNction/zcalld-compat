// Plugin-tools MCP compatibility for zcalld. Native transport validates the
// HTTP caller and prepares JSON-RPC rows; this module never opens a listener.
// Naming and tool semantics follow Magpie tools_mcp.go (MIT, see NOTICE_MCP.md).
const SERVER = "magpie-tools"
const locks = new Map()

export function toolPrefix(spec) {
  let name = String(spec).replace(/^npm:/, "")
  if (/^(?:https?:|git\+|github:)/.test(name)) {
    name = name.split("#", 1)[0].replace(/\.git$/, "")
  } else {
    const at = name.lastIndexOf("@")
    if (at > 0) name = name.slice(0, at)
  }
  name = name.split(/[/\\]/).at(-1) || ""
  for (const prefix of ["middleware-", "opencode-", "tools-", "page-", "agent-"]) {
    if (name.startsWith(prefix) && name.length > prefix.length) { name = name.slice(prefix.length); break }
  }
  name = name.replace(/-auth$/, "").replace(/[^A-Za-z0-9_-]+/g, "-").replace(/^[-_]+|[-_]+$/g, "")
  return (name || "plugin").slice(0, 24)
}

export function namedTools(plugins, one = "") {
  const out = [], seen = new Set()
  for (const plugin of plugins) {
    if (plugin.error || !plugin.tools?.length) continue
    const prefix = plugin.mcpPrefix || toolPrefix(plugin.spec)
    if (one && one !== prefix) continue
    for (const tool of plugin.tools) {
      if (!/^[A-Za-z0-9_-]{1,48}$/.test(tool.name)) continue
      const base = `${prefix}_${tool.name}`
      let name = one ? tool.name : base.slice(0, 64)
      if (!one) for (let i = 2; seen.has(name); i++) {
        const suffix = `_${i}`; name = base.slice(0, 64 - suffix.length) + suffix
      }
      seen.add(name)
      out.push({ spec: plugin.spec, original: tool.name, name, description: tool.description || "", inputSchema: tool.schema && typeof tool.schema === "object" && !Array.isArray(tool.schema) ? tool.schema : {type:"object",properties:{}} })
    }
    if (one) break // Magpie's per-plugin endpoint chooses the first matching plugin.
  }
  return out
}

function allowed(acl, name) {
  return (acl?.grants || []).every(list => !list.length || list.includes(SERVER) || list.includes(`${SERVER}/${name}`))
}

function abortError() { return new DOMException("MCP request cancelled", "AbortError") }

function take(id, max, signal) {
  if (signal.aborted) return Promise.reject(abortError())
  let state = locks.get(id)
  if (!state) { state = { active: 0, queue: [] }; locks.set(id, state) }
  return new Promise((resolve, reject) => {
    const row = { max, signal, resolve, reject, abort: null }
    row.abort = () => {
      const i = state.queue.indexOf(row)
      if (i >= 0) state.queue.splice(i, 1)
      reject(abortError())
      drain()
    }
    function drain() {
      while (state.queue.length && state.active < state.queue[0].max) {
        const next = state.queue.shift()
        next.signal.removeEventListener("abort", next.abort)
        if (next.signal.aborted) { next.reject(abortError()); continue }
        state.active++
        let released = false
        next.resolve(() => {
          if (released) return
          released = true; state.active--; drain()
        })
      }
      if (!state.active && !state.queue.length) locks.delete(id)
    }
    state.queue.push(row)
    signal.addEventListener("abort", row.abort, {once:true})
    drain()
  })
}

async function withLimits(acl, signal, run) {
  const release = []
  // A common order avoids issuer/child deadlocks across concurrent callers.
  const limits = [...(acl?.concurrency || [])].sort((a,b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0)
  try {
    for (const limit of limits) release.push(await take(limit.id, limit.max, signal))
    if (signal.aborted) throw abortError()
    return await run()
  } finally { for (const fn of release.reverse()) fn() }
}

const toolResult = (text, isError) => ({content:[{type:"text",text}],isError})
const rpcError = (code, message) => ({code,message})

// id_json is already one validated JSON scalar. Keeping it as bytes preserves
// numeric request IDs above JavaScript's safe integer range without a new parser.
function reply(row, key, value) {
  return `{"jsonrpc":"2.0","id":${row.id_json},"${key}":${JSON.stringify(value)}}`
}

export async function answerTools(params, plugins, invoke, signal) {
  const tools = namedTools(plugins, params.one || "")
  const rows = [], max = Math.min(params.max_response_bytes || 16 << 20, 16 << 20)
  let bytes = params.batch ? 2 : 0
  for (const request of params.requests || []) {
    if (signal.aborted) throw abortError()
    let response = request.reply_json
    if (response === undefined) {
      const name = request.params?.name
      if (request.method === "tools/list") {
        response = reply(request,"result",{tools:tools.filter(t => allowed(params.acl,t.name)).map(({name,description,inputSchema}) => ({name,description,inputSchema}))})
      } else if (request.method !== "tools/call") {
        response = reply(request,"error",rpcError(-32601,"method not found"))
      } else if (!allowed(params.acl,name)) {
        response = reply(request,"error",rpcError(-32602,"caller is not permitted to use this MCP tool"))
      } else {
        const tool = tools.find(t => t.name === name)
        if (!tool) response = reply(request,"error",rpcError(-32602,"unknown tool: no enabled plugin supplies it"))
        else {
          let result
          try {
            result = await withLimits(params.acl,signal,() => invoke({spec:tool.spec,name:tool.original,args:request.params?.arguments || {},ms:Math.min(params.timeout_ms || 55_000,55_000),agent:params.agent || ""}, signal))
            result = toolResult(result.text || "",false)
          } catch (error) {
            if (signal.aborted) throw abortError()
            result = toolResult(String(error?.message || "Community tool failed").slice(0,4096),true)
          }
          response = reply(request,"result",result)
        }
      }
    }
    bytes += Buffer.byteLength(response) + (rows.length ? 1 : 0)
    if (bytes > max) throw new Error("MCP response exceeds configured limit")
    rows.push(response)
  }
  return Buffer.from(params.batch ? `[${rows.join(",")}]` : rows[0] || "")
}
