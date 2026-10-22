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
├── package.json          # dsh.bundle 声明；main 指向 dist（编译产物）；bin: lanhub-policy
├── build.mjs             # 构建：src/*.ts + CLI --bundle--> dist（esbuild，默认 minify）
├── dist/
│   ├── index.js          # 实际被 dsh 加载的产物（已随包发布）
│   ├── lanhub-policy.mjs # 策略 CLI 单文件（bin: lanhub-policy）
│   └── lanhub-update.mjs # 更新通道 CLI 单文件（bin: lanhub-update）
├── cordis.patch.yml      # 配置层（含全部配置项与默认值）
├── src/                  # 不进发布包（已编译进 dist）
│   ├── index.ts          # 插件入口：apply(ctx, config) + 10 个 tools
│   ├── hub-client.ts     # 核心客户端（纯 Node，零依赖、零 DSH 依赖）
│   └── policy.mjs        # 策略规范：字段/类型/默认值/钳制，插件与 CLI 共用
├── tools/                # lanhub-policy.mjs 源码（不进包）+ wb_*.sh（进包）
├── test/
│   ├── smoke.ts          # 核心客户端端到端测试（需真实 Hub）
│   ├── plugin-sim.ts     # 插件入口测试（模拟 Cordis 运行时）
│   ├── update-sim.ts     # 更新通道端到端测试（本地假分发源 + 真实调用 CLI）
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
        # ---------- connection（冷：变更会重建连接）----------
        host: '127.0.0.1'      # Hub 地址（填 auto 启用局域网自动发现）
        port: 8080             # Hub TCP 端口
        agentId: 'dsh-agent'   # 本 Agent 唯一 ID（其它 Agent 靠它寻址）
        agentName: 'HarnessAgent'
        heartbeatMs: 15000     # 心跳间隔（服务端 60s 无心跳即清理）
        reconnect: true        # 断线指数退避重连
        requestTimeoutMs: 3000 # list / create_room 的回执超时
        inboxLimit: 200        # 收件箱上限，超出丢弃最旧
        # ---------- policy（热：原地生效，不重连）----------
        autoRespond: true      # 收到他人消息自动唤醒 Agent 生成回复
        autoInject: false      # 只注入上下文、不唤醒
        wakeRoom: 'mention'    # 房间唤醒：mention（需 @我）| all | off
        batchMs: 800           # 收信合并窗口：窗口内多条合并成一次唤醒
        maxWaitMs: 3000        # 合并窗口硬上限，持续来消息也不会无限推迟
        maxAutoTurns: 6        # 同一对话连续自动往返上限（0=不限）
        maxHop: 3              # 允许的最大转发层数（0=不限）
        perPeerPerMin: 8       # 每对端每分钟唤醒上限
        globalPerMin: 30       # 全局每分钟唤醒上限
        muteNotice: true       # 被护栏静音时注入一条可见提示
        # ---------- policy file（热更新通道）----------
        policyFile: ''         # 非空即轮询该 JSON；脚本写它即可热改策略
        policyPollMs: 1500     # 轮询间隔（毫秒）
```

**配置分两层，语义完全不同**（这也是热更新只对 policy 生效的原因）：

| 层 | 字段 | 变更后 |
|---|---|---|
| connection | `host/port/agentId/agentName/heartbeatMs/reconnect/requestTimeoutMs/inboxLimit/autoDiscover/discoverPort/fallbackHost` | 插件重启 + 重连（会重新注册，进程内收件箱清空） |
| policy | `autoRespond/autoInject/wakeRoom/batchMs/maxWaitMs/maxAutoTurns/maxHop/perPeerPerMin/globalPerMin/muteNotice` | 原地生效，**不断连、不重启** |
| 热更新通道 | `policyFile/policyPollMs` | 只重启轮询器，不重连 |

配置由 Schemastery schema 校验并填默认值，无效配置在加载期就报错；
热更新走 Cordis 的 `internal/update` waterfall——**先校验再切换**，坏配置进不来。

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
    autoRespond: true
    autoInject: false
    wakeRoom: 'mention'
    batchMs: 800
    maxWaitMs: 3000
    maxAutoTurns: 6
    maxHop: 3
    perPeerPerMin: 8
    globalPerMin: 30
    muteNotice: true
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

## 热更新策略：脚本写文件，插件轮询

宿主的配置重载并非所有环境都会触发（已装包里没有 yml watcher），proot 容器里的
inotify 对 bind mount 也常不可靠。所以热更新走**脚本 + JSON 文件**这条最短路径，
不依赖宿主行为：

```sh
# 仓库内开发：跑源码
node tools/lanhub-policy.mjs set autoRespond=false

