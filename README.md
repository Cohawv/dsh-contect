# LAN Agent Hub — 最小可运行 MVP

局域网内单中心服务器的多 Agent 通信系统最小实现。仅用 Go 标准库，**零外部依赖**，可离线构建。

## 特性（MVP）
- 单 Hub 服务器，TCP + NDJSON（每行一个 JSON 信封）传输
- 每连接 1 读 + 1 写协程，高并发、低延时
- 非阻塞发送（带缓冲信道，满则丢弃），**不阻塞、不拥堵**
- 点对点私聊、聊天室广播、全员广播
- Agent 注册 / 心跳 / 上下线事件（presence）/ 在线列表
- 全局单调递增 `seq` 保证消息有序
- 心跳超时自动清理离线连接

## 运行

```bash
# 启动 Hub（默认监听 :8080）
go run . -addr :8080

# 另开两个终端，启动两个测试客户端
go run ./client -id A -name Alice
go run ./client -id B -name Bob
```

客户端交互命令：
- `@<id> 消息`      私聊
- `#<roomId> 消息`  发到聊天室
- `create:<名称>`   创建聊天室
- `join:<roomId>`   加入聊天室
- `leave:<roomId>`  离开聊天室
- `who`            查看在线 agent
- 其它文字         全员广播

## 验证

```bash
go vet ./...
go test ./...    # 覆盖：私聊、建房+进房广播、离线提示
```

## 协议（统一信封 Envelope）

```json
{
  "msgId": "uuid",
  "type": "register|heartbeat|chat|room|broadcast|create_room|join|leave|list|system|presence",
  "from": "agentA",
  "to": "agentB",
  "roomId": null,
  "content": { },
  "seq": 1024,
  "ts": 1697890123456
}
```

传输：TCP，UTF-8，每行一个 JSON（以 `\n` 结尾）。

## 已知限制（后续增强）
- 传输为 TCP NDJSON，未做 WebSocket；生产环境可替换为 WebSocket 并套用统一 `Transport` 抽象
- 无离线消息队列（目标离线仅回执提示），无持久化
- 未做鉴权（token 校验）、限流、TLS
- 房间无权限/角色管理
- 单节点，未做水平扩展

## HTTP 网关 & Skill/DSH 接入

- Hub 同时监听 **TCP（默认 `:8080`，NDJSON）** 与 **HTTP REST 网关（默认 `:8081`，纯标准库，复用同一套 `Hub` 逻辑）**，所有接入形式共用统一 `Envelope` 协议。
- HTTP 端点：`GET /api/agents`、`POST /api/send`、`POST /api/room`、`POST /api/create_room`、`POST /api/join`、`POST /api/leave`。
- 已提供 **Node 版 WorkBuddy Skill**（零第三方依赖，仅用内置 `net`）：`~/.workbuddy/skills/lan-hub/`，含 `SKILL.md` 与 `client.js`，支持 `send / send-room / create-room / join / leave / list / listen`，经 TCP 接入 Hub。
- 验证：HTTP 网关 → Hub 路由 → Node 客户端投递 PASS；Node 客户端私聊投递 PASS；`seq` 全局单调递增。

## MCP 接入

- `mcp/mcp-server.js`：零第三方依赖的 MCP 桥接服务（只用 Node 内置 `net`/`stdio`）。
  一端作为普通 Agent 经 TCP 接入 Hub，另一端通过 stdio 讲 **MCP（JSON-RPC 2.0，每行一个 JSON）**。
- 暴露工具：`send_message`、`send_room_message`、`create_room`、`join_room`、`leave_room`、`list_agents`、`broadcast`、`read_messages`。
- MCP 是请求/响应式，因此收到的消息先入缓冲区，由 `read_messages` 拉取并清空。
- 已在 `~/.workbuddy/mcp.json` 注册为 `lan-hub`（stdio 类型），任何 MCP host 可直接加入局域网聊天。
- 验证 E2E PASS：`initialize` / `tools/list`（8 个工具）/ `send_message`（接收方真实收到）/ `list_agents` / `create_room`（返回 roomId）/ `read_messages` 全部正常。

### 两种传输

MCP 服务支持两种传输，**工具逻辑共用一份**：

