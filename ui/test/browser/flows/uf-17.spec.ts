import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('configure direct command argv working directory and timeout', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Direct command', exact: true }).click();
  await field(page, 'Executable', 'echo');
  await field(page, 'Argument 1', 'reviewed');
  await field(page, 'Working directory', '/workspace'); await field(page, 'Timeout', '2m');
  expect((await snapshot(page)).workflow.jobs.build.steps[1]).toMatchObject({ argv: ['echo', 'reviewed'], 'working-directory': '/workspace', timeout: '2m' });
  await expect(ui.getByRole('button', { name: 'step direct command', exact: true })).toBeVisible();
});

test('argument rows preserve boundaries through keyboard editing, reordering and advanced YAML', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Direct command', exact: true }).click();
  await page.locator('#a').evaluate(element => { (element as HTMLElement).style.width = '500px'; });
  await field(page, 'Executable', '/path with spaces/tool');
  await field(page, 'Argument 1', 'one "quoted" argument');
  await ui.getByRole('button', { name: 'Add argument', exact: true }).focus();
  await page.keyboard.press('Enter');
  await expect(ui.getByLabel('Argument 2', { exact: true })).toHaveValue('');
  await ui.getByRole('button', { name: 'Move argument 2 up', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[1].argv).toEqual(['/path with spaces/tool', '', 'one "quoted" argument']);
  await ui.getByRole('button', { name: 'Remove argument 2', exact: true }).click();
  await ui.getByText('Advanced command YAML', { exact: true }).click();
  await field(page, 'Arguments (YAML array)', '[echo, "a b", "", "\\\"quote\\\""]');
  expect((await snapshot(page)).workflow.jobs.build.steps[1].argv).toEqual(['echo', 'a b', '', '"quote"']);
  await expect(ui.getByLabel('Argument 3', { exact: true })).toHaveValue('"quote"');
});

test('rejected executable drafts block Save until discarded and viewer controls stay disabled', async ({ page }) => {
  const ui = await open(page); await job(page);
  await page.evaluate(() => (window as any).first.update({ onSave: () => { (window as any).saves = ((window as any).saves ?? 0) + 1; } }));
  await ui.getByRole('button', { name: 'Direct command', exact: true }).click();
  await field(page, 'Executable', '');
  await expect(ui.getByLabel('Executable', { exact: true })).toHaveAttribute('aria-invalid', 'true');
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  expect(await page.evaluate(() => (window as any).saves ?? 0)).toBe(0);
  await expect(ui.getByLabel('Executable', { exact: true })).toHaveValue('');
  await ui.getByLabel('Executable', { exact: true }).press('Escape');
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  expect(await page.evaluate(() => (window as any).saves)).toBe(1);
  await page.goto('/test/browser.html?viewer');
  await job(page, 'test');
  await ui.getByRole('button', { name: '1. step_1', exact: true }).click();
  await expect(ui.getByLabel('Executable', { exact: true })).toBeDisabled();
  await expect(ui.getByRole('button', { name: 'Add argument', exact: true })).toBeDisabled();
  await expect(ui.getByRole('button', { name: 'Use job result in Argument 1', exact: true })).toBeDisabled();
});
