import { expect, test } from '@playwright/test';
import { downloadText, field, open, snapshot } from './harness';

test('edit checklist budgets and script stages, repair errors and preserve history', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: implement, uses: agent/run, with: {agent: developer.yaml, prompt: Implement}}]}}');
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByRole('button', { name: '1. implement', exact: true }).click();
  const checklist = '{max-retries: 4, stages: [{prompt: Verify requirements, max-retries: 1}, {argv: [go, test, ./...], timeout: 5m}, {shell: sh, run: git diff --check, max-retries: 2}]}';
  await field(page, 'Checklist (YAML)', checklist);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const expected = (await snapshot(page)).workflow.jobs.build.steps[0].with.checklist;
  expect(expected['max-retries']).toBe(4);
  expect(expected.stages[1].argv).toEqual(['go', 'test', './...']);
  await field(page, 'Checklist (YAML)', checklist.replace('max-retries: 4', 'max-retries: -1'));
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'checklist')).toBe(true);
  await field(page, 'Checklist (YAML)', checklist);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.checklist['max-retries']).toBe(-1);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.checklist).toEqual(expected);
  expect(await downloadText(page)).toContain('max-retries: 4');
});

test('repair imported unquoted checklist date arguments in YAML', async ({ page }) => {
  const source = 'jobs: {build: {steps: [{uses: agent/run, with: {agent: developer.yaml, prompt: Work, checklist: {max-retries: 0, stages: [{argv: [echo, 2026-01-01]}]}}}]}}';
  const ui = await open(page, source);
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'checklist')).toBe(true);
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(source.replace('2026-01-01', "'2026-01-01'"));
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.checklist.stages[0].argv).toEqual(['echo', '2026-01-01']);
  expect(await downloadText(page)).toContain("'2026-01-01'");
});

test('import and export decoded checklist binary strings', async ({ page }) => {
  const source = 'jobs: {build: {steps: [{uses: agent/run, with: {agent: developer.yaml, prompt: Work, checklist: {max-retries: 0, stages: [!!binary VmVyaWZ5, !!binary 77u/, {argv: [echo, !!binary b2s=]}]}}}]}}';
  const ui = await open(page, source);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByRole('button', { name: '1. step_1', exact: true }).click();
  await field(page, 'Prompt', 'Work carefully');
  const checklist = (await snapshot(page)).workflow.jobs.build.steps[0].with.checklist;
  expect(checklist.stages).toEqual(['Verify', '\uFEFF', { argv: ['echo', 'ok'] }]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.checklist).toEqual(checklist);
  const exported = await downloadText(page);
  expect(exported).toContain('Verify');
  expect(exported).toContain('ok');
});

test('edit pinned agent definition selections and repair invalid catalogs', async ({ page }) => {
  const source = "on: {workflow_call: {inputs: {role: {type: string}}}}\njobs: {build: {steps: [{id: review, uses: agent/run, with: {agent: {select: '${{ inputs.role }}', definitions: {review: reviewer.yaml, fix: fixer.yaml}}, prompt: Review}}]}}";
  const ui = await open(page, source);
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByRole('button', { name: '1. review', exact: true }).click();
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const catalog = '{select: fix, definitions: {review: reviewer.yaml, fix: fixer.yaml}}';
  await field(page, 'Agent definition selection (YAML)', catalog);
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.agent.select).toBe('fix');
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.agent.select).toBe('${{ inputs.role }}');
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.agent.definitions).toEqual({ review: 'reviewer.yaml', fix: 'fixer.yaml' });
  expect(await downloadText(page)).toContain('fixer.yaml');
  for (const invalid of [catalog.replace('select: fix', 'select: unknown'), catalog.replace('fixer.yaml', "'${{ inputs.role }}'")]) {
    await field(page, 'Agent definition selection (YAML)', invalid);
    await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.code === 'agent-definition').length).toBe(1);
    await field(page, 'Agent definition selection (YAML)', catalog);
    await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  }
});

