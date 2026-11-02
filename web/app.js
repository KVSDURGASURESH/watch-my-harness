// app.js — router, state, fetch helpers

export const $  = (sel, root=document) => root.querySelector(sel);
export const $$ = (sel, root=document) => Array.from(root.querySelectorAll(sel));

const COMPACT = new Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 });
export const fmt = {
  int:   n => (n ?? 0).toLocaleString(),
  compact: n => COMPACT.format(n ?? 0),
  usd:   n => n == null ? '—' : '$' + Number(n).toFixed(2),
  usd4:  n => n == null ? '—' : '$' + Number(n).toFixed(4),
  pct:   n => n == null ? '—' : (n * 100).toFixed(0) + '%',
  short: (s, n=80) => s == null ? '' : (s.length > n ? s.slice(0, n - 1) + '…' : s),
  htmlSafe: s => (s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
  modelClass: m => {
    const s = (m || '').toLowerCase();
    if (s.includes('opus'))   return 'opus';
    if (s.includes('sonnet')) return 'sonnet';
    if (s.includes('haiku'))  return 'haiku';
    if (s.includes('gpt') || s.includes('codex')) return 'gpt';
    return '';
  },
  modelShort: m => (m || '').replace('claude-', ''),
  ts: t => (t || '').slice(0, 16).replace('T', ' '),
};

// Endpoints that must not be narrowed to the agent picked in the top bar.
const SOURCE_EXEMPT = ['/api/by-source', '/api/plan', '/api/scan', '/api/stream', '/api/sources', '/api/setup', '/api/config'];

export async function api(path, opts) {
  const source = readHashParam('source');
  if (source && !(opts && opts.method) && !SOURCE_EXEMPT.some(p => path.startsWith(p)) && path.startsWith('/api/')) {
    path += (path.includes('?') ? '&' : '?') + 'source=' + encodeURIComponent(source);
  }
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error(`${path} → ${r.status}`);
  return r.json();
}

export const state = { plan: 'api', pricing: null };

// One colour per agent, used for badges and chart series so a harness looks the same everywhere.
const AGENT_COLORS = { claude: '#E2744D', codex: '#3DDC97', hermes: '#B084FF' };
const AGENT_FALLBACK = ['#7AA7FF', '#F472B6', '#FF8A5B', '#4FD1C5'];
export function agentColor(name) {
  if (AGENT_COLORS[name]) return AGENT_COLORS[name];
  let h = 0; for (const ch of String(name)) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return AGENT_FALLBACK[h % AGENT_FALLBACK.length];
}
export const agentBadge = (name, extra = '') =>
  `<span class="badge agent" style="--agent:${agentColor(name)}">${fmt.htmlSafe(String(name || 'claude').toUpperCase())}${extra ? ' ' + extra : ''}</span>`;
// Badges for a by_agent map {agent: count}: which harnesses used this skill/tool/model/project, and how much.
export const agentBadges = (byAgent, f = fmt.compact) =>
  Object.entries(byAgent || {}).sort((a, b) => b[1] - a[1])
    .map(([a, n]) => agentBadge(a, `<b>${f(n)}</b>`)).join(' ');
// Stacked-chart series, one per agent, over rows that carry by_agent.
export function seriesByAgent(rows) {
  const agents = [...new Set(rows.flatMap(r => Object.keys(r.by_agent || {})))];
  return agents.map(a => ({ name: a.toUpperCase(), color: agentColor(a), values: rows.map(r => (r.by_agent || {})[a] || 0) }));
}

// Query params ride along inside the hash, e.g. #/overview?range=7d.
export function readHashParam(name) {
  return new URLSearchParams(location.hash.split('?')[1] || '').get(name);
}

export function writeHashParam(name, value) {
  // An empty hash means the default route, which is /overview.
  const [rawBase, rawQuery] = location.hash.replace(/^#/, '').split('?');
  const params = new URLSearchParams(rawQuery || '');
  if (value == null || value === '') params.delete(name); else params.set(name, value);
  const q = params.toString();
  location.hash = `#${rawBase || '/overview'}${q ? '?' + q : ''}`;
}

const ROUTES = {
  '/overview': () => import('/web/routes/overview.js'),
  '/prompts':  () => import('/web/routes/prompts.js'),
  '/sessions': () => import('/web/routes/sessions.js'),
  '/projects': () => import('/web/routes/projects.js'),
  '/profile':  () => import('/web/routes/profile.js'),
  '/skills':   () => import('/web/routes/skills.js'),
  '/tips':     () => import('/web/routes/tips.js'),
  '/settings': () => import('/web/routes/settings.js'),
};

function buildTopbar() {
  const wrap = document.createElement('header');
  wrap.className = 'topbar';
  wrap.innerHTML = `
    <div class="brand">WATCH MY HARNESS</div>
    <nav>
      ${Object.keys(ROUTES).map((p, i) => `<a href="#${p}" data-route="${p}"><i>${String(i + 1).padStart(2, '0')}</i>${p.slice(1).toUpperCase()}</a>`).join('')}
    </nav>
    <div class="spacer"></div>
    <select class="pill" id="source-select" title="Filter every tab to one agent" hidden></select>
    <span class="pill" id="plan-pill">API</span>
    <span class="led" id="led" title="Live: lights up when a scan ingests new data"></span>
    <span class="pill muted" title="Press 1-8 to switch tabs. Cmd/Ctrl+B blurs sensitive text.">1-8 · ⌘B BLUR</span>
  `;
  document.body.prepend(wrap);
}

// Agent picker: only shown once more than one agent has data.
async function fillSourcePicker() {
  const sel = $('#source-select');
  let rows = [];
  try { rows = await api('/api/by-source'); } catch { return; }
  if (rows.length < 2) { sel.hidden = true; return; }
  const cur = readHashParam('source') || '';
  sel.innerHTML = `<option value="">ALL AGENTS</option>` +
    rows.map(r => `<option value="${fmt.htmlSafe(r.source)}"${r.source === cur ? ' selected' : ''}>${fmt.htmlSafe(r.source.toUpperCase())}</option>`).join('');
  sel.hidden = false;
  sel.onchange = () => writeHashParam('source', sel.value);
}

function setActiveTab(routeKey) {
  $$('header.topbar nav a').forEach(a => a.classList.toggle('active', a.dataset.route === routeKey));
}

const SKELETON = `<div class="skeleton" aria-busy="true" aria-label="Loading">
  <div class="sk sk-title"></div>
  <div class="sk-row"><div class="sk sk-card"></div><div class="sk sk-card"></div><div class="sk sk-card"></div><div class="sk sk-card"></div></div>
  <div class="sk sk-chart"></div>
  <div class="sk sk-chart short"></div>
</div>`;

// Filters (range, agent) stay put when you switch tabs.
const STICKY = ['range', 'source'];
function stickyQuery() {
  const cur = new URLSearchParams(location.hash.split('?')[1] || '');
  const keep = new URLSearchParams();
  STICKY.forEach(k => { if (cur.get(k)) keep.set(k, cur.get(k)); });
  const q = keep.toString();
  return q ? '?' + q : '';
}
function syncNavLinks() {
  const q = stickyQuery();
  $$('header.topbar nav a').forEach(a => { a.href = `#${a.dataset.route}${q}`; });
}

// `silent` = background refresh (new scan data): keep the current page on screen, no skeleton.
async function render(silent = false) {
  const hash = location.hash.replace(/^#/, '') || '/overview';
  const path = hash.split('?')[0];
  let key = path;
  if (path.startsWith('/sessions/')) key = '/sessions';
  setActiveTab(key);
  syncNavLinks();
  const loader = ROUTES[key] || ROUTES['/overview'];
  const mod = await loader();
  const app = $('#app');
  if (!silent) app.innerHTML = SKELETON;
  const scrollY = silent ? window.scrollY : 0;
  // Routes fetch first and write root.innerHTML once data is in, so on a silent refresh the old
  // page stays visible until the new one swaps in. (Not a detached node: charts need layout.)
  try {
    await mod.default(app);
    if (silent) window.scrollTo(0, scrollY);
  } catch (e) {
    app.innerHTML = `<div class="card"><h2>Error</h2><pre>${fmt.htmlSafe(String(e.stack || e))}</pre></div>`;
  }
}

// Ask once: skipped when this browser already answered or the server already has a saved plan
// (a fresh browser / private window / container restart must not re-ask and overwrite it).
async function firstRun(configured) {
  if (configured || localStorage.getItem('td.plan-set')) return;
  const plans = Object.entries(state.pricing.plans);
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML = `
    <div class="modal">
      <h2>WELCOME — PICK YOUR PLAN</h2>
      <p>THIS SETS HOW COSTS ARE DISPLAYED. CHANGE IT LATER IN SETTINGS.</p>
      <select id="firstplan" style="width:100%">
        ${plans.map(([k,v]) => `<option value="${k}">${v.label}${v.monthly ? ` — $${v.monthly}/mo` : ''}</option>`).join('')}
      </select>
      <div class="actions">
        <div class="spacer"></div>
        <button class="primary" id="firstsave">Continue</button>
      </div>
    </div>`;
  document.body.appendChild(overlay);
  await new Promise(res => $('#firstsave', overlay).addEventListener('click', async () => {
    const plan = $('#firstplan', overlay).value;
    await fetch('/api/plan', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ plan }) });
    localStorage.setItem('td.plan-set', '1');
    overlay.remove();
    res();
  }));
  state.plan = (await api('/api/plan')).plan;
}

