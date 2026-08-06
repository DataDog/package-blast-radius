import * as api from '../api.js';
import * as icons from '../icons.js';
import { compact, count, plural } from '../format.js';
import { debounce, el, errorState, replace, svg } from '../dom.js';

/*
 * One reading grammar for both modes:
 *
 *   - The compromised package is always on the right.
 *   - An arrow always means "depends on", so everything flows rightward into
 *     the compromised package.
 *   - Expanding always grows the diagram leftward, one column further from the
 *     compromised package. The affordance for it lives on a node's left edge;
 *     the right edge belongs to the info mark.
 *
 * Expand mode walks the whole report, starting from the compromised packages
 * and adding a column of dependents at a time. A free-form node-link diagram
 * over 44k packages is a hairball nobody can read, and the fan-out is brutal:
 * a single popular node has thousands of dependents. Columns keep the chain
 * legible and let each level page independently.
 *
 * Focus mode pins one package and draws every route it has to a compromised
 * one, merged into a single diagram. That is the view for the question the
 * table cannot answer: which of my versions go which way. A package with one
 * route is a straight line; a package with several is a visible fan, and the
 * route list says how many versions take each branch. From there the same
 * leftward expansion adds the packages that depend on any node on screen.
 */

const NODE_WIDTH = 250;
const NODE_HEIGHT = 48;
const NODE_GAP = 10;
const COLUMN_GAP = 90;
const COLUMN_STRIDE = NODE_WIDTH + COLUMN_GAP;

// Edges attach at a node's vertical center, so the grow/detail icons sit a
// little above it. Otherwise every icon lines up with every incoming and
// outgoing edge, and the whole chain reads as one line drawn through them.
const ICON_ROW_Y = NODE_HEIGHT / 2 - 9;

const PAGE_SIZE = 25;

// One package in the keyv fixture records 242 routes. Merging them all into one
// diagram produces a wall, so focus mode draws the shortest ones and says how
// many it left out.
const FOCUS_ROUTE_LIMIT = 30;
const SUGGESTIONS = 7;

// SVG text does not ellipsize on its own, so labels are cut by character count.
// Monospace at 12.5px runs a shade under 7.6px per character; the sub-label is
// smaller and narrower.
const NAME_CHAR_PX = 7.6;
const META_CHAR_PX = 6.6;
const LABEL_PAD = 12;

// Width a node hands over to the marks on its edges: the expansion control on
// the left, the info mark on the right. Labels are cut to what is left so they
// never run underneath either one.
const GROW_RESERVE = 26;
const INFO_RESERVE = 34;

const MIN_ZOOM = 0.35;
const MAX_ZOOM = 1.8;

function truncate(text, limit) {
  return text.length <= limit ? text : text.slice(0, limit - 1) + '…';
}

function fitChars(reserveLeft, reserveRight, charPx) {
  return Math.max(4, Math.floor((NODE_WIDTH - LABEL_PAD * 2 - reserveLeft - reserveRight) / charPx));
}

// Edges leave and arrive horizontally, so a short straight stub at each end
// keeps the arrowhead square against the box and confines the curve to the gap
// between columns instead of letting it graze the boxes.
const EDGE_STUB = 14;
const ARROW_GAP = 4;

function edgePath(x1, y1, x2, y2) {
  const start = x1 + EDGE_STUB;
  const end = x2 - EDGE_STUB - ARROW_GAP;
  if (end <= start) {
    const half = (x2 - x1) / 2;
    return `M${x1} ${y1} C${x1 + half} ${y1}, ${x2 - half} ${y2}, ${x2 - ARROW_GAP} ${y2}`;
  }
  const bend = Math.max(16, (end - start) / 2);
  return `M${x1} ${y1} L${start} ${y1} C${start + bend} ${y1}, ${end - bend} ${y2}, ${end} ${y2} L${x2 - ARROW_GAP} ${y2}`;
}

