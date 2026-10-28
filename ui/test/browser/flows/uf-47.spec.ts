import { expect, test } from '@playwright/test';
import { open, field, downloadText, snapshot, step } from './harness';

const incoming = 'name: Host update\njobs: {replacement: {steps: [{argv: [echo]}]}}';

test('conflicting replacements wait for a choice and remain undoable', async ({ page }) => {
  const ui = await open(page); await field(page, 'Name', 'My draft');
  await page.evaluate(source => (window as any).first.update({ source }), incoming);
  const notice = ui.getByRole('region', { name: 'Incoming source replacement' });
  await expect(notice).toBeVisible();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('My draft');
  await notice.getByRole('button', { name: 'Keep local', exact: true }).click();
  await page.evaluate(source => (window as any).first.update({ source }), incoming);
  await expect(notice).toHaveCount(0);
  expect(await downloadText(page)).toContain('name: My draft');
  await page.evaluate(source => (window as any).first.update({ source }), incoming.replace('Host update', 'Newest update'));
  await notice.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('Newest update');
  expect((await snapshot(page)).hasLocalChanges).toBe(false);
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('My draft');
});

for (const resolveWith of ['button', 'handle']) {
  test(`${resolveWith} discards unapplied fields without applying them`, async ({ page }) => {
    const ui = await open(page); await step(page);
    const prompt = ui.getByLabel('Prompt', { exact: true });
    await prompt.fill('Unapplied prompt');
    const before = await snapshot(page);
    await page.evaluate(source => (window as any).first.update({ source }), before.source.replace('Hello', 'Incoming prompt'));
    await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toBeVisible();
    if (resolveWith === 'button') await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
    else await page.evaluate(() => (window as any).first.resolveSourceReplacement('use-incoming'));
    await expect(prompt).toHaveValue('Incoming prompt');
    await prompt.press('Tab');
    expect((await snapshot(page)).source).not.toContain('Unapplied prompt');
    await ui.getByRole('button', { name: 'Undo', exact: true }).click();
    expect((await snapshot(page)).source).toBe(before.source);
  });
}

test('keep local preserves rejected fields and newest candidate is used', async ({ page }) => {
  const ui = await open(page); await step(page);
  const id = ui.getByLabel('Step ID', { exact: true });
  await id.fill('bad id'); await id.press('Tab');
  await page.evaluate(source => (window as any).first.update({ source }), incoming);
  await ui.getByRole('button', { name: 'Keep local', exact: true }).click();
  await expect(id).toHaveValue('bad id');
  await expect(id).toHaveAttribute('aria-invalid', 'true');
  await page.evaluate(source => {
    (window as any).first.update({ source: source + '\n# newer' });
    (window as any).first.update({ source: source + '\n# newest' });
  }, incoming);
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  expect((await snapshot(page)).source).toContain('# newest');
  expect((await snapshot(page)).canUndo).toBe(true);
});

