// charts.js — ECharts wrappers in the BONE & INK theme.
// Chrome is monochrome; colour is data (agents, token types). Square bars, hairline axes,
// mono type, and a crosshair axisPointer whose axis labels carry the exact value under the cursor.

const PALETTE = ['#E2744D', '#7AA7FF', '#B084FF', '#3DDC97', '#FF5D5D', '#4FD1C5', '#F472B6'];
const INK = '#0A0B0F', PANEL = '#12141A', LINE = '#252932', LINE2 = '#373C49';
const BONE = '#EDE6D3', DIM = '#A6AAB6';
const MONO = "'Oswald', 'Arial Narrow', Arial, sans-serif";   // chart text is Oswald too (name kept for diff size)
const DISPLAY = MONO;

const BASE = {
  textStyle: { color: BONE, fontFamily: MONO },
  color: PALETTE,
  grid: { left: 36, right: 12, top: 34, bottom: 24, containLabel: true },
  animationDuration: 400,
};

const X_AXIS = {
  axisLine:  { lineStyle: { color: LINE2 } },
  axisLabel: { color: DIM, fontFamily: MONO, fontSize: 12.5 },
  axisTick:  { show: false },
};
const Y_AXIS = {
  axisLine:  { show: false },
  axisTick:  { show: false },
  splitLine: { lineStyle: { color: LINE, type: [2, 4] } },
  axisLabel: { color: DIM, fontFamily: MONO, fontSize: 12.5 },
};

// Hover a legend entry → the series highlights, the rest dim (ECharts legendHoverLink).
const LEGEND = {
  textStyle: { color: DIM, fontFamily: DISPLAY, fontSize: 12, letterSpacing: 1 },
  inactiveColor: '#3a3f4b',
  top: 0, right: 0, icon: 'rect',
  itemWidth: 9, itemHeight: 9, itemGap: 14,
  selectedMode: true,
};

const TOOLTIP_BOX = {
  backgroundColor: PANEL, borderColor: BONE, borderWidth: 1, padding: [10, 12],
  textStyle: { color: BONE, fontFamily: MONO, fontSize: 13 },
  extraCssText: 'border-radius:0;box-shadow:0 14px 40px rgba(0,0,0,.55);',
};

// axis tooltip + crosshair; the labels on the axes show the exact value under the pointer
const TOOLTIP = {
  ...TOOLTIP_BOX,
  trigger: 'axis',
  axisPointer: {
    type: 'cross',
    crossStyle: { color: BONE, opacity: .35, type: [3, 3] },
    lineStyle: { color: BONE, opacity: .35 },
    label: { backgroundColor: BONE, color: INK, fontFamily: DISPLAY, fontSize: 12, borderRadius: 0, padding: [3, 6] },
  },
};
const SHADOW_TOOLTIP = { ...TOOLTIP, axisPointer: { ...TOOLTIP.axisPointer, type: 'shadow', shadowStyle: { color: 'rgba(237,230,211,.06)' }, label: { ...TOOLTIP.axisPointer.label, show: true } } };

const nf = v => Number(v).toLocaleString();

function mount(el) {
  const c = echarts.init(el, null, { renderer: 'svg' });
  window.addEventListener('resize', () => c.resize());
  return c;
}

export function lineChart(el, { x, series }) {
  const c = mount(el);
  c.setOption({
    ...BASE, tooltip: TOOLTIP, legend: LEGEND,
    xAxis: { ...X_AXIS, type: 'category', data: x, boundaryGap: false },
    yAxis: { ...Y_AXIS, type: 'value' },
    series: series.map(s => ({ ...s, type: 'line', smooth: false, showSymbol: false, areaStyle: { opacity: 0.1 }, lineStyle: { width: 1.5 } })),
  });
  return c;
}

export function barChart(el, { categories, values, color }) {
  const c = mount(el);
  c.setOption({
    ...BASE, tooltip: SHADOW_TOOLTIP,
    xAxis: { ...X_AXIS, type: 'category', data: categories, axisLabel: { ...X_AXIS.axisLabel, interval: 0, rotate: categories.length > 5 ? 25 : 0 } },
    yAxis: { ...Y_AXIS, type: 'value' },
    series: [{ type: 'bar', data: values, itemStyle: { color: color || PALETTE[0] }, barMaxWidth: 28 }],
  });
  return c;
}

const catAxis = categories => ({
  ...X_AXIS, type: 'category', data: categories,
  axisLabel: { ...X_AXIS.axisLabel, interval: categories.length > 20 ? 'auto' : 0,
    rotate: categories.length > 12 ? 45 : categories.length > 5 ? 25 : 0, width: 110, overflow: 'truncate' },
});

