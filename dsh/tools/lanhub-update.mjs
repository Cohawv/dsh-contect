#!/usr/bin/env node
/**
 * lanhub-update —— dsh-plugin-lan-hub 的更新通道客户端。
 *
 * 服务端契约（预留接口，见 internal/plugins/httpgateway/updates.go）：
 *   GET <manifest>  -> { name, latest, proto, minHost, artifacts: { "<ver>": {...} } }
 *   GET <artifact>  -> application/octet-stream
 * 清单响应带 X-Proto-Version 头，用于判断 Hub 是否支持目标版本的线协议。
 *
 * 三段式，风险严格分开——这是本工具最重要的设计约束：
 *
 *   check   只读：拉清单、比版本、查兼容。零副作用。
 *   fetch   下载 + 校验 sha256，落进缓存目录。**不动安装目录**。
 *   apply   执行安装命令，必须显式 --yes，且装完要重启宿主才生效。
 *
 * 为什么插件自己不干这件事（而由人调用命令行）：
 *   1) 代码不是数据。policy 能热更新是因为它只是内存里的一个对象；代码更新要重新
 *      import，ESM 模块缓存 + 插件持有 TCP 长连接，做不到原地替换；
 *   2) 装完仍要重启宿主，所以"自动更新"省不掉这一步，只是把危险动作挪到你不在场时；
 *   3) LAN 无认证，让长跑进程用自身权限替换自己的可执行文件，等于把投毒面从
 *      "人手工装包"扩大到"任何能连上 Hub 的设备"。
 *
 * 用法：
 *   lanhub-update check                      看是否有更新（exit 10 = 有更新）
 *   lanhub-update fetch [version]            下载并校验到缓存
 *   lanhub-update apply [version] --yes      用缓存里的包执行安装
 *   lanhub-update rollback [--to version]    回滚到更旧的缓存版本
 *   lanhub-update list                       查看缓存
 *   lanhub-update verify                     重新校验缓存完整性
 *
 * 通用参数：
 * 信任模型（自动更新的前提）：
 *   sha256 只防传输截断，不防投毒——清单与制品同源，LAN 又无认证，能改其中一个
 *   就能改另一个。所以制品必须带 Ed25519 签名，客户端用**本地 pin 的公钥**验。
 *   公钥绝不能从清单下载，否则投毒者连公钥一起发，签名就自我证明了。
 *   降级攻击（把 latest 指回旧的但签过的版本）由"只升不降"兜底。
 *
 * 通用参数：
 *   --manifest <url>       清单地址（默认 $LAN_HUB_UPDATE_MANIFEST 或 http://127.0.0.1:8081/api/updates）
 *   --cache <dir>          缓存目录（默认 ~/.dsh/updates）
 *   --pubkey <pem|path>    验签公钥（默认 $LAN_HUB_UPDATE_PUBKEY）
 *   --staged-dir <dir>     待激活区（默认 <cache>/staged）
 *   --current <ver>        覆盖本地版本（离线/异常时兜底）
 *   --cmd <tpl>            安装命令模板，用 {tgz} 占位（默认 dsh plugin --profile web add {tgz}）
 *   --allow-unsigned       允许未签名/未配置公钥（仅本地开发，自动更新不得使用）
 *   --downgrade            允许降级（正常路径应只升不降）
 *   --max-attempts <n>     自检回滚前允许的最大启动尝试次数（默认 2）
 *   --json                 输出 JSON
 *   --yes                  确认执行安装（apply/activate/rollback 必需）
 *   --quiet                安静模式
 *
 * 退出码：0 成功/已是最新；1 错误；10 有可用更新；11 版本不兼容；12 需要 --yes；
 *         13 制品不可信（未签名 / 签名不匹配 / 未配置公钥）。
 */

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { SIG_ALGO, keyIdOf } from '../src/signing.mjs'
import {
  loadTrustChain,
  verifyWithChain,
  verifyWithChainDetailed,
  acceptKeyRotation,
  loadRevocations,
  applyManifestRevocations,
  fingerprint,
  originText,
} from '../src/keyring.mjs'

const DEFAULT_MANIFEST = 'http://127.0.0.1:8081/api/updates'
const DEFAULT_CMD = 'dsh plugin --profile web add {tgz}'

// ---------- 参数解析 ----------
const opt = {
  json: false,
  quiet: false,
  yes: false,
  manifest: process.env.LAN_HUB_UPDATE_MANIFEST || DEFAULT_MANIFEST,
  cache: process.env.LAN_HUB_UPDATE_CACHE || path.join(os.homedir(), '.dsh', 'updates'),
  pubkey: process.env.LAN_HUB_UPDATE_PUBKEY || '',
  stagedDir: '',
  allowUnsigned: false,
  downgrade: false,
  maxAttempts: 2,
  current: '',
  cmd: process.env.LAN_HUB_INSTALL_CMD || DEFAULT_CMD,
  timeoutMs: 15000,
}
let cmd = 'check'
const args = []
{
  const argv = process.argv.slice(2)
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i]
    if (a === '--manifest') opt.manifest = argv[++i] ?? ''
    else if (a.startsWith('--manifest=')) opt.manifest = a.slice(11)
    else if (a === '--cache') opt.cache = path.resolve(argv[++i] ?? '')
    else if (a.startsWith('--cache=')) opt.cache = path.resolve(a.slice(8))
    else if (a === '--current') opt.current = argv[++i] ?? ''
    else if (a === '--pubkey') opt.pubkey = argv[++i] ?? ''
    else if (a.startsWith('--pubkey=')) opt.pubkey = a.slice(9)
    else if (a === '--staged-dir') opt.stagedDir = path.resolve(argv[++i] ?? '')
    else if (a.startsWith('--staged-dir=')) opt.stagedDir = path.resolve(a.slice(13))
    else if (a === '--cmd') opt.cmd = argv[++i] ?? DEFAULT_CMD
    else if (a === '--allow-unsigned') opt.allowUnsigned = true
    else if (a === '--downgrade') opt.downgrade = true
    else if (a === '--max-attempts') opt.maxAttempts = Number(argv[++i] ?? 2)
    else if (a === '--json') opt.json = true
    else if (a === '--quiet') opt.quiet = true
    else if (a === '--yes' || a === '-y') opt.yes = true
    else if (a === '--to') args.unshift(argv[++i] ?? '')
    else if (a === '-h' || a === '--help') cmd = 'help'
    else if (!a.startsWith('-') && cmd === 'check' && !args.length) cmd = a
    else if (!a.startsWith('-')) args.push(a)
  }
}

