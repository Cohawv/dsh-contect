/**
 * 信任根链（keyring）：公钥怎么"跟着包更新"。
 *
 * 三层来源，**锚（anchor）**的确定方式：
 *   1. config  —— 配置/环境变量显式指定的 updatePublicKey（操作员明确意图，最高优先）
 *   2. embedded—— 编进构建产物的默认公钥（trusted-key.mjs）
 *   3. 二者皆无 → 无锚，无法验任何东西（拒绝自动更新）
 *
 * 为什么 config 优先于 embedded：操作员改配置就是"我要用这把根"，应无条件压过包内置。
 *
 * 锚之上是**轮换链**：发布侧用"当前私钥"给下一把公钥背书（nextKey），客户端凭锚
 * 验过背书后把新公钥接力进链。链存成**签名轮换记录**（不是裸公钥数组），加载时从锚
 * 出发逐环验签重建——任何一环对不上（被篡改/种植），整条 keyring 被丢弃，回落到锚。
 *
 * 这一点直接堵掉旧实现的漏洞：旧版只要缓存里 trusted-key.json 非空就整个返回它，
 * 配置/内置根被无视，攻击者写个 `{"chain":["攻击者公钥"]}` 就能接管。现在不行了——
 * 种植的文件因为没有锚的背书，第一环就验不过，被丢弃。
 *
 * 为什么保存的是**链**而不是单把公钥：回滚包是旧版本，由旧私钥签的。轮换后若只用
 * 新根验签，回滚包会被判 untrusted——而回滚恰恰是最需要它的时刻。历史上被信任过的
 * 根都留着，验签时逐个尝试，任意一个通过即算验过。
 */
import fs from 'node:fs'
import path from 'node:path'
import { resolvePublicKey, verifyArtifact, verifyKeyRotation, keyIdOf, verifyRevocation } from './signing.mjs'
import { EMBEDDED_PUBLIC_KEY } from './trusted-key.mjs'

const KEYRING_FILE = 'trusted-key.json'
const REVOCATION_FILE = 'revoked-keys.json'

function readJSON(p) {
  try {
    return JSON.parse(fs.readFileSync(p, 'utf8'))
  } catch {
    return null
  }
}

function writeJSONAtomic(p, data) {
  const tmp = `${p}.tmp`
  fs.mkdirSync(path.dirname(p), { recursive: true })
  fs.writeFileSync(tmp, JSON.stringify(data, null, 2) + '\n')
  fs.renameSync(tmp, p)
}

/** 把锚（config 优先于 embedded）解析出来。无锚返回 anchor=''。 */
function resolveAnchor(config) {
  const fromConfig = resolvePublicKey(config || '')
  if (fromConfig) return { anchor: fromConfig, origin: 'config' }
  const embedded = resolvePublicKey(EMBEDDED_PUBLIC_KEY)
  if (embedded) return { anchor: embedded, origin: 'embedded' }
  return { anchor: '', origin: 'none' }
}

/**
 * 取当前信任根链——**始终从锚重建**，绝不盲信缓存文件。
 *
 * @param {object} o
 * @param {string} [o.cacheDir] 缓存目录（轮换记录存这里）
 * @param {string} [o.config]   配置里的 updatePublicKey（PEM 或路径）
 * @returns {{
 *   chain:string[], origin:'config'|'embedded'|'none',
 *   valid:boolean, reason:string, rotations:object[],
 *   anchorKeyId:string
 * }}
 *
 * valid=false 表示缓存的 keyring 被篡改/损坏，已回落到锚（链里只有锚）。
 * rotations 是**通过验证**的那段记录，供 acceptKeyRotation 在此基础上追加。
 */
export function loadTrustChain(o = {}) {
  const { anchor, origin } = resolveAnchor(o.config)
  if (!anchor) {
    return { chain: [], origin: 'none', valid: false, reason: 'no anchor', rotations: [], anchorKeyId: '', revoked: new Set() }
  }

  const chain = [anchor]
  const verifiedRotations = []
  let valid = true
  let reason = ''

  const kr = o.cacheDir ? readJSON(path.join(o.cacheDir, KEYRING_FILE)) : null
  if (kr && Array.isArray(kr.rotations)) {
    for (const rot of kr.rotations) {
      const newPem = resolvePublicKey(rot.publicKey)
      const head = chain[chain.length - 1]
      // 每一环都必须由"紧邻的上一把根"背书——这是轮换链的硬约束。
      if (!newPem || !verifyKeyRotation(newPem, rot.sig, head)) {
        valid = false
        reason = `轮换记录验签失败（第 ${chain.length} 环），已丢弃整条 keyring 回落到锚`
        break
      }
      chain.push(newPem)
      verifiedRotations.push(rot)
    }
  }

  return { chain, origin, valid, reason, rotations: verifiedRotations, anchorKeyId: keyIdOf(anchor), revoked: loadRevocations(o.cacheDir) }
}

/**
 * 接受一次密钥轮换：用**当前链头**（最新根）验背书，通过则把新公钥压入链头并落盘。
 *
 * @param {object} o
 * @param {object} o.nextKey {publicKey,sig,sigAlgo?,keyId?} 来自清单
 * @param {string} [o.cacheDir]
 * @param {string} [o.config] 配置公钥（用于确定锚）
 * @param {boolean} [o.persist=true] false=只判断不写盘（check 等只读命令）
 * @returns {{ok:boolean, reason?:string, pinned?:object, chain?:string[], unchanged?:boolean}}
 */
