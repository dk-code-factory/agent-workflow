import { expect, test } from '@playwright/test';
import { open, snapshot, field, step } from './harness';

test('fix browser errors and report host validation failure', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: bad, argv: []}]}}');
  await expect(ui.locator('.aw-diagnostics')).toContainText('error');
  await step(page, 'bad'); await ui.getByLabel('Step type', { exact: true }).selectOption('argv');
  await ui.getByText('Advanced command YAML', { exact: true }).click();
  await field(page, 'Arguments (YAML array)', '[echo, fixed]');
  expect((await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await page.evaluate(() => (window as any).first.update({ validate: async () => { throw new Error('Validator unavailable'); } }));
  await expect(ui.locator('.aw-diagnostics')).toContainText('Validator unavailable');
  await expect(ui.locator('.aw-diagnostics')).toContainText('host');
});

for (const mode of ['Visual', 'Split', 'YAML']) {
  for (const narrow of [false, true]) {
    test(`diagnostics reveal fields in ${mode} at ${narrow ? 'narrow' : 'desktop'} width without editing`, async ({ page }) => {
      const source = 'jobs:\r\n  build:\r\n    steps:\r\n      - id: bad\r\n        argv: [echo]\r\n        timeout: invalid\r\n';
      const ui = await open(page, source);
      if (narrow) await ui.evaluate(element => { element.style.width = '620px'; });
      await ui.getByRole('button', { name: mode, exact: true }).click();
      await page.evaluate(() => (window as any).first.update({ layout: { positions: {}, collapsed: { build: true } } }));
      const before = await snapshot(page);
      const other = await page.evaluate(() => (window as any).second.getSnapshot());
      const diagnostic = ui.locator('.aw-diagnostics').getByRole('button', { name: /Expected a positive Go duration/ });
      for (const keyboard of [false, true]) {
        if (keyboard) { await diagnostic.focus(); await diagnostic.press('Enter'); }
        else await diagnostic.click();
        if (mode === 'YAML') {
          await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
          await expect.poll(() => page.evaluate(() => window.getSelection()?.toString())).toBe('invalid');
        } else {
          await expect(ui.getByLabel('Timeout', { exact: true })).toBeFocused();
          await expect(ui.getByLabel('Timeout', { exact: true })).toHaveValue('invalid');
          await expect(ui.getByRole('button', { name: 'Expand build' })).toHaveAttribute('aria-expanded', 'true');
          if (narrow) await expect(ui.locator('.aw-editor')).toHaveAttribute('data-panel', 'inspector');
        }
        expect(await snapshot(page)).toEqual(before);
        expect(await page.evaluate(() => (window as any).second.getSnapshot())).toEqual(other);
      }
    });
  }
}

