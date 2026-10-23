/**
 * 内置信任根：随包分发的默认公钥。
 *
 * 存在的理由：客户端不该每台都先 keygen 再配一遍才能用自动更新。把公钥编进
 * 构建产物，装好即可验签——配置里的 `updatePublicKey` 只在需要**覆盖**它时才填。
 *
 * 它**不是**轮换链的替代品，而是链的起点（第 0 环）。链的第 1 环及以后由
 * keyring.mjs 持久化到缓存目录，随版本走。
 *
 * 本文件由 `lanhub-keygen --embed` 生成，不要手改。留空表示"没有内置信任根"，
 * 此时行为与 0.7.0 之前一致：未配置公钥即拒绝自动更新。
 *
 * 安全边界：这里只有**公钥**。私钥永远不进仓库、不进包、不进任何产物。
 */
export const EMBEDDED_PUBLIC_KEY = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAMAH+EyacshC+6Y8/1QPJWYCchINsf/mMjAsOGGDESpo=
-----END PUBLIC KEY-----
`
