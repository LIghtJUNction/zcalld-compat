# Community tools MCP compatibility

The optional community tools MCP adapter follows the behavior and naming rules of
Magpie, copyright its upstream authors, distributed under the MIT license.

Pinned reference: `yetone/magpie` commit
`1e174f8f9f613a920203a4658d259456fcab5d0d`.

Relevant upstream source:

- `internal/gateway/tools_mcp.go`: aggregate/per-plugin tool names and execution results.
- `internal/gateway/search_mcp.go`: MCP initialization, notifications, HTTP semantics.
- `internal/access/mcp.go`: caller and issuer permission intersection.
- `internal/pluginrt/config.go` and `plugins/tools/tools.go`: built-in tool switch.
- `internal/plugin/tools.go`: persisted in-use note and bounded tool execution.

Source is available at
https://github.com/yetone/magpie/tree/1e174f8f9f613a920203a4658d259456fcab5d0d.
The upstream MIT license is retained in this repository's existing license files.

`community/vendor/mcp-tools.mjs` contains the tool-facing portion of the adapter.
The native `HttpPlugin` transport lives in zcalld and reuses its configured
listener. This compatibility package starts no additional network listener.

Importing the original built-in switch uses `gopkg.in/yaml.v3` version `v3.0.1`.
Its license is included as `LICENSES/gopkg.in-yaml.v3.txt`. YAML parsing is only
used by explicit configuration migration, never by the native gateway core.

The script bundle is embedded into `zcalld-pluginctl` and materialized into a
content-addressed private directory. This includes the host, pi compatibility
module, and MCP module, so installed helpers do not depend on a development tree.
