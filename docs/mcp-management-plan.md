# MCP 服务管理、按次计费与公开知识库接入计划

**评审稿 · 2026-11-19**　确认本计划后开始实施。第 6 节说明如何复用 known-engine 的业务应用令牌交换，让平台用户咨询公开库并读取自己的回答。第 7 节列出验收条件。

## 1. 已确认的产品约定

| 场景 | 约定 |
| --- | --- |
| 服务管理 | 管理员可登记 stdio、SSE、Streamable HTTP 三种服务。卡片展示配置的图标。进入详情默认看“服务配置”中的 README，第二个标签“工具测试”可直接发现和调用工具。 |
| stdio 执行 | 在服务端隔离环境执行。管理员可按需下载指定版本的包，测试和正式调用使用同一份已解析的包和参数。 |
| 工具计费 | **每个成功的工具调用按该工具单价计一次，单价可为 0。** 后台实测、创建咨询、主动查询结果都适用。连接、工具发现、OAuth 和读取已发起调用的执行记录免费。 |
| 公开知识库 | 每项服务绑定一个公开库。任何有平台身份的人均可咨询，无需逐库申请权限。创建和查询共用本人的引擎连接。new-api 校验任务所在的 MCP 实例，known-engine 校验用户与任务归属。 |
| 模型费 | 工具费与引擎实际产生的模型费分别计费、分别可查。即使工具单价为 0，引擎产生的模型用量仍按原规则结算。 |
| 改动范围 | 管理、MCP 双端、公开库实例限制与工具计费在 new-api 实施。known-engine 复用已提交的 `/v1/auth/app-exchange`、咨询和任务流接口。部署时按现有机制登记受信业务应用，并在 Casdoor 配置相应客户端。 |

## 2. 工作区与目标的差距

| 核验项 | 当前行为 | 计划改动 |
| --- | --- | --- |
| 配置与连接 | 目录表单和后端只接受 `http_sse`、`streamable_http`，OAuth 只开放给后者。旧路径已有加密 Headers，但目录凭据接口拒绝该写入。默认超时 30 秒，上限 120 秒。 | 扩展正在使用的目录创建、编辑、读取和连接路径。补 stdio、目录 Headers、图标、README，以及两种 HTTP 传输的 OAuth。 |
| 详情与测试 | 管理页以表格、连接页及工具定价为主。广场卡片使用固定插头图标，测试生成模型对话命令。新建目录服务调用管理端 `test-invocations` 会返回 404，现有 OAuth 回跳先到广场。 | 复用卡片和测试面板，让新建服务能从详情直接选工具、填参、调用、看结果。OAuth 回到发起测试的同一服务。 |
| 资金 | `QuotePrice` 和定价 UI 使用 `unit_price_usd`。MCP 已有调用账本、钱包和 Token 额度事务、幂等及补偿。模型资金会话还支持订阅、付款偏好和回退。 | 对齐 USD/CNY 报价和资金来源选择，用同一付款人的模型调用与 MCP 调用做对照测试。 |
| 知识服务 | 本地服务按固定 `provider-knowledge` 查找。现有四个知识工具，调用者在创建时选择 `knowledge_base`。结果通过任务详情和 `/events?offset=0` 反复读取。 | 用同一提供端按发布实例绑定库，给新实例注册创建和查结果两个工具。结果使用指定的 `/stream`。 |
| 知识服务鉴权 | 工作区的适配器走 `/v1/auth/mcp-exchange`，专用委托创建咨询成功，读 `/stream` 返回 403。适配器写死了 `Bearer` 和 300 秒上限。已提交的引擎另有 `/v1/auth/app-exchange`。 | 新实例使用受信应用授权，按 `/app-exchange` 的请求和响应契约交换令牌。分别处理用户需要授权、应用配置有误和引擎暂不可用。 |

相关实现位于以下入口。[目录配置](../service/mcp/management.go)、[测试 API](../controller/mcp.go)、[目录与 OAuth 回调](../controller/mcp_catalog.go)、[管理页](../web/src/features/mcp/index.tsx)、[广场](../web/src/features/mcp/marketplace.tsx)、[现有知识适配器](../service/mcp/knowledge.go)。

