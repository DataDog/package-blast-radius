const integer = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });
const compactFormat = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });

export function count(n) {
  return integer.format(n ?? 0);
}

export function compact(n) {
  return compactFormat.format(n ?? 0);
}

/**
 * A null download count means the report was not enriched, which is not the
 * same as zero. Both render, but never as each other.
 */
export function downloads(value) {
  return value === null || value === undefined ? '—' : count(value);
}

/**
 * Two decimals at most, and no trailing zeros: a chart axis reads "80%" while a
 * share that genuinely lands between ticks keeps its precision.
 */
export function percent(value) {
  return `${Math.round((value ?? 0) * 100) / 100}%`;
}

export function plural(n, singular, plural = singular + 's') {
  return `${count(n)} ${n === 1 ? singular : plural}`;
}

/**
 * Picks one of the ramp's six stops for a distance.
 *
 * The ramp stretches to fit the report rather than clamping at six, because a
 * depth-15 run would otherwise paint two thirds of its rows the same colour.
 * Colour is never the only cue: the number is always next to it.
 */
export function depthStop(depth, maxDepth = 6) {
  const span = Math.max(1, maxDepth - 1);
  const stop = maxDepth <= 1 ? 1 : 1 + Math.round(((Math.max(depth, 1) - 1) / span) * 5);
  return String(Math.min(Math.max(stop, 1), 6));
}

export function packageURL(system, name) {
  switch ((system || '').toUpperCase()) {
    case 'PYPI':
      return 'https://pypi.org/project/' + encodeURIComponent(name) + '/';
    case 'NPM':
    default:
      return 'https://www.npmjs.com/package/' + name.split('/').map(encodeURIComponent).join('/');
  }
}

export function registryLabel(system) {
  switch ((system || '').toUpperCase()) {
    case 'PYPI':
      return 'PyPI';
    case 'NPM':
    default:
      return 'npm';
  }
}

/** "axios@1.14.1" -> "axios". Scoped names start with @, so split on the last. */
export function targetName(ref) {
  const at = ref.lastIndexOf('@');
  return at > 0 ? ref.slice(0, at) : ref;
}
