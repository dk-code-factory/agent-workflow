import { useEffect, useId, useRef, useState } from 'react';
import type { WorkflowDocument } from './document';
import type { JobResultPreview, Path } from './types';
import { blockKind, stepEntries } from './step-tree';
import { stepResultPaths } from './source';

export function JobResultPicker({ store, path, selection, close }: {
  store: WorkflowDocument; path: Path; selection?: { start: number; end: number }; close: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const id = useId();
  const jobs = store.getSnapshot().workflow?.jobs ?? {};
  const producerSteps = (producer: string) => !jobs[producer]?.uses ? stepEntries(jobs[producer]?.steps).filter(entry => !blockKind(entry.step)) : [];
  const producers = Object.keys(jobs).filter(job => path[0] !== 'jobs' || job !== path[1]);
  const [producer, setProducer] = useState(producers[0] ?? '');
  const [source, setSource] = useState('step');
  const [step, setStep] = useState('0');
  const [output, setOutput] = useState('text');
  const [resultPrefix, setResultPrefix] = useState('');
  const [name, setName] = useState('result');
  const [preview, setPreview] = useState<JobResultPreview>();
  const [error, setError] = useState('');
  useEffect(() => { dialog.current?.showModal(); }, []);
  const change = (fn: () => void) => { fn(); setPreview(undefined); setError(''); };
  const dismiss = () => { const element = dialog.current; if (!element?.open) return; element.close(); close(); };
  const attempt = (fn: () => void) => { try { fn(); setError(''); } catch (error) { setError(String(error)); } };
  return <dialog className="aw-result-dialog" ref={dialog} aria-labelledby={`${id}-title`} onCancel={event => { event.preventDefault(); dismiss(); }}>
    <h2 id={`${id}-title`}>Use job result</h2>
    <label className="aw-field">Producer job<select aria-label="Producer job" value={producer} onChange={event => change(() => { setProducer(event.target.value); setStep('0'); setResultPrefix(''); if (source === 'output') setOutput(Object.keys(jobs[event.target.value]?.outputs ?? {})[0] ?? ''); })}>{producers.map(job => <option key={job}>{job}</option>)}</select></label>
    <label className="aw-field">Result source<select aria-label="Result source" value={source} onChange={event => change(() => { setSource(event.target.value); setResultPrefix(''); setOutput(event.target.value === 'output' ? Object.keys(jobs[producer]?.outputs ?? {})[0] ?? '' : 'text'); })}><option value="step">Step output</option><option value="result">Named step result</option><option value="output">Existing job output</option></select></label>
    {source === 'output'
      ? <label className="aw-field">Producer output<select aria-label="Producer output" value={output} onChange={event => change(() => setOutput(event.target.value))}><option value="" disabled>Choose an output</option>{Object.keys(jobs[producer]?.outputs ?? {}).map(key => <option key={key}>{key}</option>)}</select></label>
      : <>
        <label className="aw-field">Producer step<select aria-label="Producer step" value={step} onChange={event => change(() => { setStep(event.target.value); setResultPrefix(''); })}>{producerSteps(producer).map((entry, index) => <option key={entry.id} value={index}>{entry.id}</option>)}</select></label>
        <label className="aw-field">{source === 'result' ? 'Result field' : 'Step output'}<input value={output} list={`${id}-outputs`} onChange={event => change(() => { setOutput(event.target.value); if (source === 'result') setResultPrefix(event.target.value); })} /></label>
        <datalist id={`${id}-outputs`}>{(source === 'result' ? stepResultPaths(producerSteps(producer)[Number(step)]?.step ?? {}, store.getSnapshot().workflow?.schemas, resultPrefix) : ['text', 'structured', 'exit_code', 'stdout', 'stderr', 'path', 'commit']).map(key => <option key={key}>{key}</option>)}</datalist>
        {source === 'result' ? <p>Use text or a structured field path. For fields named text, outputs, status, or error, use outputs.structured followed by the field path. Reusable and per-item jobs expose their declared outputs instead.</p> : <label className="aw-field">Job output name<input value={name} onChange={event => change(() => setName(event.target.value))} /></label>}
      </>}
    <button type="button" disabled={!producer} onClick={() => attempt(() => setPreview(store.previewJobResult({ target: path, producer, selection,
      source: source === 'output' ? { kind: 'output', output } : source === 'result' ? { kind: 'result', step: producerSteps(producer)[Number(step)]?.path ?? [], field: output } : { kind: 'step', step: producerSteps(producer)[Number(step)]?.path ?? [], output, name } }))) }>Preview result wiring</button>
    {preview && <div className="aw-result-preview" aria-label="Result wiring preview">
      <p>{preview.request.source.kind === 'result' ? 'Use the named step result without a job output declaration' : preview.declaration ? `Declare ${producer}.outputs.${preview.output}: ${preview.declaration}` : `Reuse ${producer}.outputs.${preview.output}`}</p>
      <p>{preview.addDependency ? `Add dependency: ${String(path[1])} needs ${producer}` : 'No dependency change'}</p>
      <p>Insert: <code>{preview.expression}</code></p>
      <p>Field after insertion:</p><pre>{preview.value}</pre>
    </div>}
    {error && <p role="alert">{error}</p>}
    <div className="aw-button-row"><button type="button" disabled={!preview} onClick={() => attempt(() => { store.applyJobResult(preview!); dismiss(); })}>Apply job result</button><button type="button" onClick={dismiss}>Cancel</button></div>
  </dialog>;
}
