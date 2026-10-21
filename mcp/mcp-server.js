#!/usr/bin/env node
'use strict';

// LAN Agent Hub → MCP 服务（Node，零第三方依赖）
//
// 支持两种传输，共用同一套工具逻辑与 Hub 连接：
//   1) stdio  （默认）—— 本地子进程，供桌面 MCP host 启动
//   2) http   （--http [port]）—— MCP Streamable HTTP，供安卓 / 远程 MCP 客户端连接
//
// 用法：
//   node mcp-server.js                 # stdio（桌面 MCP host 用）
//   node mcp-server.js --http 8082     # Streamable HTTP，端点 http://<ip>:8082/mcp

const net = require('net');
const http = require('http');
const crypto = require('crypto');

const HUB_HOST = process.env.HUB_HOST || '127.0.0.1';
const HUB_PORT = parseInt(process.env.HUB_PORT || '8080', 10);
const AGENT_ID = process.env.HUB_AGENT_ID || 'mcp-agent';
const AGENT_NAME = process.env.HUB_AGENT_NAME || 'MCP Agent';

// 支持的 MCP 协议版本：initialize 时优先回显客户端支持的版本，最大化兼容性
const SUPPORTED_VERSIONS = ['2024-11-05', '2025-03-26', '2025-06-18'];
const DEFAULT_VERSION = '2025-03-26';

const inbox = []; // Hub 推来的消息，供 read_messages 拉取

// ---------------- Hub 连接 ----------------
const sock = net.connect(HUB_PORT, HUB_HOST);
let hbuf = '';

sock.on('data', (chunk) => {
  hbuf += chunk.toString('utf8');
  let idx;
  while ((idx = hbuf.indexOf('\n')) >= 0) {
    const line = hbuf.slice(0, idx).trim();
    hbuf = hbuf.slice(idx + 1);
    if (!line) continue;
    try {
      inbox.push(JSON.parse(line));
    } catch (e) {
      process.stderr.write('bad hub line: ' + e.message + '\n');
    }
  }
});

sock.on('error', (e) => process.stderr.write('hub socket error: ' + e.message + '\n'));

sock.on('connect', () => {
  send({ type: 'register', from: AGENT_ID, content: { name: AGENT_NAME } });
  setInterval(() => send({ type: 'heartbeat', from: AGENT_ID }), 15000);
});

function send(env) {
  sock.write(JSON.stringify(env) + '\n');
}

// 等待某个符合条件的回执（create_room / list_agents 这类需要返回值的调用）
function waitFor(pred, timeoutMs = 1500) {
  return new Promise((resolve) => {
    const start = Date.now();
    const tick = () => {
      const i = inbox.findIndex(pred);
      if (i >= 0) {
        resolve(inbox.splice(i, 1)[0]);
        return;
      }
      if (Date.now() - start >= timeoutMs) {
        resolve(null);
        return;
      }
      setTimeout(tick, 20);
    };
    tick();
  });
}

// ---------------- 工具定义 ----------------
const TOOLS = [
  {
    name: 'send_message',
    description: '给指定 Agent 发送点对点私聊消息',
    inputSchema: {
      type: 'object',
      properties: {
        to: { type: 'string', description: '目标 agent id' },
        text: { type: 'string', description: '消息文本' }
      },
      required: ['to', 'text']
    }
  },
  {
    name: 'send_room_message',
    description: '往指定聊天室发送消息',
    inputSchema: {
      type: 'object',
      properties: {
        roomId: { type: 'string', description: '聊天室 id' },
        text: { type: 'string', description: '消息文本' }
      },
      required: ['roomId', 'text']
    }
  },
  {
    name: 'create_room',
    description: '创建聊天室，返回 roomId',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: '聊天室名称' },
        members: { type: 'array', items: { type: 'string' }, description: '初始成员 agent id 列表（可选）' }
      },
      required: ['name']
    }
  },
  {
    name: 'join_room',
    description: '加入聊天室',
    inputSchema: {
      type: 'object',
      properties: { roomId: { type: 'string', description: '聊天室 id' } },
      required: ['roomId']
    }
  },
  {
    name: 'leave_room',
    description: '离开聊天室',
    inputSchema: {
      type: 'object',
      properties: { roomId: { type: 'string', description: '聊天室 id' } },
      required: ['roomId']
    }
  },
  {
    name: 'list_agents',
    description: '查看当前在线的 Agent 列表',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'broadcast',
    description: '向所有在线 Agent 广播消息',
    inputSchema: {
      type: 'object',
      properties: { text: { type: 'string', description: '消息文本' } },
      required: ['text']
    }
  },
  {
    name: 'read_messages',
    description: '读取并清空本 Agent 收到的消息（私聊 / 聊天室 / 系统 / 上下线）',
    inputSchema: {
      type: 'object',
      properties: { limit: { type: 'number', description: '最多返回条数，默认 50' } }
    }
  }
];

