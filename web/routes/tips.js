import { api, fmt, agentBadge } from '/web/app.js';

export default async function (root) {
  const [tips, rec] = await Promise.all([
    api('/api/tips'),
    api('/api/recommendations'),
  ]);
  root.innerHTML = `
    <div class="card">
      <div style="display:flex;align-items:center;gap:12px;margin-bottom:6px">
        <h2 style="margin:0">Suggestions</h2>
        <span class="spacer"></span>
        ${rec.markdown ? '<button id="copy-rec" class="ghost" title="Copies these suggestions as a draft for your CLAUDE.md — nothing is written automatically">Copy all as CLAUDE.md</button>' : ''}
      </div>
      ${tips.length === 0
        ? '<p class="muted">No suggestions right now. Watch My Harness surfaces patterns weekly — check back after more activity.</p>'
        : `<p class="muted" style="margin:-8px 0 14px">Rule-based pattern detection over the last 7 days. Dismissed tips re-appear after 14 days.</p>`}
      ${tips.map(t => `
        <div class="tip">
          <div class="tip-head">
            ${agentBadge(t.source)}
            <span class="badge">${fmt.htmlSafe(t.category)}</span>
            <strong>${fmt.htmlSafe(t.title)}</strong>
            <span class="spacer"></span>
            <button class="ghost" data-key="${fmt.htmlSafe(t.key)}">dismiss</button>
          </div>
          <p class="tip-body">${fmt.htmlSafe(t.body)}</p>
        </div>`).join('')}
    </div>
  `;
  root.querySelectorAll('button[data-key]').forEach(b => {
    b.addEventListener('click', async () => {
      await fetch('/api/tips/dismiss', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key: b.dataset.key }),
      });
      location.reload();
    });
  });
  const copyBtn = root.querySelector('#copy-rec');
  if (copyBtn) {
    copyBtn.addEventListener('click', async () => {
      await navigator.clipboard.writeText(rec.markdown);
      copyBtn.textContent = 'Copied!';
      setTimeout(() => { copyBtn.textContent = 'Copy all as CLAUDE.md'; }, 1500);
    });
  }
}
