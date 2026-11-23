'use strict';

/* QuickWork console: vanilla JS, no build step, no external assets. */

const state = {
  csrf: null,
  page: 'overview',
  overview: null,
  endpoints: null,
  accounts: [],
  proxies: [],
  config: null,
  logs: { items: [], stats: null },
  logFilter: 'all',
  logAuto: true,
  login: null,
  loginTimer: null,
  refreshTimer: null,
  chat: { messages: [], streaming: false, model: '', system: '' },
  theme: localStorage.getItem('qw-theme') || 'dark',
};

const PAGES = {
  overview: { title: '总览', eyebrow: 'SECTION 01 / RUNTIME', load: pageOverview },
  accounts: { title: '账号', eyebrow: 'SECTION 02 / ACCOUNTS', load: pageAccounts },
  proxies: { title: '代理池', eyebrow: 'SECTION 03 / PROXY POOL', load: pageProxies },
  login: { title: '登录上游', eyebrow: 'SECTION 04 / OAUTH', load: pageLogin },
  config: { title: '配置', eyebrow: 'SECTION 04 / CONFIG', load: pageConfig },
  logs: { title: '请求日志', eyebrow: 'SECTION 05 / TRAFFIC', load: pageLogs },
  playground: { title: 'Playground', eyebrow: 'SECTION 06 / CHAT', load: pagePlayground },
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
const content = () => $('#content');

function esc(value) {
  return String(value === undefined || value === null ? '' : value).replace(/[&<>"']/g, (ch) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[ch]));
}

function linkify(text) {
  const re = /https?:\/\/[^\s"'<>]+/g;
  let out = '';
  let last = 0;
  let match;
  while ((match = re.exec(text)) !== null) {
    out += esc(text.slice(last, match.index));
    const url = match[0];
    out += `<a href="${esc(url)}" target="_blank" rel="noreferrer noopener">${esc(url)}</a>`;
    last = match.index + url.length;
  }
  return out + esc(text.slice(last));
}

function formatNumber(value) {
  const n = Number(value || 0);
  return n.toLocaleString('en-US');
}

function formatDuration(seconds) {
  const total = Math.max(0, Math.floor(Number(seconds) || 0));
  if (total < 60) return `${total}s`;
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m ${total % 60}s`;
}

function formatExpiry(epochSeconds) {
  if (!epochSeconds) return { text: '未知', cls: 'plain' };
  const diff = epochSeconds - Math.floor(Date.now() / 1000);
  if (diff <= 0) return { text: `已过期 ${formatDuration(-diff)}`, cls: 'danger' };
  if (diff < 3600) return { text: `${formatDuration(diff)} 后`, cls: diff < 600 ? 'danger' : 'warn' };
  return { text: formatDuration(diff), cls: 'ok' };
}

function formatClock(iso) {
  if (!iso) return '—';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '—';
  return date.toLocaleString('zh-CN', { hour12: false });
}

function formatEpochClock(epochSeconds) {
  if (!epochSeconds) return '—';
  return new Date(epochSeconds * 1000).toLocaleString('zh-CN', { hour12: false });
}

/* ---------- transport ---------- */

class ApiError extends Error {
  constructor(message, status, code) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function api(path, options = {}) {
  const { method = 'GET', body } = options;
  const init = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  if (method !== 'GET' && state.csrf) init.headers['X-CSRF-Token'] = state.csrf;
  const response = await fetch('/admin/api' + path, init);
  if (response.status === 401) {
    showLogin('登录已过期，请重新登录');
    throw new ApiError('登录已过期', 401, 'authentication_required');
  }
  let payload = null;
  const text = await response.text();
  if (text) {
    try { payload = JSON.parse(text); } catch { payload = { raw: text }; }
  }
  if (!response.ok) {
    const message = payload && payload.error ? payload.error.message : response.statusText;
    const code = payload && payload.error ? payload.error.code : '';
    throw new ApiError(message || `HTTP ${response.status}`, response.status, code);
  }
  return payload;
}

/* ---------- toast / modal ---------- */

function toast(message, kind = 'ok') {
  const node = document.createElement('div');
  node.className = `toast ${kind}`;
  node.textContent = message;
  $('#toasts').appendChild(node);
  setTimeout(() => {
    node.style.opacity = '0';
    setTimeout(() => node.remove(), 220);
  }, 3200);
}

let modalResolve = null;

function confirmDialog(title, body, confirmLabel = '确认') {
  $('#modal-title').textContent = title;
  $('#modal-body').textContent = body;
  $('#modal-confirm').textContent = confirmLabel;
  $('#modal').classList.remove('hidden');
  return new Promise((resolve) => { modalResolve = resolve; });
}

function closeModal(result) {
  $('#modal').classList.add('hidden');
  if (modalResolve) {
    const resolve = modalResolve;
    modalResolve = null;
    resolve(result);
  }
}

/* ---------- theme ---------- */

function applyTheme() {
  document.documentElement.dataset.theme = state.theme;
  const button = $('#theme-toggle');
  if (button) button.textContent = state.theme === 'dark' ? '浅色' : '深色';
}

function toggleTheme() {
  state.theme = state.theme === 'dark' ? 'light' : 'dark';
  localStorage.setItem('qw-theme', state.theme);
  applyTheme();
}

/* ---------- shell ---------- */

function showLogin(message) {
  stopTimers();
  state.csrf = null;
  state.overview = null;
  state.login = null;
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  $('#login-error').textContent = message || '';
  const input = $('#login-password');
  if (input) {
    input.value = '';
    setTimeout(() => input.focus(), 30);
  }
}

async function showApp() {
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  const target = (location.hash || '#/overview').replace(/^#\/?/, '') || 'overview';
  await navigate(PAGES[target] ? target : 'overview', { replace: true });
}

function stopTimers() {
  if (state.refreshTimer) { clearInterval(state.refreshTimer); state.refreshTimer = null; }
  if (state.loginTimer) { clearInterval(state.loginTimer); state.loginTimer = null; }
}

async function navigate(page, options = {}) {
  if (!PAGES[page]) page = 'overview';
  state.page = page;
  if (!options.replace && location.hash !== `#/${page}`) location.hash = `#/${page}`;
  $$('#nav button').forEach((button) => {
    button.classList.toggle('active', button.dataset.page === page);
  });
  $('#page-title').textContent = PAGES[page].title;
  $('#page-eyebrow').textContent = PAGES[page].eyebrow;
  setTopActions([]);
  stopTimers();
  content().innerHTML = '<div class="card"><div class="empty">加载中…</div></div>';
  try {
    await PAGES[page].load();
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) return;
    content().innerHTML = `<div class="card"><div class="empty">加载失败：${esc(error.message)}</div></div>`;
  }
}

function setTopActions(actions) {
  const host = $('#topbar-actions');
  host.innerHTML = actions
    .map((action, index) => `<button type="button" class="btn ${action.cls || 'ghost'} sm" data-top="${index}">${esc(action.label)}</button>`)
    .join('');
  $$('[data-top]', host).forEach((button) => {
    button.addEventListener('click', () => actions[Number(button.dataset.top)].onClick(button));
  });
}

function statCard(label, value, sub, tone = '') {
  return `<div class="card tight"><div class="stat ${tone}">
    <div class="label">${esc(label)}</div>
    <div class="value">${esc(value)}</div>
    <div class="sub">${esc(sub)}</div>
  </div></div>`;
}

function kvList(pairs) {
  return `<dl class="kv">${pairs.map(([key, value]) => `<dt>${esc(key)}</dt><dd>${value}</dd>`).join('')}</dl>`;
}

function badge(text, tone = '') {
  return `<span class="badge ${tone}">${esc(text)}</span>`;
}

function bindCopy(root = document) {
  $$('[data-copy]', root).forEach((button) => {
    button.addEventListener('click', async () => {
      const value = button.dataset.copy;
      try {
        await navigator.clipboard.writeText(value);
        toast('已复制');
      } catch {
        toast('复制失败，请手动选择文本', 'err');
      }
    });
  });
}

/* ---------- overview ---------- */

async function pageOverview() {
  const [overview, endpoints] = await Promise.all([api('/overview'), api('/endpoints')]);
  state.overview = overview;
  state.endpoints = endpoints;
  renderOverview();
  state.refreshTimer = setInterval(async () => {
    if (state.page !== 'overview' || document.hidden) return;
    try {
      const fresh = await api('/overview');
      state.overview = fresh;
      renderOverview();
    } catch { /* keep the last good render */ }
  }, 5000);
}

function renderOverview() {
  const overview = state.overview;
  const endpoints = state.endpoints;
  if (!overview) return;
  const revision = $('#revision');
  if (revision) revision.textContent = 'rev ' + (overview.revision || 'dev');
  const accounts = overview.accounts || {};
  const usage = overview.usage || {};
  const settings = overview.settings || {};
  const successRate = usage.requests ? Math.round((usage.ok / usage.requests) * 100) : null;
  const accountTone = accounts.enabled ? 'accent' : 'danger';
  const rateTone = successRate === null ? '' : successRate >= 95 ? 'accent' : successRate >= 80 ? 'warn' : 'danger';

  setTopActions([
    { label: '刷新', onClick: () => pageOverview().catch((error) => toast(error.message, 'err')) },
    { label: '新增账号', cls: 'primary', onClick: () => navigate('login') },
  ]);

  content().innerHTML = `
    <div class="page">
      <div class="grid cols-4">
        ${statCard('可用账号', accounts.ready ?? 0, `共 ${accounts.total ?? 0} 个 · ${accounts.expiring ?? 0} 个即将到期`, accountTone)}
        ${statCard('请求数', formatNumber(usage.requests), `成功 ${usage.ok ?? 0} · 失败 ${usage.failed ?? 0}`, '')}
        ${statCard('成功率', successRate === null ? '—' : `${successRate}%`, `空响应 ${usage.empty ?? 0} 次`, rateTone)}
        ${statCard('Token 合计', formatNumber(usage.total_tokens), `prompt ${formatNumber(usage.prompt_tokens)} · completion ${formatNumber(usage.completion_tokens)}`, '')}
      </div>

      <div class="grid side">
        <div class="card">
          <div class="card-head"><h2>运行状态</h2>${badge('online', 'ok')}</div>
          ${kvList([
            ['服务', esc(overview.service) + ' · ' + esc(overview.revision)],
            ['监听', `<code>${esc(overview.listen)}</code>`],
            ['运行时长', esc(formatDuration(overview.uptime_seconds))],
            ['配置文件', `<span class="truncate" title="${esc(overview.paths.config)}">${esc(overview.paths.config)}</span>`],
            ['数据库', `<span class="truncate" title="${esc(overview.paths.database)}">${esc(overview.paths.database)}</span>`],
            ['默认模型', `<code>${esc(overview.default_model)}</code>（${esc((overview.models || []).join(' / '))}）`],
            ['API 鉴权', overview.auth.api_key_required ? badge(`${overview.auth.api_key_count} 个 key`, 'ok') : badge('未启用（本机开放）', 'warn')],
            ['会话复用', settings.session_reuse ? `开启 · TTL ${esc(settings.session_ttl)}` : '关闭'],
            ['请求超时', esc(settings.request_timeout) + ' · 最多 ' + esc(settings.max_attempts) + ' 次换号'],
            ['thinking', esc(settings.thinking_effort)],
            ['agent', esc(settings.chat_agent_id)],
          ])}
        </div>

        <div class="card">
          <div class="card-head"><h2>账号健康</h2><span class="hint">${esc(overview.oauth.provider)} · ${esc(overview.oauth.build_stage)}</span></div>
          ${kvList([
            ['就绪', String(accounts.ready ?? 0)],
            ['即将到期', String(accounts.expiring ?? 0)],
            ['已过期', String(accounts.expired ?? 0)],
            ['已停用', String(accounts.disabled ?? 0)],
            ['最近报错', String(accounts.errored ?? 0)],
            ['刷新窗口', esc(settings.refresh_before)],
            ['OAuth 回调端口', String(overview.oauth.callback_port)],
            ['登录超时', esc(overview.oauth.timeout)],
          ])}
          <div class="banner" style="margin-top:14px">
            <span>${accounts.ready ? '账号轮询正常，请求会按 id 顺序轮转。' : '还没有可用账号，先去「登录上游」添加一个。'}</span>
          </div>
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h2>接入信息</h2>
          <div class="chips">
            <button class="btn ghost sm" type="button" data-copy="${esc(endpoints.base_url)}">复制 Base URL</button>
            <button class="btn ghost sm" type="button" data-copy="${esc(endpoints.curl)}">复制 curl</button>
            <button class="btn ghost sm" type="button" data-copy="${esc(endpoints.python)}">复制 Python</button>
          </div>
        </div>
        ${kvList([
          ['Base URL', `<code>${esc(endpoints.base_url)}</code>`],
          ['Models', `<code>${esc(endpoints.models_url)}</code>`],
          ['Chat', `<code>${esc(endpoints.chat_url)}</code>`],
          ['Health', `<code>${esc(endpoints.health_url)}</code>`],
        ])}
        <div class="code" style="margin-top:14px"><pre>${esc(endpoints.curl)}</pre></div>
      </div>

      <div class="card">
        <div class="card-head"><h2>生效配置</h2><span class="hint">与 config.json 一致，改动请到「配置」页</span></div>
        <div class="grid cols-3" style="margin-top:4px">
          ${[
            ['时区 / 语言', `${settings.timezone} · ${settings.locale}`],
            ['appVersion', settings.app_version],
            ['surface', settings.surface],
            ['keep_sessions', settings.keep_sessions ? 'true' : 'false'],
            ['管理界面 TTL', overview.admin.session_ttl],
            ['密码存储', overview.admin.password_source === 'hash' ? 'pbkdf2 哈希' : '明文'],
          ].map(([key, value]) => kvList([[key, esc(value)]])).join('')}
        </div>
      </div>
    </div>`;

  bindCopy(content());
}

/* ---------- accounts ---------- */

async function pageAccounts() {
  const [data, proxyData] = await Promise.all([
    api('/accounts'),
    api('/proxies').catch(() => ({ proxies: [] })),
  ]);
  state.accounts = data.accounts || [];
  state.proxies = proxyData.proxies || [];
  renderAccounts();
  state.refreshTimer = setInterval(async () => {
    if (state.page !== 'accounts' || document.hidden) return;
    try {
      const fresh = await api('/accounts');
      state.accounts = fresh.accounts || [];
      renderAccounts();
    } catch { /* ignore */ }
  }, 8000);
}

function statusBadge(status) {
  const map = {
    ready: ['ok', 'ready'],
    expiring: ['warn', 'expiring'],
    expired: ['danger', 'expired'],
    disabled: ['plain', 'disabled'],
    unknown: ['info', 'no token'],
  };
  const [tone, label] = map[status] || ['plain', status];
  return badge(label, tone);
}

function renderAccounts() {
  setTopActions([
    { label: '刷新全部', onClick: (button) => refreshAllAccounts(button) },
    { label: '导出账号', onClick: () => exportAccounts() },
    { label: '导入账号', onClick: () => importAccounts() },
    { label: '新增账号', cls: 'primary', onClick: () => navigate('login') },
  ]);

  const rows = state.accounts.map((account) => {
    const expiry = formatExpiry(account.expires_at);
    return `<tr>
      <td class="mono">${account.id}</td>
      <td>
        <div class="cell-main truncate" title="${esc(account.label)}">${esc(account.label)}</div>
        <div class="cell-sub">${esc(account.provider)}${account.build_stage ? ' · ' + esc(account.build_stage) : ''}${account.has_refresh ? '' : ' · 无 refresh'}</div>
      </td>
      <td class="mono">${esc(account.region || '—')}</td>
      <td>${statusBadge(account.status)}</td>
      <td>${badge(expiry.text, expiry.cls)}<div class="cell-sub">${esc(formatEpochClock(account.expires_at))}</div></td>
      <td class="mono">${formatNumber(account.use_count)}</td>
      <td class="cell-sub truncate" title="${esc(account.last_error)}">${esc(account.last_error || '—')}</td>
      <td><select class="proxy-select" data-proxy-select data-account="${account.id}">${proxyOptions(account)}</select></td>
      <td class="actions">
        <button class="btn ghost sm" type="button" data-action="refresh" data-id="${account.id}">刷新</button>
        <button class="btn ghost sm" type="button" data-action="toggle" data-id="${account.id}" data-disabled="${account.status === 'disabled' ? '1' : '0'}">${account.status === 'disabled' ? '启用' : '停用'}</button>
        <button class="btn danger sm" type="button" data-action="delete" data-id="${account.id}" data-label="${esc(account.label)}">删除</button>
      </td>
    </tr>`;
  }).join('');

  content().innerHTML = `
    <div class="page">
      <div class="card">
        <div class="card-head">
          <h2>账号轮询</h2>
          <span class="hint">${state.accounts.length} 个账号 · 同一 email + tenant 重复登录会更新而不是新增</span>
        </div>
        ${state.accounts.length ? `<div class="table-wrap"><table>
          <thead><tr><th>ID</th><th>账号</th><th>区域</th><th>状态</th><th>到期</th><th>使用</th><th>最近错误</th><th>出站代理</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table></div>` : '<div class="empty">还没有账号<br /><span class="faint">运行 quickproxy login，或在「登录上游」里走一遍 OAuth</span></div>'}
      </div>
    </div>`;

  $$('[data-proxy-select]', content()).forEach((select) => {
    select.addEventListener('change', async () => {
      const id = Number(select.dataset.account);
      const raw = select.value;
      const mode = raw.startsWith('bind:') ? 'bind' : raw;
      const proxyID = mode === 'bind' ? Number(raw.split(':')[1]) : 0;
      select.disabled = true;
      try {
        await api(`/accounts/${id}/proxy`, { method: 'POST', body: { mode, proxy_id: proxyID } });
        const labels = {
          direct: '账号改为直连',
          auto: '账号改为自动绑定（固定出口 + 故障转移）',
          rotate: '账号改为自动轮询（每次请求换出口）',
        };
        toast(labels[mode] || `账号已绑定代理 #${proxyID}`);
        await pageAccounts();
      } catch (error) {
        toast(error.message, 'err');
        select.disabled = false;
      }
    });
  });

  $$('[data-action]', content()).forEach((button) => {
    button.addEventListener('click', async () => {
      const id = Number(button.dataset.id);
      if (button.dataset.action === 'refresh') {
        await refreshAccount(id, button);
      } else if (button.dataset.action === 'toggle') {
        await toggleAccount(id, button.dataset.disabled === '1', button);
      } else {
        const label = button.dataset.label;
        const ok = await confirmDialog('删除账号', `确认删除「${label}」（id ${id}）？该账号的 token 会从本地 SQLite 移除。`, '删除');
        if (!ok) return;
        try {
          await api(`/accounts/${id}`, { method: 'DELETE' });
          toast('账号已删除');
          await pageAccounts();
        } catch (error) {
          toast(error.message, 'err');
        }
      }
    });
  });
}

async function toggleAccount(id, enable, button) {
  const original = button.textContent;
  button.disabled = true;
  try {
    await api(`/accounts/${id}/${enable ? 'enable' : 'disable'}`, { method: 'POST', body: {} });
    toast(enable ? '账号已启用，重新加入轮询' : '账号已停用，不再参与轮询和自动刷新');
    await pageAccounts();
  } catch (error) {
    toast(error.message, 'err');
    button.disabled = false;
    button.textContent = original;
  }
}

async function exportAccounts() {
  try {
    const response = await fetch('/admin/api/accounts/export', { credentials: 'same-origin' });
    if (response.status === 401) {
      showLogin('登录已过期，请重新登录');
      return;
    }
    if (!response.ok) {
      const detail = await response.json().catch(() => null);
      throw new Error((detail && detail.error && detail.error.message) || `HTTP ${response.status}`);
    }
    const blob = await response.blob();
    const stamp = new Date().toISOString().slice(0, 19).replace(/[-:T]/g, '');
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = `quickwork-accounts-${stamp}.json`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
    toast('已导出全部账号（含 token，请妥善保管）');
  } catch (error) {
    toast(error.message, 'err');
  }
}

function importAccounts() {
  const input = document.createElement('input');
  input.type = 'file';
  input.accept = 'application/json,.json';
  input.addEventListener('change', async () => {
    const file = input.files && input.files[0];
    if (!file) return;
    try {
      const text = await file.text();
      let doc;
      try {
        doc = JSON.parse(text);
      } catch (error) {
        throw new Error('不是有效的 JSON 文件');
      }
      const list = Array.isArray(doc) ? doc : doc && doc.accounts;
      if (!Array.isArray(list) || list.length === 0) throw new Error('文件里没有 accounts 数组');
      const ok = await confirmDialog(
        '增量导入账号',
        `将要导入 ${list.length} 个账号。已存在（同 email + tenant）的账号会跳过，不会覆盖本地 token；新增的会立即加入轮询。`,
        '导入',
      );
      if (!ok) return;
      const result = await api('/accounts/import', {
        method: 'POST',
        body: { version: (doc && doc.version) || 1, accounts: list },
      });
      const failures = (result.errors || []).length;
      toast(`导入完成：新增 ${result.imported}，跳过 ${result.skipped}，失败 ${failures}`, failures ? 'err' : 'ok');
      if (failures) console.warn('import errors', result.errors);
      await pageAccounts();
    } catch (error) {
      toast(error.message, 'err');
    }
  });
  input.click();
}

async function refreshAccount(id, button) {
  const original = button.textContent;
  button.disabled = true;
  button.textContent = '刷新中…';
  try {
    const result = await api('/accounts/refresh', { method: 'POST', body: { id } });
    const failed = (result.results || []).find((item) => item.error);
    if (failed) throw new Error(failed.error);
    toast('token 已刷新');
    await pageAccounts();
  } catch (error) {
    toast(error.message, 'err');
    button.disabled = false;
    button.textContent = original;
  }
}

async function refreshAllAccounts(button) {
  const original = button.textContent;
  button.disabled = true;
  button.textContent = '刷新中…';
  try {
    const result = await api('/accounts/refresh', { method: 'POST', body: {} });
    toast(`刷新完成：${result.refreshed} 成功 / ${result.failed} 失败`, result.failed ? 'err' : 'ok');
    await pageAccounts();
  } catch (error) {
    toast(error.message, 'err');
  } finally {
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = original;
    }
  }
}

function proxyOptions(account) {
  const mode = account.proxy_mode || 'auto';
  const current = mode === 'bind' ? `bind:${account.proxy_id}` : mode;
  const selectable = (state.proxies || []).filter((proxy) => proxy.enabled || proxy.id === account.proxy_id);
  const options = [
    { value: 'direct', label: '直连（不走代理）' },
    { value: 'auto', label: '自动绑定（固定出口 + 故障转移）' },
    { value: 'rotate', label: '自动轮询（每次请求换出口）' },
  ].concat(selectable.map((proxy) => ({
    value: `bind:${proxy.id}`,
    label: `指定 #${proxy.id} ${proxy.label || proxy.url}${proxy.enabled ? '' : '（已停用）'}`,
  })));
  return options
    .map((option) => `<option value="${esc(option.value)}" ${current === option.value ? 'selected' : ''}>${esc(option.label)}</option>`)
    .join('');
}

/* ---------- proxy pool ---------- */

async function pageProxies() {
  setTopActions([
    { label: '测试全部', onClick: (button) => testAllProxies(button) },
  ]);
  const data = await api('/proxies');
  state.proxies = data.proxies || [];
  renderProxies(data);
}

function proxyStatus(proxy) {
  if (!proxy.enabled) return badge('已停用', 'plain');
  if (!proxy.last_checked) return badge('未测试', 'info');
  return proxy.healthy ? badge(`正常 · ${proxy.latency_ms}ms`, 'ok') : badge('连接失败', 'danger');
}

function renderProxies(data) {
  const pool = data.pool || {};
  const rows = state.proxies.map((proxy) => `<tr>
      <td><input type="checkbox" class="proxy-pick" data-id="${proxy.id}" aria-label="选择代理 ${proxy.id}" /></td>
      <td class="mono">${proxy.id}</td>
      <td>
        <div class="cell-main truncate" title="${esc(proxy.url)}">${esc(proxy.label || proxy.url)}</div>
        ${proxy.label ? `<div class="cell-sub truncate" title="${esc(proxy.url)}">${esc(proxy.url)}</div>` : ''}
      </td>
      <td>${proxyStatus(proxy)}</td>
      <td class="mono">${proxy.exit_ip ? esc(proxy.exit_ip) : '—'}</td>
      <td class="mono">${proxy.bound_accounts || 0}</td>
      <td class="cell-sub truncate" title="${esc(proxy.last_error)}">${esc(proxy.last_error || '—')}</td>
      <td class="actions">
        <button class="btn ghost sm" type="button" data-proxy-action="test" data-id="${proxy.id}">测试</button>
        <button class="btn ghost sm" type="button" data-proxy-action="${proxy.enabled ? 'disable' : 'enable'}" data-id="${proxy.id}">${proxy.enabled ? '停用' : '启用'}</button>
        <button class="btn danger sm" type="button" data-proxy-action="delete" data-id="${proxy.id}">删除</button>
      </td>
    </tr>`).join('');

  content().innerHTML = `
    <div class="page">
      <div class="card">
        <div class="card-head">
          <h2>代理池</h2>
          <span class="hint">${state.proxies.length} 个代理 · 代理池${pool.enabled ? '已启用' : '已停用（跟随池的账号按直连处理）'} · 测试地址 ${esc(pool.test_url || '')}</span>
        </div>
        ${state.proxies.length ? `
        <div class="bulk-bar" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:12px">
          <button class="btn ghost sm" type="button" id="proxy-select-all">全选</button>
          <button class="btn ghost sm" type="button" id="proxy-clear-select">取消全选</button>
          <button class="btn ghost sm" type="button" id="proxy-test-selected">⚡ 测试选中</button>
          <button class="btn ghost sm" type="button" id="proxy-enable-selected">✅ 启用选中</button>
          <button class="btn ghost sm" type="button" id="proxy-disable-selected">⏸ 停用选中</button>
          <button class="btn danger sm" type="button" id="proxy-delete-selected">🗑 删除选中</button>
          <span class="hint" id="proxy-selected-count" style="margin-left:auto">已选 0</span>
        </div>
        <div class="table-wrap"><table>
          <thead><tr><th><input type="checkbox" id="proxy-select-all-box" aria-label="全选代理" /></th><th>ID</th><th>代理</th><th>状态</th><th>出口 IP</th><th>绑定账号</th><th>最近错误</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table></div>` : '<div class="empty">代理池还是空的<br /><span class="faint">支持 http / https / socks5，地址里可以带 user:pass</span></div>'}
      </div>

      <div class="card">
        <div class="card-head"><h2>添加代理</h2><span class="hint">每行一个，重复的会自动跳过；添加后可用「测试」验证出口</span></div>
        <textarea id="proxy-bulk" rows="4" placeholder="http://user:pass@1.2.3.4:8080&#10;socks5://5.6.7.8:1080" style="width:100%"></textarea>
        <div style="margin-top:12px;display:flex;gap:10px;align-items:center;flex-wrap:wrap">
          <button class="btn primary sm" id="proxy-add" type="button">添加到代理池</button>
          <input id="proxy-test-input" placeholder="测试未保存的代理，如 http://1.2.3.4:8080" style="flex:1;min-width:260px" />
          <button class="btn ghost sm" id="proxy-test-unsaved" type="button">测试</button>
        </div>
        <div id="proxy-test-result" class="hint" style="margin-top:8px"></div>
      </div>
    </div>`;

  $('#proxy-add').addEventListener('click', addProxiesFromText);
  $('#proxy-test-unsaved').addEventListener('click', testUnsavedProxy);
  $$('[data-proxy-action]', content()).forEach((button) => {
    button.addEventListener('click', () => proxyRowAction(button));
  });

  const boxes = $$('.proxy-pick', content());
  boxes.forEach((box) => box.addEventListener('change', updateProxySelection));
  const headerBox = $('#proxy-select-all-box');
  if (headerBox) {
    headerBox.addEventListener('change', () => {
      boxes.forEach((box) => { box.checked = headerBox.checked; });
      updateProxySelection();
    });
  }
  const selectAll = () => {
    boxes.forEach((box) => { box.checked = true; });
    if (headerBox) headerBox.checked = true;
    updateProxySelection();
  };
  const clearAll = () => {
    boxes.forEach((box) => { box.checked = false; });
    if (headerBox) headerBox.checked = false;
    updateProxySelection();
  };
  // The bulk toolbar only exists while the pool has rows; bind defensively so
  // an empty pool still renders instead of failing on a missing element.
  const onBulk = (id, handler) => {
    const button = $(id);
    if (button) button.addEventListener('click', handler);
  };
  onBulk('#proxy-select-all', selectAll);
  onBulk('#proxy-clear-select', clearAll);
  onBulk('#proxy-test-selected', (event) => bulkProxyAction('test', event.currentTarget));
  onBulk('#proxy-enable-selected', (event) => bulkProxyAction('enable', event.currentTarget));
  onBulk('#proxy-disable-selected', (event) => bulkProxyAction('disable', event.currentTarget));
  onBulk('#proxy-delete-selected', (event) => bulkProxyAction('delete', event.currentTarget));
  updateProxySelection();
}

function selectedProxyIDs() {
  return $$('.proxy-pick', content())
    .filter((box) => box.checked)
    .map((box) => Number(box.dataset.id))
    .filter((id) => id > 0);
}

function updateProxySelection() {
  const count = selectedProxyIDs().length;
  const label = $('#proxy-selected-count');
  if (label) label.textContent = `已选 ${count}`;
  const headerBox = $('#proxy-select-all-box');
  const boxes = $$('.proxy-pick', content());
  if (headerBox && boxes.length) {
    headerBox.checked = count === boxes.length;
    headerBox.indeterminate = count > 0 && count < boxes.length;
  }
}

async function bulkProxyAction(action, button) {
  const ids = selectedProxyIDs();
  if (ids.length === 0) {
    toast('先勾选要操作的代理', 'err');
    return;
  }
  const labels = { test: '测试', enable: '启用', disable: '停用', delete: '删除' };
  if (action === 'delete') {
    const ok = await confirmDialog('删除选中的代理', `确认删除 ${ids.length} 个代理？绑定它们的账号会自动改为自动轮询。`, '删除');
    if (!ok) return;
  }
  const original = button.textContent;
  button.disabled = true;
  button.textContent = `${labels[action]}中…`;
  try {
    const data = await api('/proxies/actions', { method: 'POST', body: { action, ids } });
    const failed = (data.results || []).filter((item) => !item.ok || item.error);
    if (action === 'test') {
      const healthy = (data.results || []).filter((item) => item.ok && item.healthy).length;
      toast(`测试完成：${healthy} 个可用 / ${ids.length - healthy} 个异常`, healthy === ids.length ? 'ok' : 'err');
      if (failed.length) console.warn('proxy test failures', failed);
    } else {
      toast(`${labels[action]}完成：${data.ok} 个成功${data.failed ? `，${data.failed} 个失败` : ''}`, data.failed ? 'err' : 'ok');
    }
    await pageProxies();
  } catch (error) {
    toast(error.message, 'err');
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = original;
    }
  }
}

async function addProxiesFromText() {
  const text = $('#proxy-bulk').value.trim();
  if (!text) {
    toast('先填入至少一个代理', 'err');
    return;
  }
  try {
    const result = await api('/proxies/bulk', { method: 'POST', body: { text } });
    const failed = (result.failures || []).length;
    toast(`已添加 ${result.added} 个，跳过 ${result.skipped} 个${failed ? `，失败 ${failed} 个` : ''}`, failed ? 'err' : 'ok');
    if (failed) console.warn('proxy add failures', result.failures);
    await pageProxies();
  } catch (error) {
    toast(error.message, 'err');
  }
}

async function testUnsavedProxy() {
  const url = $('#proxy-test-input').value.trim();
  if (!url) {
    toast('填入代理地址再测试', 'err');
    return;
  }
  const button = $('#proxy-test-unsaved');
  button.disabled = true;
  button.textContent = '测试中…';
  try {
    const data = await api('/proxies/test', { method: 'POST', body: { url } });
    const result = data.result || {};
    $('#proxy-test-result').textContent = result.healthy
      ? `通过 · ${result.latency_ms}ms · 出口 IP ${result.exit_ip || '—'}`
      : `失败 · ${result.error || 'unknown'}`;
    toast(result.healthy ? '代理可用' : '代理不可用', result.healthy ? 'ok' : 'err');
  } catch (error) {
    toast(error.message, 'err');
  } finally {
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = '测试';
    }
  }
}

