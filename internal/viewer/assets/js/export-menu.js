/**
 * The export control: one button, two ways out of it.
 *
 * A CSV of the current filter is either a file you keep or something you paste
 * straight into a ticket, and only one of those is a download. Both are the
 * same bytes the server already streams, so the copy reads the download URL
 * rather than re-deriving the rows in the browser.
 *
 * The outcome of a copy lands on the trigger. A copy that silently failed and
 * one that worked are otherwise indistinguishable, the clipboard being
 * somewhere the page cannot look.
 */

import { el } from './dom.js';
import * as icons from './icons.js';

const RESET_AFTER_MS = 1600;

export function createExportMenu({ getURL }) {
  let timer = null;

  const label = el('span', {}, 'Export');
  const trigger = el(
    'button',
    {
      class: 'button button--primary menu__trigger',
      type: 'button',
      'aria-haspopup': 'menu',
      'aria-expanded': 'false',
      on: { click: () => (isOpen() ? close() : open()) },
    },
    icons.download(),
    label,
    icons.chevronDown(),
  );

  const downloadItem = el(
    'a',
    { class: 'menu__item', role: 'menuitem', href: '#', download: '', on: { click: () => close() } },
    'Download CSV',
  );

  const copyItem = el(
    'button',
    { class: 'menu__item', role: 'menuitem', type: 'button', on: { click: copy } },
    'Copy CSV to clipboard',
  );

  const list = el('div', { class: 'menu__list', role: 'menu', hidden: true }, downloadItem, copyItem);
  const element = el('div', { class: 'menu' }, trigger, list);

  const isOpen = () => !list.hidden;

  function open() {
    // The href is only ever read on the way out, so it is resolved here rather
    // than kept in step with every filter change.
    downloadItem.href = getURL();
    list.hidden = false;
    trigger.setAttribute('aria-expanded', 'true');
    document.addEventListener('pointerdown', onOutsidePointer, true);
    document.addEventListener('keydown', onKeydown, true);
    downloadItem.focus();
  }

  function close({ refocus = false } = {}) {
    if (!isOpen()) return;
    list.hidden = true;
    trigger.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', onOutsidePointer, true);
    document.removeEventListener('keydown', onKeydown, true);
    if (refocus) trigger.focus();
  }

  function onOutsidePointer(event) {
    if (!element.contains(event.target)) close();
  }

  function onKeydown(event) {
    if (event.key === 'Escape') {
      event.stopPropagation();
      close({ refocus: true });
      return;
    }
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
    event.preventDefault();
    const items = [downloadItem, copyItem];
    const step = event.key === 'ArrowDown' ? 1 : -1;
    const at = items.indexOf(document.activeElement);
    items[(at + step + items.length) % items.length].focus();
  }

  function settle(text, state) {
    label.textContent = text;
    trigger.dataset.state = state;
    clearTimeout(timer);
    timer = setTimeout(() => {
      label.textContent = 'Export';
      delete trigger.dataset.state;
    }, RESET_AFTER_MS);
  }

  async function copy() {
    close({ refocus: true });
    label.textContent = 'Copying…';
    try {
      if (!navigator.clipboard?.writeText) throw new Error('This browser has no clipboard access');
      const response = await fetch(getURL());
      if (!response.ok) throw new Error(`The export failed with HTTP ${response.status}`);
      await navigator.clipboard.writeText(await response.text());
      settle('Copied', 'done');
    } catch (error) {
      settle('Copy failed', 'failed');
      trigger.title = String(error.message || error);
    }
  }

  return element;
}
