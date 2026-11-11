import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, downloadText } from './harness';

test('configure job and sibling Docker containers and export exact command arguments', async ({ page }) => {
  const ui = await open(page, `jobs: {build: {runs-on: local, container: 'ubuntu:24.04', steps: [{id: shared, run: 'echo ready'}, {id: sibling, uses: 'docker://ubuntu:24.04'}]}}`);
  await job(page);
  await field(page, 'Job container image', 'ubuntu:22.04');
  expect((await snapshot(page)).workflow.jobs.build.container).toBe('ubuntu:22.04');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.container).toBe('ubuntu:24.04');
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  await ui.getByRole('button', { name: '2. sibling', exact: true }).click();
  await field(page, 'Docker entrypoint', '/bin/sh');
  await field(page, 'Docker arguments', '-c "echo separate" ""');
  expect((await snapshot(page)).workflow.jobs.build.steps[1].with).toEqual({ entrypoint: '/bin/sh', args: '-c "echo separate" ""' });
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const text = await downloadText(page);
  expect(text).toContain('docker://ubuntu:24.04');
  expect(text).toContain('echo separate');
});

test('unsupported shared job constructs remain visible and can be repaired', async ({ page }) => {
  const source = `jobs: {build: {container: {image: 'ubuntu:24.04'}, steps: [{uses: input/wait, with: {prompt: Wait, evidence-id: review}}]}}`;
  const ui = await open(page, source);
  await expect(ui.locator('.aw-diagnostics')).toContainText('Shared containers support sequential commands');
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(`jobs: {build: {container: {image: 'ubuntu:24.04'}, steps: [{run: 'echo repaired'}]}}`);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect((await snapshot(page)).workflow.jobs.build.container.image).toBe('ubuntu:24.04');
});