// Several edges leaving one node from the same point read as a single thick
// stroke. Spreading their origins down the node's right edge separates them.
const PORT_SPAN = 26;
const PORT_STEP_MAX = 9;

function portOffset(rank = 0, total = 1) {
  if (total < 2) return NODE_HEIGHT / 2;
  const step = Math.min(PORT_SPAN / (total - 1), PORT_STEP_MAX);
  return NODE_HEIGHT / 2 + (rank - (total - 1) / 2) * step;
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
 *
 * `upstream` carries the dependent sets the user has opened and `unfolded` the
 * long stretches they have asked to see in full. Both are inputs rather than
 * post-processing because either one changes which columns exist, and the
 * layout has to run against the final set.
 */
function buildFocusGraph(pkg, routes, upstream, unfolded) {
  const nodes = new Map();
  const edges = new Map();

  const addEdge = (from, to, routeIds) => {
    const id = from + ' -> ' + to;
    if (!edges.has(id)) edges.set(id, { from, to, routes: new Set() });
    for (const routeId of routeIds) edges.get(id).routes.add(routeId);
  };

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
      addEdge(chain[i], chain[i + 1], [route.id]);
      nodes.get(chain[i]).next.add(chain[i + 1]);
    }
  });

  addUpstream(nodes, addEdge, upstream);

  const byDepth = [];
  for (const node of nodes.values()) (byDepth[node.depth] ||= []).push(node);

  // byDepth is indexed by hops-to-go, and the diagram reads forward, so the
  // deepest node becomes the leftmost column. Raising a shared node's depth
  // can empty a level, and an empty column would read as a missing hop.
  const columns = byDepth
    .map((group, hopsToGo) => ({ nodes: group || [], hopsToGo, hopsFrom: hopsToGo }))
    .reverse()
    .filter((column) => column.nodes.length);

  const folded = foldChains(columns, nodes, edges, unfolded);
  layoutFocusColumns(folded);

  // pkg.targets covers the whole package, not just the routes drawn, so the
  // counts stay honest when the route list is truncated.
  return { pkg, nodes, edges: [...edges.values()], columns: folded, targets: pkg.targets };
}

/**
 * Adds the packages that depend on a node the user expanded.
 *
 * A dependent is one hop further from the compromised package than the node it
 * hangs off, so it lands in a new column to the left. It inherits that node's
 * route set: whichever ways the parent reaches a compromised package, its
 * dependents reach it the same ways, which is what keeps route highlighting
 * meaningful once the diagram has grown.
 */
function addUpstream(nodes, addEdge, upstream) {
  for (const [parentKey, group] of upstream) {
    const parent = nodes.get(parentKey);
    if (!group.open || !parent) continue;

    const depth = parent.depth + 1;
    for (const dep of group.nodes) {
      if (dep.name === parentKey) continue;

      let node = nodes.get(dep.name);
      if (!node) {
        node = { key: dep.name, depth, kind: 'upstream', dep, routes: new Set(), next: new Set() };
        nodes.set(dep.name, node);
      }
      node.depth = Math.max(node.depth, depth);
      for (const routeId of parent.routes) node.routes.add(routeId);
      node.next.add(parentKey);
      addEdge(dep.name, parentKey, parent.routes);
    }

    const remaining = group.total - group.nodes.length;
    if (remaining <= 0) continue;
    const key = MORE_PREFIX + parentKey;
    nodes.set(key, { key, depth, kind: 'more', parent: parentKey, remaining, routes: new Set(parent.routes), next: new Set() });
  }
}

const MORE_PREFIX = 'more:';
const FOLD_PREFIX = 'fold:';

// A stretch shorter than this is cheaper to read than a pill standing in for it.
const FOLD_MIN = 3;

/**
 * Collapses long single-file stretches of the chain into one pill.
 *
 * A nine-hop route whose middle is a straight run of one-in, one-out hops
 * spends most of the canvas saying nothing. Folding it leaves the two ends,
 * which are the parts anyone reads, and the pill expands again on click.
 */
