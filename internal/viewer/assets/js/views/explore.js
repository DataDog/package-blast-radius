import * as api from '../api.js';
import * as icons from '../icons.js';
import { count, depthStop, downloads, plural } from '../format.js';
import { debounce, el, emptyState, errorState, loadingState, replace } from '../dom.js';

const PAGE_SIZES = [50, 100, 250, 500];
const DEFAULT_PAGE_SIZE = 100;

// firstDir is the direction a column sorts in when it is picked: alphabetical
// and shortest-route-first read naturally, counts read largest-first.
// Widths are fixed rather than content-derived. A 15-hop route is thousands of
// pixels of unbreakable text, and left to itself the browser starves every
// other column to make room for it.
const COLUMNS = [
  { label: 'Package', sort: 'name', firstDir: 'asc', width: '26%' },
  { label: 'Versions', sort: 'version_count', firstDir: 'desc', numeric: true, width: '13%' },
  { label: 'Distance', sort: 'depth', firstDir: 'asc', width: '10%' },
  { label: 'Downloads/wk', sort: 'weekly_downloads', firstDir: 'desc', numeric: true, needsEnrichment: true, width: '11%' },
  { label: 'Route to the compromised package', sort: 'routes', firstDir: 'desc' },
];

// Past this, a route in a table cell is a wall of text nobody reads. The middle
// collapses to a count and the drawer holds the full chain.
const HOPS_BEFORE_ELISION = 3;