// ---------- 输出 ----------
function emit(data, lines) {
  // --quiet 只压掉人读输出；--json 是给脚本消费的，必须照常输出
  if (opt.quiet && !opt.json) return
  process.stdout.write(opt.json ? JSON.stringify(data, null, 2) + '\n' : lines.join('\n') + '\n')
}
function die(msg, code = 1) {
  if (opt.json) process.stdout.write(JSON.stringify({ ok: false, error: msg }, null, 2) + '\n')
  else process.stderr.write(`ERROR: ${msg}\n`)
  process.exit(code)
}

// ---------- 版本 ----------
/** 只处理 x.y.z(-pre)：够用即可，不引依赖 */
function parseVersion(v) {
  const s = String(v).trim().replace(/^v/, '')
  const [core, pre = ''] = s.split('-', 2)
  const n = core.split('.').map((x) => Number.parseInt(x, 10) || 0)
  return { n: [n[0] || 0, n[1] || 0, n[2] || 0], pre }
}

function cmpVersion(a, b) {
  const pa = parseVersion(a)
  const pb = parseVersion(b)
  for (let i = 0; i < 3; i += 1) {
    if (pa.n[i] !== pb.n[i]) return pa.n[i] > pb.n[i] ? 1 : -1
  }
  // 同为 x.y.z 时，预发布版（带 -xxx）更旧
  if (pa.pre && !pb.pre) return -1
  if (!pa.pre && pb.pre) return 1
  return 0
}

/** 本地版本：读插件自己的 package.json（仓库态与安装态都只有一层上级目录） */
function localVersion() {
  if (opt.current) return String(opt.current).trim()
  try {
    const pkg = JSON.parse(fs.readFileSync(new URL('../package.json', import.meta.url), 'utf8'))
    return String(pkg.version || '0.0.0')
  } catch {
    return '0.0.0'
  }
}

// ---------- 清单 ----------
async function fetchManifest() {
  if (!opt.manifest) die('缺少清单地址（--manifest 或 LAN_HUB_UPDATE_MANIFEST）')
  let res
  try {
    res = await fetch(opt.manifest, {
      headers: { accept: 'application/json' },
      signal: AbortSignal.timeout(opt.timeoutMs),
    })
  } catch (e) {
    die(`拉取清单失败：${e.message}\n地址：${opt.manifest}\n（Hub 的 HTTP 端口默认 :8081；确认 Hub 在跑且可达）`)
  }
  if (!res.ok) die(`清单 HTTP ${res.status} ${res.statusText}：${opt.manifest}`)
  const hubProto = Number(res.headers.get('x-proto-version') || 0)
  let body
  try {
    body = await res.json()
  } catch (e) {
    die(`清单不是合法 JSON：${e.message}`)
  }
  if (!body || typeof body !== 'object') die('清单必须是 JSON 对象')
  const artifacts = body.artifacts && typeof body.artifacts === 'object' ? body.artifacts : {}
  return {
    url: opt.manifest,
    name: String(body.name || 'dsh-plugin-lan-hub'),
    hubProto,
    latest: String(body.latest || ''),
    proto: Number(body.proto || 0),
    minHost: String(body.minHost || ''),
    keyId: String(body.keyId || ''),
    // 密钥轮换背书：由当前私钥签发的新公钥，客户端凭旧根验过后接力
    nextKey: body.nextKey && typeof body.nextKey === 'object' ? body.nextKey : null,
    // 撤销声明：由信任链上的私钥签名的 {id, sig, sigAlgo}，客户端验过才并入本地
    revocations: Array.isArray(body.revocations) ? body.revocations : [],
    artifacts,
  }
}

/**
 * 信任判定：这是自动更新的核心闸门。
 *
 * sha256 只证明"拿到的与清单说的一致"，而清单和制品同源、LAN 又无认证，所以
 * 能改其中一个就能改另一个——sha256 防不住投毒。签名才是"这包确实是发布者做的"
 * 的证据。
 *
 * 三种结果：
 *   verified         有公钥 + 验签通过 —— 唯一可以无人值守自动安装的情形
 *   unsigned-forced  缺公钥或无签名，但显式 --allow-unsigned —— 仅本地开发
 *   rejected         其余一律拒绝
 */