export function acceptKeyRotation(o) {
  const { nextKey, cacheDir } = o
  if (!nextKey || !nextKey.publicKey) return { ok: false, reason: '清单里没有 nextKey.publicKey' }
  if (!nextKey.sig) return { ok: false, reason: 'nextKey 没有签名背书——不接受无背书的公钥' }
  if (nextKey.sigAlgo && nextKey.sigAlgo !== 'ed25519') {
    return { ok: false, reason: `不支持的轮换签名算法 ${nextKey.sigAlgo}` }
  }
  if (o.persist !== false && !cacheDir) return { ok: false, reason: '没有缓存目录，无法持久化新信任根' }

  // 从锚重建当前链，取链头作为"谁有权指定接班人"。
  const cur = loadTrustChain({ cacheDir, config: o.config })
  if (!cur.chain.length) return { ok: false, reason: '当前没有可信的根，无法判断轮换是否合法' }
  const head = cur.chain[cur.chain.length - 1]

  const newPem = resolvePublicKey(nextKey.publicKey)
  if (!newPem) return { ok: false, reason: 'nextKey.publicKey 不是合法 PEM' }
  if (!verifyKeyRotation(newPem, nextKey.sig, head)) {
    return { ok: false, reason: '轮换背书验签失败：不是当前信任根签发的新公钥（疑似投毒）' }
  }
  // 已在同一条链上就不重复写，避免每次同步都落一次盘。
  if (cur.chain.some((p) => p.trim() === newPem.trim())) {
    return { ok: true, unchanged: true, chain: cur.chain, pinned: { rotations: cur.rotations } }
  }

  const rot = {
    publicKey: nextKey.publicKey,
    sig: nextKey.sig,
    sigAlgo: nextKey.sigAlgo || 'ed25519',
    keyId: nextKey.keyId || keyIdOf(newPem),
    rotatedAt: new Date().toISOString(),
  }
  const pinned = {
    rotations: [...cur.rotations, rot],
    lastAcceptedAt: new Date().toISOString(),
    from: fingerprint(head),
    to: fingerprint(newPem),
  }
  if (o.persist !== false) writeKeyring(cacheDir, pinned)
  return { ok: true, pinned, chain: [...cur.chain, newPem] }
}

function writeKeyring(cacheDir, pinned) {
  const p = path.join(cacheDir, KEYRING_FILE)
  writeJSONAtomic(p, pinned)
  // 信任根链是安全敏感文件：锁 600，避免同机其他用户读写后种植。
  try {
    fs.chmodSync(p, 0o600)
  } catch {
    /* 某些平台 chmod 受限，忽略不致命 */
  }
}

/** 用整条链验签：返回命中详情（第几把根、keyId），供撤销判断用。 */
export function verifyWithChainDetailed(payload, sig, roots) {
  if (!sig || !roots || !roots.length) return { ok: false }
  for (let i = 0; i < roots.length; i += 1) {
    if (verifyArtifact(payload, sig, roots[i])) {
      return { ok: true, index: i, rootPem: roots[i], keyId: keyIdOf(roots[i]) }
    }
  }
  return { ok: false }
}

/** 旧签名兼容：只判过不过。 */
export function verifyWithChain(art, sig, roots) {
  return verifyWithChainDetailed(art, sig, roots).ok
}

// ---------- 撤销 ----------
/**
 * 读取本地持久化的撤销集合（只增不减，跨 Hub 存活）。
 * 即便恶意 Hub 之后把清单里的 revocations 删掉，客户端已记住的吊销依然有效。
 */
export function loadRevocations(cacheDir) {
  if (!cacheDir) return new Set()
  const o = readJSON(path.join(cacheDir, REVOCATION_FILE))
  return new Set(Array.isArray(o?.revoked) ? o.revoked : [])
}

function saveRevocations(cacheDir, set) {
  if (!cacheDir) return
  const p = path.join(cacheDir, REVOCATION_FILE)
  writeJSONAtomic(p, { revoked: [...set] })
  try {
    fs.chmodSync(p, 0o600)
  } catch {
    /* 忽略 */
  }
}

/**
 * 把清单里的撤销声明并入本地集合（只增不减）。
 * 每条声明必须由信任链上的某把私钥签名（revoke\n<keyId>），否则恶意 Hub 可凭空吊销
 * 诚实钥匙做 DoS。验证通过的 keyId 才并入并（可选）持久化。
 *
 * @param {object[]} revocations 清单里的 [{id,sig,sigAlgo?}]
 * @param {string[]} chain 当前信任链（用于验撤销签名）
 * @param {string} [cacheDir]
 * @param {boolean} [persist=true]
 * @returns {Set<string>} 生效的吊销集合
 */
export function applyManifestRevocations(revocations, chain, cacheDir, persist = true) {
  const set = loadRevocations(cacheDir)
  if (Array.isArray(revocations)) {
    for (const r of revocations) {
      if (r && r.id && verifyRevocation(r.id, r.sig, chain)) set.add(String(r.id).toLowerCase())
    }
  }
  if (persist) saveRevocations(cacheDir, set)
  return set
}

/** 信任根来源的人话版本。 */
export function originText(origin) {
  return (
    {
      config: '配置/环境变量（--pubkey / $LAN_HUB_UPDATE_PUBKEY）',
      embedded: '包内置公钥（trusted-key.mjs）',
      none: '无',
    }[origin] || origin
  )
}

/** 公钥指纹（前 8 位 hex），只用于日志与人核对，不作任何安全判断。 */
export function fingerprint(pem) {
  return keyIdOf(pem)
}
