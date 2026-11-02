import { api, fmt, agentBadges, agentColor } from '/web/app.js';
import { treemapChart, hbarChart } from '/web/charts.js';
import { bindRangeTabs, rangeTabs, rangeLabel, readRange, sinceIso, withSince } from '/web/range.js';

export default async function (root) {
  const range = readRange();
  const since = sinceIso(range);
  const skills = await api(withSince('/api/skills', since));

  const totalInvocations = skills.reduce((s, r) => s + r.invocations, 0);
  const loaded = skills.reduce((s, r) => s + (r.tokens_per_call == null ? 0 : r.tokens_per_call * r.invocations), 0);
  const agents = [...new Set(skills.flatMap(s => Object.keys(s.by_agent || {})))];
  const perAgent = a => skills.reduce((s, r) => s + ((r.by_agent || {})[a] || 0), 0);
  const topSkill = skills[0];

  root.innerHTML = `
    <div class="flex" style="margin-bottom:14px">
      <h2 style="margin:0">Skills</h2>
      <span class="muted">${rangeLabel(range)}</span>
      <div class="spacer"></div>
      ${rangeTabs(range)}
    </div>

    <div class="row cols-4">
      <div class="card kpi"><div class="label">Unique skills</div><div class="value">${fmt.int(skills.length)}</div></div>
      <div class="card kpi"><div class="label">Invocations</div><div class="value">${fmt.int(totalInvocations)}</div></div>
      <div class="card kpi" title="Σ invocations × SKILL.md size, where the size is known"><div class="label">Context loaded</div><div class="value">${loaded ? fmt.compact(loaded) : '—'}</div><div class="sub">tokens, where measurable</div></div>
      <div class="card kpi"><div class="label">Most used</div><div class="value" style="font-size:17px;margin-top:14px;overflow:hidden;text-overflow:ellipsis">${topSkill ? fmt.htmlSafe(topSkill.skill) : '—'}</div></div>
    </div>

    <div class="card" style="margin-top:14px">
      <h3>Skill tiles <span class="muted" style="font-weight:400;letter-spacing:.04em;text-transform:none">area = ${'<span id="size-label">invocations</span>'} · brightness = rank within agent</span></h3>
      <div class="flex" style="margin-bottom:10px;flex-wrap:wrap">
        <div class="legend" id="legend" style="margin:0">
          ${agents.map(a => `<button data-agent="${fmt.htmlSafe(a)}" style="--c:${agentColor(a)}">${fmt.htmlSafe(a.toUpperCase())} <b>${fmt.int(perAgent(a))}</b></button>`).join('')}
        </div>
        <div class="spacer"></div>
        <div class="range-tabs" id="size-by" title="What tile area represents">
          <button data-by="inv" class="active">Invocations</button>
          <button data-by="ctx">Context tokens</button>
        </div>
      </div>
      <div id="ch-tiles" style="height:400px"></div>
      <p class="muted" style="margin:8px 0 0;font-size:12.5px">Hover a tile for detail. Hover an agent chip to spotlight it; click to hide it.</p>
    </div>

    <div class="card" style="margin-top:14px">
      <h3>Top skills, ranked</h3>
      <div id="ch-skills" style="height:${Math.max(220, Math.min(12, skills.length) * 30 + 70)}px"></div>
    </div>

    <div class="card" style="margin-top:14px">
      <h3>All skills</h3>
      <p class="muted" style="margin:-4px 0 14px;font-size:13.5px">"Tokens per call" is the size of the skill's <code>SKILL.md</code> file — what the agent loads into context each time the skill is invoked (measured from your Claude skills folder; "—" when unknown).</p>
      <table>
        <thead><tr>
          <th>skill</th><th>agent (invocations)</th><th class="num">invocations</th>
          <th class="num">tokens per call</th><th class="num">sessions</th><th>last used</th>
        </tr></thead>
        <tbody>
          ${skills.map(s => `
            <tr>
              <td><span class="badge">${fmt.htmlSafe(s.skill)}</span></td>
              <td>${agentBadges(s.by_agent, fmt.int)}</td>
              <td class="num">${fmt.int(s.invocations)}</td>
              <td class="num">${s.tokens_per_call == null ? '<span class="muted">—</span>' : fmt.int(s.tokens_per_call)}</td>
              <td class="num">${fmt.int(s.sessions)}</td>
              <td class="mono">${fmt.ts(s.last_used)}</td>
            </tr>`).join('') || '<tr><td colspan="6" class="muted">no skills invoked in this range</td></tr>'}
        </tbody>
      </table>
    </div>
  `;

  bindRangeTabs(root);
  if (!skills.length) return;

  // ── tiles: one group per agent, one tile per skill ─────────────────────────
  let by = 'inv';
  const groupsFor = mode => agents.map(a => ({
    name: a, color: agentColor(a),
    items: skills.map(s => {
      const n = (s.by_agent || {})[a] || 0;
      const v = mode === 'ctx' ? n * (s.tokens_per_call || 0) : n;
      return v > 0 ? { name: s.skill, value: v, meta: { n, tpc: s.tokens_per_call, sessions: s.sessions, last: s.last_used } } : null;
    }).filter(Boolean).sort((x, y) => y.value - x.value),
  })).filter(g => g.items.length);

  const tipFor = d => {
    const m = d.meta;
    return `<div style="font-family:'Oswald',sans-serif;font-size:12px;letter-spacing:.08em;color:${agentColor(m.agent)}">${m.agent.toUpperCase()}</div>
      <div style="margin:3px 0 7px;font-weight:500">${fmt.htmlSafe(d.name)}</div>
      <div style="display:grid;grid-template-columns:auto auto;gap:2px 18px">
        <span style="color:#8A8E98">invocations</span><b>${fmt.int(m.n)}</b>
        <span style="color:#8A8E98">tokens / call</span><b>${m.tpc == null ? '—' : fmt.int(m.tpc)}</b>
        <span style="color:#8A8E98">context loaded</span><b>${m.tpc == null ? '—' : fmt.int(m.tpc * m.n)}</b>
        <span style="color:#8A8E98">sessions (all agents)</span><b>${fmt.int(m.sessions)}</b>
        <span style="color:#8A8E98">last used</span><b>${fmt.ts(m.last)}</b>
      </div>`;
  };

  let groups = groupsFor(by);
  const tiles = treemapChart(document.getElementById('ch-tiles'), { groups, tooltip: tipFor });
  const dim = new Set(), hide = new Set();

  // ── ranked bars ─────────────────────────────────────────────────────────────
  const top = skills.slice(0, 12);
  const bars = hbarChart(document.getElementById('ch-skills'), {
    categories: top.map(t => t.skill),
    series: agents.map(a => ({ name: a.toUpperCase(), color: agentColor(a), values: top.map(t => (t.by_agent || {})[a] || 0) })),
    tooltipExtra: i => `${top[i].tokens_per_call == null ? 'size unknown' : fmt.int(top[i].tokens_per_call) + ' tokens / call'} · ${fmt.int(top[i].sessions)} sessions`,
  });

  // ── legend ↔ both charts ────────────────────────────────────────────────────
  const legend = document.getElementById('legend');
  legend.querySelectorAll('button').forEach(btn => {
    const a = btn.dataset.agent, name = a.toUpperCase();
    btn.addEventListener('mouseenter', () => {
      dim.clear(); dim.add(a); tiles.refresh(dim, hide);
      bars.dispatchAction({ type: 'highlight', seriesName: name });
    });
    btn.addEventListener('mouseleave', () => {
      dim.clear(); tiles.refresh(dim, hide);
      bars.dispatchAction({ type: 'downplay', seriesName: name });
    });
    btn.addEventListener('click', () => {
      hide.has(a) ? hide.delete(a) : hide.add(a);
      btn.classList.toggle('off', hide.has(a));
      tiles.refresh(dim, hide);
      bars.dispatchAction({ type: 'legendToggleSelect', name });
    });
  });

  document.getElementById('size-by').querySelectorAll('button').forEach(b => b.addEventListener('click', () => {
    by = b.dataset.by;
    document.querySelectorAll('#size-by button').forEach(x => x.classList.toggle('active', x === b));
    document.getElementById('size-label').textContent = by === 'ctx' ? 'context tokens loaded' : 'invocations';
    groups = groupsFor(by);
    // rebuild the chart for the new data set
    tiles.chart.dispose();
    const el = document.getElementById('ch-tiles'); el.innerHTML = '';
    Object.assign(tiles, treemapChart(el, { groups, tooltip: tipFor, unit: by === 'ctx' ? ' tok' : '' }));
    tiles.refresh(dim, hide);
  }));
}
