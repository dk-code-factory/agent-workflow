import { isMap, isScalar, isSeq, parseDocument, type Document, type Node } from 'yaml';
import type { Diagnostic, JobResultPreview, JobResultRequest, LayoutState, Path, Snapshot, Step, WorkflowOptions } from './types';
import { dependencies, effectiveStepID, stepAction, hasAgentResult, identifier, parseWorkflow, runnerTimestamp } from './validation';
import { completedSteps, hasExpressionReferences, inHandlerPath, isExpressionPath, mapReferences, pathJob, references, referenceText, stepPathLength, type Reference } from './expressions';
import { blockKind, producerStep, stepEntries } from './step-tree';
import { stepConversion } from './conversion';

// Whole workflow/job/step replacements may change anonymous owners; field leaves do not.
const preservesInspectorOwner = (path: Path) => path.length > 0 && (path[0] !== 'jobs' || (path.length >= 3 && path[2] !== 'steps') || path.length > 4);
const emptyLayout = (): LayoutState => ({ positions: {}, collapsed: {} });
export class WorkflowDocument {
  private options: WorkflowOptions;
  private parsed: ReturnType<typeof parseWorkflow>;
  private snapshot: Snapshot;
  private listeners = new Set<() => void>();
  private past: string[] = [];
  private future: string[] = [];
  private validation?: AbortController;
  private epoch = 0;
  private disposed = false;
  private historyGroup?: string;
  private lastEditTime = 0;
  private savedSource?: string;
  private inspectorDirty = false;
  private inspectorDraftEpoch = 0;
  private inspectorGeneration = 0;
  private protectionSource: string;
  private protectionEpoch = 0;
  private lastProtectedSource?: string;
  private discardInspectorListeners = new Set<() => void>();
  readonly readOnly: boolean;
  layout: LayoutState;
  constructor(options: WorkflowOptions, readOnly = false) {
    this.options = options; this.readOnly = readOnly;
    this.protectionSource = options.source;
    this.layout = structuredClone(options.layout ?? emptyLayout());
    this.parsed = parseWorkflow(options.source);
    this.snapshot = { source: options.source, workflow: this.parsed.workflow, previewWorkflow: this.parsed.workflow, diagnostics: this.parsed.diagnostics, stale: !this.parsed.usable, revision: 0, canUndo: false, canRedo: false, saving: false, validating: false, dirty: true, hasLocalChanges: false, pendingSourceReplacement: undefined };
  }
  start = () => { this.disposed = false; void this.validate(); };
  getOptions = () => this.options;
  getSnapshot = () => this.snapshot;
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private emit() {
    // A clean baseline ends repeat suppression for a previously declined source.
    if (!this.snapshot.hasLocalChanges) this.lastProtectedSource = undefined;
    if (!this.disposed) { this.listeners.forEach(fn => fn()); this.options.onChange?.(structuredClone(this.snapshot)); }
  }
  update(options: Partial<WorkflowOptions>) {
    const previous = this.options; const validationEpoch = this.epoch;
    this.options = { ...previous, ...options };
    if (options.layout) this.layout = structuredClone(options.layout);
    // Current-source echoes are acknowledgements of neither saving nor draft
    // disposal. Repeat suppression lasts only for the current protected draft.
    if (options.source !== undefined && options.source !== this.snapshot.source && (options.source !== this.lastProtectedSource || !this.snapshot.hasLocalChanges)) {
      if (!this.readOnly && this.snapshot.hasLocalChanges) {
        this.lastProtectedSource = options.source;
        this.snapshot = { ...this.snapshot, pendingSourceReplacement: options.source };
        this.emit();
      } else this.acceptSource(options.source);
    } else { this.snapshot = { ...this.snapshot }; this.listeners.forEach(fn => fn()); }
    // An accepted source may already have validated the merged options. A
    // staged or context-only update still needs its own refresh.
    if (this.epoch === validationEpoch && (this.options.validate !== previous.validate || this.options.validationKey !== previous.validationKey || JSON.stringify(this.options.agents) !== JSON.stringify(previous.agents) || JSON.stringify(this.options.workflows) !== JSON.stringify(previous.workflows))) void this.validate();
  }
  /** @internal Invalidates renderer ownership after arbitrary source/history or structural edits. */
  getInspectorGeneration = () => this.inspectorGeneration;
  onDiscardInspector = (listener: () => void) => { this.discardInspectorListeners.add(listener); return () => { this.discardInspectorListeners.delete(listener); }; };
  resolveSourceReplacement(choice: 'keep-local' | 'use-incoming') {
    if (this.disposed || this.snapshot.pendingSourceReplacement === undefined) return;
    const incoming = this.snapshot.pendingSourceReplacement;
    if (choice === 'use-incoming') this.acceptSource(incoming);
    else { this.snapshot = { ...this.snapshot, pendingSourceReplacement: undefined }; this.emit(); }
  }
  private acceptSource(source: string) {
    const { revision, pendingSourceReplacement } = this.snapshot;
    const inspectorDraftEpoch = this.inspectorDraftEpoch;
    this.discardInspectorListeners.forEach(discard => discard());
    // Discard can synchronously notify the host. A newer source edit, candidate
    // or reported inspector draft owns the next choice.
    if (this.disposed || this.snapshot.revision !== revision || this.snapshot.pendingSourceReplacement !== pendingSourceReplacement || this.inspectorDraftEpoch !== inspectorDraftEpoch) return;
    this.inspectorDirty = false;
    this.protectionSource = source; this.protectionEpoch++;
    this.snapshot = { ...this.snapshot, pendingSourceReplacement: undefined, hasLocalChanges: this.snapshot.source !== source, dirty: this.snapshot.source !== this.savedSource };
    if (source !== this.snapshot.source) this.commit(source, true);
    else this.emit();
  }
  setLayout(next: LayoutState) { this.layout = structuredClone(next); this.options.onLayoutChange?.(structuredClone(this.layout)); this.snapshot = { ...this.snapshot }; this.listeners.forEach(fn => fn()); }
  private commit(source: string, history: boolean, group?: string, preserveInspector = false) {
    if (this.disposed || source === this.snapshot.source) return;
    if (history) {
      const now = Date.now();
      if (!group || group !== this.historyGroup || now - this.lastEditTime > 750) this.past.push(this.snapshot.source);
      this.future = []; this.lastEditTime = now;
    }
    if (!preserveInspector) this.inspectorGeneration++;
    this.historyGroup = history ? group : undefined;
    this.parsed = parseWorkflow(source);
    this.snapshot = { ...this.snapshot, source, workflow: this.parsed.workflow, previewWorkflow: this.parsed.workflow ?? this.snapshot.previewWorkflow, diagnostics: this.parsed.diagnostics, stale: !this.parsed.usable, revision: this.snapshot.revision + 1, canUndo: !!this.past.length, canRedo: !!this.future.length, saveError: undefined, dirty: this.inspectorDirty || source !== this.savedSource, hasLocalChanges: this.inspectorDirty || source !== this.protectionSource };
    this.emit(); void this.validate();
  }
  getPathAtOffset(offset: number): Path {
    let result: Path = [];
    const walk = (node: Node | null, path: Path) => {
      if (!node?.range || offset < node.range[0] || offset > node.range[2]) return;
      result = path;
      if (isMap(node)) node.items.forEach(pair => {
        if (isScalar(pair.key)) walk(pair.value as Node, [...path, String(pair.key.value)]);
      });
      if (isSeq(node)) node.items.forEach((item, index) => walk(item as Node, [...path, index]));
    };
    walk(this.parsed.doc?.contents ?? null, []);
    return result;
  }
  getSourceRange(path: Path): { from: number; to: number } | undefined {
    if (!this.parsed.usable || !this.parsed.doc) return;
    const node = this.parsed.doc.getIn(path, true) as Node | undefined;
    if (!node?.range) return;
    const parent = this.parsed.doc.getIn(path.slice(0, -1), true);
    const key = isMap(parent) ? parent.items.find(pair => isScalar(pair.key) && pair.key.value === path.at(-1))?.key : undefined;
    return { from: isScalar(key) && key.range ? key.range[0] : node.range[0], to: node.range[1] };
  }
  setSource(source: string, group?: string) { if (!this.readOnly) this.commit(source, true, group); }
  undo() { if (!this.readOnly && this.past.length) { this.future.push(this.snapshot.source); this.commit(this.past.pop()!, false); } }
  redo() { if (!this.readOnly && this.future.length) { this.past.push(this.snapshot.source); this.commit(this.future.pop()!, false); } }
  private edit(fn: (doc: Document) => void, preserveInspector = false) {
    if (this.readOnly) throw new Error('Viewer is read-only');
    if (!this.parsed.usable || !this.parsed.doc) throw new Error('Visual editing is suspended while the preview is stale');
    const doc = this.parsed.doc.clone(); fn(doc); this.commit(doc.toString({ lineWidth: 0 }), true, undefined, preserveInspector);
  }
  private writeField(doc: Document, path: Path, value: unknown) {
    if (value === undefined) { doc.deleteIn(path); return; }
    // Optional mappings such as repeat may be explicitly null in imported
    // YAML. Materialize only the ancestors the user is now editing.
    for (let length = 1; length < path.length; length++) {
      const prefix = path.slice(0, length);
      const node = doc.getIn(prefix, true);
      if (node == null || (isScalar(node) && node.value == null)) {
        const replacement = doc.createNode(typeof path[length] === 'number' ? [] : {});
        if (isScalar(node)) {
          replacement.comment = node.comment;
          replacement.commentBefore = node.commentBefore;
          replacement.spaceBefore = node.spaceBefore;
        }
        doc.setIn(prefix, replacement);
      }
    }
    doc.setIn(path, value);
    if (((stepPathLength(path) === path.length - 1 && path.at(-1) === 'prompt') || (stepPathLength(path) === path.length - 2 && path.at(-2) === 'with' && ['prompt','message'].includes(String(path.at(-1))))) && typeof value === 'string' && runnerTimestamp(value)) {
      const node = doc.getIn(path, true);
      if (isScalar(node)) node.type = 'QUOTE_DOUBLE';
    }
  }
  setField(path: Path, value: unknown) { this.edit(doc => this.writeField(doc, path, value), preservesInspectorOwner(path)); }
  getFieldYAML(path: Path): string {
    const node = this.parsed.doc?.getIn(path, true);
    if (!node || (!isMap(node) && !isSeq(node) && !isScalar(node))) return '';
    const fragment = parseDocument(this.snapshot.source, { intAsBigInt: true });
    fragment.contents = (fragment.getIn(path, true) as Node).clone() as Node;
    return fragment.toString({ lineWidth: 0 }).trimEnd();
  }
  moveArgument(path: Path, from: number, to?: number) {
    this.edit(doc => {
      const seq = doc.getIn(path, true);
      if (!isSeq(seq) || from < 1 || from >= seq.items.length || (to !== undefined && (to < 1 || to >= seq.items.length))) throw new Error('Invalid argument position');
      const [item] = seq.items.splice(from, 1);
      if (to !== undefined) seq.items.splice(to, 0, item);
    }, preservesInspectorOwner(path));
  }
  previewJobResult(request: JobResultRequest): JobResultPreview {
    if (this.readOnly) throw new Error('Viewer is read-only');
    const workflow = this.snapshot.workflow;
    if (this.snapshot.stale || !workflow || !this.parsed.doc) throw new Error('Visual editing is suspended while the preview is stale');
    const { target, producer, source } = request;
    if (!hasExpressionReferences(target)) throw new Error('Choose an expression-capable field');
    const consumer = pathJob(target);
    const jobs = workflow.jobs;
    if (!Object.hasOwn(jobs, producer) || (consumer && !Object.hasOwn(jobs, consumer))) throw new Error('Unknown job');
    const reaches = (id: string, target: string, seen = new Set<string>()): boolean => id === target || (Object.hasOwn(jobs, id) && !seen.has(id) && (seen.add(id), dependencies(jobs[id]).some(dep => reaches(dep, target, seen))));
    if (consumer && reaches(producer, consumer)) throw new Error('Result dependency would create a cycle');
    // Never recreate a deleted step/interface entry or overwrite structured data.
    const owner = consumer ? target.slice(0, target[2] === 'steps' ? 4 : 2) : target.slice(0, -1);
    if (!this.parsed.doc.hasIn(owner)) throw new Error('Unknown target field');
    const stored = this.parsed.doc.getIn(target);
    if (stored !== undefined && stored !== null && typeof stored !== 'string') throw new Error('Target must be a text field');
    const text = String(stored ?? '');
    const outputs = jobs[producer].outputs ?? {};
    let output: string;
    let declaration: string | undefined;
    let directReference: string | undefined;
    if (source.kind === 'output') {
      output = source.output;
      if (!identifier.test(output) || !Object.hasOwn(outputs, output)) throw new Error('Unknown producer output');
    } else if (source.kind === 'result') {
      const entry = producerStep(jobs[producer].steps, source.step);
      if (!entry || blockKind(entry.step) || jobs[producer].uses || jobs[producer]['for-each']) throw new Error('Producer does not expose individual step results');
      if (!source.field.split('.').every(part => identifier.test(part))) throw new Error('Choose a valid result field path');
      output = source.field;
      directReference = `${consumer ? 'needs' : 'jobs'}.${producer}.steps.${entry.id}.${source.field}`;
    } else {
      const entry = producerStep(jobs[producer].steps, source.step);
      if (!entry || blockKind(entry.step) || jobs[producer].uses) throw new Error('Unknown producer step');
      if (!identifier.test(source.output) || !identifier.test(source.name)) throw new Error('Choose valid output names');
      const reference = 'steps.' + entry.id + '.outputs.' + source.output;
      const value = '${{ ' + reference + ' }}';
      const matches = (value: string) => /^\s*\$\{\{\s*(\S+)\s*\}\}\s*$/.exec(value)?.[1] === reference;
      if (Object.hasOwn(outputs, source.name) && !matches(outputs[source.name])) throw new Error('Output name conflicts with an existing declaration');
      output = Object.keys(outputs).find(key => matches(outputs[key])) ?? source.name;
      if (!Object.hasOwn(outputs, output)) declaration = value;
    }
    const reference = directReference ?? `${consumer ? 'needs' : 'jobs'}.${producer}.outputs.${output}`;
    const expression = isExpressionPath(target) && text.trim() && !text.trim().startsWith('${{') ? reference : '${{ ' + reference + ' }}';
    const { start, end } = request.selection ?? { start: text.length, end: text.length };
    if (!Number.isInteger(start) || !Number.isInteger(end) || start < 0 || end < start || end > text.length) throw new Error('Invalid text selection');
    return { revision: this.snapshot.revision, request: structuredClone(request), output, expression, declaration,
      addDependency: !!consumer && !dependencies(jobs[consumer]).includes(producer), value: text.slice(0, start) + expression + text.slice(end) };
  }
  applyJobResult(preview: JobResultPreview) {
    if (preview.revision !== this.snapshot.revision) throw new Error('The document changed. Preview the job result again.');
    // Derive every mutation again; preview fields are never trusted instructions.
    const current = this.previewJobResult(preview.request);
    const { target, producer } = current.request;
    this.edit(doc => {
      if (current.declaration !== undefined) this.writeField(doc, ['jobs', producer, 'outputs', current.output], current.declaration);
      const consumer = pathJob(target);
      if (consumer && current.addDependency) {
        const needs = doc.getIn(['jobs', consumer, 'needs'], true);
        if (isSeq(needs)) needs.add(producer);
        else this.writeField(doc, ['jobs', consumer, 'needs'], [...dependencies(this.snapshot.workflow!.jobs[consumer]), producer]);
      }
      this.writeField(doc, target, current.value);
    }, true); // Preview admission restricts these writes to non-owner fields.
  }
  private unique(base: string, ids: string[]) { let id = base; let i = 2; while (ids.includes(id)) id = `${base}_${i++}`; return id; }
  addJob(uses?: string): string {
    const id = this.unique('job', Object.keys(this.snapshot.workflow?.jobs ?? {}));
    this.edit(doc => doc.setIn(['jobs', id], uses ? { uses } : { steps: [{ id: 'step_1', argv: ['echo', 'Hello'] }] })); return id;
  }
  setJobKind(id: string, reusable: boolean) {
    this.edit(doc => {
      if (reusable) { doc.deleteIn(['jobs', id, 'steps']); doc.setIn(['jobs', id, 'uses'], this.options.workflows?.[0]?.id ?? 'workflows/example.yml'); }
      else {
        for (const key of ['uses', 'with', 'secrets']) doc.deleteIn(['jobs', id, key]);
        const steps = doc.getIn(['jobs', id, 'steps'], true);
        if (!isSeq(steps) || !steps.items.length) doc.setIn(['jobs', id, 'steps'], [{ id: 'step_1', argv: ['echo', 'Hello'] }]);
      }
    });
  }
  setStepKind(id: string, index: number, kind: string) {
    const step = this.snapshot.workflow?.jobs[id]?.steps?.[index];
    if (!step) throw new Error('Unknown step');
    const plan = stepConversion(step, kind);
    const used = stepEntries(this.snapshot.workflow!.jobs[id].steps).map(entry => entry.id);
    for (const entry of plan.set) if ((entry.path[0] === 'parallel' || entry.path[0] === 'sequence') && Array.isArray(entry.value)) {
      for (const { step: child } of stepEntries(entry.value)) { child.id = this.unique('branch', used); used.push(child.id); }
    }
    this.edit(doc => {
      const path: Path = ['jobs', id, 'steps', index];
      for (const key of plan.remove) doc.deleteIn([...path, ...key]);
      if (plan.set.some(entry => entry.path[0] === 'with') && !isMap(doc.getIn([...path, 'with'], true))) doc.setIn([...path, 'with'], doc.createNode({}));
      for (const entry of plan.set) doc.setIn([...path, ...entry.path], entry.value);
    });
  }
  duplicateJob(id: string): string {
    const next = this.unique(`${id}_copy`, Object.keys(this.snapshot.workflow?.jobs ?? {}));
    this.edit(doc => { const job = doc.getIn(['jobs', id], true) as Node; if (!job) throw new Error('Unknown job'); doc.setIn(['jobs', next], job.clone()); }); return next;
  }
  renameJob(id: string, next: string) {
    if (id === next) return;
    this.edit(doc => {
      if (!identifier.test(next) || doc.hasIn(['jobs', next])) throw new Error('Choose a unique valid job ID');
      const jobs = doc.get('jobs', true);
      if (!isMap(jobs)) throw new Error('Missing jobs');
      const pair = jobs.items.find(p => isScalar(p.key) && p.key.value === id);
      if (!pair || !isScalar(pair.key)) throw new Error('Unknown job'); pair.key.value = next;
      for (const [jobID, job] of Object.entries(this.snapshot.workflow!.jobs)) {
        if (dependencies(job).includes(id)) doc.setIn(['jobs', jobID === id ? next : jobID, 'needs'], dependencies(job).map(dep => dep === id ? next : dep));
      }
      this.rewrite(doc, (scope, ref) => (scope === 'jobs' || scope === 'needs') && ref === id ? next : undefined);
    });
    const layout = structuredClone(this.layout);
    if (Object.hasOwn(layout.positions, id)) { layout.positions = { ...layout.positions, [next]: layout.positions[id] }; delete layout.positions[id]; }
    if (Object.hasOwn(layout.collapsed, id)) { layout.collapsed = { ...layout.collapsed, [next]: layout.collapsed[id] }; delete layout.collapsed[id]; }
    this.setLayout(layout);
  }
  private rewrite(doc: Document, rename: (scope: string, id: string, path: Path) => string | undefined) {
    this.rewriteReferences(doc, (ref, path) => {
      const next = rename(ref.scope, ref.id, path);
      return next ? referenceText({ ...ref, id: next }) : undefined;
    });
  }
  private rewriteReferences(doc: Document, rename: (ref: Reference, path: Path) => string | undefined) {
    // Walk with explicit paths so local step references only change in their own job.
    const walk = (node: Node | null, path: Path) => {
      if (isScalar(node) && typeof node.value === 'string' && hasExpressionReferences(path)) node.value = mapReferences(node.value, ref => rename(ref, path), isExpressionPath(path));
      if (isMap(node)) node.items.forEach(p => walk(p.value as Node, [...path, String(isScalar(p.key) ? p.key.value : '')]));
      if (isSeq(node)) node.items.forEach((n, i) => walk(n as Node, [...path, i]));
    };
    walk(doc.contents, []);
  }
  private entryReference(path: Path, ref: Reference): boolean {
    const key = String(path[3]);
    if (path.length === 4 && path[0] === 'on' && path[1] === 'workflow_call' && ['inputs', 'secrets', 'outputs'].includes(String(path[2]))) {
      return ref.scope === path[2] && ref.id === key;
    }
    if (path.length === 4 && path[0] === 'jobs' && path[2] === 'outputs') {
      return (ref.scope === 'jobs' || ref.scope === 'needs') && ref.id === path[1] && ref.step === undefined && ref.output === key;
    }
    throw new Error('Expected a callable interface entry or job output path');
  }
  renameEntry(path: Path, next: string) {
    // Validate the supported path even when its current key matches next.
    this.entryReference(path, { scope: 'inputs', id: '' , offset: 0 });
    if (path[3] === next) return;
    this.edit(doc => {
      const parent = doc.getIn(path.slice(0, -1), true);
      if (!identifier.test(next) || !isMap(parent) || parent.has(next)) throw new Error('Choose a unique valid entry ID');
      const pair = parent.items.find(item => isScalar(item.key) && item.key.value === path[3]);
      if (!pair || !isScalar(pair.key)) throw new Error('Unknown entry');
      pair.key.value = next;
      this.rewriteReferences(doc, ref => this.entryReference(path, ref)
        ? referenceText(path[0] === 'jobs' ? { ...ref, output: next } : { ...ref, id: next }) : undefined);
    });
  }
  entryDeletionImpact(path: Path): Path[] {
    this.entryReference(path, { scope: 'inputs', id: '', offset: 0 });
    const result: Path[] = [];
    const walk = (value: unknown, location: Path) => {
      if (path.every((part, i) => location[i] === part)) return;
      if (typeof value === 'string') {
        if (hasExpressionReferences(location) && references(value, isExpressionPath(location)).some(ref => this.entryReference(path, ref))) result.push(location);
      } else if (Array.isArray(value)) value.forEach((item, i) => walk(item, [...location, i]));
      else if (value && typeof value === 'object') Object.entries(value).forEach(([key, item]) => walk(item, [...location, key]));
    };
    walk(this.snapshot.workflow, []);
    return result;
  }
  deleteEntry(path: Path, confirmed = false) {
    if (this.entryDeletionImpact(path).length && !confirmed) throw new Error('Deletion has references; confirmation is required');
    this.edit(doc => { if (!doc.deleteIn(path)) throw new Error('Unknown entry'); });
  }
  private schemaConsumers(name: string): { job: string; path: Path }[] {
    const result: { job: string; path: Path }[] = [];
    const walk = (steps: Step[], job: string, path: Path) => steps.forEach((step, index) => {
      const location = [...path, index];
      const output = step.with?.output;
      if (hasAgentResult(step) && output && typeof output === 'object' && !Array.isArray(output) && 'schema' in output && output.schema === name) result.push({ job, path: location });
      for (const kind of ['onResult', 'onComplete', 'onIncomplete', 'onError', 'onFailure', 'finally', 'parallel', 'sequence'] as const) walk(step[kind] ?? [], job, [...location, kind]);
    });
    for (const [job, value] of Object.entries(this.snapshot.workflow!.jobs)) walk(value.steps ?? [], job, ['jobs', job, 'steps']);
    return result;
  }
  private schemaConsumerReferencesStep(consumer: { job: string; path: Path }, job: string, index: number, id: string): boolean {
    const producer = completedSteps(this.snapshot.workflow!, consumer.path).get(id);
    return consumer.job === job && producer?.path.length === 4 && producer.path[3] === index;
  }
  deletionImpact(job: string, stepIndex?: number): Path[] {
    const result: Path[] = [];
    const stepID = stepIndex == null ? undefined : effectiveStepID(this.snapshot.workflow!.jobs[job].steps![stepIndex], stepIndex);
    const removed = new Set(stepIndex == null ? [] : stepEntries(this.snapshot.workflow!.jobs[job].steps).filter(entry => entry.path[1] === stepIndex).map(entry => entry.id));
    const schemas = new Set(Object.keys(this.snapshot.workflow!.schemas ?? {}).filter(name => this.schemaConsumers(name).some(consumer => consumer.job === job && (stepIndex === undefined || [...completedSteps(this.snapshot.workflow!, consumer.path)].some(([id, producer]) => removed.has(id) && !inHandlerPath(producer.path))))));
    const walk = (value: unknown, path: Path) => {
      if (typeof value === 'string') {
        if (hasExpressionReferences(path) && references(value, isExpressionPath(path)).some(ref => {
          if (!stepID) return (ref.scope === 'jobs' || ref.scope === 'needs') && ref.id === job;
          if ((ref.scope === 'jobs' || ref.scope === 'needs') && ref.id === job && ref.step !== undefined && removed.has(ref.step)) return true;
          if (ref.scope !== 'steps' || !removed.has(ref.id)) return false;
          if (path[0] === 'schemas') return schemas.has(String(path[1]));
          if (pathJob(path) !== job) return false;
          if (!inHandlerPath(path)) return true;
          const producer = completedSteps(this.snapshot.workflow!, path).get(ref.id);
          return producer !== undefined && !inHandlerPath(producer.path) && producer.path[3] === stepIndex;
        })) result.push(path);
      } else if (Array.isArray(value)) value.forEach((v, i) => walk(v, [...path, i]));
      else if (value && typeof value === 'object') Object.entries(value).forEach(([k, v]) => walk(v, [...path, k]));
    };
    walk(this.snapshot.workflow, []);
    if (!stepID) for (const [id, value] of Object.entries(this.snapshot.workflow!.jobs)) if (dependencies(value).includes(job)) result.push(['jobs', id, 'needs']);
    return result;
  }
  deleteJob(id: string, confirmed = false) {
    if (this.deletionImpact(id).length && !confirmed) throw new Error('Deletion has references; confirmation is required');
    this.edit(doc => {
      doc.deleteIn(['jobs', id]);
      for (const [key, job] of Object.entries(this.snapshot.workflow!.jobs)) if (key !== id && dependencies(job).includes(id)) doc.setIn(['jobs', key, 'needs'], dependencies(job).filter(dep => dep !== id));
    });
  }
  connect(from: string, to: string) {
    const jobs = this.snapshot.workflow?.jobs ?? {};
    if (!Object.hasOwn(jobs, from) || !Object.hasOwn(jobs, to)) throw new Error('Unknown job');
    if (from === to || dependencies(jobs[to]).includes(from)) throw new Error('Self or duplicate dependency');
    const reaches = (id: string, target: string, seen = new Set<string>()): boolean => id === target || (Object.hasOwn(jobs, id) && !seen.has(id) && (seen.add(id), dependencies(jobs[id]).some(dep => reaches(dep, target, seen))));
    if (reaches(from, to)) throw new Error('Connection would create a cycle');
    this.setField(['jobs', to, 'needs'], [...dependencies(jobs[to]), from]);
  }
  disconnect(from: string, to: string) { this.setField(['jobs', to, 'needs'], dependencies(this.snapshot.workflow!.jobs[to]).filter(dep => dep !== from)); }
  private materialize(doc: Document, job: string) {
    const steps = this.snapshot.workflow!.jobs[job].steps ?? [];
    stepEntries(steps).forEach(({ step, path, id }) => { if (!step.id) doc.setIn(['jobs', job, ...path, 'id'], id); });
  }
  addStep(job: string, template: Step = { argv: ['echo', 'Hello'] }): number {
    const steps = this.snapshot.workflow!.jobs[job].steps ?? [];
    this.edit(doc => {
      this.materialize(doc, job);
      if (!doc.hasIn(['jobs', job, 'steps'])) doc.setIn(['jobs', job, 'steps'], doc.createNode([]));
      const used = stepEntries(steps).map(entry => entry.id);
      const fresh = structuredClone(template);
      const entries = stepEntries([fresh]);
      entries.forEach(({ step }, index) => { step.id = this.unique(index === 0 ? 'step' : 'branch', used); used.push(step.id); });
      doc.addIn(['jobs', job, 'steps'], fresh);
    }); return steps.length;
  }
  renameStep(job: string, index: number, next: string) {
    const steps = this.snapshot.workflow!.jobs[job].steps!;
    const old = effectiveStepID(steps[index], index);
    if (next === old) return;
    if (!identifier.test(next) || stepEntries(steps).some(entry => !(entry.path.length === 2 && entry.path[1] === index) && entry.id === next)) throw new Error('Choose a unique valid step ID');
    const impact = this.deletionImpact(job, index);
    for (const path of impact) {
      const scopes = path[0] === 'schemas'
        ? this.schemaConsumers(String(path[1])).filter(consumer => this.schemaConsumerReferencesStep(consumer, job, index, old)).map(consumer => consumer.path)
        : pathJob(path) === job && inHandlerPath(path) ? [path] : [];
      if (scopes.some(scope => inHandlerPath(completedSteps(this.snapshot.workflow!, scope).get(next)?.path ?? []))) throw new Error('Choose a step ID that is not shadowed by a handler sibling');
    }
    const namedReferences = new Set(impact.filter(path => path[0] === 'schemas' && this.schemaConsumers(String(path[1])).some(consumer => consumer.job === job)).map(path => String(path[1])));
    this.edit(doc => {
      doc.setIn(['jobs', job, 'steps', index, 'id'], next);
      const renamedSchemas = new Set<string>();
      const names = Object.keys(this.snapshot.workflow!.schemas ?? {});
      for (const name of namedReferences) {
        const consumers = this.schemaConsumers(name);
        const affected = consumers.filter(consumer => this.schemaConsumerReferencesStep(consumer, job, index, old));
        let target = name;
        if (affected.length !== consumers.length) {
          // Keep the original contract for consumers with a different producer scope.
          target = this.unique(`${name}_${job}`, names); names.push(target);
          const declaration = doc.getIn(['schemas', name], true) as Node;
          const declarations = doc.getIn(['schemas'], true);
          if (!isMap(declarations)) throw new Error('Expected schema declarations');
          declarations.set(doc.createNode(target), declaration.clone());
          for (const consumer of affected) doc.setIn([...consumer.path, 'with', 'output', 'schema'], target);
        }
        renamedSchemas.add(target);
      }
      this.rewriteReferences(doc, (ref, path) => {
        if (ref.scope === 'steps' && ref.id === old && (pathJob(path) === job || path[0] === 'schemas' && renamedSchemas.has(String(path[1])))) {
          if (inHandlerPath(path)) {
            const producer = completedSteps(this.snapshot.workflow!, path).get(old);
            if (!producer || producer.path.length !== 4 || producer.path[3] !== index) return undefined;
          }
          return referenceText({ ...ref, id: next });
        }
        if ((ref.scope === 'jobs' || ref.scope === 'needs') && ref.id === job && ref.step === old) return referenceText({ ...ref, step: next });
        return undefined;
      });
    });
  }
  duplicateStep(job: string, index: number) {
    this.edit(doc => {
      this.materialize(doc, job);
      const seq = doc.getIn(['jobs', job, 'steps'], true);
      if (!isSeq(seq)) throw new Error('Missing steps');
      const node = (seq.items[index] as Node).clone() as Node;
      if (!isMap(node)) throw new Error('Expected a step mapping');
      const entries = stepEntries(this.snapshot.workflow!.jobs[job].steps);
      const copied = entries.filter(entry => entry.path[1] === index);
      const used = entries.map(entry => entry.id);
      const reserveHandlerIDs = (step: Step) => {
        for (const kind of ['onResult', 'onComplete', 'onIncomplete', 'onError', 'onFailure', 'finally'] as const) (step[kind] ?? []).forEach((child, index) => {
          used.push(effectiveStepID(child, index));
          reserveHandlerIDs(child);
        });
      };
      copied.forEach(entry => reserveHandlerIDs(entry.step));
      const renamed = new Map<string, string>();
      for (const entry of copied) {
        const id = this.unique(`${entry.id}_copy`, used); used.push(id); renamed.set(entry.id, id);
        node.setIn([...entry.path.slice(2), 'id'], id);
      }
      const rewrite = (value: Node | null, path: Path, context?: Path): boolean => {
        if (isScalar(value) && typeof value.value === 'string' && hasExpressionReferences(path)) {
          const next = mapReferences(value.value, ref => {
            if (ref.scope !== 'steps' || !renamed.has(ref.id)) return;
            if (inHandlerPath(context ?? path)) {
              const producer = completedSteps(this.snapshot.workflow!, context ?? path).get(ref.id);
              if (!producer || inHandlerPath(producer.path)) return;
            }
            return referenceText({ ...ref, id: renamed.get(ref.id)! });
          }, isExpressionPath(path));
          const changed = next !== value.value; value.value = next; return changed;
        }
        if (isSeq(value)) return value.items.map((item, i) => rewrite(item as Node, [...path, i], context)).some(Boolean);
        if (isMap(value)) return value.items.map(item => { const child = [...path, String(isScalar(item.key) ? item.key.value : item.key)]; return rewrite(item.value as Node, child, context); }).some(Boolean);
        return false;
      };
      rewrite(node, ['jobs', job, 'steps', index]);
      const names = Object.keys(this.snapshot.workflow!.schemas ?? {});
      for (const name of [...names]) {
        const consumers = this.schemaConsumers(name).filter(consumer => consumer.job === job && consumer.path[3] === index);
        if (!consumers.length) continue;
        const copies = new Map<string, string>();
        for (const consumer of consumers) {
          const declaration = (doc.getIn(['schemas', name], true) as Node).clone() as Node;
          if (!rewrite(declaration, ['schemas', name], consumer.path)) continue;
          const key = JSON.stringify(declaration.toJSON());
          let target = copies.get(key);
          if (!target) {
            target = this.unique(`${name}_copy`, names); names.push(target); copies.set(key, target);
            const declarations = doc.getIn(['schemas'], true);
            if (!isMap(declarations)) throw new Error('Expected schema declarations');
            declarations.set(doc.createNode(target), declaration);
          }
          node.setIn([...consumer.path.slice(4), 'with', 'output', 'schema'], target);
        }
      }
      seq.items.splice(index + 1, 0, node);
    });
  }
  deleteStep(job: string, index: number, confirmed = false) {
    if (this.deletionImpact(job, index).length && !confirmed) throw new Error('Deletion has references; confirmation is required');
    this.edit(doc => { this.materialize(doc, job); doc.deleteIn(['jobs', job, 'steps', index]); });
  }
  moveStep(job: string, from: number, to: number) {
    this.edit(doc => {
      this.materialize(doc, job); const seq = doc.getIn(['jobs', job, 'steps'], true);
      if (!isSeq(seq) || from < 0 || to < 0 || from >= seq.items.length || to >= seq.items.length) throw new Error('Invalid step position');
      seq.items.splice(to, 0, seq.items.splice(from, 1)[0]);
    });
  }
  setInspectorDirty(dirty: boolean) {
    if (this.disposed || this.inspectorDirty === dirty) return;
    this.inspectorDirty = dirty;
    if (dirty) this.inspectorDraftEpoch++;
    this.snapshot = { ...this.snapshot, dirty: dirty || this.snapshot.source !== this.savedSource, hasLocalChanges: dirty || this.snapshot.source !== this.protectionSource };
    this.emit();
  }
  dismissSaveError() {
    if (this.snapshot.saveError) {
      this.snapshot = { ...this.snapshot, saveError: undefined };
      this.emit();
    }
  }
  async save() {
    if (this.readOnly || !this.options.onSave || this.snapshot.saving) return;
    const onSave = this.options.onSave;
    const submitted = structuredClone({ ...this.snapshot, saving: true, saveError: undefined });
    const revision = submitted.revision;
    const protectionEpoch = this.protectionEpoch;
    this.snapshot = submitted; this.emit();
    try {
      await onSave(structuredClone(submitted));
      if (!this.disposed) {
        this.savedSource = submitted.source;
        // An older save still establishes a saved-source baseline, but must
        // never supersede a host replacement accepted while it was pending.
        if (protectionEpoch === this.protectionEpoch) this.protectionSource = submitted.source;
        this.snapshot = { ...this.snapshot, dirty: this.inspectorDirty || this.snapshot.source !== this.savedSource, hasLocalChanges: this.inspectorDirty || this.snapshot.source !== this.protectionSource };
      }
    }
    catch (error) { if (!this.disposed && revision === this.snapshot.revision) this.snapshot = { ...this.snapshot, saveError: String(error) }; }
    finally { if (!this.disposed) { this.snapshot = { ...this.snapshot, saving: false }; this.emit(); } }
  }
  async validate() {
    if (this.disposed) return;
    const previous = this.validation; const controller = new AbortController(); this.validation = controller; const epoch = ++this.epoch; previous?.abort();
    if (this.disposed || controller.signal.aborted || epoch !== this.epoch) return;
    let diagnostics: Diagnostic[] = [];
    const w = this.parsed.workflow;
    if (w) for (const [id, job] of Object.entries(w.jobs)) {
      if (job.uses && this.options.workflows && !this.options.workflows.some(item => item.id.replace(/^\.\//, '') === job.uses!.replace(/^\.\//, ''))) diagnostics.push({ code: 'workflow-catalog', message: `Workflow ${job.uses} is unavailable in the host catalog`, path: ['jobs', id, 'uses'], source: 'host', severity: 'warning' });
      stepEntries(job.steps).forEach(({ step, path }) => {
        for (const [index, role] of (step.conversation ?? []).entries()) if (this.options.agents && !this.options.agents.some(agent => agent.id === role)) diagnostics.push({ code: 'agent-catalog', message: `Agent ${role} is unavailable in the host catalog`, path: ['jobs', id, ...path, 'conversation', index], source: 'host', severity: 'warning' });
        if (stepAction(step) === 'agent/run' && this.options.agents && !this.options.agents.some(a => a.id === 'codex')) diagnostics.push({ code: 'agent-catalog', message: `Agent ${stepAction(step)} is unavailable in the host catalog`, path: ['jobs', id, ...path, Object.hasOwn(step, 'agent') ? 'agent' : 'uses'], source: 'host', severity: 'warning' });
      });
    }
    this.snapshot = { ...this.snapshot, diagnostics: [...this.parsed.diagnostics, ...diagnostics], validating: !!this.options.validate }; this.emit();
    // Notification and abort callbacks can dispose or supersede this request.
    if (this.disposed || controller.signal.aborted || epoch !== this.epoch || !this.options.validate) return;
    try { diagnostics = [...diagnostics, ...await this.options.validate(structuredClone(this.snapshot), controller.signal)]; }
    catch (error) { if (!controller.signal.aborted) diagnostics.push({ code: 'host-validation', message: String(error), path: [], severity: 'error', source: 'host' }); }
    if (!this.disposed && epoch === this.epoch) { this.snapshot = { ...this.snapshot, diagnostics: [...this.parsed.diagnostics, ...diagnostics], validating: false }; this.emit(); }
  }
  destroy() { this.disposed = true; this.validation?.abort(); this.listeners.clear(); }
}
