# Routing compatibility attribution

The condition, time-window, token-length and folder normalization logic in
routing/rules.go is adapted from yetone/magpie at commit
1e174f8f9f613a920203a4658d259456fcab5d0d:

- internal/provider/grouprule.go
- internal/provider/rulecond.go
- internal/provider/rulefolder.go
- internal/provider/ruleparse.go

The request-text behavior follows internal/gateway/ir.go. Directory ownership
verification and the worker protocol are zcalld-specific: untrusted request
headers and prompt text never create a trusted workspace.

Upstream license:

MIT License

Copyright (c) 2026 yetone

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
