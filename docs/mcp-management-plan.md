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

从 HTTP 挑战或受保护资源元数据发现授权服务器，支持 OAuth/OIDC 发现、动态注册或预注册客户端、授权码及 PKCE S256。内置 DeepSeek 的首次授权自动使用发现文档中的 `registration_endpoint` 在 `MCP_CASDOOR_ORGANIZATION` 注册，客户端 ID 与加密 secret 保存在服务记录中，后续授权和进程重启复用；不再读取 `MCP_OAUTH_CLIENT_ID/SECRET`。Casdoor 默认应用须允许单点登录，动态应用继承其用户标签限制、证书和会话配置。公共知识库的受信业务应用登记仍使用独立配置。短时、一次性 state 绑定发起用户、浏览器会话、服务、用途、issuer、resource、连接版本和站内返回目标。管理员的发现授权与使用者执行授权独立。刷新保持 resource 约束。拒绝授权时留在同一工具页，到期或撤权时要求重新授权。回调、审计和 stderr 只记录排查所需的非秘密信息。[MCP 授权规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization) · [现有 OAuth 流程](../service/mcp/catalog_oauth.go)

## 5. 以工具调用为单位结算

1. **报价。** 管理员按站点当前 USD 或 CNY 填每次价格，0 表示免费。建议保存十进制原价、原币种及价格版本。旧 `unit_price_usd` 按 USD 读取。站点切换币种时换算显示。原币种价格在改价前有效，新调用按受理时系统汇率换算，历史账单使用原快照。站点处于已有的 TOKENS/CUSTOM 模式时沿用其展示规则，MCP 定价输入须明确选择 USD 或 CNY。
2. **换算。** 使用系统 `USDExchangeRate`（1 USD 对应的 CNY）和 `common.QuotaPerUnit`。USD 的原价直接乘每美元 quota。CNY 的原价先除以汇率，再乘每美元 quota。十进制运算在最后一次舍入，使用 `common.QuotaFromDecimalChecked`。拒绝无效汇率、负价、超限或小于 1 quota 的正价。运行时的 clamp 走现有请求关联告警和管理员审计。举例，汇率 7.20、每美元 500,000 quota 时，¥0.072 和 $0.01 的单次报价都是 5,000 quota。受理时冻结价格、汇率、quota 换算系数、应扣 quota 和付款来源。
3. **扣款。** 后台测试、平台 API Key 及知识服务直连进入同一执行与计费路径。沿用模型计费对个人/团队付款人、钱包/订阅偏好、允许的回退、Token 限额及不足额的判断。以 `BillingSession`、`FundingSource` 的现有行为为准，复用资金操作并持久化恢复所需的标识。同一调用只允许一个入口实际扣款。免费调用仍检查平台身份、记录次数。
4. **结算。** 有价调用先预扣，余额不足在执行前返回。工具成功结算一次，有效的空列表也算成功。现有搜索/抓取的非空计费判断和咨询工具描述中的固定免费说明应随此规则修订。协议错误、`isError` 或确定失败释放预扣。创建咨询以引擎确认受理为成功。读取到有效的运行中或终态快照算一次成功查询。每个 SSE 事件、同一请求的网络重连、查询既有调用记录均不另计次数。在工具测试页面，价格上涨或工具版本变更会触发重新报价，执行前由测试者确认。
5. **幂等与对账。** 工具页为一次提交生成稳定的幂等键。在同一用户、服务和工具内，重放同键同参数返回原调用，同键异参报冲突。外部客户端未提供稳定键的独立调用按新调用处理。已发往上游但结果未知时持久化待核对状态，禁止盲目再次执行。已确认成功补结算，明确失败或截止仍无法确认则释放预扣并保留审计。请求次数、消费日志、钱包/订阅、Token 额度和工具账本通过同一调用 ID 核对。引擎的模型费按模型请求单列。

钱包、订阅和 Token 的预扣、消费与退款都要核对到实际资金来源。[模型资金会话](../service/billing_session.go) · [资金来源](../service/funding_source.go) · [现有 MCP 资金操作](../model/mcp_billing.go) · [quota 换算](../common/quota_math.go)

## 6. 按公开知识库发布的咨询服务

**服务绑定。** 管理员在 new-api 的标准 MCP 表单选择 Streamable HTTP，填写完整的 `{MCP_PUBLIC_BASE_URL}/mcp/providers/knowledge/{owner}/{kb_id}` URL、展示资料和各工具价格。后端从规范 URL 提取并持久化固定的 `owner`、`kb_id`，无需表单额外填写，也不接受通过工具参数更换。每个 URL 只能登记一次；不同知识库各有自己的服务记录、地址、工具开关、价格和 OAuth resource。未注册、未启用或未填全 owner/kb_id 的地址不提供 MCP 服务。旧 `/mcp/providers/knowledge/{server_id}` 地址已删除，不再兼容。known-engine 以 owner 与 kb_id 定位知识库，允许不同 owner 使用同一 kb_id。发布时用准确的库详情验证 `public_read`，调用及重读缓存结果时校验当前公开状态和本人授权。库转私有或被删除时停止通过该实例提供公开服务。改绑知识库创建新实例，旧任务继续归属于原实例。

| 发布的工具 | 输入与引擎契约 | 对外结果 |
| --- | --- | --- |
| `create_consultation` | 输入 `question`、稳定 `request_id` 及可选 `session_id`。服务端从实例取库，调用 `POST /v1/knowledge/kb/{kb_id}/chat?owner=…`。按调用人、服务实例和 `request_id` 生成稳定的 `Idempotency-Key`，续聊先核对会话绑定。 | 返回引擎确认的 `task_id`、`session_id`，持久化服务、用户、owner、库、会话与任务的关联。相同请求重试只创建一个任务。 |
| `get_consultation_result` | 输入 `task_id`，仅接受本人通过此实例创建的任务。调用 `GET /v1/tasks/{task_id}/stream`，按 SSE 帧读取回放和后续事件。 | 将正文增量、可公开引用和任务终态汇集为有界结果快照。返回任务 ID、状态、答案、是否结束。可选的 MCP progress 通知只发送进度，公开结果按字段白名单投影。 |

