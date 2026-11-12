import { expect, test } from '@playwright/test';
import { parse } from 'yaml';
import { open, snapshot, field, job, downloadText } from './harness';

test('retain job helper paths and replace a Docker sibling bundle with reviewed conversion and undo', async ({ page }) => {
  const ui = await open(page, `jobs: {build: {container: ubuntu:24.04, steps: [{id: sibling, uses: 'docker://ubuntu:24.04'}]}}`);
  await job(page);
  await field(page, 'Job helper files (YAML)', '[helpers/, config.json]');
  expect((await snapshot(page)).workflow.jobs.build.container).toEqual({ image: 'ubuntu:24.04', files: ['helpers/', 'config.json'] });
  await field(page, 'Job container image', 'ubuntu:22.04');
  expect((await snapshot(page)).workflow.jobs.build.container.files).toEqual(['helpers/', 'config.json']);
  await ui.getByRole('button', { name: '1. sibling', exact: true }).click();
  await field(page, 'Docker helper files (YAML)', '[alternate/]');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toEqual(['alternate/']);
  const exported = await downloadText(page); expect(exported).toContain('helpers/'); expect(exported).toContain('alternate/');
  await field(page, 'Docker helper files (YAML)', '[]');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toEqual([]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toEqual(['alternate/']);
  page.once('dialog', async dialog => { expect(dialog.message()).toContain('files'); await dialog.dismiss(); });
  await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toEqual(['alternate/']);
  page.once('dialog', dialog => dialog.accept());
  await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toBeUndefined();
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toEqual(['alternate/']);
});

test('repair helper path traversal and misplaced ordinary-step files', async ({ page }) => {
  const ui = await open(page, `jobs: {build: {container: {image: ubuntu, files: ['../escape']}, steps: [{argv: [echo, hello], files: [helpers/]}]}}`);
  await expect(ui.locator('.aw-diagnostics')).toContainText('static relative paths without traversal');
  await expect(ui.locator('.aw-diagnostics')).toContainText('Step files require a Docker step');
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(`jobs: {build: {container: {image: ubuntu, files: [helpers/]}, steps: [{argv: [echo, hello]}]}}`);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect((await snapshot(page)).workflow.jobs.build.container.files).toEqual(['helpers/']);
  expect(parse(await downloadText(page)).jobs.build.container.files).toEqual(['helpers/']);
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(`jobs: {build: {container: {image: ubuntu, files: []}, steps: [{argv: [echo, hello]}]}}`);
  await expect.poll(async () => (await snapshot(page)).workflow.jobs.build.container.files).toEqual([]);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect(parse(await downloadText(page)).jobs.build.container.files).toEqual([]);
});

test('visual edits preserve inherited null bundles and runner scalar filenames through export', async ({ page }) => {
  await open(page, `jobs: {build: {container: {image: ubuntu, files: [123, true, 01]}, steps: [{id: inherited, uses: docker://ubuntu, files: null}, {id: omitted, uses: docker://ubuntu}, {id: empty, uses: docker://ubuntu, files: []}, {id: replaced, uses: docker://ubuntu, files: [FALSE, 0xA]}]}}`);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await job(page);
  await field(page, 'Name', 'Keep helper declarations');
  const updated = (await snapshot(page)).workflow.jobs.build;
  expect(updated.name).toBe('Keep helper declarations');
  expect(updated.container.files).toEqual(['123', 'true', '01']);
  expect(updated.steps.map((step: any) => step.files)).toEqual([null, undefined, [], ['FALSE', '0xA']]);
  const exported = parse(await downloadText(page)).jobs.build;
  expect(exported.container.files).toEqual(['123', 'true', '01']);
  expect(exported.steps.map((step: any) => step.files)).toEqual([null, undefined, [], ['FALSE', '0xA']]);
});

test('helper-file controls preserve entered scalar filenames and inheritance through export', async ({ page }) => {
  const ui = await open(page, `jobs: {build: {container: ubuntu, steps: [{id: sibling, uses: docker://ubuntu}]}}`);
  const spelling = ['01', 'FALSE', '0xA', '1.0', '010'];
  await job(page);
  await field(page, 'Job helper files (YAML)', '[01, FALSE, 0xA, 1.0, !!int 010]');
  expect((await snapshot(page)).workflow.jobs.build.container.files).toEqual(spelling);
  await ui.getByRole('button', { name: '1. sibling', exact: true }).click();
  await field(page, 'Docker helper files (YAML)', '[01, FALSE, 0xA, 1.0, !!int 010]');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toEqual(spelling);
  let exported = parse(await downloadText(page)).jobs.build;
  expect(exported.container.files).toEqual(spelling);
  expect(exported.steps[0].files).toEqual(spelling);
  await field(page, 'Docker helper files (YAML)', 'null');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toBeNull();
  expect(parse(await downloadText(page)).jobs.build.steps[0].files).toBeNull();
  await field(page, 'Docker helper files (YAML)', '[]');
  expect((await snapshot(page)).workflow.jobs.build.steps[0].files).toEqual([]);
  await field(page, 'Docker helper files (YAML)', '');
  exported = parse(await downloadText(page)).jobs.build;
  expect(Object.hasOwn(exported.steps[0], 'files')).toBe(false);
  expect(exported.container.files).toEqual(spelling);
});
