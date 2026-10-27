import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { WorkflowEditor } from '@agent-workflow/ui';
import '@agent-workflow/ui/style.css';
function App() {
  const [source, setSource] = useState('jobs: {packed: {steps: [{argv: [echo, packed]}]}}');
  return <WorkflowEditor source={source} onChange={snapshot => setSource(snapshot.source)} agents={[{ id: 'claude' }]} actions={[{ uses: 'host/buffered', label: 'Buffered host action', renderInspector(element, context) {
    const input = document.createElement('input');
    const stored = String(context.step.with?.prompt ?? '');
    input.value = stored; input.readOnly = context.readOnly; input.setAttribute('aria-label', 'Packed raw prompt');
    const discard = () => { input.value = stored; };
    input.oninput = () => context.reportDraft(input.value !== stored, discard);
    element.append(input);
    return () => { input.oninput = null; };
  } }]} validate={async () => []} />;
}
createRoot(document.getElementById('root')!).render(<App />);
