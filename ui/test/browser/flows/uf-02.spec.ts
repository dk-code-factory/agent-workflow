import { expect, test } from '@playwright/test';
import { open, snapshot, job } from './harness';

test('switch views and reveal the selected definition', async ({ page }) => {
  const ui = await open(page); const before = (await snapshot(page)).source;
  await job(page);
  for (const mode of ['Visual', 'YAML', 'Split']) {
    await ui.getByRole('button', { name: mode, exact: true }).click();
    await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toHaveCount(mode === 'Visual' ? 0 : 1);
    await expect(ui.locator('.aw-graph')).toHaveCount(mode === 'YAML' ? 0 : 1);
  }
  await expect(ui.locator('.cm-line').filter({ hasText: /^  build:$/ })).toBeInViewport();
  expect((await snapshot(page)).source).toBe(before);
});
