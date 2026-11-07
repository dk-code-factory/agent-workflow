import { describe, expect, it } from 'vitest';
import { parseWorkflow } from '../src/validation';
import { WorkflowDocument } from '../src/document';
import { stepConversionImpact } from '../src/conversion';
import { readFileSync } from 'node:fs';
import timestampCases from '../../testdata/ui-compat/concise-timestamps.json';

describe('concise agent steps', () => {
  it.each(timestampCases)('matches runner string admission for $value', ({ value, timestamp }) => {
    const result = parseWorkflow(`jobs:\n  review:\n    steps:\n      - agent: claude\n        prompt: ${value}\n`);
    expect(result.diagnostics.some(d => d.code === 'shape' && d.path.at(-1) === 'prompt')).toBe(timestamp);
    const quoted = parseWorkflow(`jobs: {review: {steps: [{agent: claude, prompt: ${JSON.stringify(value)}}]}}`);
    expect(quoted.diagnostics).toEqual([]);
  });

  it('accepts explicit string tags and rejects other scalar tags', () => {
    expect(parseWorkflow('jobs: {review: {steps: [{agent: claude, prompt: !!str 2026-11-06}]}}').diagnostics).toEqual([]);
    expect(parseWorkflow('jobs: {review: {steps: [{agent: claude, prompt: !custom Review}]}}').diagnostics.some(d => d.code === 'shape')).toBe(true);
  });

  it('quotes an edited date prompt to match the runner-accepted fixture', () => {
    const expected = readFileSync(new URL('../../testdata/ui-compat/concise-prompt-edited-date.yml', import.meta.url), 'utf8');
    const doc = new WorkflowDocument({ source: 'jobs: {review: {steps: [{agent: claude, prompt: Review}]}}' });
    doc.setField(['jobs', 'review', 'steps', 0, 'prompt'], '2026-11-06');
    expect(doc.getSnapshot().source).toBe(expected);
    expect(doc.getSnapshot().diagnostics).toEqual([]);
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0].prompt).toBe('2026-11-06');
  });

  it.each([
    'agent: claude, prompt: Review',
    'agent: claude, with: {prompt: Review, model: sonnet}',
    'agent: claude, with: {message: Review}',
    "agent: claude, prompt: ''",
    'agent: claude, prompt: Review, argv: [], run: "", shell: "", timeout: "", working-directory: ""',
    'agent: claude, prompt: Review, argv: null, run: null, shell: null, timeout: null, working-directory: null',
  ])('accepts %s and keeps its authoring shape through visual edits', step => {
    const source = `# keep\njobs: {review: {steps: [{id: ask, ${step}}]}}`;
    const doc = new WorkflowDocument({ source });
    expect(doc.getSnapshot().source).toBe(source);
    expect(doc.getSnapshot().diagnostics).toEqual([]);
    const initial = doc.getSnapshot().workflow!.jobs.review.steps![0];
    doc.setField(['name'], 'Changed');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toEqual(initial);
    expect(doc.getSnapshot().source).toContain('# keep');
    expect(doc.getSnapshot().diagnostics).toEqual([]);
  });

  it.each([
    ['agent: claude, uses: agent/claude, prompt: Review', 'agent'],
    ['agent: claude, uses: null, prompt: Review', 'agent'],
    ['agent: claude, prompt: Review, with: {prompt: null}', 'prompt'],
    ['uses: agent/claude, prompt: Review', 'prompt'],
    ['agent: invalid/role, prompt: Review', 'agent'],
    ['agent: claude', 'with'],
  ])('diagnoses %s at its field', (step, field) => {
    const result = parseWorkflow(`jobs: {review: {steps: [{${step}}]}}`);
    expect(result.diagnostics.some(d => d.severity === 'error' && d.path.at(-1) === field)).toBe(true);
  });

  it.each(['argv: [echo]', 'run: echo', 'shell: sh', 'timeout: 1m', 'working-directory: /work'])('rejects active command fields: %s', command => {
    const result = parseWorkflow(`jobs: {review: {steps: [{agent: claude, prompt: Review, ${command}}]}}`);
    expect(result.diagnostics.some(d => d.severity === 'error' && d.code === 'command')).toBe(true);
  });

  it.each(['agent: null, prompt: Review', 'agent: true, prompt: Review', 'agent: claude, prompt: 42', 'agent: claude, prompt: [Review]', 'agent: claude, prompt: null'])('rejects nonstring shorthand fields: %s', step => {
    const result = parseWorkflow(`jobs: {review: {steps: [{${step}}]}}`);
    expect(result.usable).toBe(false);
    expect(result.diagnostics.some(d => d.code === 'shape')).toBe(true);
  });

  it('preserves concise prompts and options when changing agent and removes them in command conversion', () => {
    const source = 'jobs: {review: {steps: [{id: ask, agent: claude, prompt: Review, with: {model: sonnet, output: {type: object}}}]}}';
    const doc = new WorkflowDocument({ source });
    doc.setStepKind('review', 0, 'agent/codex');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toEqual({ id: 'ask', agent: 'codex', prompt: 'Review', with: { model: 'sonnet', output: { type: 'object' } } });
    expect(stepConversionImpact(doc.getSnapshot().workflow!.jobs.review.steps![0], 'argv')).toEqual(['agent', 'prompt', 'with']);
    doc.setStepKind('review', 0, 'argv');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toEqual({ id: 'ask', argv: ['echo', 'Hello'] });
    doc.undo(); doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });

  it('retains a nested prompt when changing a shorthand agent', () => {
    const doc = new WorkflowDocument({ source: 'jobs: {review: {steps: [{agent: claude, with: {prompt: Review, message: Context}}]}}' });
    doc.setStepKind('review', 0, 'agent/codex');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toEqual({ agent: 'codex', with: { prompt: 'Review', message: 'Context' } });
    expect(doc.getSnapshot().diagnostics).toEqual([]);
  });

  it('rewrites prompt expressions while keeping role and metadata literal', () => {
    const doc = new WorkflowDocument({ source: `jobs:
  producer:
    steps: [{id: command, argv: [echo]}]
    outputs: {text: '\${{ steps.command.outputs.stdout }}'}
  review:
    needs: producer
    steps:
      - id: first
        argv: [echo]
      - agent: claude
        name: '\${{ needs.producer.outputs.text }}'
        prompt: '\${{ needs.producer.outputs.text }} \${{ steps.first.outputs.stdout }}'
` });
    doc.renameJob('producer', 'build'); doc.renameStep('review', 0, 'prepare');
    const step = doc.getSnapshot().workflow!.jobs.review.steps![1];
    expect(step.prompt).toBe('${{ needs.build.outputs.text }} ${{ steps.prepare.outputs.stdout }}');
    expect(step.name).toBe('${{ needs.producer.outputs.text }}');
    expect(step.agent).toBe('claude');
    expect(doc.getSnapshot().diagnostics).toEqual([]);
  });

  it('wires job results into the concise prompt as one undoable edit', () => {
    const source = 'jobs: {build: {steps: [{id: run, argv: [echo]}]}, review: {steps: [{agent: claude, prompt: Review}]}}';
    const doc = new WorkflowDocument({ source });
    doc.applyJobResult(doc.previewJobResult({ target: ['jobs', 'review', 'steps', 0, 'prompt'], producer: 'build', source: { kind: 'step', step: 0, output: 'stdout', name: 'report' } }));
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0]).toEqual({ agent: 'claude', prompt: 'Review${{ needs.build.outputs.report }}' });
    expect(doc.getSnapshot().workflow!.jobs.review.needs).toEqual(['build']);
    expect(doc.getSnapshot().workflow!.jobs.build.outputs).toEqual({ report: '${{ steps.run.outputs.stdout }}' });
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });

  it('attaches unavailable-role warnings to agent without reinterpreting role expressions', async () => {
    const doc = new WorkflowDocument({ source: 'jobs: {review: {steps: [{agent: missing, prompt: Review}]}}', agents: [{ id: 'claude' }] });
    doc.start();
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'agent-catalog', path: ['jobs', 'review', 'steps', 0, 'agent'] }));
    const result = parseWorkflow('jobs: {review: {steps: [{agent: "${{ inputs.role }}", prompt: Review}]}}');
    expect(result.diagnostics.some(d => d.code === 'agent')).toBe(true);
  });
});
