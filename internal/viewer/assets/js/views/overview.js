import { count, downloads, depthStop, plural } from '../format.js';
import { el } from '../dom.js';

const TOP_TARGETS = 10;

/**
 * The compromised package leads the page, because everything below it is a
 * consequence of that one fact. `summary.target` is unusable here: past three
 * targets the analyzer replaces it with a sentence rather than a reference.
 */
function hero(summary) {
  const targets = summary.targets || [];
  const names = new Set(targets.map((t) => t.name));

  let title = 'No compromised package';
  let mono = false;
  let sub = 'This report has no target, so nothing downstream was attributed.';

  if (targets.length === 1) {
    title = targets[0].ref;
    mono = true;
    sub = `${plural(summary.unique_packages, 'package')} depend on it, within ${plural(summary.max_depth, 'hop')}.`;
  } else if (names.size === 1) {
    title = targets[0].name;
    mono = true;
    sub = `${plural(targets.length, 'compromised version')}, reaching ${plural(summary.unique_packages, 'package')}.`;
  } else if (targets.length > 1) {
    title = plural(names.size, 'compromised package');
    sub = `${plural(targets.length, 'version')} in total, reaching ${plural(summary.unique_packages, 'package')}.`;
  }

  return el(
    'section',
    { class: 'incident' },
    el(
      'div',
      {},
      el(
        'div',
        { class: 'incident__label' },
        el('span', { class: 'tag' }, 'Compromised'),
        el('span', { class: 'eyebrow' }, summary.system),
      ),
      el('h1', { class: mono ? 'incident__name' : 'incident__name incident__name--prose' }, title),
      el('p', { class: 'incident__sub' }, sub),
    ),
  );
}

function stat(value, label, { muted = false, title } = {}) {
  return el(
    'div',
    { class: 'stat', title },
    el('div', { class: muted ? 'stat__value stat__value--muted' : 'stat__value' }, value),
    el('div', { class: 'stat__label' }, label),
  );
}

/**
 * Distance histogram. The bar carries the quantity and the ramp stop repeats
 * the distance, so the chart still reads with the colours stripped out.
 */
function depthBars(summary, onPick) {
  const counts = summary.depth_counts || {};
  const depths = Object.keys(counts)
    .map(Number)
    .sort((a, b) => a - b);
  const largest = Math.max(...depths.map((d) => counts[d]), 1);

  const rows = depths.map((depth) => {
    const value = counts[depth];
    const label = depth === 1 ? 'Direct' : `${depth} hops`;
    return el(
      'button',
      {
        class: 'bars-link',
        type: 'button',
        title: `Show the ${count(value)} packages whose shortest route is ${plural(depth, 'hop')}`,
        on: { click: () => onPick(depth) },
      },
      el('span', { class: 'bar-row__label' }, label),
      el(
        'span',
        { class: 'bar-row__track' },
        el('span', {
          class: 'bar-row__fill',
          dataset: { depth: depthStop(depth, summary.max_depth) },
          style: `width: ${Math.max((value / largest) * 100, 1.5)}%`,
        }),
      ),
      el('span', { class: 'bar-row__value' }, count(value)),
    );
  });

  return el(
    'section',
    { class: 'panel' },
    el(
      'div',
      { class: 'panel__head' },
      el('h2', { class: 'eyebrow' }, 'Packages by shortest route'),
      el('span', { class: 'muted', style: 'font-size: var(--text-sm)' }, 'click to filter'),
    ),
    el('div', { class: 'bars' }, ...rows),
  );
}

