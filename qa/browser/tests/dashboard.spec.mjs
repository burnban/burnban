import { expect, test } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

const viewports = [
  { width: 320, height: 720 },
  { width: 375, height: 812 },
  { width: 768, height: 900 },
  { width: 1440, height: 1000 }
];
const teamBaseURL = process.env.BURNBAN_TEAM_BASE_URL;
const teamToken = process.env.BURNBAN_TEAM_TEST_TOKEN;
if (process.env.CI && (!teamBaseURL || !teamToken)) {
  throw new Error("CI must provide the isolated team gateway URL and test token");
}

async function waitForLiveDashboard(page) {
  await page.goto("/");
  await expect(page).toHaveTitle("burnban — the meter is running");
  await expect(page.locator("#statusText")).toHaveText("LIVE");
  await expect(page.locator("#proxyData")).toHaveAttribute("aria-busy", "false");
  await expect(page.locator("#demoBadge")).toBeVisible();
  await expect(page.locator("#demoBadge")).toHaveText("DEMO DATA");
  await expect(page.locator("#connectPanel")).toBeHidden();
  await expect(page.locator("#localUsagePanel")).toBeVisible();
  await expect(page.locator("#subContent")).toBeVisible();
  await expect(page.locator("#subCalls")).not.toHaveText("0");
  await expect(page.locator("#total")).toContainText("$");
  await expect(page.locator("#reqs")).not.toHaveText("0");
  await expect(page.locator("#lastUpdate")).not.toContainText("Waiting");
}

test("loads isolated demo data without external requests", async ({ page }) => {
  const externalRequests = [];
  const consoleErrors = [];
  page.on("request", request => {
    const url = new URL(request.url());
    if (url.hostname !== "127.0.0.1" && url.hostname !== "localhost") {
      externalRequests.push(request.url());
    }
  });
  page.on("console", message => {
    if (message.type() === "error") consoleErrors.push(message.text());
  });

  await waitForLiveDashboard(page);
  await expect(page.locator("#exposure")).toHaveText("localhost listener");
  expect(externalRequests).toEqual([]);
  expect(consoleErrors).toEqual([]);
});

test("demo rejects provider traffic without forwarding", async ({ page }) => {
  const externalRequests = [];
  page.on("request", request => {
    const url = new URL(request.url());
    if (url.hostname !== "127.0.0.1" && url.hostname !== "localhost") {
      externalRequests.push(request.url());
    }
  });
  await waitForLiveDashboard(page);
  const requestCountBefore = await page.locator("#reqs").textContent();

  const routes = [
    "/anthropic/v1/messages",
    "/openai/v1/chat/completions",
    "/gemini/v1beta/models/gemini-test:generateContent",
    "/xai/v1/chat/completions"
  ];
  for (const route of routes) {
    const result = await page.evaluate(async path => {
      const response = await fetch(path, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ model: "demo-isolation-check", messages: [] })
      });
      return { status: response.status, body: await response.json() };
    }, route);
    expect(result.status, route).toBe(409);
    expect(result.body?.error?.type, route).toBe("burnban_demo_network_disabled");
  }
  await expect(page.locator("#reqs")).toHaveText(requestCountBefore || "");
  expect(externalRequests).toEqual([]);
});

