/**
 * Copying a diagram out of the page as a PNG.
 *
 * An SVG in the document is painted by the page's stylesheets. A rasterised copy
 * is not: the browser loads the serialised markup as an image, in a context with
 * no access to this page's CSS, so anything left to a stylesheet comes out
 * black-on-transparent. Every painted property is therefore resolved with
 * getComputedStyle and written onto the clone as an attribute, and the theme's
 * own background is painted in behind it, because a transparent PNG pasted into
 * a dark chat window loses all its dark text.
 */

import { el } from '../dom.js';
import * as icons from '../icons.js';

const PAINTED = [
  'fill',
  'fill-opacity',
  'stroke',
  'stroke-width',
  'stroke-opacity',
  'stroke-dasharray',
  'stroke-linecap',
  'stroke-linejoin',
  'opacity',
  'font-family',
  'font-size',
  'font-weight',
  'letter-spacing',
  'text-anchor',
  'dominant-baseline',
  'marker-end',
  'marker-start',
  'visibility',
  'display',
];

// Rasterise above CSS pixels, so the copy survives being viewed on a dense
// screen or scaled up in a document.
const SCALE = 2;

// Browsers refuse to allocate a canvas past a few thousand pixels a side, and a
// fully expanded diagram can ask for more than that. Whole-diagram copies scale
// down to fit rather than coming back empty.
const MAX_CANVAS_PX = 8192;

// Breathing room around a diagram framed by its own contents, so nodes at the
// edge are not shaved by the crop.
const CONTENT_PADDING = 24;

const RESET_AFTER_MS = 1600;

/** Resolves a theme token to the concrete colour the copy has to bake in. */
export function token(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

/**
 * @param content - optional live descendant whose own bounding box frames the
 *   copy. Pass it for a panned and zoomed canvas: the copy is then the whole
 *   diagram rather than the part that happened to be on screen.
 */
export async function copySvgAsImage(source, { background, content } = {}) {
  if (typeof ClipboardItem === 'undefined' || !navigator.clipboard?.write) {
    throw new Error('This browser cannot put an image on the clipboard');
  }
  const png = await renderPNG(source, { background, content });
  await navigator.clipboard.write([new ClipboardItem({ 'image/png': png })]);
}

async function renderPNG(source, { background, content }) {
  const clone = source.cloneNode(true);
  const [live, copies] = elementPairs(source, clone);
  inlinePaintedStyles(live, copies);

  const frame = contentFrame(content) || viewportFrame(source);
  const width = frame.width;
  const height = frame.height;

  if (content) {
    // The camera lives on this element as a transform. Dropping it on the clone
    // is what turns "what I am looking at" into "the whole thing".
    const twin = copies[live.indexOf(content)];
    twin?.removeAttribute('transform');
  }

  clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
  clone.setAttribute('width', String(width));
  clone.setAttribute('height', String(height));
  clone.setAttribute('viewBox', `${frame.x} ${frame.y} ${width} ${height}`);

  if (background) {
    // Sized from the frame rather than in percentages: the viewBox is offset for
    // a content-framed copy, and percentages would put the fill somewhere else.
    const fill = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
    fill.setAttribute('x', String(frame.x));
    fill.setAttribute('y', String(frame.y));
    fill.setAttribute('width', String(width));
    fill.setAttribute('height', String(height));
    fill.setAttribute('fill', background);
    clone.insertBefore(fill, clone.firstChild);
  }

  const markup = new XMLSerializer().serializeToString(clone);
  const url = URL.createObjectURL(new Blob([markup], { type: 'image/svg+xml;charset=utf-8' }));
  try {
    const image = await loadImage(url);
    const scale = Math.min(SCALE, MAX_CANVAS_PX / width, MAX_CANVAS_PX / height);
    const canvas = document.createElement('canvas');
    canvas.width = Math.round(width * scale);
    canvas.height = Math.round(height * scale);
    const context = canvas.getContext('2d');
    context.scale(scale, scale);
    context.drawImage(image, 0, 0, width, height);
    return await canvasBlob(canvas);
  } finally {
    URL.revokeObjectURL(url);
  }
}

/** What the diagram occupies, in its own coordinates, camera ignored. */
function contentFrame(content) {
  if (!content) return null;
  const box = content.getBBox();
  if (!(box.width > 0) || !(box.height > 0)) return null;
  return {
    x: Math.floor(box.x - CONTENT_PADDING),
    y: Math.floor(box.y - CONTENT_PADDING),
    width: Math.ceil(box.width + CONTENT_PADDING * 2),
    height: Math.ceil(box.height + CONTENT_PADDING * 2),
  };
}

function viewportFrame(source) {
  const viewBox = source.viewBox?.baseVal;
  if (viewBox?.width > 0) {
    return { x: viewBox.x, y: viewBox.y, width: viewBox.width, height: viewBox.height };
  }
  const bounds = source.getBoundingClientRect();
  return { x: 0, y: 0, width: Math.max(Math.round(bounds.width), 1), height: Math.max(Math.round(bounds.height), 1) };
}

/**
 * The original and the clone, element for element. querySelectorAll returns
 * document order for both and the clone is a deep copy, so index i is the same
 * element in each.
 */
function elementPairs(source, clone) {
  return [
    [source, ...source.querySelectorAll('*')],
    [clone, ...clone.querySelectorAll('*')],
  ];
}

function inlinePaintedStyles(live, copies) {
  for (let i = 0; i < live.length; i++) {
    const computed = getComputedStyle(live[i]);
    for (const property of PAINTED) {
      const value = computed.getPropertyValue(property);
      if (value) copies[i].setAttribute(property, value);
    }
  }
}

function loadImage(url) {
  return new Promise((resolve, reject) => {
    const image = new Image();
    image.onload = () => resolve(image);
    image.onerror = () => reject(new Error('The diagram could not be rasterised'));
    image.src = url;
  });
}

function canvasBlob(canvas) {
  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error('The image could not be encoded'))), 'image/png');
  });
}