async function callTool(name, args) {
  switch (name) {
    case 'send_message': {
      if (!args.to || !args.text) throw new Error('need to/text');
      send({ type: 'chat', from: AGENT_ID, to: args.to, content: { text: args.text } });
      return '已发送给 ' + args.to + '：' + args.text;
    }
    case 'send_room_message': {
      if (!args.roomId || !args.text) throw new Error('need roomId/text');
      send({ type: 'room', from: AGENT_ID, roomId: args.roomId, content: { text: args.text } });
      return '已发到聊天室 ' + args.roomId + '：' + args.text;
    }
    case 'create_room': {
      send({ type: 'create_room', from: AGENT_ID, content: { name: args.name, members: args.members || [] } });
      const m = await waitFor((e) => e.type === 'system' && e.content && e.content.action === 'created');
      return m ? '已创建聊天室，roomId = ' + m.content.roomId : '等待 roomId 回执超时';
    }
    case 'join_room': {
      if (!args.roomId) throw new Error('need roomId');
      send({ type: 'join', from: AGENT_ID, content: { roomId: args.roomId } });
      return '已加入聊天室 ' + args.roomId;
    }
    case 'leave_room': {
      if (!args.roomId) throw new Error('need roomId');
      send({ type: 'leave', from: AGENT_ID, content: { roomId: args.roomId } });
      return '已离开聊天室 ' + args.roomId;
    }
    case 'list_agents': {
      send({ type: 'list', from: AGENT_ID });
      const m = await waitFor((e) => e.type === 'system' && e.content && e.content.action === 'list');
      return m ? JSON.stringify(m.content.agents) : '等待在线列表回执超时';
    }
    case 'broadcast': {
      if (!args.text) throw new Error('need text');
      send({ type: 'broadcast', from: AGENT_ID, content: { text: args.text } });
      return '已广播：' + args.text;
    }
    case 'read_messages': {
      const limit = args.limit || 50;
      const out = inbox.splice(0, limit);
      return out.length ? out.map((o) => JSON.stringify(o)).join('\n') : '(暂无新消息)';
    }
    default:
      throw new Error('unknown tool: ' + name);
  }
}

// ---------------- 核心：JSON-RPC 分发（与传输无关） ----------------
// 返回：响应对象；通知类返回 null（不回复）
async function handleRpc(req) {
  const id = req.id;
  const method = req.method;
  const params = req.params || {};

  if (method === 'initialize') {
    const reqVer = params.protocolVersion;
    const version = SUPPORTED_VERSIONS.includes(reqVer) ? reqVer : DEFAULT_VERSION;
    return {
      jsonrpc: '2.0',
      id,
      result: {
        protocolVersion: version,
        capabilities: { tools: {} },
        serverInfo: { name: 'lan-hub', version: '0.2.0' }
      }
    };
  }

  if (method === 'tools/list') return { jsonrpc: '2.0', id, result: { tools: TOOLS } };

  if (method === 'tools/call') {
    const name = params.name;
    const args = params.arguments || {};
    try {
      const text = await callTool(name, args);
      return { jsonrpc: '2.0', id, result: { content: [{ type: 'text', text }] } };
    } catch (e) {
      return { jsonrpc: '2.0', id, error: { code: -32000, message: String((e && e.message) || e) } };
    }
  }

  if (method === 'ping') return { jsonrpc: '2.0', id, result: {} };

  if (id === undefined) return null; // 通知，不回复

  return { jsonrpc: '2.0', id, error: { code: -32601, message: 'method not found: ' + method } };
}

