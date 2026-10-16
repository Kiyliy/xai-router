// Hand-written responsive SVG time charts (stacked bars / lines / areas) with hover tooltips.
import { h, s } from './util.js';

export const PALETTE = ['var(--c1)', 'var(--c2)', 'var(--c3)', 'var(--c4)', 'var(--c5)', 'var(--c6)', 'var(--c7)', 'var(--c8)'];

// Stable series colours shared by every chart and table: a name hashes to a preferred palette
// slot; the first name to claim a slot keeps it for the session, later names probe for a free
// slot. So the same model is the same colour in the "By model" chart and the breakdown table.
const colorMap = new Map();
function hashStr(str) {
  let x = 2166136261;
  for (let i = 0; i < str.length; i++) { x ^= str.charCodeAt(i); x = Math.imul(x, 16777619); }
  return x >>> 0;
}
export function colorFor(name, ns = 'model') {
  if (name === 'other') return 'var(--c-other)';
  if (name == null || name === '' || name === '(unknown)') return 'var(--c-other)';
  const key = `${ns}:${name}`;
  if (colorMap.has(key)) return colorMap.get(key);
  const used = new Set([...colorMap.entries()].filter(([k]) => k.startsWith(`${ns}:`)).map(([, v]) => v));
  const start = hashStr(String(name)) % PALETTE.length;
  let c = PALETTE[start];
  for (let i = 0; i < PALETTE.length; i++) {
    const cand = PALETTE[(start + i) % PALETTE.length];
    if (!used.has(cand)) { c = cand; break; }
  }
  colorMap.set(key, c);
  return c;
}

/** True when most buckets are empty — bars read better than a spiky line there. */
export function isSparse(seriesValues) {
  const n = seriesValues[0] ? seriesValues[0].length : 0;
  if (!n) return false;
  let nz = 0;
  for (let i = 0; i < n; i++) if (seriesValues.some((v) => (v[i] || 0) > 0)) nz++;
  return nz / n < 0.5;
}

function niceStep(max, ticks) {
  const raw = max / ticks;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const n = raw / mag;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10;
  return step * mag;
}

/**
 * timeChart({ buckets: ISO[], series: [{ name, values: number[], color }], kind: 'bar'|'line'|'area',
 *             stacked, height, yFormat, xFormat, tipTitle, legend, tipFormat })
 * Returns a container element that re-renders on resize.
 */
