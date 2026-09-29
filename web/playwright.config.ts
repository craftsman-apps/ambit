import { defineConfig, devices } from "@playwright/test";

// UI tests against a running Ambit (kind): `make test-ui` sets AMBIT_ADMIN_PASSWORD.
export default defineConfig({
  testDir: "e2e",
  timeout: 60_000,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: {
    baseURL: process.env.AMBIT_URL ?? "http://console.localtest.me:8080",
    trace: "retain-on-failure",
  },
  // Locally use the installed Google Chrome; CI installs Playwright's Chromium.
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], ...(process.env.CI ? {} : { channel: "chrome" }) } }],
});
