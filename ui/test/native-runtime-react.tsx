import { StrictMode, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { WorkflowEditor, WorkflowViewer, type Snapshot } from '../src';

export interface NativeRuntimeFixtureGlobals extends Window { nativeRuntimeSnapshot?: Snapshot }

const source = 'jobs: {review: {steps: [{uses: agent/run, with: {agent: reviewer.yaml, prompt: Keep}}]}}\n';
const agents = [{ id: 'claude' }];
const globals = window as NativeRuntimeFixtureGlobals;
const capture = (snapshot: Snapshot) => { globals.nativeRuntimeSnapshot = snapshot; };
const Component = new URLSearchParams(location.search).has('viewer') ? WorkflowViewer : WorkflowEditor;

function RuntimeHost() {
  const [runtime, setRuntime] = useState<string>();
  return <>
    <label>Host native runtime <select value={runtime ?? ''} onChange={event => setRuntime(event.target.value || undefined)}>
      <option value="">Default Codex</option><option value="claude">Claude</option><option value="missing-runtime">Unavailable</option>
    </select></label>
    <div className="runtime-editor"><Component source={source} agents={agents} agentRuntime={runtime} onChange={capture} /></div>
  </>;
}

createRoot(document.querySelector('#root')!).render(<StrictMode><RuntimeHost /></StrictMode>);
