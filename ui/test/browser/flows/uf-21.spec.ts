import { expect, test } from '@playwright/test';
import { open, snapshot, step } from './harness';

test('changing step type resets settings and undo restores them', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: run, argv: [echo, before], timeout: 2m, working-directory: /work}]}}');
  await step(page, 'run direct command');
  await ui.getByLabel('Step type', { exact: true }).selectOption('agent/claude');
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual({ id: 'run', uses: 'agent/claude', with: { prompt: '' } });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toMatchObject({ argv: ['echo', 'before'], timeout: '2m', 'working-directory': '/work' });
});
