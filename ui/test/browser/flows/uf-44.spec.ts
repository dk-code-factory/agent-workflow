import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, step } from './harness';

test('correct unavailable catalog references while keeping parameters', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: greet, uses: agent/missing, with: {prompt: Keep}}]}, call: {uses: workflows/missing.yml}}');
  await page.evaluate(() => (window as any).first.update({ agents: [{ id: 'claude' }], workflows: [{ id: 'workflows/available.yml' }] }));
  expect((await snapshot(page)).diagnostics.length).toBeGreaterThan(0);
  await step(page, 'greet agent/missing'); await field(page, 'Action target', 'agent/claude');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.prompt).toBe('Keep');
  await job(page, 'call'); await field(page, 'Reusable workflow', 'workflows/available.yml');
  expect((await snapshot(page)).diagnostics.filter((d: any) => /unavailable|unknown-agent|unknown-workflow/.test(d.code))).toEqual([]);
  await expect(ui.locator('.aw-diagnostics')).not.toContainText('unavailable');
});
