/**
 * dsh-plugin-lan-hub —— 让 DeepSeek Harness (dsh) 的 Agent 接入 LAN Agent Hub
 *
 * 插件形态：函数式 Cordis 插件，导出 name / inject / Config / apply。
 * 加载后注册一组模型可调用的 tools，Agent 即可与局域网内
 * Skill / MCP / HTTP / 其它 DSH 实例接入的 Agent 互通。
 *
 * 生命周期：连接通过 ctx.effect() 注册，插件卸载（含 HMR 热替换）时自动断开。
 */

import type { Context } from '@deepseek-ai/cordis'
import { defineTool } from '@deepseek-ai/dsh-tools'
import Schema from '@deepseek-ai/schemastery'
import { HubClient, type Envelope } from './hub-client.ts'

export const name = 'lan-hub'

/** 声明依赖：等工具注册表就绪后再加载本插件 */
export const inject = ['tools']

export interface Config {
  host: string
  port: number
  agentId: string
  agentName: string
  heartbeatMs: number
  reconnect: boolean
  requestTimeoutMs: number
  inboxLimit: number
  /** 把收到的消息自动注入 Agent 上下文（DSH 特有能力，MCP 形态做不到） */
  autoInject: boolean
}

export const Config: Schema<Config> = Schema.object({
  host: Schema.string().default('127.0.0.1').description('Hub 地址'),
  port: Schema.number().default(8080).description('Hub TCP 端口'),
  agentId: Schema.string().default('dsh-agent').description('本 Agent 在 Hub 中的唯一 ID'),
  agentName: Schema.string().default('HarnessAgent').description('展示名'),
  heartbeatMs: Schema.number().default(15000).description('心跳间隔（毫秒）'),
  reconnect: Schema.boolean().default(true).description('断线自动重连'),
  requestTimeoutMs: Schema.number().default(3000).description('请求响应超时（毫秒）'),
  inboxLimit: Schema.number().default(200).description('收件箱上限'),
  autoInject: Schema.boolean().default(false).description('收到消息时自动注入 Agent 上下文'),
  /** 局域网自动发现：把 host 设为 "auto" 即启用，无需手动填 IP */
  autoDiscover: Schema.boolean().default(false).description('host 为 auto 时启用 UDP 局域网自动发现'),
  discoverPort: Schema.number().default(8082).description('Hub 发现服务 UDP 端口（与服务端 -discover 一致）'),
  /** 发现失败时的兜底地址；留空则用 127.0.0.1（手机上等于手机自己，建议显式填 Hub 局域网 IP） */
  fallbackHost: Schema.string().default('').description('自动发现失败时的兜底 Hub 地址'),
})

