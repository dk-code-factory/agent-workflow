import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('expose a named job output and edit its advanced mapping', async ({ page }) => {
  const ui = await open(page); await job(page);
  await ui.getByText('Job outputs', { exact: true }).click();
  await ui.getByRole('button', { name: 'Add output', exact: true }).click();
  await field(page, 'Identifier: output', 'report');
  await field(page, 'Output value: report', '${{ steps.greet.outputs.text }}');
  expect((await snapshot(page)).workflow.jobs.build.outputs.report).toBe('${{ steps.greet.outputs.text }}');
  await ui.getByText('Advanced output YAML', { exact: true }).click();
  await field(page, 'Job outputs (YAML)', 'result: done');
  expect((await snapshot(page)).workflow.jobs.build.outputs).toEqual({ result: 'done' });
});
