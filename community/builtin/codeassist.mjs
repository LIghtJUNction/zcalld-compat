// The host treats every exported function as a plugin initializer.
// Keep implementation helpers private to the separate module.
import { createCodeAssist } from './codeassist-impl.mjs'
export default async function () { return createCodeAssist() }