export function apply(ctx: Context, config: Config) {
  const client = new HubClient({
    host: config.host,
    port: config.port,
    agentId: config.agentId,
    agentName: config.agentName,
    heartbeatMs: config.heartbeatMs,
    reconnect: config.reconnect,
    requestTimeoutMs: config.requestTimeoutMs,
    inboxLimit: config.inboxLimit,
    autoDiscover: config.host === 'auto' || config.autoDiscover,
    discoverPort: config.discoverPort,
    fallbackHost: config.fallbackHost || '127.0.0.1',
    fallbackPort: config.port,
    log: (m) => console.error(m),
  })

  // 插件卸载 / HMR 热替换时自动断开连接
  ctx.effect(() => {
    client.connect()
    return () => client.close()
  })

  /**
   * 把新消息回灌给 Agent 上下文。
   *
   * exec.agent.inject() 会在下一次模型请求时追加持久上下文；
   * 注意它不是唤醒机制（空闲的 Agent 不会因此被唤醒），
   * 所以主动拉取仍由 hub_read_messages 承担。
   *
   * 这里用运行时可选调用：不同 dsh 版本的 agent 句柄形状可能不同，
   * 取不到就静默降级，绝不让工具调用失败。
   */
  async function injectIntoAgent(exec: any, envs: Envelope[]): Promise<void> {
    if (!config.autoInject || envs.length === 0) return
    const text = formatEnvelopes(envs)
    try {
      await exec?.agent?.inject?.({
        content: [{ type: 'text', text }],
        source: { kind: 'lan-hub' },
      })
    } catch {
      /* 版本不兼容时静默降级 */
    }
  }

  // ---------------------------------------------------------------
  // 工具 1：点对点发送
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_send',
    description: 'Send a direct message to another agent on the LAN hub.',
    parameters: {
      to: { type: 'string', required: true, description: 'Target agent id' },
      text: { type: 'string', required: true, description: 'Message text' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      try {
        client.sendTo(args.to, args.text)
        return `sent to ${args.to}`
      } catch (e) {
        return `failed: ${(e as Error).message}`
      }
    },
  }))

  // ---------------------------------------------------------------
  // 工具 2：房间消息
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_send_room',
    description: 'Send a message to a chat room on the LAN hub.',
    parameters: {
      room: { type: 'string', required: true, description: 'Room id' },
      text: { type: 'string', required: true, description: 'Message text' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      try {
        client.sendRoom(args.room, args.text)
        return `sent to room ${args.room}`
      } catch (e) {
        return `failed: ${(e as Error).message}`
      }
    },
  }))

  // ---------------------------------------------------------------
  // 工具 3：全员广播
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_broadcast',
    description: 'Broadcast a message to every agent on the LAN hub.',
    parameters: {
      text: { type: 'string', required: true, description: 'Message text' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      try {
        client.broadcast(args.text)
        return 'broadcast sent'
      } catch (e) {
        return `failed: ${(e as Error).message}`
      }
    },
  }))

  // ---------------------------------------------------------------
  // 工具 4：创建房间
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_create_room',
    description: 'Create a chat room on the LAN hub and return its id.',
    parameters: {
      name: { type: 'string', required: true, description: 'Room name' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      try {
        const id = await client.createRoom(args.name)
        return id || 'failed: no roomId returned'
      } catch (e) {
        return `failed: ${(e as Error).message}`
      }
    },
  }))

  // ---------------------------------------------------------------
  // 工具 5：加入房间
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_join_room',
    description: 'Join a chat room on the LAN hub.',
    parameters: {
      room: { type: 'string', required: true, description: 'Room id' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      try {
        client.joinRoom(args.room)
        return `joined ${args.room}`
      } catch (e) {
        return `failed: ${(e as Error).message}`
      }
    },
  }))

  // ---------------------------------------------------------------
  // 工具 6：离开房间
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_leave_room',
    description: 'Leave a chat room on the LAN hub.',
    parameters: {
      room: { type: 'string', required: true, description: 'Room id' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      try {
        client.leaveRoom(args.room)
        return `left ${args.room}`
      } catch (e) {
        return `failed: ${(e as Error).message}`
      }
    },
  }))

  // ---------------------------------------------------------------
  // 工具 7：在线列表
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_list_agents',
    description: 'List agents currently online on the LAN hub.',
    parameters: {},
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute() {
      try {
        const list = await client.listAgents()
        if (list.length === 0) return 'no agents online'
        return list.map((a) => `${a.id}${a.name ? ` (${a.name})` : ''}`).join(', ')
      } catch (e) {
        return `failed: ${(e as Error).message}`
      }
    },
  }))

  // ---------------------------------------------------------------
  // 工具 8：拉取消息（Hub 是推送式的，靠它主动取）
  // ---------------------------------------------------------------
  ctx.tools.register(defineTool({
    name: 'hub_read_messages',
    description: 'Drain messages received from the LAN hub since the last read.',
    parameters: {},
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(_args, exec) {
      const msgs = client.readMessages()
      await injectIntoAgent(exec, msgs)
      if (msgs.length === 0) return 'no new messages'
      return formatEnvelopes(msgs)
    },
  }))
}

// -------------------------------------------------------------------
// 格式化：把信封渲染成模型易读的一行行文本
// -------------------------------------------------------------------
function formatEnvelopes(envs: Envelope[]): string {
  return envs
    .map((e) => {
      const c = (e.content ?? {}) as Record<string, any>
      switch (e.type) {
        case 'chat':
          return `[chat] ${e.from} -> ${e.to}: ${c.text ?? ''}`
        case 'room':
          return `[room ${e.roomId}] ${e.from}: ${c.text ?? ''}`
        case 'broadcast':
          return `[broadcast] ${e.from}: ${c.text ?? ''}`
        case 'presence':
          return `[presence] ${c.id} is ${c.status}`
        case 'system':
          return `[system] ${c.text ?? JSON.stringify(c)}`
        default:
          return `[${e.type ?? 'unknown'}] ${JSON.stringify(c)}`
      }
    })
    .join('\n')
}
