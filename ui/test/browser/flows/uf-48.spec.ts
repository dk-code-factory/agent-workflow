import { expect, test } from '@playwright/test';
import { parse } from 'yaml';
import { open, snapshot, field, job, step, downloadText } from './harness';

for (const { declaration, image } of [
  { declaration: 'null', image: undefined },
  { declaration: '{image: 123}', image: '123' },
  { declaration: '{image: 01}', image: '01' },
  { declaration: '{image: true}', image: 'true' },
]) test(`container ${declaration} matches runner scalar normalization`, async ({ page }) => {
  const source = `jobs: {build: {container: ${declaration}, steps: [{argv: [echo, ready]}]}}`;
  await open(page, source);
  const current = await snapshot(page);
  expect(current.diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  expect(current.workflow.jobs.build.container?.image).toBe(image);
  expect(await downloadText(page)).toBe(source);
});

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

test('edit native definitions and typed input while retaining six visible hook lists', async ({ page }) => {
  const source=`jobs:
  review:
    steps:
      - null
      - id: review
        uses: agent/run
        with:
          agent: reviewer.toml
          prompt: Review
          input: {count: 7, enabled: true}
          output: {format: json, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}
        onResult: [{argv: [python3, verify.py]}]
        onComplete: [{argv: [echo, accepted]}]
        onIncomplete: [{argv: [echo, feedback]}]
        onError: [{argv: [echo, recover]}]
        onFailure: [{argv: [echo, report]}]
        finally: [{argv: [echo, finalize]}]
`;
  const ui=await open(page,source);await step(page,'review agent/run');
  await expect(ui.getByLabel('Task input (YAML)',{exact:true})).toHaveValue(/count: 7/);
  await field(page,'Agent definition or reference','../.codex/agents/reviewer.toml');
  await field(page,'Agent turn allowance','0');
  expect((await snapshot(page)).diagnostics.some((d:any)=>d.path.at(-1)==='turns' && d.severity==='error')).toBe(true);
  await field(page,'Agent turn allowance','5');
  await field(page,'Task input (YAML)','{count: 9, enabled: false, exact: 9007199254740993}');
  const edited=await snapshot(page);const task=edited.workflow.jobs.review.steps[0];
  expect(task.with.turns).toBe(5);
  expect(task.with.input.count).toBe(9);expect(task.with.input.enabled).toBe(false);
  expect(edited.source).toContain('9007199254740993');
  await expect(ui.getByLabel('Task input (YAML)',{exact:true})).toHaveValue(/9007199254740993/);
  for(const key of ['onResult','onComplete','onIncomplete','onError','onFailure','finally'])expect(task[key]).toHaveLength(1);
  await ui.getByText('Failure handlers and finalization',{exact:true}).click();
  for (const [label, command] of [
    ['Result verification steps (YAML)','verify.py'], ['Completion steps (YAML)','accepted'],
    ['Incomplete steps (YAML)','feedback'], ['Fallback steps (YAML)','recover'],
    ['Failure reporting steps (YAML)','report'], ['Finalization steps (YAML)','finalize'],
  ]) {
    await expect(ui.getByLabel(label,{exact:true})).toBeVisible();
    await expect(ui.getByLabel(label,{exact:true})).toHaveValue(new RegExp(command));
  }

  await ui.getByRole('button',{name:'Undo',exact:true}).click();
  expect((await snapshot(page)).workflow.jobs.review.steps[0].with.input.count).toBe(7);
  await ui.getByRole('button',{name:'Redo',exact:true}).click();
  expect((await snapshot(page)).source).toBe(edited.source);
  const exported=await downloadText(page);expect(exported).toBe(edited.source);
  let conversionMessage = '';
  page.once('dialog',async dialog=>{conversionMessage = dialog.message();await dialog.accept();});
  await ui.getByLabel('Step type',{exact:true}).selectOption('argv');
  expect(conversionMessage).toContain('onResult');
  expect((await snapshot(page)).workflow.jobs.review.steps[0].onResult).toBeUndefined();
  await ui.getByRole('button',{name:'Undo',exact:true}).click();expect((await snapshot(page)).source).toBe(edited.source);
});

for (const [token, allowance] of [['010', 8], ['1_0', 10]] as const) test(`native turn allowance ${token} matches the runner through edits and history`, async ({ page }) => {
  const source = `jobs: {review: {steps: [{id: review, uses: agent/run, with: {agent: reviewer.toml, prompt: Review, turns: ${token}}}]}}`;
  const ui = await open(page, source);
  await step(page, 'review agent/run');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await expect(ui.getByLabel('Agent turn allowance', { exact: true })).toHaveValue(String(allowance));
  await field(page, 'Prompt', 'Keep the admitted allowance');
  expect((await snapshot(page)).workflow.jobs.review.steps[0].with.turns).toBe(allowance);
  expect(parse(await downloadText(page)).jobs.review.steps[0].with.turns).toBe(allowance);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page)).source).toBe(source);
  await expect(ui.getByLabel('Agent turn allowance', { exact: true })).toHaveValue(String(allowance));
  await ui.getByRole('button', { name: 'Redo', exact: true }).click();
  expect(parse(await downloadText(page)).jobs.review.steps[0].with.turns).toBe(allowance);
});

