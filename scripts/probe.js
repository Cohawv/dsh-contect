#!/usr/bin/env node
'use strict';
// 协议探针：以指定协议版本注册，打印服务端下发的回执，用于验证版本协商与能力协商。
// 用法：node scripts/probe.js [port] [protoVersion] [agentId]
//   例：node scripts/probe.js 8099 0 legacy-client   （模拟老客户端：不声明 v）
//       node scripts/probe.js 8099 2 modern-client   （声明 v=2）
const net = require('net');

const port = parseInt(process.argv[2] || '8099', 10);
const v = parseInt(process.argv[3] || '0', 10);
const agentId = process.argv[4] || 'probe';

const sock = net.connect(port, '127.0.0.1');
let buf = '';

sock.on('data', (chunk) => {
  buf += chunk.toString('utf8');
  let i;
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i).trim();
    buf = buf.slice(i + 1);
    if (line) console.log(line);
  }
});
sock.on('error', (e) => { console.error('socket error: ' + e.message); process.exit(1); });

sock.on('connect', () => {
  const env = { type: 'register', from: agentId, content: { name: 'Probe-' + agentId } };
  if (v > 0) {
    env.v = v;
    env.content.caps = ['structured-ack', 'heartbeat-negotiation'];
  }
  sock.write(JSON.stringify(env) + '\n');
  setTimeout(() => { sock.end(); process.exit(0); }, 800);
});
