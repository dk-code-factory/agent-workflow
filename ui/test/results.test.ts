import { describe, expect, it } from 'vitest';
import { WorkflowDocument } from '../src/document';
import type { JobResultRequest } from '../src/types';

const source = `# keep header
jobs:
  review:
    imported: keep
    steps:
      - uses: agent/claude
        with: {prompt: Review} # keep producer
  summarize:
    steps:
      - id: summary
        uses: agent/claude
        with:
          model: keep # keep model
          prompt: 'Summarize: '
`;
const request: JobResultRequest = { target: ['jobs', 'summarize', 'steps', 0, 'with', 'prompt'], producer: 'review', source: { kind: 'step', step: 0, output: 'text', name: 'report' } };
describe('guided job results', () => {
  it('previews without mutation, wires atomically, and restores exact bytes in one Undo', () => {
    const doc = new WorkflowDocument({ source });
    const preview = doc.previewJobResult(request);
    expect(doc.getSnapshot().source).toBe(source);
    expect(doc.getSnapshot().canUndo).toBe(false);
    expect(preview).toMatchObject({ declaration: '${{ steps.step_1.outputs.text }}', addDependency: true, value: 'Summarize: ${{ needs.review.outputs.report }}' });
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().revision).toBe(1);
    const jobs = doc.getSnapshot().workflow!.jobs;
    expect(jobs.review.outputs).toEqual({ report: '${{ steps.step_1.outputs.text }}' });
    expect(jobs.summarize.needs).toEqual(['review']);
    expect(jobs.summarize.steps![0].with).toEqual({ model: 'keep', prompt: preview.value });
    expect(jobs.review.imported).toBe('keep');
    expect(doc.getSnapshot().source).toContain('# keep model');
    expect(doc.getSnapshot().source).toContain('# keep producer');
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    doc.redo(); expect(doc.getSnapshot().workflow!.jobs.summarize.needs).toEqual(['review']);
  });
  it('reuses matching declarations and existing dependencies', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['jobs', 'review', 'outputs'], { existing: '${{steps.step_1.outputs.text}}' });
    doc.setField(['jobs', 'summarize', 'needs'], 'review');
    const preview = doc.previewJobResult(request);
    expect(preview).toMatchObject({ output: 'existing', declaration: undefined, addDependency: false });
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().workflow!.jobs.review.outputs).toEqual({ existing: '${{steps.step_1.outputs.text}}' });
    expect(doc.getSnapshot().workflow!.jobs.summarize.needs).toEqual(['review']);
    expect(doc.getSnapshot().source).toContain('needs: review');
  });
  it('uses existing producer outputs and replaces only selected text', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['jobs', 'review', 'outputs'], { report: '${{ steps.step_1.outputs.text }}' });
    const preview = doc.previewJobResult({ ...request, source: { kind: 'output', output: 'report' }, selection: { start: 0, end: 9 } });
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].with!.prompt).toBe('${{ needs.review.outputs.report }}: ');
  });
  it('retains existing dependency comments when adding another producer', () => {
    const doc = new WorkflowDocument({ source: source + '  lint: {steps: [{argv: [echo]}]}\n' });
    doc.setField(['jobs', 'summarize', 'needs'], ['lint']);
    doc.setSource(doc.getSnapshot().source.replace('- lint', '- lint # keep dependency'));
    doc.applyJobResult(doc.previewJobResult(request));
    expect(doc.getSnapshot().source).toContain('# keep dependency');
    expect(doc.getSnapshot().workflow!.jobs.summarize.needs).toEqual(['lint', 'review']);
  });
  it('rejects conflicting names and cycles without any mutation', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['jobs', 'review', 'outputs'], { report: 'different' });
    const before = doc.getSnapshot().source;
    expect(() => doc.previewJobResult(request)).toThrow('conflicts');
    expect(doc.getSnapshot().source).toBe(before);
    doc.setField(['jobs', 'review', 'needs'], 'summarize');
    expect(() => doc.previewJobResult(request)).toThrow('cycle');
    expect(() => doc.previewJobResult({ ...request, producer: 'summarize' })).toThrow('cycle');
  });
  it('rejects transitive dependency cycles', () => {
    const doc = new WorkflowDocument({ source: source + '  middle: {needs: summarize, steps: [{argv: [echo]}]}\n' });
    doc.setField(['jobs', 'review', 'needs'], 'middle');
    expect(() => doc.previewJobResult(request)).toThrow('cycle');
  });
  it.each(['rename', 'delete', 'edit', 'undo'])('rejects a stale preview after %s', change => {
    const doc = new WorkflowDocument({ source });
    const preview = doc.previewJobResult(request);
    if (change === 'rename') doc.renameJob('review', 'renamed');
    if (change === 'delete') doc.deleteStep('review', 0);
    if (change === 'edit' || change === 'undo') doc.setField(['name'], 'Changed');
    if (change === 'undo') doc.undo();
    const before = doc.getSnapshot().source;
    expect(() => doc.applyJobResult(preview)).toThrow('document changed');
    expect(doc.getSnapshot().source).toBe(before);
  });
  it('revalidates requests and derives mutations rather than trusting preview data', () => {
    const doc = new WorkflowDocument({ source });
    const preview = doc.previewJobResult(request);
    preview.declaration = 'malicious'; preview.value = 'malicious'; preview.addDependency = false;
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().workflow!.jobs.review.outputs!.report).toBe('${{ steps.step_1.outputs.text }}');
    expect(doc.getSnapshot().workflow!.jobs.summarize.needs).toEqual(['review']);
    const invalid = doc.previewJobResult(request);
    invalid.request.source = { kind: 'output', output: 'missing' };
    expect(() => doc.applyJobResult(invalid)).toThrow('Unknown producer output');
  });
  it('guards targets, selections, producer steps, output names and viewers', () => {
    const doc = new WorkflowDocument({ source });
    expect(() => doc.previewJobResult({ ...request, target: ['name'] })).toThrow('expression-capable');
    expect(() => doc.previewJobResult({ ...request, target: ['jobs', 'summarize', 'steps', 20, 'with', 'prompt'] })).toThrow('target');
    expect(() => doc.previewJobResult({ ...request, selection: { start: 0, end: 999 } })).toThrow('selection');
    expect(() => doc.previewJobResult({ ...request, source: { kind: 'step', step: 20, output: 'text', name: 'result' } })).toThrow('step');
    expect(() => doc.previewJobResult({ ...request, source: { kind: 'step', step: 0, output: 'bad output', name: 'result' } })).toThrow('names');
    expect(() => new WorkflowDocument({ source }, true).previewJobResult(request)).toThrow('read-only');
  });
  it('uses workflow scope for callable outputs without adding dependencies', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['on', 'workflow_call', 'outputs', 'result'], { value: '' });
    const preview = doc.previewJobResult({ ...request, target: ['on', 'workflow_call', 'outputs', 'result', 'value'] });
    expect(preview.expression).toBe('${{ jobs.review.outputs.report }}');
    expect(preview.addDependency).toBe(false);
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().workflow!.jobs.review.needs).toBeUndefined();
  });
  it('wires an absent optional repeat field and preserves bare condition syntax', () => {
    const doc = new WorkflowDocument({ source });
    doc.applyJobResult(doc.previewJobResult({ ...request, target: ['jobs', 'summarize', 'repeat', 'until'] }));
    expect(doc.getSnapshot().workflow!.jobs.summarize.repeat!.until).toBe('${{ needs.review.outputs.report }}');
    doc.setField(['jobs', 'summarize', 'if'], 'needs.review.status == ');
    doc.applyJobResult(doc.previewJobResult({ ...request, target: ['jobs', 'summarize', 'if'] }));
    expect(doc.getSnapshot().workflow!.jobs.summarize.if).toBe('needs.review.status == needs.review.outputs.report');
  });
  it('rolls back the entire transaction when the target cannot accept a nested field', () => {
    const doc = new WorkflowDocument({ source });
    doc.setField(['jobs', 'summarize', 'secrets'], 'inherit');
    const before = doc.getSnapshot().source;
    expect(() => doc.applyJobResult(doc.previewJobResult({ ...request, target: ['jobs', 'summarize', 'secrets', 'token'] }))).toThrow();
    expect(doc.getSnapshot().source).toBe(before);
    expect(doc.getSnapshot().workflow!.jobs.review.outputs).toBeUndefined();
  });
});

it('moves and removes argument nodes while keeping exact values and comments', () => {
  const original = `jobs:\n  run:\n    steps:\n      - argv:\n          - echo\n          - 'a b' # keep argument\n          - ''\n          - '\"quoted\"'\n`;
  const doc = new WorkflowDocument({ source: original });
  const path = ['jobs', 'run', 'steps', 0, 'argv'];
  doc.moveArgument(path, 1, 3);
  expect(doc.getSnapshot().workflow!.jobs.run.steps![0].argv).toEqual(['echo', '', '"quoted"', 'a b']);
  expect(doc.getSnapshot().source).toContain('# keep argument');
  doc.moveArgument(path, 2);
  expect(doc.getSnapshot().workflow!.jobs.run.steps![0].argv).toEqual(['echo', '', 'a b']);
  doc.undo(); doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  expect(() => doc.moveArgument(path, 0, 1)).toThrow('position');
});
