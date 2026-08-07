import * as api from './api.js';
import * as icons from './icons.js';
import { el, errorState, loadingState, replace } from './dom.js';
import { createPalette } from './palette.js';
import { buildHash, createRouter } from './router.js';
import { currentTheme, toggleTheme, watchSystemTheme } from './theme.js';
import { createDetailPanel } from './views/detail.js';
import { createExploreView } from './views/explore.js';
import { createGraphView } from './views/graph.js';
import { createOverviewView } from './views/overview.js';

const TABS = [
  { name: 'overview', label: 'Overview' },
  { name: 'explore', label: 'Explore' },
  { name: 'graph', label: 'Graph' },
];

// The shortcut is ⌘K on Apple keyboards and Ctrl+K everywhere else, so the
// label has to match the keyboard in front of the reader.
const APPLE = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

function buildTopbar({ summary, router, palette }) {
  const tabs = TABS.map((tab) =>
    el(
      'button',
      {
        class: 'tab',
        type: 'button',
        dataset: { view: tab.name },
        on: { click: () => router.go(tab.name, null, { replace: false }) },
      },
      tab.label,
    ),
  );

  const themeButton = el('button', { class: 'icon-button', type: 'button', on: { click: onToggle } });

  function paintThemeButton(theme) {
    // Shows the destination rather than the current state, so the control
    // reads as "go here" instead of asking you to decode which one you are on.
    const goingDark = theme === 'light';
    replace(themeButton, goingDark ? icons.moon() : icons.sun());
    themeButton.title = goingDark ? 'Switch to dark theme' : 'Switch to light theme';
    themeButton.setAttribute('aria-label', themeButton.title);
  }

  function onToggle() {
    paintThemeButton(toggleTheme());
  }

  paintThemeButton(currentTheme());
  watchSystemTheme(paintThemeButton);

  const element = el(
    'header',
    { class: 'topbar' },
    el(
      'div',
      { class: 'topbar__inner' },
      el(
        'a',
        { class: 'brand', href: buildHash(TABS[0].name, {}), title: 'Back to overview' },
        el('span', { class: 'brand__mark' }),
        'Blast radius',
      ),
      el('nav', { class: 'tabs', 'aria-label': 'Views' }, ...tabs),
      el('span', { class: 'spacer' }),
      el(
        'button',
        {
          class: 'omnibox',
          type: 'button',
          title: 'Search packages',
          on: { click: () => palette.open() },
        },
        icons.search(),
        el('span', { class: 'omnibox__label' }, 'Search packages'),
        el('kbd', { class: 'kbd' }, APPLE ? '⌘K' : 'Ctrl K'),
      ),
      themeButton,
    ),
  );

  return {
    element,
    setActiveTab(name) {
      for (const tab of tabs) {
        if (tab.dataset.view === name) tab.setAttribute('aria-current', 'page');
        else tab.removeAttribute('aria-current');
      }
    },
  };
}

function bindShortcuts(getActiveView, palette) {
  document.addEventListener('keydown', (event) => {
    if ((event.key === 'k' || event.key === 'K') && (event.metaKey || event.ctrlKey) && !event.altKey) {
      event.preventDefault();
      palette.toggle();
      return;
    }
    if (event.key !== '/' || event.metaKey || event.ctrlKey || event.altKey) return;
    const target = event.target;
    if (target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement || target instanceof HTMLSelectElement) {
      return;
    }
    const view = getActiveView();
    if (!view?.focusSearch) return;
    event.preventDefault();
    view.focusSearch();
  });
}

async function boot() {
  const root = document.getElementById('app');
  replace(root, loadingState('Loading report…'));

  let summary;
  try {
    summary = await api.summary();
  } catch (error) {
    replace(root, errorState(error));
    return;
  }

  const viewport = el('main', { class: 'view', id: 'view' });
  const detail = createDetailPanel({
    maxDepth: summary.max_depth,
    onTrace: (name) => {
      detail.close();
      router.go('graph', new URLSearchParams({ package: name }), { replace: false });
    },
  });
  const views = {};
  let active = null;

  const router = createRouter({
    views: { overview: true, explore: true, graph: true },
    fallback: 'overview',
    onNavigate: ({ name, params }, changedView) => {
      topbar.setActiveTab(name);
      if (changedView) {
        detail.close();
        replace(viewport, views[name].element);
        active = views[name];
      }
      views[name].sync(params);
    },
  });

  const palette = createPalette({ summary, router, detail });
  const topbar = buildTopbar({ summary, router, palette });

  views.overview = createOverviewView({ summary, router });
  views.explore = createExploreView({ summary, router, detail });
  views.graph = createGraphView({ router, detail });

  replace(root, topbar.element, viewport);
  bindShortcuts(() => active, palette);
  router.start();
}

boot();
