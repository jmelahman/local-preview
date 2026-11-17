import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { api, type Deploy } from "../src/api/client";

let handle = (_request: Request): Response => new Response("not found", { status: 404 });
let server: ReturnType<typeof Bun.serve>;
const networkFetch = globalThis.fetch;
let restoreFetch: () => void;

beforeEach(() => {
  server = Bun.serve({ port: 0, fetch: (request) => handle(request) });
  const translatedFetch: typeof fetch = Object.assign(
    (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) =>
      networkFetch(
        typeof input === "string" && input.startsWith("/") ? new URL(input, server.url) : input,
        init,
      ),
    { preconnect: networkFetch.preconnect },
  );
  const fetchSpy = spyOn(globalThis, "fetch").mockImplementation(translatedFetch);
  restoreFetch = () => {
    fetchSpy.mockRestore();
  };
});

afterEach(() => {
  restoreFetch();
  server.stop(true);
});

const deployment = {
  id: 7,
  repo: "demo",
  sha: "abc",
  short_sha: "abc",
  status: "ready",
  attempt_count: 1,
  preview_url: "https://abc.preview.example",
  created_at: "today",
  updated_at: "today",
} satisfies Deploy;

test("legacy deployment responses remain usable", async () => {
  handle = (request) =>
    new URL(request.url).pathname === "/api/deploys"
      ? Response.json({ id: 7, status: "queued" })
      : Response.json(deployment);
  expect(await api.createDeploy("demo", "main")).toEqual(deployment);
});

test("deployment creation follows ingestion until the real deploy is ready", async () => {
  let polls = 0;
  handle = (request) => {
    const path = new URL(request.url).pathname;
    if (path === "/api/deploys")
      return Response.json({ id: "job", status: "queued", repo: "demo" });
    if (path === "/api/repos/demo/ingestions/job") {
      polls++;
      return Response.json(
        polls === 1
          ? { id: "job", status: "leased" }
          : { id: "job", status: "done", result: { deploy: { id: 7 } } },
      );
    }
    if (path === "/api/deploys/7") return Response.json(deployment);
    return new Response("not found", { status: 404 });
  };
  expect(await api.createDeploy("demo", "main")).toEqual(deployment);
  expect(polls).toBe(2);
});

test("ingestion failure reaches the existing mutation error handler", async () => {
  handle = (request) =>
    new URL(request.url).pathname === "/api/deploys"
      ? Response.json({ id: "job", status: "queued", repo: "demo" })
      : Response.json({ id: "job", status: "failed", error: "backend compilation failed" });
  await expect(api.createDeploy("demo", "main")).rejects.toThrow("backend compilation failed");
});

test("malformed completed job metadata fails at the response boundary", async () => {
  handle = (request) =>
    new URL(request.url).pathname === "/api/deploys"
      ? Response.json({ id: "job", status: "queued", repo: "demo" })
      : Response.json({ id: "job", status: "done", result: { deploy: { id: "another-job" } } });
  await expect(api.createDeploy("demo", "main")).rejects.toThrow("Invalid completed deployment ID");
});

test("cancelling a queued deployment cancels its ingestion job", async () => {
  const cancel = new AbortController();
  let deleted = false;
  handle = (request) => {
    if (new URL(request.url).pathname === "/api/deploys") {
      return Response.json({ id: "job", status: "queued", repo: "demo" });
    }
    if (request.method === "DELETE") {
      deleted = true;
      return new Response(null, { status: 204 });
    }
    cancel.abort(new Error("stop waiting"));
    return Response.json({ id: "job", status: "queued" });
  };
  await expect(api.createDeploy("demo", "main", cancel.signal)).rejects.toThrow("stop waiting");
  expect(deleted).toBe(true);
});
