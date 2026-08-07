import * as api from '../api.js';
import { count, downloads, depthStop, percent, plural } from '../format.js';
import { el, errorState, loadingState, replace } from '../dom.js';
import { paretoChart } from '../charts/pareto.js';
import { createCopyButton, token } from '../charts/copy-image.js';

const TOP_TARGETS = 10;
const TOP_SCOPES = 12;

// Past this many rows the expanded list scrolls inside the panel rather than
// running the page down: a report can name hundreds of scopes.
const SCOPES_BEFORE_SCROLL = 18;

const UNITS = [
  { key: 'versions', label: 'Package versions', noun: 'package versions', thing: 'version' },
  { key: 'packages', label: 'Affected packages', noun: 'affected packages', thing: 'package' },
];

// A worm can republish dozens of versions of one package. Past this many the
// list stops being something you read at a glance, and the rest wait behind a
// click.
const VERSIONS_SHOWN = 12;

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
  let versions = null;

  if (names.size === 1) {
    // The name is the headline and the versions are listed under it, rather
    // than one "name@version" reference that can only describe a single target.
    title = targets[0].name;
    mono = true;
    versions = versionList(targets);
    const pronoun = targets.length === 1 ? 'it' : 'them';
    sub = `${plural(summary.unique_packages, 'package')} depend on ${pronoun}, within ${plural(summary.max_depth, 'hop')}.`;
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
      el('div', { class: 'incident__label' }, el('span', { class: 'eyebrow' }, summary.system)),
      el('h1', { class: mono ? 'incident__name' : 'incident__name incident__name--prose' }, title),
      versions,
      el('p', { class: 'incident__sub' }, sub),
    ),
  );
}

/** Every compromised version of the one package, in version order. */
function versionList(targets) {
  const versions = [...new Set(targets.map((t) => t.version))].sort((a, b) =>
    a.localeCompare(b, undefined, { numeric: true }),
  );

  const list = el('div', { class: 'incident__versions' });
  const hidden = versions.length - VERSIONS_SHOWN;

  function paint(expanded) {
    const shown = expanded ? versions : versions.slice(0, VERSIONS_SHOWN);
    replace(
      list,
      ...shown.map((version) => el('span', { class: 'incident__version' }, version)),
      expanded || hidden <= 0
        ? null
        : el(
            'button',
            {
              class: 'incident__version incident__version--more',
              type: 'button',
              on: { click: () => paint(true) },
            },
            `+${count(hidden)} more`,
          ),
    );
  }

  paint(false);
  return list;
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
    { class: 'panel panel--fill' },
    el('div', { class: 'panel__head' }, el('h2', { class: 'eyebrow' }, 'Affected packages by shortest route')),
    el(
      'p',
      { class: 'panel__hint' },
      'How far each affected package sits from the compromised one it depends on, counted by its shortest chain.',
    ),
    el('div', { class: 'bars' }, ...rows),
  );
}

/**
 * The blast radius grouped by the scope each affected package publishes under:
 * which organisations this actually lands on, and how much of it each carries.
 *
 * Counted in packages rather than versions, to match the distance histogram
 * beside it: a scope with one package at forty affected versions is one package
 * its owner has to ship.
 *
 * Like the Pareto ranking, it needs a request of its own, because grouping means
 * walking every affected package. The panel renders a loading state first rather
 * than holding up first paint.
 */
function scopePanel(onPick) {
  const bars = el('div', { class: 'bars' }, loadingState('Grouping affected packages…'));
  const note = el('p', { class: 'panel__note' });
  const more = el('div', { class: 'panel__more' });

  const panel = el(
    'section',
    { class: 'panel panel--fill' },
    el('div', { class: 'panel__head' }, el('h2', { class: 'eyebrow' }, 'Affected packages by scope')),
    el(
      'p',
      { class: 'panel__hint' },
      'Where the blast radius lands. A scope carrying many affected packages is one owner with a lot to upgrade, ' +
        'and is the shortest route to whoever needs telling.',
    ),
    bars,
    more,
    note,
  );

  let expanded = false;

  function paint(response) {
    const scopes = response.scopes || [];
    const shown = expanded ? scopes : scopes.slice(0, TOP_SCOPES);
    const hidden = scopes.length - shown.length;

    // Bars stay comparable across an expansion: they are drawn against the
    // largest scope in the report rather than the largest one on screen, so
    // showing the tail does not rescale the head.
    const largest = Math.max(...scopes.map((s) => s.packages), 1);

    bars.className = shown.length > SCOPES_BEFORE_SCROLL ? 'bars bars--scroll' : 'bars';
    replace(bars, ...shown.map((entry) => scopeRow(entry, largest, onPick)));
    replace(note, ...scopeFootnote(response, hidden));

    if (scopes.length > TOP_SCOPES) {
      replace(
        more,
        el(
          'button',
          {
            class: 'about__toggle',
            type: 'button',
            'aria-expanded': String(expanded),
            on: {
              click: () => {
                expanded = !expanded;
                paint(response);
              },
            },
          },
          expanded ? 'Show the largest only' : `Show the ${count(scopes.length)} largest scopes`,
        ),
      );
    }
  }

  api
    .scopes()
    .then((response) => {
      // Nothing to compare: a single scope, or a report that is all unscoped
      // packages, is a sentence rather than a chart.
      if ((response.scopes || []).length < 2) panel.hidden = true;
      else paint(response);
    })
    .catch((error) => replace(bars, errorState(error)));

  return panel;
}

