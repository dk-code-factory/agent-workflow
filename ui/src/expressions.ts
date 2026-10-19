import type { Path } from './types';
export interface Reference { scope: 'jobs' | 'needs' | 'steps' | 'inputs' | 'secrets'; id: string; output?: string; offset: number }
export function mapReferences(text: string, callback: (ref: Reference) => string | undefined, bareExpression = false): string {
  const mapBody = (body: string) => body.replace(/"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|(?<![\w.-])(jobs|needs|steps|inputs|secrets)\.([A-Za-z_][A-Za-z0-9_-]*)(?:\.outputs\.([A-Za-z_][A-Za-z0-9_-]*))?/g,
      (token: string, scope: Reference['scope'] | undefined, id: string, output: string | undefined, offset: number) => scope ? callback({ scope, id, output, offset }) ?? token : token);
  // Conditions and repeat.until also accept expressions without interpolation
  // delimiters. Other strings are prose unless enclosed in ${{ ... }}.
  if (bareExpression && !text.trim().startsWith('${{')) return mapBody(text);
  return text.replace(/\$\{\{([\s\S]*?)\}\}/g, (_expression: string, body: string) => {
    return '${{' + mapBody(body) + '}}';
  });
}
export function referenceText(ref: Pick<Reference, 'scope' | 'id' | 'output'>): string {
  return `${ref.scope}.${ref.id}${ref.output === undefined ? '' : `.outputs.${ref.output}`}`;
}
export function references(text: string, bareExpression = false) {
  const result: Reference[] = [];
  mapReferences(text, ref => { result.push(ref); return undefined; }, bareExpression);
  return result;
}
export function pathJob(path: Path): string | undefined { return path[0] === 'jobs' ? String(path[1]) : undefined; }
export function isExpressionPath(path: Path): boolean {
  return path[0] === 'jobs' && (path.length === 3 && path[2] === 'if'
    || path.length === 4 && path[2] === 'repeat' && path[3] === 'until'
    || path.length === 5 && path[2] === 'steps' && path[4] === 'if');
}
// Keep literal metadata and callable defaults intact. These are the value
// fields rendered by executor.go and command.go; with mappings are recursive,
// including structured-output schema values passed through renderMap.
export function hasExpressionReferences(path: Path): boolean {
  if (isExpressionPath(path)) return true;
  if (path.length === 5 && path[0] === 'on' && path[1] === 'workflow_call' && path[2] === 'outputs' && path[4] === 'value') return true;
  if (path[0] !== 'jobs') return false;
  if ((path[2] === 'with' || path[2] === 'secrets') && path.length >= 4) return true;
  if (path[2] === 'outputs' && path.length === 4) return true;
  if (path[2] !== 'steps' || typeof path[3] !== 'number') return false;
  return (path[4] === 'with' && path.length >= 6)
    || (path[4] === 'argv' && path.length === 6 && typeof path[5] === 'number')
    || (path.length === 5 && (path[4] === 'run' || path[4] === 'working-directory'));
}
