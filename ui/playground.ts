import { mountWorkflowEditor } from './src';
import type { WorkflowHandle, LayoutState } from './src';
import classify from '../workflows/classify.yml?raw';
import parallel from '../workflows/parallel-review.yml?raw';
import develop from '../workflows/develop.yml?raw';
import iteration from '../workflows/develop-iteration.yml?raw';
const examples: Record<string, string> = { classify, 'parallel-review': parallel, develop, 'develop-iteration': iteration };
const selector = document.querySelector<HTMLSelectElement>('#example')!;
for (const key of Object.keys(examples)) selector.add(new Option(key, key));
let instance: WorkflowHandle;
function load() {
  instance?.destroy();
  let layout: LayoutState | undefined;
  try { layout = JSON.parse(localStorage.getItem(`workflow-layout:${selector.value}`) ?? 'null') ?? undefined; } catch { /* Ignore stale playground state. */ }
  instance = mountWorkflowEditor(document.querySelector('#app')!, {
    source: examples[selector.value], layout,
    agents: ['claude', 'codex', 'planner', 'builder', 'reviewer_claude', 'reviewer_codex'].map(id => ({ id })),
    workflows: Object.keys(examples).map(id => ({ id: `workflows/${id}.yml` })),
    onLayoutChange: next => localStorage.setItem(`workflow-layout:${selector.value}`, JSON.stringify(next)),
    onSave: snapshot => { const link = document.createElement('a'); const url = URL.createObjectURL(new Blob([snapshot.source])); link.href = url; link.download = `${selector.value}.yml`; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000); },
  });
}
selector.addEventListener('change', load);
document.querySelector('#theme')!.addEventListener('change', e => instance.update({ theme: (e.target as HTMLSelectElement).value as 'system' | 'light' | 'dark' }));
load();
