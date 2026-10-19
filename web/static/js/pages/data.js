// 数据与存储 (#/data): storage overview, log retention, purge (with estimate), compaction, system status.
// Moved out of Settings in v0.4.0; the purge estimate (P1-7) is the only new behaviour.
import { h, fmtBytes, fmtDuration, fmtDateTime, fmtInt, fmtTotal, fmtCompactLong, relTime, toLocalInput, debounce } from '../util.js';
import { jobPanel, asJob } from '../jobs.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { get, put, post, del } from '../api.js';
import { card, toast, toastError, confirmDialog, field, input, button, busy, kv, errorMessage, progress } from '../ui.js';

const mono = (v) => h('span', { class: 'mono' }, v);
const title = (ic, text) => h('span', { class: 'card-title-ic' }, icon(ic, 16), text);

export function render(el, ctx) {
  let info = null;
  let retention = null; // saved log_retention_days
  let diskWarn = null; // saved disk_warn_percent

  // ---- 1. storage overview ----
  const stat = (l) => {
    const v = h('div', { class: 'v' }, '—'), s = h('div', { class: 's' });
    return { el: h('div', { class: 'storage-stat' }, h('div', { class: 'l' }, l), v, s), v, s };
  };
  const stDb = stat(t('settings.dbSize')), stLogs = stat(t('settings.logCount')), stOld = stat(t('settings.oldestLog')), stNew = stat(t('settings.newestLog'));
  // Data volume usage (system/info disk_*), under the four stats.
  const diskPct = h('span', { class: 'pct' }, '—'), diskUsed = h('span'), diskFree = h('span', { class: 's' });
  const diskBar = h('div');
  const diskRow = h('div', { class: 'storage-disk' },
    h('div', { class: 'storage-disk-head' }, h('span', { class: 'l' }, t('data.disk')), diskPct, diskUsed, diskFree), diskBar);
  const storageCard = card({
    cls: 'span-2', flush: true,
    body: [h('div', { class: 'storage-stats' }, stDb.el, stLogs.el, stOld.el, stNew.el), diskRow],
  });

  // ---- 2. retention ----
  const retInput = input({ type: 'number', min: 0, max: 3650, step: 1, inputmode: 'numeric' });
  const diskInput = input({ type: 'number', min: 50, max: 99, step: 1, inputmode: 'numeric' });
  const retSave = button(t('common.save'), { variant: 'primary', onClick: () => saveRetention() });
  const retCard = card({
    title: title('clock', t('data.retentionCard')), subtitle: t('data.retentionSub'),
    body: h('div', { class: 'stack' },
      field(t('settings.f.log_retention_days'), h('div', { class: 'input-affix input-affix-end' }, retInput, h('span', { class: 'affix' }, t('data.daysUnit'))), { hint: t('data.retentionHint') }),
      field(t('settings.f.disk_warn_percent'), h('div', { class: 'input-affix input-affix-end' }, diskInput, h('span', { class: 'affix' }, '%')), { hint: t('settings.h.disk_warn_percent') })),
  });
  retCard.id = 'data-retention';
  retCard.append(h('div', { class: 'card-foot-actions' }, retSave));
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
        paintPolicy();
        toast(t('settings.saved'), 'success');
        // The threshold changes system/info disk_warning, which drives the global banner.
        if ('disk_warn_percent' in body) loadInfo(); else if (info) paintInfo(info);
      } catch (e) { toastError(e); }
    });
  }

  // ---- 3. purge ----
  const policy = h('div', { class: 'policy-line' }, icon('info', 14), h('span', null, '—'));
  const before = input({ type: 'datetime-local', value: toLocalInput(new Date(Date.now() - 30 * 86400e3).toISOString()) });
  const estN = h('span', { class: 'n' }, '—');
  const estText = h('span', null, t('common.loading'));
  const estSub = h('span', { class: 'sub' });
  const estimate = h('div', { class: 'estimate', 'aria-live': 'polite' }, estN, h('div', { class: 'estimate-text' }, estText, estSub));
  const purgeLabel = h('span', null, t('settings.purge'));
  const purgeBtn = h('button', { class: 'btn btn-danger', type: 'button', disabled: true, onclick: () => purge() }, icon('trash', 15), purgeLabel);
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
  const purgeCard = card({
    title: title('trash', t('data.purge')), subtitle: t('data.purgeSub'),
    body: h('div', { class: 'stack' }, policy, field(t('settings.deleteBefore'), before), estimate, purgeJob),
  });
  purgeCard.append(h('div', { class: 'card-foot-actions' }, purgeBtn));

  const approx = (kind) => kind && kind !== 'exact';
  const countText = (e) => (approx(e.kind) ? fmtCompactLong(e.total) : fmtInt(e.total));
  function paintPolicy() {
    const span = policy.lastChild;
    if (retention == null) { span.textContent = '—'; return; }
    span.replaceChildren(
      retention > 0 ? t('data.policyDays', { n: fmtInt(retention) }) : t('data.policyForever'), ' ',
      h('a', { class: 'link', href: '#/data', onclick: (e) => { e.preventDefault(); retCard.scrollIntoView({ behavior: 'smooth', block: 'center' }); retInput.focus({ preventScroll: true }); } }, t('data.policyEdit')));
  }
  function syncPurgeBtn(running) {
    const n = est ? est.total : 0;
    const busyJob = running ?? purgeJob.running();
    purgeBtn.disabled = busyJob || estLoading || !est || !(n > 0);
    purgeLabel.textContent = est && n > 0 ? t(approx(est.kind) ? 'data.purgeNApprox' : 'data.purgeN', { n: countText(est) }) : t('settings.purge');
  }
  function paintEstimate() {
    if (!est) { estimate.classList.remove('has-rows'); estN.textContent = '—'; estText.textContent = before.value ? t('common.loading') : t('data.pickDate'); estSub.textContent = ''; syncPurgeBtn(); return; }
    const n = est.total;
    estimate.classList.toggle('has-rows', n > 0);
    estN.textContent = n > 0 ? (approx(est.kind) ? fmtTotal(n, 'approx') : countText(est)) : '0';
    estText.textContent = n > 0 ? t(approx(est.kind) ? 'data.willDeleteApprox' : 'data.willDelete') : t('data.willDeleteNone');
    const oldest = info && info.oldest_log_at;
    estSub.textContent = n > 0 && oldest ? t('data.estRange', { a: fmtDateTime(oldest, { seconds: false }), b: fmtDateTime(est.iso, { seconds: false }) })
      : !n && oldest ? t('data.oldestIs', { d: fmtDateTime(oldest, { seconds: false }) }) : '';
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
      message: t(approx(est.kind) ? 'data.purgeMsgApprox' : 'data.purgeMsg', { n: countText(est), d: fmtDateTime(iso, { seconds: false }) }),
      confirmText: label, danger: true,
    });
    if (!ok) return;
    await busy(purgeBtn, async () => {
      try { const r = await del('/api/v1/logs', { before: iso }); purgeJob.track(asJob(r, 'purge_logs')); } catch (e) { toastError(e); }
    });
    syncPurgeBtn();
  }

  // ---- 4. compact ----
  const freeN = h('span', { class: 'n' }, '—');
  const freeSub = h('span', { class: 'sub' });
  const compactBtn = h('button', { class: 'btn', type: 'button', onclick: () => compact() }, icon('database', 15), h('span', null, t('data.compactNow')));
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
  const compactCard = card({
    title: title('layers', t('settings.compact')), subtitle: t('data.compactSub'),
    body: h('div', { class: 'stack' }, h('div', { class: 'estimate' }, freeN, h('div', { class: 'estimate-text' }, h('span', null, t('data.reclaimable')), freeSub)), vacuumWarn, compactJob),
  });
  compactCard.append(h('div', { class: 'card-foot-actions' }, compactBtn));
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
  const sysCard = card({
    title: title('server', t('data.system')),
    actions: button('', { icon: 'refresh', variant: 'ghost', size: 'icon', attrs: { title: t('common.refresh'), 'aria-label': t('common.refresh') }, onClick: () => loadInfo() }),
    body: sysBody,
  });

  el.append(h('div', { class: 'data-grid' }, storageCard, retCard, purgeCard, compactCard, sysCard));

  function paintInfo(i) {
    const size = i.db_size_bytes || 0, free = i.db_free_bytes || 0;
    stDb.v.textContent = fmtBytes(size);
    stDb.s.textContent = i.db_free_bytes != null ? t('data.freeInFile', { v: fmtBytes(free) }) : '';
    stLogs.v.textContent = i.log_count != null ? fmtTotal(i.log_count, 'approx') : '—';
    stLogs.v.title = i.log_count != null ? fmtInt(i.log_count) : '';
    stLogs.s.textContent = retention == null ? '' : retention > 0 ? t('data.policyDays', { n: fmtInt(retention) }) : t('data.policyForever');
    stOld.v.textContent = i.oldest_log_at ? fmtDateTime(i.oldest_log_at).slice(0, 10) : '—';
    stOld.v.title = i.oldest_log_at ? fmtDateTime(i.oldest_log_at) : '';
    stOld.s.textContent = i.oldest_log_at ? relTime(i.oldest_log_at) : t('data.noLogs');
    stNew.v.textContent = i.newest_log_at ? relTime(i.newest_log_at) : '—';
    stNew.v.title = i.newest_log_at ? fmtDateTime(i.newest_log_at) : '';
    stNew.s.textContent = i.newest_log_at ? fmtDateTime(i.newest_log_at, { seconds: false }) : '';
    paintDisk(i);

    freeN.textContent = fmtBytes(free);
    const av = i.db_auto_vacuum || '';
    freeSub.textContent = [size ? t('data.freeShare', { p: ((free / size) * 100).toFixed(free / size < 0.1 ? 1 : 0) }) : '', av && av !== 'incremental' ? t('data.avLegacyHint') : ''].filter(Boolean).join('；');
    vacuumWarn.hidden = !av || av === 'incremental';

    sysBody.replaceChildren(kv([
      [t('settings.version'), h('span', null, mono(i.version || '—'), i.commit && i.commit !== 'none' ? h('span', { class: 'muted' }, t('data.commitParen', { c: i.commit.slice(0, 12) })) : null)],
      [t('settings.buildDate'), i.build_date && i.build_date !== 'unknown' ? mono(i.build_date) : null],
      [t('settings.goVersion'), mono(i.go_version || '—')],
      [t('settings.startedAt'), i.started_at ? h('span', { title: fmtDateTime(i.started_at) }, fmtDateTime(i.started_at, { seconds: false })) : null],
      [t('settings.uptime'), fmtDuration(i.uptime_seconds)],
      [t('settings.goSys'), i.go_sys_bytes != null ? mono(fmtBytes(i.go_sys_bytes)) : null],
      [t('settings.logQueue'), fmtInt(i.log_queue_length)],
      [t('settings.logDropped'), h('span', { class: i.log_dropped ? 'text-warning' : '' }, fmtInt(i.log_dropped))],
    ]));
  }
  function paintDisk(i) {
    const total = i.disk_total_bytes || 0;
    if (!total) {
      diskPct.textContent = '—'; diskUsed.textContent = t('data.diskUnknown'); diskFree.textContent = ''; diskBar.replaceChildren();
      return;
    }
    const free = i.disk_free_bytes || 0;
    const used = Math.max(0, total - free);
    const pct = typeof i.disk_used_percent === 'number' ? i.disk_used_percent : (used / total) * 100;
    const warnAt = i.disk_warn_percent || diskWarn || 85;
    diskPct.textContent = `${pct.toFixed(pct < 10 ? 1 : 0)}%`;
    diskPct.classList.toggle('text-warning', !!i.disk_warning);
    diskUsed.textContent = t('data.diskUsed', { used: fmtBytes(used), total: fmtBytes(total) });
    diskFree.textContent = `${t('data.diskFree', { v: fmtBytes(free) })} · ${t('data.diskWarnAt', { p: warnAt })}`;
    diskBar.replaceChildren(progress(pct / 100, i.disk_warning ? 'danger' : pct >= warnAt - 10 ? 'warning' : 'accent'));
  }
  async function loadInfo() {
    try { const i = await ctx.refreshInfo(); if (!i || !ctx.alive()) return; info = i; paintInfo(i); paintEstimate(); } catch (e) { toastError(e); }
  }
  async function loadSettings() {
    try {
      const s = await get('/api/v1/settings', null, { signal: ctx.signal });
      retention = s && s.log_retention_days != null ? s.log_retention_days : 0;
      retInput.value = retention;
      diskWarn = s && s.disk_warn_percent != null ? s.disk_warn_percent : 85;
      diskInput.value = diskWarn;
      paintPolicy();
      if (info) paintInfo(info);
    } catch (e) { if (e.name !== 'AbortError') toastError(e); }
  }

  loadSettings();
  loadInfo();
  loadEstimate();
  purgeJob.resume();
  compactJob.resume();

  const beforeUnload = (e) => { if (retDirty()) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('beforeunload', beforeUnload);
  return () => window.removeEventListener('beforeunload', beforeUnload);
}
