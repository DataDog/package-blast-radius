/**
 * Minimal DOM construction.
 *
 * Everything the viewer renders comes from a registry: package names, version
 * strings, declared ranges. None of it is trusted, so nothing in this module
 * accepts markup. Text always goes through textContent, and attributes are set
 * one at a time rather than serialized into a string.
 */

/**
 * el('div', {class: 'row', on: {click: fn}}, 'text', el('span', {}, '!'))
 *
 * Props are DOM properties by default. Two keys are special: `on` is a map of
 * event listeners, and `dataset` a map of data attributes. Keys containing a
 * dash are set as attributes, which covers aria-* and the rest.
 *
 * Children may be nodes, strings, numbers, or arrays of them. null, undefined
 * and false are skipped, so `cond && el(...)` works inline.
 */
export function el(tag, props, ...children) {
  const node = document.createElement(tag);

  for (const [key, value] of Object.entries(props || {})) {
    if (value === null || value === undefined) continue;
    if (key === 'on') {
      for (const [event, handler] of Object.entries(value)) {
        node.addEventListener(event, handler);
      }
    } else if (key === 'dataset') {
      Object.assign(node.dataset, value);
    } else if (key === 'class') {
      node.className = value;
    } else if (key.includes('-')) {
      node.setAttribute(key, value);
    } else {
      setProp(node, key, value);
    }
  }

  append(node, children);
  return node;
}

/**
 * Properties win over attributes, because `value` and `checked` only behave as
 * properties. A few IDL attributes are read-only on the element though
 * (`input.list` is one), and assigning to those throws in a module's strict
 * mode, so they fall back to the attribute.
 */
function setProp(node, key, value) {
  if (isWritable(node, key)) node[key] = value;
  else node.setAttribute(key, value);
}

function isWritable(node, key) {
  for (let obj = node; obj; obj = Object.getPrototypeOf(obj)) {
    const descriptor = Object.getOwnPropertyDescriptor(obj, key);
    if (descriptor) return Boolean(descriptor.writable || descriptor.set);
  }
  return true; // not an IDL attribute, so it is a plain expando
}

export function append(parent, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    parent.appendChild(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return parent;
}

export function frag(...children) {
  return append(document.createDocumentFragment(), children);
}

/** Replaces a node's contents. */
export function replace(parent, ...children) {
  parent.replaceChildren();
  return append(parent, children);
}

export function svg(tag, props, ...children) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === null || value === undefined) continue;
    if (key === 'on') {
      for (const [event, handler] of Object.entries(value)) node.addEventListener(event, handler);
    } else {
      node.setAttribute(key, value);
    }
  }
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    node.appendChild(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

export function $(selector, root = document) {
  return root.querySelector(selector);
}

export function loadingState(message = 'Loading…') {
  return el('div', { class: 'state' }, el('div', { class: 'spinner' }), el('div', {}, message));
}

export function emptyState(title, detail) {
  return el(
    'div',
    { class: 'state' },
    el('div', { class: 'state__title' }, title),
    detail && el('div', {}, detail),
  );
}

export function errorState(error) {
  return el(
    'div',
    { class: 'state state--error' },
    el('div', { class: 'state__title' }, 'Something went wrong'),
    el('div', {}, String(error && error.message ? error.message : error)),
  );
}

/**
 * Coalesces bursts of calls into one, so typing in the search box issues one
 * request rather than one per keystroke.
 */
export function debounce(fn, ms = 250) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), ms);
  };
}
