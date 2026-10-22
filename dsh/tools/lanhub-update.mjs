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
 *   --manifest <url>   清单地址（默认 $LAN_HUB_UPDATE_MANIFEST 或 http://127.0.0.1:8081/api/updates）
 *   --cache <dir>      缓存目录（默认 ~/.dsh/updates）
 *   --current <ver>    覆盖本地版本（离线/异常时兜底）
 *   --cmd <tpl>        安装命令模板，用 {tgz} 占位（默认 dsh plugin --profile web add {tgz}）
 *   --json             输出 JSON
 *   --yes              确认执行安装（apply/rollback 必需）
 *   --quiet            安静模式
 *
 * 退出码：0 成功/已是最新；1 错误；10 有可用更新；11 版本不兼容；12 需要 --yes。
 */

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'

const DEFAULT_MANIFEST = 'http://127.0.0.1:8081/api/updates'
const DEFAULT_CMD = 'dsh plugin --profile web add {tgz}'

// ---------- 参数解析 ----------
const opt = {
  json: false,
  quiet: false,
  yes: false,
  manifest: process.env.LAN_HUB_UPDATE_MANIFEST || DEFAULT_MANIFEST,
  cache: process.env.LAN_HUB_UPDATE_CACHE || path.join(os.homedir(), '.dsh', 'updates'),
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
    else if (a === '--cmd') opt.cmd = argv[++i] ?? DEFAULT_CMD
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
  return { url: opt.manifest, hubProto, latest: String(body.latest || ''), proto: Number(body.proto || 0), minHost: String(body.minHost || ''), artifacts }
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

async function download(v, m) {
  const art = m.artifacts[v]
  if (!art) die(`清单里没有版本 ${v}（可用：${Object.keys(m.artifacts).join(', ') || '无'}）`)
  if (!art.sha256) die(`清单里 ${v} 缺少 sha256 —— 没有校验就不下载，这是硬性要求`)
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
  fs.writeFileSync(
    metaPath(v),
    JSON.stringify({ version: v, sha256: sha, size: buf.length, url, manifest: m.url, fetchedAt: new Date().toISOString() }, null, 2) + '\n',
  )
  return { version: v, path: tgzPath(v), sha256: sha, size: buf.length, url }
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

    const lines = [
      `清单：${m.url}`,
      `本地：${cur}`,
      `最新：${latest || '（无可用更新）'}${m.hubProto ? `   hub proto=${m.hubProto}` : ''}`,
    ]
    if (art?.notes) lines.push(`说明：${art.notes}`)
    if (incompat) lines.push(`⚠️  ${incompat}`)
    if (!hasUpdate) lines.push(incompat ? '' : '✅ 已是最新版本')

    const data = { ok: true, current: cur, latest, hubProto: m.hubProto, incompatible: Boolean(incompat), hasUpdate, url: m.url }
    emit(data, lines.filter((x) => x !== ''))
    if (incompat) process.exit(11)
    process.exit(hasUpdate ? 10 : 0)
  },

  async fetch() {
    const m = await fetchManifest()
    const v = args[0] || m.latest
    if (!v) die('清单没有 latest，且未指定版本')
    const incompat = checkCompat(m, m.artifacts[v])
    if (incompat) die(incompat, 11)
    const got = await download(v, m)
    emit({ ok: true, ...got }, [`已暂存 ${got.version} -> ${got.path}`, `sha256 ${got.sha256}`, '（仅下载，未安装；下一步：lanhub-update apply ' + got.version + ' --yes）'])
  },

  apply() {
    const v = args[0] || localVersion()
    if (!readMeta(v)) die(`缓存里没有 ${v}，先执行 fetch`)
    const meta = installOrDie(v)
    emit({ ok: true, applied: v, sha256: meta.sha256 }, [
      `已安装 ${v}`,
      '',
      '⚠️  需要重启宿主（dsh / DSHA）才会加载新代码——代码不像 policy 那样能热替换。',
      '   重启后用 hub_stats 可确认版本号。',
    ])
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
      '  check                     看是否有更新（exit 10 = 有更新，11 = 不兼容）',
      '  fetch [version]           下载并校验到缓存（不动安装目录）',
      '  apply [version] --yes     用缓存里的包执行安装',
      '  rollback [--to version]   回滚到更旧的缓存版本',
      '  list                      查看缓存',
      '  verify                    重新校验缓存完整性',
      '',
      '参数：--manifest <url>  --cache <dir>  --current <ver>  --cmd <tpl>  --json  --yes',
      `清单：${opt.manifest}`,
      `缓存：${opt.cache}`,
    ]
    emit({ ok: true, manifest: opt.manifest, cache: opt.cache, commands: Object.keys(COMMANDS) }, lines)
  },
}

const run = COMMANDS[cmd]
if (!run) die(`unknown command: ${cmd}（可用：${Object.keys(COMMANDS).join(' ')}）`)
await run()
