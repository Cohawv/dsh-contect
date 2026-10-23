#!/usr/bin/env node
/**
 * lanhub-keygen —— 生成更新通道的 Ed25519 密钥对。
 *
 * 私钥留在构建机（签发制品用），公钥 pin 到每台客户端（验签用）。
 * 公钥**不能**从清单下载——那会让签名自我证明。
 *
 * 用法：
 *   lanhub-keygen                       # 生成到 ~/.dsh/lanhub-ed25519.{pub,key}.pem
 *   lanhub-keygen --out <dir|prefix>    # 指定输出位置
 *   lanhub-keygen --force               # 允许覆盖已存在的密钥
 *   lanhub-keygen --embed               # 把公钥写进 src/trusted-key.mjs（编进产物）
 *
 * --embed 是"公钥随包分发"的那一条路：写进源码后重新 build，客户端不配
 * updatePublicKey 也能验签。之后要换钥，用发布侧的 --rotate-to 走轮换链，
 * 老客户端不必重新配置。
 *
 * 生成后把公钥内容（或路径）填进插件配置的 updatePublicKey，或设环境变量
 * LAN_HUB_UPDATE_PUBKEY。配置里建议写路径：文件可以随镜像分发，改起来也方便。
 *
 * 私钥（.key.pem）**永远不进仓库、不进包**：拿到包的人等于拿到签发权。
 */
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { generateKeyPair } from '../src/signing.mjs'

const opt = { out: '', force: false, json: false, embed: false, help: false }
{
  const argv = process.argv.slice(2)
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i]
    if (a === '--out') opt.out = argv[++i] ?? ''
    else if (a.startsWith('--out=')) opt.out = a.slice(6)
    else if (a === '--embed') opt.embed = true
    else if (a === '--force' || a === '-f') opt.force = true
    else if (a === '--json') opt.json = true
    else if (a === '-h' || a === '--help') opt.help = true
    else if (opt.out === '' && !a.startsWith('-')) opt.out = a
  }
}

if (opt.help) {
  process.stdout.write(
    [
      'lanhub-keygen —— 生成 Ed25519 密钥对',
      '',
      '用法：lanhub-keygen [--out <dir|prefix>] [--force] [--embed] [--json]',
      '',
      '默认输出 ~/.dsh/lanhub-ed25519.pub.pem 与 ~/.dsh/lanhub-ed25519.key.pem',
      '',
      '  --embed   把公钥写进 src/trusted-key.mjs，重新 build 即编进产物',
      '            （客户端不配 updatePublicKey 也能验签）',
      '',
      '私钥（.key.pem）只留在构建机；公钥（.pub.pem）pin 到客户端配置 updatePublicKey。',
      '',
    ].join('\n'),
  )
  process.exit(0)
}

// --out 的判定规则（刻意定得无歧义，因为踩过坑）：
//   以 .pem 结尾          -> 当作前缀，生成 <前缀>.pub.pem / <前缀>.key.pem
//   其余情况（含不存在）   -> 当作目录，自动创建，生成 <目录>/lanhub-ed25519.{pub,key}.pem
// 之所以不以"是否存在"来猜：--out 指向一个还没建的目录是常见用法，猜成就前缀会
// 在意外位置生成文件，排查起来很烦。
let prefix = ''
if (!opt.out) {
  prefix = path.join(os.homedir(), '.dsh', 'lanhub-ed25519')
} else if (/\.pem$/i.test(opt.out)) {
  prefix = opt.out.slice(0, -4)
} else {
  prefix = path.join(opt.out, 'lanhub-ed25519')
}
const pubPath = `${prefix}.pub.pem`
const keyPath = `${prefix}.key.pem`

if (!opt.force) {
  for (const p of [pubPath, keyPath]) {
    if (fs.existsSync(p)) {
      process.stderr.write(`ERROR: ${p} 已存在（加 --force 覆盖，覆盖后所有客户端都要换公钥）\n`)
      process.exit(1)
    }
  }
}

const { publicKey, privateKey } = generateKeyPair()
fs.mkdirSync(path.dirname(pubPath), { recursive: true })
fs.writeFileSync(pubPath, publicKey)
// 私钥 0600：POSIX 上生效，Windows 上是 no-op，可接受
fs.writeFileSync(keyPath, privateKey, { mode: 0o600 })
try {
  fs.chmodSync(keyPath, 0o600)
} catch {
  /* Windows 无 chmod，忽略 */
}

// --embed：把公钥写进源码，使它随 dist 一起分发（"编进程序里"的那条路）。
// 只有公钥——私钥进包等于把签发权发给每台客户端，签名会彻底失去意义。
let embeddedPath = ''
if (opt.embed) {
  const self = path.dirname(fileURLToPath(import.meta.url))
  // tools/（源码态）或 dist/（打包态）的上一级都是包根
  embeddedPath = path.join(path.resolve(self, '..'), 'src', 'trusted-key.mjs')
  const body = [
    '/**',
    ' * 内置信任根：随包分发的默认公钥。',
    ' *',
    " * 存在的理由：客户端不该每台都先 keygen 再配一遍才能用自动更新。把公钥编进",
    " * 构建产物，装好即可验签——配置里的 `updatePublicKey` 只在需要**覆盖**它时才填。",
    ' *',
    ' * 它**不是**轮换链的替代品，而是链的起点（第 0 环）。链的第 1 环及以后由',
    ' * keyring.mjs 持久化到缓存目录，随版本走。',
    ' *',
    ' * 本文件由 `lanhub-keygen --embed` 生成，不要手改。留空表示"没有内置信任根"，',
    ' * 此时行为与 0.7.0 之前一致：未配置公钥即拒绝自动更新。',
    ' *',
    ' * 安全边界：这里只有**公钥**。私钥永远不进仓库、不进包、不进任何产物。',
    ' */',
    'export const EMBEDDED_PUBLIC_KEY = `' + publicKey.trim() + '\\n`',
    '',
  ].join('\n')
  fs.writeFileSync(embeddedPath, body)
}

if (opt.json) {
  process.stdout.write(
    JSON.stringify(
      { publicKeyPath: pubPath, privateKeyPath: keyPath, publicKey, embedded: embeddedPath || null },
      null,
      2,
    ) + '\n',
  )
} else {
  process.stdout.write(
    [
      `公钥：${pubPath}`,
      `私钥：${keyPath}  （0600，只留构建机，不要分发）`,
      ...(embeddedPath ? ['', `已写入内置信任根：${embeddedPath}`, '（重新 build 后即编进 dist；客户端不配 updatePublicKey 也能验签）'] : []),
      '',
      '把下面这行填进插件配置（可选，用于覆盖内置公钥）：',
      `  updatePublicKey: '${pubPath}'`,
      '',
      '或设环境变量：',
      `  export LAN_HUB_UPDATE_PUBKEY='${pubPath}'`,
      '',
      '换钥时不要直接改客户端：发布侧用 --rotate-to 走轮换链，老客户端会自己接力。',
      '',
    ].join('\n'),
  )
}
