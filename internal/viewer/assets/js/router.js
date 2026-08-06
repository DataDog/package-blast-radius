/**
 * Hash router.
 *
 * Location looks like `#/explore?search=axios&depth=1`. The hash carries the
 * query rather than the real one so the whole address, view and filter state
 * together, survives a copy-paste without the server needing rewrite rules.
 */

export function parseHash(hash = window.location.hash) {
  const raw = hash.replace(/^#\/?/, '');
  const split = raw.indexOf('?');
  const name = split === -1 ? raw : raw.slice(0, split);
  const params = new URLSearchParams(split === -1 ? '' : raw.slice(split + 1));
  return { name, params };
}

export function buildHash(name, params) {
  const query = params instanceof URLSearchParams ? params.toString() : new URLSearchParams(params || {}).toString();
  return '#/' + name + (query ? '?' + query : '');
}

export function createRouter({ views, fallback, onNavigate }) {
  let current = null;

  // Last query each view was seen with, so returning to a tab restores the
  // filters you left it on rather than resetting it.
  const remembered = {};

  function route() {
    const { name, params } = parseHash();
    return { name: views[name] ? name : fallback, params };
  }

  function dispatch() {
    const next = route();
    const changedView = next.name !== current;
    current = next.name;
    remembered[next.name] = next.params;
    onNavigate(next, changedView);
  }

  return {
    start() {
      window.addEventListener('hashchange', dispatch);
      dispatch();
    },
    route,
    /**
     * Replaces by default: filter changes should not each become a history
     * entry. A null `params` means "wherever that view was", which is what a
     * tab click wants.
     */
    go(name, params, { replace = true } = {}) {
      const hash = buildHash(name, params ?? remembered[name]);
      if (hash === window.location.hash) return;
      if (replace) {
        window.history.replaceState(null, '', hash);
        dispatch();
      } else {
        window.location.hash = hash;
      }
    },
  };
}
