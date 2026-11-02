import { api, state, $, $$, fmt, agentBadge } from '/web/app.js';
import { sourcesInfo, mountSourcesEditor } from '/web/sources.js';

export default async function (root) {
  const cur = await api('/api/plan');
  const plans = Object.entries(cur.pricing.plans);
  const agents = (await api('/api/by-source')).map(r => r.source);
  const srcInfo = await sourcesInfo();   // null on the Python server: the Sources card is hidden
  const sourcesCard = srcInfo ? `<div class="card" style="margin-bottom:16px">
      <h2>SOURCES</h2>
      <p class="muted" style="margin:0 0 12px;text-transform:uppercase;font-size:12.5px;letter-spacing:0.04em">WHICH AGENT LOGS ARE SCANNED. UNTICK OR REMOVE A SOURCE TO STOP SCANNING IT (DATA ALREADY IMPORTED STAYS). SAVED TO <code>${fmt.htmlSafe(srcInfo.config_path)}</code>.${srcInfo.pinned ? ' <b>PINNED BY --projects-dir / CLAUDE_PROJECTS_DIR — EDITING IS DISABLED.</b>' : ''}</p>
      <div id="sources-editor"></div>
      <div id="sources-detected" class="muted src-hint"></div>
      <div class="flex" style="margin-top:12px">
        <button class="primary" id="sources-save" ${srcInfo.pinned ? 'disabled' : ''}>Save &amp; rescan</button>
        <button id="sources-scan">Rescan now</button>
        <span id="sources-msg" class="muted"></span>
      </div>
    </div>` : '';
  let arc = null;   // Go-only: Python server 404s and the Archive card stays hidden
  try { const r = await fetch('/api/archive'); if (r.ok) arc = await r.json(); } catch (e) { /* hidden */ }
  const hint = 'margin:0 0 12px;text-transform:uppercase;font-size:12.5px;letter-spacing:0.04em';
  const archiveCard = arc ? `<div class="card" style="margin-bottom:16px">
      <h2>ARCHIVE</h2>
      <p class="muted" style="${hint}">CLAUDE CODE DELETES ITS OWN SESSION FILES AFTER ABOUT 30 DAYS. THIS DASHBOARD KEEPS WHAT IT HAS ALREADY READ. SNAPSHOTS ARE COMPRESSED COPIES OF THAT HISTORY YOU CAN RESTORE ON ANY MACHINE.</p>
      <div class="flex" style="margin-bottom:12px">
        <label class="muted">FOLDER</label>
        <input id="arc-dir" type="text" style="flex:1;min-width:240px" value="${fmt.htmlSafe(arc.settings.dir)}">
        <label class="muted"><input id="arc-auto" type="checkbox" ${arc.settings.auto ? 'checked' : ''}> MONTHLY AUTO-SNAPSHOT</label>
      </div>
      <div class="flex" style="margin-bottom:12px">
        <label class="muted">KEEP IN LIVE DATABASE (MONTHS, 0 = FOREVER)</label>
        <input id="arc-keep" type="number" min="0" style="width:90px" value="${arc.settings.keep_months}">
        <button class="primary" id="arc-save">Save</button>
        <button id="arc-snap">Snapshot now</button>
        <button id="arc-prune" title="Takes a snapshot first, then deletes rows older than the keep period">Prune old rows</button>
        <span id="arc-msg" class="muted"></span>
      </div>
      <table><tbody>${arc.snapshots.length ? arc.snapshots.map(x => `
        <tr><td>${fmt.htmlSafe(x.name)}</td><td class="num">${(x.bytes/1048576).toFixed(1)} MB</td>
        <td><button class="ghost" data-restore="${fmt.htmlSafe(x.name)}">Restore / merge</button></td></tr>`).join('')
        : '<tr><td class="muted">NO SNAPSHOTS YET.</td></tr>'}</tbody></table>
      <div class="flex" style="margin-top:12px"><span class="muted">EXPORT ALL MESSAGES:</span>
        <a href="/api/archive/export?format=csv" download>CSV</a> <a href="/api/archive/export?format=json" download>JSON</a></div>
    </div>` : '';
  root.innerHTML = sourcesCard + archiveCard + `
    <div class="card">
      <h2>SETTINGS</h2>
      <h3 style="margin-top:16px">PLAN</h3>
      <p class="muted" style="margin:0 0 12px;text-transform:uppercase;font-size:12.5px;letter-spacing:0.04em">SETS HOW COST IS DISPLAYED. API MODE SHOWS PAY-PER-TOKEN RATES. SUBSCRIPTION MODES SHOW WHAT YOU ACTUALLY PAY EACH MONTH.</p>
      <div class="flex">
        <select id="plan">
          ${plans.map(([k,v]) => `<option value="${k}" ${k===cur.plan?'selected':''}>${v.label}${v.monthly?` — $${v.monthly}/mo`:''}</option>`).join('')}
        </select>
        <button class="primary" id="save">Save</button>
        <span id="msg" class="muted"></span>
      </div>

      <hr class="divider">

      <h3>PLAN PER AGENT</h3>
      <p class="muted" style="margin:0 0 12px;text-transform:uppercase;font-size:12.5px;letter-spacing:0.04em">EACH AGENT CAN BE ON A DIFFERENT SUBSCRIPTION. AGENTS WITHOUT ONE USE THE PLAN ABOVE.</p>
      <table>
        <tbody>${agents.map(a => `
          <tr><td>${agentBadge(a)}</td><td>
            <select data-agent="${fmt.htmlSafe(a)}">${plans.map(([k,v]) => `<option value="${k}" ${k===(cur.agent_plans[a]||cur.plan)?'selected':''}>${v.label}${v.monthly?` — $${v.monthly}/mo`:''}</option>`).join('')}</select>
          </td></tr>`).join('')}
        </tbody>
      </table>

      <hr class="divider">

      <h3>PRICING TABLE</h3>
      <p class="muted" style="margin:0 0 12px;text-transform:uppercase;font-size:12.5px;letter-spacing:0.04em">EDIT <code>pricing.json</code> IN THE PROJECT ROOT TO CHANGE RATES. RELOAD THE PAGE AFTER EDITING.</p>
      <table>
        <thead><tr><th>model</th><th class="num">input</th><th class="num">output</th><th class="num">cache read</th><th class="num">cache 5m</th><th class="num">cache 1h</th></tr></thead>
        <tbody>
          ${Object.entries(cur.pricing.models).map(([k,v]) => `
            <tr><td><span class="badge ${v.tier}">${k}</span></td>
              <td class="num">$${v.input.toFixed(2)}</td>
              <td class="num">$${v.output.toFixed(2)}</td>
              <td class="num">$${v.cache_read.toFixed(2)}</td>
              <td class="num">$${v.cache_create_5m.toFixed(2)}</td>
              <td class="num">$${v.cache_create_1h.toFixed(2)}</td>
            </tr>`).join('')}
        </tbody>
      </table>
      <p class="muted" style="margin-top:8px;font-size:12.5px">Rates per 1M tokens, USD.</p>

      <hr class="divider">

      <h3>PRIVACY</h3>
      <p class="muted" style="text-transform:uppercase;font-size:12.5px;letter-spacing:0.04em">PRESS <code>CMD/CTRL + B</code> ANYWHERE TO BLUR PROMPT TEXT AND OTHER SENSITIVE CONTENT FOR SCREENSHOTS.</p>
    </div>`;

  $$('select[data-agent]').forEach(sel => sel.addEventListener('change', async () => {
    await fetch('/api/plan', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ plan: sel.value, source: sel.dataset.agent }) });
    $('#msg').textContent = `Saved ${sel.dataset.agent}.`;
    $('#msg').style.color = 'var(--good)';
  }));

  $('#save').addEventListener('click', async () => {
    const plan = $('#plan').value;
    await fetch('/api/plan', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ plan }) });
    state.plan = plan;
    document.getElementById('plan-pill').textContent = plan;
    $('#msg').textContent = 'Saved.';
    $('#msg').style.color = 'var(--good)';
  });

  if (srcInfo) {
    // Rows = file entries, plus the implicit Claude default when the file has no claude row.
    const claudeDet = srcInfo.detected.find(d => d.name === 'claude');
    const rows = srcInfo.sources.map(e => ({ ...e }));
    if (!rows.some(r => r.name === 'claude') && claudeDet) rows.unshift({ name: 'claude', kind: 'claude', path: claudeDet.path, enabled: true });
    const ed = mountSourcesEditor($('#sources-editor'), rows, { locked: srcInfo.pinned });
    const missing = srcInfo.detected.filter(d => d.exists && !rows.some(r => r.name === d.name));
    $('#sources-detected').innerHTML = missing.length
      ? `DETECTED BUT NOT TRACKED: ${missing.map(d => `<button class="ghost" data-det="${fmt.htmlSafe(d.name)}">+ ${fmt.htmlSafe(d.name.toUpperCase())} (${d.files})</button>`).join(' ')}` : '';
    $$('[data-det]').forEach(b => b.addEventListener('click', () => {
      const d = srcInfo.detected.find(x => x.name === b.dataset.det);
      $('#sources-editor [data-add]').click();
      const tr = $$('#sources-editor [data-row]').pop();
      tr.querySelector('[data-f=name]').value = d.name; tr.querySelector('[data-f=kind]').value = d.kind; tr.querySelector('[data-f=path]').value = d.path;
      b.remove();
    }));
    const msg = (t, bad) => { const m = $('#sources-msg'); m.textContent = t; m.style.color = bad ? 'var(--bad)' : 'var(--good)'; };
    $('#sources-save').addEventListener('click', async () => {
      const err = ed.validate();
      if (err) return msg(err, true);
      const r = await fetch('/api/sources', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sources: ed.getRows() }) });
      if (r.ok) msg('SAVED — RESCANNING IN THE BACKGROUND.'); else msg((await r.json().catch(() => ({}))).error || `ERROR ${r.status}`, true);
    });
    $('#sources-scan').addEventListener('click', async () => {
      await fetch('/api/scan', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      msg('RESCANNING…');
    });
  }

  if (arc) {
    const say = (t, bad) => { const m = $('#arc-msg'); m.textContent = t; m.style.color = bad ? 'var(--bad)' : 'var(--good)'; };
    const post = async (path, body) => {
      const r = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(j.error || `ERROR ${r.status}`);
      return j;
    };
    const run = (fn, reload) => async () => { try { say(await fn()); if (reload) setTimeout(() => location.reload(), 900); } catch (e) { say(e.message, true); } };
    $('#arc-save').addEventListener('click', run(async () => {
      await post('/api/archive/settings', { dir: $('#arc-dir').value.trim(), auto: $('#arc-auto').checked, keep_months: parseInt($('#arc-keep').value || '0', 10) });
      return 'SAVED.';
    }));
    $('#arc-snap').addEventListener('click', run(async () => `SAVED ${(await post('/api/archive/snapshot')).name}`, true));
    $('#arc-prune').addEventListener('click', run(async () => {
      const j = await post('/api/archive/prune');
      return `REMOVED ${j.removed} ROWS. SNAPSHOT ${j.snapshot} KEPT.`;
    }));
    $$('[data-restore]').forEach(b => b.addEventListener('click', run(async () => `MERGED — ${(await post('/api/archive/import', { name: b.dataset.restore })).added} MESSAGES ADDED.`)));
  }
}
