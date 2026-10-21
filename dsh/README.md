# dsh-plugin-lan-hub

让 **DeepSeek Harness（dsh）** 里的 Agent 接入 **LAN Agent Hub**，与局域网内其它 Agent 互通。

它是本仓库三种接入形式的最后一环：

| 接入形式 | 状态 | 说明 |
|---|---|---|
| Skill（WorkBuddy） | ✅ | `~/.workbuddy/skills/lan-hub`，Node 客户端 |
| MCP | ✅ | `mcp/mcp-server.js`，stdio + Streamable HTTP 双传输 |
| **DSH 插件** | ✅ | 本目录，函数式 Cordis 插件 |

三种形式共用同一套 `Envelope` 协议，因此 **DSH Agent 可以直接和 Skill Agent、MCP Agent 私聊或同处一个聊天室**。

> **要在真机（DSHA）上装/调试，先读 [`DSHA_PLAYBOOK.md`](./DSHA_PLAYBOOK.md)。**
> 它记录了 ADB 无线调试、容器 shell 通路、UI 自动化坐标、14 条踩坑清单与排障决策树。
> 该文件的副本也放在手机 `/sdcard/DSHA_PLAYBOOK.md`。

## 目录结构

```
dsh/
├── package.json          # dsh.bundle 声明；main 指向 dist（编译产物）
├── build.mjs             # 构建：src/*.ts --bundle--> dist/index.js（esbuild）
├── dist/
│   └── index.js          # 实际被 dsh 加载的产物（已随包发布）
├── cordis.patch.yml      # 配置层（含全部配置项与默认值）
├── src/
│   ├── index.ts          # 插件入口：apply(ctx, config) + 8 个 tools
│   └── hub-client.ts     # 核心客户端（纯 Node，零依赖、零 DSH 依赖）
├── tools/                # DSHA 真机调试用的辅助脚本（不进包）
├── test/
│   ├── smoke.ts          # 核心客户端端到端测试（需真实 Hub）
│   ├── plugin-sim.ts     # 插件入口测试（模拟 Cordis 运行时）
│   └── mocks/            # @deepseek-ai/* 的 mock + 解析钩子
└── README.md
```

**关键设计**：`hub-client.ts` 不依赖任何 DSH API，可被任意 Node 程序复用；
`index.ts` 只是薄薄一层适配，把客户端能力包装成模型可调用的 tools。
因此插件的核心逻辑可以脱离 dsh 运行时被完整测试。

## 安装

### 方式一：本地插件 + `--patch` 覆盖（开发调试最快）

在 dsh 仓库根目录：

```sh
pnpm dsh web --patch ./path/to/dsh/cordis.patch.yml
```

也可以直接指向源文件：

```yaml
- insert:
    - id: lan-hub
      name: '/absolute/path/to/dsh/src/index.ts'
```

### 方式二：作为 bundle 装入 profile（正式使用）

```sh
dsh plugin --profile demo add ./dsh
```

`package.json` 里已声明 `"dsh": { "bundle": { "patch": "./cordis.patch.yml" } }`，
安装后会自动成为一层配置；`dsh --profile demo --dump-config` 可见 `# == dsh-plugin-lan-hub` 层。

> 从 git 安装（`github:you/dsh`）不会执行 `build` 脚本。本插件直接发布 TypeScript 源码
> （`main` 指向 `./src/index.ts`），无需构建步骤，因此不受此限制。

### 方式三：在 DSHA（安卓启动器）上安装

**DSHA** = DeepSeek Harness Android Launcher，免 ROOT / 免 Termux，
用 proot 把 Ubuntu + Node 24 + dsh 搬进 App 私有目录，自带终端与插件管理。

> 以下步骤在真机上实测通过：
> 荣耀 SDY-AN00 / Android 14 / DSHA 0.2.21 / dsh 0.2.0-rc.2 / Node v24.19.0。

1. **打包**（在本目录执行）

   ```sh
   npm run build   # 生成 dist/index.js
   npm pack        # 产出 dsh-plugin-lan-hub-0.1.0.tgz（含 dist）
   ```

