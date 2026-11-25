import { describe, expect, it } from 'vitest';
import { WorkflowDocument } from '../src/document';
import { parseWorkflow } from '../src/validation';

const source = `step-definitions:
  greet:
    parameters: {message: {type: string, required: true}}
    step: {argv: [echo, '\${{ parameters.message }}']}
jobs:
  build:
    steps:
      - {id: first, use-definition: greet, with: {message: hello}}
      - {id: second, use-definition: greet, with: {message: '\${{ steps.first.outputs.stdout }}'}}
`;
describe('step definition authoring', () => {
  it('preserves declarations, references and parameters across visual edits and history', () => {
    const doc = new WorkflowDocument({ source });
    expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    doc.setField(['jobs', 'build', 'steps', 0, 'with', 'message'], 'changed');
    const edited = doc.getSnapshot().source;
    expect(doc.getSnapshot().workflow?.['step-definitions']).toEqual(parseWorkflow(source).workflow?.['step-definitions']);
    expect(edited).toContain('use-definition: greet');
    doc.undo(); expect(doc.getSnapshot().source).toBe(source);
    doc.redo(); expect(doc.getSnapshot().source).toBe(edited);
    doc.setStepKind('build', 0, 'argv');
    expect(doc.getSnapshot().workflow?.jobs.build.steps?.[0]['use-definition']).toBeUndefined();
    doc.undo(); expect(doc.getSnapshot().source).toBe(edited);
  });
  it('diagnoses unknown definitions and missing/unknown bindings', () => {
    for (const [old, next] of [['use-definition: greet', 'use-definition: missing'], ['message: hello', 'extra: hello'], ['with: {message: hello}', 'with: {}']]) {
      expect(parseWorkflow(source.replace(old, next)).diagnostics.some(d => d.code === 'step-definition' && d.severity === 'error')).toBe(true);
    }
  });
  it('leaves expanded result admission to the host without losing the authoring form', () => {
    const parsed = parseWorkflow(source.replace('steps.first.outputs.stdout', 'steps.first__child.outputs.stdout'));
    expect(parsed.diagnostics.filter(d => d.severity === 'error')).toEqual([]);
    expect(parsed.workflow?.jobs.build.steps?.[0]['use-definition']).toBe('greet');
    expect(parsed.diagnostics.some(d => d.code === 'host-step-definition')).toBe(true);
  });
});

it('gives an anonymous step an explicit identity when converting it into a definition use', () => {
  const doc = new WorkflowDocument({ source: 'step-definitions: {greet: {step: {argv: [echo]}}}\njobs: {build: {steps: [{argv: [echo]}]}}' });
  doc.setStepKind('build', 0, 'definition');
  doc.setField(['jobs', 'build', 'steps', 0, 'use-definition'], 'greet');
  expect(doc.getSnapshot().workflow?.jobs.build.steps?.[0].id).toBe('step_1');
  expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
});