async function boot() {
  buildTopbar();
  let planResp = await api('/api/plan');
  state.pricing = planResp.pricing;

  // First run (Go build only): choose which harnesses to track, then watch the first scan.
  try {
    const r = await fetch('/api/sources');
    if (r.ok) {
      const info = await r.json();
      if (!info.setup_complete) {
        await (await import('/web/setup.js')).default(info, state.pricing.plans);
        planResp = await api('/api/plan');
      }
    }
  } catch (e) { console.warn('setup check failed', e); }

  state.plan = planResp.plan;
  $('#plan-pill').textContent = state.plan;

  await firstRun(planResp.configured);
  fillSourcePicker();

  window.addEventListener('hashchange', () => render());
  await render();

  // Privacy blur (Cmd+B / Ctrl+B)
  window.addEventListener('keydown', e => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'b') {
      e.preventDefault();
      document.body.classList.toggle('privacy-on');
    }
  });

  // Keyboard: 1-8 jump between tabs (ignored while typing).
  window.addEventListener('keydown', e => {
    if (e.metaKey || e.ctrlKey || e.altKey || /^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName)) return;
    const i = parseInt(e.key, 10);
    const keys = Object.keys(ROUTES);
    if (i >= 1 && i <= keys.length) location.hash = `#${keys[i - 1]}${stickyQuery()}`;
  });

  // SSE diff stream — the LED in the top bar shows the connection and blinks on each scan.
  const led = $('#led');
  const beat = () => { led.classList.add('beat'); setTimeout(() => led.classList.remove('beat'), 900); };
  try {
    const es = new EventSource('/api/stream');
    es.onopen = () => led.classList.add('live');
    es.onerror = () => led.classList.remove('live');
    es.onmessage = ev => {
      try {
        const evt = JSON.parse(ev.data);
        if (evt.type === 'scan') { fillSourcePicker(); render(true); beat(); }
      } catch {}
    };
  } catch {}
}

boot();