/**
 * A button that copies whatever getSource() returns at click time, so a chart
 * that is re-rendered underneath it needs no rewiring.
 *
 * The outcome lands on the button itself. A copy that silently failed and a copy
 * that worked are otherwise indistinguishable, the clipboard being somewhere the
 * page cannot look.
 */
export function createCopyButton({ getSource, getContent, background, title = 'Copy as image' }) {
  let timer = null;

  const button = el(
    'button',
    { class: 'button copy', type: 'button', title, 'aria-label': title, on: { click: run } },
    icons.copy(),
  );

  function settle(text, state) {
    button.title = text;
    button.dataset.state = state;
    clearTimeout(timer);
    timer = setTimeout(() => {
      button.title = title;
      delete button.dataset.state;
    }, RESET_AFTER_MS);
  }

  async function run() {
    const source = getSource();
    if (!source) return;
    try {
      await copySvgAsImage(source, {
        background: typeof background === 'function' ? background() : background,
        content: getContent?.(),
      });
      settle('Copied', 'done');
    } catch (error) {
      settle('Failed', 'failed');
      button.title = String(error.message || error);
    }
  }

  return button;
}

export function createTextCopyButton({ getText, title = 'Copy data' }) {
  let timer = null;

  const button = el(
    'button',
    { class: 'button copy', type: 'button', title, 'aria-label': title, on: { click: run } },
    icons.copy(),
  );

  function settle(text, state) {
    button.title = text;
    button.dataset.state = state;
    clearTimeout(timer);
    timer = setTimeout(() => {
      button.title = title;
      delete button.dataset.state;
    }, RESET_AFTER_MS);
  }

  async function run() {
    const text = getText?.();
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      settle('Copied', 'done');
    } catch (error) {
      settle('Failed', 'failed');
      button.title = String(error.message || error);
    }
  }

  return button;
}
