# local-ai-relay

面向 Codex 等 Responses API 客户端的本地 Go 转发代理。所有接入同一进程的请求共享队列、并发上限、请求间隔和 429 冷却时间，减少多个代理同时重试造成的压力。

代理无法增加上游额度，也无法保证第三方服务不再返回 429。它负责调度可恢复的请求，并保留失败响应和诊断信息。

## 快速开始

需要 Go 1.25 或更新版本。以下命令在仓库目录的 PowerShell 中运行：

```powershell
go test ./...
go build -trimpath -o .\bin\local-ai-relay.exe ./cmd/local-ai-relay
.\bin\local-ai-relay.exe -tui
```

在交互终端中不带参数运行也会打开 TUI。配置保存到 `config.json`，可以选择“保存并启动”，直接进入实时日志界面。

## 终端配置界面

菜单提供上游、本地设置、队列、重试、超时、流模式以及日志查看。方向键选择，`Enter` 确认，输入框使用 `Tab` / `Shift+Tab` 切换。编辑时 `Ctrl+C` 放弃当前分类的修改并返回菜单；主菜单中 `Ctrl+C` 退出且不保存。

- **上游与 API Key**：选择 Nexavlinks、OpenAI 或自定义 API 根地址。认证可选择沿用客户端认证、直接输入 API Key，或引用环境变量。
- **本地设置**：设置监听地址、日志文件以及可选的本地 API Key。本地认证启用后，必须同时配置独立的上游密钥。
- **参数编辑**：数值、时长和地址有即时校验；保存前再次检查整个配置。
- **保存**：采用同目录临时文件替换，旧文件保留为 `.bak`。取消编辑不会写入文件或凭据库，保存时检测到外部更改会报错。

直接输入的 API Key 使用掩码显示，并保存到系统凭据库：Windows Credential Manager、macOS Keychain 或 Linux Secret Service。配置文件仅保存凭据引用。修改已有密钥时留空表示保留；选择其他认证方式可取消使用该密钥。系统凭据库不可用时可以使用环境变量方式，程序不会降级为明文保存。

旧凭据会保留，以便 `.bak` 中的旧引用仍可用。包含凭据引用的配置只能在能访问相应凭据库的用户环境中使用，跨机器迁移时需要重新配置密钥。

“查看日志”会打开配置的日志文件；选择“保存并启动”后显示当前进程的实时日志。日志视图的操作：

| 按键 | 操作 |
| --- | --- |
| `↑` / `↓`、`PgUp` / `PgDn` | 滚动日志，向上滚动自动暂停跟随 |
| `f` | 暂停 / 恢复自动跟随 |
| `End` | 跳到最新日志并恢复跟随 |
| `e` | 切换全部级别 / WARN 与 ERROR |
| `/`、`Enter` | 输入搜索内容并应用；空搜索取消过滤 |
| `r` | 切换紧凑视图 / 原始 JSON |
| `q` / `Esc` | 关闭日志视图；运行视图中会同时停止当前代理 |

查看器每次读取最多末尾 1 MiB、显示最多 500 条记录，不会将整个日志文件载入内存。相对日志路径以配置文件所在目录为基准，命令行 `-log-file` 参数优先。

保留非交互方式，适用于脚本或后台启动：

```powershell
.\bin\local-ai-relay.exe -config .\examples\nexavlinks.json -check
.\bin\local-ai-relay.exe -config .\examples\nexavlinks.json -log-file .\.runtime\relay.log
```

这个示例指向 `https://www.nexavlinks.com`，监听 `127.0.0.1:8787`。通用配置 `config.example.json` 指向官方 API 的 `/v1` 根路径。两种配置均不含密钥。编辑特定配置使用 `-tui -config 路径`。

默认将客户端的 `Authorization` 转交给配置好的上游。不要把上游 URL 改成不受信任的服务。

启动后可以在另一个终端检查：

```powershell
Invoke-RestMethod http://127.0.0.1:8787/healthz
Invoke-RestMethod http://127.0.0.1:8787/status
```

`/healthz` 只表示本地进程可用，不检查上游或消耗 API 额度。`/status` 显示当前活动请求、排队数、冷却时间、累计尝试次数及最近一次错误。停止进程使用 `Ctrl+C`；服务会取消活动的上游请求并结束排队。

Linux/macOS 构建和运行：

```sh
go build -trimpath -o ./bin/local-ai-relay ./cmd/local-ai-relay
./bin/local-ai-relay -config ./config.example.json
```

## 接入 Codex

先启动代理，然后参照 [examples/codex-provider.toml](examples/codex-provider.toml) 修改现有 provider 的对应字段。该文件是需要合并的片段，不应直接覆盖整个 Codex 配置。