export function createExploreView({ summary, router, detail }) {
  const maxDepth = Math.max(1, summary.max_depth || 1);
  const defaultSort = summary.enriched ? 'weekly_downloads' : 'depth';
  const defaultDir = summary.enriched ? 'desc' : 'asc';
  const columns = COLUMNS.filter((c) => summary.enriched || !c.needsEnrichment);

  const clampDepth = (n) => Math.min(Math.max(Math.round(n) || 1, 1), maxDepth);

  function readState(params) {
    const limit = Number(params.get('limit')) || DEFAULT_PAGE_SIZE;
    // A click on the overview histogram deep-links to one exact distance,
    // which the range expresses as a floor and ceiling that coincide.
    const exact = Number(params.get('depth')) || 0;
    const lo = exact || Number(params.get('minDepth')) || 1;
    const hi = exact || Number(params.get('maxDepth')) || maxDepth;
    return {
      search: params.get('search') || '',
      minDepth: clampDepth(Math.min(lo, hi)),
      maxDepth: clampDepth(Math.max(lo, hi)),
      target: params.get('target') || '',
      sort: params.get('sort') || defaultSort,
      dir: params.get('dir') === 'asc' ? 'asc' : params.get('dir') === 'desc' ? 'desc' : defaultDir,
      offset: Math.max(0, Number(params.get('offset')) || 0),
      limit: PAGE_SIZES.includes(limit) ? limit : DEFAULT_PAGE_SIZE,
    };
  }

  function writeState(s) {
    const params = new URLSearchParams();
    if (s.search) params.set('search', s.search);
    if (s.minDepth > 1) params.set('minDepth', String(s.minDepth));
    if (s.maxDepth < maxDepth) params.set('maxDepth', String(s.maxDepth));
    if (s.target) params.set('target', s.target);
    if (s.sort !== defaultSort) params.set('sort', s.sort);
    if (s.dir !== defaultDir) params.set('dir', s.dir);
    if (s.offset) params.set('offset', String(s.offset));
    if (s.limit !== DEFAULT_PAGE_SIZE) params.set('limit', String(s.limit));
    return params;
  }

  let state = readState(new URLSearchParams());
  let inFlight = null;
  let generation = 0;

  function navigate(changes, { resetPage = true } = {}) {
    state = { ...state, ...changes };
    if (resetPage && !('offset' in changes)) state.offset = 0;
    router.go('explore', writeState(state));
  }

  // ----------------------------------------------------------------- search

  const searchInput = el('input', {
    class: 'searchbar__input',
    type: 'search',
    placeholder: 'Search packages, intermediate dependencies, or targets',
    autocomplete: 'off',
    spellcheck: false,
    'aria-label': 'Search packages',
    on: { input: debounce((e) => navigate({ search: e.target.value }), 250) },
  });

  const searchHint = el('span', { class: 'searchbar__hint' });

  const clearButton = el(
    'button',
    {
      class: 'searchbar__clear is-hidden',
      type: 'button',
      'aria-label': 'Clear search',
      on: {
        click: () => {
          searchInput.value = '';
          navigate({ search: '' });
          searchInput.focus();
        },
      },
    },
    icons.close(),
  );

  const searchbar = el(
    'div',
    { class: 'searchbar' },
    el('span', { class: 'searchbar__icon' }, icons.search()),
    searchInput,
    searchHint,
    clearButton,
  );

  // ------------------------------------------------------------------ reach

  /*
   * A range instead of a checkbox per depth. Distance is ordered, and the
   * question people ask is "how close does this get", not "show me hops 1 and
   * 4 but not 2 and 3". Both ends move, so "at least 3 hops away" is as
   * expressible as "within 2", and the fully open range is a real position on
   * the track rather than a special case you have to know to reset.
   *
   * Two stacked native inputs rather than a custom widget: each thumb keeps
   * real keyboard and screen-reader behaviour. The track underneath is drawn
   * once and the inputs sit on top of it, transparent apart from their thumbs.
   */
  const reachValue = el('span', { class: 'reach__value' });
  const reachFill = el('span', { class: 'reach__fill' });

  function reachThumb(role, label) {
    return el('input', {
      class: 'reach__thumb reach__thumb--' + role,
      type: 'range',
      min: '1',
      max: String(maxDepth),
      step: '1',
      'aria-label': label,
      on: {
        input: () => paintReach(readReach()),
        change: () => navigate(readReach()),
      },
    });
  }

  const reachLow = reachThumb('low', 'Fewest hops to the compromised package');
  const reachHigh = reachThumb('high', 'Most hops to the compromised package');

  /*
   * The thumbs are independent inputs, so nothing stops the low one being
   * dragged past the high one. Rather than clamping (which makes a thumb feel
   * stuck against an invisible wall), the pair is read as an unordered set and
   * sorted, so dragging through the other end swaps their roles.
   */
  function readReach() {
    const a = clampDepth(Number(reachLow.value));
    const b = clampDepth(Number(reachHigh.value));
    return { minDepth: Math.min(a, b), maxDepth: Math.max(a, b) };
  }

  function reachLabel({ minDepth: lo, maxDepth: hi }) {
    if (lo === 1 && hi >= maxDepth) return 'any';
    if (lo === hi) return lo === 1 ? 'direct only' : `exactly ${lo}`;
    if (lo === 1) return '≤ ' + hi;
    if (hi >= maxDepth) return '≥ ' + lo;
    return `${lo}–${hi}`;
  }

  function paintReach(range) {
    reachValue.textContent = reachLabel(range);
    // maxDepth of 1 is filtered out below, so the span is never zero here.
    const pct = (n) => ((n - 1) / (maxDepth - 1)) * 100;
    reachFill.style.left = pct(range.minDepth) + '%';
    reachFill.style.right = 100 - pct(range.maxDepth) + '%';

    // Coincident thumbs hide one another. The high one wins by default, except
    // at the far right where it has nowhere left to go and would trap the low
    // one underneath it.
    const stuck = range.minDepth === range.maxDepth && range.maxDepth >= maxDepth;
    reachLow.style.zIndex = stuck ? '2' : '1';
    reachHigh.style.zIndex = stuck ? '1' : '2';
  }

  const reach = el(
    'div',
    { class: 'reach' },
    el('span', { class: 'reach__label' }, 'Hops'),
    el('span', { class: 'reach__track' }, reachFill, reachLow, reachHigh),
    reachValue,
  );

  // ----------------------------------------------------------------- target

  const attributedTargets = (summary.targets || []).filter((t) => t.attributed_packages > 0);

  const targetInput = el('input', {
    class: 'select',
    type: 'search',
    list: 'target-options',
    placeholder: 'Any compromised package',
    autocomplete: 'off',
    'aria-label': 'Filter by compromised package',
    on: { change: (e) => navigate({ target: e.target.value.trim() }) },
  });

  const targetFilter = el(
    'div',
    {},
    targetInput,
    el(
      'datalist',
      { id: 'target-options' },
      ...attributedTargets.slice(0, 500).map((t) => el('option', { value: t.ref })),
    ),
  );

  const filters = el(
    'div',
    { class: 'filters' },
    searchbar,
    maxDepth > 1 ? reach : null,
    attributedTargets.length > 1 ? targetFilter : null,
  );

  // ------------------------------------------------------------------ table

  const headerCells = columns.map((column) => {
    const caret = el('span', { class: 'th-sort__caret', 'aria-hidden': 'true' });
    const button = el(
      'button',
      {
        class: 'th-sort',
        type: 'button',
        on: {
          click: () => {
            const active = state.sort === column.sort;
            navigate({
              sort: column.sort,
              dir: active ? (state.dir === 'asc' ? 'desc' : 'asc') : column.firstDir,
            });
          },
        },
      },
      column.label,
      caret,
    );
    return { column, button, caret, th: el('th', { class: column.numeric ? 'col-num' : null }, button) };
  });

  const tbody = el('tbody');
  const tableCard = el(
    'div',
    { class: 'table-card' },
    el(
      'table',
      { class: 'data' },
      el('colgroup', {}, ...columns.map((c) => el('col', { width: c.width || null }))),
      el('thead', {}, el('tr', {}, ...headerCells.map((c) => c.th))),
      tbody,
    ),
  );

  const resultsInfo = el('span');
  const downloadLink = el('a', { class: 'button', href: '#', download: '' }, icons.download(), 'Export CSV');

  const pageSizeSelect = el(
    'select',
    {
      class: 'select',
      'aria-label': 'Rows per page',
      on: { change: (e) => navigate({ limit: Number(e.target.value) }) },
    },
    ...PAGE_SIZES.map((size) => el('option', { value: String(size) }, size + ' / page')),
  );

  const toolbar = el('div', { class: 'toolbar' }, resultsInfo, el('div', { class: 'toolbar__end' }, pageSizeSelect, downloadLink));

  const prevButton = el(
    'button',
    {
      class: 'button',
      type: 'button',
      on: { click: () => navigate({ offset: Math.max(0, state.offset - state.limit) }, { resetPage: false }) },
    },
    'Previous',
  );

  const nextButton = el(
    'button',
    {
      class: 'button',
      type: 'button',
      on: { click: () => navigate({ offset: state.offset + state.limit }, { resetPage: false }) },
    },
    'Next',
  );

  const pageInfo = el('span');
  const pager = el('div', { class: 'pager' }, prevButton, nextButton, pageInfo);
  const results = el('div', {}, tableCard, pager);

  const element = el('div', { class: 'page' }, filters, toolbar, results);

  // ----------------------------------------------------------------- render

  function openDetail(name, row) {
    for (const other of tbody.children) other.setAttribute('aria-selected', String(other === row));
    detail.open(name, row);
  }

  function renderRow(pkg) {
    const extraVersions = pkg.version_count - 1;
    const distance =
      pkg.min_depth === pkg.max_depth ? plural(pkg.min_depth, 'hop') : `${pkg.min_depth}–${pkg.max_depth} hops`;

    const cells = [
      el('td', { class: 'cell-package' }, pkg.name),
      el(
        'td',
        { class: 'col-num' },
        el(
          'span',
          { class: 'versions' },
          el('span', {}, pkg.latest_recorded_version),
          extraVersions > 0
            ? el(
                'span',
                {
                  class: 'versions__more',
                  title: `${count(pkg.version_count)} recorded versions, oldest ${pkg.oldest_recorded_version}`,
                },
                '+' + count(extraVersions),
              )
            : null,
        ),
      ),
      el(
        'td',
        {},
        el(
          'span',
          { class: 'distance', title: `Shortest recorded route: ${plural(pkg.min_depth, 'hop')}` },
          el('span', { class: 'distance__tick', dataset: { depth: depthStop(pkg.min_depth, maxDepth) } }),
          distance,
        ),
      ),
      summary.enriched ? el('td', { class: 'cell-num' }, downloads(pkg.weekly_downloads)) : null,
      el('td', {}, routePreview(pkg)),
    ].filter(Boolean);

    const row = el(
      'tr',
      {
        tabindex: '0',
        'aria-selected': 'false',
        title: 'Show the path to the compromised package',
      },
      ...cells,
    );
    row.addEventListener('click', () => openDetail(pkg.name, row));
    row.addEventListener('keydown', (event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault();
      openDetail(pkg.name, row);
    });
    return row;
  }

  /**
   * The first hop and the last hop are the two that carry information at a
   * glance: what this package pulls in, and what finally touches the
   * compromised one. Everything between them elides to a count.
   */
  function routePreview(pkg) {
    const route = pkg.routes && pkg.routes[0];
    if (!route) return el('span', { class: 'muted' }, '—');

    const hop = (name) => el('span', { class: 'path-cell__hop' }, name);
    const arrow = () => el('span', { class: 'path-cell__arrow' }, '→');

    const parts = [];
    if (route.hops.length <= HOPS_BEFORE_ELISION) {
      for (const name of route.hops) parts.push(hop(name), arrow());
    } else {
      const elided = route.hops.length - 2;
      parts.push(
        hop(route.hops[0]),
        arrow(),
        el('span', { class: 'path-cell__more' }, `${count(elided)} more`),
        arrow(),
        hop(route.hops[route.hops.length - 1]),
        arrow(),
      );
    }
    parts.push(el('span', { class: 'path-cell__target' }, route.target));

    const others = pkg.routes_total - 1;
    if (others > 0) parts.push(el('span', { class: 'path-cell__more' }, '+' + plural(others, 'route')));

    const full = [pkg.name, ...route.hops, route.target].join(' → ');
    return el('span', { class: 'path-cell', title: full }, ...parts);
  }

  function syncControls() {
    if (searchInput.value !== state.search) searchInput.value = state.search;
    clearButton.classList.toggle('is-hidden', state.search === '');

    if (maxDepth > 1) {
      reachLow.value = String(state.minDepth);
      reachHigh.value = String(state.maxDepth);
      paintReach(state);
    }

    if (targetInput.value !== state.target) targetInput.value = state.target;
    pageSizeSelect.value = String(state.limit);

    for (const { column, button, caret } of headerCells) {
      const active = state.sort === column.sort;
      button.setAttribute('aria-sort', active ? (state.dir === 'asc' ? 'ascending' : 'descending') : 'none');
      caret.textContent = active ? (state.dir === 'asc' ? '▲' : '▼') : '';
    }

    downloadLink.href = api.downloadURL(state);
  }

  async function load() {
    const mine = ++generation;
    inFlight?.abort();
    inFlight = new AbortController();

    try {
      const data = await api.packages(state, inFlight.signal);
      if (mine !== generation) return;
      renderResults(data);
    } catch (error) {
      if (error.name === 'AbortError' || mine !== generation) return;
      replace(results, errorState(error));
    }
  }

  function renderResults(data) {
    searchHint.textContent = state.search ? plural(data.total, 'match', 'matches') : '';

    if (data.results.length === 0) {
      replace(
        results,
        emptyState(
          'No packages match these filters',
          summary.unique_packages === 0
            ? 'This report has no affected packages.'
            : 'Try widening the hop range or clearing the search.',
        ),
      );
      resultsInfo.textContent = `0 of ${count(summary.unique_packages)} packages`;
      return;
    }

    replace(tbody, ...data.results.map(renderRow));
    replace(results, tableCard, pager);

    const first = data.offset + 1;
    const last = data.offset + data.results.length;
    resultsInfo.textContent = `${count(first)}–${count(last)} of ${plural(data.total, 'package')}`;

    const page = Math.floor(data.offset / state.limit) + 1;
    pageInfo.textContent = `Page ${count(page)} of ${count(Math.max(1, Math.ceil(data.total / state.limit)))}`;
    prevButton.disabled = data.offset === 0;
    nextButton.disabled = last >= data.total;
  }

  return {
    element,
    sync(params) {
      state = readState(params);
      syncControls();
      if (results.firstChild !== tableCard) replace(results, loadingState());
      load();
    },
    focusSearch() {
      searchInput.focus();
      searchInput.select();
    },
  };
}
