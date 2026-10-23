/**
 * 发布侧核心：把 .tgz 签名成可分发制品，并增量写入更新清单。
 *
 * 这里只放**纯逻辑**（无参数解析、无 process.exit、无输出），因为它有两个调用方：
 *   - tools/lanhub-publish.mjs：发布 CLI，负责交互与报错；
 *   - tools/lanhub-release.mjs：一条龙（构建→打包→发布→验证），要在同进程里复用。
 * 抽出来的直接原因是：两条命令必须写出**同一份**清单与签名，否则"发布"这个动作
 * 就有两个真相，排查分发事故时根本无从判断是哪条路径写的。
 *
 * 与消费侧（src/updater.mjs / lanhub-update）的分工：本文件持有私钥，是把"可信"
 * 注入分发链的地方；消费侧只持公钥。两者永不共享私钥。
 */

import fs from 'node:fs'
import path from 'node:path'
import http from 'node:http'
import https from 'node:https'
import { createHash } from 'node:crypto'
import { signArtifact, verifyArtifact, signKeyRotation, signRevocation, keyIdOf, SIG_ALGO, resolvePublicKey } from './signing.mjs'
import { cmpVersion } from './version.mjs'

/** 清单文件名。与 internal/plugins/httpgateway/updates.go 的 updateManifestFile 一致。 */
export const MANIFEST_FILE = 'lanhub-update.json'
/** 制品子目录。同上，与 Go 侧 "pkg" 一致。 */
export const PKG_DIR = 'pkg'
/** 清单里的默认制品名。 */
export const DEFAULT_NAME = 'dsh-plugin-lan-hub'

/**
 * 从 dsh-plugin-lan-hub-<ver>.tgz 解析版本。
 *
 * 文件名即真相，不解 tgz——省掉 tar 依赖，也让"发布错了版本"在入口就被拦下。
 */
