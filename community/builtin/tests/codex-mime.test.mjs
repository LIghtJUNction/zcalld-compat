import { test, expect } from 'bun:test'
import { createCodex } from '../codex-impl.mjs'

const utf8 = new TextEncoder()
const final = {
  id: 'fixture-response', object: 'response', status: 'completed', model: 'fixture',
  output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'OK你好' }] }],
  usage: { input_tokens: 3, output_tokens: 2 },
  fixture_extension: { kept: true },
}
function frame(type, fields, newline = '\r\n') {
  return 'event: ' + type + newline + 'data: ' + JSON.stringify({ type, ...fields }) + newline + newline
}
function response(bytes, { fragment = 7, contentType, open = false, status = 200 } = {}) {
  bytes = typeof bytes === 'string' ? utf8.encode(bytes) : bytes
  const state = { reads: 0, bytes: 0, cancelled: 0, reason: undefined }
  let at = 0
  const body = new ReadableStream({
    pull(controller) {
      state.reads++
      if (at === bytes.length) {
        if (!open) controller.close()
        return
      }
      const end = Math.min(bytes.length, at + fragment)
      state.bytes += end - at
      controller.enqueue(bytes.slice(at, end)); at = end
    },
    cancel(reason) { state.cancelled++; state.reason = reason },
  }, { highWaterMark: 0 })
  const headers = new Headers({ 'x-fixture': 'kept' })
  if (contentType) headers.set('content-type', contentType)
  return { state, response: new Response(body, { status, headers }) }
}
async function fetchFixture(source, stream) {
  const plugin = createCodex({ fetcher: async () => source })
  const loader = await plugin.auth.loader(async () => ({ access: 'fixture-access' }))
  return await loader.fetch('https://chatgpt.com/backend-api/codex/responses', {
    method: 'POST', body: JSON.stringify({ model: 'fixture', input: 'hello', stream }),
  })
}

test('Codex missing MIME recognizes a fragmented UTF-8 Responses event and replays every byte', async () => {
  const wire = '\uFEFF: heartbeat\r\n\r\n' + frame('response.created', { response: { id: final.id } }) +
    frame('response.output_text.delta', { delta: 'OK你好' }) + frame('response.completed', { response: final })
  const source = response(wire, { fragment: 1 })
  const output = await fetchFixture(source.response, true)
  expect(output.headers.get('content-type')).toBe('text/event-stream')
  expect(output.headers.get('x-fixture')).toBe('kept')
  expect(new Uint8Array(await output.arrayBuffer())).toEqual(utf8.encode(wire))
  expect(source.state.cancelled).toBe(0)
  const aggregate = await fetchFixture(response(wire).response, false)
  expect(aggregate.headers.get('content-type')).toBe('application/json')
  expect(await aggregate.json()).toEqual(final)
})

test('Codex missing MIME only recognizes complete Responses events and cancels rejected bodies', async () => {
  const cases = [
    '{"id":"not-an-event","output":[]}\n\n',
    '<html>not SSE</html>\n\n',
    'data: {"type":"unrelated"}\n\n',
    'event: response.created\ndata: {"type":"response.completed"}\n\n',
    'data: not-json\n\n',
    'data: {"type":"response.created"}\n',
    new Uint8Array([...utf8.encode('data: {"type":"response.created","x":"'), 0xff, ...utf8.encode('"}\n\n')]),
  ]
  for (const bytes of cases) {
    for (const stream of [false, true]) {
      const source = response(bytes)
      await expect(fetchFixture(source.response, stream)).rejects.toThrow()
      // EOF already closes incomplete streams; malformed active streams are cancelled.
      expect(source.response.body.locked).toBe(false)
      expect(source.state.cancelled).toBeLessThanOrEqual(1)
    }
  }
  for (const contentType of ['application/json', 'text/plain', ';charset=utf-8']) {
    for (const stream of [false, true]) {
      const source = response(frame('response.completed', { response: final }), { contentType })
      await expect(fetchFixture(source.response, stream)).rejects.toThrow('non-SSE success')
      expect(source.state.reads).toBe(0)
      expect(source.state.cancelled).toBe(1)
    }
  }
})

test('Codex missing MIME probe has a 64 KiB bound and cancellation retains upstream ownership', async () => {
  const large = response(':' + 'x'.repeat(70 << 10) + '\n\n' + frame('response.completed', { response: final }), { fragment: 127 })
  await expect(fetchFixture(large.response, true)).rejects.toThrow('prefix exceeds 64 KiB')
  expect(large.state.bytes).toBeLessThanOrEqual((64 << 10) + 126)
  expect(large.state.cancelled).toBe(1)
  const source = response(frame('response.created', { response: { id: final.id } }), { fragment: 65536, open: true })
  const output = await fetchFixture(source.response, true)
  expect(source.state.reads).toBe(1)
  await output.body.cancel('fixture client disconnected')
  expect(source.state.cancelled).toBe(1)
  expect(source.state.reason).toBe('fixture client disconnected')
  expect(source.response.body.locked).toBe(false)
})

test('Codex missing MIME preserves failed, incomplete and error event semantics', async () => {
  for (const status of ['completed', 'incomplete', 'failed']) {
    const value = { ...final, status, fixture_status: status }
    const output = await fetchFixture(response(frame('response.' + status, { response: value })).response, false)
    expect(output.status).toBe(200)
    expect(await output.json()).toEqual(value)
  }
  const error = { code: 'fixture_error', message: 'fixture refusal' }
  const output = await fetchFixture(response(frame('error', { error })).response, false)
  expect(output.status).toBe(502)
  expect(await output.json()).toEqual({ error })
  const denied = response('{"error":"fixture denied"}', { status: 403, contentType: 'application/json' })
  expect(await fetchFixture(denied.response, true)).toBe(denied.response)
  expect(denied.state.reads).toBe(0)
  await denied.response.body.cancel()
})

test('Codex inferred MIME replays the remainder of a large first chunk without buffering the whole body', async () => {
  const first = frame('response.created', { response: { id: final.id } })
  const wire = first + ': ' + 'x'.repeat(100000) + '\n\n' + frame('response.completed', { response: final })
  const source = response(wire, { fragment: wire.length })
  const output = await fetchFixture(source.response, true)
  expect(new Uint8Array(await output.arrayBuffer())).toEqual(utf8.encode(wire))
  expect(source.state.cancelled).toBe(0)
})
