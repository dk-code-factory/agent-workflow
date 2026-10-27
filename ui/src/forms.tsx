import { createContext, useContext, useEffect, useId, useRef, useState } from 'react';
import { controllerParameters, dependencies, effectiveStepID } from './validation';
import { parseDocument, stringify } from 'yaml';
import type { WorkflowDocument } from './document';
import type { ActionDefinition, InputType, Mapping, Path } from './types';
import { populated, stepConversionImpact } from './conversion';
import { expressionSuggestions } from './source';
import { hasExpressionReferences } from './expressions';
import { JobResultPicker } from './result-picker';

export type ApplyResult = { ok: true } | { ok: false; error: string };
export function applyEdit(fn: () => void): ApplyResult {
  try { fn(); return { ok: true }; }
  catch (error) { return { ok: false, error: String(error) }; }
}
type InspectorDraft = { apply: () => boolean; discard: () => void; reveal: () => void; identifier: boolean; custom?: boolean; isPending: () => boolean };
export class InspectorDrafts {
  private fields = new Set<InspectorDraft>();
  private discarding = 0;
  constructor(private onPendingChange: (pending: boolean) => void) {}
  changed() { if (this.discarding) return; this.onPendingChange([...this.fields].some(field => field.isPending())); }
  register(field: InspectorDraft) { this.fields.add(field); this.changed(); return () => { this.fields.delete(field); this.changed(); }; }
  pending() { return [...this.fields].find(field => field.isPending()); }
  discard() {
    // Fallible host callbacks run before managed drafts are reset. Successful
    // host callbacks are not rolled back if a later callback fails.
    this.discarding++;
    try {
      const fields = [...this.fields];
      for (const field of [...fields.filter(field => field.custom), ...fields.filter(field => !field.custom)]) {
        if (this.fields.has(field)) field.discard();
      }
    } finally {
      if (--this.discarding === 0) this.changed();
    }
  }
  apply() {
    // Apply values before converting their type or renaming containing paths;
    // nested structural fields precede parent identifiers. Blurred values are already applied.
    const fields = [...this.fields];
    for (const field of [...fields.filter(field => !field.identifier), ...fields.filter(field => field.identifier).reverse()]) {
      if (this.fields.has(field) && !field.apply()) return field;
    }
  }
}
export const InspectorDraftContext = createContext<InspectorDrafts | undefined>(undefined);

