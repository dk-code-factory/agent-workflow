import { expect, test } from '@playwright/test';

for (const declaration of ['uses: agent/claude, with: {prompt: Review}', 'agent: claude, prompt: Review']) {
test(`empty command fields do not disguise an imported agent step: ${declaration}`, async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(declaration => (window as any).first.update({ source: `jobs: {build: {steps: [{id: review, argv: [], run: null, shell: null, timeout: null, working-directory: '', ${declaration}}]}}` }), declaration);
  await first.getByRole('button', { name: 'review agent/claude', exact: true }).click();
  await expect(first.getByLabel('Step type', { exact: true })).toHaveValue('agent/claude');
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Review');
  await expect(first.getByLabel('Command', { exact: true })).toHaveCount(0);
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.filter((d: any) => d.severity === 'error'))).toEqual([]);
});
}


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

test('inspect and edit concise agent prompts and roles without expanding their YAML form', async ({ page }) => {
  const source = `# concise task
jobs:
  build:
    steps:
      - id: review
        agent: claude
        prompt: |
          Review the change.
          Keep both lines.
        with:
          model: sonnet # retain option
          workspace: /work
`;
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await page.evaluate(source => (window as any).first.update({ source }), source);
  await ui.getByRole('button', { name: 'review agent/claude', exact: true }).click();
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('agent/claude');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Review the change.\nKeep both lines.\n');
  await ui.getByLabel('Prompt', { exact: true }).fill('Revised review.\nKeep multiline text.');
  await ui.getByLabel('Prompt', { exact: true }).press('Tab');
  await ui.getByLabel('Action target', { exact: true }).fill('agent/codex');
  await ui.getByLabel('Action target', { exact: true }).press('Tab');
  await expect(ui.getByRole('button', { name: 'review agent/codex', exact: true })).toBeVisible();
  await ui.getByLabel('Model', { exact: true }).fill('review-model');
  await ui.getByLabel('Model', { exact: true }).press('Tab');
  const state = await page.evaluate(() => (window as any).first.getSnapshot());
  expect(state.workflow.jobs.build.steps[0]).toEqual({ id: 'review', agent: 'codex', prompt: 'Revised review.\nKeep multiline text.', with: { model: 'review-model', workspace: '/work' } });
  expect(state.source).toContain('# concise task'); expect(state.source).toContain('# retain option');
  expect(state.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
});

test('quote a date entered in a concise prompt for runner admission', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {build: {steps: [{id: review, agent: claude, prompt: Review}]}}' }));
  await ui.getByRole('button', { name: 'review agent/claude', exact: true }).click();
  await ui.getByLabel('Prompt', { exact: true }).fill('2026-11-06');
  await ui.getByLabel('Prompt', { exact: true }).press('Tab');
  const state = await page.evaluate(() => (window as any).first.getSnapshot());
  expect(state.source).toContain('prompt: "2026-11-06"');
  expect(state.workflow.jobs.build.steps[0].prompt).toBe('2026-11-06');
  expect(state.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('2026-11-06');
});