| 传输 | 启动方式 | 适用 |
|------|----------|------|
| stdio | `node mcp-server.js` | 桌面 MCP host 启动（本地子进程），已注册为 `lan-hub` |
| Streamable HTTP | `node mcp-server.js --http 8082` | 安卓 / 远程 MCP 客户端，端点 `http://<局域网IP>:8082/mcp` |

- **stdio 是本地子进程模型**：host 启动它并用 stdin/stdout 通信。手机无法启动子进程，所以**安卓不能用 stdio，必须用 Streamable HTTP**。
- Streamable HTTP 端点：`POST /mcp`（JSON-RPC）；`initialize` 响应头带 `Mcp-Session-Id`；`DELETE` 终止会话；`GET` 暂不支持服务端推送流（客户端用 `read_messages` 拉取）。
- 协议版本协商：支持 `2024-11-05` / `2025-03-26` / `2025-06-18`，**回显客户端请求的版本**，兼容新旧 host。

### 安卓接入

1. 电脑启动 Hub：`go run . -addr :8080`
2. 启动 MCP HTTP 服务：`node mcp-server.js --http 8082`
3. 安卓端（如 Google AI Edge Gallery，2026-05 起实验性支持 MCP over Streamable HTTP）注册 URL：`http://<电脑局域网IP>:8082/mcp`
4. 注意：端侧模型上下文窗口小，工具描述应尽量简短；手机与电脑需在同一局域网，且电脑防火墙放行 8082 端口。

## DSH 插件接入（DeepSeek Harness）

- `dsh/`：函数式 Cordis 插件，让 **DeepSeek Harness（dsh）** 里的 Agent 接入 Hub。
  至此 **Skill ✅ / MCP ✅ / DSH ✅** 三种接入形式全部闭环，且共用同一套 `Envelope` 协议，
  三种形态的 Agent 可以互相私聊或同处一个聊天室。
- 结构：`src/index.ts`（插件入口：`apply(ctx, config)` + 8 个 tools）
  + `src/hub-client.ts`（**核心客户端，纯 Node、零 DSH 依赖**，可被任意 Node 程序复用）。
  核心逻辑与框架解耦，因此可以脱离 dsh 运行时被完整测试。
- 注册工具：`hub_send`、`hub_send_room`、`hub_broadcast`、`hub_create_room`、
  `hub_join_room`、`hub_leave_room`、`hub_list_agents`、`hub_read_messages`。
- 安装：`dsh plugin --profile <name> add ./dsh`（`package.json` 已声明 `dsh.bundle`）；
  调试可用 `pnpm dsh web --patch ./dsh/cordis.patch.yml`。
- **必须发布编译后的 JS**：`npm run build`（esbuild）产出 `dist/index.js` 再 `npm pack`。
  直接把 `main` 指向 `.ts` 源码在 DSHA 上必然失败（Node 24 的 type stripping
  不支持 `node_modules` 下的文件，实测报 `failed to import`）。详见 `dsh/README.md`。
- DSH 特有设计：连接通过 `ctx.effect()` 注册，**插件卸载 / HMR 热替换时自动断开**；
  可选 `autoInject` 通过 `exec.agent.inject()` 把消息回灌进 Agent 上下文（MCP 形态做不到）。
- 验证：
  - `node dsh/test/smoke.ts` —— 核心客户端 E2E（注册/私聊/建房/房间/广播/离线提示/上下线/收件箱清空）PASS
  - `node --import ./dsh/test/mocks/register.mjs ./dsh/test/plugin-sim.ts` —— 插件入口（模拟 Cordis 运行时，
    mock `@deepseek-ai/*`）PASS，覆盖 8 个工具 execute 真实生效、disposer 断连、autoInject 注入
  - **安卓真机（DSHA）PASS**：荣耀 SDY-AN00 / Android 14 / DSHA 0.2.21 / dsh 0.2.0-rc.2。
    手机上的 `phone-agent` 上线电脑 Hub；容器内加载插件后 8 个工具全部可用，
    与电脑端 Skill Agent 双向收发消息（`hello-from-phone` / `hello-from-pc-to-phone`），
    建房→进房→房间消息→离房、广播均通过。

详见 `dsh/README.md`（含 ADB 无线调试打通容器终端的具体做法与坐标坑）。
