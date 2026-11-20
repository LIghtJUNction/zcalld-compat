// The host treats every exported function as a plugin initializer.
// Keep implementation helpers private to the separate module.
import { createCodex } from './codex-impl.mjs'
export default async function () { return createCodex() }
