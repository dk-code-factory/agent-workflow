import { expect, test } from '@playwright/test';

test('imported null form values can be edited without crashes', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'on: {workflow_call: {inputs: {task: null}}}\njobs: {build: {repeat: null, steps: null}}' }));
  await first.getByText('Callable inputs', { exact: true }).click();
  await first.getByRole('checkbox', { name: 'Required: task', exact: true }).check();
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await first.getByText('Repeat', { exact: true }).click();
  await first.getByLabel('Maximum attempts', { exact: true }).fill('3'); await page.keyboard.press('Tab');
  await first.getByLabel('Until', { exact: true }).fill('true'); await page.keyboard.press('Tab');
  await first.getByRole('button', { name: 'Add step', exact: true }).click();
  await expect(first.getByLabel('Step ID', { exact: true })).toHaveValue('step');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.filter((item: any) => item.severity === 'error'))).toEqual([]);
  expect(errors).toEqual([]);
});


test('remove configured repeat through its control', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByText('Repeat', { exact: true }).click();
  await ui.getByLabel('Maximum attempts', { exact: true }).fill('3'); await ui.getByLabel('Maximum attempts', { exact: true }).press('Tab');
  await ui.getByLabel('Until', { exact: true }).fill('true'); await ui.getByLabel('Until', { exact: true }).press('Tab');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.repeat)).toEqual({ 'max-attempts': 3, until: 'true' });
  await ui.getByRole('button', { name: 'Remove repeat', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.repeat)).toBeUndefined();
});