export function versionOfFile(file) {
  const base = path.basename(file, '.tgz')
  const m = base.match(/(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$/)
  if (!m) throw new Error(`文件名里解析不出版本号：${base}（期望 ...-<x.y.z>.tgz）`)
  return m[1]
}

/** 读取既有清单；不存在时返回空骨架，损坏时抛错（拒绝静默重建，那是分发事故）。 */
export function loadManifest(outDir, defaults = {}) {
  const manifestPath = path.join(outDir, MANIFEST_FILE)
  const empty = {
    name: defaults.name || DEFAULT_NAME,
    latest: '',
    proto: defaults.proto ?? 3,
    minHost: defaults.minHost || '0.2.0',
    keyId: '',
    revocations: [],
    artifacts: {},
  }
  if (!fs.existsSync(manifestPath)) return { manifest: empty, manifestPath, existed: false }
  let prev
  try {
    prev = JSON.parse(fs.readFileSync(manifestPath, 'utf8'))
  } catch (e) {
    throw new Error(`既有清单无法解析，拒绝继续：${manifestPath}\n${e.message}\n（修好或删掉该文件再发布）`)
  }
  return {
    manifest: { ...empty, ...prev, artifacts: { ...(prev.artifacts || {}) } },
    manifestPath,
    existed: true,
  }
}

/**
 * 发布若干制品。
 *
 * @param {object} o
 * @param {string[]} o.files        .tgz 绝对路径列表
 * @param {string}   o.privateKeyPem Ed25519 私钥 PEM
 * @param {string}   o.outDir        分发目录（内含 lanhub-update.json 与 pkg/）
 * @param {string}  [o.latest]       显式指定 latest；默认取清单中版本号最大者
 * @param {number}  [o.proto]        制品要求线协议版本
 * @param {string}  [o.minHost]      最低宿主版本
 * @param {string}  [o.notes]        变更说明
 * @param {boolean} [o.replace]      允许覆盖已发布的同名版本
 * @param {string}  [o.name]         清单里的制品名
 * @param {string}  [o.pubkeyHint]   公钥（PEM 或路径），仅用于算 keyId 提示，不下发
 * @param {string}  [o.rotateTo]     下一把公钥（PEM 或路径）；给了就由**当前私钥**签名背书
 *                                   并写进清单的 nextKey，客户端凭旧根验过后接力
 * @param {string[]}[o.revoke]       要吊销的 keyId 列表（每段 8 位 hex）；用**当前私钥**签发
 *                                   吊销声明写入清单 revocations，客户端验过才并入本地
 * @returns {{manifest:object,manifestPath:string,published:object[],skipped:string[]}}
 *
 * 注意 latest 的一致性：Go 侧会在 latest 不在 artifacts 里时返回 500（不静默降级），
 * 所以这里宁可报错退出，也不写一个自相矛盾的清单。
 */
export function publishArtifacts(o) {
  const { files, privateKeyPem, outDir } = o
  const proto = o.proto ?? 3
  const name = o.name || DEFAULT_NAME

  const pkgOutDir = path.join(outDir, PKG_DIR)
  fs.mkdirSync(pkgOutDir, { recursive: true })
  const { manifest, manifestPath } = loadManifest(outDir, { name, proto, minHost: o.minHost })

  const published = []
  const skipped = []
  for (const file of files) {
    const abs = path.resolve(file)
    if (!fs.existsSync(abs)) throw new Error(`制品不存在：${abs}`)
    const version = versionOfFile(abs)
    const body = fs.readFileSync(abs)
    const sha256 = createHash('sha256').update(body).digest('hex')
    const size = body.length

    if (manifest.artifacts[version] && !o.replace) {
      skipped.push(`${version}（已存在，加 --replace 覆盖）`)
      continue
    }

    // 签名载荷只有 name/version/sha256/proto——改其中任何一个，客户端都会验签失败。
    const sig = signArtifact({ name, version, sha256, proto }, privateKeyPem)
    const fileBase = path.basename(abs)
    fs.copyFileSync(abs, path.join(pkgOutDir, fileBase))
    manifest.artifacts[version] = {
      url: `/api/updates/pkg/${fileBase}`,
      sha256,
      size,
      builtAt: new Date().toISOString(),
      notes: o.notes || '',
      proto,
      minHost: o.minHost || manifest.minHost || '0.2.0',
      sig,
      sigAlgo: SIG_ALGO,
    }
    published.push({ version, sha256, size, sig, file: abs })
  }

  if (!published.length) {
    const why = skipped.length ? `跳过：${skipped.join(', ')}` : '没有制品'
    throw new Error(`没有发布任何版本。${why}`)
  }

  const versions = Object.keys(manifest.artifacts).sort(cmpVersion)
  manifest.latest = o.latest || versions[versions.length - 1]
  if (!manifest.artifacts[manifest.latest]) {
    throw new Error(`latest=${manifest.latest} 在 artifacts 中不存在（Hub 会因此返回 500，属分发事故）`)
  }
  manifest.proto = manifest.artifacts[manifest.latest].proto
  manifest.minHost = manifest.artifacts[manifest.latest].minHost || manifest.minHost

  // 密钥轮换：用**当前私钥**给下一把公钥背书。客户端手里 pin 的是当前公钥，
  // 它不需要相信清单里的新公钥，只需验证"这是现任签发的接班人"。
  if (o.rotateTo) {
    const newPem = resolvePublicKey(o.rotateTo)
    if (!newPem) throw new Error(`--rotate-to 取不到公钥：${o.rotateTo}`)
    manifest.nextKey = {
      publicKey: newPem.trim(),
      sig: signKeyRotation(newPem, privateKeyPem),
      sigAlgo: SIG_ALGO,
      keyId: keyIdOf(newPem),
      rotatedAt: new Date().toISOString(),
    }
  } else if (manifest.nextKey && o.keepNextKey !== true) {
    // 默认清掉上一次的轮换背书：它已被接受过，留着只会让每次同步都重复落盘。
    delete manifest.nextKey
  }

  // 撤销：用**当前私钥**给每个要吊销的 keyId 签名。客户端只认"由信任链上私钥签的"
  // 吊销声明，恶意 Hub 无法凭空吊销诚实钥匙。既有 revocations 保留（并集）。
  const existingRevokes = Array.isArray(manifest.revocations) ? manifest.revocations : []
  const merged = [...existingRevokes]
  for (const id of o.revoke || []) {
    const clean = String(id || '').trim().toLowerCase()
    if (!/^[0-9a-f]{8}$/.test(clean)) throw new Error(`--revoke 期望 8 位 hex keyId，收到：${id}`)
    if (!merged.some((r) => String(r.id).toLowerCase() === clean)) {
      merged.push({ id: clean, sig: signRevocation(clean, privateKeyPem), sigAlgo: SIG_ALGO })
    }
  }
  manifest.revocations = merged

  // keyId 只是给人核对密钥是否轮换的提示；客户端绝不能据此去下载公钥。
  if (o.pubkeyHint) {
    const pem = resolvePublicKey(o.pubkeyHint)
    if (pem) manifest.keyId = keyIdOf(pem)
  }

  fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2) + '\n')
  return { manifest, manifestPath, published, skipped }
}

/**
 * 极简 GET：返回 { status, headers, body }，整体收进内存（制品只有几十 KB）。
 * 用回调式 node:http 而不是 fetch，理由见 verifyDistribution 的说明。
 */
export function httpGet(url, o = {}) {
  return new Promise((resolve, reject) => {
    let u
    try {
      u = new URL(url)
    } catch (e) {
      reject(new Error(`URL 无法解析：${url}`))
      return
    }
    const mod = u.protocol === 'https:' ? https : http
    const req = mod.get(u, { timeout: o.timeoutMs ?? 5000, headers: o.headers }, (res) => {
      const chunks = []
      res.on('data', (c) => chunks.push(c))
      res.on('end', () => resolve({ status: res.statusCode || 0, headers: res.headers, body: Buffer.concat(chunks) }))
    })
    req.on('timeout', () => req.destroy(new Error(`请求超时 ${o.timeoutMs ?? 5000}ms：${url}`)))
    req.on('error', reject)
  })
}