## 3. 管理员与使用者的操作

| 表单项 | stdio | SSE / Streamable HTTP |
| --- | --- | --- |
| 展示信息 | 服务器名称、图标、README Markdown | 同左 |
| 连接信息 | 命令必填，支持粘贴带参数、引号的完整命令。预览解析后的程序及参数。环境变量可选，逐项增删或粘贴。独立的程序和参数字段传给执行器，不经 shell 展开。 | 服务器地址必填。Headers 可选，逐项增删或粘贴。OAuth 客户端登记信息按认证方式显示。 |
| 超时 | 新配置留空默认 60 秒，可填整数 1–600 秒，覆盖连接、工具枚举和调用。 | 同左，长连接也受该次操作截止时间和取消信号约束。 |

- 管理员新增或编辑草稿、测试连接、审阅发现的工具、逐工具定价并发布。允许先保存未补齐展示资料的草稿。新发布的卡片有图标和服务说明。旧记录有缺失时显示占位图标及说明空态，已设置的 30 秒等超时继续生效。连接 API、SDK、测试轮询与工作进程租约都按同一超时检查 600 秒边界。
- 图标支持配置和预览，建议支持项目图标选择及受限大小的 PNG 上传。README 支持直接编辑、导入本地 `.md` 和净化渲染。列表只取卡片摘要，正文在打开详情时加载。复用图标选择、Markdown 渲染和测试面板。[图标字段](../web/src/components/lobe-icon-field.tsx) · [插件图标文件处理](../web/src/features/task-plugins/lib/plugin-icon-file.ts) · [Markdown](../web/src/components/ui/markdown.tsx) · [测试面板](../web/src/features/mcp/test-panel.tsx)。
- 管理详情用独立页面地址，例如 `/mcp/servers/{id}?tab=tools`，可刷新或从授权页返回。现有 `/mcp/{server_id}` 继续承载协议流量。首个标签只渲染服务说明，编辑、定价、日志由管理操作进入。广场上的已发布卡片复用详情，普通使用者以自己的安装、授权和账户测试。
- 打开“工具测试”即检查连接和授权。无需 OAuth 或已有有效授权时列出当前身份可用的工具。有 OAuth 挑战时完成授权后自动返回同一标签并加载工具。拒绝、过期和缺少客户端登记信息分别给出原因和可执行的操作。显示工具描述、输入 schema、可编辑参数、预计费用、调用结果与实扣 quota。只有用户明确提交才执行 `tools/call`。
- 复用现有 `/api/mcp/servers`、发现/同步、定价及调用查询接口。修正目录服务的直接实测入口，为普通使用者提供同语义、受本人安装约束的调用入口。服务端区分读取、编辑、测试和修改连接/价格等权限，验收使用获授 `mcp:sensitive_write` 的实际管理员。浏览 README 或发现工具不触发模型请求。

## 4. 通用 Go 客户端与服务端

沿用根模块锁定的 `github.com/modelcontextprotocol/go-sdk v1.7.0`。客户端负责建立连接、枚举分页工具、调用及关闭。服务端接收已认证身份，注册工具名称、输入/输出 schema 和处理函数。目录、资金、引擎 HTTP 和 Casdoor 授权均放在相应业务层。协议版本协商与 JSON-RPC 编解码交给 SDK。[Go SDK v1.7.0](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk@v1.7.0/mcp) · [MCP 传输规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports)

| 上游 | 客户端实现与生命周期 |
| --- | --- |
| stdio | 隔离执行器内由 `CommandTransport` 启动进程，或以 `IOTransport` 对接执行器提供的标准流。用程序和参数数组启动。安装阶段从受信包源按显式版本下载、记录解析版本与摘要，热缓存和冷启动使用同一版本。执行器使用独立低权限身份、受限目录与网络。只注入该服务声明的环境变量，不继承网关的数据库和渠道密钥。限制资源及并发。取消、超时、启动失败时回收进程树和管道。 |
| SSE | 使用 `SSEClientTransport` 兼容旧 HTTP+SSE 服务，分别验证事件连接与消息提交地址，只向登记的目标发送相应 Headers。 |
| Streamable HTTP | 使用 `StreamableClientTransport`，支持 JSON 和事件流响应。复用现有出站地址校验、超时、取消和响应大小限制。 |

