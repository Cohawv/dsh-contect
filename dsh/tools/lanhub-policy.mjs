#!/usr/bin/env node
/**
 * lanhub-policy —— lan-hub 插件策略的热更新 CLI。
 *
 * 机制：脚本原子写一个 JSON 文件，插件按 policyPollMs 轮询它的 mtime，
 * 变化即原地换策略（不断连、不重启、不依赖宿主的配置重载）。
 *
 * 结构：每个子命令只负责「算数据 + 出文案」，打印统一走 emit()，
 * 所以 --json 与人类可读两种形态不会各写一遍（那是上一版膨胀的主因）。
 *
 * 用法：
 *   lanhub-policy get                       查看策略文件与生效值
 *   lanhub-policy set k=v [k=v ...]         写入若干字段
 *   lanhub-policy unset k [k ...]           删除覆盖项（回落到配置）
 *   lanhub-policy reset                     清空策略文件
 *   lanhub-policy keys                      列出可用字段与取值规则
 *   lanhub-policy validate                  只校验现有文件
 *
 * 通用参数：--file <path>  --json  --dry-run  --quiet
 * 退出码：0 成功；1 参数/校验/IO 错误。
 */

import fs from 'node:fs'
import path from 'node:path'
import {
  POLICY_KEYS,
  POLICY_SPEC,
  policyDefaults,
  coercePolicyValue,
  pickPolicy,
  describeSpec,
} from '../src/policy.mjs'

const W = Math.max(...POLICY_KEYS.map((k) => k.length))

// ---------- 参数解析 ----------
const opt = { json: false, dryRun: false, quiet: false }
const argv = process.argv.slice(2)
let file = process.env.LAN_HUB_POLICY_FILE || path.resolve('.lan-hub.policy.json')
let cmd = 'get'
const args = []

for (let i = 0; i < argv.length; i += 1) {
  const a = argv[i]
  if (a === '--file') file = path.resolve(argv[++i] ?? '')
  else if (a.startsWith('--file=')) file = path.resolve(a.slice(7))
  else if (a === '--json') opt.json = true
  else if (a === '--dry-run') opt.dryRun = true
  else if (a === '--quiet') opt.quiet = true
  else if (a === '-h' || a === '--help') cmd = 'help'
  else if (cmd === 'get' && !args.length && !a.includes('=')) cmd = a
  else args.push(a)
}

// ---------- 出口 ----------
function emit(data, lines) {
  process.stdout.write(
    opt.json ? JSON.stringify(data, null, 2) + '\n' : lines.join('\n') + '\n',
  )
}

function fail(msg) {
  if (opt.json) process.stdout.write(JSON.stringify({ ok: false, error: msg, file }) + '\n')
  else process.stderr.write(`ERROR: ${msg}\n`)
  process.exit(1)
}

// ---------- 文件读写 ----------
/**
 * @param {{repair?: boolean}} [o]
 *   repair=true 时，损坏的 JSON 视为空对象并重建——set/unset 的职责就是产出合法文件，
 *   不能让一个半截文件把「修文件的工具」自己锁死（插件那边会保留上一份策略，不会受影响）。
 */
function read({ repair = false } = {}) {
  if (!fs.existsSync(file)) return {}
  const text = fs.readFileSync(file, 'utf8').trim()
  if (!text) return {}
  let obj
  try {
    obj = JSON.parse(text)
  } catch (e) {
    if (!repair) fail(`策略文件不是合法 JSON：${e.message}`)
    if (!opt.quiet) process.stderr.write(`WARN: 策略文件损坏，重建为合法内容（${e.message}）\n`)
    return {}
  }
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) {
    if (!repair) fail(`策略文件必须是 JSON 对象：${file}`)
    if (!opt.quiet) process.stderr.write('WARN: 策略文件不是 JSON 对象，重建\n')
    return {}
  }
  return obj
}

/** 原子写：先写同目录临时文件再 rename，插件绝不会读到半截内容 */
function write(obj) {
  const dir = path.dirname(file)
  fs.mkdirSync(dir, { recursive: true })
  const tmp = path.join(dir, `.${path.basename(file)}.${process.pid}.tmp`)
  fs.writeFileSync(tmp, JSON.stringify(obj, null, 2) + '\n', 'utf8')
  fs.renameSync(tmp, file)
}

const prefix = () => (opt.dryRun ? '[dry-run] ' : '')