export function Field({ path, subtree, label, value, onApply, readOnly, multiline = false, type = 'text', suggestions, suggestionValues = [], identifier = false, choices, result }: {
  path?: Path; subtree?: boolean; label: string; value: unknown; onApply: (value: string) => ApplyResult; readOnly: boolean;
  multiline?: boolean; type?: string; suggestions?: string; suggestionValues?: string[]; identifier?: boolean;
  choices?: string[]; result?: { store: WorkflowDocument; path: Path };
}) {
  const text = value == null ? '' : String(value);
  const [draft, setDraft] = useState(text);
  const [error, setError] = useState('');
  const control = useRef<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement | null>(null);
  const [pickingResult, setPickingResult] = useState(false);
  const current = useRef(text);
  const applied = useRef(text);
  const errorID = useId();
  const registry = useContext(InspectorDraftContext);
  const cursor = useRef<{ start: number; end: number } | undefined>(undefined);
  const badNumber = () => control.current instanceof HTMLInputElement && control.current.validity.badInput;
  const pending = () => current.current !== applied.current || badNumber();
  const updateDraft = (next: string) => { current.current = next; setDraft(next); registry?.changed(); };
  const discard = () => { applied.current = text; if (control.current) control.current.value = text; updateDraft(text); setError(''); };
  useEffect(() => {
    // A sibling field can change this field's stored subtree. Keep its pending
    // draft until it is applied or explicitly discarded.
    const wasPending = pending();
    applied.current = text;
    if (!wasPending || readOnly) { current.current = text; setDraft(text); setError(''); }
    registry?.changed();
  }, [text, readOnly]);
  const apply = () => {
    if (readOnly) return true;
    // Number inputs report incomplete/overflowing text as an empty value. Keep
    // that browser draft instead of interpreting it as removal of a value.
    if (badNumber()) { setError('Enter a finite number'); return false; }
    if (current.current === applied.current) { setError(''); return true; }
    const result = onApply(current.current);
    if (!result.ok) { setError(result.error); return false; }
    applied.current = current.current; registry?.changed(); setError(''); return true;
  };
  const latestApply = useRef(apply);
  latestApply.current = apply;
  const latestDiscard = useRef(discard);
  latestDiscard.current = discard;
  useEffect(() => registry?.register({ identifier, isPending: pending, apply: () => latestApply.current(), discard: () => latestDiscard.current(), reveal: () => {
    for (let parent = control.current?.parentElement; parent; parent = parent.parentElement) {
      if (parent instanceof HTMLDetailsElement) parent.open = true;
    }
    requestAnimationFrame(() => { control.current?.focus(); control.current?.closest('.aw-field')?.scrollIntoView({ block: 'nearest' }); });
  } }), [registry, identifier]);
  const props = {
    'aria-label': label, 'aria-invalid': !!error, 'aria-describedby': error ? errorID : undefined, value: draft, readOnly,
    ref: (element: HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement | null) => { control.current = element; },
    onChange: (event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) => { updateDraft(event.target.value); if (choices) apply(); },
    onBlur: () => { apply(); },
    onKeyDown: (event: React.KeyboardEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) => {
      if (event.key === 'Escape') { event.preventDefault(); discard(); }
      if (event.key === 'Enter' && !multiline) { event.preventDefault(); event.currentTarget.blur(); }
    },
  };
  const selectText = (event: React.SyntheticEvent<HTMLInputElement | HTMLTextAreaElement>) => { if (event.currentTarget.selectionStart !== null) cursor.current = { start: event.currentTarget.selectionStart, end: event.currentTarget.selectionEnd! }; };
  return <div className="aw-field" data-aw-path={path && JSON.stringify(path)} data-aw-subtree={subtree} data-aw-identifier={identifier}><label><span>{label}</span>{choices ? <select {...props} disabled={readOnly}>{choices.map(choice => <option key={choice} value={choice}>{choice || 'Not set'}</option>)}</select> : multiline
    ? <textarea {...props} rows={4} onSelect={selectText} />
    : <input {...props} type={type} step={type === 'number' ? 'any' : undefined} list={suggestions} onSelect={selectText} />}</label>
    {error && <div className="aw-field-error"><p id={errorID} role="alert">{label}: Edit has not been applied. {error} Correct it, press Escape, or discard the edit before saving.</p><button type="button" onClick={discard}>Discard edit</button></div>}
    {multiline && suggestions && suggestionValues.length > 0 && <select className="aw-expression-picker" aria-label={`Insert expression into ${label}`} value="" disabled={readOnly} onChange={event => {
      const expression = '${{ ' + event.target.value + ' }}';
      const range = cursor.current ?? { start: draft.length, end: draft.length };
      const next = draft.slice(0, range.start) + expression + draft.slice(range.end);
      updateDraft(next); apply();
      const position = range.start + expression.length;
      requestAnimationFrame(() => { control.current?.focus(); if (control.current instanceof HTMLInputElement || control.current instanceof HTMLTextAreaElement) control.current.setSelectionRange(position, position); });
    }}><option value="" disabled>Insert expression…</option>{suggestionValues.map(value => <option key={value} value={value}>{value}</option>)}</select>}
    {result && <button type="button" disabled={readOnly} aria-label={`Use job result in ${label}`} onClick={() => {
      const rejected = registry?.apply();
      if (rejected) { rejected.reveal(); return; }
      if (apply()) setPickingResult(true);
    }}>Use job result</button>}
    {pickingResult && result && <JobResultPicker {...result} selection={cursor.current} close={() => { setPickingResult(false); control.current?.focus(); }} />}
  </div>;
}
export function StructuredField({ store, path, label, replacement = false }: { store: WorkflowDocument; path: Path; label: string; replacement?: boolean }) {
  let value: unknown = store.getSnapshot().workflow ?? store.getSnapshot().previewWorkflow;
  for (const key of path) value = (value as Mapping | undefined)?.[key];
  const text = value === undefined ? '' : stringify(value).trimEnd();
  const readOnly = store.readOnly || store.getSnapshot().stale;
  return <>{replacement && <p className="aw-hint">Applying this YAML replaces the entire mapping or value, including its comments. Include every existing entry you want to keep. Guided fields change only their own values.</p>}<Field path={path} subtree label={label} value={text} readOnly={readOnly} multiline onApply={text => applyEdit(() => {
    const doc = parseDocument(text);
    if (doc.errors.length) throw new Error(doc.errors[0].message);
    const data = doc.toJS({ maxAliasCount: 0 });
    store.setField(path, text.trim() ? data : undefined);
  })} /></>;
}
function valueAt(store: WorkflowDocument, path: Path): unknown {
  let value: unknown = store.getSnapshot().workflow ?? store.getSnapshot().previewWorkflow;
  for (const key of path) value = (value as Mapping | undefined)?.[key];
  return value;
}
function applyDrafts(registry?: InspectorDrafts) {
  const rejected = registry?.apply();
  if (rejected) { rejected.reveal(); return false; }
  return true;
}
function TypedValue({ store, path, label, type = 'string', expressions = false, suggestionsID }: {
  store: WorkflowDocument; path: Path; label: string; type?: InputType; expressions?: boolean; suggestionsID?: string;
}) {
  const value = valueAt(store, path);
  const disabled = store.readOnly || store.getSnapshot().stale;
  const registry = useContext(InspectorDraftContext);
  const [preferred, setPreferred] = useState<string>();
  const mode = preferred ?? (expressions && typeof value === 'string' && value.includes('${{') ? 'expression'
    : type === 'object' || type === 'array' || (value != null && typeof value !== (type === 'string' ? 'string' : type)) ? 'yaml' : 'literal');
  return <>
    <label className="aw-field"><span>{label} editor</span><select aria-label={`${label} editor`} value={mode} disabled={disabled} onChange={event => { if (applyDrafts(registry)) setPreferred(event.target.value); }}>
      <option value="literal">Literal</option>{expressions && <option value="expression">Expression</option>}<option value="yaml">YAML</option>
    </select></label>
    {mode === 'yaml' || (mode === 'literal' && (type === 'object' || type === 'array'))
      ? <StructuredField store={store} path={path} label={`${label} (YAML)`} />
      : <Field path={path} label={label} value={value} readOnly={disabled} type={mode === 'literal' && type === 'number' ? 'number' : 'text'}
        choices={mode === 'literal' && type === 'boolean' ? ['', 'true', 'false'] : undefined}
        suggestions={expressions ? suggestionsID : undefined} result={expressions && (mode === 'expression' || type === 'string') ? { store, path } : undefined}
        onApply={text => applyEdit(() => {
          let next: unknown = text;
          if (mode === 'literal' && type !== 'string') {
            next = text.trim() === '' ? undefined : type === 'boolean' ? text === 'true' : Number(text);
            if (typeof next === 'number' && !Number.isFinite(next)) throw new Error('Enter a finite number');
          }
          store.setField(path, next);
        })} />}
  </>;
}
function CommandArguments({ store, path, suggestionsID, attempt }: { store: WorkflowDocument; path: Path; suggestionsID: string; attempt: (fn: () => void) => void }) {
  const argv = valueAt(store, path) as string[] | undefined ?? [];
  const disabled = store.readOnly || store.getSnapshot().stale;
  const registry = useContext(InspectorDraftContext);
  const change = (fn: () => void) => { if (applyDrafts(registry)) attempt(fn); };
  return <fieldset disabled={disabled}><legend>Command arguments</legend>
    <Field path={[...path, 0]} label="Executable" value={argv[0]} readOnly={disabled} suggestions={suggestionsID} result={{ store, path: [...path, 0] }} onApply={text => applyEdit(() => {
      if (!text.length) throw new Error('Executable is required');
      store.setField([...path, 0], text);
    })} />
    {argv.slice(1).map((arg, index) => <div key={index}>
      <Field path={[...path, index + 1]} label={`Argument ${index + 1}`} value={arg} readOnly={disabled} suggestions={suggestionsID} result={{ store, path: [...path, index + 1] }} onApply={text => applyEdit(() => store.setField([...path, index + 1], text))} />
      <div className="aw-button-row">
        <button type="button" disabled={disabled || index === 0} aria-label={`Move argument ${index + 1} up`} onClick={() => change(() => store.moveArgument(path, index + 1, index))}>Move up</button>
        <button type="button" disabled={disabled || index === argv.length - 2} aria-label={`Move argument ${index + 1} down`} onClick={() => change(() => store.moveArgument(path, index + 1, index + 2))}>Move down</button>
        <button type="button" aria-label={`Remove argument ${index + 1}`} onClick={() => change(() => store.moveArgument(path, index + 1))}>Remove</button>
      </div>
    </div>)}
    <button type="button" onClick={() => change(() => {
      const current = valueAt(store, path) as string[] | undefined;
      if (!current?.length) store.setField(path, ['', '']);
      else store.setField([...path, current.length], '');
    })}>Add argument</button>
    <details><summary>Advanced command YAML</summary><StructuredField store={store} path={path} label="Arguments (YAML array)" replacement /></details>
  </fieldset>;
}
function CallFields({ store, job, suggestionsID }: { store: WorkflowDocument; job: string; suggestionsID: string }) {
  const current = (store.getSnapshot().workflow ?? store.getSnapshot().previewWorkflow)!.jobs[job];
  const metadata = store.getOptions().workflows?.find(entry => entry.id.replace(/^\.\//, '') === current.uses?.replace(/^\.\//, ''))?.interface;
  const disabled = store.readOnly || store.getSnapshot().stale;
  const registry = useContext(InspectorDraftContext);
  const [secretError, setSecretError] = useState('');
  return <>
    {!metadata && <p className="aw-hint">The host supplied no interface metadata for this workflow. Use Advanced call YAML to edit input and secret bindings.</p>}
    {metadata && <>
      {Object.entries(metadata.inputs ?? {}).map(([key, spec]) => <fieldset key={key} disabled={disabled}><legend>{key}{spec.required ? ' (required)' : ''}</legend>
        {spec.default !== undefined && <p className="aw-hint">Default (used when omitted): {stringify(spec.default).trimEnd()}</p>}
        <TypedValue store={store} path={['jobs', job, 'with', key]} label={`Call input: ${key}`} type={spec.type} expressions suggestionsID={suggestionsID} />
      </fieldset>)}
      {current.secrets === 'inherit' ? <><p className="aw-hint">Secrets are inherited. Named bindings replace inherit only when you choose to configure them.</p><button type="button" disabled={disabled} onClick={() => {
        if (applyDrafts(registry)) { const result = applyEdit(() => store.setField(['jobs', job, 'secrets'], {})); setSecretError(result.ok ? '' : result.error); }
      }}>Configure named secrets</button>{secretError && <p role="alert">{secretError}</p>}</>
        : Object.entries(metadata.secrets ?? {}).map(([key, spec]) => <fieldset key={key} disabled={disabled}><legend>{key}{spec.required ? ' (required)' : ''}</legend>
          <TypedValue store={store} path={['jobs', job, 'secrets', key]} label={`Call secret: ${key}`} expressions suggestionsID={suggestionsID} />
        </fieldset>)}
    </>}
    <details><summary>Advanced call YAML</summary><StructuredField store={store} path={['jobs', job, 'with']} label="Call inputs (YAML)" replacement /><StructuredField store={store} path={['jobs', job, 'secrets']} label="Call secrets (YAML)" replacement /></details>
  </>;
}
function NamedFields({ store, path, kind, title, requestDelete, suggestionsID }: { store: WorkflowDocument; path: Path; requestDelete: (path: Path) => void; suggestionsID: string; kind: 'inputs' | 'secrets' | 'outputs' | 'jobOutputs'; title: string }) {
  const registry = useContext(InspectorDraftContext);
  const snapshot = store.getSnapshot();
  let current: unknown = snapshot.workflow ?? snapshot.previewWorkflow;
  for (const key of path) current = (current as Mapping | undefined)?.[key];
  const entries = current && typeof current === 'object' && !Array.isArray(current) ? current as Mapping : {};
  const disabled = store.readOnly || snapshot.stale;
  const set = (key: string, suffix: Path, value: unknown) => store.setField([...path, key, ...suffix], value);
  return <details><summary>{title}</summary>{Object.entries(entries).map(([key, item]) => {
    const value = item && typeof item === 'object' ? item as Mapping : {};
    return <fieldset key={key} disabled={disabled}><legend>{key}</legend>
      <Field path={[...path, key]} identifier label={`Identifier: ${key}`} value={key} readOnly={disabled} onApply={next => applyEdit(() => store.renameEntry([...path, key], next))} />
      {kind === 'inputs' && <><label className="aw-field" data-aw-path={JSON.stringify([...path, key, 'type'])}><span>Input type: {key}</span><select aria-label={`Input type: ${key}`} value={String(value.type ?? 'string')} onChange={e => { if (applyDrafts(registry)) set(key, ['type'], e.target.value); }}>{['string', 'boolean', 'number', 'object', 'array'].map(type => <option key={type}>{type}</option>)}</select></label><TypedValue key={String(value.type)} store={store} path={[...path, key, 'default']} label={`Default: ${key}`} type={value.type as InputType} /></>}
      {(kind === 'inputs' || kind === 'secrets') && <label className="aw-check" data-aw-path={JSON.stringify([...path, key, 'required'])}><input type="checkbox" checked={!!value.required} onChange={e => set(key, ['required'], e.target.checked)} />Required: {key}</label>}
      {(kind === 'outputs' || kind === 'jobOutputs') && <Field path={[...path, key, ...(kind === 'outputs' ? ['value'] : [])]} result={{ store, path: [...path, key, ...(kind === 'outputs' ? ['value'] : [])] }} suggestions={suggestionsID} label={`Output value: ${key}`} value={kind === 'outputs' ? value.value : item} readOnly={disabled} onApply={text => applyEdit(() => set(key, kind === 'outputs' ? ['value'] : [], text))} />}
      <button type="button" onClick={() => requestDelete([...path, key])}>Delete {key}</button>
    </fieldset>;
  })}<button type="button" disabled={disabled} onClick={() => {
    const base = kind === 'inputs' ? 'input' : kind === 'secrets' ? 'secret' : 'output';
    let key = base; let i = 2; while (Object.hasOwn(entries, key)) key = `${base}_${i++}`;
    store.setField([...path, key], kind === 'inputs' ? { type: 'string', required: false } : kind === 'secrets' ? { required: true } : kind === 'outputs' ? { value: '' } : '');
  }}>Add {kind === 'inputs' ? 'input' : kind === 'secrets' ? 'secret' : 'output'}</button></details>;
}
function CustomForm({ definition, store, job, index }: { definition: ActionDefinition; store: WorkflowDocument; job: string; index: number }) {
  const ref = useRef<HTMLDivElement>(null);
  const registry = useContext(InspectorDraftContext);
  const [, refresh] = useState(0);
  const rendered = useRef<{ step: string; generation: number; render: ActionDefinition['renderInspector']; job: string; index: number; uses: string | undefined; id: string | undefined; readOnly: boolean; pending: () => boolean; dispose: () => void } | undefined>(undefined);
  const step = (store.getSnapshot().workflow ?? store.getSnapshot().previewWorkflow)!.jobs[job].steps![index];
  const readOnly = store.readOnly || store.getSnapshot().stale;
  const generation = store.getInspectorGeneration();
  useEffect(() => () => { rendered.current?.dispose(); rendered.current = undefined; }, [store, registry]);
  // Reconcile explicitly: effect cleanup on every step/catalog change would
  // destroy raw host buffers before a replacement choice could protect them.
  useEffect(() => {
    const previous = rendered.current;
    const text = JSON.stringify(step);
    if (previous && previous.generation === generation && previous.job === job && previous.index === index && previous.uses === step.uses && previous.id === step.id && previous.readOnly === readOnly &&
      (previous.pending() || (previous.step === text && previous.render === definition.renderInspector))) return;
    rendered.current = undefined; previous?.dispose();
    const element = ref.current!;
    let active = true; let pending = false; let discarding = false;
    let discard: (() => void) | undefined;
    const canEdit = () => {
      const snapshot = store.getSnapshot();
      const current = snapshot.workflow?.jobs[job]?.steps?.[index];
      return active && generation === store.getInspectorGeneration() && !discarding && !readOnly && !snapshot.stale && !!current && current.id === step.id && current.uses === step.uses;
    };
    const unregister = registry?.register({ identifier: false, custom: true, isPending: () => pending, apply: () => true, reveal: () => {}, discard: () => {
      if (!active || discarding || generation !== store.getInspectorGeneration()) return;
      discarding = true;
      try { if (pending) discard?.(); pending = false; }
      finally { discarding = false; registry?.changed(); }
    } });
    const cleanup = definition.renderInspector?.(element, {
      step: structuredClone(step), path: ['jobs', job, 'steps', index], readOnly,
      setField: (path, value) => { if (canEdit()) store.setField(['jobs', job, 'steps', index, ...path], value); },
      reportDraft: (next, onDiscard) => {
        if (!canEdit()) return;
        const cleared = pending && !next;
        pending = next; discard = onDiscard; registry?.changed();
        // Aggregate pending state may stay true because a built-in field is
        // still pending. Reconcile deferred host updates when this one clears.
        if (cleared) refresh(value => value + 1);
      },
    });
    rendered.current = { step: text, generation, render: definition.renderInspector, job, index, uses: step.uses, id: step.id, readOnly, pending: () => pending, dispose: () => {
      active = false; unregister?.(); cleanup?.(); element.replaceChildren();
    } };
  });
  return <div className="aw-custom-form" ref={ref} />;
}
export type Selection = { job?: string; step?: number };
export function Inspector({ store, selection, select, attempt, requestDelete, requestEntryDelete, suggestionsID }: {
  store: WorkflowDocument; selection: Selection; select: (selection: Selection) => void;
  attempt: (fn: () => void) => void; requestDelete: (job: string, step?: number) => void; requestEntryDelete: (path: Path) => void; suggestionsID: string;
}) {
  const snapshot = store.getSnapshot(); const w = snapshot.workflow ?? snapshot.previewWorkflow;
  const disabled = store.readOnly || snapshot.stale;
  const job = selection.job && w && Object.hasOwn(w.jobs, selection.job) ? w.jobs[selection.job] : undefined;
  const step = selection.step == null ? undefined : job?.steps?.[selection.step];
  const path: Path = selection.job ? ['jobs', selection.job, ...(step ? ['steps', selection.step!] : [])] : [];
  const obj = step ?? job ?? w;
  const field = (label: string, key: string, multiline = false) => <Field path={[...path, key]} key={key} label={label} value={obj?.[key]} readOnly={disabled} multiline={multiline} suggestionValues={expressionSuggestions(w, selection.job, selection.step)} suggestions={hasExpressionReferences([...path, key]) ? suggestionsID : undefined} result={hasExpressionReferences([...path, key]) ? { store, path: [...path, key] } : undefined} onApply={value => applyEdit(() => store.setField([...path, key], value || undefined))} />;
  const advanced = (key: string, label = `${key} (YAML)`) => <StructuredField key={key} store={store} path={[...path, key]} label={label} replacement />;
  const common = <>{field('Condition', 'if')}<label className="aw-check" data-aw-path={JSON.stringify([...path, 'continue-on-error'])}><input type="checkbox" disabled={disabled} checked={!!obj?.['continue-on-error']} onChange={e => attempt(() => store.setField([...path, 'continue-on-error'], e.target.checked))} />Continue on error</label></>;
  const action = step && store.getOptions().actions?.find(a => a.uses === step.uses);
  const params = step?.with ?? {};
  const commandKind = step && (step.argv?.length ? 'argv' : step.run || step.shell ? 'shell' : step.uses ? undefined : step.argv ? 'argv' : step.run !== undefined ? 'shell' : undefined);
  const paramField = (key: string, label: string, multiline = false, type?: string) => <Field path={[...path, 'with', key]} key={key} label={label} value={params[key]} readOnly={disabled} multiline={multiline} type={type} suggestionValues={expressionSuggestions(w, selection.job, selection.step)} suggestions={suggestionsID} result={{ store, path: [...path, 'with', key] }} onApply={value => applyEdit(() => store.setField([...path, 'with', key], value || undefined))} />;
  const confirmConversion = (fields: string[]) => !fields.length || window.confirm(`Changing type will discard:\n${fields.map(field => `• ${field}`).join('\n')}\n\nContinue? Undo can restore the complete previous definition.`);
  const changeStepKind = (kind: string) => {
    const currentStep = store.getSnapshot().workflow?.jobs[selection.job!]?.steps?.[selection.step!];
    if (!currentStep) throw new Error('Unknown step');
    if (!confirmConversion(stepConversionImpact(currentStep, kind))) return false;
    store.setStepKind(selection.job!, selection.step!, kind); return true;
  };
  return <div className="aw-inspector-content">
    <div className="aw-inspector-heading"><h2 tabIndex={-1}>{step ? 'Step' : job ? 'Job' : 'Workflow'}</h2>{selection.job && <button type="button" onClick={() => select(step ? { job: selection.job } : {})}>Back</button>}</div>
    {!w ? <p>Enter a workflow to begin.</p> : <>
      {field('Name', 'name')}
      {job && !step && <>
        <Field path={path} identifier label="Job ID" value={selection.job} readOnly={disabled} onApply={value => applyEdit(() => { store.renameJob(selection.job!, value); select({ job: value }); })} />
        <div className="aw-button-row"><button type="button" disabled={disabled} onClick={() => attempt(() => select({ job: store.duplicateJob(selection.job!) }))}>Duplicate job</button><button type="button" disabled={disabled} onClick={() => requestDelete(selection.job!)}>Delete job</button></div>
        {common}{field('Runner', 'runs-on')}
        <div tabIndex={-1} data-aw-subtree="true" data-aw-path={JSON.stringify([...path, 'needs'])}><fieldset disabled={disabled}><legend>Dependencies</legend>{Object.keys(w.jobs).filter(id => id !== selection.job).map(id => <label className="aw-check" key={id}><input type="checkbox" checked={dependencies(job).includes(id)} onChange={e => attempt(() => e.target.checked ? store.connect(id, selection.job!) : store.disconnect(id, selection.job!))} />{id}</label>)}</fieldset></div>
        <label className="aw-field"><span>Job kind</span><select aria-label="Job kind" disabled={disabled} value={job.uses ? 'reusable' : 'steps'} onChange={e => attempt(() => {
          const reusable = e.target.value === 'reusable';
          const discarded = (reusable ? ['steps'] : ['uses', 'with', 'secrets']).filter(key => populated(job[key]));
          if (confirmConversion(discarded)) store.setJobKind(selection.job!, reusable);
        })}><option value="steps">Sequential steps</option><option value="reusable">Reusable workflow</option></select></label>
        {job.uses && field('Reusable workflow', 'uses')}
        {job.uses && <CallFields key={job.uses} store={store} job={selection.job!} suggestionsID={suggestionsID} />}
        <NamedFields suggestionsID={suggestionsID} requestDelete={requestEntryDelete} store={store} path={[...path, 'outputs']} kind="jobOutputs" title="Job outputs" /><details><summary>Advanced output YAML</summary>{advanced('outputs', 'Job outputs (YAML)')}</details>
        <details data-aw-path={JSON.stringify([...path, 'repeat'])}><summary>Repeat</summary><Field path={[...path, 'repeat', 'max-attempts']} label="Maximum attempts" value={job.repeat?.['max-attempts']} type="number" readOnly={disabled} onApply={v => applyEdit(() => store.setField([...path, 'repeat', 'max-attempts'], Number(v)))} /><Field path={[...path, 'repeat', 'until']} result={{ store, path: [...path, 'repeat', 'until'] }} label="Until" value={job.repeat?.until} readOnly={disabled} suggestions={suggestionsID} onApply={v => applyEdit(() => store.setField([...path, 'repeat', 'until'], v))} /><button type="button" disabled={disabled || !job.repeat} onClick={() => attempt(() => store.setField([...path, 'repeat'], undefined))}>Remove repeat</button></details>
        {!job.uses && <button type="button" disabled={disabled} onClick={() => attempt(() => select({ job: selection.job, step: store.addStep(selection.job!) }))}>Add step</button>}
        {job.steps?.map((s, i) => <button type="button" className="aw-step-link" key={i} onClick={() => select({ job: selection.job, step: i })}>{i + 1}. {s.name || effectiveStepID(s, i)}</button>)}
      </>}
      {step && <>
        <Field path={[...path, 'id']} identifier label="Step ID" value={effectiveStepID(step, selection.step!)} readOnly={disabled} onApply={value => applyEdit(() => store.renameStep(selection.job!, selection.step!, value))} />
        <div className="aw-button-row"><button type="button" disabled={disabled} onClick={() => attempt(() => store.duplicateStep(selection.job!, selection.step!))}>Duplicate step</button><button type="button" disabled={disabled} onClick={() => requestDelete(selection.job!, selection.step)}>Delete step</button><button type="button" disabled={disabled || selection.step === 0} onClick={() => attempt(() => { store.moveStep(selection.job!, selection.step!, selection.step! - 1); select({ ...selection, step: selection.step! - 1 }); })}>Move up</button><button type="button" disabled={disabled || selection.step === job!.steps!.length - 1} onClick={() => attempt(() => { store.moveStep(selection.job!, selection.step!, selection.step! + 1); select({ ...selection, step: selection.step! + 1 }); })}>Move down</button></div>
        {common}
        <label className="aw-field" data-aw-path={!step.uses ? JSON.stringify([...path, 'uses']) : undefined}><span>Step type</span><select disabled={disabled} aria-label="Step type" value={commandKind ?? step.uses ?? 'agent/claude'} onChange={e => attempt(() => { changeStepKind(e.target.value); })}>{Array.from(new Set(['argv', 'shell', ...(store.getOptions().agents ?? [{ id: 'claude' }, { id: 'codex' }]).map(a => `agent/${a.id}`), ...Object.keys(controllerParameters), ...(store.getOptions().actions ?? []).map(a => a.uses), ...(step.uses ? [step.uses] : [])])).map(kind => <option key={kind} value={kind}>{kind === 'argv' ? 'Direct command' : kind === 'shell' ? 'Shell command' : store.getOptions().actions?.find(a => a.uses === kind)?.label ?? kind}</option>)}</select></label>
        {commandKind === 'argv' && <CommandArguments store={store} path={[...path, 'argv']} suggestionsID={suggestionsID} attempt={attempt} />}
        {commandKind === 'shell' && <>{field('Shell', 'shell')}{field('Command', 'run', true)}</>}
        {commandKind && <>{field('Working directory', 'working-directory')}{field('Timeout', 'timeout')}</>}
        {step.uses && <Field path={[...path, 'uses']} identifier label="Action target" value={step.uses} readOnly={disabled} onApply={value => applyEdit(() => {
          if (!value.trim()) throw new Error('Action target is required');
          if (!changeStepKind(value)) throw new Error('Type change cancelled; the previous configuration is unchanged');
        })} />}
        {step.uses?.startsWith('agent/') && <>{paramField('prompt', 'Prompt', true)}<details open={params.message !== undefined}><summary>Optional message</summary>{paramField('message', 'Message', true)}</details>{paramField('model', 'Model')}{paramField('workspace', 'Workspace')}<details open={params.output !== undefined}><summary>Structured output</summary><StructuredField store={store} path={[...path, 'with', 'output']} label="Structured output contract (YAML)" /></details></>}
        {(Object.hasOwn(controllerParameters, step.uses ?? '') ? controllerParameters[step.uses!] : undefined)?.map(key => key === 'independent' ? <label className="aw-check" data-aw-path={JSON.stringify([...path, 'with', key])} key={key}><input type="checkbox" disabled={disabled} checked={!!params[key]} onChange={e => attempt(() => store.setField([...path, 'with', key], e.target.checked))} />Independent workspace</label> : key === 'commits' ? <StructuredField key={key} store={store} path={[...path, 'with', key]} label="Commits (YAML)" /> : paramField(key, key[0].toUpperCase() + key.slice(1), key === 'body'))}
        {action?.fields?.map(f => f.type === 'yaml' ? <StructuredField key={f.key} store={store} path={[...path, 'with', f.key]} label={f.label} /> : f.type === 'number' ? <TypedValue key={f.key} store={store} path={[...path, 'with', f.key]} label={f.label} type="number" expressions suggestionsID={suggestionsID} /> : f.type === 'boolean' ? <label className="aw-check" data-aw-path={JSON.stringify([...path, 'with', f.key])} key={f.key}><input type="checkbox" disabled={disabled} checked={!!params[f.key]} onChange={e => attempt(() => store.setField([...path, 'with', f.key], e.target.checked))} />{f.label}</label> : paramField(f.key, f.label, f.type === 'textarea'))}
        {action?.renderInspector && <CustomForm definition={action} store={store} job={selection.job!} index={selection.step!} />}
        <details><summary>Advanced parameters</summary>{advanced('with', 'Parameters (YAML)')}</details>
      </>}
      {!job && <>
        <fieldset disabled={disabled}><legend>Triggers</legend>{['manual', 'workflow_call'].map(trigger => <label key={trigger} className="aw-check" data-aw-path={JSON.stringify(['on', trigger])}><input type="checkbox" checked={!!w.on && Object.hasOwn(w.on, trigger)} onChange={e => attempt(() => store.setField(['on', trigger], e.target.checked ? {} : undefined))} />{trigger}</label>)}</fieldset>
        {!!w.on?.workflow_call && <><NamedFields suggestionsID={suggestionsID} requestDelete={requestEntryDelete} store={store} path={['on', 'workflow_call', 'inputs']} kind="inputs" title="Callable inputs" /><NamedFields suggestionsID={suggestionsID} requestDelete={requestEntryDelete} store={store} path={['on', 'workflow_call', 'secrets']} kind="secrets" title="Callable secrets" /><NamedFields suggestionsID={suggestionsID} requestDelete={requestEntryDelete} store={store} path={['on', 'workflow_call', 'outputs']} kind="outputs" title="Callable outputs" /></>}
        <details><summary>Advanced trigger and interface YAML</summary>{advanced('on', 'Triggers and callable interface (YAML)')}</details>
        <p className="aw-hint">Use callable inputs to define the values supplied when this workflow is called.</p>
      </>}
    </>}
    <datalist id={suggestionsID}>{expressionSuggestions(w, selection.job, selection.step).map(value => <option key={value} value={'${{ ' + value + ' }}'} />)}</datalist>
  </div>;
}
