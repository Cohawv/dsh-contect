/* =====================================================================
   LAN Agent Hub 控制台 — Service Worker（V7 ③ PWA，零依赖静态资源）

   职责：
     1. 预缓存 app shell（HTML/CSS/JS），离线可用
     2. 运行时缓存：API GET 请求 network-first，失败回退缓存
     3. SSE 连接由页面管理，SW **不代理** —— /api/stream、/api/logs/stream 一律旁路

   为什么 SSE 必须旁路：EventSource 是长连接（text/event-stream），SW 一旦
   respondWith 就会把它当成一次普通 fetch 处理，缓冲/缓存语义都与流式推送冲突，
   且断线重连逻辑由页面的 EventSource 自己负责。这里让它直接走浏览器网络栈。

   纯逻辑（URL 分流 / 缓存策略）全部挂在 self.LanHubSW 上导出，
   供 internal/plugins/webconsole/console-pwa-sim.cjs 在 Node vm 里驱动断言。
   ===================================================================== */
(function () {
  'use strict';

  // 缓存版本：升级前端即应改此值（与 app.js 的 APP_VERSION 保持一致）。
  // V8f 加了「性能」视图并改了 app.js/index.html/styles.css —— 不 bump 的话
  // 老用户刷新拿到的仍是缓存里的旧壳，界面上根本看不到新视图。
  const SW_VERSION = '7.6';
  const CACHE_NAME = 'lanhub-shell-v' + SW_VERSION;

  // app shell：离线启动所需的最小集合（全部为静态资源）
  const SHELL = [
    './',
    './index.html',
    './styles.css',
    './app.js',
    './manifest.json',
    './icon.svg',
    './icon-maskable.svg',
  ];
  const SHELL_INDEX = './index.html';

  // 必须由页面直连、SW 不得代理的端点（SSE 长连接）
  const SSE_PATHS = ['/api/stream', '/api/logs/stream'];

  const SHELL_EXT_RE = /\.(html|css|js|json|svg|ico|png|woff2)$/i;

  /* ---------------- URL 分流（纯字符串处理，不依赖 URL 构造器） ---------------- */

  // 取 path：去掉 query / hash / origin；相对路径原样返回。
  function pathOf(url) {
    var s = String(url == null ? '' : url);
    var i = s.indexOf('?');
    if (i >= 0) s = s.slice(0, i);
    i = s.indexOf('#');
    if (i >= 0) s = s.slice(0, i);
    var m = /^[a-zA-Z][a-zA-Z0-9+.\-]*:\/\/[^/]+(\/.*)?$/.exec(s);
    if (m) return m[1] || '/';
    return s;
  }

  // SSE 端点：精确命中或以 '<path>/' 开头
  function isSSE(url) {
    var p = pathOf(url);
    for (var i = 0; i < SSE_PATHS.length; i++) {
      if (p === SSE_PATHS[i] || p.indexOf(SSE_PATHS[i] + '/') === 0) return true;
    }
    return false;
  }

  function isApi(url) {
    var p = pathOf(url);
    return p === '/api' || p.indexOf('/api/') === 0;
  }

  function isShellAsset(url) {
    var p = pathOf(url);
    if (p === '' || p === '/') return true;
    return SHELL_EXT_RE.test(p);
  }

  /**
   * 路由判定。返回：
   *   'bypass'            —— SSE，SW 完全不介入
   *   'api-network-first' —— /api/** 的 GET，网络优先、失败回落缓存
   *   'shell-cache-first' —— app shell 静态资源，缓存优先（后台更新）
   *   'passthrough'       —— 非 GET 请求 / 其它同源外资源，交给浏览器默认行为
   */
  function routeOf(url, method) {
    if (isSSE(url)) return 'bypass';
    var m = String(method == null ? 'GET' : method).toUpperCase();
    if (m !== 'GET') return 'passthrough';
    if (isApi(url)) return 'api-network-first';
    if (isShellAsset(url)) return 'shell-cache-first';
    return 'passthrough';
  }

  /* ---------------- 缓存访问（真实 SW 与 vm 替身都能跑） ---------------- */

  function cacheApi() {
    if (typeof caches !== 'undefined' && caches) return caches;
    if (typeof self !== 'undefined' && self && self.caches) return self.caches;
    return null;
  }

  function openCache() {
    var c = cacheApi();
    if (!c || typeof c.open !== 'function') return Promise.resolve(null);
    return c.open(CACHE_NAME);
  }

  function isFresh(res) {
    return !!res && res.ok !== false;
  }

  function store(cache, url, res) {
    if (!cache || typeof cache.put !== 'function') return;
    try {
      return cache.put(url, res && res.clone ? res.clone() : res);
    } catch (_) {
      return null;
    }
  }

  // 离线兜底响应：让前端拿到一个可解析的 JSON，而不是网络层报错导致白屏。
  function makeOfflineResponse() {
    const body = JSON.stringify({ ok: false, error: 'offline', offline: true });
    if (typeof Response === 'function') {
      try {
        return new Response(body, {
          status: 503,
          headers: { 'Content-Type': 'application/json' },
        });
      } catch (_) { /* 环境不支持时降级为普通对象 */ }
    }
    return { ok: false, status: 503, offline: true, _offline: true, text: function () { return Promise.resolve(body); } };
  }

  /* ---------------- 两种缓存策略 ---------------- */

  // API GET：网络优先，失败回落缓存，都没有才返回 503
  async function networkFirst(req, url) {
    const cache = await openCache();
    try {
      const res = await fetch(req);
      if (isFresh(res)) {
        await store(cache, url, res);
        return res;
      }
    } catch (_) { /* 离线 → 走缓存 */ }
    if (cache && typeof cache.match === 'function') {
      try {
        const hit = await cache.match(url);
        if (hit) {
          hit._fromCache = true;
          return hit;
        }
      } catch (_) { /* 忽略 */ }
    }
    return makeOfflineResponse();
  }

  // app shell：缓存优先 + 后台更新（stale-while-revalidate）
  async function shellCacheFirst(req, url) {
    const cache = await openCache();
    if (cache && typeof cache.match === 'function') {
      try {
        const hit = await cache.match(url);
        if (hit) {
          // 后台静默更新，失败不影响本次响应
          try {
            fetch(req).then(function (fresh) {
              if (isFresh(fresh)) store(cache, url, fresh);
            }).catch(function () {});
          } catch (_) { /* 忽略 */ }
          hit._fromCache = true;
          return hit;
        }
      } catch (_) { /* 忽略 */ }
    }
    try {
      const res = await fetch(req);
      if (isFresh(res)) {
        await store(cache, url, res);
        return res;
      }
    } catch (_) { /* 离线 → 兜底 */ }
    // 导航请求离线时回退到已缓存的首页，保证 PWA 能冷启动
    if (isNavigation(req) && cache && typeof cache.match === 'function') {
      try {
        const shell = await cache.match(SHELL_INDEX);
        if (shell) {
          shell._fromCache = true;
          return shell;
        }
      } catch (_) { /* 忽略 */ }
    }
    return makeOfflineResponse();
  }

  function isNavigation(req) {
    if (!req) return false;
    if (req.mode === 'navigate' || req.destination === 'document') return true;
    var p = pathOf(req.url);
    return p === '/' || /\/$/.test(p) || /\.html$/i.test(p);
  }

  /* ---------------- 生命周期 ---------------- */

  async function precache() {
    const cache = await openCache();
    if (!cache) return false;
    if (typeof cache.addAll === 'function') await cache.addAll(SHELL);
    return true;
  }

  async function cleanup() {
    const c = cacheApi();
    if (!c || typeof c.keys !== 'function') return [];
    let keys = [];
    try { keys = await c.keys(); } catch (_) { return []; }
    const removed = [];
    for (let i = 0; i < keys.length; i++) {
      const k = keys[i];
      if (k !== CACHE_NAME && String(k).indexOf('lanhub-shell-') === 0) {
        if (typeof c.delete === 'function') { try { await c.delete(k); } catch (_) {} }
        removed.push(k);
      }
    }
    return removed;
  }

  function onInstall(event) {
    if (typeof self !== 'undefined' && self && typeof self.skipWaiting === 'function') {
      try { self.skipWaiting(); } catch (_) {}
    }
    const p = precache();
    if (event && typeof event.waitUntil === 'function') event.waitUntil(p);
    return p;
  }

  function onActivate(event) {
    const p = cleanup().then(function (removed) {
      if (typeof self !== 'undefined' && self && self.clients && typeof self.clients.claim === 'function') {
        try { self.clients.claim(); } catch (_) {}
      }
      return removed;
    });
    if (event && typeof event.waitUntil === 'function') event.waitUntil(p);
    return p;
  }

  /* ---------------- fetch 拦截 ---------------- */

  // 返回值：bypass / passthrough 时为 undefined（不 respondWith），
  // 其余为响应 Promise。导出的目的就是让测试能直接断言这个分流行为。
  function handleFetch(event) {
    if (!event) return undefined;
    let req = event.request;
    if (typeof req === 'string') req = { url: req, method: 'GET' };
    if (!req || !req.url) return undefined;
    const method = req.method || 'GET';
    const route = routeOf(req.url, method);
    if (route === 'bypass' || route === 'passthrough') return undefined;
    const p = route === 'api-network-first'
      ? networkFirst(req, req.url)
      : shellCacheFirst(req, req.url);
    if (typeof event.respondWith === 'function') event.respondWith(p);
    return p;
  }

  /* ---------------- 注册与导出 ---------------- */

  if (typeof self !== 'undefined' && self) {
    if (typeof self.addEventListener === 'function') {
      self.addEventListener('install', onInstall);
      self.addEventListener('activate', onActivate);
      self.addEventListener('fetch', handleFetch);
    }
    // 验证钩子：给 console-pwa-sim.cjs 在 vm 里直接驱动断言
    // （sw.js 本就是公开静态资源，不构成新的信息暴露面）。
    self.LanHubSW = {
      SW_VERSION: SW_VERSION,
      CACHE_NAME: CACHE_NAME,
      SHELL: SHELL,
      SSE_PATHS: SSE_PATHS,
      pathOf: pathOf,
      isSSE: isSSE,
      isApi: isApi,
      isShellAsset: isShellAsset,
      routeOf: routeOf,
      isNavigation: isNavigation,
      networkFirst: networkFirst,
      shellCacheFirst: shellCacheFirst,
      handleFetch: handleFetch,
      onInstall: onInstall,
      onActivate: onActivate,
      precache: precache,
      cleanup: cleanup,
      makeOfflineResponse: makeOfflineResponse,
    };
  }
})();
