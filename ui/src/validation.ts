import { isAlias, isMap, isNode, isScalar, isSeq, LineCounter, parseAllDocuments, type Document, type Node } from 'yaml';
import { completedSteps, hasExpressionReferences, inHandlerPath, isConciseStepReference, isExpressionPath, references } from './expressions';
import { blockKind, containingStep, stepEntries } from './step-tree';
import type { Diagnostic, Job, Mapping, Path, Step, Workflow } from './types';

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
export const stepAction = (step: Mapping): string | undefined => step.uses as string | undefined;
export const hasAgentResult = (step: Mapping): boolean => Object.hasOwn(step, 'conversation') || !!stepAction(step)?.startsWith('agent/');
export const effectiveStepID = (step: Mapping, index: number) => String(step.id || `step_${index + 1}`);
export const mapping = (value: unknown): value is Mapping => value !== null && typeof value === 'object' && !Array.isArray(value);

// Match yaml.v3's allowedTimestampFormats and time.Parse ranges, including
// short date/time fields and fractional seconds. YAML core leaves these strings.
export function runnerTimestamp(value: string): boolean {
  const match = /^(\d{4})-(\d{1,2})-(\d{1,2})(?:([Tt]| +)(\d{1,2}):(\d{1,2}):(\d{1,2})(?:[.,]\d+)?(Z|[+-]\d{2}:\d{2})?)?$/.exec(value);
  if (!match || match[0] !== value) return false;
  const [, year, month, day, separator, hour, minute, second, zone] = match;
  const y = Number(year), m = Number(month), d = Number(day);
  const leap = y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (m < 1 || m > 12 || d < 1 || d > days[m - 1]) return false;
  if (!separator) return true;
  if (Number(hour) > 23 || Number(minute) > 59 || Number(second) > 59) return false;
  if (separator.trim() ? !zone : !!zone) return false;
  return !zone || zone === 'Z' || (Number(zone.slice(1, 3)) <= 24 && Number(zone.slice(4)) <= 60);
}

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

type Shape = 'strict-string' | 'strict-integer' | 'string' | 'boolean' | 'integer' | 'any' | 'list' | { [key: string]: Shape } | [Shape];
const context: Shape = { session: 'string', delivery: 'string' };
const input: Shape = { type: 'string', required: 'boolean', default: 'any' };
const entity: Shape = { ids: 'list', types: 'list', attributes: { '*': 'list' } };
const stepShape: { [key: string]: Shape } = {
  id: 'string', name: 'string', uses: 'string', agent: 'strict-string', prompt: 'strict-string', if: 'string', 'continue-on-error': 'boolean', with: { '*': 'any' }, context,
  conversation: ['strict-string'], start: 'strict-string', finishBy: 'strict-string', maxTurns: 'strict-integer', maxMessages: 'strict-integer',
  argv: ['string'], run: 'string', shell: 'string', 'working-directory': 'string', timeout: 'string', handlerTimeout: 'string',
};

