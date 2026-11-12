import { expect, test } from '@playwright/test';

test('pending deletion is invalidated by a host replacement', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await first.getByRole('button', { name: 'Delete job', exact: true }).click();
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {replacement: {steps: [{argv: [echo]}]}}' }));
  await expect(first.getByRole('alertdialog')).toHaveCount(0);
  await expect(first.getByRole('alert')).toContainText('workflow changed');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.replacement)).toBeTruthy();
});

test('deletion shows impact, confirms and removes structural dependencies', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await first.getByRole('button', { name: 'Delete job', exact: true }).click();
  await expect(first.getByRole('alertdialog')).toContainText('jobs.test.needs');
  const source = await page.evaluate(() => (window as any).first.getSnapshot().source);
  await first.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(source);
  await first.getByRole('button', { name: 'Delete job', exact: true }).click();
  await first.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  await expect(first.getByRole('button', { name: 'Inspect job build', exact: true })).toHaveCount(0);
  await expect(first.getByRole('button', { name: 'Inspect job test', exact: true })).toBeVisible();
});

test('step deletion reviews named-schema references before leaving repair diagnostics', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: `schemas: { Review: { type: string, enum: [ "\${{ steps.local.outputs.text }}" ] } }
jobs: { review: { steps: [ { id: local, argv: [ echo, local ] }, { with: { output: { schema: Review }, agent: claude.toml, prompt: Review }, uses: agent/run } ] } }
` }));
  await first.getByRole('button', { name: 'local direct command', exact: true }).click();
  await first.getByRole('button', { name: 'Delete step', exact: true }).click();
  await expect(first.getByRole('alertdialog')).toContainText('schemas.Review.enum.0');
  const original = await page.evaluate(() => (window as any).first.getSnapshot().source);
  await first.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(original);
  await first.getByRole('button', { name: 'Delete step', exact: true }).click();
  await first.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.some((d: any) => d.code === 'expression-reference' && d.path.join('.') === 'schemas.Review.enum.0'))).toBe(true);
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(original);
});
