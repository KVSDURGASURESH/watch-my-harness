import { api, fmt, state, agentBadge, agentBadges, seriesByAgent } from '/web/app.js';
import { donutChart, groupedBarChart, stackedBarChart } from '/web/charts.js';
import { bindRangeTabs, rangeTabs, rangeLabel, readRange, sinceIso, withSince } from '/web/range.js';

export default async function (root) {
  const range = readRange();
  const since = sinceIso(range);

  const [totals, projects, sessions, tools, daily, byModel, bySource] = await Promise.all([
    api(withSince('/api/overview', since)),
    api(withSince('/api/projects', since)),
    api(withSince('/api/sessions?limit=10', since)),
    api(withSince('/api/tools', since)),
    api(withSince('/api/daily', since)),
    api(withSince('/api/by-model', since)),
    api(withSince('/api/by-source', since)),
  ]);

  const cacheCreate =
    (totals.cache_create_5m_tokens || 0) +
    (totals.cache_create_1h_tokens || 0);

  const kpi = (label, compactVal, fullVal, cls = '') => `
    <div class="card kpi ${cls}">
      <div class="label">${label}</div>
      <div class="value" title="${fullVal}">${compactVal}</div>
    </div>`;

  root.innerHTML = `
    <div class="flex" style="margin-bottom:14px">
      <h2 style="margin:0;font-size:17px;letter-spacing:0.1em;font-weight:600">OVERVIEW</h2>
      <span class="muted" style="font-size:12.5px;text-transform:uppercase;letter-spacing:0.08em">${rangeLabel(range).toUpperCase()}</span>
      <div class="spacer"></div>
      ${rangeTabs(range)}
    </div>

    <div class="row cols-7">
      ${kpi('Sessions',     fmt.int(totals.sessions),       fmt.int(totals.sessions))}
      ${kpi('Turns',        fmt.int(totals.turns),          fmt.int(totals.turns))}
      ${kpi('Input',        fmt.compact(totals.input_tokens),       fmt.int(totals.input_tokens) + ' tokens')}
      ${kpi('Output',       fmt.compact(totals.output_tokens),      fmt.int(totals.output_tokens) + ' tokens')}
      ${kpi('Cache read',   fmt.compact(totals.cache_read_tokens),  fmt.int(totals.cache_read_tokens) + ' tokens')}
      ${kpi('Cache create', fmt.compact(cacheCreate),               fmt.int(cacheCreate) + ' tokens')}
      <div class="card kpi cost">
        <div class="label">Est. cost</div>
        <div class="value" title="${fmt.usd(totals.cost_usd)}">${fmt.usd(totals.cost_usd)}</div>
        ${planSubtitle()}
      </div>
    </div>

    ${bySource.length > 1 ? `
    <div class="card" style="margin-top:16px">
      <h3>BY AGENT</h3>
      <p class="muted" style="margin:-4px 0 10px;font-size:12.5px;text-transform:uppercase;letter-spacing:0.04em">EVERY CONFIGURED AGENT SIDE BY SIDE. PICK ONE IN THE TOP BAR TO FILTER ALL TABS. CODEX INPUT EXCLUDES CACHED TOKENS. AGENTS ON A SUBSCRIPTION SHOW THE FLAT FEE (SET PER AGENT IN SETTINGS); — MEANS NO PRICE FOR THAT AGENT'S MODELS.</p>
      <table>
        <thead><tr><th>Agent</th><th>Sessions</th><th>Turns</th><th>Input</th><th>Output</th><th>Cache read</th><th>Est. cost</th></tr></thead>
        <tbody>${bySource.map(r => `
          <tr><td>${agentBadge(r.source)}</td><td>${fmt.int(r.sessions)}</td><td>${fmt.int(r.turns)}</td>
          <td>${fmt.compact(r.input_tokens)}</td><td>${fmt.compact(r.output_tokens)}</td><td>${fmt.compact(r.cache_read_tokens)}</td>
          <td title="${r.unpriced_models ? r.unpriced_models + ' model(s) unpriced' : ''}">${costCell(r)}</td>
          </tr>`).join('')}
        </tbody>
      </table>
    </div>` : ''}

    <div class="card" style="margin-top:16px">
      <h3>MODELS — WHICH AGENT RAN THEM</h3>
      <table>
        <thead><tr><th>Model</th><th>Agent (turns)</th><th class="num">Input</th><th class="num">Output</th><th class="num">Cache read</th><th class="num">Est. cost</th></tr></thead>
        <tbody>${byModel.slice(0, 12).map(m => `
          <tr><td>${fmt.htmlSafe(m.model)}</td><td>${agentBadges(m.by_agent, fmt.int)}</td>
          <td class="num">${fmt.compact(m.input_tokens)}</td><td class="num">${fmt.compact(m.output_tokens)}</td>
          <td class="num">${fmt.compact(m.cache_read_tokens)}</td><td class="num">${fmt.usd(m.cost_usd)}</td></tr>`).join('')}
        </tbody>
      </table>
    </div>

    <details class="card glossary" style="margin-top:16px">
      <summary><h3 style="display:inline-block;margin:0">WHAT DO THESE NUMBERS MEAN?</h3><span class="muted" style="font-size:12.5px;text-transform:uppercase;letter-spacing:0.06em">— CLICK TO EXPAND</span></summary>
      <dl>
        <dt>Session</dt><dd>One run of an agent (Claude Code, Codex, …) from start to exit. Each session is a single <code>.jsonl</code> file; Codex subagents are grouped under their parent session.</dd>
        <dt>Turn</dt><dd>One message you sent to Claude. Each turn triggers a response (possibly with tool calls in between).</dd>
        <dt>Input tokens</dt><dd>The new text you (and tool results) sent to Claude this turn. Billed at the full input rate.</dd>
        <dt>Output tokens</dt><dd>The text Claude wrote back. Billed at the highest rate — usually the biggest cost driver per turn.</dd>
        <dt>Cache read</dt><dd>Tokens Claude re-used from a cache (your CLAUDE.md, previously-read files, the conversation so far). ~10× cheaper than fresh input. High cache-read counts = good cost hygiene.</dd>
        <dt>Cache create</dt><dd>Writing something into the cache for the first time. One-time cost; pays off on the next turn.</dd>
        <dt>Billable tokens</dt><dd>Input + Output + Cache create. Cache reads are billed separately (and much cheaper).</dd>
      </dl>
    </details>

    <div class="row cols-2" style="margin-top:16px">
      <div class="card">
        <h3>DAILY WORK</h3>
        <p class="muted" style="margin:-4px 0 10px;font-size:12.5px;text-transform:uppercase;letter-spacing:0.04em">TOKENS YOU PAID FOR: WHAT YOU SENT (<b>INPUT</b>), WHAT CLAUDE WROTE (<b>OUTPUT</b>), AND WHAT GOT STORED FOR RE-USE (<b>CACHE CREATE</b>).</p>
        <div id="ch-daily-billable" style="height:260px"></div>
      </div>
      <div class="card">
        <h3>DAILY CACHE READS</h3>
        <p class="muted" style="margin:-4px 0 10px;font-size:12.5px;text-transform:uppercase;letter-spacing:0.04em"><b>CACHE READS</b> ARE CHEAP RE-USES OF THINGS CLAUDE ALREADY SAW. THEY COST ~10× LESS THAN REGULAR INPUT TOKENS — HIGH NUMBERS HERE = GOOD.</p>
        <div id="ch-daily-cache" style="height:260px"></div>
      </div>
    </div>

    <div class="row cols-2" style="margin-top:16px">
      <div class="card"><h3>TOKENS BY PROJECT</h3><div id="ch-projects" style="height:320px"></div></div>
      <div class="card">
        <h3>TOKEN USAGE BY MODEL</h3>
        <p class="muted" style="margin:-4px 0 4px;font-size:12.5px;text-transform:uppercase;letter-spacing:0.04em">SHARE OF BILLABLE TOKENS PER MODEL, ACROSS ALL AGENTS IN VIEW.</p>
        <div id="ch-model" style="height:300px"></div>
      </div>
    </div>

    <div class="row cols-2" style="margin-top:16px">
      <div class="card"><h3>TOP TOOLS (BY CALL COUNT, SPLIT BY AGENT)</h3><div id="ch-tools" style="height:320px"></div></div>
      <div class="card">
        <h3 style="display:flex;align-items:center"><span>RECENT SESSIONS</span><span class="spacer"></span><a href="#/sessions" style="font-weight:500;font-size:12.5px;text-transform:uppercase;letter-spacing:0.08em">ALL →</a></h3>
        <table>
          <thead><tr><th>started</th><th>agent</th><th>project</th><th class="num">tokens</th></tr></thead>
          <tbody>
            ${sessions.map(s => `
              <tr>
                <td class="mono">${fmt.ts(s.started)}</td>
                <td>${agentBadge(s.source)}</td>
                <td><a href="#/sessions/${encodeURIComponent(s.session_id)}">${fmt.htmlSafe(s.project_name || s.project_slug)}</a></td>
                <td class="num">${fmt.compact(s.tokens)}</td>
              </tr>`).join('') || '<tr><td colspan="4" class="muted">no sessions in this range</td></tr>'}
          </tbody>
        </table>
      </div>
    </div>
  `;

  bindRangeTabs(root);

  // Your daily work — billable tokens (input + output + cache create)
  stackedBarChart(document.getElementById('ch-daily-billable'), {
    categories: daily.map(d => d.day),
    series: [
      { name: 'input',        values: daily.map(d => d.input_tokens),        color: '#7AA7FF' },
      { name: 'output',       values: daily.map(d => d.output_tokens),       color: '#B084FF' },
      { name: 'cache create', values: daily.map(d => d.cache_create_tokens), color: '#D9B46A' },
    ],
  });

  // Daily cache reads (separate — scale is 100× larger)
  stackedBarChart(document.getElementById('ch-daily-cache'), {
    categories: daily.map(d => d.day),
    series: [
      { name: 'cache read', values: daily.map(d => d.cache_read_tokens), color: '#4FD1C5' },
    ],
  });

  // by-model doughnut
  donutChart(document.getElementById('ch-model'),
    byModel.map(m => ({
      name: fmt.modelShort(m.model) || 'unknown',
      value: (m.input_tokens || 0) + (m.output_tokens || 0)
           + (m.cache_create_5m_tokens || 0) + (m.cache_create_1h_tokens || 0),
    })).filter(d => d.value > 0),
  );

  // tokens by project — input vs output
  const topProjects = projects.slice(0, 8);
  groupedBarChart(document.getElementById('ch-projects'), {
    categories: topProjects.map(p => {
      const name = p.project_name || p.project_slug;
      return name.length > 20 ? name.slice(0, 19) + '…' : name;
    }),
    series: [
      { name: 'input',  values: topProjects.map(p => p.input_tokens  || 0), color: '#7AA7FF' },
      { name: 'output', values: topProjects.map(p => p.output_tokens || 0), color: '#B084FF' },
    ],
  });

  // top tools
  const topTools = tools.slice(0, 8);
  stackedBarChart(document.getElementById('ch-tools'), {
    categories: topTools.map(t => t.tool_name),
    series: seriesByAgent(topTools),
  });
}

// Subscription agents show the flat fee; the pay-per-token figure (when the models are priced) rides along.
function costCell(r) {
  const api = r.cost_usd == null ? '' : fmt.usd(r.cost_usd) + (r.unpriced_models ? ' +' : '');
  if (!r.plan_monthly) return api || '—';
  return `<b>$${fmt.int(r.plan_monthly)}/MO</b> <span class="muted">${fmt.htmlSafe(r.plan_label.toUpperCase())}${api ? ' · API-EQUIV ' + api : ''}</span>`;
}

function planSubtitle() {
  if (!state.pricing || state.plan === 'api') return '';
  const p = state.pricing.plans[state.plan];
  if (!p || !p.monthly) return '';
  return `<div class="sub">pay $${p.monthly}/mo on ${fmt.htmlSafe(p.label)}</div>`;
}
