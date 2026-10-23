#!/usr/bin/env node
/**
 * lanhub-publish —— 把构建产物签名为可分发制品，并生成更新清单。
 *
 * 这是更新通道的**发布侧**。它做四件事：算 sha256 → 用私钥签名 → 复制制品到
 * updates/pkg/ → 写 updates/lanhub-update.json。真正的逻辑在 src/publish.mjs，
 * 本文件只负责参数解析、报错与输出——和 lanhub-release 共用同一份核心，避免
 * "发布"这个动作有两个真相。
 *
 * 与 lanhub-update（消费侧）的分工：
 *   本工具持有私钥，是把"可信"注入分发链的地方；
 *   lanhub-update 持有公钥，是判断"是否可信"的地方。
 * 两者永不共享私钥——客户端只认公钥，所以 Hub 被投毒也装不进去。
 *
 * 用法：
 *   lanhub-publish bin/dsh-plugin-lan-hub-0.7.0.tgz --key ~/.dsh/lanhub-ed25519.key.pem
 *   lanhub-publish --scan bin --key <key>            # 批量：目录下所有 *.tgz
 *   lanhub-publish <tgz> --key <key> --latest 0.7.0  # 显式指定 latest
 *   lanhub-publish <tgz> --key <key> --notes "修复 X"
 *
 * 通用参数：
 *   --key <pem>      私钥路径（或 $LAN_HUB_UPDATE_PRIVATE_KEY）
 *   --out <dir>      分发目录（默认 ./updates 或 $LANHUB_UPDATE_DIR）
 *   --latest <ver>   显式指定 latest；默认是清单中版本号最大者
 *   --proto <n>      制品要求的线协议版本（默认 3）
 *   --min-host <ver> 要求的最低宿主版本
 *   --notes <text>   变更说明（写入对应版本）
 *   --replace        允许覆盖同名已发布的版本
 *   --rotate-to <pub.pem>  声明下一把公钥：由**当前私钥**签名背书后写进清单，
 *                          客户端凭当前 pin 的旧公钥验过后接力（密钥轮换链）
 *   --revoke <id>    吊销一把密钥（keyId，8 位 hex）。可重复。用**当前私钥**签名
 *                          吊销声明写入清单 revocations；客户端验过才并入本地，恶意
 *                          Hub 无法凭空吊销诚实钥匙。泄露的私钥就用它作废。
 *   --json           输出 JSON
 *
 * 退出码：0 成功；1 错误；2 参数或密钥问题。
 */
import fs from 'node:fs'
import path from 'node:path'
import { publishArtifacts } from '../src/publish.mjs'

const opt = {
  key: process.env.LAN_HUB_UPDATE_PRIVATE_KEY || '',
  out: process.env.LANHUB_UPDATE_DIR || 'updates',
  latest: '',
  proto: 3,
  minHost: '0.2.0',
  notes: '',
  scan: '',
  replace: false,
  rotateTo: '',
  revoke: [],
  json: false,
  help: false,
}
const files = []
{
  const argv = process.argv.slice(2)
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i]
    if (a === '--key') opt.key = argv[++i] ?? ''
    else if (a.startsWith('--key=')) opt.key = a.slice(6)
    else if (a === '--out') opt.out = argv[++i] ?? ''
    else if (a.startsWith('--out=')) opt.out = a.slice(5)
    else if (a === '--latest') opt.latest = argv[++i] ?? ''
    else if (a === '--proto') opt.proto = Number(argv[++i] ?? 3)
    else if (a === '--min-host') opt.minHost = argv[++i] ?? '0.2.0'
    else if (a === '--notes') opt.notes = argv[++i] ?? ''
    else if (a === '--scan') opt.scan = argv[++i] ?? 'bin'
    else if (a.startsWith('--scan=')) opt.scan = a.slice(7)
    else if (a === '--rotate-to') opt.rotateTo = argv[++i] ?? ''
    else if (a.startsWith('--rotate-to=')) opt.rotateTo = a.slice(12)
    else if (a === '--revoke') opt.revoke.push(argv[++i] ?? '')
    else if (a.startsWith('--revoke=')) opt.revoke.push(a.slice(9))
    else if (a === '--replace') opt.replace = true
    else if (a === '--json') opt.json = true
    else if (a === '-h' || a === '--help') opt.help = true
    else if (!a.startsWith('-')) files.push(a)
  }
}