test('hook YAML edits preserve exact nested numeric input through prompt changes and export', async ({ page }) => {
  const hooks = ['onResult', 'onComplete', 'onIncomplete', 'onError', 'onFailure', 'finally'];
  const labels = ['Result verification steps (YAML)', 'Completion steps (YAML)', 'Incomplete steps (YAML)', 'Fallback steps (YAML)', 'Failure reporting steps (YAML)', 'Finalization steps (YAML)'];
  const source = `jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with:
          agent: reviewer.toml
          prompt: Review
          input: {octal: 010, positive: +010, negative: -010, text: "010", enabled: false, empty: null, fraction: 0.10000000000000001, exponent: 1.234567890123456789e-20}
          output: {format: json, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}
${hooks.map(hook => `        ${hook}:
          - uses: agent/run
            with: {agent: reviewer.toml, prompt: Verify, input: {octal: -010, exact: 9007199254740993, fraction: 1.0000000000000001}}
`).join('')}`;
  const ui = await open(page, source); await step(page, 'review agent/run');
  await expect(ui.getByLabel('Task input (YAML)', {exact:true})).toHaveValue(/0\.10000000000000001/);
  await expect(ui.getByLabel('Task input (YAML)', {exact:true})).toHaveValue(/octal: 010/);
  await ui.getByText('Failure handlers and finalization', {exact:true}).click();
  for (const label of labels) {
    const control = ui.getByLabel(label, {exact:true});
    const before = await control.inputValue();
    expect(before).toContain('octal: -010');
    expect(before).toContain('9007199254740993');
    expect(before).toContain('1.0000000000000001');
    await field(page, label, before.replace('prompt: Verify', 'prompt: Revised'));
    await expect(control).toHaveValue(/prompt: Revised/);
    await expect(control).toHaveValue(/octal: -010/);
    await expect(control).toHaveValue(/9007199254740993/);
    await expect(control).toHaveValue(/1\.0000000000000001/);
  }
  await field(page, 'Prompt', 'Review carefully');
  const edited = await snapshot(page);
  for (const token of ['octal: 010', 'positive: +010', 'negative: -010', '9007199254740993', '0.10000000000000001', '1.0000000000000001', '1.234567890123456789e-20']) expect(edited.source).toContain(token);
  expect(edited.workflow.jobs.review.steps[0].with.input).toMatchObject({text: '010', enabled: false, empty: null});
  for (const hook of hooks) expect(edited.workflow.jobs.review.steps[0][hook][0].with.prompt).toBe('Revised');
  expect(edited.diagnostics.filter((d:any) => d.severity === 'error')).toEqual([]);
  expect(await downloadText(page)).toBe(edited.source);
  await ui.getByRole('button', {name:'Undo', exact:true}).click();
  expect((await snapshot(page)).workflow.jobs.review.steps[0].with.prompt).toBe('Review');
  await ui.getByRole('button', {name:'Redo', exact:true}).click();
  expect((await snapshot(page)).source).toBe(edited.source);
});

for (const editor of ['parameters', 'parallel', 'sequence']) {
  test(`${editor} YAML edits retain exact task and nested handler input`, async ({ page }) => {
    const task = `- id: native
  uses: agent/run
  with:
    agent: reviewer.toml
    prompt: Work
    input: {exact: 9007199254740993, fraction: 0.10000000000000001, exponent: 1.234567890123456789e-20}
  onError:
    - uses: agent/run
      with: {agent: reviewer.toml, prompt: Recover, input: {nested: 9007199254740995}}
`;
    const source = `jobs:\n  review:\n    steps:\n` + (editor === 'parameters'
      ? task.split('\n').filter(Boolean).map(line => `      ${line}\n`).join('')
      : `      - id: group\n        ${editor}:\n` + task.split('\n').filter(Boolean).map(line => `          ${line}\n`).join(''));
    const ui = await open(page, source);
    await step(page, editor === 'parameters' ? 'native agent/run' : `group ${editor} block`);
    if (editor === 'parameters') await ui.getByText('Advanced parameters', {exact:true}).click();
    const label = editor === 'parameters' ? 'Parameters (YAML)' : editor === 'parallel' ? 'Parallel branches (YAML)' : 'Sequence steps (YAML)';
    const control = ui.getByLabel(label, {exact:true});
    const before = await control.inputValue();
    for (const token of ['9007199254740993', '0.10000000000000001', '1.234567890123456789e-20']) expect(before).toContain(token);
    await field(page, label, before.replace('prompt: Work', 'prompt: Revised'));
    const edited = await snapshot(page);
    const changed = editor === 'parameters' ? edited.workflow.jobs.review.steps[0] : edited.workflow.jobs.review.steps[0][editor][0];
    expect(changed.with.prompt).toBe('Revised');
    for (const token of ['9007199254740993', '0.10000000000000001', '1.234567890123456789e-20', '9007199254740995']) expect(edited.source).toContain(token);
    expect(edited.diagnostics.filter((d:any) => d.severity === 'error')).toEqual([]);
    expect(await downloadText(page)).toBe(edited.source);
    await ui.getByRole('button', {name:'Undo', exact:true}).click();
    expect((await snapshot(page)).source).toBe(source);
    await ui.getByRole('button', {name:'Redo', exact:true}).click();
    expect((await snapshot(page)).source).toBe(edited.source);
  });
}
