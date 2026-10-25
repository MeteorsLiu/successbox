# Successbox

[![Go CI](https://github.com/MeteorsLiu/successbox/actions/workflows/go.yml/badge.svg)](https://github.com/MeteorsLiu/successbox/actions/workflows/go.yml)
[![Platform CI](https://github.com/MeteorsLiu/successbox/actions/workflows/platform.yml/badge.svg)](https://github.com/MeteorsLiu/successbox/actions/workflows/platform.yml)
[![Gateway tests](https://github.com/MeteorsLiu/successbox/actions/workflows/gateways.yml/badge.svg)](https://github.com/MeteorsLiu/successbox/actions/workflows/gateways.yml)
[![Apple backend](https://github.com/MeteorsLiu/successbox/actions/workflows/apple.yml/badge.svg)](https://github.com/MeteorsLiu/successbox/actions/workflows/apple.yml)
![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8?style=flat-square&logo=go&logoColor=white)
![Swift 6.2](https://img.shields.io/badge/Swift-6.2-F05138?style=flat-square&logo=swift&logoColor=white)
![gVisor](https://img.shields.io/badge/runtime-gVisor-6F42C1?style=flat-square)
![macOS 26](https://img.shields.io/badge/macOS-26-000000?style=flat-square&logo=apple&logoColor=white)

**让 Claude Code 和 Codex 在熟悉的工作流中获得隔离保护。**

Successbox 是 Claude Code 和 Codex 的桌面配套工具，围绕执行隔离、账号与 MCP 连接、敏感信息交互和改动保存展开。它希望让你继续使用熟悉的 Agent、项目、配置、Skills 和会话，同时将运行中的修改留在私有环境中，将上游凭据留在宿主网关。

**[总体架构](https://github.com/MeteorsLiu/successbox/issues/2) · [产品设计](https://github.com/MeteorsLiu/successbox/issues/16) · [本地运行](#快速开始) · [API 文档](api/README.md)**

项目仍在开发中，运行时、网关与桌面端正在逐步接通。下文区分产品目标和当前 `main` 分支的实现；完整用户环境继承与桌面端到端流程尚未完成。

## 先看这些

| 主题 | 入口 |
| --- | --- |
| 产品方向 | [初版产品设计 #1](https://github.com/MeteorsLiu/successbox/issues/1) · [桌面配套工具 #16](https://github.com/MeteorsLiu/successbox/issues/16) |
| 总体架构 | [Agent 沙箱、LLM 网关与 Bifrost MCP 网关 #2](https://github.com/MeteorsLiu/successbox/issues/2) |
| 沙箱协议 | [Create、Exec、Display、Close #3](https://github.com/MeteorsLiu/successbox/issues/3) · [API 契约](api/README.md) |
| 文件与成果 | [worktree 设计 #7](https://github.com/MeteorsLiu/successbox/issues/7) · [runtime 边界](runtime/README.md) |
| 网络与网关 | [网络和网关生命周期 #12](https://github.com/MeteorsLiu/successbox/issues/12) · [网关运行说明](gateways/README.md) |
| MCP 与敏感字段 | [MCP 安装和敏感值交互 #14](https://github.com/MeteorsLiu/successbox/issues/14) · [桌面集成说明](desktop/README.md) |
| macOS 启动 | [Apple Containerization 承载 gVisor #24](https://github.com/MeteorsLiu/successbox/issues/24) |

Issue 是设计决策和待验证边界的索引，不代表其中所有目标都已经实现。当前行为以协议文件、源码和各模块 README 为准。

## 产品流程

产品目标是让用户继续使用熟悉的 Agent，把环境保护、连接管理和授权交互交给 Successbox。预期使用流程如下，当前接通范围见后面的能力表：

1. 用户在宿主端选择 Claude Code 或 Codex，并保留自己的项目和配置环境。
2. Successbox 创建一个沙箱，通过受控文件视图、进程树和资源限制启动 Agent。
3. Agent 的模型请求和 MCP 调用经过宿主网关；真实上游凭据留在可信管理域，沙箱只看到受限身份或占位符。
4. 用户可以在宿主 UI 中处理 MCP 安装审批和敏感值输入，这些交互不通过 Agent 对话传递。
5. Agent 结束后，Linux Git 仓库结果可以注册为 detached worktree；沙箱关闭才进入发布流程。

Successbox 的产品原则是：宿主原配置不直接回写，Agent 不能凭自报的 sandbox ID 获得管理权限，关闭沙箱和发布结果是同一个生命周期里的不同阶段。

## 架构

下图展示设计中的模块关系。虚线表示仍需完成产品接线的沙箱到网关数据通路；当前实现和验证范围见下节。

```mermaid
flowchart LR
    User["用户"] --> UI["桌面配套工具 / CLI"]

    subgraph Host["宿主管理域"]
        UI <--> Control["沙箱服务<br/>Create / Exec / Display / Close"]
        UI <--> Session["GatewaySession<br/>宿主控制流"]
        LLM["LLM 网关<br/>CLIProxyAPI + Successbox 扩展"]
        MCP["MCP 网关<br/>Bifrost + virtual key"]
        Publish["worktree helper<br/>Linux 仓库准备与发布"]
    end

    subgraph Guest["Agent 执行域"]
        Runtime["gVisor / Apple Containerization"]
        Agent["Claude Code / Codex"]
        Runtime --> Agent
    end

    Control --> Runtime
    Agent -.->|"模型请求"| LLM
    Agent -.->|"MCP 请求"| MCP
    MCP <--> Session
    Runtime -->|"Display：RDP 字节流"| UI
    Control -->|"停止 guest 后发布"| Publish
    LLM --> Providers["模型提供方"]
    MCP --> Services["远程或本地 MCP 服务"]
```

沙箱服务负责执行隔离和生命周期，网关负责协议、上游连接和授权，worktree helper 负责 Git 仓库准备与发布，前端负责用户选择和可信交互。`SandboxService` 使用本地 Unix socket；`GatewaySession` 使用宿主 TCP HTTP/2 gRPC。两者与模型/MCP 数据入口分别管理，Agent 不通过控制端口选择账号或取得管理权限。本地 MCP 进程的隔离需要单独配置，不能仅凭 Bifrost 启动它就视为已隔离。

## 已实现能力与边界

| 能力 | 当前状态 |
| --- | --- |
| 沙箱生命周期 | Go 后端提供 `Create`、`Exec`、`Display`、`Close`；控制协议是本地 Unix socket 上的 gRPC。 |
| Linux 后端 | 基于 gVisor `runsc`，支持命令执行、TTY、资源限制和 RDP 转发。 |
| macOS 后端 | Apple Silicon 上使用 Containerization / Virtualization.framework；也可以启动承载 Linux gVisor 的 VM。 |
| 桌面传输 | `Display` 传递 RDP 原始字节，键盘和鼠标输入走同一条双向流；断开 viewer 不等于关闭沙箱。 |
| 网关 | 宿主侧可以启动 CLIProxyAPI 和 Bifrost，并通过 `GatewaySession` 管理 MCP 安装、敏感值和取消操作。 |
| Git 结果 | Linux `--worktrees` 模式在首次访问仓库时准备私有副本，`Close` 后注册 detached worktree，失败时保留结果。 |
| 非 Git 文件 | 独立目录保存和目录语义分类尚未完成；普通写入仍属于一次性临时层。 |
| 桌面 UI | 已接通 MCP 审批、敏感值输入、密钥元数据列表、删除和重连；Agent 启动、账号管理、保护开关和改动展示尚未接通。 |
| 完整产品流程 | Agent 配置适配、完整用户文件继承、macOS worktree 发布和完整桌面 E2E 尚未完成。 |

宿主网关联调已覆盖真实 CLIProxyAPI、Bifrost、Claude Code 和 Codex 的部分路径，详见[网关验证记录](gateways/README.md)。这些结果不代表完整的“用户文件环境 → 沙箱 → 模型请求 → MCP → 关闭发布”已经验收。

## 快速开始

<a id="build"></a>

### 构建

Go 模块需要 Go 1.27.1。Swift 包需要 Swift 6.2 及 macOS 26；依赖版本锁定在 `Package.swift` 和 `Package.resolved`。

```sh
make go       # 构建 sbx 客户端和 Linux 后端
make apple    # 构建并签名 Apple 后端
make check    # Go race tests
```

<a id="start-the-service"></a>

### Linux 后端

准备 gVisor `runsc` 及其 release 文件后启动服务：

```sh
./bin/successbox-gvisor-linux-amd64 \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --runsc /absolute/path/to/runsc
```

需要普通出站网络和 DNS 时，在 Linux 宿主安装 `slirp4netns` 并显式启用：

```sh
./bin/successbox-gvisor-linux-amd64 \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --runsc /absolute/path/to/runsc \
  --network slirp4netns \
  --slirp4netns /absolute/path/to/slirp4netns
```

每个沙箱使用独立的网络命名空间和 NAT/DNS 实例；`--disable-host-loopback` 阻止访问宿主 loopback。默认 `--network none` 保持离线。`slirp4netns` 不包含在后端二进制中。网关数据入口必须通过 guest 的正常网络可达，宿主管理端口不暴露给 guest。

<a id="lazy-worktrees-on-linux"></a>

启用 Linux Git worktree 流程时，再构建 runtime helper 并传入 `--worktrees`：

```sh
go build -o bin/successbox-gvisor ./cmd/successbox-gvisor
CGO_ENABLED=0 go -C runtime build -o ../bin/worktree-runtime/runsc ./cmd/runsc
CGO_ENABLED=0 go -C runtime build -o ../bin/worktree-runtime/gvisor-bin/gvisor_sentry ./cmd/sentry
CGO_ENABLED=0 go -C runtime build -o ../bin/worktree-runtime/worktrees ./cmd/worktrees
sudo ./bin/successbox-gvisor \
  --socket /var/lib/successbox/control.sock \
  --state /var/lib/successbox \
  --runsc "$(pwd)/bin/worktree-runtime/runsc" \
  --worktrees "$(pwd)/bin/worktree-runtime/worktrees"
```

该模式需要 rootful Linux 服务。Create 可以使用 `/` 作为 rootfs，并传入用户的工作目录、UID/GID；普通写入留在临时层，Git 仓库首次访问时准备私有副本。Close 停止 guest 后发布结果，原工作文件和分支不被覆盖。通过 `git -C /original/project worktree list` 查看结果；准备成本、仓库类型及并发限制见 [runtime 文档](runtime/README.md#worktree-policy)。

### macOS Apple 后端

准备 ARM64 Linux kernel 和包含 `vminitd`、`vmexec` 的 `initfs.ext4`：

```sh
./bin/successbox-apple \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --kernel /absolute/path/to/vmlinux-arm64 \
  --initfs /absolute/path/to/initfs.ext4
```

macOS gVisor 模式需要额外提供 Linux rootfs 和 runtime 目录。它运行的是 Linux 程序，不会把 macOS Mach-O 应用放进 guest；宿主项目映射和 worktree 回传也不应从“VM 能启动”推断为已完成。完整命令和图形验证见 [Apple 后端说明](#macos-上的-gvisor) 与测试脚本。

initfs 与应用 rootfs 分开。控制 socket 仅允许属主访问；`--rdp-port` 默认为 3389。macOS gVisor 模式为 Linux VM 接入 NAT vmnet 网络，内部 gVisor 沙箱使用各自的网络命名空间。

<a id="run-gvisor-on-macos"></a>

#### macOS 上的 gVisor

```sh
make apple
mkdir -p bin/linux-runtime/gvisor-bin
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/linux-runtime/successbox-gvisor ./cmd/successbox-gvisor
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C runtime build -o ../bin/linux-runtime/runsc ./cmd/runsc
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C runtime build -o ../bin/linux-runtime/gvisor-bin/gvisor_sentry ./cmd/sentry

./bin/successbox-apple \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --kernel /absolute/path/to/vmlinux-arm64 \
  --initfs /absolute/path/to/initfs.ext4 \
  --linux-rootfs /absolute/path/to/linux-rootfs \
  --runtime-directory "$(pwd)/bin/linux-runtime"
```

`--linux-rootfs` 与 `--runtime-directory` 必须成对提供。Create 的 rootfs、工作目录和挂载来源在这个模式下都是 Linux VM 内的路径。共享的 Linux rootfs 和 runtime 目录是只读的，VM 使用独立的稀疏 ext4 可写层；`main` 当前默认资源为 4 个 vCPU、1 GiB 外层容器内存和 128 MiB VM 开销，这些是配置上限，不是实测占用量。

Close 只关闭指定沙箱；向 Apple 启动器发送 SIGINT/SIGTERM 才会清理所属沙箱并停止 VM。socket 文件存在不代表服务已就绪，应确认 `gVisor backend listening` 日志。

<a id="create-execute-display-close"></a>

## 协议示例

Create 请求使用 protobuf 字段名：

```json
{
  "rootfs": "/absolute/path/to/linux-rootfs",
  "env": ["TERM=xterm-256color"],
  "workingDir": "/",
  "desktop": false
}
```

```sh
./bin/sbx --socket /absolute/path/successbox/control.sock create box.json
./bin/sbx --socket /absolute/path/successbox/control.sock exec --sandbox SANDBOX_ID --tty -- /bin/sh
./bin/sbx --socket /absolute/path/successbox/control.sock close SANDBOX_ID
```

把 JSON 保存为 `box.json`，并将 `SANDBOX_ID` 替换为 Create 返回的 ID。默认初始进程是 `/bin/sleep infinity`。

<a id="wire-behavior"></a>

RPC 的最小生命周期是：

| RPC | 数据方向 |
| --- | --- |
| `Create` | rootfs、环境、初始进程和资源选项 → sandbox ID |
| `Exec` | 启动命令、stdin、resize、signal → stdout/stderr 和最终退出码 |
| `Display` | 连接 sandbox → RDP 字节和键鼠输入 |
| `Close` | sandbox ID → 停止进程并清理运行时状态 |

协议细节、认证元数据和 GatewaySession 双向消息见 [api/README.md](api/README.md)。

使用图形应用时，guest 必须已安装并启动 RDP 服务、图形会话及 `/usr/bin/socat`；`desktop: true` 不会自动安装它们。通过 `initArgv` 指定桌面启动命令后，可以用现成 RDP 客户端连接 CLI 提供的本地转发端口：

```sh
./bin/sbx --socket /absolute/path/successbox/control.sock display --sandbox SANDBOX_ID --listen 127.0.0.1:13389
```

RDP 认证在 guest 内配置。Exec 或 Display 断线不会自动结束 guest；必须调用 Close 才会关闭沙箱并触发 worktree 发布。

## 桌面应用

桌面应用位于 `desktop/`，使用 Electron、React 和 TypeScript。它默认连接真实的 `GatewaySession`，支持 MCP 安装审批、敏感值弹窗、已保存密钥列表、删除和断线重连；尚未把账号管理、全局沙箱开关、持久运行记录和全部 Agent 启动路径接成完整产品流程。

```sh
cd desktop
npm install
npm start
npm run typecheck
npm run lint
npm run package
```

真实模式需要先在宿主启动 `sbx gateways` 并配置控制 token。模拟界面必须显式设置 `SUCCESSBOX_DEMO=1`；真实服务未连接时不会静默返回模拟成功。更多配置和安全边界见 [desktop/README.md](desktop/README.md)。

<a id="continuous-integration"></a>
<a id="verification-scope"></a>

## 验证

```sh
make check
```

CI 分别覆盖 Go race tests、Linux amd64/arm64 platform runtime、macOS Apple backend 和宿主 gateway tests。原生 VM 和 RDP 检查需要目标机器上的 kernel、initfs、rootfs、RDP 客户端及桌面依赖；编译通过或协议夹具通过，不能替代图形登录、鼠标键盘和关闭发布的完整验收。

macOS 原生集成检查在准备好资源后运行：

```sh
python3 testdata/apple-gvisor.py \
  bin/sbx bin/successbox-apple \
  /absolute/path/to/vmlinux-arm64 /absolute/path/to/initfs.ext4 \
  /absolute/path/to/linux-rootfs bin/linux-runtime

python3 testdata/apple-gvisor-display.py \
  bin/sbx bin/successbox-apple \
  /absolute/path/to/vmlinux-arm64 /absolute/path/to/initfs.ext4 \
  /absolute/path/to/desktop-rootfs bin/linux-runtime \
  /absolute/path/to/test-rdp-certificate.pem
```

已有 macOS arm64 实机记录覆盖多沙箱、文件写入隔离、二进制 IO、TTY、资源限制、RDP 协商和重连；另行验证过图形登录、1024×768 画面和 xterm 键鼠输入。上述脚本不自动验证图形登录，也不能代替 Codex Desktop 的完整验收。详细记录见 [#24](https://github.com/MeteorsLiu/successbox/issues/24)。

主要入口：

| 边界 | 代码 |
| --- | --- |
| 公开协议 | `api/sandbox/v1/sandbox.proto`、`api/gateway/v1/gateway.proto` |
| Linux 生命周期 | `cmd/successbox-gvisor/` |
| Apple 启动 | `apple/Sources/SuccessboxApple/Main.swift` |
| 网关宿主服务 | `cmd/sbx/`、`gateways/` |
| gVisor 拦截与 worktree | `runtime/internal/platform/`、`runtime/cmd/worktrees/` |
| Electron 前端 | `desktop/src/main/`、`desktop/src/preload/`、`desktop/src/renderer/` |
| 原生集成检查 | `testdata/apple-gvisor.py`、`testdata/apple-gvisor-display.py`、`testdata/gvisor-worktrees.sh` |

## 设计原则

- 宿主原文件不直接回写；结果通过既有 tmpfs、私有仓库和 Close 发布流程处理。
- Agent 使用受限网关身份，真实上游 API key 和 OAuth 留在宿主网关。
- `Display` 是 RDP 双向字节流，不是截图轮询或 JSON 鼠标事件。
- 关闭 viewer、关闭 sandbox、取消网关 pending 操作和撤销凭据是不同动作。
- 尚未接通的账号、配置继承、非 Git 成果导出和完整桌面流程会明确显示为缺口，不返回模拟成功。
