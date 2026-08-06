import * as api from '../api.js';
import * as icons from '../icons.js';
import { compact, count, plural } from '../format.js';
import { debounce, el, errorState, replace, svg } from '../dom.js';

/*
 * The graph reads outward: the compromised package sits on the left, and each
 * column to its right holds packages that depend on the column before it. That
 * is the direction the blast travels, and the direction people ask about.
 *
 * Two modes share that reading direction.
 *
 * Expand mode walks the whole report from the compromised packages outward,
 * one column at a time. A free-form node-link diagram over 44k packages is a
 * hairball nobody can read, and the fan-out is brutal: a single popular node
 * has thousands of dependents. Columns keep the chain legible and let each
 * level page independently.
 *
 * Focus mode pins one package and draws every route it has to a compromised
 * one, merged into a single diagram. That is the view for the question the
 * table cannot answer: which of my versions go which way. A package with one
 * route is a straight line; a package with several is a visible fan, and the
 * route list says how many versions take each branch.
 */

const NODE_WIDTH = 250;
const NODE_HEIGHT = 48;
const NODE_GAP = 10;
const COLUMN_GAP = 90;
const COLUMN_STRIDE = NODE_WIDTH + COLUMN_GAP;

const PAGE_SIZE = 25;

// One package in the keyv fixture records 242 routes. Merging them all into one
// diagram produces a wall, so focus mode draws the shortest ones and says how
// many it left out.
const FOCUS_ROUTE_LIMIT = 30;
const SUGGESTIONS = 7;

// The label is SVG text, which does not ellipsize on its own. Monospace at
// 12.5px runs a shade under 7.6px per character inside a 250px node.
const LABEL_CHARS = 29;

const MIN_ZOOM = 0.35;
const MAX_ZOOM = 1.8;

function truncate(text, limit = LABEL_CHARS) {
  return text.length <= limit ? text : text.slice(0, limit - 1) + '…';
}

/** The one line under a node's name: whichever fact is worth the pixels. */
function nodeMeta(node, isRoot) {
  if (isRoot) {
    const versions = plural(node.versions?.length || 0, 'compromised version');
    // A compromised package can also sit downstream of another one, which is
    // why some roots have recorded routes of their own.
    return node.affected ? versions + ' · also downstream' : versions;
  }
  const parts = [];
  if (node.weekly_downloads !== null && node.weekly_downloads !== undefined) {
    parts.push(compact(node.weekly_downloads) + '/wk');
  }
  if (node.version_count) parts.push(plural(node.version_count, 'version'));
  if (node.dependent_count) parts.push(plural(node.dependent_count, 'dependent'));
  return parts.join(' · ') || 'no recorded dependents';
}

/*
 * Two routes can share their hops and differ only in which compromised package
 * they end at, so the target is part of the label whenever a package is
 * attributed to more than one.
 */
function routeLabel(route, withTarget) {
  const via = route.hops?.length ? 'via ' + route.hops.join(' → ') : 'Direct dependency';
  return withTarget ? via + ' → ' + route.target : via;
}

/**
 * Merges a package's routes into one diagram.
 *
 * A node is a package name, so a hop shared by three routes is drawn once and
 * the fan is visible as a fan. Reading runs left to right along the dependency
 * chain: the traced package first, the compromised one last, which is the
 * order the route labels and the trace panel already use.
 */
function buildFocusGraph(pkg, routes) {
  const nodes = new Map();
  const edges = new Map();

  routes.forEach((route) => {
    // Steps run [package, ...hops]; the target is not among them.
    const chain = [pkg.name, ...route.hops, route.target];

    chain.forEach((key, i) => {
      const depth = chain.length - 1 - i; // hops still to go before the target
      let node = nodes.get(key);
      if (!node) {
        node = {
          key,
          depth,
          kind: i === 0 ? 'source' : depth === 0 ? 'target' : 'hop',
          routes: new Set(),
          next: new Set(),
        };
        nodes.set(key, node);
      }
      // A name can sit at different distances on different routes. The
      // furthest placement is the one that keeps every edge pointing forward.
      node.depth = Math.max(node.depth, depth);
      node.routes.add(route.id);
    });

    for (let i = 0; i < chain.length - 1; i++) {
      const id = chain[i] + ' -> ' + chain[i + 1];
      if (!edges.has(id)) edges.set(id, { from: chain[i], to: chain[i + 1], routes: new Set() });
      edges.get(id).routes.add(route.id);
      nodes.get(chain[i]).next.add(chain[i + 1]);
    }
  });

  const byDepth = [];
  for (const node of nodes.values()) (byDepth[node.depth] ||= []).push(node);
  for (let i = 0; i < byDepth.length; i++) byDepth[i] ||= [];

  // byDepth is indexed by hops-to-go, and the diagram reads forward, so the
  // deepest node (the traced package) becomes column 0.
  const columns = byDepth.slice().reverse();
  layoutFocusColumns(columns);

  // pkg.targets covers the whole package, not just the routes drawn, so the
  // counts stay honest when the route list is truncated.
  return { pkg, routes, nodes, edges: [...edges.values()], columns, targets: pkg.targets };
}

