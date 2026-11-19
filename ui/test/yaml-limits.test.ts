import { expect, it } from 'vitest';
import { WorkflowDocument } from '../src/document';
import { parseWorkflow } from '../src/validation';
import { references } from '../src/expressions';

it('retains inherited containers and numeric expressions through edits and history', () => {
  const source = `on: {workflow_call: {inputs: {bound: {type: number}}}}
jobs:
  helper:
    container: {files: [helpers/]}
    repeat: {max-attempts: "\${{ inputs.bound }}", until: "true or false and false"}
    steps: [{argv: [true]}]
  review:
    uses: examples/pr-fix/workflows/task.yml
    for-each: {items: [], key: item.id, concurrency: "\${{ inputs.bound }}"}
  native:
    steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: Review, turns: "\${{ inputs.bound }}"}}]
`;
  const doc = new WorkflowDocument({ source });
  expect(doc.getSnapshot().diagnostics.filter(d => d.severity === 'error')).toEqual([]);
  doc.setField(['jobs', 'helper', 'name'], 'Observe');
  const edited = doc.getSnapshot().source;
  expect(edited.match(/\$\{\{ inputs.bound \}\}/g)).toHaveLength(3);
  expect(doc.getSnapshot().workflow!.jobs.helper.container).toEqual({ files: ['helpers/'] });
  doc.undo(); expect(doc.getSnapshot().source).toBe(source);
  doc.redo(); expect(doc.getSnapshot().source).toBe(edited);
});

it('finds dependencies across both logical operators and preserves quoted operands', () => {
  const result = references("needs.first.outputs.done or needs.second.outputs.done and 'needs.quoted.outputs.x or text'", true);
  expect(result.map(ref => ref.id)).toEqual(['first', 'second']);
});

it.each(['prefix ${{ inputs.bound }}', '8'])('rejects non-expression string numeric bound %s', bound => {
  const source = JSON.stringify({ jobs: { loop: { uses: 'workflows/child.yml', repeat: { 'max-attempts': bound, until: 'true' } } } });
  expect(parseWorkflow(source).diagnostics.some(d => d.severity === 'error')).toBe(true);
});
