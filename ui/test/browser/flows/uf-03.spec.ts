import { expect, test } from '@playwright/test';
import { open } from './harness';

test('search canvas and palette and select a matching step', async ({ page }) => {
  const ui = await open(page);
  await ui.getByLabel('Search workflow', { exact: true }).fill('greet');
  await expect(ui.locator('.aw-dimmed')).toHaveCount(1);
  await ui.getByLabel('Search palette', { exact: true }).fill('build');
  await expect(ui.locator('.aw-palette').getByRole('button', { name: 'test', exact: true })).toHaveCount(0);
  await ui.locator('.aw-palette').getByRole('button', { name: 'build', exact: true }).click();
  await ui.getByRole('button', { name: '1. greet', exact: true }).click();
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Hello');
});
