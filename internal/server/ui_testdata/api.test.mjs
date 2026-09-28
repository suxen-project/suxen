import assert from "node:assert/strict";
import test from "node:test";

test("API token sign-in stores a trimmed session token and sends it as Bearer", async () => {
  const values = new Map();
  globalThis.sessionStorage = {
    getItem: (key) => values.get(key) || null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
  };
  let authorization = "";
  globalThis.fetch = async (_path, options) => {
    authorization = options.headers.get("Authorization");
    return new Response(JSON.stringify({authenticated: true, username: "admin"}), {
      headers: {"Content-Type": "application/json"},
    });
  };

  const {api} = await import("../ui/api.js?token-sign-in-test");
  api.setToken("  bootstrap-secret  ");
  const identity = await api.json("/api/v1/whoami");

  assert.equal(values.get("suxenToken"), "bootstrap-secret");
  assert.equal(authorization, "Bearer bootstrap-secret");
  assert.equal(identity.username, "admin");

  api.setToken("");
  assert.equal(values.has("suxenToken"), false);
});

test("asset page requests carry a cursor without adding an offset page", async () => {
  globalThis.window = {location: {origin: "https://example.test"}};
  const {api} = await import("../ui/api.js?asset-page-test");
  let requestPath = "";
  api.json = async (path) => {
    requestPath = path;
    return {items: [], nextCursor: ""};
  };
  await api.page("/api/v1/repositories/raw/assets?prefix=releases%2F", {
    limit: 50,
    cursor: "opaque-token",
  });
  const requested = new URL(requestPath, window.location.origin);
  assert.equal(requested.searchParams.get("prefix"), "releases/");
  assert.equal(requested.searchParams.get("limit"), "50");
  assert.equal(requested.searchParams.get("cursor"), "opaque-token");
  assert.equal(requested.searchParams.has("page"), false);
  await assert.rejects(
    () => api.page("/api/v1/repositories/raw/assets", {page: 2, cursor: "opaque-token"}),
    /page and cursor cannot be combined/,
  );
});

test("problem+json responses retain structured server details", async () => {
  const {api, APIError} = await import("../ui/api.js?problem-json-test");
  globalThis.fetch = async () => new Response(
    JSON.stringify({code: "asset_generation_changed", detail: "The asset content changed."}),
    {status: 412, headers: {"Content-Type": "application/problem+json; charset=utf-8"}},
  );
  await assert.rejects(() => api.json("/api/v1/repositories/raw/assets/1/attributes/scan"), (error) => {
    assert.ok(error instanceof APIError);
    assert.equal(error.status, 412);
    assert.equal(error.payload.code, "asset_generation_changed");
    assert.equal(error.message, "The asset content changed.");
    return true;
  });
});
