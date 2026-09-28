import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import vm from "node:vm";

import {defaults, schemas} from "../ui/schemas.js";
import {cloneJSON} from "../ui/json_codec.js";

class APIError extends Error {
  constructor(status, message, payload) {
    super(message);
    this.status = status;
    this.payload = payload;
  }
}

function repositoryHarness(respond = async () => ({})) {
  const calls = [];
  const notices = [];
  let form;
  let refreshes = 0;
  const scope = {
    APIError, defaults, schemas, structuredClone, cloneJSON,
    api: {json: async (path, options) => {
      calls.push({path, options});
      return respond(path, options);
    }},
    apiPath: (...parts) => `/${parts.map(encodeURIComponent).join("/")}`,
    openForm: (options) => { form = options; },
    confirmAction: async () => true,
    showNotice: (message) => notices.push(message),
    navigateOrRefresh() {},
    refreshCurrentRoute: () => { refreshes += 1; },
    routePath: () => "#/repositories/raw",
    window: {location: {reload() {}}},
  };
  const source = readFileSync(new URL("../ui/repositories.js", import.meta.url), "utf8")
    .replace(/import\s[\s\S]*?from\s+"[^"]+";/g, "")
    .replace(/export /g, "");
  vm.createContext(scope);
  vm.runInContext(source, scope);
  return {scope, calls, notices, form: () => form, refreshes: () => refreshes};
}

test("repository edit retains format config and omits unchanged redacted upstream", async () => {
  const harness = repositoryHarness(async (path) => path === "/api/v1"
    ? {formats: ["raw", "maven"]}
    : {});
  await harness.scope.editRepository({
    name: "releases", format: "maven", type: "proxy", blobStore: "default",
    upstream: "https://repo.example/releases", formatConfig: {versionPolicy: "release"},
  });
  assert.equal(harness.form().value.formatConfig.versionPolicy, "release");
  const unchanged = structuredClone(harness.form().value);
  await harness.form().onSave(unchanged);
  const sent = harness.calls.at(-1).options.body;
  assert.equal(Object.hasOwn(sent, "upstream"), false);
  assert.equal(sent.formatConfig.versionPolicy, "release");

  const rotated = structuredClone(harness.form().value);
  rotated.upstream = "https://user:new-secret@repo.example/releases";
  await harness.form().onSave(rotated);
  assert.equal(harness.calls.at(-1).options.body.upstream, rotated.upstream);
});

test("attribute set and delete carry the displayed generation", async () => {
  const harness = repositoryHarness();
  const asset = {repository: "raw", id: 17, path: "app.jar", digest: "sha256:abc"};
  harness.scope.editAttributes(asset);
  await harness.form().onSave({namespace: "scan", value: {status: "passed"}});
  await harness.scope.deleteAttribute(asset, "scan");
  assert.equal(harness.calls[0].options.headers["If-Match"], '"sha256:abc"');
  assert.equal(harness.calls[1].options.headers["If-Match"], '"sha256:abc"');
});

test("a stale annotation refreshes the asset and asks for a retry", async () => {
  const harness = repositoryHarness(async () => {
    throw new APIError(412, "asset generation changed", {code: "asset_generation_changed"});
  });
  const asset = {repository: "raw", id: 17, path: "app.jar", digest: "sha256:old"};
  harness.scope.editAttributes(asset);
  await harness.form().onSave({namespace: "scan", value: {status: "passed"}});
  await harness.scope.deleteAttribute(asset, "scan");
  assert.equal(harness.refreshes(), 2);
  assert.equal(harness.notices.length, 2);
  assert.match(harness.notices[0], /retry the annotation on the current artifact/);
});

test("attribute editing rejects installed format namespaces and dotted children", async () => {
  const formats = ["raw", "oci", "maven", "go", "cargo", "npm", "pypi", "git", "custom"];
  const harness = repositoryHarness(async () => ({formats}));
  await harness.scope.loadFormatOptions();
  const asset = {repository: "raw", id: 17, path: "app.jar", digest: "sha256:abc"};
  harness.scope.editAttributes(asset);
  for (const root of ["sys", "classification", "provenance", ...formats]) {
    for (const namespace of [root, `${root}.extra`]) {
      assert.equal(harness.scope.reservedNamespace(namespace), true, namespace);
      await assert.rejects(harness.form().onSave({namespace, value: {}}), /managed by the server/);
    }
  }
  assert.equal(harness.calls.length, 1, "only format discovery may reach the server");
  await harness.form().onSave({namespace: "scan.vendor", value: {status: "passed"}});
  assert.equal(harness.calls.length, 2);
});

test("repository replacement policy preserves explicit deny and omits nonhosted settings", async () => {
  const harness = repositoryHarness();
  await harness.scope.editRepository({name: "files", format: "raw", type: "hosted", allowOverwrite: false});
  assert.equal(harness.form().value.allowOverwrite, "Deny");
  await harness.form().onSave(structuredClone(harness.form().value));
  assert.equal(harness.calls.at(-1).options.body.allowOverwrite, false);
  await harness.form().onSave({...structuredClone(harness.form().value), allowOverwrite: "Allow"});
  assert.equal(harness.calls.at(-1).options.body.allowOverwrite, true);
  await harness.scope.editRepository({name: "mirror", format: "raw", type: "proxy"});
  assert.equal(harness.form().schema.some(field => field.name === "allowOverwrite"), false);
  await harness.form().onSave(structuredClone(harness.form().value));
  assert.equal(Object.hasOwn(harness.calls.at(-1).options.body, "allowOverwrite"), false);
  await harness.scope.editRepository(null);
  await harness.form().onSave({...structuredClone(harness.form().value), allowOverwrite: "Format default"});
  assert.equal(Object.hasOwn(harness.calls.at(-1).options.body, "allowOverwrite"), false);
});
