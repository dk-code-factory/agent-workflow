import { expect, test } from '@playwright/test';

test('keyboard save commits the active property draft', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ onSave: (snapshot: any) => { (window as any).savedSource = snapshot.source; (window as any).saveCalls = ((window as any).saveCalls ?? 0) + 1; return new Promise<void>(resolve => { (window as any).finishSave = resolve; }); } }));
  const status = first.getByRole('status', { name: 'Save status', exact: true });
  await expect(status).toHaveText('Unsaved changes');
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  const prompt = first.getByLabel('Prompt', { exact: true }); await prompt.fill('Save this active draft');
  await prompt.press('ControlOrMeta+s');
  await expect.poll(() => page.evaluate(() => (window as any).savedSource)).toContain('Save this active draft');
  await expect(first.getByRole('button', { name: 'Saving…', exact: true })).toBeDisabled();
  await expect(status).toHaveText('Saving…');
  await prompt.press('ControlOrMeta+s');
  expect(await page.evaluate(() => (window as any).saveCalls)).toBe(1);
  await page.evaluate(() => (window as any).finishSave());
  await expect(first.getByRole('button', { name: 'Save', exact: true })).toBeEnabled();
  await expect(status).toHaveText('Saved');
});


test('failed saving dismisses and can be retried', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ onSave: async () => { throw new Error('Cannot save now'); } }));
  await first.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(first.getByRole('alert')).toContainText('Cannot save now');
  await expect(first.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Save failed');
  await first.getByRole('button', { name: 'Dismiss', exact: true }).click(); await expect(first.getByRole('alert')).toHaveCount(0);
  await expect(first.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Unsaved changes');
  await page.evaluate(() => (window as any).first.update({ onSave: (snapshot: any) => { (window as any).savedSource = snapshot.source; } }));
  await first.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as any).savedSource)).toContain('Browser workflow');
  await expect(first.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Saved');
});