function verifyTrust(m, v, art, trustIn) {
  // trustIn 是 resolveChain()/loadTrustChain() 的返回值（含 chain / origin / revoked）：
  // 必须连 origin、revoked 一起传，否则报错时说的来源、撤销状态就是错的。
  const src = trustIn && trustIn.chain ? trustIn : loadTrustChain({ cacheDir: opt.cache, config: opt.pubkey })
  const roots = src.chain
  const origin = src.origin
  const revoked = src.revoked instanceof Set ? src.revoked : new Set()
  if (!roots.length) {
    if (opt.allowUnsigned) return { ok: true, level: 'unsigned-forced' }
    return {
      ok: false,
      reason:
        '没有可信公钥，无法判断制品来源。自动更新必须能自证来源，否则等于把投毒面扩大到任何能连上 Hub 的设备。\n' +
        '  信任根 = 配置/内置公钥（锚） + 已接受的轮换链（缓存 trusted-key.json）\n' +
        '  本地开发确实要跳过：--allow-unsigned',
    }
  }
  if (!art.sig) {
    if (opt.allowUnsigned) return { ok: true, level: 'unsigned-forced' }
    return {
      ok: false,
      reason: `制品 ${v} 未签名，但本机已 pin 公钥——拒绝。（发布侧用 lanhub-publish --key <私钥> 签名）`,
    }
  }
  if (art.sigAlgo && art.sigAlgo !== SIG_ALGO) {
    return { ok: false, reason: `不支持的签名算法 ${art.sigAlgo}（本机只认 ${SIG_ALGO}）` }
  }
  const payload = {
    name: m.name,
    version: v,
    sha256: String(art.sha256 || '').toLowerCase(),
    proto: Number(art.proto ?? m.proto ?? 0),
  }
  const det = verifyWithChainDetailed(payload, art.sig, roots)
  if (!det.ok) {
    return {
      ok: false,
      reason:
        `签名校验失败：${v} 的签名与清单字段不匹配。\n` +
        `  当前信任根来源：${originText(origin)}\n` +
        '  可能是制品被换过、清单被改过，或签名用的不是本机信任链上的私钥。\n' +
        '  在无人值守场景下这必须中止——宁可不更新，也不能装来源不明的代码。',
    }
  }
  if (revoked.has(det.keyId)) {
    return {
      ok: false,
      reason:
        `该信任根（keyId ${det.keyId}）已被撤销，拒绝 ${v}。\n` +
        '  原因：该私钥可能已泄露，由发布侧 lanhub-publish --revoke 吊销。\n' +
        '  若确认是误吊销，清掉缓存里的 revoked-keys.json 再试（仅在你能确定安全时）。',
    }
  }
  return { ok: true, level: 'verified' }
}

/**
 * 只升不降。
 *
 * 签名防的是"伪造包"，防不住"把 latest 指回一个旧的、但确实被签过的版本"
 * （降级攻击：老版本可能有已知缺陷）。所以版本单向性是签名之外的第二道闸。
 * 回滚是唯一合法的下行路径，走 rollback 命令并显式指定目标。
 */
function checkMonotonic(v) {
  if (opt.downgrade) return ''
  const cur = localVersion()
  if (cmpVersion(v, cur) < 0) {
    return `目标 ${v} 比当前 ${cur} 更旧，拒绝降级（如需回滚请用 rollback --to ${v}）`
  }
  return ''
}

/** 兼容性判断：核心是 proto——装完发现连不上是最糟的失败模式 */
function checkCompat(m, art) {
  const need = Number(art?.proto ?? m.proto ?? 0)
  if (need && m.hubProto && need > m.hubProto) {
    return `目标版本要求线协议 proto ${need}，但 Hub 只支持 ${m.hubProto}；请先升级 Hub，否则装完连不上`
  }
  return ''
}

// ---------- 缓存 ----------
const pkgDir = () => path.join(opt.cache, 'pkg')
const metaDir = () => path.join(opt.cache, 'meta')
const tgzName = (v) => `dsh-plugin-lan-hub-${v}.tgz`
const tgzPath = (v) => path.join(pkgDir(), tgzName(v))
const metaPath = (v) => path.join(metaDir(), `${v}.json`)

function readMeta(v) {
  try {
    return JSON.parse(fs.readFileSync(metaPath(v), 'utf8'))
  } catch {
    return null
  }
}

function listCached() {
  try {
    return fs
      .readdirSync(metaDir())
      .filter((f) => f.endsWith('.json'))
      .map((f) => readMeta(f.slice(0, -5)))
      .filter(Boolean)
      .sort((a, b) => cmpVersion(b.version, a.version))
  } catch {
    return []
  }
}

function sha256File(p) {
  return createHash('sha256').update(fs.readFileSync(p)).digest('hex')
}

/** 原子写 JSON：先 tmp 再 rename。启动流程读这些状态文件，绝不能读到半截内容。 */
function writeJSONAtomic(p, obj) {
  fs.mkdirSync(path.dirname(p), { recursive: true })
  const tmp = `${p}.${process.pid}.tmp`
  fs.writeFileSync(tmp, JSON.stringify(obj, null, 2) + '\n')
  fs.renameSync(tmp, p)
}

/** 读状态文件（pending/attempt/ok/failed）。不存在或损坏一律当作"没有"，不抛栈。 */
function readState(dir, f) {
  try {
    const o = JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8'))
    return o && typeof o === 'object' ? o : null
  } catch {
    return null
  }
}