/**
 * Places the columns and orders each one to keep the edges from crossing.
 *
 * A left-to-right barycentre sweep: each node settles near the average height
 * of the neighbours already placed to its left. Two passes are enough for a
 * diagram this small, and the outcome is stable because the route order the
 * server returns is.
 */
function layoutFocusColumns(columns) {
  const ROW = NODE_HEIGHT + NODE_GAP;

  const place = () => {
    const tallest = columns.reduce((max, column) => Math.max(max, column.length), 0);
    columns.forEach((column, index) => {
      // Centring against the tallest column keeps the fan symmetrical instead
      // of hanging everything off the top edge.
      const offset = ((tallest - column.length) / 2) * ROW;
      column.forEach((node, row) => {
        node.x = index * COLUMN_STRIDE;
        node.y = offset + row * ROW;
      });
    });
  };

  place();
  for (let pass = 0; pass < 2; pass++) {
    for (let i = 1; i < columns.length; i++) {
      const pull = new Map();
      for (const left of columns[i - 1]) {
        for (const key of left.next) {
          const seen = pull.get(key);
          pull.set(key, seen ? { sum: seen.sum + left.y, n: seen.n + 1 } : { sum: left.y, n: 1 });
        }
      }
      // A node with no neighbour to its left keeps the height it already has.
      const height = (node) => (pull.has(node.key) ? pull.get(node.key).sum / pull.get(node.key).n : node.y);
      columns[i].sort((a, b) => height(a) - height(b));
      place();
    }
  }
}

