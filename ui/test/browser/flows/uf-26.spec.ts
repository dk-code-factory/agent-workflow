import { expect, test } from '@playwright/test';

test('step reordering and duplication preserve IDs and references through UI undo', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {build: {outputs: {result: "${{ steps.step_1.outputs.stdout }}"}, steps: [{argv: [echo, first]}, {argv: [echo, second]}]}}' }));
  await first.getByRole('button', { name: 'step_1 direct command', exact: true }).click();
  await first.getByRole('button', { name: 'Move down', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps.map((s: any) => s.id))).toEqual(['step_2', 'step_1']);
  await first.getByRole('button', { name: 'Duplicate step', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps.map((s: any) => s.id))).toEqual(['step_2', 'step_1', 'step_1_copy']);
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps.map((s: any) => s.id))).toEqual([undefined, undefined]);
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.filter((d: any) => d.severity === 'error'))).toEqual([]);
});
