import { expect, test } from '@playwright/test';

test('rejected identifier can be corrected and viewer fields remain immutable', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await editor.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  const id = editor.getByLabel('Job ID', { exact: true });
  await id.fill('test'); await id.press('Enter');
  await expect(editor.getByRole('alert')).toContainText('unique');
  await id.fill('compile'); await id.press('Enter');
  await expect(id).toHaveValue('compile');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.test.needs)).toEqual(['compile']);
  await page.goto('/test/browser.html?viewer');
  await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
  await expect(editor.getByLabel('Prompt', { exact: true })).toHaveAttribute('readonly', '');
  await expect(editor.getByRole('button', { name: 'Delete step', exact: true })).toBeDisabled();
  await expect(editor.getByRole('button', { name: 'Custom prompt', exact: true })).toBeDisabled();
});
test('named callable entries can be renamed and deleted with impact review', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'on: {workflow_call: {inputs: {task: {type: string}}}}\njobs: {build: {steps: [{uses: agent/claude, with: {prompt: "${{ inputs.task }}"}}]}}' }));
  await first.getByText('Callable inputs', { exact: true }).click();
  await first.getByLabel('Identifier: task', { exact: true }).fill('request'); await page.keyboard.press('Enter');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toBe('${{ inputs.request }}');
  await first.getByRole('button', { name: 'Delete request', exact: true }).click();
  await expect(first.getByRole('alertdialog')).toContainText('jobs.build.steps.0.with.prompt');
  await first.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.some((d: any) => d.code === 'expression-reference'))).toBe(true);
});