export function createGraphView({ router, detail }) {
  const scene = svg('g', { class: 'graph__scene' });
  const edgeLayer = svg('g', { class: 'graph__edges' });
  const nodeLayer = svg('g', { class: 'graph__nodes' });
  scene.append(edgeLayer, nodeLayer);

  const arrowhead = svg(
    'defs',
    {},
    svg(
      'marker',
      { id: 'gedge-arrow', viewBox: '0 0 8 8', refX: '7', refY: '4', markerWidth: '6', markerHeight: '6', orient: 'auto' },
      svg('path', { class: 'gedge__head', d: 'M0 0 L8 4 L0 8 z' }),
    ),
  );

  const canvas = svg('svg', { class: 'graph__canvas', role: 'presentation' }, arrowhead, scene);

  const status = el('div', { class: 'graph__status' });

  // columns[0] is the compromised packages; columns[i] holds the dependents of
  // the selected node in columns[i - 1]. Expand mode only.
  let columns = [];

  // The merged route diagram, and which route the list has highlighted. Focus
  // mode only; null whenever expand mode is on screen.
  let focused = null;
  let highlighted = null;

  // The search term that matched no compromised package, or ''. Kept as state
  // rather than painted on the spot, because any later render would wipe it.
  let rootsMiss = '';

  let camera = { x: 60, y: 80, k: 1 };
  let request = null;

  // Guards against a slow expansion painting over a chain the user has since
  // walked away from.
  let generation = 0;

  const searchInput = el('input', {
    class: 'searchbar__input',
    type: 'search',
    on: {
      input: () => onSearch(searchInput.value.trim()),
      keydown: (event) => {
        if (event.key === 'Escape') clearSuggestions();
      },
    },
  });

  // Focus mode's box names one package rather than filtering a list, so it
  // suggests instead of searching as you type.
  const suggestions = el('div', { class: 'graph__suggest', hidden: true });

  const onSearch = debounce((value) => {
    if (state.focus) return suggest(value);
    if (value === state.search) return;
    navigate({ search: value, path: '' });
  }, 250);

  const searchbar = el(
    'div',
    { class: 'searchbar' },
    el('span', { class: 'searchbar__icon' }, icons.search()),
    searchInput,
    suggestions,
  );

  const focusChip = el('div', { class: 'graph__chip', hidden: true });

  const zoomButton = (label, title, apply) =>
    el('button', { class: 'button', type: 'button', title, on: { click: apply } }, label);

  const toolbar = el(
    'div',
    { class: 'graph__toolbar' },
    searchbar,
    focusChip,
    el('span', { class: 'spacer' }),
    status,
    el(
      'div',
      { class: 'graph__zoom' },
      zoomButton('−', 'Zoom out', () => zoomBy(1 / 1.25)),
      zoomButton('+', 'Zoom in', () => zoomBy(1.25)),
      zoomButton('Reset', 'Reset the view', reset),
    ),
  );

  const legend = el('div', { class: 'graph__legend', hidden: true });
  const overlay = el('div', { class: 'graph__overlay', hidden: true });
  const stage = el('div', { class: 'graph__stage' }, canvas, overlay);
  const element = el('div', { class: 'page page--graph' }, toolbar, legend, stage);

  let state = { search: '', path: [], focus: '' };

  // -------------------------------------------------------------------------
  // Camera
  // -------------------------------------------------------------------------

  function applyCamera() {
    scene.setAttribute('transform', `translate(${camera.x},${camera.y}) scale(${camera.k})`);
  }

  function zoomBy(factor, origin) {
    const next = Math.min(Math.max(camera.k * factor, MIN_ZOOM), MAX_ZOOM);
    if (origin) {
      // Keeps the point under the cursor fixed, so zooming feels anchored
      // rather than pulling the graph toward the corner.
      const ratio = next / camera.k;
      camera.x = origin.x - (origin.x - camera.x) * ratio;
      camera.y = origin.y - (origin.y - camera.y) * ratio;
    }
    camera.k = next;
    applyCamera();
  }

  function reset() {
    camera = { x: 60, y: 80, k: 1 };
    applyCamera();
  }

  /**
   * Nudges a newly opened column into view, and only if it is not already
   * there. Recentring on every click made the whole diagram jump sideways
   * under the cursor, which is disorienting when nothing needed to move.
   */
  function panToColumn(index) {
    const width = stage.clientWidth;
    if (!width) return;

    const left = camera.x + index * COLUMN_STRIDE * camera.k;
    const right = left + NODE_WIDTH * camera.k;
    const margin = 24;
    if (left >= margin && right <= width - margin) return;

    camera.x += right > width - margin ? width - margin - right : margin - left;
    applyCamera();
  }

  canvas.addEventListener(
    'wheel',
    (event) => {
      event.preventDefault();
      const box = canvas.getBoundingClientRect();
      const origin = { x: event.clientX - box.left, y: event.clientY - box.top };
      zoomBy(event.deltaY < 0 ? 1.1 : 1 / 1.1, origin);
    },
    { passive: false },
  );

  /*
   * Panning deliberately avoids setPointerCapture. Capturing retargets the
   * click that follows to the canvas, so every node in the graph became
   * unclickable. Window listeners keep the drag working past the edge of the
   * stage without touching where the click lands.
   */
  const DRAG_SLOP = 4;

  canvas.addEventListener('pointerdown', (event) => {
    if (event.button !== 0) return;
    const origin = { x: event.clientX, y: event.clientY };
    const start = { x: event.clientX - camera.x, y: event.clientY - camera.y };
    let dragging = false;

    const move = (moved) => {
      if (!dragging) {
        const far = Math.abs(moved.clientX - origin.x) > DRAG_SLOP || Math.abs(moved.clientY - origin.y) > DRAG_SLOP;
        if (!far) return;
        dragging = true;
        canvas.classList.add('is-panning');
      }
      camera.x = moved.clientX - start.x;
      camera.y = moved.clientY - start.y;
      applyCamera();
    };

    const end = () => {
      canvas.classList.remove('is-panning');
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', end);
      window.removeEventListener('pointercancel', end);
      // A drag that ends over a node must not also activate it.
      if (dragging) canvas.addEventListener('click', swallow, { capture: true, once: true });
    };

    const swallow = (click) => {
      click.stopPropagation();
      click.preventDefault();
    };

    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', end);
    window.addEventListener('pointercancel', end);
  });

  // -------------------------------------------------------------------------
  // Rendering
  // -------------------------------------------------------------------------

  function columnY(index, row) {
    return columns[index].startY + row * (NODE_HEIGHT + NODE_GAP);
  }

  /** The shared node box. Both modes draw the same rectangle; only what goes on it differs. */
  function nodeBox({ name, meta, title, classes, x, y, activate }) {
    const group = svg('g', {
      class: classes.join(' '),
      transform: `translate(${x},${y})`,
      tabindex: activate ? '0' : null,
      role: activate ? 'button' : null,
      'aria-label': `${name}. ${meta}`,
      on: activate
        ? {
            click: activate,
            keydown: (event) => {
              if (event.key !== 'Enter' && event.key !== ' ') return;
              event.preventDefault();
              activate();
            },
          }
        : null,
    });

    group.append(
      svg('title', {}, title || name),
      svg('rect', { class: 'gnode__box', width: NODE_WIDTH, height: NODE_HEIGHT, rx: 8 }),
      svg('text', { class: 'gnode__name', x: 12, y: 20 }, truncate(name)),
      svg('text', { class: 'gnode__meta', x: 12, y: 36 }, truncate(meta, 34)),
    );
    return group;
  }

  /**
   * Opens the detail drawer, which is where the exact version specifiers live.
   *
   * Never drawn on the roots column. A compromised package can also sit
   * downstream of another one, so putting the affordance on roots would make
   * it appear on a seemingly arbitrary subset of them; their sub-label carries
   * that fact instead.
   */
  function detailAffordance(name, cx = NODE_WIDTH - 20) {
    const cy = NODE_HEIGHT / 2;
    return svg(
      'g',
      {
        class: 'gnode__detail',
        role: 'button',
        tabindex: '0',
        'aria-label': `Package details for ${name}`,
        on: {
          click: (event) => {
            event.stopPropagation();
            detail.open(name);
          },
          keydown: (event) => {
            if (event.key !== 'Enter' && event.key !== ' ') return;
            event.preventDefault();
            event.stopPropagation();
            detail.open(name);
          },
        },
      },
      svg('title', {}, 'Package details: versions, routes and specifiers'),
      svg('circle', { class: 'gnode__detail-hit', cx, cy, r: 12, fill: 'transparent' }),
      svg('circle', { class: 'gnode__detail-ring', cx, cy, r: 8 }),
      svg('circle', { class: 'gnode__detail-dot', cx, cy: cy - 3.5, r: 1 }),
      svg('path', { class: 'gnode__detail-stem', d: `M${cx} ${cy - 0.5} L${cx} ${cy + 4}` }),
    );
  }

  function drawNode(node, index, row, isRoot) {
    const expandable = node.dependent_count > 0;

    const classes = ['gnode'];
    if (isRoot) classes.push('gnode--root');
    if (columns[index].selected === node.name) classes.push('is-selected');
    if (!expandable) classes.push('is-leaf');

    const group = nodeBox({
      name: node.name,
      meta: nodeMeta(node, isRoot),
      classes,
      x: index * COLUMN_STRIDE,
      y: columnY(index, row),
      activate: () => select(index, node),
    });

    if (expandable) {
      group.append(svg('path', { class: 'gnode__chevron', d: chevronAt(NODE_WIDTH - 18, NODE_HEIGHT / 2) }));
    }
    if (!isRoot && node.affected) {
      // Left of the chevron, which owns the right edge of an expandable node.
      group.append(detailAffordance(node.name, NODE_WIDTH - (expandable ? 42 : 20)));
    }
    return group;
  }

  function chevronAt(x, y) {
    return `M${x - 3} ${y - 4} L${x + 2} ${y} L${x - 3} ${y + 4}`;
  }

  function drawMoreNode(index, row, column) {
    const x = index * COLUMN_STRIDE;
    const y = columnY(index, row);
    const remaining = column.total - column.nodes.length;

    return svg(
      'g',
      {
        class: 'gnode gnode--more',
        transform: `translate(${x},${y})`,
        tabindex: '0',
        role: 'button',
        on: {
          click: () => loadMore(index),
          keydown: (event) => {
            if (event.key !== 'Enter' && event.key !== ' ') return;
            event.preventDefault();
            loadMore(index);
          },
        },
      },
      svg('rect', { class: 'gnode__box', width: NODE_WIDTH, height: NODE_HEIGHT - 12, rx: 8 }),
      svg('text', { class: 'gnode__more', x: NODE_WIDTH / 2, y: 23 }, `Show ${count(Math.min(PAGE_SIZE, remaining))} more of ${count(remaining)}`),
    );
  }

  function drawEdge(fromIndex, fromRow, toIndex, toRow) {
    const x1 = fromIndex * COLUMN_STRIDE + NODE_WIDTH;
    const y1 = columnY(fromIndex, fromRow) + NODE_HEIGHT / 2;
    const x2 = toIndex * COLUMN_STRIDE;
    const y2 = columnY(toIndex, toRow) + NODE_HEIGHT / 2;
    const bend = (x2 - x1) / 2;
    return svg('path', { class: 'gedge', d: `M${x1} ${y1} C${x1 + bend} ${y1}, ${x2 - bend} ${y2}, ${x2} ${y2}` });
  }

  function drawHeader(index, column) {
    const x = index * COLUMN_STRIDE;
    const label = index === 0 ? 'Compromised' : plural(index, 'hop') + ' away';
    return svg(
      'g',
      { class: 'gheader', transform: `translate(${x},${column.startY - 26})` },
      svg('text', { class: 'gheader__label', x: 0, y: 0 }, label),
      svg('text', { class: 'gheader__count', x: NODE_WIDTH, y: 0 }, count(column.total)),
    );
  }

  function render() {
    replace(edgeLayer);
    replace(nodeLayer);
    if (focused) renderFocus();
    else renderColumns();
    paintOverlay();
    paintStatus();
  }

  /**
   * The search filters compromised package names, so looking for one of your
   * own finds nothing here and the canvas goes blank, which reads as a broken
   * filter. Name the mismatch and point at tracing, which is the question that
   * was actually being asked.
   */
  function paintOverlay() {
    overlay.hidden = !rootsMiss;
    if (!rootsMiss) return replace(overlay);
    replace(
      overlay,
      el('div', { class: 'state__title' }, `No compromised package matches “${rootsMiss}”.`),
      el(
        'div',
        {},
        'This view starts from the compromised packages. To follow one of your own packages to them, trace it instead.',
      ),
    );
  }

  function renderColumns() {
    let previousSelectedRow = 0;

    columns.forEach((column, index) => {
      const parentRow = index === 0 ? 0 : previousSelectedRow;
      // Each column starts level with the node it came from, which keeps the
      // chain reading as one horizontal line rather than a staircase.
      column.startY = index === 0 ? 0 : columns[index - 1].startY + parentRow * (NODE_HEIGHT + NODE_GAP);

      nodeLayer.append(drawHeader(index, column));

      column.nodes.forEach((node, row) => {
        nodeLayer.append(drawNode(node, index, row, index === 0));
        if (index > 0) edgeLayer.append(drawEdge(index - 1, parentRow, index, row));
        if (column.selected === node.name) previousSelectedRow = row;
      });

      if (column.nodes.length < column.total) {
        nodeLayer.append(drawMoreNode(index, column.nodes.length, column));
      }
    });
  }

  // -------------------------------------------------------------------------
  // Focus mode
  // -------------------------------------------------------------------------

  /** Whether a node or edge takes part in the highlighted route. */
  function lit(item) {
    return !highlighted || item.routes.has(highlighted);
  }

  function focusNodeMeta(node) {
    if (node.kind === 'target') return 'compromised';
    if (node.kind === 'source') {
      const pkg = focused.pkg;
      return `${plural(pkg.routes_total, 'route')} · ${plural(pkg.version_count, 'affected version')}`;
    }
    return plural(node.routes.size, 'route') + ' through it';
  }

  function drawFocusNode(node) {
    const classes = ['gnode', 'gnode--' + node.kind];
    if (!lit(node)) classes.push('is-dim');

    // The target is where every chain ends and the source is already the
    // subject, so only the hops in between have somewhere to go.
    const activate = node.kind === 'hop' ? () => focusOn(node.key) : null;

    const group = nodeBox({
      name: node.key,
      meta: focusNodeMeta(node),
      title: activate ? `${node.key} — trace this package instead` : node.key,
      classes,
      x: node.x,
      y: node.y,
      activate,
    });

    if (node.kind !== 'target') group.append(detailAffordance(node.key));
    return group;
  }

  function drawFocusEdge(edge) {
    const from = focused.nodes.get(edge.from);
    const to = focused.nodes.get(edge.to);
    const x1 = from.x + NODE_WIDTH;
    const y1 = from.y + NODE_HEIGHT / 2;
    const x2 = to.x;
    const y2 = to.y + NODE_HEIGHT / 2;
    const bend = Math.max(40, (x2 - x1) / 2);
    return svg('path', {
      class: lit(edge) ? 'gedge' : 'gedge is-dim',
      'marker-end': 'url(#gedge-arrow)',
      d: `M${x1} ${y1} C${x1 + bend} ${y1}, ${x2 - bend} ${y2}, ${x2 - 3} ${y2}`,
    });
  }

  function renderFocus() {
    const { columns: cols } = focused;
    // One header row for the whole diagram. Aligning them to their own column
    // made the labels stagger, which read as a rendering fault.
    const top = Math.min(...[...focused.nodes.values()].map((n) => n.y));

    cols.forEach((column, index) => {
      if (!column.length) return;
      const hopsToGo = cols.length - 1 - index;
      nodeLayer.append(
        svg(
          'g',
          { class: 'gheader', transform: `translate(${index * COLUMN_STRIDE},${top - 26})` },
          svg(
            'text',
            { class: 'gheader__label', x: 0, y: 0 },
            hopsToGo === 0 ? 'Compromised' : plural(hopsToGo, 'hop') + ' away',
          ),
        ),
      );
      column.forEach((node) => nodeLayer.append(drawFocusNode(node)));
    });

    focused.edges.forEach((edge) => edgeLayer.append(drawFocusEdge(edge)));
    paintLegend();
  }

  /**
   * The route list. This is where the version story lives: each entry says how
   * many of the package's affected versions travel that branch, which is the
   * thing a merged diagram alone cannot show.
   */
  function paintLegend() {
    const { pkg, routes, targets } = focused;
    if (routes.length < 2) {
      legend.hidden = true;
      replace(legend);
      return;
    }

    const withTarget = targets.length > 1;
    const tab = (route) =>
      el(
        'button',
        {
          class: 'route-tab',
          type: 'button',
          title: routeLabel(route, withTarget),
          'aria-pressed': String(route.id === highlighted),
          on: {
            click: () => {
              highlighted = route.id === highlighted ? null : route.id;
              render();
            },
          },
        },
        el('span', { class: 'route-tab__name' }, routeLabel(route, withTarget)),
        el(
          'span',
          { class: 'route-tab__meta' },
          `${plural(route.depth, 'hop')} · ${plural(route.version_count, 'version')}`,
        ),
      );

    const dropped = pkg.routes_total - routes.length;

    legend.hidden = false;
    replace(
      legend,
      el(
        'div',
        { class: 'graph__legend-head' },
        el('span', { class: 'eyebrow' }, plural(pkg.routes_total, 'recorded route')),
        highlighted
          ? el(
              'button',
              { class: 'button', type: 'button', on: { click: () => { highlighted = null; render(); } } },
              'Clear highlight',
            )
          : null,
      ),
      el('div', { class: 'routes' }, ...routes.map(tab)),
      dropped > 0
        ? el('p', { class: 'graph__note' }, `Drawing the ${count(routes.length)} shortest routes. ${count(dropped)} more are recorded and not shown.`)
        : null,
    );
  }

  function paintStatus() {
    if (focused) return paintFocusStatus();

    const chain = columns.map((c) => c.selected).filter(Boolean);
    if (!chain.length) {
      replace(status, 'Pick a compromised package to expand its dependents.');
      return;
    }

    // Removing the chevron marks a terminal node, which is easy to miss on a
    // node you just clicked and got no new column from.
    const last = columns[chain.length - 1];
    const node = last.nodes.find((n) => n.name === last.selected);
    const terminal = node && !node.dependent_count;

    replace(
      status,
      el('span', { class: 'graph__chain' }, chain.join(' ← ')),
      terminal ? el('span', { class: 'graph__note' }, 'Nothing in this report depends on it.') : null,
    );
  }

  function paintFocusStatus() {
    const { pkg, routes, targets } = focused;
    const summary =
      pkg.routes_total === 1
        ? `One recorded route to ${targets[0]}.`
        : `${plural(pkg.routes_total, 'recorded route')} to ${plural(targets.length, 'compromised package')}.`;

    const route = highlighted && routes.find((r) => r.id === highlighted);
    replace(
      status,
      el('span', { class: 'graph__chain' }, summary),
      route
        ? el(
            'span',
            { class: 'graph__note' },
            `${routeLabel(route, targets.length > 1)}: ${plural(route.version_count, 'affected version')}.`,
          )
        : null,
    );
  }

  // -------------------------------------------------------------------------
  // Data
  // -------------------------------------------------------------------------

  function fetchColumn(index, search, offset, signal) {
    const parent = index === 0 ? null : columns[index - 1].selected;
    const options = { search, limit: PAGE_SIZE, offset };
    return parent === null
      ? api.graphRoots(options, signal)
      : api.graphDependents(parent, options, signal);
  }

  async function loadColumn(index, { search = '', offset = 0, append = false } = {}) {
    const token = ++generation;
    request?.abort();
    request = new AbortController();

    try {
      const page = await fetchColumn(index, search, offset, request.signal);
      if (token !== generation) return;

      const existing = append ? columns[index].nodes : [];
      columns[index] = {
        nodes: [...existing, ...page.results],
        total: page.total,
        selected: append ? columns[index].selected : null,
        startY: 0,
      };
      if (index === 0) rootsMiss = page.total === 0 && search ? search : '';
      render();

      if (index !== 0) return;
      if (rootsMiss) suggest(rootsMiss);
      else clearSuggestions();
    } catch (error) {
      if (error.name === 'AbortError' || token !== generation) return;
      replace(status, errorState(error));
    }
  }

  function loadMore(index) {
    loadColumn(index, { search: index === 0 ? state.search : '', offset: columns[index].nodes.length, append: true });
  }

  async function select(index, node) {
    columns = columns.slice(0, index + 1);
    columns[index].selected = node.name;
    render();
    writePath();

    if (!node.dependent_count) {
      panToColumn(index);
      return;
    }

    columns.push({ nodes: [], total: 0, selected: null, startY: 0 });
    await loadColumn(index + 1);
    panToColumn(index + 1);
  }

  async function loadFocus(name) {
    const token = ++generation;
    request?.abort();
    request = new AbortController();
    columns = [];
    focused = null;
    highlighted = null;
    rootsMiss = '';
    paintOverlay();
    replace(edgeLayer);
    replace(nodeLayer);
    legend.hidden = true;
    replace(legend);
    replace(status, 'Loading routes…');

    try {
      const pkg = await api.packageDetail(name, {}, request.signal);
      if (token !== generation) return;

      const routes = pkg.routes.slice(0, FOCUS_ROUTE_LIMIT);
      focused = buildFocusGraph(pkg, routes);
      render();
      reset();
    } catch (error) {
      if (error.name === 'AbortError' || token !== generation) return;
      replace(status, errorState(error));
    }
  }

  function focusOn(name) {
    navigate({ focus: name }, { replace: false });
  }

  function clearFocus() {
    navigate({ focus: '' }, { replace: false });
  }

  /*
   * The URL trails the graph rather than driving it. state is updated first so
   * the navigation that follows reads as a no-op in sync(), because replaying
   * the chain the user just walked would refetch every column and throw the
   * camera away.
   */
  function writePath() {
    state = { ...state, path: columns.map((c) => c.selected).filter(Boolean) };
    navigate({}, { replace: true });
  }

  function navigate(patch, options = {}) {
    const params = new URLSearchParams();
    const next = { ...state, ...patch };
    if (next.focus) {
      // The two modes carry unrelated state, so a focused URL drops the
      // expansion chain rather than accumulating both.
      params.set('package', next.focus);
    } else {
      if (next.search) params.set('search', next.search);
      const path = Array.isArray(next.path) ? next.path.join(',') : next.path;
      if (path) params.set('path', path);
    }
    router.go('graph', params, { replace: options.replace !== false });
  }

  // -------------------------------------------------------------------------
  // Picking a package to trace
  // -------------------------------------------------------------------------

  function clearSuggestions() {
    suggestions.hidden = true;
    replace(suggestions);
  }

  let suggestToken = 0;

  async function suggest(term) {
    const token = ++suggestToken;
    if (!term) return clearSuggestions();
    try {
      const page = await api.packages({ search: term, limit: SUGGESTIONS, sort: 'weekly_downloads', dir: 'desc' });
      if (token !== suggestToken) return;
      /*
       * The server's search also matches intermediate hops and targets, so a
       * package that merely routes through the term ranks alongside one named
       * after it. Named matches lead, since that is what a picker is for.
       */
      const needle = term.toLowerCase();
      const named = (p) => p.name.toLowerCase().includes(needle);
      const hits = page.results
        .filter((p) => p.name !== state.focus)
        .sort((a, b) => Number(named(b)) - Number(named(a)));
      if (!hits.length) {
        suggestions.hidden = false;
        replace(suggestions, el('div', { class: 'graph__suggest-empty' }, 'No affected package matches.'));
        return;
      }
      suggestions.hidden = false;
      replace(
        suggestions,
        el('div', { class: 'graph__suggest-head' }, 'Trace a package'),
        ...hits.map((p) =>
          el(
            'button',
            { class: 'graph__suggest-item', type: 'button', on: { click: () => { searchInput.value = ''; clearSuggestions(); focusOn(p.name); } } },
            el('span', { class: 'graph__suggest-name' }, p.name),
            el('span', { class: 'graph__suggest-meta' }, `${plural(p.routes_total, 'route')} · ${plural(p.version_count, 'version')}`),
          ),
        ),
      );
    } catch (error) {
      if (error.name !== 'AbortError') clearSuggestions();
    }
  }

  /** The toolbar says which mode is on screen, and how to leave it. */
  function paintToolbar() {
    clearSuggestions();
    const tracing = Boolean(state.focus);

    searchInput.placeholder = tracing ? 'Trace another package…' : 'Filter compromised packages…';
    searchInput.setAttribute('aria-label', searchInput.placeholder);
    searchInput.value = tracing ? '' : state.search;

    focusChip.hidden = !tracing;
    if (!tracing) {
      replace(focusChip);
      return;
    }
    replace(
      focusChip,
      el('span', { class: 'graph__chip-label' }, 'Tracing'),
      el('code', { class: 'graph__chip-name' }, state.focus),
      el(
        'button',
        { class: 'graph__chip-clear', type: 'button', title: 'Back to expanding from the compromised packages', on: { click: clearFocus } },
        '✕',
      ),
    );
  }

  /** Replays a bookmarked chain one column at a time; each level needs the one above it. */
  async function restore(search, path) {
    columns = [{ nodes: [], total: 0, selected: null, startY: 0 }];
    await loadColumn(0, { search });

    // A single-target report has nothing to choose, so the first column
    // expands itself rather than asking for a click that has one answer.
    if (!path.length && columns[0].total === 1 && columns[0].nodes.length === 1) {
      await select(0, columns[0].nodes[0]);
      return;
    }

    for (let i = 0; i < path.length; i++) {
      const node = columns[i]?.nodes.find((n) => n.name === path[i]);
      if (!node) break;
      columns[i].selected = node.name;
      if (!node.dependent_count) break;
      columns.push({ nodes: [], total: 0, selected: null, startY: 0 });
      await loadColumn(i + 1);
    }

    render();
    panToColumn(columns.length - 1);
  }

  return {
    element,
    focusSearch() {
      searchInput.focus();
      searchInput.select();
    },
    sync(params) {
      const search = params.get('search') || '';
      const focus = params.get('package') || '';
      const path = (params.get('path') || '').split(',').filter(Boolean);

      const unchanged =
        search === state.search && focus === state.focus && path.join(',') === state.path.join(',');
      state = { search, path, focus };
      if (unchanged && (focused || columns.length)) return;

      paintToolbar();
      applyCamera();

      if (focus) {
        loadFocus(focus);
        return;
      }
      focused = null;
      legend.hidden = true;
      replace(legend);
      restore(search, path);
    },
  };
}
