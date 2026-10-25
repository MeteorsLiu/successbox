# 前端对接接口

本文记录当前已实现的契约，字段和编号以 [sandbox.proto](sandbox/v1/sandbox.proto)、[gateway.proto](gateway/v1/gateway.proto) 为准。不新增统一前端 RPC，不修改前端。尚未接通的产品能力列在最后。

## 1. 服务与连接

| 服务 | 方法 | 连接 |
| --- | --- | --- |
| `successbox.sandbox.v1.SandboxService` | Create、Exec、Display、Close | 后端 `--socket` 指定的本地 Unix socket |
| `successbox.gateway.v1.GatewayManagementService` | GatewaySession 双向流 | `sbx gateways --control-listen` 指定的 TCP HTTP/2 地址，例如 `127.0.0.1:9090` |

GatewaySession 是宿主控制接口，与 LLM `/v1/responses`、`/v1/messages` 和 MCP `/mcp` 数据接口分开。它使用原生 gRPC，不是 REST 或 gRPC-Web；当前启动器提供本地明文 HTTP/2。启动参数见 [网关说明](../gateways/README.md#run)。

前端连接网关发送以下 gRPC metadata：

```text
x-successbox-client: frontend
authorization: Bearer <control-frontend-token>
```

这个 token 来自宿主启动配置，不是模型 API key 或 MCP virtual key。Electron 主进程持有连接，renderer 通过 preload/IPC 调用主进程，不直接持有控制 token。真实密钥只在用户输入或明确读取时经过可信窗口，不进入普通状态快照。

socket、网关地址、控制凭据及 sandbox 绑定信息由宿主运行管理层提供。当前没有服务发现、列举全部 sandbox 或查询绑定的 RPC。Create 不会自动建立网关绑定：当前 MCP 适配器使用配置中的 Bifrost virtual-key ID 作为 GatewaySession 的 sandbox scope，实际运行时 ID 与这份配置的接线仍待管理层完成。

## 2. SandboxService

```proto
rpc Create(CreateRequest) returns (SandboxRef);
rpc Exec(stream ExecInput) returns (stream ExecOutput);
rpc Display(stream DisplayInput) returns (stream DisplayOutput);
rpc Close(SandboxRef) returns (Empty);
```

### Create

| 字段 | 类型与含义 |
| --- | --- |
| rootfs | string，后端可访问的 Linux 根文件系统绝对目录 |
| mounts | Mount[]，每项为 source、destination、read_only |
| env | string[]，KEY=VALUE，供进程继承 |
| working_dir | string，沙箱内绝对工作目录；空值使用 `/` |
| user | User，uint32 类型的 uid、gid；未指定时为 0 |
| hostname | string，guest 主机名 |
| init_argv | string[]，初始进程参数；空值使用 `/bin/sleep infinity` |
| desktop | bool，启用已有桌面运行配置；不会安装 RDP 服务或打开窗口 |
| cpus、memory_bytes | uint32、uint64，资源限制；0 使用后端默认行为 |

响应为 `SandboxRef { string id; }`，不返回 VM 地址。Mount 的 destination 不能替换根目录；只读挂载保持只读，可写挂载使用后端私有文件层，不直接写回来源。

Linux 与 macOS gVisor 模式的路径由 Linux 后端解释。macOS gVisor 模式下，`rootfs: "/"` 指 VM 的 Linux 根，而非 macOS 根；原 Apple 直接模式的 rootfs 是 macOS 上提供给 VM 的 Linux 用户空间目录。启动及资源默认值见 [运行说明](../README.md#start-the-service)。

### Exec

| 方向 | 消息 | 内容和顺序 |
| --- | --- | --- |
| 前端 → 服务端 | start | 第一条且仅一次；sandbox_id、非空 argv，可选 terminal { columns, rows } |
| 前端 → 服务端 | stdin | 原始字节，不按 UTF-8 或行重新编码 |
| 前端 → 服务端 | stdin_closed | 仅普通标准流可用；PTY 使用终端控制字符结束输入 |
| 前端 → 服务端 | signal | Linux guest 信号编号 1–64，针对本次程序 |
| 前端 → 服务端 | resize | 仅 PTY 可用；columns、rows 均为 1–65535 |
| 服务端 → 前端 | stdout、stderr | 原始字节；PTY 合并到 stdout |
| 服务端 → 前端 | result | exit_code；收到此消息才取得最终退出结果 |

一条流对应一次执行，继承 Create 的用户、目录和环境，Exec 没有单独覆盖这些配置的字段。普通模式的请求流半关闭会结束 stdin；取消 RPC 不等于终止进程，断连后的输出被丢弃，当前不支持重新附着到原终端。停止程序使用 signal，停止沙箱使用 Close。

程序非零退出仍是成功的 RPC，信号退出使用 `128 + 信号编号`。没有收到 result 就遇到传输错误，不能当作成功，也不能仅凭断线推断程序已停止。

### Display

客户端首条为 `DisplayInput.connect { id }`，服务端先回复 `DisplayOutput.ready { protocol: DISPLAY_PROTOCOL_RDP }`，随后双方通过 data 传 RDP 字节流。每条流对应一次 RDP 连接，断开只关闭 viewer，不关闭 sandbox。

前端需要 RDP 客户端处理画面和键鼠，data 不是图片或 JSON 输入事件。Create 必须设置 desktop，rootfs 中应已安装并启动 RDP 服务及 `/usr/bin/socat`。`sbx display` 可在客户端侧提供仅限 loopback 的测试转发端口。

### Close

请求为 `SandboxRef { id }`，完成后返回 Empty。不存在的 ID 可重复关闭；空 ID 无效。后端停止所属进程并清理临时文件层。Linux worktree 模式在 guest 停止后发布结果，发布失败通过 Close 报错。

Close 不关闭其他 sandbox、共享网关或 macOS gVisor 模式的外层 VM。当前也不会自动调用网关取消接口或撤销绑定，这属于运行管理层尚待接线的工作。

## 3. GatewaySession：四种消息方向

```proto
rpc GatewaySession(stream GatewayClientMessage)
    returns (stream GatewayServerMessage);
```

| 方向 | envelope | 用途 |
| --- | --- | --- |
| 前端请求服务端 | GatewayClientMessage.request | 开会话、恢复 pending、密钥 CRUD |
| 服务端回答前端 | GatewayServerMessage.response | 结果或 operation_error，带原 correlation_id |
| 服务端请求前端 | GatewayServerMessage.request | 输入密钥或审批安装 |
| 前端回答服务端 | GatewayClientMessage.response | 提供／拒绝密钥，批准／拒绝安装 |

GatewayServerMessage.event 是单向通知，携带 event_id，不需要回复。每个 oneof 只能选择一种 body。

| ID | 含义 |
| --- | --- |
| correlation_id | 前端发送消息时分配的非空关联 ID，使用不同值关联并发请求；普通响应原样返回 |
| SecretInputRequest.operation_id | 服务端生成的待输入 ID，ProvideSecret/CancelSecret 使用收到的值，不使用调用方另一侧的 operation_id |
| MCPInstallApprovalRequest.request_id | 服务端生成的安装申请 ID，用于审批和去重 |
| event_id | 当前事件标识，不是持久日志游标或全局顺序号 |
| sandbox_id | 绑定范围，所有密钥身份均包含它 |
| mcp_id | Bifrost 实际 client ID，不由 UI 生成，不能用名称代替 |

OperationError.operation_id 当前填入失败消息的 correlation_id。普通响应按外层 correlation_id 路由，不把错误字段当作密钥 pending ID。

以下采用 Protobuf JSON 映射展示：字段为 lowerCamelCase，bytes 使用 Base64，真实传输仍是 Protobuf。`Zml4dHVyZS1rZXk=` 是合成测试值 `fixture-key`。

### 3.1 打开和恢复会话

第一条消息必须打开会话：

```json
{"correlationId":"open-1","request":{"openSession":{"sandboxId":"sandbox-a","clientVersion":"desktop-dev"}}}
```

```json
{"correlationId":"open-1","response":{"sessionOpened":{"sessionId":"session-1","sandboxId":"sandbox-a"}}}
```

frontend 可以用空 sandbox_id 打开全局管理会话，但每条 CRUD 仍要指定非空 sandbox_id；绑定 A 的会话不能操作 B。全局会话没有列出全部 sandbox 的能力，空 sandbox_id 的 ListMCPSecrets 也不是全局查询。

重连后重新 OpenSession，再重放仍在等待的输入和审批：

```json
{"correlationId":"resume-1","request":{"resumeSession":{"replayPending":true}}}
```

ResumeSession 没有单独成功响应或重放结束消息，last_event_id 未用于历史重放。只重放当前内存 pending，不重放已完成事件，也不因重连重置安装期限。前端分别按 operation_id、request_id 去重。

### 3.2 密钥 CRUD

身份始终是 `(sandbox_id, mcp_id, field_name)`。field_name 匹配 `^[a-z0-9_]+$`，写入值不能为空；mcp_name 仅用于展示。当前没有版本号或 compare-and-swap 更新语义。

| request 消息 | 字段 | 成功响应 |
| --- | --- | --- |
| create_mcp_secret | sandbox_id、mcp_id、可选 mcp_name、field_name、secret_value | mutation_accepted，MCP_SECRET_CREATED；已存在返回 AlreadyExists |
| update_mcp_secret | 同上 | mutation_accepted，MCP_SECRET_UPDATED；不存在返回 NotFound |
| get_mcp_secret | sandbox_id、mcp_id、field_name | secret_record，显式返回该条真实值 |
| list_mcp_secrets | sandbox_id、可选 mcp_id | secret_list.records[]，不含真实值；空 mcp_id 查询该 sandbox 全部 MCP 字段 |
| delete_mcp_secret | sandbox_id、mcp_id、field_name | mutation_accepted，MCP_SECRET_DELETED；不存在返回 NotFound |

删除仍有输入等待的字段返回 FailedPrecondition，需要先取消对应输入。修改非空 mcp_name 会更新同一 sandbox/MCP 的所有字段行；若缓存多个字段的名称，应重新读取该 MCP 列表。

MCPSecretRecord 包含身份字段、mcp_name、secret_value、has_value，以及 Timestamp 类型的 created_at、updated_at、last_used_at。列表、写入响应和事件只返回元数据；未使用时 last_used_at 可缺省，删除响应的 has_value 为 false。列表只列出存在的密钥，不是 MCP 服务目录。

```json
{"correlationId":"list-1","request":{"listMcpSecrets":{"sandboxId":"sandbox-a","mcpId":"mcp-client-7f31"}}}
```

```json
{"correlationId":"list-1","response":{"secretList":{"records":[{"sandboxId":"sandbox-a","mcpId":"mcp-client-7f31","mcpName":"project-tools","fieldName":"api_key","hasValue":true,"createdAt":"2026-10-25T00:00:00Z","updatedAt":"2026-10-25T00:00:00Z"}]}}}
```

### 3.3 输入密钥

服务端请求：

```json
{"request":{"secretInput":{"operationId":"input-1","sandboxId":"sandbox-a","mcpId":"mcp-client-7f31","mcpName":"project-tools","fieldName":"api_key","placeholder":"$PRIVACY_PROTECT_api_key"}}}
```

前端提交：

```json
{"correlationId":"answer-1","response":{"provideSecret":{"operationId":"input-1","sandboxId":"sandbox-a","mcpId":"mcp-client-7f31","mcpName":"project-tools","fieldName":"api_key","secretValue":"Zml4dHVyZS1rZXk="}}}
```

服务端先提交 SQLite，再唤醒等待的网关调用，向前端返回 response.mutation_accepted 并广播 event.secret_changed。ProvideSecret 可创建或更新该字段；它始终保存，不支持 remember=false 或一次性值。SecretValue 返回给有权限的网关连接，不通过前端列表或事件广播。

拒绝时发送：

```json
{"correlationId":"cancel-1","response":{"cancelSecret":{"operationId":"input-1","reason":"user declined"}}}
```

成功取消通过 response.operation_error 返回，code 为 Aborted，message 为 `secret request cancelled`。这是取消完成的业务结果，不表示整个流断开；已不存在的 pending 返回 NotFound。

SecretInputRequest 不含 Agent 名称、cwd、工具名称或 expires_at。网关调用的 deadline 决定等待时间，前端不能自行假定为五分钟。密钥等待超时／调用断开后，没有单独的弹窗取消事件；迟到提交返回 NotFound。它与安装取消事件是两种不同情况。

### 3.4 安装审批

申请由 Agent 适配层或网关发起，frontend 只能接收和审批：

```json
{"request":{"mcpInstallApproval":{"requestId":"install-1","sandboxId":"sandbox-a","mcpName":"project-tools","transport":"http","endpoint":"https://example.invalid/mcp","requestedTools":["search","read"],"credentialFields":["api_key"]}}}
```

transport 为 http、sse 或 stdio。stdio 的 endpoint 是描述 command/args/envs 的 JSON 字符串，沿用现有适配器格式。请求不含 mcp_id，因为尚无实际绑定。requested_tools 是工具名列表，不含描述或写操作分类；credential_fields 是工具参数字段名，不是 OAuth 认证方案。

批准／拒绝：

```json
{"correlationId":"approve-1","response":{"decideMcpInstall":{"operationId":"decision-1","requestId":"install-1","approve":true,"decisionReason":"user approved"}}}
```

DecideMCPInstall 当前只按 request_id 定位申请，operation_id 未用于额外去重。消息没有批准的工具子集或 Agent 列表，不能丢弃 UI 勾选结果后仍显示按所选范围安装。

批准后的消息可能交错：

```text
前端发送 DecideMCPInstall(approve=true)
  → 服务端在 Bifrost 注册，取得真实 mcp_id
  → event.mcp_install_changed(APPROVED, mcp_id)
  → 如缺字段，request.secret_input
  → 前端发送 ProvideSecret，服务端保存并继续
  → 网关完成授权、工具发现和 MCP list_changed 通知
  → response.mcp_install_decision_accepted(APPROVED)
```

不能等待 approve-1 最终响应才继续读消息，否则密钥输入会被自己阻塞。APPROVED 事件表示已推进到注册，最终成功响应表示网关完成本次操作；两者都不等于 Codex/Claude 已刷新可执行工具目录。客户端兼容性见 [网关说明](../gateways/README.md)。

拒绝返回 mcp_install_decision_accepted(REJECTED) 并发送对应事件。安装失败返回 operation_error 并发送 FAILED；原始申请五分钟到期或 sandbox 操作取消发送 CANCELLED。五分钟包含等待审批和字段输入；相同申请重试、通知重放均不延长期限。请求没有绝对截止时间，重连后不能重置为新的五分钟倒计时。

### 3.5 通知和错误

| 通知 | 处理依据 |
| --- | --- |
| event.secret_changed | 按三元键更新元数据；change 为 CREATED、UPDATED、DELETED 或 USED |
| event.mcp_install_changed | 按 request_id 更新申请；change 为 APPROVED、REJECTED、FAILED、CANCELLED 等，mcp_id 在实际绑定前为空 |

完整枚举名以 gateway.proto 为准。通知和普通响应独立处理，不依赖固定到达顺序。

认证失败、首条消息不是 OpenSession 或传输断开使用 gRPC 状态结束流。会话内业务错误通常通过 response.operation_error 返回，不结束流：

```json
{"correlationId":"delete-1","response":{"operationError":{"operationId":"delete-1","code":"NotFound","message":"secret not found"}}}
```

code 使用 Go gRPC 的名称字符串，例如 InvalidArgument、PermissionDenied、NotFound、AlreadyExists、FailedPrecondition、Aborted、Internal，不是整数或全大写枚举字符串。

仅宿主网关可调用 request.cancel_sandbox_operations { sandbox_id }；frontend 调用返回 PermissionDenied。它取消 pending 安装和密钥等待并保留已保存数据，不关闭 sandbox 或撤销绑定；没有合并为前端 Close。

## 4. 原型功能与当前接口

| 功能 | 当前能力／缺口 |
| --- | --- |
| 可信窗口输入／拒绝密钥 | SecretInputRequest、ProvideSecret、CancelSecret；保留 sandbox_id 和 operation_id |
| 密钥列表／增删改查 | 已实现，使用三元键，不能只按 mcp_id + field_name 标识 |
| 安装审批 | 已实现整项批准／拒绝；工具子集、Agent 勾选及一次性 secret 不在契约中 |
| 手动管理 MCP 服务目录 | GatewaySession 不提供目录 CRUD；配置归 Bifrost，密钥列表不能代替目录 |
| 账号导入、登录、切换 | CLIProxyAPI 有自身登录与管理能力，Successbox proto 无对应前端管理消息 |
| 终端启动 Agent、保护开关 | Create/Exec/Close 可作为原语；终端、包装命令、配置副本、网络及账号绑定由运行管理层接线 |
| 活动实例列表、Agent/cwd 来源 | 无枚举／订阅运行实例的 RPC，不能从 sandbox_id 猜测 Agent 信息 |
| worktree 展示、diff、删除结果 | Linux 已有结果清单，当前无对应 RPC；统计不由 Close 返回，macOS 结果发布未完成 |
| 重启恢复、持久事件历史 | 尚未实现，只能重放当前内存 pending |

这些缺口需要后续明确契约后再接入，不在这里添加推测的 RPC、配置字段或数据库表。前端尚未改动。

## 5. 验证入口

```sh
go test -race ./cmd/sbx ./cmd/successbox-gvisor \
  -run 'TestGatewaySession|TestDisplayRelay' -count=1
```

已有测试通过真实 TCP gRPC 验证会话、CRUD、SQLite 持久化、输入／拒绝、范围隔离、安装、取消和重放。真实 sandbox、TTY、RDP 验证见 [主 README](../README.md#run-gvisor-on-macos)。这里的 DisplayRelay 使用受控运行时替身，不替代真实 VM 验证。
