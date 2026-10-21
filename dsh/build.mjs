/**
 * dsh-plugin-lan-hub 构建脚本：src/*.ts -> dist/index.js（ESM）
 *
 * 为什么必须编译成 JS 才能发布：
 * Node 的 type stripping 明确不支持 node_modules 下的文件，实测报错为
 *   "Stripping types is currently unsupported for files under node_modules"
 * 而 DSH 插件恰恰是以 node_modules 包的形式被加载的。因此直接把 .ts 源码
 * 作为 main 发布，在 DSHA（dsh 0.2.0-rc.2 / Node 24）上的表现就是：
 *   "dsh: warning: 1 entry did not activate"
 *   "lan-hub (dsh-plugin-lan-hub): failed to import"
 * 所以本插件发布的是编译产物，@deepseek-ai/* 依赖保持 external 由运行时提供。
 */
import { build } from 'esbuild'

await build({
  entryPoints: ['src/index.ts'],
  bundle: true,
  format: 'esm',
  platform: 'node',
  target: 'node22',
  external: [
    '@deepseek-ai/cordis',
    '@deepseek-ai/dsh-tools',
    '@deepseek-ai/schemastery',
  ],
  outfile: 'dist/index.js',
  logLevel: 'info',
})