每次发现或执行在相应用户、服务、授权版本下运行。分页处理结束、空目录、游标循环和断线都有显式结果。执行前取已审阅目录与当前身份上游工具的交集，并校验最新 schema 和调用参数。提供端沿用 `NewServer`、工具注册和 `NewStreamableHTTPHandler`。新知识服务通过 Streamable HTTP 暴露。用具有不同工具集合的两个测试服务验证注册、发现和调用。[客户端现状](../service/mcp/adapters.go) · [服务端现状](../service/mcp/protocol.go)

**凭据与 OAuth。** stdio 从隔离环境传入凭据。远程服务可无认证、使用静态 Headers，或使用上游声明的 OAuth 授权服务器。OAuth 下由平台写入用户 Bearer，拒绝相冲突的自定义 Authorization。Headers、环境变量值、客户端 secret 及用户授权加密保存。命令参数与连接细节仅对获授权的管理者可见，卡片和公开详情使用独立展示字段。更新明确区分保留、替换、删除。读接口只返回配置状态。展示文字使用独立的元数据版本。变更连接目标、可执行包、凭据或授权配置需使旧连接失效并重新发现。

从 HTTP 挑战或受保护资源元数据发现授权服务器，支持 OAuth/OIDC 发现、预注册客户端、授权码及 PKCE S256。短时、一次性 state 绑定发起用户、浏览器会话、服务、用途、issuer、resource、连接版本和站内返回目标。管理员的发现授权与使用者执行授权独立。刷新保持 resource 约束。拒绝授权时留在同一工具页，到期或撤权时要求重新授权。回调、审计和 stderr 只记录排查所需的非秘密信息。[MCP 授权规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization) · [现有 OAuth 流程](../service/mcp/catalog_oauth.go)

## 5. 以工具调用为单位结算

1. **报价。** 管理员按站点当前 USD 或 CNY 填每次价格，0 表示免费。建议保存十进制原价、原币种及价格版本。旧 `unit_price_usd` 按 USD 读取。站点切换币种时换算显示。原币种价格在改价前有效，新调用按受理时系统汇率换算，历史账单使用原快照。站点处于已有的 TOKENS/CUSTOM 模式时沿用其展示规则，MCP 定价输入须明确选择 USD 或 CNY。
2. **换算。** 使用系统 `USDExchangeRate`（1 USD 对应的 CNY）和 `common.QuotaPerUnit`。USD 的原价直接乘每美元 quota。CNY 的原价先除以汇率，再乘每美元 quota。十进制运算在最后一次舍入，使用 `common.QuotaFromDecimalChecked`。拒绝无效汇率、负价、超限或小于 1 quota 的正价。运行时的 clamp 走现有请求关联告警和管理员审计。举例，汇率 7.20、每美元 500,000 quota 时，¥0.072 和 $0.01 的单次报价都是 5,000 quota。受理时冻结价格、汇率、quota 换算系数、应扣 quota 和付款来源。
3. **扣款。** 后台测试、平台 API Key 及知识服务直连进入同一执行与计费路径。沿用模型计费对个人/团队付款人、钱包/订阅偏好、允许的回退、Token 限额及不足额的判断。以 `BillingSession`、`FundingSource` 的现有行为为准，复用资金操作并持久化恢复所需的标识。同一调用只允许一个入口实际扣款。免费调用仍检查平台身份、记录次数。
4. **结算。** 有价调用先预扣，余额不足在执行前返回。工具成功结算一次，有效的空列表也算成功。现有搜索/抓取的非空计费判断和咨询工具描述中的固定免费说明应随此规则修订。协议错误、`isError` 或确定失败释放预扣。创建咨询以引擎确认受理为成功。读取到有效的运行中或终态快照算一次成功查询。每个 SSE 事件、同一请求的网络重连、查询既有调用记录均不另计次数。在工具测试页面，价格上涨或工具版本变更会触发重新报价，执行前由测试者确认。
5. **幂等与对账。** 工具页为一次提交生成稳定的幂等键。在同一用户、服务和工具内，重放同键同参数返回原调用，同键异参报冲突。外部客户端未提供稳定键的独立调用按新调用处理。已发往上游但结果未知时持久化待核对状态，禁止盲目再次执行。已确认成功补结算，明确失败或截止仍无法确认则释放预扣并保留审计。请求次数、消费日志、钱包/订阅、Token 额度和工具账本通过同一调用 ID 核对。引擎的模型费按模型请求单列。

