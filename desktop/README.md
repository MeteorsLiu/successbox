# Successbox 桌面应用

Claude Code / Codex 的配套工具：管理账号与 MCP，打开“沙箱保护”后，Agent 的改动另存、密钥不进 Agent。产品设计见 [#16](https://github.com/MeteorsLiu/successbox/issues/16)。

技术栈：Electron 45、React 19、TypeScript、Vite，用 Electron Forge 运行和打包。

**当前状态**：界面和交互完整，后端是内置的**模拟实现**。沙箱、网关的接口确定后，只需替换 `src/main/backend/` 中的实现，界面不用改。

## 运行

```sh
cd desktop
npm install
npm start            # 开发模式；第一次运行会下载 Electron（约 100 MB）
npm run typecheck    # 类型检查
npm run lint         # 代码检查与格式检查（npm run lint:fix 自动修复）
npm run make         # 打包；在 Linux 上生成 deb、rpm 和 zip
```

演示各种情况：打开“设置”，页面底部的“模拟”区域可以触发启动 Agent、需要密钥、Agent 退出、请求安装 MCP、沙箱服务停止等事件。

## 目录结构

```text
desktop/
├── assets/                 应用图标和托盘图标
├── src/
│   ├── shared/             三个进程共用的类型
│   │   ├── types.ts        界面能看到的数据（AppState）和能调用的操作（BackendApi）
│   │   └── agents.ts       Agent 名称和命令
│   ├── main/               主进程（Node.js，能访问系统）
│   │   ├── main.ts         入口：创建后端、窗口、托盘，处理系统通知
│   │   ├── backend/
│   │   │   ├── backend.ts  后端接口
│   │   │   └── mock.ts     模拟实现
│   │   ├── ipc.ts          接收界面的调用，把状态推送给所有窗口
│   │   ├── windows.ts      主窗口和可信小窗
│   │   ├── tray.ts         托盘菜单
│   │   ├── importer.ts     只读扫描现有的 Claude Code / Codex 配置（真实实现）
│   │   └── store.ts        本地偏好（不保存任何凭据）
│   ├── preload/preload.ts  把 window.successbox 暴露给界面
│   └── renderer/           界面（React）
│       ├── main.tsx        入口：#/trusted 显示可信小窗，否则显示主界面
│       ├── App.tsx         顶栏和页面切换
│       ├── api.ts          window.successbox 的类型
│       ├── useAppState.ts  读取最新状态的 hook
│       ├── pages/          Agent、MCP、改动、设置四个页面，各自的对话框写在同一文件里
│       ├── components/     开关、对话框、提示条、首次导入
│       ├── trusted/        可信小窗的内容
│       ├── presets.ts      账号和 MCP 的预设
│       └── styles.css      全部样式
```

## 数据怎么流动

状态只保存在主进程里，界面不自己维护业务状态：

```text
界面组件 ──api.xxx()──► preload ──IPC──► ipc.ts ──► Backend（现在是 mock.ts）
    ▲                                                      │ 状态变化
    └──── useAppState() ◄── preload ◄── IPC 推送新状态 ◄────┘
```

- 读：组件调用 `useAppState()`，拿到最新的 `AppState`。
- 写：组件调用 `api` 上的方法，例如 `api.setProtection('claude', true)`。主进程改完状态后，会把新状态推送给所有窗口，界面自动刷新。
- 组件内部只保存表单输入、对话框开关这类临时状态。

## 换成真实后端

1. 在 `src/main/backend/` 中新建一个类，实现 `backend.ts` 里的 `Backend` 接口。接口覆盖 #16 第 8 节列出的全部能力。
2. 在 `src/main/main.ts` 中把 `new MockBackend(...)` 换成它。
3. `AppState.simulated` 返回 `false`，设置页的“模拟”区域会自动隐藏。

## 安全约束

- 界面进程开启 `contextIsolation` 和 `sandbox`，不能直接使用 Node.js，只能调用 preload 暴露的方法。
- 回答敏感值和 MCP 安装请求（`answerSecret`、`answerInstall`）只接受来自可信小窗的调用，主窗口调用会被拒绝（`src/main/ipc.ts`）。
- 敏感值只经过主进程交给后端，不进入状态快照，也不写入任何文件。
- 可信小窗被关闭时，视为拒绝当前请求；队列中还有请求时，会重新打开。
- 打包版本加入严格的 Content-Security-Policy；所有窗口禁止跳转到外部页面和打开新窗口。
- 打包时通过 Electron Fuses 关闭 `RunAsNode` 等调试入口。

## 加一个页面

1. 在 `src/shared/types.ts` 的 `Page` 中加一个值。
2. 在 `src/renderer/pages/` 中新建组件，参数是 `state: AppState`。
3. 在 `src/renderer/App.tsx` 的顶栏加按钮，并在 `<main>` 中按 `page` 渲染它。
4. 需要新的后端操作时：在 `BackendApi` 中加方法，在 `API_METHODS` 中登记，然后在 `mock.ts` 中实现。

## 已知限制

- 后端全部是模拟数据；“在终端中打开”目前只模拟启动，不会打开真实终端。
- 命令行集成（`~/.local/bin` 中的包装命令）还没有实现，见 #16 第 7 节。
- 已在 macOS 上运行和打包验证；Linux 上的托盘依赖 AppIndicator（GNOME 需要安装对应扩展），系统通知和打包结果需要在 Linux 上验证。
- 除“是否看过首次导入”外，状态不会持久化，重启后恢复为模拟的初始数据。