test('configure a bounded conversation and repair finisher membership', async ({ page }) => {
  const source = 'schemas: {Decision: {type: object, properties: {approved: {type: boolean}}}}\njobs: {solve: {steps: [{id: solution, conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, with: {output: {format: json, schema: Decision}}}]}}';
  const ui = await open(page, source);
  await ui.getByRole('button', { name: 'Inspect job solve', exact: true }).click();
  await ui.getByRole('button', { name: '1. solution', exact: true }).click();
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('conversation');
  await field(page, 'Participants (YAML)', '[builder, reviewer, editor]');
  await field(page, 'Starter', 'builder');
  await field(page, 'Finisher', 'outside');
  await expect(ui.locator('.aw-diagnostics')).toContainText('finishBy must name a declared participant');
  await field(page, 'Finisher', 'editor');
  await field(page, 'Maximum turns', '12');
  await field(page, 'Maximum messages', '24');
  await field(page, 'Timeout', '3m');
  await field(page, 'Prompt', 'Review and finish with a decision.');
  await expect(ui.locator('.aw-diagnostics')).not.toContainText('finishBy must name a declared participant');
  const step = (await snapshot(page)).workflow.jobs.solve.steps[0];
  expect(step).toMatchObject({ conversation: ['builder', 'reviewer', 'editor'], start: 'builder', finishBy: 'editor', maxTurns: 12, maxMessages: 24, timeout: '3m', prompt: 'Review and finish with a decision.', with: { output: { schema: 'Decision' } } });
  expect(await downloadText(page)).toContain('schema: Decision');
  page.once('dialog', async dialog => { expect(dialog.message()).toContain('conversation'); await dialog.dismiss(); });
  await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  expect((await snapshot(page)).workflow.jobs.solve.steps[0]).toEqual(step);
  page.once('dialog', async dialog => { expect(dialog.message()).toContain('finishBy'); await dialog.accept(); });
  await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  expect((await snapshot(page)).workflow.jobs.solve.steps[0]).toEqual({ id: 'solution', timeout: '3m', argv: ['echo', 'Hello'] });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.solve.steps[0]).toEqual(step);
});

test('reject imported fractional conversation budgets and repair them as integers', async ({ page }) => {
  for (const budget of ['maxTurns', 'maxMessages']) {
    const source = `jobs: {solve: {steps: [{conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, ${budget}: 1.9}]}}`;
    const ui = await open(page, source);
    await expect(ui.locator('.aw-diagnostics')).toContainText(budget);
    expect((await snapshot(page)).diagnostics).toContainEqual(expect.objectContaining({ severity: 'error', path: ['jobs', 'solve', 'steps', 0, budget] }));
    await ui.getByRole('button', { name: 'YAML', exact: true }).click();
    await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(source.replace('1.9', '2'));
    await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
    expect((await snapshot(page)).workflow.jobs.solve.steps[0][budget]).toBe(2);
  }
});

test('reject conversation-owned lifecycle hooks and repair the declaration in YAML', async ({ page }) => {
  const source = 'jobs: {solve: {steps: [{conversation: [builder, reviewer], finishBy: reviewer, prompt: Work, finally: [{argv: [echo, done]}]}]}}';
  const ui = await open(page, source);
  await expect(ui.locator('.aw-diagnostics')).toContainText('Conversations cannot own or appear inside lifecycle handlers');
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill(source.replace(', finally: [{argv: [echo, done]}]', ''));
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Visual', exact: true }).click();
  await ui.getByRole('button', { name: 'Inspect job solve', exact: true }).click();
  await ui.getByRole('button', { name: '1. step_1', exact: true }).click();
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('conversation');
  await expect(ui.getByText('Failure handlers and finalization', { exact: true })).toHaveCount(0);
});

test('preserve local named output schemas while editing and exporting an agent contract', async ({ page }) => {
  const source = `schemas:
  Review:
    # shared output
    type: object
    properties: { approved: { type: boolean } }
jobs:
  build:
    steps:
      - id: review
        uses: agent/run
        with:
          prompt: Review
          output: { format: json, schema: Review, repair-attempts: 0 }
          agent: claude.yaml
`;
  const ui = await open(page, source);
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByRole('button', { name: 'review agent/run', exact: true }).click();
  await field(page, 'Prompt', 'Review carefully');
  await expect(ui.getByLabel('Structured output contract (YAML)', { exact: true })).toBeVisible();
  await field(page, 'Structured output contract (YAML)', 'format: json\nschema: Missing\nrepair-attempts: 0');
  await expect(ui.locator('.aw-diagnostics')).toContainText('Unknown workflow-local schema "Missing"');
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'schema-reference')).toBe(true);
  await field(page, 'Structured output contract (YAML)', 'format: json\nschema: Review\nrepair-attempts: 0');
  await expect(ui.locator('.aw-diagnostics')).not.toContainText('Unknown workflow-local schema');
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  const edited = await snapshot(page);
  expect(edited.workflow.schemas).toEqual({ Review: { type: 'object', properties: { approved: { type: 'boolean' } } } });
  expect(edited.workflow.jobs.build.steps[0].with.output).toEqual({ format: 'json', schema: 'Review', 'repair-attempts': 0 });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.output.schema).toBe('Missing');
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.output.schema).toBe('Review');
  const exported = await downloadText(page);
  expect(exported).toContain('Review:');
  expect(exported).toContain('# shared output');
  expect(exported).toContain('schema: Review');
  expect(exported).toContain('prompt: Review carefully');
});

