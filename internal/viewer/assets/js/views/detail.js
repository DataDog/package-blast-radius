import * as api from '../api.js';
import * as icons from '../icons.js';
import { el, errorState, loadingState, replace } from '../dom.js';
import { count, depthStop, downloads, npmURL, plural } from '../format.js';

const VERSIONS_PER_PAGE = 60;

// One package in the keyv fixture records 242 routes. Showing them all at once
// buries the trace, which is what the panel is for, so the rest stay one click
// away.
const ROUTES_BEFORE_FOLD = 8;

/**
 * The path trace.
 *
 * This is the artifact people open the viewer for: proof that the reach is
 * real, in a form you can paste into a ticket. One chain, top to bottom, from
 * the package you clicked down to the compromised one. Every node carries the
 * exact version that was recorded, and every edge carries the range that node
 * declares, which is the line someone eventually has to go and change.
 *
 * Steps run [package, ...intermediate hops]; the target is not among them, so
 * it is appended here. steps[i].requirement is what steps[i] declares for the
 * node below it, and the last one constrains the target.
 */
function trace(steps, targetRef, maxDepth) {
  const nodes = steps.map((step, i) =>
    el(
      'div',
      { class: 'trace__step', dataset: { depth: depthStop(steps.length - i, maxDepth) } },
      el('div', { class: 'trace__pkg' }, step.package),
      el('div', { class: 'trace__version' }, step.version),
      step.requirement
        ? el('div', { class: 'trace__req' }, 'requires ', el('code', {}, step.requirement))
        : null,
    ),
  );

  nodes.push(
    el(
      'div',
      { class: 'trace__step trace__step--target' },
      el('div', { class: 'trace__pkg' }, targetRef),
      el('div', { class: 'trace__version muted' }, 'compromised'),
    ),
  );

  return el('div', { class: 'trace' }, ...nodes);
}

function routeLabel(route) {
  if (!route.hops || route.hops.length === 0) return 'Direct dependency';
  return 'via ' + route.hops.join(' → ');
}