async function proxyRowAction(button) {
  const id = Number(button.dataset.id);
  const action = button.dataset.proxyAction;
  try {
    if (action === 'delete') {
      const ok = await confirmDialog('删除代理', `确认删除代理 #${id}？绑定它的账号会自动改为「跟随代理池」。`, '删除');
      if (!ok) return;
      await api(`/proxies/${id}`, { method: 'DELETE' });
      toast('代理已删除');
    } else if (action === 'test') {
      const original = button.textContent;
      button.disabled = true;
      button.textContent = '测试中…';
      const data = await api(`/proxies/${id}/test`, { method: 'POST', body: {} });
      const result = data.result || {};
      toast(
        result.healthy
          ? `代理 #${id} 可用 · ${result.latency_ms}ms · ${result.exit_ip || '—'}`
          : `代理 #${id} 不可用：${result.error || 'unknown'}`,
        result.healthy ? 'ok' : 'err',
      );
      button.disabled = false;
      button.textContent = original;
    } else {
      await api(`/proxies/${id}/${action}`, { method: 'POST', body: {} });
      toast(action === 'enable' ? '代理已启用' : '代理已停用');
    }
    await pageProxies();
  } catch (error) {
    toast(error.message, 'err');
  }
}

async function testAllProxies(button) {
  const original = button.textContent;
  button.disabled = true;
  let ok = 0;
  let failed = 0;
  try {
    const targets = state.proxies.filter((proxy) => proxy.enabled);
    for (let i = 0; i < targets.length; i += 1) {
      button.textContent = `测试中 ${i + 1}/${targets.length}…`;
      try {
        const data = await api(`/proxies/${targets[i].id}/test`, { method: 'POST', body: {} });
        if ((data.result || {}).healthy) ok += 1;
        else failed += 1;
      } catch {
        failed += 1;
      }
    }
    toast(`测试完成：${ok} 个可用 / ${failed} 个失败`, failed ? 'err' : 'ok');
    await pageProxies();
  } finally {
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = original;
    }
  }
}