export function timeChart(opts) {
  const { buckets = [], kind = 'bar', stacked = kind === 'bar', height = 220, yFormat = String, xFormat, tipFormat, legend = false, tipTitle } = opts;
  const series = opts.series.map((se, i) => ({ ...se, color: se.color || PALETTE[i % PALETTE.length] }));
  const hidden = new Set();
  const svgHost = h('div', { class: 'chart-svg' });
  const tip = h('div', { class: 'chart-tip', hidden: true });
  const root = h('div', { class: 'chart', style: { minHeight: `${height}px` } }, svgHost, tip);
  let legendEl = null;
  if (legend) {
    legendEl = h('div', { class: 'chart-legend' });
    root.appendChild(legendEl);
  }

  const renderLegend = () => {
    if (!legendEl) return;
    legendEl.replaceChildren(...series.map((se) => h('button', {
      class: `legend-item ${hidden.has(se.name) ? 'off' : ''}`, type: 'button',
      onclick: () => { hidden.has(se.name) ? hidden.delete(se.name) : hidden.add(se.name); if (hidden.size === series.length) hidden.delete(se.name); renderLegend(); draw(); },
    }, h('span', { class: 'legend-swatch', style: { background: se.color } }), se.name)));
  };

  let lastW = 0;
  const draw = () => {
    const W = Math.max(280, Math.floor(svgHost.clientWidth || root.clientWidth || 600));
    lastW = W;
    const H = height;
    const pad = { l: 52, r: 12, t: 12, b: 26 };
    const pw = W - pad.l - pad.r, ph = H - pad.t - pad.b;
    const n = buckets.length;
    const vis = series.filter((se) => !hidden.has(se.name));
    const svg = s('svg', { width: W, height: H, viewBox: `0 0 ${W} ${H}`, class: 'chart-el', role: 'img' });

    if (!n) { svgHost.replaceChildren(svg); return; }

    let max = 0;
    for (let i = 0; i < n; i++) {
      if (stacked) max = Math.max(max, vis.reduce((a, se) => a + (se.values[i] || 0), 0));
      else for (const se of vis) max = Math.max(max, se.values[i] || 0);
    }
    if (max <= 0) max = 1;
    const step = niceStep(max, 4);
    const top = Math.ceil(max / step) * step;
    const y = (v) => pad.t + ph - (v / top) * ph;
    const band = pw / n;
    const cx = (i) => pad.l + band * i + band / 2;

    // grid + y labels
    const grid = s('g', { class: 'chart-grid' });
    for (let v = 0; v <= top + step / 2; v += step) {
      const yy = Math.round(y(v)) + 0.5;
      grid.appendChild(s('line', { x1: pad.l, x2: W - pad.r, y1: yy, y2: yy }));
      grid.appendChild(s('text', { x: pad.l - 8, y: yy + 4, 'text-anchor': 'end', class: 'chart-axis' }, yFormat(v)));
    }
    svg.appendChild(grid);

    // x labels
    const maxLabels = Math.max(2, Math.floor(pw / 74));
    const every = Math.max(1, Math.ceil(n / maxLabels));
    const xl = s('g');
    for (let i = 0; i < n; i += every) {
      xl.appendChild(s('text', { x: cx(i), y: H - 8, 'text-anchor': 'middle', class: 'chart-axis' }, xFormat ? xFormat(buckets[i], i) : buckets[i]));
    }
    svg.appendChild(xl);

    const marks = s('g');
    if (kind === 'bar') {
      const bw = Math.max(1, Math.min(28, band * 0.68));
      for (let i = 0; i < n; i++) {
        let acc = 0;
        vis.forEach((se, si) => {
          const v = se.values[i] || 0;
          if (v <= 0) return;
          const y0 = stacked ? y(acc) : y(0);
          const y1 = stacked ? y(acc + v) : y(v);
          acc += v;
          const isTop = si === vis.length - 1 || vis.slice(si + 1).every((o) => !(o.values[i] > 0));
          const hgt = Math.max(1, y0 - y1);
          const r = isTop ? Math.min(3, bw / 3, hgt) : 0;
          const x0 = cx(i) - bw / 2;
          // rounded top only
          const d = r
            ? `M${x0},${y0}V${y1 + r}Q${x0},${y1} ${x0 + r},${y1}H${x0 + bw - r}Q${x0 + bw},${y1} ${x0 + bw},${y1 + r}V${y0}Z`
            : `M${x0},${y0}V${y1}H${x0 + bw}V${y0}Z`;
          marks.appendChild(s('path', { d, class: 'chart-bar', style: { fill: se.color } }));
        });
      }
    } else {
      const defs = s('defs');
      svg.appendChild(defs);
      const base = new Array(n).fill(0);
      vis.forEach((se, si) => {
        const pts = se.values.map((v, i) => {
          const val = stacked ? base[i] + (v || 0) : (v || 0);
          return [cx(i), y(val), stacked ? y(base[i]) : y(0)];
        });
        if (stacked) se.values.forEach((v, i) => { base[i] += v || 0; });
        const line = pts.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join('');
        if (kind === 'area') {
          const gid = `g${Math.random().toString(36).slice(2, 8)}`;
          const grad = s('linearGradient', { id: gid, x1: 0, y1: 0, x2: 0, y2: 1 },
            s('stop', { offset: '0%', style: { stopColor: se.color, stopOpacity: 0.28 } }),
            s('stop', { offset: '100%', style: { stopColor: se.color, stopOpacity: 0.02 } }));
          defs.appendChild(grad);
          const back = pts.slice().reverse().map((p) => `L${p[0].toFixed(1)},${p[2].toFixed(1)}`).join('');
          marks.appendChild(s('path', { d: `${line}${back}Z`, fill: `url(#${gid})`, stroke: 'none' }));
        }
        marks.appendChild(s('path', { d: line, class: 'chart-line', style: { stroke: se.color } }));
        if (n <= 2) pts.forEach((p) => marks.appendChild(s('circle', { cx: p[0], cy: p[1], r: 3, style: { fill: se.color } })));
      });
    }
    svg.appendChild(marks);

    // hover layer
    const guide = s('line', { class: 'chart-guide', y1: pad.t, y2: pad.t + ph, x1: 0, x2: 0, visibility: 'hidden' });
    const hl = s('rect', { class: 'chart-hl', y: pad.t, height: ph, width: band, x: 0, visibility: 'hidden' });
    const dots = s('g', { visibility: 'hidden' });
    svg.insertBefore(hl, marks);
    svg.appendChild(guide);
    svg.appendChild(dots);
    const hit = s('rect', { x: pad.l, y: pad.t, width: pw, height: ph, fill: 'transparent' });
    svg.appendChild(hit);

    const show = (evt) => {
      const rect = svg.getBoundingClientRect();
      const mx = evt.clientX - rect.left;
      const i = Math.max(0, Math.min(n - 1, Math.floor((mx - pad.l) / band)));
      const x = cx(i);
      if (kind === 'bar') { hl.setAttribute('x', pad.l + band * i); hl.setAttribute('visibility', 'visible'); }
      else {
        guide.setAttribute('x1', x); guide.setAttribute('x2', x); guide.setAttribute('visibility', 'visible');
        dots.replaceChildren(); dots.setAttribute('visibility', 'visible');
        let acc = 0;
        for (const se of vis) {
          const v = se.values[i] || 0; acc += v;
          dots.appendChild(s('circle', { cx: x, cy: y(stacked ? acc : v), r: 3.5, class: 'chart-dot', style: { fill: se.color } }));
        }
      }
      const rows = vis.map((se) => ({ name: se.name, color: se.color, v: se.values[i] || 0 }));
      const ordered = kind === 'bar' ? rows.slice().reverse() : rows.slice().sort((a, b) => b.v - a.v);
      tip.replaceChildren(
        h('div', { class: 'chart-tip-title' }, tipTitle ? tipTitle(buckets[i], i) : buckets[i]),
        ...ordered.map((r) => h('div', { class: 'chart-tip-row' },
          h('span', { class: 'legend-swatch', style: { background: r.color } }),
          h('span', { class: 'chart-tip-name' }, r.name),
          h('span', { class: 'chart-tip-val' }, (tipFormat || yFormat)(r.v)))));
      tip.hidden = false;
      const tw = tip.offsetWidth, th = tip.offsetHeight;
      let left = x + 14;
      if (left + tw > W - 4) left = x - tw - 14;
      if (left < 4) left = 4;
      let topPx = Math.max(4, evt.clientY - rect.top - th / 2);
      if (topPx + th > H) topPx = H - th;
      tip.style.transform = `translate(${left}px, ${topPx}px)`;
    };
    const hide = () => { tip.hidden = true; guide.setAttribute('visibility', 'hidden'); hl.setAttribute('visibility', 'hidden'); dots.setAttribute('visibility', 'hidden'); };
    hit.addEventListener('mousemove', show);
    hit.addEventListener('mouseleave', hide);
    hit.addEventListener('touchstart', (e) => { if (e.touches[0]) show(e.touches[0]); }, { passive: true });
    hit.addEventListener('touchmove', (e) => { if (e.touches[0]) show(e.touches[0]); }, { passive: true });
    svgHost.replaceChildren(svg);
  };

  renderLegend();
  const ro = new ResizeObserver(() => {
    const w = Math.floor(svgHost.clientWidth);
    if (w && Math.abs(w - lastW) > 2) draw();
  });
  ro.observe(svgHost);
  requestAnimationFrame(draw);
  root.destroy = () => ro.disconnect();
  return root;
}

/** Tiny horizontal share bar. */
export function shareBar(ratio, color = 'var(--accent)') {
  const r = Math.max(0, Math.min(1, ratio || 0));
  return h('div', { class: 'share' },
    h('div', { class: 'share-track' }, h('div', { class: 'share-fill', style: { width: `${(r * 100).toFixed(1)}%`, background: color } })),
    h('span', { class: 'share-pct' }, `${(r * 100).toFixed(r < 0.1 && r > 0 ? 1 : 0)}%`));
}