引擎首次发 `connected`、`context`，然后可能发 `text_delta`、`completed`、`error` 等事件。重连会从头回放，`event_index` 仅对本次连接有效。新实例首版用截至当前的快照，每次重读以新快照替换旧值。限定等待窗口内收到有效状态但尚未结束可返回运行中。断流且状态未知、超出大小上限、授权到期且无法续期或权限撤回均需明确上报。关闭 MCP 请求会释放引擎流连接，已创建的引擎任务依其自身生命周期运行。新工具 schema 拒绝调用者覆写库、owner 或身份。

### 复用 known-engine 的 `POST /v1/auth/app-exchange`

将 new-api 的知识库连接登记为 known-engine 的一个受信业务应用。发布更多公开库时，只需在 new-api 登记包含具体 owner 和 kb_id 的完整 MCP URL，无需为每个库新增路由或修改代码。该应用代表实际调用人访问引擎，创建和查询共用本人的引擎连接。[known-engine 现有业务应用接入约定](../../known-engine/docs/architecture/auth/13-unified-upgrade.md)

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

## 9. known-engine 用户委托网关（2026-11-20）

本节更新前文的平台 API Key 接入约定。对外 `/mcp/gateway`、`/mcp/{server_id}` 和 `/api/mcp/self/config` 只接受 app-exchange 签发的 MCP 委托令牌，不接受模型 API Key、后台 PAT、后台登录 JWT 或 known-engine JWT。后台连接测试、工具测试、用户安装及既有 Casdoor／上游 MCP 授权继续使用原来的入口和存储；对话测试 `/v1/mcp/test/chat/completions` 仍使用模型 API Key，模型与工具计费行为不变。

### 配置及用户授权

在现有 `MCP_ENABLED`、`MCP_PUBLIC_BASE_URL`、`MCP_ENCRYPTION_KEY`、`MCP_CASDOOR_ISSUER`、`MCP_CASDOOR_ORGANIZATION`、`MCP_CASDOOR_PROVIDER_ID` 之外，设置 `MCP_TRUSTED_APPS_FILE` 指向服务账号可读的 JSON 文件（建议权限 `0600`）。多实例共用稳定的 MCP 加密密钥与公开地址。不需要新表或迁移，也不会创建、替换用户的模型 API Key。

文件的键必须是 known-engine 在 Casdoor 中实际登记的 client ID。示例中的占位值必须替换：

```json
{
  "<known-engine-casdoor-client-id>": {
    "enabled": true,
    "exchange_secret_sha256": "<独立随机交换密钥的64位十六进制SHA256摘要>",
    "casdoor_client_secret": "<该应用在Casdoor中的客户端密钥>",
    "server_ids": ["<允许使用的MCP服务ID>"],
    "max_charge_quota": 10000
  }
}
```

交换密钥应使用至少 32 字节的安全随机数，由 known-engine 后端持有原值，new-api 文件中仅保存摘要。它与 Casdoor 客户端密钥是两个不同的秘密。`server_ids` 为应用可访问的服务上限，可显式使用 `["*"]` 允许所有用户已启用的服务；登记本身不会替用户安装、启用或授权服务。`max_charge_quota` 是单次工具调用的最大内部 quota，必须为 `1..2147483647`，不是美元数或全天预算；总消费仍受用户现有资金及订阅规则约束。

在 Casdoor 为 known-engine 应用增加独立的 `mcp:gateway` scope，并配置清晰的委托／付费调用说明及用户同意流程。known-engine 的授权码流程应申请此 scope，并保留已有 PKCE、state、nonce 验证。只有 `system.login` 的既有登录令牌不足以交换；这是对下游应用的单独委托，不要求重新取得 new-api 已保存的上游 MCP 授权。用户必须已有与所配置 Casdoor provider 绑定的 new-api 账号，匹配依据为发行方和 `sub`，不按邮箱、用户名自动关联或开户。

### 接口契约

known-engine 后端调用 **`POST /v1/auth/app-exchange`**，通过 HTTPS 使用 HTTP Basic 提交上述 Casdoor client ID 和独立交换密钥，JSON 请求体为：

```json
{"access_token":"<aud为known-engine应用client ID、带mcp:gateway的Casdoor用户Access Token>"}
```

接口验证应用凭证、RS256 签名、发行方、唯一受众、组织、授权方、有效期、scope 及 Casdoor 实时 introspection 状态，再映射至本地有效用户。响应为未包裹的 JSON：

```json
{
  "access_token": "<MCP专用JWT>",
  "token_type": "Bearer",
  "expires_in": 300,
  "expires_at": 1800000300,
  "scope": "mcp:tools:read mcp:tools:call",
  "gateway_url": "https://<new-api>/mcp/gateway"
}
```

有效期取 5 分钟和 Casdoor 用户令牌剩余寿命的较小值。`expires_at` 是 Unix 秒级时间戳，与 JWT 的 `exp` 一致；以上数值仅用于展示响应格式。JWT 使用 HS256，签名密钥从 `MCP_ENCRYPTION_KEY` 按独立用途派生，不与后台登录 JWT 共用。头部 `typ=at+jwt`，公开声明包括：

- `iss`：`MCP_PUBLIC_BASE_URL`；`aud`：`MCP_PUBLIC_BASE_URL + /mcp/gateway`。
- `sub`：new-api 本地用户 ID 字符串；`azp`：发起交换的受信应用 client ID。
- `token_use=mcp_delegation`、`scope=mcp:tools:read mcp:tools:call`，以及 `iat`、`nbf`、`exp`、随机 `jti`。
- `grant`：现有 AEAD 加密封装的授权状态，包含服务端实时检查所需的 Casdoor 授权、身份及应用配置版本；客户端不解密、不修改。JWT 可读载荷不包含明文 Casdoor Token、交换密钥或上游 MCP 凭据。

签名通过后，服务端还会解密并核对授权状态与 JWT 的用户、应用和时间声明，继续检查当前 Casdoor 状态和应用登记；JWT 声明不能扩展服务权限或预算。没有新增会话表或 Redis 依赖，也不提供 refresh token。旧的 `mcpd_` 不透明令牌不再接受，调用方需重新交换。

known-engine 等调用应用可以本地解码 `exp`，或使用响应的 `expires_at`／`expires_in`，提前约 30 秒重新交换并更新 SDK 的 Authorization header。同一用户并发刷新应合并成一次请求。解码只用于安排刷新，不代表授权有效；不要向调用应用分发签名密钥。单次运行也受令牌到期时间约束。

