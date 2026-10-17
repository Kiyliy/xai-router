import { h, fmtInt, fmtCompact, fmtUSD, fmtMs, fmtPct, fmtDateTime } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get } from '../api.js';
import { segmented, card, toastError, tabs, tableWrap, skeletonRows, tableEmptyRow, button } from '../ui.js';
import { timeChart, shareBar, colorFor, isSparse, sparkline } from '../charts.js';

const RANGES = ['1h', '24h', '7d', '30d', '90d'];
const pad = (n) => String(n).padStart(2, '0');

export function render(el, ctx) {
  let range = RANGES.includes(ctx.params.range) ? ctx.params.range : (localStorage.getItem('xr.range') || '24h');
  if (!RANGES.includes(range)) range = '24h';
  let metric = 'requests';
  let breakdownBy = 'model';
  let modelSeries = null;
  let mainSeries = null;
  const charts = [];

  const rangeSel = segmented({
    options: RANGES.map((v) => ({ value: v, label: t(`range.${v}`) })), value: range,
    onChange: (v) => { range = v; try { localStorage.setItem('xr.range', v); } catch { /* ignore */ } ctx.setParams({ range: v }); load(); },
  });
  const refresh = button('', { icon: 'refresh', variant: 'ghost', size: 'icon', attrs: { title: t('common.refresh'), 'aria-label': t('common.refresh') }, onClick: () => load() });
  ctx.setActions(rangeSel, refresh);

  const kpis = h('div', { class: 'kpi-grid' }, Array.from({ length: 6 }, () => h('div', { class: 'kpi skeleton-card' }, h('span', { class: 'skeleton', style: { width: '40%' } }), h('span', { class: 'skeleton skeleton-lg', style: { width: '60%' } }), h('span', { class: 'skeleton', style: { width: '70%' } }))));
  const reqChartBody = h('div', { class: 'chart-slot' }, h('div', { class: 'skeleton skeleton-chart' }));
  const costChartBody = h('div', { class: 'chart-slot' }, h('div', { class: 'skeleton skeleton-chart' }));
  const modelChartBody = h('div', { class: 'chart-slot' }, h('div', { class: 'skeleton skeleton-chart' }));
  const breakdownBody = h('div');

  const metricSel = segmented({
    size: 'seg-sm',
    options: [{ value: 'requests', label: t('ov.requests') }, { value: 'tokens', label: t('ov.tokens') }, { value: 'cost', label: t('ov.cost') }],
    value: metric, onChange: (v) => { metric = v; drawModelChart(); },
  });

  el.append(
    kpis,
    h('div', { class: 'grid-2' },
      card({ title: t('ov.requestsOverTime'), subtitle: t('ov.requestsOverTimeSub'), body: reqChartBody }),
      card({ title: t('ov.costOverTime'), subtitle: t('ov.costOverTimeSub'), body: costChartBody })),
    card({ title: t('ov.byModel'), subtitle: t('ov.byModelSub'), actions: metricSel, body: modelChartBody }),
    card({ title: t('ov.breakdown'), body: breakdownBody, flush: true }),
  );

  const interval = () => (range === '30d' || range === '90d' ? 'day' : range === '1h' ? '5m' : 'hour');
  const xFormat = (iso) => {
    const d = new Date(iso);
    if (interval() === 'day') return `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
    if (range === '7d') return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}h`;
    return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  };
  const tipTitle = (iso) => {
    const d = new Date(iso);
    if (interval() === 'day') return fmtDateTime(iso).slice(0, 10);
    return interval() === '5m' ? fmtDateTime(iso).slice(0, 16) : `${fmtDateTime(iso).slice(0, 13)}:00`;
  };
  const destroyCharts = () => { charts.splice(0).forEach((c) => c.destroy && c.destroy()); };

  function kpi({ ic, label, value, sub, sub2, tone, tip, spark }) {
    return h('div', { class: `kpi ${tone ? 'kpi-' + tone : ''}` },
      h('div', { class: 'kpi-head' }, h('span', { class: 'kpi-icon' }, icon(ic, 15)), h('span', { class: 'kpi-label' }, label)),
      h('div', { class: 'kpi-value' }, value),
      h('div', { class: 'kpi-sub', title: tip || null }, sub),
      sub2 ? h('div', { class: 'kpi-sub' }, sub2) : null,
      spark ? h('div', { class: 'kpi-spark', 'data-spark': spark, 'data-tone': tone || '' }) : null);
  }

  // Trend lines under the KPIs, drawn from the same series as the requests chart.
  function drawSparks() {
    const ts = mainSeries;
    if (!ts) return;
    const buckets = ts.buckets || [];
    const pts = (ts.series && ts.series[0] && ts.series[0].points) || [];
    const req = alignedValues(buckets, pts, (p) => p.requests);
    const ok = alignedValues(buckets, pts, (p) => p.success ?? (p.requests - (p.errors || 0)));
    const lat = alignedValues(buckets, pts, (p) => p.avg_latency_ms);
    let last = 1;
    const values = {
      requests: req,
      success: req.map((r, i) => (r ? (last = ok[i] / r) : last)),
      tokens: alignedValues(buckets, pts, (p) => p.total_tokens),
      cost: alignedValues(buckets, pts, (p) => p.cost_usd),
      latency: lat,
    };
    const toneColor = { success: 'var(--success)', warning: 'var(--warning)', danger: 'var(--danger)' };
    kpis.querySelectorAll('[data-spark]').forEach((slot) => {
      slot.replaceChildren(sparkline(values[slot.dataset.spark] || [], toneColor[slot.dataset.tone] || 'var(--accent)'));
    });
  }

  function renderKpis(o) {
    const k = o.keys || {}, tk = o.tokens || {};
    const sr = o.success_rate;
    const tokTip = `${t('tok.prompt')}: ${fmtInt(o.prompt_tokens)}
${t('tok.completion')}: ${fmtInt(o.completion_tokens)}
${t('tok.cached')}: ${fmtInt(o.cached_tokens)}
${t('tok.reasoning')}: ${fmtInt(o.reasoning_tokens)}`;
    const dotItem = (variant, n, label) => h('span', { class: 'kpi-dot-item', title: label }, h('span', { class: `dot dot-${variant}` }), h('span', null, fmtInt(n || 0)), h('span', { class: 'kpi-dot-label' }, label));
    kpis.replaceChildren(
      kpi({ ic: 'activity', label: t('ov.requests'), spark: 'requests', value: fmtInt(o.requests), sub: [h('span', { class: 'text-success' }, `${fmtInt(o.success)} ${t('ov.ok')}`), ' · ', h('span', { class: o.errors ? 'text-danger' : '' }, `${fmtInt(o.errors)} ${t('ov.errors')}`)] }),
      kpi({ ic: 'checkCircle', label: t('ov.successRate'), spark: 'success', value: o.requests ? fmtPct(sr) : '—', tone: !o.requests ? '' : sr >= 0.98 ? 'success' : sr >= 0.9 ? 'warning' : 'danger', sub: t('ov.inRange', { r: t(`range.${range}`) }) }),
      kpi({ ic: 'hash', label: t('ov.tokens'), spark: 'tokens', value: fmtCompact(o.total_tokens), tip: tokTip, sub: `${t('ov.in')} ${fmtCompact(o.prompt_tokens)} · ${t('ov.out')} ${fmtCompact(o.completion_tokens)}`, sub2: o.cached_tokens ? `${t('ov.cachedShort')} ${fmtCompact(o.cached_tokens)}` : null }),
      kpi({ ic: 'dollar', label: t('ov.cost'), spark: 'cost', value: fmtUSD(o.cost_usd), sub: o.requests ? t('ov.perRequest', { v: fmtUSD(o.cost_usd / o.requests) }) : '—' }),
      kpi({ ic: 'timer', label: t('ov.avgLatency'), spark: 'latency', value: o.requests ? fmtMs(o.avg_latency_ms) : '—', sub: t('ov.endToEnd') }),
      kpi({
        ic: 'server', label: t('ov.resources'), tone: k.active === 0 && k.total ? 'danger' : '',
        value: [`${fmtInt(k.active)}`, h('span', { class: 'kpi-of' }, ` / ${fmtInt(k.total)} ${t('ov.keysUnit')}`)],
        sub: h('span', { class: 'kpi-dots', title: `${t('status.active')}: ${fmtInt(k.active)}
${t('status.cooldown')}: ${fmtInt(k.cooldown)}
${t('status.disabled')}: ${fmtInt(k.disabled)}` },
          dotItem('success', k.active, t('status.active')), dotItem('warning', k.cooldown, t('status.cooldown')), dotItem('danger', k.disabled, t('status.disabled'))),
        sub2: h('a', { href: '#/tokens', class: 'kpi-link' }, t('ov.tokensLine', { n: fmtInt(tk.enabled), total: fmtInt(tk.total) })),
      }),
    );
    drawSparks();
  }

  function alignedValues(buckets, points, pick) {
    const m = new Map((points || []).map((p) => [new Date(p.t).getTime(), p]));
    return buckets.map((b, i) => {
      const p = m.get(new Date(b).getTime()) || (points && points.length === buckets.length ? points[i] : null);
      return p ? pick(p) || 0 : 0;
    });
  }

  // "(unknown)" = requests without a model (e.g. 401s on /v1/models); "#5 (deleted)" = removed key/token.
  function modelLabel(name) { return !name || name === '(unknown)' ? t('ov.noModel') : name === 'other' ? t('ov.other') : name; }
  function nameEl(by, r) {
    const name = r.name || r.id || '';
    if (by === 'model' && (!r.id || name === '(unknown)')) return h('span', { class: 'muted' }, t('ov.noModel'));
    if (name === 'other') return h('span', { class: 'muted' }, t('ov.other'));
    const del = / \(deleted\)$/.exec(name);
    if (del) return h('span', { class: 'muted' }, `${name.slice(0, del.index)} ${t('ov.deleted')}`);
    return h('span', { class: by === 'model' ? 'mono nowrap' : 'nowrap' }, name);
  }

  function emptyChart(slot) { slot.replaceChildren(h('div', { class: 'chart-empty' }, icon('activity', 18), h('span', null, t('ov.noTraffic')))); }

  function drawMain(ts) {
    mainSeries = ts;
    drawSparks();
    const buckets = ts.buckets || [];
    const pts = (ts.series && ts.series[0] && ts.series[0].points) || [];
    const total = pts.reduce((a, p) => a + (p.requests || 0), 0);
    if (!buckets.length || !total) { emptyChart(reqChartBody); emptyChart(costChartBody); return; }
    const req = timeChart({
      buckets, kind: 'bar', stacked: true, height: 230, xFormat, tipTitle, yFormat: fmtCompact, tipFormat: fmtInt, legend: true,
      series: [
        { name: t('ov.success'), values: alignedValues(buckets, pts, (p) => p.success ?? (p.requests - (p.errors || 0))), color: 'var(--chart-ok)' },
        { name: t('ov.errorsCap'), values: alignedValues(buckets, pts, (p) => p.errors), color: 'var(--chart-err)' },
      ],
    });
    const costVals = alignedValues(buckets, pts, (p) => p.cost_usd);
    const cost = timeChart({
      buckets, kind: isSparse([costVals]) ? 'bar' : 'area', stacked: false, height: 230, xFormat, tipTitle, yFormat: (v) => fmtUSD(v), legend: false,
      series: [{ name: t('ov.cost'), values: costVals, color: 'var(--c1)' }],
    });
    charts.push(req, cost);
    reqChartBody.replaceChildren(req);
    costChartBody.replaceChildren(cost);
  }

  function drawModelChart() {
    if (!modelSeries) return;
    const { buckets = [], series = [] } = modelSeries;
    const pick = metric === 'requests' ? (p) => p.requests : metric === 'tokens' ? (p) => p.total_tokens : (p) => p.cost_usd;
    const yFormat = metric === 'cost' ? (v) => fmtUSD(v) : fmtCompact;
    const any = series.some((se) => (se.points || []).some((p) => pick(p) > 0));
    if (!buckets.length || !any) { emptyChart(modelChartBody); return; }
    const old = modelChartBody.firstChild; if (old && old.destroy) old.destroy();
    const ser = series.map((se) => ({ name: modelLabel(se.name), values: alignedValues(buckets, se.points, pick), color: colorFor(se.name) }));
    // Sparse hourly data reads as isolated spikes on a line chart; stacked bars are clearer.
    const sparse = isSparse(ser.map((x) => x.values));
    const c = timeChart({
      buckets, kind: sparse ? 'bar' : 'line', stacked: sparse, height: 260, xFormat, tipTitle, yFormat, tipFormat: metric === 'requests' ? fmtInt : yFormat, legend: true,
      series: ser,
    });
    charts.push(c);
    modelChartBody.replaceChildren(c);
  }

  async function loadBreakdown() {
    const by = breakdownBy;
    const cols = 8;
    const tbody = h('tbody', null, skeletonRows(cols, 5));
    const table = h('table', { class: 'table table-breakdown' },
      h('thead', null, h('tr', null,
        h('th', null, t(`ov.by.${by}`)), h('th', { class: 'num' }, t('ov.requests')), h('th', { class: 'col-share' }, t('ov.share')),
        h('th', { class: 'num' }, t('ov.successRate')), h('th', { class: 'num col-tok' }, t('ov.tokens')), h('th', { class: 'num' }, t('ov.cost')),
        h('th', { class: 'col-share' }, t('ov.costShare')), h('th', { class: 'num col-lat' }, t('ov.avgLatency')))),
      tbody);
    breakdownTable.replaceChildren(tableWrap(table, { maxHeight: '520px' }));
    try {
      const res = await get('/api/v1/stats/breakdown', { range, by }, { signal: ctx.signal });
      if (by !== breakdownBy) return;
      const rows = res.data || [];
      if (!rows.length) { tbody.replaceChildren(tableEmptyRow(cols, { icon: 'activity', title: t('ov.noTraffic') })); return; }
      const totalReq = rows.reduce((a, r) => a + (r.requests || 0), 0) || 1;
      const totalCost = rows.reduce((a, r) => a + (r.cost_usd || 0), 0) || 1;
      tbody.replaceChildren(...rows.map((r) => {
        const target = by === 'model' ? `#/logs?model=${encodeURIComponent(r.id)}` : by === 'key' ? `#/logs?key_id=${encodeURIComponent(r.id)}` : `#/logs?token_id=${encodeURIComponent(r.id)}`;
        const canLink = r.id && r.id !== 'other' && r.id !== '0' && !(by === 'model' && !r.id);
        return h('tr', { class: canLink ? 'clickable' : '', onclick: canLink ? () => ctx.navigate(target) : null, title: canLink ? t('ov.viewLogs') : null },
          h('td', null, h('div', { class: 'cell-name' }, h('span', { class: 'legend-swatch', style: { background: colorFor(by === 'model' ? r.id : (r.id || r.name), by) } }), nameEl(by, r))),
          h('td', { class: 'num' }, fmtInt(r.requests)),
          h('td', { class: 'col-share' }, shareBar(r.requests / totalReq)),
          h('td', { class: 'num' }, h('span', { class: r.success_rate < 0.9 ? 'text-danger' : r.success_rate < 0.98 ? 'text-warning' : '' }, fmtPct(r.success_rate))),
          h('td', { class: 'num col-tok', title: `${t('tok.prompt')}: ${fmtInt(r.prompt_tokens)}\n${t('tok.completion')}: ${fmtInt(r.completion_tokens)}\n${t('tok.cached')}: ${fmtInt(r.cached_tokens)}\n${t('tok.reasoning')}: ${fmtInt(r.reasoning_tokens)}` }, fmtCompact(r.total_tokens)),
          h('td', { class: 'num' }, fmtUSD(r.cost_usd)),
          h('td', { class: 'col-share' }, shareBar((r.cost_usd || 0) / totalCost, 'var(--c2)')),
          h('td', { class: 'num col-lat' }, fmtMs(r.avg_latency_ms)));
      }));
    } catch (e) { toastError(e); tbody.replaceChildren(tableEmptyRow(cols, { icon: 'alert', title: t('common.loadFailed'), desc: e.message })); }
  }

  const breakdownTable = h('div');
  breakdownBody.append(tabs(['model', 'key', 'token'].map((by) => ({ id: by, label: t(`ov.by.${by}`), render: () => breakdownTable })), {
    active: breakdownBy, cls: 'tabs-card', onChange: (v) => { breakdownBy = v; loadBreakdown(); },
  }));

  async function load() {
    const q = { range };
    destroyCharts();
    const opts = { signal: ctx.signal };
    const pOverview = get('/api/v1/stats/overview', q, opts).then(renderKpis);
    const pMain = get('/api/v1/stats/timeseries', { range, interval: interval(), group: 'none' }, opts).then(drawMain);
    const pModel = get('/api/v1/stats/timeseries', { range, interval: interval(), group: 'model', limit: 6 }, opts).then((r) => { modelSeries = r; drawModelChart(); });
    loadBreakdown();
    const results = await Promise.allSettled([pOverview, pMain, pModel]);
    const failed = results.find((r) => r.status === 'rejected' && r.reason && r.reason.name !== 'AbortError');
    if (failed) toastError(failed.reason);
  }

  load();
  const timer = setInterval(() => { if (!document.hidden) load(); }, 60000);
  return () => { clearInterval(timer); destroyCharts(); };
}