export const handlerKinds = ['onResult', 'onComplete', 'onIncomplete', 'onError', 'onFailure', 'finally'] as const;
for (const kind of handlerKinds) stepShape[kind] = [stepShape];
stepShape.parallel = [stepShape]; stepShape.sequence = [stepShape];
const schema: Shape = {
  name: 'string', gates: { '*': 'any' }, schemas: { '*': 'any' },
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
    steps: [stepShape],
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
      if (value == null && shape !== 'strict-string' && shape !== 'strict-integer') {
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
        if (shape === 'strict-string') {
          const node = doc!.getIn(path, true);
          valid = typeof value === 'string' && isScalar(node)
            && (!node.tag || node.tag === 'tag:yaml.org,2002:str')
            && !(node.type === 'PLAIN' && !node.tag && runnerTimestamp(value));
        } else if (shape === 'list') {
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
          // YAML core leaves Go's binary integer spelling as a plain string.
          const integerValue = shape === 'strict-integer' && typeof value === 'string'
            && isScalar(node) && node.type === 'PLAIN' && !node.tag ? 0 : value;
          const decoded = runnerInteger(integerValue, isScalar(node) ? node.source : undefined);
          valid = decoded !== undefined && (shape !== 'strict-integer' || (isScalar(node)
            && (!node.tag || node.tag === 'tag:yaml.org,2002:int')
            && /^[+-]?(?:0[xX][0-9a-fA-F]+|0[oO][0-7]+|0[bB][01]+|0[0-7]*|[1-9][0-9]*)$/.test(node.source?.replaceAll('_', '') ?? '')));
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
        valid = Array.isArray(value) && (path.at(-1) !== 'conversation' || value.every(item => item != null));
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
      if (!valid) { shaped = false; add('shape', `Expected ${shape === 'strict-string' ? 'string' : typeof shape === 'string' ? shape : Array.isArray(shape) ? 'sequence' : 'mapping'}`, path, undefined, 'parser'); }
    };
    checkShape(value, schema, []);
    if (!mapping(value) || !mapping(value.jobs) || Object.values(value.jobs).some(j => !mapping(j))) {
      add('jobs', 'Workflow requires a jobs mapping'); return { doc, diagnostics, usable: false };
    }
    if (!shaped || Object.values(value.jobs).some(j => Array.isArray((j as Job).steps) && (j as Job).steps!.some(s => !mapping(s)))) return { doc, diagnostics, usable: false };
    const workflow = value as Workflow;
    validateWorkflow(workflow, add, doc);
    return { doc, workflow, diagnostics, usable: true };
  } catch (error) {
    add('parse', String(error), [], undefined, 'parser');
    return { diagnostics, usable: false };
  }
}

