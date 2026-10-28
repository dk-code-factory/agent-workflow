import { expect, test } from '@playwright/test';
import { open, snapshot } from './harness';

test('paste source and retain unfamiliar fields while updating the graph', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'Split', exact: true }).click();
  const source = '# handwritten\nname: Pasted\ncustom: retained\njobs: {pasted: {steps: [{argv: [echo]}]}}';
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(source);
  await expect(ui.getByRole('button', { name: 'Inspect job pasted', exact: true })).toBeVisible();
  expect((await snapshot(page)).source).toBe(source);
  expect((await snapshot(page)).workflow.custom).toBe('retained');
});

test('author ordered iteration YAML and recover from invalid items and repeat conflicts', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'Split', exact: true }).click();
  const yaml = ui.getByRole('textbox', { name: 'Workflow YAML' });
  const source = `jobs:
  review:
    uses: workflows/reviewer.yml
    for-each: {items: '\${{ inputs.reviewers }}', key: item.id}
    with: {task: '\${{ item }}', ordinal: '\${{ iteration.number }}'}
`;
  await yaml.fill(source);
  await expect(ui.getByRole('button', { name: 'Inspect job review', exact: true })).toBeVisible();
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await yaml.fill(source.replace("'${{ inputs.reviewers }}'", 'invalid'));
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'for-each' && d.path.at(-1) === 'items')).toBe(true);
  await yaml.fill(source + '    repeat: {max-attempts: 2, until: true}\n');
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'for-each' && d.message.includes('repeat'))).toBe(true);
  await yaml.fill(source);
  expect((await snapshot(page)).workflow.jobs.review['for-each'].items).toBe('${{ inputs.reviewers }}');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
});

test('author a durable input request, correct validation errors and edit advanced parameters', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'Split', exact: true }).click();
  const yaml = ui.getByRole('textbox', { name: 'Workflow YAML' });
  const source = `jobs:
  decide:
    steps:
      - id: question
        uses: input/wait
        with: {prompt: Choose a policy, evidence-id: pinned-v1, payload: {choices: [a, b]}}
      - id: apply
        uses: agent/claude
        with: {prompt: '\${{ steps.question.outputs.answer }}'}
`;
  await yaml.fill(source);
  await expect(ui.getByRole('button', { name: 'Inspect job decide', exact: true })).toBeVisible();
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await yaml.fill(source.replace('evidence-id: pinned-v1, ', ''));
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'input-wait' && d.path.at(-1) === 'evidence-id')).toBe(true);
  await yaml.fill(source.replace('prompt: Choose a policy', "prompt: ''"));
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'input-wait' && d.path.at(-1) === 'prompt')).toBe(true);
  await yaml.fill(source);
  await ui.getByRole('button', { name: 'Inspect job decide', exact: true }).click();
  await ui.getByRole('button', { name: '1. question', exact: true }).click();
  await ui.getByText('Advanced parameters', { exact: true }).click();
  const parameters = ui.getByRole('textbox', { name: 'Parameters (YAML)', exact: true });
  await parameters.fill('prompt: Revised question\nevidence-id: pinned-v2\npayload: {choices: [a, b], approved: false}');
  await parameters.blur();
  await expect.poll(async () => (await snapshot(page)).workflow.jobs.decide.steps[0].with['evidence-id']).toBe('pinned-v2');
  const state = await snapshot(page);
  expect(state.workflow.jobs.decide.steps[0].with.payload).toEqual({ choices: ['a', 'b'], approved: false });
  expect(state.workflow.jobs.decide.steps[1].with.prompt).toBe('${{ steps.question.outputs.answer }}');
  expect(state.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
});
