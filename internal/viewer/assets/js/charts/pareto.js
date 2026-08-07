/**
 * Pareto chart: what each compromised package accounts for, as bars, against the
 * running total, as a line.
 *
 * Affected versions are attributed one apiece, so the bars divide the report
 * between them and the line climbs to all of it. Counted as packages the two
 * differ: one affected package can hold versions from several compromised
 * packages, so bars may add up to more than the total while the line, which
 * counts each package once, still ends at it.
 *
 * The legend is drawn inside the SVG rather than beside it in HTML, so that
 * copying the chart as an image copies something that explains itself.
 */

import { append, el, replace, svg } from '../dom.js';
import { count, percent } from '../format.js';
import { bandScale, linearScale, ticks } from './scale.js';

const WIDTH = 1180;
const HEIGHT = 500;
// The top margin holds the legend row; x labels are horizontal, so the bottom
// only has to hold their own lines and nothing sweeps up into the plot.
const MARGIN = { top: 46, right: 30, bottom: 46, left: 48 };
const INNER_W = WIDTH - MARGIN.left - MARGIN.right;
const INNER_H = HEIGHT - MARGIN.top - MARGIN.bottom;

// A bar shorter than this has no room to hold its own label, so the label goes
// above it instead.
const MIN_BAR_FOR_INSIDE_LABEL = 34;

// Labelling every point on the running total prints the same number several
// times over, because the line is flat wherever shares overlap. The two ends
// always carry a label; in between, only a real rise earns one.
const CUM_LABEL_RISE = 3;

// A report can name as few as two compromised packages with dependents. Left to
// divide the plot between them, two bars come out a third of it wide each; this
// keeps a bar a bar and centres however few there are.
const MAX_SLOT_PX = 110;

/*
 * x labels are horizontal, so a name has only its own slot to sit in: left to
 * itself, "gatsby-plugin-sharp" runs straight through both its neighbours.
 * Names therefore wrap at the boundaries they already have, a scope's slash and
 * then a dash, and whatever still does not fit is truncated. The full name is
 * always one hover away.
 */
const MAX_LABEL_LINES = 3;
// Roughly the average advance of a lowercase character at the label's 10.5px.
// Over-estimating costs a character of a long name; under-estimating collides.
const LABEL_CHAR_PX = 6;

/** "@keyv/redis" -> ["@keyv", "redis"] */
function splitScope(name) {
  if (name.startsWith('@')) {
    const slash = name.indexOf('/');
    if (slash > 0) return [name.slice(0, slash), name.slice(slash + 1)];
  }
  return [name];
}

function wrapAtDash(text, fitsChars) {
  if (text.length <= fitsChars) return [text];
  const cut = text.lastIndexOf('-', fitsChars);
  // The dash stays on the first line, so the break reads as a break in a name
  // rather than as two names.
  return cut > 0 ? [text.slice(0, cut + 1), text.slice(cut + 1)] : [text];
}

/** "cacheable-request@13.0.20" -> ["cacheable-request", "@13.0.20"] */
function splitRef(ref) {
  const at = ref.lastIndexOf('@');
  return at > 0 ? [ref.slice(0, at), ref.slice(at)] : [ref, ''];
}

/**
 * A row's label, as lines with the class each carries. The compromised version
 * is a "name@version" reference: the name wraps as a name, and the version drops
 * onto its own line in a quieter colour, since the ranking is about the package.
 */
function labelLines(row, budgetPx) {
  const fitsChars = Math.max(Math.floor(budgetPx / LABEL_CHAR_PX), 4);
  const fit = (text) => (text.length <= fitsChars ? text : `${text.slice(0, fitsChars - 1)}…`);

  if (row.lines) return row.lines.map((text) => ({ text: fit(text), class: null }));

  const [name, version] = splitRef(row.package);
  const scoped = splitScope(name);

  const lines = [];
  for (const [i, part] of scoped.entries()) {
    const muted = scoped.length > 1 && i === 0;
    for (const line of wrapAtDash(part, fitsChars)) {
      lines.push({ text: fit(line), class: muted ? 'pareto__scope' : null });
    }
  }
  if (version) lines.push({ text: fit(version), class: 'pareto__version' });

  return lines.slice(0, MAX_LABEL_LINES);
}

/**
 * The named rows, plus one standing for every compromised package the chart does
 * not name, so the bars and the line both end at the whole report.
 *
 * The tail bar is the one bar that is not a share of its own: it is what the rest
 * add on top of the named rows, which is the only figure attributable to a group
 * without counting overlaps twice. It is drawn in its own fill and says so.
 */
