import type { Path } from './types';
export interface Reference {
  scope: 'jobs' | 'needs' | 'steps' | 'inputs' | 'secrets'; id: string;
  step?: string; output?: string; tail?: string; offset: number;
}
export function mapReferences(text: string, callback: (ref: Reference) => string | undefined, bareExpression = false): string {
  const mapBody = (body: string): string => {
    const result: string[] = [];
    const comparisons = [' == ', ' != ', ' >= ', ' <= ', ' > ', ' < '];
    const positions = new Map(comparisons.map(operator => [operator, [] as number[]]));
    const boundaries: { offset: number; token: string; escaped: boolean }[] = [];
    for (const match of body.matchAll(/(?=(["']| and | == | != | >= | <= | > | < ))/g)) {
      const token = match[1], offset = match.index;
      if (positions.has(token)) { positions.get(token)!.push(offset); continue; }
      let backslashes = 0;
      if (token !== ' and ') for (let cursor = offset - 1; cursor >= 0 && body[cursor] === '\\'; cursor--) backslashes++;
      boundaries.push({ offset, token, escaped: backslashes % 2 === 1 });
    }
    const lowerBound = (length: number, offsetAt: (index: number) => number, offset: number) => {
      let low = 0, high = length;
      while (low < high) { const middle = (low + high) >>> 1; if (offsetAt(middle) < offset) low = middle + 1; else high = middle; }
      return low;
    };
    // A comparison can reset quote boundaries. Index AND for every starting
    // quote state so each resulting operand can be inspected without rescanning.
    const nextAnd = Array.from({ length: 3 }, () => new Int32Array(boundaries.length + 1).fill(-1));
    for (let index = boundaries.length - 1; index >= 0; index--) {
      const boundary = boundaries[index];
      for (let state = 0; state < 3; state++) {
        if (state === 0 && boundary.token === ' and ') { nextAnd[state][index] = boundary.offset; continue; }
        const quote = boundary.token === '"' ? 1 : boundary.token === "'" ? 2 : 0;
        const nextState = quote && !state ? quote : quote === state && !boundary.escaped ? 0 : state;
        nextAnd[state][index] = nextAnd[nextState][index + 1];
      }
    }
    // Flat operator chains use a worklist instead of growing the call stack.
    const pending: (string | { body: string; offset: number; scanAnd: boolean; scanComparison: boolean })[] = [{ body, offset: 0, scanAnd: true, scanComparison: true }];
    while (pending.length) {
      const item = pending.pop()!;
      if (typeof item === 'string') { result.push(item); continue; }
      const expression = item.body.trim();
      const leading = item.body.length - item.body.trimStart().length;
      const offset = item.offset + leading;
      result.push(item.body.slice(0, leading));
      const trailing = item.body.slice(leading + expression.length);
      if (trailing) pending.push(trailing);
      // Match the runner's operands so structured keys need not be identifiers.
      const end = offset + expression.length;
      const boundary = lowerBound(boundaries.length, index => boundaries[index].offset, offset);
      const and = item.scanAnd ? nextAnd[0][boundary] : -1;
      let index = and >= offset && and + 5 <= end ? and - offset : -1;
      let operator = index >= 0 ? ' and ' : '';
      if (index < 0 && item.scanComparison) for (const candidate of comparisons) {
        const offsets = positions.get(candidate)!;
        const found = offsets[lowerBound(offsets.length, index => offsets[index], offset)];
        if (found !== undefined && found + candidate.length <= end) { index = found - offset; operator = candidate; break; }
      }
      if (index >= 0) {
        pending.push(
          { body: expression.slice(index + operator.length), offset: offset + index + operator.length, scanAnd: true, scanComparison: true },
          operator,
          { body: expression.slice(0, index), offset, scanAnd: true, scanComparison: true },
        );
        continue;
      }
      if (expression.startsWith('not ')) {
        result.push('not ');
        // Removing this prefix cannot introduce either operator already ruled out.
        pending.push({ body: expression.slice(4), offset: offset + 4, scanAnd: false, scanComparison: false });
        continue;
      }
      const match = /^(jobs|needs|steps|inputs|secrets)\.([A-Za-z_][A-Za-z0-9_-]*)(\.[\s\S]*)?$/.exec(expression);
      if (!match) { result.push(expression); continue; }
      const [, scope, id, suffix = ''] = match;
      const parts = suffix.split('.').slice(1);
      let step: string | undefined;
      let output: string | undefined;
      if ((scope === 'jobs' || scope === 'needs') && parts[0] === 'steps' && parts.length >= 2) { parts.shift(); step = parts.shift(); }
      if (parts[0] === 'outputs' && parts.length >= 2) { parts.shift(); output = parts.shift(); }
      result.push(callback({ scope: scope as Reference['scope'], id, step, output, tail: parts.length ? '.' + parts.join('.') : undefined, offset }) ?? expression);
    }
    return result.join('');
  };
  // Conditions and repeat.until also accept expressions without interpolation
  // delimiters. Other strings are prose unless enclosed in ${{ ... }}.
  if (bareExpression) {
    const trimmed = text.trim();
    if (trimmed.startsWith('${{') && trimmed.endsWith('}}')) {
      const start = text.length - text.trimStart().length + 3;
      const end = text.trimEnd().length - 2;
      return text.slice(0, start) + mapBody(text.slice(start, end)) + text.slice(end);
    }
    return mapBody(text);
  }
  return text.replace(/\$\{\{([\s\S]*?)\}\}/g, (_expression: string, body: string) => {
    return '${{' + mapBody(body) + '}}';
  });
}
export function referenceText(ref: Pick<Reference, 'scope' | 'id' | 'step' | 'output' | 'tail'>): string {
  return `${ref.scope}.${ref.id}${ref.step === undefined ? '' : `.steps.${ref.step}`}${ref.output === undefined ? '' : `.outputs.${ref.output}`}${ref.tail ?? ''}`;
}
export const isConciseStepReference = (ref: Reference) => ref.scope === 'steps' && ref.output === undefined && !!ref.tail && ref.tail !== '.' && !ref.tail.startsWith('..') && !/^\.(outputs|status|error)(\.|$)/.test(ref.tail);
export function references(text: string, bareExpression = false) {
  const result: Reference[] = [];
  mapReferences(text, ref => { result.push(ref); return undefined; }, bareExpression);
  return result;
}
export function pathJob(path: Path): string | undefined { return path[0] === 'jobs' ? String(path[1]) : undefined; }
export function isExpressionPath(path: Path): boolean {
  return path[0] === 'jobs' && (path.length === 3 && path[2] === 'if'
    || path.length === 4 && path[2] === 'for-each' && path[3] === 'key'
    || path.length === 4 && path[2] === 'repeat' && path[3] === 'until'
    || path.length === 5 && path[2] === 'steps' && path[4] === 'if');
}
// Keep literal metadata and callable defaults intact. These are the value
// fields rendered by executor.go and command.go; with mappings are recursive,
// including named schema values expanded into with.output before renderMap.
export function hasExpressionReferences(path: Path): boolean {
  if (isExpressionPath(path)) return true;
  if (path[0] === 'schemas' && path.length >= 3) return true;
  if (path.length === 5 && path[0] === 'on' && path[1] === 'workflow_call' && path[2] === 'outputs' && path[4] === 'value') return true;
  if (path[0] !== 'jobs') return false;
  // Array item payloads are literal data. Only the whole items expression renders.
  if (path.length === 4 && path[2] === 'for-each' && path[3] === 'items') return true;
  if ((path[2] === 'with' || path[2] === 'secrets') && path.length >= 4) return true;
  if (path[2] === 'outputs' && path.length === 4) return true;
  if (path[2] !== 'steps' || typeof path[3] !== 'number') return false;
  return (path[4] === 'with' && path.length >= 6)
    || (path[4] === 'argv' && path.length === 6 && typeof path[5] === 'number')
    || (path.length === 5 && (path[4] === 'run' || path[4] === 'working-directory' || path[4] === 'prompt'));
}