if (opt.help) {
  process.stdout.write(
    [
      'lanhub-publish —— 把构建产物签名为可分发制品并生成更新清单',
      '',
      '用法：lanhub-publish <tgz...> --key <私钥.pem> [--out <dir>] [--latest <ver>]',
      '      lanhub-publish --scan <dir> --key <私钥.pem>',
      '',
      '参数：--key <pem>      私钥路径（或 $LAN_HUB_UPDATE_PRIVATE_KEY）',
      '      --out <dir>      分发目录（默认 ./updates 或 $LANHUB_UPDATE_DIR）',
      '      --latest <ver>   显式指定 latest（默认取版本号最大者）',
      '      --proto <n>      制品要求的线协议版本（默认 3）',
      '      --min-host <ver> 要求的最低宿主版本',
      '      --notes <text>   变更说明',
      '      --replace        允许覆盖已发布的同名版本',
      '      --scan <dir>     批量扫描目录下的 *.tgz',
      '      --rotate-to <pub.pem>  声明下一把公钥（当前私钥背书，客户端接力）',
      '      --revoke <id>    吊销密钥（8 位 hex keyId，可重复；当前私钥背书）',
      '      --json           输出 JSON',
      '',
      '私钥只在构建机；公钥 pin 到客户端（lanhub-update --pubkey）。',
      '',
    ].join('\n'),
  )
  process.exit(0)
}

function die(msg, code = 1) {
  if (opt.json) process.stdout.write(JSON.stringify({ ok: false, error: msg }, null, 2) + '\n')
  else process.stderr.write(`ERROR: ${msg}\n`)
  process.exit(code)
}

function emit(data, lines) {
  process.stdout.write(opt.json ? JSON.stringify(data, null, 2) + '\n' : lines.join('\n') + '\n')
}

// ---------- 入参 ----------
if (!opt.key) {
  die(
    '缺少私钥：--key <pem> 或环境变量 LAN_HUB_UPDATE_PRIVATE_KEY\n（密钥用 lanhub-keygen 生成；私钥只在构建机，不要分发）',
    2,
  )
}
let privateKeyPem
try {
  privateKeyPem = fs.readFileSync(opt.key, 'utf8')
} catch (e) {
  die(`读不到私钥 ${opt.key}：${e.message}`, 2)
}
if (!privateKeyPem.includes('PRIVATE KEY')) die(`${opt.key} 不是私钥 PEM`, 2)

if (opt.scan) {
  const dir = path.resolve(opt.scan)
  if (!fs.existsSync(dir)) die(`扫描目录不存在：${dir}`, 2)
  for (const f of fs.readdirSync(dir)) {
    if (f.endsWith('.tgz')) files.push(path.join(dir, f))
  }
}
if (!files.length) die('没有制品：给 .tgz 路径，或用 --scan <dir> 扫描目录', 2)

const outDir = path.resolve(opt.out)
let result
try {
  result = publishArtifacts({
    files,
    privateKeyPem,
    outDir,
    latest: opt.latest,
    proto: opt.proto,
    minHost: opt.minHost,
    notes: opt.notes,
    replace: opt.replace,
    pubkeyHint: process.env.LAN_HUB_UPDATE_PUBKEY || '',
    rotateTo: opt.rotateTo,
    revoke: opt.revoke,
  })
} catch (e) {
  die(e.message)
}

emit(
  {
    ok: true,
    manifest: result.manifestPath,
    latest: result.manifest.latest,
    published: result.published,
    skipped: result.skipped,
    nextKey: result.manifest.nextKey ? { keyId: result.manifest.nextKey.keyId } : null,
  },
  [
    `发布目录：${outDir}`,
    `清单：${result.manifestPath}`,
    `latest：${result.manifest.latest}`,
    '',
    ...result.published.map((p) => `  ✅ ${p.version}  ${String(p.sha256).slice(0, 16)}…  sig=${p.sig.slice(0, 12)}…`),
    ...(result.manifest.nextKey
      ? ['', `  🔑 已声明下一把公钥：keyId=${result.manifest.nextKey.keyId}（客户端验过后接力）`]
      : []),
    ...(result.manifest.revocations && result.manifest.revocations.length
      ? ['', `  ⛔ 已写入吊销声明：${result.manifest.revocations.map((r) => r.id).join(', ')}（客户端验过后生效）`]
      : []),
    ...(result.skipped.length ? ['', ...result.skipped.map((s) => `  ⏭  ${s}`)] : []),
    '',
    '下一步：把 updates/ 放到 Hub 的 LANHUB_UPDATE_DIR（默认 ./updates），客户端即可拉取。',
    '想连"客户端能不能真的装"一起验：lanhub-release --verify-only --hub http://<host>:<port>',
  ],
)
