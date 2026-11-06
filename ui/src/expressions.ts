import type { Path } from './types';
export interface Reference {
  scope: 'jobs' | 'needs' | 'steps' | 'inputs' | 'secrets'; id: string;
  step?: string; output?: string; tail?: string; offset: number;
}
export function mapReferences(text: string, callback: (ref: Reference) => string | undefined, bareExpression = false): string {
  const mapBody = (body: string, offset = 0): string => {
    const expression = body.trim();
    const leading = body.length - body.trimStart().length;
    const mapOperand = (): string => {
      // Match the runner's operands so structured keys need not be identifiers.
      let quote = '', escaped = false;
      for (let index = 0; index < expression.length; index++) {
        const character = expression[index];
        if (quote) {
          if (escaped) escaped = false;
          else if (character === '\\') escaped = true;
          else if (character === quote) quote = '';
        } else if (character === '"' || character === "'") quote = character;
        else if (expression.startsWith(' and ', index)) return mapBody(expression.slice(0, index), offset + leading) + ' and ' + mapBody(expression.slice(index + 5), offset + leading + index + 5);
      }
      if (/^(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')$/.test(expression)) return expression;
      for (const operator of [' == ', ' != ', ' >= ', ' <= ', ' > ', ' < ']) {
        const index = expression.indexOf(operator);
        if (index >= 0) return mapBody(expression.slice(0, index), offset + leading) + operator + mapBody(expression.slice(index + operator.length), offset + leading + index + operator.length);
      }
      if (expression.startsWith('not ')) return 'not ' + mapBody(expression.slice(4), offset + leading + 4);
      const match = /^(jobs|needs|steps|inputs|secrets)\.([A-Za-z_][A-Za-z0-9_-]*)(\.[\s\S]*)?$/.exec(expression);
      if (!match) return expression;
      const [, scope, id, suffix = ''] = match;
      const parts = suffix.split('.').slice(1);
      let step: string | undefined;
      let output: string | undefined;
      if ((scope === 'jobs' || scope === 'needs') && parts[0] === 'steps' && parts.length >= 2) { parts.shift(); step = parts.shift(); }
      if (parts[0] === 'outputs' && parts.length >= 2) { parts.shift(); output = parts.shift(); }
      return callback({ scope: scope as Reference['scope'], id, step, output, tail: parts.length ? '.' + parts.join('.') : undefined, offset: offset + leading }) ?? expression;
    };
    return body.slice(0, leading) + mapOperand() + body.slice(leading + expression.length);
  };
  // Conditions and repeat.until also accept expressions without interpolation
  // delimiters. Other strings are prose unless enclosed in ${{ ... }}.
  if (bareExpression && !text.trim().startsWith('${{')) return mapBody(text);
  return text.replace(/\$\{\{([\s\S]*?)\}\}/g, (_expression: string, body: string) => {
    return '${{' + mapBody(body) + '}}';
  });
}
export function referenceText(ref: Pick<Reference, 'scope' | 'id' | 'step' | 'output' | 'tail'>): string {
  return `${ref.scope}.${ref.id}${ref.step === undefined ? '' : `.steps.${ref.step}`}${ref.output === undefined ? '' : `.outputs.${ref.output}`}${ref.tail ?? ''}`;
}
export const isConciseStepReference = (ref: Reference) => ref.scope === 'steps' && ref.output === undefined && !!ref.tail && !/^\.(outputs|status|error)(\.|$)/.test(ref.tail);
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
