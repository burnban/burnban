import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  timeout: 30_000,
  expect: { timeout: 7_500 },
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: {
    baseURL: process.env.BURNBAN_BROWSER_BASE_URL || "http://127.0.0.1:4242",
    browserName: "chromium",
    headless: true,
    trace: "retain-on-failure"
  }
});