/**
 * 发布后的**客户端视角**自检：真的从 Hub 下载一次，比对 sha256 与签名。
 *
 * 为什么发布完还要验一遍：签名/清单是本地写的，写对了不代表**分发**对了——
 * 制品没复制进 pkg/、Hub 的 LANHUB_UPDATE_DIR 指向别处、清单被旧文件覆盖，
 * 都是"本地显示成功、客户端 404 或验签失败"的典型事故，只有真取一次才知道。
 *
 * @param {object} o
 * @param {string} o.manifestUrl 清单 URL（http://host:port/api/updates）
 * @param {string} o.version     期望的 latest 版本
 * @param {string} [o.publicKeyPem] 公钥；不给则跳过验签并在结果里标注
 * @param {typeof httpGet} [o.fetchImpl] 仅测试注入用
 * @param {number} [o.timeoutMs] 单次请求超时（默认 5000）
 * @returns {Promise<{ok:boolean, checks:object[], version:string}>}
 *
 * 请求刻意走 node:http 而不是 globalThis.fetch：Node 的默认 fetch 会读
 * HTTP_PROXY/HTTPS_PROXY 环境变量，而开发机上常有代理——结果就是"本机 Hub 明明活着，
 * 自检却报 fetch failed"。直连才是 LAN 场景想要的语义，也和插件侧一致。
 */
export async function verifyDistribution(o) {
  const doFetch = o.fetchImpl || httpGet
  const timeoutMs = o.timeoutMs ?? 5000
  const checks = []
  const push = (name, ok, detail) => checks.push({ name, ok, detail })
  const fail = (name, detail) => {
    push(name, false, detail)
    return { ok: false, checks, version: '' }
  }

  let res
  try {
    res = await doFetch(o.manifestUrl, { headers: { accept: 'application/json' }, timeoutMs })
  } catch (e) {
    return fail('manifest-fetch', `请求清单失败：${e.message}`)
  }
  if (res.status < 200 || res.status >= 300) return fail('manifest-http', `HTTP ${res.status}`)
  push('manifest-http', true, `HTTP ${res.status}`)

  let manifest
  try {
    manifest = JSON.parse(res.body.toString('utf8'))
  } catch (e) {
    return fail('manifest-json', `清单不是合法 JSON：${e.message}`)
  }
  push('manifest-json', true, `name=${manifest.name || '?'} latest=${manifest.latest || '(空)'}`)

  if (manifest.latest !== o.version) {
    return fail('manifest-latest', `latest=${manifest.latest || '(空)'}，期望 ${o.version}`)
  }
  push('manifest-latest', true, `latest=${manifest.latest}`)

  const art = (manifest.artifacts || {})[o.version]
  if (!art) return fail('artifact-present', `清单里没有 ${o.version}`)
  push('artifact-present', true, `sha256=${String(art.sha256).slice(0, 16)}…`)

  if (!art.url) return fail('artifact-url', '清单里该版本没有 url')
  let artUrl
  try {
    artUrl = new URL(art.url, o.manifestUrl).toString()
  } catch (e) {
    return fail('artifact-url', `url 无法解析：${art.url}（${e.message}）`)
  }

  let ares
  try {
    ares = await doFetch(artUrl, { timeoutMs })
  } catch (e) {
    return fail('artifact-fetch', `下载制品失败：${e.message}`)
  }
  if (ares.status < 200 || ares.status >= 300) {
    // 制品 404 是最常见的事故：清单写了，pkg/ 里没复制过去。
    return fail('artifact-http', `HTTP ${ares.status} — 制品不在 pkg/ 下？`)
  }
  const body = ares.body
  push('artifact-http', true, `${body.length} bytes`)

  if (art.size && body.length !== art.size) {
    return fail('artifact-size', `size ${body.length} ≠ 清单 ${art.size}（截断？）`)
  }
  push('artifact-size', true, `size=${body.length}`)

  const sha = createHash('sha256').update(body).digest('hex')
  if (sha !== String(art.sha256 || '').toLowerCase()) {
    return fail('artifact-sha256', `下载件 ${sha.slice(0, 16)}… ≠ 清单 ${String(art.sha256).slice(0, 16)}…`)
  }
  push('artifact-sha256', true, `sha256=${sha.slice(0, 16)}…`)

  // 验签是这条链上最后一道闸：sha256 只证明"和清单一致"，不证明"是发布者做的"。
  // 没有公钥时不能假装通过——明确标注 skipped，让调用方自己决定算不算成功。
  if (!o.publicKeyPem) {
    push('artifact-sig', null, 'skipped：未配置公钥（--pubkey / LAN_HUB_UPDATE_PUBKEY）')
  } else {
    const ok = verifyArtifact(
      { name: manifest.name || DEFAULT_NAME, version: o.version, sha256: sha, proto: art.proto ?? 0 },
      art.sig,
      o.publicKeyPem,
    )
    push('artifact-sig', ok, ok ? 'ed25519 验签通过' : (art.sig ? '验签失败：签名与制品不匹配' : '清单里没有签名'))
    if (!ok) return { ok: false, checks, version: o.version }
  }

  return { ok: checks.every((c) => c.ok !== false), checks, version: o.version }
}
