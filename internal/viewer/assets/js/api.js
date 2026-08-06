/**
 * Client for the viewer's read-only API. Every call goes to the local process
 * that loaded the report, so there is no auth and no retry policy; a failure
 * here means the server is gone or the report could not be projected.
 */

async function getJSON(path, signal) {
  const resp = await fetch(path, { signal, headers: { Accept: 'application/json' } });
  if (!resp.ok) {
    const detail = (await resp.text()).trim();
    throw new Error(detail || `${resp.status} ${resp.statusText}`);
  }
  return resp.json();
}

export function summary(signal) {
  return getJSON('/api/summary', signal);
}

/**
 * The distance filter is a closed range. Either bound may be absent, which the
 * server reads as "no floor" and "no ceiling", so an unset pair is the whole
 * report rather than an empty one.
 */
export function packagesQuery({ search, minDepth, maxDepth, target, sort, dir, limit, offset }) {
  const params = new URLSearchParams();
  if (search) params.set('search', search);
  if (target) params.set('target', target);
  if (minDepth) params.set('minDepth', String(minDepth));
  if (maxDepth) params.set('maxDepth', String(maxDepth));
  if (sort) params.set('sort', sort);
  if (dir) params.set('dir', dir);
  if (limit !== undefined) params.set('limit', String(limit));
  if (offset !== undefined) params.set('offset', String(offset));
  return params;
}

export function packages(query, signal) {
  return getJSON('/api/packages?' + packagesQuery(query), signal);
}

export function packageDetail(name, { route, versionsOffset, versionsLimit } = {}, signal) {
  const params = new URLSearchParams({ name });
  if (route) params.set('route', route);
  if (versionsOffset !== undefined) params.set('versionsOffset', String(versionsOffset));
  if (versionsLimit !== undefined) params.set('versionsLimit', String(versionsLimit));
  return getJSON('/api/package?' + params, signal);
}

/**
 * The graph expands outward from the compromised package, so a "child" here is
 * a package that *depends on* the one being expanded.
 */
export function graphRoots({ search, limit, offset } = {}, signal) {
  return getJSON('/api/graph/roots?' + graphParams({ search, limit, offset }), signal);
}

export function graphDependents(name, { search, limit, offset } = {}, signal) {
  const params = graphParams({ search, limit, offset });
  params.set('package', name);
  return getJSON('/api/graph/dependents?' + params, signal);
}

function graphParams({ search, limit, offset }) {
  const params = new URLSearchParams();
  if (search) params.set('search', search);
  if (limit !== undefined) params.set('limit', String(limit));
  if (offset) params.set('offset', String(offset));
  return params;
}

export function downloadURL(query) {
  return '/api/download?' + packagesQuery(query);
}