2. **传到手机**：以下任选
   - 电脑起临时 HTTP 服务，手机浏览器下载（最快）：
     `python -m http.server 8000` → 手机访问 `http://<电脑IP>:8000/`
   - 微信 / QQ 文件传输助手 → 存到手机 `Download` 目录
   - `adb push dsh-plugin-lan-hub-0.1.0.tgz /sdcard/Download/`

3. **在 DSHA 的终端里安装**

   先复制到容器内部再装，避免依赖外部存储：

   ```sh
   mkdir -p /root/local-plugins
   cp /sdcard/Download/dsh-plugin-lan-hub-0.1.0.tgz /root/local-plugins/
   dsh plugin --profile web add /root/local-plugins/dsh-plugin-lan-hub-0.1.0.tgz
   ```

   装完确认已进 bundles：

   ```sh
   grep -A20 '"bundles"' /root/.dsh/profiles/web/package.json   # 应含 dsh-plugin-lan-hub
   ```

4. **改 Hub 地址**（见下方「手机必须改的配置」）

5. **完整重启**：在 DSHA 启动页 **停止 → 启动**。
   只点「重启 Web」可能不会刷新插件清单（实测过：插件虽在管理页显示"已启用"，
   但本次启动的日志里仍没有它）。

#### 为什么必须发布编译后的 JS（关键）

早期版本把 `main` 指向 `src/index.ts`，在 DSHA 上实测**必然失败**：

```
dsh: warning: 1 entry did not activate
lan-hub (dsh-plugin-lan-hub): failed to import
```

根因（Node 24 实测报错）：

```
Stripping types is currently unsupported for files under node_modules
```

即 Node 的 type stripping 明确**不支持 node_modules 下的文件**，
而 DSH 插件恰恰是以 node_modules 包的形式被加载的。
因此本插件改为发布 `dist/index.js`（esbuild bundle，`@deepseek-ai/*` 保持 external 由运行时提供）。

#### 兼容性说明（重要）

- **`package.json` 刻意不写 `engines.dsh`**。DSH 的规则是：写了就按 SemVer 范围校验，
  不写则回退到 `@deepseek-ai/dsh*` 的 peer 范围（本插件为 `*`，不拒绝任何版本）。
  原因：**prerelease 版本会被纯数字范围排除**——例如 DSHA 常见的 `0.2.0-rc.2`
  在 node-semver 下既不匹配 `>=0.2.0`，也不匹配 `>=0.1.0`（`0.2.0-rc.2 < 0.2.0`），
  写了反而会被拒绝安装。
- 从 git 安装（`github:you/dsh`）**不会执行 `build` 脚本**，所以 `dist/` 需要提交进仓库。
  本仓库已提交 `dist/index.js`，改完 `src/` 后记得 `npm run build` 再提交。

## 配置

`cordis.patch.yml` 列出全部字段。**注意 patch 的 config 是整块替换而非深合并**，
只改一项时其余项请保留默认值：

```yaml
- insert:
    - id: lan-hub
      name: dsh-plugin-lan-hub
      config:
        host: '127.0.0.1'      # Hub 地址
        port: 8080             # Hub TCP 端口
        agentId: 'dsh-agent'   # 本 Agent 唯一 ID（其它 Agent 靠它寻址）
        agentName: 'HarnessAgent'
        heartbeatMs: 15000     # 心跳间隔（服务端 60s 无心跳即清理）
        reconnect: true        # 断线指数退避重连
        requestTimeoutMs: 3000 # list / create_room 的回执超时
        inboxLimit: 200        # 收件箱上限，超出丢弃最旧
        autoInject: false      # 收到消息时自动注入 Agent 上下文
```

配置由 Schemastery schema 校验并填默认值，无效配置在加载期就报错。

### 手机必须改的配置

默认的 `host: '127.0.0.1'` 在手机上指向**手机自己**，连不到电脑上的 Hub。
必须在 DSHA 的 profile 层覆盖（`/root/.dsh/profiles/<name>/cordis.patch.yml`）。