function scopeRow(entry, largest, onPick) {
  const reach = entry.weekly_downloads === null ? '' : `, ${downloads(entry.weekly_downloads)} weekly downloads`;

  return el(
    'button',
    {
      class: 'bars-link',
      type: 'button',
      title: `Show the affected packages under ${entry.scope} — ${plural(entry.versions, 'version')}${reach}`,
      on: { click: () => onPick(entry.scope) },
    },
    el('span', { class: 'bar-row__label mono' }, entry.scope),
    el(
      'span',
      { class: 'bar-row__track' },
      el('span', {
        class: 'bar-row__fill bar-row__fill--flat',
        style: `width: ${Math.max((entry.packages / largest) * 100, 1.5)}%`,
      }),
    ),
    el('span', { class: 'bar-row__value' }, count(entry.packages)),
  );
}

/**
 * What the bars leave out, which on npm is most of the report: around half of the
 * registry publishes without a scope, and those packages have no owner to group
 * them under. Said plainly, because a chart of the scoped half reads as the whole
 * blast radius otherwise.
 */
function scopeFootnote(response, hidden) {
  const unscoped = response.unscoped.packages;
  const total = unscoped + response.scoped_packages;

  return [
    `${plural(response.total_scopes, 'scope')} in total`,
    hidden > 0 ? `, ${count(hidden)} not shown` : '',
    `, holding ${percent((response.scoped_packages / (total || 1)) * 100)} of the affected packages. `,
    unscoped > 0
      ? el(
          'span',
          {},
          `The other ${count(unscoped)} publish without a scope, so they group under no owner.`,
        )
      : null,
  ].filter(Boolean);
}

/**
 * The same ranking the chart draws, as rows that filter Explore.
 *
 * It sits inside the chart's panel rather than in one of its own: two panels over
 * one set of numbers read as two findings. Here the chart carries the shape and
 * these rows carry the drilling down, which is the one thing the chart does not
 * do. The counts stay visible because a bar under a percent is hard to read back
 * as a number.
 */
function targetFilterList(summary, onPick) {
  const attributed = (summary.targets || []).filter((t) => t.attributed_packages > 0);
  const shown = attributed.slice(0, TOP_TARGETS);
  if (shown.length < 2) return null;

  const rows = shown.map((target) =>
    el(
      'button',
      {
        class: 'rank__row',
        type: 'button',
        title: `Show the affected packages attributed to ${target.ref} — ${plural(
          target.attributed_versions,
          'version',
        )}`,
        on: { click: () => onPick(target.ref) },
      },
      el('span', { class: 'rank__name' }, target.ref),
      el('span', { class: 'rank__value' }, count(target.attributed_packages)),
    ),
  );

  return el(
    'div',
    { class: 'panel__section' },
    el(
      'div',
      { class: 'panel__subhead' },
      el('h3', { class: 'eyebrow' }, `Top ${shown.length} by affected packages`),
    ),
    el('div', { class: 'rank rank--grid' }, ...rows),
  );
}

function unitToggle(active, onPick) {
  return el(
    'div',
    { class: 'seg', role: 'group', 'aria-label': 'Count impact by' },
    ...UNITS.map((unit) =>
      el(
        'button',
        {
          class: 'seg__option',
          type: 'button',
          'aria-pressed': String(unit.key === active),
          on: { click: () => onPick(unit.key) },
        },
        unit.label,
      ),
    ),
  );
}

/**
 * How to read the chart in one line, ending on what the tail bar stands for. The
 * bars name a head, so the reader is owed the size of what they do not name.
 */
