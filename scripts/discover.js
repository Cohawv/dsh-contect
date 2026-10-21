#!/usr/bin/env node
'use strict';
// 局域网 Hub 发现工具（零依赖）。
// 用法：
//   node scripts/discover.js                 # 监听 UDP 广播 beacon（默认 5s）
//   node scripts/discover.js 8082 3000      # 指定端口与超时(ms)
//   node scripts/discover.js --query        # 额外主动发一次 query（更快拿到单播应答）
//
// 输出：找到的每个 Hub 的 JSON beacon（含 hubId / ip / tcpPort / httpPort / proto / caps）。

const dgram = require('dgram');

const PORT = parseInt(process.argv[2] || process.env.HUB_DISCOVER_PORT || '8082', 10);
const TIMEOUT = parseInt(process.argv[3] || '5000', 10);
const DO_QUERY = process.argv.includes('--query');

const seen = new Map();

function show(b, ip) {
  const key = b.hubId + '@' + ip + ':' + b.tcpPort;
  if (seen.has(key)) return;
  seen.set(key, true);
  const out = {
    hubId: b.hubId,
    instanceId: b.instanceId,
    name: b.name,
    ip,
    tcpPort: b.tcpPort,
    httpPort: b.httpPort,
    proto: b.proto,
    caps: b.caps,
  };
  console.log(JSON.stringify(out));
}

function main() {
  const sock = dgram.createSocket('udp4');

  sock.on('message', (msg, rinfo) => {
    try {
      const b = JSON.parse(msg.toString('utf8'));
      if (b && b.type === '_lanhub_beacon') show(b, rinfo.address);
    } catch (e) { /* 忽略非法帧 */ }
  });

  sock.on('error', (e) => { console.error('socket error: ' + e.message); process.exit(1); });

  if (DO_QUERY) {
    // 查询模式：绑定随机端口发 query，避免与本机已运行的 Hub discovery 端口冲突
    sock.bind(() => {
      try { sock.setBroadcast(true); } catch (e) { /* ignore */ }
      console.error('[discover] query mode, sending to udp/' + PORT);
      const q = Buffer.from(JSON.stringify({ type: '_lanhub_query' }));
      sock.send(q, 0, q.length, PORT, '255.255.255.255', (err) => {
        if (err) console.error('[discover] query send failed: ' + err.message);
      });
    });
  } else {
    // 被动模式：绑定 discovery 端口监听广播 beacon（真机/跨机使用，不与本机 Hub 冲突）
    sock.bind(PORT, () => {
      try { sock.setBroadcast(true); } catch (e) { /* ignore */ }
      console.error('[discover] listening on udp/' + PORT + ' (passive)');
    });
  }

  setTimeout(() => {
    console.error('[discover] done, found ' + seen.size + ' hub(s)');
    sock.close();
    process.exit(0);
  }, TIMEOUT);
}

main();
