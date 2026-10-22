/**
 * LAN Agent Hub 客户端（纯 Node，零第三方依赖，零 DSH 依赖）
 *
 * 这个文件是与框架无关的核心：任何 Node 运行时（DSH 插件、普通脚本、测试）
 * 都可以直接用它接入 Hub。DSH 适配层见 ./index.ts。
 *
 * 传输：TCP + NDJSON（每行一个 JSON 信封），与 Go 服务端的 Envelope 结构一致，
 * 因此与 Skill / MCP / HTTP 网关等接入形式完全互通。
 *
 * 注意：源码为 TypeScript，但只使用 Node 原生 type-stripping 支持的语法
 * （不用 enum / namespace / 参数属性），因此可被 node / tsx 直接加载。
 */

import net from 'node:net'
import dgram from 'node:dgram'
import os from 'node:os'

/** 统一的消息信封，与服务端 protocol.go 的 Envelope 一一对应 */
export interface Envelope {
  msgId?: string
  type?: string
  from?: string
  to?: string
  roomId?: string
  content?: Record<string, any> | null
  seq?: number
  ts?: number
}

export interface AgentInfo {
  id: string
  name?: string
}

/** 自动发现的 Hub 端点 */
export interface DiscoveredHub {
  host: string
  port: number
}

export interface HubClientOptions {
  host: string
  port: number
  /** 本 Agent 在 Hub 中的唯一 ID，其它 Agent 通过它点对点寻址 */
  agentId: string
  /** 展示名 */
  agentName?: string
  /** 心跳间隔，服务端 60s 无心跳即清理，默认 15s 足够安全 */
  heartbeatMs?: number
  /** 断线是否自动重连（指数退避） */
  reconnect?: boolean
  reconnectMaxMs?: number
  /** 请求-响应式调用（list / create_room）的超时 */
  requestTimeoutMs?: number
  /** 收件箱上限，超出丢弃最旧的，防止长时间运行内存膨胀 */
  inboxLimit?: number
  log?: (msg: string) => void
  // ---------- 局域网自动发现 ----------
  /** 是否启用自动发现：host 设为 'auto' 时即触发，无需显式开启 */
  autoDiscover?: boolean
  /** 发现服务 UDP 端口（与服务端 -discover 一致），默认 8082 */
  discoverPort?: number
  /** 发现超时后回退的连接地址（默认 127.0.0.1:8080） */
  fallbackHost?: string
  fallbackPort?: number
}

interface Pending {
  match: (env: Envelope) => boolean
  resolve: (env: Envelope) => void
  reject: (err: Error) => void
  timer: ReturnType<typeof setTimeout>
}

const DEFAULT_HEARTBEAT_MS = 15_000
const DEFAULT_REQUEST_TIMEOUT_MS = 3_000
const DEFAULT_INBOX_LIMIT = 200
const DEFAULT_RECONNECT_MAX_MS = 30_000

/** 客户端声明的协议版本；服务端据此决定是否下发新字段（能力协商） */
const CLIENT_PROTO = 3
/**
 * 客户端声明的能力集。
 * 新增 discovery / agent-caps / endpoints 后，服务端才会下发对应字段；
 * 老服务端（v<3）不认识这些 cap，直接忽略，连接照常工作。
 */
const CLIENT_CAPS = ['structured-ack', 'heartbeat-negotiation', 'discovery', 'agent-caps', 'endpoints']
const DEFAULT_DISCOVER_PORT = 8082
const DEFAULT_DISCOVER_TIMEOUT_MS = 5000
/** 发现失败重试次数：启动早期网卡未就绪时靠重试自愈（不必重启） */
const DISCOVER_ATTEMPTS = 2
/** 两次发现尝试之间的间隔 */
const DISCOVER_RETRY_GAP_MS = 1200

