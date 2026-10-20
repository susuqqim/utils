/* 密钥输入框的 form 只用于把它与搜索框隔离（Chromium 会把裸 password 输入合成凭证表单），
   回车不提交：本面板 CSP 为 script-src 'self'，行内 onsubmit 会被拦，故用同源监听。 */
(function () {
  const kf = document.getElementById('keyForm');
  if (kf) kf.addEventListener('submit', (e) => e.preventDefault());
})();

'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme', LS_ACCD = 'wb2api.accdensity';
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let accDensity = localStorage.getItem(LS_ACCD) || 'full'; // full | compact
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;
let isRestarting = false, isShuttingDown = false;

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  h['Authorization'] = 'Bearer ' + (k || 'keyless'); // 总是带：keyless 模式下这是面板写操作的 CSRF 证明（表单无法设置自定义头）
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}

/* 模型级限流台账（issue #36 / 上游 5ae2200）：把 rate_limited_models 渲染成
   账号卡片上的"模型 + 预计解封时间"小条；无重置时间的 6004 为 AuditOnly 展示项。 */
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model, kind,
      detail: '上游无此模型服务（换模型才有用）' + (remaining ? ' · 网关 ' + dur(remaining) + ' 后再探测' : ''),
      title: model + '\n上游返回 11102 service info not found：该模型在上游已无服务（不是限流，等待无用）。\n建议改用 /v1/models 中的其它模型，或用 global:<id> 指向国际版；\n若已列入 hidden_models，则直接调用会被本地 400 model_hidden 拒绝。' + (deadline ? '\n网关最早再探测：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  let detail = resetAt
    ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
    : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && resetAt && until < resetAt) {
    detail += ' · 网关最快 ' + dur(Math.max(0, Math.round((until - now) / 1000))) + ' 后重试';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  }
  return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
  }).join('') + '</div>';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: '账号池', usage: '用量', packages: '积分构成', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  if (v === 'locks') loadModelLocks();   // 模型锁池（S1）：进入该视图时拉取快照
  if (v === 'reqlog') loadReqLog();      // 请求指标（S3）：进入该视图时拉取
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  if (v === 'taskscenter') reattachQueueView();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
go((location.hash || '#accounts').slice(1) in TITLES ? (location.hash || '#accounts').slice(1) : 'accounts');

/* ── 账号池 ───────────────────────────────────────────────────────── */
function renderAccounts(list) { renderAccCards(list); }

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
    const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
    $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;

    // 本地改造接轨：积分消耗统计与临期抢救展示
    if ($('sDailyCost')) {
      $('sDailyCost').textContent = (d.total_daily_credits_used != null ? Number(d.total_daily_credits_used).toFixed(2) : '0.00') + ' c';
    }
    if ($('sTotalCost')) {
      $('sTotalCost').textContent = (d.total_credits_used != null ? Number(d.total_credits_used).toFixed(1) : '0.0') + ' c';
    }
    if ($('sUrgent') && $('statUrgent')) {
      if (d.urgent_48h_credits > 0) {
        $('sUrgent').textContent = d.urgent_48h_credits + ' c (紧急)';
        $('statUrgent').className = 'stat bad';
      } else if (d.urgent_72h_credits > 0) {
        $('sUrgent').textContent = d.urgent_72h_credits + ' c (临期)';
        $('statUrgent').className = 'stat warn';
      } else {
        $('sUrgent').textContent = '0 c';
        $('statUrgent').className = 'stat good';
      }
    }
    // 路由模式卡片
    const pinnedMap = d.pinned_uids || {};
    const pinnedKeys = Object.keys(pinnedMap);
    if ($('sRouteMode') && $('statRouteMode')) {
      if (pinnedKeys.length > 0) {
        $('sRouteMode').textContent = '📌 强制路由';
        $('statRouteMode').className = 'stat warn';
      } else {
        $('sRouteMode').textContent = '⚡ 智能调度';
        $('statRouteMode').className = 'stat good';
      }
    }

    // 顶部强制路由横幅与一键清除按钮
    const pb = $('pinnedBanner');
    const ub = $('btnUnpinAll');
    if (pb && ub) {
      if (pinnedKeys.length > 0) {
        const text = pinnedKeys.map(r => {
          const uid = pinnedMap[r];
          const ac = (d.accounts || []).find(a => a.uid === uid);
          const name = ac ? (ac.nickname || ac.uid.slice(0, 8)) : uid.slice(0, 8);
          return (r === 'global' ? '国际版' : '国内') + ': ' + name;
        }).join(' · ');
        pb.textContent = '📌 强制路由中: ' + text + '（重启后重置）';
        pb.style.display = 'inline-flex';
        ub.style.display = 'inline-block';
      } else {
        pb.style.display = 'none';
        ub.style.display = 'none';
      }
    }

    if ($('sSticky')) $('sSticky').textContent = d.sticky_sessions;
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    if (!isRestarting && !isShuttingDown) {
      $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
      const p = $('navPulse');
      p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    }
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

async function onAccAction(ev) {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'pk') { go('packages'); return; }
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'pin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/pin', { method: 'POST' });
      toast(r.message || '已开启强制路由模式', 'ok');
    } else if (a === 'unpin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/unpin', { method: 'POST' });
      toast(r.message || '已取消强制路由，恢复智能多梯队调度', 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（保号任务照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
}
$('accCards').addEventListener('click', onAccAction);

