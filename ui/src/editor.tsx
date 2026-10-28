import { useEffect, useId, useMemo, useRef, useState, useSyncExternalStore, type CSSProperties } from 'react';
import { ReactFlow, ReactFlowProvider, Background, Controls, Handle, Position, useReactFlow, useNodesInitialized, type NodeProps, type Node, type Edge, MarkerType } from '@xyflow/react';
import dagre from '@dagrejs/dagre';
import { WorkflowDocument } from './document';
import { dependencies, diagnosticLabel, effectiveStepID } from './validation';
import { Inspector, InspectorDraftContext, InspectorDrafts, type Selection } from './forms';
import { SourceEditor } from './source';
import type { Diagnostic, Job, LayoutState, Path, Step, WorkflowOptions } from './types';
import '@xyflow/react/dist/style.css';
import './style.css';

type CardData = { job: Job; id: string; collapsed: boolean; readOnly: boolean; selectedStep?: number; select: (step?: number) => void; toggle: () => void };
type JobNode = Node<CardData, 'job'>;
type SearchResult = { job: string; step?: number; label: string; detail: string };
function searchText(value: unknown): string {
  if (Array.isArray(value)) return value.map(searchText).join(' ');
  if (value && typeof value === 'object') return Object.entries(value).map(([key, item]) => `${key} ${searchText(item)}`).join(' ');
  return String(value ?? '');
}
type PendingDeletion = { impact: string[]; revision: number } & ({ job: string; step?: number } | { path: Path });
function JobCard({ data, selected }: NodeProps<JobNode>) {
  return <article className={`aw-card ${selected ? 'aw-selected' : ''}`} aria-label={`Job ${data.id}`}>
    <Handle type="target" position={Position.Top} isConnectable={!data.readOnly} />
    <div className="aw-card-title"><button type="button" className="nodrag" aria-label={`Inspect job ${data.id}`} onClick={() => data.select()}>{data.job.name || data.id}{data.job.name && <small>{data.id}</small>}</button><button type="button" className="nodrag" aria-expanded={!data.collapsed} aria-label={`Expand ${data.id}`} onClick={data.toggle}>{data.collapsed ? '+' : '−'}</button></div>
    <div className="aw-badges">{data.job.if && <span title={data.job.if}>if</span>}{data.job.repeat && <span>repeat ≤ {data.job.repeat['max-attempts']}</span>}{data.job.uses && <span title={data.job.uses}>reusable call</span>}{!!data.job['continue-on-error'] && <span>continue on error</span>}</div>
    {!data.collapsed && <ol className="aw-steps">{data.job.steps?.map((step, i) => <li key={i}><button type="button" className={`nodrag ${data.selectedStep === i ? 'aw-active-step' : ''}`} onClick={() => data.select(i)}>{step.name || effectiveStepID(step, i)}<small>{step.uses || (step.argv ? 'direct command' : 'shell command')}{step.if ? ' · if' : ''}{step['continue-on-error'] ? ' · continue on error' : ''}</small></button></li>)}{data.job.uses && <li><code>{data.job.uses}</code></li>}</ol>}
    <Handle type="source" position={Position.Bottom} isConnectable={!data.readOnly} />
  </article>;
}
const nodeTypes = { job: JobCard };
export function automaticLayout(jobs: Record<string, Job>, collapsed: LayoutState['collapsed'] = {}): LayoutState['positions'] {
  const graph = new dagre.graphlib.Graph().setGraph({ rankdir: 'TB', nodesep: 64, ranksep: 96 }).setDefaultEdgeLabel(() => ({}));
  // Graphlib uses object dictionaries internally. Map workflow IDs to private
  // keys so valid IDs such as __proto__ cannot affect its layout bookkeeping.
  const keys = new Map(Object.keys(jobs).map((id, index) => [id, `job-${index}`]));
  for (const [id, job] of Object.entries(jobs)) graph.setNode(keys.get(id)!, { width: 280, height: (Object.hasOwn(collapsed, id) && collapsed[id]) ? 100 : 120 + 60 * (job.steps?.length ?? 1) });
  for (const [id, job] of Object.entries(jobs)) for (const dep of dependencies(job)) if (keys.has(dep)) graph.setEdge(keys.get(dep)!, keys.get(id)!);
  dagre.layout(graph);
  return Object.fromEntries(Object.keys(jobs).map(id => { const node = graph.node(keys.get(id)!); return [id, { x: node.x - node.width / 2, y: node.y - node.height / 2 }]; }));
}
const defaultTemplates: { label: string; step: Step }[] = [
    { label: 'Direct command', step: { argv: ['echo', 'Hello'] } },
  { label: 'Shell command', step: { shell: 'sh', run: 'echo Hello' } },
  ...['workspace/create', 'workspace/snapshot', 'workspace/integrate', 'git/commit', 'github/pr'].map(uses => ({ label: uses, step: { uses, with: {} } })),
];
function EditorSurface({ store }: { store: WorkflowDocument }) {
  const snapshot = useSyncExternalStore(store.subscribe, store.getSnapshot, store.getSnapshot);
  const [selection, select] = useState<Selection>({});
  const [sourceNavigation, navigateSource] = useState(0);
  const [sourceTarget, setSourceTarget] = useState<{ from: number; to: number; revision: number }>();
  const [diagnosticTarget, setDiagnosticTarget] = useState<{ diagnostic: Diagnostic; previous: Selection }>();
  const [navigationNotice, setNavigationNotice] = useState('');
  const [mode, setMode] = useState<'visual' | 'yaml' | 'split'>('split');
  const modeChosen = useRef(false);
  const [panel, setPanel] = useState<'canvas' | 'palette' | 'inspector'>('canvas');
  const [geometry, setGeometry] = useState<Record<string, Pick<JobNode, 'measured' | 'dragging'>>>({});
  const [query, setQuery] = useState(''); const [paletteQuery, setPaletteQuery] = useState('');
  const searchInput = useRef<HTMLInputElement>(null);
  const searchResults = useRef<HTMLUListElement>(null);
  const [searchTarget, setSearchTarget] = useState<{ job: string; step?: number }>();
  const term = query.trim().toLowerCase();
  const [error, setError] = useState('');
  const [deletion, setDeletion] = useState<PendingDeletion>();
  const flow = useReactFlow<JobNode>(); const id = useId();
  const canvas = useRef<HTMLDivElement>(null);
  const nodesInitialized = useNodesInitialized();
  const graphWasObserved = useRef(false);
  const pendingResizeFit = useRef(false);
  const [fitRequest, requestFit] = useState(0);
  const root = useRef<HTMLElement>(null);
  const returnFocus = useRef<HTMLElement | null>(null);
  const pendingFocus = useRef<string | undefined>(undefined);
  const [narrow, setNarrow] = useState(false);
  const options = store.getOptions(); const w = snapshot.workflow ?? snapshot.previewWorkflow;
  const results = useMemo(() => {
    const matches: SearchResult[] = [];
    if (!term) return matches;
    for (const [jobID, job] of Object.entries(w?.jobs ?? {})) {
      const { steps, ...metadata } = job;
      if (`${jobID} ${searchText(metadata)}`.toLowerCase().includes(term)) matches.push({ job: jobID, label: job.name || jobID, detail: `Job ${jobID}` });
      steps?.forEach((step, index) => {
        const stepID = effectiveStepID(step, index);
        if (`${stepID} ${searchText(step)}`.toLowerCase().includes(term)) matches.push({ job: jobID, step: index, label: step.name || stepID, detail: `Step ${stepID} · Job ${job.name ? `${job.name} (${jobID})` : jobID}` });
      });
    }
    return matches;
  }, [w, term]);
  const matchingJobs = new Set(results.map(result => result.job));
  const disabled = store.readOnly || snapshot.stale;
  const drafts = useMemo(() => new InspectorDrafts(pending => store.setInspectorDirty(pending)), [store]);
  const attempt = (fn: () => void) => { try { fn(); setError(''); } catch (e) { setError(String(e)); } };
  // Pointer Save keeps the active field focused until this preflight. A blur
  // rename can otherwise scroll the page between pointer down and click.
  const save = () => {
    if (store.readOnly || !store.getOptions().onSave || store.getSnapshot().saving) return;
    const rejected = drafts.apply();
    if (rejected) { setPanel('inspector'); rejected.reveal(); return; }
    void store.save();
  };
  const choose = (next: Selection) => {
    setSourceTarget(undefined);
    setSearchTarget(undefined);
    if (next.job && !Object.hasOwn(geometry, next.job)) pendingFocus.current = next.job;
    select(next); navigateSource(value => value + 1); setPanel('inspector');
  };
  useEffect(() => { if (selection.job && (!w || !Object.hasOwn(w.jobs, selection.job))) select({}); }, [w, selection.job]);
  useEffect(() => { if (narrow && !modeChosen.current) setMode('visual'); }, [narrow]);
  const defaults = useMemo(() => automaticLayout(w?.jobs ?? {}, store.layout.collapsed), [w, store.layout.collapsed]);
  useEffect(() => {
    const missing = Object.keys(w?.jobs ?? {}).filter(key => !Object.hasOwn(store.layout.positions, key));
    if (missing.length) {
      const positions = { ...store.layout.positions };
      for (const key of missing) {
        const candidate = { ...defaults[key] };
        const height = 120 + 60 * (w!.jobs[key].steps?.length ?? 1);
        const overlaps = () => Object.entries(positions).some(([other, position]) => {
          if (!Object.hasOwn(w!.jobs, other)) return false;
          const otherHeight = Object.hasOwn(geometry, other) ? geometry[other].measured?.height ?? 180 : 120 + 60 * (w!.jobs[other].steps?.length ?? 1);
          return candidate.x < position.x + 312 && candidate.x + 312 > position.x && candidate.y < position.y + otherHeight + 32 && candidate.y + height + 32 > position.y;
        });
        while (overlaps()) candidate.x += 344;
        Object.defineProperty(positions, key, { value: candidate, enumerable: true, writable: true, configurable: true });
      }
      store.setLayout({ ...store.layout, positions });
    }
  }, [store, w, defaults]);
  useEffect(() => {
    const job = pendingFocus.current;
    if (job && Object.hasOwn(geometry, job) && geometry[job].measured) {
      pendingFocus.current = undefined;
      void flow.fitView({ nodes: [{ id: job }], maxZoom: 1, padding: 0.2, duration: 0 });
    }
  }, [geometry, flow]);
  const activateResult = (result: SearchResult) => {
    choose({ job: result.job, step: result.step });
    pendingFocus.current = undefined;
    if (result.step !== undefined && Object.hasOwn(store.layout.collapsed, result.job) && store.layout.collapsed[result.job]) {
      store.setLayout({ ...store.layout, collapsed: { ...store.layout.collapsed, [result.job]: false } });
    }
    setSearchTarget({ job: result.job, step: result.step });
  };
  useEffect(() => {
    if (!searchTarget || !nodesInitialized || !flow.viewportInitialized || mode === 'yaml') return;
    if (!w || !Object.hasOwn(w.jobs, searchTarget.job) || (searchTarget.step !== undefined && !w.jobs[searchTarget.job].steps?.[searchTarget.step])) {
      setSearchTarget(undefined);
      return;
    }
    const frame = requestAnimationFrame(() => {
      const node = canvas.current?.querySelector<HTMLElement>(`.react-flow__node[data-id="${CSS.escape(searchTarget.job)}"]`);
      // Expansion and selection change the card height. Wait for React Flow's
      // ResizeObserver measurement before using the step's updated position.
      if (!node || geometry[searchTarget.job]?.measured?.height !== node.offsetHeight) return;
      if (searchTarget.step === undefined) {
        void flow.fitView({ nodes: [{ id: searchTarget.job }], maxZoom: 1, padding: 0.2, duration: 0 });
      } else {
        const step = node.querySelector<HTMLElement>(`.aw-steps > li:nth-child(${searchTarget.step + 1}) > button`);
        if (!step || !canvas.current) return;
        const bounds = step.getBoundingClientRect();
        const center = flow.screenToFlowPosition({ x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height / 2 });
        const zoom = Math.max(0.15, Math.min(1, (canvas.current.clientWidth - 32) / node.offsetWidth, (canvas.current.clientHeight - 32) / step.offsetHeight));
        void flow.setCenter(center.x, center.y, { zoom, duration: 0 });
      }
      setSearchTarget(undefined);
    });
    return () => cancelAnimationFrame(frame);
  }, [searchTarget, geometry, nodesInitialized, flow, flow.viewportInitialized, mode, w]);
  const clearSearch = () => { setQuery(''); searchInput.current?.focus(); };
  const focusResult = (index: number) => searchResults.current?.querySelectorAll<HTMLButtonElement>('button')[index]?.focus();
  const viewport = store.layout.viewport;
  useEffect(() => {
    if (viewport) {
      const current = flow.getViewport();
      if (current.x !== viewport.x || current.y !== viewport.y || current.zoom !== viewport.zoom) void flow.setViewport(viewport, { duration: 0 });
    }
  }, [flow, viewport?.x, viewport?.y, viewport?.zoom]);
  useEffect(() => {
    if (!canvas.current) return;
    let lastSize: { width: number; height: number } | undefined;
    const observer = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      if (width > 0 && height > 0 && ((lastSize && (lastSize.width !== width || lastSize.height !== height)) || (!lastSize && graphWasObserved.current))) {
        pendingResizeFit.current = true;
        requestFit(value => value + 1);
      }
      graphWasObserved.current = true;
      lastSize = { width, height };
    });
    observer.observe(canvas.current);
    return () => observer.disconnect();
  }, [flow, mode]);
  useEffect(() => {
    if (!pendingResizeFit.current || !nodesInitialized || !flow.viewportInitialized || mode === 'yaml') return;
    const frame = requestAnimationFrame(() => {
      void flow.fitView({ duration: 0, maxZoom: 1, padding: 0.2 }).then(fitted => { if (fitted) pendingResizeFit.current = false; });
    });
    return () => cancelAnimationFrame(frame);
  }, [fitRequest, nodesInitialized, flow.viewportInitialized, flow, mode]);
  const nodes: JobNode[] = Object.entries(w?.jobs ?? {}).map(([jobID, job]) => ({
    ...(Object.hasOwn(geometry, jobID) ? geometry[jobID] : {}),
    id: jobID, type: 'job', position: (Object.hasOwn(store.layout.positions, jobID) ? store.layout.positions[jobID] : defaults[jobID]), selected: selection.job === jobID,
    // Keep matching dependency context on the canvas; use dimming instead of hiding jobs.
    className: term && !matchingJobs.has(jobID) ? 'aw-dimmed' : '',
    data: { job, id: jobID, collapsed: (Object.hasOwn(store.layout.collapsed, jobID) && !!store.layout.collapsed[jobID]), readOnly: disabled, selectedStep: selection.job === jobID ? selection.step : undefined,
      select: step => choose({ job: jobID, step }), toggle: () => store.setLayout({ ...store.layout, collapsed: { ...store.layout.collapsed, [jobID]: !(Object.hasOwn(store.layout.collapsed, jobID) && store.layout.collapsed[jobID]) } }) },
  }));
  const edges: Edge[] = Object.entries(w?.jobs ?? {}).flatMap(([jobID, job]) => dependencies(job).filter(dep => Object.hasOwn(w!.jobs, dep)).map(dep => ({ id: `${dep}->${jobID}`, source: dep, target: jobID, markerEnd: { type: MarkerType.ArrowClosed } })));
  const templates = [{ label: 'Agent', step: { uses: `agent/${options.agents?.[0]?.id ?? 'claude'}`, with: { prompt: '' } } }, ...defaultTemplates, ...(options.agents ?? []).map(agent => ({ label: agent.label ?? agent.id, step: { uses: `agent/${agent.id}`, with: { prompt: '' } } })), ...(options.actions ?? []).map(action => ({ label: action.label, step: { uses: action.uses, with: {} } }))];
  const beginDeletion = (next: PendingDeletion) => {
    returnFocus.current = document.activeElement as HTMLElement | null;
    setDeletion(next);
  };
  const requestDelete = (job: string, step?: number) => beginDeletion({ job, step, revision: store.getSnapshot().revision, impact: store.deletionImpact(job, step).map(path => path.join('.')) });
  const requestEntryDelete = (path: Path) => beginDeletion({ path, revision: store.getSnapshot().revision, impact: store.entryDeletionImpact(path).map(item => item.join('.')) });
  useEffect(() => {
    if (deletion && deletion.revision !== snapshot.revision) {
      setDeletion(undefined);
      setError('The workflow changed. Review the deletion again.');
    }
  }, [deletion, snapshot.revision]);
  useEffect(() => {
    if (!deletion) return;
    return () => queueMicrotask(() => {
      const target = returnFocus.current?.isConnected ? returnFocus.current : root.current?.querySelector<HTMLElement>('.aw-inspector h2, .aw-toolbar button');
      target?.focus();
    });
  }, [!!deletion]);
  useEffect(() => {
    if (!root.current) return;
    const observer = new ResizeObserver(([entry]) => setNarrow(entry.contentRect.width <= 760));
    observer.observe(root.current);
    return () => observer.disconnect();
  }, []);
  useEffect(() => {
    const element = root.current;
    if (!element) return;
    for (const selector of ['.aw-toolbar', '.aw-mobile-nav', '.aw-workspace', '.aw-diagnostics']) {
      const section = element.querySelector<HTMLElement>(selector);
      if (section) section.inert = !!deletion;
    }
    for (const [selector, name] of [['.aw-main', 'canvas'], ['.aw-palette', 'palette'], ['.aw-inspector', 'inspector']]) {
      const section = element.querySelector<HTMLElement>(selector);
      if (section) section.inert = narrow && panel !== name;
    }
    if (narrow && panel === 'inspector' && !deletion) element.querySelector<HTMLElement>('.aw-inspector h2')?.focus();
  }, [narrow, panel, !!deletion]);
  const revealSource = (diagnostic: Diagnostic) => {
    const source = store.getSnapshot().source;
    const range = diagnostic.location
      ? { from: diagnostic.location.offset, to: diagnostic.source === 'parser' ? Math.min(source.length, diagnostic.location.endOffset) : diagnostic.location.endOffset }
      : diagnostic.path.length ? store.getSourceRange(diagnostic.path) : undefined;
    if (!range || !Number.isInteger(range.from) || !Number.isInteger(range.to) || range.from < 0 || range.to < range.from || range.to > source.length) {
      setNavigationNotice('No source location is available for this diagnostic. Review its message and document path.');
      return false;
    }
    modeChosen.current = true;
    setMode('yaml'); setPanel('canvas'); setSourceTarget({ ...range, revision: store.getSnapshot().revision });
    return true;
  };
  const activateDiagnostic = (diagnostic: Diagnostic) => {
    const pending = drafts.pending();
    if (pending) {
      setNavigationNotice('Complete the pending inspector edit or press Escape to discard it, then activate the diagnostic again.');
      setPanel('inspector'); pending.reveal(); return;
    }
    setNavigationNotice('');
    if (!diagnostic.path.length || mode === 'yaml' || snapshot.stale || diagnostic.source === 'parser') { revealSource(diagnostic); return; }
    const path = diagnostic.code === 'expression-dependency' ? [...diagnostic.path.slice(0, 2), 'needs'] : diagnostic.path;
    const job = path[0] === 'jobs' && typeof path[1] === 'string' && w && Object.hasOwn(w.jobs, path[1]) ? path[1] : undefined;
    if (path[0] === 'jobs' && path.length > 1 && !job) { revealSource(diagnostic); return; }
    const step = job && path[2] === 'steps' && typeof path[3] === 'number' ? path[3] : undefined;
    if (step !== undefined && !w?.jobs[job!].steps?.[step]) { revealSource(diagnostic); return; }
    // Resolve the new inspector before revealing it or moving the YAML selection.
    select({ job, step });
    setDiagnosticTarget({ diagnostic: { ...diagnostic, path }, previous: selection });
  };
  useEffect(() => {
    if (!diagnosticTarget) return;
    const { diagnostic, previous } = diagnosticTarget;
    const path = diagnostic.path;
    const candidates = Array.from(root.current?.querySelectorAll<HTMLElement>('.aw-inspector [data-aw-path]') ?? []).filter(element => {
      const fieldPath: Path = JSON.parse(element.dataset.awPath!);
      if (path.length === 2 && path[0] === 'jobs' && diagnostic.code !== 'identifier' && element.dataset.awIdentifier === 'true') return false;
      return fieldPath.length <= path.length && fieldPath.every((key, i) => key === path[i]) && (fieldPath.length === path.length || element.dataset.awSubtree === 'true');
    }).sort((a, b) => {
      const depth = JSON.parse(b.dataset.awPath!).length - JSON.parse(a.dataset.awPath!).length;
      const preferred = diagnostic.code === 'identifier' ? 'true' : 'false';
      return depth || Number(b.dataset.awIdentifier === preferred) - Number(a.dataset.awIdentifier === preferred);
    });
    const field = candidates[0];
    if (!field) { select(previous); revealSource(diagnostic); }
    else {
      choose(selection);
      if (selection.job && selection.step !== undefined && store.layout.collapsed[selection.job] === true) store.setLayout({ ...store.layout, collapsed: { ...store.layout.collapsed, [selection.job]: false } });
      for (let parent: HTMLElement | null = field; parent && parent !== root.current; parent = parent.parentElement) if (parent instanceof HTMLDetailsElement) parent.open = true;
      const control = field.querySelector<HTMLElement>('input:not(:disabled),textarea:not(:disabled),select:not(:disabled)') ?? field;
      if (!control.hasAttribute('tabindex') && control === field) control.tabIndex = -1;
      requestAnimationFrame(() => { control.focus(); field.scrollIntoView({ block: 'nearest' }); });
    }
    setDiagnosticTarget(undefined);
  }, [diagnosticTarget]);
  const remove = () => { if (!deletion) return; attempt(() => {
    if ('path' in deletion) store.deleteEntry(deletion.path, true);
    else if (deletion.step === undefined) { store.deleteJob(deletion.job, true); select({}); }
    else { store.deleteStep(deletion.job, deletion.step, true); select({ job: deletion.job }); }
    setDeletion(undefined);
  }); };
  const download = () => {
    const url = URL.createObjectURL(new Blob([snapshot.source], { type: 'application/yaml' }));
    const link = document.createElement('a'); link.href = url; link.download = options.exportFilename ?? 'workflow.yml'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return <section ref={root} className="aw-editor" data-theme={options.theme ?? 'system'} data-panel={panel} style={options.tokens as CSSProperties} aria-label={store.readOnly ? 'Workflow viewer' : 'Workflow editor'} onKeyDown={e => {
    if (e.key === 'Enter' && (e.target as HTMLElement).tagName === 'INPUT') e.preventDefault();
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      save();
    }
    // Text controls keep native local editing; CodeMirror uses the shared history directly.
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'z' && !(e.target as HTMLElement).closest('input,textarea,.cm-editor')) { e.preventDefault(); e.shiftKey ? store.redo() : store.undo(); }
  }}>
    <header className="aw-toolbar"><div className="aw-brand"><span className="aw-mark">⌘</span><strong>{w?.name || 'Workflow'}</strong><span className="aw-hint">{store.readOnly ? 'Viewer' : 'Editor'}</span></div>
      <div className="aw-mode" role="group" aria-label="Editor mode">{(['visual', 'yaml', 'split'] as const).map(value => <button type="button" key={value} aria-pressed={mode === value} onClick={() => { modeChosen.current = true; setMode(value); }}>{value === 'yaml' ? 'YAML' : value[0].toUpperCase() + value.slice(1)}</button>)}</div>
      <div className="aw-button-row">{!store.readOnly && <><button type="button" disabled={!snapshot.canUndo} onClick={() => store.undo()}>Undo</button><button type="button" disabled={!snapshot.canRedo} onClick={() => store.redo()}>Redo</button></>}<button type="button" onClick={download}>Export YAML</button>{!store.readOnly && options.onSave && <><span className="aw-save-status" role="status" aria-label="Save status" data-failed={!!snapshot.saveError}>{snapshot.saving ? 'Saving…' : snapshot.saveError ? 'Save failed' : snapshot.dirty ? 'Unsaved changes' : 'Saved'}</span><button type="button" disabled={snapshot.saving} onMouseDown={event => event.preventDefault()} onClick={save}>{snapshot.saving ? 'Saving…' : 'Save'}</button></>}</div>
    </header>
    <nav className="aw-mobile-nav" aria-label="Panels">{(['palette', 'canvas', 'inspector'] as const).map(p => <button type="button" key={p} aria-pressed={panel === p} onClick={() => setPanel(p)}>{p}</button>)}</nav>
    {snapshot.stale && <div className="aw-stale" role="status">Stale preview — fix the YAML structure to resume visual editing.</div>}
    {(error || snapshot.saveError) && <div className="aw-error" role="alert">{error || snapshot.saveError}<button type="button" onClick={() => { setError(''); store.dismissSaveError(); }}>Dismiss</button></div>}
    {(!w || !snapshot.source.trim()) && <div className="aw-empty" role="status"><strong>{snapshot.source.trim() ? 'This YAML does not have a usable workflow yet.' : 'Start a workflow'}</strong><p>Write YAML, or create a workflow with its first job.</p>{!store.readOnly && <button type="button" onClick={() => { store.setSource('name: Untitled workflow\non:\n  manual: {}\njobs:\n  job:\n    steps:\n      - id: step_1\n        argv: [echo, Hello]\n'); choose({ job: 'job' }); }}>Create workflow</button>}{snapshot.source.trim() && <small>The current source can be recovered with Undo.</small>}</div>}
    <div className="aw-workspace">
      <aside className="aw-palette" aria-label="Palette"><h2>{store.readOnly ? 'Navigate' : 'Add to workflow'}</h2><label className="aw-field"><span>Search palette</span><input aria-label="Search palette" placeholder="Filter palette entries…" aria-describedby={`${id}-palette-help`} value={paletteQuery} onChange={e => setPaletteQuery(e.target.value)} /></label>
        <p id={`${id}-palette-help`} className="aw-search-help">Filters palette entries and job navigation.</p>
        {!store.readOnly && <><button type="button" disabled={disabled} onClick={() => attempt(() => choose({ job: store.addJob() }))}>Add job</button><h3>Steps</h3>{!selection.job && <p className="aw-hint">Select a job to add steps.</p>}{templates.filter(t => t.label.toLowerCase().includes(paletteQuery.toLowerCase())).map((template, i) => <button type="button" key={i} disabled={disabled || !selection.job || !!w?.jobs[selection.job]?.uses} onClick={() => attempt(() => choose({ job: selection.job, step: store.addStep(selection.job!, template.step) }))}>{template.label}</button>)}<h3>Reusable workflows</h3>{options.workflows?.filter(item => `${item.label} ${item.id}`.toLowerCase().includes(paletteQuery.toLowerCase())).map(item => <button type="button" key={item.id} disabled={disabled} onClick={() => attempt(() => choose({ job: store.addJob(item.id) }))}>{item.label ?? item.id}</button>)}</>}
        <h3>Jobs</h3>{Object.entries(w?.jobs ?? {}).filter(([key, job]) => `${key} ${job.name}`.toLowerCase().includes(paletteQuery.toLowerCase())).map(([key, job]) => <button type="button" key={key} onClick={() => { choose({ job: key }); void flow.fitView({ nodes: [{ id: key }], duration: 0, maxZoom: 1, padding: 0.2 }); }}>{job.name || key}</button>)}
        <button type="button" onClick={() => choose({})}>Workflow properties</button>
      </aside>
      <div className={`aw-main aw-mode-${mode}`}>
        {mode !== 'yaml' && <div className="aw-graph"><div className="aw-canvas-toolbar"><input ref={searchInput} aria-label="Search workflow" aria-describedby={`${id}-search-help`} aria-controls={term ? `${id}-search-results` : undefined} placeholder="Search jobs and steps…" value={query} onChange={e => setQuery(e.target.value)} onKeyDown={e => {
            if (e.key === 'Escape') { e.preventDefault(); clearSearch(); }
            if (results.length && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) { e.preventDefault(); focusResult(e.key === 'ArrowDown' ? 0 : results.length - 1); }
          }} /><button type="button" onClick={() => void flow.fitView({ duration: 0, maxZoom: 1, padding: 0.2 })}>Fit view</button><button type="button" onClick={() => { store.setLayout({ ...store.layout, positions: automaticLayout(w?.jobs ?? {}, store.layout.collapsed) }); requestAnimationFrame(() => void flow.fitView({ duration: 0, maxZoom: 1, padding: 0.2 })); }}>Auto layout</button></div>
          <div className="aw-workflow-search">
            <p id={`${id}-search-help`} className="aw-search-help">Searches job details and step content.</p>
            <div role={term ? 'status' : undefined} aria-live="polite" aria-atomic="true" className="aw-search-count">{term ? results.length ? `${results.length} ${results.length === 1 ? 'result' : 'results'}` : 'No results' : ''}</div>
            {term && <ul ref={searchResults} tabIndex={-1} id={`${id}-search-results`} className="aw-search-results" aria-label="Workflow search results" onKeyDown={e => {
              if (e.key === 'Escape') { e.preventDefault(); clearSearch(); }
              if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
                const buttons = Array.from(e.currentTarget.querySelectorAll('button'));
                const index = buttons.indexOf(e.target as HTMLButtonElement);
                if (index >= 0) { e.preventDefault(); focusResult((index + (e.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length); }
              }
            }}>{results.map(result => <li key={`${result.job}:${result.step ?? 'job'}`}><button type="button" onClick={() => activateResult(result)}><strong>{result.label}</strong><small>{result.detail}</small></button></li>)}</ul>}
          </div>
          <div className="aw-flow" ref={canvas}><ReactFlow<JobNode> nodes={nodes} edges={edges} nodeTypes={nodeTypes} nodesDraggable onNodesChange={changes => {
            if (changes.some(change => change.type === 'dimensions' || change.type === 'position')) setGeometry(previous => {
              const next = { ...previous };
              for (const change of changes) {
                if (change.type !== 'dimensions' && change.type !== 'position') continue;
                const existing = Object.hasOwn(next, change.id) ? next[change.id] : {};
                const value = change.type === 'dimensions'
                  ? { ...existing, measured: change.dimensions ?? existing.measured }
                  : { ...existing, dragging: change.dragging };
                Object.defineProperty(next, change.id, { value, enumerable: true, writable: true, configurable: true });
              }
              return next;
            });
            const selected = changes.find(change => change.type === 'select' && change.selected);
            if (selected?.type === 'select') select(current => current.job === selected.id ? current : { job: selected.id });
            const positions = Object.fromEntries(changes.flatMap(change => change.type === 'position' && change.position ? [[change.id, change.position]] : []));
            if (Object.keys(positions).length) store.setLayout({ ...store.layout, positions: { ...store.layout.positions, ...positions } });
          }} nodesConnectable={!disabled} edgesReconnectable={false} deleteKeyCode={null} fitViewOptions={{ maxZoom: 1, padding: 0.2 }} fitView={!store.layout.viewport} defaultViewport={store.layout.viewport} minZoom={0.15} maxZoom={2} onNodeClick={(event, node) => { if (!(event.target as HTMLElement).closest('button')) choose({ job: node.id }); }} onPaneClick={() => select({})} onConnect={connection => attempt(() => store.connect(connection.source, connection.target))} onMoveEnd={(_, viewport) => store.setLayout({ ...store.layout, viewport })}><Background gap={24} /><Controls showInteractive={false} /></ReactFlow></div>
        </div>}
        {mode !== 'visual' && <div className="aw-yaml-pane"><div className="aw-pane-title">YAML source <span>{snapshot.validating ? 'Validating…' : `${snapshot.diagnostics.filter(d => d.severity === 'error').length} errors`}</span></div><SourceEditor store={store} jobID={(diagnosticTarget?.previous ?? selection).job} stepIndex={(diagnosticTarget?.previous ?? selection).step} navigationKey={sourceNavigation} target={sourceTarget} /></div>}
      </div>
      <aside className="aw-inspector" aria-label="Inspector"><InspectorDraftContext.Provider value={drafts}><Inspector key={`${selection.job ?? "workflow"}:${selection.step ?? "job"}`} store={store} selection={selection} select={choose} attempt={attempt} requestDelete={requestDelete} requestEntryDelete={requestEntryDelete} suggestionsID={`${id}-suggestions`} /></InspectorDraftContext.Provider></aside>
    </div>
    <footer className="aw-diagnostics"><p role="status">{navigationNotice}</p><details open={snapshot.diagnostics.some(d => d.severity === 'error')}><summary>{snapshot.diagnostics.length} diagnostics{snapshot.validating ? ' · validating' : ''}</summary><ul>{snapshot.diagnostics.map((d, i) => <li key={i} data-severity={d.severity}><button type="button" onMouseDown={event => event.preventDefault()} onClick={event => { if (!drafts.pending()) event.currentTarget.focus(); activateDiagnostic(d); }}><strong>{d.severity}</strong> {d.location ? `${d.location.line}:${d.location.column} ` : ''}{d.path.map(diagnosticLabel).join('.')} — {d.message} <small>({d.source})</small></button>{d.code === 'expression-reference' && <p className="aw-diagnostic-help">Review this reference in the consuming field. Restore its producer or explicitly replace or remove the reference.</p>}</li>)}</ul></details></footer>
    {deletion && <div className="aw-modal-backdrop"><div className="aw-dialog" role="alertdialog" aria-modal="true" aria-labelledby={`${id}-delete-title`} aria-describedby={`${id}-delete-impact`} onKeyDown={e => {
      if (e.key === 'Escape') setDeletion(undefined);
      if (e.key === 'Tab') {
        const buttons = Array.from(e.currentTarget.querySelectorAll<HTMLButtonElement>('button:not(:disabled)'));
        if (!buttons.length) return;
        e.preventDefault();
        const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
        const next = current < 0 ? (e.shiftKey ? buttons.length - 1 : 0) : (current + (e.shiftKey ? -1 : 1) + buttons.length) % buttons.length;
        buttons[next].focus();
      }
    }}><h2 id={`${id}-delete-title`}>Delete {'path' in deletion ? String(deletion.path.at(-1)) : deletion.step === undefined ? 'job' : 'step'}?</h2><div id={`${id}-delete-impact`}><p>{'path' in deletion ? 'Expression references remain and receive diagnostics.' : 'Structural dependencies will be removed. Expression references remain and receive diagnostics.'}</p>{deletion.impact.length ? <><p>Affected paths:</p><ul>{deletion.impact.map((path, i) => <li key={i}><code>{path}</code></li>)}</ul></> : <p>No references found.</p>}</div><div className="aw-button-row"><button type="button" autoFocus onClick={() => setDeletion(undefined)}>Cancel</button><button type="button" onClick={remove}>Confirm delete</button></div></div></div>}
  </section>;
}
export function WorkflowSurface({ store }: { store: WorkflowDocument }) { return <ReactFlowProvider><EditorSurface store={store} /></ReactFlowProvider>; }
function ReactWorkflow({ options, readOnly }: { options: WorkflowOptions; readOnly: boolean }) {
  const latest = useRef(options);
  latest.current = options;
  // Inline callbacks are normal in React. Stable delegates avoid restarting
  // validation (and notifying a controlled parent) merely because it rendered.
  const delegates = useMemo(() => ({
    onChange: (snapshot: Parameters<NonNullable<WorkflowOptions['onChange']>>[0]) => latest.current.onChange?.(snapshot),
    onLayoutChange: (layout: LayoutState) => latest.current.onLayoutChange?.(layout),
    onSave: async (snapshot: Parameters<NonNullable<WorkflowOptions['onSave']>>[0]) => { await latest.current.onSave?.(snapshot); },
    validate: async (snapshot: Parameters<NonNullable<WorkflowOptions['validate']>>[0], signal: AbortSignal) => await latest.current.validate?.(snapshot, signal) ?? [],
  }), []);
  const current = { ...options, ...delegates, onSave: options.onSave ? delegates.onSave : undefined, validate: options.validate ? delegates.validate : undefined };
  const [store] = useState(() => new WorkflowDocument(current, readOnly));
  const hostSource = useRef(options.source);
  useEffect(() => { store.start(); return () => store.destroy(); }, [store]);
  useEffect(() => {
    // Presentation/catalog updates must not reapply an unchanged initial source
    // over local edits in an uncontrolled host.
    const { source, ...next } = current;
    if (source !== hostSource.current) {
      hostSource.current = source;
      store.update({ ...next, source });
    } else store.update(next);
  }, [store, options.source, options.theme, options.tokens, options.agents, options.workflows, options.actions, options.layout, options.validationKey, options.exportFilename, !!options.onSave, !!options.validate]);
  return <WorkflowSurface store={store} />;
}
export function WorkflowEditor(options: WorkflowOptions) { return <ReactWorkflow options={options} readOnly={false} />; }
export function WorkflowViewer(options: WorkflowOptions) { return <ReactWorkflow options={options} readOnly />; }