/* ---------- upstream login ---------- */

async function pageLogin() {
  const overview = state.overview || await api('/overview');
  state.overview = overview;
  const oauth = overview.oauth || {};
  renderLoginPage(oauth, Boolean(overview.runtime && overview.runtime.in_container));
}

function renderLoginPage(oauth, inContainer) {
  setTopActions([]);
  const login = state.login;
  content().innerHTML = `
    <div class="page">
      <div class="card">
        <div class="card-head"><h2>启动一次浏览器登录</h2><span class="hint">凭据只写入本地 SQLite</span></div>
        ${inContainer ? '<div class="banner warn" style="margin-bottom:14px">当前进程运行在容器里：宿主机浏览器无法回调容器内的 <code>127.0.0.1:18080</code>，请用「手动回填 callback URL」模式。</div>' : ''}
        <div class="form-grid">
          <label class="field">
            <span>provider</span>
            <select id="login-provider">
              ${['social', 'enterprise', 'gateway', 'social_gateway'].map((value) =>
                `<option value="${value}" ${value === oauth.provider ? 'selected' : ''}>${value}</option>`).join('')}
            </select>
          </label>
          <label class="field">
            <span>stage</span>
            <select id="login-stage">
              ${['prod', 'beta', 'integ', 'dev'].map((value) =>
                `<option value="${value}" ${value === oauth.build_stage ? 'selected' : ''}>${value}</option>`).join('')}
            </select>
          </label>
          <label class="field">
            <span>回调方式</span>
            <select id="login-mode">
              <option value="manual" selected>手动回填 callback URL（服务器部署）</option>
              <option value="local">本机 127.0.0.1:${esc(oauth.callback_port)} 自动回调${inContainer ? '（容器内不可用）' : ''}</option>
            </select>
          </label>
          <div class="field">
            <span>&nbsp;</span>
            <button class="btn primary" type="button" id="login-start">开始登录</button>
          </div>
        </div>
        <div class="section-note" style="margin-top:10px">
          手动模式：终端里那套流程原样搬到了网页——打开 discovery URL，登录后把地址栏里 <code>callback?...</code> 或 <code>?...code=...</code> 的完整 URL 粘回来。
          本机模式：浏览器必须和本服务在同一台机器，回调端口 <code>${esc(oauth.callback_port)}</code> 需空闲。
        </div>
      </div>
      <div id="login-panel">${login ? '' : '<div class="card"><div class="empty">还没有进行中的登录</div></div>'}</div>
    </div>`;

  $('#login-start').addEventListener('click', startLogin);
  if (login) renderLoginPanel();
}

