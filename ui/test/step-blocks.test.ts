import { describe, expect, it } from 'vitest';
import { WorkflowDocument } from '../src/document';
import { expressionSuggestions } from '../src/source';
import { parseWorkflow } from '../src/validation';
import { stepEntries } from '../src/step-tree';
import { stepConversionImpact } from '../src/conversion';

const source = `jobs:
  review:
    steps:
      - id: prepare
        argv: [echo, ready]
      - id: reviews
        parallel:
          - id: first
            agent: claude
            prompt: \${{ steps.prepare.outputs.stdout }}
          - sequence:
              - id: collect
                argv: [echo, collect]
              - id: second
                agent: claude
                prompt: \${{ steps.collect.outputs.stdout }}
      - id: combine
        agent: claude
        prompt: \${{ steps.first.text }} \${{ steps.second.text }}
  summarize:
    needs: [review]
    steps: [{agent: claude, prompt: Summary}]
`;

describe('nested step blocks', () => {
  it('validates branch snapshots and exposes nested results only after joins', () => {
    const parsed = parseWorkflow(source);
    expect(parsed.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    expect(stepEntries(parsed.workflow!.jobs.review.steps).map(entry => entry.id)).toEqual(['prepare', 'reviews', 'first', 'step_2_parallel_2', 'collect', 'second', 'combine']);
    const path = ['jobs', 'review', 'steps', 1, 'parallel', 1, 'sequence', 1, 'prompt'];
    const nested = expressionSuggestions(parsed.workflow, 'review', 1, path);
    expect(nested).toContain('steps.collect.outputs.stdout');
    expect(nested).toContain('steps.prepare.outputs.stdout');
    expect(nested).not.toContain('steps.first.text');
    expect(nested).not.toContain('steps.second.text');
    expect(expressionSuggestions(parsed.workflow, 'review', 2)).toEqual(expect.arrayContaining(['steps.first.text', 'steps.second.text']));
    expect(expressionSuggestions(parsed.workflow, 'summarize', 0)).toContain('needs.review.steps.second.text');
    for (const reference of ['steps.first.text', 'steps.first.outputs.text', 'steps.combine.text']) {
      const invalid = parseWorkflow(source.replace('steps.collect.outputs.stdout', reference));
      expect(invalid.diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path }));
    }
  });

  it('rejects duplicate nested IDs, mixed operations, and empty blocks', () => {
    for (const yaml of [
      'jobs: {j: {steps: [{parallel: [{id: x, argv: [echo]}, {sequence: [{id: x, argv: [echo]}]}]}]}}',
      'jobs: {j: {steps: [{parallel: [], argv: [echo]}]}}',
      'jobs: {j: {steps: [{parallel: null}]}}',
      'jobs: {j: {steps: [{parallel: [{argv: [echo]}], continue-on-error: false}]}}',
    ]) expect(parseWorkflow(yaml).diagnostics.some(d => d.severity === 'error')).toBe(true);
    const malformed = parseWorkflow('jobs: {j: {steps: [{parallel: [{sequence: [not-a-step]}]}]}}');
    expect(malformed.usable).toBe(false);
  });

  it('duplicates nested IDs and local references, preserving originals and undo', () => {
    const doc = new WorkflowDocument({ source });
    doc.duplicateStep('review', 1);
    const entries = stepEntries(doc.getSnapshot().workflow!.jobs.review.steps);
    expect(new Set(entries.map(entry => entry.id)).size).toBe(entries.length);
    const copy = doc.getSnapshot().workflow!.jobs.review.steps![2];
    expect(copy.parallel![1].sequence![1].prompt).toBe('${{ steps.collect_copy.outputs.stdout }}');
    expect(copy.parallel![0].prompt).toBe('${{ steps.prepare.outputs.stdout }}');
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    expect(doc.deletionImpact('review', 1)).toContainEqual(['jobs', 'review', 'steps', 2, 'prompt']);
    expect(() => doc.deleteStep('review', 1)).toThrow('confirmation');
  });

  it('materializes nested IDs before reordering and renames references inside branches', () => {
    const doc = new WorkflowDocument({ source });
    doc.renameStep('review', 0, 'ready');
    expect(doc.getSnapshot().workflow!.jobs.review.steps![1].parallel![0].prompt).toContain('steps.ready.outputs.stdout');
    expect(() => doc.renameStep('review', 0, 'collect')).toThrow('unique');
    doc.moveStep('review', 1, 0);
    expect(doc.getSnapshot().workflow!.jobs.review.steps![0].parallel![1].id).toBe('step_2_parallel_2');
    expect(stepConversionImpact(doc.getSnapshot().workflow!.jobs.review.steps![0], 'argv')).toContain('parallel');
    doc.setStepKind('review', 1, 'parallel');
    const ids = stepEntries(doc.getSnapshot().workflow!.jobs.review.steps).map(entry => entry.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('wires nested producer results without declaring output aliases', () => {
    const doc = new WorkflowDocument({ source });
    const preview = doc.previewJobResult({ target: ['jobs', 'summarize', 'steps', 0, 'prompt'], producer: 'review', source: { kind: 'result', step: ['steps', 1, 'parallel', 1, 'sequence', 1], field: 'text' } });
    expect(preview.expression).toBe('${{ needs.review.steps.second.text }}');
    doc.applyJobResult(preview);
    expect(doc.getSnapshot().workflow!.jobs.summarize.steps![0].prompt).toContain('needs.review.steps.second.text');
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });
});
