/* =====================================================================
   LAN Agent Hub 控制台 — 前端逻辑（零依赖原生 JS）
   仅调用网关既有 REST 接口，并新增 /api/status、/api/rooms、/api/broadcast。
   覆盖：布局导航、数据表格/列表、表单与按钮、加载态、空状态、错误提示、
        成功/失败反馈、禁用态、响应式（由 CSS 负责）、自动刷新。
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

  /* ---------------- 连接状态药丸 ---------------- */
  const connPill = $('#conn-pill');
  function setConn(state, text) {
    connPill.setAttribute('data-state', state);
    $('.status-pill__text', connPill).textContent = text;
  }

  /* ---------------- 视图切换 ---------------- */
  const pageMeta = {
    overview:  ['概览', '局域网多 Agent 通信中枢运行状态'],
    agents:    ['在线 Agent', '当前连接到 Hub 的全部 Agent'],
    rooms:     ['聊天室', '创建、加入与管理聊天室'],
    messaging: ['发送消息', '向 Agent / 房间发送或广播消息'],
    about:     ['服务信息', 'Hub 协议版本与接入端点'],
  };
  function setView(name) {
    $$('.view').forEach((v) => {
      const active = v.getAttribute('data-view') === name;
      v.classList.toggle('is-active', active);
      v.hidden = !active;
    });
    $$('.nav__item').forEach((b) => b.classList.toggle('is-active', b.getAttribute('data-view') === name));
    const meta = pageMeta[name] || ['', ''];
    $('#page-title').textContent = meta[0];
    $('#page-sub').textContent = meta[1];
    if (name === 'overview') loadStatus(true);
    if (name === 'agents') loadAgents();
    if (name === 'rooms') loadRooms();
    if (name === 'about') renderAbout();
  }

  /* ---------------- 概览 + 状态 ---------------- */
  let lastStatus = null;
  async function loadStatus(silent) {
    const r = await api('GET', '/api/status');
    if (!r.ok) {
      setConn('offline', '已断开');
      if (!silent) toast('无法连接网关：' + r.error, 'error');
      return;
    }
    lastStatus = r.data;
    setConn('online', '已连接');
    $('#foot-proto').textContent = r.data.proto;
    renderStatCards(r.data);
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
      setConn('offline', '已断开');
      tbody.innerHTML = '<tr><td colspan="5"><div class="error-banner">加载失败：' +
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
      tbody.innerHTML = '<tr><td colspan="5"><div class="empty-state">' +
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
        '</tr>';
    }).join('');
  }

  /* ---------------- 聊天室 ---------------- */
  let roomsLoaded = false;
  async function loadRooms() {
    const grid = $('#room-grid');
    if (!roomsLoaded) grid.innerHTML = '<div class="skeleton skeleton-card"></div>';
    const r = await api('GET', '/api/rooms');
    if (!r.ok) {
      setConn('offline', '已断开');
      grid.innerHTML = '<div class="error-banner">加载失败：' + esc(r.error) + '</div>';
      return;
    }
    roomsLoaded = true;
    renderRooms(r.data.rooms || []);
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

  /* 我的 Agent ID（用于加入/离开），持久化到 localStorage */
  function getMyId() { return ($('#my-id') && $('#my-id').value) || localStorage.getItem('lanhub.myId') || ''; }
  function bindMyId() {
    const inp = $('#my-id');
    if (!inp) return;
    const saved = localStorage.getItem('lanhub.myId') || '';
    inp.value = saved;
    inp.addEventListener('input', () => {
      localStorage.setItem('lanhub.myId', inp.value.trim());
      // 重新渲染房间卡片以刷新按钮禁用态
      if (roomsLoaded) loadRooms();
    });
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

  /* ---------------- 初始化事件 ---------------- */
  function init() {
    // 导航
    $$('.nav__item').forEach((b) => b.addEventListener('click', () => setView(b.getAttribute('data-view'))));
    $$('[data-goto]').forEach((b) => b.addEventListener('click', () => setView(b.getAttribute('data-goto'))));

    // 我的 ID 输入
    bindMyId();

    // Agent 筛选
    $('#agents-search').addEventListener('input', renderAgents);

    // 刷新 / 自动刷新
    $('#refresh-btn').addEventListener('click', () => {
      loadStatus(true);
      if ($('.view[data-view="agents"]').classList.contains('is-active')) loadAgents();
      if ($('.view[data-view="rooms"]').classList.contains('is-active')) loadRooms();
    });
    let refreshTimer = null;
    $('#auto-refresh').addEventListener('change', (e) => {
      if (e.target.checked) {
        refreshTimer = setInterval(() => {
          loadStatus(true);
          if ($('.view[data-view="agents"]').classList.contains('is-active')) loadAgents();
          if ($('.view[data-view="rooms"]').classList.contains('is-active')) loadRooms();
        }, 5000);
        toast('已开启自动刷新（5 秒）', 'info');
      } else if (refreshTimer) {
        clearInterval(refreshTimer); refreshTimer = null;
      }
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
      if (r.ok) { toast(label + '：' + roomId, 'success'); loadRooms(); loadStatus(true); }
      else { toast('操作失败：' + r.error, 'error'); e.target.disabled = false; }
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
      if (navigator.clipboard) {
        navigator.clipboard.writeText(text).then(() => toast('已复制：' + text, 'info'))
          .catch(() => toast('复制失败', 'error'));
      }
    });

    // 首次加载
    loadStatus(false);
    loadAgents();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
