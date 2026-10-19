import { useEffect, useRef } from 'react';
import { EditorState } from '@codemirror/state';
import { EditorView, keymap, lineNumbers, highlightActiveLine, drawSelection } from '@codemirror/view';
import { defaultKeymap } from '@codemirror/commands';
import { autocompletion, type CompletionContext } from '@codemirror/autocomplete';
import { syntaxHighlighting } from '@codemirror/language';
import { classHighlighter } from '@lezer/highlight';
import { yaml } from '@codemirror/lang-yaml';
import { search, searchKeymap } from '@codemirror/search';
import type { WorkflowDocument } from './document';
import type { Workflow } from './types';
import { hasExpressionReferences, isExpressionPath } from './expressions';
import { dependencies, effectiveStepID } from './validation';
export function expressionSuggestions(workflow?: Workflow, jobID?: string, stepIndex?: number): string[] {
  if (!workflow) return [];
  const callable = workflow.on?.workflow_call as { inputs?: Record<string, unknown> } | undefined;
  const result = Object.keys(callable?.inputs ?? {}).map(id => `inputs.${id}`);
  const job = jobID ? workflow.jobs[jobID] : undefined;
  for (const dep of job ? dependencies(job) : Object.keys(workflow.jobs)) for (const output of Object.keys(workflow.jobs[dep]?.outputs ?? {})) result.push(`${job ? 'needs' : 'jobs'}.${dep}.outputs.${output}`);
  job?.steps?.slice(0, stepIndex ?? job.steps.length).forEach((s, i) => ['text', 'structured', 'exit_code', 'stdout', 'stderr', 'path', 'commit'].forEach(output => result.push(`steps.${effectiveStepID(s, i)}.outputs.${output}`)));
  return result;
}
export function SourceEditor({ store, jobID, stepIndex, navigationKey }: { store: WorkflowDocument; jobID?: string; stepIndex?: number; navigationKey?: number }) {
  const element = useRef<HTMLDivElement>(null);
  const selection = useRef({ jobID, stepIndex }); selection.current = { jobID, stepIndex };
  const revealSelection = useRef<() => void>(() => {});
  useEffect(() => {
    const complete = (context: CompletionContext) => {
      const word = context.matchBefore(/[\w.-]*/);
      if (!word || (!context.explicit && word.from === word.to)) return null;
      const path = store.getPathAtOffset(context.pos);
      if (path.length && !hasExpressionReferences(path)) return null;
      const jobID = path[0] === 'jobs' ? String(path[1]) : path.length ? undefined : selection.current.jobID;
      const stepIndex = path[2] === 'steps' && typeof path[3] === 'number' ? path[3] : path.length ? undefined : selection.current.stepIndex;
      const linePrefix = context.state.sliceDoc(context.state.doc.lineAt(context.pos).from, context.pos);
      const insideExpression = linePrefix.lastIndexOf('${{') > linePrefix.lastIndexOf('}}');
      if (!context.explicit && !insideExpression && !isExpressionPath(path)) return null;
      const suggestions = expressionSuggestions(store.getSnapshot().workflow ?? store.getSnapshot().previewWorkflow, jobID, stepIndex);
      return { from: word.from, options: suggestions.map(label => ({ label, type: 'variable', apply: insideExpression || isExpressionPath(path) ? label : '${{ ' + label + ' }}' })) };
    };
    let syncing = false;
    let editing = false;
    let syncQueued = false;
    let disposed = false;
    const view = new EditorView({ parent: element.current!, state: EditorState.create({
      doc: store.getSnapshot().source,
      extensions: [lineNumbers(), highlightActiveLine(), drawSelection(), yaml(), syntaxHighlighting(classHighlighter), search(), autocompletion({ override: [complete] }),
        EditorState.readOnly.of(store.readOnly), EditorView.editable.of(!store.readOnly), EditorView.contentAttributes.of({ 'aria-label': 'Workflow YAML', role: 'textbox', 'aria-multiline': 'true' }),
        keymap.of([{ key: 'Mod-z', run: () => { store.undo(); return true; } }, { key: 'Mod-Shift-z', run: () => { store.redo(); return true; } }, { key: 'Mod-y', run: () => { store.redo(); return true; } }, ...searchKeymap, ...defaultKeymap]),
        EditorView.lineWrapping,
        EditorView.updateListener.of(update => { if (update.docChanged && !syncing) { editing = true; try { store.setSource(update.state.doc.toString(), 'yaml'); } finally { editing = false; } } }),
      ],
    }) });
    revealSelection.current = () => {
      const { jobID, stepIndex } = selection.current;
      if (jobID === undefined) return;
      const path = stepIndex === undefined ? ['jobs', jobID] : ['jobs', jobID, 'steps', stepIndex];
      const range = store.getSourceRange(path);
      const source = store.getSnapshot().source;
      if (!range || view.state.doc.toString() !== source.replace(/\r\n?/g, '\n')) return;
      // CodeMirror counts each line break once, including imported CRLF files.
      const from = source.slice(0, range.from).replace(/\r\n?/g, '\n').length;
      const to = source.slice(0, range.to).replace(/\r\n?/g, '\n').length;
      // Reveal without taking focus away from the graph or property controls.
      view.dispatch({ selection: { anchor: from, head: to }, effects: EditorView.scrollIntoView(from, { y: 'start', yMargin: 12 }) });
    };
    const syncSource = () => {
      if (disposed) return;
      if (editing) {
        if (!syncQueued) { syncQueued = true; queueMicrotask(() => { syncQueued = false; syncSource(); }); }
        return;
      }
      const source = store.getSnapshot().source.replace(/\r\n?/g, '\n');
      const current = view.state.doc.toString();
      if (source !== current) {
        // Apply the changed span so shared undo and host updates preserve the
        // cursor and scroll position around unchanged YAML.
        let from = 0;
        while (from < current.length && from < source.length && current[from] === source[from]) from++;
        let oldEnd = current.length;
        let newEnd = source.length;
        while (oldEnd > from && newEnd > from && current[oldEnd - 1] === source[newEnd - 1]) { oldEnd--; newEnd--; }
        syncing = true;
        try { view.dispatch({ changes: { from, to: oldEnd, insert: source.slice(from, newEnd) } }); }
        finally { syncing = false; }
      }
    };
    const unsubscribe = store.subscribe(syncSource);
    return () => { disposed = true; revealSelection.current = () => {}; unsubscribe(); view.destroy(); };
  }, [store]);
  useEffect(() => { revealSelection.current(); }, [store, jobID, stepIndex, navigationKey]);
  return <div className="aw-source" ref={element} />;
}
