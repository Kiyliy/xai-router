// Overview (v0.5 "Ion", Figma 48-1233 / 127-9281): a hairline metrics strip, one traffic chart with
// the previous window for comparison, cost by model, events that need attention and the upstream
// key strip. All numbers on the page come from the same range and add up (acceptance V5).
import { h, fmtInt, fmtCompact, fmtUSD, fmtMs, fmtPct, fmtDateTime, fmtDate } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get } from '../api.js';
import { segmented, toastError } from '../ui.js';
import { openPasswordDialog } from './access.js';
import { areaChart } from '../charts.js';
import { eventSentence } from '../events.js';
import { listTime } from '../detail-drawer.js';

const RANGES = ['1h', '24h', '7d', '30d', '90d'];
const RANGE_MS = { '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3, '30d': 30 * 86400e3, '90d': 90 * 86400e3 };
// The previous window is read from the next longer range (there is nothing longer than 90 days).
const PREV_RANGE = { '1h': '24h', '24h': '7d', '7d': '30d', '30d': '90d' };
const ONBOARD_KEY = 'xr.onboard.dismissed';
const ATTN_HASH = '#/logs?type=system&problems=1';
const lsGet = (k) => { try { return localStorage.getItem(k); } catch { return null; } };
const lsSet = (k, v) => { try { localStorage.setItem(k, v); } catch { /* ignore */ } };
const pad = (n) => String(n).padStart(2, '0');
const go = (ctx, hash) => (e) => { e.preventDefault(); ctx.navigate(hash); };

/** Upstream key state as the product names it: 可用 / 冷却中 / 自动禁用 / 已禁用. */
export function keyState(k) {
  if (!k.enabled) return 'disabled';
  if (k.auto_disabled) return 'auto';
  if (k.status === 'cooldown') return 'cooldown';
  return 'active';
}

/** "1.86 亿" → ["1.86", "亿"]; "1.2 秒" → ["1.2", "秒"]; "$412.37" → ["$412.37", ""]. */
function splitUnit(str) {
  const m = /^(.*?\d)\s+(\D+)$/.exec(String(str));
  return m ? [m[1], m[2]] : [String(str), ''];
}

/**
 * First-use checklist (pm-spec P1-1, Figma 121-8074): four steps, the next one highlighted.
 * Gone once all four are done or the user dismisses it.
 */
function onboarding(ctx) {
  const slot = h('div', { class: 'onboard-slot' });
  let seq = 0;
  async function refresh() {
    if (lsGet(ONBOARD_KEY) === '1') { slot.replaceChildren(); return; }
    const my = ++seq;
    const opts = { signal: ctx.signal };
    try {
      const [info, ov, keyList] = await Promise.all([
        ctx.refreshInfo(),
        get('/api/v1/stats/overview', { range: '90d' }, opts),
        get('/api/v1/keys', { page_size: 200 }, opts).catch(() => ({ data: [] })),
      ]);
      if (my !== seq || !ctx.alive()) return;
      const keyRows = (keyList && keyList.data) || [];
      const steps = [
        { id: 'password', done: !!info && !info.needs_password_change, action: () => openPasswordDialog({ onDone: () => refresh() }) },
        { id: 'key', done: keyRows.some((k) => k.last_test && k.last_test.status === 'ok') || (ov.success || 0) > 0, href: '#/keys?new=1' },
        { id: 'token', done: ((ov.tokens && ov.tokens.total) || 0) > 0, href: '#/tokens?new=1' },
        { id: 'request', done: (ov.requests || 0) > 0 || ((info && info.log_count) || 0) > 0, href: '#/docs?tab=client' },
      ];
      const doneN = steps.filter((x) => x.done).length;
      if (doneN === steps.length) { slot.replaceChildren(); return; }
      const current = steps.find((x) => !x.done);
      const stepEl = (st, i) => {
        const isCur = st === current;
        const btnCls = `btn btn-sm ${isCur ? 'btn-primary' : ''} step-action`;
        const label = t(`onboard.${st.id}.btn`);
        return h('div', { class: `onboard-step ${st.done ? 'done' : ''} ${isCur ? 'current' : ''}` },
          h('span', { class: 'step-mark' }, st.done ? icon('check', 12) : String(i + 1)),
          h('div', { class: 'step-title' }, t(`onboard.${st.id}.title`)),
          h('div', { class: 'step-desc' }, t(`onboard.${st.id}.desc`)),
          st.done ? h('span', { class: 'step-done-text' }, t('onboard.done'))
            : st.href ? h('a', { class: btnCls, href: st.href }, label)
              : h('button', { class: btnCls, type: 'button', onclick: st.action }, label));
      };
      slot.replaceChildren(h('section', { class: 'ov-onboard' },
        h('div', { class: 'ov-onboard-head' },
          h('h2', { class: 'ov-onboard-title' }, t('onboard.title')),
          h('span', { class: 'ov-onboard-count' }, t('ov5.onboardDone', { n: doneN, total: steps.length })),
          h('button', { type: 'button', class: 'ov-link ov-onboard-dismiss', onclick: () => { lsSet(ONBOARD_KEY, '1'); slot.replaceChildren(); } }, t('onboard.dismiss'))),
        h('div', { class: 'onboard-steps' }, steps.map(stepEl))));
    } catch (e) { if (e.name !== 'AbortError') console.warn('onboarding', e); }
  }
  return { el: slot, refresh };
}

export function render(el, ctx) {
  let range = RANGES.includes(ctx.params.range) ? ctx.params.range : (lsGet('xr.range') || '24h');
  if (!RANGES.includes(range)) range = '24h';
  let chart = null;

  // --- top bar: key availability + range ---
  const pill = h('a', { class: 'ov-pill', href: '#/keys', hidden: true });
  const rangeSel = segmented({
    options: RANGES.map((v) => ({ value: v, label: t(`range.${v}`) })), value: range,
    onChange: (v) => { range = v; lsSet('xr.range', v); ctx.setParams({ range: v }); load(); },
  });
  ctx.setActions(pill, rangeSel);

  // --- sections ---
  const onboard = onboarding(ctx);
  const cell = (cls) => h('div', { class: `ov-metric ${cls || ''}` }, h('div', { class: 'eyebrow' }, h('span', { class: 'skeleton', style: { width: '48px' } })),
    h('div', { class: 'big-num' }, h('span', { class: 'skeleton skeleton-lg', style: { width: '120px', display: 'inline-block' } })), h('div', { class: 'ov-metric-sub' }));
  const strip = h('section', { class: 'ov-strip hairline-grid' }, cell(), cell(), cell(), cell(), cell());
  const legend = h('div', { class: 'ov-legend' });
  const chartHost = h('div', { class: 'ov-chart-host' }, h('div', { class: 'skeleton skeleton-chart' }));
  const chartSec = h('section', { class: 'ov-chart' }, legend, chartHost);
  const modelsBody = h('div', { class: 'ov-models-body' }, h('div', { class: 'ov-loading' }, h('span', { class: 'skeleton', style: { width: '60%' } })));
  const modelsSec = h('section', { class: 'ov-block ov-models' },
    h('div', { class: 'eyebrow' }, h('span', null, t('ov5.models')), h('a', { href: '#/models' }, t('ov5.viewAll'))), modelsBody);
  const attnBody = h('div', { class: 'ov-attn-body' }, h('div', { class: 'ov-loading' }, h('span', { class: 'skeleton', style: { width: '70%' } })));
  const attnSec = h('section', { class: 'ov-block ov-attn' },
    h('div', { class: 'eyebrow' }, h('span', null, t('ov5.attn')), h('a', { href: ATTN_HASH, onclick: go(ctx, ATTN_HASH) }, t('ov5.viewAll'))), attnBody);
  const poolSquares = h('div', { class: 'key-pool' });
  const poolLegend = h('span', { class: 'ov-pool-legend' });
  const poolSec = h('section', { class: 'ov-pool' },
    h('span', { class: 'ov-pool-label' }, t('ov5.pool')), poolSquares, poolLegend, h('a', { class: 'ov-pool-link', href: '#/keys' }, t('ov5.viewAll')));
  el.classList.add('ov5');
  el.append(onboard.el, strip, chartSec, h('div', { class: 'ov-split' }, modelsSec, attnSec), poolSec);

  // --- helpers ---
  const interval = () => (range === '30d' || range === '90d' ? 'day' : range === '1h' ? '5m' : 'hour');
  const stepMs = () => ({ '5m': 300e3, hour: 3600e3, day: 86400e3 }[interval()]);
  const tzOffset = () => -new Date().getTimezoneOffset();
  const rLabel = () => t(`range.${range}`);

  function sums(points) {
    const o = { requests: 0, success: 0, errors: 0, cost: 0, latSum: 0, tokens: 0 };
    points.forEach((p) => {
      const r = p.requests || 0;
      o.requests += r; o.errors += p.errors || 0; o.success += p.success ?? (r - (p.errors || 0));
      o.cost += p.cost_usd || 0; o.latSum += (p.avg_latency_ms || 0) * r; o.tokens += p.total_tokens || 0;
    });
    o.rate = o.requests ? o.success / o.requests : null;
    o.latency = o.requests ? o.latSum / o.requests : null;
    return o;
  }

  function metric({ label, tip, value, accent, sub, extra }) {
    const [num, unit] = splitUnit(value);
    return h('div', { class: 'ov-metric' },
      h('div', { class: 'eyebrow' }, h('span', { 'data-tip': tip || null }, label)),
      h('div', { class: `big-num ${accent ? 'is-accent' : ''}` }, num, unit ? h('span', { class: 'unit' }, unit) : null),
      extra || null,
      h('div', { class: 'ov-metric-sub' }, sub));
  }

  function vsPct(cur, prev, money) {
    if (prev == null) return t('ov.inRange', { r: rLabel() });
    if (!prev) return t('ov5.noPrev', { r: rLabel() });
    const d = money ? `${cur - prev < 0 ? '−' : '+'}${fmtUSD(Math.abs(cur - prev))}` : `${cur >= prev ? '+' : '−'}${(Math.abs(cur - prev) / prev * 100).toFixed(1)}%`;
    return t('ov5.vs', { r: rLabel(), d });
  }

  function renderStrip(o, prev) {
    const has = (o.requests || 0) > 0;
    const pr = prev && prev.requests ? prev : null;
    let rateSub = t('ov.inRange', { r: rLabel() });
    if (prev && !prev.requests) rateSub = t('ov5.noPrev', { r: rLabel() });
    else if (pr && has) {
      const dp = (o.success_rate - pr.rate) * 100;
      const a = Math.abs(dp);
      const dStr = a < 0.1 ? a.toFixed(2) : a.toFixed(1);
      rateSub = a < 0.005 ? t('ov5.rateSame', { r: rLabel() }) : t(dp > 0 ? 'ov5.rateUp' : 'ov5.rateDown', { r: rLabel(), d: dStr });
    }
    let latSub = t('ov.inRange', { r: rLabel() });
    if (prev && !prev.requests) latSub = t('ov5.noPrev', { r: rLabel() });
    else if (pr && has) {
      const dl = o.avg_latency_ms - pr.latency;
      latSub = Math.abs(dl) < 50 ? t('ov5.latSame', { r: rLabel() }) : t(dl < 0 ? 'ov5.faster' : 'ov5.slower', { r: rLabel(), d: fmtMs(Math.abs(dl)) });
    }
    const input = Math.max(0, (o.prompt_tokens || 0) - (o.cached_tokens || 0));
    const cached = o.cached_tokens || 0;
    const output = (o.completion_tokens || 0) + (o.reasoning_tokens || 0);
    const tokTotal = input + cached + output;
    const seg = (n, cls) => (n > 0 ? h('span', { class: `tok-sw ${cls}`, style: { flex: `${n} 1 0` } }) : null);
    strip.replaceChildren(
      metric({ label: t('ov.requests'), value: fmtInt(o.requests || 0), accent: true, sub: vsPct(o.requests || 0, prev ? prev.requests : null) }),
      metric({ label: t('ov.successRate'), tip: t('ov5.successTip'), value: has ? fmtPct(o.success_rate) : '—', sub: rateSub }),
      metric({ label: t('ov.avgLatency'), tip: t('ov.endToEnd'), value: has ? fmtMs(o.avg_latency_ms) : '—', sub: latSub }),
      metric({ label: t('ov.cost'), value: fmtUSD(o.cost_usd || 0), sub: vsPct(o.cost_usd || 0, prev ? (prev.requests ? prev.cost : 0) : null, true) }),
      metric({
        label: t('ov.tokens'), value: tokTotal ? fmtCompact(tokTotal) : '0',
        extra: tokTotal ? h('div', { class: 'tok-bar ov-tokbar', title: `${t('lg.tok.input')} ${fmtInt(input)}\n${t('lg.tok.cacheRead')} ${fmtInt(cached)}\n${t('lg.tok.output')} ${fmtInt(output)}` }, seg(input, 'in'), seg(cached, 'cr'), seg(output, 'out')) : null,
        sub: tokTotal ? t('ov5.tokSub', { i: fmtCompact(input), c: fmtCompact(cached), o: fmtCompact(output) }) : t('ov.inRange', { r: rLabel() }),
      }));
  }

  // --- chart ---
  function bucketTitle(ms) {
    const d = new Date(ms), e = new Date(ms + stepMs());
    const hm = (x) => `${pad(x.getHours())}:${pad(x.getMinutes())}`;
    if (interval() === 'day') return fmtDate(d);
    return `${fmtDate(d)} ${hm(d)}–${hm(e)}`;
  }
  function xLabels(times) {
    const n = times.length;
    const gap = Math.max(1, Math.round(n / 12));
    return times.map((ms, i) => {
      if (i === n - 1) return t('ov5.now');
      if (n - 1 - i < gap * 1.5) return null;
      const d = new Date(ms);
      if (interval() === '5m') return d.getMinutes() % 15 === 0 ? `${pad(d.getHours())}:${pad(d.getMinutes())}` : null;
      if (interval() === 'hour' && range === '24h') return d.getHours() % 6 === 0 ? `${pad(d.getHours())}:00` : null;
      if (interval() === 'hour') return d.getHours() === 0 && (range === '7d' || d.getDate() % 5 === 1) ? fmtDate(d) : null;
      const every = range === '90d' ? 14 : 5;
      return i % every === 0 ? fmtDate(d) : null;
    });
  }
  function drawChart(ts, prevTs) {
    if (chart && chart.destroy) chart.destroy();
    chart = null;
    const pts = (ts.series && ts.series[0] && ts.series[0].points) || [];
    const byT = new Map(pts.map((p) => [new Date(p.t).getTime(), p]));
    const times = (ts.buckets || []).map((b) => new Date(b).getTime());
    const total = pts.reduce((a, p) => a + (p.requests || 0), 0);
    const R = RANGE_MS[range];
    const lgItem = (cls, text) => h('span', { class: `ov-lg ${cls}` }, h('i'), text);
    const perKey = { '5m': 'ov5.lg.req5m', hour: 'ov5.lg.reqHour', day: 'ov5.lg.reqDay' }[interval()];
    legend.replaceChildren(lgItem('lg-cur', t(perKey)), prevTs ? lgItem('lg-prev', t('ov5.lg.prev', { r: rLabel() })) : null, lgItem('lg-fail', t('ov5.lg.fail')));
    if (!times.length || !total) {
      chartHost.replaceChildren(h('div', { class: 'chart-empty ov-chart-empty' }, h('span', { class: 'empty-icon' }, icon('activity', 18)), h('span', null, t('ov.noTraffic'))));
      return;
    }
    const pick = (m) => (ms) => m.get(ms) || null;
    const cur = pick(byT);
    const values = times.map((ms) => (cur(ms) ? cur(ms).requests || 0 : 0));
    const errs = times.map((ms) => (cur(ms) ? cur(ms).errors || 0 : 0));
    let prev = null;
    if (prevTs) {
      const pp = (prevTs.series && prevTs.series[0] && prevTs.series[0].points) || [];
      const pm = new Map(pp.map((p) => [new Date(p.t).getTime(), p]));
      prev = times.map((ms) => { const p = pm.get(ms - R); return p ? p.requests || 0 : 0; });
    }
    const fail = values.map((v, i) => v > 0 && errs[i] / v > 0.05);
    chart = areaChart({
      values, prev, fail, height: 280, yFormat: fmtCompact, partialLast: times[times.length - 1] + stepMs() > Date.now(),
      xLabel: ((labs) => (i) => labs[i])(xLabels(times)),
      tipTitle: (i) => bucketTitle(times[i]),
      tipRows: (i) => [
        [t('ov5.tip.req'), t('ov5.times', { n: fmtInt(values[i]) })],
        [t('ov5.tip.fail'), `${t('ov5.times', { n: fmtInt(errs[i]) })}${values[i] ? `（${fmtPct(errs[i] / values[i])}）` : ''}`, errs[i] ? 'text-danger' : ''],
      ],
    });
    chartHost.replaceChildren(chart);
  }

  // --- models ---
  async function loadModels() {
    try {
      const res = await get('/api/v1/stats/breakdown', { range, by: 'model' }, { signal: ctx.signal });
      const rows = (res.data || []).slice().sort((a, b) => (b.cost_usd || 0) - (a.cost_usd || 0) || (b.requests || 0) - (a.requests || 0));
      if (!rows.length) { modelsBody.replaceChildren(h('div', { class: 'ov-none' }, t('ov.noTraffic'))); return; }
      // Top 5; the rest folds into one row so the columns add up to the totals above.
      const top = rows.slice(0, rows.length > 6 ? 5 : 6);
      const rest = rows.slice(top.length);
      const list = top.map((r) => ({ id: r.id, name: !r.id || r.id === '(unknown)' ? t('ov.noModel') : r.id === 'other' ? t('ov.other') : r.id, requests: r.requests || 0, cost: r.cost_usd || 0, link: r.id && r.id !== 'other' && r.id !== '(unknown)' }));
      if (rest.length) list.push({ name: t('ov5.otherModels', { n: rest.length }), requests: rest.reduce((a, r) => a + (r.requests || 0), 0), cost: rest.reduce((a, r) => a + (r.cost_usd || 0), 0), other: true });
      const maxCost = Math.max(...list.filter((x) => !x.other).map((x) => x.cost), 1e-12);
      modelsBody.replaceChildren(h('table', { class: 'ov-mtable' },
        h('thead', null, h('tr', null, h('th', null, h('span', { class: 'sr-only' }, t('ov.by.model'))), h('th'), h('th', { class: 'num' }, t('ov.requests')), h('th', { class: 'num' }, t('ov.cost')))),
        h('tbody', null, list.map((m, i) => {
          const when = range === '1h' || range === '7d' ? `&range=${range}` : range === '24h' ? '' : `&from=${encodeURIComponent(new Date(Date.now() - RANGE_MS[range]).toISOString())}`;
          const hash = m.link ? `#/logs?type=request&model=${encodeURIComponent(m.id)}${when}` : null;
          return h('tr', { class: hash ? 'clickable' : '', title: hash ? t('ov.viewLogs') : null, onclick: hash ? () => ctx.navigate(hash) : null },
            h('td', { class: `ov-mname ${m.other ? 'muted' : 'mono'}` }, m.name),
            h('td', { class: 'ov-mbar' }, m.other ? null : h('div', { class: 'ov-bar' }, h('span', { style: { width: `${Math.max(0.5, (m.cost / maxCost) * 100).toFixed(1)}%`, background: `var(--c${Math.min(6, i + 1)})` } }))),
            h('td', { class: 'num' }, fmtInt(m.requests)),
            h('td', { class: 'num' }, fmtUSD(m.cost)));
        }))));
    } catch (e) { if (e.name !== 'AbortError') modelsBody.replaceChildren(h('div', { class: 'ov-none' }, t('common.loadFailed'))); }
  }

  // --- events needing attention (pm-spec 9.6): warnings and errors of the last 24 hours ---
  async function loadAttention(keysP) {
    try {
      const res = await get('/api/v1/logs', { type: 'system', problems: 1, from: new Date(Date.now() - 86400e3).toISOString(), page_size: 100 }, { signal: ctx.signal });
      const keys = await keysP;
      // The same failure repeated (e.g. a scheduled job) is one line with a count.
      const groups = [];
      const byKey = new Map();
      (res.data || []).forEach((e) => {
        const line = eventSentence(e);
        const k = `${e.action}|${e.target_type}|${e.target_id}|${line}`;
        const g = byKey.get(k);
        if (g) g.n++; else { const ng = { e, line, n: 1 }; byKey.set(k, ng); groups.push(ng); }
      });
      const seenAuto = new Set((res.data || []).filter((e) => e.action === 'key.auto_disable').map((e) => String(e.target_id)));
      const earlier = (keys || []).filter((k) => keyState(k) === 'auto' && !seenAuto.has(String(k.id)));
      const foot = earlier.length ? h('div', { class: 'ov-attn-foot' }, t('ov5.moreDisabled', { n: earlier.length, names: earlier.slice(0, 3).map((k) => k.name).join('、') + (earlier.length > 3 ? '…' : '') })) : null;
      if (!groups.length) {
        attnBody.replaceChildren(h('div', { class: 'ov-ok' }, h('span', { class: 'empty-icon' }, icon('checkCircle', 18)), h('div', { class: 'ov-ok-title' }, t('ov5.allOk')), h('div', { class: 'ov-ok-desc' }, t('ov5.allOkDesc'))), ...(foot ? [foot] : []));
        return;
      }
      attnBody.replaceChildren(h('div', { class: 'ov-attn-list' }, groups.slice(0, 5).map(({ e, line, n }) => h('a', {
        class: 'ov-ev', href: `${ATTN_HASH}&event=${e.id}`, onclick: go(ctx, `${ATTN_HASH}&event=${e.id}`), title: t('ov.attnOpen'),
      },
      h('span', { class: `level-dot ${e.level === 'error' ? 'is-error' : e.level === 'warn' ? 'is-warning' : ''}` }),
      h('span', { class: 'ov-ev-text' }, line, n > 1 ? h('span', { class: 'ov-ev-n' }, t('ov5.repeat', { n: n - 1 })) : null),
      h('span', { class: 'ov-ev-time', title: fmtDateTime(e.created_at) }, listTime(e.created_at))))),
      ...[groups.length > 5 ? h('div', { class: 'ov-attn-foot' }, t('ov5.moreEvents', { n: groups.length - 5 })) : null, foot].filter(Boolean));
    } catch (e) {
      if (e.name !== 'AbortError') attnBody.replaceChildren(h('div', { class: 'ov-none' }, t('common.loadFailed')));
    }
  }

  // --- upstream keys strip (one 28×10 square per key) ---
  function renderPool(keys) {
    const st = { active: 0, cooldown: 0, auto: 0, disabled: 0 };
    const sorted = keys.slice().sort((a, b) => (a.priority - b.priority) || String(a.name).localeCompare(String(b.name)));
    poolSquares.replaceChildren(...sorted.map((k) => {
      const s = keyState(k); st[s]++;
      return h('a', { class: `kp kp-${s}`, href: `#/keys?open=${k.id}`, 'data-tip': t('ov5.poolTip', { name: k.name, s: t(`ov5.st.${s}`) }), 'aria-label': t('ov5.poolTip', { name: k.name, s: t(`ov5.st.${s}`) }) });
    }));
    poolLegend.textContent = keys.length ? t('ov5.poolLegend', { a: st.active, c: st.cooldown, x: st.auto, d: st.disabled }) : t('ov5.noKeys');
    pill.hidden = false;
    pill.classList.toggle('is-danger', keys.length > 0 && st.active === 0);
    pill.replaceChildren(h('span', { class: 'ov-pill-dot' }), t('ov5.keysPill', { a: st.active, n: keys.length }));
  }

  // --- load ---
  let seq = 0;
  async function load() {
    const my = ++seq;
    const opts = { signal: ctx.signal };
    const keysP = get('/api/v1/keys', { page_size: 200 }, opts).then((r) => (r && r.data) || []).catch(() => null);
    keysP.then((k) => { if (k && my === seq) renderPool(k); });
    const pOv = get('/api/v1/stats/overview', { range }, opts);
    const pTs = get('/api/v1/stats/timeseries', { range, interval: interval(), group: 'none', tz_offset_minutes: tzOffset() }, opts);
    const pr = PREV_RANGE[range];
    const pPrev = pr ? get('/api/v1/stats/timeseries', { range: pr, interval: interval(), group: 'none', tz_offset_minutes: tzOffset() }, opts).catch(() => null) : Promise.resolve(null);
    loadAttention(keysP);
    onboard.refresh();
    try {
      const [o, ts, prevTs] = await Promise.all([pOv, pTs, pPrev]);
      if (my !== seq) return;
      if (ctx.reportKeys) ctx.reportKeys(o.keys);
      // Previous window: the same length, right before the first bucket of this one.
      let prev = null;
      if (prevTs) {
        const first = ts.buckets && ts.buckets.length ? new Date(ts.buckets[0]).getTime() : Date.now() - RANGE_MS[range];
        const from = first - RANGE_MS[range];
        const pp = ((prevTs.series && prevTs.series[0] && prevTs.series[0].points) || []).filter((p) => { const x = new Date(p.t).getTime(); return x >= from && x < first; });
        prev = sums(pp);
      }
      renderStrip(o, prev);
      drawChart(ts, prevTs);
      loadModels();
    } catch (e) {
      if (e.name !== 'AbortError') toastError(e);
    }
  }

  load();
  const timer = setInterval(() => { if (!document.hidden) load(); }, 60000);
  return () => { clearInterval(timer); if (chart && chart.destroy) chart.destroy(); };
}
