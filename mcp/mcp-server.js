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
const dgram = require('dgram');
const os = require('os');

// 取本机所有非内部 IPv4 网卡的 /24 前缀，用于定向广播与兜底扫描
function localIPv4Subnets() {
  const out = [];
  const ifs = os.networkInterfaces();
  for (const name of Object.keys(ifs)) {
    for (const it of (ifs[name] || [])) {
      if (!it || it.family !== 'IPv4' || it.internal) continue;
      const ip = it.address.split('.').map(Number);
      if (ip.length !== 4 || ip.some((n) => !Number.isFinite(n))) continue;
      out.push({ self: it.address, prefix: ip[0] + '.' + ip[1] + '.' + ip[2] });
    }
  }
  return out;
}

// 局域网自动发现：向广播地址发 query，服务端单播回 beacon（含 Hub IP 与端口）
// 真机实测：部分 Wi-Fi/AP 会过滤 255.255.255.255 与子网定向广播，
// 故增加「本网段 /24 单播扫描」兜底，确保无广播环境下的 host=auto 也能发现 Hub。
const DISCOVER_PORT = parseInt(process.env.HUB_DISCOVER_PORT || '8082', 10);
function discoverHub(timeoutMs, port) {
  return new Promise((resolve, reject) => {
    const s = dgram.createSocket('udp4');
    let done = false;
    let scanTimer = null;
    const finish = (err, val) => {
      if (done) return;
      done = true;
      if (scanTimer) clearTimeout(scanTimer);
      try { s.close(); } catch (e) { /* ignore */ }
      if (err) reject(err); else resolve(val);
    };
    const t = setTimeout(() => finish(new Error('discovery timeout')), timeoutMs);
    s.on('message', (msg, rinfo) => {
      try {
        const b = JSON.parse(msg.toString());
        if (b && b.type === '_lanhub_beacon' && typeof b.tcpPort === 'number') {
          clearTimeout(t);
          finish(null, { host: rinfo.address, port: b.tcpPort });
        }
      } catch (e) { /* 非法帧忽略 */ }
    });
    s.on('error', (e) => { clearTimeout(t); finish(e); });
    s.bind(() => {
      try { s.setBroadcast(true); } catch (e) { /* ignore */ }
      const q = Buffer.from(JSON.stringify({ type: '_lanhub_query' }));
      const subs = localIPv4Subnets();
      const sendTo = (ip) => { try { s.send(q, 0, q.length, port, ip, () => {}); } catch (e) { /* ignore */ } };
      // 阶段1：全网广播 + 各网卡定向广播
      sendTo('255.255.255.255');
      for (const x of subs) sendTo(x.prefix + '.255');
      // 阶段2：广播被 AP 过滤时，本网段 /24 单播扫描兜底
      scanTimer = setTimeout(() => {
        if (done) return;
        for (const x of subs) {
          for (let i = 1; i <= 254; i++) {
            const ip = x.prefix + '.' + i;
            if (ip === x.self) continue;
            sendTo(ip);
          }
        }
      }, Math.min(800, Math.max(300, Math.floor(timeoutMs / 3))));
    });
  });
}

const HUB_HOST = process.env.HUB_HOST || '127.0.0.1';
const HUB_PORT = parseInt(process.env.HUB_PORT || '8080', 10);
const AGENT_ID = process.env.HUB_AGENT_ID || 'mcp-agent';
const AGENT_NAME = process.env.HUB_AGENT_NAME || 'MCP Agent';

// 支持的 MCP 协议版本：initialize 时优先回显客户端支持的版本，最大化兼容性
const SUPPORTED_VERSIONS = ['2024-11-05', '2025-03-26', '2025-06-18'];
const DEFAULT_VERSION = '2025-03-26';

const inbox = []; // Hub 推来的消息，供 read_messages 拉取

// ---------------- Hub 连接 ----------------
let sock = null;
let hbuf = '';

// 客户端声明的协议版本与能力集；服务端据此决定是否下发新字段（能力协商）
const CLIENT_PROTO = 3;
const CLIENT_CAPS = ['structured-ack', 'heartbeat-negotiation', 'discovery', 'agent-caps', 'endpoints'];

let heartbeatMs = 15000;
let heartbeatTimer = null;

function startHeartbeat() {
  if (heartbeatTimer) clearInterval(heartbeatTimer);
  heartbeatTimer = setInterval(() => send({ type: 'heartbeat', from: AGENT_ID }), heartbeatMs);
  if (heartbeatTimer.unref) heartbeatTimer.unref();
}

// 服务端在注册回执里下发心跳间隔时以它为准，避免两端硬编码不一致
function applyNegotiatedHeartbeat(ms) {
  const n = Number(ms);
  if (Number.isFinite(n) && n > 0 && n !== heartbeatMs) {
    heartbeatMs = n;
    startHeartbeat();
  }
  // 协商结果总是记一条，便于排障时确认两端到底用的哪套契约
  process.stderr.write('[lan-hub-mcp] heartbeat interval -> ' + heartbeatMs + 'ms\n');
}

function isRegisterAck(env) {
  if (!env || env.type !== 'system' || !env.content) return false;
  const c = env.content;
  // 优先用结构化 action；旧服务端没有该字段时回退到历史文案
  return c.action === 'registered' ||
    (typeof c.text === 'string' && c.text.startsWith('registered as'));
}

function attachHandlers() {
sock.on('data', (chunk) => {
  hbuf += chunk.toString('utf8');
  let idx;
  while ((idx = hbuf.indexOf('\n')) >= 0) {
    const line = hbuf.slice(0, idx).trim();
    hbuf = hbuf.slice(idx + 1);
    if (!line) continue;
    try {
      const env = JSON.parse(line);
      if (isRegisterAck(env)) applyNegotiatedHeartbeat(env.content.heartbeatMs);
      inbox.push(env);
    } catch (e) {
      process.stderr.write('bad hub line: ' + e.message + '\n');
    }
  }
});

sock.on('error', (e) => process.stderr.write('hub socket error: ' + e.message + '\n'));

sock.on('connect', () => {
  send({
    type: 'register',
    v: CLIENT_PROTO,
    from: AGENT_ID,
    content: { name: AGENT_NAME, caps: CLIENT_CAPS }
  });
  startHeartbeat();
});
}

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
async function main() {
  let host = HUB_HOST;
  let port = HUB_PORT;
  if (HUB_HOST === 'auto' || HUB_HOST === '') {
    try {
      const f = await discoverHub(2000, DISCOVER_PORT);
      host = f.host;
      port = f.port;
      process.stderr.write('[lan-hub-mcp] discovered hub at ' + host + ':' + port + '\n');
    } catch (e) {
      process.stderr.write('[lan-hub-mcp] discovery failed: ' + e.message + ', fallback 127.0.0.1:8080\n');
      host = '127.0.0.1';
      port = 8080;
    }
  }
  sock = net.connect(port, host);
  attachHandlers();

  const argv = process.argv.slice(2);
  if (argv[0] === '--http') {
    const p = parseInt(argv[1] || process.env.MCP_HTTP_PORT || '8082', 10);
    runHTTP(p);
  } else {
    runStdio();
  }
}

main();
