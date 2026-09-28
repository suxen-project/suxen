import assert from "node:assert/strict";
import test from "node:test";

import {navigateOrRefresh} from "../ui/navigation.js";

test("saving the current detail route explicitly refreshes it", () => {
  const actions = [];
  const result = navigateOrRefresh("#/repositories/raw", {
    currentHash: "#/repositories/raw",
    navigate: (hash) => actions.push(["navigate", hash]),
    refresh: () => actions.push(["refresh"]),
  });

  assert.equal(result, "refreshed");
  assert.deepEqual(actions, [["refresh"]]);

  const resourceResult = navigateOrRefresh("#/roles/publisher", {
    currentHash: "#/roles/publisher",
    navigate: (hash) => actions.push(["navigate", hash]),
    refresh: () => actions.push(["refresh-resource"]),
  });
  assert.equal(resourceResult, "refreshed");
  assert.deepEqual(actions, [["refresh"], ["refresh-resource"]]);
});

test("saving a new resource navigates from list to detail", () => {
  const actions = [];
  const result = navigateOrRefresh("#/roles/publisher", {
    currentHash: "#/roles",
    navigate: (hash) => actions.push(["navigate", hash]),
    refresh: () => actions.push(["refresh"]),
  });

  assert.equal(result, "navigated");
  assert.deepEqual(actions, [["navigate", "#/roles/publisher"]]);
});
