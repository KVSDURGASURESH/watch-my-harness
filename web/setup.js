// setup.js — first-run screen: pick the harnesses to track, add custom sources (e.g. OpenClaw via the
// generic kind), watch the first scan, land on Overview. Shown only when the server has no
// setup-complete flag (Go build); resolves when the user is done.
import { $, fmt } from '/web/app.js';
import { mountSourcesEditor } from '/web/sources.js';

const NICE = { claude: 'CLAUDE CODE', codex: 'CODEX', hermes: 'HERMES' };

export default function runSetup(info, plans) {
  return new Promise(resolve => {
    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay setup';
    const detected = info.detected || [];
    const rows = detected.map(d => ({
      name: d.name, kind: d.kind, path: d.path, enabled: true,
      note: d.exists ? `FOUND — ${d.files} ${d.kind === 'hermes' ? 'DATABASE' : 'LOG'} FILE${d.files === 1 ? '' : 'S'}` : 'NOT FOUND YET — WILL BE PICKED UP IF IT APPEARS; UNTICK TO IGNORE',
    }));
    const planOpts = Object.entries(plans || {}).map(([k, v]) => `<option value="${k}">${fmt.htmlSafe(v.label)}${v.monthly ? ` — $${v.monthly}/mo` : ''}</option>`).join('');
    overlay.innerHTML = `
      <div class="modal setup-card">
        <div data-step="pick">
          <h2>WELCOME — WHAT SHOULD I TRACK?</h2>
          <p>EVERYTHING STAYS ON THIS MACHINE. TICK THE AGENT HARNESSES TO READ, OR ADD YOUR OWN. YOU CAN CHANGE THIS LATER IN SETTINGS → SOURCES.</p>
          <div id="setup-sources"></div>
          <div class="flex" style="margin-top:16px">
            <span class="muted" style="text-transform:uppercase;font-size:12.5px;letter-spacing:.06em">PLAN</span>
            <select id="setup-plan">${planOpts}</select>
          </div>
          <div id="setup-err" class="muted" style="color:var(--bad);margin-top:10px"></div>
          <div class="actions"><div class="spacer"></div><button class="primary" id="setup-go">START SCAN</button></div>
        </div>
        <div data-step="scan" hidden>
          <h2 id="scan-title">SCANNING…</h2>
          <p id="scan-sub">READING YOUR SESSION LOGS. THIS CAN TAKE A MINUTE ON A LARGE HISTORY.</p>
          <div class="bar"><i id="scan-bar"></i></div>
          <div class="muted" id="scan-meta" style="margin-top:8px;text-transform:uppercase;font-size:12.5px;letter-spacing:.06em"></div>
          <div class="actions"><div class="spacer"></div><button class="primary" id="scan-done" hidden>OPEN DASHBOARD</button></div>
        </div>
      </div>`;
    document.body.appendChild(overlay);
    const ed = mountSourcesEditor($('#setup-sources', overlay), rows);
    const planSel = $('#setup-plan', overlay);
    if (planSel) planSel.value = 'api';

    $('#setup-go', overlay).addEventListener('click', async () => {
      const err = ed.validate();
      $('#setup-err', overlay).textContent = err;
      if (err) return;
      const plan = planSel ? planSel.value : undefined;
      const t0 = Date.now() - 2000;
      const r = await fetch('/api/setup/complete', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sources: ed.getRows(), plan }) });
      if (!r.ok) { $('#setup-err', overlay).textContent = `COULD NOT SAVE: ${(await r.json().catch(() => ({}))).error || r.status}`; return; }
      localStorage.setItem('td.plan-set', '1');
      overlay.querySelector('[data-step=pick]').hidden = true;
      overlay.querySelector('[data-step=scan]').hidden = false;
      const poll = async () => {
        let s; try { s = await (await fetch('/api/scan-status')).json(); } catch { return setTimeout(poll, 800); }
        const total = s.files_total || 0, done = s.files_done || 0;
        $('#scan-bar', overlay).style.width = (total ? Math.min(100, done / total * 100) : (s.running ? 5 : 100)) + '%';
        $('#scan-meta', overlay).textContent =
          `${fmt.int(done)} / ${fmt.int(total)} FILES · ${fmt.int(s.messages)} MESSAGES · ${fmt.int(s.tools)} TOOL CALLS${s.current ? ' · ' + String(s.current).toUpperCase() : ''}`;
        const finished = !s.running && s.finished_ms >= t0;
        if (!finished) return setTimeout(poll, 500);
        $('#scan-bar', overlay).style.width = '100%';
        $('#scan-title', overlay).textContent = s.error ? 'SCAN FINISHED WITH A WARNING' : 'ALL SET';
        $('#scan-sub', overlay).textContent = s.error ? String(s.error).toUpperCase() : `IMPORTED ${fmt.int(s.messages)} MESSAGES FROM ${fmt.int(done)} FILES.`;
        const btn = $('#scan-done', overlay);
        btn.hidden = false;
        btn.addEventListener('click', () => { overlay.remove(); resolve(); });
        if (!s.error) setTimeout(() => { if (overlay.isConnected) { overlay.remove(); resolve(); } }, 1800);
      };
      poll();
    });
  });
}
