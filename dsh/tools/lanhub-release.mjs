#!/usr/bin/env node
/**
 * lanhub-release —— 一条龙发布：构建 → 打包 → 签名发布 → 对真实 Hub 自检。
 *
 * 为什么要有它：发布本来是四步手工命令，而每一步漏掉都对应一种**最隐蔽的事故**：
 *   漏 build  → 签的是上一版 dist，版本号却是新的（客户端升上去发现行为没变）；
 *   漏 pack   → 直接发布仓库里那个旧的 tgz；
 *   漏 verify → "本地显示发布成功"，但 pkg/ 里没复制过去 / Hub 的
 *               LANHUB_UPDATE_DIR 指向别处，客户端 404。
 * 这些事故的共性是：发布侧全绿，消费侧全红，两边隔着一台机器，人肉对齐很累。
 * 所以把四步串成一条命令，并在发布后**真的下载一次**——签名对只证明"我写对了"，
 * 不证明"分发对了"。
 *
 * 版本真相只有一处：package.json 的 version。npm pack 由它决定文件名，publish
 * 再从文件名反解版本，本命令会显式校验三者一致。
 *
 * 用法：
 *   lanhub-release --key ~/.dsh/lanhub-ed25519/lanhub-ed25519.key.pem --hub http://127.0.0.1:18081
 *   lanhub-release --no-build --no-pack            # dist 已就绪，直接发布现有 tgz
 *   lanhub-release --verify-only --hub http://...  # 只做分发自检（排障用）
 *   lanhub-release --dry-run --key <key>           # 全流程试跑，只写临时目录
 *
 * 参数：
 *   --key <pem>          私钥（默认 $LAN_HUB_UPDATE_PRIVATE_KEY）
 *   --out <dir>          分发目录（默认 $LANHUB_UPDATE_DIR 或 <仓库根>/updates）
 *   --dest <dir>         tgz 落盘目录（默认 <仓库根>/bin）
 *   --pkg-dir <dir>      包根目录（默认按脚本位置推导）
 *   --hub <url>          Hub 的 HTTP 基址（注意：HTTP 网关通常是 TCP 端口 +1）
 *   --manifest-url <u>   直接给清单 URL（优先于 --hub）
 *   --pubkey <pem|path>  验签公钥（默认 $LAN_HUB_UPDATE_PUBKEY）
 *   --notes <text>       变更说明
 *   --latest <ver>       显式指定 latest（默认取清单里最大版本）
 *   --proto <n>          线协议版本（默认 3）
 *   --min-host <ver>     最低宿主版本（默认 0.2.0）
 *   --replace            允许覆盖已发布的同名版本
 *   --rotate-to <pub.pem> 声明下一把公钥（当前私钥背书，客户端凭旧根验过后接力）
 *   --no-build/--no-pack/--no-verify/--verify-only/--dry-run/--json
 *
 * 退出码：0 全绿；1 某一步失败；2 参数或环境问题（缺私钥、npm 不可用等）。
 */
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { publishArtifacts, verifyDistribution, loadManifest, MANIFEST_FILE } from '../src/publish.mjs'
import { loadTrustChain, originText } from '../src/keyring.mjs'
import { resolvePublicKey } from '../src/signing.mjs'