钱包、订阅和 Token 的预扣、消费与退款都要核对到实际资金来源。[模型资金会话](../service/billing_session.go) · [资金来源](../service/funding_source.go) · [现有 MCP 资金操作](../model/mcp_billing.go) · [quota 换算](../common/quota_math.go)

## 6. 按公开知识库发布的咨询服务

**服务绑定。** 管理员在 new-api 为每个公开库登记稳定的服务 ID、`owner`、`kb_id`、展示资料和各工具价格。例如同一套代码可提供 `/mcp/providers/knowledge/{server_id}` 的两个实例，各有自己的地址、工具开关、价格和 OAuth resource。known-engine 以 owner 与 kb_id 定位知识库，允许不同 owner 使用同一 kb_id。发布时用准确的库详情验证 `public_read`，调用及重读缓存结果时校验当前公开状态和本人授权。库转私有或被删除时停止通过该实例提供公开服务。改绑知识库创建新实例，旧任务继续归属于原实例。

| 发布的工具 | 输入与引擎契约 | 对外结果 |
| --- | --- | --- |
| `create_consultation` | 输入 `question`、稳定 `request_id` 及可选 `session_id`。服务端从实例取库，调用 `POST /v1/knowledge/kb/{kb_id}/chat?owner=…`。按调用人、服务实例和 `request_id` 生成稳定的 `Idempotency-Key`，续聊先核对会话绑定。 | 返回引擎确认的 `task_id`、`session_id`，持久化服务、用户、owner、库、会话与任务的关联。相同请求重试只创建一个任务。 |
| `get_consultation_result` | 输入 `task_id`，仅接受本人通过此实例创建的任务。调用 `GET /v1/tasks/{task_id}/stream`，按 SSE 帧读取回放和后续事件。 | 将正文增量、可公开引用和任务终态汇集为有界结果快照。返回任务 ID、状态、答案、是否结束。可选的 MCP progress 通知只发送进度，公开结果按字段白名单投影。 |

引擎首次发 `connected`、`context`，然后可能发 `text_delta`、`completed`、`error` 等事件。重连会从头回放，`event_index` 仅对本次连接有效。新实例首版用截至当前的快照，每次重读以新快照替换旧值。限定等待窗口内收到有效状态但尚未结束可返回运行中。断流且状态未知、超出大小上限、授权到期且无法续期或权限撤回均需明确上报。关闭 MCP 请求会释放引擎流连接，已创建的引擎任务依其自身生命周期运行。新工具 schema 拒绝调用者覆写库、owner 或身份。

### 复用 known-engine 的 `POST /v1/auth/app-exchange`

将 new-api 的知识库连接登记为 known-engine 的一个受信业务应用。发布更多公开库时，只需在 new-api 登记各自的服务实例。该应用代表实际调用人访问引擎，创建和查询共用本人的引擎连接。[known-engine 现有业务应用接入约定](../../known-engine/docs/architecture/auth/13-unified-upgrade.md)

