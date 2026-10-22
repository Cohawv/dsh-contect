// src/index.ts
import { defineTool } from "@deepseek-ai/dsh-tools";
import Schema from "@deepseek-ai/schemastery";

// src/hub-client.ts
import net from "node:net";
import dgram from "node:dgram";
import os from "node:os";
var DEFAULT_HEARTBEAT_MS = 15e3;
var DEFAULT_REQUEST_TIMEOUT_MS = 3e3;
var DEFAULT_INBOX_LIMIT = 200;
var DEFAULT_RECONNECT_MAX_MS = 3e4;
var CLIENT_PROTO = 3;
var CLIENT_CAPS = ["structured-ack", "heartbeat-negotiation", "discovery", "agent-caps", "endpoints"];
var DEFAULT_DISCOVER_PORT = 8082;
var DEFAULT_DISCOVER_TIMEOUT_MS = 5e3;
var DISCOVER_ATTEMPTS = 2;
var DISCOVER_RETRY_GAP_MS = 1200;
var HubClient = class {
  host;
  port;
  agentId;
  agentName;
  /** 自动发现：host 为 'auto'/'' 时触发，找到后覆盖实际连接地址 */
  autoDiscover;
  discoverPort;
  fallbackHost;
  fallbackPort;
  /** 心跳间隔：初始值来自配置，注册后可能被服务端协商值覆盖 */
  heartbeatMs;
  /** 协商后的协议版本与服务端能力集（注册回执下发） */
  negotiatedProto = 1;
  serverCaps = [];
  serverFeatures = {};
  reconnectEnabled;
  reconnectMaxMs;
  requestTimeoutMs;
  inboxLimit;
  log;
  sock = null;
  buf = "";
  inbox = [];
  pending = [];
  listeners = [];
  heartbeat = null;
  retryTimer = null;
  retryDelay = 1e3;
  disposed = false;
  /** 注册回执已到达（区别于 socket 已建立） */
  ready = false;
  constructor(o) {
    this.host = o.host;
    this.port = o.port;
    this.agentId = o.agentId;
    this.agentName = o.agentName ?? o.agentId;
    this.heartbeatMs = o.heartbeatMs ?? DEFAULT_HEARTBEAT_MS;
    this.reconnectEnabled = o.reconnect ?? true;
    this.reconnectMaxMs = o.reconnectMaxMs ?? DEFAULT_RECONNECT_MAX_MS;
    this.requestTimeoutMs = o.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS;
    this.inboxLimit = o.inboxLimit ?? DEFAULT_INBOX_LIMIT;
    this.log = o.log ?? ((m) => console.error(m));
    this.autoDiscover = o.autoDiscover ?? (o.host === "auto" || o.host === "");
    this.discoverPort = o.discoverPort ?? DEFAULT_DISCOVER_PORT;
    this.fallbackHost = o.fallbackHost ?? "127.0.0.1";
    this.fallbackPort = o.fallbackPort ?? 8080;
  }
  get id() {
    return this.agentId;
  }
  get connected() {
    return this.sock !== null;
  }
  /** 是否已完成注册（socket 建立后还需等服务端回执） */
  get isReady() {
    return this.ready;
  }
  /** 订阅收到的每一帧（用于把消息回灌给 Agent 上下文） */
  onMessage(cb) {
    this.listeners.push(cb);
  }
  /**
   * 建立连接；host 为 'auto' 时先走 UDP 自动发现，找到 Hub 再连。
   * 重连逻辑内置，调用方无需关心。
   */
  async connect() {
    if (this.disposed) return;
    let host = this.host;
    let port = this.port;
    if (this.autoDiscover) {
      let lastErr = null;
      for (let attempt = 1; attempt <= DISCOVER_ATTEMPTS; attempt++) {
        try {
          const found = await this.discover();
          host = found.host;
          port = found.port;
          this.log(`[lan-hub] discovered hub at ${host}:${port} (auto${attempt > 1 ? `, attempt ${attempt}` : ""})`);
          lastErr = null;
          break;
        } catch (e) {
          lastErr = e;
          this.log(`[lan-hub] discovery attempt ${attempt}/${DISCOVER_ATTEMPTS} failed: ${lastErr.message}`);
          if (attempt < DISCOVER_ATTEMPTS) await new Promise((r) => setTimeout(r, DISCOVER_RETRY_GAP_MS));
        }
      }
      if (lastErr) {
        this.log(`[lan-hub] discovery failed, fallback ${this.fallbackHost}:${this.fallbackPort}`);
        host = this.fallbackHost;
        port = this.fallbackPort;
      }
    }
    const sock = net.connect(port, host);
    this.sock = sock;
    sock.setNoDelay(true);
    sock.on("connect", () => {
      this.retryDelay = 1e3;
      this.sendRaw({
        type: "register",
        v: CLIENT_PROTO,
        from: this.agentId,
        content: { name: this.agentName, caps: CLIENT_CAPS }
      });
      this.startHeartbeat();
      this.log(`[lan-hub] connected to ${host}:${port} as ${this.agentId}`);
    });
    sock.on("data", (chunk) => this.onData(chunk));
    sock.on("error", (e) => this.log(`[lan-hub] socket error: ${e.message}`));
    sock.on("close", () => this.onClose());
  }
  /** 彻底关闭并释放资源（插件卸载时调用），之后不再重连 */
  close() {
    this.disposed = true;
    this.stopHeartbeat();
    if (this.retryTimer) {
      clearTimeout(this.retryTimer);
      this.retryTimer = null;
    }
    this.rejectAllPending(new Error("client closed"));
    if (this.sock) {
      this.sock.destroy();
      this.sock = null;
    }
  }
  // ---------- 局域网自动发现（UDP 广播查询 + 单播应答） ----------
  /**
   * 通过 UDP 发现同网段的 Hub：向广播地址发 _lanhub_query，
   * 服务端 discovery 服务收到后立即单播回一个 _lanhub_beacon，
   * 从中取出 Hub 的真实 IP 与 tcp 端口。失败（超时/无 Hub）则 reject，
   * 由 connect 回退到 fallback 地址。
   */
  discover() {
    return new Promise((resolve, reject) => {
      const sock = dgram.createSocket("udp4");
      let done = false;
      let scanTimer = null;
      let sentCount = 0;
      let recvCount = 0;
      let subsCount = 0;
      const finish = (err, val) => {
        if (done) return;
        done = true;
        if (scanTimer) clearTimeout(scanTimer);
        try {
          sock.close();
        } catch {
        }
        if (err) reject(err);
        else resolve(val);
      };
      const subs = [];
      const ifs = os.networkInterfaces();
      for (const name2 of Object.keys(ifs)) {
        for (const it of ifs[name2] || []) {
          if (!it || it.family !== "IPv4" || it.internal) continue;
          const ip = it.address.split(".").map(Number);
          if (ip.length !== 4 || ip.some((n) => !Number.isFinite(n))) continue;
          subs.push({ self: it.address, prefix: ip[0] + "." + ip[1] + "." + ip[2] });
        }
      }
      subsCount = subs.length;
      const timeout = setTimeout(
        () => finish(new Error(`discovery timeout (ifaces=${subsCount}, sent=${sentCount}, recv=${recvCount})`)),
        DEFAULT_DISCOVER_TIMEOUT_MS
      );
      sock.on("message", (msg, rinfo) => {
        recvCount++;
        try {
          const b = JSON.parse(msg.toString("utf8"));
          if (b && b.type === "_lanhub_beacon" && typeof b.tcpPort === "number") {
            clearTimeout(timeout);
            finish(null, { host: rinfo.address, port: b.tcpPort });
          }
        } catch {
        }
      });
      sock.on("error", (e) => {
        clearTimeout(timeout);
        finish(e);
      });
      sock.bind(() => {
        try {
          sock.setBroadcast(true);
        } catch {
        }
        const q = Buffer.from(JSON.stringify({ type: "_lanhub_query" }));
        const sendTo = (ip) => {
          sentCount++;
          try {
            sock.send(q, 0, q.length, this.discoverPort, ip, () => {
            });
          } catch {
          }
        };
        sendTo("255.255.255.255");
        for (const x of subs) sendTo(x.prefix + ".255");
        scanTimer = setTimeout(() => {
          if (done) return;
          for (const x of subs) {
            for (let i = 1; i <= 254; i++) {
              const ip = x.prefix + "." + i;
              if (ip === x.self) continue;
              sendTo(ip);
            }
          }
        }, 400);
      });
    });
  }
  // ---------- 发送类（fire-and-forget，服务端保证 seq 有序） ----------
  sendTo(to, text) {
    this.sendRaw({ type: "chat", from: this.agentId, to, content: { text } });
  }
  sendRoom(roomId, text) {
    this.sendRaw({ type: "room", from: this.agentId, roomId, content: { text } });
  }
  broadcast(text) {
    this.sendRaw({ type: "broadcast", from: this.agentId, content: { text } });
  }
  joinRoom(roomId) {
    this.sendRaw({ type: "join", from: this.agentId, content: { roomId } });
  }
  leaveRoom(roomId) {
    this.sendRaw({ type: "leave", from: this.agentId, content: { roomId } });
  }
  // ---------- 请求-响应式（等待服务端回执） ----------
  /** 创建房间，返回 roomId */
  async createRoom(name2, members = []) {
    this.sendRaw({ type: "create_room", from: this.agentId, content: { name: name2, members } });
    const env = await this.waitFor(
      (e) => e.type === "system" && e.content?.action === "created",
      this.requestTimeoutMs
    );
    return String(env.content?.roomId ?? "");
  }
  /** 拉取在线 Agent 列表 */
  async listAgents() {
    this.sendRaw({ type: "list", from: this.agentId });
    const env = await this.waitFor(
      (e) => e.type === "system" && e.content?.action === "list",
      this.requestTimeoutMs
    );
    return env.content?.agents ?? [];
  }
  /** 等待注册完成（socket 建立后还需等服务端回执），用于启动时的确定性等待 */
  async waitReady(timeoutMs = this.requestTimeoutMs) {
    if (this.ready) return;
    await this.waitFor((e) => this.isRegisterAck(e), timeoutMs);
  }
  /** 协商后的协议版本（调试用） */
  get proto() {
    return this.negotiatedProto;
  }
  /** 服务端声明的能力集（调试用） */
  get caps() {
    return this.serverCaps.slice();
  }
  // ---------- 注册回执识别与协商 ----------
  /**
   * 识别注册回执：优先匹配结构化 action，旧服务端没有该字段时回退到历史文案。
   * 双判定是为了让客户端能同时对接新旧两版服务端——任一侧先升级都不会断。
   */
  isRegisterAck(env) {
    if (env.type !== "system") return false;
    const c = env.content;
    if (!c) return false;
    return c.action === "registered" || typeof c.text === "string" && c.text.startsWith("registered as");
  }
  /** 应用服务端下发的协商结果：协议版本、能力集、心跳间隔、feature 开关 */
  applyNegotiation(env) {
    const c = env.content;
    if (!c) return;
    const proto = Number(c.proto);
    if (Number.isFinite(proto) && proto > 0) this.negotiatedProto = proto;
    if (Array.isArray(c.serverCaps)) this.serverCaps = c.serverCaps;
    if (c.features && typeof c.features === "object") this.serverFeatures = c.features;
    const hb = Number(c.heartbeatMs);
    if (Number.isFinite(hb) && hb > 0 && hb !== this.heartbeatMs) {
      this.heartbeatMs = hb;
      if (this.heartbeat) this.startHeartbeat();
    }
    const feat = Object.keys(this.serverFeatures).filter((k) => this.serverFeatures[k]).join(",");
    this.log(`[lan-hub] negotiated proto=${this.negotiatedProto} heartbeat=${this.heartbeatMs}ms features=[${feat}]`);
  }
  // ---------- 收件箱（拉取后清空） ----------
  /** Hub 是推送式的：收到的消息先进 inbox，由调用方主动拉取 */
  readMessages() {
    const out = this.inbox;
    this.inbox = [];
    return out;
  }
  peekMessages() {
    return this.inbox.slice();
  }
  // ---------- 内部实现 ----------
  sendRaw(env) {
    if (!this.sock) {
      throw new Error("not connected to hub");
    }
    if (!env.msgId) env.msgId = randomId();
    if (!env.ts) env.ts = Date.now();
    this.sock.write(JSON.stringify(env) + "\n");
  }
  onData(chunk) {
    this.buf += chunk.toString("utf8");
    let idx;
    while ((idx = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, idx).trim();
      this.buf = this.buf.slice(idx + 1);
      if (!line) continue;
      let env;
      try {
        env = JSON.parse(line);
      } catch {
        continue;
      }
      this.onFrame(env);
    }
  }
  onFrame(env) {
    if (!this.ready && this.isRegisterAck(env)) {
      this.ready = true;
      this.applyNegotiation(env);
    }
    for (let i = 0; i < this.pending.length; i++) {
      const p = this.pending[i];
      let matched = false;
      try {
        matched = p.match(env);
      } catch {
        matched = false;
      }
      if (matched) {
        clearTimeout(p.timer);
        this.pending.splice(i, 1);
        p.resolve(env);
        return;
      }
    }
    for (const cb of this.listeners) {
      try {
        cb(env);
      } catch {
      }
    }
    this.inbox.push(env);
    if (this.inbox.length > this.inboxLimit) {
      this.inbox.splice(0, this.inbox.length - this.inboxLimit);
    }
  }
  waitFor(match, timeoutMs) {
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        const i = this.pending.indexOf(entry);
        if (i >= 0) this.pending.splice(i, 1);
        reject(new Error("timeout waiting for hub response"));
      }, timeoutMs);
      const entry = { match, resolve, reject, timer };
      this.pending.push(entry);
    });
  }
  startHeartbeat() {
    this.stopHeartbeat();
    this.heartbeat = setInterval(() => {
      if (!this.sock) return;
      try {
        this.sock.write(JSON.stringify({ type: "heartbeat", from: this.agentId, msgId: randomId(), ts: Date.now() }) + "\n");
      } catch {
      }
    }, this.heartbeatMs);
    this.heartbeat.unref?.();
  }
  stopHeartbeat() {
    if (this.heartbeat) {
      clearInterval(this.heartbeat);
      this.heartbeat = null;
    }
  }
  onClose() {
    this.sock = null;
    this.ready = false;
    this.stopHeartbeat();
    this.rejectAllPending(new Error("connection closed"));
    if (this.disposed || !this.reconnectEnabled) return;
    const delay = this.retryDelay;
    this.retryDelay = Math.min(this.retryDelay * 2, this.reconnectMaxMs);
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      if (!this.disposed) this.connect();
    }, delay);
    this.retryTimer.unref?.();
    this.log(`[lan-hub] disconnected, reconnecting in ${delay}ms`);
  }
  rejectAllPending(err) {
    for (const p of this.pending) {
      clearTimeout(p.timer);
      p.reject(err);
    }
    this.pending = [];
  }
};
function randomId() {
  return Math.random().toString(16).slice(2, 10) + Date.now().toString(16).slice(-6);
}

