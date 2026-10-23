/**
 * 插件内的更新同步器。
 *
 * 职责边界（刻意划得很死，这是本模块存在的理由）：
 *   做：拉清单 → 比版本 → 验签 → 下载到缓存 → 预置到待激活区 → 启动后自证健康
 *   不做：安装、切换、重启宿主、执行任何子进程
 *
 * 为什么不做安装：
 *   1) 代码不像 policy 那样能热替换——ESM 模块缓存 + 插件持有的 TCP 长连接，
 *      装完不重启就不生效，等于埋一个"下次重启悄悄变"的雷；
 *   2) 插件没有重启宿主的能力，所以"自动安装"在当前架构里没有合法执行者；
 *   3) 让长跑进程用自身权限替换自己的代码，会把投毒面扩大到任何能连上 Hub 的设备。
 * 因此切换点被刻意挪到「宿主下次启动」，由 wb_boot.sh 执行 activate。
 *
 * 为什么信任判定必须在这里做：
 *   sha256 只防截断不防投毒（清单与制品同源）。所以只有验过签的包才允许进入
 *   待激活区——待激活区是"下次启动就会跑的代码"，门槛必须和"立刻执行"一样高。
 */

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import http from 'node:http'
import https from 'node:https'
import { createHash } from 'node:crypto'
import { SIG_ALGO } from './signing.mjs'
import {
  loadTrustChain,
  verifyWithChainDetailed,
  acceptKeyRotation,
  loadRevocations,
  applyManifestRevocations,
} from './keyring.mjs'
import { cmpVersion } from './version.mjs'

/** 缓存里保留的版本数（含当前）。防止长期运行把磁盘吃满。 */
const KEEP_VERSIONS = 3
const DEFAULT_TIMEOUT_MS = 15_000

function mkdirp(p) {
  fs.mkdirSync(p, { recursive: true })
}

function readJSON(p) {
  try {
    const o = JSON.parse(fs.readFileSync(p, 'utf8'))
    return o && typeof o === 'object' ? o : null
  } catch {
    return null
  }
}

/** 原子写：启动流程会读这些状态文件，绝不能读到半截内容 */
function writeJSONAtomic(p, obj) {
  mkdirp(path.dirname(p))
  const tmp = `${p}.${process.pid}.tmp`
  fs.writeFileSync(tmp, JSON.stringify(obj, null, 2) + '\n')
  fs.renameSync(tmp, p)
}

/**
 * 极简 GET：直连（**不**走 HTTP_PROXY），返回 {status, headers, body}。
 *
 * 消费侧同步器连的是 LAN Hub，必须直连——Node 的默认 fetch 会读 HTTP_PROXY /
 * HTTPS_PROXY，开发机常带代理，结果就是「本机 Hub 明明活着，同步却 fetch failed」。
 * 这与 publish.mjs 的 httpGet 同语义（发布自检侧已踩过这个坑，见其注释）。
 */
function httpGet(url, o = {}) {
  return new Promise((resolve, reject) => {
    let u
    try {
      u = new URL(url)
    } catch {
      reject(new Error(`URL 无法解析：${url}`))
      return
    }
    const mod = u.protocol === 'https:' ? https : http
    const req = mod.get(
      u,
      { timeout: o.timeoutMs ?? DEFAULT_TIMEOUT_MS, headers: o.headers },
      (res) => {
        const chunks = []
        res.on('data', (c) => chunks.push(c))
        res.on('end', () => resolve({ status: res.statusCode || 0, headers: res.headers, body: Buffer.concat(chunks) }))
      },
    )
    req.on('timeout', () => req.destroy(new Error(`请求超时 ${o.timeoutMs ?? DEFAULT_TIMEOUT_MS}ms：${url}`)))
    req.on('error', reject)
  })
}