export class HubClient {
  private readonly host: string
  private readonly port: number
  private readonly agentId: string
  private readonly agentName: string
  /** 自动发现：host 为 'auto'/'' 时触发，找到后覆盖实际连接地址 */
  private readonly autoDiscover: boolean
  private readonly discoverPort: number
  private readonly fallbackHost: string
  private readonly fallbackPort: number
  /** 心跳间隔：初始值来自配置，注册后可能被服务端协商值覆盖 */
  private heartbeatMs: number
  /** 协商后的协议版本与服务端能力集（注册回执下发） */
  private negotiatedProto = 1
  private serverCaps: string[] = []
  private serverFeatures: Record<string, any> = {}
  private readonly reconnectEnabled: boolean
  private readonly reconnectMaxMs: number
  private readonly requestTimeoutMs: number
  private readonly inboxLimit: number
  private readonly log: (msg: string) => void

  private sock: net.Socket | null = null
  private buf = ''
  private inbox: Envelope[] = []
  private pending: Pending[] = []
  private listeners: Array<(env: Envelope) => void> = []
  private heartbeat: ReturnType<typeof setInterval> | null = null
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private retryDelay = 1_000
  private disposed = false
  /** 注册回执已到达（区别于 socket 已建立） */
  private ready = false

  constructor(o: HubClientOptions) {
    this.host = o.host
    this.port = o.port
    this.agentId = o.agentId
    this.agentName = o.agentName ?? o.agentId
    this.heartbeatMs = o.heartbeatMs ?? DEFAULT_HEARTBEAT_MS
    this.reconnectEnabled = o.reconnect ?? true
    this.reconnectMaxMs = o.reconnectMaxMs ?? DEFAULT_RECONNECT_MAX_MS
    this.requestTimeoutMs = o.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS
    this.inboxLimit = o.inboxLimit ?? DEFAULT_INBOX_LIMIT
    this.log = o.log ?? ((m) => console.error(m))
    this.autoDiscover = o.autoDiscover ?? (o.host === 'auto' || o.host === '')
    this.discoverPort = o.discoverPort ?? DEFAULT_DISCOVER_PORT
    this.fallbackHost = o.fallbackHost ?? '127.0.0.1'
    this.fallbackPort = o.fallbackPort ?? 8080
  }

  get id(): string {
    return this.agentId
  }

  get connected(): boolean {
    return this.sock !== null
  }

  /** 是否已完成注册（socket 建立后还需等服务端回执） */
  get isReady(): boolean {
    return this.ready
  }

  /** 订阅收到的每一帧（用于把消息回灌给 Agent 上下文） */
  onMessage(cb: (env: Envelope) => void): void {
    this.listeners.push(cb)
  }

  /**
   * 建立连接；host 为 'auto' 时先走 UDP 自动发现，找到 Hub 再连。
   * 重连逻辑内置，调用方无需关心。
   */
  async connect(): Promise<void> {
    if (this.disposed) return
    let host = this.host
    let port = this.port
    if (this.autoDiscover) {
      // 启动早期网卡/路由可能尚未就绪，单次失败不代表没有 Hub：重试一次再兜底
      let lastErr: Error | null = null
      for (let attempt = 1; attempt <= DISCOVER_ATTEMPTS; attempt++) {
        try {
          const found = await this.discover()
          host = found.host
          port = found.port
          this.log(`[lan-hub] discovered hub at ${host}:${port} (auto${attempt > 1 ? `, attempt ${attempt}` : ''})`)
          lastErr = null
          break
        } catch (e) {
          lastErr = e as Error
          this.log(`[lan-hub] discovery attempt ${attempt}/${DISCOVER_ATTEMPTS} failed: ${lastErr.message}`)
          if (attempt < DISCOVER_ATTEMPTS) await new Promise((r) => setTimeout(r, DISCOVER_RETRY_GAP_MS))
        }
      }
      if (lastErr) {
        this.log(`[lan-hub] discovery failed, fallback ${this.fallbackHost}:${this.fallbackPort}`)
        host = this.fallbackHost
        port = this.fallbackPort
      }
    }
    const sock = net.connect(port, host)
    this.sock = sock
    // 禁用 Nagle，降低小包延时（低延时的关键一项）
    sock.setNoDelay(true)

    sock.on('connect', () => {
      this.retryDelay = 1_000
      this.sendRaw({
        type: 'register',
        v: CLIENT_PROTO,
        from: this.agentId,
        content: { name: this.agentName, caps: CLIENT_CAPS },
      })
      this.startHeartbeat()
      this.log(`[lan-hub] connected to ${host}:${port} as ${this.agentId}`)
    })

    sock.on('data', (chunk) => this.onData(chunk))
    sock.on('error', (e) => this.log(`[lan-hub] socket error: ${e.message}`))
    sock.on('close', () => this.onClose())
  }