1. **确认调用人。** 工具测试页从当前平台会话取得用户，API Key 入口校验其所有者，外部 MCP 客户端使用面向该实例资源 URL 的访问令牌。new-api 校验当前实例的调用资格，用已验证的 Casdoor 发行方和用户标识 `(iss, sub)` 将当前账号与引擎连接匹配。[MCP 身份校验](../service/mcp/auth.go)
2. **取得受信应用的用户授权。** `/app-exchange` 要求输入的 Casdoor access token 以登记应用的 `client_id` 为受众。优先复用同一用户已授予该应用且仍有效的授权。首次缺少授权时，new-api 以登记应用身份发起授权码流程，绑定 state、PKCE S256 和 OIDC nonce，按配置申请业务登录权限（例如 `system.login`）。回调核对签名、发行方、受众及同一 `sub`，按用途加密保存授权。项目的账号绑定只保存身份关系，不能据此推定已有可交换的令牌。Casdoor 可复用浏览器登录态，首次是否出现确认页取决于该应用的授权记录。工具页回到原实例，外部客户端可通过站内连接入口补齐授权。调用者拥有有效的引擎连接后，创建和查询无需分别授权。[new-api 的 SSO 回调](../controller/oauth.go) · [账号绑定](../model/user_oauth_binding.go) · [Casdoor 的令牌受众](../../casdoor/object/token_jwt.go)
3. **交换并使用引擎令牌。** new-api 在后端通过 HTTPS 请求 `POST /v1/auth/app-exchange`，用 HTTP Basic 提交应用 ID 和引擎为该应用登记的交换密钥，JSON 请求体为 `{"access_token":"<应用受众的用户令牌>"}`。引擎校验应用、签名、受众、组织、配置的 scope 和令牌当前状态，映射到本人引擎账号，沿用已有的首次开户及个人模型计费身份配置。返回的 `access_token` 作为引擎 Bearer，只在服务端使用。适配器按不区分大小写的方式校验 `token_type` 为 `Bearer`，按引擎返回的有效期使用令牌（最长一小时，亦受用户授权到期时间限制）。调用期间按本人授权有效性刷新或重新交换，网络重连复用原调用记录。[交换入口](../../known-engine/services/routers/auth.py) · [身份及应用能力校验](../../known-engine/services/auth/app_delegation.py) · [引擎会话](../../known-engine/services/auth/casdoor_provider.py)

**业务权限与本地实测。** 从 known-engine 已提交版本 `53a2edc8` 的源码运行真实 FastAPI 路由。使用临时 SQLite、独立 Redis 和测试签发方。测试账号是公开库的非成员，引擎任务在夹具中置为已完成，未调用付费模型。

| 业务请求 | 引擎给受信应用配置的能力 | 观测结果 |
| --- | --- | --- |
| `GET /v1/knowledge/kb/{kb_id}?owner=…` | `kb:read`，供平台核实绑定库的公开状态 | 200，`public_read=true`、`can_edit=false`。 |
| `POST /v1/knowledge/kb/{kb_id}/chat?owner=…` | `kb:write`，现有权限映射对该 POST 的要求 | 200，创建本人咨询任务。 |
| `GET /v1/tasks/{task_id}/stream` | `apps:run`，读取本人的任务流 | 200，收到 `connected`、`context`、`completed`。 |

同一测试中，另一名平台用户尝试读取该任务返回 404。向 `/app-exchange` 提交以 MCP 资源 URL 为受众的用户令牌返回 401。部署为连接器登记上述三项应用能力即可，用户的公开库访问和个人任务归属仍由引擎校验。[知识库权限](../../known-engine/services/auth/knowledge_access.py) · [引擎任务流](../../known-engine/services/gateway.py) · [受信应用测试夹具](../../known-engine/tests/L4_gateway/test_casdoor_api.py)

**每个 MCP 实例的限制由 new-api 执行。** `kb:write` 和 `apps:run` 在引擎内还涵盖其他操作。连接器只允许上表三个固定业务请求，从服务端绑定取库和 owner，从经过本人授权的任务绑定取 task_id。创建成功后保存调用人、服务实例、库、会话及任务的关系再交付任务 ID。恢复在途创建沿用同一幂等键。续聊、读流、重连或返回缓存结果前都核对同一用户、实例和库。读流期间在 new-api 有界复查用户授权、实例是否上架及库的公开状态，返还快照前再次核验。失效时关闭上游流并停止提供结果。引擎按自己的应用会话与任务归属处理请求。[现有任务绑定](../service/mcp/knowledge.go) · [调用账本和任务模型](../model/mcp.go)

