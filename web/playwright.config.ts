import { defineConfig, devices } from "@playwright/test";

// Golden-path E2E against a running Next origin (SPEC-11 §7). No webServer
// entry here on purpose: the live stack (Postgres, ragctl, Next) is brought up
// by the caller (mise-tasks/web-e2e locally, the web-e2e CI job) — this config
// only points Playwright at it. web/e2e/shell.spec.ts self-skips when
// E2E_BASE_URL isn't set, so `npx playwright test` is a clean no-op run
// (reported PASS with 0 tests) rather than a failure in that case.
export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: "list",
  use: {
    baseURL: process.env.E2E_BASE_URL,
    trace: "on-first-retry",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
