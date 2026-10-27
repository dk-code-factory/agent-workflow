import { defineConfig, devices } from '@playwright/test';
const port = Number(process.env.WORKFLOW_UI_TEST_PORT ?? 4173);
export default defineConfig({
  forbidOnly: true, workers: 2, retries: 0,
  reporter: [['list'], ['html', { open: 'never' }], ['./scripts/flow-reporter.mjs']],
  testDir: './test/browser', fullyParallel: true, timeout: 30000,
  use: { baseURL: `http://127.0.0.1:${port}`, viewport: { width: 1450, height: 1000 }, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: devices['Desktop Chrome'] }, { name: 'firefox', use: devices['Desktop Firefox'] }, { name: 'webkit', use: devices['Desktop Safari'] }],
  webServer: { command: `npm run dev -- --port ${port} --strictPort`, url: `http://127.0.0.1:${port}`, reuseExistingServer: !process.env.CI },
});