type Add = (code: string, message: string, path?: Path, node?: Node | null, source?: Diagnostic['source'], severity?: Diagnostic['severity']) => void;
function validateWorkflow(w: Workflow, add: Add, doc: Document) {
  const jobs = w.jobs;
  const namedKeywords = new Set(['type', 'title', 'description', '$comment', 'pattern', 'properties', 'items', 'additionalProperties', 'required', 'enum', 'minLength', 'maxLength', 'maxItems', 'minimum']);
  const inspectSchema = (value: unknown, path: Path) => {
    if (!mapping(value)) { add('schema', 'Expected a schema mapping', path); return; }
    for (const [key, child] of Object.entries(value)) {
      if (!namedKeywords.has(key)) add('schema', `Unsupported schema keyword "${key}"; references and composition are unsupported`, [...path, key]);
      else if (key === 'properties' && mapping(child)) {
        for (const [name, property] of Object.entries(child)) inspectSchema(property, [...path, key, name]);
      } else if (key === 'items' || key === 'additionalProperties' && typeof child !== 'boolean') inspectSchema(child, [...path, key]);
    }
  };
  for (const [name, declaration] of Object.entries(w.schemas ?? {})) {
    if (!identifier.test(name)) add('identifier', 'Invalid schema name', ['schemas', name]);
    inspectSchema(declaration, ['schemas', name]);
    add('host-schema', 'Named schema constraints require runner schema admission', ['schemas', name], undefined, 'host', 'info');
  }
  const dependencySets = new Map<string, Set<string>>();
  const trees = new Map(Object.entries(jobs).map(([id, job]) => [id, stepEntries(job.steps)]));
  const stepIndexes = new Map([...trees].map(([id, entries]) => [id, new Map(entries.map((entry, index) => [entry.id, index]))]));
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
    let handlerCount = 0;
    const validateSteps = (steps: Step[], parent: Path, depth = 0) => {
    const seen = new Set<string>();
    const entries = depth === 0 ? trees.get(id)! : steps.map((step, i) => ({ step, path: [...parent.slice(2), i], id: effectiveStepID(step, i) }));
    entries.forEach(({ step, path, id: sid }) => {
      const sp = [...p, ...path];
      if (!identifier.test(sid) || seen.has(sid)) add('identifier', 'Invalid or duplicate effective step ID', [...sp, 'id']);
      seen.add(sid);
      const kind = blockKind(step);
      if (kind) {
        if (depth > 0) add('handler-action', 'Structural blocks cannot appear inside lifecycle handlers', sp);
        if (!step[kind]?.length) add('block', `${kind} requires at least one step`, [...sp, kind]);
        for (const key of Object.keys(step)) if (!['id', 'name', 'if', kind].includes(key)) add('block', `Block cannot contain ${key}`, [...sp, key]);
        return;
      }
      const conversation = Object.hasOwn(step, 'conversation');
      if (conversation) {
        if (depth > 0 || [...handlerKinds, 'handlerTimeout'].some(key => Object.hasOwn(step, key))) add('handler-action', 'Conversations cannot own or appear inside lifecycle handlers', sp);
        const participants = step.conversation ?? [];
        if (participants.length < 2 || participants.length > 16 || participants.some(role => !identifier.test(role)) || new Set(participants).size !== participants.length) add('conversation', 'conversation requires 2–16 distinct literal role identifiers', [...sp, 'conversation']);
        if (step.start && !participants.includes(step.start)) add('conversation', 'start must name a declared participant', [...sp, 'start']);
        if (!participants.includes(step.finishBy ?? '')) add('conversation', 'finishBy must name a declared participant', [...sp, 'finishBy']);
        for (const [key, limit] of [['maxTurns', 100], ['maxMessages', 256]] as const) if (step[key] !== undefined && (!Number.isInteger(step[key]) || step[key]! < 1 || step[key]! > limit)) add('conversation', `${key} must be an integer from 1 to ${limit}`, [...sp, key]);
        if (typeof step.prompt !== 'string' || !step.prompt.trim()) add('conversation', 'Conversation requires a nonempty prompt', [...sp, 'prompt']);
        if (step.timeout && !validDuration(String(step.timeout), 86400000000000n)) add('timeout', 'Expected a positive Go duration of at most 24h', [...sp, 'timeout']);
        for (const key of ['uses', 'agent', 'argv', 'run', 'shell', 'working-directory', 'context']) if (Object.hasOwn(step, key)) add('conversation', `Conversation cannot contain ${key}`, [...sp, key]);
        for (const key of Object.keys(step.with ?? {})) if (!['output', 'model', 'workspace'].includes(key)) add('conversation', `Unsupported conversation parameter "${key}"`, [...sp, 'with', key]);
        const output = step.with?.output;
        const completionSchema = mapping(output) && typeof output.schema === 'string' ? w.schemas?.[output.schema] : mapping(output) ? output.schema : undefined;
        if (mapping(completionSchema) && completionSchema.type !== undefined && completionSchema.type !== 'object') add('conversation-output', 'Conversation completion schema must have type object', [...sp, 'with', 'output', 'schema']);
        if (output != null) add('host-schema', 'Structured output contract requires runner schema admission', [...sp, 'with', 'output'], undefined, 'host', 'info');
        if (mapping(output) && typeof output.schema === 'string' && !Object.hasOwn(w.schemas ?? {}, output.schema)) add('schema-reference', `Unknown workflow-local schema "${output.schema}"`, [...sp, 'with', 'output', 'schema']);
        add('host-conversation', 'Resumable sessions and managed messaging require host adapter admission', [...sp, 'conversation'], undefined, 'host', 'info');
        return;
      }
      for (const key of ['start', 'finishBy', 'maxTurns', 'maxMessages']) if (Object.hasOwn(step, key)) add('conversation', `${key} requires conversation`, [...sp, key]);
      const shorthand = Object.hasOwn(step, 'agent');
      const prompt = Object.hasOwn(step, 'prompt');
      const uses = stepAction(step);
      if (shorthand) add('agent', 'agent shorthand was removed; use agent/run with with.agent', [...sp, 'agent']);
      if (shorthand && Object.hasOwn(step, 'uses')) add('agent', 'agent cannot be combined with uses', [...sp, 'agent']);
      if (prompt) add('agent', 'Top-level prompt requires conversation; use with.prompt for agent/run', [...sp, 'prompt']);
      if (prompt && Object.hasOwn(step.with ?? {}, 'prompt')) add('agent', 'prompt cannot be combined with with.prompt', [...sp, 'prompt']);
      const command = !!(step.argv?.length || step.run);
      if (step.onResult?.length || step.onComplete?.length || step.onIncomplete?.length) {
        if (uses !== 'agent/run') add('completion', 'Completion hooks require agent/run', sp);
        const output = mapping(step.with?.output) ? step.with.output : undefined;
        const schema = output && typeof output.schema === 'string' ? w.schemas?.[output.schema] : output?.schema;
        if (!mapping(schema) || schema.type !== 'object' || !Array.isArray(schema.required) || !schema.required.includes('complete') || !mapping(schema.properties) || !mapping(schema.properties.complete) || schema.properties.complete.type !== 'boolean') add('completion', 'Completion hooks require an object schema with required boolean complete', [...sp, 'with', 'output']);
      }
      if ((depth > 0 || handlerKinds.some(kind => step[kind]?.length)) && !command && !uses?.startsWith('agent/')) add('handler-action', 'Lifecycle handlers support agent and command steps only', sp);
      const params: Mapping = { ...step.with, ...(prompt ? { prompt: step.prompt } : {}) };
      if (command) {
        if (uses) add('command', 'Commands cannot have uses', sp);
        for (const key of Object.keys(params)) if (!['input', 'output'].includes(key)) add('command', `Unsupported command parameter ${key}`, [...sp, 'with', key]);
        if (step.run && (!step.shell || step.argv?.length)) add('command', 'run requires shell and cannot have argv', sp);
        if (!step.run && step.shell) add('command', 'shell requires run', sp);
        if (step.timeout && !validDuration(String(step.timeout))) add('timeout', 'Expected a positive Go duration', [...sp, 'timeout']);
      } else {
        if (step.shell || step.timeout || step['working-directory']) add('command', 'Command fields require argv or run', sp);
        const allowed = Object.hasOwn(controllerParameters, uses ?? '') ? controllerParameters[uses!] : undefined;
        if (allowed) {
          for (const key of Object.keys(params)) if (!allowed.includes(key)) add('action', `Unsupported controller parameter "${key}"`, [...sp, 'with', key]);
          const required = uses === 'git/commit' ? ['path', 'message'] : uses === 'github/pr' ? ['path', 'commit', 'title'] : uses === 'workspace/integrate' ? ['path'] : [];
          for (const key of required) if (params[key] == null) add('action', `Action requires ${key}`, [...sp, 'with', key]);
        } else if (uses === 'input/wait') {
          for (const key of Object.keys(params)) if (!['prompt', 'evidence-id', 'payload'].includes(key)) add('input-wait', `Unsupported input/wait parameter "${key}"`, [...sp, 'with', key]);
          for (const key of ['prompt', 'evidence-id']) if (typeof params[key] !== 'string' || !params[key].trim()) add('input-wait', `input/wait requires string ${key}`, [...sp, 'with', key]);
        } else if (/^action\/[A-Za-z_][A-Za-z0-9_-]*$/.test(uses ?? '')) {
          add('host-action', 'Registered action availability and typed schemas require host runtime validation', [...sp, 'uses'], undefined, 'host', 'info');
        } else {
          if (uses !== 'agent/run') add('action', 'Expected agent/run or a controller action', [...sp, 'uses']);
          if (typeof params.agent !== 'string' || !params.agent.trim()) add('agent-definition', 'agent/run requires a definition path or agent reference', [...sp, 'with', 'agent']);
          for (const key of Object.keys(params)) if (!['agent', 'input', 'model', 'prompt', 'message', 'workspace', 'output'].includes(key)) add('action', `Unsupported agent parameter "${key}"`, [...sp, 'with', key]);
          for (const key of ['prompt', 'message', 'model', 'workspace']) if (params[key] != null && (typeof params[key] !== 'string' || (() => { const n = doc?.getIn([...sp, 'with', key], true); return isScalar(n) && n.type === 'PLAIN' && !n.tag && runnerTimestamp(String(params[key])); })())) add('action', `Agent ${key} must be a string`, [...sp, 'with', key]);
          if (params.prompt == null && params.message == null) add('action', 'Agent requires prompt or message', [...sp, 'with']);
          if (params.output != null) add('host-schema', 'Structured output contract requires runner schema admission', [...sp, 'with', 'output'], undefined, 'host', 'info');
          if (mapping(params.output) && typeof params.output.schema === 'string' && !Object.hasOwn(w.schemas ?? {}, params.output.schema)) {
            add('schema-reference', `Unknown workflow-local schema "${params.output.schema}"`, [...sp, 'with', 'output', 'schema']);
          }
        }
      }
      if (mapping(step.context) && Object.values(step.context).some(Boolean)) add('standalone', 'Step context is unsupported standalone', [...sp, 'context']);
      if (step.handlerTimeout && (!validDuration(step.handlerTimeout, 300000000000n) || !handlerKinds.some(kind => step[kind]?.length))) add('handler-timeout', 'Handler timeout requires handlers and a positive duration no greater than 5m', [...sp, 'handlerTimeout']);
      for (const kind of handlerKinds) if (step[kind]?.length) {
        handlerCount += step[kind]!.length;
        if (depth >= 8) add('handler-depth', 'Handler nesting exceeds 8', [...sp, kind]);
        else validateSteps(step[kind]!, [...sp, kind], depth + 1);
      }
    });
    };
    validateSteps(job.steps ?? [], [...p, 'steps']);
    if (handlerCount > 100) add('handler-count', 'A job cannot contain more than 100 handler steps', [...p, 'steps']);
  }
  const state = new Map<string, number>();
  const visit = (id: string) => {
    if (state.get(id) === 1) { add('cycle', `Dependency cycle at "${id}"`, ['jobs', id, 'needs']); return; }
    if (state.get(id) === 2 || !Object.hasOwn(jobs, id)) return;
    state.set(id, 1); dependencies(jobs[id]).forEach(visit); state.set(id, 2);
  };
  Object.keys(jobs).forEach(visit);
  // Expression paths are inspected, never evaluated. Only recognized dot paths are rewritten.
  const inspect = (value: unknown, path: Path, jobID?: string, namedStepPath?: Path) => {
    const contextPath = namedStepPath ?? path;
    if (typeof value === 'string') {
      if (!hasExpressionReferences(path)) return;
      const warnedProducers = new Set<string>();
      for (const ref of references(value, isExpressionPath(path))) {
        if (ref.scope === 'failed' || ref.scope === 'error') {
          if (path[0] === 'schemas' && !namedStepPath) continue;
          const fields = ref.scope === 'failed' ? ['id', 'uses', 'prompt'] : ['message', 'class'];
          const field = `${ref.id}${ref.output !== undefined ? `.outputs.${ref.output}` : ''}${ref.tail ?? ''}`;
          if (!inHandlerPath(contextPath)) add('handler-context', `${ref.scope} context is available only inside step handlers`, path);
          else if (!fields.includes(field)) add('handler-context', `Unknown ${ref.scope} context field "${field}"`, path);
          continue;
        }
        if (ref.scope === 'steps' && inHandlerPath(contextPath)) {
          if (!completedSteps(w, contextPath).has(ref.id)) add('expression-reference', `Step result "${ref.id}" is unavailable here; reference a completed preceding step`, path);
          continue;
        }
        // Shared declarations have no local job until an agent uses them.
        if (path[0] === 'schemas' && (ref.scope === 'steps' || ref.scope === 'needs' || ref.scope === 'jobs') && namedStepPath === undefined) continue;
        if (path[0] === 'schemas' && (ref.scope === 'inputs' || ref.scope === 'secrets') && namedStepPath !== undefined) continue;
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
        const exists = ref.scope === 'steps' ? jobID && stepIndexes.get(jobID)?.has(ref.id) : Object.hasOwn(jobs, ref.id);
        if (!exists) add('expression-reference', `Unknown ${ref.scope} reference "${ref.id}"`, path);
        if (exists && ref.scope === 'needs' && jobID && !dependencySets.get(jobID)?.has(ref.id) && !warnedProducers.has(ref.id)) {
          warnedProducers.add(ref.id);
          add('expression-dependency', `Job "${diagnosticLabel(ref.id)}" is missing from Dependencies of "${diagnosticLabel(jobID)}". Review Dependencies and explicitly add the producer if needed.`, path, undefined, 'browser', 'warning');
        }
        if (exists && (ref.scope === 'jobs' || ref.scope === 'needs') && ref.step === undefined && ref.output !== undefined && !jobs[ref.id].uses && !Object.hasOwn(jobs[ref.id].outputs ?? {}, ref.output)) {
          add('expression-reference', `Unknown output "${ref.output}" of job "${ref.id}"`, path);
        }
        if (ref.step !== undefined) {
          if (ref.scope === 'jobs' && jobID || ref.scope === 'needs' && !jobID) add('expression-reference', `${ref.scope} step results are unavailable in this scope`, path);
          if (ref.scope === 'needs' && jobID && !dependencySets.get(jobID)?.has(ref.id)) add('expression-reference', `Step results require a declared dependency on "${ref.id}"`, path);
          if (exists) {
            const producer = jobs[ref.id];
            if (producer.uses || producer['for-each']) add('expression-reference', `Job "${ref.id}" does not expose individual step results; use its outputs or items`, path);
            else if (!stepIndexes.get(ref.id)?.has(ref.step)) add('expression-reference', `Unknown step "${ref.step}" of job "${ref.id}"`, path);
          }
        }
        if (exists && ref.scope === 'steps' && jobID && (isConciseStepReference(ref) || (trees.get(jobID) ?? []).some(entry => blockKind(entry.step)))) {
          const entry = containingStep(trees.get(jobID) ?? [], contextPath.slice(2));
          const inStep = entry !== undefined;
          const afterSteps = path[2] === 'outputs' || path[2] === 'repeat' && path[3] === 'until';
          if (!inStep && !afterSteps || inStep && !entry.available.has(ref.id)) add('expression-reference', `Step result "${ref.id}" is unavailable here; reference a completed preceding step`, path);
        }
      }
    }
    else if (Array.isArray(value)) value.forEach((item, i) => inspect(item, [...path, i], jobID, namedStepPath));
    else if (mapping(value)) Object.entries(value).forEach(([key, item]) => inspect(item, [...path, key], path.length === 1 && path[0] === 'jobs' ? key : jobID, namedStepPath));
  };
  inspect(w, []);
  const inspectNamedOutputs = (steps: Step[], jobID: string, path: Path) => steps.forEach((step, index) => {
    const location = [...path, index];
    const output = step.with?.output;
    if (hasAgentResult(step) && mapping(output) && typeof output.schema === 'string') {
      const declaration = w.schemas?.[output.schema];
      if (mapping(declaration)) inspect(declaration, ['schemas', output.schema], jobID, location);
    }
    for (const kind of [...handlerKinds, 'parallel', 'sequence'] as const) inspectNamedOutputs(step[kind] ?? [], jobID, [...location, kind]);
  });
  for (const [jobID, job] of Object.entries(jobs)) inspectNamedOutputs(job.steps ?? [], jobID, ['jobs', jobID, 'steps']);
}
function validDuration(value: string, maximum = 9223372036854775807n) {
  if (!/^\+?(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$/.test(value)) return false;
  const units: Record<string, bigint> = { ns: 1n, us: 1000n, 'µs': 1000n, 'μs': 1000n, ms: 1000000n, s: 1000000000n, m: 60000000000n, h: 3600000000000n };
  let total = 0n;
  for (const match of value.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g)) {
    const [whole, fraction = ''] = match[1].split('.');
    const integer = whole.replace(/^0+/, '') || '0';
    if (integer.length > 19) return false;
    const digits = fraction.slice(0, 19);
    total += BigInt(integer) * units[match[2]] + (digits ? BigInt(digits) * units[match[2]] / (10n ** BigInt(digits.length)) : 0n);
    if (total > maximum) return false;
  }
  return total > 0n && total <= maximum;
}
