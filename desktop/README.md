# Successbox 桌面应用

Electron 45、React 19、TypeScript 和 Electron Forge。产品设计见 [#16](https://github.com/MeteorsLiu/successbox/issues/16)。

默认连接真实的 `GatewaySession` 控制服务，支持 MCP 安装审批、敏感值弹窗、已保存密钥的列表和删除，以及断线重连。支持通过现有 `sbx` 在系统终端中启动沙箱程序，显示本次运行和退出清理状态。账号管理、全局保护开关、持久运行记录和改动展示尚未接入。沙箱到模型网关的网络仍未接通，因此尚不能完成 Agent 对话流程。

## 运行

```sh
cd desktop
npm install
npm start
npm run typecheck
npm run lint
npm run package
```

先按 [网关运行说明](../gateways/README.md) 启动宿主上的 `sbx gateways`，启用 `--control-listen` 和三个独立的控制 token。桌面应用不会自行启动或停止这些服务。

在宿主上创建仅当前用户可读的连接文件，例如 `gateway.json`：

```json
{
  "address": "127.0.0.1:9090",
  "token": "与 --control-frontend-token 相同的值",
  "sandboxIds": ["A", "B"]
}
```

```sh
chmod 600 /absolute/path/to/gateway.json
SUCCESSBOX_GATEWAY_CONFIG=/absolute/path/to/gateway.json npm start
```

`address` 是本机 TCP HTTP/2 控制地址，不是 LLM/MCP 的 HTTP 数据地址，也不是沙箱 socket。本轮使用本机明文 gRPC，不提供远程 TLS 配置。`sandboxIds` 对应网关已有的沙箱绑定，用来读取启动时的密钥列表；新收到的请求或事件还会加入其所属范围。现有协议没有列出所有沙箱的消息。未设置环境变量时读取 Electron `userData` 目录中的 `gateway.json`；配置缺失或无效时保持未连接状态。

如需查看原来的模拟界面，显式运行 `SUCCESSBOX_DEMO=1 npm start`，再使用设置页的模拟按钮。模拟模式不连接真实网关。

## 沙箱终端

按仓库 [运行说明](../README.md#start-the-service) 启动后端后，在同一个连接配置中添加 `sandbox`。前端复用既有 `launchInTerminal(agent)`，不新增 RPC 或 preload 方法：

```json
{
  "sandbox": {
    "client": "/absolute/path/to/sbx",
    "socket": "/absolute/path/to/control.sock",
    "create": "/absolute/path/to/box.json",
    "terminal": ["/usr/bin/open", "-a", "Terminal"],
    "commands": {
      "codex": ["codex"],
      "claude": ["claude"]
    },
    "environment": {
      "codex": [
        "HTTP_PROXY=http://gateway.internal:8080",
        "HTTPS_PROXY=http://gateway.internal:8080"
      ],
      "claude": [
        "ANTHROPIC_BASE_URL=http://gateway.internal:8080",
        "ANTHROPIC_API_KEY=successbox-client"
      ]
    }
  }
}
```

`terminal` 是宿主命令及参数数组，应用在最后追加私有、可执行的 `.command` 文件。上例是已实测的 macOS Terminal 调用；Linux 需配置能执行该文件的已安装终端启动器。`commands` 可省略，默认使用 `codex`、`claude`。命令与参数逐项引用，不作为未转义的 shell 输入。

`client`、`socket`、`create` 必须是宿主绝对路径。`create` 内容是原有 Create 请求 JSON，原样交给 `sbx`。rootfs、挂载和工作目录由后端解释：macOS gVisor 模式下指 Linux VM 内的路径，不能当成 macOS 项目路径。运行环境必须已包含待执行的 Linux 程序及 PATH。启动终端不要求网关控制流已连接，也不会自动创建账号或网关绑定。

`environment` 按 Agent 将网关数据入口注入 Create 的 `env`。它不是 GatewaySession 控制地址；控制 token 不会自动注入 Agent。每项必须是 `KEY=VALUE`，同名变量覆盖 `create` 文件中的值。真实配置文件应保持 0600，输入值不会进入 renderer 状态快照。网关入口必须是 guest 能通过网络访问的地址；`127.0.0.1` 仍指向 guest 自身。

每次点击创建独立沙箱。终端脚本执行 Create → Exec（PTY）→ Close，桌面观察该次运行的私有状态文件。退出桌面 UI 不会关闭正在使用的终端，重新打开 UI 暂不恢复原运行列表。程序退出或终端在 Exec 期间收到 HUP 后，脚本负责 Close；成功时界面移除记录，失败则保留沙箱 ID 和日志路径。启动失败不会退回宿主执行。

脚本将客户端作为后台子进程运行，显式从 `/dev/tty` 继承输入，用 shell 内置 `wait` 等待。直接等待前台命令会延迟 HUP trap；内置 `wait` 可以被信号中断，及时执行 Close，并在关闭成功后等待客户端退出。

### 验证终端

本轮在 macOS arm64 打包应用 → 系统 Terminal/测试 PTY → Apple VM 内的真实 gVisor 上验证。两个 Agent 入口使用合成 shell 程序验证接线，不代表 Claude/Codex 已完成模型请求。覆盖正常与非零退出、引号参数、终端输入、Ctrl-C 和关闭 PTY。关闭后再次 Exec 对应 ID 返回 NotFound，宿主子进程也已退出。Linux 桌面启动器尚未实测。

`test/terminal-driver.py` 可替代系统终端启动器。将 `sandbox.terminal` 配置为以下数组，在待测试 Agent 的命令中使用 `/bin/sh -c` 加下表内容，再从前端点击启动：

```json
[
  "/absolute/path/to/python3",
  "/absolute/path/to/desktop/test/terminal-driver.py",
  "input",
  "/tmp/terminal-output.log"
]
```

| 模式      | 测试命令                                                                                       | 预期退出码 |
| --------- | ---------------------------------------------------------------------------------------------- | ---------- | ---------------------------- | --- |
| input     | `stty -echo; printf 'TERMINAL_READY\n'; IFS= read -r line; test "$line" = 'hello from desktop' |            | exit 8; printf 'INPUT_OK\n'` | 0   |
| interrupt | `trap 'exit 130' INT; printf 'TERMINAL_READY\n'; while :; do sleep 1; done`                    | 130        |
| hangup    | 与 interrupt 相同，驱动关闭 PTY                                                                | 129        |

驱动等待 `TERMINAL_READY`，执行对应操作并断言最终退出码；还需检查前端移除运行记录、后端 ID 已不能执行。本测试不读取真实账号。

## 数据与操作

```text
React → preload → Electron IPC → GatewayBackend → GatewaySession
  ↑                                  │
  └──── AppState 元数据快照 ────────────┘
```

`src/main/main.ts` 中的私有 `GatewayBackend` 实现既有 `Backend` 接口。preload 的函数签名和 Go protobuf/RPC 没有增加。主进程复用一条双向流，按 `correlation_id` 对应操作结果，按服务端请求 ID 去重弹窗。

- 密钥列表只包含名称、字段和所属沙箱，不读取已保存的密钥值。A、B 使用相同 MCP ID 和字段时分别展示，删除也分别生效。
- 可信窗口提交敏感值后，网关按现有协议持久保存并唤醒等待中的调用。拒绝或关闭窗口会取消请求；没有单次使用且不保存的选项。
- 安装审批批准或拒绝该任务请求的完整工具范围。批准后继续接收凭据弹窗，不等待安装完成才读取下一条消息。
- 连接中断后清空待处理弹窗并将未确认操作报告为失败，保留上次读取的列表但禁止删除。重连后打开会话、重放当前待处理请求并刷新已知范围的列表。已提交操作不会盲目重发；服务重启前未完成的请求需要调用方重新发起。

## 边界

- 窗口开启 `contextIsolation` 和 `sandbox`，不能直接使用 Node.js。`answerSecret` 和 `answerInstall` 只接受可信窗口调用，主窗口调用会被拒绝。
- 控制 token 仅留在主进程。输入值经主进程交给宿主网关，不进入状态快照，也不写入桌面偏好文件。网关将提交值存入宿主 SQLite。
- 真实模式暂不提供手动添加/编辑 MCP、主动新增/修改密钥或 Agent 分配。未接入操作不会返回模拟成功。
- 真实密钥由网关持久化，桌面重启会重新读取已配置范围。模拟模式的业务状态仍不持久化。
- 打包带入原有 `gateway.proto`，开发和打包应用使用相同线协议。界面布局沿用现有前端，不改变后端协议。

## 目录

- `src/shared/`：状态类型、既有 `BackendApi` 与 Agent 名称。
- `src/main/main.ts`：网关适配、窗口和托盘的组装。
- `src/main/backend/`：既有后端接口及显式 demo 实现。
- `src/main/ipc.ts`、`src/preload/preload.ts`：窗口调用与元数据快照推送。
- `src/renderer/`：主界面、可信窗口及页面。
- `test/`：真实服务联调夹具和协议检查驱动。

## 真实网关联调

夹具使用本机原生 CLIProxyAPI、Bifrost 和 Go 控制服务，MCP 上游及所有凭据均为合成数据，不读取真实账号。先按网关说明安装原生二进制及账号绑定插件；插件需位于 CLIProxyAPI 版本目录的同级 `plugins/<os>/<arch>/successbox.dylib`（Linux 为 `.so`）。从仓库根目录运行：

```sh
go build -o .cache/desktop-sbx ./cmd/sbx
python3 desktop/test/gateway-fixture.py .cache/desktop-sbx /absolute/path/to/cli-proxy-api /absolute/path/to/bifrost-http
```

夹具打印临时目录，并保持前台运行。在另一个终端将 `fixture` 设为该目录，使用其中的 `gateway.json` 启动桌面应用：

```sh
fixture=/tmp/successbox-desktop-e2e-实际目录
cd desktop
SUCCESSBOX_GATEWAY_CONFIG="$fixture/gateway.json" npm start
```

再从仓库根目录依次验证：

1. `node desktop/test/gateway-driver.cjs "$fixture" seed`：A、B 使用同一 MCP ID 和字段；界面只删除任务 A 的条目。
2. `node desktop/test/gateway-driver.cjs "$fixture" resolve`：在可信窗口填写 `fixture-prompt-secret`，驱动检查等待中的真实 RPC 收到该值。
3. `node desktop/test/gateway-driver.cjs "$fixture" cancel`：点击拒绝，驱动检查后端返回 `Aborted`。
4. `node desktop/test/gateway-driver.cjs "$fixture" install`：批准安装，然后填写 `fixture-installed-secret`。
5. `node desktop/test/gateway-driver.cjs "$fixture" verify`：检查 A 的删除没有影响 B，真实 MCP 调用将占位符替换为合成值，并且 B 看不到 A 安装的工具。

本轮在 macOS arm64 的打包应用验证了以上路径，也验证了主窗口不能提交可信回答、快照不含凭据，以及断开前端 TCP 连接后自动重连并只恢复一个待处理弹窗。未用此结果代替 Linux 桌面环境验证。

## Linux 运行说明

原有模拟前端已在 Ubuntu 24.04（x86_64）虚拟显示中验证开发版、打包和 deb 安装。托盘和系统通知需要真实桌面会话，GNOME 托盘依赖 AppIndicator 扩展。真实网关前端本轮验证平台为 macOS arm64。

Ubuntu 24.04 默认限制非特权用户命名空间。如果开发模式报告 `The SUID sandbox helper binary was found, but is not configured correctly`，为 Electron 配置其 sandbox helper：

```sh
sudo chown root:root node_modules/electron/dist/chrome-sandbox
sudo chmod 4755 node_modules/electron/dist/chrome-sandbox
```

deb 安装包会设置这些权限。Linux 打包需要 `zip`、`fakeroot`（deb）；rpm 应在相应 RPM 系发行版构建。
