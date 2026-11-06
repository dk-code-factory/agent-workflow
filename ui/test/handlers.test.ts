import { describe, expect, it } from 'vitest';
import { stringify } from 'yaml';
import { WorkflowDocument } from '../src/document';
import { stepConversionImpact } from '../src/conversion';
import { expressionSuggestions } from '../src/source';
import { parseWorkflow } from '../src/validation';
import { hasExpressionReferences, isExpressionPath } from '../src/expressions';

const source = `# preserve handlers
jobs:
  build:
    steps:
      - id: earlier
        agent: claude
        prompt: Prepare
      - id: task
        agent: claude
        prompt: Work
        handlerTimeout: 45s
        onError:
          - id: earlier
            agent: codex
            prompt: \${{ failed.prompt }} \${{ steps.earlier.text }}
          - agent: claude
            prompt: \${{ steps.earlier.text }} \${{ error.message }}
        onFailure:
          - argv: [echo, '\${{ error.class }}']
        finally:
          - argv: [echo, done]
`;

describe('step lifecycle handler authoring', () => {
  it('preserves hooks through focused edits, conversion and undo', () => {
    const doc = new WorkflowDocument({ source });
    expect(doc.getSnapshot().diagnostics).toEqual([]);
    const original = doc.getSnapshot().workflow!.jobs.build.steps![1];
    doc.setField(['name'], 'Handlers');
    doc.setStepKind('build', 1, 'agent/codex');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![1]).toEqual({ ...original, agent: 'codex' });
    expect(doc.getSnapshot().source).toContain('# preserve handlers');
    doc.undo(); doc.undo();
    expect(doc.getSnapshot().source).toBe(source);
  });

  it('rewrites inherited references without changing a shadowing hook sibling', () => {
    const doc = new WorkflowDocument({ source });
    doc.renameStep('build', 0, 'prepared');
    const fallback = doc.getSnapshot().workflow!.jobs.build.steps![1].onError!;
    expect(fallback[0].prompt).toContain('steps.prepared.text');
    expect(fallback[1].prompt).toContain('steps.earlier.text');
    expect(doc.getSnapshot().diagnostics).toEqual([]);
  });

  it('tracks named schemas consumed by nested handlers without rewriting shadowed consumers', () => {
    const original = stringify({ schemas: { Shared: { type: 'string', enum: ['${{ steps.earlier.text }}'] } }, jobs: { build: { steps: [
      { id: 'earlier', agent: 'claude', prompt: 'Prepare' },
      { agent: 'claude', prompt: 'Work', onError: [
        { agent: 'codex', prompt: 'Inherited', with: { output: { schema: 'Shared' } } },
        { id: 'earlier', agent: 'codex', prompt: 'Local' },
        { agent: 'claude', prompt: 'Shadowed', with: { output: { schema: 'Shared' } } },
      ], finally: [{ agent: 'claude', prompt: 'Finalize', with: { output: { schema: 'Shared' } } }] },
    ] } } });
    const doc = new WorkflowDocument({ source: original });
    expect(doc.deletionImpact('build', 0)).toContainEqual(['schemas', 'Shared', 'enum', 0]);
    expect(() => doc.deleteStep('build', 0)).toThrow('references');
    doc.renameStep('build', 0, 'prepared');
    const edited = doc.getSnapshot();
    expect(edited.workflow!.schemas!.Shared.enum).toEqual(['${{ steps.earlier.text }}']);
    expect(edited.workflow!.schemas!.Shared_build.enum).toEqual(['${{ steps.prepared.text }}']);
    const task = edited.workflow!.jobs.build.steps![1];
    expect((task.onError![0].with!.output as any).schema).toBe('Shared_build');
    expect((task.onError![2].with!.output as any).schema).toBe('Shared');
    expect((task.finally![0].with!.output as any).schema).toBe('Shared_build');
    expect(edited.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.undo(); expect(doc.getSnapshot().source).toBe(original);
    doc.redo(); expect(doc.getSnapshot().source).toBe(edited.source);
    doc.deleteStep('build', 0, true);
    expect(doc.getSnapshot().diagnostics).toContainEqual(expect.objectContaining({ code: 'expression-reference', path: ['schemas', 'Shared_build', 'enum', 0] }));
  });

  it.each(['earlier', 'task', 'fallback', 'later', 'missing'])('validates named handler schemas against the local scope: %s', reference => {
    const yaml = stringify({ schemas: { Result: { type: 'string', enum: [`\${{ steps.${reference}.text }}`] } }, jobs: { build: { steps: [
      { id: 'earlier', agent: 'claude', prompt: 'Prepare' },
      { id: 'task', agent: 'claude', prompt: 'Work', onError: [
        { id: 'fallback', agent: 'claude', prompt: 'Fallback', with: { output: { schema: 'Result' } } },
        { id: 'later', agent: 'claude', prompt: 'Later' },
      ] },
    ] } } });
    expect(parseWorkflow(yaml).diagnostics.some(d => d.code === 'expression-reference' && d.path.join('.') === 'schemas.Result.enum.0')).toBe(reference !== 'earlier');
  });

  it('discloses unsupported handlers when converting to a controller action', () => {
    const doc = new WorkflowDocument({ source });
    expect(stepConversionImpact(doc.getSnapshot().workflow!.jobs.build.steps![1], 'workspace/create')).toEqual(expect.arrayContaining(['onError', 'onFailure', 'finally', 'handlerTimeout']));
    doc.setStepKind('build', 1, 'workspace/create');
    expect(doc.getSnapshot().workflow!.jobs.build.steps![1].onError).toBeUndefined();
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  });

  it('suggests authenticated failure context only inside hooks and uses preceding sibling scope', () => {
    const w = parseWorkflow(source).workflow!;
    const path = ['jobs', 'build', 'steps', 1, 'onError', 1, 'prompt'];
    expect(expressionSuggestions(w, 'build', 1, path)).toEqual(expect.arrayContaining(['failed.prompt', 'error.message', 'steps.earlier.text']));
    expect(expressionSuggestions(w, 'build', 1)).not.toContain('failed.prompt');
    expect(expressionSuggestions(w, 'build', 1, ['jobs', 'build', 'steps', 1, 'finally', 0, 'argv', 1])).not.toContain('steps.step_2.text');
    expect(hasExpressionReferences(path)).toBe(true);
    expect(isExpressionPath(['jobs', 'build', 'steps', 1, 'onError', 0, 'if'])).toBe(true);
    expect(hasExpressionReferences(['jobs', 'build', 'steps', 1, 'with', 'onError', 0, 'prompt'])).toBe(true);
  });

  it.each([
    ['job hooks', 'jobs: {build: {onError: [], steps: [{argv: [echo]}]}}', 'unknown-field'],
    ['workflow hooks', 'finally: []\njobs: {build: {steps: [{argv: [echo]}]}}', 'unknown-field'],
    ['invalid nested action', 'jobs: {build: {steps: [{argv: [echo], onError: [{uses: unknown}]}]}}', 'action'],
    ['invalid duration', 'jobs: {build: {steps: [{argv: [echo], handlerTimeout: 6m}]}}', 'handler-timeout'],
    ['orphan timeout', 'jobs: {build: {steps: [{argv: [echo], handlerTimeout: 45s}]}}', 'handler-timeout'],
    ['wrong hook shape', 'jobs: {build: {steps: [{argv: [echo], finally: nope}]}}', 'shape'],
    ['context outside hook', 'jobs: {build: {steps: [{agent: claude, prompt: "${{ failed.prompt }}"}]}}', 'handler-context'],
    ['invalid context field', source.replace('failed.prompt', 'failed.credentials'), 'handler-context'],
    ['sibling hook invisibility', source.replace('argv: [echo, done]', 'argv: [echo, "${{ steps.step_2.text }}"]'), 'expression-reference'],
  ])('diagnoses %s', (_name, yaml, code) => {
    expect(parseWorkflow(yaml).diagnostics.some(d => d.code === code && d.severity === 'error')).toBe(true);
  });

  it('bounds nesting and handler count', () => {
    let nested: object = { argv: ['echo'] };
    for (let i = 0; i < 9; i++) nested = { argv: ['echo'], onError: [nested] };
    expect(parseWorkflow(stringify({ jobs: { build: { steps: [nested] } } })).diagnostics.some(d => d.code === 'handler-depth')).toBe(true);
    expect(parseWorkflow(stringify({ jobs: { build: { steps: [{ argv: ['echo'], finally: Array.from({ length: 101 }, () => ({ argv: ['echo'] })) }] } } })).diagnostics.some(d => d.code === 'handler-count')).toBe(true);
  });
});