  /** 彻底关闭并释放资源（插件卸载时调用），之后不再重连 */
  close(): void {
    this.disposed = true
    this.stopHeartbeat()
    if (this.retryTimer) {
      clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
    this.rejectAllPending(new Error('client closed'))
    if (this.sock) {
      this.sock.destroy()
      this.sock = null
    }
  }

  // ---------- 局域网自动发现（UDP 广播查询 + 单播应答） ----------

  /**
   * 通过 UDP 发现同网段的 Hub：向广播地址发 _lanhub_query，
   * 服务端 discovery 服务收到后立即单播回一个 _lanhub_beacon，
   * 从中取出 Hub 的真实 IP 与 tcp 端口。失败（超时/无 Hub）则 reject，
   * 由 connect 回退到 fallback 地址。
   */
  private discover(): Promise<DiscoveredHub> {
    return new Promise((resolve, reject) => {
      const sock = dgram.createSocket('udp4')
      let done = false
      let scanTimer: ReturnType<typeof setTimeout> | null = null
      let sentCount = 0
      let recvCount = 0
      let subsCount = 0
      const finish = (err: Error | null, val?: DiscoveredHub) => {
        if (done) return
        done = true
        if (scanTimer) clearTimeout(scanTimer)
        try { sock.close() } catch { /* ignore */ }
        if (err) reject(err)
        else resolve(val as DiscoveredHub)
      }

      // 取本机所有非内部 IPv4 网卡的 /24 前缀，用于定向广播与兜底扫描
      const subs: Array<{ self: string; prefix: string }> = []
      const ifs = os.networkInterfaces()
      for (const name of Object.keys(ifs)) {
        for (const it of (ifs[name] || [])) {
          if (!it || it.family !== 'IPv4' || it.internal) continue
          const ip = it.address.split('.').map(Number)
          if (ip.length !== 4 || ip.some((n) => !Number.isFinite(n))) continue
          subs.push({ self: it.address, prefix: ip[0] + '.' + ip[1] + '.' + ip[2] })
        }
      }
      subsCount = subs.length

      // 失败时带上诊断信息（网卡数/发包数/收包数）：启动早期网卡未就绪时 ifaces=0 可直接看出来
      const timeout = setTimeout(
        () => finish(new Error(`discovery timeout (ifaces=${subsCount}, sent=${sentCount}, recv=${recvCount})`)),
        DEFAULT_DISCOVER_TIMEOUT_MS,
      )

      sock.on('message', (msg, rinfo) => {
        recvCount++
        try {
          const b = JSON.parse(msg.toString('utf8'))
          if (b && b.type === '_lanhub_beacon' && typeof b.tcpPort === 'number') {
            clearTimeout(timeout)
            finish(null, { host: rinfo.address, port: b.tcpPort })
          }
        } catch {
          /* 非法帧忽略，继续等 */
        }
      })
      sock.on('error', (e) => { clearTimeout(timeout); finish(e) })
      sock.bind(() => {
        try { sock.setBroadcast(true) } catch { /* ignore */ }
        const q = Buffer.from(JSON.stringify({ type: '_lanhub_query' }))
        const sendTo = (ip: string) => {
          sentCount++
          try { sock.send(q, 0, q.length, this.discoverPort, ip, () => {}) } catch { /* ignore */ }
        }
        // 阶段1：全网广播 + 各网卡定向广播（AP 允许广播时最快命中）
        sendTo('255.255.255.255')
        for (const x of subs) sendTo(x.prefix + '.255')
        // 阶段2：广播被 AP 过滤时，本网段 /24 单播扫描兜底，确保 host=auto 在真机可用
        scanTimer = setTimeout(() => {
          if (done) return
          for (const x of subs) {
            for (let i = 1; i <= 254; i++) {
              const ip = x.prefix + '.' + i
              if (ip === x.self) continue
              sendTo(ip)
            }
          }
        }, 400)
      })
    })
  }

  // ---------- 发送类（fire-and-forget，服务端保证 seq 有序） ----------

  sendTo(to: string, text: string): void {
    this.sendRaw({ type: 'chat', from: this.agentId, to, content: { text } })
  }

  sendRoom(roomId: string, text: string): void {
    this.sendRaw({ type: 'room', from: this.agentId, roomId, content: { text } })
  }

  broadcast(text: string): void {
    this.sendRaw({ type: 'broadcast', from: this.agentId, content: { text } })
  }

  joinRoom(roomId: string): void {
    this.sendRaw({ type: 'join', from: this.agentId, content: { roomId } })
  }

  leaveRoom(roomId: string): void {
    this.sendRaw({ type: 'leave', from: this.agentId, content: { roomId } })
  }

  // ---------- 请求-响应式（等待服务端回执） ----------

  /** 创建房间，返回 roomId */
  async createRoom(name: string, members: string[] = []): Promise<string> {
    this.sendRaw({ type: 'create_room', from: this.agentId, content: { name, members } })
    const env = await this.waitFor(
      (e) => e.type === 'system' && e.content?.action === 'created',
      this.requestTimeoutMs,
    )
    return String(env.content?.roomId ?? '')
  }

  /** 拉取在线 Agent 列表 */
  async listAgents(): Promise<AgentInfo[]> {
    this.sendRaw({ type: 'list', from: this.agentId })
    const env = await this.waitFor(
      (e) => e.type === 'system' && e.content?.action === 'list',
      this.requestTimeoutMs,
    )
    return (env.content?.agents ?? []) as AgentInfo[]
  }

  /** 等待注册完成（socket 建立后还需等服务端回执），用于启动时的确定性等待 */
  async waitReady(timeoutMs = this.requestTimeoutMs): Promise<void> {
    if (this.ready) return
    await this.waitFor((e) => this.isRegisterAck(e), timeoutMs)
  }

  /** 协商后的协议版本（调试用） */
  get proto(): number {
    return this.negotiatedProto
  }

  /** 服务端声明的能力集（调试用） */
  get caps(): string[] {
    return this.serverCaps.slice()
  }

  // ---------- 注册回执识别与协商 ----------

  /**
   * 识别注册回执：优先匹配结构化 action，旧服务端没有该字段时回退到历史文案。
   * 双判定是为了让客户端能同时对接新旧两版服务端——任一侧先升级都不会断。
   */
  private isRegisterAck(env: Envelope): boolean {
    if (env.type !== 'system') return false
    const c = env.content as Record<string, any> | undefined
    if (!c) return false
    return (
      c.action === 'registered' ||
      (typeof c.text === 'string' && c.text.startsWith('registered as'))
    )
  }

  /** 应用服务端下发的协商结果：协议版本、能力集、心跳间隔、feature 开关 */
  private applyNegotiation(env: Envelope): void {
    const c = env.content as Record<string, any> | undefined
    if (!c) return
    const proto = Number(c.proto)
    if (Number.isFinite(proto) && proto > 0) this.negotiatedProto = proto
    if (Array.isArray(c.serverCaps)) this.serverCaps = c.serverCaps as string[]
    if (c.features && typeof c.features === 'object') this.serverFeatures = c.features as Record<string, any>

    const hb = Number(c.heartbeatMs)
    if (Number.isFinite(hb) && hb > 0 && hb !== this.heartbeatMs) {
      this.heartbeatMs = hb
      if (this.heartbeat) this.startHeartbeat() // 以服务端值为准重启心跳
    }
    // 协商结果总是记一条，便于排障时确认两端到底用的哪套契约
    const feat = Object.keys(this.serverFeatures).filter((k) => this.serverFeatures[k]).join(',')
    this.log(`[lan-hub] negotiated proto=${this.negotiatedProto} heartbeat=${this.heartbeatMs}ms features=[${feat}]`)
  }

  // ---------- 收件箱（拉取后清空） ----------

  /** Hub 是推送式的：收到的消息先进 inbox，由调用方主动拉取 */
  readMessages(): Envelope[] {
    const out = this.inbox
    this.inbox = []
    return out
  }

  peekMessages(): Envelope[] {
    return this.inbox.slice()
  }

  // ---------- 内部实现 ----------

  private sendRaw(env: Envelope): void {
    if (!this.sock) {
      throw new Error('not connected to hub')
    }
    if (!env.msgId) env.msgId = randomId()
    if (!env.ts) env.ts = Date.now()
    this.sock.write(JSON.stringify(env) + '\n')
  }

  private onData(chunk: Buffer): void {
    this.buf += chunk.toString('utf8')
    let idx: number
    while ((idx = this.buf.indexOf('\n')) >= 0) {
      const line = this.buf.slice(0, idx).trim()
      this.buf = this.buf.slice(idx + 1)
      if (!line) continue
      let env: Envelope
      try {
        env = JSON.parse(line) as Envelope
      } catch {
        continue // 非法帧直接跳过，不中断连接
      }
      this.onFrame(env)
    }
  }

  private onFrame(env: Envelope): void {
    // 0) 识别注册回执，标记就绪（必须早于 pending 匹配，否则会被 waitReady 吞掉）
    if (!this.ready && this.isRegisterAck(env)) {
      this.ready = true
      this.applyNegotiation(env)
    }
    // 1) 先喂给等待中的请求-响应式调用（回执不进收件箱，避免污染）
    for (let i = 0; i < this.pending.length; i++) {
      const p = this.pending[i]
      let matched = false
      try {
        matched = p.match(env)
      } catch {
        matched = false
      }
      if (matched) {
        clearTimeout(p.timer)
        this.pending.splice(i, 1)
        p.resolve(env)
        return
      }
    }
    // 2) 通知订阅者（用于自动回灌 Agent 上下文）
    for (const cb of this.listeners) {
      try {
        cb(env)
      } catch {
        /* 单个订阅者异常不影响主流程 */
      }
    }
    // 3) 入收件箱
    this.inbox.push(env)
    if (this.inbox.length > this.inboxLimit) {
      this.inbox.splice(0, this.inbox.length - this.inboxLimit)
    }
  }

  private waitFor(match: (env: Envelope) => boolean, timeoutMs: number): Promise<Envelope> {
    return new Promise<Envelope>((resolve, reject) => {
      const timer = setTimeout(() => {
        const i = this.pending.indexOf(entry)
        if (i >= 0) this.pending.splice(i, 1)
        reject(new Error('timeout waiting for hub response'))
      }, timeoutMs)
      const entry: Pending = { match, resolve, reject, timer }
      this.pending.push(entry)
    })
  }

  private startHeartbeat(): void {
    this.stopHeartbeat()
    this.heartbeat = setInterval(() => {
      if (!this.sock) return
      try {
        this.sock.write(JSON.stringify({ type: 'heartbeat', from: this.agentId, msgId: randomId(), ts: Date.now() }) + '\n')
      } catch {
        /* 写失败交给 close 事件处理 */
      }
    }, this.heartbeatMs)
    // 心跳不应阻止进程退出
    this.heartbeat.unref?.()
  }

  private stopHeartbeat(): void {
    if (this.heartbeat) {
      clearInterval(this.heartbeat)
      this.heartbeat = null
    }
  }

  private onClose(): void {
    this.sock = null
    this.ready = false
    this.stopHeartbeat()
    this.rejectAllPending(new Error('connection closed'))
    if (this.disposed || !this.reconnectEnabled) return
    // 指数退避重连
    const delay = this.retryDelay
    this.retryDelay = Math.min(this.retryDelay * 2, this.reconnectMaxMs)
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null
      if (!this.disposed) this.connect()
    }, delay)
    this.retryTimer.unref?.()
    this.log(`[lan-hub] disconnected, reconnecting in ${delay}ms`)
  }

  private rejectAllPending(err: Error): void {
    for (const p of this.pending) {
      clearTimeout(p.timer)
      p.reject(err)
    }
    this.pending = []
  }
}

function randomId(): string {
  return Math.random().toString(16).slice(2, 10) + Date.now().toString(16).slice(-6)
}
