import { expect, test } from '@playwright/test';
import { open, snapshot, field, step } from './harness';

test('fix browser errors and report host validation failure', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: bad, argv: []}]}}');
  await expect(ui.locator('.aw-diagnostics')).toContainText('error');
  await step(page, 'bad'); await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  await field(page, 'Arguments (YAML array)', '[echo, fixed]');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await page.evaluate(() => (window as any).first.update({ validate: async () => { throw new Error('Validator unavailable'); } }));
  await expect(ui.locator('.aw-diagnostics')).toContainText('Validator unavailable');
  await expect(ui.locator('.aw-diagnostics')).toContainText('host');
});

test('registered action syntax defers availability and schema validation to the host', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: capture, uses: action/invalid.name}]}}');
  expect((await snapshot(page)).diagnostics.some((d: any) => d.severity === 'error')).toBe(true);
  await step(page, 'capture');
  await field(page, 'Action target', 'action/capture');
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await expect(ui.locator('.aw-diagnostics')).toContainText('Registered action availability and typed schemas require host runtime validation');
});