// src/index.ts
var name = "lan-hub";
var inject = ["tools"];
var Config = Schema.object({
  host: Schema.string().default("127.0.0.1").description("Hub \u5730\u5740"),
  port: Schema.number().default(8080).description("Hub TCP \u7AEF\u53E3"),
  agentId: Schema.string().default("dsh-agent").description("\u672C Agent \u5728 Hub \u4E2D\u7684\u552F\u4E00 ID"),
  agentName: Schema.string().default("HarnessAgent").description("\u5C55\u793A\u540D"),
  heartbeatMs: Schema.number().default(15e3).description("\u5FC3\u8DF3\u95F4\u9694\uFF08\u6BEB\u79D2\uFF09"),
  reconnect: Schema.boolean().default(true).description("\u65AD\u7EBF\u81EA\u52A8\u91CD\u8FDE"),
  requestTimeoutMs: Schema.number().default(3e3).description("\u8BF7\u6C42\u54CD\u5E94\u8D85\u65F6\uFF08\u6BEB\u79D2\uFF09"),
  inboxLimit: Schema.number().default(200).description("\u6536\u4EF6\u7BB1\u4E0A\u9650"),
  autoInject: Schema.boolean().default(false).description("\u6536\u5230\u6D88\u606F\u65F6\u81EA\u52A8\u6CE8\u5165 Agent \u4E0A\u4E0B\u6587"),
  /** 局域网自动发现：把 host 设为 "auto" 即启用，无需手动填 IP */
  autoDiscover: Schema.boolean().default(false).description("host \u4E3A auto \u65F6\u542F\u7528 UDP \u5C40\u57DF\u7F51\u81EA\u52A8\u53D1\u73B0"),
  discoverPort: Schema.number().default(8082).description("Hub \u53D1\u73B0\u670D\u52A1 UDP \u7AEF\u53E3\uFF08\u4E0E\u670D\u52A1\u7AEF -discover \u4E00\u81F4\uFF09"),
  /** 发现失败时的兜底地址；留空则用 127.0.0.1（手机上等于手机自己，建议显式填 Hub 局域网 IP） */
  fallbackHost: Schema.string().default("").description("\u81EA\u52A8\u53D1\u73B0\u5931\u8D25\u65F6\u7684\u515C\u5E95 Hub \u5730\u5740")
});
function apply(ctx, config) {
  const client = new HubClient({
    host: config.host,
    port: config.port,
    agentId: config.agentId,
    agentName: config.agentName,
    heartbeatMs: config.heartbeatMs,
    reconnect: config.reconnect,
    requestTimeoutMs: config.requestTimeoutMs,
    inboxLimit: config.inboxLimit,
    autoDiscover: config.host === "auto" || config.autoDiscover,
    discoverPort: config.discoverPort,
    fallbackHost: config.fallbackHost || "127.0.0.1",
    fallbackPort: config.port,
    log: (m) => console.error(m)
  });
  ctx.effect(() => {
    client.connect();
    return () => client.close();
  });
  async function injectIntoAgent(exec, envs) {
    if (!config.autoInject || envs.length === 0) return;
    const text = formatEnvelopes(envs);
    try {
      await exec?.agent?.inject?.({
        content: [{ type: "text", text }],
        source: { kind: "lan-hub" }
      });
    } catch {
    }
  }
  ctx.tools.register(defineTool({
    name: "hub_send",
    description: "Send a direct message to another agent on the LAN hub.",
    parameters: {
      to: { type: "string", required: true, description: "Target agent id" },
      text: { type: "string", required: true, description: "Message text" }
    },
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute(args) {
      try {
        client.sendTo(args.to, args.text);
        return `sent to ${args.to}`;
      } catch (e) {
        return `failed: ${e.message}`;
      }
    }
  }));
  ctx.tools.register(defineTool({
    name: "hub_send_room",
    description: "Send a message to a chat room on the LAN hub.",
    parameters: {
      room: { type: "string", required: true, description: "Room id" },
      text: { type: "string", required: true, description: "Message text" }
    },
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute(args) {
      try {
        client.sendRoom(args.room, args.text);
        return `sent to room ${args.room}`;
      } catch (e) {
        return `failed: ${e.message}`;
      }
    }
  }));
  ctx.tools.register(defineTool({
    name: "hub_broadcast",
    description: "Broadcast a message to every agent on the LAN hub.",
    parameters: {
      text: { type: "string", required: true, description: "Message text" }
    },
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute(args) {
      try {
        client.broadcast(args.text);
        return "broadcast sent";
      } catch (e) {
        return `failed: ${e.message}`;
      }
    }
  }));
  ctx.tools.register(defineTool({
    name: "hub_create_room",
    description: "Create a chat room on the LAN hub and return its id.",
    parameters: {
      name: { type: "string", required: true, description: "Room name" }
    },
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute(args) {
      try {
        const id = await client.createRoom(args.name);
        return id || "failed: no roomId returned";
      } catch (e) {
        return `failed: ${e.message}`;
      }
    }
  }));
  ctx.tools.register(defineTool({
    name: "hub_join_room",
    description: "Join a chat room on the LAN hub.",
    parameters: {
      room: { type: "string", required: true, description: "Room id" }
    },
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute(args) {
      try {
        client.joinRoom(args.room);
        return `joined ${args.room}`;
      } catch (e) {
        return `failed: ${e.message}`;
      }
    }
  }));
  ctx.tools.register(defineTool({
    name: "hub_leave_room",
    description: "Leave a chat room on the LAN hub.",
    parameters: {
      room: { type: "string", required: true, description: "Room id" }
    },
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute(args) {
      try {
        client.leaveRoom(args.room);
        return `left ${args.room}`;
      } catch (e) {
        return `failed: ${e.message}`;
      }
    }
  }));
  ctx.tools.register(defineTool({
    name: "hub_list_agents",
    description: "List agents currently online on the LAN hub.",
    parameters: {},
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute() {
      try {
        const list = await client.listAgents();
        if (list.length === 0) return "no agents online";
        return list.map((a) => `${a.id}${a.name ? ` (${a.name})` : ""}`).join(", ");
      } catch (e) {
        return `failed: ${e.message}`;
      }
    }
  }));
  ctx.tools.register(defineTool({
    name: "hub_read_messages",
    description: "Drain messages received from the LAN hub since the last read.",
    parameters: {},
    output: {
      schema: { type: "string" },
      render: (_args, value) => [{ type: "text", text: value }]
    },
    async execute(_args, exec) {
      const msgs = client.readMessages();
      await injectIntoAgent(exec, msgs);
      if (msgs.length === 0) return "no new messages";
      return formatEnvelopes(msgs);
    }
  }));
}
function formatEnvelopes(envs) {
  return envs.map((e) => {
    const c = e.content ?? {};
    switch (e.type) {
      case "chat":
        return `[chat] ${e.from} -> ${e.to}: ${c.text ?? ""}`;
      case "room":
        return `[room ${e.roomId}] ${e.from}: ${c.text ?? ""}`;
      case "broadcast":
        return `[broadcast] ${e.from}: ${c.text ?? ""}`;
      case "presence":
        return `[presence] ${c.id} is ${c.status}`;
      case "system":
        return `[system] ${c.text ?? JSON.stringify(c)}`;
      default:
        return `[${e.type ?? "unknown"}] ${JSON.stringify(c)}`;
    }
  }).join("\n");
}
export {
  Config,
  apply,
  inject,
  name
};