// ---------------- 传输一：stdio（桌面 MCP host） ----------------
function runStdio() {
  let pendingCount = 0;
  let stdinEnded = false;
  const maybeExit = () => {
    if (stdinEnded && pendingCount === 0) process.exit(0);
  };

  let sbuf = '';
  process.stdin.on('data', (chunk) => {
    sbuf += chunk.toString('utf8');
    let idx;
    while ((idx = sbuf.indexOf('\n')) >= 0) {
      const line = sbuf.slice(0, idx).trim();
      sbuf = sbuf.slice(idx + 1);
      if (!line) continue;
      let msg;
      try {
        msg = JSON.parse(line);
      } catch (e) {
        process.stderr.write('bad rpc: ' + e.message + '\n');
        continue;
      }
      pendingCount++;
      handleRpc(msg)
        .then((resp) => {
          if (resp) process.stdout.write(JSON.stringify(resp) + '\n');
        })
        .catch((e) => process.stderr.write('rpc error: ' + e.message + '\n'))
        .finally(() => {
          pendingCount--;
          maybeExit();
        });
    }
  });

  process.stdin.on('end', () => {
    stdinEnded = true;
    maybeExit();
  });
}

// ---------------- 传输二：MCP Streamable HTTP（安卓 / 远程） ----------------
const sessions = new Map();

function runHTTP(port) {
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://localhost');
    const commonHeaders = { 'MCP-Protocol-Version': DEFAULT_VERSION };

    if (url.pathname !== '/mcp') {
      res.writeHead(404, { ...commonHeaders, 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'not found' }));
      return;
    }

    // 会话终止
    if (req.method === 'DELETE') {
      const sid = req.headers['mcp-session-id'];
      if (sid) sessions.delete(sid);
      res.writeHead(200, commonHeaders);
      res.end();
      return;
    }

    // 服务端主动推送的 SSE 流：MVP 暂不支持，客户端可用 read_messages 拉取
    if (req.method === 'GET') {
      res.writeHead(405, { ...commonHeaders, 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'server-initiated SSE stream not supported; use tools/call read_messages' }));
      return;
    }

    if (req.method !== 'POST') {
      res.writeHead(405, { ...commonHeaders, Allow: 'POST, GET, DELETE', 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'method not allowed' }));
      return;
    }

    let raw = '';
    for await (const chunk of req) raw += chunk;

    let msg;
    try {
      msg = JSON.parse(raw);
    } catch (e) {
      res.writeHead(400, { ...commonHeaders, 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error' } }));
      return;
    }

    let resp;
    try {
      resp = await handleRpc(msg);
    } catch (e) {
      resp = { jsonrpc: '2.0', id: msg.id, error: { code: -32603, message: String((e && e.message) || e) } };
    }

    const headers = { ...commonHeaders, 'Content-Type': 'application/json' };

    // initialize 时分配会话 ID（MCP Streamable HTTP 要求放在响应头）
    if (msg.method === 'initialize') {
      const sid = crypto.randomUUID();
      sessions.set(sid, { id: sid, createdAt: Date.now() });
      headers['Mcp-Session-Id'] = sid;
    }

    // 通知类：202 无响应体
    if (!resp) {
      res.writeHead(202, headers);
      res.end();
      return;
    }

    res.writeHead(200, headers);
    res.end(JSON.stringify(resp));
  });

  server.listen(port, () => {
    process.stderr.write('[mcp] Streamable HTTP listening on http://0.0.0.0:' + port + '/mcp\n');
  });
}

// ---------------- 入口 ----------------
function main() {
  const argv = process.argv.slice(2);
  if (argv[0] === '--http') {
    const p = parseInt(argv[1] || process.env.MCP_HTTP_PORT || '8082', 10);
    runHTTP(p);
  } else {
    runStdio();
  }
}

main();