test('opens collapsed controls and falls back to YAML for unsupported fields', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {repeat: {max-attempts: 0, until: done}, context: {session: remote}, steps: [{argv: [echo]}]}}');
  await ui.getByRole('button', { name: 'Visual', exact: true }).click();
  // A host can identify an exact nested field without changing the diagnostic API.
  await page.evaluate(() => (window as any).first.update({ validate: async () => [
    { code: 'attempts', severity: 'error', source: 'host', path: ['jobs', 'build', 'repeat', 'max-attempts'], message: 'Fix attempts' },
  ] }));
  await ui.locator('.aw-diagnostics').getByRole('button', { name: /Fix attempts/ }).click();
  await expect(ui.getByLabel('Maximum attempts')).toBeFocused();
  await expect(ui.getByLabel('Maximum attempts')).toBeVisible();
  await ui.locator('.aw-diagnostics').getByRole('button', { name: /Session\/delivery context/ }).click();
  await expect(ui.getByRole('button', { name: 'YAML', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
  await expect.poll(() => page.evaluate(() => window.getSelection()?.toString())).toContain('session: remote');
});

for (const mode of ['Visual', 'Split']) {
  test(`whole-job configuration errors use YAML and scalar action errors use Action target in ${mode}`, async ({ page }) => {
    const ui = await open(page, 'jobs: {build: {uses: workflows/call.yml, steps: [{argv: [echo]}]}}');
    await ui.getByRole('button', { name: mode, exact: true }).click();
    const before = await snapshot(page);
    await ui.locator('.aw-diagnostics').getByRole('button', { name: /Reusable jobs cannot contain steps/ }).click();
    await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
    await expect.poll(() => page.evaluate(() => window.getSelection()?.toString())).toBe('{uses: workflows/call.yml, steps: [{argv: [echo]}]}');
    expect(await snapshot(page)).toEqual(before);
    await page.evaluate(() => (window as any).first.update({ source: 'jobs: {build: {steps: [{uses: agent/invalid.name, with: {prompt: text}}]}}' }));
    await ui.getByRole('button', { name: mode, exact: true }).click();
    const scalar = await snapshot(page);
    await ui.locator('.aw-diagnostics').getByRole('button', { name: /Expected agent\/<name>/ }).click();
    await expect(ui.getByLabel('Action target', { exact: true })).toBeFocused();
    expect(await snapshot(page)).toEqual(scalar);
    await page.evaluate(() => (window as any).first.update({ source: 'jobs: {"bad.id": {steps: [{argv: [echo]}]}}' }));
    const identifier = await snapshot(page);
    await ui.locator('.aw-diagnostics').getByRole('button', { name: /Invalid job ID/ }).click();
    await expect(ui.getByLabel('Job ID', { exact: true })).toBeFocused();
    expect(await snapshot(page)).toEqual(identifier);
  });
}

test('parser locations work with stale preview and unavailable locations keep focus and mode', async ({ page }) => {
  const ui = await open(page);
  await ui.getByRole('button', { name: 'Visual', exact: true }).click();
  await page.evaluate(() => (window as any).first.update({ source: 'jobs:\r\n  build: [\r\n' }));
  const before = await snapshot(page);
  expect(before.stale).toBe(true);
  await ui.locator('.aw-diagnostics button').first().click();
  await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
  expect(before.diagnostics[0].location.offset).toBe(before.source.length);
  await expect.poll(() => ui.locator('.cm-content').evaluate(element => {
    const selection = window.getSelection();
    return !!selection?.isCollapsed && selection.anchorOffset === 0 && !!element.lastElementChild?.contains(selection.anchorNode);
  })).toBe(true);
  expect(await snapshot(page)).toEqual(before);
  await page.evaluate(() => (window as any).first.update({ validate: async () => [
    { code: 'unavailable', severity: 'error', source: 'host', path: [], message: 'No target' },
    { code: 'invalid-location', severity: 'error', source: 'host', path: [], location: { line: 999, column: 1, offset: 99999, endOffset: 99999 }, message: 'Invalid target' },
  ] }));
  for (const message of ['No target', 'Invalid target']) {
    const selected = await page.evaluate(() => window.getSelection()?.toString());
    const button = ui.locator('.aw-diagnostics').getByRole('button', { name: new RegExp(message) });
    await button.focus(); await button.press('Space');
    await expect(button).toBeFocused();
    await expect(ui.locator('.aw-diagnostics [role=status]')).toContainText('No source location is available');
    expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(selected);
  }
});

test('job-output diagnostics distinguish consuming values from identifiers at the same path', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {outputs: {summary: "${{ steps.missing.outputs.stdout }}"}, steps: [{argv: [echo]}]}}');
  const before = await snapshot(page);
  const reference = ui.locator('.aw-diagnostics').getByRole('button', { name: /Unknown steps reference/ });
  await reference.click();
  await expect(ui.getByLabel('Output value: summary', { exact: true })).toBeFocused();
  await expect(ui.getByLabel('Identifier: summary', { exact: true })).not.toBeFocused();
  expect(await snapshot(page)).toEqual(before);
  await page.evaluate(() => (window as any).first.update({ validate: async () => [
    { code: 'identifier', severity: 'error', source: 'host', path: ['jobs', 'build', 'outputs', 'summary'], message: 'Review output identifier' },
  ] }));
  await ui.locator('.aw-diagnostics').getByRole('button', { name: /Review output identifier/ }).click();
  await expect(ui.getByLabel('Identifier: summary', { exact: true })).toBeFocused();
  await reference.focus(); await reference.press('Enter');
  await expect(ui.getByLabel('Output value: summary', { exact: true })).toBeFocused();
  expect((await snapshot(page)).source).toBe(before.source);
});

