// Thin fetch wrapper for the same-origin admin API (/api/v1/*).
const TOKEN_KEY = 'xr.token';

function lsGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function lsSet(k, v) { try { v == null ? localStorage.removeItem(k) : localStorage.setItem(k, v); } catch { /* ignore */ } }

export const auth = {
  get token() { return lsGet(TOKEN_KEY); },
  set(token) { lsSet(TOKEN_KEY, token); },
  clear() { lsSet(TOKEN_KEY, null); },
};

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message || code || `HTTP ${status}`);
    this.status = status;
    this.code = code || (status ? `http_${status}` : 'network_error');
  }
}

let unauthorizedHandler = null;
export function onUnauthorized(fn) { unauthorizedHandler = fn; }

/**
 * api('/api/v1/keys', { method, query, body, noAuthRedirect, signal })
 * Resolves to parsed JSON (or null for 204). Rejects with ApiError.
 */
export async function api(path, opts = {}) {
  const { method = 'GET', query, body, noAuthRedirect = false, signal, token } = opts;
  const url = new URL(path, location.origin);
  if (query) for (const [k, v] of Object.entries(query)) {
    if (v !== undefined && v !== null && v !== '') url.searchParams.set(k, String(v));
  }
  const headers = { Accept: 'application/json' };
  const bearer = token ?? auth.token;
  if (bearer) headers.Authorization = `Bearer ${bearer}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  let res;
  try {
    res = await fetch(url.pathname + url.search, {
      method, headers, signal, body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    if (e.name === 'AbortError') throw e;
    throw new ApiError(0, 'network_error', e.message || 'Network error');
  }

  let data = null;
  if (res.status !== 204) {
    const text = await res.text();
    if (text) { try { data = JSON.parse(text); } catch { data = text; } }
  }
  if (!res.ok) {
    const err = data && typeof data === 'object' && data.error ? data.error : {};
    const e = new ApiError(res.status, err.code, err.message || (typeof data === 'string' ? data.slice(0, 200) : res.statusText));
    if (res.status === 401 && !noAuthRedirect) {
      auth.clear();
      if (unauthorizedHandler) unauthorizedHandler(e);
    }
    throw e;
  }
  return data;
}

export const get = (path, query, o = {}) => api(path, { ...o, query });
export const post = (path, body, o = {}) => api(path, { ...o, method: 'POST', body: body ?? {} });
export const put = (path, body, o = {}) => api(path, { ...o, method: 'PUT', body });
export const patch = (path, body, o = {}) => api(path, { ...o, method: 'PATCH', body });
export const del = (path, query, o = {}) => api(path, { ...o, method: 'DELETE', query });
