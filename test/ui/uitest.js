// Drives the real web UI in headless Chromium: every page in both themes,
// plus the main interactions. Fails on any JS/CSP error.
// Usage: node uitest.js <screenshot dir>   (env: NODE_ID, JOB_ID, TASK_ID, LOG_TASK_ID)
const { chromium } = require("playwright");

const base = "https://127.0.0.1:18443";
const out = process.argv[2];
const { NODE_ID, JOB_ID, TASK_ID, LOG_TASK_ID } = process.env;
const errors = [];
const checks = [];
const check = (name, ok, detail = "") => { checks.push(`${ok ? "PASS" : "FAIL"} ${name}${detail ? " - " + detail : ""}`); if (!ok) errors.push(name); };

const pages = [
  ["dashboard", ""], ["nodes", "nodes"], ["node", "nodes/" + NODE_ID], ["jobs", "jobs"],
  ["job", "jobs/" + JOB_ID], ["task", "tasks/" + TASK_ID], ["tokens", "tokens"], ["settings", "settings"],
];

async function login(page) {
  await page.goto(base + "/");
  check("unauthenticated / redirects to /login", page.url().endsWith("/login"), page.url());
  await page.fill("#password", "wrong-password");
  await page.click("#submit");
  await page.waitForSelector("#error:not(.hidden)");
  check("wrong password shows an error", (await page.textContent("#error")).includes("wrong"));
  await page.fill("#password", "devpassword");
  await page.click("#submit");
  await page.waitForSelector("#nav a");
}

