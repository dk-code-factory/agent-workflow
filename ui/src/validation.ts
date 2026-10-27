import { isAlias, isMap, isNode, isScalar, isSeq, LineCounter, parseAllDocuments, type Document, type Node } from 'yaml';
import { hasExpressionReferences, isExpressionPath, references } from './expressions';
import type { Diagnostic, Job, Mapping, Path, Workflow } from './types';

export function diagnosticLabel(value: string | number): string {
  const text = String(value);
  return text.length > 80 ? text.slice(0, 77) + '…' : text;
}

export const identifier = /^[A-Za-z_][A-Za-z0-9_-]*$/;
export const controllerParameters: Record<string, string[]> = {
  'workspace/create': ['commit', 'independent'], 'workspace/snapshot': ['commit'],
  'workspace/integrate': ['path', 'commits'], 'git/commit': ['path', 'message'],
  'github/pr': ['path', 'commit', 'title', 'body'],
};
export const dependencies = (job: Job): string[] => (Array.isArray(job.needs) ? job.needs : job.needs ? [job.needs] : []).map(s => s.trim()).filter(Boolean);
export const effectiveStepID = (step: Mapping, index: number) => String(step.id || `step_${index + 1}`);
export const mapping = (value: unknown): value is Mapping => value !== null && typeof value === 'object' && !Array.isArray(value);

// yaml.v3 decodes an int from YAML integer scalars or truncates floats toward
// zero. Its resolver also recognizes the YAML 1.1 leading-zero octal form.
function runnerInteger(value: unknown, source?: string): number | undefined {
  if (typeof value !== 'number') return undefined;
  const spelling = source?.replaceAll('_', '') ?? '';
  const match = /^([+-]?)(0[xX][0-9a-fA-F]+|0[oO][0-7]+|0[bB][01]+|[0-9]+)$/.exec(spelling);
  if (match) {
    const [, sign, digits] = match;
    const magnitude = /^0[0-7]+$/.test(digits) ? BigInt('0o' + digits.slice(1)) : BigInt(digits);
    const integer = sign === '-' ? -magnitude : magnitude;
    if (integer >= -(1n << 63n) && integer <= (1n << 63n) - 1n) return Number(integer);
    // Negative overflow resolves as a float in yaml.v3; Go's int64 conversion
    // produces MinInt64. Positive uint64 values overflow an int and are rejected.
    if (integer < -(1n << 63n)) return -(2 ** 63);
    return undefined;
  }
  if (Number.isNaN(value) || value > 2 ** 63) return undefined;
  if (value <= -(2 ** 63) || value === 2 ** 63) return -(2 ** 63);
  return Math.trunc(value) || 0;
}

