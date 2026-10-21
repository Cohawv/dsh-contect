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

export class HubClient {
  private readonly host: string
  private readonly port: number
  private readonly agentId: string
  private readonly agentName: string
  private readonly heartbeatMs: number
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

  /** 建立连接；重连逻辑内置，调用方无需关心 */
  connect(): void {
    if (this.disposed) return
    const sock = net.connect(this.port, this.host)
    this.sock = sock
    // 禁用 Nagle，降低小包延时（低延时的关键一项）
    sock.setNoDelay(true)

    sock.on('connect', () => {
      this.retryDelay = 1_000
      this.sendRaw({ type: 'register', from: this.agentId, content: { name: this.agentName } })
      this.startHeartbeat()
      this.log(`[lan-hub] connected to ${this.host}:${this.port} as ${this.agentId}`)
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
    await this.waitFor(
      (e) => e.type === 'system' && typeof e.content?.text === 'string' && e.content.text.startsWith('registered as'),
      timeoutMs,
    )
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
    if (
      !this.ready &&
      env.type === 'system' &&
      typeof env.content?.text === 'string' &&
      env.content.text.startsWith('registered as')
    ) {
      this.ready = true
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
