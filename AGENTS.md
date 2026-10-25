# Successbox 工作约束与设计基线

## 开始工作前

- 先读本文件，再读本次涉及的 issue 正文、评论及对应源码。不得凭上一轮记忆重新设计已有机制。
- 本文件依据 2026-10-26 阅读的全部 10 个 issue（含关闭的 issue）及 #1 附件整理。issue 的 OPEN 状态不表示整篇已经批准或全部实现；正文的阶段、后续修订和代码证据需要分别核对。
- 用户最新明确指令优先于旧 issue。本文件不能反过来覆盖用户的新决定。发现冲突时说明具体来源，不自行选择一个方便实现的旧方案。
- 写代码前做有界影响检查：所属模块、调用方、配置来源、共享状态、权限、路径、平台和生命周期。先在调用方组合现有能力，不因接线方便改造通用模块。
- 新增公开函数、方法、构造器、包装入口或 RPC，须事先取得用户对具体名称、签名和契约的明确批准；Go internal 包也适用。不要把功能请求解释为新接口批准。
- 新发现的问题先给出证据、影响、修改方案和涉及文件；没有现有明确授权时先等用户决定。已批准的操作不重复询问；不要使用问题框。
- 不新增假想需求的抽象、回退、表、后台任务或平台策略。不得通过跳过失败、改断言或返回模拟成功掩盖接线缺口。
- 不推测 API、参数或协议字段；先查源码、官方文档或实际 help。普通语法修正和已批准的测试工作不需要反复询问。

## Issue 索引与优先关系