function chartRows(series) {
  const total = series.total || 0;
  const share = (n) => (total > 0 ? (n / total) * 100 : 0);

  const rows = series.rows.map((row) => ({
    package: row.package,
    count: row.count,
    pct: share(row.count),
    cumulativeCount: row.cumulative_count,
    cumulativePct: share(row.cumulative_count),
  }));

  const named = rows.length ? rows[rows.length - 1].cumulativeCount : 0;
  const restCount = series.distinct_sources - series.rows.length;
  const restAdds = total - named;

  if (restCount > 0 && restAdds > 0) {
    rows.push({
      tail: true,
      package: `the other ${count(restCount)} compromised packages`,
      lines: ['the other', count(restCount)],
      count: restAdds,
      pct: share(restAdds),
      cumulativeCount: total,
      cumulativePct: share(total),
    });
  }
  return rows;
}

/**
 * Points worth labelling: both ends, plus any point that rises meaningfully
 * over the one before it, which is where a plateau starts.
 */
function labelledPoints(rows) {
  const marked = new Set([0, rows.length - 1]);
  for (let i = 1; i < rows.length; i++) {
    if (rows[i].cumulativePct - rows[i - 1].cumulativePct > CUM_LABEL_RISE) marked.add(i);
  }
  return marked;
}

function legendGroup(hasTail) {
  const keys = [
    { shape: 'square', class: 'pareto__key-bar', label: 'Attributed to this compromised package' },
    hasTail ? { shape: 'square', class: 'pareto__key-tail', label: 'What the rest add' } : null,
    { shape: 'line', class: 'pareto__key-line', label: 'Running total, each counted once' },
  ].filter(Boolean);

  const nodes = [];
  let x = 0;
  for (const key of keys) {
    if (key.shape === 'square') {
      nodes.push(svg('rect', { class: key.class, x, y: 1, width: 11, height: 11, rx: 2 }));
    } else {
      nodes.push(
        svg('path', { class: key.class, d: `M${x} ${6.5} L${x + 11} ${6.5}` }),
        svg('circle', { class: key.class, cx: x + 5.5, cy: 6.5, r: 3 }),
      );
    }
    nodes.push(svg('text', { class: 'pareto__key-label', x: x + 17, y: 11 }, key.label));
    x += 17 + key.label.length * 6.1 + 24;
  }
  return svg('g', { transform: `translate(${MARGIN.left},14)` }, ...nodes);
}

/**
 * Package names are untrusted, so the tooltip is built from nodes rather than a
 * markup string, the same rule the rest of the viewer follows.
 */
function tooltipBody(title, lines) {
  return [el('strong', {}, title), ...lines.map((line) => el('div', {}, line))];
}

function attachTooltip(wrapper, tip, node, build) {
  const move = (event) => {
    replace(tip, ...build());
    tip.classList.add('is-visible');

    const bounds = wrapper.getBoundingClientRect();
    const x = event.clientX - bounds.left;
    // Flips to the other side of the cursor near the right edge, so the last
    // few bars do not push the tooltip out of the panel.
    const flip = x + 14 + tip.offsetWidth > bounds.width;
    tip.style.left = `${flip ? Math.max(x - 14 - tip.offsetWidth, 0) : x + 14}px`;
    tip.style.top = `${event.clientY - bounds.top - 10}px`;
  };

  node.addEventListener('pointermove', move);
  node.addEventListener('pointerleave', () => tip.classList.remove('is-visible'));
}

/**
 * @param series - one unit's series from /api/pareto
 * @param unitLabel - what the counts are counting, for the tooltips
 */
