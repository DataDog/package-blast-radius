/**
 * Inline SVG icons.
 *
 * Drawn here rather than pulled from a font or a CDN: the viewer ships as one
 * self-contained binary and must render with no network. Every icon is a
 * 16-unit box stroked with currentColor, so size and colour come from CSS.
 */

import { svg } from './dom.js';

function icon(...children) {
  return svg(
    'svg',
    {
      viewBox: '0 0 16 16',
      fill: 'none',
      stroke: 'currentColor',
      'stroke-width': '1.5',
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      'aria-hidden': 'true',
      focusable: 'false',
    },
    ...children,
  );
}

export const search = () => icon(svg('circle', { cx: '7', cy: '7', r: '4.5' }), svg('path', { d: 'M10.5 10.5 14 14' }));

export const close = () => icon(svg('path', { d: 'M4 4l8 8M12 4l-8 8' }));

export const chevronDown = () => icon(svg('path', { d: 'M4 6.5 8 10.5l4-4' }));

export const download = () =>
  icon(svg('path', { d: 'M8 2v8M4.5 7l3.5 3 3.5-3M2.5 13h11' }));

export const sun = () =>
  icon(
    svg('circle', { cx: '8', cy: '8', r: '3' }),
    svg('path', { d: 'M8 1v1.5M8 13.5V15M1 8h1.5M13.5 8H15M3.1 3.1l1 1M11.9 11.9l1 1M12.9 3.1l-1 1M4.1 11.9l-1 1' }),
  );

export const moon = () => icon(svg('path', { d: 'M13.5 9.4A5.8 5.8 0 0 1 6.6 2.5a5.8 5.8 0 1 0 6.9 6.9z' }));

export const box = () =>
  icon(svg('path', { d: 'M8 1.8 13.5 4.6v6.8L8 14.2 2.5 11.4V4.6z' }), svg('path', { d: 'M2.5 4.6 8 7.4l5.5-2.8M8 7.4v6.8' }));

export const nodes = () =>
  icon(
    svg('circle', { cx: '3.5', cy: '8', r: '1.8' }),
    svg('circle', { cx: '12.5', cy: '4', r: '1.8' }),
    svg('circle', { cx: '12.5', cy: '12', r: '1.8' }),
    svg('path', { d: 'M5.1 7.2 10.9 4.6M5.1 8.8l5.8 2.6' }),
  );

export const target = () =>
  icon(svg('circle', { cx: '8', cy: '8', r: '5.5' }), svg('circle', { cx: '8', cy: '8', r: '1.8' }));

export const rows = () => icon(svg('path', { d: 'M2.5 4h11M2.5 8h11M2.5 12h11' }));

export const chart = () => icon(svg('path', { d: 'M2.5 13.5v-4M6.5 13.5V6M10.5 13.5V9M14 13.5V3' }));

export const external = () =>
  icon(svg('path', { d: 'M9.5 2.5H13.5V6.5M13.5 2.5 7.5 8.5' }), svg('path', { d: 'M12 10v3.5H2.5V4H6' }));

export const copy = () =>
  icon(
    svg('rect', { x: '6', y: '6', width: '7.5', height: '7.5', rx: '1.5' }),
    svg('path', { d: 'M10 3.8a1.5 1.5 0 0 0-1.3-1.3H4a1.5 1.5 0 0 0-1.5 1.5v4.7a1.5 1.5 0 0 0 1.3 1.3' }),
  );
