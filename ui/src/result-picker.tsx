import { useEffect, useId, useRef, useState } from 'react';
import type { WorkflowDocument } from './document';
import type { JobResultPreview, Path } from './types';
import { effectiveStepID } from './validation';

export function JobResultPicker({ store, path, selection, close }: {
  store: WorkflowDocument; path: Path; selection?: { start: number; end: number }; close: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const id = useId();
  const jobs = store.getSnapshot().workflow?.jobs ?? {};
  const producers = Object.keys(jobs).filter(job => path[0] !== 'jobs' || job !== path[1]);
  const [producer, setProducer] = useState(producers[0] ?? '');
  const [source, setSource] = useState('step');
  const [step, setStep] = useState('0');
  const [output, setOutput] = useState('text');
  const [name, setName] = useState('result');
  const [preview, setPreview] = useState<JobResultPreview>();
  const [error, setError] = useState('');
  useEffect(() => { dialog.current?.showModal(); }, []);
  const change = (fn: () => void) => { fn(); setPreview(undefined); setError(''); };
  const attempt = (fn: () => void) => { try { fn(); setError(''); } catch (error) { setError(String(error)); } };
  return <dialog className="aw-result-dialog" ref={dialog} aria-labelledby={`${id}-title`} onCancel={event => { event.preventDefault(); close(); }}>
    <h2 id={`${id}-title`}>Use job result</h2>
    <label className="aw-field">Producer job<select aria-label="Producer job" value={producer} onChange={event => change(() => { setProducer(event.target.value); setStep('0'); if (source === 'output') setOutput(Object.keys(jobs[event.target.value]?.outputs ?? {})[0] ?? ''); })}>{producers.map(job => <option key={job}>{job}</option>)}</select></label>
    <label className="aw-field">Result source<select aria-label="Result source" value={source} onChange={event => change(() => { setSource(event.target.value); setOutput(event.target.value === 'output' ? Object.keys(jobs[producer]?.outputs ?? {})[0] ?? '' : 'text'); })}><option value="step">Step output</option><option value="output">Existing job output</option></select></label>
    {source === 'output'
      ? <label className="aw-field">Producer output<select aria-label="Producer output" value={output} onChange={event => change(() => setOutput(event.target.value))}><option value="" disabled>Choose an output</option>{Object.keys(jobs[producer]?.outputs ?? {}).map(key => <option key={key}>{key}</option>)}</select></label>
      : <>
        <label className="aw-field">Producer step<select aria-label="Producer step" value={step} onChange={event => change(() => setStep(event.target.value))}>{(!jobs[producer]?.uses ? jobs[producer]?.steps : [])?.map((step, index) => <option key={index} value={index}>{effectiveStepID(step, index)}</option>)}</select></label>
        <label className="aw-field">Step output<input value={output} list={`${id}-outputs`} onChange={event => change(() => setOutput(event.target.value))} /></label>
        <datalist id={`${id}-outputs`}>{['text', 'structured', 'exit_code', 'stdout', 'stderr', 'path', 'commit'].map(key => <option key={key}>{key}</option>)}</datalist>
        <label className="aw-field">Job output name<input value={name} onChange={event => change(() => setName(event.target.value))} /></label>
      </>}
    <button type="button" disabled={!producer} onClick={() => attempt(() => setPreview(store.previewJobResult({ target: path, producer, selection,
      source: source === 'output' ? { kind: 'output', output } : { kind: 'step', step: Number(step), output, name } }))) }>Preview result wiring</button>
    {preview && <div className="aw-result-preview" aria-label="Result wiring preview">
      <p>{preview.declaration ? `Declare ${producer}.outputs.${preview.output}: ${preview.declaration}` : `Reuse ${producer}.outputs.${preview.output}`}</p>
      <p>{preview.addDependency ? `Add dependency: ${String(path[1])} needs ${producer}` : 'No dependency change'}</p>
      <p>Insert: <code>{preview.expression}</code></p>
      <p>Field after insertion:</p><pre>{preview.value}</pre>
    </div>}
    {error && <p role="alert">{error}</p>}
    <div className="aw-button-row"><button type="button" disabled={!preview} onClick={() => attempt(() => { store.applyJobResult(preview!); close(); })}>Apply job result</button><button type="button" onClick={close}>Cancel</button></div>
  </dialog>;
}