test('diagnostics reveal guided argument and typed default controls', async ({ page }) => {
  const ui = await open(page, 'on: {workflow_call: {inputs: {count: {type: number, default: 3}}}}\njobs: {build: {steps: [{argv: [echo, hello]}]}}');
  await page.evaluate(() => (window as any).first.update({ validate: async () => [
    { code: 'argument', source: 'host', severity: 'error', path: ['jobs', 'build', 'steps', 0, 'argv', 1], message: 'Review argument' },
    { code: 'default', source: 'host', severity: 'error', path: ['on', 'workflow_call', 'inputs', 'count', 'default'], message: 'Review typed default' },
  ] }));
  const before = await snapshot(page);
  await ui.locator('.aw-diagnostics').getByRole('button', { name: /Review argument/ }).click();
  await expect(ui.getByLabel('Argument 1', { exact: true })).toBeFocused();
  await expect(ui.getByLabel('Argument 1', { exact: true })).toHaveValue('hello');
  await ui.locator('.aw-diagnostics').getByRole('button', { name: /Review typed default/ }).click();
  await expect(ui.getByLabel('Default: count', { exact: true })).toBeFocused();
  await expect(ui.getByLabel('Default: count', { exact: true })).toHaveValue('3');
  expect(await snapshot(page)).toEqual(before);
});

test('long diagnostic identifiers render bounded labels while preserving navigation paths', async ({ page }) => {
  const consumer = 'consumer_' + 'c'.repeat(4096);
  const ui = await open(page, JSON.stringify({ jobs: {
    producer: { outputs: { text: 'hello' }, steps: [{ argv: ['echo'] }] },
    [consumer]: { steps: [{ uses: 'action/capture', with: { prompt: '${{ needs.producer.outputs.text }}' } }] },
  } }));
  await ui.locator('.aw-diagnostics summary').click();
  const warning = ui.locator('.aw-diagnostics').getByRole('button', { name: /missing from Dependencies/ });
  expect((await warning.textContent())!.length).toBeLessThan(500);
  await expect(warning).toContainText('consumer_');
  await expect(warning).toContainText('…');
  const before = await snapshot(page);
  expect(before.diagnostics.find((d: any) => d.code === 'expression-dependency').path[1]).toBe(consumer);
  await warning.click();
  await expect(ui.getByRole('group', { name: 'Dependencies' }).getByLabel('producer', { exact: true })).toBeFocused();
  expect(await snapshot(page)).toEqual(before);
});

test('narrow inspector to YAML fallback focuses the range after the drawer closes', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {context: {session: remote}, steps: [{argv: [echo], timeout: invalid}]}}');
  await ui.evaluate(element => { element.style.width = '620px'; });
  await ui.getByRole('button', { name: 'Visual', exact: true }).click();
  await ui.locator('.aw-diagnostics').getByRole('button', { name: /Expected a positive Go duration/ }).click();
  await expect(ui.getByLabel('Timeout', { exact: true })).toBeFocused();
  const before = await snapshot(page);
  const unsupported = ui.locator('.aw-diagnostics').getByRole('button', { name: /Session\/delivery context/ });
  await unsupported.click();
  await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
  await expect.poll(() => page.evaluate(() => window.getSelection()?.toString())).toBe('{session: remote}');
  await unsupported.focus(); await unsupported.press('Enter');
  await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
  await expect.poll(() => page.evaluate(() => window.getSelection()?.toString())).toBe('{session: remote}');
  expect(await snapshot(page)).toEqual(before);
});