function removeState(dir, f) {
  try {
    fs.rmSync(path.join(dir, f), { force: true })
  } catch {
    /* 不存在就算了 */
  }
}

/** 落盘校验：请求前再验一次，防止缓存被改坏后才拿去安装 */
function verifyStaged(v) {
  const meta = readMeta(v)
  if (!meta) throw new Error(`缓存里没有 ${v} 的元数据（先 fetch）`)
  const p = tgzPath(v)
  if (!fs.existsSync(p)) throw new Error(`缓存里缺制品：${p}`)
  const got = sha256File(p)
  if (got !== meta.sha256) throw new Error(`${v} 校验失败：缓存文件已损坏\n  got  ${got}\n  want ${meta.sha256}`)
  return { meta, path: p }
}

/**
 * 取本次要用的信任根链，并顺带处理清单里的密钥轮换背书。
 *
 * 轮换必须**先于**验包：这一版很可能就是由新私钥签的，拿旧链去验会直接判
 * untrusted，轮换就永远轮不到。
 *
 * persist=false 只判断不落盘——check 是只读命令，不能因为"看一眼"就改状态。
 */
function resolveChain(m, persist) {
  const cur = loadTrustChain({ cacheDir: opt.cache, config: opt.pubkey })
  let rotation = null
  let chain = cur.chain
  // 轮换必须先于验包：这一版很可能就是由新私钥签的，拿旧链去验会直接判 untrusted。
  if (m && m.nextKey) {
    const r = acceptKeyRotation({ cacheDir: opt.cache, nextKey: m.nextKey, config: opt.pubkey, persist })
    if (r.ok && !r.unchanged) {
      chain = r.chain
      rotation = { ok: true, persisted: persist, from: r.pinned.from, to: r.pinned.to }
    } else if (r.ok && r.unchanged) {
      rotation = { ok: true, persisted: false, unchanged: true, from: r.pinned?.from, to: r.pinned?.to }
    } else {
      rotation = { ok: false, reason: r.reason }
    }
  }
  // 撤销：把清单里**合法签名**的吊销声明并入本地（只增不减，跨 Hub 存活）。
  // 只在 persist=true（fetch）时落盘；check 是只读命令，不写状态。
  const revoked = applyManifestRevocations(m && m.revocations, chain, opt.cache, persist)
  return { chain, origin: cur.origin, rotation, revoked, valid: cur.valid, reason: cur.reason }
}

function rotationLine(rot) {
  if (!rot) return ''
  if (!rot.ok) return `🔑 密钥轮换被拒：${rot.reason}`
  return rot.persisted
    ? `🔑 密钥轮换：${rot.from} → ${rot.to}（已接力，新公钥入链）`
    : `🔑 密钥轮换：${rot.from} → ${rot.to}（背书有效，fetch 时接力——check 不改状态）`
}

async function download(v, m, trustIn) {
  const art = m.artifacts[v]
  if (!art) die(`清单里没有版本 ${v}（可用：${Object.keys(m.artifacts).join(', ') || '无'}）`)
  if (!art.sha256) die(`清单里 ${v} 缺少 sha256 —— 没有校验就不下载，这是硬性要求`)

  // 先判信任，再下载：不可信的包连一个字节都不该落盘
  const trust = verifyTrust(m, v, art, trustIn)
  if (!trust.ok) die(trust.reason, 13)

  const url = new URL(String(art.url || ''), m.url).toString()

  let res
  try {
    res = await fetch(url, { signal: AbortSignal.timeout(opt.timeoutMs * 8) })
  } catch (e) {
    die(`下载失败：${e.message}\n地址：${url}`)
  }
  if (!res.ok) die(`下载 HTTP ${res.status} ${res.statusText}：${url}`)

  const buf = Buffer.from(await res.arrayBuffer())
  if (art.size && buf.length !== Number(art.size)) {
    die(`大小不符：收到 ${buf.length} B，清单声明 ${art.size} B（传输被截断？）`)
  }
  const sha = createHash('sha256').update(buf).digest('hex')
  if (sha !== String(art.sha256).toLowerCase()) {
    die(`sha256 不符，拒绝暂存：\n  got  ${sha}\n  want ${art.sha256}`)
  }

  fs.mkdirSync(pkgDir(), { recursive: true })
  fs.mkdirSync(metaDir(), { recursive: true })
  // 原子落盘：先写 tmp 再 rename，避免中断留下半截 tgz
  const tmp = `${tgzPath(v)}.${process.pid}.tmp`
  fs.writeFileSync(tmp, buf)
  fs.renameSync(tmp, tgzPath(v))
  // 签名随元数据一起落盘：apply 时会再验一次，防止缓存在这之后被换成别的包
  fs.writeFileSync(
    metaPath(v),
    JSON.stringify(
      {
        version: v,
        name: m.name,
        sha256: sha,
        size: buf.length,
        proto: Number(art.proto ?? m.proto ?? 0),
        url,
        manifest: m.url,
        sig: String(art.sig || ''),
        sigAlgo: String(art.sigAlgo || ''),
        trust: trust.level,
        fetchedAt: new Date().toISOString(),
      },
      null,
      2,
    ) + '\n',
  )
  return { version: v, path: tgzPath(v), sha256: sha, size: buf.length, url, trust: trust.level }
}

/**
 * 确保上一版本的制品也在缓存里，否则回滚无从谈起。
 *
 * 只升不降是给"正常升级"用的闸门，这里是明确的下行准备，所以直接调 download
 * （download 内部只做信任判定，不做单调性判断）。
 */
