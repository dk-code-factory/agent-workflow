import { expect, test } from '@playwright/test';
import { open, snapshot, job } from './harness';

test('replace reusable call with starter steps and undo', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {uses: workflows/review.yml, with: {task: Review}, secrets: {token: secret}, outputs: {report: result}}}');
  await job(page); await ui.getByLabel('Job kind', { exact: true }).selectOption('steps');
  const build = (await snapshot(page)).workflow.jobs.build;
  expect(build.uses).toBeUndefined(); expect(build.with).toBeUndefined(); expect(build.secrets).toBeUndefined();
  expect(build.steps).toEqual([{ id: 'step_1', argv: ['echo', 'Hello'] }]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build).toMatchObject({ uses: 'workflows/review.yml', with: { task: 'Review' }, secrets: { token: 'secret' } });
});
