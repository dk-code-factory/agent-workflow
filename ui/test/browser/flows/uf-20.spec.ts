import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('use host supplied action fields and renderer', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Custom Claude', exact: true }).click();
  await field(page, 'Custom model', 'host-model');
  await ui.getByRole('button', { name: 'Custom prompt', exact: true }).click();
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Custom value');
  expect((await snapshot(page)).workflow.jobs.build.steps[1].with).toEqual({ model: 'host-model', prompt: 'Custom value' });
});

test('edit a registered Go action through host fields and preserve custom parameters', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: capture, uses: action/capture, with: {repository: original, evidence: {path: retained.json}}}]}}');
  await page.evaluate(() => (window as any).first.update({ actions: [{ uses: 'action/capture', label: 'Capture evidence', fields: [{ key: 'repository', label: 'Repository binding' }] }] }));
  await job(page); await ui.getByRole('button', { name: '1. capture', exact: true }).click();
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('action/capture');
  await field(page, 'Repository binding', 'updated');
  await expect.poll(async () => (await snapshot(page)).workflow.jobs.build.steps[0].with.repository).toBe('updated');
  const current = await snapshot(page);
  expect(current.workflow.jobs.build.steps[0].with).toEqual({ repository: 'updated', evidence: { path: 'retained.json' } });
  expect(current.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect(current.diagnostics).toEqual(expect.arrayContaining([expect.objectContaining({ code: 'host-action', source: 'host', severity: 'info' })]));
});
