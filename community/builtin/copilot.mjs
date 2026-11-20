// The host treats every exported function as a plugin initializer.
// Keep implementation helpers private to the separate module.
import { createCopilot } from './copilot-impl.mjs'
export default async function () { return createCopilot() }
