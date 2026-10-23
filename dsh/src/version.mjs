/**
 * 版本号比较。
 *
 * 只处理 x.y.z(-pre)：够用即可，不引 semver 依赖。
 * 单独成文件是因为插件、更新 CLI、发布 CLI 三方都要用，且**必须一致**——
 * 判断"能不能升"的逻辑若两边算出不同结论，就会出现一边拒绝、一边放行。
 */

/** 解析出 [major,minor,patch] 与预发布后缀 */
export function parseVersion(v) {
  const s = String(v ?? '').trim().replace(/^v/, '')
  const [core, pre = ''] = s.split('-', 2)
  const n = core.split('.').map((x) => Number.parseInt(x, 10) || 0)
  return { n: [n[0] || 0, n[1] || 0, n[2] || 0], pre }
}

/** 返回 1（a 更新）/ -1（b 更新）/ 0（相同）。同为 x.y.z 时，预发布版更旧。 */
export function cmpVersion(a, b) {
  const pa = parseVersion(a)
  const pb = parseVersion(b)
  for (let i = 0; i < 3; i += 1) {
    if (pa.n[i] !== pb.n[i]) return pa.n[i] > pb.n[i] ? 1 : -1
  }
  if (pa.pre && !pb.pre) return -1
  if (!pa.pre && pb.pre) return 1
  return 0
}
