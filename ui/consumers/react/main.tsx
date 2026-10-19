import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { WorkflowEditor } from '@agent-workflow/ui';
import '@agent-workflow/ui/style.css';
function App() {
  const [source, setSource] = useState('jobs: {packed: {steps: [{argv: [echo, packed]}]}}');
  return <WorkflowEditor source={source} onChange={snapshot => setSource(snapshot.source)} agents={[{ id: 'claude' }]} validate={async () => []} />;
}
createRoot(document.getElementById('root')!).render(<App />);
