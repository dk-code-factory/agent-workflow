import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, step, downloadText } from './harness';

test('configure job and step conditions and error handling', async ({ page }) => {
  const ui = await open(page); await job(page);
  await field(page, 'Condition', 'true'); await ui.getByRole('checkbox', { name: 'Continue on error', exact: true }).check();
  await step(page); await field(page, 'Condition', 'false');
  await ui.getByRole('checkbox', { name: 'Continue on error', exact: true }).check();
  const build = (await snapshot(page)).workflow.jobs.build;
  expect(build).toMatchObject({ if: 'true', 'continue-on-error': true, steps: [{ if: 'false', 'continue-on-error': true }] });
  await expect(ui.getByRole('article', { name: 'Job build', exact: true })).toContainText('continue on error');
});

test('YAML completion offers redacted context inside a nested handler', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  const yaml = ui.getByRole('textbox', { name: 'Workflow YAML' });
  await yaml.fill('jobs:\n  build:\n    steps:\n      - agent: claude\n        prompt: Work\n        onError:\n          - agent: codex\n            prompt: |\n              ${{ failed.');
  await yaml.press('Control+Space');
  await expect(ui.getByRole('option', { name: 'failed.prompt', exact: true })).toBeVisible();
  await expect(ui.getByRole('option', { name: 'failed.credentials', exact: true })).toHaveCount(0);
  await ui.getByRole('option', { name: 'failed.prompt', exact: true }).click();
  expect((await snapshot(page)).source).toContain('${{ failed.prompt');
});

test('author bounded fallback, reporting and finalization without losing nested handlers', async ({ page }) => {
  const ui = await open(page); await step(page);
  await ui.getByText('Failure handlers and finalization', { exact: true }).click();
  await field(page, 'Handler timeout', '45s');
  await field(page, 'Fallback steps (YAML)', '- id: backup\n  agent: codex\n  prompt: "${{ failed.prompt }}"\n  finally:\n    - argv: [echo, cleanup-backup]');
  await field(page, 'Failure reporting steps (YAML)', '- argv: [echo, "${{ error.class }}"]');
  await field(page, 'Finalization steps (YAML)', '- argv: [echo, cleanup]');
  const configured = (await snapshot(page)).workflow.jobs.build.steps[0];
  expect(configured).toMatchObject({ handlerTimeout: '45s', onError: [{ id: 'backup', agent: 'codex', prompt: '${{ failed.prompt }}', finally: [{ argv: ['echo', 'cleanup-backup'] }] }], onFailure: [{ argv: ['echo', '${{ error.class }}'] }], finally: [{ argv: ['echo', 'cleanup'] }] });
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await field(page, 'Prompt', 'Revised original task');
  const exported = await downloadText(page);
  expect(exported).toContain('cleanup-backup'); expect(exported).toContain('failed.prompt');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0]).toEqual(configured);
  await field(page, 'Handler timeout', '6m');
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'handler-timeout')).toBe(true);
});

test('renaming an outer step preserves named handler schemas and sibling shadowing', async ({ page }) => {
  const ui = await open(page, `schemas:
  Shared: {type: string, enum: ['\${{ steps.earlier.text }}']}
jobs:
  build:
    steps:
      - {id: earlier, agent: claude, prompt: Prepare}
      - agent: claude
        prompt: Work
        onError:
          - {agent: codex, prompt: Inherited, with: {output: {schema: Shared}}}
          - {id: earlier, agent: codex, prompt: Local}
          - {agent: claude, prompt: Shadowed, with: {output: {schema: Shared}}}
        finally:
          - {agent: claude, prompt: Finalize, with: {output: {schema: Shared}}}
`);
  await job(page); await step(page, '1. earlier');
  await field(page, 'Step ID', 'prepared');
  const edited = await snapshot(page);
  expect(edited.workflow.schemas.Shared.enum).toEqual(['${{ steps.earlier.text }}']);
  expect(edited.workflow.schemas.Shared_build.enum).toEqual(['${{ steps.prepared.text }}']);
  expect(edited.workflow.jobs.build.steps[1].onError[0].with.output.schema).toBe('Shared_build');
  expect(edited.workflow.jobs.build.steps[1].onError[2].with.output.schema).toBe('Shared');
  expect(edited.workflow.jobs.build.steps[1].finally[0].with.output.schema).toBe('Shared_build');
  expect(edited.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect(await downloadText(page)).toContain('steps.prepared.text');
  await ui.getByRole('button', { name: 'Delete step', exact: true }).click();
  const impact = ui.getByRole('alertdialog', { name: 'Delete step?' });
  await expect(impact).toContainText('schemas.Shared_build.enum.0');
  await impact.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect((await snapshot(page)).source).toBe(edited.source);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.schemas.Shared_build).toBeUndefined();
});

test('renaming an outer step rejects capture by the new ID in a handler scope', async ({ page }) => {
  const ui = await open(page, `schemas:
  Shared: {type: string, enum: ['\${{ steps.earlier.text }}']}
jobs:
  build:
    steps:
      - {id: earlier, agent: claude, prompt: Prepare}
      - agent: claude
        prompt: Work
        finally:
          - {id: prepared, agent: codex, prompt: Local}
          - {agent: claude, prompt: '\${{ steps.earlier.text }}', with: {output: {schema: Shared}}}
`);
  const original = await snapshot(page);
  await job(page); await step(page, '1. earlier');
  const id = ui.getByLabel('Step ID', { exact: true });
  await id.fill('prepared'); await id.press('Tab');
  await expect(id).toHaveAttribute('aria-invalid', 'true');
  await expect(ui.getByRole('alert')).toContainText('Choose a step ID that is not shadowed by a handler sibling');
  expect((await snapshot(page)).source).toBe(original.source);
  await id.fill('ready'); await id.press('Tab');
  await expect(id).toHaveAttribute('aria-invalid', 'false');
  const edited = await snapshot(page);
  expect(edited.workflow.jobs.build.steps[1].finally[1].prompt).toBe('${{ steps.ready.text }}');
  expect(edited.workflow.schemas.Shared.enum).toEqual(['${{ steps.ready.text }}']);
  expect(edited.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect(await downloadText(page)).toContain('steps.ready.text');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(original.source);
});