for (const declaration of ['uses: agent/run, with: {agent: claude.yaml, prompt: Review}', 'uses: agent/run, with: {agent: codex.yaml, prompt: Review}']) {
test(`empty command fields do not disguise an imported agent step: ${declaration}`, async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(declaration => (window as any).first.update({ source: `jobs: {build: {steps: [{id: review, argv: [], run: null, shell: null, timeout: null, working-directory: '', ${declaration}}]}}` }), declaration);
  await first.getByRole('button', { name: 'review agent/run', exact: true }).click();
  await expect(first.getByLabel('Step type', { exact: true })).toHaveValue('agent/run');
  await expect(first.getByLabel('Prompt', { exact: true })).toHaveValue('Review');
  await expect(first.getByLabel('Command', { exact: true })).toHaveCount(0);
  expect(await page.evaluate(() => (window as any).first.getSnapshot().diagnostics.filter((d: any) => d.severity === 'error'))).toEqual([]);
});
}


test('configure agent prompt optional settings and output contract', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await ui.getByRole('button', { name: 'greet', exact: false }).first().click();
  for (const [label, value] of [['Prompt', 'Review changes'], ['Model', 'review-model'], ['Workspace', '/work']]) {
    await ui.getByLabel(label, { exact: true }).fill(value); await ui.getByLabel(label, { exact: true }).press('Tab');
  }
  await ui.getByText('Optional message', { exact: true }).click();
  await ui.getByLabel('Message', { exact: true }).fill('Context'); await ui.getByLabel('Message', { exact: true }).press('Tab');
  await ui.getByText('Structured output', { exact: true }).click();
  await ui.getByLabel('Structured output contract (YAML)', { exact: true }).fill('type: object');
  await ui.getByLabel('Structured output contract (YAML)', { exact: true }).press('Tab');
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with)).toEqual({ agent:'claude.yaml', prompt: 'Review changes', model: 'review-model', workspace: '/work', message: 'Context', output: { type: 'object' } });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with)).toEqual({ agent:'claude.yaml', prompt: 'Review changes', model: 'review-model', workspace: '/work', message: 'Context' });
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect(await page.evaluate(() => (window as any).first.getSnapshot().workflow.jobs.build.steps[0].with.output)).toEqual({ type: 'object' });
});

test('inspect and edit concise agent prompts and roles without expanding their YAML form', async ({ page }) => {
  const source = `# concise task
jobs:
  build:
    steps:
      - id: review
        with:
          model: sonnet # retain option
          workspace: /work
          agent: claude.yaml
          prompt: |
            Review the change.
            Keep both lines.
        uses: agent/run
`;
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await page.evaluate(source => (window as any).first.update({ source }), source);
  await ui.getByRole('button', { name: 'review agent/run', exact: true }).click();
  await expect(ui.getByLabel('Step type', { exact: true })).toHaveValue('agent/run');
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('Review the change.\nKeep both lines.\n');
  await ui.getByLabel('Prompt', { exact: true }).fill('Revised review.\nKeep multiline text.');
  await ui.getByLabel('Prompt', { exact: true }).press('Tab');
  await ui.getByLabel('Agent definition or reference', { exact: true }).fill('codex.yaml');
  await ui.getByLabel('Agent definition or reference', { exact: true }).press('Tab');
  await ui.getByLabel('Action target', { exact: true }).fill('agent/run');
  await ui.getByLabel('Action target', { exact: true }).press('Tab');
  await expect(ui.getByRole('button', { name: 'review agent/run', exact: true })).toBeVisible();
  await ui.getByLabel('Model', { exact: true }).fill('review-model');
  await ui.getByLabel('Model', { exact: true }).press('Tab');
  const state = await page.evaluate(() => (window as any).first.getSnapshot());
  expect(state.workflow.jobs.build.steps[0]).toEqual({id: 'review', uses: 'agent/run', with: {agent: "codex.yaml", model: 'review-model', workspace: '/work', prompt: 'Revised review.\nKeep multiline text.'}});
  expect(state.source).toContain('# concise task'); expect(state.source).toContain('# retain option');
  expect(state.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
});

test('quote a date entered in a concise prompt for runner admission', async ({ page }) => {
  await page.goto('/test/browser.html'); const ui = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: `jobs: { build: { steps: [ { id: review, uses: agent/run, with: { agent: claude.yaml, prompt: Review } } ] } }
` }));
  await ui.getByRole('button', { name: 'review agent/run', exact: true }).click();
  await ui.getByLabel('Prompt', { exact: true }).fill('2026-11-06');
  await ui.getByLabel('Prompt', { exact: true }).press('Tab');
  const state = await page.evaluate(() => (window as any).first.getSnapshot());
  expect(state.source).toContain('prompt: "2026-11-06"');
  expect(state.workflow.jobs.build.steps[0].with.prompt).toBe('2026-11-06');
  expect(state.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await expect(ui.getByLabel('Prompt', { exact: true })).toHaveValue('2026-11-06');
});


