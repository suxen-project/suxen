import assert from "node:assert/strict";
import test from "node:test";

import {
  canLoadPrivilegeCatalog,
  hasPrivilege,
  LatestRender,
  parseHash,
  privilegeMatches,
  routePath,
} from "../ui/router.js";

test("hash routes encode segments and preserve query values", () => {
  const hash = `${routePath("repositories", "release files", "assets", 42)}?tab=metadata`;
  assert.equal(hash, "#/repositories/release%20files/assets/42?tab=metadata");

  const route = parseHash(hash);
  assert.deepEqual(route.segments, ["repositories", "release files", "assets", "42"]);
  assert.equal(route.query.get("tab"), "metadata");
});

test("an empty hash resolves to the overview", () => {
  assert.deepEqual(parseHash("").segments, ["overview"]);
  assert.deepEqual(parseHash("#/").segments, ["overview"]);
});

test("privilege matching supports exact and variable wildcard grants", () => {
  assert.equal(privilegeMatches("*", "admin:users:write"), true);
  assert.equal(privilegeMatches("repository:*:read", "repository:raw:read"), true);
  assert.equal(privilegeMatches("admin:*", "admin:webhooks:write"), true);
  assert.equal(privilegeMatches("repository:raw:read", "repository:raw:write"), false);
  assert.equal(privilegeMatches("admin:roles:read", "admin:users:read"), false);

  const identity = {effectivePrivileges: ["repository:raw:*", "admin:tasks:read"]};
  assert.equal(hasPrivilege(identity, "repository:raw:delete"), true);
  assert.equal(hasPrivilege(identity, "admin:tasks:run"), false);
});

test("only the latest asynchronous route render can commit", async () => {
  const latest = new LatestRender();
  const first = latest.begin();
  let releaseFirst;
  const firstReady = new Promise((resolve) => { releaseFirst = resolve; });
  const commits = [];
  const delayedCommit = firstReady.then(() => {
    if (first.isCurrent()) {
      commits.push("first");
    }
  });

  const second = latest.begin();
  if (second.isCurrent()) {
    commits.push("second");
  }
  releaseFirst();
  await delayedCommit;

  assert.deepEqual(commits, ["second"]);
});

test("the privilege catalog is separate from role read access", () => {
  const roleReader = {effectivePrivileges: ["admin:roles:read"]};
  assert.equal(hasPrivilege(roleReader, "admin:roles:read"), true);
  assert.equal(canLoadPrivilegeCatalog(roleReader), false);
});