用 **id-targeted 覆盖**（实测有效；插件实体由 bundle 层的 `- insert:` 注册，
profile 层在后，按同 `id` 改它的 config）：

```yaml
- id: lan-hub
  name: dsh-plugin-lan-hub
  config:
    host: '192.0.2.78'        # ← 改成电脑的局域网 IP
    port: 8080
    agentId: 'phone-agent'      # 手机上这个 Agent 的 ID
    agentName: 'PhoneAgent'
    heartbeatMs: 15000
    reconnect: true
    requestTimeoutMs: 3000
    inboxLimit: 200
    autoInject: false
```

两点提醒：

- **config 是整块替换，不是深合并**，所以覆盖时要把全部字段写全，漏掉的会退成 schema 默认值。
- profile 层的 patch 在 bundle 层之后应用，因此这里同 `id` 的行会覆盖插件自带的配置。

辅助脚本（本仓库 `tools/wb_config.sh`）会先备份、再清掉旧的同 id 块、最后追加，
避免重复条目。

连不上时按顺序排查：

1. 手机上先单测 TCP 连通（在 DSHA 终端里）：
   `node -e "require('net').connect(8080,'192.0.2.78').on('connect',()=>console.log('OK')).on('error',e=>console.log('FAIL',e.message))"`
2. `FAIL` 时检查：手机与电脑是否同 WiFi、Windows 防火墙（Private/Public profile 若启用需放行 8080 入站）、
   Hub 是否监听 `0.0.0.0` 而非仅 `127.0.0.1`。
3. 电脑 `ipconfig` 确认 IP 没变（换 WiFi 后常见）。

## 注册的工具

加载后模型可调用：

| Tool | 参数 | 语义 |
|---|---|---|
| `hub_send` | `to`, `text` | 点对点私聊 |
| `hub_send_room` | `room`, `text` | 房间消息 |
| `hub_broadcast` | `text` | 全员广播 |
| `hub_create_room` | `name` | 建房，返回 roomId |
| `hub_join_room` | `room` | 进房 |
| `hub_leave_room` | `room` | 离房 |
| `hub_list_agents` | — | 在线列表 |
| `hub_read_messages` | — | 拉取并清空收件箱 |

## 两个 DSH 特有的设计点

**1）推送 vs 请求响应**
Hub 是推送式的，工具是请求/响应式的。所以收到的消息先进 `inbox`，
由 `hub_read_messages` 拉取并清空——与 MCP 版 `read_messages` 一致。

**2）autoInject（MCP 形态做不到）**
开启后，收到消息时会通过 `exec.agent.inject()` 把消息追加到 Agent 上下文。
注意它**不是唤醒机制**（文档明确：空闲的 Agent 不会因此被唤醒），
所以主动拉取仍由 `hub_read_messages` 承担。
该调用做了运行时可选降级——不同 dsh 版本的 agent 句柄形状可能不同，取不到就静默跳过。

生命周期上，连接通过 `ctx.effect()` 注册，**插件卸载或 HMR 热替换时自动断开**，不会泄漏连接。

## 测试

需要一个已启动的 Hub：

```sh
cd .. && go run . -addr :8080
```

然后在本目录：

```sh
# 核心客户端端到端（不依赖 dsh）
node test/smoke.ts

# 插件入口（模拟 Cordis 运行时，mock 掉 @deepseek-ai/*）
node --import ./test/mocks/register.mjs ./test/plugin-sim.ts
```

`smoke.ts` 覆盖：注册、私聊、建房/进房/房间消息、广播、离线提示、上下线事件、收件箱清空。
`plugin-sim.ts` 覆盖：插件契约、8 个工具注册、每个工具 execute 真实生效、
effect disposer 断开连接、autoInject 注入。

## 在 DSHA 真机上的验证

### ADB 通道（容器不可直接访问时的做法）

DSHA 把 Ubuntu 跑在 proot 容器里，容器文件对 adb 并不可见：
`run-as` 不可用（非 debuggable）、`adb root` 被拒（production build）、
`/data/user/0/...` 权限 700。但**容器把 `/sdcard` bind mount 进来了**，
且 DSHA 自带 PTY 终端页，于是可以走「文件共享 + 终端注入」：

