import { expect, test } from '@playwright/test';

test('empty command fields do not disguise an imported agent step', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {build: {steps: [{id: review, argv: null, run: null, shell: null, uses: agent/claude, with: {prompt: Review}}]}}' }));
  await first.getByRole('button', { name: 'review agent/claude', exact: true }).click();
  await expect(first.getByLabel('Step type', { exact: true })).toHaveValue('agent/claude');
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Review');
  await expect(first.getByLabel('Command', { exact: true })).toHaveCount(0);
});


test('configure agent prompt optional settings and output contract', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await ui.getByRole('button', { name: 'greet', exact: false }).first().click();
  for (const [label, value] of [['Prompt', 'Review changes'], ['Model', 'review-model'], ['Workspace', '/work']]) {
    await ui.getByLabel(label, { exact: true }).fill(value); await ui.getByLabel(label, { exact: true }).press('Tab');
  }
  await ui.getByText('Optional message', { exact: true }).click();
  await ui.getByLabel('Message', { exact: true }).fill('Context'); await ui.getByLabel('Message', { exact: true }).press('Tab');
  await ui.getByText('Structured output', { exact: true }).click();
  await ui.getByLabel('Structured output contract (YAML)', { exact: true }).fill('type: object');
  await ui.getByLabel('Structured output contract (YAML)', { exact: true }).press('Tab');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with)).toEqual({ prompt: 'Review changes', model: 'review-model', workspace: '/work', message: 'Context', output: { type: 'object' } });
});
