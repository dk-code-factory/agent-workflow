import { expect, test } from '@playwright/test';

test('shared YAML undo keeps the cursor at the edited text', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = first.getByRole('textbox', { name: 'Workflow YAML' });
  await yaml.fill('name: One\njobs: {build: {steps: [{argv: [echo]}]}}');
  // Finish the replacement history group, then type in the middle of the file.
  await first.getByRole('button', { name: 'Visual', exact: true }).click();
  await first.getByRole('button', { name: 'Workflow properties', exact: true }).click();
  await first.getByLabel('Name', { exact: true }).fill('Two'); await page.keyboard.press('Enter');
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  await yaml.press('ControlOrMeta+Home'); await yaml.press('ArrowRight'); await yaml.press('ArrowRight'); await yaml.press('ArrowRight'); await yaml.press('ArrowRight'); await yaml.press('ArrowRight'); await yaml.press('ArrowRight');
  await page.keyboard.type('X'); await yaml.press('ControlOrMeta+z'); await page.keyboard.type('Z');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.name)).toBe('ZTwo');
});


test('form and YAML edits share toolbar undo and redo', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await ui.getByLabel('Name', { exact: true }).fill('Form edit');
  await ui.getByLabel('Name', { exact: true }).press('Enter');
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = ui.getByRole('textbox', { name: 'Workflow YAML' });
  const source = await page.evaluate(() => (window as any).first.getSnapshot().source);
  await yaml.fill(source.replace('Form edit', 'Source edit'));
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.name)).toBe('Form edit');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.name)).toBe('Browser workflow');
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.name)).toBe('Source edit');
});