## 7. 按链路驱动实施与验收

每一阶段先在真实入口写能暴露差距的失败用例，再写最小改动并验证。优先扩展现有测试，记录输入、可观察结果和实际运行命令。知识适配器的首条失败用例从 new-api 发起，只提供已提交的 `/app-exchange` 和现有业务路由，要求调用者完成连接后创建公开库咨询并通过 `/stream` 读到终态。

| 阶段 | 首先写的验收用例 |
| --- | --- |
| 1. 元数据与详情 | 具备权限的管理员创建并重新打开三种配置，验证字段互斥、超时空值/1/600/越界、凭据保留与清除、图标和 Markdown 净化。卡片进入默认 README、刷新和键盘可切换工具标签。普通用户的编辑请求被拒绝。 |
| 2. Go SDK 双端 | 测试进程和本地 HTTP 服务分别完成发现、分页、实调、关闭。stdio 从测试包源获取固定版本，验证带空格参数、环境隔离、下载失败、超时和子进程清理。两个 HTTP 传输分别验证 Headers、不同用户、同源约束、断线与取消。用独立 SDK 客户端访问通用提供端并核对 schema 和结果。 |
| 3. 工具实测与 OAuth | 从新建的目录服务直接发现并执行。无 OAuth 时直接列工具，有 OAuth 时经授权码、PKCE 和回调返回原服务。覆盖拒绝、过期、state 重放、切换用户、错误 issuer/resource、刷新失效、撤权、连接版本变更和跨站修改。打开标签只列工具。 |
| 4. 资金一致性 | USD/CNY 等值、0 价、无效/极小/超大价格、汇率或价格在途变化。逐项断言个人、团队、订阅偏好/回退、Token 限额与模型资金行为一致。后台和协议入口各自实调，核对预算不足时上游调用为零、成功一笔（含有效空结果）、失败退款、同键并发、崩溃恢复与模型并发争用同一余额。 |
| 5. 公开知识库 | 覆盖已有应用授权、首次连接和回跳、错受众、两份授权对应不同用户、令牌到期重换、权限不足及引擎暂不可用。非成员可提问并读取本人结果。用同名异主库及同库两个实例，验证续聊、重放、跨用户和跨实例的任务绑定。在 new-api 拒绝覆写上游路径、库或任务绑定的请求，并断言未发出对应业务请求。下架、撤权、库转私有时，停止读流、重连和缓存结果交付。核对 SSE 分帧/多行/回放/终态、重复创建、网络重连，确认每次成功工具调用只收一次费。模型用量单独核账。 |
| 6. 发布和回退 | 开关关闭及开启均回归模型列表、常规模型中转、订阅、团队钱包、API Token、旧 MCP 安装/调用。分别验证普通引擎 SSO、其他已登记业务应用，以及已部署的旧知识服务仍能按各自契约运行。逐项审视对通用钱包、缓存和任务结算的改动。关闭 MCP 停止接新调用，继续对账至所有预扣有终态。 |

