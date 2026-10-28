import { mountWorkflowEditor } from './src';
import type { WorkflowHandle, LayoutState, WorkflowOptions } from './src';
import classify from '../workflows/classify.yml?raw';
import parallel from '../workflows/parallel-review.yml?raw';
import develop from '../workflows/develop.yml?raw';
import iteration from '../workflows/develop-iteration.yml?raw';
const examples: Record<string, string> = { classify, 'parallel-review': parallel, develop, 'develop-iteration': iteration };
const selector = document.querySelector<HTMLSelectElement>('#example')!;
const theme = document.querySelector<HTMLSelectElement>('#theme')!;
const app = document.querySelector<HTMLElement>('#app')!;
const sessions = new Map<string, { element: HTMLElement; instance: WorkflowHandle }>();
for (const key of Object.keys(examples)) selector.add(new Option(key, key));
const currentTheme = () => theme.value as WorkflowOptions['theme'];
function load() {
  const key = selector.value;
  let session = sessions.get(key);
  if (!session) {
    let layout: LayoutState | undefined;
    try { layout = JSON.parse(localStorage.getItem(`workflow-layout:${key}`) ?? 'null') ?? undefined; } catch { /* Ignore stale playground state. */ }
    const element = document.createElement('div');
    element.style.height = '100%';
    session = { element, instance: undefined! };
    sessions.set(key, session);
    // Mount only after attaching so the canvas can measure its container.
    app.replaceChildren(element);
    session.instance = mountWorkflowEditor(element, {
      source: examples[key], layout, theme: currentTheme(), exportFilename: `${key}.yml`,
      agents: ['claude', 'codex', 'planner', 'builder', 'reviewer_claude', 'reviewer_codex'].map(id => ({ id })),
      workflows: Object.keys(examples).map(id => ({ id: `workflows/${id}.yml` })),
      onLayoutChange: next => {
        localStorage.setItem(`workflow-layout:${key}`, JSON.stringify(next));
      },
    });
  } else {
    app.replaceChildren(session.element);
    session.instance.update({ theme: currentTheme() });
  }
}
selector.addEventListener('change', load);
theme.addEventListener('change', () => sessions.get(selector.value)!.instance.update({ theme: currentTheme() }));
const reset = document.querySelector('#reset-example')!;
// Keep focused field text unapplied if the reset confirmation is cancelled.
reset.addEventListener('pointerdown', event => event.preventDefault());
reset.addEventListener('click', () => {
  const key = selector.value;
  if (!window.confirm(`Reset ${key}? This discards its source edits, undo history, and unapplied fields.`)) return;
  const session = sessions.get(key)!;
  session.instance.destroy();
  sessions.delete(key);
  load();
});
load();
