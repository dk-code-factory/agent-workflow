import { describe, expect, it } from 'vitest';
import { WorkflowDocument } from '../src/document';
import { references } from '../src/expressions';
import { expressionSuggestions } from '../src/source';
import { parseWorkflow } from '../src/validation';

const workflow = {
  on: { workflow_call: { outputs: { answer: { value: '${{ jobs.review.steps.analyze.approved }}' } } } },
  jobs: {
    review: { steps: [
      { id: 'analyze', agent: 'claude', prompt: 'Review', with: { output: { format: 'json', schema: { type: 'object', properties: { approved: { type: 'boolean' }, details: { type: 'object', properties: { reason: { type: 'string' } } }, status: { type: 'string' } } } } } },
      { id: 'finish', agent: 'claude', prompt: '${{ steps.analyze.approved }}' },
    ] },
    summarize: { needs: ['review'], steps: [{ agent: 'claude', prompt: '${{ needs.review.steps.analyze.details.reason }}' }] },
  },
};

describe('named step result references', () => {
  it('retains every suffix while renaming producers and steps in local, dependency and callable scopes', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.renameStep('review', 0, 'inspect'); doc.renameJob('review', 'check');
    const snapshot = doc.getSnapshot();
    expect(snapshot.workflow!.jobs.check.steps![1].prompt).toBe('${{ steps.inspect.approved }}');
    expect(snapshot.workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.check.steps.inspect.details.reason }}');
    expect(snapshot.workflow!.on).toMatchObject({ workflow_call: { outputs: { answer: { value: '${{ jobs.check.steps.inspect.approved }}' } } } });
    expect(snapshot.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); doc.undo(); expect(doc.getSnapshot().source).toBe(JSON.stringify(workflow));
  });
  it('does not confuse a cross-job step output with an explicit job output', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.setField(['jobs', 'review', 'outputs', 'text'], 'declared');
    doc.setField(['jobs', 'summarize', 'steps', 0, 'prompt'], '${{ needs.review.outputs.text }} ${{ needs.review.steps.analyze.outputs.text }}');
    doc.renameEntry(['jobs', 'review', 'outputs', 'text'], 'report');
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.review.outputs.report }} ${{ needs.review.steps.analyze.outputs.text }}');
    doc.renameStep('review', 0, 'inspect');
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.review.outputs.report }} ${{ needs.review.steps.inspect.outputs.text }}');
  });
  it('preserves implicit step identity through reorder and includes dependent references in deletion review', () => {
    const doc = new WorkflowDocument({ source: 'jobs: {review: {steps: [{argv: [echo]}, {argv: [echo]}]}, summarize: {needs: review, steps: [{agent: claude, prompt: "${{ needs.review.steps.step_1.text }}"}]}}' });
    doc.moveStep('review', 0, 1);
    expect(doc.getSnapshot().workflow!.jobs.review.steps!.map(step => step.id)).toEqual(['step_2', 'step_1']);
    expect(doc.deletionImpact('review', 1)).toContainEqual(['jobs', 'summarize', 'steps', 0, 'prompt']);
    expect(() => doc.deleteStep('review', 1)).toThrow('confirmation');
    doc.deleteStep('review', 1, true);
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', message: 'Unknown step "step_1" of job "review"' }));
  });
  it.each([
    ['forward', 'steps.finish.text', 'review', 0],
    ['undeclared dependency', 'needs.review.steps.analyze.text', 'isolated', 0],
    ['job root inside a job', 'jobs.review.steps.analyze.text', 'summarize', 0],
    ['unknown dependency step', 'needs.review.steps.absent.text', 'summarize', 0],
  ])('diagnoses %s at its consuming field', (_label, expression, job, index) => {
    const value = structuredClone(workflow);
    Object.assign(value.jobs, { isolated: { steps: [{ agent: 'claude', prompt: 'Read' }] } });
    const doc = new WorkflowDocument({ source: JSON.stringify(value) });
    const path = ['jobs', job, 'steps', index, 'prompt'];
    doc.setField(path, '${{ ' + expression + ' }}');
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path, severity: 'error' }));
  });
  it('rejects reusable and per-item producer internals while retaining legacy references', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    doc.setField(['jobs', 'review'], { uses: 'workflows/review.yml' });
    expect(doc.getSnapshot().diagnostics.some(d => d.message.includes('does not expose individual step results'))).toBe(true);
    doc.setField(['jobs', 'review', 'for-each'], { items: ['one'], key: 'item' });
    expect(doc.getSnapshot().diagnostics.some(d => d.message.includes('does not expose individual step results'))).toBe(true);
    expect(parseWorkflow('jobs: {review: {steps: [{id: one, argv: [echo, "${{ steps.one.status }}"]}]}}').diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
  });
  it('offers known structured fields and direct dependency text without in-flight or reserved aliases', () => {
    const parsed = parseWorkflow(JSON.stringify(workflow));
    const local = expressionSuggestions(parsed.workflow, 'review', 1);
    expect(local).toEqual(expect.arrayContaining(['steps.analyze.text', 'steps.analyze.approved', 'steps.analyze.details.reason', 'steps.analyze.outputs.structured']));
    expect(local).not.toContain('steps.analyze.status'); expect(local).not.toContain('steps.finish.text');
    expect(expressionSuggestions(parsed.workflow, 'summarize', 0)).toContain('needs.review.steps.analyze.approved');
    expect(expressionSuggestions(parsed.workflow)).toContain('jobs.review.steps.analyze.text');
    expect(expressionSuggestions(parsed.workflow, 'review')).not.toContain('steps.analyze.text');
    expect(expressionSuggestions(parsed.workflow, 'review', undefined, ['jobs', 'review', 'outputs', 'result'])).toContain('steps.analyze.text');
  });
  it('only offers concise results for agent producers in every scope', () => {
    const parsed = parseWorkflow(JSON.stringify({ jobs: {
      build: { steps: [
        { id: 'command', argv: ['echo', 'ready'] },
        { id: 'shell', run: 'echo ready', shell: 'sh' },
        { id: 'controller', uses: 'workspace/create' },
        { id: 'legacy', uses: 'agent/claude', with: { prompt: 'Review', output: { format: 'json', schema: { type: 'object', properties: { approved: { type: 'boolean' } } } } } },
        { id: 'concise', agent: 'claude', prompt: 'Summarize' },
        { id: 'consume', agent: 'claude', prompt: 'Read' },
      ] },
      consumer: { needs: ['build'], steps: [{ agent: 'claude', prompt: 'Read' }] },
    } }));
    expect(parsed.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    for (const [suggestions, prefix] of [
      [expressionSuggestions(parsed.workflow, 'build', 5), 'steps'],
      [expressionSuggestions(parsed.workflow, 'consumer', 0), 'needs.build.steps'],
      [expressionSuggestions(parsed.workflow), 'jobs.build.steps'],
    ] as const) {
      expect(suggestions).toContain(`${prefix}.legacy.text`);
      expect(suggestions).toContain(`${prefix}.legacy.approved`);
      expect(suggestions).toContain(`${prefix}.concise.text`);
      for (const id of ['command', 'shell', 'controller']) expect(suggestions).not.toContain(`${prefix}.${id}.text`);
    }
    expect(expressionSuggestions(parsed.workflow, 'build', 5)).toContain('steps.command.outputs.stdout');
  });
  it('keeps data roots and quoted expressions distinct from cross-job step references', () => {
    expect(references("${{ 'needs.review.steps.analyze.text' == needs.review.steps.analyze.outputs.structured.status }}")).toEqual([
      expect.objectContaining({ scope: 'needs', id: 'review', step: 'analyze', output: 'structured', tail: '.status' }),
    ]);
    expect(references('${{ inputs.config.needs.review.steps.analyze.text }}')).toEqual([expect.objectContaining({ scope: 'inputs', id: 'config', step: undefined })]);
  });
  it('resolves named output schemas for local, dependency and workflow completion', () => {
    const value = structuredClone(workflow);
    const schema = value.jobs.review.steps[0].with!.output.schema;
    const named = { ...value, schemas: { Review: schema } };
    Object.assign(named.jobs.review.steps[0].with!.output, { schema: 'Review' });
    const parsed = parseWorkflow(JSON.stringify(named));
    expect(expressionSuggestions(parsed.workflow, 'review', 1)).toContain('steps.analyze.details.reason');
    expect(expressionSuggestions(parsed.workflow, 'summarize', 0)).toContain('needs.review.steps.analyze.approved');
    expect(expressionSuggestions(parsed.workflow)).toContain('jobs.review.steps.analyze.details.reason');
  });
  it.each([
    ['preceding', 'steps.first.text', false],
    ['self', 'steps.consume.text', true],
    ['forward', 'steps.last.text', true],
    ['dependency', 'needs.producer.steps.analyze.text', false],
    ['wrong root', 'jobs.producer.steps.analyze.text', true],
  ])('checks %s references inside named schemas in the consuming step context', (_name, expression, invalid) => {
    const parsed = parseWorkflow(JSON.stringify({ schemas: { Result: { type: 'string', enum: ['${{ ' + expression + ' }}'] } }, jobs: {
      producer: { steps: [{ id: 'analyze', agent: 'claude', prompt: 'Produce' }] },
      consumer: { needs: ['producer'], steps: [
        { id: 'first', agent: 'claude', prompt: 'First' },
        { id: 'consume', agent: 'claude', prompt: 'Consume', with: { output: { schema: 'Result' } } },
        { id: 'last', agent: 'claude', prompt: 'Last' },
      ] },
    } }));
    expect(parsed.diagnostics.some(d => d.code === 'expression-reference' && d.path.join('.') === 'schemas.Result.enum.0')).toBe(invalid);
  });
  it('rewrites concise references in shared named contracts and dependent declarations together', () => {
    const doc = new WorkflowDocument({ source: 'schemas: {Shared: {type: string, enum: ["${{ steps.local.text }}"]}, Dependent: {type: string, enum: ["${{ needs.review.steps.local.text }}"]}}\njobs: {review: {steps: [{id: local, agent: claude, prompt: Produce}, {agent: claude, prompt: Consume, with: {output: {schema: Shared}}}]}, second: {steps: [{id: local, agent: claude, prompt: Produce}, {agent: claude, prompt: Consume, with: {output: {schema: Shared}}}]}, summarize: {needs: review, steps: [{agent: claude, prompt: Summarize, with: {output: {schema: Dependent}}}]}}' });
    const original = doc.getSnapshot().source;
    doc.renameStep('review', 0, 'renamed');
    const snapshot = doc.getSnapshot();
    expect(snapshot.workflow!.schemas!.Shared.enum).toEqual(['${{ steps.local.text }}']);
    expect(snapshot.workflow!.schemas!.Shared_review.enum).toEqual(['${{ steps.renamed.text }}']);
    expect(snapshot.workflow!.schemas!.Dependent.enum).toEqual(['${{ needs.review.steps.renamed.text }}']);
    expect(Object.keys(snapshot.workflow!.schemas!)).toEqual(['Shared', 'Dependent', 'Shared_review']);
    expect(snapshot.diagnostics.filter(d => d.code === 'expression-reference')).toEqual([]);
    expect(doc.deletionImpact('review', 0)).toContainEqual(['schemas', 'Dependent', 'enum', 0]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
});

describe('direct named result wiring', () => {
  it('adds only dependency and expression atomically and rejects stale previews', () => {
    const value = structuredClone(workflow); value.jobs.summarize.needs = [];
    const original = JSON.stringify(value);
    const doc = new WorkflowDocument({ source: original });
    const request = { target: ['jobs', 'summarize', 'steps', 0, 'prompt'], producer: 'review', source: { kind: 'result' as const, step: 0, field: 'details.reason' }, selection: { start: 0, end: value.jobs.summarize.steps[0].prompt.length } };
    const preview = doc.previewJobResult(request);
    expect(preview.declaration).toBeUndefined(); expect(doc.getSnapshot().source).toBe(original);
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toBe('${{ needs.review.steps.analyze.details.reason }}');
    expect(doc.getSnapshot().workflow!.jobs.review.outputs).toBeUndefined();
    expect(doc.getSnapshot().workflow!.jobs.summarize.needs).toEqual(['review']);
    expect(() => doc.applyJobResult(preview)).toThrow('document changed');
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
  it('uses workflow scope and validates nested field paths', () => {
    const doc = new WorkflowDocument({ source: JSON.stringify(workflow) });
    const request = { target: ['on', 'workflow_call', 'outputs', 'answer', 'value'], producer: 'review', source: { kind: 'result' as const, step: 0, field: 'outputs.structured.status' } };
    expect(doc.previewJobResult(request).expression).toBe('${{ jobs.review.steps.analyze.outputs.structured.status }}');
    expect(() => doc.previewJobResult({ ...request, source: { ...request.source, field: 'bad..path' } })).toThrow('field path');
    doc.setField(['jobs', 'review'], { uses: 'workflows/review.yml' });
    expect(() => doc.previewJobResult(request)).toThrow('does not expose');
  });
});
