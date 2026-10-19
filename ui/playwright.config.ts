import { defineConfig, devices } from '@playwright/test';
export default defineConfig({
  forbidOnly: true, workers: 2, retries: 0,
  reporter: [['list'], ['html', { open: 'never' }], ['./scripts/flow-reporter.mjs']],
  testDir: './test/browser', fullyParallel: true, timeout: 30000,
  use: { baseURL: 'http://127.0.0.1:4173', viewport: { width: 1450, height: 1000 }, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: devices['Desktop Chrome'] }, { name: 'firefox', use: devices['Desktop Firefox'] }, { name: 'webkit', use: devices['Desktop Safari'] }],
  webServer: { command: 'npm run dev -- --port 4173 --strictPort', url: 'http://127.0.0.1:4173', reuseExistingServer: !process.env.CI },
});