for (const width of [1450, 600]) {
  test(`saved status accounts for pending inspector edits during saving at width ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto('/test/browser.html'); const editor = page.locator('#a');
    await page.evaluate(() => (window as any).first.update({ onSave: () => new Promise<void>(resolve => { (window as any).finishSave = resolve; }) }));
    const status = editor.getByRole('status', { name: 'Save status', exact: true });
    await editor.getByRole('button', { name: 'Save', exact: true }).click();
    await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
    const prompt = editor.getByLabel('Prompt', { exact: true });
    await prompt.fill('Still typing during save');
    await page.evaluate(() => (window as any).finishSave());
    await expect(status).toHaveText('Unsaved changes');
    await expect(prompt).toBeFocused();
    await prompt.press('Escape');
    await expect(status).toHaveText('Saved');
    await editor.getByText('Advanced parameters', { exact: true }).click();
    const parameters = editor.getByLabel('Parameters (YAML)', { exact: true });
    await parameters.fill('[invalid');
    await expect(status).toHaveText('Unsaved changes');
    await parameters.press('ControlOrMeta+s');
    await expect(parameters).toHaveAttribute('aria-invalid', 'true');
    await expect(status).toHaveText('Unsaved changes');
    await editor.getByRole('button', { name: 'Discard edit', exact: true }).click();
    await expect(status).toHaveText('Saved');
  });
}

test('an older successful save leaves committed edits unsaved and undo restores saved status', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ onSave: () => new Promise<void>(resolve => { (window as any).finishSave = resolve; }) }));
  const status = editor.getByRole('status', { name: 'Save status', exact: true });
  await editor.getByRole('button', { name: 'Save', exact: true }).click();
  await editor.getByRole('button', { name: 'Workflow properties', exact: true }).click();
  const name = editor.getByLabel('Name', { exact: true });
  await name.fill('Newer source'); await name.press('Enter');
  await page.evaluate(() => (window as any).finishSave());
  await expect(status).toHaveText('Unsaved changes');
  await editor.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(status).toHaveText('Saved');
  await editor.getByRole('button', { name: 'Redo', exact: true }).click();
  await expect(status).toHaveText('Unsaved changes');
});

const identifierSource = `name: Identifier fixture
on:
  workflow_call:
    inputs: {topic: {type: string}}
    secrets: {token: {required: true}}
    outputs: {result: {value: done}}
jobs:
  build:
    outputs: {artifact: done}
    steps:
      - {id: greet, uses: agent/claude, with: {prompt: Hello}}
      - {id: other, argv: [echo, ok]}
  test:
    steps: [{argv: [echo, ok]}]
`;
for (const saveWith of ['toolbar', 'Mod-S']) {
  for (const kind of ['structured YAML', 'job', 'step', 'input', 'secret', 'output', 'job output']) {
    test(`${saveWith} blocks rejected ${kind} drafts`, async ({ page }) => {
      await page.goto('/test/browser.html'); const editor = page.locator('#a');
      await page.evaluate(source => {
        (window as any).saves = [];
        (window as any).first.update({ source, onSave: (snapshot: any) => (window as any).saves.push(snapshot) });
      }, identifierSource);
      let label: string;
      if (kind === 'structured YAML' || kind === 'step') {
        await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
        if (kind === 'structured YAML') await editor.getByText('Advanced parameters', { exact: true }).click();
        label = kind === 'step' ? 'Step ID' : 'Parameters (YAML)';
      } else if (kind === 'job' || kind === 'job output') {
        await editor.getByRole('button', { name: 'Inspect job build', exact: true }).click();
        if (kind === 'job output') await editor.getByText('Job outputs', { exact: true }).click();
        label = kind === 'job' ? 'Job ID' : 'Identifier: artifact';
      } else {
        await editor.getByRole('button', { name: 'Workflow properties', exact: true }).click();
        await editor.getByText(`Callable ${kind}s`, { exact: true }).click();
        label = `Identifier: ${kind === 'input' ? 'topic' : kind === 'secret' ? 'token' : 'result'}`;
      }
      const field = editor.getByLabel(label, { exact: true });
      const invalid = kind === 'structured YAML' ? '[invalid' : kind === 'job' ? 'test' : kind === 'step' ? 'other' : 'bad id';
      await field.fill(invalid);
      if (saveWith === 'toolbar') await editor.getByRole('button', { name: 'Save', exact: true }).click();
      else await field.press('ControlOrMeta+s');
      await expect(field).toHaveValue(invalid);
      await expect(field).toHaveAttribute('aria-invalid', 'true');
      await expect(field).toBeFocused();
      await expect(editor.getByRole('alert')).toContainText(`${label}: Edit has not been applied`);
      expect(await page.evaluate(() => (window as any).saves)).toEqual([]);
      expect(await page.evaluate(() => (window as any).first.getSnapshot().source)).toBe(identifierSource);
      // Correcting the same field lets either save path submit the new value.
      await field.fill(kind === 'structured YAML' ? 'prompt: Valid parameters' : 'valid_id');
      if (saveWith === 'toolbar') await editor.getByRole('button', { name: 'Save', exact: true }).click();
      else await field.press('ControlOrMeta+s');
      await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(1);
      expect(await page.evaluate(() => (window as any).saves[0].source)).toContain(kind === 'structured YAML' ? 'Valid parameters' : 'valid_id');
    });
  }
}

for (const width of [1450, 600]) {
  test(`Save reveals hidden rejected fields and accounts for each at width ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto('/test/browser.html'); const editor = page.locator('#a');
    await page.evaluate(() => {
      (window as any).saves = [];
      (window as any).first.update({ onSave: (snapshot: any) => (window as any).saves.push(snapshot) });
    });
    await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
    await editor.getByText('Structured output', { exact: true }).click();
    const output = editor.getByLabel('Structured output contract (YAML)', { exact: true });
    await output.fill('[bad'); await output.press('Tab');
    await editor.getByText('Advanced parameters', { exact: true }).click();
    const parameters = editor.getByLabel('Parameters (YAML)', { exact: true });
    await parameters.fill('[also bad'); await parameters.press('Tab');
    await editor.getByText('Structured output', { exact: true }).click();
    await editor.getByText('Advanced parameters', { exact: true }).click();
    if (width === 600) await editor.getByRole('navigation', { name: 'Panels' }).getByRole('button', { name: 'canvas', exact: true }).click();
    await editor.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(output).toBeVisible(); await expect(output).toBeFocused();
    await output.press('Escape');
    expect(await page.evaluate(() => (window as any).saves)).toEqual([]);
    await editor.getByRole('button', { name: 'Save', exact: true }).click();
    await expect(parameters).toBeVisible(); await expect(parameters).toBeFocused();
    await expect(parameters).toHaveValue('[also bad');
    expect(await page.evaluate(() => (window as any).saves)).toEqual([]);
    await editor.getByRole('button', { name: 'Discard edit', exact: true }).click();
    expect(await page.evaluate(() => (window as any).saves)).toEqual([]);
    await editor.getByRole('button', { name: 'Save', exact: true }).click();
    await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(1);
    expect(await page.evaluate(() => (window as any).saves[0].workflow.jobs.build.steps[0].with)).toEqual({ prompt: 'Hello' });
  });
}