async function startLogin() {
  const button = $('#login-start');
  button.disabled = true;
  button.textContent = '启动中…';
  try {
    const result = await api('/accounts/login', {
      method: 'POST',
      body: {
        provider: $('#login-provider').value,
        stage: $('#login-stage').value,
        mode: $('#login-mode').value,
      },
    });
    state.login = result.login;
    renderLoginPanel();
    startLoginPolling();
  } catch (error) {
    toast(error.message, 'err');
  } finally {
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = '开始登录';
    }
  }
}

function loginStatusBadge(status) {
  const map = { running: ['info', 'running'], done: ['ok', 'done'], failed: ['danger', 'failed'], cancelled: ['plain', 'cancelled'] };
  const [tone, label] = map[status] || ['plain', status];
  return badge(label, tone);
}

function renderLoginPanel() {
  const login = state.login;
  const host = $('#login-panel');
  if (!host) return;
  const previousInput = $('#login-callback');
  const typed = previousInput ? previousInput.value : '';
  const focused = previousInput && document.activeElement === previousInput;
  const previousLog = $('#login-log');
  const pinned = !previousLog || previousLog.scrollHeight - previousLog.scrollTop - previousLog.clientHeight < 48;
  if (!login) {
    host.innerHTML = '<div class="card"><div class="empty">还没有进行中的登录</div></div>';
    return;
  }
  const urls = (login.urls || []).map((url, index) =>
    `<a class="chip" href="${esc(url)}" target="_blank" rel="noreferrer noopener">${index === 0 ? '打开登录页' : '打开授权页'} · ${esc(url.length > 54 ? url.slice(0, 54) + '…' : url)}</a>`).join('');
  host.innerHTML = `
    <div class="card">
      <div class="card-head">
        <h2>登录进度 ${loginStatusBadge(login.status)}</h2>
        <span class="hint">${esc(login.mode)} · ${esc(login.provider)}/${esc(login.stage)} · ${formatDuration(login.elapsed_seconds)}</span>
      </div>
      <div class="banner ${login.status === 'done' ? 'ok' : ''}">${esc(login.hint)}</div>
      ${urls ? `<div class="chips" style="margin-top:12px">${urls}</div>` : ''}
      <pre class="log-view" id="login-log" style="margin-top:12px">${linkify(login.text || '等待上游输出…')}</pre>
      ${login.mode === 'manual' && login.status === 'running' ? `
        <div class="form-row" style="margin-top:12px">
          <input type="text" id="login-callback" placeholder="粘贴浏览器地址栏里的完整 callback URL" />
          <button class="btn primary" type="button" id="login-submit-url">提交 URL</button>
        </div>` : ''}
      ${login.status === 'done' ? `
        <div class="banner ok" style="margin-top:12px">登录完成：${esc(login.account || '')}（id ${esc(login.account_id)}）</div>
        <div class="form-row" style="margin-top:12px">
          <button class="btn primary" type="button" id="login-goto-accounts">查看账号</button>
          <button class="btn ghost" type="button" id="login-reset">再登录一个</button>
        </div>` : ''}
      ${login.status === 'running' ? `
        <div class="form-row" style="margin-top:12px">
          <button class="btn danger sm" type="button" id="login-cancel">取消登录</button>
        </div>` : ''}
      ${login.status === 'failed' ? `
        <div class="banner warn" style="margin-top:12px">失败：${esc(login.error || 'unknown error')}</div>
        <div class="form-row" style="margin-top:12px"><button class="btn ghost" type="button" id="login-reset">重新开始</button></div>` : ''}
    </div>`;

  const autoScroll = () => {
    const log = $('#login-log');
    if (log) log.scrollTop = log.scrollHeight;
  };
  if (pinned) autoScroll();
  if (typed) {
    const input = $('#login-callback');
    if (input) input.value = typed;
  }
  if (focused) {
    const input = $('#login-callback');
    if (input) {
      input.focus();
      input.setSelectionRange(input.value.length, input.value.length);
    }
  }

  const submit = $('#login-submit-url');
  if (submit) {
    const input = $('#login-callback');
    const send = async () => {
      const value = input.value.trim();
      if (!value) return;
      submit.disabled = true;
      try {
        await api(`/accounts/login/${state.login.id}/callback`, { method: 'POST', body: { url: value } });
        input.value = '';
        toast('已提交，等待上游处理');
        await pollLogin();
      } catch (error) {
        toast(error.message, 'err');
      } finally {
        submit.disabled = false;
      }
    };
    submit.addEventListener('click', send);
    input.addEventListener('keydown', (event) => { if (event.key === 'Enter') send(); });
  }
  const cancel = $('#login-cancel');
  if (cancel) {
    cancel.addEventListener('click', async () => {
      try {
        await api(`/accounts/login/${state.login.id}`, { method: 'DELETE' });
        await pollLogin();
      } catch (error) {
        toast(error.message, 'err');
      }
    });
  }
  const goto = $('#login-goto-accounts');
  if (goto) goto.addEventListener('click', () => navigate('accounts'));
  const reset = $('#login-reset');
  if (reset) {
    reset.addEventListener('click', () => {
      state.login = null;
      stopTimers();
      renderLoginPanel();
      $('#login-start').disabled = false;
    });
  }
}

