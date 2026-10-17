// Background jobs (GET /api/v1/jobs, /api/v1/jobs/{id}): a progress panel that polls a
// job, survives navigation (it finds the running job again via GET /api/v1/jobs) and shows
// the result or error at the end.
import { h, fmtInt, fmtDuration, fmtBytes } from './util.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { get } from './api.js';
import { spinner } from './ui.js';

const SS = 'xr.job.';
const ssGet = (k) => { try { return sessionStorage.getItem(SS + k); } catch { return null; } };
const ssSet = (k, v) => { try { if (v == null) sessionStorage.removeItem(SS + k); else sessionStorage.setItem(SS + k, String(v)); } catch { /* ignore */ } };

/** A response from an endpoint that may answer with a job (202) or, synchronously, with the result itself. */
export function asJob(res, kind) {
  if (res && typeof res === 'object' && res.id != null && res.state) return res;
  const now = new Date().toISOString();
  return { id: null, kind, state: 'done', progress: 0, total: 0, result: res || {}, started_at: now, finished_at: now };
}

const secsBetween = (a, b) => Math.max(0, ((b ? new Date(b).getTime() : Date.now()) - new Date(a).getTime()) / 1000);

/**
 * jobPanel({ kind, signal, describe(job) → Node|string|null, actions(job) → Node[], onFinish(job), onState(running) })
 * Returns an element with .track(job), .resume() and .running().
 */
export function jobPanel({ kind, signal, describe, actions, onFinish, onState } = {}) {
  const el = h('div', { class: 'job-panel', hidden: true, role: 'status', 'aria-live': 'polite' });
  let cur = null, timer = null, watched = false;

  const setRunning = (on) => { if (onState) try { onState(on); } catch (e) { console.error(e); } };

  const fill = (...nodes) => el.replaceChildren(...nodes.flat().filter((n) => n != null && n !== false));
  function paint(job) {
    el.hidden = false;
    const running = job.state === 'running';
    el.className = `job-panel job-${job.state}`;
    const title = t(`jobs.kind.${job.kind || kind}`);
    if (running) {
      const total = job.total || 0, prog = job.progress || 0;
      const pct = total ? Math.min(100, (prog / total) * 100) : null;
      fill(
        h('div', { class: 'job-head' },
          spinner(15),
          h('span', { class: 'job-title' }, title),
          h('span', { class: 'job-count mono' }, job.phase === 'reclaim'
            ? (total ? `${fmtBytes(prog)} / ${fmtBytes(total)} · ${pct.toFixed(0)}%` : '')
            : total ? `${fmtInt(prog)} / ${fmtInt(total)} · ${pct.toFixed(0)}%` : prog ? fmtInt(prog) : ''),
          h('span', { class: 'toolbar-spacer' }),
          h('span', { class: 'job-elapsed muted' }, t('jobs.elapsed', { d: fmtDuration(secsBetween(job.started_at)) }))),
        h('div', { class: `job-bar ${total ? '' : 'job-bar-indet'}`, role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': total ? String(total) : null, 'aria-valuenow': total ? String(prog) : null },
          h('div', { class: 'job-bar-fill', style: total ? { width: `${pct.toFixed(1)}%` } : null })),
        h('div', { class: 'job-msg muted' }, job.phase === 'reclaim' ? t('jobs.reclaiming') : job.message || t('jobs.background')));
      return;
    }
    const ok = job.state === 'done';
    const close = h('button', { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', 'aria-label': t('common.close'), title: t('jobs.dismiss'), onclick: () => dismiss() }, icon('x', 15));
    fill(
      h('div', { class: 'job-head' },
        icon(ok ? 'checkCircle' : 'xCircle', 16, ok ? 'text-success' : 'text-danger'),
        h('span', { class: 'job-title' }, `${title} · ${t(ok ? 'jobs.done' : 'jobs.failed')}`),
        job.finished_at && job.started_at ? h('span', { class: 'job-elapsed muted' }, t('jobs.tookIn', { d: fmtDuration(secsBetween(job.started_at, job.finished_at)) })) : null,
        h('span', { class: 'toolbar-spacer' }),
        ok && actions ? actions(job) : null,
        close),
      ok ? (describe ? h('div', { class: 'job-msg' }, describe(job)) : null)
        : h('div', { class: 'job-msg text-danger break' }, job.error || job.message || t('jobs.failed')));
  }

  function dismiss() {
    ssSet(kind, null);
    el.hidden = true;
    el.replaceChildren();
    cur = null;
  }

  async function poll() {
    timer = null;
    if (!cur || cur.id == null || (signal && signal.aborted)) return;
    try {
      const j = await get(`/api/v1/jobs/${cur.id}`, null, { signal });
      if (!cur || j.id !== cur.id) return;
      cur = j;
      paint(j);
      if (j.state === 'running') timer = setTimeout(poll, 800);
      else finished(j);
    } catch (e) {
      if (e.name === 'AbortError' || (signal && signal.aborted)) return;
      if (e.status === 404) { dismiss(); setRunning(false); return; } // job expired / server restarted
      timer = setTimeout(poll, 2500); // transient: keep trying (e.g. server restarting)
    }
  }

  function finished(j) {
    setRunning(false);
    if (watched && onFinish) try { onFinish(j); } catch (e) { console.error(e); }
    watched = false;
  }

  /** Start showing a job (from a 202 response). */
  function track(job, { live = true } = {}) {
    clearTimeout(timer);
    cur = job;
    if (job.id != null) ssSet(kind, job.id);
    paint(job);
    if (job.state === 'running') { watched = live; setRunning(true); timer = setTimeout(poll, 600); }
    else if (live) { watched = true; finished(job); }
  }

  /** Re-attach to a running job of this kind, or show the last one started from this tab. */
  async function resume() {
    try {
      const res = await get('/api/v1/jobs', null, { signal });
      const list = (res && res.data) || [];
      const running = list.find((j) => j.kind === kind && j.state === 'running');
      if (running) { track(running); return running; }
      const mine = ssGet(kind);
      const last = mine && list.find((j) => String(j.id) === mine);
      if (last) { track(last, { live: false }); return last; }
      if (mine) ssSet(kind, null);
    } catch (e) { if (e.name !== 'AbortError') console.warn('jobs', e); }
    return null;
  }

  if (signal) signal.addEventListener('abort', () => clearTimeout(timer), { once: true });
  el.track = track;
  el.resume = resume;
  el.running = () => !!cur && cur.state === 'running';
  return el;
}
