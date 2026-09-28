import assert from "node:assert/strict";
import test from "node:test";

import {
  operationsRouteKind,
  repositoryRouteKind,
  resourceRouteKind,
} from "../ui/route_shapes.js";

test("valid nested routes select an exact screen", () => {
  assert.equal(repositoryRouteKind(["repositories", "raw", "assets", "42"]), "asset-detail");
  assert.equal(resourceRouteKind(["users", "publisher", "tokens"]), "tokens");
  assert.equal(operationsRouteKind(["tasks", "7"]), "task-detail");
});

test("unknown or overlong child routes are rejected", () => {
  assert.equal(repositoryRouteKind(["repositories", "raw", "unknown"]), null);
  assert.equal(repositoryRouteKind(["repositories", "raw", "assets", "42", "extra"]), null);
  assert.equal(resourceRouteKind(["roles", "publisher", "unknown"]), null);
  assert.equal(operationsRouteKind(["overview", "extra"]), null);
});