export function paretoChart({ series, unitLabel }) {
  const rows = chartRows(series);
  const wrapper = el('div', { class: 'pareto' });
  const tip = el('div', { class: 'pareto__tooltip' });

  const x = bandScale(rows.length, INNER_W, { maxStep: MAX_SLOT_PX });
  const y = linearScale(100, INNER_H);
  const gridValues = ticks(100, 5);

  const grid = gridValues.map((value) =>
    svg('line', { class: 'pareto__grid', x1: 0, x2: INNER_W, y1: y(value), y2: y(value) }),
  );

  const axis = gridValues.map((value) =>
    svg('text', { class: 'pareto__axis-label', x: -8, y: y(value) + 4 }, percent(value)),
  );

  const bars = [];
  const barLabels = [];
  rows.forEach((row, i) => {
    const top = y(row.pct);
    const height = INNER_H - top;
    const centre = x.center(i);

    const bar = svg('rect', {
      class: row.tail ? 'pareto__bar pareto__bar--tail' : 'pareto__bar',
      x: x.start(i),
      y: top,
      width: x.bandwidth,
      height: Math.max(height, 1),
      rx: 3,
    });
    attachTooltip(wrapper, tip, bar, () =>
      row.tail
        ? tooltipBody(row.package, [
            `${count(row.count)} ${unitLabel} the named ones do not account for`,
            `${percent(row.pct)} of ${count(series.total)}`,
          ])
        : tooltipBody(row.package, [
            `${count(row.count)} ${unitLabel} (${percent(row.pct)})`,
            `out of ${count(series.total)} in the report`,
          ]),
    );
    bars.push(bar);

    // Count first, share in parentheses: the quantity is the fact and the share
    // is how to read it against the total.
    const inside = height >= MIN_BAR_FOR_INSIDE_LABEL;
    barLabels.push(
      svg(
        'text',
        {
          class: inside ? 'pareto__bar-label pareto__bar-label--inside' : 'pareto__bar-label',
          x: centre,
          y: inside ? top + 15 : top - 19,
        },
        svg('tspan', { class: 'pareto__count', x: centre }, count(row.count)),
        svg('tspan', { class: 'pareto__pct', x: centre, dy: 13 }, `(${percent(row.pct)})`),
      ),
    );
  });

  const path = rows.map((row, i) => `${i === 0 ? 'M' : 'L'}${x.center(i)} ${y(row.cumulativePct)}`).join(' ');
  const line = rows.length ? [svg('path', { class: 'pareto__line', d: path })] : [];

  const marked = labelledPoints(rows);
  const dots = [];
  const cumLabels = [];
  rows.forEach((row, i) => {
    const centre = x.center(i);
    const dot = svg('circle', { class: 'pareto__dot', cx: centre, cy: y(row.cumulativePct), r: 4 });
    attachTooltip(wrapper, tip, dot, () =>
      tooltipBody(row.tail ? 'All of them together' : `Top ${i + 1} together`, [
        `${count(row.cumulativeCount)} ${unitLabel}, each counted once`,
        `${percent(row.cumulativePct)} of ${count(series.total)}`,
      ]),
    );
    dots.push(dot);

    if (marked.has(i)) {
      cumLabels.push(
        svg(
          'text',
          { class: 'pareto__cum-label', x: centre, y: y(row.cumulativePct) - 10 },
          percent(row.cumulativePct),
        ),
      );
    }
  });

  // Neighbouring labels are centred too, so a label may use its whole slot
  // rather than only the width of its bar.
  const labelBudget = x.step - 6;

  const xLabels = rows.map((row, i) => {
    const centre = x.center(i);
    return svg(
      'text',
      { class: row.tail ? 'pareto__xlabel pareto__xlabel--tail' : 'pareto__xlabel', x: centre, y: 14 },
      ...labelLines(row, labelBudget).map((line, n) =>
        svg('tspan', { class: line.class, x: centre, dy: n === 0 ? 0 : 12 }, line.text),
      ),
    );
  });

  const plot = svg(
    'g',
    { transform: `translate(${MARGIN.left},${MARGIN.top})` },
    ...grid,
    ...axis,
    ...bars,
    ...barLabels,
    ...line,
    ...dots,
    ...cumLabels,
    svg('g', { transform: `translate(0,${INNER_H})` }, ...xLabels),
  );

  const chart = svg(
    'svg',
    {
      class: 'pareto__svg',
      viewBox: `0 0 ${WIDTH} ${HEIGHT}`,
      preserveAspectRatio: 'xMidYMid meet',
      role: 'img',
      'aria-label': rows.length
        ? `${rows[0].package} carries ${percent(rows[0].pct)} of ${unitLabel} on its own; ` +
          `all ${rows.length} bars together account for ${percent(rows[rows.length - 1].cumulativePct)}.`
        : 'Nothing to chart.',
    },
    legendGroup(rows.some((row) => row.tail)),
    plot,
  );

  // The chart scales with the panel down to a floor, below which labelled bars
  // stop being readable at any font size and the plot scrolls instead. The
  // tooltip stays outside the scroller, so its coordinates need no correction.
  return append(wrapper, [el('div', { class: 'pareto__scroll' }, chart), tip]);
}
