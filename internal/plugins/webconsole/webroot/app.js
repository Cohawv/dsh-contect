/* =====================================================================
   LAN Agent Hub 控制台 — 前端逻辑（零依赖原生 JS）
   仅调用网关既有 REST 接口，并新增 /api/status、/api/rooms、/api/broadcast，
   以及 V4 的管理端点：/api/plugins、/api/plugins/detail、/api/plugins/toggle、
   /api/plugins/reload、/api/settings、/api/manifest。
   覆盖：布局导航（含 URL hash 深链）、数据表格/列表、表单与按钮、加载态、
        空状态、错误提示、成功/失败反馈、禁用态、响应式（由 CSS 负责）、自动刷新。
   ===================================================================== */
(function () {
  'use strict';

  const $ = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

  /* ---------------- i18n 引擎（V7 ④） ----------------
     语言包机制：内置 zh-CN/en 随二进制发布，外部目录（配置项 i18n.dir）里的
     <lang>.json 按键覆盖内置包 —— 新增语言 = 丢一个文件，不改代码、不重新编译。

     默认 zh-CN：保持既有偏好，不因本版改变默认行为。
     HTML 里的 <span data-i18n="key">中文</span> 本身就是 zh-CN 源文案，
     所以语言包尚未就绪时**不覆盖** DOM，首屏永远是正确的简体中文。

     缺失键回退到键名本身并 console.warn（开发期可见），绝不阻塞渲染。
     时间/数字一律走浏览器内置 Intl（零依赖）。 */
  const I18N_FALLBACK = 'zh-CN';
  // RTL 预留：本版不验证阿拉伯语，但 dir 的框架先就位，届时只需补一份语言包。
  const I18N_RTL = { ar: 'rtl', he: 'rtl', fa: 'rtl', ur: 'rtl' };

  const i18n = {
    lang: I18N_FALLBACK,      // 当前界面语言
    serverDefault: I18N_FALLBACK, // 服务端声明的默认语言（/api/i18n/ 的 default）
    packs: {},                // lang -> { key: value }
    missing: {},              // 当前语言缺失的键（缺失键计数用，便于翻译协作）
    langs: [],                // 服务端清单里的可用语言
    refKeys: 0,               // 参考键总数（内置 zh-CN 的键数）
    ready: false,             // 至少成功加载过一个语言包
  };

  // t 是唯一的翻译入口：当前语言 → 源语言兜底 → 键名 + 告警。
  function t(key, params) {
    let s = '';
    const cur = i18n.packs[i18n.lang];
    const base = i18n.packs[I18N_FALLBACK];
    if (cur && cur[key]) s = cur[key];
    else if (base && base[key]) s = base[key];
    if (!s) {
      if (!i18n.missing[key]) {
        i18n.missing[key] = true;
        // 只告警一次：高频渲染的键不该把控制台刷爆，但第一次必须看得见。
        if (typeof console !== 'undefined' && console.warn) {
          console.warn('[i18n] 缺失翻译键 ' + key + '（语言 ' + i18n.lang + '）');
        }
      }
      s = key;
    }
    if (params) {
      Object.keys(params).forEach((k) => {
        s = s.split('{' + k + '}').join(String(params[k]));
      });
    }
    return s;
  }

  function i18nMissingCount() { return Object.keys(i18n.missing).length; }

  // ts = 带源文案的翻译。动态渲染一律走 ts 而不是裸 t()：
  //   · 语言包未就绪（端点挂了 / 首屏还没拉到）→ 直接用**源码里的中文原文**；
  //   · 语言包就绪但该键缺失 → 同样回落中文原文（t 已记过缺失，这里不再重复告警）。
  // 结果：国际化是纯增强，任何情况下都不会把 "nav.overview" 这种键名露到界面上。
  function ts(key, zh) {
    return tsp(key, zh, null);
  }

  // subst 做 {name} 占位符替换（语言包与中文源共用同一套占位符名）。
  function subst(s, params) {
    if (!params) return s;
    Object.keys(params).forEach((k) => {
      s = s.split('{' + k + '}').join(String(params[k]));
    });
    return s;
  }

  // tsp = 带源文案 + 占位符参数的翻译（带变量的文案用它）。
  // 键缺失时按中文源 + 参数渲染，因此「翻译不全」只会退化成中文，不会露出 {n} 花括号。
  function tsp(key, zh, params) {
    if (!i18n.ready) return subst(zh, params);
    const v = t(key, params);
    return v === key ? subst(zh, params) : v;
  }

  /* Intl 格式化：时间与数字一律走浏览器内置实现，零依赖、随语言自动切换。 */
  function fmtInt(n) {
    try { return new Intl.NumberFormat(i18n.lang).format(n); } catch (_) { return String(n); }
  }
  function fmtTime(ms) {
    try { return new Intl.DateTimeFormat(i18n.lang, { timeStyle: 'medium' }).format(new Date(ms)); }
    catch (_) { return new Date(ms).toLocaleTimeString(); }
  }
  function fmtDateTime(ms) {
    try { return new Intl.DateTimeFormat(i18n.lang, { dateStyle: 'medium', timeStyle: 'medium' }).format(new Date(ms)); }
    catch (_) { return new Date(ms).toLocaleString(); }
  }
  // fmtAgo 优先用 RelativeTimeFormat（"3秒前" / "3 seconds ago"），
  // 环境不支持时退回语言包里的 {n} 秒前 / {n} 分钟前。
  function fmtAgo(ms) {
    if (!ms) return t('common.dash');
    const diff = Date.now() - ms;
    if (diff < 0) return fmtTime(ms);
    const secs = Math.max(0, Math.floor(diff / 1000));
    if (typeof Intl !== 'undefined' && Intl.RelativeTimeFormat) {
      try {
        const rtf = new Intl.RelativeTimeFormat(i18n.lang, { numeric: 'auto' });
        if (secs < 60) return rtf.format(-secs, 'second');
        if (secs < 3600) return rtf.format(-Math.floor(secs / 60), 'minute');
        if (secs < 86400) return rtf.format(-Math.floor(secs / 3600), 'hour');
        return rtf.format(-Math.floor(secs / 86400), 'day');
      } catch (_) { /* 落到下面的字典文案 */ }
    }
    if (diff < 60000) return t('common.agoSeconds', { n: secs });
    if (diff < 3600000) return t('common.agoMinutes', { n: Math.floor(diff / 60000) });
    return fmtTime(ms);
  }

  // applyStaticI18n 遍历 data-i18n 标记做**全量替换**（textContent / placeholder / title）。
  // 属性形式存在的文案（placeholder、title）单独标记 —— 它们不是文本节点，
  // 用 data-i18n 会把元素内容冲掉。
  function applyStaticI18n() {
    if (!i18n.ready) return; // 语言包未就绪：保留 HTML 里的源语言文案
    $$('[data-i18n]').forEach((el) => {
      const key = el.getAttribute && el.getAttribute('data-i18n');
      if (key) el.textContent = t(key);
    });
    $$('[data-i18n-placeholder]').forEach((el) => {
      const key = el.getAttribute && el.getAttribute('data-i18n-placeholder');
      if (key && el.setAttribute) el.setAttribute('placeholder', t(key));
    });
    $$('[data-i18n-title]').forEach((el) => {
      const key = el.getAttribute && el.getAttribute('data-i18n-title');
      if (key && el.setAttribute) el.setAttribute('title', t(key));
    });
    // aria-label 也是"文案"，但它是属性不是文本节点：单独走一个标记，
    // 否则读屏标签会永远停在首屏的中文上（V8f 性能视图的 KPI 组与曲线需要它）。
    $$('[data-i18n-aria-label]').forEach((el) => {
      const key = el.getAttribute && el.getAttribute('data-i18n-aria-label');
      if (key && el.setAttribute) el.setAttribute('aria-label', t(key));
    });
    // lang/dir 框架就位：RTL 语言只需在语言包里出现即可自动翻转。
    const html = document.documentElement;
    if (html && html.setAttribute) {
      html.setAttribute('lang', i18n.lang);
      html.setAttribute('dir', I18N_RTL[String(i18n.lang).split('-')[0]] || 'ltr');
    }
  }

  function loadLangPack(lang) {
    if (i18n.packs[lang]) return Promise.resolve(true);
    return api('GET', '/api/i18n/' + encodeURIComponent(lang)).then((r) => {
      if (!r.ok || !r.data || !r.data.messages) return false;
      i18n.packs[lang] = r.data.messages;
      i18n.ready = true;
      return true;
    });
  }

  // setLang 切换语言并**全量重渲染**（无需刷新页面）。
  // 重绘一律用已有缓存，不再发数据请求 —— 切语言是纯表现层动作，不该产生额外流量。
  function setLang(lang) {
    lang = String(lang || I18N_FALLBACK);
    return loadLangPack(lang).then((ok) => {
      if (!ok) return false;
      i18n.lang = lang;
      i18n.missing = {};
      try { localStorage.setItem('lanhub.lang', lang); } catch (_) { /* 隐私模式下忽略 */ }
      applyStaticI18n();
      rerenderAllViews();
      renderI18nStatus();
      return true;
    });
  }

  // rerenderAllViews 用**已有缓存**把界面上的动态文本全部重画一遍。
  //
  // 切语言是纯表现层动作：这里不发任何数据请求、不改任何业务状态 ——
  // 只把「已经在内存里的数据」按新语言重新渲染。单个渲染器抛错不该拖垮整体，
  // 因此逐个 try/catch（旧数据 + 新语言重绘本身是安全的）。
  function rerenderAllViews() {
    // ① 静态标记（data-i18n / placeholder / title）+ <html lang dir>
    applyStaticI18n();
    // ② 当前视图的标题与副标题
    const meta = pageText(activeView);
    const te = $('#page-title');
    const se = $('#page-sub');
    if (te) te.textContent = meta[0];
    if (se) se.textContent = meta[1];
    // ③ 连接药丸：沿用当前状态，只换文案（绝不重新探测连接）
    if (connPill) setConn(connPill.getAttribute('data-state') || 'offline');
    // ④ 各数据区：有缓存才重绘，没缓存就保持骨架/空态不动
    const jobs = [
      function () { if (lastStatus) renderStatCards(lastStatus); },
      function () { renderAbout(); },
      function () { if (agentsLoaded) renderAgents(); },
      function () { if (roomsLoaded) renderRooms(roomsCache); },
      function () { renderInbox(); },
      function () { if (pluginsCache) renderPlugins(); },
      function () { if (settingsCache) renderSettings(); },
      function () { if (lastMetrics) renderMetrics(lastMetrics); },
      function () { renderTrend(lastSamples, msgRateSamples); },
      function () { renderPerf(); }, // V8f：性能视图（无缓存数据时 renderPerf 自己早退）
      function () { renderEvents(); },
      function () { renderLogs(); },
    ];
    jobs.forEach((fn) => {
      try { fn(); } catch (_) { /* 单个渲染器失败不影响整体切语言 */ }
    });
    // ⑤ 命令面板条目（标题/关键词/分组随语言变，下次打开即为新语言）
    try { buildCommands(); } catch (_) {}
    // ⑥ 语言下拉自身
    syncLangSelect();
  }

  // renderI18nStatus 把「缺失键数量」显示在语言下拉旁（翻译协作时一眼可见）。
  // 全命中时显示「无」，因此它同时也是「语言包完整性」的即时指示器。
  function renderI18nStatus() {
    const el = $('#i18n-status');
    if (!el) return;
    const n = i18nMissingCount();
    el.textContent = n === 0
      ? ts('i18n.missingZero', '无')
      : ts('i18n.missingKeys', '缺失键') + ' ' + fmtInt(n);
    el.title = (i18n.refKeys ? fmtInt(i18n.refKeys) + ' / ' : '') + ts('i18n.missingKeys', '缺失键') + ' ' + n;
    el.setAttribute('data-missing', String(n));
    el.hidden = false;
  }

  // syncLangSelect 用服务端清单重建语言下拉（值 = 语言代码，文案优先用语言包里的自称）。
  function syncLangSelect() {
    const sel = $('#lang-select');
    if (!sel) return;
    const langs = (i18n.langs && i18n.langs.length) ? i18n.langs : [I18N_FALLBACK];
    const want = langs.slice();
    if (want.indexOf(i18n.lang) < 0) want.push(i18n.lang); // 语言包已在手上但不在清单里（外部新增）
    sel.innerHTML = want.map((code) => {
      // 语言名用「自称」：zh-CN → 简体中文，en → English。语言包里没有就用代码本身。
      const label = ts('i18n.lang.' + code, code);
      return '<option value="' + esc(code) + '">' + esc(label) + '</option>';
    }).join('');
    sel.value = i18n.lang;
  }

  // initI18n 启动流程：读本地偏好（默认 zh-CN）→ 拉语言清单 → 加载语言包 → 需要时重画。
  //
  // 全程静默降级：任何一步失败，界面保留 HTML 里的中文源文案，功能完全不受影响。
  function initI18n() {
    let saved = '';
    try { saved = localStorage.getItem('lanhub.lang') || ''; } catch (_) { /* 隐私模式忽略 */ }
    const want = saved || I18N_FALLBACK;
    return api('GET', '/api/i18n/').then((r) => {
      if (r.ok && r.data) {
        if (r.data.default) i18n.serverDefault = r.data.default;
        i18n.langs = Array.isArray(r.data.languages) ? r.data.languages.slice() : [];
        i18n.refKeys = r.data.refKeys || 0;
      }
      if (!i18n.langs.length) i18n.langs = [I18N_FALLBACK];
      if (want === I18N_FALLBACK) {
        // 默认语言也要把包拉下来：只有包在手上，缺失键统计才有参照系。
        return loadLangPack(I18N_FALLBACK).then(() => {
          applyStaticI18n();
          syncLangSelect();
          renderI18nStatus();
          return true;
        });
      }
      return setLang(want);
    }).catch(() => false);
  }

  /* ---------------- API 封装 ---------------- */
  async function api(method, path, body) {
    try {
      const opts = { method, headers: {} };
      if (body !== undefined) {
        opts.headers['Content-Type'] = 'application/json';
        opts.body = JSON.stringify(body);
      }
      const res = await fetch(path, opts);
      let data = null;
      const text = await res.text();
      if (text) { try { data = JSON.parse(text); } catch (_) { /* 非 JSON 也接受 */ } }
      if (!res.ok) {
        const msg = (data && (data.error || data.message)) || ('HTTP ' + res.status);
        return { ok: false, status: res.status, data, error: msg };
      }
      return { ok: true, status: res.status, data };
    } catch (e) {
      return { ok: false, status: 0, data: null, error: e.message || ts('common.networkError', '网络错误') };
    }
  }

  /* ---------------- 工具 ---------------- */
  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  // fmtAgo 由 i18n 段提供（带 Intl.RelativeTimeFormat 与语言包兜底），此处不再重复定义 ——
  // 曾经的第二份中文硬编码实现会因函数提升覆盖掉国际化版本，直接删除。
  function fmtDefault(v) {
    if (v === null || v === undefined) return '—';
    if (typeof v === 'boolean') return v ? ts('common.on', '开启') : ts('common.off', '关闭');
    if (typeof v === 'string') return v === '' ? ts('common.empty', '（空）') : v;
    return String(v);
  }
  function setBtnLoading(btn, loading) {
    if (!btn) return;
    if (loading) { btn.classList.add('is-loading'); btn.disabled = true; }
    else { btn.classList.remove('is-loading'); btn.disabled = false; }
  }

  /* ---------------- Toast ---------------- */
  const toastStack = $('#toast-stack');
  function toast(message, type) {
    type = type || 'info';
    const icons = { success: '✓', error: '✕', info: 'ℹ' };
    const el = document.createElement('div');
    el.className = 'toast toast--' + type;
    el.innerHTML = '<span class="toast__icon">' + icons[type] + '</span><span>' + esc(message) + '</span>';
    toastStack.appendChild(el);
    const ttl = type === 'error' ? 5000 : 3200;
    setTimeout(() => {
      el.classList.add('is-leaving');
      setTimeout(() => el.remove(), 220);
    }, ttl);
  }

  /* ---------------- 连接状态药丸（三态：已连接 SSE / 重连中 / 已降级轮询） ---------------- */
  const connPill = $('#conn-pill');
  // V7 ①：三态药丸的规范文案——轮询中 → 已连接(SSE) → 重连中。
  // 三态共用一套 data-state，样式与读屏播报都从这里取，避免各处散写文案导致状态漂移。
  // V7 ④：每态是 [翻译键, 简体中文源文案] 二元组——语言包就绪后走 t()，未就绪时
  // 用中文源文案（与 HTML 首屏一致，避免异步加载期间闪出键名）。
  const CONN_TEXT = {
    polling: ['conn.polling', '轮询中'],
    online: ['conn.online', '已连接(SSE)'],
    reconnecting: ['conn.reconnecting', '重连中'],
    connecting: ['conn.reconnecting', '重连中'],
    offline: ['conn.offline', '已断开'],
  };
  // connLabel 解析某状态的可显示文案：语言包就绪 → t(键)（缺失键回退中文源），否则中文源。
  function connLabel(state) {
    const pair = CONN_TEXT[state];
    if (!pair) return '';
    if (!i18n.ready) return pair[1];
    const tr = t(pair[0]);
    return tr === pair[0] ? pair[1] : tr;
  }
  function setConn(state, text) {
    connPill.setAttribute('data-state', state);
    const label = $('.status-pill__text', connPill);
    if (label) label.textContent = text || connLabel(state);
  }

  /* ---------------- 视图切换（支持 URL hash 深链） ---------------- */
  // 每项：[标题键, 副标题键, 中文源标题, 中文源副标题]。
  // 后两项是「语言包未就绪」时的兜底原文 —— 首屏永远是正确的中文，不会闪出键名。
  const pageMeta = {
    overview:  ['view.overview.title', 'view.overview.sub', '概览', '局域网多 Agent 通信中枢运行状态'],
    // V8f：性能视图。登记在这里 = 同时获得导航/hash 深链/命令面板/切语言重绘，
    // 四处（pageMeta、setView、rerenderAllViews、renderers）任缺一处都会出现"切语言后视图空白"。
    perf:      ['view.perf.title', 'view.perf.sub', '性能', '吞吐、丢弃与内存的实时同源视图（每 2 秒采样）'],
    agents:    ['view.agents.title', 'view.agents.sub', '在线 Agent', '当前连接到 Hub 的全部 Agent'],
    rooms:     ['view.rooms.title', 'view.rooms.sub', '聊天室', '创建、加入与管理聊天室'],
    messaging: ['view.messaging.title', 'view.messaging.sub', '发送消息', '向 Agent / 房间发送或广播消息——实时时间线'],
    events:    ['view.events.title', 'view.events.sub', '事件流', '订阅 /api/stream 的实时事件，可过滤、可暂停、可展开载荷'],
    plugins:   ['view.plugins.title', 'view.plugins.sub', '插件', '运行期查看与启停内置能力（必需插件与被依赖插件受保护）'],
    logs:      ['view.logs.title', 'view.logs.sub', '日志', '服务端日志环形缓冲（实时增量 + 级别过滤 + 导出）'],
    settings:  ['view.settings.title', 'view.settings.sub', '设置', '由插件配置声明自动生成的表单，改完即存'],
    about:     ['view.about.title', 'view.about.sub', '服务信息', 'Hub 协议版本与接入端点'],
  };
  // pageText 解析某视图的 [标题, 副标题] 文案（i18n 就绪则翻译，否则中文源）。
  function pageText(name) {
    const m = pageMeta[name] || pageMeta.overview;
    return [ts(m[0], m[2]), ts(m[1], m[3])];
  }
  let activeView = 'overview';

  function viewFromHash() {
    const h = (location.hash || '').replace(/^#/, '');
    return pageMeta[h] ? h : 'overview';
  }

  function setView(name, keepHash) {
    if (!pageMeta[name]) name = 'overview';
    activeView = name;
    $$('.view').forEach((v) => {
      const active = v.getAttribute('data-view') === name;
      v.classList.toggle('is-active', active);
      v.hidden = !active;
    });
    $$('.nav__item').forEach((b) => b.classList.toggle('is-active', b.getAttribute('data-view') === name));
    const meta = pageText(name);
    $('#page-title').textContent = meta[0];
    $('#page-sub').textContent = meta[1];
    if (!keepHash && (location.hash || '').replace(/^#/, '') !== name) {
      location.hash = name; // 深链：刷新后仍停在当前视图
    }
    if (name === 'overview') { loadStatus(true); loadMetrics(true); }
    // V8f：性能视图自带 2 秒采样；进入时立刻取一次，避免首屏等 2 秒才有数。
    if (name === 'perf') { loadPerf(true); perfStartPolling(); }
    if (name === 'agents') loadAgents();
    if (name === 'rooms') loadRooms();
    if (name === 'events') renderEvents(); // 切回事件流时按当前过滤条件重画一次
    if (name === 'plugins') loadPlugins(true);
    if (name === 'logs') loadLogs(true);
    if (name === 'settings') { loadPlugins(true); loadSettings(true); }
    if (name === 'about') renderAbout();
    // 离开日志视图时断开日志流：不在看的连接不该占着服务端名额。
    if (name !== 'logs') closeLogStream();
    // 同理，离开性能视图立刻停表：后台标签页不该每 2 秒打一次接口。
    if (name !== 'perf') perfStopPolling();
  }

  /* ---------------- 概览 + 状态 ---------------- */
  let lastStatus = null;
  async function loadStatus(silent) {
    const r = await api('GET', '/api/status');
    if (!r.ok) {
      if (realtime.state !== 'open') setConn('reconnecting');
      if (!silent) toast(ts('common.connFailed', '无法连接网关：') + r.error, 'error');
      return;
    }
    lastStatus = r.data;
    // 药丸优先表达「实时通道」的状态：SSE 已连上时不要被一次成功的 REST 调用覆盖成"已连接"。
    if (realtime.state !== 'open') setConn('online');
    $('#foot-proto').textContent = r.data.proto;
    requestRender('stats');
    if ($('.view[data-view="about"]').classList.contains('is-active')) renderAbout();
  }
  function renderStatCards(s) {
    const cards = [
      {
        label: ts('overview.stat.agents.label', '在线 Agent'),
        value: s.agentCount,
        sub: ts('overview.stat.agents.sub', '当前已注册'),
      },
      {
        label: ts('overview.stat.rooms.label', '聊天室'),
        value: s.roomCount,
        sub: ts('overview.stat.rooms.sub', '进行中的会话'),
      },
      {
        label: ts('overview.stat.proto.label', '协议版本'),
        value: 'v' + s.proto,
        sub: ts('overview.stat.proto.sub', '对外契约'),
      },
      {
        label: ts('overview.stat.discovery.label', '自动发现'),
        value: s.discovery ? ts('common.enabled', '已启用') : ts('common.disabled', '已关闭'),
        sub: s.discovery
          ? ts('overview.stat.discovery.subOn', 'UDP Beacon')
          : ts('overview.stat.discovery.subOff', '需手动配置 IP'),
      },
    ];
    const grid = $('#stat-grid');
    grid.innerHTML = cards.map((c) =>
      '<div class="stat-card"><div class="stat-card__label">' + esc(c.label) + '</div>' +
      '<div class="stat-card__value">' + esc(c.value) + '</div>' +
      '<div class="stat-card__sub">' + esc(c.sub) + '</div></div>'
    ).join('');
  }
  function renderAbout() {
    const s = lastStatus;
    const kv = $('#about-kv');
    if (!s) { kv.innerHTML = '<div class="skeleton skeleton-line"></div>'; return; }
    const rows = [
      [ts('about.proto', '协议版本'), 'v' + s.proto],
      [ts('about.tcpPort', 'TCP 端口'), s.tcpPort || '—'],
      [ts('about.httpPort', 'HTTP 端口'), s.httpPort || '—'],
      [ts('about.agents', '在线 Agent'), s.agentCount],
      [ts('about.rooms', '聊天室'), s.roomCount],
      [ts('about.discovery', '局域网发现'), s.discovery ? ts('common.enabled', '已启用') : ts('common.disabled', '已关闭')],
      [ts('about.serverTime', '服务器时间'), fmtDateTime(s.serverTime)],
    ];
    kv.innerHTML = rows.map((r) =>
      '<dt>' + esc(r[0]) + '</dt><dd>' + esc(r[1]) + '</dd>'
    ).join('');
  }

  /* ---------------- 在线 Agent 表格 ---------------- */
  let agentsLoaded = false;
  let agentsCache = [];
  async function loadAgents() {
    const tbody = $('#agents-tbody');
    if (!agentsLoaded) {
      tbody.innerHTML = '';
      for (let i = 0; i < 4; i++) {
        tbody.innerHTML += '<tr class="skeleton">' +
          Array(5).fill('<td><div class="skeleton-line"></div></td>').join('') + '</tr>';
      }
    }
    const r = await api('GET', '/api/agents');
    if (!r.ok) {
      if (realtime.state !== 'open') setConn('reconnecting');
      tbody.innerHTML = '<tr><td colspan="6"><div class="error-banner">' +
        esc(ts('common.loadFailed', '加载失败：')) + esc(r.error) +
        '<button class="btn btn--ghost" onclick="location.reload()">' +
        esc(ts('common.retry', '重试')) + '</button></div></td></tr>';
      return;
    }
    agentsCache = (r.data.agents || []);
    agentsLoaded = true;
    renderAgents();
  }
  function renderAgents() {
    const q = ($('#agents-search').value || '').trim().toLowerCase();
    const tbody = $('#agents-tbody');
    const list = agentsCache.filter((a) =>
      !q || (a.id || '').toLowerCase().includes(q) || (a.name || '').toLowerCase().includes(q)
    );
    $('#agents-count').textContent = agentsCache.length;
    if (list.length === 0) {
      const emptyMsg = agentsCache.length === 0
        ? ts('agents.emptyTitle', '当前没有 Agent 在线')
        : ts('agents.emptyFiltered', '没有匹配的 Agent');
      tbody.innerHTML = '<tr><td colspan="6"><div class="empty-state">' +
        '<div class="empty-state__icon">☻</div>' +
        '<div class="empty-state__title">' + esc(emptyMsg) + '</div>' +
        '<div class="empty-state__text">' +
        (agentsCache.length === 0
          ? esc(ts('agents.emptyHint', '等待 Agent 通过 TCP 或 DSH 插件接入 Hub。'))
          : esc(ts('agents.emptyFilterHint', '试试调整筛选条件。'))) +
        '</div></div></td></tr>';
      return;
    }
    tbody.innerHTML = list.map((a) => {
      const caps = (a.caps || []).map((c) =>
        '<span class="badge badge--accent">' + esc(c) + '</span>').join('') || '<span class="muted">—</span>';
      // data-label：≤480px 表格转卡片流时由 CSS 的 content: attr(data-label) 补回列名（V7 ③）
      return '<tr>' +
        '<td data-label="ID"><span class="copyable badge" title="' +
          esc(ts('common.clickToCopy', '点击复制')) + '">' + esc(a.id) + '</span></td>' +
        '<td data-label="' + esc(ts('agents.th.name', '名称')) + '">' + esc(a.name || '—') + '</td>' +
        '<td data-label="' + esc(ts('agents.th.proto', '协议')) + '">' + (a.proto ? ('v' + esc(a.proto)) : '—') + '</td>' +
        '<td data-label="' + esc(ts('agents.th.caps', '能力 (caps)')) + '"><div class="badge-row">' + caps + '</div></td>' +
        '<td data-label="' + esc(ts('agents.th.lastSeen', '最近活跃')) + '">' + esc(fmtAgo(a.lastSeen)) + '</td>' +
        '<td data-label="' + esc(ts('agents.th.action', '操作')) + '"><button class="btn btn--sm" data-msg="' +
          esc(a.id) + '" type="button">' + esc(ts('agents.sendMsg', '发消息')) + '</button></td>' +
        '</tr>';
    }).join('');
  }

  /* ---------------- 聊天室 ---------------- */
  let roomsLoaded = false;
  let roomsCache = [];

  // 会话视图状态：我加入的房间集合 + 收件箱（按窗口从 msg.content 流累积）。
  const myRooms = new Set();
  let inbox = [];
  const INBOX_MAX = 200;
  // 概览消息速率采样（客户端按窗口计数，避免依赖后端逐样本速率字段）。
  let msgWindow = 0;
  let msgRateSamples = [];
  let lastSamples = [];
  let lastTickTs = 0;
  let lastMetrics = null;   // V7 ①：最近一次指标快照，面板冻结解除后据此重绘

  // V7 ① 投递状态索引（seq 为关联键）：msg.routed 给出"送达/离线"，ack 回执给出"已读"。
  // 两个索引都定长淘汰，长时间挂机不会无界增长。
  const routeBySeq = new Map();
  const ackBySeq = new Map();
  const DELIVERY_INDEX_MAX = 1000;
  async function loadRooms() {
    const grid = $('#room-grid');
    if (!roomsLoaded) grid.innerHTML = '<div class="skeleton skeleton-card"></div>';
    const r = await api('GET', '/api/rooms');
    if (!r.ok) {
      if (realtime.state !== 'open') setConn('reconnecting');
      grid.innerHTML = '<div class="error-banner">加载失败：' + esc(r.error) + '</div>';
      return;
    }
    roomsLoaded = true;
    roomsCache = (r.data.rooms || []);
    renderRooms(roomsCache);
  }
  function renderRooms(rooms) {
    const grid = $('#room-grid');
    const myId = getMyId();
    if (rooms.length === 0) {
      grid.innerHTML = '<div class="empty-state" style="grid-column:1/-1">' +
        '<div class="empty-state__icon">⌬</div>' +
        '<div class="empty-state__title">' + esc(ts('rooms.emptyTitle', '还没有聊天室')) + '</div>' +
        '<div class="empty-state__text">' +
          esc(ts('rooms.emptyText', '在上方填写名称即可创建一个聊天室，并把 Agent 邀请进来。')) + '</div></div>';
      return;
    }
    grid.innerHTML = rooms.map((rm) => {
      const members = (rm.members || []).map((m) =>
        '<span class="member-chip">' + esc(m) + '</span>').join('');
      const memberHtml = members || '<span class="member-chip member-chip--empty">' +
        esc(ts('rooms.noMembers', '暂无成员')) + '</span>';
      const joinDisabled = myId ? '' : 'disabled title="' + esc(ts('rooms.needMyId', '先在上方填写你的 Agent ID')) + '"';
      return '<div class="room-card">' +
        '<div class="room-card__head"><div>' +
          '<div class="room-card__name">' + esc(rm.name || ts('rooms.unnamed', '(未命名)')) + '</div>' +
          '<div class="room-card__id copyable" title="' + esc(ts('rooms.copyRoomId', '点击复制房间 ID')) + '">' +
            esc(rm.id) + '</div>' +
        '</div><span class="count-badge">' +
          esc(tsp('rooms.memberCount', '{n} 人', { n: rm.memberCount })) + '</span></div>' +
        '<div class="room-card__members">' + memberHtml + '</div>' +
        '<div class="room-card__actions">' +
          '<button class="btn btn--ghost" data-join="' + esc(rm.id) + '" ' + joinDisabled + '>' +
            esc(ts('rooms.join', '加入')) + '</button>' +
          '<button class="btn btn--danger" data-leave="' + esc(rm.id) + '" ' + joinDisabled + '>' +
            esc(ts('rooms.leave', '离开')) + '</button>' +
        '</div></div>';
    }).join('');
  }

  /* 我的 Agent ID（发送身份 + 加入/离开），持久化到 localStorage */
  function getMyId() { return ($('#my-id') && $('#my-id').value) || localStorage.getItem('lanhub.myId') || ''; }

  // 统一身份：把 id 写入所有「发送方」字段与本地存储，并刷新界面。
  function applyIdentity(id) {
    id = (id || '').trim();
    if (!id) return;
    localStorage.setItem('lanhub.myId', id);
    const fields = ['#my-id', '#d-from', '#r-from', '#b-from'];
    fields.forEach((sel) => {
      const el = $(sel);
      if (el && !el.value) el.value = id; // 仅填空字段，避免覆盖用户手改
    });
    const badge = $('#inbox-id');
    if (badge) badge.textContent = '· ' + id;
    if (roomsLoaded) loadRooms(); // 刷新房间按钮禁用态
  }

  function bindMyId() {
    const inp = $('#my-id');
    if (!inp) return;
    const saved = localStorage.getItem('lanhub.myId') || '';
    // 预填「我的身份」并同步到各发送方字段
    ['#my-id', '#d-from', '#r-from', '#b-from'].forEach((sel) => {
      const el = $(sel);
      if (el && saved) el.value = saved;
    });
    const badge = $('#inbox-id');
    if (badge && saved) badge.textContent = '· ' + saved;

    // #my-id 变更 → 作为权威身份，回填到空的发送方字段
    inp.addEventListener('input', () => applyIdentity(inp.value));
    // 任一发送方字段手填 → 记住为身份（若 #my-id 为空则同步过去）
    ['#d-from', '#r-from', '#b-from'].forEach((sel) => {
      const el = $(sel);
      if (!el) return;
      el.addEventListener('input', () => {
        const v = el.value.trim();
        if (!v) return;
        localStorage.setItem('lanhub.myId', v);
        if (!$('#my-id').value) { $('#my-id').value = v; }
        const badge = $('#inbox-id');
        if (badge) badge.textContent = '· ' + v;
      });
    });
  }

  /* ---------------- 会话视图（V7 ① 升级为「消息时间线」） ---------------- */
  // applyMsgContent 处理 SSE 推送的 msg.content（含正文）。
  // 只把「与我相关 / 我所在房间 / 他人广播」的消息放进时间线，避免信息过载。
  function applyMsgContent(e) {
    let m;
    try { m = JSON.parse(e.data); } catch (_) { return; }
    // 概览速率采样：统计**所有**流过的消息（不论是否入箱），才能反映真实消息速率
    msgWindow++;
    pushEvent('msg.content', m);
    const myId = getMyId();
    const isOut = !!myId && m.from === myId;
    const isIn = !!myId && m.to === myId;
    const isRoom = !!m.roomId && myRooms.has(m.roomId);
    const isBroadcast = !m.to && !m.roomId;
    if (!(isOut || isIn || isRoom || (isBroadcast && m.from !== myId))) return;

    let dir = 'in';
    if (isOut) dir = 'out';
    else if (isBroadcast) dir = 'broadcast';
    else if (m.roomId) dir = 'room';

    const item = {
      dir,
      from: m.from || '',
      to: m.to || '',
      roomId: m.roomId || '',
      text: m.text || '',
      ts: m.ts || 0,
      seq: m.seq || 0,
      msgId: m.msgId || '',
    };
    inbox.push(item);
    if (inbox.length > INBOX_MAX) inbox = inbox.slice(inbox.length - INBOX_MAX);
    // 前向兼容：V6 的 ack 回执键是 msgId，当前事件载荷不带该字段，
    // 一旦协议补齐（或 store 历史回放出 msgId）这里会自动点亮「已读」。
    if (item.msgId) queryDelivery(item.msgId, item.seq);
    requestRender('timeline');
  }

  // applyMsgRouted 处理 msg.routed：既进事件流，也按 seq 回填时间线的投递结果。
  function applyMsgRouted(e) {
    let p = null;
    try { p = JSON.parse(e.data); } catch (_) { return; }
    if (!p) return;
    pushEvent('msg.routed', p);
    const seq = Number(p.seq);
    if (seq > 0) {
      routeBySeq.set(seq, { delivered: Number(p.delivered) || 0, kind: p.kind || '', roomId: p.roomId || '' });
      trimIndex(routeBySeq);
    }
    requestRender('timeline');
  }

  // queryDelivery 查单条消息的 ack 回执（/api/delivery/{msgId}）。
  function queryDelivery(msgId, seq) {
    if (!msgId || !seq) return;
    api('GET', '/api/delivery/' + encodeURIComponent(msgId)).then((r) => {
      if (!r.ok || !r.data || !r.data.status || r.data.status === 'unknown') return;
      ackBySeq.set(seq, r.data.status);
      trimIndex(ackBySeq);
      requestRender('timeline');
    });
  }

  function trimIndex(map) {
    if (map.size > DELIVERY_INDEX_MAX) map.delete(map.keys().next().value);
  }

  // 投递状态徽标（V6 ack 语义）：已读 / 未读 / 失败 / 投递中。
  // 只对我发出的消息判定——收到的消息无从知晓对端是否回执，不猜。
  // 文案走 i18n：label/title 在渲染时才解析（语言切换后重绘即生效）。
  const ACK_BADGE = {
    pending:   { state: 'pending', lk: 'msg.ack.pending.label',   lz: '未读',  tk: 'msg.ack.pending.title',   tz: '已送达对端，等待回执' },
    delivered: { state: 'read',    lk: 'msg.ack.delivered.label', lz: '已读',  tk: 'msg.ack.delivered.title', tz: '对端已回执' },
    failed:    { state: 'failed',  lk: 'msg.ack.failed.label',    lz: '失败',  tk: 'msg.ack.failed.title',    tz: '目标离线，投递失败' },
    expired:   { state: 'failed',  lk: 'msg.ack.expired.label',   lz: '已过期', tk: 'msg.ack.expired.title',   tz: '超过 TTL，记录已清理' },
  };
  function ackBadgeOf(kind) {
    const b = ACK_BADGE[kind];
    if (!b) return null;
    return { state: b.state, label: ts(b.lk, b.lz), title: ts(b.tk, b.tz) };
  }
  function ackStateOf(m) {
    const myId = getMyId();
    if (!myId || m.from !== myId) return null;
    const ack = ackBySeq.get(m.seq);
    if (ack) return ackBadgeOf(ack);
    const routed = routeBySeq.get(m.seq);
    if (!routed) {
      return {
        state: 'sending',
        label: ts('msg.ack.sending.label', '投递中'),
        title: ts('msg.ack.sending.title', '尚未收到路由结果'),
      };
    }
    return routed.delivered > 0 ? ackBadgeOf('pending') : ackBadgeOf('failed');
  }

  // convKeyOf 计算会话分组键：房间按 roomId、广播归一类、私聊按对端（不是我的那一方）。
  function convPeer(m) {
    const myId = getMyId();
    return m.from === myId ? (m.to || m.from) : (m.from || m.to);
  }
  function convKeyOf(m) {
    if (m.roomId) return 'room:' + m.roomId;
    if (m.dir === 'broadcast') return 'broadcast';
    return 'dm:' + convPeer(m);
  }
  function convTitleOf(m) {
    if (m.roomId) return ts('msg.conv.roomPrefix', '房间 · ') + m.roomId;
    if (m.dir === 'broadcast') return ts('msg.conv.broadcast', '全员广播');
    return ts('msg.conv.dmPrefix', '私聊 · ') + (convPeer(m) || ts('msg.conv.unknown', '未知'));
  }
  function msgsOfConv(key) { return inbox.filter((m) => convKeyOf(m) === key); }
  function findMsgBySeq(seq) {
    const want = String(seq);
    return inbox.find((m) => String(m.seq) === want) || null;
  }
  // msgJSON 是一条消息的可复制 JSON 视图（调试场景高频：直接贴给后端同学复现）。
  function msgJSON(m) {
    return {
      type: m.roomId ? 'room' : (m.dir === 'broadcast' ? 'broadcast' : 'chat'),
      from: m.from, to: m.to, roomId: m.roomId,
      text: m.text, ts: m.ts, seq: m.seq,
      dir: m.dir,
    };
  }

  function msgHtml(m) {
    let who;
    if (m.dir === 'out') who = '→ ' + esc(m.to || ts('msg.conv.broadcast', '广播'));
    else if (m.dir === 'broadcast') who = '📢 ' + esc(m.from);
    else if (m.dir === 'room') who = '#' + esc(m.roomId) + ' · ' + esc(m.from);
    else who = esc(m.from) + ' → ' + esc(ts('msg.toMe', '我'));
    const ack = ackStateOf(m);
    // 只有**首次**绘制的消息带入场动画（重绘不应让整列消息重新跳动）。
    const enter = m._painted ? '' : ' msg--enter';
    m._painted = true;
    return '<div class="msg msg--' + m.dir + enter + '" data-msg="' + esc(String(m.seq)) + '">' +
      '<div class="msg__head">' +
        '<span class="msg__who">' + who + '</span>' +
        '<span class="msg__time">' + esc(fmtClock(m.ts)) + '</span>' +
        (ack ? '<span class="msg__ack msg__ack--' + ack.state + '" title="' + esc(ack.title) + '">' +
          esc(ack.label) + '</span>' : '') +
        '<button class="msg__copy" type="button" data-copy-msg="' + esc(String(m.seq)) +
          '" aria-label="' + esc(ts('msg.copy', '复制')) + '">' +
          esc(ts('msg.copy', '复制')) + '</button>' +
      '</div>' +
      '<div class="msg__text">' + esc(m.text || '') + '</div></div>';
  }

  function renderInbox() {
    const list = $('#inbox-list');
    if (!list) return;
    if (!inbox.length) {
      list.innerHTML = '<div class="empty-state">' +
        '<div class="empty-state__icon">✉</div>' +
        '<div class="empty-state__title">' + esc(ts('msg.emptyTitle', '暂无消息')) + '</div>' +
        '<div class="empty-state__text">' + esc(ts('msg.emptyText',
          '发送或接收的消息会按会话分组出现在这里。先在「聊天室」里填写你的 Agent ID，发出的消息会标记为「我发出」并显示投递状态。')) +
        '</div></div>';
      return;
    }

    // 按会话分组；组间按该会话最后一条消息时间倒序，组内最新在最上（新消息从顶部滑入）。
    const order = [];
    const groups = {};
    inbox.forEach((m) => {
      const k = convKeyOf(m);
      if (!groups[k]) { groups[k] = { key: k, title: convTitleOf(m), msgs: [] }; order.push(k); }
      groups[k].msgs.push(m);
    });
    order.sort((a, b) => lastTsOf(groups[b].msgs) - lastTsOf(groups[a].msgs));

    list.innerHTML = order.map((k) => {
      const g = groups[k];
      const msgs = g.msgs.slice().sort((x, y) => (y.ts - x.ts) || (y.seq - x.seq));
      // data-conv：窄屏左右滑切换会话时用它定位当前会话分组（V7 ③）
      return '<section class="conv" data-conv="' + esc(g.key) + '">' +
        '<header class="conv__head">' +
          '<h3 class="conv__title">' + esc(g.title) + '</h3>' +
          '<span class="conv__meta">' + esc(tsp('msg.count', '{n} 条', { n: msgs.length })) + '</span>' +
          '<button class="btn btn--ghost btn--xs" type="button" data-copy-conv="' + esc(g.key) +
            '">' + esc(ts('msg.copyConv', '复制会话 JSON')) + '</button>' +
        '</header>' +
        '<div class="conv__msgs">' + msgs.map(msgHtml).join('') + '</div>' +
      '</section>';
    }).join('');
    // 最新在顶部 → 视口留在顶部，否则用户永远看不到刚来的消息。
    list.scrollTop = 0;
  }

  function lastTsOf(msgs) {
    let max = 0;
    for (let i = 0; i < msgs.length; i++) if ((msgs[i].ts || 0) > max) max = msgs[i].ts || 0;
    return max;
  }

  /* ---------------- 表单：通用校验 + 提交态 ---------------- */
  function wireValidation(form) {
    const btn = form.querySelector('button[type="submit"]');
    const required = $$('[required]', form);
    function update() {
      const valid = required.every((f) => (f.value || '').trim() !== '');
      if (btn) btn.disabled = !valid;
    }
    required.forEach((f) => f.addEventListener('input', update));
    update();
  }

  async function submitJson(btn, path, body, okMsg) {
    setBtnLoading(btn, true);
    const r = await api('POST', path, body);
    setBtnLoading(btn, false);
    if (r.ok) {
      toast(okMsg, 'success');
      loadStatus(true);
      return true;
    }
    toast(ts('common.operationFailed', '操作失败：') + r.error, 'error');
    return false;
  }

  /* =====================================================================
     插件视图（V4 ②）
     数据来源：/api/plugins（含每个插件的状态、依赖、路由归属、启停次数）。
     快照 10s 轮询，操作有 loading 态防重复点击，failed 的错误**必须可见**。
     ===================================================================== */
  const PLUGINS_POLL_MS = 10000;

  // 状态名走 i18n：stateText 每次渲染时解析，切语言重绘即生效。
  const STATE_KEYS = {
    running: ['plugins.state.running', '运行中'],
    stopped: ['plugins.state.stopped', '已停止'],
    disabled: ['plugins.state.disabled', '已停用'],
    failed: ['plugins.state.failed', '失败'],
    initialized: ['plugins.state.initialized', '已装配'],
    registered: ['plugins.state.registered', '已注册'],
  };
  function stateText(state) {
    const p = STATE_KEYS[state];
    return p ? ts(p[0], p[1]) : String(state);
  }
  function stateLabelKnown(state) { return !!STATE_KEYS[state]; }

  function stateBadge(state) {
    const cls = 'plugin-state plugin-state--' + (stateLabelKnown(state) ? state : 'registered');
    return '<span class="' + cls + '"><span class="plugin-state__dot"></span>' +
      esc(stateText(state)) + '</span>';
  }

  let pluginsCache = null;
  let pluginsPollTimer = null;

  async function loadPlugins(silent) {
    const tbody = $('#plugins-tbody');
    if (tbody && !pluginsCache) {
      tbody.innerHTML = '<tr class="skeleton"><td colspan="8"><div class="skeleton-line"></div></td></tr>';
    }
    const r = await api('GET', '/api/plugins');
    if (!r.ok) {
      if (realtime.state !== 'open') setConn('reconnecting');
      const alertBox = $('#plugins-alert');
      if (alertBox) {
        // 区分「服务端不可达」与「插件自身失败」
        alertBox.innerHTML = r.status === 0
          ? '<div class="error-banner">' + esc(ts('plugins.connFailed', '无法连接 Hub（可能未启动或端口不通）：')) +
            esc(r.error) + '</div>'
          : '<div class="error-banner">' + esc(ts('plugins.loadFailedPrefix', '插件列表加载失败：')) +
            esc(r.error) + '</div>';
      }
      return;
    }
    setConn('online');
    if ($('#plugins-alert')) $('#plugins-alert').innerHTML = '';
    pluginsCache = r.data;
    renderPlugins();
    syncPluginCommands(); // V7 ①：命令面板的「启动/停止插件 <name>」跟随最新插件快照
    if (!silent) toast(ts('plugins.refreshed', '插件快照已刷新'), 'info');
  }

  function renderPlugins() {
    const data = pluginsCache;
    const tbody = $('#plugins-tbody');
    if (!data || !tbody) return;
    const list = data.plugins || [];
    const runningList = data.running || [];
    const disabledList = data.disabled || [];
    const failedList = data.failed || [];
    const runningSet = new Set(runningList);

    $('#plugins-count').textContent = data.count || list.length;
    $('#plugins-summary').textContent = tsp('plugins.summaryCounts', '{on} 启用 / {off} 停用 / {failed} 失败', {
      on: runningList.length, off: disabledList.length, failed: failedList.length,
    });

    if (!list.length) {
      tbody.innerHTML = '<tr><td colspan="8"><div class="empty-state">' +
        '<div class="empty-state__icon">▤</div>' +
        '<div class="empty-state__title">' + esc(ts('plugins.emptyTitle', '二进制里没有插件')) + '</div>' +
        '<div class="empty-state__text">' +
          esc(ts('plugins.emptyHint', '检查 cmd/hub/plugins/plugins.go 的功能清单。')) + '</div></div></td></tr>';
      return;
    }

    tbody.innerHTML = list.map((p) => {
      const isOn = (p.state !== 'disabled' && p.state !== 'stopped');
      const missingDeps = (p.dependencies || []).filter((d) => !runningSet.has(d));
      // 停用态开关的可操作性：Required 不可停用；依赖未就绪时不允许启用
      const lockForRequired = p.required && isOn;
      const lockForDeps = !isOn && missingDeps.length > 0;
      const locked = lockForRequired || lockForDeps;

      let title = isOn ? ts('plugins.toggleOff', '停用该插件') : ts('plugins.toggleOn', '启用该插件');
      if (lockForRequired) title = p.requiredReason || ts('plugins.requiredLocked', '必需插件，不可停用');
      if (lockForDeps) title = tsp('plugins.depMissing', '依赖未运行：{deps}', { deps: missingDeps.join('、') });

      const requiredBadge = p.required
        ? ' <span class="badge badge--magenta" title="' + esc(ts('plugins.required', '必需插件')) + '">' +
          esc(ts('plugins.requiredBadge', '必需')) + '</span>'
        : '';
      const depsHtml = (p.dependencies || []).length
        ? p.dependencies.map((d) => '<span class="badge ' + (runningSet.has(d) ? 'badge--accent' : '') + '">' + esc(d) + '</span>').join('')
        : '<span class="muted">—</span>';
      const routesTitle = (p.routes || []).join(' ');
      const routesHtml = '<span class="plugin-num" title="' + esc(routesTitle) + '">' + esc(p.routeCount) + '</span>';

      // data-label：同 Agent 表，供窄屏卡片流补回列名（V7 ③）
      const mainRow = '<tr class="plugin-row' + (p.state === 'failed' ? ' plugin-row--failed' : '') + '">' +
        '<td data-label="' + esc(ts('plugins.th.enabled', '启用')) + '"><label class="switch plugin-toggle' + (locked ? ' is-locked' : '') + '" title="' + esc(title) + '">' +
          '<input type="checkbox" data-toggle="' + esc(p.name) + '"' + (isOn ? ' checked' : '') +
          (locked ? ' disabled' : '') + '>' +
          '<span class="switch__track"><span class="switch__thumb"></span></span>' +
        '</label></td>' +
        '<td data-label="' + esc(ts('plugins.th.name', '名称')) + '"><span class="plugin-name">' +
          esc(p.name) + '</span>' + requiredBadge + '</td>' +
        '<td class="plugin-num" data-label="' + esc(ts('plugins.th.version', '版本')) + '">' + esc(p.version) + '</td>' +
        '<td data-label="' + esc(ts('plugins.th.state', '状态')) + '">' + stateBadge(p.state) + '</td>' +
        '<td data-label="' + esc(ts('plugins.th.usage', '用途')) + '"><div class="plugin-desc" title="' +
          esc(p.description) + '">' + esc(p.description) + '</div></td>' +
        '<td data-label="' + esc(ts('plugins.th.deps', '依赖')) + '"><div class="plugin-deps">' + depsHtml + '</div></td>' +
        '<td data-label="' + esc(ts('plugins.th.routes', '路由')) + '">' + routesHtml + '</td>' +
        '<td class="plugin-num" data-label="' + esc(ts('plugins.th.toggle', '启停')) + '">' + esc(p.restartCount) + '</td>' +
        '</tr>';

      // 失败详情展开行：错误**绝不隐藏**（可运维性的核心）
      const errorRow = p.state === 'failed'
        ? '<tr><td class="plugin-error-cell" colspan="8"><div class="error-banner plugin-error">' +
            '<div><div class="plugin-error__title">' +
              esc(tsp('plugins.failedTitle', '{name} 启动/装配失败', { name: p.name })) + '</div>' +
            '<code>' + esc(p.error || ts('plugins.noErrorInfo', '（无错误信息）')) + '</code>' +
            '<div class="plugin-error__meta">' +
              esc(tsp('plugins.failedMeta', '状态变更于 {ago} · 启停次数 {n}',
                { ago: fmtAgo(p.stateChangedAt), n: p.restartCount })) +
            '</div></div></div></td></tr>'
        : '';
      return mainRow + errorRow;
    }).join('');
  }

  async function togglePlugin(input) {
    const name = input.getAttribute('data-toggle');
    if (!name) return;
    const action = input.checked ? 'enable' : 'disable';
    const label = input.closest('.plugin-toggle');
    if (label) label.classList.add('is-busy');
    input.disabled = true; // loading 态：防重复点击
    const r = await api('POST', '/api/plugins/toggle', { name: name, action: action });
    if (label) label.classList.remove('is-busy');
    if (r.ok) {
      toast(tsp(action === 'enable' ? 'plugins.toggledOn' : 'plugins.toggledOff',
        (action === 'enable' ? '已启用 ' : '已停用 ') + '{name}', { name: name }), 'success');
    } else {
      toast('操作失败：' + r.error, 'error');
      input.checked = !input.checked; // 失败回滚开关
    }
    await loadPlugins(true);
    loadStatus(true); // 让「自动发现」等状态与新快照保持一致
  }

  async function reloadPlugins() {
    const btn = $('#plugins-reload');
    setBtnLoading(btn, true);
    const r = await api('POST', '/api/plugins/reload', {});
    setBtnLoading(btn, false);
    if (!r.ok) {
      toast(ts('plugins.reloadFailed', '重载失败：') + r.error, 'error');
    } else if (r.data && r.data.failed) {
      toast(tsp('plugins.reloadPartial', '重载完成，但有 {n} 个插件失败，请查看错误详情', { n: r.data.failed }), 'error');
    } else {
      toast(ts('plugins.reloadOk', '全部插件已重载（未重启进程）'), 'success');
    }
    await loadPlugins(true);
    loadStatus(true);
  }

  function startPluginsPolling() {
    if (pluginsPollTimer) return;
    pluginsPollTimer = setInterval(() => {
      if (activeView === 'plugins') loadPlugins(true);
    }, PLUGINS_POLL_MS);
  }

  /* =====================================================================
     设置视图（V4 ④）：前端**不写任何插件专属表单代码**，
     完全由 /api/settings 返回的 schema（OptionDef）驱动渲染。
     ===================================================================== */
  const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
  let settingsCache = null;

  async function loadSettings(silent) {
    const form = $('#settings-form');
    const r = await api('GET', '/api/settings');
    if (!r.ok) {
      if ($('#settings-alert')) {
        $('#settings-alert').innerHTML = '<div class="error-banner">设置加载失败：' + esc(r.error) + '</div>';
      }
      if (form) form.innerHTML = '';
      return;
    }
    if ($('#settings-alert')) $('#settings-alert').innerHTML = '';
    settingsCache = r.data;
    renderSettings();
    if (!silent) toast(ts('settings.reloaded', '设置已重新载入'), 'info');
  }

  function findDef(pluginName, key) {
    if (!settingsCache || !settingsCache.schema) return null;
    const defs = settingsCache.schema[pluginName] || [];
    for (let i = 0; i < defs.length; i++) {
      if (defs[i].key === key) return defs[i];
    }
    return null;
  }

  function renderSettings() {
    const data = settingsCache;
    const form = $('#settings-form');
    if (!data || !form) return;

    $('#settings-path').textContent = data.configPath || '（未配置文件，改动仅在内存）';
    $('#settings-version').textContent = 'v' + data.version;

    // 磁盘上残留了本二进制里已不存在的插件段或配置项（降级 / 删插件 / 手工编辑造成）。
    // 它们已被排除在 plugins 之外，这里明确告知"保存一次即会清理"，用户不必手改文件。
    const staleSections = data.unknownPlugins || [];
    const staleKeys = data.staleOptions || [];
    const staleCount = staleSections.length + staleKeys.length;
    const staleBox = $('#settings-stale');
    if (staleBox) {
      if (!staleCount) {
        staleBox.innerHTML = '';
      } else {
        const parts = [];
        if (staleSections.length) {
          parts.push(ts('settings.staleSections', '已不存在的插件段') + ' ' +
            staleSections.map(function (n) { return esc(n); }).join('、'));
        }
        if (staleKeys.length) {
          parts.push(ts('settings.staleKeys', '已废弃的配置项') + ' ' + staleKeys.map(function (o) {
            return esc(o.plugin) + '<code>.' + esc(o.key) + '</code>';
          }).join('、'));
        }
        staleBox.innerHTML = '<div class="settings__hint-invalid">' +
          esc(tsp('settings.staleHint',
            '磁盘配置里有 {n} 个本二进制中已废弃的段/配置项（{detail}），保存一次即会被清理。',
            { n: staleCount, detail: parts.join('；') })) + '</div>';
      }
    }

    const saveBtn = $('#settings-save');
    if (saveBtn && data.readOnly) {
      saveBtn.disabled = true;
      saveBtn.title = ts('settings.readonlyTitle', '只读模式（-config-readonly）下不可保存');
    }

    const schema = data.schema || {};
    const names = Object.keys(schema).sort();
    if (!names.length) {
      form.innerHTML = '<div class="settings__empty">' +
        esc(ts('settings.empty', '当前二进制中的插件都没有声明可配置项。')) + '</div>';
      return;
    }

    const stateByName = {};
    ((pluginsCache && pluginsCache.plugins) || []).forEach((p) => { stateByName[p.name] = p; });

    form.innerHTML = names.map((name) => {
      const defs = schema[name] || [];
      const pc = (data.plugins && data.plugins[name]) || { enabled: true, options: {} };
      const opts = pc.options || {};
      const meta = stateByName[name] || {};
      const state = meta.state || (pc.enabled ? 'running' : 'disabled');
      const usable = (state !== 'disabled' && state !== 'stopped');
      const fields = defs.map((d) => renderField(name, d, opts[d.key], usable)).join('');
      return '<details class="settings__section" data-plugin="' + esc(name) + '" data-enabled="' + (usable ? 'true' : 'false') + '" open>' +
        '<summary class="settings__head">' +
          '<span class="settings__caret" aria-hidden="true">▸</span>' +
          '<span class="settings__name">' + esc(name) + '</span>' +
          stateBadge(state) +
          '<span class="settings__desc">' + esc(meta.description || '') + '</span>' +
        '</summary>' +
        '<div class="settings__body">' + fields +
          '<div class="settings__actions">' +
            '<button class="btn btn--ghost" type="button" data-reset="' + esc(name) + '">' +
              esc(ts('settings.reset', '恢复默认')) + '</button>' +
            (usable ? '' : '<span class="muted">' +
              esc(ts('settings.notRunning', '插件当前未运行，改动会写入配置并需要重启/重载生效。')) + '</span>') +
          '</div>' +
        '</div>' +
      '</details>';
    }).join('');

    bindSettingFields();
  }

  // renderField 按 OptionDef.Type 生成控件：string/int/bool/duration/select 五种全覆盖。
  function renderField(pluginName, def, value, usable) {
    const id = 'opt-' + pluginName + '-' + def.key;
    const current = (value === undefined || value === null) ? def.default : value;
    const dis = usable ? '' : ' disabled';
    const dataAttrs = ' id="' + esc(id) + '" data-opt="' + esc(def.key) + '" data-type="' + esc(def.type) + '"' + dis;
    let control = '';

    switch (def.type) {
      case 'int':
        control = '<input class="input" type="number" step="1"' + dataAttrs +
          (def.min ? ' min="' + esc(def.min) + '"' : '') +
          (def.max ? ' max="' + esc(def.max) + '"' : '') +
          ' value="' + esc(current) + '">';
        break;
      case 'bool':
        control = '<label class="switch settings__switch" title="' + esc(def.description || def.key) + '">' +
          '<input type="checkbox" data-opt="' + esc(def.key) + '" data-type="bool" id="' + esc(id) + '"' +
          (current ? ' checked' : '') + dis + '>' +
          '<span class="switch__track"><span class="switch__thumb"></span></span>' +
          '<span class="switch__label">' +
            (current ? esc(ts('common.on', '开启')) : esc(ts('common.off', '关闭'))) + '</span></label>';
        break;
      case 'duration':
        control = '<input class="input" type="text" placeholder="30s / 1m / 500ms"' + dataAttrs +
          ' value="' + esc(current) + '">';
        break;
      case 'select':
        control = '<select class="select"' + dataAttrs + '>' +
          (def.choices || []).map((c) =>
            '<option value="' + esc(c) + '"' + (c === current ? ' selected' : '') + '>' + esc(c) + '</option>'
          ).join('') + '</select>';
        break;
      default: // string
        control = '<input class="input" type="text"' + dataAttrs + ' value="' + esc(current) + '">';
    }

    return '<div class="settings__field" data-field="' + esc(def.key) + '">' +
      '<div class="settings__label">' +
        '<label class="settings__key" for="' + esc(id) + '">' + esc(def.key) + '</label>' +
        '<span class="settings__default">' + esc(ts('settings.defaultPrefix', '默认 ')) +
          esc(fmtDefault(def.default)) + '</span>' +
        '<span class="settings__modified" hidden>' + esc(ts('settings.modified', '已修改')) + '</span>' +
      '</div>' +
      control +
      (def.description ? '<div class="settings__default">' + esc(def.description) + '</div>' : '') +
      '<div class="settings__hint-invalid" hidden>' + esc(ts('settings.invalidValue', '取值不合法')) + '</div>' +
      '</div>';
  }

  function validateFieldValue(el, def) {
    const type = el.getAttribute('data-type');
    if (type === 'bool') return true;
    if (type === 'select') return (def.choices || []).indexOf(el.value) >= 0;
    if (type === 'int') {
      const raw = String(el.value || '').trim();
      if (raw === '') return false;
      const n = Number(raw);
      if (!Number.isInteger(n)) return false;
      if (def.min && n < def.min) return false;
      if (def.max && n > def.max) return false;
      return true;
    }
    if (type === 'duration') return DURATION_RE.test(String(el.value || '').trim());
    return true;
  }

  function isFieldModified(el, def) {
    const type = el.getAttribute('data-type');
    if (type === 'bool') return !!el.checked !== !!def.default;
    return String(el.value) !== String(def.default == null ? '' : def.default);
  }

  function bindSettingFields() {
    $$('#settings-form [data-opt]').forEach((el) => {
      const handler = () => {
        const section = el.closest('.settings__section');
        const field = el.closest('.settings__field');
        if (!section || !field) return;
        const def = findDef(section.getAttribute('data-plugin'), el.getAttribute('data-opt'));
        if (!def) return;
        const bad = !validateFieldValue(el, def);
        el.classList.toggle('input--invalid', bad);
        const hint = field.querySelector('.settings__hint-invalid');
        if (hint) hint.hidden = !bad;
        const mark = field.querySelector('.settings__modified');
        if (mark) mark.hidden = !isFieldModified(el, def);
        if (el.getAttribute('data-type') === 'bool') {
          const lbl = el.parentNode && el.parentNode.querySelector('.switch__label');
          if (lbl) lbl.textContent = el.checked ? ts('common.on', '开启') : ts('common.off', '关闭');
        }
      };
      el.addEventListener('input', handler);
      el.addEventListener('change', handler);
    });
  }

  function setFieldValue(el, value) {
    const type = el.getAttribute('data-type');
    if (type === 'bool') {
      el.checked = !!value;
      const lbl = el.parentNode && el.parentNode.querySelector('.switch__label');
      if (lbl) lbl.textContent = el.checked ? '开启' : '关闭';
    } else {
      el.value = value == null ? '' : String(value);
    }
    el.dispatchEvent(new Event('change'));
  }

  // collectSettings 把表单值收集成 POST /api/settings 的载荷；
  // enabled 沿用服务端当前值（启停请用「插件」视图的开关，避免两个入口互相打架）。
  function collectSettings() {
    const data = settingsCache;
    const plugins = {};
    Object.keys(data.plugins || {}).forEach((name) => {
      const pc = data.plugins[name] || {};
      plugins[name] = { enabled: !!pc.enabled, options: Object.assign({}, pc.options || {}) };
    });
    Object.keys(data.schema || {}).forEach((name) => {
      if (!plugins[name]) plugins[name] = { enabled: true, options: {} };
    });

    let invalid = null;
    $$('#settings-form [data-opt]').forEach((el) => {
      const section = el.closest('.settings__section');
      if (!section) return;
      const name = section.getAttribute('data-plugin');
      const key = el.getAttribute('data-opt');
      const def = findDef(name, key);
      if (!def) return;
      if (!validateFieldValue(el, def)) {
        if (!invalid) invalid = name + '.' + key;
        return;
      }
      const type = el.getAttribute('data-type');
      let v;
      if (type === 'bool') v = !!el.checked;
      else if (type === 'int') v = Number(String(el.value).trim());
      else v = String(el.value).trim();
      if (!plugins[name]) plugins[name] = { enabled: true, options: {} };
      plugins[name].options[key] = v;
    });
    return { invalid: invalid, payload: { version: data.version, plugins: plugins } };
  }

  async function saveSettings() {
    if (!settingsCache) return;
    const btn = $('#settings-save');
    const collected = collectSettings();
    if (collected.invalid) {
      toast(tsp('settings.invalidField', '配置项 {key} 的取值不合法，请先修正', { key: collected.invalid }), 'error');
      return;
    }
    setBtnLoading(btn, true);
    const r = await api('POST', '/api/settings', collected.payload);
    setBtnLoading(btn, false);

    if (r.ok) {
      // 成功响应里 unknownPlugins / staleOptions 必然为空（保存时已清理），
      // 一并写回缓存，让 renderSettings 立刻清掉「保存一次即会被清理」的提示（N4）。
      settingsCache = Object.assign({}, settingsCache, {
        version: r.data.version,
        plugins: r.data.plugins || settingsCache.plugins,
        unknownPlugins: r.data.unknownPlugins || [],
        staleOptions: r.data.staleOptions || [],
      });
      $('#settings-version').textContent = 'v' + r.data.version;
      const pending = r.data.pendingRestart || [];
      if (pending.length) {
        $('#settings-alert').innerHTML = '<div class="error-banner">已保存（v' + esc(r.data.version) +
          '），但以下插件的改动需要重启或「全部重载」才生效：<code>' + esc(pending.join('、')) + '</code></div>';
        toast(tsp('settings.savedPendingToast', '已保存 v{version}；待生效：', { version: r.data.version }) +
          pending.join('、'), 'info');
      } else {
        $('#settings-alert').innerHTML = '';
        toast(tsp('settings.savedOk', '设置已保存并生效（v{version}）', { version: r.data.version }), 'success');
      }
      renderSettings();
      loadStatus(true);
      return;
    }

    if (r.status === 409) {
      const d = r.data || {};
      if ($('#settings-alert')) {
        $('#settings-alert').innerHTML = '<div class="error-banner">' +
          esc(tsp('settings.conflict', '他人已修改配置（磁盘版本 v{version}），你的改动未保存。', { version: d.version })) +
          '<button class="btn btn--ghost" id="settings-view-latest" type="button">' +
          esc(ts('settings.viewLatest', '查看最新值')) + '</button></div>';
        const latest = $('#settings-view-latest');
        if (latest) latest.addEventListener('click', () => loadSettings(false));
      }
      toast(ts('settings.conflictToast', '保存冲突：配置已被他人修改（409），你的改动未保存'), 'error');
      return;
    }

    if ($('#settings-alert')) {
      $('#settings-alert').innerHTML = '<div class="error-banner">保存失败：' + esc(r.error) + '</div>';
    }
    toast(ts('common.saveFailed', '保存失败：') + r.error, 'error');
  }

  function exportSettings() {
    if (!settingsCache) return;
    const payload = { version: settingsCache.version, plugins: settingsCache.plugins };
    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'hub.json';
    document.body.appendChild(a);
    a.click();
    setTimeout(() => { URL.revokeObjectURL(url); a.remove(); }, 0);
    toast(ts('settings.exported', '已导出当前配置副本'), 'success');
  }

  /* =====================================================================
     V5 ① 实时通道（SSE）：顶栏三态药丸 + 增量更新 + 全量对账 + 自动降级
     ---------------------------------------------------------------------
     设计要点（对应 V5 验收 1/2/10）：
     - 用浏览器原生 EventSource，零依赖；断线重连由浏览器负责；
     - 重连成功（open）后**再拉一次全量快照**对账：增量 + 全量兜底，不追求强一致；
     - 连续失败或环境不支持 EventSource → 自动降级为 5 秒轮询，并在药丸上明说；
     - 增量只消费事件里的最小字段（如 agentId），明细字段靠一次防抖全量补齐，
       避免"事件载荷随界面需求膨胀"。
     ===================================================================== */
  const SSE_TOPICS = [
    'agent.online',   // Agent 上线（表格行即时插入）
    'agent.offline',  // Agent 下线（表格行即时移除）
    'room.changed',   // 房间变化（房间卡片刷新）
    'plugin.state',   // 插件启停（插件/设置视图徽标更新）
    'stats.tick',     // 在线数/房间数低频兜底（概览数字滚动）
    'msg.content',    // 已路由消息正文（供会话视图实时接收）
    // V7 ①：事件流视图要渲染 msg.routed。注：服务端 realtime 目前未订阅该 topic
    // （V5 注释明确它"频率高、对界面无意义，仅用于 metrics 计数"），
    // 因此本版本它在 SSE 上收不到；前端的接收/渲染/过滤链路已完整就绪，
    // 后端补一次 SubscribeAsync(protocol.TopicMsgRouted, ...) 即可点亮，无需改前端。
    'msg.routed',
  ].join(',');

  const realtime = {
    es: null,
    state: 'idle',      // idle | connecting | open | polling
    failures: 0,
    pollTimer: null,
    degradedNotified: false,
  };
  const MAX_SSE_FAILURES = 3;

  function startRealtime() {
    if (typeof EventSource === 'undefined') {
      degradeToPolling(ts('common.eventSourceUnsupported', '当前浏览器不支持 EventSource'));
      return;
    }
    stopPolling();
    realtime.state = 'connecting';
    setConn('connecting');

    let es;
    try {
      es = new EventSource('/api/stream?topics=' + encodeURIComponent(SSE_TOPICS));
    } catch (e) {
      degradeToPolling(ts('common.eventSourceFailed', '无法创建 EventSource：') + (e.message || e));
      return;
    }
    realtime.es = es;

    es.addEventListener('open', () => {
      realtime.failures = 0;
      realtime.state = 'open';
      setConn('online');
      // 全量对账：断线期间可能漏事件，重连后拉一次快照把状态拉回真实值。
      resyncAll();
    });

    es.addEventListener('error', () => {
      if (realtime.state === 'open') {
        // 已经连上后断开：浏览器会自动重连，先如实显示"重连中"。
        realtime.state = 'connecting';
        setConn('connecting');
      }
      realtime.failures++;
      if (realtime.failures >= MAX_SSE_FAILURES || es.readyState === 2 /* CLOSED */) {
        degradeToPolling(ts('common.realtimeFailed', '实时连接连续失败'));
      }
    });

    es.addEventListener('agent.online', (e) => applyAgentEvent(e, true));
    es.addEventListener('agent.offline', (e) => applyAgentEvent(e, false));
    es.addEventListener('room.changed', applyRoomEvent);
    es.addEventListener('plugin.state', applyPluginStateEvent);
    es.addEventListener('stats.tick', applyStatsTick);
    es.addEventListener('msg.content', applyMsgContent);
    // V7 ①：msg.routed —— 事件流渲染 + 时间线投递状态（按 seq 与 msg.content 关联）。
    es.addEventListener('msg.routed', applyMsgRouted);
  }

  // resyncAll 重连后对账：把所有列表拉一遍（不阻塞界面，失败静默）。
  function resyncAll() {
    loadStatus(true);
    if (activeView === 'agents') loadAgents();
    if (activeView === 'rooms') loadRooms();
    if (activeView === 'plugins' || activeView === 'settings') loadPlugins(true);
    if (activeView === 'overview') loadMetrics(true);
  }

  // degradeToPolling 降级为 5 秒轮询（并在药丸上说明原因，不静默降级）。
  function degradeToPolling(reason) {
    if (realtime.state === 'polling') return;
    if (realtime.es) {
      try { realtime.es.close(); } catch (_) { /* 忽略 */ }
      realtime.es = null;
    }
    realtime.state = 'polling';
    setConn('polling');
    connPill.title = tsp('common.degraded5s', '实时连接不可用（{reason}），已切换为每 5 秒轮询', { reason: reason });
    if (!realtime.degradedNotified) {
      realtime.degradedNotified = true;
      toast(tsp('common.degraded', '实时连接不可用（{reason}），已切换为 5 秒轮询', { reason: reason }), 'info');
    }
    startPolling();
  }

  function startPolling() {
    stopPolling();
    realtime.pollTimer = setInterval(() => {
      loadStatus(true);
      if (activeView === 'agents') loadAgents();
      if (activeView === 'rooms') loadRooms();
      if (activeView === 'plugins') loadPlugins(true);
      if (activeView === 'overview') loadMetrics(true);
    }, 5000);
  }

  function stopPolling() {
    if (realtime.pollTimer) {
      clearInterval(realtime.pollTimer);
      realtime.pollTimer = null;
    }
  }

  // ---- 增量应用（全部只改缓存 + 局部重渲染，不整页刷新） ----

  // applyAgentEvent 处理 agent.online / agent.offline（载荷是 agentId 字符串）。
  function applyAgentEvent(e, online) {
    let id = null;
    try { id = JSON.parse(e.data); } catch (_) { return; }
    if (typeof id !== 'string' || !id) return;
    pushEvent(online ? 'agent.online' : 'agent.offline', id);

    if (online) {
      if (!agentsCache.some((a) => a.id === id)) {
        agentsCache.push({ id: id, name: '' });
        if (activeView === 'agents') renderAgents();
        // 事件只带 id：明细（name/caps/proto）靠一次防抖全量补齐。
        scheduleAgentsResync();
      }
      if (lastStatus) {
        lastStatus.agentCount = agentsCache.length;
        requestRender('stats');
      }
    } else {
      const before = agentsCache.length;
      agentsCache = agentsCache.filter((a) => a.id !== id);
      if (agentsCache.length !== before && activeView === 'agents') renderAgents();
      if (lastStatus) {
        lastStatus.agentCount = agentsCache.length;
        requestRender('stats');
      }
    }
  }

  let agentsResyncTimer = null;
  function scheduleAgentsResync() {
    if (agentsResyncTimer) return;
    agentsResyncTimer = setTimeout(() => {
      agentsResyncTimer = null;
      loadAgents();
    }, 400);
  }

  // applyRoomEvent 处理 room.changed（载荷是完整房间快照，幂等）。
  function applyRoomEvent(e) {
    let p = null;
    try { p = JSON.parse(e.data); } catch (_) { return; }
    const room = p && p.room;
    if (!room || !room.id) return;
    pushEvent('room.changed', p);
    const idx = roomsCache.findIndex((r) => r.id === room.id);
    if (idx >= 0) roomsCache[idx] = room;
    else roomsCache.push(room);
    roomsLoaded = true;
    if (activeView === 'rooms') renderRooms(roomsCache);
    if (lastStatus) {
      lastStatus.roomCount = roomsCache.length;
      requestRender('stats');
    }
  }

  // applyPluginStateEvent 处理 plugin.state（插件徽标即时更新）。
  function applyPluginStateEvent(e) {
    let p = null;
    try { p = JSON.parse(e.data); } catch (_) { return; }
    if (!p || !p.name || !pluginsCache) return;
    pushEvent('plugin.state', p);
    const list = pluginsCache.plugins || [];
    const row = list.find((x) => x.name === p.name);
    if (!row) {
      // 未知插件（例如本视图负载是旧快照）：拉一次全量。
      loadPlugins(true);
      return;
    }
    row.state = p.state;
    row.error = p.error || '';
    row.stateChangedAt = p.at || row.stateChangedAt;
    recomputePluginLists();
    if (activeView === 'plugins') renderPlugins();
    if (activeView === 'settings') renderSettings();
  }

  // recomputePluginLists 由插件数组重算 running/disabled/failed 三个派生列表。
  function recomputePluginLists() {
    if (!pluginsCache) return;
    const running = [];
    const disabled = [];
    const failed = [];
    (pluginsCache.plugins || []).forEach((p) => {
      if (p.state === 'disabled' || p.state === 'stopped') disabled.push(p.name);
      else {
        running.push(p.name);
        if (p.state === 'failed') failed.push(p.name);
      }
    });
    pluginsCache.running = running;
    pluginsCache.disabled = disabled;
    pluginsCache.failed = failed;
  }

  // applyStatsTick 处理 stats.tick（在线数/房间数的低频兜底）。
  function applyStatsTick(e) {
    let p = null;
    try { p = JSON.parse(e.data); } catch (_) { return; }

    // 消息速率采样：以「两次 stats.tick 之间的实际间隔」为窗口，换算成条/秒。
    // 这样与服务端 statsInterval 配置无关（间隔自测量），也不会写死 5 秒。
    const now = Date.now();
    let rate = 0;
    if (lastTickTs) {
      const dt = (now - lastTickTs) / 1000;
      if (dt > 0) rate = msgWindow / dt;
    }
    lastTickTs = now;
    msgWindow = 0;
    msgRateSamples.push(rate);
    if (msgRateSamples.length > 15) msgRateSamples.shift();

    if (!p || !lastStatus) return;
    if (typeof p.agentCount === 'number') lastStatus.agentCount = p.agentCount;
    if (typeof p.roomCount === 'number') lastStatus.roomCount = p.roomCount;
    requestRender('stats');
    requestRender('trend');
  }

  /* =====================================================================
     V5 ③ 概览指标卡与趋势（原生 SVG + CSS，零图表库）
     ===================================================================== */
  async function loadMetrics(silent) {
    const r = await api('GET', '/api/metrics/summary');
    if (!r.ok) {
      if (!silent) toast(ts('metrics.loadFailed', '指标加载失败：') + r.error, 'error');
      return;
    }
    lastMetrics = r.data;
    requestRender('metrics');
  }

  function fmtDuration(ms) {
    if (!ms || ms < 0) return '—';
    const s = Math.floor(ms / 1000);
    if (s < 60) return tsp('metrics.durSeconds', '{n} 秒', { n: s });
    if (s < 3600) {
      return tsp('metrics.durMinutes', '{m} 分 {n} 秒', { m: Math.floor(s / 60), n: s % 60 });
    }
    return tsp('metrics.durHours', '{h} 时 {m} 分', {
      h: Math.floor(s / 3600), m: Math.floor((s % 3600) / 60),
    });
  }

  function renderMetrics(m) {
    const grid = $('#metrics-cards');
    if (!grid || !m) return;
    lastMetrics = m; // 冻结解除后要能原样重绘，故留住最近一次快照

    const cards = [
      {
        label: ts('metrics.agentsOnline.label', '在线 Agent'),
        value: m.agentsOnline,
        sub: ts('metrics.agentsOnline.sub', 'hub_agents_online'),
      },
      {
        label: ts('metrics.rooms.label', '房间数'),
        value: m.rooms,
        sub: ts('metrics.rooms.sub', 'hub_rooms'),
      },
      {
        label: ts('metrics.messagesTotal.label', '累计消息'),
        value: m.messagesTotal,
        sub: ts('metrics.messagesTotal.sub', 'hub_messages_total'),
      },
      {
        label: ts('metrics.msgRate.label', '消息速率'),
        value: (m.messagesPerSecond || 0).toFixed(2),
        sub: ts('metrics.msgRate.sub', '条 / 秒（最近窗口）'),
      },
      {
        label: ts('metrics.sseClients.label', 'SSE 连接'),
        value: m.sseClients,
        sub: ts('metrics.sseClients.sub', 'hub_sse_clients'),
      },
      {
        label: ts('metrics.uptime.label', '运行时长'),
        value: fmtDuration(m.uptimeMs),
        sub: ts('metrics.uptime.sub', 'metrics 插件'),
      },
    ];
    grid.innerHTML = cards.map((c) =>
      '<div class="metric-card">' +
        '<div class="metric-card__label">' + esc(c.label) + '</div>' +
        '<div class="metric-card__value">' + esc(c.value) + '</div>' +
        '<div class="metric-card__sub">' + esc(c.sub) + '</div>' +
      '</div>'
    ).join('');

    const updated = $('#metrics-updated');
    if (updated) {
      updated.textContent = tsp('metrics.updated', '采样间隔 {n} 秒',
        { n: Math.round((m.intervalMs || 0) / 1000) });
    }

    lastSamples = m.samples || [];
    renderTrend(lastSamples, msgRateSamples);
  }

  /* ---------- SVG 折线的公共坐标算法（概览与 V8f 性能视图共用） ----------
     抽出来的唯一原因：性能视图要画两条同规格曲线，照抄两份"几乎一样"的实现
     必然会漂移（改了一处忘另一处）。这里只提取**纯几何**，概览的标记顺序、
     class、归一化规则一概不动 —— overview 曲线的外观必须零变化。 */
  const TREND_W = 600, TREND_H = 120, TREND_PAD = 6;

  // seriesY 把数值映射为 Y：大值在上（SVG 原点在左上）。
  function seriesY(v, maxV) {
    return TREND_H - TREND_PAD - (v / maxV) * (TREND_H - TREND_PAD * 2);
  }

  // seriesPoints 把数值序列映射为坐标点；长度 1 时 step=0（单点退化，不除零）。
  function seriesPoints(values, maxV) {
    const step = values.length > 1 ? (TREND_W - TREND_PAD * 2) / (values.length - 1) : 0;
    return values.map((v, i) => [TREND_PAD + i * step, seriesY(v, maxV)]);
  }

  // pointsAttr 生成 polyline 的 points 属性（保留 1 位小数，避免长浮点撑大 DOM）。
  function pointsAttr(pts) {
    return pts.map((p) => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' ');
  }

  // areaPath 生成「折线 + 底边闭合」的面积路径（与概览面积完全同形）。
  function areaPath(pts) {
    if (!pts.length) return '';
    const base = TREND_H - TREND_PAD;
    return 'M' + pts[0][0].toFixed(1) + ',' + base +
      ' L' + pts.map((p) => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' L') +
      ' L' + pts[pts.length - 1][0].toFixed(1) + ',' + base + ' Z';
  }

  // dotsCircle 生成点标记；radius 由调用方给（概览主序列 2.5，速率线 2）。
  function dotsCircle(pts, radius) {
    return pts.map((p) =>
      '<circle cx="' + p[0].toFixed(1) + '" cy="' + p[1].toFixed(1) + '" r="' + radius + '" />').join('');
  }

  // trendGridLines 生成 0 / 50% / 100% 三条水平虚线。
  function trendGridLines() {
    return [0, 0.5, 1].map((f) =>
      '<line x1="0" x2="' + TREND_W + '" y1="' + (TREND_PAD + f * (TREND_H - TREND_PAD * 2)).toFixed(1) +
      '" y2="' + (TREND_PAD + f * (TREND_H - TREND_PAD * 2)).toFixed(1) + '" />').join('');
  }

  // seriesMax 取序列最大值作为归一化基准（下限 1，避免全 0 时除零）；
  // extraMax 让调用方把"参考线"也纳入同一 Y 轴（例如内存告警阈值）。
  function seriesMax(values, extraMax) {
    const list = values && values.length ? values : [0];
    let maxV = Math.max.apply(null, list);
    if (extraMax && extraMax > maxV) maxV = extraMax;
    return Math.max(1, maxV);
  }

  // renderTrend 用原生 SVG 画折线 + 面积（不引图表库）。
  // 两条线：在线 Agent 数（服务端采样）+ 消息速率（客户端按窗口计数，单位 条/秒）。
  function renderTrend(samples, msgRates) {
    const svg = $('#trend-svg');
    const axis = $('#trend-axis');
    if (!svg) return;
    msgRates = msgRates || [];
    if (!samples.length && !msgRates.length) {
      svg.innerHTML = '';
      if (axis) axis.innerHTML = '<span>' + esc(ts('metrics.noSamples', '暂无采样点')) + '</span>';
      return;
    }

    const values = samples.map((s) => (s.agentsOnline || 0));
    const maxV = seriesMax(values);
    const pts = seriesPoints(values, maxV);
    const line = pointsAttr(pts);
    const area = areaPath(pts);

    // 第二条线：消息速率（条/秒），按自己的最大值归一化，独立 Y 轴。
    let rateLine = '';
    if (msgRates.length) {
      // 下限取 0.01（而不是 1）：速率常年很小，按 1 归一化会把曲线压成一条直线。
      const maxR = Math.max(0.01, Math.max.apply(null, msgRates));
      const rPts = seriesPoints(msgRates, maxR);
      rateLine = '<polyline class="trend__line trend__line--rate" points="' +
        pointsAttr(rPts) + '" />' +
        '<g class="trend__dots trend__dots--rate">' + dotsCircle(rPts, 2) + '</g>';
    }

    svg.innerHTML =
      '<g class="trend__grid">' + trendGridLines() + '</g>' +
      (area ? '<path class="trend__area" d="' + area + '" />' : '') +
      '<polyline class="trend__line" points="' + line + '" />' +
      '<g class="trend__dots">' + dotsCircle(pts, 2.5) + '</g>' +
      rateLine;

    if (axis) {
      const first = samples[0] || {};
      const last = samples[samples.length - 1] || {};
      const lastRate = msgRates.length ? msgRates[msgRates.length - 1] : null;
      axis.innerHTML =
        '<span>' + esc(fmtClock(first.ts)) + '</span>' +
        '<span>' + esc(tsp('metrics.peak', '在线峰值 {n}', { n: maxV })) + '</span>' +
        '<span>' + esc(lastRate === null
          ? ts('metrics.rateNone', '消息速率 —')
          : tsp('metrics.rate', '消息速率 {n} 条/秒', { n: lastRate.toFixed(2) })) + '</span>' +
        '<span>' + esc(fmtClock(last.ts)) + '</span>';
    }
  }

  function fmtClock(ms) {
    if (!ms) return '—';
    const d = new Date(ms);
    return ('0' + d.getHours()).slice(-2) + ':' + ('0' + d.getMinutes()).slice(-2) +
      ':' + ('0' + d.getSeconds()).slice(-2);
  }

  /* =====================================================================
     V8f 性能视图（吞吐 / 丢弃 / 内存的同一份 summary 视图）
     ---------------------------------------------------------------------
     数据只来自既有的 GET /api/metrics/summary（V8 起追加了丢弃与内存字段），
     不新增端点、不引图表库：曲线复用 renderTrend 的坐标算法（seriesPoints 等）。
     采样策略：视图可见时每 2 秒拉一次，本地保留 120 点（≈4 分钟窗口）；
     离开视图立刻停表 —— 后台标签页持续打接口本身就是一种性能问题。
     ===================================================================== */
  const PERF_POLL_MS = 2000;   // 采样周期：与「每 2 秒」的对外承诺一致
  const PERF_WIN_CAP = 120;    // 滑动窗口上限：120 点 × 2 秒 ≈ 4 分钟

  const perfState = {
    timer: null,                 // 轮询定时器（null = 未轮询）
    win: [],                     // 本地窗口：{ ts, rate, heap, gor }
    last: null,                  // 最近一次 summary 原始响应
    prev: null,                  // 上一次 summary（算「较上次」增量用）
    error: '',                   // 最近一次失败原因（空 = 正常）
    live: { drops: false, mem: false }, // a11y 播报去重：只在状态跃迁时说话
  };

  // perfNum 把任意输入收敛成有限数字：字段缺失（后端尚未升级）时一律当 0，
  // 否则 NaN 会一路渗进 SVG 坐标，把整条曲线画成空白。
  function perfNum(v) {
    const n = typeof v === 'number' ? v : Number(v);
    return isFinite(n) ? n : 0;
  }

  // perfModel 是**纯函数**：把 summary 响应收敛成视图用的模型（含告警判定）。
  // 之所以抽出来：console-perf-sim.cjs 可以不碰 DOM 就验证判定与格式化逻辑。
  function perfModel(raw) {
    const m = raw || {};
    const deliverDropped = perfNum(m.deliverDropped);
    const deliverClosed = perfNum(m.deliverClosed);
    const memLimit = perfNum(m.memLimitBytes);
    const heapAlloc = perfNum(m.heapAllocBytes);
    return {
      rate: perfNum(m.messagesPerSecond),
      deliverDropped: deliverDropped,
      deliverClosed: deliverClosed,
      // 「丢弃总数」= 队列满 + 连接已关闭：用户关心的是"有多少消息没能送到"，
      // 两者的拆分放在 sub / 明细表里，KPI 上只给一个总数。
      dropsTotal: deliverDropped + deliverClosed,
      goroutines: perfNum(m.goroutines),
      heapAlloc: heapAlloc,
      heapInuse: perfNum(m.heapInuseBytes),
      gcTotal: perfNum(m.gcTotal),
      sseClients: perfNum(m.sseClients),
      eventsDropped: perfNum(m.eventsDropped),
      memLimit: memLimit,
      memHigh: !!m.memHigh,
      memHighTotal: perfNum(m.memHighTotal),
      // 占用率只在启用阈值时有意义：0 = 未启用，既不能算 0% 也不能算无穷大。
      heapPct: memLimit > 0 ? Math.round((heapAlloc / memLimit) * 100) : null,
      warnDrops: deliverDropped + deliverClosed > 0,
      warnMem: !!m.memHigh,
    };
  }

  // fmtBytes 用 1024 进制换算成人读的字节数；数字部分走 Intl（随语言变千分位/小数点）。
  function fmtBytes(n) {
    const v0 = perfNum(n);
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let v = Math.abs(v0);
    let i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i += 1; }
    let s;
    try {
      s = new Intl.NumberFormat(i18n.lang, { maximumFractionDigits: i === 0 ? 0 : 1 }).format(v);
    } catch (_) {
      s = i === 0 ? String(Math.round(v)) : v.toFixed(1);
    }
    return (v0 < 0 ? '-' : '') + s + ' ' + units[i];
  }

  // fmtRate 统一速率文案（两位小数）：KPI、曲线 aria-label、坐标轴共用，避免各处精度不一。
  function fmtRate(v) {
    return perfNum(v).toFixed(2);
  }

  // perfPushPoint 往本地窗口追加一个采样点。速率取服务端窗口值，堆与 goroutine
  // 与 /metrics 的 gauge **同源同刻**（同一份 summary），因此曲线末端与当前值永远对得上。
  function perfPushPoint(m) {
    perfState.win.push({
      ts: Date.now(),
      rate: perfNum(m.messagesPerSecond),
      heap: perfNum(m.heapAllocBytes),
      gor: perfNum(m.goroutines),
    });
    // 定长淘汰：长时间挂机会把数组撑成无界内存，这正是本视图要防的问题本身。
    if (perfState.win.length > PERF_WIN_CAP) perfState.win.shift();
  }

  async function loadPerf(silent) {
    const r = await api('GET', '/api/metrics/summary');
    if (!r.ok) {
      perfState.error = r.error || ts('common.networkError', '网络错误');
      if (!silent) toast(ts('perf.loadFailed', '性能数据加载失败：') + perfState.error, 'error');
      if (activeView === 'perf') renderPerfAlert();
      return;
    }
    perfState.error = '';
    perfState.prev = perfState.last;
    perfState.last = r.data || {};
    perfPushPoint(perfState.last);
    requestRender('perf');
  }

  // perfStartPolling / perfStopPolling 是性能视图轮询的唯一开关。
  // 定时器回调里再判一次 activeView：即便某条路径漏了 stop，也绝不会继续发请求。
  function perfStartPolling() {
    if (perfState.timer) return;
    perfState.timer = setInterval(function () {
      if (activeView !== 'perf') { perfStopPolling(); return; }
      if (document && document.hidden) return; // 标签页在后台：不采样（省 CPU 也省服务端）
      loadPerf(true);
    }, PERF_POLL_MS);
  }

  function perfStopPolling() {
    if (!perfState.timer) return;
    clearInterval(perfState.timer);
    perfState.timer = null;
  }

  // perfTrendText 把增量渲染成「符号 + 数值」：符号不是装饰，是**不依赖颜色**的方向信号
  // （色盲用户同样能读出升降），读屏也会把 ▲/▼ 读成"上升/下降"。
  function perfTrendText(delta, formatted) {
    if (!isFinite(delta) || Math.abs(delta) < 1e-9) return '● ' + ts('perf.trend.flat', '持平');
    return (delta > 0 ? '▲ ' : '▼ ') + formatted;
  }

  // perfKpiCard 生成一张 KPI 卡：复用概览 .metric-card 的视觉，告警时加 .is-warn。
  function perfKpiCard(o) {
    return '<div class="metric-card' + (o.warn ? ' is-warn' : '') + '"' +
      (o.warn ? ' data-warn="1"' : '') + '>' +
      '<div class="metric-card__label">' + esc(o.label) + '</div>' +
      '<div class="metric-card__value">' + esc(o.value) + '</div>' +
      '<div class="metric-card__sub">' + esc(o.sub) + '</div>' +
    '</div>';
  }

  function renderPerfKpis(model) {
    const host = $('#perf-kpis');
    if (!host) return;
    const win = perfState.win;
    const lastPt = win.length ? win[win.length - 1] : null;
    const prevPt = win.length > 1 ? win[win.length - 2] : null;
    // 增量优先用本地窗口的两点（同口径、同间隔），窗口不足时退化为 0（显示"持平"）。
    const rateDelta = (lastPt && prevPt) ? (lastPt.rate - prevPt.rate) : 0;
    const gorNow = model.goroutines;
    const gorDelta = (lastPt && prevPt) ? (lastPt.gor - prevPt.gor) : 0;

    const dropsSub = model.warnDrops
      ? '⚠ ' + ts('perf.flag.warn', '告警') + ' · ' +
        tsp('perf.kpi.drops.sub', '队列满 {queued} · 已关闭 {closed}',
          { queued: fmtInt(model.deliverDropped), closed: fmtInt(model.deliverClosed) })
      : '● ' + ts('perf.kpi.drops.ok', '无丢弃');

    const heapSub = model.memLimit > 0
      ? tsp('perf.kpi.heap.pct', '占告警阈值 {pct}%', { pct: fmtInt(model.heapPct == null ? 0 : model.heapPct) }) +
        (model.warnMem ? ' · ⚠ ' + ts('perf.flag.high', '超阈值') : ' · ● ' + ts('perf.flag.normal', '正常'))
      : '● ' + ts('perf.kpi.heap.noLimit', '未启用告警阈值');

    const cards = [
      perfKpiCard({
        label: ts('perf.kpi.rate.label', '当前速率'),
        value: fmtRate(model.rate),
        sub: perfTrendText(rateDelta, tsp('perf.delta.rate', '较上次 {n}', { n: fmtRate(Math.abs(rateDelta)) })) +
          ' · ' + ts('perf.unit.perSec', '条/秒'),
        warn: false,
      }),
      perfKpiCard({
        label: ts('perf.kpi.drops.label', '丢弃总数'),
        value: fmtInt(model.dropsTotal),
        sub: dropsSub,
        warn: model.warnDrops,
      }),
      perfKpiCard({
        label: ts('perf.kpi.goroutines.label', 'goroutine'),
        value: fmtInt(gorNow),
        sub: perfTrendText(gorDelta, tsp('perf.delta.goroutines', '较上次 {n}', { n: fmtInt(Math.abs(gorDelta)) })),
        warn: false,
      }),
      perfKpiCard({
        label: ts('perf.kpi.heap.label', '堆占用'),
        value: fmtBytes(model.heapAlloc),
        sub: heapSub,
        warn: model.warnMem,
      }),
    ];
    host.innerHTML = cards.join('');
  }

  // paintSeries 把一条序列画进给定 SVG。标记顺序、class 与概览曲线完全一致，
  // 因此两块曲线的观感同源（不另起一套视觉语言）。refValue > 0 时画一条虚线参考线。
  function paintSeries(svg, values, maxV, refValue) {
    if (!svg) return;
    const pts = seriesPoints(values, maxV);
    let ref = '';
    if (perfNum(refValue) > 0) {
      const y = seriesY(perfNum(refValue), maxV);
      // 参考线必须在同一 Y 轴上：否则「堆一路逼近阈值」看起来仍贴着底部。
      ref = '<line class="trend__ref" x1="0" x2="' + TREND_W + '" y1="' + y.toFixed(1) +
        '" y2="' + y.toFixed(1) + '" />' +
        '<text class="trend__ref-label" x="4" y="' + (y - 4).toFixed(1) + '">' +
        esc(ts('perf.trend.refLimit', '告警阈值')) + '</text>';
    }
    svg.innerHTML = '<g class="trend__grid">' + trendGridLines() + '</g>' +
      (pts.length ? '<path class="trend__area" d="' + areaPath(pts) + '" />' : '') +
      (pts.length ? '<polyline class="trend__line" points="' + pointsAttr(pts) + '" />' : '') +
      '<g class="trend__dots">' + dotsCircle(pts, 2.5) + '</g>' +
      ref;
  }

  // setSvgLabel 给 SVG 写 role/aria-label：曲线是数据表达而不是装饰，
  // 读屏必须能听到"从多少到多少、几个点"这一句，而不是被跳过。
  function setSvgLabel(el, text) {
    if (!el || typeof el.setAttribute !== 'function') return;
    el.setAttribute('role', 'img');
    el.setAttribute('aria-label', text);
  }

  function renderPerfCharts(model) {
    const win = perfState.win;
    const rates = win.map((p) => p.rate);
    const heaps = win.map((p) => p.heap);

    const rateSvg = $('#perf-rate-svg');
    const rateMax = seriesMax(rates);
    paintSeries(rateSvg, rates, rateMax, 0);
    if (!rates.length) {
      setSvgLabel(rateSvg, ts('perf.trend.aria.empty', '曲线：暂无采样点'));
    } else {
      setSvgLabel(rateSvg, tsp('perf.trend.aria.rate',
        '消息速率曲线：最近 {n} 个采样点，从 {from} 条/秒 到 {to} 条/秒',
        { n: rates.length, from: fmtRate(rates[0]), to: fmtRate(rates[rates.length - 1]) }));
    }

    const heapSvg = $('#perf-heap-svg');
    // 阈值也纳入归一化基准：参考线不能被裁掉到画布外。
    const heapMax = seriesMax(heaps, model.memLimit);
    paintSeries(heapSvg, heaps, heapMax, model.memLimit);
    if (!heaps.length) {
      setSvgLabel(heapSvg, ts('perf.trend.aria.empty', '曲线：暂无采样点'));
    } else {
      setSvgLabel(heapSvg, tsp('perf.trend.aria.heap',
        '堆占用曲线：最近 {n} 个采样点，从 {from} 到 {to}{limit}',
        {
          n: heaps.length,
          from: fmtBytes(heaps[0]),
          to: fmtBytes(heaps[heaps.length - 1]),
          limit: model.memLimit > 0
            ? tsp('perf.trend.aria.limit', '；告警阈值 {v}', { v: fmtBytes(model.memLimit) })
            : '',
        }));
    }

    // 坐标轴三格：起点时刻 / 峰值 / 终点时刻（与概览的 .trend__axis 同结构）。
    const first = win.length ? win[0].ts : 0;
    const last = win.length ? win[win.length - 1].ts : 0;
    const rateAxis = $('#perf-rate-axis');
    if (rateAxis) {
      rateAxis.innerHTML = '<span>' + esc(fmtClock(first)) + '</span>' +
        '<span>' + esc(tsp('perf.axis.peakRate', '峰值 {n} 条/秒', { n: fmtRate(rates.length ? rateMax : 0) })) + '</span>' +
        '<span>' + esc(fmtClock(last)) + '</span>';
    }
    const heapAxis = $('#perf-heap-axis');
    if (heapAxis) {
      heapAxis.innerHTML = '<span>' + esc(fmtClock(first)) + '</span>' +
        '<span>' + esc(tsp('perf.axis.peakHeap', '峰值 {n}', {
          n: fmtBytes(heaps.length ? Math.max.apply(null, heaps) : 0),
        })) + '</span>' +
        '<span>' + esc(fmtClock(last)) + '</span>';
    }
  }

  function renderPerfTable(model) {
    const body = $('#perf-tbody');
    if (!body) return;
    // 每行：{ 标签, 值, 说明, 是否告警 }。告警行同时给 class 与 ⚠ 文本（不只靠颜色）。
    const rows = [
      {
        label: ts('perf.row.deliverDropped', '丢弃 · 发送队列满'),
        value: fmtInt(model.deliverDropped),
        note: ts('perf.note.deliverDropped', '连接发送队列满导致消息被丢弃（消费端过慢）'),
        warn: model.deliverDropped > 0,
      },
      {
        label: ts('perf.row.deliverClosed', '丢弃 · 连接已关闭'),
        value: fmtInt(model.deliverClosed),
        note: ts('perf.note.deliverClosed', '连接已关闭后仍尝试下发而被丢弃（正常下线也会计入）'),
        warn: model.deliverClosed > 0,
      },
      {
        label: ts('perf.row.gcTotal', 'GC 次数（累计）'),
        value: fmtInt(model.gcTotal),
        note: ts('perf.note.gcTotal', '累计 GC 轮次；短窗口内快速上涨说明分配压力大'),
        warn: false,
      },
      {
        label: ts('perf.row.heapInuse', '堆 inuse'),
        value: fmtBytes(model.heapInuse),
        note: ts('perf.note.heapInuse', '已向操作系统申请并正在使用的堆字节'),
        warn: false,
      },
      {
        label: ts('perf.row.sseClients', 'SSE 客户端'),
        value: fmtInt(model.sseClients),
        note: ts('perf.note.sseClients', '当前订阅实时事件流的浏览器连接数'),
        warn: false,
      },
      {
        label: ts('perf.row.eventsDropped', '事件丢弃（总线）'),
        value: fmtInt(model.eventsDropped),
        note: ts('perf.note.eventsDropped', '事件总线缓冲满被丢弃的事件数'),
        warn: model.eventsDropped > 0,
      },
      {
        label: ts('perf.row.memHigh', '内存告警状态'),
        value: model.memHigh
          ? ts('perf.flag.high', '超阈值')
          : ts('perf.flag.normal', '正常'),
        note: model.memLimit > 0
          ? tsp('perf.note.memHigh', '堆走势相对告警阈值 {v} 的比较结果', { v: fmtBytes(model.memLimit) })
          : ts('perf.note.memNoLimit', '未启用告警阈值（memLimitBytes = 0）'),
        warn: model.memHigh,
      },
      {
        label: ts('perf.row.memHighTotal', '内存告警累计次数'),
        value: fmtInt(model.memHighTotal),
        note: ts('perf.note.memHighTotal', '进入"超阈值"状态的累计次数（回落后再次越线会再计一次）'),
        warn: false,
      },
    ];
    body.innerHTML = rows.map((r) =>
      '<tr' + (r.warn ? ' class="is-warn"' : '') + '>' +
        '<th scope="row">' + esc(r.label) + '</th>' +
        '<td>' + (r.warn ? '<span aria-hidden="true">⚠</span> ' : '') + esc(r.value) + '</td>' +
        '<td class="muted">' + esc(r.note) + '</td>' +
      '</tr>'
    ).join('');
  }

  // renderPerfAlert 只画错误横幅，不清空已有曲线（旧数据比空白有用）。
  function renderPerfAlert() {
    const host = $('#perf-alert');
    if (!host) return;
    if (!perfState.error) { host.innerHTML = ''; return; }
    host.innerHTML = '<div class="error-banner">' + esc(ts('perf.loadFailed', '性能数据加载失败：')) +
      esc(perfState.error) + '</div>';
  }

  // renderPerfLive 只在**状态跃迁**时播报：每 2 秒播一次数值会把人烦死，
  // 而"丢弃从 0 变正""堆进入告警"这两个跃迁才是运维真正需要被叫醒的时刻。
  function renderPerfLive(model) {
    const el = $('#perf-live');
    let msg = '';
    if (model.warnDrops && !perfState.live.drops) {
      msg = tsp('perf.live.dropsOn', '开始出现消息丢弃：队列满 {queued} 条，连接已关闭 {closed} 条',
        { queued: fmtInt(model.deliverDropped), closed: fmtInt(model.deliverClosed) });
    }
    perfState.live.drops = model.warnDrops;
    if (!msg && model.warnMem && !perfState.live.mem) {
      msg = tsp('perf.live.memHigh', '堆占用已超过告警阈值（{pct}%）',
        { pct: fmtInt(model.heapPct == null ? 0 : model.heapPct) });
    } else if (!msg && !model.warnMem && perfState.live.mem) {
      msg = ts('perf.live.memOk', '堆占用已回落至告警阈值以下');
    }
    perfState.live.mem = model.warnMem;
    if (!msg) return;           // 没有跃迁：不动 live 区域
    if (el) el.textContent = msg;
  }

  function renderPerf() {
    const m = perfState.last;
    // 还没拿到第一份摘要：保留 HTML 里的骨架/空态（renderPerf 也服务于切语言重绘，
    // 此时无缓存数据必须安全早退，不能抛错）。
    if (!m) return;
    const model = perfModel(m);
    renderPerfKpis(model);
    renderPerfCharts(model);
    renderPerfTable(model);
    renderPerfAlert();
    renderPerfLive(model);

    const updated = $('#perf-updated');
    if (updated) {
      updated.textContent = tsp('perf.updated', '最近更新 {time}', { time: fmtTime(Date.now()) });
    }
    const winEl = $('#perf-window');
    if (winEl) {
      winEl.textContent = tsp('perf.window', '本地窗口 {n} / {cap} 点（约 {sec} 秒窗口）', {
        n: perfState.win.length,
        cap: PERF_WIN_CAP,
        sec: Math.round((PERF_WIN_CAP * PERF_POLL_MS) / 1000),
      });
    }
  }

  /* =====================================================================
     V5 ① 日志视图（/api/logs 全量 + /api/logs/stream 增量）
     ===================================================================== */
  const logState = {
    entries: [],
    seen: {},
    paused: false,
    level: '',
    es: null,
    limit: 200,
    maxRows: 1000,
  };

  const levelLabel = { error: 'error', warn: 'warn', info: 'info' };

  async function loadLogs(openStream) {
    const list = $('#log-list');
    const r = await api('GET', '/api/logs?limit=' + logState.limit +
      (logState.level ? '&level=' + encodeURIComponent(logState.level) : ''));
    if (!r.ok) {
      if (list) {
        list.innerHTML = '<div class="error-banner">' + esc(ts('logs.loadFailed', '日志加载失败：')) +
          esc(r.error) + esc(ts('logs.logviewOff', '（logview 插件可能未启用）')) + '</div>';
      }
      return;
    }
    logState.entries = [];
    logState.seen = {};
    (r.data.entries || []).forEach(addLogEntry);
    renderLogs();
    if (openStream !== false) openLogStream();
  }

  function openLogStream() {
    if (typeof EventSource === 'undefined') return;
    if (logState.es) {
      try { logState.es.close(); } catch (_) { /* 忽略 */ }
      logState.es = null;
    }
    const url = '/api/logs/stream' + (logState.level ? '?level=' + encodeURIComponent(logState.level) : '');
    let es;
    try { es = new EventSource(url); } catch (_) { return; }
    logState.es = es;
    es.addEventListener('log', (e) => {
      let entry = null;
      try { entry = JSON.parse(e.data); } catch (_) { return; }
      if (!entry) return;
      addLogEntry(entry);
      renderLogs();
    });
  }

  function closeLogStream() {
    if (logState.es) {
      try { logState.es.close(); } catch (_) { /* 忽略 */ }
      logState.es = null;
    }
  }

  function addLogEntry(entry) {
    if (!entry || entry.seq == null) return;
    if (logState.seen[entry.seq]) return; // 补发与实时可能有重叠：按 seq 去重
    logState.seen[entry.seq] = true;
    logState.entries.push(entry);
    if (logState.entries.length > logState.maxRows) {
      const dropped = logState.entries.splice(0, logState.entries.length - logState.maxRows);
      dropped.forEach((d) => { delete logState.seen[d.seq]; });
    }
  }

  function renderLogs() {
    const list = $('#log-list');
    if (!list) return;
    const count = $('#logs-count');
    if (count) count.textContent = logState.entries.length;

    if (!logState.entries.length) {
      list.innerHTML = '<div class="empty-state">' +
        '<div class="empty-state__icon">≡</div>' +
        '<div class="empty-state__title">' + esc(ts('logs.emptyTitle', '缓冲里还没有日志')) + '</div>' +
        '<div class="empty-state__text">' +
          esc(ts('logs.emptyText', '日志会在服务端产生时实时推送过来。')) + '</div></div>';
      return;
    }

    const rows = logState.entries.map((e) => {
      const lv = levelLabel[e.level] || 'info';
      return '<div class="log-row log-row--' + esc(lv) + '" data-log="' + esc(e.seq) +
        '" title="' + esc(ts('common.clickToCopy', '点击复制')) + '">' +
        '<span class="log-row__time">' + esc(fmtClock(e.ts)) + '</span>' +
        '<span class="log-badge log-badge--' + esc(lv) + '">' + esc(lv) + '</span>' +
        '<span class="log-row__msg">' + esc(e.msg) + '</span>' +
      '</div>';
    }).join('');
    list.innerHTML = rows;

    if (!logState.paused) list.scrollTop = list.scrollHeight;
  }

  function exportLogs() {
    if (!logState.entries.length) {
      toast(ts('logs.exportEmpty', '当前没有可导出的日志'), 'info');
      return;
    }
    const text = logState.entries.map((e) =>
      '[' + new Date(e.ts).toLocaleString('zh-CN') + '] ' + (e.level || 'info') + ' ' + e.msg
    ).join('\n');
    const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'lan-agent-hub-logs.txt';
    document.body.appendChild(a);
    a.click();
    setTimeout(() => { URL.revokeObjectURL(url); a.remove(); }, 0);
    toast(tsp('logs.exported', '已导出 {n} 行日志', { n: logState.entries.length }), 'success');
  }

  /* ---------------- 初始化事件 ---------------- */
  // setAutoRefresh 是自动刷新的唯一开关入口（界面勾选与命令面板共用，避免两处状态漂移）。
  let refreshTimer = null;
  let autoRefreshOn = false;
  function setAutoRefresh(on) {
    autoRefreshOn = !!on;
    const box = $('#auto-refresh');
    if (box) box.checked = autoRefreshOn;
    if (autoRefreshOn) {
      if (refreshTimer) return;
      refreshTimer = setInterval(() => {
        loadStatus(true);
        if (activeView === 'agents') loadAgents();
        if (activeView === 'rooms') loadRooms();
        if (activeView === 'plugins') loadPlugins(true);
        if (activeView === 'overview') loadMetrics(true);
      }, 5000);
      toast(ts('topbar.autoRefreshOn', '已开启自动刷新（5 秒）'), 'info');
    } else if (refreshTimer) {
      clearInterval(refreshTimer); refreshTimer = null;
    }
  }

  function init() {
    // 导航 + 深链
    $$('.nav__item').forEach((b) => b.addEventListener('click', () => setView(b.getAttribute('data-view'))));
    $$('[data-goto]').forEach((b) => b.addEventListener('click', () => setView(b.getAttribute('data-goto'))));
    window.addEventListener('hashchange', () => setView(viewFromHash(), true));

    // 我的 ID 输入
    bindMyId();

    // Agent 筛选
    $('#agents-search').addEventListener('input', renderAgents);

    // V7 ④：语言下拉（选项在 initI18n 里按服务端清单重建）
    const langSel = $('#lang-select');
    if (langSel) langSel.addEventListener('change', () => setLang(langSel.value));

    // 刷新 / 自动刷新
    $('#refresh-btn').addEventListener('click', () => {
      loadStatus(true);
      if (activeView === 'agents') loadAgents();
      if (activeView === 'rooms') loadRooms();
      if (activeView === 'plugins') loadPlugins(false);
      if (activeView === 'logs') loadLogs(true);
      if (activeView === 'settings') loadSettings(false);
      if (activeView === 'overview') loadMetrics(true);
      if (activeView === 'perf') loadPerf(true); // V8f：手动刷新要能立刻重取，而不是等下一拍
    });
    $('#auto-refresh').addEventListener('change', (e) => setAutoRefresh(!!e.target.checked));

    // 日志视图：级别过滤 / 暂停滚动 / 导出 / 清空 / 点击复制
    const levelSel = $('#logs-level');
    if (levelSel) {
      levelSel.addEventListener('change', () => {
        logState.level = levelSel.value;
        loadLogs(true); // 级别变化需要重取缓冲（服务端按级别过滤）+ 重开流
      });
    }
    const pauseBox = $('#logs-pause');
    if (pauseBox) {
      pauseBox.addEventListener('change', () => {
        logState.paused = pauseBox.checked;
        if (!logState.paused) renderLogs();
      });
    }
    const exportBtn = $('#logs-export');
    if (exportBtn) exportBtn.addEventListener('click', exportLogs);
    const clearBtn = $('#logs-clear');
    if (clearBtn) {
      clearBtn.addEventListener('click', () => {
        logState.entries = [];
        logState.seen = {};
        renderLogs();
        toast(ts('logs.cleared', '已清空视图（服务端缓冲不受影响）'), 'info');
      });
    }
    const logList = $('#log-list');
    if (logList) {
      logList.addEventListener('click', (e) => {
        const row = e.target.closest('.log-row');
        if (!row) return;
        const msg = row.querySelector('.log-row__msg');
        const text = msg ? msg.textContent : row.textContent;
        copyText(text);
      });
    }

    // 插件视图：开关 / 全部重载
    $('#plugins-tbody').addEventListener('change', (e) => {
      const input = e.target;
      if (!input || !input.getAttribute || !input.getAttribute('data-toggle')) return;
      togglePlugin(input);
    });
    $('#plugins-reload').addEventListener('click', reloadPlugins);

    // 设置视图：保存 / 重新载入 / 导出 / 恢复默认
    $('#settings-save').addEventListener('click', saveSettings);
    $('#settings-reload').addEventListener('click', () => loadSettings(false));
    $('#settings-export').addEventListener('click', exportSettings);
    $('#settings-form').addEventListener('click', (e) => {
      const name = e.target && e.target.getAttribute && e.target.getAttribute('data-reset');
      if (!name || !settingsCache) return;
      const defaults = (settingsCache.defaults || {})[name] || {};
      const section = $('#settings-form .settings__section[data-plugin="' + name + '"]');
      if (!section) return;
      $$('[data-opt]', section).forEach((el) => {
        const key = el.getAttribute('data-opt');
        if (Object.prototype.hasOwnProperty.call(defaults, key)) setFieldValue(el, defaults[key]);
      });
      toast(tsp('settings.resetDone', '已恢复 {name} 的默认值（记得保存）', { name: name }), 'info');
    });

    // 创建聊天室
    const crForm = $('#create-room-form');
    wireValidation(crForm);
    crForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const name = $('#room-name').value.trim();
      const members = $('#room-members').value.split(',').map((s) => s.trim()).filter(Boolean);
      const ok = await submitJson($('#create-room-btn'), '/api/create_room', { name, members }, ts('msg.created', '聊天室已创建'));
      if (ok) { crForm.reset(); wireValidation(crForm); loadRooms(); }
    });

    // 房间卡片 加入/离开（事件委托）
    $('#room-grid').addEventListener('click', async (e) => {
      const join = e.target.getAttribute('data-join');
      const leave = e.target.getAttribute('data-leave');
      if (!join && !leave) return;
      const myId = getMyId();
      if (!myId) { toast(ts('msg.needMyId', '请先在上方填写你的 Agent ID'), 'error'); return; }
      const roomId = join || leave;
      const path = join ? '/api/join' : '/api/leave';
      const label = join ? '已加入' : '已离开';
      e.target.disabled = true;
      const r = await api('POST', path, { from: myId, roomId });
      if (r.ok) {
        if (join) myRooms.add(roomId); else myRooms.delete(roomId);
        toast(label + '：' + roomId, 'success'); loadRooms(); loadStatus(true);
      } else { toast('操作失败：' + r.error, 'error'); e.target.disabled = false; }
    });

    // 收件箱清空
    const inboxClear = $('#inbox-clear');
    if (inboxClear) inboxClear.addEventListener('click', () => { inbox = []; renderInbox(); });
    // V7 ①：时间线的「复制消息 JSON」（单条 / 按会话）
    bindTimelineCopy();

    // 在线 Agent 表「发消息」按钮（事件委托）
    $('#agents-tbody').addEventListener('click', (e) => {
      const btn = e.target.closest('[data-msg]');
      if (!btn) return;
      const id = btn.getAttribute('data-msg');
      const to = $('#d-to');
      if (to) to.value = id;
      applyIdentity($('#d-from') ? $('#d-from').value : ''); // 确保身份已记住
      setView('messaging');
      toast(tsp('msg.selectedRecipient', '已选 {id} 为接收方，填好内容即可发送', { id: id }), 'info');
    });

    // 点对点消息
    const sdForm = $('#send-direct-form');
    wireValidation(sdForm);
    sdForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const ok = await submitJson($('#send-direct-btn'), '/api/send', {
        from: $('#d-from').value.trim(), to: $('#d-to').value.trim(), text: $('#d-text').value.trim(),
      }, ts('msg.sentDirect', '已发送（点对点）'));
      if (ok) sdForm.reset(), wireValidation(sdForm);
    });

    // 房间消息
    const srForm = $('#send-room-form');
    wireValidation(srForm);
    srForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const ok = await submitJson($('#send-room-btn'), '/api/room', {
        from: $('#r-from').value.trim(), roomId: $('#r-room').value.trim(), text: $('#r-text').value.trim(),
      }, ts('msg.sentRoom', '已发送到房间'));
      if (ok) srForm.reset(), wireValidation(srForm);
    });

    // 广播
    const bcForm = $('#broadcast-form');
    wireValidation(bcForm);
    bcForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const ok = await submitJson($('#broadcast-btn'), '/api/broadcast', {
        from: $('#b-from').value.trim(), text: $('#b-text').value.trim(),
      }, ts('msg.sentBroadcast', '已广播给所有在线 Agent'));
      if (ok) bcForm.reset(), wireValidation(bcForm);
    });

    // 复制 ID（事件委托）
    document.addEventListener('click', (e) => {
      const t = e.target.closest('.copyable');
      if (!t) return;
      const text = t.getAttribute('data-copy') || t.textContent.trim();
      copyText(text);
    });

    // V7 ①：面板级冻结（事件流与 metrics 共用暂停状态）
    const freezeBox = $('#panel-freeze');
    if (freezeBox) freezeBox.addEventListener('change', () => setFreeze(freezeBox.checked));

    // V7 ①：事件流视图（过滤 / 暂停 / 清空 / 展开载荷）
    bindEventsView();
    renderEvents();

    // V7 ①：命令面板（Ctrl/⌘+K）——先注册内置命令，再绑定键盘与点击
    buildCommands();
    bindCmdk();

    // 首次加载：恢复 hash 指向的视图（刷新后保持位置）
    loadAgents();
    setView(viewFromHash(), true);
    // V7 ④：语言在数据加载之后启动 —— 语言包一到就重画，此时缓存多半已就绪。
    initI18n();
    loadStatus(false);
    loadMetrics(true);
    startPluginsPolling();
    // 实时通道：连上就用推送，连不上自动降级轮询（药丸三态可见）。
    startRealtime();
  }

  // copyText 统一复制入口（剪贴板不可用时明确提示，而不是静默失败）。
  function copyText(text) {
    if (!text) return;
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text)
        .then(() => toast(ts('common.copied', '已复制：') + String(text).slice(0, 60), 'info'))
        .catch(() => toast(ts('common.copyFailed', '复制失败（浏览器拒绝剪贴板访问）'), 'error'));
      return;
    }
    toast(ts('common.clipboardUnsupported', '当前浏览器不支持剪贴板 API'), 'error');
  }

  /* =====================================================================
     V7 ① 事件流 / 命令面板 / 面板级冻结
     ---------------------------------------------------------------------
     后端零改动：全部数据来自 V5 已建好的 /api/stream 与既有 REST 端点。
     纯逻辑（环形缓冲 / 事件过滤 / 模糊打分）刻意抽成独立函数，
     由 internal/plugins/webconsole/console-v7-sim.cjs 在 vm 中直接驱动断言。
     ===================================================================== */

  /* ---------- 常量 ---------- */
  const EVENT_RING_CAP = 500;    // 小建议 1：环形淘汰上限，防长时间挂机内存增长
  const EVENTS_RENDER_MS = 100;  // 事件批处理合并窗口（requestAnimationFrame 不可用时的兜底）
  const EVENT_TYPES = [
    'agent.online', 'agent.offline', 'room.changed', 'plugin.state', 'msg.routed', 'msg.content',
  ];
  // EVENT_TYPE_LABEL 保留「类型 → 中文源」映射（对外导出，且是多处渲染的兜底），
  // EVENT_TYPE_KEY 提供对应的翻译键；eventTypeLabel 在渲染时解析，切语言即生效。
  const EVENT_TYPE_LABEL = {
    'agent.online': 'Agent 上线',
    'agent.offline': 'Agent 离线',
    'room.changed': '房间变更',
    'plugin.state': '插件状态',
    'msg.routed': '消息路由',
    'msg.content': '消息内容',
  };
  const EVENT_TYPE_KEY = {
    'agent.online': 'ev.type.agent.online',
    'agent.offline': 'ev.type.agent.offline',
    'room.changed': 'ev.type.room.changed',
    'plugin.state': 'ev.type.plugin.state',
    'msg.routed': 'ev.type.msg.routed',
    'msg.content': 'ev.type.msg.content',
  };
  function eventTypeLabel(type) {
    const zh = EVENT_TYPE_LABEL[type];
    if (!zh) return String(type);
    return ts(EVENT_TYPE_KEY[type], zh);
  }

  /* ---------- 纯逻辑 1：定长环形缓冲 ---------- */
  function makeRing(cap) { return { cap: cap, items: [], head: 0, total: 0, evicted: 0 }; }
  // ringPush 满了就覆盖最旧的一条（head 始终指向最旧），不再 shift 整个数组。
  function ringPush(ring, item) {
    if (ring.items.length < ring.cap) ring.items.push(item);
    else {
      ring.items[ring.head] = item;
      ring.head = (ring.head + 1) % ring.cap;
      ring.evicted += 1;
    }
    ring.total += 1;
    return ring;
  }
  // ringToArray 按「旧 → 新」顺序导出，方便调用方按需反转。
  function ringToArray(ring) {
    const n = ring.items.length;
    const out = new Array(n);
    for (let i = 0; i < n; i++) out[i] = ring.items[(ring.head + i) % ring.cap];
    return out;
  }
  function ringClear(ring) {
    ring.items = []; ring.head = 0; ring.total = 0; ring.evicted = 0;
    return ring;
  }

  /* ---------- 纯逻辑 2：事件过滤 ---------- */
  // filterEvents 返回**倒序**（最新在前）的可见事件。
  // types 为空 Set 表示"一个类型都没选"→ 结果为空（如实反映，不偷偷回退成全部）。
  function filterEvents(items, opts) {
    const types = opts && opts.types;
    const agent = String(((opts && opts.agent) || '')).trim().toLowerCase();
    const out = [];
    for (let i = items.length - 1; i >= 0; i--) {
      const it = items[i];
      if (types && !types.has(it.type)) continue;
      if (agent && String(it.agent || '').toLowerCase().indexOf(agent) === -1) continue;
      out.push(it);
    }
    return out;
  }

  /* ---------- 纯逻辑 3：模糊匹配打分 ---------- */
  // 返回 <0 表示不匹配。加分项：连续命中、词首命中、标题前缀命中；减分项：文本过长。
  function fuzzyScore(query, text) {
    const q = String(query || '').toLowerCase();
    const t = String(text || '').toLowerCase();
    if (!q) return 0;
    if (!t || q.length > t.length) return -1;
    let qi = 0, score = 0, streak = 0;
    for (let ti = 0; ti < t.length && qi < q.length; ti++) {
      if (t.charAt(ti) === q.charAt(qi)) {
        score += 10 + streak * 6;
        if (ti === 0 || ' -_/.('.indexOf(t.charAt(ti - 1)) >= 0) score += 12;
        streak += 1; qi += 1;
      } else {
        streak = 0;
      }
    }
    if (qi < q.length) return -1;            // 不是子序列 → 不匹配
    if (t.indexOf(q) === 0) score += 25;     // 前缀命中
    score -= Math.max(0, t.length - q.length) * 0.2;
    return Math.round(score);
  }

  /* ---------- 事件流状态 ---------- */
  const eventRing = makeRing(EVENT_RING_CAP);
  const eventState = {
    paused: false,                    // 视图级暂停：只冻结渲染，事件照常进环
    buffered: 0,                      // 暂停期间累积的条数（恢复后清零）
    types: new Set(EVENT_TYPES),      // 多选类型过滤
    agent: '',
    expanded: {},                     // id -> true（展开显示完整 JSON 载荷）
  };
  let evSeq = 0;

  function eventAgentOf(type, data) {
    if (!data || typeof data !== 'object') return type === 'agent.online' || type === 'agent.offline' ? String(data || '') : '';
    if (type === 'agent.online' || type === 'agent.offline') return String(data.id || data.agentId || data.from || '');
    if (type === 'msg.content') return String(data.from || '');
    if (type === 'plugin.state') return String(data.name || '');
    return '';
  }
  function eventTsOf(data) {
    if (data && typeof data === 'object') {
      if (data.ts) return Number(data.ts) || Date.now();
      if (data.at) return Number(data.at) || Date.now();
    }
    return Date.now();
  }
  function safeJSON(v) {
    try { return JSON.stringify(v === undefined ? null : v, null, 2); }
    catch (_) { return String(v); }
  }
  // pushEvent 是事件流的唯一入口：先入环（不丢），再决定要不要重绘。
  function pushEvent(type, data) {
    const item = {
      id: ++evSeq,
      type: type,
      ts: eventTsOf(data),
      agent: eventAgentOf(type, data),
      data: data,
      raw: safeJSON(data),
    };
    ringPush(eventRing, item);
    if (eventState.paused) eventState.buffered += 1;
    scheduleEventsRender();
    return item;
  }

  /* ---------- 面板级冻结（小建议 3）：事件流与 metrics 共用暂停状态 ---------- */
  const freeze = { on: false, dirty: new Set() };
  // renderers 是各渲染入口的名字 → 函数映射，冻结解除时按脏集合重放。
  const renderers = {
    events: function () { renderEvents(); },
    metrics: function () { if (lastMetrics) renderMetrics(lastMetrics); },
    stats: function () { if (lastStatus) renderStatCards(lastStatus); },
    trend: function () { renderTrend(lastSamples, msgRateSamples); },
    perf: function () { renderPerf(); }, // V8f：冻结期间只标脏，解冻后一次性补画
    timeline: function () { renderInbox(); },
  };
  function requestRender(name) {
    if (freeze.on) { freeze.dirty.add(name); return; }
    const fn = renderers[name];
    if (fn) fn();
  }
  function setFreeze(on) {
    freeze.on = !!on;
    const box = $('#panel-freeze');
    if (box) box.checked = freeze.on;
    const hint = $('#freeze-hint');
    if (hint) hint.textContent = freeze.on ? ts('topbar.frozenToast', '面板已冻结') : '';
    if (!freeze.on) {
      const pending = Array.from(freeze.dirty);
      freeze.dirty.clear();
      for (let i = 0; i < pending.length; i++) {
        const fn = renderers[pending[i]];
        if (fn) fn();
      }
    }
  }

  /* ---------- 事件流渲染 ---------- */
  let evRafPending = false;
  // scheduleEventsRender 做批处理合并：高频事件（Agent 频繁上下线）只触发一次重绘。
  // 冻结/暂停时**不入队**，只标记脏——恢复后由补显逻辑统一重绘。
  function scheduleEventsRender() {
    if (freeze.on || eventState.paused) { freeze.dirty.add('events'); return; }
    if (evRafPending) return;
    evRafPending = true;
    const run = function () { evRafPending = false; renderEvents(); };
    if (typeof requestAnimationFrame === 'function') requestAnimationFrame(run);
    else setTimeout(run, EVENTS_RENDER_MS);
  }

  function eventKind(type) { return String(type || '').split('.')[0] || 'other'; }

  function eventSummary(it) {
    const d = it.data;
    if (d === null || d === undefined) return '';
    if (typeof d !== 'object') return esc(String(d));
    if (it.type === 'agent.online' || it.type === 'agent.offline') return esc(String(d.id || d.agentId || ''));
    if (it.type === 'room.changed') {
      const r = d.room || {};
      const n = (r.members || []).length;
      return esc(String(r.name || r.id || '')) + esc(tsp('ev.summary.members', '（成员 {n}）', { n: n }));
    }
    if (it.type === 'plugin.state') return esc(String(d.name || '')) + ' → ' + esc(String(d.state || ''));
    if (it.type === 'msg.routed') {
      return esc(String(d.kind || '')) +
        esc(tsp('ev.summary.delivered', ' · 投递 {n}', { n: Number(d.delivered) || 0 })) +
        ' · seq #' + esc(String(d.seq || ''));
    }
    if (it.type === 'msg.content') {
      const target = d.roomId ? ('#' + d.roomId) : (d.to || ts('ev.summary.broadcast', '广播'));
      return esc(String(d.from || '')) + ' → ' + esc(String(target)) + '：' + esc(String(d.text || '').slice(0, 80));
    }
    return esc(String(d.type || ''));
  }

  function evRowHtml(it) {
    const open = !!eventState.expanded[it.id];
    const kind = eventKind(it.type);
    return '<div class="ev ev--' + esc(kind) + '">' +
      '<button class="ev__row" type="button" data-ev-toggle="' + it.id +
        '" aria-expanded="' + (open ? 'true' : 'false') + '">' +
        '<span class="ev__time">' + esc(fmtClock(it.ts)) + '</span>' +
        '<span class="ev__badge ev__badge--' + esc(kind) + '">' +
          esc(EVENT_TYPE_LABEL[it.type] ? eventTypeLabel(it.type) : it.type) + '</span>' +
        '<span class="ev__sum">' + eventSummary(it) + '</span>' +
        '<span class="ev__caret" aria-hidden="true">' + (open ? '▾' : '▸') + '</span>' +
      '</button>' +
      (open ? '<pre class="ev__json">' + esc(it.raw) + '</pre>' : '') +
    '</div>';
  }

  function renderEvents() {
    const list = $('#event-list');
    if (!list) return;
    const all = ringToArray(eventRing);
    const shown = filterEvents(all, { types: eventState.types, agent: eventState.agent });

    // 展开态随事件一起淘汰：已经被挤出环的事件，其展开标记不该永久留在内存里
    // （长时间挂机下 expanded 会无界增长，这正是「防内存增长」要堵的口子）。
    if (all.length) {
      const oldest = all[0].id;
      Object.keys(eventState.expanded).forEach((k) => {
        if (Number(k) < oldest) delete eventState.expanded[k];
      });
    }

    const countEl = $('#events-count');
    if (countEl) countEl.textContent = String(shown.length);
    const bufEl = $('#events-buffered');
    if (bufEl) {
      bufEl.textContent = eventState.paused
        ? tsp('ev.paused', '已暂停 · 缓冲 {n} 条', { n: eventState.buffered })
        : '';
    }
    const totalEl = $('#events-total');
    if (totalEl) {
      totalEl.textContent = tsp('ev.total', '共 {n} / 上限 {cap}',
        { n: all.length, cap: EVENT_RING_CAP }) +
        (eventRing.evicted ? tsp('ev.evicted', '（已淘汰 {n}）', { n: eventRing.evicted }) : '');
    }

    if (!shown.length) {
      list.innerHTML = '<div class="empty-state">' +
        '<div class="empty-state__icon">⚡</div>' +
        '<div class="empty-state__title">' +
          esc(all.length ? ts('ev.emptyFiltered', '当前过滤条件下没有事件') : ts('ev.emptyWaiting', '等待事件…')) +
          '</div>' +
        '<div class="empty-state__text">' + esc(ts('ev.emptyText.prefix', '来自')) +
          ' <code>/api/stream</code> ' +
          esc(ts('ev.emptyText.suffix', '的实时事件会按时间倒序出现在这里；点击任意一条可展开完整 JSON 载荷。')) +
          '</div></div>';
      return;
    }
    // 上限保护：极端情况下也不让 DOM 行数失控（环形缓冲本身已限 500）。
    list.innerHTML = shown.slice(0, EVENT_RING_CAP).map(evRowHtml).join('');
  }

  function setEventsPaused(on) {
    eventState.paused = !!on;
    // 恢复：缓冲计数清零（它们马上就要被画出来），随后立刻补显。
    if (!eventState.paused) eventState.buffered = 0;
    requestRender('events');
  }
  function clearEvents() {
    ringClear(eventRing);
    eventState.expanded = {};
    eventState.buffered = 0;
    renderEvents();
  }
  function toggleEventExpanded(id) {
    if (eventState.expanded[id]) delete eventState.expanded[id];
    else eventState.expanded[id] = true;
    renderEvents();
  }

  /* ---------- 命令面板（Ctrl/⌘+K）---------- */
  const commands = [];
  function registerCommand(cmd) { commands.push(cmd); return cmd; }
  // matchCommands 先用模糊打分过一遍（标题优先于关键词），再按分数降序、注册序升序稳定排序。
  function matchCommands(query) {
    const q = String(query || '').trim();
    const out = [];
    for (let i = 0; i < commands.length; i++) {
      const c = commands[i];
      if (!q) { out.push({ cmd: c, score: 0, idx: i }); continue; }
      const byTitle = fuzzyScore(q, c.title);
      const byWords = fuzzyScore(q, c.keywords || '');
      const score = Math.max(byTitle, byWords - 5);
      if (score < 0) continue;
      out.push({ cmd: c, score: score, idx: i });
    }
    out.sort((a, b) => (b.score - a.score) || (a.idx - b.idx));
    return out;
  }

  const cmdk = { open: false, results: [], index: 0, restore: null };

  function buildCommands() {
    commands.length = 0;
    Object.keys(pageMeta).forEach((name) => {
      const pt = pageText(name);
      registerCommand({
        id: 'view:' + name,
        group: ts('cmd.group.view', '视图'),
        title: pt[0],
        keywords: name + ' ' + ts('cmd.keywords.view', '视图 view goto 跳转') + ' ' + pt[1],
        run: function () { setView(name); },
      });
    });
    registerCommand({
      id: 'act:refresh',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.refresh.title', '刷新全部数据'),
      keywords: ts('cmd.keywords.refresh', 'refresh reload 刷新 重试'),
      run: function () { $('#refresh-btn').click(); },
    });
    registerCommand({
      id: 'act:autorefresh',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.autorefresh.title', '切换自动刷新'),
      keywords: ts('cmd.keywords.autorefresh', 'auto refresh 自动刷新 轮询'),
      run: function () { setAutoRefresh(!autoRefreshOn); },
    });
    registerCommand({
      id: 'act:freeze',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.freeze.title', '冻结 / 解冻面板'),
      keywords: ts('cmd.keywords.freeze', 'freeze pause 冻结 暂停 排查'),
      run: function () { setFreeze(!freeze.on); },
    });
    registerCommand({
      id: 'act:evpause',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.evpause.title', '暂停 / 恢复事件流'),
      keywords: ts('cmd.keywords.evpause', 'pause stream 事件流 暂停 缓冲'),
      run: function () {
        const box = $('#events-pause');
        if (box) box.checked = !eventState.paused;
        setEventsPaused(!eventState.paused);
      },
    });
    registerCommand({
      id: 'act:evclear',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.evclear.title', '清空事件流'),
      keywords: ts('cmd.keywords.evclear', 'clear 清空 事件流'),
      run: clearEvents,
    });
    registerCommand({
      id: 'act:room',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.room.title', '创建聊天室（填写名称）'),
      keywords: ts('cmd.keywords.room', 'create room 创建 聊天室 房间'),
      run: function () {
        setView('rooms');
        const name = $('#room-name');
        if (name) name.focus();
      },
    });
    registerCommand({
      id: 'act:send',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.send.title', '发送点对点消息（填写内容）'),
      keywords: ts('cmd.keywords.send', 'send direct 发送 消息 私聊'),
      run: function () {
        setView('messaging');
        const text = $('#d-text');
        if (text) text.focus();
      },
    });
    registerCommand({
      id: 'act:broadcast',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.broadcast.title', '发送全员广播（填写内容）'),
      keywords: ts('cmd.keywords.broadcast', 'broadcast 广播 全员'),
      run: function () {
        setView('messaging');
        const text = $('#b-text');
        if (text) text.focus();
      },
    });
    registerCommand({
      id: 'act:copyconv',
      group: ts('cmd.group.action', '操作'),
      title: ts('cmd.copyconv.title', '复制当前会话消息 JSON'),
      keywords: ts('cmd.keywords.copyconv', 'copy json 复制 会话 消息'),
      run: function () {
        const latest = inbox[inbox.length - 1];
        if (!latest) { toast(ts('msg.nothingToCopy', '暂无消息可复制'), 'info'); return; }
        const key = convKeyOf(latest);
        copyText(JSON.stringify(msgsOfConv(key).map(msgJSON), null, 2));
      },
    });
    syncPluginCommands();
  }

  // syncPluginCommands 把「启动/停止插件」注册成命令（插件列表刷新后重算）。
  function syncPluginCommands() {
    for (let i = commands.length - 1; i >= 0; i--) {
      if (String(commands[i].id).indexOf('plugin:') === 0) commands.splice(i, 1);
    }
    if (!pluginsCache || !pluginsCache.plugins) return;
    pluginsCache.plugins.forEach((p) => {
      if (p.required) return; // 必需插件不允许启停，不给命令，避免"执行了却无效"
      registerCommand({
        id: 'plugin:' + p.name,
        group: ts('cmd.group.plugin', '插件'),
        title: (p.enabled
          ? ts('cmd.plugin.stop', '停止插件 ')
          : ts('cmd.plugin.start', '启动插件 ')) + p.name,
        keywords: ts('cmd.keywords.plugin', 'plugin toggle 插件 启停') + ' ' + p.name,
        run: function () { togglePluginByName(p.name, !p.enabled); },
      });
    });
  }

  function togglePluginByName(name, wantEnabled) {
    const input = $('[data-toggle="' + name + '"]');
    if (!input) { loadPlugins(true); toast(ts('plugins.notRendered', '插件开关尚未渲染，已刷新列表'), 'info'); return; }
    if (!!input.checked === !!wantEnabled) {
      toast(tsp('plugins.alreadyInState', '插件 {name} 已处于目标状态', { name: name }), 'info');
      return;
    }
    input.checked = wantEnabled;
    togglePlugin(input);
  }

  function renderCmdk() {
    const list = $('#cmdk-list');
    if (!list) return;
    if (!cmdk.results.length) {
      list.innerHTML = '<li class="cmdk__empty">' + esc(ts('cmd.empty', '没有匹配的命令')) + '</li>';
      return;
    }
    list.innerHTML = cmdk.results.map((r, i) => {
      const active = i === cmdk.index;
      return '<li class="cmdk__item' + (active ? ' is-active' : '') + '" role="option" data-cmdk-index="' + i + '"' +
        ' aria-selected="' + (active ? 'true' : 'false') + '">' +
        '<span class="cmdk__group">' + esc(r.cmd.group || '') + '</span>' +
        '<span class="cmdk__title">' + esc(r.cmd.title) + '</span>' +
        '</li>';
    }).join('');
  }

  function openCmdk() {
    if (cmdk.open) return;
    cmdk.open = true;
    // 焦点管理（⑤ a11y 的结构性基础）：记住来源，关闭后原样还回去。
    cmdk.restore = (document.activeElement && document.activeElement.focus) ? document.activeElement : null;
    const dlg = $('#cmdk');
    const input = $('#cmdk-input');
    if (dlg) dlg.hidden = false;
    if (input) { input.value = ''; input.focus(); }
    cmdk.index = 0;
    cmdk.results = matchCommands('');
    renderCmdk();
  }

  function closeCmdk() {
    if (!cmdk.open) return;
    cmdk.open = false;
    const dlg = $('#cmdk');
    if (dlg) dlg.hidden = true;
    if (cmdk.restore && cmdk.restore.focus) cmdk.restore.focus();
    cmdk.restore = null;
  }

  function moveCmdk(step) {
    const n = cmdk.results.length;
    if (!n) return;
    cmdk.index = ((cmdk.index + step) % n + n) % n;   // 环形移动，越界回绕
    renderCmdk();
  }

  function runCmdk(index) {
    const hit = cmdk.results[index];
    if (!hit) return;
    closeCmdk();
    try { hit.cmd.run(); }
    catch (err) { toast(ts('cmd.execFailed', '命令执行失败：') + (err && err.message ? err.message : err), 'error'); }
  }

  function onCmdkInput() {
    const input = $('#cmdk-input');
    const q = input ? input.value : '';
    cmdk.results = matchCommands(q);
    cmdk.index = 0;
    renderCmdk();
  }

  // bindCmdkKeys 全程键盘可达：↑↓ 选择、Enter 执行、Esc 关闭、Tab 圈禁在面板内。
  function bindCmdkKeys() {
    document.addEventListener('keydown', (e) => {
      const key = e.key || '';
      if ((e.ctrlKey || e.metaKey) && key.toLowerCase() === 'k') {
        e.preventDefault();
        if (cmdk.open) closeCmdk(); else openCmdk();
        return;
      }
      if (!cmdk.open) return;
      if (key === 'Escape') { e.preventDefault(); closeCmdk(); return; }
      if (key === 'ArrowDown') { e.preventDefault(); moveCmdk(1); return; }
      if (key === 'ArrowUp') { e.preventDefault(); moveCmdk(-1); return; }
      if (key === 'Enter') { e.preventDefault(); runCmdk(cmdk.index); return; }
      // 焦点圈禁：面板里只有输入框可聚焦，Tab 不应把焦点甩到背景内容上。
      if (key === 'Tab') { e.preventDefault(); const input = $('#cmdk-input'); if (input) input.focus(); }
    });
  }

  /* ---------- 事件流视图与命令面板的事件绑定 ---------- */
  function bindEventsView() {
    // 类型过滤：多选切换（aria-pressed 让读屏也能读出开关状态）
    const chips = $('#event-type-filters');
    if (chips) {
      chips.innerHTML = EVENT_TYPES.map((t) =>
        '<button class="chip is-on" type="button" data-ev-type="' + esc(t) + '" aria-pressed="true">' +
          esc(EVENT_TYPE_LABEL[t] ? eventTypeLabel(t) : t) + '</button>').join('');
      chips.addEventListener('click', (e) => {
        const btn = e.target.closest ? e.target.closest('[data-ev-type]') : null;
        const type = btn ? btn.getAttribute('data-ev-type') : (e.target.getAttribute ? e.target.getAttribute('data-ev-type') : null);
        if (!type) return;
        if (eventState.types.has(type)) eventState.types.delete(type);
        else eventState.types.add(type);
        if (btn) {
          btn.setAttribute('aria-pressed', eventState.types.has(type) ? 'true' : 'false');
          btn.classList.toggle('is-on', eventState.types.has(type));
        }
        renderEvents();
      });
    }

    const agentInput = $('#events-agent');
    if (agentInput) {
      agentInput.addEventListener('input', () => {
        eventState.agent = agentInput.value || '';
        renderEvents();
      });
    }
    const pauseBox = $('#events-pause');
    if (pauseBox) {
      pauseBox.addEventListener('change', () => setEventsPaused(pauseBox.checked));
    }
    const clearBtn = $('#events-clear');
    if (clearBtn) clearBtn.addEventListener('click', clearEvents);

    const list = $('#event-list');
    if (list) {
      list.addEventListener('click', (e) => {
        const row = e.target.closest ? e.target.closest('[data-ev-toggle]') : null;
        const raw = row ? row.getAttribute('data-ev-toggle')
          : (e.target.getAttribute ? e.target.getAttribute('data-ev-toggle') : null);
        if (!raw) return;
        toggleEventExpanded(Number(raw));
      });
    }
  }

  function bindCmdk() {
    const openBtn = $('#cmdk-open');
    if (openBtn) openBtn.addEventListener('click', openCmdk);
    const backdrop = $('[data-cmdk-close]');
    if (backdrop) backdrop.addEventListener('click', closeCmdk);
    const input = $('#cmdk-input');
    if (input) input.addEventListener('input', onCmdkInput);
    const list = $('#cmdk-list');
    if (list) {
      list.addEventListener('click', (e) => {
        const item = e.target.closest ? e.target.closest('[data-cmdk-index]') : null;
        const raw = item ? item.getAttribute('data-cmdk-index')
          : (e.target.getAttribute ? e.target.getAttribute('data-cmdk-index') : null);
        if (raw === null || raw === undefined) return;
        runCmdk(Number(raw));
      });
    }
    bindCmdkKeys();
  }

  // 时间线上的「复制」按钮：单条复制 + 按会话（from/to）整体复制。
  function bindTimelineCopy() {
    const list = $('#inbox-list');
    if (!list) return;
    list.addEventListener('click', (e) => {
      const el = e.target || {};
      const convKey = el.getAttribute ? el.getAttribute('data-copy-conv') : null;
      if (convKey) {
        copyText(JSON.stringify(msgsOfConv(convKey).map(msgJSON), null, 2));
        return;
      }
      const seq = el.getAttribute ? el.getAttribute('data-copy-msg') : null;
      if (seq) {
        const m = findMsgBySeq(seq);
        if (m) copyText(JSON.stringify(msgJSON(m), null, 2));
        return;
      }
      // 兜底：点到消息空白处复制整条（沿用日志视图的交互习惯）
      const row = el.closest ? el.closest('.msg') : null;
      if (row) {
        const m = findMsgBySeq(row.getAttribute('data-msg'));
        if (m) copyText(JSON.stringify(msgJSON(m), null, 2));
      }
    });
  }

  // 验证钩子：把纯逻辑与关键状态暴露给 console-v7-sim.cjs 直接断言。
  // 仅为自动化验证服务（app.js 源码本就完全公开，不构成新的信息暴露面）。
  window.LanHubV7 = {
    ring: { makeRing, ringPush, ringToArray, ringClear },
    filterEvents: filterEvents,
    fuzzyScore: fuzzyScore,
    matchCommands: matchCommands,
    pushEvent: pushEvent,
    renderEvents: renderEvents,
    clearEvents: clearEvents,
    setEventsPaused: setEventsPaused,
    setFreeze: setFreeze,
    openCmdk: openCmdk,
    closeCmdk: closeCmdk,
    moveCmdk: moveCmdk,
    runCmdk: runCmdk,
    onCmdkInput: onCmdkInput,
    state: { eventRing, eventState, freeze, cmdk, commands, inbox: () => inbox },
    ackStateOf: ackStateOf,
    convKeyOf: convKeyOf,
    msgJSON: msgJSON,
    CONST: { EVENT_RING_CAP, EVENT_TYPES, EVENT_TYPE_LABEL },
  };

  /* =====================================================================
     V7 ③ PWA 与移动端
     —— 安装提示状态机 / 离线检测与提示 / 触控手势 / 版本号 / SW 注册。
     纯逻辑（状态机、方向判定、阈值判定）全部导出到 window.LanHubPWA，
     供 console-pwa-sim.cjs 在 Node vm 里用 DOM 替身驱动断言。
     核心功能不依赖安装：未安装时完整体验不减，安装只是增强。
     ===================================================================== */

  // 与 sw.js 的 SW_VERSION 同值（缓存版本 / 设置页展示的版本号）
  const APP_VERSION = '7.4';
  const INSTALL_STATES = ['idle', 'available', 'installing', 'installed', 'unsupported'];
  const INSTALL_LABEL = '安装应用';
  const OFFLINE_TEXT = '已离线：正在显示最近一次缓存的数据，恢复网络后会自动刷新。';
  // 手势阈值（px）：下拉刷新 72、左右滑切换会话 64；主轴之外允许的偏移量
  const GESTURE = { PULL_THRESHOLD: 72, PULL_MAX_OFF: 40, SWIPE_THRESHOLD: 64, SWIPE_MAX_OFF: 32 };

  const PWA = {
    install: { state: 'idle', prompt: null, btn: null },
    net: { online: true, banner: null },
    gesture: { convIndex: -1, convKey: null, focusEl: null, pullCount: 0, lastGesture: 'none' },
    motion: { instant: false, last: 'none', appliedClass: '' },
    sw: { state: 'idle' },
  };

  /* ---- 安装提示：beforeinstallprompt 状态机 ---- */

  function syncInstallBtn() {
    const st = PWA.install;
    const btn = st.btn;
    if (!btn) return;
    btn.hidden = !(st.state === 'available');
    btn.disabled = st.state === 'installing';
    if (btn.setAttribute) btn.setAttribute('data-install-state', st.state);
    const label = btn.querySelector ? btn.querySelector('.install-btn__label') : null;
    if (label) label.textContent = st.state === 'installing' ? '安装中…' : INSTALL_LABEL;
  }

  function setInstallState(next) {
    PWA.install.state = next;
    syncInstallBtn();
    return next;
  }

  function onBeforeInstallPrompt(evt) {
    if (evt && typeof evt.preventDefault === 'function') evt.preventDefault();
    PWA.install.prompt = evt || null;
    return setInstallState('available');
  }

  function onAppInstalled() {
    PWA.install.prompt = null;
    return setInstallState('installed');
  }

  async function promptInstall() {
    const st = PWA.install;
    if (st.state !== 'available' || !st.prompt) return 'ignored';
    setInstallState('installing');
    let outcome = 'dismissed';
    try {
      if (typeof st.prompt.prompt === 'function') st.prompt.prompt();
      const choice = await st.prompt.userChoice;
      outcome = (choice && choice.outcome) || 'dismissed';
    } catch (_) {
      outcome = 'dismissed';
    }
    if (outcome === 'accepted') {
      st.prompt = null;
      setInstallState('installed');
      toast(ts('pwa.installed', '已添加到桌面'), 'success');
    } else {
      setInstallState('available');
    }
    return outcome;
  }

  function mountInstallButton() {
    const host = $('.topbar__actions');
    if (!host || !document.createElement) return null;
    const btn = document.createElement('button');
    btn.id = 'install-btn';
    btn.type = 'button';
    btn.className = 'btn btn--ghost install-btn';
    btn.hidden = true;
    btn.innerHTML = '<span class="btn__icon" aria-hidden="true">⇩</span>' +
      '<span class="install-btn__label">' + INSTALL_LABEL + '</span>';
    btn.addEventListener('click', function () { promptInstall(); });
    host.appendChild(btn);
    PWA.install.btn = btn;
    syncInstallBtn();
    return btn;
  }

  /* ---- 离线检测：断网后显示缓存数据 + 明确提示（而不是白屏） ---- */

  function isOnline() {
    const nv = typeof navigator !== 'undefined' ? navigator : {};
    return nv.onLine === undefined ? true : !!nv.onLine;
  }

  function syncOfflineBanner() {
    const b = PWA.net.banner;
    if (!b) return;
    const off = !PWA.net.online;
    b.hidden = !off;
    b.textContent = off ? OFFLINE_TEXT : '';
    if (b.setAttribute) {
      if (off) b.setAttribute('data-state', 'offline');
      else b.removeAttribute('data-state');
    }
  }

  function setOnline(online) {
    PWA.net.online = !!online;
    syncOfflineBanner();
    if (!PWA.net.online) {
      setConn('offline');
    } else {
      // 恢复后立刻重取一次，让缓存数据被真实数据替换
      setConn('reconnecting');
      loadStatus(true);
    }
    return PWA.net.online;
  }

  function mountOfflineBanner() {
    if (!document.createElement) return null;
    const banner = document.createElement('div');
    banner.id = 'offline-banner';
    banner.className = 'offline-banner';
    banner.hidden = true;
    if (banner.setAttribute) {
      banner.setAttribute('role', 'status');
      banner.setAttribute('aria-live', 'polite');
    }
    const main = $('.main');
    if (main && typeof main.insertBefore === 'function' && main.firstChild) {
      main.insertBefore(banner, main.firstChild);
    } else if (main && typeof main.appendChild === 'function') {
      main.appendChild(banner);
    } else if (document.body && typeof document.body.appendChild === 'function') {
      document.body.appendChild(banner);
    }
    PWA.net.banner = banner;
    syncOfflineBanner();
    return banner;
  }

  /* ---- 触控手势 ---- */

  // 手势判定（纯函数）：dx/dy 为位移，atTop 表示容器已滚到顶部
  function gestureVerdict(dx, dy, atTop) {
    const adx = Math.abs(dx);
    const ady = Math.abs(dy);
    const top = atTop === undefined ? true : !!atTop;
    if (dy > 0 && dy >= GESTURE.PULL_THRESHOLD && adx <= GESTURE.PULL_MAX_OFF && ady >= adx && top) {
      return 'pull-refresh';
    }
    if (dx < 0 && adx >= GESTURE.SWIPE_THRESHOLD && ady <= GESTURE.SWIPE_MAX_OFF && adx > ady) {
      return 'swipe-left';
    }
    if (dx > 0 && adx >= GESTURE.SWIPE_THRESHOLD && ady <= GESTURE.SWIPE_MAX_OFF && adx > ady) {
      return 'swipe-right';
    }
    return 'none';
  }

  function touchPoint(evt) {
    if (!evt) return null;
    const list = (evt.changedTouches && evt.changedTouches.length) ? evt.changedTouches : (evt.touches || []);
    const t = list && list[0];
    if (!t || typeof t.clientX !== 'number' || typeof t.clientY !== 'number') return null;
    return { x: t.clientX, y: t.clientY };
  }

  function runPullRefresh() {
    PWA.gesture.pullCount += 1;
    renderEvents();
    toast(ts('ev.refreshed', '已刷新事件流'), 'info');
    return PWA.gesture.pullCount;
  }

  // 会话分组键（按时间线当前的渲染顺序去重）
  function convKeys() {
    const seen = [];
    const set = {};
    for (let i = 0; i < inbox.length; i++) {
      const k = convKeyOf(inbox[i]);
      if (!set[k]) { set[k] = true; seen.push(k); }
    }
    return seen;
  }

  function focusConv(index) {
    const keys = convKeys();
    const prev = PWA.gesture.focusEl;
    if (prev && prev.classList && prev.classList.remove) prev.classList.remove('is-conv-focus');
    PWA.gesture.focusEl = null;
    if (!keys.length) {
      PWA.gesture.convIndex = -1;
      PWA.gesture.convKey = null;
      return -1;
    }
    let i = index % keys.length;
    if (i < 0) i += keys.length;
    PWA.gesture.convIndex = i;
    PWA.gesture.convKey = keys[i];
    const list = $('#inbox-list');
    const target = list && list.querySelector ? list.querySelector('[data-conv="' + keys[i] + '"]') : null;
    if (target) {
      if (target.classList && target.classList.add) target.classList.add('is-conv-focus');
      if (typeof target.scrollIntoView === 'function') {
        try { target.scrollIntoView({ block: 'nearest', behavior: motionInstant() ? 'auto' : 'smooth' }); } catch (_) {}
      }
      PWA.gesture.focusEl = target;
    }
    return i;
  }

  function swipeConv(dir) {
    return focusConv(PWA.gesture.convIndex + (dir === 'left' ? 1 : -1));
  }

  // prefers-reduced-motion：滑动动画退化为**瞬时切换**（不位移）
  function motionInstant() {
    let instant = false;
    if (typeof window !== 'undefined' && window && typeof window.matchMedia === 'function') {
      try { instant = !!window.matchMedia('(prefers-reduced-motion: reduce)').matches; } catch (_) { instant = false; }
    }
    PWA.motion.instant = instant;
    return instant;
  }

  function animateSwipe(dir) {
    const list = $('#inbox-list');
    if (!list) return 'none';
    if (list.classList && list.classList.remove) {
      list.classList.remove('is-swipe-left');
      list.classList.remove('is-swipe-right');
      list.classList.remove('is-instant');
    }
    const instant = motionInstant();
    const cls = instant ? 'is-instant' : (dir === 'left' ? 'is-swipe-left' : 'is-swipe-right');
    if (list.classList && list.classList.add) list.classList.add(cls);
    PWA.motion.appliedClass = cls;
    PWA.motion.last = instant ? 'instant' : 'animated';
    return PWA.motion.last;
  }

  function bindPullRefresh(el) {
    if (!el || typeof el.addEventListener !== 'function') return false;
    let start = null;
    el.addEventListener('touchstart', function (e) { start = touchPoint(e); }, { passive: true });
    el.addEventListener('touchend', function (e) {
      const end = touchPoint(e);
      if (!start || !end) { start = null; return; }
      const dx = end.x - start.x;
      const dy = end.y - start.y;
      start = null;
      const atTop = !(typeof el.scrollTop === 'number' && el.scrollTop > 0);
      const v = gestureVerdict(dx, dy, atTop);
      PWA.gesture.lastGesture = v;
      if (v === 'pull-refresh') runPullRefresh();
    });
    return true;
  }

  function bindConvSwipe(el) {
    if (!el || typeof el.addEventListener !== 'function') return false;
    let start = null;
    el.addEventListener('touchstart', function (e) { start = touchPoint(e); }, { passive: true });
    el.addEventListener('touchend', function (e) {
      const end = touchPoint(e);
      if (!start || !end) { start = null; return; }
      const dx = end.x - start.x;
      const dy = end.y - start.y;
      start = null;
      const v = gestureVerdict(dx, dy, false);
      PWA.gesture.lastGesture = v;
      if (v !== 'swipe-left' && v !== 'swipe-right') return;
      const dir = v === 'swipe-left' ? 'left' : 'right';
      animateSwipe(dir);
      swipeConv(dir);
    });
    return true;
  }

  function bindGestures() {
    bindPullRefresh($('#event-list'));
    bindConvSwipe($('#inbox-list'));
    return true;
  }

  /* ---- 版本号（设置页）与 SW 注册 ---- */

  function renderShellVersion() {
    let span = $('#settings-shell-version');
    if (!span || !span.id) {
      if (!document.createElement) return '';
      span = document.createElement('span');
      span.id = 'settings-shell-version';
      span.className = 'muted';
      const anchor = $('#settings-version');
      const parent = anchor && anchor.parentNode;
      if (parent && typeof parent.insertBefore === 'function' && anchor.nextSibling !== undefined) {
        parent.insertBefore(span, anchor.nextSibling || null);
      } else if (document.body && typeof document.body.appendChild === 'function') {
        document.body.appendChild(span);
      }
    }
    span.textContent = tsp('pwa.shellVersion', '控制台 v{version}', { version: APP_VERSION });
    return span.textContent;
  }

  function registerSW() {
    const nv = typeof navigator !== 'undefined' ? navigator : {};
    if (!nv.serviceWorker || typeof nv.serviceWorker.register !== 'function') {
      PWA.sw.state = 'unsupported';
      return PWA.sw.state;
    }
    PWA.sw.state = 'registering';
    try {
      nv.serviceWorker.register('sw.js').then(function () {
        PWA.sw.state = 'registered';
      }).catch(function () {
        PWA.sw.state = 'failed';
      });
    } catch (_) {
      PWA.sw.state = 'failed';
    }
    return PWA.sw.state;
  }

  // 验证钩子：给 console-pwa-sim.cjs 直接驱动断言（app.js 源码本就完全公开）。
  window.LanHubPWA = {
    APP_VERSION: APP_VERSION,
    state: PWA,
    CONST: {
      INSTALL_STATES: INSTALL_STATES,
      INSTALL_LABEL: INSTALL_LABEL,
      OFFLINE_TEXT: OFFLINE_TEXT,
      GESTURE: GESTURE,
    },
    gestureVerdict: gestureVerdict,
    onBeforeInstallPrompt: onBeforeInstallPrompt,
    onAppInstalled: onAppInstalled,
    promptInstall: promptInstall,
    setInstallState: setInstallState,
    syncInstallBtn: syncInstallBtn,
    isOnline: isOnline,
    setOnline: setOnline,
    syncOfflineBanner: syncOfflineBanner,
    convKeys: convKeys,
    focusConv: focusConv,
    swipeConv: swipeConv,
    animateSwipe: animateSwipe,
    motionInstant: motionInstant,
    runPullRefresh: runPullRefresh,
    touchPoint: touchPoint,
    bindGestures: bindGestures,
    mountInstallButton: mountInstallButton,
    mountOfflineBanner: mountOfflineBanner,
    renderShellVersion: renderShellVersion,
    registerSW: registerSW,
  };

  /* V7 ④：i18n 引擎的可测入口——供 console-i18n-sim.cjs 在 Node vm 里驱动断言
     （t / 切换语言 / 静态替换 / Intl 格式化 / 缺失键计数）。生产行为不变。 */
  window.LanHubI18n = {
    t: t,
    ts: ts,
    tsp: tsp,
    subst: subst,
    setLang: setLang,
    loadLangPack: loadLangPack,
    applyStaticI18n: applyStaticI18n,
    rerenderAllViews: rerenderAllViews,
    renderI18nStatus: renderI18nStatus,
    syncLangSelect: syncLangSelect,
    initI18n: initI18n,
    pageText: pageText,
    PAGES: pageMeta,
    fmtInt: fmtInt,
    fmtTime: fmtTime,
    fmtDateTime: fmtDateTime,
    fmtAgo: fmtAgo,
    missingCount: i18nMissingCount,
    state: i18n,
    CONST: { FALLBACK: I18N_FALLBACK, RTL: I18N_RTL },
  };

  /* V8f：性能视图的可测入口——供 console-perf-sim.cjs 在 Node vm 里驱动断言
     （视图注册 / 渲染 / 曲线 / 轮询开关 / 纯函数模型）。生产行为不变。 */
  window.LanHubPerf = {
    state: perfState,
    CONST: { POLL_MS: PERF_POLL_MS, WIN_CAP: PERF_WIN_CAP, TREND_W: TREND_W, TREND_H: TREND_H },
    setView: setView,
    viewFromHash: viewFromHash,
    activeView: function () { return activeView; },
    loadPerf: loadPerf,
    renderPerf: renderPerf,
    perfModel: perfModel,
    perfNum: perfNum,
    perfPushPoint: perfPushPoint,
    fmtBytes: fmtBytes,
    startPolling: perfStartPolling,
    stopPolling: perfStopPolling,
    seriesPoints: seriesPoints,
    pointsAttr: pointsAttr,
    seriesMax: seriesMax,
  };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