function startLoginPolling() {
  if (state.loginTimer) clearInterval(state.loginTimer);
  state.loginTimer = setInterval(pollLogin, 1200);
}

async function pollLogin() {
  if (!state.login) return;
  try {
    const result = await api(`/accounts/login/${state.login.id}`);
    const previousStatus = state.login.status;
    state.login = result.login;
    if (state.page === 'login') renderLoginPanel();
    if (state.login.status !== 'running') {
      if (state.loginTimer) { clearInterval(state.loginTimer); state.loginTimer = null; }
      if (previousStatus === 'running' && state.login.status === 'done') {
        toast(`账号已加入：${state.login.account}`);
        state.overview = null;
      }
    }
  } catch (error) {
    if (state.loginTimer) { clearInterval(state.loginTimer); state.loginTimer = null; }
    if (!(error instanceof ApiError) || error.status !== 404) toast(error.message, 'err');
  }
}

/* ---------- config ---------- */

async function pageConfig() {
  const data = await api('/config');
  state.config = data;
  renderConfig();
}

function field(label, input, hint) {
  return `<label class="field"><span>${esc(label)}</span>${input}${hint ? `<span class="section-note">${esc(hint)}</span>` : ''}</label>`;
}

function renderConfig() {
  const data = state.config;
  const cfg = data.config;
  const restart = data.restart_required || [];
  const passwordSource = data.password_source === 'hash' ? 'pbkdf2 哈希' : '明文';

  setTopActions([
    { label: '保存配置', cls: 'primary', onClick: (button) => saveConfig(button) },
  ]);

  content().innerHTML = `
    <div class="page">
      ${restart.length ? `<div class="banner warn"><span><b>有改动等待重启：</b>${esc(restart.join('、'))}。这些字段在下次启动后生效，当前进程仍使用旧值。</span></div>` : ''}
      <div class="card">
        <div class="card-head"><h2>服务与模型</h2><span class="hint">addr / database 修改后必须重启</span></div>
        <div class="form-grid">
          ${field('监听地址 addr', `<input type="text" id="cfg-addr" value="${esc(cfg.addr)}" placeholder="127.0.0.1:8080" />`)}
          ${field('默认模型 default_model', `<input type="text" id="cfg-default-model" value="${esc(cfg.default_model)}" />`, '必须是 models 里的一个')}
          ${field('模型列表 models（逗号分隔）', `<input type="text" id="cfg-models" value="${esc((cfg.models || []).join(', '))}" />`)}
          ${field('请求超时 request_timeout', `<input type="text" id="cfg-request-timeout" value="${esc(cfg.request_timeout)}" />`, 'Go duration，例如 5m')}
          ${field('刷新提前量 refresh_before', `<input type="text" id="cfg-refresh-before" value="${esc(cfg.refresh_before)}" />`, 'token 到期前多久自动刷新')}
          ${field('最多换号尝试 max_attempts', `<input type="number" id="cfg-max-attempts" min="1" max="100" value="${esc(cfg.max_attempts)}" />`)}
          ${field('本地 API key（每行一个）', `<textarea id="cfg-api-keys" rows="5" placeholder="留空则关闭鉴权">${esc((cfg.api_keys || []).join('\n'))}</textarea>`, '非空时 /v1 请求必须带 Authorization: Bearer <key>')}
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2>上游会话</h2><span class="hint">会话复用决定 64 KB 单帧预算怎么花</span></div>
        <div class="form-grid">
          ${field('会话复用 session_reuse', `<select id="cfg-session-reuse">
            <option value="true" ${cfg.session_reuse ? 'selected' : ''}>开启（只发送新增消息）</option>
            <option value="false" ${!cfg.session_reuse ? 'selected' : ''}>关闭（每轮完整 transcript）</option>
          </select>`)}
          ${field('保留上游会话 keep_sessions', `<select id="cfg-keep-sessions">
            <option value="false" ${!cfg.keep_sessions ? 'selected' : ''}>false</option>
            <option value="true" ${cfg.keep_sessions ? 'selected' : ''}>true</option>
          </select>`)}
          ${field('会话内存 TTL session_ttl', `<input type="text" id="cfg-session-ttl" value="${esc(cfg.session_ttl)}" />`)}
          ${field('思考强度 thinking_effort', `<select id="cfg-thinking">
            ${['', 'off', 'low', 'medium', 'high'].map((value) =>
              `<option value="${value}" ${cfg.thinking_effort === value ? 'selected' : ''}>${value || '（服务端默认）'}</option>`).join('')}
          </select>`)}
          ${field('自定义 agent chat_agent_id', `<input type="text" id="cfg-agent" value="${esc(cfg.chat_agent_id)}" placeholder="留空使用内置 agent" />`)}
          ${field('时区 timezone', `<input type="text" id="cfg-timezone" value="${esc(cfg.timezone)}" />`, 'IANA 名称，例如 Asia/Shanghai')}
          ${field('语言 locale', `<input type="text" id="cfg-locale" value="${esc(cfg.locale)}" />`)}
          ${field('appVersion', `<input type="text" id="cfg-app-version" value="${esc(cfg.app_version)}" />`)}
          ${field('surface', `<input type="text" id="cfg-surface" value="${esc(cfg.surface)}" />`)}
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2>OAuth 默认值</h2><span class="hint">登录上游时用作预填</span></div>
        <div class="form-grid">
          ${field('provider', `<select id="cfg-oauth-provider">
            ${['social', 'enterprise', 'gateway', 'social_gateway'].map((value) =>
              `<option value="${value}" ${cfg.oauth.provider === value ? 'selected' : ''}>${value}</option>`).join('')}
          </select>`)}
          ${field('stage', `<select id="cfg-oauth-stage">
            ${['prod', 'beta', 'integ', 'dev'].map((value) =>
              `<option value="${value}" ${cfg.oauth.build_stage === value ? 'selected' : ''}>${value}</option>`).join('')}
          </select>`)}
          ${field('region', `<input type="text" id="cfg-oauth-region" value="${esc(cfg.oauth.region)}" placeholder="留空自动" />`)}
          ${field('回调端口 callback_port', `<input type="number" id="cfg-oauth-port" min="0" max="65535" value="${esc(cfg.oauth.callback_port)}" />`)}
          ${field('登录超时 timeout', `<input type="text" id="cfg-oauth-timeout" value="${esc(cfg.oauth.timeout)}" />`)}
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2>管理界面</h2><span class="hint">这部分保存后立即生效，无需重启</span></div>
        <div class="form-grid">
          ${field('启用 /admin', `<select id="cfg-webui-enabled">
            <option value="true" ${cfg.webui.enabled ? 'selected' : ''}>启用</option>
            <option value="false" ${!cfg.webui.enabled ? 'selected' : ''}>关闭（返回 503）</option>
          </select>`)}
          ${field('会话 TTL', `<input type="text" id="cfg-webui-ttl" value="${esc(cfg.webui.session_ttl)}" />`)}
          ${field('登录失败上限 / 5 分钟', `<input type="number" id="cfg-webui-attempts" min="1" max="100" value="${esc(cfg.webui.max_attempts)}" />`)}
          ${field('信任反向代理头', `<select id="cfg-webui-trust-proxy">
            <option value="false" ${!cfg.webui.trust_proxy ? 'selected' : ''}>否（用直连 IP）</option>
            <option value="true" ${cfg.webui.trust_proxy ? 'selected' : ''}>是（读取 X-Forwarded-For）</option>
          </select>`, '仅在确实挂在可信反代后面时开启')}
          ${field('设置新密码', `<input type="password" id="cfg-new-password" autocomplete="new-password" placeholder="留空则不修改（至少 8 位）" />`, `当前存储方式：${passwordSource}`)}
          <label class="field">
            <span>移除密码</span>
            <span class="check"><input type="checkbox" id="cfg-clear-password" /> 清空密码（保存后 /admin 返回 503）</span>
          </label>
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2>文件位置</h2></div>
        ${kvList([
          ['config.json', `<code>${esc(data.paths.config)}</code>`],
          ['database', `<code>${esc(data.paths.database)}</code>`],
        ])}
      </div>
    </div>`;
}

