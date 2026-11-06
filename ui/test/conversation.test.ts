import { describe, expect, it } from 'vitest';
import { parseWorkflow } from '../src/validation';
import { WorkflowDocument } from '../src/document';
import { expressionSuggestions } from '../src/source';
import { stepConversionImpact } from '../src/conversion';

const source = `schemas:
  decision: {type: object, properties: {approved: {type: boolean}}}
jobs:
  solve:
    steps:
      - id: solution
        conversation: [builder, reviewer]
        finishBy: reviewer
        prompt: Complete the task.
        with: {output: {format: json, schema: decision}}
      - id: report
        agent: reviewer
        prompt: '\${{ steps.solution.approved }}'
`;

describe('YAML conversations', () => {
  it('admits conversations with default bounds and exposes their named completion fields', () => {
    const result = parseWorkflow(source);
    expect(result.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    expect(result.diagnostics.some(d => d.code === 'host-conversation')).toBe(true);
    expect(parseWorkflow(source.replace('finishBy: reviewer', 'start: \"\"\n        finishBy: reviewer')).diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    expect(expressionSuggestions(result.workflow, 'solve', 1)).toContain('steps.solution.approved');
    expect(expressionSuggestions(result.workflow, 'solve', 0)).not.toContain('steps.solution.approved');
  });

  it.each([
    ['conversation: [builder, builder], finishBy: builder, prompt: Work', 'conversation'],
    ['conversation: [builder, null, reviewer], finishBy: reviewer, prompt: Work', 'conversation'],
    ['conversation: [builder], finishBy: builder, prompt: Work', 'conversation'],
    ['conversation: [builder, reviewer], finishBy: outsider, prompt: Work', 'finishBy'],
    ['conversation: [builder, reviewer], finishBy: reviewer, start: outsider, prompt: Work', 'start'],
    ['conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, maxTurns: 101', 'maxTurns'],
    ['conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, maxMessages: 257', 'maxMessages'],
    ['conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, timeout: 25h', 'timeout'],
    ['conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, uses: agent/builder', 'uses'],
    ['conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, with: {message: hidden}', 'message'],
    ['conversation: [builder, reviewer], finishBy: reviewer, prompt: ""', 'prompt'],
    ['agent: builder, prompt: Work, maxTurns: 4', 'maxTurns'],
  ])('diagnoses invalid conversation fields: %s', (step, field) => {
    expect(parseWorkflow(`jobs: {solve: {steps: [{${step}}]}}`).diagnostics).toContainEqual(expect.objectContaining({ severity: 'error', path: ['jobs', 'solve', 'steps', 0, ...(field === 'message' ? ['with'] : []), field] }));
  });

  it('keeps prompts and compatible contracts when converting through conversations', () => {
    const doc = new WorkflowDocument({ source: 'jobs: {solve: {steps: [{agent: claude, prompt: Work, with: {model: chosen, message: Old, output: {format: json}}}]}}' });
    expect(stepConversionImpact(doc.getSnapshot().workflow!.jobs.solve.steps![0], 'conversation')).toEqual(['agent', 'with.message']);
    doc.setStepKind('solve', 0, 'conversation');
    expect(doc.getSnapshot().workflow!.jobs.solve.steps![0]).toMatchObject({ conversation: ['builder', 'reviewer'], finishBy: 'reviewer', prompt: 'Work', with: { model: 'chosen', output: { format: 'json' } } });
    doc.setStepKind('solve', 0, 'agent/claude');
    expect(doc.getSnapshot().workflow!.jobs.solve.steps![0]).toEqual({ agent: 'claude', prompt: 'Work', with: { model: 'chosen', output: { format: 'json' } } });
    doc.undo();
    expect(doc.getSnapshot().workflow!.jobs.solve.steps![0].conversation).toEqual(['builder', 'reviewer']);
  });

  it('rewrites conversation prompt references and preserves literal participant identities', () => {
    const doc = new WorkflowDocument({ source });
    doc.renameStep('solve', 0, 'decision');
    expect(doc.getSnapshot().workflow!.jobs.solve.steps![1].prompt).toBe('${{ steps.decision.approved }}');
    expect(doc.getSnapshot().workflow!.jobs.solve.steps![0].conversation).toEqual(['builder', 'reviewer']);
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
  });

  it('checks named schema and participant catalogs at their exact fields', () => {
    const doc = new WorkflowDocument({ source: source.replace('schema: decision', 'schema: absent'), agents: [{ id: 'builder' }] });
    doc.start();
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'schema-reference', path: ['jobs', 'solve', 'steps', 0, 'with', 'output', 'schema'] }));
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'agent-catalog', path: ['jobs', 'solve', 'steps', 0, 'conversation', 1] }));
  });

  it('requires an object completion schema for managed finisher arguments', () => {
    const scalar = parseWorkflow(source.replace('decision: {type: object', 'decision: {type: string'));
    expect(scalar.diagnostics).toContainEqual(expect.objectContaining({ code: 'conversation-output', path: ['jobs', 'solve', 'steps', 0, 'with', 'output', 'schema'] }));
    const inline = parseWorkflow(source.replace('schema: decision', 'schema: {type: array}'));
    expect(inline.diagnostics.some(d => d.code === 'conversation-output')).toBe(true);
  });

  it('wires conversation results across jobs and reverses the dependency and prompt together', () => {
    const doc = new WorkflowDocument({ source: source + '  report:\n    steps: [{agent: reviewer, prompt: Review}]\n' });
    const before = doc.getSnapshot().source;
    doc.applyJobResult(doc.previewJobResult({ target: ['jobs', 'report', 'steps', 0, 'prompt'], producer: 'solve', source: { kind: 'result', step: 0, field: 'approved' } }));
    expect(doc.getSnapshot().workflow!.jobs.report.needs).toEqual(['solve']);
    expect(doc.getSnapshot().workflow!.jobs.report.steps![0].prompt).toBe('Review${{ needs.solve.steps.solution.approved }}');
    doc.undo();
    expect(doc.getSnapshot().source).toBe(before);
  });

  it('rebinds named contracts consumed by conversations when copying a sequence', () => {
    const original = `schemas:
  Completion:
    type: object
    properties:
      text: {type: string, enum: ['\${{ steps.collect.outputs.stdout }}']}
jobs:
  solve:
    steps:
      - sequence:
          - {id: collect, argv: [echo, expected]}
          - {id: solution, conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, with: {output: {format: json, schema: Completion}}}
`;
    const doc = new WorkflowDocument({ source: original });
    doc.duplicateStep('solve', 0);
    const snapshot = doc.getSnapshot();
    const copied = snapshot.workflow!.jobs.solve.steps![1].sequence![1];
    expect(copied.with?.output).toEqual({ format: 'json', schema: 'Completion_copy' });
    expect((snapshot.workflow!.schemas!.Completion_copy.properties as any).text.enum).toEqual(['${{ steps.collect_copy.outputs.stdout }}']);
    expect((snapshot.workflow!.schemas!.Completion.properties as any).text.enum).toEqual(['${{ steps.collect.outputs.stdout }}']);
    expect(snapshot.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
  });
});
