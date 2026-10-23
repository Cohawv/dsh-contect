/**
 * dsh-plugin-lan-hub 构建脚本
 *
 * 产物五个：
 *   dist/index.js             插件主体（DSH 加载的 main）
 *   dist/lanhub-policy.mjs    策略 CLI（单文件，无外部依赖，可作 bin 直接跑）
 *   dist/lanhub-update.mjs    更新通道 CLI —— 消费侧：验签 / 下载 / 预置 / 自检
 *   dist/lanhub-publish.mjs   发布侧：签名并生成清单（私钥只在构建机）
 *   dist/lanhub-keygen.mjs    生成 Ed25519 密钥对
 *
 * 为什么必须编译成 JS 发布：
 * Node 的 type stripping 明确不支持 node_modules 下的文件，实测报错
 *   "Stripping types is currently unsupported for files under node_modules"
 * 而 DSH 插件恰恰是以 node_modules 包的形式被加载的。因此直接把 .ts 源码
 * 作为 main 发布，在 DSHA（dsh 0.2.0-rc.2 / Node 24）上的表现就是：
 *   "dsh: warning: 1 entry did not activate"
 *   "lan-hub (dsh-plugin-lan-hub): failed to import"
 * 所以本插件发布的是编译产物，@deepseek-ai/* 依赖保持 external 由运行时提供。
 *
 * 为什么要 minify：
 *   插件 41.7KB → 24.8KB（约 -40%），安装包同步变小；keepNames 保留函数名，
 *   stack trace 仍可定位。需要可读产物调试时：NO_MINIFY=1 node build.mjs
 *
 * 为什么 CLI 也要打包：
 *   源码态 CLI 依赖 ../src/policy.mjs，发布包就必须带上整个 src/（含 54KB .ts）。
 *   打包成单文件后 src/ 完全不进包，且每个 CLI 只剩一个文件要分发。
 */
import fs from 'node:fs'
import { build } from 'esbuild'

const pkg = JSON.parse(fs.readFileSync(new URL('./package.json', import.meta.url), 'utf8'))
const minify = !process.env.NO_MINIFY
const external = ['@deepseek-ai/cordis', '@deepseek-ai/dsh-tools', '@deepseek-ai/schemastery']
const common = {
  bundle: true,
  format: 'esm',
  platform: 'node',
  target: 'node22',
  external,
  minify,
  keepNames: minify,
  logLevel: 'info',
  define: { __LANHUB_VERSION__: JSON.stringify(pkg.version) },
}

await build({ ...common, entryPoints: ['src/index.ts'], outfile: 'dist/index.js' })

// 两个 CLI 共用同一套构建参数。源文件自带 #!/usr/bin/env node，esbuild 会把它保留在
// 产物首行；这里不能再加 banner，否则会得到两行 shebang，Node 只剥第一行 → SyntaxError。
for (const [entry, out] of [
  ['tools/lanhub-policy.mjs', 'dist/lanhub-policy.mjs'],
  ['tools/lanhub-update.mjs', 'dist/lanhub-update.mjs'],
  ['tools/lanhub-publish.mjs', 'dist/lanhub-publish.mjs'],
  ['tools/lanhub-keygen.mjs', 'dist/lanhub-keygen.mjs'],
  ['tools/lanhub-release.mjs', 'dist/lanhub-release.mjs'],
]) {
  await build({ ...common, entryPoints: [entry], outfile: out })
}
