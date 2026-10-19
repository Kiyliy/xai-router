// 数据与存储 (#/data): storage strip, log retention, purge (with estimate), compaction, system status.
// v0.5 layout (frames 69-3508 / 127-18793): a four-cell strip, then two columns split by hairlines.
import { h, fmtBytes, fmtDuration, fmtDateTime, fmtInt, fmtTotal, fmtCompactLong, toLocalInput, debounce } from '../util.js';
import { jobPanel, asJob } from '../jobs.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, put, post, del } from '../api.js';
import { toast, toastError, confirmDialog, input, button, busy, kv, errorMessage, statusDot } from '../ui.js';

const mono = (v) => h('span', { class: 'mono' }, v);
const listTime = (iso) => fmtDateTime(iso, { seconds: false });
const daysAgo = (iso) => Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 86400e3));

export function render(el, ctx) {
  el.classList.add('pg-data');
  let info = null;
  let retention = null; // saved log_retention_days
  let diskWarn = null; // saved disk_warn_percent
  let eventTotal = null; // audit + system records

  // ---- 1. storage strip ----
  const stat = (l) => {
    const v = h('div', { class: 'big-num' }, '—'), s = h('div', { class: 'data-stat-sub' });
    return { el: h('div', { class: 'data-stat' }, h('div', { class: 'data-stat-l' }, l), v, s), v, s };
  };
  const stDb = stat(t('settings.dbSize')), stLogs = stat(t('data.requestLogs')), stEv = stat(t('data.eventRecords')), stOld = stat(t('data.oldestRequestLog'));
  const strip = h('div', { class: 'data-strip' }, stDb.el, stLogs.el, stEv.el, stOld.el);

  const section = (title, desc, ...body) => h('section', { class: 'data-sec' },
    h('h2', { class: 'data-sec-title' }, title), desc ? h('p', { class: 'data-sec-desc' }, desc) : null, ...body);
  const numBox = (inp, unit) => h('div', { class: 'set-num data-num' }, inp, h('span', { class: 'set-unit' }, unit));

  // ---- 2. retention ----
  const retInput = input({ type: 'number', min: 0, max: 3650, step: 1, inputmode: 'numeric', 'aria-label': t('settings.f.log_retention_days') });
  const diskInput = input({ type: 'number', min: 50, max: 99, step: 1, inputmode: 'numeric', 'aria-label': t('settings.f.disk_warn_percent') });
  const retSave = button(t('common.save'), { onClick: () => saveRetention() });
  const retSec = section(t('data.retention'), t('data.retentionSub'),
    h('div', { class: 'data-line' }, numBox(retInput, t('data.daysUnit')), h('span', { class: 'data-hint' }, t('data.retentionHint')), retSave),
    h('div', { class: 'data-line' }, numBox(diskInput, '%'), h('span', { class: 'data-hint' }, t('data.diskWarnLine'))));
  retSec.id = 'data-retention';
  const changedNum = (inp, saved) => saved != null && inp.value !== '' && Number(inp.value) !== saved;
  const retDirty = () => changedNum(retInput, retention) || changedNum(diskInput, diskWarn);
  ctx.setGuard(retDirty);
  async function saveRetention() {
    const checks = [[retInput, 'log_retention_days', 0, 3650], [diskInput, 'disk_warn_percent', 50, 99]];
    const body = {};
    for (const [inp, k, a, b] of checks) {
      const v = Number(inp.value);
      const bad = inp.value === '' || !Number.isInteger(v) || v < a || v > b;
      inp.classList.toggle('invalid', bad);
      if (bad) {
        toast(t('err.settingsRange', { field: t(`settings.f.${k}`), a, b }), 'warning');
        inp.focus();
        return;
      }
      body[k] = v;
    }
    if (body.log_retention_days === retention) delete body.log_retention_days;
    if (body.disk_warn_percent === diskWarn) delete body.disk_warn_percent;
    if (!Object.keys(body).length) return;
    await busy(retSave, async () => {
      try {
        const res = await put('/api/v1/settings', body);
        const r = res && typeof res === 'object' ? res : {};
        retention = r.log_retention_days ?? body.log_retention_days ?? retention;
        diskWarn = r.disk_warn_percent ?? body.disk_warn_percent ?? diskWarn;
        retInput.value = retention;
        diskInput.value = diskWarn;
        toast(t('settings.saved'), 'success');
        // The threshold changes system/info disk_warning, which drives the global banner.
        if ('disk_warn_percent' in body) loadInfo(); else if (info) paintInfo(info);
      } catch (e) { toastError(e); }
    });
  }

  // ---- 3. purge ----
  const before = input({ type: 'datetime-local', class: 'input mono', value: toLocalInput(new Date(Date.now() - 30 * 86400e3).toISOString()), 'aria-label': t('settings.deleteBefore') });
  const estN = h('span', { class: 'n' }, '—');
  const estText = h('span', { class: 'estimate-l' }, t('common.loading'));
  const estSub = h('div', { class: 'sub' });
  const estimate = h('div', { class: 'data-estimate', 'aria-live': 'polite' }, h('div', { class: 'estimate-main' }, estN, estText), estSub);
  const purgeLabel = h('span', null, t('settings.purge'));
  const purgeBtn = h('button', { class: 'btn btn-danger', type: 'button', disabled: true, onclick: () => purge() }, purgeLabel);
  let est = null; // { total, kind, iso } for the date currently chosen
  let estLoading = false;
  const purgeText = (r) => (r && r.freed_bytes ? t('settings.purgedFreed', { n: fmtInt(r.deleted ?? 0), freed: fmtBytes(r.freed_bytes) }) : t('settings.purged', { n: fmtInt((r && r.deleted) ?? 0) }));
  const purgeJob = jobPanel({
    kind: 'purge_logs', signal: ctx.signal,
    describe: (j) => purgeText(j.result),
    onFinish: (j) => {
      if (j.state === 'done') toast(purgeText(j.result), 'success'); else toast(j.error || t('jobs.failed'), 'error', 6000);
      if (ctx.alive()) { loadInfo(); loadEstimate(); }
    },
    onState: (running) => syncPurgeBtn(running),
  });
  const purgeSec = section(t('data.purgeTitle'), t('data.purgeSub'),
    h('div', { class: 'data-line' }, h('span', { class: 'data-hint data-hint-text' }, t('data.beforePre')), before, t('data.beforePost') ? h('span', { class: 'data-hint data-hint-text' }, t('data.beforePost')) : null),
    estimate, h('div', null, purgeBtn), purgeJob);

  const approx = (kind) => kind && kind !== 'exact';
  const countText = (e) => (approx(e.kind) ? fmtCompactLong(e.total) : fmtInt(e.total));
  function syncPurgeBtn(running) {
    const n = est ? est.total : 0;
    const busyJob = running ?? purgeJob.running();
    purgeBtn.disabled = busyJob || estLoading || !est || !(n > 0);
    purgeLabel.textContent = est && n > 0 ? t(approx(est.kind) ? 'data.purgeNApprox' : 'data.purgeN', { n: countText(est) }) : t('settings.purge');
  }
  function paintEstimate() {
    if (!est) { estimate.classList.remove('has-rows'); estN.textContent = ''; estText.textContent = before.value ? t('common.loading') : t('data.pickDate'); estSub.textContent = ''; syncPurgeBtn(); return; }
    const n = est.total;
    estimate.classList.toggle('has-rows', n > 0);
    estN.textContent = n > 0 ? `${approx(est.kind) ? t('data.approxSign') : ''}${countText(est)}` : '';
    estText.textContent = n > 0 ? t('data.willDelete') : t('data.willDeleteNone');
    const oldest = info && info.oldest_log_at;
    estSub.textContent = n > 0 && oldest ? t('data.estRange', { a: listTime(oldest), b: listTime(est.iso) })
      : !n && oldest ? t('data.oldestIs', { d: listTime(oldest) }) : '';
    estimate.title = approx(est.kind) ? t('common.totalApproxHint') : '';
    syncPurgeBtn();
  }
  let estSeq = 0;
  async function loadEstimate() {
    const seq = ++estSeq;
    est = null;
    if (!before.value) { estLoading = false; paintEstimate(); return; }
    const iso = new Date(before.value).toISOString();
    estLoading = true; paintEstimate();
    try {
      // Purge deletes created_at < before; the list filter is created_at <= to, hence the 1 ms.
      const to = new Date(new Date(iso).getTime() - 1).toISOString();
      const r = await get('/api/v1/logs', { to, page_size: 1 }, { signal: ctx.signal });
      if (seq !== estSeq) return;
      est = { total: r.total || 0, kind: r.total_kind || 'exact', iso };
    } catch (e) {
      if (e.name === 'AbortError' || seq !== estSeq) return;
      estText.textContent = errorMessage(e);
    } finally {
      if (seq === estSeq) { estLoading = false; if (est) paintEstimate(); else syncPurgeBtn(); }
    }
  }
  const loadEstimateSoon = debounce(loadEstimate, 250);
  before.addEventListener('input', () => { est = null; estLoading = true; paintEstimate(); loadEstimateSoon(); });
  async function purge() {
    if (!before.value || !est || !(est.total > 0)) return;
    const iso = est.iso;
    const label = t(approx(est.kind) ? 'data.purgeNApprox' : 'data.purgeN', { n: countText(est) });
    const ok = await confirmDialog({
      title: t('settings.purgeTitle'),
      message: t(approx(est.kind) ? 'data.purgeMsgApprox' : 'data.purgeMsg', { n: countText(est), d: listTime(iso) }),
      confirmText: label, danger: true,
    });
    if (!ok) return;
    await busy(purgeBtn, async () => {
      try { const r = await del('/api/v1/logs', { before: iso }); purgeJob.track(asJob(r, 'purge_logs')); } catch (e) { toastError(e); }
    });
    syncPurgeBtn();
  }

  // ---- 4. compact ----
  const freeN = h('div', { class: 'big-num big-num-md' }, '—');
  const freeSub = h('div', { class: 'data-hint' });
  const compactBtn = h('button', { class: 'btn', type: 'button', onclick: () => compact() }, h('span', null, t('data.compactNow')));
  const compactJob = jobPanel({
    kind: 'compact', signal: ctx.signal,
    describe: (j) => {
      const r = j.result || {};
      return t('settings.compacted', { freed: fmtBytes(r.freed_bytes || 0), before: fmtBytes(r.size_before || 0), after: fmtBytes(r.size_after || 0) });
    },
    onFinish: (j) => {
      if (j.state === 'done') toast(t('settings.compacted', { freed: fmtBytes((j.result || {}).freed_bytes || 0), before: fmtBytes((j.result || {}).size_before || 0), after: fmtBytes((j.result || {}).size_after || 0) }), 'success');
      else toast(j.error || t('jobs.failed'), 'error', 6000);
      if (ctx.alive()) loadInfo();
    },
    onState: (running) => { compactBtn.disabled = running; },
  });
  const vacuumWarn = h('div', { class: 'callout callout-warning', hidden: true }, icon('alert', 16), h('span', null, t('settings.compactVacuumWarn')));
  const compactSec = section(t('settings.compact'), t('data.compactSub'),
    h('div', { class: 'data-compact' }, h('div', null, h('div', { class: 'data-stat-l' }, t('data.reclaimable')), freeN), compactBtn),
    freeSub, vacuumWarn, compactJob);
  async function compact() {
    const legacy = info && info.db_auto_vacuum && info.db_auto_vacuum !== 'incremental';
    const ok = await confirmDialog({
      title: t('settings.compactTitle'), message: t('settings.compactMsg'), confirmText: t('settings.compact'), danger: !!legacy,
      extra: legacy ? h('div', { class: 'callout callout-warning' }, icon('alert', 16), h('span', null, t('settings.compactVacuumWarn'))) : null,
    });
    if (!ok) return;
    await busy(compactBtn, async () => {
      try { const r = await post('/api/v1/system/compact', {}); compactJob.track(asJob(r, 'compact')); } catch (e) { toastError(e); }
    });
    if (compactJob.running()) compactBtn.disabled = true;
  }

  // ---- 5. system status ----
  const sysBody = h('div', null, h('div', { class: 'skeleton', style: { width: '80%' } }));
  const sysSec = section(t('data.system'), null, sysBody);

  el.append(strip, h('div', { class: 'data-cols' }, h('div', { class: 'data-col' }, retSec, purgeSec), h('div', { class: 'data-col' }, compactSec, sysSec)));

  const keepText = () => (retention == null ? '' : retention > 0 ? t('data.keepDays', { n: fmtInt(retention) }) : t('data.keepForever'));
  function paintInfo(i) {
    const size = i.db_size_bytes || 0, free = i.db_free_bytes || 0;
    stDb.v.textContent = fmtBytes(size);
    stDb.s.textContent = i.db_free_bytes != null ? t('data.reclaimShort', { v: fmtBytes(free) }) : '';
    stLogs.v.textContent = i.log_count != null ? fmtTotal(i.log_count, i.log_count >= 1e4 ? 'approx' : 'exact') : '—';
    stLogs.v.title = i.log_count != null ? fmtInt(i.log_count) : '';
    stLogs.s.textContent = keepText();
    stEv.v.textContent = eventTotal != null ? fmtInt(eventTotal) : '—';
    stEv.s.textContent = t('data.keepForever');
    stOld.v.textContent = i.oldest_log_at ? listTime(i.oldest_log_at) : '—';
    stOld.v.title = i.oldest_log_at ? fmtDateTime(i.oldest_log_at) : '';
    stOld.s.textContent = i.oldest_log_at ? t('data.daysAgo', { n: fmtInt(daysAgo(i.oldest_log_at)) }) : t('data.noLogs');

    freeN.textContent = fmtBytes(free);
    const av = i.db_auto_vacuum || '';
    freeSub.textContent = [size && free ? t('data.freeShare', { p: ((free / size) * 100).toFixed(free / size < 0.1 ? 1 : 0) }) : '', av && av !== 'incremental' ? t('data.avLegacyHint') : ''].filter(Boolean).join('；');
    freeSub.hidden = !freeSub.textContent;
    vacuumWarn.hidden = !av || av === 'incremental';

    sysBody.replaceChildren(kv([
      [t('settings.version'), h('span', null, mono(i.version || '—'), i.commit && i.commit !== 'none' ? h('span', { class: 'muted' }, t('data.commitParen', { c: i.commit.slice(0, 12) })) : null)],
      i.build_date && i.build_date !== 'unknown' ? [t('settings.buildDate'), mono(i.build_date)] : null,
      [t('settings.uptime'), i.started_at ? h('span', { title: `${t('settings.startedAt')}: ${fmtDateTime(i.started_at)}` }, fmtDuration(i.uptime_seconds)) : fmtDuration(i.uptime_seconds)],
      [t('settings.goSys'), i.go_sys_bytes != null ? mono(fmtBytes(i.go_sys_bytes)) : null],
      [t('settings.logQueue'), t('data.nItems', { n: fmtInt(i.log_queue_length) })],
      i.log_dropped ? [t('settings.logDropped'), statusDot(t('data.nItems', { n: fmtInt(i.log_dropped) }), 'warning')] : null,
      [t('data.newestRequestLog'), i.newest_log_at ? h('span', { title: fmtDateTime(i.newest_log_at) }, listTime(i.newest_log_at)) : null],
      [t('data.disk'), diskLine(i)],
    ]));
  }
  function diskLine(i) {
    const total = i.disk_total_bytes || 0;
    if (!total) return h('span', { class: 'muted' }, t('data.diskUnknown'));
    const free = i.disk_free_bytes || 0;
    const used = Math.max(0, total - free);
    const pct = typeof i.disk_used_percent === 'number' ? i.disk_used_percent : (used / total) * 100;
    const warnAt = i.disk_warn_percent || diskWarn || 85;
    const text = t('data.diskLine', { p: pct.toFixed(pct < 10 ? 1 : 0), total: fmtBytes(total), free: fmtBytes(free) });
    return h('span', { title: t('data.diskWarnAt', { p: warnAt }) }, i.disk_warning ? statusDot(text, 'warning') : text);
  }
  async function loadInfo() {
    try { const i = await ctx.refreshInfo(); if (!i || !ctx.alive()) return; info = i; paintInfo(i); paintEstimate(); } catch (e) { toastError(e); }
  }
  async function loadEvents() {
    try {
      const r = await get('/api/v1/logs', { type: 'audit,system', page_size: 1 }, { signal: ctx.signal });
      eventTotal = r.total ?? null;
      if (info) paintInfo(info);
    } catch (e) { if (e.name !== 'AbortError') eventTotal = null; }
  }
  async function loadSettings() {
    try {
      const s = await get('/api/v1/settings', null, { signal: ctx.signal });
      retention = s && s.log_retention_days != null ? s.log_retention_days : 0;
      retInput.value = retention;
      diskWarn = s && s.disk_warn_percent != null ? s.disk_warn_percent : 85;
      diskInput.value = diskWarn;
      if (info) paintInfo(info);
    } catch (e) { if (e.name !== 'AbortError') toastError(e); }
  }

  loadSettings();
  loadInfo();
  loadEvents();
  loadEstimate();
  purgeJob.resume();
  compactJob.resume();

  const beforeUnload = (e) => { if (retDirty()) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', beforeUnload);
  return () => window.removeEventListener('beforeunload', beforeUnload);
}