if ($('btnUnpinAll')) {
  $('btnUnpinAll').onclick = async () => {
    if (!confirm('确认清除所有域的强制路由指定，恢复自动智能多梯队调度？')) return;
    try {
      await api('accounts/unpin_all', { method: 'POST' });
      toast('已恢复全局自动智能多梯队调度', 'ok');
    } catch (e) { toast(e.message, 'err'); }
    finally { loadOverview(true); }
  };
}

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测数据是可选增强：拉取失败不影响模型列表本身
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    const list = d.models || [];
    if (!list.length) { tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>'; return; }
    const probes = pr.probes || {};
    const probeKeys = Object.keys(probes);
    const probeOf = id => probes[id] || probes[probeKeys.find(k => k.endsWith(':' + id))];
    // 2026-10：模型页按 realm 分成「国内版 / 国际版」两块（用户需求）。
    // 命名协议：cn 模型带 cn: 前缀或裸名，global 模型带 global: 前缀（后端只发 global:<id>）。
    const __modelsSplitByRealm = true;
    const rowOf = m => {
      const eff = (m.supported_efforts || []).slice();
      if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
      const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
        : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
      // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
      const caps = [];
      if (m.is_default) caps.push('<span class="tag ok">默认</span>');
      if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
      if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
      if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
      const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
      const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
      return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
        '<td class="num">' + rateCell(m) + '</td>' +
        '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
        '<td class="efs" style="white-space:normal">' + effs + '</td>' +
        '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
        outCell(m, probeOf(m.id)) + '</tr>';
    };
    const cnModels = list.filter(m => !String(m.id).startsWith('global:'));
    const glModels = list.filter(m => String(m.id).startsWith('global:'));
    const sectRow = (title, n, hint) =>
      '<tr class="sect"><td colspan="7" style="background:var(--surface-2);font-weight:600;padding:8px 10px">' +
      title + '（' + n + '）' + (hint ? ' <span style="font-weight:400;color:var(--ink-3);font-size:12.5px">' + hint + '</span>' : '') + '</td></tr>';
    let html = '';
    html += sectRow('🇨🇳 国内版', cnModels.length, cnModels.length ? '' : '（池内无 cn 账号或上游未返回）');
    if (cnModels.length) html += cnModels.map(rowOf).join('');
    html += sectRow('🌐 国际版', glModels.length, glModels.length ? '' : '（未启用 global 或池内无 global 账号）');
    if (glModels.length) html += glModels.map(rowOf).join('');
    tb.innerHTML = html;
    const hit = list.filter(m => probeOf(m.id)).length;
    $('mdNote').textContent = list.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}