test('configure a selected skill and bounded foreground child with recovery controls', async ({ page }) => {
  const ui = await open(page, 'on: {workflow_call: {inputs: {selected: {type: skill, required: true}}}}\njobs: {build: {steps: [{id: review, uses: agent/run, with: {agent: reviewer.yaml, prompt: Review}}]}}');
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByRole('button', { name: '1. review', exact: true }).click();
  const skill = '{input: selected, arguments: exact, resources: [reference.txt], evidence: {invocation: required}}';
  await field(page, 'Skill invocation (YAML)', skill);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'host-skill')).toBe(true);
  await field(page, 'Skill invocation (YAML)', skill.replace('arguments: exact', 'children: [{name: child}], arguments: exact'));
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'skill')).toBe(true);
  await field(page, 'Skill invocation (YAML)', skill);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.skill.children).toEqual([{ name: 'child' }]);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.skill.evidence).toEqual({ invocation: 'required' });
  const withChild = '{input: selected, arguments: exact, resources: [reference.txt], evidence: {invocation: required, children: [review]}, children: [{name: review, prompt: Review., description: Retained review, tools: [Read], resources: [reference.txt], model: inherit, max-turns: 2, max-calls: 1, background: false}]}';
  await field(page, 'Skill invocation (YAML)', withChild);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'host-skill')).toBe(true);
  await field(page, 'Skill invocation (YAML)', withChild.replace('background: false', 'background: true'));
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'skill')).toBe(true);
  await field(page, 'Skill invocation (YAML)', withChild);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.skill.children[0].background).toBe(true);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.skill.children[0]).toEqual({ name: 'review', prompt: 'Review.', description: 'Retained review', tools: ['Read'], resources: ['reference.txt'], model: 'inherit', 'max-turns': 2, 'max-calls': 1, background: false });
  expect(await downloadText(page)).toContain('reference.txt');
  expect(await downloadText(page)).toContain('max-calls: 1');
  const dotResources = withChild.replaceAll('resources: [reference.txt]', 'resources: [./reference.txt]');
  await field(page, 'Skill invocation (YAML)', dotResources);
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.skill.resources).toEqual(['./reference.txt']);
  await field(page, 'Skill invocation (YAML)', dotResources.replace('tools: [Read], resources: [./reference.txt]', 'tools: [Read], resources: [foreign.txt]'));
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'skill')).toBe(true);
  await field(page, 'Skill invocation (YAML)', dotResources);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).workflow.jobs.build.steps[0].with.skill.children[0].resources).toEqual(['foreign.txt']);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect(await downloadText(page)).toContain('./reference.txt');
  await ui.getByText('Structured output', { exact: true }).click();
  await field(page, 'Structured output contract (YAML)', 'json');
  await expect.poll(async () => (await snapshot(page)).diagnostics.some((d: any) => d.code === 'skill-repair')).toBe(true);
  await field(page, 'Structured output contract (YAML)', '{format: json, repair-attempts: 0}');
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).diagnostics.some((d: any) => d.code === 'skill-repair')).toBe(true);
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect(await downloadText(page)).toContain('repair-attempts: 0');

});
