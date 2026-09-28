import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import vm from "node:vm";

function loadModule(name, scope) {
  const source = readFileSync(new URL(`../ui/${name}.js`, import.meta.url), "utf8")
    .replace(/import\s[\s\S]*?from\s+"[^"]+";/g, "")
    .replace(/export /g, "");
  vm.runInNewContext(source, scope);
}

test("cleanup chooser finds an attached policy beyond the first 200", async () => {
  globalThis.sessionStorage = {getItem: () => null};
  globalThis.window = {location: {origin: "https://example.test"}};
  const {api} = await import("../ui/api.js?cleanup-pagination");
  const requests = [];
  api.json = async (path) => {
    const url = new URL(path, window.location.origin);
    requests.push(url);
    return url.searchParams.has("cursor")
      ? {items: [{name: "last-policy", repositories: ["releases"]}]}
      : {items: Array.from({length: 200}, (_, i) => ({name: `policy-${i}`, repositories: ["other"]})), nextCursor: "next-page"};
  };
  let form;
  const scope = {api, openForm: (value) => { form = value; }, showNotice() {}};
  loadModule("repositories", scope);
  await scope.chooseCleanupPolicy("releases");
  assert.equal(form?.value.policy, "last-policy");
  assert.deepEqual([...form.schema[0].options], ["last-policy"]);
  assert.equal(requests.length, 2);
  assert.equal(requests[1].searchParams.get("cursor"), "next-page");
});

test("operations cleanup controls can reach policies after the first page", async () => {
  const requests = [];
  const displayed = [];
  let pager;
  const scope = {
    api: {page: async (path, options) => {
      requests.push({path, ...options});
      return {items: [{name: `page-${options.page}`}], total: 201};
    }},
    hasPrivilege: () => true,
    element: () => ({append() {}, replaceChildren() {}}),
    renderTable: (_table, _columns, items) => displayed.push(items[0].name),
    numberedPager: (page, total, size, load) => { pager = {page, total, size, load}; },
  };
  loadModule("operations_ui", scope);
  await scope.cleanupTriggersPanel({});
  assert.equal(pager.total, 201);
  await pager.load(5);
  assert.deepEqual(displayed, ["page-1", "page-5"]);
  assert.deepEqual(requests.map(({page, limit}) => [page, limit]), [[1, 50], [5, 50]]);
});
