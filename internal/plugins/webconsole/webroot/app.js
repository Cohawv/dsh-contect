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
      return { ok: false, status: 0, data: null, error: e.message || '网络错误' };
    }
  }

  /* ---------------- 工具 ---------------- */
  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  function fmtAgo(ms) {
    if (!ms) return '—';
    const diff = Date.now() - ms;
    if (diff < 0) return new Date(ms).toLocaleTimeString('zh-CN');
    if (diff < 60000) return Math.max(0, Math.floor(diff / 1000)) + ' 秒前';
    if (diff < 3600000) return Math.floor(diff / 60000) + ' 分钟前';
    return new Date(ms).toLocaleTimeString('zh-CN');
  }
  function fmtDefault(v) {
    if (v === null || v === undefined) return '—';
    if (typeof v === 'boolean') return v ? '开启' : '关闭';
    if (typeof v === 'string') return v === '' ? '（空）' : v;
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
  const CONN_TEXT = {
    polling: '轮询中',
    online: '已连接(SSE)',
    reconnecting: '重连中',
    connecting: '重连中',
    offline: '已断开',
  };
  function setConn(state, text) {
    connPill.setAttribute('data-state', state);
    const label = $('.status-pill__text', connPill);
    if (label) label.textContent = text || CONN_TEXT[state] || '';
  }

  /* ---------------- 视图切换（支持 URL hash 深链） ---------------- */
  const pageMeta = {
    overview:  ['概览', '局域网多 Agent 通信中枢运行状态'],
    agents:    ['在线 Agent', '当前连接到 Hub 的全部 Agent'],
    rooms:     ['聊天室', '创建、加入与管理聊天室'],
    messaging: ['发送消息', '向 Agent / 房间发送或广播消息——实时时间线'],
    events:    ['事件流', '订阅 /api/stream 的实时事件，可过滤、可暂停、可展开载荷'],
    plugins:   ['插件', '运行期查看与启停内置能力（必需插件与被依赖插件受保护）'],
    logs:      ['日志', '服务端日志环形缓冲（实时增量 + 级别过滤 + 导出）'],
    settings:  ['设置', '由插件配置声明自动生成的表单，改完即存'],
    about:     ['服务信息', 'Hub 协议版本与接入端点'],
  };
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
    const meta = pageMeta[name] || ['', ''];
    $('#page-title').textContent = meta[0];
    $('#page-sub').textContent = meta[1];
    if (!keepHash && (location.hash || '').replace(/^#/, '') !== name) {
      location.hash = name; // 深链：刷新后仍停在当前视图
    }
    if (name === 'overview') { loadStatus(true); loadMetrics(true); }
    if (name === 'agents') loadAgents();
    if (name === 'rooms') loadRooms();
    if (name === 'events') renderEvents(); // 切回事件流时按当前过滤条件重画一次
    if (name === 'plugins') loadPlugins(true);
    if (name === 'logs') loadLogs(true);
    if (name === 'settings') { loadPlugins(true); loadSettings(true); }
    if (name === 'about') renderAbout();
    // 离开日志视图时断开日志流：不在看的连接不该占着服务端名额。
    if (name !== 'logs') closeLogStream();
  }

  /* ---------------- 概览 + 状态 ---------------- */
  let lastStatus = null;
  async function loadStatus(silent) {
    const r = await api('GET', '/api/status');
    if (!r.ok) {
      if (realtime.state !== 'open') setConn('reconnecting');
      if (!silent) toast('无法连接网关：' + r.error, 'error');
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
      { label: '在线 Agent', value: s.agentCount, sub: '当前已注册' },
      { label: '聊天室', value: s.roomCount, sub: '进行中的会话' },
      { label: '协议版本', value: 'v' + s.proto, sub: '对外契约' },
      { label: '自动发现', value: s.discovery ? '已启用' : '已关闭', sub: s.discovery ? 'UDP Beacon' : '需手动配置 IP' },
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
      ['协议版本', 'v' + s.proto],
      ['TCP 端口', s.tcpPort || '—'],
      ['HTTP 端口', s.httpPort || '—'],
      ['在线 Agent', s.agentCount],
      ['聊天室', s.roomCount],
      ['局域网发现', s.discovery ? '已启用' : '已关闭'],
      ['服务器时间', new Date(s.serverTime).toLocaleString('zh-CN')],
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
      tbody.innerHTML = '<tr><td colspan="6"><div class="error-banner">加载失败：' +
        esc(r.error) + '<button class="btn btn--ghost" onclick="location.reload()">重试</button></div></td></tr>';
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
        ? '当前没有 Agent 在线'
        : '没有匹配的 Agent';
      tbody.innerHTML = '<tr><td colspan="6"><div class="empty-state">' +
        '<div class="empty-state__icon">☻</div>' +
        '<div class="empty-state__title">' + esc(emptyMsg) + '</div>' +
        '<div class="empty-state__text">' +
        (agentsCache.length === 0 ? '等待 Agent 通过 TCP 或 DSH 插件接入 Hub。' : '试试调整筛选条件。') +
        '</div></div></td></tr>';
      return;
    }
    tbody.innerHTML = list.map((a) => {
      const caps = (a.caps || []).map((c) =>
        '<span class="badge badge--accent">' + esc(c) + '</span>').join('') || '<span class="muted">—</span>';
      return '<tr>' +
        '<td><span class="copyable badge" title="点击复制">' + esc(a.id) + '</span></td>' +
        '<td>' + esc(a.name || '—') + '</td>' +
        '<td>' + (a.proto ? ('v' + esc(a.proto)) : '—') + '</td>' +
        '<td><div class="badge-row">' + caps + '</div></td>' +
        '<td>' + esc(fmtAgo(a.lastSeen)) + '</td>' +
        '<td><button class="btn btn--sm" data-msg="' + esc(a.id) + '" type="button">发消息</button></td>' +
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
        '<div class="empty-state__title">还没有聊天室</div>' +
        '<div class="empty-state__text">在上方填写名称即可创建一个聊天室，并把 Agent 邀请进来。</div></div>';
      return;
    }
    grid.innerHTML = rooms.map((rm) => {
      const members = (rm.members || []).map((m) =>
        '<span class="member-chip">' + esc(m) + '</span>').join('');
      const memberHtml = members || '<span class="member-chip member-chip--empty">暂无成员</span>';
      const joinDisabled = myId ? '' : 'disabled title="先在上方填写你的 Agent ID"';
      return '<div class="room-card">' +
        '<div class="room-card__head"><div>' +
          '<div class="room-card__name">' + esc(rm.name || '(未命名)') + '</div>' +
          '<div class="room-card__id copyable" title="点击复制房间 ID">' + esc(rm.id) + '</div>' +
        '</div><span class="count-badge">' + esc(rm.memberCount) + ' 人</span></div>' +
        '<div class="room-card__members">' + memberHtml + '</div>' +
        '<div class="room-card__actions">' +
          '<button class="btn btn--ghost" data-join="' + esc(rm.id) + '" ' + joinDisabled + '>加入</button>' +
          '<button class="btn btn--danger" data-leave="' + esc(rm.id) + '" ' + joinDisabled + '>离开</button>' +
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
  const ACK_BADGE = {
    pending:   { state: 'pending', label: '未读',   title: '已送达对端，等待回执' },
    delivered: { state: 'read',    label: '已读',   title: '对端已回执' },
    failed:    { state: 'failed',  label: '失败',   title: '目标离线，投递失败' },
    expired:   { state: 'failed',  label: '已过期', title: '超过 TTL，记录已清理' },
  };
  function ackStateOf(m) {
    const myId = getMyId();
    if (!myId || m.from !== myId) return null;
    const ack = ackBySeq.get(m.seq);
    if (ack) return ACK_BADGE[ack] || null;
    const routed = routeBySeq.get(m.seq);
    if (!routed) return { state: 'sending', label: '投递中', title: '尚未收到路由结果' };
    return routed.delivered > 0
      ? ACK_BADGE.pending
      : ACK_BADGE.failed;
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
    if (m.roomId) return '房间 · ' + m.roomId;
    if (m.dir === 'broadcast') return '全员广播';
    return '私聊 · ' + (convPeer(m) || '未知');
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
    if (m.dir === 'out') who = '→ ' + esc(m.to || '广播');
    else if (m.dir === 'broadcast') who = '📢 ' + esc(m.from);
    else if (m.dir === 'room') who = '#' + esc(m.roomId) + ' · ' + esc(m.from);
    else who = esc(m.from) + ' → 我';
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
          '" aria-label="复制该消息的 JSON">复制</button>' +
      '</div>' +
      '<div class="msg__text">' + esc(m.text || '') + '</div></div>';
  }

  function renderInbox() {
    const list = $('#inbox-list');
    if (!list) return;
    if (!inbox.length) {
      list.innerHTML = '<div class="empty-state">' +
        '<div class="empty-state__icon">✉</div>' +
        '<div class="empty-state__title">暂无消息</div>' +
        '<div class="empty-state__text">发送或接收的消息会按会话分组出现在这里。先在「聊天室」里填写你的 Agent ID，发出的消息会标记为「我发出」并显示投递状态。</div></div>';
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
      return '<section class="conv">' +
        '<header class="conv__head">' +
          '<h3 class="conv__title">' + esc(g.title) + '</h3>' +
          '<span class="conv__meta">' + msgs.length + ' 条</span>' +
          '<button class="btn btn--ghost btn--xs" type="button" data-copy-conv="' + esc(g.key) +
            '">复制会话 JSON</button>' +
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
    toast('操作失败：' + r.error, 'error');
    return false;
  }

  /* =====================================================================
     插件视图（V4 ②）
     数据来源：/api/plugins（含每个插件的状态、依赖、路由归属、启停次数）。
     快照 10s 轮询，操作有 loading 态防重复点击，failed 的错误**必须可见**。
     ===================================================================== */
  const PLUGINS_POLL_MS = 10000;

  const stateLabel = {
    running: '运行中',
    stopped: '已停止',
    disabled: '已停用',
    failed: '失败',
    initialized: '已装配',
    registered: '已注册',
  };

  function stateBadge(state) {
    const cls = 'plugin-state plugin-state--' + (stateLabel[state] ? state : 'registered');
    return '<span class="' + cls + '"><span class="plugin-state__dot"></span>' +
      esc(stateLabel[state] || state) + '</span>';
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
          ? '<div class="error-banner">无法连接 Hub（可能未启动或端口不通）：' + esc(r.error) + '</div>'
          : '<div class="error-banner">插件列表加载失败：' + esc(r.error) + '</div>';
      }
      return;
    }
    setConn('online');
    if ($('#plugins-alert')) $('#plugins-alert').innerHTML = '';
    pluginsCache = r.data;
    renderPlugins();
    syncPluginCommands(); // V7 ①：命令面板的「启动/停止插件 <name>」跟随最新插件快照
    if (!silent) toast('插件快照已刷新', 'info');
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
    $('#plugins-summary').textContent =
      runningList.length + ' 启用 / ' + disabledList.length + ' 停用 / ' + failedList.length + ' 失败';

    if (!list.length) {
      tbody.innerHTML = '<tr><td colspan="8"><div class="empty-state">' +
        '<div class="empty-state__icon">▤</div>' +
        '<div class="empty-state__title">二进制里没有插件</div>' +
        '<div class="empty-state__text">检查 cmd/hub/plugins/plugins.go 的功能清单。</div></div></td></tr>';
      return;
    }

    tbody.innerHTML = list.map((p) => {
      const isOn = (p.state !== 'disabled' && p.state !== 'stopped');
      const missingDeps = (p.dependencies || []).filter((d) => !runningSet.has(d));
      // 停用态开关的可操作性：Required 不可停用；依赖未就绪时不允许启用
      const lockForRequired = p.required && isOn;
      const lockForDeps = !isOn && missingDeps.length > 0;
      const locked = lockForRequired || lockForDeps;

      let title = isOn ? '停用该插件' : '启用该插件';
      if (lockForRequired) title = p.requiredReason || '必需插件，不可停用';
      if (lockForDeps) title = '依赖未运行：' + missingDeps.join('、');

      const requiredBadge = p.required ? ' <span class="badge badge--magenta" title="必需插件">必需</span>' : '';
      const depsHtml = (p.dependencies || []).length
        ? p.dependencies.map((d) => '<span class="badge ' + (runningSet.has(d) ? 'badge--accent' : '') + '">' + esc(d) + '</span>').join('')
        : '<span class="muted">—</span>';
      const routesTitle = (p.routes || []).join(' ');
      const routesHtml = '<span class="plugin-num" title="' + esc(routesTitle) + '">' + esc(p.routeCount) + '</span>';

      const mainRow = '<tr class="plugin-row' + (p.state === 'failed' ? ' plugin-row--failed' : '') + '">' +
        '<td><label class="switch plugin-toggle' + (locked ? ' is-locked' : '') + '" title="' + esc(title) + '">' +
          '<input type="checkbox" data-toggle="' + esc(p.name) + '"' + (isOn ? ' checked' : '') +
          (locked ? ' disabled' : '') + '>' +
          '<span class="switch__track"><span class="switch__thumb"></span></span>' +
        '</label></td>' +
        '<td><span class="plugin-name">' + esc(p.name) + '</span>' + requiredBadge + '</td>' +
        '<td class="plugin-num">' + esc(p.version) + '</td>' +
        '<td>' + stateBadge(p.state) + '</td>' +
        '<td><div class="plugin-desc" title="' + esc(p.description) + '">' + esc(p.description) + '</div></td>' +
        '<td><div class="plugin-deps">' + depsHtml + '</div></td>' +
        '<td>' + routesHtml + '</td>' +
        '<td class="plugin-num">' + esc(p.restartCount) + '</td>' +
        '</tr>';

      // 失败详情展开行：错误**绝不隐藏**（可运维性的核心）
      const errorRow = p.state === 'failed'
        ? '<tr><td class="plugin-error-cell" colspan="8"><div class="error-banner plugin-error">' +
            '<div><div class="plugin-error__title">' + esc(p.name) + ' 启动/装配失败</div>' +
            '<code>' + esc(p.error || '（无错误信息）') + '</code>' +
            '<div class="plugin-error__meta">状态变更于 ' + esc(fmtAgo(p.stateChangedAt)) +
            ' · 启停次数 ' + esc(p.restartCount) + '</div></div></div></td></tr>'
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
      toast((action === 'enable' ? '已启用 ' : '已停用 ') + name, 'success');
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
      toast('重载失败：' + r.error, 'error');
    } else if (r.data && r.data.failed) {
      toast('重载完成，但有 ' + r.data.failed + ' 个插件失败，请查看错误详情', 'error');
    } else {
      toast('全部插件已重载（未重启进程）', 'success');
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
    if (!silent) toast('设置已重新载入', 'info');
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
          parts.push('已不存在的插件段 ' + staleSections.map(function (n) { return esc(n); }).join('、'));
        }
        if (staleKeys.length) {
          parts.push('已废弃的配置项 ' + staleKeys.map(function (o) {
            return esc(o.plugin) + '<code>.' + esc(o.key) + '</code>';
          }).join('、'));
        }
        staleBox.innerHTML = '<div class="settings__hint-invalid">磁盘配置里有 ' + staleCount +
          ' 个本二进制中已废弃的段/配置项（' + parts.join('；') + '），保存一次即会被清理。</div>';
      }
    }

    const saveBtn = $('#settings-save');
    if (saveBtn && data.readOnly) {
      saveBtn.disabled = true;
      saveBtn.title = '只读模式（-config-readonly）下不可保存';
    }

    const schema = data.schema || {};
    const names = Object.keys(schema).sort();
    if (!names.length) {
      form.innerHTML = '<div class="settings__empty">当前二进制中的插件都没有声明可配置项。</div>';
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
            '<button class="btn btn--ghost" type="button" data-reset="' + esc(name) + '">恢复默认</button>' +
            (usable ? '' : '<span class="muted">插件当前未运行，改动会写入配置并需要重启/重载生效。</span>') +
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
          '<span class="switch__label">' + (current ? '开启' : '关闭') + '</span></label>';
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
        '<span class="settings__default">默认 ' + esc(fmtDefault(def.default)) + '</span>' +
        '<span class="settings__modified" hidden>已修改</span>' +
      '</div>' +
      control +
      (def.description ? '<div class="settings__default">' + esc(def.description) + '</div>' : '') +
      '<div class="settings__hint-invalid" hidden>取值不合法</div>' +
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
          if (lbl) lbl.textContent = el.checked ? '开启' : '关闭';
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
      toast('配置项 ' + collected.invalid + ' 的取值不合法，请先修正', 'error');
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
        toast('已保存 v' + r.data.version + '；待生效：' + pending.join('、'), 'info');
      } else {
        $('#settings-alert').innerHTML = '';
        toast('设置已保存并生效（v' + r.data.version + '）', 'success');
      }
      renderSettings();
      loadStatus(true);
      return;
    }

    if (r.status === 409) {
      const d = r.data || {};
      if ($('#settings-alert')) {
        $('#settings-alert').innerHTML = '<div class="error-banner">他人已修改配置（磁盘版本 v' + esc(d.version) +
          '），你的改动未保存。<button class="btn btn--ghost" id="settings-view-latest" type="button">查看最新值</button></div>';
        const latest = $('#settings-view-latest');
        if (latest) latest.addEventListener('click', () => loadSettings(false));
      }
      toast('保存冲突：配置已被他人修改（409），你的改动未保存', 'error');
      return;
    }

    if ($('#settings-alert')) {
      $('#settings-alert').innerHTML = '<div class="error-banner">保存失败：' + esc(r.error) + '</div>';
    }
    toast('保存失败：' + r.error, 'error');
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
    toast('已导出当前配置副本', 'success');
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
      degradeToPolling('当前浏览器不支持 EventSource');
      return;
    }
    stopPolling();
    realtime.state = 'connecting';
    setConn('connecting');

    let es;
    try {
      es = new EventSource('/api/stream?topics=' + encodeURIComponent(SSE_TOPICS));
    } catch (e) {
      degradeToPolling('无法创建 EventSource：' + (e.message || e));
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
        degradeToPolling('实时连接连续失败');
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
    connPill.title = '实时连接不可用（' + reason + '），已切换为每 5 秒轮询';
    if (!realtime.degradedNotified) {
      realtime.degradedNotified = true;
      toast('实时连接不可用（' + reason + '），已切换为 5 秒轮询', 'info');
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
      if (!silent) toast('指标加载失败：' + r.error, 'error');
      return;
    }
    lastMetrics = r.data;
    requestRender('metrics');
  }

  function fmtDuration(ms) {
    if (!ms || ms < 0) return '—';
    const s = Math.floor(ms / 1000);
    if (s < 60) return s + ' 秒';
    if (s < 3600) return Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒';
    return Math.floor(s / 3600) + ' 时 ' + Math.floor((s % 3600) / 60) + ' 分';
  }

  function renderMetrics(m) {
    const grid = $('#metrics-cards');
    if (!grid || !m) return;
    lastMetrics = m; // 冻结解除后要能原样重绘，故留住最近一次快照

    const cards = [
      { label: '在线 Agent', value: m.agentsOnline, sub: 'hub_agents_online' },
      { label: '房间数', value: m.rooms, sub: 'hub_rooms' },
      { label: '累计消息', value: m.messagesTotal, sub: 'hub_messages_total' },
      { label: '消息速率', value: (m.messagesPerSecond || 0).toFixed(2), sub: '条 / 秒（最近窗口）' },
      { label: 'SSE 连接', value: m.sseClients, sub: 'hub_sse_clients' },
      { label: '运行时长', value: fmtDuration(m.uptimeMs), sub: 'metrics 插件' },
    ];
    grid.innerHTML = cards.map((c) =>
      '<div class="metric-card">' +
        '<div class="metric-card__label">' + esc(c.label) + '</div>' +
        '<div class="metric-card__value">' + esc(c.value) + '</div>' +
        '<div class="metric-card__sub">' + esc(c.sub) + '</div>' +
      '</div>'
    ).join('');

    const updated = $('#metrics-updated');
    if (updated) updated.textContent = '采样间隔 ' + Math.round((m.intervalMs || 0) / 1000) + ' 秒';

    lastSamples = m.samples || [];
    renderTrend(lastSamples, msgRateSamples);
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
      if (axis) axis.innerHTML = '<span>暂无采样点</span>';
      return;
    }

    const W = 600, H = 120, PAD = 6;
    const values = samples.map((s) => (s.agentsOnline || 0));
    const maxV = Math.max(1, Math.max.apply(null, values.length ? values : [0]));
    const step = values.length > 1 ? (W - PAD * 2) / (values.length - 1) : 0;

    const pts = values.map((v, i) => {
      const x = PAD + i * step;
      const y = H - PAD - (v / maxV) * (H - PAD * 2);
      return [x, y];
    });
    const line = pts.map((p) => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' ');
    const area = pts.length
      ? 'M' + pts[0][0].toFixed(1) + ',' + (H - PAD) + ' L' + line.split(' ').join(' L') +
        ' L' + pts[pts.length - 1][0].toFixed(1) + ',' + (H - PAD) + ' Z'
      : '';

    const dots = pts.map((p) =>
      '<circle cx="' + p[0].toFixed(1) + '" cy="' + p[1].toFixed(1) + '" r="2.5" />').join('');
    const grid = [0, 0.5, 1].map((f) =>
      '<line x1="0" x2="' + W + '" y1="' + (PAD + f * (H - PAD * 2)).toFixed(1) +
      '" y2="' + (PAD + f * (H - PAD * 2)).toFixed(1) + '" />').join('');

    // 第二条线：消息速率（条/秒），按自己的最大值归一化，独立 Y 轴。
    let rateLine = '';
    if (msgRates.length) {
      const maxR = Math.max(0.01, Math.max.apply(null, msgRates));
      const rStep = msgRates.length > 1 ? (W - PAD * 2) / (msgRates.length - 1) : 0;
      const rPts = msgRates.map((v, i) => {
        const x = PAD + i * rStep;
        const y = H - PAD - (v / maxR) * (H - PAD * 2);
        return [x, y];
      });
      rateLine = '<polyline class="trend__line trend__line--rate" points="' +
        rPts.map((p) => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' ') + '" />' +
        '<g class="trend__dots trend__dots--rate">' +
        rPts.map((p) => '<circle cx="' + p[0].toFixed(1) + '" cy="' + p[1].toFixed(1) + '" r="2" />').join('') +
        '</g>';
    }

    svg.innerHTML =
      '<g class="trend__grid">' + grid + '</g>' +
      (area ? '<path class="trend__area" d="' + area + '" />' : '') +
      '<polyline class="trend__line" points="' + line + '" />' +
      '<g class="trend__dots">' + dots + '</g>' +
      rateLine;

    if (axis) {
      const first = samples[0] || {};
      const last = samples[samples.length - 1] || {};
      const lastRate = msgRates.length ? msgRates[msgRates.length - 1] : null;
      axis.innerHTML =
        '<span>' + esc(fmtClock(first.ts)) + '</span>' +
        '<span>在线峰值 ' + esc(maxV) + '</span>' +
        '<span>' + (lastRate === null ? '消息速率 —' : '消息速率 ' + lastRate.toFixed(2) + ' 条/秒') + '</span>' +
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
        list.innerHTML = '<div class="error-banner">日志加载失败：' + esc(r.error) +
          '（logview 插件可能未启用）</div>';
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
        '<div class="empty-state__title">缓冲里还没有日志</div>' +
        '<div class="empty-state__text">日志会在服务端产生时实时推送过来。</div></div>';
      return;
    }

    const rows = logState.entries.map((e) => {
      const lv = levelLabel[e.level] || 'info';
      return '<div class="log-row log-row--' + esc(lv) + '" data-log="' + esc(e.seq) + '" title="点击复制">' +
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
      toast('当前没有可导出的日志', 'info');
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
    toast('已导出 ' + logState.entries.length + ' 行日志', 'success');
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
      toast('已开启自动刷新（5 秒）', 'info');
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

    // 刷新 / 自动刷新
    $('#refresh-btn').addEventListener('click', () => {
      loadStatus(true);
      if (activeView === 'agents') loadAgents();
      if (activeView === 'rooms') loadRooms();
      if (activeView === 'plugins') loadPlugins(false);
      if (activeView === 'logs') loadLogs(true);
      if (activeView === 'settings') loadSettings(false);
      if (activeView === 'overview') loadMetrics(true);
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
        toast('已清空视图（服务端缓冲不受影响）', 'info');
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
      toast('已恢复 ' + name + ' 的默认值（记得保存）', 'info');
    });

    // 创建聊天室
    const crForm = $('#create-room-form');
    wireValidation(crForm);
    crForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const name = $('#room-name').value.trim();
      const members = $('#room-members').value.split(',').map((s) => s.trim()).filter(Boolean);
      const ok = await submitJson($('#create-room-btn'), '/api/create_room', { name, members }, '聊天室已创建');
      if (ok) { crForm.reset(); wireValidation(crForm); loadRooms(); }
    });

    // 房间卡片 加入/离开（事件委托）
    $('#room-grid').addEventListener('click', async (e) => {
      const join = e.target.getAttribute('data-join');
      const leave = e.target.getAttribute('data-leave');
      if (!join && !leave) return;
      const myId = getMyId();
      if (!myId) { toast('请先在上方填写你的 Agent ID', 'error'); return; }
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
      toast('已选 ' + id + ' 为接收方，填好内容即可发送', 'info');
    });

    // 点对点消息
    const sdForm = $('#send-direct-form');
    wireValidation(sdForm);
    sdForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const ok = await submitJson($('#send-direct-btn'), '/api/send', {
        from: $('#d-from').value.trim(), to: $('#d-to').value.trim(), text: $('#d-text').value.trim(),
      }, '已发送（点对点）');
      if (ok) sdForm.reset(), wireValidation(sdForm);
    });

    // 房间消息
    const srForm = $('#send-room-form');
    wireValidation(srForm);
    srForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const ok = await submitJson($('#send-room-btn'), '/api/room', {
        from: $('#r-from').value.trim(), roomId: $('#r-room').value.trim(), text: $('#r-text').value.trim(),
      }, '已发送到房间');
      if (ok) srForm.reset(), wireValidation(srForm);
    });

    // 广播
    const bcForm = $('#broadcast-form');
    wireValidation(bcForm);
    bcForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const ok = await submitJson($('#broadcast-btn'), '/api/broadcast', {
        from: $('#b-from').value.trim(), text: $('#b-text').value.trim(),
      }, '已广播给所有在线 Agent');
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
        .then(() => toast('已复制：' + String(text).slice(0, 60), 'info'))
        .catch(() => toast('复制失败（浏览器拒绝剪贴板访问）', 'error'));
      return;
    }
    toast('当前浏览器不支持剪贴板 API', 'error');
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
  const EVENT_TYPE_LABEL = {
    'agent.online': 'Agent 上线',
    'agent.offline': 'Agent 离线',
    'room.changed': '房间变更',
    'plugin.state': '插件状态',
    'msg.routed': '消息路由',
    'msg.content': '消息内容',
  };

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
    if (hint) hint.textContent = freeze.on ? '面板已冻结' : '';
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
      return esc(String(r.name || r.id || '')) + '（成员 ' + n + '）';
    }
    if (it.type === 'plugin.state') return esc(String(d.name || '')) + ' → ' + esc(String(d.state || ''));
    if (it.type === 'msg.routed') {
      return esc(String(d.kind || '')) + ' · 投递 ' + (Number(d.delivered) || 0) + ' · seq #' + esc(String(d.seq || ''));
    }
    if (it.type === 'msg.content') {
      const target = d.roomId ? ('#' + d.roomId) : (d.to || '广播');
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
          esc(EVENT_TYPE_LABEL[it.type] || it.type) + '</span>' +
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
    if (bufEl) bufEl.textContent = eventState.paused ? ('已暂停 · 缓冲 ' + eventState.buffered + ' 条') : '';
    const totalEl = $('#events-total');
    if (totalEl) totalEl.textContent = '共 ' + all.length + ' / 上限 ' + EVENT_RING_CAP +
      (eventRing.evicted ? '（已淘汰 ' + eventRing.evicted + '）' : '');

    if (!shown.length) {
      list.innerHTML = '<div class="empty-state">' +
        '<div class="empty-state__icon">⚡</div>' +
        '<div class="empty-state__title">' +
          (all.length ? '当前过滤条件下没有事件' : '等待事件…') + '</div>' +
        '<div class="empty-state__text">来自 <code>/api/stream</code> 的实时事件会按时间倒序出现在这里；' +
          '点击任意一条可展开完整 JSON 载荷。</div></div>';
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
      registerCommand({
        id: 'view:' + name,
        group: '视图',
        title: pageMeta[name][0],
        keywords: name + ' 视图 view goto 跳转 ' + pageMeta[name][1],
        run: function () { setView(name); },
      });
    });
    registerCommand({
      id: 'act:refresh', group: '操作', title: '刷新全部数据', keywords: 'refresh reload 刷新 重试',
      run: function () { $('#refresh-btn').click(); },
    });
    registerCommand({
      id: 'act:autorefresh', group: '操作', title: '切换自动刷新', keywords: 'auto refresh 自动刷新 轮询',
      run: function () { setAutoRefresh(!autoRefreshOn); },
    });
    registerCommand({
      id: 'act:freeze', group: '操作', title: '冻结 / 解冻面板', keywords: 'freeze pause 冻结 暂停 排查',
      run: function () { setFreeze(!freeze.on); },
    });
    registerCommand({
      id: 'act:evpause', group: '操作', title: '暂停 / 恢复事件流', keywords: 'pause stream 事件流 暂停 缓冲',
      run: function () {
        const box = $('#events-pause');
        if (box) box.checked = !eventState.paused;
        setEventsPaused(!eventState.paused);
      },
    });
    registerCommand({
      id: 'act:evclear', group: '操作', title: '清空事件流', keywords: 'clear 清空 事件流',
      run: clearEvents,
    });
    registerCommand({
      id: 'act:room', group: '操作', title: '创建聊天室（填写名称）', keywords: 'create room 创建 聊天室 房间',
      run: function () {
        setView('rooms');
        const name = $('#room-name');
        if (name) name.focus();
      },
    });
    registerCommand({
      id: 'act:send', group: '操作', title: '发送点对点消息（填写内容）', keywords: 'send direct 发送 消息 私聊',
      run: function () {
        setView('messaging');
        const text = $('#d-text');
        if (text) text.focus();
      },
    });
    registerCommand({
      id: 'act:broadcast', group: '操作', title: '发送全员广播（填写内容）', keywords: 'broadcast 广播 全员',
      run: function () {
        setView('messaging');
        const text = $('#b-text');
        if (text) text.focus();
      },
    });
    registerCommand({
      id: 'act:copyconv', group: '操作', title: '复制当前会话消息 JSON', keywords: 'copy json 复制 会话 消息',
      run: function () {
        const latest = inbox[inbox.length - 1];
        if (!latest) { toast('暂无消息可复制', 'info'); return; }
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
        group: '插件',
        title: (p.enabled ? '停止插件 ' : '启动插件 ') + p.name,
        keywords: 'plugin toggle 插件 启停 ' + p.name,
        run: function () { togglePluginByName(p.name, !p.enabled); },
      });
    });
  }

  function togglePluginByName(name, wantEnabled) {
    const input = $('[data-toggle="' + name + '"]');
    if (!input) { loadPlugins(true); toast('插件开关尚未渲染，已刷新列表', 'info'); return; }
    if (!!input.checked === !!wantEnabled) { toast('插件 ' + name + ' 已处于目标状态', 'info'); return; }
    input.checked = wantEnabled;
    togglePlugin(input);
  }

  function renderCmdk() {
    const list = $('#cmdk-list');
    if (!list) return;
    if (!cmdk.results.length) {
      list.innerHTML = '<li class="cmdk__empty">没有匹配的命令</li>';
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
    catch (err) { toast('命令执行失败：' + (err && err.message ? err.message : err), 'error'); }
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
          esc(EVENT_TYPE_LABEL[t] || t) + '</button>').join('');
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

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