async function saveConfig(button) {
  const value = (id) => $(`#${id}`).value;
  const payload = {
    addr: value('cfg-addr').trim(),
    models: value('cfg-models').split(',').map((item) => item.trim()).filter(Boolean),
    default_model: value('cfg-default-model').trim(),
    request_timeout: value('cfg-request-timeout').trim(),
    refresh_before: value('cfg-refresh-before').trim(),
    max_attempts: Number(value('cfg-max-attempts')),
    api_keys: value('cfg-api-keys').split('\n').map((item) => item.trim()).filter(Boolean),
    keep_sessions: value('cfg-keep-sessions') === 'true',
    session_reuse: value('cfg-session-reuse') === 'true',
    session_ttl: value('cfg-session-ttl').trim(),
    thinking_effort: value('cfg-thinking'),
    chat_agent_id: value('cfg-agent').trim(),
    timezone: value('cfg-timezone').trim(),
    locale: value('cfg-locale').trim(),
    app_version: value('cfg-app-version').trim(),
    surface: value('cfg-surface').trim(),
    oauth: {
      provider: value('cfg-oauth-provider'),
      build_stage: value('cfg-oauth-stage'),
      region: value('cfg-oauth-region').trim(),
      callback_port: Number(value('cfg-oauth-port')),
      timeout: value('cfg-oauth-timeout').trim(),
    },
    webui: {
      enabled: value('cfg-webui-enabled') === 'true',
      session_ttl: value('cfg-webui-ttl').trim(),
      max_attempts: Number(value('cfg-webui-attempts')),
      trust_proxy: value('cfg-webui-trust-proxy') === 'true',
      new_password: value('cfg-new-password'),
      clear_password: $('#cfg-clear-password').checked,
    },
  };

  button.disabled = true;
  button.textContent = '保存中…';
  try {
    const result = await api('/config', { method: 'PUT', body: payload });
    if (result.password_set === false) {
      showLogin('管理密码已清空，/admin 已关闭');
      return;
    }
    toast(result.restart_required.length ? '已保存，部分字段重启后生效' : '配置已保存');
    await pageConfig();
    state.overview = null;
  } catch (error) {
    toast(error.message, 'err');
  } finally {
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = '保存配置';
    }
  }
}