type Shape = 'string' | 'boolean' | 'integer' | 'any' | 'list' | { [key: string]: Shape } | [Shape];
const context: Shape = { session: 'string', delivery: 'string' };
const input: Shape = { type: 'string', required: 'boolean', default: 'any' };
const entity: Shape = { ids: 'list', types: 'list', attributes: { '*': 'list' } };
const schema: Shape = {
  name: 'string', gates: { '*': 'any' },
  on: {
    manual: { '*': 'any' }, schedule: [{ cron: 'string' }],
    channel_message: { channels: 'list', chats: 'list', senders: 'list', mentioned: 'boolean', command: 'string', text_matches: 'string', passthrough: 'boolean', conversation: context },
    command: { name: 'string', channels: 'list', chats: 'list', senders: 'list', args: { '*': input }, passthrough: 'boolean', conversation: context },
    runtime_event: { kinds: 'list', sources: 'list', agents: 'list', sessions: 'list', channels: 'list', chats: 'list' },
    event: { sources: 'list', connectors: 'list', types: 'list', actor: entity, subject: entity, attributes: { '*': 'list' } },
    workflow_call: { inputs: { '*': input }, secrets: { '*': { required: 'boolean' } }, outputs: { '*': { value: 'string' } } },
  },
  jobs: { '*': {
    name: 'string', 'runs-on': 'string', needs: 'list', uses: 'string', if: 'string', 'continue-on-error': 'boolean',
    with: { '*': 'any' }, secrets: 'any', outputs: { '*': 'string' }, context,
    repeat: { 'max-attempts': 'integer', until: 'string' },
    'for-each': { items: 'any', key: 'string' },
    steps: [{ id: 'string', name: 'string', uses: 'string', if: 'string', 'continue-on-error': 'boolean', with: { '*': 'any' }, context,
      argv: ['string'], run: 'string', shell: 'string', 'working-directory': 'string', timeout: 'string' }],
  } },
};
export interface Parsed { doc?: Document; workflow?: Workflow; diagnostics: Diagnostic[]; usable: boolean }
export function parseWorkflow(source: string): Parsed {
  const diagnostics: Diagnostic[] = [];
  const lines = new LineCounter();
  let doc: Document | undefined;
  const add = (code: string, message: string, path: Path = [], node?: Node | null, sourceType: Diagnostic['source'] = 'browser', severity: Diagnostic['severity'] = 'error') => {
    const range = node?.range ?? (doc?.getIn(path, true) as Node | undefined)?.range;
    const offset = range?.[0] ?? 0;
    const pos = lines.linePos(offset);
    diagnostics.push({ code, message, path, severity, source: sourceType, location: range || code === 'size' ? { line: Math.max(1, pos.line), column: Math.max(1, pos.col), offset, endOffset: range?.[1] ?? offset } : undefined });
  };
  if (new TextEncoder().encode(source).length > 4 * 1024 * 1024) {
    add('size', 'Workflow exceeds 4 MiB', [], undefined, 'parser');
    return { diagnostics, usable: false };
  }
  try {
    const documents = parseAllDocuments(source, { lineCounter: lines, uniqueKeys: true, keepSourceTokens: true, prettyErrors: false });
    doc = documents[0];
    if (documents.length !== 1 || !doc) {
      add('documents', 'Expected exactly one YAML document', [], undefined, 'parser');
      return { diagnostics, usable: false };
    }
    for (const error of doc.errors) {
      const pos = lines.linePos(error.pos[0]);
      diagnostics.push({ severity: 'error', code: error.code, message: error.message, source: 'parser', path: [], location: { line: pos.line, column: pos.col, offset: error.pos[0], endOffset: error.pos[1] } });
    }
    let safe = doc.errors.length === 0;
    const walk = (node: Node | null, path: Path, depth: number) => {
      if (depth > 64) { add('depth', 'YAML nesting exceeds 64', path, node, 'parser'); safe = false; return; }
      if (isAlias(node)) { add('alias', 'YAML aliases are unsupported', path, node, 'parser'); safe = false; return; }
      if (isMap(node)) {
        const keys = new Set<string>();
        for (const pair of node.items) {
          if (!isScalar(pair.key) || pair.key.value === '<<') { add('key', 'Invalid mapping key or merge key', path, node, 'parser'); safe = false; }
          if (isScalar(pair.key)) {
            // yaml.v3 decodes map[string] keys from their scalar spelling.
            const key = typeof pair.key.value === 'string' ? pair.key.value : pair.key.source ?? String(pair.key.value);
            if (keys.has(key)) { add('duplicate-key', `Duplicate key "${key}"`, [...path, key], pair.key, 'parser'); safe = false; }
            keys.add(key);
            if (typeof pair.key.value !== 'string' && pair.key.tag) pair.key.tag = 'tag:yaml.org,2002:str';
            pair.key.value = key;
          }
          walk(pair.value as Node, [...path, String(isScalar(pair.key) ? pair.key.value : '?')], depth + 1);
        }
      }
      if (isSeq(node)) node.items.forEach((item, i) => walk(item as Node, [...path, i], depth + 1));
    };
    walk(doc.contents, [], 0);
    if (!safe) return { doc, diagnostics, usable: false };
    const value = doc.toJS({ maxAliasCount: 0 }) as unknown;
    const valueRoot = value;
    const assign = (path: Path, next: unknown) => {
      let parent = valueRoot as Mapping;
      for (const key of path.slice(0, -1)) parent = parent[key] as Mapping;
      Object.defineProperty(parent, path[path.length - 1], { value: next, writable: true, configurable: true, enumerable: true });
    };
    const scalarString = (item: unknown, path: Path) => {
      const node = doc!.getIn(path, true);
      const text = typeof item === 'string' ? item : isScalar(node) ? node.source ?? String(item) : String(item);
      // Subsequent visual edits serialize this tree. Keep string semantics even
      // for YAML booleans/numbers whose canonical spelling differs (TRUE, 01).
      if (isScalar(node) && item != null) {
        node.value = text;
        if (typeof item !== 'string' && node.tag) node.tag = 'tag:yaml.org,2002:str';
      }
      return text;
    };
    let shaped = true;
    const checkShape = (value: unknown, shape: Shape, path: Path) => {
      if (shape === 'any') return;
      if (value == null) {
        // Null map/struct values decode to Go zero values. Optional pointers
        // remain absent; other mappings become editable empty collections.
        const pointer = (path[0] === 'on' && path.length === 2 && ['workflow_call', 'channel_message', 'command', 'runtime_event', 'event'].includes(String(path[1])))
          || (path[0] === 'jobs' && path.length === 3 && ['repeat', 'for-each'].includes(String(path[2])))
          || (path[0] === 'on' && path[1] === 'event' && path.length === 3 && ['actor', 'subject'].includes(String(path[2])));
        if (!path.length || pointer) return;
        value = shape === 'string' ? '' : shape === 'boolean' ? false : shape === 'integer' ? 0
          : shape === 'list' || Array.isArray(shape) ? [] : {};
        assign(path, value);
        const previous = doc!.getIn(path, true);
        const replacement = doc!.createNode(value);
        if (isNode(previous)) {
          replacement.commentBefore = previous.commentBefore;
          replacement.comment = previous.comment;
          replacement.spaceBefore = previous.spaceBefore;
          replacement.range = previous.range;
        }
        doc!.setIn(path, replacement);
      }
      let valid = true;
      if (typeof shape === 'string') {
        if (shape === 'list') {
          // Go StringList accepts scalar values decoded as strings, trims them,
          // and discards empty/null entries. Use YAML scalar text for numbers
          // so IDs such as 01 and 0xA retain the runner's spelling.
          const items = Array.isArray(value) ? value : [value];
          valid = items.every(item => item == null || ['string', 'number', 'boolean'].includes(typeof item));
          if (valid) {
            const normalized = items.map((item, index) => {
              if (item == null) return '';
              return scalarString(item, Array.isArray(value) ? [...path, index] : path).trim();
            }).filter(Boolean);
            assign(path, normalized);
          }
        } else if (shape === 'string') {
          valid = ['string', 'number', 'boolean'].includes(typeof value);
          if (valid) assign(path, scalarString(value, path));
        } else if (shape === 'boolean') {
          const legacy = typeof value === 'string' && ['y', 'Y', 'yes', 'Yes', 'YES', 'on', 'On', 'ON', 'n', 'N', 'no', 'No', 'NO', 'off', 'Off', 'OFF'].includes(value);
          valid = typeof value === 'boolean' || !!legacy;
          if (legacy) {
            const decoded = ['y', 'Y', 'yes', 'Yes', 'YES', 'on', 'On', 'ON'].includes(value as string);
            assign(path, decoded);
            const node = doc!.getIn(path, true);
            if (isScalar(node)) {
              node.value = decoded;
              if (node.tag) node.tag = 'tag:yaml.org,2002:bool';
            }
          }
        } else {
          const node = doc!.getIn(path, true);
          const decoded = runnerInteger(value, isScalar(node) ? node.source : undefined);
          valid = decoded !== undefined;
          if (valid) {
            assign(path, decoded);
            // Canonicalize the parsed integer before future visual edits, so
            // leading-zero octal and fractional syntax retain runner meaning.
            if (isScalar(node)) {
              node.value = decoded;
              if (node.tag === 'tag:yaml.org,2002:float') node.minFractionDigits = Math.max(1, node.minFractionDigits ?? 0);
            }
          }
        }
      } else if (Array.isArray(shape)) {
        valid = Array.isArray(value);
        if (valid) {
          // yaml.v3 omits null elements from typed slices, including steps,
          // schedules and argv. Keep the YAML tree aligned with semantic step
          // indexes before any visual mutation, while retaining their comments.
          const items = (value as unknown[]).filter(item => item != null);
          const sequence = doc!.getIn(path, true);
          if (isSeq(sequence)) {
            let comments = '';
            sequence.items = sequence.items.filter(item => {
              if (!isNode(item) || (isScalar(item) && item.value == null)) {
                if (isNode(item)) comments += [item.commentBefore, item.comment].filter(Boolean).join('\n') + '\n';
                return false;
              }
              if (comments) { item.commentBefore = comments.trimEnd() + (item.commentBefore ? '\n' + item.commentBefore : ''); comments = ''; }
              return true;
            });
            if (comments) sequence.comment = [sequence.comment, comments.trimEnd()].filter(Boolean).join('\n');
          }
          assign(path, items);
          items.forEach((item, i) => checkShape(item, shape[0], [...path, i]));
        }
      } else {
        valid = mapping(value);
        if (mapping(value)) for (const [key, item] of Object.entries(value)) {
          const child = Object.hasOwn(shape, key) ? shape[key] : shape['*'];
          if (!child) add('unknown-field', `Unknown field "${key}"`, [...path, key], undefined, 'parser');
          else checkShape(item, child, [...path, key]);
        }
      }
      if (!valid) { shaped = false; add('shape', `Expected ${typeof shape === 'string' ? shape : Array.isArray(shape) ? 'sequence' : 'mapping'}`, path, undefined, 'parser'); }
    };
    checkShape(value, schema, []);
    if (!mapping(value) || !mapping(value.jobs) || Object.values(value.jobs).some(j => !mapping(j))) {
      add('jobs', 'Workflow requires a jobs mapping'); return { doc, diagnostics, usable: false };
    }
    if (!shaped || Object.values(value.jobs).some(j => Array.isArray((j as Job).steps) && (j as Job).steps!.some(s => !mapping(s)))) return { doc, diagnostics, usable: false };
    const workflow = value as Workflow;
    validateWorkflow(workflow, add);
    return { doc, workflow, diagnostics, usable: true };
  } catch (error) {
    add('parse', String(error), [], undefined, 'parser');
    return { diagnostics, usable: false };
  }
}

