import { expect, test } from '@playwright/test';
import { downloadText, field, open, snapshot } from './harness';

test('preserve local named output schemas while editing and exporting an agent contract', async ({ page }) => {
  const source = `schemas:\n  Review: # shared output\n    type: object\n    properties: {approved: {type: boolean}}\njobs:\n  build:\n    steps:\n      - id: review\n        uses: agent/claude\n        with:\n          prompt: Review\n          output: {format: json, schema: Review, repair-attempts: 0}\n`;
  const ui = await open(page, source);
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByRole('button', { name: 'review agent/claude', exact: true }).click();
  await field(page, 'Prompt', 'Review carefully');
  await expect(ui.getByLabel('Structured output contract (YAML)', { exact: true })).toBeVisible();
  await field(page, 'Structured output contract (YAML)', 'format: json\nschema: Missing\nrepair-attempts: 0');
  await expect(ui.locator('.aw-diagnostics')).toContainText('Unknown workflow-local schema "Missing"');
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'schema-reference')).toBe(true);
  await field(page, 'Structured output contract (YAML)', 'format: json\nschema: Review\nrepair-attempts: 0');
  await expect(ui.locator('.aw-diagnostics')).not.toContainText('Unknown workflow-local schema');
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const edited = await snapshot(page);
  expect(edited.workflow.schemas).toEqual({ Review: { type: 'object', properties: { approved: { type: 'boolean' } } } });
  expect(edited.workflow.jobs.build.steps[0].with.output).toEqual({ format: 'json', schema: 'Review', 'repair-attempts': 0 });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.output.schema).toBe('Missing');
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.output.schema).toBe('Review');
  const exported = await downloadText(page);
  expect(exported).toContain('Review:');
  expect(exported).toContain('# shared output');
  expect(exported).toContain('schema: Review');
  expect(exported).toContain('prompt: Review carefully');
});

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