function foldChains(columns, nodes, edges, unfolded) {
  const degree = new Map();
  const bump = (key, side) => {
    const seen = degree.get(key) || { in: 0, out: 0 };
    seen[side]++;
    degree.set(key, seen);
  };
  for (const edge of edges.values()) {
    bump(edge.from, 'out');
    bump(edge.to, 'in');
  }

  const passthrough = (column) => {
    if (column.nodes.length !== 1 || column.nodes[0].kind !== 'hop') return false;
    const seen = degree.get(column.nodes[0].key);
    return Boolean(seen) && seen.in === 1 && seen.out === 1;
  };

  const result = [];
  for (let i = 0; i < columns.length; ) {
    let end = i;
    while (end < columns.length && passthrough(columns[end])) end++;

    const run = columns.slice(i, end);
    const id = run.length ? FOLD_PREFIX + run[0].nodes[0].key : '';
    // Past the whole run, not one column: re-scanning from the second hop
    // would just fold the stretch again under a different name.
    if (run.length && unfolded.has(id)) {
      result.push(...run);
      i = end;
      continue;
    }
    if (run.length < FOLD_MIN) {
      result.push(columns[i]);
      i++;
      continue;
    }

    result.push({
      nodes: [foldRun(run, id, nodes, edges)],
      hopsToGo: run[run.length - 1].hopsToGo,
      hopsFrom: run[0].hopsFrom,
    });
    i = end;
  }
  return result;
}

/** Swaps a run of columns for one node and rewires whatever touched it. */
function foldRun(run, id, nodes, edges) {
  const chain = run.map((column) => column.nodes[0]);
  const inner = new Set(chain.map((node) => node.key));
  const last = chain[chain.length - 1];

  const fold = {
    key: id,
    kind: 'fold',
    hidden: chain.map((node) => node.key),
    depth: last.depth,
    routes: new Set(chain[0].routes),
    next: new Set(),
  };

  for (const [edgeId, edge] of [...edges]) {
    const fromInner = inner.has(edge.from);
    const toInner = inner.has(edge.to);
    if (!fromInner && !toInner) continue;
    edges.delete(edgeId);
    if (fromInner && toInner) continue;

    const from = fromInner ? id : edge.from;
    const to = toInner ? id : edge.to;
    const existing = edges.get(from + ' -> ' + to);
    if (existing) for (const routeId of edge.routes) existing.routes.add(routeId);
    else edges.set(from + ' -> ' + to, { from, to, routes: new Set(edge.routes) });
  }

  for (const key of inner) nodes.delete(key);
  for (const node of nodes.values()) {
    for (const key of inner) if (node.next.delete(key)) node.next.add(id);
  }

  nodes.set(id, fold);
  for (const edge of edges.values()) if (edge.from === id) fold.next.add(edge.to);
  return fold;
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
    const tallest = columns.reduce((max, column) => Math.max(max, column.nodes.length), 0);
    columns.forEach((column, index) => {
      // Centring against the tallest column keeps the fan symmetrical instead
      // of hanging everything off the top edge.
      const offset = ((tallest - column.nodes.length) / 2) * ROW;
      column.nodes.forEach((node, row) => {
        node.x = index * COLUMN_STRIDE;
        node.y = offset + row * ROW;
      });
    });
  };

  place();
  for (let pass = 0; pass < 2; pass++) {
    for (let i = 1; i < columns.length; i++) {
      const pull = new Map();
      for (const left of columns[i - 1].nodes) {
        for (const key of left.next) {
          const seen = pull.get(key);
          pull.set(key, seen ? { sum: seen.sum + left.y, n: seen.n + 1 } : { sum: left.y, n: 1 });
        }
      }
      // A node with no neighbour to its left keeps the height it already has.
      const height = (node) => (pull.has(node.key) ? pull.get(node.key).sum / pull.get(node.key).n : node.y);
      columns[i].nodes.sort((a, b) => height(a) - height(b));
      place();
    }
  }
}

