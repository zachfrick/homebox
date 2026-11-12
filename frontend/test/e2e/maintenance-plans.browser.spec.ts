import { expect, test } from "@playwright/test";
import type { Page } from "@playwright/test";

test.setTimeout(120_000);
test.use({ timezoneId: "Europe/Moscow" });

async function fixture(page: Page, active = true, completed = false) {
  const login = await page.request.post("/api/v1/users/login", {
    data: {
      username: "demo@example.com",
      password: process.env.E2E_PASSWORD || "demodemo",
      stayLoggedIn: false,
    },
  });
  expect(login.ok()).toBeTruthy();
  const { token } = await login.json();
  const headers = {
    Authorization: token.startsWith("Bearer") ? token : `Bearer ${token}`,
  };
  const response = await page.request.post("/api/v1/entities", {
    headers,
    data: { name: `Maintenance regression ${Date.now()}`, description: "" },
  });
  expect(response.ok()).toBeTruthy();
  const item = await response.json();
  const planResponse = await page.request.post(`/api/v1/entities/${item.id}/maintenance/plans`, {
    headers,
    data: {
      name: "Filter cartridges",
      description: "",
      intervalValue: 6,
      intervalUnit: "month",
      startDate: "2026-08-31",
      active,
    },
  });
  expect(planResponse.ok()).toBeTruthy();
  const plan = await planResponse.json();
  if (completed) {
    const entries = await (
      await page.request.get(`/api/v1/entities/${item.id}/maintenance?status=both`, { headers })
    ).json();
    const entry = entries[0];
    const result = await page.request.put(`/api/v1/maintenance/${entry.id}`, {
      headers,
      data: { ...entry, completedDate: "2026-08-31" },
    });
    expect(result.ok()).toBeTruthy();
  }
  await page.context().clearCookies();
  await page.goto("/");
  await page.getByRole("textbox", { name: "Email", exact: true }).fill("demo@example.com");
  await page.locator("input[type='password']").fill(process.env.E2E_PASSWORD || "demodemo");
  await page.locator("button[type='submit']").click();
  await expect(page).toHaveURL(/\/home$/, { timeout: 30_000 });
  await page.goto(`/item/${item.id}/maintenance`);
  if (completed) await page.getByRole("button", { name: "Completed", exact: true }).click();
  return { item, plan, headers };
}

test("waits for plan hydration before allowing a save", async ({ page }) => {
  await fixture(page);
  let release!: () => void;
  const gate = new Promise<void>(resolve => {
    release = resolve;
  });
  await page.route("**/maintenance/plans", async route => {
    const response = await route.fetch();
    await gate;
    await route.fulfill({ response });
  });
  await page.getByRole("button", { name: "Edit", exact: true }).first().click();
  const dialog = page.getByRole("dialog");
  try {
    await expect(dialog.locator("button[type='submit']")).toBeDisabled();
  } finally {
    release();
  }
  await expect(dialog.getByLabel("Every", { exact: true })).toHaveValue("6");
  await expect(dialog.locator("button[type='submit']")).toBeEnabled();
});

test("editing completed history does not resume a paused plan", async ({ page }) => {
  const { item, plan, headers } = await fixture(page, false, true);
  await page.getByRole("button", { name: "Edit", exact: true }).first().click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByLabel("Cost", { exact: true })).toBeEnabled();
  await dialog.getByLabel("Cost", { exact: true }).fill("42.50");
  await dialog.locator("button[type='submit']").click();
  await expect(dialog).toBeHidden();
  const plans = await (
    await page.request.get(`/api/v1/entities/${item.id}/maintenance/plans`, {
      headers,
    })
  ).json();
  expect(plans.find((p: { id: string }) => p.id === plan.id).active).toBe(false);
  // Explicitly resuming a paused plan must remain possible from its history.
  await page.getByRole("button", { name: "Edit", exact: true }).first().click();
  await expect(dialog.getByRole("checkbox")).toBeEnabled();
  await dialog.getByRole("checkbox").check();
  await dialog.locator("button[type='submit']").click();
  await expect(dialog).toBeHidden();
  const resumed = await (await page.request.get(`/api/v1/entities/${item.id}/maintenance/plans`, { headers })).json();
  expect(resumed.find((p: { id: string }) => p.id === plan.id).active).toBe(true);
});

test("creating a recurring occurrence preserves its cost", async ({ page }) => {
  const { item, headers } = await fixture(page);
  await page.getByRole("button", { name: "Duplicate", exact: true }).first().click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("checkbox").check();
  await dialog.getByLabel("Cost", { exact: true }).fill("42.50");
  await dialog.locator("button[type='submit']").click();
  await expect(dialog).toBeHidden();
  const entries = await (
    await page.request.get(`/api/v1/entities/${item.id}/maintenance?status=both`, { headers })
  ).json();
  expect(entries.some((e: { cost: string }) => Number(e.cost) === 42.5)).toBe(true);
});

test("a failed plan load cannot overwrite its period and can be retried", async ({ page }) => {
  await fixture(page);
  await page.route("**/maintenance/plans", route => route.fulfill({ status: 503, body: "Unavailable" }));
  await page.getByRole("button", { name: "Edit", exact: true }).first().click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("alert")).toBeVisible();
  await expect(dialog.locator("button[type='submit']")).toBeDisabled();
  await page.unroute("**/maintenance/plans");
  await dialog.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(dialog.getByLabel("Every", { exact: true })).toHaveValue("6");
  await expect(dialog.locator("button[type='submit']")).toBeEnabled();
});

test("completing in the editor keeps the plan and next occurrence aligned", async ({ page }) => {
  const { item, plan, headers } = await fixture(page);
  await page.getByRole("button", { name: "Edit", exact: true }).first().click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByLabel("Every", { exact: true })).toHaveValue("6");
  await dialog.locator("input.dp__input").first().click();
  await dialog.locator(".dp__today").click();
  await dialog.getByRole("button", { name: "Select", exact: true }).click();
  await dialog.locator("button[type='submit']").click();
  await expect(dialog).toBeHidden();
  const plans = await (await page.request.get(`/api/v1/entities/${item.id}/maintenance/plans`, { headers })).json();
  const stored = plans.find((p: { id: string }) => p.id === plan.id);
  const entries = await (
    await page.request.get(`/api/v1/entities/${item.id}/maintenance?status=both`, { headers })
  ).json();
  const pending = entries.filter((e: { completedDate: string }) => !e.completedDate);
  expect(pending).toHaveLength(1);
  expect(stored.nextDueAt.slice(0, 10)).toBe(pending[0].scheduledDate);
  expect(stored.nextDueAt.slice(0, 10)).not.toBe("2026-08-31");
});