test("team dashboard authenticates by header or tab prompt, never by URL", async ({ browser, request }) => {
  test.skip(!teamBaseURL || !teamToken, "set the isolated team server URL and test token");

  const shell = await request.get(`${teamBaseURL}/`);
  expect(shell.status()).toBe(200);
  expect(await shell.text()).toContain("MODEL SPEND METER");

  const withoutToken = await request.get(`${teamBaseURL}/api/summary`);
  expect(withoutToken.status()).toBe(401);
  const wrongHeader = await request.get(`${teamBaseURL}/api/summary`, {
    headers: { "x-burnban-token": "definitely-wrong" }
  });
  expect(wrongHeader.status()).toBe(401);
  const correctHeader = await request.get(`${teamBaseURL}/api/summary`, {
    headers: { "x-burnban-token": teamToken }
  });
  expect(correctHeader.status()).toBe(200);
  const correctBody = await correctHeader.text();
  const correctJSON = JSON.parse(correctBody);
  expect(correctJSON.auth_required).toBe(true);
  expect(correctJSON.exposure).toBe("team/network");
  expect(correctJSON.local_usage_enabled).toBe(false);
  expect(correctBody).not.toContain(teamToken);
  const forbiddenLocalUsage = await request.get(`${teamBaseURL}/api/subsidy?window=today`, {
    headers: { "x-burnban-token": teamToken }
  });
  expect(forbiddenLocalUsage.status()).toBe(403);

  const promptContext = await browser.newContext();
  const promptPage = await promptContext.newPage();
  const localUsageRequests = [];
  promptPage.on("request", req => {
    if (req.url().includes("/api/subsidy")) localUsageRequests.push(req.url());
  });
  await promptPage.goto(teamBaseURL);
  await expect(promptPage).toHaveTitle("burnban — the meter is running");
  await expect(promptPage.locator("#statusText")).toHaveText("AUTH REQUIRED");
  await expect(promptPage.locator("#authPrompt")).toBeVisible();
  const authAxe = await new AxeBuilder({ page: promptPage }).analyze();
  const authSevere = authAxe.violations.filter(violation =>
    violation.impact === "serious" || violation.impact === "critical"
  ).map(violation => ({
    id: violation.id,
    impact: violation.impact,
    targets: violation.nodes.map(node => node.target)
  }));
  expect(authSevere, JSON.stringify(authSevere, null, 2)).toEqual([]);

  await promptPage.locator("#authToken").fill("definitely-wrong");
  const wrongPromptResponse = promptPage.waitForResponse(response =>
    response.url().endsWith("/api/summary") && response.status() === 401
  );
  await promptPage.locator("#authToken").press("Enter");
  await wrongPromptResponse;
  await expect(promptPage.locator("#statusText")).toHaveText("AUTH REQUIRED");
  expect(await promptPage.evaluate(() => sessionStorage.getItem("bb_token"))).toBeNull();

  await promptPage.locator("#authToken").fill(teamToken);
  const correctPromptResponse = promptPage.waitForResponse(response =>
    response.url().endsWith("/api/summary") && response.status() === 200
  );
  await promptPage.locator("#authSubmit").click();
  await correctPromptResponse;
  await expect(promptPage.locator("#statusText")).toHaveText("LIVE");
  await expect(promptPage.locator("#authPrompt")).toBeHidden();
  await expect(promptPage.locator("#localUsagePanel")).toBeHidden();
  expect(localUsageRequests).toEqual([]);
  expect(await promptPage.evaluate(() => sessionStorage.getItem("bb_token"))).toBe(teamToken);
  await promptContext.close();

  const legacyContext = await browser.newContext();
  const legacyPage = await legacyContext.newPage();
  const summaryHeaders = [];
  legacyPage.on("request", req => {
    if (req.url().endsWith("/api/summary")) summaryHeaders.push(req.headers());
  });
  const legacySummary = legacyPage.waitForResponse(response =>
    response.url().endsWith("/api/summary") && response.status() === 401
  );
  await legacyPage.goto(`${teamBaseURL}/?token=${encodeURIComponent(teamToken)}&view=qa#meter`);
  await legacySummary;
  const legacyState = await legacyPage.evaluate(() => ({
    href: location.href,
    query: location.search,
    hash: location.hash,
    history: JSON.stringify(history.state) || "",
    stored: sessionStorage.getItem("bb_token"),
    persistent: localStorage.getItem("bb_token"),
    cookies: document.cookie
  }));
  expect(legacyState.href).not.toContain("token=");
  expect(legacyState.query).toBe("?view=qa");
  expect(legacyState.hash).toBe("#meter");
  expect(legacyState.history).not.toContain(teamToken);
  expect(legacyState.stored).toBeNull();
  expect(legacyState.persistent).toBeNull();
  expect(legacyState.cookies).not.toContain(teamToken);
  expect(summaryHeaders.length).toBeGreaterThan(0);
  expect(summaryHeaders.every(headers => !headers["x-burnban-token"])).toBe(true);
  await expect(legacyPage.locator("#statusText")).toHaveText("AUTH REQUIRED");
  await legacyContext.close();
});

for (const viewport of viewports) {
  test(`fits ${viewport.width}px without horizontal page overflow`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await waitForLiveDashboard(page);
    const dimensions = await page.evaluate(() => {
      const viewportWidth = document.documentElement.clientWidth;
      const offenders = [...document.querySelectorAll("body *")]
        .map(element => {
          const rect = element.getBoundingClientRect();
          return {
            element: element.id ? `#${element.id}` : element.classList.length
              ? `${element.tagName.toLowerCase()}.${[...element.classList].join(".")}`
              : element.tagName.toLowerCase(),
            left: Math.round(rect.left),
            right: Math.round(rect.right),
            width: Math.round(rect.width)
          };
        })
        .filter(rect => rect.left < -1 || rect.right > viewportWidth + 1)
        .slice(0, 12);
      return {
        viewport: viewportWidth,
        page: document.documentElement.scrollWidth,
        offenders
      };
    });
    expect(
      dimensions.page,
      `Horizontal overflow diagnostics:\n${JSON.stringify(dimensions, null, 2)}`
    ).toBeLessThanOrEqual(dimensions.viewport + 1);
    await expect(page.locator("#budgetTrack")).toHaveAttribute("role", "progressbar");
  });
}

test("keyboard focus reaches controls and activates a usage window", async ({ page }) => {
  await waitForLiveDashboard(page);
  const sevenDays = page.getByRole("button", { name: "7 days" });
  await sevenDays.focus();
  await expect(sevenDays).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(sevenDays).toHaveAttribute("aria-pressed", "true");

  await page.keyboard.press("Tab");
  const focusState = await page.evaluate(() => {
    const active = document.activeElement;
    const style = active instanceof HTMLElement ? getComputedStyle(active) : null;
    return {
      tag: active?.tagName || "",
      visible: Boolean(active && active instanceof HTMLElement && active.offsetParent !== null),
      outline: style?.outlineStyle || ""
    };
  });
  expect(focusState.tag).not.toBe("BODY");
  expect(focusState.visible).toBe(true);
  expect(focusState.outline).not.toBe("none");
});

test("has no serious or critical axe violations", async ({ page }) => {
  const summary = [];
  for (const viewport of viewports) {
    await page.setViewportSize(viewport);
    await waitForLiveDashboard(page);
    const results = await new AxeBuilder({ page }).analyze();
    for (const violation of results.violations.filter(result =>
      result.impact === "serious" || result.impact === "critical"
    )) {
      summary.push({
        viewport: viewport.width,
        id: violation.id,
        impact: violation.impact,
        help: violation.help,
        targets: violation.nodes.map(node => node.target)
      });
    }
  }
  expect(summary, JSON.stringify(summary, null, 2)).toEqual([]);
});