$('btnModels').onclick = loadModels;

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  try {
    const d = await api('logs');
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + esc({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
  } catch (e) { /* 概览已提示 */ }
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  listen: ['listen'], api_key: ['api_key'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  resource_sync_enabled: ['schedule', 'resource_sync_enabled'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  read_timeout: ['server', 'read_timeout'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  chunk_quota_credits: ['pool', 'chunk_quota_credits'],
  critical_expire_hours: ['pool', 'critical_expire_hours'],
  urgent_expire_hours: ['pool', 'urgent_expire_hours'],
  near_expire_days: ['pool', 'near_expire_days'],
  safe_reserve_days: ['pool', 'safe_reserve_days'],
  low_credit_cutoff: ['pool', 'low_credit_cutoff'],
  expire_urgency_weight: ['pool', 'expire_urgency_weight'],
  expire_power: ['pool', 'expire_power'],
  adaptive_chunking: ['pool', 'adaptive_chunking'],
  tier_cheap_quota: ['pool', 'tier_cheap_quota'],
  tier_normal_quota: ['pool', 'tier_normal_quota'],
  tier_flagship_quota: ['pool', 'tier_flagship_quota'],
  paced_breathing_enabled: ['pool', 'paced_breathing_enabled'],
  paced_tier2_consecutive_max: ['pool', 'paced_tier2_consecutive_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  ui_tray: ['ui', 'tray'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  global_enabled: ['global', 'enabled'],
  global_chat_base: ['global', 'chat_base'],
  global_billing_base: ['global', 'billing_base'],
};
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? '' : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      if (raw === '') v = undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 配置未成功加载时禁止保存：此时表单里所有 checkbox 都处于未勾选的初始态，
  // 提交会把 ui_tray / session_sticky_enabled / global_enabled / *_enabled 等
  // 一律写成 false —— 等于用一次失败的读取静默关掉一堆开关。
  if (!cfgLoaded) {
    toast('配置尚未成功加载，已阻止保存（避免误将各开关写成关闭）。请先"放弃修改"重载。', 'err');
    return;
  }
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const need = (r.restart_required || []);
    // 列出字段名而不是只报个数：用户需要知道"改了哪一项没生效"（第十二轮审计 F5）。
    toast(need.length ? '配置已保存；以下需重启进程生效：' + need.join('、') : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  h['Authorization'] = 'Bearer ' + (k || 'keyless'); // 总是带：keyless 模式下这是面板写操作的 CSRF 证明（表单无法设置自定义头）
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 系统控制（左下角 重启/关闭） ─────────────────────────────────── */
if ($('btnRestart')) {
  $('btnRestart').onclick = async () => {
    if (isRestarting || isShuttingDown) return;
    if (!confirm('确定要重启网关服务吗？\n重启过程中将中断正在进行的请求，耗时约 2-3 秒。')) return;

    isRestarting = true;
    const bRestart = $('btnRestart');
    const bShutdown = $('btnShutdown');
    bRestart.disabled = true;
    bShutdown.disabled = true;
    const txtSpan = bRestart.querySelector('span');
    if (txtSpan) txtSpan.textContent = '重启中…';
    $('navPulse').className = 'pulse warn';
    $('navState').textContent = '正在重启...';

    try {
      await api('system/restart', { method: 'POST' });
      toast('服务正在重启，请稍候…', 'ok');
    } catch (e) {
      toast('重启请求失败：' + e.message, 'err');
      bRestart.disabled = false;
      bShutdown.disabled = false;
      if (txtSpan) txtSpan.textContent = '重启';
      isRestarting = false;
      loadOverview(true);
      return;
    }

    // 延时 1.5s 后轮询 /healthz 探测恢复
    let attempts = 0;
    const maxAttempts = 30; // 最长等待 24 秒
    setTimeout(function pollHealth() {
      fetch('/healthz', { cache: 'no-store' })
        .then(r => {
          if (r.ok) {
            toast('服务已重启成功，正在刷新页面…', 'ok');
            $('navPulse').className = 'pulse';
            $('navState').textContent = '服务正常';
            setTimeout(() => window.location.reload(), 1000);
          } else {
            throw new Error('not ready');
          }
        })
        .catch(() => {
          attempts++;
          if (attempts < maxAttempts) {
            setTimeout(pollHealth, 800);
          } else {
            toast('重启等待超时，请检查服务状态或手动刷新', 'err');
            bRestart.disabled = false;
            bShutdown.disabled = false;
            if (txtSpan) txtSpan.textContent = '重启';
            isRestarting = false;
          }
        });
    }, 1500);
  };
}

if ($('btnShutdown')) {
  $('btnShutdown').onclick = async () => {
    if (isRestarting || isShuttingDown) return;
    if (!confirm('⚠️ 确定要关闭网关服务吗？\n关闭后服务将停止监听，需在终端或宿主机手动启动。')) return;

    isShuttingDown = true;
    const bRestart = $('btnRestart');
    const bShutdown = $('btnShutdown');
    bRestart.disabled = true;
    bShutdown.disabled = true;
    const txtSpan = bShutdown.querySelector('span');
    if (txtSpan) txtSpan.textContent = '关闭中…';

    try {
      await api('system/shutdown', { method: 'POST' });
      toast('网关服务已关闭。如需重新使用，请手动启动服务。', 'warn');
      if (refTimer) clearInterval(refTimer);
      $('navPulse').className = 'pulse bad';
      $('navState').textContent = '服务已停止';
      if (txtSpan) txtSpan.textContent = '已关闭';
    } catch (e) {
      toast('关闭请求失败：' + e.message, 'err');
      bRestart.disabled = false;
      bShutdown.disabled = false;
      if (txtSpan) txtSpan.textContent = '关闭';
      isShuttingDown = false;
    }
  };
}

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') loadOverview(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') reattachQueueView();
}
function start() {
  loadOverview(true);
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟；若需补报多次 mp 对话事件（每条间隔 45s，最多 10 条）可能长达约 10 分钟，期间请勿关闭页面。确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
// （2026-10-03 删除死代码：staskHTML / LUCK_SVG / SCHOOL_TITLES 三处零调用；
//   staskHTML 里读的 target_count/progress 与后端字段 target/current 不符，
//   留着就是「改了字段名没人发现」的温床。需要时从 git 历史取回。）

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : esc(ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!groups.length) {
    empty.style.display = '';
    if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
    if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true; $('qcSummary').textContent = '';
    return;
  }
  empty.style.display = 'none';
  empty.style.display = 'none';
  let total = 0;
  list.innerHTML = groups.map(g => {
    total += g.rows.length;
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  $('qcSummary').textContent = total + ' 项';
  updateProgress(progress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    toast('任务队列执行结束', 'ok');
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

function usBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-wrapbar">' +
    '<span class="bar bar-p" style="width:' + (pp * 0.8).toFixed(1) + 'px" title="prompt"></span>' +
    '<span class="bar bar-c" style="width:' + Math.max(2, pc * 0.8).toFixed(1) + 'px" title="completion"></span>' +
    '</span>';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制是否追加延迟/速率两列。
   isAccount 控制积分单元格是否展示今日/累计。 */
function usRow(name, sub, a, mid, withPerf, isAccount) {
  let costHtml = '';
  if (isAccount) {
    const today = a.daily_credits_used != null ? Number(a.daily_credits_used).toFixed(2) : null;
    const total = a.total_credits_used != null ? Number(a.total_credits_used).toFixed(2) : null;
    const win = a.credits != null && a.credits > 0 ? Number(a.credits).toFixed(2) : null;
    if (today !== null || total !== null) {
      costHtml = '<td class="num"><div style="font-family:var(--mono);font-weight:600;color:var(--ink)">' +
        (today !== null ? today : '0.00') + ' <span style="font-weight:normal;color:var(--ink-3);font-size:11px">c/今</span></div>' +
        (total !== null ? '<div class="note" style="font-size:10.5px">累计 ' + total + ' c</div>' : '') +
        '</td>';
    } else {
      costHtml = '<td class="num font-mono">' + (win ? win + ' c' : '—') + '</td>';
    }
  } else {
    const c = a.credits != null && a.credits > 0 ? Number(a.credits).toFixed(2) : '0.00';
    costHtml = '<td class="num font-mono" style="font-weight:550">' + c + ' <span style="font-weight:normal;color:var(--ink-3);font-size:11px">c</span></td>';
  }

  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    costHtml +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

let lastUsageData = null;
let usageChartMode = 'tokens'; // 'tokens' | 'credits'

function renderUsage(d) {
  lastUsageData = d;
  const t = d.totals || {};
  const todayCost = (d.total_daily_credits_used != null ? Number(d.total_daily_credits_used).toFixed(2) : '0.00') + ' c';
  const totalCost = (d.total_credits_used != null ? Number(d.total_credits_used).toFixed(2) : '0.00') + ' c';
  const winCost = (t.credits != null && t.credits > 0 ? Number(t.credits).toFixed(2) + ' c' : null);

  const promptT = Number(t.prompt_tokens || 0);
  const cachedT = Number(t.cached_tokens || 0);
  const hitPct = promptT > 0 ? cachedT / promptT * 100 : 0;
  const hitCls = hitPct >= 50 ? 'good' : '';
  const reqN = Number(t.requests || 0), errN = Number(t.errors || 0);
  const okPct = reqN > 0 ? (reqN - errN) / reqN * 100 : 100;
  const kpiRow = (k, v, cls, tip) =>
    '<div class="us-kpi-row"><span class="k"' + (tip ? ' title="' + esc(tip) + '"' : '') + '>' + esc(k) + '</span>' +
    '<span class="v' + (cls ? ' ' + cls : '') + '">' + esc(String(v)) + '</span></div>';
  // 口径（2026-10-03 起）：一次"请求" = 一次**上游尝试**（含轮转/重试的每一次，故重试会放大请求数）；
  // "失败尝试" = 该次尝试未得到可交付结果（4xx/5xx/传输错误/聚合失败/空流或中途读错误）；
  // 客户端取消（499）计请求但不计失败。历史数据是旧口径（"有 usage token 即算成功"），不完全可比。
  const TIP_REQ = '一次"请求" = 一次上游尝试（含轮转/重试的每一次）';
  const TIP_ERR = '该次上游尝试未得到可交付结果（4xx/5xx/传输错误/聚合失败/空流或中途读错误）；客户端取消（499）不计失败';
  const TIP_OK = '成功率 = 1 − 失败尝试 / 请求数（口径自 2026-10-03 起统一，历史数据为旧口径）';
  $('usStats').innerHTML =
    '<div class="us-kpi-group"><h4>Token 用量</h4>' +
      kpiRow('总 token', fmtTok(t.total_tokens)) +
      kpiRow('输入 token', fmtTok(t.prompt_tokens)) +
      kpiRow('输出 token', fmtTok(t.completion_tokens)) +
      kpiRow('缓存命中 token', fmtTok(t.cached_tokens)) +
      kpiRow('缓存命中率', hitPct.toFixed(1) + '%', hitCls) +
      // 口径标注（S4 ④）：本页 KPI 一律为**生成期**口径；与 DSH 对账须同口径。
      // tok/s 用 completion_tokens /（总耗时 − 首字时延），不用端到端总时长 —— 否则把排队/首字等待
      // 算进吞吐，会出现"同一请求两边数字不同"的口径混用。
      kpiRow('口径', '生成期') +
    '</div>' +
    '<div class="us-kpi-group"><h4>请求与质量</h4>' +
      kpiRow('请求数', reqN, '', TIP_REQ) +
      kpiRow('失败尝试', errN, errN ? 'warn' : '', TIP_ERR) +
      kpiRow('成功率', okPct.toFixed(1) + '%', okPct < 95 ? 'warn' : 'good', TIP_OK) +
      kpiRow('平均延迟', fmtMs(t.avg_latency_ms)) +
      kpiRow('平均速率', formatRate(t.avg_tokens_per_second)) +
    '</div>' +
    '<div class="us-kpi-group"><h4>积分消耗</h4>' +
      kpiRow('今日全池消耗', todayCost, 'primary') +
      kpiRow('累计全池消耗', totalCost) +
      kpiRow('窗口消耗积分', winCost || '—') +
    '</div>';

  // 卡片、三张表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = ($('usWindow') && $('usWindow').selectedOptions[0]) ?
    $('usWindow').selectedOptions[0].textContent.trim() : '';
  $('usNote').textContent =
    (winLabel ? winLabel + ' · ' : '') +
    (d.buckets || 0) + ' 个分桶' +
    (d.since ? ' · 数据自 ' + d.since.replace('T', ' ') : '') +
    (d.file_bytes ? ' · 文件 ' + (d.file_bytes / 1024).toFixed(1) + ' KB' : '');

  $('usAccBody').innerHTML = (d.by_account || []).map(x =>
    usRow(x.key.slice(0, 8), x.extra || '', x,
      '<td class="num">' + esc(x.realm || '') + '</td>', true, true)
  ).join('') || '<tr><td colspan="11" class="empty">暂无数据</td></tr>';

  $('usModelBody').innerHTML = (d.by_model || []).map(x =>
    usRow(x.key, '', x, '', false, false)).join('') || '<tr><td colspan="8" class="empty">暂无数据</td></tr>';

  $('usRealmBody').innerHTML = (d.by_realm || []).map(x =>
    usRow(x.key, '', x, '', false, false)).join('') || '<tr><td colspan="8" class="empty">暂无数据</td></tr>';

  renderUsageChart(d.series || [], usageChartMode);
}

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

function renderUsageChart(series, mode) {
  const host = $('usChart');
  mode = mode || usageChartMode || 'tokens';

  // 丢掉时间解析不出来的点，按时间升序排列
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    const tt = Number(p.total_tokens || 0) || (pt + ct);
    const credits = Number(p.credits || 0);
    pts.push({
      t, scope: p.scope, raw: p.t, pt, ct, tt, credits,
      req: p.requests || 0
    });
  }
  pts.sort((a, b) => a.t - b.t);

  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    return;
  }

  const W = 760, H = 180, PL = 54, PR = 14, PT = 14, PB = 28;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(0, t1 - t0);

  let maxVal = 1;
  if (mode === 'credits') {
    maxVal = Math.max(0.1, ...pts.map(p => p.credits));
  } else {
    maxVal = Math.max(1, ...pts.map(p => p.tt));
  }

  // 柱宽取合理间距，首尾柱子内缩确保完全位于 [PL, W - PR] 绘图区域内
  let bw = 24;
  if (pts.length > 1 && span > 0) {
    let minGap = Infinity;
    for (let i = 1; i < pts.length; i++) {
      const g = pts[i].t - pts[i - 1].t;
      if (g > 0) minGap = Math.min(minGap, g);
    }
    if (!isFinite(minGap) || minGap <= 0) minGap = span / pts.length;
    const slot = iw * (minGap / span);
    bw = Math.max(3, Math.min(28, slot * 0.75));
  }
  const pad = bw / 2 + 2;
  const xOf = t => span === 0 ? (PL + iw / 2) : (PL + pad + (t - t0) / span * (iw - 2 * pad));

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" preserveAspectRatio="xMidYMid meet">';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) + '" y2="' + y.toFixed(1) + '"/>';
    const tickVal = mode === 'credits' ? (maxVal * i / 4).toFixed(1) + ' c' : fmtTok(maxVal * i / 4);
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) + '" text-anchor="end">' + tickVal + '</text>';
  }

  // 跨天分界虚线（绘制在真正的午夜 00:00，避免劈在柱身）
  if (span > 0) {
    const startDay = new Date(t0);
    startDay.setHours(24, 0, 0, 0);
    let mid = startDay.getTime();
    while (mid <= t1) {
      const xMid = xOf(mid);
      if (xMid >= PL + pad && xMid <= W - PR - pad) {
        out += '<line class="gl" x1="' + xMid.toFixed(1) + '" y1="' + PT + '" x2="' + xMid.toFixed(1) + '" y2="' + (PT + ih) + '" stroke-dasharray="3 3" style="opacity:.6"/>';
      }
      mid += 24 * 3600 * 1000;
    }
  }

  // 柱子（包含在 <g class="bar-col"> 中，内嵌 <title> 保证浏览器悬浮 tooltip 正常弹出）
  for (const p of pts) {
    const cx = xOf(p.t);
    const x = cx - bw / 2;
    const yBase = PT + ih;
    const d = new Date(p.t);
    const dateStr = (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' + String(d.getHours()).padStart(2, '0') + ':00';
    const tip = dateStr + '\n' +
      (p.credits > 0 ? '消耗积分: ' + p.credits.toFixed(2) + ' c\n' : '') +
      'Prompt: ' + fmtTok(p.pt) + ' (' + p.pt.toLocaleString() + ')\n' +
      'Completion: ' + fmtTok(p.ct) + ' (' + p.ct.toLocaleString() + ')\n' +
      '总 Token: ' + fmtTok(p.tt) + ' (' + p.tt.toLocaleString() + ')\n' +
      '请求次数: ' + p.req + ' 次';

    out += '<g class="bar-col">';
    out += '<title>' + esc(tip) + '</title>';

    if (mode === 'credits') {
      const val = p.credits;
      const hTot = maxVal > 0 ? Math.min(ih, Math.max(val > 0 ? 1.5 : 0, ih * (val / maxVal))) : 0;
      const y = yBase - hTot;
      if (hTot > 0) {
        out += '<rect x="' + x.toFixed(2) + '" y="' + y.toFixed(2) + '" width="' + bw.toFixed(2) + '" height="' + hTot.toFixed(2) + '" fill="#f59e0b" rx="1.5"/>';
      }
    } else {
      const val = p.tt;
      const hTot = maxVal > 0 ? Math.min(ih, ih * (val / maxVal)) : 0;
      let hP = val > 0 ? hTot * (p.pt / val) : 0;
      let hC = hTot - hP;
      if (p.ct > 0 && hC < 1.5 && hTot >= 2) {
        hC = 1.5;
        hP = hTot - hC;
      }
      const yP = yBase - hP;
      const yC = yBase - hTot;
      if (hP > 0) {
        out += '<rect x="' + x.toFixed(2) + '" y="' + yP.toFixed(2) + '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) + '" fill="var(--accent)" rx="1"/>';
      }
      if (hC > 0) {
        out += '<rect x="' + x.toFixed(2) + '" y="' + yC.toFixed(2) + '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) + '" fill="var(--ok)" rx="1"/>';
      }
    }
    out += '</g>';
  }

  // x 轴基线
  out += '<line class="ax" x1="' + PL + '" y1="' + (PT + ih) + '" x2="' + (W - PR) + '" y2="' + (PT + ih) + '"/>';

  // x 轴刻度：跨天时展示月-日与时间，单日展示小时
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  const multiDay = (t1 - t0) > 28 * 3600 * 1000;
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const diff = Math.abs(pts[i].t - target);
      if (diff < best) { best = diff; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    const d = new Date(p.t);
    const dt = (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0');
    const tm = String(d.getHours()).padStart(2, '0') + ':00';
    const lab = p.scope === 'day' ? dt : (multiDay ? dt + ' ' + tm : tm);
    const cx = xOf(p.t);
    const anchor = cx < PL + 20 ? 'start' : (cx > W - PR - 20 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) + '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(lab) + '</text>';
  }

  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

async function loadUsage() {
  const hours = ($('usWindow') && $('usWindow').value) || 72;
  try {
    const d = await api('usage?hours=' + encodeURIComponent(hours));
    lastUsageData = d;
    renderUsage(d);
    renderScheduleTimeline();
  } catch (e) {
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
if ($('usWindow')) $('usWindow').onchange = loadUsage;

if ($('btnModeTokens')) {
  $('btnModeTokens').onclick = () => {
    usageChartMode = 'tokens';
    $('btnModeTokens').classList.add('on');
    if ($('btnModeCredits')) $('btnModeCredits').classList.remove('on');
    if ($('usChartTitle')) $('usChartTitle').textContent = 'Token 时序';
    if ($('usChartLegend')) $('usChartLegend').innerHTML = '<i class="sw sw-p"></i>prompt<i class="sw sw-c"></i>completion';
    renderUsageChart(lastUsageData ? (lastUsageData.series || []) : [], 'tokens');
  };
}
if ($('btnModeCredits')) {
  $('btnModeCredits').onclick = () => {
    usageChartMode = 'credits';
    $('btnModeCredits').classList.add('on');
    if ($('btnModeTokens')) $('btnModeTokens').classList.remove('on');
    if ($('usChartTitle')) $('usChartTitle').textContent = '积分消耗时序';
    if ($('usChartLegend')) $('usChartLegend').innerHTML = '<i class="sw sw-cr"></i>消耗积分 (credits)';
    renderUsageChart(lastUsageData ? (lastUsageData.series || []) : [], 'credits');
  };
}

/* 调度卷轴：把日志环形缓冲里的排程事件按时间轴渲染（B 方案，零后端改动）。
   与柱状图共用 #usChart 容器，通过顶部 Tab 切换。 */
const SCHED_RE = /签到|保活|旅行|活跃|成长|夜猫|黑猫|排程|调度|schedul|checkin|keepalive|travel|activity|blackcat|growth/i;
function fmtClock(ts) {
  const d = new Date(ts);
  if (!ts || isNaN(d)) return '--:--:--';
  const p = n => String(n).padStart(2, '0');
  return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}
async function renderScheduleTimeline() {
  const host = $('schedTimeline');
  if (!host) return;
  host.innerHTML = '<div class="us-empty">读取调度日志…</div>';
  try {
    const d = await api('logs');
    const seen = new Set(), uniq = [];
    for (const e of (d.entries || [])) {
      const t = String(e.text || '');
      if (!t || !SCHED_RE.test(t)) continue;
      const k = (e.ts || '') + '|' + t;
      if (seen.has(k)) continue;
      seen.add(k);
      uniq.push(e);
    }
    if (!uniq.length) { host.innerHTML = '<div class="us-empty">暂无调度日志（签到 / 保活 / 旅行 / 活跃 / 成长 / 夜猫）</div>'; return; }
    host.innerHTML = '<div class="us-timeline">' + uniq.map(e => {
      const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断|跳过/.test(e.text) ? ' w' : '';
      return '<div class="tl-row' + lvl + '"><span class="tl-t">' + esc(fmtClock(e.ts)) + '</span>' +
        '<span class="tl-dot"></span><span class="tl-x">' + esc(e.text) + '</span></div>';
    }).join('') + '</div>';
  } catch (err) {
    host.innerHTML = '<div class="us-empty">读取失败：' + esc(err.message) + '</div>';
  }
}
if ($('btnSched')) $('btnSched').onclick = renderScheduleTimeline;

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  $('pkExpiry').innerHTML = (rows
    ? '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d) {
  const list = (d.accounts || []);
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="background:' + expiryColor +
          ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
          ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，包的**面额**列是重点
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const packs = (a.packages || []);
    const rows = packs.map(p => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    }).join('');
    return '<details class="box pk-fold"><summary><b>' +
      esc(a.nickname || a.uid.slice(0, 8)) + '</b> · ' + esc(a.realm || '') +
      '<span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · ' + packs.length + ' 个包</span>' +
      '</summary><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div></details>';
  }).join('');
}

async function loadPackages() {
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const d = await api('packages');
    renderPackages(d);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

if ($('btnPk')) $('btnPk').onclick = loadPackages;

/* ── 账号卡片视图（P1：只读概览 + 复用现有操作委托）────────────────── */
function applyAccView() {
  const cards = $('accCards'), seg = $('accDensityToggle');
  if (!cards) return;
  const compact = accDensity === 'compact';
  cards.classList.toggle('compact', compact);
  if (seg) seg.querySelectorAll('button').forEach(b => b.classList.toggle('on', b.dataset.v === (compact ? 'compact' : 'full')));
}
if ($('accDensityToggle')) {
  $('accDensityToggle').addEventListener('click', e => {
    const b = e.target.closest('button[data-v]');
    if (!b) return;
    accDensity = b.dataset.v === 'compact' ? 'compact' : 'full';
    try { localStorage.setItem(LS_ACCD, accDensity); } catch (_) {}
    applyAccView();
  });
}
function dtShort(iso) {
  const d = new Date(iso);
  if (!iso || isNaN(d)) return '—';
  const p2 = n => String(n).padStart(2, '0');
  return p2(d.getMonth() + 1) + '/' + p2(d.getDate()) + ' ' + p2(d.getHours()) + ':' + p2(d.getMinutes());
}
function hhmm(iso) {
  const d = new Date(iso);
  if (!iso || isNaN(d)) return '—';
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}
function urgent48(s) {
  const now = Date.now(); let sum = 0;
  for (const p of (s.packages || [])) {
    if (p && p.remain > 0 && p.expire && (new Date(p.expire) - now) <= 48 * 3600 * 1000) sum += p.remain;
  }
  return sum;
}
function cardPkgs(s) {
  const now = Date.now();
  const all = (s.packages || []).filter(p => p && p.remain > 0 && p.expire)
    .slice().sort((a, b) => new Date(a.expire) - new Date(b.expire));
  const maxRemain = Math.max(1, ...all.map(p => p.remain));
  const rows = all.slice(0, 3).map(p => {
    const h = (new Date(p.expire) - now) / 3600000;
    const cls = h <= 48 ? 'crit' : (h <= 168 ? 'warn' : 'safe');
    const w = Math.max(10, Math.round(p.remain / maxRemain * 100));
    const left = h <= 48 ? Math.max(0, Math.round(h)) + 'h' : Math.round(h / 24) + 'd';
    return '<div class="pk"><span class="amt">' + esc(p.remain) + ' 积分</span>' +
      '<span class="exp">' + dtShort(p.expire) + ' · ' + left + '</span>' +
      '<span class="bar ' + cls + '"><i style="width:' + w + '%"></i></span></div>';
  });
  while (rows.length < 3) rows.push('<div class="pk pk-empty"><span class="amt">—</span><span class="exp"></span><span class="bar"><i style="width:0%"></i></span></div>');
  return rows.join('');
}
function checkinBadge(s) {
  const st = s.last_checkin_status || '';
  const at = s.last_checkin_at;
  const isToday = at && new Date(at).toDateString() === new Date().toDateString();
  if (!isToday || !st) return '<span class="ck none">今日未签到</span>';
  if (st === 'ok') return '<span class="ck ok">已签到 ' + hhmm(at) + '</span>';
  if (st === 'already') return '<span class="ck ok">今日已签到</span>';
  return '<span class="ck bad">签到失败</span>';
}
function expiryHint(s) {
  const d = s.days_until_expire != null ? Number(s.days_until_expire) : NaN;
  let txt = '', near = false;
  if (isFinite(d) && d > 0) {
    if (d < 1) { txt = '最近 ' + Math.max(1, Math.round(d * 24)) + ' 小时后到期'; near = true; }
    else { txt = '最近 ' + Math.round(d) + ' 天后到期'; near = d <= 7; }
  } else if (s.earliest_expire) {
    const h = (new Date(s.earliest_expire) - Date.now()) / 3600000;
    if (isFinite(h) && h > 0) { txt = '最近 ' + Math.round(h) + ' 小时后到期'; near = h <= 168; }
  }
  if (!txt) return '<div class="urgent none">⏳ 暂无到期信息</div>';
  return '<div class="urgent none' + (near ? ' near' : '') + '">⏳ ' + esc(txt) + '</div>';
}
function cardHTML(s) {
  const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
  const un = (new Date(s.until || 0) - Date.now()) / 1000;
  const cool = Math.max(bl > 0 ? bl : 0, un > 0 ? un : 0);
  let cls = '', tag;
  if (s.pinned) cls += ' pinned';
  if (s.disabled) { cls += ' off'; tag = '<span class="tag bad">已禁用</span>'; }
  else if (s.paused) { cls += ' off'; tag = '<span class="tag warn">已暂停选号</span>'; }
  else if (s.cooling || cool > 0) { cls += ' cool'; tag = '<span class="tag warn">冷却 · ' + (cool > 0 ? dur(cool) : '—') + '</span>'; }
  else tag = '<span class="tag ok">可用</span>';

  let tierTag = '';
  if (s.tier) {
    const n = (String(s.tier).match(/Tier\s*(\d)/) || [])[1] || '5';
    tierTag = '<span class="tag tier t' + n + '" title="' + esc(s.tier) + '">T' + n + '</span>';
  }
  const chunk = s.is_active_chunk ? '<span class="tag chunk">主刷中</span>' : '';
  const pin = s.pinned ? '<span class="tag pinned" title="强制路由（重启后重置）">📌 强制</span>' : '';

  const uid = s.uid || '';
  const short = uid.length > 16 ? uid.slice(0, 16) + '…' : uid;
  const initial = esc(String(s.nickname || uid || '?').trim().slice(0, 1).toUpperCase());
  const name = s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>';
  // 企业版标记（S5 第 1 步，只读）：只认**非空** enterprise_id（trim 后），空/缺失一律不显示 ——
  // 与后端 Status.IsEnterprise() 同规则，避免'空白被当企业号'这类误标。
  const isEnt = typeof s.enterprise_id === 'string' && s.enterprise_id.trim() !== '';
  const entTag = isEnt ? ' <span class="tag">企业号</span>' : '';
  const realm = (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + entTag;
  const today = (s.daily_credits_used != null ? Number(s.daily_credits_used).toFixed(2) : '0.00');
  const total = (s.credits_used != null ? Number(s.credits_used).toFixed(1) : '0.0');
  const u48 = urgent48(s);
  const u48box = u48 > 0
    ? '<div class="urgent">🚨 48h 临期 ' + u48 + 'c · 优先清仓</div>'
    : expiryHint(s);
  const frozen = s.disabled || s.cooling || cool > 0;
  const pinBtn = s.pinned
    ? '<button class="xs warn" data-a="unpin" data-u="' + esc(uid) + '">取消强制</button>'
    : '<button class="xs ghost" data-a="pin" data-u="' + esc(uid) + '">强制使用</button>';

  return '<div class="acard' + cls + '">' +
    '<div class="top"><div class="ava">' + initial + '</div>' +
      '<div class="who"><div class="nm">' + name + realm + '</div><div class="id">' + esc(short) + '</div></div>' +
      '<div class="badges">' + tag + tierTag + chunk + pin + '</div></div>' +
    '<div class="asset"><span class="n">' + (s.credits == null ? '—' : s.credits) + '</span>' +
      '<span class="u">剩余积分' + (s.credits_total > 0 ? ' / ' + s.credits_total : '') + '</span></div>' +
    '<div class="meta-line"><span>' + ((s.packages || []).length) + ' 个积分包</span>' +
      '<span>累计 ' + total + 'c</span><span>更新 ' + hhmm(s.last_resource_sync) + '</span></div>' +
    '<div class="act">今日 <b>' + today + 'c</b> · ' + formatTokenCount(s.daily_tokens_used) + ' tok · ' + checkinBadge(s) + '</div>' +
    u48box +
    rateLimitRowsHtml(s.rate_limited_models, Date.now()) +
    '<div class="sec-title">近期到期<span class="grow"></span>' +
      '<button class="link" data-a="pk" data-u="' + esc(uid) + '">全部积分包 →</button></div>' +
    cardPkgs(s) +
    '<div class="foot"><span>成功 <b>' + (s.success_count || 0) + '</b></span>' +
      '<span>失败 <b style="color:var(--bad)">' + (s.err_total || 0) + '</b></span>' +
      '<span>在途 <b>' + (s.in_flight || 0) + '</b></span>' +
      '<span>最近 <b>' + ago(s.last_success) + '</b></span></div>' +
    '<div class="acts">' + pinBtn +
      '<button class="xs ghost" data-a="checkin" data-u="' + esc(uid) + '">签到</button>' +
      '<button class="xs ghost" data-a="balance" data-u="' + esc(uid) + '">余额</button>' +
      '<button class="xs ghost" data-a="tasks" data-u="' + esc(uid) + '">任务</button>' +
      ((frozen && !s.paused) ? '<button class="xs primary" data-a="revive" data-u="' + esc(uid) + '">解冻</button>'
              : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(uid) + '">恢复选号</button>'
                          : '<button class="xs ghost" data-a="pause" data-u="' + esc(uid) + '" title="退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额 / 旅行 / 成长任务">暂停选号</button>')) +
      (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(uid) + '">禁用</button>') +
      '<button class="xs ghost danger" data-a="remove" data-u="' + esc(uid) + '">移除</button></div>' +
  '</div>';
}
function renderAccCards(list) {
  const box = $('accCards');
  if (!box) return;
  if (!list.length) {
    box.innerHTML = '<div class="empty" style="grid-column:1/-1"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div>';
    return;
  }
  box.innerHTML = list.map(cardHTML).join('');
}
applyAccView();

/* 模型锁池（S1，上游 b3f92dd 的本地化）：只读展示当前被模型级限流/负缓存挡住的 (域, 模型)。 */
async function loadModelLocks() {
  const box = document.getElementById('locksBox'), meta = document.getElementById('locksMeta');
  if (!box) return;
  box.innerHTML = '<div class="hint">加载中…</div>';
  try {
    const d = await api('model-locks');
    const rows = (d && d.rows) || [];
    if (meta) meta.textContent = rows.length ? rows.length + ' 个 (域, 模型) 处于锁定' : '';
    if (!rows.length) { box.innerHTML = '<div class="hint">当前没有模型级锁定 ✓</div>'; return; }
    let h = '<table><thead><tr><th>域</th><th>模型</th><th>被挡账号</th><th>最早解封</th><th>原因</th></tr></thead><tbody>';
    for (const r of rows) {
      const until = r.until && r.until !== '0001-01-01T00:00:00Z'
        ? new Date(r.until).toLocaleString() : '（无重置时间）';
      const tag = r.not_found ? ' <span class="tag">换模型</span>' : '';
      h += '<tr><td>' + esc(r.realm) + '</td><td>' + esc(r.model) + tag + '</td><td>' + (r.accounts || 0) +
           '</td><td>' + esc(until) + '</td><td class="hint">' + esc(r.reason || '') + '</td></tr>';
    }
    box.innerHTML = h + '</tbody></table>';
  } catch (e) {
    box.innerHTML = '<div class="hint">加载失败：' + esc(String(e && e.message || e)) + '</div>';
  }
}

/* 请求指标（S3，上游 b2a93bb/4118b22/f8bca38 + 7d462bb）：只读；默认内存环，填 since 读归档。 */
async function loadReqLog() {
  const box = document.getElementById('rlBox'), meta = document.getElementById('rlMeta');
  if (!box) return;
  const since = (document.getElementById('rlSince') || {}).value || '';
  const limit = (document.getElementById('rlLimit') || {}).value || '200';
  const btn = document.getElementById('rlLoad');
  if (btn && !btn.dataset.bound) { btn.dataset.bound = '1'; btn.onclick = () => loadReqLog(); }
  box.innerHTML = '<div class="hint">加载中…</div>';
  try {
    const q = 'reqlog?limit=' + encodeURIComponent(limit) + (since ? '&since=' + encodeURIComponent(since) : '');
    const d = await api(q);
    if (meta) {
      meta.textContent = (d.enabled ? '' : '采集未启用 · ') + '来源 ' + (d.source || 'ring') +
        ' · ' + (d.count || 0) + ' 条 · 归档 ' + (d.archive_files || 0) + ' 文件 / ' +
        Math.round((d.archive_bytes || 0) / 1024) + ' KB';
    }
    const rows = d.rows || [];
    if (!d.enabled) { box.innerHTML = '<div class="hint">采集未启用（配置 <code>reqlog.enabled=true</code> 后重启）</div>'; return; }
    if (!rows.length) { box.innerHTML = '<div class="hint">暂无记录</div>'; return; }
    let h = '<table><thead><tr><th>时间</th><th>方法</th><th>路径</th><th>状态</th><th>耗时</th><th>模型</th><th>IP</th><th>UA</th></tr></thead><tbody>';
    for (const r of rows) {
      h += '<tr><td>' + esc(new Date(r.ts).toLocaleString()) + '</td><td>' + esc(r.method || '') +
           '</td><td>' + esc(r.path || '') + '</td><td>' + (r.status || 0) +
           '</td><td>' + (r.latency_ms || 0) + 'ms</td><td>' + esc(r.model || '') +
           '</td><td>' + esc(r.ip || '') + '</td><td class="hint">' + esc((r.ua || '').slice(0, 60)) + '</td></tr>';
    }
    box.innerHTML = h + '</tbody></table>';
  } catch (e) {
    box.innerHTML = '<div class="hint">加载失败：' + esc(String(e && e.message || e)) + '</div>';
  }
}

/* ── [2026-10] 检查更新（用户需求）────────────────────────────────────────
   网关在 Go 进程内取清单（net/http），**从不 spawn powershell/cmd** ⇒ 不弹控制台窗口。
   端点：GET /panel/api/update/check[?force=1] ⇒ {current,latest,available,url,error}
   入口：左下角「检查更新」；发现新版本时在右上角（明暗切换左侧）显示徽标。 */
function openUpdateDialog(d) {
  const veil = $('updVeil');
  if (!veil) return;
  const q = id => $(id) || null;   // 元素缺失时静默降级（面板测试的最小 DOM 桩）
  veil.style.display = 'block';
  (q('updBusy')||{}).style.display = d ? 'none' : 'block';
  (q('updBody')||{}).style.display = d ? 'block' : 'none';
  if (!d) return;
  (q('updCur')||{}).textContent = d.current || '-';
  (q('updLatest')||{}).textContent = d.latest || '-';
  const avail = !!d.available;
  (q('updAvail')||{}).textContent = d.error ? '检查失败' : (avail ? '发现新版本' : '已是最新');
  (q('updAvail')||{}).style.color = d.error ? 'var(--bad)' : (avail ? 'var(--warn)' : 'var(--ok)');
  (q('updMetaRow')||{}).style.display = d.latest ? 'block' : 'none';
  (q('updSize')||{}).textContent = d.size ? (d.size + ' B') : '-';
  (q('updSha')||{}).textContent = d.sha256 ? (d.sha256.slice(0, 16) + '…') : '-';
  (q('updErrRow')||{}).style.display = d.error ? 'block' : 'none';
  (q('updErr')||{}).textContent = d.error || '';
  const url = d.url || '';
  (q('updUrlRow')||{}).style.display = (avail && url) ? 'block' : 'none';
  (q('updUrl')||{}).value = url;
  (q('updCopy')||{}).style.display = (avail && url) ? 'inline-block' : 'none';
  (q('updTitle')||{}).textContent = d.error ? '检查更新（失败）' : (avail ? '发现新版本 ' + (d.latest || '') : '检查更新');
}
function updShow(d) {
  const b = $('updBadge'), t = $('updBadgeText');
  if (!b || !t) return;
  if (d && d.available) {
    b.hidden = false;
    t.textContent = '可更新 ' + (d.latest || '');
    b.title = '发现新版本 ' + (d.latest || '') + '（当前 ' + (d.current || '') + '）· 点击查看下载页';
    b.onclick = function () { openUpdateDialog(d); };   // 用户要求：点提示弹更新弹窗
  } else if (d && d.error) {
    b.hidden = false;
    t.textContent = '检查失败';
    b.title = '更新检查失败：' + d.error;
    b.onclick = null;
  } else {
    b.hidden = true; b.onclick = null;
  }
}
async function checkUpdatePanel(force) {
  const btn = $('btnCheckUpdate');
  if (btn) { btn.disabled = true; btn.title = '正在检查…'; }
  openUpdateDialog(null);   // 先弹出"正在检查…"（按钮与徽标统一体验）
  try {
    const d = await api('update/check' + (force ? '?force=1' : ''));
    window.__lastUpdate = d;
    updShow(d);
    openUpdateDialog(d);
    if (btn) btn.title = '已检查：当前 ' + (d.current || '?') + '，最新 ' + (d.latest || '?');
    return d;
  } catch (e) {
    updShow({ error: (e && e.message) ? e.message : String(e) });
    if (btn) btn.title = '检查失败';
  } finally { if (btn) btn.disabled = false; }
}
(function () {
  const b = $('btnCheckUpdate');
  if (b) b.onclick = function () { checkUpdatePanel(true); };
// 只在"确实有新版"时自动弹框（QA：每次开面板都弹太烦）；失败只更新按钮提示。
setTimeout(function () {
  checkUpdatePanel(false).then(function (d) {
    if (d && d.available) openUpdateDialog(d);
  });
}, 1200);
})();

/* 弹窗交互：关闭 / 复制 / 徽标与按钮共用 */
// 更新弹窗的 DOM 在 app.js 之后解析（index.html 里 <script> 在前）⇒ 必须等 DOMContentLoaded，
// 否则 close/copy 在 document 里查不到元素，弹窗关不掉（QA 实测）。
function bindUpdateDialog() {
  const veil = $('updVeil');
  if (!veil) return;
  const closeBtn = $('updClose'), copyBtn = $('updCopy'), urlInput = $('updUrl');
  // 空值守卫：面板测试用最小 DOM 桩加载本文件，元素缺失时不得抛异常
  if (closeBtn) closeBtn.onclick = function () { veil.style.display = 'none'; };
  veil.onclick = function (e) { if (e.target === veil) veil.style.display = 'none'; };
  if (urlInput) urlInput.onclick = function () { urlInput.select(); };   // updUrlSelect：替代被 CSP 拦掉的行内 onclick
  if (!copyBtn) return;
  copyBtn.onclick = async function () {
    const v = (urlInput && urlInput.value) || '';
    try { await navigator.clipboard.writeText(v); copyBtn.textContent = '已复制 ✓'; }
    catch (e) { if (urlInput) urlInput.select(); copyBtn.textContent = '请手动复制'; }
    setTimeout(function () { copyBtn.textContent = '复制下载地址'; }, 1800);
  };
  const badge = $('updBadge');
  if (badge) badge.onclick = function () {
    const d = (window.__lastUpdate || null);
    openUpdateDialog(d);
    if (!d) { checkUpdatePanel(false).then(function (r) { if (r) openUpdateDialog(r); }); }
  };
}
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', bindUpdateDialog);
} else {
  bindUpdateDialog();
}
