/**
 * 更新制品的 Ed25519 签名与验签。
 *
 * 为什么 sha256 不够、必须签名：
 *   清单和制品都放在同一个 Hub 上，而 LAN 无认证。能改其中一个就能改另一个，
 *   所以 sha256 只防传输截断，不防投毒——它验证的是"我拿到的和清单说的一致"，
 *   而不是"这个包是发布者做的"。若开启自动安装却只有 sha256，等于任何能连上
 *   Hub 的设备都能让所有 Agent 加载任意代码（RCE）。
 *
 * 签什么：
 *   逐个 artifact 签（而不是签整个 manifest）。载荷是 name/version/sha256/proto
 *   的规范化拼接。这样增量发布新版本时不必重签历史版本，客户端也只需验自己要装的
 *   那一个，逻辑最清晰。
 *
 * 降级攻击怎么防：
 *   签名不能防"Hub 把 latest 指向一个旧的、但确实被签过的版本"。这一条由
 *   **只升不降**兜底：客户端要求 target > current。二者叠加才闭合。
 *
 * 公钥从哪来：
 *   必须 pin 在客户端本地（配置 / 环境变量 / 文件），**绝不能由清单下发**——
 *   否则投毒者连自己的公钥一起发，签名就自我证明了。
 */

import fs from 'node:fs'
import {
  createPublicKey,
  createPrivateKey,
  generateKeyPairSync,
  sign as cryptoSign,
  verify as cryptoVerify,
  createHash,
} from 'node:crypto'

/** 签名算法标识，写进清单便于将来轮换。 */
export const SIG_ALGO = 'ed25519'
/** 载荷格式版本。将来改字段拼接方式时递增，旧签名仍可按旧版本验。 */
export const SIG_VERSION = 1

/**
 * 规范化载荷。
 *
 * 用 \n 分隔且字段顺序固定，是因为拼接式载荷最容易出的错是「字段滑动」：
 * 若允许任意字符且不固定顺序，"a|bc" 和 "ab|c" 会算出同一个签名。这里 name 与
 * version 虽可含任意字符，但 sha256/proto 是定长/数字并被放在末尾，配合固定
 * 字段数，滑动无法构造出另一组有意义的输入。
 */
export function signingPayload(art) {
  return [
    'lanhub-update',
    SIG_VERSION,
    String(art.name ?? ''),
    String(art.version ?? ''),
    String(art.sha256 ?? '').toLowerCase(),
    String(art.proto ?? 0),
  ].join('\n')
}

/** 用私钥签名，返回 base64。 */
export function signArtifact(art, privateKeyPem) {
  const buf = Buffer.from(signingPayload(art), 'utf8')
  return cryptoSign(null, buf, createPrivateKey(String(privateKeyPem))).toString('base64')
}

/** 验签。签名缺失/为空返回 false——调用方再决定是否放行。 */
export function verifyArtifact(art, sigBase64, publicKeyPem) {
  if (!sigBase64) return false
  try {
    const buf = Buffer.from(signingPayload(art), 'utf8')
    return cryptoVerify(null, buf, createPublicKey(String(publicKeyPem)), Buffer.from(String(sigBase64), 'base64'))
  } catch {
    // 公钥不是 PEM、签名不是 base64、长度不对，都归为验签失败而不是抛栈
    return false
  }
}

/**
 * 密钥轮换：用**当前私钥**给"下一把公钥"签名。
 *
 * 为什么需要它：公钥若只能手工改配置，密钥一旦想换（或怀疑泄露），所有客户端
 * 都得挨个改配置，改不到的那些就永远停在旧版本。而"让新包自带验它的公钥"是
 * 不行的——投毒者连自己的公钥一起塞进去，签名就自我证明了。
 *
 * 折中且安全的做法是把轮换做成**链**：新公钥由**旧私钥**背书。客户端手里 pin 的
 * 是旧公钥，它不需要信任清单里的新公钥，只需要验证"这把新公钥确实是旧私钥签发的"。
 * 信任根始终在客户端本地，只是被安全地接力了一次。
 *
 * 载荷只含公钥本身（PEM 规范化后），不含 keyId 之类的可变字段——字段越多，
 * 越容易在实现不一致时验出不同的结果。
 */
export function keyRotationPayload(publicKeyPem) {
  return ['lanhub-key-rotation', SIG_VERSION, String(publicKeyPem ?? '').trim()].join('\n')
}

