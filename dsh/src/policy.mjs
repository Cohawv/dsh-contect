/**
 * 策略规范（唯一真相源）——插件（src/index.ts）与 CLI（tools/lanhub-policy.mjs）共用。
 *
 * 设计要点：**一张表说清一切**。字段名、类型、默认值、取值范围都写在 SPEC 里，
 * 校验、默认值推导、`keys` 帮助输出全部由它派生。新增一个字段只需加一行，
 * 不需要同时改「键列表 / 类型列表 / 整数表 / 枚举表」四处（那正是过去容易漏的地方）。
 *
 * 只放 policy（热）字段：connection 字段变更必须重连，不参与文件热更新。
 */

/** @typedef {{type:'bool'|'int'|'enum', default:any, min?:number, max?:number, values?:string[]}} FieldSpec */

/** @type {Record<string, FieldSpec>} */
export const POLICY_SPEC = {
  autoRespond: { type: 'bool', default: true },
  autoInject: { type: 'bool', default: false },
  wakeRoom: { type: 'enum', default: 'mention', values: ['mention', 'all', 'off'] },
  batchMs: { type: 'int', default: 800, min: 0, max: 10_000 },
  maxWaitMs: { type: 'int', default: 3000, min: 0, max: 60_000 },
  maxAutoTurns: { type: 'int', default: 6, min: 0, max: 1000 },
  maxHop: { type: 'int', default: 3, min: 0, max: 1000 },
  perPeerPerMin: { type: 'int', default: 8, min: 0, max: 10_000 },
  globalPerMin: { type: 'int', default: 30, min: 0, max: 100_000 },
  muteNotice: { type: 'bool', default: true },
}

/** 允许通过策略文件 / CLI 热更新的字段 */
export const POLICY_KEYS = Object.keys(POLICY_SPEC)

/** 白名单 Set：热路径（每条入站消息）用它判合法性，避免 includes 的线性扫描 */
export const POLICY_KEY_SET = new Set(POLICY_KEYS)

/** 由 SPEC 推导默认策略（与 Schema 默认值必须一致，测试会校验） */
export function policyDefaults() {
  const out = {}
  for (const k of POLICY_KEYS) out[k] = POLICY_SPEC[k].default
  return out
}

const BOOL_TRUE = new Set([true, 1, 'true', 'on', 'yes', '1'])
const BOOL_FALSE = new Set([false, 0, 'false', 'off', 'no', '0'])

/**
 * 校验并规范化单个字段。
 * @returns {{ok: true, value: unknown} | {ok: false, error: string}}
 */
export function coercePolicyValue(key, value) {
  const spec = POLICY_SPEC[key]
  if (!spec) return { ok: false, error: `unknown policy key: ${key}（可用：${POLICY_KEYS.join(', ')}）` }

  if (spec.type === 'bool') {
    if (BOOL_TRUE.has(typeof value === 'string' ? value.trim() : value)) return { ok: true, value: true }
    if (BOOL_FALSE.has(typeof value === 'string' ? value.trim() : value)) return { ok: true, value: false }
    return { ok: false, error: `${key} 需要布尔值（true/false/on/off），收到 ${JSON.stringify(value)}` }
  }

  if (spec.type === 'int') {
    const n = typeof value === 'number' ? value : Number(String(value).trim())
    if (!Number.isFinite(n)) return { ok: false, error: `${key} 需要数字，收到 ${JSON.stringify(value)}` }
    return { ok: true, value: Math.min(spec.max, Math.max(spec.min, Math.trunc(n))) }
  }

  const s = String(value).trim()
  if (!spec.values.includes(s)) {
    return { ok: false, error: `${key} 只能是 ${spec.values.join(' | ')}，收到 ${JSON.stringify(value)}` }
  }
  return { ok: true, value: s }
}

/**
 * 规范化整份策略：缺项用默认值，非法项忽略（保留默认），未知键忽略。
 * 传入的多层对象由调用方自行合并（后者覆盖前者）。
 */
export function normalizePolicy(raw = {}) {
  const out = policyDefaults()
  for (const k of POLICY_KEYS) {
    if (raw && k in raw) {
      const r = coercePolicyValue(k, raw[k])
      if (r.ok) out[k] = r.value
    }
  }
  return out
}

/**
 * 只挑出 whitelist 里存在且合法的字段（写文件前用）。
 * @returns {{values: object, errors: string[]}}
 */
export function pickPolicy(raw = {}) {
  const values = {}
  const errors = []
  for (const [k, v] of Object.entries(raw || {})) {
    if (!POLICY_KEY_SET.has(k)) {
      errors.push(`unknown policy key: ${k}`)
      continue
    }
    const r = coercePolicyValue(k, v)
    if (r.ok) values[k] = r.value
    else errors.push(r.error)
  }
  return { values, errors }
}

/** 一行人类可读的取值规则，用于 `keys` / `--help` */
export function describeSpec(key) {
  const spec = POLICY_SPEC[key]
  if (spec.type === 'bool') return 'bool (true|false)'
  if (spec.type === 'int') return `int ${spec.min}..${spec.max}`
  return spec.values.join(' | ')
}
