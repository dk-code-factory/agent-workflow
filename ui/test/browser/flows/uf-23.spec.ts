import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

test('expression insertion replaces selection and keeps typing at the insertion point', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'on: {workflow_call: {inputs: {task: {type: string}}}}\njobs: {build: {steps: [{id: greet, uses: agent/claude, with: {prompt: "Before replace after"}}]}}' }));
  await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
  const prompt = editor.getByLabel('Prompt', { exact: true });
  await prompt.focus(); await prompt.press('Home');
  for (let index = 0; index < 7; index++) await prompt.press('ArrowRight');
  for (let index = 0; index < 7; index++) await prompt.press('Shift+ArrowRight');
  await editor.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('inputs.task');
  await expect(prompt).toBeFocused();
  await prompt.press('!'); await prompt.press('Tab');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toBe('Before ${{ inputs.task }}! after');
});

test('YAML expression completion follows the cursor and only offers preceding steps', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await first.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = first.getByRole('textbox', { name: 'Workflow YAML' });
  await yaml.fill('jobs:\n  build:\n    steps:\n      - id: earlier\n        argv: [echo]\n      - id: current\n        uses: agent/claude\n        with:\n          prompt: |\n            ${{ steps.');
  await yaml.press('Control+Space');
  await expect(first.getByRole('option', { name: 'steps.earlier.outputs.stdout', exact: true })).toBeVisible();
  await expect(first.getByRole('option', { name: 'steps.current.outputs.stdout', exact: true })).toHaveCount(0);
});

test('property expression hints are limited to fields the runner evaluates', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: (window as any).first.getSnapshot().source.replace('on: {manual: {}}', 'on: {manual: {}, workflow_call: {inputs: {task: {type: string}}}}') }));
  await expect(first.getByLabel('Name', { exact: true })).not.toHaveAttribute('list');
  await first.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await expect(first.getByLabel('Runner', { exact: true })).not.toHaveAttribute('list');
  await expect(first.getByLabel('Condition', { exact: true })).toHaveAttribute('list', /suggestions/);
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  await first.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('inputs.task');
  await expect(first.getByLabel('Prompt', { exact: true })).toContainText('${{ inputs.task }}');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toContain('${{ inputs.task }}');
  expect((await new AxeBuilder({ page }).include('#a').withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze()).violations).toEqual([]);
  await expect(first.getByLabel('Action target', { exact: true })).not.toHaveAttribute('list');
});