type Add = (code: string, message: string, path?: Path, node?: Node | null, source?: Diagnostic['source'], severity?: Diagnostic['severity']) => void;
function validateWorkflow(w: Workflow, add: Add) {
  const jobs = w.jobs;
  const dependencySets = new Map<string, Set<string>>();
  if (!Object.keys(jobs).length) add('jobs', 'At least one job is required', ['jobs']);
  if (mapping(w.gates) && Object.keys(w.gates).length) add('standalone', 'Gates require PicoClaw', ['gates']);
  for (const key of ['schedule', 'channel_message', 'command', 'runtime_event', 'event']) {
    if (w.on?.[key] != null && (!Array.isArray(w.on[key]) || (w.on[key] as unknown[]).length)) add('standalone', 'Standalone supports manual and workflow_call triggers only', ['on', key]);
  }
  const callable = w.on?.workflow_call as Mapping | undefined;
  if (mapping(callable?.inputs)) for (const [id, input] of Object.entries(callable.inputs)) {
    if (mapping(input) && input.type && !['string', 'boolean', 'number', 'object', 'array'].includes(String(input.type))) add('input-type', 'Unsupported input type', ['on', 'workflow_call', 'inputs', id, 'type']);
  }
  for (const [id, job] of Object.entries(jobs)) {
    const p: Path = ['jobs', id];
    if (!identifier.test(id)) add('identifier', 'Invalid job ID', p);
    if (job['runs-on'] && job['runs-on'] !== 'local') add('standalone', 'Only runs-on: local is supported', [...p, 'runs-on']);
    if (mapping(job.context) && Object.values(job.context).some(Boolean)) add('standalone', 'Session/delivery context requires PicoClaw', [...p, 'context']);
    if (job.repeat && (!Number.isInteger(job.repeat['max-attempts']) || job.repeat['max-attempts'] < 1 || job.repeat['max-attempts'] > 100 || !job.repeat.until?.trim())) add('repeat', 'Repeat requires max-attempts 1–100 and until', [...p, 'repeat']);
    const each = job['for-each'];
    if (each) {
      if (!job.uses || job.repeat || !each.key?.trim()) add('for-each', 'for-each requires a reusable workflow and key, and cannot combine with repeat', [...p, 'for-each']);
      if (!Array.isArray(each.items) && !(typeof each.items === 'string' && /^\s*\$\{\{[^}]+\}\}\s*$/.test(each.items))) add('for-each', 'for-each.items requires an array or a whole expression', [...p, 'for-each', 'items']);
      add('host-iteration', 'Ordered item identities, evidence and checkpoints require host runtime validation', [...p, 'for-each'], undefined, 'host', 'info');
    }
    const needs = dependencies(job);
    dependencySets.set(id, new Set(needs));
    for (const dep of needs) if (!Object.hasOwn(jobs, dep)) add('dependency', `Unknown dependency "${dep}"`, [...p, 'needs']);
    if (new Set(needs).size !== needs.length) add('duplicate-dependency', 'Duplicate dependency', [...p, 'needs'], undefined, 'browser', 'warning');
    if (job.uses) {
      const ref = job.uses.trim().replace(/^\.\//, '');
      if (!/^workflows\/.+\.ya?ml$/i.test(ref) || ref.split('/').some(s => !s || s === '.' || s === '..')) add('workflow-ref', 'Expected a safe local workflows/*.yml or *.yaml reference', [...p, 'uses']);
      if (job.steps?.length) add('job-kind', 'Reusable jobs cannot contain steps', p);
    } else {
      if (!job.steps?.length) add('steps', 'Job requires steps', [...p, 'steps']);
      if ((mapping(job.with) && Object.keys(job.with).length) || job.secrets != null) add('job-kind', 'with/secrets require a reusable workflow', p);
    }
    const seen = new Set<string>();
    job.steps?.forEach((step, i) => {
      const sp = [...p, 'steps', i];
      const sid = effectiveStepID(step, i);
      if (!identifier.test(sid) || seen.has(sid)) add('identifier', 'Invalid or duplicate effective step ID', [...sp, 'id']);
      seen.add(sid);
      const command = !!(step.argv?.length || step.run);
      const params = step.with ?? {};
      if (command) {
        if (step.uses || Object.keys(params).length) add('command', 'Commands cannot have uses or with', sp);
        if (step.run && (!step.shell || step.argv?.length)) add('command', 'run requires shell and cannot have argv', sp);
        if (!step.run && step.shell) add('command', 'shell requires run', sp);
        if (step.timeout && !validDuration(String(step.timeout))) add('timeout', 'Expected a positive Go duration', [...sp, 'timeout']);
      } else {
        if (step.shell || step.timeout || step['working-directory']) add('command', 'Command fields require argv or run', sp);
        const allowed = Object.hasOwn(controllerParameters, step.uses ?? '') ? controllerParameters[step.uses!] : undefined;
        if (allowed) {
          for (const key of Object.keys(params)) if (!allowed.includes(key)) add('action', `Unsupported controller parameter "${key}"`, [...sp, 'with', key]);
          const required = step.uses === 'git/commit' ? ['path', 'message'] : step.uses === 'github/pr' ? ['path', 'commit', 'title'] : step.uses === 'workspace/integrate' ? ['path'] : [];
          for (const key of required) if (params[key] == null) add('action', `Action requires ${key}`, [...sp, 'with', key]);
        } else if (/^action\/[A-Za-z_][A-Za-z0-9_-]*$/.test(step.uses ?? '')) {
          add('host-action', 'Registered action availability and typed schemas require host runtime validation', [...sp, 'uses'], undefined, 'host', 'info');
        } else {
          if (!/^agent\/[A-Za-z_][A-Za-z0-9_-]*$/.test(step.uses ?? '')) add('action', 'Expected agent/<name> or a controller action', [...sp, 'uses']);
          for (const key of Object.keys(params)) if (!['model', 'prompt', 'message', 'workspace', 'output'].includes(key)) add('action', `Unsupported agent parameter "${key}"`, [...sp, 'with', key]);
          if (params.prompt == null && params.message == null) add('action', 'Agent requires prompt or message', [...sp, 'with']);
          if (params.output != null) add('host-schema', 'Structured output contract requires runner schema admission', [...sp, 'with', 'output'], undefined, 'host', 'info');
        }
      }
      if (mapping(step.context) && Object.values(step.context).some(Boolean)) add('standalone', 'Step context is unsupported standalone', [...sp, 'context']);
    });
  }
  const state = new Map<string, number>();
  const visit = (id: string) => {
    if (state.get(id) === 1) { add('cycle', `Dependency cycle at "${id}"`, ['jobs', id, 'needs']); return; }
    if (state.get(id) === 2 || !Object.hasOwn(jobs, id)) return;
    state.set(id, 1); dependencies(jobs[id]).forEach(visit); state.set(id, 2);
  };
  Object.keys(jobs).forEach(visit);
  // Expression paths are inspected, never evaluated. Only recognized dot paths are rewritten.
  const inspect = (value: unknown, path: Path, jobID?: string) => {
    if (typeof value === 'string') {
      if (!hasExpressionReferences(path)) return;
      const warnedProducers = new Set<string>();
      for (const ref of references(value, isExpressionPath(path))) {
        if (ref.scope === 'inputs' || ref.scope === 'secrets') {
          // Manual runs may receive inputs and secrets from their host. A
          // callable interface makes these names statically knowable.
          const entries = callable?.[ref.scope];
          if (mapping(callable) && (!mapping(entries) || !Object.hasOwn(entries, ref.id))) {
            if (ref.scope === 'secrets') add('expression-reference', `Secret "${ref.id}" is not declared in the callable interface; the host must supply it`, path, undefined, 'host', 'warning');
            else add('expression-reference', `Unknown inputs reference "${ref.id}"`, path);
          }
          continue;
        }
        const exists = ref.scope === 'steps' ? jobID && jobs[jobID]?.steps?.some((s, i) => effectiveStepID(s, i) === ref.id) : Object.hasOwn(jobs, ref.id);
        if (!exists) add('expression-reference', `Unknown ${ref.scope} reference "${ref.id}"`, path);
        if (exists && ref.scope === 'needs' && jobID && !dependencySets.get(jobID)?.has(ref.id) && !warnedProducers.has(ref.id)) {
          warnedProducers.add(ref.id);
          add('expression-dependency', `Job "${diagnosticLabel(ref.id)}" is missing from Dependencies of "${diagnosticLabel(jobID)}". Review Dependencies and explicitly add the producer if needed.`, path, undefined, 'browser', 'warning');
        }
        if (exists && (ref.scope === 'jobs' || ref.scope === 'needs') && ref.output !== undefined && !jobs[ref.id].uses && !Object.hasOwn(jobs[ref.id].outputs ?? {}, ref.output)) {
          add('expression-reference', `Unknown output "${ref.output}" of job "${ref.id}"`, path);
        }
      }
    }
    else if (Array.isArray(value)) value.forEach((item, i) => inspect(item, [...path, i], jobID));
    else if (mapping(value)) Object.entries(value).forEach(([key, item]) => inspect(item, [...path, key], path.length === 1 && path[0] === 'jobs' ? key : jobID));
  };
  inspect(w, []);
}
function validDuration(value: string) {
  if (!/^\+?(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$/.test(value)) return false;
  const units: Record<string, bigint> = { ns: 1n, us: 1000n, 'µs': 1000n, 'μs': 1000n, ms: 1000000n, s: 1000000000n, m: 60000000000n, h: 3600000000000n };
  let total = 0n;
  for (const match of value.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g)) {
    const [whole, fraction = ''] = match[1].split('.');
    const integer = whole.replace(/^0+/, '') || '0';
    if (integer.length > 19) return false;
    const digits = fraction.slice(0, 19);
    total += BigInt(integer) * units[match[2]] + (digits ? BigInt(digits) * units[match[2]] / (10n ** BigInt(digits.length)) : 0n);
    if (total > 9223372036854775807n) return false;
  }
  return total > 0n;
}
