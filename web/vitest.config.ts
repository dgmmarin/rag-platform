import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "@": path.resolve(__dirname, ".") },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./vitest.setup.ts"],
    // web/e2e holds Playwright specs (a separate runner, `npx playwright
    // test`) — its test.describe/test conflict with Vitest's, so exclude it
    // alongside Vitest's own defaults.
    exclude: ["**/node_modules/**", "**/dist/**", "e2e/**"],
  },
});