// ---------- 子命令（只算数据 + 出文案） ----------
const COMMANDS = {
  get() {
    const { values } = pickPolicy(read())
    const merged = { ...policyDefaults(), ...values }
    const sources = {}
    const lines = [`file: ${file}`]
    for (const k of POLICY_KEYS) {
      sources[k] = k in values ? 'file' : 'default'
      const mark = k in values ? '*' : ' '
      lines.push(`  ${mark} ${k.padEnd(W)} = ${String(merged[k]).padEnd(8)} [${sources[k]}]`)
    }
    lines.push('  （* 表示被策略文件覆盖）')
    return { data: { ok: true, file, values: merged, sources }, lines }
  },

  set(a) {
    if (!a.length) fail('set 需要至少一个 k=v')
    const next = { ...read({ repair: true }) }
    const applied = []
    const errors = []
    for (const pair of a) {
      const i = pair.indexOf('=')
      if (i <= 0) {
        errors.push(`参数格式应为 key=value，收到 ${pair}`)
        continue
      }
      const k = pair.slice(0, i).trim()
      const r = coercePolicyValue(k, pair.slice(i + 1).trim())
      if (!r.ok) {
        errors.push(r.error)
        continue
      }
      next[k] = r.value
      applied.push(`${k}=${r.value}`)
    }
    if (errors.length) fail(errors.join('; '))
    if (!opt.dryRun) write(next)
    const lines = [`${prefix()}applied -> ${file}`, ...applied.map((x) => `  ${x}`)]
    if (!opt.quiet && !opt.json) {
      lines.push(
        `\n提示：插件需配置 policyFile 指向该文件才会生效，例如\n` +
          `  config:\n    policyFile: '${file}'\n    policyPollMs: 1500\n` +
          `生效后用 hub_stats 可看到每个字段的来源（file / runtime / config）。`,
      )
    }
    return { data: { ok: true, dryRun: opt.dryRun, file, applied, values: next }, lines }
  },

  unset(a) {
    if (!a.length) fail('unset 需要至少一个字段名')
    const next = { ...read({ repair: true }) }
    const removed = []
    for (const k of a) {
      if (!POLICY_SPEC[k]) fail(`unknown policy key: ${k}（可用：${POLICY_KEYS.join(', ')}）`)
      if (k in next) {
        delete next[k]
        removed.push(k)
      }
    }
    if (!opt.dryRun) write(next)
    return {
      data: { ok: true, dryRun: opt.dryRun, file, removed },
      lines: [`${prefix()}unset ${removed.join(', ') || '(none)'}`],
    }
  },

  reset() {
    if (!opt.dryRun) write({})
    return { data: { ok: true, dryRun: opt.dryRun, file }, lines: [`${prefix()}reset -> ${file}`] }
  },

  validate() {
    const { values, errors } = pickPolicy(read())
    const ok = errors.length === 0
    if (!ok) process.exitCode = 1
    const lines = ok
      ? [`OK: ${file}（${Object.keys(values).length} 个覆盖项）`]
      : [`INVALID: ${file}`, ...errors.map((e) => `  - ${e}`)]
    return { data: { ok, file, values, errors }, lines }
  },

  keys() {
    const defaults = policyDefaults()
    const rules = Object.fromEntries(POLICY_KEYS.map((k) => [k, describeSpec(k)]))
    const lines = POLICY_KEYS.map(
      (k) => `  ${k.padEnd(W)}  ${rules[k].padEnd(20)} default=${defaults[k]}`,
    )
    return { data: { ok: true, keys: POLICY_KEYS, defaults, rules }, lines }
  },

  help() {
    const lines = [
      'lanhub-policy —— 热更新 lan-hub 插件策略',
      '',
      '用法：',
      '  lanhub-policy get                     查看策略文件与生效值',
      '  lanhub-policy set <k=v> [k=v ...]     写入若干字段',
      '  lanhub-policy unset <k> [k ...]       删除覆盖项（回落到配置）',
      '  lanhub-policy reset                   清空策略文件',
      '  lanhub-policy keys                    列出可用字段与取值规则',
      '  lanhub-policy validate                只校验现有文件',
      '',
      '参数：--file <path>  --json  --dry-run  --quiet',
      `文件：${file}`,
    ]
    return { data: { ok: true, file, commands: Object.keys(COMMANDS) }, lines }
  },
}

// ---------- 分发 ----------
const run = COMMANDS[cmd]
if (!run) fail(`unknown command: ${cmd}（可用：${Object.keys(COMMANDS).join(' ')}）`)
const { data, lines } = run(args)
emit(data, lines)