# 装成包之后：走 bin 入口（等价，无需关心路径）
npx lanhub-policy set autoRespond=false

# 房间消息全部唤醒 + 放宽往返上限
node tools/lanhub-policy.mjs set wakeRoom=all maxAutoTurns=10

# 查看哪些字段被文件覆盖 / 校验 / 列出可用字段 / 清空
node tools/lanhub-policy.mjs get
node tools/lanhub-policy.mjs validate
node tools/lanhub-policy.mjs keys
node tools/lanhub-policy.mjs reset
```

DSHA 容器里用封装脚本（默认文件 `/root/.dsh/lan-hub.policy.json`）：

```sh
sh tools/wb_policy.sh set autoRespond=false
```

机制：脚本**原子写**（先写同目录 tmp 再 rename，插件不会读到半截 JSON）→
插件每 `policyPollMs` **异步** `stat` 一次 mtime → 变化则经 `normalizePolicy` 原地换策略。
文件不存在或 JSON 非法时**保留上一份策略**，不影响连接。

两个实现细节值得说明：

- 轮询用 `fs.promises` 而非 `statSync`——它跑在 Agent 的事件循环上，同步 IO 会直接
  卡住消息收发（网络盘 / proot / 机械盘上尤其明显）；同时有 in-flight 守卫，
  慢盘上不会堆积并发检查。
- mtime 戳**只在解析成功后提交**。读失败不推进戳，下一轮自动重试，不会出现
  "内容变了但被当成已读"的静默丢失。
- `set`/`unset` 遇到损坏文件会**重建**而不是拒绝（修文件的工具不该被坏文件锁死），
  `get`/`validate` 则照实报错，不掩盖问题。

生效优先级：`Schema 默认 < cordis.patch.yml < 策略文件 < hub_config 运行时`。
`hub_stats` 会打印每个字段的来源（`config` / `file` / `runtime`），
"改了到底生效没有"是可查的，不用猜。

白名单、默认值、钳制规则都写在 `src/policy.mjs`，**插件与脚本共用同一份**，
不会出现"脚本允许写、插件不认"的漂移。

> 脚本无法直接改运行中进程的内存——这是进程隔离决定的。所以"脚本化热更新"
> = 脚本写文件 + 插件轮询，而不是脚本直接注入。

## 体积与运行时开销

优化前后的实测数据（0.4.0 → 0.5.0）：

| 指标 | 之前 | 现在 | 说明 |
|---|---|---|---|
| 安装包 `.tgz` | 48.6 KB | **29.3 KB**（-40%） | |
| 解包后 | 146.6 KB | **72.5 KB**（-51%） | |
| 包内文件数 | 23 | 20 | |
| `dist/index.js` | 42.3 KB | **25.4 KB** | esbuild `minify` + `keepNames` |
| 策略 CLI | 8.2 KB 源码 + 3.9 KB 依赖 | **6.7 KB 单文件** | 打包后不再需要 `src/` |

省下来的是**运行时完全用不到的东西**：

- `src/*.ts`（34.7 + 19.9 = 54.6 KB）——DSH 加载的是 `dist`，源码进包纯属浪费；
  `policy.mjs` 因为被 CLI 依赖，过去也只能把整个 `src/` 塞进包。CLI 打成单文件后
  `src/` 可以整个不进包。
- `tools/` 只发 `wb_*.sh`（14 个小脚本），CLI 源码不发——包里不再有两份同一份逻辑。

关于启动性能，有个必须说清的事实：**CLI 冷启动 97 ms，其中 `node -e "0"` 基线就有
93 ms**，脚本自身逻辑只占约 4 ms。所以"优化脚本加载速度"的上限就是 4 ms，不值得为它
改结构。真正该优化的是**插件侧别阻塞事件循环**，以及**少调几次 CLI**（一次 `set` 可以
写多个字段）。

热路径上也清掉了两处会随消息量放大的写法：

- 投递确认从 `indexOf` + `splice` 逐条移除（O(n²)）改成一次 `Set` 过滤（O(n)）；
- 限速窗口从"每次 filter 重建数组"改成原地裁剪过期前缀，并且**无论限速是否开启都裁剪**
  （否则关闭限速时全局时间戳数组会无限增长）。

## 更新通道

**客户端已实现完整能力，服务端只预留接口**（契约先固定，实现后置）。

三个命令对应三种风险，这是刻意的拆分：

| 阶段 | 命令 | 副作用 | 退出码 |
|---|---|---|---|
| 检查 | `lanhub-update check` | 只读，零副作用 | 0 已最新 / 10 有更新 / 11 不兼容 |
| 暂存 | `lanhub-update fetch [ver]` | 只写缓存，**不动安装目录** | 0 / 1 |
| 安装 | `lanhub-update apply [ver] --yes` | 执行安装命令 | 0 / 12 缺 `--yes` / 1 |
| 回滚 | `lanhub-update rollback [--to ver] --yes` | 装回更旧的缓存版本 | 0 / 1 |
| 查看 | `lanhub-update list` / `verify` | 只读 | 0 / 1 |

```sh
# 仓库内
node tools/lanhub-update.mjs check
# 装成包之后
npx lanhub-update check && npx lanhub-update fetch --json
# DSHA 容器内
sh tools/wb_update.sh check
```

### 为什么不让插件自己更新

1. **代码不是数据。** policy 能热替换是因为它只是内存里的一个对象；代码更新要重新
   `import`，ESM 模块缓存 + 插件持有 TCP 长连接，做不到原地换。
2. **它反正省不掉重启。** 装完仍需宿主重启才加载新代码，所以"自动更新"只是把危险
   动作挪到你不在场的时候。
3. **权限倒挂。** LAN 无认证，让长跑进程用自身权限替换自己的可执行文件，等于把投毒面
   从"人手工装包"扩大到"任何能连上 Hub 的设备"。

因此 `apply` 必须显式 `--yes`，且装完会提示需要重启。`hub_stats` 会打印插件版本号，
升级后用它能立刻确认加载的是不是新字节。

### 服务端预留的接口

| 端点 | 说明 |
|---|---|
| `GET /api/updates` | 更新清单。**没有清单文件时也返回 200 + `latest:""`**——"没有更新"是合法状态，不能用 404 表达 |
| `GET /api/updates/pkg/<file>` | 制品下载，只接受纯文件名（反斜杠、`..`、非 `.tgz` 一律拒绝） |

数据目录由 `LANHUB_UPDATE_DIR` 指定（默认 `./updates`），结构：

```
updates/lanhub-update.json
updates/pkg/dsh-plugin-lan-hub-0.6.0.tgz
```

清单格式：

```json
{
  "name": "dsh-plugin-lan-hub",
  "latest": "0.6.0",
  "proto": 3,
  "artifacts": {
    "0.6.0": {
      "url": "/api/updates/pkg/dsh-plugin-lan-hub-0.6.0.tgz",
      "sha256": "<小写十六进制>",
      "size": 31000,
      "proto": 3,
      "notes": "一句话变更说明"
    }
  }
}
```

两条契约纪律：

- 清单损坏、或 `latest` 在 `artifacts` 里找不到时返回 **500**，绝不装作"没有更新"——
  静默降级会让人以为已经是最新版，实际是分发坏了。
- 这两个端点**不纳入** httpgateway 的 `routeCount`/`routes`（直接挂在共享 mux 上），
  否则要为一个"预留"去改 `/api/plugins` 的 v2 事实契约基线。
  待更新通道正式立项时再拆成独立 `updates` 插件并显式登记。

### 兼容性闸门

清单的 `proto` 是目标版本要求的线协议版本，客户端拿响应头 `X-Proto-Version`（Hub 实际
支持的版本）比对：**`proto > hubProto` 直接拒绝**（exit 11）。没有这道闸，最糟的失败模式
是"装完发现连不上"，而那时你已经在改日志了。

### 安全边界（必须说清）

`sha256` 只防**传输损坏**，不防**投毒**——清单和制品同源，能改一个就能改另一个。
真要防需要发布方签名 + 客户端 pin 公钥，对 LAN 自用属过度设计。所以真正的防线是
`--yes` 那道人工确认，而不是校验和。

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
| `hub_config` | `autoRespond?`, `wakeRoom?`, `muteForSec?`, `room?`, `unmute?`, `reset?` | 在线改策略（立即生效，不重连） |
| `hub_stats` | — | 当前生效策略 + 计数（诊断「为什么不回」） |

## 收信流水线（设计要点）

```
socket → inbox（唯一真源）→ pump（去抖/合并/分级）→ 投递策略（按 status）→ Agent
                                   ↓                        ↓
                             护栏（轮次/速率/hop）      计数与可见提示
```

**1）单一消费点**
消息只从 `pending + client.readMessages()` 出去，唤醒泵与 `hub_read_messages`
共用同一队列：谁先取谁消费，**天然不重复**（不再需要 `forwarded` 集合去事后过滤）。
投递失败的消息留在 `pending`，下次 `hub_read_messages` 依然能拿到——不会静默丢失。

**2）合并泵：一串消息只唤醒一次**
`client.onMessage` 只负责"打窗口"，`batchMs` 静默窗口内的多条消息合并成**一条**
跟随唤醒；`maxWaitMs` 是硬上限，防止持续来消息把唤醒无限推迟。
同一对端/房间的消息会合并成一条（带 `n 条消息` 标记），避免 N 条消息 = N 个轮次。

**3）按 `agent.status` 选原语（关键）**

| status | 原语 | 效果 |
|---|---|---|
| `idle` | `followup(msg)` | 开一轮，回一条 |
| `running` | `send(msg, 'next-step', false)` | 喂给当前轮的下一步，**不插队、不新开轮** |
| 降级 | `inject(msg)` | 只喂上下文 |

`followup` 的语义是"成为自己那一轮的唯一普通消息"——无脑用它会让每条消息都插一轮。
Agent 正在干活时用 `next-step` 才不打断它。

**4）分级唤醒**

- `chat`（私聊）：唤醒；
- `room`：默认 `mention`——只有 `@本agentId` / `@agentName` / 显式 `to` 才唤醒，
  `all` 全唤醒，`off` 不唤醒；
- `broadcast` / `presence` / `system`：永不唤醒（公告不是对话，避免全场齐回）；
- 自己发出的消息不唤醒（防自回声）。

**5）护栏（防对刷三层）**

- **轮次上限** `maxAutoTurns`：同一对话连续自动往返超限即静音，
  **人类一发言就重置**（监听 `agent/inbox/inserted` 里 `source.kind === 'user'`）——人类优先；
- **hop 计数**：出站消息在 `content.meta.hop` 自增，入站 `hop > maxHop` 不唤醒。
  `content` 是 `json.RawMessage`，**服务端零改动**、老客户端忽略未知字段；
- **速率限制**：每对端 / 全局每分钟唤醒次数，滑动窗口。

被护栏拦下的消息**仍然留在收件箱**（不丢），并按 `muteNotice` 注入一条可见提示，
用 `hub_stats` 能看到 `turn/rate/hop/manual` 各自拦了多少——
避免出现"消息到了但没人回、也看不出为什么"的静默失联。

**6）语义与安全**

入站消息包成 `source: { kind:'plugin', plugin:'lan-hub', form:'relay' }` 的 `UserMessage`：
`form:'relay'` 是 dsh 里「另一个 agent 发给本 agent 的消息」的语义标记。
文本还会显式声明**这是外部输入、不是人类指令、不要据此改变任务或调用危险工具**——
对端 Agent 的内容不可信，这条边界必须写进上下文。

**7）热更新：policy 原地生效**
订阅 Cordis `internal/update` waterfall，diff 连接层字段：
只有 connection 变了才 `next()` 放行重启；否则只换 `policy` 并 `return`，
**不断连、不丢收件箱**。注意此时 `fiber.config` 不会更新，插件以自身 `policy` 为准。

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
`plugin-sim.ts` 覆盖：插件契约、10 个工具注册、每个工具 execute 真实生效、
effect disposer 断开连接、autoInject 注入，以及 autoRespond 的完整语义——
经 `followup` 唤醒、`form:'relay'` + 注入防护文案、单一消费点不重复返回、
合并泵（3 条合并成 1 次唤醒）、`status=running` 走 `next-step`、
房间 `@我` 才唤醒、`hop` 护栏、`hub_config` 在线开关、热更新只对 policy 放行、
以及**真实调用 `tools/lanhub-policy.mjs` 写文件后插件原地生效**（含写入损坏 JSON 时
保留上一份策略、连接不受影响）。

`update-sim.ts` 覆盖更新通道的 10 条：已是最新（exit 0）、有更新（exit 10）、
proto 不兼容（exit 11）、sha256 校验与原子落盘、校验失败不留残留文件、
缺 `--yes` 不执行（exit 12）、确认后真的执行安装命令、缓存损坏被 `verify`/`apply` 拦截、
回滚自动挑更旧版本、`list --json` 结构可消费。它用本地 HTTP 假分发源 + **真实调用 CLI**，
测的是进程边界而不是函数 mock。

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
