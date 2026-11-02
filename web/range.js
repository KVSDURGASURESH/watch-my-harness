// range.js — the shared 7d/30d/90d/All time-range tabs, driven by ?range= in the hash.

import { readHashParam, writeHashParam } from '/web/app.js';

export const RANGES = [
  { key: '1h',  label: '1h',  hours: 1 },
  { key: '6h',  label: '6h',  hours: 6 },
  { key: '24h', label: '24h', hours: 24 },
  { key: '7d',  label: '7d',  days: 7 },
  { key: '30d', label: '30d', days: 30 },
  { key: '90d', label: '90d', days: 90 },
  { key: 'all', label: 'All', days: null },
];

const DEFAULT_RANGE = RANGES[1]; // 30d

export function readRange() {
  const key = readHashParam('range');
  const preset = RANGES.find(r => r.key === key);
  if (preset) return preset;
  // Any custom hour window, e.g. ?range=12h or ?range=72h.
  const m = /^(\d{1,5})h$/.exec(key || '');
  if (m && +m[1] > 0) return { key, label: key, hours: +m[1], custom: true };
  return DEFAULT_RANGE;
}

function rangeMs(range) {
  return range.hours ? range.hours * 3600 * 1000 : range.days ? range.days * 86400 * 1000 : null;
}

export function sinceIso(range) {
  const ms = rangeMs(range);
  return ms ? new Date(Date.now() - ms).toISOString() : null;
}

export function withSince(url, since) {
  if (!since) return url;
  return url + (url.includes('?') ? '&' : '?') + 'since=' + encodeURIComponent(since);
}

export function rangeTabs(active) {
  return `
    <div class="range-tabs" role="tablist">
      ${RANGES.map(r => `<button data-range="${r.key}" class="${r.key === active.key ? 'active' : ''}">${r.label}</button>`).join('')}
      <input class="range-custom ${active.custom ? 'active' : ''}" type="number" min="1" max="99999" placeholder="N hrs"
             title="Custom window in hours — type a number and press Enter" value="${active.custom ? active.hours : ''}" aria-label="Custom hours">
    </div>`;
}

// Call after the tabs are in the DOM — switching range re-renders via hashchange.
export function bindRangeTabs(root) {
  root.querySelectorAll('.range-tabs button[data-range]').forEach(btn => {
    btn.addEventListener('click', () => writeHashParam('range', btn.dataset.range));
  });
  const custom = root.querySelector('.range-custom');
  if (custom) custom.addEventListener('keydown', e => {
    const n = parseInt(custom.value, 10);
    if (e.key === 'Enter' && n > 0) writeHashParam('range', n + 'h');
  });
}

// Human label for headings: "LAST 6 HOURS", "LAST 30 DAYS", "ALL TIME".
export function rangeLabel(range) {
  if (range.hours) return `last ${range.hours} hour${range.hours === 1 ? '' : 's'}`;
  if (range.days) return `last ${range.days} days`;
  return 'all time';
}