| 来源 | 应当如何使用 |
| --- | --- |
| [#1 产品初稿](https://github.com/MeteorsLiu/successbox/issues/1)及附件 | 历史目标。目录白名单、专用镜像、透明 TLS/CA、钥匙串唯一存储、固定性能指标等不能自动成为当前实现要求。后续设计和用户指令已改变多处选择。 |
| [#2 总架构](https://github.com/MeteorsLiu/successbox/issues/2) | 模块和信任边界；LLM/MCP 网关位于 Agent 沙箱外。具体契约以专项 issue 为准，不能把该文旧的“待定”或候选方案视为新授权。 |
| [#3 沙箱协议](https://github.com/MeteorsLiu/successbox/issues/3) | 当前 Create、Exec、Display、Close 的字段、流和生命周期。以 `api/sandbox/v1/sandbox.proto` 对照实现。 |
| [#7 文件与 worktree](https://github.com/MeteorsLiu/successbox/issues/7) | 当前拦截器、按需私有仓库、tmpfs 临时修改和 Close 发布流程。第 8 节文件系统后端方案仅讨论，暂不实施。 |
| [#12 网络与网关](https://github.com/MeteorsLiu/successbox/issues/12) | 已确定共享网关、token 绑定账号和 Bifrost 虚拟 Key；网络章节含旧候选和未完成项，须结合用户最新网络决定及实际代码。 |
| [#13 重复提案](https://github.com/MeteorsLiu/successbox/issues/13) | 已关闭，评论明确以 #14 为准。不得恢复这里的旧存储模型或接口。 |
| [#14 MCP 与敏感字段](https://github.com/MeteorsLiu/successbox/issues/14) | 单表、GatewaySession 双向流、默认提示词、安装审批、热更新、取消和验收。实现状态与剩余集成边界均在正文中。 |
| [#15 统一事件总线](https://github.com/MeteorsLiu/successbox/issues/15) | CLOSED 且有 `proposal-reject` 标签。禁止按正文的旧“待评审”字样实施 FrontendSession、通用 event_type/event_body 总线或多表模型。 |
| [#16 前端](https://github.com/MeteorsLiu/successbox/issues/16) | Agent 配套工具、账号/MCP/改动/可信小窗的产品基线。其 CLI 首发、桌面后续及 mock 阶段描述已被后续真实对接和 Codex Desktop 演示要求推进，不能作为继续只做 CLI 的理由。 |
| [#24 macOS 启动](https://github.com/MeteorsLiu/successbox/issues/24) | Apple Containerization 承载 Linux gVisor 的启动阶段。文中明确未包含完整宿主文件映射和 worktree 回传；VM 能启动不能证明这些已经完成。 |

修改专项逻辑时重新获取对应 issue，避免把这里的摘要当成完整协议。不要未经要求改写远端 issue 的历史结论。

## 当前用户明确要求

- 复用用户完整文件环境，包括项目、配置、Skills、已有会话及应用数据；不得以挑几个配置文件复制到空 HOME 的演示代替。不得要求预先列出项目白名单或制作专用 Agent 镜像。
- 宿主原文件不直接回写。文件访问与修改遵循 #7 的既有 tmpfs + worktree 流程，不另造全局复制、全量快照导出或普通磁盘写入层替代它。
- 当前要真实 Codex Desktop 图形演示，通过 RDP 像打开软件一样使用，并用 OBS 录制。Codex CLI、合成 shell、登录截图和模拟模型回复都不等于完整桌面演示。
- RDP 画面应铺满外层窗口内部，适配窗口大小，键鼠可操作；不是将宿主外层窗口强制全屏。黑色桌面背景、guest 应用未最大化与 RDP 缩放黑边须分别诊断。
- 用户要求关闭 viewer/沙箱时先关闭，不继续抢占桌面。当前已要求退出，重新验证优先使用不弹出窗口的方式，不自动恢复全屏。
- 本次 macOS 演示外层内存配置为 8192 MiB，不能偷偷退回 1 GiB。资源上限不代表已经实测的占用量。
- 网络使用运行时原生网络接入；用户已允许普通联网并要求限制宿主部分接口访问，不得又用自写 Exec/FD relay 替代已要求的 `--network` 路径。
- 当前保留 Apple Containerization 承载 Linux gVisor；不擅自切换 Lume、Docker 或 macOS guest。共享 macOS 文件不代表 Linux 能运行 Mach-O 程序。

## 文件环境：必须复用 #7

所有权与处理路径：

```text
用户原文件与路径 → 沙箱看到的用户环境
  普通写入 → 既有一次性临时文件视图（用户 bind 的 upper 位于 tmpfs）
  首次访问 Git 仓库 → syscall 拦截器 → 私有仓库 → 路径映射
  Close → 停止 guest → helper 发布 detached worktree → 清理临时资源
```

- Linux 服务必须启用已有 `--worktrees <helper>`，使用 Successbox platform。检查实际启动 argv，不能只看到二进制存在就声称已启用。
- 对照 `cmd/successbox-gvisor/server.go`、`worktrees_linux.go`、`runtime/cmd/worktrees/`。当前实现已有 root overlay 和以 tmpfs 为 upper 的 bind overlay；这里的“tmpfs + worktree”不是让人删除内部实现所必需的 overlay，而是禁止绕过 worktree 模式另走普通写入/全量导出方案。
- 仓库在首次访问时准备，不预扫描用户全部目录；保留现有 staged、dirty、untracked 内容。同一沙箱复用映射，不同沙箱分别处理。
- 私有结果目录是预挂载的 `/run/successbox-results`。不要在结果上方再套临时 upper，使仓库修改绕开结果目录。
- Close 才发布结果；仅 Exec 完成、viewer 断开或 RPC 断线不等于发布完成。发布失败保留结果并显示错误，不能无条件删除。
- 产品目标是 Git → worktree、非 Git 工作成果 → 独立目录、系统修改 → 丢弃。#7 明确非 Git 成果导出及系统目录语义分类尚未完成；前端不得把目标写成已实现能力。
- macOS 用户目录映射、Linux UID/GID、符号链接、嵌套挂载、权限与向宿主注册 worktree 需要实际验证。只做只读 virtiofs 分享不能算完整集成。
- 完整文件视图不等于宿主实时原子快照或双向同步；macOS 系统权限拒绝的路径不得假装已读到。已知凭据与运行时状态仍按 #2/#12 的既有保护边界处理，不能通过其他路径别名泄露。
- 不因修复配置继承而递归 chown/copy 整个 HOME，不修改宿主的认证、偏好或欢迎页状态；需要适配的内容只在沙箱私有视图中改。

## 协议和生命周期

- SandboxService：Create / Exec / Display / Close；本地 Unix socket。GatewaySession：宿主 TCP HTTP/2 gRPC。两者不是同一个连接或权限域。
- Create 返回 sandbox ID，不返回 VM 地址。字段以 proto 为准，不杜撰 network、网关对象或 Exec 单独 env/cwd 字段。
- Display 是双向 RDP 字节流，包含画面与键鼠协议；不是截图轮询、JSON 鼠标事件或 TTY。服务端断开 Display 不会关闭 sandbox。
- Exec 一条流对应一次命令；只有 result 才有最终退出码。断线不表示 guest 已退出。PTY 的 resize、信号和 EOF 以现有契约处理。
- Close 只关闭目标 sandbox；不能停止其他实例、共享网关或外层 VM。明确退出管理服务与关闭普通窗口不是同一个动作。
- Sandbox Close、网关 token 撤销、GatewaySession 的 CancelSandboxOperations 是不同操作。最后一项只取消当前 pending 操作，不永久撤销 token，也不删除已保存敏感值。
- 不增加被否决的统一前端事件 RPC。新增能力先看现有双向流是否已提供，不重复设计 watch/submit/cancel 入口。

## 网关与敏感值

- LLM 使用宿主共享 CLIProxyAPI + Successbox scheduler；按认证 token → provider → 实际账号绑定选择，不用模型前缀隔离，不要求过滤其他账号模型的名称。
- token 无绑定或账号不可用必须拒绝，不落到其他账号。插件缺失/禁用/返回未处理会触发上游默认调度，不能在这种状态宣布就绪。
- MCP 使用宿主 Bifrost + virtual key 的 client/tool 授权。virtual key 不会自动隔离同一 client 的 HTTP session 或 stdio 进程；需要独立上游状态时单独验证 client 归属。
- LLM/MCP 数据入口与管理入口分离。guest 不通过 Host、URL 或自报 sandbox_id 选择其他账号或管理权限。不得把整个 Bifrost 管理端口开放给 guest。
- 两种凭据不可混淆：Agent 拿到的是受限网关 token；真实上游 API key/OAuth 在宿主网关使用。完整继承配置仍需在私有视图替换已识别的服务入口和认证。
- SQLite 业务表只有 `sandbox_mcp_secrets`，主键 `(sandbox_id, mcp_id, field_name)`；`mcp_name` 是展示数据。明文保存是已确认选择，不擅自增加加密、七张表、审计/恢复系统。
- `mcp_id` 来自 Bifrost 实际 client ID，不由模型猜测；安装前的请求不携带它，request_id 不能替代它。
- pending 请求在内存中，同键去重；数据库提交后唤醒等待者。列表和事件不返回真实值，只有授权的显式 Get 或网关 Resolve 返回对应值。
- GatewaySession 以 ClientRequest/ClientResponse、ServerRequest/ServerResponse/ServerEvent 表达双向交互；身份来自认证上下文，不在消息里加 role。
- LLM 对认证后的请求默认注入 MCP 安装和 `$PRIVACY_PROTECT_<field_name>` 规则，不检测工具列表后才注入，不虚构客户端没有的可执行工具。
- MCP 网关实际提供 `request_mcp_install`：申请 → 同沙箱去重 → 宿主 UI 审批 → 取得真实 client ID → 敏感值写入 → 注册/发现/授权 → 客户端目录刷新 → 实际调用。
- `APPROVED`、Bifrost 注册、通知已发出、客户端可调用是不同状态。Claude 与 Codex 的刷新机制分别验证；#14 的 Codex app-server 测试方式不等于任意运行中的桌面/CLI 已接好生产控制器。
- 安装期限从首次申请起五分钟，不因重复/重连续期。取消不回滚已发生的远端动作，不自动重放有副作用的调用。

## 前端边界与验收

- Successbox 是 Agent 配套工具，不是另一个 Agent 或通用 VM 管理器。用户流程不暴露 mount、VM IP、token 调度等实现细节。
- Electron renderer 通过已有 preload/IPC 调用 main；main 对接已有服务。真实凭据不得进入 AppState、列表、日志、URL 或模拟数据。
- Codex Desktop 入口必须走真实图形应用 + Display/RDP；缺少桌面接线时明确失败，不能静默启动 Codex CLI。Claude Code 保持独立启动路径，不能因共享 desktop 配置误启动 Codex。
- 模拟模式必须显式启用。真实模式的未接通账号管理、改动读取、MCP 目录或发布能力应明确说明，不返回模拟成功，不显示“已保存”却没有实际记录。
- 新旧 UI 文案必须区分当前能力和目标；不要从 #16 mock 页面复制未实现的开关、即时切换、成果保存承诺。
- 验证真实链路：前端启动 → 正确 Agent → 用户环境 → 模型请求 → MCP 宿主弹窗/调用 → 退出及结果。合成上游、登录画面、编译通过分别说明，不能替代完整 E2E。
- 图形验收包括：普通外层窗口、guest 画面铺满、调整大小、鼠标点击/键盘、关闭路径；OBS 录像须真正由 OBS 产生，不能用静态截图冒充。
- 不自动修改上游应用。用户已批准本次 Codex Linux 桌面演示在 guest 内使用 root 并仅为它添加 `--no-sandbox`，用于处理 Apple 共享目录的 UID 0 与 Chromium 启动限制；外层 gVisor 保留，不把它扩为所有应用默认策略。此前 `--disable-seccomp-filter-sandbox` 是启动对照，不能将两者的保护范围混为一谈。

## 代码与验证入口

| 边界 | 位置 |
| --- | --- |
| 公开协议与对接说明 | `api/sandbox/v1/sandbox.proto`、`api/gateway/v1/gateway.proto`、`api/README.md` |
| Linux 生命周期、网络、挂载 | `cmd/successbox-gvisor/` |
| 拦截器与 worktree 策略 | `runtime/internal/platform/`、`runtime/cmd/worktrees/`、`runtime/README.md` |
| Apple 启动 | `apple/Sources/SuccessboxApple/Main.swift` |
| 网关宿主服务与扩展 | `cmd/sbx/`、`gateways/` |
| 前端 | `desktop/src/main/`、`desktop/src/preload/`、`desktop/src/renderer/`、`desktop/README.md` |
| 原生集成检查 | `testdata/gvisor-worktrees.sh`、`testdata/apple-gvisor.py`、`testdata/apple-gvisor-display.py`、`desktop/test/` |

按实际变更运行检查；React/TypeScript 用 `npm run typecheck`、lint 及受影响真实 UI 路径，Swift 用 `swift build --product successbox-apple` 并做相应原生检查。不要为了赶进度把一平台证据推广到另一平台。当前用户要求不等待 CI，仍须如实记录本地验证与未完成项。

提交前区分任务源码与 `.cache`、录制产物、临时配置及凭据；后者不得混入提交。推送/建 PR 前核对实际 remote URL 和 head/base，不凭 remote 名推断归属。GitHub 正文段落不按固定列宽硬换行。未获当前授权不要自行发布或合并。
