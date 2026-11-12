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
  await page.evaluate(() => (window as any).first.update({ source: `on: { workflow_call: { inputs: { task: { type: string } } } }
schemas: { Review: { type: string, enum: [ "\${{ inputs.task }}" ] } }
jobs: { build: { steps: [ { uses: agent/run, with: { prompt: "\${{ inputs.task }}", output: { schema: Review }, agent: claude.toml } } ] } }
` }));
  await first.getByText('Callable inputs', { exact: true }).click();
  await first.getByLabel('Identifier: task', { exact: true }).fill('request'); await page.keyboard.press('Enter');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.prompt)).toBe('${{ inputs.request }}');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.schemas.Review.enum)).toEqual(['${{ inputs.request }}']);
  await first.getByRole('button', { name: 'Delete request', exact: true }).click();
  await expect(first.getByRole('alertdialog')).toContainText('jobs.build.steps.0.with.prompt');
  await expect(first.getByRole('alertdialog')).toContainText('schemas.Review.enum.0');
  await first.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.some((d: any) => d.code === 'expression-reference'))).toBe(true);
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.some((d: any) => d.code === 'expression-reference' && d.path.join('.') === 'schemas.Review.enum.0'))).toBe(true);
});

test('step rename preserves each job context in a shared named contract', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: `schemas: { Review: { type: string, enum: [ "\${{ steps.local.outputs.text }}" ] } }
jobs:
  review:
    steps:
      - { id: local, argv: [ echo, local ] }
      - { uses: agent/run, with: { prompt: Review, output: { schema: Review }, agent: claude.toml } }
  second:
    steps:
      - { id: local, argv: [ echo, second ] }
      - { with: { output: { schema: Review }, agent: claude.toml, prompt: Review }, uses: agent/run }
` }));
  await first.getByRole('button', { name: 'Inspect job review', exact: true }).click();
  await first.getByRole('button', { name: 'local direct command', exact: true }).first().click();
  const id = first.getByLabel('Step ID', { exact: true });
  await id.fill('renamed'); await id.press('Enter');
  const result = await page.evaluate(() => (window as any).first.getSnapshot().workflow);
  expect(result.jobs.review.steps[0].id).toBe('renamed');
  expect(result.jobs.review.steps[1].with.output.schema).toBe('Review_review');
  expect(result.schemas.Review_review.enum).toEqual(['${{ steps.renamed.outputs.text }}']);
  expect(result.jobs.second.steps[1].with.output.schema).toBe('Review');
  expect(result.schemas.Review.enum).toEqual(['${{ steps.local.outputs.text }}']);
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.filter((d: any) => d.code === 'expression-reference'))).toEqual([]);
  await first.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.schemas.Review_review)).toBeUndefined();
  await first.getByRole('button', { name: 'Redo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow)).toEqual(result);
});

test('step renames update concise local and dependency results and deletion reviews consumers', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: `schemas: { Result: { type: string, enum: [ "\${{ needs.review.steps.analyze.text }}" ] } }
jobs: { review: { steps: [ { id: analyze, uses: agent/run, with: { agent: claude.toml, prompt: Review } }, { id: finish, uses: agent/run, with: { agent: claude.toml, prompt: "\${{ steps.analyze.text }}" } } ] }, summarize: { needs: review, steps: [ { with: { output: { schema: Result }, agent: claude.toml, prompt: "\${{ needs.review.steps.analyze.text }}" }, uses: agent/run } ] } }
` }));
  await editor.getByRole('button', { name: 'Inspect job review', exact: true }).click();
  await editor.getByRole('button', { name: '1. analyze', exact: true }).click();
  const id = editor.getByLabel('Step ID', { exact: true });
  await id.fill('inspect'); await id.press('Enter');
  const workflow = await page.evaluate(() => (window as any).first.getSnapshot().workflow);
  expect(workflow.jobs.review.steps[1].with.prompt).toBe('${{ steps.inspect.text }}');
  expect(workflow.jobs.summarize.steps[0].with.prompt).toBe('${{ needs.review.steps.inspect.text }}');
  expect(workflow.schemas.Result.enum).toEqual(['${{ needs.review.steps.inspect.text }}']);
  await editor.getByRole('button', { name: 'Delete step', exact: true }).click();
  await expect(editor.getByRole('alertdialog')).toContainText('jobs.summarize.steps.0.with.prompt');
  await expect(editor.getByRole('alertdialog')).toContainText('jobs.review.steps.1.with.prompt');
  await expect(editor.getByRole('alertdialog')).toContainText('schemas.Result.enum.0');
  await editor.getByRole('button', { name: 'Confirm delete', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.some((d: any) => d.message.includes('Unknown step "inspect"')))).toBe(true);
});

test('YAML next-line whitespace keeps legacy references in rename and deletion review', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  const source = String.raw`jobs: { review: { steps: [ { id: first, uses: agent/run, with: { agent: claude.toml, prompt: Review } }, { id: consume, uses: agent/run, with: { agent: claude.toml, prompt: "${'${{'} \u0085steps.first.outputs.text\u0085 }}" } } ] } }
`;
  await page.evaluate(source => (window as any).first.update({ source }), source);
  await editor.getByRole('button', { name: 'Inspect job review', exact: true }).click();
  await editor.getByRole('button', { name: '1. first', exact: true }).click();
  const id = editor.getByLabel('Step ID', { exact: true });
  await id.fill('renamed'); await id.press('Enter');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.review.steps[1].with.prompt)).toBe('${{ \u0085steps.renamed.outputs.text\u0085 }}');
  await editor.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(source);
  await editor.getByRole('button', { name: 'Redo', exact: true }).click();
  await editor.getByRole('button', { name: 'Delete step', exact: true }).click();
  await expect(editor.getByRole('alertdialog')).toContainText('jobs.review.steps.1.with.prompt');
});