const positional = []
const opt = {
  key: process.env.LAN_HUB_UPDATE_PRIVATE_KEY || '',
  pubkey: process.env.LAN_HUB_UPDATE_PUBKEY || '',
  out: process.env.LANHUB_UPDATE_DIR || '',
  dest: '',
  pkgDir: '',
  hub: process.env.LAN_HUB_URL || '',
  manifestUrl: process.env.LAN_HUB_UPDATE_MANIFEST || '',
  notes: '',
  latest: '',
  proto: 3,
  minHost: '0.2.0',
  replace: false,
  rotateTo: '',
  revoke: [],
  build: true,
  pack: true,
  verify: true,
  verifyOnly: false,
  dryRun: false,
  json: false,
  help: false,
}
{
  const argv = process.argv.slice(2)
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i]
    const next = () => argv[++i] ?? ''
    if (a === '--key') opt.key = next()
    else if (a.startsWith('--key=')) opt.key = a.slice(6)
    else if (a === '--pubkey') opt.pubkey = next()
    else if (a.startsWith('--pubkey=')) opt.pubkey = a.slice(9)
    else if (a === '--out') opt.out = next()
    else if (a.startsWith('--out=')) opt.out = a.slice(6)
    else if (a === '--dest') opt.dest = next()
    else if (a.startsWith('--dest=')) opt.dest = a.slice(7)
    else if (a === '--pkg-dir') opt.pkgDir = next()
    else if (a.startsWith('--pkg-dir=')) opt.pkgDir = a.slice(10)
    else if (a === '--hub') opt.hub = next()
    else if (a.startsWith('--hub=')) opt.hub = a.slice(6)
    else if (a === '--manifest-url') opt.manifestUrl = next()
    else if (a.startsWith('--manifest-url=')) opt.manifestUrl = a.slice(15)
    else if (a === '--notes') opt.notes = next()
    else if (a === '--latest') opt.latest = next()
    else if (a === '--proto') opt.proto = Number(next() || 3)
    else if (a === '--min-host') opt.minHost = next()
    else if (a === '--rotate-to') opt.rotateTo = next()
    else if (a.startsWith('--rotate-to=')) opt.rotateTo = a.slice(12)
    else if (a === '--revoke') opt.revoke.push(next())
    else if (a.startsWith('--revoke=')) opt.revoke.push(a.slice(9))
    else if (a === '--replace') opt.replace = true
    else if (a === '--no-build') opt.build = false
    else if (a === '--no-pack') opt.pack = false
    else if (a === '--no-verify') opt.verify = false
    else if (a === '--verify-only') {
      opt.verifyOnly = true
      opt.build = false
      opt.pack = false
    } else if (a === '--dry-run') opt.dryRun = true
    else if (a === '--json') opt.json = true
    else if (a === '-h' || a === '--help') opt.help = true
    else if (!a.startsWith('-')) positional.push(a)
  }
}

if (opt.help) {
  process.stdout.write(
    [
      'lanhub-release —— 构建 → 打包 → 签名发布 → 对真实 Hub 自检',
      '',
      '用法：lanhub-release [--key <私钥.pem>] [--hub <http://host:port>] [选项]',
      '',
      '  --key <pem>        私钥（默认 $LAN_HUB_UPDATE_PRIVATE_KEY）',
      '  --out <dir>        分发目录（默认 $LANHUB_UPDATE_DIR 或 <仓库根>/updates）',
      '  --dest <dir>       tgz 落盘目录（默认 <仓库根>/bin）',
      '  --hub <url>        Hub HTTP 基址（网关端口通常是 TCP 端口 +1）',
      '  --manifest-url <u> 直接给清单 URL（优先于 --hub）',
      '  --pubkey <pem|path> 验签公钥（默认 $LAN_HUB_UPDATE_PUBKEY）',
      '  --notes <text>     变更说明     --latest <ver>  显式 latest',
      '  --rotate-to <pub.pem>  声明下一把公钥（密钥轮换，随包更新）',
      '  --revoke <id>          吊销密钥（8 位 hex keyId，可重复；泄露私钥用它作废）',
      '  --no-build/--no-pack/--no-verify',
      '  --verify-only      只做分发自检（排障用）',
      '  --dry-run          全流程试跑，只写临时目录',
      '  --json             输出 JSON',
      '',
      '退出码：0 全绿；1 某步失败；2 参数或环境问题。',
      '',
    ].join('\n'),
  )
  process.exit(0)
}

const log = (s) => {
  if (!opt.json) process.stdout.write(`${s}\n`)
}
function die(msg, code = 1) {
  if (opt.json) process.stdout.write(JSON.stringify({ ok: false, error: msg }, null, 2) + '\n')
  else process.stderr.write(`ERROR: ${msg}\n`)
  process.exit(code)
}

// ---------- 路径推导 ----------
// 脚本可能在 tools/（源码态）或 dist/（打包态），两者的上一级都是包根。
const SELF = path.dirname(fileURLToPath(import.meta.url))
const pkgDir = opt.pkgDir ? path.resolve(opt.pkgDir) : path.resolve(SELF, '..')
if (!fs.existsSync(path.join(pkgDir, 'package.json'))) {
  die(`定位不到包根（${pkgDir} 下没有 package.json）。用 --pkg-dir 指定 dsh/ 目录。`, 2)
}
const pkgJson = JSON.parse(fs.readFileSync(path.join(pkgDir, 'package.json'), 'utf8'))
const repoRoot = path.resolve(pkgDir, '..')
const destDir = path.resolve(opt.dest || path.join(repoRoot, 'bin'))
const outDir = path.resolve(opt.out || path.join(repoRoot, 'updates'))

