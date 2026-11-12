import { expect, test } from '@playwright/test';
import { parse } from 'yaml';
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
  expect(text).toBe((await snapshot(page)).source);
  expect(parse(text).jobs.build.steps[1]).toMatchObject({ uses: 'docker://ubuntu:24.04', with: { entrypoint: '/bin/sh', args: '-c "echo separate" ""' } });
});

test('malformed Docker argument quotes and escapes diagnose and clear after repair', async ({ page }) => {
  const ui = await open(page, `jobs: {build: {steps: [{id: sibling, uses: 'docker://ubuntu:24.04'}]}}`);
  await job(page);
  await ui.getByRole('button', { name: '1. sibling', exact: true }).click();
  for (const args of ["'unfinished", '"unfinished', 'trailing\\']) {
    await field(page, 'Docker arguments', args);
    await expect(ui.locator('.aw-diagnostics')).toContainText('Docker arguments contain an unfinished quote or escape');
    await field(page, 'Docker arguments', `-c "echo repaired" ''`);
    expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  }
});

test('mapping container image edits preserve comments, anchors and quoted scalars', async ({ page }) => {
  const source = `jobs:
  build:
    container: &build_image # container note
      # Explain the image choice
      image: "ubuntu:24.04" # pinned release
    steps:
      - {id: shared, run: echo ready}
`;
  const ui = await open(page, source);
  await job(page);
  await field(page, 'Job container image', 'ubuntu:22.04');
  const edited = await snapshot(page);
  expect(edited.workflow.jobs.build.container.image).toBe('ubuntu:22.04');
  expect(edited.source).toContain('&build_image');
  expect(edited.source).toContain('# container note');
  expect(edited.source).toContain('# Explain the image choice');
  expect(edited.source).toContain('image: "ubuntu:22.04" # pinned release');
  expect(edited.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect(await downloadText(page)).toBe(edited.source);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(source);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(edited.source);
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
