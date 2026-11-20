# 使用原生 Claude CLI 作为可选提供者

Claude provider 把已登录的原生 Claude CLI 接到 zcalld 的 `/v1/messages`。客户端使用 zcalld 的路由名，生成由明确绑定的原生账号执行；支持普通 JSON、SSE、客户端工具和内联图片。生成始终经过真正的 PTY／ConPTY 交互，不会因交互失败而自动改用 `-p`。

**验收状态（2026-11-20）：Linux x86_64 的 10 项隔离 provider 全链路测试全部通过；zcalld-compat 0.3.1 已完成五平台的正式 helper 与维护命令验收。core 的五平台 provider 全链路门禁仍待本轮正式发布完成。** 测试使用合成账号和编译出的假 Claude CLI，没有调用真实 Claude 服务。本文需要包含本功能的配套 zcalld 与 zcalld-compat；仅有 Claude 维护功能的旧版本不足以提供此接口。

## 最快上手：已有原生 Claude 登录

以下是 Linux／Arch 的 Bash 示例。先安装配套的 core、compat 包，并准备自己已经正常使用的 Claude CLI 登录配置。接入不需要 Magpie，也不需要导出 OAuth token。首次绑定默认关闭自动维护并启用 `NoExtra` 额度保护。

### 1. 确定本机路径

使用现有 zcalld 配置；只有尚未创建配置时才执行 `init`。如果你的服务使用其他配置文件，请修改 `ZCALL_CONFIG`，后续所有命令始终使用同一个路径。

```bash
ZCALL_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/zcalld/config.conf"
ZCALL_SETUP="$(dirname "$ZCALL_CONFIG")/claude-provider-setup"
CLAUDE_BINARY="$(command -v claude)"
CLAUDE_CONFIG_DIR="$HOME/.claude"
CLAUDE_PROFILE="$HOME/.claude.json"
CLAUDE_OWNERSHIP=current

umask 077
test -f "$ZCALL_CONFIG" || zcalld init --config "$ZCALL_CONFIG"
install -d -m 700 -- "$ZCALL_SETUP"
/usr/bin/zcalld-claude-tui --capabilities
```

`CLAUDE_BINARY` 必须指向你现有的 Claude 可执行文件；如果命令不在 PATH 中，直接填写其绝对路径。`/usr/bin/zcalld-claude-tui` 是 Arch 包提供的稳定入口。其他 Linux 安装方式应填写自己配套安装的 helper 路径。`--capabilities` 只返回 helper 能力，不启动 Claude、不读取账号。

上面使用默认原生布局。若你有已经独立保存的原生配置，在下一步前改用以下三个变量；**不要为接入而复制当前账号的身份文件或凭据**。

```bash
# 改成你已经存在的、属于当前用户的私有原生配置目录。
CLAUDE_CONFIG_DIR="$HOME/.local/share/claude-accounts/work"
CLAUDE_PROFILE="$CLAUDE_CONFIG_DIR/.claude.json"
CLAUDE_OWNERSHIP=saved
```

| 布局 | `config_dir` | `profile` | `ownership` |
|---|---|---|---|
| 默认原生账号 | `$HOME/.claude` | `$HOME/.claude.json` | `current` |
| 独立保存的原生账号 | 已存在的私有目录 | 该目录内的 `.claude.json` | `saved` |

绑定文件和 profile 必须属于当前用户且为私有文件；Unix 通常为 `0600`。保存的账号目录必须私有，通常为 `0700`。当前账号的 `.claude` 可以保持原生 CLI 常见的 `0755`，但不能允许其他用户写入。配置目录和 profile 的路径组成部分不得是符号链接；绑定流程不会修改这些原生路径的权限。

### 2. 声明并绑定这一个账号

下面生成完整的独立绑定声明，不含迁移专用的 `login_id` 或 `user_hash`。Python 只负责正确转义路径；不会启动 Claude。文件已存在时会停止，避免覆盖先前的声明。