export function stackedBarChart(el, { categories, series, formatter }) {
  const c = mount(el);
  c.setOption({
    ...BASE,
    tooltip: { ...SHADOW_TOOLTIP, valueFormatter: formatter || nf },
    legend: LEGEND,
    xAxis: catAxis(categories),
    yAxis: { ...Y_AXIS, type: 'value' },
    series: series.map((s, i) => ({
      name: s.name, type: 'bar', stack: 'total', data: s.values,
      itemStyle: { color: s.color || PALETTE[i % PALETTE.length] },
      barMaxWidth: 22, emphasis: { focus: 'series' },
    })),
  });
  return c;
}

export function groupedBarChart(el, { categories, series, formatter }) {
  const c = mount(el);
  c.setOption({
    ...BASE,
    tooltip: { ...SHADOW_TOOLTIP, valueFormatter: formatter || nf },
    legend: LEGEND,
    xAxis: catAxis(categories),
    yAxis: { ...Y_AXIS, type: 'value' },
    series: series.map((s, i) => ({
      name: s.name, type: 'bar', data: s.values,
      itemStyle: { color: s.color || PALETTE[i % PALETTE.length] },
      barMaxWidth: 22, emphasis: { focus: 'series' },
    })),
  });
  return c;
}

export function donutChart(el, data) {
  const c = mount(el);
  c.setOption({
    color: PALETTE, textStyle: { fontFamily: MONO },
    tooltip: {
      ...TOOLTIP_BOX, trigger: 'item',
      formatter: p => `<b style="font-family:${DISPLAY};font-size:12px;letter-spacing:.08em">${p.name}</b><br/>${nf(p.value)} tokens · ${p.percent.toFixed(1)}%`,
    },
    legend: { ...LEGEND, top: 'auto', right: 'auto', bottom: 4, left: 'center', type: 'scroll' },
    series: [{
      type: 'pie', center: ['50%', '44%'], radius: ['50%', '68%'], avoidLabelOverlap: true, padAngle: 1,
      itemStyle: { borderColor: PANEL, borderWidth: 2, borderRadius: 0 },
      label: { show: true, position: 'inside', color: INK, fontFamily: DISPLAY, fontSize: 12, fontWeight: 500,
        formatter: ({ percent }) => percent >= 6 ? percent.toFixed(0) + '%' : '' },
      labelLine: { show: false },
      emphasis: { scaleSize: 6 },
      data,
    }],
  });
  return c;
}

// ── Tile (treemap) chart ──────────────────────────────────────────────────────
// groups: [{ name, color, items: [{ name, value, meta }] }]. Tile area = value; tile brightness
// = rank within its group, so the biggest tiles in each agent are the loudest.
// `dim` is a Set of group names to fade (legend hover) and `hide` a Set to drop (legend click).
// Opaque blend of `hex` over the panel colour. Tiles are filled with solid colours (not rgba):
// a translucent fill rendered over a white fallback shape washes out to pale on hover.
const rgba = (hex, a) => {
  const f = parseInt(hex.slice(1), 16), b = parseInt(PANEL.slice(1), 16);
  const ch = sh => Math.round((f >> sh & 255) * a + (b >> sh & 255) * (1 - a));
  return `rgb(${ch(16)},${ch(8)},${ch(0)})`;
};

