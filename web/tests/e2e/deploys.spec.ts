import { execFileSync } from "node:child_process";
import { appendFileSync, cpSync, mkdtempSync } from "node:fs";
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
  // A downloadable artifact alongside the two sides, with one file per
  // "platform", so the dashboard's download menu is exercised end-to-end.
  appendFileSync(
    join(repoDir, "preview.toml"),
    `
[artifacts.cli]
path  = "backend"
build = [["sh", "-c", "mkdir -p bin && echo linux-build > bin/fixture-cli-linux && echo darwin-build > bin/fixture-cli-darwin"]]
files = ["bin/fixture-cli-linux", "bin/fixture-cli-darwin"]
`,
  );
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

  // Dashboard lists the deploy with a preview link and the commit's
  // metadata: the branch is derived even for this deploy-by-sha, and the
  // author comes from the fixture commit. Fresh builds auto-start their
  // processes, so the badge converges on the warm state.
  await page.goto("/");
  await expect(page.getByText(latest.short_sha)).toBeVisible();
  await expect(page.getByText("running", { exact: true })).toBeVisible();
  await expect(page.getByText("main", { exact: true })).toBeVisible();
  await expect(page.getByText("by e2e")).toBeVisible();
  await expect(page.getByRole("link", { name: /open/ })).toBeVisible();

  // The per-service content hashes moved off the row into the commit sha's
  // hover text.
  await expect(page.getByText(latest.short_sha)).toHaveAttribute(
    "title",
    new RegExp(`backend ${latest.be_hash.slice(0, 12)}`),
  );

  // The declared artifact is listed on the deploy (files sorted by name) and
  // downloadable both via the API and the dashboard's per-artifact download
  // menu, which lists one entry per file.
  expect(latest.artifacts).toEqual([
    {
      name: "cli",
      hash: expect.any(String),
      files: [
        {
          name: "fixture-cli-darwin",
          size: 13,
          url: expect.stringMatching(/fixture-cli-darwin$/),
        },
        { name: "fixture-cli-linux", size: 12, url: expect.stringMatching(/fixture-cli-linux$/) },
      ],
    },
  ]);
  const download = page.getByRole("button", { name: "cli" });
  await expect(download).toBeVisible();
  await download.click();
  const menuItem = page.getByRole("menuitem", { name: /fixture-cli-linux/ });
  await expect(menuItem).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menuItem).toBeHidden();
  const linux = latest.artifacts[0].files.find(
    (f: { name: string }) => f.name === "fixture-cli-linux",
  );
  const dlRes = await page.request.get(linux.url);
  expect(dlRes.status()).toBe(200);
  expect(dlRes.headers()["content-disposition"]).toContain("attachment");
  expect(await dlRes.text()).toBe("linux-build\n");

  // The preview itself, at its real subdomain, served by Host routing.
  await page.goto(latest.preview_url);
  await expect(page.getByText("fixture frontend v1")).toBeVisible();

  // /api on the preview host reaches the fixture backend (cold start
  // included).
  const health = await page.evaluate(() => fetch("/api/health").then((r) => r.text()));
  expect(health).toBe("ok");
  const counter = await page.evaluate(() => fetch("/api/counter").then((r) => r.text()));
  expect(counter).toBe("1");

  // Back on the dashboard the deploy now shows as warm.
  await page.goto("/");
  await expect(page.getByText("running", { exact: true })).toBeVisible();

  // The observability panel: the run log tails the live process output and
  // the stats strip samples the warm process.
  await page.getByRole("button", { name: "logs" }).click();
  await expect(page.getByText(/fixture backend listening on/)).toBeVisible();
  await expect(page.getByText("start attempt 1")).toBeVisible();
  await expect(page.getByText(/[KMG]iB/).first()).toBeVisible({ timeout: 10_000 });

  // Build logs are one tab over.
  await page.getByRole("button", { name: "build log" }).click();
  await expect(page.getByText("--- backend build ---")).toBeVisible();
});