function paretoFootnote(series, unit, summary) {
  const total = series.total || 1;
  const rows = series.rows;
  const top = rows[0];
  const share = (n) => percent((n / total) * 100);
  const named = rows[rows.length - 1].cumulative_count;
  const unnamed = series.distinct_sources - rows.length;
  const idle = (summary.targets || []).length - series.distinct_sources;

  const sentences = [
    el('span', {}, ' alone accounts for ', el('strong', {}, share(top.count)), ` of ${unit.noun}. `),
    rows.length > 1
      ? el('span', {}, `The ${rows.length} named here account for ${share(named)} between them. `)
      : null,
    unnamed > 0
      ? el(
          'span',
          {},
          `The last bar is ${plural(unnamed, 'compromised version')} with a smaller share each, ` +
            `${share(total - named)} together. `,
        )
      : null,
    // The chart can only rank what something depends on, and in a wide incident
    // most compromised versions turn out to have no dependents at all. Left
    // unsaid, the chart reads as if the whole list were on it.
    idle > 0
      ? el('span', {}, `${plural(idle, 'other compromised version')} had nothing depending on ${idle === 1 ? 'it' : 'them'}.`)
      : null,
  ].filter(Boolean);

  return el('p', { class: 'panel__note' }, el('code', { class: 'mono' }, top.package), ...sentences);
}

/**
 * Which of the compromised packages account for the affected set.
 *
 * Unlike the rest of this page it needs a request of its own, because ranking
 * them means walking every recorded route. The panel therefore renders a loading
 * state first and fills itself in, rather than holding up first paint.
 */
function paretoPanel(summary, onPick) {
  // One compromised package accounts for everything by definition, and the hero
  // has already said so.
  if ((summary.targets || []).length < 2) return null;

  const controls = el('div', { class: 'panel__controls' });
  const body = el('div', {}, loadingState('Ranking compromised packages…'));

  const copy = createCopyButton({
    getSource: () => body.querySelector('.pareto__svg'),
    background: () => token('--surface-raised'),
    title: 'Copy this chart to the clipboard as an image',
  });

  const panel = el(
    'section',
    { class: 'panel panel--chart' },
    el(
      'div',
      { class: 'panel__head' },
      el('h2', { class: 'eyebrow' }, 'Which compromised packages account for the blast radius'),
      el('span', { class: 'spacer' }),
      copy,
    ),
    el(
      'p',
      { class: 'panel__hint' },
      'Every package version is attributed to the compromised package the traversal reached it from, so the bars ' +
        'divide the report between them and the running total ends at all of it. Counted as packages instead, one ' +
        'affected package can hold versions from several compromised packages: the bars can then add up to more than ' +
        'the whole, while the line, which counts each package once, still ends at it.',
    ),
    controls,
    body,
    targetFilterList(summary, onPick),
  );

  let ranked = null;
  let active = UNITS[0].key;

  function paint() {
    const unit = UNITS.find((u) => u.key === active);
    const series = ranked[unit.key];
    replace(controls, unitToggle(active, pick));
    replace(body, paretoChart({ series, unitLabel: unit.noun }), paretoFootnote(series, unit, summary));
  }

  function pick(next) {
    if (next === active) return;
    active = next;
    paint(); // Both units arrived together, so switching costs no request.
  }

  api
    .pareto()
    .then((response) => {
      ranked = response;
      // A single bar is the hero statistic drawn twice.
      if (response.versions.rows.length < 2) panel.hidden = true;
      else paint();
    })
    .catch((error) => replace(body, errorState(error)));

  return panel;
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
    stat(count(summary.total_affected), 'Package versions'),
    stat(count(summary.direct_dependents), 'Direct dependents'),
    summary.enriched
      ? stat(downloads(summary.combined_weekly_downloads), 'Combined weekly downloads', {
          title: 'Sums per-package counts, so consumers shared between packages are counted more than once.',
        })
      : null,
  );

  // Each panel earns its place. One depth makes for a card holding a single row,
  // which says less than the stat tiles already did. The compromised-package
  // ranking used to sit here as a second card; it now lives with the chart that
  // draws the same numbers.
  const panels = [
    Object.keys(summary.depth_counts || {}).length > 1
      ? depthBars(summary, (depth) => filterExplore({ depth: String(depth) }))
      : null,
    scopePanel((scope) => filterExplore({ scope })),
  ].filter(Boolean);

  const breakdown = panels.length
    ? el('div', { class: panels.length === 1 ? 'split split--single' : 'split' }, ...panels)
    : null;

  const element = el(
    'div',
    { class: 'page' },
    hero(summary),
    stats,
    breakdown,
    paretoPanel(summary, (ref) => filterExplore({ target: ref })),
    about(),
    runmeta(summary),
  );

  return { element, sync() {} };
}