Claude Code SDK 配置一个 HTTP MCP Server：URL 使用返回的 `gateway_url`，headers 使用 `Authorization: Bearer <access_token>`。网关合并当前用户已启用且应用允许的服务工具，给工具名称加稳定命名空间，并在描述中保留服务及原始工具名。仍可用同一委托令牌访问 `/mcp/{server_id}` 保留原始工具名，或 `GET /api/mcp/self/config` 读取可用服务及网关地址。MCP `tools/call` 可以携带 `Idempotency-Key`；知识咨询继续使用参数中的 `request_id`。不要对有副作用的工具在未知结果后用新幂等键盲目重试。

每次 HTTP 请求重新核验委托及 Casdoor 状态，每次新执行前再次核验，执行期间每 30 秒检查一次，返回新结果前再核验。撤销 Casdoor 授权、禁用应用、修改应用登记、解绑身份或失效用户认证版本都会阻止后续请求；停用安装或修改连接版本会移除对应服务。已执行的工作保留持久化计费结果，撤销后不再交付新结果。Bearer 凭证可在有效授权期间重复用于多个请求，业务重放依赖既有幂等与结算机制。应用交换和网关访问写入独立安全审计，包含用户、应用、请求及结果状态，不记录交换密钥、Casdoor Token、委托令牌或上游凭据。

委托身份没有模型 TokenID，继续走现有钱包／订阅预扣、结算、退款和恢复机制。付款人由 new-api 根据已验证用户和既有个人／团队计费规则解析，不接受 known-engine 提交用户或付款账户 ID。单次工具价格还必须通过受信应用预算限制。

### known-engine JWT 缓存

公开知识库适配器按 new-api 用户和引擎侧受信应用复用 known-engine JWT。缓存键还绑定引擎地址、Casdoor 发行方／组织／provider、源 Access Token 和有效期、交换密钥、OAuth 客户端密钥及 scopes 配置，避免跨用户、跨应用或配置变更后复用旧凭据。缓存只在当前进程内保存，最多 1024 条；不同实例各自缓存，重启后首次调用重新交换，不新增表或 Redis 依赖。

有效期取交换请求开始时间加引擎返回的 `expires_in`，再与用户源授权到期时间取较早值，避免网络等待延长令牌寿命。正常令牌提前 30 秒刷新；短期令牌提前其有效期的 10%（最多 30 秒）刷新。同一缓存键的并发刷新合并成一次；调用方取消等待不会中断其他调用方共享的交换，交换本身仍受服务超时限制。刷新失败直接返回错误，不使用临过期缓存继续调用业务接口，也不缓存失败结果。

复用 JWT 前仍实时调用 Casdoor introspection，核对授权当前状态、用户、应用、发行方、受众和到期时间。此检查使用已有 `MCP_KNOWLEDGE_OAUTH_CLIENT_SECRET`；未配置该密钥的 OAuth 公共客户端继续使用原来的逐次 app-exchange 路径，避免在无法验证实时撤销时使用缓存。因此，要启用缓存，应在 new-api 配置知识应用的 Casdoor 客户端密钥；它不同于引擎交换密钥 `MCP_KNOWLEDGE_CLIENT_SECRET`。

知识库公开状态、安装启用状态和任务归属不缓存，仍按原路径检查。引擎在知识库检查、任务创建或进度流返回 401 时，移除对应缓存，下一次请求重新交换；不会自动重放创建任务等业务请求。迟到的旧令牌 401 不会删除已经刷新的新令牌。用户删除或更换源授权、切换应用或修改交换凭据后，旧缓存不可代替当前授权。

### 安全依据和验证