```bash
python3 - "$ZCALL_SETUP/bindings.json" "$CLAUDE_BINARY" \
  "$CLAUDE_CONFIG_DIR" "$CLAUDE_PROFILE" "$CLAUDE_OWNERSHIP" <<'PY'
import json
import os
import sys

target, binary, config_dir, profile, ownership = sys.argv[1:]
if not os.path.isabs(binary) or not os.access(binary, os.X_OK):
    raise SystemExit("请将 CLAUDE_BINARY 设为现有 Claude 可执行文件的绝对路径")
binding = {
    "version": 1,
    "claude_binary": binary,
    "interactive_helper": "/usr/bin/zcalld-claude-tui",
    "trust_workspace": False,
    "accounts": [{
        "config_dir": config_dir,
        "profile": profile,
        "ownership": ownership,
        "enabled": True,
    }],
}
with open(target, "x", encoding="utf-8") as output:
    json.dump(binding, output, ensure_ascii=False, indent=2)
    output.write("\n")
PY

zcalld plugins claude-provider bind plan \
  --file "$ZCALL_SETUP/bindings.json" --config "$ZCALL_CONFIG" --json \
  > "$ZCALL_SETUP/plan.json"
cat "$ZCALL_SETUP/plan.json"
```

检查计划中的账号数量和 `account_ids`。确认这是你选定的一个账号后，应用刚刚审核的计划：

```bash
PLAN_HASH="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["plan_hash"])' \
  "$ZCALL_SETUP/plan.json")"

zcalld plugins claude-provider bind apply \
  --file "$ZCALL_SETUP/bindings.json" --expect-plan "$PLAN_HASH" \
  --config "$ZCALL_CONFIG" --json > "$ZCALL_SETUP/applied.json"
cat "$ZCALL_SETUP/applied.json"
```

成功结果应有 `applied: true`、`credentials_imported: false`、`scheduler_changed: false`。保存返回的 `account_ids`：下一步需要其中精确的、不透明的账号键。不要按邮箱拼接账号键，也不要在多账号结果中随意选择第一项。

计划和应用只读取明确选择的原生身份，不启动 CLI、不登录、不刷新 token。真正执行请求时，原生 Claude CLI 仍管理自己的登录状态。账号身份或声明发生变化后，旧计划失效，需要重新查看计划。重绑同一已知身份会保留已有的限额、维护偏好和账本，不把它们重置成新账号默认值。[绑定实现](../claudemaintenance/native_binding.go)

### 3. 选择已有 Community 或新装原生模式

**已经启用普通 Community 插件：** 保留现有 Community 注册、账号和绑定。升级配套组件后，按已有运行时升级流程配置当前 helper；普通模式仍需要原有 Bun 环境：

```bash
zcalld plugins runtime configure --config "$ZCALL_CONFIG" --json
```

然后直接进入第 4 步。混合模式会把明确的 `builtin:claude` 请求分派给原生 provider，其他插件继续走原路径。不要再注册第二个原生模式实例，也不要用原生模式输出覆盖现有 `host.json`。

**尚未启用普通 Community 的新安装：** 采用原生模式即可，无须额外安装外部 Bun。以下命令读取配置并通过现有插件安装事务注册已安装的 Community 动态库：

```bash
zcalld plugins claude-provider native-config \
  --config "$ZCALL_CONFIG" --json > "$ZCALL_SETUP/native.json"

COMMUNITY_LIB=/usr/lib/zcalld/plugins/libzcalld_community.so
COMMUNITY_SHA="$(sha256sum "$COMMUNITY_LIB" | cut -d ' ' -f 1)"

zcalld plugins install "$COMMUNITY_LIB" --name claude-native \
  --sha256 "$COMMUNITY_SHA" --config "$ZCALL_CONFIG" --json
zcalld plugins config claude-native --file "$ZCALL_SETUP/native.json" \
  --config "$ZCALL_CONFIG" --json
zcalld plugins enable claude-native --config "$ZCALL_CONFIG" --json
```