(async () => {
  const browser = await chromium.launch();
  for (const theme of ["dark", "light"]) {
    const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 }, colorScheme: theme });
    const page = await ctx.newPage();
    // The deliberate wrong-password attempt logs a 401; anything else is a bug.
    page.on("console", (m) => { if (m.type() === "error" && !m.text().includes("status of 401")) errors.push(`[${theme}] console: ${m.text()}`); });
    page.on("pageerror", (e) => errors.push(`[${theme}] page error: ${e.message}`));
    await login(page);
    for (const [name, hash] of pages) {
      await page.goto(`${base}/#/${hash}`);
      await page.waitForTimeout(1800);
      const failed = await page.locator("text=Could not load this page").count();
      check(`[${theme}] page ${name} renders`, failed === 0);
      await page.screenshot({ path: `${out}/${theme}-${name}.png`, fullPage: true });
    }

    if (theme !== "dark") { await ctx.close(); continue; }

    // Dashboard numbers and charts.
    await page.goto(base + "/#/");
    await page.waitForTimeout(1500);
    check("dashboard shows 2 nodes online", (await page.locator(".tile").nth(3).textContent()).includes("2"));
    check("dashboard draws charts", (await page.locator(".chart canvas").count()) >= 2);

    // Join token creation shows a code.
    await page.goto(base + "/#/tokens");
    await page.click("#create button[type=submit]");
    await page.waitForSelector(".joincode");
    const code = (await page.textContent(".joincode")).trim();
    check("join code created", /^[0-9A-Z]{4}(-[0-9A-Z]{4}){4}$/.test(code), code);

    // Submit a job from the form and watch it finish.
    await page.goto(base + "/#/jobs");
    await page.click("#toggle-form");
    await page.fill("textarea[name=command]", "echo from the UI {i}");
    await page.fill("input[name=array]", "1-3");
    await page.fill("input[name=cpus]", "0.5");
    await page.click("#submit button[type=submit]");
    await page.waitForURL(/#\/jobs\/j[0-9a-f]+$/);
    const jobId = page.url().split("/").pop();
    await page.waitForFunction(() => document.querySelector("#state")?.textContent.includes("succeeded"), null, { timeout: 20000 });
    check("UI-submitted job succeeds", true, jobId);
    check("job page lists 3 tasks", (await page.locator("#tasks tbody tr").count()) === 3);
    await page.click("#tasks tbody tr >> nth=1");
    await page.waitForURL(/#\/tasks\//);
    await page.waitForFunction(() => document.querySelector("#log")?.textContent.includes("from the UI 2"), null, { timeout: 10000 });
    check("task log shows output", true);

    // A job with an uploaded input file, a script and an output pattern.
    await page.goto(base + "/#/jobs");
    await page.click("#toggle-form");
    await page.setInputFiles("input[name=script]", { name: "shout.sh", mimeType: "text/plain",
      buffer: Buffer.from("#!/bin/sh\nmkdir -p out\ntr a-z A-Z < hello.txt > out/HELLO.txt\necho script says $1\n") });
    await page.setInputFiles("input[name=files]", { name: "hello.txt", mimeType: "text/plain", buffer: Buffer.from("hello world\n") });
    await page.fill("textarea[name=command]", "first-arg");
    await page.fill("input[name=outputs]", "out/*");
    await page.fill("input[name=cpus]", "0.5");
    await page.click("#submit button[type=submit]");
    await page.waitForURL(/#\/jobs\/j[0-9a-f]+$/);
    await page.waitForFunction(() => document.querySelector("#state")?.textContent.match(/succeeded|failed/), null, { timeout: 30000 });
    check("job with files succeeds", (await page.textContent("#state")).includes("succeeded"));
    await page.waitForSelector("#outputs a");
    const href = await page.getAttribute("#outputs a", "href");
    const content = await page.evaluate(async (u) => (await fetch(u)).text(), href);
    check("output file downloads with the right content", content === "HELLO WORLD\n", JSON.stringify(content));
    const zipHead = await page.evaluate(async () => {
      const u = document.getElementById("zip").href;
      const b = new Uint8Array(await (await fetch(u)).arrayBuffer());
      return String.fromCharCode(b[0], b[1]) + ":" + b.length;
    });
    check("outputs zip downloads", zipHead.startsWith("PK:"), zipHead);
    await page.screenshot({ path: `${out}/dark-job-files.png`, fullPage: true });
    await page.click("#tasks tbody tr >> nth=0");
    await page.waitForFunction(() => document.querySelector("#log")?.textContent.includes("script says first-arg"), null, { timeout: 10000 });
    check("uploaded script ran with its argument", true);

    // Live log of a running task.
    await page.goto(`${base}/#/tasks/${LOG_TASK_ID}`);
    await page.waitForFunction(() => /line [3-9]/.test(document.querySelector("#log")?.textContent || ""), null, { timeout: 20000 });
    const before = (await page.textContent("#log")).length;
    await page.waitForTimeout(2500);
    const after = (await page.textContent("#log")).length;
    check("live log grows while the task runs", after > before, `${before} -> ${after} chars`);
    await page.screenshot({ path: `${out}/dark-task-live.png`, fullPage: true });

    // Drain and resume a node through the confirmation dialog.
    await page.goto(base + "/#/nodes");
    await page.waitForSelector(".node-card");
    await page.locator(".node-card").first().locator("[data-action=drain]").click();
    await page.click(".modal button[type=submit]");
    await page.waitForSelector(".node-card .badge.warn:has-text('draining')");
    check("drain marks the node draining", true);
    await page.screenshot({ path: `${out}/dark-nodes-draining.png`, fullPage: true });
    await page.locator("[data-action=undrain]").first().click();
    await page.waitForFunction(() => !document.querySelector(".node-card .badge.warn"), null, { timeout: 5000 });
    check("resume clears draining", true);

    // Labels dialog.
    await page.locator(".node-card").first().locator("[data-action=edit]").click();
    await page.fill(".modal textarea", "gpu=none\nzone=ui-test");
    await page.fill(".modal input[name=location]", "lab");
    await page.selectOption(".modal select[name=network]", "remote");
    await page.click(".modal button[type=submit]");
    await page.waitForSelector(".chip:has-text('zone=ui-test')");
    check("labels can be edited", true);
    await page.waitForSelector(".node-card .badge:has-text('remote')");
    check("network override marks the node remote", true);
    check("location can be edited", (await page.locator(".node-card .sub", { hasText: "lab" }).count()) > 0);

    // Theme toggle and logout.
    await page.click("#theme-toggle");
    check("theme toggle sets data-theme", (await page.evaluate(() => document.documentElement.dataset.theme)) === "light");
    await page.click("#logout");
    await page.waitForURL(/\/login$/);
    check("logout returns to login", true);
    await ctx.close();
  }
  await browser.close();

  console.log(checks.join("\n"));
  if (errors.length) {
    console.log("\nERRORS:\n" + errors.join("\n"));
    process.exit(1);
  }
  console.log("\nALL UI CHECKS PASSED");
})().catch((e) => { console.log(checks.join("\n")); console.error("FATAL:", e.message); process.exit(1); });
