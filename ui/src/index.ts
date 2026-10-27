import { createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { flushSync } from 'react-dom';
import { WorkflowSurface } from './editor';
import { WorkflowDocument } from './document';
import type { WorkflowHandle, WorkflowOptions } from './types';
export { WorkflowEditor, WorkflowViewer, automaticLayout } from './editor';
export { WorkflowDocument } from './document';
export { parseWorkflow, effectiveStepID } from './validation';
export { expressionSuggestions } from './source';
export type * from './types';
function mount(element: HTMLElement, options: WorkflowOptions, readOnly: boolean): WorkflowHandle {
  const store = new WorkflowDocument(options, readOnly);
  const root = createRoot(element);
  let destroyed = false;
  flushSync(() => root.render(createElement(WorkflowSurface, { store })));
  store.start();
  return {
    update: next => { if (destroyed) throw new Error('Workflow instance was destroyed'); store.update(next); },
    getSnapshot: () => structuredClone(store.getSnapshot()),
    resolveSourceReplacement: choice => { if (destroyed) throw new Error('Workflow instance was destroyed'); store.resolveSourceReplacement(choice); },
    destroy: () => { if (!destroyed) { destroyed = true; root.unmount(); store.destroy(); } },
  };
}
export const mountWorkflowEditor = (element: HTMLElement, options: WorkflowOptions) => mount(element, options, false);
export const mountWorkflowViewer = (element: HTMLElement, options: WorkflowOptions) => mount(element, options, true);