function columnLabel(column) {
  if (column.hopsToGo === 0) return 'Compromised';
  if (column.hopsFrom > column.hopsToGo) return `${column.hopsToGo}–${column.hopsFrom} hops away`;
  return plural(column.hopsToGo, 'hop') + ' away';
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
      {
        id: 'gedge-arrow',
        viewBox: '0 0 10 10',
        refX: '9',
        refY: '5',
        markerWidth: '9',
        markerHeight: '9',
        // Sized in user units so a thicker highlighted edge keeps the same head.
        markerUnits: 'userSpaceOnUse',
        orient: 'auto',
      },
      svg('path', { class: 'gedge__head', d: 'M0 1.6 L9 5 L0 8.4 Z' }),
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

  // What focus mode is built from. The diagram is derived, so every control
  // that changes it edits one of these and rebuilds.
  let focusPkg = null;
  let focusRoutes = [];
  let soloed = false;

  // Package name -> { nodes, total, open }. The dependent sets expanded off
  // nodes in the diagram. Deliberately not in the URL: it is scratch work on a
  // view the URL already identifies, and it resets when you trace elsewhere.
  let upstream = new Map();

  // Fold ids the user has opened back up.
  let unfolded = new Set();

  // The search term that matched no compromised package, or ''. Kept as state
  // rather than painted on the spot, because any later render would wipe it.
  let rootsMiss = '';

  let camera = { x: 900, y: 80, k: 1 };
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

  /**
   * Expand mode grows leftward from a column pinned at x = 0, so the home
   * position parks that column near the right of the stage and leaves the rest
   * of the canvas as the room the chain will grow into. Focus mode already
   * spans its own width, so it starts at the left edge.
   */
  function homeCamera() {
    const width = stage.clientWidth || 1200;
    return { x: focused ? 60 : Math.max(60, width - NODE_WIDTH - 120), y: 80, k: 1 };
  }

  function reset() {
    camera = homeCamera();
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

    const left = camera.x + columnX(index) * camera.k;
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

  /*
   * The compromised column is pinned at x = 0 and every expansion runs into
   * negative x. Anchoring the far end means the package you started from never
   * moves out from under you as the chain grows.
   */
  function columnX(index) {
    return -index * COLUMN_STRIDE;
  }

  function columnY(index, row) {
    return columns[index].startY + row * (NODE_HEIGHT + NODE_GAP);
  }

  /** The shared node box. Both modes draw the same rectangle; only what goes on it differs. */
  function nodeBox({ name, meta, title, classes, x, y, activate, reserveLeft = 0, reserveRight = 0 }) {
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

    const textX = LABEL_PAD + reserveLeft;
    group.append(
      svg('title', {}, title || name),
      svg('rect', { class: 'gnode__box', width: NODE_WIDTH, height: NODE_HEIGHT, rx: 8 }),
      svg('text', { class: 'gnode__name', x: textX, y: 20 }, truncate(name, fitChars(reserveLeft, reserveRight, NAME_CHAR_PX))),
      svg('text', { class: 'gnode__meta', x: textX, y: 36 }, truncate(meta, fitChars(reserveLeft, reserveRight, META_CHAR_PX))),
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
    const cy = ICON_ROW_Y;
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

  /**
   * The expansion control: a circled plus that becomes a minus once open.
   *
   * Always on the left edge, because expanding always adds a column to the
   * left. Passing no activate leaves it decorative, which is what expand mode
   * wants: there the whole node is already the button.
   */
  function growAffordance({ open, label, activate = null, disabled = false, loading = false }) {
    const cx = 18;
    const cy = ICON_ROW_Y;

    const classes = ['gnode__grow'];
    if (open) classes.push('is-open');
    if (disabled) classes.push('is-disabled');
    if (loading) classes.push('is-loading');
    if (!activate) classes.push('is-static');

    const group = svg('g', {
      class: classes.join(' '),
      role: activate ? 'button' : null,
      tabindex: activate ? '0' : null,
      'aria-label': activate ? label : null,
      'aria-hidden': activate ? null : 'true',
      on: activate
        ? {
            click: (event) => {
              event.stopPropagation();
              activate();
            },
            keydown: (event) => {
              if (event.key !== 'Enter' && event.key !== ' ') return;
              event.preventDefault();
              event.stopPropagation();
              activate();
            },
          }
        : null,
    });

    group.append(
      svg('title', {}, label),
      svg('circle', { class: 'gnode__grow-hit', cx, cy, r: 13, fill: 'transparent' }),
      svg('circle', { class: 'gnode__grow-ring', cx, cy, r: 8 }),
      svg('path', { class: 'gnode__grow-bar', d: `M${cx - 4} ${cy} L${cx + 4} ${cy}` }),
    );
    // The vertical stroke is what turns a minus into a plus, so only a closed
    // control draws it.
    if (!open) group.append(svg('path', { class: 'gnode__grow-bar', d: `M${cx} ${cy - 4} L${cx} ${cy + 4}` }));
    return group;
  }

  function drawNode(node, index, row, isRoot) {
    const expandable = node.dependent_count > 0;
    const expanded = columns[index].selected === node.name;
    const withInfo = !isRoot && node.affected;

    const classes = ['gnode'];
    if (isRoot) classes.push('gnode--root');
    if (expanded) classes.push('is-selected');
    if (!expandable) classes.push('is-leaf');

    const group = nodeBox({
      name: node.name,
      meta: nodeMeta(node, isRoot),
      title: expandable
        ? `${node.name} — click to ${expanded ? 'collapse' : 'show the packages that depend on it'}`
        : node.name,
      classes,
      x: columnX(index),
      y: columnY(index, row),
      activate: () => select(index, node),
      reserveLeft: expandable ? GROW_RESERVE : 0,
      reserveRight: withInfo ? INFO_RESERVE : 0,
    });

    if (expandable) {
      group.append(growAffordance({ open: expanded, label: plural(node.dependent_count, 'dependent') }));
    }
    if (withInfo) group.append(detailAffordance(node.name));
    return group;
  }

  function drawMoreNode(index, row, column) {
    const x = columnX(index);
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

  /**
   * The dependent sits one column to the left of the package it depends on, so
   * the arrow runs rightward out of `dependentIndex` and into `parentIndex`.
   * A parent collects a whole column of dependents, so it is the parent's edge
   * that fans.
   */
  function drawEdge(parentIndex, parentRow, dependentIndex, dependentRow, fanRank, fanTotal) {
    const x1 = columnX(dependentIndex) + NODE_WIDTH;
    const y1 = columnY(dependentIndex, dependentRow) + NODE_HEIGHT / 2;
    const x2 = columnX(parentIndex);
    const y2 = columnY(parentIndex, parentRow) + portOffset(fanRank, fanTotal);
    return svg('path', { class: 'gedge', 'marker-end': 'url(#gedge-arrow)', d: edgePath(x1, y1, x2, y2) });
  }

  function drawHeader(index, column) {
    const x = columnX(index);
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
        if (index > 0) edgeLayer.append(drawEdge(index - 1, parentRow, index, row, row, column.nodes.length));
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
      return `${plural(focusPkg.routes_total, 'route')} · ${plural(focusPkg.version_count, 'affected version')}`;
    }
    if (node.kind === 'upstream') return nodeMeta(node.dep, false);
    return plural(node.routes.size, 'route') + ' through it';
  }

  /**
   * A node's dependents can be pulled in unless nothing depends on it. Every
   * node knows its count before it is drawn, the traced package and its hops
   * from the detail payload and a dependent from its own entry, so a control
   * that would open onto nothing is never offered.
   */
  function growable(node) {
    if (node.kind === 'upstream') return node.dep.dependent_count > 0;
    if (node.kind !== 'source' && node.kind !== 'hop') return false;
    const counts = focusPkg.dependent_counts;
    return !counts || counts[node.key] > 0;
  }

  function growLabel(node) {
    const group = upstream.get(node.key);
    if (group?.loading) return `Loading the packages that depend on ${node.key}…`;
    if (group?.open) return `Hide the packages that depend on ${node.key}`;
    if (group && !group.total) return `Nothing in this report depends on ${node.key}`;
    return `Show the packages that depend on ${node.key}`;
  }

  function drawFocusNode(node) {
    if (node.kind === 'fold') return drawFoldNode(node);
    if (node.kind === 'more') return drawMoreUpstreamNode(node);

    const classes = ['gnode', 'gnode--' + node.kind];
    if (!lit(node)) classes.push('is-dim');

    // The target is where every chain ends and the source is already the
    // subject, so everything else can be traced in its own right.
    const activate = node.kind === 'source' || node.kind === 'target' ? null : () => focusOn(node.key);
    const withGrow = growable(node);
    const group = upstream.get(node.key);

    const box = nodeBox({
      name: node.key,
      meta: focusNodeMeta(node),
      title: activate ? `${node.key} — trace this package instead` : node.key,
      classes,
      x: node.x,
      y: node.y,
      activate,
      reserveLeft: withGrow ? GROW_RESERVE : 0,
      reserveRight: node.kind === 'target' ? 0 : INFO_RESERVE,
    });

    if (withGrow) {
      box.append(
        growAffordance({
          open: Boolean(group?.open && !group.loading),
          loading: Boolean(group?.loading),
          disabled: Boolean(group && !group.loading && !group.total),
          label: growLabel(node),
          activate: () => toggleUpstream(node.key),
        }),
      );
    }
    if (node.kind !== 'target') box.append(detailAffordance(node.key));
    return box;
  }

  function drawFoldNode(node) {
    const classes = ['gnode', 'gnode--fold'];
    if (!lit(node)) classes.push('is-dim');
    return nodeBox({
      name: plural(node.hidden.length, 'hop') + ' hidden',
      meta: 'one route through, click to show',
      title: node.hidden.join(' → '),
      classes,
      x: node.x,
      y: node.y,
      activate: () => {
        unfolded.add(node.key);
        rebuildFocus();
        render();
      },
    });
  }

  function drawMoreUpstreamNode(node) {
    return nodeBox({
      name: `Show ${count(Math.min(PAGE_SIZE, node.remaining))} more`,
      meta: `of ${count(node.remaining)} still to show`,
      classes: ['gnode', 'gnode--more-upstream'],
      x: node.x,
      y: node.y,
      activate: () => loadMoreUpstream(node.parent),
    });
  }

  /**
   * Assigns every edge a departure and an arrival point on its two nodes.
   *
   * Ranking a node's edges by the vertical position of the package at the other
   * end means the fan leaves in the same order it arrives, so edges sharing a
   * node splay apart instead of crossing over each other on the way out.
   */
  function focusPorts() {
    const outgoing = new Map();
    const incoming = new Map();
    for (const edge of focused.edges) {
      if (!outgoing.has(edge.from)) outgoing.set(edge.from, []);
      if (!incoming.has(edge.to)) incoming.set(edge.to, []);
      outgoing.get(edge.from).push(edge);
      incoming.get(edge.to).push(edge);
    }

    const ports = new Map();
    const at = (edge) => (ports.has(edge) ? ports.get(edge) : ports.set(edge, {}).get(edge));

    for (const [key, edges] of outgoing) {
      edges.sort((a, b) => focused.nodes.get(a.to).y - focused.nodes.get(b.to).y);
      edges.forEach((edge, rank) => (at(edge).y1 = focused.nodes.get(key).y + portOffset(rank, edges.length)));
    }
    for (const [key, edges] of incoming) {
      edges.sort((a, b) => focused.nodes.get(a.from).y - focused.nodes.get(b.from).y);
      edges.forEach((edge, rank) => (at(edge).y2 = focused.nodes.get(key).y + portOffset(rank, edges.length)));
    }
    return ports;
  }

  function drawFocusEdge(edge, port) {
    const from = focused.nodes.get(edge.from);
    const to = focused.nodes.get(edge.to);
    const classes = ['gedge'];
    if (!lit(edge)) classes.push('is-dim');
    else if (highlighted) classes.push('is-lit');

    return svg('path', {
      class: classes.join(' '),
      'marker-end': 'url(#gedge-arrow)',
      d: edgePath(from.x + NODE_WIDTH, port.y1, to.x, port.y2),
    });
  }

  function renderFocus() {
    // One header row for the whole diagram. Aligning them to their own column
    // made the labels stagger, which read as a rendering fault.
    const top = Math.min(...[...focused.nodes.values()].map((n) => n.y));

    focused.columns.forEach((column, index) => {
      nodeLayer.append(
        svg(
          'g',
          { class: 'gheader', transform: `translate(${index * COLUMN_STRIDE},${top - 26})` },
          svg('text', { class: 'gheader__label', x: 0, y: 0 }, columnLabel(column)),
        ),
      );
      column.nodes.forEach((node) => nodeLayer.append(drawFocusNode(node)));
    });

    // Highlighted edges are drawn last so the route being read sits on top of
    // the ones it crosses.
    const ports = focusPorts();
    const byPriority = [...focused.edges].sort((a, b) => Number(lit(a)) - Number(lit(b)));
    byPriority.forEach((edge) => edgeLayer.append(drawFocusEdge(edge, ports.get(edge))));
    paintLegend();
  }

  /**
   * The route list. This is where the version story lives: each entry says how
   * many of the package's affected versions travel that branch, which is the
   * thing a merged diagram alone cannot show.
   */
  function pickRoute(id) {
    highlighted = id === highlighted ? null : id;
    // Solo without a route to solo would empty the canvas.
    if (!highlighted) soloed = false;
    rebuildFocus();
    render();
  }

  function toggleSolo() {
    soloed = !soloed;
    rebuildFocus();
    render();
  }

  function paintLegend() {
    const targets = focused.targets;
    if (focusRoutes.length < 2) {
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
          on: { click: () => pickRoute(route.id) },
        },
        el('span', { class: 'route-tab__name' }, routeLabel(route, withTarget)),
        el(
          'span',
          { class: 'route-tab__meta' },
          `${plural(route.depth, 'hop')} · ${plural(route.version_count, 'version')}`,
        ),
      );

    const dropped = focusPkg.routes_total - focusRoutes.length;

    legend.hidden = false;
    replace(
      legend,
      el(
        'div',
        { class: 'graph__legend-head' },
        el('span', { class: 'eyebrow' }, plural(focusPkg.routes_total, 'recorded route')),
        highlighted
          ? el(
              'button',
              {
                class: 'button',
                type: 'button',
                'aria-pressed': String(soloed),
                title: soloed
                  ? 'Bring the other routes back into the diagram'
                  : 'Draw only the highlighted route, instead of fading the rest',
                on: { click: toggleSolo },
              },
              soloed ? 'Show all routes' : 'Show only this route',
            )
          : null,
        highlighted
          ? el('button', { class: 'button', type: 'button', on: { click: () => pickRoute(highlighted) } }, 'Clear highlight')
          : null,
      ),
      el('div', { class: 'routes' }, ...focusRoutes.map(tab)),
      dropped > 0
        ? el('p', { class: 'graph__note' }, `Drawing the ${count(focusRoutes.length)} shortest routes. ${count(dropped)} more are recorded and not shown.`)
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

    // Withholding the expansion control marks a terminal node, which is easy
    // to miss on a node you just clicked and got no new column from.
    const last = columns[chain.length - 1];
    const node = last.nodes.find((n) => n.name === last.selected);
    const terminal = node && !node.dependent_count;

    replace(
      status,
      // Reads in the direction the diagram does: outermost dependent first,
      // compromised package last.
      el('span', { class: 'graph__chain' }, [...chain].reverse().join(' → ')),
      terminal ? el('span', { class: 'graph__note' }, 'Nothing in this report depends on it.') : null,
    );
  }

  function paintFocusStatus() {
    const targets = focused.targets;
    const summary =
      focusPkg.routes_total === 1
        ? `One recorded route to ${targets[0]}.`
        : `${plural(focusPkg.routes_total, 'recorded route')} to ${plural(targets.length, 'compromised package')}.`;

    const route = highlighted && focusRoutes.find((r) => r.id === highlighted);
    const added = [...upstream.values()].filter((group) => group.open).reduce((sum, group) => sum + group.nodes.length, 0);

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
      added
        ? el('span', { class: 'graph__note' }, `Plus ${plural(added, 'package')} that depend on what is on screen.`)
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
    const collapse = columns[index].selected === node.name;
    columns = columns.slice(0, index + 1);
    columns[index].selected = collapse ? null : node.name;

    if (collapse) {
      render();
      writePath();
      panToColumn(index);
      return;
    }

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

  /**
   * The diagram is derived state; every focus control edits an input and
   * rebuilds. Columns are laid out from the left, so a new one pushes the rest
   * along; the camera moves with them to hold the diagram still under the
   * cursor.
   */
  function rebuildFocus() {
    const before = focused ? focused.columns.length : 0;
    const routes = soloed && highlighted ? focusRoutes.filter((r) => r.id === highlighted) : focusRoutes;
    focused = buildFocusGraph(focusPkg, routes, upstream, unfolded);
    const grew = focused.columns.length - before;
    if (before && grew) camera.x -= grew * COLUMN_STRIDE * camera.k;
  }

  async function loadFocus(name) {
    const token = ++generation;
    request?.abort();
    request = new AbortController();
    columns = [];
    focused = null;
    focusPkg = null;
    focusRoutes = [];
    highlighted = null;
    soloed = false;
    upstream = new Map();
    unfolded = new Set();
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

      focusPkg = pkg;
      focusRoutes = pkg.routes.slice(0, FOCUS_ROUTE_LIMIT);
      rebuildFocus();
      render();
      reset();
    } catch (error) {
      if (error.name === 'AbortError' || token !== generation) return;
      replace(status, errorState(error));
    }
  }

  /**
   * Pulls in the packages that depend on one node of the diagram, or puts them
   * away again. The fetch is not aborted on navigation; the generation check
   * discards it instead, so a slow expansion cannot paint over a diagram the
   * user has already left.
   */
  async function toggleUpstream(name) {
    const group = upstream.get(name);
    if (group) {
      if (group.total) group.open = !group.open;
      rebuildFocus();
      render();
      return;
    }

    const token = generation;
    upstream.set(name, { nodes: [], total: 0, open: true, loading: true });
    render();

    try {
      const page = await api.graphDependents(name, { limit: PAGE_SIZE });
      if (token !== generation) return;
      upstream.set(name, { nodes: page.results, total: page.total, open: true, loading: false });
    } catch (error) {
      if (token !== generation) return;
      upstream.delete(name);
      render();
      replace(status, errorState(error));
      return;
    }
    rebuildFocus();
    render();
  }

  async function loadMoreUpstream(name) {
    const group = upstream.get(name);
    if (!group) return;

    const token = generation;
    try {
      const page = await api.graphDependents(name, { limit: PAGE_SIZE, offset: group.nodes.length });
      if (token !== generation) return;
      group.nodes = [...group.nodes, ...page.results];
      group.total = page.total;
    } catch (error) {
      if (token !== generation) return;
      replace(status, errorState(error));
      return;
    }
    rebuildFocus();
    render();
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
    const arriving = !columns.length;
    columns = [{ nodes: [], total: 0, selected: null, startY: 0 }];
    if (arriving) reset();
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
