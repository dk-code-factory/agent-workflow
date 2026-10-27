import { expect, test } from '@playwright/test';

for (const kind of ['structured YAML', 'identifier']) {
  for (const recovery of ['correct', 'Escape', 'Discard edit']) {
    test(`${recovery} recovers a rejected ${kind} draft before saving`, async ({ page }) => {
      await page.goto('/test/browser.html'); const editor = page.locator('#a');
      await page.evaluate(() => {
        (window as any).saves = [];
        (window as any).first.update({ onSave: (snapshot: any) => (window as any).saves.push(snapshot) });
      });
      await editor.getByRole('button', { name: 'greet', exact: false }).first().click();
      if (kind === 'structured YAML') await editor.getByText('Advanced parameters', { exact: true }).click();
      const field = editor.getByLabel(kind === 'structured YAML' ? 'Parameters (YAML)' : 'Step ID', { exact: true });
      const initial = await field.inputValue();
      const original = await page.evaluate(() => (window as any).first.getSnapshot().source);
      await field.fill(kind === 'structured YAML' ? '[invalid' : 'bad id');
      await field.press('Tab');
      await expect(field).toHaveAttribute('aria-invalid', 'true');
      await expect(editor.getByRole('alert')).toContainText('Edit has not been applied');
      await editor.getByRole('button', { name: 'Save', exact: true }).click();
      await expect(field).toBeFocused();
      expect(await page.evaluate(() => (window as any).saves)).toEqual([]);
      if (recovery === 'correct') {
        await field.fill(kind === 'structured YAML' ? 'prompt: Recovered draft' : 'recovered_step');
      } else if (recovery === 'Escape') {
        await field.press('Escape');
      } else {
        await editor.getByRole('button', { name: 'Discard edit', exact: true }).click();
      }
      if (recovery !== 'correct') {
        await expect(field).toHaveValue(initial);
        await expect(field).toHaveAttribute('aria-invalid', 'false');
        await expect(editor.getByRole('alert')).toHaveCount(0);
      }
      expect(await page.evaluate(() => (window as any).saves)).toEqual([]);
      await editor.getByRole('button', { name: 'Save', exact: true }).click();
      await expect.poll(() => page.evaluate(() => (window as any).saves.length)).toBe(1);
      const saved = await page.evaluate(() => (window as any).saves[0].source);
      if (recovery === 'correct') expect(saved).toContain(kind === 'structured YAML' ? 'Recovered draft' : 'recovered_step');
      else expect(saved).toBe(original);
      await expect(field).toHaveAttribute('aria-invalid', 'false');
      await expect(editor.getByRole('alert')).toHaveCount(0);
    });
  }
}