export function treemapChart(el, { groups, tooltip, unit = '' }) {
  const c = mount(el);
  let hover = null;   // {agent, name} of the tile under the pointer
  const build = (dim = new Set(), hide = new Set()) => groups.filter(g => !hide.has(g.name)).map(g => {
    const max = Math.max(...g.items.map(i => i.value), 1);
    const faded = dim.size && !dim.has(g.name);
    return {
      name: g.name.toUpperCase(),
      itemStyle: { color: PANEL, borderColor: INK, borderWidth: 0, gapWidth: 3 },
      // hovering a tile must not light up its whole agent block
      emphasis: { disabled: true },
      children: g.items.map(i => ({
        name: i.name, value: i.value, meta: { ...i.meta, agent: g.name },
        emphasis: { disabled: true },   // hover is drawn by us below: ECharts' own highlight washes the tile out pale
        itemStyle: hover && hover.agent === g.name && hover.name === i.name
          ? { color: rgba(g.color, 0.72), borderColor: BONE, borderWidth: 1.5 }
          : {
            color: rgba(g.color, faded ? 0.07 : 0.20 + 0.30 * Math.pow(i.value / max, 0.6)),
            borderColor: faded ? 'transparent' : rgba(g.color, 0.55), borderWidth: 1,
          },
      })),
    };
  });
  const base = {
    tooltip: { ...TOOLTIP_BOX, trigger: 'item', formatter: p => p.data && p.data.meta ? tooltip(p.data) : '' },
    series: [{
      type: 'treemap', left: 0, right: 0, top: 0, bottom: 0,
      roam: false, nodeClick: false, breadcrumb: { show: false },
      squareRatio: 0.9, animationDurationUpdate: 250,
      upperLabel: { show: false, height: 22, formatter: p => `${p.name}  ${nf(p.value)}${unit}`, color: BONE, fontFamily: DISPLAY, fontSize: 12, letterSpacing: 1.5, fontWeight: 500 },
      label: {
        show: true, color: '#EDE6D3', fontFamily: MONO, fontSize: 13, position: 'insideTopLeft', padding: [6, 8],
        formatter: p => `{n|${p.name.replace(/^.*:/, '')}}\n{v|${nf(p.value)}${unit}}`,
        rich: { n: { fontFamily: DISPLAY, fontSize: 12.5, fontWeight: 500, lineHeight: 16, color: '#EDE6D3' }, v: { fontSize: 12.5, color: DIM } },
        overflow: 'truncate', ellipsis: '…',
      },
      emphasis: { disabled: true },   // hover styling is applied by our own mouseover handler
      // level 0 is the invisible root; level 1 = agent groups (labelled); level 2 = skill tiles
      levels: [
        { itemStyle: { borderWidth: 0, gapWidth: 0 }, upperLabel: { show: false } },
        { itemStyle: { borderColor: INK, borderWidth: 0, gapWidth: 8 }, upperLabel: { show: true } },
        { itemStyle: { gapWidth: 3, borderWidth: 1 }, upperLabel: { show: false } },
      ],
      data: build(),
    }],
  };
  c.setOption(base);
  let lastDim, lastHide;
  const refresh = (dim, hide) => { lastDim = dim; lastHide = hide; c.setOption({ series: [{ data: build(dim, hide) }] }); };
  c.on('mouseover', p => { if (p.data && p.data.meta) { hover = { agent: p.data.meta.agent, name: p.data.name }; refresh(lastDim, lastHide); } });
  c.on('globalout', () => { if (hover) { hover = null; refresh(lastDim, lastHide); } });
  return { chart: c, refresh };
}

// ── Ranked horizontal bars (stacked by agent) with crosshair + detail tooltip ─────
export function hbarChart(el, { categories, series, tooltipExtra, formatter }) {
  const c = mount(el);
  c.setOption({
    ...BASE, grid: { left: 8, right: 36, top: 34, bottom: 8, containLabel: true },
    tooltip: {
      ...SHADOW_TOOLTIP,
      axisPointer: { ...SHADOW_TOOLTIP.axisPointer, type: 'shadow' },
      formatter: ps => {
        const total = ps.reduce((s, p) => s + (p.value || 0), 0);
        const rows = ps.filter(p => p.value).map(p =>
          `<div style="display:flex;justify-content:space-between;gap:18px"><span><i style="display:inline-block;width:7px;height:7px;background:${p.color};margin-right:6px"></i>${p.seriesName}</span><b>${nf(p.value)}</b></div>`).join('');
        return `<div style="font-family:${DISPLAY};font-size:12px;letter-spacing:.08em;margin-bottom:6px">${ps[0].name}</div>${rows}` +
          `<div style="border-top:1px solid ${LINE2};margin-top:6px;padding-top:6px;display:flex;justify-content:space-between"><span style="color:${DIM}">TOTAL</span><b>${nf(total)}</b></div>` +
          (tooltipExtra ? `<div style="color:${DIM};margin-top:4px">${tooltipExtra(ps[0].dataIndex)}</div>` : '');
      },
    },
    legend: LEGEND,
    xAxis: { ...Y_AXIS, type: 'value', position: 'top', axisLabel: { ...Y_AXIS.axisLabel } },
    yAxis: { ...X_AXIS, type: 'category', inverse: true, data: categories,
      axisLabel: { ...X_AXIS.axisLabel, color: BONE, width: 150, overflow: 'truncate' } },
    series: series.map(s => ({
      name: s.name, type: 'bar', stack: 'total', data: s.values,
      itemStyle: { color: s.color }, barMaxWidth: 16, emphasis: { focus: 'series' },
    })),
  });
  return c;
}
