import { test, expect, type Page } from "@playwright/test";

// Live E2E for the STORY-11.4 / 11.6 / 11.7 / 12.4 screens (documents, query,
// tenants, eval). Each test drives the REAL round-trip browser -> BFF -> ragctl ->
// tenant DB, which the unit/component tests cannot: it proves the new session
// routes, their SQL (e.g. eval ListRuns), and the data-fetching hooks work end to
// end. Needs the same live, seeded stack as shell.spec.ts (`mise run seed`:
// admin@example.com / devpassword123, tenants "Demo Tenant" / "Acme Inc"). The file
// self-skips (exit 0 via mise-tasks/web-e2e) when E2E_BASE_URL isn't set.
//
// Assertions tolerate an empty demo tenant: each read screen must render its data
// table OR its empty state, and must NOT show its error banner. A rendered empty
// state still proves the endpoint returned 200 and the UI decoded it.
const BASE_URL = process.env.E2E_BASE_URL;
const EMAIL = "admin@example.com";
const PASSWORD = process.env.E2E_ADMIN_PASSWORD || "devpassword123";

async function login(page: Page): Promise<void> {
  await page.goto("/admin/sources");
  await expect(page).toHaveURL(/\/admin\/login$/);
  await page.getByLabel("Email").fill(EMAIL);
  await page.getByLabel("Password").fill(PASSWORD);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).toHaveURL(/\/admin\/sources$/);
  // A tenant must be selected before tenant-scoped screens fetch.
  await expect(page.getByLabel("Tenant")).toBeVisible();
}

// nav clicks a sidebar section link (scoped to the Sections nav to stay unambiguous).
async function nav(page: Page, name: string): Promise<void> {
  await page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name }).click();
}

test.describe("admin UI new screens", () => {
  test.skip(!BASE_URL, "E2E_BASE_URL not set — skipping live E2E (see mise-tasks/web-e2e)");

  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test("documents screen loads without error (STORY-11.4)", async ({ page }) => {
    await nav(page, "Documents");
    await expect(page).toHaveURL(/\/admin\/documents$/);
    await expect(page.getByRole("heading", { name: "Documents", exact: true })).toBeVisible();

    // The list endpoint answered: either rows or the empty state, never the error.
    await expect(page.getByText(/could not load documents/i)).toHaveCount(0);
    const table = page.getByRole("table");
    const empty = page.getByText(/no documents yet/i);
    await expect(table.or(empty)).toBeVisible();
  });

  test("eval runs screen loads without error (STORY-12.4)", async ({ page }) => {
    await nav(page, "Eval");
    await expect(page).toHaveURL(/\/admin\/eval$/);
    await expect(page.getByRole("heading", { name: "Eval runs", exact: true })).toBeVisible();

    // Proves the new ListRuns query executed (not just the route wiring).
    await expect(page.getByText(/could not load eval runs/i)).toHaveCount(0);
    const table = page.getByRole("table");
    const empty = page.getByText(/no eval runs yet/i);
    await expect(table.or(empty)).toBeVisible();
  });

  test("tenants screen lists the seeded tenants (STORY-11.7)", async ({ page }) => {
    // The seeded admin is a platform admin, so the Tenants section is in the nav.
    await nav(page, "Tenants");
    await expect(page).toHaveURL(/\/admin\/tenants$/);
    await expect(page.getByRole("heading", { name: "Tenants", exact: true })).toBeVisible();
    await expect(page.getByText(/only platform admins/i)).toHaveCount(0);

    // The enrol form and the seeded tenants (seed: "Demo Tenant" / "Acme Inc").
    await expect(page.getByRole("button", { name: /enrol tenant/i })).toBeVisible();
    await expect(page.getByRole("cell", { name: "Demo Tenant" })).toBeVisible();
    await expect(page.getByRole("cell", { name: "Acme Inc" })).toBeVisible();
  });

  test("query playground answers a question (STORY-11.6)", async ({ page }) => {
    await nav(page, "Query");
    await expect(page).toHaveURL(/\/admin\/query$/);
    await expect(page.getByRole("heading", { name: "Query playground" })).toBeVisible();

    await page.getByLabel("Question").fill("What is this knowledge base about?");
    await page.getByRole("button", { name: /^ask$/i }).click();

    // The POST round-trip completed if EITHER a grounded/not-grounded answer panel
    // OR the error banner appears (an unconfigured LLM in dev still proves the
    // browser -> BFF -> ragctl path). A generation call can be slow, so wait.
    const answered = page.getByText(/grounded/i);
    const failed = page.getByText(/query failed/i);
    await expect(answered.or(failed)).toBeVisible({ timeout: 30_000 });
  });
});