/* ---------- logs ---------- */

async function pageLogs() {
  await refreshLogs(true);
  state.refreshTimer = setInterval(() => {
    if (state.page !== 'logs' || document.hidden || !state.logAuto) return;
    refreshLogs(false);
  }, 4000);
}

async function refreshLogs(initial) {
  const data = await api('/logs?limit=200');
  state.logs = { items: data.items || [], stats: data.stats || {} };
  if (initial) renderLogs();
  else updateLogsBody();
}

function logStatusBadge(status) {
  const map = { ok: ['ok', 'ok'], empty: ['warn', 'empty'], failed: ['danger', 'failed'] };
  const [tone, label] = map[status] || ['plain', status];
  return badge(label, tone);
}

function renderLogs() {
  const stats = state.logs.stats || {};
  setTopActions([
    {
      label: state.logAuto ? '自动刷新：开' : '自动刷新：关',
      onClick: () => {
        state.logAuto = !state.logAuto;
        renderLogs();
      },
    },
    { label: '立即刷新', onClick: () => refreshLogs(true).catch((error) => toast(error.message, 'err')) },
  ]);

  content().innerHTML = `
    <div class="page">
      <div class="grid cols-4">
        ${statCard('记录数', formatNumber(stats.requests), '内存环形缓冲，最多 500 条', '')}
        ${statCard('成功', formatNumber(stats.ok), `失败 ${stats.failed || 0}`, 'accent')}
        ${statCard('平均耗时', `${formatNumber(stats.avg_elapsed_ms)}ms`, `空响应 ${stats.empty || 0}`, '')}
        ${statCard('Token', formatNumber(stats.total_tokens), `cache read ${formatNumber(stats.cache_read_tokens)}`, '')}
      </div>
      <div class="card">
        <div class="card-head">
          <h2>最近的请求</h2>
          <div class="seg" id="log-filter">
            ${[['all', '全部'], ['ok', '成功'], ['failed', '失败']].map(([value, label]) =>
              `<button type="button" data-filter="${value}" class="${state.logFilter === value ? 'active' : ''}">${label}</button>`).join('')}
          </div>
        </div>
        <div id="log-table">${logsTable()}</div>
      </div>
    </div>`;

  $$('#log-filter button').forEach((button) => {
    button.addEventListener('click', () => {
      state.logFilter = button.dataset.filter;
      renderLogs();
    });
  });
}

function filteredLogs() {
  const items = state.logs.items || [];
  if (state.logFilter === 'all') return items;
  if (state.logFilter === 'ok') return items.filter((item) => item.status === 'ok');
  return items.filter((item) => item.status !== 'ok');
}

function logsTable() {
  const items = filteredLogs();
  if (!items.length) return '<div class="empty">还没有请求记录<br /><span class="faint">发一次 /v1/chat/completions 就会出现在这里</span></div>';
  const rows = items.map((item) => `<tr>
    <td class="mono nowrap">${esc(new Date(item.time).toLocaleTimeString('zh-CN', { hour12: false }))}</td>
    <td>${logStatusBadge(item.status)}</td>
    <td><code>${esc(item.model)}</code>${item.stream ? ' <span class="faint">stream</span>' : ''}</td>
    <td class="cell-sub truncate" title="${esc(item.account)}">${esc(item.account)}</td>
    <td class="mono">${item.attempts}</td>
    <td class="mono">${formatNumber(item.elapsed_ms)}ms</td>
    <td class="mono">${formatNumber(item.total_tokens)}<div class="cell-sub">p${formatNumber(item.prompt_tokens)} / c${formatNumber(item.completion_tokens)}</div></td>
    <td class="mono">${esc(item.finish)}</td>
    <td class="cell-sub truncate" title="${esc(item.error)}">${esc(item.error || '—')}</td>
  </tr>`).join('');
  return `<div class="table-wrap"><table>
    <thead><tr><th>时间</th><th>状态</th><th>模型</th><th>账号</th><th>尝试</th><th>耗时</th><th>tokens</th><th>finish</th><th>错误</th></tr></thead>
    <tbody>${rows}</tbody>
  </table></div>`;
}

function updateLogsBody() {
  const host = $('#log-table');
  if (host) host.innerHTML = logsTable();
  const cards = $$('#content .stat .value');
  const stats = state.logs.stats || {};
  const values = [formatNumber(stats.requests), formatNumber(stats.ok), `${formatNumber(stats.avg_elapsed_ms)}ms`, formatNumber(stats.total_tokens)];
  cards.forEach((node, index) => { if (values[index] !== undefined) node.textContent = values[index]; });
}

/* ---------- playground ---------- */

