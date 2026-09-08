import { test, expect } from "@playwright/test";

// Golden-path E2E against a running Next origin (SPEC-11 §7, STORY-11.1 Task
// 8): guarded route -> login redirect -> sign in -> shell chrome -> tenant
// switcher persists across a reload -> logout -> guarded route redirects
// again. Needs a live Next origin fronting a live `ragctl` API, seeded via
// `mise run seed` (admin@example.com / devpassword123, demo tenant slugs
// "demo"/"acme" -> tenant NAMES "Demo Tenant"/"Acme Inc"). The whole file
// self-skips (clear message, exit 0 via mise-tasks/web-e2e) when
// E2E_BASE_URL isn't set, so a runner without the live stack is never
// red-walled (mirrors mise-tasks/e2e, backup-drill, vulncheck-gate).
//
// Locators are accessible (getByRole/getByLabel/getByText), not CSS
// selectors, so the Tailwind redesign's markup can change freely without
// making this spec brittle.
const BASE_URL = process.env.E2E_BASE_URL;
const EMAIL = "admin@example.com";
const PASSWORD = process.env.E2E_ADMIN_PASSWORD || "devpassword123";
const TENANT_NAME = "Demo Tenant";

test.describe("admin UI golden path", () => {
  test.skip(!BASE_URL, "E2E_BASE_URL not set — skipping live E2E (see mise-tasks/web-e2e)");

  test("guarded route -> login -> shell -> tenant switch -> logout", async ({ page }) => {
    // 1. A guarded route with no session redirects to /admin/login.
    await page.goto("/admin/sources");
    await expect(page).toHaveURL(/\/admin\/login$/);

    // 2. Sign in with the seeded platform admin (`mise run seed`); lands
    // back in the shell at /admin/sources (the first nav section). Shell
    // chrome — nav, tenant switcher, signed-in user's email — is visible.
    await page.getByLabel("Email").fill(EMAIL);
    await page.getByLabel("Password").fill(PASSWORD);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).toHaveURL(/\/admin\/sources$/);

    await expect(page.getByRole("navigation", { name: "Sections" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Sources" })).toBeVisible();
    const tenantSelect = page.getByLabel("Tenant");
    await expect(tenantSelect).toBeVisible();
    await expect(page.getByText(EMAIL)).toBeVisible();

    // 3. The tenant switcher lists the seeded demo tenants (`mise run seed`:
    // slugs demo/acme -> names "Demo Tenant"/"Acme Inc"). Pick one, reload,
    // and confirm the pick persisted (lib/tenant.tsx stores it in
    // localStorage) rather than reverting to the default selection.
    await expect(tenantSelect.getByRole("option", { name: "Demo Tenant" })).toBeAttached();
    await expect(tenantSelect.getByRole("option", { name: "Acme Inc" })).toBeAttached();

    await tenantSelect.selectOption({ label: TENANT_NAME });
    const selectedId = await tenantSelect.inputValue();

    await page.reload();
    await expect(page.getByLabel("Tenant")).toHaveValue(selectedId);

    // 4. Logout returns to /admin/login, and a guarded route redirects there
    // again (no lingering session).
    await page.getByRole("button", { name: "Log out" }).click();
    await expect(page).toHaveURL(/\/admin\/login$/);

    await page.goto("/admin/sources");
    await expect(page).toHaveURL(/\/admin\/login$/);
  });
});
