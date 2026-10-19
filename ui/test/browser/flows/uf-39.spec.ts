import { expect, test } from '@playwright/test';
import { open, snapshot, field, job } from './harness';

test('declare producer output and insert it in dependent prompt', async ({ page }) => {
  const ui = await open(page, 'jobs: {review: {steps: [{id: analyze, uses: agent/claude, with: {prompt: Review}}]}, summarize: {steps: [{id: summary, uses: agent/claude, with: {prompt: Summarize}}]}}');
  await job(page, 'review'); await ui.getByText('Job outputs', { exact: true }).click();
  await ui.getByRole('button', { name: 'Add output', exact: true }).click();
  await field(page, 'Identifier: output', 'report'); await field(page, 'Output value: report', '${{ steps.analyze.outputs.text }}');
  await job(page, 'summarize'); await ui.getByRole('checkbox', { name: 'review', exact: true }).check();
  await ui.getByRole('button', { name: '1. summary', exact: true }).click();
  await ui.getByLabel('Insert expression into Prompt', { exact: true }).selectOption('needs.review.outputs.report');
  const jobs = (await snapshot(page)).workflow.jobs;
  expect(jobs.review.outputs.report).toBe('${{ steps.analyze.outputs.text }}');
  expect(jobs.summarize.needs).toEqual(['review']);
  expect(jobs.summarize.steps[0].with.prompt).toContain('${{ needs.review.outputs.report }}');
});