// ---------- 私钥 ----------
let privateKeyPem = ''
if (!opt.verifyOnly) {
  if (!opt.key) {
    die('缺少私钥：--key <pem> 或 $LAN_HUB_UPDATE_PRIVATE_KEY（用 lanhub-keygen 生成）', 2)
  }
  try {
    privateKeyPem = fs.readFileSync(opt.key, 'utf8')
  } catch (e) {
    die(`读不到私钥 ${opt.key}：${e.message}`, 2)
  }
  if (!privateKeyPem.includes('PRIVATE KEY')) die(`${opt.key} 不是私钥 PEM`, 2)
}

const steps = []

// ---------- 1) 构建 ----------
// 默认构建而不是"有 dist 就跳过"：发布旧 dist 是最难发现的一类事故——
// 版本号是新的，字节是旧的，客户端升上去以为拿到了修复。
if (opt.build) {
  const buildScript = path.join(pkgDir, 'build.mjs')
  if (!fs.existsSync(buildScript)) die(`找不到构建脚本 ${buildScript}（或加 --no-build）`, 2)
  if (!fs.existsSync(path.join(pkgDir, 'node_modules', 'esbuild'))) {
    die(`${pkgDir} 下没有 node_modules/esbuild。先 npm i，或加 --no-build 直接用现有 dist。`, 2)
  }
  const r = spawnSync(process.execPath, [buildScript], { cwd: pkgDir, encoding: 'utf8' })
  if (r.status !== 0) {
    process.stderr.write(r.stdout || '')
    process.stderr.write(r.stderr || '')
    die('构建失败', 1)
  }
  steps.push({ step: 'build', ok: true, detail: `v${pkgJson.version}` })
  log(`[1/4] build   ✅ dist/ 已更新（v${pkgJson.version}）`)
} else {
  log('[1/4] build   ⏭  跳过')
}

// ---------- 2) 打包 ----------
let tgz = ''
if (opt.pack) {
  fs.mkdirSync(destDir, { recursive: true })
  // Windows 上 npm 是 npm.cmd，必须经 shell 才能 spawn。
  const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm'
  const r = spawnSync(npm, ['pack', '--pack-destination', destDir], {
    cwd: pkgDir,
    encoding: 'utf8',
    shell: process.platform === 'win32',
  })
  if (r.status !== 0 || r.error) {
    process.stderr.write(r.stdout || '')
    process.stderr.write(r.stderr || '')
    die(`npm pack 失败：${r.error?.message || `exit ${r.status}`}`, 1)
  }
  // npm 的输出里混着 notice，最后一条 .tgz 行才是产物名。
  const lines = String(r.stdout || '')
    .split('\n')
    .map((s) => s.trim())
    .filter((s) => s.endsWith('.tgz'))
  tgz = lines.length ? path.join(destDir, path.basename(lines[lines.length - 1])) : ''
  if (!tgz || !fs.existsSync(tgz)) {
    tgz = path.join(destDir, `${pkgJson.name}-${pkgJson.version}.tgz`)
    if (!fs.existsSync(tgz)) die(`npm pack 没产出预期的 ${tgz}`, 1)
  }
  // 版本真相校验：文件名里的版本必须等于 package.json 的版本。
  const fromFile = path.basename(tgz, '.tgz').slice(`${pkgJson.name}-`.length)
  if (fromFile !== pkgJson.version) {
    die(`版本不一致：tgz 是 ${fromFile}，package.json 是 ${pkgJson.version}（改版本后要重新 pack）`, 1)
  }
  steps.push({ step: 'pack', ok: true, detail: path.basename(tgz) })
  log(`[2/4] pack    ✅ ${path.basename(tgz)}（${(fs.statSync(tgz).size / 1024).toFixed(1)} KB）`)
} else {
  // 跳过打包时也得知道发哪个文件：位置参数优先，否则按 package.json 版本推导。
  tgz = positional[0] ? path.resolve(positional[0]) : path.join(destDir, `${pkgJson.name}-${pkgJson.version}.tgz`)
  if (!fs.existsSync(tgz)) {
    die(`--no-pack 但找不到 ${tgz}。先 npm pack，或把 tgz 路径直接作为位置参数给出。`, 2)
  }
  log(`[2/4] pack    ⏭  跳过（用现有 ${path.basename(tgz)}）`)
}