实现参考 [OWASP Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)、[Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)、[OAuth2](https://cheatsheetseries.owasp.org/cheatsheets/OAuth2_Cheat_Sheet.html) 及 [MCP Security](https://cheatsheetseries.owasp.org/cheatsheets/MCP_Security_Cheat_Sheet.html)。针对 [ASVS 5.0.0 V9](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x18-V9-Self-contained-Tokens.md) 的 9.1.1–9.1.3、9.2.1–9.2.3 和 [V10](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x19-V10-OAuth-and-OIDC.md) 的 10.3.1–10.3.3，验证可信签名、用途／受众隔离、有效期、授权范围和稳定身份映射；应用回程认证参考 10.4.10。用户同意由 Casdoor 管理，部署时需配置并验收 scope 的展示、授权及撤销。此变更不宣称整个项目通过 ASVS 认证；没有实现 L3 的 mTLS／DPoP 发送方绑定。

回归扩展在既有 `router/mcp_router_test.go`，覆盖真实 HTTP 路由及 MCP SDK 初始化／工具调用、上游授权复用和后台测试、两个服务的同名工具隔离、跨用户隔离、错误应用／签名／发行方／受众／组织／scope、MCP JWT 用途／类型／算法隔离、公开载荷凭据保护、时间声明与加密状态一致性、实时撤销、过期、用户禁用、认证版本变化、应用服务范围／预算、API Key／PAT 拒绝、重复扣费防护、余额不足、工具错误退款及审计脱敏。Casdoor 和上游服务使用本地 HTTP 契约夹具；实际私有 Casdoor 与 known-engine Claude Code SDK 的部署联调需在配置真实应用后进行。

验证环境：SQLite **3.50.4**、MySQL **8.0.46**、PostgreSQL **15.19**；独立日志库 ClickHouse **25.8.33.6**。数据库均为本机隔离测试实例，MySQL／PostgreSQL 的网关测试只使用 `mcp_router_test`，每次运行前重建该测试库。没有修改模型、数据库驱动、表结构或迁移，因此不涉及发布版 schema 升级。

执行命令（下列数据库密码均为隔离测试实例凭据）：

```sh
go test ./service/mcp ./router ./model -run TestMCP -count=1
go test -race ./router -run TestMCPManagementHTTPContract -count=1
go build ./...

MCP_ROUTER_TEST_SQL_DSN='root@tcp(127.0.0.1:23306)/mcp_router_test?parseTime=true&charset=utf8mb4' go test ./router -run TestMCPManagementHTTPContract -count=1 -v
MCP_ROUTER_TEST_SQL_DSN='postgres://postgres:mcp-local-test@127.0.0.1:25432/mcp_router_test?sslmode=disable' go test ./router -run TestMCPManagementHTTPContract -count=1 -v
MCP_ROUTER_TEST_LOG_SQL_DSN="clickhouse://mcp_gateway_test:${MCP_GATEWAY_TEST_PASSWORD}@127.0.0.1:56751/mcp_gateway_log_test" go test ./router -run TestMCPManagementHTTPContract -count=1 -v
```

ClickHouse 使用仅授权 `mcp_gateway_log_test.*` 的临时测试账号和本轮随机生成的密码，测试结束后删除账号；不会修改已有默认账号。独立日志验证通过公开的审计查询接口读取原生 JSON，并检查写入的应用／网关事件和凭据脱敏。

本轮结果：上述构建、MCP 普通回归及竞态检查、SQLite／MySQL／PostgreSQL HTTP 合约回归、ClickHouse 独立日志库回归均通过。未修改或部署 known-engine，也未修改真实 Casdoor 的应用配置；真实授权展示与 Claude Code SDK 部署联调不在本轮验证结果内。

MCP 专用 JWT 更新验证：以上命令已针对 JWT 版本重新执行并全部通过，数据库版本不变；未增加数据库表、迁移或 Redis 依赖。新增验证包括两个受信调用应用各自交换 JWT、`exp` 与 `expires_at` 一致、可读载荷不含明文上游凭据、用途／受众／签名算法／类型隔离，以及用户／应用／时间声明与加密授权状态的绑定。原有后台测试、授权复用、MCP SDK 调用、幂等扣费和退款回归均通过。旧不透明令牌需重新交换；尚未部署生产环境。

引擎 JWT 缓存验证沿用同一 SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 15.19 隔离测试环境。除前述完整 MCP 回归、构建外，执行：

```sh
go test -race ./service/mcp ./router -run 'TestMCPOAuthCryptographicAndLiveAuthorization|TestMCPManagementHTTPContract' -count=1
MCP_SERVICE_TEST_SQL_DSN='root@tcp(127.0.0.1:23306)/mcp_service_test?parseTime=true&charset=utf8mb4' go test ./service/mcp -run TestMCPOAuthCryptographicAndLiveAuthorization -count=1 -v
MCP_SERVICE_TEST_SQL_DSN='postgres://postgres:mcp-local-test@127.0.0.1:25432/mcp_service_test?sslmode=disable' go test ./service/mcp -run TestMCPOAuthCryptographicAndLiveAuthorization -count=1 -v
```

服务级数据库用例运行前重建专用 `mcp_service_test` 测试库。回归验证发现、创建、幂等重放、查询及返回前复核复用同一 JWT；并覆盖提前刷新与到期、刷新失败、并发刷新与首个调用方取消、用户／应用隔离、源授权删除／更换、密钥轮换、短期授权上限、Casdoor 实时撤销、引擎 401 失效及迟到旧 401 不清除新令牌。知识库私有化仍阻止缓存结果交付；无 introspection 密钥时保留逐次交换兼容路径。安全依据继续采用前述 OWASP 指南及 ASVS 5.0.0 有效期、受众、授权和稳定用户标识控制；没有新增持久化 schema，真实 Casdoor／known-engine 部署联调仍需在目标环境进行。

本轮缓存结果：完整 MCP 回归、构建、服务／路由竞态检查及 SQLite／MySQL／PostgreSQL 服务级回归全部通过；没有修改后台连接／工具测试入口或用户既有授权存储。代码尚未部署。

## 11. 恢复既有余额链路（2026-11-21）

本节覆盖前文关于全局额度操作表和同步 SQL 余额链路的设计。按本次范围要求，钱包、Token、缓存、签到、人工调额、订阅、模型资金会话、异步任务及 Midjourney 退款实现恢复到 `7b4a212655bf6ea9d87ad44b5aee6eba51380931` 的父提交；保留原来的 Redis 原子预扣、批量额度队列、缓存增量更新及 Redis 不可用时的数据库条件扣减。不再新增 `QuotaOperation`、全局迁移或 `quota_reconcile` 任务，也不删除任何数据库中已经存在的表或记录。

MCP 的资金编排只调用既有额度接口，调用账本继续负责防止同一次操作被并发重复执行。`applied` 表示既有接口接受操作，不承诺进程内批量队列已经持久化。余额操作之后、账本写回之前发生故障时，`executing` 操作不自动重放，保留调用 ID 和操作 ID 供人工核对。批量模式的预扣标记为 `batch:<进程随机标识>`；只有同一发起进程才自动完成后续资金处理，其他节点或重启后的进程不得根据账本直接退款，避免给未落库的扣款发放退款。原有批量队列的崩溃丢失窗口、缓存丢失及降级行为保持原样，本次不宣称把它们升级为持久化 exactly-once 资金链路。

MCP 订阅退款单独在一个事务内更新订阅额度和预扣记录，以避免 SQLite WAL 下跨事务读写冲突；公共订阅 API 保持原样。原有共享额度回归测试恢复原始断言，MCP 测试集中验证与模型并发预扣、批量落库、重复调用、Token 不足补偿、订阅付款偏好及不确定操作不重复扣款/退款。

本轮实测（以上余额回退代码，不包括生产部署）：

- `go test ./model ./service ./service/mcp ./controller ./router ./middleware -count=1`：通过；后续 MCP 批量归属保护更新后，补跑 `go test ./model ./service/mcp ./router -count=1`，通过。
- `go test ./model -run '^TestMCP' -count=1 -v`：真实 SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 15.19 分别通过。MySQL 使用 `MCP_TEST_MYSQL_DSN` 指向 `127.0.0.1:23306/mcp_test`；PostgreSQL 使用 `MCP_TEST_POSTGRES_DSN` 指向 `127.0.0.1:25432/mcp_test`，密码由隔离测试环境提供。两者设置 `MCP_TEST_REDIS_ADDR=127.0.0.1:26379`，使用 Redis 7.4.11、DB 14；没有连接生产数据库。日志分别位于 `/tmp/new-api-legacy-quota-sqlite.log`、`/tmp/new-api-legacy-quota-mysql.log`、`/tmp/new-api-legacy-quota-postgres.log`。
- `go test -race ./model -run '^TestMCP(AndModelConcurrentReservation|ConcurrentDuplicateDoesNotRepeatBalanceMutation|AbandonedBatchCannotCreateRefundCredit|ReservationUsesLegacyBatchQueue)$' -count=1`：通过。
- 数据库用例包含新建 MCP 表、历史 MCP 字段升级、重复迁移和已有数据保留检查。对配置的 origin、upstream 执行 `git ls-remote --tags` 均没有返回发布标签，尚无可确认的最新发布版基线，因此未完成“最新发布版整库升级 + 完整启动两次”的发布演练，不将上述定向验证等同于发布兼容性验收。此次未改日志库迁移及日志写入实现，也未重新验证 ClickHouse。

## 12. Public Knowledge 按固定知识库 URL 注册（2026-11-22）

后台统一使用 Streamable HTTP 和完整 MCP URL，例如 `https://gateway.example/mcp/providers/knowledge/library-owner/manual` 与 `https://gateway.example/mcp/providers/knowledge/library-owner/second-manual`。域名必须与部署的 `MCP_PUBLIC_BASE_URL` 一致。服务端登记时从 URL 固定绑定 owner/kb_id，后续只能编辑展示资料和运行限制，换库需要新建服务。不存在一个留空 owner/kb_id、由工具调用选择库的通用公开服务。旧实例 ID 路由及对应 metadata 路由已删除，不迁移旧登记记录。

登记后仍需管理员授权、发现并启用工具、配置展示资料并发布；发布检查目标库的 `public_read`。用户安装后通过既有 OAuth 授权流程连接知识应用，有效的本人应用授权可复用，但每个服务的安装资格独立。后端统一使用 `MCP_KNOWLEDGE_ENGINE_URL`、`MCP_KNOWLEDGE_CLIENT_ID` 和 `MCP_KNOWLEDGE_CLIENT_SECRET`，不把交换密钥交给客户端。每个服务仅提供创建咨询和读取本人咨询结果，服务、用户、知识库、会话与任务之间的绑定继续在后端检查。

前端复用现有 `ServerForm`、共享 `Dialog`、`Form`、`Input`、`Select` 和安装授权组件，移除公共知识库专用传输选项及 owner/kb_id 输入框，无新增组件或翻译键。

验证结果：

| 环境 | 命令／范围 | 结果 |
| --- | --- | --- |
| SQLite 3.50.4（项目 Go 驱动） | `go test ./service/mcp ./router -count=1` | 通过 |
| MySQL 8.0.46 | 下列服务注册、OAuth/双库执行、HTTP 合约三个定向用例 | 全部通过 |
| PostgreSQL 15.19 | 同上 | 全部通过 |
| Go | `go build ./...` | 通过 |
| 前端，Node 24.21.0、Bun 1.4.3 | `bun run typecheck`；`bun run test src/features/mcp/__tests__/mcp.test.tsx`；`bun run build` | 通过，38 个测试通过 |
| 前端静态检查 | `bun x oxlint -c .oxlintrc.json src/features/mcp/server-form.tsx src/features/mcp/__tests__/mcp.test.tsx`；`bun x oxfmt --check src/features/mcp/server-form.tsx` | 无 lint error，表单格式通过；测试文件原有一条 prefer-template warning 及整文件格式差异，未进行无关格式重写 |

MySQL/PostgreSQL 使用专用本地测试库。以下每个定向用例运行前均重建对应测试库；原有测试库已备份并在验证后恢复。没有修改 schema、迁移、驱动或日志库行为。

```sh
# MySQL；PostgreSQL 时替换为下方 DSN。
export MCP_SERVICE_TEST_SQL_DSN='root@tcp(127.0.0.1:23306)/mcp_service_test?charset=utf8mb4&parseTime=True&loc=Local'
# export MCP_SERVICE_TEST_SQL_DSN='postgres://postgres:mcp-local-test@127.0.0.1:25432/mcp_service_test?sslmode=disable'
go test ./service/mcp -run '^TestMCPPublicLibraryURLRegistration$' -count=1
go test ./service/mcp -run '^TestMCPOAuthCryptographicAndLiveAuthorization$' -count=1
export MCP_ROUTER_TEST_SQL_DSN='root@tcp(127.0.0.1:23306)/mcp_router_test?charset=utf8mb4&parseTime=True&loc=Local'
# export MCP_ROUTER_TEST_SQL_DSN='postgres://postgres:mcp-local-test@127.0.0.1:25432/mcp_router_test?sslmode=disable'
go test ./router -run '^TestMCPManagementHTTPContract$' -count=1
```

本轮覆盖完整 URL 登记、同 owner 多库及不同 owner 同名库、重复登记、不可改绑、非法 URL、未注册／未启用／旧地址的 404、未授权请求的 401、OAuth PKCE S256、回调的用户／浏览器会话绑定、过期及重放拒绝、两库各自创建和查询、跨库令牌、跨库／跨用户任务结果隔离、授权撤回和公开库转私有。安全依据为 [OWASP ASVS 5.0.0 OAuth/OIDC](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x19-V10-OAuth-and-OIDC.md) 的 10.1.2、10.2.1、10.3.1–10.3.3，以及 [授权控制](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x17-V8-Authorization.md) 的 8.2.2、8.3.1；同时参照 [Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)、[Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)、[OAuth2](https://cheatsheetseries.owasp.org/cheatsheets/OAuth2_Cheat_Sheet.html) 和 [CSRF](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html) 指南。

OAuth 与 known-engine 在本轮集成测试中使用 HTTP 测试服务，数据库为真实实例；没有进行目标运行实例上的浏览器授权或付费模型咨询，也没有重启部署。以上为所列控制的回归证据，不是整个应用的 ASVS 合规声明。前端测试使用 PATH 中的 Node 运行 Vitest；缺少 Node 而回退为 Bun 运行 Vitest 时，既有依赖会在 Zod 导入处失败。

### 发现工具时补齐知识应用授权（2026-11-22）

修正首次 URL 重构遗漏：Public Knowledge 的工具发现曾直接返回内置 schema，未触发知识应用授权，导致发现成功后发布才报 `ENGINE_AUTHORIZATION_REQUIRED`。现在 `DiscoverTools` 使用与发布相同的 `publicLibraryClient` 检查当前管理员的应用授权及目标库公开状态；失败时不生成可同步的 discovery。前端复用已有发现授权入口，针对 `REAUTHORIZATION_REQUIRED` 和 `ENGINE_AUTHORIZATION_REQUIRED` 跳转 OAuth，成功回调后继续发现并自动同步，发布复用同一授权。

先补回归并确认失败（无授权发现竟返回成功、知识应用错误码未触发跳转），再修正后通过：`go test ./service/mcp ./router -count=1`；真实 SQLite 3.50.4、MySQL 8.0.46、PostgreSQL 15.19 的 `TestMCPOAuthCryptographicAndLiveAuthorization`（命令及专用 DSN 同上）；`bun run typecheck`、`bun run test src/features/mcp/__tests__/mcp.test.tsx`（41 个用例）、`bun run build`；涉及文件 lint 无 error，`tool-panel.tsx` 格式检查通过。回归覆盖无授权、撤销授权、库转私有、授权后发现和发布，以及浏览器授权回跳后单次发现／同步。没有修改 OAuth 凭据、scope、PKCE、nonce、回调会话绑定或 token 校验规则；安全依据沿用本节 ASVS 5.0.0 10.1.2、10.2.1、10.3.1–10.3.3、8.2.2、8.3.1 及 OWASP 指南。

修复版已重新构建并部署到本地 `new-api-dev`（`:3000`），前端开发服务（`:5173`）加载新代码，Casdoor 转发容器已重新连接。部署后 API 状态、Casdoor discovery 和 known-engine 连通检查通过。未代替用户授予知识应用权限；原生浏览器验证因用户切换活动窗口而停止，真实账号的 OAuth 确认及最终发布仍由用户操作。

### 直接启用授权入口与本地账号排查（2026-11-22）

管理员直接启用时，如服务返回 `ENGINE_AUTHORIZATION_REQUIRED` 或 `REAUTHORIZATION_REQUIRED`，现在复用工具发现的授权入口；回调继续发现／同步，管理员随后再次确认启用。复用现有 ConfirmDialog 与回调流程，不新增授权页面，不自动放宽服务端身份校验。本轮仅调整前端入口，不改变数据库或 OAuth 协议；安全依据沿用上节 OWASP ASVS 5.0.0 与 OAuth2、Authentication、Session Management 指南。

本地 `Reconnect the trusted knowledge application` 的已确认原因：失败回调的访问令牌 `owner=built-in`，运行配置只信任 `mcp-local`；平台绑定账号为 `mcp-local/test_casdoor`，该账号的回调和发现成功。未修改组织信任边界，未记录原始令牌或授权码。支持其他组织需要同时修改 new-api 与 known-engine 的显式信任配置，并保留平台身份一致性校验，不能通过移除 owner 校验修复账号不一致。

本次验证：新增“直接启用触发授权”用例先失败、修改后通过；MCP 文件共 42 个用例中 41 个通过，既有 `pricing=null` 定价弹窗用例两次均因找不到价格 textbox 失败，未宣称整套测试通过。`bun run typecheck`、涉及文件 oxlint（仅既有 prefer-template warning）及 `bun run build` 通过。前端开发服务加载本次入口修复，未重建无改动的后端。

用户明确要求启用时不要自动 OAuth，已撤回本节的启用自动跳转及对应测试；发现工具入口的授权行为保持原样。上述直接启用跳转仅为已撤回方案的验证记录，不代表当前行为。

### 定价测试隔离修复（2026-11-22）

调用栈确认先前定价失败来自管理页遗留的 `/api/status` 请求：在定价用例设置 TOKENS 后，`fetchStatus` 将全局币种覆盖为本地服务的 CNY，导致 USD 标签断言失败。现于 MCP 测试文件模拟 `getStatus` 固定响应，并在清理时先卸载组件、取消查询，再清缓存、恢复系统配置。没有修改定价产品行为。`bun run test src/features/mcp/__tests__/mcp.test.tsx` 41/41 通过；`bun run typecheck` 通过；该文件 oxlint 无 error，仅既有 prefer-template warning。当前 `:5173` 是 Rsbuild 开发服务，撤回启用自动 OAuth 刷新即可生效，无需重建后端镜像；此次测试隔离改动也不需要部署。

### 管理员发布与用户授权分离（2026-11-22，当前行为）

纠正前述“发现与启用要求管理员个人知识授权”的流程。Public Knowledge 的工具 schema 为本地固定定义：后台发现、同步、审核定价和发布只要求平台管理权限，不再调用 `publicLibraryClient`，所以未绑定 Casdoor 的 root 也可以管理。保留固定 owner/kb_id URL、显示资料、已启用且已定价工具、加密配置和公开 URL 等发布约束。启用不自动跳转 OAuth，也不会给管理员创建个人知识授权。

发布成功仅表示目录配置可用，不代表对目标库完成连通性或公开状态验证。用户安装后的 OAuth、实际测试调用、咨询创建、结果读取仍沿用个人授权校验；每次业务访问及结果交付继续验证库公开状态、当前授权、用户及任务归属。未新增服务账号或管理员代用凭据，未扩大 Casdoor 组织信任范围。前述让管理员先 OAuth 的方案已被本节取代。

回归在既有 `TestMCPOAuthCryptographicAndLiveAuthorization` 中新增无 Casdoor 绑定、无知识授权的 root，验证其发现／同步／启用成功，但访问知识客户端仍被拒绝；授权撤销后管理发现仍可用，实际调用继续失败。先验证旧实现会报 `ENGINE_AUTHORIZATION_REQUIRED`，再移除管理阶段的个人授权依赖。`go test ./service/mcp ./router -count=1` 通过（SQLite 3.50.4）；真实 MySQL 8.0.46 和 PostgreSQL 15.19 上同一 OAuth／实时授权测试通过，执行命令为 `python3 /tmp/public-knowledge-db-matrix.py mysql TestMCPOAuthCryptographicAndLiveAuthorization` 及 postgres 对应命令，DSN 与前文相同。数据库结构与 SQL 未修改。

安全边界沿用 OWASP ASVS 5.0.0 的授权控制 8.2.2、8.3.1 及 OAuth/OIDC 10.1.2、10.2.1、10.3.1–10.3.3，参照前文 Authentication、Session Management、OAuth2 与 CSRF 指南。管理路由保留 AdminAuth、MCP 权限及 CSRF guard；个人 OAuth 的签名、受众、组织、nonce、PKCE、会话绑定、过期与重放校验未放宽，现有相关回归通过。这不构成整个应用的 ASVS 合规声明。

上述后端修复已通过 `docker compose -f docker-compose.dev.yml build new-api` 与 `up -d --no-deps new-api` 部署到本地 `new-api-dev`，并重新建立 Casdoor loopback 转发。运行镜像为 `sha256:a4dd50de10cddd06301207928305259db3bb5ee76e93106ea20bd38a01353ed2`，回退镜像标签为 `new-api-dev:before-admin-publication-20261122`。部署后 `/api/status` 返回 200/success，容器内 Casdoor discovery 与 known-engine OpenAPI 均可达。现有 Public Knowledge 配置保留未启用状态，两个工具已满足审核／启用／定价条件；未代替用户点击发布。

### Public Knowledge SSE consultation updates

`get_consultation_result` accepts the standard MCP request metadata
`params._meta.progressToken`. With a token, the provider consumes the engine SSE
stream until completion or the configured invocation/credential deadline, rather
than using the five-second snapshot window. Without a token, snapshot behavior is
unchanged. Invocation replay returns the saved final result; it does not replay
preview notifications.

Intermediate messages are standard `notifications/progress` messages. Their
`progressToken` correlates the request and `progress` increases monotonically.
`params._meta["knowyet.com/consultation"]` is an application extension, not a
standard MCP partial tool result. Its payload includes `task_id` and one of:

- `type: "answer_delta"`, `text`: append this text to the answer preview.
- `type: "citation"`: allowlisted `title`, `url`, `reference` fields.
- `type: "status"`, `status`: consultation status.
- `type: "progress"`, `message`: sanitized execution progress.

The single final JSON-RPC response retains the complete `content` and
`structuredContent`, including billing metadata. Clients should replace previews
with that authoritative result, rather than append it. Generic clients may ignore
the extension and still consume the final result. The web test endpoint supports `Accept: text/event-stream`: an `invocation`
event supplies the durable invocation ID, `message` events carry the same MCP
progress envelopes, and `complete` tells the client to finish reading the stream.
The dashboard consumes this channel for `get_consultation_result`, renders
sanitized Markdown and citations, and still reads the authorized saved result
for final content and billing. Disconnection falls back to result polling rather
than resubmitting the call. Retrying an accepted idempotency key does not replay
preview events or duplicate execution. Leaving the page closes the browser
subscription without cancelling the durable invocation.

No raw context, reasoning, or upstream error messages are forwarded. The existing
live application/library authorization check runs before each preview release,
and credential/invocation deadlines remain enforced. This applies the server-side
session expiry and authorization principles in the OWASP
[Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
and [Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html);
it is not a claim of a full ASVS audit.

Validation: `go test ./service/mcp ./router -count=1` passed. Regression coverage
includes actual MCP SSE framing/token correlation/final result, delivery before
upstream completion, private-event filtering, stream truncation, upstream failure,
cancellation, and revocation before an answer delta is released. No database
schema, query, or persistence behavior changes are included in this stream change.

Dashboard stream validation: `go test ./router ./service/mcp -count=1` covers
SSE acceptance/completion, idempotent replay and one-time billing, saved-result
retrieval and cross-user isolation. The UI reuses `components/ui/markdown.tsx`
and the existing `sse.js` transport with shared `getFreshAuthHeaders`; the
playground hook is chat-completion-specific and cannot parse MCP notifications.

Local rollout (2026-11-22): rebuilt and recreated `new-api-dev` with
`docker compose -f docker-compose.dev.yml build new-api` and
`docker compose -f docker-compose.dev.yml up -d --no-deps new-api`; recreated its
Casdoor loopback sidecar against the new container namespace. Restarted the
Rsbuild frontend on port 5173. Backend health, frontend HTTP response and Casdoor
discovery succeeded. The previously reported consultation task still returns
completed with 479 answer characters through its authenticated upstream SSE.
Frontend validation: 48 tests passed across `mcp.test.tsx` and
`test-stream.test.ts` under Node 24; `bun run typecheck` and `bun run build` passed.
Targeted lint has no errors (one pre-existing prefer-template warning).
The browser loaded the updated local MCP management page; full live browser
consultation rendering was not verified (the available browser session belongs
to a different user from the consultation owner). Component/transport tests cover
incremental rendering, final replacement, disconnect fallback and safe citations.


### Public Knowledge 动态 OAuth 客户端准入（2026-11-22）

Public Knowledge 不再使用 `MCP_OAUTH_ALLOWED_CLIENT_IDS` 判断外部客户端。Casdoor 在现有的、需服务端身份认证的令牌内省响应中增加 `client_organization` 和 `client_dynamic_mcp`；后者来自当前应用记录：必须具有动态注册凭据、类型为 MCP、允许 authorization_code 且未禁用登录。注册凭据本身不对外返回。new-api 要求应用组织与受信任组织一致；旧 Casdoor 缺失字段时拒绝访问。其他 MCP 类型保持原准入规则。

保留本地 JWT 签名/issuer/有效期/唯一资源 audience/用户组织/scope 校验，以及每次请求的在线内省、主体和 client_id 一致性、有效期和撤销校验。调用权限仍同时取 JWT 与内省 scope 的交集。`MCP_OAUTH_CLIENT_ID/SECRET` 是资源服务访问内省接口的身份，不能理解成允许调用的客户端名单。

安全参考：OWASP ASVS 5.0.0（v5.0.0-9.1.1、9.1.2、9.1.3、9.2.3、10.3.1、10.3.2、10.3.3）；Authentication、Session Management、OAuth2 Cheat Sheets（https://cheatsheetseries.owasp.org/）。本次检查范围为动态客户端准入及原令牌保护的回归，不代表整个系统已通过 ASVS 审计。

验证：`go test ./service/mcp ./router -count=1`；Casdoor `go test ./object -run 'TestDynamicMCPClientAdmission|TestDiscoveryRegistrationUsesMCPLocalOrganization' -count=1`。覆盖动态客户端无白名单准入、静态/禁用/缺失/跨组织注册拒绝、伪造签名、过期、错误 issuer/audience/用户组织、scope 降级、撤销和内省客户端不一致。无数据库 schema、查询或写入逻辑修改。

本地部署验证：Casdoor 与 new-api 已重建并重启；使用 Qwen 已取得的动态客户端令牌（未添加客户端白名单），`initialize` 返回 200、`notifications/initialized` 返回 202、`tools/list` 返回 200，发现 `create_consultation` 和 `get_consultation_result`。未代用户创建新的计费咨询任务。


### 流式调用授权与连接复用（2026-11-22）

`get_consultation_result` 在方法入口完整校验一次用户授权、任务归属与知识库访问权限。同一次调用的增量推送及最终返回不再重复在线校验。调用开始后的撤销/知识库下架在下一次调用时生效；已授权的流受调用超时、取消、调用方令牌及知识应用/引擎凭据有效期限制，不因读取缓冲数据而绕过截止时间。创建咨询保留原有的接受后校验。

本次调用使用按来源、超时与当前可信来源配置隔离的 HTTP 连接池；内部客户端关闭操作不销毁共享连接，调用结束统一关闭空闲连接。连接池不缓存凭据或授权决定，保留来源锁定、SSRF/DNS 地址检查、响应大小限制与跨源重定向限制。本地 Casdoor Nginx 转发使用 HTTP/1.1 上游 keepalive。

安全参考沿用前节 OWASP ASVS 5.0.0 与 Authentication / Session Management / OAuth2 指引。本次明确采用请求级授权语义，不提供流内实时撤销。回归覆盖方法入口只进行一次交换/校验、增量及最终返回不重新授权、撤销后新请求拒绝、取消/过期截止，以及实际 TCP 连接复用和跨源阻断。无数据库行为修改。

验证：`go test ./service/mcp ./router -count=1` 通过，本地重建部署完成。使用 Qwen 现有令牌读取已有咨询，231 条进度通知，最终 done=true、答案 450 字符，约 0.86 秒；该次验证含 HTTP 入口认证共 2 次 Casdoor 内省，全部 200，增量和最终返回不触发在线复核。


## 14. MCP 配置后台化与 stdio 退役（2026-11-24）

本节取代前文的环境变量／受信应用文件配置方式。部署仅保留 `MCP_ENABLED`、`MCP_ENCRYPTION_KEY`、`MCP_ALLOWED_CLIENT_ORIGINS`、`MCP_ALLOWED_UPSTREAM_ORIGINS`。旧业务环境变量和 `MCP_TRUSTED_APPS_FILE` 不再读取，无自动导入；请在现有 MCP 页的“设置”“受信应用”中重新填写。保留数据库与原加密根密钥，避免已有凭据不可解密。

公共地址默认使用站点 ServerAddress，管理前端默认与公共地址一致；开发前端分离时填写 http://localhost:5173。认证 provider 从已有网站 OAuth provider 中选择，要求启用、sub 映射和有效 OIDC discovery；issuer 从配置派生并保存快照，原 provider 地址变化后停止信任。组织、资源令牌校验客户端及知识库连接分别配置。内省客户端身份与调用客户端准入列表不能混用；知识库引擎交换密钥与 Casdoor OAuth 密钥独立保存。秘密字段未提交表示保留，显式空字符串表示清除。

受信应用记录加密存储，交换密钥保存 SHA-256 摘要（至少 32 字符随机值），Casdoor 客户端密钥可解密供在线内省使用。应用范围、启停或凭据变化使旧委托的配置摘要失效。所有配置更新使用版本条件更新，并要求 MCP 敏感写权限、CSRF 检查和绑定操作的一次性安全验证；审计不记录密钥。

stdio 容器执行、命令预览和镜像安装入口已退役。已有 stdio 服务在迁移时停用，保留历史记录；新的 stdio 服务请求或旧服务执行返回 TRANSPORT_RETIRED。

认证验证基线：OWASP ASVS 5.0.0；Authentication、Session Management、OAuth2、CSRF、SSRF 和 Cryptographic Storage Cheat Sheets。保留 issuer/audience/scope、PKCE、一次性 state、组织与身份绑定及在线撤销检查。本节不宣称全量 ASVS 合规。

### 本轮验证记录

- SQLite 3.50.4：临时数据库，运行 `go test -mod=readonly ./service/mcp ./router ./service -run 'TestMCP|TestSecurityProofBindsIdentityAndOperation' -count=1 -timeout=120s` 通过。包含权限拒绝、密钥隔离、配置版本冲突、来源配置、授权失效及历史 stdio 数据保留。
- MySQL 8.0.46：复用仅监听 `127.0.0.1:23306` 的 `codex-mcp-test-mysql`，执行 `MCP_SERVICE_TEST_SQL_DSN='root@tcp(127.0.0.1:23306)/mcp_service_test?charset=utf8mb4&parseTime=True&loc=Local' go test -mod=readonly ./service/mcp -run '^TestMCPDatabaseSettingsAndRetiredTransport$' -v -count=1 -timeout=120s`，通过。
- PostgreSQL 15.19：复用仅监听 `127.0.0.1:25432` 的 `codex-mcp-test-postgres`，执行 `MCP_SERVICE_TEST_SQL_DSN='postgres://postgres:<isolated-test-password>@127.0.0.1:25432/mcp_service_test?sslmode=disable' go test -mod=readonly ./service/mcp -run '^TestMCPDatabaseSettingsAndRetiredTransport$' -v -count=1 -timeout=120s`，通过。
- 数据库回归覆盖配置加密、显式清除、条件更新、受信应用变更、请求快照、授权失效、旧 stdio 服务停用及迁移连续运行两次；MySQL/PostgreSQL 使用已有专用 MCP 测试库，SQLite 使用全新临时库。本轮不涉及独立日志库的表或驱动变更。
- 前端受影响 MCP 测试 45 项通过；执行 `bun run typecheck`、受影响文件 oxlint、`bun run build`。本机使用 `/Users/wangheyu/.bun/bin/bun`。
- 尚未完成最新发布版本整库升级演练及真实远程 MCP、DeepSeek、Casdoor／知识库、委托应用的端到端联调；本轮测试使用真实数据库和模拟认证／上游边界，不能替代上述发布验收。
