import { expect, test } from '@playwright/test';

test('Escape cancels invalid structured YAML and clears its error', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
  await editor.getByText('Advanced parameters', { exact: true }).click();
  const parameters = editor.getByLabel('Parameters (YAML)', { exact: true });
  const initial = await parameters.inputValue();
  await parameters.fill('[invalid'); await parameters.press('Tab');
  await expect(editor.getByRole('alert')).toBeVisible();
  await parameters.focus(); await parameters.press('Escape');
  await expect(parameters).toHaveValue(initial);
  await expect(editor.getByRole('alert')).toHaveCount(0);
});
