/**
 * Command palette (⌘K / Ctrl+K).
 *
 * One box that takes a package name and offers the handful of things this
 * report can answer about it: what it looks like, how it reaches a compromised
 * package, and which slice of the table it belongs to. Matching packages come
 * from the same endpoint the Explore table uses, so what the palette lists is
 * exactly what the report holds.
 *
 * Rows are a flat list under section headings. The keyboard walks the flat
 * list, which is why headings are not rows and never take selection.
 */

import * as api from './api.js';
import * as icons from './icons.js';
import { debounce, el, replace } from './dom.js';
import { downloads, npmURL, plural, targetName } from './format.js';

const MATCH_LIMIT = 6;

const NAV = [
  { view: 'overview', title: 'Overview', meta: 'Report summary', icon: icons.chart },
  { view: 'explore', title: 'Explore', meta: 'Filterable table of affected packages', icon: icons.rows },
  { view: 'graph', title: 'Graph', meta: 'Routes to the compromised package', icon: icons.nodes },
];

export function createPalette({ summary, router, detail }) {
  const targets = summary.targets || [];
  const preferDownloads = Boolean(summary.enriched);

  let scrim = null;
  let panel = null;
  let listbox = null;
  let footerHint = null;
  let input = null;
  let returnFocus = null;

  let items = [];
  let selected = 0;
  let request = null;

  // Guards against a slow response for a query the user has already typed past.
  let generation = 0;

  function isOpen() {
    return Boolean(panel);
  }

  function close() {
    if (!panel) return;
    request?.abort();
    request = null;
    generation++;
    scrim.remove();
    panel.remove();
    scrim = null;
    panel = null;
    document.removeEventListener('keydown', onKeydown, true);
    returnFocus?.focus();
    returnFocus = null;
  }

  function open(initial = '') {
    if (panel) {
      input.select();
      return;
    }
    returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    mount();
    input.value = initial;
    render(initial ? [] : navSections(), initial);
    if (initial) search(initial);
    input.focus();
    input.select();
  }

  function mount() {
    input = el('input', {
      class: 'palette__input',
      type: 'text',
      placeholder: 'Search for a package…',
      autocomplete: 'off',
      spellcheck: false,
      'aria-label': 'Search packages',
      'aria-autocomplete': 'list',
      on: { input: onInput },
    });

    listbox = el('div', { class: 'palette__list', role: 'listbox', 'aria-label': 'Results' });
    footerHint = el('span', { class: 'palette__footer-end' });

    const footer = el(
      'div',
      { class: 'palette__footer' },
      el('span', { class: 'palette__keyhint' }, 'Navigate ', key('↓'), key('↑')),
      el('span', { class: 'palette__keyhint' }, 'Open ', key('↵')),
      el('span', { class: 'palette__keyhint' }, 'Close ', key('esc')),
      footerHint,
    );

    panel = el(
      'div',
      { class: 'palette', role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Search' },
      el('div', { class: 'palette__head' }, el('span', { class: 'palette__head-icon' }, icons.search()), input),
      listbox,
      footer,
    );

    scrim = el('button', { class: 'scrim scrim--palette', type: 'button', 'aria-label': 'Close search', on: { click: close } });

    document.body.append(scrim, panel);
    document.addEventListener('keydown', onKeydown, true);
  }

  const onInput = debounce((event) => {
    const term = event.target.value.trim();
    if (!term) {
      request?.abort();
      generation++;
      render(navSections(), '');
      return;
    }
    // Actions land immediately; matches fill in when the request returns, so the
    // palette is never blank while typing.
    render(sections(term, null), term);
    search(term);
  }, 140);

  async function search(term) {
    const mine = ++generation;
    request?.abort();
    request = new AbortController();
    try {
      const data = await api.packages(
        {
          search: term,
          limit: MATCH_LIMIT,
          sort: preferDownloads ? 'weekly_downloads' : 'depth',
          dir: preferDownloads ? 'desc' : 'asc',
        },
        request.signal,
      );
      if (mine !== generation) return;
      render(sections(term, data), term);
    } catch (error) {
      if (error.name === 'AbortError' || mine !== generation) return;
      render(sections(term, { results: [], total: 0, error }), term);
    }
  }

  // ---------------------------------------------------------------- sections

  function navSections() {
    return [
      {
        label: 'Jump to',
        items: NAV.map((entry) => ({
          icon: entry.icon,
          title: entry.title,
          meta: entry.meta,
          hint: '/' + entry.view,
          run: () => router.go(entry.view, null, { replace: false }),
        })),
      },
    ];
  }

  function sections(term, data) {
    const matches = data?.results || [];
    const exact = matches.some((pkg) => pkg.name === term);
    const target = targets.find((t) => t.ref === term || t.name === term || targetName(t.ref) === term);

    const out = [];

    out.push({
      label: 'Packages',
      items: data
        ? matches.map(packageItem)
        : [{ icon: icons.search, title: 'Searching…', muted: true }],
      empty: data && matches.length === 0 ? (data.error ? String(data.error.message || data.error) : 'No package matches') : null,
    });

    const actions = [
      {
        icon: icons.rows,
        title: 'Search in Explore',
        quoted: term,
        meta: 'Packages, intermediate dependencies and targets',
        hint: '/explore',
        run: () => router.go('explore', new URLSearchParams({ search: term }), { replace: false }),
      },
      {
        icon: icons.nodes,
        title: 'Trace in Graph',
        quoted: term,
        meta: exact ? 'Expand every route this package has' : 'Start the graph from a matching package',
        hint: '/graph',
        run: () =>
          router.go('graph', new URLSearchParams(exact ? { package: term } : { search: term }), { replace: false }),
      },
    ];

    if (target) {
      actions.push({
        icon: icons.target,
        title: 'Packages reaching',
        quoted: target.ref,
        meta: plural(target.attributed_packages, 'affected package'),
        hint: '/explore',
        run: () => router.go('explore', new URLSearchParams({ target: target.ref }), { replace: false }),
      });
    }

    actions.push({
      icon: icons.external,
      title: 'Open on npm',
      quoted: term,
      meta: 'npmjs.com',
      hint: 'new tab',
      run: () => window.open(npmURL(term), '_blank', 'noreferrer'),
    });

    out.push({ label: 'Search options', items: actions });
    return out;
  }

  function packageItem(pkg) {
    const meta = [
      plural(pkg.min_depth, 'hop'),
      plural(pkg.version_count, 'version'),
      pkg.weekly_downloads === null || pkg.weekly_downloads === undefined
        ? null
        : downloads(pkg.weekly_downloads) + ' downloads/wk',
    ]
      .filter(Boolean)
      .join(' · ');

    return {
      icon: icons.box,
      title: pkg.name,
      mono: true,
      meta,
      hint: 'Details',
      altHint: 'trace in graph',
      run: () => detail.open(pkg.name),
      altRun: () => router.go('graph', new URLSearchParams({ package: pkg.name }), { replace: false }),
    };
  }

  // ------------------------------------------------------------------ render

  function render(list, term) {
    items = [];
    const nodes = [];

    for (const section of list) {
      nodes.push(el('div', { class: 'palette__group' }, section.label));
      if (section.empty) {
        nodes.push(el('div', { class: 'palette__empty' }, section.empty, term ? el('span', { class: 'palette__quoted' }, term) : null));
        continue;
      }
      for (const item of section.items) {
        const row = renderRow(item);
        nodes.push(row);
        if (!item.muted) {
          item.row = row;
          items.push(item);
        }
      }
    }

    replace(listbox, ...nodes);
    selectIndex(0, { scroll: false });
  }

  function renderRow(item) {
    const row = el(
      'div',
      {
        class: 'palette__row' + (item.muted ? ' palette__row--muted' : ''),
        role: item.muted ? null : 'option',
        'aria-selected': item.muted ? null : 'false',
      },
      el('span', { class: 'palette__icon' }, item.icon()),
      el(
        'span',
        { class: 'palette__label' },
        el('span', { class: 'palette__title' + (item.mono ? ' mono' : '') }, item.title),
        item.quoted ? el('span', { class: 'palette__quoted' }, item.quoted) : null,
        item.meta ? el('span', { class: 'palette__meta' }, item.meta) : null,
      ),
      item.hint ? el('span', { class: 'palette__hint' }, item.hint) : null,
    );

    if (item.muted) return row;
    row.addEventListener('click', () => activate(item));
    row.addEventListener('mousemove', () => {
      const index = items.indexOf(item);
      if (index !== -1 && index !== selected) selectIndex(index, { scroll: false });
    });
    return row;
  }

  function selectIndex(index, { scroll = true } = {}) {
    if (items.length === 0) {
      selected = 0;
      footerHint.textContent = '';
      return;
    }
    selected = (index + items.length) % items.length;
    items.forEach((item, i) => item.row.setAttribute('aria-selected', String(i === selected)));
    const current = items[selected];
    if (scroll) current.row.scrollIntoView({ block: 'nearest' });
    // Only some rows have a second action, so the shift hint appears with them
    // rather than sitting in the footer as a permanent claim.
    replace(footerHint, current.altHint ? el('span', { class: 'palette__keyhint' }, key('⇧↵'), ' ' + current.altHint) : null);
  }

  function activate(item, { alt = false } = {}) {
    const run = alt && item.altRun ? item.altRun : item.run;
    if (!run) return;
    close();
    run();
  }

  function onKeydown(event) {
    if (!panel) return;
    // Captured before anything else on the page, and swallowed: a drawer may be
    // open underneath, and Escape means "close the palette", not both.
    switch (event.key) {
      case 'Escape':
        event.preventDefault();
        event.stopPropagation();
        close();
        return;
      case 'ArrowDown':
        event.preventDefault();
        event.stopPropagation();
        selectIndex(selected + 1);
        return;
      case 'ArrowUp':
        event.preventDefault();
        event.stopPropagation();
        selectIndex(selected - 1);
        return;
      case 'Enter': {
        const item = items[selected];
        if (!item) return;
        event.preventDefault();
        event.stopPropagation();
        activate(item, { alt: event.shiftKey });
        return;
      }
      case 'Tab':
        // The palette is the whole interaction while it is up; tabbing out of it
        // would leave a modal box nobody can see they are inside.
        event.preventDefault();
        event.stopPropagation();
        selectIndex(selected + (event.shiftKey ? -1 : 1));
        return;
      default:
    }
  }

  return { open, close, isOpen, toggle: (initial) => (isOpen() ? close() : open(initial)) };
}

function key(label) {
  return el('kbd', { class: 'kbd' }, label);
}