// ---------- 3) 签名发布 ----------
let version = ''
let manifestPath = ''
if (!opt.verifyOnly) {
  const publishOut = opt.dryRun ? fs.mkdtempSync(path.join(os.tmpdir(), 'lanhub-release-')) : outDir
  let result
  try {
    result = publishArtifacts({
      files: [tgz],
      privateKeyPem,
      outDir: publishOut,
      name: pkgJson.name,
      latest: opt.latest,
      proto: opt.proto,
      minHost: opt.minHost,
      notes: opt.notes,
      replace: opt.replace,
      pubkeyHint: opt.pubkey,
      rotateTo: opt.rotateTo,
      revoke: opt.revoke,
    })
  } catch (e) {
    die(e.message, 1)
  }
  version = result.published[0].version
  manifestPath = result.manifestPath
  steps.push({
    step: 'publish',
    ok: true,
    detail: opt.dryRun ? `dry-run → ${publishOut}` : `${version} → ${result.manifestPath}`,
  })
  log(
    opt.dryRun
      ? `[3/4] publish ✅ 试跑：${version} 已写入临时目录 ${publishOut}`
      : `[3/4] publish ✅ ${version} → ${result.manifestPath}（latest=${result.manifest.latest}）`,
  )
  if (result.manifest.nextKey) {
    log(`           🔑 已声明下一把公钥 keyId=${result.manifest.nextKey.keyId}（客户端验过后接力）`)
  }
} else {
  // verify-only：期望版本从本地清单的 latest 取，不猜。
  try {
    const { manifest } = loadManifest(outDir, { name: pkgJson.name })
    version = opt.latest || manifest.latest
    manifestPath = path.join(outDir, MANIFEST_FILE)
  } catch (e) {
    die(e.message, 1)
  }
  if (!version) die(`${manifestPath} 里 latest 为空——还没有发布过任何版本`, 1)
  log(`[3/4] publish ⏭  跳过（自检目标：清单 latest=${version}）`)
}

// ---------- 4) 对真实 Hub 自检 ----------
// 前面三步都在本机，客户端看到的却是另一台机器上的另一份文件。这一步用客户端
// 的方式取一次（GET 清单 → 解析 url → 下载 → 比 sha256 → 验签），把"分发"也纳入
// 成败判定，而不是只保证"写对了"。
let verify = null
if (!opt.verify) {
  log('[4/4] verify  ⏭  跳过')
} else {
  const manifestUrl = opt.manifestUrl || (opt.hub ? `${opt.hub.replace(/\/+$/, '')}/api/updates` : '')
  if (!manifestUrl) {
    log('[4/4] verify  ⏭  跳过（给 --hub 或 --manifest-url 才会自检）')
  } else {
    // 公钥来源与客户端保持一致：显式 --pubkey > 轮换链 > 包内置。
    // 不回落的话，明明客户端能验过，发布侧却报"未配置公钥跳过验签"。
    const t = loadTrustChain({ cacheDir: '', config: opt.pubkey })
    const pub = t.chain[0] || ''
    if (!pub) log('[4/4] verify  ⚠  没有可信公钥：验签这一步会跳过')
    else if (!opt.pubkey) log(`[4/4] verify  🔑 验签公钥来源：${originText(t.origin)}`)
    verify = await verifyDistribution({ manifestUrl, version, publicKeyPem: pub })
    for (const c of verify.checks) {
      log(`           ${c.ok === false ? '❌' : c.ok === null ? '⏭ ' : '✅'} ${c.name}  ${c.detail}`)
    }
    steps.push({ step: 'verify', ok: verify.ok, detail: manifestUrl, checks: verify.checks })
    log(`[4/4] verify  ${verify.ok ? '✅' : '❌'} ${verify.ok ? '客户端视角可安装' : '分发自检失败'}  ${manifestUrl}`)
  }
}

// ---------- 汇总 ----------
const ok = steps.every((s) => s.ok !== false)
if (opt.json) {
  process.stdout.write(
    JSON.stringify(
      { ok, version, manifestPath, tgz: tgz ? path.basename(tgz) : '', steps, verify: verify?.checks ?? null },
      null,
      2,
    ) + '\n',
  )
} else if (ok) {
  log('')
  log(`发布完成：v${version}${verify ? '，Hub 自检通过' : ''}`)
}
process.exit(ok ? 0 : 1)