function targetLeaderboard(summary, onPick) {
  const targets = summary.targets || [];
  const attributed = targets.filter((t) => t.attributed_packages > 0);
  const shown = attributed.slice(0, TOP_TARGETS);
  const alsoAttributed = attributed.length - shown.length;
  const unattributed = targets.length - attributed.length;

  const rows = shown.map((target) =>
    el(
      'button',
      {
        class: 'rank__row',
        type: 'button',
        title: `${target.ref} — ${plural(target.attributed_versions, 'attributed version')}`,
        on: { click: () => onPick(target.ref) },
      },
      el('span', { class: 'rank__name' }, target.ref),
      el('span', { class: 'rank__value' }, count(target.attributed_packages)),
    ),
  );

  return el(
    'section',
    { class: 'panel' },
    el(
      'div',
      { class: 'panel__head' },
      el('h2', { class: 'eyebrow' }, 'Reach per compromised package'),
      el('span', { class: 'muted', style: 'font-size: var(--text-sm)' }, 'click to filter'),
    ),
    el(
      'p',
      { class: 'panel__hint' },
      'How many affected packages each compromised version accounts for. A package is counted once, against the compromised version the traversal reached it from.',
    ),
    el('div', { class: 'rank' }, ...rows),
    alsoAttributed > 0 || unattributed > 0
      ? el(
          'p',
          { class: 'panel__note' },
          [
            alsoAttributed > 0 ? `${plural(alsoAttributed, 'more target')} with attributed packages` : null,
            unattributed > 0 ? `${plural(unattributed, 'target')} the traversal attributed nothing to` : null,
          ]
            .filter(Boolean)
            .join('. ') + '.',
        )
      : null,
  );
}

/*
 * Every number on this page is an attribution count over the routes one
 * traversal happened to record, which is a weaker claim than it looks. The
 * caveats stay one click away rather than on screen, but they are one click
 * away from the numbers themselves rather than buried in the README.
 */
const CAVEATS = [
  'Routes and targets are the ones the analyzer recorded, not every dependency path that exists.',
  'Each affected package version carries exactly one recorded path, because traversal dedupes globally on name and version.',
  'Target counts are attribution counts. A package attributed to one target is not proof that no other target reaches it.',
  'Version lists summarise recorded versions. They are a sparse set, not a continuous semver range.',
  'Combined weekly downloads sums per-package counts, so consumers shared between two affected packages are counted twice.',
  'The graph aggregates nodes by package name, so several compromised versions of one package share a single node.',
];

function about() {
  const body = el('div', { class: 'about__body', hidden: true },
    el('ul', { class: 'about__list' }, ...CAVEATS.map((line) => el('li', {}, line))));

  const toggle = el('button', {
    class: 'about__toggle',
    type: 'button',
    'aria-expanded': 'false',
    on: {
      click: () => {
        body.hidden = !body.hidden;
        toggle.setAttribute('aria-expanded', String(!body.hidden));
      },
    },
  }, 'About this report');

  return el('div', { class: 'about' }, toggle, body);
}

function runmeta(summary) {
  const item = (label, value) => el('span', { class: 'runmeta__item' }, label + ' ', el('strong', {}, value));
  return el(
    'footer',
    { class: 'runmeta' },
    item('Ecosystem', summary.system),
    item('Traversal depth', String(summary.max_depth)),
    item('Source', summary.source_path),
  );
}

export function createOverviewView({ summary, router }) {
  const filterExplore = (params) => router.go('explore', new URLSearchParams(params), { replace: false });

  // An unenriched report has no download data at all, so the tile is dropped
  // rather than kept as a placeholder saying so.
  const stats = el(
    'div',
    { class: 'stat-row' },
    stat(count(summary.unique_packages), 'Affected packages'),
    stat(count(summary.total_affected), 'Affected versions'),
    stat(count(summary.direct_dependents), 'Direct dependents'),
    summary.enriched
      ? stat(downloads(summary.combined_weekly_downloads), 'Combined weekly downloads', {
          title: 'Sums per-package counts, so consumers shared between packages are counted more than once.',
        })
      : null,
  );

  // Each panel earns its place. One depth or one target makes for a card
  // holding a single row, which says less than the stat tiles already did.
  const panels = [
    Object.keys(summary.depth_counts || {}).length > 1
      ? depthBars(summary, (depth) => filterExplore({ depth: String(depth) }))
      : null,
    (summary.targets || []).length > 1 ? targetLeaderboard(summary, (ref) => filterExplore({ target: ref })) : null,
  ].filter(Boolean);

  const breakdown = panels.length
    ? el('div', { class: panels.length === 1 ? 'split split--single' : 'split' }, ...panels)
    : null;

  const element = el('div', { class: 'page' }, hero(summary), stats, breakdown, about(), runmeta(summary));

  return { element, sync() {} };
}