这个 `.so` 路径适用于标准 Linux 安装；使用私有安装前缀时改成该安装的配套库。`native-config` 是只读生成命令，遇到既有普通 Community 配置会拒绝转换。它只在显式原生模式下免除 Bun 要求，不会放宽普通 Community 的校验。[原生模式实现](../claudemaintenance/provider_api.go)

### 4. 添加 provider，再用 CLI 创建路由

当前 `providers add/edit` 还没有写入 `plugin`、`plugin_account`、`auth` 的命令参数。因此先备份并编辑 `$ZCALL_CONFIG`，保留其他内容，添加以下唯一小节。将 `RETURNED_ACCOUNT_ID` 替换为上一步 `account_ids` 中你确认的完整账号键：

```ini
[provider claude-native]
base_url = http://127.0.0.1:9/v1
protocol = anthropic
auth = none
plugin = builtin:claude
plugin_account = RETURNED_ACCOUNT_ID
```

`base_url` 是当前 core 配置格式的必填项；这里的本机地址是占位，`builtin:claude` 不使用它向供应商发送 HTTP。登录和请求由绑定的原生 CLI 处理。不要填写 API key，也不要省略明确的 `plugin_account`。

用已存在的路由 CLI 添加客户端可选名称：

```bash
zcalld routes set claude/haiku --provider claude-native --model haiku \
  --config "$ZCALL_CONFIG" --json
zcalld routes list --config "$ZCALL_CONFIG" --json
zcalld check --config "$ZCALL_CONFIG" --json
```

`claude/haiku` 是客户端选择的路由名；`haiku` 是交给原生 CLI 的模型名。可以换成你原生 CLI 实际支持的明确模型名。新增其他路由时继续使用 `routes set NAME --provider PROVIDER --model MODEL`。这不自动修改任何 Agent 配置。

新装可以前台启动：

```bash
zcalld serve --config "$ZCALL_CONFIG"
```

已有服务应在检查通过后按原有服务管理方式重启，并确认它使用同一个配置文件；不要在相同监听地址再启动第二个网关。然后在使用 Anthropic Messages 接口的客户端中选择 `claude/haiku`。启动服务不等于发送生成请求；主动发送请求会使用绑定账号的额度。

