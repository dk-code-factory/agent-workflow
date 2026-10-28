import { expect, test } from '@playwright/test';
import { open, snapshot, field, step } from './harness';

test('repair expression after deleting its producer', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: collect, argv: [echo]}, {id: analyze, uses: agent/claude, with: {prompt: "${{ steps.collect.outputs.stdout }}"}}]}}');
  await step(page, 'collect direct command'); await ui.getByRole('button', { name: 'Delete step', exact: true }).click();
  await expect(ui.getByRole('alertdialog')).toContainText('jobs.build.steps.1.with.prompt');
  await ui.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'expression-reference')).toBe(true);
  await expect(ui.locator('.aw-diagnostics')).toContainText('Restore its producer');
  await ui.locator('.aw-diagnostics').getByRole('button', { name: /Unknown steps reference/ }).click();
  await expect(ui.getByLabel('Prompt', { exact: true })).toBeFocused();
  await field(page, 'Prompt', 'Review without command output');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'expression-reference')).toEqual([]);
});

test('missing dependency guidance requires an explicit edge repair', async ({ page }) => {
  const ui = await open(page, 'jobs: {producer: {outputs: {text: hello}, steps: [{argv: [echo]}]}, consumer: {steps: [{uses: agent/claude, with: {prompt: "${{ needs.producer.outputs.text }} ${{ needs.producer.outputs.text }}"}}]}}');
  await ui.locator('.aw-diagnostics summary').click();
  const warning = ui.locator('.aw-diagnostics').getByRole('button', { name: /missing from Dependencies/ });
  await expect(warning).toHaveCount(1);
  await expect(warning).toContainText('explicitly add the producer');
  const before = await snapshot(page);
  await warning.focus(); await warning.press('Enter');
  await expect(ui.getByRole('group', { name: 'Dependencies' }).getByLabel('producer')).toBeFocused();
  await expect(ui.getByRole('group', { name: 'Dependencies' }).getByLabel('producer')).not.toBeChecked();
  expect(await snapshot(page)).toEqual(before);
  await ui.getByRole('group', { name: 'Dependencies' }).getByLabel('producer').check();
  await expect(warning).toHaveCount(0);
  expect((await snapshot(page)).workflow.jobs.consumer.needs).toEqual(['producer']);
});

test('viewer dependency diagnostics focus read-only Dependencies', async ({ page }) => {
  await page.goto('/test/browser.html?viewer');
  const ui = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {producer: {outputs: {text: hello}, steps: [{argv: [echo]}]}, consumer: {steps: [{uses: agent/claude, with: {prompt: "${{ needs.producer.outputs.text }}"}}]}}' }));
  await ui.locator('.aw-diagnostics summary').click();
  const before = await snapshot(page);
  const warning = ui.locator('.aw-diagnostics').getByRole('button', { name: /missing from Dependencies/ });
  await warning.click();
  await expect(ui.getByRole('group', { name: 'Dependencies' }).locator('..')).toBeFocused();
  await expect(ui.getByRole('group', { name: 'Dependencies' }).getByLabel('producer')).toBeDisabled();
  await warning.focus(); await warning.press('Enter');
  await expect(ui.getByRole('group', { name: 'Dependencies' }).locator('..')).toBeFocused();
  expect(await snapshot(page)).toEqual(before);
});