/** 用当前私钥签一封"下一把公钥"的背书信，返回 base64。 */
export function signKeyRotation(newPublicKeyPem, privateKeyPem) {
  const buf = Buffer.from(keyRotationPayload(newPublicKeyPem), 'utf8')
  return cryptoSign(null, buf, createPrivateKey(String(privateKeyPem))).toString('base64')
}

/** 用当前信任的公钥验证轮换背书。 */
export function verifyKeyRotation(newPublicKeyPem, sigBase64, currentPublicKeyPem) {
  if (!sigBase64 || !currentPublicKeyPem) return false
  try {
    const buf = Buffer.from(keyRotationPayload(newPublicKeyPem), 'utf8')
    return cryptoVerify(
      null,
      buf,
      createPublicKey(String(currentPublicKeyPem)),
      Buffer.from(String(sigBase64), 'base64'),
    )
  } catch {
    return false
  }
}

/**
 * 公钥指纹（keyId）：与发布侧 publish.mjs 完全一致——sha256(规范化 PEM).hex 前 8 位。
 * 撤销、轮换都靠它指代"哪一把钥匙"，所以两侧必须算出同一个值，否则撤销对不上号。
 */
export function keyIdOf(publicKeyPem) {
  return createHash('sha256').update(String(publicKeyPem || '').trim()).digest('hex').slice(0, 8)
}

/**
 * 用**链上任意一把**根验签。任一通过即 true。
 * 撤销声明和制品签名都复用它——"信任链"的本质就是"链上任一先辈可背书"。
 */
export function verifyAgainstRoots(payloadStr, sigBase64, roots) {
  if (!sigBase64 || !roots || !roots.length) return false
  const buf = Buffer.from(payloadStr, 'utf8')
  for (const root of roots) {
    try {
      if (cryptoVerify(null, buf, createPublicKey(String(root)), Buffer.from(String(sigBase64), 'base64'))) return true
    } catch {
      /* 这把根验不过就试下一把，不抛栈 */
    }
  }
  return false
}

/**
 * 密钥撤销声明：由信任链上的某把私钥签发 `revoke\n<keyId>`。
 *
 * 为什么撤销也要签名：否则恶意 Hub 可以直接在清单里写 `revocations:[诚实钥匙的id]`
 * 把整个更新通道 DoS 掉。只有拿得出合法私钥签名的撤销才算数——而泄露的私钥本身
 * 才是被撤销的对象，发布者用**还没泄露的那把**私钥来吊销它。
 */
export function revokePayload(keyId) {
  return ['lanhub-revoke', SIG_VERSION, String(keyId || '')].join('\n')
}

/** 用私钥签一封"吊销某 keyId"的声明，返回 base64。 */
export function signRevocation(keyId, privateKeyPem) {
  const buf = Buffer.from(revokePayload(keyId), 'utf8')
  return cryptoSign(null, buf, createPrivateKey(String(privateKeyPem))).toString('base64')
}

/** 用信任链验一枚撤销声明的合法性（不判断是否"应不应该吊销"，那是调用方的事）。 */
export function verifyRevocation(keyId, sigBase64, roots) {
  return verifyAgainstRoots(revokePayload(keyId), sigBase64, roots)
}

/**
 * 解析公钥来源：可以是 PEM 字符串，也可以是 .pem 文件路径。
 * 取不到返回 ''（调用方按"未配置公钥"处理，通常意味着拒绝自动安装）。
 */
export function resolvePublicKey(src) {
  const s = String(src ?? '').trim()
  if (!s) return ''
  // 内联 PEM（配置里用 YAML 块标量写的那种）常带尾随换行，trim 后再交给 crypto。
  if (s.includes('-----BEGIN')) return s.trim()
  try {
    if (fs.existsSync(s)) return fs.readFileSync(s, 'utf8')
  } catch {
    /* 不是文件路径就当无效处理 */
  }
  try {
    return fs.readFileSync(s, 'utf8')
  } catch {
    return ''
  }
}

/** 生成一对 Ed25519 密钥（PEM）。私钥留构建机，公钥 pin 到客户端。 */
export function generateKeyPair() {
  return generateKeyPairSync('ed25519', {
    publicKeyEncoding: { type: 'spki', format: 'pem' },
    privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
  })
}
