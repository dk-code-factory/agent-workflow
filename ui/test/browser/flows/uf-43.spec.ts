import { expect, test } from '@playwright/test';
import { open, snapshot, field, step } from './harness';

test('repair expression after deleting its producer', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: collect, argv: [echo]}, {id: analyze, uses: agent/claude, with: {prompt: "${{ steps.collect.outputs.stdout }}"}}]}}');
  await step(page, 'collect direct command'); await ui.getByRole('button', { name: 'Delete step', exact: true }).click();
  await expect(ui.getByRole('alertdialog')).toContainText('jobs.build.steps.1.with.prompt');
  await ui.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'expression-reference')).toBe(true);
  await step(page, 'analyze agent/claude'); await field(page, 'Prompt', 'Review without command output');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'expression-reference')).toEqual([]);
});
