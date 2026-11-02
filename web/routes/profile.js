import { api, fmt, agentBadge, readHashParam } from '/web/app.js';

const DIMENSIONS = [
  { key: 'steering',    label: 'Steering' },
  { key: 'execution',   label: 'Execution' },
  { key: 'engineering', label: 'Engineering' },
  { key: 'planning',    label: 'Planning' },
];

export default async function (root) {
  const profile = await api('/api/profile');
  const scope = readHashParam('source');
  // With no agent picked, also profile each agent separately: same four scores, so
  // the harnesses can be compared directly.
  const per = scope ? [] : await Promise.all(
    (await api('/api/by-source')).map(async r => ({ source: r.source, p: await api('/api/profile?source=' + encodeURIComponent(r.source)) })));
  const perAgent = per.length > 1 ? `
    <div class="card" style="margin-top:16px">
      <h3>BY AGENT</h3>
      <p class="muted" style="margin:-4px 0 10px;font-size:12.5px;text-transform:uppercase;letter-spacing:0.04em">THE SAME FOUR SCORES, COMPUTED SEPARATELY PER HARNESS.</p>
      <table>
        <thead><tr><th>Agent</th><th class="num">Prompts</th>${DIMENSIONS.map(d => `<th class="num">${d.label}</th>`).join('')}<th>Archetype</th></tr></thead>
        <tbody>${per.map(({ source, p }) => p.insufficient_data
          ? `<tr><td>${agentBadge(source)}</td><td class="num">${fmt.int(p.turns)}</td><td colspan="${DIMENSIONS.length + 1}" class="muted">NEEDS ${fmt.int(p.minimum)}+ PROMPTS</td></tr>`
          : `<tr><td>${agentBadge(source)}</td><td class="num">${fmt.int(p.turns)}</td>${DIMENSIONS.map(d => `<td class="num">${p.scores[d.key]}</td>`).join('')}<td>${fmt.htmlSafe(p.archetype)}</td></tr>`).join('')}
        </tbody>
      </table>
    </div>` : '';

  if (profile.insufficient_data) {
    root.innerHTML = `
      <div class="card">
        <h2>Builder Profile</h2>
        <p class="muted">Not enough data yet — ${fmt.int(profile.turns)} of ${fmt.int(profile.minimum)} prompts needed. Keep using your agents and check back.</p>
      </div>${perAgent}`;
    return;
  }

  root.innerHTML = `
    <div class="card">
      <h2>Builder Profile ${agentBadgeScope(scope)}</h2>
      <p class="muted" style="margin:-8px 0 4px">Archetype: <strong style="color:var(--accent)">${fmt.htmlSafe(profile.archetype)}</strong></p>
      <p class="muted" style="font-size:12.5px;margin:0 0 18px">Rule-based signals from your own session history — not a comparison to other users.</p>
      ${DIMENSIONS.map(d => `
        <div class="score-row">
          <span class="dim-label">${d.label}</span>
          <span class="score-bar-track"><span class="score-bar-fill" style="width:${profile.scores[d.key]}%"></span></span>
          <span class="dim-value">${fmt.int(profile.scores[d.key])}</span>
        </div>`).join('')}
    </div>${perAgent}`;
}

function agentBadgeScope(scope) {
  return scope ? agentBadge(scope) : '<span class="badge">ALL AGENTS</span>';
}
