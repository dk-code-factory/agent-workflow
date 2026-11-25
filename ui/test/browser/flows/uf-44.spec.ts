import { expect, test } from '@playwright/test';
import { open, snapshot, field, job, step } from './harness';
import type { NativeRuntimeFixtureGlobals } from '../../native-runtime-react';

test('restore the native runtime catalog and correct a workflow reference', async ({ page }) => {
  const ui = await open(page, `jobs: { build: { steps: [ { id: greet, uses: agent/run, with: { prompt: Keep, agent: missing.yaml, input: { count: 7 }, turns: 2 } } ] }, call: { uses: workflows/missing.yml } }
`);
  await page.evaluate(() => (window as any).first.update({ agents: [{ id: 'claude' }], workflows: [{ id: 'workflows/available.yml' }] }));
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toHaveLength(1);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'workflow-catalog')).toHaveLength(1);
  await step(page, 'greet agent/run'); await field(page, 'Agent definition or reference', 'reviewer.yaml');
  const edited = await snapshot(page);
  expect(edited.workflow.jobs.build.steps[0]).toMatchObject({ uses: 'agent/run', with: { agent: 'reviewer.yaml', prompt: 'Keep', input: { count: 7 }, turns: 2 } });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with).toEqual({ agent: 'missing.yaml', prompt: 'Keep', input: { count: 7 }, turns: 2 });
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(edited.source);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toHaveLength(1);
  await page.evaluate(() => (window as any).first.update({ agentRuntime: 'claude' }));
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toEqual([]);
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'workflow-catalog')).toHaveLength(1);
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.prompt).toBe('Keep');
  await page.evaluate(() => (window as any).first.update({ agentRuntime: 'missing-runtime' }));
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toHaveLength(1);
  await page.evaluate(() => (window as any).first.update({ agentRuntime: undefined, agents: [{ id: 'codex' }] }));
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-catalog')).toEqual([]);
  await job(page, 'call'); await field(page, 'Reusable workflow', 'workflows/available.yml');
  expect((await snapshot(page)).diagnostics.filter((d: any) => /catalog|unavailable|unknown-agent|unknown-workflow/.test(d.code))).toEqual([]);
  await expect(ui.locator('.aw-diagnostics')).not.toContainText('unavailable');
});

for (const viewer of [false, true]) test(`React runtime-only props refresh ${viewer ? 'viewer' : 'editor'} catalog diagnostics`, async ({ page }) => {
  await page.goto(`/test/native-runtime-react.html${viewer ? '?viewer' : ''}`);
  const current = () => page.evaluate(() => (window as NativeRuntimeFixtureGlobals).nativeRuntimeSnapshot);
  const warnings = async () => (await current())?.diagnostics.filter(d => d.code === 'agent-catalog') ?? [];
  await expect.poll(warnings).toHaveLength(1);
  const before = (await current())!;
  await page.getByLabel('Host native runtime').selectOption('claude');
  await expect.poll(warnings).toEqual([]);
  const after = (await current())!;
  expect({ source: after.source, workflow: after.workflow, revision: after.revision, canUndo: after.canUndo, canRedo: after.canRedo, dirty: after.dirty }).toEqual({ source: before.source, workflow: before.workflow, revision: before.revision, canUndo: before.canUndo, canRedo: before.canRedo, dirty: before.dirty });
  await page.getByLabel('Host native runtime').selectOption('missing-runtime');
  await expect.poll(async () => (await warnings()).map(d => d.message).join('\n')).toContain('missing-runtime');
  await page.getByLabel('Host native runtime').selectOption('');
  await expect.poll(async () => (await warnings()).map(d => d.message).join('\n')).toContain('codex');
  expect((await current())!.source).toBe(before.source);
});
