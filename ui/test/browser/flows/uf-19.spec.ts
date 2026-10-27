import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

const actions = [
  ['workspace/create', { Commit: 'HEAD' }],
  ['workspace/snapshot', { Commit: 'HEAD' }],
  ['workspace/integrate', { Path: '/work', 'Commits (YAML)': '[abc, def]' }],
  ['git/commit', { Path: '/work', Message: 'Reviewed' }],
  ['github/pr', { Path: '/work', Commit: 'HEAD', Title: 'Review', Body: 'Details' }],
] as const;
for (const [action, fields] of actions) test(`author ${action} definition`, async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByRole('button', { name: 'Add step', exact: true }).click();
  page.once('dialog', async dialog => { expect(dialog.message()).toContain('argv'); await dialog.accept(); });
  await ui.getByLabel('Step type', { exact: true }).selectOption(action);
  for (const [label, value] of Object.entries(fields)) await field(page, label, value);
  if (action === 'workspace/create') await ui.getByRole('checkbox', { name: 'Independent workspace', exact: true }).check();
  const step = (await snapshot(page)).workflow.jobs.build.steps[1];
  expect(step.uses).toBe(action);
  for (const [label, value] of Object.entries(fields)) expect(step.with[label === 'Commits (YAML)' ? 'commits' : label.toLowerCase()]).toEqual(label === 'Commits (YAML)' ? ['abc', 'def'] : value);
  if (action === 'workspace/create') expect(step.with.independent).toBe(true);
});
