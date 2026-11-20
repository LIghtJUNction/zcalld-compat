import { test, expect } from 'bun:test'
import { prepare } from '../codex-impl.mjs'
test('native compaction controls retain opaque IDs and ciphertext without changing ordinary item normalization',()=>{
  const opaque='opaque jane.doe@acme.io 15551234567 sk-proj-Wn4rT7yQ2mLx9VbK3cZp /+=='
  const controls=['compaction','compaction_summary','context_compaction'].map(type=>({
    type,id:'cmp_fixture',encrypted_content:opaque,
    internal_chat_message_metadata_passthrough:{fixture:true},
  }))
  const input=[{type:'message',id:'message-id',role:'system',content:'fixture'},
    ...controls,{type:'compaction_trigger'}]
  const original={model:'fixture',input,stream:false}
  const prepared=prepare(original)
  expect(prepared.input.slice(1,4)).toEqual(controls)
  expect(prepared.input[4]).toEqual({type:'compaction_trigger'})
  expect(prepared.input[0].id).toBeUndefined()
  expect(prepared.input[0].role).toBe('developer')
  expect(original.input[0].id).toBe('message-id')
  expect(original.stream).toBe(false)
})
