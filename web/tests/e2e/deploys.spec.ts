import { execFileSync } from "node:child_process";
import { cpSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

// Drives the whole vertical slice against the real backend: register the Go
// fixture repo, deploy a commit, watch the dashboard, then open the real
// preview subdomain (Chromium resolves *.localhost natively).

const FIXTURE_SRC = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../../../internal/build/testdata/fixture-repo",
);

function git(dir: string, ...args: string[]): string {
  return execFileSync("git", args, {
    cwd: dir,
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "e2e",
      GIT_AUTHOR_EMAIL: "e2e@example.com",
      GIT_COMMITTER_NAME: "e2e",
      GIT_COMMITTER_EMAIL: "e2e@example.com",
    },
  })
    .toString()
    .trim();
}

let repoDir: string;
let sha: string;

test.beforeAll(() => {
  repoDir = mkdtempSync(join(tmpdir(), "preview-fixture-"));
  cpSync(FIXTURE_SRC, repoDir, { recursive: true });
  git(repoDir, "init", "-q", "-b", "main");
  git(repoDir, "add", "-A");
  git(repoDir, "commit", "-qm", "initial");
  sha = git(repoDir, "rev-parse", "HEAD");
});

test("register, deploy, and open a preview", async ({ page }) => {
  test.setTimeout(180_000);

  const repoRes = await page.request.post("/api/repos", {
    data: { name: "fixture", source: repoDir },
  });
  expect(repoRes.status()).toBe(201);

  const depRes = await page.request.post("/api/deploys", {
    data: { repo: "fixture", ref: sha },
  });
  expect(depRes.status()).toBe(202);
  const deploy = await depRes.json();

  let latest = deploy;
  await expect
    .poll(
      async () => {
        const r = await page.request.get(`/api/deploys/${deploy.id}`);
        latest = await r.json();
        if (latest.status === "failed") {
          const logs = await page.request.get(`/api/deploys/${deploy.id}/logs`);
          throw new Error(`deploy failed: ${latest.error}\n${await logs.text()}`);
        }
        return latest.status;
      },
      { timeout: 120_000, intervals: [500] },
    )
    .toBe("ready");

  // Dashboard lists the deploy with a preview link.
  await page.goto("/");
  await expect(page.getByText(latest.short_sha)).toBeVisible();
  await expect(page.getByText("ready", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: /open/ })).toBeVisible();

  // The preview itself, at its real subdomain, served by Host routing.
  await page.goto(latest.preview_url);
  await expect(page.getByText("fixture frontend v1")).toBeVisible();

  // /api on the preview host reaches the fixture backend (cold start
  // included).
  const health = await page.evaluate(() => fetch("/api/health").then((r) => r.text()));
  expect(health).toBe("ok");
  const counter = await page.evaluate(() => fetch("/api/counter").then((r) => r.text()));
  expect(counter).toBe("1");
});
