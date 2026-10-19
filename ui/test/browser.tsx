import { StrictMode, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { mountWorkflowEditor, mountWorkflowViewer, WorkflowEditor, WorkflowViewer } from '../src';
import type { Snapshot, WorkflowHandle, WorkflowOptions } from '../src';
const source = `# Fixture\nname: Browser workflow\non: {manual: {}}\njobs:\n  build:\n    steps:\n      - id: greet\n        uses: agent/claude\n        with: {prompt: Hello}\n  test:\n    needs: build\n    steps:\n      - argv: [echo, ok]\n`;
const mode = new URLSearchParams(location.search);
const globals = window as unknown as { first: WorkflowHandle; second: WorkflowHandle; latest: unknown; cleanupCount: number; reactUnmount: () => void };
globals.cleanupCount = 0;
const options: WorkflowOptions = {
  source, onChange: snapshot => { globals.latest = snapshot; },
  actions: [{ uses: 'agent/claude', label: 'Custom Claude', fields: [{ key: 'model', label: 'Custom model' }], renderInspector: (element, context) => {
    const button = document.createElement('button'); button.type = 'button'; button.textContent = 'Custom prompt'; button.disabled = context.readOnly;
    button.onclick = () => context.setField(['with', 'prompt'], 'Custom value'); element.append(button);
    (window as any).customSetField = context.setField;
    return () => { button.onclick = null; globals.cleanupCount++; };
  } }],
};
function ControlledEditor() {
  const [snapshot, setSnapshot] = useState<Snapshot>();
  const [policy, setPolicy] = useState(0);
  (window as any).changePolicy = () => setPolicy(value => value + 1);
  return <WorkflowEditor source={snapshot?.source ?? source} agents={[{ id: 'claude' }]} validationKey={policy} validate={async () => policy ? [{ code: 'policy', message: `Policy ${policy}`, path: [], severity: 'warning', source: 'host' }] : []} onChange={next => {
    globals.latest = next;
    setSnapshot(next);
  }} />;
}
function UncontrolledEditor() {
  const [theme, setTheme] = useState<'light' | 'dark'>('light');
  const [hostSource, setHostSource] = useState(source);
  (window as any).rerenderHost = () => setTheme(value => value === 'light' ? 'dark' : 'light');
  (window as any).replaceHostSource = (value: string) => setHostSource(value);
  return <WorkflowEditor {...options} source={hostSource} theme={theme} agents={[{ id: 'claude' }]} />;
}
if (mode.has('react') || mode.has('controlled') || mode.has('uncontrolled')) {
  const root = createRoot(document.querySelector('#a')!);
  const Component = mode.has('viewer') ? WorkflowViewer : WorkflowEditor;
  root.render(<StrictMode>{mode.has('controlled') ? <ControlledEditor /> : mode.has('uncontrolled') ? <UncontrolledEditor /> : <Component {...options} />}</StrictMode>);
  globals.reactUnmount = () => root.unmount();
} else {
  globals.first = (mode.has('viewer') ? mountWorkflowViewer : mountWorkflowEditor)(document.querySelector('#a')!, options);
  globals.second = mountWorkflowEditor(document.querySelector('#b')!, { source: 'jobs: {independent: {steps: [{argv: [echo]}]}}', theme: 'dark' });
}
