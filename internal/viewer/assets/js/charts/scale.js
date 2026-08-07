/**
 * The arithmetic a bar-and-line chart needs, and nothing else.
 *
 * This is the whole of what a charting library would be pulled in for here, so
 * it lives apart from any one chart: a second chart reuses it, and a later
 * decision to vendor a real library replaces this file rather than a view.
 */

/**
 * Evenly spaced slots across a width, with a gap between them.
 *
 * `start` is a bar's left edge and `center` the slot's midpoint, which is where
 * labels, dots and line vertices go.
 *
 * `maxStep` caps how wide one slot may get and centres the group in whatever is
 * left. Without it a chart of two or three bars divides the whole width between
 * them and draws slabs a third of the plot wide, which reads as a different kind
 * of chart rather than as the same chart with less in it.
 */
export function bandScale(slots, width, { padding = 0.32, maxStep = Infinity } = {}) {
  const step = Math.min(slots > 0 ? width / slots : width, maxStep);
  const bandwidth = step * (1 - padding);
  const inset = (step - bandwidth) / 2;
  const offset = Math.max((width - step * slots) / 2, 0);
  return {
    step,
    bandwidth,
    start: (i) => offset + i * step + inset,
    center: (i) => offset + i * step + step / 2,
  };
}

/** Maps 0..max onto a height, inverted: SVG y grows downward. */
export function linearScale(max, height) {
  return (value) => (max <= 0 ? height : height - (value / max) * height);
}

/**
 * Tick values from 0 to max on a round step. `wanted` is a target rather than a
 * promise: the step is rounded up to something readable, so the count can come
 * out one either side.
 */
export function ticks(max, wanted = 5) {
  if (!(max > 0) || !(wanted > 0)) return [0];

  const rough = max / wanted;
  const magnitude = Math.pow(10, Math.floor(Math.log10(rough)));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * magnitude).find((s) => s >= rough) ?? magnitude * 10;

  const out = [];
  // Half a step of slack, so a max that lands exactly on a tick still gets it
  // despite floating-point drift.
  for (let value = 0; value <= max + step / 2; value += step) {
    out.push(Math.round(value * 1e6) / 1e6);
  }
  return out;
}