provider 小节结构、真实 core CLI 的绑定及插件安装流程已进入本轮全链路测试；`routes set` 的语法由 core 的控制层夹具覆盖。不要使用不存在的 `providers add --plugin-account` 参数。[全链路夹具](../scripts/test-claude-provider.py) · [core 路由 CLI](https://github.com/LIghtJUNction/zcalld/blob/1c1441404ddc54416cf24b57d276958a8b98b811/src/maintenance.rs) · [core 控制层测试](https://github.com/LIghtJUNction/zcalld/blob/1c1441404ddc54416cf24b57d276958a8b98b811/scripts/test-control.py)

## 支持范围

| 能力 | 当前行为 | 限制 |
|---|---|---|
| 普通与流式请求 | `/v1/messages` 的 JSON 和 SSE；文本、system、完整对话历史 | 本页未把其他客户端协议的转换列为已验收合同 |
| 输出控制 | `max_tokens` 为 1–128,000；`output_config.effort` 为 low、medium 或 high | 输出限制传给原生 CLI，不是供应商级的绝对计费上限 |
| 客户端工具 | 工具定义、并行调用、整组结果续接、`is_error`；auto／none 选择 | 不支持强制指定工具；工具由调用客户端执行，不提供任意本地命令执行入口 |
| 图片 | 内联 base64 PNG、JPEG、GIF、WebP；每张解码后最多 4 MiB | 不读取远程图片 URL，不接受未知内容类型 |
| 历史 | 校验完整前序对话；同一挂起工具会话可以续接 | 新会话不重放外来的 opaque thinking 或 redacted thinking；不提供跨重启恢复、idle rewind 或自动 replay |

请求体最多 8 MiB，最多 1,024 条消息、64 个工具。网关自身更小的上限仍然生效。未知字段、重复 JSON 键和不支持的结构会被明确拒绝，不会悄悄丢弃。当前未支持的字段包括 `temperature`、`top_p`、`stop_sequences`、`metadata`、显式 `thinking` 控制以及结构化输出格式控制。[请求合同](../claudeprovider/request.go)

本功能使用原生 CLI 的明确账号和交互能力，并不等于完整复刻 Claude Code 的所有终端功能，也不表示这些管理命令已有 GUI 控件。

## 工具续接与取消

收到工具调用后，客户端执行工具，并将**完整前序对话、本轮完整 assistant 回复和全部并行工具结果**一起返回。续接必须匹配账号、模型、工具定义及上下文；重复结果、缺失结果、跨账号或不一致的历史会被拒绝。工具标识由服务端随机生成，应按会话凭证对待；现有接口并未提供独立的可信 caller 身份，因此不能把工具标识当成调用者身份认证。[会话实现](../claudeprovider/session.go)

每个原生配置同时只允许一个活动会话；单个 worker 最多保留四个活动或尚未确认排空的会话。工具会话最多 16 轮，单次等待工具结果最多 120 秒，完整会话受首次请求剩余时间及 300 秒上限约束。续接不会重置总期限。

客户端取消或期限到达后，helper 清理自己拥有的进程组／Job，再释放账号租约。如果无法确认进程已排空，账号会继续被阻止复用。不要删除运行标记来强行绕过：先确认之前的原生进程和回合已经结束，再恢复使用。取消和未知结果不会被自动重放。[进程与租约](../claudemaintenance/provider_api.go)

## 额度与维护是不同操作

新绑定默认 `NoExtra=true`、自动维护关闭。有适用限额的账号在生成和工具续接前通过固定的 `-p /usage` 读取额度；未知、畸形或已达到适用上限的结果不会被当成可用额度继续生成。与当前模型无关的已知限额窗口不会被误当作该模型的限额。

这是一次已完成额度读数上的预检，不是供应商的费用预留或禁止计费开关；其他客户端或当前请求仍可能改变额度。额度读取与生成分开：**`-p /usage` 只用于额度，生成始终使用交互 helper。** 维护设置中的 print 偏好不会让 provider 自动退回 print 生成。

绑定和启用 provider 不会启用定时预热。需要自动维护时，另外阅读并审核 [Claude 维护说明](CLAUDE-MAINTENANCE.md)。从迁移来源标记为过期或暂停的账号不会被自动启用；独立绑定是用户的明确选择，不作为迁移绕过手段。

## 常见结果与处理

| 结果 | 下一步 |
|---|---|
| 绑定不可用或身份变化 | 核对明确的可执行文件、两个原生路径、权限和当前登录身份，再重新查看绑定计划；不要复制 token 修补 |
| `native-config` 拒绝已有 Community | 保留已有混合模式，按第 3 步的已有安装流程处理 |
| 不支持请求，HTTP 400 | 按本页支持表删去未支持字段，或改用支持该能力的其他路由 |
| 工具续接不可用，HTTP 409 | 核对完整历史和整组结果；过期或已消费的工具标识不能重用 |
| 已达适用额度或会话上限，HTTP 429 | 等待额度或现有会话释放，或由你明确选择其他账号／路由 |
| 超时或进程退出未确认 | 先确认上次回合和原生进程状态，避免自动重复一个可能已执行的请求 |

## 验证与来源

2026-11-20 的 Linux x86_64 验收使用实际编译的 Rust 网关、Go worker、PTY helper 和假 Claude CLI，10／10 通过。它覆盖独立绑定、当前／保存两种布局、无外部 Bun 的原生模式、JSON／SSE、图片、并行工具及重复结果拒绝、身份改变、额度阻断、取消排空，以及普通 Community 模式缺 Bun 时仍拒绝。测试没有真实 Claude 执行、供应商请求或设备状态变更。

**compat 0.3.1 的正式原生验收已通过五个平台。** [正式运行 44791806657](https://github.com/LIghtJUNction/zcalld-compat/actions/runs/44791806657) 使用 Go 1.26.8，固定源码为 [`a634b0f34a2054d66c4dd3d51cce94fe90b20735`](https://github.com/LIghtJUNction/zcalld-compat/commit/a634b0f34a2054d66c4dd3d51cce94fe90b20735)。各平台均实际执行 Claude helper 和维护命令夹具；表中这些原生用例没有跳过。

| 正式平台 | Claude helper 场景 | 维护 CLI 场景 | 进程清理回归 |
|---|---:|---:|---:|
| Linux x86_64 | 9／9 | 8／8 | 2／2 |
| Linux ARM64 | 9／9 | 8／8 | 2／2 |
| macOS x86_64 | 9／9 | 8／8 | 2／2 |
| macOS ARM64 | 9／9 | 8／8 | 2／2 |
| Windows x86_64 | 9／9 | 8／8 | 4／4 |

**Windows 的退出确认有独立的失败与修复证据。** [0.3.0 的正式 Windows 失败](https://github.com/LIghtJUNction/zcalld-compat/actions/runs/44788342484/job/136469093274) 显示，Job 的活动进程计数已归零时，测试中的自有后代进程句柄仍未进入已退出状态。0.3.1 保存并核验所属 Job 的进程句柄，在原有截止时间内等待实际退出确认；无法确认时仍返回清理失败。[实现](https://github.com/LIghtJUNction/zcalld-compat/blob/a634b0f34a2054d66c4dd3d51cce94fe90b20735/claudemaintenance/process_windows.go)保留了原来的强退出断言，并新增 stop→finish 与退出确认回归；[Windows 候选运行](https://github.com/LIghtJUNction/zcalld-compat/actions/runs/44791352868)及随后正式运行均通过。原 0.3.0 标签和失败记录保留，修复以新版本发布。

这一退出边界也符合 Windows API 的语义：[TerminateJobObject](https://learn.microsoft.com/en-us/windows/win32/api/jobapi2/nf-jobapi2-terminatejobobject) 对 Job 成员执行终止，而 [TerminateProcess](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-terminateprocess) 可以异步返回；实际退出应通过进程句柄等待确认。这里的验收范围是夹具中已核验归属的自有进程，不表示能够强行结束所有第三方 CLI 状态；退出未知时仍应保持账号不可复用的保守处理。

**完整网关 provider 的五平台正式结果仍待补齐。** 上述 Linux 10 项 provider 门禁使用 Go 1.27.4；本次 compat 正式运行使用 Go 1.26.8，验证的是 helper、维护命令和进程清理。后续 core 正式运行还需执行实际 Rust 网关、Community 插件与这些 helper 组成的 10 项 provider 场景。当前结果不证明全部 Claude 版本、模型或真实账号环境均已验证。

正式 [v0.3.1 发行](https://github.com/LIghtJUNction/zcalld-compat/releases/tag/v0.3.1)已发布五平台 helper 和源码制品。源码归档 `zcalld-compat-0.3.1-source.tar.gz` 为 861,625 字节，发行资产记录的 SHA-256 为 `ddb48aafa3d2605cc77caf9c5d82d7e8328559b6865f45d9b3ef917f50932cd6`；它标识源码资产，不是本机 provider 测试回执。

本轮 provider 验收回执的 SHA-256 为 `9c243a855a975e1355ccf64059f913c48cdcfd3f21f25a8525b7d9a6d1754fd7`；它标识测试回执，不是发行包哈希。可复现入口是 [test-claude-provider.py](../scripts/test-claude-provider.py)，使用配套 core、Community 库、pluginctl、Claude helper 和编译出的假 CLI；不需要真实 Claude 登录。

PTY／ConPTY helper 的上游来源固定为 [Magpie 1e174f8f9f613a920203a4658d259456fcab5d0d](https://github.com/yetone/magpie/tree/1e174f8f9f613a920203a4658d259456fcab5d0d)，文件来源与 MIT 归属记录见 [CLAUDE-TUI-SOURCE.json](CLAUDE-TUI-SOURCE.json)。本功能没有引入 Magpie 的 GUI 或完整账号目录管理，也不提供 WSL 身份路径转换。
