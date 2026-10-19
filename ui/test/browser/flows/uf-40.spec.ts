import { expect, test } from '@playwright/test';
import { open, snapshot, step } from './harness';

test('insert preceding command output and review a reordered reference', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: collect, argv: [echo]}, {id: analyze, uses: agent/claude, with: {prompt: Summarize}}]}}');
  await step(page, 'analyze agent/claude');
  await ui.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('steps.collect.outputs.stdout');
  expect((await snapshot(page)).workflow.jobs.build.steps[1].with.prompt).toContain('${{ steps.collect.outputs.stdout }}');
  await expect(ui.getByLabel('Insert expression into Prompt', { exact: true }).locator('option[value="steps.analyze.outputs.stdout"]')).toHaveCount(0);
  await ui.getByRole('button', { name: 'Move up', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps.map((s: any) => s.id)).toEqual(['analyze', 'collect']);
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.prompt).toContain('${{ steps.collect.outputs.stdout }}');
  await expect(ui.getByLabel('Insert expression into Prompt', { exact: true }).locator('option[value="steps.collect.outputs.stdout"]')).toHaveCount(0);
});