for (const value of ['Active valid name', 'bad id']) {
  test(`diagnostic pointer activation preserves pending edit: ${value}`, async ({ page }) => {
    const ui = await open(page, 'jobs: {build: {steps: [{id: first, argv: [echo]}, {id: bad, argv: [echo], timeout: invalid}]}}');
    await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
    await ui.getByRole('button', { name: '1. first', exact: true }).click();
    const control = ui.getByLabel(value === 'bad id' ? 'Step ID' : 'Name', { exact: true });
    await control.fill(value);
    const before = await snapshot(page);
    const button = ui.locator('.aw-diagnostics').getByRole('button', { name: /Expected a positive Go duration/ });
    await button.click();
    await expect(control).toBeFocused(); await expect(control).toHaveValue(value);
    await expect(ui.locator('.aw-diagnostics [role=status]')).toContainText('Complete the pending inspector edit');
    expect(await snapshot(page)).toEqual(before);
    if (value === 'bad id') {
      await control.press('Tab');
      await expect(control).toHaveAttribute('aria-invalid', 'true');
      await button.focus(); await button.press('Enter');
      await expect(control).toBeFocused();
    }
    await control.press('Escape');
    await button.click();
    await expect(ui.getByLabel('Timeout', { exact: true })).toBeFocused();
    expect((await snapshot(page)).source).toBe(before.source);
  });
}

test('viewer can navigate diagnostics without changing source', async ({ page }) => {
  await page.goto('/test/browser.html?viewer'); const ui = page.locator('#a');
  await page.evaluate(() => (window as any).first.update({ source: 'jobs: {build: {steps: [{argv: [echo], timeout: invalid}]}}' }));
  const before = await snapshot(page);
  const button = ui.locator('.aw-diagnostics').getByRole('button', { name: /Expected a positive Go duration/ });
  await button.click();
  await expect(ui.getByLabel('Timeout', { exact: true })).toBeFocused();
  await expect(ui.getByLabel('Timeout', { exact: true })).toHaveAttribute('readonly', '');
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  await button.focus(); await button.press('Enter');
  await expect(ui.getByRole('textbox', { name: 'Workflow YAML' })).toBeFocused();
  expect(await snapshot(page)).toEqual(before);
});

test('unavailable inspector paths preserve the current selection and save failure', async ({ page }) => {
  const ui = await open(page);
  await step(page, 'greet agent/claude');
  await page.evaluate(() => (window as any).first.update({
    onSave: async () => { throw new Error('Retry persistence'); },
    validate: async () => [{ code: 'missing', severity: 'error', source: 'host', path: ['jobs', 'test', 'unavailable'], message: 'Unavailable field' }],
  }));
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(ui.getByLabel('Save status')).toHaveText('Save failed');
  const before = await snapshot(page);
  await ui.getByLabel('Name', { exact: true }).focus();
  const selected = await page.evaluate(() => window.getSelection()?.toString());
  const unavailable = ui.locator('.aw-diagnostics').getByRole('button', { name: /Unavailable field/ });
  await unavailable.click();
  await expect(unavailable).toBeFocused();
  await unavailable.press('Enter');
  await expect(unavailable).toBeFocused();
  await expect(ui.getByLabel('Step ID', { exact: true })).toHaveValue('greet');
  await expect(ui.locator('.aw-diagnostics [role=status]')).toContainText('No source location is available');
  await expect(ui.getByRole('button', { name: 'Split', exact: true })).toHaveAttribute('aria-pressed', 'true');
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(selected);
  expect(await snapshot(page)).toEqual(before);
});

test('registered action syntax defers availability and schema validation to the host', async ({ page }) => {
  const ui = await open(page, 'jobs: {build: {steps: [{id: capture, uses: action/invalid.name}]}}');
  expect((await snapshot(page)).diagnostics.some((d: any) => d.severity === 'error')).toBe(true);
  await ui.getByRole('button', { name: 'Inspect job build', exact: true }).click();
  await ui.getByRole('button', { name: '1. capture', exact: true }).click();
  await field(page, 'Action target', 'action/capture');
  await expect.poll(async () => (await snapshot(page)).diagnostics.filter((d: any) => d.severity === 'error')).toEqual([]);
  await expect(ui.locator('.aw-diagnostics')).toContainText('Registered action availability and typed schemas require host runtime validation');
});