```toml
[model_providers.blackpixel]
name = "blackpixel"
base_url = "http://127.0.0.1:8787"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 900000
capabilities = { remote_compaction = "unsupported" }
```

保留原来的模型选择、认证方式以及其他适用设置。完全退出 Codex（包括托盘中的进程）后重新启动，让配置生效。所有希望共享调度的主代理、子代理和其他客户端都必须连接这个代理进程；绕过代理的请求不在其统计和限制内。

由代理负责 HTTP 重试，因此示例关闭 Codex 自身的请求重试和断流重试。代理已开始向客户端传输响应后不会重新发送请求；客户端重新发起请求属于独立行为，代理不做跨请求去重。

`stream_idle_timeout_ms` 是客户端设置，用于给排队、等待上游响应头和退避留出时间。它不同于本代理的 `stream_idle_timeout`。如果将代理等待时间调得更长，也应相应检查客户端等待设置。

字段以 [OpenAI Docs 的 Codex 配置参考](https://developers.openai.com/codex/config-reference)和[配置 schema](https://developers.openai.com/codex/config-schema.json)为依据。Responses 流式协议见 [OpenAI Docs](https://developers.openai.com/api/docs/guides/streaming-responses)。本项目不会自动修改用户的 Codex 配置。

## 调度与失败处理

- 先在接入总数限制内读取并检查请求，再按进入队列的顺序分配并发名额。一个名额覆盖所有上游重试和整个响应流，直到结束或取消。读取完成后，排队期间的客户端断开也能及时释放等待位置。
- `min_interval` 限制所有上游尝试的启动间隔，包括重试。它不是精确的 token 配额器；大上下文仍可能触发 token 限制。
- 429 会更新进程共享的冷却截止时间。已经发往上游的请求继续执行，尚未发送的请求等待冷却；更长的新冷却会延长已有等待。
- 遵守 `Retry-After` 的秒数和 HTTP 日期格式。该等待不会被 `max_backoff` 截短；如果超过本次重试窗口，就返回原来的错误，同时保留共享冷却。
- 请求最多尝试 `max_attempts` 次，其中包含首次尝试。退避采用带随机抖动的指数增长；通常可重试的 HTTP 状态为 408、429、500、502、503、504、529。
- `insufficient_quota`、账单限制、无效密钥等已知永久错误直接返回。无法识别的服务商错误仍受尝试次数和窗口约束，不会无限重试。
- 只有尚未向客户端提交响应的连接失败或可重试 HTTP 错误才会重试。成功响应收到后立即转发并刷新 SSE；之后遇到断流或读写失败会结束连接，不会从头重放。
- 编码及解码后的请求均受大小限制。支持普通 JSON、gzip 和 zstd；检查 `stream: true` 后发送原始请求字节，未知 JSON 字段保持原样。
- 队列满或等待超时返回本地 `503`，错误码为 `relay_queue_full` 或 `relay_queue_timeout`，便于与上游 429 区分。
- 上游最终错误的状态、响应体及普通响应头传回客户端。代理不跟随重定向，不记录对话正文或密钥。

重试窗口约束后续尝试的启动时间，不会在成功生成过程中终止一个仍在输出的长响应。等待响应头由 `response_header_timeout` 约束；响应开始后的空闲由 `stream_idle_timeout` 约束。代理不生成假 SSE 数据或心跳来掩盖上游故障。

请求体缓存在内存中，接入总数最多为 `max_concurrent + max_queue`。排队请求也持有其原始请求体，因此大上下文场景应保持较小的队列，按需调整请求大小上限。`/status` 中的 `admitted` 包含正在读取、排队或转发的请求，`queued` 只计算已经进入调度队列的请求。

流内容按字节转发，不重新生成工具调用，也不把 Responses 转换为 Chat Completions。HTTP 200 的流中仍可能包含 `response.failed` 等应用错误，客户端需要按 Responses 协议处理。当前统计中的成功表示 HTTP 响应正常传输完毕，不代表模型完成了任务。

## 配置

JSON 配置会与默认值合并，未知字段报错。时长必须是 `"1s"`、`"3m"` 这样的字符串。

| 字段 | 默认值 | 含义 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | 仅允许回环地址，端口被占用时明确报错 |
| `upstream_url` | 必填 | 上游 API 根路径，不包含 `/responses`、密钥或查询参数 |
| `max_concurrent` | `1` | 活动请求上限，包含正在输出的流 |
| `max_queue` | `32` | 额外等待并发名额的请求数；`0` 表示不排队 |
| `queue_timeout` | `3m` | 排队、读取请求及首次冷却等待的总时间上限 |
| `min_interval` | `1s` | 任意两次上游尝试的最小启动间隔；允许 `0s` |
| `max_attempts` | `5` | 单个请求的总尝试次数 |
| `retry_window` | `3m` | 首次尝试开始后，允许启动后续尝试的窗口 |
| `initial_backoff` | `2s` | 指数退避的初始上界，实际等待带随机抖动 |
| `max_backoff` | `30s` | 指数退避上界，不限制上游的 `Retry-After` |
| `response_header_timeout` | `5m` | 每次尝试等待上游响应头的上限 |
| `stream_idle_timeout` | `5m` | 转发过程中连续无字节可读或写入客户端阻塞的上限；`0s` 关闭 |
| `error_body_timeout` | `10s` | 读取错误响应前缀的上限，防止错误连接卡住 |
| `max_request_bytes` | `67108864` | 请求大小上限，同时应用于压缩前后 |
| `require_stream` | `true` | 要求 Responses 请求包含 `stream: true`，且成功响应为 SSE |
| `upstream_api_key_env` | 空 | 从指定环境变量读取上游密钥并替换客户端认证头 |
| `local_api_key_env` | 空 | 可选的本地 Bearer 密钥变量；必须同时配置独立的上游密钥 |
| `upstream_api_key_ref` | 空 | TUI 保存的上游系统凭据引用，不与对应环境变量同时使用 |
| `local_api_key_ref` | 空 | TUI 保存的本地系统凭据引用，不与对应环境变量同时使用 |
| `log_file` | 空 | 日志文件；相对配置文件目录解析，TUI 默认 `.runtime/relay.log` |

上游路径映射示例：

| `upstream_url` | 本地请求 | 上游请求 |
| --- | --- | --- |
| `https://example.com` | `/responses` 或 `/v1/responses` | `https://example.com/responses` |
| `https://example.com/v1` | `/responses` 或 `/v1/responses` | `https://example.com/v1/responses` |

另支持 `GET /models` 和 `GET /v1/models`，同样经过队列。没有实现 Chat Completions、WebSocket、远程压缩接口或多服务商自动切换。

## 可选的独立密钥

默认保留客户端认证头，适用于沿用 Codex 当前认证的场景。如果希望本地客户端和上游使用不同密钥，在个人配置中设置环境变量名：

```json
{
  "upstream_url": "https://example.com/v1",
  "upstream_api_key_env": "LOCAL_AI_RELAY_UPSTREAM_KEY",
  "local_api_key_env": "LOCAL_AI_RELAY_CLIENT_KEY"
}
```

启动代理前，通过当前终端环境注入这两个变量。该方式从环境读取值；也可以在 TUI 直接输入并保存到系统凭据库。JSON 中不要填写密钥本身。启用本地认证后，API 和 `/status` 需要本地 Bearer token，`/healthz` 仍公开；本地密钥不会发给上游。已配置但不存在的环境变量或凭据会使启动失败。

`config.json`、`config.local.json`、`.runtime/`、`bin/` 和日志文件已加入 `.gitignore`。

## 诊断与验证

日志采用 JSON Lines，包含本地请求 ID、上游请求 ID、HTTP 状态、错误 code/type、可能的限制类别、退避时间、排队时间和流量统计。错误 message 可能包含私密内容，因此只用于生成有限的 `hint`（如 `concurrency` 或 `token_rate`），不原样写入日志。hint 只是启发式线索，原始错误响应仍传给客户端。错误诊断最多检查前 64 KiB，超过该大小的错误直接完整转发，不进行自动重试。

每个 API 响应提供 `X-Relay-Request-Id`；如果上游提供 `X-Request-Id`，也会保留。调试 429 时，先看 `/status` 中的 `last_error` 和对应日志，再决定调整并发、间隔还是联系服务商。

```powershell
go vet ./...
go test -race -timeout 2m ./...
go build -trimpath -o .\bin\local-ai-relay.exe ./cmd/local-ai-relay
```

测试使用本机模拟上游，不调用真实模型，不需要 API 密钥。覆盖限流恢复、永久错误、共享冷却、并发上限、排队取消、即时流式输出、断流、压缩、大小限制、认证和日志脱敏。GitHub Actions 配置了 Windows 与 Linux 检查。

可选的真实 Codex 客户端烟雾测试（仍只访问本地模拟服务）：

```powershell
$env:LOCAL_AI_RELAY_CODEX_BIN = (Get-Command codex).Source
go test -run TestCodexClient -v -timeout 2m ./internal/relay
```

该测试要求支持隔离运行参数的 Codex CLI。它禁用用户配置和规则，使用临时目录，先模拟 429，再返回包含 `OK` 的 Responses SSE，验证客户端能正常完成。测试会分别打开和关闭请求压缩功能；是否实际压缩由客户端决定，gzip/zstd 字节保留由独立协议测试覆盖。
