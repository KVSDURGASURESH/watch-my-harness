// sources.js — editable list of log sources (shared by Settings and the first-run setup).
// Only the Go build serves /api/sources; sourcesInfo() returns null elsewhere so callers can hide the UI.
import { fmt } from '/web/app.js';

export async function sourcesInfo() {
  try {
    const r = await fetch('/api/sources');
    return r.ok ? await r.json() : null;
  } catch { return null; }
}

const KIND_HINT = {
  claude: 'CLAUDE CODE TRANSCRIPTS (.jsonl FOLDER)',
  codex: 'CODEX CLI / DESKTOP ROLLOUTS (.jsonl FOLDER)',
  hermes: 'HERMES — FOLDER WITH state.db (+ profiles/*/state.db)',
  generic: 'NORMALISED JSONL: ONE OBJECT PER API CALL, E.G. OPENCLAW',
};
export const GENERIC_FORMAT = '{"session_id","timestamp","model","input_tokens","output_tokens","cache_read_tokens"?,"cwd"?,"project"?,"prompt"?,"tools"?:["name"]}';

const rowHtml = (r, kinds, locked) => `
  <tr data-row>
    <td style="width:34px"><input type="checkbox" data-f="enabled" ${r.enabled !== false ? 'checked' : ''} ${locked ? 'disabled' : ''} aria-label="Scan this source"></td>
    <td style="width:130px"><input type="text" data-f="name" value="${fmt.htmlSafe(r.name || '')}" placeholder="name" ${locked ? 'disabled' : ''} style="width:100%"></td>
    <td style="width:120px"><select data-f="kind" ${locked ? 'disabled' : ''}>${kinds.map(k => `<option value="${k}" ${k === r.kind ? 'selected' : ''}>${k}</option>`).join('')}</select></td>
    <td><input type="text" data-f="path" value="${fmt.htmlSafe(r.path || '')}" placeholder="~/path/to/logs" ${locked ? 'disabled' : ''} style="width:100%">
        <div class="muted src-hint" data-hint>${fmt.htmlSafe(r.note || KIND_HINT[r.kind] || '')}</div></td>
    <td style="width:60px">${locked ? '' : '<button class="ghost" data-del title="Remove (stops scanning; imported data stays)">✕</button>'}</td>
  </tr>`;

// Mounts the editor into `root`. rows: [{name,kind,path,enabled?,note?}]. Returns {getRows, validate}.
export function mountSourcesEditor(root, rows, { kinds = ['claude', 'codex', 'hermes', 'generic'], locked = false } = {}) {
  root.innerHTML = `
    <table class="src-table"><tbody></tbody></table>
    ${locked ? '' : '<div class="flex" style="margin-top:10px"><button class="ghost" data-add>+ ADD SOURCE</button></div>'}
    <div class="muted src-hint" data-generic hidden>GENERIC JSONL LINE: <code>${fmt.htmlSafe(GENERIC_FORMAT)}</code></div>`;
  const body = root.querySelector('tbody');
  const refresh = () => {
    root.querySelector('[data-generic]').hidden = ![...body.querySelectorAll('[data-f=kind]')].some(s => s.value === 'generic');
  };
  const add = r => {
    body.insertAdjacentHTML('beforeend', rowHtml(r, kinds, locked));
    refresh();
  };
  rows.forEach(add);
  root.addEventListener('click', e => {
    if (e.target.closest('[data-del]')) { e.target.closest('[data-row]').remove(); refresh(); }
    if (e.target.closest('[data-add]')) add({ name: '', kind: 'generic', path: '', enabled: true });
  });
  root.addEventListener('change', e => {
    if (e.target.dataset.f === 'kind') {
      e.target.closest('[data-row]').querySelector('[data-hint]').textContent = KIND_HINT[e.target.value] || '';
      refresh();
    }
  });
  const getRows = () => [...body.querySelectorAll('[data-row]')].map(tr => {
    const g = f => tr.querySelector(`[data-f=${f}]`);
    const on = g('enabled').checked;
    return { name: g('name').value.trim(), kind: g('kind').value, path: g('path').value.trim(), ...(on ? {} : { enabled: false }) };
  });
  const validate = () => {
    const seen = new Set();
    for (const r of getRows()) {
      if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$/.test(r.name)) return `BAD NAME "${r.name}" — LETTERS, DIGITS, _ . - ONLY`;
      if (!r.path) return `SOURCE "${r.name}" NEEDS A PATH`;
      if (seen.has(r.name)) return `DUPLICATE NAME "${r.name}"`;
      seen.add(r.name);
    }
    return '';
  };
  return { getRows, validate };
}