test('valid active drafts apply once and rejected drafts are isolated per editor', async ({ page }) => {
  await page.goto('/test/browser.html'); const first = page.locator('#a'); const second = page.locator('#b');
  await page.evaluate(() => {
    (window as any).saves = [];
    (window as any).first.update({ onSave: (snapshot: any) => (window as any).saves.push(snapshot) });
    (window as any).second.update({ onSave: (snapshot: any) => (window as any).saves.push(snapshot) });
  });
  await first.getByRole('button', { name: 'greet', exact: false }).first().click();
  await first.getByText('Advanced parameters', { exact: true }).click();
  const parameters = first.getByLabel('Parameters (YAML)', { exact: true });
  await parameters.fill('[invalid'); await parameters.press('ControlOrMeta+s');
  await second.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(1);
  expect(await page.evaluate(() => (window as any).saves[0].source)).toContain('independent');
  await parameters.press('Escape');
  const revision = await page.evaluate(() => (window as any).first.getSnapshot().revision);
  const prompt = first.getByLabel('Prompt', { exact: true });
  await prompt.fill('Applied once'); await prompt.press('ControlOrMeta+s');
  await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(2);
  // Blurring after keyboard Save must not apply the draft a second time.
  await first.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(3);
  expect(await page.evaluate(() => (window as any).first.getSnapshot().revision)).toBe(revision + 1);
  expect(await page.evaluate(() => (window as any).saves.slice(1).map((snapshot: any) => snapshot.workflow.jobs.build.steps[0].with.prompt))).toEqual(['Applied once', 'Applied once']);
});


test('source diagnostics remain saveable and stale preview recovery resumes draft preflight', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => {
    (window as any).saves = [];
    (window as any).first.update({ source: 'jobs: [broken', onSave: (snapshot: any) => (window as any).saves.push(snapshot) });
  });
  await expect(editor.getByRole('status').filter({ hasText: 'Stale preview' })).toBeVisible();
  await editor.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(1);
  expect(await page.evaluate(() => (window as any).saves[0].source)).toBe('jobs: [broken');
  await editor.getByRole('button', { name: 'Undo', exact: true }).click();
  await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
  const prompt = editor.getByLabel('Prompt', { exact: true });
  await prompt.fill('Recovered preview'); await prompt.press('ControlOrMeta+s');
  await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(2);
  expect(await page.evaluate(() => (window as any).saves[1].workflow.jobs.build.steps[0].with.prompt)).toBe('Recovered preview');
});


test('a valid sibling edit preserves a rejected structured draft until discarded', async ({ page }) => {
  await page.goto('/test/browser.html'); const editor = page.locator('#a');
  await page.evaluate(() => {
    (window as any).saves = [];
    (window as any).first.update({ onSave: (snapshot: any) => (window as any).saves.push(snapshot) });
  });
  await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
  await editor.getByText('Advanced parameters', { exact: true }).click();
  const parameters = editor.getByLabel('Parameters (YAML)', { exact: true });
  await parameters.fill('[invalid'); await parameters.press('Tab');
  const prompt = editor.getByLabel('Prompt', { exact: true });
  await prompt.fill('Valid sibling edit'); await prompt.press('ControlOrMeta+s');
  await expect(parameters).toHaveValue('[invalid');
  await expect(parameters).toHaveAttribute('aria-invalid', 'true');
  await expect(parameters).toBeFocused();
  expect(await page.evaluate(() => (window as any).saves)).toEqual([]);
  await editor.getByRole('button', { name: 'Discard edit', exact: true }).click();
  await expect(parameters).toHaveValue('prompt: Valid sibling edit');
  await editor.getByRole('button', { name: 'Save', exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(1);
  expect(await page.evaluate(() => (window as any).saves[0].workflow.jobs.build.steps[0].with.prompt)).toBe('Valid sibling edit');
});