export function createUpdater(opts) {
  const version = String(opts.version || '0.0.0')
  const name = String(opts.name || 'dsh-plugin-lan-hub')
  // 这两个是可变的：热更新通道改动时会 reconfigure，路径变了要跟着走
  let cacheDir = opts.cacheDir || path.join(os.homedir(), '.dsh', 'updates')
  let stagedDir = opts.stagedDir || path.join(cacheDir, 'staged')
  const pkgDir = () => path.join(cacheDir, 'pkg')
  const metaDir = () => path.join(cacheDir, 'meta')
  const tgzPath = (v) => path.join(pkgDir(), `${name}-${v}.tgz`)
  const metaPath = (v) => path.join(metaDir(), `${v}.json`)
  const log = opts.log || (() => {})

  let timer = null
  let inFlight = false
  /** 上一次结论；只在变化时打日志，避免每 10 分钟刷屏 */
  let lastStatus = ''
  const state = { lastCheck: 0, lastStatus: 'never', lastError: '', latest: '', staged: '' }

  async function fetchManifest() {
    const url = String(opts.manifestUrl || '')
    if (!url) throw new Error('清单地址为空（配置 updateManifest 或让它按 host 推导）')
    const res = await httpGet(url, {
      headers: { accept: 'application/json' },
      timeoutMs: DEFAULT_TIMEOUT_MS,
    })
    if (res.status < 200 || res.status >= 300) {
      throw new Error(`清单 HTTP ${res.status} ${res.statusMessage || ''}`)
    }
    let body
    try {
      body = JSON.parse(res.body.toString('utf8'))
    } catch {
      throw new Error('清单不是合法 JSON')
    }
    if (!body || typeof body !== 'object') throw new Error('清单不是 JSON 对象')
    return {
      url,
      name: String(body.name || name),
      hubProto: Number(res.headers['x-proto-version'] || 0),
      latest: String(body.latest || ''),
      proto: Number(body.proto || 0),
      // 密钥轮换背书：由**当前**私钥签发的新公钥。先于验包处理，见 syncOnce。
      nextKey: body.nextKey && typeof body.nextKey === 'object' ? body.nextKey : null,
      // 撤销声明：由信任链上的私钥签名，先于验包并入本地（只增不减）。
      revocations: Array.isArray(body.revocations) ? body.revocations : [],
      artifacts: body.artifacts && typeof body.artifacts === 'object' ? body.artifacts : {},
    }
  }

  /**
   * 信任判定。返回 {ok, level, reason}。
   *
   * level:
   *   verified         有公钥且签名匹配 —— 唯一可无人值守进入待激活区的情形
   *   unsigned-forced  显式允许未签名（仅开发）
   *   rejected         其余
   */
  function judgeTrust(m, v, art, chain, revoked) {
    const roots = chain && chain.length ? chain : loadTrustChain({ cacheDir, config: opts.publicKey }).chain
    const revokedSet = revoked instanceof Set ? revoked : loadRevocations(cacheDir)
    if (!roots.length) {
      return opts.allowUnsigned
        ? { ok: true, level: 'unsigned-forced' }
        : { ok: false, reason: '没有可信公钥（配置/内置/轮换链都为空），无法判断制品来源——拒绝自动更新' }
    }
    if (!art.sig) {
      return opts.allowUnsigned
        ? { ok: true, level: 'unsigned-forced' }
        : { ok: false, reason: `${v} 未签名，但本机已 pin 公钥——拒绝` }
    }
    if (art.sigAlgo && art.sigAlgo !== SIG_ALGO) {
      return { ok: false, reason: `不支持的签名算法 ${art.sigAlgo}` }
    }
    const payload = {
      name: m.name,
      version: v,
      sha256: String(art.sha256 || '').toLowerCase(),
      proto: Number(art.proto ?? m.proto ?? 0),
    }
    const det = verifyWithChainDetailed(payload, art.sig, roots)
    if (!det.ok) {
      return { ok: false, reason: `${v} 签名校验失败：制品与清单不匹配，或不是本机信任链上的私钥签发` }
    }
    if (revokedSet.has(det.keyId)) {
      return { ok: false, reason: `${v} 的签名根（keyId ${det.keyId}）已被撤销，拒绝` }
    }
    return { ok: true, level: 'verified' }
  }

  async function syncOnce() {
    if (inFlight) return { status: 'skipped' }
    inFlight = true
    try {
      const m = await fetchManifest()
      state.lastCheck = Date.now()
      const latest = m.latest
      state.latest = latest
      if (!latest) return settle('up-to-date', { latest: '' })

      // 只升不降：签名防伪造，防不住把 latest 指回旧的、但确实签过的版本
      if (cmpVersion(latest, version) <= 0) return settle('up-to-date', { latest })

      const art = m.artifacts[latest]
      if (!art) return settle('inconsistent', { latest, reason: 'latest 在 artifacts 中不存在' })

      // proto：装完发现连不上是最糟的失败模式，必须在下载前就拒绝
      const need = Number(art.proto ?? m.proto ?? 0)
      if (need && m.hubProto && need > m.hubProto) {
        return settle('incompatible', { latest, reason: `需要 proto ${need}，Hub 只支持 ${m.hubProto}` })
      }

      // 轮换必须**先于**验包：这一版的包很可能就是由新私钥签的，先用旧链验包会
      // 直接判 untrusted，轮换永远轮不到。接受轮换后 chain 变成 [新根, ...旧根]。
      const trustInfo = loadTrustChain({ cacheDir, config: opts.publicKey })
      let chain = trustInfo.chain
      let rotation = null
      if (m.nextKey) {
        const r = acceptKeyRotation({ cacheDir, nextKey: m.nextKey, config: opts.publicKey })
        if (r.ok && !r.unchanged) {
          chain = r.chain
          rotation = { from: r.pinned.from, to: r.pinned.to }
          log(`[lan-hub] update: 已接受密钥轮换 ${rotation.from} → ${rotation.to}`)
        } else if (!r.ok) {
          // 轮换失败不阻断：继续用旧链，旧链签的包依然该装。
          state.lastError = `密钥轮换被拒：${r.reason}`
        }
      }

      // 撤销：把清单里合法签名的吊销声明并入本地（只增不减）。
      const revoked = applyManifestRevocations(m.revocations, chain, cacheDir, true)

      const trust = judgeTrust(m, latest, art, chain, revoked)
      if (!trust.ok) return settle('untrusted', { latest, reason: trust.reason })

      if (!opts.autoFetch) return settle('available', { latest, trust: trust.level })

      const got = await download(latest, m, art, trust.level)
      if (!opts.autoStage) return settle('fetched', { latest, ...got })

      // 先把回滚包备好，再预置。顺序重要：一旦 pending.json 就位，下次启动就会切换，
      // 此时若没有上一版本的包，自检回滚会陷入"想回滚但手上没有旧包"。
      await ensurePrevious(m, chain)

      const staged = stage(latest, got, trust.level)
      return settle('staged', { latest, ...got, staged, ...(rotation ? { rotation } : {}) })
    } catch (e) {
      state.lastError = e.message
      return settle('error', { reason: e.message })
    } finally {
      inFlight = false
    }
  }

  function settle(status, extra) {
    state.lastStatus = status
    // 只在结论变化时打日志："每次同步都刷一行"很快就会变成噪音
    if (status !== lastStatus) {
      lastStatus = status
      const detail = extra && extra.reason ? ` — ${extra.reason}` : ''
      log(`[lan-hub] update: ${status}${extra?.latest ? ` (latest=${extra.latest})` : ''}${detail}`)
    }
    return { status, ...extra }
  }

  async function download(v, m, art, level) {
    const url = new URL(String(art.url || ''), m.url).toString()
    const res = await httpGet(url, { timeoutMs: DEFAULT_TIMEOUT_MS * 8 })
    if (res.status < 200 || res.status >= 300) throw new Error(`下载 HTTP ${res.status}`)
    const buf = res.body
    if (art.size && buf.length !== Number(art.size)) {
      throw new Error(`大小不符：收到 ${buf.length}，清单声明 ${art.size}`)
    }
    const sha = createHash('sha256').update(buf).digest('hex')
    if (sha !== String(art.sha256).toLowerCase()) {
      throw new Error(`sha256 不符，拒绝暂存（got ${sha.slice(0, 12)}…）`)
    }
    mkdirp(pkgDir())
    mkdirp(metaDir())
    const tmp = `${tgzPath(v)}.${process.pid}.tmp`
    fs.writeFileSync(tmp, buf)
    fs.renameSync(tmp, tgzPath(v))
    writeJSONAtomic(metaPath(v), {
      version: v,
      name: m.name,
      sha256: sha,
      size: buf.length,
      proto: Number(art.proto ?? m.proto ?? 0),
      url,
      manifest: m.url,
      sig: String(art.sig || ''),
      sigAlgo: String(art.sigAlgo || ''),
      trust: level,
      fetchedAt: new Date().toISOString(),
    })
    prune()
    return { sha256: sha, size: buf.length }
  }

  /** 只保留最近 KEEP_VERSIONS 个版本，避免长期运行撑满磁盘 */
  function prune() {
    try {
      const all = fs
        .readdirSync(metaDir())
        .filter((f) => f.endsWith('.json'))
        .map((f) => f.slice(0, -5))
        .sort(cmpVersion)
      for (const v of all.slice(0, Math.max(0, all.length - KEEP_VERSIONS))) {
        fs.rmSync(tgzPath(v), { force: true })
        fs.rmSync(metaPath(v), { force: true })
      }
    } catch {
      /* 缓存目录还没建起来，忽略 */
    }
  }

  /**
   * 确保当前版本（切换后的 previous）也在缓存里。
   *
   * 它通常从没被"下载"过——因为它是当前正在跑的那个。不在这里补下载，
   * 回滚就无从谈起。失败只记日志，不阻断本次预置。
   */
  async function ensurePrevious(m, chain) {
    const prev = version
    if (!prev || prev === '0.0.0') return false
    if (readJSON(metaPath(prev)) && fs.existsSync(tgzPath(prev))) return true
    const art = m.artifacts[prev]
    if (!art) {
      state.lastError = `清单里没有 ${prev}，回滚需手工安装`
      return false
    }
    try {
      const trust = judgeTrust(m, prev, art, chain, loadRevocations(cacheDir))
      // 手上根本没有可信公钥时（纯本地开发）不因为"验不过"就放弃回滚包——
      // 回滚是救命通道，此时信任问题本来就不成立。
      const hasRoot = (chain || loadTrustChain({ cacheDir, config: opts.publicKey }).chain).length > 0
      if (!trust.ok && hasRoot) {
        state.lastError = `回滚包未通过信任校验：${trust.reason}`
        return false
      }
      await download(prev, m, art, trust.level || 'unsigned-forced')
      return true
    } catch (e) {
      state.lastError = `回滚包准备失败：${e.message}`
      return false
    }
  }

  function stage(v, got, level) {
    mkdirp(stagedDir)
    const dst = path.join(stagedDir, `${name}-${v}.tgz`)
    fs.copyFileSync(tgzPath(v), dst)
    const pending = {
      name,
      version: v,
      sha256: got.sha256,
      sig: String(readJSON(metaPath(v))?.sig || ''),
      sigAlgo: String(readJSON(metaPath(v))?.sigAlgo || ''),
      proto: readJSON(metaPath(v))?.proto ?? 0,
      tgz: dst,
      previous: version,
      trust: level,
      stagedAt: new Date().toISOString(),
      stagedBy: 'plugin-autosync',
    }
    writeJSONAtomic(path.join(stagedDir, 'pending.json'), pending)
    state.staged = v
    return dst
  }

  /**
   * 启动后自证健康。
   *
   * 这是自动更新唯一可靠的安全网：新版本若连不上 Hub，它也拉不到修复版本
   * （更新源就在 Hub 上），会永久离线。所以只要"能跑起来并完成注册"就算健康，
   * 由本函数写 ok.json；启动流程据此判定要不要回滚。
   */
  function markHealthy() {
    try {
      const pending = readJSON(path.join(stagedDir, 'pending.json'))
      if (!pending) return false
      // 版本必须一致：若实际跑的还是旧版，说明切换没生效，不能算健康
      if (pending.version !== version) return false
      writeJSONAtomic(path.join(stagedDir, 'ok.json'), { version, at: new Date().toISOString() })
      log(`[lan-hub] update: ${version} 已自证健康`)
      return true
    } catch (e) {
      log(`[lan-hub] update: 写 ok.json 失败 — ${e.message}`)
      return false
    }
  }

  function start(intervalMs) {
    stop()
    if (!opts.autoSync) return
    const every = Math.min(86_400_000, Math.max(60_000, Number(intervalMs) || 600_000))
    timer = setInterval(() => void syncOnce(), every)
    timer.unref?.()
    // 启动后延迟一轮再查：刚起来时 Hub 可能还没就绪，立刻失败只会污染日志
    setTimeout(() => void syncOnce(), Math.min(every, 30_000)).unref?.()
  }

  function stop() {
    if (timer) {
      clearInterval(timer)
      timer = null
    }
  }

  /**
   * 热更新同步器自身配置。
   * 只接受"不需要重连"的那些字段——连接层变了应该走插件重启，不是这里。
   */
  function reconfigure(next) {
    if (!next) return
    for (const k of ['manifestUrl', 'publicKey', 'autoFetch', 'autoStage']) {
      if (next[k] !== undefined) opts[k] = next[k]
    }
    if (next.cacheDir !== undefined) cacheDir = next.cacheDir || path.join(os.homedir(), '.dsh', 'updates')
    if (next.stagedDir !== undefined) stagedDir = next.stagedDir || path.join(cacheDir, 'staged')
    // 换了配置就当作一次新结论，确保下一次结果会被记录/打印
    lastStatus = ''
  }

  return { syncOnce, markHealthy, start, stop, reconfigure, state }
}