async function pagePlayground() {
  const overview = state.overview || await api('/overview');
  state.overview = overview;
  const models = overview.models || [];
  if (!state.chat.model && models.length) state.chat.model = overview.default_model || models[0];
  if (!state.chat.model) state.chat.model = overview.default_model || 'smart';

  setTopActions([
    { label: '清空对话', onClick: () => { state.chat.messages = []; renderPlayground(); } },
  ]);

  content().innerHTML = `
    <div class="page">
      <div class="card">
        <div class="card-head">
          <h2>Playground</h2>
          <span class="hint">走 /admin/api/playground → 与 /v1 完全相同的上游链路，不需要 API key</span>
        </div>
        <div class="form-grid">
          ${field('模型', `<select id="chat-model">
            ${models.map((model) => `<option value="${esc(model)}" ${model === state.chat.model ? 'selected' : ''}>${esc(model)}</option>`).join('')}
          </select>`)}
          <label class="field">
            <span>流式</span>
            <span class="check"><input type="checkbox" id="chat-stream" checked /> stream=true（SSE）</span>
          </label>
        </div>
        <div style="margin-top:14px">
          <label class="field">
            <span>System prompt（可选）</span>
            <textarea id="chat-system" rows="3" placeholder="例如：你是一个严谨的技术助手，用中文回答。">${esc(state.chat.system)}</textarea>
          </label>
        </div>
      </div>

      <div class="card">
        <div class="card-head"><h2>对话</h2><span class="hint">token 用量来自上游 turn_usage</span></div>
        <div class="chat">
          <div class="chat-log" id="chat-log"></div>
          <div class="chat-input">
            <textarea id="chat-input" placeholder="输入消息，Ctrl/⌘ + Enter 发送"></textarea>
            <div class="form-row">
              <button class="btn primary" type="button" id="chat-send">发送</button>
              <span class="section-note" id="chat-state"></span>
            </div>
          </div>
        </div>
      </div>
    </div>`;

  renderChatLog();
  $('#chat-send').addEventListener('click', sendPlayground);
  $('#chat-input').addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      sendPlayground();
    }
  });
  $('#chat-model').addEventListener('change', (event) => { state.chat.model = event.target.value; });
  $('#chat-system').addEventListener('change', (event) => { state.chat.system = event.target.value; });
}

function renderPlayground() {
  pagePlayground().catch((error) => toast(error.message, 'err'));
}

function renderChatLog() {
  const host = $('#chat-log');
  if (!host) return;
  if (!state.chat.messages.length) {
    host.innerHTML = '<div class="empty">发一条消息试试<br /><span class="faint">请求会以当前登录的管理会话转发，不消耗本地 API key</span></div>';
    return;
  }
  host.innerHTML = state.chat.messages.map((message) => {
    const meta = message.meta ? `<div class="meta">${esc(message.meta)}</div>` : '';
    return `<div class="msg ${esc(message.role)}${message.error ? ' error' : ''}">
      <div class="role">${esc(message.role)}</div>
      <div class="body">${esc(message.content || (message.pending ? '…' : ''))}</div>
      ${meta}
    </div>`;
  }).join('');
  host.scrollTop = host.scrollHeight;
}

async function sendPlayground() {
  if (state.chat.streaming) return;
  const input = $('#chat-input');
  const text = input.value.trim();
  if (!text) return;
  state.chat.system = $('#chat-system').value;
  state.chat.model = $('#chat-model').value;
  const stream = $('#chat-stream').checked;

  state.chat.messages.push({ role: 'user', content: text });
  const assistant = { role: 'assistant', content: '', pending: true };
  state.chat.messages.push(assistant);
  input.value = '';
  renderChatLog();

  const history = state.chat.messages
    .filter((message) => !message.pending && message !== assistant)
    .map((message) => ({ role: message.role, content: message.content }));
  const payload = {
    model: state.chat.model,
    stream,
    messages: state.chat.system.trim()
      ? [{ role: 'system', content: state.chat.system.trim() }, ...history]
      : history,
  };
  if (stream) payload.stream_options = { include_usage: true };

  const started = performance.now();
  state.chat.streaming = true;
  $('#chat-send').disabled = true;
  $('#chat-state').textContent = stream ? '流式接收中…' : '请求中…';

  try {
    const response = await fetch('/admin/api/playground', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': state.csrf || '',
        ...(stream ? { Accept: 'text/event-stream' } : {}),
      },
      body: JSON.stringify(payload),
    });
    if (response.status === 401) {
      // The proxy answers 401 when no upstream account is usable; only a
      // missing admin session means the console has to log in again.
      let payload = null;
      try { payload = await response.clone().json(); } catch { /* no body */ }
      const code = payload && payload.error ? payload.error.code : '';
      if (code !== 'upstream_session_unavailable') {
        showLogin('登录已过期，请重新登录');
        throw new Error('登录已过期');
      }
    }
    if (!response.ok) {
      let message = response.statusText;
      try {
        const error = await response.json();
        message = (error.error && error.error.message) || message;
      } catch { /* keep status text */ }
      throw new Error(message);
    }

    if (!stream) {
      const data = await response.json();
      assistant.content = (data.choices && data.choices[0] && data.choices[0].message.content) || '';
      assistant.meta = usageLine(data.usage, data.choices && data.choices[0] && data.choices[0].finish_reason, started);
      delete assistant.pending;
      renderChatLog();
    } else {
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      let usage = null;
      let finish = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const chunks = buffer.split('\n\n');
        buffer = chunks.pop() || '';
        for (const chunk of chunks) {
          for (const line of chunk.split('\n')) {
            if (!line.startsWith('data:')) continue;
            const raw = line.slice(5).trim();
            if (!raw || raw === '[DONE]') continue;
            let event;
            try { event = JSON.parse(raw); } catch { continue; }
            if (event.error) throw new Error(event.error.message || 'upstream error');
            const choice = event.choices && event.choices[0];
            if (choice && choice.delta && choice.delta.content) {
              assistant.content += choice.delta.content;
              delete assistant.pending;
              renderChatLog();
            }
            if (choice && choice.finish_reason) finish = choice.finish_reason;
            if (event.usage) usage = event.usage;
          }
        }
      }
      delete assistant.pending;
      assistant.meta = usageLine(usage, finish, started);
      renderChatLog();
    }
  } catch (error) {
    assistant.error = true;
    assistant.content = assistant.content || `请求失败：${error.message}`;
    delete assistant.pending;
    renderChatLog();
  } finally {
    state.chat.streaming = false;
    const button = $('#chat-send');
    if (button) button.disabled = false;
    const status = $('#chat-state');
    if (status) status.textContent = '';
  }
}

function usageLine(usage, finish, started) {
  const elapsed = ((performance.now() - started) / 1000).toFixed(2);
  const parts = [];
  if (finish) parts.push(`finish=${finish}`);
  if (usage && usage.total_tokens) {
    parts.push(`${usage.total_tokens} tokens`);
    parts.push(`p${usage.prompt_tokens || 0}/c${usage.completion_tokens || 0}`);
    if (usage.prompt_tokens_details && usage.prompt_tokens_details.cached_tokens) {
      parts.push(`cache ${usage.prompt_tokens_details.cached_tokens}`);
    }
  }
  parts.push(`${elapsed}s`);
  return parts.join(' · ');
}

/* ---------- boot ---------- */

function bindLoginForm() {
  $('#login-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const password = $('#login-password').value;
    const button = $('#login-submit');
    const error = $('#login-error');
    error.textContent = '';
    button.disabled = true;
    button.textContent = '验证中…';
    try {
      const session = await api('/auth/login', { method: 'POST', body: { password } });
      state.csrf = session.csrf_token;
      $('#login-password').value = '';
      await showApp();
    } catch (failure) {
      error.textContent = failure.message || '登录失败';
    } finally {
      button.disabled = false;
      button.textContent = '进入控制台';
    }
  });
}

function bindShell() {
  $$('#nav button').forEach((button) => {
    button.addEventListener('click', () => navigate(button.dataset.page));
  });
  window.addEventListener('hashchange', () => {
    const page = (location.hash || '#/overview').replace(/^#\/?/, '') || 'overview';
    if (page !== state.page && PAGES[page] && !$('#app').classList.contains('hidden')) navigate(page, { replace: true });
  });
  $('#theme-toggle').addEventListener('click', toggleTheme);
  $('#logout').addEventListener('click', async () => {
    try { await api('/auth/logout', { method: 'POST' }); } catch { /* ignore */ }
    showLogin('已退出登录');
  });
  $('#modal-cancel').addEventListener('click', () => closeModal(false));
  $('#modal-confirm').addEventListener('click', () => closeModal(true));
  $('#modal').addEventListener('click', (event) => {
    if (event.target.id === 'modal') closeModal(false);
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') closeModal(false);
  });
}

async function boot() {
  applyTheme();
  bindLoginForm();
  bindShell();
  pingHealth();
  try {
    const session = await api('/auth/session');
    state.csrf = session.csrf_token;
    await showApp();
  } catch (error) {
    if (!(error instanceof ApiError) || error.status !== 401) {
      $('#login-error').textContent = '无法访问管理接口：' + error.message;
    }
    showLogin('');
  }
}

async function pingHealth() {
  const status = $('#login-status');
  if (!status) return;
  try {
    const response = await fetch('/healthz', { cache: 'no-store' });
    status.textContent = response.ok ? '服务在线' : '服务异常';
  } catch {
    status.textContent = '服务不可达';
  }
}

boot();