export function createDetailPanel({ maxDepth, onTrace }) {
  let scrim = null;
  let drawer = null;
  let returnFocus = null;
  let request = null;

  // Identifies the in-flight open, so a slow response for a package the user
  // already navigated away from does not paint over the current one.
  let openToken = 0;

  function close() {
    if (!drawer) return;
    request?.abort();
    request = null;
    scrim.remove();
    drawer.remove();
    scrim = null;
    drawer = null;
    document.removeEventListener('keydown', onKeydown);
    returnFocus?.focus();
    returnFocus = null;
  }

  function onKeydown(event) {
    if (event.key === 'Escape') {
      event.preventDefault();
      close();
    }
  }

  function mount(name) {
    const body = el('div', { class: 'drawer__body' }, loadingState('Loading routes…'));
    const title = el('h2', { class: 'drawer__title' }, name);

    const closeButton = el(
      'button',
      { class: 'icon-button', type: 'button', title: 'Close (Esc)', on: { click: close } },
      icons.close(),
    );

    // The drawer proves one route at a time; the graph shows every route this
    // package has at once, which is the view that answers "do my versions all
    // go the same way".
    const traceButton = onTrace
      ? el(
          'button',
          { class: 'button', type: 'button', on: { click: () => onTrace(name) } },
          'View routes as a graph',
        )
      : null;

    const head = el(
      'div',
      { class: 'drawer__head' },
      title,
      traceButton,
      el(
        'a',
        { class: 'icon-button', href: npmURL(name), target: '_blank', rel: 'noreferrer', title: 'Open on npm' },
        icons.external(),
      ),
      closeButton,
    );

    scrim = el('button', { class: 'scrim', type: 'button', 'aria-label': 'Close panel', on: { click: close } });
    drawer = el('aside', { class: 'drawer', role: 'dialog', 'aria-modal': 'true', 'aria-label': name }, head, body);

    document.body.append(scrim, drawer);
    document.addEventListener('keydown', onKeydown);
    return { body, head, closeButton };
  }

  function renderDetail(pkg, body, head, selectedRouteID) {
    const route = pkg.routes.find((r) => r.id === selectedRouteID) || pkg.routes[0];

    const subtitle = el(
      'div',
      { class: 'drawer__sub' },
      [
        plural(pkg.version_count, 'recorded version'),
        plural(pkg.min_depth, 'hop') + ' at the shortest',
        pkg.weekly_downloads === null ? null : downloads(pkg.weekly_downloads) + ' weekly downloads',
      ]
        .filter(Boolean)
        .join(' · '),
    );
    replace(head.querySelector('.drawer__title'), pkg.name, subtitle);

    const sections = [];

    if (pkg.routes_total > 1) {
      const tab = (r) =>
        el(
          'button',
          {
            class: 'route-tab',
            type: 'button',
            title: routeLabel(r),
            'aria-pressed': String(r.id === route.id),
            on: { click: () => load(pkg.name, r.id, body, head) },
          },
          el('span', { class: 'route-tab__name' }, routeLabel(r)),
          el('span', { class: 'route-tab__meta' }, `${plural(r.depth, 'hop')} · ${plural(r.version_count, 'version')}`),
        );

      // The selected route always stays visible, even when the fold would
      // otherwise hide it.
      const folded = pkg.routes.slice(0, ROUTES_BEFORE_FOLD);
      if (!folded.includes(route)) folded[ROUTES_BEFORE_FOLD - 1] = route;
      const hidden = pkg.routes.length - folded.length;

      const list = el('div', { class: 'routes' }, ...folded.map(tab));
      const expand = el(
        'button',
        {
          class: 'button',
          type: 'button',
          on: {
            click: () => {
              replace(list, ...pkg.routes.map(tab));
              expand.remove();
            },
          },
        },
        `Show all ${count(pkg.routes.length)} routes`,
      );

      sections.push(
        el(
          'section',
          { class: 'drawer__section' },
          el(
            'div',
            { class: 'drawer__section-head' },
            el('h3', { class: 'eyebrow' }, plural(pkg.routes_total, 'recorded route')),
          ),
          list,
          hidden > 0 ? expand : null,
        ),
      );
    }

    const versions = route?.versions || [];
    if (versions.length) {
      const traceHost = el('div', {}, trace(versions[0].steps, route.target, maxDepth));

      const chips = el(
        'div',
        { class: 'version-picker' },
        ...versions.map((v, i) =>
          el(
            'button',
            {
              class: 'version-chip',
              type: 'button',
              'aria-pressed': String(i === 0),
              on: {
                click: (event) => {
                  for (const chip of chips.children) chip.setAttribute('aria-pressed', 'false');
                  event.currentTarget.setAttribute('aria-pressed', 'true');
                  replace(traceHost, trace(v.steps, route.target, maxDepth));
                },
              },
            },
            v.version,
          ),
        ),
      );

      // Both offset and total carry omitempty, so an absent field is a zero.
      const total = route.versions_total || versions.length;
      const shown = Math.min((route.versions_offset || 0) + versions.length, total);

      // A single version is not a choice, and the trace already names it.
      if (versions.length > 1) {
        sections.push(
          el(
            'section',
            { class: 'drawer__section' },
            el(
              'div',
              { class: 'drawer__section-head' },
              el('h3', { class: 'eyebrow' }, 'Package versions on this route'),
              total > shown
                ? el('span', { class: 'muted', style: 'font-size: var(--text-sm)' }, `showing ${count(shown)} of ${count(total)}`)
                : null,
            ),
            chips,
          ),
        );
      }

      sections.push(
        el(
          'section',
          { class: 'drawer__section' },
          el(
            'div',
            { class: 'drawer__section-head' },
            el('h3', { class: 'eyebrow' }, 'Path to the compromised package'),
          ),
          traceHost,
        ),
      );
    }

    sections.push(
      el(
        'p',
        { class: 'caveat' },
        'Each package version carries the one route the traversal recorded for it. Other routes to the same package may exist and are not shown.',
      ),
    );

    replace(body, ...sections);
    body.scrollTop = 0;
  }

  async function load(name, routeID, body, head) {
    const token = ++openToken;
    request?.abort();
    request = new AbortController();
    replace(body, loadingState('Loading routes…'));
    try {
      const pkg = await api.packageDetail(
        name,
        { route: routeID, versionsLimit: VERSIONS_PER_PAGE },
        request.signal,
      );
      if (token !== openToken) return;
      renderDetail(pkg, body, head, routeID);
    } catch (error) {
      if (error.name === 'AbortError' || token !== openToken) return;
      replace(body, errorState(error));
    }
  }

  return {
    open(name, trigger) {
      close();
      returnFocus = trigger || null;
      const { body, head, closeButton } = mount(name);
      closeButton.focus();
      load(name, undefined, body, head);
    },
    close,
  };
}