test('controlled React echoes protect edits without opening a notice', async ({ page }) => {
  await page.goto('/test/browser.html?controlled');
  await field(page, 'Name', 'Controlled draft');
  await page.evaluate(() => (window as any).changePolicy());
  await expect.poll(() => page.evaluate(() => (window as any).latest.diagnostics.map((item: any) => item.message))).toContain('Policy 1');
  await expect(page.locator('#a').getByLabel('Name', { exact: true })).toHaveValue('Controlled draft');
  await expect(page.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
  await expect.poll(() => page.evaluate(() => (window as any).latest.hasLocalChanges)).toBe(true);
});

test('React host replacement prompts and unchanged props do not reopen it', async ({ page }) => {
  await page.goto('/test/browser.html?uncontrolled');
  await field(page, 'Name', 'React draft');
  await page.evaluate(source => (window as any).replaceHostSource(source), incoming);
  await page.getByRole('button', { name: 'Keep local', exact: true }).click();
  await page.evaluate(() => (window as any).rerenderHost());
  await expect(page.locator('#a .aw-editor')).toHaveAttribute('data-theme', 'dark');
  await expect(page.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
  await expect(page.locator('#a').getByLabel('Name', { exact: true })).toHaveValue('React draft');
});

test('clean editors and read-only viewers update immediately', async ({ page }) => {
  for (const query of ['', '?viewer']) {
    await page.goto('/test/browser.html' + query);
    await page.evaluate(source => (window as any).first.update({ source }), incoming);
    await expect(page.locator('#a').getByRole('button', { name: 'Inspect job replacement', exact: true })).toBeVisible();
    await expect(page.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
    expect((await snapshot(page)).hasLocalChanges).toBe(false);
  }
});

// This host fixture deliberately buffers raw DOM text and commits on blur.
// It stays local to this flow so other flows keep their existing harness.
async function rawInspector(page: import('@playwright/test').Page, viewer = false, commitOnBlur = true) {
  await page.goto('/test/browser.html' + (viewer ? '?viewer' : ''));
  await page.evaluate(commitOnBlur => {
    const state: { contexts: import('../../../src').CustomInspectorContext[]; cleanups: number; discards: number; renders: number; failDiscard?: boolean; onDiscard?: () => void; action?: import('../../../src').ActionDefinition } = (window as any).raw = { contexts: [], cleanups: 0, discards: 0, renders: 0 };
    const action: import('../../../src').ActionDefinition = {
      uses: 'agent/claude', label: 'Buffered renderer',
      renderInspector(element, context) {
        state.contexts.push(context); state.renders++;
        const input = document.createElement('input');
        input.setAttribute('aria-label', 'Raw prompt'); input.readOnly = context.readOnly;
        const initial = String(context.step.with?.prompt ?? ''); input.value = initial;
        const discard = () => {
          state.discards++;
          if (state.failDiscard) throw new Error('Custom discard failed');
          state.onDiscard?.();
          input.value = initial;
          // Neither discard nor cleanup may sneak a source edit into history.
          context.setField(['with', 'prompt'], 'Discard write');
        };
        input.oninput = () => context.reportDraft(input.value !== initial, discard);
        input.onblur = () => {
          if (!commitOnBlur) return;
          if (input.value !== initial) context.setField(['with', 'prompt'], input.value);
          context.reportDraft(false, discard);
        };
        element.append(input);
        return () => {
          state.cleanups++; context.setField(['with', 'prompt'], 'Cleanup write');
          context.reportDraft(true, discard); input.oninput = null; input.onblur = null;
        };
      },
    };
    state.action = action;
    (window as any).first.update({ actions: [action] });
  }, commitOnBlur);
  const ui = page.locator('#a'); await step(page);
  await expect(ui.getByLabel('Raw prompt', { exact: true })).toBeVisible();
  return ui;
}

for (const resolveWith of ['button', 'handle']) {
  test(`participating raw buffers survive Keep local and ${resolveWith} discards before blur`, async ({ page }) => {
    const ui = await rawInspector(page);
    const raw = ui.getByLabel('Raw prompt', { exact: true });
    const before = await snapshot(page);
    await raw.fill('Buffered raw draft');
    expect((await snapshot(page)).source).toBe(before.source);
    expect((await snapshot(page)).hasLocalChanges).toBe(true);
    await page.evaluate(source => (window as any).first.update({ source }), before.source.replace('Hello', 'Incoming raw prompt'));
    await ui.getByRole('button', { name: 'Keep local', exact: true }).click();
    await expect(raw).toHaveValue('Buffered raw draft');
    await expect(raw).toBeFocused();
    expect((await snapshot(page)).source).toBe(before.source);
    await page.evaluate(source => {
      (window as any).first.update({ source: source.replace('Hello', 'New raw prompt') });
      (window as any).first.update({ source: source.replace('Hello', 'Newest raw prompt') });
    }, before.source);
    if (resolveWith === 'button') await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
    else await page.evaluate(() => (window as any).first.resolveSourceReplacement('use-incoming'));
    await expect(raw).toHaveValue('Newest raw prompt');
    await raw.focus(); await raw.press('Tab');
    const after = await snapshot(page);
    expect(after.source).not.toContain('Buffered raw draft');
    expect(after.source).not.toContain('Discard write');
    expect(after.source).not.toContain('Cleanup write');
    expect(after.revision).toBe(before.revision + 1);
    expect(after.hasLocalChanges).toBe(false);
    expect(await page.evaluate(() => (window as any).raw.discards)).toBe(1);
    await ui.getByRole('button', { name: 'Undo', exact: true }).click();
    await expect(raw).toHaveValue('Hello');
    expect((await snapshot(page)).source).toBe(before.source);
    await expect(ui.getByRole('button', { name: 'Undo', exact: true })).toBeDisabled();
  });
}

test('raw contributions aggregate with fields and survive sibling edits, catalogs, echoes and saves', async ({ page }) => {
  const ui = await rawInspector(page, false, false);
  const raw = ui.getByLabel('Raw prompt', { exact: true });
  const id = ui.getByLabel('Step ID', { exact: true });
  await id.fill('bad id'); await id.press('Tab');
  await raw.fill('Buffered through rerenders');
  await raw.fill('Hello');
  expect((await snapshot(page)).hasLocalChanges).toBe(true);
  await raw.fill('Still pending');
  await id.focus(); await id.press('Escape');
  expect((await snapshot(page)).hasLocalChanges).toBe(true);
  await raw.fill('Retained buffer');
  const renders = await page.evaluate(() => (window as any).raw.renders);
  await page.evaluate(() => {
    const host = (window as any);
    host.first.update({ theme: 'dark', actions: [{ ...host.raw.action, renderInspector: (...args: any[]) => host.raw.action.renderInspector(...args) }] });
    host.raw.contexts.at(-1).setField(['with', 'model'], 'Sibling model');
    host.first.update({ source: host.first.getSnapshot().source });
  });
  await expect(raw).toHaveValue('Retained buffer');
  expect(await page.evaluate(() => (window as any).raw.renders)).toBe(renders);
  await page.evaluate(() => {
    const host = (window as any);
    host.first.update({ onSave: () => new Promise<void>(resolve => { host.finishRawSave = resolve; }) });
  });
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(ui.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Saving…');
  await page.evaluate(() => (window as any).finishRawSave());
  await expect(ui.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Unsaved changes');
  await expect(raw).toHaveValue('Retained buffer');
  expect((await snapshot(page)).hasLocalChanges).toBe(true);
  await raw.fill('Hello');
  await expect.poll(() => page.evaluate(() => (window as any).raw.renders)).toBeGreaterThan(renders);
  expect((await snapshot(page)).hasLocalChanges).toBe(false);
});

test('renderer replacement and destruction release only their own pending contribution', async ({ page }) => {
  const ui = await rawInspector(page);
  const raw = ui.getByLabel('Raw prompt', { exact: true });
  await raw.fill('Old buffer');
  await page.evaluate(() => {
    const host = (window as any); host.oldRawContext = host.raw.contexts.at(-1);
    // Simulate a local identity change while the custom control is focused.
    host.oldRawContext.setField(['id'], 'replacement');
  });
  await expect(raw).toHaveValue('Hello');
  await page.evaluate(() => (window as any).first.update({ onSave: () => {} }));
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(ui.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Saved');
  await page.evaluate(() => {
    const host = (window as any);
    host.oldRawContext.setField(['with', 'prompt'], 'Obsolete write');
    host.oldRawContext.reportDraft(true, () => { throw new Error('Obsolete discard'); });
  });
  expect((await snapshot(page)).hasLocalChanges).toBe(false);
  await raw.fill('New renderer draft');
  await page.evaluate(() => (window as any).oldRawContext.reportDraft(false, () => {}));
  expect((await snapshot(page)).hasLocalChanges).toBe(true);
  await page.evaluate(source => (window as any).first.update({ source }), incoming);
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  expect((await snapshot(page)).source).toBe(incoming);
  expect((await snapshot(page)).hasLocalChanges).toBe(false);
  // A stale report must not make a subsequent clean replacement conflict.
  await page.evaluate(source => {
    const host = (window as any);
    host.oldRawContext.reportDraft(true, () => {});
    host.first.update({ source });
  }, incoming + '\n# clean');
  expect((await snapshot(page)).source).toBe(incoming + '\n# clean');
  await page.evaluate(() => {
    const host = (window as any);
    host.second.update({ source: 'jobs: {other: {steps: [{uses: agent/claude, with: {prompt: Second}}]}}', actions: [host.raw.action] });
  });
  await page.locator('#b').getByRole('button', { name: /step_1/ }).first().click();
  await page.locator('#b').getByLabel('Raw prompt', { exact: true }).fill('Other editor pending');
  await page.evaluate(() => {
    const host = (window as any); host.first.destroy();
    for (const context of host.raw.contexts.slice(0, -1)) {
      context.reportDraft(false, () => {}); context.setField(['with', 'prompt'], 'Destroyed write');
    }
  });
  expect(await page.evaluate(() => (window as any).second.getSnapshot().hasLocalChanges)).toBe(true);
  expect((await snapshot(page)).source).toBe(incoming + '\n# clean');
});

test('read-only raw renderer reports and writes cannot stage replacements', async ({ page }) => {
  const ui = await rawInspector(page, true);
  await expect(ui.getByLabel('Raw prompt', { exact: true })).toHaveAttribute('readonly', '');
  await page.evaluate(source => {
    const host = (window as any); const context = host.raw.contexts.at(-1);
    context.reportDraft(true, () => { throw new Error('Read-only discard'); });
    context.setField(['with', 'prompt'], 'Viewer write');
    host.first.update({ source });
  }, incoming);
  expect((await snapshot(page)).source).toBe(incoming);
  expect((await snapshot(page)).hasLocalChanges).toBe(false);
});

for (const resolveWith of ['button', 'handle']) {
  test(`${resolveWith} keeps source, candidate and raw protection when discard fails`, async ({ page }) => {
    const ui = await rawInspector(page);
    const raw = ui.getByLabel('Raw prompt', { exact: true });
    const before = await snapshot(page);
    await raw.fill('Draft that cannot be discarded yet');
    await page.evaluate(source => {
      const host = (window as any); host.raw.failDiscard = true;
      host.first.update({ source });
    }, before.source.replace('Hello', 'Incoming after retry'));
    if (resolveWith === 'button') {
      await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
      await expect(ui.getByRole('alert')).toContainText('Custom discard failed');
    } else {
      expect(await page.evaluate(() => {
        try { (window as any).first.resolveSourceReplacement('use-incoming'); }
        catch (error) { return String(error); }
      })).toContain('Custom discard failed');
    }
    await expect(raw).toHaveValue('Draft that cannot be discarded yet');
    await expect(raw).toBeFocused();
    const failed = await snapshot(page);
    expect(failed.source).toBe(before.source);
    expect(failed.revision).toBe(before.revision);
    expect(failed.hasLocalChanges).toBe(true);
    expect(failed.pendingSourceReplacement).toContain('Incoming after retry');
    await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toBeVisible();
    await page.evaluate(() => { (window as any).raw.failDiscard = false; });
    await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
    await expect(raw).toHaveValue('Incoming after retry');
    expect((await snapshot(page)).hasLocalChanges).toBe(false);
    expect((await snapshot(page)).revision).toBe(before.revision + 1);
  });
}

test('anonymous step replacement invalidates old raw callbacks before React cleanup', async ({ page }) => {
  const ui = await rawInspector(page, false, false);
  // Remove the explicit ID without changing the action, then save the setup.
  await page.evaluate(() => {
    const host = (window as any);
    host.raw.contexts.at(-1).setField(['id'], undefined);
    host.first.update({ onSave: () => {} });
  });
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  const raw = ui.getByLabel('Raw prompt', { exact: true });
  await raw.fill('Anonymous pending');
  await page.evaluate(() => {
    const host = (window as any); host.anonymousContext = host.raw.contexts.at(-1);
    // A constrained sibling edit keeps this anonymous owner's raw buffer.
    host.anonymousContext.setField(['with', 'model'], 'Sibling model');
  });
  await expect(raw).toHaveValue('Anonymous pending');
  await ui.getByRole('button', { name: 'YAML', exact: true }).click();
  // Prepare two anonymous steps with the same action for local YAML authoring.
  await page.evaluate(() => {
    const host = (window as any);
    host.first.update({ source: 'jobs: {build: {steps: [{uses: agent/claude, with: {prompt: First}}, {uses: agent/claude, with: {prompt: Second}}]}}' });
    host.first.resolveSourceReplacement('use-incoming');
  });
  await expect(raw).toHaveValue('First');
  await raw.fill('Pending first anonymous step');
  await page.evaluate(() => {
    const host = (window as any); host.beforeReorderContext = host.raw.contexts.at(-1);
    host.first.update({ onChange: (snapshot: any) => {
      if (!snapshot.source.includes('# reordered') || host.reorderGuard) return;
      host.reorderGuard = true;
      // This callback runs inside the source edit, before React can clean up.
      host.beforeReorderContext.setField(['with', 'prompt'], 'Wrong owner write');
      host.beforeReorderContext.reportDraft(true, () => { throw new Error('Stale anonymous discard'); });
    } });
  });
  // Filling CodeMirror is a local arbitrary source edit, not a host candidate.
  await ui.getByRole('textbox', { name: 'Workflow YAML' }).fill('jobs: {build: {steps: [{uses: agent/claude, with: {prompt: Second}}, {uses: agent/claude, with: {prompt: First}}]}}\n# reordered');
  await expect(raw).toHaveValue('Second');
  expect((await snapshot(page)).source).not.toContain('Wrong owner write');
  expect(await page.evaluate(() => (window as any).reorderGuard)).toBe(true);
  await page.evaluate(() => {
    const host = (window as any);
    host.beforeReorderContext.setField(['with', 'prompt'], 'Late wrong owner write');
  });
  await expect(raw).toHaveValue('Second');
});


test('whole anonymous-step edits and undo invalidate the prior renderer callbacks', async ({ page }) => {
  const ui = await rawInspector(page, false, false);
  await page.evaluate(() => (window as any).raw.contexts.at(-1).setField(['id'], undefined));
  const raw = ui.getByLabel('Raw prompt', { exact: true });
  await raw.fill('First raw buffer');
  await page.evaluate(() => {
    const host = (window as any); host.beforeStepEdit = host.raw.contexts.at(-1);
    host.beforeStepEdit.setField([], { uses: 'agent/claude', with: { prompt: 'Replacement anonymous step' } });
    host.beforeStepEdit.setField(['with', 'prompt'], 'Wrong immediate owner');
  });
  await expect(raw).toHaveValue('Replacement anonymous step');
  await raw.fill('Replacement raw buffer');
  await page.evaluate(() => {
    const host = (window as any); host.beforeUndo = host.raw.contexts.at(-1);
    host.first.update({ onChange: (snapshot: any) => {
      if (snapshot.source.includes('Replacement anonymous step') || host.undoGuard) return;
      host.undoGuard = true; host.beforeUndo.setField(['with', 'prompt'], 'Wrong undo owner');
    } });
  });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(raw).toHaveValue('Hello');
  expect((await snapshot(page)).source).not.toContain('Wrong');
  expect(await page.evaluate(() => (window as any).undoGuard)).toBe(true);
});


test('custom discard failure preserves rejected fields and Keep local clears only its error', async ({ page }) => {
  const ui = await rawInspector(page);
  await page.evaluate(() => (window as any).first.update({ onSave: () => { throw new Error('Existing save failure'); } }));
  await ui.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(ui.locator('.aw-error')).toContainText('Existing save failure');
  const id = ui.getByLabel('Step ID', { exact: true });
  await id.fill('bad id'); await id.press('Tab');
  const raw = ui.getByLabel('Raw prompt', { exact: true });
  await raw.fill('Raw buffer to keep');
  const before = await snapshot(page);
  await page.evaluate(source => {
    const host = (window as any); host.raw.failDiscard = true;
    host.first.update({ source: source.replace('Hello', 'Incoming replacement') });
  }, before.source);
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(ui.locator('.aw-error')).toContainText('Custom discard failed');
  await expect(id).toHaveValue('bad id');
  await expect(id).toHaveAttribute('aria-invalid', 'true');
  await expect(raw).toHaveValue('Raw buffer to keep');
  await expect(raw).toBeFocused();
  expect((await snapshot(page)).source).toBe(before.source);
  await ui.getByRole('button', { name: 'Keep local', exact: true }).click();
  await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
  await expect(ui.locator('.aw-error')).not.toContainText('Custom discard failed');
  await expect(ui.locator('.aw-error')).toContainText('Existing save failure');
  await expect(id).toHaveValue('bad id');
  await expect(id).toHaveAttribute('aria-invalid', 'true');
  await expect(raw).toHaveValue('Raw buffer to keep');
  await expect(raw).toBeFocused();
  const kept = await snapshot(page);
  expect(kept.source).toBe(before.source);
  expect(kept.revision).toBe(before.revision);
  expect(kept.hasLocalChanges).toBe(true);
  expect(kept.saveError).toContain('Existing save failure');
});

for (const narrow of [false, true]) {
  test(`a fresh raw buffer from discard notification survives at ${narrow ? 'narrow' : 'desktop'} width`, async ({ page }) => {
    const ui = await rawInspector(page, false, false);
    if (narrow) await ui.evaluate(element => { element.style.width = '620px'; });
    const raw = ui.getByLabel('Raw prompt', { exact: true });
    await raw.fill('Original raw draft');
    const before = await snapshot(page);
    await page.evaluate(source => {
      const host = (window as any);
      host.first.update({ source: source.replace('Hello', 'Incoming after notification') });
      host.freshRawInput = document.querySelector('#a input[aria-label="Raw prompt"]');
      host.beforeFreshCleanups = host.raw.cleanups;
      let armed = true;
      host.first.update({ onChange: (snapshot: import('../../../src').Snapshot) => {
        if (armed && !snapshot.hasLocalChanges) {
          armed = false;
          host.freshRawInput.value = 'Fresh raw draft';
          host.freshRawInput.dispatchEvent(new Event('input', { bubbles: true }));
        }
      } });
    }, before.source);
    const staged = await snapshot(page);
    const notice = ui.getByRole('region', { name: 'Incoming source replacement' });
    await notice.getByRole('button', { name: 'Use incoming', exact: true }).focus();
    await page.keyboard.press('Enter');
    await expect(raw).toHaveValue('Fresh raw draft');
    expect(await snapshot(page)).toEqual(staged);
    expect(await raw.evaluate(element => element === (window as any).freshRawInput)).toBe(true);
    expect(await page.evaluate(() => (window as any).raw.cleanups)).toBe(await page.evaluate(() => (window as any).beforeFreshCleanups));
    expect(await page.evaluate(() => (window as any).raw.discards)).toBe(1);
    await expect(notice).toBeVisible();
    await notice.getByRole('button', { name: 'Use incoming', exact: true }).focus();
    await page.keyboard.press('Enter');
    await expect(raw).toHaveValue('Incoming after notification');
    expect(await page.evaluate(() => (window as any).raw.discards)).toBe(2);
    expect((await snapshot(page))).toMatchObject({ revision: before.revision + 1, hasLocalChanges: false, pendingSourceReplacement: undefined });
    await ui.getByRole('button', { name: 'Undo', exact: true }).click();
    expect(await snapshot(page)).toMatchObject({ source: before.source, canUndo: before.canUndo });
  });
}

test('a newer source from custom discard survives the captured replacement without extra history', async ({ page }) => {
  const ui = await rawInspector(page);
  const raw = ui.getByLabel('Raw prompt', { exact: true });
  const before = await snapshot(page);
  await raw.fill('Pending raw buffer');
  const newest = before.source.replace('Hello', 'Newest source');
  await page.evaluate(({ first, newest }) => {
    const host = (window as any);
    host.first.update({ source: first });
    host.raw.onDiscard = () => host.first.update({ source: newest });
  }, { first: before.source.replace('Hello', 'Older source'), newest });
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  expect((await snapshot(page))).toMatchObject({ source: before.source, revision: before.revision, pendingSourceReplacement: newest });
  await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toBeVisible();
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(raw).toHaveValue('Newest source');
  expect((await snapshot(page))).toMatchObject({ source: newest, revision: before.revision + 1, hasLocalChanges: false });
  await ui.getByRole('button', { name: 'Undo', exact: true }).click();
  expect((await snapshot(page))).toMatchObject({ source: before.source, canUndo: false });
});

for (const choice of ['keep-local', 'use-incoming'] as const) {
  test(`handle ${choice} clears a prior visible discard failure`, async ({ page }) => {
    const ui = await rawInspector(page); const raw = ui.getByLabel('Raw prompt', { exact: true });
    await raw.fill('Keep this buffer'); const original = (await snapshot(page)).source;
    await page.evaluate(source => {
      const host = (window as any); host.raw.failDiscard = true; host.first.update({ source: source.replace('Hello', 'Incoming retry') });
    }, original);
    await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
    await expect(ui.locator('.aw-error')).toContainText('Custom discard failed');
    await page.evaluate(choice => {
      const host = (window as any); host.raw.failDiscard = false; host.first.resolveSourceReplacement(choice);
    }, choice);
    await expect(ui.locator('.aw-error')).toHaveCount(0);
    await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
    await expect(raw).toHaveValue(choice === 'keep-local' ? 'Keep this buffer' : 'Incoming retry');
  });
}

test('a new reentrant discard failure stays visible until imperative resolution', async ({ page }) => {
  const ui = await rawInspector(page); const raw = ui.getByLabel('Raw prompt', { exact: true });
  await raw.fill('Buffered draft'); const original = (await snapshot(page)).source;
  await page.evaluate(source => {
    const host = (window as any); host.raw.failDiscard = true; host.first.update({ source: source.replace('Hello', 'Older source') });
  }, original);
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(ui.locator('.aw-error')).toContainText('Custom discard failed');
  await page.evaluate(source => {
    const host = (window as any); host.raw.failDiscard = false;
    host.raw.onDiscard = () => { host.first.update({ source: source.replace('Hello', 'Newer source') }); throw new Error('Newer discard failure'); };
  }, original);
  await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
  await expect(ui.locator('.aw-error')).toContainText('Newer discard failure');
  expect((await snapshot(page)).pendingSourceReplacement).toContain('Newer source');
  await page.evaluate(() => (window as any).first.resolveSourceReplacement('keep-local'));
  await expect(ui.locator('.aw-error')).toHaveCount(0); await expect(raw).toHaveValue('Buffered draft');
});

for (const clean of [false, true]) {
  test(`explicit handle reload honors the original source ${clean ? 'after saving' : 'while protected'}`, async ({ page }) => {
    const ui = await open(page); const original = (await snapshot(page)).source;
    await field(page, 'Name', 'Local request');
    if (clean) {
      await page.evaluate(() => (window as any).first.update({ onSave: () => {} }));
      await ui.getByRole('button', { name: 'Save', exact: true }).click();
      await expect(ui.getByRole('status', { name: 'Save status', exact: true })).toHaveText('Saved');
    }
    await page.evaluate(source => (window as any).first.update({ source }), original);
    if (clean) expect((await snapshot(page)).source).toBe(original);
    else await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toBeVisible();
  });
}

test('unchanged React initial props and theme updates preserve local edits', async ({ page }) => {
  await page.goto('/test/browser.html?uncontrolled'); const ui = page.locator('#a');
  await field(page, 'Name', 'Uncontrolled draft');
  await page.evaluate(() => (window as any).rerenderHost());
  await expect(ui.locator('.aw-editor')).toHaveAttribute('data-theme', 'dark');
  await expect(ui.getByLabel('Name', { exact: true })).toHaveValue('Uncontrolled draft');
  await expect(ui.getByRole('region', { name: 'Incoming source replacement' })).toHaveCount(0);
});

test('guided result wiring preserves a pending raw sibling buffer', async ({ page }) => {
  const ui = await rawInspector(page, false, false);
  await page.evaluate(() => {
    const host = (window as any); host.first.update({ source: host.first.getSnapshot().source.replace('    needs: build\n', '') });
  });
  const raw = ui.getByLabel('Raw prompt', { exact: true });
  await expect(raw).toHaveValue('Hello'); await raw.fill('Pending sibling buffer');
  const renders = await page.evaluate(() => (window as any).raw.renders);
  const before = await snapshot(page);
  await ui.getByRole('button', { name: 'Use job result in Model', exact: true }).click();
  const picker = ui.getByRole('dialog', { name: 'Use job result', exact: true });
  await picker.getByLabel('Producer job', { exact: true }).selectOption('test');
  await picker.getByLabel('Step output', { exact: true }).fill('stdout');
  await picker.getByLabel('Job output name', { exact: true }).fill('captured');
  await picker.getByRole('button', { name: 'Preview result wiring', exact: true }).click();
  expect((await snapshot(page)).source).toBe(before.source);
  await picker.getByRole('button', { name: 'Apply job result', exact: true }).click();
  await expect(raw).toHaveValue('Pending sibling buffer');
  expect(await page.evaluate(() => (window as any).raw.renders)).toBe(renders);
  const after = await snapshot(page);
  expect(after.source).not.toContain('Pending sibling buffer');
  expect(after.revision).toBe(before.revision + 1);
  expect(after.workflow.jobs.build.needs).toEqual(['test']);
  expect(after.workflow.jobs.test.outputs).toEqual({ captured: '${{ steps.step_1.outputs.stdout }}' });
  expect(after.workflow.jobs.build.steps[0].with.model).toBe('${{ needs.test.outputs.captured }}');
  expect(after.hasLocalChanges).toBe(true);
});

for (const narrow of [false, true]) {
  test(`diagnostic navigation protects reported raw buffers at ${narrow ? 'narrow' : 'desktop'} width`, async ({ page }) => {
    const ui = await rawInspector(page, false, false);
    if (narrow) await ui.evaluate(element => { element.style.width = '620px'; });
    await page.evaluate(() => (window as any).first.update({ validate: async () => [
      { code: 'command-policy', severity: 'error', source: 'host', path: ['jobs', 'test', 'steps', 0, 'argv', 0], message: 'Review command executable' },
    ] }));
    const diagnostic = ui.locator('.aw-diagnostics').getByRole('button', { name: /Review command executable/ });
    await expect(diagnostic).toBeVisible();
    const raw = ui.getByLabel('Raw prompt', { exact: true });
    await raw.fill('Buffered during diagnostic navigation');
    const before = await snapshot(page);
    const cleanups = await page.evaluate(() => (window as any).raw.cleanups);
    await diagnostic.click();
    await expect(raw).toHaveValue('Buffered during diagnostic navigation');
    await expect(ui.locator('.aw-diagnostics [role=status]')).toContainText('Complete the pending inspector edit or discard it using its controls');
    expect(await snapshot(page)).toEqual(before);
    await diagnostic.focus(); await diagnostic.press('Enter');
    await expect(raw).toBeVisible();
    expect(await page.evaluate(() => (window as any).raw.cleanups)).toBe(cleanups);
    expect(await snapshot(page)).toEqual(before);
    await page.evaluate(source => (window as any).first.update({ source: source.replace('Hello', 'Incoming after navigation') }), before.source);
    await ui.getByRole('button', { name: 'Keep local', exact: true }).click();
    await expect(raw).toHaveValue('Buffered during diagnostic navigation');
    await page.evaluate(source => (window as any).first.update({ source: source.replace('Hello', 'Newest after navigation') }), before.source);
    await ui.getByRole('button', { name: 'Use incoming', exact: true }).click();
    await expect(raw).toHaveValue('Newest after navigation');
    await diagnostic.focus(); await diagnostic.press('Enter');
    await expect(ui.getByLabel('Executable', { exact: true })).toBeFocused();
    expect((await snapshot(page)).source).not.toContain('Buffered during diagnostic navigation');
    expect((await snapshot(page)).revision).toBe(before.revision + 1);
    await ui.getByRole('button', { name: 'Undo', exact: true }).click();
    expect((await snapshot(page)).source).toBe(before.source);
  });
}