async function ensureRollbackPackage() {
  const prev = localVersion()
  if (!prev || prev === '0.0.0') return { ready: false, reason: '无法确定当前版本' }
  if (readMeta(prev)) return { ready: true, reason: '' }
  try {
    const m = await fetchManifest()
    if (!m.artifacts[prev]) return { ready: false, reason: `清单里没有 ${prev}，回滚需手工安装` }
    await download(prev, m, resolveChain(m, true))
    return { ready: true, reason: '' }
  } catch (e) {
    return { ready: false, reason: `回滚包准备失败：${e.message}` }
  }
}

/**
 * 安装前的信任复核。
 *
 * sha256 只保证包没坏，不保证包还是"当初验过签的那个"。若有人把缓存里的包
 * 换成了另一个（哪怕 sha 一致几乎不可能，但元数据可能被改），这里再用签名兜一次。
 */
function reverify(v, meta) {
  const t = loadTrustChain({ cacheDir: opt.cache, config: opt.pubkey })
  const roots = t.chain
  if (!roots.length) {
    if (opt.allowUnsigned) return 'unsigned-forced'
    die('没有可信公钥，无法确认制品来源。加 --pubkey，或本地开发用 --allow-unsigned', 13)
  }
  if (!meta.sig) {
    if (opt.allowUnsigned) return 'unsigned-forced'
    die(`${v} 的缓存元数据里没有签名，而本机已 pin 公钥——拒绝安装`, 13)
  }
  const payload = { name: meta.name || 'dsh-plugin-lan-hub', version: v, sha256: meta.sha256, proto: meta.proto ?? 0 }
  const det = verifyWithChainDetailed(payload, meta.sig, roots)
  if (!det.ok) die(`${v} 签名复核失败——缓存在下载后被改动过，拒绝安装`, 13)
  if (t.revoked.has(det.keyId)) die(`${v} 的签名根（keyId ${det.keyId}）已被撤销，拒绝安装`, 13)
  return 'verified'
}

// ---------- 安装 ----------
/**
 * 执行安装。两道闸：先要 --yes（人确认），再重验缓存完整性（防缓存在此之前被改坏）。
 * verifyStaged 抛出的异常统一转成退出码 1，不吐栈给用户。
 */
function installOrDie(v) {
  try {
    return runInstall(v)
  } catch (e) {
    die(e.message)
  }
}

function runInstall(v) {
  if (!opt.yes) {
    die(
      `安装会执行：\n  ${opt.cmd.replace('{tgz}', tgzPath(v))}\n确认无误后加 --yes 重跑`,
      12,
    )
  }
  const { meta } = verifyStaged(v)
  reverify(v, meta)
  const command = opt.cmd.replace(/\{tgz\}/g, tgzPath(v))
  if (!opt.quiet) process.stderr.write(`$ ${command}\n`)
  const r = spawnSync(command, { shell: true, stdio: 'inherit' })
  if (r.error) die(`执行安装命令失败：${r.error.message}`)
  if (r.status !== 0) die(`安装命令退出码 ${r.status}`)
  return meta
}