前端覆盖七个 locale，运行受影响 Vitest、`bun run typecheck`、受影响文件 lint 和 `bun run build`。最终用真实 SDK 客户端、私有 Casdoor 和 known-engine 测试部署，分别以首次连接及已有应用授权的非成员账号验证真实回调，完成一次创建和一次结果读取，用该用户的账单核对两笔工具调用和实际模型用量。OAuth 回归对应 ASVS 5.0.0 的 V8.2.2、V10.1.1–2、V10.2.1–2、V10.3.1–3，分别核验对象权限、凭据边界、回调绑定、受众和身份。交付时记录实测与剩余缺口。[ASVS 5.0.0](https://asvs.owasp.org/) · [Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html) · [Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html) · [OAuth2](https://cheatsheetseries.owasp.org/cheatsheets/OAuth2_Cheat_Sheet.html) · [CSRF](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)

**迁移与兼容。** 使用增量字段，旧价格标识 USD，保留服务 ID、安装、已设超时、凭据、历史调用与未结资金。保留旧知识提供端的原有契约和资源地址。新实例使用带稳定服务 ID 的地址。前端广场、DeepSeek 和模型对话测试属于工作区已有能力，作为兼容回归覆盖。共享额度表、缓存与现有模型付款路径的改动独立审查、按兼容顺序部署。MCP 关闭时也要验证这些公共路径。

部署沿用默认关闭的 `MCP_ENABLED`、稳定的 `MCP_ENCRYPTION_KEY` 和 `MCP_PUBLIC_BASE_URL`。预注册回调为 `{MCP_PUBLIC_BASE_URL}/api/mcp/oauth/callback`。前后端分离时使用 `MCP_DASHBOARD_BASE_URL` 与 `MCP_ALLOWED_CLIENT_ORIGINS`，私有引擎和 HTTP 开发端点按完整 origin 加入 `MCP_ALLOWED_UPSTREAM_ORIGINS`。在已启用 Casdoor 的引擎中通过 `CASDOOR_TRUSTED_APPS_FILE` 登记一个受信业务应用，配置应用 ID、组织、证书、内省凭据、交换密钥摘要、所需 scope 和表中三项业务能力。与 Casdoor 核对该应用的准确回调地址，在 new-api 配置对应交换凭据。由 new-api 发布首个公开库实例做端到端验收，后续新增知识库复用此应用登记。灰度时配置 stdio 隔离执行器和包源，并确认共享钱包的所有实例均使用兼容版本。

数据库验收使用真实 SQLite、MySQL、PostgreSQL，在空库、从届时最新发布版构建的代表库、含本工作区旧 MCP 数据的库上分别启动迁移两次。核对余额、索引与唯一性、旧价格和在途退款。主库用项目兼容的 GORM 锁及 TEXT 编码。日志变动另测独立日志库，含适用的 ClickHouse 路径。记录精确数据库版本、命令与结果，涉及版本特性时覆盖最低支持版本。回退使用兼容程序和资金恢复任务，保留原始数据供对账。

## 8. 本轮基线与历史记录

| 本轮复核 | 结果 |
| --- | --- |
| new-api 临时 SQLite，`go test ./service/mcp ./router ./model -run '^TestMCP' -count=1` | 三个包通过，覆盖现有实现。 |
| `web/` 下 `bun run test src/features/mcp/__tests__/mcp.test.tsx` | 10 项通过。 |
| `go test ./router -run '^(TestListModelsSupportsOpenAIAndGeminiAuthentication\|TestGetOpenAIVideoRouteRendersJimengTask)$' -count=1` | 两项均复现 403，测试夹具的 `billing_units` 查询失败。实施前后单列对照。 |
| Casdoor 临时 SQLite，`go test ./object -run '^TestOAuthExistingFlowContracts$/(ordinary-jwt-preserves-identity\|refresh-scope-and-replay)$' -count=1` | 通过。验证用户标识、组织、指定资源受众和刷新令牌约束。 |
| new-api 临时 SQLite，`go test ./service/mcp -run '^(TestMCPOAuthCryptographicAndLiveAuthorization\|TestMCPKnowledgeHTTPReuse)$' -count=1` | 通过。覆盖现有用户授权校验与知识适配器的 HTTP 调用，适配器目前仍用旧结果接口。 |
| known-engine 已提交版本 `53a2edc8` 的公开库 HTTP 契约 | 正确的应用令牌交换 200，非成员创建咨询和读取本人任务流均为 200，另一用户读该任务 404，受众错误的令牌交换 401。第 6 节记录夹具和请求。私有 Casdoor、new-api 新适配器及真实模型账单的端到端验证留在实施阶段。 |

旧稿曾记录 SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 15.19 和从 `v1.0.0-rc.51` 升级的演练，以及开发库 `new-api-dev-pg/new-api` 的一次手工清理。它们属于旧实现的历史。此前记录位于 `/tmp/mcp-upgrade-validation`、`/tmp/mcp-engine-upgrade-validation`、`/tmp/mcp-reuse-newapi-*`、`/tmp/mcp-catalog-go-test-final.log` 和 `/tmp/mcp-baseline-router.log`。本计划以可重新运行的用例验收新契约。