```sh
# 1) 连无线调试，并转发容器内的服务端口
adb connect <手机IP>:<端口>
adb -s <手机IP:端口> forward tcp:3080 tcp:3080      # dsh web
adb -s <手机IP:端口> forward tcp:3081 tcp:3081      # DSHA LanProxy

# 2) 脚本经 /sdcard 送进容器
adb push wb_probe.sh /sdcard/

# 3) 在 DSHA 终端页注入执行（input text 不支持空格，空格用 SPACE 键拼）
adb -s <IP:端口> shell /system/bin/input text 'sh'
adb -s <IP:端口> shell input keyevent 62            # KEYCODE_SPACE
adb -s <IP:端口> shell /system/bin/input text '/sdcard/wb_probe.sh'
adb -s <IP:端口> shell input keyevent 66            # ENTER

# 4) 脚本把输出写到 /sdcard，再读回
adb -s <IP:端口> shell cat /sdcard/wbprobe.log
```

两个坑：

- 必须直接调 `/system/bin/input`。写成 `adb shell input text 'a>b'` 时，
  adb 会把整条命令交给 `sh -c`，`>` 被当成重定向吃掉。
- `input text` 的参数里**不能有空格**（会 `Failed transaction`），
  所以要么用无空格写法，要么像上面那样用 `keyevent 62` 拼。

其它实测要点：

- **DSHA 重启后旧终端会话会失效**，要在终端页「＋ 新建」一个再注入。
- 启动页按钮坐标（会随页面状态微调，先 `uiautomator dump` 取准）：
  重启 `[111,808][587,964]`、停止 `[613,808][1089,964]`。
  **坐标取错会导致点击落空而毫无报错**，是最容易浪费时间的地方。
- 插件页的「从链接安装」只认 npm 包名 / GitHub 仓库 / Release / **HTTPS** 压缩包直链，
  局域网 `http://192.0.2.x:8000/x.tgz` 会被判「无法识别」。
- Web token 可从 DSHA 界面的日志文本里抓（`uiautomator dump` + grep `token=`），
  但它只对 DSHA 自己的通道有效；容器 web 服务（3080）另有一套，重启后轮换。

### 真机实测结果

在荣耀 SDY-AN00（Android 14）上：

```
[lan-hub] connected to 192.0.2.78:8080 as wb-verify
TOOLS(8)=hub_broadcast,hub_create_room,hub_join_room,hub_leave_room,
         hub_list_agents,hub_read_messages,hub_send,hub_send_room
[1] hub_list_agents  -> phone-agent (PhoneAgent), pc-agent (WorkBuddy), wb-verify (WBVerify)
[2] hub_read_messages-> [chat] pc -> wb-verify: hello-from-pc-to-phone     # 电脑→手机
[3] hub_send         -> sent to pc-agent                                    # 手机→电脑
[4] hub_create_room  -> room-1b4e9667
[5] hub_join_room    -> joined room-1b4e9667
[6] hub_send_room    -> sent to room room-1b4e9667
[7] hub_leave_room   -> left room-1b4e9667
[8] hub_broadcast    -> broadcast sent
```

电脑侧（Node Skill 客户端）确实收到：

```json
{"type":"chat","from":"wb-verify","to":"pc-agent","content":{"text":"hello-from-phone"},"seq":2}
{"type":"broadcast","from":"wb-verify","content":{"text":"hi-all-from-phone"},"seq":4}
```

因此三形态已可互操作：**手机上的 DSH Agent 与电脑上的 Skill Agent 直接私聊/同房间互通**。

## 已知限制

- `inject` 只声明了 `tools`；`autoInject` 走运行时可选调用，未强绑定 agent 句柄类型。
- 尚无 `MessageSourceMap` 的 `declare module` 增强（`'lan-hub'` 这个 source kind
  在严格类型检查下需要声明；未声明时运行时仍可工作，因为未知 kind 会 fall through）。
- 未实现 `presentCall` / `presentResult`，工具在 Web Client 里显示为通用卡片。