// ---------- 子命令 ----------
const COMMANDS = {
  async check() {
    const m = await fetchManifest()
    const cur = localVersion()
    const latest = m.latest
    const art = latest ? m.artifacts[latest] : null
    const incompat = checkCompat(m, art)
    const hasUpdate = Boolean(latest) && cmpVersion(latest, cur) > 0
    // 轮换只在 check 里"预演"：判断这封背书能不能接受，但不落盘（check 必须零副作用）
    const chain = resolveChain(m, false)
    // 只在"确实会装"时才做信任判定，否则本地开发没配公钥也会被误报
    const trust = hasUpdate ? verifyTrust(m, latest, art, chain) : null

    const lines = [
      `清单：${m.url}`,
      `本地：${cur}`,
      `最新：${latest || '（无可用更新）'}${m.hubProto ? `   hub proto=${m.hubProto}` : ''}`,
    ]
    if (art?.notes) lines.push(`说明：${art.notes}`)
    if (incompat) lines.push(`⚠️  ${incompat}`)
    const rot = rotationLine(chain.rotation)
    if (rot) lines.push(rot)
    if (trust) {
      lines.push(
        trust.ok
          ? `🔒 信任：${trust.level === 'verified' ? '签名校验通过（可自动安装）' : '未校验（--allow-unsigned）'}`
          : `🔒 不可信：${trust.reason}`,
      )
    }
    if (!hasUpdate) lines.push(incompat ? '' : '✅ 已是最新版本')

    const data = {
      ok: true,
      current: cur,
      latest,
      hubProto: m.hubProto,
      incompatible: Boolean(incompat),
      hasUpdate,
      trusted: trust ? trust.ok : null,
      trustLevel: trust ? trust.level : null,
      trustOrigin: chain.origin,
      rotation: chain.rotation || null,
      url: m.url,
    }
    emit(data, lines.filter((x) => x !== ''))
    if (incompat) process.exit(11)
    // 13 = 有更新但不可信。自动化脚本据此区分"没更新"和"有但不能装"。
    if (trust && !trust.ok) process.exit(13)
    process.exit(hasUpdate ? 10 : 0)
  },

  async fetch() {
    const m = await fetchManifest()
    const v = args[0] || m.latest
    if (!v) die('清单没有 latest，且未指定版本')
    const incompat = checkCompat(m, m.artifacts[v])
    if (incompat) die(incompat, 11)
    const mono = checkMonotonic(v)
    if (mono) die(mono)
    // fetch 是写操作，轮换在这里真正落盘
    const chain = resolveChain(m, true)
    const got = await download(v, m, chain)
    const rot = rotationLine(chain.rotation)
    emit(
      { ok: true, ...got, rotation: chain.rotation || null },
      [
        `已暂存 ${got.version} -> ${got.path}`,
        `sha256 ${got.sha256}`,
        `信任：${got.trust}`,
        ...(rot ? [rot] : []),
        '（仅下载，未安装；下一步：lanhub-update stage ' + got.version + '，或直接 apply --yes）',
      ],
    )
  },

  /**
   * 显示当前信任根链。
   *
   * 公钥不再只有"配置里那一把"：轮换后链会变长（旧根保留，用于验回滚包）。
   * 出"明明配了公钥却说不可信"这类问题时，第一件事就是看这里——链的第 0 把
   * 是谁、从哪来的，往往就是答案。
   */
  trust() {
    const t = loadTrustChain({ cacheDir: opt.cache, config: opt.pubkey })
    const originLabel = {
      config: '配置/环境变量（--pubkey / $LAN_HUB_UPDATE_PUBKEY）',
      embedded: '包内置公钥（trusted-key.mjs）',
      none: '无',
    }[t.origin]
    const lines = [`锚来源：${originLabel}`, `链长度：${t.chain.length}`]
    t.chain.forEach((pem, i) => {
      const kid = keyIdOf(pem)
      const revoked = t.revoked.has(kid) ? '  ⛔ 已撤销' : ''
      lines.push(`  ${i === 0 ? '*' : ' '} ${i}. ${kid}${i === 0 ? '  （锚）' : ''}${revoked}`)
    })
    if (t.revoked.size) lines.push(`已撤销的信任根：${[...t.revoked].join(', ')}`)
    if (t.rotations && t.rotations.length) {
      const last = t.rotations[t.rotations.length - 1]
      lines.push(`轮换历史：${t.rotations.length} 次；最近 ${last.keyId} @ ${last.rotatedAt}`)
    }
    if (t.origin === 'none') lines.push('', '没有可信公钥 → 自动更新会被判 untrusted（exit 13）。')
    emit(
      { ok: t.chain.length > 0, origin: t.origin, chain: t.chain.map(keyIdOf), revoked: [...t.revoked], rotations: t.rotations },
      lines,
    )
    if (t.origin === 'none') process.exit(13)
  },

  apply() {
    const v = args[0] || localVersion()
    if (!readMeta(v)) die(`缓存里没有 ${v}，先执行 fetch`)
    const mono = checkMonotonic(v)
    if (mono) die(mono)
    const meta = installOrDie(v)
    emit({ ok: true, applied: v, sha256: meta.sha256 }, [
      `已安装 ${v}`,
      '',
      '⚠️  需要重启宿主（dsh / DSHA）才会加载新代码——代码不像 policy 那样能热替换。',
      '   重启后用 hub_stats 可确认版本号。',
    ])
  },

  /**
   * 预置待激活版本。
   *
   * 与 apply 的差别：apply 立刻执行安装命令，stage 只把包放到待激活区并写
   * pending.json，真正的切换交给启动流程（wb_boot.sh）。这样切换点永远落在
   * 「进程还没起来」的干净时刻，不存在"改运行中代码"的幻象，也让自检回滚成为可能。
   */
  async stage() {
    const v = args[0] || ''
    if (!v) die('stage 需要指定版本（先 fetch）')
    const { meta } = verifyStaged(v)
    const level = reverify(v, meta)
    const mono = checkMonotonic(v)
    if (mono) die(mono)

    // 预备回滚包。
    // 这是必需的而非可选：回滚要装回 previous，但缓存里通常只有刚下载的新版本——
    // previous 是当前正在跑的那个，从来没被"下载"过。不在这里补下载，
    // healthcheck 就会陷入"想回滚但手上没有旧包"。失败只警告，不阻断本次预置。
    const rollbackNote = await ensureRollbackPackage()

    const dir = opt.stagedDir || path.join(opt.cache, 'staged')
    fs.mkdirSync(dir, { recursive: true })
    const name = meta.name || 'dsh-plugin-lan-hub'
    const dst = path.join(dir, `${name}-${v}.tgz`)
    fs.copyFileSync(tgzPath(v), dst)

    const pending = {
      name,
      version: v,
      sha256: meta.sha256,
      sig: meta.sig || '',
      sigAlgo: meta.sigAlgo || '',
      proto: meta.proto ?? 0,
      tgz: dst,
      previous: localVersion(),
      trust: level,
      stagedAt: new Date().toISOString(),
    }
    writeJSONAtomic(path.join(dir, 'pending.json'), pending)

    emit({ ok: true, ...pending, stagedDir: dir, rollbackReady: rollbackNote.ready }, [
      `已预置 ${v} -> ${dst}`,
      `信任：${level}`,
      `上一版本：${pending.previous}${rollbackNote.ready ? '（回滚包已就位）' : `（⚠️ ${rollbackNote.reason}）`}`,
      '',
      '⚠️  尚未激活。下次启动宿主时由 wb_boot.sh 切换；若新版本连续 2 次起不来会自动回滚。',
    ])
  },

  /** 查看待激活状态。诊断"为什么没更新/为什么回滚了"主要看这个。 */
  pending() {
    const dir = opt.stagedDir || path.join(opt.cache, 'staged')
    const pending = readState(dir, 'pending.json')
    const attempt = readState(dir, 'attempt.json')
    const ok = readState(dir, 'ok.json')
    const failed = readState(dir, 'failed.json')
    const lines = [`待激活目录：${dir}`]
    lines.push(pending ? `  pending：${pending.version}（上一版 ${pending.previous}）` : '  pending：无')
    lines.push(attempt ? `  attempt：${attempt.version} x${attempt.count}  最近 ${attempt.at}` : '  attempt：无')
    lines.push(ok ? `  ok：${ok.version} @ ${ok.at}` : '  ok：无')
    if (failed) lines.push(`  ❌ failed：${failed.version} — ${failed.reason}`)
    emit({ ok: true, dir, pending, attempt, healthy: ok ? { version: ok.version, at: ok.at } : null, failed }, lines)
  },

  rollback() {
    const cur = localVersion()
    const target = args[0] || listCached().find((x) => cmpVersion(x.version, cur) < 0)?.version
    if (!target) die('缓存里没有比当前更旧的版本可回滚')
    const meta = installOrDie(target)
    emit({ ok: true, rolledBackTo: target, sha256: meta.sha256 }, [
      `已回滚到 ${target}（当前为 ${cur}）`,
      '⚠️  同样需要重启宿主才生效。',
    ])
  },

  /**
   * 激活待激活版本（由启动流程 wb_boot.sh 调用，不要在运行中的会话里调用）。
   *
   * 为什么单独拆出这一步：切换只能发生在宿主还没起来的时候。若在运行中调用，
   * 装完不生效、下一次重启才突然变，行为就不可观测了。
   */
  activate() {
    const dir = opt.stagedDir || path.join(opt.cache, 'staged')
    const pending = readState(dir, 'pending.json')
    if (!pending) {
      emit({ ok: true, activated: null, reason: 'no-pending' }, ['没有待激活版本，跳过'])
      return
    }
    if (!opt.yes) {
      die(`将执行：\n  ${opt.cmd.replace('{tgz}', pending.tgz)}\n激活会切换运行版本，确认后加 --yes`, 12)
    }
    // 已经试过足够多次还没自证健康：不要再装一遍，交给 healthcheck 回滚。
    // 否则每次启动都重装一遍坏版本，永远轮不到回滚。
    const attempted = readState(dir, 'attempt.json')
    if (attempted && attempted.version === pending.version && Number(attempted.count || 0) >= Number(opt.maxAttempts || 2)) {
      die(
        `${pending.version} 已尝试 ${attempted.count} 次仍未自证健康，拒绝再次激活。\n` +
          `先跑：lanhub-update healthcheck（会回滚到 ${pending.previous}）`,
      )
    }
    if (!fs.existsSync(pending.tgz)) die(`待激活制品缺失：${pending.tgz}`)
    const sha = sha256File(pending.tgz)
    if (sha !== pending.sha256) die(`待激活制品校验失败：\n  got  ${sha}\n  want ${pending.sha256}`)

    const command = opt.cmd.replace(/\{tgz\}/g, pending.tgz)
    if (!opt.quiet) process.stderr.write(`$ ${command}\n`)
    const r = spawnSync(command, { shell: true, stdio: 'inherit' })
    if (r.error) die(`执行安装命令失败：${r.error.message}`)
    if (r.status !== 0) die(`安装命令退出码 ${r.status}`)

    const prev = readState(dir, 'attempt.json')
    const count = prev && prev.version === pending.version ? Number(prev.count || 0) + 1 : 1
    writeJSONAtomic(path.join(dir, 'attempt.json'), { version: pending.version, count, at: new Date().toISOString() })
    emit({ ok: true, activated: pending.version, attempt: count }, [
      `已激活 ${pending.version}（第 ${count} 次尝试）`,
      '启动后插件会写 ok.json 自证健康；连续失败会自动回滚。',
    ])
  },

  /** 标记当前待激活版本健康。通常由插件启动成功注册后写，这里提供等价的手工路径。 */
  confirm() {
    const dir = opt.stagedDir || path.join(opt.cache, 'staged')
    const pending = readState(dir, 'pending.json')
    if (!pending) die('没有待激活版本可确认')
    const v = args[0] || opt.current || localVersion()
    if (v !== pending.version) {
      die(`版本不匹配：待激活 ${pending.version}，实际 ${v}——不写 ok，交给回滚逻辑处理`)
    }
    writeJSONAtomic(path.join(dir, 'ok.json'), { version: v, at: new Date().toISOString() })
    emit({ ok: true, confirmed: v }, [`✅ ${v} 已确认健康`])
  },

  /**
   * 启动自检：新版本起不来就自动回滚。
   *
   * 这是自动更新唯一可靠的安全网。签名防的是"包被换过"，自检防的是"包本身有问题"
   * ——新版本若连不上 Hub，它就再也拉不到修复版本（更新源也在 Hub 上），会永久离线。
   * 所以必须能在无人干预下退回上一个好版本。
   */
  healthcheck() {
    const dir = opt.stagedDir || path.join(opt.cache, 'staged')
    const pending = readState(dir, 'pending.json')
    const maxAttempts = Number(opt.maxAttempts || 2)
    if (!pending) {
      emit({ ok: true, action: 'idle', reason: 'no-pending' }, ['没有待激活版本，无需自检'])
      return
    }

    const ok = readState(dir, 'ok.json')
    if (ok && ok.version === pending.version) {
      // 健康：清掉待激活痕迹，本次更新闭环
      removeState(dir, 'pending.json')
      removeState(dir, 'attempt.json')
      removeState(dir, 'ok.json')
      emit({ ok: true, action: 'settled', version: pending.version }, [`✅ ${pending.version} 运行正常，更新闭环`])
      return
    }

    const attempt = readState(dir, 'attempt.json')
    const count = attempt && attempt.version === pending.version ? Number(attempt.count || 0) : 0
    if (count < maxAttempts) {
      emit({ ok: true, action: 'observing', version: pending.version, attempt: count, maxAttempts }, [
        `观察中：${pending.version} 已尝试 ${count}/${maxAttempts} 次，尚未自证健康`,
      ])
      return
    }

    // 达到上限 -> 回滚
    const target = pending.previous
    const lines = [`❌ ${pending.version} 连续 ${count} 次未自证健康，回滚到 ${target}`]
    let rolledBack = false
    if (target && readMeta(target)) {
      try {
        opt.yes = true
        runInstall(target)
        rolledBack = true
        lines.push(`已装回 ${target}`)
      } catch (e) {
        lines.push(`回滚失败：${e.message}`)
      }
    } else {
      lines.push(`缓存里没有 ${target}，无法自动回滚——请手工安装上一个好版本`)
    }
    writeJSONAtomic(path.join(dir, 'failed.json'), {
      version: pending.version,
      attempt: count,
      reason: '连续启动未自证健康',
      rolledBackTo: rolledBack ? target : null,
      at: new Date().toISOString(),
    })
    removeState(dir, 'pending.json')
    removeState(dir, 'attempt.json')
    emit({ ok: !rolledBack ? false : true, action: rolledBack ? 'rolled-back' : 'rollback-failed', version: pending.version, rolledBackTo: rolledBack ? target : null }, lines)
    if (!rolledBack) process.exit(1)
  },

  list() {
    const cur = localVersion()
    const items = listCached()
    const lines = [`缓存：${opt.cache}`, `本地版本：${cur}`]
    if (!items.length) lines.push('（空）')
    for (const it of items) {
      const mark = it.version === cur ? '*' : ' '
      const exists = fs.existsSync(tgzPath(it.version))
      lines.push(`  ${mark} ${it.version.padEnd(10)} ${String(it.size).padStart(8)} B  ${exists ? '' : '[文件缺失]'}${it.fetchedAt || ''}`)
    }
    emit({ ok: true, cache: opt.cache, current: cur, items }, lines)
  },

  verify() {
    const items = listCached()
    const bad = []
    const lines = [`缓存：${opt.cache}`]
    for (const it of items) {
      try {
        verifyStaged(it.version)
        lines.push(`  ✅ ${it.version}`)
      } catch (e) {
        bad.push({ version: it.version, error: e.message })
        lines.push(`  ❌ ${it.version}: ${e.message}`)
      }
    }
    if (!items.length) lines.push('（空）')
    emit({ ok: bad.length === 0, errors: bad }, lines)
    process.exit(bad.length ? 1 : 0)
  },

  help() {
    const lines = [
      'lanhub-update —— dsh-plugin-lan-hub 更新通道',
      '',
      '用法：',
      '  check                     看是否有更新（exit 10 = 有更新，11 = 不兼容，13 = 不可信）',
      '  fetch [version]           验签 + 下载 + 校验到缓存（不动安装目录）',
      '  stage [version]           预置为待激活（写 pending.json，不切换）',
      '  apply [version] --yes     立刻用缓存里的包执行安装',
      '  activate --yes            激活待激活版本（供 wb_boot.sh 在启动前调用）',
      '  confirm [version]         标记健康（插件启动成功后也会自动写）',
      '  healthcheck               自检：连续起不来则自动回滚',
      '  pending                   查看待激活 / 尝试 / 健康 / 失败状态',
      '  rollback [--to version]   回滚到更旧的缓存版本',
      '  list                      查看缓存',
      '  verify                    重新校验缓存完整性',
      '  trust                     查看当前信任根链（来源 + 指纹）',
      '',
      '参数：--manifest <url>  --cache <dir>  --current <ver>  --cmd <tpl>',
      '      --pubkey <pem|path>  --allow-unsigned  --downgrade  --max-attempts <n>',
      '      --staged-dir <dir>  --json  --yes  --quiet',
      '',
      '信任根 = 配置/内置公钥（锚） + 已接受的轮换链（缓存 trusted-key.json）。',
      '轮换：发布侧 lanhub-publish --rotate-to <新公钥>（用当前私钥背书），',
      '      客户端 check 只预演、fetch 才落盘；撤销：lanhub-publish --revoke <keyId>。',
      '',
      `清单：${opt.manifest}`,
      `缓存：${opt.cache}`,
      `公钥：${opt.pubkey || '（未显式配置——会用轮换链或包内置公钥）'}`,
    ]
    emit({ ok: true, manifest: opt.manifest, cache: opt.cache, commands: Object.keys(COMMANDS) }, lines)
  },
}

const run = COMMANDS[cmd]
if (!run) die(`unknown command: ${cmd}（可用：${Object.keys(COMMANDS).join(' ')}）`)
await run()
